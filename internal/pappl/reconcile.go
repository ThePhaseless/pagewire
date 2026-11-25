package pappl

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/device"
)

// Reconcile makes the PAPPL system hold exactly one queue named want.Name on
// the wanted device with the wanted driver, and applies want's defaults. It
// is idempotent and never panics on IPP failures: every failure becomes a
// QueueResult with State "error", retried on the agent's next cycle.
func Reconcile(ctx context.Context, c *Client, want device.QueueSpec, inv device.Inventory) device.QueueResult {
	printers, err := c.Printers(ctx)
	if err != nil {
		return failed(err.Error())
	}

	dev := Device{URI: want.DeviceURI}
	if dev.URI == "" {
		devs, err := c.FindDevices(ctx)
		if err != nil {
			return failed(err.Error())
		}
		var ok bool
		if dev, ok = pickDevice(devs, want.Schemes, inv); !ok {
			r := device.QueueResult{State: device.QueuePending, Message: "printer not listed by the printer application yet"}
			if p := byName(printers, want.Name); p != nil {
				r.PrinterURI = p.URI // keep the existing queue usable through a transient miss
			}
			return r
		}
	}

	var keep *Printer
	for i := range printers {
		p := &printers[i]
		if p.Name == want.Name && p.DeviceURI == dev.URI && (want.Driver == "auto" || p.Driver == want.Driver) {
			keep = p
			continue
		}
		if err := c.DeletePrinter(ctx, p.ID); err != nil {
			return failed(err.Error())
		}
	}

	if keep == nil {
		if msg := create(ctx, c, want, dev); msg != "" {
			return failed(msg)
		}
		if printers, err = c.Printers(ctx); err != nil {
			return failed(err.Error())
		}
		if keep = byName(printers, want.Name); keep == nil {
			return failed("printer " + want.Name + " missing after Create-Printer")
		}
	}
	if keep.URI == "" {
		return failed("printer " + want.Name + " has no ipp:// printer-uri-supported")
	}

	if keep.Location != want.Location || differs(keep.Media, want.Media) || differs(keep.Sides, want.Sides) ||
		differs(keep.ColorMode, want.ColorMode) || differs(keep.Quality, want.Quality) {
		if err := c.SetDefaults(ctx, keep.URI, want); err != nil {
			return device.QueueResult{State: device.QueueError, Message: err.Error(), PrinterURI: keep.URI, DeviceURI: dev.URI}
		}
	}
	return device.QueueResult{State: device.QueueOK, PrinterURI: keep.URI, DeviceURI: dev.URI}
}

// create adds the queue, falling back from driver auto to the first
// Find-Drivers match when the application refuses auto. It returns an error
// message, or "" on success.
func create(ctx context.Context, c *Client, want device.QueueSpec, dev Device) string {
	_, err := c.CreatePrinter(ctx, want.Name, dev.URI, want.Driver, dev.DeviceID)
	var ie *IPPError
	if err == nil || want.Driver != "auto" || !errors.As(err, &ie) || ie.Status != goipp.StatusErrorAttributesOrValues {
		return errText(err)
	}
	if dev.DeviceID == "" {
		return "driver auto needs a device ID"
	}
	drivers, ferr := c.FindDrivers(ctx, dev.DeviceID)
	if ferr != nil {
		return ferr.Error()
	}
	if len(drivers) == 0 {
		return "no driver of the printer application matches " + dev.DeviceID
	}
	_, err = c.CreatePrinter(ctx, want.Name, dev.URI, drivers[0].Name, dev.DeviceID)
	return errText(err)
}

// pickDevice chooses the Find-Devices entry for the inventory's printer. The
// agent's inventory is already narrowed by the device.usb selector, so the
// printer is its first device with a printer-class interface. Matching never
// compares VID:PID, which CUPS backends do not list.
func pickDevice(devs []Device, schemes []string, inv device.Inventory) (Device, bool) {
	var cands []Device
	for _, d := range devs {
		if slices.Contains(schemes, scheme(d.URI)) {
			cands = append(cands, d)
		}
	}
	var serial, mfg, mdl string
	if u := printerDevice(inv); u != nil {
		serial = u.Serial
		id := device.ParseDeviceID(u.DeviceID)
		mfg, mdl = id["MFG"], id["MDL"]
	}

	var matches []Device
	if serial != "" {
		for _, d := range cands {
			if strings.EqualFold(serialOf(d.URI), serial) {
				matches = append(matches, d)
			}
		}
	}
	if len(matches) == 0 && mfg != "" && mdl != "" {
		for _, d := range cands {
			id := device.ParseDeviceID(d.DeviceID)
			if strings.EqualFold(id["MFG"], mfg) && strings.EqualFold(id["MDL"], mdl) {
				matches = append(matches, d)
			}
		}
	}
	if len(matches) == 0 && len(cands) == 1 {
		matches = cands
	}
	if len(matches) == 0 {
		return Device{}, false
	}
	// Stable: among equal schemes the first listed entry wins.
	slices.SortStableFunc(matches, func(a, b Device) int {
		return slices.Index(schemes, scheme(a.URI)) - slices.Index(schemes, scheme(b.URI))
	})
	return matches[0], true
}

func printerDevice(inv device.Inventory) *device.USBDevice {
	for i, d := range inv.Devices {
		for _, it := range d.Interfaces {
			if it.Class == 7 {
				return &inv.Devices[i]
			}
		}
	}
	return nil
}

// backendURI strips the optional "cups:" prefix pappl-retrofit adds.
func backendURI(uri string) string { return strings.TrimPrefix(uri, "cups:") }

// scheme is the backend URI's scheme: the text before its first ':'.
func scheme(uri string) string {
	s, _, _ := strings.Cut(backendURI(uri), ":")
	return s
}

func serialOf(uri string) string {
	_, q, ok := strings.Cut(backendURI(uri), "?")
	if !ok {
		return ""
	}
	v, err := url.ParseQuery(q)
	if err != nil {
		return ""
	}
	return v.Get("serial")
}

func byName(ps []Printer, name string) *Printer {
	for i := range ps {
		if ps[i].Name == name {
			return &ps[i]
		}
	}
	return nil
}

// differs reports whether a wanted (non-empty) default differs from the
// queue's; an empty wanted value leaves the queue's default alone.
func differs(have, want string) bool { return want != "" && have != want }

func failed(msg string) device.QueueResult {
	return device.QueueResult{State: device.QueueError, Message: msg}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
