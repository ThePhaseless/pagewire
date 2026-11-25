// Package config holds the pagewire/v2 config file: types, defaults, strict
// parsing with positions, semantic validation, and the migration of
// pagewire/v1 files.
package config

import (
	"fmt"
	"strings"
)

// APIVersion is the apiVersion of the current file format. APIVersionV1 files
// are migrated (see Migrate).
const (
	APIVersion   = "pagewire/v2"
	APIVersionV1 = "pagewire/v1"
)

// SchemaHeader is the first line of a config file pagewire writes. It is a
// literal because this package cannot import internal/schema; a test there
// checks it against schema.ConfigID.
const SchemaHeader = "# yaml-language-server: $schema=https://raw.githubusercontent.com/ThePhaseless/pagewire/main/schema/config.schema.json\n"

// Enum values other packages compare against.
const (
	ModeUSB       = "usb"
	ModeSimulated = "simulated"
	Auto          = "auto" // printers[].driver
	BackendAuto   = "auto" // scanners[].backend
	RolePrinter   = "printer"
	RoleScanner   = "scanner"
)

// Config is the pagewire/v2 file. Every field except apiVersion and name has
// a default (see Default).
type Config struct {
	APIVersion     string          `yaml:"apiVersion" json:"apiVersion" validate:"required,eq=pagewire/v2" jsonschema:"required"`
	Name           string          `yaml:"name" json:"name" validate:"required,max=63,nocontrol,startsnotwith=-" jsonschema:"required,minLength=1,maxLength=63,pattern=^[^\\-\\x00-\\x1f\\x7f-\\x9f][^\\x00-\\x1f\\x7f-\\x9f]*$" jsonschema_description:"Required, 1 to 63 characters, no control characters, not starting with -. The UI title."`
	Printers       []Printer       `yaml:"printers" json:"printers" validate:"dive" jsonschema_description:"Printers pagewire serves (default [])."`
	Scanners       []Scanner       `yaml:"scanners" json:"scanners" validate:"dive" jsonschema_description:"Scanners pagewire serves (default [])."`
	IgnoredDevices []IgnoredDevice `yaml:"ignoredDevices" json:"ignoredDevices" validate:"dive" jsonschema_description:"USB devices the setup wizard does not offer again (default [])."`
	Print          PrintConfig     `yaml:"print" json:"print"`
	Scan           ScanConfig      `yaml:"scan" json:"scan"`
	Licenses       Licenses        `yaml:"licenses" json:"licenses"`
}

// Printer is one printer entry.
type Printer struct {
	ID       string `yaml:"id" json:"id" validate:"required,entryid" jsonschema:"required,pattern=^[a-z0-9]([a-z0-9-]{0\\,30}[a-z0-9])?$" jsonschema_description:"Required, 1 to 32 lower-case letters, digits or dashes, not starting or ending with a dash. Unique across printers and scanners."`
	Name     string `yaml:"name" json:"name" validate:"required,max=63,nocontrol,startsnotwith=-" jsonschema:"required,minLength=1,maxLength=63,pattern=^[^\\-\\x00-\\x1f\\x7f-\\x9f][^\\x00-\\x1f\\x7f-\\x9f]*$" jsonschema_description:"Required, 1 to 63 characters, no control characters, not starting with -, unique among printers (ignoring case). The queue and DNS-SD name of PAPPL and simulated printers; ipp-usb and AirSane advertise the device's own names."`
	Location string `yaml:"location" json:"location" validate:"max=127,nocontrol" jsonschema:"maxLength=127,pattern=^[^\\x00-\\x1f\\x7f-\\x9f]*$" jsonschema_description:"printer-location, 0 to 127 characters."`
	Enabled  bool   `yaml:"enabled" json:"enabled" jsonschema:"default=true" jsonschema_description:"false keeps the entry but does not serve the printer (default true)."`
	Mode     string `yaml:"mode" json:"mode" validate:"oneof=usb simulated" jsonschema:"enum=usb,enum=simulated,default=usb" jsonschema_description:"usb, or simulated (a test printer, no USB resource)."`
	USB      *USB   `yaml:"usb,omitempty" json:"usb,omitempty" jsonschema_description:"Selects the USB device. Only allowed with mode usb; at most one usb printer may leave it out."`
	Driver   string `yaml:"driver" json:"driver" validate:"oneof=auto ipp-usb ps hplip gutenprint ghostscript legacy" jsonschema:"enum=auto,enum=ipp-usb,enum=ps,enum=hplip,enum=gutenprint,enum=ghostscript,enum=legacy,default=auto"`
	// DriverName pins the PAPPL driver; "" lets the dispatcher choose.
	DriverName string        `yaml:"driverName,omitempty" json:"driverName,omitempty" validate:"max=255,nocontrol" jsonschema:"maxLength=255,pattern=^[^\\x00-\\x1f\\x7f-\\x9f]*$" jsonschema_description:"Optional exact driver name from the printer application, at most 255 characters. With driver auto, the first family that lists it runs; with a family, that family's driver of that name runs. A name that is not listed leaves pagewire's own choice running (within the family, when driver names one) and the printer degraded. Not allowed with driver ipp-usb or mode simulated."`
	Defaults   PrintDefaults `yaml:"defaults" json:"defaults"`
}

