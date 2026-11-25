package scanning

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
)

// fakeScanner serves pages; when gate is set, every NextPage waits for it.
type fakeScanner struct {
	pages    [][]byte
	startErr error
	gate     chan struct{}
	failAt   int // NextPage call that errors, 0 never

	mu       sync.Mutex
	next     int
	canceled int
}

func (f *fakeScanner) Capabilities(context.Context) (Caps, error) { return Caps{}, nil }
func (f *fakeScanner) Status(context.Context) (State, error)      { return StateIdle, nil }

func (f *fakeScanner) Start(context.Context, Settings) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = 0
	return "/eSCL/ScanJobs/1", f.startErr
}

func (f *fakeScanner) NextPage(ctx context.Context, _ string) (io.ReadCloser, error) {
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAt > 0 && f.next+1 == f.failAt {
		return nil, errors.New("HTTP: 503 Service Unavailable")
	}
	if f.next >= len(f.pages) {
		return nil, io.EOF
	}
	f.next++
	return io.NopCloser(bytes.NewReader(f.pages[f.next-1])), nil
}

func (f *fakeScanner) Cancel(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled++
	return nil
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.now = t }

type harness struct {
	svc    *Service
	dir    string
	events chan apitypes.Scan
	clock  *clock
	scan   *fakeScanner
}

func newHarness(t *testing.T, pages int) *harness {
	t.Helper()
	h := &harness{
		dir:    t.TempDir(),
		events: make(chan apitypes.Scan, 64),
		clock:  &clock{now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)},
		scan:   &fakeScanner{},
	}
	for range pages {
		h.scan.pages = append(h.scan.pages, jpegBytes(t, 60, 80, false))
	}
	h.svc = h.newService(func() time.Duration { return 0 })
	return h
}

func (h *harness) newService(retention func() time.Duration) *Service {
	return New(Options{
		Dir:       h.dir,
		Scanner:   func(string) Scanner { return h.scan },
		Retention: retention,
		Events:    h.events,
		Now:       h.clock.Now,
	})
}

// wait reads events until scan id reaches state; it fails after 5 s.
func (h *harness) wait(t *testing.T, id, state string) []apitypes.Scan {
	t.Helper()
	var seen []apitypes.Scan
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-h.events:
			if e.ID != id {
				continue
			}
			seen = append(seen, e)
			if e.State == state {
				return seen
			}
		case <-deadline:
			t.Fatalf("scan %s did not reach %s; events %+v", id, state, seen)
		}
	}
}

