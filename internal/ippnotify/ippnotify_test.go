package ippnotify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"
)

// server is an IPP endpoint answering each request through reply, where n
// counts the requests of that operation from 1. A nil answer with status 200
// holds a Get-Notifications until the client gives up, like an idle
// notify-wait, and answers any other operation successful-ok.
type server struct {
	mu    sync.Mutex
	reqs  []*goipp.Message
	count map[goipp.Op]int
	reply func(op goipp.Op, n int, req *goipp.Message) (int, *goipp.Message)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := &goipp.Message{}
	if err := req.Decode(r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	op := goipp.Op(req.Code)
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	if s.count == nil {
		s.count = map[goipp.Op]int{}
	}
	s.count[op]++
	n := s.count[op]
	s.mu.Unlock()
	code, rsp := s.reply(op, n, req)
	switch {
	case code == -1: // drop the connection
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	case code == -2: // a 200 that is not IPP
		_, _ = w.Write([]byte("<html>not ipp</html>"))
	case code != http.StatusOK:
		w.WriteHeader(code)
	case rsp == nil && op == goipp.OpGetNotifications:
		<-r.Context().Done()
	case rsp == nil:
		w.Header().Set("Content-Type", goipp.ContentType)
		_ = answer(req, goipp.StatusOk).Encode(w)
	default:
		w.Header().Set("Content-Type", goipp.ContentType)
		_ = rsp.Encode(w)
	}
}

func (s *server) ops() []goipp.Op {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []goipp.Op
	for _, r := range s.reqs {
		out = append(out, goipp.Op(r.Code))
	}
	return out
}

// requests returns the requests of op in order.
func (s *server) requests(op goipp.Op) []*goipp.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*goipp.Message
	for _, r := range s.reqs {
		if goipp.Op(r.Code) == op {
			out = append(out, r)
		}
	}
	return out
}

func answer(req *goipp.Message, st goipp.Status, groups ...goipp.Group) *goipp.Message {
	rsp := goipp.NewResponse(goipp.DefaultVersion, st, req.RequestID)
	rsp.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	rsp.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	if len(groups) > 0 {
		rsp.Groups = append(goipp.Groups{{Tag: goipp.TagOperationGroup, Attrs: rsp.Operation}}, groups...)
	}
	return rsp
}

func subscribed(req *goipp.Message, id int) *goipp.Message {
	return answer(req, goipp.StatusOk, goipp.Group{Tag: goipp.TagSubscriptionGroup, Attrs: goipp.Attributes{
		goipp.MakeAttribute("notify-subscription-id", goipp.TagInteger, goipp.Integer(int32(id))), //nolint:gosec // test ids
	}})
}

// event is one event group with its sequence number and job-state (0: none).
func event(seq, jobState int) goipp.Group {
	a := goipp.Attributes{
		goipp.MakeAttribute("notify-sequence-number", goipp.TagInteger, goipp.Integer(int32(seq))), //nolint:gosec // test values
		goipp.MakeAttribute("notify-subscribed-event", goipp.TagKeyword, goipp.String(EventPrinterStateChanged)),
	}
	if jobState > 0 {
		a.Add(goipp.MakeAttribute("job-state", goipp.TagEnum, goipp.Integer(int32(jobState)))) //nolint:gosec // test values
	}
	return goipp.Group{Tag: goipp.TagEventNotificationGroup, Attrs: a}
}

// clock is the fake now; it moves only when a test handler advances it.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// harness swaps now and sleep for the test: sleeps return at once and are
// recorded. Watch must have returned before the test ends (watch does that).
type harness struct {
	clk    *clock
	mu     sync.Mutex
	sleeps []time.Duration
	slept  chan time.Duration
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{clk: &clock{t: time.Unix(1e9, 0)}, slept: make(chan time.Duration, 1000)}
	oldNow, oldSleep := now, sleep
	now = h.clk.now
	sleep = func(ctx context.Context, d time.Duration) error {
		h.mu.Lock()
		h.sleeps = append(h.sleeps, d)
		h.mu.Unlock()
		select {
		case h.slept <- d:
		default:
		}
		return ctx.Err()
	}
	t.Cleanup(func() { now, sleep = oldNow, oldSleep })
	return h
}

