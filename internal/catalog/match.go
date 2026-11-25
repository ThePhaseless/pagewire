package catalog

import (
	"cmp"
	"slices"
	"strings"

	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
)

// Placed is the device a config entry is bound to, and the node it is on
// ("" in single-node mode, docker and sim).
type Placed struct {
	Node   string
	Device device.USBDevice
}

// Detected is one USB device for the setup wizard: its roles, the entries
// configured on it (disabled ones included) and the roles it is ignored for.
// Ignored is never nil.
type Detected struct {
	Node              string
	Device            device.USBDevice
	CanPrint, CanScan bool
	PrinterID         string
	ScannerID         string
	Ignored           []string // config.RolePrinter, config.RoleScanner, in that order
}

// Bind binds the enabled usb entries of cfg to the devices of nodes (node
// name to that node's devices), keyed by entry id. An entry left out has no
// device. Per role, in this order:
//   - an entry with a selector takes the first device matching its vendor,
//     product and serial; selectors with a serial go first, so one without
//     takes another device of the model rather than the first;
//   - an entry without one (a migrated v1 file) takes the first device with
//     the role, unclaimed by another entry of the role and not ignored for it;
//   - the unbound scanner first tries the unbound printer's device whatever
//     its roles, as v1 ran the scanner on the printer's device.
//
// "First" is bus/address order on the first node in sorted order. Disabled
// entries are not returned but still claim their devices.
func (c *Catalog) Bind(cfg *config.Config, nodes map[string][]device.USBDevice) (printers, scanners map[string]Placed) {
	all := ordered(nodes)
	pi, si := c.bind(cfg, all)
	printers, scanners = map[string]Placed{}, map[string]Placed{}
	if cfg == nil {
		return printers, scanners
	}
	for _, p := range cfg.Printers {
		if i, ok := pi[p.ID]; ok && p.Enabled {
			printers[p.ID] = placed(all[i])
		}
	}
	for _, s := range cfg.Scanners {
		if i, ok := si[s.ID]; ok && s.Enabled {
			scanners[s.ID] = placed(all[i])
		}
	}
	return printers, scanners
}

// Detect lists every device of nodes, in Bind's order, with its roles, the
// ids of the entries bound to it (Bind's rules over every entry, disabled
// ones included) and the roles cfg ignores it for. cfg may be nil.
func (c *Catalog) Detect(cfg *config.Config, nodes map[string][]device.USBDevice) []Detected {
	all := ordered(nodes)
	pi, si := c.bind(cfg, all)
	out := make([]Detected, len(all))
	for i, p := range all {
		d := Detected{Node: p.Node, Device: clone(p.Device), Ignored: []string{}}
		d.CanPrint, d.CanScan = c.Roles(p.Device)
		for id, j := range pi {
			if j == i {
				d.PrinterID = id
			}
		}
		for id, j := range si {
			if j == i {
				d.ScannerID = id
			}
		}
		if cfg != nil {
			for _, role := range []string{config.RolePrinter, config.RoleScanner} {
				if ignored(cfg.IgnoredDevices, p.Device, role) {
					d.Ignored = append(d.Ignored, role)
				}
			}
		}
		out[i] = d
	}
	return out
}

// entry is a usb config entry of one role.
type entry struct {
	id  string
	usb *config.USB
}

// bind binds every usb entry of cfg, disabled ones included, to an index into
// all (see Bind).
func (c *Catalog) bind(cfg *config.Config, all []Placed) (printers, scanners map[string]int) {
	if cfg == nil {
		return map[string]int{}, map[string]int{}
	}
	var ps, ss []entry
	for _, p := range cfg.Printers {
		if !p.Simulated() {
			ps = append(ps, entry{p.ID, p.USB})
		}
	}
	for _, s := range cfg.Scanners {
		if !s.Simulated() {
			ss = append(ss, entry{s.ID, s.USB})
		}
	}
	isPrinter := func(d device.USBDevice) bool { p, _ := c.Roles(d); return p }
	isScanner := func(d device.USBDevice) bool { _, s := c.Roles(d); return s }
	printers = bindRole(all, ps, config.RolePrinter, isPrinter, cfg.IgnoredDevices, -1)
	prefer := -1
	if i := slices.IndexFunc(ps, func(e entry) bool { return e.usb == nil }); i >= 0 {
		if j, ok := printers[ps[i].id]; ok {
			prefer = j
		}
	}
	scanners = bindRole(all, ss, config.RoleScanner, isScanner, cfg.IgnoredDevices, prefer)
	return printers, scanners
}

// bindRole binds the entries of one role. prefer, when not -1, is the device
// the first unbound entry tries before the devices with the role.
func bindRole(all []Placed, entries []entry, role string, hasRole func(device.USBDevice) bool, ign []config.IgnoredDevice, prefer int) map[string]int {
	out := map[string]int{}
	claimed := map[int]bool{}
	take := func(id string, ok func(int) bool, order []int) {
		for _, i := range order {
			if !claimed[i] && ok(i) {
				out[id], claimed[i] = i, true
				return
			}
		}
	}
	order := make([]int, len(all))
	for i := range order {
		order[i] = i
	}
	for _, withSerial := range []bool{true, false} {
		for _, e := range entries {
			if e.usb != nil && (e.usb.Serial != "") == withSerial {
				take(e.id, func(i int) bool { return selects(e.usb.VendorID, e.usb.ProductID, e.usb.Serial, all[i].Device) }, order)
			}
		}
	}
	for _, e := range entries {
		if e.usb != nil {
			continue
		}
		if prefer >= 0 {
			take(e.id, func(i int) bool { return !ignored(ign, all[i].Device, role) }, []int{prefer})
			prefer = -1
			if _, ok := out[e.id]; ok {
				continue
			}
		}
		take(e.id, func(i int) bool { return hasRole(all[i].Device) && !ignored(ign, all[i].Device, role) }, order)
	}
	return out
}

// selects reports whether a selector (serial "" matches any) picks d, as v1 did.
func selects(vendor, product, serial string, d device.USBDevice) bool {
	return strings.EqualFold(d.VendorID, vendor) && strings.EqualFold(d.ProductID, product) &&
		(serial == "" || d.Serial == serial)
}

// ignored reports whether ign ignores d for role.
func ignored(ign []config.IgnoredDevice, d device.USBDevice, role string) bool {
	return slices.ContainsFunc(ign, func(g config.IgnoredDevice) bool {
		return slices.Contains(g.Roles, role) && selects(g.VendorID, g.ProductID, g.Serial, d)
	})
}

// ordered flattens nodes into sorted node order, each node's devices in
// bus/address order. A device listed twice on a node (inventory and live
// sysfs both) is kept once, the first listed.
func ordered(nodes map[string][]device.USBDevice) []Placed {
	var out []Placed
	for _, n := range slices.Sorted(func(yield func(string) bool) {
		for n := range nodes {
			if !yield(n) {
				return
			}
		}
	}) {
		devs := slices.Clone(nodes[n])
		slices.SortStableFunc(devs, func(a, b device.USBDevice) int {
			return cmp.Or(cmp.Compare(a.Bus, b.Bus), cmp.Compare(a.Address, b.Address))
		})
		devs = slices.CompactFunc(devs, func(a, b device.USBDevice) bool { return a.Bus == b.Bus && a.Address == b.Address })
		for _, d := range devs {
			out = append(out, Placed{Node: n, Device: d})
		}
	}
	return out
}

// placed copies p with its own device slices.
func placed(p Placed) Placed { p.Device = clone(p.Device); return p }
