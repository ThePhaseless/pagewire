package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const minimalYAML = `apiVersion: printscan/v1
name: Printer
`

const fullYAML = `# a comment
apiVersion: printscan/v1
name: Living room printer
location: Living room
device:
  mode: usb
  usb:
    vendorId: "03f0"
    productId: "2b17"
    serial: "ABC123"
  driver: hplip
  scanner: airsane
print:
  defaults:
    media: na_letter_8.5x11in
    sides: two-sided-long-edge
    colorMode: monochrome
    quality: high
  maxUploadMiB: 100
scan:
  defaults:
    source: adf-duplex
    resolution: 600
    colorMode: grayscale
    format: jpeg
  retentionDays: 0
licenses:
  acceptedEulas:
    - id: hplip-plugin
      version: "3.24.4"
      textSha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`

// parseErrors parses src and returns its field errors, failing when Parse
// succeeds or returns anything but an *ErrorList.
func parseErrors(t *testing.T, src string) []FieldError {
	t.Helper()
	c, err := Parse([]byte(src))
	if err == nil {
		t.Fatalf("Parse succeeded, want error; got %+v", c)
	}
	var el *ErrorList
	if !errors.As(err, &el) {
		t.Fatalf("error %T %v is not *ErrorList", err, err)
	}
	if len(el.Errors) == 0 {
		t.Fatal("empty ErrorList")
	}
	return el.Errors
}

func findError(t *testing.T, errs []FieldError, path string) FieldError {
	t.Helper()
	for _, e := range errs {
		if e.Path == path {
			return e
		}
	}
	t.Fatalf("no error at %s in %+v", path, errs)
	return FieldError{}
}

func TestParseMinimal(t *testing.T) {
	c, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatal(err)
	}
	want := Default("Printer")
	if !reflect.DeepEqual(*c, want) {
		t.Errorf("minimal file\n got  %+v\n want %+v", *c, want)
	}
	if c.Licenses.AcceptedEULAs == nil {
		t.Error("acceptedEulas is nil, want empty list")
	}
}

func TestParseFull(t *testing.T) {
	c, err := Parse([]byte(fullYAML))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		APIVersion: APIVersion,
		Name:       "Living room printer",
		Location:   "Living room",
		Device: Device{
			Mode:    ModeUSB,
			USB:     &USB{VendorID: "03f0", ProductID: "2b17", Serial: "ABC123"},
			Driver:  "hplip",
			Scanner: "airsane",
		},
		Print: PrintConfig{
			Defaults:     PrintDefaults{Media: "na_letter_8.5x11in", Sides: "two-sided-long-edge", ColorMode: "monochrome", Quality: "high"},
			MaxUploadMiB: 100,
		},
		Scan: ScanConfig{
			Defaults:      ScanDefaults{Source: "adf-duplex", Resolution: 600, ColorMode: "grayscale", Format: "jpeg"},
			RetentionDays: 0,
		},
		Licenses: Licenses{AcceptedEULAs: []EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: testSHA}}},
	}
	if !reflect.DeepEqual(*c, want) {
		t.Errorf("full file\n got  %+v\n want %+v", *c, want)
	}
}

func TestParseDefaults(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want func(*Config)
	}{
		{
			name: "partly specified nested mapping keeps sibling defaults",
			src:  minimalYAML + "print:\n  defaults:\n    sides: two-sided-short-edge\n",
			want: func(c *Config) { c.Print.Defaults.Sides = "two-sided-short-edge" },
		},
		{
			name: "null section keeps its defaults",
			src:  minimalYAML + "device:\nscan:\n",
		},
		{
			name: "null acceptedEulas stays an empty list",
			src:  minimalYAML + "licenses:\n  acceptedEulas:\n",
		},
		{
			name: "explicit empty acceptedEulas",
			src:  minimalYAML + "licenses:\n  acceptedEulas: []\n",
		},
		{
			name: "zero retention is kept, not defaulted",
			src:  minimalYAML + "scan:\n  retentionDays: 0\n",
			want: func(c *Config) { c.Scan.RetentionDays = 0 },
		},
		{
			name: "usb null stays nil",
			src:  minimalYAML + "device:\n  usb: null\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse([]byte(tt.src))
			if err != nil {
				t.Fatal(err)
			}
			want := Default("Printer")
			if tt.want != nil {
				tt.want(&want)
			}
			if !reflect.DeepEqual(*c, want) {
				t.Errorf("\n got  %+v\n want %+v", *c, want)
			}
			if c.Licenses.AcceptedEULAs == nil {
				t.Error("acceptedEulas is nil")
			}
		})
	}
}

