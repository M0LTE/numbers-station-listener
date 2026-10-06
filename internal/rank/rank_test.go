package rank

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
	"github.com/m0lte/numbers-station-listener/internal/stations"
)

func site(lat, lon float64, label string) *stations.Site {
	return &stations.Site{Lat: &lat, Lon: &lon, Label: label, Confidence: "known"}
}

var e11 = site(warsaw[0], warsaw[1], "Chotomow near Warsaw, Poland")

// rx is an eligible receiver at a position; tweak fields per test.
func rx(id string, lat, lon float64) model.Receiver {
	return model.Receiver{
		Provider: "ubersdr", ID: id, Callsign: strings.ToUpper(id),
		Lat: lat, Lon: lon, HasPos: true,
		MinHz: 10000, MaxHz: 30000000,
		Online: true, AntennaConnected: true, LoadStatus: "ok",
		MaxClients: 20, AvailableClients: 10,
		SNR: 25, HasSNR: true,
	}
}

func keys(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Receiver.Key()
	}
	return out
}

func find(t *testing.T, cs []Candidate, key string) Candidate {
	t.Helper()
	for _, c := range cs {
		if c.Receiver.Key() == key {
			return c
		}
	}
	t.Fatalf("%s not in %v", key, keys(cs))
	return Candidate{}
}

func TestHardFilters(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.Receiver)
		held   bool
		want   bool
	}{
		{"baseline eligible", func(*model.Receiver) {}, false, true},
		{"offline", func(r *model.Receiver) { r.Online = false }, false, false},
		{"below tuning range", func(r *model.Receiver) { r.MinHz = 6000000 }, false, false},
		{"above tuning range", func(r *model.Receiver) { r.MaxHz = 4999999 }, false, false},
		{"range edges are inclusive", func(r *model.Receiver) { r.MinHz, r.MaxHz = 5000000, 5000000 }, false, true},
		{"no range reported covers everything", func(r *model.Receiver) { r.MinHz, r.MaxHz = 0, 0 }, false, true},
		{"no free slots", func(r *model.Receiver) { r.AvailableClients = 0 }, false, false},
		{"no free slots but held", func(r *model.Receiver) { r.AvailableClients = 0 }, true, true},
		{"overloaded", func(r *model.Receiver) { r.LoadStatus = "overloaded" }, false, false},
		{"full", func(r *model.Receiver) { r.LoadStatus = "full" }, false, false},
		{"critical", func(r *model.Receiver) { r.LoadStatus = "critical" }, false, false},
		{"Critical any case", func(r *model.Receiver) { r.LoadStatus = " CRITICAL " }, false, false},
		{"overloaded even when held", func(r *model.Receiver) { r.LoadStatus = "full" }, true, false},
		{"warning is fine", func(r *model.Receiver) { r.LoadStatus = "warning" }, false, true},
		{"empty status is fine", func(r *model.Receiver) { r.LoadStatus = "" }, false, true},
		{"OK is fine", func(r *model.Receiver) { r.LoadStatus = "OK" }, false, true},
		{"antenna disconnected", func(r *model.Receiver) { r.AntennaConnected = false }, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := rx("a", reading[0], reading[1])
			c.mutate(&r)
			held := c.held
			got := Rank(Input{
				Receivers: []model.Receiver{r},
				Tx:        e11, FreqHz: 5000000, At: nightUTC,
				Held: func(k string) bool { return held && k == r.Key() },
			})
			if (len(got) == 1) != c.want {
				t.Errorf("eligible = %v, want %v", len(got) == 1, c.want)
			}
		})
	}
}

func TestHeldReceiverScoresFullLoad(t *testing.T) {
	r := rx("a", reading[0], reading[1])
	r.AvailableClients = 0
	got := Rank(Input{
		Receivers: []model.Receiver{r}, Tx: e11, FreqHz: 5000000, At: nightUTC,
		Held: func(string) bool { return true },
	})
	if len(got) != 1 {
		t.Fatal("held receiver filtered out")
	}
	if got[0].Components.Load != 1 || !hasReason(got[0].Reasons, "already relaying") {
		t.Errorf("load %v reasons %v", got[0].Components.Load, got[0].Reasons)
	}
}

