package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/godbus/dbus/v5"

	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/dnssd"
	"github.com/ThePhaseless/pagewire/internal/ippnotify"
	"github.com/ThePhaseless/pagewire/internal/pappl"
	"github.com/ThePhaseless/pagewire/internal/usb"
)

const (
	minBackoff       = time.Second
	maxBackoff       = 30 * time.Second
	reconcileTimeout = 30 * time.Second
	// debounce gathers a burst of events (a replug, Avahi start-up, a queue
	// re-created) into one cycle.
	debounce = 200 * time.Millisecond
	// pendingRetry is the first re-check of a pending queue whose device is
	// present; it doubles up to pendingRetryMax while the queue stays pending.
	pendingRetry    = 30 * time.Second
	pendingRetryMax = 5 * time.Minute

	// MsgNoSystemURI is the QueueResult message for a desired queue whose
	// SystemURI is empty, or that Core.SystemURL translates to "".
	MsgNoSystemURI = "queue has no system URI"
)

// Browser watches DNS-SD for the given service types. It calls update with
// this host's full service list on start and after each change, and blocks
// until ctx ends or the watch breaks; Loop starts it again after an error.
type Browser func(ctx context.Context, types []string, update func([]device.DNSSDService)) error

// Core is the transport-independent agent: one cycle's logic plus Loop. Make
// it with NewCore (a zero Core panics in Loop) and set the exported fields
// before calling Loop; they must not change afterwards.
//
// Loop runs a cycle only when something changed: an event source fired, the
// desired state changed, Wake was called, or a queue retry is due. Between
// cycles it re-sends the last report as its heartbeat.
type Core struct {
	PodUID    string
	Workload  string // reported as AgentReport.Workload (Spec.Node)
	Inventory device.Inventory
	// Instance is random hex per Core, sent in every report with a Seq
	// counting from 1, so the controller can tell a late report from a new one.
	Instance string
	// Attached lists the USB devices attached now (sysfs); nil: not reported.
	// Called from the Loop goroutine, once per cycle.
	Attached   func() []device.USBDevice
	ProbeNodes []string                   // the probe's usbfs nodes, copied into every report; nil: unknown
	Blobs      func() []device.BlobStatus // nil: no blob reporting; called from the Loop goroutine, once per cycle
	// SystemURL translates a queue's SystemURI to the URL the agent connects
	// to; "" reports the queue as MsgNoSystemURI. nil is identity, which k8s,
	// docker and compose use: the printer applications listen on their
	// catalog ports. The sim sets it to reach its fakes on random ports.
	// Called from the Loop goroutine, once per cycle.
	SystemURL func(systemURI string) string
	Browse    Browser // nil: no DNS-SD browsing
	// USBEvents blocks until ctx ends and calls changed when the attached USB
	// devices may have changed (usb.Watch), once as soon as it listens; nil:
	// Attached changes only show on other events. When it returns early,
	// every heartbeat runs a cycle.
	USBEvents func(ctx context.Context, changed func()) error
	// BlobEvents is USBEvents for the Blobs source (the blob status file).
	BlobEvents func(ctx context.Context, changed func()) error
	// Stage is called every cycle with the desired StageEULAs (nil before the
	// first DesiredState) and returns their current texts; nil: none reported.
	// It must not block on downloads (Stager.Stage does not).
	Stage func(ctx context.Context, refs []device.EULARef) []device.EULAText

	minBackoff time.Duration
	maxBackoff time.Duration
	agentWait  time.Duration // the hold a report asks for; device.AgentWait
	// papplClients are keyed by resolved system URL, made on first use and
	// kept for the agent's life: each remembers its Find-Devices choices.
	// shortcut: clients of URLs no longer desired (sim re-Applies) stay; bounded by the re-Applies of one session.
	papplClients map[string]*pappl.Client
	after        func(time.Duration) <-chan time.Time
	wake         chan struct{}

	// Touched only by the Loop goroutine.
	desired     *device.DesiredState
	seq         uint64
	attached    []device.USBDevice            // the last cycle's Attached
	results     map[string]urlResult          // the last reconcile per system URL
	watchers    map[string]context.CancelFunc // PAPPL watches per system URL
	errFails    int                           // reconciles in a row with a queue error
	pendFails   int                           // reconciles in a row with a pending queue whose device is present
	retry       <-chan time.Time              // the queue retry; nil: none
	retryURLs   []string                      // the system URLs retry reconciles
	browsing    bool                          // a Browse runs or ran for browseTypes
	browseTypes []string
	browseStop  context.CancelFunc

	mu        sync.Mutex // guards the fields below
	dirty     dirty
	services  []device.DNSSDService
	browseGen int               // the running Browse; older ones' updates are dropped
	degraded  map[string]bool   // event sources ("usb", "blobs") that stopped
	unwatched map[string]bool   // system URLs without a PAPPL watch
	failing   map[string]string // problem key -> the message logged when it started
}

