// Package webm writes a live WebM (Matroska) stream carrying one Opus audio
// track. It only remuxes: Opus packets go in unchanged as SimpleBlocks.
//
// The output is shaped for a plain <audio src="..."> element joining a stream
// that is already running: the Segment has unknown size, there is no
// Duration, SeekHead or Cues, every response's timestamps start at zero, and
// each cluster is buffered and written with a known size as soon as it holds
// Config.ClusterDuration of audio.
package webm

import (
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// AppName is written as both MuxingApp and WritingApp.
const AppName = "numbers-station-listener"

// DefaultClusterDuration is used when Config.ClusterDuration is zero. Audio
// reaches the browser one cluster at a time, so this is the latency the muxer
// adds; overhead per cluster is about 15 bytes.
const DefaultClusterDuration = 500 * time.Millisecond

// SeekPreRoll is the Opus seek pre-roll recommended by the Matroska Opus
// mapping: the decoder needs 80 ms to converge after a discontinuity.
const SeekPreRoll = 80 * time.Millisecond

// Config describes the Opus stream.
type Config struct {
	// Channels is 1 or 2.
	Channels int
	// InputSampleRate is the original sample rate written into OpusHead. It
	// is informational only; Opus always decodes at 48 kHz. Zero is allowed.
	InputSampleRate uint32
	// PreSkip is the number of 48 kHz samples the decoder should discard at
	// the start (OpusHead pre-skip). CodecDelay is written to match it,
	// because Firefox refuses a track where the two disagree.
	PreSkip uint16
	// ClusterDuration is how much audio is buffered before a cluster is
	// written and flushed. Zero means DefaultClusterDuration.
	ClusterDuration time.Duration
}

// ErrBadConfig is returned (wrapped) by the first WritePacket when the
// Config is invalid.
var ErrBadConfig = errors.New("webm: bad config")

// Muxer writes one WebM stream. It is not safe for concurrent use; the
// intended use is one Muxer per HTTP response.
type Muxer struct {
	w       io.Writer
	flush   func() error
	cfg     Config
	target  time.Duration
	started bool
	err     error // sticky writer error

	pts time.Duration // start time of the next packet

	clusterOpen  bool
	clusterStart time.Duration // start time of the cluster's first packet
	clusterTC    int64         // cluster Timecode in ms
	body         []byte        // cluster payload being built
	out          []byte        // scratch for the framed cluster
}

// NewMuxer returns a Muxer writing to w. Nothing is written until the first
// packet. If w has a Flush() method (http.Flusher) or a Flush() error method
// (bufio.Writer), it is called after the header and after every cluster.
func NewMuxer(w io.Writer, cfg Config) *Muxer {
	m := &Muxer{w: w, cfg: cfg, target: cfg.ClusterDuration}
	if m.target <= 0 {
		m.target = DefaultClusterDuration
	}
	switch f := w.(type) {
	case interface{ Flush() error }:
		m.flush = f.Flush
	case interface{ Flush() }:
		m.flush = func() error { f.Flush(); return nil }
	}
	return m
}

// Elapsed returns the total duration of the packets accepted so far, which
// is also the timestamp the next packet will get.
func (m *Muxer) Elapsed() time.Duration { return m.pts }

// WritePacket adds one Opus packet lasting dur. If dur is zero it is derived
// from the packet's TOC byte. Timestamps are the running sum of durations
// from zero, independent of any upstream clock.
//
// The first call writes the stream header. Any error from the underlying
// writer is returned at once and also by every later call: the caller should
// treat it as the listener having gone away.
func (m *Muxer) WritePacket(opus []byte, dur time.Duration) error {
	if m.err != nil {
		return m.err
	}
	if len(opus) == 0 {
		return ErrEmptyPacket
	}
	if dur == 0 {
		d, err := OpusPacketDuration(opus)
		if err != nil {
			return err
		}
		dur = d
	}
	if dur < 0 {
		return fmt.Errorf("webm: negative packet duration %v", dur)
	}
	if !m.started {
		if err := m.writeHeader(); err != nil {
			return err
		}
	}

	tc := roundMs(m.pts)
	if m.clusterOpen && tc-m.clusterTC > math.MaxInt16 {
		if err := m.flushCluster(); err != nil {
			return err
		}
	}
	if !m.clusterOpen {
		m.clusterOpen = true
		m.clusterStart = m.pts
		m.clusterTC = tc
		m.body = appendUint(m.body[:0], idTimecode, uint64(tc))
	}

	// SimpleBlock: track number (varint), int16 relative timecode, flags.
	rel := tc - m.clusterTC
	m.body = appendID(m.body, idSimpleBlock)
	m.body = appendVarint(m.body, uint64(4+len(opus)))
	m.body = append(m.body, 0x81, byte(uint16(rel)>>8), byte(rel), 0x80) // track 1, keyframe
	m.body = append(m.body, opus...)

	m.pts += dur
	if m.pts-m.clusterStart >= m.target {
		return m.flushCluster()
	}
	return nil
}

// Flush writes any partly filled cluster now. Use it before ending the
// response so the tail of the audio is not lost.
func (m *Muxer) Flush() error {
	if m.err != nil {
		return m.err
	}
	if !m.clusterOpen {
		return nil
	}
	return m.flushCluster()
}

func (m *Muxer) writeHeader() error {
	if m.cfg.Channels != 1 && m.cfg.Channels != 2 {
		return fmt.Errorf("%w: channels %d, want 1 or 2", ErrBadConfig, m.cfg.Channels)
	}
	m.started = true
	return m.emit(header(m.cfg))
}

func (m *Muxer) flushCluster() error {
	m.clusterOpen = false
	m.out = appendElement(m.out[:0], idCluster, m.body)
	return m.emit(m.out)
}

// emit writes b and flushes, recording any failure as sticky.
func (m *Muxer) emit(b []byte) error {
	if _, err := m.w.Write(b); err != nil {
		m.err = err
		return err
	}
	if m.flush != nil {
		if err := m.flush(); err != nil {
			m.err = err
			return err
		}
	}
	return nil
}

// roundMs converts a non-negative duration to milliseconds, rounding half up.
func roundMs(d time.Duration) int64 {
	return int64((d + time.Millisecond/2) / time.Millisecond)
}

// header returns the EBML header, the start of the unknown-size Segment,
// Info and Tracks.
func header(cfg Config) []byte {
	var ebml []byte
	ebml = appendUint(ebml, idEBMLVersion, 1)
	ebml = appendUint(ebml, idEBMLReadVersion, 1)
	ebml = appendUint(ebml, idEBMLMaxIDLength, 4)
	ebml = appendUint(ebml, idEBMLMaxSizeLength, 8)
	ebml = appendString(ebml, idDocType, "webm")
	ebml = appendUint(ebml, idDocTypeVersion, 4)
	ebml = appendUint(ebml, idDocTypeReadVersion, 2)

	var info []byte
	info = appendUint(info, idTimecodeScale, uint64(time.Millisecond)) // 1 ms ticks
	info = appendString(info, idMuxingApp, AppName)
	info = appendString(info, idWritingApp, AppName)

	var audio []byte
	audio = appendFloat(audio, idSamplingFrequency, 48000)
	audio = appendUint(audio, idChannels, uint64(cfg.Channels))

	codecDelay := uint64(cfg.PreSkip) * uint64(time.Second) / 48000
	var track []byte
	track = appendUint(track, idTrackNumber, 1)
	track = appendUint(track, idTrackUID, 1)
	track = appendUint(track, idTrackType, 2) // audio
	track = appendUint(track, idFlagLacing, 0)
	track = appendString(track, idCodecID, "A_OPUS")
	track = appendElement(track, idCodecPrivate, opusHead(cfg.Channels, cfg.PreSkip, cfg.InputSampleRate))
	track = appendUint(track, idCodecDelay, codecDelay)
	track = appendUint(track, idSeekPreRoll, uint64(SeekPreRoll))
	track = appendElement(track, idAudio, audio)

	var b []byte
	b = appendElement(b, idEBML, ebml)
	b = appendID(b, idSegment)
	b = append(b, unknownSize...)
	b = appendElement(b, idInfo, info)
	b = appendElement(b, idTracks, appendElement(nil, idTrackEntry, track))
	return b
}
