package pappl_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/pappl"
	"github.com/ThePhaseless/pagewire/internal/pappl/papplfake"
	"github.com/ThePhaseless/pagewire/internal/printing"
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

// dev is the HP printer a queue is bound to.
func dev(serial, deviceID string) device.USBDevice {
	return device.USBDevice{
		VendorID: "03f0", ProductID: "2b17", Serial: serial, Product: "HP LaserJet 1020",
		Interfaces: []device.USBInterface{{Class: 7, SubClass: 1, Protocol: 2}}, DeviceID: deviceID,
	}
}

// reconcile runs Reconcile for the one want w bound to d.
func reconcile(t *testing.T, c *pappl.Client, w device.QueueSpec, d device.USBDevice) device.QueueResult {
	t.Helper()
	w.Device = d
	rs := pappl.Reconcile(ctx(t), c, []device.QueueSpec{w})
	if len(rs) != 1 || rs[0].ID != w.ID {
		t.Fatalf("results = %+v, want one for %q", rs, w.ID)
	}
	return rs[0]
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
		ID: strings.ToLower(name), Name: name, Location: "Living room", Driver: "auto", Schemes: []string{"usb"},
		Defaults: device.PrintSettings{Media: "iso_a4_210x297mm", Sides: "one-sided", ColorMode: "auto", Quality: "normal"},
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
	r := reconcile(t, c, want("Living room printer"), dev("ABC123", hpID))
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
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	before := onlyPrinter(t, fake)
	// A fresh client (agent restart) must adopt the existing queue too.
	r := reconcile(t, pappl.New(c.URI()), w, dev("ABC123", hpID))
	mustOK(t, r)
	if after := onlyPrinter(t, fake); after.ID != before.ID {
		t.Errorf("printer recreated: id %d -> %d", before.ID, after.ID)
	}
}

// On real hardware every Find-Devices forks each CUPS backend and claims the
// printer's USB interface, so a queue that is already right must not ask again.
func TestExistingQueueSkipsDiscovery(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	w := want("P")
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	before := onlyPrinter(t, fake)
	for range 3 {
		r := reconcile(t, c, w, dev("ABC123", hpID))
		mustOK(t, r)
		if r.DeviceURI != before.DeviceURI {
			t.Errorf("DeviceURI = %q, want %q", r.DeviceURI, before.DeviceURI)
		}
	}
	if n := fake.FindDevicesCalls(); n != 1 {
		t.Errorf("Find-Devices calls = %d, want 1 (the one that created the queue)", n)
	}
	if after := onlyPrinter(t, fake); after.ID != before.ID {
		t.Errorf("printer recreated: id %d -> %d", before.ID, after.ID)
	}
}

// A queue on a less preferred scheme still moves once the preferred one is listed.
func TestQueueOnLesserSchemeRediscovers(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	mustOK(t, reconcile(t, c, want("P"), dev("ABC123", hpID))) // Schemes: [usb]
	w := want("P")
	w.Schemes = []string{"hp", "usb"}
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	if p := onlyPrinter(t, fake); p.DeviceURI != "cups:hp:/usb/HP_LaserJet_1020?serial=ABC123" {
		t.Errorf("device uri = %q", p.DeviceURI)
	}
}

// An hplip queue whose hp backend never lists the printer settles on usb:
// the listing offered nothing better, and the client remembers it.
func TestQueueOnOnlyListedLesserSchemeSettles(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices()[:1], Drivers: hpDrivers})
	w := want("P")
	w.Schemes = []string{"hp", "usb"}
	for range 4 {
		mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	}
	if n := fake.FindDevicesCalls(); n != 1 {
		t.Errorf("Find-Devices calls = %d, want 1", n)
	}
}

// A driver change on a settled queue recreates it with the bound device's ID.
func TestSettledRecreateSendsDeviceID(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	w := want("P")
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	w.Driver, w.DriverPinned = "hp_laserjet_1020", true
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	if p := onlyPrinter(t, fake); p.Driver != w.Driver || p.DeviceID != hpID {
		t.Errorf("printer = %+v, want driver %s and device ID %q", p, w.Driver, hpID)
	}
	if n := fake.FindDevicesCalls(); n != 1 {
		t.Errorf("Find-Devices calls = %d, want 1", n)
	}
}

