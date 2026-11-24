package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ThePhaseless/printscan/internal/apitypes"
	"github.com/ThePhaseless/printscan/internal/catalog"
	"github.com/ThePhaseless/printscan/internal/config"
	"github.com/ThePhaseless/printscan/internal/device"
	"github.com/ThePhaseless/printscan/internal/printing"
	"github.com/ThePhaseless/printscan/internal/scanning"
	"github.com/ThePhaseless/printscan/internal/schema"
)

// fakeBackend records every Apply (nil included) and returns a settable Observation.
type fakeBackend struct {
	mu        sync.Mutex
	obs       device.Observation
	obsErr    error
	applyErr  error
	applied   []*device.Spec
	onApply   func()
	onObserve func()
}

func (f *fakeBackend) Apply(_ context.Context, s *device.Spec) error {
	if f.onApply != nil {
		f.onApply()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, s)
	return f.applyErr
}

func (f *fakeBackend) Observe(context.Context) (device.Observation, error) {
	if f.onObserve != nil {
		f.onObserve()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.obs, f.obsErr
}

func (f *fakeBackend) set(o device.Observation) {
	f.mu.Lock()
	f.obs = o
	f.mu.Unlock()
}

func (f *fakeBackend) applies() []*device.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*device.Spec{}, f.applied...)
}

func (f *fakeBackend) last(t *testing.T) *device.Spec {
	t.Helper()
	a := f.applies()
	if len(a) == 0 {
		t.Fatal("no Apply recorded")
	}
	return a[len(a)-1]
}

type restartBackend struct {
	*fakeBackend
	restarts int
}

func (r *restartBackend) RestartPod(context.Context) error {
	r.restarts++
	return nil
}

type fakePrinter struct{ err error }

func (p fakePrinter) Attributes(context.Context) (printing.Attributes, error) {
	return printing.Attributes{State: "idle", Formats: []string{"application/pdf"}, Media: []string{"iso_a4_210x297mm"}}, p.err
}

type fakeScanner struct{}

func (fakeScanner) Status(context.Context) (scanning.State, error) { return scanning.StateIdle, nil }

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time                       { return c.t }
func (c *fakeClock) add(d time.Duration)                  { c.t = c.t.Add(d) }
func (c *fakeClock) set(start time.Time, d time.Duration) { c.t = start.Add(d) }

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

type harness struct {
	c       *Controller
	b       *fakeBackend
	clock   *fakeClock
	printed []string // printer URIs queried
	pubs    []apitypes.Status
}

func newHarness(t *testing.T, o Options) *harness {
	t.Helper()
	h := &harness{b: &fakeBackend{obs: device.Observation{Phase: device.PhaseAbsent}}, clock: &fakeClock{t: t0}}
	if o.Backend == nil {
		o.Backend = h.b
	}
	if o.BackendName == "" {
		o.BackendName = BackendSim
	}
	o.Now = h.clock.now
	o.Printer = func(uri string) PrinterObserver {
		h.printed = append(h.printed, uri)
		return fakePrinter{}
	}
	o.Scanner = func(string) ScannerObserver { return fakeScanner{} }
	o.Publish = func(s apitypes.Status) { h.pubs = append(h.pubs, s) }
	h.c = New(o)
	return h
}

func (h *harness) step() apitypes.Status {
	h.c.step(context.Background())
	return h.c.Current()
}

func (h *harness) report(t *testing.T, fixture string, mod ...func(*device.AgentReport)) device.DesiredState {
	t.Helper()
	r := device.AgentReport{PodUID: "pod", Inventory: catalog.Fixture(fixture), DevicePresent: true}
	for _, m := range mod {
		m(&r)
	}
	ds, err := h.c.AgentReport(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ds.Browse, device.BrowseTypes) {
		t.Fatalf("Browse = %v", ds.Browse)
	}
	return ds
}

func usbConfig(name string) *config.Config {
	c := config.Default(name)
	return &c
}

func simConfig(name string) *config.Config {
	c := config.Default(name)
	c.Device.Mode = config.ModeSimulated
	return &c
}

func wantPhase(t *testing.T, s apitypes.Status, phase, message string) {
	t.Helper()
	if s.Device.Phase != phase || (message != "*" && s.Device.Message != message) {
		t.Fatalf("phase = %q %q, want %q %q", s.Device.Phase, s.Device.Message, phase, message)
	}
}

func wantApplies(t *testing.T, b *fakeBackend, n int) {
	t.Helper()
	if got := len(b.applies()); got != n {
		t.Fatalf("%d Apply calls, want %d", got, n)
	}
}

func running(endpoints device.Endpoints) device.Observation {
	return device.Observation{Phase: device.PhaseRunning, HostIP: "127.0.0.1", Endpoints: endpoints}
}

