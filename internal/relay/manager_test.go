package relay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
)

// fakeProvider records every session it opens. Everything runs inside a
// synctest bubble, so time only moves when every goroutine is blocked.
type fakeProvider struct {
	mu       sync.Mutex
	sessions []*fakeSession
	openErr  error
	// gate, if set, blocks Open until it is closed.
	gate chan struct{}
}

func (p *fakeProvider) ID() string { return "fake" }
func (p *fakeProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{LiveSpectrum: true}
}
func (p *fakeProvider) List(context.Context) ([]model.Receiver, error) { return nil, nil }
func (p *fakeProvider) Probe(context.Context, model.Receiver, int64, model.Mode) (provider.ProbeResult, error) {
	return provider.ProbeResult{}, nil
}
func (p *fakeProvider) DeepLink(model.Receiver, int64, model.Mode) string { return "" }
func (p *fakeProvider) Spectrogram(context.Context, model.Receiver, int64, int, int) ([]byte, error) {
	return nil, provider.ErrUnsupported
}

func (p *fakeProvider) Open(ctx context.Context, r model.Receiver, req provider.OpenRequest) (provider.Session, error) {
	if p.gate != nil {
		select {
		case <-p.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.openErr != nil {
		return nil, p.openErr
	}
	s := &fakeSession{
		rx: r.Key(), req: req, openedAt: time.Now(),
		audio: make(chan model.AudioPacket, 16),
		spec:  make(chan model.SpectrumRow, 16),
		done:  make(chan struct{}),
	}
	p.mu.Lock()
	p.sessions = append(p.sessions, s)
	p.mu.Unlock()
	return s, nil
}

func (p *fakeProvider) all() []*fakeSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*fakeSession(nil), p.sessions...)
}

func (p *fakeProvider) open() []*fakeSession {
	var out []*fakeSession
	for _, s := range p.all() {
		if !s.isClosed() {
			out = append(out, s)
		}
	}
	return out
}

type fakeSession struct {
	rx       string
	req      provider.OpenRequest
	openedAt time.Time

	audio chan model.AudioPacket
	spec  chan model.SpectrumRow
	done  chan struct{}

	mu       sync.Mutex
	closedAt time.Time
	closed   bool
	err      error
	byUs     bool
}

func (s *fakeSession) Audio() <-chan model.AudioPacket    { return s.audio }
func (s *fakeSession) Spectrum() <-chan model.SpectrumRow { return s.spec }
func (s *fakeSession) Done() <-chan struct{}              { return s.done }
func (s *fakeSession) MaxDuration() time.Duration         { return time.Hour }
func (s *fakeSession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
func (s *fakeSession) Close() error {
	s.end(nil, true)
	return nil
}

// end finishes the session; byUs distinguishes our Close from upstream ending.
func (s *fakeSession) end(err error, byUs bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed, s.closedAt, s.err, s.byUs = true, time.Now(), err, byUs
	close(s.audio)
	close(s.spec)
	close(s.done)
}

func (s *fakeSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *fakeSession) closedTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closedAt
}

var rx1 = model.Receiver{Provider: "fake", ID: "rx1", Callsign: "M0LTE"}
var rx2 = model.Receiver{Provider: "fake", ID: "rx2", Callsign: "G0XYZ"}

type rig struct {
	t    *testing.T
	p    *fakeProvider
	m    *Manager
	stop context.CancelFunc
}

func newRig(t *testing.T, cfg Config) *rig {
	t.Helper()
	p := &fakeProvider{}
	m := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), p)
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	r := &rig{t: t, p: p, m: m, stop: cancel}
	t.Cleanup(func() {
		cancel()
		synctest.Wait()
	})
	return r
}

func (r *rig) channel(rx model.Receiver, hz int64) string {
	r.t.Helper()
	info, err := r.m.Ensure(rx, hz, model.ModeUSB, 10000)
	if err != nil {
		r.t.Fatal(err)
	}
	return info.ID
}

func (r *rig) attach(ctx context.Context, id, listener string, kind Kind) *Lease {
	r.t.Helper()
	l, err := r.m.Attach(ctx, id, listener, kind)
	if err != nil {
		r.t.Fatalf("attach: %v", err)
	}
	return l
}

func (r *rig) wantOpen(n int) {
	r.t.Helper()
	synctest.Wait()
	if got := len(r.p.open()); got != n {
		r.t.Fatalf("open upstream sessions = %d, want %d", got, n)
	}
}

// wantClosedAfter asserts s closed exactly `after` the reference instant.
func wantClosedAfter(t *testing.T, s *fakeSession, ref time.Time, after time.Duration) {
	t.Helper()
	if !s.isClosed() {
		t.Fatalf("session still open")
	}
	if got := s.closedTime().Sub(ref); got != after {
		t.Fatalf("session closed %v after the last listener left, want %v", got, after)
	}
}

