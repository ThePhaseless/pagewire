// Package watch follows one file in a directory and reports every content
// change as applied, invalid or missing, keeping a last-known-good copy.
package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/renameio/v2"
)

// ErrWatchUnavailable marks a Start error where Dir could not be watched;
// the file was still read and changes arrive through the resync alone.
var ErrWatchUnavailable = errors.New("config directory cannot be watched")

// Watcher watches Dir/Name. Set the fields, then call Start once.
// Callbacks run one at a time: the first inside Start, the rest on the
// watcher's goroutine.
type Watcher[T any] struct {
	Dir  string
	Name string
	// Parse turns the file's bytes into a value; any error, syntactic or
	// semantic, makes the content invalid.
	Parse func([]byte) (T, error)
	// Debounce defaults to 300 ms, Resync to 30 s.
	Debounce time.Duration
	Resync   time.Duration
	// OnApply, OnInvalid and OnMissing may be nil. lastGood is nil when no
	// valid content has been seen.
	OnApply   func(v T, etag string)
	OnInvalid func(err error, etag string, lastGood *T)
	OnMissing func()
	// LastGoodPath is where valid content is copied after each apply and read
	// from when the file is invalid at start. Empty disables the copy.
	LastGoodPath string

	// watch opens the event source; tests replace it to drop events.
	watch func(dir string) (<-chan fsnotify.Event, <-chan error, func() error, error)

	mu       sync.Mutex
	lastGood *T

	// Owned by Start, then by the watcher goroutine.
	seen    bool
	missing bool
	hash    string
}

// LastGood returns the last valid value, or nil. Safe for concurrent use.
func (w *Watcher[T]) LastGood() *T {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastGood
}

// Start reads the file and runs the matching callback before it returns,
// then watches in a goroutine until ctx is done. If Dir cannot be watched
// (for example the inotify limit is reached) it falls back to the periodic
// resync alone, still reads the file and runs the callback, and returns the
// watch error wrapped in ErrWatchUnavailable. If the file exists but cannot be read it returns an error yet
// keeps watching; the first successful read then runs the first callback.
func (w *Watcher[T]) Start(ctx context.Context) error {
	if w.Dir == "" || w.Name == "" || w.Parse == nil {
		return errors.New("watch: Dir, Name and Parse are required")
	}
	if w.Debounce <= 0 {
		w.Debounce = 300 * time.Millisecond
	}
	if w.Resync <= 0 {
		w.Resync = 30 * time.Second
	}
	if w.watch == nil {
		w.watch = watchFS
	}
	// Watch before the first read so no change slips in between.
	events, errs, closeFn, watchErr := w.watch(w.Dir)
	if watchErr != nil {
		watchErr = fmt.Errorf("%w: watch %s: %w", ErrWatchUnavailable, w.Dir, watchErr)
		slog.Warn("config watch: cannot watch the directory; relying on the periodic resync", "err", watchErr, "resync", w.Resync)
		events, errs, closeFn = nil, nil, func() error { return nil }
	}
	raw, err := os.ReadFile(filepath.Join(w.Dir, w.Name))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// The loop keeps retrying; its first successful read runs the first callback.
		go w.loop(ctx, events, errs, closeFn)
		err = fmt.Errorf("watch: %w", err)
		if watchErr != nil {
			err = fmt.Errorf("%w; %w", err, watchErr) // one line, unlike errors.Join
		}
		return err
	}
	w.handle(raw, err)
	go w.loop(ctx, events, errs, closeFn)
	return watchErr
}

func watchFS(dir string) (<-chan fsnotify.Event, <-chan error, func() error, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := fw.Add(dir); err != nil {
		_ = fw.Close()
		return nil, nil, nil, err
	}
	return fw.Events, fw.Errors, fw.Close, nil
}

func (w *Watcher[T]) loop(ctx context.Context, events <-chan fsnotify.Event, errs <-chan error, closeFn func() error) {
	defer func() { _ = closeFn() }()
	resync := time.NewTicker(w.Resync)
	defer resync.Stop()
	debounce := time.NewTimer(w.Debounce)
	debounce.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				events = nil // keep resyncing
				continue
			}
			if filepath.Base(ev.Name) == w.Name {
				debounce.Reset(w.Debounce)
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			slog.Warn("config watch: event error; resync still runs", "dir", w.Dir, "err", err)
		case <-debounce.C:
			w.sync()
		case <-resync.C:
			w.sync()
		}
	}
}

func (w *Watcher[T]) sync() {
	raw, err := os.ReadFile(filepath.Join(w.Dir, w.Name))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("config watch: read failed; keeping current state", "err", err)
		return
	}
	w.handle(raw, err)
}

// handle runs the callback for one read, skipping content already seen.
func (w *Watcher[T]) handle(raw []byte, readErr error) {
	if readErr != nil {
		if w.seen && w.missing {
			return
		}
		w.seen, w.missing, w.hash = true, true, ""
		if w.OnMissing != nil {
			w.OnMissing()
		}
		return
	}
	sum := sha256.Sum256(raw)
	etag := hex.EncodeToString(sum[:])
	if w.seen && !w.missing && etag == w.hash {
		return
	}
	first := !w.seen
	w.seen, w.missing, w.hash = true, false, etag
	v, err := w.Parse(raw)
	if err != nil {
		if first {
			w.loadLastGood()
		}
		if w.OnInvalid != nil {
			w.OnInvalid(err, etag, w.LastGood())
		}
		return
	}
	w.mu.Lock()
	w.lastGood = &v
	w.mu.Unlock()
	if w.LastGoodPath != "" {
		if err := renameio.WriteFile(w.LastGoodPath, raw, 0o644); err != nil {
			slog.Warn("config watch: writing last-good copy failed", "path", w.LastGoodPath, "err", err)
		}
	}
	if w.OnApply != nil {
		w.OnApply(v, etag)
	}
}

// loadLastGood reads the last-good copy through Parse; used only at start.
func (w *Watcher[T]) loadLastGood() {
	if w.LastGoodPath == "" {
		return
	}
	raw, err := os.ReadFile(w.LastGoodPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("config watch: reading last-good copy failed", "path", w.LastGoodPath, "err", err)
		}
		return
	}
	v, err := w.Parse(raw)
	if err != nil {
		slog.Warn("config watch: last-good copy is invalid; ignoring it", "path", w.LastGoodPath, "err", err)
		return
	}
	w.mu.Lock()
	w.lastGood = &v
	w.mu.Unlock()
}
