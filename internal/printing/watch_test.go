package printing

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/ippnotify"
	"github.com/ThePhaseless/pagewire/internal/pappl"
	"github.com/ThePhaseless/pagewire/internal/pappl/papplfake"
)

// fakeQueue serves a papplfake system with one queue and returns the fake,
// the system client and a printing client on the queue's URI.
func fakeQueue(t *testing.T) (*papplfake.System, *pappl.Client, *Client) {
	t.Helper()
	fake := papplfake.New(papplfake.Options{})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	host := "ipp://" + strings.TrimPrefix(srv.URL, "http://")
	sys := pappl.New(host + "/ipp/system")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sys.CreatePrinter(ctx, "Q", "cups:q", "any", ""); err != nil {
		t.Fatal(err)
	}
	return fake, sys, New(host + "/ipp/print/" + papplfake.Resource("Q"))
}

// signal returns a changed func that never blocks and the channel it fills.
func signal() (func(), <-chan struct{}) {
	ch := make(chan struct{}, 16)
	return func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}, ch
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("no changed call: %s", what)
	}
}

func waitErr(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not return")
		return nil
	}
}

// papplfake hard-codes printer-state idle, so a Set-Printer-Attributes on the
// queue (printer-config-changed, as PAPPL emits on every Set) is the event.
func TestWatchPrinterConfigChanged(t *testing.T) {
	fake, sys, c := fakeQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed, ch := signal()
	done := make(chan error, 1)
	go func() { done <- c.Watch(ctx, changed) }()
	waitSignal(t, ch, "after subscribing")

	set := goipp.Attributes{goipp.MakeAttribute("printer-location", goipp.TagText, goipp.String("hall"))}
	if _, err := sys.SetDefaults(ctx, c.uri, set); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, ch, "after Set-Printer-Attributes")

	cancel()
	if err := waitErr(t, done); !errors.Is(err, context.Canceled) {
		t.Errorf("Watch = %v, want context.Canceled", err)
	}
	if n := fake.Subscriptions(); n != 0 {
		t.Errorf("subscriptions after cancel = %d, want 0", n)
	}
}

// A job moves the printer to processing and back: printer-state-changed.
func TestWatchPrinterStateChanged(t *testing.T) {
	_, _, c := fakeQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed, ch := signal()
	done := make(chan error, 1)
	go func() { done <- c.Watch(ctx, changed) }()
	waitSignal(t, ch, "after subscribing")

	cj := c.request(goipp.OpCreateJob)
	cj.Operation.Add(goipp.MakeAttribute("job-name", goipp.TagName, goipp.String("doc")))
	rsp, err := c.do(ctx, cj, nil)
	if err != nil {
		t.Fatal(err)
	}
	sd := c.request(goipp.OpSendDocument)
	sd.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(int32(ints(rsp.Job, "job-id")[0])))) //nolint:gosec // small test id
	sd.Operation.Add(goipp.MakeAttribute("last-document", goipp.TagBoolean, goipp.Boolean(true)))
	if _, err := c.do(ctx, sd, strings.NewReader("%PDF-")); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, ch, "after the job")

	cancel()
	if err := waitErr(t, done); !errors.Is(err, context.Canceled) {
		t.Errorf("Watch = %v, want context.Canceled", err)
	}
}

// A printer without IPP notifications, as go-mfp's IPP server.
func TestWatchUnsupported(t *testing.T) {
	c, _ := ippServer(t, func(req *goipp.Message) *goipp.Message {
		return goipp.NewResponse(goipp.DefaultVersion, goipp.StatusErrorOperationNotSupported, req.RequestID)
	})
	changed, ch := signal()
	if err := c.Watch(context.Background(), changed); !errors.Is(err, ippnotify.ErrUnsupported) {
		t.Errorf("Watch = %v, want ErrUnsupported", err)
	}
	if err := c.WatchJob(context.Background(), 1, changed); !errors.Is(err, ippnotify.ErrUnsupported) {
		t.Errorf("WatchJob = %v, want ErrUnsupported", err)
	}
	if len(ch) != 0 {
		t.Errorf("changed called %d times, want 0", len(ch))
	}
}

func TestWatchJobReturnsWhenCompleted(t *testing.T) {
	fake, _, c := fakeQueue(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Create-Job alone leaves the job pending, so it outlives the subscribe.
	cj := c.request(goipp.OpCreateJob)
	cj.Operation.Add(goipp.MakeAttribute("job-name", goipp.TagName, goipp.String("doc")))
	rsp, err := c.do(ctx, cj, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := ints(rsp.Job, "job-id")[0]

	changed, ch := signal()
	done := make(chan error, 1)
	go func() { done <- c.WatchJob(ctx, id, changed) }()
	waitSignal(t, ch, "after subscribing")

	sd := c.request(goipp.OpSendDocument)
	sd.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(int32(id)))) //nolint:gosec // small test id
	sd.Operation.Add(goipp.MakeAttribute("last-document", goipp.TagBoolean, goipp.Boolean(true)))
	if _, err := c.do(ctx, sd, strings.NewReader("%PDF-")); err != nil {
		t.Fatal(err)
	}
	if err := waitErr(t, done); err != nil {
		t.Errorf("WatchJob = %v, want nil after job-completed", err)
	}
	if len(ch) == 0 {
		t.Error("no changed call for the job events")
	}
	if n := fake.Subscriptions(); n != 0 {
		t.Errorf("subscriptions after completion = %d, want 0", n)
	}
}

func TestWatchJobGone(t *testing.T) {
	_, _, c := fakeQueue(t)
	changed, _ := signal()
	if err := c.WatchJob(context.Background(), 99, changed); !errors.Is(err, ippnotify.ErrRefused) {
		t.Errorf("WatchJob(unknown) = %v, want ErrRefused", err)
	}
}
