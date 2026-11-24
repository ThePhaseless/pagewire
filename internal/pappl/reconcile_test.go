package pappl_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThePhaseless/printscan/internal/device"
	"github.com/ThePhaseless/printscan/internal/pappl"
	"github.com/ThePhaseless/printscan/internal/pappl/papplfake"
	"github.com/ThePhaseless/printscan/internal/printing"
)

const hpID = "MFG:HP;MDL:LaserJet 1020;CMD:PJL,PCL;CLS:PRINTER;"

func newSystem(t *testing.T, opts papplfake.Options) (*papplfake.System, *pappl.Client) {
	t.Helper()
	fake := papplfake.New(opts)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return fake, pappl.New("ipp://" + srv.Listener.Addr().String() + "/ipp/system")
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func inv(serial, deviceID string) device.Inventory {
	return device.Inventory{Devices: []device.USBDevice{
		{VendorID: "046d", ProductID: "c31c", Product: "Keyboard", Interfaces: []device.USBInterface{{Class: 3}}},
		{
			VendorID: "03f0", ProductID: "2b17", Serial: serial, Product: "HP LaserJet 1020",
			Interfaces: []device.USBInterface{{Class: 7, SubClass: 1, Protocol: 2}}, DeviceID: deviceID,
		},
	}}
}

var hpDrivers = []papplfake.Driver{{Name: "hp_laserjet_1020", Description: "HP LaserJet 1020", MatchMFG: "HP"}}

func hpDevices() []papplfake.Device {
	return []papplfake.Device{
		{URI: "cups:usb://HP/LaserJet%201020?serial=ABC123", Info: "HP LaserJet 1020 USB", DeviceID: hpID},
		{URI: "cups:hp:/usb/HP_LaserJet_1020?serial=ABC123", Info: "HP LaserJet 1020 HPLIP", DeviceID: hpID},
	}
}

func want(name string) device.QueueSpec {
	return device.QueueSpec{
		Name: name, Location: "Living room", Driver: "auto", Schemes: []string{"usb"},
		Media: "iso_a4_210x297mm", Sides: "one-sided", ColorMode: "auto", Quality: "normal",
	}
}

func onlyPrinter(t *testing.T, fake *papplfake.System) papplfake.Printer {
	t.Helper()
	ps := fake.Printers()
	if len(ps) != 1 {
		t.Fatalf("printers = %+v, want exactly one", ps)
	}
	return ps[0]
}

func mustOK(t *testing.T, r device.QueueResult) {
	t.Helper()
	if r.State != device.QueueOK {
		t.Fatalf("result = %+v, want ok", r)
	}
}

func TestFreshCreate(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	r := pappl.Reconcile(ctx(t), c, want("Living room printer"), inv("ABC123", hpID))
	mustOK(t, r)
	p := onlyPrinter(t, fake)
	if p.Name != "Living room printer" || p.DeviceURI != "cups:usb://HP/LaserJet%201020?serial=ABC123" || p.Driver != "auto" {
		t.Errorf("printer = %+v", p)
	}
	if !strings.HasPrefix(r.PrinterURI, "ipp://127.0.0.1:") || !strings.HasSuffix(r.PrinterURI, "/ipp/print/Living_room_printer") {
		t.Errorf("PrinterURI = %q", r.PrinterURI)
	}
	if r.DeviceURI != p.DeviceURI {
		t.Errorf("DeviceURI = %q, want %q", r.DeviceURI, p.DeviceURI)
	}
}

func TestSecondRunIsNoOp(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	w := want("P")
	mustOK(t, pappl.Reconcile(ctx(t), c, w, inv("ABC123", hpID)))
	before := onlyPrinter(t, fake)
	// A fresh client (agent restart) must adopt the existing queue too.
	r := pappl.Reconcile(ctx(t), pappl.New(c.URI()), w, inv("ABC123", hpID))
	mustOK(t, r)
	if after := onlyPrinter(t, fake); after.ID != before.ID {
		t.Errorf("printer recreated: id %d -> %d", before.ID, after.ID)
	}
}

func TestRenameDeletesAndCreates(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	mustOK(t, pappl.Reconcile(ctx(t), c, want("Old"), inv("ABC123", hpID)))
	old := onlyPrinter(t, fake)
	mustOK(t, pappl.Reconcile(ctx(t), c, want("New"), inv("ABC123", hpID)))
	p := onlyPrinter(t, fake)
	if p.Name != "New" || p.ID == old.ID {
		t.Errorf("printer = %+v, old id %d", p, old.ID)
	}
}

func TestStaleExtraQueueRemoved(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	if _, err := c.CreatePrinter(ctx(t), "stale", hpDevices()[0].URI, "hp_laserjet_1020", ""); err != nil {
		t.Fatal(err)
	}
	mustOK(t, pappl.Reconcile(ctx(t), c, want("P"), inv("ABC123", hpID)))
	if p := onlyPrinter(t, fake); p.Name != "P" {
		t.Errorf("printer = %+v", p)
	}
}

func TestDifferentDeviceURIRecreated(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	if _, err := c.CreatePrinter(ctx(t), "P", "cups:usb://Other/Printer?serial=X", "hp_laserjet_1020", ""); err != nil {
		t.Fatal(err)
	}
	old := onlyPrinter(t, fake)
	mustOK(t, pappl.Reconcile(ctx(t), c, want("P"), inv("ABC123", hpID)))
	p := onlyPrinter(t, fake)
	if p.ID == old.ID || p.DeviceURI != hpDevices()[0].URI {
		t.Errorf("printer = %+v, old %+v", p, old)
	}
}

func TestDriverFallbackThroughFindDrivers(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers, NoAutoDriver: true})
	r := pappl.Reconcile(ctx(t), c, want("P"), inv("ABC123", hpID))
	mustOK(t, r)
	if p := onlyPrinter(t, fake); p.Driver != "hp_laserjet_1020" {
		t.Errorf("driver = %q", p.Driver)
	}
	// The fallback driver is kept on the next run (want.Driver stays auto).
	id := onlyPrinter(t, fake).ID
	mustOK(t, pappl.Reconcile(ctx(t), c, want("P"), inv("ABC123", hpID)))
	if onlyPrinter(t, fake).ID != id {
		t.Error("fallback-driver queue recreated on second run")
	}
}