// Without a device serial, a queue on another printer's serial is not settled.
func TestQueueOnOtherSerialRediscoversWithoutInventorySerial(t *testing.T) {
	devs := []papplfake.Device{
		{URI: "cups:usb://HP/LaserJet%201022?serial=OTHER", DeviceID: "MFG:HP;MDL:LaserJet 1022;CLS:PRINTER;"},
		{URI: "cups:usb://HP/LaserJet%201020", DeviceID: hpID},
	}
	fake, c := newSystem(t, papplfake.Options{Devices: devs, Drivers: hpDrivers})
	w := want("P")
	w.DeviceURI, w.Driver = devs[0].URI, "hp_laserjet_1020"
	mustOK(t, reconcile(t, c, w, dev("", hpID)))
	mustOK(t, reconcile(t, c, want("P"), dev("", hpID)))
	if p := onlyPrinter(t, fake); p.DeviceURI != devs[1].URI {
		t.Errorf("device uri = %q, want %q", p.DeviceURI, devs[1].URI)
	}
}

// A queue on PAPPL's own usb:// URI is not settled: the retrofit apps print
// through the CUPS backends only.
func TestQueueOnPAPPLOwnURIRediscovers(t *testing.T) {
	own := "usb://HP/LaserJet%201020?serial=ABC123"
	fake, c := newSystem(t, papplfake.Options{Schemes: []string{"cups", "usb"}, Devices: hpDevices(), Drivers: hpDrivers})
	w := want("P")
	w.DeviceURI, w.Driver = own, "hp_laserjet_1020"
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	mustOK(t, reconcile(t, c, want("P"), dev("ABC123", hpID)))
	if p := onlyPrinter(t, fake); p.DeviceURI != hpDevices()[0].URI {
		t.Errorf("device uri = %q, want %q", p.DeviceURI, hpDevices()[0].URI)
	}
}

// PAPPL lists its own usb:// entry for the same printer beside the CUPS
// backend's. The retrofit apps print through the CUPS backends only, and a
// second matching entry made the queue flip between the two URIs.
func TestPAPPLOwnSchemesNotListed(t *testing.T) {
	devs := append([]papplfake.Device{{URI: "usb://HP/LaserJet%201020?serial=ABC123", Info: "HP LaserJet 1020 (USB)", DeviceID: hpID}}, hpDevices()...)
	fake, c := newSystem(t, papplfake.Options{Devices: devs, Drivers: hpDrivers})
	mustOK(t, reconcile(t, c, want("P"), dev("ABC123", hpID)))
	if p := onlyPrinter(t, fake); p.DeviceURI != hpDevices()[0].URI {
		t.Errorf("device uri = %q, want %q", p.DeviceURI, hpDevices()[0].URI)
	}
}

func TestRenameDeletesAndCreates(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	mustOK(t, reconcile(t, c, want("Old"), dev("ABC123", hpID)))
	old := onlyPrinter(t, fake)
	mustOK(t, reconcile(t, c, want("New"), dev("ABC123", hpID)))
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
	mustOK(t, reconcile(t, c, want("P"), dev("ABC123", hpID)))
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
	mustOK(t, reconcile(t, c, want("P"), dev("ABC123", hpID)))
	p := onlyPrinter(t, fake)
	if p.ID == old.ID || p.DeviceURI != hpDevices()[0].URI {
		t.Errorf("printer = %+v, old %+v", p, old)
	}
}

func TestDriverFallbackThroughFindDrivers(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers, NoAutoDriver: true})
	r := reconcile(t, c, want("P"), dev("ABC123", hpID))
	mustOK(t, r)
	if p := onlyPrinter(t, fake); p.Driver != "hp_laserjet_1020" {
		t.Errorf("driver = %q", p.Driver)
	}
	// The fallback driver is kept on the next run (want.Driver stays auto).
	id := onlyPrinter(t, fake).ID
	mustOK(t, reconcile(t, c, want("P"), dev("ABC123", hpID)))
	if onlyPrinter(t, fake).ID != id {
		t.Error("fallback-driver queue recreated on second run")
	}
}

func TestCupsPrefixStrippedForMatchingKeptInCreate(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices()[:1], Drivers: hpDrivers})
	r := reconcile(t, c, want("P"), dev("ABC123", hpID)) // Schemes: [usb], URI cups:usb://...
	mustOK(t, r)
	if p := onlyPrinter(t, fake); !strings.HasPrefix(p.DeviceURI, "cups:usb://") || !strings.HasPrefix(r.DeviceURI, "cups:usb://") {
		t.Errorf("device uri = %q / %q", p.DeviceURI, r.DeviceURI)
	}
}

