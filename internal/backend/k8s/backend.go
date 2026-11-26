package k8s

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/device"
)

// Backend runs the device workloads through the Kubernetes API: one
// Deployment per node with USB devices, in the fixed slots SlotName(0) to
// SlotName(MaxSlots-1). It implements device.Backend, device.PodRestarter,
// device.NodeLister, device.AgentTokenSource and device.Notifier.
//
// Single-node mode (the default, and whenever listing nodes fails, such as
// without the optional pagewire-nodes ClusterRole) runs slot 0 alone, unpinned,
// and makes only the calls the namespaced Role allows for slot 0. Multi-node
// mode (after USBNodes succeeds) pins each workload to its node.
type Backend struct {
	client kubernetes.Interface
	p      Params

	// mu guards everything below; it is held across API calls, but never
	// by AgentToken.
	mu       sync.Mutex
	resolved bool // p.Owner.UID and p.SelfImage come from the owner Deployment

	multi      bool           // set by the last USBNodes call
	recovered  bool           // slots was read from the Deployments since multi turned true
	slots      map[int]string // multi-node: slot -> node; "" is an unpinned slot (0 only)
	nodesErr   error          // the last 403/404 node-list failure, returned until nodesAt+nodeBackoff
	nodesAt    time.Time
	nodesFails bool         // the last node list failed; latches the log line
	warned     map[int]bool // slots whose pods were skipped for want of a node, logged once

	tokMu  sync.Mutex // separate from mu: agent auth must not wait for an Apply
	tok    string
	tokErr error
	tokAt  time.Time
	now    func() time.Time

	nodesSig chan nodesState                                     // latest-wins mode signal from USBNodes to Notify
	newTimer func(time.Duration) (<-chan time.Time, func() bool) // every Notify wait; tests replace it
}

// tokenTTL bounds how often AgentToken reads the Secret, so unauthenticated
// requests to the internal listener cannot drive API server load.
const tokenTTL = 5 * time.Second

// nodeBackoff is how long a failed node list is reused before the next try,
// so single-node installs (no ClusterRole) call the API every 5 minutes at most.
// Notify re-arms a one-shot re-check from each failed list, so a ClusterRole
// added later is found without an event.
const nodeBackoff = 5 * time.Minute

// nodesState is the node mode USBNodes signals to Notify: multi-node, or
// single-node after the failed list at at.
type nodesState struct {
	multi bool
	at    time.Time
}

var (
	_ device.Backend          = (*Backend)(nil)
	_ device.PodRestarter     = (*Backend)(nil)
	_ device.NodeLister       = (*Backend)(nil)
	_ device.AgentTokenSource = (*Backend)(nil)
	_ device.Notifier         = (*Backend)(nil)
)

// New returns a Backend. It makes no API call; the owner Deployment is read on
// the first Apply with a spec.
func New(client kubernetes.Interface, p Params) *Backend {
	return &Backend{client: client, p: p, now: time.Now, nodesSig: make(chan nodesState, 1), newTimer: func(d time.Duration) (<-chan time.Time, func() bool) {
		t := time.NewTimer(d)
		return t.C, t.Stop
	}}
}

// signalNodes replaces any unread signal with s; it never blocks. Callers hold b.mu.
func (b *Backend) signalNodes(s nodesState) {
	select {
	case <-b.nodesSig:
	default:
	}
	select {
	case b.nodesSig <- s:
	default:
	}
}

// slotSelector lists the pods of one slot.
func slotSelector(slot int) metav1.ListOptions {
	return metav1.ListOptions{LabelSelector: NameLabel + "=" + SlotName(slot)}
}

// allSlotsSelector lists the pods of every slot.
func allSlotsSelector() metav1.ListOptions {
	names := make([]string, MaxSlots)
	for i := range names {
		names[i] = SlotName(i)
	}
	return metav1.ListOptions{LabelSelector: NameLabel + " in (" + strings.Join(names, ",") + ")"}
}

