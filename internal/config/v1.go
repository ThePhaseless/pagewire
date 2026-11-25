package config

import "github.com/goccy/go-yaml/ast"

// The pagewire/v1 file, kept only to migrate it (see Migrate). These types are
// a copy of the v1 Config and are never reflected into a schema.

type v1Config struct {
	APIVersion string        `yaml:"apiVersion" json:"apiVersion" validate:"required,eq=pagewire/v1"`
	Name       string        `yaml:"name" json:"name" validate:"required,max=63,nocontrol,startsnotwith=-"`
	Location   string        `yaml:"location" json:"location" validate:"max=127,nocontrol"`
	Device     v1Device      `yaml:"device" json:"device"`
	Print      v1PrintConfig `yaml:"print" json:"print"`
	Scan       v1ScanConfig  `yaml:"scan" json:"scan"`
	Licenses   Licenses      `yaml:"licenses" json:"licenses"`
}

type v1Device struct {
	Mode       string `yaml:"mode" json:"mode" validate:"oneof=usb simulated"`
	USB        *USB   `yaml:"usb,omitempty" json:"usb,omitempty"`
	Driver     string `yaml:"driver" json:"driver" validate:"oneof=auto ipp-usb ps hplip gutenprint ghostscript legacy"`
	DriverName string `yaml:"driverName,omitempty" json:"driverName,omitempty" validate:"max=255,nocontrol"`
	Scanner    string `yaml:"scanner" json:"scanner" validate:"oneof=auto ipp-usb airsane none"`
}

type v1PrintConfig struct {
	Defaults     v1PrintDefaults `yaml:"defaults" json:"defaults"`
	MaxUploadMiB int             `yaml:"maxUploadMiB" json:"maxUploadMiB" validate:"min=1,max=500"`
}

type v1PrintDefaults struct {
	Media     string `yaml:"media" json:"media" validate:"pwgmedia"`
	Sides     string `yaml:"sides" json:"sides" validate:"oneof=one-sided two-sided-long-edge two-sided-short-edge"`
	ColorMode string `yaml:"colorMode" json:"colorMode" validate:"oneof=auto color monochrome"`
	Quality   string `yaml:"quality" json:"quality" validate:"oneof=draft normal high"`
}

type v1ScanConfig struct {
	Defaults      v1ScanDefaults `yaml:"defaults" json:"defaults"`
	RetentionDays int            `yaml:"retentionDays" json:"retentionDays" validate:"min=0,max=3650"`
}

type v1ScanDefaults struct {
	Source     string `yaml:"source" json:"source" validate:"oneof=platen adf adf-duplex"`
	Resolution int    `yaml:"resolution" json:"resolution" validate:"min=75,max=1200"`
	ColorMode  string `yaml:"colorMode" json:"colorMode" validate:"oneof=color grayscale blackandwhite"`
	Format     string `yaml:"format" json:"format" validate:"oneof=pdf jpeg"`
}

// v1ScannerNone is v1's device.scanner value for "no scanner".
const v1ScannerNone = "none"

// v1Default is v1's Default: every default filled in, apiVersion left empty
// so its absence is reported.
func v1Default() v1Config {
	return v1Config{
		Device: v1Device{Mode: ModeUSB, Driver: Auto, Scanner: Auto},
		Print: v1PrintConfig{
			Defaults:     v1PrintDefaults{Media: "iso_a4_210x297mm", Sides: "one-sided", ColorMode: "auto", Quality: "normal"},
			MaxUploadMiB: 50,
		},
		Scan: v1ScanConfig{
			Defaults:      v1ScanDefaults{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"},
			RetentionDays: 7,
		},
		Licenses: Licenses{AcceptedEULAs: []EULARef{}},
	}
}

// parseV1 decodes a pagewire/v1 file with the same strict decoding, struct
// checks and positions as Parse.
func parseV1(b []byte) (*v1Config, *ast.File, error) {
	c := v1Default()
	file, err := decode(b, &c, func(ast.Node) {
		if c.Licenses.AcceptedEULAs == nil {
			c.Licenses.AcceptedEULAs = []EULARef{}
		}
	})
	if err != nil {
		return nil, nil, err
	}
	return &c, file, nil
}
