package docker

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"github.com/ThePhaseless/pagewire/internal/backend/k8s"
	"github.com/ThePhaseless/pagewire/internal/device"
)

const goldenSelf = "sha256:self"

// goldenParams matches render_test.go's goldenParams, with an image ID as the
// self image and the docker pod UID rule.
func goldenParams(s *device.Spec) k8s.Params {
	return k8s.Params{
		Registry:  "ghcr.io/thephaseless",
		Tag:       "0.0.0-test",
		AgentURL:  "http://pagewire.printing.svc:8081",
		BlobPVC:   BlobVolume,
		SelfImage: goldenSelf,
		PodUID:    "docker-" + s.Hash[:12],
	}
}

func withHash(s *device.Spec) *device.Spec {
	s.Hash = device.SpecHash(s)
	return s
}

const goldenTextSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func goldenSpecs() map[string]*device.Spec {
	return map[string]*device.Spec{
		"probe":  withHash(&device.Spec{Mode: device.ModeProbe}),
		"ippusb": withHash(&device.Spec{Mode: device.ModeUSB, Printer: "ipp-usb", Scanner: "ipp-usb"}),
		"hplip-airsane-blob": withHash(&device.Spec{
			Mode: device.ModeUSB, Printer: "hplip", Scanner: "airsane", SaneBackends: []string{"hpaio"},
			Blobs:         []device.BlobRef{{ID: "hplip-plugin", Version: "3.24.4", Optional: true}},
			AcceptedEULAs: []device.EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: goldenTextSHA}},
		}),
		"simulated": withHash(&device.Spec{
			Mode: device.ModeSimulated, Printer: "sim", Scanner: "sim",
			Name: "Living room printer", Location: "Living room",
		}),
	}
}

func planned(t *testing.T, s *device.Spec) ([]plannedContainer, k8s.Pod) {
	t.Helper()
	pod, err := k8s.Plan(s, goldenParams(s))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := translate(pod)
	if err != nil {
		t.Fatal(err)
	}
	return cs, pod
}

func allPlanned(t *testing.T) map[string][]plannedContainer {
	t.Helper()
	out := map[string][]plannedContainer{}
	for name, s := range goldenSpecs() {
		out[name], _ = planned(t, s)
	}
	return out
}

func TestGolden(t *testing.T) {
	type entry struct {
		Name       string                `json:"name"`
		RunOnce    bool                  `json:"runOnce"`
		Config     *container.Config     `json:"config"`
		HostConfig *container.HostConfig `json:"hostConfig"`
	}
	for name, cs := range allPlanned(t) {
		var out []entry
		for _, c := range cs {
			if _, ok := c.Opts.Config.Labels[LabelWorkloadHash]; ok {
				t.Errorf("%s/%s: translate set the workload-hash label", name, c.Name)
			}
			out = append(out, entry{c.Name, c.RunOnce, c.Opts.Config, c.Opts.HostConfig})
		}
		got, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, '\n')
		path := filepath.Join("testdata", "device-"+name+".golden.json")
		if os.Getenv("UPDATE_GOLDEN") == "1" {
			if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // test fixture
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path) //nolint:gosec // test fixture path
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the translation; rerun with UPDATE_GOLDEN=1 and review the diff\n%s", path, got)
		}
	}
}

func TestEveryVolumeMapped(t *testing.T) {
	for _, v := range k8s.VolumeOrder {
		n := 0
		if namedVolumes[v] != "" {
			n++
		}
		if _, ok := scratchVolumes[v]; ok {
			n++
		}
		if tmpfsVolumes[v] {
			n++
		}
		if n != 1 {
			t.Errorf("volume %q is mapped %d times, want once", v, n)
		}
	}
	_, err := translateOne(k8s.Container{Name: "x", Res: [4]string{"1", "1", "1", "1"}, Mounts: []k8s.Mount{{Vol: "bogus", Path: "/x"}}}, false)
	if err == nil || !strings.Contains(err.Error(), `"bogus"`) {
		t.Errorf("unknown volume: err = %v", err)
	}
}

