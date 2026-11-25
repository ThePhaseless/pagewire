package catalog

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ThePhaseless/printscan/internal/config"
	"github.com/ThePhaseless/printscan/internal/device"
)

// ErrNoPrinter means no device in the inventory can be dispatched: none has a
// printer-class interface, or none matches the device.usb selector.
var ErrNoPrinter = errors.New("no printer found")

const (
	classPrinter = 7
	scannerSim   = "sim"
	scannerSANE  = "airsane" // the only scanner family that loads SANE backends
)

// Decide picks the device from inv per sel and maps it to printer and scanner
// families through the embedded catalog's rules. Slices in the result are
// never nil.
func Decide(inv device.Inventory, sel config.Device) (device.Decision, error) {
	return current().Decide(inv, sel)
}

// Decide is the package-level Decide against catalog c.
func (c *Catalog) Decide(inv device.Inventory, sel config.Device) (device.Decision, error) {
	if sel.Mode == config.ModeSimulated {
		return device.Decision{
			Printer: "sim", Scanner: scannerSim, Rule: "simulated",
			SaneBackends: []string{}, RequiredEULAs: []string{}, OptionalEULAs: []string{},
		}, nil
	}
	d, ok := pickDevice(inv.Devices, sel.USB)
	if !ok {
		return device.Decision{}, ErrNoPrinter
	}
	i := slices.IndexFunc(c.Rules, func(r Rule) bool { return r.Match.matches(d) })
	if i < 0 {
		return device.Decision{}, fmt.Errorf("catalog: no rule matches %s:%s", d.VendorID, d.ProductID)
	}
	r := c.Rules[i]
	dec := device.Decision{
		Printer:       r.Printer,
		Scanner:       r.Scanner,
		SaneBackends:  []string{},
		RequiredEULAs: append([]string{}, r.RequiredEULAs...),
		OptionalEULAs: append([]string{}, r.OptionalEULAs...),
		Rule:          r.Name,
		Device:        &d,
	}
	// The rule's EULAs stay with a driver override: they belong to the matched
	// device (the HP plug-in serves its scanner and firmware too), not the driver.
	if sel.Driver != "" && sel.Driver != config.Auto {
		dec.Printer = sel.Driver
	}
	switch sel.Scanner {
	case "", config.Auto:
	case config.ScannerNone:
		dec.Scanner = ""
	default:
		dec.Scanner = sel.Scanner
	}
	if dec.Scanner == scannerSANE {
		dec.SaneBackends = append(dec.SaneBackends, r.SaneBackends...)
		if len(dec.SaneBackends) == 0 {
			dec.SaneBackends = append(dec.SaneBackends, c.DefaultSaneBackends...)
		}
	}
	return dec, nil
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

func (m Match) matches(d device.USBDevice) bool {
	if m.VendorID != "" && !strings.EqualFold(m.VendorID, d.VendorID) {
		return false
	}
	if m.Interface != nil {
		want := device.USBInterface{Class: m.Interface.Class, SubClass: m.Interface.SubClass, Protocol: m.Interface.Protocol}
		if !hasPrinterClass(d) || !slices.Contains(d.Interfaces, want) {
			return false
		}
	}
	if len(m.Command) > 0 {
		tokens := strings.Split(device.ParseDeviceID(d.DeviceID)["CMD"], ",")
		if !slices.ContainsFunc(tokens, func(t string) bool {
			return slices.ContainsFunc(m.Command, func(c string) bool { return strings.EqualFold(strings.TrimSpace(t), c) })
		}) {
			return false
		}
	}
	return true
}
