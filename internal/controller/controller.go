// Package controller turns the config, the agents' reports and the backend's
// observations into the desired device workloads (one device.Spec per node),
// applies them, and aggregates everything into the API's Status and Setup DTOs.
//
// The loop steps on events (config, changed agent reports, backend
// notifications, endpoint watchers, Refresh) and at the deadlines its time
// rules record (wakeAt); it has no periodic tick. The endpoint watchers
// re-query each printer and scanner every endpointPoll and wake the loop
// only when the answer differs from the last step's (watchers.go).
//
// Concurrency: the loop is one goroutine. It holds mu only to read or write
// its own fields, never across a backend call (USBNodes, Observe, Apply,
// SetUSBDevices, RestartPod) or a printer or scanner query. AgentReport never
// calls the backend, and holds a long-poll without holding mu.
package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/ippnotify"
	"github.com/ThePhaseless/pagewire/internal/printing"
	"github.com/ThePhaseless/pagewire/internal/scanning"
)

const (
	queryTimeout = 5 * time.Second
	applyTimeout = time.Minute
)

// PrinterObserver reads a printer's state (printing.Client implements it).
type PrinterObserver interface {
	Attributes(ctx context.Context) (printing.Attributes, error)
}

// ScannerObserver reads a scanner's state and capabilities (scanning.Scanner implements it).
type ScannerObserver interface {
	Status(ctx context.Context) (scanning.State, error)
	Capabilities(ctx context.Context) (scanning.Caps, error)
}

// Options configure New.
type Options struct {
	Backend        device.Backend
	BackendName    string           // BackendK8s, BackendDocker or BackendSim
	Catalog        *catalog.Catalog // nil: catalog.Default()
	USBAvailable   bool             // k8s: --usb-resource set; docker: always; sim: --sim-device != none
	USBUnavailable string           // k8s without --usb-resource: why usb entries cannot run; else ""
	Printer        func(uri string) PrinterObserver
	Scanner        func(url string) ScannerObserver
	Publish        func(apitypes.Status) // optional; called (without locks) when Status changes
	// PublishEULA is optional; called (without locks) when an agent report
	// changed a catalog EULA's state, version or text.
	PublishEULA func(apitypes.EULA)
	// AgentWait bounds an AgentReport hold (AgentReport.Wait); 0: device.AgentWait.
	AgentWait time.Duration
	Now       func() time.Time
}

// reportState is the latest report of one workload.
type reportState struct {
	r       device.AgentReport
	at      time.Time // when it arrived
	firstAt time.Time // the first report of r.PodUID
}

// lossState is one entry's replug restart key (A8): key is lossKey's result,
// since when it was first seen, restartedKey the key RestartPod was last called for.
type lossState struct {
	workload, key, restartedKey string
	since                       time.Time
}

// scanCaps is a cached ScannerObserver.Capabilities result.
type scanCaps struct {
	caps  apitypes.ScannerCaps
	ok    bool
	at    time.Time
	fails int // consecutive failed asks
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
	publishEULA    func(apitypes.EULA)
	agentWait      time.Duration
	now            func() time.Time
	started        time.Time
	wake           chan struct{} // a step is due
	rearm          chan struct{} // a heartbeat moved the agent gap: re-arm without a step
	pollEvery      time.Duration // endpointPoll; tests shorten it
	pollRetry      time.Duration // the endpoint watchers' first retry, 1 s; tests shorten it
	leaseWait      time.Duration // ippnotify.Lease; tests shorten it

	// Touched only by the loop goroutine.
	applied     bool              // a successful Apply of appliedHash happened
	appliedHash string            // combined hash (combinedHash) of the applied specs
	appliedBy   map[string]string // workload -> spec hash of the last successful Apply
	failed      bool              // the last Apply (of failHash, at failAt) failed: failMsg for every workload, or failW per workload
	failHash    string
	failAt      time.Time
	failMsg     string
	failCount   int                 // consecutive failed Applies of failHash
	failW       map[string]string   // workload -> its Apply error, from device.WorkloadErrors
	failLogged  string              // the last Apply error logged, so a failure that repeats on each retry logs once
	nodesFailed bool                // the nodes reconcile item is false
	nodesBlip   bool                // the node list failed transiently (logged once per streak)
	blipCount   int                 // consecutive steps with a node-list blip or an Observe error
	itemsFailed map[string]bool     // workload items last set false
	caps        map[string]scanCaps // scanner caps by eSCL URL
	next        time.Time           // the last step's earliest recorded deadline, zero none
	stepAt      time.Time           // the last step's now
	watchWant   []watchTarget       // the last step's endpoints, for syncWatchers
	watchers    map[string]*watcher // running endpoint watchers by URI

	mu          sync.Mutex
	cfgRead     bool // a Config* input has arrived; until then the specs are left alone
	cfgState    string
	cfg         *config.Config // applied config
	lastGood    *config.Config // last-known-good while the file is invalid
	etag        string
	appliedEtag string
	cfgErrors   []config.FieldError
	changedAt   time.Time
	reports     map[string]*reportState    // by workload
	reported    map[string]bool            // workloads that reported since start; never pruned
	idCache     map[device.USBKey]string   // IEEE-1284 device ID of every device a probe read one for
	loss        map[string]*lossState      // by entry id
	eulaTexts   map[string]device.EULAText // latest agent EULA text by id; in memory only
	workloads   []string                   // the last step's workload set
	alias       string                     // the node the legacy unpinned workload "" stands for (legacyAlias), "" none
	specs       map[string]*device.Spec    // desired spec by workload
	placed      map[string]catalog.Placed  // entry id -> its device (bound now or sticky), DeviceID filled
	detectAt    time.Time                  // the last Detect that restarted pods
	status      apitypes.Status            // the loop's parts of Status
	published   apitypes.Status            // the last Status sent to Publish and subscribers
	reconcile   []apitypes.ReconcileItem
	subs        map[chan apitypes.Status]struct{}
	desiredCh   chan struct{}   // closed and replaced whenever the desired states may have changed
	capsReset   map[string]bool // workloads whose Attached key set changed: their scanners' caps retry at once
}

