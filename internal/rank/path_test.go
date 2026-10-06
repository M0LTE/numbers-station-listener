package rank

import (
	"strings"
	"testing"
	"time"
)

var (
	// Night everywhere in Europe and the North Atlantic.
	nightUTC = time.Date(2026, 10, 6, 23, 0, 0, 0, time.UTC)
	// Late morning in central Europe, early evening in Taiwan; the
	// Taiwan-Europe midpoint (Kazakhstan) is in full sun.
	dayUTC = time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)

	warsaw  = [2]float64{52.4294, 20.8814} // E11, Chotomow
	reading = [2]float64{51.460168, -0.979156}
	utah    = [2]float64{40.67, -111.83}
	taipei  = [2]float64{25.03, 121.56}
)

// north returns a point km due north of (lat, lon).
func north(lat, lon, km float64) (float64, float64) {
	return lat + km/(2*3.141592653589793*EarthRadiusKm/360), lon
}

func hasReason(rs []string, sub string) bool {
	for _, r := range rs {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

func TestPathScenarios(t *testing.T) {
	m := DefaultPathModel()
	type pt = [2]float64
	at := func(lat, lon float64) pt { return pt{lat, lon} }
	// Synthetic paths along the 10 E meridian from 45 N: day at 08:00 UTC
	// in October, night at 23:00 UTC.
	tx := at(45, 10)
	far := func(km float64) pt { la, lo := north(45, 10, km); return at(la, lo) }

	cases := []struct {
		name        string
		tx, rx      pt
		mhz         float64
		when        time.Time
		min, max    float64
		label       string
		wantReasons []string
	}{
		{"night 5 MHz Warsaw-Reading", warsaw, reading, 5, nightUTC, 0.9, 1, "night path, 1500 km", []string{"favourable distance"}},
		{"night 5 MHz Warsaw-Utah", warsaw, utah, 5, nightUTC, 0.2, 0.45, "night path, 8710 km", nil},
		{"day 15 MHz Taiwan-Reading", taipei, reading, 15, dayUTC, 0.5, 0.7, "daylight path, 9830 km", nil},
		{"day 15 MHz 3000 km", tx, far(3000), 15, dayUTC, 1, 1, "daylight path, 3000 km", []string{"favourable distance"}},
		{"day 15 MHz 400 km skip zone", tx, far(400), 15, dayUTC, 0, 0.3, "daylight path", []string{"likely in skip zone"}},
		{"day 15 MHz 40 km groundwave", tx, far(40), 15, dayUTC, 0.5, 0.7, "daylight path, 40 km", []string{"within groundwave range"}},
		{"day 4 MHz 300 km NVIS", tx, far(300), 4, dayUTC, 0.85, 0.9, "daylight path", []string{"favourable distance"}},
		{"day 4 MHz 2000 km absorbed", tx, far(2000), 4, dayUTC, 0, 0.3, "daylight path", []string{"daytime absorption"}},
		{"night 4 MHz 2000 km", tx, far(2000), 4, nightUTC, 1, 1, "night path", nil},
		{"night 15 MHz 3000 km above MUF", tx, far(3000), 15, nightUTC, 0, 0.35, "night path", []string{"above the night-time MUF"}},
		{"night 9 MHz 2000 km", tx, far(2000), 9, nightUTC, 0.9, 1, "night path", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := m.Score(c.tx[0], c.tx[1], c.rx[0], c.rx[1], int64(c.mhz*1e6), c.when)
			t.Logf("score %.3f, %v, midpoint elevation %.1f", r.Score, r.Reasons, r.MidElevationDeg)
			if r.Score < c.min-1e-9 || r.Score > c.max+1e-9 {
				t.Errorf("score %.3f, want %.2f..%.2f", r.Score, c.min, c.max)
			}
			if !strings.HasPrefix(r.Reasons[0], c.label) {
				t.Errorf("first reason %q, want prefix %q", r.Reasons[0], c.label)
			}
			for _, w := range c.wantReasons {
				if !hasReason(r.Reasons, w) {
					t.Errorf("reasons %v lack %q", r.Reasons, w)
				}
			}
		})
	}
}

func TestPathVeryLongIsModestNotZero(t *testing.T) {
	m := DefaultPathModel()
	for _, when := range []time.Time{dayUTC, nightUTC} {
		for _, mhz := range []float64{3, 5, 7, 9, 11, 15, 22} {
			// Reading to New Zealand, about 18 700 km.
			r := m.Score(reading[0], reading[1], -41.29, 174.78, int64(mhz*1e6), when)
			if r.Score < 0.1 || r.Score > 0.35 {
				t.Errorf("%v MHz at %s: score %.3f, want modest 0.1..0.35", mhz, when.Format("15:04"), r.Score)
			}
			if !hasReason(r.Reasons, "very long path") {
				t.Errorf("reasons %v lack very long path", r.Reasons)
			}
		}
	}
}

// Between anchors the score moves monotonically from one curve to the other.
func TestPathInterpolatesBetweenBands(t *testing.T) {
	m := DefaultPathModel()
	la, lo := north(45, 10, 2000)
	prev := -1.0
	for _, mhz := range []float64{6, 7, 8, 9, 10} {
		s := m.Score(45, 10, la, lo, int64(mhz*1e6), dayUTC).Score
		if s <= prev {
			t.Errorf("day 2000 km: %v MHz scored %.3f, not above %.3f", mhz, s, prev)
		}
		prev = s
	}
	lo6 := m.Score(45, 10, la, lo, 6e6, dayUTC).Score
	hi10 := m.Score(45, 10, la, lo, 10e6, dayUTC).Score
	mid := m.Score(45, 10, la, lo, 8e6, dayUTC).Score
	near(t, "8 MHz is halfway between 6 and 10", mid, (lo6+hi10)/2, 1e-9)
	// Clamped outside the anchors.
	near(t, "2 MHz same as 6 MHz", m.Score(45, 10, la, lo, 2e6, dayUTC).Score, lo6, 1e-9)
	near(t, "25 MHz same as 10 MHz", m.Score(45, 10, la, lo, 25e6, dayUTC).Score, hi10, 1e-9)
}

func TestPathGreyLine(t *testing.T) {
	m := DefaultPathModel()
	for _, c := range []struct{ elev, want float64 }{{-20, 0}, {-6, 0}, {-3, 0.5}, {0, 1}, {40, 1}} {
		near(t, "daylight", m.daylight(c.elev), c.want, 1e-9)
	}
	// Find a grey-line instant at the midpoint of a synthetic path and
	// check the score is the blend and the label says so.
	la, lo := north(45, 10, 2000)
	mLat, mLon := Midpoint(45, 10, la, lo)
	start := time.Date(2026, 10, 6, 16, 0, 0, 0, time.UTC)
	var when time.Time
	for i := 0; i < 240; i++ {
		ti := start.Add(time.Duration(i) * time.Minute)
		if e := SolarElevation(mLat, mLon, ti); e < -2 && e > -4 {
			when = ti
			break
		}
	}
	if when.IsZero() {
		t.Fatal("no dusk found")
	}
	r := m.Score(45, 10, la, lo, 5e6, when)
	if !strings.HasPrefix(r.Reasons[0], "grey-line path") {
		t.Errorf("first reason %q, want grey-line", r.Reasons[0])
	}
	day := bandScore(m.Day, 5, r.DistanceKm)
	night := bandScore(m.Night, 5, r.DistanceKm)
	near(t, "blend", r.Score, r.Daylight*day+(1-r.Daylight)*night, 1e-9)
	if r.Daylight <= 0 || r.Daylight >= 1 {
		t.Errorf("daylight %.3f, want strictly between 0 and 1", r.Daylight)
	}
}

func TestCurveAt(t *testing.T) {
	c := Curve{{100, 0.2}, {200, 1}, {400, 0.5}}
	for _, x := range []struct{ km, want float64 }{{0, 0.2}, {100, 0.2}, {150, 0.6}, {200, 1}, {300, 0.75}, {400, 0.5}, {9999, 0.5}} {
		near(t, "curve", c.At(x.km), x.want, 1e-9)
	}
	near(t, "empty curve is neutral", Curve(nil).At(5), 0.5, 0)
}

// Every default curve must be in distance order with scores in 0..1, and
// the anchors in frequency order, or the interpolation is meaningless.
func TestDefaultPathModelWellFormed(t *testing.T) {
	m := DefaultPathModel()
	for name, anchors := range map[string][]Anchor{"day": m.Day, "night": m.Night} {
		for i, a := range anchors {
			if i > 0 && a.MHz <= anchors[i-1].MHz {
				t.Errorf("%s anchors out of order at %v MHz", name, a.MHz)
			}
			for j, p := range a.Curve {
				if p.Score < 0 || p.Score > 1 {
					t.Errorf("%s %v MHz point %d score %v", name, a.MHz, j, p.Score)
				}
				if j > 0 && p.DistanceKm <= a.Curve[j-1].DistanceKm {
					t.Errorf("%s %v MHz point %d out of order", name, a.MHz, j)
				}
			}
			if last := a.Curve[len(a.Curve)-1]; last.DistanceKm < 20000 || last.Score <= 0 {
				t.Errorf("%s %v MHz: curve must reach the antipode with a non-zero floor", name, a.MHz)
			}
		}
	}
}
