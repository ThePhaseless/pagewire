package k8s

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ThePhaseless/pagewire/internal/device"
)

const selfImage = "ghcr.io/thephaseless/pagewire:0.0.0-test"

func goldenParams() Params {
	return Params{
		Namespace:    "printing",
		Owner:        Owner{Name: "pagewire", UID: "00000000-0000-0000-0000-000000000001"},
		Registry:     "ghcr.io/thephaseless",
		Tag:          "0.0.0-test",
		AgentURL:     "http://pagewire.printing.svc:8081",
		USBResource:  "devic.es/printer",
		BlobPVC:      "pagewire-blobs",
		SelfImage:    selfImage,
		BlobWorkload: true,
	}
}

// paramsFor is goldenParams with the slot of the golden spec name.
func paramsFor(name string) Params {
	p := goldenParams()
	if name == "multi" {
		p.Slot, p.BlobWorkload = 1, false
	}
	return p
}

func withHash(s *device.Spec) *device.Spec {
	s.Hash = device.SpecHash(s)
	return s
}

// goldenTextSHA is a synthetic accepted licence text sha256.
const goldenTextSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// USB keys of the office fixture's devices.
var (
	keyHP      = device.USBKey{VendorID: "03f0", ProductID: "2754", Serial: "FAKE0005"}
	keySamsung = device.USBKey{VendorID: "04e8", ProductID: "342e", Serial: "FAKE0008"}
	keyLiDE    = device.USBKey{VendorID: "04a9", ProductID: "1904", Serial: "FAKE0011"}
)

var goldenSpecs = map[string]*device.Spec{
	"probe": withHash(&device.Spec{Mode: device.ModeProbe}),
	"ippusb": withHash(&device.Spec{
		Mode:     device.ModeUSB,
		Printers: []device.PrinterSpec{{ID: "printer", Family: "ipp-usb", USB: keyHP}},
		Scanners: []device.ScannerSpec{{ID: "scanner", Family: "ipp-usb", USB: keyHP}},
	}),
	"hplip-airsane-blob": withHash(&device.Spec{
		Mode:          device.ModeUSB,
		Printers:      []device.PrinterSpec{{ID: "printer", Family: "hplip", USB: keyHP}},
		Scanners:      []device.ScannerSpec{{ID: "scanner", Family: "airsane", SaneBackends: []string{"hpaio"}, USB: keyHP}},
		Blobs:         []device.BlobRef{{ID: "hplip-plugin", Version: "3.24.4", Optional: true}},
		AcceptedEULAs: []device.EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: goldenTextSHA}},
	}),
	"simulated": withHash(&device.Spec{
		Mode: device.ModeSimulated,
		Printers: []device.PrinterSpec{{
			ID: "printer", Family: "sim", Name: "Living room printer", Location: "Living room", Port: 8631,
		}},
		Scanners: []device.ScannerSpec{{ID: "scanner", Family: "sim"}},
	}),
	// The office fixture's devices on one node of a multi-node install, in slot 1.
	"multi": withHash(&device.Spec{
		Mode: device.ModeUSB,
		Node: "node-b",
		Printers: []device.PrinterSpec{
			{ID: "hp", Family: "ipp-usb", USB: keyHP},
			{ID: "samsung", Family: "legacy", USB: keySamsung},
		},
		Scanners: []device.ScannerSpec{
			{ID: "hp-scanner", Family: "ipp-usb", USB: keyHP},
			{ID: "lide", Family: "airsane", SaneBackends: []string{"genesys"}, USB: keyLiDE},
			{ID: "samsung-scanner", Family: "airsane", SaneBackends: []string{"xerox_mfp"}, USB: keySamsung},
		},
	}),
}

func render(t *testing.T, s *device.Spec, p Params) (*appsv1.Deployment, *corev1.ConfigMap) {
	t.Helper()
	d, cm, err := Render(s, p)
	if err != nil {
		t.Fatal(err)
	}
	var dep appsv1.Deployment
	var c corev1.ConfigMap
	roundTrip(t, d, &dep)
	roundTrip(t, cm, &c)
	return &dep, &c
}

func roundTrip(t *testing.T, in, out any) {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
}

