package k8s

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

	"github.com/ThePhaseless/printscan/internal/catalog"
	"github.com/ThePhaseless/printscan/internal/device"
)

// Names and labels of the rendered objects.
const (
	DeviceName      = "printscan-device" // Deployment and ConfigMap
	TokenSecretName = "printscan-agent-token"
	TokenSecretKey  = "token"
	NameLabel       = "app.kubernetes.io/name"
	ManagedByLabel  = "app.kubernetes.io/managed-by"
	FieldManager    = "printscan"

	SpecHashAnnotation   = "printscan.dev/spec-hash"
	ConfigHashAnnotation = "printscan.dev/config-hash"
)

// ErrNoUSBResource is returned for usb and probe specs when Params.USBResource is empty.
var ErrNoUSBResource = errors.New("needs a USB resource but Params.USBResource is empty")

// Owner is the control plane's own Deployment, which owns everything rendered.
type Owner struct {
	Name string
	UID  types.UID
}

// Params are the renderer's inputs besides the Spec.
type Params struct {
	Namespace   string
	Owner       Owner
	Registry    string
	Tag         string
	Overrides   map[string]string
	AgentURL    string
	USBResource string // extended resource name, e.g. devic.es/printer; "" means no USB
	BlobPVC     string
	SelfImage   string // image of the owner's SelfContainer: probe, agent and fetch-blobs
	PodUID      string // agent --pod-uid; "" renders $(POD_UID) from the downward API
}

// Fixed uids. messagebus and avahi match the LABELs of images/device/Dockerfile.
const (
	uidRoot       = 0
	uidMessagebus = 101
	uidAvahi      = 102
	uidNonroot    = 65532
)

// Reasons for the kube-linter ignore annotations.
const (
	hostNetworkReason = "the device pod advertises the printer and scanner over mDNS and serves them on the node's LAN address"
	rootReason        = "USB containers run as uid 0 with every capability dropped because device-plugin nodes are root:root 0664"
)

// VolumeOrder lists every volume the pod can mount, in pod order; Render
// includes only those a container uses.
var VolumeOrder = []string{
	"run-printscan", "run-dbus", "run-avahi", "tmp", "agent-token", "blobs",
	"ipp-usb-conf", "ipp-usb-state", "ipp-usb-log", "app-state", "app-spool", "app-run", "app-ssl",
	"app-fontconfig", "airsane-conf", "sane-conf",
}

// Pod is the backend-neutral device workload: Render turns it into a
// Deployment and ConfigMap, the docker backend into containers.
type Pod struct {
	Inits  []Container       // start order; Sidecar ones keep running
	Mains  []Container       // started after the inits
	Config map[string]string // ConfigMap data: ipp-usb.conf, dll.conf, options.conf, ignore.conf, access.conf
}

// Container is one container of the device workload.
type Container struct {
	Name, Image   string
	Command, Args []string
	Env           []EnvVar
	UID           int64
	USB, Sidecar  bool
	Mounts        []Mount
	Res           [4]string // cpu request, memory request, cpu limit, memory limit
}

// EnvVar is a container environment variable. FieldPath is for the Kubernetes
// downward API only; Value is ignored when it is set.
type EnvVar struct{ Name, Value, FieldPath string }

// Mount mounts the volume Vol (a VolumeOrder name) at Path.
type Mount struct {
	Vol, Path string
	RO        bool
}

