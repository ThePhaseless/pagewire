// Package papplfake is an in-process fake of a PAPPL printer application's
// IPP system service, used by unit tests and the sim backend. It mirrors the
// PAPPL 1.4.12 behaviour pagewire depends on: system operations at
// /ipp/system (Get-Printers, Find-Devices, Find-Drivers, Create-Printer,
// Delete-Printer), queues at /ipp/print/<name>, device schemes limited to
// what the retrofit apps register ("cups" by default), any named driver
// accepted by Create-Printer, and driver "auto" stored as "auto". Jobs are
// completed as soon as their document arrives.
//
// It also serves ippget subscriptions as PAPPL does: Create-System-Subscriptions
// at /ipp/system, Create-Printer- and Create-Job-Subscriptions on a queue,
// Get-Notifications (notify-wait held until any event, Options.NotifyWait or
// the request's end), Renew- and Cancel-Subscription on either path. Events:
// printer-created, printer-deleted, printer-config-changed on every successful
// Set-Printer-Attributes (changed or not), job-state-changed and job-completed.
// Leases never expire, and a deleted queue's subscriptions stay, as in PAPPL
// until their lease ends.
package papplfake

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/ippcaps"
	"github.com/ThePhaseless/pagewire/internal/ippnotify"
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
	// Caps are the supported lists and defaults every queue reports, encoded
	// the way PAPPL encodes them. Zero means today's: A4 and Letter, every
	// sides, colour mode and quality value, defaults A4, one-sided, auto, normal.
	// A queue starts on the *Default values; Set-Printer-Attributes changes them
	// and refuses a value its supported list lacks.
	Caps device.PrinterCaps
	// Refuse lists Set-Printer-Attributes attributes refused whatever their
	// value, as ps-printer-app refuses output-bin-default while reporting
	// output-bin-supported face-down (its driver lists no bins).
	Refuse []string
	// DropLocation accepts printer-location without storing it, as PAPPL
	// 1.4.12 does (it passes the value to papplPrinterSetGeoLocation).
	DropLocation bool
	// NotifyWait is how long a Get-Notifications with notify-wait is held
	// without events; default 30 s, as PAPPL.
	NotifyWait time.Duration
	// NoSubscriptions answers every subscription operation
	// operation-not-supported, like a server without IPP notifications.
	NoSubscriptions bool
	// MaxSubscriptions caps live subscriptions (0: 100, PAPPL's default); a
	// Create above it answers client-error-ignored-all-subscriptions.
	MaxSubscriptions int
	// ReadBack maps a keyword default (media-default, sides-default,
	// print-color-mode-default, print-quality-default,
	// print-content-optimize-default, orientation-requested-default,
	// output-bin-default) or printer-location to the value the queue stores
	// whatever a Set sent; the Set is accepted, as for a value PAPPL normalizes.
	ReadBack map[string]string
}

// defaultCaps are the caps of a fake without Options.Caps.
func defaultCaps() device.PrinterCaps {
	return device.PrinterCaps{
		Media:        []string{"iso_a4_210x297mm", "na_letter_8.5x11in"},
		MediaDefault: "iso_a4_210x297mm",
		Sides:        []string{"one-sided", "two-sided-long-edge", "two-sided-short-edge"}, SidesDefault: "one-sided",
		ColorModes: []string{"auto", "color", "monochrome"}, ColorModeDefault: "auto",
		Qualities: []string{"draft", "normal", "high"}, QualityDefault: "normal",
		Formats: []string{"application/pdf", "image/jpeg", "image/png", "image/pwg-raster", "image/urf"},
	}
}

