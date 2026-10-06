package stations

import (
	"path/filepath"
	"runtime"
	"testing"
)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..", rel)
}

func TestLoadRealCatalogue(t *testing.T) {
	c, err := Load(repoFile(t, "data/stations.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Meta.Licence == "" {
		t.Fatal("catalogue lost its licence metadata")
	}
	for _, s := range c.All() {
		if s.PriyomURL == "" {
			t.Errorf("%s has no Priyom URL (attribution)", s.Designator)
		}
	}
}

func TestVariantFallback(t *testing.T) {
	c, err := Parse([]byte(`{"F03":{"name":"a"},"S11a":{"name":"b"},"XPA":{"name":"c"},"XPA2":{"name":"d"}}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{"F03j": "a", "f03l": "a", "S11a": "b", "XPA2": "d", "XPA": "c"}
	for in, want := range cases {
		s, ok := c.Lookup(in)
		if !ok || s.Name != want {
			t.Errorf("Lookup(%q) = %v,%v want %q", in, s, ok, want)
		}
	}
	if _, ok := c.Lookup("XPB"); ok {
		t.Error("XPB should not resolve")
	}
	if _, ok := c.Lookup("XPAB"); ok {
		t.Error("XPAB must not fall back to XPA (no digit before the suffix)")
	}
}
