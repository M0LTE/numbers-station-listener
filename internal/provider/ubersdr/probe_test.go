package ubersdr

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
)

// httpFake serves handler over a pipeNet inside a synctest bubble.
func httpFake(t testing.TB, handler http.Handler) (*http.Client, func()) {
	n := newPipeNet()
	srv := &http.Server{Handler: handler}
	go srv.Serve(n)
	tr := &http.Transport{DialContext: n.dial}
	return &http.Client{Transport: tr}, func() {
		srv.Close()
		n.Close()
		tr.CloseIdleConnections()
	}
}

func flatFFT(level float64) *bandFFT {
	f := &bandFFT{StartFreq: 0, EndFreq: 30_000_000, BinWidth: widebandBinHz, Data: make([]float64, 4096)}
	for i := range f.Data {
		f.Data[i] = level
	}
	return f
}

func binAt(hz float64) int { return int(hz / widebandBinHz) }

func TestMeasure(t *testing.T) {
	// AM carrier 20 dB above a flat floor.
	f := flatFFT(-100)
	f.Data[binAt(15_388_000)] = -80
	snr, ok, why := measure(f, 15_388_000, model.ModeAM)
	if !ok || snr != 20 {
		t.Fatalf("am: %v %v %q", snr, ok, why)
	}

	// USB: the signal sits above the dial, possibly in the next bin up.
	f = flatFFT(-100)
	dial := float64(binAt(8_000_000))*widebandBinHz + widebandBinHz - 1000 // passband straddles a bin edge
	f.Data[binAt(dial+2000)] = -90
	snr, ok, _ = measure(f, int64(dial), model.ModeUSB)
	if !ok || snr != 10 {
		t.Fatalf("usb: %v %v", snr, ok)
	}
	// LSB looks below the dial, where there is nothing.
	snr, ok, _ = measure(f, int64(dial+5000), model.ModeLSB)
	if !ok || snr > 0.001 && snr < 9.999 {
		t.Fatalf("lsb: %v %v", snr, ok)
	}

	// A weak signal is measured but would not be called present.
	f = flatFFT(-100)
	f.Data[binAt(17_437_000)] = -97
	snr, ok, _ = measure(f, 17_437_000, model.ModeCWU)
	if !ok || snr != 3 || snr >= presentSNR {
		t.Fatalf("weak cw: %v %v", snr, ok)
	}

	// Neighbouring stations raise the reference: median, not mean.
	f = flatFFT(-100)
	c := binAt(9_500_000)
	f.Data[c] = -70
	f.Data[c-3], f.Data[c+4] = -60, -65
	snr, ok, _ = measure(f, 9_500_000, model.ModeAM)
	if !ok || snr != 30 {
		t.Fatalf("crowded: %v %v", snr, ok)
	}

	// Edges and missing data.
	if _, ok, _ := measure(flatFFT(-100), 29_999_000, model.ModeUSB); ok {
		t.Fatal("past the top of the spectrum measured")
	}
	f = flatFFT(-999)
	if _, ok, _ := measure(f, 7_000_000, model.ModeAM); ok {
		t.Fatal("no-data bins measured")
	}
}

type fftServer struct {
	mu       sync.Mutex
	status   int
	stale    bool
	carrier  float64 // Hz, 0 for none
	requests atomic.Int64
	uas      []string
}

func (s *fftServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/noisefloor/fft/wideband" {
		http.NotFound(w, r)
		return
	}
	s.requests.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uas = append(s.uas, r.UserAgent())
	if s.status != 0 && s.status != 200 {
		w.WriteHeader(s.status)
		return
	}
	f := flatFFT(-100)
	if s.carrier > 0 {
		f.Data[binAt(s.carrier)] = -75
	}
	ts := time.Now()
	if s.stale {
		ts = ts.Add(-10 * time.Minute)
	}
	json.NewEncoder(w).Encode(map[string]any{"timestamp": ts, "band": "wideband", "start_freq": 0,
		"end_freq": 30000000, "bin_width": widebandBinHz, "data": f.Data, "markers": nil})
}

