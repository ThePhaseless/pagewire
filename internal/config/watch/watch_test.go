package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/goccy/go-yaml"
)

type doc struct {
	Name string `yaml:"name"`
}

// parseDoc rejects bad YAML and, separately, valid YAML with an empty name.
func parseDoc(b []byte) (doc, error) {
	var d doc
	if err := yaml.Unmarshal(b, &d); err != nil {
		return d, err
	}
	if d.Name == "" {
		return d, errors.New("name is required")
	}
	return d, nil
}

type event struct {
	kind     string // apply, invalid, missing
	name     string
	etag     string
	err      error
	lastGood *doc
}

type harness struct {
	t      *testing.T
	dir    string
	w      *Watcher[doc]
	events chan event
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, dir: dir, events: make(chan event, 100)}
	h.w = &Watcher[doc]{
		Dir:          dir,
		Name:         "printscan.yaml",
		Parse:        parseDoc,
		Debounce:     100 * time.Millisecond,
		Resync:       time.Hour,
		LastGoodPath: filepath.Join(dir, "printscan.last-good.yaml"),
		OnApply: func(v doc, etag string) {
			h.events <- event{kind: "apply", name: v.Name, etag: etag}
		},
		OnInvalid: func(err error, etag string, lastGood *doc) {
			h.events <- event{kind: "invalid", etag: etag, err: err, lastGood: lastGood}
		},
		OnMissing: func() { h.events <- event{kind: "missing"} },
	}
	return h
}

func (h *harness) write(name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, name), []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	if err := h.w.Start(ctx); err != nil {
		h.t.Fatal(err)
	}
}

// next waits for the next callback, failing after a short deadline.
func (h *harness) next(kind string) event {
	h.t.Helper()
	select {
	case e := <-h.events:
		if e.kind != kind {
			h.t.Fatalf("got %s event %+v, want %s", e.kind, e, kind)
		}
		return e
	case <-time.After(3 * time.Second):
		h.t.Fatalf("timed out waiting for %s", kind)
		return event{}
	}
}

// noneQueued asserts that no callback has run since the last next().
func (h *harness) noneQueued() {
	h.t.Helper()
	select {
	case e := <-h.events:
		h.t.Fatalf("unexpected %s event %+v", e.kind, e)
	default:
	}
}

func TestInitialLoadRunsBeforeStartReturns(t *testing.T) {
	h := newHarness(t)
	h.write("printscan.yaml", "name: a\n")
	h.start()
	if e := h.now("apply"); e.name != "a" {
		t.Fatalf("applied %q, want a", e.name)
	}
	h.noneQueued()
	if lg := h.w.LastGood(); lg == nil || lg.Name != "a" {
		t.Fatalf("LastGood = %+v", lg)
	}
	got, err := os.ReadFile(h.w.LastGoodPath)
	if err != nil || string(got) != "name: a\n" {
		t.Fatalf("last-good copy = %q, %v", got, err)
	}
}

// now returns the callback that already ran, without waiting: Start must
// have run it before returning.
func (h *harness) now(kind string) event {
	h.t.Helper()
	select {
	case e := <-h.events:
		if e.kind != kind {
			h.t.Fatalf("got %s event %+v, want %s", e.kind, e, kind)
		}
		return e
	default:
		h.t.Fatalf("%s callback had not run when Start returned", kind)
		return event{}
	}
}

func TestEditByRename(t *testing.T) {
	h := newHarness(t)
	h.write("printscan.yaml", "name: a\n")
	h.start()
	h.next("apply")
	h.write("printscan.yaml.tmp", "name: b\n")
	if err := os.Rename(filepath.Join(h.dir, "printscan.yaml.tmp"), filepath.Join(h.dir, "printscan.yaml")); err != nil {
		t.Fatal(err)
	}
	if e := h.next("apply"); e.name != "b" {
		t.Fatalf("applied %q, want b", e.name)
	}
}

func TestEditInPlace(t *testing.T) {
	h := newHarness(t)
	h.write("printscan.yaml", "name: a\n")
	h.start()
	h.next("apply")
	h.write("printscan.yaml", "name: b\n")
	if e := h.next("apply"); e.name != "b" {
		t.Fatalf("applied %q, want b", e.name)
	}
}

func TestRapidWritesCoalesce(t *testing.T) {
	h := newHarness(t)
	h.w.Debounce = 200 * time.Millisecond
	h.write("printscan.yaml", "name: a\n")
	h.start()
	h.next("apply")
	for _, n := range []string{"b", "c", "d", "e"} {
		h.write("printscan.yaml", "name: "+n+"\n")
		time.Sleep(5 * time.Millisecond)
	}
	if e := h.next("apply"); e.name != "e" {
		t.Fatalf("first apply after the burst was %q, want e", e.name)
	}
	// A later change is the very next callback, so the burst gave exactly one.
	h.write("printscan.yaml", "name: f\n")
	if e := h.next("apply"); e.name != "f" {
		t.Fatalf("applied %q, want f", e.name)
	}
}

func TestIdenticalRewriteSkipped(t *testing.T) {
	h := newHarness(t)
	h.write("printscan.yaml", "name: a\n")
	h.start()
	first := h.next("apply")
	h.write("printscan.yaml", "name: a\n")
	time.Sleep(3 * h.w.Debounce)
	h.write("printscan.yaml", "name: b\n")
	e := h.next("apply")
	if e.name != "b" {
		t.Fatalf("identical rewrite produced an apply; next was %q", e.name)
	}
	if e.etag == first.etag {
		t.Fatal("etag did not change with the content")
	}
}

