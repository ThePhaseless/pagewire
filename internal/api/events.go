package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
)

// subscriberBuffer is how many events a slow SSE client may lag behind
// before it is dropped (it reconnects and gets the current Status).
const subscriberBuffer = 32

// Hub fans SSE events out to every connected client. Publish never blocks.
type Hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: map[chan []byte]struct{}{}} }

// Publish sends one event (apitypes.Event*) with payload v to every client.
// A client whose buffer is full is dropped.
func (h *Hub) Publish(event string, v any) {
	frame, err := sseFrame(event, v)
	if err != nil {
		slog.Error("encode SSE event", "event", event, "err", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- frame:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// subscribe returns a channel of encoded frames, closed when the client is
// dropped or cancel is called.
func (h *Hub) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, subscriberBuffer)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// sseFrame encodes one event; the payload goes through encodeJSON.
func sseFrame(event string, v any) ([]byte, error) {
	b, err := encodeJSON(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\ndata: ")
	buf.Write(b) // compact JSON has no newline
	buf.WriteString("\n\n")
	return buf.Bytes(), nil
}

// events streams the hub's events, starting with the current Status, plus a
// heartbeat comment.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	ch, cancel := s.hub.subscribe()
	defer cancel()
	first, err := sseFrame(apitypes.EventStatus, s.d.Status.Current())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal-error", err.Error())
		return
	}

	noWriteDeadline(w)
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(b []byte) bool {
		if _, err := w.Write(b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send(first) {
		return
	}

	t := time.NewTicker(s.d.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case frame, ok := <-ch:
			if !ok || !send(frame) {
				return
			}
		case <-t.C:
			if !send([]byte(": heartbeat\n\n")) {
				return
			}
		}
	}
}
