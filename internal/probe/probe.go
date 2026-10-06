// Package probe checks whether scheduled signals are actually on the air.
//
// Probing is shared: one round per interval covers every live or imminent
// transmission on its top few candidate receivers, and the results serve
// every visitor. The number of probes never depends on how many people are
// using the site. Probes go through provider.Probe, which must use cheap
// HTTP endpoints and never hold a receiver session.
package probe

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
)

// Target is one (event, frequency) worth probing this round.
type Target struct {
	EventID string
	FreqHz  int64
	Mode    model.Mode
	// Live is true once the event's start time has passed.
	Live bool
	// Candidates are the receivers to probe, best first; the prober takes
	// at most TopK of them.
	Candidates []model.Receiver
}

// State is the verdict for one (event, frequency).
type State string

const (
	Unknown State = "unknown"
	Present State = "present"
	Absent  State = "absent"
)

// Signal is what the API shows for one (event, frequency).
type Signal struct {
	State       State     `json:"state"`
	SNR         float64   `json:"snr,omitempty"`
	At          time.Time `json:"at,omitzero"`
	ReceiverKey string    `json:"receiverKey,omitempty"`
}

// Options configures a Prober.
type Options struct {
	Every       time.Duration // default 60 s
	TopK        int           // default 5
	Timeout     time.Duration // per probe, default 10 s
	Concurrency int           // probes in flight at once, default 4
	// GoneAfter is how many consecutive absent rounds, after the signal was
	// seen, end a live event early. Default 2.
	GoneAfter int
	Logger    *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Every <= 0 {
		o.Every = 60 * time.Second
	}
	if o.TopK <= 0 {
		o.TopK = 5
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.GoneAfter <= 0 {
		o.GoneAfter = 2
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

type rxFreq struct {
	rx string
	hz int64
}

type evFreq struct {
	ev string
	hz int64
}

type track struct {
	seen       bool // present at least once while live
	absentRuns int
	firstGone  time.Time
}

// Prober runs probe rounds and remembers the results.
type Prober struct {
	opt       Options
	providers map[string]provider.Provider

	mu      sync.Mutex
	results map[rxFreq]provider.ProbeResult
	signals map[evFreq]Signal
	tracks  map[evFreq]*track
	ended   map[string]time.Time
	changed chan struct{}
	rounds  int64
	probes  int64
}

// New builds a prober over the given providers.
func New(opt Options, providers ...provider.Provider) *Prober {
	p := &Prober{
		opt:       opt.withDefaults(),
		providers: map[string]provider.Provider{},
		results:   map[rxFreq]provider.ProbeResult{},
		signals:   map[evFreq]Signal{},
		tracks:    map[evFreq]*track{},
		ended:     map[string]time.Time{},
		changed:   make(chan struct{}),
	}
	for _, pr := range providers {
		p.providers[pr.ID()] = pr
	}
	return p
}

// Run probes every Every until ctx ends. targets is asked for the work at
// the start of each round.
func (p *Prober) Run(ctx context.Context, targets func(now time.Time) []Target) {
	t := time.NewTicker(p.opt.Every)
	defer t.Stop()
	p.Round(ctx, targets(time.Now()))
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Round(ctx, targets(time.Now()))
		}
	}
}

// Round probes the given targets once and updates the verdicts.
func (p *Prober) Round(ctx context.Context, targets []Target) {
	type job struct {
		t  Target
		rx model.Receiver
	}
	var jobs []job
	seen := map[rxFreq]bool{}
	for _, t := range targets {
		n := 0
		for _, rx := range t.Candidates {
			if n >= p.opt.TopK {
				break
			}
			n++
			k := rxFreq{rx.Key(), t.FreqHz}
			if seen[k] {
				continue // same receiver and frequency shared by two events
			}
			seen[k] = true
			jobs = append(jobs, job{t, rx})
		}
	}

	sem := make(chan struct{}, p.opt.Concurrency)
	var wg sync.WaitGroup
	fresh := make(map[rxFreq]provider.ProbeResult, len(jobs))
	var fmu sync.Mutex
	for _, j := range jobs {
		prov, ok := p.providers[j.rx.Provider]
		if !ok {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, p.opt.Timeout)
			res, err := prov.Probe(pctx, j.rx, j.t.FreqHz, j.t.Mode)
			cancel()
			if err != nil {
				res = provider.ProbeResult{OK: false, Err: err.Error()}
			}
			if res.At.IsZero() {
				res.At = time.Now()
			}
			fmu.Lock()
			fresh[rxFreq{j.rx.Key(), j.t.FreqHz}] = res
			fmu.Unlock()
		}()
	}
	wg.Wait()

	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rounds++
	p.probes += int64(len(fresh))
	for k, v := range fresh {
		p.results[k] = v
	}
	changed := false
	live := map[evFreq]bool{}
	for _, t := range targets {
		key := evFreq{t.EventID, t.FreqHz}
		live[key] = true
		sig := p.verdictLocked(t)
		if old := p.signals[key]; old.State != sig.State || old.ReceiverKey != sig.ReceiverKey {
			changed = true
		}
		p.signals[key] = sig
		if !t.Live {
			continue
		}
		tr := p.tracks[key]
		if tr == nil {
			tr = &track{}
			p.tracks[key] = tr
		}
		switch sig.State {
		case Present:
			tr.seen, tr.absentRuns = true, 0
			if _, ok := p.ended[t.EventID]; ok {
				delete(p.ended, t.EventID) // came back: not over after all
				changed = true
			}
		case Absent:
			if tr.seen {
				if tr.absentRuns == 0 {
					tr.firstGone = sig.At
				}
				tr.absentRuns++
				if tr.absentRuns >= p.opt.GoneAfter {
					if _, ok := p.ended[t.EventID]; !ok {
						p.ended[t.EventID] = tr.firstGone
						changed = true
						p.opt.Logger.Info("signal gone, ending event early", "event", t.EventID, "freqHz", t.FreqHz, "at", tr.firstGone)
					}
				}
			}
		}
	}
	// Forget what is no longer being probed, after a day.
	for k := range p.tracks {
		if !live[k] && now.Sub(p.signals[k].At) > 24*time.Hour {
			delete(p.tracks, k)
			delete(p.signals, k)
		}
	}
	for k, v := range p.results {
		if now.Sub(v.At) > 24*time.Hour {
			delete(p.results, k)
		}
	}
	if changed {
		close(p.changed)
		p.changed = make(chan struct{})
	}
}

