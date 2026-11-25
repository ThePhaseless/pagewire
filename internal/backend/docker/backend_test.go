package docker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/ThePhaseless/pagewire/internal/device"
)

const testHostIP = "192.0.2.10"

// sysfs builds a fake /sys with one USB device at bus 1 address 7: the HP
// printer of keyHP when printer is true, else a keyboard.
func sysfs(t *testing.T, printer bool) string {
	t.Helper()
	root := t.TempDir()
	setPrinter(t, root, printer)
	return root
}

func setPrinter(t *testing.T, root string, printer bool) {
	t.Helper()
	if printer {
		addUSB(t, root, "1-1", keyHP, 7, "07")
		return
	}
	addUSB(t, root, "1-1", device.USBKey{VendorID: "046d", ProductID: "c31c"}, 7, "03")
}

// addUSB writes (or rewrites) sysfs device dir with key k at bus 1 address
// devnum and one interface of class (hex).
func addUSB(t *testing.T, root, dir string, k device.USBKey, devnum int, class string) {
	t.Helper()
	dev := filepath.Join(root, "bus", "usb", "devices", dir)
	write := func(dir string, attrs map[string]string) {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		for k, v := range attrs {
			if err := os.WriteFile(filepath.Join(dir, k), []byte(v+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(dev, map[string]string{"idVendor": k.VendorID, "idProduct": k.ProductID, "serial": k.Serial, "busnum": "1", "devnum": strconv.Itoa(devnum)})
	write(filepath.Join(dev, dir+":1.0"), map[string]string{
		"bInterfaceNumber": "00", "bInterfaceClass": class, "bInterfaceSubClass": "01", "bInterfaceProtocol": "02",
	})
}

func testParams(t *testing.T) Params {
	t.Helper()
	return Params{
		Registry: "ghcr.io/thephaseless", Tag: "1.2.3", AgentURL: "http://127.0.0.1:8081",
		HostIP: testHostIP, TokenFile: filepath.Join(t.TempDir(), "agent-token"), SysRoot: sysfs(t, true),
	}
}

// testPrinter is the controller's bound device for the printer in sysfs(t, true).
func testPrinter() device.USBDevice {
	return device.USBDevice{VendorID: keyHP.VendorID, ProductID: keyHP.ProductID, Serial: keyHP.Serial, Bus: 1, Address: 7}
}

func newTestBackend(f *fakeEngine, p Params) *Backend {
	b := New(f, p)
	b.SetUSBDevices([]device.USBDevice{testPrinter()}, false)
	b.healthTimeout, b.healthPoll = 50*time.Millisecond, 2*time.Millisecond
	return b
}

func newFakeWithSelf() *fakeEngine {
	f := newFake()
	f.addContainer("pagewire", "sha256:self", nil, container.StateRunning)
	return f
}

func usbSpec() *device.Spec    { return goldenSpecs()["hplip-airsane-blob"] }
func simSpec() *device.Spec    { return goldenSpecs()["simulated"] }
func ippusbSpec() *device.Spec { return goldenSpecs()["ippusb"] }

func mustApply(t *testing.T, b *Backend, s *device.Spec) {
	t.Helper()
	var specs []*device.Spec
	if s != nil {
		specs = append(specs, s)
	}
	if err := b.Apply(t.Context(), specs); err != nil {
		t.Fatal(err)
	}
}

func converge(t *testing.T, b *Backend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	b.converge(ctx)
	if ctx.Err() != nil {
		t.Fatal("converge hit its deadline")
	}
}

func observe(t *testing.T, b *Backend) device.Observation {
	t.Helper()
	obs, err := b.Observe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Workload != "" {
		t.Fatalf("observations %+v, want one for workload \"\"", obs)
	}
	return obs[0]
}

func wantPhase(t *testing.T, b *Backend, phase string) device.Observation {
	t.Helper()
	o := observe(t, b)
	if o.Phase != phase {
		t.Fatalf("phase %q (%q), want %q", o.Phase, o.Message, phase)
	}
	return o
}

// waitFor polls cond every 5 ms and fails after 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// runBackend starts b.Run and stops it at cleanup.
func runBackend(t *testing.T, b *Backend) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return")
		}
	})
}

// mutations are the calls that change the workload.
func mutations(f *fakeEngine) []string {
	var out []string
	for _, c := range f.callLog() {
		for _, m := range []string{"ContainerStop ", "ContainerRemove ", "ContainerCreate ", "VolumeRemove ", "ContainerStart "} {
			if strings.HasPrefix(c, m) {
				out = append(out, c)
			}
		}
	}
	return out
}