func (h *harness) sleepCalls() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.sleeps)
}

// run is one Watch against s.
type run struct {
	changed chan []goipp.Attributes
	done    chan error
	cancel  context.CancelFunc
}

// watch starts Watch on t against s; the test's cleanup cancels it and waits.
func watch(t *testing.T, s *server, target Target) *run {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	target.URI = "ipp://" + srv.Listener.Addr().String() + target.URI
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	r := &run{changed: make(chan []goipp.Attributes, 100), done: make(chan error, 1), cancel: cancel}
	go func() { r.done <- Watch(ctx, target, func(ev []goipp.Attributes) { r.changed <- ev }) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-r.done:
			_ = err // the test checked it, if it cared
		case <-time.After(5 * time.Second):
			t.Error("Watch did not return after cancel")
		}
	})
	return r
}

// next returns the next changed call, failing after a deadline.
func (r *run) next(t *testing.T) []goipp.Attributes {
	t.Helper()
	select {
	case ev := <-r.changed:
		return ev
	case err := <-r.done:
		t.Fatalf("Watch returned %v, want another changed call", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no changed call within 5 s")
	}
	return nil
}

// result waits for Watch to return.
func (r *run) result(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		r.done <- err // for the cleanup
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return within 5 s")
	}
	return nil
}

// stop cancels Watch and returns its error.
func (r *run) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	return r.result(t)
}

var printerTarget = Target{URI: "/ipp/print/q", URIAttr: "printer-uri", Op: goipp.OpCreatePrinterSubscriptions, Events: []string{EventPrinterStateChanged}}

func ints(attrs goipp.Attributes, name string) int { return intAttr(attrs, name) }

func subAttrs(m *goipp.Message) goipp.Attributes {
	for _, g := range m.AttrGroups() {
		if g.Tag == goipp.TagSubscriptionGroup {
			return g.Attrs
		}
	}
	return nil
}

func TestSubscribeAndEvent(t *testing.T) {
	newHarness(t)
	s := &server{reply: func(op goipp.Op, n int, req *goipp.Message) (int, *goipp.Message) {
		switch {
		case op == goipp.OpCreatePrinterSubscriptions:
			return 200, subscribed(req, 7)
		case op == goipp.OpGetNotifications && n == 1:
			return 200, answer(req, goipp.StatusOk, event(1, 0))
		case op == goipp.OpCancelSubscription:
			return 200, answer(req, goipp.StatusOk)
		}
		return 200, nil
	}}
	r := watch(t, s, printerTarget)
	if ev := r.next(t); ev != nil {
		t.Fatalf("first changed = %v, want nil after Create", ev)
	}
	if ev := r.next(t); len(ev) != 1 || ints(ev[0], "notify-sequence-number") != 1 {
		t.Fatalf("second changed = %v, want the one event", ev)
	}
	// Wait for the held second Get-Notifications, then end the watch.
	deadline := time.Now().Add(5 * time.Second)
	for len(s.requests(goipp.OpGetNotifications)) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := r.stop(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("Watch = %v, want context.Canceled", err)
	}

	cr := s.requests(goipp.OpCreatePrinterSubscriptions)[0]
	sa := subAttrs(cr)
	if first(sa, "notify-pull-method") != "ippget" || ints(sa, "notify-lease-duration") != Lease || first(sa, "notify-events") != EventPrinterStateChanged {
		t.Errorf("Create subscription group = %v", sa)
	}
	gets := s.requests(goipp.OpGetNotifications)
	for i, want := range []int{1, 2} {
		g := gets[i].Operation
		if ints(g, "notify-subscription-ids") != 7 || ints(g, "notify-sequence-numbers") != want || !boolAttr(g, "notify-wait") {
			t.Errorf("Get-Notifications %d = %v, want id 7, sequence %d, notify-wait", i+1, g, want)
		}
	}
	cs := s.requests(goipp.OpCancelSubscription)
	if len(cs) != 1 || ints(cs[0].Operation, "notify-subscription-id") != 7 {
		t.Errorf("Cancel-Subscription requests = %v, want one for id 7", cs)
	}
}

