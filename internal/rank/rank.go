package rank

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
	"github.com/m0lte/numbers-station-listener/internal/stations"
)

// Weights sets how much each score component counts. They need not sum to
// 1; the score is divided by their sum so it stays in 0..1.
type Weights struct {
	Probe   float64 `json:"probe"`
	Path    float64 `json:"path"`
	Quality float64 `json:"quality"`
	Load    float64 `json:"load"`
	// PSKReporter is the later path-open signal (Input.PathOpen). It is 0
	// by default so it costs nothing until that feed exists.
	PSKReporter float64 `json:"pskreporter"`
}

// DefaultWeights: the probe is the only direct evidence the signal is
// there, so it carries half the score; path plausibility is the best guess
// without a probe; receiver quality and free slots break ties.
func DefaultWeights() Weights {
	return Weights{Probe: 0.5, Path: 0.3, Quality: 0.1, Load: 0.1}
}

func (w Weights) sum() float64 { return w.Probe + w.Path + w.Quality + w.Load + w.PSKReporter }

func (w Weights) validate() error {
	for name, v := range map[string]float64{"probe": w.Probe, "path": w.Path, "quality": w.Quality, "load": w.Load, "pskreporter": w.PSKReporter} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return fmt.Errorf("rank: weight %s must be a finite number >= 0, got %v", name, v)
		}
	}
	if w.sum() <= 0 {
		return errors.New("rank: at least one weight must be > 0")
	}
	return nil
}

// ParseWeights reads weights from JSON such as {"probe":0.6,"path":0.2}.
// Fields left out keep their defaults; an empty or blank string returns the
// defaults. Unknown fields, negative values and an all-zero set are errors.
func ParseWeights(s string) (Weights, error) {
	w := DefaultWeights()
	if strings.TrimSpace(s) == "" {
		return w, nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return Weights{}, fmt.Errorf("rank: weights: %w", err)
	}
	if dec.More() {
		return Weights{}, errors.New("rank: weights: trailing data after JSON object")
	}
	if err := w.validate(); err != nil {
		return Weights{}, err
	}
	return w, nil
}

// Scoring holds the thresholds that turn raw figures into components.
type Scoring struct {
	// Neutral is the component value when nothing is known.
	Neutral float64
	// ProbeUnknown is the probe component with no usable probe (none
	// yet, or it failed). It is evidence-based rather than Neutral: no
	// evidence scores nothing, so a strong probe outweighs every other
	// component together, which is what "the probe dominates" means.
	ProbeUnknown float64
	// ProbeSNRLowDB and ProbeSNRHighDB map a present probe's SNR linearly
	// onto ProbePresentFloor..1.
	ProbeSNRLowDB     float64
	ProbeSNRHighDB    float64
	ProbePresentFloor float64
	// ProbeAbsentFactor multiplies the whole score when a probe found
	// nothing there (the component itself is 0 as well).
	ProbeAbsentFactor float64
	// QualitySNRLowDB and QualitySNRHighDB map the receiver's own SNR
	// figure linearly onto 0..1. Values at or below 0 mean unknown (M0LTE
	// reports -1).
	QualitySNRLowDB  float64
	QualitySNRHighDB float64
}

// DefaultScoring: unprobed 0; a present probe's SNR 0 dB -> 0, 20 dB -> 1
// (20 dB above the neighbouring bins is a solid copy), so a present signal
// never ranks below an unprobed twin. An absent verdict also scores 0 and
// then halves the total. Receiver SNR 10 dB -> 0, 35 dB -> 1, which spreads
// the 9-45 dB seen across the live directory (median about 24 dB) over most
// of the range.
func DefaultScoring() Scoring {
	return Scoring{
		Neutral:           0.5,
		ProbeUnknown:      0,
		ProbeSNRLowDB:     0,
		ProbeSNRHighDB:    20,
		ProbePresentFloor: 0,
		ProbeAbsentFactor: 0.5,
		QualitySNRLowDB:   10,
		QualitySNRHighDB:  35,
	}
}

