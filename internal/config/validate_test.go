package config

import (
	"errors"
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
		{name: "default is valid", change: func(*Config) {}},
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
			name: "usb selector with simulated mode",
			change: func(c *Config) {
				c.Device.Mode = ModeSimulated
				c.Device.USB = &USB{VendorID: "03f0", ProductID: "2b17"}
			},
			path: "/device/usb", msg: "mode: usb",
		},
		{
			name:   "usb selector with usb mode",
			change: func(c *Config) { c.Device.USB = &USB{VendorID: "03f0", ProductID: "2b17"} },
		},
		{
			name:   "driver unknown to the catalog",
			change: func(c *Config) { c.Device.Driver = "brlaser" },
			path:   "/device/driver", msg: "brlaser",
		},
		{
			name:   "known driver",
			change: func(c *Config) { c.Device.Driver = "hplip" },
		},
		{
			name:   "driverName with driver auto",
			change: func(c *Config) { c.Device.DriverName = "samsung--scx-4300--2-0-0-en" },
		},
		{
			name: "driverName its family lists",
			change: func(c *Config) {
				c.Device.Driver, c.Device.DriverName = "ps", "generic--postscript-printer--en"
			},
		},
		// The index follows image updates, so a name no app lists only degrades
		// the device (the controller reports it); the config stays valid.
		{
			name:   "driverName nobody lists",
			change: func(c *Config) { c.Device.DriverName = "acme--x-en" },
		},
		{
			name: "driverName another family lists",
			change: func(c *Config) {
				c.Device.Driver, c.Device.DriverName = "gutenprint", "samsung--scx-4300--2-0-0-en"
			},
		},
		{
			name: "driverName with driver ipp-usb",
			change: func(c *Config) {
				c.Device.Driver, c.Device.DriverName = "ipp-usb", "samsung--scx-4300--2-0-0-en"
			},
			path: "/device/driverName", msg: "ipp-usb",
		},
		{
			name: "driverName with mode simulated",
			change: func(c *Config) {
				c.Device.Mode, c.Device.DriverName = ModeSimulated, "samsung--scx-4300--2-0-0-en"
			},
			path: "/device/driverName", msg: "simulated",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default("Printer")
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

func TestCheck(t *testing.T) {
	c, err := Check([]byte(fullYAML), testKnown)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Living room printer" {
		t.Errorf("name %q", c.Name)
	}
	if _, err := Check([]byte("apiVersion: pagewire/v1\n"), testKnown); err == nil {
		t.Error("Check accepted a file without name")
	}
}
