package controller

import "time"

// Time rules. The loop has no ticker: each rule that holds until an instant
// records it in the step's wakeAt, and the loop sleeps until the earliest.
const (
	startGrace   = 30 * time.Second // a young pod's connection-refused queue still shows starting
	replugDelay  = 30 * time.Second // a replug key restarts the pod after it held this long
	detectLimit  = 30 * time.Second // Detect restarts pods at most once per this
	agentGap     = 45 * time.Second // a report older than this shows "Device agent not reporting"
	endpointPoll = 60 * time.Second // the endpoint watchers' query interval, IPP notifications or not
	pendingWait  = 5 * time.Minute  // how long a pending or running pod is waited for before the first report

	retryMin = 30 * time.Second // the first Apply, caps and node-list retry
	retryMax = 5 * time.Minute
)

// wakeAt is the earliest future instant a step's time rules recorded; zero
// means none. A nil *wakeAt records nothing.
type wakeAt struct{ t time.Time }

// at keeps t when it is after now and earlier than the instant kept so far.
func (w *wakeAt) at(t, now time.Time) {
	if w != nil && t.After(now) && (w.t.IsZero() || t.Before(w.t)) {
		w.t = t
	}
}

// within reports whether now is before since+d, and records since+d then.
func (w *wakeAt) within(now, since time.Time, d time.Duration) bool {
	end := since.Add(d)
	if now.Before(end) {
		w.at(end, now)
		return true
	}
	return false
}

// silent reports whether a report that arrived at at shows as not reporting
// at now. The loop wakes at at+agentGap on its own (gapDeadline), so a
// heartbeat that moves at never needs a step.
func silent(now, at time.Time) bool { return !now.Before(at.Add(agentGap)) }

// retryDelay is the wait after fails consecutive failures: 30 s, doubling,
// capped at 5 min; 0 for no failure.
func retryDelay(fails int) time.Duration {
	if fails <= 0 {
		return 0
	}
	d := retryMin
	for i := 1; i < fails && d < retryMax; i++ {
		d *= 2
	}
	return min(d, retryMax)
}

// earliest is the earlier of two instants, ignoring zeros.
func earliest(a, b time.Time) time.Time {
	if a.IsZero() || !b.IsZero() && b.Before(a) {
		return b
	}
	return a
}
