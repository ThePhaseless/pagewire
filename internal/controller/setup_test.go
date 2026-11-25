package controller

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
)

func TestSetupOffice(t *testing.T) {
	h := newHarness(t, Options{BackendName: BackendK8s, USBAvailable: true})
	h.report(t, "office")
	s := h.c.Setup()
	if !s.Required || s.Backend != "k8s" || !s.USBResourceConfigured || len(s.Devices) != 4 {
		t.Fatalf("setup %+v", s)
	}
	byVendor := map[string]apitypes.DetectedDevice{}
	for _, d := range s.Devices {
		byVendor[d.Device.VendorID] = d
	}
	kb, hp, sam, lide := byVendor["046d"], byVendor["03f0"], byVendor["04e8"], byVendor["04a9"]
	if kb.CanPrint || kb.CanScan || kb.SuggestedPrinter != nil || kb.SuggestedScanner != nil || kb.PrinterDecision != nil {
		t.Fatalf("keyboard %+v", kb)
	}
	if !lide.CanScan || lide.CanPrint || lide.SuggestedScanner == nil || lide.ScannerDecision == nil || lide.ScannerDecision.Rule != device.RuleSANEIndex {
		t.Fatalf("LiDE %+v", lide)
	}
	if hp.SuggestedPrinter == nil || hp.SuggestedScanner == nil || hp.PrinterDecision.Rule != device.RuleIPPOverUSB {
		t.Fatalf("HP %+v", hp)
	}
	sp := sam.SuggestedPrinter
	if sp == nil || sp.ID != "scx-4300-series" || sp.Name != "SCX-4300 Series" || sp.Mode != "usb" || !sp.Enabled ||
		sp.USB == nil || sp.USB.VendorID != "04e8" || sp.USB.Serial != "FAKE0008" || sp.Driver != "auto" {
		t.Fatalf("Samsung printer suggestion %+v", sp)
	}
	if ss := sam.SuggestedScanner; ss.ID != "scx-4300-series-scanner" || ss.Name != "SCX-4300 Series scanner" || ss.Backend != "auto" {
		t.Fatalf("Samsung scanner suggestion %+v", ss)
	}
	if hp.SuggestedScanner.ID != "officejet-pro-8020-serie-scanner" {
		t.Fatalf("HP scanner id %q", hp.SuggestedScanner.ID)
	}
	if s.Suggested.Name != "Printer" || len(s.Suggested.Printers) != 0 || len(s.Suggested.Scanners) != 0 {
		t.Fatalf("suggested %+v", s.Suggested)
	}

	// The suggestions make a valid config, and empty defaults marshal as {}.
	cfg := config.Default("Home")
	for _, d := range s.Devices {
		if d.SuggestedPrinter != nil {
			cfg.Printers = append(cfg.Printers, *d.SuggestedPrinter)
		}
		if d.SuggestedScanner != nil {
			cfg.Scanners = append(cfg.Scanners, *d.SuggestedScanner)
		}
	}
	raw, _ := json.Marshal(cfg)
	if !strings.Contains(string(raw), `"defaults":{}`) {
		t.Fatalf("defaults not empty: %s", raw)
	}
	if _, err := config.Check(raw, catalog.Known()); err != nil {
		t.Fatalf("suggested config invalid: %v\n%s", err, raw)
	}
	validate(t, "Setup", s)
}

func TestSetupUniqueIDs(t *testing.T) {
	two := func(r *device.AgentReport) {
		d := r.Inventory.Devices[0]
		d.Serial, d.Address = "OTHER", 7
		r.Inventory.Devices = append(r.Inventory.Devices, d)
	}
	h := newHarness(t, Options{USBAvailable: true})
	cfg := cfgOf(nil)
	cfg.Scanners = append(cfg.Scanners, config.Scanner{ID: "scx-4300-series-scanner", Name: "x", Enabled: true, Mode: config.ModeSimulated, Backend: config.BackendAuto})
	h.c.ConfigApplied(cfg, "e")
	h.report(t, "samsung-scx4300-mfp", two)
	s := h.c.Setup()
	var ids, names []string
	for _, d := range s.Devices {
		ids = append(ids, d.SuggestedPrinter.ID, d.SuggestedScanner.ID)
		names = append(names, d.SuggestedPrinter.Name)
	}
	want := []string{"scx-4300-series", "scx-4300-series-scanner-2", "scx-4300-series-2", "scx-4300-series-scanner-3"}
	if !slices.Equal(ids, want) || !slices.Equal(names, []string{"SCX-4300 Series", "SCX-4300 Series 2"}) {
		t.Fatalf("ids %v names %v", ids, names)
	}
}