// New creates a controller. It calls nothing on the backend.
func New(o Options) *Controller {
	c := &Controller{
		backend: o.Backend, backendName: o.BackendName, cat: o.Catalog,
		usbAvailable: o.USBAvailable, usbUnavailable: o.USBUnavailable,
		printer: o.Printer, scanner: o.Scanner, publish: o.Publish, publishEULA: o.PublishEULA, agentWait: o.AgentWait, now: o.Now,
		wake: make(chan struct{}, 1), rearm: make(chan struct{}, 1), pollEvery: endpointPoll, pollRetry: time.Second, leaseWait: ippnotify.Lease * time.Second,
		watchers: map[string]*watcher{}, desiredCh: make(chan struct{}), capsReset: map[string]bool{},
		cfgState: apitypes.ConfigMissing, cfgErrors: []config.FieldError{},
		appliedBy: map[string]string{}, caps: map[string]scanCaps{}, itemsFailed: map[string]bool{}, reported: map[string]bool{},
		reports: map[string]*reportState{}, idCache: map[device.USBKey]string{}, loss: map[string]*lossState{},
		specs: map[string]*device.Spec{}, placed: map[string]catalog.Placed{},
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
	if c.agentWait <= 0 {
		c.agentWait = device.AgentWait
	}
	c.started = c.now()
	return c
}

type errScanner struct{ err error }

func (e errScanner) Status(context.Context) (scanning.State, error) {
	return scanning.StateUnknown, e.err
}

func (e errScanner) Capabilities(context.Context) (scanning.Caps, error) {
	return scanning.Caps{}, e.err
}

// Run runs the loop until ctx is done. It steps at once, then after every
// input (config, a changed agent report, a backend notification, an endpoint
// watcher, Refresh) and at the earliest deadline the last step recorded or
// the live agent gap (deadline). It runs the backend's Notify when the
// backend is a device.Notifier, and the endpoint watchers (syncWatchers).
func (c *Controller) Run(ctx context.Context) {
	if n, ok := c.backend.(device.Notifier); ok {
		go n.Notify(ctx, c.poke, c.backendDegraded)
	}
	defer c.syncWatchers(ctx, nil) // cancel every watcher
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		c.step(ctx)
		c.syncWatchers(ctx, c.watchWant)
		for due := false; !due; {
			if d := c.deadline(); d.IsZero() {
				t.Stop()
			} else {
				t.Reset(d.Sub(c.now()))
			}
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
				due = true
			case <-c.rearm:
			case <-t.C:
				// A heartbeat may have moved the agent gap since the timer was set.
				d := c.deadline()
				due = !d.IsZero() && !d.After(c.now())
			}
		}
	}
}

// deadline is when the loop must step without an event: the last step's
// earliest recorded deadline or the live agent gap, zero for none.
func (c *Controller) deadline() time.Time { return earliest(c.next, c.gapDeadline()) }

// gapDeadline is the earliest instant a report not shown silent at the last
// step becomes silent. Computed live, so heartbeats move it without a step.
func (c *Controller) gapDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var w wakeAt
	for _, rs := range c.reports {
		w.at(rs.at.Add(agentGap), c.stepAt)
	}
	return w.t
}

func (c *Controller) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Refresh asks the loop for a step, for inputs it has no event for (a
// pagewire job or scan changing a printer's or scanner's state).
func (c *Controller) Refresh() { c.poke() }

func (c *Controller) signalRearm() {
	select {
	case c.rearm <- struct{}{}:
	default:
	}
}

// backendDegraded is the backend's Notify degraded callback: the
// backend-events reconcile item, failed with msg while it is not "".
func (c *Controller) backendDegraded(msg string) {
	c.SetReconcileItem("backend-events", msg == "", msg)
}

// desiredChanged wakes every held AgentReport to recompute its desired
// state. Callers hold mu.
func (c *Controller) desiredChanged() {
	close(c.desiredCh)
	c.desiredCh = make(chan struct{})
}

// ConfigApplied records a valid config file.
func (c *Controller) ConfigApplied(cfg *config.Config, etag string) {
	c.mu.Lock()
	c.cfgRead, c.cfgState, c.cfg, c.lastGood = true, apitypes.ConfigApplied, cfg, nil
	c.etag, c.appliedEtag, c.cfgErrors, c.changedAt = etag, etag, []config.FieldError{}, c.now()
	c.desiredChanged()
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
	c.desiredChanged()
	c.mu.Unlock()
	c.poke()
}

// ConfigMissing records that the config file does not exist.
func (c *Controller) ConfigMissing() {
	c.mu.Lock()
	c.cfgRead, c.cfgState, c.cfg, c.lastGood = true, apitypes.ConfigMissing, nil, nil
	c.etag, c.cfgErrors, c.changedAt = "", []config.FieldError{}, c.now()
	c.desiredChanged()
	c.mu.Unlock()
	c.poke()
}

// effective is the config the specs are built from: applied, else last-known-good. Callers hold mu.
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

// AgentReport handles an agent report (a device.ReportFunc): it stores the
// report, learns the probe's device IDs, updates the replug keys of the
// entries on the report's workload and returns that workload's desired state.
// A stale report (staleReport) is answered without being stored. Only a
// changed report wakes the loop; an identical heartbeat only moves the agent
// gap. With r.Wait and r.DesiredVersion equal to the current version, it
// holds the answer (without mu) until the desired state changes, AgentWait
// passes or ctx ends. It never calls the backend.
func (c *Controller) AgentReport(ctx context.Context, r device.AgentReport) (device.DesiredState, error) {
	c.mu.Lock()
	if r.Workload == "" && c.alias != "" {
		r.Workload = c.alias // the legacy agent of the unpinned workload (legacyAlias)
	}
	now := c.now()
	old := c.reports[r.Workload]
	if old != nil && staleReport(old.r, r) {
		ds := c.desiredFor(r.Workload)
		c.mu.Unlock()
		return ds, nil
	}
	first := now
	if old != nil && old.r.PodUID == r.PodUID {
		first = old.firstAt
	}
	changed := old == nil || silent(now, old.at) || !sameReport(old.r, r)
	if old != nil && !sameKeys(old.r.Attached, r.Attached) {
		c.capsReset[r.Workload] = true // a returning device's scanner is asked at once
	}
	c.reports[r.Workload] = &reportState{r: r, at: now, firstAt: first}
	c.reported[r.Workload] = true
	for _, d := range r.Inventory.Devices {
		if d.DeviceID != "" {
			c.idCache[d.Key()] = d.DeviceID
		}
	}
	eulas := c.storeTexts(r.EULAs)
	cfg := c.effective()
	set := c.workloads
	if len(set) == 0 {
		set = []string{r.Workload}
	}
	v := c.view(set, c.reports)
	if slices.Contains(set, r.Workload) && c.updateLoss(cfg, v, &r, now) {
		changed = true
	}
	ds := c.desiredState(r.Workload, cfg, v)
	ch := c.desiredCh
	c.mu.Unlock()
	if changed {
		c.poke()
	} else {
		c.signalRearm()
	}
	if c.publishEULA != nil {
		for _, e := range eulas {
			c.publishEULA(e)
		}
	}
	if !r.Wait || ds.Version != r.DesiredVersion {
		return ds, nil
	}
	return c.hold(ctx, r.Workload, r.DesiredVersion, ds, ch), nil
}

// hold waits for workload w's desired state to leave version: it recomputes
// it each time ch closes, and returns the latest one when it differs, after
// AgentWait, or when ctx ends.
func (c *Controller) hold(ctx context.Context, w, version string, ds device.DesiredState, ch <-chan struct{}) device.DesiredState {
	t := time.NewTimer(c.agentWait)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ds
		case <-t.C:
			return ds
		case <-ch:
		}
		c.mu.Lock()
		ds, ch = c.desiredFor(w), c.desiredCh
		c.mu.Unlock()
		if ds.Version != version {
			return ds
		}
	}
}

