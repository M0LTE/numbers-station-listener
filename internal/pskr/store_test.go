package pskr

import (
	"math"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/stations"
)

func spot(band, from, to string) Spot {
	return Spot{Band: band, SenderLocator: from, ReceiverLocator: to, SenderCall: "S1S", ReceiverCall: "R1R"}
}

func addN(t *testing.T, s *Store, n int, sp Spot, now time.Time) {
	t.Helper()
	for range n {
		if !s.Add(sp, now) {
			t.Fatalf("spot %+v refused", sp)
		}
	}
}

func TestStoreAcceptsAndRejects(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := NewStore(DefaultParams())
	for _, sp := range []Spot{
		spot("20m", "JO22", "IO91wm"),
		{FreqHz: 14_074_000, SenderLocator: "jo22", ReceiverLocator: "IO91"}, // band from frequency
		spot("20M", "JO22ab", "IO91"),
	} {
		if !s.Add(sp, now) {
			t.Errorf("%+v refused", sp)
		}
	}
	for _, sp := range []Spot{
		spot("6m", "JO22", "IO91"), // not HF
		{FreqHz: 50_313_000, SenderLocator: "JO22", ReceiverLocator: "IO91"},
		spot("", "JO22", "IO91"),    // no band at all
		spot("20m", "JO", "IO91"),   // field only
		spot("20m", "JO22", "ZZ99"), // invalid
	} {
		if s.Add(sp, now) {
			t.Errorf("%+v accepted", sp)
		}
	}
	st := s.Stats(now)
	if st.Accepted != 3 || st.Rejected != 5 || st.InWindow != 3 || st.Entries != 1 || st.Dropped != 0 {
		t.Fatalf("stats %+v", st)
	}
}

// Spots age out one bucket at a time on the fake clock: a spot counts for
// the 15 one-minute buckets starting with the one it arrived in.
func TestStoreExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewStore(DefaultParams())
		// The bubble's clock starts on a whole minute; move into it so the
		// bucket edges are not the same instants as the steps below.
		time.Sleep(10 * time.Second)
		addN(t, s, 3, spot("20m", "JO22", "IO91"), time.Now())
		time.Sleep(5 * time.Minute)
		addN(t, s, 2, spot("40m", "JO22", "IO91"), time.Now())
		addN(t, s, 1, spot("20m", "JO22", "IO91"), time.Now())
		if st := s.Stats(time.Now()); st.InWindow != 6 || st.Entries != 3 {
			t.Fatalf("after 5 min: %+v", st)
		}
		// 14 min 49 s after the first batch: still in its 15th bucket.
		time.Sleep(9*time.Minute + 49*time.Second)
		if st := s.Stats(time.Now()); st.InWindow != 6 {
			t.Fatalf("at 14:49: %+v", st)
		}
		// 14 min 50 s: the first batch's bucket has left the window.
		time.Sleep(time.Second)
		st := s.Stats(time.Now())
		if st.InWindow != 3 || st.Entries != 2 || st.Accepted != 6 {
			t.Fatalf("at 14:50: %+v", st)
		}
		if n := len(s.path[bandIdx(t, "20m")]); n != 1 {
			t.Fatalf("20m running total has %d senders, want 1", n)
		}
		time.Sleep(time.Hour)
		st = s.Stats(time.Now())
		if st.InWindow != 0 || st.Entries != 0 {
			t.Fatalf("after an hour: %+v", st)
		}
		for b := range HFBands {
			if len(s.path[b])+len(s.sent[b])+len(s.heard[b]) != 0 {
				t.Fatalf("band %s totals not empty after expiry", HFBands[b].Name)
			}
		}
	})
}

// A spot stamped earlier than the latest one seen goes into the current
// bucket rather than reopening an old one.
func TestStoreClockBackwards(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 12, 0, 30, 0, time.UTC)
	s := NewStore(DefaultParams())
	addN(t, s, 1, spot("20m", "JO22", "IO91"), t0.Add(10*time.Minute))
	addN(t, s, 1, spot("20m", "JO22", "IO91"), t0)
	if st := s.Stats(t0.Add(24 * time.Minute)); st.InWindow != 2 {
		t.Fatalf("both spots should still count: %+v", st)
	}
	if st := s.Stats(t0.Add(25 * time.Minute)); st.InWindow != 0 {
		t.Fatalf("both spots should have gone together: %+v", st)
	}
}

