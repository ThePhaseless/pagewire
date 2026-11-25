package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// v1Example is examples/pagewire.yaml as it was in pagewire/v1.
const v1Example = `# yaml-language-server: $schema=https://raw.githubusercontent.com/ThePhaseless/pagewire/main/schema/config.schema.json
#
# pagewire config with every key.
apiVersion: pagewire/v1          # required

# The UI title.
name: Living room printer

location: Living room             # printer-location

device:
  mode: usb                       # usb | simulated
  usb:
    vendorId: "03f0"              # 4 lowercase hex digits
    productId: "2754"
    serial: ""                    # optional
  driver: auto
  scanner: auto

print:
  defaults:
    media: iso_a4_210x297mm       # PWG self-describing media name
    sides: one-sided
    colorMode: auto
    quality: normal
  maxUploadMiB: 50                # 1 to 500

scan:
  defaults:
    source: platen
    resolution: 300
    colorMode: color
    format: pdf
  retentionDays: 7                # 0 (keep forever) to 3650

licenses:
  # Vendor licences you accepted.
  acceptedEulas: []
`

const v2Example = `# yaml-language-server: $schema=https://raw.githubusercontent.com/ThePhaseless/pagewire/main/schema/config.schema.json
#
# pagewire config with every key.
apiVersion: pagewire/v2          # required

# The UI title.
name: Living room printer

print:
  maxUploadMiB: 50                # 1 to 500

scan:
  retentionDays: 7                # 0 (keep forever) to 3650

licenses:
  # Vendor licences you accepted.
  acceptedEulas: []
printers:
  - id: printer
    name: Living room printer
    location: Living room
    enabled: true
    mode: usb
    usb:
      vendorId: "03f0"
      productId: "2754"
      serial: ""
    driver: auto
    defaults:
      media: iso_a4_210x297mm
      sides: one-sided
      colorMode: auto
      quality: normal
scanners:
  - id: scanner
    name: Living room printer scanner
    location: Living room
    enabled: true
    mode: usb
    usb:
      vendorId: "03f0"
      productId: "2754"
      serial: ""
    backend: auto
    defaults:
      source: platen
      resolution: 300
      colorMode: color
      format: pdf
ignoredDevices: []
`

// migrated runs Migrate on a v1 file and parses the result.
func migrated(t *testing.T, src string) ([]byte, *Config) {
	t.Helper()
	out, ok, err := Migrate([]byte(src))
	if err != nil || !ok {
		t.Fatalf("Migrate = %v, %v", ok, err)
	}
	c, err := Check(out, testKnown)
	if err != nil {
		t.Fatalf("migrated file: %v\n%s", err, out)
	}
	return out, c
}

