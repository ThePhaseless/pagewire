package printing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/OpenPrinting/go-mfp/abstract"
	"github.com/OpenPrinting/go-mfp/proto/ipp"
	"github.com/OpenPrinting/go-mfp/transport"
	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/ippcaps"
)

// recorder is the virtual printer's abstract.Printer backend plus a hook
// that records every IPP request the client sends.
type recorder struct {
	mu       sync.Mutex
	requests []*goipp.Message
	params   []abstract.PrinterRequest
	docs     [][]byte
}

func (r *recorder) PrintDocument(p abstract.PrinterRequest, body io.Reader) error {
	b, err := io.ReadAll(body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.params = append(r.params, p)
	r.docs = append(r.docs, b)
	return err
}

func (r *recorder) onRequest(_ *transport.ServerQuery, msg *goipp.Message) *goipp.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, msg)
	return nil
}

func (r *recorder) received() ([]abstract.PrinterRequest, [][]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.params), slices.Clone(r.docs)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *recorder) lastRequest(op goipp.Op) *goipp.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.requests) - 1; i >= 0; i-- {
		if goipp.Op(r.requests[i].Code) == op {
			return r.requests[i]
		}
	}
	return nil
}

var testFormats = []string{"application/pdf", "image/jpeg", "image/pwg-raster", "image/urf"}

// newVirtualPrinter serves go-mfp's virtual IPP printer and returns a client for it.
func newVirtualPrinter(t *testing.T) (*Client, *recorder, string) {
	t.Helper()
	rec := &recorder{}
	attrs := &ipp.PrinterAttributes{}
	idle := 3
	attrs.PrinterState = &idle
	attrs.PrinterStateReasons = []ipp.KwPrinterStateReasons{"media-low-warning"}
	attrs.DocumentFormatSupported = testFormats
	attrs.MediaSupported = []ipp.KwMedia{"iso_a4_210x297mm", "na_letter_8.5x11in"}
	attrs.MarkerNames = []string{"Black Toner", "Cyan Toner"}
	attrs.MarkerColors = []string{"#000000", "#00FFFF"}
	attrs.MarkerLevels = []int{80, -2}

	printer := ipp.NewPrinter(attrs, ipp.PrinterOptions{
		ServerOptions: ipp.ServerOptions{Hooks: ipp.ServerHooks{OnIPPRequest: rec.onRequest}},
	})
	printer.SetPrintBackend(rec)
	srv := httptest.NewServer(printer)
	t.Cleanup(srv.Close)
	srvURL := "ipp://" + strings.TrimPrefix(srv.URL, "http://") + "/ipp/print"
	attrs.PrinterURISupported = []string{srvURL}
	return New(srvURL), rec, srvURL
}

func TestAttributes(t *testing.T) {
	c, _, uri := newVirtualPrinter(t)
	a, err := c.Attributes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Attributes{
		State:   "idle",
		Reasons: []string{"media-low-warning"},
		Formats: testFormats,
		Media:   []string{"iso_a4_210x297mm", "na_letter_8.5x11in"},
		Markers: []apitypes.Marker{{Name: "Black Toner", Color: "#000000", Level: 80}, {Name: "Cyan Toner", Color: "#00FFFF", Level: -2}},
		URIs:    []string{uri},
	}
	if a.State != want.State || !slices.Equal(a.Reasons, want.Reasons) || !slices.Equal(a.Formats, want.Formats) ||
		!slices.Equal(a.Media, want.Media) || !slices.Equal(a.Markers, want.Markers) || !slices.Equal(a.URIs, want.URIs) {
		t.Errorf("Attributes() = %+v\nwant %+v", a, want)
	}
}

