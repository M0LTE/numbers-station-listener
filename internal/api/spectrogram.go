package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
)

// gramCache shares recorded-spectrogram images between all visitors. One
// fetch per (receiver, frequency, minutes) per gramTTL, with concurrent
// requests for the same key waiting on the one fetch in flight. The
// provider applies its own per-receiver upstream rate limit on top.
type gramCache struct {
	providers map[string]provider.Provider
	mu        sync.Mutex
	entries   map[string]*gramEntry
}

type gramEntry struct {
	done    chan struct{}
	png     []byte
	err     error
	fetched time.Time
}

const gramTTL = 60 * time.Second

func newGramCache(p map[string]provider.Provider) *gramCache {
	return &gramCache{providers: p, entries: map[string]*gramEntry{}}
}

func (g *gramCache) get(ctx context.Context, rx model.Receiver, hz int64, span, minutes int) ([]byte, time.Time, error) {
	key := rx.Key() + "|" + strconv.FormatInt(hz/1000, 10) + "|" + strconv.Itoa(minutes)
	g.mu.Lock()
	e, ok := g.entries[key]
	if ok {
		select {
		case <-e.done:
			if time.Since(e.fetched) > gramTTL {
				ok = false
			}
		default:
		}
	}
	if !ok {
		e = &gramEntry{done: make(chan struct{})}
		g.entries[key] = e
		for k, old := range g.entries { // drop stale entries
			select {
			case <-old.done:
				if time.Since(old.fetched) > 10*gramTTL {
					delete(g.entries, k)
				}
			default:
			}
		}
		g.mu.Unlock()
		go func() {
			fctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			p := g.providers[rx.Provider]
			if p == nil {
				e.err = provider.ErrUnsupported
			} else {
				e.png, e.err = p.Spectrogram(fctx, rx, hz, span, minutes)
			}
			e.fetched = time.Now()
			close(e.done)
		}()
	} else {
		g.mu.Unlock()
	}
	select {
	case <-e.done:
		return e.png, e.fetched, e.err
	case <-ctx.Done():
		return nil, time.Time{}, ctx.Err()
	}
}

func (s *Server) handleSpectrogram(w http.ResponseWriter, r *http.Request) {
	info, ok := s.relay.Info(r.PathValue("channel"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no_channel", "")
		return
	}
	p := s.providers[info.Receiver.Provider]
	if p == nil || !p.Capabilities().HistoricalSpectrogram {
		writeErr(w, http.StatusNotFound, "unsupported", "")
		return
	}
	minutes := 30
	if v := r.URL.Query().Get("minutes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_minutes", "")
			return
		}
		minutes = min(max(n, 5), 60)
	}
	png, at, err := s.grams.get(r.Context(), info.Receiver, info.FreqHz, info.SpanHz, minutes)
	switch {
	case errors.Is(err, provider.ErrUnsupported):
		writeErr(w, http.StatusNotFound, "unsupported", "")
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, "upstream", err.Error())
		return
	case len(png) == 0:
		writeErr(w, http.StatusNotFound, "no_data", "")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("Last-Modified", at.UTC().Format(http.TimeFormat))
	_, _ = w.Write(png)
}
