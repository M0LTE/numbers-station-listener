package schedule

import (
	"testing"

	"github.com/m0lte/numbers-station-listener/internal/model"
)

func TestMapMode(t *testing.T) {
	cases := []struct {
		in      string
		mode    model.Mode
		digital bool
		known   bool
	}{
		{"AM", model.ModeAM, false, true},
		{"am", model.ModeAM, false, true},
		{"USB", model.ModeUSB, false, true},
		{"USB/AM", model.ModeUSB, false, true},
		{" usb/am ", model.ModeUSB, false, true},
		{"LSB", model.ModeLSB, false, true},
		{"CW", model.ModeCWU, false, true},
		{"cw", model.ModeCWU, false, true},
		{"MCW", model.ModeAM, false, true},
		{"RTTY", model.ModeUSB, true, true},
		{"rtty", model.ModeUSB, true, true},
		{"FSK", model.ModeUSB, true, true},
		{"FSK 200/1000", model.ModeUSB, true, true},
		{"MFSK", model.ModeUSB, true, true},
		{"MFSK-64", model.ModeUSB, true, true},
		{"PSK-31", model.ModeUSB, true, true},
		{"BPSK", model.ModeUSB, true, true},
		{"OFDM", model.ModeUSB, true, true},
		{"XPA", model.ModeUSB, true, true},
		{"XPA-Polytone", model.ModeUSB, true, true},
		{"STANAG 4285", model.ModeUSB, true, true},
		{"", model.ModeUSB, false, false},
		{"ZORK", model.ModeUSB, false, false},
		{"SCALE", model.ModeUSB, false, false},
		{"FM", model.ModeUSB, false, false},
	}
	for _, c := range cases {
		mode, digital, known := MapMode(c.in)
		if mode != c.mode || digital != c.digital || known != c.known {
			t.Errorf("MapMode(%q) = %q,%v,%v want %q,%v,%v", c.in, mode, digital, known, c.mode, c.digital, c.known)
		}
	}
}