// dirty is what the next cycle must reconcile.
type dirty struct {
	all  bool            // every queue: Wake, a desired change, a USB change
	urls map[string]bool // the queues of these system URLs: a PAPPL event, a retry, a heartbeat re-check
	// reset restarts the retry backoffs: set by events, never by the retry
	// timer or a heartbeat, so a failure that persists still backs off.
	reset bool
}

func (d *dirty) url(u string) {
	if d.urls == nil {
		d.urls = map[string]bool{}
	}
	d.urls[u] = true
}

// urlResult is one system URL's last reconcile: the queue ids, in order, and
// their results.
type urlResult struct {
	ids []string
	res []device.QueueResult
}

// NewCore returns a Core reporting inv as pod podUID.
func NewCore(podUID string, inv device.Inventory) *Core {
	id := make([]byte, 8)
	_, _ = rand.Read(id) // never fails (crypto/rand panics instead)
	return &Core{
		PodUID: podUID, Inventory: inv, Instance: hex.EncodeToString(id),
		minBackoff: minBackoff, maxBackoff: maxBackoff, agentWait: device.AgentWait,
		after: time.After, wake: make(chan struct{}, 1),
		papplClients: map[string]*pappl.Client{}, results: map[string]urlResult{}, watchers: map[string]context.CancelFunc{},
		degraded: map[string]bool{}, unwatched: map[string]bool{}, failing: map[string]string{},
	}
}

// Wake makes Loop run a full cycle soon, every queue reconciled even when
// nothing it watches changed (the sim's re-Apply moves its PAPPL fakes; a
// staged EULA finished). It never blocks and is safe to call from any
// goroutine.
func (c *Core) Wake() { c.poke(func(d *dirty) { d.all, d.reset = true, true }) }

