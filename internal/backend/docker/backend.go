// Package docker is the Docker Engine device backend, for Docker or rootful
// Podman through its Docker-compatible API. It runs the device workload that
// k8s.Plan describes as plain containers on the host network.
//
// Apply only records the wanted workload; Run's worker does the slow part
// (pulls, the probe, fetch-blobs) and Observe reports from the containers it
// lists without waiting on the worker. The worker does nothing before the
// first successful Apply, so a restarted control plane adopts a running
// workload instead of tearing it down.
//
// Ownership: the backend only stops or removes containers and volumes that
// carry io.github.thephaseless.pagewire.managed-by=pagewire and, for
// containers, whose names start with pagewire-device-. A foreign container or
// volume holding one of its names fails the rollout and is left alone.
package docker

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/google/renameio/v2"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
	corev1 "k8s.io/api/core/v1"

	"github.com/ThePhaseless/pagewire/internal/backend/k8s"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/usb"
)

// Params are the backend's inputs besides the Spec.
type Params struct {
	Registry  string
	Tag       string
	Overrides map[string]string
	AgentURL  string
	Container string        // the control plane's container, whose image ID is the self image
	HostIP    string        // "": the default route's source address, re-read per Observe
	TokenFile string        // the agent token, created on the first Apply
	SysRoot   string        // sysfs for the USB gate; "": /sys
	Resync    time.Duration // fallback re-check while Docker events are unavailable; 0: 30s
}

const (
	defaultResync     = 30 * time.Second
	gateRecheck       = 5 * time.Second
	maxRetryWait      = 5 * time.Minute
	defaultHealthWait = 60 * time.Second
	eventsRetry       = time.Second            // first Events reconnect wait, doubled to maxRetryWait
	eventsQuiet       = 100 * time.Millisecond // a new Events stream with no error by then is up
	eventsStable      = 30 * time.Second       // an Events stream up this long ends a failure streak
	msgNoPrinter      = "no USB printer or scanner on this host"
	msgNoDevice       = "waiting for the USB device nodes of " // + the entry ids, comma-separated
)

var (
	_ device.Backend          = (*Backend)(nil)
	_ device.PodRestarter     = (*Backend)(nil)
	_ device.AgentTokenSource = (*Backend)(nil)
	_ device.USBDeviceSetter  = (*Backend)(nil)
	_ device.Notifier         = (*Backend)(nil)
)

// lifecycleActions are the container event actions that can change what
// Observe reports or what converge does. The server-side event filter and the
// client-side guard both use them, so the avahi HEALTHCHECK's exec_* events,
// attach, CopyToContainer's extract-to-dir and archive-path, top and resize
// never wake anything.
var lifecycleActions = []string{
	"create", "start", "restart", "die", "stop", "kill", "oom", "destroy", "pause", "unpause", "rename", "health_status",
}

// EventFilters selects the lifecycle events of the backend's own containers:
// the server-side filters of the backend's Events stream. The Docker
// integration test opens a stream with them too.
func EventFilters() client.Filters {
	return make(client.Filters).Add("type", "container").
		Add("label", LabelManagedBy+"="+ManagedBy).
		Add("event", lifecycleActions...)
}

// routeIP returns the source address of the default route. Dialing UDP sends
// no packet; it only picks the route.
var routeIP = func(ctx context.Context) (string, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, "udp", "192.0.2.1:9")
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	a, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", fmt.Errorf("default route: unexpected local address %v", c.LocalAddr())
	}
	return a.IP.String(), nil
}

// wanted is the workload the last successful Apply recorded.
type wanted struct {
	spec       *device.Spec
	hash       string // workload hash
	containers []plannedContainer
	config     map[string]string
	selfImage  string
	token      string
}

// Backend runs the device workload through the Docker Engine API. It
// implements device.Backend, device.PodRestarter and device.AgentTokenSource.
type Backend struct {
	eng Engine
	p   Params

	healthTimeout time.Duration // how long a container with a healthcheck may take
	healthPoll    time.Duration

	selfMu sync.Mutex
	self   string

	mu       sync.Mutex
	applied  bool    // a successful Apply recorded want
	want     *wanted // nil: no workload
	restart  bool
	gated    bool               // usb or probe spec and no wanted device on the host
	usbDevs  []device.USBDevice // the controller's bound devices, sorted by Key
	nextDevs []device.USBDevice // usbDevs once the next Apply succeeds, if staged
	staged   bool               // nextDevs waits for the next Apply
	noDev    bool               // every USB daemon is left out: none of its devices has a node
	waiting  []string           // entry ids whose device has no usbfs node, sorted
	done     string             // workload hash of the last finished rollout or adoption
	busy     string             // current rollout step
	err      string             // last rollout error
	cancel   context.CancelFunc
	logged   string // last error logged, so a stuck rollout logs once
	fails    int    // consecutive failed converges since the last success or new want

	eventsOK  bool     // Notify's Events stream is up: no Resync timer
	ueventsOK bool     // Notify's USB uevent watch is running: no gateRecheck timer
	changed   func()   // Notify's changed; nil before Notify
	reported  progress // the worker state changed was last called for

	wake     chan struct{}
	softWake chan struct{} // softPoke; Run ignores it while fails > 0

	watchUSB     func(ctx context.Context, changed func()) error // usb.Watch; tests replace it
	eventsRetry  time.Duration
	eventsStable time.Duration

	ipMu      sync.Mutex
	lastIP    string
	ipFailing bool

	tokMu sync.Mutex
	tok   string

	warnSelf, warnHealth, warnSysfs sync.Once
}