func TestNothingOpensBeforePlay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Config{})
		r.channel(rx1, 7_910_000)
		r.channel(rx2, 8_175_000)
		time.Sleep(time.Hour)
		r.wantOpen(0)
		if n := len(r.p.all()); n != 0 {
			t.Fatalf("provider saw %d opens with no listener", n)
		}
		if st := r.m.Stats(); st.UpstreamSessions != 0 || st.Opens != 0 {
			t.Fatalf("stats show upstream activity: %+v", st)
		}
	})
}

func TestCleanDisconnectClosesAtGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Config{})
		id := r.channel(rx1, 7_910_000)
		l := r.attach(t.Context(), id, "a", KindAudio)
		r.wantOpen(1)
		s := r.p.all()[0]

		time.Sleep(3 * time.Minute) // listening for a while
		r.wantOpen(1)

		l.Release()
		left := time.Now()
		time.Sleep(10*time.Second - time.Nanosecond)
		r.wantOpen(1) // grace absorbs reloads
		time.Sleep(time.Nanosecond)
		r.wantOpen(0)
		wantClosedAfter(t, s, left, 10*time.Second)
		if st := r.m.Stats(); st.WatchdogCloses != 0 || st.GraceCloses != 1 {
			t.Fatalf("expected a normal grace close, got %+v", st)
		}
	})
}

// The HTTP handler's pattern: the request context ends (client vanished)
// and the handler's deferred Release runs.
func TestAbruptDropClosesAtGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Config{})
		id := r.channel(rx1, 7_910_000)
		ctx, drop := context.WithCancel(t.Context())
		l := r.attach(ctx, id, "a", KindAudio)
		go func() {
			defer l.Release()
			for {
				select {
				case <-l.Audio():
				case <-ctx.Done():
					return
				case <-l.Done():
					return
				}
			}
		}()
		r.wantOpen(1)
		s := r.p.all()[0]
		time.Sleep(time.Minute)
		drop()
		dropped := time.Now()
		time.Sleep(time.Minute)
		r.wantOpen(0)
		wantClosedAfter(t, s, dropped, 10*time.Second)
	})
}

func TestPauseReleasesAudioAndSpectrum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Config{})
		id := r.channel(rx1, 7_910_000)
		a := r.attach(t.Context(), id, "a", KindAudio)
		sp := r.attach(t.Context(), id, "a", KindSpectrum)
		r.wantOpen(1)
		s := r.p.all()[0]

		a.Release()
		time.Sleep(time.Minute)
		r.wantOpen(1) // the spectrum WS still counts as listening

		sp.Release()
		paused := time.Now()
		time.Sleep(time.Minute)
		r.wantOpen(0)
		wantClosedAfter(t, s, paused, 10*time.Second)

		// Resume rejoins with a fresh upstream.
		a2 := r.attach(t.Context(), id, "a", KindAudio)
		r.wantOpen(1)
		a2.Release()
	})
}

func TestReceiverSwitch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Config{})
		idA := r.channel(rx1, 7_910_000)
		idB := r.channel(rx2, 7_910_000)
		la := r.attach(t.Context(), idA, "a", KindAudio)
		r.wantOpen(1)
		sa := r.p.all()[0]

		// The frontend's switch: explicit leave (beacon) then join B.
		if n := r.m.ReleaseListener(idA, "a"); n != 1 {
			t.Fatalf("released %d leases, want 1", n)
		}
		switched := time.Now()
		select {
		case <-la.Done():
		default:
			t.Fatal("lease not ended by explicit leave")
		}
		lb := r.attach(t.Context(), idB, "a", KindAudio)
		time.Sleep(time.Minute)
		r.wantOpen(1)
		wantClosedAfter(t, sa, switched, 10*time.Second)
		if got := r.p.open()[0].rx; got != rx2.Key() {
			t.Fatalf("open session is on %s, want %s", got, rx2.Key())
		}
		lb.Release()
	})
}

func TestExpiryWithNoListenersDoesNotReconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Config{})
		id := r.channel(rx1, 7_910_000)
		l := r.attach(t.Context(), id, "a", KindAudio)
		r.wantOpen(1)
		s := r.p.all()[0]
		l.Release()
		time.Sleep(5 * time.Second)
		// max_session_time runs out during the grace period
		s.end(errors.New("session time limit"), false)
		time.Sleep(time.Minute)
		r.wantOpen(0)
		if n := len(r.p.all()); n != 1 {
			t.Fatalf("reconnected with no listeners: %d opens", n)
		}
	})
}

func TestExpiryWithListenersReconnects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Config{})
		id := r.channel(rx1, 7_910_000)
		l := r.attach(t.Context(), id, "a", KindAudio)
		r.wantOpen(1)
		time.Sleep(time.Hour)
		r.p.all()[0].end(errors.New("session time limit"), false)
		synctest.Wait()
		r.wantOpen(1)
		if n := len(r.p.all()); n != 2 {
			t.Fatalf("opens = %d, want 2 (one reconnect)", n)
		}
		select {
		case <-l.Done():
			t.Fatal("lease ended across a reconnect")
		default:
		}
		l.Release()
		time.Sleep(time.Minute)
		r.wantOpen(0)
	})
}

func TestWatchdogCatchesLeakedLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Config{})
		id := r.channel(rx1, 7_910_000)
		ctx, cancel := context.WithCancel(t.Context())
		r.attach(ctx, id, "leaky", KindAudio) // nobody will ever call Release
		r.wantOpen(1)
		s := r.p.all()[0]
		cancel()
		gone := time.Now()
		time.Sleep(time.Minute)
		r.wantOpen(0)
		st := r.m.Stats()
		if st.LeakedLeases != 1 {
			t.Fatalf("leaked leases = %d, want 1", st.LeakedLeases)
		}
		// Found on the next 5 s sweep, then the usual grace.
		if d := s.closedTime().Sub(gone); d > 5*time.Second+10*time.Second {
			t.Fatalf("leak took %v to clear", d)
		}
	})
}

// Simulate a refcount bug directly: drop the lease bookkeeping without the
// grace timer, as a buggy code path would, and check the watchdog notices.
func TestWatchdogForceClosesIdleUpstream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Config{})
		id := r.channel(rx1, 7_910_000)
		r.attach(t.Context(), id, "a", KindAudio)
		r.wantOpen(1)
		s := r.p.all()[0]

		r.m.mu.Lock()
		ch := r.m.channels[id]
		for l := range ch.leases {
			delete(ch.leases, l) // the bug: lease gone, no zeroSince, no timer
		}
		r.m.mu.Unlock()
		leaked := time.Now()

		time.Sleep(time.Minute)
		r.wantOpen(0)
		if st := r.m.Stats(); st.WatchdogCloses != 1 {
			t.Fatalf("watchdog closes = %d, want 1", st.WatchdogCloses)
		}
		if d := s.closedTime().Sub(leaked); d > 5*time.Second {
			t.Fatalf("watchdog took %v", d)
		}
	})
}

func TestFanOutSharesOneUpstream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Config{})
		id := r.channel(rx1, 7_910_000)
		var leases []*Lease
		for _, who := range []string{"a", "b", "c"} {
			leases = append(leases, r.attach(t.Context(), id, who, KindAudio))
		}
		r.wantOpen(1)
		s := r.p.all()[0]
		s.audio <- model.AudioPacket{Opus: []byte{1, 2, 3}}
		synctest.Wait()
		for i, l := range leases {
			select {
			case p := <-l.Audio():
				if len(p.Opus) != 3 {
					t.Fatalf("listener %d got %v", i, p.Opus)
				}
			default:
				t.Fatalf("listener %d got nothing", i)
			}
		}
		leases[0].Release()
		leases[1].Release()
		time.Sleep(time.Minute)
		r.wantOpen(1)
		leases[2].Release()
		time.Sleep(time.Minute)
		r.wantOpen(0)
	})
}

func TestPerReceiverCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Config{PerReceiverCap: 2})
		l1 := r.attach(t.Context(), r.channel(rx1, 7_000_000), "a", KindAudio)
		l2 := r.attach(t.Context(), r.channel(rx1, 8_000_000), "b", KindAudio)
		if _, err := r.m.Attach(t.Context(), r.channel(rx1, 9_000_000), "c", KindAudio); !errors.Is(err, ErrReceiverBusy) {
			t.Fatalf("third session on one receiver: err = %v, want ErrReceiverBusy", err)
		}
		// Other receivers are unaffected.
		l3 := r.attach(t.Context(), r.channel(rx2, 9_000_000), "c", KindAudio)
		r.wantOpen(3)
		l1.Release()
		l2.Release()
		l3.Release()
	})
}

func TestListenerLeavesWhileOpening(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Config{})
		r.p.gate = make(chan struct{})
		id := r.channel(rx1, 7_910_000)
		ctx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		go func() {
			_, err := r.m.Attach(ctx, id, "a", KindAudio)
			errc <- err
		}()
		synctest.Wait()
		cancel() // user gave up before the receiver answered
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("attach err = %v", err)
		}
		close(r.p.gate)
		synctest.Wait()
		r.wantOpen(0)
		if n := len(r.p.all()); n != 1 {
			t.Fatalf("opens = %d", n)
		}
	})
}

func TestOpenFailureEndsWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Config{})
		r.p.openErr = &provider.RejectedError{Reason: "full"}
		id := r.channel(rx1, 7_910_000)
		_, err := r.m.Attach(t.Context(), id, "a", KindAudio)
		var rej *provider.RejectedError
		if !errors.As(err, &rej) {
			t.Fatalf("err = %v, want RejectedError", err)
		}
		r.wantOpen(0)
	})
}

func TestGraceIsCapped(t *testing.T) {
	m := New(Config{Grace: time.Hour}, nil)
	if m.Grace() != MaxGrace {
		t.Fatalf("grace = %v, want %v", m.Grace(), MaxGrace)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Config{})
		id := r.channel(rx1, 7_910_000)
		l := r.attach(t.Context(), id, "a", KindAudio)
		l2 := r.attach(t.Context(), id, "b", KindAudio)
		l.Release()
		l.Release()
		r.m.ReleaseListener(id, "a")
		time.Sleep(time.Minute)
		r.wantOpen(1) // b still listening
		l2.Release()
		time.Sleep(time.Minute)
		r.wantOpen(0)
	})
}