// Render builds the device Deployment and ConfigMap for spec. It is pure: it
// reads only spec, p and the embedded catalog.
func Render(spec *device.Spec, p Params) (*appsv1ac.DeploymentApplyConfiguration, *corev1ac.ConfigMapApplyConfiguration, error) {
	if spec == nil {
		return nil, nil, errors.New("render: nil spec")
	}
	usbMode := spec.Mode == device.ModeUSB || spec.Mode == device.ModeProbe
	if usbMode && p.USBResource == "" {
		return nil, nil, fmt.Errorf("spec mode %s %w", spec.Mode, ErrNoUSBResource)
	}
	plan, err := Plan(spec, p)
	if err != nil {
		return nil, nil, err
	}

	cm := configMap(plan.Config, p)
	used := map[string]bool{}
	pod := corev1ac.PodSpec().
		WithHostNetwork(true).
		WithDNSPolicy(corev1.DNSClusterFirstWithHostNet).
		WithAutomountServiceAccountToken(false).
		WithEnableServiceLinks(false).
		WithTerminationGracePeriodSeconds(10).
		WithSecurityContext(corev1ac.PodSecurityContext().
			WithFSGroup(uidNonroot).
			WithSeccompProfile(corev1ac.SeccompProfile().WithType(corev1.SeccompProfileTypeRuntimeDefault)))
	for _, c := range plan.Inits {
		pod.WithInitContainers(c.build(p.USBResource, used))
	}
	for _, c := range plan.Mains {
		pod.WithContainers(c.build(p.USBResource, used))
	}
	for _, name := range VolumeOrder {
		if used[name] {
			pod.WithVolumes(volume(name, p.BlobPVC))
		}
	}

	labels := map[string]string{NameLabel: DeviceName, ManagedByLabel: FieldManager}
	deploy := appsv1ac.Deployment(DeviceName, p.Namespace).
		WithLabels(labels).
		WithAnnotations(map[string]string{
			"reloader.stakater.com/auto":                  "false",
			SpecHashAnnotation:                            spec.Hash,
			"ignore-check.kube-linter.io/host-network":    hostNetworkReason,
			"ignore-check.kube-linter.io/run-as-non-root": rootReason,
		}).
		WithOwnerReferences(ownerRef(p.Owner)).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(1).
			WithStrategy(appsv1ac.DeploymentStrategy().WithType("Recreate")).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{NameLabel: DeviceName})).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(labels).
				WithAnnotations(map[string]string{
					SpecHashAnnotation:   spec.Hash,
					ConfigHashAnnotation: dataHash(cm.Data),
				}).
				WithSpec(pod)))
	return deploy, cm, nil
}

