package k8s

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ThePhaseless/pagewire/internal/device"
)

const ns = "printing"

var ownerImage = "ghcr.io/thephaseless/pagewire:1.2.3" + digest

func ownerDeployment(containers ...corev1.Container) *appsv1.Deployment {
	if containers == nil {
		containers = []corev1.Container{{Name: SelfContainer, Image: ownerImage}}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "pagewire", Namespace: ns, UID: "owner-uid"},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: containers}}},
	}
}

func backendParams() Params {
	return Params{
		Namespace:   ns,
		Owner:       Owner{Name: "pagewire"},
		Registry:    "ghcr.io/thephaseless",
		Tag:         "1.2.3",
		AgentURL:    "http://pagewire.printing.svc:8081",
		USBResource: "devic.es/printer",
		BlobPVC:     "pagewire-blobs",
	}
}

func count(c *fake.Clientset, verb, resource string) int {
	n := 0
	for _, a := range c.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == resource {
			if g, ok := a.(k8stesting.GetAction); ok && verb == "get" && resource == "deployments" && g.GetName() != "pagewire" {
				continue // only owner lookups
			}
			n++
		}
	}
	return n
}

func one(s *device.Spec) []*device.Spec { return []*device.Spec{s} }

func deviceDeployment(t *testing.T, c *fake.Clientset) *appsv1.Deployment {
	t.Helper()
	d, err := c.AppsV1().Deployments(ns).Get(context.Background(), DeviceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func image(t *testing.T, d *appsv1.Deployment, name string) string {
	t.Helper()
	for _, c := range append(d.Spec.Template.Spec.InitContainers, d.Spec.Template.Spec.Containers...) {
		if c.Name == name {
			return c.Image
		}
	}
	t.Fatalf("no container %s", name)
	return ""
}

func TestApply(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset(ownerDeployment())
	b := New(c, backendParams())
	if len(c.Actions()) != 0 {
		t.Fatalf("New made API calls: %v", c.Actions())
	}
	spec := goldenSpecs["ippusb"]
	if err := b.Apply(ctx, one(spec)); err != nil {
		t.Fatal(err)
	}
	d := deviceDeployment(t, c)
	if image(t, d, "probe") != ownerImage || image(t, d, "agent") != ownerImage {
		t.Errorf("self images %s %s", image(t, d, "probe"), image(t, d, "agent"))
	}
	if refs := d.OwnerReferences; len(refs) != 1 || refs[0].UID != "owner-uid" {
		t.Errorf("ownerReferences %+v", refs)
	}
	cm, err := c.CoreV1().ConfigMaps(ns).Get(ctx, DeviceName, metav1.GetOptions{})
	if err != nil || len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != "owner-uid" {
		t.Fatalf("configmap %+v %v", cm, err)
	}
	tok, err := b.AgentToken(ctx)
	if err != nil || len(tok) != 64 {
		t.Fatalf("token %q %v", tok, err)
	}

	// Idempotent second apply, then a spec change: owner fetched once, Secret applied once and kept.
	if err := b.Apply(ctx, one(spec)); err != nil {
		t.Fatal(err)
	}
	if err := b.Apply(ctx, one(goldenSpecs["hplip-airsane-blob"])); err != nil {
		t.Fatal(err)
	}
	if n := count(c, "get", "deployments"); n != 1 {
		t.Errorf("owner fetched %d times", n)
	}
	if n := count(c, "patch", "secrets"); n != 1 || count(c, "create", "secrets") != 0 {
		t.Errorf("secret applied %d times, actions %v", n, c.Actions())
	}
	if count(c, "patch", "deployments") != 3 || count(c, "patch", "configmaps") != 3 {
		t.Errorf("want 3 applies each, actions %v", c.Actions())
	}
	if tok2, _ := b.AgentToken(ctx); tok2 != tok {
		t.Error("token changed")
	}
	d = deviceDeployment(t, c)
	if d.Spec.Template.Annotations[SpecHashAnnotation] != goldenSpecs["hplip-airsane-blob"].Hash || image(t, d, "fetch-blobs") != ownerImage {
		t.Errorf("spec change not applied: %v", d.Spec.Template.Annotations)
	}

	// Nil spec deletes, without an owner lookup; deleting again succeeds.
	for range 2 {
		if err := b.Apply(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.AppsV1().Deployments(ns).Get(ctx, DeviceName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("deployment still there: %v", err)
	}
	if _, err := c.CoreV1().ConfigMaps(ns).Get(ctx, DeviceName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("configmap still there: %v", err)
	}
}

func TestApplyNilWithoutOwner(t *testing.T) {
	c := fake.NewClientset()
	if err := New(c, backendParams()).Apply(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if n := count(c, "get", "deployments"); n != 0 {
		t.Errorf("nil spec looked up the owner %d times", n)
	}
}

// assertNothingCreated checks that no write was recorded and no device object exists.
func assertNothingCreated(t *testing.T, c *fake.Clientset) {
	t.Helper()
	if n := count(c, "create", "secrets") + count(c, "patch", "secrets") + count(c, "patch", "deployments") + count(c, "patch", "configmaps"); n != 0 {
		t.Errorf("%d objects written", n)
	}
	ctx := context.Background()
	_, e1 := c.AppsV1().Deployments(ns).Get(ctx, DeviceName, metav1.GetOptions{})
	_, e2 := c.CoreV1().ConfigMaps(ns).Get(ctx, DeviceName, metav1.GetOptions{})
	_, e3 := c.CoreV1().Secrets(ns).Get(ctx, TokenSecretName, metav1.GetOptions{})
	for _, err := range []error{e1, e2, e3} {
		if !apierrors.IsNotFound(err) {
			t.Errorf("want NotFound, got %v", err)
		}
	}
}

func TestApplyNoUSBResource(t *testing.T) {
	c := fake.NewClientset(ownerDeployment())
	p := backendParams()
	p.USBResource = ""
	err := New(c, p).Apply(context.Background(), one(goldenSpecs["ippusb"]))
	if !errors.Is(err, ErrNoUSBResource) {
		t.Fatalf("err = %v", err)
	}
	assertNothingCreated(t, c)
}

func TestOwnerFallback(t *testing.T) {
	ctx := context.Background()
	other := corev1.Container{Name: "sidecar", Image: "busybox:1"}

	c := fake.NewClientset(ownerDeployment(other))
	if err := New(c, backendParams()).Apply(ctx, one(goldenSpecs["probe"])); err != nil {
		t.Fatal(err)
	}
	if got := image(t, deviceDeployment(t, c), "agent"); got != "ghcr.io/thephaseless/pagewire:1.2.3" {
		t.Errorf("fallback image %s", got)
	}

	p := backendParams()
	p.Overrides = map[string]string{"pagewire": "localhost:5000/ps:x"}
	c = fake.NewClientset(ownerDeployment(other))
	if err := New(c, p).Apply(ctx, one(goldenSpecs["probe"])); err != nil {
		t.Fatal(err)
	}
	if got := image(t, deviceDeployment(t, c), "agent"); got != "localhost:5000/ps:x" {
		t.Errorf("override fallback image %s", got)
	}

	// The owner's pagewire container beats the override.
	c = fake.NewClientset(ownerDeployment())
	if err := New(c, p).Apply(ctx, one(goldenSpecs["probe"])); err != nil {
		t.Fatal(err)
	}
	if got := image(t, deviceDeployment(t, c), "agent"); got != ownerImage {
		t.Errorf("owner image %s", got)
	}
}

func TestOwnerMissing(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset()
	b := New(c, backendParams())
	err := b.Apply(ctx, one(goldenSpecs["probe"]))
	if err == nil || !strings.Contains(err.Error(), "owner deployment printing/pagewire") || !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v", err)
	}
	assertNothingCreated(t, c)
	if _, err := c.AppsV1().Deployments(ns).Create(ctx, ownerDeployment(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.Apply(ctx, one(goldenSpecs["probe"])); err != nil {
		t.Fatal(err)
	}
	if got := image(t, deviceDeployment(t, c), "agent"); got != ownerImage {
		t.Errorf("agent image %s", got)
	}
}

func pod(name string, labels map[string]string, mutate func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}}
	if mutate != nil {
		mutate(p)
	}
	return p
}

var deviceLabels = map[string]string{NameLabel: DeviceName}

func TestObserve(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		objs []runtime.Object
		want device.Observation
	}{
		"absent": {nil, device.Observation{Phase: device.PhaseAbsent}},
		"image pull": {[]runtime.Object{pod("d", deviceLabels, func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Status.HostIP = "192.168.1.20"
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "printer-app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}}
		})}, device.Observation{Phase: device.PhasePending, HostIP: "192.168.1.20", Message: "printer-app: ImagePullBackOff"}},
		"unschedulable": {[]runtime.Object{pod("d", deviceLabels, func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Status.Conditions = []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
				Message: "0/3 nodes are available: 3 Insufficient devic.es/printer.",
			}}
		})}, device.Observation{Phase: device.PhasePending, Unschedulable: true, Message: "0/3 nodes are available: 3 Insufficient devic.es/printer."}},
		"running": {[]runtime.Object{pod("d", deviceLabels, func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodRunning
			p.Status.HostIP = "192.168.1.20"
		})}, device.Observation{Phase: device.PhaseRunning, HostIP: "192.168.1.20"}},
		"unrelated pod only": {[]runtime.Object{pod("x", map[string]string{NameLabel: "other"}, nil)}, device.Observation{Phase: device.PhaseAbsent}},
	}
	for name, tc := range cases {
		got, err := New(fake.NewClientset(tc.objs...), backendParams()).Observe(ctx)
		if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], tc.want) {
			t.Errorf("%s: got %+v, %v; want %+v", name, got, err, tc.want)
		}
	}
}

