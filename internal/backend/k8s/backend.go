package k8s

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ThePhaseless/printscan/internal/device"
)

// Backend runs the device pod through the Kubernetes API. It implements
// device.Backend, device.PodRestarter and device.AgentTokenSource.
type Backend struct {
	client kubernetes.Interface
	p      Params

	mu       sync.Mutex
	resolved bool // p.Owner.UID and p.SelfImage come from the owner Deployment

	tokMu  sync.Mutex // separate from mu: agent auth must not wait for an Apply
	tok    string
	tokErr error
	tokAt  time.Time
	now    func() time.Time
}

// tokenTTL bounds how often AgentToken reads the Secret, so unauthenticated
// requests to the internal listener cannot drive API server load.
const tokenTTL = 5 * time.Second

var (
	_ device.Backend          = (*Backend)(nil)
	_ device.PodRestarter     = (*Backend)(nil)
	_ device.AgentTokenSource = (*Backend)(nil)
)

// New returns a Backend. It makes no API call; the owner Deployment is read on
// the first Apply with a spec.
func New(client kubernetes.Interface, p Params) *Backend {
	return &Backend{client: client, p: p, now: time.Now}
}

var selector = metav1.ListOptions{LabelSelector: NameLabel + "=" + DeviceName}

// Apply renders spec and server-side applies it; a nil spec deletes the device
// Deployment and ConfigMap.
func (b *Backend) Apply(ctx context.Context, spec *device.Spec) error {
	ns := b.p.Namespace
	if spec == nil {
		del := metav1.DeleteOptions{}
		return errors.Join(
			ignoreNotFound(b.client.AppsV1().Deployments(ns).Delete(ctx, DeviceName, del)),
			ignoreNotFound(b.client.CoreV1().ConfigMaps(ns).Delete(ctx, DeviceName, del)),
		)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.resolveOwner(ctx); err != nil {
		return err
	}
	// Render before creating anything, so a spec it refuses leaves no Secret behind.
	deploy, cm, err := Render(spec, b.p)
	if err != nil {
		return err
	}
	if err := b.ensureToken(ctx); err != nil {
		return err
	}
	opts := metav1.ApplyOptions{FieldManager: FieldManager, Force: true}
	if _, err := b.client.CoreV1().ConfigMaps(ns).Apply(ctx, cm, opts); err != nil {
		return fmt.Errorf("apply configmap %s/%s: %w", ns, DeviceName, err)
	}
	if _, err := b.client.AppsV1().Deployments(ns).Apply(ctx, deploy, opts); err != nil {
		return fmt.Errorf("apply deployment %s/%s: %w", ns, DeviceName, err)
	}
	return nil
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
		b.p.SelfImage = ImageRef(b.p.Registry, b.p.Tag, "printscan", b.p.Overrides)
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

// Observe reports the device pod's state. Endpoints stay empty: in Kubernetes
// they come from agent reports.
func (b *Backend) Observe(ctx context.Context) (device.Observation, error) {
	pods, err := b.client.CoreV1().Pods(b.p.Namespace).List(ctx, selector)
	if err != nil {
		return device.Observation{}, fmt.Errorf("list device pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return device.Observation{Phase: device.PhaseAbsent}, nil
	}
	pod := pods.Items[0]
	for _, p := range pods.Items { // prefer a pod that is not terminating (Recreate rollouts)
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
	return obs, nil
}

// RestartPod deletes every device pod by name, so the Deployment recreates it
// and the kubelet allocates the current USB device node.
func (b *Backend) RestartPod(ctx context.Context) error {
	pods := b.client.CoreV1().Pods(b.p.Namespace)
	list, err := pods.List(ctx, selector)
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
