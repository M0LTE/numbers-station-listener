package ubersdr

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/m0lte/numbers-station-listener/internal/webm"
)

// v4 builds a version 4 Opus frame header (our own encoder, from the spec).
type v4 struct {
	flags        byte
	abs          uint64
	delta        int64
	rate         uint64
	channels     byte
	power, noise int16
}

func (h v4) frame(opus []byte) []byte {
	b := []byte{h.flags}
	if h.flags&opusFlagMetadata != 0 {
		b = binary.LittleEndian.AppendUint64(b, h.abs)
		b = binary.AppendUvarint(b, h.rate)
		b = append(b, h.channels)
	} else {
		b = binary.AppendVarint(b, h.delta)
	}
	if h.flags&opusFlagQuality != 0 {
		b = binary.LittleEndian.AppendUint16(b, uint16(h.power))
		b = binary.LittleEndian.AppendUint16(b, uint16(h.noise))
	}
	return append(b, opus...)
}

var opusBody = []byte{0x48, 0x01, 0x02, 0x03} // TOC config 9, one 20 ms frame

func TestOpusV4AllFlagCombinations(t *testing.T) {
	d := newOpusDecoder(framingUnknown)

	// 0x03: metadata and quality.
	got, err := d.decode(v4{flags: 3, abs: 1_000_000_000, rate: 12000, channels: 1, power: -8545, noise: -9602}.frame(opusBody))
	if err != nil {
		t.Fatal(err)
	}
	if got.timestamp != 1_000_000_000 || got.pkt.SampleRate != 12000 || got.pkt.Channels != 1 {
		t.Fatalf("metadata: %+v", got)
	}
	if !got.pkt.HasSignal || got.pkt.Signal != -85.45 || !got.hasNoise || got.noise != -96.02 {
		t.Fatalf("quality: %+v", got)
	}
	if string(got.pkt.Opus) != string(opusBody) {
		t.Fatalf("opus body %x", got.pkt.Opus)
	}

	// 0x00: delta only; quality and metadata carried forward.
	got, err = d.decode(v4{flags: 0, delta: 20_000_000}.frame(opusBody))
	if err != nil {
		t.Fatal(err)
	}
	if got.timestamp != 1_020_000_000 || got.pkt.SampleRate != 12000 || !got.pkt.HasSignal || got.pkt.Signal != -85.45 {
		t.Fatalf("delta frame: %+v", got)
	}

	// 0x01: delta plus new quality, including a negative delta.
	got, err = d.decode(v4{flags: 1, delta: -5, power: -7000, noise: qualityNoReading}.frame(opusBody))
	if err != nil {
		t.Fatal(err)
	}
	if got.timestamp != 1_019_999_995 || got.pkt.Signal != -70 || got.hasNoise {
		t.Fatalf("quality frame: %+v", got)
	}

	// 0x02: metadata change (rate 24000) without quality; quality carried.
	got, err = d.decode(v4{flags: 2, abs: 5, rate: 24000, channels: 1}.frame(opusBody))
	if err != nil {
		t.Fatal(err)
	}
	if got.timestamp != 5 || got.pkt.SampleRate != 24000 || got.pkt.Signal != -70 {
		t.Fatalf("metadata-only frame: %+v", got)
	}

	// The no-reading sentinel clears HasSignal.
	got, err = d.decode(v4{flags: 1, power: qualityNoReading, noise: qualityNoReading}.frame(opusBody))
	if err != nil {
		t.Fatal(err)
	}
	if got.pkt.HasSignal {
		t.Fatalf("sentinel should mean no signal: %+v", got)
	}
}

