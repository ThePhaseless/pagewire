package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const minimalYAML = `apiVersion: pagewire/v2
name: Printer
`

const fullYAML = `# a comment
apiVersion: pagewire/v2
name: Home
printers:
  - id: living-room
    name: Living room printer
    location: Living room
    enabled: true
    mode: usb
    usb:
      vendorId: "03f0"
      productId: "2b17"
      serial: "ABC123"
    driver: hplip
    driverName: hp-officejet
    defaults:
      media: na_letter_8.5x11in
      mediaSource: tray-1
      mediaType: stationery
      sides: two-sided-long-edge
      colorMode: monochrome
      quality: high
      resolution: 600
      contentOptimize: text-and-graphic
      orientation: reverse-landscape
      numberUp: 4
      printScaling: auto-fit
      outputBin: face-down
      finishings: staple-top-left
scanners:
  - id: living-room-scanner
    name: Living room scanner
    enabled: false
    usb:
      vendorId: "03f0"
      productId: "2b17"
      serial: "ABC123"
    backend: airsane
    defaults:
      source: adf-duplex
      resolution: 600
      colorMode: grayscale
      intent: photo
      format: jpeg
ignoredDevices:
  - {vendorId: "046d", productId: "c31c", serial: "", roles: [printer, scanner]}
print:
  maxUploadMiB: 100
scan:
  retentionDays: 0
licenses:
  acceptedEulas:
    - id: hplip-plugin
      version: "3.24.4"
      textSha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`

// printer is a minimal printer list entry for tests to append to.
const printer = "printers:\n  - id: p\n    name: P\n"

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
	usb := &USB{VendorID: "03f0", ProductID: "2b17", Serial: "ABC123"}
	want := Config{
		APIVersion: APIVersion,
		Name:       "Home",
		Printers: []Printer{{
			ID: "living-room", Name: "Living room printer", Location: "Living room", Enabled: true,
			Mode: ModeUSB, USB: usb, Driver: "hplip", DriverName: "hp-officejet",
			Defaults: PrintDefaults{
				Media: "na_letter_8.5x11in", MediaSource: "tray-1", MediaType: "stationery", Sides: "two-sided-long-edge",
				ColorMode: "monochrome", Quality: "high", Resolution: 600, ContentOptimize: "text-and-graphic",
				Orientation: "reverse-landscape", NumberUp: 4, PrintScaling: "auto-fit", OutputBin: "face-down", Finishings: "staple-top-left",
			},
		}},
		Scanners: []Scanner{{
			ID: "living-room-scanner", Name: "Living room scanner", Enabled: false, Mode: ModeUSB, USB: usb, Backend: "airsane",
			Defaults: ScanDefaults{Source: "adf-duplex", Resolution: 600, ColorMode: "grayscale", Intent: "photo", Format: "jpeg"},
		}},
		IgnoredDevices: []IgnoredDevice{{VendorID: "046d", ProductID: "c31c", Roles: []string{RolePrinter, RoleScanner}}},
		Print:          PrintConfig{MaxUploadMiB: 100},
		Scan:           ScanConfig{RetentionDays: 0},
		Licenses:       Licenses{AcceptedEULAs: []EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: testSHA}}},
	}
	if !reflect.DeepEqual(*c, want) {
		t.Errorf("full file\n got  %+v\n want %+v", *c, want)
	}
}

