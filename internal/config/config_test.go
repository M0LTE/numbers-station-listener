package config

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := parse(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.PriyomPoll != 15*time.Minute || c.Grace != 10*time.Second || c.PerReceiverCap != 2 || !c.ProbeEnabled {
		t.Fatalf("defaults: %+v", c)
	}
	if !c.Allowed("ANY", "x", "") {
		t.Fatal("empty allow list must allow everything")
	}
}

func TestFloorsAndAllowList(t *testing.T) {
	c, err := parse(env(map[string]string{
		"NSL_PRIYOM_POLL":    "1m",
		"NSL_RECEIVER_ALLOW": "M0LTE, reading-ubersdr",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PriyomPoll != 5*time.Minute {
		t.Fatalf("Priyom poll floor not applied: %v", c.PriyomPoll)
	}
	if !c.Allowed("m0lte", "", "") || !c.Allowed("", "", "https://reading-ubersdr.m0lte.uk/") || c.Allowed("G0ABC", "id", "https://example.org/") {
		t.Fatal("allow list wrong")
	}
}

func TestBadValue(t *testing.T) {
	if _, err := parse(env(map[string]string{"NSL_GRACE": "soon"})); err == nil {
		t.Fatal("expected error")
	}
}
