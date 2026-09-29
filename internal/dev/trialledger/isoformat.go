//go:build dev

package trialledger

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// fromISOFormat is datetime.datetime.fromisoformat as CPython 3.14's C module parses it (the
// interpreter every Python-era ledger on the relay host was graded with): the separator is found
// by characters 4, 5, 8 and 10, the time runs up to the first 'Z', '+' or '-', a fraction keeps
// six digits, and hour 24 is the next midnight. aware is false for a time with no offset. err
// is the ValueError's text.
func fromISOFormat(text string) (at time.Time, aware bool, err error) {
	invalid := newValueError("Invalid isoformat string: %s", pyRepr(text))
	length := byteOfRune(text, -1)
	if length < 7 {
		return time.Time{}, false, invalid
	}
	// A surrogate is allowed only as the separator, where it stands for 'T'.
	for _, pos := range []int{7, 8, 10} {
		if pos >= length {
			break
		}
		if surrogate(text, pos) {
			text = replaceRune(text, pos, 'T')
			break
		}
	}
	if hasSurrogate(text) {
		return time.Time{}, false, invalid
	}
	raw := []byte(text)
	// at is the C string's byte, with the terminating NUL past the end.
	at_ := func(i int) byte {
		if i < 0 || i >= len(raw) {
			return 0
		}
		return raw[i]
	}
	separator := isoSeparator(raw, at_)
	if separator < 0 {
		return time.Time{}, false, invalid
	}
	year, month, day, rv := isoDate(raw, separator, at_)
	var hour, minute, second, micro, tzOffset, tzMicro int
	if rv == 0 && len(raw) > separator {
		p := separator
		switch lead := at_(p); {
		case lead&0x80 == 0:
			p++
		case lead&0xf0 == 0xe0:
			p += 3
		case lead&0xf0 == 0xf0:
			p += 4
		default:
			p += 2
		}
		rv = isoTime(p, len(raw), at_, &hour, &minute, &second, &micro, &tzOffset, &tzMicro)
	}
	if rv < 0 {
		return time.Time{}, false, invalid
	}
	offset := time.Duration(0)
	if rv == 1 && tzOffset != 0 {
		offset = time.Duration(tzOffset)*time.Second + time.Duration(tzMicro)*time.Microsecond
		if offset <= -24*time.Hour || offset >= 24*time.Hour {
			return time.Time{}, false, newValueError("offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24), not %s", timedeltaRepr(offset))
		}
	}
	if hour == 24 && month <= 12 {
		if dim := daysIn(year, month); day <= dim {
			if minute != 0 || second != 0 || micro != 0 {
				return time.Time{}, false, newValueError("minute, second, and microsecond must be 0 when hour is 24")
			}
			hour, day = 0, day+1
			if day > dim {
				day, month = 1, month+1
				if month > 12 {
					month, year = 1, year+1
				}
			}
		}
	}
	switch {
	case year < 1 || year > 9999:
		return time.Time{}, false, newValueError("year must be in 1..9999, not %d", year)
	case month < 1 || month > 12:
		return time.Time{}, false, newValueError("month must be in 1..12, not %d", month)
	case day < 1 || day > daysIn(year, month):
		return time.Time{}, false, newValueError("day %d must be in range 1..%d for month %d in year %d", day, daysIn(year, month), month, year)
	case hour > 23:
		return time.Time{}, false, newValueError("hour must be in 0..23, not %d", hour)
	case minute > 59:
		return time.Time{}, false, newValueError("minute must be in 0..59, not %d", minute)
	case second > 59:
		return time.Time{}, false, newValueError("second must be in 0..59, not %d", second)
	}
	local := time.Date(year, time.Month(month), day, hour, minute, second, micro*1000, time.UTC)
	return local.Add(-offset), rv == 1, nil
}

// valueError is a ValueError's text, spelled as Python spells it.
type valueError string

func (e valueError) Error() string { return string(e) }

func newValueError(format string, args ...any) error { return valueError(fmt.Sprintf(format, args...)) }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// digits is parse_digits: n ASCII digits from i, or ok false.
func digits(at func(int) byte, i, n int) (value, next int, ok bool) {
	for k := 0; k < n; k++ {
		c := at(i + k)
		if !isDigit(c) {
			return 0, 0, false
		}
		value = value*10 + int(c-'0')
	}
	return value, i + n, true
}