func TestRestartPod(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset(
		pod("dev-a", deviceLabels, nil),
		pod("dev-b", deviceLabels, nil),
		pod("other", map[string]string{NameLabel: "pagewire"}, nil),
	)
	if err := New(c, backendParams()).RestartPod(ctx, ""); err != nil {
		t.Fatal(err)
	}
	left, err := c.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(left.Items) != 1 || left.Items[0].Name != "other" {
		t.Errorf("pods left: %v", left.Items)
	}
	// Per-name deletes are recorded as "delete", DeleteCollection as "delete-collection".
	if n := count(c, "delete", "pods"); n != 2 {
		t.Errorf("%d delete actions", n)
	}
	if n := count(c, "delete-collection", "pods"); n != 0 {
		t.Errorf("%d delete-collection actions", n)
	}
}

func TestAgentTokenMissing(t *testing.T) {
	if _, err := New(fake.NewClientset(), backendParams()).AgentToken(context.Background()); err == nil {
		t.Error("want error without the Secret")
	}
}

func tokenSecret(tok string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: TokenSecretName, Namespace: ns},
		Data:       map[string][]byte{TokenSecretKey: []byte(tok)},
	}
}

func TestAgentTokenEmpty(t *testing.T) {
	if _, err := New(fake.NewClientset(tokenSecret("")), backendParams()).AgentToken(context.Background()); err == nil {
		t.Error("want error for an empty token")
	}
}

