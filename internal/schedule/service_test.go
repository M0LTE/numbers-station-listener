package schedule

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// fakeFetcher serves the fixture rebased onto whichever day is asked for,
// and records every call with the (bubble) time it was made.
type fakeFetcher struct {
	mu    sync.Mutex
	items []Item
	calls []fetchCall
	fail  func(min time.Time) bool
}

type fetchCall struct{ at, min, max time.Time }

func (f *fakeFetcher) Fetch(ctx context.Context, min, max time.Time) ([]Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fetchCall{time.Now(), min, max})
	if f.fail != nil && f.fail(min) {
		return nil, errors.New("boom")
	}
	out := make([]Item, 0, len(f.items))
	for _, it := range f.items {
		st, ok := it.Start.Time()
		if !ok {
			out = append(out, it)
			continue
		}
		tod := st.Sub(st.Truncate(24 * time.Hour))
		it.Start = ItemTime{DateTime: min.Add(tod).Format("2006-01-02T15:04:05.000Z")}
		out = append(out, it)
	}
	return out, nil
}

func (f *fakeFetcher) setFail(fn func(time.Time) bool) {
	f.mu.Lock()
	f.fail = fn
	f.mu.Unlock()
}

func (f *fakeFetcher) setItems(items []Item) {
	f.mu.Lock()
	f.items = items
	f.mu.Unlock()
}

func (f *fakeFetcher) snapshot() []fetchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fetchCall(nil), f.calls...)
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func startService(t *testing.T, cfg Config, ff *fakeFetcher) *Service {
	t.Helper()
	cfg.Fetcher = ff
	svc := New(cfg, loadCatalog(t), discard())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return svc
}

func dayOf(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

func TestPollsTodayAndTomorrowEvery15Minutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := &fakeFetcher{items: loadFixture(t)}
		start := time.Now()
		svc := startService(t, Config{}, ff)
		synctest.Wait()

		calls := ff.snapshot()
		if len(calls) != 2 {
			t.Fatalf("calls after start = %d, want 2", len(calls))
		}
		today := dayOf(start)
		for i, c := range calls {
			wantMin := today.AddDate(0, 0, i)
			if !c.at.Equal(start) || !c.min.Equal(wantMin) || !c.max.Equal(wantMin.AddDate(0, 0, 1)) {
				t.Errorf("call %d = %+v, want at %v window %v..+1d", i, c, start, wantMin)
			}
		}
		if n := len(svc.Events(time.Time{}, time.Time{})); n != 256 {
			t.Errorf("events = %d, want 256 (two days of 128)", n)
		}
		if !svc.Updated().Equal(start) {
			t.Errorf("Updated = %v", svc.Updated())
		}

		time.Sleep(15*time.Minute - time.Second)
		synctest.Wait()
		if n := len(ff.snapshot()); n != 2 {
			t.Fatalf("polled early: %d calls", n)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		calls = ff.snapshot()
		if len(calls) != 4 || !calls[2].at.Equal(start.Add(15*time.Minute)) {
			t.Fatalf("second poll: %+v", calls)
		}

		time.Sleep(45 * time.Minute)
		synctest.Wait()
		calls = ff.snapshot()
		if len(calls) != 10 {
			t.Fatalf("after 1 h: %d calls, want 10", len(calls))
		}
		for i := 0; i < len(calls); i += 2 {
			want := start.Add(time.Duration(i/2) * 15 * time.Minute)
			if !calls[i].at.Equal(want) || !calls[i+1].at.Equal(want) {
				t.Errorf("poll %d at %v/%v, want %v", i/2, calls[i].at, calls[i+1].at, want)
			}
		}
	})
}

func TestBackoffKeepsLastGoodData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := &fakeFetcher{items: loadFixture(t)}
		start := time.Now()
		svc := startService(t, Config{}, ff)
		synctest.Wait()
		good := svc.Events(time.Time{}, time.Time{})
		if len(good) == 0 {
			t.Fatal("no data")
		}

		ff.setFail(func(time.Time) bool { return true })
		time.Sleep(15 * time.Minute) // first failing poll
		// Retries at +2, +4, +8, then capped at the 15 min interval.
		wantPolls := []time.Duration{0, 15, 17, 21, 29, 44, 59}
		time.Sleep(44 * time.Minute)
		synctest.Wait()
		ff.setFail(nil)
		time.Sleep(15 * time.Minute) // success at 74, then normal interval
		synctest.Wait()
		wantPolls = append(wantPolls, 74)
		time.Sleep(15 * time.Minute)
		synctest.Wait()
		wantPolls = append(wantPolls, 89)

		calls := ff.snapshot()
		var polls []time.Duration
		for i := 0; i < len(calls); i += 2 {
			polls = append(polls, calls[i].at.Sub(start)/time.Minute)
		}
		if len(polls) != len(wantPolls) {
			t.Fatalf("polls at minutes %v, want %v", polls, wantPolls)
		}
		for i := range polls {
			if polls[i] != wantPolls[i] {
				t.Fatalf("polls at minutes %v, want %v", polls, wantPolls)
			}
		}
		if got := svc.Events(time.Time{}, time.Time{}); len(got) != len(good) {
			t.Errorf("events after recovery = %d, want %d", len(got), len(good))
		}
		if !svc.Updated().Equal(start.Add(89 * time.Minute)) {
			t.Errorf("Updated = %v", svc.Updated().Sub(start))
		}
	})
}

func TestDataSurvivesFailuresAndPartialFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		items := loadFixture(t)
		ff := &fakeFetcher{items: items}
		start := time.Now()
		svc := startService(t, Config{}, ff)
		synctest.Wait()
		before := svc.Events(time.Time{}, time.Time{})

		// Everything fails: nothing changes, Updated stays.
		ff.setFail(func(time.Time) bool { return true })
		time.Sleep(15 * time.Minute)
		synctest.Wait()
		if got := svc.Events(time.Time{}, time.Time{}); len(got) != len(before) {
			t.Fatalf("lost data on failure: %d -> %d", len(before), len(got))
		}
		if !svc.Updated().Equal(start) {
			t.Errorf("Updated moved on a failed poll")
		}

		// Today now has one item fewer; tomorrow keeps failing and keeps its data.
		tomorrow := dayOf(start).AddDate(0, 0, 1)
		ff.setItems(items[1:])
		ff.setFail(func(min time.Time) bool { return min.Equal(tomorrow) })
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		todayN, tomorrowN := 0, 0
		for _, ev := range svc.Events(time.Time{}, time.Time{}) {
			if ev.Start.Before(tomorrow) {
				todayN++
			} else {
				tomorrowN++
			}
		}
		if todayN != 127 || tomorrowN != 128 {
			t.Errorf("today %d tomorrow %d, want 127 and 128", todayN, tomorrowN)
		}
	})
}

func TestEtiquetteFloor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := &fakeFetcher{items: loadFixture(t)}
		ff.setFail(func(time.Time) bool { return true })
		start := time.Now()
		startService(t, Config{Interval: time.Second, RetryMin: time.Millisecond}, ff)
		time.Sleep(time.Hour)
		synctest.Wait()
		calls := ff.snapshot()
		for i := 2; i < len(calls); i += 2 {
			if gap := calls[i].at.Sub(calls[i-2].at); gap < MinPollGap {
				t.Fatalf("polls %v apart", gap)
			}
		}
		if n := len(calls) / 2; n != 31 { // t=0, then every 2 min for an hour
			t.Errorf("polls in an hour = %d, want 31 (first at %v)", n, calls[0].at.Sub(start))
		}
	})
}

func TestChangedSignalsOnlyOnChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := &fakeFetcher{items: loadFixture(t)}
		ff.setFail(func(time.Time) bool { return true })
		cfg := Config{Fetcher: ff}
		svc := New(cfg, loadCatalog(t), discard())
		ch := svc.Changed()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		svc.Poll(ctx)
		if isClosed(ch) {
			t.Fatal("signalled with no data")
		}
		ff.setFail(nil)
		svc.Poll(ctx)
		if !isClosed(ch) {
			t.Fatal("not signalled after first data")
		}
		ch2 := svc.Changed()
		svc.Poll(ctx)
		if isClosed(ch2) {
			t.Fatal("signalled for identical data")
		}
		ff.setItems(loadFixture(t)[3:])
		svc.Poll(ctx)
		if !isClosed(ch2) {
			t.Fatal("not signalled after items were removed")
		}
	})
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestRetentionDropsOldEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := &fakeFetcher{items: loadFixture(t)}
		start := time.Now()
		svc := startService(t, Config{}, ff)
		time.Sleep(72 * time.Hour)
		synctest.Wait()
		now := time.Now()
		all := svc.Events(time.Time{}, time.Time{})
		if len(all) == 0 {
			t.Fatal("no events")
		}
		for _, ev := range all {
			if ev.End().Before(now.Add(-24 * time.Hour)) {
				t.Fatalf("kept %q ending %v, now %v", ev.Raw, ev.End(), now)
			}
		}
		if first := all[0].Start; first.Before(dayOf(start).AddDate(0, 0, 2)) {
			t.Errorf("oldest event starts %v", first)
		}
	})
}

