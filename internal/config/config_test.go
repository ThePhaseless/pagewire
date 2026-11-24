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
	if c.Device.Mode != ModeUSB || c.Device.Driver != Auto || c.Device.Scanner != Auto || c.Device.USB != nil {
		t.Errorf("device %+v", c.Device)
	}
	if c.Licenses.AcceptedEULAs == nil || len(c.Licenses.AcceptedEULAs) != 0 {
		t.Errorf("acceptedEulas %#v, want empty non-nil", c.Licenses.AcceptedEULAs)
	}
	if c.Print.MaxUploadMiB != 50 || c.Scan.RetentionDays != 7 || c.Print.Defaults.Media != "iso_a4_210x297mm" || c.Scan.Defaults.Resolution != 300 {
		t.Errorf("defaults %+v %+v", c.Print, c.Scan)
	}
}

func TestDefaultRoundTripsThroughParse(t *testing.T) {
	want := Default("Living room printer")
	b, err := yaml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(b)
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("\n got  %+v\n want %+v", *got, want)
	}
	if strings.Contains(string(b), "usb:") {
		t.Errorf("YAML of a config without device.usb has a usb key:\n%s", b)
	}
}

func TestJSONOmitsAbsentUSB(t *testing.T) {
	b, err := json.Marshal(Default("Printer"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"usb":`) {
		t.Errorf("JSON of a config without device.usb has a usb key: %s", b)
	}
	if !strings.Contains(string(b), `"acceptedEulas":[]`) {
		t.Errorf("acceptedEulas not an empty list: %s", b)
	}

	c := Default("Printer")
	c.Device.USB = &USB{VendorID: "03f0", ProductID: "2b17"}
	b, err = json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"usb":{"vendorId":"03f0","productId":"2b17","serial":""}`) {
		t.Errorf("usb selector JSON: %s", b)
	}
}

func TestErrorListError(t *testing.T) {
	el := &ErrorList{Errors: []FieldError{{Path: "/name", Line: 2, Column: 7, Message: "is required"}, {Path: "/device/mode", Message: "bad"}}}
	got := el.Error()
	for _, want := range []string{"2:7", "/name", "is required", "/device/mode", "bad"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q lacks %q", got, want)
		}
	}
}

func TestNumberLikeStringsRoundTrip(t *testing.T) {
	want := Default("2024")
	want.Location = "true"
	want.Device.USB = &USB{VendorID: "0482", ProductID: "0001", Serial: "012345"}
	want.Licenses.AcceptedEULAs = []EULARef{{ID: "hplip-plugin", Version: "3.24"}}
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
		{"device:\n  usb:\n    vendorId: 0482\n    productId: \"0001\"\n", "0482"},
		{"device:\n  usb:\n    vendorId: \"0482\"\n    productId: \"0001\"\n    serial: 012345\n", "012345"},
		{"licenses:\n  acceptedEulas:\n    - {id: a, version: 3.20}\n", "3.20"},
	} {
		c, err := Parse([]byte(minimalYAML + tt.src))
		if err != nil {
			continue // a clear rejection is fine
		}
		var vals []string
		if c.Device.USB != nil {
			vals = append(vals, c.Device.USB.VendorID, c.Device.USB.Serial)
		}
		for _, e := range c.Licenses.AcceptedEULAs {
			vals = append(vals, e.Version)
		}
		if !slices.Contains(vals, tt.want) {
			t.Errorf("%q parsed to %v, want the literal %q or an error", tt.src, vals, tt.want)
		}
	}
}
