package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	if err := b.Apply(ctx, spec); err != nil {
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
	if err := b.Apply(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := b.Apply(ctx, goldenSpecs["hplip-airsane-blob"]); err != nil {
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
	err := New(c, p).Apply(context.Background(), goldenSpecs["ippusb"])
	if !errors.Is(err, ErrNoUSBResource) {
		t.Fatalf("err = %v", err)
	}
	assertNothingCreated(t, c)
}

func TestOwnerFallback(t *testing.T) {
	ctx := context.Background()
	other := corev1.Container{Name: "sidecar", Image: "busybox:1"}

	c := fake.NewClientset(ownerDeployment(other))
	if err := New(c, backendParams()).Apply(ctx, goldenSpecs["probe"]); err != nil {
		t.Fatal(err)
	}
	if got := image(t, deviceDeployment(t, c), "agent"); got != "ghcr.io/thephaseless/pagewire:1.2.3" {
		t.Errorf("fallback image %s", got)
	}

	p := backendParams()
	p.Overrides = map[string]string{"pagewire": "localhost:5000/ps:x"}
	c = fake.NewClientset(ownerDeployment(other))
	if err := New(c, p).Apply(ctx, goldenSpecs["probe"]); err != nil {
		t.Fatal(err)
	}
	if got := image(t, deviceDeployment(t, c), "agent"); got != "localhost:5000/ps:x" {
		t.Errorf("override fallback image %s", got)
	}

	// The owner's pagewire container beats the override.
	c = fake.NewClientset(ownerDeployment())
	if err := New(c, p).Apply(ctx, goldenSpecs["probe"]); err != nil {
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
	err := b.Apply(ctx, goldenSpecs["probe"])
	if err == nil || !strings.Contains(err.Error(), "owner deployment printing/pagewire") || !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v", err)
	}
	assertNothingCreated(t, c)
	if _, err := c.AppsV1().Deployments(ns).Create(ctx, ownerDeployment(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.Apply(ctx, goldenSpecs["probe"]); err != nil {
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
		if err != nil || got != tc.want {
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
	if err := New(c, backendParams()).RestartPod(ctx); err != nil {
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
		if err := b.Apply(ctx, goldenSpecs["probe"]); err != nil {
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
