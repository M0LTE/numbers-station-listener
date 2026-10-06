package pskr

import (
	"math"
	"sort"
	"testing"

	"github.com/m0lte/numbers-station-listener/internal/rank"
)

func TestGridCenter(t *testing.T) {
	for _, tc := range []struct {
		loc      string
		lat, lon float64
	}{
		{"IO", 55, -10},
		{"IO91", 51.5, -1},
		{"io91", 51.5, -1},
		{"AA00", -89.5, -179},
		{"RR99", 89.5, 179},
		{"JO22UU", 52 + 20.0/24 + 1.0/48, 4 + 20*2.0/24 + 1.0/24},
		{"IO91WM", 51 + 12.0/24 + 1.0/48, -2 + 22*2.0/24 + 1.0/24},
		{"CM87un16", 37 + 13.0/24 + 6.0/240 + 1.0/480, -124 + 20*2.0/24 + 1*0.2/24 + 0.1/24},
	} {
		lat, lon, ok := GridCenter(tc.loc)
		if !ok || math.Abs(lat-tc.lat) > 1e-9 || math.Abs(lon-tc.lon) > 1e-9 {
			t.Errorf("%s: got %v,%v ok=%v, want %v,%v", tc.loc, lat, lon, ok, tc.lat, tc.lon)
		}
	}
	// A 10-character locator is inside its 6-character parent.
	lat, lon, ok := GridCenter("FN42ls09ps")
	plat, plon, _ := GridCenter("FN42ls")
	if !ok || math.Abs(lat-plat) > 1.0/48 || math.Abs(lon-plon) > 1.0/24 {
		t.Errorf("FN42ls09ps: %v,%v not inside FN42ls (%v,%v)", lat, lon, plat, plon)
	}
	for _, bad := range []string{"", "I", "IO9", "SA00", "IS00", "IOA1", "IO9A", "IO91YY", "IO91WMA", "IO91wm0", "IO91WM0A0", "IO91WM00AAA"} {
		if _, _, ok := GridCenter(bad); ok {
			t.Errorf("%q: want invalid", bad)
		}
	}
	if _, _, ok := GridCenter(" IO91 "); !ok {
		t.Error("surrounding space should be ignored")
	}
}

func TestGridCell(t *testing.T) {
	for in, want := range map[string]string{"IO91": "IO91", "io91wm": "IO91", "FN42ls09ps": "FN42", "AA00": "AA00", "RR99": "RR99"} {
		c, ok := gridCell(in)
		if !ok || c.String() != want {
			t.Errorf("%s: got %s ok=%v, want %s", in, c, ok, want)
		}
	}
	for _, bad := range []string{"", "IO", "IO9", "XX99", "IO91ZZ"} {
		if _, ok := gridCell(bad); ok {
			t.Errorf("%q: want invalid", bad)
		}
	}
	// Every square's centre maps back to that square, and agrees with
	// GridCenter.
	for i := 0; i < gridCells; i++ {
		c := cell(i)
		lat, lon := c.center()
		if got := cellOf(lat, lon); got != c {
			t.Fatalf("cell %s: centre maps to %s", c, got)
		}
		glat, glon, ok := GridCenter(c.String())
		if !ok || glat != lat || glon != lon {
			t.Fatalf("cell %s: centre %v,%v but GridCenter %v,%v", c, lat, lon, glat, glon)
		}
		if back, _ := gridCell(c.String()); back != c {
			t.Fatalf("cell %s round-trips to %s", c, back)
		}
	}
	// Out-of-range input clamps rather than overflowing: 180E is 180W.
	if got := cellOf(90, 180).String(); got != "AR09" {
		t.Errorf("north pole on the date line: %s, want AR09", got)
	}
}

// cellsWithin must match a brute-force scan of every square, including
// across the date line and near the poles.
func TestCellsWithin(t *testing.T) {
	for _, tc := range []struct{ lat, lon, r float64 }{
		{51.5, -1, 500},
		{52, 5, 800},
		{0, 179.5, 800},
		{-10, -179.9, 500},
		{88, 20, 800},
		{-87, -100, 500},
		{60, 30, 50},
		{45, 0, 0},
	} {
		got := cellsWithin(tc.lat, tc.lon, tc.r)
		home := cellOf(tc.lat, tc.lon)
		want := map[cell]bool{home: true}
		for i := 0; i < gridCells; i++ {
			lat, lon := cell(i).center()
			if rank.DistanceKm(tc.lat, tc.lon, lat, lon) <= tc.r {
				want[cell(i)] = true
			}
		}
		seen := map[cell]bool{}
		for _, c := range got {
			if seen[c] {
				t.Errorf("%v: %s listed twice", tc, c)
			}
			seen[c] = true
		}
		if len(seen) != len(want) {
			var extra, missing []string
			for c := range seen {
				if !want[c] {
					extra = append(extra, c.String())
				}
			}
			for c := range want {
				if !seen[c] {
					missing = append(missing, c.String())
				}
			}
			sort.Strings(extra)
			sort.Strings(missing)
			t.Errorf("%v: %d cells, want %d; extra %v missing %v", tc, len(seen), len(want), extra, missing)
		}
	}
}

func TestBandsNear(t *testing.T) {
	names := func(idx []int) []string {
		var out []string
		for _, i := range idx {
			out = append(out, HFBands[i].Name)
		}
		return out
	}
	for _, tc := range []struct {
		hz   int64
		want []string
	}{
		{4_625_000, []string{"80m", "60m"}},
		{14_500_000, []string{"30m", "20m", "17m", "15m"}},
		{5_000_000, []string{"80m", "60m", "40m"}},
		{100_000, nil},
		{60_000_000, nil},
	} {
		got := names(bandsNear(tc.hz, 1.5))
		if len(got) != len(tc.want) {
			t.Errorf("%d: got %v want %v", tc.hz, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%d: got %v want %v", tc.hz, got, tc.want)
			}
		}
	}
}
