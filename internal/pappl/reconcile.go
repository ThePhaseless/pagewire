package pappl

import (
	"context"
	"errors"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/ippcaps"
)

// findBackoff is how long a queue the application did not list waits before
// Find-Devices is asked again for it.
// shortcut: a 30 s delay before a newly listed printer gets its queue; shorten it if users notice.
const findBackoff = 30 * time.Second

// Reconcile makes the PAPPL system hold exactly one queue per want, named
// want.Name, on the want's device with its driver, applies the defaults the
// queue supports, and deletes the queues no want names. It returns one result
// per want, in order. It is idempotent and never panics on IPP failures: every
// failure becomes a QueueResult with State "error", retried on the agent's
// next cycle. All wants belong to c's printer application.
//
// Find-Devices makes the application run every CUPS backend, and each claims
// its printer's USB interface, so asking competes with a job that is printing.
// Reconcile therefore asks at most once per call, only for the wants whose
// queue is not settled (settledURI), and not again within findBackoff for
// wants it listed nothing for. A settled want is never held back by a sibling.
func Reconcile(ctx context.Context, c *Client, wants []device.QueueSpec) []device.QueueResult {
	out := make([]device.QueueResult, len(wants))
	printers, err := c.Printers(ctx)
	if err != nil {
		for i, w := range wants {
			out[i] = failedErr(w, err)
		}
		return out
	}
	c.forgetGone(printers)

	devs := make([]Device, len(wants)) // the device each want's queue uses; URI "" when none
	var unsettled []int
	for i, w := range wants {
		switch {
		case w.DeviceURI != "":
			devs[i] = Device{URI: w.DeviceURI}
		default:
			last, _ := c.picked(w)
			if u := settledURI(printers, w, last.uri); u != "" {
				devs[i] = Device{URI: u, DeviceID: w.Device.DeviceID}
			} else {
				unsettled = append(unsettled, i)
			}
		}
	}
	done := make([]bool, len(wants))
	if len(unsettled) > 0 && !c.backedOff(wants, unsettled) {
		found, err := c.FindDevices(ctx)
		if err != nil {
			for _, i := range unsettled {
				out[i], done[i] = failedErr(wants[i], err), true
			}
		} else {
			pickDevices(found, wants, unsettled, devs)
			for _, i := range unsettled {
				c.setPicked(wants[i], devs[i].URI)
			}
		}
	}
	for _, i := range unsettled {
		if !done[i] && devs[i].URI == "" {
			r := device.QueueResult{ID: wants[i].ID, State: device.QueuePending, Message: "printer not listed by the printer application yet"}
			if p := byName(printers, wants[i].Name); p != nil {
				r.PrinterURI = p.URI // keep the existing queue usable through a transient miss
			}
			out[i], done[i] = r, true
		}
	}

	for _, p := range printers {
		if slices.ContainsFunc(wants, func(w device.QueueSpec) bool { return w.Name == p.Name }) {
			continue
		}
		if err := c.DeletePrinter(ctx, p.ID); err != nil {
			for i, w := range wants {
				if !done[i] {
					out[i], done[i] = failed(w, err.Error()), true
				}
			}
		}
	}

	for i, w := range wants {
		if !done[i] {
			out[i] = reconcileQueue(ctx, c, printers, w, devs[i])
		}
	}
	return out
}

