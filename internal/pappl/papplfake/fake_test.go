package papplfake_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/ippnotify"
	"github.com/ThePhaseless/pagewire/internal/pappl"
	"github.com/ThePhaseless/pagewire/internal/pappl/papplfake"
	"github.com/ThePhaseless/pagewire/internal/printing"
)

// fake serves a papplfake system with one queue "q".
type fake struct {
	sys        *papplfake.System
	srv        *httptest.Server
	system     string // the system URI
	printerURI string // queue q's URI
}

func newFake(t *testing.T, o papplfake.Options) *fake {
	t.Helper()
	if o.NotifyWait == 0 {
		o.NotifyWait = 10 * time.Second
	}
	f := &fake{sys: papplfake.New(o)}
	f.srv = httptest.NewServer(f.sys)
	t.Cleanup(f.srv.Close)
	base := "ipp://" + f.srv.Listener.Addr().String()
	f.system, f.printerURI = base+"/ipp/system", base+"/ipp/print/q"
	if _, err := pappl.New(f.system).CreatePrinter(deadline(t), "q", "cups:/q", "drv", ""); err != nil {
		t.Fatal(err)
	}
	return f
}

func deadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func integer(name string, v int) goipp.Attribute {
	return goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(int32(v))) //nolint:gosec // test values
}

// call sends op to uri (named by uriAttr) with extra operation attributes and
// an optional subscription group, and returns the decoded answer.
func call(ctx context.Context, uri, uriAttr string, op goipp.Op, opAttrs, sub goipp.Attributes) (*goipp.Message, error) {
	m := goipp.NewRequest(goipp.DefaultVersion, op, 1)
	m.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	m.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	m.Operation.Add(goipp.MakeAttribute(uriAttr, goipp.TagURI, goipp.String(uri)))
	m.Operation = append(m.Operation, opAttrs...)
	m.Subscription = sub
	enc, err := m.EncodeBytes()
	if err != nil {
		return nil, err
	}
	httpURI := "http" + uri[len("ipp"):]
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpURI, bytes.NewReader(enc))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", goipp.ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	rsp := &goipp.Message{}
	return rsp, rsp.Decode(resp.Body)
}

func mustCall(t *testing.T, uri, uriAttr string, op goipp.Op, opAttrs, sub goipp.Attributes) *goipp.Message {
	t.Helper()
	rsp, err := call(deadline(t), uri, uriAttr, op, opAttrs, sub)
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	return rsp
}

func subGroup(events ...string) goipp.Attributes {
	ev := goipp.Attribute{Name: "notify-events"}
	for _, e := range events {
		ev.Values.Add(goipp.TagKeyword, goipp.String(e))
	}
	return goipp.Attributes{goipp.MakeAttribute("notify-pull-method", goipp.TagKeyword, goipp.String("ippget")), ev}
}

// subscribe creates a printer subscription on q (or a system one) and returns its id.
func (f *fake) subscribe(t *testing.T, system bool, events ...string) int {
	t.Helper()
	uri, attr, op := f.printerURI, "printer-uri", goipp.OpCreatePrinterSubscriptions
	if system {
		uri, attr, op = f.system, "system-uri", goipp.OpCreateSystemSubscriptions
	}
	rsp := mustCall(t, uri, attr, op, nil, subGroup(events...))
	ids := pappl.Ints(rsp.Subscription, "notify-subscription-id")
	if goipp.Status(rsp.Code) != goipp.StatusOk || len(ids) != 1 {
		t.Fatalf("%s = %s %v, want one subscription id", op, goipp.Status(rsp.Code), rsp.Subscription)
	}
	return ids[0]
}

func getNotifications(ctx context.Context, uri string, id, seq int) (*goipp.Message, error) {
	return call(ctx, uri, "printer-uri", goipp.OpGetNotifications, goipp.Attributes{
		integer("notify-subscription-ids", id), integer("notify-sequence-numbers", seq),
		goipp.MakeAttribute("notify-wait", goipp.TagBoolean, goipp.Boolean(true)),
	}, nil)
}

func events(m *goipp.Message) []goipp.Attributes {
	var out []goipp.Attributes
	for _, g := range m.AttrGroups() {
		if g.Tag == goipp.TagEventNotificationGroup {
			out = append(out, g.Attrs)
		}
	}
	return out
}

