package k8s

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// notified records what Notify reported.
type notified struct {
	changed atomic.Int32
	mu      sync.Mutex
	msgs    []string
}

func (n *notified) degraded() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.msgs)
}

// startNotify runs b.Notify until the test ends, and waits for it to return.
func startNotify(t *testing.T, b *Backend) *notified {
	t.Helper()
	n := &notified{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Notify(ctx, func() { n.changed.Add(1) }, func(msg string) {
			n.mu.Lock()
			n.msgs = append(n.msgs, msg)
			n.mu.Unlock()
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Notify did not return after ctx end")
		}
	})
	return n
}

// waitFor polls cond until it holds, failing the test after 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// settle gives Notify's goroutines time to act on an event that must cause nothing.
func settle() { time.Sleep(50 * time.Millisecond) }

func watched(c *fake.Clientset, resource string) bool {
	return slices.ContainsFunc(c.Actions(), func(a k8stesting.Action) bool {
		return a.GetVerb() == "watch" && a.GetResource().Resource == resource
	})
}

// fastTimer runs every Notify wait 10000 times faster: 30 s is 3 ms.
func fastTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d / 10000)
	return t.C, t.Stop
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

type timerReq struct {
	deadline time.Time
	c        chan time.Time
	stopped  atomic.Bool
}

// recordTimers hands every requested timer to the test, which fires it.
type recordTimers struct {
	clk  *clock
	reqs chan *timerReq
}

func (r *recordTimers) newTimer(d time.Duration) (<-chan time.Time, func() bool) {
	tr := &timerReq{deadline: r.clk.now().Add(d), c: make(chan time.Time, 1)}
	r.reqs <- tr
	return tr.c, func() bool { return !tr.stopped.Swap(true) }
}

func (r *recordTimers) next(t *testing.T) *timerReq {
	t.Helper()
	select {
	case tr := <-r.reqs:
		return tr
	case <-time.After(5 * time.Second):
		t.Fatal("no timer requested")
		return nil
	}
}

func (r *recordTimers) none(t *testing.T) {
	t.Helper()
	settle()
	select {
	case tr := <-r.reqs:
		t.Fatalf("unexpected timer at %v", tr.deadline)
	default:
	}
}

func forbidWatch(c *fake.Clientset, resource string, on *atomic.Bool) {
	c.PrependWatchReactor(resource, func(k8stesting.Action) (bool, watch.Interface, error) {
		if !on.Load() {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("RBAC"))
	})
}

func TestNotifyPods(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset(ownerDeployment())
	n := startNotify(t, New(c, backendParams()))
	p, err := c.CoreV1().Pods(ns).Create(ctx, pod("dev-a", deviceLabels, nil), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "changed on pod create", func() bool { return n.changed.Load() >= 1 })
	p.Status.Phase = corev1.PodRunning
	if _, err := c.CoreV1().Pods(ns).UpdateStatus(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "changed on pod update", func() bool { return n.changed.Load() >= 2 })
	if d := n.degraded(); len(d) != 0 {
		t.Errorf("degraded %q", d)
	}
}