// Simulated reports whether the printer is a virtual test printer.
func (p Printer) Simulated() bool { return p.Mode == ModeSimulated }

// Scanner is one scanner entry.
type Scanner struct {
	ID       string       `yaml:"id" json:"id" validate:"required,entryid" jsonschema:"required,pattern=^[a-z0-9]([a-z0-9-]{0\\,30}[a-z0-9])?$" jsonschema_description:"Required, 1 to 32 lower-case letters, digits or dashes, not starting or ending with a dash. Unique across printers and scanners."`
	Name     string       `yaml:"name" json:"name" validate:"required,max=63,nocontrol,startsnotwith=-" jsonschema:"required,minLength=1,maxLength=63,pattern=^[^\\-\\x00-\\x1f\\x7f-\\x9f][^\\x00-\\x1f\\x7f-\\x9f]*$" jsonschema_description:"Required, 1 to 63 characters, no control characters, not starting with -. The scanner's name in the UI; ipp-usb and AirSane advertise the device's own name."`
	Location string       `yaml:"location" json:"location" validate:"max=127,nocontrol" jsonschema:"maxLength=127,pattern=^[^\\x00-\\x1f\\x7f-\\x9f]*$" jsonschema_description:"0 to 127 characters."`
	Enabled  bool         `yaml:"enabled" json:"enabled" jsonschema:"default=true" jsonschema_description:"false keeps the entry but does not serve the scanner (default true)."`
	Mode     string       `yaml:"mode" json:"mode" validate:"oneof=usb simulated" jsonschema:"enum=usb,enum=simulated,default=usb" jsonschema_description:"usb, or simulated (the SANE test scanner, no USB resource). At most one scanner may be simulated."`
	USB      *USB         `yaml:"usb,omitempty" json:"usb,omitempty" jsonschema_description:"Selects the USB device. Only allowed with mode usb; at most one usb scanner may leave it out."`
	Backend  string       `yaml:"backend" json:"backend" validate:"oneof=auto ipp-usb airsane" jsonschema:"enum=auto,enum=ipp-usb,enum=airsane,default=auto"`
	Defaults ScanDefaults `yaml:"defaults" json:"defaults"`
}

// Simulated reports whether the scanner is the virtual test scanner.
func (s Scanner) Simulated() bool { return s.Mode == ModeSimulated }

