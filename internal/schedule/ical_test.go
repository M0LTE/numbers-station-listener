package schedule

import (
	"bufio"
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestEscapeText(t *testing.T) {
	cases := map[string]string{
		"plain":                 "plain",
		"a,b;c\\d":              `a\,b\;c\\d`,
		"line1\nline2":          `line1\nline2`,
		"crlf\r\nx":             `crlf\nx`,
		"bell\x07tab\tok":       "belltab\tok",
		`already\,escaped`:      `already\\\,escaped`,
		"Target: East Asia; x,": `Target: East Asia\; x\,`,
	}
	for in, want := range cases {
		if got := escapeText(in); got != want {
			t.Errorf("escapeText(%q) = %q, want %q", in, got, want)
		}
	}
}

func fold(s string) string {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	writeFolded(w, s)
	w.Flush()
	return buf.String()
}

func unfold(s string) string { return strings.ReplaceAll(s, "\r\n ", "") }

func TestFolding(t *testing.T) {
	inputs := []string{
		"SUMMARY:short",
		"DESCRIPTION:" + strings.Repeat("x", 63),  // exactly 75 octets
		"DESCRIPTION:" + strings.Repeat("x", 64),  // 76
		"DESCRIPTION:" + strings.Repeat("y", 500), // many folds
		"DESCRIPTION:" + strings.Repeat("é", 100),
		"DESCRIPTION:a" + strings.Repeat("中文", 60),
		"X:" + strings.Repeat("\xff", 200), // not UTF-8: must still terminate
	}
	for _, in := range inputs {
		out := fold(in)
		if !strings.HasSuffix(out, "\r\n") {
			t.Fatalf("no CRLF terminator: %q", out)
		}
		lines := strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n")
		for i, l := range lines {
			if len(l) > 75 {
				t.Errorf("line %d is %d octets", i, len(l))
			}
			if i > 0 && !strings.HasPrefix(l, " ") {
				t.Errorf("continuation line %d lacks leading space", i)
			}
			if utf8.ValidString(in) && !utf8.ValidString(l) {
				t.Errorf("line %d splits a UTF-8 sequence: %q", i, l)
			}
			if strings.Contains(l, "\n") || strings.Contains(l, "\r") {
				t.Errorf("bare line break in %q", l)
			}
		}
		if got := unfold(strings.TrimSuffix(out, "\r\n")); got != in {
			t.Errorf("unfold mismatch:\n got %q\nwant %q", got, in)
		}
		if len(in) <= 75 && len(lines) != 1 {
			t.Errorf("folded a %d-octet line", len(in))
		}
	}
}

func TestWriteICS(t *testing.T) {
	cat := loadCatalog(t)
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	events := []Event{
		NewEvent(start, "V13 15388kHz USB/AM [Target: East Asia]", cat),
		NewEvent(start.Add(time.Hour), "V13 5422, 4462.5kHz USB/AM (In case of traffic; maybe) [Target: East Asia]", cat),
		NewEvent(start.Add(2*time.Hour), "V13 Search", cat),
	}
	var buf bytes.Buffer
	url := "https://priyom.org/number-stations/other/v13"
	if err := WriteICS(&buf, "V13", events, "New Star Broadcasting Station", url); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Fatal("bare LF in output")
	}
	for _, l := range strings.Split(out, "\r\n") {
		if len(l) > 75 {
			t.Errorf("unfolded line %q", l)
		}
	}
	text := unfold(out)
	lines := strings.Split(strings.TrimSuffix(text, "\r\n"), "\r\n")
	if lines[0] != "BEGIN:VCALENDAR" || lines[len(lines)-1] != "END:VCALENDAR" {
		t.Errorf("not wrapped in VCALENDAR: %q ... %q", lines[0], lines[len(lines)-1])
	}
	has := func(l string) bool {
		for _, x := range lines {
			if x == l {
				return true
			}
		}
		return false
	}
	for _, want := range []string{
		"VERSION:2.0",
		"PRODID:-//numbers-station-listener//schedule//EN",
		"X-WR-CALNAME:V13 New Star Broadcasting Station (numbers-station-listener)",
		"UID:" + events[0].ID + "@numbers-station-listener",
		"DTSTART:20261006T000000Z",
		"DTEND:20261006T005500Z", // V13 typical 55 min
		"SUMMARY:V13 15388 kHz USB/AM",
		`SUMMARY:V13 5422\, 4462.5 kHz USB/AM`,
		"SUMMARY:V13 Search",
		`DESCRIPTION:Target: East Asia\nEnd time is an estimate.\nSchedule: Priyom.org (CC BY-NC-SA 4.0)\n` + url,
		`DESCRIPTION:In case of traffic\; maybe\nTarget: East Asia\nEnd time is an estimate.\nSchedule: Priyom.org (CC BY-NC-SA 4.0)\n` + url,
		"URL:" + url,
	} {
		if !has(want) {
			t.Errorf("missing line %q in\n%s", want, text)
		}
	}
	if n := strings.Count(text, "BEGIN:VEVENT\r\n"); n != 3 || strings.Count(text, "END:VEVENT\r\n") != 3 {
		t.Errorf("VEVENT count %d", n)
	}
	stamp := regexp.MustCompile(`(?m)^DTSTAMP:\d{8}T\d{6}Z\r$`)
	if n := len(stamp.FindAllString(text, -1)); n != 3 {
		t.Errorf("DTSTAMP lines = %d, want 3", n)
	}
	for _, l := range lines {
		for _, r := range l {
			if r >= 0x80 {
				t.Fatalf("unexpected non-ASCII in %q", l)
			}
		}
	}
}

func TestFormatKHz(t *testing.T) {
	for hz, want := range map[int64]string{15388000: "15388", 4625500: "4625.5", 4625250: "4625.25", 1: "0.001"} {
		if got := FormatKHz(hz); got != want {
			t.Errorf("FormatKHz(%d) = %q, want %q", hz, got, want)
		}
	}
}
