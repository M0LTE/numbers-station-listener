// Package schedule turns Priyom's calendar feed into scheduled
// transmissions: it parses the free-text summaries, maps Priyom's mode names
// onto receiver modes, derives status, polls the feed and writes iCal.
//
// Priyom content is CC BY-NC-SA 4.0; callers must keep attribution.
package schedule

import (
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Parsed is the result of parsing one Priyom summary such as
// "V13 15388kHz USB/AM [Target: East Asia]".
type Parsed struct {
	// OK is false when the summary did not fit the expected shape. Raw and
	// (when there was one) Station are still set; nothing else is.
	OK bool
	// Raw is the summary exactly as given.
	Raw string
	// Station is the designator as written, variant suffix kept ("F03j").
	Station string
	// Freqs are in Hz, in the order written, duplicates removed.
	Freqs []int64
	// Mode is Priyom's mode string verbatim ("USB/AM", "RTTY", "FSK 200/1000").
	// It can be empty when an entry lists frequencies and nothing else.
	Mode string
	// Search marks an entry with no frequency ("XPA2 Search").
	Search bool
	// Remarks are the bracketed and parenthesised notes, brackets stripped,
	// plus any loose trailing text, in the order they appear.
	Remarks []string
	// Target is the value of a "Target: X" remark, empty if there is none.
	Target string
}

// maxHz bounds a believable frequency (300 GHz); anything outside (0, maxHz]
// makes the entry unparsed rather than producing a nonsense number.
const maxHz = 300_000_000_000

// ParseSummary parses a Priyom summary. It never fails and never panics: a
// summary it cannot make sense of comes back with OK false.
//
// Accepted shape, all parts forgiving about spacing and case:
//
//	STATION (Search | FREQ[, FREQ...] [MODE]) [remarks...]
//
// where FREQ is a number with an optional decimal part and an optional unit
// (kHz, MHz or Hz; a unit applies to the unitless numbers before it, and a
// list with no unit at all is taken as kHz), separators are ",", "/", ";" or
// "&", and remarks are "[...]" or "(...)" groups anywhere after the head.
func ParseSummary(s string) Parsed {
	p := Parsed{Raw: s}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	segs := splitSegments(s)

	// The head is the plain text before the first bracket group; every later
	// segment is a remark.
	head := ""
	var remarks []string
	for i, sg := range segs {
		if i == 0 && !sg.bracketed {
			head = sg.text
			continue
		}
		if t := strings.TrimSpace(sg.text); t != "" {
			remarks = append(remarks, t)
		}
	}

	rest := strings.TrimLeftFunc(head, unicode.IsSpace)
	field, rest := nextField(rest)
	station := strings.TrimRight(field, ":,;")
	if !validStation(station) {
		return p
	}
	p.Station = station

	rest = strings.TrimSpace(rest)
	if rest == "" {
		return p
	}

	word, after := nextField(rest)
	if w := strings.TrimRight(word, ":"); strings.EqualFold(w, "search") {
		p.Search = true
		if t := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(after), ":")); t != "" {
			remarks = append([]string{t}, remarks...)
		}
		p.finish(remarks)
		return p
	}

	freqs, mode, ok := parseFreqList(rest)
	if !ok {
		return p
	}
	p.Freqs = freqs
	p.Mode = mode
	p.finish(remarks)
	return p
}

func (p *Parsed) finish(remarks []string) {
	p.OK = true
	p.Remarks = remarks
	for _, r := range remarks {
		if v, ok := targetOf(r); ok {
			p.Target = v
			break
		}
	}
}

// targetOf recognises "Target: X" (any case, any spacing around the colon).
func targetOf(r string) (string, bool) {
	const key = "target"
	if len(r) < len(key) || !strings.EqualFold(r[:len(key)], key) {
		return "", false
	}
	rest := strings.TrimLeftFunc(r[len(key):], unicode.IsSpace)
	if !strings.HasPrefix(rest, ":") {
		return "", false
	}
	v := strings.TrimSpace(rest[1:])
	return v, v != ""
}