func TestInitialStatus(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	got := h.c.Current()
	want := apitypes.Status{
		Config: apitypes.StatusConfig{State: "missing", Etag: "", AppliedEtag: "", Errors: []config.FieldError{}, ChangedAt: time.Time{}},
		Device: apitypes.StatusDevice{
			Phase: "none", Mode: "none", Decision: nil, Inventory: nil, Message: "",
			Advertised: []apitypes.AdvertisedService{}, Blobs: []device.BlobStatus{},
		},
		Printer: apitypes.StatusPrinter{
			Available: false, URI: "", State: "unknown",
			Reasons: []string{}, Formats: []string{}, Media: []string{}, Markers: []apitypes.Marker{},
		},
		Scanner:   apitypes.StatusScanner{Available: false, URL: "", State: "unknown"},
		Reconcile: []apitypes.ReconcileItem{},
		Connect:   apitypes.Connect{DNSSDName: "", IPPURIs: []string{}, ESCLURLs: []string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Current() =\n%+v\nwant\n%+v", got, want)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"state":""`, `"mode":""`, `"phase":""`} {
		if bytes.Contains(b, []byte(bad)) {
			t.Errorf("initial Status contains %s: %s", bad, b)
		}
	}
	if !strings.Contains(string(b), `"changedAt":"0001-01-01T00:00:00Z"`) {
		t.Errorf("changedAt not the zero time: %s", b)
	}
	validateStatus(t, b)
	wantApplies(t, h.b, 0)
}

func validateStatus(t *testing.T, b []byte) {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema.API()))
	if err != nil {
		t.Fatal(err)
	}
	comp := jsonschema.NewCompiler()
	comp.AssertFormat()
	if err := comp.AddResource(schema.APIID, doc); err != nil {
		t.Fatal(err)
	}
	sch, err := comp.Compile(schema.APIID + "#/$defs/Status")
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate(v); err != nil {
		t.Fatalf("Status does not validate: %v\n%s", err, b)
	}
}

func TestConfigEffective(t *testing.T) {
	h := newHarness(t, Options{})
	if got := h.c.Config(); !reflect.DeepEqual(got, config.Default("Printer")) {
		t.Fatalf("before input: %+v", got)
	}
	applied := usbConfig("Applied")
	h.c.ConfigApplied(applied, "e1")
	if got := h.c.Config(); got.Name != "Applied" {
		t.Fatalf("after apply: %q", got.Name)
	}
	good := usbConfig("Good")
	h.c.ConfigInvalid(errors.New("bad"), "e2", good)
	if got := h.c.Config(); got.Name != "Good" {
		t.Fatalf("after invalid: %q", got.Name)
	}
	h.c.ConfigInvalid(errors.New("bad"), "e3", nil)
	if got := h.c.Config(); got.Name != "Printer" {
		t.Fatalf("after invalid without last-good: %q", got.Name)
	}
	h.c.ConfigMissing()
	if got := h.c.Config(); !reflect.DeepEqual(got, config.Default("Printer")) {
		t.Fatalf("after missing: %+v", got)
	}
}

func TestMissingConfig(t *testing.T) {
	t.Run("usb resource", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true})
		h.c.ConfigMissing()
		s := h.step()
		if sp := h.b.last(t); sp == nil || sp.Mode != device.ModeProbe || sp.Hash != device.SpecHash(sp) {
			t.Fatalf("applied %+v, want the probe spec", sp)
		}
		if s.Device.Mode != "probe" {
			t.Fatalf("mode %q", s.Device.Mode)
		}
		wantPhase(t, s, "probing", msgWaitingAgent)
	})
	t.Run("no usb resource", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.c.ConfigMissing()
		s := h.step()
		h.step()
		a := h.b.applies()
		if len(a) != 1 || a[0] != nil {
			t.Fatalf("applies %v, want one Apply(nil)", a)
		}
		if s.Device.Mode != "none" {
			t.Fatalf("mode %q", s.Device.Mode)
		}
		wantPhase(t, s, "none", "")
	})
}

func TestIPPUSBReachesReady(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.step()
	h.report(t, "ippusb-mfp")
	h.step()
	h.c.ConfigApplied(usbConfig("Printer"), "e1")
	h.b.set(running(device.Endpoints{PrinterURI: "ipp://127.0.0.1:4000/ipp/print", ScannerURL: "http://127.0.0.1:4001/eSCL"}))
	s := h.step()
	sp := h.b.last(t)
	if sp.Mode != device.ModeUSB || sp.Printer != "ipp-usb" || sp.Scanner != "ipp-usb" {
		t.Fatalf("spec %+v", sp)
	}
	wantPhase(t, s, "ready", "")
	if !s.Printer.Available || s.Printer.URI != "ipp://127.0.0.1:4000/ipp/print" || s.Printer.State != "idle" {
		t.Fatalf("printer %+v", s.Printer)
	}
	if !s.Scanner.Available || s.Scanner.URL != "http://127.0.0.1:4001/eSCL" || s.Scanner.State != "idle" {
		t.Fatalf("scanner %+v", s.Scanner)
	}
	if s.Device.Decision == nil || s.Device.Decision.Rule != "ipp-over-usb" || s.Device.Inventory == nil {
		t.Fatalf("decision %+v", s.Device.Decision)
	}
	if len(h.pubs) == 0 || h.pubs[len(h.pubs)-1].Device.Phase != "ready" {
		t.Fatal("ready not published")
	}
	b, _ := json.Marshal(s)
	validateStatus(t, b)
}

