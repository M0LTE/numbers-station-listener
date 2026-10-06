// Package api is the HTTP surface: JSON API, live updates, the audio and
// spectrum relays, admin views and the embedded frontend.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/config"
	"github.com/m0lte/numbers-station-listener/internal/directory"
	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/probe"
	"github.com/m0lte/numbers-station-listener/internal/provider"
	"github.com/m0lte/numbers-station-listener/internal/rank"
	"github.com/m0lte/numbers-station-listener/internal/relay"
	"github.com/m0lte/numbers-station-listener/internal/schedule"
	"github.com/m0lte/numbers-station-listener/internal/stations"
)

// Deps are the services the API reads from.
type Deps struct {
	Config    config.Config
	Logger    *slog.Logger
	Catalog   *stations.Catalog
	Schedule  *schedule.Service
	Directory *directory.Service
	Prober    *probe.Prober // nil when probing is off
	Relay     *relay.Manager
	Providers []provider.Provider
	Weights   rank.Weights
	// PathOpen is the PSKReporter path-open hint; nil when off.
	PathOpen PathOpenFunc
	// Static is the built frontend; nil serves nothing at /.
	Static fs.FS
}

// PathOpenFunc scores whether the path from a transmitter site to a
// receiver is open on frequencies near freqHz (see internal/pskr).
type PathOpenFunc func(tx stations.Site, rx model.Receiver, freqHz int64, now time.Time) (score float64, reason string, ok bool)

// Server implements every route.
type Server struct {
	cfg       config.Config
	log       *slog.Logger
	catalog   *stations.Catalog
	sched     *schedule.Service
	dir       *directory.Service
	prober    *probe.Prober
	relay     *relay.Manager
	providers map[string]provider.Provider
	weights   rank.Weights
	pathOpen  PathOpenFunc
	static    fs.FS

	hub   *hub
	grams *gramCache

	nowMu   sync.Mutex
	nowJSON []byte // latest /api/now body without serverTime churn
	nowAt   time.Time
}

// New builds the server. Call Run to start its background refresher.
func New(d Deps) *Server {
	s := &Server{
		cfg: d.Config, log: d.Logger, catalog: d.Catalog, sched: d.Schedule,
		dir: d.Directory, prober: d.Prober, relay: d.Relay,
		providers: map[string]provider.Provider{}, weights: d.Weights, pathOpen: d.PathOpen, static: d.Static,
		hub: newHub(),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	for _, p := range d.Providers {
		s.providers[p.ID()] = p
	}
	s.grams = newGramCache(s.providers)
	return s
}

// Handler returns the full route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/now", s.handleNow)
	mux.HandleFunc("GET /api/schedule", s.handleSchedule)
	mux.HandleFunc("GET /api/live", s.handleLive)
	mux.HandleFunc("POST /api/channels", s.handleCreateChannel)
	mux.HandleFunc("DELETE /api/channels/{channel}/listeners/{listener}", s.handleLeave)
	mux.HandleFunc("POST /api/channels/{channel}/listeners/{listener}/leave", s.handleLeave)
	mux.HandleFunc("GET /listen/{channel}/audio.webm", s.handleAudio)
	mux.HandleFunc("GET /listen/{channel}/spectrum", s.handleSpectrum)
	mux.HandleFunc("GET /listen/{channel}/spectrogram.png", s.handleSpectrogram)
	mux.HandleFunc("GET /api/stations", s.handleStations)
	mux.HandleFunc("GET /api/stations/{file}", s.handleStationICS)
	mux.HandleFunc("GET /api/receivers", s.handleReceivers)
	mux.HandleFunc("GET /admin/sessions", s.adminOnly(s.handleAdminSessions))
	mux.HandleFunc("GET /metrics", s.adminOnly(s.handleMetrics))
	if s.static != nil {
		mux.Handle("GET /", s.staticHandler())
	}
	return securityHeaders(mux)
}

