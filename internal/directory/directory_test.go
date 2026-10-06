package directory

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

// fakeProvider answers List from a script, one entry per call; the last
// entry repeats. A nil list with a nil error in the script means "block
// until the context ends".
type fakeProvider struct {
	id string

	mu     sync.Mutex
	script []step
	calls  []time.Time
}

type step struct {
	list  []model.Receiver
	err   error
	block bool
}

func (f *fakeProvider) ID() string                          { return f.id }
func (f *fakeProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

func (f *fakeProvider) List(ctx context.Context) ([]model.Receiver, error) {
	f.mu.Lock()
	f.calls = append(f.calls, time.Now())
	n := len(f.calls) - 1
	if n >= len(f.script) {
		n = len(f.script) - 1
	}
	s := f.script[n]
	f.mu.Unlock()
	if s.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.list, s.err
}

func (f *fakeProvider) callTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.calls...)
}

func (f *fakeProvider) Probe(context.Context, model.Receiver, int64, model.Mode) (provider.ProbeResult, error) {
	return provider.ProbeResult{}, provider.ErrUnsupported
}
func (f *fakeProvider) Open(context.Context, model.Receiver, provider.OpenRequest) (provider.Session, error) {
	return nil, provider.ErrUnsupported
}
func (f *fakeProvider) DeepLink(model.Receiver, int64, model.Mode) string { return "" }
func (f *fakeProvider) Spectrogram(context.Context, model.Receiver, int64, int, int) ([]byte, error) {
	return nil, provider.ErrUnsupported
}

func recv(prov, id string, avail int) model.Receiver {
	return model.Receiver{Provider: prov, ID: id, Callsign: id, Online: true, MaxClients: 20, AvailableClients: avail}
}

func quiet() Options { return Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))} }

func ids(rs []model.Receiver) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Key()
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Run lists at once, then every 5 minutes on the dot, and stops with ctx.
func TestRunCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &fakeProvider{id: "ubersdr", script: []step{{list: []model.Receiver{recv("ubersdr", "a", 1)}}}}
		s := New([]provider.Provider{p}, quiet())
		start := time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()

		time.Sleep(14*time.Minute + 59*time.Second)
		synctest.Wait()
		var offsets []time.Duration
		for _, c := range p.callTimes() {
			offsets = append(offsets, c.Sub(start))
		}
		want := []time.Duration{0, 5 * time.Minute, 10 * time.Minute}
		if len(offsets) != len(want) {
			t.Fatalf("calls at %v, want %v", offsets, want)
		}
		for i := range want {
			if offsets[i] != want[i] {
				t.Errorf("call %d at %v, want %v", i, offsets[i], want[i])
			}
		}
		if got := s.Updated(); !got.Equal(start.Add(10 * time.Minute)) {
			t.Errorf("Updated %v, want start+10m", got.Sub(start))
		}

		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v", err)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if n := len(p.callTimes()); n != 3 {
			t.Errorf("%d calls after cancel, want 3", n)
		}
	})
}

func TestIntervalConfigurable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &fakeProvider{id: "ubersdr", script: []step{{list: nil}}}
		opts := quiet()
		opts.Interval = time.Minute
		s := New([]provider.Provider{p}, opts)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.Run(ctx)
		time.Sleep(3*time.Minute + time.Second)
		synctest.Wait()
		if n := len(p.callTimes()); n != 4 {
			t.Errorf("%d calls in 3 minutes at 1-minute interval, want 4", n)
		}
	})
}

// A failing provider keeps its last good list until it recovers.
func TestKeepLastGoodOnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		good := []model.Receiver{recv("ubersdr", "a", 3), recv("ubersdr", "b", 4)}
		recovered := []model.Receiver{recv("ubersdr", "c", 5)}
		p := &fakeProvider{id: "ubersdr", script: []step{
			{list: good},
			{err: errors.New("directory 502")},
			{block: true}, // times out
			{list: recovered},
		}}
		s := New([]provider.Provider{p}, quiet())
		start := time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.Run(ctx)

		synctest.Wait() // first refresh done
		if got := ids(s.Receivers()); !equalStrings(got, []string{"ubersdr:a", "ubersdr:b"}) {
			t.Fatalf("after first list: %v", got)
		}
		firstOK := s.Updated()

		time.Sleep(5 * time.Minute) // error
		synctest.Wait()
		if got := ids(s.Receivers()); !equalStrings(got, []string{"ubersdr:a", "ubersdr:b"}) {
			t.Errorf("after error: %v, want last good list kept", got)
		}
		if !s.Updated().Equal(firstOK) {
			t.Errorf("Updated moved on a failed refresh")
		}
		st := s.Status()[0]
		if st.LastErr != "directory 502" || st.Count != 2 || !st.LastOK.Equal(start) || !st.LastAttempt.Equal(start.Add(5*time.Minute)) {
			t.Errorf("status %+v", st)
		}

		time.Sleep(5*time.Minute + DefaultTimeout) // hang, cut off by the timeout
		synctest.Wait()
		if got := ids(s.Receivers()); !equalStrings(got, []string{"ubersdr:a", "ubersdr:b"}) {
			t.Errorf("after timeout: %v, want last good list kept", got)
		}
		if st := s.Status()[0]; st.LastErr != context.DeadlineExceeded.Error() || !st.LastAttempt.Equal(start.Add(10*time.Minute+DefaultTimeout)) {
			t.Errorf("status after timeout %+v", st)
		}

		time.Sleep(5 * time.Minute) // recovered (tick at 15m)
		synctest.Wait()
		if got := ids(s.Receivers()); !equalStrings(got, []string{"ubersdr:c"}) {
			t.Errorf("after recovery: %v", got)
		}
		if _, ok := s.Get("ubersdr:a"); ok {
			t.Error("stale receiver still found by Get")
		}
		if st := s.Status()[0]; st.LastErr != "" || !s.Updated().Equal(start.Add(15*time.Minute)) {
			t.Errorf("status after recovery %+v, updated %v", st, s.Updated().Sub(start))
		}
	})
}

