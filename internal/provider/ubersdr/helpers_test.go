package ubersdr

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

const fixtureDir = "../../../tests/fixtures/ubersdr"

// record is one WebSocket message from a .rec capture (layout in the
// fixtures README).
type record struct {
	text bool
	atMs uint32
	data []byte
}

func readRecords(t testing.TB, name string) []record {
	t.Helper()
	d, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	var out []record
	for len(d) > 0 {
		if len(d) < 9 {
			t.Fatalf("%s: truncated record header", name)
		}
		n := binary.LittleEndian.Uint32(d[5:9])
		if uint32(len(d)-9) < n {
			t.Fatalf("%s: truncated record", name)
		}
		out = append(out, record{text: d[0] == 1, atMs: binary.LittleEndian.Uint32(d[1:5]), data: d[9 : 9+n]})
		d = d[9+n:]
	}
	return out
}

func binaries(rs []record) [][]byte {
	var out [][]byte
	for _, r := range rs {
		if !r.text {
			out = append(out, r.data)
		}
	}
	return out
}

func readFixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return b
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// uuidRE is the form UberSDR's /connection accepts (lowercase RFC 4122).
var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func jsonDecode(r *http.Request, v any) { _ = json.NewDecoder(r.Body).Decode(v) }

func urlParseQuery(q string) (url.Values, error) { return url.ParseQuery(q) }