func TestStoreMaxEntries(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	p := DefaultParams()
	p.MaxEntries = 2
	s := NewStore(p)
	addN(t, s, 2, spot("20m", "JO22", "IO91"), now)
	addN(t, s, 1, spot("40m", "JO22", "IO91"), now)
	if s.Add(spot("20m", "JO22", "IO92"), now) {
		t.Fatal("third distinct counter accepted past the cap")
	}
	addN(t, s, 1, spot("20m", "JO22", "IO91"), now) // existing counter still counts
	// The cap is across buckets, so a new minute does not reset it...
	if s.Add(spot("20m", "JO22", "IO92"), now.Add(time.Minute)) {
		t.Fatal("cap reset by a new bucket")
	}
	// ...but expiry frees room.
	later := now.Add(20 * time.Minute)
	if !s.Add(spot("20m", "JO22", "IO92"), later) {
		t.Fatal("no room after expiry")
	}
	st := s.Stats(later)
	if st.Dropped != 2 || st.Entries != 1 || st.InWindow != 1 || st.Accepted != 5 {
		t.Fatalf("stats %+v", st)
	}
}

func bandIdx(t *testing.T, name string) int {
	t.Helper()
	i, ok := bandIndex(name, 0)
	if !ok {
		t.Fatalf("no band %s", name)
	}
	return i
}

func fp(v float64) *float64 { return &v }

// Transmitter near Utrecht (JO22), SDR near Reading (IO91), about 450 km
// apart. 14.5 MHz consults 30m, 20m, 17m and 15m.
var (
	txSite = stations.Site{Lat: fp(52.1), Lon: fp(5.1), Label: "test"}
	sdr    = model.Receiver{Provider: "ubersdr", ID: "x", Lat: 51.45, Lon: -0.97, HasPos: true}
	t0     = time.Date(2026, 10, 6, 12, 0, 30, 0, time.UTC)
)

const freq = 14_500_000

func TestPathOpenNotEnoughToSay(t *testing.T) {
	s := NewStore(DefaultParams())
	for _, tc := range []struct {
		name   string
		tx     stations.Site
		rx     model.Receiver
		hz     int64
		reason string
	}{
		{"unknown site", stations.Site{Label: "?"}, sdr, freq, "transmitter site unknown"},
		{"receiver without position", txSite, model.Receiver{ID: "y"}, freq, "receiver location unknown"},
		{"no band nearby", txSite, sdr, 77_500, "no amateur band near 77 kHz"},
		{"empty store", txSite, sdr, freq, "too little activity near the transmitter and near this receiver"},
	} {
		score, reason, ok := s.PathOpen(tc.tx, tc.rx, tc.hz, t0)
		if ok || score != 0 || reason != "PSKReporter: "+tc.reason {
			t.Errorf("%s: got %v %q %v", tc.name, score, reason, ok)
		}
	}

	// Only the transmitter end is active.
	addN(t, s, 20, spot("20m", "JO32", "FN42"), t0)
	if _, reason, ok := s.PathOpen(txSite, sdr, freq, t0); ok || reason != "PSKReporter: too little activity near this receiver" {
		t.Errorf("tx side only: %q %v", reason, ok)
	}
	// Receivers near the SDR are active, but on a band that is not
	// consulted, and on a consulted band other than the senders'.
	addN(t, s, 20, spot("80m", "FN42", "IO92"), t0)
	addN(t, s, 20, spot("15m", "FN42", "IO92"), t0)
	if _, reason, ok := s.PathOpen(txSite, sdr, freq, t0); ok || reason != "PSKReporter: the two ends are active on different bands" {
		t.Errorf("mismatched bands: %q %v", reason, ok)
	}
}

