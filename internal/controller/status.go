package controller

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/printing"
)

// Backend names serve passes as Options.BackendName (the Setup DTO's backend).
const (
	BackendK8s    = "k8s"
	BackendDocker = "docker"
	BackendSim    = "sim"
)

// Status messages the controller shows.
const (
	msgWaitingAgent  = "Waiting for the device agent"
	msgUnschedulable = "Printer not plugged in or not visible to the device plugin"
	msgNotPlugged    = "Not plugged in, or not visible to pagewire"
	msgNotReporting  = "Device agent not reporting"
	msgNoQueues      = "Device agent not reporting queues"
	msgStarting      = "Starting the device workload"
	msgStartingApp   = "Starting the printer application"
	msgNewDevice     = "Restarting the device workload to read the new device"
	msgLeftOut       = "Its node is past the limit of 4 nodes with devices, so pagewire does not serve it"
	msgWaitPrinter   = "Waiting for the printer"
	msgWaitScanner   = "Waiting for the scanner"
	msgPodFailed     = "Device pod failed"
	msgNothingInUse  = "Nothing in use"
	msgUSBStale      = "The printer's USB connection changed and the device pod still has the old one. pagewire restarted it once; check the device plugin"
	// msgNoDriver is the degraded message while no printer driver matches the device (rule none).
	msgNoDriver = "No printer driver matches this device. Choose a driver in Settings."
	// msgDriverNotListed is the degraded message while driverName names a
	// driver the driver index does not list for the chosen family (an image
	// update dropped it, a typo, or a driver that needs hpijs). With a family
	// override, pagewire's own choice is that family's match.
	msgDriverNotListed = "Driver %s is not in pagewire's driver index for this printer application, so pagewire's own choice is used. Choose another driver in Settings."
)

const defaultName = "Printer"

// initialStatus is Current() before any input: config missing, phase none,
// empty lists.
func initialStatus() apitypes.Status {
	return apitypes.Status{
		Config:   apitypes.StatusConfig{State: apitypes.ConfigMissing, Errors: []config.FieldError{}},
		Phase:    apitypes.PhaseNone,
		Printers: []apitypes.StatusPrinter{}, Scanners: []apitypes.StatusScanner{},
		Workloads: []apitypes.StatusWorkload{}, Devices: []apitypes.DetectedDevice{},
		Reconcile: []apitypes.ReconcileItem{},
	}
}

// offlinePrinter is a printer entry's Status before its endpoint answers.
func offlinePrinter(p config.Printer) apitypes.StatusPrinter {
	return apitypes.StatusPrinter{
		ID: p.ID, Name: p.Name, Location: p.Location, Enabled: p.Enabled, Warnings: []apitypes.Warning{},
		State: apitypes.StateUnknown, Reasons: []string{}, Formats: []string{}, ConvertedFormats: []apitypes.FormatConversion{},
		Media: []string{}, Markers: []apitypes.Marker{}, Capabilities: printerCaps(device.PrinterCaps{}),
		Connect: apitypes.PrinterConnect{IPPURIs: []string{}},
	}
}

// offlineScanner is a scanner entry's Status before its endpoint answers.
func offlineScanner(s config.Scanner) apitypes.StatusScanner {
	return apitypes.StatusScanner{
		ID: s.ID, Name: s.Name, Location: s.Location, Enabled: s.Enabled, Warnings: []apitypes.Warning{},
		State: apitypes.StateUnknown, Capabilities: scannerCaps(apitypes.ScannerCaps{}),
		Connect: apitypes.ScannerConnect{ESCLURLs: []string{}},
	}
}

// printerCaps is c with every list non-nil.
func printerCaps(c device.PrinterCaps) device.PrinterCaps {
	for _, l := range []*[]string{
		&c.Media, &c.MediaSources, &c.MediaTypes, &c.Sides, &c.ColorModes, &c.Qualities, &c.ContentOptimize,
		&c.Orientations, &c.PrintScalings, &c.OutputBins, &c.Finishings, &c.Formats,
	} {
		*l = nonNil(*l)
	}
	c.Resolutions, c.NumberUp = nonNil(c.Resolutions), nonNil(c.NumberUp)
	return c
}

// scannerCaps is c with every list non-nil.
func scannerCaps(c apitypes.ScannerCaps) apitypes.ScannerCaps {
	c.Sources, c.Resolutions, c.ColorModes, c.Intents = nonNil(c.Sources), nonNil(c.Resolutions), nonNil(c.ColorModes), nonNil(c.Intents)
	return c
}

// ippUSBPort reports whether port is one ipp-usb listens on.
func ippUSBPort(port int) bool { return port >= device.IPPUSBMinPort && port <= device.IPPUSBMaxPort }

// onPort is a port filter for one port.
func onPort(port int) func(int) bool { return func(p int) bool { return p == port } }

// pick returns the instance of type typ with TXT key on a port on accepts
// (nil: any) that scores highest, ties going to name order.
func pick(services []device.DNSSDService, typ, key string, on func(int) bool, score func(device.DNSSDService) int) (device.DNSSDService, bool) {
	var best device.DNSSDService
	bestScore, found := 0, false
	for _, s := range services {
		if _, ok := s.TXT[key]; !ok || s.Type != typ || on != nil && !on(s.Port) {
			continue
		}
		sc := 0
		if score != nil {
			sc = score(s)
		}
		if !found || sc > bestScore || sc == bestScore && s.Name < best.Name {
			best, bestScore, found = s, sc, true
		}
	}
	return best, found
}

