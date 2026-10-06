package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/config"
	"github.com/m0lte/numbers-station-listener/internal/directory"
	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
	"github.com/m0lte/numbers-station-listener/internal/rank"
	"github.com/m0lte/numbers-station-listener/internal/relay"
	"github.com/m0lte/numbers-station-listener/internal/schedule"
	"github.com/m0lte/numbers-station-listener/internal/stations"
)

// fakeProv lists one receiver and opens sessions that emit a 20 ms Opus
// silence packet every 20 ms until closed.
type fakeProv struct {
	mu       sync.Mutex
	sessions []*fakeSess
}

var testRx = model.Receiver{
	Provider: "fake", ID: "rx1", Callsign: "M0LTE", Name: "Test", Online: true,
	AntennaConnected: true, MaxClients: 10, AvailableClients: 9, MinHz: 10_000, MaxHz: 30_000_000,
	Lat: 51.46, Lon: -0.98, HasPos: true, PublicURL: "https://rx.example/",
}

func (p *fakeProv) ID() string { return "fake" }
func (p *fakeProv) Capabilities() provider.Capabilities {
	return provider.Capabilities{LiveSpectrum: true}
}
func (p *fakeProv) List(context.Context) ([]model.Receiver, error) {
	return []model.Receiver{testRx}, nil
}
func (p *fakeProv) Probe(context.Context, model.Receiver, int64, model.Mode) (provider.ProbeResult, error) {
	return provider.ProbeResult{}, nil
}
func (p *fakeProv) DeepLink(model.Receiver, int64, model.Mode) string { return "https://rx.example/" }
func (p *fakeProv) Spectrogram(context.Context, model.Receiver, int64, int, int) ([]byte, error) {
	return nil, provider.ErrUnsupported
}
func (p *fakeProv) Open(_ context.Context, _ model.Receiver, _ provider.OpenRequest) (provider.Session, error) {
	s := &fakeSess{audio: make(chan model.AudioPacket), spec: make(chan model.SpectrumRow), done: make(chan struct{}), stop: make(chan struct{})}
	go s.run()
	p.mu.Lock()
	p.sessions = append(p.sessions, s)
	p.mu.Unlock()
	return s, nil
}

func (p *fakeProv) openCount() (opened, live int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sessions {
		opened++
		select {
		case <-s.done:
		default:
			live++
		}
	}
	return
}

type fakeSess struct {
	audio chan model.AudioPacket
	spec  chan model.SpectrumRow
	done  chan struct{}
	stop  chan struct{}
	once  sync.Once
}

func (s *fakeSess) run() {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	defer close(s.done)
	defer close(s.spec)
	defer close(s.audio)
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			select {
			case s.audio <- model.AudioPacket{Opus: []byte{0xF8, 0xFF, 0xFE}, SampleRate: 12000, Channels: 1, Duration: 20 * time.Millisecond}:
			case <-s.stop:
				return
			}
		}
	}
}
func (s *fakeSess) Audio() <-chan model.AudioPacket    { return s.audio }
func (s *fakeSess) Spectrum() <-chan model.SpectrumRow { return s.spec }
func (s *fakeSess) Done() <-chan struct{}              { return s.done }
func (s *fakeSess) Err() error                         { return nil }
func (s *fakeSess) MaxDuration() time.Duration         { return 0 }
func (s *fakeSess) Close() error {
	s.once.Do(func() { close(s.stop) })
	<-s.done
	return nil
}

type fixedFetcher struct{ items []schedule.Item }

func (f fixedFetcher) Fetch(_ context.Context, from, to time.Time) ([]schedule.Item, error) {
	var out []schedule.Item
	for _, it := range f.items {
		if t, ok := it.Start.Time(); ok && !t.Before(from) && t.Before(to) {
			out = append(out, it)
		}
	}
	return out, nil
}