func TestPathOpenSpots(t *testing.T) {
	p := DefaultParams()
	s := NewStore(p)
	// Noise that must not count: right regions on an unconsulted band, a
	// sender far from the site, a receiver far from the SDR.
	addN(t, s, 50, spot("80m", "JO22", "IO91"), t0)
	addN(t, s, 50, spot("20m", "FN42", "IO91"), t0)
	addN(t, s, 50, spot("20m", "JO22", "FN42"), t0)
	score, reason, ok := s.PathOpen(txSite, sdr, freq, t0)
	if !ok || score != p.ClosedScore {
		t.Fatalf("both ends active, nothing across: %v %q %v", score, reason, ok)
	}
	if reason != "PSKReporter: no spots on 20m from near the transmitter to near this receiver in 15 min, though both ends are active" {
		t.Fatalf("reason %q", reason)
	}

	// One spot across, from a neighbouring square (JO32 is about 130 km
	// from the site) to one near the SDR (IO92).
	addN(t, s, 1, spot("20m", "JO32", "IO92"), t0)
	score1, reason, ok := s.PathOpen(txSite, sdr, freq, t0)
	if !ok || reason != "PSKReporter: 1 spot on 20m from near the transmitter to near this receiver in 15 min" {
		t.Fatalf("one spot: %v %q %v", score1, reason, ok)
	}
	if score1 <= p.ClosedScore || score1 >= 0.5 {
		t.Fatalf("one spot scores %v", score1)
	}
	addN(t, s, 13, spot("20m", "JO22", "IO91"), t0.Add(time.Minute))
	score14, reason, ok := s.PathOpen(txSite, sdr, freq, t0.Add(time.Minute))
	if !ok || reason != "PSKReporter: 14 spots on 20m from near the transmitter to near this receiver in 15 min" {
		t.Fatalf("14 spots: %q %v", reason, ok)
	}
	want := 1 - (1-p.ClosedScore)*math.Exp(-14/p.SaturationSpots)
	if math.Abs(score14-want) > 1e-12 || score14 < 0.9 {
		t.Fatalf("14 spots score %v, want %v", score14, want)
	}
	addN(t, s, 5, spot("17m", "JO22", "IO91"), t0.Add(time.Minute))
	addN(t, s, 2, spot("30m", "JO22", "IO91"), t0.Add(time.Minute))
	score21, reason, _ := s.PathOpen(txSite, sdr, freq, t0.Add(time.Minute))
	if reason != "PSKReporter: 21 spots on 30m, 20m and 17m from near the transmitter to near this receiver in 15 min" {
		t.Fatalf("reason %q", reason)
	}
	if !(score21 > score14 && score21 < 1) {
		t.Fatalf("score not rising and saturating: %v then %v", score14, score21)
	}
	// After the window the forward spots are gone and so is the verdict.
	if _, reason, ok := s.PathOpen(txSite, sdr, freq, t0.Add(16*time.Minute)); ok {
		t.Fatalf("still ok after the window: %q", reason)
	}
}

func TestPathOpenRadii(t *testing.T) {
	p := DefaultParams()
	p.SenderRadiusKm = 100
	p.ReceiverRadiusKm = 100
	s := NewStore(p)
	addN(t, s, 10, spot("20m", "JO32", "IO91"), t0) // sender 130 km away: out
	if _, reason, _ := s.PathOpen(txSite, sdr, freq, t0); strings.Contains(reason, "spots on 20m from") && !strings.HasPrefix(reason, "PSKReporter: no") {
		t.Fatalf("sender outside the radius counted: %q", reason)
	}
	addN(t, s, 10, spot("20m", "JO22", "IO91"), t0)
	if _, reason, ok := s.PathOpen(txSite, sdr, freq, t0); !ok || !strings.HasPrefix(reason, "PSKReporter: 10 spots on 20m") {
		t.Fatalf("got %q %v", reason, ok)
	}
	// The path is directional: the same spots say nothing about a site
	// near the SDR and a receiver near the site.
	rev := model.Receiver{Lat: *txSite.Lat, Lon: *txSite.Lon, HasPos: true}
	revSite := stations.Site{Lat: fp(sdr.Lat), Lon: fp(sdr.Lon)}
	if _, reason, ok := s.PathOpen(revSite, rev, freq, t0); ok {
		t.Fatalf("reverse path counted forward spots: %q", reason)
	}
	if p := NewStore(Params{}).Params(); p != DefaultParams() {
		t.Fatalf("zero Params should take the defaults, got %+v", p)
	}
}
