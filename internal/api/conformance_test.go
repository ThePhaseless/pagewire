package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/config/store"
	"github.com/ThePhaseless/pagewire/internal/controller"
	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/printing"
	"github.com/ThePhaseless/pagewire/internal/scanning"
	"github.com/ThePhaseless/pagewire/internal/schema"
)

// The production types the server wires in must keep satisfying its interfaces.
var (
	_ StatusSource = (*controller.Controller)(nil)
	_ SetupSource  = (*controller.Controller)(nil)
	_ ConfigStore  = (*store.Store)(nil)
	_ interface {
		Submit(ctx context.Context, name, user, format string, r io.Reader, opts apitypes.PrintOptions) (int, error)
		Jobs(ctx context.Context, limit int) ([]apitypes.Job, error)
		Job(ctx context.Context, id int) (apitypes.Job, error)
		Cancel(ctx context.Context, id int) error
	} = (*printing.Client)(nil)
	_ interface {
		Start(set apitypes.ScanSettings) (apitypes.Scan, error)
		List() []apitypes.Scan
		Get(id string) (apitypes.Scan, error)
		Page(id string, n int) (*os.File, error)
		Download(id string) (scanning.Download, error)
		Delete(id string) error
	} = (*scanning.Service)(nil)
)

// ---- fakes shared by every test file ----

type statusFake struct {
	mu sync.Mutex
	st apitypes.Status
	ch chan apitypes.Status
}

func (f *statusFake) Current() apitypes.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *statusFake) Subscribe() (<-chan apitypes.Status, func()) { return f.ch, func() {} }

type setupFake struct {
	s     apitypes.Setup
	eulas map[string]apitypes.EULA // nil: the real controller's (pending)
}

func (f setupFake) Setup() apitypes.Setup { return f.s }

func (f setupFake) EULA(id string) (apitypes.EULA, bool) {
	if f.eulas == nil {
		return realController().EULA(id)
	}
	e, ok := f.eulas[id]
	return e, ok
}

type printerFake struct {
	mu        sync.Mutex
	formats   []string
	formatErr error
	submitted []submission
	submitErr error
	nextID    int
	// job answers Job; nil means the job from jobs, else ErrNotFound.
	job       func(id int) (apitypes.Job, error)
	jobs      []apitypes.Job
	jobsErr   error
	jobCalls  int
	cancelErr error
	canceled  []int
}

type submission struct {
	name, user, format string
	size               int
	opts               apitypes.PrintOptions
	head               []byte // first bytes of the document
}

func (f *printerFake) Formats(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.formats, f.formatErr
}