func checkGolden(t *testing.T, file string, obj any) {
	t.Helper()
	j, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	got, err := yaml.JSONToYAML(j)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", file)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the render; rerun with UPDATE_GOLDEN=1 and review the diff\n%s", file, got)
	}
}

func TestGolden(t *testing.T) {
	for name, s := range goldenSpecs {
		d, cm, err := Render(s, paramsFor(name))
		if err != nil {
			t.Fatal(name, err)
		}
		checkGolden(t, "device-"+name+".golden.yaml", d)
		if name == "hplip-airsane-blob" {
			checkGolden(t, "configmap-hplip-airsane.golden.yaml", cm)
		}
	}
}

func names(cs []corev1.Container) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func allContainers(d *appsv1.Deployment) []corev1.Container {
	return append(slices.Clone(d.Spec.Template.Spec.InitContainers), d.Spec.Template.Spec.Containers...)
}

func find(t *testing.T, d *appsv1.Deployment, name string) corev1.Container {
	t.Helper()
	for _, c := range allContainers(d) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no container %q", name)
	return corev1.Container{}
}

func TestLayout(t *testing.T) {
	cases := map[string]struct{ inits, mains []string }{
		"probe":              {[]string{"probe", "dbus", "avahi"}, []string{"agent"}},
		"ippusb":             {[]string{"probe", "dbus", "avahi", "agent"}, []string{"ipp-usb"}},
		"hplip-airsane-blob": {[]string{"probe", "dbus", "avahi", "agent", "fetch-blobs"}, []string{"printer-app-hplip", "airsane"}},
		"simulated":          {[]string{"probe", "dbus", "avahi", "agent"}, []string{"sim-printer-0", "airsane"}},
		"multi":              {[]string{"probe", "dbus", "avahi", "agent"}, []string{"ipp-usb", "printer-app-legacy", "airsane"}},
	}
	for name, want := range cases {
		d, _ := render(t, goldenSpecs[name], paramsFor(name))
		pod := d.Spec.Template.Spec
		if got := names(pod.InitContainers); !slices.Equal(got, want.inits) {
			t.Errorf("%s inits = %v, want %v", name, got, want.inits)
		}
		if got := names(pod.Containers); !slices.Equal(got, want.mains) || len(got) == 0 {
			t.Errorf("%s containers = %v, want %v", name, got, want.mains)
		}
		for _, c := range pod.Containers {
			if c.RestartPolicy != nil {
				t.Errorf("%s: regular container %s has a restartPolicy", name, c.Name)
			}
		}
		for _, c := range pod.InitContainers {
			sidecar := c.Name == "dbus" || c.Name == "avahi" || c.Name == "agent"
			isAlways := c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
			if sidecar != isAlways {
				t.Errorf("%s: init %s restartPolicy Always = %v", name, c.Name, isAlways)
			}
		}
		if !pod.HostNetwork || pod.DNSPolicy != corev1.DNSClusterFirstWithHostNet || *pod.AutomountServiceAccountToken ||
			*pod.EnableServiceLinks || *pod.TerminationGracePeriodSeconds != 10 || *pod.SecurityContext.FSGroup != 65532 ||
			pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s: pod settings wrong: %+v", name, pod)
		}
		if *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
			t.Errorf("%s: replicas or strategy wrong", name)
		}
	}
}