func TestUSBNoReportAbsentOrFailed(t *testing.T) {
	for _, ph := range []string{device.PhaseAbsent, device.PhaseFailed} {
		t.Run(ph, func(t *testing.T) {
			h := newHarness(t, Options{USBAvailable: true})
			h.b.set(device.Observation{Phase: ph})
			h.c.ConfigApplied(usbConfig("P"), "e")
			h.step()
			wantApplies(t, h.b, 1)
			if sp := h.b.last(t); sp == nil || sp.Mode != device.ModeProbe {
				t.Fatalf("spec %+v", sp)
			}
		})
	}
}

func TestUSBUnavailable(t *testing.T) {
	const msg = "device.mode usb needs a USB resource (set --usb-resource)"
	for _, invalid := range []bool{false, true} {
		h := newHarness(t, Options{BackendName: BackendK8s, USBUnavailable: msg})
		if invalid {
			h.c.ConfigInvalid(errors.New("bad"), "e", usbConfig("P"))
		} else {
			h.c.ConfigApplied(usbConfig("P"), "e")
		}
		s := h.step()
		wantPhase(t, s, "error", msg)
		h.report(t, "ippusb-mfp")
		h.clock.add(resync)
		h.step()
		a := h.b.applies()
		if len(a) != 1 || a[0] != nil {
			t.Fatalf("invalid=%v: applies %v, want exactly one Apply(nil)", invalid, a)
		}
	}
}

func TestUSBWithoutResourceInSim(t *testing.T) {
	h := newHarness(t, Options{})
	h.c.ConfigApplied(usbConfig("P"), "e")
	h.step()
	if sp := h.b.last(t); sp == nil || sp.Mode != device.ModeProbe {
		t.Fatalf("spec %+v", sp)
	}
	h.b.set(running(device.Endpoints{}))
	h.report(t, "none") // empty inventory
	s := h.step()
	wantPhase(t, s, "probing", msgNoPrinter)
	wantApplies(t, h.b, 1)
}

func TestRestartWithRunningPod(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
	h.b.set(device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20"})
	h.c.ConfigApplied(usbConfig("P"), "e")
	s := h.step()
	wantPhase(t, s, "deploying", msgWaitingAgent)
	h.clock.add(29 * time.Second)
	h.step()
	wantApplies(t, h.b, 0)
	h.report(t, "ippusb-mfp")
	h.step()
	if sp := h.b.last(t); sp.Mode != device.ModeUSB || sp.Printer != "ipp-usb" {
		t.Fatalf("spec %+v", sp)
	}
	wantApplies(t, h.b, 1)
}

func TestRestartWithPendingPod(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
	h.b.set(device.Observation{Phase: device.PhasePending})
	h.c.ConfigApplied(usbConfig("P"), "e")
	h.step()
	h.clock.set(t0, 31*time.Second)
	h.step()
	h.clock.set(t0, 4*time.Minute)
	h.step()
	wantApplies(t, h.b, 0)
	h.clock.set(t0, 5*time.Minute)
	h.step()
	if sp := h.b.last(t); sp.Mode != device.ModeProbe {
		t.Fatalf("spec %+v", sp)
	}
}

// A control-plane restart must not replace a running device pod whose agent
// is still backing off (up to 30 s) or waiting for Service endpoints.
func TestRestartWithRunningPodNoReport(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
	h.b.set(device.Observation{Phase: device.PhaseRunning})
	h.c.ConfigApplied(usbConfig("P"), "e")
	h.step()
	wantApplies(t, h.b, 0)
	h.clock.set(t0, resync)
	h.step()
	h.clock.set(t0, 2*time.Minute)
	s := h.step()
	wantApplies(t, h.b, 0)
	wantPhase(t, s, "deploying", msgWaitingAgent)
	h.report(t, "ippusb-mfp")
	h.step()
	if sp := h.b.last(t); sp.Mode != device.ModeUSB {
		t.Fatalf("spec %+v, want the usb spec without a probe in between", sp)
	}
	wantApplies(t, h.b, 1)
}

func TestRestartWithRunningPodNeverReporting(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
	h.b.set(device.Observation{Phase: device.PhaseRunning})
	h.c.ConfigApplied(usbConfig("P"), "e")
	h.clock.set(t0, pendingWait-time.Second)
	h.step()
	wantApplies(t, h.b, 0)
	h.clock.set(t0, pendingWait)
	h.step()
	if sp := h.b.last(t); sp.Mode != device.ModeProbe {
		t.Fatalf("spec %+v", sp)
	}
}

// Until the watcher's first callback the file is not known to be missing, so
// the controller must not tear down or probe over a running device pod.
func TestNoApplyBeforeConfigRead(t *testing.T) {
	for _, usb := range []bool{true, false} {
		h := newHarness(t, Options{USBAvailable: usb, BackendName: BackendK8s})
		h.b.set(device.Observation{Phase: device.PhaseRunning})
		h.step()
		h.clock.set(t0, 10*time.Minute)
		h.report(t, "ippusb-mfp")
		s := h.step()
		wantApplies(t, h.b, 0)
		if s.Config.State != "missing" {
			t.Fatalf("config state %q", s.Config.State)
		}
		h.c.ConfigMissing()
		h.step()
		wantApplies(t, h.b, 1)
	}
}

func TestUnschedulable(t *testing.T) {
	t.Run("probe pod", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
		h.c.ConfigMissing()
		h.step()
		h.b.set(device.Observation{Phase: device.PhasePending, Unschedulable: true, Message: "0/3 nodes"})
		s := h.step()
		wantPhase(t, s, "probing", msgUnschedulable)
		wantApplies(t, h.b, 1)
	})
	t.Run("usb pod before first report", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
		h.b.set(device.Observation{Phase: device.PhasePending, Unschedulable: true})
		h.c.ConfigApplied(usbConfig("P"), "e")
		s := h.step()
		wantPhase(t, s, "probing", msgUnschedulable)
		h.clock.set(t0, 2*time.Minute)
		h.step()
		wantApplies(t, h.b, 0)
		h.clock.set(t0, 5*time.Minute)
		h.step()
		wantApplies(t, h.b, 1)
	})
}

