// Package controller turns the config, the agent's reports and the backend's
// observations into the desired device workload (device.Spec), applies it, and
// aggregates everything into the API's Status and Setup DTOs.
//
// Concurrency: the loop is one goroutine. It holds mu only to read or write
// its own fields, never across backend.Apply, Observe, RestartPod or the
// printer and scanner queries. AgentReport never calls the backend.
package controller

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/ThePhaseless/printscan/internal/apitypes"
	"github.com/ThePhaseless/printscan/internal/catalog"
	"github.com/ThePhaseless/printscan/internal/config"
	"github.com/ThePhaseless/printscan/internal/device"
	"github.com/ThePhaseless/printscan/internal/printing"
	"github.com/ThePhaseless/printscan/internal/scanning"
)

const (
	resync       = 30 * time.Second // resync, apply retry, agent gap, start-up grace and device-loss delay
	pendingWait  = 5 * time.Minute  // how long a pending or running pod is waited for before the first report
	queryTimeout = 5 * time.Second
	applyTimeout = time.Minute
)

// PrinterObserver reads a printer's state (printing.Client implements it).
type PrinterObserver interface {
	Attributes(ctx context.Context) (printing.Attributes, error)
}

// ScannerObserver reads a scanner's state (scanning.Scanner implements it).
type ScannerObserver interface {
	Status(ctx context.Context) (scanning.State, error)
}

// Options configure New.
type Options struct {
	Backend        device.Backend
	BackendName    string           // BackendK8s or BackendSim
	Catalog        *catalog.Catalog // nil: catalog.Default()
	USBAvailable   bool             // k8s: --usb-resource set; sim: --sim-device != none
	USBUnavailable string           // k8s without --usb-resource: why mode usb cannot run; else ""
	Printer        func(uri string) PrinterObserver
	Scanner        func(url string) ScannerObserver
	Publish        func(apitypes.Status) // optional; called (without locks) when Status changes
	Now            func() time.Time
}

// Controller is the control loop. Create it with New and run Run in a goroutine.
type Controller struct {
	backend        device.Backend
	backendName    string
	cat            *catalog.Catalog
	usbAvailable   bool
	usbUnavailable string
	printer        func(string) PrinterObserver
	scanner        func(string) ScannerObserver
	publish        func(apitypes.Status)
	now            func() time.Time
	started        time.Time
	wake           chan struct{}

	// Touched only by the loop goroutine.
	applied     bool // a successful Apply of appliedHash happened
	appliedHash string
	failed      bool // the last Apply (of failHash, at failAt) failed with failMsg
	failHash    string
	failAt      time.Time
	failMsg     string

	mu          sync.Mutex
	cfgRead     bool // a Config* input has arrived; until then the spec is left alone
	cfgState    string
	cfg         *config.Config // applied config
	lastGood    *config.Config // last-known-good while the file is invalid
	etag        string
	appliedEtag string
	cfgErrors   []config.FieldError
	changedAt   time.Time
	report      *device.AgentReport        // latest report since start
	eulaTexts   map[string]device.EULAText // latest agent EULA text by id; in memory only
	reportAt    time.Time
	inventory   *device.Inventory
	absentSince time.Time // first DevicePresent=false report of the current loss
	restarted   bool      // RestartPod already called for the current loss
	spec        *device.Spec
	status      apitypes.Status // device, printer, scanner and connect parts from the loop
	published   apitypes.Status // the last Status sent to Publish and subscribers
	reconcile   []apitypes.ReconcileItem
	subs        map[chan apitypes.Status]struct{}
}

// New creates a controller. It calls nothing on the backend.
func New(o Options) *Controller {
	c := &Controller{
		backend: o.Backend, backendName: o.BackendName, cat: o.Catalog,
		usbAvailable: o.USBAvailable, usbUnavailable: o.USBUnavailable,
		printer: o.Printer, scanner: o.Scanner, publish: o.Publish, now: o.Now,
		wake: make(chan struct{}, 1), cfgState: apitypes.ConfigMissing, cfgErrors: []config.FieldError{},
		status: initialStatus(), published: initialStatus(), reconcile: []apitypes.ReconcileItem{}, subs: map[chan apitypes.Status]struct{}{},
	}
	if c.cat == nil {
		c.cat = catalog.Default()
	}
	if c.printer == nil {
		c.printer = func(uri string) PrinterObserver { return printing.New(uri) }
	}
	if c.scanner == nil {
		c.scanner = func(url string) ScannerObserver {
			s, err := scanning.NewMFP(url)
			if err != nil {
				return errScanner{err}
			}
			return s
		}
	}
	if c.now == nil {
		c.now = time.Now
	}
	c.started = c.now()
	return c
}

