package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/ThePhaseless/pagewire/internal/blobs"
	"github.com/ThePhaseless/pagewire/internal/blobs/blobtest"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/pappl/papplfake"
)

const hpID = "MFG:HP;MDL:LaserJet 1020;CMD:PJL,PCL;CLS:PRINTER;"

// sysfs builds a minimal fake /sys with one keyboard and one printer at bus 1
// address 5, and returns the root and the printer's directory.
func sysfs(t *testing.T) (root, printer string) {
	t.Helper()
	root = t.TempDir()
	base := filepath.Join(root, "bus", "usb", "devices")
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
	dev := func(name, vid, pid, serial, product, addr, class string) string {
		dir := filepath.Join(base, name)
		write(dir, map[string]string{
			"idVendor": vid, "idProduct": pid, "serial": serial, "product": product,
			"manufacturer": "Test", "busnum": "1", "devnum": addr,
		})
		write(filepath.Join(dir, name+":1.0"), map[string]string{
			"bInterfaceNumber": "00", "bInterfaceClass": class, "bInterfaceSubClass": "01", "bInterfaceProtocol": "02",
		})
		return dir
	}
	dev("1-1", "046d", "c31c", "", "Keyboard", "2", "03")
	return root, dev("1-2", "03f0", "2b17", "ABC123", "HP LaserJet 1020", "5", "07")
}

func TestProbe(t *testing.T) {
	root, _ := sysfs(t)
	inv := Probe(root, t.TempDir(), nil) // no device node: the device ID read fails, non-fatally
	if len(inv.Devices) != 2 || inv.Devices[1].VendorID != "03f0" || inv.Devices[1].DeviceID != "" {
		t.Fatalf("inventory = %+v, want keyboard and printer with an empty device ID", inv.Devices)
	}
	if got := Probe(root, t.TempDir(), &config.USB{VendorID: "ffff", ProductID: "0001"}); len(got.Devices) != 2 {
		t.Errorf("a selector must not drop devices from the inventory: %+v", got.Devices)
	}
	if got := Probe(filepath.Join(root, "nope"), t.TempDir(), nil); got.Devices == nil || len(got.Devices) != 0 {
		t.Errorf("no USB bus: devices = %#v, want empty non-nil", got.Devices)
	}

	out := filepath.Join(t.TempDir(), "inventory.json")
	if err := WriteJSON(out, inv); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"vendorId":"03f0"`, `"deviceId":""`, `"productId"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("probe JSON lacks %s: %s", key, raw)
		}
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", fi.Mode().Perm())
	}
}

// --- fetch ---

type fetchEnv struct {
	cfg     FetchConfig
	version string
}

// licenseSHA is the text sha256 of the synthetic archive's licence.
var licenseSHA = blobs.TextSHA256([]byte(blobtest.License))

// accept is the --accept value for the catalog EULA the env's blobs use.
func (e fetchEnv) accept() string { return "hplip-plugin@" + e.version + "@" + licenseSHA }

// newFetchEnv serves two synthetic blobs that both use the catalog's HPLIP EULA
// and format: "good" verifies and installs, "bad" 404s at the vendor.
func newFetchEnv(t *testing.T) fetchEnv {
	t.Helper()
	base, ok := catalog.Default().Blob("hplip-plugin")
	if !ok {
		t.Fatal("catalog has no hplip-plugin blob")
	}
	eula, _ := catalog.EULA(base.EULA)
	key := blobtest.NewKey()
	archive := blobtest.Archive(runtime.GOARCH)
	files := map[string][]byte{
		"https://vendor.invalid/good.run":     archive,
		"https://vendor.invalid/good.run.asc": blobtest.Sign(key, archive),
	}
	mk := func(id string) catalog.Blob {
		b := base
		b.ID, b.Enabled = id, true
		b.URL = "https://vendor.invalid/" + id + ".run"
		b.Verify = catalog.Verify{PGPSignatureURL: b.URL + ".asc", PGPKey: "keys/test.asc"}
		return b
	}
	known := map[string]catalog.Blob{"good": mk("good"), "bad": mk("bad")}
	dir := t.TempDir()
	return fetchEnv{version: eula.Version, cfg: FetchConfig{
		Dir:       filepath.Join(dir, "blobs"),
		StatusOut: filepath.Join(dir, "blobs.json"),
		Client:    &http.Client{Transport: blobtest.Transport(files)},
		blob:      func(id string) (catalog.Blob, bool) { b, ok := known[id]; return b, ok },
		key:       func(string) (openpgp.EntityList, error) { return openpgp.EntityList{key}, nil },
	}}
}

