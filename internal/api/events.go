package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// heartbeat keeps idle event streams open through proxies (ingress).
const heartbeat = 25 * time.Second

// broker fans server events out to the open event streams.
type broker struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func (b *broker) subscribe() chan []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = map[chan []byte]struct{}{}
	}
	ch := make(chan []byte, 16)
	b.subs[ch] = struct{}{}
	return ch
}

func (b *broker) unsubscribe(ch chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, ch)
}

// publish sends an event to every subscriber. A subscriber that is not
// keeping up loses the event instead of blocking the server.
func (b *broker) publish(kind string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", kind, data))
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

// eventStream is GET /api/v1/events: Server-Sent Events. Each job start and
// end is sent as `event: job` with the job as data. The web UI and the HACS
// integration use it to refresh without polling.
func (s *Server) eventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := s.events.subscribe()
	defer s.events.unsubscribe(ch)

	_, _ = fmt.Fprint(w, "event: hello\ndata: {}\n\n")
	flusher.Flush()
	t := time.NewTicker(heartbeat)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		case <-t.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