// reconcileQueue keeps, recreates or creates the queue named want.Name on dev,
// touching only queues of that name, then sets its defaults when they differ.
func reconcileQueue(ctx context.Context, c *Client, printers []Printer, want device.QueueSpec, dev Device) device.QueueResult {
	var keep *Printer
	unlisted := lazyUnlisted(ctx, c, want.Driver)
	// A pinned driver the app does not list is an error; check before deleting
	// anything, so the queue that still works stays usable.
	if want.DriverPinned && want.Driver != "auto" && !slices.ContainsFunc(printers, func(p Printer) bool {
		return p.Name == want.Name && p.DeviceURI == dev.URI && p.Driver == want.Driver
	}) {
		missing, err := unlisted()
		if err != nil {
			return failed(want, err.Error())
		}
		if missing {
			r := failed(want, "driver "+want.Driver+" is not listed by the printer application")
			if p := byName(printers, want.Name); p != nil {
				r.PrinterURI = p.URI
			}
			return r
		}
	}
	for i := range printers {
		p := &printers[i]
		if p.Name != want.Name {
			continue
		}
		if p.DeviceURI == dev.URI {
			ok, err := keepDriver(p.Driver, want, unlisted)
			if err != nil {
				return failed(want, err.Error())
			}
			if ok {
				keep = p
				continue
			}
		}
		if err := c.DeletePrinter(ctx, p.ID); err != nil {
			return failed(want, err.Error())
		}
	}

	if keep == nil {
		if msg := create(ctx, c, want, dev, unlisted); msg != "" {
			return failed(want, msg)
		}
		printers, err := c.Printers(ctx)
		if err != nil {
			return failed(want, err.Error())
		}
		if keep = byName(printers, want.Name); keep == nil {
			return failed(want, "printer "+want.Name+" missing after Create-Printer")
		}
	}
	if keep.URI == "" {
		return failed(want, "printer "+want.Name+" has no ipp:// printer-uri-supported")
	}

	r := device.QueueResult{ID: want.ID, State: device.QueueOK, PrinterURI: keep.URI, DeviceURI: dev.URI}
	r.Warnings, r.Refused = setDefaults(ctx, c, keep, want)
	if want.Driver != "auto" && keep.Driver != want.Driver {
		r.Message = "Driver " + want.Driver + " is not in use: the printer application runs its own driver choice."
	}
	return r
}

// queueMemo is what reconcileQueue sent one queue: the defaults PAPPL refused
// and the last location set.
type queueMemo struct {
	refused map[string]string // attribute name -> refused value (goipp.Values.String)
	// location was set while the queue read back locationSeen. PAPPL 1.4.12
	// accepts printer-location but stores it as printer-geo-location
	// (printer-ipp.c), so the read-back never changes.
	location, locationSeen string
	locationSet            bool
}

// setDefaults sets the queue's location and the defaults it supports, sending
// only those that differ from what it reports: PAPPL rewrites its state file
// on every Set-Printer-Attributes. A value PAPPL refused is not sent again
// until the wanted value or the queue changes; it is reported in refused on
// every cycle instead, by option kind. The controller warns about the defaults
// the queue does not support.
func setDefaults(ctx context.Context, c *Client, keep *Printer, want device.QueueSpec) (warnings []string, refusedOpts []device.Dropped) {
	m := c.memo(keep.ID)
	var send goipp.Attributes
	if keep.Location != want.Location && (!m.locationSet || m.location != want.Location || m.locationSeen != keep.Location) {
		send.Add(goipp.MakeAttribute("printer-location", goipp.TagText, goipp.String(want.Location)))
	}
	fitted, _ := device.FitPrint(want.Defaults, keep.Caps)
	ch := changed(keep.Caps, fitted)
	send = append(send, ippcaps.Defaults(ch)...)

	// Only refusals of values still wanted and still differing are kept.
	refused := map[string]string{}
	send = slices.DeleteFunc(send, func(a goipp.Attribute) bool {
		v, ok := m.refused[a.Name]
		if !ok || v != a.Values.String() {
			return false
		}
		refused[a.Name] = v
		return true
	})
	if len(send) > 0 {
		rejected, err := c.SetDefaults(ctx, keep.URI, send)
		for _, a := range rejected {
			refused[a.Name] = a.Values.String()
		}
		if err != nil {
			warnings = append(warnings, "Could not set the printer's defaults: "+err.Error())
		} else if send[0].Name == "printer-location" && !slices.ContainsFunc(rejected, func(a goipp.Attribute) bool { return a.Name == "printer-location" }) {
			m.location, m.locationSeen, m.locationSet = want.Location, keep.Location, true
		}
	}
	m.refused = refused
	c.setMemo(keep.ID, m)
	for _, name := range slices.Sorted(maps.Keys(refused)) {
		if name == "printer-location" {
			warnings = append(warnings, "The printer refused the location “"+want.Location+"”: it keeps its own.")
		} else {
			// By option kind only, so the UI words it with its labels; an attribute
			// that maps to no differing kind leaves the queue as wanted, so nothing to report.
			refusedOpts = append(refusedOpts, refusedKinds(name, ch, keep.Caps)...)
		}
	}
	return warnings, refusedOpts
}

