package webm

import (
	"bytes"
	"errors"
	"math"
	"testing"
	"time"
)

// v3Fixture is a 2 s capture of real UberSDR Opus audio. Each binary
// message is a 21-byte header followed by one Opus packet.
const v3Fixture = "../../tests/fixtures/ubersdr/audio-opus-v3-7910k-usb.rec"

// celtSilence is a 20 ms CELT fullband silence packet (config 31, code 0).
var celtSilence = []byte{0xF8, 0xFF, 0xFE}

func TestVarint(t *testing.T) {
	cases := []struct {
		v    uint64
		want []byte
	}{
		{0, []byte{0x80}},
		{1, []byte{0x81}},
		{126, []byte{0xFE}},
		{127, []byte{0x40, 0x7F}}, // 0xFF is reserved for unknown
		{128, []byte{0x40, 0x80}},
		{16382, []byte{0x7F, 0xFE}},
		{16383, []byte{0x20, 0x3F, 0xFF}},
		{1<<21 - 2, []byte{0x3F, 0xFF, 0xFE}},
		{1<<21 - 1, []byte{0x10, 0x1F, 0xFF, 0xFF}},
		{maxVarint, []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFE}},
	}
	for _, c := range cases {
		got := appendVarint(nil, c.v)
		if !bytes.Equal(got, c.want) {
			t.Errorf("varint(%d) = % X, want % X", c.v, got, c.want)
		}
		v, n, allOnes, err := readVint(got, false)
		if err != nil || v != c.v || n != len(got) || allOnes {
			t.Errorf("readVint(% X) = %d, %d, %v, %v", got, v, n, allOnes, err)
		}
	}
	// Every boundary round-trips and never produces the reserved value.
	for n := 1; n <= 8; n++ {
		for _, v := range []uint64{1<<(7*n) - 3, 1<<(7*n) - 2, 1<<(7*n) - 1, 1 << (7 * n)} {
			if v > maxVarint {
				continue
			}
			b := appendVarint(nil, v)
			got, _, allOnes, err := readVint(b, false)
			if err != nil || got != v || allOnes {
				t.Errorf("round trip %d: % X -> %d allOnes=%v err=%v", v, b, got, allOnes, err)
			}
		}
	}
	// The unknown-size marker reads back as all-ones.
	if _, _, allOnes, _ := readVint(unknownSize, false); !allOnes {
		t.Errorf("unknownSize is not all-ones")
	}
}

func TestAppendUintMinimal(t *testing.T) {
	cases := []struct {
		v    uint64
		want []byte
	}{
		{0, []byte{0xD7, 0x81, 0x00}},
		{1, []byte{0xD7, 0x81, 0x01}},
		{255, []byte{0xD7, 0x81, 0xFF}},
		{256, []byte{0xD7, 0x82, 0x01, 0x00}},
		{80000000, []byte{0xD7, 0x84, 0x04, 0xC4, 0xB4, 0x00}},
		{math.MaxUint64, []byte{0xD7, 0x88, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}},
	}
	for _, c := range cases {
		if got := appendUint(nil, idTrackNumber, c.v); !bytes.Equal(got, c.want) {
			t.Errorf("appendUint(%d) = % X, want % X", c.v, got, c.want)
		}
	}
	if got, want := appendID(nil, idSegment), []byte{0x18, 0x53, 0x80, 0x67}; !bytes.Equal(got, want) {
		t.Errorf("Segment ID = % X", got)
	}
	if got, want := appendID(nil, idTimecodeScale), []byte{0x2A, 0xD7, 0xB1}; !bytes.Equal(got, want) {
		t.Errorf("TimecodeScale ID = % X", got)
	}
}

func TestOpusHead(t *testing.T) {
	got := opusHead(2, 312, 12000)
	want := []byte{
		'O', 'p', 'u', 's', 'H', 'e', 'a', 'd',
		1,          // version
		2,          // channels
		0x38, 0x01, // pre-skip 312 LE
		0xE0, 0x2E, 0x00, 0x00, // 12000 LE
		0x00, 0x00, // output gain
		0, // mapping family
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("OpusHead\n got % X\nwant % X", got, want)
	}
}