// poke applies mark (nil: none) to the dirty set and wakes Loop. Event
// sources call it from their own goroutines.
func (c *Core) poke(mark func(*dirty)) {
	if mark != nil {
		c.mu.Lock()
		mark(&c.dirty)
		c.mu.Unlock()
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Loop runs cycles until ctx is done. Each report goes out with Wait, so the
// control plane holds it until the desired state changes or AgentWait passes;
// its return is the heartbeat, which re-sends the last report without a
// cycle. A changed DesiredState (compared whole, never by Version) runs a
// cycle at once; an event, Wake or a due queue retry cancels the held report
// and runs one after a 200 ms debounce. While report fails Loop backs off from
// 1 s to 30 s. Loop is the only caller of report; a result that arrives after
// ctx is done is dropped. It returns once its event sources have stopped.
func (c *Core) Loop(ctx context.Context, report device.ReportFunc) {
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// No mark: cycle reconciles every queue only when Attached differs (a replug changes the address).
	c.source(ctx, &wg, "usb", "USB events unavailable; re-reading USB devices at each report", c.USBEvents, nil)
	c.source(ctx, &wg, "blobs", "blob status events unavailable; re-reading it at each report", c.BlobEvents, nil)

	c.poke(func(d *dirty) { d.all = true })
	var (
		last    device.AgentReport
		backoff time.Duration
		cycle   = true
	)
	for ctx.Err() == nil {
		if cycle {
			last = c.cycle(ctx, &wg)
		}
		c.seq++
		r := last
		r.Instance, r.Seq, r.Wait = c.Instance, c.seq, true
		if c.desired != nil {
			r.DesiredVersion = c.desired.Version
		}
		sent := time.Now()
		res, interrupted := c.send(ctx, report, r)
		if ctx.Err() != nil {
			return
		}
		if interrupted {
			cycle = true
			continue
		}
		c.problem("report", "control plane unreachable", res.err)
		var wait time.Duration
		if res.err != nil {
			backoff = min(max(2*backoff, c.minBackoff), c.maxBackoff)
			wait = backoff
		} else {
			backoff = 0
			if c.desired == nil || !reflect.DeepEqual(*c.desired, res.ds) {
				c.desired = &res.ds
				c.poke(func(d *dirty) { d.all, d.reset = true, true })
				cycle = true
				continue
			}
			// An answer well before the hold ends comes from a control plane
			// that does not hold (an old one): keep the heartbeat at AgentWait.
			if took := time.Since(sent); took < c.agentWait/2 {
				wait = c.agentWait - took
			}
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
				c.settle(ctx)
				cycle = true
				continue
			case <-c.retry:
				c.retryDue()
				cycle = true
				continue
			case <-c.after(wait):
			}
		}
		cycle = c.heartbeatCycle()
	}
}

// send runs report in its own goroutine and returns its answer, or cancels it
// and returns interrupted when an event, Wake or the queue retry comes first.
func (c *Core) send(ctx context.Context, report device.ReportFunc, r device.AgentReport) (answer, bool) {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan answer, 1)
	go func() {
		ds, err := report(rctx, r)
		ch <- answer{ds, err}
	}()
	woke := false
	select {
	case res := <-ch:
		return res, false
	case <-ctx.Done():
		return answer{}, false
	case <-c.wake:
		woke = true
	case <-c.retry:
		c.retryDue()
	}
	cancel()
	select { // Loop stays the only caller of report
	case <-ch:
	case <-ctx.Done():
		return answer{}, false
	}
	if woke {
		c.settle(ctx)
	}
	return answer{}, true
}

// answer is one report's result.
type answer struct {
	ds  device.DesiredState
	err error
}

// settle waits out the debounce, so a burst of events runs one cycle.
func (c *Core) settle(ctx context.Context) {
	t := time.NewTimer(debounce)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// retryDue marks the queues the retry timer was armed for.
func (c *Core) retryDue() {
	c.retry = nil
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, u := range c.retryURLs {
		c.dirty.url(u)
	}
}

// heartbeatCycle reports whether the heartbeat must run a cycle instead of
// re-sending the last report, the one re-read for a missing event source:
// while USB or blob events are down a full cycle (every queue), else one that
// re-checks the queues of system URLs without a PAPPL watch. Neither resets
// the retry backoffs.
func (c *Core) heartbeatCycle() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.degraded) > 0 {
		c.dirty.all = true
	}
	for u := range c.unwatched {
		c.dirty.url(u)
	}
	return len(c.degraded) > 0 || len(c.unwatched) > 0
}

// source runs one event source until ctx ends; mark is what its events dirty.
// A source that stops early is logged and marked degraded.
func (c *Core) source(ctx context.Context, wg *sync.WaitGroup, key, msg string, run func(context.Context, func()) error, mark func(*dirty)) {
	if run == nil {
		return
	}
	wg.Go(func() {
		err := run(ctx, func() { c.poke(mark) })
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("stopped")
		}
		c.mu.Lock()
		c.degraded[key] = true
		c.mu.Unlock()
		c.problem(key, msg, err)
		c.poke(mark) // events may have been missed
	})
}

