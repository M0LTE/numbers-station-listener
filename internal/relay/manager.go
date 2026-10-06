// Package relay owns every upstream receiver session and shares each one
// between all the browsers listening to it.
//
// The rule this package exists to enforce: an upstream connection may only
// exist while at least one browser is actively listening to its channel.
// See docs/brief.md, "Idle connection policy".
//
//   - Nothing opens upstream until a listener attaches (no pre-warming).
//   - When the last listener leaves, the upstream closes after Grace.
//   - A watchdog sweeps every WatchdogEvery and force-closes anything the
//     normal path missed, logging it as a bug.
//   - A listener whose context has ended but which was never released is a
//     leak; the watchdog releases it and logs that too.
//
// All timing goes through the time package so tests can run inside a
// testing/synctest bubble with a fake clock.
package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
)

// Kind is what a listener consumes.
type Kind int

const (
	KindAudio Kind = iota
	KindSpectrum
)

func (k Kind) String() string {
	if k == KindSpectrum {
		return "spectrum"
	}
	return "audio"
}

// Config tunes the manager. Zero values take the defaults.
type Config struct {
	// Grace is how long an upstream survives with no listeners. Default 10 s,
	// never more than MaxGrace.
	Grace time.Duration
	// PerReceiverCap is the most upstream sessions held on one receiver.
	PerReceiverCap int
	// WatchdogEvery is the sweep interval. Default 5 s.
	WatchdogEvery time.Duration
	// OpenTimeout bounds one attempt to open an upstream session.
	OpenTimeout time.Duration
	// ChannelTTL is how long an unused channel record is remembered.
	ChannelTTL time.Duration
	// ListenerBuffer is the per-listener queue length (packets or rows).
	ListenerBuffer int
}

// MaxGrace is the hard ceiling on Config.Grace from the brief.
const MaxGrace = 30 * time.Second

func (c Config) withDefaults() Config {
	if c.Grace <= 0 {
		c.Grace = 10 * time.Second
	}
	if c.Grace > MaxGrace {
		c.Grace = MaxGrace
	}
	if c.PerReceiverCap <= 0 {
		c.PerReceiverCap = 2
	}
	if c.WatchdogEvery <= 0 {
		c.WatchdogEvery = 5 * time.Second
	}
	if c.OpenTimeout <= 0 {
		c.OpenTimeout = 15 * time.Second
	}
	if c.ChannelTTL <= 0 {
		c.ChannelTTL = 30 * time.Minute
	}
	if c.ListenerBuffer <= 0 {
		c.ListenerBuffer = 64
	}
	return c
}

var (
	// ErrNoChannel means the channel id is unknown (or expired).
	ErrNoChannel = errors.New("no such channel")
	// ErrReceiverBusy means we already hold PerReceiverCap sessions there.
	ErrReceiverBusy = errors.New("receiver session cap reached")
	// ErrUnknownProvider means the receiver's provider is not registered.
	ErrUnknownProvider = errors.New("unknown provider")
	// ErrClosed means the manager has shut down.
	ErrClosed = errors.New("relay closed")
)

// Manager owns all channels. Create with New and run Run in a goroutine.
type Manager struct {
	cfg       Config
	log       *slog.Logger
	providers map[string]provider.Provider

	base   context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	channels map[string]*channel
	closed   bool

	// counters, read by Stats
	opens          atomic.Int64
	openFailures   atomic.Int64
	graceCloses    atomic.Int64
	watchdogCloses atomic.Int64
	leakedLeases   atomic.Int64
	reconnects     atomic.Int64
}

// New builds a manager over the given providers.
func New(cfg Config, log *slog.Logger, providers ...provider.Provider) *Manager {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:       cfg.withDefaults(),
		log:       log,
		providers: map[string]provider.Provider{},
		base:      ctx,
		cancel:    cancel,
		channels:  map[string]*channel{},
	}
	for _, p := range providers {
		m.providers[p.ID()] = p
	}
	return m
}

// Grace returns the effective grace period.
func (m *Manager) Grace() time.Duration { return m.cfg.Grace }

type channel struct {
	id     string
	rx     model.Receiver
	freqHz int64
	mode   model.Mode
	spanHz int

	// all fields below are guarded by Manager.mu
	leases    map[*Lease]struct{}
	zeroSince time.Time // when listeners last dropped to zero; zero while listened
	grace     *time.Timer
	up        *upstream // nil when no upstream session is held
	opening   *openAttempt
	lastUsed  time.Time
	// rapid counts upstream sessions that ended soon after opening, to stop
	// a reconnect loop against a receiver that keeps dropping us.
	rapid int
}