// ippUSBScore ranks an ipp-usb _ipp._tcp instance for device d: its TXT
// usb_MFG and usb_MDL (ipp-usb 0.9.34 sets both from the device ID) equal d's
// MFG and MDL. shortcut: two identical IPP-USB models on one node may swap;
// match on a serial-bearing TXT key if ipp-usb gains one.
func ippUSBScore(d device.USBDevice) func(device.DNSSDService) int {
	id := device.ParseDeviceID(d.DeviceID)
	return func(s device.DNSSDService) int {
		mfg, mdl := s.TXT["usb_MFG"], s.TXT["usb_MDL"]
		if mfg != "" && mdl != "" && strings.EqualFold(mfg, id["MFG"]) && strings.EqualFold(mdl, id["MDL"]) {
			return 1
		}
		return 0
	}
}

// tyScore ranks an AirSane _uscan._tcp instance for device d: TXT ty holding
// the USB product string, then the manufacturer (case-insensitive).
func tyScore(d device.USBDevice) func(device.DNSSDService) int {
	return func(s device.DNSSDService) int {
		ty := strings.ToLower(s.TXT["ty"])
		switch {
		case ty == "":
			return 0
		case d.Product != "" && strings.Contains(ty, strings.ToLower(d.Product)):
			return 2
		case d.Manufacturer != "" && strings.Contains(ty, strings.ToLower(d.Manufacturer)):
			return 1
		}
		return 0
	}
}

func serviceURL(scheme, hostIP string, s device.DNSSDService, key string) string {
	return scheme + "://" + net.JoinHostPort(hostIP, strconv.Itoa(s.Port)) + "/" + strings.TrimPrefix(s.TXT[key], "/")
}

// queueResult is the report's queue result for printer entry id.
func queueResult(r *device.AgentReport, id string) *device.QueueResult {
	if r == nil {
		return nil
	}
	for i := range r.Queues {
		if r.Queues[i].ID == id {
			return &r.Queues[i]
		}
	}
	return nil
}

// endpoint is one entry's matched address: the URI or URL and the DNS-SD
// instance name it came from ("" none).
type endpoint struct {
	uri, dnssd string
	ippUSB     bool // served by ipp-usb, which ties up one of the device's few IPP-USB interfaces per request in flight
}

// workloadIn is what the endpoint and phase rules read of one workload.
type workloadIn struct {
	name string
	spec *device.Spec
	obs  device.Observation
	rs   *reportState // nil: no report since start
}

func (w workloadIn) report() *device.AgentReport {
	if w.rs == nil {
		return nil
	}
	return &w.rs.r
}

// printerEndpoint is A9 for printer entry p served as ps on workload w.
func printerEndpoint(cat *catalog.Catalog, backend string, w workloadIn, p config.Printer, ps device.PrinterSpec, dev device.USBDevice) endpoint {
	fam, _ := cat.Printer(ps.Family)
	r := w.report()
	switch {
	case fam.App != "": // PAPPL: the agent's loopback URI, rewritten to the host IP
		q := queueResult(r, p.ID)
		if q == nil || q.PrinterURI == "" || w.obs.HostIP == "" {
			return endpoint{}
		}
		u, err := url.Parse(q.PrinterURI)
		if err != nil || u.Port() == "" {
			return endpoint{}
		}
		e := endpoint{uri: "ipp://" + net.JoinHostPort(w.obs.HostIP, u.Port()) + u.EscapedPath()}
		s, ok := pick(r.Services, device.ServiceIPP, "rp", onPort(fam.Port), func(s device.DNSSDService) int {
			if s.Name == p.Name {
				return 1
			}
			return 0
		})
		if ok && (s.Name == p.Name || countOn(r.Services, device.ServiceIPP, fam.Port) == 1) {
			e.dnssd = s.Name
		}
		return e
	case backend == BackendSim:
		for _, e := range w.obs.Endpoints {
			if e.ID == p.ID {
				return endpoint{uri: e.PrinterURI}
			}
		}
		return endpoint{}
	case r == nil || w.obs.HostIP == "":
		return endpoint{}
	case fam.ID == "ipp-usb":
		if s, ok := pick(r.Services, device.ServiceIPP, "rp", ippUSBPort, ippUSBScore(dev)); ok {
			return endpoint{uri: serviceURL("ipp", w.obs.HostIP, s, "rp"), dnssd: s.Name, ippUSB: true}
		}
		return endpoint{}
	}
	// Simulated printers: ippeveprinter on the printer's own port, else by name.
	s, ok := pick(r.Services, device.ServiceIPP, "rp", onPort(ps.Port), nil)
	if !ok {
		s, ok = pick(r.Services, device.ServiceIPP, "rp", nil, nil)
		ok = ok && s.Name == p.Name
	}
	if !ok {
		return endpoint{}
	}
	return endpoint{uri: serviceURL("ipp", w.obs.HostIP, s, "rp"), dnssd: s.Name}
}

func countOn(services []device.DNSSDService, typ string, port int) int {
	n := 0
	for _, s := range services {
		if s.Type == typ && s.Port == port {
			n++
		}
	}
	return n
}