func TestSubmitStreamsDocumentWithOptions(t *testing.T) {
	c, rec, _ := newVirtualPrinter(t)
	ctx := context.Background()
	pdf := readTestdata(t, "sample.pdf")
	opts := PrintOptions{Copies: 2, Sides: "two-sided-long-edge", ColorMode: "monochrome", Media: "iso_a4_210x297mm", Quality: "high"}

	id, err := c.Submit(ctx, "report.pdf", "alice", FormatPDF, bytes.NewReader(pdf), opts)
	if err != nil {
		t.Fatal(err)
	}
	if id <= 0 {
		t.Fatalf("job id = %d", id)
	}

	// The backend received exactly the file: the IPP message and the
	// document were joined at the right boundary.
	params, docs := rec.received()
	if len(docs) != 1 || !bytes.Equal(docs[0], pdf) {
		t.Fatalf("backend got %d documents; want exactly the %d-byte sample", len(docs), len(pdf))
	}
	if params[0].Format != FormatPDF || params[0].JobName != "report.pdf" {
		t.Errorf("backend params = %+v", params[0])
	}

	// Options travel as job template attributes of Create-Job.
	cj := rec.lastRequest(goipp.OpCreateJob)
	if cj == nil {
		t.Fatal("no Create-Job request")
	}
	wantJob := map[string]string{
		"copies":           "2",
		"sides":            "two-sided-long-edge",
		"print-color-mode": "monochrome",
		"media":            "iso_a4_210x297mm",
		"print-quality":    "5",
	}
	for name, v := range wantJob {
		if got := attrString(cj.Job, name); got != v {
			t.Errorf("Create-Job %s = %q, want %q", name, got, v)
		}
	}
	if got := attrString(cj.Operation, "requesting-user-name"); got != "alice" {
		t.Errorf("requesting-user-name = %q", got)
	}
	if got := attrString(cj.Operation, "job-name"); got != "report.pdf" {
		t.Errorf("job-name = %q", got)
	}
	sd := rec.lastRequest(goipp.OpSendDocument)
	if got := attrString(sd.Operation, "last-document"); got != "true" {
		t.Errorf("Send-Document last-document = %q", got)
	}
	if got := attrString(sd.Operation, "document-format"); got != FormatPDF {
		t.Errorf("Send-Document document-format = %q", got)
	}

	// go-mfp's virtual printer (b3cc636803df) never moves a job past
	// pending-held after Send-Document: handleSendDocument only clears
	// SendDocumentActive, and only Cancel-Job reaches a terminal state. The
	// abstract.Printer backend gets no job handle, so it cannot set completed
	// either. We assert what it reports.
	j, err := c.Job(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if j.ID != id || j.Name != "report.pdf" || j.State != "held" || !slices.Equal(j.Reasons, []string{"job-incoming"}) {
		t.Errorf("Job() = %+v, want held/job-incoming (go-mfp never completes jobs)", j)
	}
	if !j.CreatedAt.IsZero() {
		t.Errorf("CreatedAt = %v; go-mfp sends no date-time-at-creation, want zero", j.CreatedAt)
	}
}

func TestSubmitWithoutOptionsSendsNoJobGroup(t *testing.T) {
	c, rec, _ := newVirtualPrinter(t)
	if _, err := c.Submit(context.Background(), "a", "web", FormatJPEG, bytes.NewReader(readTestdata(t, "sample.jpg")), PrintOptions{}); err != nil {
		t.Fatal(err)
	}
	if cj := rec.lastRequest(goipp.OpCreateJob); len(cj.Job) != 0 {
		t.Errorf("Create-Job job group = %v, want none", cj.Job)
	}
}

func TestJobsCancelAndErrors(t *testing.T) {
	c, _, _ := newVirtualPrinter(t)
	ctx := context.Background()
	var ids []int
	for _, name := range []string{"one", "two", "three"} {
		id, err := c.Submit(ctx, name, "web", FormatPDF, strings.NewReader("%PDF-1.4\n"), PrintOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	if err := c.Cancel(ctx, ids[2]); err != nil {
		t.Fatal(err)
	}
	j, err := c.Job(ctx, ids[2])
	if err != nil {
		t.Fatal(err)
	}
	if j.State != "canceled" {
		t.Errorf("canceled job state = %q", j.State)
	}
	if err := c.Cancel(ctx, ids[2]); !errors.Is(err, ErrFinished) {
		t.Errorf("second Cancel = %v, want ErrFinished", err)
	}
	if err := c.Cancel(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Cancel(999) = %v, want ErrNotFound", err)
	}
	if _, err := c.Job(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Job(999) = %v, want ErrNotFound", err)
	}
	// An id past int32 must not wrap around to an existing job.
	if err := c.Cancel(ctx, ids[0]+1<<32); !errors.Is(err, ErrNotFound) {
		t.Errorf("Cancel(id+2^32) = %v, want ErrNotFound", err)
	}

	// Jobs merges active and finished jobs, newest first, cut to limit.
	all, err := c.Jobs(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, j := range all {
		got = append(got, j.ID)
	}
	if want := []int{ids[2], ids[1], ids[0]}; !slices.Equal(got, want) {
		t.Fatalf("Jobs(20) ids = %v, want %v", got, want)
	}
	if all[0].State != "canceled" || all[1].Name != "two" || all[1].State != "held" {
		t.Errorf("Jobs(20) = %+v", all)
	}
	two, err := c.Jobs(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(two) != 2 || two[0].ID != ids[2] || two[1].ID != ids[1] {
		t.Errorf("Jobs(2) = %+v", two)
	}
}

func TestUnsupportedFormatRefusedBeforeAnyIPPCall(t *testing.T) {
	c, rec, _ := newVirtualPrinter(t)
	a, err := c.Attributes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := rec.count()

	// The caller (the API's print handler) negotiates before submitting.
	head := []byte("just some text\n")
	_, err = Negotiate(Detect(head, "notes.txt"), a.Formats)
	var ufe *UnsupportedFormatError
	if !errors.As(err, &ufe) || ufe.Detected != FormatText || !slices.Equal(ufe.Supported, testFormats) {
		t.Fatalf("Negotiate = %v, want UnsupportedFormatError for text/plain", err)
	}
	if rec.count() != before {
		t.Errorf("IPP requests went from %d to %d during negotiation", before, rec.count())
	}
}

func TestTypedErrors(t *testing.T) {
	var cancels atomic.Int32
	var status atomic.Value // goipp.Status
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := &goipp.Message{}
		if err := req.Decode(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Read the whole document first, so a failing upload reaches the
		// client as a write error rather than racing this response.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		if goipp.Op(req.Code) == goipp.OpCancelJob {
			cancels.Add(1)
		}
		if r.URL.Path == "/http-error" {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		rsp := goipp.NewResponse(goipp.DefaultVersion, status.Load().(goipp.Status), req.RequestID)
		rsp.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
		rsp.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
		if goipp.Op(req.Code) == goipp.OpCreateJob {
			rsp.Job.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(7)))
		}
		w.Header().Set("Content-Type", goipp.ContentType)
		_ = rsp.Encode(w)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	ctx := context.Background()

	status.Store(goipp.StatusErrorInternal)
	_, err := New("ipp://" + host + "/ipp/print").Attributes(ctx)
	var ie *IPPError
	if !errors.As(err, &ie) || ie.Status != goipp.StatusErrorInternal {
		t.Errorf("Attributes() = %v, want *IPPError{server-error-internal-error}", err)
	}

	_, err = New("ipp://" + host + "/http-error").Attributes(ctx)
	if err == nil || errors.As(err, &ie) {
		t.Errorf("HTTP 403 gave %v, want a plain error", err)
	}

	if _, err := New("ftp://" + host + "/x").Attributes(ctx); err == nil {
		t.Error("ftp URI accepted")
	}

	// A failed Send-Document cancels the job it created, so nothing is
	// left held in the queue.
	status.Store(goipp.StatusOk)
	c := New("ipp://" + host + "/ipp/print")
	failing := io.MultiReader(strings.NewReader("%PDF-"), errReader{})
	if _, err := c.Submit(ctx, "x", "web", FormatPDF, failing, PrintOptions{}); err == nil {
		t.Error("Submit with a failing reader succeeded")
	}
	if cancels.Load() != 1 {
		t.Errorf("Cancel-Job sent %d times after a failed Send-Document, want 1", cancels.Load())
	}
}

// TestJobsDeduplicatesAJobFinishingBetweenRequests: Jobs asks for
// not-completed then completed jobs; one that finished in between is listed
// once, with its completed state.
func TestJobsDeduplicatesAJobFinishingBetweenRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := &goipp.Message{}
		if err := req.Decode(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		job := func(id, state int32) goipp.Group {
			return goipp.Group{Tag: goipp.TagJobGroup, Attrs: goipp.Attributes{
				goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(id)),
				goipp.MakeAttribute("job-state", goipp.TagEnum, goipp.Integer(state)),
			}}
		}
		groups := goipp.Groups{{Tag: goipp.TagOperationGroup}}
		if attrString(req.Operation, "which-jobs") == "completed" {
			groups = append(groups, job(4, 9)) // job 4 finished meanwhile
		} else {
			groups = append(groups, job(4, 5), job(3, 3))
		}
		rsp := goipp.NewMessageWithGroups(goipp.DefaultVersion, goipp.Code(goipp.StatusOk), req.RequestID, groups)
		w.Header().Set("Content-Type", goipp.ContentType)
		_ = rsp.Encode(w)
	}))
	defer srv.Close()
	jobs, err := New("ipp://"+strings.TrimPrefix(srv.URL, "http://")+"/ipp/print").Jobs(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != 4 || jobs[0].State != "completed" || jobs[1].ID != 3 || jobs[1].State != "pending" {
		t.Errorf("Jobs() = %+v, want job 4 completed once, then job 3 pending", jobs)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

func attrString(attrs goipp.Attributes, name string) string {
	for _, a := range attrs {
		if a.Name == name && len(a.Values) > 0 {
			return a.Values[0].V.String()
		}
	}
	return ""
}

func TestResponseSizeLimit(t *testing.T) {
	big := strings.Repeat("x", 32000)
	respond := func(values int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			req := &goipp.Message{}
			if err := req.Decode(r.Body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			rsp := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, req.RequestID)
			attr := goipp.MakeAttribute("printer-state-reasons", goipp.TagKeyword, goipp.String(big))
			for range values - 1 {
				attr.Values.Add(goipp.TagKeyword, goipp.String(big))
			}
			rsp.Printer.Add(attr)
			w.Header().Set("Content-Type", goipp.ContentType)
			_ = rsp.Encode(w)
		}
	}
	// 3 values: under the limit; 300 values (about 9.6 MB): over it.
	for _, tc := range []struct {
		values int
		ok     bool
	}{{3, true}, {300, false}} {
		srv := httptest.NewServer(respond(tc.values))
		_, err := New("ipp://" + strings.TrimPrefix(srv.URL, "http://") + "/ipp/print").Attributes(context.Background())
		srv.Close()
		if (err == nil) != tc.ok {
			t.Errorf("%d values of 32000 bytes: err = %v, want ok=%v", tc.values, err, tc.ok)
		}
	}
	if maxResponseBytes != 8<<20 {
		t.Errorf("maxResponseBytes = %d, want 8 MiB", maxResponseBytes)
	}
}

// ippServer serves raw IPP: respond builds the response to each decoded
// request, and every request is kept in order.
func ippServer(t *testing.T, respond func(req *goipp.Message) *goipp.Message) (*Client, func() []*goipp.Message) {
	t.Helper()
	var mu sync.Mutex
	var reqs []*goipp.Message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := &goipp.Message{}
		if err := req.Decode(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body) // the document, if any
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()
		w.Header().Set("Content-Type", goipp.ContentType)
		_ = respond(req).Encode(w)
	}))
	t.Cleanup(srv.Close)
	return New("ipp://" + strings.TrimPrefix(srv.URL, "http://") + "/ipp/print"), func() []*goipp.Message {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(reqs)
	}
}