// Printer is a snapshot of one queue.
type Printer struct {
	ID                          int
	Name, Resource              string // Resource is the sanitised name in /ipp/print/<Resource>
	DeviceURI, DeviceID, Driver string
	Location                    string
	Defaults                    device.PrintSettings // the queue's current *-default values
	Jobs                        int
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

// subscription is one ippget subscription; printer and job are 0 when it is
// not bound to one.
type subscription struct {
	printer, job int
	events       map[string]bool
	log          []goipp.Attributes // event groups; notify-sequence-number is index+1
}

// System is the fake. It is an http.Handler; serve it with httptest or any
// listener and use ipp://<host:port>/ipp/system as the system URI.
type System struct {
	opts Options

	mu      sync.Mutex
	queues  []*queue
	nextID  int
	nextJob int
	finds   int
	sets    [][]string // the attribute names of each Set-Printer-Attributes
	subs    map[int]*subscription
	nextSub int
	wake    chan struct{} // closed and replaced on every event, like PAPPL's broadcast
	gets    int           // Get-Notifications served
}

// New returns a fake system with no queues.
func New(o Options) *System {
	if len(o.Schemes) == 0 {
		o.Schemes = []string{"cups"}
	}
	if reflect.ValueOf(o.Caps).IsZero() {
		o.Caps = defaultCaps()
	}
	if o.NotifyWait == 0 {
		o.NotifyWait = 30 * time.Second
	}
	if o.MaxSubscriptions == 0 {
		o.MaxSubscriptions = 100
	}
	return &System{opts: o, nextID: 1, nextJob: 1, nextSub: 1, subs: map[int]*subscription{}, wake: make(chan struct{})}
}

// GetNotificationsCalls returns how many Get-Notifications requests the system
// has served.
func (s *System) GetNotificationsCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

// Subscriptions returns how many subscriptions are live (created, not cancelled).
func (s *System) Subscriptions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// DeleteExternally deletes the queue named name and emits printer-deleted, as
// a user deleting it in PAPPL's web UI would. Unknown names do nothing.
func (s *System) DeleteExternally(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.queues, func(q *queue) bool { return q.Name == name }); i >= 0 {
		s.deleteQueue(i)
	}
}

func (s *System) deleteQueue(i int) {
	q := s.queues[i]
	s.queues = slices.Delete(s.queues, i, i+1)
	s.emit(ippnotify.EventPrinterDeleted, q, nil)
}

// FindDevicesCalls returns how many Find-Devices requests the system has served.
func (s *System) FindDevicesCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finds
}

// SetCalls returns how many Set-Printer-Attributes requests the system has
// served, refused ones included.
func (s *System) SetCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sets)
}

// Sets returns the printer attribute names of every Set-Printer-Attributes
// request the system has served, in order, refused ones included.
func (s *System) Sets() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sets)
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
	rsp := s.handle(r.Context(), req, r.Body, r.Host)
	if rsp.Groups != nil { // goipp encodes only Groups once it is set
		rsp.Groups = append(goipp.Groups{{Tag: goipp.TagOperationGroup, Attrs: rsp.Operation}}, rsp.Groups...)
	}
	w.Header().Set("Content-Type", goipp.ContentType)
	_ = rsp.Encode(w)
}