// scannerEndpoint is A9 for scanner entry sc served as ss on workload w.
// usbAirSane are the devices of the workload's AirSane USB scanners.
func scannerEndpoint(cat *catalog.Catalog, backend string, w workloadIn, sc config.Scanner, ss device.ScannerSpec, dev device.USBDevice, usbAirSane []device.USBDevice) endpoint {
	r := w.report()
	switch {
	case backend == BackendSim:
		for _, e := range w.obs.Endpoints {
			if e.ID == sc.ID {
				return endpoint{uri: e.ScannerURL}
			}
		}
		return endpoint{}
	case r == nil || w.obs.HostIP == "":
		return endpoint{}
	}
	found := func(s device.DNSSDService, ok bool) endpoint {
		if !ok {
			return endpoint{}
		}
		return endpoint{uri: serviceURL("http", w.obs.HostIP, s, "rs"), dnssd: s.Name}
	}
	switch ss.Family {
	case "ipp-usb": // ipp-usb serves IPP and eSCL of one device on one port
		if p, ok := pick(r.Services, device.ServiceIPP, "rp", ippUSBPort, ippUSBScore(dev)); ok && ippUSBScore(dev)(p) > 0 {
			if s, ok := pick(r.Services, device.ServiceUScan, "rs", onPort(p.Port), nil); ok {
				return found(s, true)
			}
		}
		return found(pick(r.Services, device.ServiceUScan, "rs", ippUSBPort, ippUSBScore(dev)))
	case "sim":
		// The simulated scanner is the AirSane instance that is none of the
		// workload's USB scanners.
		return found(pick(r.Services, device.ServiceUScan, "rs", onPort(scannerPort(cat, ss.Family)), func(s device.DNSSDService) int {
			for _, d := range usbAirSane {
				if tyScore(d)(s) > 0 {
					return -1
				}
			}
			return 0
		}))
	}
	return found(pick(r.Services, device.ServiceUScan, "rs", onPort(scannerPort(cat, ss.Family)), tyScore(dev)))
}

// scannerPort is the catalog port of scanner family id, 0 when it has none.
func scannerPort(cat *catalog.Catalog, id string) int {
	if i := slices.IndexFunc(cat.Scanners, func(s catalog.Scanner) bool { return s.ID == id }); i >= 0 {
		return cat.Scanners[i].Port
	}
	return 0
}

func advertised(r *device.AgentReport) []apitypes.AdvertisedService {
	out := []apitypes.AdvertisedService{}
	if r != nil {
		for _, s := range r.Services {
			out = append(out, apitypes.AdvertisedService{Type: s.Type, Name: s.Name, Port: s.Port})
		}
	}
	return out
}

// phaseIn is what one entry's phase rule (A10) reads.
type phaseIn struct {
	enabled     bool
	usbErr      string // USBUnavailable for a usb entry, else ""
	bound       bool   // the entry has a device, or is simulated
	inSpec      bool
	noDriver    bool // printer rule none
	leftOut     bool // its node is past the workload cap
	wait        bool // the control-plane restart wait keeps the old specs
	unreported  bool // a workload of the set has not reported since start
	w           workloadIn
	obsErr      error
	applyErr    string
	pappl       bool
	queue       *device.QueueResult
	stale       bool // a node-change replug key survived its restart
	degraded    string
	available   bool
	waitMessage string
	now         time.Time
	wake        *wakeAt // records the start grace's end
}

// entryPhase is A10's per-device phase, highest priority first.
func entryPhase(in phaseIn) (string, string) {
	o, rs := in.w.obs, in.w.rs
	switch {
	case !in.enabled:
		return apitypes.PhaseNone, ""
	case in.usbErr != "":
		return apitypes.PhaseError, in.usbErr
	case !in.bound && (in.wait || in.unreported):
		return apitypes.PhaseDeploying, msgWaitingAgent
	case !in.bound:
		return apitypes.PhaseProbing, msgNotPlugged
	case in.noDriver:
		return apitypes.PhaseDegraded, msgNoDriver
	case in.leftOut:
		return apitypes.PhaseError, msgLeftOut
	case !in.inSpec && in.wait:
		return apitypes.PhaseDeploying, msgWaitingAgent
	case !in.inSpec:
		return apitypes.PhaseDeploying, msgNewDevice
	case in.queue != nil && in.queue.State == device.QueueError:
		// Connection refused: the printer application is still starting while
		// the pod is young or a container is still waiting.
		waiting := o.Message != "" && o.Phase != device.PhaseFailed && !o.Unschedulable
		if in.queue.Unreachable && (rs != nil && in.wake.within(in.now, rs.firstAt, startGrace) || waiting) {
			return apitypes.PhaseDeploying, msgStartingApp
		}
		return apitypes.PhaseError, in.queue.Message
	case in.applyErr != "":
		return apitypes.PhaseError, in.applyErr
	case in.obsErr != nil:
		return apitypes.PhaseError, in.obsErr.Error()
	case o.Phase == device.PhaseFailed:
		return apitypes.PhaseError, orDefault(o.Message, msgPodFailed)
	case o.Unschedulable:
		return apitypes.PhaseProbing, msgUnschedulable
	case rs != nil && silent(in.now, rs.at):
		return apitypes.PhaseDegraded, msgNotReporting
	case rs == nil:
		return apitypes.PhaseDeploying, msgWaitingAgent
	case in.stale:
		return apitypes.PhaseDegraded, msgUSBStale
	case in.degraded != "":
		return apitypes.PhaseDegraded, in.degraded
	case in.queue != nil && in.queue.State == device.QueueOK && in.queue.Message != "":
		return apitypes.PhaseDegraded, in.queue.Message // the queue runs the app's own driver choice
	case o.Phase != device.PhaseRunning:
		return apitypes.PhaseDeploying, orDefault(o.Message, msgStarting)
	case in.pappl && in.queue == nil:
		return apitypes.PhaseDeploying, msgNoQueues
	case in.queue != nil && in.queue.State == device.QueuePending:
		return apitypes.PhaseDeploying, orDefault(in.queue.Message, in.waitMessage)
	case !in.available:
		return apitypes.PhaseDeploying, in.waitMessage
	}
	return apitypes.PhaseReady, ""
}

