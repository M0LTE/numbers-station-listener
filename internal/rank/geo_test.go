package rank

import (
	"math"
	"testing"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.4f, want %.4f +/- %g", name, got, want, tol)
	}
}

func TestDistanceKm(t *testing.T) {
	cases := []struct {
		name                   string
		lat1, lon1, lat2, lon2 float64
		want, tol              float64
	}{
		{"same point", 51.5, -0.1, 51.5, -0.1, 0, 1e-9},
		// One degree of arc on a 6371 km sphere is 2*pi*6371/360.
		{"one degree along equator", 0, 0, 0, 1, 2 * math.Pi * EarthRadiusKm / 360, 1e-6},
		{"one degree along meridian", 10, 30, 11, 30, 2 * math.Pi * EarthRadiusKm / 360, 1e-6},
		{"antipodes", 10, 20, -10, -160, math.Pi * EarthRadiusKm, 1e-6},
		{"pole to pole", 90, 0, -90, 0, math.Pi * EarthRadiusKm, 1e-6},
		{"across the date line", 0, 179.5, 0, -179.5, 2 * math.Pi * EarthRadiusKm / 360, 1e-6},
		// London to Paris, the stock haversine example.
		{"London to Paris", 51.5074, -0.1278, 48.8566, 2.3522, 343.56, 0.05},
		{"symmetric", 48.8566, 2.3522, 51.5074, -0.1278, 343.56, 0.05},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			near(t, "distance", DistanceKm(c.lat1, c.lon1, c.lat2, c.lon2), c.want, c.tol)
		})
	}
}

func TestMidpoint(t *testing.T) {
	cases := []struct {
		name                   string
		lat1, lon1, lat2, lon2 float64
		wantLat, wantLon       float64
	}{
		{"equator quarter", 0, 0, 0, 90, 0, 45},
		{"meridian", 0, 0, 60, 0, 30, 0},
		{"across the date line", 0, 170, 0, -170, 0, -180},
		{"same point", 40, -70, 40, -70, 40, -70},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lat, lon := Midpoint(c.lat1, c.lon1, c.lat2, c.lon2)
			near(t, "lat", lat, c.wantLat, 1e-9)
			near(t, "lon", lon, c.wantLon, 1e-9)
		})
	}

	// A high-latitude path bulges poleward: London to Vancouver's
	// midpoint is far north of either end, and equidistant from both.
	lat, lon := Midpoint(51.5, -0.13, 49.28, -123.12)
	if lat < 65 {
		t.Errorf("London-Vancouver midpoint lat %.2f, want well north of both ends", lat)
	}
	near(t, "equidistant", DistanceKm(51.5, -0.13, lat, lon), DistanceKm(lat, lon, 49.28, -123.12), 1e-6)
}

func TestInitialBearing(t *testing.T) {
	cases := []struct {
		name                   string
		lat1, lon1, lat2, lon2 float64
		want                   float64
	}{
		{"due north", 0, 0, 10, 0, 0},
		{"due east on equator", 0, 0, 0, 10, 90},
		{"due south", 10, 0, 0, 0, 180},
		{"due west on equator", 0, 10, 0, 0, 270},
		{"London to Paris", 51.5074, -0.1278, 48.8566, 2.3522, 148.116},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			near(t, "bearing", InitialBearing(c.lat1, c.lon1, c.lat2, c.lon2), c.want, 0.001)
		})
	}
}
