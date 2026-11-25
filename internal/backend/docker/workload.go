package docker

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/ThePhaseless/printscan/internal/backend/k8s"
)

// Labels and names of the objects the backend owns.
const (
	LabelManagedBy    = "dev.printscan.managed-by"
	ManagedBy         = "printscan"
	LabelWorkloadHash = "dev.printscan.workload-hash"
	LabelContainer    = "dev.printscan.container"
	LabelInit         = "dev.printscan.init" // "true" only for run-to-completion inits

	Prefix        = "printscan-device-" // + the workload container's name
	SeedContainer = Prefix + "seed"

	RunVolume    = "printscan-device-run"
	DBusVolume   = "printscan-device-dbus"
	TokenVolume  = "printscan-device-token"
	ConfigVolume = "printscan-device-config"
	// BlobVolume holds vendor blobs and staged EULA texts. It is never removed
	// and may be a volume the user created.
	BlobVolume = "printscan-blobs"

	// DefaultContainer is the control plane's own container name, whose image
	// the probe, agent, fetch-blobs and seed containers run.
	DefaultContainer = "printscan"

	usbCgroupRule = "c 189:* rwm" // USB character devices, including nodes that appear later
	usbDevDir     = "/dev/bus/usb"
	stopTimeout   = 10 // seconds, as terminationGracePeriodSeconds
	seedConfigDir = "/seed/config"
	seedTokenDir  = "/seed/token"
)

// plannedContainer is one device container ready for ContainerCreate. RunOnce
// marks an init that runs to completion.
type plannedContainer struct {
	Name    string
	Opts    client.ContainerCreateOptions
	RunOnce bool
}

// namedVolumes maps k8s volume names to the Docker named volume behind them.
// The three config volumes share one volume seeded with every config file.
var namedVolumes = map[string]string{
	"run-printscan": RunVolume,
	"run-dbus":      DBusVolume,
	"agent-token":   TokenVolume,
	"blobs":         BlobVolume,
	"ipp-usb-conf":  ConfigVolume,
	"airsane-conf":  ConfigVolume,
	"sane-conf":     ConfigVolume,
}

// scratchVolumes are per-container anonymous volumes on disk: tmpfs pages
// count against the memory limit, and a spooled print job can be large. The
// value is NoCopy: tmp keeps the image's /tmp (mode 1777) through copy-up.
var scratchVolumes = map[string]bool{"tmp": false, "app-spool": true, "app-state": true}

// tmpfsVolumes are small per-container scratch: sockets, PID files, a TLS
// cert, ipp-usb's self-rotating log and the font cache.
var tmpfsVolumes = map[string]bool{
	"run-avahi": true, "ipp-usb-state": true, "ipp-usb-log": true,
	"app-run": true, "app-ssl": true, "app-fontconfig": true,
}

// writersAsRoot are the volumes whose writers run as uid 0: a new named
// volume's root is root:root 0755 and Docker has no fsGroup.
var writersAsRoot = map[string]bool{"run-printscan": true, "blobs": true}

func ownerLabels() map[string]string { return map[string]string{LabelManagedBy: ManagedBy} }

// namedMount mounts a named volume. NoCopy keeps image content out of a
// fresh volume, as k8s ConfigMap and emptyDir mounts hide it.
func namedMount(name, path string, ro bool) mount.Mount {
	return mount.Mount{
		Type: mount.TypeVolume, Source: name, Target: path, ReadOnly: ro,
		VolumeOptions: &mount.VolumeOptions{NoCopy: true},
	}
}

// translate turns the plan into containers in start order (inits, then mains),
// without the workload-hash label.
func translate(pod k8s.Pod) ([]plannedContainer, error) {
	var out []plannedContainer
	for i, c := range append(slices.Clone(pod.Inits), pod.Mains...) {
		runOnce := i < len(pod.Inits) && !c.Sidecar
		pc, err := translateOne(c, runOnce)
		if err != nil {
			return nil, err
		}
		out = append(out, pc)
	}
	return out, nil
}

