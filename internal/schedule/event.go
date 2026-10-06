package schedule

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/stations"
)

// DefaultDuration is used for stations missing from the catalogue or with no
// typical duration recorded.
const DefaultDuration = 10 * time.Minute

// Event is one scheduled transmission from the Priyom feed.
type Event struct {
	// ID is stable across polls: the first 16 hex chars of a SHA-256 over
	// the start, station and sorted frequencies (see EventID).
	ID           string        `json:"id"`
	Station      string        `json:"station"`
	Start        time.Time     `json:"start"`
	Duration     time.Duration `json:"duration"`
	EndEstimated bool          `json:"endEstimated"`
	Freqs        []int64       `json:"freqs"`
	PriyomMode   string        `json:"priyomMode"`
	Mode         model.Mode    `json:"mode"`
	Digital      bool          `json:"digital"`
	Remarks      []string      `json:"remarks"`
	Target       string        `json:"target"`
	Search       bool          `json:"search"`
	Raw          string        `json:"raw"`
	Parsed       bool          `json:"parsed"`
	// ModeKnown is false when PriyomMode was not recognised (Mode is then a
	// USB fallback).
	ModeKnown bool `json:"modeKnown"`
}

// End is the catalogue-estimated end, Start + Duration.
func (e Event) End() time.Time { return e.Start.Add(e.Duration) }

// NewEvent builds an Event from one feed item. cat may be nil.
func NewEvent(start time.Time, summary string, cat *stations.Catalog) Event {
	p := ParseSummary(summary)
	ev := Event{
		Station:      p.Station,
		Start:        start.UTC(),
		Duration:     durationFor(cat, p.Station),
		EndEstimated: true,
		Freqs:        p.Freqs,
		PriyomMode:   p.Mode,
		Remarks:      p.Remarks,
		Target:       p.Target,
		Search:       p.Search,
		Raw:          summary,
		Parsed:       p.OK,
	}
	if p.OK && !p.Search {
		ev.Mode, ev.Digital, ev.ModeKnown = MapMode(p.Mode)
	} else {
		ev.Mode, ev.ModeKnown = model.ModeUSB, true
	}
	ev.ID = EventID(ev.Start, ev.Station, ev.Freqs)
	return ev
}

func durationFor(cat *stations.Catalog, station string) time.Duration {
	if cat == nil || station == "" {
		return DefaultDuration
	}
	if s, ok := cat.Lookup(station); ok && s.TypicalDurationMin != nil && *s.TypicalDurationMin > 0 {
		return time.Duration(*s.TypicalDurationMin) * time.Minute
	}
	return DefaultDuration
}

// EventID is the first 16 hex chars of SHA-256 over
// "<start RFC 3339>|<station>|<freqs in Hz, sorted, comma separated>".
func EventID(start time.Time, station string, freqs []int64) string {
	return hashID(start, station, freqs, "")
}

func hashID(start time.Time, station string, freqs []int64, extra string) string {
	sorted := slices.Clone(freqs)
	slices.Sort(sorted)
	var b strings.Builder
	b.WriteString(start.UTC().Format(time.RFC3339))
	b.WriteByte('|')
	b.WriteString(station)
	b.WriteByte('|')
	for i, f := range sorted {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(f, 10))
	}
	if extra != "" {
		b.WriteByte('|')
		b.WriteString(extra)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:16]
}

// Item is one entry of the Priyom feed as it arrives on the wire.
type Item struct {
	Summary string   `json:"summary"`
	Start   ItemTime `json:"start"`
}

// ItemTime is a Google-Calendar-style time: dateTime for timed entries,
// date for all-day ones.
type ItemTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
}

// Time parses the start. An all-day date is taken as 00:00 UTC.
func (t ItemTime) Time() (time.Time, bool) {
	if t.DateTime != "" {
		if v, err := time.Parse(time.RFC3339Nano, t.DateTime); err == nil {
			return v.UTC(), true
		}
	}
	if t.Date != "" {
		if v, err := time.Parse(time.DateOnly, t.Date); err == nil {
			return v.UTC(), true
		}
	}
	return time.Time{}, false
}

// BuildEvents turns feed items into events. Items with no usable start time
// are returned in skipped (an event cannot be placed without one). When two
// different items would get the same ID (same start, station and
// frequencies, for example two Search entries), the later one's ID also
// covers its raw summary, so no item is lost; an exact duplicate item is
// collapsed.
func BuildEvents(items []Item, cat *stations.Catalog) (events []Event, skipped []Item) {
	events, _, skipped = buildEvents(items, cat)
	return events, skipped
}

// buildEvents is BuildEvents plus the source item of each event.
func buildEvents(items []Item, cat *stations.Catalog) (events []Event, srcs, skipped []Item) {
	byID := map[string]int{}
	for _, it := range items {
		start, ok := it.Start.Time()
		if !ok {
			skipped = append(skipped, it)
			continue
		}
		ev := NewEvent(start, it.Summary, cat)
		if i, dup := byID[ev.ID]; dup {
			if events[i].Raw == ev.Raw {
				continue
			}
			for n := 2; ; n++ {
				ev.ID = hashID(ev.Start, ev.Station, ev.Freqs, ev.Raw+"#"+strconv.Itoa(n))
				if _, clash := byID[ev.ID]; !clash {
					break
				}
			}
		}
		byID[ev.ID] = len(events)
		events = append(events, ev)
		srcs = append(srcs, it)
	}
	return events, srcs, skipped
}

// Status is where an event is relative to now.
type Status string

const (
	StatusUpcoming Status = "upcoming"
	StatusLive     Status = "live"
	StatusDone     Status = "done"
)

// EffectiveEnd is when the event ends: the catalogue estimate, or earlier if
// a probe saw the carrier go (endedAt). endedAt is ignored unless it is after
// Start and before the estimated end.
func EffectiveEnd(ev Event, endedAt *time.Time) time.Time {
	end := ev.End()
	if endedAt != nil && endedAt.After(ev.Start) && endedAt.Before(end) {
		return *endedAt
	}
	return end
}

// StatusAt derives the status at now: upcoming before Start, live in
// [Start, end), done from end on, where end is EffectiveEnd.
func StatusAt(ev Event, now time.Time, endedAt *time.Time) Status {
	if now.Before(ev.Start) {
		return StatusUpcoming
	}
	if now.Before(EffectiveEnd(ev, endedAt)) {
		return StatusLive
	}
	return StatusDone
}
