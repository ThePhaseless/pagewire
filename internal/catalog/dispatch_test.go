package catalog

import (
	"reflect"
	"testing"

	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
)

func autoPrinter() config.Printer {
	return config.Printer{ID: "p", Name: "P", Enabled: true, Mode: config.ModeUSB, Driver: config.Auto}
}

func autoScanner() config.Scanner {
	return config.Scanner{ID: "s", Name: "S", Enabled: true, Mode: config.ModeUSB, Backend: config.BackendAuto}
}

// first returns a pointer to the fixture's first device.
func first(fixture string) *device.USBDevice { return &Fixture(fixture).Devices[0] }

func TestRoles(t *testing.T) {
	for _, tc := range []struct {
		fixture          string
		printer, scanner bool
	}{
		{"ippusb-mfp", true, true},
		{"samsung-scx4300-mfp", true, true}, // xerox_mfp in the SANE index
		{"epson-stylus-photo", true, true},  // epson2 in the SANE index
		{"brother-pcl-laser", true, false},
		{"hp-legacy-mfp", true, false}, // hpaio is not in the SANE USB index
		{"unknown-printer", true, false},
		{"no-printer", false, false},
		{"canon-lide-scanner", false, true}, // genesys in the SANE index, no printer interface
	} {
		p, s := Default().Roles(*first(tc.fixture))
		if p != tc.printer || s != tc.scanner {
			t.Errorf("%s: roles %v %v, want %v %v", tc.fixture, p, s, tc.printer, tc.scanner)
		}
	}
	// A class-6 (still image) interface alone makes a scanner, a camera included.
	cam := device.USBDevice{VendorID: "1234", ProductID: "5678", Interfaces: []device.USBInterface{{Class: 6, SubClass: 1, Protocol: 1}}}
	if p, s := Default().Roles(cam); p || !s {
		t.Errorf("class 6: roles %v %v", p, s)
	}
	// A driver-index match alone does not make a printer: CUPS and ipp-usb need class 7.
	noClass := *first("samsung-scx4300-mfp")
	noClass.Interfaces = []device.USBInterface{{Class: 255, SubClass: 255, Protocol: 255}}
	if p, _ := Default().Roles(noClass); p {
		t.Error("Samsung without its class-7 interface is a printer")
	}
}

func TestDecidePrinterFixtures(t *testing.T) {
	none := []string{}
	for _, tc := range []struct {
		fixture, rule, family, driver string
		required, optional            []string
	}{
		{"ippusb-mfp", device.RuleIPPOverUSB, "ipp-usb", "", none, none},
		// The acceptance fixture: SpliX in the legacy image.
		{"samsung-scx4300-mfp", device.RuleModel, "legacy", "samsung--scx-4300--2-0-0-en", none, none},
		{"hp-legacy-mfp", device.RuleModel, "hplip", "hp--laserjet-professional-m-1132-mfp--requires-proprietary-plugin-en", []string{"hplip-plugin"}, none},
		{"epson-stylus-photo", device.RuleModel, "gutenprint", "epson--stylus-photo-r-300--en", none, none},
		{"canon-pixma-mfp", device.RuleModel, "gutenprint", "canon--mg-3600--en", none, none},
		{"brother-pcl-laser", device.RuleModel, "legacy", "brother--hl-l-2350-dw--using-owl-maintain-brlaser-v-6-2-8-en", none, none},
		{"epson-inkjet-mfp", device.RuleModel, "legacy", "epson--et-2720--epson-inkjet-printer-driver-esc-p-r-for-linux-en", none, none},
		{"generic-postscript", device.RuleModel, "ps", "lexmark--ms-610-dn--en", none, none},
		{"postscript-unlisted", device.RuleCommandSet, "ps", "generic--postscript-printer--recommended-en", none, none},
		{"unknown-printer", device.RuleNone, "", "", none, none},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			d := first(tc.fixture)
			got := Default().DecidePrinter(d, autoPrinter())
			want := device.PrinterDecision{
				Family: tc.family, Driver: tc.driver, RequiredEULAs: tc.required, OptionalEULAs: tc.optional,
				Rule: tc.rule, Device: d,
			}
			if drv, ok := Default().Drivers.Lookup(tc.family, tc.driver); ok {
				want.DriverDescription = drv.Description
			} else if tc.driver != "" {
				t.Errorf("driver %s/%s is not in the index", tc.family, tc.driver)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got  %+v\nwant %+v", got, want)
			}
		})
	}
	if got := Default().DecidePrinter(first("samsung-scx4300-mfp"), autoPrinter()); got.DriverDescription != "Samsung SCX-4300, 2.0.0 (en)" {
		t.Errorf("SCX-4300 description %q", got.DriverDescription)
	}
}