// USB selects one device by its descriptors.
type USB struct {
	VendorID  string `yaml:"vendorId" json:"vendorId" validate:"hex4" jsonschema:"pattern=^[0-9a-f]{4}$"`
	ProductID string `yaml:"productId" json:"productId" validate:"hex4" jsonschema:"pattern=^[0-9a-f]{4}$"`
	Serial    string `yaml:"serial" json:"serial" validate:"max=126,nocontrol" jsonschema:"maxLength=126,pattern=^[^\\x00-\\x1f\\x7f-\\x9f]*$" jsonschema_description:"USB serial number, at most 126 characters, no control characters."`
}

// IgnoredDevice is a USB device, and the roles of it, the wizard does not
// offer again.
type IgnoredDevice struct {
	VendorID  string   `yaml:"vendorId" json:"vendorId" validate:"hex4" jsonschema:"pattern=^[0-9a-f]{4}$"`
	ProductID string   `yaml:"productId" json:"productId" validate:"hex4" jsonschema:"pattern=^[0-9a-f]{4}$"`
	Serial    string   `yaml:"serial" json:"serial" validate:"max=126,nocontrol" jsonschema:"maxLength=126,pattern=^[^\\x00-\\x1f\\x7f-\\x9f]*$" jsonschema_description:"USB serial number, at most 126 characters, no control characters."`
	Roles     []string `yaml:"roles" json:"roles" validate:"min=1,unique,dive,oneof=printer scanner" jsonschema:"minItems=1,uniqueItems=true,enum=printer,enum=scanner" jsonschema_description:"printer, scanner or both."`
}

// PrintConfig holds print limits.
type PrintConfig struct {
	MaxUploadMiB int `yaml:"maxUploadMiB" json:"maxUploadMiB" validate:"min=1,max=500" jsonschema:"minimum=1,maximum=500,default=50"`
}

// PrintDefaults are a printer's job defaults for web print and its PAPPL
// queue. Every key is optional: an absent one ("" or 0) means the printer's
// own default. device.PrintSettings must keep exactly these field names,
// order and types, because callers convert between the two.
type PrintDefaults struct {
	Media           string `yaml:"media,omitempty" json:"media,omitempty" validate:"omitempty,pwgmedia" jsonschema:"pattern=^[a-z0-9-]+_[a-z0-9.-]+_[0-9.]+x[0-9.]+(mm|in)$" jsonschema_description:"PWG self-describing media name, such as iso_a4_210x297mm."`
	MediaSource     string `yaml:"mediaSource,omitempty" json:"mediaSource,omitempty" validate:"omitempty,ippkeyword" jsonschema:"pattern=^[a-z0-9][a-z0-9._-]{0\\,254}$" jsonschema_description:"Tray keyword the printer reports, such as auto, main, tray-1 or manual."`
	MediaType       string `yaml:"mediaType,omitempty" json:"mediaType,omitempty" validate:"omitempty,ippkeyword" jsonschema:"pattern=^[a-z0-9][a-z0-9._-]{0\\,254}$" jsonschema_description:"Media type keyword the printer reports, such as stationery or photographic-glossy."`
	Sides           string `yaml:"sides,omitempty" json:"sides,omitempty" validate:"omitempty,oneof=one-sided two-sided-long-edge two-sided-short-edge" jsonschema:"enum=one-sided,enum=two-sided-long-edge,enum=two-sided-short-edge"`
	ColorMode       string `yaml:"colorMode,omitempty" json:"colorMode,omitempty" validate:"omitempty,oneof=auto color monochrome" jsonschema:"enum=auto,enum=color,enum=monochrome"`
	Quality         string `yaml:"quality,omitempty" json:"quality,omitempty" validate:"omitempty,oneof=draft normal high" jsonschema:"enum=draft,enum=normal,enum=high"`
	Resolution      int    `yaml:"resolution,omitempty" json:"resolution,omitempty" validate:"omitempty,min=75,max=2400" jsonschema:"minimum=75,maximum=2400" jsonschema_description:"Dots per inch."`
	ContentOptimize string `yaml:"contentOptimize,omitempty" json:"contentOptimize,omitempty" validate:"omitempty,oneof=auto graphic photo text text-and-graphic" jsonschema:"enum=auto,enum=graphic,enum=photo,enum=text,enum=text-and-graphic"`
	Orientation     string `yaml:"orientation,omitempty" json:"orientation,omitempty" validate:"omitempty,oneof=portrait landscape reverse-landscape reverse-portrait" jsonschema:"enum=portrait,enum=landscape,enum=reverse-landscape,enum=reverse-portrait"`
	NumberUp        int    `yaml:"numberUp,omitempty" json:"numberUp,omitempty" validate:"omitempty,min=1,max=16" jsonschema:"minimum=1,maximum=16" jsonschema_description:"Pages per sheet."`
	PrintScaling    string `yaml:"printScaling,omitempty" json:"printScaling,omitempty" validate:"omitempty,oneof=auto auto-fit fill fit none" jsonschema:"enum=auto,enum=auto-fit,enum=fill,enum=fit,enum=none"`
	OutputBin       string `yaml:"outputBin,omitempty" json:"outputBin,omitempty" validate:"omitempty,ippkeyword" jsonschema:"pattern=^[a-z0-9][a-z0-9._-]{0\\,254}$" jsonschema_description:"Output bin keyword the printer reports, such as face-down or top."`
	Finishings      string `yaml:"finishings,omitempty" json:"finishings,omitempty" validate:"omitempty,ippkeyword" jsonschema:"pattern=^[a-z0-9][a-z0-9._-]{0\\,254}$" jsonschema_description:"One IANA finishings keyword, such as none, staple or punch."`
}