func translateOne(c k8s.Container, runOnce bool) (plannedContainer, error) {
	uid := c.UID
	env := make([]string, 0, len(c.Env))
	for _, e := range c.Env {
		if e.FieldPath != "" {
			return plannedContainer{}, fmt.Errorf("docker: %s: env %s uses the downward API", c.Name, e.Name)
		}
		env = append(env, e.Name+"="+e.Value)
	}
	cpu, err := resource.ParseQuantity(c.Res[2])
	if err != nil {
		return plannedContainer{}, fmt.Errorf("docker: %s: cpu limit: %w", c.Name, err)
	}
	mem, err := resource.ParseQuantity(c.Res[3])
	if err != nil {
		return plannedContainer{}, fmt.Errorf("docker: %s: memory limit: %w", c.Name, err)
	}

	hc := &container.HostConfig{
		NetworkMode:    "host",
		UsernsMode:     "host",
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyOnFailure},
		Resources:      container.Resources{NanoCPUs: cpu.MilliValue() * 1e6, Memory: mem.Value()},
	}
	if runOnce {
		hc.RestartPolicy.Name = container.RestartPolicyDisabled
	}
	for _, m := range c.Mounts {
		switch {
		case namedVolumes[m.Vol] != "":
			hc.Mounts = append(hc.Mounts, namedMount(namedVolumes[m.Vol], m.Path, m.RO))
			if !m.RO && writersAsRoot[m.Vol] {
				uid = 0
			}
		case tmpfsVolumes[m.Vol]:
			if hc.Tmpfs == nil {
				hc.Tmpfs = map[string]string{}
			}
			hc.Tmpfs[m.Path] = "mode=1777"
		default:
			noCopy, ok := scratchVolumes[m.Vol]
			if !ok {
				return plannedContainer{}, fmt.Errorf("docker: %s: volume %q has no Docker mapping", c.Name, m.Vol)
			}
			hc.Mounts = append(hc.Mounts, mount.Mount{
				Type: mount.TypeVolume, Target: m.Path, ReadOnly: m.RO,
				VolumeOptions: &mount.VolumeOptions{NoCopy: noCopy, Labels: ownerLabels()},
			})
		}
	}
	if c.USB {
		// A bind and a cgroup rule, not static Devices: a re-plugged printer
		// gets a new node, which both still cover.
		hc.Mounts = append(hc.Mounts, mount.Mount{Type: mount.TypeBind, Source: usbDevDir, Target: usbDevDir})
		hc.DeviceCgroupRules = []string{usbCgroupRule}
	}

	health := &container.HealthConfig{Test: []string{"NONE"}}
	if c.Name == "avahi" {
		// AirSane never retries a failed Avahi registration, so the rollout
		// waits for this before starting the next container.
		health = &container.HealthConfig{
			Test:     []string{"CMD", "/usr/sbin/avahi-daemon", "-c"},
			Interval: 5 * time.Second, Timeout: 3 * time.Second, Retries: 3, StartPeriod: 30 * time.Second,
		}
	}
	timeout := stopTimeout
	labels := ownerLabels()
	labels[LabelContainer] = c.Name
	labels[LabelInit] = strconv.FormatBool(runOnce)
	return plannedContainer{
		Name:    c.Name,
		RunOnce: runOnce,
		Opts: client.ContainerCreateOptions{
			Name: Prefix + c.Name,
			Config: &container.Config{
				Image:       c.Image,
				Entrypoint:  c.Command,
				Cmd:         c.Args,
				Env:         env,
				User:        fmt.Sprintf("%d:%d", uid, uid),
				Labels:      labels,
				Healthcheck: health,
				StopTimeout: &timeout,
			},
			HostConfig: hc,
		},
	}, nil
}

// workloadHash identifies a rollout: the spec hash, the planned containers
// (images, agent URL and the rest) and the config files. encoding/json sorts
// map keys, so equal inputs give equal hashes across restarts.
func workloadHash(specHash string, cs []plannedContainer, config map[string]string) (string, error) {
	cj, err := json.Marshal(cs)
	if err != nil {
		return "", err
	}
	fj, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\n%s\n%s", specHash, cj, fj) // hash writes never fail
	return hex.EncodeToString(h.Sum(nil)), nil
}

// stampHash sets the workload-hash label on every container.
func stampHash(cs []plannedContainer, h string) {
	for _, c := range cs {
		c.Opts.Config.Labels[LabelWorkloadHash] = h
	}
}

type volumeSpec struct {
	Name       string
	DriverOpts map[string]string
}

// rolloutVolumes are the named volumes created fresh for every rollout.
func rolloutVolumes() []volumeSpec {
	return []volumeSpec{
		{Name: RunVolume},
		{Name: DBusVolume, DriverOpts: map[string]string{"type": "tmpfs", "device": "tmpfs", "o": "mode=1777"}},
		{Name: ConfigVolume},
		{Name: TokenVolume},
	}
}

// seedContainer is a never-started container that the config files and the
// token are copied into, standing in for the ConfigMap and Secret.
func seedContainer(selfImage string) client.ContainerCreateOptions {
	return client.ContainerCreateOptions{
		Name: SeedContainer,
		Config: &container.Config{
			Image:       selfImage,
			Labels:      ownerLabels(),
			Healthcheck: &container.HealthConfig{Test: []string{"NONE"}},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    "none",
			UsernsMode:     "host",
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges:true"},
			Mounts: []mount.Mount{
				namedMount(ConfigVolume, seedConfigDir, false),
				namedMount(TokenVolume, seedTokenDir, false),
			},
		},
	}
}

// tarFiles archives files as root-owned 0444 regular files in name order, with
// a fixed ModTime.
func tarFiles(files map[string]string) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg, Name: n, Mode: 0o444, Size: int64(len(files[n])),
			Uid: 0, Gid: 0, ModTime: time.Unix(0, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(files[n])); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}