// Plan builds the backend-neutral device workload for spec. Unlike Render it
// does not check Params.USBResource, which only Kubernetes uses. It is pure:
// it reads only spec, p and the embedded catalog.
func Plan(spec *device.Spec, p Params) (Pod, error) {
	if spec == nil {
		return Pod{}, errors.New("render: nil spec")
	}
	usbMode := spec.Mode == device.ModeUSB || spec.Mode == device.ModeProbe
	// ippeveprinter takes the name as its last argument: one starting with -
	// would be read as an option. Config validation refuses it too.
	if spec.Mode == device.ModeSimulated && strings.HasPrefix(spec.Name, "-") {
		return Pod{}, fmt.Errorf("render: printer name %q must not start with -", spec.Name)
	}
	cat := catalog.Default()
	deviceImage := ImageRef(p.Registry, p.Tag, "printscan-device", p.Overrides)

	probe := Container{
		Name: "probe", Image: p.SelfImage, Command: []string{"/printscan"},
		Args: []string{"agent", "probe", "--out", device.InventoryFile},
		UID:  uidNonroot, USB: usbMode,
		Mounts: []Mount{{"run-printscan", "/run/printscan", false}},
		Res:    [4]string{"10m", "16Mi", "200m", "64Mi"},
	}
	if usbMode {
		probe.UID = uidRoot
	}
	dbus := Container{
		Name: "dbus", Image: deviceImage, Sidecar: true, UID: uidMessagebus,
		Command: []string{"/usr/bin/dbus-daemon"},
		Args:    []string{"--config-file=/etc/printscan/dbus.conf", "--nofork", "--nopidfile"},
		Mounts:  []Mount{{"run-dbus", "/run/dbus", false}},
		Res:     [4]string{"5m", "8Mi", "100m", "32Mi"},
	}
	avahi := Container{
		Name: "avahi", Image: deviceImage, Sidecar: true, UID: uidAvahi,
		Command: []string{"/usr/sbin/avahi-daemon"},
		Args:    []string{"--no-drop-root", "--no-chroot", "--no-rlimits", "-f", "/etc/printscan/avahi-daemon.conf"},
		// avahi-daemon creates /run/avahi-daemon itself and refuses it unless it
		// is owned by avahi:avahi, which an emptyDir mounted there never is; so
		// its own emptyDir is /run, with the shared /run/dbus mounted inside.
		Mounts: []Mount{{"run-avahi", "/run", false}, {"run-dbus", "/run/dbus", false}},
		Res:    [4]string{"5m", "8Mi", "100m", "32Mi"},
	}
	podUID, podUIDEnv := "$(POD_UID)", []EnvVar{{Name: "POD_UID", FieldPath: "metadata.uid"}}
	if p.PodUID != "" {
		podUID, podUIDEnv = p.PodUID, nil
	}
	agent := Container{
		Name: "agent", Image: p.SelfImage, Sidecar: spec.Mode != device.ModeProbe, UID: uidNonroot,
		Command: []string{"/printscan"},
		Args: []string{
			"agent", "run",
			"--control-plane-url", p.AgentURL,
			"--token-file", device.AgentTokenFile,
			"--pod-uid", podUID,
			"--dbus-address", device.DBusAddress,
			"--inventory", device.InventoryFile,
			"--blob-status", device.BlobStatusFile,
		},
		Env: podUIDEnv,
		Mounts: []Mount{
			{"agent-token", "/var/run/printscan", true},
			{"run-printscan", "/run/printscan", true},
			{"run-dbus", "/run/dbus", false},
		},
		Res: [4]string{"10m", "32Mi", "200m", "128Mi"},
	}

	// The agent stages EULA texts on the blob volume, so the wizard and
	// Settings can show a licence before it is accepted.
	if usbMode && p.BlobPVC != "" {
		agent.Args = append(agent.Args, "--blob-dir", "/blobs")
		agent.Mounts = append(agent.Mounts, Mount{"blobs", "/blobs", false})
	}

	var printer catalog.Printer
	if spec.Printer != "" {
		var ok bool
		if printer, ok = cat.Printer(spec.Printer); !ok {
			return Pod{}, fmt.Errorf("render: unknown printer family %q", spec.Printer)
		}
		if printer.App != "" {
			agent.Args = append(agent.Args, "--pappl-url", fmt.Sprintf("ipp://127.0.0.1:%d/ipp/system", printer.Port))
		}
	}

	inits := []Container{probe, dbus, avahi}
	var mains []Container
	switch spec.Mode {
	case device.ModeProbe:
		mains = []Container{agent}
	case device.ModeUSB:
		inits = append(inits, agent)
		if len(spec.Blobs) > 0 {
			inits = append(inits, fetchBlobs(spec, p.SelfImage))
		}
		if spec.Printer == "ipp-usb" || spec.Scanner == "ipp-usb" {
			mains = append(mains, Container{
				Name: "ipp-usb", Image: deviceImage, UID: uidRoot, USB: true,
				Command: []string{"/usr/local/sbin/ipp-usb"}, Args: []string{"standalone"},
				Mounts: []Mount{
					{"ipp-usb-conf", "/etc/ipp-usb", true},
					{"run-dbus", "/run/dbus", false},
					{"ipp-usb-state", "/var/ipp-usb", false},
					{"ipp-usb-log", "/var/log/ipp-usb", false},
				},
				Res: [4]string{"10m", "32Mi", "500m", "128Mi"},
			})
		}
		if printer.App != "" {
			c := Container{
				Name: "printer-app", Image: ImageRef(p.Registry, p.Tag, printer.Image, p.Overrides), UID: uidRoot, USB: true,
				Command: []string{"/usr/local/bin/" + printer.App},
				// log-file=- logs to stderr, so nothing is written under the read-only root.
				Args: []string{"-o", "server-port=" + strconv.Itoa(printer.Port), "-o", "log-file=-", "server"},
				Mounts: []Mount{
					{"app-run", "/run", false}, // PAPPL's control socket /run/<app>.sock
					{"run-dbus", "/run/dbus", false},
					{"app-state", "/var/lib/" + printer.App, false},
					{"app-spool", "/var/spool/" + printer.App, false},
					{"tmp", "/tmp", false},
					{"app-ssl", "/etc/cups/ssl", false},                // PAPPL's self-signed TLS certificate
					{"app-fontconfig", "/var/cache/fontconfig", false}, // the filters' font cache
				},
				Res: [4]string{"50m", "64Mi", "2", "1Gi"},
			}
			if len(spec.Blobs) > 0 {
				c.Mounts = append(c.Mounts, Mount{"blobs", "/blobs", true})
			}
			mains = append(mains, c)
		}
		if spec.Scanner == "airsane" {
			mains = append(mains, airsane(cat, spec, deviceImage, uidRoot, true))
		}
	case device.ModeSimulated:
		inits = append(inits, agent)
		mains = append(mains, Container{
			Name: "sim-printer", Image: deviceImage, UID: uidNonroot,
			Command: []string{"/usr/sbin/ippeveprinter"},
			Args: []string{
				"-p", strconv.Itoa(printer.Port),
				"-f", "application/pdf,application/postscript,image/jpeg,image/pwg-raster,image/urf",
				"-l", spec.Location, spec.Name,
			},
			Mounts: []Mount{{"run-dbus", "/run/dbus", false}, {"tmp", "/tmp", false}},
			Res:    [4]string{"10m", "16Mi", "500m", "128Mi"},
		})
		if spec.Scanner != "" {
			mains = append(mains, airsane(cat, spec, deviceImage, uidNonroot, false))
		}
	default:
		return Pod{}, fmt.Errorf("render: unknown spec mode %q", spec.Mode)
	}

	return Pod{Inits: inits, Mains: mains, Config: configData(cat, spec)}, nil
}