func (f *printerFake) Submit(_ context.Context, name, user, format string, r io.Reader, opts apitypes.PrintOptions) (int, error) {
	var head bytes.Buffer
	n, err := io.Copy(io.MultiWriter(io.Discard, &limitWriter{&head, 64}), r)
	if err != nil {
		// The real client returns its HTTP round trip's error, not the body's.
		return 0, errors.New("ipp: send document failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return 0, f.submitErr
	}
	f.nextID++
	f.submitted = append(f.submitted, submission{name, user, format, int(n), opts, head.Bytes()})
	return f.nextID, nil
}

// limitWriter keeps the first n bytes written to it.
type limitWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if room := l.n - l.w.Len(); room > 0 {
		l.w.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (f *printerFake) Jobs(context.Context, int) ([]apitypes.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobs, f.jobsErr
}

func (f *printerFake) Job(_ context.Context, id int) (apitypes.Job, error) {
	f.mu.Lock()
	f.jobCalls++
	fn := f.job
	jobs := f.jobs
	f.mu.Unlock()
	if fn != nil {
		return fn(id)
	}
	for _, j := range jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return apitypes.Job{}, printing.ErrNotFound
}

func (f *printerFake) Cancel(_ context.Context, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.canceled = append(f.canceled, id)
	return nil
}

func (f *printerFake) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobCalls
}

type scansFake struct {
	mu       sync.Mutex
	caps     apitypes.ScannerCaps
	capsErr  error
	startErr error
	scans    []apitypes.Scan
	pageDir  string // page files are <pageDir>/<id>-<n>.jpg
	dlErr    error
	running  string // id Delete refuses with ErrRunning and Cancel cancels
	canceled int
}

func (f *scansFake) Caps(context.Context) (apitypes.ScannerCaps, error) { return f.caps, f.capsErr }

func (f *scansFake) Start(set apitypes.ScanSettings) (apitypes.Scan, error) {
	if err := scanning.Validate(set); err != nil {
		return apitypes.Scan{}, err
	}
	if f.startErr != nil {
		return apitypes.Scan{}, f.startErr
	}
	sc := apitypes.Scan{ID: "0123456789abcdef", State: "scanning", CreatedAt: time.Now(), Format: set.Format, Settings: set}
	f.mu.Lock()
	f.scans = append(f.scans, sc)
	f.mu.Unlock()
	return sc, nil
}

func (f *scansFake) List() []apitypes.Scan {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scans
}

func (f *scansFake) Get(id string) (apitypes.Scan, error) {
	for _, sc := range f.List() {
		if sc.ID == id {
			return sc, nil
		}
	}
	return apitypes.Scan{}, scanning.ErrNotFound
}

func (f *scansFake) Page(id string, n int) (*os.File, error) {
	if _, err := f.Get(id); err != nil {
		return nil, err
	}
	fh, err := os.Open(filepath.Join(f.pageDir, id+"-"+strconv.Itoa(n)+".jpg")) //nolint:gosec // test temp dir
	if errors.Is(err, os.ErrNotExist) {
		return nil, scanning.ErrNotFound
	}
	return fh, err
}

func (f *scansFake) Download(id string) (scanning.Download, error) {
	if f.dlErr != nil {
		return scanning.Download{}, f.dlErr
	}
	if _, err := f.Get(id); err != nil {
		return scanning.Download{}, err
	}
	return scanning.Download{Name: "scan.pdf", ContentType: "application/pdf", Write: func(w io.Writer) error {
		_, err := io.WriteString(w, "%PDF-1.4\n")
		return err
	}}, nil
}

func (f *scansFake) Delete(id string) error {
	if _, err := f.Get(id); err != nil {
		return err
	}
	if id == f.running {
		return scanning.ErrRunning
	}
	return nil
}

func (f *scansFake) Cancel(id string) (apitypes.Scan, error) {
	sc, err := f.Get(id)
	if err != nil {
		return sc, err
	}
	if id != f.running {
		return apitypes.Scan{}, scanning.ErrNotRunning
	}
	f.canceled++
	return sc, nil
}

// ---- test environment ----

type testEnv struct {
	t        *testing.T
	path     string
	store    *store.Store
	status   *statusFake
	printer  *printerFake
	scans    *scansFake
	hub      *Hub
	mu       sync.Mutex
	cfg      config.Config
	lastGood *config.Config
	public   http.Handler
	internal http.Handler
	cancel   context.CancelFunc
}

func check(b []byte) (*config.Config, error) { return config.Check(b, catalog.Known()) }

func newEnv(t *testing.T, mod ...func(*Deps)) *testEnv {
	t.Helper()
	e := &testEnv{
		t:       t,
		path:    filepath.Join(t.TempDir(), "pagewire.yaml"),
		status:  &statusFake{st: emptyStatus(t), ch: make(chan apitypes.Status, 1)},
		printer: &printerFake{formats: []string{printing.FormatPDF, printing.FormatJPEG}},
		scans:   &scansFake{pageDir: t.TempDir()},
		hub:     NewHub(),
		cfg:     config.Default("Printer"),
	}
	e.store = store.New(e.path, func(b []byte) error { _, err := check(b); return err })
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	t.Cleanup(cancel)
	d := Deps{
		Store:  e.store,
		Check:  check,
		Config: func() config.Config { e.mu.Lock(); defer e.mu.Unlock(); return e.cfg },
		LastGood: func() *config.Config {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.lastGood
		},
		Status:          e.status,
		Setup:           setupFake{s: emptySetup(t)},
		Printer:         e.printer,
		Scans:           e.scans,
		Hub:             e.hub,
		BaseContext:     ctx,
		JobPollInterval: 10 * time.Millisecond,
	}
	for _, m := range mod {
		m(&d)
	}
	e.public, e.internal = New(d)
	return e
}

func (e *testEnv) setConfig(f func(*config.Config)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f(&e.cfg)
}

// do runs one request against the public handler.
func (e *testEnv) do(method, target string, body io.Reader, header ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequestWithContext(e.t.Context(), method, target, body)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	e.public.ServeHTTP(rec, req)
	return rec
}

func (e *testEnv) writeFile(s string) {
	e.t.Helper()
	if err := os.WriteFile(e.path, []byte(s), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) etag() string {
	e.t.Helper()
	_, etag, err := e.store.Read()
	if err != nil {
		e.t.Fatal(err)
	}
	return etag
}

// multipartBody builds a print request body from (name, filename, content) parts.
func multipartBody(t *testing.T, parts ...[3]string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		if p[1] != "" {
			fn := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p[1])
			h.Set("Content-Disposition", `form-data; name="`+p[0]+`"; filename="`+fn+`"`)
			h.Set("Content-Type", "application/octet-stream")
		} else {
			h.Set("Content-Disposition", `form-data; name="`+p[0]+`"`)
		}
		w, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, p[2])
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

const goodOptions = `{"copies":1,"sides":"one-sided","colorMode":"auto","media":"iso_a4_210x297mm","quality":"normal"}`

const samplePDF = "%PDF-1.4\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF\n"

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

// wantProblem checks status, problem type and that the body validates as a Problem.
func wantProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, typ string) apitypes.Problem {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type %q", ct)
	}
	validateDef(t, "Problem", rec.Body.Bytes())
	p := decode[apitypes.Problem](t, rec.Body.Bytes())
	if p.Type != typ || p.Status != status {
		t.Fatalf("problem %+v, want type %s", p, typ)
	}
	return p
}

