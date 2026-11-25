// Package papplfake is an in-process fake of a PAPPL printer application's
// IPP system service, used by unit tests and the sim backend. It mirrors the
// PAPPL 1.4.12 behaviour pagewire depends on: system operations at
// /ipp/system (Get-Printers, Find-Devices, Find-Drivers, Create-Printer,
// Delete-Printer), queues at /ipp/print/<name>, device schemes limited to
// what the retrofit apps register ("cups" by default), any named driver
// accepted by Create-Printer, and driver "auto" stored as "auto". Jobs are completed as soon as their document arrives.
package papplfake

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/pappl"
)

// Device is one entry Find-Devices lists.
type Device struct{ URI, Info, DeviceID string }

// Driver is one driver the application offers. Find-Drivers with a device ID
// and Create-Printer with driver "auto" match it when the device ID's MFG
// equals MatchMFG (case-insensitive); an empty MatchMFG never matches.
type Driver struct{ Name, Description, MatchMFG string }

// Options configure a fake system.
type Options struct {
	Schemes   []string // top-level device URI schemes Create-Printer accepts; default ["cups"]
	Devices   []Device
	Drivers   []Driver
	OutputDir string // documents land in OutputDir/<queue>-<job>.bin; "" discards them
	// NoAutoDriver makes Create-Printer refuse driver "auto", like an
	// application without an auto-add callback.
	NoAutoDriver bool
}

// Printer is a snapshot of one queue.
type Printer struct {
	ID                                int
	Name, Resource                    string // Resource is the sanitised name in /ipp/print/<Resource>
	DeviceURI, DeviceID, Driver       string
	Location, Media, Sides, ColorMode string
	Quality                           string // draft | normal | high
	Jobs                              int
}

type job struct {
	id      int
	name    string
	state   goipp.Integer // 3 pending, 9 completed
	created time.Time
}

type queue struct {
	Printer
	jobs []*job
}

// System is the fake. It is an http.Handler; serve it with httptest or any
// listener and use ipp://<host:port>/ipp/system as the system URI.
type System struct {
	opts Options

	mu      sync.Mutex
	queues  []*queue
	nextID  int
	nextJob int
}

// New returns a fake system with no queues.
func New(o Options) *System {
	if len(o.Schemes) == 0 {
		o.Schemes = []string{"cups"}
	}
	return &System{opts: o, nextID: 1, nextJob: 1}
}

// Printers returns a snapshot of the queues in creation order.
func (s *System) Printers() []Printer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Printer, 0, len(s.queues))
	for _, q := range s.queues {
		p := q.Printer
		p.Jobs = len(q.jobs)
		out = append(out, p)
	}
	return out
}

// ServeHTTP handles one IPP request.
func (s *System) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "IPP needs POST", http.StatusMethodNotAllowed)
		return
	}
	req := &goipp.Message{}
	if err := req.Decode(r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rsp := s.handle(req, r.Body, r.Host)
	if rsp.Groups != nil { // goipp encodes only Groups once it is set
		rsp.Groups = append(goipp.Groups{{Tag: goipp.TagOperationGroup, Attrs: rsp.Operation}}, rsp.Groups...)
	}
	w.Header().Set("Content-Type", goipp.ContentType)
	_ = rsp.Encode(w)
}

func (s *System) handle(req *goipp.Message, body io.Reader, host string) *goipp.Message {
	rsp := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, req.RequestID)
	rsp.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	rsp.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	fail := func(st goipp.Status, format string, a ...any) *goipp.Message {
		rsp.Code = goipp.Code(st)
		rsp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String(fmt.Sprintf(format, a...))))
		return rsp
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	op := goipp.Op(req.Code)
	if uri := pappl.First(req.Operation, "system-uri"); uri != "" {
		if !strings.HasSuffix(uri, "/ipp/system") {
			return fail(goipp.StatusErrorAttributesOrValues, "Bad system-uri value '%s'.", uri)
		}
		return s.systemOp(op, req, rsp, host, fail)
	}
	uri := pappl.First(req.Operation, "printer-uri")
	var q *queue
	if u, err := url.Parse(uri); err == nil { // PAPPL routes on the decoded path
		if res, ok := strings.CutPrefix(u.Path, "/ipp/print/"); ok {
			q = s.find(res)
		}
	}
	if q == nil {
		return fail(goipp.StatusErrorNotFound, "printer-uri %s not found.", uri)
	}
	return s.printerOp(op, q, req, rsp, body, host, fail)
}

type failFunc func(goipp.Status, string, ...any) *goipp.Message

