package ubersdr

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"

	"github.com/m0lte/numbers-station-listener/internal/model"
)

// Spectrum framing on /ws/user-spectrum. See docs/ubersdr-protocol.md 4.
//
// Binary messages are either "SPEC" frames or gzip-compressed JSON control
// messages. Bins arrive in raw FFT order (DC first, then positive
// frequencies, then negative ones) and deltas index that order, so the
// accumulator stays raw and rows are rotated on the way out.

const (
	specV1Header = 22
	specV2Header = 24

	specV1FullU8  = 0x03
	specV1DeltaU8 = 0x04
	specV2Full    = 0x05
	specV2Delta   = 0x06

	maxBins = 1 << 16
)

var errNotSpec = errors.New("ubersdr: not a SPEC frame")

// specControl is a gzip JSON message on the spectrum socket.
type specControl struct {
	Type                string  `json:"type"`
	CenterFreq          float64 `json:"centerFreq"`
	BinCount            int     `json:"binCount"`
	BinBandwidth        float64 `json:"binBandwidth"`
	DefaultBinCount     int     `json:"defaultBinCount"`
	DefaultBinBandwidth float64 `json:"defaultBinBandwidth"`
	Error               string  `json:"error"`
	Status              int     `json:"status"`
}

func isGzip(b []byte) bool { return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b }

func isSpec(b []byte) bool { return len(b) >= 4 && string(b[:4]) == "SPEC" }

// decodeControl decompresses and parses a gzip JSON control message.
func decodeControl(b []byte) (specControl, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return specControl{}, err
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, 1<<20))
	if err != nil {
		return specControl{}, err
	}
	var c specControl
	if err := json.Unmarshal(raw, &c); err != nil {
		return specControl{}, err
	}
	return c, nil
}

// specDecoder accumulates SPEC frames into rows. One per socket.
type specDecoder struct {
	binHz float64 // from the latest config message

	codes    []uint8 // raw FFT order
	ref      int16   // version 2 scale, centi-dB
	step     uint8
	v1       bool // codes use the version 1 code - 256 scale
	haveFull bool

	lastSeq uint16
	haveSeq bool
	gaps    int
}

// config applies a config message. A geometry change invalidates the
// accumulator: the server does not send a full frame after a zoom that
// keeps the bin count, so deltas would land on codes from the old view
// (docs/ubersdr-protocol.md 4.5). Rows resume at the next full frame.
func (d *specDecoder) config(c specControl) {
	if c.BinBandwidth <= 0 {
		return
	}
	if d.binHz != 0 && (c.BinBandwidth != d.binHz || (c.BinCount > 0 && c.BinCount != len(d.codes))) {
		d.haveFull = false
	}
	d.binHz = c.BinBandwidth
}

// frame decodes one SPEC message. ok is false when there is nothing to emit
// yet (no config, or a delta with no full frame to apply it to).
func (d *specDecoder) frame(b []byte) (row model.SpectrumRow, ok bool, err error) {
	if !isSpec(b) || len(b) < specV1Header {
		return row, false, errNotSpec
	}
	var centre uint64
	switch b[4] {
	case 2:
		if len(b) < specV2Header {
			return row, false, errTruncated
		}
		seq := binary.LittleEndian.Uint16(b[6:8])
		if d.haveSeq && seq != d.lastSeq+1 {
			d.gaps++
		}
		d.lastSeq, d.haveSeq = seq, true
		centre = binary.LittleEndian.Uint64(b[16:24])
		body := b[specV2Header:]
		switch b[5] {
		case specV2Full:
			if len(body) < 4 || len(body)-3 > maxBins || body[2] == 0 {
				return row, false, fmt.Errorf("ubersdr: bad v2 full frame (%d bytes)", len(body))
			}
			d.ref = int16(binary.LittleEndian.Uint16(body[0:2]))
			d.step = body[2]
			d.codes = append(d.codes[:0], body[3:]...)
			d.v1, d.haveFull = false, true
		case specV2Delta:
			if !d.haveFull || d.v1 {
				return row, false, nil
			}
			if err := applyMask(d.codes, body); err != nil {
				return row, false, err
			}
		default:
			return row, false, fmt.Errorf("ubersdr: unknown v2 spectrum flags 0x%02x", b[5])
		}
	case 1:
		centre = binary.LittleEndian.Uint64(b[14:22])
		body := b[specV1Header:]
		switch b[5] {
		case specV1FullU8:
			if len(body) == 0 || len(body) > maxBins {
				return row, false, fmt.Errorf("ubersdr: bad v1 full frame (%d bytes)", len(body))
			}
			d.codes = append(d.codes[:0], body...)
			d.v1, d.haveFull = true, true
		case specV1DeltaU8:
			if !d.haveFull || !d.v1 {
				return row, false, nil
			}
			if len(body) < 2 {
				return row, false, errTruncated
			}
			n := int(binary.LittleEndian.Uint16(body))
			if len(body) < 2+3*n {
				return row, false, errTruncated
			}
			for i := 0; i < n; i++ {
				o := 2 + 3*i
				if idx := int(binary.LittleEndian.Uint16(body[o:])); idx < len(d.codes) {
					d.codes[idx] = body[o+2]
				}
			}
		default:
			// float32 frames are only sent when mode=binary8 is absent,
			// which we never do.
			return row, false, fmt.Errorf("ubersdr: unsupported v1 spectrum flags 0x%02x", b[5])
		}
	default:
		return row, false, fmt.Errorf("ubersdr: unknown spectrum version %d", b[4])
	}
	if !d.haveFull || d.binHz <= 0 {
		return row, false, nil
	}
	return d.row(float64(centre)), true, nil
}

// applyMask applies a version 2 delta body: a bitmask of ceil(n/8) bytes,
// bit i&7 of byte i>>3 for raw bin i, then one code per set bit.
func applyMask(codes []uint8, body []byte) error {
	n := len(codes)
	maskLen := (n + 7) / 8
	if len(body) < maskLen {
		return errTruncated
	}
	set := 0
	for _, m := range body[:maskLen] {
		set += bits.OnesCount8(m)
	}
	if len(body) != maskLen+set {
		return fmt.Errorf("ubersdr: v2 delta length %d, mask says %d", len(body), maskLen+set)
	}
	vi := maskLen
	for i := 0; i < n; i++ {
		if body[i>>3]&(1<<(uint(i)&7)) != 0 {
			codes[i] = body[vi]
			vi++
		}
	}
	return nil
}

// row converts the accumulator to an ascending-frequency row. Bin k of the
// result is centred on centre + (k - n/2) * binHz.
func (d *specDecoder) row(centre float64) model.SpectrumRow {
	n := len(d.codes)
	half := n / 2
	levels := make([]float32, n)
	for i, c := range d.codes {
		j := i + n - half // raw bin i lands here after rotating left by half
		if j >= n {
			j -= n
		}
		if d.v1 {
			levels[j] = float32(int(c) - 256)
		} else {
			levels[j] = float32(int(d.ref)+int(c)*int(d.step)) / 100
		}
	}
	return model.SpectrumRow{StartHz: centre - float64(half)*d.binHz, BinHz: d.binHz, Levels: levels}
}
