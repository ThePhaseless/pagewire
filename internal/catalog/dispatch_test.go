package catalog

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
)

func auto() config.Device {
	return config.Device{Mode: config.ModeUSB, Driver: config.Auto, Scanner: config.Auto}
}

func TestDecideFixtures(t *testing.T) {
	def := Default().DefaultSaneBackends
	none := []string{}
	for _, tc := range []struct {
		fixture, rule, printer, driver, scanner string
		sane, required, optional                []string
	}{
		{"ippusb-mfp", device.RuleIPPOverUSB, "ipp-usb", "", "ipp-usb", none, none, none},
		// The acceptance fixture: SpliX in the legacy image, scanner through xerox_mfp.
		{"samsung-scx4300-mfp", device.RuleModel, "legacy", "samsung--scx-4300--2-0-0-en", "airsane", []string{"xerox_mfp"}, none, none},
		{"hp-legacy-mfp", device.RuleModel, "hplip", "hp--laserjet-professional-m-1132-mfp--requires-proprietary-plugin-en", "airsane", def, []string{"hplip-plugin"}, none},
		// 04b8:0806 is in the epson and epson2 USB ID lists of sane-backends, but
		// dll.conf enables only epson2 (epson is superseded).
		{"epson-stylus-photo", device.RuleModel, "gutenprint", "epson--stylus-photo-r-300--en", "airsane", []string{"epson2"}, none, none},
		{"canon-pixma-mfp", device.RuleModel, "gutenprint", "canon--mg-3600--en", "airsane", []string{"pixma"}, none, none},
		{"brother-pcl-laser", device.RuleModel, "legacy", "brother--hl-l-2350-dw--using-owl-maintain-brlaser-v-6-2-8-en", "airsane", def, none, none},
		{"epson-inkjet-mfp", device.RuleModel, "legacy", "epson--et-2720--epson-inkjet-printer-driver-esc-p-r-for-linux-en", "airsane", def, none, none},
		{"generic-postscript", device.RuleModel, "ps", "lexmark--ms-610-dn--en", "airsane", def, none, none},
		{"postscript-unlisted", device.RuleCommandSet, "ps", "generic--postscript-printer--recommended-en", "airsane", def, none, none},
		{"unknown-printer", device.RuleNone, "", "", "airsane", def, none, none},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			inv := Fixture(tc.fixture)
			got, err := Decide(inv, auto())
			if err != nil {
				t.Fatal(err)
			}
			want := device.Decision{
				Printer: tc.printer, Driver: tc.driver, Scanner: tc.scanner, SaneBackends: tc.sane,
				RequiredEULAs: tc.required, OptionalEULAs: tc.optional, Rule: tc.rule, Device: &inv.Devices[0],
			}
			if drv, ok := Default().Drivers.Lookup(tc.printer, tc.driver); ok {
				want.DriverDescription = drv.Description
			} else if tc.driver != "" {
				t.Errorf("driver %s/%s is not in the index", tc.printer, tc.driver)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got  %+v\nwant %+v", got, want)
			}
		})
	}
	got, _ := Decide(Fixture("samsung-scx4300-mfp"), auto())
	if got.DriverDescription != "Samsung SCX-4300, 2.0.0 (en)" {
		t.Errorf("SCX-4300 description %q", got.DriverDescription)
	}
}

func TestDecideNoPrinter(t *testing.T) {
	if _, err := Decide(Fixture("no-printer"), auto()); !errors.Is(err, ErrNoPrinter) {
		t.Errorf("no-printer: %v", err)
	}
	if _, err := Decide(device.Inventory{}, auto()); !errors.Is(err, ErrNoPrinter) {
		t.Errorf("empty inventory: %v", err)
	}
}