// Input is everything the ranker needs for one transmission.
type Input struct {
	Receivers []model.Receiver
	// Tx is the transmitter site; nil or unknown gives a neutral path score.
	Tx *stations.Site
	// Alternates are other possible transmitter sites; each receiver is
	// judged against whichever known site scores best for it.
	Alternates []stations.Site
	FreqHz     int64
	At         time.Time
	// Probes holds probe results for FreqHz keyed by receiver Key().
	Probes map[string]provider.ProbeResult
	// Held reports whether we already hold a relay session on a receiver;
	// nil means none.
	Held func(receiverKey string) bool
	// PathOpen is the hook for a later PSKReporter-style path-open signal:
	// a 0..1 score and a reason. nil, or ok=false, counts as neutral.
	PathOpen func(r model.Receiver) (score float64, reason string, ok bool)

	// Weights for the components; the zero value means DefaultWeights.
	Weights Weights
	// Path and Scoring override the defaults when non-nil.
	Path    *PathModel
	Scoring *Scoring
}

// Components is the per-component breakdown of a candidate's score, each
// 0..1 before weighting.
type Components struct {
	Probe       float64 `json:"probe"`
	Path        float64 `json:"path"`
	Quality     float64 `json:"quality"`
	Load        float64 `json:"load"`
	PSKReporter float64 `json:"pskreporter"`
}

// Candidate is a receiver that passed the hard filters, with its score.
type Candidate struct {
	Receiver model.Receiver
	// Score is 0..1, higher is better; only comparable within one Input.
	Score   float64
	Reasons []string
	// DistanceKm is from the chosen transmitter site; nil when either
	// end's position is unknown.
	DistanceKm *float64
	Components Components
}

// Rank filters and scores the receivers, best first. Ties keep a stable
// order by receiver key.
func Rank(in Input) []Candidate {
	w := in.Weights
	if w == (Weights{}) {
		w = DefaultWeights()
	}
	if w.validate() != nil {
		w = DefaultWeights()
	}
	pm := DefaultPathModel()
	if in.Path != nil {
		pm = *in.Path
	}
	sc := DefaultScoring()
	if in.Scoring != nil {
		sc = *in.Scoring
	}
	sites := knownSites(in.Tx, in.Alternates)

	out := make([]Candidate, 0, len(in.Receivers))
	for _, r := range in.Receivers {
		held := in.Held != nil && in.Held(r.Key())
		if !Eligible(r, in.FreqHz, held) {
			continue
		}
		out = append(out, score(r, held, in, w, pm, sc, sites))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Receiver.Key() < out[j].Receiver.Key()
	})
	return out
}

// Eligible applies the hard filters: online, freqHz in the tuning range, a
// free slot (waived when we already hold a session there), not overloaded,
// antenna connected.
func Eligible(r model.Receiver, freqHz int64, held bool) bool {
	return r.Online &&
		r.Covers(freqHz) &&
		(held || r.AvailableClients > 0) &&
		!Overloaded(r.LoadStatus) &&
		r.AntennaConnected
}

// Overloaded reports whether a provider load status means "do not use".
// "overloaded", "full" and "critical" (any case) do; anything else,
// including "ok", "warning" and "", does not.
func Overloaded(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "overloaded", "full", "critical":
		return true
	}
	return false
}

func knownSites(tx *stations.Site, alts []stations.Site) []stations.Site {
	var out []stations.Site
	if tx.Known() {
		out = append(out, *tx)
	}
	for i := range alts {
		if alts[i].Known() {
			out = append(out, alts[i])
		}
	}
	return out
}