// Run keeps the /api/now snapshot fresh and pushes changes to SSE clients.
func (s *Server) Run(ctx context.Context) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	var last []byte
	var lastPush time.Time
	pending := false
	for {
		body := s.refreshNow(time.Now())
		if !bytes.Equal(body, last) {
			pending = true
		}
		if pending && time.Since(lastPush) >= 2*time.Second {
			s.hub.publish(body)
			last, lastPush, pending = body, time.Now(), false
		}
		var probeCh <-chan struct{}
		if s.prober != nil {
			probeCh = s.prober.Changed()
		}
		wait := tick.C
		var soon <-chan time.Time
		if pending {
			soon = time.After(2*time.Second - time.Since(lastPush))
		}
		select {
		case <-ctx.Done():
			s.hub.close()
			return
		case <-wait:
		case <-soon:
		case <-s.sched.Changed():
		case <-s.dir.Changed():
		case <-probeCh:
		}
	}
}

// refreshNow rebuilds the snapshot and returns its body, which compares
// equal across rebuilds when nothing but the clock moved.
func (s *Server) refreshNow(now time.Time) []byte {
	v := s.buildNow(now)
	v.ServerTime = time.Time{}
	body, _ := json.Marshal(v)
	s.nowMu.Lock()
	s.nowJSON, s.nowAt = body, now
	s.nowMu.Unlock()
	return body
}

// currentNow returns the snapshot with the server time filled in.
func (s *Server) currentNow() []byte {
	s.nowMu.Lock()
	body, at := s.nowJSON, s.nowAt
	s.nowMu.Unlock()
	if body == nil || time.Since(at) > 20*time.Second {
		body = s.refreshNow(time.Now())
	}
	return withServerTime(body, time.Now())
}

func withServerTime(body []byte, now time.Time) []byte {
	ts, _ := json.Marshal(now.UTC().Truncate(time.Second))
	// body starts with {"serverTime":"0001-01-01T00:00:00Z", ...
	const zero = `"serverTime":"0001-01-01T00:00:00Z"`
	return bytes.Replace(body, []byte(zero), append([]byte(`"serverTime":`), ts...), 1)
}