func TestParseListReplacesNotAppends(t *testing.T) {
	c, err := Parse([]byte(minimalYAML + "licenses:\n  acceptedEulas:\n    - {id: a, version: \"1\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []EULARef{{ID: "a", Version: "1"}}; !reflect.DeepEqual(c.Licenses.AcceptedEULAs, want) {
		t.Errorf("got %+v, want %+v", c.Licenses.AcceptedEULAs, want)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name         string
		src          string
		path         string
		line, column int
		msg          string // substring
	}{
		{
			name: "unknown key",
			src:  minimalYAML + "device:\n  mode: usb\n  colour: red\n",
			path: "/device/colour", line: 5, column: 3, msg: "unknown",
		},
		{
			name: "unknown top-level key",
			src:  minimalYAML + "extra: 1\n",
			path: "/extra", line: 3, column: 1, msg: "unknown",
		},
		{
			name: "bad enum",
			src:  minimalYAML + "device:\n  mode: wifi\n",
			path: "/device/mode", line: 4, column: 9, msg: "usb simulated",
		},
		{
			name: "bad pattern",
			src:  minimalYAML + "device:\n  usb:\n    vendorId: \"03F0\"\n    productId: \"2b17\"\n",
			path: "/device/usb/vendorId", line: 5, column: 15, msg: "hex",
		},
		{
			name: "bad media",
			src:  minimalYAML + "print:\n  defaults:\n    media: A4\n",
			path: "/print/defaults/media", line: 5, column: 12, msg: "PWG",
		},
		{
			name: "out of range",
			src:  minimalYAML + "print:\n  maxUploadMiB: 501\n",
			path: "/print/maxUploadMiB", line: 4, column: 17, msg: "500",
		},
		{
			name: "wrong type",
			src:  minimalYAML + "scan:\n  defaults:\n    resolution: high\n",
			path: "/scan/defaults/resolution", line: 5, column: 17, msg: "integer",
		},
		{
			name: "string where integer expected",
			src:  minimalYAML + "scan:\n  retentionDays: soon\n",
			path: "/scan/retentionDays", line: 4, column: 18, msg: "integer",
		},
		{
			name: "name starting with -",
			src:  "apiVersion: printscan/v1\nname: \"-h\"\n",
			path: "/name", line: 2, column: 7, msg: "must not start with -",
		},
		{
			name: "serial too long",
			src:  minimalYAML + "device:\n  usb:\n    vendorId: \"03f0\"\n    productId: \"2b17\"\n    serial: \"" + strings.Repeat("9", 127) + "\"\n",
			path: "/device/usb/serial", line: 7, column: 13, msg: "126",
		},
		{
			name: "serial with a control character",
			src:  minimalYAML + "device:\n  usb:\n    vendorId: \"03f0\"\n    productId: \"2b17\"\n    serial: \"AB\\u0007C\"\n",
			path: "/device/usb/serial", line: 7, column: 13, msg: "control",
		},
		{
			name: "missing name",
			src:  "apiVersion: printscan/v1\nlocation: x\n",
			path: "/name", line: 1, column: 1, msg: "required",
		},
		{
			name: "wrong apiVersion",
			src:  "apiVersion: printscan/v2\nname: x\n",
			path: "/apiVersion", line: 1, column: 13, msg: "printscan/v1",
		},
		{
			name: "control character in name",
			src:  "apiVersion: printscan/v1\nname: \"bad\\tname\"\n",
			path: "/name", line: 2, column: 7, msg: "control",
		},
		{
			name: "name too long",
			src:  "apiVersion: printscan/v1\nname: " + strings.Repeat("x", 64) + "\n",
			path: "/name", line: 2, column: 7, msg: "63",
		},
		{
			name: "duplicate EULA",
			src:  minimalYAML + "licenses:\n  acceptedEulas:\n    - {id: a, version: \"1\"}\n    - {id: a, version: \"1\"}\n",
			path: "/licenses/acceptedEulas", line: 5, column: 5, msg: "duplicate",
		},
		{
			name: "EULA without version",
			src:  minimalYAML + "licenses:\n  acceptedEulas:\n    - id: a\n",
			path: "/licenses/acceptedEulas/0/version", line: 5, column: 7, msg: "required",
		},
		{
			name: "syntax error",
			src:  "apiVersion: printscan/v1\nname: [unclosed\n",
			path: "", line: 2, column: 7, msg: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := findError(t, parseErrors(t, tt.src), tt.path)
			if e.Line != tt.line || e.Column != tt.column {
				t.Errorf("position %d:%d, want %d:%d (%+v)", e.Line, e.Column, tt.line, tt.column, e)
			}
			if !strings.Contains(e.Message, tt.msg) {
				t.Errorf("message %q lacks %q", e.Message, tt.msg)
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	for _, src := range []string{"", "# only a comment\n", "---\n"} {
		errs := parseErrors(t, src)
		findError(t, errs, "/apiVersion")
		e := findError(t, errs, "/name")
		if e.Line != 1 || e.Column != 1 {
			t.Errorf("%q: position %d:%d, want 1:1", src, e.Line, e.Column)
		}
	}
}

func TestParseRejectsNonMapping(t *testing.T) {
	for _, src := range []string{"hello\n", "- a\n- b\n"} {
		errs := parseErrors(t, src)
		if errs[0].Line != 1 {
			t.Errorf("%q: line %d, want 1", src, errs[0].Line)
		}
	}
}

func TestParseRejectsMultipleDocuments(t *testing.T) {
	errs := parseErrors(t, minimalYAML+"---\n"+minimalYAML)
	if errs[0].Line != 3 || !strings.Contains(errs[0].Message, "document") {
		t.Errorf("got %+v", errs[0])
	}
}

func TestEnumsAccepted(t *testing.T) {
	// Every enum value the schema lists must also pass Parse (the validate and
	// jsonschema tags are two sources of truth; internal/schema checks the other side).
	for _, src := range []string{
		"device:\n  mode: simulated\n", "device:\n  mode: usb\n",
		"device:\n  driver: auto\n", "device:\n  driver: ipp-usb\n", "device:\n  driver: ps\n",
		"device:\n  driver: hplip\n", "device:\n  driver: gutenprint\n", "device:\n  driver: ghostscript\n",
		"device:\n  scanner: auto\n", "device:\n  scanner: ipp-usb\n", "device:\n  scanner: airsane\n", "device:\n  scanner: none\n",
		"print:\n  defaults:\n    sides: one-sided\n", "print:\n  defaults:\n    sides: two-sided-long-edge\n", "print:\n  defaults:\n    sides: two-sided-short-edge\n",
		"print:\n  defaults:\n    colorMode: auto\n", "print:\n  defaults:\n    colorMode: color\n", "print:\n  defaults:\n    colorMode: monochrome\n",
		"print:\n  defaults:\n    quality: draft\n", "print:\n  defaults:\n    quality: normal\n", "print:\n  defaults:\n    quality: high\n",
		"scan:\n  defaults:\n    source: platen\n", "scan:\n  defaults:\n    source: adf\n", "scan:\n  defaults:\n    source: adf-duplex\n",
		"scan:\n  defaults:\n    colorMode: color\n", "scan:\n  defaults:\n    colorMode: grayscale\n", "scan:\n  defaults:\n    colorMode: blackandwhite\n",
		"scan:\n  defaults:\n    format: pdf\n", "scan:\n  defaults:\n    format: jpeg\n",
		"scan:\n  defaults:\n    resolution: 75\n", "scan:\n  defaults:\n    resolution: 1200\n",
		"scan:\n  retentionDays: 3650\n", "print:\n  maxUploadMiB: 1\n",
	} {
		if _, err := Parse([]byte(minimalYAML + src)); err != nil {
			t.Errorf("%q: %v", src, err)
		}
	}
	if _, err := Parse([]byte("apiVersion: printscan/v1\nname: " + strings.Repeat("ü", 63) + "\n")); err != nil {
		t.Errorf("63-rune name: %v", err)
	}
}
