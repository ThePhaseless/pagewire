package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/ThePhaseless/pagewire/internal/blobs"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/device"
)

const (
	// stageRetry is the delay after a first failure; it doubles with each
	// failure in a row up to stageRetryMax, since a failure after a full
	// download (a bad checksum, a cut transfer) downloads the archive again.
	stageRetry    = 5 * time.Minute
	stageRetryMax = 6 * time.Hour
	// MsgNoBlobVolume is the EULA message when the agent has no --blob-dir.
	MsgNoBlobVolume = "no blob volume"
)

// Stager stages EULA texts from verified vendor archives, one goroutine per
// ref, so Core's report loop never waits on a download. Results are cached
// for the life of the process; a failure is retried after 5 min, doubling up
// to 6 h while failures continue. Set the
// fields before the first Stage call; they must not change afterwards.
type Stager struct {
	Dir    string       // the blob volume; "": every ref fails with MsgNoBlobVolume
	Client *http.Client // nil: a client with a 10 min timeout
	// Blob looks up a blob by id; nil: the embedded catalog. The sim backend
	// points it at its synthetic vendor.
	Blob func(id string) (catalog.Blob, bool)
	Keys func(id string) (openpgp.EntityList, error) // nil: catalog.BlobKey
	Wake func()                                      // called when a ref's state or text changes; nil: none

	retry time.Duration                                         // first retry delay; 0: stageRetry
	now   func() time.Time                                      // nil: time.Now
	text  func(context.Context, device.EULARef) ([]byte, error) // test seam; nil: stageText

	mu      sync.Mutex
	entries map[device.EULARef]*stageEntry
}

type stageEntry struct {
	res      device.EULAText
	running  bool
	retryAt  time.Time
	failures int // in a row; reset by a success
}

// backoff is the retry delay after the given number of failures in a row.
func backoff(first time.Duration, failures int) time.Duration {
	d := first
	for i := 1; i < failures && d < stageRetryMax; i++ {
		d *= 2
	}
	return min(d, stageRetryMax)
}

// Stage returns the current result for each ref, in ref order, and starts
// staging any ref that has none yet or whose failure is due for a retry.
// Goroutines it starts stop with ctx.
func (s *Stager) Stage(ctx context.Context, refs []device.EULARef) []device.EULAText {
	out := make([]device.EULAText, 0, len(refs))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[device.EULARef]*stageEntry{}
	}
	now := s.clock()
	for _, ref := range refs {
		ref.TextSHA256 = ""
		e := s.entries[ref]
		if e == nil {
			e = &stageEntry{res: device.EULAText{ID: ref.ID, Version: ref.Version, State: device.EULAPending}}
			s.entries[ref] = e
		}
		if s.Dir == "" {
			e.res.State, e.res.Message = device.EULAFailed, MsgNoBlobVolume
		} else if !e.running && (e.res.State == device.EULAPending || e.res.State == device.EULAFailed && !now.Before(e.retryAt)) {
			e.running = true
			go s.run(ctx, ref, e)
		}
		out = append(out, e.res)
	}
	return out
}

func (s *Stager) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// run stages one ref and stores the result; it calls Wake when the state or
// text differs from the stored one.
func (s *Stager) run(ctx context.Context, ref device.EULARef, e *stageEntry) {
	fn := s.text
	if fn == nil {
		fn = s.stageText
	}
	text, err := fn(ctx, ref)
	res := device.EULAText{ID: ref.ID, Version: ref.Version, State: device.EULAAvailable}
	if err != nil {
		res.State, res.Message = device.EULAFailed, err.Error()
	} else {
		res.Text, res.TextSHA256 = string(text), blobs.TextSHA256(text)
	}
	retry := s.retry
	if retry <= 0 {
		retry = stageRetry
	}
	s.mu.Lock()
	changed := e.res.State != res.State || e.res.TextSHA256 != res.TextSHA256
	if err != nil {
		e.failures++
	} else {
		e.failures = 0
	}
	e.res, e.running, e.retryAt = res, false, s.clock().Add(backoff(retry, e.failures))
	s.mu.Unlock()
	if !changed || ctx.Err() != nil {
		return
	}
	slog.Info("EULA text", "id", ref.ID, "version", ref.Version, "state", res.State, "message", res.Message)
	if s.Wake != nil {
		s.Wake()
	}
}

func (s *Stager) stageText(ctx context.Context, ref device.EULARef) ([]byte, error) {
	e, ok := catalog.EULA(ref.ID)
	switch {
	case !ok:
		return nil, fmt.Errorf("unknown EULA %s", ref.ID)
	case e.Version != ref.Version:
		return nil, fmt.Errorf("EULA %s version %s is not the catalog's %s", ref.ID, ref.Version, e.Version)
	}
	lookup, keys := s.Blob, s.Keys
	if lookup == nil {
		lookup = catalog.Default().Blob
	}
	if keys == nil {
		keys = catalog.BlobKey
	}
	b, ok := lookup(e.Text.Blob)
	switch {
	case !ok:
		return nil, fmt.Errorf("unknown blob %s", e.Text.Blob)
	case !b.Enabled && b.DisabledMessage != "":
		return nil, errors.New(b.DisabledMessage)
	case !b.Enabled:
		return nil, errors.New(b.ID + " is not yet supported")
	}
	opt := blobs.Options{Client: s.Client}
	if opt.Client == nil {
		opt.Client = &http.Client{Timeout: 10 * time.Minute}
	}
	if b.Verify.PGPKey != "" || b.Verify.PGPSignatureURL != "" {
		k, err := keys(b.ID)
		if err != nil {
			return nil, err
		}
		opt.Keys = k
	}
	return blobs.Text(ctx, b, e, s.Dir, opt)
}