// desiredFor is workload w's desired state now. Callers hold mu.
func (c *Controller) desiredFor(w string) device.DesiredState {
	set := c.workloads
	if len(set) == 0 {
		set = []string{w}
	}
	return c.desiredState(w, c.effective(), c.view(set, c.reports))
}

// staleReport reports whether r is older than the stored report old: the
// same PodUID and non-empty Instance, and a lower non-zero Seq. A different
// Instance (a restarted agent) is never stale.
func staleReport(old, r device.AgentReport) bool {
	return r.Instance != "" && r.Seq != 0 && old.PodUID == r.PodUID && old.Instance == r.Instance && r.Seq < old.Seq
}

// sameReport reports whether a and b say the same, apart from the fields
// that change on every heartbeat.
func sameReport(a, b device.AgentReport) bool {
	a.Seq, a.Wait, a.DesiredVersion = 0, false, ""
	b.Seq, b.Wait, b.DesiredVersion = 0, false, ""
	return reflect.DeepEqual(a, b)
}

// sameKeys reports whether two device lists hold the same USB keys.
func sameKeys(a, b []device.USBDevice) bool {
	keys := func(l []device.USBDevice) map[device.USBKey]bool {
		m := map[device.USBKey]bool{}
		for _, d := range l {
			m[d.Key()] = true
		}
		return m
	}
	return maps.Equal(keys(a), keys(b))
}

// updateLoss recomputes the replug key of every entry bound on r's workload
// and clears the keys of entries no longer bound there. It reports whether a
// key changed, since a new key needs a step to record its deadline. Callers
// hold mu.
func (c *Controller) updateLoss(cfg *config.Config, v view, r *device.AgentReport, now time.Time) bool {
	seen := map[string]bool{}
	mod := false
	if cfg != nil {
		k8s, docker := c.backendName == BackendK8s, c.backendName == BackendDocker
		check := func(id string, p catalog.Placed, ok, printer bool) {
			if !ok || p.Node != r.Workload {
				return
			}
			seen[id] = true
			_, idKnown := c.idCache[p.Device.Key()]
			k := lossKey(k8s, docker, printer, p.Device.Key(), r, idKnown)
			st := c.loss[id]
			switch {
			case k == "":
				mod = mod || st != nil
				delete(c.loss, id)
			case st == nil:
				c.loss[id] = &lossState{workload: r.Workload, key: k, since: now}
				mod = true
			case st.key != k:
				st.workload, st.key, st.since = r.Workload, k, now
				mod = true
			}
		}
		for _, p := range cfg.Printers {
			pl, ok := v.printers[p.ID]
			check(p.ID, pl, ok, true)
		}
		for _, s := range cfg.Scanners {
			pl, ok := v.scanners[s.ID]
			check(s.ID, pl, ok, false)
		}
	}
	for id, st := range c.loss {
		if st.workload == r.Workload && !seen[id] {
			delete(c.loss, id)
			mod = true
		}
	}
	return mod
}

// lossKey is A8's replug key for the device key bound to an entry, from the
// report of its workload: "moved BBB/DDD" or "unallocated BBB/DDD" (k8s), or
// "appeared BBB/DDD" (k8s; docker only for a printer without a known device
// ID). "" means nothing to restart for. Old agents (Attached nil) give "".
func lossKey(k8s, docker, printer bool, key device.USBKey, r *device.AgentReport, idKnown bool) string {
	if r.Attached == nil {
		return ""
	}
	var att []device.USBDevice
	for _, d := range r.Attached {
		if d.Key() == key {
			att = append(att, d)
		}
	}
	i := slices.IndexFunc(r.Inventory.Devices, func(d device.USBDevice) bool { return d.Key() == key })
	switch {
	case k8s && i >= 0:
		inv := r.Inventory.Devices[i]
		if r.ProbeNodes != nil && !slices.Contains(r.ProbeNodes, inv.Node()) {
			return "unallocated " + inv.Node()
		}
		if !slices.ContainsFunc(att, func(d device.USBDevice) bool { return d.Node() == inv.Node() }) && len(att) == 1 {
			return "moved " + att[0].Node()
		}
	case k8s && len(att) > 0:
		return "appeared " + att[0].Node()
	case docker && printer && i < 0 && !idKnown && len(att) > 0:
		return "appeared " + att[0].Node()
	}
	return ""
}

// lossKind is the first word of a loss key.
func lossKind(key string) string {
	k, _, _ := strings.Cut(key, " ")
	return k
}

