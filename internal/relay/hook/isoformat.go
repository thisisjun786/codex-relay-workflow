package hook

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// fromISOFormat is datetime.datetime.fromisoformat as CPython 3.13's C module parses a str
// (Modules/_datetimemodule.c datetime_fromisoformat), the interpreter the retained fence's
// control.py serves under: a surrogate at character 7, 8 or 10 stands for 'T' and any other
// fails, the separator is found by bytes 4, 5, 8 and 10 and may be any character, the time runs
// up to the first 'Z', '+' or '-', a fraction keeps six digits, a zero offset is UTC whatever
// its fraction, and the fields are range-checked with 3.13's words (an hour of 24 is out of
// range). at is the moment in UTC; aware is false for a value with no offset. err is the
// ValueError's message. A lone surrogate reaches here in its WTF-8 form.
func fromISOFormat(text string) (at time.Time, aware bool, err error) {
	invalid := isoError("Invalid isoformat string: %s", pyvalue.StrRepr(text))
	if characters(text) < 7 {
		return time.Time{}, false, invalid
	}
	// _sanitize_isoformat_str: only the separator may be a surrogate, and only the first found.
	for _, pos := range []int{7, 8, 10} {
		if pos > characters(text) {
			break
		}
		if at := byteOfCharacter(text, pos); surrogateAt(text, at) {
			text = text[:at] + "T" + text[at+3:]
			break
		}
	}
	for i := 0; i < len(text); i++ {
		if surrogateAt(text, i) {
			return time.Time{}, false, invalid // PyUnicode_AsUTF8AndSize refuses it
		}
	}
	raw := []byte(text)
	// c is the C string's byte at i, with the terminating NUL at and past the end.
	c := func(i int) byte {
		if i < 0 || i >= len(raw) {
			return 0
		}
		return raw[i]
	}
	separator := isoSeparator(len(raw), c)
	if separator < 0 {
		return time.Time{}, false, invalid
	}
	year, month, day, rv := isoDate(separator, c)
	var hour, minute, second, micro, tzOffset, tzMicro int
	if rv == 0 && len(raw) > separator {
		p := separator
		switch lead := c(p); {
		case lead&0x80 == 0:
			p++
		case lead&0xf0 == 0xe0:
			p += 3
		case lead&0xf0 == 0xf0:
			p += 4
		default:
			p += 2
		}
		rv = isoTime(p, len(raw), c, &hour, &minute, &second, &micro, &tzOffset, &tzMicro)
	}
	if rv < 0 {
		return time.Time{}, false, invalid
	}
	// tzinfo_from_isoformat_results, then new_timezone's range check, before the fields'.
	offset := time.Duration(0)
	if rv == 1 && tzOffset != 0 {
		offset = time.Duration(tzOffset)*time.Second + time.Duration(tzMicro)*time.Microsecond
		if offset <= -24*time.Hour || offset >= 24*time.Hour {
			return time.Time{}, false, isoError("offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24), not %s.", timedeltaRepr(offset))
		}
	}
	// check_date_args and check_time_args.
	switch {
	case year < 1 || year > 9999:
		return time.Time{}, false, isoError("year %d is out of range", year)
	case month < 1 || month > 12:
		return time.Time{}, false, isoError("month must be in 1..12")
	case day < 1 || day > daysIn(year, month):
		return time.Time{}, false, isoError("day is out of range for month")
	case hour > 23:
		return time.Time{}, false, isoError("hour must be in 0..23")
	case minute > 59:
		return time.Time{}, false, isoError("minute must be in 0..59")
	case second > 59:
		return time.Time{}, false, isoError("second must be in 0..59")
	}
	local := time.Date(year, time.Month(month), day, hour, minute, second, micro*1000, time.UTC)
	return local.Add(-offset), rv == 1, nil
}

func isoError(format string, args ...any) error { return fmt.Errorf(format, args...) }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// digits is parse_digits: n ASCII digits from i, or ok false.
func digits(c func(int) byte, i, n int) (value, next int, ok bool) {
	for k := 0; k < n; k++ {
		if !isDigit(c(i + k)) {
			return 0, 0, false
		}
		value = value*10 + int(c(i+k)-'0')
	}
	return value, i + n, true
}

