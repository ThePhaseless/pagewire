package sim

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/blobs"
	"github.com/ThePhaseless/pagewire/internal/blobs/blobtest"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/pappl/papplfake"
	"github.com/ThePhaseless/pagewire/internal/printing"
	"github.com/ThePhaseless/pagewire/internal/scanning"
)

// reporter records every report and answers with desired.
type reporter struct {
	mu      sync.Mutex
	reports []device.AgentReport
	desired device.DesiredState
}

func (r *reporter) report(_ context.Context, rep device.AgentReport) (device.DesiredState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, rep)
	return r.desired, nil
}

// wait polls the reports every 10 ms until ok accepts one, or fails after 15 s.
func (r *reporter) wait(t *testing.T, what string, ok func(device.AgentReport) bool) device.AgentReport {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, rep := range slices.Backward(r.reports) {
			if ok(rep) {
				r.mu.Unlock()
				return rep
			}
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a report with %s", what)
	return device.AgentReport{}
}

func newSim(t *testing.T, fixture string, r *reporter) (*Backend, string) {
	t.Helper()
	dir := t.TempDir()
	b := New(Options{Fixture: fixture, DataDir: dir})
	if r != nil {
		b.SetReporter(r.report)
	}
	t.Cleanup(func() { _ = b.Apply(context.Background(), nil) })
	return b, dir
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func observe(t *testing.T, b *Backend) device.Observation {
	t.Helper()
	obs, err := b.Observe(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Workload != "" {
		t.Fatalf("Observe() = %+v, want one node-less observation", obs)
	}
	return obs[0]
}

func apply(t *testing.T, b *Backend, spec *device.Spec) {
	t.Helper()
	if err := b.Apply(ctx(t), []*device.Spec{spec}); err != nil {
		t.Fatal(err)
	}
}

// endpoint is the observed endpoint of entry id; it fails the test when there is none.
func endpoint(t *testing.T, o device.Observation, id string) device.Endpoint {
	t.Helper()
	i := slices.IndexFunc(o.Endpoints, func(e device.Endpoint) bool { return e.ID == id })
	if i < 0 {
		t.Fatalf("Observe().Endpoints = %+v, no %q", o.Endpoints, id)
	}
	return o.Endpoints[i]
}

// fixtureDevice is the device of fixture with vendor:product key.
func fixtureDevice(t *testing.T, fixture, key string) device.USBDevice {
	t.Helper()
	for _, d := range catalog.Fixture(fixture).Devices {
		if modelKey(d) == key {
			return d
		}
	}
	t.Fatalf("fixture %s has no %s", fixture, key)
	return device.USBDevice{}
}

// cycles wakes the agent n times, waiting for a new report after each.
func (r *reporter) cycles(t *testing.T, b *Backend, n int) {
	t.Helper()
	for range n {
		r.mu.Lock()
		seen := len(r.reports)
		r.mu.Unlock()
		b.mu.Lock()
		core := b.core
		b.mu.Unlock()
		core.Wake()
		deadline := time.Now().Add(15 * time.Second)
		for {
			r.mu.Lock()
			got := len(r.reports)
			r.mu.Unlock()
			if got > seen {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for the next agent cycle")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// queue is the report's result for queue id, or nil.
func queue(rep device.AgentReport, id string) *device.QueueResult {
	i := slices.IndexFunc(rep.Queues, func(q device.QueueResult) bool { return q.ID == id })
	if i < 0 {
		return nil
	}
	return &rep.Queues[i]
}

func queueOK(id string) func(device.AgentReport) bool {
	return func(rep device.AgentReport) bool {
		q := queue(rep, id)
		return q != nil && q.State == device.QueueOK
	}
}

func usbPrinter(id, family string, d device.USBDevice) device.PrinterSpec {
	return device.PrinterSpec{ID: id, Family: family, USB: d.Key()}
}

func usbScanner(id, family string, d device.USBDevice) device.ScannerSpec {
	return device.ScannerSpec{ID: id, Family: family, USB: d.Key()}
}

// ippusbSpec serves the ippusb-mfp fixture's HP as printer "hp" and scanner "hp-scan".
func ippusbSpec(t *testing.T) *device.Spec {
	hp := fixtureDevice(t, "ippusb-mfp", hpOfficeJet)
	return &device.Spec{
		Mode:     device.ModeUSB,
		Printers: []device.PrinterSpec{usbPrinter("hp", "ipp-usb", hp)},
		Scanners: []device.ScannerSpec{usbScanner("hp-scan", "ipp-usb", hp)},
	}
}

func samplePDF(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../printing/testdata/sample.pdf")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hostPort(t *testing.T, raw string) string {
	t.Helper()
	rest := raw[strings.Index(raw, "://")+3:]
	host, _, _ := strings.Cut(rest, "/")
	return host
}

// closed reports whether srv was closed: Serve on a closed server returns
// http.ErrServerClosed at once. It asserts on the server itself, not its port,
// which another package's test server may take over once it is freed.
func closed(srv *http.Server) bool {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return false
	}
	_ = ln.Close() // a server still open gets an Accept error, not ErrServerClosed
	return errors.Is(srv.Serve(ln), http.ErrServerClosed)
}

func servers(b *Backend) []*http.Server {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.servers)
}

func TestObserveAbsentBeforeApply(t *testing.T) {
	b, _ := newSim(t, "ippusb-mfp", &reporter{})
	if o := observe(t, b); o.Phase != device.PhaseAbsent || len(o.Endpoints) != 0 {
		t.Errorf("Observe() = %+v, want absent", o)
	}
}

func TestApplyBeforeSetReporter(t *testing.T) {
	b, _ := newSim(t, "ippusb-mfp", nil)
	if err := b.Apply(ctx(t), []*device.Spec{{Mode: device.ModeProbe}}); err == nil {
		t.Error("Apply(spec) before SetReporter succeeded")
	}
	if err := b.Apply(ctx(t), nil); err != nil {
		t.Errorf("Apply(nil) = %v", err)
	}
	if o := observe(t, b); o.Phase != device.PhaseAbsent {
		t.Errorf("Observe() = %+v, want absent", o)
	}
}

func TestApplyTakesOneSpec(t *testing.T) {
	b, _ := newSim(t, "ippusb-mfp", &reporter{})
	if err := b.Apply(ctx(t), []*device.Spec{{Mode: device.ModeProbe}, {Mode: device.ModeProbe, Node: "n2"}}); err == nil {
		t.Error("Apply(two specs) succeeded")
	}
	if o := observe(t, b); o.Phase != device.PhaseAbsent {
		t.Errorf("Observe() = %+v, want absent", o)
	}
}

func TestProbeReportsFixture(t *testing.T) {
	r := &reporter{}
	b, _ := newSim(t, "hp-legacy-mfp", r)
	apply(t, b, &device.Spec{Mode: device.ModeProbe})
	want := catalog.Fixture("hp-legacy-mfp").Devices
	rep := r.wait(t, "the fixture inventory", func(rep device.AgentReport) bool { return len(rep.Inventory.Devices) > 0 })
	if rep.Inventory.Devices[0].Serial != want[0].Serial || len(rep.Queues) != 0 || !rep.Inventory.Simulated || rep.Workload != "" {
		t.Errorf("report = %+v", rep)
	}
	if len(rep.Attached) != len(want) || rep.Attached[0].Key() != want[0].Key() {
		t.Errorf("Attached = %+v, want the fixture devices", rep.Attached)
	}
	o := observe(t, b)
	if o.Phase != device.PhaseRunning || o.HostIP != "127.0.0.1" || len(o.Endpoints) != 0 || o.Unschedulable {
		t.Errorf("Observe() = %+v, want running without endpoints", o)
	}
}

func TestNoneFixtureReportsEmptyInventory(t *testing.T) {
	r := &reporter{}
	b, _ := newSim(t, "none", r)
	apply(t, b, &device.Spec{Mode: device.ModeProbe})
	rep := r.wait(t, "any inventory", func(device.AgentReport) bool { return true })
	if rep.Inventory.Devices == nil || len(rep.Inventory.Devices) != 0 {
		t.Errorf("inventory = %+v, want empty non-nil devices", rep.Inventory)
	}
}

func TestIPPUSBPrinterAndScanner(t *testing.T) {
	b, dir := newSim(t, "ippusb-mfp", &reporter{})
	apply(t, b, ippusbSpec(t))
	o := observe(t, b)
	printerURI, scannerURL := endpoint(t, o, "hp").PrinterURI, endpoint(t, o, "hp-scan").ScannerURL
	if o.Phase != device.PhaseRunning || o.HostIP != "127.0.0.1" || len(o.Endpoints) != 2 ||
		!strings.HasPrefix(printerURI, "ipp://127.0.0.1:") || !strings.HasPrefix(scannerURL, "http://127.0.0.1:") {
		t.Fatalf("Observe() = %+v", o)
	}

	pc := printing.New(printerURI)
	attrs, err := pc.Attributes(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{printing.FormatPDF, printing.FormatJPEG, "image/pwg-raster", printing.FormatURF} {
		if !slices.Contains(attrs.Formats, f) {
			t.Errorf("formats %v lack %s", attrs.Formats, f)
		}
	}
	if attrs.State != "idle" || len(attrs.Media) != 5 || len(attrs.Markers) == 0 || !slices.Contains(attrs.URIs, printerURI) {
		t.Errorf("Attributes() = %+v", attrs)
	}

	pdf := samplePDF(t)
	before := time.Now().Add(-2 * time.Second)
	id, err := pc.Submit(ctx(t), "sample.pdf", "web", printing.FormatPDF, bytes.NewReader(pdf), printing.PrintOptions{})
	if err != nil {
		t.Fatal(err)
	}
	j, err := pc.Job(ctx(t), id)
	if err != nil || j.State != "completed" {
		t.Errorf("Job(%d) = %+v, %v; want completed", id, j, err)
	}
	if j.CreatedAt.Before(before) || j.CreatedAt.After(time.Now().Add(2*time.Second)) {
		t.Errorf("Job(%d).CreatedAt = %v, want about now", id, j.CreatedAt)
	}
	jobs, err := pc.Jobs(ctx(t), 20)
	if err != nil || len(jobs) != 1 || jobs[0].State != "completed" || !jobs[0].CreatedAt.Equal(j.CreatedAt) {
		t.Errorf("Jobs() = %+v, %v; want createdAt %v", jobs, err, j.CreatedAt)
	}
	got, err := os.ReadFile(filepath.Join(dir, "sim-output", "hp-1.bin")) //nolint:gosec // test temp dir
	if err != nil || !bytes.Equal(got, pdf) {
		t.Errorf("sim-output/hp-1.bin = %d bytes, %v; want the sample", len(got), err)
	}

	sc, err := scanning.NewMFP(scannerURL)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := sc.Capabilities(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if !caps.ADF || !slices.Contains(caps.Sources, "platen") || !slices.Contains(caps.Resolutions, 300) || !slices.Contains(caps.ColorModes, "color") {
		t.Errorf("Capabilities() = %+v", caps)
	}
	for _, tc := range []struct {
		source string
		res    int
		want   int
	}{{"platen", 300, 1}, {"adf", 300, 2}, {"platen", 75, 1}} {
		source, want := tc.source, tc.want
		job, err := sc.Start(ctx(t), scanning.Settings{Source: source, Resolution: tc.res, ColorMode: "color", Format: "jpeg"})
		if err != nil {
			t.Fatalf("%s: Start: %v", source, err)
		}
		n := 0
		for {
			page, err := sc.NextPage(ctx(t), job)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s: NextPage: %v", source, err)
			}
			_, _ = io.Copy(io.Discard, page)
			_ = page.Close()
			n++
		}
		if n != want {
			t.Errorf("%s: %d pages, want %d", source, n, want)
		}
	}
}

// printerName asks the printer for printer-name and printer-location over raw IPP.
func printerName(t *testing.T, uri string) (name, location string) {
	t.Helper()
	m := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, 1)
	m.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	m.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	m.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(uri)))
	body, err := m.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx(t), http.MethodPost, "http://"+hostPort(t, uri)+"/ipp/print", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", goipp.ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var rsp goipp.Message
	if err := rsp.Decode(resp.Body); err != nil {
		t.Fatal(err)
	}
	for _, a := range rsp.Printer {
		switch a.Name {
		case "printer-name":
			name = a.Values[0].V.String()
		case "printer-location":
			location = a.Values[0].V.String()
		}
	}
	return name, location
}

func TestSimulatedPrinterNamedFromSpec(t *testing.T) {
	b, _ := newSim(t, "none", &reporter{})
	p := device.PrinterSpec{ID: "sim", Family: "sim", Name: "Living room printer", Location: "Hall", Port: 8631}
	apply(t, b, &device.Spec{Mode: device.ModeSimulated, Printers: []device.PrinterSpec{p}, Scanners: []device.ScannerSpec{{ID: "sim-scan", Family: "sim"}}})
	o := observe(t, b)
	uri := endpoint(t, o, "sim").PrinterURI
	if uri == "" || endpoint(t, o, "sim-scan").ScannerURL == "" {
		t.Fatalf("Observe() = %+v, want both endpoints", o)
	}
	if name, loc := printerName(t, uri); name != p.Name || loc != p.Location {
		t.Errorf("printer-name, printer-location = %q, %q", name, loc)
	}
	attrs, err := printing.New(uri).Attributes(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if c := attrs.Caps; len(c.Media) != 2 || c.CopiesMax != 99 || len(c.Sides) != 0 {
		t.Errorf("Caps = %+v, want A4 and Letter plus copies 1-99", c)
	}
}

func TestPAPPLPath(t *testing.T) {
	hp := catalog.Fixture("hp-legacy-mfp").Devices[0]
	fam, _ := catalog.Default().Printer("hplip")
	spec := &device.Spec{
		Mode:          device.ModeUSB,
		Printers:      []device.PrinterSpec{usbPrinter("printer", "hplip", hp)},
		Scanners:      []device.ScannerSpec{{ID: "scanner", Family: "airsane", SaneBackends: []string{"hpaio"}, USB: hp.Key()}},
		Blobs:         []device.BlobRef{{ID: "hplip-plugin", Version: "3.24.4", Optional: true}},
		AcceptedEULAs: []device.EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: blobs.TextSHA256([]byte(blobtest.License))}},
	}
	spec.Hash = device.SpecHash(spec)
	r := &reporter{desired: device.DesiredState{
		Hash: spec.Hash,
		Queues: []device.QueueSpec{{
			ID: "printer", Name: "Printer", Driver: "auto", Schemes: fam.DeviceSchemes,
			SystemURI: device.PAPPLSystemURI(fam.Port), Device: hp, Defaults: device.PrintSettings{Media: "iso_a4_210x297mm"},
		}},
		Browse:     device.BrowseTypes,
		StageEULAs: []device.EULARef{{ID: "hplip-plugin", Version: "3.24.4"}},
	}}
	b, dir := newSim(t, "hp-legacy-mfp", r)
	apply(t, b, spec)

	o := observe(t, b)
	if endpoint(t, o, "printer").PrinterURI != "" || !strings.HasPrefix(endpoint(t, o, "scanner").ScannerURL, "http://127.0.0.1:") {
		t.Errorf("Observe().Endpoints = %+v, want no printer URI (PAPPL) and a scanner URL", o.Endpoints)
	}

	rep := r.wait(t, "queue ok", queueOK("printer"))
	q := queue(rep, "printer")
	if !strings.HasPrefix(q.PrinterURI, "ipp://127.0.0.1:") || !strings.HasPrefix(q.DeviceURI, "cups:hp:") {
		t.Errorf("QueueResult = %+v, want a 127.0.0.1 printer URI and a cups:hp: device", q)
	}
	if rep.AppliedHash != spec.Hash {
		t.Errorf("AppliedHash = %q, want %q", rep.AppliedHash, spec.Hash)
	}

	pdf := samplePDF(t)
	pc := printing.New(q.PrinterURI)
	id, err := pc.Submit(ctx(t), "sample.pdf", "web", printing.FormatPDF, bytes.NewReader(pdf), printing.PrintOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if j, err := pc.Job(ctx(t), id); err != nil || j.State != "completed" {
		t.Errorf("Job(%d) = %+v, %v", id, j, err)
	}
	out, _ := filepath.Glob(filepath.Join(dir, "sim-output", "*.bin"))
	if len(out) != 1 {
		t.Errorf("sim-output holds %v, want one document", out)
	}

	// The real catalog entry may be enabled or disabled; the sim's own copy is
	// always enabled and served by the in-process vendor.
	rep = r.wait(t, "the blob installed", func(rep device.AgentReport) bool {
		return len(rep.Blobs) == 1 && rep.Blobs[0].State == device.BlobInstalled
	})
	if rep.Blobs[0].ID != "hplip-plugin" || rep.Blobs[0].Version != "3.24.4" {
		t.Errorf("Blobs = %+v", rep.Blobs)
	}
	if _, err := os.Stat(filepath.Join(dir, "sim-blobs", "hplip-plugin", ".pagewire-version")); err != nil {
		t.Error(err)
	}

	// The licence text comes from the synthetic vendor's archive.
	rep = r.wait(t, "the EULA text", func(rep device.AgentReport) bool {
		return len(rep.EULAs) == 1 && rep.EULAs[0].State == device.EULAAvailable
	})
	if e := rep.EULAs[0]; e.Text != blobtest.License || e.TextSHA256 != spec.AcceptedEULAs[0].TextSHA256 {
		t.Errorf("EULAs = %+v", rep.EULAs)
	}
}

// TestPAPPLIndexDriver: the fake app offers the driver name the dispatcher
// takes from the index, so a queue created with it comes up.
func TestPAPPLIndexDriver(t *testing.T) {
	d := fixtureDevice(t, "samsung-scx4300-mfp", samsungSCX4300)
	dec := catalog.Default().DecidePrinter(&d, config.Printer{Mode: config.ModeUSB, Driver: config.Auto})
	if dec.Family != "legacy" || dec.Driver == "" {
		t.Fatalf("DecidePrinter = %+v; want a legacy driver", dec)
	}
	fam, _ := catalog.Default().Printer(dec.Family)
	spec := &device.Spec{Mode: device.ModeUSB, Printers: []device.PrinterSpec{usbPrinter("printer", dec.Family, d)}}
	spec.Hash = device.SpecHash(spec)
	r := &reporter{desired: device.DesiredState{
		Hash: spec.Hash,
		Queues: []device.QueueSpec{{
			ID: "printer", Name: "Printer", Driver: dec.Driver, Schemes: fam.DeviceSchemes, SystemURI: device.PAPPLSystemURI(fam.Port), Device: d,
		}},
		Browse: device.BrowseTypes,
	}}
	b, _ := newSim(t, "samsung-scx4300-mfp", r)
	apply(t, b, spec)
	rep := r.wait(t, "queue ok", queueOK("printer"))
	if q := queue(rep, "printer"); !strings.HasPrefix(q.DeviceURI, "cups:usb:") {
		t.Errorf("QueueResult = %+v, want a cups:usb: device", q)
	}
}

// TestProbeStagesEULA: before any config, the probe-mode agent stages the
// licence text the wizard shows.
func TestProbeStagesEULA(t *testing.T) {
	spec := &device.Spec{Mode: device.ModeProbe}
	spec.Hash = device.SpecHash(spec)
	r := &reporter{desired: device.DesiredState{
		Hash: spec.Hash, Browse: device.BrowseTypes, StageEULAs: []device.EULARef{{ID: "hplip-plugin", Version: "3.24.4"}},
	}}
	b, _ := newSim(t, "hp-legacy-mfp", r)
	apply(t, b, spec)
	rep := r.wait(t, "the EULA text", func(rep device.AgentReport) bool {
		return len(rep.EULAs) == 1 && rep.EULAs[0].State == device.EULAAvailable
	})
	if rep.EULAs[0].Text != blobtest.License {
		t.Errorf("EULAs = %+v", rep.EULAs)
	}
}

func TestSwitchAndNilStopServers(t *testing.T) {
	b, _ := newSim(t, "ippusb-mfp", &reporter{})
	apply(t, b, ippusbSpec(t))
	o := observe(t, b)
	first := o.Endpoints
	old := servers(b)

	// The same spec again keeps the servers.
	apply(t, b, ippusbSpec(t))
	if got := observe(t, b).Endpoints; !slices.Equal(got, first) {
		t.Errorf("re-applying the same spec changed endpoints: %+v -> %+v", first, got)
	}

	// A spec without scanners opens no eSCL port.
	spec := ippusbSpec(t)
	spec.Scanners = nil
	apply(t, b, spec)
	o = observe(t, b)
	second := endpoint(t, o, "hp").PrinterURI
	if len(o.Endpoints) != 1 || second == "" {
		t.Errorf("endpoints after switch = %+v", o.Endpoints)
	}
	// Servers, not ports: the kernel may hand a freed port to the new server.
	now := servers(b)
	if len(now) != 1 || slices.Contains(old, now[0]) {
		t.Errorf("servers after the switch = %p (before %p), want one new printer server", now, old)
	}
	for _, srv := range old {
		if !slices.Contains(now, srv) && !closed(srv) {
			t.Errorf("old server %p still open after the spec switch", srv)
		}
	}

	if err := b.Apply(ctx(t), nil); err != nil {
		t.Fatal(err)
	}
	if len(servers(b)) != 0 {
		t.Error("servers left after Apply(nil)")
	}
	for _, srv := range now {
		if !closed(srv) {
			t.Error("printer server still open after Apply(nil)")
		}
	}
	if o := observe(t, b); o.Phase != device.PhaseAbsent || len(o.Endpoints) != 0 {
		t.Errorf("Observe() after nil = %+v", o)
	}
}

// TestApplyNeverWaitsForReport: the controller may hold a lock around Apply
// that its report handler also takes. Both Apply(spec) and
// Apply(nil) must return while a report is blocked on that lock.
func TestApplyNeverWaitsForReport(t *testing.T) {
	var m sync.Mutex
	entered := make(chan struct{}, 1)
	report := func(context.Context, device.AgentReport) (device.DesiredState, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		m.Lock()
		defer m.Unlock()
		return device.DesiredState{Browse: device.BrowseTypes}, nil
	}
	b := New(Options{Fixture: "ippusb-mfp", DataDir: t.TempDir()})
	b.SetReporter(report)
	testPage() // the deadline measures locking, not the one-off page encode

	within := func(what string, f func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- f() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not return within 2 s while the reporter waits for the lock", what)
		}
	}

	m.Lock()
	defer m.Unlock()
	within("Apply(spec)", func() error {
		return b.Apply(context.Background(), []*device.Spec{ippusbSpec(t)})
	})
	select {
	case <-entered: // a report is now in flight, blocked on m
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never reported")
	}
	within("Apply(other spec)", func() error {
		return b.Apply(context.Background(), []*device.Spec{{Mode: device.ModeProbe}})
	})
	within("Apply(nil)", func() error { return b.Apply(context.Background(), nil) })
}

func TestScanDelay(t *testing.T) {
	const delay = time.Second
	b := New(Options{Fixture: "ippusb-mfp", DataDir: t.TempDir(), ScanDelay: delay})
	b.SetReporter((&reporter{}).report)
	t.Cleanup(func() { _ = b.Apply(context.Background(), nil) })
	apply(t, b, ippusbSpec(t))
	sc, err := scanning.NewMFP(endpoint(t, observe(t, b), "hp-scan").ScannerURL)
	if err != nil {
		t.Fatal(err)
	}
	set := scanning.Settings{Source: "platen", Resolution: 75, ColorMode: "color", Format: "jpeg"}

	// Only the page fetch waits.
	start := time.Now()
	job, err := sc.Start(ctx(t), set)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d >= delay {
		t.Errorf("Start took %v, want under %v", d, delay)
	}
	start = time.Now()
	page, err := sc.NextPage(ctx(t), job)
	if err != nil {
		t.Fatal(err)
	}
	_ = page.Close()
	if d := time.Since(start); d < delay {
		t.Errorf("NextPage took %v, want at least %v", d, delay)
	}
	_ = sc.Cancel(ctx(t), job)

	// A cancelled page fetch returns at once, and the scanner takes the next job.
	job, err = sc.Start(ctx(t), set)
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx(t), delay/6)
	defer cancel()
	start = time.Now()
	if _, err := sc.NextPage(cctx, job); err == nil {
		t.Error("NextPage with a cancelled context succeeded")
	}
	if d := time.Since(start); d >= delay {
		t.Errorf("cancelled NextPage took %v, want under %v", d, delay)
	}
	if err := sc.Cancel(ctx(t), job); err != nil {
		t.Errorf("Cancel: %v", err)
	}
	if job, err = sc.Start(ctx(t), set); err != nil {
		t.Fatalf("Start after cancel: %v", err)
	}
	if page, err = sc.NextPage(ctx(t), job); err != nil {
		t.Fatalf("NextPage after cancel: %v", err)
	}
	_ = page.Close()
}

// officeSpec serves the office fixture: the HP (ipp-usb), the Samsung
// (legacy PAPPL, AirSane scanner), a simulated printer and the LiDE scanner.
func officeSpec(t *testing.T) *device.Spec {
	hp := fixtureDevice(t, "office", hpOfficeJet)
	scx := fixtureDevice(t, "office", samsungSCX4300)
	lide := fixtureDevice(t, "office", canonLiDE100)
	return &device.Spec{
		Mode: device.ModeUSB,
		Printers: []device.PrinterSpec{
			usbPrinter("hp", "ipp-usb", hp),
			{ID: "sim", Family: "sim", Name: "Simulated", Port: 8631},
			usbPrinter("scx-4300", "legacy", scx),
		},
		Scanners: []device.ScannerSpec{
			usbScanner("hp-scan", "ipp-usb", hp),
			{ID: "lide", Family: "airsane", SaneBackends: []string{"genesys"}, USB: lide.Key()},
			{ID: "scx-4300-scan", Family: "airsane", SaneBackends: []string{"xerox_mfp"}, USB: scx.Key()},
		},
	}
}

// legacyQueue is the Samsung's queue as the controller sends it: the legacy
// family's fixed system URI, the bound device and the index driver.
func legacyQueue(t *testing.T) device.QueueSpec {
	scx := fixtureDevice(t, "office", samsungSCX4300)
	fam, _ := catalog.Default().Printer("legacy")
	dec := catalog.Default().DecidePrinter(&scx, config.Printer{Mode: config.ModeUSB, Driver: config.Auto})
	return device.QueueSpec{
		ID: "scx-4300", Name: "Samsung SCX-4300", Location: "Office", Driver: dec.Driver, Schemes: fam.DeviceSchemes,
		SystemURI: device.PAPPLSystemURI(fam.Port), Device: scx,
		Defaults: device.PrintSettings{ColorMode: "auto", Quality: "high"}, // auto is not offered: never set
	}
}

func scannerCaps(t *testing.T, o device.Observation, id string) scanning.Caps {
	t.Helper()
	sc, err := scanning.NewMFP(endpoint(t, o, id).ScannerURL)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := sc.Capabilities(ctx(t))
	if err != nil {
		t.Fatalf("%s: Capabilities: %v", id, err)
	}
	return caps
}

func TestOfficeDevices(t *testing.T) {
	r := &reporter{desired: device.DesiredState{Queues: []device.QueueSpec{legacyQueue(t)}, Browse: device.BrowseTypes}}
	b, _ := newSim(t, "office", r)
	apply(t, b, officeSpec(t))
	o := observe(t, b)
	if len(o.Endpoints) != 6 || endpoint(t, o, "scx-4300").PrinterURI != "" {
		t.Fatalf("Observe().Endpoints = %+v, want six, the PAPPL printer without a URI", o.Endpoints)
	}

	// The HP reports every option kind, with a single output bin and finishing.
	hp, err := printing.New(endpoint(t, o, "hp").PrinterURI).Attributes(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	want := virtualCaps(fixtureDevice(t, "office", hpOfficeJet))
	want.Formats = hp.Caps.Formats
	if !reflect.DeepEqual(hp.Caps, want) {
		t.Errorf("HP caps =\n%+v\nwant\n%+v", hp.Caps, want)
	}
	if len(hp.Caps.MediaSources) != 3 || len(hp.Caps.OutputBins) != 1 || len(hp.Caps.Finishings) != 1 || hp.Caps.CopiesMax != 99 {
		t.Errorf("HP caps = %+v, want three trays, one output bin, one finishing and copies 1-99", hp.Caps)
	}

	sim, err := printing.New(endpoint(t, o, "sim").PrinterURI).Attributes(ctx(t))
	if err != nil || sim.State != "idle" || sim.Caps.CopiesMax != 99 {
		t.Errorf("simulated printer: %+v, %v", sim, err)
	}

	// The Samsung prints monochrome and one-sided, and reports nothing else beyond copies.
	rep := r.wait(t, "the Samsung queue ok", queueOK("scx-4300"))
	scx, err := printing.New(queue(rep, "scx-4300").PrinterURI).Attributes(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	c := scx.Caps
	if !slices.Equal(c.ColorModes, []string{"monochrome"}) || !slices.Equal(c.Sides, []string{"one-sided"}) || c.CopiesMax != 999 ||
		c.MediaDefault != "na_letter_8.5x11in" || c.QualityDefault != "high" || c.ColorModeDefault != "monochrome" {
		t.Errorf("Samsung caps = %+v", c)
	}
	for kind, l := range map[string][]string{
		"mediaSources": c.MediaSources, "mediaTypes": c.MediaTypes, "contentOptimize": c.ContentOptimize, "orientations": c.Orientations,
		"printScalings": c.PrintScalings, "outputBins": c.OutputBins, "finishings": c.Finishings,
	} {
		if l == nil || len(l) != 0 {
			t.Errorf("Samsung %s = %#v, want empty", kind, l)
		}
	}
	if len(c.NumberUp) != 0 {
		t.Errorf("Samsung numberUp = %v, want empty", c.NumberUp)
	}

	hpScan := scannerCaps(t, o, "hp-scan")
	if !hpScan.ADF || !hpScan.Duplex || !slices.Equal(hpScan.Intents, []string{"document", "text-and-graphic", "photo", "preview"}) {
		t.Errorf("HP scanner caps = %+v", hpScan)
	}
	lide := scannerCaps(t, o, "lide")
	if !slices.Equal(lide.Sources, []string{"platen"}) || lide.ADF || !slices.Equal(lide.Intents, []string{"document", "photo"}) ||
		!slices.Contains(lide.Resolutions, 1200) || !slices.Equal(lide.ColorModes, []string{"color", "grayscale"}) {
		t.Errorf("LiDE caps = %+v, want platen only with intents document and photo", lide)
	}
	scxScan := scannerCaps(t, o, "scx-4300-scan")
	if !slices.Equal(scxScan.Sources, []string{"platen", "adf"}) || scxScan.Duplex || len(scxScan.Intents) != 0 ||
		!slices.Equal(scxScan.Resolutions, []int{75, 150, 300, 600}) {
		t.Errorf("Samsung scanner caps = %+v, want platen and simplex ADF, 75-600 dpi, no intents", scxScan)
	}
}

// TestLegacyQueueReachesOK: the in-memory agent reaches the legacy family's
// fake through the fixed system URI, also after a re-Apply moves the fake to
// a new port, and settled queues neither ask Find-Devices again nor resend
// their defaults.
func TestLegacyQueueReachesOK(t *testing.T) {
	scx := fixtureDevice(t, "office", samsungSCX4300)
	spec := &device.Spec{Mode: device.ModeUSB, Printers: []device.PrinterSpec{usbPrinter("scx-4300", "legacy", scx)}}
	r := &reporter{desired: device.DesiredState{Queues: []device.QueueSpec{legacyQueue(t)}, Browse: device.BrowseTypes}}
	b, _ := newSim(t, "office", r)
	fake := func() *papplfake.System {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.pappls["legacy"]
	}

	apply(t, b, spec)
	first := fake()
	rep := r.wait(t, "the queue ok", queueOK("scx-4300"))
	if q := queue(rep, "scx-4300"); q.PrinterURI == "" || !strings.HasPrefix(q.DeviceURI, "cups:usb:") {
		t.Fatalf("QueueResult = %+v", q)
	}

	// A changed spec starts a new fake on a new port; the queue comes up there.
	spec.Scanners = []device.ScannerSpec{usbScanner("scx-4300-scan", "airsane", scx)}
	apply(t, b, spec)
	second := fake()
	if second == first {
		t.Fatal("the re-Apply kept the old fake")
	}
	deadline := time.Now().Add(15 * time.Second)
	for len(second.Printers()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the queue on the new fake")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.cycles(t, b, 1)
	r.mu.Lock()
	last := r.reports[len(r.reports)-1]
	r.mu.Unlock()
	b.mu.Lock()
	url := b.papplURLs[device.PAPPLSystemURI(8005)]
	b.mu.Unlock()
	q := queue(last, "scx-4300")
	if q == nil || q.State != device.QueueOK || url == "" || hostPort(t, q.PrinterURI) != hostPort(t, url) {
		t.Fatalf("QueueResult after the re-Apply = %+v, want ok on the new fake at %s", q, url)
	}
	if p := second.Printers()[0]; p.Location != "Office" || p.Defaults.Quality != "high" || p.Defaults.ColorMode != "monochrome" {
		t.Errorf("queue on the new fake = %+v, want location Office, quality high, colour mode monochrome", p)
	}

	finds, sets := second.FindDevicesCalls(), second.SetCalls()
	// The exported Wake forces a full reconcile, so these are three real
	// reconciles: the pin proves they send no extra Find-Devices or Set.
	r.cycles(t, b, 3)
	if got := second.FindDevicesCalls(); got != 1 || finds != 1 {
		t.Errorf("Find-Devices calls = %d then %d, want 1", finds, got)
	}
	if got := second.SetCalls(); got != sets {
		t.Errorf("Set-Printer-Attributes calls grew from %d to %d over unchanged cycles", sets, got)
	}
}

// TestHPAcceptsEveryOption: a job setting every kind the HP reports, and a
// scan with an intent, complete.
func TestHPAcceptsEveryOption(t *testing.T) {
	b, dir := newSim(t, "ippusb-mfp", &reporter{})
	apply(t, b, ippusbSpec(t))
	o := observe(t, b)
	pc := printing.New(endpoint(t, o, "hp").PrinterURI)
	opts := printing.PrintOptions{Copies: 2, PrintSettings: device.PrintSettings{
		Media: "na_letter_8.5x11in", MediaSource: "manual", MediaType: "photographic-glossy", Sides: "two-sided-long-edge",
		ColorMode: "monochrome", Quality: "high", Resolution: 300, ContentOptimize: "photo", Orientation: "landscape",
		NumberUp: 2, PrintScaling: "fit", OutputBin: "face-down", Finishings: "none",
	}}
	id, err := pc.Submit(ctx(t), "sample.pdf", "web", printing.FormatPDF, bytes.NewReader(samplePDF(t)), opts)
	if err != nil {
		t.Fatal(err)
	}
	if j, err := pc.Job(ctx(t), id); err != nil || j.State != "completed" {
		t.Errorf("Job(%d) = %+v, %v; want completed", id, j, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sim-output", "hp-1.bin")); err != nil {
		t.Error(err)
	}

	sc, err := scanning.NewMFP(endpoint(t, o, "hp-scan").ScannerURL)
	if err != nil {
		t.Fatal(err)
	}
	if n := scanPages(t, sc, scanning.Settings{Source: "platen", Resolution: 150, ColorMode: "grayscale", Intent: "photo", Format: "jpeg"}); n != 1 {
		t.Errorf("photo scan: %d pages, want 1", n)
	}
}

// TestOfficeScans: the LiDE scans from its platen and the Samsung from its ADF.
// They stay at 300 dpi and below: go-mfp upsamples the 300 dpi test page, so a
// 1200 dpi page takes seconds (minutes under -race).
func TestOfficeScans(t *testing.T) {
	b, _ := newSim(t, "office", &reporter{})
	apply(t, b, officeSpec(t))
	o := observe(t, b)
	for _, tc := range []struct {
		id   string
		set  scanning.Settings
		want int
	}{
		{"lide", scanning.Settings{Source: "platen", Resolution: 300, ColorMode: "color", Intent: "document", Format: "jpeg"}, 1},
		{"scx-4300-scan", scanning.Settings{Source: "adf", Resolution: 150, ColorMode: "grayscale", Format: "jpeg"}, 2},
	} {
		sc, err := scanning.NewMFP(endpoint(t, o, tc.id).ScannerURL)
		if err != nil {
			t.Fatal(err)
		}
		if n := scanPages(t, sc, tc.set); n != tc.want {
			t.Errorf("%s: %d pages, want %d", tc.id, n, tc.want)
		}
	}
}

// scanPages runs one scan and returns its page count.
func scanPages(t *testing.T, sc scanning.Scanner, set scanning.Settings) int {
	t.Helper()
	job, err := sc.Start(ctx(t), set)
	if err != nil {
		t.Fatalf("Start(%+v): %v", set, err)
	}
	n := 0
	for {
		page, err := sc.NextPage(ctx(t), job)
		if errors.Is(err, io.EOF) {
			return n
		}
		if err != nil {
			t.Fatalf("NextPage(%+v): %v", set, err)
		}
		_, _ = io.Copy(io.Discard, page)
		_ = page.Close()
		n++
	}
}