type openAttempt struct {
	done chan struct{}
	err  error
}

type upstream struct {
	s      provider.Session
	opened time.Time
	// closing is set (under mu) when we decided to close it ourselves.
	closing bool
}

// ChannelInfo describes a channel to the API layer.
type ChannelInfo struct {
	ID       string         `json:"channelId"`
	Receiver model.Receiver `json:"receiver"`
	FreqHz   int64          `json:"freqHz"`
	Mode     model.Mode     `json:"mode"`
	SpanHz   int            `json:"spanHz"`
}

// ChannelID is the deterministic id for (receiver, frequency, mode).
func ChannelID(rx model.Receiver, freqHz int64, mode model.Mode) string {
	h := sha256.Sum256([]byte(rx.Key() + "|" + strconv.FormatInt(freqHz, 10) + "|" + string(mode)))
	return hex.EncodeToString(h[:8])
}

// Ensure returns the channel for (receiver, frequency, mode), creating the
// record if needed. It never opens anything upstream.
func (m *Manager) Ensure(rx model.Receiver, freqHz int64, mode model.Mode, spanHz int) (ChannelInfo, error) {
	if _, ok := m.providers[rx.Provider]; !ok {
		return ChannelInfo{}, fmt.Errorf("%w: %s", ErrUnknownProvider, rx.Provider)
	}
	id := ChannelID(rx, freqHz, mode)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ChannelInfo{}, ErrClosed
	}
	ch, ok := m.channels[id]
	if !ok {
		ch = &channel{id: id, rx: rx, freqHz: freqHz, mode: mode, spanHz: spanHz, leases: map[*Lease]struct{}{}}
		m.channels[id] = ch
	} else if ch.up == nil && ch.opening == nil {
		// Refresh receiver details (URL, limits) while nothing is open.
		ch.rx = rx
	}
	ch.lastUsed = time.Now()
	return ch.info(), nil
}

func (ch *channel) info() ChannelInfo {
	return ChannelInfo{ID: ch.id, Receiver: ch.rx, FreqHz: ch.freqHz, Mode: ch.mode, SpanHz: ch.spanHz}
}

// Info looks a channel up by id.
func (m *Manager) Info(id string) (ChannelInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.channels[id]
	if !ok {
		return ChannelInfo{}, false
	}
	return ch.info(), true
}

// Lease is one listener's attachment to a channel. The consumer reads Audio
// or Spectrum until Done closes, and must call Release when it stops,
// however it stops. Release is idempotent.
type Lease struct {
	m          *Manager
	ch         *channel
	ListenerID string
	Kind       Kind
	// alive is the consumer's context; once it ends the lease is dead even
	// if Release was never called (the watchdog cleans that up).
	alive context.Context

	audio chan model.AudioPacket
	spec  chan model.SpectrumRow
	done  chan struct{}
	err   error // set before done closes
	// released is guarded by Manager.mu
	released bool
}

func (l *Lease) Audio() <-chan model.AudioPacket    { return l.audio }
func (l *Lease) Spectrum() <-chan model.SpectrumRow { return l.spec }

// Done closes when the lease ends: released, or the channel failed for good.
func (l *Lease) Done() <-chan struct{} { return l.done }

// Err says why the lease ended, nil for a normal release.
func (l *Lease) Err() error {
	select {
	case <-l.done:
		return l.err
	default:
		return nil
	}
}

// Release detaches the listener. Safe to call more than once.
func (l *Lease) Release() {
	l.m.mu.Lock()
	defer l.m.mu.Unlock()
	l.m.releaseLocked(l, nil)
}

// Attach adds a listener to a channel, opening the upstream if this is the
// first listener. ctx is the listener's own lifetime (the HTTP request); it
// bounds the wait for the upstream and marks the lease dead when it ends.
func (m *Manager) Attach(ctx context.Context, channelID, listenerID string, kind Kind) (*Lease, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	ch, ok := m.channels[channelID]
	if !ok {
		m.mu.Unlock()
		return nil, ErrNoChannel
	}
	l := &Lease{
		m: m, ch: ch, ListenerID: listenerID, Kind: kind, alive: ctx,
		done: make(chan struct{}),
	}
	if kind == KindAudio {
		l.audio = make(chan model.AudioPacket, m.cfg.ListenerBuffer)
	} else {
		l.spec = make(chan model.SpectrumRow, m.cfg.ListenerBuffer)
	}

	if ch.up == nil && ch.opening == nil {
		if n := m.sessionsOnReceiverLocked(ch.rx.Key()); n >= m.cfg.PerReceiverCap {
			m.mu.Unlock()
			return nil, ErrReceiverBusy
		}
	}
	ch.leases[l] = struct{}{}
	ch.zeroSince = time.Time{}
	if ch.grace != nil {
		ch.grace.Stop()
		ch.grace = nil
	}
	ch.lastUsed = time.Now()
	if ch.up == nil && ch.opening == nil {
		m.startOpenLocked(ch, 0)
	}
	att := ch.opening
	m.mu.Unlock()

	if att == nil {
		return l, nil
	}
	select {
	case <-att.done:
		if att.err != nil {
			l.Release()
			return nil, att.err
		}
		return l, nil
	case <-l.done:
		// The open failed and ended every waiting lease, or we were released.
		if err := l.Err(); err != nil {
			return nil, err
		}
		return nil, context.Canceled
	case <-ctx.Done():
		l.Release()
		return nil, ctx.Err()
	}
}