func TestCupsPrefixStrippedForMatchingKeptInCreate(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices()[:1], Drivers: hpDrivers})
	r := pappl.Reconcile(ctx(t), c, want("P"), inv("ABC123", hpID)) // Schemes: [usb], URI cups:usb://...
	mustOK(t, r)
	if p := onlyPrinter(t, fake); !strings.HasPrefix(p.DeviceURI, "cups:usb://") || !strings.HasPrefix(r.DeviceURI, "cups:usb://") {
		t.Errorf("device uri = %q / %q", p.DeviceURI, r.DeviceURI)
	}
}

func TestHPPreferredOverUSB(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	w := want("P")
	w.Schemes = []string{"hp", "usb"}
	mustOK(t, pappl.Reconcile(ctx(t), c, w, inv("ABC123", hpID)))
	if p := onlyPrinter(t, fake); p.DeviceURI != "cups:hp:/usb/HP_LaserJet_1020?serial=ABC123" {
		t.Errorf("device uri = %q", p.DeviceURI)
	}
}

func TestSerialMatch(t *testing.T) {
	devs := []papplfake.Device{
		{URI: "cups:usb://HP/LaserJet%201020?serial=OTHER", DeviceID: hpID},
		{URI: "cups:usb://HP/LaserJet%201020?serial=abc123", DeviceID: hpID},
	}
	fake, c := newSystem(t, papplfake.Options{Devices: devs, Drivers: hpDrivers})
	mustOK(t, pappl.Reconcile(ctx(t), c, want("P"), inv("ABC123", hpID)))
	if p := onlyPrinter(t, fake); p.DeviceURI != devs[1].URI {
		t.Errorf("device uri = %q", p.DeviceURI)
	}
}

func TestMFGMDLMatchWithoutSerial(t *testing.T) {
	devs := []papplfake.Device{
		{URI: "cups:usb://HP/LaserJet%20P1005", DeviceID: "MFG:HP;MDL:LaserJet P1005;"},
		{URI: "cups:usb://HP/LaserJet%201020", DeviceID: "MANUFACTURER:hp;MODEL:laserjet 1020;"},
	}
	fake, c := newSystem(t, papplfake.Options{Devices: devs, Drivers: hpDrivers})
	mustOK(t, pappl.Reconcile(ctx(t), c, want("P"), inv("", hpID)))
	if p := onlyPrinter(t, fake); p.DeviceURI != devs[1].URI {
		t.Errorf("device uri = %q", p.DeviceURI)
	}
}

func TestSingleCandidateFallback(t *testing.T) {
	devs := []papplfake.Device{{URI: "cups:usb://Generic/Printer", DeviceID: "MFG:HP;MDL:Something Else;"}}
	fake, c := newSystem(t, papplfake.Options{Devices: devs, Drivers: hpDrivers})
	mustOK(t, pappl.Reconcile(ctx(t), c, want("P"), device.Inventory{}))
	if p := onlyPrinter(t, fake); p.DeviceURI != devs[0].URI {
		t.Errorf("device uri = %q", p.DeviceURI)
	}
}