func TestEventsWindowAndGet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := &fakeFetcher{items: loadFixture(t)}
		svc := New(Config{Fetcher: ff}, loadCatalog(t), discard())
		svc.Poll(context.Background())
		day := dayOf(time.Now())
		// V13 at 00:00 runs 55 min, so a window from 00:30 still includes it.
		evs := svc.Events(day.Add(30*time.Minute), day.Add(31*time.Minute))
		var raws []string
		for _, ev := range evs {
			raws = append(raws, ev.Raw)
		}
		want := []string{
			"V13 15388kHz USB/AM [Target: East Asia]",
			"V13 15890kHz USB/AM [Target: East Asia]",
			"V13 18040kHz USB/AM [Target: East Asia]",
			"F06 Search [Last used: 11405kHz]",
		}
		if strings.Join(raws, "\n") != strings.Join(want, "\n") {
			t.Errorf("window events\n%s\nwant\n%s", strings.Join(raws, "\n"), strings.Join(want, "\n"))
		}
		for i := 1; i < len(evs); i++ {
			a, b := evs[i-1], evs[i]
			if a.Start.After(b.Start) || (a.Start.Equal(b.Start) && a.Station > b.Station) {
				t.Errorf("not sorted at %d", i)
			}
		}
		got, ok := svc.Get(evs[0].ID)
		if !ok || got.Raw != evs[0].Raw {
			t.Errorf("Get = %+v %v", got, ok)
		}
		if _, ok := svc.Get("nope"); ok {
			t.Error("Get found a missing id")
		}
	})
}

func TestSnapshotRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sub", "schedule.json")
		ff := &fakeFetcher{items: loadFixture(t)}
		svc := New(Config{Fetcher: ff, SnapshotPath: path}, loadCatalog(t), discard())
		if err := svc.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := svc.Events(time.Time{}, time.Time{})

		time.Sleep(time.Minute)
		down := &fakeFetcher{fail: func(time.Time) bool { return true }}
		svc2 := New(Config{Fetcher: down, SnapshotPath: path}, loadCatalog(t), discard())
		got := svc2.Events(time.Time{}, time.Time{})
		if len(got) != len(want) || len(got) != 256 {
			t.Fatalf("restored %d events, want %d", len(got), len(want))
		}
		for i := range got {
			if got[i].ID != want[i].ID || got[i].Raw != want[i].Raw || !got[i].Start.Equal(want[i].Start) {
				t.Fatalf("event %d differs: %+v vs %+v", i, got[i], want[i])
			}
		}
		if !svc2.Updated().Equal(svc.Updated()) {
			t.Errorf("Updated %v, want %v", svc2.Updated(), svc.Updated())
		}
		if len(down.snapshot()) != 0 {
			t.Error("New fetched from Priyom")
		}

		// No snapshot file: empty, no complaint.
		svc3 := New(Config{Fetcher: down, SnapshotPath: filepath.Join(t.TempDir(), "missing.json")}, nil, discard())
		if len(svc3.Events(time.Time{}, time.Time{})) != 0 || !svc3.Updated().IsZero() {
			t.Error("phantom data")
		}
	})
}

func TestLogsOddItemsOnceInASCII(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		at := ItemTime{DateTime: "2026-10-06T10:00:00.000Z"}
		ff := &fakeFetcher{items: []Item{
			{"V99 7000kHz ZORK", at},
			{"V98 7100kHz zork", at}, // same mode, different case
			{"V97 7200kHz Цифры", at},
			{"what is this", at},
			{"V13 15388kHz USB", ItemTime{DateTime: "never"}},
		}}
		svc := New(Config{Fetcher: ff}, nil, log)
		svc.Poll(context.Background())
		svc.Poll(context.Background())
		out := buf.String()
		for _, c := range []struct {
			substr string
			n      int
		}{
			{"unrecognised priyom mode", 2},
			{"mode=ZORK", 1},
			{"unparsed priyom summary", 1},
			{"no usable start time", 1},
		} {
			if got := strings.Count(out, c.substr); got != c.n {
				t.Errorf("%q logged %d times, want %d\n%s", c.substr, got, c.n, out)
			}
		}
		for _, r := range out {
			if r > 0x7e || (r < 0x20 && r != '\n') {
				t.Fatalf("non-ASCII in log: %q", out)
			}
		}
		if !strings.Contains(out, "\\u0426") {
			t.Errorf("Cyrillic mode not escaped: %s", out)
		}
		// The odd items are all kept.
		if n := len(svc.Events(time.Time{}, time.Time{})); n != 8 {
			t.Errorf("events = %d, want 8 (4 a day)", n)
		}
	})
}