type rig struct {
	srv  *Server
	prov *fakeProv
	rel  *relay.Manager
	ev   schedule.Event
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newRig builds a server whose schedule holds one live V13 transmission.
// The schedule only fetches today and tomorrow (UTC), so a synctest bubble,
// whose clock starts at midnight, moves on an hour first.
func newRig(t *testing.T, ctx context.Context) *rig {
	t.Helper()
	if time.Now().UTC().Hour() == 0 {
		time.Sleep(time.Hour)
	}
	now := time.Now().UTC()
	start := now.Add(-2 * time.Minute).Truncate(time.Second)
	var item schedule.Item
	raw, _ := json.Marshal(map[string]any{
		"summary": "V13 15388kHz USB/AM [Target: East Asia]",
		"start":   map[string]string{"dateTime": start.Format("2006-01-02T15:04:05.000Z")},
	})
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	cat, err := stations.Parse([]byte(`{"V13":{"name":"New Star","priyomUrl":"https://priyom.org/v13","typicalDurationMin":55,"txSite":{"lat":23.7,"lon":121,"label":"Taiwan","confidence":"approximate"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	sched := schedule.New(schedule.Config{Fetcher: fixedFetcher{[]schedule.Item{item}}}, cat, quietLog())
	if err := sched.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	p := &fakeProv{}
	dir := directory.New([]provider.Provider{p}, directory.Options{Logger: quietLog()})
	dir.Refresh(ctx)
	rel := relay.New(relay.Config{}, quietLog(), p)
	cfg, _ := config.FromEnv()
	cfg.SpanHz = 12000
	srv := New(Deps{Config: cfg, Logger: quietLog(), Catalog: cat, Schedule: sched, Directory: dir, Relay: rel, Providers: []provider.Provider{p}, Weights: rank.DefaultWeights()})
	evs := sched.Events(now.Add(-time.Hour), now.Add(time.Hour))
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	return &rig{srv: srv, prov: p, rel: rel, ev: evs[0]}
}

func (r *rig) createChannel(t *testing.T) createChannelResp {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/channels", strings.NewReader(`{"eventId":"`+r.ev.ID+`"}`))
	r.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create channel: %d %s", rec.Code, rec.Body)
	}
	var resp createChannelResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestNowShowsLiveEvent(t *testing.T) {
	r := newRig(t, t.Context())
	rec := httptest.NewRecorder()
	r.srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/now", nil))
	var v NowView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Now) != 1 || v.Now[0].Station != "V13" || v.Now[0].Status != "live" {
		t.Fatalf("now = %+v", v.Now)
	}
	f := v.Now[0].Freqs
	if len(f) != 1 || f[0].Hz != 15_388_000 || len(f[0].Receivers) != 1 || f[0].Receivers[0].Callsign != "M0LTE" {
		t.Fatalf("freqs = %+v", f)
	}
	if v.ServerTime.IsZero() {
		t.Fatal("serverTime missing")
	}
	if opened, _ := r.prov.openCount(); opened != 0 {
		t.Fatal("viewing the schedule opened an upstream session")
	}
}

func TestCreateChannelOpensNothing(t *testing.T) {
	r := newRig(t, t.Context())
	resp := r.createChannel(t)
	if resp.ChannelID == "" || resp.ListenerID == "" || resp.Mode != "usb" || resp.FreqHz != 15_388_000 {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Event == nil || resp.Event.PriyomURL == nil {
		t.Fatal("channel response lost the Priyom attribution")
	}
	if opened, _ := r.prov.openCount(); opened != 0 {
		t.Fatal("creating a channel must not open upstream (no pre-warming)")
	}
}

// flushRecorder is a ResponseWriter for streaming handlers that can be made
// to fail, standing in for a browser that vanished.
type flushRecorder struct {
	mu      sync.Mutex
	h       http.Header
	code    int
	buf     bytes.Buffer
	failing bool
}

func (f *flushRecorder) Header() http.Header {
	if f.h == nil {
		f.h = http.Header{}
	}
	return f.h
}
func (f *flushRecorder) WriteHeader(c int) {
	f.mu.Lock()
	f.code = c
	f.mu.Unlock()
}
func (f *flushRecorder) snapshot() (int, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.code, append([]byte(nil), f.buf.Bytes()...)
}
func (f *flushRecorder) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing {
		return 0, errors.New("broken pipe")
	}
	return f.buf.Write(b)
}
func (f *flushRecorder) Flush() {}
func (f *flushRecorder) fail() {
	f.mu.Lock()
	f.failing = true
	f.mu.Unlock()
}

func TestAudioClientGoneReleasesUpstream(t *testing.T) {
	for _, how := range []string{"request cancelled", "write fails"} {
		t.Run(how, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, stop := context.WithCancel(t.Context())
				defer stop()
				r := newRig(t, ctx)
				go r.rel.Run(ctx)
				ch := r.createChannel(t)

				reqCtx, drop := context.WithCancel(ctx)
				req := httptest.NewRequest("GET", "/listen/"+ch.ChannelID+"/audio.webm?listener="+ch.ListenerID, nil).WithContext(reqCtx)
				w := &flushRecorder{}
				done := make(chan struct{})
				go func() { r.srv.Handler().ServeHTTP(w, req); close(done) }()

				time.Sleep(5 * time.Second)
				if _, live := r.prov.openCount(); live != 1 {
					t.Fatalf("live upstream sessions while listening = %d", live)
				}
				if code, body := w.snapshot(); code != 200 || !bytes.HasPrefix(body, []byte{0x1A, 0x45, 0xDF, 0xA3}) {
					t.Fatalf("expected a WebM stream, got %d with %d bytes", code, len(body))
				}

				if how == "request cancelled" {
					drop()
				} else {
					w.fail()
				}
				gone := time.Now()
				<-done
				time.Sleep(r.rel.Grace() - time.Millisecond)
				if _, live := r.prov.openCount(); live != 1 {
					t.Fatal("upstream closed before the grace period")
				}
				time.Sleep(2 * time.Millisecond)
				synctest.Wait()
				if _, live := r.prov.openCount(); live != 0 {
					t.Fatalf("upstream still open %v after the listener left", time.Since(gone))
				}
				drop()
				stop()
			})
		})
	}
}

func TestBeaconLeaveReleasesUpstream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		r := newRig(t, ctx)
		go r.rel.Run(ctx)
		ch := r.createChannel(t)
		req := httptest.NewRequest("GET", "/listen/"+ch.ChannelID+"/audio.webm?listener="+ch.ListenerID, nil).WithContext(ctx)
		w := &flushRecorder{}
		done := make(chan struct{})
		go func() { r.srv.Handler().ServeHTTP(w, req); close(done) }()
		time.Sleep(time.Second)

		rec := httptest.NewRecorder()
		r.srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/channels/"+ch.ChannelID+"/listeners/"+ch.ListenerID+"/leave", nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("leave = %d", rec.Code)
		}
		<-done // the beacon ends the stream even if the socket lingers
		time.Sleep(r.rel.Grace() + time.Millisecond)
		synctest.Wait()
		if _, live := r.prov.openCount(); live != 0 {
			t.Fatal("upstream still open after explicit leave plus grace")
		}
		stop()
	})
}

func TestAdminHiddenFromTunnel(t *testing.T) {
	r := newRig(t, t.Context())
	h := r.srv.Handler()

	req := httptest.NewRequest("GET", "/admin/sessions", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("admin from loopback = %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/admin/sessions", nil)
	req.RemoteAddr = "10.45.0.5:5555" // the cloudflared container is on the LAN
	req.Header.Set("Cf-Connecting-Ip", "203.0.113.9")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("admin via tunnel = %d, want 404", rec.Code)
	}

	req = httptest.NewRequest("GET", "/metrics", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("metrics from the internet = %d, want 404", rec.Code)
	}
}

func TestTuneHz(t *testing.T) {
	if got := TuneHz(15_388_000, model.ModeUSB, false); got != 15_388_000 {
		t.Fatal(got)
	}
	if got := TuneHz(8_175_000, model.ModeUSB, true); got != 8_175_000-rttyDialOffsetHz {
		t.Fatal(got)
	}
	if got := TuneHz(17_437_000, model.ModeCWU, false); got != 17_437_000 {
		t.Fatal(got)
	}
}

func TestPreferCountry(t *testing.T) {
	mk := func(id, cc string) rank.Candidate {
		return rank.Candidate{Receiver: model.Receiver{Provider: "fake", ID: id, Country: cc}}
	}
	in := []rank.Candidate{mk("py", "py"), mk("gb1", "gb"), mk("at", "at"), mk("gb2", "gb")}
	got := preferCountry(in, "gb")
	order := ""
	for _, c := range got {
		order += c.Receiver.ID + " "
	}
	if order != "gb1 gb2 py at " {
		t.Fatalf("order = %q", order)
	}
	if got[0].Reasons[len(got[0].Reasons)-1] != "in your country" {
		t.Fatalf("reasons = %v", got[0].Reasons)
	}
	if len(in[1].Reasons) != 0 {
		t.Fatal("input candidates were modified")
	}
	if g := preferCountry(in, "jp"); g[0].Receiver.ID != "py" {
		t.Fatal("no match must leave the ranking alone")
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Cf-Ipcountry", "GB")
	if listenerCountry(req) != "gb" {
		t.Fatal("header not read")
	}
	req.Header.Set("Cf-Ipcountry", "XX")
	if listenerCountry(req) != "" {
		t.Fatal("XX means unknown")
	}
}