// ReleaseListener releases every lease a listener holds on a channel (the
// explicit-leave path). It reports how many leases it released.
func (m *Manager) ReleaseListener(channelID, listenerID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.channels[channelID]
	if !ok {
		return 0
	}
	n := 0
	for l := range ch.leases {
		if l.ListenerID == listenerID {
			m.releaseLocked(l, nil)
			n++
		}
	}
	return n
}

func (m *Manager) sessionsOnReceiverLocked(key string) int {
	n := 0
	for _, c := range m.channels {
		if c.rx.Key() == key && (c.up != nil || c.opening != nil) {
			n++
		}
	}
	return n
}

// releaseLocked removes a lease; err (if any) is reported to the consumer.
func (m *Manager) releaseLocked(l *Lease, err error) {
	if l.released {
		return
	}
	l.released = true
	l.err = err
	close(l.done)
	ch := l.ch
	delete(ch.leases, l)
	if len(ch.leases) > 0 {
		return
	}
	ch.zeroSince = time.Now()
	ch.lastUsed = ch.zeroSince
	if ch.up != nil && ch.grace == nil {
		ch.grace = time.AfterFunc(m.cfg.Grace, func() { m.graceExpired(ch) })
	}
	// An open in flight notices zero listeners itself when it completes.
}

func (m *Manager) graceExpired(ch *channel) {
	m.mu.Lock()
	ch.grace = nil
	if len(ch.leases) > 0 || ch.up == nil {
		m.mu.Unlock()
		return
	}
	up := m.detachUpstreamLocked(ch)
	m.mu.Unlock()
	m.graceCloses.Add(1)
	m.log.Info("upstream released, no listeners", "channel", ch.id, "receiver", ch.rx.Key(), "freqHz", ch.freqHz, "mode", ch.mode)
	closeUpstream(up)
}

// detachUpstreamLocked marks the channel's upstream as ours to close and
// forgets it. The caller closes it outside the lock.
func (m *Manager) detachUpstreamLocked(ch *channel) *upstream {
	up := ch.up
	ch.up = nil
	if up != nil {
		up.closing = true
	}
	if ch.grace != nil {
		ch.grace.Stop()
		ch.grace = nil
	}
	return up
}

func closeUpstream(up *upstream) {
	if up == nil {
		return
	}
	_ = up.s.Close()
}

func (m *Manager) startOpenLocked(ch *channel, delay time.Duration) {
	att := &openAttempt{done: make(chan struct{})}
	ch.opening = att
	go m.open(ch, att, delay)
}

func (m *Manager) open(ch *channel, att *openAttempt, delay time.Duration) {
	if delay > 0 {
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-m.base.Done():
			t.Stop()
		}
		m.mu.Lock()
		abandon := len(ch.leases) == 0 || m.closed
		if abandon {
			ch.opening = nil
			close(att.done)
		}
		m.mu.Unlock()
		if abandon {
			return
		}
	}

	prov := m.providers[ch.rx.Provider]
	ctx, cancel := context.WithTimeout(m.base, m.cfg.OpenTimeout)
	s, err := prov.Open(ctx, ch.rx, provider.OpenRequest{FreqHz: ch.freqHz, Mode: ch.mode, SpanHz: ch.spanHz})
	cancel()

	m.mu.Lock()
	ch.opening = nil
	if err != nil {
		m.openFailures.Add(1)
		att.err = err
		close(att.done)
		for l := range ch.leases {
			m.releaseLocked(l, err)
		}
		m.mu.Unlock()
		m.log.Warn("upstream open failed", "channel", ch.id, "receiver", ch.rx.Key(), "err", err)
		return
	}
	m.opens.Add(1)
	if len(ch.leases) == 0 || m.closed {
		// Everyone left while we were connecting: never hold it idle.
		close(att.done)
		m.mu.Unlock()
		_ = s.Close()
		m.log.Info("upstream opened with no listeners left, closed at once", "channel", ch.id)
		return
	}
	up := &upstream{s: s, opened: time.Now()}
	ch.up = up
	close(att.done)
	m.mu.Unlock()
	m.log.Info("upstream opened", "channel", ch.id, "receiver", ch.rx.Key(), "freqHz", ch.freqHz, "mode", ch.mode)
	go m.pump(ch, up)
}