func fetchBlobs(spec *device.Spec, image string) Container {
	args := []string{"agent", "fetch", "--dir", "/blobs", "--status-out", device.BlobStatusFile}
	for _, b := range spec.Blobs {
		args = append(args, "--blob", b.ID+"@"+b.Version)
		if b.Optional {
			args = append(args, "--optional", b.ID)
		}
	}
	for _, e := range spec.AcceptedEULAs {
		args = append(args, "--accept", e.ID+"@"+e.Version+"@"+e.TextSHA256)
	}
	return Container{
		Name: "fetch-blobs", Image: image, UID: uidNonroot,
		Command: []string{"/printscan"}, Args: args,
		Mounts: []Mount{{"blobs", "/blobs", false}, {"run-printscan", "/run/printscan", false}},
		Res:    [4]string{"10m", "32Mi", "500m", "256Mi"},
	}
}

func airsane(cat *catalog.Catalog, spec *device.Spec, image string, uid int64, usb bool) Container {
	port := 0
	for _, s := range cat.Scanners {
		if s.ID == spec.Scanner {
			port = s.Port
		}
	}
	c := Container{
		Name: "airsane", Image: image, UID: uid, USB: usb,
		Command: []string{"/usr/local/bin/airsaned"},
		Args: []string{
			"--listen-port=" + strconv.Itoa(port), "--hotplug=false",
			"--options-file=/etc/printscan/airsane/options.conf",
			"--ignore-list=/etc/printscan/airsane/ignore.conf",
			"--access-file=/etc/printscan/airsane/access.conf",
		},
		Env: []EnvVar{{Name: "SANE_CONFIG_DIR", Value: "/etc/printscan/sane.d:"}},
		Mounts: []Mount{
			{"airsane-conf", "/etc/printscan/airsane", true},
			{"sane-conf", "/etc/printscan/sane.d", true},
			{"run-dbus", "/run/dbus", false},
			{"tmp", "/tmp", false},
		},
		Res: [4]string{"20m", "32Mi", "1", "512Mi"},
	}
	if len(spec.Blobs) > 0 {
		c.Mounts = append(c.Mounts, Mount{"blobs", "/blobs", true})
	}
	return c
}

func (c Container) build(usbResource string, used map[string]bool) *corev1ac.ContainerApplyConfiguration {
	req := corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse(c.Res[0]), corev1.ResourceMemory: resource.MustParse(c.Res[1]),
	}
	lim := corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse(c.Res[2]), corev1.ResourceMemory: resource.MustParse(c.Res[3]),
	}
	if c.USB {
		req[corev1.ResourceName(usbResource)] = resource.MustParse("1")
		lim[corev1.ResourceName(usbResource)] = resource.MustParse("1")
	}
	sc := corev1ac.SecurityContext().
		WithRunAsUser(c.UID).
		WithRunAsGroup(c.UID).
		WithReadOnlyRootFilesystem(true).
		WithAllowPrivilegeEscalation(false).
		WithCapabilities(corev1ac.Capabilities().WithDrop("ALL"))
	if c.UID != uidRoot {
		sc.WithRunAsNonRoot(true)
	}
	out := corev1ac.Container().
		WithName(c.Name).
		WithImage(c.Image).
		WithImagePullPolicy(PullPolicy(c.Image)).
		WithCommand(c.Command...).
		WithArgs(c.Args...).
		WithResources(corev1ac.ResourceRequirements().WithRequests(req).WithLimits(lim)).
		WithSecurityContext(sc)
	for _, e := range c.Env {
		ev := corev1ac.EnvVar().WithName(e.Name)
		if e.FieldPath != "" {
			ev.WithValueFrom(corev1ac.EnvVarSource().WithFieldRef(corev1ac.ObjectFieldSelector().WithFieldPath(e.FieldPath)))
		} else {
			ev.WithValue(e.Value)
		}
		out.WithEnv(ev)
	}
	if c.Sidecar {
		out.WithRestartPolicy(corev1.ContainerRestartPolicyAlways)
	}
	for _, m := range c.Mounts {
		used[m.Vol] = true
		vm := corev1ac.VolumeMount().WithName(m.Vol).WithMountPath(m.Path)
		if m.RO {
			vm.WithReadOnly(true)
		}
		out.WithVolumeMounts(vm)
	}
	return out
}