// workloadMessage is a workload's own state message, and whether it is an error.
func workloadMessage(w workloadIn, obsErr error, applyErr string, now time.Time) (string, bool) {
	switch {
	case obsErr != nil:
		return obsErr.Error(), true
	case applyErr != "":
		return applyErr, true
	case w.spec == nil:
		return "", false
	case w.obs.Phase == device.PhaseFailed:
		return orDefault(w.obs.Message, msgPodFailed), true
	case w.obs.Unschedulable:
		return msgUnschedulable, false
	case w.obs.Message != "":
		return w.obs.Message, false
	case w.rs == nil:
		return msgWaitingAgent, false
	case silent(now, w.rs.at):
		return msgNotReporting, false
	}
	return "", false
}

// warning is the Warning for a dropped option of a printer or scanner.
func warning(option, value, using, role string) apitypes.Warning {
	return apitypes.Warning{
		Option: option, Value: value, Using: using,
		Message: fmt.Sprintf("%s %s is not offered by this %s; it uses %s", option, value, role, orDefault(using, "its own default")),
	}
}

// fitScan drops every configured scanner default the caps do not list
// (source, resolution, colorMode, intent), using the first listed value instead.
func fitScan(d config.ScanDefaults, caps apitypes.ScannerCaps) []apitypes.Warning {
	out := []apitypes.Warning{}
	str := func(option, v string, supported []string) {
		if v != "" && !slices.Contains(supported, v) {
			using := ""
			if len(supported) > 0 {
				using = supported[0]
			}
			out = append(out, warning(option, v, using, "scanner"))
		}
	}
	str(device.OptionSource, d.Source, caps.Sources)
	if d.Resolution != 0 && !slices.Contains(caps.Resolutions, d.Resolution) {
		using := ""
		if len(caps.Resolutions) > 0 {
			using = strconv.Itoa(caps.Resolutions[0])
		}
		out = append(out, warning(device.OptionResolution, strconv.Itoa(d.Resolution), using, "scanner"))
	}
	str(device.OptionColorMode, d.ColorMode, caps.ColorModes)
	str(device.OptionIntent, d.Intent, caps.Intents)
	return out
}

// printerWarnings are a printer's option warnings: the configured defaults
// the caps do not offer, the defaults the queue refused, then the agent's
// other queue warnings.
func printerWarnings(p config.Printer, caps *device.PrinterCaps, q *device.QueueResult) []apitypes.Warning {
	out := []apitypes.Warning{}
	if caps != nil {
		_, dropped := device.FitPrint(device.PrintSettings(p.Defaults), *caps)
		for _, d := range dropped {
			out = append(out, warning(d.Option, d.Value, d.Using, "printer"))
		}
	}
	if q != nil {
		for _, d := range q.Refused { // offered, but PAPPL would not make it the default
			w := warning(d.Option, d.Value, d.Using, "printer")
			w.Message = fmt.Sprintf("%s %s could not be set as the printer's default; it uses %s", d.Option, d.Value, orDefault(d.Using, "its own default"))
			out = append(out, w)
		}
		for _, m := range q.Warnings {
			out = append(out, apitypes.Warning{Option: device.OptionOther, Message: m})
		}
	}
	return out
}

// query is one endpoint query's result.
type printerQuery struct {
	attrs printing.Attributes
	err   error
}

type scannerQuery struct {
	state   string
	ok      bool
	caps    apitypes.ScannerCaps
	capsErr error
	asked   bool // Capabilities was called
}

// absentKey reports whether an entry's device key is known to be unplugged:
// the key is non-zero and missing from a non-nil Attached list of its
// workload's latest report. A zero key (or no list) counts as present.
func absentKey(key device.USBKey, rs *reportState) bool {
	if key == (device.USBKey{}) || rs == nil || rs.r.Attached == nil {
		return false
	}
	return !slices.ContainsFunc(rs.r.Attached, func(d device.USBDevice) bool { return d.Key() == key })
}