func byName(cs []plannedContainer) map[string]plannedContainer {
	m := map[string]plannedContainer{}
	for _, c := range cs {
		m[c.Name] = c
	}
	return m
}

func TestUIDRule(t *testing.T) {
	// Writers of run-pagewire or blobs run as root (named volumes have no
	// fsGroup); in usb and probe modes the agent stages EULA texts on blobs.
	want := map[string]map[string]string{
		"simulated":          {"probe": "0:0", "agent": "65532:65532", "dbus": "101:101", "avahi": "102:102", "sim-printer": "65532:65532", "airsane": "65532:65532"},
		"hplip-airsane-blob": {"probe": "0:0", "agent": "0:0", "fetch-blobs": "0:0", "dbus": "101:101", "printer-app": "0:0", "airsane": "0:0"},
		"probe":              {"probe": "0:0", "agent": "0:0"},
	}
	all := allPlanned(t)
	for spec, users := range want {
		m := byName(all[spec])
		for name, u := range users {
			if got := m[name].Opts.Config.User; got != u {
				t.Errorf("%s/%s: user %q, want %q", spec, name, got, u)
			}
		}
	}
}

func TestUSBContainers(t *testing.T) {
	for spec, s := range goldenSpecs() {
		cs, pod := planned(t, s)
		usb := map[string]bool{}
		for _, c := range append(slices.Clone(pod.Inits), pod.Mains...) {
			usb[c.Name] = c.USB
		}
		for _, c := range cs {
			hc := c.Opts.HostConfig
			hasBind := slices.ContainsFunc(hc.Mounts, func(m mount.Mount) bool {
				return m.Type == mount.TypeBind && m.Source == "/dev/bus/usb" && m.Target == "/dev/bus/usb" && !m.ReadOnly
			})
			hasRule := slices.Equal(hc.DeviceCgroupRules, []string{"c 189:* rwm"})
			if hasBind != usb[c.Name] || hasRule != usb[c.Name] {
				t.Errorf("%s/%s: usb %v, bind %v, cgroup rule %v", spec, c.Name, usb[c.Name], hasBind, hasRule)
			}
			if len(hc.Devices) > 0 || hc.Privileged {
				t.Errorf("%s/%s: static devices or privileged", spec, c.Name)
			}
		}
	}
}

func TestHardening(t *testing.T) {
	for spec, cs := range allPlanned(t) {
		for _, c := range cs {
			cfg, hc := c.Opts.Config, c.Opts.HostConfig
			id := spec + "/" + c.Name
			if c.Opts.Name != "pagewire-device-"+c.Name {
				t.Errorf("%s: name %q", id, c.Opts.Name)
			}
			if !hc.ReadonlyRootfs || hc.Privileged || !slices.Equal(hc.CapDrop, []string{"ALL"}) || len(hc.CapAdd) > 0 ||
				!slices.Equal(hc.SecurityOpt, []string{"no-new-privileges:true"}) ||
				hc.NetworkMode != "host" || hc.UsernsMode != "host" {
				t.Errorf("%s: host config not hardened: %+v", id, hc)
			}
			if cfg.StopTimeout == nil || *cfg.StopTimeout != 10 {
				t.Errorf("%s: stop timeout %v", id, cfg.StopTimeout)
			}
			if hc.Memory <= 0 || hc.NanoCPUs <= 0 {
				t.Errorf("%s: no limits: %+v", id, hc.Resources)
			}
			want := []string{"NONE"}
			if c.Name == "avahi" {
				want = []string{"CMD", "/usr/sbin/avahi-daemon", "-c"}
				h := cfg.Healthcheck
				if h.Interval != 5*time.Second || h.Timeout != 3*time.Second || h.Retries != 3 || h.StartPeriod != 30*time.Second {
					t.Errorf("%s: healthcheck timing %+v", id, h)
				}
			}
			if !slices.Equal(cfg.Healthcheck.Test, want) {
				t.Errorf("%s: healthcheck %v, want %v", id, cfg.Healthcheck.Test, want)
			}
			if cfg.Labels[LabelManagedBy] != ManagedBy || cfg.Labels[LabelContainer] != c.Name {
				t.Errorf("%s: labels %v", id, cfg.Labels)
			}
		}
	}
	// Limits come from the plan's limits: the agent is 200m and 128Mi.
	agent := byName(allPlanned(t)["simulated"])["agent"].Opts.HostConfig.Resources
	if agent.NanoCPUs != 200_000_000 || agent.Memory != 128<<20 {
		t.Errorf("agent limits %d cpu, %d memory", agent.NanoCPUs, agent.Memory)
	}
}