// TestMixedSpec: simulated entries join a usb workload (slot 0), and each
// printer family and simulated printer gets its own container.
func TestMixedSpec(t *testing.T) {
	spec := withHash(&device.Spec{
		Mode: device.ModeUSB,
		Printers: []device.PrinterSpec{
			{ID: "a", Family: "legacy", USB: keySamsung},
			{ID: "b", Family: "hplip", USB: keyHP},
			{ID: "c", Family: "legacy", USB: device.USBKey{VendorID: "04e8", ProductID: "3413"}},
			{ID: "s1", Family: "sim", Name: "One", Port: 8631},
			{ID: "s2", Family: "sim", Name: "Two", Port: 8632},
		},
		Scanners: []device.ScannerSpec{{ID: "sim", Family: "sim"}},
	})
	d, cm := render(t, spec, goldenParams())
	want := []string{"printer-app-hplip", "printer-app-legacy", "sim-printer-0", "sim-printer-1", "airsane"}
	if got := names(d.Spec.Template.Spec.Containers); !slices.Equal(got, want) {
		t.Fatalf("containers %v, want %v (PAPPL families in catalog order)", got, want)
	}
	if a := find(t, d, "sim-printer-1"); !slices.Contains(a.Args, "8632") || a.Args[len(a.Args)-1] != "Two" {
		t.Errorf("sim-printer-1 args %v", a.Args)
	}
	if a := find(t, d, "printer-app-legacy"); !slices.Contains(a.Args, "server-port=8005") {
		t.Errorf("legacy args %v", a.Args)
	}
	// AirSane serves only the simulated scanner: no USB, not root.
	if a := find(t, d, "airsane"); *a.SecurityContext.RunAsUser == 0 {
		t.Error("airsane runs as root without a USB scanner")
	}
	if got := cm.Data["dll.conf"]; got != "test\n" {
		t.Errorf("dll.conf %q", got)
	}
	// Two printer apps share the pod: each keeps its state in its own subdirectory.
	for _, n := range []string{"printer-app-hplip", "printer-app-legacy"} {
		fam := strings.TrimPrefix(n, "printer-app-")
		for _, m := range find(t, d, n).VolumeMounts {
			if strings.HasPrefix(m.Name, "app-") && m.SubPath != fam {
				t.Errorf("%s mount %s subPath %q, want %q", n, m.Name, m.SubPath, fam)
			}
		}
	}
}

func TestSecurityAndUSB(t *testing.T) {
	usbContainers := map[string][]string{
		"probe":              {"probe"},
		"ippusb":             {"probe", "ipp-usb"},
		"hplip-airsane-blob": {"probe", "printer-app-hplip", "airsane"},
		"simulated":          nil,
		"multi":              {"probe", "ipp-usb", "printer-app-legacy", "airsane"},
	}
	res := corev1.ResourceName("devic.es/printer")
	for name, wantUSB := range usbContainers {
		d, _ := render(t, goldenSpecs[name], paramsFor(name))
		var gotUSB []string
		for _, c := range allContainers(d) {
			sc := c.SecurityContext
			if sc == nil || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation ||
				!slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || sc.RunAsUser == nil {
				t.Errorf("%s/%s: security context %+v", name, c.Name, sc)
				continue
			}
			for _, r := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
				if _, ok := c.Resources.Requests[r]; !ok {
					t.Errorf("%s/%s: no %s request", name, c.Name, r)
				}
				if _, ok := c.Resources.Limits[r]; !ok {
					t.Errorf("%s/%s: no %s limit", name, c.Name, r)
				}
			}
			if q, ok := c.Resources.Limits[res]; ok {
				gotUSB = append(gotUSB, c.Name)
				if q.Value() != 1 || c.Resources.Requests[res] != q {
					t.Errorf("%s/%s: USB request %v limit %v, want 1", name, c.Name, c.Resources.Requests[res], q)
				}
				if *sc.RunAsUser != 0 {
					t.Errorf("%s/%s: USB container uid %d", name, c.Name, *sc.RunAsUser)
				}
			} else if *sc.RunAsUser == 0 || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
				t.Errorf("%s/%s: non-USB container runs as root", name, c.Name)
			}
		}
		if !slices.Equal(gotUSB, wantUSB) {
			t.Errorf("%s: USB containers %v, want %v", name, gotUSB, wantUSB)
		}
	}
}