// observe matches endpoints, queries every printer and scanner (no lock held,
// concurrently) and builds the loop's parts of Status, and the endpoint
// watchers' targets with this step's results as their baselines.
func (c *Controller) observe(ctx context.Context, in snapshot, v view, d desired, obs map[string]device.Observation, obsErr error, applyErr func(workload string) string) (apitypes.Status, []watchTarget) {
	st := initialStatus()
	cfg := in.cfg
	wl := func(w string) workloadIn {
		return workloadIn{name: w, spec: d.specs[w], obs: orAbsent(obs, w), rs: v.reports[w]}
	}
	unreported := slices.ContainsFunc(v.workloads, func(w string) bool { return d.specs[w] != nil && v.reports[w] == nil })

	var printers []config.Printer
	var scanners []config.Scanner
	if cfg != nil {
		printers, scanners = cfg.Printers, cfg.Scanners
	}
	pEnd := make([]endpoint, len(printers))
	sEnd := make([]endpoint, len(scanners))
	pAbsent := make([]bool, len(printers))
	sAbsent := make([]bool, len(scanners))
	for i, p := range printers {
		w := d.workloadW[p.ID]
		if s := d.specs[w]; s != nil && p.Enabled {
			if j := slices.IndexFunc(s.Printers, func(ps device.PrinterSpec) bool { return ps.ID == p.ID }); j >= 0 {
				pEnd[i] = printerEndpoint(c.cat, c.backendName, wl(w), p, s.Printers[j], d.placed[p.ID].Device)
				pAbsent[i] = absentKey(s.Printers[j].USB, v.reports[w])
			}
		}
	}
	for i, sc := range scanners {
		w := d.workloadW[sc.ID]
		if s := d.specs[w]; s != nil && sc.Enabled {
			if j := slices.IndexFunc(s.Scanners, func(ss device.ScannerSpec) bool { return ss.ID == sc.ID }); j >= 0 {
				sAbsent[i] = absentKey(s.Scanners[j].USB, v.reports[w])
				var usb []device.USBDevice
				for _, ss := range s.Scanners {
					if ss.Family == "airsane" && ss.USB != (device.USBKey{}) {
						usb = append(usb, d.placed[ss.ID].Device)
					}
				}
				sEnd[i] = scannerEndpoint(c.cat, c.backendName, wl(w), sc, s.Scanners[j], d.placed[sc.ID].Device, usb)
			}
		}
	}

	// Query every endpoint at once; the caps cache is read here and written after.
	pq := make([]printerQuery, len(printers))
	sq := make([]scannerQuery, len(scanners))
	var wg sync.WaitGroup
	for i := range printers {
		if uri := pEnd[i].uri; uri != "" {
			wg.Go(func() {
				qctx, cancel := context.WithTimeout(ctx, queryTimeout)
				defer cancel()
				pq[i].attrs, pq[i].err = c.printer(uri).Attributes(qctx)
			})
		}
	}
	for i := range scanners {
		u := sEnd[i].uri
		if u == "" {
			continue
		}
		cached, ok := c.caps[u]
		if ok && in.capsReset[d.workloadW[scanners[i].ID]] {
			cached.fails = 0
			c.caps[u] = cached
		}
		// A failed ask is retried after retryDelay(fails), but never while
		// the device is unplugged: its return resets fails (capsReset).
		ask := !ok || !cached.ok && !sAbsent[i] && !in.wake.within(in.now, cached.at, retryDelay(cached.fails))
		wg.Go(func() {
			qctx, cancel := context.WithTimeout(ctx, queryTimeout)
			defer cancel()
			obsv := c.scanner(u)
			state, err := obsv.Status(qctx)
			sq[i].state, sq[i].ok = string(state), err == nil
			if ask {
				sq[i].asked = true
				sq[i].caps, sq[i].capsErr = obsv.Capabilities(qctx)
			}
		})
	}
	wg.Wait()
	inUse := map[string]bool{}
	for i := range scanners {
		u := sEnd[i].uri
		if u == "" {
			continue
		}
		inUse[u] = true
		if sq[i].asked {
			fails := 0
			if sq[i].capsErr != nil {
				fails = c.caps[u].fails + 1
				if !sAbsent[i] {
					in.wake.at(in.now.Add(retryDelay(fails)), in.now)
				}
			}
			c.caps[u] = scanCaps{caps: scannerCaps(sq[i].caps), ok: sq[i].capsErr == nil, at: in.now, fails: fails}
		}
	}
	for u := range c.caps {
		if !inUse[u] {
			delete(c.caps, u)
		}
	}

	phase := func(p *phaseIn, w string) {
		p.w, p.wait, p.unreported, p.now, p.wake, p.obsErr = wl(w), d.wait, unreported, in.now, in.wake, obsErr
		p.applyErr, p.leftOut = applyErr(w), d.leftOut[w]
	}
	for i, p := range printers {
		sp := offlinePrinter(p)
		w := d.workloadW[p.ID]
		pl, placed := d.placed[p.ID]
		dec, decided := d.printers[p.ID]
		if decided {
			sp.Decision = &dec
		}
		if placed && !p.Simulated() {
			dev := pl.Device
			sp.Device = &dev
		}
		fam, _ := c.cat.Printer(dec.Family)
		var q *device.QueueResult
		if fam.App != "" {
			q = queueResult(wl(w).report(), p.ID)
		}
		e := pEnd[i]
		sp.URI, sp.Connect.DNSSDName = e.uri, e.dnssd
		if e.uri != "" {
			sp.Connect.IPPURIs = []string{e.uri}
		}
		var caps *device.PrinterCaps
		if e.uri != "" && pq[i].err == nil {
			a := pq[i].attrs
			sp.Available, sp.State = true, a.State
			sp.Reasons, sp.Formats, sp.ConvertedFormats = nonNil(a.Reasons), nonNil(a.Formats), printing.Conversions(a.Formats)
			sp.Media, sp.Markers, sp.Capabilities = nonNil(a.Media), nonNil(a.Markers), printerCaps(a.Caps)
			caps = &sp.Capabilities
		}
		if p.Enabled {
			sp.Warnings = printerWarnings(p, caps, q)
		}
		pi := phaseIn{
			enabled: p.Enabled, bound: placed || p.Simulated(), inSpec: inSpec(d.specs[w], p.ID),
			noDriver: decided && dec.Family == "" && !d.noID[p.ID], pappl: fam.App != "", queue: q, available: sp.Available,
			waitMessage: msgWaitPrinter, stale: stale(in.loss[p.ID]),
		}
		if !p.Simulated() {
			pi.usbErr = c.usbUnavailable
		}
		if decided && placed {
			pi.degraded = printerDegraded(c.cat, dec, p, cfg.Licenses.AcceptedEULAs, wl(w).report())
		}
		phase(&pi, w)
		sp.Phase, sp.Message = entryPhase(pi)
		st.Printers = append(st.Printers, sp)
	}
	for i, sc := range scanners {
		ss := offlineScanner(sc)
		w := d.workloadW[sc.ID]
		pl, placed := d.placed[sc.ID]
		if dec, ok := d.scanners[sc.ID]; ok {
			ss.Decision = &dec
		}
		if placed && !sc.Simulated() {
			dev := pl.Device
			ss.Device = &dev
		}
		e := sEnd[i]
		ss.URL, ss.Connect.DNSSDName = e.uri, e.dnssd
		if e.uri != "" {
			ss.Connect.ESCLURLs = []string{e.uri}
			if sq[i].ok {
				ss.Available, ss.State = true, sq[i].state
			}
			if cached := c.caps[e.uri]; cached.ok {
				ss.Capabilities = cached.caps
				if sc.Enabled {
					ss.Warnings = fitScan(sc.Defaults, cached.caps)
				}
			}
		}
		pi := phaseIn{
			enabled: sc.Enabled, bound: placed || sc.Simulated(), inSpec: inSpec(d.specs[w], sc.ID),
			available: ss.Available, waitMessage: msgWaitScanner, stale: stale(in.loss[sc.ID]),
		}
		if !sc.Simulated() {
			pi.usbErr = c.usbUnavailable
		}
		phase(&pi, w)
		ss.Phase, ss.Message = entryPhase(pi)
		st.Scanners = append(st.Scanners, ss)
	}

	names := slices.Clone(v.workloads)
	for w := range obs {
		if !slices.Contains(names, w) && obs[w].Phase != device.PhaseAbsent {
			names = append(names, w)
		}
	}
	slices.Sort(names)
	wlErr := ""
	var probeMsg string
	probe := false
	for _, n := range names {
		w := wl(n)
		sw := apitypes.StatusWorkload{
			Node: n, Phase: w.obs.Phase, Mode: apitypes.ModeNone,
			Advertised: advertised(w.report()), Blobs: []device.BlobStatus{},
		}
		if w.spec != nil {
			sw.Mode = w.spec.Mode
		}
		if w.rs != nil {
			sw.Blobs = nonNil(slices.Clone(w.rs.r.Blobs))
		}
		var isErr bool
		sw.Message, isErr = workloadMessage(w, obsErr, applyErr(n), in.now)
		if isErr && wlErr == "" {
			wlErr = sw.Message
		}
		if w.spec != nil && w.spec.Mode == device.ModeProbe && !probe {
			probe, probeMsg = true, sw.Message
		}
		st.Workloads = append(st.Workloads, sw)
	}
	st.Devices = detect(c.cat, cfg, v, c.usbAvailable)
	st.Phase, st.Message = overall(cfg, st, wlErr, probe, probeMsg)

	var want []watchTarget
	for i := range printers {
		if u := pEnd[i].uri; u != "" {
			want = append(want, watchTarget{uri: u, absent: pAbsent[i], pull: pEnd[i].ippUSB, seed: printerResult(pq[i].attrs, pq[i].err)})
		}
	}
	for i := range scanners {
		if u := sEnd[i].uri; u != "" {
			want = append(want, watchTarget{uri: u, scanner: true, absent: sAbsent[i], seed: scannerResult(sq[i].state, sq[i].ok)})
		}
	}
	return st, want
}

