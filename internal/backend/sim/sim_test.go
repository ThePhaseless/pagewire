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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/blobs"
	"github.com/ThePhaseless/pagewire/internal/blobs/blobtest"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/device"
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
	o, err := b.Observe(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	return o
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

func refused(addr string) bool {
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(context.Background(), "tcp", addr)
	if err == nil {
		_ = c.Close()
	}
	return err != nil
}

func TestObserveAbsentBeforeApply(t *testing.T) {
	b, _ := newSim(t, "ippusb-mfp", &reporter{})
	if o := observe(t, b); o.Phase != device.PhaseAbsent || o.Endpoints != (device.Endpoints{}) {
		t.Errorf("Observe() = %+v, want absent", o)
	}
}

func TestApplyBeforeSetReporter(t *testing.T) {
	b, _ := newSim(t, "ippusb-mfp", nil)
	if err := b.Apply(ctx(t), &device.Spec{Mode: device.ModeProbe}); err == nil {
		t.Error("Apply(spec) before SetReporter succeeded")
	}
	if err := b.Apply(ctx(t), nil); err != nil {
		t.Errorf("Apply(nil) = %v", err)
	}
	if o := observe(t, b); o.Phase != device.PhaseAbsent {
		t.Errorf("Observe() = %+v, want absent", o)
	}
}

func TestProbeReportsFixture(t *testing.T) {
	r := &reporter{}
	b, _ := newSim(t, "hp-legacy-mfp", r)
	if err := b.Apply(ctx(t), &device.Spec{Mode: device.ModeProbe}); err != nil {
		t.Fatal(err)
	}
	want := catalog.Fixture("hp-legacy-mfp").Devices
	rep := r.wait(t, "the fixture inventory", func(rep device.AgentReport) bool { return len(rep.Inventory.Devices) > 0 })
	if rep.Inventory.Devices[0].Serial != want[0].Serial || !rep.DevicePresent || rep.Queue != nil || !rep.Inventory.Simulated {
		t.Errorf("report = %+v", rep)
	}
	o := observe(t, b)
	if o.Phase != device.PhaseRunning || o.HostIP != "127.0.0.1" || o.Endpoints != (device.Endpoints{}) || o.Unschedulable {
		t.Errorf("Observe() = %+v, want running without endpoints", o)
	}
}

func TestNoneFixtureReportsEmptyInventory(t *testing.T) {
	r := &reporter{}
	b, _ := newSim(t, "none", r)
	if err := b.Apply(ctx(t), &device.Spec{Mode: device.ModeProbe}); err != nil {
		t.Fatal(err)
	}
	rep := r.wait(t, "any inventory", func(device.AgentReport) bool { return true })
	if rep.Inventory.Devices == nil || len(rep.Inventory.Devices) != 0 {
		t.Errorf("inventory = %+v, want empty non-nil devices", rep.Inventory)
	}
}

func TestIPPUSBPrinterAndScanner(t *testing.T) {
	b, dir := newSim(t, "ippusb-mfp", &reporter{})
	if err := b.Apply(ctx(t), &device.Spec{Mode: device.ModeUSB, Printer: "ipp-usb", Scanner: "ipp-usb"}); err != nil {
		t.Fatal(err)
	}
	o := observe(t, b)
	if o.Phase != device.PhaseRunning || o.HostIP != "127.0.0.1" ||
		!strings.HasPrefix(o.Endpoints.PrinterURI, "ipp://127.0.0.1:") || !strings.HasPrefix(o.Endpoints.ScannerURL, "http://127.0.0.1:") {
		t.Fatalf("Observe() = %+v", o)
	}

	pc := printing.New(o.Endpoints.PrinterURI)
	attrs, err := pc.Attributes(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{printing.FormatPDF, printing.FormatJPEG, "image/pwg-raster", printing.FormatURF} {
		if !slices.Contains(attrs.Formats, f) {
			t.Errorf("formats %v lack %s", attrs.Formats, f)
		}
	}
	if attrs.State != "idle" || len(attrs.Media) != 2 || len(attrs.Markers) == 0 || !slices.Contains(attrs.URIs, o.Endpoints.PrinterURI) {
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
	got, err := os.ReadFile(filepath.Join(dir, "sim-output", "1.bin")) //nolint:gosec // test temp dir
	if err != nil || !bytes.Equal(got, pdf) {
		t.Errorf("sim-output/1.bin = %d bytes, %v; want the sample", len(got), err)
	}

	sc, err := scanning.NewMFP(o.Endpoints.ScannerURL)
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
	spec := &device.Spec{Mode: device.ModeSimulated, Printer: "sim", Scanner: "sim", Name: "Living room printer", Location: "Hall"}
	if err := b.Apply(ctx(t), spec); err != nil {
		t.Fatal(err)
	}
	o := observe(t, b)
	if o.Endpoints.PrinterURI == "" || o.Endpoints.ScannerURL == "" {
		t.Fatalf("Observe() = %+v, want both endpoints", o)
	}
	if name, loc := printerName(t, o.Endpoints.PrinterURI); name != spec.Name || loc != spec.Location {
		t.Errorf("printer-name, printer-location = %q, %q", name, loc)
	}
}

func TestPAPPLPath(t *testing.T) {
	spec := &device.Spec{
		Mode: device.ModeUSB, Printer: "hplip", Scanner: "airsane", SaneBackends: []string{"hpaio"},
		Blobs:         []device.BlobRef{{ID: "hplip-plugin", Version: "3.24.4", Optional: true}},
		AcceptedEULAs: []device.EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: blobs.TextSHA256([]byte(blobtest.License))}},
	}
	spec.Hash = device.SpecHash(spec)
	r := &reporter{desired: device.DesiredState{
		Hash:       spec.Hash,
		Queue:      &device.QueueSpec{Name: "Printer", Driver: "auto", Schemes: []string{"hp", "usb"}, Media: "iso_a4_210x297mm"},
		Browse:     device.BrowseTypes,
		StageEULAs: []device.EULARef{{ID: "hplip-plugin", Version: "3.24.4"}},
	}}
	b, dir := newSim(t, "hp-legacy-mfp", r)
	if err := b.Apply(ctx(t), spec); err != nil {
		t.Fatal(err)
	}

	o := observe(t, b)
	if o.Endpoints.PrinterURI != "" || !strings.HasPrefix(o.Endpoints.ScannerURL, "http://127.0.0.1:") {
		t.Errorf("Observe().Endpoints = %+v, want no printer URI (PAPPL) and a scanner URL", o.Endpoints)
	}

	rep := r.wait(t, "queue ok", func(rep device.AgentReport) bool { return rep.Queue != nil && rep.Queue.State == device.QueueOK })
	if !strings.HasPrefix(rep.Queue.PrinterURI, "ipp://127.0.0.1:") || !strings.HasPrefix(rep.Queue.DeviceURI, "cups:hp:") {
		t.Errorf("QueueResult = %+v, want a 127.0.0.1 printer URI and a cups:hp: device", rep.Queue)
	}
	if rep.AppliedHash != spec.Hash {
		t.Errorf("AppliedHash = %q, want %q", rep.AppliedHash, spec.Hash)
	}

	pdf := samplePDF(t)
	pc := printing.New(rep.Queue.PrinterURI)
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

// TestProbeStagesEULA: before any config, the probe-mode agent stages the
// licence text the wizard shows.
func TestProbeStagesEULA(t *testing.T) {
	spec := &device.Spec{Mode: device.ModeProbe}
	spec.Hash = device.SpecHash(spec)
	r := &reporter{desired: device.DesiredState{
		Hash: spec.Hash, Browse: device.BrowseTypes, StageEULAs: []device.EULARef{{ID: "hplip-plugin", Version: "3.24.4"}},
	}}
	b, _ := newSim(t, "hp-legacy-mfp", r)
	if err := b.Apply(ctx(t), spec); err != nil {
		t.Fatal(err)
	}
	rep := r.wait(t, "the EULA text", func(rep device.AgentReport) bool {
		return len(rep.EULAs) == 1 && rep.EULAs[0].State == device.EULAAvailable
	})
	if rep.EULAs[0].Text != blobtest.License {
		t.Errorf("EULAs = %+v", rep.EULAs)
	}
}

func TestSwitchAndNilStopServers(t *testing.T) {
	b, _ := newSim(t, "ippusb-mfp", &reporter{})
	if err := b.Apply(ctx(t), &device.Spec{Mode: device.ModeUSB, Printer: "ipp-usb", Scanner: "ipp-usb"}); err != nil {
		t.Fatal(err)
	}
	first := observe(t, b).Endpoints
	oldPrinter, oldScanner := hostPort(t, first.PrinterURI), hostPort(t, first.ScannerURL)

	// The same spec again keeps the servers.
	if err := b.Apply(ctx(t), &device.Spec{Mode: device.ModeUSB, Printer: "ipp-usb", Scanner: "ipp-usb"}); err != nil {
		t.Fatal(err)
	}
	if got := observe(t, b).Endpoints; got != first {
		t.Errorf("re-applying the same spec changed endpoints: %+v -> %+v", first, got)
	}

	if err := b.Apply(ctx(t), &device.Spec{Mode: device.ModeUSB, Printer: "ipp-usb", Scanner: ""}); err != nil {
		t.Fatal(err)
	}
	second := observe(t, b).Endpoints
	if second.ScannerURL != "" || second.PrinterURI == "" || second.PrinterURI == first.PrinterURI {
		t.Errorf("endpoints after switch = %+v", second)
	}
	if !refused(oldPrinter) || !refused(oldScanner) {
		t.Error("old servers still accept connections after the spec switch")
	}

	if err := b.Apply(ctx(t), nil); err != nil {
		t.Fatal(err)
	}
	if !refused(hostPort(t, second.PrinterURI)) {
		t.Error("printer still accepts connections after Apply(nil)")
	}
	if o := observe(t, b); o.Phase != device.PhaseAbsent || o.Endpoints != (device.Endpoints{}) {
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
		return b.Apply(context.Background(), &device.Spec{Mode: device.ModeUSB, Printer: "ipp-usb", Scanner: "ipp-usb"})
	})
	select {
	case <-entered: // a report is now in flight, blocked on m
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never reported")
	}
	within("Apply(other spec)", func() error {
		return b.Apply(context.Background(), &device.Spec{Mode: device.ModeProbe})
	})
	within("Apply(nil)", func() error { return b.Apply(context.Background(), nil) })
}

func TestScanDelay(t *testing.T) {
	const delay = time.Second
	b := New(Options{Fixture: "ippusb-mfp", DataDir: t.TempDir(), ScanDelay: delay})
	b.SetReporter((&reporter{}).report)
	t.Cleanup(func() { _ = b.Apply(context.Background(), nil) })
	if err := b.Apply(ctx(t), &device.Spec{Mode: device.ModeUSB, Printer: "ipp-usb", Scanner: "ipp-usb"}); err != nil {
		t.Fatal(err)
	}
	sc, err := scanning.NewMFP(observe(t, b).Endpoints.ScannerURL)
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