func TestDecideScannerFixtures(t *testing.T) {
	def := Default().DefaultSaneBackends
	for _, tc := range []struct {
		fixture, family, rule string
		sane                  []string
	}{
		{"ippusb-mfp", "ipp-usb", device.RuleIPPOverUSB, []string{}},
		{"samsung-scx4300-mfp", "airsane", device.RuleSANEIndex, []string{"xerox_mfp"}},
		// 04b8:0806 is in the epson and epson2 USB ID lists of sane-backends, but
		// dll.conf enables only epson2 (epson is superseded).
		{"epson-stylus-photo", "airsane", device.RuleSANEIndex, []string{"epson2"}},
		{"canon-pixma-mfp", "airsane", device.RuleSANEIndex, []string{"pixma"}},
		{"canon-lide-scanner", "airsane", device.RuleSANEIndex, []string{"genesys"}},
		// Bound without scanner evidence (a selector, or v1's unbound rule): the
		// default backends, as v1 ran them.
		{"hp-legacy-mfp", "airsane", device.RuleStillImage, def},
		{"unknown-printer", "airsane", device.RuleStillImage, def},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			d := first(tc.fixture)
			got := Default().DecideScanner(d, autoScanner())
			want := device.ScannerDecision{Family: tc.family, SaneBackends: tc.sane, Rule: tc.rule, Device: d}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got  %+v\nwant %+v", got, want)
			}
		})
	}
	cam := &device.USBDevice{VendorID: "1234", ProductID: "5678", Interfaces: []device.USBInterface{{Class: 6, SubClass: 1, Protocol: 1}}}
	if got := Default().DecideScanner(cam, autoScanner()); got.Family != "airsane" || got.Rule != device.RuleStillImage || !reflect.DeepEqual(got.SaneBackends, def) {
		t.Errorf("class 6: got %+v", got)
	}
}

func TestDecideScannerOverrides(t *testing.T) {
	def := Default().DefaultSaneBackends
	for _, tc := range []struct {
		name, fixture, backend, family string
		sane                           []string
	}{
		{"airsane on ipp-usb uses defaults", "ippusb-mfp", "airsane", "airsane", def},
		{"airsane keeps the SANE index", "samsung-scx4300-mfp", "airsane", "airsane", []string{"xerox_mfp"}},
		{"ipp-usb drops SANE", "hp-legacy-mfp", "ipp-usb", "ipp-usb", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := autoScanner()
			s.Backend = tc.backend
			got := Default().DecideScanner(first(tc.fixture), s)
			if got.Family != tc.family || got.Rule != device.RuleOverride || !reflect.DeepEqual(got.SaneBackends, tc.sane) {
				t.Errorf("got %+v", got)
			}
		})
	}
}

