package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/ThePhaseless/pagewire/internal/blobs"
	"github.com/ThePhaseless/pagewire/internal/blobs/blobtest"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/device"
)

func hplipRef(t *testing.T) device.EULARef {
	t.Helper()
	e, ok := catalog.EULA("hplip-plugin")
	if !ok {
		t.Fatal("no hplip-plugin EULA")
	}
	return device.EULARef{ID: e.ID, Version: e.Version}
}

// waitStage polls Stage until ok holds, failing after 10 s.
func waitStage(t *testing.T, s *Stager, refs []device.EULARef, ok func([]device.EULAText) bool) []device.EULAText {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := s.Stage(context.Background(), refs)
		if ok(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out; last result %+v", got)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStageNoBlobDir(t *testing.T) {
	s := &Stager{text: func(context.Context, device.EULARef) ([]byte, error) {
		t.Error("staged without a blob dir")
		return nil, nil
	}}
	got := s.Stage(context.Background(), []device.EULARef{hplipRef(t)})
	if len(got) != 1 || got[0].State != device.EULAFailed || got[0].Message != MsgNoBlobVolume {
		t.Errorf("Stage = %+v, want failed with %q", got, MsgNoBlobVolume)
	}
}

// TestRunWiresBlobDir: agent run's core stages through --blob-dir.
func TestRunWiresBlobDir(t *testing.T) {
	core, _, err := newRun(Config{ControlPlaneURL: "http://127.0.0.1:1", TokenFile: "token"})
	if err != nil {
		t.Fatal(err)
	}
	if got := core.Stage(context.Background(), []device.EULARef{hplipRef(t)}); len(got) != 1 || got[0].Message != MsgNoBlobVolume {
		t.Errorf("without --blob-dir: %+v", got)
	}
	core, _, err = newRun(Config{ControlPlaneURL: "http://127.0.0.1:1", TokenFile: "token", BlobDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// An id outside the catalog fails without a download, but only past the
	// blob-dir check.
	nope := []device.EULARef{{ID: "no-such-eula", Version: "1"}}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := core.Stage(context.Background(), nope)
		if got[0].State == device.EULAFailed {
			if got[0].Message == MsgNoBlobVolume {
				t.Fatal("--blob-dir not passed to the stager")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out; last %+v", got)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStageWakesOnChange(t *testing.T) {
	release := make(chan struct{})
	var calls, wakes atomic.Int64
	s := &Stager{
		Dir:  t.TempDir(),
		Wake: func() { wakes.Add(1) },
		text: func(context.Context, device.EULARef) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("licence\n"), nil
		},
	}
	refs := []device.EULARef{hplipRef(t)}
	if got := s.Stage(context.Background(), refs); len(got) != 1 || got[0].State != device.EULAPending {
		t.Fatalf("first Stage = %+v, want pending", got)
	}
	s.Stage(context.Background(), refs) // still running: no second goroutine
	close(release)
	got := waitStage(t, s, refs, func(e []device.EULAText) bool { return e[0].State == device.EULAAvailable })
	want := device.EULAText{
		ID: refs[0].ID, Version: refs[0].Version, State: device.EULAAvailable, Text: "licence\n",
		TextSHA256: blobs.TextSHA256([]byte("licence\n")),
	}
	if got[0] != want {
		t.Errorf("Stage = %+v", got[0])
	}
	for range 3 {
		s.Stage(context.Background(), refs)
	}
	if calls.Load() != 1 || wakes.Load() != 1 {
		t.Errorf("calls = %d, wakes = %d; want 1 each (cached after success, one wake per change)", calls.Load(), wakes.Load())
	}
}

func TestStageRetriesFailure(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1000, 0)
	var calls, wakes atomic.Int64
	s := &Stager{
		Dir:   t.TempDir(),
		Wake:  func() { wakes.Add(1) },
		retry: time.Minute,
		now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
		text: func(context.Context, device.EULARef) ([]byte, error) {
			calls.Add(1)
			return nil, errors.New("vendor down")
		},
	}
	refs := []device.EULARef{hplipRef(t)}
	got := waitStage(t, s, refs, func(e []device.EULAText) bool { return e[0].State == device.EULAFailed })
	if got[0].Message != "vendor down" || got[0].Text != "" || got[0].TextSHA256 != "" {
		t.Errorf("failed result = %+v", got[0])
	}
	s.Stage(context.Background(), refs)
	if calls.Load() != 1 {
		t.Fatalf("retried before the retry delay: %d calls", calls.Load())
	}
	mu.Lock()
	now = now.Add(time.Minute)
	mu.Unlock()
	s.Stage(context.Background(), refs)
	deadline := time.Now().Add(10 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatalf("no retry after the delay: %d calls", calls.Load())
	}
	// The same failure again is not a change.
	waitStage(t, s, refs, func([]device.EULAText) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.entries[refs[0]].running
	})
	if wakes.Load() != 1 {
		t.Errorf("wakes = %d, want 1 (pending to failed only)", wakes.Load())
	}
	// A second failure in a row doubles the delay: one more minute is not enough, two are.
	mu.Lock()
	now = now.Add(time.Minute)
	mu.Unlock()
	s.Stage(context.Background(), refs)
	if calls.Load() != 2 {
		t.Fatalf("retried before the doubled delay: %d calls", calls.Load())
	}
	mu.Lock()
	now = now.Add(time.Minute)
	mu.Unlock()
	s.Stage(context.Background(), refs)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 3 {
		t.Fatalf("no retry after the doubled delay: %d calls", calls.Load())
	}
}

func TestStageBackoff(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 5 * time.Minute}, // a success: the next run is never a retry, any value will do
		{1, 5 * time.Minute},
		{2, 10 * time.Minute},
		{3, 20 * time.Minute},
		{7, 5 * time.Minute << 6}, // 320 min
		{8, stageRetryMax},
		{100, stageRetryMax},
	}
	for _, c := range cases {
		if got := backoff(stageRetry, c.failures); got != c.want {
			t.Errorf("backoff(%d) = %v, want %v", c.failures, got, c.want)
		}
	}
}