var (
	v1Printer = PrintDefaults{Media: "iso_a4_210x297mm", Sides: "one-sided", ColorMode: "auto", Quality: "normal"}
	v1Scanner = ScanDefaults{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"}
)

func TestMigrateExample(t *testing.T) {
	out, _ := migrated(t, v1Example)
	if string(out) != v2Example {
		t.Errorf("migrated example:\n%s\nwant:\n%s", out, v2Example)
	}
}

func TestMigrate(t *testing.T) {
	samsung := &USB{VendorID: "04e8", ProductID: "342e"}
	tests := []struct {
		name string
		src  string
		want func(*Config)
	}{
		{
			name: "minimal: one usb printer and scanner without selectors, v1 defaults explicit",
			src:  "apiVersion: pagewire/v1\nname: Home\n",
			want: func(c *Config) {
				c.Printers = []Printer{{ID: "printer", Name: "Home", Enabled: true, Mode: ModeUSB, Driver: Auto, Defaults: v1Printer}}
				c.Scanners = []Scanner{{ID: "scanner", Name: "Home scanner", Enabled: true, Mode: ModeUSB, Backend: BackendAuto, Defaults: v1Scanner}}
			},
		},
		{
			name: "owner's Samsung",
			src: "apiVersion: pagewire/v1\nname: Home\nlocation: Hall\ndevice:\n  mode: usb\n  usb:\n    vendorId: \"04e8\"\n    productId: \"342e\"\n    serial: \"\"\n" +
				"  driver: auto\n  scanner: auto\nprint:\n  defaults:\n    colorMode: monochrome\n  maxUploadMiB: 20\nscan:\n  retentionDays: 30\n",
			want: func(c *Config) {
				pd := v1Printer
				pd.ColorMode = "monochrome"
				c.Printers = []Printer{{ID: "printer", Name: "Home", Location: "Hall", Enabled: true, Mode: ModeUSB, USB: samsung, Driver: Auto, Defaults: pd}}
				c.Scanners = []Scanner{{ID: "scanner", Name: "Home scanner", Location: "Hall", Enabled: true, Mode: ModeUSB, USB: samsung, Backend: BackendAuto, Defaults: v1Scanner}}
				c.Print.MaxUploadMiB, c.Scan.RetentionDays = 20, 30
			},
		},
		{
			name: "scanner none with usb: the scanner role is ignored",
			src:  "apiVersion: pagewire/v1\nname: Home\ndevice:\n  usb:\n    vendorId: \"04e8\"\n    productId: \"342e\"\n  scanner: none\n  driver: legacy\n  driverName: samsung--scx-4300--2-0-0-en\n",
			want: func(c *Config) {
				c.Printers = []Printer{{ID: "printer", Name: "Home", Enabled: true, Mode: ModeUSB, USB: samsung, Driver: "legacy", DriverName: "samsung--scx-4300--2-0-0-en", Defaults: v1Printer}}
				c.IgnoredDevices = []IgnoredDevice{{VendorID: "04e8", ProductID: "342e", Roles: []string{RoleScanner}}}
			},
		},
		{
			name: "scanner none without usb",
			src:  "apiVersion: pagewire/v1\nname: Home\ndevice:\n  scanner: none\n",
			want: func(c *Config) {
				c.Printers = []Printer{{ID: "printer", Name: "Home", Enabled: true, Mode: ModeUSB, Driver: Auto, Defaults: v1Printer}}
			},
		},
		{
			name: "simulated, scanner backend kept, long name cut",
			src:  "apiVersion: pagewire/v1\nname: " + strings.Repeat("n", 60) + "\ndevice:\n  mode: simulated\n  scanner: airsane\n",
			want: func(c *Config) {
				c.Name = strings.Repeat("n", 60)
				c.Printers = []Printer{{ID: "printer", Name: c.Name, Enabled: true, Mode: ModeSimulated, Driver: Auto, Defaults: v1Printer}}
				c.Scanners = []Scanner{{ID: "scanner", Name: c.Name + " sc", Enabled: true, Mode: ModeSimulated, Backend: "airsane", Defaults: v1Scanner}}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := migrated(t, tt.src)
			want := Default("Home")
			tt.want(&want)
			if !reflect.DeepEqual(*got, want) {
				t.Errorf("\n got  %+v\n want %+v", *got, want)
			}
		})
	}
}

func TestMigrateKeepsComments(t *testing.T) {
	out, _ := migrated(t, "# top\napiVersion: pagewire/v1\nname: Home # title\n# about printing\nprint:\n  maxUploadMiB: 20 # limit\n  defaults:\n    sides: one-sided # gone\n")
	for _, want := range []string{"# top\n", "name: Home # title\n", "# about printing\nprint:\n  maxUploadMiB: 20 # limit\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("migrated file lost %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "# gone") || strings.Contains(string(out), "Migrated from") {
		t.Errorf("migrated file:\n%s", out)
	}
	out, _ = migrated(t, "apiVersion: pagewire/v1\nname: \" -x\"\n# about scanning\nscan:\n  defaults:\n    source: adf\n")
	if !strings.Contains(string(out), "# about scanning\nscan: {}\n") || !strings.Contains(string(out), "name: \" -x scanner\"") {
		t.Errorf("migrated file:\n%s", out)
	}
}

func TestMigrateFallback(t *testing.T) {
	src := "# lost\napiVersion: pagewire/v1\nname: &n Home\nlocation: *n\nprint: {maxUploadMiB: 20, defaults: {sides: two-sided-long-edge}}\n"
	out, got := migrated(t, src)
	if !strings.HasPrefix(string(out), SchemaHeader+"# Migrated from pagewire/v1 by pagewire; comments could not be kept.\n") {
		t.Errorf("fallback output:\n%s", out)
	}
	if strings.Contains(string(out), "# lost") || strings.Contains(string(out), "{") {
		t.Errorf("fallback output is not plain block YAML:\n%s", out)
	}
	pd := v1Printer
	pd.Sides = "two-sided-long-edge"
	want := Default("Home")
	want.Printers = []Printer{{ID: "printer", Name: "Home", Location: "Home", Enabled: true, Mode: ModeUSB, Driver: Auto, Defaults: pd}}
	want.Scanners = []Scanner{{ID: "scanner", Name: "Home scanner", Location: "Home", Enabled: true, Mode: ModeUSB, Backend: BackendAuto, Defaults: v1Scanner}}
	want.Print.MaxUploadMiB = 20
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("\n got  %+v\n want %+v", *got, want)
	}
}

