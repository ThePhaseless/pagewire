package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/printing"
	"github.com/ThePhaseless/pagewire/internal/scanning"
	"github.com/ThePhaseless/pagewire/internal/schema"
)

// fakeBackend records every Apply and returns settable Observations.
type fakeBackend struct {
	mu        sync.Mutex
	obs       []device.Observation
	obsErr    error
	applyErr  error
	applied   [][]*device.Spec
	calls     []string // "nodes", "observe", "apply" in order
	onApply   func()
	onObserve func()
}

func (f *fakeBackend) Apply(_ context.Context, specs []*device.Spec) error {
	if f.onApply != nil {
		f.onApply()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "apply")
	f.applied = append(f.applied, specs)
	return f.applyErr
}

func (f *fakeBackend) Observe(context.Context) ([]device.Observation, error) {
	if f.onObserve != nil {
		f.onObserve()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "observe")
	if len(f.obs) == 0 {
		return []device.Observation{{Phase: device.PhaseAbsent}}, f.obsErr
	}
	return slices.Clone(f.obs), f.obsErr
}

// set makes Observe return obs.
func (f *fakeBackend) set(obs ...device.Observation) {
	f.mu.Lock()
	f.obs = obs
	f.mu.Unlock()
}

func (f *fakeBackend) applies() [][]*device.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.applied)
}

// last is the last Apply's specs.
func (f *fakeBackend) last(t *testing.T) []*device.Spec {
	t.Helper()
	a := f.applies()
	if len(a) == 0 {
		t.Fatal("no Apply recorded")
	}
	return a[len(a)-1]
}

// one is the last Apply's only spec.
func (f *fakeBackend) one(t *testing.T) *device.Spec {
	t.Helper()
	specs := f.last(t)
	if len(specs) != 1 {
		t.Fatalf("last Apply has %d specs, want 1: %+v", len(specs), specs)
	}
	return specs[0]
}

type restartBackend struct {
	*fakeBackend
	restarts []string // workloads
}

func (r *restartBackend) RestartPod(_ context.Context, w string) error {
	r.mu.Lock()
	r.restarts = append(r.restarts, w)
	r.mu.Unlock()
	return nil
}

func (r *restartBackend) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.restarts)
}

// nodeBackend is a k8s-like backend: it lists nodes and restarts pods.
type nodeBackend struct {
	*restartBackend
	nodes    []string
	nodesErr error
	onNodes  func()
}

func (n *nodeBackend) USBNodes(context.Context) ([]string, error) {
	if n.onNodes != nil {
		n.onNodes()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, "nodes")
	return slices.Clone(n.nodes), n.nodesErr
}

func newNodeBackend(nodes ...string) *nodeBackend {
	return &nodeBackend{restartBackend: &restartBackend{fakeBackend: &fakeBackend{}}, nodes: nodes}
}

// deviceBackend records SetUSBDevices calls, and whether each came before an Apply.
type deviceBackend struct {
	*fakeBackend
	devs    [][]device.USBDevice
	staged  []bool
	applies []int // len(devs) at each Apply
}

func (d *deviceBackend) SetUSBDevices(devs []device.USBDevice, beforeApply bool) {
	d.devs = append(d.devs, devs)
	d.staged = append(d.staged, beforeApply)
}

func (d *deviceBackend) Apply(ctx context.Context, s []*device.Spec) error {
	d.applies = append(d.applies, len(d.devs))
	return d.fakeBackend.Apply(ctx, s)
}

type fakePrinter struct {
	attrs printing.Attributes
	err   error
}

func (p fakePrinter) Attributes(context.Context) (printing.Attributes, error) { return p.attrs, p.err }

type fakeScanner struct {
	h   *harness
	url string
}

func (s fakeScanner) Status(context.Context) (scanning.State, error) { return scanning.StateIdle, nil }

func (s fakeScanner) Capabilities(context.Context) (scanning.Caps, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.h.capsCalls[s.url]++
	if err := s.h.capsErr[s.url]; err != nil {
		return scanning.Caps{}, err
	}
	if c, ok := s.h.scanCaps[s.url]; ok {
		return c, nil
	}
	return scanning.Caps{Sources: []string{"platen"}, Resolutions: []int{300}, ColorModes: []string{"color"}}, nil
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) set(start time.Time, d time.Duration) {
	c.mu.Lock()
	c.t = start.Add(d)
	c.mu.Unlock()
}

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

type harness struct {
	c     *Controller
	b     *fakeBackend
	clock *fakeClock
	pubs  []apitypes.Status

	mu        sync.Mutex
	printed   []string                       // printer URIs queried
	attrs     map[string]printing.Attributes // by URI; default idleAttrs
	scanCaps  map[string]scanning.Caps
	capsErr   map[string]error
	capsCalls map[string]int
}

var idleAttrs = printing.Attributes{State: "idle", Formats: []string{"application/pdf"}, Media: []string{"iso_a4_210x297mm"}}

func newHarness(t *testing.T, o Options) *harness {
	t.Helper()
	h := &harness{
		b: &fakeBackend{}, clock: &fakeClock{t: t0},
		attrs: map[string]printing.Attributes{}, scanCaps: map[string]scanning.Caps{}, capsErr: map[string]error{}, capsCalls: map[string]int{},
	}
	switch b := o.Backend.(type) {
	case nil:
		o.Backend = h.b
	case *restartBackend:
		h.b = b.fakeBackend
	case *nodeBackend:
		h.b = b.fakeBackend
	case *deviceBackend:
		h.b = b.fakeBackend
	}
	if o.BackendName == "" {
		o.BackendName = BackendSim
	}
	o.Now = h.clock.now
	o.Printer = func(uri string) PrinterObserver {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.printed = append(h.printed, uri)
		a, ok := h.attrs[uri]
		if !ok {
			a = idleAttrs
		}
		return fakePrinter{attrs: a}
	}
	o.Scanner = func(url string) ScannerObserver { return fakeScanner{h: h, url: url} }
	o.Publish = func(s apitypes.Status) { h.pubs = append(h.pubs, s) }
	h.c = New(o)
	return h
}

func (h *harness) step() apitypes.Status {
	h.c.step(context.Background())
	return h.c.Current()
}

// report sends an agent report of workload "" with the fixture's inventory.
func (h *harness) report(t *testing.T, fixture string, mod ...func(*device.AgentReport)) device.DesiredState {
	t.Helper()
	return h.reportOn(t, "", fixture, mod...)
}

func (h *harness) reportOn(t *testing.T, workload, fixture string, mod ...func(*device.AgentReport)) device.DesiredState {
	t.Helper()
	r := device.AgentReport{PodUID: "pod", Workload: workload, Inventory: catalog.Fixture(fixture)}
	for _, m := range mod {
		m(&r)
	}
	ds, err := h.c.AgentReport(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ds.Browse, device.BrowseTypes) || ds.Queues == nil || ds.StageEULAs == nil {
		t.Fatalf("desired state %+v", ds)
	}
	return ds
}

// dev is the fixture's device with the given vendor:product.
func dev(t *testing.T, fixture, vendorProduct string) device.USBDevice {
	t.Helper()
	for _, d := range catalog.Fixture(fixture).Devices {
		if d.VendorID+":"+d.ProductID == vendorProduct {
			return d
		}
	}
	t.Fatalf("no %s in fixture %s", vendorProduct, fixture)
	return device.USBDevice{}
}

func sel(d device.USBDevice) *config.USB {
	return &config.USB{VendorID: d.VendorID, ProductID: d.ProductID, Serial: d.Serial}
}

func usbPrinter(id string, d device.USBDevice) config.Printer {
	return config.Printer{ID: id, Name: id, Enabled: true, Mode: config.ModeUSB, USB: sel(d), Driver: config.Auto}
}

func usbScanner(id string, d device.USBDevice) config.Scanner {
	return config.Scanner{ID: id, Name: id, Enabled: true, Mode: config.ModeUSB, USB: sel(d), Backend: config.BackendAuto}
}

func simPrinter(id string) config.Printer {
	return config.Printer{ID: id, Name: id, Enabled: true, Mode: config.ModeSimulated, Driver: config.Auto}
}

func simScanner(id string) config.Scanner {
	return config.Scanner{ID: id, Name: id, Enabled: true, Mode: config.ModeSimulated, Backend: config.BackendAuto}
}

func cfgOf(printers []config.Printer, scanners ...config.Scanner) *config.Config {
	c := config.Default("Home")
	c.Printers, c.Scanners = append(c.Printers, printers...), append(c.Scanners, scanners...)
	return &c
}

// mfpConfig is a printer and a scanner entry on the fixture's first device.
func mfpConfig(t *testing.T, fixture string) *config.Config {
	t.Helper()
	d := catalog.Fixture(fixture).Devices[0]
	return cfgOf([]config.Printer{usbPrinter("printer", d)}, usbScanner("scanner", d))
}

func simConfig() *config.Config {
	return cfgOf([]config.Printer{simPrinter("printer")}, simScanner("scanner"))
}

func wantPhase(t *testing.T, s apitypes.Status, phase, message string) {
	t.Helper()
	if s.Phase != phase || (message != "*" && s.Message != message) {
		t.Fatalf("phase = %q %q, want %q %q", s.Phase, s.Message, phase, message)
	}
}

func printerStatus(t *testing.T, s apitypes.Status, id string) apitypes.StatusPrinter {
	t.Helper()
	for _, p := range s.Printers {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no printer %s in %+v", id, s.Printers)
	return apitypes.StatusPrinter{}
}

func scannerStatus(t *testing.T, s apitypes.Status, id string) apitypes.StatusScanner {
	t.Helper()
	for _, sc := range s.Scanners {
		if sc.ID == id {
			return sc
		}
	}
	t.Fatalf("no scanner %s in %+v", id, s.Scanners)
	return apitypes.StatusScanner{}
}

func wantEntry(t *testing.T, gotPhase, gotMsg, phase, message string) {
	t.Helper()
	if gotPhase != phase || (message != "*" && gotMsg != message) {
		t.Fatalf("entry phase = %q %q, want %q %q", gotPhase, gotMsg, phase, message)
	}
}

func wantApplies(t *testing.T, b *fakeBackend, n int) {
	t.Helper()
	if got := len(b.applies()); got != n {
		t.Fatalf("%d Apply calls, want %d", got, n)
	}
}

func running(endpoints ...device.Endpoint) device.Observation {
	return device.Observation{Phase: device.PhaseRunning, HostIP: "127.0.0.1", Endpoints: endpoints}
}

func validateStatus(t *testing.T, s apitypes.Status) {
	t.Helper()
	validate(t, "Status", s)
}

func validate(t *testing.T, def string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema.API()))
	if err != nil {
		t.Fatal(err)
	}
	comp := jsonschema.NewCompiler()
	comp.AssertFormat()
	if err := comp.AddResource(schema.APIID, doc); err != nil {
		t.Fatal(err)
	}
	sch, err := comp.Compile(schema.APIID + "#/$defs/" + def)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("%s does not validate: %v\n%s", def, err, b)
	}
}