// New returns a Backend. It makes no API call.
func New(eng Engine, p Params) *Backend {
	if p.Resync <= 0 {
		p.Resync = defaultResync
	}
	if p.SysRoot == "" {
		p.SysRoot = "/sys"
	}
	if p.Container == "" {
		p.Container = DefaultContainer
	}
	return &Backend{
		eng: eng, p: p,
		healthTimeout: defaultHealthWait, healthPoll: time.Second,
		wake:     make(chan struct{}, 1),
		softWake: make(chan struct{}, 1),
		watchUSB: usb.Watch, eventsRetry: eventsRetry, eventsStable: eventsStable,
	}
}

func (b *Backend) poke() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// softPoke wakes the worker unless it is backing off after a failure: the
// rollout's own container events and same-spec Applies would retry it hot.
func (b *Backend) softPoke() {
	select {
	case b.softWake <- struct{}{}:
	default:
	}
}

// Apply records the one spec as the wanted workload and wakes the worker. A
// failing Apply changes no backend state. Empty specs remove the workload; the
// host is one node-less workload, so more than one spec, or a spec naming a
// node, is an error.
func (b *Backend) Apply(ctx context.Context, specs []*device.Spec) error {
	switch {
	case len(specs) > 1:
		return fmt.Errorf("docker: %d device workloads; the host runs one", len(specs))
	case len(specs) == 1 && specs[0] == nil:
		return errors.New("docker: nil spec")
	case len(specs) == 1 && specs[0].Node != "":
		return fmt.Errorf("docker: spec for node %q; the host runs one node-less workload", specs[0].Node)
	}
	if len(specs) == 0 {
		b.mu.Lock()
		b.applied, b.want, b.fails = true, nil, 0
		b.takeStaged()
		if b.cancel != nil {
			b.cancel()
		}
		b.mu.Unlock()
		b.poke()
		return nil
	}
	spec := specs[0]
	h := spec.Hash
	if len(h) < 12 { // the pod UID takes 12 characters of it
		h = device.SpecHash(spec)
	}
	self, err := b.resolveSelf(ctx)
	if err != nil {
		return err
	}
	pod, err := k8s.Plan(spec, k8s.Params{
		Registry: b.p.Registry, Tag: b.p.Tag, Overrides: b.p.Overrides, AgentURL: b.p.AgentURL,
		BlobPVC: BlobVolume, SelfImage: self, PodUID: "docker-" + h[:12], BlobWorkload: true,
	})
	if err != nil {
		return err
	}
	cs, err := translate(pod)
	if err != nil {
		return err
	}
	wh, err := workloadHash(h, cs, pod.Config)
	if err != nil {
		return err
	}
	stampHash(cs, wh)
	tok, err := b.ensureToken()
	if err != nil {
		return err
	}

	b.mu.Lock()
	// The controller re-applies the same spec on every step, so only a new
	// spec or new devices may cut a failure backoff short.
	renewed := b.want == nil || b.want.hash != wh || b.staged
	if b.want == nil || b.want.hash != wh {
		b.fails = 0
		if b.cancel != nil {
			b.cancel()
		}
	}
	sc := *spec // the worker reads it later
	b.want = &wanted{spec: &sc, hash: wh, containers: cs, config: pod.Config, selfImage: self, token: tok}
	b.applied = true
	b.takeStaged()
	b.mu.Unlock()
	if renewed {
		b.poke()
	} else {
		b.softPoke()
	}
	return nil
}

// resolveSelf returns the image ID of the control plane's container, or the
// registry image when that container does not exist. Success is cached.
func (b *Backend) resolveSelf(ctx context.Context) (string, error) {
	b.selfMu.Lock()
	defer b.selfMu.Unlock()
	if b.self != "" {
		return b.self, nil
	}
	r, err := b.eng.ContainerInspect(ctx, b.p.Container, client.ContainerInspectOptions{})
	switch {
	case cerrdefs.IsNotFound(err):
		b.self = k8s.ImageRef(b.p.Registry, b.p.Tag, "pagewire", b.p.Overrides)
		b.warnSelf.Do(func() {
			slog.Warn("no container named "+b.p.Container+"; the device containers use the registry image",
				"image", b.self)
		})
	case err != nil:
		return "", fmt.Errorf("inspect container %s: %w", b.p.Container, err)
	case r.Container.Image == "":
		return "", fmt.Errorf("inspect container %s: no image ID", b.p.Container)
	default:
		b.self = r.Container.Image
	}
	return b.self, nil
}

// ensureToken returns the agent token, creating the file when it is missing
// or empty. An existing token is never changed, so a running agent keeps
// authenticating across control-plane restarts.
func (b *Backend) ensureToken() (string, error) {
	raw, err := os.ReadFile(b.p.TokenFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("agent token: %w", err) // never replace a token we could not read
	}
	if tok := strings.TrimSpace(string(raw)); tok != "" {
		return tok, nil
	}
	raw = make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	// Atomic, so a crash never leaves an empty token file behind.
	if err := renameio.WriteFile(b.p.TokenFile, []byte(tok), 0o600); err != nil {
		return "", fmt.Errorf("agent token: %w", err)
	}
	return tok, nil
}