// An existing Secret without a token counts as missing and gets one; one with a
// token is kept.
func TestEnsureTokenFillsEmptySecret(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		secret *corev1.Secret
		keep   bool
	}{
		"empty key":   {tokenSecret(""), false},
		"no data":     {&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: TokenSecretName, Namespace: ns}}, false},
		"token there": {tokenSecret("kept"), true},
	} {
		c := fake.NewClientset(ownerDeployment(), tc.secret)
		b := New(c, backendParams())
		if err := b.Apply(ctx, one(goldenSpecs["probe"])); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s, err := c.CoreV1().Secrets(ns).Get(ctx, TokenSecretName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		tok := string(s.Data[TokenSecretKey])
		if tc.keep && tok != "kept" || !tc.keep && len(tok) != 64 {
			t.Errorf("%s: token %q", name, tok)
		}
		if got, err := b.AgentToken(ctx); err != nil || got != tok {
			t.Errorf("%s: AgentToken %q %v, want %q", name, got, err, tok)
		}
		if n := count(c, "create", "secrets"); n != 0 {
			t.Errorf("%s: %d plain creates", name, n)
		}
	}
}

// AgentToken reads the Secret at most once per tokenTTL, failures included.
func TestAgentTokenCache(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset()
	b := New(c, backendParams())
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }

	for range 3 {
		if _, err := b.AgentToken(ctx); err == nil {
			t.Fatal("want error without the Secret")
		}
	}
	if n := count(c, "get", "secrets"); n != 1 {
		t.Errorf("%d GETs for cached failures", n)
	}
	if _, err := c.CoreV1().Secrets(ns).Create(ctx, tokenSecret("one"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(tokenTTL)
	for range 3 {
		if tok, err := b.AgentToken(ctx); err != nil || tok != "one" {
			t.Fatalf("token %q %v", tok, err)
		}
	}
	if n := count(c, "get", "secrets"); n != 2 {
		t.Errorf("%d GETs, want 2", n)
	}
	if _, err := c.CoreV1().Secrets(ns).Update(ctx, tokenSecret("two"), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if tok, _ := b.AgentToken(ctx); tok != "one" {
		t.Errorf("re-read before the TTL: %q", tok)
	}
	now = now.Add(tokenTTL)
	if tok, _ := b.AgentToken(ctx); tok != "two" {
		t.Errorf("not re-read after the TTL: %q", tok)
	}
}

func usbNode(name string, units int64) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if units >= 0 {
		n.Status.Allocatable = corev1.ResourceList{"devic.es/printer": *resource.NewQuantity(units, resource.DecimalSI)}
	}
	return n
}

// forbidNodes makes nodes list answer 403 while *on, as without the
// pagewire-nodes ClusterRole; it returns the number of list calls seen.
func forbidNodes(c *fake.Clientset, on *bool) *int {
	n := 0
	c.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		n++
		if !*on {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("RBAC"))
	})
	return &n
}

