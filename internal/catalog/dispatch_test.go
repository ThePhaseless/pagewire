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
	for _, tc := range []struct {
		fixture, rule, printer, scanner string
		sane, optional                  []string
	}{
		{"ippusb-mfp", "ipp-over-usb", "ipp-usb", "ipp-usb", []string{}, []string{}},
		{"hp-legacy-mfp", "hp", "hplip", "airsane", []string{"hpaio"}, []string{"hplip-plugin"}},
		{"generic-postscript", "postscript", "ps", "airsane", def, []string{}},
		{"brother-pcl-laser", "pcl", "ghostscript", "airsane", def, []string{}},
		{"epson-inkjet-mfp", "epson", "gutenprint", "airsane", []string{"epson2", "epsonds"}, []string{}},
		{"canon-pixma-mfp", "canon", "gutenprint", "airsane", []string{"pixma"}, []string{}},
		{"unknown-printer", "fallback", "ghostscript", "airsane", def, []string{}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			inv := Fixture(tc.fixture)
			got, err := Decide(inv, auto())
			if err != nil {
				t.Fatal(err)
			}
			want := device.Decision{
				Printer: tc.printer, Scanner: tc.scanner, SaneBackends: tc.sane,
				RequiredEULAs: []string{}, OptionalEULAs: tc.optional, Rule: tc.rule, Device: &inv.Devices[0],
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got  %+v\nwant %+v", got, want)
			}
		})
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
	if err != nil || got.Rule != "ipp-over-usb" {
		t.Errorf("no selector: first printer-class device wins, got %+v, %v", got, err)
	}

	sel.USB = &config.USB{VendorID: "04f9", ProductID: "0446", Serial: brother.Serial}
	if got, err := Decide(inv, sel); err != nil || got.Rule != "pcl" || got.Device.Serial != brother.Serial {
		t.Errorf("selector: got %+v, %v", got, err)
	}
	sel.USB.Serial = ""
	if got, err := Decide(inv, sel); err != nil || got.Rule != "pcl" {
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
	for _, tc := range []struct {
		name, fixture, driver, scanner string
		printer, wantScanner           string
		sane, optional                 []string
	}{
		{"driver ps on ipp-usb", "ippusb-mfp", "ps", "auto", "ps", "ipp-usb", []string{}, []string{}},
		{"driver ipp-usb on hp", "hp-legacy-mfp", "ipp-usb", "auto", "ipp-usb", "airsane", []string{"hpaio"}, []string{"hplip-plugin"}},
		{"driver hplip", "brother-pcl-laser", "hplip", "auto", "hplip", "airsane", def, []string{}},
		{"driver gutenprint", "unknown-printer", "gutenprint", "auto", "gutenprint", "airsane", def, []string{}},
		{"driver ghostscript", "generic-postscript", "ghostscript", "auto", "ghostscript", "airsane", def, []string{}},
		{"scanner airsane on ipp-usb uses defaults", "ippusb-mfp", "auto", "airsane", "ipp-usb", "airsane", def, []string{}},
		{"scanner ipp-usb drops SANE", "hp-legacy-mfp", "auto", "ipp-usb", "hplip", "ipp-usb", []string{}, []string{"hplip-plugin"}},
		{"scanner none", "epson-inkjet-mfp", "auto", "none", "gutenprint", "", []string{}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel := auto()
			sel.Driver, sel.Scanner = tc.driver, tc.scanner
			got, err := Decide(Fixture(tc.fixture), sel)
			if err != nil {
				t.Fatal(err)
			}
			if got.Printer != tc.printer || got.Scanner != tc.wantScanner ||
				!reflect.DeepEqual(got.SaneBackends, tc.sane) || !reflect.DeepEqual(got.OptionalEULAs, tc.optional) {
				t.Errorf("got %+v", got)
			}
		})
	}
}

func TestDecideCommandTokens(t *testing.T) {
	dev := func(cmd string) device.Inventory {
		return device.Inventory{Devices: []device.USBDevice{{
			VendorID: "1234", ProductID: "5678",
			Interfaces: []device.USBInterface{{Class: 7, SubClass: 1, Protocol: 2}},
			DeviceID:   "MFG:X;MDL:Y;CMD:" + cmd + ";",
		}}}
	}
	for cmd, rule := range map[string]string{
		"pjl, postscript":        "postscript", // case-insensitive, spaces trimmed
		"PostScript Level 3,PCL": "postscript",
		"PCLM,URF":               "fallback", // a token, not a substring
		"PWGRaster,pcl6":         "pcl",
		"":                       "fallback",
	} {
		got, err := Decide(dev(cmd), auto())
		if err != nil || got.Rule != rule {
			t.Errorf("CMD %q: got %q, %v, want %q", cmd, got.Rule, err, rule)
		}
	}
	// The interface rule wants exactly 7/1/4, not just subClass 1 and protocol 4.
	inv := dev("ZPL")
	inv.Devices[0].Interfaces = append(inv.Devices[0].Interfaces, device.USBInterface{Class: 255, SubClass: 1, Protocol: 4})
	if got, _ := Decide(inv, auto()); got.Rule != "fallback" {
		t.Errorf("255/1/4 matched %q", got.Rule)
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
			Printer: "sim", Scanner: "sim", Rule: "simulated",
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
