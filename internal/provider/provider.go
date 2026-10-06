// Package provider defines the seam between receiver networks (UberSDR now,
// KiwiSDR later) and everything downstream. Nothing outside a provider's own
// package may know which network a receiver belongs to.
package provider

import (
	"context"
	"errors"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
)

// Capabilities says which optional features a provider supports.
type Capabilities struct {
	// HistoricalSpectrogram means Spectrogram works (UberSDR yes, Kiwi no).
	HistoricalSpectrogram bool `json:"historicalSpectrogram"`
	// LiveSpectrum means sessions deliver spectrum rows.
	LiveSpectrum bool `json:"liveSpectrum"`
}

// ProbeResult is the outcome of checking whether a signal is present.
type ProbeResult struct {
	// Present is the verdict; only meaningful when OK.
	Present bool `json:"present"`
	// SNR is signal level at the frequency against neighbouring bins, dB.
	SNR float64 `json:"snr"`
	// OK is false when the probe could not measure (rate limited, error).
	OK  bool      `json:"ok"`
	At  time.Time `json:"at"`
	Err string    `json:"err,omitempty"`
}

// OpenRequest describes an upstream session to open.
type OpenRequest struct {
	FreqHz int64
	Mode   model.Mode
	// SpanHz is the wanted waterfall span centred on FreqHz.
	SpanHz int
}

// Session is one live upstream connection (audio plus, when supported,
// spectrum). Closing it must close every socket it holds with a proper
// close frame. Audio and Spectrum are closed by the session when the
// upstream ends for any reason; Err then says why (nil after Close).
type Session interface {
	Audio() <-chan model.AudioPacket
	Spectrum() <-chan model.SpectrumRow
	// Done is closed once the session has ended and all sockets are closed.
	Done() <-chan struct{}
	Err() error
	// MaxDuration is how long the upstream will allow the session, 0 if unlimited.
	MaxDuration() time.Duration
	Close() error
}

// Provider is one receiver network.
type Provider interface {
	ID() string
	Capabilities() Capabilities
	List(ctx context.Context) ([]model.Receiver, error)
	Probe(ctx context.Context, r model.Receiver, freqHz int64, mode model.Mode) (ProbeResult, error)
	Open(ctx context.Context, r model.Receiver, req OpenRequest) (Session, error)
	DeepLink(r model.Receiver, freqHz int64, mode model.Mode) string
	// Spectrogram returns a PNG of recorded history around freqHz covering
	// the last `minutes`, or ErrUnsupported. Callers cache; providers need
	// not.
	Spectrogram(ctx context.Context, r model.Receiver, freqHz int64, spanHz int, minutes int) ([]byte, error)
}

// ErrUnsupported is returned for capabilities a provider lacks.
var ErrUnsupported = errors.New("not supported by this provider")

// RejectedError means the upstream refused us (full, banned, rate limited,
// session time used up). Callers should try the next receiver.
type RejectedError struct {
	Reason string
	// RetryAfter is a hint, zero if unknown.
	RetryAfter time.Duration
}

func (e *RejectedError) Error() string { return "upstream rejected: " + e.Reason }
