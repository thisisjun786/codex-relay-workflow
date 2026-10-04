package goalplan

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The JavaScript value rules the plan revival leans on. A plan reader decodes a stored plan with json.Decoder.UseNumber, so a
// number is a json.Number (a float64 from a caller that already has one is accepted too).

// quote is JSON.stringify of a string: the quote and backslash escaped, \b \f \n \r \t in their short forms, the other control
// characters as \u00xx, and everything else (U+2028, "<", ">" and "&" included) as it is. projectcfg keeps the same function; it
// is not exported and lies outside this package's edit region. A lone surrogate cannot reach a Go string (revive.go), so the
// escape JSON.stringify writes for one is not produced.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch short := strings.IndexRune("\b\f\n\r\t", r); {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case short >= 0:
			b.WriteByte('\\')
			b.WriteByte("bfnrt"[short])
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// jsNumber is a JavaScript number read from a decoded value. JSON.parse reads 1e999 as Infinity; strconv reports it as ErrRange
// with the infinity as the value, which is kept.
func jsNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(n), 64)
		return f, err == nil || errors.Is(err, strconv.ErrRange)
	case float64:
		return n, true
	}
	return 0, false
}

// isLifecycleID is LIFECYCLE_ID_RE, /^[a-z0-9][a-z0-9-]{0,39}$/ (goalplan.ts:1236, outside this unit's lines but tested by
// reviveDecisions): one to forty lowercase ASCII letters, digits or hyphens, the first not a hyphen.
func isLifecycleID(s string) bool {
	if len(s) == 0 || len(s) > 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i > 0 && c == '-') {
			return false
		}
	}
	return true
}

// validIsoTime is validIsoTime (goalplan.ts:522), new Date(value).toISOString() === value: the text is exactly what toISOString
// prints, YYYY-MM-DDTHH:mm:ss.sssZ for the year 0000 to 9999 and a sign with six digits for any other year ("-000000" is never
// printed, nor a sign on a year below 10000), naming a real moment within the range of a Date, +-8.64e15 ms. A day or an hour
// that does not exist would roll over, so it would print as another text.
func validIsoTime(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	var year int
	switch {
	case len(s) == 24:
		year, ok = digits(s[:4])
		s = s[4:]
	case len(s) == 27 && (s[0] == '+' || s[0] == '-'):
		year, ok = digits(s[1:7])
		ok = ok && (s[0] == '-' && year > 0 || s[0] == '+' && year > 9999)
		if s[0] == '-' {
			year = -year
		}
		s = s[7:]
	default:
		return false
	}
	if !ok || s[0] != '-' || s[3] != '-' || s[6] != 'T' || s[9] != ':' || s[12] != ':' || s[15] != '.' || s[19] != 'Z' {
		return false
	}
	var f [6]int // month, day, hour, minute, second, millisecond
	for i, at := range [...]int{1, 4, 7, 10, 13, 16} {
		if f[i], ok = digits(s[at : at+2+i/5]); !ok {
			return false
		}
	}
	if f[0] < 1 || f[0] > 12 || f[2] > 23 || f[3] > 59 || f[4] > 59 {
		return false
	}
	// A day that does not exist (0, or past the end of the month) rolls into another month, which is how the month check sees it.
	t := time.Date(year, time.Month(f[0]), f[1], f[2], f[3], f[4], f[5]*int(time.Millisecond), time.UTC)
	ms := t.UnixMilli()
	return int(t.Month()) == f[0] && ms >= -8.64e15 && ms <= 8.64e15
}

// digits is the number an all-ASCII-digit text spells.
func digits(s string) (n int, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}