func TestHPPreferredOverUSB(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	w := want("P")
	w.Schemes = []string{"hp", "usb"}
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
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
	mustOK(t, reconcile(t, c, want("P"), dev("ABC123", hpID)))
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
	mustOK(t, reconcile(t, c, want("P"), dev("", hpID)))
	if p := onlyPrinter(t, fake); p.DeviceURI != devs[1].URI {
		t.Errorf("device uri = %q", p.DeviceURI)
	}
}

func TestSingleCandidateFallback(t *testing.T) {
	devs := []papplfake.Device{{URI: "cups:usb://Generic/Printer", DeviceID: "MFG:HP;MDL:Something Else;"}}
	fake, c := newSystem(t, papplfake.Options{Devices: devs, Drivers: hpDrivers})
	mustOK(t, reconcile(t, c, want("P"), device.USBDevice{}))
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
			r := reconcile(t, c, want("P"), dev("ABC123", hpID))
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
	r := reconcile(t, c, w, device.USBDevice{})
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
	r := reconcile(t, c, w, device.USBDevice{})
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
	r := reconcile(t, c, w, device.USBDevice{})
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
	w.Defaults.Sides, w.Defaults.ColorMode, w.Defaults.Quality = "two-sided-long-edge", "monochrome", "high"
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	p := onlyPrinter(t, fake)
	if p.Location != "Living room" || p.Defaults.Media != w.Defaults.Media || p.Defaults.Sides != w.Defaults.Sides ||
		p.Defaults.ColorMode != w.Defaults.ColorMode || p.Defaults.Quality != w.Defaults.Quality {
		t.Errorf("printer = %+v", p)
	}
	ps, err := c.Printers(ctx(t))
	if err != nil || len(ps) != 1 || ps[0].Caps.QualityDefault != "high" || ps[0].Location != "Living room" {
		t.Errorf("Printers() = %+v, %v", ps, err)
	}
}

func TestMediaChangeUpdatesWithoutRecreate(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	w := want("P")
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	before := onlyPrinter(t, fake)
	w.Defaults.Media = "na_letter_8.5x11in"
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	after := onlyPrinter(t, fake)
	if after.ID != before.ID || after.Defaults.Media != "na_letter_8.5x11in" {
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
	r := reconcile(t, c, want("Living room printer"), dev("ABC123", hpID))
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
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	if p := onlyPrinter(t, fake); p.ID == old.ID || p.Driver != "hp_laserjet_1020" {
		t.Errorf("printer = %+v, old %+v", p, old)
	}
}

// An index driver the running app does not list (image drift) falls back to
// the app's own choice, and the fallback queue is kept on later runs.
func TestUnlistedIndexDriverFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		noAuto     bool
		wantDriver string
	}{
		{"to auto", false, "auto"},
		{"to Find-Drivers", true, "hp_laserjet_1020"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers, NoAutoDriver: tc.noAuto})
			w := want("P")
			w.Driver = "hp--laserjet-1020--en" // not listed by this app
			r := reconcile(t, c, w, dev("ABC123", hpID))
			mustOK(t, r)
			mustFallback(t, r, w.Driver)
			first := onlyPrinter(t, fake)
			if first.Driver != tc.wantDriver {
				t.Fatalf("driver = %q, want %q", first.Driver, tc.wantDriver)
			}
			r = reconcile(t, c, w, dev("ABC123", hpID))
			mustOK(t, r)
			mustFallback(t, r, w.Driver)
			if p := onlyPrinter(t, fake); p.ID != first.ID {
				t.Errorf("fallback queue recreated: id %d -> %d", first.ID, p.ID)
			}
		})
	}
}

// A driver the user pinned (device.driverName) never falls back.
func TestPinnedUnlistedDriverIsError(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	w := want("P")
	w.Driver, w.DriverPinned = "hp--laserjet-1020--en", true
	r := reconcile(t, c, w, dev("ABC123", hpID))
	if r.State != device.QueueError || !strings.Contains(r.Message, "hp--laserjet-1020--en") {
		t.Errorf("result = %+v", r)
	}
	if len(fake.Printers()) != 0 {
		t.Errorf("printers = %+v", fake.Printers())
	}
}