func (s *Server) handleNow(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(s.currentNow())
}

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	day := now.Truncate(24 * time.Hour)
	from, to := day, day.Add(48*time.Hour)
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_from", err.Error())
			return
		}
		from = t
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_to", err.Error())
			return
		}
		to = t
	}
	if to.Sub(from) > 72*time.Hour || !to.After(from) {
		writeErr(w, http.StatusBadRequest, "bad_range", "range must be positive and at most 72 h")
		return
	}
	evs := s.sched.Events(from, to)
	out := make([]EventView, 0, len(evs))
	for _, ev := range evs {
		out = append(out, s.eventView(ev, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// handleLive streams /api/now snapshots as Server-Sent Events. Holding it
// open is not listening: it never touches the relay.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	sub := s.hub.subscribe()
	defer s.hub.unsubscribe(sub)

	send := func(body []byte) bool {
		if _, err := w.Write([]byte("event: now\ndata: ")); err != nil {
			return false
		}
		if _, err := w.Write(body); err != nil {
			return false
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send(s.currentNow()) {
		return
	}
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case body, ok := <-sub:
			if !ok {
				return
			}
			if !send(withServerTime(body, time.Now())) {
				return
			}
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

type createChannelReq struct {
	EventID     string `json:"eventId"`
	FreqHz      int64  `json:"freqHz"`
	Mode        string `json:"mode"`
	ReceiverKey string `json:"receiverKey"`
}

type createChannelResp struct {
	ChannelID    string                 `json:"channelId"`
	ListenerID   string                 `json:"listenerId"`
	Receiver     ReceiverView           `json:"receiver"`
	FreqHz       int64                  `json:"freqHz"`
	TunedHz      int64                  `json:"tunedHz"`
	Mode         string                 `json:"mode"`
	SpanHz       int                    `json:"spanHz"`
	Capabilities provider.Capabilities  `json:"capabilities"`
	Alternatives []ReceiverView         `json:"alternatives"`
	Event        *channelEventAttribute `json:"event,omitempty"`
}

type channelEventAttribute struct {
	ID          string  `json:"id"`
	Station     string  `json:"station"`
	StationName string  `json:"stationName"`
	PriyomURL   *string `json:"priyomUrl"`
}

// handleCreateChannel resolves a receiver and returns a channel id. It
// never opens anything upstream; that happens when audio or spectrum
// attaches.
func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req createChannelReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_body", err.Error())
		return
	}
	now := time.Now()
	var (
		hz      = req.FreqHz
		mode    = req.Mode
		digital bool
		cands   []rank.Candidate
		evAttr  *channelEventAttribute
	)
	if req.EventID != "" {
		ev, ok := s.sched.Get(req.EventID)
		if !ok {
			writeErr(w, http.StatusNotFound, "no_event", "")
			return
		}
		if !ev.Parsed || ev.Search || len(ev.Freqs) == 0 {
			writeErr(w, http.StatusConflict, "not_listenable", "this entry has no frequency")
			return
		}
		if hz == 0 {
			hz = ev.Freqs[0]
		} else if !containsHz(ev.Freqs, hz) {
			writeErr(w, http.StatusBadRequest, "bad_freq", "frequency not part of this event")
			return
		}
		mode, digital = string(ev.Mode), ev.Digital
		st, _ := s.catalog.Lookup(ev.Station)
		at := ev.Start
		if now.After(at) {
			at = now
		}
		cands = s.candidates(ev, st, hz, at, true)
		ev2 := s.eventView(ev, now)
		evAttr = &channelEventAttribute{ID: ev.ID, Station: ev.Station, StationName: ev2.StationName, PriyomURL: ev2.PriyomURL}
	} else {
		if hz < 10_000 || hz > 30_000_000 {
			writeErr(w, http.StatusBadRequest, "bad_freq", "frequency must be 10 kHz to 30 MHz")
			return
		}
		cands = rank.Rank(rank.Input{Receivers: s.allowedReceivers(), FreqHz: hz, At: now, Held: s.relay.HeldOn, Weights: s.weights})
	}
	m, err := parseMode(mode)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_mode", err.Error())
		return
	}

	var chosen *rank.Candidate
	if req.ReceiverKey != "" {
		for i := range cands {
			if cands[i].Receiver.Key() == req.ReceiverKey {
				chosen = &cands[i]
				break
			}
		}
		if chosen == nil {
			rx, ok := s.dir.Get(req.ReceiverKey)
			switch {
			case !ok || !s.cfg.Allowed(rx.Callsign, rx.ID, rx.PublicURL):
				writeErr(w, http.StatusNotFound, "no_receiver", "")
				return
			case !rx.Covers(hz):
				writeErr(w, http.StatusConflict, "out_of_range", "receiver cannot tune that frequency")
				return
			default:
				writeErr(w, http.StatusConflict, "unavailable", "receiver is offline, full or not suitable")
				return
			}
		}
	} else if len(cands) > 0 {
		chosen = &cands[0]
	} else {
		writeErr(w, http.StatusNotFound, "no_receiver", "no suitable receiver is available")
		return
	}

	tuned := TuneHz(hz, m, digital)
	info, err := s.relay.Ensure(chosen.Receiver, tuned, m, s.cfg.SpanHz)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "relay", err.Error())
		return
	}
	resp := createChannelResp{
		ChannelID:    info.ID,
		ListenerID:   newID(),
		Receiver:     s.receiverView(chosen.Receiver, chosen, hz, m, digital),
		FreqHz:       hz,
		TunedHz:      tuned,
		Mode:         string(m),
		SpanHz:       s.cfg.SpanHz,
		Capabilities: s.providers[chosen.Receiver.Provider].Capabilities(),
		Alternatives: []ReceiverView{},
		Event:        evAttr,
	}
	for i := range cands {
		if cands[i].Receiver.Key() == chosen.Receiver.Key() {
			continue
		}
		if len(resp.Alternatives) >= receiversShow {
			break
		}
		resp.Alternatives = append(resp.Alternatives, s.receiverView(cands[i].Receiver, &cands[i], hz, m, digital))
	}
	writeJSON(w, http.StatusOK, resp)
}

