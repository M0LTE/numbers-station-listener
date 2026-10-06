package schedule

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/stations"
)

func loadCatalog(t testing.TB) *stations.Catalog {
	t.Helper()
	c, err := stations.Load(repoFile(t, "data/stations.json"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var t0 = time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)

func TestEventID(t *testing.T) {
	sum := sha256.Sum256([]byte("2026-10-07T00:00:00Z|E11|4462000,5422000"))
	want := hex.EncodeToString(sum[:])[:16]
	if got := EventID(t0, "E11", []int64{5422000, 4462000}); got != want {
		t.Errorf("EventID = %s, want %s", got, want)
	}
	// Frequency order does not matter; start in another zone is the same instant.
	if EventID(t0.In(time.FixedZone("x", 3600)), "E11", []int64{4462000, 5422000}) != want {
		t.Error("EventID depends on frequency order or zone")
	}
	if EventID(t0, "E11", []int64{5422000}) == want || EventID(t0.Add(time.Minute), "E11", []int64{5422000, 4462000}) == want ||
		EventID(t0, "E07", []int64{5422000, 4462000}) == want {
		t.Error("EventID ignores an input")
	}
	ev := NewEvent(t0, "E11 5422, 4462kHz USB", nil)
	if ev.ID != want {
		t.Errorf("NewEvent ID = %s, want %s", ev.ID, want)
	}
}

func TestNewEventFields(t *testing.T) {
	cat := loadCatalog(t)
	cases := []struct {
		summary string
		dur     time.Duration
		mode    model.Mode
		digital bool
		known   bool
	}{
		{"V13 15388kHz USB/AM [Target: East Asia]", 55 * time.Minute, model.ModeUSB, false, true},
		{"F03j 12457kHz USB", 10 * time.Minute, model.ModeUSB, false, true}, // variant falls back to F03
		{"E11 13470kHz USB", 7 * time.Minute, model.ModeUSB, false, true},
		{"F06 8175kHz RTTY", 7 * time.Minute, model.ModeUSB, true, true},
		{"M12 14537kHz CW", 8 * time.Minute, model.ModeCWU, false, true},
		{"ZZ99 7000kHz LSB", DefaultDuration, model.ModeLSB, false, true}, // not in catalogue
		{"V28 7000kHz AM", DefaultDuration, model.ModeAM, false, true},    // in catalogue, no duration
		{"V13 15388kHz WOBBLE", 55 * time.Minute, model.ModeUSB, false, false},
		{"XPA2 Search", 10 * time.Minute, model.ModeUSB, false, true},
	}
	for _, c := range cases {
		ev := NewEvent(t0, c.summary, cat)
		if ev.Duration != c.dur || ev.Mode != c.mode || ev.Digital != c.digital || ev.ModeKnown != c.known {
			t.Errorf("%q: dur %v mode %q digital %v known %v; want %v %q %v %v",
				c.summary, ev.Duration, ev.Mode, ev.Digital, ev.ModeKnown, c.dur, c.mode, c.digital, c.known)
		}
		if !ev.EndEstimated || !ev.Parsed || ev.Raw != c.summary || !ev.End().Equal(t0.Add(c.dur)) {
			t.Errorf("%q: bad event %+v", c.summary, ev)
		}
	}
	if ev := NewEvent(t0, "V13 15388kHz USB", nil); ev.Duration != DefaultDuration {
		t.Errorf("nil catalogue duration = %v", ev.Duration)
	}
	ev := NewEvent(t0, "garbage here", cat)
	if ev.Parsed || ev.Raw != "garbage here" || ev.ID == "" {
		t.Errorf("unparsed event %+v", ev)
	}
}

func TestBuildEventsFixture(t *testing.T) {
	cat := loadCatalog(t)
	items := loadFixture(t)
	events, skipped := BuildEvents(items, cat)
	if len(events) != len(items) || len(skipped) != 0 {
		t.Fatalf("events %d skipped %d from %d items", len(events), len(skipped), len(items))
	}
	ids := map[string]bool{}
	for _, ev := range events {
		if ids[ev.ID] {
			t.Errorf("duplicate id %s (%q)", ev.ID, ev.Raw)
		}
		ids[ev.ID] = true
	}
	midnight := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	var v13 []Event
	for _, ev := range events {
		if ev.Station == "V13" && ev.Start.Equal(midnight) {
			v13 = append(v13, ev)
		}
	}
	if len(v13) != 3 || v13[0].ID == v13[1].ID || v13[1].ID == v13[2].ID || v13[0].ID == v13[2].ID {
		t.Fatalf("V13 at 00:00: %+v", v13)
	}
	for _, ev := range v13 {
		if ev.Target != "East Asia" || ev.Duration != 55*time.Minute || ev.PriyomMode != "USB/AM" {
			t.Errorf("V13 event %+v", ev)
		}
	}
	searches := 0
	for _, ev := range events {
		if ev.Search {
			searches++
		}
	}
	if searches != 5 {
		t.Errorf("search events = %d, want 5", searches)
	}
}

func TestBuildEventsCollisions(t *testing.T) {
	at := func(s string) ItemTime { return ItemTime{DateTime: s} }
	items := []Item{
		{"XPA2 Search", at("2026-10-06T06:30:00.000Z")},
		{"XPA2 Search (again)", at("2026-10-06T06:30:00.000Z")}, // same start, station, no freqs
		{"XPA2 Search", at("2026-10-06T06:30:00.000Z")},         // exact duplicate
		{"V13 15388kHz USB", ItemTime{Date: "2026-10-07"}},      // all-day: 00:00 UTC
		{"V13 15388kHz USB", ItemTime{DateTime: "not a time"}},  // unplaceable
		{"V13 15388kHz USB", ItemTime{}},
	}
	events, skipped := BuildEvents(items, nil)
	if len(events) != 3 || len(skipped) != 2 {
		t.Fatalf("events %d skipped %d", len(events), len(skipped))
	}
	if events[0].ID == events[1].ID {
		t.Error("colliding items share an id")
	}
	if events[0].ID != EventID(events[0].Start, "XPA2", nil) {
		t.Error("first of a collision should keep the plain id")
	}
	if !events[2].Start.Equal(time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("all-day start %v", events[2].Start)
	}
	// Stable across rebuilds.
	again, _ := BuildEvents(items, nil)
	for i := range events {
		if again[i].ID != events[i].ID {
			t.Error("ids not stable across builds")
		}
	}
}

func TestStatusAt(t *testing.T) {
	ev := Event{Start: t0, Duration: 20 * time.Minute}
	tp := func(d time.Duration) *time.Time { v := t0.Add(d); return &v }
	cases := []struct {
		name    string
		now     time.Duration
		endedAt *time.Time
		want    Status
	}{
		{"well before", -time.Hour, nil, StatusUpcoming},
		{"1ns before start", -1, nil, StatusUpcoming},
		{"at start", 0, nil, StatusLive},
		{"middle", 10 * time.Minute, nil, StatusLive},
		{"1ns before end", 20*time.Minute - 1, nil, StatusLive},
		{"at end", 20 * time.Minute, nil, StatusDone},
		{"after end", time.Hour, nil, StatusDone},
		{"ended early, before that", 4 * time.Minute, tp(5 * time.Minute), StatusLive},
		{"ended early, at that", 5 * time.Minute, tp(5 * time.Minute), StatusDone},
		{"ended early, after that", 6 * time.Minute, tp(5 * time.Minute), StatusDone},
		{"ended early but not started", -time.Minute, tp(5 * time.Minute), StatusUpcoming},
		{"endedAt before start ignored", 5 * time.Minute, tp(-time.Minute), StatusLive},
		{"endedAt at start ignored", 5 * time.Minute, tp(0), StatusLive},
		{"endedAt after estimate ignored", 20 * time.Minute, tp(25 * time.Minute), StatusDone},
	}
	for _, c := range cases {
		if got := StatusAt(ev, t0.Add(c.now), c.endedAt); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if got := EffectiveEnd(ev, tp(5*time.Minute)); !got.Equal(t0.Add(5 * time.Minute)) {
		t.Errorf("EffectiveEnd early = %v", got)
	}
	if got := EffectiveEnd(ev, nil); !got.Equal(t0.Add(20 * time.Minute)) {
		t.Errorf("EffectiveEnd = %v", got)
	}
}
