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

	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/device"
)

// Names and labels of the rendered objects.
const (
	DeviceName      = "pagewire-device" // slot 0's Deployment and ConfigMap
	TokenSecretName = "pagewire-agent-token"
	TokenSecretKey  = "token"
	NameLabel       = "app.kubernetes.io/name"
	ManagedByLabel  = "app.kubernetes.io/managed-by"
	FieldManager    = "pagewire"

	SpecHashAnnotation   = "pagewire.thephaseless.github.io/spec-hash"
	ConfigHashAnnotation = "pagewire.thephaseless.github.io/config-hash"
	// NodeAnnotation names the node a Deployment is pinned to. It is on the
	// Deployment only, so the pod template (and its hash) does not carry it;
	// slot recovery reads it.
	NodeAnnotation = "pagewire.thephaseless.github.io/node"
	// WorkloadLabel marks every device workload; it is not in the selector.
	WorkloadLabel = "pagewire.thephaseless.github.io/device-workload"
)

// MaxSlots is the number of device workload slots. RBAC resourceNames match
// exactly, so deploy/base/role.yaml lists every SlotName.
const MaxSlots = 4

// SlotName is the Deployment and ConfigMap name of slot i: slot 0 keeps
// today's name, so an existing install is adopted in place.
func SlotName(i int) string {
	if i == 0 {
		return DeviceName
	}
	return DeviceName + "-" + strconv.Itoa(i)
}

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
	Slot        int    // workload slot, 0 to MaxSlots-1: names the Deployment and ConfigMap
	// BlobWorkload marks the one workload that mounts the blob volume (a
	// ReadWriteOnce PVC): its agent stages EULAs, and only it may fetch blobs.
	BlobWorkload bool
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
	"run-pagewire", "run-dbus", "run-avahi", "tmp", "agent-token", "blobs",
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
	USB           []device.USBKey // the devices the container serves
	USBAll        bool            // the container needs every USB device of the node (the probe)
	Sidecar       bool
	Mounts        []Mount
	Res           [4]string // cpu request, memory request, cpu limit, memory limit
}

func (c Container) usesUSB() bool { return c.USBAll || len(c.USB) > 0 }

// EnvVar is a container environment variable. FieldPath is for the Kubernetes
// downward API only; Value is ignored when it is set.
type EnvVar struct{ Name, Value, FieldPath string }

