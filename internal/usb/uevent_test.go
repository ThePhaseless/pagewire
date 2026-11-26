//go:build linux

package usb

import (
	"strings"
	"testing"
)

// uevent joins fields with NUL the way the kernel sends them.
func uevent(fields ...string) []byte {
	return []byte(strings.Join(fields, "\x00") + "\x00")
}

func TestUSBDeviceEvent(t *testing.T) {
	const dev = "/devices/pci0000:00/0000:00:14.0/usb1/1-2"
	device := func(action string) []byte {
		return uevent(action+"@"+dev, "ACTION="+action, "DEVPATH="+dev, "SUBSYSTEM=usb",
			"MAJOR=189", "MINOR=1", "DEVNAME=bus/usb/001/002", "DEVTYPE=usb_device",
			"PRODUCT=4e8/3441/100", "TYPE=0/0/0", "BUSNUM=001", "DEVNUM=002", "SEQNUM=4321")
	}
	for _, tc := range []struct {
		name string
		msg  []byte
		want bool
	}{
		{"usb_device add", device("add"), true},
		{"usb_device remove", device("remove"), true},
		{"usb_device bind", device("bind"), true},
		{"usb_device unbind", device("unbind"), true},
		{"usb_device change", device("change"), false},
		{"usb_interface add", uevent("add@"+dev+"/1-2:1.0", "ACTION=add", "DEVPATH="+dev+"/1-2:1.0",
			"SUBSYSTEM=usb", "DEVTYPE=usb_interface", "PRODUCT=4e8/3441/100", "TYPE=0/0/0",
			"INTERFACE=7/1/2", "MODALIAS=usb:v04E8p3441d0100dc00dsc00dp00ic07isc01ip02in00", "SEQNUM=4322"), false},
		{"non-usb add", uevent("add@/devices/virtual/net/veth1", "ACTION=add", "DEVPATH=/devices/virtual/net/veth1",
			"SUBSYSTEM=net", "INTERFACE=veth1", "IFINDEX=7", "SEQNUM=4400"), false},
		{"udev rebroadcast", append([]byte("libudev\x00\xfe\xed\xca\xfe"), device("add")[len("add@"+dev):]...), false},
		{"empty", nil, false},
		{"garbage", []byte("SUBSYSTEM=usb"), false},
	} {
		if got := usbDeviceEvent(tc.msg); got != tc.want {
			t.Errorf("%s: usbDeviceEvent = %v, want %v", tc.name, got, tc.want)
		}
	}
}
