package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ThePhaseless/printscan/internal/config"
	"github.com/ThePhaseless/printscan/internal/device"
)

// hplipFingerprint is the GPG_Verification default key in hplip 3.24.4's
// base/validation.py.
const hplipFingerprint = "4aba2f66dbd5a95894910e0673d770cda59047b9"

func TestIntegrity(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.APIVersion != "printscan.catalog/v1" {
		t.Errorf("apiVersion %q", c.APIVersion)
	}
	ids := func(name string, list []string) {
		seen := map[string]bool{}
		for _, id := range list {
			if id == "" || seen[id] {
				t.Errorf("%s: empty or duplicate id %q", name, id)
			}
			seen[id] = true
		}
	}
	var printers, scanners, eulas, blobs, rules []string
	for _, p := range c.Printers {
		printers = append(printers, p.ID)
		if p.App != "" && len(p.DeviceSchemes) == 0 {
			t.Errorf("PAPPL printer %s has no deviceSchemes", p.ID)
		}
		if p.App != "" && p.Image == "" {
			t.Errorf("PAPPL printer %s has no image", p.ID)
		}
	}
	for _, s := range c.Scanners {
		scanners = append(scanners, s.ID)
	}
	for _, e := range c.EULAs {
		eulas = append(eulas, e.ID)
		if e.Version == "" || e.Vendor == "" || e.Title == "" || e.URL == "" || e.Summary == "" {
			t.Errorf("EULA %s has empty metadata: %+v", e.ID, e)
		}
	}
	for _, b := range c.Blobs {
		blobs = append(blobs, b.ID)
		if !slices.Contains(eulas, b.EULA) {
			t.Errorf("blob %s: unknown EULA %q", b.ID, b.EULA)
		}
		if !b.Enabled && b.DisabledMessage == "" {
			t.Errorf("disabled blob %s has no disabledMessage", b.ID)
		}
		if b.Enabled && b.Verify.PGPKey == "" && b.Verify.SHA256 == "" {
			t.Errorf("enabled blob %s has no verification", b.ID)
		}
		if b.Enabled && b.Verify.PGPKey != "" {
			keys, err := c.BlobKey(b.ID)
			if err != nil || len(keys) == 0 {
				t.Errorf("blob %s key: %v", b.ID, err)
			}
		}
	}
	ids("printers", printers)
	ids("scanners", scanners)
	ids("eulas", eulas)
	ids("blobs", blobs)
	for i, r := range c.Rules {
		rules = append(rules, r.Name)
		if !slices.Contains(printers, r.Printer) {
			t.Errorf("rule %q: unknown printer %q", r.Name, r.Printer)
		}
		if !slices.Contains(scanners, r.Scanner) {
			t.Errorf("rule %q: unknown scanner %q", r.Name, r.Scanner)
		}
		for _, e := range slices.Concat(r.RequiredEULAs, r.OptionalEULAs) {
			if !slices.Contains(eulas, e) {
				t.Errorf("rule %q: unknown EULA %q", r.Name, e)
			}
		}
		for _, sb := range r.SaneBackends {
			if !slices.Contains(c.DefaultSaneBackends, sb) {
				t.Errorf("rule %q: SANE backend %q is not in defaultSaneBackends (the device image installs only those)", r.Name, sb)
			}
		}
		catchAll := reflect.DeepEqual(r.Match, Match{})
		if last := i == len(c.Rules)-1; catchAll != last {
			t.Errorf("rule %q: catch-all %v, last %v", r.Name, catchAll, last)
		}
	}
	ids("rules", rules)
	for _, banned := range []string{"net", "escl", "airscan"} {
		if slices.Contains(c.DefaultSaneBackends, banned) {
			t.Errorf("defaultSaneBackends contains %s", banned)
		}
	}
}

// Ports are distinct per family. airsane and sim share one because they are
// the same daemon and never run together; ipp-usb owns 60000 to 60009.
func TestUniquePorts(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	used := map[int]string{}
	claim := func(port int, owner string) {
		if port == 0 {
			return
		}
		if port >= 60000 && port <= 60009 {
			t.Errorf("%s: port %d is in ipp-usb's range", owner, port)
		}
		if prev, ok := used[port]; ok {
			t.Errorf("%s: port %d already used by %s", owner, port, prev)
		}
		used[port] = owner
	}
	for _, p := range c.Printers {
		claim(p.Port, "printer "+p.ID)
	}
	scannerPorts := map[int]bool{}
	for _, s := range c.Scanners {
		if s.Port != 0 && !scannerPorts[s.Port] {
			scannerPorts[s.Port] = true
			claim(s.Port, "scanner "+s.ID)
		}
	}
}

