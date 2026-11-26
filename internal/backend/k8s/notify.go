package k8s

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	// fallbackEvery is how often a source whose watch is refused asks for a re-list.
	fallbackEvery = 30 * time.Second
	// watchRetry is how often a refused watch is tried again.
	watchRetry = 5 * time.Minute
	// maxWatchBackoff caps the wait after a transient watch error.
	maxWatchBackoff = 30 * time.Second
)

// The event sources, in the order their problems are joined.
const (
	srcPods = iota
	srcNodes
	numSources
)

// problems holds one message per degraded event source and reports their
// join whenever it changes. degraded runs under mu, so calls stay in order.
type problems struct {
	mu       sync.Mutex
	msgs     [numSources]string
	last     string
	degraded func(string)
}

func (p *problems) set(src int, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs[src] = msg
	var active []string
	for _, m := range p.msgs {
		if m != "" {
			active = append(active, m)
		}
	}
	if joined := strings.Join(active, "; "); joined != p.last {
		p.last = joined
		p.degraded(joined)
	}
}

// source is one watched resource.
type source struct {
	id       int
	name     string
	refused  string // the problem while the watch is refused
	open     func(ctx context.Context, rv string) (watch.Interface, error)
	relevant func(watch.Event) bool // called for every non-bookmark event, in order
}

// Notify watches the device pods, and the nodes in multi-node mode, and
// calls changed when an Observe or USBNodes result may differ. In single-node
// mode it calls changed once at nodesAt+nodeBackoff after each failed node
// list, so the step it causes lists the nodes again. It returns when ctx is done.
func (b *Backend) Notify(ctx context.Context, changed func(), degraded func(msg string)) {
	p := &problems{degraded: degraded}
	var wg sync.WaitGroup
	wg.Go(func() { b.watch(ctx, b.podsSource(), p, changed) })
	b.nodeMode(ctx, p, changed)
	wg.Wait()
}

func (b *Backend) podsSource() source {
	return source{
		id: srcPods, name: "pods",
		refused: "watching device pods is not allowed; re-checking every 30 s — add watch to the Role",
		open: func(ctx context.Context, rv string) (watch.Interface, error) {
			opts := allSlotsSelector()
			opts.ResourceVersion, opts.AllowWatchBookmarks = rv, true
			return b.client.CoreV1().Pods(b.p.Namespace).Watch(ctx, opts)
		},
		relevant: func(watch.Event) bool { return true },
	}
}

// nodesSource reports only changes to the set of nodes with USB devices:
// kubelet status updates must not wake the controller.
func (b *Backend) nodesSource() source {
	res := b.p.USBResource
	usb := map[string]bool{}
	return source{
		id: srcNodes, name: "nodes",
		refused: "watching nodes is not allowed; re-checking every 30 s — add watch to the ClusterRole",
		open: func(ctx context.Context, rv string) (watch.Interface, error) {
			if rv == "" {
				clear(usb) // the watch replays every node as ADDED
			}
			return b.client.CoreV1().Nodes().Watch(ctx, metav1.ListOptions{ResourceVersion: rv, AllowWatchBookmarks: true})
		},
		relevant: func(ev watch.Event) bool {
			n, ok := ev.Object.(*corev1.Node)
			if !ok {
				return false
			}
			has := ev.Type != watch.Deleted && hasUSB(n, res)
			if usb[n.Name] == has {
				return false
			}
			if has {
				usb[n.Name] = true
			} else {
				delete(usb, n.Name)
			}
			return true
		},
	}
}

