package webm

import (
	"encoding/binary"
	"math"
)

// Element IDs used by the muxer. IDs are written with their length marker
// bits included, exactly as they appear in the Matroska specification.
const (
	idEBML               = 0x1A45DFA3
	idEBMLVersion        = 0x4286
	idEBMLReadVersion    = 0x42F7
	idEBMLMaxIDLength    = 0x42F2
	idEBMLMaxSizeLength  = 0x42F3
	idDocType            = 0x4282
	idDocTypeVersion     = 0x4287
	idDocTypeReadVersion = 0x4285

	idSegment = 0x18538067

	idInfo          = 0x1549A966
	idTimecodeScale = 0x2AD7B1
	idMuxingApp     = 0x4D80
	idWritingApp    = 0x5741

	idTracks            = 0x1654AE6B
	idTrackEntry        = 0xAE
	idTrackNumber       = 0xD7
	idTrackUID          = 0x73C5
	idTrackType         = 0x83
	idFlagLacing        = 0x9C
	idCodecID           = 0x86
	idCodecPrivate      = 0x63A2
	idCodecDelay        = 0x56AA
	idSeekPreRoll       = 0x56BB
	idAudio             = 0xE1
	idSamplingFrequency = 0xB5
	idChannels          = 0x9F

	idCluster     = 0x1F43B675
	idTimecode    = 0xE7
	idSimpleBlock = 0xA3
)

// unknownSize is the 8-byte "size unknown" marker (all value bits set).
var unknownSize = []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

// maxVarint is the largest value appendVarint can encode. 2^56-1 itself is
// reserved for "unknown".
const maxVarint = 1<<56 - 2

// appendVarint appends v as an EBML variable-length integer in the shortest
// form that does not collide with the reserved all-ones value of that length.
// It panics if v > maxVarint, which no element this package writes can reach.
func appendVarint(b []byte, v uint64) []byte {
	for n := 1; n <= 8; n++ {
		limit := uint64(1)<<(7*n) - 1 // all-ones for this length, reserved
		if v < limit {
			marked := v | uint64(1)<<(7*n)
			for i := n - 1; i >= 0; i-- {
				b = append(b, byte(marked>>(8*i)))
			}
			return b
		}
	}
	panic("webm: varint out of range")
}

// appendID appends an element ID, which already carries its length marker.
func appendID(b []byte, id uint32) []byte {
	switch {
	case id >= 1<<24:
		return append(b, byte(id>>24), byte(id>>16), byte(id>>8), byte(id))
	case id >= 1<<16:
		return append(b, byte(id>>16), byte(id>>8), byte(id))
	case id >= 1<<8:
		return append(b, byte(id>>8), byte(id))
	default:
		return append(b, byte(id))
	}
}

// appendElement appends a complete element with a known-size payload.
func appendElement(b []byte, id uint32, payload []byte) []byte {
	b = appendID(b, id)
	b = appendVarint(b, uint64(len(payload)))
	return append(b, payload...)
}

// appendUint appends an unsigned integer element in its minimal width (at
// least one byte).
func appendUint(b []byte, id uint32, v uint64) []byte {
	n := 1
	for n < 8 && v>>(8*n) != 0 {
		n++
	}
	b = appendID(b, id)
	b = appendVarint(b, uint64(n))
	for i := n - 1; i >= 0; i-- {
		b = append(b, byte(v>>(8*i)))
	}
	return b
}

// appendString appends an ASCII/UTF-8 string element.
func appendString(b []byte, id uint32, s string) []byte {
	return appendElement(b, id, []byte(s))
}

// appendFloat appends an 8-byte IEEE 754 float element.
func appendFloat(b []byte, id uint32, f float64) []byte {
	b = appendID(b, id)
	b = appendVarint(b, 8)
	return binary.BigEndian.AppendUint64(b, math.Float64bits(f))
}
