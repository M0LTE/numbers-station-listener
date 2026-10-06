package ubersdr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
)

// Probing uses GET /api/noisefloor/fft/wideband: one HTTP call returns the
// whole 0-30 MHz span as a 10-second average in bins of about 7.3 kHz, and
// holds no session, so it never competes with the relay for the two UUIDs
// per IP an instance usually allows (docs/ubersdr-protocol.md 7).
//
// What it can and cannot see. A bin is 7.3 kHz wide, so a 2.7 kHz USB voice
// signal is diluted by about 4 dB against its own bandwidth, a CW signal by
// far more, and neighbouring stations in a crowded broadcast band raise the
// reference. It reliably detects carriers and AM-like signals that stand
// well clear of the band around them. It does not reliably detect weak SSB
// voice or CW: a "not present" verdict means "not visible at this
// resolution", not "off air".
//
// Rate: the server allows 1 request per 2 s per IP. We fetch at most once
// per fftMinInterval per receiver, one in flight, and serve every probe on
// that receiver within fftCacheTTL from the same snapshot.

const (
	fftMinInterval = 5 * time.Second
	fftCacheTTL    = 15 * time.Second
	// fftMaxAge rejects a snapshot whose own timestamp is older than this.
	fftMaxAge = 2 * time.Minute

	// presentSNR is the margin over the neighbouring bins needed to call a
	// signal present.
	presentSNR = 6.0
	// Reference bins sit guardBins..guardBins+refBins away on each side.
	guardBins = 2
	refBins   = 8
)

// bandFFT is the server's BandFFT JSON.
type bandFFT struct {
	Timestamp time.Time `json:"timestamp"`
	StartFreq float64   `json:"start_freq"`
	EndFreq   float64   `json:"end_freq"`
	BinWidth  float64   `json:"bin_width"`
	Data      []float64 `json:"data"`
}

type fftSnapshot struct {
	fetched time.Time
	fft     *bandFFT // nil when the last fetch got no data
	err     string
}

// Probe estimates whether a signal is present at freqHz. It never opens a
// session. Upstream trouble (no data, rate limited, network) gives OK=false
// and a nil error; only a refused or invalid receiver is an error.
func (p *Provider) Probe(ctx context.Context, r model.Receiver, freqHz int64, mode model.Mode) (provider.ProbeResult, error) {
	base, err := p.guard(r)
	if err != nil {
		return provider.ProbeResult{}, err
	}
	snap := p.fftSnapshot(ctx, r.Key(), base.String())
	res := provider.ProbeResult{At: time.Now()}
	if snap == nil {
		res.Err = "rate limited locally; no recent spectrum"
		return res, nil
	}
	if snap.fft == nil {
		res.Err = snap.err
		return res, nil
	}
	res.At = snap.fetched
	snr, ok, why := measure(snap.fft, freqHz, mode)
	if !ok {
		res.Err = why
		return res, nil
	}
	res.OK, res.SNR, res.Present = true, snr, snr >= presentSNR
	return res, nil
}

// fftSnapshot returns a recent snapshot for the receiver, fetching one when
// the cache is stale and the gate allows. nil means nothing usable and no
// fetch allowed now.
func (p *Provider) fftSnapshot(ctx context.Context, key, base string) *fftSnapshot {
	g := p.gatesFor(key)
	g.fftMu.Lock()
	cached := g.fftCache
	g.fftMu.Unlock()
	if cached != nil && time.Since(cached.fetched) < fftCacheTTL {
		return cached
	}
	if !g.fft.tryAcquire() {
		// Another probe is fetching, or we fetched too recently. Use what
		// we have if it is not ancient.
		if cached != nil && time.Since(cached.fetched) < fftMaxAge {
			return cached
		}
		return nil
	}
	defer g.fft.release()
	snap := &fftSnapshot{fetched: time.Now()}
	snap.fft, snap.err = p.fetchFFT(ctx, base)
	g.fftMu.Lock()
	g.fftCache = snap
	g.fftMu.Unlock()
	return snap
}

func (p *Provider) fetchFFT(ctx context.Context, base string) (*bandFFT, string) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/noisefloor/fft/wideband", nil)
	if err != nil {
		return nil, err.Error()
	}
	req.Header.Set("User-Agent", p.ua)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, "fetch failed: " + err.Error()
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNoContent:
		return nil, "receiver has no spectrum data yet (204)"
	case http.StatusTooManyRequests:
		return nil, "receiver rate limited us (429)"
	default:
		return nil, fmt.Sprintf("receiver answered HTTP %d", resp.StatusCode)
	}
	var f bandFFT
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&f); err != nil {
		return nil, "bad spectrum JSON: " + err.Error()
	}
	if f.BinWidth <= 0 || len(f.Data) < 2*(guardBins+refBins)+1 {
		return nil, "spectrum JSON has no usable bins"
	}
	if !f.Timestamp.IsZero() && time.Since(f.Timestamp) > fftMaxAge {
		return nil, "receiver spectrum is stale"
	}
	return &f, ""
}

// passband is the RF range the mode's audio comes from, relative to the
// dial frequency.
func passband(mode model.Mode) (lo, hi float64) {
	switch mode {
	case model.ModeUSB:
		return 300, 2700
	case model.ModeLSB:
		return -2700, -300
	case model.ModeCWU, model.ModeCWL:
		return -450, 450
	case model.ModeFM:
		return -8000, 8000
	case model.ModeNFM:
		return -5000, 5000
	default: // am, sam
		return -3000, 3000
	}
}

// measure returns the SNR of the strongest bin overlapping the passband
// against the median of bins guardBins+1..guardBins+refBins away on both
// sides.
func measure(f *bandFFT, freqHz int64, mode model.Mode) (snr float64, ok bool, why string) {
	lo, hi := passband(mode)
	n := len(f.Data)
	binOf := func(hz float64) int { return int(math.Floor((hz - f.StartFreq) / f.BinWidth)) }
	first, last := binOf(float64(freqHz)+lo), binOf(float64(freqHz)+hi)
	if first < 0 || last >= n {
		return 0, false, "frequency outside the receiver's spectrum"
	}
	sig := math.Inf(-1)
	for i := first; i <= last; i++ {
		if v := f.Data[i]; valid(v) && v > sig {
			sig = v
		}
	}
	if math.IsInf(sig, -1) {
		return 0, false, "no data at the frequency"
	}
	var ref []float64
	for k := guardBins + 1; k <= guardBins+refBins; k++ {
		if i := first - k; i >= 0 && valid(f.Data[i]) {
			ref = append(ref, f.Data[i])
		}
		if i := last + k; i < n && valid(f.Data[i]) {
			ref = append(ref, f.Data[i])
		}
	}
	if len(ref) < refBins/2 {
		return 0, false, "too few neighbouring bins"
	}
	sort.Float64s(ref)
	med := ref[len(ref)/2]
	if len(ref)%2 == 0 {
		med = (ref[len(ref)/2-1] + ref[len(ref)/2]) / 2
	}
	return sig - med, true, ""
}

func valid(v float64) bool { return v > -900 && !math.IsNaN(v) && !math.IsInf(v, 0) }