// desiredState is the response to workload w's report. Callers hold mu.
func (c *Controller) desiredState(w string, cfg *config.Config, v view) device.DesiredState {
	spec := c.specs[w]
	ds := device.DesiredState{
		Hash: device.SpecHash(spec), Queues: []device.QueueSpec{}, Browse: slices.Clone(device.BrowseTypes),
		StageEULAs: []device.EULARef{},
	}
	acc := []config.EULARef{}
	if cfg != nil {
		acc = cfg.Licenses.AcceptedEULAs
	}
	if spec != nil && cfg != nil {
		for _, ps := range spec.Printers {
			fam, ok := c.cat.Printer(ps.Family)
			i := slices.IndexFunc(cfg.Printers, func(p config.Printer) bool { return p.ID == ps.ID })
			pl, placed := c.placed[ps.ID]
			if now, ok := v.printers[ps.ID]; ok && now.Node == w {
				// This report's view of the device, at its current address.
				if now.Device.DeviceID == "" && now.Device.Key() == pl.Device.Key() {
					now.Device.DeviceID = pl.Device.DeviceID
				}
				pl, placed = now, true
			}
			if !ok || fam.App == "" || i < 0 || !placed {
				continue
			}
			p := cfg.Printers[i]
			dev := pl.Device
			if dev.DeviceID == "" {
				dev.DeviceID = c.idCache[dev.Key()]
			}
			dec := c.cat.DecidePrinter(&dev, p)
			driver := config.Auto
			if dec.Family == ps.Family && dec.Driver != "" {
				driver = dec.Driver
			}
			ds.Queues = append(ds.Queues, device.QueueSpec{
				ID: p.ID, Name: p.Name, Location: p.Location, Driver: driver, DriverPinned: driver == p.DriverName,
				Schemes: slices.Clone(fam.DeviceSchemes), SystemURI: device.PAPPLSystemURI(fam.Port),
				Device: dev, Defaults: device.PrintSettings(p.Defaults),
			})
		}
	}
	if w == c.blobWorkload(w) {
		ds.StageEULAs = stageEULAs(c.cat, c.printerDecisions(cfg, v), acc)
	}
	ds.Version = device.DesiredStateVersion(ds)
	return ds
}

// blobWorkload is the workload whose agent stages EULAs: the k8s backend's
// blob slot rule (blobSlot) over the desired specs, in node order: the first
// with blobs, else the first whose printers' family has a EULA, else the
// first. Without specs it is the first workload of the last step, else self.
// Callers hold mu.
func (c *Controller) blobWorkload(self string) string {
	names := slices.Sorted(maps.Keys(c.specs))
	for _, want := range []func(*device.Spec) bool{
		func(s *device.Spec) bool { return len(s.Blobs) > 0 },
		func(s *device.Spec) bool { return c.eulaFamily(s) },
		func(*device.Spec) bool { return true },
	} {
		for _, w := range names {
			if want(c.specs[w]) {
				return w
			}
		}
	}
	if len(c.workloads) > 0 {
		return c.workloads[0]
	}
	return self
}

// eulaFamily reports whether a printer of s is of a family with a EULA.
func (c *Controller) eulaFamily(s *device.Spec) bool {
	return slices.ContainsFunc(s.Printers, func(ps device.PrinterSpec) bool {
		f, ok := c.cat.Printer(ps.Family)
		return ok && len(f.RequiredEULAs)+len(f.OptionalEULAs) > 0
	})
}

// storeTexts keeps the latest text per EULA id and returns the EULA DTOs
// that changed. A pending report does not replace a known text of the same
// version: a rolled pod's agent reports pending until it has re-verified its
// staged archive. Callers hold mu.
func (c *Controller) storeTexts(texts []device.EULAText) []apitypes.EULA {
	if c.eulaTexts == nil {
		c.eulaTexts = map[string]device.EULAText{}
	}
	var changed []apitypes.EULA
	for _, t := range texts {
		e, known := c.cat.EULA(t.ID)
		if !known {
			continue // only catalog EULAs are ever shown, so the map stays bounded
		}
		if old, ok := c.eulaTexts[t.ID]; ok && t.State == device.EULAPending && old.Version == t.Version {
			continue
		}
		before := eulaDTO(e, c.eulaTexts)
		c.eulaTexts[t.ID] = t
		if after := eulaDTO(e, c.eulaTexts); !reflect.DeepEqual(before, after) {
			changed = append(changed, after)
		}
	}
	return changed
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
	defer c.mu.Unlock()
	set := c.workloads
	if len(set) == 0 {
		set = slices.Sorted(maps.Keys(c.reports))
	}
	cfg := c.effective()
	v := c.view(set, c.reports)
	return setup(c.cat, setupInput{
		backend: c.backendName, usbAvailable: c.usbAvailable, missing: c.cfgState == apitypes.ConfigMissing,
		cfg: cfg, devices: detect(c.cat, cfg, v, c.usbAvailable), decisions: c.printerDecisions(cfg, v), texts: c.eulaTexts,
	})
}

// Detect re-runs device detection (POST /setup/detect): it restarts every
// workload, so each probe looks at the USB bus again. At most once per detectLimit;
// a no-op on backends that cannot restart (sim).
func (c *Controller) Detect(ctx context.Context) error {
	r, ok := c.backend.(device.PodRestarter)
	if !ok {
		return nil
	}
	c.mu.Lock()
	now := c.now()
	if !c.detectAt.IsZero() && now.Sub(c.detectAt) < detectLimit { // a rate limit: it wakes nothing
		c.mu.Unlock()
		return nil
	}
	c.detectAt = now
	set := slices.Clone(c.workloads)
	c.mu.Unlock()
	if len(set) == 0 {
		set = []string{""}
	}
	var errs []error
	for _, w := range set {
		if err := r.RestartPod(ctx, w); err != nil {
			errs = append(errs, fmt.Errorf("restart workload %q: %w", w, err))
		}
	}
	return errors.Join(errs...)
}

// view is the device side of one moment: the workload set, each workload's
// known devices, and the binding of the config's entries to them.
type view struct {
	workloads []string
	nodes     map[string][]device.USBDevice // workload -> known devices (inventory ∪ Attached)
	probed    map[string]map[device.USBKey]bool
	simulated map[string]bool           // workload -> its inventory is simulated
	printers  map[string]catalog.Placed // Bind's printers
	scanners  map[string]catalog.Placed // Bind's scanners
	reports   map[string]*reportState   // by workload, only those in workloads
	cfg       *config.Config
}

// view builds the view of workload set set from reports. Callers hold mu
// (it reads idCache and the effective config).
func (c *Controller) view(set []string, reports map[string]*reportState) view {
	v := view{
		workloads: set, nodes: map[string][]device.USBDevice{}, probed: map[string]map[device.USBKey]bool{},
		simulated: map[string]bool{}, reports: map[string]*reportState{}, cfg: c.effective(),
	}
	for _, w := range set {
		rs := reports[w]
		if rs == nil {
			continue
		}
		v.reports[w] = rs
		v.nodes[w] = known(rs.r, c.idCache)
		v.simulated[w] = rs.r.Inventory.Simulated
		p := map[device.USBKey]bool{}
		for _, d := range rs.r.Inventory.Devices {
			p[d.Key()] = true
		}
		v.probed[w] = p
	}
	v.printers, v.scanners = c.cat.Bind(v.cfg, v.nodes)
	return v
}

