package api

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/printing"
)

func (e *testEnv) print(parts ...[3]string) (code int, body []byte) {
	e.t.Helper()
	b, ct := multipartBody(e.t, parts...)
	rec := e.do("POST", "/api/v1/print", b, "Content-Type", ct, "X-Authentik-Username", "alice")
	return rec.Code, rec.Body.Bytes()
}

// jobEvents collects job events from a hub subscriber.
type jobEvents struct {
	mu   sync.Mutex
	jobs []apitypes.Job
}

func collectJobs(t *testing.T, h *Hub) *jobEvents {
	t.Helper()
	ch, cancel := h.subscribe()
	t.Cleanup(cancel)
	je := &jobEvents{}
	go func() {
		for frame := range ch {
			if ev, data := parseFrame(t, frame); ev == apitypes.EventJob {
				var j apitypes.Job
				if err := json.Unmarshal(data, &j); err == nil {
					je.mu.Lock()
					je.jobs = append(je.jobs, j)
					je.mu.Unlock()
				}
			}
		}
	}()
	return je
}

func (je *jobEvents) states() []string {
	je.mu.Lock()
	defer je.mu.Unlock()
	var s []string
	for _, j := range je.jobs {
		s = append(s, j.State)
	}
	return s
}

// waitFor polls cond every 5 ms for up to 2 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPrintAccepted(t *testing.T) {
	e := newEnv(t)
	je := collectJobs(t, e.hub)
	code, body := e.print([3]string{"options", "", goodOptions}, [3]string{"file", `C:\Users\al\Report.pdf`, samplePDF})
	if code != 202 {
		t.Fatalf("%d %s", code, body)
	}
	job := decode[apitypes.Job](t, body)
	if job.ID != 1 || job.State != "pending" || job.Name != "Report.pdf" {
		t.Fatalf("job %+v", job)
	}
	sub := e.printer.submitted[0]
	if sub.name != "Report.pdf" || sub.user != "alice" || sub.format != printing.FormatPDF || sub.size != len(samplePDF) || sub.opts.Copies != 1 {
		t.Fatalf("submission %+v", sub)
	}
	waitFor(t, "job event", func() bool { return len(je.states()) == 1 })
}

