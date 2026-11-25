package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
)

var textSHARe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Known lists what the catalog offers, so this package need not import it.
type Known struct {
	EULAs    []EULARef // every EULA id with its current version
	Printers []string  // printer family ids
}

// Validate runs the semantic checks a parsed config needs: accepted EULAs
// exist in the catalog (matched on id and version), appear once and carry a
// text sha256, the driver is a catalog family or auto, device.usb is only set
// with mode usb, and device.driverName is not set where no PAPPL driver runs.
// Whether an app lists driverName is not checked here: the driver index follows
// image updates, so a name it drops degrades the device instead of invalidating
// the config. Errors are an *ErrorList without positions; Check adds them.
func Validate(c *Config, k Known) error {
	el := &ErrorList{}
	add := func(path, format string, args ...any) {
		el.Errors = append(el.Errors, FieldError{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	if c.Device.USB != nil && c.Device.Mode != ModeUSB {
		add("/device/usb", "is only allowed with mode: usb")
	}
	if c.Device.Driver != Auto && !slices.Contains(k.Printers, c.Device.Driver) {
		add("/device/driver", "driver %s is not in the catalog", c.Device.Driver)
	}
	if c.Device.DriverName != "" {
		switch {
		case c.Device.Mode == ModeSimulated:
			add("/device/driverName", "is not allowed with mode: simulated")
		case c.Device.Driver == "ipp-usb":
			add("/device/driverName", "is not allowed with driver: ipp-usb, which needs no driver")
		}
	}
	for i, e := range c.Licenses.AcceptedEULAs {
		at := fmt.Sprintf("/licenses/acceptedEulas/%d", i)
		same := func(o EULARef) bool { return o.ID == e.ID && o.Version == e.Version }
		switch {
		case !slices.ContainsFunc(k.EULAs, same):
			add(at, "EULA %s version %s is not in the catalog", e.ID, e.Version)
		case slices.ContainsFunc(c.Licenses.AcceptedEULAs[:i], same):
			add(at, "duplicate: EULA %s version %s is already accepted above", e.ID, e.Version)
		}
		switch {
		case e.TextSHA256 == "":
			add(at+"/textSha256", "is required: review and accept the licence again in Settings")
		case !textSHARe.MatchString(e.TextSHA256):
			add(at+"/textSha256", "must be 64 lowercase hex characters: review and accept the licence again in Settings")
		}
	}
	if len(el.Errors) > 0 {
		return el
	}
	return nil
}

// Check is the one check function the store, the watcher, the API and the
// validate command share: Parse, then Validate, with file positions added to
// the semantic errors.
func Check(b []byte, k Known) (*Config, error) {
	c, file, err := parse(b)
	if err != nil {
		return nil, err
	}
	if err := Validate(c, k); err != nil {
		var el *ErrorList
		if errors.As(err, &el) && len(file.Docs) > 0 {
			locate(file.Docs[0].Body, el)
		}
		return nil, err
	}
	return c, nil
}
