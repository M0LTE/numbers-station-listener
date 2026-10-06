package pskr

import "strings"

// Band is an amateur HF band as the feed names it.
type Band struct {
	Name   string
	LowHz  int64
	HighHz int64
}

// HFBands are the bands the client subscribes to and the store keeps,
// 160m to 10m, with ADIF edges (60m is the whole 5.06-5.45 MHz ADIF band,
// as pskr2mqtt uses). Their order is the band index used inside Store.
var HFBands = []Band{
	{"160m", 1_800_000, 2_000_000},
	{"80m", 3_500_000, 4_000_000},
	{"60m", 5_060_000, 5_450_000},
	{"40m", 7_000_000, 7_300_000},
	{"30m", 10_100_000, 10_150_000},
	{"20m", 14_000_000, 14_350_000},
	{"17m", 18_068_000, 18_168_000},
	{"15m", 21_000_000, 21_450_000},
	{"12m", 24_890_000, 24_990_000},
	{"10m", 28_000_000, 29_700_000},
}

// bandIndex finds a band by the feed's name (any case), falling back to
// the frequency when the name is missing or unknown.
func bandIndex(name string, freqHz int64) (int, bool) {
	if name != "" {
		for i, b := range HFBands {
			if strings.EqualFold(b.Name, name) {
				return i, true
			}
		}
	}
	if freqHz > 0 {
		for i, b := range HFBands {
			if freqHz >= b.LowHz && freqHz <= b.HighHz {
				return i, true
			}
		}
	}
	return 0, false
}

// bandsNear lists the indices of the bands with any part inside
// [freqHz/ratio, freqHz*ratio].
func bandsNear(freqHz int64, ratio float64) []int {
	if freqHz <= 0 || ratio < 1 {
		return nil
	}
	lo := float64(freqHz) / ratio
	hi := float64(freqHz) * ratio
	var out []int
	for i, b := range HFBands {
		if float64(b.HighHz) >= lo && float64(b.LowHz) <= hi {
			out = append(out, i)
		}
	}
	return out
}