func TestMigrateWizardFile(t *testing.T) {
	// The v1 wizard wrote SchemaHeader plus its JSON body converted to YAML.
	body := `{"apiVersion":"pagewire/v1","name":"Home","location":"","device":{"mode":"usb","usb":{"vendorId":"04e8","productId":"342e","serial":""},"driver":"auto","scanner":"auto"},` +
		`"print":{"defaults":{"media":"iso_a4_210x297mm","sides":"one-sided","colorMode":"auto","quality":"normal"},"maxUploadMiB":50},` +
		`"scan":{"defaults":{"source":"platen","resolution":300,"colorMode":"color","format":"pdf"},"retentionDays":7},"licenses":{"acceptedEulas":[]}}`
	y, err := yaml.JSONToYAML([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, c := migrated(t, SchemaHeader+string(y))
	if !strings.HasPrefix(string(out), SchemaHeader) || strings.Contains(string(out), "Migrated from") {
		t.Errorf("wizard file did not keep its header on the comment-keeping path:\n%s", out)
	}
	if len(c.Printers) != 1 || len(c.Scanners) != 1 || c.Printers[0].USB == nil || c.Scanners[0].USB == nil {
		t.Errorf("migrated %+v", c)
	}
}

func TestMigrateV1Errors(t *testing.T) {
	src := "apiVersion: pagewire/v1\nname: Home\nscan:\n  defaults:\n    resolution: high\n"
	for name, run := range map[string]func() error{
		"Migrate": func() error { _, _, err := Migrate([]byte(src)); return err },
		"Check":   func() error { _, err := Check([]byte(src), testKnown); return err },
	} {
		var el *ErrorList
		if err := run(); !errors.As(err, &el) {
			t.Fatalf("%s: error %T %v is not *ErrorList", name, err, err)
		}
		e := findError(t, el.Errors, "/scan/defaults/resolution")
		if e.Line != 5 || e.Column != 17 || !strings.Contains(e.Message, "integer") {
			t.Errorf("%s: %+v, want the v1 position 5:17", name, e)
		}
	}
}

func TestMigrateLeavesV2Alone(t *testing.T) {
	for _, src := range []string{fullYAML, minimalYAML, "name: [\n", "apiVersion: pagewire/v1x\nname: x\n", ""} {
		out, ok, err := Migrate([]byte(src))
		if err != nil || ok || string(out) != src {
			t.Errorf("Migrate(%q) = %q, %v, %v; want it unchanged", src, out, ok, err)
		}
	}
}

func TestIsV1(t *testing.T) {
	for src, want := range map[string]bool{
		"apiVersion: pagewire/v1\n":            true,
		"apiVersion: \"pagewire/v1\"\nname: x": true,
		`{"apiVersion": "pagewire/v1"}`:        true,
		"apiVersion: pagewire/v2\n":            false,
		"name: x\n":                            false,
		"- apiVersion: pagewire/v1\n":          false,
		"apiVersion: [\n":                      false,
		"":                                     false,
	} {
		if got := IsV1([]byte(src)); got != want {
			t.Errorf("IsV1(%q) = %v", src, got)
		}
	}
}

func TestCheckV1EqualsCheckMigrated(t *testing.T) {
	for _, src := range [][]byte{[]byte(v1Example), []byte("apiVersion: pagewire/v1\nname: Home\ndevice:\n  scanner: none\n")} {
		direct, err := Check(src, testKnown)
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := Migrate(src)
		if err != nil {
			t.Fatal(err)
		}
		via, err := Check(out, testKnown)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(direct, via) {
			t.Errorf("Check(v1) = %+v\nCheck(Migrate(v1)) = %+v", direct, via)
		}
	}
}
