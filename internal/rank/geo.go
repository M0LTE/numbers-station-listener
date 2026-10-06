// Package rank scores receivers for listening to one transmission: hard
// filters, then a weighted sum of probe, path plausibility, receiver quality
// and load.
package rank

import "math"

// EarthRadiusKm is the mean Earth radius used for great-circle maths.
const EarthRadiusKm = 6371.0

func rad(d float64) float64 { return d * math.Pi / 180 }
func deg(r float64) float64 { return r * 180 / math.Pi }

// DistanceKm is the great-circle distance between two points (haversine).
func DistanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	p1, p2 := rad(lat1), rad(lat2)
	dp := p2 - p1
	dl := rad(lon2 - lon1)
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	if a > 1 {
		a = 1
	}
	return 2 * EarthRadiusKm * math.Asin(math.Sqrt(a))
}

// Midpoint is the point halfway along the great-circle path between two
// points. Longitude is normalised to [-180, 180).
func Midpoint(lat1, lon1, lat2, lon2 float64) (lat, lon float64) {
	p1, p2 := rad(lat1), rad(lat2)
	l1 := rad(lon1)
	dl := rad(lon2 - lon1)
	bx := math.Cos(p2) * math.Cos(dl)
	by := math.Cos(p2) * math.Sin(dl)
	pm := math.Atan2(math.Sin(p1)+math.Sin(p2), math.Sqrt((math.Cos(p1)+bx)*(math.Cos(p1)+bx)+by*by))
	lm := l1 + math.Atan2(by, math.Cos(p1)+bx)
	return deg(pm), normLon(deg(lm))
}

// InitialBearing is the initial great-circle bearing from point 1 towards
// point 2, in degrees clockwise from true north, in [0, 360).
func InitialBearing(lat1, lon1, lat2, lon2 float64) float64 {
	p1, p2 := rad(lat1), rad(lat2)
	dl := rad(lon2 - lon1)
	y := math.Sin(dl) * math.Cos(p2)
	x := math.Cos(p1)*math.Sin(p2) - math.Sin(p1)*math.Cos(p2)*math.Cos(dl)
	b := math.Mod(deg(math.Atan2(y, x))+360, 360)
	if b >= 360 {
		b -= 360
	}
	return b
}

func normLon(l float64) float64 {
	l = math.Mod(l+180, 360)
	if l < 0 {
		l += 360
	}
	return l - 180
}