// refusedKinds maps a refused -default attribute back to the option kinds it
// carried, with the wanted value from ch and the queue's current default.
func refusedKinds(name string, ch device.PrintSettings, have device.PrinterCaps) []device.Dropped {
	var out []device.Dropped
	add := func(kind, v, using string) {
		if v != "" && v != using { // media-col-default carries unchanged members along
			out = append(out, device.Dropped{Option: kind, Value: v, Using: using})
		}
	}
	switch name {
	case "media-default":
		add(device.OptionMedia, ch.Media, have.MediaDefault)
	case "media-col-default":
		add(device.OptionMedia, ch.Media, have.MediaDefault)
		add(device.OptionMediaSource, ch.MediaSource, have.MediaSourceDefault)
		add(device.OptionMediaType, ch.MediaType, have.MediaTypeDefault)
	case "sides-default":
		add(device.OptionSides, ch.Sides, have.SidesDefault)
	case "print-color-mode-default":
		add(device.OptionColorMode, ch.ColorMode, have.ColorModeDefault)
	case "print-quality-default":
		add(device.OptionQuality, ch.Quality, have.QualityDefault)
	case "printer-resolution-default":
		if ch.Resolution != 0 {
			using := ""
			if have.ResolutionDefault != 0 {
				using = strconv.Itoa(have.ResolutionDefault)
			}
			add(device.OptionResolution, strconv.Itoa(ch.Resolution), using)
		}
	case "print-content-optimize-default":
		add(device.OptionContentOptimize, ch.ContentOptimize, have.ContentOptimizeDefault)
	case "orientation-requested-default":
		add(device.OptionOrientation, ch.Orientation, have.OrientationDefault)
	case "output-bin-default":
		add(device.OptionOutputBin, ch.OutputBin, have.OutputBinDefault)
	}
	return out
}

// changed returns the PAPPL-settable values of d that differ from the queue's
// current defaults, the others left empty. Media, tray and media type stay
// together, as media-col-default carries them together.
func changed(have device.PrinterCaps, d device.PrintSettings) device.PrintSettings {
	if !differs(have.MediaDefault, d.Media) && !differs(have.MediaSourceDefault, d.MediaSource) && !differs(have.MediaTypeDefault, d.MediaType) {
		d.Media, d.MediaSource, d.MediaType = "", "", ""
	}
	blank := func(have string, want *string) {
		if !differs(have, *want) {
			*want = ""
		}
	}
	blank(have.SidesDefault, &d.Sides)
	blank(have.ColorModeDefault, &d.ColorMode)
	blank(have.QualityDefault, &d.Quality)
	blank(have.ContentOptimizeDefault, &d.ContentOptimize)
	blank(have.OrientationDefault, &d.Orientation)
	blank(have.OutputBinDefault, &d.OutputBin)
	if !differsInt(have.ResolutionDefault, d.Resolution) {
		d.Resolution = 0
	}
	return d
}

func (c *Client) memo(id int) queueMemo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.memos[id]
}

func (c *Client) setMemo(id int, m queueMemo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.memos == nil {
		c.memos = map[int]queueMemo{}
	}
	c.memos[id] = m
}

// forgetGone drops the memos of queues the system no longer lists, so a
// restarted PAPPL that hands out the same printer-id starts clean.
func (c *Client) forgetGone(printers []Printer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	maps.DeleteFunc(c.memos, func(id int, _ queueMemo) bool {
		return !slices.ContainsFunc(printers, func(p Printer) bool { return p.ID == id })
	})
}

// keepDriver reports whether a queue with driver have serves want. An unpinned
// driver comes from the index, so a queue the app made with driver auto
// (earlier versions, or the fallback in create) stays, and so does any queue
// while the app does not list the wanted driver: create would fall back again.
func keepDriver(have string, want device.QueueSpec, unlisted func() (bool, error)) (bool, error) {
	switch {
	case want.Driver == "auto" || have == want.Driver:
		return true, nil
	case want.DriverPinned:
		return false, nil
	case have == "auto":
		return true, nil
	}
	return unlisted()
}