// A pinned driver the app does not list keeps the queue that still works and
// reports the error, instead of deleting the queue first.
func TestPinnedUnlistedDriverKeepsQueue(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	w := want("P")
	w.Driver = "hp_laserjet_1020"
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	old := onlyPrinter(t, fake)
	w.Driver, w.DriverPinned = "hp--laserjet-1020--en", true
	r := reconcile(t, c, w, dev("ABC123", hpID))
	if r.State != device.QueueError || !strings.Contains(r.Message, "hp--laserjet-1020--en") || r.PrinterURI == "" {
		t.Errorf("result = %+v", r)
	}
	if p := onlyPrinter(t, fake); p.ID != old.ID || p.Driver != "hp_laserjet_1020" {
		t.Errorf("printer = %+v, want the queue %d kept", p, old.ID)
	}
}

func mustFallback(t *testing.T, r device.QueueResult, driver string) {
	t.Helper()
	if !strings.Contains(r.Message, driver) {
		t.Errorf("message = %q, want the fallback from %s", r.Message, driver)
	}
}

// A queue made with driver auto (by an earlier version) is kept for an index
// driver, so an upgrade does not recreate it; a pinned driver replaces it.
func TestAutoQueueKeptUnlessPinned(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	mustOK(t, reconcile(t, c, want("P"), dev("ABC123", hpID)))
	old := onlyPrinter(t, fake)
	w := want("P")
	w.Driver = "hp_laserjet_1020"
	r := reconcile(t, c, w, dev("ABC123", hpID))
	mustOK(t, r)
	mustFallback(t, r, w.Driver)
	if p := onlyPrinter(t, fake); p.ID != old.ID || p.Driver != "auto" {
		t.Errorf("printer = %+v, want the auto queue %d kept", p, old.ID)
	}
	w.DriverPinned = true
	r = reconcile(t, c, w, dev("ABC123", hpID))
	mustOK(t, r)
	if r.Message != "" {
		t.Errorf("message = %q, want none for the driver in use", r.Message)
	}
	if p := onlyPrinter(t, fake); p.ID == old.ID || p.Driver != "hp_laserjet_1020" {
		t.Errorf("printer = %+v, want a new queue with the pinned driver", p)
	}
}

// Names with spaces, non-ASCII and '%' must survive the URI round trip.
func TestOddQueueNamePrints(t *testing.T) {
	out := t.TempDir()
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers, OutputDir: out})
	r := reconcile(t, c, want("Büro 50%"), dev("ABC123", hpID))
	mustOK(t, r)
	mustOK(t, reconcile(t, c, want("Büro 50%"), dev("ABC123", hpID)))
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
	r := reconcile(t, c, want("P"), dev("ABC123", hpID))
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

// --- several queues per printer application ---

const otherID = "MFG:HP;MDL:LaserJet P1005;CLS:PRINTER;"

// twoDevices lists the HP 1020 (serial ABC123) and a P1005 (serial XYZ789).
func twoDevices() []papplfake.Device {
	return []papplfake.Device{
		hpDevices()[0],
		{URI: "cups:usb://HP/LaserJet%20P1005?serial=XYZ789", Info: "HP LaserJet P1005 USB", DeviceID: otherID},
	}
}

// twoWants are queues A on the 1020 and B on the P1005.
func twoWants() []device.QueueSpec {
	a, b := want("A"), want("B")
	a.Device, b.Device = dev("ABC123", hpID), dev("XYZ789", otherID)
	b.Device.ProductID = "3d17"
	return []device.QueueSpec{a, b}
}

func mustAllOK(t *testing.T, rs []device.QueueResult, wants []device.QueueSpec) {
	t.Helper()
	if len(rs) != len(wants) {
		t.Fatalf("results = %+v, want %d", rs, len(wants))
	}
	for i, r := range rs {
		if r.ID != wants[i].ID || r.State != device.QueueOK {
			t.Fatalf("result %d = %+v, want ok for %q", i, r, wants[i].ID)
		}
	}
}

func byName(ps []papplfake.Printer) map[string]papplfake.Printer {
	out := map[string]papplfake.Printer{}
	for _, p := range ps {
		out[p.Name] = p
	}
	return out
}

