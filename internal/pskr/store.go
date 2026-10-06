package pskr

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/stations"
)

// Params tunes the rolling window and the path-open judgement. Zero
// fields take the defaults from DefaultParams.
type Params struct {
	// Window is how far back spots count. Default 15 min.
	Window time.Duration
	// Buckets is how many slices the window is kept in; spots leave the
	// window one slice at a time, so the effective window is between
	// Window*(Buckets-1)/Buckets and Window. Default 15 (one per minute).
	Buckets int
	// SenderRadiusKm: a sender counts as "near the transmitter" when its
	// square's centre is this close to the site. Default 800 km.
	SenderRadiusKm float64
	// ReceiverRadiusKm: a reporting receiver counts as "near this SDR"
	// when its square's centre is this close. Default 500 km.
	ReceiverRadiusKm float64
	// BandRatio: amateur bands with any part within this factor of the
	// station's frequency are consulted. Default 1.5.
	BandRatio float64
	// MinRegionSpots: with no path spots at all, the answer is only
	// "probably closed" (ok=true, ClosedScore) when, on one consulted
	// band, at least this many spots were sent from near the transmitter
	// and at least this many were heard near the SDR. Otherwise there is
	// too little data and PathOpen says ok=false. Default 10.
	MinRegionSpots int
	// ClosedScore is the score when both ends are active but nothing
	// crosses. It is above 0 because amateur FT8 at 100 W failing says
	// less about a broadcast-power numbers station. Default 0.1.
	ClosedScore float64
	// SaturationSpots sets how quickly the score rises with path spots n:
	// score = 1 - (1-ClosedScore)*exp(-n/SaturationSpots). With the
	// defaults 1 spot gives 0.26, 3 give 0.51, 10 give 0.88, 15 give
	// 0.96. Default 5.
	SaturationSpots float64
	// MaxEntries caps the store's memory: it is the most (band, sender
	// square, receiver square) counters held across all slices. A spot
	// that would need a new counter past the cap is dropped and counted
	// in Stats.Dropped. Each counter costs roughly 50 bytes all in, so
	// the default 400,000 bounds the store at about 20 MB.
	MaxEntries int
}

// DefaultParams returns the documented defaults.
func DefaultParams() Params {
	return Params{
		Window:           15 * time.Minute,
		Buckets:          15,
		SenderRadiusKm:   800,
		ReceiverRadiusKm: 500,
		BandRatio:        1.5,
		MinRegionSpots:   10,
		ClosedScore:      0.1,
		SaturationSpots:  5,
		MaxEntries:       400_000,
	}
}

func (p Params) withDefaults() Params {
	d := DefaultParams()
	if p.Window <= 0 {
		p.Window = d.Window
	}
	if p.Buckets <= 0 {
		p.Buckets = d.Buckets
	}
	if p.SenderRadiusKm <= 0 {
		p.SenderRadiusKm = d.SenderRadiusKm
	}
	if p.ReceiverRadiusKm <= 0 {
		p.ReceiverRadiusKm = d.ReceiverRadiusKm
	}
	if p.BandRatio < 1 {
		p.BandRatio = d.BandRatio
	}
	if p.MinRegionSpots <= 0 {
		p.MinRegionSpots = d.MinRegionSpots
	}
	if p.ClosedScore <= 0 || p.ClosedScore >= 1 {
		p.ClosedScore = d.ClosedScore
	}
	if p.SaturationSpots <= 0 {
		p.SaturationSpots = d.SaturationSpots
	}
	if p.MaxEntries <= 0 {
		p.MaxEntries = d.MaxEntries
	}
	return p
}

// Stats is a snapshot of the store's counters.
type Stats struct {
	// Accepted is every spot added to the window since start.
	Accepted uint64 `json:"accepted"`
	// Rejected spots were outside the HF bands or had an unusable
	// locator.
	Rejected uint64 `json:"rejected"`
	// Dropped spots were refused at the MaxEntries cap.
	Dropped uint64 `json:"dropped"`
	// InWindow is how many spots the window holds now.
	InWindow uint64 `json:"inWindow"`
	// Entries is how many counters the window holds now (see MaxEntries).
	Entries int `json:"entries"`
}