// lazyUnlisted reports whether the app does not list driver, asking
// Find-Drivers at most once and only when called.
func lazyUnlisted(ctx context.Context, c *Client, driver string) func() (bool, error) {
	var (
		done     bool
		unlisted bool
		err      error
	)
	return func() (bool, error) {
		if !done {
			var ds []Driver
			ds, err = c.FindDrivers(ctx, "")
			unlisted = !slices.ContainsFunc(ds, func(d Driver) bool { return d.Name == driver })
			done = true
		}
		return unlisted, err
	}
}

// create adds the queue. The retrofit apps accept any driver name in
// Create-Printer, so an unpinned named driver is first checked against
// Find-Drivers: one the app lacks (the index came from other package versions)
// falls back to driver auto. Reconcile has already checked a pinned one.
// Driver auto falls back to the first Find-Drivers match when the application
// refuses it. It returns an error message, or "" on success.
func create(ctx context.Context, c *Client, want device.QueueSpec, dev Device, unlisted func() (bool, error)) string {
	driver := want.Driver
	if driver != "auto" && !want.DriverPinned {
		missing, err := unlisted()
		if err != nil {
			return err.Error()
		}
		if missing {
			driver = "auto"
		}
	}
	_, err := c.CreatePrinter(ctx, want.Name, dev.URI, driver, dev.DeviceID)
	var ie *IPPError
	if err == nil || driver != "auto" || !errors.As(err, &ie) || ie.Status != goipp.StatusErrorAttributesOrValues {
		return errText(err)
	}
	if dev.DeviceID == "" {
		return "driver auto needs a device ID"
	}
	drivers, ferr := c.FindDrivers(ctx, dev.DeviceID)
	if ferr != nil {
		return ferr.Error()
	}
	if len(drivers) == 0 {
		return "no driver of the printer application matches " + dev.DeviceID
	}
	_, err = c.CreatePrinter(ctx, want.Name, dev.URI, drivers[0].Name, dev.DeviceID)
	return errText(err)
}

// settledURI returns the device URI of the queue named want.Name when it is a
// CUPS backend URI on want.Device's serial (none matches none; not checked
// when no device is bound), and either on the preferred scheme or on picked,
// this client's last Find-Devices choice for want. Reconcile then leaves
// Find-Devices unasked for want.
func settledURI(printers []Printer, want device.QueueSpec, picked string) string {
	p := byName(printers, want.Name)
	if p == nil || len(want.Schemes) == 0 || !strings.HasPrefix(p.DeviceURI, "cups:") {
		return ""
	}
	if scheme(p.DeviceURI) != want.Schemes[0] && p.DeviceURI != picked {
		return ""
	}
	if want.Device.VendorID != "" && !strings.EqualFold(serialOf(p.DeviceURI), want.Device.Serial) {
		return ""
	}
	return p.DeviceURI
}

// picked returns the last Find-Devices choice for want's name, device and
// schemes; ok is false when Find-Devices was never asked for it. The key holds
// the device, so a want rebound to another device asks again, and a listing
// that offered nothing better than a lesser scheme stays settled.
func (c *Client) picked(want device.QueueSpec) (p pick, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok = c.picks[pickKey(want)]
	return p, ok
}

func (c *Client) setPicked(want device.QueueSpec, uri string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.picks == nil {
		c.picks = map[string]pick{}
	}
	c.picks[pickKey(want)] = pick{uri: uri, at: c.clock()}
}

// backedOff reports whether every unsettled want was listed nothing for by a
// Find-Devices less than findBackoff ago. A want never asked for asks at once.
func (c *Client) backedOff(wants []device.QueueSpec, unsettled []int) bool {
	now := c.clock()
	for _, i := range unsettled {
		p, ok := c.picked(wants[i])
		if !ok || p.uri != "" || now.Sub(p.at) >= findBackoff {
			return false
		}
	}
	return true
}

func (c *Client) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func pickKey(want device.QueueSpec) string {
	return want.Name + "\x00" + want.Device.Key().String() + "\x00" + strings.Join(want.Schemes, "\x00")
}

