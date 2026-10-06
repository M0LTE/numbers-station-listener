package ubersdr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
)

const testUA = "numbers-station-listener/test (+https://example.invalid; test)"

func fakeReceiver() model.Receiver {
	return model.Receiver{Provider: ID, ID: "fake", Callsign: "M0LTE", PublicURL: "http://fake.invalid/",
		BaseURL: "http://fake.invalid", MinHz: 10000, MaxHz: 30000000}
}

// fixtureScenario serves the first n captured audio frames and the captured
// spectrum stream behind a config message saying binBW Hz per bin.
func fixtureScenario(t testing.TB, n int, binBW float64) scenario {
	audio := binaries(readRecords(t, "audio-opus-v4-7910k-usb.rec"))[:n]
	var spec [][]byte
	for _, r := range readRecords(t, "spectrum-v2-binary8-7910k.rec") {
		if !isGzip(r.data) {
			spec = append(spec, r.data)
		}
	}
	return scenario{
		audioFrames: audio,
		specConfig: gz(t, map[string]any{"type": "config", "centerFreq": 7910000, "binCount": 1024,
			"binBandwidth": binBW, "totalBandwidth": binBW * 1024, "defaultBinCount": 1024, "defaultBinBandwidth": 29296.875}),
		specFrames: spec,
	}
}

func drainAudio(s provider.Session) int {
	n := 0
	for {
		select {
		case _, ok := <-s.Audio():
			if !ok {
				return n
			}
			n++
		default:
			return n
		}
	}
}

func drainRows(s provider.Session) []model.SpectrumRow {
	var out []model.SpectrumRow
	for {
		select {
		case r, ok := <-s.Spectrum():
			if !ok {
				return out
			}
			out = append(out, r)
		default:
			return out
		}
	}
}

func TestOpenAndClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeServer(t, fixtureScenario(t, 100, 10))
		defer f.shutdown()
		p := New(Options{UserAgent: testUA, HTTPClient: f.client(), Logger: quietLogger()})

		s, err := p.Open(context.Background(), fakeReceiver(), provider.OpenRequest{FreqHz: 7910000, Mode: model.ModeUSB, SpanHz: 12000})
		if err != nil {
			t.Fatal(err)
		}
		if s.MaxDuration() != time.Hour {
			t.Fatalf("MaxDuration %v", s.MaxDuration())
		}
		synctest.Wait()
		if n := drainAudio(s); n != 100 {
			t.Fatalf("%d audio packets, want 100", n)
		}
		rows := drainRows(s)
		// The fake sends one config (10 Hz/bin) and then all 28 captured
		// SPEC frames; it ignores our zoom, so every frame yields a row.
		if len(rows) != 28 {
			t.Fatalf("%d spectrum rows", len(rows))
		}
		if rows[0].BinHz != 10 || rows[0].CenterHz() != 7910000 {
			t.Fatalf("row geometry %+v", rows[0].BinHz)
		}

		start := time.Now()
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if el := time.Since(start); el > closeBound {
			t.Fatalf("Close took %v", el)
		}
		<-s.Done()
		synctest.Wait()
		if s.Err() != nil {
			t.Fatalf("Err after Close: %v", s.Err())
		}
		if _, ok := <-s.Audio(); ok {
			t.Fatal("audio channel still open")
		}
		if _, ok := <-s.Spectrum(); ok {
			t.Fatal("spectrum channel still open")
		}

		f.mu.Lock()
		defer f.mu.Unlock()
		if f.audioClose != websocket.StatusNormalClosure || f.specClose != websocket.StatusNormalClosure {
			t.Fatalf("close codes audio %v spectrum %v", f.audioClose, f.specClose)
		}
		if len(f.uas) != 3 {
			t.Fatalf("requests %v", f.paths)
		}
		for i, ua := range f.uas {
			if ua != testUA {
				t.Fatalf("request %s carried UA %q", f.paths[i], ua)
			}
		}
		if len(f.ids) != 1 {
			t.Fatalf("UUIDs registered: %v", f.ids)
		}
		for id := range f.ids {
			if !uuidRE.MatchString(id) {
				t.Fatalf("bad uuid %q", id)
			}
		}
		// 12 kHz does not fit 1024 x 10 Hz, so we asked for 20 Hz/bin.
		if len(f.specMessages) != 1 {
			t.Fatalf("spectrum messages %v", f.specMessages)
		}
		var zoom struct {
			Type         string  `json:"type"`
			Frequency    int64   `json:"frequency"`
			BinBandwidth float64 `json:"binBandwidth"`
		}
		if err := json.Unmarshal([]byte(f.specMessages[0]), &zoom); err != nil || zoom.Type != "zoom" || zoom.Frequency != 7910000 || zoom.BinBandwidth != 20 {
			t.Fatalf("zoom %q", f.specMessages[0])
		}
	})
}

// Keepalive: while audio flows, JSON pings go out on both sockets every
// 30 s.
func TestKeepalivePings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := fixtureScenario(t, 10, 20)
		sc.keepSending = true
		f := newFakeServer(t, sc)
		defer f.shutdown()
		p := New(Options{UserAgent: testUA, HTTPClient: f.client(), Logger: quietLogger()})
		s, err := p.Open(context.Background(), fakeReceiver(), provider.OpenRequest{FreqHz: 7910000, Mode: model.ModeUSB, SpanHz: 20000})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(65 * time.Second)
		synctest.Wait()
		select {
		case <-s.Done():
			t.Fatalf("session ended: %v", s.Err())
		default:
		}
		f.mu.Lock()
		audioMsgs, specMsgs := append([]string(nil), f.audioMessages...), append([]string(nil), f.specMessages...)
		f.mu.Unlock()
		for _, msgs := range [][]string{audioMsgs, specMsgs} {
			if len(msgs) != 2 {
				t.Fatalf("messages after 65 s: %v", msgs)
			}
			for _, m := range msgs {
				if m != `{"type":"ping"}` {
					t.Fatalf("unexpected message %q", m)
				}
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		<-s.Done()
	})
}

