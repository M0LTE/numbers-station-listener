package schedule

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func repoFile(t testing.TB, rel string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..", rel)
}

func loadFixture(t testing.TB) []Item {
	t.Helper()
	b, err := os.ReadFile(repoFile(t, "tests/fixtures/priyom-2026-10-06.json"))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Items []Item `json:"items"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	return body.Items
}

func TestParseSummary(t *testing.T) {
	cases := []struct {
		in   string
		want Parsed
	}{
		{"V13 15388kHz USB/AM [Target: East Asia]", Parsed{OK: true, Station: "V13", Freqs: []int64{15388000}, Mode: "USB/AM", Remarks: []string{"Target: East Asia"}, Target: "East Asia"}},
		{"F06 Search [Last used: 11405kHz]", Parsed{OK: true, Station: "F06", Search: true, Remarks: []string{"Last used: 11405kHz"}}},
		{"XPA2 Search", Parsed{OK: true, Station: "XPA2", Search: true}},
		{"F03 Search (May not always transmit)", Parsed{OK: true, Station: "F03", Search: true, Remarks: []string{"May not always transmit"}}},
		{"M12 14537kHz CW (In case of traffic) [Target: Pacific]", Parsed{OK: true, Station: "M12", Freqs: []int64{14537000}, Mode: "CW", Remarks: []string{"In case of traffic", "Target: Pacific"}, Target: "Pacific"}},
		{"F06 8175kHz RTTY", Parsed{OK: true, Station: "F06", Freqs: []int64{8175000}, Mode: "RTTY"}},
		{"E11 5422kHz, 4462kHz USB", Parsed{OK: true, Station: "E11", Freqs: []int64{5422000, 4462000}, Mode: "USB"}},
		{"E11 5422, 4462kHz USB", Parsed{OK: true, Station: "E11", Freqs: []int64{5422000, 4462000}, Mode: "USB"}},
		{"E11 4625.5kHz USB", Parsed{OK: true, Station: "E11", Freqs: []int64{4625500}, Mode: "USB"}},
		{"E11  5422 kHz ,4462.25 KHZ  USB ", Parsed{OK: true, Station: "E11", Freqs: []int64{5422000, 4462250}, Mode: "USB"}},
		{"V13 5422kHz/4462kHz USB/AM", Parsed{OK: true, Station: "V13", Freqs: []int64{5422000, 4462000}, Mode: "USB/AM"}},
		{"V13 5422, 5422kHz USB", Parsed{OK: true, Station: "V13", Freqs: []int64{5422000}, Mode: "USB"}},
		{"V13 15388 USB", Parsed{OK: true, Station: "V13", Freqs: []int64{15388000}, Mode: "USB"}},
		{"F01 5.4MHz FSK 200/1000", Parsed{OK: true, Station: "F01", Freqs: []int64{5400000}, Mode: "FSK 200/1000"}},
		{"XPA 9100kHz MFSK-64 [a (b)] trailing words", Parsed{OK: true, Station: "XPA", Freqs: []int64{9100000}, Mode: "MFSK-64", Remarks: []string{"a (b)", "trailing words"}}},
		{"E11 5422kHz", Parsed{OK: true, Station: "E11", Freqs: []int64{5422000}}},
		{"V13: 15388kHz USB", Parsed{OK: true, Station: "V13", Freqs: []int64{15388000}, Mode: "USB"}},
		{"S11a 10728kHz USB [target : East Asia", Parsed{OK: true, Station: "S11a", Freqs: []int64{10728000}, Mode: "USB", Remarks: []string{"target : East Asia"}, Target: "East Asia"}},
		{"XPA2 search ongoing [x]", Parsed{OK: true, Station: "XPA2", Search: true, Remarks: []string{"ongoing", "x"}}},
		{"V13 15388kHz ZORK-9", Parsed{OK: true, Station: "V13", Freqs: []int64{15388000}, Mode: "ZORK-9"}},
		{"V13 15388kHz USB [] ()", Parsed{OK: true, Station: "V13", Freqs: []int64{15388000}, Mode: "USB"}},
		// Unparseable: Raw and the station token if any, nothing else.
		{"", Parsed{}},
		{"V13", Parsed{Station: "V13"}},
		{"V13 Unknown frequency", Parsed{Station: "V13"}},
		{"[note] V13 1234kHz USB", Parsed{}},
		{"??? 1234kHz", Parsed{}},
		{"V13 0kHz USB", Parsed{Station: "V13"}},
		{"V13 99999999999kHz USB", Parsed{Station: "V13"}},
	}
	for _, c := range cases {
		got := ParseSummary(c.in)
		c.want.Raw = c.in
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseSummary(%q)\n got %+v\nwant %+v", c.in, got, c.want)
		}
	}
}

func TestParseFixture(t *testing.T) {
	items := loadFixture(t)
	if len(items) != 128 {
		t.Fatalf("fixture has %d items, want 128", len(items))
	}
	counts := map[string]int{}
	var search, traffic []string
	for _, it := range items {
		p := ParseSummary(it.Summary)
		if !p.OK {
			t.Errorf("fixture summary did not parse: %q", it.Summary)
		}
		counts[p.Station]++
		if p.Search {
			search = append(search, it.Summary)
			if len(p.Freqs) != 0 {
				t.Errorf("search entry with freqs: %q", it.Summary)
			}
		} else if len(p.Freqs) == 0 {
			t.Errorf("no freqs: %q", it.Summary)
		}
		for _, r := range p.Remarks {
			if r == "In case of traffic" {
				traffic = append(traffic, it.Summary)
			}
		}
	}
	want := map[string]int{
		"V13": 24, "F06": 18, "XPB": 12, "E11": 11, "M12": 9, "XPA2": 9, "F01": 6, "F06a": 6,
		"F07": 6, "F03l": 4, "F03j": 3, "P03": 3, "E07": 3, "V32": 2, "S06": 2, "S11a": 2,
		"M14": 2, "M01": 2, "P03i": 1, "F03": 1, "P03k": 1, "P03g": 1,
	}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("counts by station\n got %v\nwant %v", counts, want)
	}
	if len(search) != 5 {
		t.Errorf("search entries = %d %q, want 5", len(search), search)
	}
	wantTraffic := []string{
		"M12 14537kHz CW (In case of traffic) [Target: Pacific]",
		"E07 14361kHz USB (In case of traffic)",
	}
	if !reflect.DeepEqual(traffic, wantTraffic) {
		t.Errorf("in-case-of-traffic entries = %q, want %q", traffic, wantTraffic)
	}
}

func FuzzParseSummary(f *testing.F) {
	for _, it := range loadFixture(f) {
		f.Add(it.Summary)
	}
	for _, s := range []string{
		"", " ", "[", "(", "]", ")", "[[(", "E11 5422kHz, 4462kHz USB", "E11 5422, 4462kHz USB",
		"E11 4625.5kHz USB", "V13 1.kHz", "V13 .5kHz", "V13 5422,", "V13 5422, ,", "V13 ,5422",
		"V13 99999999999999999999kHz", "V13 1e9kHz", "V13 \xff\xfekHz", "\xff", "V13 5422kHz [\xff",
		"V13 Search:", "V13 Search: [Target:]", "Target: x", "V13 5422 MHzMHz", "V13 0.0004Hz",
	} {
		f.Add(s)
	}
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, s string) {
		p := ParseSummary(s)
		if p.Raw != s {
			t.Fatalf("Raw changed: %q -> %q", s, p.Raw)
		}
		if !p.OK {
			if p.Freqs != nil || p.Remarks != nil || p.Mode != "" || p.Search || p.Target != "" {
				t.Fatalf("unparsed result carries fields: %+v", p)
			}
		} else if p.Search {
			if len(p.Freqs) != 0 {
				t.Fatalf("search with freqs: %+v", p)
			}
		} else {
			if len(p.Freqs) == 0 {
				t.Fatalf("parsed without freqs: %+v", p)
			}
			for _, hz := range p.Freqs {
				if hz <= 0 || hz > maxHz {
					t.Fatalf("bad freq %d in %q", hz, s)
				}
			}
		}
		if p.OK && p.Station == "" {
			t.Fatalf("parsed without station: %q", s)
		}
		ev := NewEvent(start, s, nil)
		if ev.ID == "" || ev.Raw != s {
			t.Fatalf("bad event %+v", ev)
		}
		MapMode(p.Mode)
		_ = icsSummary(ev)
		_ = escapeText(s)
	})
}