func TestNoLockAcrossBackendCalls(t *testing.T) {
	nb := newNodeBackend()
	h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
	h.c.ConfigApplied(mfpConfig(t, "ippusb-mfp"), "e")
	call := func() {
		h.c.Current()
		h.c.Setup()
		if _, err := h.c.AgentReport(context.Background(), device.AgentReport{Inventory: catalog.Fixture("ippusb-mfp")}); err != nil {
			t.Error(err)
		}
	}
	h.b.onApply, h.b.onObserve, nb.onNodes = call, call, call
	done := make(chan struct{})
	go func() {
		h.c.step(context.Background())
		h.c.step(context.Background())
		_ = h.c.Detect(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop blocked: a lock is held across a backend call")
	}
	wantApplies(t, h.b, 1)
}

func TestInitialStatus(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	got := h.c.Current()
	want := apitypes.Status{
		Config:   apitypes.StatusConfig{State: "missing", Errors: []config.FieldError{}},
		Phase:    "none",
		Printers: []apitypes.StatusPrinter{}, Scanners: []apitypes.StatusScanner{},
		Workloads: []apitypes.StatusWorkload{}, Devices: []apitypes.DetectedDevice{},
		Reconcile: []apitypes.ReconcileItem{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Current() =\n%+v\nwant\n%+v", got, want)
	}
	b, _ := json.Marshal(got)
	for _, bad := range []string{`"state":""`, `"phase":""`, `null`} {
		if bytes.Contains(b, []byte(bad)) {
			t.Errorf("initial Status contains %s: %s", bad, b)
		}
	}
	validateStatus(t, got)
	wantApplies(t, h.b, 0)
}

func TestConfigEffective(t *testing.T) {
	h := newHarness(t, Options{})
	if got := h.c.Config(); !reflect.DeepEqual(got, config.Default("Printer")) {
		t.Fatalf("before input: %+v", got)
	}
	h.c.ConfigApplied(cfgOf(nil), "e1")
	if got := h.c.Config(); got.Name != "Home" {
		t.Fatalf("after apply: %q", got.Name)
	}
	h.c.ConfigInvalid(errors.New("bad"), "e3", nil)
	if got := h.c.Config(); got.Name != "Printer" {
		t.Fatalf("after invalid without last-good: %q", got.Name)
	}
}

func TestOverallPhase(t *testing.T) {
	t.Run("no config with usb: probing", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true})
		h.c.ConfigMissing()
		s := h.step()
		if sp := h.b.one(t); sp.Mode != device.ModeProbe || sp.Node != "" || sp.Hash != device.SpecHash(sp) {
			t.Fatalf("spec %+v", sp)
		}
		wantPhase(t, s, "probing", msgWaitingAgent)
		if len(s.Workloads) != 1 || s.Workloads[0].Mode != "probe" {
			t.Fatalf("workloads %+v", s.Workloads)
		}
		validateStatus(t, s)
	})
	t.Run("no config, no usb: none", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.c.ConfigMissing()
		s := h.step()
		h.step()
		if a := h.b.applies(); len(a) != 1 || len(a[0]) != 0 {
			t.Fatalf("applies %v, want one empty Apply", a)
		}
		wantPhase(t, s, "none", "")
	})
	t.Run("no config, probe failed: error", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true})
		h.c.ConfigMissing()
		h.step()
		h.b.set(device.Observation{Phase: device.PhaseFailed, Message: "probe: CrashLoopBackOff"})
		wantPhase(t, h.step(), "error", "probe: CrashLoopBackOff")
	})
	t.Run("no config, unschedulable probe", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
		h.c.ConfigMissing()
		h.step()
		h.b.set(device.Observation{Phase: device.PhasePending, Unschedulable: true, Message: "0/3 nodes"})
		wantPhase(t, h.step(), "probing", msgUnschedulable)
	})
	t.Run("every entry disabled: none", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true})
		cfg := mfpConfig(t, "ippusb-mfp")
		cfg.Printers[0].Enabled, cfg.Scanners[0].Enabled = false, false
		h.c.ConfigApplied(cfg, "e")
		h.report(t, "ippusb-mfp")
		s := h.step()
		wantPhase(t, s, "none", msgNothingInUse)
		wantEntry(t, s.Printers[0].Phase, s.Printers[0].Message, "none", "")
		if sp := h.b.one(t); sp.Mode != device.ModeProbe {
			t.Fatalf("spec %+v, want the probe", sp)
		}
	})
	t.Run("unplugged device: deploying", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true})
		cfg := mfpConfig(t, "ippusb-mfp")
		h.c.ConfigApplied(cfg, "e")
		h.report(t, "no-printer")
		h.b.set(running())
		s := h.step()
		wantEntry(t, s.Printers[0].Phase, s.Printers[0].Message, "probing", msgNotPlugged)
		wantPhase(t, s, "deploying", "printer: "+msgNotPlugged)
		if sp := h.b.one(t); sp.Mode != device.ModeProbe {
			t.Fatalf("spec %+v", sp)
		}
	})
}

func TestIPPUSBReachesReady(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.c.ConfigApplied(mfpConfig(t, "ippusb-mfp"), "e1")
	h.report(t, "ippusb-mfp")
	h.b.set(running(device.Endpoint{ID: "printer", PrinterURI: "ipp://127.0.0.1:4000/ipp/print"}, device.Endpoint{ID: "scanner", ScannerURL: "http://127.0.0.1:4001/eSCL"}))
	s := h.step()
	sp := h.b.one(t)
	want := []device.PrinterSpec{{ID: "printer", Family: "ipp-usb", USB: dev(t, "ippusb-mfp", "03f0:2754").Key()}}
	if sp.Mode != device.ModeUSB || !reflect.DeepEqual(sp.Printers, want) || len(sp.Scanners) != 1 || sp.Scanners[0].Family != "ipp-usb" {
		t.Fatalf("spec %+v", sp)
	}
	wantPhase(t, s, "ready", "")
	p, sc := s.Printers[0], s.Scanners[0]
	if !p.Available || p.URI != "ipp://127.0.0.1:4000/ipp/print" || p.Decision == nil || p.Decision.Rule != "ipp-over-usb" || p.Device == nil {
		t.Fatalf("printer %+v", p)
	}
	if !sc.Available || sc.URL != "http://127.0.0.1:4001/eSCL" || !reflect.DeepEqual(sc.Connect.ESCLURLs, []string{sc.URL}) {
		t.Fatalf("scanner %+v", sc)
	}
	if len(h.pubs) == 0 || h.pubs[len(h.pubs)-1].Phase != "ready" {
		t.Fatal("ready not published")
	}
	validateStatus(t, s)
}

// officeServices are the browsed services of the office node: ipp-usb's
// HP (IPP and eSCL on one port) and AirSane's Samsung and LiDE.
func officeServices() []device.DNSSDService {
	return []device.DNSSDService{
		{Type: device.ServiceIPP, Name: "HP OfficeJet Pro 8020 [USB]", Port: 60000, TXT: map[string]string{"rp": "ipp/print", "usb_MFG": "HP", "usb_MDL": "OfficeJet Pro 8020 series"}},
		{Type: device.ServiceUScan, Name: "HP OfficeJet Pro 8020 [USB] scanner", Port: 60000, TXT: map[string]string{"rs": "eSCL"}},
		{Type: device.ServiceUScan, Name: "Samsung SCX-4300", Port: 8090, TXT: map[string]string{"rs": "eSCL/samsung", "ty": "SCX-4300 Series"}},
		{Type: device.ServiceUScan, Name: "CanoScan", Port: 8090, TXT: map[string]string{"rs": "eSCL/lide", "ty": "CanoScan LiDE 100"}},
		{Type: device.ServiceIPP, Name: "samsung", Port: 8005, TXT: map[string]string{"rp": "ipp/print/samsung"}},
	}
}

func officeConfig(t *testing.T) *config.Config {
	t.Helper()
	hp, sam := dev(t, "office", "03f0:2754"), dev(t, "office", "04e8:342e")
	return cfgOf([]config.Printer{usbPrinter("hp", hp), usbPrinter("samsung", sam)}, usbScanner("hp-scan", hp), usbScanner("samsung-scan", sam))
}

func samsungQueue(r *device.AgentReport) {
	r.Queues = []device.QueueResult{{ID: "samsung", State: device.QueueOK, PrinterURI: "ipp://127.0.0.1:8005/ipp/print/samsung"}}
}

