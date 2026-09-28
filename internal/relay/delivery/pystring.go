package delivery

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"unicode"
)

// pyStrip is str.strip(): Python includes the ASCII information separators in
// addition to Unicode White_Space. Ack validation uses this without changing
// the original string that enters the proof.
func pyStrip(value string) string {
	return strings.TrimFunc(value, func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f })
}

// sqliteIntString is Python int(str, 10), followed by sqlite3's signed-64-bit
// binding. int's parser does not accept the ASCII information separators that
// str.strip accepts, so its whitespace boundary is deliberately narrower.
func sqliteIntString(value string) (any, error) {
	text := strings.TrimSpace(value)
	invalid := func() (any, error) {
		return nil, &hostError{"ValueError", "invalid literal for int() with base 10: " + intLiteralRepr(value)}
	}
	var normalized strings.Builder
	if len(text) > 0 && (text[0] == '+' || text[0] == '-') {
		normalized.WriteByte(text[0])
		text = text[1:]
	}
	digits, previousDigit := 0, false
	for _, r := range text {
		if r == '_' && previousDigit {
			previousDigit = false
			continue
		}
		digit, ok := decimalDigit(r)
		if !ok {
			return invalid()
		}
		normalized.WriteByte('0' + digit)
		digits++
		previousDigit = true
	}
	if !previousDigit {
		return invalid()
	}
	if digits > 4300 {
		return nil, &hostError{"ValueError", "Exceeds the limit (4300 digits) for integer string conversion: value has " + strconv.Itoa(digits) + " digits; use sys.set_int_max_str_digits() to increase the limit"}
	}
	n, _ := new(big.Int).SetString(normalized.String(), 10)
	if !n.IsInt64() {
		return nil, &hostError{"OverflowError", "Python int too large to convert to SQLite INTEGER"}
	}
	return n.Int64(), nil
}

// repr also escapes non-printing Unicode (for example a zero-width space).
func intLiteralRepr(value string) string {
	var out strings.Builder
	for _, r := range pyReprValue(value) {
		switch {
		case unicode.IsPrint(r):
			out.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&out, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&out, `\u%04x`, r)
		default:
			fmt.Fprintf(&out, `\U%08x`, r)
		}
	}
	return out.String()
}

func decimalDigit(r rune) (byte, bool) {
	for _, span := range unicode.Digit.R16 {
		if uint32(r) >= uint32(span.Lo) && uint32(r) <= uint32(span.Hi) && (uint32(r)-uint32(span.Lo))%uint32(span.Stride) == 0 {
			return byte((uint32(r) - uint32(span.Lo)) / uint32(span.Stride) % 10), true
		}
	}
	for _, span := range unicode.Digit.R32 {
		if uint32(r) >= span.Lo && uint32(r) <= span.Hi && (uint32(r)-span.Lo)%span.Stride == 0 {
			return byte((uint32(r) - span.Lo) / span.Stride % 10), true
		}
	}
	return 0, false
}