func nodeSpec(node string) *device.Spec {
	return withHash(&device.Spec{
		Mode: device.ModeUSB, Node: node,
		Printers: []device.PrinterSpec{{ID: "p-" + node, Family: "ipp-usb", USB: keyHP}},
	})
}

func slotPod(name string, slot int, phase corev1.PodPhase) *corev1.Pod {
	return pod(name, map[string]string{NameLabel: SlotName(slot)}, func(p *corev1.Pod) { p.Status.Phase = phase })
}

func workloads(obs []device.Observation) []string {
	var out []string
	for _, o := range obs {
		out = append(out, o.Workload+"="+o.Phase)
	}
	return out
}

func getDeployment(t *testing.T, c *fake.Clientset, name string) *appsv1.Deployment {
	t.Helper()
	d, err := c.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("deployment %s: %v", name, err)
	}
	return d
}

func pinnedTo(d *appsv1.Deployment) string {
	a := d.Spec.Template.Spec.Affinity
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return ""
	}
	return a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields[0].Values[0]
}

// singleNodeColdStart is a control-plane cold start without the nodes
// ClusterRole: the node list answers 403, then Observe, Apply, Observe and
// RestartPod run in single-node mode. It returns the client with its call log.
func singleNodeColdStart(t *testing.T) *fake.Clientset {
	t.Helper()
	ctx := context.Background()
	c := fake.NewClientset(ownerDeployment(), slotPod("dev-a", 0, corev1.PodRunning))
	on := true
	lists := forbidNodes(c, &on)
	b := New(c, backendParams())
	if nodes, err := b.USBNodes(ctx); !apierrors.IsForbidden(err) || nodes != nil {
		t.Fatalf("USBNodes = %v, %v; want a 403", nodes, err)
	}
	obs, err := b.Observe(ctx)
	if err != nil || !slices.Equal(workloads(obs), []string{"=running"}) {
		t.Fatalf("Observe = %v, %v", workloads(obs), err)
	}
	if err := b.Apply(ctx, one(goldenSpecs["ippusb"])); err != nil {
		t.Fatal(err)
	}
	if _, err := b.USBNodes(ctx); err == nil {
		t.Fatal("cached node-list failure not returned")
	}
	if _, err := b.Observe(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.RestartPod(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if *lists != 1 {
		t.Errorf("%d node lists, want 1 within the backoff", *lists)
	}
	return c
}

// TestSingleNodeColdStart: without the ClusterRole nothing names slots 1-3
// and only the owner and slot 0 are read, so today's Role keeps working.
func TestSingleNodeColdStart(t *testing.T) {
	c := singleNodeColdStart(t)
	for _, a := range c.Actions() {
		name := ""
		if n, ok := a.(interface{ GetName() string }); ok {
			name = n.GetName()
		}
		sel := ""
		if l, ok := a.(k8stesting.ListAction); ok {
			sel = l.GetListRestrictions().Labels.String()
		}
		for i := 1; i < MaxSlots; i++ {
			if name == SlotName(i) || strings.Contains(sel, SlotName(i)) {
				t.Errorf("single-node call names slot %d: %v", i, a)
			}
		}
		if a.GetVerb() == "get" && a.GetResource().Resource == "deployments" && name != "pagewire" && name != DeviceName {
			t.Errorf("deployment get of %s", name)
		}
	}
	d := getDeployment(t, c, DeviceName)
	if pinnedTo(d) != "" || d.Annotations[NodeAnnotation] != "" {
		t.Errorf("single-node workload pinned: %q %v", pinnedTo(d), d.Annotations)
	}
}

func TestMultiNode(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset(ownerDeployment(), usbNode("n2", 8), usbNode("n1", 3), usbNode("n3", 0), usbNode("cp", -1))
	b := New(c, backendParams())
	nodes, err := b.USBNodes(ctx)
	if err != nil || !slices.Equal(nodes, []string{"n1", "n2"}) {
		t.Fatalf("USBNodes = %v, %v", nodes, err)
	}
	if err := b.Apply(ctx, []*device.Spec{nodeSpec("n2"), nodeSpec("n1")}); err != nil {
		t.Fatal(err)
	}
	for slot, node := range []string{"n1", "n2"} {
		d := getDeployment(t, c, SlotName(slot))
		if pinnedTo(d) != node || d.Annotations[NodeAnnotation] != node {
			t.Errorf("slot %d pinned %q annotated %q, want %s", slot, pinnedTo(d), d.Annotations[NodeAnnotation], node)
		}
		if _, err := c.CoreV1().ConfigMaps(ns).Get(ctx, SlotName(slot), metav1.GetOptions{}); err != nil {
			t.Errorf("configmap %s: %v", SlotName(slot), err)
		}
		// Only the lowest slot mounts the blob volume, when no printer needs a EULA.
		hasBlobs := slices.ContainsFunc(d.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == "blobs" })
		if hasBlobs != (slot == 0) {
			t.Errorf("slot %d blobs volume %v", slot, hasBlobs)
		}
	}
	for _, p := range []*corev1.Pod{slotPod("a", 0, corev1.PodRunning), slotPod("b", 1, corev1.PodPending), slotPod("stray", 2, corev1.PodRunning)} {
		if _, err := c.CoreV1().Pods(ns).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	obs, err := b.Observe(ctx)
	if err != nil || !slices.Equal(workloads(obs), []string{"n1=running", "n2=pending"}) {
		t.Errorf("Observe = %v, %v", workloads(obs), err)
	}
}

// TestBlobWorkloadFollowsEULA: the workload whose printers offer a EULA gets
// the blob volume, even in a higher slot.
func TestBlobWorkloadFollowsEULA(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset(ownerDeployment(), usbNode("n1", 8), usbNode("n2", 8))
	b := New(c, backendParams())
	if _, err := b.USBNodes(ctx); err != nil {
		t.Fatal(err)
	}
	hp := withHash(&device.Spec{Mode: device.ModeUSB, Node: "n2", Printers: []device.PrinterSpec{{ID: "hp", Family: "hplip", USB: keyHP}}})
	if err := b.Apply(ctx, []*device.Spec{nodeSpec("n1"), hp}); err != nil {
		t.Fatal(err)
	}
	for slot, want := range []bool{false, true} {
		a := find(t, getDeployment(t, c, SlotName(slot)), "agent")
		if got := slices.Contains(a.Args, "--blob-dir"); got != want {
			t.Errorf("slot %d --blob-dir %v, want %v", slot, got, want)
		}
	}
	// Two workloads needing blobs: the second is refused, the rest applied.
	blob := func(node string) *device.Spec {
		s := *goldenSpecs["hplip-airsane-blob"]
		s.Node = node
		return withHash(&s)
	}
	if err := b.Apply(ctx, []*device.Spec{blob("n1"), blob("n2")}); err == nil || !strings.Contains(err.Error(), SlotName(1)) {
		t.Errorf("two blob workloads: %v", err)
	}
}

// TestSlotRecovery: a new backend (control-plane restart) reads the slots
// from the Deployments' node annotations before any Apply, and keeps them.
func TestSlotRecovery(t *testing.T) {
	ctx := context.Background()
	annotated := func(slot int, node string) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: SlotName(slot), Namespace: ns, Annotations: map[string]string{NodeAnnotation: node},
		}}
	}
	c := fake.NewClientset(ownerDeployment(), usbNode("n1", 8), usbNode("n2", 8),
		annotated(0, "n2"), annotated(1, "n1"),
		slotPod("a", 0, corev1.PodRunning), slotPod("b", 1, corev1.PodRunning))
	b := New(c, backendParams())
	if _, err := b.USBNodes(ctx); err != nil {
		t.Fatal(err)
	}
	obs, err := b.Observe(ctx)
	if err != nil || !slices.Equal(workloads(obs), []string{"n2=running", "n1=running"}) {
		t.Fatalf("Observe = %v, %v", workloads(obs), err)
	}
	if err := b.Apply(ctx, []*device.Spec{nodeSpec("n1"), nodeSpec("n2")}); err != nil {
		t.Fatal(err)
	}
	gets := 0
	for _, a := range c.Actions() {
		if g, ok := a.(k8stesting.GetAction); ok && a.GetVerb() == "get" && a.GetResource().Resource == "deployments" && g.GetName() != "pagewire" {
			gets++
		}
	}
	if gets != MaxSlots {
		t.Errorf("%d slot gets, want %d (recovered once)", gets, MaxSlots)
	}
	if got := pinnedTo(getDeployment(t, c, SlotName(0))); got != "n2" {
		t.Errorf("slot 0 pinned to %q after recovery, want n2", got)
	}
}