// isoSeparator is _find_isoformat_datetime_separator.
func isoSeparator(raw []byte, at func(int) byte) int {
	n := len(raw)
	if n == 7 {
		return 7
	}
	if at(4) == '-' {
		if at(5) != 'W' {
			return 10
		}
		if n < 8 {
			return -1
		}
		if n > 8 && at(8) == '-' {
			if n == 9 {
				return -1
			}
			if n > 10 && isDigit(at(10)) {
				return 8
			}
			return 10
		}
		return 8
	}
	if at(4) == 'W' {
		idx := 7
		for idx < n && isDigit(at(idx)) {
			idx++
		}
		if idx < 9 {
			return idx
		}
		if idx%2 == 0 {
			return 7
		}
		return 8
	}
	return 8
}

// isoDate is parse_isoformat_date: 0, or a negative code.
func isoDate(raw []byte, length int, at func(int) byte) (year, month, day, rv int) {
	year, p, ok := digits(at, 0, 4)
	if !ok {
		return 0, 0, 0, -1
	}
	separated := at(p) == '-'
	if separated {
		p++
	}
	if at(p) == 'W' {
		p++
		week, next, ok := digits(at, p, 2)
		if !ok {
			return 0, 0, 0, -3
		}
		p = next
		weekday := 1
		if p < length {
			if separated {
				if at(p) != '-' {
					return 0, 0, 0, -2
				}
				p++
			}
			if weekday, _, ok = digits(at, p, 1); !ok {
				return 0, 0, 0, -4
			}
		}
		y, m, d, ok := isoWeekDate(year, week, weekday)
		if !ok {
			return 0, 0, 0, -5
		}
		return y, m, d, 0
	}
	month, p, ok = digits(at, p, 2)
	if !ok {
		return 0, 0, 0, -1
	}
	if separated {
		if at(p) != '-' {
			return 0, 0, 0, -2
		}
		p++
	}
	if day, _, ok = digits(at, p, 2); !ok {
		return 0, 0, 0, -1
	}
	return year, month, day, 0
}

// isoWeekDate is iso_to_ymd.
func isoWeekDate(year, week, weekday int) (int, int, int, bool) {
	if year < 1 || year > 9999 {
		return 0, 0, 0, false
	}
	if week <= 0 || week >= 53 {
		first := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Weekday() // Sunday is 0
		mondayBased := (int(first) + 6) % 7
		leap := year%4 == 0 && (year%100 != 0 || year%400 == 0)
		if week != 53 || !(mondayBased == 3 || (mondayBased == 2 && leap)) {
			return 0, 0, 0, false
		}
	}
	if weekday <= 0 || weekday >= 8 {
		return 0, 0, 0, false
	}
	fourth := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
	monday := fourth.AddDate(0, 0, -((int(fourth.Weekday()) + 6) % 7))
	d := monday.AddDate(0, 0, (week-1)*7+weekday-1)
	return d.Year(), int(d.Month()), d.Day(), true
}

// hhmmssff is parse_hh_mm_ss_ff over [p, end): 0 at the end of the string, 1 when something
// follows, or a negative code.
func hhmmssff(p, end int, at func(int) byte, hour, minute, second, micro *int) int {
	*hour, *minute, *second, *micro = 0, 0, 0, 0
	values := []*int{hour, minute, second}
	separated := true
	for i := 0; i < 3; i++ {
		v, next, ok := digits(at, p, 2)
		if !ok {
			return -3
		}
		*values[i] = v
		p = next
		c := at(p)
		p++
		if i == 0 {
			separated = c == ':'
		}
		switch {
		case p >= end:
			if c != 0 {
				return 1
			}
			return 0
		case separated && c == ':':
			if i == 2 {
				return -4
			}
			continue
		case c == '.' || c == ',':
			if i < 2 {
				return -3
			}
		case !separated:
			p--
			continue
		default:
			return -4
		}
		break
	}
	toParse := min(end-p, 6)
	v, next, ok := digits(at, p, toParse)
	if !ok {
		return -3
	}
	p = next
	*micro = v
	for k := toParse; k > 0 && k < 6; k++ {
		*micro *= 10
	}
	for isDigit(at(p)) {
		p++
	}
	if at(p) != 0 {
		return 1
	}
	return 0
}