type pathKey struct {
	band uint8
	s, r cell
}

type bucket struct {
	num    int64
	counts map[pathKey]uint32
}

// Store is the rolling window of spots, aggregated as counts per (band,
// sender square, receiver square). It is safe for concurrent use. Every
// method takes the current time explicitly; the window runs on arrival
// time, never on the spot's own timestamp, because stations' clocks can be
// years out.
type Store struct {
	p     Params
	width int64 // bucket width, ns

	mu      sync.Mutex
	started bool
	cur     int64 // current bucket number
	ring    []bucket
	entries int
	// Running totals over the live buckets.
	path  []map[cell]map[cell]uint32 // per band: sender -> receiver -> spots
	sent  []map[cell]uint32          // per band: spots sent from a square
	heard []map[cell]uint32          // per band: spots heard in a square
	stats Stats
}

// NewStore makes an empty store.
func NewStore(p Params) *Store {
	p = p.withDefaults()
	width := int64(p.Window) / int64(p.Buckets)
	if width <= 0 {
		width = 1
	}
	s := &Store{
		p:     p,
		width: width,
		ring:  make([]bucket, p.Buckets),
		path:  make([]map[cell]map[cell]uint32, len(HFBands)),
		sent:  make([]map[cell]uint32, len(HFBands)),
		heard: make([]map[cell]uint32, len(HFBands)),
	}
	for i := range HFBands {
		s.path[i] = map[cell]map[cell]uint32{}
		s.sent[i] = map[cell]uint32{}
		s.heard[i] = map[cell]uint32{}
	}
	return s
}

// Params returns the effective parameters, defaults filled in.
func (s *Store) Params() Params { return s.p }

// Add counts one spot that arrived at now. It reports whether the spot was
// kept.
func (s *Store) Add(sp Spot, now time.Time) bool {
	b, okB := bandIndex(sp.Band, sp.FreqHz)
	sc, okS := gridCell(sp.SenderLocator)
	rc, okR := gridCell(sp.ReceiverLocator)

	s.mu.Lock()
	defer s.mu.Unlock()
	if !okB || !okS || !okR {
		s.stats.Rejected++
		return false
	}
	s.advance(now)
	slot := &s.ring[mod(s.cur, int64(len(s.ring)))]
	if slot.counts == nil || slot.num != s.cur {
		s.expire(slot)
		slot.num = s.cur
		slot.counts = map[pathKey]uint32{}
	}
	k := pathKey{band: uint8(b), s: sc, r: rc}
	if _, ok := slot.counts[k]; !ok {
		if s.entries >= s.p.MaxEntries {
			s.stats.Dropped++
			return false
		}
		s.entries++
	}
	slot.counts[k]++
	inner := s.path[b][sc]
	if inner == nil {
		inner = map[cell]uint32{}
		s.path[b][sc] = inner
	}
	inner[rc]++
	s.sent[b][sc]++
	s.heard[b][rc]++
	s.stats.Accepted++
	s.stats.InWindow++
	return true
}

// Stats returns the counters, after expiring anything older than the
// window at now.
func (s *Store) Stats(now time.Time) Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance(now)
	st := s.stats
	st.Entries = s.entries
	return st
}

// advance moves the current bucket forward to now, expiring buckets that
// have left the window. Time going backwards keeps the current bucket.
func (s *Store) advance(now time.Time) {
	n := floorDiv(now.UnixNano(), s.width)
	if s.started && n <= s.cur {
		return
	}
	s.started = true
	s.cur = n
	oldest := n - int64(len(s.ring)) + 1
	for i := range s.ring {
		if s.ring[i].counts != nil && s.ring[i].num < oldest {
			s.expire(&s.ring[i])
		}
	}
}

// expire removes a bucket's counts from the running totals.
func (s *Store) expire(b *bucket) {
	for k, c := range b.counts {
		inner := s.path[k.band][k.s]
		if inner[k.r] <= c {
			delete(inner, k.r)
			if len(inner) == 0 {
				delete(s.path[k.band], k.s)
			}
		} else {
			inner[k.r] -= c
		}
		decr(s.sent[k.band], k.s, c)
		decr(s.heard[k.band], k.r, c)
		s.stats.InWindow -= uint64(c)
	}
	s.entries -= len(b.counts)
	b.counts = nil
}