func TestTwoQueuesKeptStaleDeleted(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: twoDevices(), Drivers: hpDrivers})
	if _, err := c.CreatePrinter(ctx(t), "stale", hpDevices()[0].URI, "hp_laserjet_1020", ""); err != nil {
		t.Fatal(err)
	}
	ws := twoWants()
	rs := pappl.Reconcile(ctx(t), c, ws)
	mustAllOK(t, rs, ws)
	ps := byName(fake.Printers())
	if len(ps) != 2 || ps["A"].DeviceURI != twoDevices()[0].URI || ps["B"].DeviceURI != twoDevices()[1].URI {
		t.Errorf("printers = %+v, want A and B on their own devices", ps)
	}
	if rs[0].PrinterURI == rs[1].PrinterURI || !strings.HasSuffix(rs[1].PrinterURI, "/ipp/print/B") {
		t.Errorf("printer URIs = %q, %q", rs[0].PrinterURI, rs[1].PrinterURI)
	}
}

// Two queues created in one cycle share one Find-Devices, and neither asks again.
func TestTwoNewQueuesAskFindDevicesOnce(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: twoDevices(), Drivers: hpDrivers})
	ws := twoWants()
	for range 4 {
		mustAllOK(t, pappl.Reconcile(ctx(t), c, ws), ws)
	}
	if n := fake.FindDevicesCalls(); n != 1 {
		t.Errorf("Find-Devices calls = %d, want 1", n)
	}
}

// A second printer that is switched off (the app lists only the first) stays
// pending: it must not take its sibling's device through the MFG/MDL or the
// single-candidate fallback, and the settled sibling is never touched.
func TestSettledQueueBesideUnlistedOne(t *testing.T) {
	for name, offID := range map[string]string{"same model": hpID, "other model": otherID} {
		t.Run(name, func(t *testing.T) {
			fake, c := newSystem(t, papplfake.Options{Devices: hpDevices()[:1], Drivers: hpDrivers})
			ws := twoWants()
			ws[1].Device.Serial, ws[1].Device.DeviceID = "OFF1", offID
			mustOK(t, pappl.Reconcile(ctx(t), c, ws[:1])[0])
			before := onlyPrinter(t, fake)
			for range 3 {
				rs := pappl.Reconcile(ctx(t), c, ws)
				if rs[0].State != device.QueueOK || rs[0].DeviceURI != before.DeviceURI || rs[0].ID != "a" {
					t.Errorf("settled result = %+v", rs[0])
				}
				if rs[1].State != device.QueuePending || rs[1].ID != "b" || !strings.Contains(rs[1].Message, "not listed") {
					t.Errorf("unlisted result = %+v, want pending", rs[1])
				}
			}
			if after := onlyPrinter(t, fake); after.ID != before.ID || after.Name != "A" {
				t.Errorf("printer = %+v, want queue %d kept", after, before.ID)
			}
		})
	}
}

// A queue the app does not list asks Find-Devices once per 30 s, not every cycle.
func TestUnlistedQueueFindDevicesBackoff(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Drivers: hpDrivers})
	now := time.Unix(1_000_000, 0)
	pappl.SetNow(c, func() time.Time { return now })
	for _, step := range []struct {
		at    time.Duration
		finds int
	}{{0, 1}, {time.Second, 1}, {10 * time.Second, 1}, {31 * time.Second, 2}, {40 * time.Second, 2}} {
		now = time.Unix(1_000_000, 0).Add(step.at)
		r := reconcile(t, c, want("P"), dev("ABC123", hpID))
		if r.State != device.QueuePending {
			t.Errorf("t=%v: result = %+v, want pending", step.at, r)
		}
		if n := fake.FindDevicesCalls(); n != step.finds {
			t.Errorf("t=%v: Find-Devices calls = %d, want %d", step.at, n, step.finds)
		}
	}
}

