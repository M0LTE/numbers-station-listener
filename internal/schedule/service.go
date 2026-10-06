package schedule

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/stations"
)

// MinPollGap is the etiquette floor: Priyom is never asked more often than
// this, whether polling normally or retrying after a failure.
const MinPollGap = 2 * time.Minute

// Config tunes the Service. Zero values take the defaults.
type Config struct {
	// BaseURL of the feed. Default DefaultBaseURL.
	BaseURL string
	// UserAgent sent to Priyom; should name the site and a contact.
	UserAgent string
	// Interval between successful polls. Default 15 min, never below MinPollGap.
	Interval time.Duration
	// RetryMin is the first retry delay after a failure; it doubles per
	// consecutive failure up to Interval. Default and floor MinPollGap.
	RetryMin time.Duration
	// RequestTimeout bounds one HTTP request. Default 30 s.
	RequestTimeout time.Duration
	// SnapshotPath is where the last good data is persisted; empty disables
	// persistence.
	SnapshotPath string
	// Retention is how long an event is kept after it ended. Default 24 h.
	Retention time.Duration
	// Fetcher overrides the HTTP fetcher (tests). HTTPClient is used by the
	// default fetcher when set.
	Fetcher    Fetcher
	HTTPClient *http.Client
}

func (c Config) withDefaults() Config {
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.Interval <= 0 {
		c.Interval = 15 * time.Minute
	}
	c.Interval = max(c.Interval, MinPollGap)
	c.RetryMin = max(c.RetryMin, MinPollGap)
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 30 * time.Second
	}
	if c.Retention <= 0 {
		c.Retention = 24 * time.Hour
	}
	if c.Fetcher == nil {
		c.Fetcher = &HTTPFetcher{BaseURL: c.BaseURL, UserAgent: c.UserAgent, Client: c.HTTPClient}
	}
	return c
}

// Service keeps the merged Priyom schedule for today and tomorrow (UTC) in
// memory. Create with New, then run Run in a goroutine.
type Service struct {
	cfg Config
	cat *stations.Catalog
	log *slog.Logger

	mu      sync.RWMutex
	events  map[string]Event
	updated time.Time
	changed chan struct{}

	// raw keeps the items behind events, for the snapshot.
	raw map[string]Item

	logMu      sync.Mutex
	loggedMode map[string]bool
	loggedRaw  map[string]bool
}

// New builds a Service and loads the snapshot, if any, so Events works
// before the first poll.
func New(cfg Config, cat *stations.Catalog, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	s := &Service{
		cfg:        cfg.withDefaults(),
		cat:        cat,
		log:        log,
		events:     map[string]Event{},
		raw:        map[string]Item{},
		changed:    make(chan struct{}),
		loggedMode: map[string]bool{},
		loggedRaw:  map[string]bool{},
	}
	s.loadSnapshot()
	return s
}