// pickDevices fills devs[i] for every unsettled want i from the Find-Devices
// entries found. Matching runs in passes over all of them: serial, then MFG
// and MDL from the device ID, then the single remaining candidate when exactly
// one want is still unmatched. An entry another want already uses (by URI or
// serial) is taken, and the later passes skip it, so a printer the app does
// not list (switched off) never gets its sibling's device. Matching never
// compares VID:PID, which CUPS backends do not list.
func pickDevices(found []Device, wants []device.QueueSpec, unsettled []int, devs []Device) {
	taken := func(i int, d Device) bool {
		for j := range wants {
			if j == i || devs[j].URI == "" {
				continue
			}
			if devs[j].URI == d.URI {
				return true
			}
			if s := serialOf(devs[j].URI); s != "" && strings.EqualFold(s, serialOf(d.URI)) {
				return true
			}
		}
		return false
	}
	pass := func(match func(w device.QueueSpec, d Device) bool) {
		for _, i := range unsettled {
			if devs[i].URI != "" {
				continue
			}
			var matches []Device
			for _, d := range found {
				if slices.Contains(wants[i].Schemes, scheme(d.URI)) && !taken(i, d) && match(wants[i], d) {
					matches = append(matches, d)
				}
			}
			if len(matches) == 0 {
				continue
			}
			// Stable: among equal schemes the first listed entry wins.
			slices.SortStableFunc(matches, func(a, b Device) int {
				return slices.Index(wants[i].Schemes, scheme(a.URI)) - slices.Index(wants[i].Schemes, scheme(b.URI))
			})
			devs[i] = matches[0]
		}
	}
	pass(func(w device.QueueSpec, d Device) bool {
		return w.Device.Serial != "" && strings.EqualFold(serialOf(d.URI), w.Device.Serial)
	})
	pass(func(w device.QueueSpec, d Device) bool {
		want := device.ParseDeviceID(w.Device.DeviceID)
		id := device.ParseDeviceID(d.DeviceID)
		return want["MFG"] != "" && want["MDL"] != "" && strings.EqualFold(id["MFG"], want["MFG"]) && strings.EqualFold(id["MDL"], want["MDL"])
	})
	var left []int
	for _, i := range unsettled {
		if devs[i].URI == "" {
			left = append(left, i)
		}
	}
	if len(left) != 1 {
		return
	}
	i := left[0]
	var cands []Device
	for _, d := range found {
		if slices.Contains(wants[i].Schemes, scheme(d.URI)) && !taken(i, d) {
			cands = append(cands, d)
		}
	}
	if len(cands) == 1 {
		devs[i] = cands[0]
	}
}

// backendURI strips the optional "cups:" prefix pappl-retrofit adds.
func backendURI(uri string) string { return strings.TrimPrefix(uri, "cups:") }

// scheme is the backend URI's scheme: the text before its first ':'.
func scheme(uri string) string {
	s, _, _ := strings.Cut(backendURI(uri), ":")
	return s
}

func serialOf(uri string) string {
	_, q, ok := strings.Cut(backendURI(uri), "?")
	if !ok {
		return ""
	}
	v, err := url.ParseQuery(q)
	if err != nil {
		return ""
	}
	return v.Get("serial")
}

func byName(ps []Printer, name string) *Printer {
	for i := range ps {
		if ps[i].Name == name {
			return &ps[i]
		}
	}
	return nil
}

// differs reports whether a wanted (non-empty) default differs from the
// queue's; an empty wanted value leaves the queue's default alone.
func differs(have, want string) bool { return want != "" && have != want }

// differsInt is differs for int kinds, where 0 means not wanted.
func differsInt(have, want int) bool { return want != 0 && have != want }

func failed(want device.QueueSpec, msg string) device.QueueResult {
	return device.QueueResult{ID: want.ID, State: device.QueueError, Message: msg}
}

// failedErr is failed for err, flagging a refused connection: the printer
// application is still starting.
func failedErr(want device.QueueSpec, err error) device.QueueResult {
	r := failed(want, err.Error())
	r.Unreachable = errors.Is(err, syscall.ECONNREFUSED)
	return r
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
