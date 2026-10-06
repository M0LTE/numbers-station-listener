package rank

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Point is one knot of a piecewise-linear distance curve: the plausibility
// score (0..1) of a path of DistanceKm on some band and time of day.
type Point struct {
	DistanceKm float64
	Score      float64
}

// Curve is a piecewise-linear plausibility curve over distance. Points must
// be in increasing distance order; the score is held flat before the first
// point and after the last.
type Curve []Point

// At evaluates the curve at a distance.
func (c Curve) At(km float64) float64 {
	if len(c) == 0 {
		return 0.5
	}
	if km <= c[0].DistanceKm {
		return c[0].Score
	}
	for i := 1; i < len(c); i++ {
		if km <= c[i].DistanceKm {
			a, b := c[i-1], c[i]
			if b.DistanceKm == a.DistanceKm {
				return b.Score
			}
			f := (km - a.DistanceKm) / (b.DistanceKm - a.DistanceKm)
			return a.Score + f*(b.Score-a.Score)
		}
	}
	return c[len(c)-1].Score
}

// Anchor is the distance curve that applies at one frequency. Between two
// anchors the scores of both curves are blended linearly by frequency;
// outside them the nearest anchor applies unchanged.
type Anchor struct {
	MHz   float64
	Curve Curve
}

// PathModel holds every threshold of the path plausibility heuristic. It is
// deliberately crude: a few hand-drawn curves that encode HF lore, not a
// propagation prediction. DefaultPathModel documents the defaults.
type PathModel struct {
	// NightBelowDeg: the path counts as fully night when the sun's
	// elevation at the path midpoint is at or below this.
	NightBelowDeg float64
	// DayAboveDeg: fully daylight at or above this. Between the two the
	// path is "grey line" and the day and night scores are blended
	// linearly.
	DayAboveDeg float64

	// Day and Night are the distance curves by frequency, in increasing
	// MHz order.
	Day   []Anchor
	Night []Anchor

	// Thresholds that only choose the human-readable reasons; they do
	// not change the score.

	// GroundwaveKm: paths this short can be heard by groundwave.
	GroundwaveKm float64
	// SkipZoneMinMHz and SkipZoneMaxKm: on this frequency and above, a
	// poorly scoring path longer than groundwave and shorter than
	// SkipZoneMaxKm is explained as the skip zone.
	SkipZoneMinMHz float64
	SkipZoneMaxKm  float64
	// AbsorptionMaxMHz: below this, a poorly scoring daylight path beyond
	// groundwave is explained as D-layer absorption.
	AbsorptionMaxMHz float64
	// NightMUFMinMHz: at or above this, a poorly scoring night path beyond
	// the skip zone is explained as the frequency being above the MUF.
	NightMUFMinMHz float64
	// LongPathKm: paths longer than this are called very long.
	LongPathKm float64
	// GoodScore and PoorScore: scores at or above GoodScore get a
	// favourable reason; the explanations above only fire below PoorScore.
	GoodScore float64
	PoorScore float64
}

// DefaultPathModel returns the documented defaults.
//
//   - Night is the sun at or below -6 degrees at the midpoint (end of civil
//     twilight), day is at or above 0 degrees; the band between is grey line.
//   - Below about 6 MHz at night the favoured range is 300-2500 km (one F2
//     hop, short skip), with near-vertical paths still decent.
//   - Below about 6 MHz by day, D-layer absorption leaves short paths
//     (NVIS, up to roughly 500 km) good and kills long ones.
//   - About 10 MHz at night behaves like a longer-reaching low band:
//     800-4500 km favoured, a modest skip zone inside 300 km.
//   - 10 MHz and above by day favours 1500-6000 km with a skip zone
//     between groundwave (100 km) and about 1000 km.
//   - 12 MHz and above at night: the MUF has usually dropped below the
//     frequency, so everything beyond groundwave is poor.
//   - Every curve keeps a modest floor (0.1-0.3) out to the antipode, so a
//     very long path is unlikely but never ruled out.
func DefaultPathModel() PathModel {
	return PathModel{
		NightBelowDeg: -6,
		DayAboveDeg:   0,
		Day: []Anchor{
			{MHz: 6, Curve: Curve{{0, 0.9}, {500, 0.85}, {1000, 0.45}, {2500, 0.15}, {20000, 0.1}}},
			{MHz: 10, Curve: Curve{{0, 0.6}, {100, 0.5}, {400, 0.2}, {1000, 0.4}, {1500, 1}, {6000, 1}, {10000, 0.55}, {15000, 0.3}, {20000, 0.25}}},
		},
		Night: []Anchor{
			{MHz: 6, Curve: Curve{{0, 0.7}, {300, 1}, {2500, 1}, {5000, 0.5}, {10000, 0.25}, {20000, 0.2}}},
			{MHz: 10, Curve: Curve{{0, 0.6}, {100, 0.5}, {300, 0.45}, {800, 1}, {4500, 1}, {8000, 0.5}, {12000, 0.3}, {20000, 0.2}}},
			{MHz: 12, Curve: Curve{{0, 0.6}, {100, 0.5}, {400, 0.2}, {1500, 0.35}, {3000, 0.3}, {6000, 0.2}, {20000, 0.15}}},
		},
		GroundwaveKm:     100,
		SkipZoneMinMHz:   8,
		SkipZoneMaxKm:    1500,
		AbsorptionMaxMHz: 8,
		NightMUFMinMHz:   11,
		LongPathKm:       10000,
		GoodScore:        0.85,
		PoorScore:        0.5,
	}
}