func TestDecideSelector(t *testing.T) {
	brother := Fixture("brother-pcl-laser").Devices[0]
	inv := device.Inventory{Devices: slices.Concat(Fixture("no-printer").Devices, Fixture("ippusb-mfp").Devices, []device.USBDevice{brother})}

	sel := auto()
	got, err := Decide(inv, sel)
	if err != nil || got.Rule != device.RuleIPPOverUSB {
		t.Errorf("no selector: first printer-class device wins, got %+v, %v", got, err)
	}

	sel.USB = &config.USB{VendorID: "04f9", ProductID: "0446", Serial: brother.Serial}
	if got, err := Decide(inv, sel); err != nil || got.Printer != "legacy" || got.Device.Serial != brother.Serial {
		t.Errorf("selector: got %+v, %v", got, err)
	}
	sel.USB.Serial = ""
	if got, err := Decide(inv, sel); err != nil || got.Printer != "legacy" {
		t.Errorf("selector without serial: got %+v, %v", got, err)
	}
	for _, bad := range []config.USB{
		{VendorID: "04f9", ProductID: "0446", Serial: "other"},
		{VendorID: "04f9", ProductID: "ffff"},
		{VendorID: "1234", ProductID: "0446"},
	} {
		sel.USB = &bad
		if _, err := Decide(inv, sel); !errors.Is(err, ErrNoPrinter) {
			t.Errorf("selector %+v: %v", bad, err)
		}
	}
}

