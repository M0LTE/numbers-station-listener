package ubersdr

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
)

// The recorded spectrogram (docs/ubersdr-protocol.md 6) is one PNG row per
// minute and one pixel per ~7.3 kHz bin over 0-30 MHz. We ask for the
// rolling 24-hour image cropped to the frequency range and keep the newest
// `minutes` rows (the bottom of the image).
//
// The server allows 1 PNG per second per IP; we take at most one per
// gramMinInterval per receiver, one in flight, waiting for our turn.

const (
	gramMinInterval = 10 * time.Second
	// widebandBinHz is the recorder's bin width on a 30 MHz receiver.
	widebandBinHz = 7324.21875
	// gramMinSpanHz keeps the crop several bins wide; a crop narrower than
	// one bin makes the server ignore it and send all 4096 columns.
	gramMinSpanHz = 6 * widebandBinHz
	// gramMaxWidth rejects an image the server did not crop.
	gramMaxWidth = 256
)

// Spectrogram returns a PNG of the last `minutes` of recorded spectrum
// around freqHz, at least gramMinSpanHz wide.
func (p *Provider) Spectrogram(ctx context.Context, r model.Receiver, freqHz int64, spanHz int, minutes int) ([]byte, error) {
	base, err := p.guard(r)
	if err != nil {
		return nil, err
	}
	if minutes < 1 {
		minutes = 1
	}
	if minutes > 1440 {
		minutes = 1440
	}
	span := float64(spanHz)
	if span < gramMinSpanHz {
		span = gramMinSpanHz
	}
	lo := float64(freqHz) - span/2
	if lo < 0 {
		lo = 0
	}
	hi := lo + span

	g := p.gatesFor(r.Key()).gram
	if err := g.acquire(ctx.Done()); err != nil {
		return nil, err
	}
	defer g.release()

	q := url.Values{}
	q.Set("rolling", "1")
	q.Set("freq_min", strconv.FormatInt(int64(lo), 10))
	q.Set("freq_max", strconv.FormatInt(int64(hi), 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+"/api/spectrogram?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.ua)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ubersdr spectrogram: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ubersdr spectrogram: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("ubersdr spectrogram: %w", err)
	}
	return lastRows(raw, minutes)
}

// lastRows crops a spectrogram PNG to its bottom n rows.
func lastRows(raw []byte, n int) ([]byte, error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("ubersdr spectrogram: %w", err)
	}
	if cfg.Width > gramMaxWidth {
		return nil, fmt.Errorf("ubersdr spectrogram: server sent %d columns, crop not applied", cfg.Width)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("ubersdr spectrogram: %w", err)
	}
	b := img.Bounds()
	if b.Dy() <= n {
		return raw, nil
	}
	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok {
		return nil, fmt.Errorf("ubersdr spectrogram: cannot crop %T", img)
	}
	crop := sub.SubImage(image.Rect(b.Min.X, b.Max.Y-n, b.Max.X, b.Max.Y))
	var out bytes.Buffer
	if err := png.Encode(&out, crop); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
