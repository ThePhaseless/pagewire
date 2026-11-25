package catalog

import (
	"cmp"
	"slices"
	"strings"

	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
)

const (
	classStillImage = 6
	classPrinter    = 7
	ippUSB          = "ipp-usb" // printer and scanner family
	familySim       = "sim"     // simulated printer and scanner family
	scannerSANE     = "airsane" // the only scanner family that loads SANE backends
)

// ippOverUSB is the USB interface triple of IPP over USB (printer class 7,
// subclass 1, protocol 4): a protocol fact, not catalog data.
var ippOverUSB = device.USBInterface{Class: classPrinter, SubClass: 1, Protocol: 4}

// Roles says whether d can be a printer and whether it can be a scanner. A
// printer needs a class-7 interface: CUPS' usb backend and ipp-usb drive
// nothing else, so a driver-index match alone does not make one. A scanner has
// IPP over USB (eSCL through ipp-usb), an entry in the SANE USB index, or a
// class-6 (still image) interface; the last also takes in PTP cameras, and the
// user decides.
func (c *Catalog) Roles(d device.USBDevice) (printer, scanner bool) {
	printer = hasClass(d, classPrinter)
	scanner = slices.Contains(d.Interfaces, ippOverUSB) || len(c.saneBackends(d)) > 0 || hasClass(d, classStillImage)
	return printer, scanner
}

// choice is a printer family, its PAPPL driver name ("" none) and the rule.
type choice struct{ printer, driver, rule string }

// DecidePrinter maps printer entry p, bound to d, to a printer family and
// driver. The family comes from IPP over USB, else d's model in the driver
// index, else a generic driver for its command set, else none (rule none, no
// printer runs). p's driver and driverName override it. A simulated entry gets
// family sim; a usb entry with no device (d nil) gets rule none. Slices in the
// result are never nil, and Device is a copy of d.
func (c *Catalog) DecidePrinter(d *device.USBDevice, p config.Printer) device.PrinterDecision {
	switch {
	case p.Simulated():
		return device.PrinterDecision{Family: familySim, Rule: device.RuleSimulated, RequiredEULAs: []string{}, OptionalEULAs: []string{}}
	case d == nil:
		return device.PrinterDecision{Rule: device.RuleNone, RequiredEULAs: []string{}, OptionalEULAs: []string{}}
	}
	dev := clone(*d)
	auto := choice{printer: ippUSB, rule: device.RuleIPPOverUSB}
	if !slices.Contains(dev.Interfaces, ippOverUSB) {
		auto = c.match(dev, "")
	}
	eff := auto
	switch {
	case p.Driver != "" && p.Driver != config.Auto:
		eff = choice{printer: p.Driver, rule: device.RuleOverride}
		if f, ok := c.Printer(p.Driver); ok && f.App != "" {
			// A name the family does not list (empty, or dropped from the index)
			// gives way to the family's own match; the controller reports it.
			eff.driver = p.DriverName
			if _, ok := c.Drivers.Lookup(p.Driver, eff.driver); !ok {
				eff.driver = c.match(dev, p.Driver).driver
			}
		}
	case p.DriverName != "":
		// A name no family lists (dropped from the index) leaves the automatic
		// choice; the controller reports it.
		if i := slices.IndexFunc(c.DriverPriority, func(f string) bool {
			_, ok := c.Drivers.Lookup(f, p.DriverName)
			return ok
		}); i >= 0 {
			eff = choice{printer: c.DriverPriority[i], driver: p.DriverName, rule: device.RuleOverride}
		}
	}
	dec := device.PrinterDecision{Family: eff.printer, Driver: eff.driver, Rule: eff.rule, Device: &dev}
	if drv, ok := c.Drivers.Lookup(eff.printer, eff.driver); ok {
		dec.DriverDescription = drv.Description
	}
	dec.RequiredEULAs, dec.OptionalEULAs = c.eulas(auto, eff)
	return dec
}

// DecideScanner maps scanner entry s, bound to d, to a scanner family: ipp-usb
// for IPP over USB, else airsane with d's backends from the SANE index (rule
// sane-index), else airsane with the default backends (rule still-image). The
// last also covers a device bound without any scanner evidence, by a selector
// or v1's rule that the scanner is the printer's device, which v1 ran the same
// way. s's backend overrides the family; a simulated entry gets family sim. A
// usb entry with no device (d nil) gets no family and no rule: the controller
// shows no decision for it. Slices in the result are never nil, and Device is
// a copy of d.
func (c *Catalog) DecideScanner(d *device.USBDevice, s config.Scanner) device.ScannerDecision {
	switch {
	case s.Simulated():
		return device.ScannerDecision{Family: familySim, Rule: device.RuleSimulated, SaneBackends: []string{}}
	case d == nil:
		return device.ScannerDecision{SaneBackends: []string{}}
	}
	dev := clone(*d)
	dec := device.ScannerDecision{Family: scannerSANE, Rule: device.RuleStillImage, SaneBackends: []string{}, Device: &dev}
	switch {
	case slices.Contains(dev.Interfaces, ippOverUSB):
		dec.Family, dec.Rule = ippUSB, device.RuleIPPOverUSB
	case len(c.saneBackends(dev)) > 0:
		dec.Rule = device.RuleSANEIndex
	}
	if s.Backend != "" && s.Backend != config.BackendAuto {
		dec.Family, dec.Rule = s.Backend, device.RuleOverride
	}
	if dec.Family == scannerSANE {
		dec.SaneBackends = append(dec.SaneBackends, c.saneBackends(dev)...)
		if len(dec.SaneBackends) == 0 {
			dec.SaneBackends = append(dec.SaneBackends, c.DefaultSaneBackends...)
		}
	}
	return dec
}

// saneBackends lists d's backends in the SANE USB index, nil when it has none.
func (c *Catalog) saneBackends(d device.USBDevice) []string {
	return c.Sane[strings.ToLower(d.VendorID+":"+d.ProductID)]
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

// clone copies d with its own Interfaces, so a decision never aliases the
// caller's device.
func clone(d device.USBDevice) device.USBDevice {
	d.Interfaces = slices.Clone(d.Interfaces)
	return d
}

func hasClass(d device.USBDevice, class uint8) bool {
	return slices.ContainsFunc(d.Interfaces, func(i device.USBInterface) bool { return i.Class == class })
}