// ---- schema validation ----

var (
	schemaOnce sync.Once
	schemaComp *jsonschema.Compiler
	schemaErr  error
	schemaMu   sync.Mutex
)

func compiled(t *testing.T, def string) *jsonschema.Schema {
	t.Helper()
	schemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema.API()))
		if err != nil {
			schemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		schemaErr = c.AddResource(schema.APIID, doc)
		for _, item := range []string{"Job", "Scan"} {
			arr := map[string]any{"type": "array", "items": map[string]any{"$ref": schema.APIID + "#/$defs/" + item}}
			if err := c.AddResource("https://pagewire.invalid/"+item+"-array.json", arr); err != nil {
				schemaErr = err
			}
		}
		schemaComp = c
	})
	if schemaErr != nil {
		t.Fatal(schemaErr)
	}
	url := schema.APIID + "#/$defs/" + def
	if item, ok := strings.CutSuffix(def, "[]"); ok {
		url = "https://pagewire.invalid/" + item + "-array.json"
	}
	schemaMu.Lock()
	defer schemaMu.Unlock()
	s, err := schemaComp.Compile(url)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// validateDef validates a JSON body against an API $defs entry ("Job[]" for an array).
func validateDef(t *testing.T, def string, body []byte) {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s: not JSON: %v\n%s", def, err, body)
	}
	if err := compiled(t, def).Validate(v); err != nil {
		t.Fatalf("%s does not validate: %v\n%s", def, err, body)
	}
}

// ---- states ----

type nopBackend struct{}

func (nopBackend) Apply(context.Context, *device.Spec) error { return nil }
func (nopBackend) Observe(context.Context) (device.Observation, error) {
	return device.Observation{Phase: "absent"}, nil
}

// realController is a controller before any input: the empty state.
func realController() *controller.Controller {
	return controller.New(controller.Options{Backend: nopBackend{}, BackendName: "sim", USBAvailable: true})
}

func emptyStatus(*testing.T) apitypes.Status { return realController().Current() }
func emptySetup(*testing.T) apitypes.Setup   { return realController().Setup() }

var cet = time.FixedZone("CET", 3600)

var textSHA = strings.Repeat("a", 64)