func first(attrs goipp.Attributes, name string) string {
	for _, a := range attrs {
		if a.Name == name && len(a.Values) > 0 {
			return a.Values[0].V.String()
		}
	}
	return ""
}

func boolAttr(attrs goipp.Attributes, name string) bool {
	for _, a := range attrs {
		if a.Name == name && len(a.Values) > 0 {
			b, ok := a.Values[0].V.(goipp.Boolean)
			return ok && bool(b)
		}
	}
	return false
}

func TestGetNotificationsNotFoundRecreates(t *testing.T) {
	newHarness(t)
	s := &server{reply: func(op goipp.Op, n int, req *goipp.Message) (int, *goipp.Message) {
		switch op {
		case goipp.OpCreatePrinterSubscriptions:
			return 200, subscribed(req, n)
		case goipp.OpGetNotifications:
			switch n {
			case 1:
				return 200, answer(req, goipp.StatusOk, event(5, 0))
			case 2:
				return 200, answer(req, goipp.StatusErrorNotFound)
			case 3:
				return 200, answer(req, goipp.StatusOk, event(1, 0))
			}
		}
		return 200, nil
	}}
	r := watch(t, s, printerTarget)
	for i, want := range []int{0, 1, 0, 1} {
		if ev := r.next(t); len(ev) != want {
			t.Fatalf("changed call %d = %v, want %d events", i+1, ev, want)
		}
	}
	if got := len(s.requests(goipp.OpCreatePrinterSubscriptions)); got != 2 {
		t.Errorf("Creates = %d, want 2", got)
	}
	// The new subscription's events start again at 1.
	g := s.requests(goipp.OpGetNotifications)[2].Operation
	if ints(g, "notify-subscription-ids") != 2 || ints(g, "notify-sequence-numbers") != 1 {
		t.Errorf("Get-Notifications after re-Create = %v, want id 2 from sequence 1", g)
	}
}

func TestJobCreateNotFoundRefused(t *testing.T) {
	newHarness(t)
	s := &server{reply: func(_ goipp.Op, _ int, req *goipp.Message) (int, *goipp.Message) {
		return 200, answer(req, goipp.StatusErrorNotFound)
	}}
	r := watch(t, s, Target{URI: "/ipp/print/q", URIAttr: "printer-uri", Op: goipp.OpCreateJobSubscriptions, JobID: 3, Events: []string{EventJobCompleted}})
	if err := r.result(t); !errors.Is(err, ErrRefused) {
		t.Fatalf("Watch = %v, want ErrRefused", err)
	}
	if ops := s.ops(); !slices.Equal(ops, []goipp.Op{goipp.OpCreateJobSubscriptions}) {
		t.Errorf("requests = %v, want one Create and no Cancel", ops)
	}
}

func TestPrinterCreateNotFoundRetries(t *testing.T) {
	h := newHarness(t)
	s := &server{reply: func(op goipp.Op, n int, req *goipp.Message) (int, *goipp.Message) {
		switch {
		case op == goipp.OpCreatePrinterSubscriptions && n == 1:
			return 200, answer(req, goipp.StatusErrorNotFound)
		case op == goipp.OpCreatePrinterSubscriptions:
			return 200, subscribed(req, 3)
		}
		return 200, nil
	}}
	r := watch(t, s, printerTarget)
	if ev := r.next(t); ev != nil {
		t.Fatalf("changed = %v, want nil after the second Create", ev)
	}
	if got := len(s.requests(goipp.OpCreatePrinterSubscriptions)); got != 2 {
		t.Errorf("Creates = %d, want 2", got)
	}
	if got := h.sleepCalls(); !slices.Equal(got, []time.Duration{time.Second}) {
		t.Errorf("sleeps = %v, want one 1 s backoff", got)
	}
}

