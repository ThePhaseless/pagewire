package scanning

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/renameio/v2"

	"github.com/ThePhaseless/printscan/internal/apitypes"
)

// Scan states.
const (
	ScanScanning  = "scanning"
	ScanCompleted = "completed"
	ScanFailed    = "failed"
	ScanCanceled  = "canceled"
)

// Errors the API maps to status codes.
var (
	ErrBusy        = errors.New("a scan is already running") // 409
	ErrNoScanner   = errors.New("no scanner available")      // 503
	ErrNotFound    = errors.New("scan not found")            // 404
	ErrRunning     = errors.New("scan is still running")     // 409 on DELETE
	ErrNotFinished = errors.New("scan has not finished yet") // 409 on download
)

// StorageError is a failure to write the scan directory (API: 500 storage-error).
type StorageError struct{ Err error }

func (e *StorageError) Error() string { return "scan storage: " + e.Err.Error() }
func (e *StorageError) Unwrap() error { return e.Err }

const (
	// shortcut: one deadline for the whole job, all pages; make it per page if
	// long high-resolution ADF batches hit it.
	scanTimeout     = 10 * time.Minute
	janitorInterval = time.Hour
)

var idPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Options configures a Service.
type Options struct {
	Dir string // <data-dir>/scans
	// Scanner returns the current scanner, or a nil interface when there is none.
	Scanner func() Scanner
	// Retention is read on every janitor run; 0 (or a nil func) keeps scans forever.
	Retention func() time.Duration
	// Events receives every scan state change; the receiver must keep draining it.
	Events  chan<- apitypes.Scan
	Now     func() time.Time // default time.Now
	Context context.Context  // bounds running scans and event sends; default Background
}

// Service runs one scan at a time and keeps scans in Dir as
// <id>/meta.json, <id>/page-<n>.jpg and, for PDF, <id>/scan.pdf.
type Service struct {
	o       Options
	mu      sync.Mutex
	scans   map[string]*apitypes.Scan
	running string // id of the running scan, "" when idle
}

// New returns a Service and loads the scans already in o.Dir. A scan left
// "scanning" by a previous process is marked failed.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Context == nil {
		o.Context = context.Background()
	}
	s := &Service{o: o, scans: map[string]*apitypes.Scan{}}
	entries, err := os.ReadDir(o.Dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("cannot read scan directory", "dir", o.Dir, "err", err)
	}
	for _, e := range entries {
		if !idPattern.MatchString(e.Name()) {
			continue
		}
		var sc apitypes.Scan
		b, err := os.ReadFile(filepath.Join(o.Dir, e.Name(), "meta.json"))
		if err == nil {
			err = json.Unmarshal(b, &sc)
		}
		if err != nil || sc.ID != e.Name() {
			slog.Warn("skipping unreadable scan", "id", e.Name(), "err", err)
			continue
		}
		if sc.State == ScanScanning {
			sc.State, sc.Error = ScanFailed, ptr("interrupted by a restart")
			if err := s.writeMeta(sc); err != nil {
				slog.Warn("cannot update interrupted scan", "id", sc.ID, "err", err)
			}
		}
		s.scans[sc.ID] = &sc
	}
	return s
}

func ptr(s string) *string { return &s }