func TestProbeDominates(t *testing.T) {
	// "good" has everything going for it except the probe; "probed" sits
	// 8700 km away on a night 5 MHz path with a poor receiver and few
	// slots, but the probe hears the signal strongly.
	good := rx("good", reading[0], reading[1])
	good.SNR, good.AvailableClients = 40, 20
	probed := rx("probed", utah[0], utah[1])
	probed.SNR, probed.AvailableClients = 12, 2
	absent := rx("absent", reading[0]+0.1, reading[1])
	absent.SNR, absent.AvailableClients = 40, 20

	in := Input{
		Receivers: []model.Receiver{good, probed, absent},
		Tx:        e11, FreqHz: 5000000, At: nightUTC,
		Probes: map[string]provider.ProbeResult{
			probed.Key(): {OK: true, Present: true, SNR: 25},
			absent.Key(): {OK: true, Present: false, SNR: 1},
		},
	}
	got := Rank(in)
	if k := keys(got); k[0] != "ubersdr:probed" || k[2] != "ubersdr:absent" {
		t.Fatalf("order %v, want probed first and absent last", k)
	}
	for _, c := range got {
		t.Logf("%s %.3f %+v %v", c.Receiver.Key(), c.Score, c.Components, c.Reasons)
	}
	// Absent is pushed down hard: below half of the otherwise-identical
	// unprobed receiver.
	if a, g := find(t, got, "ubersdr:absent").Score, find(t, got, "ubersdr:good").Score; a > g/2 {
		t.Errorf("absent %.3f not pushed below half of good %.3f", a, g)
	}
	if !hasReason(find(t, got, "ubersdr:probed").Reasons, "signal detected, 25 dB SNR") {
		t.Error("missing probe reason")
	}

	// A failed probe is neutral, the same as no probe at all.
	in.Probes = map[string]provider.ProbeResult{good.Key(): {OK: false, Err: "rate limited"}}
	failed := find(t, Rank(in), good.Key())
	in.Probes = nil
	none := find(t, Rank(in), good.Key())
	if failed.Score != none.Score || !hasReason(failed.Reasons, "probe failed") {
		t.Errorf("failed probe %.3f %v, unprobed %.3f", failed.Score, failed.Reasons, none.Score)
	}

	// A present probe never scores below an unprobed twin, even at 0 dB.
	twin := good
	twin.ID = "twin"
	in.Receivers = []model.Receiver{good, twin}
	in.Probes = map[string]provider.ProbeResult{twin.Key(): {OK: true, Present: true, SNR: 0}}
	got = Rank(in)
	if find(t, got, twin.Key()).Score < find(t, got, good.Key()).Score {
		t.Error("weak present probe scored below unprobed twin")
	}
}

func TestNightLowBandEuropeBeatsLongPath(t *testing.T) {
	near := rx("reading", reading[0], reading[1])
	far := rx("utah", utah[0], utah[1])
	got := Rank(Input{Receivers: []model.Receiver{far, near}, Tx: e11, FreqHz: 5000000, At: nightUTC})
	if keys(got)[0] != "ubersdr:reading" {
		t.Fatalf("order %v", keys(got))
	}
	r := got[0]
	if r.DistanceKm == nil || math.Abs(*r.DistanceKm-1497) > 2 {
		t.Errorf("distance %v, want about 1497 km", r.DistanceKm)
	}
	if !hasReason(r.Reasons, "night path, 1500 km") {
		t.Errorf("reasons %v", r.Reasons)
	}
	if got[0].Components.Path-got[1].Components.Path < 0.5 {
		t.Errorf("path components %.3f vs %.3f, want a clear gap", got[0].Components.Path, got[1].Components.Path)
	}
}