func TestJobNameSanitised(t *testing.T) {
	long := strings.Repeat("é", 300)
	for in, want := range map[string]string{
		"/tmp/a\x07b.pdf": "ab.pdf",
		`C:\x\y.pdf`:      "y.pdf",
		"  ":              "document",
		"":                "document",
		long:              strings.Repeat("é", 255),
	} {
		if got := jobName(in); got != want {
			t.Errorf("jobName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrintMultipartOrder(t *testing.T) {
	e := newEnv(t)
	code, body := e.print([3]string{"file", "a.pdf", samplePDF}, [3]string{"options", "", goodOptions})
	if code != 400 || !bytes.Contains(body, []byte("multipart-order")) {
		t.Fatalf("file first: %d %s", code, body)
	}
	code, body = e.print([3]string{"options", "", goodOptions})
	if code != 400 || !bytes.Contains(body, []byte("multipart-order")) {
		t.Fatalf("no file: %d %s", code, body)
	}
	wantProblem(t, e.do("POST", "/api/v1/print", strings.NewReader(samplePDF), "Content-Type", "application/pdf"), 400, "multipart-order")
	if len(e.printer.submitted) != 0 {
		t.Fatal("submitted despite a bad request")
	}
}

func TestPrintUnsupportedFormat(t *testing.T) {
	e := newEnv(t)
	b, ct := multipartBody(t, [3]string{"options", "", goodOptions}, [3]string{"file", "a.txt", "hello\n"})
	p := wantProblem(t, e.do("POST", "/api/v1/print", b, "Content-Type", ct), 415, "unsupported-format")
	if !strings.Contains(p.Detail, printing.FormatPDF) || !strings.Contains(p.Detail, printing.FormatJPEG) {
		t.Fatalf("415 does not list the formats: %q", p.Detail)
	}
}

func TestPrintInvalidOptions(t *testing.T) {
	e := newEnv(t)
	b, ct := multipartBody(t, [3]string{"options", "", `{"copies":0,"sides":"x","colorMode":"auto","media":"","quality":"normal"}`},
		[3]string{"file", "a.pdf", samplePDF})
	p := wantProblem(t, e.do("POST", "/api/v1/print", b, "Content-Type", ct), 422, "invalid-options")
	if len(p.Errors) != 3 {
		t.Fatalf("errors %+v", p.Errors)
	}
	b, ct = multipartBody(t, [3]string{"options", "", `{"copies":1,"extra":true}`}, [3]string{"file", "a.pdf", samplePDF})
	wantProblem(t, e.do("POST", "/api/v1/print", b, "Content-Type", ct), 400, "bad-request")
}

func TestPrintNoPrinter(t *testing.T) {
	e := newEnv(t)
	e.printer.formatErr = ErrNoPrinter
	b, ct := multipartBody(t, [3]string{"options", "", goodOptions}, [3]string{"file", "a.pdf", samplePDF})
	wantProblem(t, e.do("POST", "/api/v1/print", b, "Content-Type", ct), 503, "printer-unavailable")
	e.printer.jobsErr = ErrNoPrinter
	wantProblem(t, e.do("GET", "/api/v1/jobs", nil), 503, "printer-unavailable")
}

func TestPrintUploadLimitFollowsConfig(t *testing.T) {
	e := newEnv(t)
	e.setConfig(func(c *config.Config) { c.Print.MaxUploadMiB = 1 })
	doc := samplePDF + strings.Repeat("x", 2<<20)
	b, ct := multipartBody(t, [3]string{"options", "", goodOptions}, [3]string{"file", "big.pdf", doc})
	raw, _ := io.ReadAll(b)
	p := wantProblem(t, e.do("POST", "/api/v1/print", bytes.NewReader(raw), "Content-Type", ct), 413, "too-large")
	if !strings.Contains(p.Detail, "1 MiB") {
		t.Fatalf("detail %q", p.Detail)
	}

	e.setConfig(func(c *config.Config) { c.Print.MaxUploadMiB = 3 })
	rec := e.do("POST", "/api/v1/print", bytes.NewReader(raw), "Content-Type", ct)
	if rec.Code != 202 {
		t.Fatalf("after raising the limit: %d %s", rec.Code, rec.Body)
	}
}

// scripted returns the states in order, then the last one forever.
func scripted(states ...string) func(int) (apitypes.Job, error) {
	var mu sync.Mutex
	i := 0
	return func(id int) (apitypes.Job, error) {
		mu.Lock()
		defer mu.Unlock()
		s := states[min(i, len(states)-1)]
		i++
		return apitypes.Job{ID: id, Name: "a.pdf", State: s, CreatedAt: time.Now()}, nil
	}
}

func TestJobWatcherPublishesChanges(t *testing.T) {
	e := newEnv(t)
	je := collectJobs(t, e.hub)
	e.printer.job = scripted("pending", "processing", "processing", "completed")
	if code, body := e.print([3]string{"options", "", goodOptions}, [3]string{"file", "a.pdf", samplePDF}); code != 202 {
		t.Fatalf("%d %s", code, body)
	}
	waitFor(t, "completed event", func() bool { s := je.states(); return len(s) > 0 && s[len(s)-1] == "completed" })
	calls := e.printer.calls()
	time.Sleep(60 * time.Millisecond)
	if got := je.states(); strings.Join(got, ",") != "pending,processing,completed" {
		t.Fatalf("events %v", got)
	}
	if e.printer.calls() != calls || calls != 4 {
		t.Fatalf("Job calls %d then %d, want 4 and none after completed", calls, e.printer.calls())
	}
}

func TestJobWatcherStops(t *testing.T) {
	cases := map[string]struct {
		job  func(int) (apitypes.Job, error)
		stop func(e *testEnv)
		mod  func(*Deps)
	}{
		"not found": {job: func(int) (apitypes.Job, error) { return apitypes.Job{}, printing.ErrNotFound }},
		"base context": {
			job:  scripted("processing"),
			stop: func(e *testEnv) { e.cancel() },
		},
		"timeout": {
			job: scripted("processing"),
			mod: func(d *Deps) { d.JobWatchTimeout = 50 * time.Millisecond },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			mods := []func(*Deps){}
			if tc.mod != nil {
				mods = append(mods, tc.mod)
			}
			e := newEnv(t, mods...)
			e.printer.job = tc.job
			if code, body := e.print([3]string{"options", "", goodOptions}, [3]string{"file", "a.pdf", samplePDF}); code != 202 {
				t.Fatalf("%d %s", code, body)
			}
			if tc.stop != nil {
				time.Sleep(30 * time.Millisecond)
				tc.stop(e)
			}
			// Stopped once the call count holds still over many poll intervals.
			waitFor(t, "watcher to stop", func() bool {
				before := e.printer.calls()
				time.Sleep(80 * time.Millisecond)
				return e.printer.calls() == before
			})
		})
	}
}

func TestDeleteJob(t *testing.T) {
	e := newEnv(t)
	je := collectJobs(t, e.hub)
	e.printer.job = scripted("canceled")
	rec := e.do("DELETE", "/api/v1/jobs/5", nil)
	if rec.Code != 204 || len(e.printer.canceled) != 1 || e.printer.canceled[0] != 5 {
		t.Fatalf("%d %v", rec.Code, e.printer.canceled)
	}
	waitFor(t, "canceled event", func() bool { return strings.Join(je.states(), ",") == "canceled" })

	wantProblem(t, e.do("DELETE", "/api/v1/jobs/abc", nil), 404, "not-found")
	for err, want := range map[error]struct {
		code int
		typ  string
	}{
		printing.ErrNotFound: {404, "not-found"},
		printing.ErrFinished: {409, "job-finished"},
		ErrNoPrinter:         {503, "printer-unavailable"},
	} {
		e.printer.cancelErr = err
		wantProblem(t, e.do("DELETE", "/api/v1/jobs/5", nil), want.code, want.typ)
	}
}

func TestGetJobs(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/api/v1/jobs", nil)
	if rec.Code != 200 || rec.Body.String() != "[]" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	e.printer.jobs = []apitypes.Job{{ID: 3, Name: "x", State: "completed", CreatedAt: time.Now()}}
	rec = e.do("GET", "/api/v1/jobs/3", nil)
	if rec.Code != 200 || decode[apitypes.Job](t, rec.Body.Bytes()).ID != 3 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	wantProblem(t, e.do("GET", "/api/v1/jobs/4", nil), 404, "not-found")
	wantProblem(t, e.do("GET", "/api/v1/jobs/0", nil), 404, "not-found")
	e.printer.job = func(int) (apitypes.Job, error) { return apitypes.Job{}, ErrNoPrinter }
	wantProblem(t, e.do("GET", "/api/v1/jobs/3", nil), 503, "printer-unavailable")
}

func pngFile(t *testing.T, w, h int) string {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func jpegFile(t *testing.T, w, h int) string {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// cmykJPEGFile is the header of a four-component JPEG; image/jpeg cannot encode CMYK.
func cmykJPEGFile() string {
	b := []byte{0xff, 0xd8, 0xff, 0xc0, 0, 20, 8, 0, 8, 0, 8, 4}
	for id := range byte(4) {
		b = append(b, id+1, 0x11, 0)
	}
	return string(append(b, 0xff, 0xda, 0, 14, 4, 1, 0, 2, 0, 3, 0, 4, 0, 0, 63, 0))
}

// pngHeaderFile is a PNG signature and IHDR (8-bit RGBA, w x h), no pixels.
func pngHeaderFile(w, h uint32) string {
	ihdr := binary.BigEndian.AppendUint32(nil, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	b := append([]byte("\x89PNG\r\n\x1a\n"), 0, 0, 0, 13)
	b = append(append(b, "IHDR"...), ihdr...)
	return string(binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(append([]byte("IHDR"), ihdr...))))
}

func TestPrintConverts(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	cases := []struct {
		name, file string
		formats    []string
		want       string // format submitted
		magic      string
	}{
		{"png to jpeg", pngFile(t, 30, 20), []string{printing.FormatPDF, printing.FormatJPEG}, printing.FormatJPEG, "\xff\xd8\xff"},
		{"png to pdf", pngFile(t, 30, 20), []string{printing.FormatPDF}, printing.FormatPDF, "%PDF-"},
		{"jpeg to pdf", jpegFile(t, 30, 20), []string{printing.FormatPDF, "image/urf"}, printing.FormatPDF, "%PDF-"},
		{"jpeg streamed", jpegFile(t, 30, 20), []string{printing.FormatJPEG}, printing.FormatJPEG, "\xff\xd8\xff"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.printer.formats = tc.formats
			code, body := e.print([3]string{"options", "", goodOptions}, [3]string{"file", "photo", tc.file})
			if code != 202 {
				t.Fatalf("%d %s", code, body)
			}
			sub := e.printer.submitted[0]
			if sub.format != tc.want || !bytes.HasPrefix(sub.head, []byte(tc.magic)) {
				t.Fatalf("submitted %s starting %q, want %s", sub.format, sub.head, tc.want)
			}
		})
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "pagewire-print-*")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func TestPrintConversionRefusals(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	for name, tc := range map[string]struct {
		file    string
		formats []string
		code    int
		typ     string
		detail  string
	}{
		"cmyk jpeg":     {cmykJPEGFile(), []string{printing.FormatPDF}, 415, "unsupported-format", "This JPEG uses a colour format the printer path cannot embed. Save it as an RGB JPEG or a PDF."},
		"broken png":    {"\x89PNG\r\n\x1a\nnot really", []string{printing.FormatPDF}, 400, "invalid-image", ""},
		"broken jpeg":   {"\xff\xd8\xffnot really", []string{printing.FormatPDF}, 400, "invalid-image", ""},
		"oversized png": {pngHeaderFile(20000, 20000), []string{printing.FormatJPEG}, 413, "image-too-large", "20000 x 20000"},
		"text":          {"hello\n", []string{printing.FormatPDF}, 415, "unsupported-format", printing.FormatPDF},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.printer.formats = tc.formats
			b, ct := multipartBody(t, [3]string{"options", "", goodOptions}, [3]string{"file", "x", tc.file})
			p := wantProblem(t, e.do("POST", "/api/v1/print", b, "Content-Type", ct), tc.code, tc.typ)
			if !strings.Contains(p.Detail, tc.detail) {
				t.Fatalf("detail %q lacks %q", p.Detail, tc.detail)
			}
			if len(e.printer.submitted) != 0 {
				t.Fatal("submitted a refused file")
			}
		})
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "pagewire-print-*")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func TestConversionBusy(t *testing.T) {
	rec := httptest.NewRecorder()
	conversionError(rec, printing.ErrBusy)
	wantProblem(t, rec, 429, "too-many-jobs")
}

func TestPrintWatcherLimit(t *testing.T) {
	e := newEnv(t)
	e.printer.job = scripted("processing") // never finishes: every watcher stays
	for i := range maxJobWatchers {
		if code, body := e.print([3]string{"options", "", goodOptions}, [3]string{"file", "a.pdf", samplePDF}); code != 202 {
			t.Fatalf("print %d: %d %s", i+1, code, body)
		}
	}
	code, body := e.print([3]string{"options", "", goodOptions}, [3]string{"file", "a.pdf", samplePDF})
	if code != 429 || !bytes.Contains(body, []byte("too-many-jobs")) {
		t.Fatalf("print %d: %d %s, want 429 too-many-jobs", maxJobWatchers+1, code, body)
	}
	if n := len(e.printer.submitted); n != maxJobWatchers {
		t.Fatalf("%d jobs submitted, want %d", n, maxJobWatchers)
	}
	// Finished watchers free their slots.
	e.printer.mu.Lock()
	e.printer.job = scripted("completed")
	e.printer.mu.Unlock()
	waitFor(t, "a free watcher slot", func() bool {
		code, _ := e.print([3]string{"options", "", goodOptions}, [3]string{"file", "a.pdf", samplePDF})
		return code == 202
	})
	if maxJobWatchers != 32 {
		t.Fatalf("maxJobWatchers = %d, want 32", maxJobWatchers)
	}
}

func TestPrinterErrorDetailIsGeneric(t *testing.T) {
	e := newEnv(t)
	e.printer.formatErr = errors.New("dial tcp 10.1.2.3:631: connection refused")
	b, ct := multipartBody(t, [3]string{"options", "", goodOptions}, [3]string{"file", "a.pdf", samplePDF})
	p := wantProblem(t, e.do("POST", "/api/v1/print", b, "Content-Type", ct), 503, "printer-unavailable")
	if strings.Contains(p.Detail, "10.1.2.3") {
		t.Fatalf("detail %q leaks the raw error", p.Detail)
	}
}