// cycle lists the attached devices, re-reads blob statuses, collects staged
// EULA texts and the browsed services, and reconciles the desired PAPPL
// queues that are dirty, in that order. The other queues keep their last
// results.
func (c *Core) cycle(ctx context.Context, wg *sync.WaitGroup) device.AgentReport {
	select { // this cycle takes every pending event
	case <-c.wake:
	default:
	}
	c.mu.Lock()
	d := c.dirty
	c.dirty = dirty{}
	c.mu.Unlock()

	r := device.AgentReport{
		PodUID: c.PodUID, Workload: c.Workload, Inventory: c.Inventory, ProbeNodes: c.ProbeNodes,
		Services: []device.DNSSDService{}, Queues: []device.QueueResult{}, Blobs: []device.BlobStatus{}, EULAs: []device.EULAText{},
	}
	if r.Inventory.Devices == nil {
		r.Inventory.Devices = []device.USBDevice{}
	}
	if c.Attached != nil {
		r.Attached = c.Attached()
	}
	if !reflect.DeepEqual(r.Attached, c.attached) {
		if added(c.attached, r.Attached) {
			// A device came: PAPPL may list it now, so skip the listed-nothing backoff.
			for _, cl := range c.papplClients {
				cl.DeviceChanged()
			}
		}
		d.all, d.reset = true, true
	}
	c.attached = r.Attached
	if c.Blobs != nil {
		if b := c.Blobs(); b != nil {
			r.Blobs = b
		}
	}
	types := device.BrowseTypes
	var stage []device.EULARef
	if c.desired != nil {
		r.AppliedHash, types, stage = c.desired.Hash, c.desired.Browse, c.desired.StageEULAs
	}
	if c.Stage != nil {
		if e := c.Stage(ctx, stage); e != nil {
			r.EULAs = e
		}
	}
	c.syncBrowse(ctx, wg, types)
	c.mu.Lock()
	if c.services != nil {
		r.Services = slices.Clone(c.services)
	}
	c.mu.Unlock()
	var qs []device.QueueSpec
	if c.desired != nil {
		qs = c.desired.Queues
	}
	r.Queues = c.reconcile(ctx, wg, qs, d, r.Attached)
	return r
}

// added reports whether now holds a device key that before lacks.
func added(before, now []device.USBDevice) bool {
	for _, d := range now {
		if !slices.ContainsFunc(before, func(b device.USBDevice) bool { return b.Key() == d.Key() }) {
			return true
		}
	}
	return false
}

// present reports whether the queue's device counts as attached for the
// pending retry: Attached unknown (nil), a device without a USB key (a queue
// found by DeviceID only), or its key in Attached.
func present(attached []device.USBDevice, dev device.USBDevice) bool {
	k := dev.Key()
	return attached == nil || k == (device.USBKey{}) ||
		slices.ContainsFunc(attached, func(a device.USBDevice) bool { return a.Key() == k })
}

// reconcile groups the queues by printer application (resolved system URL),
// keeps one PAPPL watch per URL, and reconciles each dirty group with its own
// timeout; clean groups keep their last results. The results come back in the
// order of qs. When it reconciled anything it re-arms the queue retry.
func (c *Core) reconcile(ctx context.Context, wg *sync.WaitGroup, qs []device.QueueSpec, d dirty, attached []device.USBDevice) []device.QueueResult {
	out := make([]device.QueueResult, len(qs))
	groups := map[string][]int{}
	var urls []string
	for i, q := range qs {
		url := q.SystemURI
		if url != "" && c.SystemURL != nil {
			url = c.SystemURL(url)
		}
		if url == "" {
			out[i] = device.QueueResult{ID: q.ID, State: device.QueueError, Message: MsgNoSystemURI}
			continue
		}
		if _, ok := groups[url]; !ok {
			urls = append(urls, url)
		}
		groups[url] = append(groups[url], i)
	}
	c.syncWatchers(ctx, wg, urls)
	maps.DeleteFunc(c.results, func(u string, _ urlResult) bool { _, ok := groups[u]; return !ok })

	ran := false
	for _, url := range urls {
		ids := make([]string, 0, len(groups[url]))
		for _, i := range groups[url] {
			ids = append(ids, qs[i].ID)
		}
		prev, ok := c.results[url]
		if ok && !d.all && !d.urls[url] && slices.Equal(prev.ids, ids) {
			for j, i := range groups[url] {
				out[i] = prev.res[j]
			}
			continue
		}
		wants := make([]device.QueueSpec, 0, len(groups[url]))
		for _, i := range groups[url] {
			wants = append(wants, qs[i])
		}
		rctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
		res := pappl.Reconcile(rctx, c.client(url), wants)
		cancel()
		for j, i := range groups[url] {
			out[i] = res[j]
		}
		c.results[url] = urlResult{ids: ids, res: res}
		ran = true
	}
	if ran {
		c.armRetry(qs, out, urls, groups, attached, d.reset)
	}
	return out
}