// TestPlanUSBKeys: each daemon lists the devices it serves; the probe all of them.
func TestPlanUSBKeys(t *testing.T) {
	pod, err := Plan(goldenSpecs["multi"], paramsFor("multi"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]device.USBKey{
		"ipp-usb":            {keyHP},
		"printer-app-legacy": {keySamsung},
		"airsane":            {keyLiDE, keySamsung},
	}
	for _, c := range append(slices.Clone(pod.Inits), pod.Mains...) {
		if c.Name == "probe" {
			if !c.USBAll || len(c.USB) != 0 {
				t.Errorf("probe USBAll %v USB %v", c.USBAll, c.USB)
			}
			continue
		}
		if c.USBAll || !slices.Equal(c.USB, want[c.Name]) {
			t.Errorf("%s: USB %v USBAll %v, want %v", c.Name, c.USB, c.USBAll, want[c.Name])
		}
	}
}

// TestBlobWorkload: only the blob workload's agent stages EULAs on the blob
// volume, and only it may fetch blobs (the PVC is ReadWriteOnce).
func TestBlobWorkload(t *testing.T) {
	for _, name := range []string{"probe", "ippusb", "multi"} {
		for _, blob := range []bool{true, false} {
			p := paramsFor(name)
			p.BlobWorkload = blob
			d, _ := render(t, goldenSpecs[name], p)
			a := find(t, d, "agent")
			if got := slices.Contains(a.Args, "--blob-dir"); got != blob {
				t.Errorf("%s blob workload %v: agent --blob-dir %v", name, blob, got)
			}
			hasVol := slices.ContainsFunc(d.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == "blobs" })
			if hasVol != blob {
				t.Errorf("%s blob workload %v: blobs volume %v", name, blob, hasVol)
			}
		}
	}
	p := goldenParams()
	p.BlobWorkload = false
	if _, _, err := Render(goldenSpecs["hplip-airsane-blob"], p); err == nil {
		t.Error("blobs rendered on a workload that is not the blob workload")
	}
}

// TestNoBlobPVC: without a blob PVC the agent stages nothing and mounts no
// blob volume.
func TestNoBlobPVC(t *testing.T) {
	p := goldenParams()
	p.BlobPVC = ""
	for _, name := range []string{"probe", "ippusb", "simulated"} {
		d, _ := render(t, goldenSpecs[name], p)
		a := find(t, d, "agent")
		if slices.Contains(a.Args, "--blob-dir") {
			t.Errorf("%s agent args %v", name, a.Args)
		}
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.Name == "blobs" {
				t.Errorf("%s: blobs volume without a PVC", name)
			}
		}
	}
}

func TestSimulatedNameIsNotAnOption(t *testing.T) {
	spec := *goldenSpecs["simulated"]
	spec.Printers = []device.PrinterSpec{{ID: "p", Family: "sim", Name: "-h", Port: 8631}}
	if _, _, err := Render(&spec, goldenParams()); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("Render with name -h: %v, want an error naming the name", err)
	}
}

func TestUnknownFamily(t *testing.T) {
	spec := &device.Spec{Mode: device.ModeUSB, Printers: []device.PrinterSpec{{ID: "p", Family: "nope", USB: keyHP}}}
	if _, err := Plan(spec, goldenParams()); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("Plan: %v", err)
	}
}

func TestIPPUSBPortRange(t *testing.T) {
	_, cm := render(t, goldenSpecs["ippusb"], goldenParams())
	conf := cm.Data["ipp-usb.conf"]
	for _, want := range []string{
		"http-min-port = " + strconv.Itoa(device.IPPUSBMinPort),
		"http-max-port = " + strconv.Itoa(device.IPPUSBMaxPort),
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("ipp-usb.conf lacks %q:\n%s", want, conf)
		}
	}
	if device.IPPUSBMinPort != 60000 || device.IPPUSBMaxPort != 60009 {
		t.Errorf("ipp-usb ports %d-%d, want 60000-60009", device.IPPUSBMinPort, device.IPPUSBMaxPort)
	}
}

func TestLANExposure(t *testing.T) {
	_, cm := render(t, goldenSpecs["ippusb"], goldenParams())
	conf := cm.Data["ipp-usb.conf"]
	if !strings.Contains(conf, "[auth uid]\n  print, scan = *\n") || strings.Contains(conf, "all = *") || strings.Contains(conf, "config") {
		t.Errorf("ipp-usb.conf must allow only print and scan:\n%s", conf)
	}
	airsanes := 0
	for name, s := range goldenSpecs {
		d, _ := render(t, s, paramsFor(name))
		for _, c := range allContainers(d) {
			if c.Name == "airsane" {
				airsanes++
				if !slices.Contains(c.Args, "--web-interface=false") {
					t.Errorf("%s airsane args %v lack --web-interface=false", name, c.Args)
				}
			}
			if strings.HasPrefix(c.Name, "printer-app-") {
				for _, a := range c.Args {
					if strings.HasPrefix(a, "auth-service=") {
						t.Errorf("%s %s has %q", name, c.Name, a)
					}
				}
			}
		}
	}
	if airsanes == 0 {
		t.Error("no golden spec renders an airsane container")
	}
}