func TestOpusPacketDuration(t *testing.T) {
	ms := time.Millisecond
	// Code 0, every config: one frame of the table duration.
	perConfig := []time.Duration{
		10 * ms, 20 * ms, 40 * ms, 60 * ms, 10 * ms, 20 * ms, 40 * ms, 60 * ms,
		10 * ms, 20 * ms, 40 * ms, 60 * ms, 10 * ms, 20 * ms, 10 * ms, 20 * ms,
		2500 * time.Microsecond, 5 * ms, 10 * ms, 20 * ms, 2500 * time.Microsecond, 5 * ms, 10 * ms, 20 * ms,
		2500 * time.Microsecond, 5 * ms, 10 * ms, 20 * ms, 2500 * time.Microsecond, 5 * ms, 10 * ms, 20 * ms,
	}
	for cfg := 0; cfg < 32; cfg++ {
		for _, stereo := range []byte{0, 4} {
			toc := byte(cfg<<3) | stereo
			d, err := OpusPacketDuration([]byte{toc})
			if err != nil || d != perConfig[cfg] {
				t.Errorf("config %d code 0: %v, %v; want %v", cfg, d, err, perConfig[cfg])
			}
		}
	}
	cases := []struct {
		name string
		p    []byte
		want time.Duration
		err  bool
	}{
		{"celt silence", celtSilence, 20 * ms, false},
		{"code 1 two equal frames", []byte{9<<3 | 1, 0, 0}, 40 * ms, false},
		{"code 2 two frames", []byte{18<<3 | 2, 1, 0, 0}, 20 * ms, false},
		{"code 1 60 ms x2 = 120", []byte{3<<3 | 1}, 120 * ms, false},
		{"code 3 three 20 ms", []byte{1<<3 | 3, 3}, 60 * ms, false},
		{"code 3 VBR+padding bits ignored", []byte{31<<3 | 3, 0xC0 | 5}, 100 * ms, false},
		{"code 3 48 x 2.5 = 120", []byte{16<<3 | 3, 48}, 120 * ms, false},
		{"code 3 49 x 2.5 too long", []byte{16<<3 | 3, 49}, 0, true},
		{"code 3 3 x 60 too long", []byte{3<<3 | 3, 3}, 0, true},
		{"code 3 zero frames", []byte{1<<3 | 3, 0}, 0, true},
		{"code 3 no count byte", []byte{1<<3 | 3}, 0, true},
		{"empty", nil, 0, true},
	}
	for _, c := range cases {
		d, err := OpusPacketDuration(c.p)
		if (err != nil) != c.err || d != c.want {
			t.Errorf("%s: got %v, %v; want %v, err=%v", c.name, d, err, c.want, c.err)
		}
	}
}

func TestFixturePacketsAre20ms(t *testing.T) {
	pkts := readRec(t, v3Fixture, 21)
	if len(pkts) != 100 {
		t.Fatalf("fixture has %d packets, want 100", len(pkts))
	}
	for i, p := range pkts {
		d, err := OpusPacketDuration(p)
		if err != nil || d != 20*time.Millisecond {
			t.Fatalf("packet %d (TOC 0x%02X): %v, %v", i, p[0], d, err)
		}
	}
}

// recorder counts writes and flushes.
type recorder struct {
	bytes.Buffer
	writes, flushes int
}

func (r *recorder) Write(p []byte) (int, error) { r.writes++; return r.Buffer.Write(p) }
func (r *recorder) Flush()                      { r.flushes++ }