// verdictLocked combines the candidates' latest results for one target.
func (p *Prober) verdictLocked(t Target) Signal {
	best := Signal{State: Unknown}
	anyOK := false
	n := 0
	for _, rx := range t.Candidates {
		if n >= p.opt.TopK {
			break
		}
		n++
		r, ok := p.results[rxFreq{rx.Key(), t.FreqHz}]
		if !ok || !r.OK {
			continue
		}
		anyOK = true
		if r.Present && (best.State != Present || r.SNR > best.SNR) {
			best = Signal{State: Present, SNR: r.SNR, At: r.At, ReceiverKey: rx.Key()}
		}
		if best.State != Present && (best.At.IsZero() || r.At.After(best.At)) {
			best.At = r.At
		}
	}
	if best.State != Present && anyOK {
		best.State = Absent
	}
	if best.State == Unknown {
		best.At = time.Time{}
	}
	return best
}

// Result is the latest probe of a receiver at a frequency.
func (p *Prober) Result(receiverKey string, hz int64) (provider.ProbeResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.results[rxFreq{receiverKey, hz}]
	return r, ok
}

// Results returns the latest probe for each receiver at hz, for ranking.
func (p *Prober) Results(hz int64) map[string]provider.ProbeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]provider.ProbeResult{}
	for k, v := range p.results {
		if k.hz == hz {
			out[k.rx] = v
		}
	}
	return out
}

// Signal is the verdict for an event's frequency.
func (p *Prober) Signal(eventID string, hz int64) Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.signals[evFreq{eventID, hz}]; ok {
		return s
	}
	return Signal{State: Unknown}
}

// EndedAt reports when probes saw the event's carrier go, if they did.
func (p *Prober) EndedAt(eventID string) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.ended[eventID]
	return t, ok
}

// Changed returns a channel closed at the next verdict change.
func (p *Prober) Changed() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed
}

// Counts reports rounds and probes made, for metrics.
func (p *Prober) Counts() (rounds, probes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rounds, p.probes
}