type errScanner struct{ err error }

func (e errScanner) Status(context.Context) (scanning.State, error) {
	return scanning.StateUnknown, e.err
}

// Run runs the loop until ctx is done: at once, after every input and every 30 s.
func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(resync)
	defer t.Stop()
	for {
		c.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-t.C:
		}
	}
}

func (c *Controller) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// ConfigApplied records a valid config file.
func (c *Controller) ConfigApplied(cfg *config.Config, etag string) {
	c.mu.Lock()
	c.cfgRead, c.cfgState, c.cfg, c.lastGood = true, apitypes.ConfigApplied, cfg, nil
	c.etag, c.appliedEtag, c.cfgErrors, c.changedAt = etag, etag, []config.FieldError{}, c.now()
	c.mu.Unlock()
	c.poke()
}

// ConfigInvalid records an invalid config file and the last-known-good config (nil if none).
func (c *Controller) ConfigInvalid(err error, etag string, lastGood *config.Config) {
	errs := []config.FieldError{{Message: err.Error()}}
	var el *config.ErrorList
	if errors.As(err, &el) {
		errs = append([]config.FieldError{}, el.Errors...)
	}
	c.mu.Lock()
	c.cfgRead, c.cfgState, c.cfg, c.lastGood = true, apitypes.ConfigInvalid, nil, lastGood
	c.etag, c.cfgErrors, c.changedAt = etag, errs, c.now()
	c.mu.Unlock()
	c.poke()
}

// ConfigMissing records that the config file does not exist.
func (c *Controller) ConfigMissing() {
	c.mu.Lock()
	c.cfgRead, c.cfgState, c.cfg, c.lastGood = true, apitypes.ConfigMissing, nil, nil
	c.etag, c.cfgErrors, c.changedAt = "", []config.FieldError{}, c.now()
	c.mu.Unlock()
	c.poke()
}

// effective is the config the spec is built from: applied, else last-known-good. Callers hold mu.
func (c *Controller) effective() *config.Config {
	if c.cfg != nil {
		return c.cfg
	}
	return c.lastGood
}

// Config returns the effective config: the applied config, else the
// last-known-good while the file is invalid, else config.Default("Printer").
func (c *Controller) Config() config.Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cfg := c.effective(); cfg != nil {
		return *cfg
	}
	return config.Default(defaultName)
}

// AgentReport handles an agent report (a device.ReportFunc). It never calls the backend.
func (c *Controller) AgentReport(_ context.Context, r device.AgentReport) (device.DesiredState, error) {
	c.mu.Lock()
	now := c.now()
	c.report, c.reportAt = &r, now
	inv := r.Inventory
	c.inventory = &inv
	c.storeTexts(r.EULAs)
	if r.DevicePresent {
		c.absentSince, c.restarted = time.Time{}, false
	} else if c.absentSince.IsZero() {
		c.absentSince = now
	}
	cfg := c.effective()
	sel, acc := config.Default(defaultName).Device, []config.EULARef{}
	if cfg != nil {
		sel, acc = cfg.Device, cfg.Licenses.AcceptedEULAs
	}
	ds := device.DesiredState{
		Hash: device.SpecHash(c.spec), Queue: c.queueFor(c.spec, cfg), Browse: slices.Clone(device.BrowseTypes),
		StageEULAs: stageEULAs(c.cat, decisionFor(c.cat, sel, &inv), acc),
	}
	c.mu.Unlock()
	c.poke()
	return ds, nil
}

// storeTexts keeps the latest text per EULA id. A pending report does not
// replace a known text of the same version: a rolled pod's agent reports
// pending until it has re-verified its staged archive. Callers hold mu.
func (c *Controller) storeTexts(texts []device.EULAText) {
	if c.eulaTexts == nil {
		c.eulaTexts = map[string]device.EULAText{}
	}
	for _, t := range texts {
		if _, known := c.cat.EULA(t.ID); !known {
			continue // only catalog EULAs are ever shown, so the map stays bounded
		}
		if old, ok := c.eulaTexts[t.ID]; ok && t.State == device.EULAPending && old.Version == t.Version {
			continue
		}
		c.eulaTexts[t.ID] = t
	}
}

