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
// carry dev.pagewire.managed-by=pagewire. A foreign container or volume
// holding one of its names fails the rollout and is left alone.
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
	"slices"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/google/renameio/v2"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	corev1 "k8s.io/api/core/v1"

	"github.com/ThePhaseless/pagewire/internal/backend/k8s"
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
	Resync    time.Duration // health re-check interval; 0: 30s
}

const (
	defaultResync     = 30 * time.Second
	gateRecheck       = 5 * time.Second
	maxRetryWait      = 5 * time.Minute
	defaultHealthWait = 60 * time.Second
	msgNoPrinter      = "no USB printer on this host"
	printerClass      = 7
)

var (
	_ device.Backend          = (*Backend)(nil)
	_ device.PodRestarter     = (*Backend)(nil)
	_ device.AgentTokenSource = (*Backend)(nil)
)

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

	mu      sync.Mutex
	applied bool    // a successful Apply recorded want
	want    *wanted // nil: no workload
	restart bool
	gated   bool   // usb or probe spec and no printer on the host
	done    string // workload hash of the last finished rollout or adoption
	busy    string // current rollout step
	err     string // last rollout error
	cancel  context.CancelFunc
	logged  string // last error logged, so a stuck rollout logs once
	fails   int    // consecutive failed converges since the last success or new want

	wake chan struct{}

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
		wake: make(chan struct{}, 1),
	}
}

func (b *Backend) poke() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// Apply records spec as the wanted workload and wakes the worker. A failing
// Apply changes no backend state. A nil spec removes the workload.
func (b *Backend) Apply(ctx context.Context, spec *device.Spec) error {
	if spec == nil {
		b.mu.Lock()
		b.applied, b.want, b.fails = true, nil, 0
		if b.cancel != nil {
			b.cancel()
		}
		b.mu.Unlock()
		b.poke()
		return nil
	}
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
		BlobPVC: BlobVolume, SelfImage: self, PodUID: "docker-" + h[:12],
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
	if b.want == nil || b.want.hash != wh {
		b.fails = 0
		if b.cancel != nil {
			b.cancel()
		}
	}
	sc := *spec // the worker reads it later
	b.want = &wanted{spec: &sc, hash: wh, containers: cs, config: pod.Config, selfImage: self, token: tok}
	b.applied = true
	b.mu.Unlock()
	b.poke()
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

// RestartPod makes the worker roll the workload out again, probe included.
func (b *Backend) RestartPod(context.Context) error {
	b.mu.Lock()
	b.restart = true
	if b.cancel != nil {
		b.cancel()
	}
	b.mu.Unlock()
	b.poke()
	return nil
}

// Run reconciles until ctx is done: after every Apply or RestartPod, every
// Resync, and every 5 s while the USB gate is closed. It never tears the
// workload down on exit.
func (b *Backend) Run(ctx context.Context) {
	for {
		cctx, cancel := context.WithCancel(ctx)
		b.mu.Lock()
		b.cancel = cancel
		b.mu.Unlock()
		b.converge(cctx)
		b.mu.Lock()
		b.cancel = nil
		wait := retryWait(b.p.Resync, b.fails)
		if b.gated {
			wait = gateRecheck
		}
		b.mu.Unlock()
		cancel()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-b.wake:
		case <-t.C:
		}
		t.Stop()
	}
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
}

// fail records a rollout error; one caused by the cancelled ctx is dropped.
func (b *Backend) fail(ctx context.Context, err error) {
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
	return r.Items, nil
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
	w, restart := b.want, b.restart
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
	if usbMode(w.spec) && !b.printerPresent() {
		b.mu.Lock()
		b.gated, b.done = true, ""
		b.mu.Unlock()
		if err := b.teardown(ctx, owned); err != nil {
			b.fail(ctx, err)
		}
		return
	}
	b.mu.Lock()
	b.gated = false
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

// printerPresent reports whether any USB device has a printer-class
// interface. An unreadable sysfs counts as present, so it never blocks forever.
func (b *Backend) printerPresent() bool {
	devs, err := usb.Enumerate(b.p.SysRoot)
	if err != nil {
		b.warnSysfs.Do(func() { slog.Warn("read USB devices; starting without the printer check", "err", err) })
		return true
	}
	for _, d := range devs {
		for _, i := range d.Interfaces {
			if i.Class == printerClass {
				return true
			}
		}
	}
	return false
}

// healthy reports whether the owned containers are exactly the wanted
// workload and still up: run-once inits exited 0, the rest running or restarting.
func (b *Backend) healthy(ctx context.Context, owned []container.Summary, w *wanted) (bool, error) {
	var have, want []string
	for _, c := range owned {
		have = append(have, containerName(c))
	}
	for _, c := range w.containers {
		want = append(want, c.Opts.Name)
	}
	slices.Sort(have)
	slices.Sort(want)
	if !slices.Equal(have, want) {
		return false, nil
	}
	for _, c := range owned {
		if c.Labels[LabelWorkloadHash] != w.hash {
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

// waitHealthy polls the engine's health status. An engine that does not run
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

// Observe reports the device workload's state from the listed containers and
// the worker's progress. Endpoints stay empty: they come from agent reports.
func (b *Backend) Observe(ctx context.Context) (device.Observation, error) {
	owned, err := b.list(ctx)
	if err != nil {
		return device.Observation{}, fmt.Errorf("list device containers: %w", err)
	}
	obs := device.Observation{HostIP: b.HostIP(ctx)}
	b.mu.Lock()
	w, gated, done, applied := b.want, b.gated, b.done, b.applied
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
		obs.Phase = device.PhaseRunning
	}
	return obs, nil
}