func TestProbeThenConfigAppliesOnce(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.c.ConfigMissing()
	h.step()
	h.report(t, "brother-pcl-laser")
	h.step()
	if d := h.c.Setup().Decision; d == nil || d.Rule != "pcl" {
		t.Fatalf("setup decision %+v", d)
	}
	h.c.ConfigApplied(usbConfig("Brother"), "e")
	h.step()
	wantApplies(t, h.b, 2)
	if sp := h.b.last(t); sp.Mode != device.ModeUSB || sp.Printer != "ghostscript" {
		t.Fatalf("spec %+v", sp)
	}
	h.report(t, "brother-pcl-laser")
	h.step()
	wantApplies(t, h.b, 2)
}

func TestAgentOutage(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.c.ConfigApplied(usbConfig("P"), "e")
	h.report(t, "ippusb-mfp")
	h.b.set(running(device.Endpoints{PrinterURI: "ipp://127.0.0.1:1/ipp/print", ScannerURL: "http://127.0.0.1:2/eSCL"}))
	wantPhase(t, h.step(), "ready", "")
	h.clock.add(31 * time.Second)
	s := h.step()
	wantPhase(t, s, "degraded", msgNotReporting)
	wantApplies(t, h.b, 1)
	if s.Device.Inventory == nil || len(s.Device.Inventory.Devices) == 0 {
		t.Fatal("inventory dropped")
	}
}

func TestNoPrinterKeepsProbe(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.c.ConfigApplied(usbConfig("P"), "e")
	h.report(t, "no-printer")
	h.b.set(running(device.Endpoints{}))
	s := h.step()
	wantPhase(t, s, "probing", msgNoPrinter)
	if sp := h.b.last(t); sp.Mode != device.ModeProbe {
		t.Fatalf("spec %+v", sp)
	}
	sel := usbConfig("P")
	sel.Device.USB = &config.USB{VendorID: "ffff", ProductID: "ffff"}
	h.c.ConfigApplied(sel, "e2")
	wantPhase(t, h.step(), "probing", msgNoMatch)
	wantApplies(t, h.b, 1)
}

func TestPodRelevantChange(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	cfg := usbConfig("P")
	h.c.ConfigApplied(cfg, "e1")
	h.report(t, "ippusb-mfp")
	h.step()
	first := h.b.last(t)
	h.c.ConfigApplied(usbConfig("P"), "e2") // unchanged content
	h.step()
	wantApplies(t, h.b, 1)
	none := usbConfig("P")
	none.Device.Scanner = config.ScannerNone
	h.c.ConfigApplied(none, "e3")
	h.step()
	wantApplies(t, h.b, 2)
	if sp := h.b.last(t); sp.Hash == first.Hash || sp.Scanner != "" {
		t.Fatalf("spec %+v", sp)
	}
	h.step()
	wantApplies(t, h.b, 2)
}