// Audio that stops is a dead session, reported as a plain error (not a
// refusal, so the relay may reconnect).
func TestStallEndsSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeServer(t, fixtureScenario(t, 10, 20))
		defer f.shutdown()
		p := New(Options{UserAgent: testUA, HTTPClient: f.client(), Logger: quietLogger()})
		s, err := p.Open(context.Background(), fakeReceiver(), provider.OpenRequest{FreqHz: 7910000, Mode: model.ModeUSB, SpanHz: 20000})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		<-s.Done()
		if el := time.Since(start); el < stallTimeout || el > stallTimeout+checkEvery+closeBound {
			t.Fatalf("stall detected after %v", el)
		}
		var rej *provider.RejectedError
		if err := s.Err(); err == nil || errors.As(err, &rej) || !strings.Contains(err.Error(), "no audio") {
			t.Fatalf("stall: %v", err)
		}
		synctest.Wait()
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.audioClose != websocket.StatusNormalClosure || f.specClose != websocket.StatusNormalClosure {
			t.Fatalf("close codes %v %v", f.audioClose, f.specClose)
		}
	})
}

// UberSDR kicks by dropping the TCP connection; /connection then answers
// 410 for the UUID, which turns the end into a RejectedError.
func TestKickIsRejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := fixtureScenario(t, 3, 20)
		sc.dropAudio = true
		f := newFakeServer(t, sc)
		defer f.shutdown()
		p := New(Options{UserAgent: testUA, HTTPClient: f.client(), Logger: quietLogger()})
		s, err := p.Open(context.Background(), fakeReceiver(), provider.OpenRequest{FreqHz: 7910000, Mode: model.ModeUSB, SpanHz: 20000})
		if err != nil {
			t.Fatal(err)
		}
		<-s.Done()
		var rej *provider.RejectedError
		if !errors.As(s.Err(), &rej) || !strings.Contains(rej.Reason, "kicked") {
			t.Fatalf("kick should be RejectedError, got %v", s.Err())
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close after end: %v", err)
		}
		if !errors.As(s.Err(), &rej) {
			t.Fatal("Close after the end must not erase the reason")
		}
		synctest.Wait()
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.specClose != websocket.StatusNormalClosure {
			t.Fatalf("spectrum not closed cleanly: %v", f.specClose)
		}
	})
}

// A peer that never answers the close handshake (nor reads it) cannot hold
// Close past its bound.
func TestCloseBoundedWhenPeerDeaf(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := fixtureScenario(t, 5, 20)
		sc.deaf = true
		f := newFakeServer(t, sc)
		defer f.shutdown()
		p := New(Options{UserAgent: testUA, HTTPClient: f.client(), Logger: quietLogger()})
		s, err := p.Open(context.Background(), fakeReceiver(), provider.OpenRequest{FreqHz: 7910000, Mode: model.ModeUSB, SpanHz: 20000})
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		start := time.Now()
		err = s.Close()
		el := time.Since(start)
		if el > closeBound {
			t.Fatalf("Close took %v, bound %v", el, closeBound)
		}
		if err != nil {
			t.Fatalf("Close: %v (forced close should still count as closed)", err)
		}
		if el != gracefulClose {
			t.Fatalf("Close returned after %v; expected the forced close at %v", el, gracefulClose)
		}
		<-s.Done()
		if s.Err() != nil {
			t.Fatalf("Err %v", s.Err())
		}
	})
}

func TestOpenRefusedOnSocket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := fixtureScenario(t, 0, 20)
		sc.audioText = `{"type":"error","error":"maximum unique users per IP reached (2)"}`
		f := newFakeServer(t, sc)
		defer f.shutdown()
		p := New(Options{UserAgent: testUA, HTTPClient: f.client(), Logger: quietLogger()})
		_, err := p.Open(context.Background(), fakeReceiver(), provider.OpenRequest{FreqHz: 7910000, Mode: model.ModeUSB})
		var rej *provider.RejectedError
		if !errors.As(err, &rej) || !strings.Contains(rej.Reason, "per IP") {
			t.Fatalf("got %v", err)
		}
		synctest.Wait()
	})
}

func TestOpenValidation(t *testing.T) {
	ct := &countingTransport{}
	p := New(Options{UserAgent: testUA, HTTPClient: &http.Client{Transport: ct}, Logger: quietLogger()})
	r := fakeReceiver()
	var rej *provider.RejectedError
	if _, err := p.Open(context.Background(), r, provider.OpenRequest{FreqHz: 31_000_000, Mode: model.ModeUSB}); !errors.As(err, &rej) {
		t.Fatalf("out of range: %v", err)
	}
	if _, err := p.Open(context.Background(), r, provider.OpenRequest{FreqHz: 7_000_000, Mode: "rtty"}); err == nil {
		t.Fatal("bad mode accepted")
	}
	if ct.n.Load() != 0 {
		t.Fatal("validation failures touched the network")
	}
}