// pump copies upstream data to every lease of the right kind. A slow
// consumer loses its oldest queued item rather than stalling the others.
func (m *Manager) pump(ch *channel, up *upstream) {
	audio := up.s.Audio()
	spec := up.s.Spectrum()
	for audio != nil || spec != nil {
		select {
		case p, ok := <-audio:
			if !ok {
				audio = nil
				continue
			}
			m.mu.Lock()
			for l := range ch.leases {
				if l.audio != nil {
					offerAudio(l.audio, p)
				}
			}
			m.mu.Unlock()
		case r, ok := <-spec:
			if !ok {
				spec = nil
				continue
			}
			m.mu.Lock()
			for l := range ch.leases {
				if l.spec != nil {
					offerSpec(l.spec, r)
				}
			}
			m.mu.Unlock()
		case <-up.s.Done():
			audio, spec = nil, nil
		}
	}
	<-up.s.Done()
	m.upstreamEnded(ch, up)
}

func offerAudio(c chan model.AudioPacket, p model.AudioPacket) {
	select {
	case c <- p:
		return
	default:
	}
	select {
	case <-c:
	default:
	}
	select {
	case c <- p:
	default:
	}
}

func offerSpec(c chan model.SpectrumRow, r model.SpectrumRow) {
	select {
	case c <- r:
		return
	default:
	}
	select {
	case <-c:
	default:
	}
	select {
	case c <- r:
	default:
	}
}

// upstreamEnded handles a session that ended on its own (expiry, receiver
// restart, network). It reconnects only if someone is still listening.
func (m *Manager) upstreamEnded(ch *channel, up *upstream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if up.closing || ch.up != up {
		return // we closed it on purpose
	}
	ch.up = nil
	reason := up.s.Err()
	if len(ch.leases) == 0 || m.closed {
		m.log.Info("upstream ended with no listeners, not reconnecting", "channel", ch.id, "err", reason)
		return
	}
	if time.Since(up.opened) < 30*time.Second {
		ch.rapid++
	} else {
		ch.rapid = 0
	}
	var rej *provider.RejectedError
	if ch.rapid > 3 || errors.As(reason, &rej) {
		err := fmt.Errorf("upstream ended: %w", reason)
		if reason == nil {
			err = errors.New("upstream keeps dropping the session")
		}
		m.log.Warn("upstream ended, giving up", "channel", ch.id, "err", err)
		for l := range ch.leases {
			m.releaseLocked(l, err)
		}
		return
	}
	m.reconnects.Add(1)
	delay := time.Duration(ch.rapid) * 2 * time.Second
	m.log.Info("upstream ended, reconnecting for remaining listeners", "channel", ch.id, "listeners", len(ch.leases), "err", reason, "delay", delay)
	m.startOpenLocked(ch, delay)
}

// Run drives the watchdog until ctx ends, then closes everything.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.cfg.WatchdogEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.Close()
			return
		case <-t.C:
			m.Sweep()
		}
	}
}

// Sweep is one watchdog pass. Anything it has to fix is a bug elsewhere, so
// it logs at error level.
func (m *Manager) Sweep() {
	now := time.Now()
	var toClose []*upstream
	m.mu.Lock()
	for id, ch := range m.channels {
		for l := range ch.leases {
			if l.alive.Err() != nil {
				m.leakedLeases.Add(1)
				m.log.Error("BUG: listener context ended but lease never released; releasing", "channel", id, "listener", l.ListenerID, "kind", l.Kind)
				m.releaseLocked(l, nil)
			}
		}
		if ch.up != nil && len(ch.leases) == 0 && (ch.zeroSince.IsZero() || now.Sub(ch.zeroSince) > m.cfg.Grace) {
			m.watchdogCloses.Add(1)
			m.log.Error("BUG: upstream held with no listeners past the grace period; force closing", "channel", id, "receiver", ch.rx.Key(), "idleFor", now.Sub(ch.zeroSince))
			toClose = append(toClose, m.detachUpstreamLocked(ch))
		}
		if ch.up == nil && ch.opening == nil && len(ch.leases) == 0 && now.Sub(ch.lastUsed) > m.cfg.ChannelTTL {
			delete(m.channels, id)
		}
	}
	m.mu.Unlock()
	for _, up := range toClose {
		closeUpstream(up)
	}
}

