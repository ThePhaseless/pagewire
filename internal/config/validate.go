package config

import (
	"errors"
	"fmt"
	"slices"
)

// Known lists what the catalog offers, so this package need not import it.
type Known struct {
	EULAs    []EULARef // every EULA id with its current version
	Printers []string  // printer family ids
}

// Validate runs the semantic checks a parsed config needs: accepted EULAs
// exist in the catalog, the driver is a catalog family or auto, and
// device.usb is only set with mode usb. Errors are an *ErrorList without
// positions; Check adds them.
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
	for i, e := range c.Licenses.AcceptedEULAs {
		if !slices.Contains(k.EULAs, e) {
			add(fmt.Sprintf("/licenses/acceptedEulas/%d", i), "EULA %s version %s is not in the catalog", e.ID, e.Version)
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