func containsHz(fs []int64, hz int64) bool {
	for _, f := range fs {
		if f == hz {
			return true
		}
	}
	return false
}

func (s *Server) handleLeave(w http.ResponseWriter, r *http.Request) {
	n := s.relay.ReleaseListener(r.PathValue("channel"), r.PathValue("listener"))
	if n > 0 {
		s.log.Debug("listener left explicitly", "channel", r.PathValue("channel"), "leases", n)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStations(w http.ResponseWriter, _ *http.Request) {
	m := map[string]*stations.Station{}
	for _, st := range s.catalog.All() {
		m[st.Designator] = st
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stations": m,
		"attribution": map[string]string{
			"source":  "Priyom.org",
			"url":     "https://priyom.org",
			"licence": "CC BY-NC-SA 4.0",
			"note":    s.catalog.Meta.ReviewStatus,
		},
	})
}

func (s *Server) handleStationICS(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	station, ok := strings.CutSuffix(file, ".ics")
	if !ok || station == "" || len(station) > 12 {
		http.NotFound(w, r)
		return
	}
	now := time.Now().UTC()
	var evs []schedule.Event
	for _, ev := range s.sched.Events(now.Add(-24*time.Hour), now.Add(72*time.Hour)) {
		if strings.EqualFold(ev.Station, station) {
			evs = append(evs, ev)
		} else if st, ok := s.catalog.Lookup(ev.Station); ok && strings.EqualFold(st.Designator, station) {
			evs = append(evs, ev) // variants (F03j) appear in their base station's feed
		}
	}
	name, url := "", ""
	if st, ok := s.catalog.Lookup(station); ok {
		name, url = st.Name, st.PriyomURL
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=900")
	if err := schedule.WriteICS(w, station, evs, name, url); err != nil {
		s.log.Warn("ics write failed", "err", err)
	}
}

func (s *Server) handleReceivers(w http.ResponseWriter, _ *http.Request) {
	rxs := s.dir.Receivers()
	out := make([]ReceiverView, 0, len(rxs))
	for _, r := range rxs {
		out = append(out, s.receiverView(r, nil, 0, "", false))
	}
	var updated *time.Time
	if u := s.dir.Updated(); !u.IsZero() {
		updated = &u
	}
	writeJSON(w, http.StatusOK, map[string]any{"receivers": out, "updated": updated})
}

func (s *Server) handleAdminSessions(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{"relay": s.relay.Stats()}
	if s.prober != nil {
		rounds, probes := s.prober.Counts()
		out["probes"] = map[string]int64{"rounds": rounds, "probes": probes}
	}
	out["directory"] = s.dir.Status()
	out["sseClients"] = s.hub.count()
	writeJSON(w, http.StatusOK, out)
}

// adminOnly admits the admin networks, and never anything that came in
// through Cloudflare (the tunnel connector sits on the LAN, so its
// source address alone proves nothing).
func (s *Server) adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cf-Connecting-Ip") != "" || r.Header.Get("Cf-Ray") != "" {
			http.NotFound(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		ip = ip.Unmap()
		for _, p := range s.cfg.AdminNets {
			if p.Contains(ip) {
				h(w, r)
				return
			}
		}
		http.NotFound(w, r)
	}
}

func (s *Server) staticHandler() http.Handler {
	files := http.FileServerFS(s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(s.static, p); err != nil {
			if strings.HasPrefix(p, "assets/") || strings.Contains(path.Base(p), ".") {
				http.NotFound(w, r)
				return
			}
			// client-side route: serve the app
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; connect-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; frame-ancestors 'none'; base-uri 'self'")
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, kind, reason string) {
	body := map[string]string{"error": kind}
	if reason != "" {
		body["reason"] = reason
	}
	writeJSON(w, code, body)
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func isRejected(err error) bool {
	var rej *provider.RejectedError
	return errors.As(err, &rej)
}