func TestNoUSBResource(t *testing.T) {
	p := goldenParams()
	p.USBResource = ""
	for _, name := range []string{"probe", "ippusb", "hplip-airsane-blob"} {
		d, cm, err := Render(goldenSpecs[name], p)
		if !errors.Is(err, ErrNoUSBResource) || d != nil || cm != nil {
			t.Errorf("%s: got %v, %v, %v", name, d, cm, err)
		}
	}
	if _, _, err := Render(goldenSpecs["simulated"], p); err != nil {
		t.Errorf("simulated: %v", err)
	}
}

func TestArgs(t *testing.T) {
	fixed := []string{
		"--control-plane-url", "http://pagewire.printing.svc:8081",
		"--token-file", "/var/run/pagewire/token",
		"--pod-uid", "$(POD_UID)",
		"--dbus-address", "unix:path=/run/dbus/system_bus_socket",
		"--inventory", "/run/pagewire/inventory.json",
		"--blob-status", "/run/pagewire/blobs.json",
	}
	for name, s := range goldenSpecs {
		d, _ := render(t, s, paramsFor(name))
		a := find(t, d, "agent")
		want := append([]string{"agent", "run"}, fixed...)
		want = append(want, "--workload", s.Node)
		if name != "simulated" && name != "multi" {
			want = append(want, "--blob-dir", "/blobs")
			if !slices.ContainsFunc(a.VolumeMounts, func(m corev1.VolumeMount) bool {
				return m.Name == "blobs" && m.MountPath == "/blobs" && !m.ReadOnly
			}) {
				t.Errorf("%s agent: no writable /blobs: %+v", name, a.VolumeMounts)
			}
		}
		if !slices.Equal(a.Command, []string{"/pagewire"}) || !slices.Equal(a.Args, want) {
			t.Errorf("%s agent: %v %v", name, a.Command, a.Args)
		}
		if len(a.Env) != 1 || a.Env[0].Name != "POD_UID" || a.Env[0].ValueFrom.FieldRef.FieldPath != "metadata.uid" {
			t.Errorf("%s agent env: %+v", name, a.Env)
		}
		for _, n := range []string{"probe", "agent", "fetch-blobs"} {
			for _, c := range allContainers(d) {
				if c.Name == n && c.Image != selfImage {
					t.Errorf("%s/%s image %s", name, n, c.Image)
				}
			}
		}
		for _, c := range allContainers(d) {
			if c.ImagePullPolicy != corev1.PullIfNotPresent {
				t.Errorf("%s/%s pull policy %s", name, c.Name, c.ImagePullPolicy)
			}
			last := c.Image[strings.LastIndex(c.Image, "/")+1:]
			if !strings.Contains(last, ":") || strings.HasSuffix(c.Image, ":latest") {
				t.Errorf("%s/%s image %s lacks a tag or is latest", name, c.Name, c.Image)
			}
		}
	}

	d, _ := render(t, goldenSpecs["simulated"], goldenParams())
	sim := find(t, d, "sim-printer-0")
	if n := len(sim.Args); n < 3 || !slices.Equal(sim.Args[n-3:], []string{"-l", "Living room", "Living room printer"}) ||
		!slices.Contains(sim.Args, "8631") {
		t.Errorf("sim-printer-0 args %v", sim.Args)
	}

	d, _ = render(t, goldenSpecs["hplip-airsane-blob"], goldenParams())
	fb := find(t, d, "fetch-blobs")
	wantFB := []string{
		"agent", "fetch", "--dir", "/blobs", "--status-out", "/run/pagewire/blobs.json",
		"--blob", "hplip-plugin@3.24.4", "--optional", "hplip-plugin",
		"--accept", "hplip-plugin@3.24.4@" + goldenTextSHA,
	}
	if !slices.Equal(fb.Args, wantFB) {
		t.Errorf("fetch-blobs args %v", fb.Args)
	}
	if !slices.ContainsFunc(fb.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == "/run/pagewire" && !m.ReadOnly }) {
		t.Errorf("fetch-blobs has no writable /run/pagewire: %+v", fb.VolumeMounts)
	}

	required := *goldenSpecs["hplip-airsane-blob"]
	required.Blobs = []device.BlobRef{{ID: "hplip-plugin", Version: "3.24.4"}}
	d, _ = render(t, &required, goldenParams())
	if slices.Contains(find(t, d, "fetch-blobs").Args, "--optional") {
		t.Error("required blob rendered with --optional")
	}
}