// Mount mounts the volume Vol (a VolumeOrder name) at Path; SubPath, when
// set, mounts that subdirectory of the volume instead.
type Mount struct {
	Vol, Path string
	RO        bool
	SubPath   string
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
	if p.Slot < 0 || p.Slot >= MaxSlots {
		return nil, nil, fmt.Errorf("render: slot %d out of range 0-%d", p.Slot, MaxSlots-1)
	}
	// The blob PVC is ReadWriteOnce: a second workload mounting it would never start.
	if len(spec.Blobs) > 0 && !p.BlobWorkload {
		return nil, nil, fmt.Errorf("render: workload %s needs vendor blobs but is not the blob workload", SlotName(p.Slot))
	}
	plan, err := Plan(spec, p)
	if err != nil {
		return nil, nil, err
	}

	name := SlotName(p.Slot)
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
	for _, v := range VolumeOrder {
		if used[v] {
			pod.WithVolumes(volume(v, name, p.BlobPVC))
		}
	}
	// Pinned by the Node object's name, which is what USBNodes returns; the
	// kubernetes.io/hostname label need not equal it.
	if spec.Node != "" {
		pod.WithAffinity(corev1ac.Affinity().WithNodeAffinity(corev1ac.NodeAffinity().
			WithRequiredDuringSchedulingIgnoredDuringExecution(corev1ac.NodeSelector().
				WithNodeSelectorTerms(corev1ac.NodeSelectorTerm().
					WithMatchFields(corev1ac.NodeSelectorRequirement().
						WithKey("metadata.name").WithOperator(corev1.NodeSelectorOpIn).WithValues(spec.Node))))))
	}

	labels := map[string]string{NameLabel: name, ManagedByLabel: FieldManager, WorkloadLabel: "true"}
	annotations := map[string]string{
		"reloader.stakater.com/auto":                  "false",
		SpecHashAnnotation:                            spec.Hash,
		"ignore-check.kube-linter.io/host-network":    hostNetworkReason,
		"ignore-check.kube-linter.io/run-as-non-root": rootReason,
	}
	if spec.Node != "" {
		annotations[NodeAnnotation] = spec.Node
	}
	deploy := appsv1ac.Deployment(name, p.Namespace).
		WithLabels(labels).
		WithAnnotations(annotations).
		WithOwnerReferences(ownerRef(p.Owner)).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(1).
			WithStrategy(appsv1ac.DeploymentStrategy().WithType("Recreate")).
			// Slot 0 keeps today's selector, which cannot change, so the live Deployment is adopted.
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{NameLabel: name})).
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
//
// One daemon per family serves every device of the workload: ipp-usb, one
// printer application per PAPPL family (in catalog order), one ippeveprinter
// per simulated printer and one AirSane for the airsane and simulated scanners.
func Plan(spec *device.Spec, p Params) (Pod, error) {
	if spec == nil {
		return Pod{}, errors.New("render: nil spec")
	}
	usbMode := spec.Mode == device.ModeUSB || spec.Mode == device.ModeProbe
	cat := catalog.Default()
	deviceImage := ImageRef(p.Registry, p.Tag, "pagewire-device", p.Overrides)

	probe := Container{
		Name: "probe", Image: p.SelfImage, Command: []string{"/pagewire"},
		Args: []string{"agent", "probe", "--out", device.InventoryFile},
		UID:  uidNonroot, USBAll: usbMode,
		Mounts: []Mount{{Vol: "run-pagewire", Path: "/run/pagewire"}},
		Res:    [4]string{"10m", "16Mi", "200m", "64Mi"},
	}
	if usbMode {
		probe.UID = uidRoot
	}
	dbus := Container{
		Name: "dbus", Image: deviceImage, Sidecar: true, UID: uidMessagebus,
		Command: []string{"/usr/bin/dbus-daemon"},
		Args:    []string{"--config-file=/etc/pagewire/dbus.conf", "--nofork", "--nopidfile"},
		Mounts:  []Mount{{Vol: "run-dbus", Path: "/run/dbus"}},
		Res:     [4]string{"5m", "8Mi", "100m", "32Mi"},
	}
	avahi := Container{
		Name: "avahi", Image: deviceImage, Sidecar: true, UID: uidAvahi,
		Command: []string{"/usr/sbin/avahi-daemon"},
		Args:    []string{"--no-drop-root", "--no-chroot", "--no-rlimits", "-f", "/etc/pagewire/avahi-daemon.conf"},
		// avahi-daemon creates /run/avahi-daemon itself and refuses it unless it
		// is owned by avahi:avahi, which an emptyDir mounted there never is; so
		// its own emptyDir is /run, with the shared /run/dbus mounted inside.
		Mounts: []Mount{{Vol: "run-avahi", Path: "/run"}, {Vol: "run-dbus", Path: "/run/dbus"}},
		Res:    [4]string{"5m", "8Mi", "100m", "32Mi"},
	}
	podUID, podUIDEnv := "$(POD_UID)", []EnvVar{{Name: "POD_UID", FieldPath: "metadata.uid"}}
	if p.PodUID != "" {
		podUID, podUIDEnv = p.PodUID, nil
	}
	agent := Container{
		Name: "agent", Image: p.SelfImage, Sidecar: spec.Mode != device.ModeProbe, UID: uidNonroot,
		Command: []string{"/pagewire"},
		Args: []string{
			"agent", "run",
			"--control-plane-url", p.AgentURL,
			"--token-file", device.AgentTokenFile,
			"--pod-uid", podUID,
			"--dbus-address", device.DBusAddress,
			"--inventory", device.InventoryFile,
			"--blob-status", device.BlobStatusFile,
			device.AgentWorkloadArg, spec.Node,
		},
		Env: podUIDEnv,
		Mounts: []Mount{
			{Vol: "agent-token", Path: "/var/run/pagewire", RO: true},
			{Vol: "run-pagewire", Path: "/run/pagewire", RO: true},
			{Vol: "run-dbus", Path: "/run/dbus"},
		},
		Res: [4]string{"10m", "32Mi", "200m", "128Mi"},
	}

	// The agent stages EULA texts on the blob volume, so the wizard and
	// Settings can show a licence before it is accepted.
	if usbMode && p.BlobWorkload && p.BlobPVC != "" {
		agent.Args = append(agent.Args, "--blob-dir", "/blobs")
		agent.Mounts = append(agent.Mounts, Mount{Vol: "blobs", Path: "/blobs"})
	}

	inits := []Container{probe, dbus, avahi}
	if spec.Mode == device.ModeProbe {
		return Pod{Inits: inits, Mains: []Container{agent}, Config: configData(cat, spec)}, nil
	}
	if spec.Mode != device.ModeUSB && spec.Mode != device.ModeSimulated {
		return Pod{}, fmt.Errorf("render: unknown spec mode %q", spec.Mode)
	}
	inits = append(inits, agent)
	if len(spec.Blobs) > 0 {
		inits = append(inits, fetchBlobs(spec, p.SelfImage))
	}

	var ippUSB, airsaneUSB []device.USBKey
	pappl := map[string][]device.USBKey{} // PAPPL family -> its printers' devices
	var sims []device.PrinterSpec
	for _, ps := range spec.Printers {
		fam, ok := cat.Printer(ps.Family)
		switch {
		case !ok:
			return Pod{}, fmt.Errorf("render: unknown printer family %q", ps.Family)
		case ps.Family == "ipp-usb":
			ippUSB = addKey(ippUSB, ps.USB)
		case ps.Family == "sim":
			// ippeveprinter takes the name as its last argument: one starting
			// with - would be read as an option. Config validation refuses it too.
			if strings.HasPrefix(ps.Name, "-") {
				return Pod{}, fmt.Errorf("render: printer name %q must not start with -", ps.Name)
			}
			sims = append(sims, ps)
		case fam.App != "":
			pappl[ps.Family] = addKey(pappl[ps.Family], ps.USB)
		default:
			return Pod{}, fmt.Errorf("render: printer family %q has no daemon", ps.Family)
		}
	}
	runAirsane := false
	for _, ss := range spec.Scanners {
		switch ss.Family {
		case "ipp-usb":
			ippUSB = addKey(ippUSB, ss.USB)
		case "airsane":
			airsaneUSB, runAirsane = addKey(airsaneUSB, ss.USB), true
		case "sim":
			runAirsane = true
		default:
			return Pod{}, fmt.Errorf("render: unknown scanner family %q", ss.Family)
		}
	}

	var mains []Container
	if len(ippUSB) > 0 {
		mains = append(mains, Container{
			Name: "ipp-usb", Image: deviceImage, UID: uidRoot, USB: ippUSB,
			Command: []string{"/usr/local/sbin/ipp-usb"}, Args: []string{"standalone"},
			Mounts: []Mount{
				{Vol: "ipp-usb-conf", Path: "/etc/ipp-usb", RO: true},
				{Vol: "run-dbus", Path: "/run/dbus"},
				{Vol: "ipp-usb-state", Path: "/var/ipp-usb"},
				{Vol: "ipp-usb-log", Path: "/var/log/ipp-usb"},
			},
			Res: [4]string{"10m", "32Mi", "500m", "128Mi"},
		})
	}
	for _, fam := range cat.Printers {
		keys := pappl[fam.ID]
		if len(keys) == 0 {
			continue
		}
		// Each app keeps its state in its own subdirectory of the shared
		// volumes, so two apps in one pod never touch each other's files.
		sub := fam.ID
		c := Container{
			Name: "printer-app-" + fam.ID, Image: ImageRef(p.Registry, p.Tag, fam.Image, p.Overrides), UID: uidRoot, USB: keys,
			Command: []string{"/usr/local/bin/" + fam.App},
			// log-file=- logs to stderr, so nothing is written under the read-only root.
			Args: []string{"-o", "server-port=" + strconv.Itoa(fam.Port), "-o", "log-file=-", "server"},
			Mounts: []Mount{
				{Vol: "app-run", Path: "/run", SubPath: sub}, // PAPPL's control socket /run/<app>.sock
				{Vol: "run-dbus", Path: "/run/dbus"},
				{Vol: "app-state", Path: "/var/lib/" + fam.App, SubPath: sub},
				{Vol: "app-spool", Path: "/var/spool/" + fam.App, SubPath: sub},
				{Vol: "tmp", Path: "/tmp"},
				{Vol: "app-ssl", Path: "/etc/cups/ssl", SubPath: sub},                // PAPPL's self-signed TLS certificate
				{Vol: "app-fontconfig", Path: "/var/cache/fontconfig", SubPath: sub}, // the filters' font cache
			},
			Res: [4]string{"50m", "64Mi", "2", "1Gi"},
		}
		if len(spec.Blobs) > 0 {
			c.Mounts = append(c.Mounts, Mount{Vol: "blobs", Path: "/blobs", RO: true})
		}
		mains = append(mains, c)
	}
	for i, ps := range sims {
		mains = append(mains, Container{
			Name: "sim-printer-" + strconv.Itoa(i), Image: deviceImage, UID: uidNonroot,
			Command: []string{"/usr/sbin/ippeveprinter"},
			Args: []string{
				"-p", strconv.Itoa(ps.Port),
				"-f", "application/pdf,application/postscript,image/jpeg,image/pwg-raster,image/urf",
				"-l", ps.Location, ps.Name,
			},
			Mounts: []Mount{{Vol: "run-dbus", Path: "/run/dbus"}, {Vol: "tmp", Path: "/tmp"}},
			Res:    [4]string{"10m", "16Mi", "500m", "128Mi"},
		})
	}
	if runAirsane {
		mains = append(mains, airsane(cat, spec, deviceImage, airsaneUSB))
	}
	return Pod{Inits: inits, Mains: mains, Config: configData(cat, spec)}, nil
}