func TestCreateAnswers(t *testing.T) {
	tests := []struct {
		name string
		code int
		st   goipp.Status
		noID bool
		want error // nil: retried, then the next Create succeeds
	}{
		{name: "operation-not-supported", code: 200, st: goipp.StatusErrorOperationNotSupported, want: ErrUnsupported},
		{name: "attributes-or-values", code: 200, st: goipp.StatusErrorAttributesOrValues, want: ErrUnsupported},
		{name: "bad-request", code: 200, st: goipp.StatusErrorBadRequest, want: ErrUnsupported},
		{name: "ignored-all-subscriptions", code: 200, st: goipp.StatusErrorIgnoredAllSubscriptions, want: ErrRefused},
		{name: "ok without id", code: 200, st: goipp.StatusOkIgnoredSubscriptions, noID: true, want: ErrRefused},
		{name: "HTTP 403", code: 403, want: ErrRefused},
		{name: "HTTP 401", code: 401, want: ErrRefused},
		{name: "HTTP 426", code: 426, want: ErrRefused},
		{name: "HTTP 404", code: 404, want: ErrUnsupported},
		{name: "HTTP 405", code: 405, want: ErrUnsupported},
		{name: "HTTP 503", code: 503},
		{name: "HTTP 429", code: 429},
		{name: "connection dropped", code: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newHarness(t)
			s := &server{reply: func(op goipp.Op, n int, req *goipp.Message) (int, *goipp.Message) {
				switch {
				case op == goipp.OpCreatePrinterSubscriptions && n == 1:
					if tt.code != 200 {
						return tt.code, nil
					}
					return 200, answer(req, tt.st)
				case op == goipp.OpCreatePrinterSubscriptions:
					return 200, subscribed(req, 1)
				case op == goipp.OpCancelSubscription:
					return 200, answer(req, goipp.StatusOk)
				}
				return 200, nil
			}}
			r := watch(t, s, printerTarget)
			if tt.want != nil {
				if err := r.result(t); !errors.Is(err, tt.want) {
					t.Fatalf("Watch = %v, want %v", err, tt.want)
				}
				if got := s.ops(); len(got) != 1 {
					t.Errorf("requests = %v, want only the Create", got)
				}
				return
			}
			if ev := r.next(t); ev != nil {
				t.Fatalf("changed = %v, want nil after the retried Create", ev)
			}
			if got := len(s.requests(goipp.OpCreatePrinterSubscriptions)); got != 2 {
				t.Errorf("Creates = %d, want 2", got)
			}
		})
	}
}

func TestTransportErrorKeepsSubscription(t *testing.T) {
	h := newHarness(t)
	s := &server{reply: func(op goipp.Op, n int, req *goipp.Message) (int, *goipp.Message) {
		switch {
		case op == goipp.OpCreatePrinterSubscriptions:
			return 200, subscribed(req, 4)
		case op == goipp.OpGetNotifications && n == 1:
			return 503, nil
		case op == goipp.OpGetNotifications && n == 2:
			return -1, nil
		case op == goipp.OpGetNotifications && n == 3:
			return 200, answer(req, goipp.StatusOk, event(1, 0))
		}
		return 200, nil
	}}
	r := watch(t, s, printerTarget)
	r.next(t)
	if ev := r.next(t); len(ev) != 1 {
		t.Fatalf("changed = %v, want the event after two transport errors", ev)
	}
	if got := len(s.requests(goipp.OpCreatePrinterSubscriptions)); got != 1 {
		t.Errorf("Creates = %d, want 1", got)
	}
	for i, g := range s.requests(goipp.OpGetNotifications)[:3] {
		if id := ints(g.Operation, "notify-subscription-ids"); id != 4 {
			t.Errorf("Get-Notifications %d asked for id %d, want 4", i+1, id)
		}
	}
	if got := h.sleepCalls(); !slices.Equal(got, []time.Duration{time.Second, 2 * time.Second}) {
		t.Errorf("sleeps = %v, want 1 s then 2 s", got)
	}
}