func readStatuses(t *testing.T, path string) map[string]device.BlobStatus {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	var list []device.BlobStatus
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("status file %s: %v", raw, err)
	}
	out := map[string]device.BlobStatus{}
	for _, s := range list {
		out[s.ID] = s
	}
	return out
}

func TestFetch(t *testing.T) {
	t.Run("optional blob fails: exit 0, every status written", func(t *testing.T) {
		e := newFetchEnv(t)
		e.cfg.Blobs = []string{"good@" + e.version, "bad@" + e.version}
		e.cfg.Accept = []string{e.accept()}
		e.cfg.Optional = []string{"bad"}
		if err := Fetch(context.Background(), e.cfg); err != nil {
			t.Fatalf("Fetch = %v, want nil", err)
		}
		st := readStatuses(t, e.cfg.StatusOut)
		if st["good"].State != device.BlobInstalled || st["bad"].State != device.BlobFailed || len(st) != 2 {
			t.Errorf("statuses = %+v, want good installed and bad failed", st)
		}
		if fi, _ := os.Stat(e.cfg.StatusOut); fi.Mode().Perm() != 0o644 {
			t.Errorf("status file mode = %v, want 0644", fi.Mode().Perm())
		}
	})
	t.Run("required blob fails: exit 1, every status written", func(t *testing.T) {
		e := newFetchEnv(t)
		e.cfg.Blobs = []string{"good@" + e.version, "bad@" + e.version}
		e.cfg.Accept = []string{e.accept()}
		if err := Fetch(context.Background(), e.cfg); err == nil || !strings.Contains(err.Error(), "bad") {
			t.Fatalf("Fetch = %v, want an error naming bad", err)
		}
		st := readStatuses(t, e.cfg.StatusOut)
		if st["good"].State != device.BlobInstalled || st["bad"].State != device.BlobFailed {
			t.Errorf("statuses = %+v, want good installed and bad failed", st)
		}
	})
	t.Run("not accepted is missing, not a failure", func(t *testing.T) {
		e := newFetchEnv(t)
		e.cfg.Blobs = []string{"good@" + e.version}
		e.cfg.Accept = []string{"hplip-plugin@0.0.1@" + licenseSHA}
		if err := Fetch(context.Background(), e.cfg); err != nil {
			t.Fatalf("Fetch = %v", err)
		}
		if st := readStatuses(t, e.cfg.StatusOut); st["good"].State != device.BlobMissing {
			t.Errorf("statuses = %+v, want missing", st)
		}
	})
	t.Run("unknown blob fails", func(t *testing.T) {
		e := newFetchEnv(t)
		e.cfg.Blobs = []string{"nope@1"}
		if err := Fetch(context.Background(), e.cfg); err == nil {
			t.Fatal("Fetch = nil, want an error")
		}
		if st := readStatuses(t, e.cfg.StatusOut); st["nope"].State != device.BlobFailed {
			t.Errorf("statuses = %+v, want failed", st)
		}
	})
	t.Run("no blobs writes an empty list", func(t *testing.T) {
		e := newFetchEnv(t)
		if err := Fetch(context.Background(), e.cfg); err != nil {
			t.Fatal(err)
		}
		if raw, _ := os.ReadFile(e.cfg.StatusOut); strings.TrimSpace(string(raw)) != "[]" {
			t.Errorf("status file = %q, want []", raw)
		}
	})
	t.Run("bad flags", func(t *testing.T) {
		for _, cfg := range []FetchConfig{
			{Blobs: []string{"no-version"}},
			{Accept: []string{"@1"}},
			{Accept: []string{"hplip-plugin@3.24.4"}},
			{Accept: []string{"hplip-plugin@3.24.4@" + strings.ToUpper(licenseSHA)}},
			{Accept: []string{"hplip-plugin@3.24.4@abc"}},
			{Accept: []string{"hplip-plugin@3.24.4@" + licenseSHA + "@x"}},
			{Blobs: []string{"good@1"}, Optional: []string{"other"}},
		} {
			e := newFetchEnv(t)
			cfg.StatusOut, cfg.blob, cfg.key = e.cfg.StatusOut, e.cfg.blob, e.cfg.key
			if err := Fetch(context.Background(), cfg); err == nil {
				t.Errorf("Fetch(%+v) = nil, want an error", cfg)
			}
		}
	})
	t.Run("real catalog: key from catalog.BlobKey, not accepted is missing, no request", func(t *testing.T) {
		dir := t.TempDir()
		cfg := FetchConfig{
			Dir: filepath.Join(dir, "blobs"), StatusOut: filepath.Join(dir, "blobs.json"),
			Blobs:  []string{"hplip-plugin@" + newFetchEnv(t).version},
			Client: &http.Client{Transport: failTransport(t)},
		}
		if err := Fetch(context.Background(), cfg); err != nil {
			t.Fatalf("Fetch = %v", err)
		}
		st := readStatuses(t, cfg.StatusOut)["hplip-plugin"]
		if b, _ := catalog.Default().Blob("hplip-plugin"); b.Enabled && st.State != device.BlobMissing {
			t.Errorf("status = %+v, want missing", st)
		} else if !b.Enabled && st.State != device.BlobDisabled {
			t.Errorf("status = %+v, want disabled", st)
		}
	})
	t.Run("disabled required blob: no key lookup, no request, exit 0", func(t *testing.T) {
		e := newFetchEnv(t)
		b, _ := e.cfg.blob("good")
		b.Enabled, b.DisabledMessage = false, "HP plug-in not yet supported"
		e.cfg.blob = func(string) (catalog.Blob, bool) { return b, true }
		e.cfg.key = func(string) (openpgp.EntityList, error) {
			t.Error("BlobKey called for a disabled blob")
			return nil, nil
		}
		e.cfg.Client = &http.Client{Transport: failTransport(t)}
		e.cfg.Blobs = []string{"good@" + e.version}
		e.cfg.Accept = []string{e.accept()}
		if err := Fetch(context.Background(), e.cfg); err != nil {
			t.Fatalf("Fetch = %v", err)
		}
		if st := readStatuses(t, e.cfg.StatusOut)["good"]; st.State != device.BlobDisabled || st.Message != "HP plug-in not yet supported" {
			t.Errorf("status = %+v, want disabled with its message", st)
		}
	})
	t.Run("unwritable status file is exit 1", func(t *testing.T) {
		e := newFetchEnv(t)
		e.cfg.StatusOut = filepath.Join(t.TempDir(), "missing-dir", "blobs.json")
		if err := Fetch(context.Background(), e.cfg); err == nil {
			t.Fatal("Fetch = nil, want an error")
		}
	})
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// failTransport fails the test on any request.
func failTransport(t *testing.T) http.RoundTripper {
	return roundTrip(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request to %s", r.URL)
		return nil, errors.New("no network in this test")
	})
}