func readToken(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the configured token file
	if err != nil {
		return "", fmt.Errorf("agent token: %w", err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", fmt.Errorf("agent token: %s is empty", path)
	}
	return tok, nil
}

// AgentToken returns the agent's bearer token. It is an error until the first
// Apply creates the token file; the first token read is cached.
func (b *Backend) AgentToken(context.Context) (string, error) {
	b.tokMu.Lock()
	defer b.tokMu.Unlock()
	if b.tok != "" {
		return b.tok, nil
	}
	tok, err := readToken(b.p.TokenFile)
	if err != nil {
		return "", err
	}
	b.tok = tok
	return tok, nil
}

// SetUSBDevices records the controller's bound devices, whose usbfs nodes the
// USB daemons get, and wakes the worker when they changed. With beforeApply it
// only stages devs for the next successful Apply.
func (b *Backend) SetUSBDevices(devs []device.USBDevice, beforeApply bool) {
	dc := make([]device.USBDevice, 0, len(devs))
	for _, d := range devs {
		d.Interfaces = slices.Clone(d.Interfaces)
		dc = append(dc, d)
	}
	// Sorted, so the same devices in another order are no change.
	slices.SortFunc(dc, func(x, y device.USBDevice) int {
		return cmp.Or(strings.Compare(x.Key().String(), y.Key().String()), cmp.Compare(x.Bus, y.Bus), cmp.Compare(x.Address, y.Address))
	})
	b.mu.Lock()
	if beforeApply {
		b.nextDevs, b.staged = dc, true
		b.mu.Unlock()
		return
	}
	b.nextDevs, b.staged = nil, false
	same := reflect.DeepEqual(b.usbDevs, dc)
	b.usbDevs = dc
	b.mu.Unlock()
	if !same {
		b.poke()
	}
}

// takeStaged commits staged devices; b.mu is held.
func (b *Backend) takeStaged() {
	if b.staged {
		b.usbDevs, b.nextDevs, b.staged = b.nextDevs, nil, false
	}
}

// RestartPod makes the worker roll the workload out again, probe included.
// The host's one workload is "".
func (b *Backend) RestartPod(_ context.Context, workload string) error {
	if workload != "" {
		return fmt.Errorf("docker: no workload %q; the host runs one node-less workload", workload)
	}
	b.mu.Lock()
	b.restart = true
	if b.cancel != nil {
		b.cancel()
	}
	b.mu.Unlock()
	b.poke()
	return nil
}

// Run reconciles until ctx is done: after every Apply, RestartPod, device
// change and Notify event; while backing off after a failure, only a new
// spec, new devices, RestartPod or a USB uevent end the wait early. Timers
// remain only as fallbacks: the failure backoff, every Resync while Docker
// events are unavailable, and every 5 s while a wanted USB device is missing
// and uevents are unavailable. It never tears the workload down on exit.
func (b *Backend) Run(ctx context.Context) {
	timed := false // the last wait ended on the fallback timer
	for {
		cctx, cancel := context.WithCancel(ctx)
		b.mu.Lock()
		b.cancel = cancel
		b.mu.Unlock()
		b.converge(cctx)
		b.mu.Lock()
		b.cancel = nil
		wait := b.fallbackWait()
		var soft <-chan struct{}
		if b.fails == 0 {
			soft = b.softWake
		}
		var recheck func() // without Docker events the controller re-observes on each fallback re-check too
		if timed && !b.eventsOK {
			recheck = b.changed
		}
		b.mu.Unlock()
		cancel()
		b.progressChanged()
		if recheck != nil {
			recheck()
		}
		var tc <-chan time.Time
		var t *time.Timer
		if wait > 0 {
			t = time.NewTimer(wait)
			tc = t.C
		}
		timed = false
		select {
		case <-ctx.Done():
		case <-b.wake:
		case <-soft:
		case <-tc:
			timed = true
		}
		if t != nil {
			t.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// fallbackWait is how long Run sleeps without a wake; 0: no timer. b.mu is held.
func (b *Backend) fallbackWait() time.Duration {
	// Without uevents, sysfs is re-read until a missing device appears; a
	// failing rollout keeps its backoff unless the whole workload is gated.
	missing := b.noDev || len(b.waiting) > 0
	switch {
	case b.gated && !b.ueventsOK:
		return gateRecheck
	case b.fails > 0:
		return retryWait(b.p.Resync, b.fails)
	case missing && !b.ueventsOK:
		return gateRecheck
	case !b.eventsOK:
		return b.p.Resync
	}
	return 0
}

// progress is the worker state Observe reports from.
type progress struct {
	busy, err, done string
	gated, noDev    bool
	waiting         string
}

// progressChanged calls Notify's changed when the worker state differs from
// the last call, so the controller, which no longer polls, sees each step.
func (b *Backend) progressChanged() {
	b.mu.Lock()
	p := progress{b.busy, b.err, b.done, b.gated, b.noDev, strings.Join(b.waiting, ",")}
	same, changed := p == b.reported, b.changed
	b.reported = p
	b.mu.Unlock()
	if !same && changed != nil {
		changed()
	}
}

// setFlag sets an event-source flag and wakes the worker when it flipped, so
// Run re-arms or drops its fallback timer.
func (b *Backend) setFlag(f *bool, v bool) {
	b.mu.Lock()
	flipped := *f != v
	*f = v
	b.mu.Unlock()
	if flipped {
		b.softPoke()
	}
}

// Notify wakes the worker and calls changed on every lifecycle event of the
// backend's containers (Docker Engine /events) and every USB uevent, until
// ctx is done. While a source is unavailable, Run falls back to its timer and
// degraded names it.
func (b *Backend) Notify(ctx context.Context, changed func(), degraded func(msg string)) {
	b.mu.Lock()
	b.changed = changed
	b.mu.Unlock()
	pr := &problems{msgs: map[string]string{}, degraded: degraded}
	var wg sync.WaitGroup
	wg.Go(func() { b.watchEvents(ctx, func() { b.softPoke(); changed() }, pr) })
	wg.Go(func() { b.watchUSBEvents(ctx, func() { b.poke(); changed() }, pr) })
	wg.Wait()
}

// problems joins the messages of the degraded event sources, in a fixed order.
type problems struct {
	mu       sync.Mutex // held across degraded, so two sources never deliver out of order
	msgs     map[string]string
	last     string
	degraded func(string)
}

func (p *problems) set(source, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs[source] = msg
	var all []string
	for _, s := range []string{"events", "usb"} {
		if m := p.msgs[s]; m != "" {
			all = append(all, m)
		}
	}
	if j := strings.Join(all, "; "); j != p.last {
		p.last = j
		p.degraded(j)
	}
}

// watchEvents follows the Events stream, reconnecting with backoff. A
// stream that stays up eventsStable ends the failure streak: it resets the
// backoff and clears the degraded message, so a stream a proxy keeps closing
// logs and degrades once and reconnects ever more slowly.
//
// shortcut: a stream a proxy closes only after eventsStable or longer still
// logs a Warn and Info pair and flips the backend-events item every cycle,
// and after a single drop the degraded text lingers up to eventsStable;
// accepted because this backend is scheduled for removal, upgrade to a
// decaying failure count if it outlives that.
func (b *Backend) watchEvents(ctx context.Context, wake func(), pr *problems) {
	defer b.setFlag(&b.eventsOK, false)
	delay, failing := b.eventsRetry, false
	for {
		err := b.streamEvents(ctx, wake, func() {
			b.setFlag(&b.eventsOK, true)
			wake() // events may have been missed while disconnected
		}, func() {
			delay = b.eventsRetry
			pr.set("events", "")
			if failing {
				failing = false
				slog.Info("Docker events available again")
			}
		})
		if ctx.Err() != nil {
			return
		}
		b.setFlag(&b.eventsOK, false)
		if !failing {
			failing = true
			slog.Warn("Docker events unavailable; falling back to a periodic re-check", "err", err, "every", b.p.Resync)
		}
		pr.set("events", fmt.Sprintf("Docker events unavailable (%v); re-checking every %g s — allow GET /events in the socket proxy", err, b.p.Resync.Seconds()))
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		delay = min(delay*2, maxRetryWait)
	}
}

// streamEvents reads one Events stream until it fails or ctx ends. connected
// runs once the stream is up: on its first message, or when no error came
// within eventsQuiet (the client reports a refused request just after it
// returns). stable runs once it has been up for eventsStable.
func (b *Backend) streamEvents(ctx context.Context, wake, connected, stable func()) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r := b.eng.Events(sctx, client.EventsListOptions{Filters: EventFilters()})
	quiet := time.NewTimer(eventsQuiet)
	defer quiet.Stop()
	steady := time.NewTimer(b.eventsStable)
	steady.Stop()
	defer steady.Stop()
	var steadyC <-chan time.Time // nil until up
	up := false
	setUp := func() {
		if !up {
			up = true
			connected()
			steady.Reset(b.eventsStable)
			steadyC = steady.C
		}
	}
	lastHealth := map[string]string{} // container ID → health status; reset per stream
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-r.Err:
			if err == nil { // closed after its one error
				err = errors.New("events stream closed")
			}
			return err
		case <-quiet.C:
			setUp()
		case <-steadyC:
			steadyC = nil
			stable()
		case m := <-r.Messages:
			setUp()
			if passEvent(m, lastHealth) {
				wake()
			}
		}
	}
}

// passEvent is the client-side guard: it passes only lifecycle actions, and a
// health_status only when the container's status changed (Podman logs one per
// healthcheck run, Docker one per change).
//
// shortcut: the server-side event filter is assumed to match the action
// before its first ":" on Docker and Podman; if TestDockerBackend's
// self-heal sees no die or destroy on its EventFilters stream on an engine,
// drop the "event" terms from EventFilters and rely on this guard.
func passEvent(m events.Message, lastHealth map[string]string) bool {
	action, status, _ := strings.Cut(string(m.Action), ":") // "health_status: healthy"
	action = strings.TrimSpace(action)
	if !slices.Contains(lifecycleActions, action) {
		return false
	}
	id := m.Actor.ID
	switch action {
	case "destroy":
		delete(lastHealth, id)
	case "health_status":
		status = cmp.Or(strings.TrimSpace(status), m.Actor.Attributes["health_status"])
		if prev, ok := lastHealth[id]; ok && prev == status {
			return false
		}
		lastHealth[id] = status
	}
	return true
}

// watchUSBEvents wakes on USB uevents. An open failure never recovers within
// the process, so its degraded note stays.
func (b *Backend) watchUSBEvents(ctx context.Context, wake func(), pr *problems) {
	defer b.setFlag(&b.ueventsOK, false)
	b.setFlag(&b.ueventsOK, true)
	err := b.watchUSB(ctx, wake)
	if ctx.Err() != nil || err == nil {
		return
	}
	b.setFlag(&b.ueventsOK, false)
	slog.Warn("USB events unavailable; re-checking every 5 s while a device is missing", "err", err)
	pr.set("usb", fmt.Sprintf("USB events unavailable (%v); re-checking every %g s", err, gateRecheck.Seconds()))
}

// retryWait backs off like CrashLoopBackOff: resync doubled per consecutive
// failure after the first, capped at maxRetryWait (or resync, if longer).
func retryWait(resync time.Duration, fails int) time.Duration {
	wait := resync
	for i := 1; i < fails && wait < maxRetryWait; i++ {
		wait *= 2
	}
	return min(wait, max(resync, maxRetryWait))
}

func (b *Backend) setBusy(msg string) {
	b.mu.Lock()
	b.busy = msg
	b.mu.Unlock()
	b.progressChanged()
}

// fail records a rollout error; one caused by the cancelled ctx is dropped.
func (b *Backend) fail(ctx context.Context, err error) {
	defer b.progressChanged()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.busy = ""
	if ctx.Err() != nil {
		return
	}
	b.err = err.Error()
	b.fails++
	if b.err != b.logged {
		b.logged = b.err
		slog.Warn("device workload", "err", err)
	}
}

func (b *Backend) list(ctx context.Context) ([]container.Summary, error) {
	r, err := b.eng.ContainerList(ctx, client.ContainerListOptions{
		All: true, Filters: make(client.Filters).Add("label", LabelManagedBy+"="+ManagedBy),
	})
	if err != nil {
		return nil, err
	}
	// The label alone is not enough to stop or remove a container.
	return slices.DeleteFunc(r.Items, func(c container.Summary) bool {
		return !strings.HasPrefix(containerName(c), Prefix)
	}), nil
}

// attached lists the host's USB devices; ok is false when sysfs is
// unreadable, which is warned once.
func (b *Backend) attached() (devs []device.USBDevice, ok bool) {
	devs, err := usb.Enumerate(b.p.SysRoot)
	if err != nil {
		b.warnSysfs.Do(func() { slog.Warn("read USB devices; starting without the device check", "err", err) })
		return nil, false
	}
	return devs, true
}

// deviceNode returns the usbfs node of d as it is attached now (host lists
// the attached devices; ok false: sysfs unreadable): the same vendor, product
// and serial, at its recorded address first. A replugged device has a new
// address; it is taken only when it is the one such device, so two identical
// devices wait for the controller's next match. "" when d is not attached or
// ambiguous. With sysfs unreadable it trusts the recorded address.
func deviceNode(d device.USBDevice, host []device.USBDevice, ok bool) string {
	node := func(x device.USBDevice) string { return fmt.Sprintf("%s/%03d/%03d", usbDevDir, x.Bus, x.Address) }
	if !ok {
		return node(d)
	}
	same := func(x device.USBDevice) bool { return x.Key() == d.Key() }
	if i := slices.IndexFunc(host, func(x device.USBDevice) bool { return same(x) && x.Bus == d.Bus && x.Address == d.Address }); i >= 0 {
		return node(host[i])
	}
	host = slices.DeleteFunc(slices.Clone(host), func(x device.USBDevice) bool { return !same(x) })
	if len(host) != 1 {
		return ""
	}
	return node(host[0])
}

// deviceNodes maps the key of each of devs to its usbfs nodes now, leaving
// out devices with none.
func deviceNodes(devs, host []device.USBDevice, ok bool) map[device.USBKey][]string {
	nodes := map[device.USBKey][]string{}
	for _, d := range devs {
		if n := deviceNode(d, host, ok); n != "" {
			nodes[d.Key()] = append(nodes[d.Key()], n)
		}
	}
	return nodes
}

// specKeys returns the USB keys of s's entries, without the zero key of
// simulated ones, and the entry ids of each key.
func specKeys(s *device.Spec) map[device.USBKey][]string {
	ids := map[device.USBKey][]string{}
	for _, p := range s.Printers {
		if p.USB != (device.USBKey{}) {
			ids[p.USB] = append(ids[p.USB], p.ID)
		}
	}
	for _, sc := range s.Scanners {
		if sc.USB != (device.USBKey{}) {
			ids[sc.USB] = append(ids[sc.USB], sc.ID)
		}
	}
	return ids
}

func usbMode(s *device.Spec) bool { return s.Mode == device.ModeUSB || s.Mode == device.ModeProbe }

// converge makes the containers match the last Apply. It does nothing before
// the first successful Apply.
func (b *Backend) converge(ctx context.Context) {
	b.mu.Lock()
	if !b.applied {
		b.mu.Unlock()
		return
	}
	w, restart, devs := b.want, b.restart, b.usbDevs
	b.restart = false
	b.mu.Unlock()
	rolled := false
	defer func() {
		if restart && !rolled { // a RestartPod is never lost to a failed or cancelled converge
			b.mu.Lock()
			b.restart = true
			b.mu.Unlock()
		}
	}()

	owned, err := b.list(ctx)
	if err != nil {
		b.fail(ctx, fmt.Errorf("list device containers: %w", err))
		return
	}
	if w == nil {
		b.mu.Lock()
		b.gated = false
		b.mu.Unlock()
		if err := b.teardown(ctx, owned); err != nil {
			b.fail(ctx, err)
			return
		}
		b.mu.Lock()
		b.done, b.err, b.busy, b.fails = "", "", "", 0
		b.mu.Unlock()
		return
	}
	var host []device.USBDevice
	hostOK := false // a simulated spec has no USB daemon and needs no sysfs
	if usbMode(w.spec) {
		host, hostOK = b.attached()
	}
	if usbMode(w.spec) && !wantedPresent(w.spec, host, hostOK) {
		b.mu.Lock()
		b.gated, b.noDev, b.waiting, b.done = true, false, nil, ""
		b.mu.Unlock()
		b.progressChanged()
		if err := b.teardown(ctx, owned); err != nil {
			b.fail(ctx, err)
		}
		return
	}
	// Each USB daemon runs with its own devices' nodes or not at all; the
	// probe and agent still run, so a replugged or new device is reported.
	ew := *w
	nodes := deviceNodes(devs, host, hostOK)
	ew.containers = withDevice(w.containers, nodes)
	var waiting []string
	for k, ids := range specKeys(w.spec) {
		if len(nodes[k]) == 0 {
			waiting = append(waiting, ids...)
		}
	}
	slices.Sort(waiting)
	daemons := func(cs []plannedContainer) int {
		n := 0
		for _, c := range cs {
			if len(c.usb) > 0 {
				n++
			}
		}
		return n
	}
	noDev := daemons(w.containers) > 0 && daemons(ew.containers) == 0
	w = &ew
	b.mu.Lock()
	b.gated, b.noDev, b.waiting = false, noDev, waiting
	b.mu.Unlock()

	if !restart {
		ok, err := b.healthy(ctx, owned, w)
		if err != nil {
			b.fail(ctx, err)
			return
		}
		if ok {
			b.mu.Lock()
			b.done, b.err, b.fails = w.hash, "", 0
			b.mu.Unlock()
			return
		}
	}

	b.mu.Lock()
	b.done, b.busy = "", "starting"
	b.mu.Unlock()
	b.progressChanged()
	if err := b.rollout(ctx, owned, w); err != nil {
		b.fail(ctx, err)
		return
	}
	rolled = true
	b.mu.Lock()
	b.done, b.err, b.busy, b.logged, b.fails = w.hash, "", "", "", 0
	b.mu.Unlock()
	slog.Info("device workload started", "mode", w.spec.Mode, "workloadHash", w.hash[:12])
}

func (b *Backend) rollout(ctx context.Context, owned []container.Summary, w *wanted) error {
	if err := b.teardown(ctx, owned); err != nil {
		return err
	}
	if err := b.ensureImages(ctx, w); err != nil {
		return err
	}
	b.setBusy("starting")
	if err := b.ensureVolumes(ctx, w); err != nil {
		return err
	}
	for _, c := range w.containers {
		if _, err := b.eng.ContainerCreate(ctx, c.Opts); err != nil {
			return fmt.Errorf("create %s: %w", c.Opts.Name, err)
		}
	}
	if err := b.seed(ctx, w); err != nil {
		return err
	}
	return b.start(ctx, w)
}

// wantedPresent reports whether host has a device s wants: in probe mode any
// device that can be a printer or scanner, else any device of s's entries. An
// unreadable sysfs (ok false) counts as present, so it never blocks forever.
func wantedPresent(s *device.Spec, host []device.USBDevice, ok bool) bool {
	if !ok {
		return true
	}
	keys := specKeys(s)
	cat := catalog.Default()
	return slices.ContainsFunc(host, func(d device.USBDevice) bool {
		if s.Mode == device.ModeProbe {
			p, sc := cat.Roles(d)
			return p || sc
		}
		return len(keys[d.Key()]) > 0
	})
}

// healthy reports whether the owned containers are exactly the wanted
// workload and still up: run-once inits exited 0, the rest running or restarting.
func (b *Backend) healthy(ctx context.Context, owned []container.Summary, w *wanted) (bool, error) {
	want := map[string]string{} // name: usbfs nodes
	for _, c := range w.containers {
		want[c.Opts.Name] = c.Opts.Config.Labels[LabelUSBDevice]
	}
	if len(owned) != len(want) {
		return false, nil
	}
	for _, c := range owned {
		node, ok := want[containerName(c)]
		if !ok || c.Labels[LabelWorkloadHash] != w.hash || c.Labels[LabelUSBDevice] != node {
			return false, nil
		}
		if c.Labels[LabelInit] != "true" {
			if c.State != container.StateRunning && c.State != container.StateRestarting {
				return false, nil
			}
			continue
		}
		if c.State != container.StateExited {
			return false, nil
		}
		r, err := b.eng.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if cerrdefs.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect %s: %w", containerName(c), err)
		}
		if r.Container.State == nil || r.Container.State.ExitCode != 0 {
			return false, nil
		}
	}
	return true, nil
}