// isProbed reports whether a probe read device k: it is in w's inventory or
// a probe read its device ID before.
func (v view) isProbed(w string, k device.USBKey, ids map[device.USBKey]string) bool {
	_, cached := ids[k]
	return v.probed[w][k] || cached
}

// known is a workload's known device set: the probe inventory, with a device
// that moved (one inventory and one attached device of that key, at different
// addresses) at its new address, plus the other attached devices with their
// device ID from ids. Inventory devices come first, so Bind keeps their
// DeviceID.
func known(r device.AgentReport, ids map[device.USBKey]string) []device.USBDevice {
	out := make([]device.USBDevice, 0, len(r.Inventory.Devices)+len(r.Attached))
	for _, d := range r.Inventory.Devices {
		d.Interfaces = slices.Clone(d.Interfaces)
		out = append(out, d)
	}
	count := func(list []device.USBDevice, k device.USBKey) int {
		n := 0
		for _, d := range list {
			if d.Key() == k {
				n++
			}
		}
		return n
	}
	at := func(list []device.USBDevice, node string) bool {
		return slices.ContainsFunc(list, func(d device.USBDevice) bool { return d.Node() == node })
	}
	for _, a := range r.Attached {
		if at(r.Inventory.Devices, a.Node()) {
			continue
		}
		k := a.Key()
		if count(r.Inventory.Devices, k) == 1 && count(r.Attached, k) == 1 {
			i := slices.IndexFunc(out, func(d device.USBDevice) bool { return d.Key() == k })
			if !at(r.Attached, out[i].Node()) {
				out[i].Bus, out[i].Address = a.Bus, a.Address
				continue
			}
		}
		a.Interfaces = slices.Clone(a.Interfaces)
		if a.DeviceID == "" {
			a.DeviceID = ids[k]
		}
		out = append(out, a)
	}
	return out
}

// workloadSet is A7's workload set: the listed nodes plus the named
// workloads that report or run a device-serving spec, sorted; or [""] when
// the node list is unknown (no lister, an error) or empty with no named
// workload. An unlisted node stays while its agent reported within
// pendingWait (recorded in wake) or its applied spec serves a device, so a
// node can leave.
func workloadSet(listed bool, nodes []string, reports map[string]*reportState, applied map[string]*device.Spec, now time.Time, wake *wakeAt) []string {
	if !listed {
		return []string{""}
	}
	set := map[string]bool{}
	for _, n := range nodes {
		if n != "" {
			set[n] = true
		}
	}
	for w, s := range applied {
		if w != "" && s != nil && s.Mode != device.ModeProbe {
			set[w] = true
		}
	}
	for w, rs := range reports {
		if w != "" && !set[w] && wake.within(now, rs.at, pendingWait) {
			set[w] = true
		}
	}
	if len(set) == 0 {
		return []string{""}
	}
	return slices.Sorted(maps.Keys(set))
}

// legacyAlias is the node the workload "" stands for after the upgrade from
// the single-workload release: in multi-node mode the backend still observes
// the old unpinned slot 0 as "" and its agent reports as "", until the first
// Apply pins it. With exactly one node in the set, "" is that node, so its
// devices stay bound across the switch and the first Apply is the full spec.
// shortcut: with several nodes "" is not mapped and its node gets a probe spec first; map by the pod's node name if multi-node upgrades matter.
func legacyAlias(set []string, obs []device.Observation) string {
	if len(set) != 1 || set[0] == "" {
		return ""
	}
	legacy := false
	for _, o := range obs {
		switch o.Workload {
		case "":
			legacy = o.Phase != device.PhaseAbsent
		case set[0]:
			return ""
		}
	}
	if legacy {
		return set[0]
	}
	return ""
}

// aliasObs names the observation of workload "" alias (legacyAlias).
func aliasObs(obs []device.Observation, alias string) []device.Observation {
	if alias == "" {
		return obs
	}
	out := slices.Clone(obs)
	for i := range out {
		if out[i].Workload == "" {
			out[i].Workload = alias
		}
	}
	return out
}

// desired is the outcome of the spec rules.
type desired struct {
	specs     map[string]*device.Spec   // by workload
	placed    map[string]catalog.Placed // entry id -> device (bound now or sticky), DeviceID filled
	present   []device.USBDevice        // the bound devices of served entries, for SetUSBDevices
	noID      map[string]bool           // bound printers left out until a probe reads their device ID
	leftOut   map[string]bool           // workloads with devices past maxWorkloads, not applied
	apply     bool                      // false: wait, or leave whatever runs
	wait      bool                      // the control-plane restart wait (A7)
	nodesErr  string                    // the nodes reconcile item's message, "" fine
	printers  map[string]device.PrinterDecision
	scanners  map[string]device.ScannerDecision
	workloadW map[string]string // entry id -> workload of its spec
}