func TestTwoPrintersTwoScanners(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
	h.c.ConfigApplied(officeConfig(t), "e")
	h.b.set(device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20"})
	withServices := func(r *device.AgentReport) { r.Services = officeServices() }
	h.report(t, "office", withServices, samsungQueue)
	s := h.step()
	sp := h.b.one(t)
	if len(sp.Printers) != 2 || sp.Printers[0].Family != "ipp-usb" || sp.Printers[1].Family != "legacy" ||
		len(sp.Scanners) != 2 || sp.Scanners[0].Family != "ipp-usb" || sp.Scanners[1].Family != "airsane" {
		t.Fatalf("spec %+v", sp)
	}
	want := map[string]string{
		"hp":           "ipp://192.168.1.20:60000/ipp/print",
		"samsung":      "ipp://192.168.1.20:8005/ipp/print/samsung",
		"hp-scan":      "http://192.168.1.20:60000/eSCL",
		"samsung-scan": "http://192.168.1.20:8090/eSCL/samsung",
	}
	names := map[string]string{"hp": "HP OfficeJet Pro 8020 [USB]", "samsung": "samsung", "hp-scan": "HP OfficeJet Pro 8020 [USB] scanner", "samsung-scan": "Samsung SCX-4300"}
	for _, p := range s.Printers {
		if p.URI != want[p.ID] || !reflect.DeepEqual(p.Connect.IPPURIs, []string{want[p.ID]}) || p.Connect.DNSSDName != names[p.ID] {
			t.Errorf("printer %s: uri %q connect %+v", p.ID, p.URI, p.Connect)
		}
		wantEntry(t, p.Phase, p.Message, "ready", "")
	}
	for _, sc := range s.Scanners {
		if sc.URL != want[sc.ID] || !reflect.DeepEqual(sc.Connect.ESCLURLs, []string{want[sc.ID]}) || sc.Connect.DNSSDName != names[sc.ID] {
			t.Errorf("scanner %s: url %q connect %+v", sc.ID, sc.URL, sc.Connect)
		}
	}
	wantPhase(t, s, "ready", "")
	validateStatus(t, s)
	if len(s.Devices) != 4 {
		t.Fatalf("devices %+v", s.Devices)
	}

	// One queue error turns only that printer to error.
	h.report(t, "office", withServices, func(r *device.AgentReport) {
		r.Queues = []device.QueueResult{{ID: "samsung", State: device.QueueError, Message: "create queue: bad driver"}}
	})
	s = h.step()
	wantEntry(t, printerStatus(t, s, "samsung").Phase, printerStatus(t, s, "samsung").Message, "error", "create queue: bad driver")
	wantEntry(t, printerStatus(t, s, "hp").Phase, "", "ready", "*")
	wantEntry(t, scannerStatus(t, s, "samsung-scan").Phase, "", "ready", "*")
	wantPhase(t, s, "error", "samsung: create queue: bad driver")
}

func TestTwoNodes(t *testing.T) {
	nb := newNodeBackend("n1", "n2")
	h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
	hp, sam := dev(t, "ippusb-mfp", "03f0:2754"), dev(t, "samsung-scx4300-mfp", "04e8:342e")
	h.c.ConfigApplied(cfgOf([]config.Printer{usbPrinter("hp", hp), usbPrinter("samsung", sam)}), "e")
	h.reportOn(t, "n1", "ippusb-mfp")
	h.reportOn(t, "n2", "samsung-scx4300-mfp")
	h.step()
	specs := h.b.last(t)
	if len(specs) != 2 || specs[0].Node != "n1" || specs[1].Node != "n2" ||
		specs[0].Printers[0].ID != "hp" || specs[1].Printers[0].ID != "samsung" {
		t.Fatalf("specs %+v", specs)
	}
	if ds := h.reportOn(t, "n2", "samsung-scx4300-mfp"); ds.Hash != specs[1].Hash || len(ds.Queues) != 1 || ds.Queues[0].ID != "samsung" {
		t.Fatalf("n2 desired state %+v", ds)
	}
	if ds := h.reportOn(t, "n1", "ippusb-mfp"); ds.Hash != specs[0].Hash || len(ds.Queues) != 0 {
		t.Fatalf("n1 desired state %+v", ds)
	}

	t.Run("lister error: single-node", func(t *testing.T) {
		nb := newNodeBackend()
		nb.nodesErr = fmt.Errorf("%w: nodes is forbidden", device.ErrSingleNode)
		h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
		h.c.ConfigApplied(cfgOf([]config.Printer{usbPrinter("hp", hp)}), "e")
		h.report(t, "ippusb-mfp")
		h.step()
		if sp := h.b.one(t); sp.Node != "" || len(sp.Printers) != 1 {
			t.Fatalf("spec %+v", sp)
		}
	})
	t.Run("transient lister error: keep the set, no Apply", func(t *testing.T) {
		nb := newNodeBackend("n1", "n2")
		h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
		h.c.ConfigApplied(cfgOf([]config.Printer{usbPrinter("hp", hp), usbPrinter("samsung", sam)}), "e")
		h.reportOn(t, "n1", "ippusb-mfp")
		h.reportOn(t, "n2", "samsung-scx4300-mfp")
		h.step()
		before := len(h.b.applies())
		nb.mu.Lock()
		nb.nodesErr = errors.New("list nodes: etcd timeout")
		nb.mu.Unlock()
		h.c.ConfigApplied(cfgOf([]config.Printer{usbPrinter("hp", hp)}), "e2") // a change that would Apply
		h.clock.add(resync)
		s := h.step()
		if n := len(h.b.applies()); n != before {
			t.Fatalf("%d Applies during a transient node-list error, want %d: %+v", n, before, h.b.last(t))
		}
		if ws := workloadNodes(s); !slices.Equal(ws, []string{"n1", "n2"}) {
			t.Errorf("workloads %v, want the previous set", ws)
		}
		i := slices.IndexFunc(s.Reconcile, func(r apitypes.ReconcileItem) bool { return r.Item == "nodes" })
		if i < 0 || s.Reconcile[i].OK || !strings.Contains(s.Reconcile[i].Message, "listing nodes failed: list nodes: etcd timeout") {
			t.Errorf("reconcile %+v, want a failed nodes item during the blip", s.Reconcile)
		}
		nb.mu.Lock()
		nb.nodesErr = nil
		nb.mu.Unlock()
		s = h.step()
		if i := slices.IndexFunc(s.Reconcile, func(r apitypes.ReconcileItem) bool { return r.Item == "nodes" }); i < 0 || !s.Reconcile[i].OK {
			t.Errorf("reconcile %+v, want the nodes item cleared after the blip", s.Reconcile)
		}
		if specs := h.b.last(t); len(specs) != 2 || specs[0].Node != "n1" || len(specs[1].Printers) != 0 {
			t.Errorf("after the blip specs %+v", specs)
		}
	})
	t.Run("transient lister error at cold start: reports kept", func(t *testing.T) {
		nb := newNodeBackend("n1", "n2")
		nb.nodesErr = errors.New("list nodes: etcd timeout")
		h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
		h.c.ConfigApplied(cfgOf([]config.Printer{usbPrinter("hp", hp), usbPrinter("samsung", sam)}), "e")
		h.reportOn(t, "n1", "ippusb-mfp")
		h.reportOn(t, "n2", "samsung-scx4300-mfp")
		h.step()
		h.step()
		wantApplies(t, h.b, 0)
		nb.mu.Lock()
		nb.nodesErr = nil
		nb.mu.Unlock()
		s := h.step()
		if i := slices.IndexFunc(s.Reconcile, func(r apitypes.ReconcileItem) bool { return r.Item == "nodes" }); i < 0 || !s.Reconcile[i].OK {
			t.Errorf("reconcile %+v, want the nodes item cleared after the blip", s.Reconcile)
		}
		for _, sp := range h.b.last(t) {
			if sp.Mode == device.ModeProbe || len(sp.Printers) != 1 {
				t.Fatalf("spec %+v, want the node's printer", sp)
			}
		}
	})
	t.Run("empty node list: one node-less probe", func(t *testing.T) {
		nb := newNodeBackend()
		h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
		h.c.ConfigMissing()
		h.step()
		if sp := h.b.one(t); sp.Node != "" || sp.Mode != device.ModeProbe {
			t.Fatalf("spec %+v", sp)
		}
	})
	t.Run("no config: a probe per node", func(t *testing.T) {
		nb := newNodeBackend("n1", "n2")
		h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
		h.c.ConfigMissing()
		h.step()
		specs := h.b.last(t)
		if len(specs) != 2 || specs[0].Node != "n1" || specs[1].Node != "n2" || specs[0].Mode != device.ModeProbe || specs[1].Mode != device.ModeProbe {
			t.Fatalf("specs %+v", specs)
		}
	})
}

func TestStandaloneScanner(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	lide := dev(t, "canon-lide-scanner", "04a9:1904")
	h.c.ConfigApplied(cfgOf(nil, usbScanner("lide", lide)), "e")
	h.report(t, "canon-lide-scanner")
	h.step()
	sp := h.b.one(t)
	if sp.Mode != device.ModeUSB || len(sp.Printers) != 0 || len(sp.Scanners) != 1 ||
		sp.Scanners[0].Family != "airsane" || !slices.Equal(sp.Scanners[0].SaneBackends, []string{"genesys"}) {
		t.Fatalf("spec %+v", sp)
	}
}

func TestDisabledEntries(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	cfg := mfpConfig(t, "ippusb-mfp")
	cfg.Scanners[0].Enabled = false
	h.c.ConfigApplied(cfg, "e")
	h.report(t, "ippusb-mfp")
	s := h.step()
	if sp := h.b.one(t); len(sp.Printers) != 1 || len(sp.Scanners) != 0 {
		t.Fatalf("spec %+v", sp)
	}
	sc := s.Scanners[0]
	if sc.Enabled || sc.Phase != "none" || sc.Decision != nil || sc.Device != nil {
		t.Fatalf("scanner %+v", sc)
	}
}

func TestSimulatedJoinFirstWorkload(t *testing.T) {
	nb := newNodeBackend("n1", "n2")
	h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
	sam := dev(t, "samsung-scx4300-mfp", "04e8:342e")
	cfg := cfgOf([]config.Printer{usbPrinter("samsung", sam), simPrinter("sim-b"), simPrinter("sim-a")}, simScanner("sim-scan"))
	h.c.ConfigApplied(cfg, "e")
	h.reportOn(t, "n1", "none")
	h.reportOn(t, "n2", "samsung-scx4300-mfp")
	h.step()
	specs := h.b.last(t)
	if len(specs) != 2 || specs[0].Node != "n1" || specs[0].Mode != device.ModeSimulated {
		t.Fatalf("specs %+v", specs)
	}
	want := []device.PrinterSpec{
		{ID: "sim-a", Family: "sim", Name: "sim-a", Port: 8631},
		{ID: "sim-b", Family: "sim", Name: "sim-b", Port: 8632},
	}
	if !reflect.DeepEqual(specs[0].Printers, want) || len(specs[0].Scanners) != 1 || specs[0].Scanners[0].Family != "sim" {
		t.Fatalf("n1 spec %+v", specs[0])
	}
	if specs[1].Mode != device.ModeUSB || len(specs[1].Printers) != 1 {
		t.Fatalf("n2 spec %+v", specs[1])
	}

	single := newHarness(t, Options{})
	single.c.ConfigApplied(simConfig(), "e")
	single.step()
	if sp := single.b.one(t); sp.Node != "" || sp.Mode != device.ModeSimulated || len(sp.Printers) != 1 || len(sp.Scanners) != 1 {
		t.Fatalf("sim spec %+v", sp)
	}
}

// samsungCaps are the Samsung queue's live attributes (section 0 of the plan).
var samsungCaps = device.PrinterCaps{
	Media: []string{"iso_a4_210x297mm", "na_letter_8.5x11in"}, MediaDefault: "na_letter_8.5x11in",
	Sides: []string{"one-sided"}, SidesDefault: "one-sided",
	ColorModes: []string{"monochrome"}, ColorModeDefault: "monochrome",
	Qualities: []string{"draft", "normal", "high"}, QualityDefault: "normal",
	Resolutions: []int{300, 600}, ResolutionDefault: 600, CopiesMax: 999,
}

func TestOptionWarnings(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	cfg := mfpConfig(t, "samsung-scx4300-mfp")
	cfg.Printers[0].Defaults = config.PrintDefaults{ColorMode: "auto", MediaSource: "tray-1"}
	cfg.Scanners[0].Defaults = config.ScanDefaults{Intent: "photo", Source: "platen"}
	h.c.ConfigApplied(cfg, "e")
	const uri = "ipp://127.0.0.1:8005/ipp/print/printer"
	h.attrs[uri] = printing.Attributes{State: "idle", Caps: samsungCaps}
	h.scanCaps["http://127.0.0.1:8090/eSCL"] = scanning.Caps{
		Sources: []string{"platen", "adf"}, Resolutions: []int{75, 150, 300, 600}, ColorModes: []string{"color", "grayscale"}, ADF: true,
	}
	h.b.set(running(device.Endpoint{ID: "scanner", ScannerURL: "http://127.0.0.1:8090/eSCL"}))
	h.report(t, "samsung-scx4300-mfp", func(r *device.AgentReport) {
		r.Queues = []device.QueueResult{{
			ID: "printer", State: device.QueueOK, PrinterURI: uri, Warnings: []string{"Could not set the printer's defaults: x"},
			Refused: []device.Dropped{{Option: "quality", Value: "high", Using: "normal"}},
		}}
	})
	s := h.step()
	p := s.Printers[0]
	wantEntry(t, p.Phase, p.Message, "ready", "")
	want := []apitypes.Warning{
		{Option: "mediaSource", Value: "tray-1", Using: "", Message: "mediaSource tray-1 is not offered by this printer; it uses its own default"},
		{Option: "colorMode", Value: "auto", Using: "monochrome", Message: "colorMode auto is not offered by this printer; it uses monochrome"},
		{Option: "quality", Value: "high", Using: "normal", Message: "quality high could not be set as the printer's default; it uses normal"},
		{Option: "other", Message: "Could not set the printer's defaults: x"},
	}
	if !reflect.DeepEqual(p.Warnings, want) {
		t.Fatalf("printer warnings %+v", p.Warnings)
	}
	if !reflect.DeepEqual(p.Capabilities.ColorModes, []string{"monochrome"}) || p.Capabilities.MediaSources == nil {
		t.Fatalf("capabilities %+v", p.Capabilities)
	}
	sc := s.Scanners[0]
	if len(sc.Warnings) != 1 || sc.Warnings[0].Option != "intent" || sc.Warnings[0].Value != "photo" || sc.Warnings[0].Using != "" {
		t.Fatalf("scanner warnings %+v", sc.Warnings)
	}
	for _, r := range s.Reconcile {
		if r.Item != "workload/default" {
			t.Fatalf("unexpected reconcile item %+v", r)
		}
	}
	validateStatus(t, s)
}

func TestDesiredStateQueue(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	cfg := mfpConfig(t, "hp-legacy-mfp")
	cfg.Printers[0].Name, cfg.Printers[0].Location = "HP", "Office"
	cfg.Printers[0].Defaults = config.PrintDefaults{
		Media: "na_letter_8.5x11in", MediaSource: "manual", Sides: "two-sided-long-edge", ColorMode: "monochrome",
		Quality: "high", Resolution: 600, ContentOptimize: "text", Orientation: "landscape", NumberUp: 2,
		PrintScaling: "fit", OutputBin: "face-down", Finishings: "staple",
	}
	h.c.ConfigApplied(cfg, "e")
	if ds := h.report(t, "hp-legacy-mfp"); ds.Hash != "" || len(ds.Queues) != 0 {
		t.Fatalf("before any spec: %+v", ds)
	}
	h.step()
	sp := h.b.one(t)
	ds := h.report(t, "hp-legacy-mfp")
	if ds.Hash != sp.Hash || len(ds.Queues) != 1 {
		t.Fatalf("desired %+v", ds)
	}
	q := ds.Queues[0]
	hp := catalog.Fixture("hp-legacy-mfp").Devices[0]
	want := device.QueueSpec{
		ID: "printer", Name: "HP", Location: "Office", Driver: hpDriver, Schemes: []string{"hp", "usb"},
		SystemURI: device.PAPPLSystemURI(8002), Device: hp, Defaults: device.PrintSettings(cfg.Printers[0].Defaults),
	}
	if !reflect.DeepEqual(q, want) {
		t.Fatalf("queue\n%+v\nwant\n%+v", q, want)
	}
	if q.Defaults.Finishings != "staple" || q.Defaults.NumberUp != 2 || q.Device.DeviceID == "" {
		t.Fatalf("queue defaults %+v", q.Defaults)
	}
	// Queue settings never roll the pod.
	loc := mfpConfig(t, "hp-legacy-mfp")
	loc.Printers[0].Location = "Hall"
	h.c.ConfigApplied(loc, "e2")
	h.step()
	if ds := h.report(t, "hp-legacy-mfp"); ds.Queues[0].Location != "Hall" || ds.Hash != sp.Hash {
		t.Fatalf("desired %+v", ds)
	}
	wantApplies(t, h.b, 1)
}

func TestQueueDeviceIDFromCache(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
	h.b.set(device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20"})
	sam := dev(t, "samsung-scx4300-mfp", "04e8:342e")
	h.c.ConfigApplied(cfgOf([]config.Printer{usbPrinter("samsung", sam)}), "e")
	h.report(t, "samsung-scx4300-mfp")
	h.step()
	// A new pod started while the printer was off; it is back, attached only.
	live := sam
	live.DeviceID, live.Address = "", 12
	ds := h.report(t, "none", func(r *device.AgentReport) {
		r.PodUID, r.Attached, r.ProbeNodes = "pod-2", []device.USBDevice{live}, []string{}
	})
	if len(ds.Queues) != 1 || ds.Queues[0].Device.DeviceID != sam.DeviceID || ds.Queues[0].Device.Address != 12 {
		t.Fatalf("queues %+v, want the cached device ID at the new address", ds.Queues)
	}
	h.step()
	wantApplies(t, h.b, 1)
}

func TestQueueDriverName(t *testing.T) {
	queue := func(t *testing.T, cfg *config.Config, fixture string) (device.QueueSpec, apitypes.Status) {
		t.Helper()
		h := newHarness(t, Options{USBAvailable: true})
		h.c.ConfigApplied(cfg, "e")
		h.report(t, fixture)
		h.b.set(running())
		s := h.step()
		ds := h.report(t, fixture)
		if len(ds.Queues) != 1 {
			t.Fatalf("queues %+v", ds.Queues)
		}
		return ds.Queues[0], s
	}
	t.Run("legacy model match", func(t *testing.T) {
		q, _ := queue(t, mfpConfig(t, "samsung-scx4300-mfp"), "samsung-scx4300-mfp")
		if q.Driver != "samsung--scx-4300--2-0-0-en" || q.DriverPinned || q.SystemURI != device.PAPPLSystemURI(8005) {
			t.Fatalf("queue %+v", q)
		}
	})
	t.Run("driverName pins the driver", func(t *testing.T) {
		cfg := mfpConfig(t, "samsung-scx4300-mfp")
		cfg.Printers[0].DriverName = "samsung--scx-4300--2-0-0-en"
		if q, _ := queue(t, cfg, "samsung-scx4300-mfp"); q.Driver != cfg.Printers[0].DriverName || !q.DriverPinned {
			t.Fatalf("queue %+v", q)
		}
	})
	t.Run("driverName no app lists", func(t *testing.T) {
		cfg := mfpConfig(t, "samsung-scx4300-mfp")
		cfg.Printers[0].DriverName = "acme--x-en"
		q, s := queue(t, cfg, "samsung-scx4300-mfp")
		if q.Driver != "samsung--scx-4300--2-0-0-en" || q.DriverPinned {
			t.Fatalf("queue %+v", q)
		}
		p := s.Printers[0]
		wantEntry(t, p.Phase, p.Message, "degraded", fmt.Sprintf(msgDriverNotListed, "acme--x-en"))
	})
	t.Run("family override without a match", func(t *testing.T) {
		cfg := mfpConfig(t, "unknown-printer")
		cfg.Printers[0].Driver = "gutenprint"
		if q, _ := queue(t, cfg, "unknown-printer"); q.Driver != config.Auto || q.SystemURI != device.PAPPLSystemURI(8003) {
			t.Fatalf("queue %+v", q)
		}
	})
}

const hpDriver = "hp--laserjet-professional-m-1132-mfp--requires-proprietary-plugin-en"

func TestNoDriver(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.c.ConfigApplied(mfpConfig(t, "unknown-printer"), "e")
	h.report(t, "unknown-printer")
	h.b.set(running())
	s := h.step()
	sp := h.b.one(t)
	if sp.Mode != device.ModeUSB || len(sp.Printers) != 0 || len(sp.Scanners) != 1 || sp.Scanners[0].Family != "airsane" {
		t.Fatalf("spec %+v, want the scanner only", sp)
	}
	p := s.Printers[0]
	wantEntry(t, p.Phase, p.Message, "degraded", msgNoDriver)
	if p.Decision == nil || p.Decision.Rule != device.RuleNone {
		t.Fatalf("decision %+v", p.Decision)
	}
	if ds := h.report(t, "unknown-printer"); len(ds.Queues) != 0 {
		t.Fatalf("queues %+v, want none", ds.Queues)
	}
}

func TestStickyPrinterOff(t *testing.T) {
	rb := &restartBackend{fakeBackend: &fakeBackend{}}
	h := newHarness(t, Options{Backend: rb, BackendName: BackendK8s, USBAvailable: true})
	h.b.set(device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20"})
	h.c.ConfigApplied(mfpConfig(t, "samsung-scx4300-mfp"), "e")
	sam := catalog.Fixture("samsung-scx4300-mfp").Devices[0]
	healthy := func(r *device.AgentReport) {
		r.Attached, r.ProbeNodes = []device.USBDevice{sam}, []string{sam.Node()}
	}
	h.report(t, "samsung-scx4300-mfp", healthy)
	h.step()
	first := h.b.one(t)
	off := func(r *device.AgentReport) { r.Attached, r.ProbeNodes = []device.USBDevice{}, []string{sam.Node()} }
	for range 3 {
		h.clock.add(20 * time.Second)
		h.report(t, "samsung-scx4300-mfp", off)
		h.step()
	}
	// The pod restarts while the printer is off: its probe finds nothing.
	h.report(t, "none", off, func(r *device.AgentReport) { r.PodUID, r.ProbeNodes = "pod-2", []string{} })
	h.step()
	h.clock.add(time.Minute)
	ds := h.report(t, "none", off, func(r *device.AgentReport) { r.PodUID, r.ProbeNodes = "pod-2", []string{} })
	h.step()
	wantApplies(t, h.b, 1)
	if ds.Hash != first.Hash || len(ds.Queues) != 1 {
		t.Fatalf("desired %+v, want the sticky printer's queue", ds)
	}
	if rb.count() != 0 {
		t.Fatalf("%d restarts for a switched-off printer", rb.count())
	}
}

// TestStickySurvivesInvalidConfig: an invalid file without last-good keeps
// the sticky placement, so a printer that is off when the file turns valid
// again does not roll the pod.
func TestStickySurvivesInvalidConfig(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
	h.b.set(device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20"})
	cfg := mfpConfig(t, "samsung-scx4300-mfp")
	h.c.ConfigApplied(cfg, "e")
	h.report(t, "samsung-scx4300-mfp")
	h.step()
	first := h.b.one(t)
	h.c.ConfigInvalid(errors.New("bad"), "e2", nil)
	h.step()
	h.report(t, "none", func(r *device.AgentReport) { r.PodUID = "pod-2" })
	h.c.ConfigApplied(cfg, "e3")
	h.step()
	wantApplies(t, h.b, 1)
	if sp := h.b.one(t); sp.Hash != first.Hash {
		t.Fatalf("spec %+v, want the sticky one", sp)
	}
}

// replug runs the replug restart cases per device on k8s.
type replug struct {
	t  *testing.T
	h  *harness
	rb *restartBackend
	p  device.USBDevice
}

func newReplug(t *testing.T, backend string) *replug {
	rb := &restartBackend{fakeBackend: &fakeBackend{}}
	h := newHarness(t, Options{Backend: rb, BackendName: backend, USBAvailable: true})
	h.b.set(device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20"})
	h.c.ConfigApplied(mfpConfig(t, "ippusb-mfp"), "e")
	return &replug{t: t, h: h, rb: rb, p: catalog.Fixture("ippusb-mfp").Devices[0]}
}

// at moves p to address a in Attached; probeNodes nil keeps p's node.
func (r *replug) send(pod string, invAddr, attAddr int, probeNodes []string) {
	r.t.Helper()
	r.h.report(r.t, "ippusb-mfp", func(rep *device.AgentReport) {
		rep.PodUID = pod
		inv := r.p
		inv.Address = invAddr
		rep.Inventory.Devices = []device.USBDevice{inv}
		att := r.p
		att.Address, att.DeviceID = attAddr, ""
		rep.Attached = []device.USBDevice{att}
		rep.ProbeNodes = probeNodes
		if rep.ProbeNodes == nil {
			rep.ProbeNodes = []string{inv.Node()}
		}
	})
	r.h.step()
}

func (r *replug) want(n int) {
	r.t.Helper()
	if got := r.rb.count(); got != n {
		r.t.Fatalf("%d restarts, want %d", got, n)
	}
}

func TestReplugMovedRestartsOnce(t *testing.T) {
	r := newReplug(t, BackendK8s)
	r.send("pod", 4, 4, nil)
	r.send("pod", 4, 5, nil)
	r.h.clock.add(20 * time.Second)
	r.send("pod", 4, 5, nil)
	r.want(0)
	r.h.clock.add(10 * time.Second)
	r.send("pod", 4, 5, nil)
	r.want(1)
	for range 3 {
		r.h.clock.add(20 * time.Second)
		r.send("pod", 4, 5, nil)
	}
	r.want(1)
	s := r.h.c.Current()
	wantEntry(t, s.Printers[0].Phase, s.Printers[0].Message, "degraded", msgUSBStale)
	wantEntry(t, s.Scanners[0].Phase, s.Scanners[0].Message, "degraded", msgUSBStale)
}

func TestUnallocatedNodeRestartsOnce(t *testing.T) {
	r := newReplug(t, BackendK8s)
	r.send("pod", 4, 4, []string{"001/099"})
	r.h.clock.add(resync)
	r.send("pod", 4, 4, []string{"001/099"})
	r.want(1)
	for range 4 {
		r.h.clock.add(30 * time.Second)
		r.send("pod-2", 4, 4, []string{"001/099"})
	}
	r.want(1)
	s := r.h.c.Current()
	wantEntry(t, s.Printers[0].Phase, s.Printers[0].Message, "degraded", msgUSBStale)
}

func TestReplugTwiceRestartsTwice(t *testing.T) {
	r := newReplug(t, BackendK8s)
	r.send("pod", 4, 5, nil)
	r.h.clock.add(resync)
	r.send("pod", 4, 5, nil)
	r.want(1)
	r.send("pod-2", 5, 5, nil) // the new pod holds the new node: the latch resets
	r.send("pod-2", 5, 6, nil)
	r.h.clock.add(resync)
	r.send("pod-2", 5, 6, nil)
	r.want(2)
}

func TestReplugFlapNoRestart(t *testing.T) {
	r := newReplug(t, BackendK8s)
	r.send("pod", 4, 5, nil)
	r.h.clock.add(20 * time.Second)
	r.send("pod", 4, 4, nil)
	r.send("pod", 4, 5, nil)
	r.h.clock.add(20 * time.Second)
	r.send("pod", 4, 5, nil)
	r.want(0)
}

func TestTwoDevicesOneRestart(t *testing.T) {
	rb := &restartBackend{fakeBackend: &fakeBackend{}}
	h := newHarness(t, Options{Backend: rb, BackendName: BackendK8s, USBAvailable: true})
	h.b.set(device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20"})
	h.c.ConfigApplied(officeConfig(t), "e")
	office := catalog.Fixture("office").Devices
	moved := func(r *device.AgentReport) {
		for _, d := range office {
			d.DeviceID = ""
			if d.VendorID == "03f0" || d.VendorID == "04e8" {
				d.Address += 20
			}
			r.Attached = append(r.Attached, d)
		}
		for _, d := range office {
			r.ProbeNodes = append(r.ProbeNodes, d.Node())
		}
	}
	h.report(t, "office", moved)
	h.step()
	h.clock.add(resync)
	h.report(t, "office", moved)
	h.step()
	if !slices.Equal(rb.restarts, []string{""}) {
		t.Fatalf("restarts %v, want one of the node-less workload", rb.restarts)
	}
}

func TestOldAgentNoRestarts(t *testing.T) {
	r := newReplug(t, BackendK8s)
	for range 3 {
		r.h.report(t, "ippusb-mfp")
		r.h.step()
		r.h.clock.add(resync)
	}
	r.want(0)
}

// A device that appears without a known device ID restarts the pod once,
// and the new pod's report adds it to the spec.
func TestAppearedWithoutDeviceID(t *testing.T) {
	for _, backend := range []string{BackendK8s, BackendDocker} {
		t.Run(backend, func(t *testing.T) {
			r := newReplug(t, backend)
			att := r.p
			att.DeviceID = ""
			appeared := func(rep *device.AgentReport) { rep.Attached, rep.ProbeNodes = []device.USBDevice{att}, []string{} }
			r.h.report(t, "none", appeared)
			s := r.h.step()
			if sp := r.h.b.one(t); len(sp.Printers) != 0 || len(sp.Scanners) != 1 {
				t.Fatalf("spec %+v, want the scanner only until the printer's device ID is known", sp)
			}
			wantEntry(t, s.Printers[0].Phase, s.Printers[0].Message, "deploying", msgNewDevice)
			r.h.clock.add(resync)
			r.h.report(t, "none", appeared)
			r.h.step()
			r.want(1)
			r.h.clock.add(resync)
			r.h.report(t, "none", appeared)
			r.h.step()
			r.want(1)
			r.send("pod-2", 4, 4, nil)
			if sp := r.h.b.one(t); len(sp.Printers) != 1 || len(sp.Scanners) != 1 {
				t.Fatalf("spec %+v, want the printer and scanner", sp)
			}
			r.want(1)
		})
	}
}

// k8s: a sticky entry whose pod started while the device was off, with the
// device ID cached: when the device attaches, one restart after 30 s.
func TestAppearedStickyRestartsOnce(t *testing.T) {
	r := newReplug(t, BackendK8s)
	r.send("pod", 4, 4, nil)
	first := r.h.b.one(t)
	off := func(rep *device.AgentReport) {
		rep.PodUID, rep.Attached, rep.ProbeNodes = "pod-2", []device.USBDevice{}, []string{}
	}
	r.h.report(t, "none", off)
	r.h.step()
	att := r.p
	att.DeviceID, att.Address = "", 7
	on := func(rep *device.AgentReport) {
		rep.PodUID, rep.Attached, rep.ProbeNodes = "pod-2", []device.USBDevice{att}, []string{}
	}
	r.h.report(t, "none", on)
	r.h.step()
	r.want(0)
	r.h.clock.add(resync)
	r.h.report(t, "none", on)
	r.h.step()
	r.want(1)
	if sp := r.h.b.one(t); sp.Hash != first.Hash {
		t.Fatalf("spec changed: %+v", sp)
	}
}

func TestConnectionRefusedGrace(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
	h.b.set(device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20"})
	h.c.ConfigApplied(mfpConfig(t, "samsung-scx4300-mfp"), "e")
	refused := func(r *device.AgentReport) {
		r.Queues = []device.QueueResult{{ID: "printer", State: device.QueueError, Message: "get-printers: connection refused", Unreachable: true}}
	}
	h.report(t, "samsung-scx4300-mfp", refused)
	s := h.step()
	wantEntry(t, s.Printers[0].Phase, s.Printers[0].Message, "deploying", msgStartingApp)
	h.clock.add(20 * time.Second)
	h.report(t, "samsung-scx4300-mfp", refused)
	s = h.step()
	wantEntry(t, s.Printers[0].Phase, s.Printers[0].Message, "deploying", msgStartingApp)
	h.clock.add(10 * time.Second)
	h.report(t, "samsung-scx4300-mfp", refused)
	s = h.step()
	wantEntry(t, s.Printers[0].Phase, s.Printers[0].Message, "error", "get-printers: connection refused")
	// A container still waiting (pulling) keeps the grace.
	h.b.set(device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20", Message: "printer-app-legacy: ContainerCreating"})
	s = h.step()
	wantEntry(t, s.Printers[0].Phase, s.Printers[0].Message, "deploying", msgStartingApp)
	// A new pod gets a new grace.
	h.b.set(device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20"})
	h.report(t, "samsung-scx4300-mfp", refused, func(r *device.AgentReport) { r.PodUID = "pod-2" })
	s = h.step()
	wantEntry(t, s.Printers[0].Phase, s.Printers[0].Message, "deploying", msgStartingApp)
}

func TestScannerCapsCached(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	const u = "http://127.0.0.1:4001/eSCL"
	h.c.ConfigApplied(mfpConfig(t, "ippusb-mfp"), "e")
	h.report(t, "ippusb-mfp")
	h.b.set(running(device.Endpoint{ID: "scanner", ScannerURL: u}))
	h.capsErr[u] = errors.New("timeout")
	h.step()
	h.step()
	if h.capsCalls[u] != 1 {
		t.Fatalf("%d caps calls after a failure within resync, want 1", h.capsCalls[u])
	}
	h.capsErr[u] = nil
	h.clock.add(resync)
	s := h.step()
	h.step()
	h.clock.add(time.Hour)
	h.report(t, "ippusb-mfp")
	h.step()
	if h.capsCalls[u] != 2 || !slices.Equal(s.Scanners[0].Capabilities.Sources, []string{"platen"}) {
		t.Fatalf("%d caps calls, caps %+v", h.capsCalls[u], s.Scanners[0].Capabilities)
	}
	h.b.set(running(device.Endpoint{ID: "scanner", ScannerURL: u + "2"}))
	h.step()
	if h.capsCalls[u+"2"] != 1 {
		t.Fatal("a new URL was not queried")
	}
}

func TestBlobWorkload(t *testing.T) {
	nb := newNodeBackend("n1", "n2")
	h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
	hp, sam := dev(t, "hp-legacy-mfp", "03f0:042a"), dev(t, "samsung-scx4300-mfp", "04e8:342e")
	cfg := cfgOf([]config.Printer{usbPrinter("samsung", sam), usbPrinter("hp", hp)})
	cfg.Licenses.AcceptedEULAs = []config.EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: textSHA}}
	h.c.ConfigApplied(cfg, "e")
	h.reportOn(t, "n1", "samsung-scx4300-mfp")
	h.reportOn(t, "n2", "hp-legacy-mfp")
	h.step()
	specs := h.b.last(t)
	if len(specs[0].Blobs) != 0 || len(specs[1].Blobs) != 1 {
		t.Fatalf("specs %+v, want the blob on n2", specs)
	}
	want := []device.EULARef{{ID: "hplip-plugin", Version: "3.24.4"}}
	if ds := h.reportOn(t, "n2", "hp-legacy-mfp"); !reflect.DeepEqual(ds.StageEULAs, want) {
		t.Fatalf("n2 stage %+v", ds.StageEULAs)
	}
	if ds := h.reportOn(t, "n1", "samsung-scx4300-mfp"); len(ds.StageEULAs) != 0 {
		t.Fatalf("n1 stage %+v", ds.StageEULAs)
	}

	// Two nodes needing blobs: the first keeps them, and the nodes item says so.
	hp2 := hp
	hp2.Serial = "OTHER"
	cfg2 := cfgOf([]config.Printer{usbPrinter("hp", hp), usbPrinter("hp2", hp2)})
	cfg2.Licenses = cfg.Licenses
	h.c.ConfigApplied(cfg2, "e2")
	h.reportOn(t, "n1", "hp-legacy-mfp", func(r *device.AgentReport) { r.Inventory.Devices[0].Serial = "OTHER" })
	s := h.step()
	specs = h.b.last(t)
	if len(specs[0].Blobs) != 1 || len(specs[1].Blobs) != 0 {
		t.Fatalf("specs %+v", specs)
	}
	i := slices.IndexFunc(s.Reconcile, func(r apitypes.ReconcileItem) bool { return r.Item == "nodes" })
	if i < 0 || s.Reconcile[i].OK || !strings.Contains(s.Reconcile[i].Message, "n1, n2") {
		t.Fatalf("reconcile %+v", s.Reconcile)
	}
}

// A control-plane restart with a running pod: no Apply until it reports,
// and its first report yields the same spec hash.
func TestControlPlaneRestartSingleNode(t *testing.T) {
	before := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
	before.c.ConfigApplied(mfpConfig(t, "ippusb-mfp"), "e")
	before.report(t, "ippusb-mfp")
	before.step()
	running := before.b.one(t)

	for _, phase := range []string{device.PhaseRunning, device.PhasePending} {
		t.Run(phase, func(t *testing.T) {
			h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
			h.b.set(device.Observation{Phase: phase})
			h.c.ConfigApplied(mfpConfig(t, "ippusb-mfp"), "e")
			s := h.step()
			wantEntry(t, s.Printers[0].Phase, s.Printers[0].Message, "deploying", msgWaitingAgent)
			h.clock.set(t0, resync)
			h.step()
			h.clock.set(t0, pendingWait-time.Second)
			h.step()
			wantApplies(t, h.b, 0)
			h.report(t, "ippusb-mfp")
			h.step()
			if sp := h.b.one(t); sp.Hash != running.Hash {
				t.Fatalf("spec %+v, want the running hash", sp)
			}
		})
	}
	t.Run("never reporting", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
		h.b.set(device.Observation{Phase: device.PhaseRunning})
		h.c.ConfigApplied(mfpConfig(t, "ippusb-mfp"), "e")
		h.clock.set(t0, pendingWait)
		h.step()
		if sp := h.b.one(t); sp.Mode != device.ModeProbe {
			t.Fatalf("spec %+v", sp)
		}
	})
	t.Run("absent pod applies at once", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
		h.c.ConfigApplied(mfpConfig(t, "ippusb-mfp"), "e")
		h.step()
		wantApplies(t, h.b, 1)
	})
}

func TestControlPlaneRestartMultiNode(t *testing.T) {
	nb := newNodeBackend("n1", "n2")
	listed := false
	nb.onNodes = func() { listed = true }
	nb.onObserve = func() {
		if listed {
			nb.obs = []device.Observation{{Workload: "n1", Phase: device.PhaseRunning}, {Workload: "n2", Phase: device.PhaseRunning}}
		}
	}
	h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
	hp, sam := dev(t, "ippusb-mfp", "03f0:2754"), dev(t, "samsung-scx4300-mfp", "04e8:342e")
	h.c.ConfigApplied(cfgOf([]config.Printer{usbPrinter("hp", hp), usbPrinter("samsung", sam)}), "e")
	h.step()
	if nb.calls[0] != "nodes" || nb.calls[1] != "observe" {
		t.Fatalf("calls %v, want the node list before Observe", nb.calls)
	}
	h.reportOn(t, "n1", "ippusb-mfp")
	h.step()
	wantApplies(t, h.b, 0)
	h.reportOn(t, "n2", "samsung-scx4300-mfp")
	h.step()
	wantApplies(t, h.b, 1)
	specs := h.b.last(t)

	ref := newHarness(t, Options{Backend: newNodeBackend("n1", "n2"), BackendName: BackendK8s, USBAvailable: true})
	ref.c.ConfigApplied(cfgOf([]config.Printer{usbPrinter("hp", hp), usbPrinter("samsung", sam)}), "e")
	ref.reportOn(t, "n1", "ippusb-mfp")
	ref.reportOn(t, "n2", "samsung-scx4300-mfp")
	ref.step()
	want := ref.b.last(t)
	if specs[0].Hash != want[0].Hash || specs[1].Hash != want[1].Hash {
		t.Fatal("the first reports after a restart changed the hashes")
	}
}

// Until the watcher's first callback the file is not known to be missing, so
// the controller must not tear down or probe over a running device pod.
func TestNoApplyBeforeConfigRead(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true, BackendName: BackendK8s})
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

func TestUSBUnavailable(t *testing.T) {
	const msg = "USB printers and scanners need a USB resource (set --usb-resource)"
	h := newHarness(t, Options{BackendName: BackendK8s, USBUnavailable: msg})
	cfg := mfpConfig(t, "ippusb-mfp")
	cfg.Printers = append(cfg.Printers, simPrinter("sim"))
	h.c.ConfigApplied(cfg, "e")
	s := h.step()
	wantEntry(t, printerStatus(t, s, "printer").Phase, printerStatus(t, s, "printer").Message, "error", msg)
	wantEntry(t, scannerStatus(t, s, "scanner").Phase, "", "error", "*")
	if sp := h.b.one(t); sp.Mode != device.ModeSimulated || len(sp.Printers) != 1 || sp.Printers[0].ID != "sim" {
		t.Fatalf("spec %+v, want the simulated printer only", sp)
	}
	wantPhase(t, s, "error", "printer: "+msg)
}

func TestAgentOutage(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.c.ConfigApplied(mfpConfig(t, "ippusb-mfp"), "e")
	h.report(t, "ippusb-mfp")
	h.b.set(running(device.Endpoint{ID: "printer", PrinterURI: "ipp://127.0.0.1:1/ipp/print"}, device.Endpoint{ID: "scanner", ScannerURL: "http://127.0.0.1:2/eSCL"}))
	wantPhase(t, h.step(), "ready", "")
	h.clock.add(31 * time.Second)
	s := h.step()
	wantPhase(t, s, "degraded", "printer: "+msgNotReporting)
	wantApplies(t, h.b, 1)
	if len(s.Devices) != 1 {
		t.Fatal("devices dropped")
	}
}

func deviceItem(t *testing.T, s apitypes.Status) apitypes.ReconcileItem {
	t.Helper()
	for _, r := range s.Reconcile {
		if r.Item == "workload/default" {
			return r
		}
	}
	t.Fatalf("no workload/default reconcile item in %+v", s.Reconcile)
	return apitypes.ReconcileItem{}
}

func TestApplyFailureRetried(t *testing.T) {
	h := newHarness(t, Options{})
	h.b.applyErr = errors.New("owner deployment printing/pagewire: not found")
	h.c.ConfigApplied(simConfig(), "e")
	s := h.step()
	if r := deviceItem(t, s); r.OK || r.Message != "owner deployment printing/pagewire: not found" {
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
	// The P2055dn's hplip driver needs no plug-in, so the family's optional EULA applies.
	p2055 := func(r *device.AgentReport) {
		r.Inventory.Devices[0].DeviceID = "MFG:Hewlett-Packard;CMD:PJL,PCL,POSTSCRIPT;MDL:HP LaserJet P2055dn;CLS:PRINTER;"
	}
	h := newHarness(t, Options{USBAvailable: true})
	h.c.ConfigApplied(mfpConfig(t, "hp-legacy-mfp"), "e1")
	h.report(t, "hp-legacy-mfp", p2055)
	h.step()
	if sp := h.b.one(t); len(sp.Blobs) != 0 {
		t.Fatalf("blobs %+v without an accepted EULA", sp.Blobs)
	}
	acc := mfpConfig(t, "hp-legacy-mfp")
	acc.Licenses.AcceptedEULAs = []config.EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: textSHA}}
	h.c.ConfigApplied(acc, "e2")
	h.step()
	sp := h.b.one(t)
	if want := []device.BlobRef{{ID: "hplip-plugin", Version: "3.24.4", Optional: true}}; !reflect.DeepEqual(sp.Blobs, want) {
		t.Fatalf("blobs %+v", sp.Blobs)
	}
	if want := []device.EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: textSHA}}; !reflect.DeepEqual(sp.AcceptedEULAs, want) {
		t.Fatalf("acceptedEulas %+v", sp.AcceptedEULAs)
	}
	h.b.set(running())
	h.report(t, "hp-legacy-mfp", p2055, func(r *device.AgentReport) {
		r.Blobs = []device.BlobStatus{{ID: "hplip-plugin", Version: "3.24.4", State: "failed", Message: "download failed"}}
	})
	s := h.step()
	if len(s.Workloads) != 1 || len(s.Workloads[0].Blobs) != 1 || s.Workloads[0].Blobs[0].State != "failed" {
		t.Fatalf("workloads %+v", s.Workloads)
	}
	p := s.Printers[0]
	wantEntry(t, p.Phase, p.Message, "degraded", "HP Linux Imaging and Printing proprietary plug-in: download failed")
}

// TestRequiredEULA: hplip's plug-in drivers need the plug-in, so the M1132 needs it out of the box.
func TestRequiredEULA(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.report(t, "hp-legacy-mfp")
	if e := h.c.Setup().EULAs; len(e) != 1 || !e[0].Required {
		t.Fatalf("eulas %+v", e)
	}
	h.c.ConfigApplied(mfpConfig(t, "hp-legacy-mfp"), "e")
	h.b.set(running())
	s := h.step()
	if sp := h.b.one(t); sp.Mode != device.ModeUSB || len(sp.Blobs) != 0 {
		t.Fatalf("spec %+v, want the usb spec without the blob", sp)
	}
	p := s.Printers[0]
	wantEntry(t, p.Phase, p.Message, "degraded", "Accept EULA HP Linux Imaging and Printing proprietary plug-in in Settings")
}

// A queue that runs the app's own driver choice instead of the decided one
// (the app lacks the index's driver) leaves the printer degraded.
func TestQueueDriverFallbackDegraded(t *testing.T) {
	h := newHarness(t, Options{BackendName: BackendK8s, USBAvailable: true})
	h.c.ConfigApplied(mfpConfig(t, "samsung-scx4300-mfp"), "e1")
	h.b.set(device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20"})
	msg := "Driver x is not in use: the printer application runs its own driver choice."
	h.report(t, "samsung-scx4300-mfp", func(r *device.AgentReport) {
		r.Queues = []device.QueueResult{{ID: "printer", State: device.QueueOK, Message: msg, PrinterURI: "ipp://127.0.0.1:8005/ipp/print/x"}}
	})
	s := h.step()
	p := s.Printers[0]
	wantEntry(t, p.Phase, p.Message, "degraded", msg)
	if p.URI != "ipp://192.168.1.20:8005/ipp/print/x" {
		t.Fatalf("uri %q", p.URI)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, u := range h.printed {
		if strings.Contains(u, "127.0.0.1") {
			t.Fatalf("queried loopback URI %s", u)
		}
	}
}

func TestInvalidConfig(t *testing.T) {
	t.Run("keeps last-good spec", func(t *testing.T) {
		h := newHarness(t, Options{})
		good := simConfig()
		h.c.ConfigApplied(good, "e1")
		h.step()
		el := &config.ErrorList{Errors: []config.FieldError{{Path: "/name", Line: 2, Column: 7, Message: "bad"}}}
		h.c.ConfigInvalid(el, "e2", good)
		s := h.step()
		wantApplies(t, h.b, 1)
		if s.Config.State != "invalid" || s.Config.Etag != "e2" || s.Config.AppliedEtag != "e1" || s.Config.Errors[0].Line != 2 {
			t.Fatalf("config %+v", s.Config)
		}
		if len(s.Printers) != 1 || s.Workloads[0].Mode != "simulated" {
			t.Fatalf("status %+v", s)
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
		h.b.set(running())
		h.c.ConfigInvalid(errors.New("bad"), "e", nil)
		s := h.step()
		wantApplies(t, h.b, 1)
		if s.Phase != "probing" {
			t.Fatalf("phase %q", s.Phase)
		}
	})
}

func TestPublishesConfigAndReconcileChanges(t *testing.T) {
	h := newHarness(t, Options{})
	ch, cancel := h.c.Subscribe()
	defer cancel()
	h.c.ConfigApplied(simConfig(), "e1")
	h.b.set(running(device.Endpoint{ID: "printer", PrinterURI: "ipp://127.0.0.1:1/ipp/print"}))
	h.step()
	n := len(h.pubs)
	h.step()
	if len(h.pubs) != n {
		t.Fatal("published an unchanged Status")
	}
	h.c.ConfigInvalid(errors.New("bad"), "e2", simConfig())
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

// textSHA is a well-formed accepted text sha256.
var textSHA = strings.Repeat("a", 64)

// withText makes the report carry the hplip-plugin text in state.
func withText(state string) func(*device.AgentReport) {
	return func(r *device.AgentReport) {
		t := device.EULAText{ID: "hplip-plugin", Version: "3.24.4", State: state}
		switch state {
		case device.EULAAvailable:
			t.Text, t.TextSHA256 = "licence\n", textSHA
		case device.EULAFailed:
			t.Message = "download failed"
		}
		r.EULAs = []device.EULAText{t}
	}
}

func TestEULA(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	if _, ok := h.c.EULA("no-such-eula"); ok {
		t.Fatal("EULA found for an id not in the catalog")
	}
	e, ok := h.c.EULA("hplip-plugin")
	if !ok || e.State != apitypes.EULAPending || e.Text != nil || e.Version != "3.24.4" || e.Vendor != "HP" {
		t.Fatalf("before any report: %+v, %v", e, ok)
	}
	h.report(t, "hp-legacy-mfp", withText(device.EULAAvailable))
	if e, _ = h.c.EULA("hplip-plugin"); e.State != apitypes.EULAAvailable || e.Text == nil || *e.Text != "licence\n" {
		t.Fatalf("available: %+v", e)
	}
	h.report(t, "hp-legacy-mfp", withText(device.EULAPending))
	if e, _ = h.c.EULA("hplip-plugin"); e.State != apitypes.EULAAvailable {
		t.Fatalf("pending replaced the known text: %+v", e)
	}
	h.report(t, "hp-legacy-mfp", withText(device.EULAFailed))
	if e, _ = h.c.EULA("hplip-plugin"); e.State != apitypes.EULAFailed || e.Message != "download failed" {
		t.Fatalf("failed: %+v", e)
	}
	h.report(t, "hp-legacy-mfp", func(r *device.AgentReport) {
		r.EULAs = []device.EULAText{{ID: "not-in-catalog", Version: "1", State: device.EULAAvailable}}
	})
	h.c.mu.Lock()
	_, stored := h.c.eulaTexts["not-in-catalog"]
	h.c.mu.Unlock()
	if stored {
		t.Error("stored the text of a EULA not in the catalog")
	}
}

func TestStageEULAs(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	want := []device.EULARef{{ID: "hplip-plugin", Version: "3.24.4"}}
	// No config: the suggested printer's decision offers the HP plug-in.
	if ds := h.report(t, "hp-legacy-mfp"); !reflect.DeepEqual(ds.StageEULAs, want) {
		t.Fatalf("no config: stageEulas %+v", ds.StageEULAs)
	}
	if ds := h.report(t, "ippusb-mfp"); len(ds.StageEULAs) != 0 {
		t.Fatalf("no EULA device: stageEulas %#v, want empty", ds.StageEULAs)
	}
	// An accepted EULA stays staged even when the device does not offer it.
	cfg := mfpConfig(t, "ippusb-mfp")
	cfg.Licenses.AcceptedEULAs = []config.EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: textSHA}}
	h.c.ConfigApplied(cfg, "e")
	if ds := h.report(t, "ippusb-mfp"); !reflect.DeepEqual(ds.StageEULAs, want) {
		t.Fatalf("accepted: stageEulas %+v", ds.StageEULAs)
	}
}

func TestAcceptedMatchesIDAndVersion(t *testing.T) {
	cat := catalog.Default()
	acc := []config.EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: textSHA}}
	if !accepted(cat, "hplip-plugin", acc) {
		t.Error("an entry with a text sha256 is not accepted")
	}
	if accepted(cat, "hplip-plugin", []config.EULARef{{ID: "hplip-plugin", Version: "1.0", TextSHA256: textSHA}}) {
		t.Error("another version is accepted")
	}
}

func TestRunReactsToInputsAndStops(t *testing.T) {
	b := &fakeBackend{}
	c := New(Options{Backend: b, BackendName: BackendSim})
	ch, cancelSub := c.Subscribe()
	defer cancelSub()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	c.ConfigApplied(simConfig(), "e1")
	deadline := time.After(2 * time.Second)
	for s := (apitypes.Status{}); len(s.Printers) == 0; {
		select {
		case s = <-ch:
		case <-deadline:
			t.Fatal("no Status with the simulated printer published within 2 s")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestConvertedFormats(t *testing.T) {
	h := newHarness(t, Options{USBAvailable: true})
	h.c.ConfigApplied(mfpConfig(t, "ippusb-mfp"), "e")
	h.b.set(running(device.Endpoint{ID: "printer", PrinterURI: "ipp://127.0.0.1:40000/ipp/print"}))
	h.report(t, "ippusb-mfp")
	s := h.step()
	// The fake printer lists only application/pdf.
	want := []apitypes.FormatConversion{{From: "image/png", To: "application/pdf"}, {From: "image/jpeg", To: "application/pdf"}}
	if p := s.Printers[0]; !p.Available || !reflect.DeepEqual(p.ConvertedFormats, want) {
		t.Fatalf("printer %+v, want convertedFormats %v", p, want)
	}
}

func TestUSBDevicesPassedBeforeApply(t *testing.T) {
	db := &deviceBackend{fakeBackend: &fakeBackend{obs: []device.Observation{{Phase: device.PhaseRunning}}}}
	h := newHarness(t, Options{Backend: db, BackendName: BackendDocker, USBAvailable: true})
	h.c.ConfigApplied(officeConfig(t), "e")
	h.step() // no report yet: wait, no Apply and no devices
	if len(db.devs) != 0 {
		t.Fatalf("devices set while waiting: %v", db.devs)
	}
	h.report(t, "office")
	h.step()
	if len(db.devs) != 1 || len(db.devs[0]) != 2 || db.devs[0][0].Address != 4 || db.devs[0][1].Address != 9 {
		t.Fatalf("devices %+v, want the HP and the Samsung once each", db.devs)
	}
	if !slices.Equal(db.applies, []int{1}) {
		t.Fatalf("Apply after %v SetUSBDevices calls, want [1]", db.applies)
	}
	h.step() // same specs, no Apply: the devices are taken at once
	if !slices.Equal(db.staged, []bool{true, false}) || !slices.Equal(db.applies, []int{1}) {
		t.Fatalf("staged %v, applies %v: want [true false], [1]", db.staged, db.applies)
	}
	h.c.ConfigApplied(simConfig(), "e2")
	h.step()
	if last := db.devs[len(db.devs)-1]; len(last) != 0 {
		t.Fatalf("simulated devices %+v, want none", last)
	}
}

func TestDetect(t *testing.T) {
	rb := &restartBackend{fakeBackend: &fakeBackend{}}
	h := newHarness(t, Options{Backend: rb, BackendName: BackendK8s, USBAvailable: true})
	h.c.ConfigMissing()
	h.step()
	if err := h.c.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.clock.add(10 * time.Second)
	_ = h.c.Detect(context.Background())
	if !slices.Equal(rb.restarts, []string{""}) {
		t.Fatalf("restarts %v, want one within 30 s", rb.restarts)
	}
	sim := newHarness(t, Options{})
	if err := sim.c.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// After a control-plane restart, a node whose only printer is off is not
// listed but still runs its pod: nothing is applied until it reports, so its
// workload is not deleted.
func TestControlPlaneRestartUnlistedNode(t *testing.T) {
	nb := newNodeBackend("n1")
	nb.obs = []device.Observation{{Workload: "n1", Phase: device.PhaseRunning}, {Workload: "n2", Phase: device.PhaseRunning}}
	h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
	hp, sam := dev(t, "ippusb-mfp", "03f0:2754"), dev(t, "samsung-scx4300-mfp", "04e8:342e")
	h.c.ConfigApplied(cfgOf([]config.Printer{usbPrinter("hp", hp), usbPrinter("samsung", sam)}), "e")
	h.reportOn(t, "n1", "ippusb-mfp")
	h.step()
	wantApplies(t, h.b, 0)
	h.reportOn(t, "n2", "samsung-scx4300-mfp", func(r *device.AgentReport) { r.Attached = []device.USBDevice{} })
	h.step()
	specs := h.b.last(t)
	if len(specs) != 2 || specs[1].Node != "n2" || len(specs[1].Printers) != 1 {
		t.Fatalf("specs %+v, want both nodes", specs)
	}
}

func TestTooManyNodes(t *testing.T) {
	nodes := []string{"n1", "n2", "n3", "n4", "n5"}
	nb := newNodeBackend(nodes...)
	h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
	var printers []config.Printer
	for i, n := range nodes {
		d := catalog.Fixture("ippusb-mfp").Devices[0]
		d.Serial = fmt.Sprint("S", i)
		printers = append(printers, usbPrinter(n, d))
		h.reportOn(t, n, "ippusb-mfp", func(r *device.AgentReport) { r.Inventory.Devices[0].Serial = d.Serial })
	}
	h.c.ConfigApplied(cfgOf(printers), "e")
	s := h.step()
	i := slices.IndexFunc(s.Reconcile, func(r apitypes.ReconcileItem) bool { return r.Item == "nodes" })
	if i < 0 || s.Reconcile[i].OK || !strings.Contains(s.Reconcile[i].Message, "n5 are left out") {
		t.Fatalf("reconcile %+v", s.Reconcile)
	}
	// The backend gets only what fits its slots; the left-out printer says so.
	if specs := h.b.last(t); len(specs) != 4 || specs[3].Node != "n4" {
		t.Fatalf("specs %+v, want n1-n4", specs)
	}
	p := printerStatus(t, s, "n5")
	wantEntry(t, p.Phase, p.Message, "error", msgLeftOut)
	wantEntry(t, printerStatus(t, s, "n1").Phase, "", "deploying", "*")

	// A node that sorts first but is new does not evict a node already served.
	nb.mu.Lock()
	nb.nodes = append([]string{"n0"}, nodes...)
	nb.mu.Unlock()
	d := catalog.Fixture("ippusb-mfp").Devices[0]
	d.Serial = "S-new"
	h.reportOn(t, "n0", "ippusb-mfp", func(r *device.AgentReport) { r.Inventory.Devices[0].Serial = d.Serial })
	h.c.ConfigApplied(cfgOf(append(printers, usbPrinter("n0", d))), "e2")
	h.step()
	var got []string
	for _, sp := range h.b.last(t) {
		got = append(got, sp.Node)
	}
	if !slices.Equal(got, []string{"n1", "n2", "n3", "n4"}) {
		t.Fatalf("specs for %v, want the served nodes kept", got)
	}
}

// TestNoConfigProbesCapped: with no config, probes fit the backend's slots.
func TestNoConfigProbesCapped(t *testing.T) {
	nb := newNodeBackend("n1", "n2", "n3", "n4", "n5")
	h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
	h.c.ConfigMissing()
	s := h.step()
	if specs := h.b.last(t); len(specs) != maxWorkloads {
		t.Fatalf("%d probe specs, want %d", len(specs), maxWorkloads)
	}
	for _, r := range s.Reconcile {
		if !r.OK {
			t.Errorf("item %+v failed", r)
		}
	}
}

// TestProbeSpecsDroppedFirst: probe-only nodes give way to nodes with devices
// when the slots run out, and no nodes item is set for them.
func TestProbeSpecsDroppedFirst(t *testing.T) {
	nodes := []string{"n1", "n2", "n3", "n4", "n5"}
	nb := newNodeBackend(nodes...)
	h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
	var printers []config.Printer
	for i, n := range []string{"n1", "n2", "n4", "n5"} {
		d := catalog.Fixture("ippusb-mfp").Devices[0]
		d.Serial = fmt.Sprint("S", i)
		printers = append(printers, usbPrinter(n, d))
		h.reportOn(t, n, "ippusb-mfp", func(r *device.AgentReport) { r.Inventory.Devices[0].Serial = d.Serial })
	}
	h.reportOn(t, "n3", "no-printer")
	h.c.ConfigApplied(cfgOf(printers), "e")
	s := h.step()
	var got []string
	for _, sp := range h.b.last(t) {
		if sp.Mode == device.ModeProbe {
			t.Errorf("probe spec for %s applied", sp.Node)
		}
		got = append(got, sp.Node)
	}
	if !slices.Equal(got, []string{"n1", "n2", "n4", "n5"}) {
		t.Fatalf("specs for %v", got)
	}
	for _, r := range s.Reconcile {
		if !r.OK {
			t.Errorf("item %+v failed", r)
		}
	}
}

// TestApplyFailsOneWorkload: a backend error naming one workload marks only
// that workload's item and printers, and the other workload's item stays ok.
func TestApplyFailsOneWorkload(t *testing.T) {
	nb := newNodeBackend("n1", "n2")
	h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
	hp, sam := dev(t, "ippusb-mfp", "03f0:2754"), dev(t, "samsung-scx4300-mfp", "04e8:342e")
	h.c.ConfigApplied(cfgOf([]config.Printer{usbPrinter("hp", hp), usbPrinter("samsung", sam)}), "e")
	h.reportOn(t, "n1", "ippusb-mfp")
	h.reportOn(t, "n2", "samsung-scx4300-mfp")
	h.b.applyErr = device.WorkloadErrors{"n2": errors.New("denied by admission")}
	s := h.step()
	p := printerStatus(t, s, "samsung")
	if p.Phase != "error" || !strings.Contains(p.Message, "denied by admission") {
		t.Fatalf("samsung %s %q, want the n2 error", p.Phase, p.Message)
	}
	if p := printerStatus(t, s, "hp"); p.Phase == "error" {
		t.Fatalf("hp %s %q, want it unaffected", p.Phase, p.Message)
	}
	items := map[string]bool{}
	for _, r := range s.Reconcile {
		items[r.Item] = r.OK
	}
	if ok, set := items["workload/n2"]; !set || ok || !items["workload/n1"] {
		t.Fatalf("reconcile %+v", s.Reconcile)
	}
	// n2 recovers: its item clears.
	h.b.applyErr = nil
	h.clock.add(resync)
	s = h.step()
	for _, r := range s.Reconcile {
		if !r.OK {
			t.Fatalf("item %+v still failed", r)
		}
	}
}

func TestStaleWorkloadItemCleared(t *testing.T) {
	nb := newNodeBackend("n1", "n2")
	h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
	h.c.ConfigMissing()
	h.b.applyErr = errors.New("quota")
	h.step()
	nb.mu.Lock()
	nb.nodes = []string{"n1"}
	nb.mu.Unlock()
	h.b.applyErr = nil
	h.clock.add(resync)
	s := h.step()
	for _, r := range s.Reconcile {
		if !r.OK {
			t.Fatalf("item %+v still failed", r)
		}
	}
	if len(s.Reconcile) != 2 {
		t.Fatalf("reconcile %+v", s.Reconcile)
	}
}

func workloadNodes(s apitypes.Status) []string {
	var out []string
	for _, w := range s.Workloads {
		out = append(out, w.Node)
	}
	return out
}

// TestUpgradeFromSingleWorkload: after the upgrade that adds the nodes
// ClusterRole, the backend still observes the old unpinned slot 0 as workload
// "" and its old agent reports as "". With one listed node, that report is
// the node's: the first Apply pins the full spec to it, with no probe spec in
// between.
func TestUpgradeFromSingleWorkload(t *testing.T) {
	nb := newNodeBackend("n1")
	nb.set(running())
	h := newHarness(t, Options{Backend: nb, BackendName: BackendK8s, USBAvailable: true})
	sam := dev(t, "samsung-scx4300-mfp", "04e8:342e")
	h.c.ConfigApplied(cfgOf([]config.Printer{usbPrinter("samsung", sam)}), "e")
	h.report(t, "samsung-scx4300-mfp")
	h.step()
	h.report(t, "samsung-scx4300-mfp")
	h.step()
	for i, specs := range h.b.applies() {
		for _, sp := range specs {
			if sp.Mode == device.ModeProbe || sp.Node != "n1" || len(sp.Printers) != 1 {
				t.Fatalf("Apply %d: spec %+v, want the full spec pinned to n1", i, sp)
			}
		}
	}
	wantApplies(t, h.b, 1)
}
