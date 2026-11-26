package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ThePhaseless/pagewire/internal/device"
)

type roleRule struct {
	APIGroups     []string `yaml:"apiGroups"`
	Resources     []string `yaml:"resources"`
	ResourceNames []string `yaml:"resourceNames"`
	Verbs         []string `yaml:"verbs"`
}

func parseRules(t *testing.T, raw []byte) []roleRule {
	t.Helper()
	var role struct {
		Rules []roleRule `yaml:"rules"`
	}
	if err := yaml.Unmarshal(raw, &role); err != nil {
		t.Fatal(err)
	}
	return role.Rules
}

func loadRules(t *testing.T, file string) []roleRule {
	t.Helper()
	raw, err := os.ReadFile("../../../deploy/base/" + file) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	return parseRules(t, raw)
}

// oldRole is deploy/base/role.yaml at 6d82547 (unchanged up to e92f01d): the
// Role of installs that predate multi-node support. Single-node mode must
// keep working under it, with no ClusterRole.
const oldRole = `apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: pagewire
  namespace: printing
# Every rule names its objects except pods. The "pagewire" resourceName is the owner
# Deployment: if you change PAGEWIRE_K8S_OWNER, change it here too (two rules).
# internal/backend/k8s/policy_test.go checks every client call against this file.
rules:
  # Owner lookup of this control plane's own Deployment (PAGEWIRE_K8S_OWNER), and
  # reads of the device Deployment.
  - apiGroups: [apps]
    resources: [deployments]
    resourceNames: [pagewire, pagewire-device]
    verbs: [get]
  # The device Deployment. Server-side apply is a PATCH by name that may create
  # the object, so it needs both patch and create on that name.
  - apiGroups: [apps]
    resources: [deployments]
    resourceNames: [pagewire-device]
    verbs: [create, patch, delete]
  # The device Deployment, ConfigMap and token Secret set blockOwnerDeletion in their
  # ownerReference to the owner Deployment, which needs update on its finalizers.
  - apiGroups: [apps]
    resources: [deployments/finalizers]
    resourceNames: [pagewire]
    verbs: [update]
  # The device ConfigMap (ipp-usb, SANE and AirSane settings), server-side applied.
  - apiGroups: [""]
    resources: [configmaps]
    resourceNames: [pagewire-device]
    verbs: [get, create, patch, delete]
  # The agent token Secret, server-side applied by name.
  - apiGroups: [""]
    resources: [secrets]
    resourceNames: [pagewire-agent-token]
    verbs: [get, create, patch]
  # Device pods get generated names, so these cannot be scoped by resourceNames.
  # Observe and RestartPod list them by label; RestartPod deletes them one by one by
  # name, so neither deletecollection nor get or watch is granted.
  - apiGroups: [""]
    resources: [pods]
    verbs: [list, delete]
`

// allowed reports whether a rule grants verb on group/resource for name. An
// empty name (create by POST, list) needs a rule without resourceNames.
func allowed(rules []roleRule, group, resource, verb, name string) bool {
	for _, r := range rules {
		if slices.Contains(r.APIGroups, group) && slices.Contains(r.Resources, resource) && slices.Contains(r.Verbs, verb) &&
			(len(r.ResourceNames) == 0 || name != "" && slices.Contains(r.ResourceNames, name)) {
			return true
		}
	}
	return false
}

type access struct{ verb, name string }

// accesses maps a recorded action to the RBAC checks the API server makes. A
// server-side apply may create the object, so it needs patch and create on its
// name. Create by POST, list and watch carry no name the authorizer could match.
func accesses(a k8stesting.Action) []access {
	name := ""
	if n, ok := a.(interface{ GetName() string }); ok {
		name = n.GetName()
	}
	switch a.GetVerb() {
	case "create", "list", "watch", "delete-collection":
		name = ""
	case "patch":
		if p, ok := a.(k8stesting.PatchAction); ok && p.GetPatchType() == types.ApplyPatchType {
			return []access{{"patch", name}, {"create", name}}
		}
	}
	return []access{{a.GetVerb(), name}}
}

