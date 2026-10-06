package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is Priyom's calendar feed.
const DefaultBaseURL = "https://calendar.priyom.org"

// Fetcher gets the feed items for one window. The Service only ever asks
// for whole UTC days. It is an interface so the poll loop can be tested in a
// synctest bubble without real sockets.
type Fetcher interface {
	Fetch(ctx context.Context, timeMin, timeMax time.Time) ([]Item, error)
}

// HTTPFetcher fetches from Priyom's undocumented JSON feed, the way
// Priyom's own calendar viewer does.
type HTTPFetcher struct {
	BaseURL   string // default DefaultBaseURL
	UserAgent string
	Client    *http.Client // default http.DefaultClient
}

// maxFeedBytes bounds a response; a real day is a few tens of KB.
const maxFeedBytes = 8 << 20

// feedTime is the timestamp format of the viewer's timeMin/timeMax.
const feedTime = "2006-01-02T15:04:05.000Z"

// Fetch implements Fetcher.
func (f *HTTPFetcher) Fetch(ctx context.Context, timeMin, timeMax time.Time) ([]Item, error) {
	base := f.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	q := url.Values{}
	q.Set("timeMin", timeMin.UTC().Format(feedTime))
	q.Set("timeMax", timeMax.UTC().Format(feedTime))
	u := strings.TrimRight(base, "/") + "/events?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if f.UserAgent != "" {
		req.Header.Set("User-Agent", f.UserAgent)
	}
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("priyom: %s", resp.Status)
	}
	var body struct {
		Items *[]Item `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxFeedBytes)).Decode(&body); err != nil {
		return nil, fmt.Errorf("priyom: decode: %w", err)
	}
	if body.Items == nil {
		return nil, fmt.Errorf("priyom: response has no items array")
	}
	return *body.Items, nil
}