func TestNotifyNodesOnlyOnUSBSetChange(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset(ownerDeployment(), usbNode("n0", 1), usbNode("n1", -1))
	b := New(c, backendParams())
	if _, err := b.USBNodes(ctx); err != nil {
		t.Fatal(err)
	}
	n := startNotify(t, b)
	waitFor(t, "nodes watch", func() bool { return watched(c, "nodes") })
	waitFor(t, "changed for the initial USB set", func() bool { return n.changed.Load() >= 1 })
	settle()
	base := n.changed.Load()

	n0, err := c.CoreV1().Nodes().Get(ctx, "n0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n0.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: metav1.Now()}}
	if _, err := c.CoreV1().Nodes().UpdateStatus(ctx, n0, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	settle()
	if got := n.changed.Load(); got != base {
		t.Fatalf("status-only update: changed %d times, want 0", got-base)
	}

	n1 := usbNode("n1", 2)
	if _, err := c.CoreV1().Nodes().UpdateStatus(ctx, n1, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "changed on a new USB node", func() bool { return n.changed.Load() > base })
	settle()
	if got := n.changed.Load(); got != base+1 {
		t.Errorf("allocatable change: changed %d times, want 1", got-base)
	}
}

const (
	podsRefused  = "watching device pods is not allowed; re-checking every 30 s — add watch to the Role"
	nodesRefused = "watching nodes is not allowed; re-checking every 30 s — add watch to the ClusterRole"
)

func TestNotifyWatchForbiddenFallsBack(t *testing.T) {
	c := fake.NewClientset(ownerDeployment())
	var on atomic.Bool
	on.Store(true)
	forbidWatch(c, "pods", &on)
	b := New(c, backendParams())
	b.newTimer = fastTimer
	n := startNotify(t, b)
	waitFor(t, "degraded", func() bool { return slices.Equal(n.degraded(), []string{podsRefused}) })
	waitFor(t, "fallback re-checks", func() bool { return n.changed.Load() >= 2 })
}

func TestNotifyDegradedJoinsSources(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientset(ownerDeployment(), usbNode("n0", 1))
	var pods, nodes atomic.Bool
	pods.Store(true)
	nodes.Store(true)
	forbidWatch(c, "pods", &pods)
	forbidWatch(c, "nodes", &nodes)
	b := New(c, backendParams())
	b.newTimer = fastTimer
	if _, err := b.USBNodes(ctx); err != nil {
		t.Fatal(err)
	}
	n := startNotify(t, b)
	both := podsRefused + "; " + nodesRefused
	waitFor(t, "both problems", func() bool { return slices.Contains(n.degraded(), both) })

	pods.Store(false) // the Role gains watch
	waitFor(t, "pods recovered", func() bool { d := n.degraded(); return d[len(d)-1] == nodesRefused })

	// Back to single-node mode: the nodes watch stops and its problem goes.
	list := true
	forbidNodes(c, &list)
	if _, err := b.USBNodes(ctx); err == nil {
		t.Fatal("node list not refused")
	}
	waitFor(t, "nodes problem cleared", func() bool { d := n.degraded(); return d[len(d)-1] == "" })
	d := n.degraded()
	if i := slices.Index(d, ""); i != len(d)-1 {
		t.Errorf("degraded sequence %q: \"\" before the end", d)
	}
}

// TestNotifySingleNodeRecheck pins the single-node re-check chain: each
// failed node list re-arms one timer; the timer's fire never does.
func TestNotifySingleNodeRecheck(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := &clock{t: t0}
	c := fake.NewClientset(ownerDeployment())
	on := true
	lists := forbidNodes(c, &on)
	b := New(c, backendParams())
	b.now = clk.now
	timers := &recordTimers{clk: clk, reqs: make(chan *timerReq, 16)}
	b.newTimer = timers.newTimer
	n := startNotify(t, b)
	waitFor(t, "pods watch", func() bool { return watched(c, "pods") })
	timers.none(t)

	// 1. A refused list arms one timer at t0+nodeBackoff.
	if _, err := b.USBNodes(ctx); err == nil {
		t.Fatal("node list not refused")
	}
	tr := timers.next(t)
	if !tr.deadline.Equal(t0.Add(nodeBackoff)) {
		t.Fatalf("timer at %v, want %v", tr.deadline, t0.Add(nodeBackoff))
	}

	// 2. Its fire calls changed once and arms nothing.
	tr.c <- t0.Add(nodeBackoff)
	waitFor(t, "changed on the re-check", func() bool { return n.changed.Load() == 1 })
	timers.none(t)

	// 3. The cached error before nodeBackoff: no list, no timer, no changed.
	clk.set(t0.Add(nodeBackoff - time.Second))
	if _, err := b.USBNodes(ctx); err == nil {
		t.Fatal("cached failure not returned")
	}
	if *lists != 1 {
		t.Fatalf("%d node lists, want 1", *lists)
	}
	timers.none(t)

	// 4. The next refused list re-arms at its own nodesAt+nodeBackoff.
	clk.set(t0.Add(nodeBackoff))
	if _, err := b.USBNodes(ctx); err == nil {
		t.Fatal("node list not refused")
	}
	tr = timers.next(t)
	if !tr.deadline.Equal(t0.Add(2 * nodeBackoff)) {
		t.Fatalf("timer at %v, want %v", tr.deadline, t0.Add(2*nodeBackoff))
	}
	if got := n.changed.Load(); got != 1 {
		t.Fatalf("changed %d times, want 1", got)
	}

	// 5. A signal while armed replaces the timer; the old one firing does nothing.
	clk.set(t0.Add(2 * nodeBackoff))
	if _, err := b.USBNodes(ctx); err == nil {
		t.Fatal("node list not refused")
	}
	tr2 := timers.next(t)
	if !tr.stopped.Load() {
		t.Error("replaced timer not stopped")
	}
	tr.c <- t0.Add(2 * nodeBackoff)
	tr2.c <- t0.Add(3 * nodeBackoff)
	waitFor(t, "changed on the new timer", func() bool { return n.changed.Load() >= 2 })
	settle()
	if got := n.changed.Load(); got != 2 {
		t.Fatalf("changed %d times, want 2 (the replaced timer fired)", got)
	}

	// 6. Arm again, then a list that works: the timer stops and the nodes watch starts.
	clk.set(t0.Add(3 * nodeBackoff))
	if _, err := b.USBNodes(ctx); err == nil {
		t.Fatal("node list not refused")
	}
	tr3 := timers.next(t)
	on = false
	clk.set(t0.Add(4 * nodeBackoff))
	if _, err := b.USBNodes(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "nodes watch", func() bool { return watched(c, "nodes") })
	if !tr3.stopped.Load() {
		t.Error("armed timer not stopped on the switch to multi-node")
	}
	timers.none(t)
}

// TestNotifyStartsFromStoredFailure: a node list that failed before Notify
// listened arms the re-check from the stored nodesAt, with no signal.
func TestNotifyStartsFromStoredFailure(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := &clock{t: t0}
	c := fake.NewClientset(ownerDeployment())
	on := true
	forbidNodes(c, &on)
	b := New(c, backendParams())
	b.now = clk.now
	if _, err := b.USBNodes(ctx); err == nil {
		t.Fatal("node list not refused")
	}
	<-b.nodesSig // the signal is gone: Notify must read the stored state
	clk.set(t0.Add(time.Minute))
	timers := &recordTimers{clk: clk, reqs: make(chan *timerReq, 16)}
	b.newTimer = timers.newTimer
	startNotify(t, b)
	tr := timers.next(t)
	if !tr.deadline.Equal(t0.Add(nodeBackoff)) {
		t.Fatalf("timer at %v, want %v", tr.deadline, t0.Add(nodeBackoff))
	}
	timers.none(t)
}

// TestNotifyWatchResumes: a closed watch resumes from the last resource
// version; an expired one restarts from "" and calls changed.
func TestNotifyWatchResumes(t *testing.T) {
	c := fake.NewClientset(ownerDeployment())
	watchers := make(chan *watch.FakeWatcher, 4)
	var mu sync.Mutex
	var rvs []string
	c.PrependWatchReactor("pods", func(a k8stesting.Action) (bool, watch.Interface, error) {
		mu.Lock()
		rvs = append(rvs, a.(k8stesting.WatchAction).GetWatchRestrictions().ResourceVersion)
		mu.Unlock()
		w := watch.NewFakeWithChanSize(4, false)
		watchers <- w
		return true, w, nil
	})
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(rvs)
	}
	next := func() *watch.FakeWatcher {
		t.Helper()
		select {
		case w := <-watchers:
			return w
		case <-time.After(5 * time.Second):
			t.Fatal("no watch")
			return nil
		}
	}
	b := New(c, backendParams())
	timers := &recordTimers{clk: &clock{}, reqs: make(chan *timerReq, 16)}
	b.newTimer = timers.newTimer
	n := startNotify(t, b)

	w := next()
	w.Add(pod("dev-a", deviceLabels, func(p *corev1.Pod) { p.ResourceVersion = "17" }))
	w.Action(watch.Bookmark, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "21"}})
	waitFor(t, "changed on the event", func() bool { return n.changed.Load() == 1 })
	w.Stop()
	w = next()
	if got := seen(); !slices.Equal(got, []string{"", "21"}) {
		t.Fatalf("watch resource versions %q, want [\"\" 21]", got)
	}
	timers.none(t)

	w.Error(&apierrors.NewResourceExpired("too old").ErrStatus)
	next()
	if got := seen(); !slices.Equal(got, []string{"", "21", ""}) {
		t.Fatalf("watch resource versions %q, want [\"\" 21 \"\"]", got)
	}
	waitFor(t, "changed on expiry", func() bool { return n.changed.Load() == 2 })
	timers.none(t)
}