// TestAdoptUnpinnedSlot0: the single-node workload is observed as workload ""
// until the first node takes its slot.
func TestAdoptUnpinnedSlot0(t *testing.T) {
	ctx := context.Background()
	old := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: DeviceName, Namespace: ns}}
	c := fake.NewClientset(ownerDeployment(), usbNode("n1", 8), old, slotPod("a", 0, corev1.PodRunning))
	b := New(c, backendParams())
	if _, err := b.USBNodes(ctx); err != nil {
		t.Fatal(err)
	}
	obs, err := b.Observe(ctx)
	if err != nil || !slices.Equal(workloads(obs), []string{"=running"}) {
		t.Fatalf("Observe = %v, %v", workloads(obs), err)
	}
	if err := b.Apply(ctx, one(nodeSpec("n1"))); err != nil {
		t.Fatal(err)
	}
	if got := pinnedTo(getDeployment(t, c, DeviceName)); got != "n1" {
		t.Errorf("slot 0 pinned to %q", got)
	}
	obs, _ = b.Observe(ctx)
	if !slices.Equal(workloads(obs), []string{"n1=running"}) {
		t.Errorf("Observe after adoption = %v", workloads(obs))
	}
}

func TestDeleteUnusedSlots(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset(ownerDeployment(), usbNode("n1", 8), usbNode("n2", 8), usbNode("n3", 8))
	b := New(c, backendParams())
	if _, err := b.USBNodes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Apply(ctx, []*device.Spec{nodeSpec("n1"), nodeSpec("n2")}); err != nil {
		t.Fatal(err)
	}
	if err := b.Apply(ctx, one(nodeSpec("n2"))); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AppsV1().Deployments(ns).Get(ctx, DeviceName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("unused slot 0 deployment: %v", err)
	}
	if _, err := c.CoreV1().ConfigMaps(ns).Get(ctx, DeviceName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("unused slot 0 configmap: %v", err)
	}
	if got := pinnedTo(getDeployment(t, c, SlotName(1))); got != "n2" {
		t.Errorf("slot 1 pinned to %q", got)
	}
	// A new node takes the lowest free slot.
	if err := b.Apply(ctx, []*device.Spec{nodeSpec("n3"), nodeSpec("n2")}); err != nil {
		t.Fatal(err)
	}
	if got := pinnedTo(getDeployment(t, c, DeviceName)); got != "n3" {
		t.Errorf("slot 0 pinned to %q, want n3", got)
	}
	// No specs remove every workload.
	if err := b.Apply(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := c.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{}); len(l.Items) != 1 {
		t.Errorf("deployments left: %d, want the owner only", len(l.Items))
	}
}

