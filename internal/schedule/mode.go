package schedule

import (
	"strings"

	"github.com/m0lte/numbers-station-listener/internal/model"
)

// digitalSubstrings mark a digital mode wherever they appear ("MFSK-64",
// "8FSK", "PSK-31", "FSK 200/1000").
var digitalSubstrings = []string{"RTTY", "FSK", "PSK", "OFDM", "QAM", "STANAG", "PACTOR", "OLIVIA", "SITOR", "BAUDOT", "DIGITAL", "POLYTONE"}

// digitalTokens mark a digital mode only as a whole token, because they are
// short enough to turn up inside unrelated words.
var digitalTokens = map[string]bool{
	"XPA": true, "ALE": true, "CIS": true, "MIL": true, "DATA": true,
	"FAX": true, "HELL": true, "THROB": true,
}

// MapMode maps a Priyom mode string onto the receiver mode to tune. digital
// is true for data modes, which are listened to in USB with a note in the UI.
// known is false for a string it does not recognise; the mode is then USB
// and the caller should log the string (once).
func MapMode(priyomMode string) (mode model.Mode, digital bool, known bool) {
	m := strings.ToUpper(strings.TrimSpace(priyomMode))
	m = strings.Join(strings.Fields(m), " ")
	switch m {
	case "AM":
		return model.ModeAM, false, true
	case "USB", "USB/AM":
		return model.ModeUSB, false, true
	case "LSB":
		return model.ModeLSB, false, true
	case "CW":
		return model.ModeCWU, false, true
	case "MCW":
		return model.ModeAM, false, true
	}
	if m != "" && looksDigital(m) {
		return model.ModeUSB, true, true
	}
	return model.ModeUSB, false, false
}

func looksDigital(upper string) bool {
	for _, s := range digitalSubstrings {
		if strings.Contains(upper, s) {
			return true
		}
	}
	tokens := strings.FieldsFunc(upper, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
	for _, t := range tokens {
		if digitalTokens[t] {
			return true
		}
	}
	return false
}