// isoTime is parse_isoformat_time over [p, end): 0 without an offset, 1 with one, or negative.
func isoTime(p, end int, at func(int) byte, hour, minute, second, micro, tzOffset, tzMicro *int) int {
	tz := p
	for {
		if c := at(tz); c == 'Z' || c == '+' || c == '-' {
			break
		}
		tz++
		if tz >= end {
			break
		}
	}
	rv := hhmmssff(p, tz, at, hour, minute, second, micro)
	if rv < 0 {
		return rv
	}
	if tz == end {
		if rv == 1 {
			return -5
		}
		return 0
	}
	if at(tz) == 'Z' {
		*tzOffset, *tzMicro = 0, 0
		if at(tz+1) != 0 {
			return -5
		}
		return 1
	}
	sign := 1
	if at(tz) == '-' {
		sign = -1
	}
	var h, m, s int
	rv = hhmmssff(tz+1, end, at, &h, &m, &s, tzMicro)
	*tzOffset = sign * (h*3600 + m*60 + s)
	*tzMicro *= sign
	if rv != 0 {
		return -5
	}
	return 1
}

func daysIn(year, month int) int {
	if month < 1 || month > 12 {
		return 0
	}
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// timedeltaRepr is repr(datetime.timedelta) of a normalized duration.
func timedeltaRepr(d time.Duration) string {
	total := d.Microseconds()
	const day = 86400 * 1_000_000
	days := total / day
	rest := total % day
	if rest < 0 {
		rest += day
		days--
	}
	seconds, micros := rest/1_000_000, rest%1_000_000
	var parts []string
	if days != 0 {
		parts = append(parts, fmt.Sprintf("days=%d", days))
	}
	if seconds != 0 {
		parts = append(parts, fmt.Sprintf("seconds=%d", seconds))
	}
	if micros != 0 {
		parts = append(parts, fmt.Sprintf("microseconds=%d", micros))
	}
	if len(parts) == 0 {
		parts = []string{"0"}
	}
	return "datetime.timedelta(" + strings.Join(parts, ", ") + ")"
}

// pyRepr is repr() of a str: a quote the text does not hold, and every character str.isprintable
// refuses escaped, a lone surrogate among them.
func pyRepr(s string) string {
	quote := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = `"`
	}
	var b strings.Builder
	b.WriteString(quote)
	for i := 0; i < len(s); {
		if surrogateAt(s, i) {
			fmt.Fprintf(&b, `\u%04x`, rune(s[i]&0x0f)<<12|rune(s[i+1]&0x3f)<<6|rune(s[i+2]&0x3f))
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case string(r) == quote:
			b.WriteString(`\` + quote)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == ' ' || unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteString(quote)
	return b.String()
}

// A Python str holding a lone surrogate reaches Go as WTF-8 (ED A0..BF 80..BF).
func surrogateAt(s string, i int) bool {
	return i+3 <= len(s) && s[i] == 0xed && s[i+1] >= 0xa0 && s[i+1] <= 0xbf && s[i+2] >= 0x80 && s[i+2] <= 0xbf
}

func hasSurrogate(s string) bool {
	for i := 0; i < len(s); i++ {
		if surrogateAt(s, i) {
			return true
		}
	}
	return false
}

// byteOfRune is where code point n starts, counting a WTF-8 surrogate as one code point; with n
// negative it is the number of code points.
func byteOfRune(s string, n int) int {
	if n < 0 {
		count := 0
		for i := 0; i < len(s); count++ {
			if surrogateAt(s, i) {
				i += 3
				continue
			}
			_, size := utf8.DecodeRuneInString(s[i:])
			i += size
		}
		return count
	}
	i := 0
	for ; n > 0 && i < len(s); n-- {
		if surrogateAt(s, i) {
			i += 3
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return i
}

func surrogate(s string, pos int) bool { return surrogateAt(s, byteOfRune(s, pos)) }

func replaceRune(s string, pos int, r rune) string {
	i := byteOfRune(s, pos)
	return s[:i] + string(r) + s[i+3:]
}