func TestRestartPodScoped(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset(ownerDeployment(), usbNode("n1", 8), usbNode("n2", 8),
		slotPod("a", 0, corev1.PodRunning), slotPod("b", 1, corev1.PodRunning))
	b := New(c, backendParams())
	if _, err := b.USBNodes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Apply(ctx, []*device.Spec{nodeSpec("n1"), nodeSpec("n2")}); err != nil {
		t.Fatal(err)
	}
	if err := b.RestartPod(ctx, "n2"); err != nil {
		t.Fatal(err)
	}
	left, _ := c.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if len(left.Items) != 1 || left.Items[0].Name != "a" {
		t.Errorf("pods left %v", left.Items)
	}
	if err := b.RestartPod(ctx, "n9"); err == nil {
		t.Error("restart of an unknown node succeeded")
	}
}

func TestTooManyNodes(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset(ownerDeployment())
	b := New(c, backendParams())
	var specs []*device.Spec
	for i := range MaxSlots + 1 {
		n := "n" + strconv.Itoa(i+1)
		if _, err := c.CoreV1().Nodes().Create(ctx, usbNode(n, 8), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		specs = append(specs, nodeSpec(n))
	}
	if _, err := b.USBNodes(ctx); err != nil {
		t.Fatal(err)
	}
	err := b.Apply(ctx, specs)
	if err == nil || !strings.Contains(err.Error(), "n5") || strings.Contains(err.Error(), "n4") {
		t.Fatalf("Apply = %v, want an error naming n5 only", err)
	}
	for i := range MaxSlots {
		if got := pinnedTo(getDeployment(t, c, SlotName(i))); got != "n"+strconv.Itoa(i+1) {
			t.Errorf("slot %d pinned to %q", i, got)
		}
	}
}

// TestNodeListBackoff: a failed node list is retried after nodeBackoff, not
// on every call; a later success switches to multi-node mode.
func TestNodeListBackoff(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset(ownerDeployment(), usbNode("n1", 8))
	on := true
	lists := forbidNodes(c, &on)
	b := New(c, backendParams())
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }
	for range 3 {
		if _, err := b.USBNodes(ctx); err == nil {
			t.Fatal("want the 403")
		}
	}
	if *lists != 1 {
		t.Errorf("%d lists within the backoff", *lists)
	}
	now = now.Add(nodeBackoff - time.Second)
	_, _ = b.USBNodes(ctx)
	if *lists != 1 {
		t.Errorf("%d lists before the backoff ends", *lists)
	}
	on = false
	now = now.Add(time.Second)
	nodes, err := b.USBNodes(ctx)
	if err != nil || !slices.Equal(nodes, []string{"n1"}) || *lists != 2 {
		t.Fatalf("after backoff: %v, %v, %d lists", nodes, err, *lists)
	}
	if err := b.Apply(ctx, one(nodeSpec("n1"))); err != nil {
		t.Fatalf("multi-node Apply: %v", err)
	}
	// Single-node mode refuses node-pinned specs.
	on = true
	now = now.Add(nodeBackoff)
	if _, err := b.USBNodes(ctx); err == nil {
		t.Fatal("want the 403")
	}
	if err := b.Apply(ctx, one(nodeSpec("n1"))); err == nil {
		t.Error("single-node Apply of a pinned spec succeeded")
	}
}