// A failed Find-Devices is not a "not listed" answer, so it does not back off.
func TestFindDevicesErrorNoBackoff(t *testing.T) {
	fake := papplfake.New(papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	failFind := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m goipp.Message
		if failFind && m.DecodeBytes(body) == nil && goipp.Op(m.Code) == pappl.OpFindDevices {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := pappl.New("ipp://" + srv.Listener.Addr().String() + "/ipp/system")
	if r := reconcile(t, c, want("P"), dev("ABC123", hpID)); r.State != device.QueueError {
		t.Fatalf("result = %+v, want error", r)
	}
	failFind = false
	mustOK(t, reconcile(t, c, want("P"), dev("ABC123", hpID)))
}

// A queue rebound to another device asks Find-Devices again, once, even when
// neither printer has a serial and the queue sits on a lesser scheme, so only
// the remembered pick (keyed by the device) settles it.
func TestRebindAsksFindDevicesOnce(t *testing.T) {
	devs := []papplfake.Device{
		{URI: "cups:usb://HP/LaserJet%201020", DeviceID: hpID},
		{URI: "cups:usb://HP/LaserJet%20P1005", DeviceID: otherID},
	}
	fake, c := newSystem(t, papplfake.Options{Devices: devs, Drivers: hpDrivers})
	w := want("P")
	w.Schemes = []string{"hp", "usb"}
	other := dev("", otherID)
	other.ProductID = "3d17"
	mustOK(t, reconcile(t, c, w, dev("", hpID)))
	mustOK(t, reconcile(t, c, w, dev("", hpID)))
	for range 3 {
		mustOK(t, reconcile(t, c, w, other))
	}
	if n := fake.FindDevicesCalls(); n != 2 {
		t.Errorf("Find-Devices calls = %d, want 2", n)
	}
	if p := onlyPrinter(t, fake); p.DeviceURI != devs[1].URI {
		t.Errorf("device uri = %q, want %q", p.DeviceURI, devs[1].URI)
	}
}

func TestConnectionRefusedUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	c := pappl.New("ipp://" + srv.Listener.Addr().String() + "/ipp/system")
	srv.Close()
	ws := twoWants()
	rs := pappl.Reconcile(ctx(t), c, ws)
	for i, r := range rs {
		if r.ID != ws[i].ID || r.State != device.QueueError || !r.Unreachable || r.Message == "" {
			t.Errorf("result %d = %+v, want an unreachable error", i, r)
		}
	}
	if len(rs) != 2 {
		t.Errorf("results = %+v, want 2", rs)
	}
}

// --- defaults ---

// monoCaps is a Samsung-like queue: one-sided, monochrome, A4 and Letter.
func monoCaps() device.PrinterCaps {
	return device.PrinterCaps{
		Media: []string{"iso_a4_210x297mm", "na_letter_8.5x11in"}, MediaDefault: "na_letter_8.5x11in",
		Sides: []string{"one-sided"}, SidesDefault: "one-sided",
		ColorModes: []string{"monochrome"}, ColorModeDefault: "monochrome",
		Qualities: []string{"draft", "normal", "high"}, QualityDefault: "normal",
		Resolutions: []int{300, 600}, ResolutionDefault: 600,
	}
}

// An unsupported colorMode auto is left out of Set-Printer-Attributes, which
// the queue would refuse; the controller warns about it, not the agent.
func TestUnsupportedColorModeNotSet(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers, Caps: monoCaps()})
	r := reconcile(t, c, want("P"), dev("ABC123", hpID))
	mustOK(t, r)
	if len(r.Warnings) != 0 {
		t.Errorf("warnings = %q, want none", r.Warnings)
	}
	if p := onlyPrinter(t, fake); p.Defaults.ColorMode != "monochrome" || p.Defaults.Media != "iso_a4_210x297mm" || p.Location != "Living room" {
		t.Errorf("printer = %+v, want monochrome kept and media and location set", p)
	}
	if n := fake.SetCalls(); n != 1 {
		t.Errorf("Set-Printer-Attributes calls = %d, want 1", n)
	}
}

func TestNewKindDefaultsSet(t *testing.T) {
	caps := monoCaps()
	caps.MediaSources, caps.MediaSourceDefault = []string{"auto", "manual"}, "auto"
	caps.OutputBins, caps.OutputBinDefault = []string{"face-down", "face-up"}, "face-up"
	caps.NumberUp, caps.NumberUpDefault = []int{1, 2}, 1
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers, Caps: caps})
	w := want("P")
	w.Defaults.MediaSource, w.Defaults.OutputBin, w.Defaults.NumberUp, w.Defaults.Resolution = "manual", "face-down", 2, 300
	r := reconcile(t, c, w, dev("ABC123", hpID))
	mustOK(t, r)
	if len(r.Warnings) != 0 {
		t.Errorf("warnings = %q, want none (number-up must never be sent)", r.Warnings)
	}
	d := onlyPrinter(t, fake).Defaults
	if d.MediaSource != "manual" || d.Media != "iso_a4_210x297mm" || d.OutputBin != "face-down" || d.Resolution != 300 || d.NumberUp != 1 {
		t.Errorf("defaults = %+v, want tray manual, A4, face-down, 300 dpi, number-up unchanged", d)
	}
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	if n := fake.SetCalls(); n != 1 {
		t.Errorf("Set-Printer-Attributes calls = %d, want 1 (the read-back matches)", n)
	}
}