// TestAttributesSamsungCaps: a printer reporting what the Samsung SCX-4300's
// queue reports gives exactly those caps, and nothing for the other kinds.
func TestAttributesSamsungCaps(t *testing.T) {
	c, reqs := ippServer(t, func(req *goipp.Message) *goipp.Message {
		rsp := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, req.RequestID)
		kw := func(name string, vs ...string) {
			a := goipp.Attribute{Name: name}
			for _, v := range vs {
				a.Values.Add(goipp.TagKeyword, goipp.String(v))
			}
			rsp.Printer.Add(a)
		}
		rsp.Printer.Add(goipp.MakeAttribute("printer-state", goipp.TagEnum, goipp.Integer(3)))
		kw("media-supported", "iso_a4_210x297mm", "na_letter_8.5x11in")
		kw("media-default", "na_letter_8.5x11in")
		kw("sides-supported", "one-sided")
		kw("sides-default", "one-sided")
		kw("print-color-mode-supported", "monochrome")
		kw("print-color-mode-default", "monochrome")
		q := goipp.Attribute{Name: "print-quality-supported"}
		for _, v := range []goipp.Integer{3, 4, 5} {
			q.Values.Add(goipp.TagEnum, v)
		}
		rsp.Printer.Add(q)
		rsp.Printer.Add(goipp.MakeAttribute("print-quality-default", goipp.TagEnum, goipp.Integer(4)))
		res := goipp.Attribute{Name: "printer-resolution-supported"}
		for _, v := range []int{300, 600} {
			res.Values.Add(goipp.TagResolution, goipp.Resolution{Xres: v, Yres: v, Units: goipp.UnitsDpi})
		}
		rsp.Printer.Add(res)
		rsp.Printer.Add(goipp.MakeAttribute("printer-resolution-default", goipp.TagResolution, goipp.Resolution{Xres: 600, Yres: 600, Units: goipp.UnitsDpi}))
		rsp.Printer.Add(goipp.MakeAttribute("copies-supported", goipp.TagRange, goipp.Range{Lower: 1, Upper: 999}))
		f := goipp.Attribute{Name: "document-format-supported"}
		f.Values.Add(goipp.TagMimeType, goipp.String("application/pdf"))
		rsp.Printer.Add(f)
		return rsp
	})
	a, err := c.Attributes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	none := []string{}
	want := device.PrinterCaps{
		Media: []string{"iso_a4_210x297mm", "na_letter_8.5x11in"}, MediaSources: none, MediaTypes: none,
		Sides: []string{"one-sided"}, ColorModes: []string{"monochrome"}, Qualities: []string{"draft", "normal", "high"},
		ContentOptimize: none, Orientations: none, PrintScalings: none, OutputBins: none, Finishings: none,
		Formats: []string{"application/pdf"}, Resolutions: []int{300, 600}, NumberUp: []int{},
		CopiesMax:    999,
		MediaDefault: "na_letter_8.5x11in", SidesDefault: "one-sided", ColorModeDefault: "monochrome",
		QualityDefault: "normal", ResolutionDefault: 600,
	}
	if !reflect.DeepEqual(a.Caps, want) {
		t.Errorf("Caps = %+v\nwant %+v", a.Caps, want)
	}
	if a.State != "idle" || !slices.Equal(a.Media, want.Media) || !slices.Equal(a.Formats, want.Formats) {
		t.Errorf("Attributes() = %+v", a)
	}
	asked := all(reqs()[0].Operation, "requested-attributes")
	for _, name := range append([]string{"printer-state", "marker-levels"}, ippcaps.Requested...) {
		if !slices.Contains(asked, name) {
			t.Errorf("requested-attributes lacks %s: %v", name, asked)
		}
	}
}