func countCalls(f *fakeEngine, prefix string) int {
	n := 0
	for _, c := range f.callLog() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// rolledOut gives f a healthy workload for spec, rolled out by a throwaway
// Backend with p, and clears the call log.
func rolledOut(t *testing.T, f *fakeEngine, p Params, s *device.Spec) {
	t.Helper()
	b := newTestBackend(f, p)
	mustApply(t, b, s)
	converge(t, b)
	wantPhase(t, b, device.PhaseRunning)
	f.resetCalls()
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

func TestFirstRollout(t *testing.T) {
	f := newFakeWithSelf()
	b := newTestBackend(f, testParams(t))
	mustApply(t, b, usbSpec())
	converge(t, b)
	o := wantPhase(t, b, device.PhaseRunning)
	if o.HostIP != testHostIP || o.Unschedulable || len(o.Endpoints) != 0 {
		t.Errorf("observation %+v", o)
	}

	calls := f.callLog()
	at := func(c string) int {
		t.Helper()
		i := slices.Index(calls, c)
		if i < 0 {
			t.Fatalf("no call %q in %q", c, calls)
		}
		return i
	}
	before := func(a, b string) {
		t.Helper()
		if at(a) >= at(b) {
			t.Errorf("%q after %q", a, b)
		}
	}
	for _, v := range []string{RunVolume, DBusVolume, ConfigVolume, TokenVolume, BlobVolume} {
		before("VolumeCreate "+v, "ContainerCreate pagewire-device-probe")
	}
	before("ContainerCreate pagewire-device-airsane", "ContainerCreate "+SeedContainer)
	before("ContainerCreate "+SeedContainer, "CopyToContainer "+SeedContainer+":"+seedConfigDir)
	before("CopyToContainer "+SeedContainer+":"+seedConfigDir, "CopyToContainer "+SeedContainer+":"+seedTokenDir)
	before("CopyToContainer "+SeedContainer+":"+seedTokenDir, "ContainerRemove "+SeedContainer)
	before("ContainerRemove "+SeedContainer, "ContainerStart pagewire-device-probe")
	before("ContainerStart pagewire-device-probe", "ContainerWait pagewire-device-probe")
	before("ContainerWait pagewire-device-probe", "ContainerStart pagewire-device-dbus")
	before("ContainerStart pagewire-device-avahi", "ContainerInspect pagewire-device-avahi")
	before("ContainerInspect pagewire-device-avahi", "ContainerStart pagewire-device-agent")
	before("ContainerStart pagewire-device-agent", "ContainerStart pagewire-device-fetch-blobs")
	before("ContainerWait pagewire-device-fetch-blobs", "ContainerStart pagewire-device-printer-app-hplip")
	before("ContainerStart pagewire-device-printer-app-hplip", "ContainerStart pagewire-device-airsane")

	cfg := f.volume(ConfigVolume).files
	for _, name := range []string{"ipp-usb.conf", "dll.conf", "options.conf", "ignore.conf", "access.conf"} {
		if cfg[name] == "" {
			t.Errorf("config volume lacks %s: %v", name, cfg)
		}
	}
	if cfg["dll.conf"] != "hpaio\n" {
		t.Errorf("dll.conf %q", cfg["dll.conf"])
	}
	tok, err := b.AgentToken(t.Context())
	if err != nil || len(tok) != 64 {
		t.Fatalf("token %q, %v", tok, err)
	}
	if got := f.volume(TokenVolume).files["token"]; got != tok {
		t.Errorf("token volume holds %q, want %q", got, tok)
	}
	if v := f.volume(DBusVolume); v.driverOpts["type"] != "tmpfs" || v.labels[LabelManagedBy] != ManagedBy {
		t.Errorf("dbus volume %+v", v)
	}
	if slices.Contains(f.names(), SeedContainer) {
		t.Error("seed container left behind")
	}
	if f.anonVolumes() == 0 {
		t.Error("no anonymous scratch volumes")
	}
}

func TestHealthGate(t *testing.T) {
	avahi := Prefix + "avahi"
	cases := []struct {
		name    string
		health  *container.Health
		wantErr bool
	}{
		{"starting with log", &container.Health{Status: container.Starting, Log: []*container.HealthcheckResult{{ExitCode: 1}}}, true},
		{"unhealthy", &container.Health{Status: container.Unhealthy, FailingStreak: 3, Log: []*container.HealthcheckResult{{ExitCode: 1}}}, true},
		{"no health", nil, false},
		{"never ran", &container.Health{Status: container.Starting}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			f := newFakeWithSelf()
			f.health[avahi] = tc.health
			b := newTestBackend(f, testParams(t))
			mustApply(t, b, simSpec())
			converge(t, b)
			o := observe(t, b)
			if tc.wantErr {
				if o.Phase != device.PhasePending || !strings.Contains(o.Message, "avahi: not healthy") {
					t.Errorf("observation %+v, want Pending with avahi: not healthy", o)
				}
				if countCalls(f, "ContainerStart pagewire-device-agent") != 0 {
					t.Error("the agent started before avahi was healthy")
				}
				return
			}
			if o.Phase != device.PhaseRunning {
				t.Fatalf("observation %+v, want Running", o)
			}
			if err := b.RestartPod(t.Context(), ""); err != nil {
				t.Fatal(err)
			}
			converge(t, b)
			wantPhase(t, b, device.PhaseRunning)
			if n := strings.Count(logs.String(), "does not run healthchecks"); n != 1 {
				t.Errorf("health skip warned %d times, want once:\n%s", n, logs)
			}
		})
	}
}

func TestNoApplyYet(t *testing.T) {
	f := newFakeWithSelf()
	p := testParams(t)
	rolledOut(t, f, p, usbSpec())
	p.Resync = 20 * time.Millisecond
	b := newTestBackend(f, p)
	runBackend(t, b)
	time.Sleep(150 * time.Millisecond)
	if m := mutations(f); len(m) > 0 {
		t.Errorf("worker changed the workload before any Apply: %q", m)
	}
	wantPhase(t, b, device.PhaseRunning)
}

func TestFailedFirstApply(t *testing.T) {
	t.Run("self inspect error", func(t *testing.T) {
		f := newFakeWithSelf()
		p := testParams(t)
		rolledOut(t, f, p, usbSpec())
		p.Resync = 20 * time.Millisecond
		b := newTestBackend(f, p)
		runBackend(t, b)
		f.setErr("ContainerInspect pagewire", errors.New("engine down"))
		if err := b.Apply(t.Context(), []*device.Spec{usbSpec()}); err == nil || !strings.Contains(err.Error(), "engine down") {
			t.Fatalf("Apply = %v, want the inspect error", err)
		}
		checkAdoptsAfterFailure(t, f, b)
	})
	t.Run("token dir unwritable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can write any directory")
		}
		f := newFakeWithSelf()
		p := testParams(t)
		rolledOut(t, f, p, usbSpec())
		dir := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		p.TokenFile, p.Resync = filepath.Join(dir, "agent-token"), 20*time.Millisecond
		b := newTestBackend(f, p)
		runBackend(t, b)
		if err := b.Apply(t.Context(), []*device.Spec{usbSpec()}); err == nil {
			t.Fatal("Apply succeeded with an unwritable token directory")
		}
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // test directory
			t.Fatal(err)
		}
		checkAdoptsAfterFailure(t, f, b)
	})
}

// checkAdoptsAfterFailure: after the failed Apply nothing changes; the next
// successful Apply adopts the running workload.
func checkAdoptsAfterFailure(t *testing.T, f *fakeEngine, b *Backend) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	if m := mutations(f); len(m) > 0 {
		t.Fatalf("worker changed the workload after a failed Apply: %q", m)
	}
	f.setErr("ContainerInspect pagewire", nil)
	mustApply(t, b, usbSpec())
	waitFor(t, "adoption", func() bool { return observe(t, b).Phase == device.PhaseRunning })
	time.Sleep(100 * time.Millisecond)
	if m := mutations(f); len(m) > 0 {
		t.Errorf("adoption changed the workload: %q", m)
	}
}

func TestRebootObservesAbsent(t *testing.T) {
	f := newFakeWithSelf()
	p := testParams(t)
	rolledOut(t, f, p, usbSpec())
	for _, n := range []string{"dbus", "avahi", "agent", "printer-app-hplip", "airsane"} {
		f.setState(Prefix+n, container.StateExited, 0)
	}
	b := newTestBackend(f, p)
	wantPhase(t, b, device.PhaseAbsent)
	// After an Apply the same containers are a rollout in progress, not Absent.
	mustApply(t, b, usbSpec())
	wantPhase(t, b, device.PhasePending)
	converge(t, b)
	wantPhase(t, b, device.PhaseRunning)
}