// Real PAPPL rewrites its state file on every Set-Printer-Attributes, so an
// agent cycle with unchanged wants must not send one.
func TestSetDefaultsOnlyOnChange(t *testing.T) {
	caps := monoCaps()
	caps.NumberUp, caps.NumberUpDefault = []int{1, 2}, 1
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers, Caps: caps})
	w := want("P")
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	if n := fake.SetCalls(); n != 1 {
		t.Fatalf("first reconcile: Set-Printer-Attributes calls = %d, want 1", n)
	}
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	if n := fake.SetCalls(); n != 1 {
		t.Errorf("unchanged wants: Set-Printer-Attributes calls = %d, want 1", n)
	}
	w.Defaults.Quality = "high"
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	if n := fake.SetCalls(); n != 2 {
		t.Errorf("one changed kind: Set-Printer-Attributes calls = %d, want 2", n)
	}
	w.Defaults.NumberUp = 2 // PAPPL cannot set it, so it never triggers a set
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	if n := fake.SetCalls(); n != 2 {
		t.Errorf("unsettable kind: Set-Printer-Attributes calls = %d, want 2", n)
	}
}

// A queue whose location and defaults already match gets no
// Set-Printer-Attributes, and a change sends only the changed attribute.
func TestSetDefaultsSendsOnlyChanges(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	w := want("P")
	w.Location = ""
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	if n := fake.SetCalls(); n != 0 {
		t.Fatalf("nothing differs: Set-Printer-Attributes calls = %d, want 0", n)
	}
	w.Defaults.Quality = "high"
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	if sets := fake.Sets(); len(sets) != 1 || !slices.Equal(sets[0], []string{"print-quality-default"}) {
		t.Errorf("sets = %q, want one with print-quality-default only", sets)
	}
}

// PAPPL refuses the whole request for one value it names in the unsupported
// group: the rest is sent again without it, and the refused value is not sent
// again while it stays wanted, but stays a warning.
func TestRefusedDefaultRetriedWithout(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers, Refuse: []string{"print-quality-default"}})
	w := want("P")
	w.Defaults.Quality, w.Defaults.Sides = "high", "two-sided-long-edge"
	r := reconcile(t, c, w, dev("ABC123", hpID))
	mustOK(t, r)
	wantRefused := []device.Dropped{{Option: device.OptionQuality, Value: "high", Using: "normal"}}
	if len(r.Warnings) != 0 || !slices.Equal(r.Refused, wantRefused) {
		t.Errorf("warnings %q, refused %+v; want none and %+v", r.Warnings, r.Refused, wantRefused)
	}
	if sets := fake.Sets(); len(sets) != 2 || slices.Contains(sets[1], "print-quality-default") || !slices.Contains(sets[1], "sides-default") {
		t.Errorf("sets = %q, want a retry without print-quality-default", sets)
	}
	if p := onlyPrinter(t, fake); p.Defaults.Sides != "two-sided-long-edge" || p.Location != "Living room" || p.Defaults.Quality != "normal" {
		t.Errorf("printer = %+v, want sides and location set, quality kept", p)
	}

	r = reconcile(t, c, w, dev("ABC123", hpID))
	if n := fake.SetCalls(); n != 2 || !slices.Equal(r.Refused, wantRefused) {
		t.Errorf("next cycle: Set-Printer-Attributes calls = %d, refused %+v; want 2 and the refusal kept", n, r.Refused)
	}
	w.Defaults.Quality = "draft" // a new wanted value is tried again
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	if sets := fake.Sets(); len(sets) != 3 || !slices.Equal(sets[2], []string{"print-quality-default"}) {
		t.Errorf("sets = %q, want the new quality sent once", sets)
	}
	w.Defaults.Quality = "normal" // the queue's own: nothing to send, nothing to warn about
	if r := reconcile(t, c, w, dev("ABC123", hpID)); fake.SetCalls() != 3 || len(r.Warnings) != 0 || len(r.Refused) != 0 {
		t.Errorf("quality normal: Set-Printer-Attributes calls = %d, warnings %q, refused %+v; want 3 and none", fake.SetCalls(), r.Warnings, r.Refused)
	}
}

