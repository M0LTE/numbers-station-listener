package api

import (
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/probe"
	"github.com/m0lte/numbers-station-listener/internal/rank"
	"github.com/m0lte/numbers-station-listener/internal/schedule"
	"github.com/m0lte/numbers-station-listener/internal/stations"
)

// ReceiverView is the receiver summary of docs/api.md.
type ReceiverView struct {
	Key              string   `json:"key"`
	Provider         string   `json:"provider"`
	Callsign         string   `json:"callsign"`
	Name             string   `json:"name"`
	Location         string   `json:"location"`
	Country          string   `json:"country"`
	Lat              *float64 `json:"lat"`
	Lon              *float64 `json:"lon"`
	PublicURL        string   `json:"publicUrl"`
	DistanceKm       *float64 `json:"distanceKm,omitempty"`
	Score            *float64 `json:"score,omitempty"`
	Reasons          []string `json:"reasons,omitempty"`
	AvailableClients int      `json:"availableClients"`
	MaxClients       int      `json:"maxClients"`
	DeepLink         string   `json:"deepLink,omitempty"`
}

// FreqView is one frequency of an event.
type FreqView struct {
	Hz        int64          `json:"hz"`
	Signal    probe.Signal   `json:"signal"`
	Receivers []ReceiverView `json:"receivers"`
}

// EventView is the Event of docs/api.md.
type EventView struct {
	ID           string     `json:"id"`
	Station      string     `json:"station"`
	StationName  string     `json:"stationName"`
	PriyomURL    *string    `json:"priyomUrl"`
	Language     string     `json:"language"`
	Category     string     `json:"category"`
	Start        time.Time  `json:"start"`
	End          time.Time  `json:"end"`
	EndEstimated bool       `json:"endEstimated"`
	Status       string     `json:"status"`
	Search       bool       `json:"search"`
	Freqs        []FreqView `json:"freqs"`
	PriyomMode   string     `json:"priyomMode"`
	Mode         model.Mode `json:"mode"`
	Digital      bool       `json:"digital"`
	Remarks      []string   `json:"remarks"`
	Target       *string    `json:"target"`
	Raw          string     `json:"raw"`
	Parsed       bool       `json:"parsed"`
}

// NowView is GET /api/now.
type NowView struct {
	ServerTime      time.Time   `json:"serverTime"`
	ScheduleUpdated *time.Time  `json:"scheduleUpdated"`
	Now             []EventView `json:"now"`
	Next            []EventView `json:"next"`
	Later           []EventView `json:"later"`
}

const (
	nextCount     = 8
	laterHorizon  = 24 * time.Hour
	receiversShow = 3
)

func (s *Server) receiverView(r model.Receiver, c *rank.Candidate, hz int64, mode model.Mode, digital bool) ReceiverView {
	v := ReceiverView{
		Key: r.Key(), Provider: r.Provider, Callsign: r.Callsign, Name: r.Name,
		Location: r.Location, Country: r.Country, PublicURL: r.PublicURL,
		AvailableClients: r.AvailableClients, MaxClients: r.MaxClients,
	}
	if r.HasPos {
		lat, lon := r.Lat, r.Lon
		v.Lat, v.Lon = &lat, &lon
	}
	if c != nil {
		sc := c.Score
		v.Score = &sc
		v.Reasons = c.Reasons
		v.DistanceKm = c.DistanceKm
	}
	if hz > 0 {
		if p, ok := s.providers[r.Provider]; ok {
			v.DeepLink = p.DeepLink(r, TuneHz(hz, mode, digital), mode)
		}
	}
	return v
}

// candidates ranks the allowed receivers for one event frequency.
func (s *Server) candidates(ev schedule.Event, st *stations.Station, hz int64, at time.Time, withProbes bool) []rank.Candidate {
	in := rank.Input{
		Receivers: s.allowedReceivers(),
		FreqHz:    hz,
		At:        at,
		Held:      s.relay.HeldOn,
		Weights:   s.weights,
	}
	if st != nil {
		in.Tx = st.TxSite
		in.Alternates = st.Alternates
		if s.pathOpen != nil && st.TxSite.Known() {
			tx := *st.TxSite
			in.PathOpen = func(r model.Receiver) (float64, string, bool) {
				return s.pathOpen(tx, r, hz, at)
			}
		}
	}
	if withProbes && s.prober != nil {
		in.Probes = s.prober.Results(hz)
	}
	return rank.Rank(in)
}

