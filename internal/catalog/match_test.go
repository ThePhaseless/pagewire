package catalog

import (
	"reflect"
	"slices"
	"testing"

	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
)

func printerEntry(id string, usb *config.USB) config.Printer {
	p := autoPrinter()
	p.ID, p.Name, p.USB = id, id, usb
	return p
}

func scannerEntry(id string, usb *config.USB) config.Scanner {
	s := autoScanner()
	s.ID, s.Name, s.USB = id, id, usb
	return s
}

func sel(fixture, serial string) *config.USB {
	d := first(fixture)
	return &config.USB{VendorID: d.VendorID, ProductID: d.ProductID, Serial: serial}
}

// office is the office fixture on node "", reversed, so Bind's own order is tested.
func office() map[string][]device.USBDevice {
	devs := Fixture("office").Devices
	slices.Reverse(devs)
	return map[string][]device.USBDevice{"": devs}
}

// bound maps entry id to "node product" for a compact comparison.
func bound(m map[string]Placed) map[string]string {
	out := map[string]string{}
	for id, p := range m {
		out[id] = p.Node + " " + p.Device.Product
	}
	return out
}

func TestBindSelectors(t *testing.T) {
	cfg := config.Default("x")
	cfg.Printers = []config.Printer{
		printerEntry("samsung", sel("samsung-scx4300-mfp", "FAKE0008")),
		printerEntry("hp", sel("ippusb-mfp", "")),
		printerEntry("wrong-serial", sel("samsung-scx4300-mfp", "other")),
		printerEntry("absent", &config.USB{VendorID: "1234", ProductID: "0001"}),
	}
	cfg.Scanners = []config.Scanner{
		scannerEntry("lide", sel("canon-lide-scanner", "FAKE0011")),
		// A selector binds whatever the device's roles: the user chose it.
		scannerEntry("keyboard", &config.USB{VendorID: "046D", ProductID: "C31C"}),
	}
	nodes := office()
	printers, scanners := Default().Bind(&cfg, nodes)
	want := map[string]string{"samsung": " SCX-4300 Series", "hp": " OfficeJet Pro 8020 series"}
	if got := bound(printers); !reflect.DeepEqual(got, want) {
		t.Errorf("printers %v, want %v", got, want)
	}
	want = map[string]string{"lide": " CanoScan LiDE 100", "keyboard": " USB Keyboard"}
	if got := bound(scanners); !reflect.DeepEqual(got, want) {
		t.Errorf("scanners %v, want %v", got, want)
	}
	// The placed device is a copy.
	p := printers["samsung"]
	p.Device.Interfaces[0].Class = 1
	if slices.ContainsFunc(nodes[""], func(d device.USBDevice) bool { return d.Interfaces[0].Class == 1 }) {
		t.Error("Bind aliased the fixture")
	}
}

// Two devices of one model: the selector with a serial takes its device, and
// the one without takes the other instead of the first.
func TestBindSerialFirst(t *testing.T) {
	a, b := *first("brother-pcl-laser"), *first("brother-pcl-laser")
	a.Serial, a.Address = "A", 1
	b.Serial, b.Address = "B", 2
	cfg := config.Default("x")
	cfg.Printers = []config.Printer{
		printerEntry("any", sel("brother-pcl-laser", "")),
		printerEntry("a", sel("brother-pcl-laser", "A")),
	}
	printers, _ := Default().Bind(&cfg, map[string][]device.USBDevice{"": {b, a}})
	if printers["a"].Device.Serial != "A" || printers["any"].Device.Serial != "B" {
		t.Errorf("got %+v", printers)
	}
}

// A migrated v1 file: one printer and one scanner without selectors share the
// first printer in bus/address order.
func TestBindUnboundMFP(t *testing.T) {
	cfg := config.Default("x")
	cfg.Printers = []config.Printer{printerEntry("printer", nil)}
	cfg.Scanners = []config.Scanner{scannerEntry("scanner", nil)}
	printers, scanners := Default().Bind(&cfg, office())
	if got := bound(printers)["printer"]; got != " OfficeJet Pro 8020 series" {
		t.Errorf("printer on %q", got)
	}
	if got := bound(scanners)["scanner"]; got != " OfficeJet Pro 8020 series" {
		t.Errorf("scanner on %q", got)
	}

	// Entries with selectors claim their devices first.
	cfg.Printers = append(cfg.Printers, printerEntry("hp", sel("ippusb-mfp", "FAKE0005")))
	printers, scanners = Default().Bind(&cfg, office())
	if got := bound(printers)["printer"]; got != " SCX-4300 Series" {
		t.Errorf("printer beside a claimed HP on %q", got)
	}
	if got := bound(scanners)["scanner"]; got != " SCX-4300 Series" {
		t.Errorf("scanner beside a claimed HP on %q", got)
	}
}

