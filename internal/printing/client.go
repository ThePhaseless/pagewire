// Package printing is the IPP client pagewire uses to print and to read
// printer and job state, plus document format detection and negotiation.
package printing

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sync/atomic"
	"time"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/ippcaps"
)

// PrintOptions are the job options of a web print (POST /api/v1/printers/{id}/print).
type PrintOptions = apitypes.PrintOptions

var (
	// ErrNotFound means the printer does not know the job.
	ErrNotFound = errors.New("job not found")
	// ErrFinished means the job can no longer be canceled.
	ErrFinished = errors.New("job already finished")
)

// IPPError is an IPP response with an unsuccessful status.
type IPPError struct {
	Status  goipp.Status
	Message string // status-message, if the printer sent one
}

func (e *IPPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("ipp: %s: %s", e.Status, e.Message)
	}
	return "ipp: " + e.Status.String()
}

// Attributes is the printer state pagewire shows in Status.
type Attributes struct {
	State   string // idle | processing | stopped | unknown
	Reasons []string
	Formats []string // document-format-supported
	Media   []string // media-supported
	Markers []apitypes.Marker
	URIs    []string // printer-uri-supported
	Caps    device.PrinterCaps
}

// Client talks IPP to one printer URI (ipp:// or ipps://) over net/http.
type Client struct {
	uri string
}

// New returns a client for the printer at uri. It makes no request.
func New(uri string) *Client { return &Client{uri: uri} }

var requestID atomic.Uint32

// maxResponseBytes caps an IPP response; a longer one fails to decode.
const maxResponseBytes = 8 << 20

func (c *Client) request(op goipp.Op) *goipp.Message {
	m := goipp.NewRequest(goipp.DefaultVersion, op, requestID.Add(1))
	m.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	m.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	m.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(c.uri)))
	return m
}

// do sends msg followed by the optional document body and decodes the
// response. Any status other than successful-* is returned as *IPPError.
func (c *Client) do(ctx context.Context, msg *goipp.Message, doc io.Reader) (*goipp.Message, error) {
	u, err := url.Parse(c.uri)
	if err != nil {
		return nil, fmt.Errorf("printer uri: %w", err)
	}
	switch u.Scheme {
	case "ipp":
		u.Scheme = "http"
	case "ipps":
		u.Scheme = "https"
	default:
		return nil, fmt.Errorf("printer uri %q: scheme must be ipp or ipps", c.uri)
	}
	if u.Port() == "" {
		u.Host += ":631"
	}

	enc, err := msg.EncodeBytes()
	if err != nil {
		return nil, err
	}
	body := io.Reader(bytes.NewReader(enc))
	if doc != nil {
		body = io.MultiReader(body, doc) // streamed; never buffered in memory
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", goipp.ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ipp %s: HTTP %s", goipp.Op(msg.Code), resp.Status)
	}
	rsp := &goipp.Message{}
	if err := rsp.Decode(io.LimitReader(resp.Body, maxResponseBytes)); err != nil {
		return nil, fmt.Errorf("ipp %s: decode response: %w", goipp.Op(msg.Code), err)
	}
	if st := goipp.Status(rsp.Code); st > 0x00ff { // 0x0000-0x00ff are successful-*
		return nil, &IPPError{Status: st, Message: first(rsp.Operation, "status-message")}
	}
	return rsp, nil
}

// requestedAttrs is what Attributes asks for: state, supplies and URIs, plus
// every option attribute ippcaps parses.
var requestedAttrs = func() []string {
	r := append([]string{
		"printer-state", "printer-state-reasons", "document-format-supported", "media-supported",
		"marker-names", "marker-colors", "marker-levels", "printer-uri-supported",
	}, ippcaps.Requested...)
	slices.Sort(r)
	return slices.Compact(r)
}()