func populatedUSB(t *testing.T) (apitypes.Status, apitypes.Setup) {
	t.Helper()
	inv := catalog.Fixture("ippusb-mfp")
	inv.ProbedAt = time.Date(2026, 11, 24, 17, 0, 0, 123456789, cet)
	dec, err := catalog.Decide(inv, config.Default("x").Device)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Device == nil {
		t.Fatal("usb decision without device")
	}
	at := time.Date(2026, 11, 24, 18, 30, 0, 0, cet)
	st := apitypes.Status{
		Config: apitypes.StatusConfig{State: "applied", Etag: "abc", AppliedEtag: "abc", ChangedAt: at},
		Device: apitypes.StatusDevice{
			Phase: "ready", Mode: "usb", Decision: &dec, Inventory: &inv, Message: "",
			Advertised: []apitypes.AdvertisedService{{Type: "_ipp._tcp", Name: "MFP", Port: 60000}},
			Blobs:      []device.BlobStatus{{ID: "hplip-plugin", Version: "3.24.4", State: "installed"}},
		},
		Printer: apitypes.StatusPrinter{
			Available: true, URI: "ipp://192.168.1.20:60000/ipp/print", State: "idle",
			Formats: []string{printing.FormatPDF}, Media: []string{"iso_a4_210x297mm"},
			Markers: []apitypes.Marker{{Name: "Black", Color: "#000000", Level: 80}},
		},
		Scanner:   apitypes.StatusScanner{Available: true, URL: "http://192.168.1.20:60000/eSCL", State: "idle"},
		Reconcile: []apitypes.ReconcileItem{{Item: "device", OK: true, At: at}},
		Connect:   apitypes.Connect{DNSSDName: "MFP", IPPURIs: []string{"ipp://192.168.1.20:60000/ipp/print"}},
	}
	setup := apitypes.Setup{
		Backend: "k8s", USBResourceConfigured: true, Inventory: &inv, Decision: &dec,
		EULAs: []apitypes.SetupEULA{{
			ID: "hplip-plugin", Version: "3.24.4", Title: "HP", Vendor: "HP", Summary: "s",
			State: apitypes.EULAAvailable, TextSHA256: &textSHA,
		}},
		Suggested: config.Default("MFP"),
	}
	return st, setup
}

func populatedSimulated(t *testing.T) (apitypes.Status, apitypes.Setup) {
	t.Helper()
	sel := config.Default("x").Device
	sel.Mode = config.ModeSimulated
	dec, err := catalog.Decide(device.Inventory{}, sel)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Device != nil {
		t.Fatal("simulated decision with a device")
	}
	st := realController().Current()
	st.Config.State = "applied"
	st.Device.Phase, st.Device.Mode, st.Device.Decision = "ready", "simulated", &dec
	setup := realController().Setup()
	setup.Decision = &dec
	setup.Suggested.Device.Mode = config.ModeSimulated
	return st, setup
}

// routeDefs maps every public route to the $defs entry of its success body
// ("" for routes without a JSON success body). A route missing here fails.
var routeDefs = map[string]string{
	"GET /api/v1/config":               "ConfigResponse",
	"GET /api/v1/config/raw":           "",
	"PUT /api/v1/config":               "ConfigResponse",
	"PATCH /api/v1/config":             "ConfigResponse",
	"GET /api/v1/setup":                "Setup",
	"GET /api/v1/eulas/{id}":           "EULA",
	"GET /api/v1/status":               "Status",
	"GET /api/v1/events":               "",
	"POST /api/v1/print":               "Job",
	"GET /api/v1/jobs":                 "Job[]",
	"GET /api/v1/jobs/{id}":            "Job",
	"DELETE /api/v1/jobs/{id}":         "",
	"GET /api/v1/scanner":              "ScannerCaps",
	"POST /api/v1/scans":               "Scan",
	"GET /api/v1/scans":                "Scan[]",
	"GET /api/v1/scans/{id}":           "Scan",
	"GET /api/v1/scans/{id}/pages/{n}": "",
	"GET /api/v1/scans/{id}/download":  "",
	"DELETE /api/v1/scans/{id}":        "Scan", // 202 on a running scan; 204 has no body
	"/api/":                            "",
	"GET /healthz":                     "",
	"GET /readyz":                      "",
	"/":                                "",
}