func TestRoundTripStructure(t *testing.T) {
	pkts := readRec(t, v3Fixture, 21)
	var out recorder
	m := NewMuxer(&out, Config{Channels: 1, InputSampleRate: 12000, PreSkip: 312})
	if out.Len() != 0 {
		t.Fatalf("NewMuxer wrote %d bytes before any packet", out.Len())
	}
	for _, p := range pkts {
		if err := m.WritePacket(p, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	ps := parseStream(t, out.Bytes())

	ebml := ps.top[0]
	for _, c := range []struct {
		id   uint32
		want uint64
	}{
		{idEBMLVersion, 1}, {idEBMLReadVersion, 1}, {idEBMLMaxIDLength, 4},
		{idEBMLMaxSizeLength, 8}, {idDocTypeVersion, 4}, {idDocTypeReadVersion, 2},
	} {
		if got := ebml.child(t, c.id).uint(t); got != c.want {
			t.Errorf("EBML 0x%X = %d, want %d", c.id, got, c.want)
		}
	}
	if got := string(ebml.child(t, idDocType).data); got != "webm" {
		t.Errorf("DocType %q", got)
	}

	info := ps.segment.child(t, idInfo)
	if got := info.child(t, idTimecodeScale).uint(t); got != 1000000 {
		t.Errorf("TimecodeScale %d", got)
	}
	for _, id := range []uint32{idMuxingApp, idWritingApp} {
		if got := string(info.child(t, id).data); got != AppName {
			t.Errorf("app 0x%X = %q", id, got)
		}
	}

	tr := ps.track
	checks := []struct {
		id   uint32
		want uint64
	}{
		{idTrackNumber, 1}, {idTrackType, 2}, {idFlagLacing, 0},
		{idCodecDelay, 312 * 1e9 / 48000}, {idSeekPreRoll, 80000000},
	}
	for _, c := range checks {
		if got := tr.child(t, c.id).uint(t); got != c.want {
			t.Errorf("track 0x%X = %d, want %d", c.id, got, c.want)
		}
	}
	if tr.child(t, idTrackUID).uint(t) == 0 {
		t.Errorf("TrackUID must be non-zero")
	}
	if got := string(tr.child(t, idCodecID).data); got != "A_OPUS" {
		t.Errorf("CodecID %q", got)
	}
	if got := tr.child(t, idCodecPrivate).data; !bytes.Equal(got, opusHead(1, 312, 12000)) {
		t.Errorf("CodecPrivate % X", got)
	}
	audio := tr.child(t, idAudio)
	if got := audio.child(t, idSamplingFrequency).float(t); got != 48000 {
		t.Errorf("SamplingFrequency %v", got)
	}
	if got := audio.child(t, idChannels).uint(t); got != 1 {
		t.Errorf("Channels %d", got)
	}

	// Every packet comes back, in order, unchanged, 20 ms apart from 0.
	if len(ps.blocks) != len(pkts) {
		t.Fatalf("%d blocks, want %d", len(ps.blocks), len(pkts))
	}
	for i, b := range ps.blocks {
		if b.abs() != int64(i*20) {
			t.Fatalf("block %d at %d ms, want %d", i, b.abs(), i*20)
		}
		if !bytes.Equal(b.payload, pkts[i]) {
			t.Fatalf("block %d payload differs", i)
		}
	}
	// 2 s at 500 ms per cluster: 4 clusters of 25 packets.
	if len(ps.clusters) != 4 {
		t.Fatalf("%d clusters, want 4", len(ps.clusters))
	}
	for i, cl := range ps.clusters {
		if n := len(cl.children) - 1; n != 25 {
			t.Errorf("cluster %d has %d blocks", i, n)
		}
	}
	// Header, then one write per cluster, each followed by a flush.
	if out.writes != 5 || out.flushes != 5 {
		t.Errorf("writes %d flushes %d, want 5 and 5", out.writes, out.flushes)
	}
	if m.Elapsed() != 2*time.Second {
		t.Errorf("Elapsed %v", m.Elapsed())
	}
}

func TestClusterRolloverIsImmediate(t *testing.T) {
	var out recorder
	m := NewMuxer(&out, Config{Channels: 1, ClusterDuration: 100 * time.Millisecond})
	for i := 0; i < 4; i++ {
		if err := m.WritePacket(celtSilence, 0); err != nil {
			t.Fatal(err)
		}
	}
	// Header only so far: the 80 ms cluster is still buffered.
	if out.writes != 1 || out.flushes != 1 {
		t.Fatalf("after 80 ms: writes %d flushes %d", out.writes, out.flushes)
	}
	if err := m.WritePacket(celtSilence, 0); err != nil {
		t.Fatal(err)
	}
	// The packet that completes 100 ms sends the cluster without waiting
	// for the next packet.
	if out.writes != 2 || out.flushes != 2 {
		t.Fatalf("after 100 ms: writes %d flushes %d", out.writes, out.flushes)
	}
	ps := parseStream(t, out.Bytes())
	if len(ps.clusters) != 1 || len(ps.blocks) != 5 {
		t.Fatalf("clusters %d blocks %d", len(ps.clusters), len(ps.blocks))
	}
}

func TestClusterSplitsBeforeInt16Overflow(t *testing.T) {
	var out bytes.Buffer
	// A cluster target far above the int16 range forces the overflow path.
	m := NewMuxer(&out, Config{Channels: 2, ClusterDuration: time.Hour})
	pkt := []byte{3 << 3} // SILK NB 60 ms, code 0
	const n = 1200        // 72 s
	for i := 0; i < n; i++ {
		if err := m.WritePacket(pkt, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	ps := parseStream(t, out.Bytes())
	if len(ps.blocks) != n {
		t.Fatalf("%d blocks", len(ps.blocks))
	}
	if len(ps.clusters) != 3 {
		t.Fatalf("%d clusters, want 3 (72 s in clusters of at most 32.767 s)", len(ps.clusters))
	}
	for i, b := range ps.blocks {
		if b.rel < 0 {
			t.Fatalf("block %d negative relative timecode", i)
		}
		if b.abs() != int64(i*60) {
			t.Fatalf("block %d at %d ms, want %d", i, b.abs(), i*60)
		}
	}
}

func TestMonotonicWithFractionalDurations(t *testing.T) {
	var out bytes.Buffer
	m := NewMuxer(&out, Config{Channels: 1, ClusterDuration: 50 * time.Millisecond})
	celt2_5 := []byte{16 << 3}        // 2.5 ms
	celt3x2_5 := []byte{16<<3 | 3, 3} // 7.5 ms
	var want []int64
	var sum time.Duration
	for i := 0; i < 300; i++ {
		var p []byte
		var dur time.Duration
		switch i % 4 {
		case 0:
			p = celt2_5
		case 1:
			p = celt3x2_5
		case 2:
			p, dur = celtSilence, 20*time.Millisecond+333*time.Microsecond // caller-supplied
		case 3:
			p = celtSilence
		}
		want = append(want, int64((sum+time.Millisecond/2)/time.Millisecond))
		if err := m.WritePacket(p, dur); err != nil {
			t.Fatal(err)
		}
		if dur == 0 {
			dur, _ = OpusPacketDuration(p)
		}
		sum += dur
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	ps := parseStream(t, out.Bytes())
	if len(ps.blocks) != len(want) {
		t.Fatalf("%d blocks", len(ps.blocks))
	}
	prev := int64(-1)
	for i, b := range ps.blocks {
		if b.abs() != want[i] {
			t.Fatalf("block %d at %d ms, want %d", i, b.abs(), want[i])
		}
		if b.abs() < prev {
			t.Fatalf("block %d goes backwards", i)
		}
		prev = b.abs()
	}
	prevTC := int64(-1)
	for i, cl := range ps.clusters {
		tc := int64(cl.children[0].uint(t))
		if tc <= prevTC {
			t.Fatalf("cluster %d timecode %d not after %d", i, tc, prevTC)
		}
		prevTC = tc
	}
	if m.Elapsed() != sum {
		t.Errorf("Elapsed %v, want %v", m.Elapsed(), sum)
	}
}

// failWriter accepts limit writes, then fails.
type failWriter struct {
	limit, writes int
}

var errGone = errors.New("listener gone")

func (f *failWriter) Write(p []byte) (int, error) {
	f.writes++
	if f.writes > f.limit {
		return 0, errGone
	}
	return len(p), nil
}

func TestWriterErrorReturnedImmediately(t *testing.T) {
	// Fails on the header.
	f := &failWriter{limit: 0}
	m := NewMuxer(f, Config{Channels: 1})
	if err := m.WritePacket(celtSilence, 0); !errors.Is(err, errGone) {
		t.Fatalf("first packet: %v", err)
	}
	if err := m.WritePacket(celtSilence, 0); !errors.Is(err, errGone) || f.writes != 1 {
		t.Fatalf("after failure: %v, writes %d", err, f.writes)
	}

	// Fails on the first cluster: the packet that completes it gets the error.
	f = &failWriter{limit: 1}
	m = NewMuxer(f, Config{Channels: 1, ClusterDuration: 40 * time.Millisecond})
	if err := m.WritePacket(celtSilence, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.WritePacket(celtSilence, 0); !errors.Is(err, errGone) {
		t.Fatalf("cluster write: %v", err)
	}
	if err := m.Flush(); !errors.Is(err, errGone) {
		t.Fatalf("Flush after failure: %v", err)
	}
	if f.writes != 2 {
		t.Fatalf("writes %d, want 2", f.writes)
	}
}

// errFlusher has a Flush() error method, as bufio.Writer does.
type errFlusher struct {
	bytes.Buffer
	flushes int
	err     error
}

func (e *errFlusher) Flush() error { e.flushes++; return e.err }

func TestFlushErrorIsReturned(t *testing.T) {
	e := &errFlusher{}
	m := NewMuxer(e, Config{Channels: 1, ClusterDuration: 20 * time.Millisecond})
	if err := m.WritePacket(celtSilence, 0); err != nil {
		t.Fatal(err)
	}
	if e.flushes != 2 {
		t.Fatalf("flushes %d, want 2 (header and cluster)", e.flushes)
	}
	e.err = errGone
	if err := m.WritePacket(celtSilence, 0); !errors.Is(err, errGone) {
		t.Fatalf("flush error not returned: %v", err)
	}
}

func TestBadInput(t *testing.T) {
	var out bytes.Buffer
	if err := NewMuxer(&out, Config{Channels: 3}).WritePacket(celtSilence, 0); !errors.Is(err, ErrBadConfig) {
		t.Errorf("3 channels: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("bad config wrote %d bytes", out.Len())
	}
	m := NewMuxer(&out, Config{Channels: 1})
	if err := m.WritePacket(nil, 0); !errors.Is(err, ErrEmptyPacket) {
		t.Errorf("empty packet: %v", err)
	}
	if err := m.WritePacket([]byte{1<<3 | 3, 0}, 0); !errors.Is(err, ErrInvalidPacket) {
		t.Errorf("bad TOC: %v", err)
	}
	if err := m.WritePacket(celtSilence, -time.Millisecond); err == nil {
		t.Errorf("negative duration accepted")
	}
	// Rejected packets neither write nor advance time.
	if out.Len() != 0 || m.Elapsed() != 0 {
		t.Errorf("rejected packets wrote %d bytes, elapsed %v", out.Len(), m.Elapsed())
	}
	// And the muxer still works afterwards.
	if err := m.WritePacket(celtSilence, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	if ps := parseStream(t, out.Bytes()); len(ps.blocks) != 1 || ps.blocks[0].abs() != 0 {
		t.Errorf("after rejects: %+v", ps.blocks)
	}
}

func TestEachMuxerStartsAtZero(t *testing.T) {
	// Two listeners joining at different points of the same upstream both
	// start at timestamp 0.
	pkts := readRec(t, v3Fixture, 21)
	for _, start := range []int{0, 37} {
		var out bytes.Buffer
		m := NewMuxer(&out, Config{Channels: 1, InputSampleRate: 12000})
		for _, p := range pkts[start : start+30] {
			if err := m.WritePacket(p, 0); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.Flush(); err != nil {
			t.Fatal(err)
		}
		ps := parseStream(t, out.Bytes())
		if ps.blocks[0].abs() != 0 || ps.blocks[29].abs() != 29*20 {
			t.Errorf("start %d: first %d last %d", start, ps.blocks[0].abs(), ps.blocks[29].abs())
		}
	}
}