// addKey appends k to keys unless it is already there.
func addKey(keys []device.USBKey, k device.USBKey) []device.USBKey {
	if slices.Contains(keys, k) {
		return keys
	}
	return append(keys, k)
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
		Command: []string{"/pagewire"}, Args: args,
		Mounts: []Mount{{Vol: "blobs", Path: "/blobs"}, {Vol: "run-pagewire", Path: "/run/pagewire"}},
		Res:    [4]string{"10m", "32Mi", "500m", "256Mi"},
	}
}

// airsane serves the airsane scanners' devices (usb) and the simulated
// scanner; it runs as root only when it has a USB device.
func airsane(cat *catalog.Catalog, spec *device.Spec, image string, usb []device.USBKey) Container {
	port := 0
	for _, s := range cat.Scanners {
		if s.ID == "airsane" {
			port = s.Port
		}
	}
	uid := int64(uidNonroot)
	if len(usb) > 0 {
		uid = uidRoot
	}
	c := Container{
		Name: "airsane", Image: image, UID: uid, USB: usb,
		Command: []string{"/usr/local/bin/airsaned"},
		Args: []string{
			"--listen-port=" + strconv.Itoa(port), "--hotplug=false",
			"--options-file=/etc/pagewire/airsane/options.conf",
			"--ignore-list=/etc/pagewire/airsane/ignore.conf",
			"--access-file=/etc/pagewire/airsane/access.conf",
		},
		Env: []EnvVar{{Name: "SANE_CONFIG_DIR", Value: "/etc/pagewire/sane.d:"}},
		Mounts: []Mount{
			{Vol: "airsane-conf", Path: "/etc/pagewire/airsane", RO: true},
			{Vol: "sane-conf", Path: "/etc/pagewire/sane.d", RO: true},
			{Vol: "run-dbus", Path: "/run/dbus"},
			{Vol: "tmp", Path: "/tmp"},
		},
		Res: [4]string{"20m", "32Mi", "1", "512Mi"},
	}
	if len(spec.Blobs) > 0 {
		c.Mounts = append(c.Mounts, Mount{Vol: "blobs", Path: "/blobs", RO: true})
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
	if c.usesUSB() {
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
		if m.SubPath != "" {
			vm.WithSubPath(m.SubPath)
		}
		out.WithVolumeMounts(vm)
	}
	return out
}

// volume is the pod volume name; configMap is the slot's ConfigMap.
func volume(name, configMap, blobPVC string) *corev1ac.VolumeApplyConfiguration {
	v := corev1ac.Volume().WithName(name)
	items := func(keys ...string) *corev1ac.ConfigMapVolumeSourceApplyConfiguration {
		s := corev1ac.ConfigMapVolumeSource().WithName(configMap)
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
	name := SlotName(p.Slot)
	return corev1ac.ConfigMap(name, p.Namespace).
		WithLabels(map[string]string{NameLabel: name, ManagedByLabel: FieldManager}).
		WithOwnerReferences(ownerRef(p.Owner)).
		WithData(data)
}

// configData is the device config files, keyed by file name.
func configData(cat *catalog.Catalog, spec *device.Spec) map[string]string {
	// dll.conf: the sorted union of the airsane scanners' backends (the
	// default ones for a scanner without any), plus "test" for the simulated
	// scanner; the default backends, in catalog order, when that leaves nothing
	// (probe, printers only).
	var backends []string
	for _, ss := range spec.Scanners {
		switch {
		case ss.Family == "sim":
			backends = append(backends, "test")
		case ss.Family != "airsane":
		case len(ss.SaneBackends) == 0:
			backends = append(backends, cat.DefaultSaneBackends...)
		default:
			backends = append(backends, ss.SaneBackends...)
		}
	}
	slices.Sort(backends)
	backends = slices.Compact(backends)
	if len(backends) == 0 {
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
