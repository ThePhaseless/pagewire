package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testSHA is a well-formed accepted text sha256.
var testSHA = strings.Repeat("a", 64)

var testKnown = Known{
	EULAs:    []EULARef{{ID: "hplip-plugin", Version: "3.24.4"}},
	Printers: []string{"ipp-usb", "ps", "hplip", "gutenprint", "ghostscript", "legacy", "sim"},
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
		path   string // "" means valid
		msg    string
	}{
		{name: "default is valid", change: func(c *Config) { *c = Default("Home") }},
		{name: "a usb printer and scanner are valid", change: func(*Config) {}},
		{
			name: "accepted known EULA",
			change: func(c *Config) {
				c.Licenses.AcceptedEULAs = []EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: testSHA}}
			},
		},
		{
			name: "unknown EULA",
			change: func(c *Config) {
				c.Licenses.AcceptedEULAs = []EULARef{{ID: "no-such-eula", Version: "1", TextSHA256: testSHA}}
			},
			path: "/licenses/acceptedEulas/0", msg: "no-such-eula",
		},
		{
			name: "known EULA with the wrong version",
			change: func(c *Config) {
				c.Licenses.AcceptedEULAs = []EULARef{
					{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: testSHA},
					{ID: "hplip-plugin", Version: "3.20.1", TextSHA256: testSHA},
				}
			},
			path: "/licenses/acceptedEulas/1", msg: "3.20.1",
		},
		{
			name:   "accepted EULA without textSha256",
			change: func(c *Config) { c.Licenses.AcceptedEULAs = []EULARef{{ID: "hplip-plugin", Version: "3.24.4"}} },
			path:   "/licenses/acceptedEulas/0/textSha256", msg: "accept the licence again in Settings",
		},
		{
			name: "textSha256 not lowercase hex",
			change: func(c *Config) {
				c.Licenses.AcceptedEULAs = []EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: strings.ToUpper(testSHA)}}
			},
			path: "/licenses/acceptedEulas/0/textSha256", msg: "64 lowercase hex",
		},
		{
			name: "textSha256 too short",
			change: func(c *Config) {
				c.Licenses.AcceptedEULAs = []EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: "abc"}}
			},
			path: "/licenses/acceptedEulas/0/textSha256", msg: "64 lowercase hex",
		},
		{
			// The unique tag compares whole entries, so a second sha slips past it.
			name: "same id and version twice with different text sha",
			change: func(c *Config) {
				c.Licenses.AcceptedEULAs = []EULARef{
					{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: testSHA},
					{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: strings.Repeat("b", 64)},
				}
			},
			path: "/licenses/acceptedEulas/1", msg: "duplicate",
		},
		{
			name:   "usb selector with simulated mode",
			change: func(c *Config) { c.Printers[0].Mode = ModeSimulated },
			path:   "/printers/0/usb", msg: "mode: usb",
		},
		{
			name:   "scanner usb selector with simulated mode",
			change: func(c *Config) { c.Scanners[0].Mode = ModeSimulated },
			path:   "/scanners/0/usb", msg: "mode: usb",
		},
		{
			name:   "one usb printer and one usb scanner without selectors",
			change: func(c *Config) { c.Printers[0].USB, c.Scanners[0].USB = nil, nil },
		},
		{
			name: "second usb printer without a selector",
			change: func(c *Config) {
				c.Printers[0].USB = nil
				c.Printers = append(c.Printers, Printer{ID: "q", Name: "Q", Mode: ModeUSB, Driver: Auto})
			},
			path: "/printers/1", msg: "/printers/0 already has none",
		},
		{
			name: "second usb scanner without a selector",
			change: func(c *Config) {
				c.Scanners[0].USB = nil
				c.Scanners = append(c.Scanners, Scanner{ID: "t", Name: "T", Mode: ModeUSB, Backend: BackendAuto})
			},
			path: "/scanners/1", msg: "only one usb scanner",
		},
		{
			name: "simulated printers need no selector",
			change: func(c *Config) {
				c.Printers = append(c.Printers, Printer{ID: "q", Name: "Q", Mode: ModeSimulated, Driver: Auto}, Printer{ID: "r", Name: "R", Mode: ModeSimulated, Driver: Auto})
			},
		},
		{
			name: "two printers with the same selector",
			change: func(c *Config) {
				c.Printers = append(c.Printers, Printer{ID: "q", Name: "Q", Mode: ModeUSB, Driver: Auto, USB: &USB{VendorID: "03f0", ProductID: "2b17"}})
			},
			path: "/printers/1/usb", msg: "/printers/0",
		},
		{
			name: "two printers of one model told apart by serial",
			change: func(c *Config) {
				c.Printers = append(c.Printers, Printer{ID: "q", Name: "Q", Mode: ModeUSB, Driver: Auto, USB: &USB{VendorID: "03f0", ProductID: "2b17", Serial: "B"}})
			},
		},
		{
			name: "two scanners with the same selector",
			change: func(c *Config) {
				c.Scanners = append(c.Scanners, Scanner{ID: "t", Name: "T", Mode: ModeUSB, Backend: BackendAuto, USB: &USB{VendorID: "03f0", ProductID: "2b17"}})
			},
			path: "/scanners/1/usb", msg: "/scanners/0",
		},
		{
			name:   "duplicate id across printers and scanners",
			change: func(c *Config) { c.Scanners[0].ID = "p" },
			path:   "/scanners/0/id", msg: "duplicate id p: also /printers/0",
		},
		{
			name: "duplicate printer name ignoring case",
			change: func(c *Config) {
				c.Printers = append(c.Printers, Printer{ID: "q", Name: "p", Mode: ModeSimulated, Driver: Auto})
			},
			path: "/printers/1/name", msg: "/printers/0",
		},
		{
			name:   "a scanner may share the printer's name",
			change: func(c *Config) { c.Scanners[0].Name = "P" },
		},
		{
			name:   "one simulated scanner",
			change: func(c *Config) { c.Scanners[0].Mode, c.Scanners[0].USB = ModeSimulated, nil },
		},
		{
			name: "two simulated scanners",
			change: func(c *Config) {
				c.Scanners = []Scanner{
					{ID: "s", Name: "S", Mode: ModeSimulated, Backend: BackendAuto},
					{ID: "t", Name: "T", Mode: ModeSimulated, Backend: BackendAuto},
				}
			},
			path: "/scanners/1/mode", msg: "only one simulated scanner is supported",
		},
		{
			name:   "driver unknown to the catalog",
			change: func(c *Config) { c.Printers[0].Driver = "brlaser" },
			path:   "/printers/0/driver", msg: "brlaser",
		},
		{
			name:   "known driver",
			change: func(c *Config) { c.Printers[0].Driver = "hplip" },
		},
		{
			name:   "driverName with driver auto",
			change: func(c *Config) { c.Printers[0].DriverName = "samsung--scx-4300--2-0-0-en" },
		},
		{
			name: "driverName its family lists",
			change: func(c *Config) {
				c.Printers[0].Driver, c.Printers[0].DriverName = "ps", "generic--postscript-printer--en"
			},
		},
		// The index follows image updates, so a name no app lists only degrades
		// the printer (the controller reports it); the config stays valid.
		{
			name:   "driverName nobody lists",
			change: func(c *Config) { c.Printers[0].DriverName = "acme--x-en" },
		},
		{
			name: "driverName another family lists",
			change: func(c *Config) {
				c.Printers[0].Driver, c.Printers[0].DriverName = "gutenprint", "samsung--scx-4300--2-0-0-en"
			},
		},
		{
			name: "driverName with driver ipp-usb",
			change: func(c *Config) {
				c.Printers[0].Driver, c.Printers[0].DriverName = "ipp-usb", "samsung--scx-4300--2-0-0-en"
			},
			path: "/printers/0/driverName", msg: "ipp-usb",
		},
		{
			name: "driverName with mode simulated",
			change: func(c *Config) {
				c.Printers[0].Mode, c.Printers[0].USB, c.Printers[0].DriverName = ModeSimulated, nil, "samsung--scx-4300--2-0-0-en"
			},
			path: "/printers/0/driverName", msg: "simulated",
		},
		{
			name: "ignored device nobody uses",
			change: func(c *Config) {
				c.IgnoredDevices = []IgnoredDevice{{VendorID: "046d", ProductID: "c31c", Roles: []string{RolePrinter, RoleScanner}}}
			},
		},
		{
			name: "ignored scanner role of the printer's device",
			change: func(c *Config) {
				c.Scanners = []Scanner{}
				c.IgnoredDevices = []IgnoredDevice{{VendorID: "03f0", ProductID: "2b17", Roles: []string{RoleScanner}}}
			},
		},
		{
			name: "ignored scanner role in use",
			change: func(c *Config) {
				c.IgnoredDevices = []IgnoredDevice{{VendorID: "03f0", ProductID: "2b17", Roles: []string{RoleScanner}}}
			},
			path: "/ignoredDevices/0", msg: "is ignored and used: remove one (/scanners/0)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := devices()
			tt.change(&c)
			err := Validate(&c, testKnown)
			if tt.path == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var el *ErrorList
			if !errors.As(err, &el) {
				t.Fatalf("error %T %v is not *ErrorList", err, err)
			}
			e := findError(t, el.Errors, tt.path)
			if !strings.Contains(e.Message, tt.msg) {
				t.Errorf("message %q lacks %q", e.Message, tt.msg)
			}
		})
	}
}