// TestSubmitMediaCol: a tray or media type sends media-col (with the size
// name) instead of media, and one copy sends no copies attribute.
func TestSubmitMediaCol(t *testing.T) {
	c, reqs := ippServer(t, func(req *goipp.Message) *goipp.Message {
		rsp := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, req.RequestID)
		if goipp.Op(req.Code) == goipp.OpCreateJob {
			rsp.Job.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(7)))
		}
		return rsp
	})
	opts := PrintOptions{Copies: 1, PrintSettings: device.PrintSettings{
		Media: "iso_a4_210x297mm", MediaSource: "tray-1", MediaType: "stationery", OutputBin: "face-down",
	}}
	id, err := c.Submit(context.Background(), "a.pdf", "web", FormatPDF, strings.NewReader("%PDF-1.4"), opts)
	if err != nil || id != 7 {
		t.Fatalf("Submit = %d, %v", id, err)
	}
	cj := reqs()[0]
	if goipp.Op(cj.Code) != goipp.OpCreateJob {
		t.Fatalf("first request is %s, want Create-Job", goipp.Op(cj.Code))
	}
	for _, name := range []string{"media", "copies"} {
		if slices.ContainsFunc(cj.Job, func(a goipp.Attribute) bool { return a.Name == name }) {
			t.Errorf("Create-Job sends %s, want none", name)
		}
	}
	if got := attrString(cj.Job, "output-bin"); got != "face-down" {
		t.Errorf("output-bin = %q, want face-down", got)
	}
	var col goipp.Collection
	for _, a := range cj.Job {
		if a.Name == "media-col" && len(a.Values) == 1 {
			col, _ = a.Values[0].V.(goipp.Collection)
		}
	}
	got := map[string]string{}
	for _, m := range col {
		got[m.Name] = attrString(goipp.Attributes{m}, m.Name)
	}
	want := map[string]string{"media-size-name": "iso_a4_210x297mm", "media-source": "tray-1", "media-type": "stationery"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("media-col = %v, want %v", got, want)
	}
}