func TestQueueChangesWithoutApply(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.c.ConfigApplied(usbConfig("HP"), "e1")
	h.report(t, "hp-legacy-mfp")
	h.step()
	sp := h.b.last(t)
	ds := h.report(t, "hp-legacy-mfp")
	if ds.Hash != sp.Hash || ds.Queue == nil {
		t.Fatalf("desired %+v", ds)
	}
	want := device.QueueSpec{
		Name: "HP", Driver: "auto", Schemes: []string{"hp", "usb"},
		Media: "iso_a4_210x297mm", Sides: "one-sided", ColorMode: "auto", Quality: "normal",
	}
	if !reflect.DeepEqual(*ds.Queue, want) {
		t.Fatalf("queue %+v", *ds.Queue)
	}

	media := usbConfig("HP")
	media.Print.Defaults.Media = "na_letter_8.5x11in"
	h.c.ConfigApplied(media, "e2")
	if ds := h.report(t, "hp-legacy-mfp"); ds.Queue.Media != "na_letter_8.5x11in" || ds.Hash != sp.Hash {
		t.Fatalf("desired %+v", ds)
	}
	h.step()

	loc := usbConfig("HP")
	loc.Location = "Office"
	h.c.ConfigApplied(loc, "e3")
	if ds := h.report(t, "hp-legacy-mfp"); ds.Queue.Location != "Office" || ds.Hash != sp.Hash {
		t.Fatalf("desired %+v", ds)
	}
	h.step()
	wantApplies(t, h.b, 1)
}

func TestSimulatedNameChange(t *testing.T) {
	h := newHarness(t, Options{})
	h.c.ConfigApplied(simConfig("One"), "e1")
	s := h.step()
	first := h.b.last(t)
	if first.Mode != device.ModeSimulated || first.Name != "One" {
		t.Fatalf("spec %+v", first)
	}
	if s.Device.Decision == nil || s.Device.Decision.Device != nil || s.Device.Mode != "simulated" {
		t.Fatalf("decision %+v", s.Device.Decision)
	}
	h.c.ConfigApplied(simConfig("Two"), "e2")
	h.step()
	h.step()
	wantApplies(t, h.b, 2)
	if sp := h.b.last(t); sp.Name != "Two" || sp.Hash == first.Hash {
		t.Fatalf("spec %+v", sp)
	}
}

func deviceItem(t *testing.T, s apitypes.Status) apitypes.ReconcileItem {
	t.Helper()
	for _, r := range s.Reconcile {
		if r.Item == "device" {
			return r
		}
	}
	t.Fatalf("no device reconcile item in %+v", s.Reconcile)
	return apitypes.ReconcileItem{}
}

func TestApplyFailureRetried(t *testing.T) {
	h := newHarness(t, Options{})
	h.b.applyErr = errors.New("owner deployment printing/printscan: not found")
	h.c.ConfigApplied(simConfig("S"), "e")
	s := h.step()
	if r := deviceItem(t, s); r.OK || r.Message != "owner deployment printing/printscan: not found" {
		t.Fatalf("item %+v", r)
	}
	wantPhase(t, s, "error", "*")
	h.clock.add(10 * time.Second)
	h.step()
	wantApplies(t, h.b, 1)
	h.b.applyErr = nil
	h.clock.add(20 * time.Second)
	s = h.step()
	wantApplies(t, h.b, 2)
	if r := deviceItem(t, s); !r.OK || r.Message != "" {
		t.Fatalf("item %+v", r)
	}
}

func TestOptionalBlob(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.c.ConfigApplied(usbConfig("HP"), "e1")
	h.report(t, "hp-legacy-mfp")
	h.step()
	if sp := h.b.last(t); len(sp.Blobs) != 0 {
		t.Fatalf("blobs %+v without an accepted EULA", sp.Blobs)
	}
	acc := usbConfig("HP")
	acc.Licenses.AcceptedEULAs = []config.EULARef{{ID: "hplip-plugin", Version: "3.24.4"}}
	h.c.ConfigApplied(acc, "e2")
	h.step()
	want := []device.BlobRef{{ID: "hplip-plugin", Version: "3.24.4", Optional: true}}
	if sp := h.b.last(t); !reflect.DeepEqual(sp.Blobs, want) {
		t.Fatalf("blobs %+v", sp.Blobs)
	}
	h.b.set(running(device.Endpoints{}))
	h.report(t, "hp-legacy-mfp", func(r *device.AgentReport) {
		r.Blobs = []device.BlobStatus{{ID: "hplip-plugin", Version: "3.24.4", State: "failed", Message: "download failed"}}
	})
	s := h.step()
	if len(s.Device.Blobs) != 1 || s.Device.Blobs[0].State != "failed" {
		t.Fatalf("blobs %+v", s.Device.Blobs)
	}
	wantPhase(t, s, "degraded", "HP Linux Imaging and Printing proprietary plug-in: download failed")
}

func TestQueueError(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.c.ConfigApplied(usbConfig("HP"), "e1")
	h.b.set(running(device.Endpoints{}))
	h.report(t, "hp-legacy-mfp", func(r *device.AgentReport) {
		r.Queue = &device.QueueResult{State: device.QueueError, Message: "agent has no --pappl-url"}
	})
	wantPhase(t, h.step(), "error", "agent has no --pappl-url")
}