func TestLegacyPrinterApp(t *testing.T) {
	spec := withHash(&device.Spec{
		Mode:     device.ModeUSB,
		Printers: []device.PrinterSpec{{ID: "printer", Family: "legacy", USB: keySamsung}},
		Scanners: []device.ScannerSpec{{ID: "scanner", Family: "airsane", SaneBackends: []string{"xerox_mfp"}, USB: keySamsung}},
	})
	d, _ := render(t, spec, goldenParams())
	var drivers []corev1.Container
	for _, c := range allContainers(d) {
		if strings.Contains(c.Image, "pagewire-driver-") {
			drivers = append(drivers, c)
		}
	}
	if len(drivers) != 1 || drivers[0].Name != "printer-app-legacy" {
		t.Fatalf("driver containers %+v, want printer-app-legacy alone", drivers)
	}
	c := drivers[0]
	if c.Image != "ghcr.io/thephaseless/pagewire-driver-legacy:0.0.0-test" {
		t.Errorf("image %s", c.Image)
	}
	if !slices.Equal(c.Command, []string{"/usr/local/bin/legacy-printer-app"}) || !slices.Contains(c.Args, "server-port=8005") {
		t.Errorf("command %v args %v", c.Command, c.Args)
	}
	if a := find(t, d, "agent"); slices.Contains(a.Args, "--pappl-url") {
		t.Errorf("agent args %v: the system URI travels in the desired state", a.Args)
	}
}

func TestPullPolicies(t *testing.T) {
	selfNames := map[string]bool{"probe": true, "agent": true, "fetch-blobs": true}
	policies := func(p Params) (self, other map[corev1.PullPolicy]bool, images map[string]string) {
		d, _ := render(t, goldenSpecs["hplip-airsane-blob"], p)
		self, other, images = map[corev1.PullPolicy]bool{}, map[corev1.PullPolicy]bool{}, map[string]string{}
		for _, c := range allContainers(d) {
			images[c.Name] = c.Image
			if selfNames[c.Name] {
				self[c.ImagePullPolicy] = true
			} else {
				other[c.ImagePullPolicy] = true
			}
		}
		return self, other, images
	}
	one := func(m map[corev1.PullPolicy]bool, want corev1.PullPolicy) bool { return len(m) == 1 && m[want] }

	p := goldenParams()
	p.Tag = "main"
	self, other, _ := policies(p)
	if !one(self, corev1.PullIfNotPresent) || !one(other, corev1.PullAlways) {
		t.Errorf("tag main: self %v other %v", self, other)
	}
	p = goldenParams()
	p.SelfImage = "ghcr.io/thephaseless/pagewire:main"
	if self, _, _ = policies(p); !one(self, corev1.PullAlways) {
		t.Errorf("self main: %v", self)
	}
	p.SelfImage = "ghcr.io/thephaseless/pagewire:main" + digest
	self, _, images := policies(p)
	if !one(self, corev1.PullIfNotPresent) || images["agent"] != p.SelfImage {
		t.Errorf("self digest: %v %s", self, images["agent"])
	}
}