func containerName(c container.Summary) string {
	if len(c.Names) == 0 {
		return c.ID
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// teardown stops and removes the owned containers, mains before inits, with
// their anonymous volumes, then the owned per-rollout volumes. pagewire-blobs
// is never removed.
func (b *Backend) teardown(ctx context.Context, owned []container.Summary) error {
	ordered := slices.Clone(owned)
	slices.SortStableFunc(ordered, func(x, y container.Summary) int {
		return strings.Compare(x.Labels[LabelInit], y.Labels[LabelInit]) // "false" and "" before "true"
	})
	timeout := stopTimeout
	for _, c := range ordered {
		if _, err := b.eng.ContainerStop(ctx, c.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("stop %s: %w", containerName(c), err)
		}
		_, err := b.eng.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		if err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("remove %s: %w", containerName(c), err)
		}
	}
	for _, v := range rolloutVolumes() {
		r, err := b.eng.VolumeInspect(ctx, v.Name, client.VolumeInspectOptions{})
		switch {
		case cerrdefs.IsNotFound(err):
			continue
		case err != nil:
			return fmt.Errorf("inspect volume %s: %w", v.Name, err)
		case r.Volume.Labels[LabelManagedBy] != ManagedBy:
			continue // foreign: never removed; ensureVolumes reports it
		}
		if _, err := b.eng.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("remove volume %s: %w", v.Name, err)
		}
	}
	return nil
}