// TestNodeListTransientError: a node-list error other than 403 (an API-server
// blip) keeps multi-node mode and the slot map, is not cached, and is not the
// single-node error; slot 0 stays pinned. A 403 is the single-node error.
func TestNodeListTransientError(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset(ownerDeployment(), usbNode("n1", 8), usbNode("n2", 8))
	b := New(c, backendParams())
	if _, err := b.USBNodes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Apply(ctx, []*device.Spec{nodeSpec("n1"), nodeSpec("n2")}); err != nil {
		t.Fatal(err)
	}
	fail, lists := true, 0
	c.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists++
		if !fail {
			return false, nil, nil
		}
		return true, nil, apierrors.NewInternalError(errors.New("etcd timeout"))
	})
	_, err := b.USBNodes(ctx)
	if err == nil || errors.Is(err, device.ErrSingleNode) {
		t.Fatalf("USBNodes = %v, want a transient error", err)
	}
	if _, err := b.USBNodes(ctx); err == nil || lists != 2 {
		t.Errorf("transient error cached: %v after %d lists", err, lists)
	}
	obs, err := b.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if o.Workload == "" {
			t.Errorf("Observe fell back to single-node: %v", workloads(obs))
		}
	}
	if got := pinnedTo(getDeployment(t, c, SlotName(0))); got != "n1" {
		t.Errorf("slot 0 pinned to %q, want n1", got)
	}
	fail = false
	if nodes, err := b.USBNodes(ctx); err != nil || !slices.Equal(nodes, []string{"n1", "n2"}) {
		t.Fatalf("after the blip: %v, %v", nodes, err)
	}

	on := true
	forbidNodes(c, &on)
	if _, err := b.USBNodes(ctx); !errors.Is(err, device.ErrSingleNode) || !apierrors.IsForbidden(err) {
		t.Errorf("403: %v, want the single-node error wrapping it", err)
	}
}

