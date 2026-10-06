package probe

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
)

type scripted struct {
	mu    sync.Mutex
	calls map[string]int
	// answer gives the result for a receiver id
	answer func(id string) provider.ProbeResult
}

func (s *scripted) ID() string                          { return "fake" }
func (s *scripted) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (s *scripted) List(context.Context) ([]model.Receiver, error) {
	return nil, nil
}
func (s *scripted) Open(context.Context, model.Receiver, provider.OpenRequest) (provider.Session, error) {
	panic("a probe must never open a session")
}
func (s *scripted) DeepLink(model.Receiver, int64, model.Mode) string { return "" }
func (s *scripted) Spectrogram(context.Context, model.Receiver, int64, int, int) ([]byte, error) {
	return nil, provider.ErrUnsupported
}
func (s *scripted) Probe(_ context.Context, r model.Receiver, _ int64, _ model.Mode) (provider.ProbeResult, error) {
	s.mu.Lock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[r.ID]++
	ans := s.answer
	s.mu.Unlock()
	res := ans(r.ID)
	res.At = time.Now()
	return res, nil
}

func (s *scripted) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		n += c
	}
	return n
}

func rxs(ids ...string) []model.Receiver {
	var out []model.Receiver
	for _, id := range ids {
		out = append(out, model.Receiver{Provider: "fake", ID: id})
	}
	return out
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestTopKAndSharing(t *testing.T) {
	s := &scripted{answer: func(string) provider.ProbeResult { return provider.ProbeResult{OK: true} }}
	p := New(Options{TopK: 2, Logger: quiet()}, s)
	targets := []Target{
		{EventID: "a", FreqHz: 7_910_000, Candidates: rxs("r1", "r2", "r3")},
		// a second event on the same frequency shares the probes
		{EventID: "b", FreqHz: 7_910_000, Candidates: rxs("r1", "r2", "r3")},
	}
	p.Round(t.Context(), targets)
	if n := s.total(); n != 2 {
		t.Fatalf("probes = %d, want 2 (top 2, shared across events)", n)
	}
	if got := p.Signal("b", 7_910_000).State; got != Absent {
		t.Fatalf("state = %s", got)
	}
}

func TestVerdicts(t *testing.T) {
	s := &scripted{answer: func(id string) provider.ProbeResult {
		switch id {
		case "r1":
			return provider.ProbeResult{OK: false, Err: "rate limited"}
		case "r2":
			return provider.ProbeResult{OK: true, Present: true, SNR: 9}
		default:
			return provider.ProbeResult{OK: true, Present: true, SNR: 15}
		}
	}}
	p := New(Options{Logger: quiet()}, s)
	p.Round(t.Context(), []Target{{EventID: "a", FreqHz: 1, Candidates: rxs("r1", "r2", "r3")}})
	sig := p.Signal("a", 1)
	if sig.State != Present || sig.SNR != 15 || sig.ReceiverKey != "fake:r3" {
		t.Fatalf("signal = %+v, want present on r3 at 15 dB", sig)
	}

	s.answer = func(string) provider.ProbeResult { return provider.ProbeResult{OK: false} }
	p.Round(t.Context(), []Target{{EventID: "z", FreqHz: 2, Candidates: rxs("r1")}})
	if got := p.Signal("z", 2).State; got != Unknown {
		t.Fatalf("all probes failed: state = %s, want unknown", got)
	}
}

func TestEndsEarlyAfterCarrierGoes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		on := true
		s := &scripted{answer: func(string) provider.ProbeResult {
			mu.Lock()
			defer mu.Unlock()
			return provider.ProbeResult{OK: true, Present: on, SNR: 12}
		}}
		p := New(Options{Every: time.Minute, Logger: quiet()}, s)
		target := []Target{{EventID: "a", FreqHz: 1, Live: true, Candidates: rxs("r1")}}
		ctx, cancel := context.WithCancel(t.Context())
		go p.Run(ctx, func(time.Time) []Target { return target })

		time.Sleep(3*time.Minute + time.Second)
		if _, ended := p.EndedAt("a"); ended {
			t.Fatal("ended while present")
		}
		mu.Lock()
		on = false
		mu.Unlock()
		goneFrom := time.Now()
		time.Sleep(time.Minute) // one absent round: not yet
		synctest.Wait()
		if _, ended := p.EndedAt("a"); ended {
			t.Fatal("one absent round must not end the event")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		at, ended := p.EndedAt("a")
		if !ended {
			t.Fatal("two absent rounds should end the event")
		}
		if at.Before(goneFrom) || at.Sub(goneFrom) > time.Minute {
			t.Fatalf("ended at %v, carrier went at %v", at, goneFrom)
		}
		cancel()
	})
}

func TestNeverSeenIsNotEnded(t *testing.T) {
	s := &scripted{answer: func(string) provider.ProbeResult { return provider.ProbeResult{OK: true} }}
	p := New(Options{Logger: quiet()}, s)
	tg := []Target{{EventID: "a", FreqHz: 1, Live: true, Candidates: rxs("r1")}}
	for range 5 {
		p.Round(t.Context(), tg)
	}
	if _, ended := p.EndedAt("a"); ended {
		t.Fatal("an event never heard (late start, poor path) must not be ended by probes")
	}
}

// The core etiquette guarantee: probe volume depends on the schedule, not
// on visitors. Here there is nothing to probe, so nothing is probed.
func TestNoTargetsNoProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &scripted{answer: func(string) provider.ProbeResult { return provider.ProbeResult{OK: true} }}
		p := New(Options{Logger: quiet()}, s)
		ctx, cancel := context.WithCancel(t.Context())
		go p.Run(ctx, func(time.Time) []Target { return nil })
		time.Sleep(time.Hour)
		cancel()
		synctest.Wait()
		if n := s.total(); n != 0 {
			t.Fatalf("%d probes with nothing scheduled", n)
		}
	})
}