// decide builds the desired specs (A6, A7) from the snapshot and view. The
// snapshot's previous specs and placements keep sticky entries and are kept
// as they are while the control-plane restart wait holds.
func (c *Controller) decide(in snapshot, v view, obs map[string]device.Observation, obsErr error) desired {
	d := desired{
		specs: in.specs, placed: map[string]catalog.Placed{}, noID: map[string]bool{}, leftOut: map[string]bool{},
		printers: map[string]device.PrinterDecision{}, scanners: map[string]device.ScannerDecision{},
		workloadW: map[string]string{},
	}
	cfg := in.cfg
	switch {
	case !in.cfgRead:
		d.placed = in.placed
		return d // the file has not been read yet: it is not known to be missing
	case cfg == nil && in.cfgState == apitypes.ConfigInvalid:
		d.placed = in.placed
		return d // no last-known-good: no Apply, leave whatever runs; sticky placements stay
	case cfg == nil:
		d.specs, d.apply = map[string]*device.Spec{}, true
		if c.usbAvailable {
			for _, w := range v.workloads[:min(len(v.workloads), maxWorkloads)] { // the backend's slots
				d.specs[w] = finish(&device.Spec{Mode: device.ModeProbe, Node: w})
			}
		}
		return d
	}

	served := map[string]*device.Spec{}
	spec := func(w string) *device.Spec {
		if served[w] == nil {
			served[w] = &device.Spec{Node: w}
		}
		return served[w]
	}
	// sticky finds the entry in the previous spec of a workload in the set,
	// with its placement, while the entry still selects that device.
	sticky := func(id string, usb *config.USB) (catalog.Placed, bool) {
		pl, ok := in.placed[id]
		if !ok || !slices.Contains(v.workloads, pl.Node) || in.specs[pl.Node] == nil {
			return catalog.Placed{}, false
		}
		s := in.specs[pl.Node]
		if !slices.ContainsFunc(s.Printers, func(p device.PrinterSpec) bool { return p.ID == id }) &&
			!slices.ContainsFunc(s.Scanners, func(p device.ScannerSpec) bool { return p.ID == id }) {
			return catalog.Placed{}, false
		}
		if usb != nil && !selects(usb, pl.Device) {
			return catalog.Placed{}, false
		}
		return pl, true
	}
	first := v.workloads[0]
	simPort := 0
	simFam, _ := c.cat.Printer("sim")
	printers := slices.Clone(cfg.Printers)
	slices.SortFunc(printers, func(a, b config.Printer) int { return strings.Compare(a.ID, b.ID) })
	for _, p := range printers {
		switch {
		case !p.Enabled:
			continue
		case p.Simulated():
			dec := c.cat.DecidePrinter(nil, p)
			d.printers[p.ID] = dec
			s := spec(first)
			s.Printers = append(s.Printers, device.PrinterSpec{
				ID: p.ID, Family: dec.Family, Name: p.Name, Location: p.Location, Port: simFam.Port + simPort,
			})
			d.workloadW[p.ID] = first
			simPort++
			continue
		case c.usbUnavailable != "":
			continue
		}
		pl, bound := v.printers[p.ID]
		current := bound && v.isProbed(pl.Node, pl.Device.Key(), in.idCache)
		if !current {
			spl, ok := sticky(p.ID, p.USB)
			switch {
			case ok:
				pl = spl
			case bound:
				// Seen only live: it joins once a probe reads its device ID.
				dev := pl.Device
				d.printers[p.ID], d.placed[p.ID], d.noID[p.ID] = c.cat.DecidePrinter(&dev, p), pl, true
				continue
			default:
				continue
			}
		}
		if pl.Device.DeviceID == "" {
			pl.Device.DeviceID = in.idCache[pl.Device.Key()]
		}
		dev := pl.Device
		dec := c.cat.DecidePrinter(&dev, p)
		d.printers[p.ID], d.placed[p.ID] = dec, pl
		if dec.Family == "" {
			continue // rule none: no printer runs
		}
		s := spec(pl.Node)
		s.Printers = append(s.Printers, device.PrinterSpec{ID: p.ID, Family: dec.Family, USB: pl.Device.Key()})
		d.workloadW[p.ID] = pl.Node
		if current {
			d.present = append(d.present, pl.Device)
		}
	}
	scanners := slices.Clone(cfg.Scanners)
	slices.SortFunc(scanners, func(a, b config.Scanner) int { return strings.Compare(a.ID, b.ID) })
	for _, sc := range scanners {
		switch {
		case !sc.Enabled:
			continue
		case sc.Simulated():
			dec := c.cat.DecideScanner(nil, sc)
			d.scanners[sc.ID] = dec
			s := spec(first)
			s.Scanners = append(s.Scanners, device.ScannerSpec{ID: sc.ID, Family: dec.Family, SaneBackends: slices.Clone(dec.SaneBackends)})
			d.workloadW[sc.ID] = first
			continue
		case c.usbUnavailable != "":
			continue
		}
		pl, current := v.scanners[sc.ID]
		ok := current
		if !ok {
			if pl, ok = sticky(sc.ID, sc.USB); !ok {
				continue
			}
		}
		dev := pl.Device
		dec := c.cat.DecideScanner(&dev, sc)
		d.scanners[sc.ID], d.placed[sc.ID] = dec, pl
		s := spec(pl.Node)
		s.Scanners = append(s.Scanners, device.ScannerSpec{ID: sc.ID, Family: dec.Family, SaneBackends: slices.Clone(dec.SaneBackends), USB: pl.Device.Key()})
		d.workloadW[sc.ID] = pl.Node
		if current {
			d.present = append(d.present, pl.Device)
		}
	}
	d.present = slices.CompactFunc(sortedByNode(d.present), func(a, b device.USBDevice) bool { return a.Node() == b.Node() })

	// The control-plane restart wait: a workload the backend sees pending or
	// running whose agent has not reported since this control plane started
	// keeps the running specs, as every usb entry is unbound until it reports.
	usbEntries := c.usbUnavailable == "" &&
		(slices.ContainsFunc(cfg.Printers, func(p config.Printer) bool { return p.Enabled && !p.Simulated() }) ||
			slices.ContainsFunc(cfg.Scanners, func(s config.Scanner) bool { return s.Enabled && !s.Simulated() }))
	// Observed workloads count whether listed or not: a node whose only
	// printer is off advertises no USB resource but still runs its pod.
	if usbEntries && obsErr != nil && slices.ContainsFunc(v.workloads, func(w string) bool { return !in.reported[w] }) {
		d.wait = true
		return d
	}
	if usbEntries {
		for w, o := range obs {
			if !in.reported[w] && (o.Phase == device.PhasePending || o.Phase == device.PhaseRunning) && in.wake.within(in.now, c.started, pendingWait) {
				d.wait = true
				return d
			}
		}
	}

	specs := map[string]*device.Spec{}
	var withDevices []string
	for _, w := range v.workloads {
		s := served[w]
		switch {
		case s != nil:
			s.Mode = device.ModeSimulated
			if slices.ContainsFunc(s.Printers, func(p device.PrinterSpec) bool { return p.USB != device.USBKey{} }) ||
				slices.ContainsFunc(s.Scanners, func(p device.ScannerSpec) bool { return p.USB != device.USBKey{} }) {
				s.Mode = device.ModeUSB
			}
			var decs []device.PrinterDecision
			for _, p := range s.Printers {
				decs = append(decs, d.printers[p.ID])
			}
			s.Blobs = blobRefs(decs, cfg.Licenses.AcceptedEULAs, c.cat)
			specs[w] = s
			if w != "" {
				withDevices = append(withDevices, w)
			}
		case c.usbAvailable:
			specs[w] = &device.Spec{Mode: device.ModeProbe, Node: w}
		}
	}
	// The backend has maxWorkloads slots: device nodes past the cap are left
	// out (those already served first, then name order), then probe-only
	// nodes give way until the rest fits.
	var problems []string
	if len(withDevices) > maxWorkloads {
		served := func(w string) bool { s := in.specs[w]; return s != nil && s.Mode != device.ModeProbe }
		slices.SortStableFunc(withDevices, func(a, b string) int {
			switch {
			case served(a) == served(b):
				return 0
			case served(a):
				return -1
			}
			return 1
		})
		out := slices.Sorted(slices.Values(withDevices[maxWorkloads:]))
		for _, w := range out {
			delete(specs, w)
			d.leftOut[w] = true
		}
		problems = append(problems, fmt.Sprintf("devices on more than %d nodes: %s are left out", maxWorkloads, strings.Join(out, ", ")))
	}
	for i := len(v.workloads) - 1; i >= 0 && len(specs) > maxWorkloads; i-- {
		if s := specs[v.workloads[i]]; s != nil && s.Mode == device.ModeProbe {
			delete(specs, v.workloads[i])
		}
	}
	// Only the blob workload may mount the ReadWriteOnce blob volume.
	var needBlobs []string
	for _, w := range v.workloads {
		if s := specs[w]; s != nil && len(s.Blobs) > 0 {
			needBlobs = append(needBlobs, w)
		}
	}
	if len(needBlobs) > 1 {
		for _, w := range needBlobs[1:] {
			specs[w].Blobs = nil
		}
		problems = append(problems, fmt.Sprintf("nodes %s need vendor blobs; only %s gets them", strings.Join(needBlobs, ", "), needBlobs[0]))
	}
	d.nodesErr = strings.Join(problems, "; ")
	for w, s := range specs {
		s.AcceptedEULAs = acceptedEULAs(s.Blobs, cfg.Licenses.AcceptedEULAs, c.cat)
		specs[w] = finish(s)
	}
	d.specs, d.apply = specs, true
	return d
}

