package config

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/ThePhaseless/pagewire/internal/config/yamledit"
)

// Migration ids of the entries a v1 file becomes.
const (
	v1PrinterID = "printer"
	v1ScannerID = "scanner"
)

// migratedHeader follows SchemaHeader in a migrated file whose comments could
// not be kept.
const migratedHeader = "# Migrated from pagewire/v1 by pagewire; comments could not be kept.\n"

// IsV1 reports whether b is a YAML mapping whose top-level apiVersion is
// pagewire/v1. Unparsable input is not v1.
func IsV1(b []byte) bool {
	file, err := parser.ParseBytes(b, 0)
	if err != nil || len(file.Docs) != 1 || file.Docs[0].Body == nil {
		return false
	}
	v := child(unwrap(file.Docs[0].Body), "apiVersion")
	if v == nil {
		return false
	}
	s, ok := unwrap(v).(*ast.StringNode)
	return ok && s.Value == APIVersionV1
}

// Migrate rewrites a pagewire/v1 file as pagewire/v2 and reports true. Other
// input is returned unchanged with false.
//
// The v1 device becomes printer "printer" and, unless device.scanner is none,
// scanner "scanner", each with the v1 defaults. The rewrite is a merge patch,
// so comments outside the removed keys stay. When the file uses YAML the patch
// cannot edit (flow mappings, anchors, ...), the whole file is re-encoded under
// SchemaHeader and a note, and its comments are lost. A v1 file that does not
// parse gives its v1 errors with positions.
func Migrate(b []byte) ([]byte, bool, error) {
	if !IsV1(b) {
		return b, false, nil
	}
	old, file, err := parseV1(b)
	if err != nil {
		return nil, false, err
	}
	c := fromV1(old)
	patch, err := v1Patch(c, file.Docs[0].Body)
	if err != nil {
		return nil, false, err
	}
	out, err := yamledit.Apply(b, patch)
	if errors.Is(err, yamledit.ErrUnsupported) {
		var enc []byte
		enc, err = yaml.Marshal(c)
		out = append([]byte(SchemaHeader+migratedHeader), enc...)
	}
	if err != nil {
		return nil, false, err
	}
	if _, _, err := parse(out); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// fromV1 is the v2 config a v1 config migrates to.
func fromV1(o *v1Config) *Config {
	c := Default(o.Name)
	d := o.Device
	c.Printers = []Printer{{
		ID: v1PrinterID, Name: o.Name, Location: o.Location, Enabled: true,
		Mode: d.Mode, USB: d.USB, Driver: d.Driver, DriverName: d.DriverName,
		Defaults: PrintDefaults{
			Media: o.Print.Defaults.Media, Sides: o.Print.Defaults.Sides,
			ColorMode: o.Print.Defaults.ColorMode, Quality: o.Print.Defaults.Quality,
		},
	}}
	switch {
	case d.Scanner != v1ScannerNone:
		name := []rune(o.Name + " scanner")
		c.Scanners = []Scanner{{
			ID: v1ScannerID, Name: strings.TrimRight(string(name[:min(len(name), 63)]), " "), Location: o.Location, Enabled: true,
			Mode: d.Mode, USB: d.USB, Backend: d.Scanner,
			Defaults: ScanDefaults{
				Source: o.Scan.Defaults.Source, Resolution: o.Scan.Defaults.Resolution,
				ColorMode: o.Scan.Defaults.ColorMode, Format: o.Scan.Defaults.Format,
			},
		}}
	case d.USB != nil:
		c.IgnoredDevices = []IgnoredDevice{{VendorID: d.USB.VendorID, ProductID: d.USB.ProductID, Serial: d.USB.Serial, Roles: []string{RoleScanner}}}
	}
	c.Print.MaxUploadMiB = o.Print.MaxUploadMiB
	c.Scan.RetentionDays = o.Scan.RetentionDays
	c.Licenses = o.Licenses
	return &c
}

// v1Patch is the ordered merge patch that turns the v1 file with AST body
// into c.
func v1Patch(c *Config, body ast.Node) ([]byte, error) {
	type kv struct {
		key string
		val any
	}
	m := []kv{{"apiVersion", APIVersion}, {"location", nil}, {"device", nil}}
	// print.defaults and scan.defaults go; a section without them is left alone.
	for _, section := range []string{"print", "scan"} {
		if sec := child(unwrap(body), section); sec != nil && child(unwrap(sec), "defaults") != nil {
			m = append(m, kv{section, json.RawMessage(`{"defaults":null}`)})
		}
	}
	m = append(m, kv{"printers", c.Printers}, kv{"scanners", c.Scanners}, kv{"ignoredDevices", c.IgnoredDevices})
	parts := make([]string, len(m))
	for i, e := range m {
		v, err := json.Marshal(e.val)
		if err != nil {
			return nil, err
		}
		parts[i] = `"` + e.key + `":` + string(v)
	}
	return []byte("{" + strings.Join(parts, ",") + "}"), nil
}
