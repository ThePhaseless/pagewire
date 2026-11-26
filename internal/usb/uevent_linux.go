package usb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// settle waits for the last of a burst of uevents, so a new device's interface
// attributes exist in sysfs before the caller re-reads it.
const settle = 250 * time.Millisecond

// Watch calls changed once as soon as the socket is bound (a change between
// the caller's first sysfs read and the bind raised no event), then 250 ms
// after the last USB device add, remove, bind or unbind kernel uevent, and
// after the kernel reports lost events (ENOBUFS). It
// blocks until ctx ends and then returns nil. It returns an error when the
// netlink socket cannot be opened or bound, or (never seen in practice) when
// reading it fails with anything but ENOBUFS; callers fall back to polling.
//
// Uevents reach only network namespaces owned by the initial user namespace
// (host network or a normal container). In a userns-remapped namespace the
// socket opens but receives nothing, and callers' fallbacks do not notice.
// Message content is only a trigger: callers re-read sysfs, so a spoofed
// message costs one enumerate.
func Watch(ctx context.Context, changed func()) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return fmt.Errorf("uevent socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: 1}); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("uevent bind: %w", err)
	}
	// Best effort: a full buffer only means ENOBUFS, which triggers a re-read.
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 1<<20)
	f := os.NewFile(uintptr(fd), "uevent")
	defer func() { _ = f.Close() }()
	// Closing the file unblocks Read when ctx ends.
	defer context.AfterFunc(ctx, func() { _ = f.Close() })()
	changed()

	var t *time.Timer
	defer func() {
		if t != nil {
			t.Stop()
		}
	}()
	poke := func() {
		if t == nil {
			t = time.AfterFunc(settle, changed)
		} else {
			t.Reset(settle)
		}
	}

	buf := make([]byte, 64<<10)
	for {
		n, err := f.Read(buf)
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, unix.ENOBUFS):
			poke()
		case err != nil:
			return fmt.Errorf("uevent read: %w", err)
		case usbDeviceEvent(buf[:n]):
			poke()
		}
	}
}

// usbDeviceEvent reports whether msg, a kernel uevent "ACTION@DEVPATH\0KEY=VAL\0…",
// is an add, remove, bind or unbind of a whole USB device (not an interface).
// udev's re-broadcasts ("libudev" header) and malformed messages are not.
func usbDeviceEvent(msg []byte) bool {
	fields := bytes.Split(msg, []byte{0})
	if !bytes.Contains(fields[0], []byte("@")) {
		return false
	}
	env := map[string]string{}
	for _, f := range fields[1:] {
		if k, v, ok := bytes.Cut(f, []byte("=")); ok {
			env[string(k)] = string(v)
		}
	}
	switch env["ACTION"] {
	case "add", "remove", "bind", "unbind":
		return env["SUBSYSTEM"] == "usb" && env["DEVTYPE"] == "usb_device"
	}
	return false
}