func TestDecidePrinterOverrides(t *testing.T) {
	none, plugin := []string{}, []string{"hplip-plugin"}
	const scx = "samsung--scx-4300--2-0-0-en"
	for _, tc := range []struct {
		name, fixture, driver, driverName string
		family, wantDriver, rule          string
		required, optional                []string
	}{
		{"driver ps on ipp-usb", "ippusb-mfp", "ps", "", "ps", "", device.RuleOverride, none, none},
		// The plug-in belongs to the device, so an override still offers it, but
		// only a driver that needs it requires it.
		{"driver ipp-usb on hp", "hp-legacy-mfp", "ipp-usb", "", "ipp-usb", "", device.RuleOverride, none, plugin},
		{"driver ghostscript on hp", "hp-legacy-mfp", "ghostscript", "", "ghostscript", "", device.RuleOverride, none, plugin},
		{"driver legacy on hp", "hp-legacy-mfp", "legacy", "", "legacy", "", device.RuleOverride, none, plugin},
		{"driver hplip on hp keeps the plug-in driver", "hp-legacy-mfp", "hplip", "", "hplip", "hp--laserjet-professional-m-1132-mfp--requires-proprietary-plugin-en", device.RuleOverride, plugin, none},
		{"driver hplip on brother", "brother-pcl-laser", "hplip", "", "hplip", "", device.RuleOverride, none, plugin},
		{"driver gutenprint on unknown", "unknown-printer", "gutenprint", "", "gutenprint", "", device.RuleOverride, none, none},
		{"driver ghostscript matches in its family", "generic-postscript", "ghostscript", "", "ghostscript", "lexmark--ms-610-dn--postscript-en", device.RuleOverride, none, none},
		{"driver ghostscript falls back to the command set", "postscript-unlisted", "ghostscript", "", "ghostscript", "generic--postscript-printer--postscript-recommended-en", device.RuleOverride, none, none},
		{"driver legacy with driverName", "unknown-printer", "legacy", scx, "legacy", scx, device.RuleOverride, none, none},
		{"driver legacy with a driverName legacy does not list uses legacy's match", "samsung-scx4300-mfp", "legacy", "generic--postscript-printer--en", "legacy", scx, device.RuleOverride, none, none},
		{"driverName with driver auto", "generic-postscript", "auto", scx, "legacy", scx, device.RuleOverride, none, none},
		{"driverName in several families goes to the first in priority", "unknown-printer", "auto", "generic--postscript-printer--en", "ps", "generic--postscript-printer--en", device.RuleOverride, none, none},
		{"driverName nobody lists leaves the automatic choice", "samsung-scx4300-mfp", "auto", "acme--x-en", "legacy", scx, device.RuleModel, none, none},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := autoPrinter()
			p.Driver, p.DriverName = tc.driver, tc.driverName
			got := Default().DecidePrinter(first(tc.fixture), p)
			if got.Family != tc.family || got.Driver != tc.wantDriver || got.Rule != tc.rule ||
				!reflect.DeepEqual(got.RequiredEULAs, tc.required) || !reflect.DeepEqual(got.OptionalEULAs, tc.optional) {
				t.Errorf("got %+v", got)
			}
		})
	}
}

// commandIndex holds generic drivers only, so every device below is matched
// by its command set.
const commandIndex = `# pagewire driver index v1
gutenprint	generic--pcl-6-pcl-xl-printer--en	0	Generic	Generic PCL 6/PCL XL Printer		Generic PCL 6/PCL XL Printer (en)
ps	generic--pcl-6-pcl-xl-printer--recommended-en	1	Generic	PCL 6/PCL XL Printer		Generic PCL 6/PCL XL Printer (recommended) (en)
ps	generic--postscript-printer--recommended-en	1	Generic	PostScript Printer		Generic PostScript Printer (recommended) (en)
`

