package ubersdr

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
)

func TestListMapsDirectory(t *testing.T) {
	body := readFixture(t, "directory-instances.json")
	var gotUA, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotQuery = r.UserAgent(), r.URL.RawQuery
		if r.URL.Path != "/api/instances" {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()

	p := New(Options{DirectoryURL: srv.URL + "/", UserAgent: "nsl-test", Logger: quietLogger(),
		Allow: func(cs, id, u string) bool { return cs == "M0LTE" }})
	rs, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotUA != "nsl-test" || gotQuery != "online_only=true" {
		t.Fatalf("request UA %q query %q", gotUA, gotQuery)
	}
	if len(rs) != 4 {
		t.Fatalf("%d receivers, want 4 (Allow must not filter the listing)", len(rs))
	}
	by := map[string]model.Receiver{}
	for _, r := range rs {
		by[r.Callsign] = r
	}

	m := by["M0LTE"]
	want := model.Receiver{
		Provider: "ubersdr", ID: "b838fc45-8dd2-4fa8-bb0d-8670244ad5da", Callsign: "M0LTE",
		Name: "SDR with Active Loop", Location: "Reading, England, UK", Country: "gb",
		Lat: 51.460168, Lon: -0.979156, HasPos: true,
		PublicURL: "https://reading-ubersdr.m0lte.uk/", BaseURL: "https://reading-ubersdr.m0lte.uk",
		MinHz: 10000, MaxHz: 30000000, Online: true, AntennaConnected: true, LoadStatus: "ok",
		MaxClients: 20, AvailableClients: 20, MaxSessionTime: time.Hour, Version: "0.1.66",
	}
	if m != want {
		t.Fatalf("M0LTE mapped to\n%+v\nwant\n%+v", m, want)
	}
	if m.Key() != "ubersdr:b838fc45-8dd2-4fa8-bb0d-8670244ad5da" {
		t.Fatalf("key %q", m.Key())
	}
	if m.HasSNR || m.SNR != 0 {
		t.Fatal("snr -1 must mean unknown")
	}
	if r := by["M0EYT"]; r.BaseURL != "http://pjmarsh.co.uk:9080" || !r.HasSNR || r.SNR != 27 {
		t.Fatalf("M0EYT: %+v", r)
	}
	if r := by["K1RA"]; r.BaseURL != "https://ubersdr.k1ra.us:9443" {
		t.Fatalf("K1RA base %q", r.BaseURL)
	}
	if r := by["NA5B"]; r.AntennaConnected || r.BaseURL != "http://na5b.com:8080" {
		t.Fatalf("NA5B: %+v", r)
	}
}

func TestInstanceBaseURLFallbacks(t *testing.T) {
	cases := []struct {
		in   instance
		want string
	}{
		{instance{Host: "a.example", TLS: true, Port: 443}, "https://a.example"},
		{instance{Host: "a.example", Port: 80}, "http://a.example"},
		{instance{Host: "a.example", Port: 8073}, "http://a.example:8073"},
		{instance{Host: "::1", Port: 8080}, "http://[::1]:8080"},
		{instance{PublicURL: "https://b.example:8443/sdr/"}, "https://b.example:8443"},
		{instance{PublicURL: "ftp://c.example/"}, ""},
		{instance{Host: "bad/host", PublicURL: "http://d.example/"}, "http://d.example"},
	}
	for _, c := range cases {
		if got := c.in.baseURL(); got != c.want {
			t.Errorf("%+v: got %q want %q", c.in, got, c.want)
		}
	}
	if _, ok := (instance{}).receiver(); ok {
		t.Error("entry without id mapped")
	}
}

func TestDeepLink(t *testing.T) {
	p := New(Options{Logger: quietLogger()})
	r := model.Receiver{Provider: ID, PublicURL: "https://reading-ubersdr.m0lte.uk/", MinHz: 10000, MaxHz: 30000000}
	if got := p.DeepLink(r, 15388000, model.ModeUSB); got != "https://reading-ubersdr.m0lte.uk/?freq=15388000&mode=usb" {
		t.Fatalf("deep link %q", got)
	}
	for _, f := range []int64{9999, 30000001, 0, -1} {
		if got := p.DeepLink(r, f, model.ModeAM); got != "" {
			t.Errorf("%d Hz: %q, want empty", f, got)
		}
	}
	narrow := r
	narrow.MaxHz = 20000000
	if got := p.DeepLink(narrow, 25000000, model.ModeAM); got != "" {
		t.Errorf("outside receiver range: %q", got)
	}
	if got := p.DeepLink(model.Receiver{Provider: ID}, 7000000, model.ModeUSB); got != "" {
		t.Errorf("no public url: %q", got)
	}
	if got := p.DeepLink(r, 7000000, model.Mode("bogus")); got != "" {
		t.Errorf("bad mode: %q", got)
	}
	if got := p.DeepLink(model.Receiver{PublicURL: "http://x.example/?a=1"}, 7000000, model.ModeCWU); got != "http://x.example/?a=1&freq=7000000&mode=cwu" {
		t.Errorf("existing query: %q", got)
	}
}

// countingTransport fails and counts any attempt to reach the network.
type countingTransport struct{ n atomic.Int64 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errors.New("network used")
}

// The Allow guarantee: refused receivers never touch the network, whatever
// operation is asked for.
func TestAllowRefusesBeforeNetwork(t *testing.T) {
	ct := &countingTransport{}
	var dials atomic.Int64
	tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("dialled")
	}}
	for _, client := range []*http.Client{{Transport: ct}, {Transport: tr}} {
		p := New(Options{UserAgent: "x", HTTPClient: client, Logger: quietLogger(),
			Allow: func(cs, id, u string) bool { return strings.EqualFold(cs, "M0LTE") }})
		r := model.Receiver{Provider: ID, ID: "other", Callsign: "G0XXX", PublicURL: "https://evil.example/",
			BaseURL: "https://evil.example", MinHz: 10000, MaxHz: 30000000}
		ctx := context.Background()
		if _, err := p.Open(ctx, r, provider.OpenRequest{FreqHz: 7000000, Mode: model.ModeUSB}); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Open: %v", err)
		}
		if _, err := p.Probe(ctx, r, 7000000, model.ModeUSB); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Probe: %v", err)
		}
		if _, err := p.Spectrogram(ctx, r, 7000000, 20000, 30); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Spectrogram: %v", err)
		}
		// A receiver claiming another provider, or with no usable URL, is
		// also refused before any network use.
		other := r
		other.Callsign, other.Provider = "M0LTE", "kiwisdr"
		if _, err := p.Open(ctx, other, provider.OpenRequest{FreqHz: 7000000, Mode: model.ModeUSB}); err == nil {
			t.Error("foreign provider accepted")
		}
		bad := r
		bad.Callsign, bad.BaseURL = "M0LTE", "file:///etc/passwd"
		if _, err := p.Probe(ctx, bad, 7000000, model.ModeUSB); err == nil {
			t.Error("bad base URL accepted")
		}
	}
	if ct.n.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("network touched: %d requests, %d dials", ct.n.Load(), dials.Load())
	}
}