func TestDaytimeHighBandTaiwanToEuropePlausible(t *testing.T) {
	tw := site(taipei[0], taipei[1], "Taiwan")
	got := Rank(Input{Receivers: []model.Receiver{rx("reading", reading[0], reading[1])}, Tx: tw, FreqHz: 15000000, At: dayUTC})
	c := got[0]
	if c.Components.Path < 0.5 {
		t.Errorf("path %.3f, want plausible (>= 0.5); reasons %v", c.Components.Path, c.Reasons)
	}
	if !hasReason(c.Reasons, "daylight path") {
		t.Errorf("reasons %v", c.Reasons)
	}
}

func TestUnknownTransmitterIsNeutral(t *testing.T) {
	r := rx("a", reading[0], reading[1])
	for name, tx := range map[string]*stations.Site{
		"nil site":       nil,
		"no coordinates": {Label: "somewhere", Confidence: "unknown"},
		"lat only":       {Lat: e11.Lat, Confidence: "unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			got := Rank(Input{Receivers: []model.Receiver{r}, Tx: tx, FreqHz: 5000000, At: nightUTC})
			c := got[0]
			if c.Components.Path != 0.5 || c.DistanceKm != nil || !hasReason(c.Reasons, "transmitter site unknown") {
				t.Errorf("path %v distance %v reasons %v", c.Components.Path, c.DistanceKm, c.Reasons)
			}
		})
	}
}

func TestReceiverWithoutPositionIsNeutral(t *testing.T) {
	r := rx("a", 0, 0)
	r.HasPos = false
	c := Rank(Input{Receivers: []model.Receiver{r}, Tx: e11, FreqHz: 5000000, At: nightUTC})[0]
	if c.Components.Path != 0.5 || c.DistanceKm != nil || !hasReason(c.Reasons, "receiver location unknown") {
		t.Errorf("path %v distance %v reasons %v", c.Components.Path, c.DistanceKm, c.Reasons)
	}
}

func TestAlternatesUseBestSite(t *testing.T) {
	// S06: Moscow, Orenburg or Chita. A receiver in Mongolia (Ulaanbaatar)
	// is about 550 km from Chita and over 4500 km from Moscow, so at night
	// on 5 MHz Chita is its best site.
	moscow := site(55.76, 37.62, "Moscow, Russia")
	alts := []stations.Site{*site(51.77, 55.1, "Orenburg, Russia"), *site(52.03, 113.5, "Chita, Russia")}
	mongolia := rx("ub", 47.92, 106.92)
	london := rx("london", reading[0], reading[1])
	when := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC) // after midnight in Mongolia
	got := Rank(Input{Receivers: []model.Receiver{mongolia, london}, Tx: moscow, Alternates: alts, FreqHz: 5000000, At: when})

	ub := find(t, got, mongolia.Key())
	want := DistanceKm(52.03, 113.5, 47.92, 106.92)
	if ub.DistanceKm == nil || math.Abs(*ub.DistanceKm-want) > 0.01 {
		t.Errorf("distance %v, want %.1f (from Chita)", ub.DistanceKm, want)
	}
	if !hasReason(ub.Reasons, "best transmitter site: Chita, Russia") {
		t.Errorf("reasons %v", ub.Reasons)
	}
	if !hasReason(find(t, got, london.Key()).Reasons, "best transmitter site: Moscow, Russia") {
		t.Errorf("london reasons %v", find(t, got, london.Key()).Reasons)
	}

	// Unknown primary but known alternates still gives a real path.
	got = Rank(Input{Receivers: []model.Receiver{mongolia}, Alternates: alts, FreqHz: 5000000, At: when})
	if got[0].DistanceKm == nil {
		t.Error("alternates ignored when primary site unknown")
	}
}

func TestQualityComponent(t *testing.T) {
	cases := []struct {
		name   string
		snr    float64
		hasSNR bool
		want   float64
	}{
		{"M0LTE reports -1 meaning unknown", -1, true, 0.5},
		{"zero is unknown", 0, true, 0.5},
		{"not reported", 30, false, 0.5},
		{"low end", 10, true, 0},
		{"below range clamps", 5, true, 0},
		{"middle", 22.5, true, 0.5},
		{"top", 35, true, 1},
		{"above range clamps", 45, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := rx("a", reading[0], reading[1])
			r.SNR, r.HasSNR = c.snr, c.hasSNR
			got := Rank(Input{Receivers: []model.Receiver{r}, Tx: e11, FreqHz: 5000000, At: nightUTC})[0]
			near(t, "quality", got.Components.Quality, c.want, 1e-9)
		})
	}
}