// waitFor polls cond every 5 ms for up to 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEventWakesWaitingGetNotifications(t *testing.T) {
	f := newFake(t, papplfake.Options{})
	id := f.subscribe(t, false, ippnotify.EventPrinterConfigChanged)
	type result struct {
		rsp *goipp.Message
		err error
		in  time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		rsp, err := getNotifications(deadline(t), f.printerURI, id, 1)
		done <- result{rsp, err, time.Since(start)}
	}()
	waitFor(t, "the held Get-Notifications", func() bool { return f.sys.GetNotificationsCalls() == 1 })
	// An unchanged value still emits printer-config-changed, as PAPPL does.
	if _, err := pappl.New(f.system).SetDefaults(deadline(t), f.printerURI, goipp.Attributes{
		goipp.MakeAttribute("sides-default", goipp.TagKeyword, goipp.String("one-sided")),
	}); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.in > 5*time.Second {
		t.Errorf("Get-Notifications took %v, want it woken by the event", r.in)
	}
	ev := events(r.rsp)
	if len(ev) != 1 || pappl.First(ev[0], "notify-subscribed-event") != ippnotify.EventPrinterConfigChanged ||
		pappl.Ints(ev[0], "notify-subscription-id")[0] != id || pappl.Ints(ev[0], "notify-sequence-number")[0] != 1 {
		t.Fatalf("events = %v, want one printer-config-changed with id %d, sequence 1", ev, id)
	}
	if pappl.Ints(r.rsp.Operation, "notify-get-interval") == nil {
		t.Error("no notify-get-interval with the events")
	}
	// Seen events are not sent again; the next ask without events and
	// without notify-wait answers at once and empty.
	rsp := mustCall(t, f.printerURI, "printer-uri", goipp.OpGetNotifications, goipp.Attributes{
		integer("notify-subscription-ids", id), integer("notify-sequence-numbers", 2),
	}, nil)
	if goipp.Status(rsp.Code) != goipp.StatusOk || len(events(rsp)) != 0 {
		t.Errorf("second Get-Notifications = %s with %d events, want ok and none", goipp.Status(rsp.Code), len(events(rsp)))
	}
}

func TestHeldGetNotificationsEndsWithRequest(t *testing.T) {
	f := newFake(t, papplfake.Options{NotifyWait: time.Hour})
	id := f.subscribe(t, true, ippnotify.EventPrinterDeleted)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := getNotifications(ctx, f.printerURI, id, 1) // a system subscription, asked on the queue path
		done <- err
	}()
	waitFor(t, "the held Get-Notifications", func() bool { return f.sys.GetNotificationsCalls() == 1 })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Get-Notifications = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Get-Notifications not ended by its context")
	}
	// The handler is gone too: Close waits for running handlers.
	closed := make(chan struct{})
	go func() { f.srv.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("a Get-Notifications handler is still blocked")
	}
}

func TestNoSubscriptions(t *testing.T) {
	f := newFake(t, papplfake.Options{NoSubscriptions: true})
	for _, c := range []struct {
		uri, attr string
		op        goipp.Op
	}{
		{f.system, "system-uri", goipp.OpCreateSystemSubscriptions},
		{f.printerURI, "printer-uri", goipp.OpCreatePrinterSubscriptions},
	} {
		rsp := mustCall(t, c.uri, c.attr, c.op, nil, subGroup(ippnotify.EventPrinterCreated))
		if st := goipp.Status(rsp.Code); st != goipp.StatusErrorOperationNotSupported {
			t.Errorf("%s = %s, want operation-not-supported", c.op, st)
		}
	}
}

func TestMaxSubscriptions(t *testing.T) {
	f := newFake(t, papplfake.Options{MaxSubscriptions: 1})
	f.subscribe(t, true, ippnotify.EventPrinterCreated)
	rsp := mustCall(t, f.system, "system-uri", goipp.OpCreateSystemSubscriptions, nil, subGroup(ippnotify.EventPrinterCreated))
	if st := goipp.Status(rsp.Code); st != goipp.StatusErrorIgnoredAllSubscriptions {
		t.Errorf("Create past the cap = %s, want ignored-all-subscriptions", st)
	}
	if n := f.sys.Subscriptions(); n != 1 {
		t.Errorf("Subscriptions() = %d, want 1", n)
	}
}

func TestCancelSubscription(t *testing.T) {
	f := newFake(t, papplfake.Options{})
	a := f.subscribe(t, true, ippnotify.EventPrinterCreated)
	f.subscribe(t, false, ippnotify.EventPrinterStateChanged)
	if n := f.sys.Subscriptions(); n != 2 {
		t.Fatalf("Subscriptions() = %d, want 2", n)
	}
	if rsp := mustCall(t, f.system, "system-uri", goipp.OpRenewSubscription, goipp.Attributes{integer("notify-subscription-id", a), integer("notify-lease-duration", 600)}, nil); goipp.Status(rsp.Code) != goipp.StatusOk {
		t.Errorf("Renew = %s, want ok", goipp.Status(rsp.Code))
	}
	if rsp := mustCall(t, f.system, "system-uri", goipp.OpCancelSubscription, goipp.Attributes{integer("notify-subscription-id", a)}, nil); goipp.Status(rsp.Code) != goipp.StatusOk {
		t.Fatalf("Cancel = %s, want ok", goipp.Status(rsp.Code))
	}
	if n := f.sys.Subscriptions(); n != 1 {
		t.Errorf("Subscriptions() after Cancel = %d, want 1", n)
	}
	for _, op := range []goipp.Op{goipp.OpRenewSubscription, goipp.OpCancelSubscription} {
		if rsp := mustCall(t, f.system, "system-uri", op, goipp.Attributes{integer("notify-subscription-id", a)}, nil); goipp.Status(rsp.Code) != goipp.StatusErrorNotFound {
			t.Errorf("%s on a cancelled subscription = %s, want not-found", op, goipp.Status(rsp.Code))
		}
	}
	rsp, err := getNotifications(deadline(t), f.printerURI, a, 1)
	if err != nil || goipp.Status(rsp.Code) != goipp.StatusErrorNotFound {
		t.Errorf("Get-Notifications on a cancelled subscription = %v, %v, want not-found", rsp, err)
	}
}