// --- run loop ---

// plane is a scripted in-memory control plane.
type plane struct {
	mu      sync.Mutex
	desired device.DesiredState
	err     error
	reports chan device.AgentReport
}

func newPlane(ds device.DesiredState) *plane {
	return &plane{desired: ds, reports: make(chan device.AgentReport, 1000)}
}

func (p *plane) set(f func(*plane)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

func (p *plane) report(_ context.Context, r device.AgentReport) (device.DesiredState, error) {
	select {
	case p.reports <- r:
	default:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.desired, p.err
}

// waitReport returns the first report that satisfies ok, failing after 10 s.
func waitReport(t *testing.T, ch <-chan device.AgentReport, what string, ok func(device.AgentReport) bool) device.AgentReport {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case r := <-ch:
			if ok(r) {
				return r
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a report with %s", what)
		}
	}
}

// start runs core.Loop with a 5 ms interval until the test ends.
func start(t *testing.T, core *Core, report device.ReportFunc) {
	t.Helper()
	core.Interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); core.Loop(ctx, report) }()
	t.Cleanup(func() { cancel(); <-done })
}

func hpInventory() device.Inventory {
	return device.Inventory{Devices: []device.USBDevice{{
		VendorID: "03f0", ProductID: "2b17", Serial: "ABC123", Product: "HP LaserJet 1020",
		Interfaces: []device.USBInterface{{Class: 7, SubClass: 1, Protocol: 2}}, DeviceID: hpID,
	}}}
}