func TestNoCandidatePending(t *testing.T) {
	for name, devs := range map[string][]papplfake.Device{
		"none listed":  nil,
		"wrong scheme": {{URI: "cups:socket://10.0.0.5", DeviceID: hpID}},
		"two others": {
			{URI: "cups:usb://A/B?serial=1", DeviceID: "MFG:A;MDL:B;"},
			{URI: "cups:usb://C/D?serial=2", DeviceID: "MFG:C;MDL:D;"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake, c := newSystem(t, papplfake.Options{Devices: devs, Drivers: hpDrivers})
			r := pappl.Reconcile(ctx(t), c, want("P"), inv("ABC123", hpID))
			if r.State != device.QueuePending || !strings.Contains(r.Message, "not listed") {
				t.Errorf("result = %+v", r)
			}
			if len(fake.Printers()) != 0 {
				t.Errorf("printers = %+v", fake.Printers())
			}
		})
	}
}

func TestExplicitDeviceURIAndDriver(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Drivers: hpDrivers}) // Find-Devices would list nothing
	w := want("it")
	w.DeviceURI, w.Driver, w.Schemes = "cups:ipp://127.0.0.1:18631/ipp/print", "hp_laserjet_1020", []string{"ipp"}
	r := pappl.Reconcile(ctx(t), c, w, device.Inventory{})
	mustOK(t, r)
	p := onlyPrinter(t, fake)
	if p.DeviceURI != w.DeviceURI || p.Driver != w.Driver || r.DeviceURI != w.DeviceURI {
		t.Errorf("printer = %+v, result %+v", p, r)
	}
}

func TestAutoWithoutDeviceIDIsError(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Drivers: hpDrivers})
	w := want("P")
	w.DeviceURI = "cups:ipp://127.0.0.1:18631/ipp/print"
	r := pappl.Reconcile(ctx(t), c, w, device.Inventory{})
	if r.State != device.QueueError || r.Message != "driver auto needs a device ID" {
		t.Errorf("result = %+v", r)
	}
	if len(fake.Printers()) != 0 {
		t.Errorf("printers = %+v", fake.Printers())
	}
}

func TestBareIPPURIRefused(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Drivers: hpDrivers})
	w := want("P")
	w.DeviceURI, w.Driver = "ipp://127.0.0.1:18631/ipp/print", "hp_laserjet_1020"
	r := pappl.Reconcile(ctx(t), c, w, device.Inventory{})
	if r.State != device.QueueError || !strings.Contains(r.Message, "smi55357-device-uri") {
		t.Errorf("result = %+v", r)
	}
	if len(fake.Printers()) != 0 {
		t.Errorf("printers = %+v", fake.Printers())
	}
}

func TestDefaultsApplied(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	w := want("P")
	w.Sides, w.ColorMode, w.Quality = "two-sided-long-edge", "monochrome", "high"
	mustOK(t, pappl.Reconcile(ctx(t), c, w, inv("ABC123", hpID)))
	p := onlyPrinter(t, fake)
	if p.Location != "Living room" || p.Media != w.Media || p.Sides != w.Sides || p.ColorMode != w.ColorMode || p.Quality != w.Quality {
		t.Errorf("printer = %+v", p)
	}
	ps, err := c.Printers(ctx(t))
	if err != nil || len(ps) != 1 || ps[0].Quality != "high" || ps[0].Location != "Living room" {
		t.Errorf("Printers() = %+v, %v", ps, err)
	}
}

func TestMediaChangeUpdatesWithoutRecreate(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	w := want("P")
	mustOK(t, pappl.Reconcile(ctx(t), c, w, inv("ABC123", hpID)))
	before := onlyPrinter(t, fake)
	w.Media = "na_letter_8.5x11in"
	mustOK(t, pappl.Reconcile(ctx(t), c, w, inv("ABC123", hpID)))
	after := onlyPrinter(t, fake)
	if after.ID != before.ID || after.Media != "na_letter_8.5x11in" {
		t.Errorf("before %+v after %+v", before, after)
	}
}

func TestFindDriversWithoutDeviceIDListsAll(t *testing.T) {
	drivers := append([]papplfake.Driver{{Name: "generic_ps", Description: "Generic PostScript Printer"}}, hpDrivers...)
	_, c := newSystem(t, papplfake.Options{Drivers: drivers})
	ds, err := c.FindDrivers(ctx(t), "")
	if err != nil || len(ds) != 2 || ds[0].Name != "generic_ps" || ds[0].Description != "Generic PostScript Printer" {
		t.Errorf("FindDrivers = %+v, %v", ds, err)
	}
	ds, err = c.FindDrivers(ctx(t), "MFG:Nobody;MDL:X;")
	if err != nil || len(ds) != 0 {
		t.Errorf("FindDrivers(no match) = %+v, %v", ds, err)
	}
}