// isoSeparator is _find_isoformat_datetime_separator over the UTF-8 bytes.
func isoSeparator(n int, c func(int) byte) int {
	if n == 7 {
		return 7
	}
	if c(4) == '-' {
		if c(5) != 'W' {
			return 10
		}
		if n < 8 {
			return -1
		}
		if n > 8 && c(8) == '-' {
			if n == 9 {
				return -1
			}
			if n > 10 && isDigit(c(10)) {
				return 8
			}
			return 10
		}
		return 8
	}
	if c(4) == 'W' {
		idx := 7
		for idx < n && isDigit(c(idx)) {
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

// isoDate is parse_isoformat_date over the bytes before length: 0, or a negative code.
func isoDate(length int, c func(int) byte) (year, month, day, rv int) {
	year, p, ok := digits(c, 0, 4)
	if !ok {
		return 0, 0, 0, -1
	}
	separated := c(p) == '-'
	if separated {
		p++
	}
	if c(p) == 'W' {
		p++
		week, next, ok := digits(c, p, 2)
		if !ok {
			return 0, 0, 0, -3
		}
		p = next
		weekday := 1
		if p < length {
			if separated {
				if c(p) != '-' {
					return 0, 0, 0, -2
				}
				p++
			}
			if weekday, _, ok = digits(c, p, 1); !ok {
				return 0, 0, 0, -4
			}
		}
		y, m, d, ok := isoWeekDate(year, week, weekday)
		if !ok {
			return 0, 0, 0, -5
		}
		return y, m, d, 0
	}
	month, p, ok = digits(c, p, 2)
	if !ok {
		return 0, 0, 0, -1
	}
	if separated {
		if c(p) != '-' {
			return 0, 0, 0, -2
		}
		p++
	}
	if day, _, ok = digits(c, p, 2); !ok {
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
		first := (int(time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Weekday()) + 6) % 7 // Monday is 0
		leap := year%4 == 0 && (year%100 != 0 || year%400 == 0)
		if week != 53 || !(first == 3 || (first == 2 && leap)) {
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

// hhmmssff is 3.13's parse_hh_mm_ss_ff over [p, end): 0 at the end of the string, 1 when
// something follows, or a negative code. A fraction may follow any field, and a third ':' is
// read as the fraction's separator.
func hhmmssff(p, end int, c func(int) byte, hour, minute, second, micro *int) int {
	*hour, *minute, *second, *micro = 0, 0, 0, 0
	values := []*int{hour, minute, second}
	separated := true
fields:
	for i := 0; i < 3; i++ {
		v, next, ok := digits(c, p, 2)
		if !ok {
			return -3
		}
		*values[i] = v
		p = next
		ch := c(p)
		p++
		if i == 0 {
			separated = ch == ':'
		}
		switch {
		case p >= end:
			if ch != 0 {
				return 1
			}
			return 0
		case separated && ch == ':':
			continue
		case ch == '.' || ch == ',':
			break fields
		case !separated:
			p--
		default:
			return -4
		}
	}
	toParse := min(end-p, 6)
	v, next, ok := digits(c, p, toParse)
	if !ok {
		return -3
	}
	p = next
	*micro = v
	for k := toParse; k < 6; k++ {
		*micro *= 10
	}
	for isDigit(c(p)) {
		p++
	}
	if c(p) != 0 {
		return 1
	}
	return 0
}

// isoTime is parse_isoformat_time over [p, end): 0 without an offset, 1 with one, or negative.
func isoTime(p, end int, c func(int) byte, hour, minute, second, micro, tzOffset, tzMicro *int) int {
	tz := p
	for {
		if ch := c(tz); ch == 'Z' || ch == '+' || ch == '-' {
			break
		}
		tz++
		if tz >= end {
			break
		}
	}
	rv := hhmmssff(p, tz, c, hour, minute, second, micro)
	if rv < 0 {
		return rv
	}
	if tz == end {
		if rv == 1 {
			return -5
		}
		return 0
	}
	if c(tz) == 'Z' {
		*tzOffset, *tzMicro = 0, 0
		if c(tz+1) != 0 {
			return -5
		}
		return 1
	}
	sign := 1
	if c(tz) == '-' {
		sign = -1
	}
	var h, m, s int
	rv = hhmmssff(tz+1, end, c, &h, &m, &s, tzMicro)
	*tzOffset = sign * (h*3600 + m*60 + s)
	*tzMicro *= sign
	if rv != 0 {
		return -5
	}
	return 1
}

func daysIn(year, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// timedeltaRepr is repr(datetime.timedelta) of a normalized duration.
func timedeltaRepr(d time.Duration) string {
	total := d.Microseconds()
	const day = 86400 * 1_000_000
	days, rest := total/day, total%day
	if rest < 0 {
		rest += day
		days--
	}
	var parts []string
	if days != 0 {
		parts = append(parts, fmt.Sprintf("days=%d", days))
	}
	if seconds := rest / 1_000_000; seconds != 0 {
		parts = append(parts, fmt.Sprintf("seconds=%d", seconds))
	}
	if micros := rest % 1_000_000; micros != 0 {
		parts = append(parts, fmt.Sprintf("microseconds=%d", micros))
	}
	if len(parts) == 0 {
		parts = []string{"0"}
	}
	return "datetime.timedelta(" + strings.Join(parts, ", ") + ")"
}

// surrogateAt is whether s holds a lone surrogate's WTF-8 bytes (ED A0..BF 80..BF) at byte i.
func surrogateAt(s string, i int) bool {
	return i >= 0 && i+3 <= len(s) && s[i] == 0xed && s[i+1] >= 0xa0 && s[i+1] <= 0xbf && s[i+2] >= 0x80 && s[i+2] <= 0xbf
}

// characters is the number of Python characters in s, a WTF-8 surrogate counting as one.
func characters(s string) int {
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

// byteOfCharacter is where Python character n of s starts, a WTF-8 surrogate counting as one.
func byteOfCharacter(s string, n int) int {
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