func TestDecideCommandSets(t *testing.T) {
	c := &Catalog{
		Printers: []Printer{
			{ID: "ipp-usb", Role: "ipp-usb"},
			{ID: "ps", App: "ps-printer-app"},
			{ID: "gutenprint", App: "gutenprint-printer-app"},
		},
		DriverPriority: []string{"ps", "gutenprint"},
		CommandSets: []CommandSet{
			{Commands: []string{"POSTSCRIPT", "POSTSCRIPT LEVEL 3"}, MFG: "Generic", MDL: "PostScript Printer"},
			{Commands: []string{"PCLXL", "PCL6"}, MFG: "Generic", MDL: "PCL 6/PCL XL Printer"},
		},
		Drivers: parseTestIndex(t, commandIndex),
	}
	dev := func(cmd string) *device.USBDevice {
		return &device.USBDevice{
			VendorID: "1234", ProductID: "5678",
			Interfaces: []device.USBInterface{{Class: 7, SubClass: 1, Protocol: 2}},
			DeviceID:   "MFG:X;MDL:Y;CMD:" + cmd + ";",
		}
	}
	const ps, pxl = "generic--postscript-printer--recommended-en", "generic--pcl-6-pcl-xl-printer--recommended-en"
	for _, tc := range []struct{ cmd, rule, driver string }{
		{"pjl, postscript", device.RuleCommandSet, ps},          // case-insensitive, spaces trimmed
		{"PCLXL,PostScript Level 3", device.RuleCommandSet, ps}, // the first commandSet wins, not the first token
		{"PWGRaster,pcl6", device.RuleCommandSet, pxl},
		{"PCLM,URF", device.RuleNone, ""}, // a token, not a substring
		{"", device.RuleNone, ""},
	} {
		got := c.DecidePrinter(dev(tc.cmd), autoPrinter())
		if got.Rule != tc.rule || got.Driver != tc.driver {
			t.Errorf("CMD %q: got %q %q, want %q %q", tc.cmd, got.Rule, got.Driver, tc.rule, tc.driver)
		}
	}
	// Restricted to a family, a set without a driver there gives way to the next.
	p := autoPrinter()
	p.Driver = "gutenprint"
	if got := c.DecidePrinter(dev("POSTSCRIPT,PCLXL"), p); got.Driver != "generic--pcl-6-pcl-xl-printer--en" {
		t.Errorf("gutenprint override: got %+v", got)
	}
	// IPP over USB wants exactly 7/1/4, not just subClass 1 and protocol 4.
	d := dev("ZPL")
	d.Interfaces = append(d.Interfaces, device.USBInterface{Class: 255, SubClass: 1, Protocol: 4})
	if got := c.DecidePrinter(d, autoPrinter()); got.Rule != device.RuleNone || got.Family != "" {
		t.Errorf("255/1/4: got %+v", got)
	}
	if got := c.DecideScanner(d, autoScanner()); got.Family != "airsane" {
		t.Errorf("255/1/4 scanner: got %+v", got)
	}
}

// Without MFG and MDL in the device ID, the USB descriptor strings are used.
func TestDecideDescriptorFallback(t *testing.T) {
	d := first("samsung-scx4300-mfp")
	d.DeviceID = "CMD:GDI;"
	if got := Default().DecidePrinter(d, autoPrinter()); got.Driver != "samsung--scx-4300--2-0-0-en" {
		t.Errorf("got %+v", got)
	}
}

func TestDecideSimulated(t *testing.T) {
	for _, d := range []*device.USBDevice{nil, first("hp-legacy-mfp")} {
		p := autoPrinter()
		p.Mode, p.Driver = config.ModeSimulated, "ps"
		want := device.PrinterDecision{Family: "sim", Rule: device.RuleSimulated, RequiredEULAs: []string{}, OptionalEULAs: []string{}}
		if got := Default().DecidePrinter(d, p); !reflect.DeepEqual(got, want) {
			t.Errorf("printer: got %+v", got)
		}
		s := autoScanner()
		s.Mode, s.Backend = config.ModeSimulated, "ipp-usb"
		wantS := device.ScannerDecision{Family: "sim", Rule: device.RuleSimulated, SaneBackends: []string{}}
		if got := Default().DecideScanner(d, s); !reflect.DeepEqual(got, wantS) {
			t.Errorf("scanner: got %+v", got)
		}
	}
}

// A usb entry with no bound device gets no family: nothing runs for it.
func TestDecideUnbound(t *testing.T) {
	want := device.PrinterDecision{Rule: device.RuleNone, RequiredEULAs: []string{}, OptionalEULAs: []string{}}
	if got := Default().DecidePrinter(nil, autoPrinter()); !reflect.DeepEqual(got, want) {
		t.Errorf("printer: got %+v", got)
	}
	if got := Default().DecideScanner(nil, autoScanner()); got.Family != "" || got.Device != nil || got.SaneBackends == nil {
		t.Errorf("scanner: got %+v", got)
	}
}

// The returned device is a copy: editing it never changes the caller's.
func TestDecideCopiesDevice(t *testing.T) {
	d := first("ippusb-mfp")
	p := Default().DecidePrinter(d, autoPrinter())
	s := Default().DecideScanner(d, autoScanner())
	for _, got := range []*device.USBDevice{p.Device, s.Device} {
		got.Serial = "x"
		got.Interfaces[0].Class = 1
	}
	if d.Serial == "x" || d.Interfaces[0].Class == 1 {
		t.Error("Decide aliased the device")
	}
}