func (c *Core) client(url string) *pappl.Client {
	cl := c.papplClients[url]
	if cl == nil {
		cl = pappl.New(url)
		c.papplClients[url] = cl
	}
	return cl
}

// armRetry sets the queue retry after a reconcile: a queue error retries its
// URL after 1 s, doubling to 30 s per reconcile in a row with an error; a
// pending queue whose device is present after 30 s, doubling to 5 min. A
// pending queue whose USB device is known absent gets no timer: its device
// coming back is a USB event.
func (c *Core) armRetry(qs []device.QueueSpec, out []device.QueueResult, urls []string, groups map[string][]int, attached []device.USBDevice, reset bool) {
	if reset {
		c.errFails, c.pendFails = 0, 0
	}
	var errs, pend bool
	c.retryURLs = nil
	for _, url := range urls {
		hit := false
		for _, i := range groups[url] {
			switch {
			case out[i].State == device.QueueError:
				errs, hit = true, true
			case out[i].State == device.QueuePending && present(attached, qs[i].Device):
				pend, hit = true, true
			}
		}
		if hit {
			c.retryURLs = append(c.retryURLs, url)
		}
	}
	c.retry = nil
	switch {
	case errs:
		c.errFails, c.pendFails = c.errFails+1, 0
		c.retry = c.after(doubled(c.minBackoff, c.maxBackoff, c.errFails))
	case pend:
		c.errFails, c.pendFails = 0, c.pendFails+1
		c.retry = c.after(doubled(pendingRetry, pendingRetryMax, c.pendFails))
	default:
		c.errFails, c.pendFails = 0, 0
	}
}

// doubled is first doubled for each of n-1 failures, capped at most.
func doubled(first, most time.Duration, n int) time.Duration {
	d := first
	for i := 1; i < n && d < most; i++ {
		d *= 2
	}
	return min(d, most)
}

// syncWatchers keeps one PAPPL system watch per URL in urls and stops the
// others.
func (c *Core) syncWatchers(ctx context.Context, wg *sync.WaitGroup, urls []string) {
	for u, stop := range c.watchers {
		if !slices.Contains(urls, u) {
			stop()
			delete(c.watchers, u)
			c.mu.Lock()
			delete(c.unwatched, u)
			c.mu.Unlock()
		}
	}
	for _, u := range urls {
		if _, ok := c.watchers[u]; ok {
			continue
		}
		wctx, stop := context.WithCancel(ctx)
		c.watchers[u] = stop
		cl := c.client(u)
		wg.Go(func() { c.watchPAPPL(wctx, u, cl) })
	}
}

// watchPAPPL marks url's queues dirty on each PAPPL queue event and after
// every (re)subscription. Without subscriptions (ErrUnsupported) the URL's
// queues are re-checked at each heartbeat instead; a refused subscription
// (ErrRefused) does the same and subscribes again after one lease, by when
// leaked subscriptions have expired.
func (c *Core) watchPAPPL(ctx context.Context, url string, cl *pappl.Client) {
	key := "watch " + url
	for {
		err := cl.Watch(ctx, func() {
			c.problem(key, "", nil)
			c.poke(func(d *dirty) { d.url(url); d.reset = true })
		})
		if ctx.Err() != nil {
			return
		}
		c.setUnwatched(ctx, url, true)
		c.problem(key, "PAPPL events unavailable for "+url+"; its queues are re-checked at each report", err)
		if errors.Is(err, ippnotify.ErrUnsupported) || sleep(ctx, ippnotify.Lease*time.Second) != nil {
			return
		}
		c.setUnwatched(ctx, url, false)
	}
}

// setUnwatched records url's watch state unless its watch was stopped.
func (c *Core) setUnwatched(ctx context.Context, url string, v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() == nil {
		if v {
			c.unwatched[url] = true
		} else {
			delete(c.unwatched, url)
		}
	}
}