func TestLoadComponent(t *testing.T) {
	r := rx("a", reading[0], reading[1])
	r.MaxClients, r.AvailableClients = 20, 18
	c := Rank(Input{Receivers: []model.Receiver{r}, Tx: e11, FreqHz: 5000000, At: nightUTC})[0]
	near(t, "load", c.Components.Load, 0.9, 1e-9)
	if !hasReason(c.Reasons, "18 free slots") {
		t.Errorf("reasons %v", c.Reasons)
	}
	r.MaxClients, r.AvailableClients = 0, 3
	c = Rank(Input{Receivers: []model.Receiver{r}, Tx: e11, FreqHz: 5000000, At: nightUTC})[0]
	near(t, "unknown capacity is neutral", c.Components.Load, 0.5, 1e-9)
}

func TestTieBreakByKey(t *testing.T) {
	var rs []model.Receiver
	for _, id := range []string{"c", "a", "b"} {
		rs = append(rs, rx(id, reading[0], reading[1]))
	}
	got := Rank(Input{Receivers: rs, Tx: e11, FreqHz: 5000000, At: nightUTC})
	if k := strings.Join(keys(got), ","); k != "ubersdr:a,ubersdr:b,ubersdr:c" {
		t.Errorf("order %s", k)
	}
}

func TestPathOpenHook(t *testing.T) {
	a, b := rx("a", reading[0], reading[1]), rx("b", reading[0], reading[1])
	hook := func(r model.Receiver) (float64, string, bool) {
		if r.ID == "b" {
			return 1, "path open on PSKReporter", true
		}
		return 0, "", false
	}
	// Weight 0 (the default): the hook is ignored.
	got := Rank(Input{Receivers: []model.Receiver{a, b}, Tx: e11, FreqHz: 5000000, At: nightUTC, PathOpen: hook})
	if got[0].Score != got[1].Score {
		t.Errorf("hook counted at weight 0: %.3f vs %.3f", got[0].Score, got[1].Score)
	}
	w := DefaultWeights()
	w.PSKReporter = 0.2
	got = Rank(Input{Receivers: []model.Receiver{a, b}, Tx: e11, FreqHz: 5000000, At: nightUTC, PathOpen: hook, Weights: w})
	if got[0].Receiver.ID != "b" || !hasReason(got[0].Reasons, "path open on PSKReporter") {
		t.Errorf("hook ignored: %v %v", keys(got), got[0].Reasons)
	}
}

func TestWeightsNormalise(t *testing.T) {
	r := rx("a", reading[0], reading[1])
	in := Input{Receivers: []model.Receiver{r}, Tx: e11, FreqHz: 5000000, At: nightUTC}
	base := Rank(in)[0].Score
	in.Weights = Weights{Probe: 1, Path: 0.6, Quality: 0.2, Load: 0.2} // defaults doubled
	near(t, "scaled weights give the same score", Rank(in)[0].Score, base, 1e-12)
	in.Weights = Weights{Path: 1}
	near(t, "path only", Rank(in)[0].Score, Rank(in)[0].Components.Path, 1e-12)
}