func TestReadBack(t *testing.T) {
	f := newFake(t, papplfake.Options{ReadBack: map[string]string{"media-default": "na_letter_8.5x11in"}})
	rejected, err := pappl.New(f.system).SetDefaults(deadline(t), f.printerURI, goipp.Attributes{
		goipp.MakeAttribute("media-default", goipp.TagKeyword, goipp.String("iso_a5_148x210mm")), // not even supported
	})
	if err != nil || len(rejected) > 0 {
		t.Fatalf("SetDefaults = %v, %v, want accepted", rejected, err)
	}
	if got := f.sys.Printers()[0].Defaults.Media; got != "na_letter_8.5x11in" {
		t.Errorf("stored media = %q, want the read-back value", got)
	}
}

func TestJobSubscription(t *testing.T) {
	f := newFake(t, papplfake.Options{})
	ctx := deadline(t)
	rsp := mustCall(t, f.printerURI, "printer-uri", goipp.OpCreateJob, nil, nil)
	jobID := pappl.Ints(rsp.Job, "job-id")[0]

	unknown := mustCall(t, f.printerURI, "printer-uri", goipp.OpCreateJobSubscriptions, goipp.Attributes{integer("notify-job-id", jobID+1)}, subGroup(ippnotify.EventJobCompleted))
	if st := goipp.Status(unknown.Code); st != goipp.StatusErrorNotFound {
		t.Errorf("job subscription on an unknown job = %s, want not-found", st)
	}

	var seen [][]goipp.Attributes
	done := make(chan error, 1)
	go func() {
		done <- ippnotify.Watch(ctx, ippnotify.Target{
			URI: f.printerURI, URIAttr: "printer-uri", Op: goipp.OpCreateJobSubscriptions, JobID: jobID,
			Events: []string{ippnotify.EventJobStateChanged, ippnotify.EventJobCompleted},
		}, func(ev []goipp.Attributes) { seen = append(seen, ev) })
	}()
	waitFor(t, "the job subscription", func() bool { return f.sys.Subscriptions() == 1 })
	if err := printing.New(f.printerURI).Cancel(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch = %v, want nil after the job ended", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return after the job ended")
	}
	if len(seen) < 2 || seen[0] != nil || pappl.Ints(seen[len(seen)-1][0], "job-state")[0] != 7 {
		t.Errorf("changed calls = %v, want nil, then events ending in job-state 7", seen)
	}
	if n := f.sys.Subscriptions(); n != 0 {
		t.Errorf("Subscriptions() = %d, want 0 after Watch cancelled it", n)
	}
}

func TestSystemWatchSeesExternalDelete(t *testing.T) {
	f := newFake(t, papplfake.Options{})
	ctx, cancel := context.WithCancel(deadline(t))
	changed := make(chan []goipp.Attributes, 10)
	done := make(chan error, 1)
	go func() {
		done <- ippnotify.Watch(ctx, ippnotify.Target{
			URI: f.system, URIAttr: "system-uri", Op: goipp.OpCreateSystemSubscriptions,
			Events: []string{ippnotify.EventPrinterCreated, ippnotify.EventPrinterDeleted, ippnotify.EventPrinterConfigChanged},
		}, func(ev []goipp.Attributes) { changed <- ev })
	}()
	if ev := <-changed; ev != nil {
		t.Fatalf("first changed = %v, want nil", ev)
	}
	f.sys.DeleteExternally("q")
	select {
	case ev := <-changed:
		if len(ev) != 1 || pappl.First(ev[0], "notify-subscribed-event") != ippnotify.EventPrinterDeleted {
			t.Errorf("events = %v, want printer-deleted", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event after DeleteExternally")
	}
	if n := len(f.sys.Printers()); n != 0 {
		t.Errorf("queues = %d, want 0", n)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Watch = %v, want context.Canceled", err)
	}
	if n := f.sys.Subscriptions(); n != 0 {
		t.Errorf("Subscriptions() = %d, want 0 after cancel", n)
	}
}
