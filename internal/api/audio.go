package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/relay"
	"github.com/m0lte/numbers-station-listener/internal/webm"
)

// audioWriteTimeout bounds one write to a listener. A browser that stops
// reading (frozen tab, dead network) is dropped after this rather than when
// TCP eventually gives up.
const audioWriteTimeout = 10 * time.Second

// handleAudio streams the channel's audio as live WebM/Opus. The open
// response is what makes this browser a listener; every way out of this
// function releases the lease.
func (s *Server) handleAudio(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("channel")
	listener := r.URL.Query().Get("listener")
	if listener == "" {
		writeErr(w, http.StatusBadRequest, "no_listener", "listener query parameter required")
		return
	}
	lease, err := s.relay.Attach(r.Context(), id, listener, relay.KindAudio)
	if err != nil {
		code, reason := classify(err)
		status := http.StatusServiceUnavailable
		switch code {
		case "no_channel":
			status = http.StatusNotFound
		case "upstream":
			status = http.StatusBadGateway
		}
		if errors.Is(err, r.Context().Err()) {
			return // the browser gave up first
		}
		writeErr(w, status, code, reason)
		return
	}
	defer lease.Release()

	h := w.Header()
	h.Set("Content-Type", "audio/webm")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	var mux *webm.Muxer
	for {
		select {
		case <-r.Context().Done():
			return
		case <-lease.Done():
			return // channel failed for good; EOF tells the player to move on
		case p, ok := <-lease.Audio():
			if !ok {
				return
			}
			if mux == nil {
				ch := p.Channels
				if ch <= 0 {
					ch = 1
				}
				mux = webm.NewMuxer(w, webm.Config{Channels: ch, InputSampleRate: uint32(max(p.SampleRate, 0))})
			}
			_ = rc.SetWriteDeadline(time.Now().Add(audioWriteTimeout))
			if err := mux.WritePacket(p.Opus, p.Duration); err != nil {
				return // write failed: the listener is gone
			}
		}
	}
}