func TestInvalidConfig(t *testing.T) {
	t.Run("keeps last-good spec", func(t *testing.T) {
		h := newHarness(t, Options{})
		good := simConfig("S")
		h.c.ConfigApplied(good, "e1")
		h.step()
		el := &config.ErrorList{Errors: []config.FieldError{{Path: "/name", Line: 2, Column: 7, Message: "bad"}}}
		h.c.ConfigInvalid(el, "e2", good)
		s := h.step()
		wantApplies(t, h.b, 1)
		if s.Config.State != "invalid" || s.Config.Etag != "e2" || s.Config.AppliedEtag != "e1" || s.Config.Errors[0].Line != 2 {
			t.Fatalf("config %+v", s.Config)
		}
		if s.Device.Mode != "simulated" {
			t.Fatalf("mode %q", s.Device.Mode)
		}
	})
	t.Run("no last-good", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true})
		h.c.ConfigInvalid(errors.New("bad"), "e", nil)
		h.step()
		h.clock.add(resync)
		h.step()
		wantApplies(t, h.b, 0)
	})
	t.Run("probe spec then invalid is not ready", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true})
		h.c.ConfigMissing()
		h.step()
		h.b.set(running(device.Endpoints{}))
		h.c.ConfigInvalid(errors.New("bad"), "e", nil)
		s := h.step()
		wantApplies(t, h.b, 1)
		if s.Device.Phase != "probing" {
			t.Fatalf("phase %q", s.Device.Phase)
		}
	})
}