func TestParseDefaults(t *testing.T) {
	entry := func(c *Config) {
		c.Printers = []Printer{{ID: "p", Name: "P", Enabled: true, Mode: ModeUSB, Driver: Auto}}
	}
	tests := []struct {
		name string
		src  string
		want func(*Config)
	}{
		{
			name: "entry without enabled, mode, driver or defaults gets them",
			src:  minimalYAML + printer,
			want: entry,
		},
		{
			name: "enabled null is true",
			src:  minimalYAML + printer + "    enabled: null\n",
			want: entry,
		},
		{
			name: "enabled false is kept",
			src:  minimalYAML + printer + "    enabled: false\n",
			want: func(c *Config) { entry(c); c.Printers[0].Enabled = false },
		},
		{
			name: "empty mode and driver default",
			src:  minimalYAML + printer + "    mode: \"\"\n    driver: \"\"\n",
			want: entry,
		},
		{
			name: "scanner defaults",
			src:  minimalYAML + "scanners:\n  - {id: s, name: S}\n",
			want: func(c *Config) {
				c.Scanners = []Scanner{{ID: "s", Name: "S", Enabled: true, Mode: ModeUSB, Backend: BackendAuto}}
			},
		},
		{
			name: "null lists stay empty lists",
			src:  minimalYAML + "printers:\nscanners:\nignoredDevices:\nlicenses:\n  acceptedEulas:\n",
		},
		{
			name: "explicit empty lists",
			src:  minimalYAML + "printers: []\nscanners: []\nignoredDevices: []\nlicenses:\n  acceptedEulas: []\n",
		},
		{
			name: "null section keeps its defaults",
			src:  minimalYAML + "print:\nscan:\n",
		},
		{
			name: "zero retention is kept, not defaulted",
			src:  minimalYAML + "scan:\n  retentionDays: 0\n",
			want: func(c *Config) { c.Scan.RetentionDays = 0 },
		},
		{
			name: "usb null stays nil",
			src:  minimalYAML + printer + "    usb: null\n",
			want: entry,
		},
		{
			name: "every defaults key accepted",
			src: minimalYAML + printer + "    defaults:\n      media: iso_a5_148x210mm\n      mediaSource: by-pass-tray\n      mediaType: photographic-glossy\n" +
				"      resolution: 1200\n      contentOptimize: photo\n      orientation: portrait\n      numberUp: 16\n      printScaling: none\n" +
				"      outputBin: tray-1\n      finishings: punch\n",
			want: func(c *Config) {
				entry(c)
				c.Printers[0].Defaults = PrintDefaults{
					Media: "iso_a5_148x210mm", MediaSource: "by-pass-tray", MediaType: "photographic-glossy", Resolution: 1200,
					ContentOptimize: "photo", Orientation: "portrait", NumberUp: 16, PrintScaling: "none", OutputBin: "tray-1", Finishings: "punch",
				}
			},
		},
		{
			name: "scan intent accepted",
			src:  minimalYAML + "scanners:\n  - id: s\n    name: S\n    defaults:\n      intent: photo\n",
			want: func(c *Config) {
				c.Scanners = []Scanner{{ID: "s", Name: "S", Enabled: true, Mode: ModeUSB, Backend: BackendAuto, Defaults: ScanDefaults{Intent: "photo"}}}
			},
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
			src:  minimalYAML + printer + "    colour: red\n",
			path: "/printers/0/colour", line: 6, column: 5, msg: "unknown",
		},
		{
			name: "unknown top-level key",
			src:  minimalYAML + "extra: 1\n",
			path: "/extra", line: 3, column: 1, msg: "unknown",
		},
		{
			name: "v1 key in a v2 file",
			src:  minimalYAML + "device:\n  mode: usb\n",
			path: "/device", line: 3, column: 1, msg: "unknown",
		},
		{
			name: "bad enum",
			src:  minimalYAML + printer + "    mode: wifi\n",
			path: "/printers/0/mode", line: 6, column: 11, msg: "usb simulated",
		},
		{
			name: "bad backend",
			src:  minimalYAML + "scanners:\n  - id: s\n    name: S\n    backend: none\n",
			path: "/scanners/0/backend", line: 6, column: 14, msg: "airsane",
		},
		{
			name: "bad pattern",
			src:  minimalYAML + printer + "    usb:\n      vendorId: \"03F0\"\n      productId: \"2b17\"\n",
			path: "/printers/0/usb/vendorId", line: 7, column: 17, msg: "hex",
		},
		{
			name: "bad media",
			src:  minimalYAML + printer + "    defaults:\n      media: A4\n",
			path: "/printers/0/defaults/media", line: 7, column: 14, msg: "PWG",
		},
		{
			name: "tray that is not a keyword",
			src:  minimalYAML + printer + "    defaults:\n      mediaSource: Tray 1\n",
			path: "/printers/0/defaults/mediaSource", line: 7, column: 20, msg: "must be a lower-case IPP keyword",
		},
		{
			name: "numberUp out of range",
			src:  minimalYAML + printer + "    defaults:\n      numberUp: 32\n",
			path: "/printers/0/defaults/numberUp", line: 7, column: 17, msg: "16",
		},
		{
			name: "print resolution out of range",
			src:  minimalYAML + printer + "    defaults:\n      resolution: 4800\n",
			path: "/printers/0/defaults/resolution", line: 7, column: 19, msg: "2400",
		},
		{
			name: "bad intent",
			src:  minimalYAML + "scanners:\n  - id: s\n    name: S\n    defaults:\n      intent: receipt\n",
			path: "/scanners/0/defaults/intent", line: 7, column: 15, msg: "business-card",
		},
		{
			name: "id starting with a dash",
			src:  minimalYAML + "printers:\n  - id: -bad\n    name: P\n",
			path: "/printers/0/id", line: 4, column: 9, msg: "1 to 32 lower-case letters, digits or dashes",
		},
		{
			name: "id of 33 characters",
			src:  minimalYAML + "scanners:\n  - id: " + strings.Repeat("a", 33) + "\n    name: S\n",
			path: "/scanners/0/id", line: 4, column: 9, msg: "1 to 32 lower-case letters, digits or dashes",
		},
		{
			name: "entry without id",
			src:  minimalYAML + "printers:\n  - name: P\n",
			path: "/printers/0/id", line: 4, column: 5, msg: "required",
		},
		{
			name: "entry without name",
			src:  minimalYAML + "scanners:\n  - id: s\n",
			path: "/scanners/0/name", line: 4, column: 5, msg: "required",
		},
		{
			name: "ignored device without roles",
			src:  minimalYAML + "ignoredDevices:\n  - vendorId: \"046d\"\n    productId: \"c31c\"\n",
			path: "/ignoredDevices/0/roles", line: 4, column: 5, msg: "at least 1",
		},
		{
			name: "ignored device with an unknown role",
			src:  minimalYAML + "ignoredDevices:\n  - vendorId: \"046d\"\n    productId: \"c31c\"\n    roles: [fax]\n",
			path: "/ignoredDevices/0/roles/0", line: 6, column: 13, msg: "printer scanner",
		},
		{
			name: "ignored device with a role twice",
			src:  minimalYAML + "ignoredDevices:\n  - vendorId: \"046d\"\n    productId: \"c31c\"\n    roles: [printer, printer]\n",
			path: "/ignoredDevices/0/roles", line: 6, column: 12, msg: "duplicate",
		},
		{
			name: "ignored device without vendorId",
			src:  minimalYAML + "ignoredDevices:\n  - productId: \"c31c\"\n    roles: [printer]\n",
			path: "/ignoredDevices/0/vendorId", line: 4, column: 5, msg: "hex",
		},
		{
			name: "out of range",
			src:  minimalYAML + "print:\n  maxUploadMiB: 501\n",
			path: "/print/maxUploadMiB", line: 4, column: 17, msg: "500",
		},
		{
			name: "wrong type",
			src:  minimalYAML + "scanners:\n  - id: s\n    name: S\n    defaults:\n      resolution: high\n",
			path: "/scanners/0/defaults/resolution", line: 7, column: 19, msg: "integer",
		},
		{
			name: "string where boolean expected",
			src:  minimalYAML + printer + "    enabled: sometimes\n",
			path: "/printers/0/enabled", line: 6, column: 14, msg: "true or false",
		},
		{
			name: "string where integer expected",
			src:  minimalYAML + "scan:\n  retentionDays: soon\n",
			path: "/scan/retentionDays", line: 4, column: 18, msg: "integer",
		},
		{
			name: "name starting with -",
			src:  "apiVersion: pagewire/v2\nname: \"-h\"\n",
			path: "/name", line: 2, column: 7, msg: "must not start with -",
		},
		{
			name: "printer name starting with -",
			src:  minimalYAML + "printers:\n  - id: p\n    name: \"-h\"\n",
			path: "/printers/0/name", line: 5, column: 11, msg: "must not start with -",
		},
		{
			name: "serial too long",
			src:  minimalYAML + printer + "    usb:\n      vendorId: \"03f0\"\n      productId: \"2b17\"\n      serial: \"" + strings.Repeat("9", 127) + "\"\n",
			path: "/printers/0/usb/serial", line: 9, column: 15, msg: "126",
		},
		{
			name: "serial with a control character",
			src:  minimalYAML + printer + "    usb:\n      vendorId: \"03f0\"\n      productId: \"2b17\"\n      serial: \"AB\\u0007C\"\n",
			path: "/printers/0/usb/serial", line: 9, column: 15, msg: "control",
		},
		{
			name: "driverName too long",
			src:  minimalYAML + printer + "    driverName: " + strings.Repeat("x", 256) + "\n",
			path: "/printers/0/driverName", line: 6, column: 17, msg: "255",
		},
		{
			name: "driverName with a control character",
			src:  minimalYAML + printer + "    driverName: \"a\\tb\"\n",
			path: "/printers/0/driverName", line: 6, column: 17, msg: "control",
		},
		{
			name: "missing name",
			src:  "apiVersion: pagewire/v2\nprinters: []\n",
			path: "/name", line: 1, column: 1, msg: "required",
		},
		{
			name: "wrong apiVersion",
			src:  "apiVersion: pagewire/v3\nname: x\n",
			path: "/apiVersion", line: 1, column: 13, msg: "pagewire/v2",
		},
		{
			name: "control character in name",
			src:  "apiVersion: pagewire/v2\nname: \"bad\\tname\"\n",
			path: "/name", line: 2, column: 7, msg: "control",
		},
		{
			name: "name too long",
			src:  "apiVersion: pagewire/v2\nname: " + strings.Repeat("x", 64) + "\n",
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
			src:  "apiVersion: pagewire/v2\nname: [unclosed\n",
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
	scanner := "scanners:\n  - id: s\n    name: S\n"
	var srcs []string
	add := func(prefix, key string, values ...string) {
		for _, v := range values {
			srcs = append(srcs, prefix+key+": "+v+"\n")
		}
	}
	add(printer+"    ", "mode", "usb", "simulated")
	add(printer+"    ", "driver", "auto", "ipp-usb", "ps", "hplip", "gutenprint", "ghostscript", "legacy")
	add(printer+"    ", "driverName", "samsung--scx-4300--2-0-0-en")
	pd := printer + "    defaults:\n      "
	add(pd, "sides", "one-sided", "two-sided-long-edge", "two-sided-short-edge")
	add(pd, "colorMode", "auto", "color", "monochrome")
	add(pd, "quality", "draft", "normal", "high")
	add(pd, "contentOptimize", "auto", "graphic", "photo", "text", "text-and-graphic")
	add(pd, "orientation", "portrait", "landscape", "reverse-landscape", "reverse-portrait")
	add(pd, "printScaling", "auto", "auto-fit", "fill", "fit", "none")
	add(pd, "resolution", "75", "2400")
	add(pd, "numberUp", "1", "16")
	add(scanner+"    ", "mode", "usb", "simulated")
	add(scanner+"    ", "backend", "auto", "ipp-usb", "airsane")
	sd := scanner + "    defaults:\n      "
	add(sd, "source", "platen", "adf", "adf-duplex")
	add(sd, "colorMode", "color", "grayscale", "blackandwhite")
	add(sd, "intent", "document", "text-and-graphic", "photo", "preview", "object", "business-card")
	add(sd, "format", "pdf", "jpeg")
	add(sd, "resolution", "75", "1200")
	add("ignoredDevices:\n  - vendorId: \"046d\"\n    productId: \"c31c\"\n    ", "roles", "[printer]", "[scanner]", "[printer, scanner]")
	add("scan:\n  ", "retentionDays", "3650")
	add("print:\n  ", "maxUploadMiB", "1")
	for _, src := range srcs {
		if _, err := Parse([]byte(minimalYAML + src)); err != nil {
			t.Errorf("%q: %v", src, err)
		}
	}
	for _, src := range []string{
		"apiVersion: pagewire/v2\nname: " + strings.Repeat("ü", 63) + "\n",
		minimalYAML + "printers:\n  - id: " + strings.Repeat("a", 32) + "\n    name: P\n",
		minimalYAML + "printers:\n  - id: a\n    name: P\n  - id: a-1\n    name: Q\n",
	} {
		if _, err := Parse([]byte(src)); err != nil {
			t.Errorf("%q: %v", src, err)
		}
	}
}