func validStation(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r < utf8.RuneSelf && (r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r))) {
			return false
		}
	}
	return true
}

// nextField splits off the first whitespace-delimited field.
func nextField(s string) (field, rest string) {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	i := strings.IndexFunc(s, unicode.IsSpace)
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

type segment struct {
	text      string
	bracketed bool
}

// splitSegments cuts s into plain runs and bracket groups, in order. Groups
// may nest ("[a (b)]" is one group, "a (b)"), an unclosed group runs to the
// end, and a stray closer is kept as plain text.
func splitSegments(s string) []segment {
	var out []segment
	plainStart := 0
	depth, groupStart := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[', '(':
			if depth == 0 {
				if i > plainStart {
					out = append(out, segment{text: s[plainStart:i]})
				}
				groupStart = i + 1
			}
			depth++
		case ']', ')':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 {
				out = append(out, segment{text: s[groupStart:i], bracketed: true})
				plainStart = i + 1
			}
		}
	}
	if depth > 0 {
		out = append(out, segment{text: s[groupStart:], bracketed: true})
	} else if plainStart < len(s) {
		out = append(out, segment{text: s[plainStart:]})
	}
	return out
}

// parseFreqList reads "5422kHz, 4462.5 kHz USB" from the front of s and
// returns the frequencies in Hz and whatever follows (the mode, trimmed).
func parseFreqList(s string) (freqs []int64, mode string, ok bool) {
	type num struct {
		val  float64
		unit float64 // 0 until a unit is seen
	}
	var nums []num
	pos := 0
	for {
		v, n := readNumber(s[pos:])
		if n == 0 {
			break
		}
		pos += n
		nums = append(nums, num{val: v})
		pos = skipSpace(s, pos)
		if mult, ulen := readUnit(s[pos:]); ulen > 0 {
			for i := len(nums) - 1; i >= 0 && nums[i].unit == 0; i-- {
				nums[i].unit = mult
			}
			pos = skipSpace(s, pos+ulen)
		}
		// A separator only counts if another number follows it.
		if pos < len(s) && strings.IndexByte(",/;&", s[pos]) >= 0 {
			next := skipSpace(s, pos+1)
			if _, n := readNumber(s[next:]); n > 0 {
				pos = next
				continue
			}
		}
		break
	}
	if len(nums) == 0 {
		return nil, "", false
	}
	seen := map[int64]bool{}
	for _, n := range nums {
		unit := n.unit
		if unit == 0 {
			unit = 1000 // Priyom always means kHz
		}
		hz := n.val * unit
		if math.IsNaN(hz) || hz <= 0 || hz > maxHz {
			return nil, "", false
		}
		h := int64(math.Round(hz))
		if h <= 0 {
			return nil, "", false
		}
		if !seen[h] {
			seen[h] = true
			freqs = append(freqs, h)
		}
	}
	return freqs, strings.TrimSpace(s[pos:]), true
}

func skipSpace(s string, i int) int {
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !unicode.IsSpace(r) {
			break
		}
		i += size
	}
	return i
}

// readNumber reads digits with an optional "." fraction. It returns the
// number of bytes consumed, 0 if s does not start with a digit.
func readNumber(s string) (float64, int) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i > 15 {
		return 0, 0
	}
	if i+1 < len(s) && s[i] == '.' && s[i+1] >= '0' && s[i+1] <= '9' {
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j-i > 10 {
			return 0, 0
		}
		i = j
	}
	v, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, 0
	}
	return v, i
}

// readUnit recognises kHz, MHz or Hz (any case) at the start of s.
func readUnit(s string) (mult float64, n int) {
	for _, u := range []struct {
		name string
		mult float64
	}{{"khz", 1e3}, {"mhz", 1e6}, {"hz", 1}} {
		if len(s) >= len(u.name) && strings.EqualFold(s[:len(u.name)], u.name) {
			return u.mult, len(u.name)
		}
	}
	return 0, 0
}
