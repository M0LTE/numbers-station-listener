package ubersdr

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/m0lte/numbers-station-listener/internal/config"
	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
)

// TestIntegrationM0LTE opens one short session against Tom's own receiver
// and closes it. It never runs unless NSL_INTEGRATION=1, and it can only
// reach M0LTE: the Allow function refuses everything else.
//
// There is deliberately no timeout: the session ends when enough audio
// (about 5 s) and spectrum have arrived. A hang is an honest failure.
func TestIntegrationM0LTE(t *testing.T) {
	if os.Getenv("NSL_INTEGRATION") != "1" {
		t.Skip("set NSL_INTEGRATION=1 to run against M0LTE")
	}
	ua := config.Config{Contact: "M0LTE"}.UserAgent()
	p := New(Options{
		UserAgent: ua,
		Allow:     func(cs, id, u string) bool { return strings.EqualFold(cs, "M0LTE") },
	})
	r := model.Receiver{
		Provider: ID, ID: "b838fc45-8dd2-4fa8-bb0d-8670244ad5da", Callsign: "M0LTE",
		PublicURL: "https://reading-ubersdr.m0lte.uk/", BaseURL: "https://reading-ubersdr.m0lte.uk",
		MinHz: 10_000, MaxHz: 30_000_000,
	}
	ctx := context.Background()

	s, err := p.Open(ctx, r, provider.OpenRequest{FreqHz: 7_910_000, Mode: model.ModeUSB, SpanHz: 12_000})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Logf("session open, max duration %v", s.MaxDuration())
	audio, rows := 0, 0
	var last model.SpectrumRow
	for audio < 250 || rows < 10 {
		select {
		case pkt, ok := <-s.Audio():
			if !ok {
				t.Fatalf("audio ended early: %v", s.Err())
			}
			if pkt.SampleRate != 12000 || len(pkt.Opus) == 0 {
				t.Fatalf("packet %+v", pkt)
			}
			audio++
		case row, ok := <-s.Spectrum():
			if !ok {
				t.Fatalf("spectrum ended early: %v", s.Err())
			}
			last = row
			rows++
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-s.Done()
	if s.Err() != nil {
		t.Fatalf("Err after Close: %v", s.Err())
	}
	t.Logf("%d audio packets, %d spectrum rows; last row %d bins at %.0f Hz/bin centred %.0f Hz",
		audio, rows, len(last.Levels), last.BinHz, last.CenterHz())
	if last.BinHz != 20 || last.CenterHz() != 7_910_000 {
		t.Fatalf("spectrum geometry %v Hz/bin centre %v", last.BinHz, last.CenterHz())
	}

	res, err := p.Probe(ctx, r, 7_910_000, model.ModeUSB)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	t.Logf("probe: %+v", res)
}