func (s *System) handle(ctx context.Context, req *goipp.Message, body io.Reader, host string) *goipp.Message {
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
		if subscriptionOps[op] {
			return s.subscriptionOp(ctx, op, nil, req, rsp, fail)
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
	if subscriptionOps[op] {
		return s.subscriptionOp(ctx, op, q, req, rsp, fail)
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
		s.finds++
		// PAPPL lists its own schemes (usb, dnssd, snmp) beside the custom "cups"
		// one unless the request asks for custom local schemes only.
		devs := s.opts.Devices
		if pappl.First(req.Operation, pappl.AttrDeviceType) == pappl.DeviceTypeCustomLocal {
			devs = slices.DeleteFunc(slices.Clone(devs), func(d Device) bool { return !strings.HasPrefix(d.URI, "cups:") })
		}
		if len(devs) == 0 {
			return fail(goipp.StatusErrorNotFound, "No devices found.")
		}
		a := goipp.Attribute{Name: pappl.AttrDeviceCol}
		for _, d := range devs {
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
		s.deleteQueue(i)
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
	c := s.opts.Caps
	q := &queue{Printer: Printer{
		ID: s.nextID, Name: name, Resource: res, DeviceURI: uri, DeviceID: deviceID, Driver: driver,
		Defaults: device.PrintSettings{
			Media: c.MediaDefault, MediaSource: c.MediaSourceDefault, MediaType: c.MediaTypeDefault,
			Sides: c.SidesDefault, ColorMode: c.ColorModeDefault, Quality: c.QualityDefault,
			Resolution: c.ResolutionDefault, ContentOptimize: c.ContentOptimizeDefault,
			Orientation: c.OrientationDefault, NumberUp: c.NumberUpDefault, PrintScaling: c.PrintScalingDefault,
			OutputBin: c.OutputBinDefault, Finishings: c.FinishingsDefault,
		},
	}}
	s.nextID++
	if st, msg, bad := s.set(q, req.Printer); st != goipp.StatusOk {
		r := fail(st, "%s", msg)
		r.Unsupported = bad
		return r
	}
	s.queues = append(s.queues, q)
	s.emit(ippnotify.EventPrinterCreated, q, nil)
	rsp.Printer.Add(goipp.MakeAttribute("printer-id", goipp.TagInteger, goipp.Integer(int32(q.ID)))) //nolint:gosec // small counter
	return rsp
}

// settable is what PAPPL's Set-Printer-Attributes accepts among the
// attributes pagewire sends (ippcaps.PAPPLSettable, confirmed against
// PAPPL_COMMIT there).
var settable = func() map[string]bool {
	m := map[string]bool{"printer-location": true}
	for _, n := range ippcaps.PAPPLSettable {
		m[n] = true
	}
	return m
}()

// set applies settable printer attributes, all or none. Like PAPPL it checks
// every attribute and answers the last refusal's message, with the refused
// attributes in the unsupported group, except output-bin-default, which PAPPL
// refuses without naming it. Unknown attributes are refused, and so is a value
// the supported list lacks, as the Samsung's queue refused
// print-color-mode-default auto. media-col-default is merged member by member
// onto the current default.
func (s *System) set(q *queue, attrs goipp.Attributes) (st goipp.Status, msg string, unsupported goipp.Attributes) {
	c := s.opts.Caps
	d, loc := q.Defaults, q.Location
	st = goipp.StatusOk
	for _, a := range attrs {
		if a.Name == "printer-name" || a.Name == "printer-device-id" {
			continue
		}
		if _, ok := s.opts.ReadBack[a.Name]; ok && readBackField(&d, &loc, a.Name) != nil {
			continue // accepted whatever its value; stored below
		}
		if !settable[a.Name] || len(a.Values) != 1 {
			st, msg = goipp.StatusErrorAttributesOrValues, "Unsupported attribute '"+a.Name+"'."
			unsupported.Add(a)
			continue
		}
		v := a.Values[0].V
		kw := func(dst *string, supported []string) bool {
			if !slices.Contains(supported, v.String()) {
				return false
			}
			*dst = v.String()
			return true
		}
		enum := func(dst *string, supported []string, table map[string]goipp.Integer) bool {
			n, ok := v.(goipp.Integer)
			k := ippcaps.Keyword(table, n)
			if !ok || !slices.Contains(supported, k) {
				return false
			}
			*dst = k
			return true
		}
		ok := !slices.Contains(s.opts.Refuse, a.Name)
		switch {
		case !ok:
		case a.Name == "printer-location":
			if !s.opts.DropLocation {
				loc = v.String()
			}
		case a.Name == "media-default":
			ok = kw(&d.Media, c.Media)
		case a.Name == "media-col-default":
			col, isCol := v.(goipp.Collection)
			ok = isCol
			for _, m := range col {
				if len(m.Values) != 1 {
					ok = false
					continue
				}
				mv := m.Values[0].V.String()
				switch m.Name {
				case "media-size-name":
					ok = ok && slices.Contains(c.Media, mv)
					d.Media = mv
				case "media-source":
					ok = ok && slices.Contains(c.MediaSources, mv)
					d.MediaSource = mv
				case "media-type":
					ok = ok && slices.Contains(c.MediaTypes, mv)
					d.MediaType = mv
				}
			}
		case a.Name == "sides-default":
			ok = kw(&d.Sides, c.Sides)
		case a.Name == "print-color-mode-default":
			ok = kw(&d.ColorMode, c.ColorModes)
		case a.Name == "print-content-optimize-default":
			ok = kw(&d.ContentOptimize, c.ContentOptimize)
		case a.Name == "output-bin-default":
			ok = kw(&d.OutputBin, c.OutputBins)
		case a.Name == "print-quality-default":
			ok = enum(&d.Quality, c.Qualities, ippcaps.Qualities)
		case a.Name == "orientation-requested-default":
			ok = enum(&d.Orientation, c.Orientations, ippcaps.Orientations)
		case a.Name == "printer-resolution-default":
			r, isRes := v.(goipp.Resolution)
			ok = isRes && r.Units == goipp.UnitsDpi && r.Xres == r.Yres && slices.Contains(c.Resolutions, r.Xres)
			d.Resolution = r.Xres
		}
		if !ok {
			st, msg = goipp.StatusErrorAttributesOrValues, fmt.Sprintf("Unsupported %q value '%s'.", a.Name, v)
			if a.Name != "output-bin-default" {
				unsupported.Add(a)
			}
		}
	}
	if st == goipp.StatusOk {
		for _, a := range attrs {
			if v, ok := s.opts.ReadBack[a.Name]; ok {
				if p := readBackField(&d, &loc, a.Name); p != nil {
					*p = v
				}
			}
		}
		q.Defaults, q.Location = d, loc
	}
	return st, msg, unsupported
}

// readBackField is the stored field Options.ReadBack can substitute for the
// attribute name, or nil.
func readBackField(d *device.PrintSettings, loc *string, name string) *string {
	switch name {
	case "printer-location":
		return loc
	case "media-default":
		return &d.Media
	case "sides-default":
		return &d.Sides
	case "print-color-mode-default":
		return &d.ColorMode
	case "print-quality-default":
		return &d.Quality
	case "print-content-optimize-default":
		return &d.ContentOptimize
	case "orientation-requested-default":
		return &d.Orientation
	case "output-bin-default":
		return &d.OutputBin
	}
	return nil
}

func (s *System) printerOp(op goipp.Op, q *queue, req, rsp *goipp.Message, body io.Reader, host string, fail failFunc) *goipp.Message {
	switch op {
	case goipp.OpGetPrinterAttributes:
		rsp.Printer = append(rsp.Printer, s.printerAttrs(q, host)...)
	case goipp.OpSetPrinterAttributes:
		var names []string
		for _, a := range req.Printer {
			names = append(names, a.Name)
		}
		s.sets = append(s.sets, names)
		if st, msg, bad := s.set(q, req.Printer); st != goipp.StatusOk {
			r := fail(st, "%s", msg)
			r.Unsupported = bad
			return r
		}
		s.emit(ippnotify.EventPrinterConfigChanged, q, nil) // even when nothing changed, as PAPPL
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
		s.setJobState(q, j, 5) // processing
		path := ""
		if s.opts.OutputDir != "" {
			path = filepath.Join(s.opts.OutputDir, q.Resource+"-"+strconv.Itoa(j.id)+".bin")
		}
		// The upload streams in from the client: never hold the lock across it.
		s.mu.Unlock()
		err := save(path, body)
		s.mu.Lock()
		if err != nil {
			s.setJobState(q, j, 8) // aborted
			return fail(goipp.StatusErrorInternal, "%v", err)
		}
		s.setJobState(q, j, 9) // completed
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
		s.setJobState(q, j, 7) // canceled
	default:
		return fail(goipp.StatusErrorOperationNotSupported, "This operation is not supported.")
	}
	return rsp
}

// setJobState moves j to state and emits job-state-changed, plus
// job-completed for a terminal state.
func (s *System) setJobState(q *queue, j *job, state goipp.Integer) {
	j.state = state
	s.emit(ippnotify.EventJobStateChanged, q, j)
	if state >= 7 {
		s.emit(ippnotify.EventJobCompleted, q, j)
	}
	if state == 5 || state >= 7 { // the printer starts or stops processing it, as PAPPL reports
		s.emit(ippnotify.EventPrinterStateChanged, q, nil)
	}
}

// subscriptionOps are the operations subscriptionOp serves.
var subscriptionOps = map[goipp.Op]bool{
	goipp.OpCreateSystemSubscriptions: true, goipp.OpCreatePrinterSubscriptions: true, goipp.OpCreateJobSubscriptions: true,
	goipp.OpGetNotifications: true, goipp.OpRenewSubscription: true, goipp.OpCancelSubscription: true,
}

// subscriptionOp serves a subscription operation on the system (q nil) or on q.
func (s *System) subscriptionOp(ctx context.Context, op goipp.Op, q *queue, req, rsp *goipp.Message, fail failFunc) *goipp.Message {
	unsupported := func() *goipp.Message {
		return fail(goipp.StatusErrorOperationNotSupported, "This operation is not supported.")
	}
	if s.opts.NoSubscriptions {
		return unsupported()
	}
	switch op {
	case goipp.OpCreateSystemSubscriptions:
		if q != nil {
			return unsupported()
		}
	case goipp.OpCreatePrinterSubscriptions, goipp.OpCreateJobSubscriptions:
		if q == nil {
			return unsupported()
		}
	case goipp.OpGetNotifications:
		return s.notifications(ctx, req, rsp, fail)
	case goipp.OpRenewSubscription, goipp.OpCancelSubscription:
		ids := pappl.Ints(req.Operation, "notify-subscription-id")
		if len(ids) == 0 {
			return fail(goipp.StatusErrorBadRequest, "Missing \"notify-subscription-id\" attribute.")
		}
		if s.subs[ids[0]] == nil {
			return fail(goipp.StatusErrorNotFound, "Subscription #%d was not found.", ids[0])
		}
		if op == goipp.OpCancelSubscription {
			delete(s.subs, ids[0])
		}
		return rsp
	}

	sub := &subscription{events: map[string]bool{}}
	if q != nil {
		sub.printer = q.ID
	}
	if op == goipp.OpCreateJobSubscriptions {
		ids := pappl.Ints(req.Operation, "notify-job-id")
		if len(ids) == 0 {
			return fail(goipp.StatusErrorBadRequest, "Missing \"notify-job-id\" attribute.")
		}
		if !slices.ContainsFunc(q.jobs, func(j *job) bool { return j.id == ids[0] }) {
			return fail(goipp.StatusErrorNotFound, "Job #%d not found.", ids[0])
		}
		sub.job = ids[0]
	}
	for _, a := range req.Subscription {
		if a.Name == "notify-events" {
			for _, v := range a.Values {
				sub.events[v.V.String()] = true
			}
		}
	}
	// PAPPL answers a bad subscription group, or one past the cap, with a
	// notify-status-code and ignored-all-subscriptions.
	bad := goipp.StatusOk
	switch {
	case pappl.First(req.Subscription, "notify-pull-method") != "ippget" || len(sub.events) == 0:
		bad = goipp.StatusErrorBadRequest
	case len(s.subs) >= s.opts.MaxSubscriptions:
		bad = goipp.StatusErrorInternal
	}
	if bad != goipp.StatusOk {
		r := fail(goipp.StatusErrorIgnoredAllSubscriptions, "No subscriptions created.")
		r.Subscription.Add(goipp.MakeAttribute("notify-status-code", goipp.TagEnum, goipp.Integer(bad)))
		return r
	}
	id := s.nextSub
	s.nextSub++
	s.subs[id] = sub
	rsp.Subscription.Add(goipp.MakeAttribute("notify-subscription-id", goipp.TagInteger, goipp.Integer(int32(id)))) //nolint:gosec // small counter
	return rsp
}

// notifications serves Get-Notifications: the events of each subscription
// from its asked sequence number on, waiting once (with notify-wait) for any
// event, NotifyWait or the end of the request when there are none.
func (s *System) notifications(ctx context.Context, req, rsp *goipp.Message, fail failFunc) *goipp.Message {
	s.gets++
	ids := pappl.Ints(req.Operation, "notify-subscription-ids")
	seqs := pappl.Ints(req.Operation, "notify-sequence-numbers")
	if len(ids) == 0 {
		return fail(goipp.StatusErrorBadRequest, "Missing \"notify-subscription-ids\" attribute.")
	}
	wait := false
	for _, a := range req.Operation {
		if a.Name == "notify-wait" && len(a.Values) == 1 {
			b, _ := a.Values[0].V.(goipp.Boolean)
			wait = bool(b)
		}
	}
	for {
		var groups goipp.Groups
		for i, id := range ids {
			sub := s.subs[id]
			if sub == nil {
				r := fail(goipp.StatusErrorNotFound, "Subscription #%d was not found.", id)
				r.Unsupported.Add(goipp.MakeAttribute("notify-subscription-ids", goipp.TagInteger, goipp.Integer(int32(id)))) //nolint:gosec // client value
				return r
			}
			from := 1
			if i < len(seqs) {
				from = max(seqs[i], 1)
			}
			for _, e := range sub.log[min(from-1, len(sub.log)):] {
				groups.Add(goipp.Group{Tag: goipp.TagEventNotificationGroup, Attrs: e})
			}
		}
		if len(groups) > 0 {
			rsp.Operation.Add(goipp.MakeAttribute("notify-get-interval", goipp.TagInteger, goipp.Integer(30)))
			rsp.Groups = groups
			return rsp
		}
		if !wait {
			return rsp
		}
		wait = false // PAPPL waits once
		wake := s.wake
		s.mu.Unlock()
		t := time.NewTimer(s.opts.NotifyWait)
		select {
		case <-wake:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
		s.mu.Lock()
	}
}

// emit records event for every subscription it matches and wakes every
// waiting Get-Notifications, matched or not, as PAPPL's broadcast does. q is
// the queue and j the job it concerns (nil: none).
func (s *System) emit(event string, q *queue, j *job) {
	for id, sub := range s.subs {
		if !sub.events[event] || sub.printer != 0 && sub.printer != q.ID || sub.job != 0 && (j == nil || j.id != sub.job) {
			continue
		}
		e := goipp.Attributes{
			goipp.MakeAttribute("notify-subscription-id", goipp.TagInteger, goipp.Integer(int32(id))),             //nolint:gosec // small counter
			goipp.MakeAttribute("notify-sequence-number", goipp.TagInteger, goipp.Integer(int32(len(sub.log)+1))), //nolint:gosec // small counter
			goipp.MakeAttribute("notify-subscribed-event", goipp.TagKeyword, goipp.String(event)),
		}
		if j != nil {
			name := "job-id"
			if sub.job != 0 {
				name = "notify-job-id" // as PAPPL names it for a job subscription
			}
			e.Add(goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(int32(j.id)))) //nolint:gosec // small counter
			e.Add(goipp.MakeAttribute("job-state", goipp.TagEnum, j.state))
		}
		sub.log = append(sub.log, e)
	}
	close(s.wake)
	s.wake = make(chan struct{})
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
	attrs := goipp.Attributes{
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
	}
	return append(attrs, capsAttrs(s.opts.Caps, q.Defaults)...)
}

// capsAttrs encodes the supported lists of c and the defaults d as PAPPL
// reports them; empty lists and defaults are left out.
func capsAttrs(c device.PrinterCaps, d device.PrintSettings) goipp.Attributes {
	var out goipp.Attributes
	add := func(name string, tag goipp.Tag, vs ...goipp.Value) {
		if len(vs) > 0 {
			out.Add(goipp.MakeAttr(name, tag, vs[0], vs[1:]...))
		}
	}
	strs := func(name string, tag goipp.Tag, vs ...string) {
		var gv []goipp.Value
		for _, v := range vs {
			if v != "" {
				gv = append(gv, goipp.String(v))
			}
		}
		add(name, tag, gv...)
	}
	enums := func(name string, table map[string]goipp.Integer, vs ...string) {
		var gv []goipp.Value
		for _, v := range vs {
			if n, ok := table[v]; ok {
				gv = append(gv, n)
			}
		}
		add(name, goipp.TagEnum, gv...)
	}
	ints := func(name string, vs ...int) {
		var gv []goipp.Value
		for _, v := range vs {
			if v > 0 {
				gv = append(gv, goipp.Integer(int32(v))) //nolint:gosec // small test values
			}
		}
		add(name, goipp.TagInteger, gv...)
	}
	res := func(name string, vs ...int) {
		var gv []goipp.Value
		for _, v := range vs {
			if v > 0 {
				gv = append(gv, goipp.Resolution{Xres: v, Yres: v, Units: goipp.UnitsDpi})
			}
		}
		add(name, goipp.TagResolution, gv...)
	}

	strs("document-format-supported", goipp.TagMimeType, c.Formats...)
	strs("media-supported", goipp.TagKeyword, c.Media...)
	strs("media-default", goipp.TagKeyword, d.Media)
	strs("media-source-supported", goipp.TagKeyword, c.MediaSources...)
	strs("media-type-supported", goipp.TagKeyword, c.MediaTypes...)
	var col goipp.Collection
	for _, m := range []struct{ name, v string }{{"media-size-name", d.Media}, {"media-source", d.MediaSource}, {"media-type", d.MediaType}} {
		if m.v != "" {
			col.Add(goipp.MakeAttribute(m.name, goipp.TagKeyword, goipp.String(m.v)))
		}
	}
	if len(col) > 0 {
		add("media-col-default", goipp.TagBeginCollection, col)
	}
	strs("sides-supported", goipp.TagKeyword, c.Sides...)
	strs("sides-default", goipp.TagKeyword, d.Sides)
	strs("print-color-mode-supported", goipp.TagKeyword, c.ColorModes...)
	strs("print-color-mode-default", goipp.TagKeyword, d.ColorMode)
	enums("print-quality-supported", ippcaps.Qualities, c.Qualities...)
	enums("print-quality-default", ippcaps.Qualities, d.Quality)
	res("printer-resolution-supported", c.Resolutions...)
	res("printer-resolution-default", d.Resolution)
	strs("print-content-optimize-supported", goipp.TagKeyword, c.ContentOptimize...)
	strs("print-content-optimize-default", goipp.TagKeyword, d.ContentOptimize)
	enums("orientation-requested-supported", ippcaps.Orientations, c.Orientations...)
	enums("orientation-requested-default", ippcaps.Orientations, d.Orientation)
	ints("number-up-supported", c.NumberUp...)
	ints("number-up-default", d.NumberUp)
	strs("print-scaling-supported", goipp.TagKeyword, c.PrintScalings...)
	strs("print-scaling-default", goipp.TagKeyword, d.PrintScaling)
	strs("output-bin-supported", goipp.TagKeyword, c.OutputBins...)
	strs("output-bin-default", goipp.TagKeyword, d.OutputBin)
	enums("finishings-supported", ippcaps.Finishings, c.Finishings...)
	enums("finishings-default", ippcaps.Finishings, d.Finishings)
	if c.CopiesMax > 0 {
		add("copies-supported", goipp.TagRange, goipp.Range{Lower: 1, Upper: c.CopiesMax})
		add("copies-default", goipp.TagInteger, goipp.Integer(1))
	}
	return out
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