// maxWorkloads is the k8s backend's slot count (k8s.MaxSlots).
const maxWorkloads = 4

// finish sorts the spec's lists by ID and sets its hash.
func finish(s *device.Spec) *device.Spec {
	slices.SortFunc(s.Printers, func(a, b device.PrinterSpec) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(s.Scanners, func(a, b device.ScannerSpec) int { return strings.Compare(a.ID, b.ID) })
	s.Hash = device.SpecHash(s)
	return s
}

// selects reports whether a usb selector (serial "" matches any) picks d.
func selects(u *config.USB, d device.USBDevice) bool {
	return strings.EqualFold(d.VendorID, u.VendorID) && strings.EqualFold(d.ProductID, u.ProductID) &&
		(u.Serial == "" || d.Serial == u.Serial)
}

func sortedByNode(devs []device.USBDevice) []device.USBDevice {
	slices.SortFunc(devs, func(a, b device.USBDevice) int { return strings.Compare(a.Node(), b.Node()) })
	return devs
}

// combinedHash is the hash over every workload's spec hash, sorted by workload.
func combinedHash(specs map[string]*device.Spec) string {
	var lines []string
	for w, s := range specs {
		lines = append(lines, w+"="+s.Hash)
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// applyErr is the last Apply's error for workload w, "" when it applied.
func (c *Controller) applyErr(w string) string {
	if c.failMsg != "" {
		return c.failMsg
	}
	return c.failW[w]
}

// due is the Apply rule: the hash differs from the last successful Apply, or
// the last Apply of it failed and its retry delay (retryDelay of failCount)
// passed, which wake records.
func (c *Controller) due(hash string, now time.Time, wake *wakeAt) bool {
	if c.failed && hash == c.failHash {
		return !wake.within(now, c.failAt, retryDelay(c.failCount))
	}
	return !c.applied || hash != c.appliedHash
}

// snapshot is the loop's copy of its inputs.
type snapshot struct {
	now       time.Time
	wake      *wakeAt         // the step's deadlines
	capsReset map[string]bool // workloads whose scanners' caps retry at once
	cfgRead   bool
	cfgState  string
	cfg       *config.Config
	specs     map[string]*device.Spec
	placed    map[string]catalog.Placed
	loss      map[string]lossState
	idCache   map[device.USBKey]string
	reported  map[string]bool
}

// step is one loop iteration.
func (c *Controller) step(ctx context.Context) {
	lister, hasLister := c.backend.(device.NodeLister)
	var nodes []string
	var nodesErr error
	if hasLister {
		// Before Observe, so the backend's mode and slot map are settled and
		// Observe names the workloads the agents report.
		nodes, nodesErr = lister.USBNodes(ctx)
	}
	// A transient node-list error (not single-node mode) keeps the last
	// workload set and applies nothing, so one API-server blip never drops a
	// multi-node install to one unpinned workload.
	blip := nodesErr != nil && !errors.Is(nodesErr, device.ErrSingleNode)
	if blip && !c.nodesBlip {
		slog.Warn("listing nodes failed; keeping the device workloads as they are", "err", nodesErr)
	}
	wasBlip := c.nodesBlip
	c.nodesBlip = blip
	obsList, obsErr := c.backend.Observe(ctx)

	c.mu.Lock()
	now := c.now()
	wake := &wakeAt{}
	applied := map[string]*device.Spec{}
	for w := range c.appliedBy {
		applied[w] = c.specs[w]
	}
	set := workloadSet(hasLister && nodesErr == nil, nodes, c.reports, applied, now, wake)
	if blip && len(c.workloads) > 0 {
		set = slices.Clone(c.workloads)
	}
	c.alias = legacyAlias(set, obsList)
	obsList = aliasObs(obsList, c.alias)
	if a := c.alias; a != "" {
		if rs := c.reports[""]; rs != nil && (c.reports[a] == nil || rs.at.After(c.reports[a].at)) {
			rs.r.Workload = a
			c.reports[a] = rs
		}
		if c.reported[""] {
			c.reported[a] = true
		}
	}
	for w := range c.reports {
		if !blip && !slices.Contains(set, w) { // a blip's set may be a stand-in [""]
			delete(c.reports, w)
		}
	}
	for id, st := range c.loss {
		if !blip && !slices.Contains(set, st.workload) {
			delete(c.loss, id)
		}
	}
	c.workloads = set
	in := snapshot{
		now: now, wake: wake, capsReset: c.capsReset, cfgRead: c.cfgRead, cfgState: c.cfgState, cfg: c.effective(),
		specs: maps.Clone(c.specs), placed: maps.Clone(c.placed), idCache: maps.Clone(c.idCache), loss: map[string]lossState{},
		reported: maps.Clone(c.reported),
	}
	c.capsReset = map[string]bool{}
	for id, st := range c.loss {
		in.loss[id] = *st
	}
	v := c.view(set, c.reports)
	obs := observations(obsList)
	d := c.decide(in, v, obs, obsErr)
	if blip {
		d.specs, d.apply = in.specs, false
	}
	c.specs, c.placed = d.specs, d.placed
	c.desiredChanged()
	c.mu.Unlock()

	hash := combinedHash(d.specs)
	if t, ok := c.backend.(device.USBDeviceSetter); ok && d.apply {
		// Before Apply, so the backend adopts what runs. Until the backend's
		// want is these specs, the devices wait for the Apply that brings them.
		t.SetUSBDevices(d.present, !c.applied || hash != c.appliedHash)
	}
	if d.apply && c.due(hash, now, wake) {
		list := make([]*device.Spec, 0, len(d.specs))
		for _, w := range slices.Sorted(maps.Keys(d.specs)) {
			list = append(list, d.specs[w])
		}
		actx, cancel := context.WithTimeout(ctx, applyTimeout)
		err := c.backend.Apply(actx, list)
		cancel()
		// A WorkloadErrors failed only the workloads it names; the rest applied.
		var we device.WorkloadErrors
		partial := errors.As(err, &we)
		again := c.failed && c.failHash == hash
		c.failed, c.failMsg, c.failW = err != nil, "", map[string]string{}
		if err != nil {
			if again {
				c.failCount++
			} else {
				c.failCount = 1
			}
			c.applied, c.failHash, c.failAt = false, hash, now
			wake.at(now.Add(retryDelay(c.failCount)), now)
			if partial {
				for w, e := range we {
					c.failW[w] = e.Error()
				}
			} else {
				c.failMsg = err.Error()
			}
			if err.Error() != c.failLogged {
				slog.Warn("device workload apply failed", "err", err)
				c.failLogged = err.Error()
			}
		} else {
			c.applied, c.appliedHash, c.failLogged, c.failCount = true, hash, "", 0
		}
		if err == nil || partial {
			c.appliedBy = map[string]string{}
			for w, s := range d.specs {
				if _, bad := we[w]; !bad {
					c.appliedBy[w] = s.Hash
				}
			}
		}
		items := slices.Sorted(maps.Keys(d.specs))
		if len(items) == 0 {
			items = set
		}
		c.mu.Lock()
		failedItems := map[string]bool{}
		for _, w := range items {
			msg := c.applyErr(w)
			c.setItem(workloadItem(w), msg == "", msg)
			if msg != "" {
				failedItems[workloadItem(w)] = true
			}
		}
		for w, msg := range c.failW { // a slot freed for a node that left the specs
			if !failedItems[workloadItem(w)] {
				c.setItem(workloadItem(w), false, msg)
				failedItems[workloadItem(w)] = true
			}
		}
		for item := range c.itemsFailed { // workloads that left the specs or recovered
			if !failedItems[item] && !slices.ContainsFunc(items, func(w string) bool { return workloadItem(w) == item }) {
				c.setItem(item, true, "")
			}
		}
		c.itemsFailed = failedItems
		c.mu.Unlock()
		obsList, obsErr = c.backend.Observe(ctx)
		obs = observations(aliasObs(obsList, c.alias))
	}
	switch {
	case blip: // nothing applies while it lasts, so say why
		c.nodesFailed = true
		c.mu.Lock()
		c.setItem("nodes", false, "listing nodes failed: "+nodesErr.Error()+"; device workloads are kept as they are")
		c.mu.Unlock()
	case d.apply && (d.nodesErr != "" || c.nodesFailed):
		c.nodesFailed = d.nodesErr != ""
		c.mu.Lock()
		c.setItem("nodes", d.nodesErr == "", d.nodesErr)
		c.mu.Unlock()
	case wasBlip:
		c.nodesFailed = false
		c.mu.Lock()
		c.setItem("nodes", true, "")
		c.mu.Unlock()
	}
	applyErr := func(string) string { return "" }
	if c.failed && d.apply {
		applyErr = c.applyErr
	}
	// A transient node-list error or an Observe error has no event of its
	// own when it clears: retry with the same backoff as Apply. In k8s
	// single-node mode this step's USBNodes also re-arms the backend's
	// node-list re-check.
	if blip || obsErr != nil {
		c.blipCount++
		wake.at(now.Add(retryDelay(c.blipCount)), now)
	} else {
		c.blipCount = 0
	}

	c.restart(ctx, in, d)

	c.mu.Lock()
	for id, st := range c.loss {
		in.loss[id] = *st
	}
	c.mu.Unlock()
	st, want := c.observe(ctx, in, v, d, obs, obsErr, applyErr)
	c.watchWant, c.next, c.stepAt = want, wake.t, now

	c.mu.Lock()
	c.status = st
	after := c.current()
	changed := !reflect.DeepEqual(c.published, after)
	if changed {
		c.published = after
		for ch := range c.subs {
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

// workloadItem is the reconcile item of workload w's Apply.
func workloadItem(w string) string {
	return "workload/" + cmp.Or(w, "default")
}

// observations indexes the backend's observations by workload.
func observations(list []device.Observation) map[string]device.Observation {
	out := map[string]device.Observation{}
	for _, o := range list {
		out[o.Workload] = o
	}
	return out
}

// restart calls RestartPod once per workload with a due replug key (A8) and
// marks every due key on it as restarted: at most one restart per change.
func (c *Controller) restart(ctx context.Context, in snapshot, d desired) {
	r, ok := c.backend.(device.PodRestarter)
	if !ok {
		return
	}
	c.mu.Lock()
	due := map[string][]string{} // workload -> entry ids
	for id, st := range c.loss {
		if st.key != "" && st.key != st.restartedKey && d.specs[st.workload] != nil && !in.wake.within(in.now, st.since, replugDelay) {
			due[st.workload] = append(due[st.workload], id)
		}
	}
	keys := map[string]lossState{}
	for _, ids := range due {
		for _, id := range ids {
			keys[id] = *c.loss[id]
		}
	}
	c.mu.Unlock()
	for _, w := range slices.Sorted(maps.Keys(due)) {
		ids := due[w]
		slices.Sort(ids)
		var reasons []string
		for _, id := range ids {
			reasons = append(reasons, id+": "+keys[id].key)
		}
		slog.Info("device pod restart", "workload", w, "reason", strings.Join(reasons, ", "))
		if err := r.RestartPod(ctx, w); err != nil {
			slog.Warn("device pod restart failed", "workload", w, "err", err)
		}
		c.mu.Lock()
		for _, id := range ids {
			if st := c.loss[id]; st != nil && st.key == keys[id].key && st.since.Equal(keys[id].since) {
				st.restartedKey = st.key
			}
		}
		c.mu.Unlock()
	}
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