func TestIdempotentAndAdoption(t *testing.T) {
	f := newFakeWithSelf()
	p := testParams(t)
	b := newTestBackend(f, p)
	mustApply(t, b, usbSpec())
	converge(t, b)
	f.resetCalls()
	converge(t, b)
	if m := mutations(f); len(m) > 0 {
		t.Errorf("second converge changed the workload: %q", m)
	}
	b2 := newTestBackend(f, p)
	mustApply(t, b2, usbSpec())
	converge(t, b2)
	if m := mutations(f); len(m) > 0 {
		t.Errorf("restarted control plane changed the workload: %q", m)
	}
	wantPhase(t, b2, device.PhaseRunning)
}

func TestUpgradeRolls(t *testing.T) {
	t.Run("self image", func(t *testing.T) {
		f := newFakeWithSelf()
		p := testParams(t)
		rolledOut(t, f, p, usbSpec())
		f.mu.Lock()
		f.containers["pagewire"].opts.Config.Image = "sha256:new"
		f.mu.Unlock()
		b := newTestBackend(f, p)
		mustApply(t, b, usbSpec())
		converge(t, b)
		if countCalls(f, "ContainerCreate pagewire-device-agent") != 1 || countCalls(f, "ContainerRemove pagewire-device-agent") != 1 {
			t.Errorf("new self image did not roll the workload: %q", mutations(f))
		}
		r, _ := f.ContainerInspect(t.Context(), Prefix+"agent", client.ContainerInspectOptions{})
		if r.Container.Image != "sha256:new" {
			t.Errorf("agent image %q", r.Container.Image)
		}
	})
	t.Run("tag", func(t *testing.T) {
		f := newFakeWithSelf()
		p := testParams(t)
		rolledOut(t, f, p, usbSpec())
		p.Tag = "1.2.4"
		b := newTestBackend(f, p)
		mustApply(t, b, usbSpec())
		converge(t, b)
		if countCalls(f, "ContainerCreate pagewire-device-printer-app-hplip") != 1 {
			t.Errorf("new tag did not roll the workload: %q", mutations(f))
		}
		wantPhase(t, b, device.PhaseRunning)
	})
}

func TestSpecChangeAndOwnership(t *testing.T) {
	f := newFakeWithSelf()
	f.addContainer("someone-else", "busybox", map[string]string{"x": "y"}, container.StateRunning)
	b := newTestBackend(f, testParams(t))
	mustApply(t, b, usbSpec())
	converge(t, b)
	anonBefore := f.anonVolumes()
	f.resetCalls()
	mustApply(t, b, simSpec())
	converge(t, b)
	wantPhase(t, b, device.PhaseRunning)
	if countCalls(f, "ContainerRemove pagewire-device-printer-app-hplip") != 1 || slices.Contains(f.names(), Prefix+"printer-app-hplip") {
		t.Error("old printer-app not removed")
	}
	if countCalls(f, "ContainerCreate pagewire-device-sim-printer-0") != 1 {
		t.Error("sim-printer not created")
	}
	if got := f.anonVolumes(); got != 2 || anonBefore == 2 { // sim-printer and airsane /tmp
		t.Errorf("anonymous volumes %d (before %d): teardown must remove them", got, anonBefore)
	}
	for _, c := range f.callLog() {
		if strings.HasSuffix(c, " someone-else") {
			t.Errorf("foreign container touched: %s", c)
		}
	}

	// A foreign volume with a per-rollout name fails the rollout and stays.
	f2 := newFakeWithSelf()
	f2.volumes[RunVolume] = &fakeVolume{labels: map[string]string{"owner": "me"}}
	b2 := newTestBackend(f2, testParams(t))
	mustApply(t, b2, simSpec())
	converge(t, b2)
	o := observe(t, b2)
	if o.Phase != device.PhasePending || !strings.Contains(o.Message, RunVolume) || !strings.Contains(o.Message, "not labelled") {
		t.Errorf("observation %+v, want the foreign volume error", o)
	}
	if v := f2.volume(RunVolume); v == nil || v.labels["owner"] != "me" {
		t.Error("foreign volume removed or changed")
	}
	if countCalls(f2, "VolumeRemove "+RunVolume) != 0 {
		t.Error("foreign volume removal attempted")
	}
}

func TestBlobsSurviveRemove(t *testing.T) {
	f := newFakeWithSelf()
	b := newTestBackend(f, testParams(t))
	mustApply(t, b, usbSpec())
	converge(t, b)
	if f.volume(BlobVolume) == nil {
		t.Fatal("blobs volume not created")
	}
	mustApply(t, b, nil)
	converge(t, b)
	wantPhase(t, b, device.PhaseAbsent)
	if names := f.names(); !slices.Equal(names, []string{"pagewire"}) {
		t.Errorf("containers left: %v", names)
	}
	for _, v := range rolloutVolumes() {
		if f.volume(v.Name) != nil {
			t.Errorf("volume %s left", v.Name)
		}
	}
	if f.volume(BlobVolume) == nil {
		t.Error("blobs volume removed")
	}
	if n := f.anonVolumes(); n != 0 {
		t.Errorf("%d anonymous volumes left", n)
	}
}

func TestFetchBlobsFailure(t *testing.T) {
	f := newFakeWithSelf()
	f.exitCodes[Prefix+"fetch-blobs"] = 1
	b := newTestBackend(f, testParams(t))
	mustApply(t, b, usbSpec())
	converge(t, b)
	o := wantPhase(t, b, device.PhasePending)
	if o.Message != "fetch-blobs: exited with code 1" {
		t.Errorf("message %q", o.Message)
	}
	converge(t, b)
	if b.fails != 2 {
		t.Errorf("fails = %d after two failed rollouts, want 2", b.fails)
	}
	f.mu.Lock()
	f.exitCodes[Prefix+"fetch-blobs"] = 0
	f.mu.Unlock()
	converge(t, b)
	wantPhase(t, b, device.PhaseRunning)
	if b.fails != 0 {
		t.Errorf("fails = %d after a successful rollout, want 0", b.fails)
	}
}