func newPAPPL(t *testing.T) (*papplfake.System, string) {
	t.Helper()
	fake := papplfake.New(papplfake.Options{
		Devices: []papplfake.Device{{URI: "cups:usb://HP/LaserJet%201020?serial=ABC123", Info: "HP LaserJet 1020", DeviceID: hpID}},
		Drivers: []papplfake.Driver{{Name: "hp_laserjet_1020", Description: "HP LaserJet 1020", MatchMFG: "HP"}},
	})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return fake, "ipp://" + srv.Listener.Addr().String() + "/ipp/system"
}

func queue(media string) *device.QueueSpec {
	return &device.QueueSpec{
		Name: "Printer", Location: "Hall", Driver: "auto", Schemes: []string{"usb"},
		Media: media, Sides: "one-sided", ColorMode: "auto", Quality: "normal",
	}
}

func TestRunHTTPToken(t *testing.T) {
	got := make(chan device.AgentReport, 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != device.AgentReportPath || r.Header.Get("Authorization") != "Bearer s3cret" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		var rep device.AgentReport
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case got <- rep:
		default:
		}
		_ = json.NewEncoder(w).Encode(device.DesiredState{Hash: "h1", Browse: device.BrowseTypes})
	}))
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	core, report, err := newRun(Config{ControlPlaneURL: srv.URL, TokenFile: tokenFile, PodUID: "pod-1"})
	if err != nil {
		t.Fatal(err)
	}
	start(t, core, report)
	first := waitReport(t, got, "pod uid", func(r device.AgentReport) bool { return true })
	if first.PodUID != "pod-1" || !first.DevicePresent || first.AppliedHash != "" {
		t.Errorf("first report = %+v, want pod-1, present, no applied hash", first)
	}
	waitReport(t, got, "applied hash h1", func(r device.AgentReport) bool { return r.AppliedHash == "h1" })

	wrong := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(wrong, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, report, _ = newRun(Config{ControlPlaneURL: srv.URL, TokenFile: wrong})
	if _, err := report(context.Background(), device.AgentReport{}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("wrong token: err = %v, want 401", err)
	}
	if _, _, err := newRun(Config{TokenFile: wrong}); err == nil {
		t.Error("newRun without --control-plane-url succeeded")
	}
}

func TestReportJSONShape(t *testing.T) {
	p := newPlane(device.DesiredState{})
	start(t, NewCore("pod", device.Inventory{}), p.report)
	r := waitReport(t, p.reports, "any", func(device.AgentReport) bool { return true })
	raw, _ := json.Marshal(r)
	for _, want := range []string{`"devices":[]`, `"services":[]`, `"blobs":[]`, `"eulas":[]`, `"devicePresent":true`, `"queue":null`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("report JSON lacks %s: %s", want, raw)
		}
	}
}

