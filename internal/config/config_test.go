package config

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestDefault(t *testing.T) {
	c := Default("Office")
	if c.APIVersion != APIVersion || c.Name != "Office" {
		t.Errorf("apiVersion %q name %q", c.APIVersion, c.Name)
	}
	if c.Printers == nil || len(c.Printers) != 0 || c.Scanners == nil || len(c.Scanners) != 0 ||
		c.IgnoredDevices == nil || len(c.IgnoredDevices) != 0 {
		t.Errorf("lists %#v %#v %#v, want empty non-nil", c.Printers, c.Scanners, c.IgnoredDevices)
	}
	if c.Licenses.AcceptedEULAs == nil || len(c.Licenses.AcceptedEULAs) != 0 {
		t.Errorf("acceptedEulas %#v, want empty non-nil", c.Licenses.AcceptedEULAs)
	}
	if c.Print.MaxUploadMiB != 50 || c.Scan.RetentionDays != 7 {
		t.Errorf("limits %+v %+v", c.Print, c.Scan)
	}
}

// devices returns a config with one usb printer and one usb scanner.
func devices() Config {
	c := Default("Home")
	c.Printers = []Printer{{ID: "p", Name: "P", Enabled: true, Mode: ModeUSB, Driver: Auto, USB: &USB{VendorID: "03f0", ProductID: "2b17"}}}
	c.Scanners = []Scanner{{ID: "s", Name: "S", Enabled: true, Mode: ModeUSB, Backend: BackendAuto, USB: &USB{VendorID: "03f0", ProductID: "2b17"}}}
	return c
}

func TestRoundTripsThroughParse(t *testing.T) {
	full := devices()
	full.Printers[0].Defaults = PrintDefaults{
		Media: "iso_a4_210x297mm", MediaSource: "tray-1", MediaType: "stationery", Sides: "one-sided",
		ColorMode: "monochrome", Quality: "high", Resolution: 600, ContentOptimize: "text", Orientation: "landscape",
		NumberUp: 2, PrintScaling: "fit", OutputBin: "face-down", Finishings: "staple",
	}
	full.Scanners[0].Defaults = ScanDefaults{Source: "adf", Resolution: 300, ColorMode: "grayscale", Intent: "photo", Format: "jpeg"}
	full.Scanners[0].Enabled = false
	full.IgnoredDevices = []IgnoredDevice{{VendorID: "046d", ProductID: "c31c", Roles: []string{RolePrinter, RoleScanner}}}
	for name, want := range map[string]Config{"default": Default("Living room printer"), "devices": devices(), "full": full} {
		t.Run(name, func(t *testing.T) {
			for enc, marshal := range map[string]func(any) ([]byte, error){"yaml": yaml.Marshal, "json": json.Marshal} {
				b, err := marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				got, err := Parse(b)
				if err != nil {
					t.Fatalf("%s: %v\n%s", enc, err, b)
				}
				if !reflect.DeepEqual(*got, want) {
					t.Errorf("%s:\n got  %+v\n want %+v\n%s", enc, *got, want, b)
				}
			}
		})
	}
}

func TestJSONShape(t *testing.T) {
	b, err := json.Marshal(devices())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"defaults":{}`, `"enabled":true`, `"printers":[{`, `"ignoredDevices":[]`, `"acceptedEulas":[]`,
		`"usb":{"vendorId":"03f0","productId":"2b17","serial":""}`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON lacks %s: %s", want, b)
		}
	}
	c := devices()
	c.Printers[0].USB, c.Scanners[0].USB, c.Scanners[0].Enabled = nil, nil, false
	b, err = json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"usb":`) || !strings.Contains(string(b), `"enabled":false`) {
		t.Errorf("JSON of a printer without usb or a disabled scanner: %s", b)
	}
	y, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(y), "usb:") || !strings.Contains(string(y), "enabled: false") {
		t.Errorf("YAML of a printer without usb or a disabled scanner:\n%s", y)
	}
}

func TestSimulated(t *testing.T) {
	if !(Printer{Mode: ModeSimulated}).Simulated() || (Printer{Mode: ModeUSB}).Simulated() ||
		!(Scanner{Mode: ModeSimulated}).Simulated() || (Scanner{Mode: ModeUSB}).Simulated() {
		t.Error("Simulated()")
	}
}

func TestErrorListError(t *testing.T) {
	el := &ErrorList{Errors: []FieldError{{Path: "/name", Line: 2, Column: 7, Message: "is required"}, {Path: "/printers/0/mode", Message: "bad"}}}
	got := el.Error()
	for _, want := range []string{"2:7", "/name", "is required", "/printers/0/mode", "bad"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q lacks %q", got, want)
		}
	}
}

func TestNumberLikeStringsRoundTrip(t *testing.T) {
	want := devices()
	want.Name = "2024"
	want.Printers[0].Location = "true"
	want.Printers[0].USB = &USB{VendorID: "0482", ProductID: "0001", Serial: "012345"}
	want.Licenses.AcceptedEULAs = []EULARef{{ID: "hplip-plugin", Version: "3.24", TextSHA256: testSHA}}
	b, err := yaml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(b)
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("\n got  %+v\n want %+v\n%s", *got, want, b)
	}
}

func TestUnquotedNumberLikeValues(t *testing.T) {
	// Hand-typed unquoted values must come back as the literal text or fail;
	// a silent octal or float conversion would break selector matching.
	for _, tt := range []struct{ src, want string }{
		{"printers:\n  - id: p\n    name: P\n    usb:\n      vendorId: 0482\n      productId: \"0001\"\n", "0482"},
		{"printers:\n  - id: p\n    name: P\n    usb:\n      vendorId: \"0482\"\n      productId: \"0001\"\n      serial: 012345\n", "012345"},
		{"ignoredDevices:\n  - vendorId: \"0482\"\n    productId: \"0001\"\n    serial: 012345\n    roles: [printer]\n", "012345"},
		{"licenses:\n  acceptedEulas:\n    - {id: a, version: 3.20}\n", "3.20"},
	} {
		c, err := Parse([]byte(minimalYAML + tt.src))
		if err != nil {
			continue // a clear rejection is fine
		}
		var vals []string
		for _, p := range c.Printers {
			if p.USB != nil {
				vals = append(vals, p.USB.VendorID, p.USB.Serial)
			}
		}
		for _, d := range c.IgnoredDevices {
			vals = append(vals, d.Serial)
		}
		for _, e := range c.Licenses.AcceptedEULAs {
			vals = append(vals, e.Version)
		}
		if !slices.Contains(vals, tt.want) {
			t.Errorf("%q parsed to %v, want the literal %q or an error", tt.src, vals, tt.want)
		}
	}
}