func (h *harness) scanDone(t *testing.T, format string) apitypes.Scan {
	t.Helper()
	sc, err := h.svc.Start(Settings{Source: "adf", Resolution: 150, ColorMode: "color", Format: format})
	if err != nil {
		t.Fatal(err)
	}
	h.wait(t, sc.ID, ScanCompleted)
	got, err := h.svc.Get(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func download(t *testing.T, svc *Service, id string) (string, []byte) {
	t.Helper()
	d, err := svc.Download(id)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := d.Write(&b); err != nil {
		t.Fatal(err)
	}
	return d.Name, b.Bytes()
}

func TestServiceScanPDF(t *testing.T) {
	h := newHarness(t, 2)
	sc, err := h.svc.Start(Settings{Source: "adf", Resolution: 150, ColorMode: "color", Format: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if sc.State != ScanScanning || sc.Pages != 0 || sc.Format != "pdf" || sc.Error != nil {
		t.Fatalf("Start returned %+v", sc)
	}
	if !idPattern.MatchString(sc.ID) {
		t.Fatalf("id %q is not 16 hex characters", sc.ID)
	}
	var got []string
	for _, e := range h.wait(t, sc.ID, ScanCompleted) {
		got = append(got, e.State+"/"+strconv.Itoa(e.Pages))
	}
	if want := []string{"scanning/0", "scanning/1", "scanning/2", "completed/2"}; !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}

	for _, f := range []string{"meta.json", "page-1.jpg", "page-2.jpg", "scan.pdf"} {
		if _, err := os.Stat(filepath.Join(h.dir, sc.ID, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	name, body := download(t, h.svc, sc.ID)
	if !strings.HasSuffix(name, ".pdf") || !bytes.HasPrefix(body, []byte("%PDF-")) || !bytes.Contains(body, []byte("/Count 2")) {
		t.Fatalf("download %q is not a 2-page PDF", name)
	}
	page, err := h.svc.Page(sc.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(page)
	_ = page.Close()
	if !bytes.Equal(b, h.scan.pages[1]) {
		t.Fatal("page 2 differs from the scanned image")
	}
	if _, err := h.svc.Page(sc.ID, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("page 3: %v, want ErrNotFound", err)
	}
	if list := h.svc.List(); len(list) != 1 || list[0].ID != sc.ID || list[0].State != ScanCompleted {
		t.Fatalf("List = %+v", list)
	}
}

func TestServiceDownloadJPEG(t *testing.T) {
	h := newHarness(t, 1)
	sc := h.scanDone(t, "jpeg")
	name, body := download(t, h.svc, sc.ID)
	if !strings.HasSuffix(name, ".jpg") || !bytes.Equal(body, h.scan.pages[0]) {
		t.Fatalf("single-page jpeg download %q is not the page", name)
	}

	h.scan.pages = append(h.scan.pages, jpegBytes(t, 10, 10, true))
	sc = h.scanDone(t, "jpeg")
	name, body = download(t, h.svc, sc.ID)
	if !strings.HasSuffix(name, ".zip") {
		t.Fatalf("multi-page jpeg download is %q, want .zip", name)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 2 || zr.File[0].Name != "page-1.jpg" || zr.File[1].Name != "page-2.jpg" {
		t.Fatalf("zip entries %v", zr.File)
	}
	rc, _ := zr.File[1].Open()
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(b, h.scan.pages[1]) {
		t.Fatal("zip page 2 differs")
	}
}

func TestServiceBusy(t *testing.T) {
	h := newHarness(t, 1)
	h.scan.gate = make(chan struct{})
	set := Settings{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"}
	sc, err := h.svc.Start(set)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Start(set); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Start: %v, want ErrBusy", err)
	}
	if err := h.svc.Delete(sc.ID); !errors.Is(err, ErrRunning) {
		t.Fatalf("Delete running: %v, want ErrRunning", err)
	}
	if _, err := h.svc.Download(sc.ID); !errors.Is(err, ErrNotFinished) {
		t.Fatalf("Download running: %v, want ErrNotFinished", err)
	}
	close(h.scan.gate)
	h.wait(t, sc.ID, ScanCompleted)
	sc, err = h.svc.Start(set)
	if err != nil {
		t.Fatalf("Start after completion: %v", err)
	}
	h.wait(t, sc.ID, ScanCompleted)
}

func TestServiceDelete(t *testing.T) {
	h := newHarness(t, 1)
	sc := h.scanDone(t, "pdf")
	if err := h.svc.Delete(sc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, sc.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scan directory still there: %v", err)
	}
	if _, err := h.svc.Get(sc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
	if err := h.svc.Delete(sc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestServicePathTraversal(t *testing.T) {
	h := newHarness(t, 1)
	outside := filepath.Join(filepath.Dir(h.dir), "victim")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"..", "../victim", "../../etc", "0123456789abcdef/..", "0123456789ABCDEF", "", "/etc"} {
		if _, err := h.svc.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v", id, err)
		}
		if _, err := h.svc.Page(id, 1); !errors.Is(err, ErrNotFound) {
			t.Errorf("Page(%q) = %v", id, err)
		}
		if _, err := h.svc.Download(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Download(%q) = %v", id, err)
		}
		if err := h.svc.Delete(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Delete(%q) = %v", id, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("directory outside the scan dir was touched: %v", err)
	}
}

func TestServiceJanitorRetentionChanges(t *testing.T) {
	h := newHarness(t, 1)
	var mu sync.Mutex
	retention, calls := time.Duration(0), 0
	h.svc = h.newService(func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return retention
	})
	setRetention := func(d time.Duration) { mu.Lock(); retention = d; mu.Unlock() }

	t0 := h.clock.Now()
	old := h.scanDone(t, "pdf")
	h.clock.Set(t0.Add(48 * time.Hour))
	young := h.scanDone(t, "pdf")
	h.clock.Set(t0.Add(72 * time.Hour)) // old is 3 days old, young 1 day

	exists := func(id string) bool { _, err := h.svc.Get(id); return err == nil }

	h.svc.sweep() // retention 0 keeps everything
	if !exists(old.ID) || !exists(young.ID) {
		t.Fatal("retention 0 deleted a scan")
	}
	setRetention(48 * time.Hour)
	h.svc.sweep()
	if exists(old.ID) || !exists(young.ID) {
		t.Fatalf("retention 2d: old kept=%v young kept=%v, want only young", exists(old.ID), exists(young.ID))
	}
	if _, err := os.Stat(filepath.Join(h.dir, old.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old scan directory still on disk")
	}
	setRetention(12 * time.Hour)
	h.svc.sweep()
	if exists(young.ID) {
		t.Fatal("retention 12h kept a 1-day-old scan")
	}
	if calls != 3 {
		t.Fatalf("retention called %d times, want once per run (3)", calls)
	}
}

func TestServiceJanitorSkipsRunning(t *testing.T) {
	h := newHarness(t, 1)
	h.svc = h.newService(func() time.Duration { return time.Hour })
	h.scan.gate = make(chan struct{})
	sc, err := h.svc.Start(Settings{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	h.clock.Set(h.clock.Now().Add(24 * time.Hour))
	h.svc.sweep()
	if _, err := h.svc.Get(sc.ID); err != nil {
		t.Fatalf("janitor deleted the running scan: %v", err)
	}
	close(h.scan.gate)
	h.wait(t, sc.ID, ScanCompleted)
}

func TestServiceFailures(t *testing.T) {
	t.Run("scanner refuses the job", func(t *testing.T) {
		h := newHarness(t, 1)
		h.scan.startErr = errors.New("HTTP: 409 Conflict")
		sc, err := h.svc.Start(Settings{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"})
		if err != nil {
			t.Fatal(err)
		}
		ev := h.wait(t, sc.ID, ScanFailed)
		if last := ev[len(ev)-1]; last.Error == nil || !strings.Contains(*last.Error, "409") {
			t.Fatalf("failed event %+v lacks the scanner error", last)
		}
	})
	t.Run("scanner fails mid-job", func(t *testing.T) {
		h := newHarness(t, 3)
		h.scan.failAt = 2
		set := Settings{Source: "adf", Resolution: 300, ColorMode: "color", Format: "pdf"}
		sc, err := h.svc.Start(set)
		if err != nil {
			t.Fatal(err)
		}
		ev := h.wait(t, sc.ID, ScanFailed)
		if last := ev[len(ev)-1]; last.Error == nil || last.Pages != 1 {
			t.Fatalf("failed event %+v, want 1 page and an error", last)
		}
		h.scan.mu.Lock()
		canceled := h.scan.canceled
		h.scan.mu.Unlock()
		if canceled != 1 {
			t.Fatalf("eSCL job canceled %d times, want 1", canceled)
		}
		h.scan.failAt = 0
		sc, err = h.svc.Start(set)
		if err != nil {
			t.Fatalf("Start after a failure: %v", err)
		}
		h.wait(t, sc.ID, ScanCompleted)
	})
	t.Run("service context canceled", func(t *testing.T) {
		h := newHarness(t, 1)
		h.scan.gate = make(chan struct{})
		c, cancel := context.WithCancel(context.Background())
		svc := New(Options{Dir: h.dir, Scanner: func(string) Scanner { return h.scan }, Context: c})
		sc, err := svc.Start(Settings{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"})
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		deadline := time.Now().Add(5 * time.Second)
		for {
			got, _ := svc.Get(sc.ID)
			if got.State == ScanCanceled {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("scan state %q, want canceled", got.State)
			}
			time.Sleep(10 * time.Millisecond)
		}
		// meta.json is written before the scanner is released.
		b, err := os.ReadFile(filepath.Join(h.dir, sc.ID, "meta.json"))
		if err != nil || !strings.Contains(string(b), `"state":"canceled"`) {
			t.Fatalf("meta.json = %s, %v", b, err)
		}
	})
	t.Run("no pages", func(t *testing.T) {
		h := newHarness(t, 0)
		sc, err := h.svc.Start(Settings{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"})
		if err != nil {
			t.Fatal(err)
		}
		h.wait(t, sc.ID, ScanFailed)
		if _, err := h.svc.Download(sc.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Download of a failed scan: %v", err)
		}
	})
	t.Run("no scanner", func(t *testing.T) {
		h := newHarness(t, 1)
		svc := New(Options{Dir: h.dir, Scanner: func(string) Scanner { return nil }})
		if _, err := svc.Start(Settings{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"}); !errors.Is(err, ErrNoScanner) {
			t.Fatalf("Start: %v, want ErrNoScanner", err)
		}
	})
	t.Run("invalid settings", func(t *testing.T) {
		h := newHarness(t, 1)
		for _, set := range []Settings{
			{Source: "camera", Resolution: 300, ColorMode: "color", Format: "pdf"},
			{Source: "platen", Resolution: 10, ColorMode: "color", Format: "pdf"},
			{Source: "platen", Resolution: 300, ColorMode: "sepia", Format: "pdf"},
			{Source: "platen", Resolution: 300, ColorMode: "color", Format: "png"},
		} {
			if _, err := h.svc.Start(set); !errors.Is(err, ErrInvalidSettings) {
				t.Errorf("Start(%+v) = %v, want ErrInvalidSettings", set, err)
			}
		}
	})
	t.Run("unwritable directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can write anywhere")
		}
		h := newHarness(t, 1)
		if err := os.Chmod(h.dir, 0o500); err != nil { //nolint:gosec // the test needs a read-only directory
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(h.dir, 0o700) }) //nolint:gosec // restore for TempDir cleanup
		_, err := h.svc.Start(Settings{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"})
		var se *StorageError
		if !errors.As(err, &se) {
			t.Fatalf("Start: %v, want *StorageError", err)
		}
		// The refusal did not leave the service busy.
		if err := os.Chmod(h.dir, 0o700); err != nil { //nolint:gosec // writable again
			t.Fatal(err)
		}
		sc, err := h.svc.Start(Settings{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"})
		if err != nil {
			t.Fatalf("Start after fixing the directory: %v", err)
		}
		h.wait(t, sc.ID, ScanCompleted)
	})
}

func TestServiceReload(t *testing.T) {
	h := newHarness(t, 2)
	done := h.scanDone(t, "pdf")

	// A scan that was running when the process stopped.
	h.scan.gate = make(chan struct{})
	running, err := h.svc.Start(Settings{Source: "platen", Resolution: 300, ColorMode: "color", Format: "jpeg"})
	if err != nil {
		t.Fatal(err)
	}
	h.wait(t, running.ID, ScanScanning)

	svc := h.newService(nil)
	got, err := svc.Get(done.ID)
	if err != nil || got.State != ScanCompleted || got.Pages != 2 {
		t.Fatalf("reloaded completed scan = %+v, %v", got, err)
	}
	got, err = svc.Get(running.ID)
	if err != nil || got.State != ScanFailed || got.Error == nil {
		t.Fatalf("reloaded interrupted scan = %+v, %v; want failed", got, err)
	}
	if err := svc.Delete(running.ID); err != nil {
		t.Fatalf("Delete of the interrupted scan: %v", err)
	}
	// The first service's goroutine now fails to write into the deleted directory.
	close(h.scan.gate)
	h.wait(t, running.ID, ScanFailed)
}

func TestServiceCancel(t *testing.T) {
	h := newHarness(t, 3)
	h.scan.gate = make(chan struct{})
	sc, err := h.svc.Start(Settings{Source: "adf", Resolution: 300, ColorMode: "color", Format: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	select { // let one page through
	case h.scan.gate <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("the scanner never asked for a page")
	}
	deadline := time.Now().Add(5 * time.Second)
	for got, _ := h.svc.Get(sc.ID); got.Pages != 1; got, _ = h.svc.Get(sc.ID) {
		if time.Now().After(deadline) {
			t.Fatal("first page never landed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := h.svc.Cancel(sc.ID)
	if err != nil || got.ID != sc.ID {
		t.Fatalf("Cancel = %+v, %v", got, err)
	}
	ev := h.wait(t, sc.ID, ScanCanceled)
	if last := ev[len(ev)-1]; last.Pages != 0 || last.Error != nil {
		t.Fatalf("canceled event %+v, want no pages and no error", last)
	}
	h.scan.mu.Lock()
	canceled := h.scan.canceled
	h.scan.mu.Unlock()
	if canceled != 1 {
		t.Fatalf("eSCL job canceled %d times, want 1", canceled)
	}
	entries, err := os.ReadDir(filepath.Join(h.dir, sc.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "meta.json" {
			t.Errorf("canceled scan kept %s", e.Name())
		}
	}
	if _, err := h.svc.Download(sc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Download of a canceled scan: %v, want ErrNotFound", err)
	}
	// The record stays until it is deleted; a second Cancel finds it finished.
	if _, err := h.svc.Cancel(sc.ID); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Cancel of a canceled scan: %v, want ErrNotRunning", err)
	}
	if err := h.svc.Delete(sc.ID); err != nil {
		t.Fatalf("Delete of a canceled scan: %v", err)
	}
	if _, err := h.svc.Cancel(sc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Cancel of a deleted scan: %v, want ErrNotFound", err)
	}
	// The scanner is free again.
	h.scan.gate = nil
	h.scanDone(t, "pdf")
}

func TestServicePageLimit(t *testing.T) {
	h := newHarness(t, 2)
	h.svc.pageLimit = int64(len(h.scan.pages[0]) - 1)
	sc, err := h.svc.Start(Settings{Source: "adf", Resolution: 300, ColorMode: "color", Format: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	ev := h.wait(t, sc.ID, ScanFailed)
	if last := ev[len(ev)-1]; last.Error == nil || !strings.Contains(*last.Error, "larger than") || last.Pages != 0 {
		t.Fatalf("failed event %+v, want the page size error and no pages", last)
	}
	if _, err := os.Stat(filepath.Join(h.dir, sc.ID, pageName(1))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the truncated page was kept: %v", err)
	}
}

func TestServiceQuota(t *testing.T) {
	h := newHarness(t, 1)
	h.scanDone(t, "pdf")
	h.svc.quota = 1
	set := Settings{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"}
	if _, err := h.svc.Start(set); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("Start over the quota: %v, want ErrStorageFull", err)
	}
	h.svc.quota = 1 << 30
	sc, err := h.svc.Start(set)
	if err != nil {
		t.Fatalf("Start under the quota: %v", err)
	}
	h.wait(t, sc.ID, ScanCompleted)
	if MaxStoredBytes != 1<<30 || MaxPageBytes != 256<<20 {
		t.Fatalf("limits %d and %d, want 1 GiB and 256 MiB", MaxStoredBytes, MaxPageBytes)
	}
}

// TestServiceScannersByID: a scan runs on the scanner its settings name and
// records it; an unknown id gives ErrNoScanner.
func TestServiceScannersByID(t *testing.T) {
	h := newHarness(t, 0)
	a, b := &fakeScanner{pages: [][]byte{jpegBytes(t, 60, 80, false)}}, &fakeScanner{pages: [][]byte{jpegBytes(t, 60, 80, false)}}
	scanners := map[string]Scanner{"a": a, "b": b}
	svc := New(Options{Dir: h.dir, Events: h.events, Now: h.clock.Now, Scanner: func(id string) Scanner {
		if s, ok := scanners[id]; ok {
			return s
		}
		return nil
	}})
	h.svc = svc
	sc, err := svc.Start(Settings{Scanner: "b", Source: "platen", Resolution: 300, ColorMode: "color", Format: "jpeg"})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Scanner != "b" {
		t.Errorf("Start returned scanner %q, want b", sc.Scanner)
	}
	h.wait(t, sc.ID, ScanCompleted)
	a.mu.Lock()
	b.mu.Lock()
	aNext, bNext := a.next, b.next
	a.mu.Unlock()
	b.mu.Unlock()
	if aNext != 0 || bNext != 1 {
		t.Errorf("pages read: a %d, b %d; want only b", aNext, bNext)
	}
	if _, err := svc.Start(Settings{Scanner: "c", Source: "platen", Resolution: 300, ColorMode: "color", Format: "jpeg"}); !errors.Is(err, ErrNoScanner) {
		t.Errorf("Start on unknown scanner: %v, want ErrNoScanner", err)
	}

	// The scanner is persisted in the meta and survives a reload.
	got, err := h.newService(nil).Get(sc.ID)
	if err != nil || got.Scanner != "b" || got.Settings.Scanner != "b" {
		t.Errorf("reloaded scan = %+v, %v; want scanner b", got, err)
	}
}

// TestServiceLegacyMetaWithoutScanner: a meta.json written before scans named
// their scanner loads with scanner "".
func TestServiceLegacyMetaWithoutScanner(t *testing.T) {
	h := newHarness(t, 0)
	const id = "0123456789abcdef"
	if err := os.MkdirAll(filepath.Join(h.dir, id), 0o750); err != nil {
		t.Fatal(err)
	}
	legacy := `{"id":"` + id + `","state":"completed","createdAt":"2026-11-01T10:00:00Z","pages":1,"format":"jpeg",` +
		`"settings":{"source":"platen","resolution":300,"colorMode":"color","format":"jpeg"},"error":null}`
	if err := os.WriteFile(filepath.Join(h.dir, id, "meta.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := h.newService(nil).Get(id)
	if err != nil || got.Scanner != "" || got.State != ScanCompleted {
		t.Fatalf("legacy scan = %+v, %v; want loaded with scanner \"\"", got, err)
	}
}