func (s *System) systemOp(op goipp.Op, req, rsp *goipp.Message, host string, fail failFunc) *goipp.Message {
	switch op {
	case goipp.OpGetPrinters:
		for _, q := range s.queues {
			rsp.Groups.Add(goipp.Group{Tag: goipp.TagPrinterGroup, Attrs: s.printerAttrs(q, host)})
		}
	case pappl.OpFindDevices:
		if len(s.opts.Devices) == 0 {
			return fail(goipp.StatusErrorNotFound, "No devices found.")
		}
		a := goipp.Attribute{Name: pappl.AttrDeviceCol}
		for _, d := range s.opts.Devices {
			a.Values.Add(goipp.TagBeginCollection, goipp.Collection{
				goipp.MakeAttribute(pappl.AttrDeviceID, goipp.TagText, goipp.String(d.DeviceID)),
				goipp.MakeAttribute(pappl.AttrDeviceInfo, goipp.TagText, goipp.String(d.Info)),
				goipp.MakeAttribute(pappl.AttrDeviceURI, goipp.TagURI, goipp.String(d.URI)),
			})
		}
		rsp.Groups.Add(goipp.Group{Tag: goipp.TagSystemGroup, Attrs: goipp.Attributes{a}})
	case pappl.OpFindDrivers:
		id := pappl.First(req.Operation, pappl.AttrDeviceID)
		a := goipp.Attribute{Name: pappl.AttrDriverCol}
		for _, d := range s.opts.Drivers {
			if id != "" && !matches(d, id) {
				continue
			}
			a.Values.Add(goipp.TagBeginCollection, goipp.Collection{
				goipp.MakeAttribute(pappl.AttrDriver, goipp.TagKeyword, goipp.String(d.Name)),
				goipp.MakeAttribute(pappl.AttrDriverInfo, goipp.TagText, goipp.String(d.Description)),
			})
		}
		if len(a.Values) == 0 {
			return fail(goipp.StatusErrorNotFound, "No matching drivers found.")
		}
		rsp.Groups.Add(goipp.Group{Tag: goipp.TagSystemGroup, Attrs: goipp.Attributes{a}})
	case goipp.OpCreatePrinter:
		return s.createPrinter(req, rsp, fail)
	case goipp.OpDeletePrinter:
		ids := pappl.Ints(req.Operation, "printer-id")
		i := slices.IndexFunc(s.queues, func(q *queue) bool { return len(ids) > 0 && q.ID == ids[0] })
		if i < 0 {
			return fail(goipp.StatusErrorNotFound, "Printer not found.")
		}
		s.queues = slices.Delete(s.queues, i, i+1)
	default:
		return fail(goipp.StatusErrorOperationNotSupported, "This operation is not supported.")
	}
	return rsp
}

func (s *System) createPrinter(req, rsp *goipp.Message, fail failFunc) *goipp.Message {
	name := pappl.First(req.Printer, "printer-name")
	uri := pappl.First(req.Operation, pappl.AttrDeviceURI)
	driver := pappl.First(req.Operation, pappl.AttrDriver)
	switch {
	case pappl.First(req.Operation, "printer-service-type") != "print":
		return fail(goipp.StatusErrorBadRequest, "Missing 'printer-service-type' attribute in request.")
	case name == "":
		return fail(goipp.StatusErrorBadRequest, "Missing 'printer-name' attribute in request.")
	case uri == "":
		return fail(goipp.StatusErrorBadRequest, "Missing '%s' attribute in request.", pappl.AttrDeviceURI)
	case driver == "":
		return fail(goipp.StatusErrorBadRequest, "Missing '%s' attribute in request.", pappl.AttrDriver)
	}
	if sch, _, _ := strings.Cut(uri, ":"); !slices.Contains(s.opts.Schemes, sch) {
		return fail(goipp.StatusErrorAttributesOrValues, "Unsupported '%s' value '%s'.", pappl.AttrDeviceURI, uri)
	}
	for _, a := range req.Printer {
		if a.Name != "printer-name" && a.Name != "printer-device-id" && !settable[a.Name] {
			return fail(goipp.StatusErrorAttributesOrValues, "Unsupported attribute '%s'.", a.Name)
		}
	}
	res := Resource(name)
	if s.find(res) != nil {
		return fail(goipp.StatusErrorNotPossible, "Printer name '%s' already exists.", name)
	}
	deviceID := pappl.First(req.Printer, "printer-device-id")
	if deviceID == "" {
		for _, d := range s.opts.Devices {
			if d.URI == uri {
				deviceID = d.DeviceID
			}
		}
	}
	// The retrofit apps accept any named driver, listed or not (pappl-retrofit 1.0b3).
	ok := driver != "auto"
	for _, d := range s.opts.Drivers {
		if driver == "auto" && !s.opts.NoAutoDriver && deviceID != "" && matches(d, deviceID) {
			ok = true
		}
	}
	if !ok {
		return fail(goipp.StatusErrorAttributesOrValues, "Driver '%s' cannot be used with this printer.", driver)
	}
	q := &queue{Printer: Printer{
		ID: s.nextID, Name: name, Resource: res, DeviceURI: uri, DeviceID: deviceID, Driver: driver,
		Media: "iso_a4_210x297mm", Sides: "one-sided", ColorMode: "auto", Quality: "normal",
	}}
	s.nextID++
	if st, msg := q.set(req.Printer); st != goipp.StatusOk {
		return fail(st, "%s", msg)
	}
	s.queues = append(s.queues, q)
	rsp.Printer.Add(goipp.MakeAttribute("printer-id", goipp.TagInteger, goipp.Integer(int32(q.ID)))) //nolint:gosec // small counter
	return rsp
}

