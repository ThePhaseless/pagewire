package usb

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ThePhaseless/pagewire/internal/device"
)

// sysfsTree builds a fake /sys in t.TempDir() laid out like the real one: device
// directories under devices/, symlinked from bus/usb/devices/ together with their
// interfaces.
type sysfsTree struct {
	t    *testing.T
	root string
}

func newTree(t *testing.T) *sysfsTree {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bus", "usb", "devices"), 0o750); err != nil {
		t.Fatal(err)
	}
	return &sysfsTree{t: t, root: root}
}

func (s *sysfsTree) write(dir string, attrs map[string]string) {
	s.t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		s.t.Fatal(err)
	}
	for k, v := range attrs {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v+"\n"), 0o600); err != nil {
			s.t.Fatal(err)
		}
	}
}

func (s *sysfsTree) link(name, target string) {
	s.t.Helper()
	rel, err := filepath.Rel(filepath.Join(s.root, "bus", "usb", "devices"), target)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := os.Symlink(rel, filepath.Join(s.root, "bus", "usb", "devices", name)); err != nil {
		s.t.Fatal(err)
	}
}

// dev adds a device; parent is "" for a root hub or the parent device's path.
func (s *sysfsTree) dev(parent, name string, attrs map[string]string) string {
	s.t.Helper()
	if parent == "" {
		parent = filepath.Join(s.root, "devices", "pci0000:00", "0000:00:14.0")
	}
	dir := filepath.Join(parent, name)
	s.write(dir, attrs)
	s.link(name, dir)
	return dir
}

// iface adds an interface directory named ifName under the device dir.
func (s *sysfsTree) iface(devDir, ifName, num, class, sub, proto string) {
	s.t.Helper()
	dir := filepath.Join(devDir, ifName)
	s.write(dir, map[string]string{
		"bInterfaceNumber":   num,
		"bInterfaceClass":    class,
		"bInterfaceSubClass": sub,
		"bInterfaceProtocol": proto,
	})
	s.link(ifName, dir)
}

func devAttrs(vid, pid, bus, dev string, extra map[string]string) map[string]string {
	m := map[string]string{"idVendor": vid, "idProduct": pid, "busnum": bus, "devnum": dev}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestEnumerate(t *testing.T) {
	s := newTree(t)
	hub := s.dev("", "usb1", devAttrs("1d6b", "0002", "1", "1", map[string]string{
		"manufacturer": "Linux 6.8.0 xhci-hcd", "product": "xHCI Host Controller", "serial": "0000:00:14.0",
	}))
	s.iface(hub, "1-0:1.0", "00", "09", "00", "00")
	// Keyboard on port 2, enumerated before the printer; no serial descriptor.
	kbd := s.dev(hub, "1-2", devAttrs("046d", "c31c", "1", "3", map[string]string{
		"manufacturer": "Logitech", "product": "USB Keyboard",
	}))
	s.iface(kbd, "1-2:1.0", "00", "03", "01", "01")
	s.iface(kbd, "1-2:1.1", "01", "03", "00", "00")
	prn := s.dev(hub, "1-1", devAttrs("03F0", "2b17", "1", "7", map[string]string{
		"manufacturer": "HP", "product": "LaserJet 1020", "serial": "  ABC123 ",
	}))
	s.iface(prn, "1-1:1.0", "00", "07", "01", "02")
	s.iface(prn, "1-1:1.1", "01", "07", "01", "04")
	s.iface(prn, "1-1:1.2", "02", "ff", "04", "01")

	got, err := Enumerate(s.root)
	if err != nil {
		t.Fatal(err)
	}
	want := []device.USBDevice{
		{
			VendorID: "1d6b", ProductID: "0002", Serial: "0000:00:14.0",
			Manufacturer: "Linux 6.8.0 xhci-hcd", Product: "xHCI Host Controller",
			Bus: 1, Address: 1, Interfaces: []device.USBInterface{{Class: 9}},
		},
		{
			VendorID: "046d", ProductID: "c31c", Manufacturer: "Logitech", Product: "USB Keyboard",
			Bus: 1, Address: 3, Interfaces: []device.USBInterface{{Class: 3, SubClass: 1, Protocol: 1}, {Class: 3, SubClass: 0, Protocol: 0}},
		},
		{
			VendorID: "03f0", ProductID: "2b17", Serial: "  ABC123 ", Manufacturer: "HP", Product: "LaserJet 1020",
			Bus: 1, Address: 7, Interfaces: []device.USBInterface{{Class: 7, SubClass: 1, Protocol: 2}, {Class: 7, SubClass: 1, Protocol: 4}, {Class: 0xff, SubClass: 4, Protocol: 1}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Enumerate:\n got %+v\nwant %+v", got, want)
	}
}

func TestEnumerateInterfaceOrder(t *testing.T) {
	s := newTree(t)
	d := s.dev("", "2-1", devAttrs("04b8", "1143", "2", "4", nil))
	// Lexical order of the names (1.10 < 1.2) differs from bInterfaceNumber order.
	for _, n := range []struct{ name, num, class string }{
		{"2-1:1.10", "0a", "0a"}, {"2-1:1.2", "02", "02"}, {"2-1:1.0", "00", "07"},
	} {
		s.iface(d, n.name, n.num, n.class, "00", "00")
	}
	got, err := Enumerate(s.root)
	if err != nil {
		t.Fatal(err)
	}
	want := []device.USBInterface{{Class: 7}, {Class: 2}, {Class: 10}}
	if len(got) != 1 || !reflect.DeepEqual(got[0].Interfaces, want) {
		t.Fatalf("interfaces = %+v, want %+v", got, want)
	}
}

func TestEnumerateNoBus(t *testing.T) {
	got, err := Enumerate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want empty non-nil slice", got)
	}
}

func TestEnumerateSkipsBrokenEntries(t *testing.T) {
	s := newTree(t)
	s.dev("", "1-1", map[string]string{"busnum": "1", "devnum": "2"})     // no idVendor
	s.dev("", "1-2", devAttrs("03f0", "2b17", "1", "x", nil))             // bad devnum
	s.dev("", "1-3", devAttrs("03f0", "2b17", "1", "4", nil))             // good, no interfaces
	s.write(filepath.Join(s.root, "bus", "usb", "devices", "stray"), nil) // plain dir, no attrs
	got, err := Enumerate(s.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Address != 4 {
		t.Fatalf("got %+v, want only 1-3", got)
	}
	if got[0].Interfaces == nil {
		t.Fatal("Interfaces is nil, want empty slice")
	}
}

func TestPresent(t *testing.T) {
	s := newTree(t)
	dir := s.dev("", "1-1", devAttrs("03f0", "2b17", "1", "7", map[string]string{"serial": "ABC123"}))
	devs, err := Enumerate(s.root)
	if err != nil || len(devs) != 1 {
		t.Fatalf("Enumerate = %v, %v", devs, err)
	}
	d := devs[0]
	if !Present(s.root, d) {
		t.Fatal("Present = false for an attached device")
	}

	// Re-enumeration after a replug gives a new address: the old node is stale.
	if err := os.WriteFile(filepath.Join(dir, "devnum"), []byte("8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Present(s.root, d) {
		t.Fatal("Present = true after the device got a new address")
	}

	other := d
	other.Address = 8
	other.ProductID = "2b18"
	if Present(s.root, other) {
		t.Fatal("Present = true for a different product at the same address")
	}

	if err := os.RemoveAll(filepath.Join(s.root, "bus")); err != nil {
		t.Fatal(err)
	}
	d.Address = 8
	if Present(s.root, d) {
		t.Fatal("Present = true with no USB bus")
	}
}