// Run polls until ctx ends: immediately, then every Interval; after a
// failure it retries with doubling backoff (RetryMin up to Interval). It
// always returns ctx.Err().
func (s *Service) Run(ctx context.Context) error {
	failures := 0
	for {
		wait := s.cfg.Interval
		if err := s.Poll(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures++
			wait = s.backoff(failures)
			s.log.Warn("priyom poll failed, keeping last good schedule",
				"err", ascii(err.Error()), "failures", failures, "retry_in", wait.String())
		} else {
			failures = 0
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (s *Service) backoff(failures int) time.Duration {
	d := s.cfg.RetryMin
	for i := 1; i < failures && d < s.cfg.Interval; i++ {
		d *= 2
	}
	return max(min(d, s.cfg.Interval), MinPollGap)
}

// Poll fetches today and tomorrow (UTC), one request per day, sequentially.
// Each day that succeeds replaces that day's events; a day that fails keeps
// what was there. It returns the first error. Run calls it; it is exported
// for one-shot tools.
func (s *Service) Poll(ctx context.Context) error {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var firstErr error
	accepted := false
	for _, day := range []time.Time{today, today.AddDate(0, 0, 1)} {
		next := day.AddDate(0, 0, 1)
		rctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
		items, err := s.cfg.Fetcher.Fetch(rctx, day, next)
		cancel()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", day.Format(time.DateOnly), err)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		s.merge(day, next, items)
		accepted = true
	}
	s.mu.Lock()
	changed := s.pruneLocked(time.Now())
	if accepted {
		s.updated = time.Now().UTC()
	}
	s.mu.Unlock()
	if changed {
		s.signal()
	}
	if accepted {
		s.saveSnapshot()
	}
	return firstErr
}

// merge replaces the events starting in [from, to) with the fetched ones.
func (s *Service) merge(from, to time.Time, items []Item) {
	events, srcs, skipped := buildEvents(items, s.cat)
	for _, it := range skipped {
		s.logOnce(s.loggedRaw, "start:"+it.Summary, "priyom item has no usable start time, skipped",
			"summary", ascii(it.Summary), "start", ascii(it.Start.DateTime+it.Start.Date))
	}
	for _, ev := range events {
		s.noteOddities(ev)
	}

	s.mu.Lock()
	before := s.fingerprintLocked()
	for id, ev := range s.events {
		if !ev.Start.Before(from) && ev.Start.Before(to) {
			delete(s.events, id)
			delete(s.raw, id)
		}
	}
	for i, ev := range events {
		s.events[ev.ID] = ev
		s.raw[ev.ID] = srcs[i]
	}
	changed := s.fingerprintLocked() != before
	s.mu.Unlock()
	if changed {
		s.signal()
	}
}

func (s *Service) noteOddities(ev Event) {
	if !ev.Parsed {
		s.logOnce(s.loggedRaw, ev.Raw, "unparsed priyom summary, kept as raw text",
			"summary", ascii(ev.Raw), "station", ascii(ev.Station))
		return
	}
	if !ev.ModeKnown {
		s.logOnce(s.loggedMode, strings.ToUpper(ev.PriyomMode), "unrecognised priyom mode, tuning USB",
			"mode", ascii(ev.PriyomMode), "summary", ascii(ev.Raw))
	}
}

// logOnce logs msg the first time key is seen in seen. The sets are bounded
// so a feed full of junk cannot grow them forever.
func (s *Service) logOnce(seen map[string]bool, key, msg string, args ...any) {
	s.logMu.Lock()
	if seen[key] {
		s.logMu.Unlock()
		return
	}
	if len(seen) >= 4096 {
		clear(seen)
	}
	seen[key] = true
	s.logMu.Unlock()
	s.log.Warn(msg, args...)
}

// pruneLocked drops events that ended more than Retention ago.
func (s *Service) pruneLocked(now time.Time) bool {
	cut := now.Add(-s.cfg.Retention)
	changed := false
	for id, ev := range s.events {
		if ev.End().Before(cut) {
			delete(s.events, id)
			delete(s.raw, id)
			changed = true
		}
	}
	return changed
}

func (s *Service) fingerprintLocked() string {
	keys := make([]string, 0, len(s.events))
	for id, ev := range s.events {
		keys = append(keys, id+"\x00"+ev.Raw)
	}
	slices.Sort(keys)
	return strings.Join(keys, "\x01")
}

// signal wakes everyone waiting on the current Changed channel.
func (s *Service) signal() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

// Changed returns a channel that is closed the next time the schedule
// changes. Fetch it again after each wake-up:
//
//	for { ch := svc.Changed(); <-ch; ... }
//
// Any number of goroutines may wait on it.
func (s *Service) Changed() <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.changed
}

// Updated is when Priyom data was last accepted (or the snapshot's time
// after a restart). Zero if never.
func (s *Service) Updated() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.updated
}

// Events returns the events overlapping [from, to), in SortEvents order. A zero from or to leaves that side open.
func (s *Service) Events(from, to time.Time) []Event {
	s.mu.RLock()
	out := make([]Event, 0, len(s.events))
	for _, ev := range s.events {
		if !to.IsZero() && !ev.Start.Before(to) {
			continue
		}
		if !from.IsZero() && !ev.End().After(from) {
			continue
		}
		out = append(out, ev)
	}
	s.mu.RUnlock()
	SortEvents(out)
	return out
}

// SortEvents sorts by start, then station, then first frequency, then ID.
func SortEvents(evs []Event) {
	first := func(e Event) int64 {
		if len(e.Freqs) == 0 {
			return 0
		}
		return e.Freqs[0]
	}
	slices.SortFunc(evs, func(a, b Event) int {
		return cmp.Or(a.Start.Compare(b.Start), cmp.Compare(a.Station, b.Station),
			cmp.Compare(first(a), first(b)), cmp.Compare(a.ID, b.ID))
	})
}

// Get returns one event by ID.
func (s *Service) Get(id string) (Event, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ev, ok := s.events[id]
	return ev, ok
}

// snapshot is the on-disk form. It stores the raw feed items, not the
// derived events, so a restart re-derives them with the current parser and
// catalogue.
type snapshot struct {
	Version int       `json:"version"`
	Updated time.Time `json:"updated"`
	Items   []Item    `json:"items"`
}

func (s *Service) saveSnapshot() {
	if s.cfg.SnapshotPath == "" {
		return
	}
	s.mu.RLock()
	snap := snapshot{Version: 1, Updated: s.updated, Items: make([]Item, 0, len(s.raw))}
	for _, it := range s.raw {
		snap.Items = append(snap.Items, it)
	}
	s.mu.RUnlock()
	slices.SortFunc(snap.Items, func(a, b Item) int {
		return cmp.Or(cmp.Compare(a.Start.DateTime, b.Start.DateTime), cmp.Compare(a.Start.Date, b.Start.Date), cmp.Compare(a.Summary, b.Summary))
	})
	b, err := json.Marshal(snap)
	if err == nil {
		err = writeFileAtomic(s.cfg.SnapshotPath, b)
	}
	if err != nil {
		s.log.Warn("could not save schedule snapshot", "path", ascii(s.cfg.SnapshotPath), "err", ascii(err.Error()))
	}
}

func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".schedule-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp)
		return cmp.Or(werr, cerr)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Service) loadSnapshot() {
	if s.cfg.SnapshotPath == "" {
		return
	}
	b, err := os.ReadFile(s.cfg.SnapshotPath)
	if err != nil {
		if !os.IsNotExist(err) {
			s.log.Warn("could not read schedule snapshot", "path", ascii(s.cfg.SnapshotPath), "err", ascii(err.Error()))
		}
		return
	}
	var snap snapshot
	if err := json.Unmarshal(b, &snap); err != nil || snap.Version != 1 {
		s.log.Warn("ignoring unreadable schedule snapshot", "path", ascii(s.cfg.SnapshotPath))
		return
	}
	events, srcs, _ := buildEvents(snap.Items, s.cat)
	s.mu.Lock()
	for i, ev := range events {
		s.events[ev.ID] = ev
		s.raw[ev.ID] = srcs[i]
	}
	s.updated = snap.Updated
	s.pruneLocked(time.Now())
	n := len(s.events)
	s.mu.Unlock()
	for _, ev := range events {
		s.noteOddities(ev)
	}
	s.log.Info("loaded schedule snapshot", "events", n, "updated", snap.Updated.UTC().Format(time.RFC3339))
}

// ascii makes a string safe for a C-locale log: non-ASCII and control
// characters become Go escapes.
func ascii(v string) string {
	q := strconv.QuoteToASCII(v)
	return q[1 : len(q)-1]
}