func TestProbeCachesAndLimits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := &fftServer{carrier: 15_388_000}
		client, stop := httpFake(t, fs)
		defer stop()
		p := New(Options{UserAgent: testUA, HTTPClient: client, Logger: quietLogger()})
		r := fakeReceiver()
		ctx := context.Background()

		res, err := p.Probe(ctx, r, 15_388_000, model.ModeAM)
		if err != nil || !res.OK || !res.Present || res.SNR != 25 {
			t.Fatalf("first probe %+v %v", res, err)
		}
		// Same receiver, other frequency, 10 s later: served from cache.
		time.Sleep(10 * time.Second)
		res, err = p.Probe(ctx, r, 9_000_000, model.ModeUSB)
		if err != nil || !res.OK || res.Present || fs.requests.Load() != 1 {
			t.Fatalf("cached probe %+v %v, %d requests", res, err, fs.requests.Load())
		}
		// Stale cache: refetch.
		time.Sleep(10 * time.Second)
		if res, _ := p.Probe(ctx, r, 15_388_000, model.ModeAM); !res.OK || fs.requests.Load() != 2 {
			t.Fatalf("refetch %+v, %d requests", res, fs.requests.Load())
		}

		// Many concurrent probes on a stale cache: one request in flight.
		time.Sleep(20 * time.Second)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); p.Probe(ctx, r, 15_388_000, model.ModeAM) }()
		}
		wg.Wait()
		if n := fs.requests.Load(); n != 3 {
			t.Fatalf("%d requests after concurrent probes, want 3", n)
		}
		for _, ua := range fs.uas {
			if ua != testUA {
				t.Fatalf("UA %q", ua)
			}
		}
	})
}

func TestProbeNoDataIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		stale  bool
	}{{"204", http.StatusNoContent, false}, {"429", http.StatusTooManyRequests, false}, {"500", 500, false}, {"stale", 200, true}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fs := &fftServer{status: tc.status, stale: tc.stale}
				client, stop := httpFake(t, fs)
				defer stop()
				p := New(Options{UserAgent: testUA, HTTPClient: client, Logger: quietLogger()})
				res, err := p.Probe(context.Background(), fakeReceiver(), 7_000_000, model.ModeUSB)
				if err != nil || res.OK || res.Err == "" {
					t.Fatalf("%+v %v", res, err)
				}
				// The failure is cached too: no hammering.
				time.Sleep(time.Second)
				p.Probe(context.Background(), fakeReceiver(), 7_000_000, model.ModeUSB)
				if fs.requests.Load() != 1 {
					t.Fatalf("%d requests", fs.requests.Load())
				}
			})
		})
	}
}

func gramPNG(w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(y), G: uint8(y >> 8), B: uint8(x), A: 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func TestSpectrogramCropAndLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var queries []string
		width := 6
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			queries = append(queries, r.URL.RawQuery)
			wd := width
			mu.Unlock()
			if r.URL.Path != "/api/spectrogram" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			rows := 1000
			if wd > gramMaxWidth {
				rows = 2 // keep the uncropped case cheap; only its width matters
			}
			w.Write(gramPNG(wd, rows))
		})
		client, stop := httpFake(t, h)
		defer stop()
		p := New(Options{UserAgent: testUA, HTTPClient: client, Logger: quietLogger()})
		r := fakeReceiver()
		ctx := context.Background()

		out, err := p.Spectrogram(ctx, r, 15_388_000, 12_000, 30)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		if b := img.Bounds(); b.Dx() != 6 || b.Dy() != 30 {
			t.Fatalf("cropped to %v", b)
		}
		// Top row of the crop is source row 970 (the newest 30 of 1000).
		if c := color.NRGBAModel.Convert(img.At(img.Bounds().Min.X, img.Bounds().Min.Y)).(color.NRGBA); int(c.R)+int(c.G)<<8 != 970 {
			t.Fatalf("first kept row %d", int(c.R)+int(c.G)<<8)
		}
		mu.Lock()
		q := queries[0]
		mu.Unlock()
		lo, hi := queryInt(t, q, "freq_min"), queryInt(t, q, "freq_max")
		if float64(hi-lo) < gramMinSpanHz-1 || (lo+hi)/2 < 15_387_000 || (lo+hi)/2 > 15_389_000 {
			t.Fatalf("crop %d..%d", lo, hi)
		}

		// A second request waits out the 10 s spacing.
		start := time.Now()
		if _, err := p.Spectrogram(ctx, r, 15_388_000, 12_000, 30); err != nil {
			t.Fatal(err)
		}
		if el := time.Since(start); el != gramMinInterval {
			t.Fatalf("second request after %v", el)
		}

		// An uncropped image is refused rather than decoded.
		mu.Lock()
		width = 4096
		mu.Unlock()
		if _, err := p.Spectrogram(ctx, r, 15_388_000, 12_000, 30); err == nil {
			t.Fatal("uncropped image accepted")
		}

		// A cancelled caller does not wait for its turn.
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := p.Spectrogram(cctx, r, 15_388_000, 12_000, 30); err == nil {
			t.Fatal("cancelled request went ahead")
		}
	})
}

func queryInt(t *testing.T, q, k string) int64 {
	t.Helper()
	vals, err := urlParseQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	if vals.Get("rolling") != "1" {
		t.Fatalf("query %q lacks rolling=1", q)
	}
	n, err := strconv.ParseInt(vals.Get(k), 10, 64)
	if err != nil {
		t.Fatalf("%s in %q: %v", k, q, err)
	}
	return n
}
