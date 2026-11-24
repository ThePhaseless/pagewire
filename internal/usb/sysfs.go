// Package usb lists USB devices from sysfs and reads a printer's IEEE-1284
// device ID through usbfs. It never runs commands and does not use libusb.
package usb

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ThePhaseless/printscan/internal/device"
)

// Enumerate lists the USB devices under <root>/bus/usb/devices (root is the
// sysfs mount, normally "/sys"), sorted by bus and address. DeviceID is left
// empty. Each device's Interfaces are those of its active configuration, ordered
// by bInterfaceNumber; USB numbers them from zero without gaps, so an
// interface's index is its number. No USB bus gives an empty list. Entries that
// vanish or are malformed mid-read (a device unplugged while listing) are skipped.
func Enumerate(root string) ([]device.USBDevice, error) {
	base := filepath.Join(root, "bus", "usb", "devices")
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return []device.USBDevice{}, nil
	}
	if err != nil {
		return nil, err
	}
	devs := []device.USBDevice{}
	for _, e := range entries {
		if strings.Contains(e.Name(), ":") { // interface entry, read through its device
			continue
		}
		if d, ok := readDevice(filepath.Join(base, e.Name())); ok {
			devs = append(devs, d)
		}
	}
	slices.SortFunc(devs, func(a, b device.USBDevice) int {
		return cmp.Or(cmp.Compare(a.Bus, b.Bus), cmp.Compare(a.Address, b.Address))
	})
	return devs, nil
}

// Present reports whether d is still attached at the same bus address. A device
// that was re-plugged gets a new address, so it counts as gone: its old device
// node is stale.
func Present(root string, d device.USBDevice) bool {
	devs, err := Enumerate(root)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(devs, func(x device.USBDevice) bool {
		return x.Bus == d.Bus && x.Address == d.Address && x.VendorID == d.VendorID && x.ProductID == d.ProductID
	})
}

func readDevice(dir string) (device.USBDevice, bool) {
	vid, pid := hexID(dir, "idVendor"), hexID(dir, "idProduct")
	bus, err1 := strconv.Atoi(attr(dir, "busnum"))
	addr, err2 := strconv.Atoi(attr(dir, "devnum"))
	if vid == "" || pid == "" || err1 != nil || err2 != nil {
		return device.USBDevice{}, false
	}
	return device.USBDevice{
		VendorID:     vid,
		ProductID:    pid,
		Serial:       str(dir, "serial"),
		Manufacturer: str(dir, "manufacturer"),
		Product:      str(dir, "product"),
		Bus:          bus,
		Address:      addr,
		Interfaces:   readInterfaces(dir),
	}, true
}

// readInterfaces reads the <dev>:<cfg>.<if> children of a device directory.
// The glob does not use the device's own name: a root hub "usb1" has "1-0:1.0".
func readInterfaces(dir string) []device.USBInterface {
	type numbered struct {
		num uint8
		device.USBInterface
	}
	var found []numbered
	paths, _ := filepath.Glob(filepath.Join(dir, "*:*.*")) // only fails on a bad pattern
	for _, p := range paths {
		num, ok1 := hexByte(p, "bInterfaceNumber")
		class, ok2 := hexByte(p, "bInterfaceClass")
		sub, ok3 := hexByte(p, "bInterfaceSubClass")
		proto, ok4 := hexByte(p, "bInterfaceProtocol")
		if ok1 && ok2 && ok3 && ok4 {
			found = append(found, numbered{num, device.USBInterface{Class: class, SubClass: sub, Protocol: proto}})
		}
	}
	slices.SortFunc(found, func(a, b numbered) int { return cmp.Compare(a.num, b.num) })
	out := make([]device.USBInterface, len(found))
	for i, f := range found {
		out[i] = f.USBInterface
	}
	return out
}

// str reads a string descriptor attribute; only the newline sysfs appends is
// removed, so serials keep any padding the device reports (CUPS backends list
// them raw, and queue matching compares serials).
func str(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // path built from the sysfs root
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(b), "\n")
}

func attr(dir, name string) string { return strings.TrimSpace(str(dir, name)) }

// hexID returns a 4-digit hex attribute in lowercase, or "" when malformed.
func hexID(dir, name string) string {
	s := strings.ToLower(attr(dir, name))
	if _, err := strconv.ParseUint(s, 16, 16); err != nil || len(s) != 4 {
		return ""
	}
	return s
}

func hexByte(dir, name string) (uint8, bool) {
	v, err := strconv.ParseUint(attr(dir, name), 16, 8)
	return uint8(v), err == nil
}
