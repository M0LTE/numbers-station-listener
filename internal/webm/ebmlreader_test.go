package webm

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"testing"
)

// A deliberately small, independent EBML reader used to check the muxer's
// output. It does not share any code with the writer.

type elem struct {
	id       uint32
	unknown  bool // size field was the reserved "unknown" value
	data     []byte
	children []*elem
}

var masterIDs = map[uint32]bool{
	idEBML: true, idSegment: true, idInfo: true, idTracks: true,
	idTrackEntry: true, idAudio: true, idCluster: true,
}

// readVint reads a variable-length integer. With keepMarker it returns the
// raw value (for IDs); otherwise the marker bit is stripped. allOnes reports
// whether every value bit was set.
func readVint(b []byte, keepMarker bool) (v uint64, n int, allOnes bool, err error) {
	if len(b) == 0 {
		return 0, 0, false, fmt.Errorf("vint: no data")
	}
	first := b[0]
	n = 1
	for mask := byte(0x80); n <= 8 && first&mask == 0; mask >>= 1 {
		n++
	}
	if n > 8 {
		return 0, 0, false, fmt.Errorf("vint: zero first byte")
	}
	if len(b) < n {
		return 0, 0, false, fmt.Errorf("vint: need %d bytes, have %d", n, len(b))
	}
	for i := 0; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	value := v &^ (uint64(1) << (7 * n))
	allOnes = value == uint64(1)<<(7*n)-1
	if keepMarker {
		return v, n, allOnes, nil
	}
	return value, n, allOnes, nil
}

// parseElems parses consecutive elements filling b.
func parseElems(b []byte) ([]*elem, error) {
	var out []*elem
	for len(b) > 0 {
		id, n, _, err := readVint(b, true)
		if err != nil {
			return nil, fmt.Errorf("id: %w", err)
		}
		if n > 4 {
			return nil, fmt.Errorf("id 0x%X longer than 4 bytes", id)
		}
		b = b[n:]
		size, n, unknown, err := readVint(b, false)
		if err != nil {
			return nil, fmt.Errorf("size of 0x%X: %w", id, err)
		}
		b = b[n:]
		e := &elem{id: uint32(id), unknown: unknown}
		if unknown {
			// Only the Segment may be unknown-sized here; it runs to EOF.
			if id != idSegment {
				return nil, fmt.Errorf("unknown size on 0x%X", id)
			}
			e.data, b = b, nil
		} else {
			if uint64(len(b)) < size {
				return nil, fmt.Errorf("element 0x%X size %d overruns (%d left)", id, size, len(b))
			}
			e.data, b = b[:size], b[size:]
		}
		if masterIDs[e.id] {
			if e.children, err = parseElems(e.data); err != nil {
				return nil, fmt.Errorf("in 0x%X: %w", id, err)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

func (e *elem) child(t *testing.T, id uint32) *elem {
	t.Helper()
	var found *elem
	for _, c := range e.children {
		if c.id == id {
			if found != nil {
				t.Fatalf("element 0x%X appears twice in 0x%X", id, e.id)
			}
			found = c
		}
	}
	if found == nil {
		t.Fatalf("element 0x%X missing from 0x%X", id, e.id)
	}
	return found
}

func (e *elem) has(id uint32) bool {
	for _, c := range e.children {
		if c.id == id {
			return true
		}
	}
	return false
}

func (e *elem) uint(t *testing.T) uint64 {
	t.Helper()
	if len(e.data) == 0 || len(e.data) > 8 {
		t.Fatalf("uint element 0x%X has %d bytes", e.id, len(e.data))
	}
	var v uint64
	for _, c := range e.data {
		v = v<<8 | uint64(c)
	}
	return v
}

func (e *elem) float(t *testing.T) float64 {
	t.Helper()
	switch len(e.data) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(e.data)))
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(e.data))
	}
	t.Fatalf("float element 0x%X has %d bytes", e.id, len(e.data))
	return 0
}

