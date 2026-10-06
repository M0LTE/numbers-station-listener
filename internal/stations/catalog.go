// Package stations loads the hand-curated station catalogue
// (data/stations.json, derived from Priyom.org, CC BY-NC-SA 4.0).
package stations

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"
)

// Site is a transmitter location.
type Site struct {
	Lat        *float64 `json:"lat"`
	Lon        *float64 `json:"lon"`
	Label      string   `json:"label"`
	Confidence string   `json:"confidence"` // known | approximate | unknown
}

// Known reports whether the site has coordinates.
func (s *Site) Known() bool { return s != nil && s.Lat != nil && s.Lon != nil }

// Station is one catalogue entry.
type Station struct {
	Designator         string `json:"designator"`
	Name               string `json:"name"`
	PriyomURL          string `json:"priyomUrl"`
	Category           string `json:"category"`
	Language           string `json:"language"`
	Operator           string `json:"operator"`
	TxSite             *Site  `json:"txSite"`
	Alternates         []Site `json:"alternates,omitempty"`
	TypicalDurationMin *int   `json:"typicalDurationMin"`
	DurationSource     string `json:"durationSource"`
	Notes              string `json:"notes"`
}

// Meta is the catalogue's provenance block.
type Meta struct {
	Source       string `json:"source"`
	Licence      string `json:"licence"`
	Retrieved    string `json:"retrieved"`
	ReviewStatus string `json:"reviewStatus"`
}

// Catalog is an immutable set of stations.
type Catalog struct {
	Meta     Meta
	stations map[string]*Station
}

// Load reads a catalogue file.
func Load(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes catalogue JSON.
func Parse(b []byte) (*Catalog, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("stations: %w", err)
	}
	c := &Catalog{stations: map[string]*Station{}}
	for k, v := range raw {
		if k == "_meta" {
			if err := json.Unmarshal(v, &c.Meta); err != nil {
				return nil, fmt.Errorf("stations: _meta: %w", err)
			}
			continue
		}
		var s Station
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("stations: %s: %w", k, err)
		}
		s.Designator = k
		c.stations[strings.ToUpper(k)] = &s
	}
	return c, nil
}

// Empty is a catalogue with no stations, for when the file is missing.
func Empty() *Catalog { return &Catalog{stations: map[string]*Station{}} }

// Lookup finds a station by designator. A variant such as "F03j" or
// "S11a" falls back to its base ("F03", "S11") when it has no entry of its
// own. Matching ignores case for the lookup but an exact entry wins.
func (c *Catalog) Lookup(designator string) (*Station, bool) {
	d := strings.ToUpper(strings.TrimSpace(designator))
	if s, ok := c.stations[d]; ok {
		return s, true
	}
	base := strings.TrimRightFunc(d, func(r rune) bool { return unicode.IsLetter(r) })
	// Only strip a suffix after digits: "XPA2" has none, "XPA" is a base.
	if base != d && base != "" && unicode.IsDigit(rune(base[len(base)-1])) {
		if s, ok := c.stations[base]; ok {
			return s, true
		}
	}
	return nil, false
}

// All returns every station sorted by designator.
func (c *Catalog) All() []*Station {
	out := make([]*Station, 0, len(c.stations))
	for _, s := range c.stations {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Designator < out[j].Designator })
	return out
}
