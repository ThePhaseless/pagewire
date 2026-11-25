package catalog

import (
	"cmp"
	"errors"
	"slices"
	"strings"

	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
)

// ErrNoPrinter means no device in the inventory can be dispatched: none has a
// printer-class interface, or none matches the device.usb selector.
var ErrNoPrinter = errors.New("no printer found")

const (
	classPrinter = 7
	ippUSB       = "ipp-usb" // printer and scanner family
	scannerSim   = "sim"
	scannerSANE  = "airsane" // the only scanner family that loads SANE backends
)

// ippOverUSB is the USB interface triple of IPP over USB (printer class 7,
// subclass 1, protocol 4): a protocol fact, not catalog data.
var ippOverUSB = device.USBInterface{Class: classPrinter, SubClass: 1, Protocol: 4}

// Decide picks the device from inv per sel and maps it to printer and scanner
// families through the embedded catalog and indexes. Slices in the result are
// never nil.
func Decide(inv device.Inventory, sel config.Device) (device.Decision, error) {
	return current().Decide(inv, sel)
}

// choice is a printer family, its PAPPL driver name ("" none) and the rule.
type choice struct{ printer, driver, rule string }

// Decide is the package-level Decide against catalog c. The printer comes
// from IPP over USB, else the device's model in the driver index, else a
// generic driver for its command set, else none (rule none, no printer; the
// scanner still runs). device.driver and device.driverName override it.
func (c *Catalog) Decide(inv device.Inventory, sel config.Device) (device.Decision, error) {
	if sel.Mode == config.ModeSimulated {
		return device.Decision{
			Printer: "sim", Scanner: scannerSim, Rule: device.RuleSimulated,
			SaneBackends: []string{}, RequiredEULAs: []string{}, OptionalEULAs: []string{},
		}, nil
	}
	d, ok := pickDevice(inv.Devices, sel.USB)
	if !ok {
		return device.Decision{}, ErrNoPrinter
	}
	ipp := slices.Contains(d.Interfaces, ippOverUSB)
	auto := choice{printer: ippUSB, rule: device.RuleIPPOverUSB}
	if !ipp {
		auto = c.match(d, "")
	}
	eff := auto
	switch {
	case sel.Driver != "" && sel.Driver != config.Auto:
		eff = choice{printer: sel.Driver, rule: device.RuleOverride}
		if p, ok := c.Printer(sel.Driver); ok && p.App != "" {
			// A name the family does not list (empty, or dropped from the index)
			// gives way to the family's own match; the controller reports it.
			eff.driver = sel.DriverName
			if _, ok := c.Drivers.Lookup(sel.Driver, eff.driver); !ok {
				eff.driver = c.match(d, sel.Driver).driver
			}
		}
	case sel.DriverName != "":
		// A name no family lists (dropped from the index) leaves the automatic
		// choice; the controller reports it.
		if i := slices.IndexFunc(c.DriverPriority, func(f string) bool {
			_, ok := c.Drivers.Lookup(f, sel.DriverName)
			return ok
		}); i >= 0 {
			eff = choice{printer: c.DriverPriority[i], driver: sel.DriverName, rule: device.RuleOverride}
		}
	}
	dec := device.Decision{
		Printer:      eff.printer,
		Driver:       eff.driver,
		Scanner:      scannerSANE,
		SaneBackends: []string{},
		Rule:         eff.rule,
		Device:       &d,
	}
	if drv, ok := c.Drivers.Lookup(eff.printer, eff.driver); ok {
		dec.DriverDescription = drv.Description
	}
	dec.RequiredEULAs, dec.OptionalEULAs = c.eulas(auto, eff)
	if ipp {
		dec.Scanner = ippUSB
	}
	switch sel.Scanner {
	case "", config.Auto:
	case config.ScannerNone:
		dec.Scanner = ""
	default:
		dec.Scanner = sel.Scanner
	}
	if dec.Scanner == scannerSANE {
		dec.SaneBackends = append(dec.SaneBackends, c.Sane[strings.ToLower(d.VendorID+":"+d.ProductID)]...)
		if len(dec.SaneBackends) == 0 {
			dec.SaneBackends = append(dec.SaneBackends, c.DefaultSaneBackends...)
		}
	}
	return dec, nil
}

// match finds d's driver in the index, restricted to family unless it is "":
// by model, else a generic driver for its command set, else rule none.
func (c *Catalog) match(d device.USBDevice, family string) choice {
	id := device.ParseDeviceID(d.DeviceID)
	mfg, mdl := cmp.Or(id["MFG"], d.Manufacturer), cmp.Or(id["MDL"], d.Product)
	if drv, ok := c.Drivers.Match(mfg, mdl, c.DriverPriority, family); ok {
		return choice{drv.Family, drv.Name, device.RuleModel}
	}
	tokens := strings.Split(id["CMD"], ",")
	for _, cs := range c.CommandSets {
		if !slices.ContainsFunc(tokens, func(t string) bool {
			return slices.ContainsFunc(cs.Commands, func(c string) bool { return strings.EqualFold(strings.TrimSpace(t), c) })
		}) {
			continue
		}
		if drv, ok := c.Drivers.Match(cs.MFG, cs.MDL, c.DriverPriority, family); ok {
			return choice{drv.Family, drv.Name, device.RuleCommandSet}
		}
	}
	return choice{rule: device.RuleNone}
}

// eulas requires what eff's family and driver need. EULAs belong to the device,
// so every other EULA of eff and of the automatic choice is optional: an
// override still offers the HP plug-in (it serves the scanner and firmware too)
// but never requires what only the automatic driver needs.
func (c *Catalog) eulas(auto, eff choice) (required, optional []string) {
	required, optional = c.choiceEULAs(eff)
	ar, ao := c.choiceEULAs(auto)
	required = uniq(required)
	optional = append(append(optional, ar...), ao...) // not slices.Concat: it returns nil for no EULAs
	optional = slices.DeleteFunc(uniq(optional), func(e string) bool { return slices.Contains(required, e) })
	return required, optional
}

// choiceEULAs lists the EULAs of ch's family and those driverEulas gives its driver.
func (c *Catalog) choiceEULAs(ch choice) (required, optional []string) {
	required, optional = []string{}, []string{}
	if p, ok := c.Printer(ch.printer); ok {
		required = append(required, p.RequiredEULAs...)
		optional = append(optional, p.OptionalEULAs...)
	}
	for _, de := range c.DriverEULAs {
		if ch.driver != "" && de.Family == ch.printer && strings.Contains(ch.driver, de.NameContains) {
			required = append(required, de.RequiredEULAs...)
			optional = append(optional, de.OptionalEULAs...)
		}
	}
	return required, optional
}

// uniq drops repeats, keeping the first of each.
func uniq(s []string) []string {
	out := s[:0]
	for _, v := range s {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// pickDevice returns the device matching sel, or without sel the first device
// with a printer-class interface. The result is a copy with its own slices.
func pickDevice(devs []device.USBDevice, sel *config.USB) (device.USBDevice, bool) {
	for _, d := range devs {
		ok := hasPrinterClass(d)
		if sel != nil {
			ok = strings.EqualFold(d.VendorID, sel.VendorID) && strings.EqualFold(d.ProductID, sel.ProductID) &&
				(sel.Serial == "" || d.Serial == sel.Serial)
		}
		if ok {
			d.Interfaces = slices.Clone(d.Interfaces)
			return d, true
		}
	}
	return device.USBDevice{}, false
}

func hasPrinterClass(d device.USBDevice) bool {
	return slices.ContainsFunc(d.Interfaces, func(i device.USBInterface) bool { return i.Class == classPrinter })
}