// syncBrowse keeps one Browse running for types; a change of types restarts
// it with an empty service list.
func (c *Core) syncBrowse(ctx context.Context, wg *sync.WaitGroup, types []string) {
	if c.Browse == nil || c.browsing && slices.Equal(types, c.browseTypes) {
		return
	}
	if c.browseStop != nil {
		c.browseStop()
		c.browseStop = nil
	}
	types = slices.Clone(types)
	c.browsing, c.browseTypes = true, types
	c.mu.Lock()
	c.browseGen++
	gen := c.browseGen
	c.services = nil
	c.mu.Unlock()
	if len(types) == 0 {
		return
	}
	bctx, stop := context.WithCancel(ctx)
	c.browseStop = stop
	wg.Go(func() { c.browse(bctx, gen, types) })
}

// browse runs Browse until ctx ends, storing each update and waking Loop. A
// broken watch empties the list and reconnects after 1 s, doubling to 30 s
// while it keeps failing before its first update.
func (c *Core) browse(ctx context.Context, gen int, types []string) {
	var backoff time.Duration
	for {
		var got atomic.Bool
		err := c.Browse(ctx, types, func(s []device.DNSSDService) {
			c.mu.Lock()
			current := c.browseGen == gen
			if current {
				c.services = slices.Clone(s)
			}
			c.mu.Unlock()
			if current {
				got.Store(true)
				c.problem("browse", "", nil)
				c.poke(nil)
			}
		})
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("DNS-SD watch stopped")
		}
		c.mu.Lock()
		if c.browseGen == gen {
			c.services = []device.DNSSDService{}
		}
		c.mu.Unlock()
		c.problem("browse", "DNS-SD browse failed", err)
		c.poke(nil)
		if got.Load() {
			backoff = 0
		}
		backoff = min(max(2*backoff, minBackoff), maxBackoff)
		if sleep(ctx, backoff) != nil {
			return
		}
	}
}

// sleep waits d or until ctx ends, returning ctx.Err() then.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// problem logs a recurring failure only when it starts and when it ends, so a
// daemon that stays down does not log every cycle. Safe from any goroutine.
// A recovery may pass an empty msg: it logs the one logged at the start.
func (c *Core) problem(key, msg string, err error) {
	c.mu.Lock()
	was, failing := c.failing[key]
	if err != nil && !failing {
		c.failing[key] = msg
	} else if err == nil {
		delete(c.failing, key)
	}
	c.mu.Unlock()
	switch {
	case err != nil && !failing:
		slog.Warn(msg, "err", err)
	case err == nil && failing:
		slog.Info(was + ": recovered")
	}
}

// Config holds the `agent run` flags.
type Config struct {
	ControlPlaneURL string       // --control-plane-url
	TokenFile       string       // --token-file, re-read on every report
	PodUID          string       // --pod-uid
	Workload        string       // --workload
	InventoryFile   string       // --inventory (a ProbeFile); "": empty inventory, nothing attached reported
	BlobStatusFile  string       // --blob-status, re-read when it changes; "": no blob reporting
	BlobDir         string       // --blob-dir, where EULA texts are staged; "": every EULA fails with MsgNoBlobVolume
	SysRoot         string       // --sys-root; "": /sys
	DBusAddress     string       // --dbus-address; "": no DNS-SD browsing
	Browse          Browser      // nil: Avahi over DBusAddress
	Client          *http.Client // nil: a client with a timeout of AgentWait plus 15 s
}

// Run reports to the control plane until ctx is done. It fails only on bad
// flags or an unreadable inventory file; everything else is retried.
func Run(ctx context.Context, cfg Config) error {
	core, report, err := newRun(cfg)
	if err != nil {
		return err
	}
	core.Loop(ctx, report)
	return nil
}

