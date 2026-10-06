package api

import (
	"github.com/m0lte/numbers-station-listener/internal/model"
)

// rttyDialOffsetHz puts an FSK signal listed by its centre frequency in the
// middle of a USB passband. VERIFY against Priyom's convention for each
// digital station (docs/ubersdr-protocol.md).
const rttyDialOffsetHz = 1500

// TuneHz is the dial frequency we send upstream for a listed frequency.
// Voice (USB, AM) and CW are tuned exactly as listed; digital modes listed
// by centre frequency are heard in USB with the dial below them.
func TuneHz(listedHz int64, mode model.Mode, digital bool) int64 {
	if digital && mode == model.ModeUSB {
		return listedHz - rttyDialOffsetHz
	}
	return listedHz
}

func parseMode(s string) (model.Mode, error) { return model.ParseMode(s) }
