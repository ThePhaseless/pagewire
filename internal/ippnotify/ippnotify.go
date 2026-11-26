// Package ippnotify watches an IPP printer, system or job through an ippget
// subscription (RFC 3995, RFC 3996): Create-*-Subscriptions, then a
// Get-Notifications long-poll with notify-wait, Renew-Subscription at half the
// lease and Cancel-Subscription on return. pappl (the agent's system watch) and
// printing (printer and job watches) share it.
package ippnotify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/OpenPrinting/goipp"
)

// Lease is the notify-lease-duration Watch asks for, in seconds. It is renewed
// at half-life; a short lease lets subscriptions leaked by a crash expire
// before they fill a server's cap (PAPPL allows 100).
const Lease = 600

// Event keywords (notify-events, notify-subscribed-event).
const (
	EventPrinterStateChanged      = "printer-state-changed"
	EventPrinterConfigChanged     = "printer-config-changed"
	EventPrinterMediaChanged      = "printer-media-changed"
	EventPrinterFinishingsChanged = "printer-finishings-changed"
	EventPrinterStopped           = "printer-stopped"
	EventPrinterRestarted         = "printer-restarted"
	EventPrinterShutdown          = "printer-shutdown"
	EventPrinterCreated           = "printer-created"
	EventPrinterDeleted           = "printer-deleted"
	EventJobStateChanged          = "job-state-changed"
	EventJobCompleted             = "job-completed"
)

var (
	// ErrUnsupported is permanent: the server has no ippget subscriptions for
	// this target (IPP operation-not-supported, attributes-or-values-not-supported
	// or bad-request; HTTP 404 or 405).
	ErrUnsupported = errors.New("ipp notifications not supported")
	// ErrRefused is temporary: the server refused the subscription (HTTP 401,
	// 403 or 426; a job that is already gone; any other IPP error, such as
	// client-error-ignored-all-subscriptions at the subscription cap; or OK
	// without notify-subscription-id). Retry after a Lease.
	ErrRefused = errors.New("ipp subscription refused")
)

// Target is what Watch subscribes to.
type Target struct {
	URI     string   // ipp:// or ipps:// URI the requests go to, sent as URIAttr
	URIAttr string   // "system-uri" or "printer-uri"
	Op      goipp.Op // OpCreateSystemSubscriptions, OpCreatePrinterSubscriptions or OpCreateJobSubscriptions
	JobID   int      // notify-job-id, for OpCreateJobSubscriptions
	Events  []string // notify-events keywords
}

// Package vars so tests skip real waits.
var (
	sleep = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	now = time.Now
)

const (
	callTimeout   = 45 * time.Second // per request; PAPPL holds a Get-Notifications at most 30 s
	cancelTimeout = 2 * time.Second
	maxBackoff    = 30 * time.Second
	heldAfter     = time.Second // a Get-Notifications this long was held by notify-wait
	// heldQuick quick empty answers in a row mean a server that held before
	// has stopped holding (an ipp-usb device that was slow only while busy);
	// fewer are PAPPL's spurious wakeups.
	heldQuick = 10
	// answeredTries bounds Create attempts the server answered without a
	// usable result (an unmapped HTTP status, a body that does not decode), so
	// callers fall back to polling. Unanswered attempts (dial, reset, timeout)
	// retry without a bound: the server may still be starting.
	answeredTries = 3
	maxResponse   = 8 << 20
)

// errGone means the subscription no longer exists (expired lease, restarted
// server): subscribe again.
var errGone = errors.New("subscription gone")

// errTransport means the request did not get an IPP answer worth acting on:
// retry it with backoff.
var errTransport = errors.New("transport error")

// errAnswered marks an errTransport the server answered (an unmapped HTTP
// status or an undecodable body), as opposed to no answer at all.
var errAnswered = errors.New("unusable answer")