// EULA returns the licence text DTO of catalog EULA id, false for an id not
// in the catalog. Its state is pending until an agent report carries it.
func (c *Controller) EULA(id string) (apitypes.EULA, bool) {
	e, ok := c.cat.EULA(id)
	if !ok {
		return apitypes.EULA{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return eulaDTO(e, c.eulaTexts), true
}

// queueFor is the desired PAPPL queue: nil unless the spec's printer family is a PAPPL app.
func (c *Controller) queueFor(spec *device.Spec, cfg *config.Config) *device.QueueSpec {
	if spec == nil || cfg == nil {
		return nil
	}
	fam, ok := c.cat.Printer(spec.Printer)
	if !ok || fam.App == "" {
		return nil
	}
	d := cfg.Print.Defaults
	return &device.QueueSpec{
		Name: cfg.Name, Location: cfg.Location, Driver: config.Auto, Schemes: slices.Clone(fam.DeviceSchemes),
		Media: d.Media, Sides: d.Sides, ColorMode: d.ColorMode, Quality: d.Quality,
	}
}

// SetReconcileItem sets one reconcile[] item (data-dir, config, ...).
func (c *Controller) SetReconcileItem(item string, ok bool, message string) {
	c.mu.Lock()
	changed := c.setItem(item, ok, message)
	c.mu.Unlock()
	if changed {
		c.poke()
	}
}

// setItem updates or appends an item; at changes only with ok or message. Callers hold mu.
func (c *Controller) setItem(item string, ok bool, message string) bool {
	i := slices.IndexFunc(c.reconcile, func(r apitypes.ReconcileItem) bool { return r.Item == item })
	if i >= 0 && c.reconcile[i].OK == ok && c.reconcile[i].Message == message {
		return false
	}
	r := apitypes.ReconcileItem{Item: item, OK: ok, Message: message, At: c.now()}
	if i < 0 {
		c.reconcile = append(c.reconcile, r)
	} else {
		c.reconcile = slices.Clone(c.reconcile) // Current() callers may hold the old slice
		c.reconcile[i] = r
	}
	return true
}

// Current returns the aggregated Status.
func (c *Controller) Current() apitypes.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current()
}

func (c *Controller) current() apitypes.Status {
	s := c.status
	s.Config = apitypes.StatusConfig{State: c.cfgState, Etag: c.etag, AppliedEtag: c.appliedEtag, Errors: c.cfgErrors, ChangedAt: c.changedAt}
	s.Reconcile = c.reconcile
	return s
}

// Subscribe returns a channel that receives the latest Status whenever it
// changes (older undelivered values are dropped), and a cancel func.
func (c *Controller) Subscribe() (<-chan apitypes.Status, func()) {
	ch := make(chan apitypes.Status, 1)
	c.mu.Lock()
	c.subs[ch] = struct{}{}
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		delete(c.subs, ch)
		c.mu.Unlock()
	}
}

// Setup returns the wizard data.
func (c *Controller) Setup() apitypes.Setup {
	c.mu.Lock()
	in := setupInput{
		backend: c.backendName, usbAvailable: c.usbAvailable,
		missing: c.cfgState == apitypes.ConfigMissing, cfg: c.effective(), inventory: c.inventory,
		texts: maps.Clone(c.eulaTexts),
	}
	c.mu.Unlock()
	return setup(c.cat, in)
}

// snapshot is the loop's copy of its inputs.
type snapshot struct {
	now         time.Time
	cfgRead     bool
	cfgState    string
	cfg         *config.Config
	inventory   *device.Inventory
	report      *device.AgentReport
	reportAt    time.Time
	absentSince time.Time
	restarted   bool
	spec        *device.Spec
}

// desired is the outcome of the spec rules.
type desired struct {
	spec           *device.Spec
	apply          bool // false: wait, or leave whatever runs
	phase, message string
}

func (c *Controller) probe(message string) desired {
	s := &device.Spec{Mode: device.ModeProbe}
	s.Hash = device.SpecHash(s)
	return desired{spec: s, apply: true, phase: apitypes.PhaseProbing, message: message}
}

func (c *Controller) deviceSpec(mode string, dec device.Decision, cfg *config.Config) desired {
	s := &device.Spec{
		Mode: mode, Printer: dec.Printer, Scanner: dec.Scanner,
		SaneBackends: slices.Clone(dec.SaneBackends), Blobs: blobRefs(dec, cfg.Licenses.AcceptedEULAs, c.cat),
	}
	s.AcceptedEULAs = acceptedEULAs(s.Blobs, cfg.Licenses.AcceptedEULAs, c.cat)
	if mode == device.ModeSimulated {
		s.Name, s.Location = cfg.Name, cfg.Location
	}
	s.Hash = device.SpecHash(s)
	return desired{spec: s, apply: true}
}

