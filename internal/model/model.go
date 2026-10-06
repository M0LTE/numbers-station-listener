// Package model holds the provider-agnostic types shared by the schedule,
// ranking, relay and API layers.
package model

import (
	"fmt"
	"strings"
	"time"
)

// Mode is a receiver demodulator mode, using UberSDR's names since they are
// the most common denominator. Providers translate as needed.
type Mode string

const (
	ModeUSB Mode = "usb"
	ModeLSB Mode = "lsb"
	ModeAM  Mode = "am"
	ModeSAM Mode = "sam"
	ModeCWU Mode = "cwu"
	ModeCWL Mode = "cwl"
	ModeFM  Mode = "fm"
	ModeNFM Mode = "nfm"
)

// ParseMode accepts a mode name in any case.
func ParseMode(s string) (Mode, error) {
	m := Mode(strings.ToLower(strings.TrimSpace(s)))
	switch m {
	case ModeUSB, ModeLSB, ModeAM, ModeSAM, ModeCWU, ModeCWL, ModeFM, ModeNFM:
		return m, nil
	}
	return "", fmt.Errorf("unknown mode %q", s)
}

// Receiver is one remote SDR as reported by its provider's directory.
type Receiver struct {
	Provider string  `json:"provider"` // "ubersdr", later "kiwisdr"
	ID       string  `json:"id"`       // provider-scoped id
	Callsign string  `json:"callsign"`
	Name     string  `json:"name"`
	Location string  `json:"location"`
	Country  string  `json:"country"` // ISO 3166 alpha-2, lower case
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	HasPos   bool    `json:"hasPos"`
	// PublicURL is the receiver's own web UI, used for attribution and deep links.
	PublicURL string `json:"publicUrl"`
	// BaseURL is where the provider talks to the receiver's API (scheme://host:port).
	BaseURL string `json:"-"`

	MinHz int64 `json:"minHz"`
	MaxHz int64 `json:"maxHz"`

	Online           bool          `json:"online"`
	AntennaConnected bool          `json:"antennaConnected"`
	LoadStatus       string        `json:"loadStatus"`
	MaxClients       int           `json:"maxClients"`
	AvailableClients int           `json:"availableClients"`
	SNR              float64       `json:"snr"` // provider quality figure, dB; NaN-free, 0 when unknown
	HasSNR           bool          `json:"hasSnr"`
	MaxSessionTime   time.Duration `json:"-"`
	CORSEnabled      bool          `json:"corsEnabled"`
	Version          string        `json:"version"`
}

// Key is the globally unique receiver key, "provider:id".
func (r Receiver) Key() string { return r.Provider + ":" + r.ID }

// Covers reports whether hz is inside the receiver's tuning range.
func (r Receiver) Covers(hz int64) bool {
	if r.MinHz == 0 && r.MaxHz == 0 {
		return true
	}
	return hz >= r.MinHz && hz <= r.MaxHz
}

// AudioPacket is one Opus packet as received from upstream. Opus always runs
// at 48 kHz internally; SampleRate is the decoder output rate the upstream
// advertised (it goes into OpusHead as the input sample rate).
type AudioPacket struct {
	Opus       []byte
	SampleRate int
	Channels   int
	// Duration of the packet; zero means "derive from the Opus TOC".
	Duration time.Duration
	// Signal is the upstream's signal level reading in dBFS if it sent one.
	Signal    float32
	HasSignal bool
}

// SpectrumRow is one waterfall line. Bins run upwards in frequency from
// StartHz with spacing BinHz. Levels are dB values (dBFS or whatever the
// provider reports; consumers only care about relative levels).
type SpectrumRow struct {
	StartHz float64
	BinHz   float64
	Levels  []float32
}

// CenterHz returns the frequency of the middle of the row.
func (s SpectrumRow) CenterHz() float64 {
	return s.StartHz + s.BinHz*float64(len(s.Levels))/2
}
