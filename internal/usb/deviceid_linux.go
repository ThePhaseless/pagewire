package usb

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ctrlTransfer is struct usbdevfs_ctrltransfer from <linux/usbdevice_fs.h>;
// Go lays it out the same way on 32- and 64-bit platforms.
type ctrlTransfer struct {
	RequestType uint8
	Request     uint8
	Value       uint16
	Index       uint16
	Length      uint16
	Timeout     uint32 // milliseconds
	Data        unsafe.Pointer
}

// USBDEVFS_CONTROL; x/sys/unix does not define it.
var usbdevfsControl = ioc(true, true, 0, unsafe.Sizeof(ctrlTransfer{}))

// ioc is the kernel's _IOC('U', nr, size) with the direction bits of the
// running architecture.
func ioc(read, write bool, nr, size uintptr) uint {
	dirShift, rd, wr := uintptr(30), uintptr(2), uintptr(1)
	switch runtime.GOARCH {
	case "mips", "mipsle", "mips64", "mips64le", "ppc64", "ppc64le":
		dirShift, rd, wr = 29, 2, 4
	}
	var dir uintptr
	if read {
		dir |= rd
	}
	if write {
		dir |= wr
	}
	return uint(dir<<dirShift | size<<16 | 'U'<<8 | nr)
}

const (
	deviceIDBufSize = 1024 // what the CUPS usb backend and the usblp driver use
	deviceIDMinLen  = 14   // 2 length bytes plus the shortest valid "MFG:x;MDL:y;"
)

// ReadDeviceID sends the USB printer class GET_DEVICE_ID request to interface
// iface of the usbfs device node devPath (/dev/bus/usb/BBB/DDD) and returns the
// raw IEEE-1284 device ID. usbfs allows this one request without claiming the
// interface (devio.c check_ctrlrecip), so it works while usblp or a daemon such
// as ipp-usb holds the interface. The transfer times out after 1 s.
func ReadDeviceID(devPath string, iface int) (string, error) {
	if iface < 0 || iface > 0xff {
		return "", fmt.Errorf("usb: interface %d out of range", iface)
	}
	fd, err := unix.Open(devPath, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("usb: open %s: %w", devPath, err)
	}
	defer func() { _ = unix.Close(fd) }()

	buf := make([]byte, deviceIDBufSize)
	ct := ctrlTransfer{
		RequestType: 0xa1,               // device-to-host, class, interface
		Request:     0,                  // GET_DEVICE_ID
		Value:       0,                  // configuration index
		Index:       uint16(iface) << 8, // interface in the high byte, alternate setting 0
		Length:      deviceIDBufSize,
		Timeout:     1000,
		Data:        unsafe.Pointer(&buf[0]), //nolint:gosec // usbfs reads into buf; kept alive below
	}
	n, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(usbdevfsControl), uintptr(unsafe.Pointer(&ct))) //nolint:gosec // ioctl argument
	runtime.KeepAlive(buf)
	if errno != 0 {
		return "", fmt.Errorf("usb: GET_DEVICE_ID on %s interface %d: %w", devPath, iface, errno)
	}
	return decodeDeviceID(buf[:n])
}

// decodeDeviceID strips the 2-byte length prefix from a GET_DEVICE_ID reply.
// The length is big-endian and includes itself. Like the CUPS usb backend, it
// rereads an out-of-range length as little-endian (some devices get the byte
// order wrong) and clamps it to the bytes received.
func decodeDeviceID(b []byte) (string, error) {
	if len(b) < 2 {
		return "", errors.New("usb: device ID reply too short")
	}
	n := int(b[0])<<8 | int(b[1])
	if n > len(b) || n < deviceIDMinLen {
		n = int(b[1])<<8 | int(b[0])
	}
	n = min(n, len(b))
	if n < deviceIDMinLen {
		return "", fmt.Errorf("usb: device ID length %d too short", n)
	}
	return strings.TrimRight(string(b[2:n]), "\x00 \t\r\n"), nil
}