func decr(m map[cell]uint32, k cell, c uint32) {
	if m[k] <= c {
		delete(m, k)
	} else {
		m[k] -= c
	}
}

type bandTally struct {
	band        int
	path, sent  uint64
	heard       uint64
	informative bool
}

// PathOpen judges whether HF is open from the transmitter site to the
// receiver near freqHz, from the spots in the window at now. score is
// 0..1; ok is false when there is too little data to say anything (unknown
// positions, no amateur band near freqHz, or too few spots near either
// end), and the caller should then treat the signal as neutral. reason is
// set either way, for logs.
func (s *Store) PathOpen(tx stations.Site, rx model.Receiver, freqHz int64, now time.Time) (score float64, reason string, ok bool) {
	if !tx.Known() {
		return 0, "PSKReporter: transmitter site unknown", false
	}
	if !rx.HasPos {
		return 0, "PSKReporter: receiver location unknown", false
	}
	bands := bandsNear(freqHz, s.p.BandRatio)
	if len(bands) == 0 {
		return 0, fmt.Sprintf("PSKReporter: no amateur band near %d kHz", freqHz/1000), false
	}
	txCells := cellsWithin(*tx.Lat, *tx.Lon, s.p.SenderRadiusKm)
	rxCells := cellsWithin(rx.Lat, rx.Lon, s.p.ReceiverRadiusKm)
	rxSet := make([]uint64, (gridCells+63)/64)
	for _, c := range rxCells {
		rxSet[c/64] |= 1 << (c % 64)
	}

	tallies := make([]bandTally, len(bands))
	s.mu.Lock()
	s.advance(now)
	for i, b := range bands {
		t := &tallies[i]
		t.band = b
		for _, sc := range txCells {
			t.sent += uint64(s.sent[b][sc])
			inner := s.path[b][sc]
			if len(inner) == 0 {
				continue
			}
			if len(inner) <= len(rxCells) {
				for rc, n := range inner {
					if rxSet[rc/64]&(1<<(rc%64)) != 0 {
						t.path += uint64(n)
					}
				}
			} else {
				for _, rc := range rxCells {
					t.path += uint64(inner[rc])
				}
			}
		}
		for _, rc := range rxCells {
			t.heard += uint64(s.heard[b][rc])
		}
	}
	s.mu.Unlock()

	var total, sent, heard uint64
	var withSpots, active []string
	for _, t := range tallies {
		total += t.path
		sent += t.sent
		heard += t.heard
		if t.path > 0 {
			withSpots = append(withSpots, HFBands[t.band].Name)
		}
		need := uint64(s.p.MinRegionSpots)
		if t.sent >= need && t.heard >= need {
			active = append(active, HFBands[t.band].Name)
		}
	}
	win := windowText(s.p.Window)
	switch {
	case total > 0:
		score = 1 - (1-s.p.ClosedScore)*math.Exp(-float64(total)/s.p.SaturationSpots)
		noun := "spots"
		if total == 1 {
			noun = "spot"
		}
		return score, fmt.Sprintf("PSKReporter: %d %s on %s from near the transmitter to near this receiver in %s",
			total, noun, joinAnd(withSpots), win), true
	case len(active) > 0:
		return s.p.ClosedScore, fmt.Sprintf("PSKReporter: no spots on %s from near the transmitter to near this receiver in %s, though both ends are active",
			joinAnd(active), win), true
	case sent < uint64(s.p.MinRegionSpots) && heard < uint64(s.p.MinRegionSpots):
		return 0, "PSKReporter: too little activity near the transmitter and near this receiver", false
	case sent < uint64(s.p.MinRegionSpots):
		return 0, "PSKReporter: too little activity near the transmitter", false
	case heard < uint64(s.p.MinRegionSpots):
		return 0, "PSKReporter: too little activity near this receiver", false
	default:
		return 0, "PSKReporter: the two ends are active on different bands", false
	}
}

func windowText(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d min", int(d/time.Minute))
	}
	return d.String()
}

func joinAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func mod(a, b int64) int64 {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}
