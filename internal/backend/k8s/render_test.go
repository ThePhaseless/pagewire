package k8s

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ThePhaseless/printscan/internal/device"
)

const selfImage = "ghcr.io/thephaseless/printscan:0.0.0-test"

func goldenParams() Params {
	return Params{
		Namespace:   "printing",
		Owner:       Owner{Name: "printscan", UID: "00000000-0000-0000-0000-000000000001"},
		Registry:    "ghcr.io/thephaseless",
		Tag:         "0.0.0-test",
		AgentURL:    "http://printscan.printing.svc:8081",
		USBResource: "devic.es/printer",
		BlobPVC:     "printscan-blobs",
		SelfImage:   selfImage,
	}
}

func withHash(s *device.Spec) *device.Spec {
	s.Hash = device.SpecHash(s)
	return s
}

var goldenSpecs = map[string]*device.Spec{
	"probe":  withHash(&device.Spec{Mode: device.ModeProbe}),
	"ippusb": withHash(&device.Spec{Mode: device.ModeUSB, Printer: "ipp-usb", Scanner: "ipp-usb"}),
	"hplip-airsane-blob": withHash(&device.Spec{
		Mode: device.ModeUSB, Printer: "hplip", Scanner: "airsane", SaneBackends: []string{"hpaio"},
		Blobs: []device.BlobRef{{ID: "hplip-plugin", Version: "3.24.4", Optional: true}},
	}),
	"simulated": withHash(&device.Spec{
		Mode: device.ModeSimulated, Printer: "sim", Scanner: "sim",
		Name: "Living room printer", Location: "Living room",
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
		d, cm, err := Render(s, goldenParams())
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
		"hplip-airsane-blob": {[]string{"probe", "dbus", "avahi", "agent", "fetch-blobs"}, []string{"printer-app", "airsane"}},
		"simulated":          {[]string{"probe", "dbus", "avahi", "agent"}, []string{"sim-printer", "airsane"}},
	}
	for name, want := range cases {
		d, _ := render(t, goldenSpecs[name], goldenParams())
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

func TestSecurityAndUSB(t *testing.T) {
	usbContainers := map[string][]string{
		"probe":              {"probe"},
		"ippusb":             {"probe", "ipp-usb"},
		"hplip-airsane-blob": {"probe", "printer-app", "airsane"},
		"simulated":          nil,
	}
	res := corev1.ResourceName("devic.es/printer")
	for name, wantUSB := range usbContainers {
		d, _ := render(t, goldenSpecs[name], goldenParams())
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
			if _, ok := c.Resources.Limits[res]; ok {
				gotUSB = append(gotUSB, c.Name)
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
		"--control-plane-url", "http://printscan.printing.svc:8081",
		"--token-file", "/var/run/printscan/token",
		"--pod-uid", "$(POD_UID)",
		"--dbus-address", "unix:path=/run/dbus/system_bus_socket",
		"--inventory", "/run/printscan/inventory.json",
		"--blob-status", "/run/printscan/blobs.json",
	}
	for name, s := range goldenSpecs {
		d, _ := render(t, s, goldenParams())
		a := find(t, d, "agent")
		want := append([]string{"agent", "run"}, fixed...)
		if name == "hplip-airsane-blob" {
			want = append(want, "--pappl-url", "ipp://127.0.0.1:8002/ipp/system")
		}
		if !slices.Equal(a.Command, []string{"/printscan"}) || !slices.Equal(a.Args, want) {
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
	sim := find(t, d, "sim-printer")
	if n := len(sim.Args); n < 3 || !slices.Equal(sim.Args[n-3:], []string{"-l", "Living room", "Living room printer"}) {
		t.Errorf("sim-printer args %v", sim.Args)
	}

	d, _ = render(t, goldenSpecs["hplip-airsane-blob"], goldenParams())
	fb := find(t, d, "fetch-blobs")
	wantFB := []string{
		"agent", "fetch", "--dir", "/blobs", "--status-out", "/run/printscan/blobs.json",
		"--blob", "hplip-plugin@3.24.4", "--accept", "hplip-plugin@3.24.4", "--optional", "hplip-plugin",
	}
	if !slices.Equal(fb.Args, wantFB) {
		t.Errorf("fetch-blobs args %v", fb.Args)
	}
	if !slices.ContainsFunc(fb.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == "/run/printscan" && !m.ReadOnly }) {
		t.Errorf("fetch-blobs has no writable /run/printscan: %+v", fb.VolumeMounts)
	}

	required := *goldenSpecs["hplip-airsane-blob"]
	required.Blobs = []device.BlobRef{{ID: "hplip-plugin", Version: "3.24.4"}}
	d, _ = render(t, &required, goldenParams())
	if slices.Contains(find(t, d, "fetch-blobs").Args, "--optional") {
		t.Error("required blob rendered with --optional")
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
	p.SelfImage = "ghcr.io/thephaseless/printscan:main"
	if self, _, _ = policies(p); !one(self, corev1.PullAlways) {
		t.Errorf("self main: %v", self)
	}
	p.SelfImage = "ghcr.io/thephaseless/printscan:main" + digest
	self, _, images := policies(p)
	if !one(self, corev1.PullIfNotPresent) || images["agent"] != p.SelfImage {
		t.Errorf("self digest: %v %s", self, images["agent"])
	}
}

func TestMetadata(t *testing.T) {
	d, _ := render(t, goldenSpecs["ippusb"], goldenParams())
	changed := *goldenSpecs["ippusb"]
	changed.Scanner = ""
	d2, _ := render(t, withHash(&changed), goldenParams())
	a, b := d.Spec.Template.Annotations[SpecHashAnnotation], d2.Spec.Template.Annotations[SpecHashAnnotation]
	if a == "" || a == b {
		t.Errorf("spec hash annotation %q -> %q", a, b)
	}
	if d.Spec.Template.Annotations[ConfigHashAnnotation] == "" {
		t.Error("no config hash annotation")
	}
	for name, s := range goldenSpecs {
		d, cm := render(t, s, goldenParams())
		for _, refs := range [][]metav1.OwnerReference{d.OwnerReferences, cm.OwnerReferences} {
			if len(refs) != 1 || refs[0].UID != "00000000-0000-0000-0000-000000000001" || refs[0].Name != "printscan" ||
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
		if d.Name != DeviceName || d.Namespace != "printing" || d.Labels[NameLabel] != DeviceName || d.Labels[ManagedByLabel] != "printscan" {
			t.Errorf("%s: metadata %+v", name, d.ObjectMeta)
		}
	}
}

func TestConfigMapBackends(t *testing.T) {
	cases := map[string]string{
		"hplip-airsane-blob": "hpaio\n",
		"simulated":          "test\n",
		"probe":              "hpaio\nepson2\nepsonds\npixma\ngenesys\nplustek\nfujitsu\ncanon_dr\nxerox_mfp\nkodakaio\n",
	}
	for name, want := range cases {
		_, cm := render(t, goldenSpecs[name], goldenParams())
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
}
