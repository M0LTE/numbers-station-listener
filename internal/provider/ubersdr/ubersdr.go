// Package ubersdr is the UberSDR receiver provider: directory listing,
// relay sessions (Opus audio plus spectrum), HTTP probes and recorded
// spectrograms. The wire protocol is written up in docs/ubersdr-protocol.md.
//
// UberSDR is GPL-3.0; this package is written from that description and
// contains none of its code.
package ubersdr

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
)

// ID is the provider id and the Provider field of every receiver it lists.
const ID = "ubersdr"

// Deep links accept this range (docs/brief.md).
const (
	deepLinkMinHz = 10_000
	deepLinkMaxHz = 30_000_000
)

// ErrNotAllowed is returned, before any network access, for receivers the
// Allow function refuses.
var ErrNotAllowed = errors.New("ubersdr: receiver not allowed by configuration")

// Options configures a Provider.
type Options struct {
	// DirectoryURL is the instance directory, e.g. https://instances.ubersdr.org.
	DirectoryURL string
	// UserAgent is sent on every request. It must be non-empty: UberSDR
	// refuses WebSocket sessions whose /connection carried none.
	UserAgent string
	// HTTPClient is used for everything; http.DefaultClient when nil. If its
	// Transport is an *http.Transport (or nil), sessions use a clone of it
	// that lets Close force-close dead sockets.
	HTTPClient *http.Client
	// Allow gates every network operation on a receiver. nil allows all.
	Allow func(callsign, id, publicURL string) bool
	// Logger; slog.Default() when nil.
	Logger *slog.Logger
}

// Provider implements provider.Provider for UberSDR.
type Provider struct {
	dirURL string
	ua     string
	client *http.Client
	allow  func(callsign, id, publicURL string) bool
	log    *slog.Logger

	mu    sync.Mutex
	gates map[string]*receiverGates
}

var _ provider.Provider = (*Provider)(nil)

// New builds a provider.
func New(cfg Options) *Provider {
	p := &Provider{
		dirURL: strings.TrimRight(cfg.DirectoryURL, "/"),
		ua:     cfg.UserAgent,
		client: cfg.HTTPClient,
		allow:  cfg.Allow,
		log:    cfg.Logger,
		gates:  map[string]*receiverGates{},
	}
	if p.client == nil {
		p.client = http.DefaultClient
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	if p.ua == "" {
		p.ua = "numbers-station-listener"
	}
	return p
}

// ID returns "ubersdr".
func (p *Provider) ID() string { return ID }

// Capabilities reports what UberSDR offers.
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{HistoricalSpectrogram: true, LiveSpectrum: true}
}

// guard is the single gate in front of every network operation on a
// receiver. It returns the validated base URL.
func (p *Provider) guard(r model.Receiver) (*url.URL, error) {
	if r.Provider != ID {
		return nil, fmt.Errorf("ubersdr: receiver %q belongs to provider %q", r.ID, r.Provider)
	}
	if p.allow != nil && !p.allow(r.Callsign, r.ID, r.PublicURL) {
		return nil, fmt.Errorf("%w: %s (%s)", ErrNotAllowed, r.Callsign, r.ID)
	}
	u, err := url.Parse(r.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("ubersdr: receiver %s has no usable base URL %q", r.ID, r.BaseURL)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// DeepLink opens the receiver's own UI tuned to the frequency, or "" when
// the frequency is outside what deep links (and the receiver) accept.
func (p *Provider) DeepLink(r model.Receiver, freqHz int64, mode model.Mode) string {
	lo, hi := int64(deepLinkMinHz), int64(deepLinkMaxHz)
	if r.MinHz > lo {
		lo = r.MinHz
	}
	if r.MaxHz > 0 && r.MaxHz < hi {
		hi = r.MaxHz
	}
	if freqHz < lo || freqHz > hi {
		return ""
	}
	if _, err := model.ParseMode(string(mode)); err != nil {
		return ""
	}
	u, err := url.Parse(r.PublicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	q := u.Query()
	q.Set("freq", strconv.FormatInt(freqHz, 10))
	q.Set("mode", string(mode))
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String()
}

// newUUID returns a lowercase random (version 4) UUID, the only form
// UberSDR accepts as user_session_id.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, c := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hexd[c>>4], hexd[c&0x0f])
	}
	return string(out)
}

// receiverGates holds the per-receiver HTTP rate limits.
type receiverGates struct {
	fft  *gate
	gram *gate

	fftMu    sync.Mutex
	fftCache *fftSnapshot
}

func (p *Provider) gatesFor(key string) *receiverGates {
	p.mu.Lock()
	defer p.mu.Unlock()
	g, ok := p.gates[key]
	if !ok {
		g = &receiverGates{fft: newGate(fftMinInterval), gram: newGate(gramMinInterval)}
		p.gates[key] = g
	}
	return g
}

// gate allows at most one operation in flight and spaces their starts at
// least interval apart.
type gate struct {
	sem      chan struct{}
	interval time.Duration
	next     time.Time // guarded by holding sem
}

func newGate(interval time.Duration) *gate {
	return &gate{sem: make(chan struct{}, 1), interval: interval}
}

// tryAcquire takes the gate only if it is free and the interval has passed.
func (g *gate) tryAcquire() bool {
	select {
	case g.sem <- struct{}{}:
	default:
		return false
	}
	if now := time.Now(); now.Before(g.next) {
		<-g.sem
		return false
	}
	g.next = time.Now().Add(g.interval)
	return true
}

// acquire waits for the gate, bounded by done.
func (g *gate) acquire(done <-chan struct{}) error {
	select {
	case g.sem <- struct{}{}:
	case <-done:
		return errors.New("ubersdr: cancelled waiting for rate limit")
	}
	if wait := time.Until(g.next); wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-done:
			t.Stop()
			<-g.sem
			return errors.New("ubersdr: cancelled waiting for rate limit")
		}
	}
	g.next = time.Now().Add(g.interval)
	return nil
}

func (g *gate) release() { <-g.sem }