func TestDeviceLossRestartsPod(t *testing.T) {
	rb := &restartBackend{fakeBackend: &fakeBackend{obs: device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20"}}}
	h := newHarness(t, Options{Backend: rb, BackendName: BackendK8s, USBAvailable: true})
	h.c.ConfigApplied(usbConfig("P"), "e")
	h.report(t, "ippusb-mfp")
	h.step()
	h.report(t, "ippusb-mfp", func(r *device.AgentReport) { r.DevicePresent = false })
	h.step()
	h.clock.add(20 * time.Second)
	h.report(t, "ippusb-mfp", func(r *device.AgentReport) { r.DevicePresent = false })
	h.step()
	if rb.restarts != 0 {
		t.Fatal("restarted before 30 s")
	}
	h.clock.add(10 * time.Second)
	h.report(t, "ippusb-mfp", func(r *device.AgentReport) { r.DevicePresent = false })
	h.step()
	h.clock.add(10 * time.Second)
	h.report(t, "ippusb-mfp", func(r *device.AgentReport) { r.DevicePresent = false })
	h.step()
	if rb.restarts != 1 {
		t.Fatalf("%d restarts, want 1", rb.restarts)
	}
}

func TestPrinterURIK8sPAPPL(t *testing.T) {
	h := newHarness(t, Options{BackendName: BackendK8s, USBAvailable: true})
	h.c.ConfigApplied(usbConfig("x"), "e")
	h.b.set(device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20"})
	h.report(t, "hp-legacy-mfp", func(r *device.AgentReport) {
		r.Queue = &device.QueueResult{State: device.QueueOK, PrinterURI: "ipp://127.0.0.1:8002/ipp/print/x"}
	})
	s := h.step()
	const want = "ipp://192.168.1.20:8002/ipp/print/x"
	if s.Printer.URI != want || !reflect.DeepEqual(s.Connect.IPPURIs, []string{want}) || !s.Printer.Available {
		t.Fatalf("printer %+v connect %+v", s.Printer, s.Connect)
	}
	wantPhase(t, s, "ready", "")
	if s.Scanner.Available || s.Scanner.State != "unknown" {
		t.Fatalf("scanner %+v without _uscan._tcp", s.Scanner)
	}
	for _, u := range h.printed {
		if strings.Contains(u, "127.0.0.1") {
			t.Fatalf("queried loopback URI %s", u)
		}
	}
}

func TestPrinterURISimPAPPL(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.c.ConfigApplied(usbConfig("Printer"), "e")
	h.b.set(running(device.Endpoints{PrinterURI: "", ScannerURL: "http://127.0.0.1:41234/eSCL"}))
	h.report(t, "hp-legacy-mfp")
	s := h.step()
	if s.Printer.Available || s.Printer.State != "unknown" || s.Printer.URI != "" {
		t.Fatalf("printer before QueueResult %+v", s.Printer)
	}
	h.report(t, "hp-legacy-mfp", func(r *device.AgentReport) {
		r.Queue = &device.QueueResult{State: device.QueueOK, PrinterURI: "ipp://127.0.0.1:39001/ipp/print/Printer"}
	})
	s = h.step()
	if s.Printer.URI != "ipp://127.0.0.1:39001/ipp/print/Printer" || s.Scanner.URL != "http://127.0.0.1:41234/eSCL" {
		t.Fatalf("printer %+v scanner %+v", s.Printer, s.Scanner)
	}
	wantPhase(t, s, "ready", "")
}

func TestPrinterURISimIPPUSB(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.c.ConfigApplied(usbConfig("Printer"), "e")
	h.b.set(running(device.Endpoints{PrinterURI: "ipp://127.0.0.1:40000/ipp/print"}))
	h.report(t, "ippusb-mfp", func(r *device.AgentReport) {
		r.Services = []device.DNSSDService{{Type: device.ServiceIPP, Name: "x", Port: 1, TXT: map[string]string{"rp": "ipp/print"}}}
	})
	if s := h.step(); s.Printer.URI != "ipp://127.0.0.1:40000/ipp/print" {
		t.Fatalf("printer %+v", s.Printer)
	}
}

func TestPrinterURIK8sSimulated(t *testing.T) {
	ipp := func(name string, port int) device.DNSSDService {
		return device.DNSSDService{Type: device.ServiceIPP, Name: name, Port: port, TXT: map[string]string{"rp": "ipp/print"}}
	}
	cases := []struct {
		services []device.DNSSDService
		uri      string
		name     string
	}{
		{[]device.DNSSDService{ipp("Other printer", 631), ipp("Living room printer", 8631)}, "ipp://192.168.1.20:8631/ipp/print", "Living room printer"},
		{[]device.DNSSDService{ipp("Something else", 8631)}, "ipp://192.168.1.20:8631/ipp/print", "Something else"},
	}
	for _, tc := range cases {
		h := newHarness(t, Options{BackendName: BackendK8s})
		h.c.ConfigApplied(simConfig("Living room printer"), "e")
		h.b.set(device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20"})
		h.report(t, "none", func(r *device.AgentReport) {
			r.Services = append(tc.services, device.DNSSDService{
				Type: device.ServiceUScan, Name: "scan", Port: 8090, TXT: map[string]string{"rs": "eSCL"},
			})
		})
		s := h.step()
		if s.Printer.URI != tc.uri || s.Connect.DNSSDName != tc.name {
			t.Fatalf("printer %q dnssd %q, want %q %q", s.Printer.URI, s.Connect.DNSSDName, tc.uri, tc.name)
		}
		if s.Scanner.URL != "http://192.168.1.20:8090/eSCL" {
			t.Fatalf("scanner %q", s.Scanner.URL)
		}
		if len(s.Device.Advertised) != len(tc.services)+1 {
			t.Fatalf("advertised %+v", s.Device.Advertised)
		}
	}
}

func TestNoLockAcrossBackendCalls(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.c.ConfigMissing()
	call := func() {
		h.c.Current()
		if _, err := h.c.AgentReport(context.Background(), device.AgentReport{Inventory: catalog.Fixture("ippusb-mfp"), DevicePresent: true}); err != nil {
			t.Error(err)
		}
	}
	h.b.onApply, h.b.onObserve = call, call
	done := make(chan struct{})
	go func() {
		h.c.step(context.Background())
		h.c.step(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop blocked: a lock is held across a backend call")
	}
	wantApplies(t, h.b, 1)
}

func TestDesiredStateHash(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	if ds := h.report(t, "ippusb-mfp"); ds.Hash != "" || ds.Queue != nil {
		t.Fatalf("before any spec: %+v", ds)
	}
	h.c.ConfigMissing()
	h.step()
	if ds := h.report(t, "ippusb-mfp"); ds.Hash != h.b.last(t).Hash || ds.Hash == "" {
		t.Fatalf("hash %q, want %q", ds.Hash, h.b.last(t).Hash)
	}
	h.c.ConfigApplied(usbConfig("P"), "e")
	h.step()
	if ds := h.report(t, "ippusb-mfp"); ds.Hash != h.b.last(t).Hash || ds.Queue != nil {
		t.Fatalf("desired %+v, spec %+v", ds, h.b.last(t))
	}
}

func TestPublishesConfigAndReconcileChanges(t *testing.T) {
	h := newHarness(t, Options{})
	ch, cancel := h.c.Subscribe()
	defer cancel()
	h.c.ConfigApplied(simConfig("S"), "e1")
	h.b.set(running(device.Endpoints{PrinterURI: "ipp://127.0.0.1:1/ipp/print", ScannerURL: "http://127.0.0.1:2/eSCL"}))
	h.step()
	n := len(h.pubs)
	h.step()
	if len(h.pubs) != n {
		t.Fatal("published an unchanged Status")
	}
	h.c.ConfigInvalid(errors.New("bad"), "e2", simConfig("S"))
	h.step()
	if len(h.pubs) != n+1 || h.pubs[n].Config.State != "invalid" {
		t.Fatalf("config change not published: %d publishes", len(h.pubs))
	}
	h.c.SetReconcileItem("data-dir", false, "data directory /data is not writable")
	h.step()
	if len(h.pubs) != n+2 || len(h.pubs[n+1].Reconcile) != 2 {
		t.Fatalf("reconcile change not published: %+v", h.pubs[len(h.pubs)-1].Reconcile)
	}
	select {
	case s := <-ch:
		if len(s.Reconcile) != 2 {
			t.Fatalf("subscriber got %+v", s.Reconcile)
		}
	default:
		t.Fatal("subscriber got nothing")
	}
}

func TestSetup(t *testing.T) {
	t.Run("missing with hp-legacy", func(t *testing.T) {
		h := newHarness(t, Options{BackendName: BackendK8s, USBAvailable: true})
		h.report(t, "hp-legacy-mfp")
		s := h.c.Setup()
		if !s.Required || s.Backend != "k8s" || !s.USBResourceConfigured || s.Inventory == nil {
			t.Fatalf("setup %+v", s)
		}
		if s.Decision == nil || s.Decision.Rule != "hp" {
			t.Fatalf("decision %+v", s.Decision)
		}
		if len(s.EULAs) != 1 || s.EULAs[0].ID != "hplip-plugin" || s.EULAs[0].Required || s.EULAs[0].Version != "3.24.4" || s.EULAs[0].Title == "" {
			t.Fatalf("eulas %+v", s.EULAs)
		}
		want := config.Default("HP LaserJet Professional M1132 MFP")
		if !reflect.DeepEqual(s.Suggested, want) {
			t.Fatalf("suggested %+v", s.Suggested)
		}
	})
	t.Run("missing before any report", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true})
		s := h.c.Setup()
		if !s.Required || s.Inventory != nil || s.Decision != nil || len(s.EULAs) != 0 || s.EULAs == nil || s.Suggested.Name != "Printer" {
			t.Fatalf("setup %+v", s)
		}
	})
	t.Run("no usb", func(t *testing.T) {
		h := newHarness(t, Options{})
		s := h.c.Setup()
		if s.USBResourceConfigured || s.Decision != nil || s.Suggested.Device.Mode != "simulated" || s.Backend != "sim" {
			t.Fatalf("setup %+v", s)
		}
	})
	t.Run("sanitised name", func(t *testing.T) {
		for product, want := range map[string]string{
			"":                                "Printer",
			"  \t":                            "Printer",
			"A\x01" + strings.Repeat("é", 70): "A" + strings.Repeat("é", 62),
		} {
			h := newHarness(t, Options{USBAvailable: true})
			h.report(t, "ippusb-mfp", func(r *device.AgentReport) { r.Inventory.Devices[0].Product = product })
			s := h.c.Setup()
			if s.Suggested.Name != want {
				t.Fatalf("product %q: name %q, want %q", product, s.Suggested.Name, want)
			}
			raw, _ := json.Marshal(s.Suggested)
			if _, err := config.Check(raw, catalog.Known()); err != nil {
				t.Fatalf("suggested %q invalid: %v", s.Suggested.Name, err)
			}
		}
	})
	t.Run("invalid with simulated last-good", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true})
		h.c.ConfigInvalid(errors.New("bad"), "e", simConfig("S"))
		s := h.c.Setup()
		if s.Required || s.Decision == nil || s.Decision.Rule != "simulated" || s.Decision.Device != nil {
			t.Fatalf("setup %+v", s)
		}
	})
	t.Run("accepted EULA listed", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true})
		cfg := usbConfig("P")
		cfg.Licenses.AcceptedEULAs = []config.EULARef{{ID: "hplip-plugin", Version: "3.24.4"}}
		h.c.ConfigApplied(cfg, "e")
		h.report(t, "ippusb-mfp")
		s := h.c.Setup()
		if s.Required || len(s.EULAs) != 1 || s.EULAs[0].ID != "hplip-plugin" || s.EULAs[0].Required {
			t.Fatalf("eulas %+v", s.EULAs)
		}
	})
}