func TestRenewAtHalfLease(t *testing.T) {
	h := newHarness(t)
	s := &server{reply: func(op goipp.Op, n int, req *goipp.Message) (int, *goipp.Message) {
		switch {
		case op == goipp.OpCreatePrinterSubscriptions:
			return 200, subscribed(req, 5)
		case op == goipp.OpGetNotifications && n == 1:
			h.clk.advance(Lease / 2 * time.Second) // held until the half-life
			return 200, answer(req, goipp.StatusOk)
		case op == goipp.OpRenewSubscription && n == 1:
			return 200, answer(req, goipp.StatusOk)
		case op == goipp.OpGetNotifications && n == 2:
			h.clk.advance(Lease / 2 * time.Second)
			return 200, answer(req, goipp.StatusOk)
		case op == goipp.OpRenewSubscription: // the server lost it
			return 200, answer(req, goipp.StatusErrorNotFound)
		case op == goipp.OpCancelSubscription:
			return 200, answer(req, goipp.StatusOk)
		}
		return 200, nil
	}}
	r := watch(t, s, printerTarget)
	r.next(t)
	r.next(t) // the re-Create after the second Renew's not-found
	want := []goipp.Op{
		goipp.OpCreatePrinterSubscriptions, goipp.OpGetNotifications, goipp.OpRenewSubscription,
		goipp.OpGetNotifications, goipp.OpRenewSubscription, goipp.OpCreatePrinterSubscriptions,
	}
	if got := s.ops(); !slices.Equal(got[:len(want)], want) {
		t.Fatalf("requests = %v, want %v first", got, want)
	}
	rn := s.requests(goipp.OpRenewSubscription)[0].Operation
	if ints(rn, "notify-subscription-id") != 5 || ints(rn, "notify-lease-duration") != Lease {
		t.Errorf("Renew-Subscription = %v, want id 5 and lease %d", rn, Lease)
	}
}