// Close releases every lease and closes every upstream.
func (m *Manager) Close() {
	var toClose []*upstream
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	for _, ch := range m.channels {
		for l := range ch.leases {
			m.releaseLocked(l, ErrClosed)
		}
		toClose = append(toClose, m.detachUpstreamLocked(ch))
	}
	m.mu.Unlock()
	m.cancel()
	for _, up := range toClose {
		closeUpstream(up)
	}
}

// ChannelStats is the admin view of one channel.
type ChannelStats struct {
	ID                string     `json:"channelId"`
	Receiver          string     `json:"receiver"`
	Callsign          string     `json:"callsign"`
	FreqHz            int64      `json:"freqHz"`
	Mode              model.Mode `json:"mode"`
	AudioListeners    int        `json:"audioListeners"`
	SpectrumListeners int        `json:"spectrumListeners"`
	UpstreamOpen      bool       `json:"upstreamOpen"`
	UpstreamOpening   bool       `json:"upstreamOpening"`
	UpstreamSince     *time.Time `json:"upstreamSince,omitempty"`
	IdleSince         *time.Time `json:"idleSince,omitempty"`
	// Overdue is true when an upstream has had no listeners for longer than
	// the grace period. It should never be true; alert if it is.
	Overdue bool `json:"overdue"`
}

// Stats is the admin view of the whole relay.
type Stats struct {
	Channels         []ChannelStats `json:"channels"`
	UpstreamSessions int            `json:"upstreamSessions"`
	Listeners        int            `json:"listeners"`
	IdleUpstreams    int            `json:"idleUpstreams"`
	OverdueUpstreams int            `json:"overdueUpstreams"`
	Opens            int64          `json:"opens"`
	OpenFailures     int64          `json:"openFailures"`
	GraceCloses      int64          `json:"graceCloses"`
	WatchdogCloses   int64          `json:"watchdogCloses"`
	LeakedLeases     int64          `json:"leakedLeases"`
	Reconnects       int64          `json:"reconnects"`
	GraceSeconds     float64        `json:"graceSeconds"`
}

// Stats snapshots the relay for /admin/sessions and metrics.
func (m *Manager) Stats() Stats {
	now := time.Now()
	m.mu.Lock()
	st := Stats{GraceSeconds: m.cfg.Grace.Seconds()}
	for _, ch := range m.channels {
		cs := ChannelStats{
			ID: ch.id, Receiver: ch.rx.Key(), Callsign: ch.rx.Callsign,
			FreqHz: ch.freqHz, Mode: ch.mode,
			UpstreamOpen: ch.up != nil, UpstreamOpening: ch.opening != nil,
		}
		for l := range ch.leases {
			if l.Kind == KindAudio {
				cs.AudioListeners++
			} else {
				cs.SpectrumListeners++
			}
		}
		if ch.up != nil {
			t := ch.up.opened
			cs.UpstreamSince = &t
			st.UpstreamSessions++
		}
		if len(ch.leases) == 0 && !ch.zeroSince.IsZero() {
			t := ch.zeroSince
			cs.IdleSince = &t
		}
		if ch.up != nil && len(ch.leases) == 0 {
			st.IdleUpstreams++
			if ch.zeroSince.IsZero() || now.Sub(ch.zeroSince) > m.cfg.Grace {
				cs.Overdue = true
				st.OverdueUpstreams++
			}
		}
		st.Listeners += len(ch.leases)
		if cs.UpstreamOpen || cs.UpstreamOpening || len(ch.leases) > 0 {
			st.Channels = append(st.Channels, cs)
		}
	}
	m.mu.Unlock()
	sort.Slice(st.Channels, func(i, j int) bool { return st.Channels[i].ID < st.Channels[j].ID })
	st.Opens = m.opens.Load()
	st.OpenFailures = m.openFailures.Load()
	st.GraceCloses = m.graceCloses.Load()
	st.WatchdogCloses = m.watchdogCloses.Load()
	st.LeakedLeases = m.leakedLeases.Load()
	st.Reconnects = m.reconnects.Load()
	return st
}

// HeldOn reports whether we hold (or are opening) a session on a receiver.
// The ranker uses it to waive the free-slot filter for receivers we are
// already on.
func (m *Manager) HeldOn(receiverKey string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionsOnReceiverLocked(receiverKey) > 0
}