// checkCalls checks every recorded call: cluster-scoped nodes against the
// ClusterRole's rules, the rest against the Role's.
func checkCalls(t *testing.T, label string, c *fake.Clientset, role, cluster []roleRule, skip func(k8stesting.Action) bool) {
	t.Helper()
	if len(c.Actions()) == 0 {
		t.Fatalf("%s: no actions recorded", label)
	}
	for _, a := range c.Actions() {
		if skip != nil && skip(a) {
			continue
		}
		res := a.GetResource()
		resource := res.Resource
		if sub := a.GetSubresource(); sub != "" {
			resource += "/" + sub
		}
		rules := role
		if resource == "nodes" {
			rules = cluster
		}
		for _, acc := range accesses(a) {
			if !allowed(rules, res.Group, resource, acc.verb, acc.name) {
				t.Errorf("%s: RBAC does not allow %s %s/%s name %q", label, acc.verb, res.Group, resource, acc.name)
			}
		}
	}
}

// TestRoleAllowsEveryCall runs every Backend method, Notify included, against a
// fake clientset and checks each recorded call against deploy/base/role.yaml
// and clusterrole.yaml, resourceNames included, in single-node and multi-node
// mode.
func TestRoleAllowsEveryCall(t *testing.T) {
	ctx := context.Background()
	rules, cluster := loadRules(t, "role.yaml"), loadRules(t, "clusterrole.yaml")
	empty := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: TokenSecretName, Namespace: ns}}
	for name, c := range map[string]*fake.Clientset{
		"no secret":    fake.NewClientset(ownerDeployment(), pod("dev-a", deviceLabels, nil)),
		"empty secret": fake.NewClientset(ownerDeployment(), pod("dev-a", deviceLabels, nil), empty),
	} {
		b := New(c, backendParams())
		for _, spec := range []string{"ippusb", "hplip-airsane-blob"} {
			if err := b.Apply(ctx, one(goldenSpecs[spec])); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := b.AgentToken(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Observe(ctx); err != nil {
			t.Fatal(err)
		}
		if err := b.RestartPod(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if err := b.Apply(ctx, nil); err != nil {
			t.Fatal(err)
		}
		driveNotify(t, b, c, "pods")
		checkCalls(t, name, c, rules, cluster, nil)
	}

	// Multi-node: four nodes fill every slot, a new backend recovers them,
	// then unused slots are deleted.
	c := fake.NewClientset(ownerDeployment(), pod("dev-a", deviceLabels, nil))
	var specs []*device.Spec
	for i := range MaxSlots {
		n := "n" + strconv.Itoa(i)
		if _, err := c.CoreV1().Nodes().Create(ctx, usbNode(n, 8), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		specs = append(specs, nodeSpec(n))
	}
	c.ClearActions()
	for _, b := range []*Backend{New(c, backendParams()), New(c, backendParams())} {
		if _, err := b.USBNodes(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Observe(ctx); err != nil {
			t.Fatal(err)
		}
		if err := b.Apply(ctx, specs); err != nil {
			t.Fatal(err)
		}
		if err := b.RestartPod(ctx, "n1"); err != nil {
			t.Fatal(err)
		}
	}
	b := New(c, backendParams())
	if _, err := b.USBNodes(ctx); err != nil {
		t.Fatal(err)
	}
	driveNotify(t, b, c, "pods", "nodes")
	if err := b.Apply(ctx, specs[:1]); err != nil {
		t.Fatal(err)
	}
	if err := b.Apply(ctx, nil); err != nil {
		t.Fatal(err)
	}
	checkCalls(t, "multi-node", c, rules, cluster, nil)
	named := map[string]bool{}
	for _, a := range c.Actions() {
		if n, ok := a.(interface{ GetName() string }); ok && a.GetResource().Resource == "deployments" {
			named[a.GetVerb()+" "+n.GetName()] = true
		}
	}
	for i := range MaxSlots {
		for _, verb := range []string{"get", "patch", "delete"} {
			if !named[verb+" "+SlotName(i)] {
				t.Errorf("multi-node run never made %s %s", verb, SlotName(i))
			}
		}
	}

	// No client call records the finalizers update: the API server checks it
	// because the ownerReferences set blockOwnerDeletion on the owner.
	owner := backendParams().Owner.Name
	if !allowed(rules, "apps", "deployments/finalizers", "update", owner) {
		t.Errorf("role.yaml lacks update on deployments/finalizers for %q", owner)
	}
	for _, r := range rules {
		if slices.Contains(r.Resources, "deployments/finalizers") && !slices.Equal(r.ResourceNames, []string{owner}) {
			t.Errorf("deployments/finalizers rule not scoped to the owner: %+v", r)
		}
	}
}

// driveNotify runs b.Notify until it has watched every resource, then stops it.
func driveNotify(t *testing.T, b *Backend, c *fake.Clientset, resources ...string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Notify(ctx, func() {}, func(string) {})
	}()
	for _, r := range resources {
		waitFor(t, "watch "+r, func() bool { return watched(c, r) })
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Notify did not return after ctx end")
	}
}

// TestOldRoleAllowsSingleNode: an install with the pre-multi-node Role and
// no ClusterRole keeps working; only the refused node list falls outside it.
// It does not drive Notify: the pods watch is new, the pre-slot Role refuses
// it, and notify_test.go covers the 403 degrade.
func TestOldRoleAllowsSingleNode(t *testing.T) {
	c := singleNodeColdStart(t)
	nodesList := func(a k8stesting.Action) bool { return a.GetVerb() == "list" && a.GetResource().Resource == "nodes" }
	checkCalls(t, "old role", c, parseRules(t, []byte(oldRole)), nil, nodesList)
}

// TestRoleNamesEverySlot: every rule naming slot 0 names all slots, so a node
// taking slot 1-3 never meets a 403.
func TestRoleNamesEverySlot(t *testing.T) {
	var slots []string
	for i := range MaxSlots {
		slots = append(slots, SlotName(i))
	}
	n := 0
	for _, r := range loadRules(t, "role.yaml") {
		if !slices.Contains(r.ResourceNames, DeviceName) {
			continue
		}
		n++
		for _, s := range slots {
			if !slices.Contains(r.ResourceNames, s) {
				t.Errorf("rule %+v lacks %s", r, s)
			}
		}
	}
	if n != 3 {
		t.Errorf("%d rules name %s, want 3 (deployments get, deployments write, configmaps)", n, DeviceName)
	}
	cluster := loadRules(t, "clusterrole.yaml")
	if len(cluster) != 1 || !slices.Equal(cluster[0].Resources, []string{"nodes"}) || !slices.Equal(cluster[0].Verbs, []string{"list", "watch"}) {
		t.Errorf("clusterrole rules %+v, want list and watch on nodes only", cluster)
	}
}

// The messages of deploy/examples/admission-policy.yaml's validations, in
// file order. policyViolations mirrors each rule's CEL; cel-go is not a
// dependency, so TestAdmissionPolicyMessages ties this list to the YAML.
const (
	msgHostPID        = "hostPID is not allowed"
	msgHostIPC        = "hostIPC is not allowed"
	msgHostPath       = "hostPath volumes are not allowed"
	msgPrivileged     = "privileged containers are not allowed"
	msgEscalation     = "allowPrivilegeEscalation must not be true"
	msgAddedCaps      = "added capabilities are not allowed"
	msgDropAll        = "containers must drop ALL capabilities"
	msgSeccompUnconf  = "Unconfined seccomp profiles are not allowed"
	msgAppArmorUnconf = "Unconfined AppArmor profiles are not allowed"
)

var policyMessages = []string{msgHostPID, msgHostIPC, msgHostPath, msgPrivileged, msgEscalation, msgAddedCaps, msgDropAll, msgSeccompUnconf, msgAppArmorUnconf}

// policyViolations returns the admission policy's message for every rule spec
// breaks, once each, in policyMessages order. hostNetwork is allowed.
func policyViolations(spec corev1.PodSpec) []string {
	bad := map[string]bool{msgHostPID: spec.HostPID, msgHostIPC: spec.HostIPC}
	for _, v := range spec.Volumes {
		bad[msgHostPath] = bad[msgHostPath] || v.HostPath != nil
	}
	if sc := spec.SecurityContext; sc != nil {
		bad[msgSeccompUnconf] = sc.SeccompProfile != nil && sc.SeccompProfile.Type == corev1.SeccompProfileTypeUnconfined
		bad[msgAppArmorUnconf] = sc.AppArmorProfile != nil && sc.AppArmorProfile.Type == corev1.AppArmorProfileTypeUnconfined
	}
	var scs []*corev1.SecurityContext
	for _, c := range spec.Containers {
		scs = append(scs, c.SecurityContext)
	}
	for _, c := range spec.InitContainers {
		scs = append(scs, c.SecurityContext)
	}
	for _, c := range spec.EphemeralContainers {
		scs = append(scs, c.SecurityContext)
	}
	for _, sc := range scs {
		if sc == nil {
			bad[msgDropAll] = true
			continue
		}
		bad[msgPrivileged] = bad[msgPrivileged] || sc.Privileged != nil && *sc.Privileged
		bad[msgEscalation] = bad[msgEscalation] || sc.AllowPrivilegeEscalation != nil && *sc.AllowPrivilegeEscalation
		caps := sc.Capabilities
		bad[msgAddedCaps] = bad[msgAddedCaps] || caps != nil && len(caps.Add) > 0
		bad[msgDropAll] = bad[msgDropAll] || caps == nil || !slices.Contains(caps.Drop, "ALL")
		bad[msgSeccompUnconf] = bad[msgSeccompUnconf] || sc.SeccompProfile != nil && sc.SeccompProfile.Type == corev1.SeccompProfileTypeUnconfined
		bad[msgAppArmorUnconf] = bad[msgAppArmorUnconf] || sc.AppArmorProfile != nil && sc.AppArmorProfile.Type == corev1.AppArmorProfileTypeUnconfined
	}
	var out []string
	for _, m := range policyMessages {
		if bad[m] {
			out = append(out, m)
		}
	}
	return out
}

func loadDeployment(t *testing.T, path string) appsv1.Deployment {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	js, err := yaml.YAMLToJSON(raw)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var d appsv1.Deployment
	if err := json.Unmarshal(js, &d); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return d
}

// TestGoldensPassAdmissionPolicy checks every rendered pod template, and the
// control plane's own Deployment in the same namespace, against
// deploy/examples/admission-policy.yaml.
func TestGoldensPassAdmissionPolicy(t *testing.T) {
	files, err := filepath.Glob("testdata/*.golden.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("goldens %v %v", files, err)
	}
	checked := 0
	for _, f := range files {
		d := loadDeployment(t, f)
		if d.Kind != "Deployment" {
			continue
		}
		checked++
		if v := policyViolations(d.Spec.Template.Spec); len(v) > 0 {
			t.Errorf("%s: %v", f, v)
		}
		if !d.Spec.Template.Spec.HostNetwork {
			t.Errorf("%s: device pod without hostNetwork", f)
		}
	}
	if checked == 0 {
		t.Fatal("no golden holds a pod template")
	}
	base := "../../../deploy/base/deployment.yaml"
	d := loadDeployment(t, base)
	if len(d.Spec.Template.Spec.Containers) == 0 {
		t.Fatalf("%s: no containers", base)
	}
	if v := policyViolations(d.Spec.Template.Spec); len(v) > 0 {
		t.Errorf("%s: %v", base, v)
	}
}

func TestAdmissionPolicyRejects(t *testing.T) {
	dropAll := func() *corev1.SecurityContext {
		return &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	}
	clean := func() corev1.PodSpec {
		return corev1.PodSpec{HostNetwork: true, Containers: []corev1.Container{{Name: "a", SecurityContext: dropAll()}}}
	}
	yes := true
	for _, tc := range []struct {
		name string
		edit func(*corev1.PodSpec)
		want string
	}{
		{"clean", func(*corev1.PodSpec) {}, ""},
		{"hostPID", func(s *corev1.PodSpec) { s.HostPID = true }, msgHostPID},
		{"hostIPC", func(s *corev1.PodSpec) { s.HostIPC = true }, msgHostIPC},
		{"hostPath", func(s *corev1.PodSpec) {
			s.Volumes = []corev1.Volume{{Name: "sys", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/sys"}}}}
		}, msgHostPath},
		{"privileged", func(s *corev1.PodSpec) { s.Containers[0].SecurityContext.Privileged = &yes }, msgPrivileged},
		{"escalation", func(s *corev1.PodSpec) { s.Containers[0].SecurityContext.AllowPrivilegeEscalation = &yes }, msgEscalation},
		{"added capability", func(s *corev1.PodSpec) {
			s.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
		}, msgAddedCaps},
		{"drop NET_RAW only", func(s *corev1.PodSpec) {
			s.Containers[0].SecurityContext.Capabilities.Drop = []corev1.Capability{"NET_RAW"}
		}, msgDropAll},
		{"no capabilities", func(s *corev1.PodSpec) { s.Containers[0].SecurityContext = &corev1.SecurityContext{} }, msgDropAll},
		{"init container without drop ALL", func(s *corev1.PodSpec) {
			s.InitContainers = []corev1.Container{{Name: "init", SecurityContext: &corev1.SecurityContext{}}}
		}, msgDropAll},
		{"ephemeral container without securityContext", func(s *corev1.PodSpec) {
			s.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug"}}}
		}, msgDropAll},
		{"pod seccomp Unconfined", func(s *corev1.PodSpec) {
			s.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}
		}, msgSeccompUnconf},
		{"container seccomp Unconfined", func(s *corev1.PodSpec) {
			s.Containers[0].SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
		}, msgSeccompUnconf},
		{"pod AppArmor Unconfined", func(s *corev1.PodSpec) {
			s.SecurityContext = &corev1.PodSecurityContext{AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}}
		}, msgAppArmorUnconf},
		{"container AppArmor Unconfined", func(s *corev1.PodSpec) {
			s.Containers[0].SecurityContext.AppArmorProfile = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}
		}, msgAppArmorUnconf},
		{"RuntimeDefault seccomp", func(s *corev1.PodSpec) {
			s.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := clean()
			tc.edit(&s)
			var want []string
			if tc.want != "" {
				want = []string{tc.want}
			}
			if got := policyViolations(s); !slices.Equal(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

// TestAdmissionPolicyMessages fails when the YAML gains a rule the checker
// lacks, or the reverse.
func TestAdmissionPolicyMessages(t *testing.T) {
	raw, err := os.ReadFile("../../../deploy/examples/admission-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var vap struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Validations []struct {
				Message string `yaml:"message"`
			} `yaml:"validations"`
		} `yaml:"spec"`
	}
	// The first document is the ValidatingAdmissionPolicy, the second its binding.
	if err := yaml.NewDecoder(bytes.NewReader(raw)).Decode(&vap); err != nil {
		t.Fatal(err)
	}
	if vap.Kind != "ValidatingAdmissionPolicy" {
		t.Fatalf("first document is %q", vap.Kind)
	}
	var got []string
	for _, v := range vap.Spec.Validations {
		got = append(got, v.Message)
	}
	if !slices.Equal(got, policyMessages) {
		t.Errorf("YAML messages %q, checker %q", got, policyMessages)
	}
}