func TestRetryWait(t *testing.T) {
	for _, c := range []struct {
		resync time.Duration
		fails  int
		want   time.Duration
	}{
		{30 * time.Second, 0, 30 * time.Second},
		{30 * time.Second, 1, 30 * time.Second},
		{30 * time.Second, 2, time.Minute},
		{30 * time.Second, 4, 4 * time.Minute},
		{30 * time.Second, 5, maxRetryWait},
		{30 * time.Second, 1000, maxRetryWait},
		{time.Hour, 3, time.Hour}, // a resync longer than the cap is kept
	} {
		if got := retryWait(c.resync, c.fails); got != c.want {
			t.Errorf("retryWait(%v, %d) = %v, want %v", c.resync, c.fails, got, c.want)
		}
	}
}

func TestNewSpecResetsFailures(t *testing.T) {
	f := newFakeWithSelf()
	f.exitCodes[Prefix+"fetch-blobs"] = 1
	b := newTestBackend(f, testParams(t))
	mustApply(t, b, usbSpec())
	converge(t, b)
	if b.fails != 1 {
		t.Fatalf("fails = %d, want 1", b.fails)
	}
	mustApply(t, b, usbSpec()) // same spec: the backoff stays
	if b.fails != 1 {
		t.Errorf("fails = %d after the same Apply, want 1", b.fails)
	}
	mustApply(t, b, ippusbSpec())
	if b.fails != 0 {
		t.Errorf("fails = %d after a new spec, want 0", b.fails)
	}
}

