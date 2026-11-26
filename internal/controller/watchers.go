package controller

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/ThePhaseless/pagewire/internal/ippnotify"
	"github.com/ThePhaseless/pagewire/internal/printing"
)

// PrinterWatcher is an optional PrinterObserver capability (printing.Client
// implements it): Watch calls changed on the printer's IPP events until ctx
// ends, and otherwise returns an error wrapping ippnotify.ErrUnsupported or
// ippnotify.ErrRefused.
type PrinterWatcher interface {
	Watch(ctx context.Context, changed func()) error
}

// watchTarget is one endpoint a step matched, for syncWatchers.
type watchTarget struct {
	uri     string
	scanner bool
	absent  bool       // its device's non-zero USB key is missing from its workload's Attached list
	pull    bool       // never subscribe, only poll: a held Get-Notifications through ipp-usb keeps one of the device's IPP-USB interfaces busy
	seed    pollResult // the step's own query result, the watcher's baseline
}

// pollResult is what an endpoint poll compares: the value and whether the
// endpoint answered, never the error text.
type pollResult struct {
	ok    bool
	attrs printing.Attributes
	state string
}

func printerResult(a printing.Attributes, err error) pollResult {
	if err != nil {
		return pollResult{}
	}
	return pollResult{ok: true, attrs: a}
}

func scannerResult(state string, ok bool) pollResult {
	if !ok {
		return pollResult{}
	}
	return pollResult{ok: true, state: state}
}

// watcher is one running endpoint watcher. base is the last step's result
// for the endpoint, what Status shows: the poll pokes the loop when its own
// query differs from it. Only syncWatchers writes base.
type watcher struct {
	cancel context.CancelFunc
	kick   chan struct{} // a step's query failed: retry soon

	mu   sync.Mutex
	base pollResult
}

func (w *watcher) baseline() pollResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.base
}

// rebase makes r the baseline, and kicks the poll into its 1 s retry when
// the step's query failed where the last one answered.
func (w *watcher) rebase(r pollResult) {
	w.mu.Lock()
	failed := w.base.ok && !r.ok
	w.base = r
	w.mu.Unlock()
	if failed {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
}

// syncWatchers keeps one watcher per wanted URI whose device is not
// unplugged, starting or cancelling them, and hands every running one the
// step's result as its baseline. Only Run calls it, so a direct step starts
// nothing.
func (c *Controller) syncWatchers(ctx context.Context, want []watchTarget) {
	seen := map[string]bool{}
	for _, t := range want {
		if seen[t.uri] {
			continue
		}
		seen[t.uri] = true
		w := c.watchers[t.uri]
		switch {
		case t.absent:
			if w != nil {
				w.cancel()
				delete(c.watchers, t.uri)
			}
		case w == nil:
			wctx, cancel := context.WithCancel(ctx)
			w = &watcher{cancel: cancel, kick: make(chan struct{}, 1), base: t.seed}
			c.watchers[t.uri] = w
			go c.watch(wctx, t, w)
		default:
			w.rebase(t.seed)
		}
	}
	for uri, w := range c.watchers {
		if !seen[uri] {
			w.cancel()
			delete(c.watchers, uri)
		}
	}
}

// watch follows one endpoint until ctx ends. Every endpoint is polled (an
// IPP event does not cover everything: PAPPL refreshes reasons and supplies
// only inside Get-Printer-Attributes, and a hung application sends nothing).
// A printer with IPP notifications is also watched, so its changes show at
// once; never one served by ipp-usb (t.pull). A refused subscription is
// tried again after one lease, by when subscriptions leaked by a crash have
// expired.
func (c *Controller) watch(ctx context.Context, t watchTarget, w *watcher) {
	if !t.scanner && !t.pull {
		if pw, ok := c.printer(t.uri).(PrinterWatcher); ok {
			go c.subscribe(ctx, t.uri, pw)
		}
	}
	c.poll(ctx, t, w)
}

// subscribe runs pw.Watch until ctx ends or the printer has no IPP
// notifications.
func (c *Controller) subscribe(ctx context.Context, uri string, pw PrinterWatcher) {
	for {
		err := pw.Watch(ctx, c.poke)
		if ctx.Err() != nil {
			return
		}
		if !errors.Is(err, ippnotify.ErrRefused) {
			slog.Debug("printer without IPP notifications; polling only", "uri", uri, "err", err)
			return
		}
		slog.Debug("printer refused an IPP subscription; polling until the next try", "uri", uri, "err", err)
		tm := time.NewTimer(c.leaseWait)
		select {
		case <-ctx.Done():
			tm.Stop()
			return
		case <-tm.C:
		}
	}
}

// poll queries t every pollEvery and pokes the loop when the result differs
// from w's baseline. While the baseline or its own query is a failure it
// retries after 1 s, doubling up to pollEvery; otherwise its first query is
// one interval after start, because the step just queried t.
func (c *Controller) poll(ctx context.Context, t watchTarget, w *watcher) {
	first := min(c.pollRetry, c.pollEvery)
	backoff, failing := first, false
	wait := c.pollEvery
	if !w.baseline().ok {
		wait = first
	}
	tm := time.NewTimer(wait)
	defer tm.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.kick:
			backoff = first
			tm.Reset(first)
			continue
		case <-tm.C:
		}
		r, err := c.query(ctx, t)
		if ctx.Err() != nil {
			return
		}
		base := w.baseline()
		if !reflect.DeepEqual(r, base) {
			c.poke()
		}
		wait := c.pollEvery
		if err == nil && base.ok {
			if failing {
				slog.Info("endpoint answers again", "uri", t.uri)
			}
			backoff, failing = first, false
		} else {
			if err != nil && !failing {
				slog.Info("endpoint query failed; retrying", "uri", t.uri, "err", err)
			}
			// A failed baseline retries too: the step's own query may fail again.
			wait, backoff, failing = backoff, min(backoff*2, c.pollEvery), failing || err != nil
		}
		tm.Reset(wait)
	}
}

// query is one poll of t, bounded by queryTimeout.
func (c *Controller) query(ctx context.Context, t watchTarget) (pollResult, error) {
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	if t.scanner {
		st, err := c.scanner(t.uri).Status(qctx)
		return scannerResult(string(st), err == nil), err
	}
	a, err := c.printer(t.uri).Attributes(qctx)
	return printerResult(a, err), err
}