func TestCheckAddsPositions(t *testing.T) {
	src := minimalYAML + "licenses:\n  acceptedEulas:\n    - id: no-such-eula\n      version: \"1\"\n"
	_, err := Check([]byte(src), testKnown)
	var el *ErrorList
	if !errors.As(err, &el) {
		t.Fatalf("error %T %v is not *ErrorList", err, err)
	}
	e := findError(t, el.Errors, "/licenses/acceptedEulas/0")
	if e.Line != 5 || e.Column != 7 {
		t.Errorf("position %d:%d, want 5:7", e.Line, e.Column)
	}
}

func TestCheckLocatesMissingTextSHA(t *testing.T) {
	src := minimalYAML + "licenses:\n  acceptedEulas:\n    - id: hplip-plugin\n      version: \"3.24.4\"\n"
	_, err := Check([]byte(src), testKnown)
	var el *ErrorList
	if !errors.As(err, &el) {
		t.Fatalf("error %T %v is not *ErrorList", err, err)
	}
	e := findError(t, el.Errors, "/licenses/acceptedEulas/0/textSha256")
	if e.Line != 5 || e.Column != 7 {
		t.Errorf("position %d:%d, want 5:7 (the entry)", e.Line, e.Column)
	}
}

func TestCheckLocatesEntryErrors(t *testing.T) {
	src := minimalYAML + "scanners:\n  - id: s\n    name: S\n    mode: simulated\n  - id: t\n    name: T\n    mode: simulated\n"
	_, err := Check([]byte(src), testKnown)
	var el *ErrorList
	if !errors.As(err, &el) {
		t.Fatalf("error %T %v is not *ErrorList", err, err)
	}
	e := findError(t, el.Errors, "/scanners/1/mode")
	if e.Line != 9 || e.Column != 11 {
		t.Errorf("position %d:%d, want 9:11", e.Line, e.Column)
	}
}

func TestCheck(t *testing.T) {
	c, err := Check([]byte(fullYAML), testKnown)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Home" {
		t.Errorf("name %q", c.Name)
	}
	if _, err := Check([]byte("apiVersion: pagewire/v2\n"), testKnown); err == nil {
		t.Error("Check accepted a file without name")
	}
}

// TestExamplesValid checks every example config against the catalog's families.
func TestExamplesValid(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.yaml"))
	if err != nil || len(files) < 2 {
		t.Fatalf("examples: %v %v", files, err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // fixed test inputs
		if err != nil {
			t.Fatal(err)
		}
		if IsV1(raw) {
			t.Errorf("%s is still pagewire/v1", f)
		}
		if _, err := Check(raw, testKnown); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