// ScanConfig holds scan retention.
type ScanConfig struct {
	RetentionDays int `yaml:"retentionDays" json:"retentionDays" validate:"min=0,max=3650" jsonschema:"minimum=0,maximum=3650,default=7" jsonschema_description:"Days to keep scans; 0 keeps them forever."`
}

// ScanDefaults are a scanner's defaults for the web scan form. Every key is
// optional: an absent one ("" or 0) means the scanner's own default (pdf for
// format).
type ScanDefaults struct {
	Source     string `yaml:"source,omitempty" json:"source,omitempty" validate:"omitempty,oneof=platen adf adf-duplex" jsonschema:"enum=platen,enum=adf,enum=adf-duplex"`
	Resolution int    `yaml:"resolution,omitempty" json:"resolution,omitempty" validate:"omitempty,min=75,max=1200" jsonschema:"minimum=75,maximum=1200" jsonschema_description:"Dots per inch."`
	ColorMode  string `yaml:"colorMode,omitempty" json:"colorMode,omitempty" validate:"omitempty,oneof=color grayscale blackandwhite" jsonschema:"enum=color,enum=grayscale,enum=blackandwhite"`
	Intent     string `yaml:"intent,omitempty" json:"intent,omitempty" validate:"omitempty,oneof=document text-and-graphic photo preview object business-card" jsonschema:"enum=document,enum=text-and-graphic,enum=photo,enum=preview,enum=object,enum=business-card"`
	Format     string `yaml:"format,omitempty" json:"format,omitempty" validate:"omitempty,oneof=pdf jpeg" jsonschema:"enum=pdf,enum=jpeg"`
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

// Default returns the config the wizard starts from: no devices, the default
// limits, no EULAs accepted.
func Default(name string) Config {
	return Config{
		APIVersion:     APIVersion,
		Name:           name,
		Printers:       []Printer{},
		Scanners:       []Scanner{},
		IgnoredDevices: []IgnoredDevice{},
		Print:          PrintConfig{MaxUploadMiB: 50},
		Scan:           ScanConfig{RetentionDays: 7},
		Licenses:       Licenses{AcceptedEULAs: []EULARef{}},
	}
}

// FieldError is one problem in a config file. Path is a JSON pointer such as
// /printers/0/usb/vendorId ("" for the whole file). Line and Column are 1-based,
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
