//go:build linux

package usb

import (
	"runtime"
	"testing"
)

func withLen(hi, lo byte, s string) []byte { return append([]byte{hi, lo}, s...) }

func TestDecodeDeviceID(t *testing.T) {
	const id = "MFG:HP;MDL:LaserJet 1020;CMD:ACL;"
	n := byte(len(id) + 2)
	tests := []struct {
		name    string
		in      []byte
		want    string
		wantErr bool
	}{
		{"big-endian length", withLen(0, n, id), id, false},
		{"little-endian length from a buggy device", withLen(n, 0, id), id, false},
		{"bytes after the length are ignored", append(withLen(0, n, id), "\x00\x00junk"...), id, false},
		{"length past the received bytes is clamped", withLen(0x03, 0xff, id), id, false},
		{"trailing NUL and whitespace trimmed", withLen(0, n+3, id+"\x00 \n"), id, false},
		{"too short", withLen(0, 6, "MFG:"), "", true},
		{"empty", nil, "", true},
		{"one byte", []byte{0}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeDeviceID(tc.in)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("decodeDeviceID = %q, %v; want %q, err %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// The ioctl number matches <linux/usbdevice_fs.h> on the generic encoding.
func TestIoctlNumbers(t *testing.T) {
	switch runtime.GOARCH {
	case "amd64", "arm64", "riscv64", "s390x", "loong64":
	default:
		t.Skip("values below are for 64-bit generic ioctl encoding")
	}
	if usbdevfsControl != 0xc0185500 {
		t.Errorf("USBDEVFS_CONTROL = %#x, want 0xc0185500", usbdevfsControl)
	}
}

func TestReadDeviceIDMissingNode(t *testing.T) {
	if _, err := ReadDeviceID(t.TempDir()+"/nope", 0); err == nil {
		t.Fatal("want an error for a missing device node")
	}
}