func volume(name, blobPVC string) *corev1ac.VolumeApplyConfiguration {
	v := corev1ac.Volume().WithName(name)
	items := func(keys ...string) *corev1ac.ConfigMapVolumeSourceApplyConfiguration {
		s := corev1ac.ConfigMapVolumeSource().WithName(DeviceName)
		for _, k := range keys {
			s.WithItems(corev1ac.KeyToPath().WithKey(k).WithPath(k))
		}
		return s
	}
	switch name {
	case "agent-token":
		return v.WithSecret(corev1ac.SecretVolumeSource().WithSecretName(TokenSecretName))
	case "blobs":
		return v.WithPersistentVolumeClaim(corev1ac.PersistentVolumeClaimVolumeSource().WithClaimName(blobPVC))
	case "ipp-usb-conf":
		return v.WithConfigMap(items("ipp-usb.conf"))
	case "airsane-conf":
		return v.WithConfigMap(items("options.conf", "ignore.conf", "access.conf"))
	case "sane-conf":
		return v.WithConfigMap(items("dll.conf"))
	default:
		return v.WithEmptyDir(corev1ac.EmptyDirVolumeSource())
	}
}

var ippUSBConf = `[network]
  http-min-port = ` + strconv.Itoa(device.IPPUSBMinPort) + `
  http-max-port = ` + strconv.Itoa(device.IPPUSBMaxPort) + `
  dns-sd = enable
  interface = all
  ipv6 = enable

[auth uid]
  all = *

[logging]
  device-log = error
  main-log = info
  console-log = info
  console-color = disable
`

func configMap(data map[string]string, p Params) *corev1ac.ConfigMapApplyConfiguration {
	return corev1ac.ConfigMap(DeviceName, p.Namespace).
		WithLabels(map[string]string{NameLabel: DeviceName, ManagedByLabel: FieldManager}).
		WithOwnerReferences(ownerRef(p.Owner)).
		WithData(data)
}

// configData is the device config files, keyed by file name.
func configData(cat *catalog.Catalog, spec *device.Spec) map[string]string {
	backends := spec.SaneBackends
	switch {
	case spec.Mode == device.ModeSimulated:
		backends = []string{"test"}
	case len(backends) == 0:
		backends = cat.DefaultSaneBackends
	}
	return map[string]string{
		"ipp-usb.conf": ippUSBConf,
		"dll.conf":     strings.Join(backends, "\n") + "\n",
		"options.conf": "# AirSane per-device options; none are needed.\n",
		// Loop guard: AirSane must not re-export network scanners.
		"ignore.conf": "escl:.*\nairscan:.*\n",
		// An empty list allows every client: the control plane scans from the
		// pod network, which AirSane's "local" rule does not cover.
		"access.conf": "# Empty: access from anywhere (see SECURITY.md).\n",
	}
}

func ownerRef(o Owner) *metav1ac.OwnerReferenceApplyConfiguration {
	return metav1ac.OwnerReference().
		WithAPIVersion("apps/v1").WithKind("Deployment").
		WithName(o.Name).WithUID(o.UID).
		WithController(true).WithBlockOwnerDeletion(true)
}

func dataHash(data map[string]string) string {
	h := sha256.New()
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "%d:%s%d:%s", len(k), k, len(data[k]), data[k]) // hash writes never fail
	}
	return hex.EncodeToString(h.Sum(nil))
}