type conformance struct {
	t       *testing.T
	e       *testEnv
	checked map[string]bool
}

// call runs a request for route pattern and validates the body: a problem
// against Problem, a success against the route's def.
func (c *conformance) call(pattern, method, target, body string, wantStatus int, header ...string) []byte {
	c.t.Helper()
	def, ok := routeDefs[pattern]
	if !ok {
		c.t.Fatalf("route %s missing from routeDefs", pattern)
	}
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	rec := c.e.do(method, target, r, header...)
	if rec.Code != wantStatus {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, target, rec.Code, wantStatus, rec.Body)
	}
	switch {
	case rec.Code == http.StatusPreconditionFailed:
		validateDef(c.t, "ConfigResponse", rec.Body.Bytes()) // the 412 body is the file now on disk
	case rec.Code >= 400:
		validateDef(c.t, "Problem", rec.Body.Bytes())
	case def != "" && rec.Code != http.StatusNoContent:
		validateDef(c.t, def, rec.Body.Bytes())
		c.checked[pattern] = true
	}
	return rec.Body.Bytes()
}

func TestConformanceRouteTable(t *testing.T) {
	for _, rt := range publicRoutes {
		if _, ok := routeDefs[rt.pattern]; !ok {
			t.Errorf("route %s has no entry in routeDefs", rt.pattern)
		}
	}
}

func TestConformanceEmpty(t *testing.T) {
	e := newEnv(t)
	c := &conformance{t: t, e: e, checked: map[string]bool{}}
	c.call("GET /api/v1/status", "GET", "/api/v1/status", "", 200)
	c.call("GET /api/v1/setup", "GET", "/api/v1/setup", "", 200)
	c.call("GET /api/v1/jobs", "GET", "/api/v1/jobs", "", 200)
	c.call("GET /api/v1/scans", "GET", "/api/v1/scans", "", 200)
	c.call("GET /api/v1/config", "GET", "/api/v1/config", "", 404)
	c.call("GET /api/v1/jobs/{id}", "GET", "/api/v1/jobs/7", "", 404)
	c.call("GET /api/v1/scans/{id}", "GET", "/api/v1/scans/0123456789abcdef", "", 404)
	e.scans.capsErr = scanning.ErrNoScanner
	c.call("GET /api/v1/scanner", "GET", "/api/v1/scanner", "", 503)
	if body := c.call("GET /api/v1/status", "GET", "/api/v1/status", "", 200); !bytes.Contains(body, []byte(`"mode":"none"`)) {
		t.Fatalf("empty Status is not the initial Status: %s", body)
	}
	// Every route again, with nil slices and zero values in the fakes.
	checked := runRoutes(t, realController().Current(), realController().Setup(), false)
	for pattern, def := range routeDefs {
		if def != "" && pattern != "GET /api/v1/jobs/{id}" && pattern != "GET /api/v1/scans/{id}" && pattern != "DELETE /api/v1/scans/{id}" && !checked[pattern] {
			t.Errorf("route %s (%s) never validated in the empty state", pattern, def)
		}
	}
}

func runPopulated(t *testing.T, st apitypes.Status, setup apitypes.Setup) map[string]bool {
	t.Helper()
	return runRoutes(t, st, setup, true)
}

