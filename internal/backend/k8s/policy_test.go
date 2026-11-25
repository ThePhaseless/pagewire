package k8s

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/goccy/go-yaml"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type roleRule struct {
	APIGroups     []string `yaml:"apiGroups"`
	Resources     []string `yaml:"resources"`
	ResourceNames []string `yaml:"resourceNames"`
	Verbs         []string `yaml:"verbs"`
}

func loadRole(t *testing.T) []roleRule {
	t.Helper()
	raw, err := os.ReadFile("../../../deploy/base/role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var role struct {
		Rules []roleRule `yaml:"rules"`
	}
	if err := yaml.Unmarshal(raw, &role); err != nil {
		t.Fatal(err)
	}
	return role.Rules
}

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

// TestRoleAllowsEveryCall runs every Backend method against a fake clientset and
// checks each recorded call against deploy/base/role.yaml, resourceNames included.
func TestRoleAllowsEveryCall(t *testing.T) {
	ctx := context.Background()
	rules := loadRole(t)
	empty := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: TokenSecretName, Namespace: ns}}
	for name, c := range map[string]*fake.Clientset{
		"no secret":    fake.NewClientset(ownerDeployment(), pod("dev-a", deviceLabels, nil)),
		"empty secret": fake.NewClientset(ownerDeployment(), pod("dev-a", deviceLabels, nil), empty),
	} {
		b := New(c, backendParams())
		for _, spec := range []string{"ippusb", "hplip-airsane-blob"} {
			if err := b.Apply(ctx, goldenSpecs[spec]); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := b.AgentToken(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Observe(ctx); err != nil {
			t.Fatal(err)
		}
		if err := b.RestartPod(ctx); err != nil {
			t.Fatal(err)
		}
		if err := b.Apply(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if len(c.Actions()) == 0 {
			t.Fatal("no actions recorded")
		}
		for _, a := range c.Actions() {
			res := a.GetResource()
			resource := res.Resource
			if sub := a.GetSubresource(); sub != "" {
				resource += "/" + sub
			}
			for _, acc := range accesses(a) {
				if !allowed(rules, res.Group, resource, acc.verb, acc.name) {
					t.Errorf("%s: role.yaml does not allow %s %s/%s name %q", name, acc.verb, res.Group, resource, acc.name)
				}
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

// TestGoldensPassAdmissionPolicy checks that no rendered object uses what
// deploy/examples/admission-policy.yaml denies.
func TestGoldensPassAdmissionPolicy(t *testing.T) {
	files, err := filepath.Glob("testdata/*.golden.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("goldens %v %v", files, err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // test fixture path
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		walk(doc, func(key string, v any) {
			switch {
			case slices.Contains([]string{"privileged", "hostPID", "hostIPC", "allowPrivilegeEscalation"}, key) && v == true,
				key == "hostPath",
				key == "add" && v != nil:
				t.Errorf("%s sets %s: %v", f, key, v)
			}
		})
	}
}

func walk(v any, fn func(key string, v any)) {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			fn(k, x)
			walk(x, fn)
		}
	case []any:
		for _, x := range v {
			walk(x, fn)
		}
	}
}
