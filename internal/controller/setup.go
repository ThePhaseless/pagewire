package controller

import (
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
)

const (
	maxIDLen   = 32
	maxNameLen = 63
)

type setupInput struct {
	backend      string
	usbAvailable bool
	missing      bool
	cfg          *config.Config // the valid config, else the last-known-good, else nil
	devices      []apitypes.DetectedDevice
	decisions    []device.PrinterDecision   // suggested and configured printer decisions
	texts        map[string]device.EULAText // latest agent EULA texts by id
}

// setup builds the Setup DTO: the detected devices with their suggested
// entries, the EULAs the suggested and configured printers need or offer plus
// the accepted ones with their text state, and a suggested config holding the
// name and empty lists.
func setup(cat *catalog.Catalog, in setupInput) apitypes.Setup {
	acc := []config.EULARef{}
	name := defaultName
	if in.cfg != nil {
		acc, name = in.cfg.Licenses.AcceptedEULAs, in.cfg.Name
	}
	s := apitypes.Setup{
		Required: in.missing, Backend: in.backend, USBResourceConfigured: in.usbAvailable,
		Devices: nonNil(in.devices), EULAs: []apitypes.SetupEULA{}, Suggested: config.Default(name),
	}
	for _, id := range eulaIDs(cat, in.decisions, acc) {
		e, _ := cat.EULA(id)
		d := eulaDTO(e, in.texts)
		s.EULAs = append(s.EULAs, apitypes.SetupEULA{
			ID: e.ID, Version: e.Version, Title: e.Title, Vendor: e.Vendor, Summary: e.Summary,
			Required: slices.ContainsFunc(in.decisions, func(d device.PrinterDecision) bool { return slices.Contains(d.RequiredEULAs, id) }),
			State:    d.State, TextSHA256: d.TextSHA256,
		})
	}
	return s
}

// suggester makes suggested entries whose ids are unique across both lists
// and the config, and whose printer names are unique (ignoring case).
type suggester struct {
	ids   map[string]bool
	names map[string]bool // lower-case printer names
}

func newSuggester(cfg *config.Config) *suggester {
	s := &suggester{ids: map[string]bool{}, names: map[string]bool{}}
	if cfg != nil {
		for _, p := range cfg.Printers {
			s.ids[p.ID], s.names[strings.ToLower(p.Name)] = true, true
		}
		for _, sc := range cfg.Scanners {
			s.ids[sc.ID] = true
		}
	}
	return s
}

// printer suggests a printer entry for d: usb with d's selector (simulated
// when the inventory is simulated and there is no USB), driver auto, enabled,
// empty defaults.
func (s *suggester) printer(d device.USBDevice, simulated bool) config.Printer {
	name := sanitizeName(d.Product, defaultName)
	for n := 2; s.names[strings.ToLower(name)]; n++ {
		name = withSuffix(sanitizeName(d.Product, defaultName), " "+strconv.Itoa(n), maxNameLen)
	}
	s.names[strings.ToLower(name)] = true
	p := config.Printer{
		ID: s.id(sanitizeID(d.Product, config.RolePrinter)), Name: name, Enabled: true,
		Mode: config.ModeUSB, USB: selector(d), Driver: config.Auto,
	}
	if simulated {
		p.Mode, p.USB = config.ModeSimulated, nil
	}
	return p
}

// scanner suggests a scanner entry for d, named "<product> scanner".
func (s *suggester) scanner(d device.USBDevice, simulated bool) config.Scanner {
	base := sanitizeID(d.Product, "")
	id := config.RoleScanner
	if base != "" {
		id = withSuffix(base, "-"+config.RoleScanner, maxIDLen)
	}
	sc := config.Scanner{
		ID: s.id(id), Name: withSuffix(sanitizeName(d.Product, "USB"), " scanner", maxNameLen), Enabled: true,
		Mode: config.ModeUSB, USB: selector(d), Backend: config.BackendAuto,
	}
	if simulated {
		sc.Mode, sc.USB = config.ModeSimulated, nil
	}
	return sc
}

// id makes want unique with -2, -3 and so on, and takes it.
func (s *suggester) id(want string) string {
	id := want
	for n := 2; s.ids[id]; n++ {
		id = withSuffix(want, "-"+strconv.Itoa(n), maxIDLen)
	}
	s.ids[id] = true
	return id
}

func selector(d device.USBDevice) *config.USB {
	return &config.USB{VendorID: strings.ToLower(d.VendorID), ProductID: strings.ToLower(d.ProductID), Serial: d.Serial}
}

// withSuffix appends suffix to s, cutting s so the result has at most max
// runes, without a trailing dash or space before the suffix.
func withSuffix(s, suffix string, limit int) string {
	r := []rune(s)
	if n := limit - len([]rune(suffix)); len(r) > n {
		r = r[:max(n, 0)]
	}
	return strings.TrimRight(string(r), "- ") + suffix
}

// sanitizeID turns a USB product string into an entry id: lower-case letters
// and digits, other runs as one dash, at most 32 characters, no dash at
// either end; fallback when nothing is left.
func sanitizeID(s, fallback string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
			continue
		}
		dash = true
	}
	id := b.String()
	if len(id) > maxIDLen {
		id = strings.TrimRight(id[:maxIDLen], "-")
	}
	if id == "" {
		return fallback
	}
	return id
}

// sanitizeName turns a USB product string into a valid entry name; fallback
// when nothing is left.
func sanitizeName(s, fallback string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	// A leading - is refused by validation (ippeveprinter would read it as an option).
	s = strings.TrimLeft(strings.TrimSpace(s), "- ")
	if r := []rune(s); len(r) > maxNameLen {
		s = strings.TrimSpace(string(r[:maxNameLen]))
	}
	if s == "" {
		return fallback
	}
	return s
}