// The sim backend prints to the fake queue with internal/printing.
func TestFakeQueuePrintsAndCompletes(t *testing.T) {
	out := t.TempDir()
	_, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers, OutputDir: out})
	r := pappl.Reconcile(ctx(t), c, want("Living room printer"), inv("ABC123", hpID))
	mustOK(t, r)

	pc := printing.New(r.PrinterURI)
	attrs, err := pc.Attributes(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if attrs.State != "idle" || !slices.Contains(attrs.Formats, "application/pdf") || !slices.Contains(attrs.Media, "iso_a4_210x297mm") {
		t.Errorf("attributes = %+v", attrs)
	}
	doc := []byte("%PDF-1.4 test")
	id, err := pc.Submit(ctx(t), "doc.pdf", "web", "application/pdf", bytes.NewReader(doc), printing.PrintOptions{Copies: 1})
	if err != nil {
		t.Fatal(err)
	}
	job, err := pc.Job(ctx(t), id)
	if err != nil || job.State != "completed" || job.CreatedAt.IsZero() {
		t.Errorf("job = %+v, %v", job, err)
	}
	jobs, err := pc.Jobs(ctx(t), 20)
	if err != nil || len(jobs) != 1 || jobs[0].ID != id {
		t.Errorf("jobs = %+v, %v", jobs, err)
	}
	got, err := os.ReadFile(filepath.Join(out, "Living_room_printer-"+strconv.Itoa(id)+".bin")) //nolint:gosec // test temp dir
	if err != nil || !bytes.Equal(got, doc) {
		t.Errorf("output = %q, %v", got, err)
	}
	if err := pc.Cancel(ctx(t), id); !errors.Is(err, printing.ErrFinished) {
		t.Errorf("cancel finished job = %v", err)
	}
}

func TestDifferentExplicitDriverRecreated(t *testing.T) {
	drivers := append([]papplfake.Driver{{Name: "generic_pcl", Description: "Generic PCL"}}, hpDrivers...)
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: drivers})
	if _, err := c.CreatePrinter(ctx(t), "P", hpDevices()[0].URI, "generic_pcl", ""); err != nil {
		t.Fatal(err)
	}
	old := onlyPrinter(t, fake)
	w := want("P")
	w.Driver = "hp_laserjet_1020"
	mustOK(t, pappl.Reconcile(ctx(t), c, w, inv("ABC123", hpID)))
	if p := onlyPrinter(t, fake); p.ID == old.ID || p.Driver != "hp_laserjet_1020" {
		t.Errorf("printer = %+v, old %+v", p, old)
	}
}

// Names with spaces, non-ASCII and '%' must survive the URI round trip.
func TestOddQueueNamePrints(t *testing.T) {
	out := t.TempDir()
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers, OutputDir: out})
	r := pappl.Reconcile(ctx(t), c, want("Büro 50%"), inv("ABC123", hpID))
	mustOK(t, r)
	mustOK(t, pappl.Reconcile(ctx(t), c, want("Büro 50%"), inv("ABC123", hpID)))
	if _, err := url.Parse(r.PrinterURI); err != nil {
		t.Fatalf("PrinterURI %q: %v", r.PrinterURI, err)
	}
	id, err := printing.New(r.PrinterURI).Submit(ctx(t), "a.pdf", "web", "application/pdf", strings.NewReader("%PDF-"), printing.PrintOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p := onlyPrinter(t, fake); p.Jobs != 1 {
		t.Errorf("printer = %+v", p)
	}
	if _, err := os.Stat(filepath.Join(out, onlyPrinter(t, fake).Resource+"-"+strconv.Itoa(id)+".bin")); err != nil {
		t.Error(err)
	}
}

// A slow upload must not block other requests to the fake.
func TestUploadDoesNotBlockSystem(t *testing.T) {
	_, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	r := pappl.Reconcile(ctx(t), c, want("P"), inv("ABC123", hpID))
	mustOK(t, r)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := printing.New(r.PrinterURI).Submit(ctx(t), "a.pdf", "web", "application/pdf", pr, printing.PrintOptions{})
		done <- err
	}()
	if _, err := pw.Write([]byte("%PDF-")); err != nil { // the upload has reached the fake
		t.Fatal(err)
	}
	qctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.Printers(qctx); err != nil {
		t.Errorf("Get-Printers during upload: %v", err)
	}
	_ = pw.Close()
	if err := <-done; err != nil {
		t.Errorf("submit: %v", err)
	}
}