var settable = map[string]bool{
	"printer-location": true, "media-default": true, "sides-default": true,
	"print-color-mode-default": true, "print-quality-default": true,
}

// set applies settable printer attributes. Unknown ones are refused, as PAPPL does.
func (q *queue) set(attrs goipp.Attributes) (goipp.Status, string) {
	for _, a := range attrs {
		if a.Name == "printer-name" || a.Name == "printer-device-id" {
			continue
		}
		if !settable[a.Name] || len(a.Values) != 1 {
			return goipp.StatusErrorAttributesOrValues, "Unsupported attribute '" + a.Name + "'."
		}
		v := a.Values[0].V.String()
		switch a.Name {
		case "printer-location":
			q.Location = v
		case "media-default":
			q.Media = v
		case "sides-default":
			q.Sides = v
		case "print-color-mode-default":
			q.ColorMode = v
		case "print-quality-default":
			q.Quality = ""
			for k, e := range pappl.Qualities {
				if e.String() == v {
					q.Quality = k
				}
			}
			if q.Quality == "" {
				return goipp.StatusErrorAttributesOrValues, "Unsupported \"print-quality-default\" value '" + v + "'."
			}
		}
	}
	return goipp.StatusOk, ""
}

func (s *System) printerOp(op goipp.Op, q *queue, req, rsp *goipp.Message, body io.Reader, host string, fail failFunc) *goipp.Message {
	switch op {
	case goipp.OpGetPrinterAttributes:
		rsp.Printer = append(rsp.Printer, s.printerAttrs(q, host)...)
	case goipp.OpSetPrinterAttributes:
		if st, msg := q.set(req.Printer); st != goipp.StatusOk {
			return fail(st, "%s", msg)
		}
	case goipp.OpCreateJob:
		j := &job{id: s.nextJob, name: pappl.First(req.Operation, "job-name"), state: 3, created: time.Now()}
		s.nextJob++
		q.jobs = append(q.jobs, j)
		rsp.Job = jobAttrs(j)
	case goipp.OpSendDocument:
		j := q.job(req)
		if j == nil {
			return fail(goipp.StatusErrorNotFound, "Job not found.")
		}
		if j.state != 3 {
			return fail(goipp.StatusErrorNotPossible, "Job is not pending.")
		}
		j.state = 5 // processing
		path := ""
		if s.opts.OutputDir != "" {
			path = filepath.Join(s.opts.OutputDir, q.Resource+"-"+strconv.Itoa(j.id)+".bin")
		}
		// The upload streams in from the client: never hold the lock across it.
		s.mu.Unlock()
		err := save(path, body)
		s.mu.Lock()
		if err != nil {
			j.state = 8 // aborted
			return fail(goipp.StatusErrorInternal, "%v", err)
		}
		j.state = 9 // completed
		rsp.Job = jobAttrs(j)
	case goipp.OpGetJobAttributes:
		j := q.job(req)
		if j == nil {
			return fail(goipp.StatusErrorNotFound, "Job not found.")
		}
		rsp.Job = jobAttrs(j)
	case goipp.OpGetJobs:
		completed := pappl.First(req.Operation, "which-jobs") == "completed"
		for _, j := range q.jobs {
			if (j.state >= 7) == completed { // canceled, aborted and completed are terminal
				rsp.Groups.Add(goipp.Group{Tag: goipp.TagJobGroup, Attrs: jobAttrs(j)})
			}
		}
	case goipp.OpCancelJob:
		j := q.job(req)
		switch {
		case j == nil:
			return fail(goipp.StatusErrorNotFound, "Job not found.")
		case j.state >= 7:
			return fail(goipp.StatusErrorNotPossible, "Job is already finished.")
		}
		j.state = 7 // canceled
	default:
		return fail(goipp.StatusErrorOperationNotSupported, "This operation is not supported.")
	}
	return rsp
}