func newRun(cfg Config) (*Core, device.ReportFunc, error) {
	if cfg.ControlPlaneURL == "" || cfg.TokenFile == "" {
		return nil, nil, errors.New("agent run needs --control-plane-url and --token-file")
	}
	var pf ProbeFile
	if cfg.InventoryFile != "" {
		b, err := os.ReadFile(cfg.InventoryFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read inventory: %w", err)
		}
		if err := json.Unmarshal(b, &pf); err != nil {
			return nil, nil, fmt.Errorf("read inventory %s: %w", cfg.InventoryFile, err)
		}
	}
	if pf.Devices == nil {
		pf.Devices = []device.USBDevice{}
	}
	core := NewCore(cfg.PodUID, pf.Inventory)
	core.Workload = cfg.Workload
	core.ProbeNodes = pf.Nodes
	if cfg.InventoryFile != "" {
		sysRoot := cfg.SysRoot
		if sysRoot == "" {
			sysRoot = "/sys"
		}
		// The control plane follows moved, gone and new devices through this list.
		core.Attached = func() []device.USBDevice {
			devs, err := usb.Enumerate(sysRoot)
			if err != nil {
				return nil
			}
			return devs
		}
		core.USBEvents = usb.Watch
	}
	if cfg.BlobStatusFile != "" {
		core.Blobs = func() []device.BlobStatus {
			b, err := readBlobStatus(cfg.BlobStatusFile)
			core.problem("blob-status", "cannot read the blob status file "+cfg.BlobStatusFile, err)
			return b
		}
		core.BlobEvents = fileEvents(cfg.BlobStatusFile)
	}
	stager := &Stager{Dir: cfg.BlobDir, Wake: core.Wake}
	core.Stage = stager.Stage
	core.Browse = cfg.Browse
	if core.Browse == nil && cfg.DBusAddress != "" {
		core.Browse = avahiBrowser(cfg.DBusAddress)
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: device.AgentWait + 15*time.Second} // the hold plus the round trip
	}
	return core, httpReport(cfg.ControlPlaneURL, cfg.TokenFile, client), nil
}

// readBlobStatus reads the fetch-blobs status file: missing means no blobs yet,
// unreadable or malformed is an error and an empty list.
func readBlobStatus(path string) ([]device.BlobStatus, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path from the --blob-status flag
	if errors.Is(err, fs.ErrNotExist) {
		return []device.BlobStatus{}, nil
	}
	if err != nil {
		return []device.BlobStatus{}, err
	}
	var st []device.BlobStatus
	if err := json.Unmarshal(b, &st); err != nil {
		return []device.BlobStatus{}, err
	}
	if st == nil {
		st = []device.BlobStatus{}
	}
	return st, nil
}

func httpReport(baseURL, tokenFile string, client *http.Client) device.ReportFunc {
	url := strings.TrimSuffix(baseURL, "/") + device.AgentReportPath
	return func(ctx context.Context, r device.AgentReport) (device.DesiredState, error) {
		var ds device.DesiredState
		token, err := os.ReadFile(tokenFile) //nolint:gosec // path from the --token-file flag
		if err != nil {
			return ds, err
		}
		body, err := json.Marshal(r)
		if err != nil {
			return ds, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return ds, err
		}
		req.Header.Set("Authorization", device.TokenScheme+" "+strings.TrimSpace(string(token)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return ds, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return ds, fmt.Errorf("control plane answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ds); err != nil {
			return ds, fmt.Errorf("decode desired state: %w", err)
		}
		return ds, nil
	}
}

// avahiBrowser connects to the bus, watches Avahi until ctx ends or the
// watch breaks, and closes the connection; Loop reconnects after an error.
func avahiBrowser(addr string) Browser {
	return func(ctx context.Context, types []string, update func([]device.DNSSDService)) error {
		conn, err := dbus.Connect(addr)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		return dnssd.Watch(ctx, conn, types, update)
	}
}

// fileEvents watches path's directory and calls changed once the watch is in
// place (a write between the first read and the watch raised no event), when
// an entry named like path is written, created, renamed or removed (agent
// fetch replaces the file), and when the kernel dropped events. It returns an
// error when the directory cannot be watched or the watch breaks.
func fileEvents(path string) func(ctx context.Context, changed func()) error {
	return func(ctx context.Context, changed func()) error {
		w, err := fsnotify.NewWatcher()
		if err != nil {
			return err
		}
		defer func() { _ = w.Close() }()
		if err := w.Add(filepath.Dir(path)); err != nil {
			return err
		}
		changed()
		name := filepath.Clean(path)
		for {
			select {
			case <-ctx.Done():
				return nil
			case ev, ok := <-w.Events:
				if !ok {
					return errors.New("file watch closed")
				}
				if filepath.Clean(ev.Name) == name {
					changed()
				}
			case err, ok := <-w.Errors:
				if !ok {
					return errors.New("file watch closed")
				}
				if !errors.Is(err, fsnotify.ErrEventOverflow) {
					return err
				}
				changed()
			}
		}
	}
}