// USBNodes lists the Nodes and returns, sorted, those whose allocatable
// USBResource is above 0. A 403 (no optional ClusterRole) or 404 switches to
// single-node mode with an error wrapping device.ErrSingleNode, returned
// again without an API call for nodeBackoff. Any other error (a timeout, a
// 5xx) is transient: the mode and slot map stay, and the next call retries.
// Each 403/404 and each switch to multi-node mode signals Notify, which arms
// or drops the single-node re-check; cached and transient errors send nothing.
func (b *Backend) USBNodes(ctx context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.nodesErr != nil && b.now().Sub(b.nodesAt) < nodeBackoff {
		return nil, b.nodesErr
	}
	list, err := b.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsForbidden(err) && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	if err != nil {
		err = fmt.Errorf("%w: %w", device.ErrSingleNode, err)
		if !b.nodesFails {
			slog.Info("listing nodes failed; running one device workload (single-node mode)", "err", err)
		}
		b.nodesErr, b.nodesAt, b.nodesFails = err, b.now(), true
		b.multi, b.recovered, b.slots = false, false, nil
		b.signalNodes(nodesState{at: b.nodesAt})
		return nil, err
	}
	if b.nodesFails {
		slog.Info("listing nodes works again; running one device workload per node (multi-node mode)")
	}
	b.nodesErr, b.nodesFails = nil, false
	if !b.multi {
		b.multi, b.recovered, b.slots = true, false, nil
		b.signalNodes(nodesState{multi: true})
	}
	var names []string
	for i := range list.Items {
		if hasUSB(&list.Items[i], b.p.USBResource) {
			names = append(names, list.Items[i].Name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// hasUSB reports whether node n's allocatable resource res is above 0.
func hasUSB(n *corev1.Node, res string) bool {
	q, ok := n.Status.Allocatable[corev1.ResourceName(res)]
	return res != "" && ok && q.Sign() > 0
}

// recoverSlots reads the slot -> node map from the Deployments' node
// annotations, once per switch to multi-node mode. A slot-0 Deployment
// without the annotation (the single-node workload) is an unpinned slot,
// observed as workload "" until an Apply pins it. Callers hold b.mu.
func (b *Backend) recoverSlots(ctx context.Context) error {
	if !b.multi || b.recovered {
		return nil
	}
	slots := map[int]string{}
	for i := range MaxSlots {
		d, err := b.client.AppsV1().Deployments(b.p.Namespace).Get(ctx, SlotName(i), metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return fmt.Errorf("recover device workload %s: %w", SlotName(i), err)
		}
		node := d.Annotations[NodeAnnotation]
		if node == "" && i != 0 {
			continue // not ours to adopt: only slot 0 ever ran unpinned
		}
		slots[i] = node
	}
	b.slots, b.recovered = slots, true
	return nil
}

// slotMap is the current slot -> node map. Callers hold b.mu.
func (b *Backend) slotMap() map[int]string {
	if !b.multi {
		return map[int]string{0: ""}
	}
	return b.slots
}

// Apply makes the device workloads exactly specs. A spec with Node "" runs
// in slot 0; in multi-node mode each node keeps its slot and a new node takes
// the lowest free one, and the workloads of unused slots are deleted. Single-
// node mode never touches slots 1 to MaxSlots-1. Empty specs delete the
// workloads. The nodes that fit are applied even when some do not. When only
// some slots fail, the error is a device.WorkloadErrors naming their nodes (a
// freed slot by the node it held).
func (b *Backend) Apply(ctx context.Context, specs []*device.Spec) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.recoverSlots(ctx); err != nil {
		return err
	}
	held := maps.Clone(b.slotMap())
	assigned, err := b.assign(specs)
	if assigned == nil || err != nil && len(assigned) == 0 {
		return err
	}
	if len(assigned) > 0 {
		if err := b.resolveOwner(ctx); err != nil {
			return err
		}
	}
	failed := device.WorkloadErrors{}
	blob := blobSlot(assigned)
	for slot := range MaxSlots {
		spec, ok := assigned[slot]
		switch {
		case ok:
			p := b.p
			p.Slot, p.BlobWorkload = slot, slot == blob
			if e := b.applySlot(ctx, spec, p); e != nil {
				failed[spec.Node] = e
			}
		case slot == 0 || b.multi:
			if e := b.deleteSlot(ctx, slot); e != nil {
				failed[held[slot]] = errors.Join(failed[held[slot]], e)
				if _, kept := held[slot]; kept && b.multi {
					b.slots[slot] = held[slot] // the retry names the same node
				}
			}
		}
	}
	switch {
	case err != nil: // nodes left out: not one workload's failure
		if len(failed) > 0 {
			return errors.Join(err, failed)
		}
		return err
	case len(failed) > 0:
		return failed
	}
	return nil
}

// assign maps specs to slots and records the multi-node slot map. A nil map
// means nothing may be applied. Callers hold b.mu.
func (b *Backend) assign(specs []*device.Spec) (map[int]*device.Spec, error) {
	assigned := map[int]*device.Spec{}
	var named []*device.Spec
	for _, s := range specs {
		switch {
		case s == nil:
			return nil, errors.New("apply: nil spec")
		case s.Node == "":
			if len(specs) != 1 {
				return nil, errors.New("apply: a node-less spec must be the only one")
			}
			assigned[0] = s
		case !b.multi:
			return nil, fmt.Errorf("apply: spec for node %s in single-node mode", s.Node)
		default:
			named = append(named, s)
		}
	}
	if !b.multi {
		return assigned, nil
	}
	slices.SortFunc(named, func(x, y *device.Spec) int { return strings.Compare(x.Node, y.Node) })
	slots := map[int]string{}
	if assigned[0] != nil {
		slots[0] = ""
	}
	var unplaced []*device.Spec
	for _, s := range named { // a node keeps its slot
		if i, ok := slotOf(b.slots, s.Node); ok {
			assigned[i], slots[i] = s, s.Node
		} else {
			unplaced = append(unplaced, s)
		}
	}
	var out []string
	i := 0
	for _, s := range unplaced { // a new node takes the lowest free slot
		for i < MaxSlots && assigned[i] != nil {
			i++
		}
		if i == MaxSlots {
			out = append(out, s.Node)
			continue
		}
		assigned[i], slots[i] = s, s.Node
	}
	b.slots = slots
	if len(out) > 0 {
		return assigned, fmt.Errorf("more than %d nodes with USB devices: %s not served", MaxSlots, strings.Join(out, ", "))
	}
	return assigned, nil
}

// slotOf returns the slot pinned to node.
func slotOf(slots map[int]string, node string) (int, bool) {
	for i, n := range slots {
		if n == node && node != "" {
			return i, true
		}
	}
	return 0, false
}

// blobSlot picks the one workload that mounts the ReadWriteOnce blob volume,
// from the specs alone: the spec with blobs, else the one with a printer whose
// catalog family needs or offers a EULA, else any; ties go to the lowest node
// name ("" first). It returns that spec's slot, or -1 for no specs.
func blobSlot(assigned map[int]*device.Spec) int {
	cat := catalog.Default()
	eula := func(s *device.Spec) bool {
		return slices.ContainsFunc(s.Printers, func(ps device.PrinterSpec) bool {
			f, ok := cat.Printer(ps.Family)
			return ok && len(f.RequiredEULAs)+len(f.OptionalEULAs) > 0
		})
	}
	slots := slices.Collect(maps.Keys(assigned))
	slices.SortFunc(slots, func(x, y int) int { return strings.Compare(assigned[x].Node, assigned[y].Node) })
	for _, want := range []func(*device.Spec) bool{
		func(s *device.Spec) bool { return len(s.Blobs) > 0 },
		eula,
		func(*device.Spec) bool { return true },
	} {
		for _, i := range slots {
			if want(assigned[i]) {
				return i
			}
		}
	}
	return -1
}

// applySlot renders spec and server-side applies its ConfigMap and Deployment.
// Callers hold b.mu.
func (b *Backend) applySlot(ctx context.Context, spec *device.Spec, p Params) error {
	ns, name := b.p.Namespace, SlotName(p.Slot)
	// Render before creating anything, so a spec it refuses leaves no Secret behind.
	deploy, cm, err := Render(spec, p)
	if err != nil {
		return fmt.Errorf("device workload %s: %w", name, err)
	}
	if err := b.ensureToken(ctx); err != nil {
		return err
	}
	opts := metav1.ApplyOptions{FieldManager: FieldManager, Force: true}
	if _, err := b.client.CoreV1().ConfigMaps(ns).Apply(ctx, cm, opts); err != nil {
		return fmt.Errorf("apply configmap %s/%s: %w", ns, name, err)
	}
	if _, err := b.client.AppsV1().Deployments(ns).Apply(ctx, deploy, opts); err != nil {
		return fmt.Errorf("apply deployment %s/%s: %w", ns, name, err)
	}
	return nil
}

// deleteSlot deletes the Deployment and ConfigMap of slot; missing ones are fine.
func (b *Backend) deleteSlot(ctx context.Context, slot int) error {
	ns, name, del := b.p.Namespace, SlotName(slot), metav1.DeleteOptions{}
	return errors.Join(
		ignoreNotFound(b.client.AppsV1().Deployments(ns).Delete(ctx, name, del)),
		ignoreNotFound(b.client.CoreV1().ConfigMaps(ns).Delete(ctx, name, del)),
	)
}

// resolveOwner reads the owner's UID and SelfContainer image once; a failure is
// not cached. Callers hold b.mu.
func (b *Backend) resolveOwner(ctx context.Context) error {
	if b.resolved {
		return nil
	}
	ns, name := b.p.Namespace, b.p.Owner.Name
	d, err := b.client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("owner deployment %s/%s: %w", ns, name, err)
	}
	b.p.Owner.UID = d.UID
	b.p.SelfImage = ""
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == SelfContainer {
			b.p.SelfImage = c.Image
		}
	}
	if b.p.SelfImage == "" {
		b.p.SelfImage = ImageRef(b.p.Registry, b.p.Tag, "pagewire", b.p.Overrides)
		slog.Warn("owner deployment has no container named "+SelfContainer+"; using the fallback agent image",
			"deployment", ns+"/"+name, "image", b.p.SelfImage)
	}
	b.resolved = true
	return nil
}

// ensureToken gives the agent token Secret a token when it is missing or empty
// and never changes an existing token. It server-side applies the Secret by
// name, so RBAC can scope create and patch to that one Secret.
func (b *Backend) ensureToken(ctx context.Context) error {
	ns := b.p.Namespace
	secrets := b.client.CoreV1().Secrets(ns)
	s, err := secrets.Get(ctx, TokenSecretName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("secret %s/%s: %w", ns, TokenSecretName, err)
	}
	if err == nil && len(s.Data[TokenSecretKey]) > 0 {
		return nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	tok := hex.EncodeToString(raw)
	ac := corev1ac.Secret(TokenSecretName, ns).
		WithLabels(map[string]string{NameLabel: DeviceName, ManagedByLabel: FieldManager}).
		WithOwnerReferences(ownerRef(b.p.Owner)).
		WithType(corev1.SecretTypeOpaque).
		WithData(map[string][]byte{TokenSecretKey: []byte(tok)})
	if _, err := secrets.Apply(ctx, ac, metav1.ApplyOptions{FieldManager: FieldManager, Force: true}); err != nil {
		return fmt.Errorf("apply secret %s/%s: %w", ns, TokenSecretName, err)
	}
	b.tokMu.Lock()
	b.tok, b.tokErr, b.tokAt = tok, nil, b.now()
	b.tokMu.Unlock()
	return nil
}

// Observe reports one Observation per known workload, by slot order, or one
// PhaseAbsent Observation for workload "" when there is none. Endpoints stay
// empty: in Kubernetes they come from agent reports. Single-node mode lists
// slot 0's pods only; multi-node mode lists every slot's and skips the pods of
// a slot with no known node.
func (b *Backend) Observe(ctx context.Context) ([]device.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.recoverSlots(ctx); err != nil {
		return nil, err
	}
	sel := slotSelector(0)
	if b.multi {
		sel = allSlotsSelector()
	}
	pods, err := b.client.CoreV1().Pods(b.p.Namespace).List(ctx, sel)
	if err != nil {
		return nil, fmt.Errorf("list device pods: %w", err)
	}
	bySlot := map[int][]corev1.Pod{}
	for _, p := range pods.Items {
		slot := podSlot(p.Labels[NameLabel])
		if slot < 0 {
			continue
		}
		bySlot[slot] = append(bySlot[slot], p)
	}
	slots := b.slotMap()
	if b.warned == nil {
		b.warned = map[int]bool{}
	}
	for slot := range bySlot {
		if _, ok := slots[slot]; !ok && !b.warned[slot] {
			b.warned[slot] = true
			slog.Warn("device pods of a workload with no known node are ignored", "workload", SlotName(slot))
		}
	}
	var out []device.Observation
	for slot := range MaxSlots {
		node, ok := slots[slot]
		if !ok {
			continue
		}
		delete(b.warned, slot)
		obs := observe(bySlot[slot])
		obs.Workload = node
		out = append(out, obs)
	}
	if len(out) == 0 {
		out = []device.Observation{{Phase: device.PhaseAbsent}}
	}
	return out, nil
}

// podSlot is the slot of a pod's name label, -1 for none.
func podSlot(name string) int {
	if name == DeviceName {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(name, DeviceName+"-"))
	if err != nil || n < 1 || n >= MaxSlots || SlotName(n) != name {
		return -1
	}
	return n
}

// observe reports the state of one slot's pods.
func observe(pods []corev1.Pod) device.Observation {
	if len(pods) == 0 {
		return device.Observation{Phase: device.PhaseAbsent}
	}
	pod := pods[0]
	for _, p := range pods { // prefer a pod that is not terminating (Recreate rollouts)
		if p.DeletionTimestamp == nil {
			pod = p
			break
		}
	}
	obs := device.Observation{HostIP: pod.Status.HostIP}
	switch pod.Status.Phase {
	case corev1.PodRunning:
		obs.Phase = device.PhaseRunning
	case corev1.PodFailed, corev1.PodSucceeded:
		obs.Phase = device.PhaseFailed
	default:
		obs.Phase = device.PhasePending
	}
	statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, s := range statuses {
		if w := s.State.Waiting; w != nil && w.Reason != "" {
			obs.Message = s.Name + ": " + w.Reason
			break
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			obs.Unschedulable = true
			obs.Message = c.Message
		}
	}
	return obs
}

// RestartPod deletes, by name, every pod of the workload pinned to node
// workload ("" is slot 0), so its Deployment recreates it and the kubelet
// allocates the current USB device nodes.
func (b *Backend) RestartPod(ctx context.Context, workload string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.recoverSlots(ctx); err != nil {
		return err
	}
	slot, ok := 0, workload == ""
	if !ok {
		slot, ok = slotOf(b.slotMap(), workload)
	}
	if !ok {
		return fmt.Errorf("restart: no device workload for node %s", workload)
	}
	pods := b.client.CoreV1().Pods(b.p.Namespace)
	list, err := pods.List(ctx, slotSelector(slot))
	if err != nil {
		return fmt.Errorf("list device pods: %w", err)
	}
	var errs []error
	for _, p := range list.Items {
		errs = append(errs, ignoreNotFound(pods.Delete(ctx, p.Name, metav1.DeleteOptions{})))
	}
	return errors.Join(errs...)
}

// AgentToken returns the agent's bearer token from its Secret. It reads the
// Secret at most once per tokenTTL and returns the cached token or error between
// reads; an empty token is an error.
func (b *Backend) AgentToken(ctx context.Context) (string, error) {
	b.tokMu.Lock()
	defer b.tokMu.Unlock()
	if !b.tokAt.IsZero() && b.now().Sub(b.tokAt) < tokenTTL {
		return b.tok, b.tokErr
	}
	b.tok, b.tokErr = "", nil
	s, err := b.client.CoreV1().Secrets(b.p.Namespace).Get(ctx, TokenSecretName, metav1.GetOptions{})
	switch {
	case err != nil:
		b.tokErr = fmt.Errorf("secret %s/%s: %w", b.p.Namespace, TokenSecretName, err)
	case len(s.Data[TokenSecretKey]) == 0:
		b.tokErr = fmt.Errorf("secret %s/%s has no %q key", b.p.Namespace, TokenSecretName, TokenSecretKey)
	default:
		b.tok = string(s.Data[TokenSecretKey])
	}
	if ctx.Err() == nil { // a cancelled request must not lock agents out for tokenTTL
		b.tokAt = b.now()
	}
	return b.tok, b.tokErr
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