// overall is A10's Status phase: the first matching row wins.
func overall(cfg *config.Config, st apitypes.Status, workloadErr string, probe bool, probeMsg string) (string, string) {
	type entry struct{ name, phase, message string }
	var entries []entry
	for _, p := range st.Printers {
		if p.Enabled {
			entries = append(entries, entry{p.Name, p.Phase, p.Message})
		}
	}
	for _, s := range st.Scanners {
		if s.Enabled {
			entries = append(entries, entry{s.Name, s.Phase, s.Message})
		}
	}
	first := func(phases ...string) (entry, bool) {
		i := slices.IndexFunc(entries, func(e entry) bool { return slices.Contains(phases, e.phase) })
		if i < 0 {
			return entry{}, false
		}
		return entries[i], true
	}
	if e, ok := first(apitypes.PhaseError); ok {
		return apitypes.PhaseError, e.name + ": " + e.message
	}
	switch {
	case workloadErr != "":
		return apitypes.PhaseError, workloadErr
	case cfg == nil && probe:
		return apitypes.PhaseProbing, probeMsg
	case cfg == nil:
		return apitypes.PhaseNone, ""
	case len(entries) == 0:
		return apitypes.PhaseNone, msgNothingInUse
	}
	if e, ok := first(apitypes.PhaseDegraded); ok {
		return apitypes.PhaseDegraded, e.name + ": " + e.message
	}
	if e, ok := first(apitypes.PhaseProbing, apitypes.PhaseDeploying); ok {
		return apitypes.PhaseDeploying, e.name + ": " + e.message
	}
	return apitypes.PhaseReady, ""
}