func TestDecideOverrides(t *testing.T) {
	def := Default().DefaultSaneBackends
	none, plugin := []string{}, []string{"hplip-plugin"}
	const scx = "samsung--scx-4300--2-0-0-en"
	for _, tc := range []struct {
		name, fixture, driver, driverName, scanner string
		printer, wantDriver, wantScanner, rule     string
		sane, required, optional                   []string
	}{
		{"driver ps on ipp-usb", "ippusb-mfp", "ps", "", "auto", "ps", "", "ipp-usb", device.RuleOverride, none, none, none},
		// The plug-in belongs to the device, so an override still offers it, but
		// only a driver that needs it requires it.
		{"driver ipp-usb on hp", "hp-legacy-mfp", "ipp-usb", "", "auto", "ipp-usb", "", "airsane", device.RuleOverride, def, none, plugin},
		{"driver ghostscript on hp", "hp-legacy-mfp", "ghostscript", "", "auto", "ghostscript", "", "airsane", device.RuleOverride, def, none, plugin},
		{"driver legacy on hp", "hp-legacy-mfp", "legacy", "", "auto", "legacy", "", "airsane", device.RuleOverride, def, none, plugin},
		{"driver hplip on hp keeps the plug-in driver", "hp-legacy-mfp", "hplip", "", "auto", "hplip", "hp--laserjet-professional-m-1132-mfp--requires-proprietary-plugin-en", "airsane", device.RuleOverride, def, plugin, none},
		{"driver hplip on brother", "brother-pcl-laser", "hplip", "", "auto", "hplip", "", "airsane", device.RuleOverride, def, none, plugin},
		{"driver gutenprint on unknown", "unknown-printer", "gutenprint", "", "auto", "gutenprint", "", "airsane", device.RuleOverride, def, none, none},
		{"driver ghostscript matches in its family", "generic-postscript", "ghostscript", "", "auto", "ghostscript", "lexmark--ms-610-dn--postscript-en", "airsane", device.RuleOverride, def, none, none},
		{"driver ghostscript falls back to the command set", "postscript-unlisted", "ghostscript", "", "auto", "ghostscript", "generic--postscript-printer--postscript-recommended-en", "airsane", device.RuleOverride, def, none, none},
		{"driver legacy with driverName", "unknown-printer", "legacy", scx, "auto", "legacy", scx, "airsane", device.RuleOverride, def, none, none},
		{"driver legacy with a driverName legacy does not list uses legacy's match", "samsung-scx4300-mfp", "legacy", "generic--postscript-printer--en", "auto", "legacy", scx, "airsane", device.RuleOverride, []string{"xerox_mfp"}, none, none},
		{"driverName with driver auto", "generic-postscript", "auto", scx, "auto", "legacy", scx, "airsane", device.RuleOverride, def, none, none},
		{"driverName in several families goes to the first in priority", "unknown-printer", "auto", "generic--postscript-printer--en", "auto", "ps", "generic--postscript-printer--en", "airsane", device.RuleOverride, def, none, none},
		{"driverName nobody lists leaves the automatic choice", "samsung-scx4300-mfp", "auto", "acme--x-en", "auto", "legacy", scx, "airsane", device.RuleModel, []string{"xerox_mfp"}, none, none},
		{"scanner airsane on ipp-usb uses defaults", "ippusb-mfp", "auto", "", "airsane", "ipp-usb", "", "airsane", device.RuleIPPOverUSB, def, none, none},
		{"scanner ipp-usb drops SANE", "hp-legacy-mfp", "auto", "", "ipp-usb", "hplip", "hp--laserjet-professional-m-1132-mfp--requires-proprietary-plugin-en", "ipp-usb", device.RuleModel, none, plugin, none},
		{"scanner none", "epson-inkjet-mfp", "auto", "", "none", "legacy", "epson--et-2720--epson-inkjet-printer-driver-esc-p-r-for-linux-en", "", device.RuleModel, none, none, none},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel := auto()
			sel.Driver, sel.DriverName, sel.Scanner = tc.driver, tc.driverName, tc.scanner
			got, err := Decide(Fixture(tc.fixture), sel)
			if err != nil {
				t.Fatal(err)
			}
			if got.Printer != tc.printer || got.Driver != tc.wantDriver || got.Scanner != tc.wantScanner || got.Rule != tc.rule ||
				!reflect.DeepEqual(got.SaneBackends, tc.sane) ||
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
	dev := func(cmd string) device.Inventory {
		return device.Inventory{Devices: []device.USBDevice{{
			VendorID: "1234", ProductID: "5678",
			Interfaces: []device.USBInterface{{Class: 7, SubClass: 1, Protocol: 2}},
			DeviceID:   "MFG:X;MDL:Y;CMD:" + cmd + ";",
		}}}
	}
	const ps, pxl = "generic--postscript-printer--recommended-en", "generic--pcl-6-pcl-xl-printer--recommended-en"
	for _, tc := range []struct{ cmd, rule, driver string }{
		{"pjl, postscript", device.RuleCommandSet, ps},          // case-insensitive, spaces trimmed
		{"PCLXL,PostScript Level 3", device.RuleCommandSet, ps}, // the first commandSet wins, not the first token
		{"PWGRaster,pcl6", device.RuleCommandSet, pxl},
		{"PCLM,URF", device.RuleNone, ""}, // a token, not a substring
		{"", device.RuleNone, ""},
	} {
		got, err := c.Decide(dev(tc.cmd), auto())
		if err != nil || got.Rule != tc.rule || got.Driver != tc.driver {
			t.Errorf("CMD %q: got %q %q, %v, want %q %q", tc.cmd, got.Rule, got.Driver, err, tc.rule, tc.driver)
		}
	}
	// Restricted to a family, a set without a driver there gives way to the next.
	sel := auto()
	sel.Driver = "gutenprint"
	if got, _ := c.Decide(dev("POSTSCRIPT,PCLXL"), sel); got.Driver != "generic--pcl-6-pcl-xl-printer--en" {
		t.Errorf("gutenprint override: got %+v", got)
	}
	// IPP over USB wants exactly 7/1/4, not just subClass 1 and protocol 4.
	inv := dev("ZPL")
	inv.Devices[0].Interfaces = append(inv.Devices[0].Interfaces, device.USBInterface{Class: 255, SubClass: 1, Protocol: 4})
	if got, _ := c.Decide(inv, auto()); got.Rule != device.RuleNone || got.Printer != "" || got.Scanner != "airsane" {
		t.Errorf("255/1/4: got %+v", got)
	}
}

// Without MFG and MDL in the device ID, the USB descriptor strings are used.
func TestDecideDescriptorFallback(t *testing.T) {
	inv := Fixture("samsung-scx4300-mfp")
	inv.Devices[0].DeviceID = "CMD:GDI;"
	got, err := Decide(inv, auto())
	if err != nil || got.Driver != "samsung--scx-4300--2-0-0-en" {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestDecideSimulated(t *testing.T) {
	for _, inv := range []device.Inventory{{}, Fixture("hp-legacy-mfp")} {
		sel := config.Device{Mode: config.ModeSimulated, Driver: "ps", Scanner: "none"}
		got, err := Decide(inv, sel)
		if err != nil {
			t.Fatal(err)
		}
		want := device.Decision{
			Printer: "sim", Scanner: "sim", Rule: device.RuleSimulated,
			SaneBackends: []string{}, RequiredEULAs: []string{}, OptionalEULAs: []string{},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v", got)
		}
	}
}

// The returned device is a copy: editing it never changes the inventory.
func TestDecideCopiesDevice(t *testing.T) {
	inv := Fixture("ippusb-mfp")
	got, err := Decide(inv, auto())
	if err != nil {
		t.Fatal(err)
	}
	got.Device.Serial = "x"
	got.Device.Interfaces[0].Class = 1
	if inv.Devices[0].Serial == "x" || inv.Devices[0].Interfaces[0].Class == 1 {
		t.Error("Decide aliased the inventory")
	}
}