// The unbound scanner is the unbound printer's device (v1), before a
// standalone scanner that comes first on the bus.
func TestBindUnboundScannerPrefersPrinter(t *testing.T) {
	lide := *first("canon-lide-scanner")
	lide.Address = 1
	nodes := map[string][]device.USBDevice{"": {*first("samsung-scx4300-mfp"), lide}}
	cfg := config.Default("x")
	cfg.Printers = []config.Printer{printerEntry("printer", nil)}
	cfg.Scanners = []config.Scanner{scannerEntry("scanner", nil)}
	_, scanners := Default().Bind(&cfg, nodes)
	if got := bound(scanners)["scanner"]; got != " SCX-4300 Series" {
		t.Errorf("scanner on %q", got)
	}

	// A scanner selector on the printer's device sends it to the next scanner.
	cfg.Scanners = append(cfg.Scanners, scannerEntry("samsung", sel("samsung-scx4300-mfp", "")))
	printers, scanners := Default().Bind(&cfg, nodes)
	if got := bound(scanners); got["scanner"] != " CanoScan LiDE 100" || got["samsung"] != " SCX-4300 Series" {
		t.Errorf("scanners %v", got)
	}
	// Claims are per role: a scanner selector leaves the printer where it was.
	if got := bound(printers)["printer"]; got != " SCX-4300 Series" {
		t.Errorf("printer on %q", got)
	}

	// Without a printer entry, the first device with the scanner role.
	cfg.Printers, cfg.Scanners = nil, cfg.Scanners[:1]
	_, scanners = Default().Bind(&cfg, map[string][]device.USBDevice{"": {*first("brother-pcl-laser"), *first("samsung-scx4300-mfp")}})
	if got := bound(scanners)["scanner"]; got != " SCX-4300 Series" {
		t.Errorf("scanner alone on %q", got)
	}
}

// v1 ran AirSane on the printer's device whatever it was; the unbound rule keeps that.
func TestBindUnboundScannerWithoutRole(t *testing.T) {
	cfg := config.Default("x")
	cfg.Printers = []config.Printer{printerEntry("printer", nil)}
	cfg.Scanners = []config.Scanner{scannerEntry("scanner", nil)}
	_, scanners := Default().Bind(&cfg, map[string][]device.USBDevice{"": Fixture("hp-legacy-mfp").Devices})
	if got := bound(scanners)["scanner"]; got != " HP LaserJet Professional M1132 MFP" {
		t.Errorf("scanner unbound: %v", scanners)
	}
}

func TestBindTwoNodes(t *testing.T) {
	nodes := map[string][]device.USBDevice{
		"node-b": Fixture("ippusb-mfp").Devices,
		"node-a": slices.Concat(Fixture("no-printer").Devices, Fixture("canon-lide-scanner").Devices),
		"node-c": Fixture("samsung-scx4300-mfp").Devices,
	}
	cfg := config.Default("x")
	cfg.Printers = []config.Printer{printerEntry("printer", nil), printerEntry("samsung", sel("samsung-scx4300-mfp", ""))}
	cfg.Scanners = []config.Scanner{scannerEntry("scanner", nil), scannerEntry("lide", sel("canon-lide-scanner", ""))}
	printers, scanners := Default().Bind(&cfg, nodes)
	want := map[string]string{"printer": "node-b OfficeJet Pro 8020 series", "samsung": "node-c SCX-4300 Series"}
	if got := bound(printers); !reflect.DeepEqual(got, want) {
		t.Errorf("printers %v, want %v", got, want)
	}
	want = map[string]string{"scanner": "node-b OfficeJet Pro 8020 series", "lide": "node-a CanoScan LiDE 100"}
	if got := bound(scanners); !reflect.DeepEqual(got, want) {
		t.Errorf("scanners %v, want %v", got, want)
	}
}