// ensureImages pulls what the rollout needs: an image ID never, an undigested
// :main ref always, any other ref only when it is missing.
func (b *Backend) ensureImages(ctx context.Context, w *wanted) error {
	seen := map[string]bool{}
	for _, c := range w.containers {
		ref := c.Opts.Config.Image
		if seen[ref] || strings.HasPrefix(ref, "sha256:") {
			continue
		}
		seen[ref] = true
		if k8s.PullPolicy(ref) != corev1.PullAlways {
			_, err := b.eng.ImageInspect(ctx, ref)
			if err == nil {
				continue
			}
			if !cerrdefs.IsNotFound(err) {
				return fmt.Errorf("inspect image %s: %w", ref, err)
			}
		}
		b.setBusy("pulling " + ref)
		if err := b.pull(ctx, ref); err != nil {
			return fmt.Errorf("pull %s: %w", ref, err)
		}
	}
	return nil
}

func (b *Backend) pull(ctx context.Context, ref string) error {
	resp, err := b.eng.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Close() }()
	return resp.Wait(ctx)
}

// ensureVolumes recreates the owned per-rollout volumes, refuses foreign ones
// with the same names, and creates pagewire-blobs when it is mounted and missing.
func (b *Backend) ensureVolumes(ctx context.Context, w *wanted) error {
	for _, v := range rolloutVolumes() {
		r, err := b.eng.VolumeInspect(ctx, v.Name, client.VolumeInspectOptions{})
		switch {
		case cerrdefs.IsNotFound(err):
		case err != nil:
			return fmt.Errorf("inspect volume %s: %w", v.Name, err)
		case r.Volume.Labels[LabelManagedBy] != ManagedBy:
			return fmt.Errorf("volume %s exists and is not labelled %s=%s; remove or rename it", v.Name, LabelManagedBy, ManagedBy)
		default:
			if _, err := b.eng.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{}); err != nil {
				return fmt.Errorf("remove volume %s: %w", v.Name, err)
			}
		}
		if _, err := b.eng.VolumeCreate(ctx, client.VolumeCreateOptions{
			Name: v.Name, Driver: "local", DriverOpts: v.DriverOpts, Labels: ownerLabels(),
		}); err != nil {
			return fmt.Errorf("create volume %s: %w", v.Name, err)
		}
	}
	if !mountsBlobs(w) {
		return nil
	}
	_, err := b.eng.VolumeInspect(ctx, BlobVolume, client.VolumeInspectOptions{})
	if err == nil {
		return nil // ours or the user's: used as is
	}
	if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect volume %s: %w", BlobVolume, err)
	}
	if _, err := b.eng.VolumeCreate(ctx, client.VolumeCreateOptions{Name: BlobVolume, Driver: "local", Labels: ownerLabels()}); err != nil {
		return fmt.Errorf("create volume %s: %w", BlobVolume, err)
	}
	return nil
}

