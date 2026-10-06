package schedule

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// The real HTTP fetcher against httptest, outside any synctest bubble.
func TestHTTPFetcher(t *testing.T) {
	fixture, err := os.ReadFile(repoFile(t, "tests/fixtures/priyom-2026-10-06.json"))
	if err != nil {
		t.Fatal(err)
	}
	type req struct{ path, min, max, ua string }
	var got []req
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		got = append(got, req{r.URL.Path, q.Get("timeMin"), q.Get("timeMax"), r.UserAgent()})
		w.Header().Set("Content-Type", "application/json")
		w.Write(fixture)
	}))
	defer srv.Close()

	f := &HTTPFetcher{BaseURL: srv.URL + "/", UserAgent: "nsl-test (+https://example.org; ops@example.org)", Client: srv.Client()}
	day := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	items, err := f.Fetch(context.Background(), day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 128 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].Summary != "V13 15388kHz USB/AM [Target: East Asia]" || items[0].Start.DateTime != "2026-10-06T00:00:00.000Z" {
		t.Errorf("first item %+v", items[0])
	}
	want := req{"/events", "2026-10-06T00:00:00.000Z", "2026-10-07T00:00:00.000Z", "nsl-test (+https://example.org; ops@example.org)"}
	if len(got) != 1 || got[0] != want {
		t.Errorf("requests %+v, want [%+v]", got, want)
	}
}

func TestHTTPFetcherErrors(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"status":   func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusServiceUnavailable) },
		"bad json": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) },
		"no items": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"error":"x"}`)) },
	}
	day := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	for name, h := range cases {
		srv := httptest.NewServer(h)
		f := &HTTPFetcher{BaseURL: srv.URL, Client: srv.Client()}
		if _, err := f.Fetch(context.Background(), day, day.AddDate(0, 0, 1)); err == nil {
			t.Errorf("%s: no error", name)
		}
		srv.Close()
	}
}