// PathResult is the outcome of judging one path.
type PathResult struct {
	Score      float64
	DistanceKm float64
	// MidElevationDeg is the sun's elevation at the path midpoint.
	MidElevationDeg float64
	// Daylight is 0 for a night path, 1 for a daylight path, between for
	// grey line.
	Daylight float64
	Reasons  []string
}

// Score judges a transmitter-to-receiver path on freqHz at an instant.
func (m PathModel) Score(txLat, txLon, rxLat, rxLon float64, freqHz int64, at time.Time) PathResult {
	d := DistanceKm(txLat, txLon, rxLat, rxLon)
	mLat, mLon := Midpoint(txLat, txLon, rxLat, rxLon)
	elev := SolarElevation(mLat, mLon, at)
	day := m.daylight(elev)
	mhz := float64(freqHz) / 1e6

	score := day*bandScore(m.Day, mhz, d) + (1-day)*bandScore(m.Night, mhz, d)
	score = math.Max(0, math.Min(1, score))

	label := "grey-line"
	switch {
	case day >= 1:
		label = "daylight"
	case day <= 0:
		label = "night"
	}
	reasons := []string{fmt.Sprintf("%s path, %s", label, formatKm(d))}
	switch {
	case d <= m.GroundwaveKm:
		reasons = append(reasons, "within groundwave range")
	case score < m.PoorScore && mhz >= m.SkipZoneMinMHz && d < m.SkipZoneMaxKm:
		reasons = append(reasons, "likely in skip zone")
	case score < m.PoorScore && mhz < m.AbsorptionMaxMHz && day >= 0.5:
		reasons = append(reasons, "daytime absorption likely on this band")
	case score < m.PoorScore && mhz >= m.NightMUFMinMHz && day < 0.5:
		reasons = append(reasons, "frequency likely above the night-time MUF")
	case score >= m.GoodScore:
		reasons = append(reasons, "favourable distance for the band")
	}
	if d > m.LongPathKm {
		reasons = append(reasons, "very long path")
	}
	return PathResult{Score: score, DistanceKm: d, MidElevationDeg: elev, Daylight: day, Reasons: reasons}
}

func (m PathModel) daylight(elev float64) float64 {
	if m.DayAboveDeg <= m.NightBelowDeg {
		if elev > m.NightBelowDeg {
			return 1
		}
		return 0
	}
	f := (elev - m.NightBelowDeg) / (m.DayAboveDeg - m.NightBelowDeg)
	return math.Max(0, math.Min(1, f))
}

// bandScore blends the anchors either side of mhz.
func bandScore(anchors []Anchor, mhz, km float64) float64 {
	if len(anchors) == 0 {
		return 0.5
	}
	i := sort.Search(len(anchors), func(i int) bool { return anchors[i].MHz >= mhz })
	switch {
	case i == 0:
		return anchors[0].Curve.At(km)
	case i == len(anchors):
		return anchors[len(anchors)-1].Curve.At(km)
	}
	lo, hi := anchors[i-1], anchors[i]
	f := (mhz - lo.MHz) / (hi.MHz - lo.MHz)
	return (1-f)*lo.Curve.At(km) + f*hi.Curve.At(km)
}

// formatKm rounds to a readable figure: whole km under 100, else tens.
func formatKm(d float64) string {
	if d < 100 {
		return fmt.Sprintf("%.0f km", d)
	}
	return fmt.Sprintf("%.0f km", math.Round(d/10)*10)
}
