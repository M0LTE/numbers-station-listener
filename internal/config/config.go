// Package config reads all settings from environment variables.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the whole runtime configuration. See docs/deploy.md for the
// variable list.
type Config struct {
	Listen string // NSL_LISTEN, default ":8080"
	// RedirectListen, if set (e.g. ":80"), serves a plain redirect to the
	// same host on Listen's port, so http://host/ works on the LAN.
	RedirectListen string // NSL_REDIRECT_LISTEN
	PublicURL      string // NSL_PUBLIC_URL, the site's own URL (for User-Agent and links)
	Contact        string // NSL_CONTACT, how operators reach us (User-Agent)
	DataDir        string // NSL_DATA_DIR, snapshots; empty disables persistence

	StationsFile string // NSL_STATIONS_FILE, override the embedded catalogue

	PriyomURL  string        // NSL_PRIYOM_URL
	PriyomPoll time.Duration // NSL_PRIYOM_POLL, default 15m, floor 5m

	UberSDRDirectory string        // NSL_UBERSDR_DIRECTORY
	DirectoryPoll    time.Duration // NSL_DIRECTORY_POLL, default 5m, floor 1m
	// ReceiverAllow restricts which receivers we ever connect to or probe
	// (callsigns, ids or hostnames, case-insensitive). Empty = all. Used in
	// development to stay on M0LTE only.
	ReceiverAllow []string // NSL_RECEIVER_ALLOW

	Grace          time.Duration // NSL_GRACE, default 10s, capped at 30s by the relay
	PerReceiverCap int           // NSL_PER_RECEIVER_CAP, default 2
	SpanHz         int           // NSL_SPAN_HZ, waterfall span, default 12000

	ProbeEnabled bool          // NSL_PROBE, default true
	ProbeTopK    int           // NSL_PROBE_TOP_K, default 5
	ProbeEvery   time.Duration // NSL_PROBE_EVERY, default 60s, floor 30s

	RankWeights string // NSL_RANK_WEIGHTS, JSON, optional

	// PSKReporter turns on the PSKReporter MQTT path-open hint. It also
	// sets the hint's ranking weight to 0.1 unless NSL_RANK_WEIGHTS says
	// otherwise.
	PSKReporter bool // NSL_PSKR, default true

	// AdminNets may reach /admin and /metrics. Requests that arrived through
	// Cloudflare (Cf-Connecting-Ip set) never may.
	AdminNets []netip.Prefix // NSL_ADMIN_NETS, default loopback + RFC 1918
}

// RepoURL is where the source lives (AGPL section 13).
const RepoURL = "https://github.com/M0LTE/numbers-station-listener"

// Version is set at build time with -ldflags "-X ...config.Version=...".
var Version = "dev"

// UserAgent is sent on every upstream request.
func (c Config) UserAgent() string {
	ua := "numbers-station-listener/" + Version + " (+" + RepoURL
	if c.PublicURL != "" {
		ua += "; site " + c.PublicURL
	}
	if c.Contact != "" {
		ua += "; contact " + c.Contact
	}
	return ua + ")"
}

// FromEnv reads the configuration.
func FromEnv() (Config, error) {
	return parse(os.Getenv)
}

func parse(get func(string) string) (Config, error) {
	c := Config{
		Listen:           str(get, "NSL_LISTEN", ":8080"),
		RedirectListen:   get("NSL_REDIRECT_LISTEN"),
		PublicURL:        get("NSL_PUBLIC_URL"),
		Contact:          str(get, "NSL_CONTACT", "M0LTE"),
		DataDir:          get("NSL_DATA_DIR"),
		StationsFile:     get("NSL_STATIONS_FILE"),
		PriyomURL:        strings.TrimRight(str(get, "NSL_PRIYOM_URL", "https://calendar.priyom.org"), "/"),
		UberSDRDirectory: strings.TrimRight(str(get, "NSL_UBERSDR_DIRECTORY", "https://instances.ubersdr.org"), "/"),
		RankWeights:      get("NSL_RANK_WEIGHTS"),
	}
	var err error
	errs := func(e error) {
		if e != nil && err == nil {
			err = e
		}
	}
	var e error
	c.PriyomPoll, e = dur(get, "NSL_PRIYOM_POLL", 15*time.Minute, 5*time.Minute)
	errs(e)
	c.DirectoryPoll, e = dur(get, "NSL_DIRECTORY_POLL", 5*time.Minute, time.Minute)
	errs(e)
	c.Grace, e = dur(get, "NSL_GRACE", 10*time.Second, time.Second)
	errs(e)
	c.ProbeEvery, e = dur(get, "NSL_PROBE_EVERY", 60*time.Second, 30*time.Second)
	errs(e)
	c.PerReceiverCap, e = integer(get, "NSL_PER_RECEIVER_CAP", 2)
	errs(e)
	c.SpanHz, e = integer(get, "NSL_SPAN_HZ", 12000)
	errs(e)
	c.ProbeTopK, e = integer(get, "NSL_PROBE_TOP_K", 5)
	errs(e)
	c.ProbeEnabled, e = boolean(get, "NSL_PROBE", true)
	errs(e)
	c.PSKReporter, e = boolean(get, "NSL_PSKR", true)
	errs(e)
	for _, s := range strings.Split(get("NSL_RECEIVER_ALLOW"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.ReceiverAllow = append(c.ReceiverAllow, strings.ToLower(s))
		}
	}
	nets := str(get, "NSL_ADMIN_NETS", "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7")
	for _, s := range strings.Split(nets, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		p, perr := netip.ParsePrefix(s)
		if perr != nil {
			errs(fmt.Errorf("NSL_ADMIN_NETS: %w", perr))
			continue
		}
		c.AdminNets = append(c.AdminNets, p)
	}
	return c, err
}

// Allowed reports whether ReceiverAllow permits a receiver.
func (c Config) Allowed(callsign, id, publicURL string) bool {
	if len(c.ReceiverAllow) == 0 {
		return true
	}
	cs, i, u := strings.ToLower(callsign), strings.ToLower(id), strings.ToLower(publicURL)
	for _, a := range c.ReceiverAllow {
		if a == cs || a == i || (u != "" && strings.Contains(u, a)) {
			return true
		}
	}
	return false
}

func str(get func(string) string, k, def string) string {
	if v := strings.TrimSpace(get(k)); v != "" {
		return v
	}
	return def
}

func dur(get func(string) string, k string, def, floor time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(get(k))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", k, err)
	}
	if d < floor {
		d = floor
	}
	return d, nil
}

func integer(get func(string) string, k string, def int) (int, error) {
	v := strings.TrimSpace(get(k))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}

func boolean(get func(string) string, k string, def bool) (bool, error) {
	v := strings.TrimSpace(get(k))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", k, err)
	}
	return b, nil
}
