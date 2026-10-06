package webm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Errors returned by OpusPacketDuration.
var (
	ErrEmptyPacket   = errors.New("webm: empty Opus packet")
	ErrInvalidPacket = errors.New("webm: invalid Opus packet")
)

// maxPacketDuration is the longest an Opus packet may be (RFC 6716 section
// 3.2.5, requirement R5).
const maxPacketDuration = 120 * time.Millisecond

// frameDurations maps the TOC config number (RFC 6716 section 3.1, table 2)
// to the duration of one frame.
var frameDurations = [32]time.Duration{
	// 0..11 SILK-only: NB, MB, WB at 10, 20, 40, 60 ms.
	10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 60 * time.Millisecond,
	10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 60 * time.Millisecond,
	10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 60 * time.Millisecond,
	// 12..15 Hybrid: SWB, FB at 10, 20 ms.
	10 * time.Millisecond, 20 * time.Millisecond,
	10 * time.Millisecond, 20 * time.Millisecond,
	// 16..31 CELT-only: NB, WB, SWB, FB at 2.5, 5, 10, 20 ms.
	2500 * time.Microsecond, 5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond,
	2500 * time.Microsecond, 5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond,
	2500 * time.Microsecond, 5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond,
	2500 * time.Microsecond, 5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond,
}

// OpusPacketDuration returns the audio duration of one Opus packet from its
// TOC byte and frame count code (RFC 6716 section 3.1). For code 3 packets
// the frame count byte must be present, give at least one frame, and the
// packet must not exceed 120 ms. Frame lengths are not otherwise validated:
// the browser's decoder does that.
func OpusPacketDuration(p []byte) (time.Duration, error) {
	if len(p) == 0 {
		return 0, ErrEmptyPacket
	}
	toc := p[0]
	frame := frameDurations[toc>>3]
	var frames int
	switch toc & 0x03 {
	case 0:
		frames = 1
	case 1, 2:
		frames = 2
	case 3:
		if len(p) < 2 {
			return 0, fmt.Errorf("%w: code 3 packet without frame count byte", ErrInvalidPacket)
		}
		frames = int(p[1] & 0x3F)
		if frames == 0 {
			return 0, fmt.Errorf("%w: code 3 packet with zero frames", ErrInvalidPacket)
		}
	}
	d := time.Duration(frames) * frame
	if d > maxPacketDuration {
		return 0, fmt.Errorf("%w: %d frames of %v exceed 120 ms", ErrInvalidPacket, frames, frame)
	}
	return d, nil
}

// opusHead builds the OpusHead identification header (RFC 7845 section 5.1)
// with channel mapping family 0, which covers mono and stereo.
func opusHead(channels int, preSkip uint16, inputSampleRate uint32) []byte {
	b := make([]byte, 0, 19)
	b = append(b, "OpusHead"...)
	b = append(b, 1, byte(channels)) // version 1, channel count
	b = binary.LittleEndian.AppendUint16(b, preSkip)
	b = binary.LittleEndian.AppendUint32(b, inputSampleRate)
	b = binary.LittleEndian.AppendUint16(b, 0) // output gain, Q7.8 dB
	b = append(b, 0)                           // channel mapping family 0
	return b
}