func TestDiskBackedScratch(t *testing.T) {
	cs, _ := planned(t, goldenSpecs()["hplip-airsane-blob"])
	app := byName(cs)["printer-app"].Opts.HostConfig
	anon := map[string]*mount.VolumeOptions{}
	for _, m := range app.Mounts {
		if m.Type == mount.TypeVolume && m.Source == "" {
			anon[m.Target] = m.VolumeOptions
		}
	}
	want := map[string]bool{"/tmp": false, "/var/spool/hplip-printer-app": true, "/var/lib/hplip-printer-app": true}
	if len(anon) != len(want) {
		t.Fatalf("anonymous volumes %v, want targets %v", anon, want)
	}
	for path, noCopy := range want {
		o := anon[path]
		if o == nil || o.NoCopy != noCopy || o.Labels[LabelManagedBy] != ManagedBy {
			t.Errorf("%s: volume options %+v, want NoCopy %v and the owner label", path, o, noCopy)
		}
		if _, ok := app.Tmpfs[path]; ok {
			t.Errorf("%s is a tmpfs", path)
		}
	}
	for _, path := range []string{"/run", "/etc/cups/ssl", "/var/cache/fontconfig"} {
		if app.Tmpfs[path] != "mode=1777" {
			t.Errorf("%s: tmpfs %q", path, app.Tmpfs[path])
		}
	}
	ipp := byName(allPlanned(t)["ippusb"])["ipp-usb"].Opts.HostConfig
	for _, path := range []string{"/var/ipp-usb", "/var/log/ipp-usb"} {
		if ipp.Tmpfs[path] != "mode=1777" {
			t.Errorf("ipp-usb %s: tmpfs %q", path, ipp.Tmpfs[path])
		}
	}
}

func TestNamedVolumesNoCopy(t *testing.T) {
	check := func(id string, ms []mount.Mount) {
		for _, m := range ms {
			if m.Type != mount.TypeVolume {
				continue
			}
			noCopy := m.VolumeOptions != nil && m.VolumeOptions.NoCopy
			if (m.Source != "" || m.Target != "/tmp") && !noCopy {
				t.Errorf("%s: volume mount %s -> %s without NoCopy", id, m.Source, m.Target)
			}
			if m.Source == "" && m.Target == "/tmp" && noCopy {
				t.Errorf("%s: /tmp must keep copy-up", id)
			}
		}
	}
	for spec, cs := range allPlanned(t) {
		for _, c := range cs {
			check(spec+"/"+c.Name, c.Opts.HostConfig.Mounts)
		}
	}
	check("seed", seedContainer(goldenSelf).HostConfig.Mounts)
}

func TestTokenOnlyInAgent(t *testing.T) {
	for spec, cs := range allPlanned(t) {
		for _, c := range cs {
			for _, m := range c.Opts.HostConfig.Mounts {
				if m.Source == TokenVolume && (c.Name != "agent" || !m.ReadOnly) {
					t.Errorf("%s/%s mounts the token volume (read-only %v)", spec, c.Name, m.ReadOnly)
				}
			}
		}
	}
}