// blockPulls makes the next pull block until release is closed; entered is
// closed when it starts.
func blockPulls(f *fakeEngine) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.mu.Lock()
	f.pullHook = func(ctx context.Context, _ string) error {
		first := false
		once.Do(func() { first = true; close(entered) })
		if !first {
			return nil
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Unlock()
	return entered, release
}

func waitClosed(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// convergeAsync runs one converge in the background; the returned channel
// closes when it returns.
func convergeAsync(t *testing.T, b *Backend) chan struct{} {
	t.Helper()
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	go func() { defer cancel(); b.converge(ctx); close(done) }()
	t.Cleanup(func() { waitClosed(t, done, "converge") })
	return done
}

func TestRestartPodPendingNotAbsent(t *testing.T) {
	f := newFakeWithSelf()
	p := testParams(t)
	p.Tag = "main" // device images are pulled on every rollout
	b := newTestBackend(f, p)
	mustApply(t, b, usbSpec())
	converge(t, b)
	wantPhase(t, b, device.PhaseRunning)
	f.resetCalls()

	entered, release := blockPulls(f)
	if err := b.RestartPod(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	done := convergeAsync(t, b)
	waitClosed(t, entered, "pull")
	o := wantPhase(t, b, device.PhasePending)
	if !strings.HasPrefix(o.Message, "pulling ") {
		t.Errorf("message %q, want the pull step", o.Message)
	}
	close(release)
	waitClosed(t, done, "converge")
	wantPhase(t, b, device.PhaseRunning)
	if countCalls(f, "ContainerStart pagewire-device-probe") != 1 {
		t.Error("RestartPod did not run the probe again")
	}
}

func TestEmptySpecHash(t *testing.T) {
	f := newFakeWithSelf()
	b := newTestBackend(f, testParams(t))
	s := simSpec()
	s.Hash = ""
	mustApply(t, b, s)
	converge(t, b)
	wantPhase(t, b, device.PhaseRunning)
}

func TestUSBGate(t *testing.T) {
	t.Run("opens when a printer appears", func(t *testing.T) {
		f := newFakeWithSelf()
		p := testParams(t)
		p.SysRoot, p.Tag = sysfs(t, false), "main"
		b := newTestBackend(f, p)
		mustApply(t, b, ippusbSpec())
		converge(t, b)
		if c := countCalls(f, "ContainerCreate"); c != 0 {
			t.Errorf("%d containers created without a printer", c)
		}
		o := wantPhase(t, b, device.PhasePending)
		if !o.Unschedulable || o.Message != msgNoPrinter {
			t.Errorf("observation %+v", o)
		}

		setPrinter(t, p.SysRoot, true)
		entered, release := blockPulls(f)
		done := convergeAsync(t, b)
		waitClosed(t, entered, "pull")
		if o := wantPhase(t, b, device.PhasePending); o.Unschedulable {
			t.Errorf("still unschedulable during the rollout: %+v", o)
		}
		close(release)
		waitClosed(t, done, "converge")
		wantPhase(t, b, device.PhaseRunning)
	})
	t.Run("rollout errors show through", func(t *testing.T) {
		f := newFakeWithSelf()
		f.setErr("ImagePull ghcr.io/thephaseless/pagewire-device:1.2.3", errors.New("registry down"))
		b := newTestBackend(f, testParams(t))
		mustApply(t, b, ippusbSpec())
		converge(t, b)
		o := wantPhase(t, b, device.PhasePending)
		if o.Unschedulable || o.Message != "pull ghcr.io/thephaseless/pagewire-device:1.2.3: registry down" {
			t.Errorf("observation %+v", o)
		}

		f2 := newFakeWithSelf()
		f2.exitCodes[Prefix+"fetch-blobs"] = 1
		b2 := newTestBackend(f2, testParams(t))
		mustApply(t, b2, usbSpec())
		converge(t, b2)
		if o := wantPhase(t, b2, device.PhasePending); o.Unschedulable || o.Message != "fetch-blobs: exited with code 1" {
			t.Errorf("observation %+v", o)
		}
	})
	t.Run("simulated is never gated", func(t *testing.T) {
		f := newFakeWithSelf()
		p := testParams(t)
		p.SysRoot = sysfs(t, false)
		b := newTestBackend(f, p)
		mustApply(t, b, usbSpec())
		converge(t, b)
		if o := observe(t, b); !o.Unschedulable {
			t.Fatalf("usb spec not gated: %+v", o)
		}
		mustApply(t, b, simSpec())
		converge(t, b)
		if o := wantPhase(t, b, device.PhaseRunning); o.Unschedulable {
			t.Errorf("simulated spec gated: %+v", o)
		}
	})
}

func TestPulls(t *testing.T) {
	f := newFakeWithSelf()
	p := testParams(t)
	p.Overrides = map[string]string{"device": "example.com/device:main", "driver-hplip": "example.com/hplip:1.2.3"}
	f.images["example.com/hplip:1.2.3"] = true
	b := newTestBackend(f, p)
	mustApply(t, b, usbSpec())
	converge(t, b)
	wantPhase(t, b, device.PhaseRunning)
	if countCalls(f, "ImagePull example.com/device:main") != 1 {
		t.Error(":main not pulled")
	}
	if countCalls(f, "ImagePull example.com/hplip:1.2.3") != 0 {
		t.Error("present image pulled")
	}
	if countCalls(f, "ImagePull sha256:") != 0 || countCalls(f, "ImageInspect sha256:") != 0 {
		t.Error("image ID pulled or inspected")
	}
	f.resetCalls()
	if err := b.RestartPod(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	converge(t, b)
	if countCalls(f, "ImagePull example.com/device:main") != 1 || countCalls(f, "ImagePull example.com/hplip") != 0 {
		t.Errorf("second rollout pulls: %q", f.callLog())
	}

	// A missing image is pulled; a pull error shows in Observe.
	f2 := newFakeWithSelf()
	f2.setErr("ImagePull ghcr.io/thephaseless/pagewire-device:1.2.3", errors.New("denied"))
	b2 := newTestBackend(f2, testParams(t))
	mustApply(t, b2, simSpec())
	converge(t, b2)
	if o := wantPhase(t, b2, device.PhasePending); o.Message != "pull ghcr.io/thephaseless/pagewire-device:1.2.3: denied" {
		t.Errorf("message %q", o.Message)
	}
}

var listOwned = client.ContainerListOptions{All: true, Filters: make(client.Filters).Add("label", LabelManagedBy+"="+ManagedBy)}

func TestSelfImage(t *testing.T) {
	images := func(f *fakeEngine) map[string]string {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := map[string]string{}
		for n, c := range f.containers {
			out[n] = c.opts.Config.Image
		}
		return out
	}
	f := newFakeWithSelf()
	b := newTestBackend(f, testParams(t))
	mustApply(t, b, usbSpec())
	converge(t, b)
	got := images(f)
	for _, n := range []string{"probe", "agent", "fetch-blobs"} {
		if got[Prefix+n] != "sha256:self" {
			t.Errorf("%s runs %q", n, got[Prefix+n])
		}
	}
	if got[Prefix+"dbus"] != "ghcr.io/thephaseless/pagewire-device:1.2.3" {
		t.Errorf("dbus runs %q", got[Prefix+"dbus"])
	}
	f.mu.Lock()
	seedImage := f.created[SeedContainer]
	f.mu.Unlock()
	if seedImage != "sha256:self" {
		t.Errorf("seed runs %q", seedImage)
	}

	// Not found: the registry image.
	f2 := newFake()
	b2 := newTestBackend(f2, testParams(t))
	mustApply(t, b2, simSpec())
	converge(t, b2)
	if got := images(f2)[Prefix+"agent"]; got != "ghcr.io/thephaseless/pagewire:1.2.3" {
		t.Errorf("fallback agent image %q", got)
	}

	// Any other error fails Apply and changes nothing.
	f3 := newFakeWithSelf()
	f3.setErr("ContainerInspect pagewire", errors.New("boom"))
	b3 := newTestBackend(f3, testParams(t))
	if err := b3.Apply(t.Context(), []*device.Spec{simSpec()}); err == nil {
		t.Fatal("Apply succeeded")
	}
	b3.mu.Lock()
	applied, want := b3.applied, b3.want
	b3.mu.Unlock()
	if applied || want != nil {
		t.Error("failed Apply changed the backend state")
	}
}

func TestToken(t *testing.T) {
	f := newFakeWithSelf()
	p := testParams(t)
	b := newTestBackend(f, p)
	if _, err := b.AgentToken(t.Context()); err == nil {
		t.Fatal("AgentToken before the first Apply succeeded")
	}
	mustApply(t, b, simSpec())
	fi, err := os.Stat(p.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token mode %v", fi.Mode().Perm())
	}
	tok, err := b.AgentToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b2 := newTestBackend(f, p)
	mustApply(t, b2, simSpec())
	tok2, err := b2.AgentToken(t.Context())
	if err != nil || tok2 != tok {
		t.Errorf("token changed across backends: %q, %q (%v)", tok, tok2, err)
	}
}

func TestApplyCancelsBlockedRollout(t *testing.T) {
	f := newFakeWithSelf()
	p := testParams(t)
	p.Tag, p.Resync = "main", time.Hour
	b := newTestBackend(f, p)
	entered, _ := blockPulls(f)
	runBackend(t, b)
	mustApply(t, b, usbSpec())
	waitClosed(t, entered, "first pull")
	mustApply(t, b, simSpec())
	b.mu.Lock()
	wantHash := b.want.hash
	b.mu.Unlock()
	waitFor(t, "the simulated rollout", func() bool {
		o := observe(t, b)
		return o.Phase == device.PhaseRunning
	})
	if !slices.Contains(f.names(), Prefix+"sim-printer-0") || slices.Contains(f.names(), Prefix+"printer-app-hplip") {
		t.Errorf("containers %v", f.names())
	}
	r, _ := f.ContainerList(t.Context(), listOwned)
	for _, c := range r.Items {
		if c.Labels[LabelWorkloadHash] != wantHash {
			t.Errorf("%v: workload hash %q", c.Names, c.Labels[LabelWorkloadHash])
		}
	}
}

func TestObserveMapping(t *testing.T) {
	f := newFakeWithSelf()
	b := newTestBackend(f, testParams(t))
	wantPhase(t, b, device.PhaseAbsent)
	mustApply(t, b, simSpec())
	converge(t, b)
	if o := wantPhase(t, b, device.PhaseRunning); o.HostIP != testHostIP || o.Message != "" {
		t.Errorf("observation %+v", o)
	}
	f.setState(Prefix+"sim-printer-0", container.StateRestarting, 0)
	if o := wantPhase(t, b, device.PhasePending); o.Message != "sim-printer-0: restarting" {
		t.Errorf("message %q", o.Message)
	}
	f.setState(Prefix+"sim-printer-0", container.StateExited, 137)
	if o := wantPhase(t, b, device.PhasePending); o.Message != "sim-printer-0: exited (exit 137)" {
		t.Errorf("message %q", o.Message)
	}
	f.setErr("ContainerList ", errors.New("socket gone"))
	if _, err := b.Observe(t.Context()); err == nil || !strings.Contains(err.Error(), "list device containers") {
		t.Errorf("Observe error %v", err)
	}
}

func TestHostIP(t *testing.T) {
	old := routeIP
	t.Cleanup(func() { routeIP = old })
	var mu sync.Mutex
	ip, fail := "198.51.100.7", false
	routeIP = func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return "", errors.New("no route")
		}
		return ip, nil
	}
	f := newFakeWithSelf()
	p := testParams(t)
	if got := newTestBackend(f, p).HostIP(t.Context()); got != testHostIP {
		t.Errorf("Params.HostIP ignored: %q", got)
	}
	p.HostIP = ""
	b := newTestBackend(f, p)
	if o := observe(t, b); o.HostIP != "198.51.100.7" {
		t.Errorf("observed host IP %q", o.HostIP)
	}
	mu.Lock()
	ip = "198.51.100.8"
	mu.Unlock()
	if o := observe(t, b); o.HostIP != "198.51.100.8" {
		t.Errorf("host IP not re-read: %q", o.HostIP)
	}
	mu.Lock()
	fail = true
	mu.Unlock()
	if o := observe(t, b); o.HostIP != "198.51.100.8" {
		t.Errorf("failing route lost the last good address: %q", o.HostIP)
	}
	if got := newTestBackend(f, p).HostIP(t.Context()); got != "127.0.0.1" {
		t.Errorf("no route and no last address: %q", got)
	}
}

func TestRestartPodSurvivesFailedConverge(t *testing.T) {
	f := newFakeWithSelf()
	b := newTestBackend(f, testParams(t))
	mustApply(t, b, simSpec())
	converge(t, b)
	f.resetCalls()
	if err := b.RestartPod(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	f.setErr("ContainerList ", errors.New("socket gone"))
	converge(t, b)
	f.setErr("ContainerList ", nil)
	converge(t, b)
	if countCalls(f, "ContainerStart pagewire-device-probe") != 1 {
		t.Errorf("RestartPod lost after a failed converge: %q", mutations(f))
	}
	wantPhase(t, b, device.PhaseRunning)
}

func TestRestartPodBeforeFirstApply(t *testing.T) {
	f := newFakeWithSelf()
	p := testParams(t)
	rolledOut(t, f, p, simSpec())
	b := newTestBackend(f, p)
	if err := b.RestartPod(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	converge(t, b)
	if m := mutations(f); len(m) > 0 {
		t.Fatalf("RestartPod before Apply changed the workload: %q", m)
	}
	mustApply(t, b, simSpec())
	converge(t, b)
	if countCalls(f, "ContainerStart pagewire-device-probe") != 1 {
		t.Errorf("RestartPod before Apply did not roll out after it: %q", mutations(f))
	}
}

func TestForeignContainerWithOurName(t *testing.T) {
	f := newFakeWithSelf()
	f.addContainer(Prefix+"dbus", "busybox", nil, container.StateRunning)
	b := newTestBackend(f, testParams(t))
	mustApply(t, b, simSpec())
	for range 2 {
		converge(t, b)
		o := wantPhase(t, b, device.PhasePending)
		if !strings.Contains(o.Message, "create "+Prefix+"dbus") || !strings.Contains(o.Message, "in use") {
			t.Errorf("message %q, want the name conflict", o.Message)
		}
	}
	for _, c := range f.callLog() {
		if (strings.HasPrefix(c, "ContainerStop ") || strings.HasPrefix(c, "ContainerRemove ")) && strings.HasSuffix(c, " "+Prefix+"dbus") {
			t.Errorf("foreign container touched: %s", c)
		}
	}
	if !slices.Contains(f.names(), Prefix+"dbus") {
		t.Error("foreign container removed")
	}
}

func TestTokenUnreadableNotReplaced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	p := testParams(t)
	if err := os.WriteFile(p.TokenFile, []byte("keep"), 0o200); err != nil { //nolint:gosec // test file
		t.Fatal(err)
	}
	b := newTestBackend(newFakeWithSelf(), p)
	if err := b.Apply(t.Context(), []*device.Spec{simSpec()}); err == nil {
		t.Fatal("Apply succeeded with an unreadable token file")
	}
	if err := os.Chmod(p.TokenFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(p.TokenFile); string(raw) != "keep" {
		t.Errorf("token replaced: %q", raw)
	}
}

// TestLabelledForeignContainerLeftAlone: the owner label alone does not make a
// container ours; without the name prefix it is never stopped or removed and
// does not make the workload look unhealthy.
func TestLabelledForeignContainerLeftAlone(t *testing.T) {
	f := newFakeWithSelf()
	f.addContainer("someone-else", "busybox", ownerLabels(), container.StateRunning)
	p := testParams(t)
	rolledOut(t, f, p, simSpec())
	b := newTestBackend(f, p)
	mustApply(t, b, simSpec())
	converge(t, b)
	if m := mutations(f); len(m) > 0 {
		t.Errorf("adoption changed the workload: %q", m)
	}
	mustApply(t, b, nil)
	converge(t, b)
	for _, c := range f.callLog() {
		if strings.HasSuffix(c, " someone-else") {
			t.Errorf("foreign container touched: %s", c)
		}
	}
	if !slices.Contains(f.names(), "someone-else") {
		t.Error("foreign container removed")
	}
}

func setDevnum(t *testing.T, root string, addr string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "bus", "usb", "devices", "1-1", "devnum"), []byte(addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func usbDevices(f *fakeEngine, name string) []container.DeviceMapping {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[Prefix+name]
	if c == nil {
		return nil
	}
	return c.opts.HostConfig.Devices
}

func wantNode(t *testing.T, f *fakeEngine, node string) {
	t.Helper()
	d := usbDevices(f, "ipp-usb")
	if len(d) != 1 || d[0].PathOnHost != node || d[0].PathInContainer != node || d[0].CgroupPermissions != "rw" {
		t.Fatalf("ipp-usb devices %+v, want %s rw", d, node)
	}
}

// TestUSBDeviceNode: the USB daemons get the matched printer's node and are
// recreated when the printer comes back at a new address; with the printer
// not attached they are left out, while the probe and agent still run.
func TestUSBDeviceNode(t *testing.T) {
	f := newFakeWithSelf()
	p := testParams(t)
	b := newTestBackend(f, p)
	mustApply(t, b, ippusbSpec())
	converge(t, b)
	wantPhase(t, b, device.PhaseRunning)
	wantNode(t, f, "/dev/bus/usb/001/007")

	t.Run("replug rolls to the new node", func(t *testing.T) {
		setDevnum(t, p.SysRoot, "9")
		converge(t, b)
		wantPhase(t, b, device.PhaseRunning)
		wantNode(t, f, "/dev/bus/usb/001/009")
		f.resetCalls()
		converge(t, b)
		if m := mutations(f); len(m) > 0 {
			t.Errorf("second converge changed the workload: %q", m)
		}
	})
	t.Run("RestartPod after a replug uses the new node", func(t *testing.T) {
		setDevnum(t, p.SysRoot, "11")
		if err := b.RestartPod(t.Context(), ""); err != nil {
			t.Fatal(err)
		}
		converge(t, b)
		wantNode(t, f, "/dev/bus/usb/001/011")
	})
	t.Run("two identical printers off the recorded address: daemons left out", func(t *testing.T) {
		twin := filepath.Join(p.SysRoot, "bus", "usb", "devices", "1-2")
		if err := os.CopyFS(twin, os.DirFS(filepath.Join(p.SysRoot, "bus", "usb", "devices", "1-1"))); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(twin, "devnum"), []byte("12\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		converge(t, b)
		if slices.Contains(f.names(), Prefix+"ipp-usb") {
			t.Fatalf("containers %v: ipp-usb runs on one of two identical printers", f.names())
		}
		if o := wantPhase(t, b, device.PhasePending); o.Message != msgNoDevice+"printer, scanner" {
			t.Errorf("observation %+v", o)
		}
		if err := os.RemoveAll(twin); err != nil {
			t.Fatal(err)
		}
		converge(t, b)
		wantNode(t, f, "/dev/bus/usb/001/011")
	})
	t.Run("no bound device", func(t *testing.T) {
		b.SetUSBDevices(nil, false)
		converge(t, b)
		if slices.Contains(f.names(), Prefix+"ipp-usb") || !slices.Contains(f.names(), Prefix+"agent") {
			t.Fatalf("containers %v: want the agent and no ipp-usb", f.names())
		}
		if o := wantPhase(t, b, device.PhasePending); o.Unschedulable || o.Message != msgNoDevice+"printer, scanner" {
			t.Errorf("observation %+v", o)
		}
		b.SetUSBDevices([]device.USBDevice{{VendorID: keyHP.VendorID, ProductID: keyHP.ProductID, Serial: keyHP.Serial, Bus: 1, Address: 11}}, false)
		converge(t, b)
		wantPhase(t, b, device.PhaseRunning)
		wantNode(t, f, "/dev/bus/usb/001/011")
	})
}

// TestUSBDeviceStagedForApply: devices passed before an Apply reach the
// worker with that Apply's spec, never with the old one, and a failing Apply
// leaves them out.
func TestUSBDeviceStagedForApply(t *testing.T) {
	f := newFakeWithSelf()
	b := newTestBackend(f, testParams(t))
	<-b.wake
	other := testPrinter()
	other.Address = 11
	current := func() []device.USBDevice {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.usbDevs
	}
	b.SetUSBDevices([]device.USBDevice{other}, true)
	if d := current(); len(d) != 1 || d[0].Address != 7 || len(b.wake) != 0 {
		t.Fatalf("staged devices taken or worker woken: %+v, %d wakes", d, len(b.wake))
	}
	f.setErr("ContainerInspect pagewire", errors.New("engine down"))
	if err := b.Apply(t.Context(), []*device.Spec{ippusbSpec()}); err == nil {
		t.Fatal("Apply succeeded with the engine down")
	}
	if d := current(); d[0].Address != 7 {
		t.Fatalf("failing Apply took the staged devices %+v", d)
	}
	f.setErr("ContainerInspect pagewire", nil)
	mustApply(t, b, ippusbSpec())
	if d := current(); d[0].Address != 11 {
		t.Fatalf("Apply did not take the staged devices: %+v", d)
	}
	<-b.wake
	b.SetUSBDevices([]device.USBDevice{testPrinter()}, false)
	if d := current(); d[0].Address != 7 || len(b.wake) != 1 {
		t.Fatalf("devices %+v, %d wakes: want them taken at once", d, len(b.wake))
	}
}

// lideDevice is the controller's bound device for the LiDE at addUSB's 1-2.
func lideDevice(addr int) device.USBDevice {
	return device.USBDevice{VendorID: keyLiDE.VendorID, ProductID: keyLiDE.ProductID, Serial: keyLiDE.Serial, Bus: 1, Address: addr}
}

// hpAndLiDE is an IPP-over-USB MFP beside a standalone scanner.
func hpAndLiDE() *device.Spec {
	return withHash(&device.Spec{
		Mode:     device.ModeUSB,
		Printers: []device.PrinterSpec{{ID: "hp", Family: "ipp-usb", USB: keyHP}},
		Scanners: []device.ScannerSpec{
			{ID: "hp-scanner", Family: "ipp-usb", USB: keyHP},
			{ID: "lide", Family: "airsane", SaneBackends: []string{"genesys"}, USB: keyLiDE},
		},
	})
}

func nodesOf(f *fakeEngine, name string) []string {
	var out []string
	for _, d := range usbDevices(f, name) {
		out = append(out, d.PathOnHost)
	}
	return out
}

// TestTwoDevices: one workload serves two devices, each daemon with only its
// own device's node; a replug of one rolls the workload onto its new node; a
// device that goes away leaves only its daemon out, and the workload runs.
func TestTwoDevices(t *testing.T) {
	f := newFakeWithSelf()
	p := testParams(t)
	addUSB(t, p.SysRoot, "1-2", keyLiDE, 11, "ff")
	b := newTestBackend(f, p)
	b.SetUSBDevices([]device.USBDevice{lideDevice(11), testPrinter()}, false)
	mustApply(t, b, hpAndLiDE())
	converge(t, b)
	if o := wantPhase(t, b, device.PhaseRunning); o.Message != "" {
		t.Errorf("observation %+v", o)
	}
	if got := nodesOf(f, "ipp-usb"); !slices.Equal(got, []string{testNode}) {
		t.Errorf("ipp-usb nodes %v, want the HP's alone", got)
	}
	if got := nodesOf(f, "airsane"); !slices.Equal(got, []string{"/dev/bus/usb/001/011"}) {
		t.Errorf("airsane nodes %v, want the LiDE's alone", got)
	}

	f.resetCalls()
	addUSB(t, p.SysRoot, "1-2", keyLiDE, 12, "ff")
	converge(t, b)
	wantPhase(t, b, device.PhaseRunning)
	if got := nodesOf(f, "airsane"); !slices.Equal(got, []string{"/dev/bus/usb/001/012"}) {
		t.Errorf("airsane nodes after the replug %v", got)
	}
	if got := nodesOf(f, "ipp-usb"); !slices.Equal(got, []string{testNode}) {
		t.Errorf("ipp-usb nodes after the LiDE's replug %v", got)
	}
	if countCalls(f, "ContainerCreate pagewire-device-ipp-usb") != 1 || countCalls(f, "ContainerCreate pagewire-device-airsane") != 1 {
		t.Errorf("replug did not recreate the workload: %q", mutations(f))
	}

	if err := os.RemoveAll(filepath.Join(p.SysRoot, "bus", "usb", "devices", "1-2")); err != nil {
		t.Fatal(err)
	}
	converge(t, b)
	if slices.Contains(f.names(), Prefix+"airsane") || !slices.Contains(f.names(), Prefix+"ipp-usb") {
		t.Fatalf("containers %v: want ipp-usb and no airsane", f.names())
	}
	if o := wantPhase(t, b, device.PhaseRunning); o.Message != msgNoDevice+"lide" {
		t.Errorf("observation %+v", o)
	}
}

// TestGateWantedDevice: the gate opens for any device the spec serves, a
// standalone scanner included, and in probe mode for any device that can be a
// printer or scanner; a keyboard alone keeps it closed.
func TestGateWantedDevice(t *testing.T) {
	scannerOnly := withHash(&device.Spec{
		Mode:     device.ModeUSB,
		Scanners: []device.ScannerSpec{{ID: "lide", Family: "airsane", SaneBackends: []string{"genesys"}, USB: keyLiDE}},
	})
	lide := func(t *testing.T) Params {
		p := testParams(t)
		p.SysRoot = t.TempDir()
		addUSB(t, p.SysRoot, "1-2", keyLiDE, 11, "ff")
		return p
	}
	t.Run("standalone scanner", func(t *testing.T) {
		f := newFakeWithSelf()
		b := newTestBackend(f, lide(t))
		b.SetUSBDevices([]device.USBDevice{lideDevice(11)}, false)
		mustApply(t, b, scannerOnly)
		converge(t, b)
		if o := wantPhase(t, b, device.PhaseRunning); o.Unschedulable {
			t.Errorf("observation %+v", o)
		}
		if got := nodesOf(f, "airsane"); !slices.Equal(got, []string{"/dev/bus/usb/001/011"}) {
			t.Errorf("airsane nodes %v", got)
		}
	})
	t.Run("probe with a scanner", func(t *testing.T) {
		f := newFakeWithSelf()
		b := newTestBackend(f, lide(t))
		mustApply(t, b, goldenSpecs()["probe"])
		converge(t, b)
		if o := wantPhase(t, b, device.PhaseRunning); o.Unschedulable {
			t.Errorf("observation %+v", o)
		}
	})
	t.Run("a device the spec does not serve", func(t *testing.T) {
		f := newFakeWithSelf()
		b := newTestBackend(f, lide(t))
		mustApply(t, b, ippusbSpec())
		converge(t, b)
		if o := wantPhase(t, b, device.PhasePending); !o.Unschedulable || o.Message != msgNoPrinter {
			t.Errorf("observation %+v", o)
		}
	})
	t.Run("probe with a keyboard", func(t *testing.T) {
		f := newFakeWithSelf()
		p := testParams(t)
		p.SysRoot = sysfs(t, false)
		b := newTestBackend(f, p)
		mustApply(t, b, goldenSpecs()["probe"])
		converge(t, b)
		if c := countCalls(f, "ContainerCreate"); c != 0 {
			t.Errorf("%d containers created for a keyboard", c)
		}
		if o := wantPhase(t, b, device.PhasePending); !o.Unschedulable || o.Message != msgNoPrinter {
			t.Errorf("observation %+v", o)
		}
	})
}

// TestOneWorkload: the host runs one node-less workload. More specs, a spec
// naming a node, or a restart of a named workload are errors that change
// nothing.
func TestOneWorkload(t *testing.T) {
	f := newFakeWithSelf()
	b := newTestBackend(f, testParams(t))
	mustApply(t, b, simSpec())
	<-b.wake
	pinned := simSpec()
	pinned.Node = "n1"
	for what, specs := range map[string][]*device.Spec{
		"two specs": {simSpec(), ippusbSpec()},
		"a node":    {pinned},
		"nil spec":  {nil},
	} {
		if err := b.Apply(t.Context(), specs); err == nil {
			t.Errorf("%s: Apply succeeded", what)
		}
	}
	if err := b.RestartPod(t.Context(), "n1"); err == nil {
		t.Error("RestartPod of a named workload succeeded")
	}
	b.mu.Lock()
	h, restart := b.want.hash, b.restart
	b.mu.Unlock()
	want, err := workloadHashOf(t, b, simSpec())
	if err != nil {
		t.Fatal(err)
	}
	if h != want || restart || len(b.wake) != 0 {
		t.Errorf("a failing call changed the backend: hash %s, restart %v, %d wakes", h, restart, len(b.wake))
	}
}

// workloadHashOf is the workload hash a fresh backend records for s.
func workloadHashOf(t *testing.T, b *Backend, s *device.Spec) (string, error) {
	t.Helper()
	b2 := newTestBackend(newFakeWithSelf(), b.p)
	if err := b2.Apply(t.Context(), []*device.Spec{s}); err != nil {
		return "", err
	}
	return b2.want.hash, nil
}

// TestSetUSBDevicesOrder: the same devices in another order are no change, so
// the controller's every-step call does not wake the worker.
func TestSetUSBDevicesOrder(t *testing.T) {
	b := newTestBackend(newFakeWithSelf(), testParams(t))
	b.SetUSBDevices([]device.USBDevice{lideDevice(11), testPrinter()}, false)
	<-b.wake
	b.SetUSBDevices([]device.USBDevice{testPrinter(), lideDevice(11)}, false)
	if len(b.wake) != 0 {
		t.Error("reordered devices woke the worker")
	}
	b.SetUSBDevices(nil, false)
	<-b.wake
	b.SetUSBDevices([]device.USBDevice{}, false)
	if len(b.wake) != 0 {
		t.Error("empty after nil woke the worker")
	}
}

// TestIdenticalTwins: two bound devices with one key (no serial to tell them
// apart) at their recorded addresses give their daemon both nodes.
func TestIdenticalTwins(t *testing.T) {
	f := newFakeWithSelf()
	p := testParams(t)
	addUSB(t, p.SysRoot, "1-2", keyHP, 12, "07")
	b := newTestBackend(f, p)
	twin := testPrinter()
	twin.Address = 12
	b.SetUSBDevices([]device.USBDevice{twin, testPrinter()}, false)
	mustApply(t, b, ippusbSpec())
	converge(t, b)
	wantPhase(t, b, device.PhaseRunning)
	if got := nodesOf(f, "ipp-usb"); !slices.Equal(got, []string{testNode, "/dev/bus/usb/001/012"}) {
		t.Errorf("ipp-usb nodes %v, want both twins", got)
	}
	f.mu.Lock()
	label := f.containers[Prefix+"ipp-usb"].opts.Config.Labels[LabelUSBDevice]
	f.mu.Unlock()
	if label != testNode+",/dev/bus/usb/001/012" {
		t.Errorf("label %q", label)
	}
}