func score(r model.Receiver, held bool, in Input, w Weights, pm PathModel, sc Scoring, sites []stations.Site) Candidate {
	c := Candidate{Receiver: r}
	absent := false

	// Probe.
	c.Components.Probe = sc.ProbeUnknown
	if p, ok := in.Probes[r.Key()]; ok {
		switch {
		case !p.OK:
			c.Reasons = append(c.Reasons, "probe failed")
		case p.Present:
			s := ramp(p.SNR, sc.ProbeSNRLowDB, sc.ProbeSNRHighDB)
			c.Components.Probe = sc.ProbePresentFloor + (1-sc.ProbePresentFloor)*s
			c.Reasons = append(c.Reasons, fmt.Sprintf("signal detected, %.0f dB SNR", p.SNR))
		default:
			c.Components.Probe = 0
			absent = true
			c.Reasons = append(c.Reasons, "no signal detected")
		}
	}

	// Path.
	c.Components.Path = sc.Neutral
	switch {
	case len(sites) == 0:
		c.Reasons = append(c.Reasons, "transmitter site unknown")
	case !r.HasPos:
		c.Reasons = append(c.Reasons, "receiver location unknown")
	default:
		var best PathResult
		bestIdx := -1
		for i, s := range sites {
			pr := pm.Score(*s.Lat, *s.Lon, r.Lat, r.Lon, in.FreqHz, in.At)
			if bestIdx < 0 || pr.Score > best.Score {
				best, bestIdx = pr, i
			}
		}
		c.Components.Path = best.Score
		d := best.DistanceKm
		c.DistanceKm = &d
		c.Reasons = append(c.Reasons, best.Reasons...)
		if len(sites) > 1 && sites[bestIdx].Label != "" {
			c.Reasons = append(c.Reasons, "best transmitter site: "+sites[bestIdx].Label)
		}
	}

	// Receiver quality. HasSNR with a non-positive figure is "unknown".
	c.Components.Quality = sc.Neutral
	if r.HasSNR && r.SNR > 0 {
		c.Components.Quality = ramp(r.SNR, sc.QualitySNRLowDB, sc.QualitySNRHighDB)
		switch {
		case c.Components.Quality >= 0.6:
			c.Reasons = append(c.Reasons, fmt.Sprintf("good receiver SNR (%.0f dB)", r.SNR))
		case c.Components.Quality <= 0.2:
			c.Reasons = append(c.Reasons, fmt.Sprintf("low receiver SNR (%.0f dB)", r.SNR))
		}
	}

	// Load: joining a session we already hold costs no slot.
	switch {
	case held:
		c.Components.Load = 1
		c.Reasons = append(c.Reasons, "already relaying from this receiver")
	case r.MaxClients > 0:
		c.Components.Load = math.Max(0, math.Min(1, float64(r.AvailableClients)/float64(r.MaxClients)))
		c.Reasons = append(c.Reasons, slots(r.AvailableClients))
	default:
		c.Components.Load = sc.Neutral
		c.Reasons = append(c.Reasons, slots(r.AvailableClients))
	}

	// Later: PSKReporter path-open signal.
	c.Components.PSKReporter = sc.Neutral
	if w.PSKReporter > 0 && in.PathOpen != nil {
		if s, reason, ok := in.PathOpen(r); ok {
			c.Components.PSKReporter = math.Max(0, math.Min(1, s))
			if reason != "" {
				c.Reasons = append(c.Reasons, reason)
			}
		}
	}

	total := w.Probe*c.Components.Probe + w.Path*c.Components.Path + w.Quality*c.Components.Quality +
		w.Load*c.Components.Load + w.PSKReporter*c.Components.PSKReporter
	total /= w.sum()
	if absent {
		total *= sc.ProbeAbsentFactor
	}
	c.Score = math.Max(0, math.Min(1, total))
	return c
}

func slots(n int) string {
	if n == 1 {
		return "1 free slot"
	}
	return fmt.Sprintf("%d free slots", n)
}

// ramp maps v linearly from [lo, hi] onto [0, 1], clamped.
func ramp(v, lo, hi float64) float64 {
	if hi <= lo {
		if v >= hi {
			return 1
		}
		return 0
	}
	return math.Max(0, math.Min(1, (v-lo)/(hi-lo)))
}