// Watch subscribes to t and calls changed with the event groups of each
// Get-Notifications answer that holds any, and once with nil after every
// successful Create (events may have been missed before). Transport errors
// (dial, reset, timeout, HTTP 5xx, 408, 429 and other HTTP statuses not
// mapped below, an undecodable answer) retry with 1 s to 30 s backoff,
// keeping the subscription across Get-Notifications failures; a printer or
// system Create answered not-found (a queue being re-created) retries the
// same way. A Create the server answers unusably answeredTries times returns
// ErrRefused, so the caller polls. A Get-Notifications or Renew
// answered not-found subscribes again.
//
// Watch returns ctx.Err() when ctx ends, nil after a job target's event with
// a terminal job-state, and otherwise only errors wrapping ErrUnsupported or
// ErrRefused. Every return after a successful Create sends a best-effort
// Cancel-Subscription.
func Watch(ctx context.Context, t Target, changed func(events []goipp.Attributes)) error {
	if u, err := url.Parse(t.URI); err != nil || (u.Scheme != "ipp" && u.Scheme != "ipps") {
		return fmt.Errorf("watch %q: want an ipp:// or ipps:// URI: %w", t.URI, ErrUnsupported)
	}
	var bo, gone backoff // gone paces a subscription that is gone at once, across re-Creates
	for {
		id, err := subscribe(ctx, t, &bo)
		if err != nil {
			return err
		}
		changed(nil)
		err = poll(ctx, t, id, &bo, &gone, changed)
		if !errors.Is(err, errGone) {
			cancelSub(ctx, t, id)
			return err
		}
	}
}

// subscribe creates the subscription and returns its id, retrying transport
// errors and a printer or system not-found with backoff.
func subscribe(ctx context.Context, t Target, bo *backoff) (int, error) {
	answered := 0 // unusable answers so far
	for {
		m := request(t.Op, t)
		if t.Op == goipp.OpCreateJobSubscriptions {
			m.Operation.Add(goipp.MakeAttribute("notify-job-id", goipp.TagInteger, goipp.Integer(int32(t.JobID)))) //nolint:gosec // job ids are IPP integers
		}
		sub := goipp.Attributes{goipp.MakeAttribute("notify-pull-method", goipp.TagKeyword, goipp.String("ippget"))}
		if len(t.Events) > 0 {
			ev := goipp.Attribute{Name: "notify-events"}
			for _, e := range t.Events {
				ev.Values.Add(goipp.TagKeyword, goipp.String(e))
			}
			sub.Add(ev)
		}
		// PAPPL leases job subscriptions too (an hour without this), so a leaked
		// one expires as soon as a printer one.
		sub.Add(goipp.MakeAttribute("notify-lease-duration", goipp.TagInteger, goipp.Integer(Lease)))
		m.Groups = goipp.Groups{{Tag: goipp.TagOperationGroup, Attrs: m.Operation}, {Tag: goipp.TagSubscriptionGroup, Attrs: sub}}

		rsp, err := call(ctx, t, m)
		if err != nil && ctx.Err() != nil { // a body cut by cancellation is not an answer
			return 0, ctx.Err()
		}
		switch {
		case err == nil:
			for _, g := range rsp.AttrGroups() {
				if g.Tag == goipp.TagSubscriptionGroup {
					if id := intAttr(g.Attrs, "notify-subscription-id"); id > 0 {
						bo.reset()
						return id, nil
					}
				}
			}
			return 0, fmt.Errorf("%s %s: no notify-subscription-id: %w", t.Op, t.URI, ErrRefused)
		case errors.Is(err, errGone) && t.Op == goipp.OpCreateJobSubscriptions:
			return 0, fmt.Errorf("%s %s job %d: %w: %w", t.Op, t.URI, t.JobID, err, ErrRefused)
		case errors.Is(err, errAnswered) && answered+1 >= answeredTries:
			return 0, fmt.Errorf("%s (%d times): %w", err.Error(), answeredTries, ErrRefused) // keep the chain to ErrRefused only
		case errors.Is(err, errGone), errors.Is(err, errTransport):
			if errors.Is(err, errAnswered) {
				answered++
			}
			if err := sleep(ctx, bo.next()); err != nil {
				return 0, err
			}
		default:
			return 0, err
		}
	}
}