// decide picks the device spec and phase from the config, inventory and pod state.
func (c *Controller) decide(in snapshot, obs device.Observation, obsErr error) desired {
	wait := desired{spec: in.spec, phase: apitypes.PhaseDeploying, message: msgWaitingAgent}
	cfg := in.cfg
	switch {
	case !in.cfgRead:
		return desired{spec: in.spec} // the file has not been read yet: it is not known to be missing
	case cfg == nil && in.cfgState == apitypes.ConfigInvalid:
		return desired{spec: in.spec} // no last-known-good: no Apply, leave whatever runs
	case cfg == nil && c.usbAvailable:
		if in.inventory == nil {
			return c.probe(msgWaitingAgent)
		}
		sel := config.Default(defaultName).Device
		if _, err := c.cat.Decide(*in.inventory, sel); err != nil {
			return c.probe(noPrinterMessage(err, sel))
		}
		return c.probe("")
	case cfg == nil:
		return desired{apply: true}
	case cfg.Device.Mode == config.ModeSimulated:
		dec, err := c.cat.Decide(device.Inventory{}, cfg.Device)
		if err != nil {
			return desired{spec: in.spec, phase: apitypes.PhaseError, message: err.Error()}
		}
		return c.deviceSpec(device.ModeSimulated, dec, cfg)
	case c.usbUnavailable != "":
		return desired{apply: true, phase: apitypes.PhaseError, message: c.usbUnavailable}
	case in.inventory == nil:
		since := in.now.Sub(c.started)
		switch {
		case obsErr != nil:
			return wait
		case obs.Phase == device.PhaseAbsent || obs.Phase == device.PhaseFailed:
			return c.probe(msgWaitingAgent)
		// A running pod's agent can take longer than resync to report after a
		// control-plane restart (its own backoff alone reaches 30 s), and probing
		// would kill a working device pod, so running pods get the pending wait too.
		case since < resync, (obs.Phase == device.PhasePending || obs.Phase == device.PhaseRunning) && since < pendingWait:
			return wait
		}
		return c.probe(msgWaitingAgent)
	}
	dec, err := c.cat.Decide(*in.inventory, cfg.Device)
	if errors.Is(err, catalog.ErrNoPrinter) {
		return c.probe(noPrinterMessage(err, cfg.Device))
	}
	if err != nil {
		return desired{spec: in.spec, phase: apitypes.PhaseError, message: err.Error()}
	}
	return c.deviceSpec(device.ModeUSB, dec, cfg)
}

// due is the Apply rule: the hash differs from the last successful Apply, or
// the last Apply failed (retried at the next resync).
func (c *Controller) due(hash string, now time.Time) bool {
	if c.failed && hash == c.failHash {
		return now.Sub(c.failAt) >= resync
	}
	return !c.applied || hash != c.appliedHash
}

// step is one loop iteration.
func (c *Controller) step(ctx context.Context) {
	obs, obsErr := c.backend.Observe(ctx)

	c.mu.Lock()
	in := snapshot{
		now: c.now(), cfgRead: c.cfgRead, cfgState: c.cfgState, cfg: c.effective(), inventory: c.inventory,
		report: c.report, reportAt: c.reportAt, absentSince: c.absentSince, restarted: c.restarted, spec: c.spec,
	}
	d := c.decide(in, obs, obsErr)
	c.spec = d.spec
	c.mu.Unlock()

	if hash := device.SpecHash(d.spec); d.apply && c.due(hash, in.now) {
		actx, cancel := context.WithTimeout(ctx, applyTimeout)
		err := c.backend.Apply(actx, d.spec)
		cancel()
		c.failed, c.failMsg = err != nil, ""
		if err != nil {
			c.applied, c.failHash, c.failAt, c.failMsg = false, hash, in.now, err.Error()
		} else {
			c.applied, c.appliedHash = true, hash
		}
		c.mu.Lock()
		if c.setItem("device", err == nil, c.failMsg) && err != nil {
			slog.Warn("device workload apply failed", "err", err)
		}
		c.mu.Unlock()
		obs, obsErr = c.backend.Observe(ctx)
	}
	applyErr := ""
	if c.failed && d.apply {
		applyErr = c.failMsg
	}

	if d.spec != nil && d.spec.Mode == device.ModeUSB && !in.absentSince.IsZero() && !in.restarted &&
		in.now.Sub(in.absentSince) >= resync {
		if r, ok := c.backend.(device.PodRestarter); ok {
			if err := r.RestartPod(ctx); err != nil {
				slog.Warn("device pod restart failed", "err", err)
			}
			c.mu.Lock()
			if c.absentSince.Equal(in.absentSince) {
				c.restarted = true
			}
			c.mu.Unlock()
		}
	}

	st := c.observe(ctx, in, d, obs, obsErr, applyErr)

	c.mu.Lock()
	c.status = st
	after := c.current()
	changed := !reflect.DeepEqual(c.published, after)
	if changed {
		c.published = after
	}
	var subs []chan apitypes.Status
	if changed {
		for ch := range c.subs {
			subs = append(subs, ch)
		}
		for _, ch := range subs {
			select {
			case <-ch:
			default:
			}
			ch <- after
		}
	}
	c.mu.Unlock()
	if changed && c.publish != nil {
		c.publish(after)
	}
}

