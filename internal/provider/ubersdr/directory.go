package ubersdr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
)

// Defaults for a receiver whose tuning range is not reported
// (instance_reporter.go: "absent or zero means 10 kHz-30 MHz").
const (
	defaultMinHz = 10_000
	defaultMaxHz = 30_000_000
)

// instance is one entry of GET {directory}/api/instances. Pointers mark
// fields whose absence means something different from their zero value.
type instance struct {
	ID               string   `json:"id"`
	Callsign         string   `json:"callsign"`
	Name             string   `json:"name"`
	Location         string   `json:"location"`
	Latitude         *float64 `json:"latitude"`
	Longitude        *float64 `json:"longitude"`
	CountryCode      string   `json:"country_code"`
	PublicURL        string   `json:"public_url"`
	Host             string   `json:"host"`
	Port             int      `json:"port"`
	TLS              bool     `json:"tls"`
	Version          string   `json:"version"`
	LoadStatus       string   `json:"load_status"`
	MaxClients       int      `json:"max_clients"`
	AvailableClients int      `json:"available_clients"`
	MaxSessionTime   int      `json:"max_session_time"`
	CORSEnabled      bool     `json:"cors_enabled"`
	SNR              *float64 `json:"snr_1_8_30_mhz"`
	AntennaConnected *bool    `json:"antenna_connected"`
	IsOnline         *bool    `json:"is_online"`
	TuningRange      *struct {
		Min float64 `json:"min_frequency"`
		Max float64 `json:"max_frequency"`
	} `json:"tuning_range"`
}

type instanceList struct {
	Instances []instance `json:"instances"`
}

// List fetches the directory. Receivers that Allow refuses are still
// listed; only network operations on them are refused.
func (p *Provider) List(ctx context.Context) ([]model.Receiver, error) {
	u := p.dirURL + "/api/instances?online_only=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.ua)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ubersdr directory: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ubersdr directory: HTTP %d", resp.StatusCode)
	}
	var l instanceList
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&l); err != nil {
		return nil, fmt.Errorf("ubersdr directory: %w", err)
	}
	out := make([]model.Receiver, 0, len(l.Instances))
	for _, in := range l.Instances {
		if r, ok := in.receiver(); ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// receiver maps a directory entry. ok is false for entries without an id.
func (in instance) receiver() (model.Receiver, bool) {
	if strings.TrimSpace(in.ID) == "" {
		return model.Receiver{}, false
	}
	r := model.Receiver{
		Provider:         ID,
		ID:               in.ID,
		Callsign:         in.Callsign,
		Name:             in.Name,
		Location:         in.Location,
		Country:          strings.ToLower(in.CountryCode),
		PublicURL:        in.PublicURL,
		BaseURL:          in.baseURL(),
		MinHz:            defaultMinHz,
		MaxHz:            defaultMaxHz,
		Online:           in.IsOnline == nil || *in.IsOnline,
		AntennaConnected: in.AntennaConnected == nil || *in.AntennaConnected,
		LoadStatus:       in.LoadStatus,
		MaxClients:       in.MaxClients,
		AvailableClients: in.AvailableClients,
		MaxSessionTime:   time.Duration(in.MaxSessionTime) * time.Second,
		CORSEnabled:      in.CORSEnabled,
		Version:          in.Version,
	}
	if in.Latitude != nil && in.Longitude != nil && !(*in.Latitude == 0 && *in.Longitude == 0) {
		r.Lat, r.Lon, r.HasPos = *in.Latitude, *in.Longitude, true
	}
	// -1 means the instance has not measured it.
	if in.SNR != nil && *in.SNR >= 0 {
		r.SNR, r.HasSNR = *in.SNR, true
	}
	if tr := in.TuningRange; tr != nil && tr.Max > tr.Min && tr.Max > 0 {
		r.MinHz, r.MaxHz = int64(tr.Min), int64(tr.Max)
	}
	return r, true
}

// baseURL is where we talk to the instance: host/port/tls when reported
// (that is how the instance asks clients to connect), else the origin of
// public_url. Empty when neither gives a usable http(s) origin.
func (in instance) baseURL() string {
	if h := strings.TrimSpace(in.Host); h != "" && !strings.ContainsAny(h, "/?#@ ") {
		scheme, def := "http", 80
		if in.TLS {
			scheme, def = "https", 443
		}
		hostport := h
		if in.Port > 0 && in.Port <= 65535 && in.Port != def {
			hostport = h + ":" + strconv.Itoa(in.Port)
		}
		if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
			// Bare IPv6 literal.
			hostport = "[" + h + "]"
			if in.Port > 0 && in.Port != def {
				hostport += ":" + strconv.Itoa(in.Port)
			}
		}
		return scheme + "://" + hostport
	}
	u, err := url.Parse(strings.TrimSpace(in.PublicURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