// poll runs the Get-Notifications loop for subscription id until ctx ends, a
// job target ends, the subscription is gone (errGone) or the server refuses.
func poll(ctx context.Context, t Target, id int, bo, gone *backoff, changed func([]goipp.Attributes)) error {
	job := t.Op == goipp.OpCreateJobSubscriptions
	seq := 0 // last notify-sequence-number seen
	renewed := now()
	answered := false // any Get-Notifications succeeded on this subscription
	held := false     // the server held a Get-Notifications since it last stopped holding
	quick := 0        // empty answers in a row under heldAfter
	interval := 0     // the last notify-get-interval seen, seconds
	for {
		if now().Sub(renewed) >= Lease/2*time.Second {
			m := request(goipp.OpRenewSubscription, t)
			m.Operation.Add(goipp.MakeAttribute("notify-subscription-id", goipp.TagInteger, goipp.Integer(int32(id)))) //nolint:gosec // ids are IPP integers
			m.Operation.Add(goipp.MakeAttribute("notify-lease-duration", goipp.TagInteger, goipp.Integer(Lease)))
			switch _, err := call(ctx, t, m); {
			case err == nil:
				renewed = now()
			case errors.Is(err, errGone):
				return errGone
			case ctx.Err() != nil:
				return ctx.Err()
			} // other errors: try again before the next Get-Notifications
		}

		m := request(goipp.OpGetNotifications, t)
		m.Operation.Add(goipp.MakeAttribute("notify-subscription-ids", goipp.TagInteger, goipp.Integer(int32(id))))    //nolint:gosec // ids are IPP integers
		m.Operation.Add(goipp.MakeAttribute("notify-sequence-numbers", goipp.TagInteger, goipp.Integer(int32(seq+1)))) //nolint:gosec // sequence numbers are IPP integers
		m.Operation.Add(goipp.MakeAttribute("notify-wait", goipp.TagBoolean, goipp.Boolean(true)))
		start := now()
		rsp, err := call(ctx, t, m)
		took := now().Sub(start)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch {
		case errors.Is(err, errTransport):
			if err := sleep(ctx, bo.next()); err != nil {
				return err
			}
			continue
		case errors.Is(err, errGone):
			if !answered { // gone at once: do not spin Create against a broken server
				if err := sleep(ctx, gone.next()); err != nil {
					return err
				}
			}
			return errGone
		case err != nil:
			return err
		}
		answered = true
		bo.reset()
		gone.reset()
		if n := intAttr(rsp.Operation, "notify-get-interval"); n > 0 {
			interval = n
		}
		if took >= heldAfter {
			held, quick = true, 0
		}
		var events []goipp.Attributes
		for _, g := range rsp.AttrGroups() {
			if g.Tag != goipp.TagEventNotificationGroup {
				continue
			}
			events = append(events, g.Attrs)
			if n := intAttr(g.Attrs, "notify-sequence-number"); n > seq {
				seq = n
			}
		}
		if len(events) > 0 {
			quick = 0
			changed(events)
			if job && terminal(events) {
				return nil
			}
			continue
		}
		if took >= heldAfter {
			continue
		}
		// An early empty answer from a server that held before is a spurious
		// wakeup (PAPPL wakes every waiter on any event): ask again at once,
		// until so many in a row show it stopped holding.
		quick++
		if held && quick >= heldQuick {
			held = false
		}
		if !held && quick >= 3 { // the server ignores notify-wait
			d := time.Duration(min(max(orDefault(interval, 30), 5), 60)) * time.Second
			if err := sleep(ctx, d); err != nil {
				return err
			}
		}
	}
}

// orDefault returns v, or def when v is 0.
func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// terminal reports whether an event carries job-state canceled, aborted or completed.
func terminal(events []goipp.Attributes) bool {
	for _, e := range events {
		if s := intAttr(e, "job-state"); s >= 7 && s <= 9 {
			return true
		}
	}
	return false
}

