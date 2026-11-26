//go:build !linux

package usb

import (
	"context"
	"errors"
)

// Watch needs Linux netlink uevents; elsewhere it returns an error at once.
func Watch(ctx context.Context, changed func()) error {
	return errors.New("uevents need Linux")
}