// nodeMode follows USBNodes' signals: the nodes watch runs in multi-node
// mode, and single-node mode arms one re-check timer per failed list.
func (b *Backend) nodeMode(ctx context.Context, p *problems, changed func()) {
	var (
		timer       <-chan time.Time
		stopTimer   = func() bool { return false }
		cancelWatch context.CancelFunc
		watchDone   chan struct{}
	)
	apply := func(s nodesState) {
		stopTimer()
		timer = nil
		if s.multi {
			if cancelWatch == nil {
				var wctx context.Context
				wctx, cancelWatch = context.WithCancel(ctx)
				watchDone = make(chan struct{})
				go func() {
					defer close(watchDone)
					b.watch(wctx, b.nodesSource(), p, changed)
				}()
			}
			return
		}
		if cancelWatch != nil {
			cancelWatch()
			<-watchDone
			cancelWatch = nil
			p.set(srcNodes, "")
		}
		// A transient error on the list this fire causes sends no signal; the
		// controller's node-list blip retry steps again, and that list signals or succeeds.
		timer, stopTimer = b.newTimer(s.at.Add(nodeBackoff).Sub(b.now()))
	}

	// USBNodes may have run before Notify listened: act on the stored state,
	// and drop its signal, which says the same.
	b.mu.Lock()
	select {
	case <-b.nodesSig:
	default:
	}
	start, known := nodesState{multi: b.multi, at: b.nodesAt}, b.multi || b.nodesErr != nil
	b.mu.Unlock()
	if known {
		apply(start)
	}
	for {
		select {
		case <-ctx.Done():
			stopTimer()
			if cancelWatch != nil {
				<-watchDone
			}
			return
		case s := <-b.nodesSig:
			apply(s)
		case <-timer:
			timer = nil // re-armed only by the next failed list's signal
			changed()
		}
	}
}

// watch runs one source until ctx is done: it re-watches from the last
// resource version when the server closes the watch, from "" when that
// version expired, backs off on errors, and falls back to a periodic
// changed while the watch is refused (403/404).
func (b *Backend) watch(ctx context.Context, s source, p *problems, changed func()) {
	rv, backoff, failing := "", time.Second, false
	for ctx.Err() == nil {
		w, err := s.open(ctx, rv)
		if err == nil {
			p.set(s.id, "")
			if failing {
				failing = false
				slog.Info("watch works again", "resource", s.name)
			}
			var got bool
			rv, got, err = consume(ctx, w, rv, s.relevant, changed)
			w.Stop()
			if err == nil && got {
				backoff = time.Second
				continue
			}
		}
		switch {
		case ctx.Err() != nil:
			return
		case apierrors.IsGone(err) || apierrors.IsResourceExpired(err):
			rv = ""
			changed() // events may be lost
			continue
		case apierrors.IsForbidden(err) || apierrors.IsNotFound(err):
			p.set(s.id, s.refused)
			if !b.fallback(ctx, changed) {
				return
			}
			continue
		case err != nil && !failing:
			failing = true
			slog.Warn("watch failed; retrying", "resource", s.name, "err", err)
		}
		// An error, or a watch the server closed before any event.
		if !b.sleep(ctx, backoff) {
			return
		}
		backoff = min(2*backoff, maxWatchBackoff)
	}
}

// consume reads w until it closes, ctx ends or an error event arrives, and
// returns the last resource version seen, whether any event came, and the
// error event's error. Events already queued form one batch with at most one
// changed, so a burst (the ADDED replay of a watch from "") wakes once.
func consume(ctx context.Context, w watch.Interface, rv string, relevant func(watch.Event) bool, changed func()) (string, bool, error) {
	got := false
	for {
		var ev watch.Event
		var ok bool
		select {
		case <-ctx.Done():
			return rv, got, nil
		case ev, ok = <-w.ResultChan():
		}
		hit := false
		for {
			if !ok || ev.Type == watch.Error {
				if hit {
					changed()
				}
				if !ok {
					return rv, got, nil
				}
				return rv, true, apierrors.FromObject(ev.Object)
			}
			got = true
			if m, err := meta.Accessor(ev.Object); err == nil && m.GetResourceVersion() != "" {
				rv = m.GetResourceVersion()
			}
			if ev.Type != watch.Bookmark && relevant(ev) {
				hit = true
			}
			select {
			case ev, ok = <-w.ResultChan():
				continue
			default:
			}
			break
		}
		if hit {
			changed()
		}
	}
}

// fallback calls changed every fallbackEvery until watchRetry has passed; it
// returns false when ctx ends first.
func (b *Backend) fallback(ctx context.Context, changed func()) bool {
	for range watchRetry / fallbackEvery {
		if !b.sleep(ctx, fallbackEvery) {
			return false
		}
		changed()
	}
	return true
}

// sleep waits d, and returns false when ctx ends first.
func (b *Backend) sleep(ctx context.Context, d time.Duration) bool {
	c, stop := b.newTimer(d)
	defer stop()
	select {
	case <-ctx.Done():
		return false
	case <-c:
		return true
	}
}