// runRoutes walks every JSON route. Unpopulated, the fakes hold nil slices and
// zero values, and the by-id GETs answer 404.
func runRoutes(t *testing.T, st apitypes.Status, setup apitypes.Setup, populated bool) map[string]bool {
	t.Helper()
	jobs := []apitypes.Job{
		{ID: 1, Name: "a.pdf", State: "completed", Reasons: []string{"job-completed-successfully"}, CreatedAt: time.Date(2026, 11, 24, 9, 0, 0, 0, cet)},
		{ID: 2, Name: "b.pdf", State: "pending", CreatedAt: time.Date(2026, 11, 24, 9, 5, 0, 0, cet)},
	}
	errMsg := "jammed"
	scans := []apitypes.Scan{
		{
			ID: "00000000000000aa", State: "completed", CreatedAt: time.Date(2026, 11, 24, 10, 0, 0, 0, cet), Pages: 2, Format: "pdf",
			Settings: apitypes.ScanSettings{Source: "adf", Resolution: 300, ColorMode: "color", Format: "pdf"},
		},
		{
			ID: "00000000000000bb", State: "failed", CreatedAt: time.Date(2026, 11, 24, 10, 5, 0, 0, cet), Format: "jpeg",
			Settings: apitypes.ScanSettings{Source: "platen", Resolution: 150, ColorMode: "grayscale", Format: "jpeg"}, Error: &errMsg,
		},
	}
	fake := setupFake{s: setup}
	if populated {
		text := "licence\n"
		fake.eulas = map[string]apitypes.EULA{"hplip-plugin": {
			ID: "hplip-plugin", Version: "3.24.4", Title: "HP", Vendor: "HP", Summary: "s",
			State: apitypes.EULAAvailable, TextSHA256: &textSHA, Text: &text,
		}}
	}
	e := newEnv(t, func(d *Deps) { d.Setup = fake })
	e.status.st = st
	byID := 404
	if populated {
		e.printer.jobs = jobs
		e.scans.scans = scans
		e.scans.caps = apitypes.ScannerCaps{Sources: []string{"platen", "adf"}, Resolutions: []int{150, 300}, ColorModes: []string{"color"}, ADF: true}
		byID = 200
	} else {
		// No job to read back: POST /print answers with its fallback job.
		e.printer.job = func(int) (apitypes.Job, error) { return apitypes.Job{}, printing.ErrNotFound }
	}
	c := &conformance{t: t, e: e, checked: map[string]bool{}}
	c.call("GET /api/v1/config", "GET", "/api/v1/config", "", 404)

	c.call("GET /api/v1/status", "GET", "/api/v1/status", "", 200)
	c.call("GET /api/v1/setup", "GET", "/api/v1/setup", "", 200)
	c.call("GET /api/v1/eulas/{id}", "GET", "/api/v1/eulas/hplip-plugin", "", 200)
	c.call("GET /api/v1/eulas/{id}", "GET", "/api/v1/eulas/no-such-eula", "", 404)
	c.call("GET /api/v1/jobs", "GET", "/api/v1/jobs", "", 200)
	c.call("GET /api/v1/jobs/{id}", "GET", "/api/v1/jobs/2", "", byID)
	c.call("GET /api/v1/scanner", "GET", "/api/v1/scanner", "", 200)
	c.call("GET /api/v1/scans", "GET", "/api/v1/scans", "", 200)
	c.call("GET /api/v1/scans/{id}", "GET", "/api/v1/scans/00000000000000bb", "", byID)
	c.call("POST /api/v1/scans", "POST", "/api/v1/scans", `{"source":"platen","resolution":300,"colorMode":"color","format":"pdf"}`, 202)
	c.call("POST /api/v1/scans", "POST", "/api/v1/scans", `{"source":"glass","resolution":300,"colorMode":"color","format":"pdf"}`, 422)
	if populated {
		e.scans.running = "00000000000000bb"
		c.call("DELETE /api/v1/scans/{id}", "DELETE", "/api/v1/scans/00000000000000bb", "", 202)
		e.scans.running = ""
		c.call("DELETE /api/v1/scans/{id}", "DELETE", "/api/v1/scans/00000000000000bb", "", 204)
	}

	body, ct := multipartBody(t, [3]string{"options", "", goodOptions}, [3]string{"file", "a.pdf", samplePDF})
	b, _ := io.ReadAll(body)
	c.call("POST /api/v1/print", "POST", "/api/v1/print", string(b), 202, "Content-Type", ct)

	// Config: applied without device.usb, PUT and PATCH success, both 412s, invalid.
	c.call("PUT /api/v1/config", "PUT", "/api/v1/config", `{"apiVersion":"pagewire/v1","name":"Office","device":{"mode":"simulated"}}`, 201,
		"If-None-Match", "*")
	body1 := c.call("GET /api/v1/config", "GET", "/api/v1/config", "", 200)
	if bytes.Contains(body1, []byte(`"usb"`)) {
		t.Fatalf("config without device.usb has a usb key: %s", body1)
	}
	stale := e.etag()
	c.call("PATCH /api/v1/config", "PATCH", "/api/v1/config", `{"location":"Hall"}`, 200,
		"If-Match", quote(stale), "Content-Type", "application/merge-patch+json")
	c.call("PUT /api/v1/config", "PUT", "/api/v1/config", `{"apiVersion":"pagewire/v1","name":"Office 2"}`, 200, "If-Match", quote(e.etag()))
	c.call("PUT /api/v1/config", "PUT", "/api/v1/config", `{"apiVersion":"pagewire/v1","name":"x"}`, 412, "If-Match", quote(stale))
	c.call("PUT /api/v1/config", "PUT", "/api/v1/config", `{"apiVersion":"pagewire/v1","name":"x","bogus":1}`, 422, "If-Match", quote(e.etag()))
	e.writeFile("apiVersion: pagewire/v1\nname: [\n")
	rec := e.do("PATCH", "/api/v1/config", strings.NewReader(`{"name":"y"}`), "If-Match", quote(stale), "Content-Type", "application/merge-patch+json")
	if rec.Code != 412 {
		t.Fatalf("412 against invalid file: %d %s", rec.Code, rec.Body)
	}
	validateDef(t, "ConfigResponse", rec.Body.Bytes())
	invalid := c.call("GET /api/v1/config", "GET", "/api/v1/config", "", 200)
	if !bytes.Contains(invalid, []byte(`"config":null`)) || !bytes.Contains(invalid, []byte(`"lastGood":null`)) {
		t.Fatalf("invalid config response: %s", invalid)
	}
	return c.checked
}