func orAbsent(obs map[string]device.Observation, w string) device.Observation {
	if o, ok := obs[w]; ok {
		if o.Phase == "" {
			o.Phase = device.PhaseAbsent
		}
		return o
	}
	return device.Observation{Workload: w, Phase: device.PhaseAbsent}
}

func inSpec(s *device.Spec, id string) bool {
	return s != nil && (slices.ContainsFunc(s.Printers, func(p device.PrinterSpec) bool { return p.ID == id }) ||
		slices.ContainsFunc(s.Scanners, func(p device.ScannerSpec) bool { return p.ID == id }))
}

// stale reports whether a node-change replug key held after its restart.
func stale(st lossState) bool {
	k := lossKind(st.key)
	return st.key != "" && st.key == st.restartedKey && (k == "moved" || k == "unallocated")
}

// printerDegraded is a printer's first degraded reason: its driverName not
// listed, a required EULA not accepted, or a failed or disabled blob reported
// by its workload.
func printerDegraded(cat *catalog.Catalog, dec device.PrinterDecision, p config.Printer, acc []config.EULARef, r *device.AgentReport) string {
	if p.DriverName != "" && dec.Driver != p.DriverName {
		return fmt.Sprintf(msgDriverNotListed, p.DriverName)
	}
	for _, id := range dec.RequiredEULAs {
		if !accepted(cat, id, acc) {
			return "Accept EULA " + eulaTitle(cat, id) + " in Settings"
		}
	}
	if r == nil {
		return ""
	}
	for _, b := range r.Blobs {
		if (b.State == device.BlobFailed || b.State == device.BlobDisabled) &&
			(slices.Contains(dec.RequiredEULAs, blobEULA(cat, b.ID)) || slices.Contains(dec.OptionalEULAs, blobEULA(cat, b.ID))) {
			return eulaTitle(cat, blobEULA(cat, b.ID)) + ": " + b.Message
		}
	}
	return ""
}

func blobEULA(cat *catalog.Catalog, id string) string {
	if b, ok := cat.Blob(id); ok {
		return b.EULA
	}
	return id
}

// detect is Status.devices and Setup.devices: every device of the view with
// its roles, entries, ignored roles, suggested entries and decisions.
func detect(cat *catalog.Catalog, cfg *config.Config, v view, usbAvailable bool) []apitypes.DetectedDevice {
	out := []apitypes.DetectedDevice{}
	s := newSuggester(cfg)
	for _, d := range cat.Detect(cfg, v.nodes) {
		dd := apitypes.DetectedDevice{
			Device: d.Device, Node: d.Node, CanPrint: d.CanPrint, CanScan: d.CanScan,
			PrinterID: d.PrinterID, ScannerID: d.ScannerID, Ignored: d.Ignored,
		}
		simulated := v.simulated[d.Node] && !usbAvailable
		if d.CanPrint {
			p := s.printer(d.Device, simulated)
			dd.SuggestedPrinter = &p
		}
		if d.CanScan {
			sc := s.scanner(d.Device, simulated)
			dd.SuggestedScanner = &sc
		}
		dev := d.Device
		if p, ok := entry(cfg, d.PrinterID, dd.SuggestedPrinter); ok {
			dec := cat.DecidePrinter(&dev, p)
			dd.PrinterDecision = &dec
		}
		if sc, ok := scannerEntry(cfg, d.ScannerID, dd.SuggestedScanner); ok {
			dec := cat.DecideScanner(&dev, sc)
			dd.ScannerDecision = &dec
		}
		out = append(out, dd)
	}
	return out
}

// entry is the configured printer id, else the suggestion.
func entry(cfg *config.Config, id string, suggested *config.Printer) (config.Printer, bool) {
	if cfg != nil && id != "" {
		if i := slices.IndexFunc(cfg.Printers, func(p config.Printer) bool { return p.ID == id }); i >= 0 {
			return cfg.Printers[i], true
		}
	}
	if suggested != nil {
		return *suggested, true
	}
	return config.Printer{}, false
}

// scannerEntry is the configured scanner id, else the suggestion.
func scannerEntry(cfg *config.Config, id string, suggested *config.Scanner) (config.Scanner, bool) {
	if cfg != nil && id != "" {
		if i := slices.IndexFunc(cfg.Scanners, func(s config.Scanner) bool { return s.ID == id }); i >= 0 {
			return cfg.Scanners[i], true
		}
	}
	if suggested != nil {
		return *suggested, true
	}
	return config.Scanner{}, false
}