func mountsBlobs(w *wanted) bool {
	for _, c := range w.containers {
		for _, m := range c.Opts.HostConfig.Mounts {
			if m.Source == BlobVolume {
				return true
			}
		}
	}
	return false
}

// seed copies the config files and the token into their volumes through a
// never-started container, then removes it.
func (b *Backend) seed(ctx context.Context, w *wanted) error {
	if _, err := b.eng.ContainerCreate(ctx, seedContainer(w.selfImage)); err != nil {
		return fmt.Errorf("create %s: %w", SeedContainer, err)
	}
	err := b.copyFiles(ctx, seedConfigDir, w.config)
	if err == nil {
		err = b.copyFiles(ctx, seedTokenDir, map[string]string{"token": w.token})
	}
	_, rmErr := b.eng.ContainerRemove(ctx, SeedContainer, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if rmErr != nil && !cerrdefs.IsNotFound(rmErr) {
		rmErr = fmt.Errorf("remove %s: %w", SeedContainer, rmErr)
	} else {
		rmErr = nil
	}
	return errors.Join(err, rmErr)
}

func (b *Backend) copyFiles(ctx context.Context, dir string, files map[string]string) error {
	buf, err := tarFiles(files)
	if err != nil {
		return err
	}
	if _, err := b.eng.CopyToContainer(ctx, SeedContainer, client.CopyToContainerOptions{DestinationPath: dir, Content: buf}); err != nil {
		return fmt.Errorf("copy to %s:%s: %w", SeedContainer, dir, err)
	}
	return nil
}

// start starts the containers in plan order. A run-once init is waited for
// and must exit 0; a container with a healthcheck must turn healthy first.
func (b *Backend) start(ctx context.Context, w *wanted) error {
	for _, c := range w.containers {
		id := c.Opts.Name
		if _, err := b.eng.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
			return fmt.Errorf("start %s: %w", c.Name, err)
		}
		switch {
		case c.RunOnce:
			if err := b.waitExit(ctx, c.Name, id); err != nil {
				return err
			}
		case !slices.Equal(c.Opts.Config.Healthcheck.Test, []string{"NONE"}):
			if err := b.waitHealthy(ctx, c.Name, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Backend) waitExit(ctx context.Context, name, id string) error {
	r := b.eng.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-r.Result:
		if res.StatusCode != 0 {
			return fmt.Errorf("%s: exited with code %d", name, res.StatusCode)
		}
		return nil
	case err := <-r.Error:
		return fmt.Errorf("wait for %s: %w", name, err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitHealthy polls the engine's health status, once per healthPoll for at
// most healthTimeout of one rollout step: Docker sends health_status events
// only on a change and Podman on every run, so an inspect loop is the one
// answer both engines give alike. An engine that does not run
// the check (no State.Health, or no result and no failure by the deadline)
// does not block the rollout; that is warned once.
func (b *Backend) waitHealthy(ctx context.Context, name, id string) error {
	deadline := time.Now().Add(b.healthTimeout)
	skip := func() error {
		b.warnHealth.Do(func() {
			slog.Warn("the container engine does not run healthchecks; starting without waiting for " + name)
		})
		return nil
	}
	for {
		r, err := b.eng.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		var h *container.Health
		if r.Container.State != nil {
			h = r.Container.State.Health
		}
		if h == nil {
			return skip()
		}
		if h.Status == container.Healthy {
			return nil
		}
		if !time.Now().Before(deadline) {
			if len(h.Log) == 0 && h.FailingStreak == 0 {
				return skip()
			}
			return fmt.Errorf("%s: not healthy after %v (%s)", name, b.healthTimeout, h.Status)
		}
		t := time.NewTimer(b.healthPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// HostIP is the address the device serves on: Params.HostIP, else the
// default route's source address, else the last good one, else 127.0.0.1.
func (b *Backend) HostIP(ctx context.Context) string {
	if b.p.HostIP != "" {
		return b.p.HostIP
	}
	ip, err := routeIP(ctx)
	b.ipMu.Lock()
	defer b.ipMu.Unlock()
	if err == nil && ip != "" {
		b.lastIP, b.ipFailing = ip, false
		return ip
	}
	if !b.ipFailing {
		b.ipFailing = true
		slog.Warn("find the host address from the default route; set --host-ip", "err", err)
	}
	if b.lastIP != "" {
		return b.lastIP
	}
	return "127.0.0.1"
}

// Observe reports the host's one device workload (Workload "") from the
// listed containers and the worker's progress. Endpoints stay empty: they come
// from agent reports. It is pending while no USB daemon has a device node;
// with only some missing it follows the containers, and the message names the
// entries still waiting, so a switched-off printer never holds up its siblings.
func (b *Backend) Observe(ctx context.Context) ([]device.Observation, error) {
	owned, err := b.list(ctx)
	if err != nil {
		return nil, fmt.Errorf("list device containers: %w", err)
	}
	obs := device.Observation{HostIP: b.HostIP(ctx)}
	b.mu.Lock()
	w, gated, noDev, done, applied := b.want, b.gated, b.noDev, b.done, b.applied
	waitMsg := ""
	if w != nil && len(b.waiting) > 0 {
		waitMsg = msgNoDevice + strings.Join(b.waiting, ", ")
	}
	msg := cmp.Or(b.err, b.busy, "starting")
	b.mu.Unlock()

	// Only workload containers count; the seed container has no name label.
	// down: long-running containers not running; stopped: of those, the ones
	// not restarting either.
	var down []container.Summary
	n, stopped := 0, 0
	for _, c := range owned {
		if c.Labels[LabelContainer] == "" {
			continue
		}
		n++
		if c.Labels[LabelInit] == "true" || c.State == container.StateRunning {
			continue
		}
		down = append(down, c)
		if c.State != container.StateRestarting {
			stopped++
		}
	}
	switch {
	case w != nil && gated:
		obs.Phase, obs.Unschedulable, obs.Message = device.PhasePending, true, msgNoPrinter
	case w != nil && done != w.hash:
		obs.Phase, obs.Message = device.PhasePending, msg
	case w != nil && noDev:
		obs.Phase, obs.Message = device.PhasePending, waitMsg
	case n == 0:
		obs.Phase = device.PhaseAbsent
	case !applied && stopped > 0:
		// After a host reboot the on-failure containers come back exited:
		// Absent makes the controller probe and roll out at once.
		obs.Phase = device.PhaseAbsent
	case len(down) > 0:
		slices.SortFunc(down, func(x, y container.Summary) int {
			return strings.Compare(x.Labels[LabelContainer], y.Labels[LabelContainer])
		})
		c := down[0]
		obs.Phase = device.PhasePending
		obs.Message = c.Labels[LabelContainer] + ": " + string(c.State)
		if c.State == container.StateExited {
			if r, err := b.eng.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{}); err == nil && r.Container.State != nil {
				obs.Message += fmt.Sprintf(" (exit %d)", r.Container.State.ExitCode)
			}
		}
	default:
		obs.Phase, obs.Message = device.PhaseRunning, waitMsg
	}
	return []device.Observation{obs}, nil
}
