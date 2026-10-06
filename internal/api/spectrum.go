package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/coder/websocket"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/relay"
)

const (
	wsPingEvery   = 5 * time.Second
	wsPongTimeout = 15 * time.Second
)

type spectrumHeader struct {
	Type     string  `json:"type"`
	StartHz  float64 `json:"startHz"`
	BinHz    float64 `json:"binHz"`
	Bins     int     `json:"bins"`
	CenterHz float64 `json:"centerHz"`
	TunedHz  int64   `json:"tunedHz"`
	DbMin    float64 `json:"dbMin"`
	DbMax    float64 `json:"dbMax"`
}

// handleSpectrum relays waterfall rows over a WebSocket. The socket counts
// as a listener for as long as it is open and answering pings.
func (s *Server) handleSpectrum(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("channel")
	listener := r.URL.Query().Get("listener")
	info, ok := s.relay.Info(id)
	if !ok || listener == "" {
		writeErr(w, http.StatusNotFound, "no_channel", "")
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()

	// CloseRead consumes control frames (so pongs are seen) and ends ctx as
	// soon as the browser goes away.
	ctx := c.CloseRead(r.Context())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	lease, err := s.relay.Attach(ctx, id, listener, relay.KindSpectrum)
	if err != nil {
		code, reason := classify(err)
		b, _ := json.Marshal(map[string]string{"type": "error", "error": code, "reason": reason})
		_ = c.Write(ctx, websocket.MessageText, b)
		c.Close(websocket.StatusTryAgainLater, code)
		return
	}
	defer lease.Release()

	// Dead-listener detection: ping every 5 s, drop after 15 s with no pong.
	go func() {
		t := time.NewTicker(wsPingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, wsPongTimeout)
				err := c.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	var sc scaler
	var hdr spectrumHeader
	buf := []byte{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-lease.Done():
			if err := lease.Err(); err != nil {
				code, reason := classify(err)
				b, _ := json.Marshal(map[string]string{"type": "error", "error": code, "reason": reason})
				wctx, wc := context.WithTimeout(context.Background(), 2*time.Second)
				_ = c.Write(wctx, websocket.MessageText, b)
				wc()
			}
			c.Close(websocket.StatusNormalClosure, "")
			return
		case row, ok := <-lease.Spectrum():
			if !ok {
				return
			}
			row = cropRow(row, info.FreqHz, info.SpanHz)
			if len(row.Levels) == 0 {
				continue
			}
			lo, hi, changed := sc.update(row.Levels)
			if changed || hdr.Bins != len(row.Levels) || hdr.StartHz != row.StartHz || hdr.BinHz != row.BinHz {
				hdr = spectrumHeader{
					Type: "header", StartHz: row.StartHz, BinHz: row.BinHz, Bins: len(row.Levels),
					CenterHz: row.CenterHz(), TunedHz: info.FreqHz, DbMin: lo, DbMax: hi,
				}
				b, _ := json.Marshal(hdr)
				if err := writeWS(ctx, c, websocket.MessageText, b); err != nil {
					return
				}
			}
			buf = scaleRow(buf[:0], row.Levels, hdr.DbMin, hdr.DbMax)
			if err := writeWS(ctx, c, websocket.MessageBinary, buf); err != nil {
				return
			}
		}
	}
}

func writeWS(ctx context.Context, c *websocket.Conn, typ websocket.MessageType, b []byte) error {
	wctx, cancel := context.WithTimeout(ctx, wsPongTimeout)
	defer cancel()
	return c.Write(wctx, typ, b)
}

// cropRow keeps at most spanHz around the tuned frequency.
func cropRow(row model.SpectrumRow, tunedHz int64, spanHz int) model.SpectrumRow {
	if row.BinHz <= 0 || spanHz <= 0 {
		return row
	}
	want := int(math.Ceil(float64(spanHz) / row.BinHz))
	if want >= len(row.Levels) {
		return row
	}
	centre := int(math.Round((float64(tunedHz) - row.StartHz) / row.BinHz))
	start := centre - want/2
	if start < 0 {
		start = 0
	}
	if start+want > len(row.Levels) {
		start = len(row.Levels) - want
	}
	return model.SpectrumRow{
		StartHz: row.StartHz + float64(start)*row.BinHz,
		BinHz:   row.BinHz,
		Levels:  row.Levels[start : start+want],
	}
}

// scaler tracks the noise floor so the 8-bit rows use their range well,
// and only reports a change when it has moved enough to matter.
type scaler struct {
	floor, peak float64
	init        bool
	lo, hi      float64
}

func (s *scaler) update(levels []float32) (lo, hi float64, changed bool) {
	tmp := make([]float64, len(levels))
	for i, v := range levels {
		tmp[i] = float64(v)
	}
	sort.Float64s(tmp)
	floor := tmp[len(tmp)/5]
	peak := tmp[len(tmp)-1-len(tmp)/100]
	if !s.init {
		s.floor, s.peak, s.init = floor, peak, true
	} else {
		s.floor += (floor - s.floor) * 0.05
		s.peak += (peak - s.peak) * 0.02
	}
	nlo := math.Round(s.floor - 8)
	nhi := math.Round(math.Max(s.peak+6, s.floor+45))
	if s.hi == 0 && s.lo == 0 || math.Abs(nlo-s.lo) >= 3 || math.Abs(nhi-s.hi) >= 3 {
		s.lo, s.hi = nlo, nhi
		return s.lo, s.hi, true
	}
	return s.lo, s.hi, false
}

func scaleRow(dst []byte, levels []float32, lo, hi float64) []byte {
	span := hi - lo
	if span <= 0 {
		span = 1
	}
	for _, v := range levels {
		x := math.Round((float64(v) - lo) / span * 255)
		if x < 0 {
			x = 0
		} else if x > 255 {
			x = 255
		}
		dst = append(dst, byte(x))
	}
	return dst
}

func classify(err error) (code, reason string) {
	switch {
	case errors.Is(err, relay.ErrReceiverBusy):
		return "receiver_busy", ""
	case errors.Is(err, relay.ErrNoChannel):
		return "no_channel", ""
	case isRejected(err):
		return "rejected", err.Error()
	default:
		return "upstream", err.Error()
	}
}