// Attributes reads the printer's state, formats, media, supply levels and the
// options it reports (Caps).
func (c *Client) Attributes(ctx context.Context) (Attributes, error) {
	m := c.request(goipp.OpGetPrinterAttributes)
	m.Operation.Add(keywords("requested-attributes", requestedAttrs...))
	rsp, err := c.do(ctx, m, nil)
	if err != nil {
		return Attributes{}, err
	}
	p := rsp.Printer
	a := Attributes{
		State:   "unknown",
		Reasons: all(p, "printer-state-reasons"),
		Formats: all(p, "document-format-supported"),
		Media:   all(p, "media-supported"),
		URIs:    all(p, "printer-uri-supported"),
		Caps:    ippcaps.Parse(p),
	}
	switch first(p, "printer-state") {
	case "3":
		a.State = "idle"
	case "4":
		a.State = "processing"
	case "5":
		a.State = "stopped"
	}
	names, colors, levels := all(p, "marker-names"), all(p, "marker-colors"), ints(p, "marker-levels")
	for i, n := range names {
		mk := apitypes.Marker{Name: n, Level: -2} // -2: IPP's "unknown" level
		if i < len(colors) {
			mk.Color = colors[i]
		}
		if i < len(levels) {
			mk.Level = levels[i]
		}
		a.Markers = append(a.Markers, mk)
	}
	return a, nil
}

// Submit prints r with Create-Job, then Send-Document with
// last-document=true, streaming r as the HTTP body after the IPP message.
// format must already have been negotiated (see Negotiate). If Send-Document
// fails, the created job is canceled so it does not stay held.
func (c *Client) Submit(ctx context.Context, name, user, format string, r io.Reader, opts PrintOptions) (int, error) {
	m := c.request(goipp.OpCreateJob)
	m.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
	m.Operation.Add(goipp.MakeAttribute("job-name", goipp.TagName, goipp.String(name)))
	m.Job = append(m.Job, ippcaps.Job(opts)...)
	rsp, err := c.do(ctx, m, nil)
	if err != nil {
		return 0, fmt.Errorf("create-job: %w", err)
	}
	id := ints(rsp.Job, "job-id")
	if len(id) == 0 || id[0] <= 0 {
		return 0, errors.New("create-job: response has no job-id")
	}

	sd := c.request(goipp.OpSendDocument)
	sd.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(int32(id[0])))) //nolint:gosec // id came from the printer as an IPP integer
	sd.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String(user)))
	sd.Operation.Add(goipp.MakeAttribute("document-name", goipp.TagName, goipp.String(name)))
	sd.Operation.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(format)))
	sd.Operation.Add(goipp.MakeAttribute("last-document", goipp.TagBoolean, goipp.Boolean(true)))
	if _, err := c.do(ctx, sd, r); err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = c.Cancel(cctx, id[0]) // best effort; the Send-Document error is what matters
		return 0, fmt.Errorf("send-document: %w", err)
	}
	return id[0], nil
}

var jobAttrs = []string{"job-id", "job-name", "job-state", "job-state-reasons", "date-time-at-creation"}

// Jobs returns at most limit jobs, newest (highest id) first, active and
// finished alike. It asks for not-completed and completed jobs separately,
// since which-jobs=all is optional (go-mfp's virtual printer rejects it).
func (c *Client) Jobs(ctx context.Context, limit int) ([]apitypes.Job, error) {
	var jobs []apitypes.Job
	seen := map[int]int{} // job id -> index in jobs
	for _, which := range []string{"not-completed", "completed"} {
		m := c.request(goipp.OpGetJobs)
		m.Operation.Add(goipp.MakeAttribute("which-jobs", goipp.TagKeyword, goipp.String(which)))
		m.Operation.Add(keywords("requested-attributes", jobAttrs...))
		rsp, err := c.do(ctx, m, nil)
		if err != nil {
			return nil, fmt.Errorf("get-jobs %s: %w", which, err)
		}
		for _, g := range rsp.AttrGroups() {
			if g.Tag != goipp.TagJobGroup {
				continue
			}
			j := toJob(g.Attrs)
			// A job that finished between the two requests is in both;
			// the later (completed) entry wins.
			if i, ok := seen[j.ID]; ok {
				jobs[i] = j
			} else {
				seen[j.ID] = len(jobs)
				jobs = append(jobs, j)
			}
		}
	}
	// Sorted here, not with the limit attribute: printers apply limit to an
	// oldest-first list.
	slices.SortFunc(jobs, func(a, b apitypes.Job) int { return cmp.Compare(b.ID, a.ID) })
	if limit >= 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, nil
}

