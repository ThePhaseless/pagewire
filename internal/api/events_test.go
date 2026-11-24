package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ThePhaseless/printscan/internal/apitypes"
)

// sseClient reads frames ("event: x\ndata: y" or ": heartbeat") from /api/v1/events.
func sseClient(t *testing.T, e *testEnv) <-chan string {
	t.Helper()
	srv := httptest.NewServer(e.public)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	frames := make(chan string, 64)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		var cur []string
		for sc.Scan() {
			if sc.Text() == "" {
				frames <- strings.Join(cur, "\n")
				cur = nil
				continue
			}
			cur = append(cur, sc.Text())
		}
	}()
	return frames
}

func nextFrame(t *testing.T, frames <-chan string, match func(string) bool) string {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("stream closed")
			}
			if match(f) {
				return f
			}
		case <-timeout:
			t.Fatal("timed out waiting for an SSE frame")
		}
	}
}

func isEvent(name string) func(string) bool {
	return func(f string) bool { return strings.HasPrefix(f, "event: "+name+"\n") }
}

func TestSSEStatusAndHeartbeat(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.heartbeat = 20 * time.Millisecond })
	frames := sseClient(t, e)

	// The current Status comes first.
	first := nextFrame(t, frames, func(string) bool { return true })
	ev, data := parseFrame(t, []byte(first))
	if ev != apitypes.EventStatus {
		t.Fatalf("first frame %q", first)
	}
	validateDef(t, "Status", data)

	// A Status change from the source reaches the client.
	st := e.status.Current()
	st.Device.Phase = "ready"
	e.status.ch <- st
	f := nextFrame(t, frames, isEvent(apitypes.EventStatus))
	if _, data := parseFrame(t, []byte(f)); !strings.Contains(string(data), `"phase":"ready"`) {
		t.Fatalf("status event %s", f)
	}

	nextFrame(t, frames, func(f string) bool { return f == ": heartbeat" })
}

func TestSSEConfigAndScanEvents(t *testing.T) {
	scans := make(chan apitypes.Scan)
	e := newEnv(t, func(d *Deps) { d.ScanEvents = scans })
	frames := sseClient(t, e)
	nextFrame(t, frames, isEvent(apitypes.EventStatus))

	e.hub.Publish(apitypes.EventConfig, apitypes.ConfigEvent{Etag: "abc"})
	f := nextFrame(t, frames, isEvent(apitypes.EventConfig))
	if _, data := parseFrame(t, []byte(f)); string(data) != `{"etag":"abc"}` {
		t.Fatalf("config event %s", f)
	}

	scans <- apitypes.Scan{
		ID: "00000000000000aa", State: "scanning", Format: "pdf",
		Settings: apitypes.ScanSettings{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"},
	}
	f = nextFrame(t, frames, isEvent(apitypes.EventScan))
	_, data := parseFrame(t, []byte(f))
	validateDef(t, "Scan", data)
}

func TestScanEventsDrainedWithoutClients(t *testing.T) {
	scans := make(chan apitypes.Scan)
	newEnv(t, func(d *Deps) { d.ScanEvents = scans })
	for range 3 {
		select {
		case scans <- apitypes.Scan{}:
		case <-time.After(time.Second):
			t.Fatal("scan events are not drained")
		}
	}
}

func TestHubDropsSlowClient(t *testing.T) {
	h := NewHub()
	slow, cancelSlow := h.subscribe()
	defer cancelSlow()
	fast, cancelFast := h.subscribe()
	defer cancelFast()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range fast { //nolint:revive // drain
		}
	}()
	for range subscriberBuffer + 1 {
		h.Publish(apitypes.EventConfig, apitypes.ConfigEvent{Etag: "x"})
	}
	n := 0
	for range slow {
		n++
	}
	if n != subscriberBuffer {
		t.Fatalf("slow client got %d frames before being dropped, want %d", n, subscriberBuffer)
	}
	cancelFast()
	<-done
}