// The config file's enums and the catalog must not drift apart.
func TestConfigEnumsInCatalog(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	dt := reflect.TypeFor[config.Device]()
	for field, have := range map[string]func(string) bool{
		"Driver": func(id string) bool { _, ok := c.Printer(id); return ok },
		"Scanner": func(id string) bool {
			return slices.ContainsFunc(c.Scanners, func(s Scanner) bool { return s.ID == id })
		},
	} {
		f, _ := dt.FieldByName(field)
		_, list, ok := strings.Cut(f.Tag.Get("validate"), "oneof=")
		if !ok {
			t.Fatalf("config.Device.%s has no oneof", field)
		}
		for _, v := range strings.Fields(list) {
			if v != config.Auto && v != config.ScannerNone && !have(v) {
				t.Errorf("config.Device.%s value %q is not in the catalog", field, v)
			}
		}
	}
}

func TestKnown(t *testing.T) {
	k := Known()
	if !slices.Contains(k.EULAs, config.EULARef{ID: "hplip-plugin", Version: "3.24.4"}) {
		t.Errorf("Known().EULAs = %v", k.EULAs)
	}
	for _, p := range []string{"ipp-usb", "ps", "hplip", "gutenprint", "ghostscript", "sim"} {
		if !slices.Contains(k.Printers, p) {
			t.Errorf("Known().Printers lacks %s: %v", p, k.Printers)
		}
	}
}

func TestEULA(t *testing.T) {
	e, ok := EULA("hplip-plugin")
	if !ok || e.Vendor != "HP" || e.Version != "3.24.4" {
		t.Errorf("EULA(hplip-plugin) = %+v, %v", e, ok)
	}
	if _, ok := EULA("nope"); ok {
		t.Error("EULA(nope) found")
	}
}

func TestHPLIPKey(t *testing.T) {
	b, ok := Default().Blob("hplip-plugin")
	if !ok {
		t.Fatal("no hplip-plugin blob")
	}
	if !b.Enabled {
		t.Skip("hplip-plugin is disabled")
	}
	keys, err := BlobKey("hplip-plugin")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("%d keys", len(keys))
	}
	if got := fmt.Sprintf("%x", keys[0].PrimaryKey.Fingerprint); got != hplipFingerprint {
		t.Errorf("fingerprint %s, want %s", got, hplipFingerprint)
	}
}

func TestBlobKeyRefusals(t *testing.T) {
	c := &Catalog{Blobs: []Blob{
		{ID: "off", Enabled: false, DisabledMessage: "x", Verify: Verify{PGPKey: "keys/does-not-exist.asc"}},
		{ID: "nokey", Enabled: true, Verify: Verify{SHA256: "00"}},
		{ID: "missing", Enabled: true, Verify: Verify{PGPKey: "keys/does-not-exist.asc"}},
	}}
	for id, want := range map[string]string{
		"off":     "disabled",
		"nokey":   "no pgpKey",
		"unknown": "unknown blob",
		"missing": "does-not-exist",
	} {
		_, err := c.BlobKey(id)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("BlobKey(%s) = %v, want error containing %q", id, err, want)
		}
	}
	// keys/README.md is not a key: the embed pattern must still compile without hplip.asc.
	if _, err := fs.Stat(files, "keys/README.md"); err != nil {
		t.Error(err)
	}
}

var fixtureNames = []string{
	"ippusb-mfp", "hp-legacy-mfp", "generic-postscript", "brother-pcl-laser",
	"epson-inkjet-mfp", "canon-pixma-mfp", "unknown-printer", "no-printer",
}

func TestFixturesStrict(t *testing.T) {
	entries, err := fs.Glob(files, "fixtures/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(fixtureNames) {
		t.Errorf("fixtures %v, want %v", entries, fixtureNames)
	}
	for _, name := range fixtureNames {
		t.Run(name, func(t *testing.T) {
			raw, err := files.ReadFile("fixtures/" + name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			var inv device.Inventory
			if err := dec.Decode(&inv); err != nil {
				t.Fatal(err)
			}
			if _, err := dec.Token(); err != io.EOF {
				t.Errorf("trailing data: %v", err)
			}
			if inv.ProbedAt.IsZero() || len(inv.Devices) == 0 {
				t.Errorf("probedAt %v, %d devices", inv.ProbedAt, len(inv.Devices))
			}
			if !reflect.DeepEqual(Fixture(name), inv) {
				t.Error("Fixture() differs from the strict decode")
			}
			for _, d := range inv.Devices {
				if !hasPrinterClass(d) {
					continue
				}
				id := device.ParseDeviceID(d.DeviceID)
				if d.Serial == "" || d.Product == "" || id["MFG"] == "" || id["MDL"] == "" {
					t.Errorf("printer %s:%s needs serial, product, MFG and MDL", d.VendorID, d.ProductID)
				}
			}
		})
	}
	if inv := Fixture("nope"); inv.Devices == nil || len(inv.Devices) != 0 {
		t.Errorf("Fixture(nope) = %+v", inv)
	}
}