// Start validates set and starts a scan in the background. The returned Scan
// is in state scanning; progress arrives on Options.Events.
func (s *Service) Start(set Settings) (apitypes.Scan, error) {
	if err := Validate(set); err != nil {
		return apitypes.Scan{}, err
	}
	var sc Scanner
	if s.o.Scanner != nil {
		sc = s.o.Scanner()
	}
	if sc == nil {
		return apitypes.Scan{}, ErrNoScanner
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running != "" {
		return apitypes.Scan{}, ErrBusy
	}
	id, err := newID()
	if err != nil {
		return apitypes.Scan{}, err
	}
	scan := apitypes.Scan{ID: id, State: ScanScanning, CreatedAt: s.o.Now(), Format: set.Format, Settings: set}
	if err := os.MkdirAll(filepath.Join(s.o.Dir, id), 0o750); err != nil {
		return apitypes.Scan{}, &StorageError{err}
	}
	if err := s.writeMeta(scan); err != nil {
		_ = os.RemoveAll(filepath.Join(s.o.Dir, id))
		return apitypes.Scan{}, &StorageError{err}
	}
	s.scans[id] = &scan
	s.running = id
	go s.run(sc, scan)
	return scan, nil
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// run fetches every page, assembles the PDF and records the outcome.
func (s *Service) run(sc Scanner, scan apitypes.Scan) {
	s.emit(scan)
	ctx, cancel := context.WithTimeout(s.o.Context, scanTimeout)
	defer cancel()
	dir := filepath.Join(s.o.Dir, scan.ID)

	fail := func(job string, err error) {
		if job != "" {
			// The job context may be done; give the scanner its own short deadline.
			cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if cerr := sc.Cancel(cctx, job); cerr != nil {
				slog.Warn("cannot cancel eSCL job", "job", job, "err", cerr)
			}
			ccancel()
		}
		state := ScanFailed
		if s.o.Context.Err() != nil {
			state = ScanCanceled
		}
		s.finish(scan.ID, state, err)
	}

	job, err := sc.Start(ctx, scan.Settings)
	if err != nil {
		fail("", err)
		return
	}
	pages := 0
	for {
		body, err := sc.NextPage(ctx, job)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// shortcut: any error other than the end of the job (a 503 while the
			// scanner is still busy, too) fails the scan with no retry; add
			// sane-airscan-style retries if a real scanner needs them.
			fail(job, err)
			return
		}
		err = writeFile(filepath.Join(dir, pageName(pages+1)), body)
		_ = body.Close()
		if err != nil {
			fail(job, &StorageError{err})
			return
		}
		pages++
		s.mu.Lock()
		s.scans[scan.ID].Pages = pages
		cur := *s.scans[scan.ID]
		s.mu.Unlock()
		s.emit(cur)
	}
	if pages == 0 {
		s.finish(scan.ID, ScanFailed, errors.New("the scanner returned no pages"))
		return
	}
	if scan.Format == "pdf" {
		if err := s.writePDF(dir, pages, scan.Settings.Resolution); err != nil {
			s.finish(scan.ID, ScanFailed, err)
			return
		}
	}
	s.finish(scan.ID, ScanCompleted, nil)
}

func pageName(n int) string { return "page-" + strconv.Itoa(n) + ".jpg" }

func writeFile(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // path built from a validated id
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (s *Service) writePDF(dir string, pages, dpi int) error {
	imgs := make([][]byte, pages)
	for i := range imgs {
		b, err := os.ReadFile(filepath.Join(dir, pageName(i+1))) //nolint:gosec // path built from a validated id
		if err != nil {
			return &StorageError{err}
		}
		imgs[i] = b
	}
	f, err := renameio.NewPendingFile(filepath.Join(dir, "scan.pdf"), renameio.WithPermissions(0o600))
	if err != nil {
		return &StorageError{err}
	}
	defer func() { _ = f.Cleanup() }()
	if err := WriteJPEGPDF(f, imgs, dpi); err != nil {
		return err
	}
	if err := f.CloseAtomicallyReplace(); err != nil {
		return &StorageError{err}
	}
	return nil
}

// finish records the scan's final state, persists it and emits it.
func (s *Service) finish(id, state string, err error) {
	s.mu.Lock()
	sc := s.scans[id]
	sc.State = state
	if err != nil {
		sc.Error = ptr(err.Error())
	}
	cur := *sc
	// Written before running is cleared, so Delete and the janitor cannot race it.
	if err := s.writeMeta(cur); err != nil {
		slog.Warn("cannot write scan metadata", "id", id, "err", err)
	}
	s.running = ""
	s.mu.Unlock()
	s.emit(cur)
}

func (s *Service) writeMeta(sc apitypes.Scan) error {
	b, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	return renameio.WriteFile(filepath.Join(s.o.Dir, sc.ID, "meta.json"), b, 0o600)
}

func (s *Service) emit(sc apitypes.Scan) {
	if s.o.Events == nil {
		return
	}
	select {
	case s.o.Events <- sc:
	case <-s.o.Context.Done():
	}
}

// List returns every scan, newest first.
func (s *Service) List() []apitypes.Scan {
	s.mu.Lock()
	out := make([]apitypes.Scan, 0, len(s.scans))
	for _, sc := range s.scans {
		out = append(out, *sc)
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b apitypes.Scan) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	return out
}

// Get returns one scan, or ErrNotFound.
func (s *Service) Get(id string) (apitypes.Scan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !idPattern.MatchString(id) || s.scans[id] == nil {
		return apitypes.Scan{}, ErrNotFound
	}
	return *s.scans[id], nil
}

// Page opens JPEG page n (1-based) of a scan, or returns ErrNotFound.
func (s *Service) Page(id string, n int) (*os.File, error) {
	sc, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if n < 1 || n > sc.Pages {
		return nil, ErrNotFound
	}
	f, err := os.Open(filepath.Join(s.o.Dir, id, pageName(n))) //nolint:gosec // validated id and page number
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// Download is a finished scan's file: the PDF, the single JPEG page, or a ZIP
// of the JPEG pages built while it is written.
type Download struct {
	Name, ContentType string
	Write             func(w io.Writer) error
}

// Download returns the scan's file. A running scan gives ErrNotFinished; a
// failed or canceled one has no file and gives ErrNotFound.
func (s *Service) Download(id string) (Download, error) {
	sc, err := s.Get(id)
	if err != nil {
		return Download{}, err
	}
	switch sc.State {
	case ScanScanning:
		return Download{}, ErrNotFinished
	case ScanCompleted:
	default:
		return Download{}, ErrNotFound
	}
	dir := filepath.Join(s.o.Dir, id)
	copyFile := func(name string) func(io.Writer) error {
		return func(w io.Writer) error {
			f, err := os.Open(filepath.Join(dir, name)) //nolint:gosec // validated id
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			_, err = io.Copy(w, f)
			return err
		}
	}
	base := "scan-" + sc.CreatedAt.Format("20060102-150405")
	switch {
	case sc.Format == "pdf":
		return Download{base + ".pdf", "application/pdf", copyFile("scan.pdf")}, nil
	case sc.Pages == 1:
		return Download{base + ".jpg", "image/jpeg", copyFile(pageName(1))}, nil
	}
	return Download{base + ".zip", "application/zip", func(w io.Writer) error {
		zw := zip.NewWriter(w)
		for n := 1; n <= sc.Pages; n++ {
			// JPEG does not compress further; store the pages.
			fw, err := zw.CreateHeader(&zip.FileHeader{Name: pageName(n), Method: zip.Store, Modified: sc.CreatedAt})
			if err != nil {
				return err
			}
			if err := copyFile(pageName(n))(fw); err != nil {
				return err
			}
		}
		return zw.Close()
	}}, nil
}

// Delete removes a finished scan; a running one gives ErrRunning.
func (s *Service) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !idPattern.MatchString(id) || s.scans[id] == nil {
		return ErrNotFound
	}
	if s.running == id {
		return ErrRunning
	}
	if err := os.RemoveAll(filepath.Join(s.o.Dir, id)); err != nil {
		return &StorageError{err}
	}
	delete(s.scans, id)
	return nil
}

// RunJanitor deletes expired scans now and then every hour until ctx is done.
func (s *Service) RunJanitor(ctx context.Context) {
	t := time.NewTicker(janitorInterval)
	defer t.Stop()
	for {
		s.sweep()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sweep deletes every finished scan older than the current retention.
func (s *Service) sweep() {
	if s.o.Retention == nil {
		return
	}
	keep := s.o.Retention()
	if keep <= 0 {
		return
	}
	cutoff := s.o.Now().Add(-keep)
	for _, sc := range s.List() {
		if sc.CreatedAt.Before(cutoff) {
			if err := s.Delete(sc.ID); err != nil && !errors.Is(err, ErrRunning) && !errors.Is(err, ErrNotFound) {
				slog.Warn("janitor cannot delete scan", "id", sc.ID, "err", err)
			}
		}
	}
}