func TestWorkloadHash(t *testing.T) {
	s := goldenSpecs()["hplip-airsane-blob"]
	hash := func(mut func(*k8s.Params), specHash string, cfg func(map[string]string)) string {
		t.Helper()
		p := goldenParams(s)
		mut(&p)
		pod, err := k8s.Plan(s, p)
		if err != nil {
			t.Fatal(err)
		}
		cfg(pod.Config)
		cs, err := translate(pod)
		if err != nil {
			t.Fatal(err)
		}
		h, err := workloadHash(specHash, cs, pod.Config)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	same := func(*k8s.Params) {}
	keep := func(map[string]string) {}
	base := hash(same, s.Hash, keep)
	if again := hash(same, s.Hash, keep); again != base {
		t.Fatal("hash is not stable")
	}
	changed := map[string]string{
		"self image":   hash(func(p *k8s.Params) { p.SelfImage = "sha256:other" }, s.Hash, keep),
		"device image": hash(func(p *k8s.Params) { p.Overrides = map[string]string{"device": "x/device:1"} }, s.Hash, keep),
		"driver tag":   hash(func(p *k8s.Params) { p.Tag = "9.9.9" }, s.Hash, keep),
		"agent url":    hash(func(p *k8s.Params) { p.AgentURL = "http://127.0.0.1:9999" }, s.Hash, keep),
		"config":       hash(same, s.Hash, func(c map[string]string) { c["dll.conf"] = "x\n" }),
		"spec hash":    hash(same, "other", keep),
	}
	for what, h := range changed {
		if h == base {
			t.Errorf("changing the %s keeps the workload hash", what)
		}
	}

	cs, _ := planned(t, s)
	stampHash(cs, "abc")
	for _, c := range cs {
		if c.Opts.Config.Labels[LabelWorkloadHash] != "abc" {
			t.Errorf("%s: not stamped", c.Name)
		}
	}
}

func TestRestartPolicies(t *testing.T) {
	cs, _ := planned(t, goldenSpecs()["hplip-airsane-blob"])
	want := map[string]container.RestartPolicyMode{
		"probe": "no", "fetch-blobs": "no",
		"dbus": "on-failure", "avahi": "on-failure", "agent": "on-failure",
		"printer-app": "on-failure", "airsane": "on-failure",
	}
	for _, c := range cs {
		if got := c.Opts.HostConfig.RestartPolicy.Name; got != want[c.Name] {
			t.Errorf("%s: restart %q, want %q", c.Name, got, want[c.Name])
		}
		if c.RunOnce != (want[c.Name] == "no") || c.Opts.Config.Labels[LabelInit] != map[bool]string{true: "true", false: "false"}[c.RunOnce] {
			t.Errorf("%s: runOnce %v, init label %q", c.Name, c.RunOnce, c.Opts.Config.Labels[LabelInit])
		}
	}
	// In probe mode the agent is a main: it keeps running.
	ps, _ := planned(t, goldenSpecs()["probe"])
	if a := byName(ps)["agent"]; a.RunOnce || a.Opts.HostConfig.RestartPolicy.Name != "on-failure" {
		t.Errorf("probe agent: %+v", a.Opts.HostConfig.RestartPolicy)
	}
}

func TestFieldPathEnvRejected(t *testing.T) {
	s := goldenSpecs()["simulated"]
	p := goldenParams(s)
	p.PodUID = ""
	pod, err := k8s.Plan(s, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := translate(pod); err == nil || !strings.Contains(err.Error(), "POD_UID") {
		t.Errorf("err = %v, want a downward-API error naming POD_UID", err)
	}
}

func TestTarFiles(t *testing.T) {
	buf, err := tarFiles(map[string]string{"b.conf": "bee\n", "a.conf": "a\n"})
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(buf)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		names = append(names, h.Name+"="+string(body))
		if h.Typeflag != tar.TypeReg || h.Mode != 0o444 || h.Uid != 0 || h.Gid != 0 || !h.ModTime.Equal(time.Unix(0, 0)) {
			t.Errorf("%s: header %+v", h.Name, h)
		}
	}
	if want := []string{"a.conf=a\n", "b.conf=bee\n"}; !slices.Equal(names, want) {
		t.Errorf("entries %q, want %q", names, want)
	}
}
