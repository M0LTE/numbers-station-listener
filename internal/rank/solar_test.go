package rank

import (
	"testing"
	"time"
)

// Meeus, "Astronomical Algorithms" (2nd ed.), Examples 25.a and 28.a, both
// for 1992 October 13.0 TD: apparent declination -7.78507 degrees and
// equation of time +13m 42.6s (13.71 minutes).
func TestSunPositionMeeus(t *testing.T) {
	at := time.Date(1992, 10, 13, 0, 0, 0, 0, time.UTC)
	decl, eot := sunPosition(at)
	near(t, "declination", decl, -7.78507, 0.01)
	near(t, "equation of time", eot, 13.71, 0.05)
}

// apparentNoon is when the sun crosses the meridian at lon on a UTC date.
func apparentNoon(y int, m time.Month, d int, lon float64) time.Time {
	t := time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
	for range 3 {
		_, eot := sunPosition(t)
		t = time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Add(time.Duration((-eot - 4*lon) * float64(time.Minute)))
	}
	return t
}

func TestSolarElevation(t *testing.T) {
	cases := []struct {
		name     string
		lat, lon float64
		at       time.Time
		want     float64
		tol      float64
	}{
		// March equinox 2026 is 20 March 14:46 UTC, so declination is
		// about -0.04 degrees at Greenwich apparent noon (12:07 UTC,
		// equation of time about -7.5 min). Noon elevation is
		// 90 - lat + decl.
		{"equinox noon Greenwich", 51.4769, 0, time.Date(2026, 3, 20, 12, 7, 0, 0, time.UTC), 90 - 51.4769 - 0.04, 0.05},
		// Near the zenith the elevation moves 0.25 degrees a minute, so
		// these use apparent noon from the equation of time.
		{"equinox noon equator", 0, 0, apparentNoon(2026, 3, 20, 0), 90 - 0.04, 0.05},
		// June solstice (21 June 08:24 UTC), declination +23.44: the sun
		// is overhead at the Tropic of Cancer at local noon.
		{"solstice overhead at tropic", 23.44, 0, time.Date(2026, 6, 21, 12, 2, 0, 0, time.UTC), 90, 0.1},
		// Midnight sun: Longyearbyen at local solar midnight, elevation
		// lat + decl - 90.
		{"midnight sun Svalbard", 78.22, 15.65, time.Date(2026, 6, 20, 23, 0, 0, 0, time.UTC), 78.22 + 23.44 - 90, 0.1},
		// Polar night: Longyearbyen at local noon on the December
		// solstice, elevation 90 - lat - 23.44.
		{"polar night noon Svalbard", 78.22, 15.65, time.Date(2026, 12, 21, 10, 59, 0, 0, time.UTC), 90 - 78.22 - 23.44, 0.1},
		// Equator at midnight on the equinox: sun straight down.
		{"equinox midnight equator", 0, 0, time.Date(2026, 3, 20, 0, 7, 0, 0, time.UTC), -90, 0.3},
		// Longitude shifts local noon: 90 E has noon 6 hours earlier,
		// 8.6 hours before the equinox, so declination is about -0.14.
		{"equinox noon 90E", 0, 90, apparentNoon(2026, 3, 20, 90), 90 - 0.14, 0.05},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			near(t, "elevation", SolarElevation(c.lat, c.lon, c.at), c.want, c.tol)
		})
	}
}

// At 78 N on the December solstice the sun never rises above -6 degrees, so
// the whole day is night for the path model.
func TestSolarElevationPolarNightAllDay(t *testing.T) {
	day := time.Date(2026, 12, 21, 0, 0, 0, 0, time.UTC)
	for m := 0; m < 24*60; m += 10 {
		at := day.Add(time.Duration(m) * time.Minute)
		if e := SolarElevation(78.22, 15.65, at); e > -6 {
			t.Fatalf("elevation %.2f at %s, want below -6 all day", e, at.Format(time.RFC3339))
		}
	}
}

// Equator on the equinox: the sun crosses the horizon about 6 hours either
// side of apparent noon.
func TestSolarElevationSunriseEquator(t *testing.T) {
	at := time.Date(2026, 3, 20, 6, 7, 0, 0, time.UTC)
	near(t, "elevation at sunrise", SolarElevation(0, 0, at), 0, 0.3)
}