func TestFastEmptyGuard(t *testing.T) {
	for _, tt := range []struct {
		name     string
		interval int // notify-get-interval on the first answer's event; 0: none
		want     time.Duration
	}{
		{name: "no interval", want: 30 * time.Second},
		{name: "interval clamped", interval: 2, want: 5 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			s := &server{reply: func(op goipp.Op, n int, req *goipp.Message) (int, *goipp.Message) {
				switch {
				case op == goipp.OpCreatePrinterSubscriptions:
					return 200, subscribed(req, 1)
				case op == goipp.OpGetNotifications && n == 1 && tt.interval > 0:
					rsp := answer(req, goipp.StatusOk, event(1, 0))
					rsp.Groups[0].Attrs.Add(goipp.MakeAttribute("notify-get-interval", goipp.TagInteger, goipp.Integer(int32(tt.interval)))) //nolint:gosec // test value
					return 200, rsp
				case op == goipp.OpGetNotifications:
					return 200, answer(req, goipp.StatusOk) // never holds
				}
				return 200, answer(req, goipp.StatusOk)
			}}
			r := watch(t, s, printerTarget)
			select {
			case d := <-h.slept:
				if d != tt.want {
					t.Errorf("guard sleep = %v, want %v", d, tt.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no guard sleep")
			}
			_ = r.stop(t)
			empty := len(s.requests(goipp.OpGetNotifications))
			if tt.interval > 0 {
				empty--
			}
			if empty < 3 {
				t.Errorf("guard slept after %d empty answers, want 3", empty)
			}
		})
	}
}

func TestHeldServerQuickEmptiesReaskAtOnce(t *testing.T) {
	h := newHarness(t)
	s := &server{reply: func(op goipp.Op, n int, req *goipp.Message) (int, *goipp.Message) {
		switch {
		case op == goipp.OpCreatePrinterSubscriptions:
			return 200, subscribed(req, 1)
		case op == goipp.OpGetNotifications && n == 1:
			h.clk.advance(2 * time.Second) // held, then empty
			return 200, answer(req, goipp.StatusOk)
		case op == goipp.OpGetNotifications && n <= 6:
			return 200, answer(req, goipp.StatusOk) // spurious wakeups
		case op == goipp.OpGetNotifications && n == 7:
			return 200, answer(req, goipp.StatusOk, event(1, 0))
		case op == goipp.OpGetNotifications:
			return 200, nil
		}
		return 200, answer(req, goipp.StatusOk)
	}}
	r := watch(t, s, printerTarget)
	r.next(t)
	if ev := r.next(t); len(ev) != 1 {
		t.Fatalf("changed = %v, want the event", ev)
	}
	if got := h.sleepCalls(); len(got) != 0 {
		t.Errorf("sleeps = %v, want none", got)
	}
}

func TestJobTerminalCancels(t *testing.T) {
	newHarness(t)
	s := &server{reply: func(op goipp.Op, n int, req *goipp.Message) (int, *goipp.Message) {
		switch {
		case op == goipp.OpCreateJobSubscriptions:
			return 200, subscribed(req, 9)
		case op == goipp.OpGetNotifications && n == 1:
			return 200, answer(req, goipp.StatusOk, event(1, 5))
		case op == goipp.OpGetNotifications && n == 2:
			return 200, answer(req, goipp.StatusOk, event(2, 9))
		}
		return 200, answer(req, goipp.StatusOk)
	}}
	r := watch(t, s, Target{URI: "/ipp/print/q", URIAttr: "printer-uri", Op: goipp.OpCreateJobSubscriptions, JobID: 42, Events: []string{EventJobStateChanged, EventJobCompleted}})
	if err := r.result(t); err != nil {
		t.Fatalf("Watch = %v, want nil after the terminal event", err)
	}
	if n := len(r.changed); n != 3 {
		t.Errorf("changed calls = %d, want 3 (create, processing, completed)", n)
	}
	cr := s.requests(goipp.OpCreateJobSubscriptions)[0]
	if ints(cr.Operation, "notify-job-id") != 42 || ints(subAttrs(cr), "notify-lease-duration") != Lease {
		t.Errorf("Create-Job-Subscriptions = %v, want notify-job-id 42 and lease %d", cr, Lease)
	}
	want := []goipp.Op{goipp.OpCreateJobSubscriptions, goipp.OpGetNotifications, goipp.OpGetNotifications, goipp.OpCancelSubscription}
	if got := s.ops(); !slices.Equal(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
	if id := ints(s.requests(goipp.OpCancelSubscription)[0].Operation, "notify-subscription-id"); id != 9 {
		t.Errorf("Cancel-Subscription id = %d, want 9", id)
	}
}

func TestBadURIUnsupported(t *testing.T) {
	err := Watch(context.Background(), Target{URI: "http://x/ipp/print/q"}, func([]goipp.Attributes) {})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Watch = %v, want ErrUnsupported", err)
	}
}

// A server that answers every Create unusably (a status the mapping does not
// know, a body that is not IPP) gets answeredTries tries, then ErrRefused, so
// the caller polls instead of waiting forever.
func TestCreateUnusableAnswersRefused(t *testing.T) {
	for _, tt := range []struct {
		name string
		code int
	}{
		{name: "HTTP 500", code: 500},
		{name: "HTTP 400", code: 400},
		{name: "undecodable 200", code: -2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			newHarness(t)
			s := &server{reply: func(op goipp.Op, _ int, req *goipp.Message) (int, *goipp.Message) {
				if op == goipp.OpCreatePrinterSubscriptions {
					return tt.code, nil
				}
				return 200, answer(req, goipp.StatusOk)
			}}
			r := watch(t, s, printerTarget)
			if err := r.result(t); !errors.Is(err, ErrRefused) {
				t.Fatalf("Watch = %v, want ErrRefused", err)
			}
			if got := len(s.requests(goipp.OpCreatePrinterSubscriptions)); got != answeredTries {
				t.Errorf("Creates = %d, want %d", got, answeredTries)
			}
			if len(r.changed) != 0 {
				t.Error("changed called without a subscription")
			}
		})
	}
}