func TestQueueReconcile(t *testing.T) {
	fake, url := newPAPPL(t)
	p := newPlane(device.DesiredState{Hash: "h", Queue: queue("iso_a4_210x297mm"), Browse: device.BrowseTypes})
	core := NewCore("pod", hpInventory())
	core.PAPPLURL = func() string { return url }
	start(t, core, p.report)

	r := waitReport(t, p.reports, "queue ok", func(r device.AgentReport) bool { return r.Queue != nil && r.Queue.State == device.QueueOK })
	if !strings.HasPrefix(r.Queue.PrinterURI, "ipp://127.0.0.1:") || r.Queue.DeviceURI != "cups:usb://HP/LaserJet%201020?serial=ABC123" {
		t.Errorf("queue result = %+v", r.Queue)
	}
	if ps := fake.Printers(); len(ps) != 1 || ps[0].Media != "iso_a4_210x297mm" || ps[0].Location != "Hall" {
		t.Fatalf("printers = %+v", ps)
	}

	p.set(func(p *plane) { p.desired.Queue = queue("na_letter_8.5x11in") })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if ps := fake.Printers(); len(ps) == 1 && ps[0].Media == "na_letter_8.5x11in" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("media change never reached PAPPL: %+v", fake.Printers())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestQueueReconcileError(t *testing.T) {
	_, url := newPAPPL(t)
	q := queue("iso_a4_210x297mm")
	q.DeviceURI, q.Driver = "ipp://127.0.0.1:1/ipp/print", "hp_laserjet_1020" // the fake only takes cups: URIs
	p := newPlane(device.DesiredState{Queue: q})
	core := NewCore("pod", hpInventory())
	core.PAPPLURL = func() string { return url }
	start(t, core, p.report)
	r := waitReport(t, p.reports, "queue error", func(r device.AgentReport) bool { return r.Queue != nil })
	if r.Queue.State != device.QueueError || r.Queue.Message == "" {
		t.Errorf("queue result = %+v, want an error with a message", r.Queue)
	}
}

func TestQueueWithoutPAPPLURL(t *testing.T) {
	p := newPlane(device.DesiredState{Queue: queue("iso_a4_210x297mm")})
	start(t, NewCore("pod", hpInventory()), p.report)
	for i := range 3 { // the loop keeps going
		r := waitReport(t, p.reports, "queue", func(r device.AgentReport) bool { return r.Queue != nil })
		if *r.Queue != (device.QueueResult{State: device.QueueError, Message: "agent has no --pappl-url"}) {
			t.Fatalf("report %d queue = %+v", i, r.Queue)
		}
	}
}

func TestDeviceVanishes(t *testing.T) {
	root, printerDir := sysfs(t)
	invFile := filepath.Join(t.TempDir(), "inventory.json")
	if err := WriteJSON(invFile, Probe(root, t.TempDir(), nil)); err != nil {
		t.Fatal(err)
	}
	core, _, err := newRun(Config{ControlPlaneURL: "http://unused", TokenFile: "unused", InventoryFile: invFile, SysRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	p := newPlane(device.DesiredState{})
	start(t, core, p.report)
	r := waitReport(t, p.reports, "present", func(r device.AgentReport) bool { return r.DevicePresent })
	if len(r.Inventory.Devices) != 2 {
		t.Errorf("inventory = %+v, want the probed devices", r.Inventory.Devices)
	}
	if err := os.RemoveAll(printerDir); err != nil {
		t.Fatal(err)
	}
	waitReport(t, p.reports, "device gone", func(r device.AgentReport) bool { return !r.DevicePresent })

	if _, _, err := newRun(Config{ControlPlaneURL: "x", TokenFile: "x", InventoryFile: filepath.Join(root, "missing")}); err == nil {
		t.Error("newRun with a missing inventory file succeeded")
	}
}

func TestBackoff(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // unreachable
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	core, report, err := newRun(Config{ControlPlaneURL: srv.URL, TokenFile: tokenFile})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	core.after = func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		if len(waits) == 8 {
			cancel()
		}
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	core.Loop(ctx, func(ctx context.Context, r device.AgentReport) (device.DesiredState, error) {
		ds, err := report(ctx, r)
		if err == nil {
			t.Error("report to a closed server succeeded")
		}
		return ds, err
	})
	s := time.Second
	want := []time.Duration{s, 2 * s, 4 * s, 8 * s, 16 * s, 30 * s, 30 * s, 30 * s}
	if !reflect.DeepEqual(waits, want) {
		t.Errorf("waits = %v, want %v", waits, want)
	}

	// After a success the wait is back to the interval (a changed desired state first runs at once).
	p := newPlane(device.DesiredState{})
	core = NewCore("pod", device.Inventory{})
	waits = nil
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	n := 0
	core.after = func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	core.Loop(ctx2, func(ctx context.Context, r device.AgentReport) (device.DesiredState, error) {
		n++
		switch n {
		case 1:
			return device.DesiredState{}, errors.New("down")
		case 4:
			cancel2()
		}
		return p.report(ctx, r)
	})
	if want := []time.Duration{s, 5 * s}; !reflect.DeepEqual(waits, want) {
		t.Errorf("waits = %v, want %v (backoff, then the immediate cycle for the first desired state, then the interval)", waits, want)
	}
}

func TestBlobStatusReread(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blobs.json")
	core, _, err := newRun(Config{ControlPlaneURL: "x", TokenFile: "x", BlobStatusFile: path})
	if err != nil {
		t.Fatal(err)
	}
	p := newPlane(device.DesiredState{})
	start(t, core, p.report)

	r := waitReport(t, p.reports, "blobs", func(device.AgentReport) bool { return true })
	if r.Blobs == nil || len(r.Blobs) != 0 {
		t.Fatalf("no file: blobs = %#v, want []", r.Blobs)
	}
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`[{"id":"hplip-plugin","version":"3.24.4","state":"failed","message":"boom"}]`)
	waitReport(t, p.reports, "failed blob", func(r device.AgentReport) bool {
		return len(r.Blobs) == 1 && r.Blobs[0].State == device.BlobFailed && r.Blobs[0].Message == "boom"
	})
	write(`[{"id":"hplip-plugin","version":"3.24.4","state":"installed","message":""}]`)
	waitReport(t, p.reports, "installed blob", func(r device.AgentReport) bool {
		return len(r.Blobs) == 1 && r.Blobs[0].State == device.BlobInstalled
	})
	write(`{not json`)
	waitReport(t, p.reports, "empty blobs after a malformed file", func(r device.AgentReport) bool { return r.Blobs != nil && len(r.Blobs) == 0 })
	waitReport(t, p.reports, "the loop still running", func(device.AgentReport) bool { return true })
}

func TestBrowse(t *testing.T) {
	var mu sync.Mutex
	var seen [][]string
	svc := device.DNSSDService{Type: device.ServiceIPP, Name: "Printer", Host: "node.local", Port: 631, TXT: map[string]string{"rp": "ipp/print"}}
	fail := true
	browse := func(_ context.Context, types []string) ([]device.DNSSDService, error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, types)
		if fail {
			fail = false
			return nil, errors.New("avahi down")
		}
		return []device.DNSSDService{svc}, nil
	}
	core, _, err := newRun(Config{ControlPlaneURL: "x", TokenFile: "x", Browse: browse})
	if err != nil {
		t.Fatal(err)
	}
	p := newPlane(device.DesiredState{Browse: []string{"_custom._tcp"}})
	start(t, core, p.report)

	r := waitReport(t, p.reports, "first", func(device.AgentReport) bool { return true })
	if r.Services == nil || len(r.Services) != 0 {
		t.Errorf("failed browse: services = %#v, want [] and the report still sent", r.Services)
	}
	waitReport(t, p.reports, "services", func(r device.AgentReport) bool { return len(r.Services) == 1 && r.Services[0].Name == "Printer" })
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(seen[0], device.BrowseTypes) || !reflect.DeepEqual(seen[len(seen)-1], []string{"_custom._tcp"}) {
		t.Errorf("browsed types = %v, want BrowseTypes first, then the desired Browse", seen)
	}
}

func TestWake(t *testing.T) {
	p := newPlane(device.DesiredState{})
	core := NewCore("pod", device.Inventory{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); core.Loop(ctx, p.report) }()
	defer func() { cancel(); <-done }()
	// Two reports arrive at once (the second for the first desired state), then the 5 s interval.
	for range 2 {
		waitReport(t, p.reports, "start", func(device.AgentReport) bool { return true })
	}
	core.Wake()
	core.Wake() // never blocks
	select {
	case <-p.reports:
	case <-time.After(2 * time.Second):
		t.Fatal("Wake did not run a cycle")
	}
}