func TestOpusV4Errors(t *testing.T) {
	cases := map[string][]byte{
		"delta before metadata": v4{flags: 0, delta: 1}.frame(opusBody),
		"truncated timestamp":   {0x02, 1, 2, 3},
		"truncated quality":     {0x01, 0x00, 0x01},
		"no payload":            v4{flags: 2, abs: 1, rate: 12000, channels: 1}.frame(nil),
		"bad channels":          v4{flags: 2, abs: 1, rate: 12000, channels: 3}.frame(opusBody),
		"zero rate":             v4{flags: 2, abs: 1, rate: 0, channels: 1}.frame(opusBody),
		"empty":                 {},
	}
	for name, b := range cases {
		d := newOpusDecoder(framingV4)
		if _, err := d.decode(b); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	d := newOpusDecoder(framingV4)
	if _, err := d.decode(append([]byte("PCM4"), 0, 0, 0, 0)); !errors.Is(err, errNotOpus) {
		t.Errorf("PCM4 frame: %v", err)
	}
	if _, err := d.decode([]byte{0x04, 0, 0}); !errors.Is(err, errNotOpus) {
		t.Errorf("unknown flag bit: %v", err)
	}
}

// The captured v4 stream: every frame decodes, the first carries a zero
// timestamp and the second resynchronises, then frames are 20 ms apart.
func TestOpusV4Fixture(t *testing.T) {
	frames := binaries(readRecords(t, "audio-opus-v4-7910k-usb.rec"))
	if len(frames) != 251 {
		t.Fatalf("fixture has %d frames", len(frames))
	}
	d := newOpusDecoder(framingUnknown)
	var prev uint64
	withSignal := 0
	for i, f := range frames {
		got, err := d.decode(f)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if got.pkt.SampleRate != 12000 || got.pkt.Channels != 1 {
			t.Fatalf("frame %d: rate %d ch %d", i, got.pkt.SampleRate, got.pkt.Channels)
		}
		dur, err := webm.OpusPacketDuration(got.pkt.Opus)
		if err != nil || dur.Milliseconds() != 20 {
			t.Fatalf("frame %d: opus duration %v %v", i, dur, err)
		}
		switch {
		case i == 0:
			if got.timestamp != 0 {
				t.Fatalf("first frame timestamp %d, fixture has 0", got.timestamp)
			}
		case i >= 2:
			if delta := got.timestamp - prev; delta < 20_000_000 || delta > 20_100_000 {
				t.Fatalf("frame %d: delta %d ns", i, delta)
			}
		}
		if got.pkt.HasSignal {
			withSignal++
			if got.pkt.Signal > -50 || got.pkt.Signal < -120 {
				t.Fatalf("frame %d: implausible signal %v", i, got.pkt.Signal)
			}
		}
		prev = got.timestamp
	}
	if withSignal != len(frames) {
		t.Fatalf("%d of %d frames carry a signal level", withSignal, len(frames))
	}
}

func TestOpusFixedHeaderFixture(t *testing.T) {
	frames := binaries(readRecords(t, "audio-opus-v3-7910k-usb.rec"))
	d := newOpusDecoder(framingV3)
	for i, f := range frames {
		got, err := d.decode(f)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if got.pkt.SampleRate != 12000 || !got.pkt.HasSignal || !got.hasNoise || got.pkt.Signal-got.noise < -10 || got.pkt.Signal-got.noise > 40 {
			t.Fatalf("frame %d: %+v", i, got)
		}
		if _, err := webm.OpusPacketDuration(got.pkt.Opus); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
}

// A pre-0.1.63 server answers version=4 with the 13-byte version 1 header;
// the decoder spots it on the first frame.
func TestOpusLegacyV1Detected(t *testing.T) {
	f := binary.LittleEndian.AppendUint64(nil, 1791323977343695124)
	f = binary.LittleEndian.AppendUint32(f, 12000)
	f = append(f, 1)
	f = append(f, opusBody...)
	d := newOpusDecoder(framingUnknown)
	got, err := d.decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if d.framing != framingV1 || got.pkt.SampleRate != 12000 || string(got.pkt.Opus) != string(opusBody) || got.pkt.HasSignal {
		t.Fatalf("legacy: framing %v %+v", d.framing, got)
	}
	if math.IsNaN(float64(got.pkt.Signal)) {
		t.Fatal("NaN signal")
	}
}