func TestConnectionRejectionMapping(t *testing.T) {
	cases := []struct {
		status   int
		body     string
		rejected bool
		retry    time.Duration
		reason   string
	}{
		{200, `{"allowed":true,"max_session_time":3600}`, false, 0, ""},
		{200, `{"allowed":false,"reason":"nope"}`, true, 0, "nope"},
		{403, `{"allowed":false,"reason":"Your IP address has been banned"}`, true, 0, "banned"},
		{403, `<!DOCTYPE html><html>Access Denied</html>`, true, 0, "HTTP 403"},
		{403, `{"allowed":false,"reason":"This receiver requires a password to access"}`, true, 0, "password"},
		{410, `{"allowed":false,"reason":"Your session has been terminated. Please refresh the page."}`, true, 0, "terminated"},
		{503, `{"allowed":false,"reason":"Maximum unique users per IP reached (2)"}`, true, 0, "per IP"},
		{429, `{"allowed":false,"reason":"Rate limit exceeded. Please wait before trying again."}`, true, 10 * time.Second, "Rate limit"},
		{429, `{"allowed":false,"reason":"Daily time limit reached (60 minutes per 24 hours)."}`, true, time.Hour, "Daily"},
		{400, `{"allowed":false,"reason":"Invalid or missing user_session_id"}`, false, 0, "user_session_id"},
	}
	for _, c := range cases {
		var gotUA, gotID string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotUA = r.UserAgent()
			var b struct {
				ID string `json:"user_session_id"`
			}
			jsonDecode(r, &b)
			gotID = b.ID
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		}))
		p := New(Options{UserAgent: "nsl-test", Logger: quietLogger()})
		base, _ := url.Parse(srv.URL)
		_, err := p.register(context.Background(), http.DefaultClient, base, newUUID())
		srv.Close()
		if gotUA != "nsl-test" || !uuidRE.MatchString(gotID) {
			t.Fatalf("status %d: UA %q id %q", c.status, gotUA, gotID)
		}
		var rej *provider.RejectedError
		switch {
		case c.status == 200 && !c.rejected:
			if err != nil {
				t.Errorf("200 allowed: %v", err)
			}
		case c.rejected:
			if !errors.As(err, &rej) || rej.RetryAfter != c.retry || !strings.Contains(rej.Reason, c.reason) {
				t.Errorf("status %d: got %v (%+v)", c.status, err, rej)
			}
		default:
			if err == nil || errors.As(err, &rej) || !strings.Contains(err.Error(), c.reason) {
				t.Errorf("status %d: want plain error, got %v", c.status, err)
			}
		}
	}
}

// Regression (seen live): a session transport cloned from a transport that
// already offers h2 must still negotiate HTTP/1.1, or the WebSocket upgrade
// and even /connection read HTTP/2 frames as HTTP/1.
func TestSessionClientForcesHTTP1(t *testing.T) {
	base := &http.Transport{TLSClientConfig: &tls.Config{NextProtos: []string{"h2", "http/1.1"}}}
	p := New(Options{HTTPClient: &http.Client{Transport: base}, Logger: quietLogger()})
	c, tr := p.sessionClient()
	got := c.Transport.(*http.Transport)
	if tr == nil || got == base {
		t.Fatal("expected a tracked clone")
	}
	if len(got.TLSClientConfig.NextProtos) != 1 || got.TLSClientConfig.NextProtos[0] != "http/1.1" {
		t.Fatalf("NextProtos %v", got.TLSClientConfig.NextProtos)
	}
	if len(base.TLSClientConfig.NextProtos) != 2 {
		t.Fatal("the shared transport was modified")
	}
	if _, tr := New(Options{HTTPClient: &http.Client{Transport: &countingTransport{}}}).sessionClient(); tr != nil {
		t.Fatal("custom round tripper cannot be tracked")
	}
}