func TestStageFromSyntheticVendor(t *testing.T) {
	key := blobtest.NewKey()
	run := blobtest.Archive(runtime.GOARCH)
	base, _ := catalog.Default().Blob("hplip-plugin")
	base.URL = "https://vendor.invalid/hplip.run"
	base.Verify = catalog.Verify{PGPSignatureURL: base.URL + ".asc"}
	dir := t.TempDir()
	s := &Stager{
		Dir:    dir,
		Client: &http.Client{Transport: blobtest.Transport(map[string][]byte{base.URL: run, base.URL + ".asc": blobtest.Sign(key, run)})},
		Blob:   func(id string) (catalog.Blob, bool) { return base, id == base.ID },
		Keys:   func(string) (openpgp.EntityList, error) { return openpgp.EntityList{key}, nil },
	}
	ref := hplipRef(t)
	got := waitStage(t, s, []device.EULARef{ref}, func(e []device.EULAText) bool { return e[0].State != device.EULAPending })
	if got[0].State != device.EULAAvailable || got[0].Text != blobtest.License || got[0].TextSHA256 != licenseSHA {
		t.Fatalf("Stage = %+v", got[0])
	}
	if _, err := os.Stat(filepath.Join(dir, ".staging", base.ID, ref.Version, "hplip.run")); err != nil {
		t.Errorf("archive not staged: %v", err)
	}

	other := device.EULARef{ID: ref.ID, Version: "0.0.1"}
	got = waitStage(t, s, []device.EULARef{other}, func(e []device.EULAText) bool { return e[0].State != device.EULAPending })
	if got[0].State != device.EULAFailed {
		t.Errorf("a version not in the catalog: %+v, want failed", got[0])
	}
}

// TestCoreReportsStagedEULAs: the report carries the desired refs' texts, and
// a finished staging wakes the loop instead of waiting for the interval.
func TestCoreReportsStagedEULAs(t *testing.T) {
	ref := hplipRef(t)
	p := newPlane(device.DesiredState{Browse: device.BrowseTypes, StageEULAs: []device.EULARef{ref}})
	core := NewCore("pod", device.Inventory{})
	release := make(chan struct{})
	s := &Stager{Dir: t.TempDir(), Wake: core.Wake, text: func(ctx context.Context, _ device.EULARef) ([]byte, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return []byte("licence\n"), nil
	}}
	core.Stage = s.Stage
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); core.Loop(ctx, p.report) }() // default 5 s interval
	defer func() { cancel(); <-done }()
	waitReport(t, p.reports, "a pending EULA", func(r device.AgentReport) bool {
		return len(r.EULAs) == 1 && r.EULAs[0].State == device.EULAPending
	})
	close(release)
	select {
	case r := <-p.reports:
		if len(r.EULAs) != 1 || r.EULAs[0].State != device.EULAAvailable {
			t.Errorf("woken report EULAs = %+v", r.EULAs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("staging did not wake the loop")
	}
}