// ps-printer-app refuses output-bin-default without naming it: each attribute
// is then tried alone, so the others still get set.
func TestUnnamedRefusalTriesEachAlone(t *testing.T) {
	caps := monoCaps()
	caps.OutputBins, caps.OutputBinDefault = []string{"face-down", "face-up"}, "face-up"
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers, Caps: caps, Refuse: []string{"output-bin-default"}})
	w := want("P")
	w.Defaults.ColorMode, w.Defaults.OutputBin = "", "face-down"
	r := reconcile(t, c, w, dev("ABC123", hpID))
	mustOK(t, r)
	wantRefused := []device.Dropped{{Option: device.OptionOutputBin, Value: "face-down", Using: "face-up"}}
	if len(r.Warnings) != 0 || !slices.Equal(r.Refused, wantRefused) {
		t.Errorf("warnings %q, refused %+v; want none and %+v", r.Warnings, r.Refused, wantRefused)
	}
	if p := onlyPrinter(t, fake); p.Location != "Living room" || p.Defaults.Media != "iso_a4_210x297mm" || p.Defaults.OutputBin != "face-up" {
		t.Errorf("printer = %+v, want location and media set, output bin kept", p)
	}
	n := fake.SetCalls()
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	if fake.SetCalls() != n {
		t.Errorf("next cycle: Set-Printer-Attributes calls %d -> %d, want none", n, fake.SetCalls())
	}
}

// A refused media-col-default names only the members that differ from the
// queue's own: media rides along unchanged and is not a refusal.
func TestRefusedMediaColNamesChangedMembers(t *testing.T) {
	caps := monoCaps()
	caps.MediaDefault = "iso_a4_210x297mm"
	caps.MediaSources, caps.MediaSourceDefault = []string{"auto", "manual"}, "auto"
	_, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers, Caps: caps, Refuse: []string{"media-col-default"}})
	w := want("P")
	w.Defaults.ColorMode, w.Defaults.MediaSource = "", "manual"
	r := reconcile(t, c, w, dev("ABC123", hpID))
	mustOK(t, r)
	wantRefused := []device.Dropped{{Option: device.OptionMediaSource, Value: "manual", Using: "auto"}}
	if len(r.Warnings) != 0 || !slices.Equal(r.Refused, wantRefused) {
		t.Errorf("warnings %q, refused %+v; want none and %+v", r.Warnings, r.Refused, wantRefused)
	}
}

// PAPPL 1.4.12 never reads printer-location back: it is sent once per queue
// and value, not every cycle.
func TestLocationNotReadBackSentOnce(t *testing.T) {
	fake, c := newSystem(t, papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers, DropLocation: true})
	w := want("P")
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	if n := fake.SetCalls(); n != 1 {
		t.Errorf("Set-Printer-Attributes calls = %d, want 1", n)
	}
	w.Location = "Office"
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	mustOK(t, reconcile(t, c, w, dev("ABC123", hpID)))
	if sets := fake.Sets(); len(sets) != 2 || !slices.Equal(sets[1], []string{"printer-location"}) {
		t.Errorf("sets = %q, want the new location sent once", sets)
	}
}

func TestSetDefaultsFailureWarns(t *testing.T) {
	fake := papplfake.New(papplfake.Options{Devices: hpDevices(), Drivers: hpDrivers})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m goipp.Message
		if m.DecodeBytes(body) == nil && goipp.Op(m.Code) == goipp.OpSetPrinterAttributes {
			rsp := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusErrorInternal, m.RequestID)
			rsp.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
			rsp.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
			w.Header().Set("Content-Type", goipp.ContentType)
			_ = rsp.Encode(w)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	r := reconcile(t, pappl.New("ipp://"+srv.Listener.Addr().String()+"/ipp/system"), want("P"), dev("ABC123", hpID))
	mustOK(t, r)
	if len(r.Warnings) != 1 || !strings.HasPrefix(r.Warnings[0], "Could not set the printer's defaults: ") || r.PrinterURI == "" {
		t.Errorf("result = %+v, want ok with one defaults warning", r)
	}
}
