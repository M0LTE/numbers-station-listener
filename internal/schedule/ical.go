package schedule

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const icsTime = "20060102T150405Z"

// WriteICS writes events as an RFC 5545 calendar for one station. The
// caller chooses which events to include (typically that station's, in the
// cached window). stationName and priyomURL may be empty.
func WriteICS(w io.Writer, station string, events []Event, stationName, priyomURL string) error {
	bw := bufio.NewWriter(w)
	line := func(name, value string) {
		writeFolded(bw, name+":"+value)
	}
	calName := station
	if stationName != "" {
		calName = station + " " + stationName
	}
	stamp := time.Now().UTC().Format(icsTime)

	line("BEGIN", "VCALENDAR")
	line("VERSION", "2.0")
	line("PRODID", "-//numbers-station-listener//schedule//EN")
	line("CALSCALE", "GREGORIAN")
	line("METHOD", "PUBLISH")
	line("X-WR-CALNAME", escapeText(calName+" (numbers-station-listener)"))
	line("X-WR-CALDESC", escapeText("Schedule from Priyom.org, CC BY-NC-SA 4.0"))
	for _, ev := range events {
		line("BEGIN", "VEVENT")
		line("UID", ev.ID+"@numbers-station-listener")
		line("DTSTAMP", stamp)
		line("DTSTART", ev.Start.UTC().Format(icsTime))
		line("DTEND", ev.End().UTC().Format(icsTime))
		line("SUMMARY", escapeText(icsSummary(ev)))
		line("DESCRIPTION", escapeText(icsDescription(ev, priyomURL)))
		if priyomURL != "" {
			line("URL", priyomURL)
		}
		line("TRANSP", "TRANSPARENT")
		line("END", "VEVENT")
	}
	line("END", "VCALENDAR")
	return bw.Flush()
}

// icsSummary is "V13 15388 kHz USB/AM", "E11 5422, 4462 kHz USB",
// "XPA2 Search", or the raw summary when it did not parse.
func icsSummary(ev Event) string {
	if !ev.Parsed {
		return ev.Raw
	}
	if ev.Search {
		return ev.Station + " Search"
	}
	parts := make([]string, len(ev.Freqs))
	for i, hz := range ev.Freqs {
		parts[i] = FormatKHz(hz)
	}
	s := ev.Station + " " + strings.Join(parts, ", ") + " kHz"
	if ev.PriyomMode != "" {
		s += " " + ev.PriyomMode
	}
	return s
}

func icsDescription(ev Event, priyomURL string) string {
	var lines []string
	lines = append(lines, ev.Remarks...)
	if ev.Digital {
		lines = append(lines, "Digital mode: listen in USB")
	}
	lines = append(lines, "End time is an estimate.")
	lines = append(lines, "Schedule: Priyom.org (CC BY-NC-SA 4.0)")
	if priyomURL != "" {
		lines = append(lines, priyomURL)
	}
	return strings.Join(lines, "\n")
}

// FormatKHz renders Hz as kHz without trailing zeros: 15388000 -> "15388",
// 4625500 -> "4625.5".
func FormatKHz(hz int64) string {
	if hz%1000 == 0 {
		return strconv.FormatInt(hz/1000, 10)
	}
	return strconv.FormatFloat(float64(hz)/1000, 'f', -1, 64)
}

// escapeText escapes an RFC 5545 TEXT value (section 3.3.11).
func escapeText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case ';':
			b.WriteString(`\;`)
		case ',':
			b.WriteString(`\,`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				continue
			}
			b.WriteString(`\n`)
		default:
			if (c < 0x20 && c != '\t') || c == 0x7f {
				continue // controls other than newline are not allowed
			}
			b.WriteByte(c)
		}
	}
	return b.String()
}

// writeFolded writes one content line, folded so no physical line exceeds
// 75 octets (RFC 5545 section 3.1), never splitting a UTF-8 sequence, and
// terminated with CRLF.
func writeFolded(w *bufio.Writer, s string) {
	limit := 75
	for len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if cut == 0 { // not UTF-8 at all; cut anywhere rather than loop
			cut = limit
		}
		w.WriteString(s[:cut])
		w.WriteString("\r\n ")
		s = s[cut:]
		limit = 74 // the leading space counts
	}
	w.WriteString(s)
	w.WriteString("\r\n")
}
