// Package directory keeps the merged set of receivers from every provider,
// refreshed on a fixed cadence. When a provider's listing fails, its last
// good list stays in place so one flaky directory does not empty the site.
package directory

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
)

// Defaults for Options.
const (
	DefaultInterval = 5 * time.Minute
	DefaultTimeout  = 30 * time.Second
)

// Options configures a Service. Zero values take the defaults.
type Options struct {
	// Interval between refreshes; DefaultInterval (5 min) when zero.
	Interval time.Duration
	// Timeout for one provider's List call; DefaultTimeout (30 s) when
	// zero.
	Timeout time.Duration
	// Logger; slog.Default() when nil.
	Logger *slog.Logger
}

// ProviderStatus describes one provider's last refresh, for admin pages.
type ProviderStatus struct {
	ID string `json:"id"`
	// Count is how many receivers of this provider are in the merged set.
	Count       int       `json:"count"`
	LastAttempt time.Time `json:"lastAttempt"`
	// LastOK is when List last succeeded; zero if never.
	LastOK time.Time `json:"lastOk"`
	// LastErr is the error from the most recent attempt, "" if it worked.
	LastErr string `json:"lastErr,omitempty"`
}

type providerState struct {
	p      provider.Provider
	list   []model.Receiver // last good list
	status ProviderStatus
}

// Service polls providers and serves the merged receiver set. It is safe
// for concurrent use.
type Service struct {
	interval time.Duration
	timeout  time.Duration
	log      *slog.Logger

	mu      sync.RWMutex
	states  []*providerState
	merged  []model.Receiver // sorted by key
	byKey   map[string]int   // key -> index into merged
	updated time.Time
	changed chan struct{}
}

// New builds a Service over the given providers. Nothing is fetched until
// Run or Refresh.
func New(providers []provider.Provider, opts Options) *Service {
	s := &Service{
		interval: opts.Interval,
		timeout:  opts.Timeout,
		log:      opts.Logger,
		byKey:    map[string]int{},
		changed:  make(chan struct{}),
	}
	if s.interval <= 0 {
		s.interval = DefaultInterval
	}
	if s.timeout <= 0 {
		s.timeout = DefaultTimeout
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	for _, p := range providers {
		s.states = append(s.states, &providerState{p: p, status: ProviderStatus{ID: p.ID()}})
	}
	return s
}

// Run refreshes immediately, then every Interval, until ctx is done. It
// returns ctx.Err().
func (s *Service) Run(ctx context.Context) error {
	s.Refresh(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.Refresh(ctx)
		}
	}
}

// Refresh asks every provider for its receivers once, concurrently, and
// merges the results. A provider that fails keeps its last good list.
func (s *Service) Refresh(ctx context.Context) {
	type result struct {
		list []model.Receiver
		err  error
		at   time.Time
	}
	results := make([]result, len(s.states))
	var wg sync.WaitGroup
	for i, st := range s.states {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, s.timeout)
			defer cancel()
			list, err := st.p.List(cctx)
			results[i] = result{list: list, err: err, at: time.Now()}
		}()
	}
	wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	anyOK := false
	for i, st := range s.states {
		r := results[i]
		st.status.LastAttempt = r.at
		if r.err != nil {
			st.status.LastErr = r.err.Error()
			s.log.Warn("directory: provider list failed, keeping last good list",
				"provider", st.status.ID, "err", r.err, "kept", len(st.list))
			continue
		}
		anyOK = true
		st.status.LastErr = ""
		st.status.LastOK = r.at
		st.list = normalise(st.status.ID, r.list)
	}
	if anyOK {
		s.updated = time.Now()
	}

	merged := make([]model.Receiver, 0, len(s.merged))
	for _, st := range s.states {
		merged = append(merged, st.list...)
		st.status.Count = len(st.list)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Key() < merged[j].Key() })
	if slices.Equal(merged, s.merged) {
		return
	}
	s.merged = merged
	s.byKey = make(map[string]int, len(merged))
	for i, r := range merged {
		s.byKey[r.Key()] = i
	}
	close(s.changed)
	s.changed = make(chan struct{})
}

// normalise fills in a missing provider id and drops duplicate keys (the
// first occurrence wins).
func normalise(id string, in []model.Receiver) []model.Receiver {
	out := make([]model.Receiver, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, r := range in {
		if r.Provider == "" {
			r.Provider = id
		}
		if seen[r.Key()] {
			continue
		}
		seen[r.Key()] = true
		out = append(out, r)
	}
	return out
}

// Receivers returns a copy of the merged set, sorted by key.
func (s *Service) Receivers() []model.Receiver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.merged)
}

// Get looks a receiver up by Key().
func (s *Service) Get(key string) (model.Receiver, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i, ok := s.byKey[key]
	if !ok {
		return model.Receiver{}, false
	}
	return s.merged[i], true
}

// Updated is when any provider last listed successfully; zero before the
// first success.
func (s *Service) Updated() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.updated
}

// Changed returns a channel that is closed the next time the merged set
// changes. Take a fresh channel after each wake-up:
//
//	for {
//		ch := svc.Changed()
//		use(svc.Receivers())
//		select { case <-ch: case <-ctx.Done(): return }
//	}
func (s *Service) Changed() <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.changed
}

// Status reports each provider's last refresh, in provider order.
func (s *Service) Status() []ProviderStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ProviderStatus, len(s.states))
	for i, st := range s.states {
		out[i] = st.status
	}
	return out
}