func TestBindDisabledAndSimulated(t *testing.T) {
	cfg := config.Default("x")
	off := printerEntry("off", sel("ippusb-mfp", ""))
	off.Enabled = false
	sim := printerEntry("sim", nil)
	sim.Mode = config.ModeSimulated
	simS := scannerEntry("sim-scanner", nil)
	simS.Mode = config.ModeSimulated
	cfg.Printers = []config.Printer{off, sim, printerEntry("printer", nil)}
	cfg.Scanners = []config.Scanner{simS}
	printers, scanners := Default().Bind(&cfg, office())
	// The disabled entry is not bound, but still keeps its device from the
	// unbound printer.
	if got, want := bound(printers), map[string]string{"printer": " SCX-4300 Series"}; !reflect.DeepEqual(got, want) {
		t.Errorf("printers %v, want %v", got, want)
	}
	if len(scanners) != 0 {
		t.Errorf("scanners %v", scanners)
	}
	if p, s := Default().Bind(nil, office()); len(p) != 0 || len(s) != 0 || p == nil || s == nil {
		t.Errorf("nil config: %v %v", p, s)
	}
}

// An unbound entry never takes a device that is ignored for its role.
func TestBindUnboundSkipsIgnored(t *testing.T) {
	cfg := config.Default("x")
	cfg.Printers = []config.Printer{printerEntry("printer", nil)}
	cfg.Scanners = []config.Scanner{scannerEntry("scanner", nil)}
	cfg.IgnoredDevices = []config.IgnoredDevice{
		{VendorID: "03f0", ProductID: "2754", Roles: []string{config.RolePrinter, config.RoleScanner}},
		{VendorID: "04e8", ProductID: "342e", Serial: "FAKE0008", Roles: []string{config.RoleScanner}},
	}
	printers, scanners := Default().Bind(&cfg, office())
	if got := bound(printers)["printer"]; got != " SCX-4300 Series" {
		t.Errorf("printer on %q", got)
	}
	if got := bound(scanners)["scanner"]; got != " CanoScan LiDE 100" {
		t.Errorf("scanner on %q", got)
	}
}

func TestDetect(t *testing.T) {
	cfg := config.Default("x")
	off := scannerEntry("hp-scanner", sel("ippusb-mfp", "FAKE0005"))
	off.Enabled = false
	cfg.Printers = []config.Printer{printerEntry("hp", sel("ippusb-mfp", "FAKE0005"))}
	cfg.Scanners = []config.Scanner{off}
	cfg.IgnoredDevices = []config.IgnoredDevice{{VendorID: "04e8", ProductID: "342e", Roles: []string{config.RoleScanner, config.RolePrinter}}}
	nodes := office()
	nodes["node-a"] = Fixture("brother-pcl-laser").Devices
	got := Default().Detect(&cfg, nodes)
	type row struct {
		node, product     string
		canPrint, canScan bool
		printer, scanner  string
		ignored           []string
	}
	var rows []row
	for _, d := range got {
		rows = append(rows, row{d.Node, d.Device.Product, d.CanPrint, d.CanScan, d.PrinterID, d.ScannerID, d.Ignored})
	}
	want := []row{
		{"", "USB Keyboard", false, false, "", "", []string{}},
		// A disabled entry still configures its device.
		{"", "OfficeJet Pro 8020 series", true, true, "hp", "hp-scanner", []string{}},
		{"", "SCX-4300 Series", true, true, "", "", []string{config.RolePrinter, config.RoleScanner}},
		{"", "CanoScan LiDE 100", false, true, "", "", []string{}},
		{"node-a", "HL-L2350DW series", true, false, "", "", []string{}},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("got  %+v\nwant %+v", rows, want)
	}

	// No config: every device, nothing configured or ignored.
	for _, d := range Default().Detect(nil, office()) {
		if d.PrinterID != "" || d.ScannerID != "" || d.Ignored == nil || len(d.Ignored) != 0 {
			t.Errorf("nil config: %+v", d)
		}
	}
	if got := Default().Detect(nil, nil); got == nil || len(got) != 0 {
		t.Errorf("no nodes: %v", got)
	}
}