// Job returns one job, or ErrNotFound.
func (c *Client) Job(ctx context.Context, id int) (apitypes.Job, error) {
	n, ok := ippInt(id)
	if !ok {
		return apitypes.Job{}, ErrNotFound
	}
	m := c.request(goipp.OpGetJobAttributes)
	m.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, n))
	m.Operation.Add(keywords("requested-attributes", jobAttrs...))
	rsp, err := c.do(ctx, m, nil)
	if err != nil {
		return apitypes.Job{}, jobError(err, false)
	}
	return toJob(rsp.Job), nil
}

// Cancel cancels a job. It returns ErrNotFound for an unknown job and
// ErrFinished for one that already reached a terminal state.
func (c *Client) Cancel(ctx context.Context, id int) error {
	n, ok := ippInt(id)
	if !ok {
		return ErrNotFound
	}
	m := c.request(goipp.OpCancelJob)
	m.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, n))
	_, err := c.do(ctx, m, nil)
	return jobError(err, true)
}

// jobError maps the IPP statuses printers use for job lookups to the
// sentinel errors. For Cancel-Job, not-possible means the job is finished
// (CUPS, PAPPL and go-mfp all answer so).
func jobError(err error, cancel bool) error {
	var ie *IPPError
	if errors.As(err, &ie) {
		switch {
		case ie.Status == goipp.StatusErrorNotFound:
			return ErrNotFound
		case cancel && ie.Status == goipp.StatusErrorNotPossible:
			return ErrFinished
		}
	}
	return err
}

var jobStates = map[string]string{
	"3": "pending", "4": "held", "5": "processing", "6": "stopped",
	"7": "canceled", "8": "aborted", "9": "completed",
}

// toJob converts a job attribute group. CreatedAt comes from
// date-time-at-creation and stays zero when the printer omits it
// (time-at-creation is printer uptime, not a date).
func toJob(attrs goipp.Attributes) apitypes.Job {
	j := apitypes.Job{Name: first(attrs, "job-name"), State: "pending", Reasons: all(attrs, "job-state-reasons")}
	if id := ints(attrs, "job-id"); len(id) > 0 {
		j.ID = id[0]
	}
	if s, ok := jobStates[first(attrs, "job-state")]; ok {
		j.State = s
	}
	for _, a := range attrs {
		if a.Name == "date-time-at-creation" && len(a.Values) > 0 {
			if t, ok := a.Values[0].V.(goipp.Time); ok {
				j.CreatedAt = t.Time
			}
		}
	}
	return j
}

// ippInt converts a positive int to an IPP integer; ids and counts outside
// 1..MaxInt32 (for example a job id from a URL) would otherwise wrap around.
func ippInt(n int) (goipp.Integer, bool) {
	if n <= 0 || n > math.MaxInt32 {
		return 0, false
	}
	return goipp.Integer(n), true
}

func keywords(name string, values ...string) goipp.Attribute {
	a := goipp.Attribute{Name: name}
	for _, v := range values {
		a.Values.Add(goipp.TagKeyword, goipp.String(v))
	}
	return a
}

func all(attrs goipp.Attributes, name string) []string {
	var out []string
	for _, a := range attrs {
		if a.Name == name {
			for _, v := range a.Values {
				out = append(out, v.V.String())
			}
		}
	}
	return out
}

func first(attrs goipp.Attributes, name string) string {
	if v := all(attrs, name); len(v) > 0 {
		return v[0]
	}
	return ""
}

func ints(attrs goipp.Attributes, name string) []int {
	var out []int
	for _, a := range attrs {
		if a.Name == name {
			for _, v := range a.Values {
				if i, ok := v.V.(goipp.Integer); ok {
					out = append(out, int(i))
				}
			}
		}
	}
	return out
}