func TestParseWeights(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    Weights
		wantErr bool
	}{
		{"empty gives defaults", "", DefaultWeights(), false},
		{"blank gives defaults", "  \n", DefaultWeights(), false},
		{"partial override", `{"probe":0.6,"path":0.2}`, Weights{Probe: 0.6, Path: 0.2, Quality: 0.1, Load: 0.1}, false},
		{"pskreporter", `{"pskreporter":0.15}`, Weights{Probe: 0.5, Path: 0.3, Quality: 0.1, Load: 0.1, PSKReporter: 0.15}, false},
		{"zero one component", `{"quality":0}`, Weights{Probe: 0.5, Path: 0.3, Load: 0.1}, false},
		{"unknown field", `{"prob":0.6}`, Weights{}, true},
		{"negative", `{"load":-0.1}`, Weights{}, true},
		{"all zero", `{"probe":0,"path":0,"quality":0,"load":0}`, Weights{}, true},
		{"not json", `probe=0.5`, Weights{}, true},
		{"trailing data", `{"probe":0.5} {}`, Weights{}, true},
		{"string value", `{"probe":"0.5"}`, Weights{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseWeights(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err %v, wantErr %v", err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// loadDirectoryFixture maps the captured UberSDR directory onto receivers.
// It is a test-only mapping of the fields the ranker reads; the real one
// belongs to the UberSDR provider.
func loadDirectoryFixture(t *testing.T) []model.Receiver {
	t.Helper()
	b, err := os.ReadFile("../../tests/fixtures/ubersdr-directory.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Instances []struct {
			ID               string  `json:"id"`
			Callsign         string  `json:"callsign"`
			Lat              float64 `json:"latitude"`
			Lon              float64 `json:"longitude"`
			Country          string  `json:"country_code"`
			LoadStatus       string  `json:"load_status"`
			MaxClients       int     `json:"max_clients"`
			AvailableClients int     `json:"available_clients"`
			SNR              float64 `json:"snr_1_8_30_mhz"`
			Antenna          bool    `json:"antenna_connected"`
			Online           bool    `json:"is_online"`
			Range            struct {
				Min int64 `json:"min_frequency"`
				Max int64 `json:"max_frequency"`
			} `json:"tuning_range"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var out []model.Receiver
	for _, i := range doc.Instances {
		out = append(out, model.Receiver{
			Provider: "ubersdr", ID: i.ID, Callsign: i.Callsign, Country: i.Country,
			Lat: i.Lat, Lon: i.Lon, HasPos: true,
			MinHz: i.Range.Min, MaxHz: i.Range.Max,
			Online: i.Online, AntennaConnected: i.Antenna, LoadStatus: i.LoadStatus,
			MaxClients: i.MaxClients, AvailableClients: i.AvailableClients,
			SNR: i.SNR, HasSNR: true,
		})
	}
	return out
}

// The whole live directory: E11 on 5 MHz at night. Critical receivers drop
// out, every score is in range, the order is sorted, and Europe leads.
func TestRankLiveDirectory(t *testing.T) {
	rs := loadDirectoryFixture(t)
	if len(rs) != 53 {
		t.Fatalf("fixture has %d receivers, want 53", len(rs))
	}
	got := Rank(Input{Receivers: rs, Tx: e11, FreqHz: 5000000, At: nightUTC})
	if len(got) != 50 {
		t.Errorf("%d candidates, want 50 (53 less 3 critical)", len(got))
	}
	europe := map[string]bool{"gb": true, "be": true, "nl": true, "fr": true, "de": true, "ie": true, "ch": true, "at": true, "it": true, "gr": true, "es": true}
	for i, c := range got {
		if c.Score < 0 || c.Score > 1 || math.IsNaN(c.Score) {
			t.Errorf("%s score %v", c.Receiver.Callsign, c.Score)
		}
		if strings.EqualFold(c.Receiver.LoadStatus, "critical") {
			t.Errorf("%s is critical but ranked", c.Receiver.Callsign)
		}
		if i > 0 && c.Score > got[i-1].Score {
			t.Errorf("not sorted at %d", i)
		}
		if i < 10 && !europe[c.Receiver.Country] {
			t.Errorf("top 10 includes %s (%s)", c.Receiver.Callsign, c.Receiver.Country)
		}
	}
	m0lte := find(t, got, "ubersdr:b838fc45-8dd2-4fa8-bb0d-8670244ad5da")
	near(t, "M0LTE quality (-1 is unknown)", m0lte.Components.Quality, 0.5, 0)
	for _, c := range got[:5] {
		t.Logf("%-10s %s %.3f %v", c.Receiver.Callsign, c.Receiver.Country, c.Score, c.Reasons)
	}
}
