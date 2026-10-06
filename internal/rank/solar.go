package rank

import (
	"math"
	"time"
)

// SolarElevation returns the geometric elevation of the sun's centre above
// the horizon, in degrees, at the given place and instant. It follows the
// NOAA solar calculator (itself after Meeus, "Astronomical Algorithms"),
// without the atmospheric refraction correction, so it is good to a few
// hundredths of a degree for dates within a few centuries of 2000. Refraction
// only matters within about a degree of the horizon, which is well inside the
// grey-line band the path model blends over anyway.
func SolarElevation(lat, lon float64, at time.Time) float64 {
	decl, eot := sunPosition(at)
	at = at.UTC()
	midnight := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	minutes := at.Sub(midnight).Minutes()
	// True solar time in minutes, then hour angle in degrees.
	tst := math.Mod(minutes+eot+4*lon, 1440)
	if tst < 0 {
		tst += 1440
	}
	ha := tst/4 - 180
	cosZ := math.Sin(rad(lat))*math.Sin(rad(decl)) + math.Cos(rad(lat))*math.Cos(rad(decl))*math.Cos(rad(ha))
	cosZ = math.Max(-1, math.Min(1, cosZ))
	return 90 - deg(math.Acos(cosZ))
}

// sunPosition returns the sun's apparent declination (degrees) and the
// equation of time (minutes, apparent minus mean solar time) at an instant.
func sunPosition(at time.Time) (declDeg, eotMin float64) {
	// Julian day from the Unix epoch (JD 2440587.5), then Julian centuries
	// since J2000.0. UT is used in place of TT; the difference (about a
	// minute) moves the sun by well under 0.001 degree.
	jd := float64(at.UnixNano())/86400e9 + 2440587.5
	t := (jd - 2451545.0) / 36525

	l0 := math.Mod(280.46646+t*(36000.76983+t*0.0003032), 360)
	if l0 < 0 {
		l0 += 360
	}
	m := 357.52911 + t*(35999.05029-0.0001537*t)
	e := 0.016708634 - t*(0.000042037+0.0000001267*t)
	c := math.Sin(rad(m))*(1.914602-t*(0.004817+0.000014*t)) +
		math.Sin(rad(2*m))*(0.019993-0.000101*t) +
		math.Sin(rad(3*m))*0.000289
	trueLong := l0 + c
	omega := 125.04 - 1934.136*t
	appLong := trueLong - 0.00569 - 0.00478*math.Sin(rad(omega))
	eps0 := 23 + (26+(21.448-t*(46.815+t*(0.00059-t*0.001813)))/60)/60
	eps := eps0 + 0.00256*math.Cos(rad(omega))

	declDeg = deg(math.Asin(math.Sin(rad(eps)) * math.Sin(rad(appLong))))

	y := math.Tan(rad(eps/2)) * math.Tan(rad(eps/2))
	eotRad := y*math.Sin(2*rad(l0)) -
		2*e*math.Sin(rad(m)) +
		4*e*y*math.Sin(rad(m))*math.Cos(2*rad(l0)) -
		0.5*y*y*math.Sin(4*rad(l0)) -
		1.25*e*e*math.Sin(2*rad(m))
	eotMin = 4 * deg(eotRad)
	return declDeg, eotMin
}
