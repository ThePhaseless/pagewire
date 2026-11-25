package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var textSHARe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Known lists what the catalog offers, so this package need not import it.
type Known struct {
	EULAs    []EULARef // every EULA id with its current version
	Printers []string  // printer family ids
}

// Validate runs the semantic checks a parsed config needs: ids are unique
// across printers and scanners, printer names are unique ignoring case, usb
// selectors appear only with mode usb, at most one usb printer and one usb
// scanner leave theirs out and no two entries of one kind share one, at most
// one scanner is simulated, a printer's driver is a catalog family or auto,
// driverName is not set where no PAPPL driver runs, an ignored device role is
// not also in use, and accepted EULAs exist in the catalog (matched on id and
// version), appear once and carry a text sha256. Whether an app lists
// driverName is not checked here: the driver index follows image updates, so
// a name it drops degrades the printer instead of invalidating the config.
// Errors are an *ErrorList without positions; Check adds them.
func Validate(c *Config, k Known) error {
	el := &ErrorList{}
	add := func(path, format string, args ...any) {
		el.Errors = append(el.Errors, FieldError{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	ids := map[string]string{} // id -> path of its first entry
	id := func(at, v string) {
		if first, ok := ids[v]; ok {
			add(at+"/id", "duplicate id %s: also %s", v, first)
			return
		}
		ids[v] = at
	}
	// selectors checks one kind's usb selectors: only with mode usb, at most
	// one entry without one, and no two equal ones.
	type sel struct {
		at   string
		mode string
		usb  *USB
	}
	selectors := func(kind string, entries []sel) {
		bare, seen := "", map[USB]string{}
		for _, e := range entries {
			switch {
			case e.usb != nil && e.mode != ModeUSB:
				add(e.at+"/usb", "is only allowed with mode: usb")
			case e.usb != nil:
				if first, ok := seen[*e.usb]; ok {
					add(e.at+"/usb", "selects the same device as %s", first)
				} else {
					seen[*e.usb] = e.at
				}
			case e.mode == ModeUSB && bare != "":
				add(e.at, "needs a usb selector: %s already has none, and only one usb %s may leave it out", bare, kind)
			case e.mode == ModeUSB:
				bare = e.at
			}
		}
	}

	names := map[string]string{}
	var ps []sel
	for i, p := range c.Printers {
		at := fmt.Sprintf("/printers/%d", i)
		id(at, p.ID)
		if first, ok := names[strings.ToLower(p.Name)]; ok {
			add(at+"/name", "duplicate printer name %s: also %s (names are compared ignoring case)", p.Name, first)
		} else {
			names[strings.ToLower(p.Name)] = at
		}
		ps = append(ps, sel{at, p.Mode, p.USB})
		if p.Driver != Auto && !slices.Contains(k.Printers, p.Driver) {
			add(at+"/driver", "driver %s is not in the catalog", p.Driver)
		}
		if p.DriverName != "" {
			switch {
			case p.Mode == ModeSimulated:
				add(at+"/driverName", "is not allowed with mode: simulated")
			case p.Driver == "ipp-usb":
				add(at+"/driverName", "is not allowed with driver: ipp-usb, which needs no driver")
			}
		}
	}
	selectors(RolePrinter, ps)

	var ss []sel
	simulated := false
	for i, s := range c.Scanners {
		at := fmt.Sprintf("/scanners/%d", i)
		id(at, s.ID)
		ss = append(ss, sel{at, s.Mode, s.USB})
		if s.Simulated() {
			// shortcut: one simulated scanner, because nothing tells two SANE
			// test scanners apart; lift the cap when the sim needs several, by
			// giving each a distinct test device and an endpoint matching rule.
			if simulated {
				add(at+"/mode", "only one simulated scanner is supported")
			}
			simulated = true
		}
	}
	selectors(RoleScanner, ss)

	for i, d := range c.IgnoredDevices {
		ignored := USB{VendorID: d.VendorID, ProductID: d.ProductID, Serial: d.Serial}
		for _, role := range d.Roles {
			used := ""
			if role == RolePrinter {
				for _, e := range ps {
					if e.usb != nil && *e.usb == ignored {
						used = e.at
					}
				}
			} else {
				for _, e := range ss {
					if e.usb != nil && *e.usb == ignored {
						used = e.at
					}
				}
			}
			if used != "" {
				add(fmt.Sprintf("/ignoredDevices/%d", i), "role %s is ignored and used: remove one (%s)", role, used)
			}
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
// validate command share: a pagewire/v1 file is migrated in memory first
// (Migrate), then Parse and Validate run, with file positions added to the
// semantic errors. A v1 file that fails v1 parsing gets its v1 errors and
// positions; any later error of a v1 file carries positions in the migrated
// text, not in the file.
func Check(b []byte, k Known) (*Config, error) {
	b, _, err := Migrate(b)
	if err != nil {
		return nil, err
	}
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