func TestInvalidEditKeepsLastGood(t *testing.T) {
	for name, content := range map[string]string{
		"syntax":   "name: [\n",
		"semantic": "name: \"\"\n", // valid YAML that Parse rejects
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.write("printscan.yaml", "name: a\n")
			h.start()
			h.next("apply")
			h.write("printscan.yaml", content)
			e := h.next("invalid")
			if e.err == nil || e.lastGood == nil || e.lastGood.Name != "a" {
				t.Fatalf("invalid event = %+v", e)
			}
			if lg := h.w.LastGood(); lg == nil || lg.Name != "a" {
				t.Fatalf("LastGood = %+v", lg)
			}
			got, _ := os.ReadFile(h.w.LastGoodPath)
			if string(got) != "name: a\n" {
				t.Fatalf("last-good copy overwritten: %q", got)
			}
		})
	}
}

func TestDeletionCallsMissing(t *testing.T) {
	h := newHarness(t)
	h.write("printscan.yaml", "name: a\n")
	h.start()
	h.next("apply")
	if err := os.Remove(filepath.Join(h.dir, "printscan.yaml")); err != nil {
		t.Fatal(err)
	}
	h.next("missing")
	if lg := h.w.LastGood(); lg == nil || lg.Name != "a" {
		t.Fatalf("LastGood = %+v", lg)
	}
	// Restoring the same content applies it again.
	h.write("printscan.yaml", "name: a\n")
	if e := h.next("apply"); e.name != "a" {
		t.Fatalf("applied %q, want a", e.name)
	}
}

func TestStartMissing(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.now("missing")
	h.noneQueued()
	if h.w.LastGood() != nil {
		t.Fatal("LastGood not nil")
	}
}

func TestStartInvalidWithLastGoodFile(t *testing.T) {
	h := newHarness(t)
	h.write("printscan.last-good.yaml", "name: saved\n")
	h.write("printscan.yaml", "name: [\n")
	h.start()
	if e := h.now("invalid"); e.err == nil || e.lastGood == nil || e.lastGood.Name != "saved" {
		t.Fatalf("got %+v", e)
	}
	if lg := h.w.LastGood(); lg == nil || lg.Name != "saved" {
		t.Fatalf("LastGood = %+v", lg)
	}
}

func TestStartInvalidWithoutLastGoodFile(t *testing.T) {
	for name, content := range map[string]string{
		"syntax":   "name: [\n",
		"semantic": "name: \"\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.write("printscan.yaml", content)
			h.start()
			e := h.now("invalid")
			if e.err == nil || e.lastGood != nil {
				t.Fatalf("got %+v, want an error and nil lastGood", e)
			}
			if h.w.LastGood() != nil {
				t.Fatal("LastGood not nil")
			}
		})
	}
}

func TestResyncCatchesDroppedEvents(t *testing.T) {
	h := newHarness(t)
	h.w.Resync = 50 * time.Millisecond
	// The fake source never delivers: every fsnotify event is dropped.
	h.w.watch = func(string) (<-chan fsnotify.Event, <-chan error, func() error, error) {
		return nil, nil, func() error { return nil }, nil
	}
	h.write("printscan.yaml", "name: a\n")
	h.start()
	h.next("apply")
	h.write("printscan.yaml", "name: b\n")
	if e := h.next("apply"); e.name != "b" {
		t.Fatalf("applied %q, want b", e.name)
	}
}

func TestStartFailsForMissingDir(t *testing.T) {
	w := &Watcher[doc]{Dir: filepath.Join(t.TempDir(), "nope"), Name: "printscan.yaml", Parse: parseDoc}
	if err := w.Start(t.Context()); err == nil {
		t.Fatal("Start succeeded on a missing directory")
	}
}

// Without inotify (for example at fs.inotify.max_user_instances) the file is
// still read at start and the resync still picks up changes.
func TestWatchFailureFallsBackToResync(t *testing.T) {
	h := newHarness(t)
	h.w.Resync = 50 * time.Millisecond
	h.w.watch = func(string) (<-chan fsnotify.Event, <-chan error, func() error, error) {
		return nil, nil, nil, errors.New("too many open files")
	}
	h.write("printscan.yaml", "name: a\n")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.w.Start(ctx); !errors.Is(err, ErrWatchUnavailable) || !strings.Contains(err.Error(), "too many open files") {
		t.Fatalf("Start error %v, want the watch error", err)
	}
	if e := h.next("apply"); e.name != "a" {
		t.Fatalf("applied %q, want a", e.name)
	}
	h.write("printscan.yaml", "name: b\n")
	if e := h.next("apply"); e.name != "b" {
		t.Fatalf("applied %q, want b", e.name)
	}
}

func TestStartReadErrorKeepsWatching(t *testing.T) {
	h := newHarness(t)
	// A directory under the file's name makes the read fail with something other than not-exist.
	path := filepath.Join(h.dir, "printscan.yaml")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.w.Start(ctx); err == nil || errors.Is(err, ErrWatchUnavailable) {
		t.Fatalf("Start error %v, want a read error only", err)
	}
	h.noneQueued()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	h.write("printscan.yaml", "name: a\n")
	// A slow write may let the removal be seen on its own first.
	select {
	case e := <-h.events:
		if e.kind == "missing" {
			e = h.next("apply")
		}
		if e.kind != "apply" || e.name != "a" {
			t.Fatalf("got %+v, want apply a", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for apply")
	}
}
