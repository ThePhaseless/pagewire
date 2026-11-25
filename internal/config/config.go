// Package config holds the pagewire/v1 config file: types, defaults, strict
// parsing with positions, and semantic validation.
package config

import (
	"fmt"
	"strings"
)

// APIVersion is the only accepted apiVersion.
const APIVersion = "pagewire/v1"

// Enum values other packages compare against.
const (
	ModeUSB       = "usb"
	ModeSimulated = "simulated"
	Auto          = "auto" // device.driver and device.scanner
	ScannerNone   = "none" // device.scanner
)

// Config is the pagewire/v1 file. Every field except apiVersion and name has
// a default (see Default).
type Config struct {
	APIVersion string      `yaml:"apiVersion" json:"apiVersion" validate:"required,eq=pagewire/v1" jsonschema:"required"`
	Name       string      `yaml:"name" json:"name" validate:"required,max=63,nocontrol,startsnotwith=-" jsonschema:"required,minLength=1,maxLength=63,pattern=^[^\\-\\x00-\\x1f\\x7f-\\x9f][^\\x00-\\x1f\\x7f-\\x9f]*$" jsonschema_description:"Required, 1 to 63 characters, no control characters, not starting with -. UI title, and the queue and DNS-SD name of PAPPL and simulated printers. ipp-usb and AirSane advertise the device's own names."`
	Location   string      `yaml:"location" json:"location" validate:"max=127,nocontrol" jsonschema:"maxLength=127,pattern=^[^\\x00-\\x1f\\x7f-\\x9f]*$" jsonschema_description:"printer-location, 0 to 127 characters."`
	Device     Device      `yaml:"device" json:"device"`
	Print      PrintConfig `yaml:"print" json:"print"`
	Scan       ScanConfig  `yaml:"scan" json:"scan"`
	Licenses   Licenses    `yaml:"licenses" json:"licenses"`
}

// Device selects the device and its drivers.
type Device struct {
	Mode    string `yaml:"mode" json:"mode" validate:"oneof=usb simulated" jsonschema:"enum=usb,enum=simulated,default=usb" jsonschema_description:"usb, or simulated (test printer and SANE test scanner, no USB resource)."`
	USB     *USB   `yaml:"usb,omitempty" json:"usb,omitempty" jsonschema_description:"Optional selector when several USB devices are visible. Only allowed with mode usb."`
	Driver  string `yaml:"driver" json:"driver" validate:"oneof=auto ipp-usb ps hplip gutenprint ghostscript" jsonschema:"enum=auto,enum=ipp-usb,enum=ps,enum=hplip,enum=gutenprint,enum=ghostscript,default=auto"`
	Scanner string `yaml:"scanner" json:"scanner" validate:"oneof=auto ipp-usb airsane none" jsonschema:"enum=auto,enum=ipp-usb,enum=airsane,enum=none,default=auto"`
}

// USB selects one device by its descriptors.
type USB struct {
	VendorID  string `yaml:"vendorId" json:"vendorId" validate:"hex4" jsonschema:"pattern=^[0-9a-f]{4}$"`
	ProductID string `yaml:"productId" json:"productId" validate:"hex4" jsonschema:"pattern=^[0-9a-f]{4}$"`
	Serial    string `yaml:"serial" json:"serial" validate:"max=126,nocontrol" jsonschema:"maxLength=126,pattern=^[^\\x00-\\x1f\\x7f-\\x9f]*$" jsonschema_description:"USB serial number, at most 126 characters, no control characters."`
}

// PrintConfig holds print defaults and limits.
type PrintConfig struct {
	Defaults     PrintDefaults `yaml:"defaults" json:"defaults"`
	MaxUploadMiB int           `yaml:"maxUploadMiB" json:"maxUploadMiB" validate:"min=1,max=500" jsonschema:"minimum=1,maximum=500,default=50"`
}

