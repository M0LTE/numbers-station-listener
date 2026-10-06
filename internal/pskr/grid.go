package pskr

import (
	"math"
	"strings"

	"github.com/m0lte/numbers-station-listener/internal/rank"
)

// A grid cell is a 4-character Maidenhead square (for example IO91): 2
// degrees of longitude by 1 degree of latitude. Cells are numbered
// lonIdx*gridLatCells + latIdx, where lonIdx counts 2-degree columns east
// from 180W (0..179) and latIdx counts 1-degree rows north from 90S
// (0..179), so every square fits in a uint16.
const (
	gridLonCells = 180
	gridLatCells = 180
	gridCells    = gridLonCells * gridLatCells
)

type cell uint16

// GridCenter returns the centre of a Maidenhead locator of 2, 4, 6, 8 or
// 10 characters, in any case. ok is false for anything that is not a valid
// locator.
func GridCenter(loc string) (lat, lon float64, ok bool) {
	loc = strings.ToUpper(strings.TrimSpace(loc))
	n := len(loc)
	if n < 2 || n > 10 || n%2 != 0 {
		return 0, 0, false
	}
	lon, lat = -180, -90
	lonStep, latStep := 360.0, 180.0
	for i := 0; i < n; i += 2 {
		var base byte
		var div float64
		switch {
		case i == 0: // field, A..R
			base, div = 'A', 18
		case i%4 == 2: // square, extended square: digits
			base, div = '0', 10
		default: // subsquare, extended subsquare: A..X
			base, div = 'A', 24
		}
		a, b := loc[i], loc[i+1]
		if a < base || b < base || float64(a-base) >= div || float64(b-base) >= div {
			return 0, 0, false
		}
		lonStep /= div
		latStep /= div
		lon += float64(a-base) * lonStep
		lat += float64(b-base) * latStep
	}
	return lat + latStep/2, lon + lonStep/2, true
}

// gridCell returns the 4-character square a locator of 4 or more
// characters lies in. Anything shorter or invalid gives ok=false: a 2-
// character field is too coarse to place a station near anything.
func gridCell(loc string) (cell, bool) {
	loc = strings.TrimSpace(loc)
	if len(loc) < 4 {
		return 0, false
	}
	if _, _, ok := GridCenter(loc); !ok {
		return 0, false
	}
	u := strings.ToUpper(loc[:4])
	lonIdx := int(u[0]-'A')*10 + int(u[2]-'0')
	latIdx := int(u[1]-'A')*10 + int(u[3]-'0')
	return cell(lonIdx*gridLatCells + latIdx), true
}

func cellOf(lat, lon float64) cell {
	lonIdx := int(math.Floor((normLon(lon) + 180) / 2))
	latIdx := int(math.Floor(lat + 90))
	lonIdx = min(max(lonIdx, 0), gridLonCells-1)
	latIdx = min(max(latIdx, 0), gridLatCells-1)
	return cell(lonIdx*gridLatCells + latIdx)
}

func (c cell) center() (lat, lon float64) {
	lonIdx := int(c) / gridLatCells
	latIdx := int(c) % gridLatCells
	return float64(latIdx) - 90 + 0.5, float64(lonIdx)*2 - 180 + 1
}

// String is the 4-character locator, for tests and debugging.
func (c cell) String() string {
	lonIdx := int(c) / gridLatCells
	latIdx := int(c) % gridLatCells
	return string([]byte{byte('A' + lonIdx/10), byte('A' + latIdx/10), byte('0' + lonIdx%10), byte('0' + latIdx%10)})
}

// cellsWithin lists the squares whose centres lie within radiusKm of the
// point, plus the square the point itself is in.
func cellsWithin(lat, lon, radiusKm float64) []cell {
	home := cellOf(lat, lon)
	out := []cell{home}
	if radiusKm <= 0 {
		return out
	}
	dLat := radiusKm / (rank.EarthRadiusKm * math.Pi / 180)
	latLo := max(lat-dLat, -90)
	latHi := min(lat+dLat, 90)
	// The widest longitude span is at the latitude furthest from the
	// equator; near a pole every column is in range.
	edge := max(math.Abs(latLo), math.Abs(latHi))
	lonCols := gridLonCells
	firstCol := 0
	if edge < 89 {
		dLon := dLat / math.Cos(edge*math.Pi/180)
		if dLon < 180 {
			firstCol = int(math.Floor((normLon(lon-dLon) + 180) / 2))
			lastCol := int(math.Floor((normLon(lon+dLon) + 180) / 2))
			lonCols = (lastCol-firstCol+gridLonCells)%gridLonCells + 1
		}
	}
	firstRow := int(math.Floor(latLo + 90))
	lastRow := min(int(math.Floor(latHi+90)), gridLatCells-1)
	for i := 0; i < lonCols; i++ {
		col := (firstCol + i) % gridLonCells
		for row := max(firstRow, 0); row <= lastRow; row++ {
			c := cell(col*gridLatCells + row)
			if c == home {
				continue
			}
			clat, clon := c.center()
			if rank.DistanceKm(lat, lon, clat, clon) <= radiusKm {
				out = append(out, c)
			}
		}
	}
	return out
}

func normLon(l float64) float64 {
	l = math.Mod(l+180, 360)
	if l < 0 {
		l += 360
	}
	return l - 180
}