// printerDecisions are every printer decision Setup and EULA staging look at:
// each detected device's, and each enabled configured printer's on its
// device. Callers hold mu.
func (c *Controller) printerDecisions(cfg *config.Config, v view) []device.PrinterDecision {
	var out []device.PrinterDecision
	for _, d := range detect(c.cat, cfg, v, c.usbAvailable) {
		if d.PrinterDecision != nil {
			out = append(out, *d.PrinterDecision)
		}
	}
	if cfg != nil {
		for _, p := range cfg.Printers {
			if pl, ok := c.placed[p.ID]; ok && p.Enabled {
				dev := pl.Device
				out = append(out, c.cat.DecidePrinter(&dev, p))
			}
		}
	}
	return out
}

// acceptedRef returns the config entry accepting EULA id at the catalog's
// version, matched on id and version only.
func acceptedRef(cat *catalog.Catalog, id string, acc []config.EULARef) (config.EULARef, bool) {
	e, ok := cat.EULA(id)
	if !ok {
		return config.EULARef{}, false
	}
	i := slices.IndexFunc(acc, func(a config.EULARef) bool { return a.ID == id && a.Version == e.Version })
	if i < 0 {
		return config.EULARef{}, false
	}
	return acc[i], true
}

// accepted reports whether EULA id is accepted at the catalog's version.
func accepted(cat *catalog.Catalog, id string, acc []config.EULARef) bool {
	_, ok := acceptedRef(cat, id, acc)
	return ok
}

// acceptedEULAs is Spec.AcceptedEULAs: the accepted entries, with their text
// sha256, of the EULAs of refs, sorted by id.
func acceptedEULAs(refs []device.BlobRef, acc []config.EULARef, cat *catalog.Catalog) []device.EULARef {
	out := []device.EULARef{}
	for _, r := range refs {
		b, ok := cat.Blob(r.ID)
		if !ok || slices.ContainsFunc(out, func(e device.EULARef) bool { return e.ID == b.EULA }) {
			continue
		}
		if a, ok := acceptedRef(cat, b.EULA, acc); ok {
			out = append(out, device.EULARef{ID: a.ID, Version: a.Version, TextSHA256: a.TextSHA256})
		}
	}
	slices.SortFunc(out, func(a, b device.EULARef) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// eulaIDs lists the catalog EULAs the Setup DTO shows, without duplicates:
// the decisions' required and optional EULAs, then the accepted ones.
func eulaIDs(cat *catalog.Catalog, decs []device.PrinterDecision, acc []config.EULARef) []string {
	var ids []string
	for _, d := range decs {
		ids = append(append(ids, d.RequiredEULAs...), d.OptionalEULAs...)
	}
	for _, a := range acc {
		ids = append(ids, a.ID)
	}
	out := []string{}
	for _, id := range ids {
		if _, ok := cat.EULA(id); ok && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// stageEULAs is DesiredState.StageEULAs: every EULA of eulaIDs at its
// catalog version, sorted by id so an unchanged list compares equal.
// Accepted EULAs stay in, as texts live only in memory and Settings needs
// them after a control-plane restart.
func stageEULAs(cat *catalog.Catalog, decs []device.PrinterDecision, acc []config.EULARef) []device.EULARef {
	out := []device.EULARef{}
	for _, id := range eulaIDs(cat, decs, acc) {
		e, _ := cat.EULA(id)
		out = append(out, device.EULARef{ID: id, Version: e.Version})
	}
	slices.SortFunc(out, func(a, b device.EULARef) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// eulaText is the stored agent text of EULA e: pending until a report carried
// it at the catalog version.
func eulaText(e catalog.EULAInfo, texts map[string]device.EULAText) device.EULAText {
	if t, ok := texts[e.ID]; ok && t.Version == e.Version {
		return t
	}
	return device.EULAText{ID: e.ID, Version: e.Version, State: device.EULAPending}
}

// eulaDTO is the body of GET /eulas/{id}.
func eulaDTO(e catalog.EULAInfo, texts map[string]device.EULAText) apitypes.EULA {
	t := eulaText(e, texts)
	out := apitypes.EULA{
		ID: e.ID, Version: e.Version, Title: e.Title, Vendor: e.Vendor, Summary: e.Summary,
		State: t.State, Message: t.Message,
	}
	if t.State == device.EULAAvailable {
		out.TextSHA256, out.Text = &t.TextSHA256, &t.Text
	}
	return out
}

// blobRefs lists, in catalog order, the blobs whose EULA one of the decisions
// needs or offers and the config accepts. A blob is optional when no
// decision requires its EULA.
func blobRefs(decs []device.PrinterDecision, acc []config.EULARef, cat *catalog.Catalog) []device.BlobRef {
	refs := []device.BlobRef{}
	for _, b := range cat.Blobs {
		required := slices.ContainsFunc(decs, func(d device.PrinterDecision) bool { return slices.Contains(d.RequiredEULAs, b.EULA) })
		optional := slices.ContainsFunc(decs, func(d device.PrinterDecision) bool { return slices.Contains(d.OptionalEULAs, b.EULA) })
		if !required && !optional || !accepted(cat, b.EULA, acc) {
			continue
		}
		e, _ := cat.EULA(b.EULA)
		refs = append(refs, device.BlobRef{ID: b.ID, Version: e.Version, Optional: !required})
	}
	return refs
}

func eulaTitle(cat *catalog.Catalog, id string) string {
	if e, ok := cat.EULA(id); ok && e.Title != "" {
		return e.Title
	}
	return id
}