func TestConformancePopulatedUSB(t *testing.T) {
	st, setup := populatedUSB(t)
	checked := runPopulated(t, st, setup)
	for pattern, def := range routeDefs {
		if def != "" && !checked[pattern] {
			t.Errorf("route %s (%s) never validated", pattern, def)
		}
	}
}

func TestConformancePopulatedSimulated(t *testing.T) {
	st, setup := populatedSimulated(t)
	runPopulated(t, st, setup)
	e := newEnv(t, func(d *Deps) { d.Setup = setupFake{s: setup} })
	e.status.st = st
	for _, target := range []string{"/api/v1/status", "/api/v1/setup"} {
		rec := e.do("GET", target, nil)
		if !bytes.Contains(rec.Body.Bytes(), []byte(`"device":null`)) {
			t.Fatalf("%s: decision.device is not null: %s", target, rec.Body)
		}
	}
}

func TestConformanceSSEPayloads(t *testing.T) {
	usb, _ := populatedUSB(t)
	errMsg := "x"
	payloads := []struct {
		event, def string
		v          any
	}{
		{apitypes.EventStatus, "Status", realController().Current()},
		{apitypes.EventStatus, "Status", usb},
		{apitypes.EventConfig, "ConfigEvent", apitypes.ConfigEvent{Etag: "abc"}},
		{apitypes.EventJob, "Job", apitypes.Job{ID: 1, Name: "a", State: "pending", CreatedAt: time.Now().In(cet)}},
		{apitypes.EventScan, "Scan", apitypes.Scan{
			ID: "00000000000000aa", State: "failed", CreatedAt: time.Now().In(cet), Format: "pdf",
			Settings: apitypes.ScanSettings{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"}, Error: &errMsg,
		}},
	}
	for _, p := range payloads {
		frame, err := sseFrame(p.event, p.v)
		if err != nil {
			t.Fatal(err)
		}
		ev, data := parseFrame(t, frame)
		if ev != p.event {
			t.Fatalf("event %q, want %q", ev, p.event)
		}
		validateDef(t, p.def, data)
	}
}

// parseFrame splits one SSE frame into its event name and data.
func parseFrame(t *testing.T, frame []byte) (string, []byte) {
	t.Helper()
	var ev string
	var data []byte
	for line := range strings.SplitSeq(strings.TrimSpace(string(frame)), "\n") {
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			ev = v
		} else if v, ok := strings.CutPrefix(line, "data: "); ok {
			data = []byte(v)
		}
	}
	return ev, data
}
