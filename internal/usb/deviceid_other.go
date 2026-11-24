//go:build !linux

package usb

import "errors"

// ReadDeviceID needs Linux usbfs; elsewhere it returns errors.ErrUnsupported.
func ReadDeviceID(devPath string, iface int) (string, error) {
	return "", errors.ErrUnsupported
}