// Unanswered Creates (a server still starting) retry without a bound.
func TestCreateUnansweredRetriesUnbounded(t *testing.T) {
	newHarness(t)
	s := &server{reply: func(op goipp.Op, n int, req *goipp.Message) (int, *goipp.Message) {
		switch {
		case op == goipp.OpCreatePrinterSubscriptions && n <= 2*answeredTries:
			return -1, nil
		case op == goipp.OpCreatePrinterSubscriptions:
			return 200, subscribed(req, 3)
		}
		return 200, nil
	}}
	r := watch(t, s, printerTarget)
	if ev := r.next(t); ev != nil {
		t.Fatalf("changed = %v, want nil after the subscribe", ev)
	}
	if got := len(s.requests(goipp.OpCreatePrinterSubscriptions)); got != 2*answeredTries+1 {
		t.Errorf("Creates = %d, want %d", got, 2*answeredTries+1)
	}
}

// A server that held once and then answers every Get-Notifications empty at
// once has stopped holding: after heldQuick quick answers Watch paces itself
// like for a server that never held, one sleep per request.
func TestHeldServerStopsHolding(t *testing.T) {
	h := newHarness(t)
	s := &server{reply: func(op goipp.Op, n int, req *goipp.Message) (int, *goipp.Message) {
		switch {
		case op == goipp.OpCreatePrinterSubscriptions:
			return 200, subscribed(req, 1)
		case op == goipp.OpGetNotifications && n == 1:
			h.clk.advance(2 * time.Second) // held, then empty
			return 200, answer(req, goipp.StatusOk)
		}
		return 200, answer(req, goipp.StatusOk) // never holds again
	}}
	r := watch(t, s, printerTarget)
	r.next(t)
	for range 3 {
		select {
		case d := <-h.slept:
			if d != 30*time.Second {
				t.Errorf("guard sleep = %v, want 30 s", d)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no guard sleep: the requests are unbounded")
		}
	}
	_ = r.stop(t)
	gn, sleeps := len(s.requests(goipp.OpGetNotifications)), len(h.sleepCalls())
	if gn-sleeps > heldQuick+1 { // the held answer, heldQuick quick ones before the first sleep
		t.Errorf("%d Get-Notifications for %d sleeps, want at most %d unpaced", gn, sleeps, heldQuick+1)
	}
}

// A server that accepts Create but answers every Get-Notifications not-found
// must not be re-subscribed every second: the backoff grows across re-Creates.
func TestGoneAtOnceBacksOff(t *testing.T) {
	h := newHarness(t)
	s := &server{reply: func(op goipp.Op, n int, req *goipp.Message) (int, *goipp.Message) {
		switch op {
		case goipp.OpCreatePrinterSubscriptions:
			return 200, subscribed(req, n)
		case goipp.OpGetNotifications:
			return 200, answer(req, goipp.StatusErrorNotFound)
		}
		return 200, nil
	}}
	watch(t, s, printerTarget)
	var got []time.Duration
	for len(got) < 4 {
		select {
		case d := <-h.slept:
			got = append(got, d)
		case <-time.After(5 * time.Second):
			t.Fatalf("sleeps = %v, want 4", got)
		}
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}; !slices.Equal(got, want) {
		t.Errorf("sleeps = %v, want %v", got, want)
	}
}

// Backoff built up by refused Creates must not carry into the new
// subscription: its first transport error waits 1 s.
func TestCreateBackoffResetsForPoll(t *testing.T) {
	h := newHarness(t)
	s := &server{reply: func(op goipp.Op, n int, req *goipp.Message) (int, *goipp.Message) {
		switch {
		case op == goipp.OpCreatePrinterSubscriptions && n <= 3:
			return 200, answer(req, goipp.StatusErrorNotFound)
		case op == goipp.OpCreatePrinterSubscriptions:
			return 200, subscribed(req, 5)
		case op == goipp.OpGetNotifications && n == 1:
			return 503, nil
		case op == goipp.OpGetNotifications && n == 2:
			return 200, answer(req, goipp.StatusOk, event(1, 0))
		}
		return 200, nil
	}}
	r := watch(t, s, printerTarget)
	r.next(t)
	if ev := r.next(t); len(ev) != 1 {
		t.Fatalf("changed = %v, want the event after one transport error", ev)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, time.Second}
	if got := h.sleepCalls(); len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Errorf("sleeps = %v, want %v first", got, want)
	}
}