func (s *Server) allowedReceivers() []model.Receiver {
	all := s.dir.Receivers()
	out := all[:0:0]
	for _, r := range all {
		if s.cfg.Allowed(r.Callsign, r.ID, r.PublicURL) {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) eventView(ev schedule.Event, now time.Time) EventView {
	st, _ := s.catalog.Lookup(ev.Station)
	v := EventView{
		ID: ev.ID, Station: ev.Station, Start: ev.Start,
		EndEstimated: ev.EndEstimated, Search: ev.Search,
		PriyomMode: ev.PriyomMode, Mode: ev.Mode, Digital: ev.Digital,
		Remarks: ev.Remarks, Raw: ev.Raw, Parsed: ev.Parsed,
		Freqs: []FreqView{},
	}
	if v.Remarks == nil {
		v.Remarks = []string{}
	}
	if ev.Target != "" {
		t := ev.Target
		v.Target = &t
	}
	if st != nil {
		v.StationName, v.Language, v.Category = st.Name, st.Language, st.Category
		if st.PriyomURL != "" {
			u := st.PriyomURL
			v.PriyomURL = &u
		}
	}
	var endedAt *time.Time
	if s.prober != nil {
		if t, ok := s.prober.EndedAt(ev.ID); ok {
			endedAt = &t
			v.EndEstimated = false
		}
	}
	v.End = schedule.EffectiveEnd(ev, endedAt)
	v.Status = string(schedule.StatusAt(ev, now, endedAt))

	if ev.Parsed && !ev.Search && v.Status != string(schedule.StatusDone) {
		for _, hz := range ev.Freqs {
			fv := FreqView{Hz: hz, Signal: probe.Signal{State: probe.Unknown}, Receivers: []ReceiverView{}}
			if s.prober != nil {
				fv.Signal = s.prober.Signal(ev.ID, hz)
			}
			// Rank for the middle of the transmission, or now if it is on.
			at := ev.Start
			if now.After(at) {
				at = now
			}
			cands := s.candidates(ev, st, hz, at, true)
			for i := range cands {
				if i >= receiversShow {
					break
				}
				fv.Receivers = append(fv.Receivers, s.receiverView(cands[i].Receiver, &cands[i], hz, ev.Mode, ev.Digital))
			}
			v.Freqs = append(v.Freqs, fv)
		}
	} else {
		for _, hz := range ev.Freqs {
			v.Freqs = append(v.Freqs, FreqView{Hz: hz, Signal: probe.Signal{State: probe.Unknown}, Receivers: []ReceiverView{}})
		}
	}
	return v
}

// buildNow assembles GET /api/now.
func (s *Server) buildNow(now time.Time) NowView {
	out := NowView{ServerTime: now, Now: []EventView{}, Next: []EventView{}, Later: []EventView{}}
	if u := s.sched.Updated(); !u.IsZero() {
		out.ScheduleUpdated = &u
	}
	evs := s.sched.Events(now.Add(-12*time.Hour), now.Add(laterHorizon))
	for _, ev := range evs {
		switch {
		case !ev.Start.After(now):
			v := s.eventView(ev, now)
			if v.Status == string(schedule.StatusLive) {
				out.Now = append(out.Now, v)
			}
		case len(out.Next) < nextCount:
			out.Next = append(out.Next, s.eventView(ev, now))
		default:
			out.Later = append(out.Later, s.eventView(ev, now))
		}
	}
	// soonest-ending first
	for i := 1; i < len(out.Now); i++ {
		for j := i; j > 0 && out.Now[j].End.Before(out.Now[j-1].End); j-- {
			out.Now[j], out.Now[j-1] = out.Now[j-1], out.Now[j]
		}
	}
	return out
}

// ProbeTargets is what the prober checks this round: live transmissions and
// those starting within the next minute, on their top candidates (ranked
// without probe results, so probing cannot feed back into its own choice).
func (s *Server) ProbeTargets(now time.Time) []probe.Target {
	var out []probe.Target
	for _, ev := range s.sched.Events(now.Add(-6*time.Hour), now.Add(time.Minute)) {
		if !ev.Parsed || ev.Search {
			continue
		}
		var endedAt *time.Time
		if t, ok := s.prober.EndedAt(ev.ID); ok {
			endedAt = &t
		}
		status := schedule.StatusAt(ev, now, endedAt)
		upcomingSoon := status == schedule.StatusUpcoming && ev.Start.Sub(now) <= time.Minute
		if status != schedule.StatusLive && !upcomingSoon {
			continue
		}
		st, _ := s.catalog.Lookup(ev.Station)
		for _, hz := range ev.Freqs {
			cands := s.candidates(ev, st, hz, now, false)
			rxs := make([]model.Receiver, 0, len(cands))
			for _, c := range cands {
				rxs = append(rxs, c.Receiver)
			}
			out = append(out, probe.Target{
				EventID: ev.ID, FreqHz: hz, Mode: ev.Mode,
				Live: status == schedule.StatusLive, Candidates: rxs,
			})
		}
	}
	return out
}
