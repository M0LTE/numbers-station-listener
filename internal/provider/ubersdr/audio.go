package ubersdr

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/m0lte/numbers-station-listener/internal/model"
)

// Audio framing on /ws with format=opus. See docs/ubersdr-protocol.md 3.4.
//
// Version 4 (what we ask for) is a stateful header in front of each Opus
// packet:
//
//	flags u8              bit 0 quality present, bit 1 metadata present
//	timestamp             bit 1 set: u64 LE absolute ns; else zigzag varint delta
//	sampleRate uvarint    bit 1 set
//	channels u8           bit 1 set
//	power i16, noise i16  bit 0 set; centi-dB, -32768 = no reading
//	Opus packet           the rest
//
// Servers older than 0.1.63 answer version=4 with the fixed 13-byte
// version 1 header instead; the decoder recognises that on the first frame
// and switches (legacy mode).

const (
	opusFlagQuality  = 1 << 0
	opusFlagMetadata = 1 << 1

	qualityNoReading = math.MinInt16
)

// Errors from the audio frame decoder.
var (
	errNotOpus       = errors.New("ubersdr: binary audio frame is not an Opus frame")
	errNoMetadata    = errors.New("ubersdr: Opus frame before any metadata")
	errTruncated     = errors.New("ubersdr: truncated audio frame header")
	errBadAudioValue = errors.New("ubersdr: implausible audio frame metadata")
)

type audioFraming int

const (
	framingUnknown audioFraming = iota // decide on the first frame
	framingV4
	framingV1 // legacy fixed 13-byte header
	framingV3 // fixed 21-byte header (version 2 or 3)
)

// opusDecoder turns binary audio frames into packets. One per socket; frames
// must be fed in order.
type opusDecoder struct {
	framing     audioFraming
	have        bool // metadata seen
	ts          uint64
	rate        int
	channels    int
	power       int16
	noise       int16
	haveQuality bool
}

// decoded is one frame's contents.
type decoded struct {
	pkt       model.AudioPacket
	timestamp uint64 // upstream capture time, ns; unreliable, informational only
	noise     float32
	hasNoise  bool
}

func newOpusDecoder(framing audioFraming) *opusDecoder {
	return &opusDecoder{framing: framing, power: qualityNoReading, noise: qualityNoReading}
}

// decode parses one binary WebSocket message. The returned Opus slice aliases b.
func (d *opusDecoder) decode(b []byte) (decoded, error) {
	if len(b) == 0 {
		return decoded{}, errTruncated
	}
	if d.framing == framingUnknown {
		d.framing = framingV4
		if b[0] > 0x03 {
			if looksLikeV1(b) {
				d.framing = framingV1
			} else {
				return decoded{}, fmt.Errorf("%w (first byte 0x%02x)", errNotOpus, b[0])
			}
		}
	}
	switch d.framing {
	case framingV1:
		return d.decodeFixed(b, 13)
	case framingV3:
		return d.decodeFixed(b, 21)
	}
	return d.decodeV4(b)
}

// looksLikeV1 reports whether b plausibly has the fixed version 1 header:
// a sample rate radiod uses and a channel count of 1 or 2.
func looksLikeV1(b []byte) bool {
	if len(b) < 14 {
		return false
	}
	return plausibleRate(int(binary.LittleEndian.Uint32(b[8:12]))) && (b[12] == 1 || b[12] == 2)
}

func plausibleRate(r int) bool {
	switch r {
	case 8000, 10000, 12000, 16000, 24000, 48000:
		return true
	}
	return false
}

func (d *opusDecoder) decodeV4(b []byte) (decoded, error) {
	flags := b[0]
	if flags&^(opusFlagQuality|opusFlagMetadata) != 0 {
		if len(b) >= 4 && string(b[:4]) == "PCM4" {
			return decoded{}, fmt.Errorf("%w (lossless PCM v4 frame)", errNotOpus)
		}
		return decoded{}, fmt.Errorf("%w (flags 0x%02x)", errNotOpus, flags)
	}
	off := 1
	if flags&opusFlagMetadata != 0 {
		if len(b) < off+8 {
			return decoded{}, errTruncated
		}
		d.ts = binary.LittleEndian.Uint64(b[off:])
		off += 8
		rate, n := binary.Uvarint(b[off:])
		if n <= 0 {
			return decoded{}, errTruncated
		}
		off += n
		if len(b) < off+1 {
			return decoded{}, errTruncated
		}
		ch := int(b[off])
		off++
		if rate == 0 || rate > 384000 || ch < 1 || ch > 2 {
			return decoded{}, fmt.Errorf("%w (rate %d, channels %d)", errBadAudioValue, rate, ch)
		}
		d.rate, d.channels, d.have = int(rate), ch, true
	} else {
		if !d.have {
			return decoded{}, errNoMetadata
		}
		delta, n := binary.Varint(b[off:])
		if n <= 0 {
			return decoded{}, errTruncated
		}
		off += n
		d.ts = uint64(int64(d.ts) + delta)
	}
	if flags&opusFlagQuality != 0 {
		if len(b) < off+4 {
			return decoded{}, errTruncated
		}
		d.power = int16(binary.LittleEndian.Uint16(b[off:]))
		d.noise = int16(binary.LittleEndian.Uint16(b[off+2:]))
		d.haveQuality = true
		off += 4
	}
	if off >= len(b) {
		return decoded{}, fmt.Errorf("%w (no Opus payload)", errTruncated)
	}
	out := decoded{
		pkt:       model.AudioPacket{Opus: b[off:], SampleRate: d.rate, Channels: d.channels},
		timestamp: d.ts,
	}
	if d.haveQuality && d.power != qualityNoReading {
		out.pkt.Signal = float32(d.power) / 100
		out.pkt.HasSignal = true
	}
	if d.haveQuality && d.noise != qualityNoReading {
		out.noise = float32(d.noise) / 100
		out.hasNoise = true
	}
	return out, nil
}

// decodeFixed handles the version 1 (13-byte) and version 2/3 (21-byte)
// headers: u64 timestamp, u32 rate, u8 channels, then for 21 bytes f32
// power and f32 noise.
func (d *opusDecoder) decodeFixed(b []byte, hdr int) (decoded, error) {
	if len(b) <= hdr {
		return decoded{}, errTruncated
	}
	rate := int(binary.LittleEndian.Uint32(b[8:12]))
	ch := int(b[12])
	if !plausibleRate(rate) || ch < 1 || ch > 2 {
		return decoded{}, fmt.Errorf("%w (rate %d, channels %d)", errBadAudioValue, rate, ch)
	}
	d.ts, d.rate, d.channels, d.have = binary.LittleEndian.Uint64(b[0:8]), rate, ch, true
	out := decoded{pkt: model.AudioPacket{Opus: b[hdr:], SampleRate: rate, Channels: ch}, timestamp: d.ts}
	if hdr == 21 {
		p := math.Float32frombits(binary.LittleEndian.Uint32(b[13:17]))
		n := math.Float32frombits(binary.LittleEndian.Uint32(b[17:21]))
		if p > -998 && !math.IsNaN(float64(p)) {
			out.pkt.Signal, out.pkt.HasSignal = p, true
		}
		if n > -998 && !math.IsNaN(float64(n)) {
			out.noise, out.hasNoise = n, true
		}
	}
	return out, nil
}
