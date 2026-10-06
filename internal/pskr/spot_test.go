package pskr

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type fixtureMsg struct {
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload"`
}

func loadFixture(t *testing.T) []fixtureMsg {
	t.Helper()
	b, err := os.ReadFile("../../tests/fixtures/pskr/sample.json")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []fixtureMsg
	if err := json.Unmarshal(b, &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 20 {
		t.Fatalf("fixture has %d messages, want at least 20", len(msgs))
	}
	return msgs
}

// The fixture is a live capture from mqtt.pskreporter.info; every message
// must parse, agree with its own topic, and land in the store.
func TestParseFixture(t *testing.T) {
	st := NewStore(DefaultParams())
	for _, m := range loadFixture(t) {
		sp, err := ParseSpot(m.Payload)
		if err != nil {
			t.Fatalf("%s: %v", m.Topic, err)
		}
		lv := strings.Split(m.Topic, "/")
		if len(lv) != 11 || lv[0] != "pskr" || lv[1] != "filter" || lv[2] != "v2" {
			t.Fatalf("unexpected topic shape %q", m.Topic)
		}
		if sp.Band != lv[3] || sp.Mode != lv[4] {
			t.Errorf("%s: band/mode %q/%q disagree with topic", m.Topic, sp.Band, sp.Mode)
		}
		if strings.ReplaceAll(sp.SenderCall, "/", ".") != lv[5] || strings.ReplaceAll(sp.ReceiverCall, "/", ".") != lv[6] {
			t.Errorf("%s: calls %q/%q disagree with topic", m.Topic, sp.SenderCall, sp.ReceiverCall)
		}
		if strings.ToUpper(sp.SenderLocator[:4]) != lv[7] || strings.ToUpper(sp.ReceiverLocator[:4]) != lv[8] {
			t.Errorf("%s: locators %q/%q disagree with topic", m.Topic, sp.SenderLocator, sp.ReceiverLocator)
		}
		if sp.FreqHz <= 0 || sp.Time.IsZero() || sp.Seq == 0 || !sp.HasSNR {
			t.Errorf("%s: missing fields: %+v", m.Topic, sp)
		}
		if i, ok := bandIndex("", sp.FreqHz); !ok || HFBands[i].Name != sp.Band {
			t.Errorf("%s: %d Hz is not in band %s", m.Topic, sp.FreqHz, sp.Band)
		}
		if !st.Add(sp, sp.Time) {
			t.Errorf("%s: store refused the spot", m.Topic)
		}
	}
}

func TestParseSpotFields(t *testing.T) {
	sp, err := ParseSpot([]byte(`{"sq":73468726341,"f":18101172,"md":"FT8","rp":-9,"t":1791326160,"t_tx":1791326145,"sc":"JE3EDJ","sl":"PM74SN","rc":"WCF190","rl":"OF77xw","sa":339,"ra":null,"b":"17m","extra":1}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Spot{Seq: 73468726341, FreqHz: 18101172, Mode: "FT8", SNR: -9, HasSNR: true, SenderCall: "JE3EDJ",
		SenderLocator: "PM74SN", ReceiverCall: "WCF190", ReceiverLocator: "OF77xw", SenderADIF: 339, Band: "17m"}
	want.Time = sp.Time
	if sp != want {
		t.Fatalf("got %+v\nwant %+v", sp, want)
	}
	if sp.Time.Unix() != 1791326160 {
		t.Fatalf("time %v", sp.Time)
	}
}

func TestParseSpotErrors(t *testing.T) {
	for _, in := range []string{
		``,
		`not json`,
		`{"f":14074000,"sc":"A1A","rc":"B1B","rl":"IO91"}`,
		`{"f":14074000,"sc":"A1A","rc":"B1B","sl":"IO91"}`,
		`{"f":"fourteen","sl":"IO91","rl":"IO91"}`,
	} {
		if _, err := ParseSpot([]byte(in)); err == nil {
			t.Errorf("%q: want an error", in)
		}
	}
}