// Providers merge into one key-sorted set; one failing does not disturb the
// other, and an empty provider id is filled in.
func TestMergeProviders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		u := &fakeProvider{id: "ubersdr", script: []step{
			{list: []model.Receiver{recv("ubersdr", "z", 1), recv("ubersdr", "m", 2), recv("ubersdr", "m", 9)}},
			{err: errors.New("down")},
		}}
		k := &fakeProvider{id: "kiwisdr", script: []step{
			{list: []model.Receiver{recv("", "k1", 7)}},
			{list: []model.Receiver{recv("kiwisdr", "k1", 6), recv("kiwisdr", "k2", 1)}},
		}}
		s := New([]provider.Provider{u, k}, quiet())
		s.Refresh(context.Background())
		want := []string{"kiwisdr:k1", "ubersdr:m", "ubersdr:z"}
		if got := ids(s.Receivers()); !equalStrings(got, want) {
			t.Fatalf("merged %v, want %v", got, want)
		}
		if r, ok := s.Get("ubersdr:m"); !ok || r.AvailableClients != 2 {
			t.Errorf("Get m = %+v %v, want first occurrence kept", r, ok)
		}
		if r, ok := s.Get("kiwisdr:k1"); !ok || r.Provider != "kiwisdr" {
			t.Errorf("Get k1 = %+v %v", r, ok)
		}

		s.Refresh(context.Background())
		want = []string{"kiwisdr:k1", "kiwisdr:k2", "ubersdr:m", "ubersdr:z"}
		if got := ids(s.Receivers()); !equalStrings(got, want) {
			t.Errorf("merged %v, want %v", got, want)
		}
		if r, _ := s.Get("kiwisdr:k1"); r.AvailableClients != 6 {
			t.Errorf("k1 not updated: %+v", r)
		}
		st := s.Status()
		if st[0].ID != "ubersdr" || st[0].LastErr != "down" || st[0].Count != 2 || st[1].Count != 2 {
			t.Errorf("status %+v", st)
		}

		// Callers get a copy.
		rs := s.Receivers()
		rs[0].Callsign = "MUTATED"
		if r, _ := s.Get(rs[0].Key()); r.Callsign == "MUTATED" {
			t.Error("Receivers returned shared storage")
		}
	})
}

// Changed fires when the set changes and not when a refresh brings
// identical data or only errors.
func TestChanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &fakeProvider{id: "ubersdr", script: []step{
			{list: []model.Receiver{recv("ubersdr", "a", 3)}},
			{list: []model.Receiver{recv("ubersdr", "a", 3)}},
			{err: errors.New("down")},
			{list: []model.Receiver{recv("ubersdr", "a", 2)}},
		}}
		s := New([]provider.Provider{p}, quiet())

		ch0 := s.Changed()
		if closed(ch0) {
			t.Fatal("Changed closed before any refresh")
		}
		// A subscriber blocked on the channel is woken by the change.
		woke := make(chan []model.Receiver, 1)
		go func() {
			<-ch0
			woke <- s.Receivers()
		}()
		s.Refresh(context.Background())
		synctest.Wait()
		select {
		case rs := <-woke:
			if len(rs) != 1 {
				t.Errorf("woken subscriber saw %d receivers", len(rs))
			}
		default:
			t.Fatal("subscriber not woken by first list")
		}

		ch1 := s.Changed()
		s.Refresh(context.Background()) // identical
		if closed(ch1) {
			t.Error("Changed fired for identical data")
		}
		s.Refresh(context.Background()) // error, last good kept
		if closed(ch1) {
			t.Error("Changed fired for a failed refresh")
		}
		s.Refresh(context.Background()) // free slots changed
		if !closed(ch1) {
			t.Error("Changed did not fire when a receiver changed")
		}
		if closed(s.Changed()) {
			t.Error("fresh Changed channel already closed")
		}
	})
}

func TestEmptyService(t *testing.T) {
	s := New(nil, quiet())
	s.Refresh(context.Background())
	if len(s.Receivers()) != 0 || !s.Updated().IsZero() || len(s.Status()) != 0 {
		t.Error("empty service not empty")
	}
	if _, ok := s.Get("ubersdr:x"); ok {
		t.Error("Get found something in an empty service")
	}
}