// observe derives endpoints, queries the printer and scanner (no lock held)
// and builds the device, printer, scanner and connect parts of Status.
func (c *Controller) observe(ctx context.Context, in snapshot, d desired, obs device.Observation, obsErr error, applyErr string) apitypes.Status {
	st := initialStatus()
	spec, r := d.spec, in.report
	var fam catalog.Printer
	if spec != nil {
		fam, _ = c.cat.Printer(spec.Printer)
	}
	prefer := preferredName(fam, in.cfg)
	ipp := printerURI(c.backendName, fam, obs, r, prefer)
	escl := scannerURL(c.cat, c.backendName, spec, obs, r)

	st.Printer = offlinePrinter(ipp)
	if ipp != "" {
		qctx, cancel := context.WithTimeout(ctx, queryTimeout)
		a, err := c.printer(ipp).Attributes(qctx)
		cancel()
		if err == nil {
			st.Printer = apitypes.StatusPrinter{
				Available: true, URI: ipp, State: a.State,
				Reasons: nonNil(a.Reasons), Formats: nonNil(a.Formats), ConvertedFormats: printing.Conversions(a.Formats),
				Media: nonNil(a.Media), Markers: nonNil(a.Markers),
			}
		}
	}
	st.Scanner.URL = escl
	if escl != "" {
		qctx, cancel := context.WithTimeout(ctx, queryTimeout)
		state, err := c.scanner(escl).Status(qctx)
		cancel()
		if err == nil {
			st.Scanner.Available, st.Scanner.State = true, string(state)
		}
	}

	sel := config.Default(defaultName).Device
	if in.cfg != nil {
		sel = in.cfg.Device
	}
	dec := decisionFor(c.cat, sel, in.inventory)
	st.Device.Mode = modeOf(spec)
	st.Device.Decision = dec
	st.Device.Inventory = in.inventory
	st.Device.Advertised = advertised(r)
	if r != nil {
		st.Device.Blobs = nonNil(r.Blobs)
	}
	st.Connect = connect(r, fam, prefer, ipp, escl)

	var specDec *device.Decision
	if spec != nil && spec.Mode != device.ModeProbe {
		specDec = dec
	}
	degraded := degradedMessage(c.cat, specDec, in.cfg, st.Device.Blobs)
	st.Device.Phase, st.Device.Message = phase(in, d, obs, obsErr, applyErr, degraded, st)
	return st
}

// phase picks Status device.phase and message, highest priority first. Ready
// needs a reachable printer only: many decided scanners never advertise
// (print-only devices, HP scanners without the plug-in); scanner.available says so.
func phase(in snapshot, d desired, obs device.Observation, obsErr error, applyErr, degraded string, st apitypes.Status) (string, string) {
	spec, r := d.spec, in.report
	switch {
	case d.phase == apitypes.PhaseError:
		return d.phase, d.message
	case r != nil && r.Queue != nil && r.Queue.State == device.QueueError && spec != nil:
		return apitypes.PhaseError, r.Queue.Message
	case applyErr != "":
		return apitypes.PhaseError, applyErr
	case spec == nil && d.phase == "":
		return apitypes.PhaseNone, ""
	case obsErr != nil:
		return apitypes.PhaseError, obsErr.Error()
	case obs.Phase == device.PhaseFailed:
		return apitypes.PhaseError, orDefault(obs.Message, "Device pod failed")
	case obs.Unschedulable:
		return apitypes.PhaseProbing, msgUnschedulable
	case r != nil && spec != nil && in.now.Sub(in.reportAt) > resync:
		return apitypes.PhaseDegraded, msgNotReporting
	case d.phase != "":
		return d.phase, d.message
	case degraded != "":
		return apitypes.PhaseDegraded, degraded
	case spec.Mode == device.ModeProbe:
		return apitypes.PhaseProbing, ""
	case obs.Phase != device.PhaseRunning:
		return apitypes.PhaseDeploying, orDefault(obs.Message, msgStarting)
	case spec.Printer != "" && !st.Printer.Available:
		return apitypes.PhaseDeploying, msgWaitPrinter
	}
	return apitypes.PhaseReady, ""
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