func TestMetadata(t *testing.T) {
	d, _ := render(t, goldenSpecs["ippusb"], goldenParams())
	changed := *goldenSpecs["ippusb"]
	changed.Scanners = nil
	d2, _ := render(t, withHash(&changed), goldenParams())
	a, b := d.Spec.Template.Annotations[SpecHashAnnotation], d2.Spec.Template.Annotations[SpecHashAnnotation]
	if a == "" || a == b {
		t.Errorf("spec hash annotation %q -> %q", a, b)
	}
	if d.Spec.Template.Annotations[ConfigHashAnnotation] == "" {
		t.Error("no config hash annotation")
	}
	for name, s := range goldenSpecs {
		d, cm := render(t, s, paramsFor(name))
		for _, refs := range [][]metav1.OwnerReference{d.OwnerReferences, cm.OwnerReferences} {
			if len(refs) != 1 || refs[0].UID != "00000000-0000-0000-0000-000000000001" || refs[0].Name != "pagewire" ||
				refs[0].Kind != "Deployment" || !*refs[0].Controller || !*refs[0].BlockOwnerDeletion {
				t.Errorf("%s ownerReferences %+v", name, refs)
			}
		}
		for _, k := range []string{"ignore-check.kube-linter.io/host-network", "ignore-check.kube-linter.io/run-as-non-root"} {
			if d.Annotations[k] == "" {
				t.Errorf("%s: annotation %s missing or empty", name, k)
			}
		}
		if d.Annotations["reloader.stakater.com/auto"] != "false" || d.Annotations[SpecHashAnnotation] != s.Hash {
			t.Errorf("%s: annotations %v", name, d.Annotations)
		}
		slot := SlotName(paramsFor(name).Slot)
		if d.Name != slot || cm.Name != slot || d.Namespace != "printing" || d.Labels[NameLabel] != slot ||
			d.Labels[ManagedByLabel] != "pagewire" || d.Labels[WorkloadLabel] != "true" ||
			d.Spec.Template.Labels[NameLabel] != slot || d.Spec.Template.Labels[WorkloadLabel] != "true" {
			t.Errorf("%s: metadata %+v", name, d.ObjectMeta)
		}
		// The selector is immutable; the workload label stays out of it.
		if want := map[string]string{NameLabel: slot}; !maps.Equal(d.Spec.Selector.MatchLabels, want) || len(d.Spec.Selector.MatchExpressions) != 0 {
			t.Errorf("%s: selector %+v, want %v", name, d.Spec.Selector, want)
		}
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.ConfigMap != nil && v.ConfigMap.Name != slot {
				t.Errorf("%s: volume %s uses ConfigMap %s", name, v.Name, v.ConfigMap.Name)
			}
		}
	}
}

// TestSlot0Selector pins slot 0's selector to today's, so the live
// Deployment is adopted in place (a selector cannot change).
func TestSlot0Selector(t *testing.T) {
	d, _ := render(t, goldenSpecs["ippusb"], goldenParams())
	if d.Name != "pagewire-device" || !maps.Equal(d.Spec.Selector.MatchLabels, map[string]string{"app.kubernetes.io/name": "pagewire-device"}) {
		t.Errorf("slot 0: name %s selector %+v", d.Name, d.Spec.Selector)
	}
}

func TestSlotNames(t *testing.T) {
	want := []string{"pagewire-device", "pagewire-device-1", "pagewire-device-2", "pagewire-device-3"}
	var got []string
	for i := range MaxSlots {
		got = append(got, SlotName(i))
	}
	if !slices.Equal(got, want) {
		t.Errorf("slot names %v", got)
	}
	for _, slot := range []int{-1, MaxSlots} {
		p := goldenParams()
		p.Slot = slot
		if _, _, err := Render(goldenSpecs["probe"], p); err == nil {
			t.Errorf("slot %d rendered", slot)
		}
	}
}

// TestNodePinning: a node-pinned spec gets node affinity on the Node object's
// name and the node annotation on the Deployment only (not the pod
// template, so the pod hash does not depend on it).
func TestNodePinning(t *testing.T) {
	d, _ := render(t, goldenSpecs["multi"], paramsFor("multi"))
	if got := d.Annotations[NodeAnnotation]; got != "node-b" {
		t.Errorf("deployment node annotation %q", got)
	}
	if _, ok := d.Spec.Template.Annotations[NodeAnnotation]; ok {
		t.Error("node annotation on the pod template")
	}
	pod := d.Spec.Template.Spec
	if len(pod.NodeSelector) != 0 {
		t.Errorf("nodeSelector %v", pod.NodeSelector)
	}
	a := pod.Affinity
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("affinity %+v", a)
	}
	terms := a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	want := []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{{
		Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"node-b"},
	}}}}
	if !reflect.DeepEqual(terms, want) {
		t.Errorf("terms %+v", terms)
	}

	for _, name := range []string{"probe", "ippusb", "simulated"} {
		d, _ := render(t, goldenSpecs[name], goldenParams())
		if d.Spec.Template.Spec.Affinity != nil {
			t.Errorf("%s: affinity without a node", name)
		}
		if _, ok := d.Annotations[NodeAnnotation]; ok {
			t.Errorf("%s: node annotation without a node", name)
		}
	}
}