// save writes the document to path, or discards it when path is "".
func save(path string, body io.Reader) error {
	if path == "" {
		_, err := io.Copy(io.Discard, body)
		return err
	}
	f, err := os.Create(path) //nolint:gosec // path is OutputDir plus the sanitised queue name
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (q *queue) job(req *goipp.Message) *job {
	ids := pappl.Ints(req.Operation, "job-id")
	for _, j := range q.jobs {
		if len(ids) > 0 && j.id == ids[0] {
			return j
		}
	}
	return nil
}

func jobAttrs(j *job) goipp.Attributes {
	reason := "none"
	if j.state == 9 {
		reason = "job-completed-successfully"
	}
	return goipp.Attributes{
		goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(int32(j.id))), //nolint:gosec // small counter
		goipp.MakeAttribute("job-name", goipp.TagName, goipp.String(j.name)),
		goipp.MakeAttribute("job-state", goipp.TagEnum, j.state),
		goipp.MakeAttribute("job-state-reasons", goipp.TagKeyword, goipp.String(reason)),
		goipp.MakeAttribute("date-time-at-creation", goipp.TagDateTime, goipp.Time{Time: j.created}),
	}
}

func (s *System) printerAttrs(q *queue, host string) goipp.Attributes {
	quality := pappl.Qualities[q.Quality]
	return goipp.Attributes{
		goipp.MakeAttribute("printer-id", goipp.TagInteger, goipp.Integer(int32(q.ID))), //nolint:gosec // small counter
		goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(q.Name)),
		goipp.MakeAttribute("printer-info", goipp.TagText, goipp.String(q.Name)),
		goipp.MakeAttribute("printer-uri-supported", goipp.TagURI, goipp.String((&url.URL{Scheme: "ipp", Host: host, Path: "/ipp/print/" + q.Resource}).String())),
		goipp.MakeAttribute(pappl.AttrDeviceURI, goipp.TagURI, goipp.String(q.DeviceURI)),
		goipp.MakeAttribute(pappl.AttrDriver, goipp.TagKeyword, goipp.String(q.Driver)),
		goipp.MakeAttribute("printer-device-id", goipp.TagText, goipp.String(q.DeviceID)),
		goipp.MakeAttribute("printer-location", goipp.TagText, goipp.String(q.Location)),
		goipp.MakeAttribute("printer-state", goipp.TagEnum, goipp.Integer(3)), // idle
		goipp.MakeAttribute("printer-state-reasons", goipp.TagKeyword, goipp.String("none")),
		goipp.MakeAttribute("printer-is-accepting-jobs", goipp.TagBoolean, goipp.Boolean(true)),
		goipp.MakeAttr("document-format-supported", goipp.TagMimeType,
			goipp.String("application/pdf"), goipp.String("image/jpeg"), goipp.String("image/png"),
			goipp.String("image/pwg-raster"), goipp.String("image/urf")),
		goipp.MakeAttr("media-supported", goipp.TagKeyword,
			goipp.String("iso_a4_210x297mm"), goipp.String("na_letter_8.5x11in")),
		goipp.MakeAttribute("media-default", goipp.TagKeyword, goipp.String(q.Media)),
		goipp.MakeAttribute("sides-default", goipp.TagKeyword, goipp.String(q.Sides)),
		goipp.MakeAttribute("print-color-mode-default", goipp.TagKeyword, goipp.String(q.ColorMode)),
		goipp.MakeAttribute("print-quality-default", goipp.TagEnum, quality),
	}
}

func (s *System) find(res string) *queue {
	for _, q := range s.queues {
		if res != "" && q.Resource == res {
			return q
		}
	}
	return nil
}

func matches(d Driver, deviceID string) bool {
	return d.MatchMFG != "" && strings.EqualFold(device.ParseDeviceID(deviceID)["MFG"], d.MatchMFG)
}

// Resource is PAPPL's resource name for a printer name (printer.c,
// papplPrinterCreate): a leading digit gets a "_" prefix, spaces, control and
// URL-reserved characters become "_", and repeated or trailing "_" are dropped.
func Resource(name string) string {
	var b strings.Builder
	if name != "" && name[0] >= '0' && name[0] <= '9' {
		b.WriteByte('_')
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c <= ' ' || c == 0x7f || strings.IndexByte("/\\'\"?#", c) >= 0 {
			c = '_'
		}
		if c == '_' && strings.HasSuffix(b.String(), "_") {
			continue
		}
		b.WriteByte(c)
	}
	return strings.TrimSuffix(b.String(), "_")
}
