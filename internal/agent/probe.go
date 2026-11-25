// Package agent is the device-pod side of pagewire: the USB probe, the blob
// fetcher and the report loop that talks to the control plane.
package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/renameio/v2"

	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/usb"
)

const classPrinter = 7

// Probe lists the USB devices under sysRoot and reads the IEEE-1284 device ID
// of every printer-class device through its node under devRoot. With sel set,
// only devices matching it get a device ID read; the inventory still lists
// every device. A failed read leaves DeviceID empty and is logged, never fatal.
func Probe(sysRoot, devRoot string, sel *config.USB) device.Inventory {
	inv := device.Inventory{Devices: []device.USBDevice{}, ProbedAt: time.Now()}
	devs, err := usb.Enumerate(sysRoot)
	if err != nil {
		slog.Warn("probe: cannot list USB devices", "sysRoot", sysRoot, "err", err)
		return inv
	}
	for i := range devs {
		d := &devs[i]
		if sel != nil && !matches(*d, sel) {
			continue
		}
		node := filepath.Join(devRoot, "bus", "usb", fmt.Sprintf("%03d", d.Bus), fmt.Sprintf("%03d", d.Address))
		for n, it := range d.Interfaces { // an interface's index is its number (usb.Enumerate)
			if it.Class != classPrinter {
				continue
			}
			id, err := usb.ReadDeviceID(node, n)
			if err != nil {
				slog.Warn("probe: cannot read the device ID", "device", d.VendorID+":"+d.ProductID, "node", node, "interface", n, "err", err)
				continue
			}
			d.DeviceID = id
			break
		}
	}
	inv.Devices = devs
	return inv
}

// matches applies a USB selector (vendor, product and, when set, serial) the
// way catalog.Bind does.
func matches(d device.USBDevice, sel *config.USB) bool {
	return strings.EqualFold(d.VendorID, sel.VendorID) && strings.EqualFold(d.ProductID, sel.ProductID) &&
		(sel.Serial == "" || d.Serial == sel.Serial)
}

// WriteJSON writes v as JSON to path atomically with mode 0644, so the agent
// (another uid) can read files the probe and fetch containers write.
func WriteJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return renameio.WriteFile(path, append(b, '\n'), 0o644, renameio.WithStaticPermissions(0o644))
}

// ProbeFile is what `agent probe` writes: the inventory, plus the usbfs nodes
// the probe container sees. The embedded inventory's keys stay at the top
// level, so an older file without nodes still reads (Nodes nil).
type ProbeFile struct {
	device.Inventory
	Nodes []string `json:"nodes"`
}

// ProbeNodes lists the usbfs nodes under <devRoot>/bus/usb as "BBB/DDD"
// (USBDevice.Node), sorted. On k8s they are the nodes the device plugin
// allocated to the pod. A missing directory or a read error gives nil
// (unknown); an existing empty one gives [].
func ProbeNodes(devRoot string) []string {
	base := filepath.Join(devRoot, "bus", "usb")
	buses, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, b := range buses {
		if !b.IsDir() {
			continue
		}
		devs, err := os.ReadDir(filepath.Join(base, b.Name()))
		if err != nil {
			return nil
		}
		for _, d := range devs { // ReadDir sorts by name, and the names are fixed-width
			out = append(out, b.Name()+"/"+d.Name())
		}
	}
	return out
}