func TestConfigMapBackends(t *testing.T) {
	cases := map[string]string{
		"hplip-airsane-blob": "hpaio\n",
		"simulated":          "test\n",
		"multi":              "genesys\nxerox_mfp\n",
		"probe":              "hpaio\nepson2\nepsonds\npixma\ngenesys\nplustek\nfujitsu\ncanon_dr\nxerox_mfp\nkodakaio\n",
	}
	for name, want := range cases {
		_, cm := render(t, goldenSpecs[name], paramsFor(name))
		if cm.Data["dll.conf"] != want {
			t.Errorf("%s dll.conf = %q, want %q", name, cm.Data["dll.conf"], want)
		}
		for _, k := range []string{"ipp-usb.conf", "options.conf", "ignore.conf", "access.conf"} {
			if _, ok := cm.Data[k]; !ok {
				t.Errorf("%s: ConfigMap lacks %s", name, k)
			}
		}
		if !strings.Contains(cm.Data["ignore.conf"], "airscan") || !strings.Contains(cm.Data["ignore.conf"], "escl") {
			t.Errorf("%s ignore.conf %q", name, cm.Data["ignore.conf"])
		}
	}
	// A simulated scanner beside a USB scanner adds the test backend.
	spec := withHash(&device.Spec{Mode: device.ModeUSB, Scanners: []device.ScannerSpec{
		{ID: "lide", Family: "airsane", SaneBackends: []string{"genesys"}, USB: keyLiDE},
		{ID: "sim", Family: "sim"},
	}})
	if _, cm := render(t, spec, goldenParams()); cm.Data["dll.conf"] != "genesys\ntest\n" {
		t.Errorf("usb + simulated dll.conf %q", cm.Data["dll.conf"])
	}
}

func TestPlanPodUID(t *testing.T) {
	agentOf := func(p Params) Container {
		t.Helper()
		pod, err := Plan(goldenSpecs["ippusb"], p)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range append(slices.Clone(pod.Inits), pod.Mains...) {
			if c.Name == "agent" {
				return c
			}
		}
		t.Fatal("no agent")
		return Container{}
	}
	p := goldenParams()
	p.PodUID = "x"
	a := agentOf(p)
	if i := slices.Index(a.Args, "--pod-uid"); i < 0 || a.Args[i+1] != "x" || len(a.Env) != 0 {
		t.Errorf("PodUID x: args %v env %v", a.Args, a.Env)
	}
	a = agentOf(goldenParams())
	if i := slices.Index(a.Args, "--pod-uid"); i < 0 || a.Args[i+1] != "$(POD_UID)" ||
		!slices.Equal(a.Env, []EnvVar{{Name: "POD_UID", FieldPath: "metadata.uid"}}) {
		t.Errorf("no PodUID: args %v env %v", a.Args, a.Env)
	}
}

func TestPlanMatchesRender(t *testing.T) {
	planNames := func(cs []Container) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Name)
		}
		return out
	}
	for name, s := range goldenSpecs {
		pod, err := Plan(s, paramsFor(name))
		if err != nil {
			t.Fatal(name, err)
		}
		d, cm := render(t, s, paramsFor(name))
		spec := d.Spec.Template.Spec
		if got, want := planNames(pod.Inits), names(spec.InitContainers); !slices.Equal(got, want) {
			t.Errorf("%s inits: Plan %v, Render %v", name, got, want)
		}
		if got, want := planNames(pod.Mains), names(spec.Containers); !slices.Equal(got, want) {
			t.Errorf("%s mains: Plan %v, Render %v", name, got, want)
		}
		if !maps.Equal(pod.Config, cm.Data) {
			t.Errorf("%s: Plan config differs from the ConfigMap data", name)
		}
	}
}

func TestPlanNoUSBCheck(t *testing.T) {
	p := goldenParams()
	p.USBResource = ""
	if _, err := Plan(goldenSpecs["ippusb"], p); err != nil {
		t.Errorf("Plan: %v", err)
	}
	if _, _, err := Render(goldenSpecs["ippusb"], p); !errors.Is(err, ErrNoUSBResource) {
		t.Errorf("Render: %v, want ErrNoUSBResource", err)
	}
}