func TestSuggestedIDLength(t *testing.T) {
	s := newSuggester(nil)
	d := device.USBDevice{VendorID: "03f0", ProductID: "2754", Product: "OfficeJet Pro 8020 series"}
	p, sc := s.printer(d, false), s.scanner(d, false)
	if p.ID != "officejet-pro-8020-series" || sc.ID != "officejet-pro-8020-serie-scanner" {
		t.Fatalf("ids %q %q", p.ID, sc.ID)
	}
	for _, id := range []string{p.ID, sc.ID, s.id(sc.ID), s.id(strings.Repeat("a", 32))} {
		if len(id) > maxIDLen || strings.HasSuffix(id, "-") {
			t.Errorf("id %q", id)
		}
	}
}

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"OfficeJet Pro 8710":              "OfficeJet Pro 8710",
		"--printer":                       "printer",
		" - - x":                          "x",
		"---":                             defaultName,
		"\x01-\x02":                       defaultName,
		"":                                defaultName,
		"A\x01" + strings.Repeat("é", 70): "A" + strings.Repeat("é", 62),
	} {
		if got := sanitizeName(in, defaultName); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"SCX-4300 Series":         "scx-4300-series",
		"  HP  LaserJet (USB) ":   "hp-laserjet-usb",
		"--":                      "printer",
		strings.Repeat("ab ", 20): strings.TrimRight(strings.Repeat("ab-", 11)[:32], "-"),
	} {
		if got := sanitizeID(in, "printer"); got != want {
			t.Errorf("sanitizeID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSetupEULAs(t *testing.T) {
	t.Run("before any report", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true})
		s := h.c.Setup()
		if !s.Required || len(s.Devices) != 0 || s.Devices == nil || len(s.EULAs) != 0 || s.EULAs == nil {
			t.Fatalf("setup %+v", s)
		}
	})
	t.Run("no usb", func(t *testing.T) {
		h := newHarness(t, Options{})
		if s := h.c.Setup(); s.USBResourceConfigured || s.Backend != "sim" {
			t.Fatalf("setup %+v", s)
		}
	})
	t.Run("required by the suggested printer", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true})
		h.report(t, "hp-legacy-mfp")
		s := h.c.Setup()
		if len(s.EULAs) != 1 || s.EULAs[0].ID != "hplip-plugin" || !s.EULAs[0].Required || s.EULAs[0].State != apitypes.EULAPending {
			t.Fatalf("eulas %+v", s.EULAs)
		}
		h.report(t, "hp-legacy-mfp", withText(device.EULAAvailable))
		if e := h.c.Setup().EULAs[0]; e.State != apitypes.EULAAvailable || e.TextSHA256 == nil || *e.TextSHA256 != textSHA {
			t.Fatalf("after the text: %+v", e)
		}
	})
	t.Run("accepted EULA listed", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true})
		cfg := mfpConfig(t, "ippusb-mfp")
		cfg.Licenses.AcceptedEULAs = []config.EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: textSHA}}
		h.c.ConfigApplied(cfg, "e")
		h.report(t, "ippusb-mfp")
		s := h.c.Setup()
		if s.Required || len(s.EULAs) != 1 || s.EULAs[0].Required || s.Suggested.Name != "Home" {
			t.Fatalf("setup %+v", s)
		}
		if d := s.Devices[0]; d.PrinterID != "printer" || d.ScannerID != "scanner" {
			t.Fatalf("device %+v, want the configured entries", d)
		}
	})
	t.Run("invalid with simulated last-good", func(t *testing.T) {
		h := newHarness(t, Options{USBAvailable: true})
		h.c.ConfigInvalid(errors.New("bad"), "e", simConfig())
		if s := h.c.Setup(); s.Required {
			t.Fatalf("setup %+v", s)
		}
	})
}
