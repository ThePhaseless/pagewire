package config

import (
	"errors"
	"strings"
	"testing"
)

var testKnown = Known{
	EULAs:    []EULARef{{ID: "hplip-plugin", Version: "3.24.4"}},
	Printers: []string{"ipp-usb", "ps", "hplip", "gutenprint", "ghostscript", "sim"},
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
			name:   "accepted known EULA",
			change: func(c *Config) { c.Licenses.AcceptedEULAs = []EULARef{{ID: "hplip-plugin", Version: "3.24.4"}} },
		},
		{
			name:   "unknown EULA",
			change: func(c *Config) { c.Licenses.AcceptedEULAs = []EULARef{{ID: "no-such-eula", Version: "1"}} },
			path:   "/licenses/acceptedEulas/0", msg: "no-such-eula",
		},
		{
			name: "known EULA with the wrong version",
			change: func(c *Config) {
				c.Licenses.AcceptedEULAs = []EULARef{{ID: "hplip-plugin", Version: "3.24.4"}, {ID: "hplip-plugin", Version: "3.20.1"}}
			},
			path: "/licenses/acceptedEulas/1", msg: "3.20.1",
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

func TestCheck(t *testing.T) {
	c, err := Check([]byte(fullYAML), testKnown)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Living room printer" {
		t.Errorf("name %q", c.Name)
	}
	if _, err := Check([]byte("apiVersion: printscan/v1\n"), testKnown); err == nil {
		t.Error("Check accepted a file without name")
	}
}