// block is one SimpleBlock seen in the output.
type block struct {
	clusterTC int64
	rel       int16
	flags     byte
	payload   []byte
}

func (b block) abs() int64 { return b.clusterTC + int64(b.rel) }

// parsedStream is the checked shape of a muxer output.
type parsedStream struct {
	top      []*elem
	segment  *elem
	track    *elem
	clusters []*elem
	blocks   []block
}

// parseStream parses a complete muxer output and checks the invariants
// every stream must satisfy: EBML header then one unknown-size Segment,
// Info and Tracks before any Cluster, no Cues, SeekHead or Duration, and
// SimpleBlocks on track 1 with the keyframe flag.
func parseStream(t *testing.T, out []byte) parsedStream {
	t.Helper()
	top, err := parseElems(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(top) != 2 || top[0].id != idEBML || top[1].id != idSegment {
		t.Fatalf("top level: want EBML, Segment; got %d elements", len(top))
	}
	seg := top[1]
	if !seg.unknown {
		t.Fatalf("Segment size should be unknown")
	}
	ps := parsedStream{top: top, segment: seg}
	for i, c := range seg.children {
		switch {
		case i == 0 && c.id == idInfo:
		case i == 1 && c.id == idTracks:
		case i >= 2 && c.id == idCluster:
			ps.clusters = append(ps.clusters, c)
		default:
			t.Fatalf("Segment child %d is 0x%X, want Info, Tracks, then Clusters", i, c.id)
		}
	}
	if seg.has(0x1C53BB6B) || seg.has(0x114D9B74) {
		t.Fatalf("Cues or SeekHead present")
	}
	info := seg.child(t, idInfo)
	if info.has(0x4489) {
		t.Fatalf("Duration present in a live stream")
	}
	tracks := seg.child(t, idTracks)
	if len(tracks.children) != 1 {
		t.Fatalf("want one TrackEntry, got %d", len(tracks.children))
	}
	ps.track = tracks.child(t, idTrackEntry)

	for ci, cl := range ps.clusters {
		if cl.children[0].id != idTimecode {
			t.Fatalf("cluster %d: first child 0x%X, want Timecode", ci, cl.children[0].id)
		}
		tc := int64(cl.children[0].uint(t))
		if len(cl.children) < 2 {
			t.Fatalf("cluster %d is empty", ci)
		}
		for _, c := range cl.children[1:] {
			if c.id != idSimpleBlock {
				t.Fatalf("cluster %d: unexpected child 0x%X", ci, c.id)
			}
			track, n, _, err := readVint(c.data, false)
			if err != nil || track != 1 {
				t.Fatalf("cluster %d: block track %d err %v", ci, track, err)
			}
			d := c.data[n:]
			if len(d) < 3 {
				t.Fatalf("cluster %d: short block", ci)
			}
			b := block{
				clusterTC: tc,
				rel:       int16(binary.BigEndian.Uint16(d[0:2])),
				flags:     d[2],
				payload:   d[3:],
			}
			if b.flags != 0x80 {
				t.Fatalf("block flags 0x%02X, want keyframe 0x80 and no lacing", b.flags)
			}
			ps.blocks = append(ps.blocks, b)
		}
	}
	return ps
}

// readRec reads the Opus payloads from a capture in tests/fixtures/ubersdr
// (see its README for the record layout), dropping the given number of
// header bytes from each binary message.
func readRec(t *testing.T, path string, headerLen int) [][]byte {
	t.Helper()
	d, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	var pkts [][]byte
	for len(d) > 0 {
		if len(d) < 9 {
			t.Fatalf("truncated record")
		}
		kind, n := d[0], binary.LittleEndian.Uint32(d[5:9])
		p := d[9 : 9+n]
		d = d[9+n:]
		if kind == 0 {
			pkts = append(pkts, p[headerLen:])
		}
	}
	return pkts
}