// TestApplyNamesFailedWorkload: one slot's failure is a device.WorkloadErrors
// naming only that slot's node; the other slot is applied.
func TestApplyNamesFailedWorkload(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset(ownerDeployment(), usbNode("n1", 8), usbNode("n2", 8))
	b := New(c, backendParams())
	if _, err := b.USBNodes(ctx); err != nil {
		t.Fatal(err)
	}
	c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.PatchAction).GetName() == SlotName(1) {
			return true, nil, errors.New("denied by admission")
		}
		return false, nil, nil
	})
	err := b.Apply(ctx, []*device.Spec{nodeSpec("n1"), nodeSpec("n2")})
	var we device.WorkloadErrors
	if !errors.As(err, &we) || len(we) != 1 || we["n2"] == nil || !strings.Contains(we["n2"].Error(), "denied by admission") {
		t.Fatalf("Apply = %#v, want a WorkloadErrors for n2 only", err)
	}
	if got := pinnedTo(getDeployment(t, c, SlotName(0))); got != "n1" {
		t.Errorf("slot 0 pinned to %q, want n1 applied", got)
	}
}

// TestApplyNamesFailedFreedSlot: a freed slot whose delete keeps failing is
// named after the node it held on every retry, not only the first.
func TestApplyNamesFailedFreedSlot(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset(ownerDeployment(), usbNode("n1", 8), usbNode("n2", 8))
	b := New(c, backendParams())
	if _, err := b.USBNodes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Apply(ctx, []*device.Spec{nodeSpec("n1"), nodeSpec("n2")}); err != nil {
		t.Fatal(err)
	}
	c.PrependReactor("delete", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.DeleteAction).GetName() == SlotName(1) {
			return true, nil, errors.New("denied by admission")
		}
		return false, nil, nil
	})
	for try := range 2 {
		err := b.Apply(ctx, []*device.Spec{nodeSpec("n1")})
		var we device.WorkloadErrors
		if !errors.As(err, &we) || len(we) != 1 || we["n2"] == nil {
			t.Fatalf("try %d: Apply = %#v, want a WorkloadErrors for n2 only", try, err)
		}
	}
}