func TestRunReactsToInputsAndStops(t *testing.T) {
	b := &fakeBackend{obs: device.Observation{Phase: device.PhaseAbsent}}
	c := New(Options{Backend: b, BackendName: BackendSim})
	ch, cancelSub := c.Subscribe()
	defer cancelSub()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	c.ConfigApplied(simConfig("S"), "e1")
	deadline := time.After(2 * time.Second)
	for s := (apitypes.Status{}); s.Device.Mode != "simulated"; {
		select {
		case s = <-ch:
		case <-deadline:
			t.Fatal("no simulated Status published within 2 s")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRequiredEULA(t *testing.T) {
	cat := *catalog.Default()
	cat.Rules = slices.Clone(cat.Rules)
	i := slices.IndexFunc(cat.Rules, func(r catalog.Rule) bool { return r.Name == "hp" })
	cat.Rules[i].RequiredEULAs, cat.Rules[i].OptionalEULAs = []string{"hplip-plugin"}, nil
	h := newHarness(t, Options{Catalog: &cat, USBAvailable: true})
	h.report(t, "hp-legacy-mfp")
	if e := h.c.Setup().EULAs; len(e) != 1 || !e[0].Required {
		t.Fatalf("eulas %+v", e)
	}
	h.c.ConfigApplied(usbConfig("HP"), "e")
	h.b.set(running(device.Endpoints{}))
	s := h.step()
	if sp := h.b.last(t); sp.Mode != device.ModeUSB || len(sp.Blobs) != 0 {
		t.Fatalf("spec %+v, want the usb spec without the blob", sp)
	}
	wantPhase(t, s, "degraded", "Accept EULA HP Linux Imaging and Printing proprietary plug-in in Settings")
}