// cancelSub sends a best-effort Cancel-Subscription that outlives ctx.
func cancelSub(ctx context.Context, t Target, id int) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer cancel()
	m := request(goipp.OpCancelSubscription, t)
	m.Operation.Add(goipp.MakeAttribute("notify-subscription-id", goipp.TagInteger, goipp.Integer(int32(id)))) //nolint:gosec // ids are IPP integers
	_, _ = call(cctx, t, m)
}

var requestID atomic.Uint32

func request(op goipp.Op, t Target) *goipp.Message {
	m := goipp.NewRequest(goipp.DefaultVersion, op, requestID.Add(1))
	m.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	m.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	m.Operation.Add(goipp.MakeAttribute(t.URIAttr, goipp.TagURI, goipp.String(t.URI)))
	m.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("pagewire")))
	return m
}

// call posts m and maps the answer: nil for successful-*, errTransport,
// errGone for IPP not-found, or an error wrapping ErrUnsupported or ErrRefused.
func call(ctx context.Context, t Target, m *goipp.Message) (*goipp.Message, error) {
	op := goipp.Op(m.Code)
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	rsp, code, err := post(ctx, t.URI, m)
	switch {
	case err != nil && code != 0: // answered, but the body does not decode
		return nil, fmt.Errorf("%s %s: %w: %w: %w", op, t.URI, err, errTransport, errAnswered)
	case err != nil:
		return nil, fmt.Errorf("%s %s: %w: %w", op, t.URI, err, errTransport)
	case code == http.StatusUnauthorized, code == http.StatusForbidden, code == http.StatusUpgradeRequired:
		return nil, fmt.Errorf("%s %s: HTTP %d: %w", op, t.URI, code, ErrRefused)
	case code == http.StatusNotFound, code == http.StatusMethodNotAllowed:
		return nil, fmt.Errorf("%s %s: HTTP %d: %w", op, t.URI, code, ErrUnsupported)
	case code != http.StatusOK:
		return nil, fmt.Errorf("%s %s: HTTP %d: %w: %w", op, t.URI, code, errTransport, errAnswered)
	}
	st := goipp.Status(rsp.Code)
	switch {
	case st <= 0x00ff: // successful-*
		return rsp, nil
	case st == goipp.StatusErrorNotFound:
		return nil, fmt.Errorf("%s %s: %s: %w", op, t.URI, st, errGone)
	case st == goipp.StatusErrorOperationNotSupported, st == goipp.StatusErrorAttributesOrValues, st == goipp.StatusErrorBadRequest:
		return nil, fmt.Errorf("%s %s: %s: %w", op, t.URI, st, ErrUnsupported)
	default:
		return nil, fmt.Errorf("%s %s: %s: %w", op, t.URI, st, ErrRefused)
	}
}

// post sends m over HTTP and returns the decoded answer with the HTTP status;
// a non-200 answer comes back with a nil message. err is a transport error,
// with code 0 when nothing answered.
func post(ctx context.Context, uri string, m *goipp.Message) (*goipp.Message, int, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, 0, err
	}
	switch u.Scheme {
	case "ipp":
		u.Scheme = "http"
	case "ipps":
		u.Scheme = "https"
	default:
		return nil, 0, fmt.Errorf("uri %q: scheme must be ipp or ipps", uri)
	}
	if u.Port() == "" {
		u.Host += ":631"
	}
	enc, err := m.EncodeBytes()
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(enc))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", goipp.ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse)) // keep the connection reusable
		return nil, resp.StatusCode, nil
	}
	rsp := &goipp.Message{}
	if err := rsp.Decode(io.LimitReader(resp.Body, maxResponse)); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decode response: %w", err)
	}
	return rsp, resp.StatusCode, nil
}

func intAttr(attrs goipp.Attributes, name string) int {
	for _, a := range attrs {
		if a.Name == name && len(a.Values) > 0 {
			if n, ok := a.Values[0].V.(goipp.Integer); ok {
				return int(n)
			}
		}
	}
	return 0
}

// backoff doubles from 1 s to maxBackoff.
type backoff struct{ d time.Duration }

func (b *backoff) next() time.Duration {
	b.d = min(max(2*b.d, time.Second), maxBackoff)
	return b.d
}

func (b *backoff) reset() { b.d = 0 }