// PrintDefaults are the job defaults for web print and the PAPPL queue.
type PrintDefaults struct {
	Media     string `yaml:"media" json:"media" validate:"pwgmedia" jsonschema:"pattern=^[a-z0-9-]+_[a-z0-9.-]+_[0-9.]+x[0-9.]+(mm|in)$,default=iso_a4_210x297mm" jsonschema_description:"PWG self-describing media name."`
	Sides     string `yaml:"sides" json:"sides" validate:"oneof=one-sided two-sided-long-edge two-sided-short-edge" jsonschema:"enum=one-sided,enum=two-sided-long-edge,enum=two-sided-short-edge,default=one-sided"`
	ColorMode string `yaml:"colorMode" json:"colorMode" validate:"oneof=auto color monochrome" jsonschema:"enum=auto,enum=color,enum=monochrome,default=auto"`
	Quality   string `yaml:"quality" json:"quality" validate:"oneof=draft normal high" jsonschema:"enum=draft,enum=normal,enum=high,default=normal"`
}

// ScanConfig holds scan defaults and retention.
type ScanConfig struct {
	Defaults      ScanDefaults `yaml:"defaults" json:"defaults"`
	RetentionDays int          `yaml:"retentionDays" json:"retentionDays" validate:"min=0,max=3650" jsonschema:"minimum=0,maximum=3650,default=7" jsonschema_description:"Days to keep scans; 0 keeps them forever."`
}

// ScanDefaults are the defaults of the web scan form.
type ScanDefaults struct {
	Source     string `yaml:"source" json:"source" validate:"oneof=platen adf adf-duplex" jsonschema:"enum=platen,enum=adf,enum=adf-duplex,default=platen"`
	Resolution int    `yaml:"resolution" json:"resolution" validate:"min=75,max=1200" jsonschema:"minimum=75,maximum=1200,default=300"`
	ColorMode  string `yaml:"colorMode" json:"colorMode" validate:"oneof=color grayscale blackandwhite" jsonschema:"enum=color,enum=grayscale,enum=blackandwhite,default=color"`
	Format     string `yaml:"format" json:"format" validate:"oneof=pdf jpeg" jsonschema:"enum=pdf,enum=jpeg,default=pdf"`
}

// Licenses records accepted vendor EULAs.
type Licenses struct {
	AcceptedEULAs []EULARef `yaml:"acceptedEulas" json:"acceptedEulas" validate:"unique,dive" jsonschema:"uniqueItems=true" jsonschema_description:"Each {id, version} must exist in the catalog and appear once; textSha256 is required."`
}

// EULARef names one EULA version and, in acceptedEulas, the sha256 of the
// licence text the user accepted. The blob is installed only while the text
// in the vendor archive still has that sha256.
type EULARef struct {
	ID         string `yaml:"id" json:"id" validate:"required"`
	Version    string `yaml:"version" json:"version" validate:"required"`
	TextSHA256 string `yaml:"textSha256,omitempty" json:"textSha256,omitempty" jsonschema:"pattern=^[0-9a-f]{64}$" jsonschema_description:"Lowercase hex sha256 of the accepted licence text. Settings writes it when you accept; required in acceptedEulas."`
}

// Default returns the config the wizard starts from: every default filled in,
// mode usb, driver and scanner auto, no EULAs accepted.
func Default(name string) Config {
	return Config{
		APIVersion: APIVersion,
		Name:       name,
		Device:     Device{Mode: ModeUSB, Driver: Auto, Scanner: Auto},
		Print: PrintConfig{
			Defaults:     PrintDefaults{Media: "iso_a4_210x297mm", Sides: "one-sided", ColorMode: "auto", Quality: "normal"},
			MaxUploadMiB: 50,
		},
		Scan: ScanConfig{
			Defaults:      ScanDefaults{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"},
			RetentionDays: 7,
		},
		Licenses: Licenses{AcceptedEULAs: []EULARef{}},
	}
}

// FieldError is one problem in a config file. Path is a JSON pointer such as
// /device/usb/vendorId ("" for the whole file). Line and Column are 1-based,
// 0 when unknown.
type FieldError struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}

// ErrorList is the error type of Parse, Validate and Check.
type ErrorList struct {
	Errors []FieldError
}

func (e *ErrorList) Error() string {
	parts := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		p := fe.Path
		if p == "" {
			p = "/"
		}
		if fe.Line > 0 {
			parts[i] = fmt.Sprintf("%d:%d: %s: %s", fe.Line, fe.Column, p, fe.Message)
		} else {
			parts[i] = fmt.Sprintf("%s: %s", p, fe.Message)
		}
	}
	return strings.Join(parts, "; ")
}
