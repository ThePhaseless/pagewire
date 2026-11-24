package api

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThePhaseless/printscan/internal/apitypes"
	"github.com/ThePhaseless/printscan/internal/config"
	"github.com/ThePhaseless/printscan/internal/printing"
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
