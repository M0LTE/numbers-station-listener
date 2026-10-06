// Command serve builds WebM streams with internal/webm from the captured
// UberSDR Opus fixture and serves them for the browser playback check in
// scripts/webm-playback/check.mjs. It is a manual tool, not a test.
//
//	go run ./scripts/webm-playback/serve [-addr 127.0.0.1:0] [-out sample.webm] [-preskip 312] [-cluster 500ms]
//
// Endpoints:
//
//	/static.webm   the fixture looped to 6 s, complete, with Content-Length
//	/silence.webm  6 s of 20 ms CELT silence packets, complete
//	/live.webm     a never-ending chunked stream paced in real time, joined
//	               mid-fixture, the way a listener joins a running relay
//	/              a bare page with an <audio> element
//
// It prints "listening on http://HOST:PORT" once ready.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/webm"
)

const fixture = "tests/fixtures/ubersdr/audio-opus-v3-7910k-usb.rec"

// v3HeaderLen is the fixed UberSDR v3 audio header in front of each packet.
const v3HeaderLen = 21

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	out := flag.String("out", "", "also write the static sample to this file")
	fix := flag.String("fixture", fixture, "UberSDR v3 Opus capture (.rec)")
	clusterDur := flag.Duration("cluster", 0, "cluster duration (0 = muxer default)")
	preSkip := flag.Uint("preskip", 312, "OpusHead pre-skip in 48 kHz samples (CodecDelay follows it)")
	flag.Parse()

	pkts, err := readRec(*fix)
	if err != nil {
		log.Fatal(err)
	}
	cfg := webm.Config{Channels: 1, InputSampleRate: 12000, PreSkip: uint16(*preSkip), ClusterDuration: *clusterDur}

	static := build(cfg, loop(pkts, 300))
	silence := build(cfg, loop([][]byte{{0xF8, 0xFF, 0xFE}}, 300))
	if *out != "" {
		if err := os.WriteFile(*out, static, 0o644); err != nil {
			log.Fatal(err)
		}
	}

	mux := http.NewServeMux()
	serveBytes := func(b []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "audio/webm")
			http.ServeContent(w, r, "a.webm", time.Time{}, bytes.NewReader(b))
		}
	}
	mux.HandleFunc("/static.webm", serveBytes(static))
	mux.HandleFunc("/silence.webm", serveBytes(silence))
	mux.HandleFunc("/live.webm", func(w http.ResponseWriter, r *http.Request) {
		// As the real handler will: no length, no ranges, no caching.
		w.Header().Set("Content-Type", "audio/webm")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Accept-Ranges", "none")
		// ?burst=N sends N packets at once before pacing (default 0: a
		// listener joining a relay with no backlog). ?src=silence uses CELT
		// silence packets instead of the fixture. ?cluster=250ms overrides
		// the cluster duration.
		src := pkts
		if r.URL.Query().Get("src") == "silence" {
			src = [][]byte{{0xF8, 0xFF, 0xFE}}
		}
		burst, _ := strconv.Atoi(r.URL.Query().Get("burst"))
		lcfg := cfg
		if d, err := time.ParseDuration(r.URL.Query().Get("cluster")); err == nil {
			lcfg.ClusterDuration = d
		}
		m := webm.NewMuxer(w, lcfg)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		// Start mid-fixture, as a listener joining a running relay does.
		i := 37
		for n := 0; n < burst; n++ {
			if err := m.WritePacket(src[i%len(src)], 0); err != nil {
				return
			}
			i++
		}
		pkts := src
		for {
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
			}
			if err := m.WritePacket(pkts[i%len(pkts)], 0); err != nil {
				log.Printf("live: listener gone after %v: %v", m.Elapsed(), err)
				return
			}
			i++
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>webm check</title><audio id="a" preload="auto"></audio>`)
	})

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("listening on http://%s\n", ln.Addr())
	log.Fatal(http.Serve(ln, mux))
}

func build(cfg webm.Config, pkts [][]byte) []byte {
	var b bytes.Buffer
	m := webm.NewMuxer(&b, cfg)
	for _, p := range pkts {
		if err := m.WritePacket(p, 0); err != nil {
			log.Fatal(err)
		}
	}
	if err := m.Flush(); err != nil {
		log.Fatal(err)
	}
	return b.Bytes()
}

func loop(pkts [][]byte, n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = pkts[i%len(pkts)]
	}
	return out
}

// readRec reads the binary messages of a capture (layout in
// tests/fixtures/ubersdr/README.md) and strips the v3 header.
func readRec(path string) ([][]byte, error) {
	d, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pkts [][]byte
	for len(d) >= 9 {
		kind, n := d[0], int(binary.LittleEndian.Uint32(d[5:9]))
		if len(d) < 9+n {
			return nil, fmt.Errorf("%s: truncated record", path)
		}
		if kind == 0 && n > v3HeaderLen {
			pkts = append(pkts, d[9+v3HeaderLen:9+n])
		}
		d = d[9+n:]
	}
	if len(pkts) == 0 {
		return nil, fmt.Errorf("%s: no audio packets", path)
	}
	return pkts, nil
}
