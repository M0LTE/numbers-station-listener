package api

import "sync"

// hub fans /api/now snapshots out to Server-Sent Event clients. A slow
// client misses intermediate snapshots rather than holding anyone up; it
// always gets the latest.
type hub struct {
	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	closed bool
}

func newHub() *hub { return &hub{subs: map[chan []byte]struct{}{}} }

func (h *hub) subscribe() chan []byte {
	c := make(chan []byte, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(c)
		return c
	}
	h.subs[c] = struct{}{}
	return c
}

func (h *hub) unsubscribe(c chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[c]; ok {
		delete(h.subs, c)
		close(c)
	}
}

func (h *hub) publish(body []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs {
		select {
		case c <- body:
		default:
			select {
			case <-c:
			default:
			}
			select {
			case c <- body:
			default:
			}
		}
	}
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for c := range h.subs {
		delete(h.subs, c)
		close(c)
	}
}
