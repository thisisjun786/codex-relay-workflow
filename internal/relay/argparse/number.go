package argparse

import (
	"errors"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// ParseInt and ParseFloat read a number's text as Python's int() and float() do (Unicode
// digits, underscores, surrounding whitespace, int()'s 4300-digit limit), for the packages that
// still read stored or forge text that way. The command-line parser does not use them: an
// option's number is read Go's way (argparse.go).
var intSyntax = regexp.MustCompile(`^[+-]?[0-9](?:_?[0-9])*$`)
var floatSyntax = regexp.MustCompile(`^[+-]?(?:(?:[0-9](?:_?[0-9])*(?:\.(?:[0-9](?:_?[0-9])*)?)?|\.[0-9](?:_?[0-9])*)(?:[eE][+-]?[0-9](?:_?[0-9])*)?|(?i:inf(?:inity)?|nan))$`)

func decimalText(s string) string {
	// PyUnicode_TransformDecimalAndSpaceToASCII leaves ASCII controls alone.
	return strings.Map(func(r rune) rune {
		if r < 128 {
			return r
		}
		if unicode.IsSpace(r) {
			return ' '
		}
		for _, rr := range unicode.Nd.R16 {
			if r >= rune(rr.Lo) && r <= rune(rr.Hi) && (r-rune(rr.Lo))%rune(rr.Stride) == 0 {
				return '0' + (r-rune(rr.Lo))/rune(rr.Stride)%10
			}
		}
		for _, rr := range unicode.Nd.R32 {
			if r >= rune(rr.Lo) && r <= rune(rr.Hi) && (r-rune(rr.Lo))%rune(rr.Stride) == 0 {
				return '0' + (r-rune(rr.Lo))/rune(rr.Stride)%10
			}
		}
		return r
	}, s)
}

func ParseInt(s string) (*big.Int, bool) {
	s = strings.Trim(decimalText(s), " \t\n\r\v\f")
	if !intSyntax.MatchString(s) {
		return nil, false
	}
	s = strings.ReplaceAll(s, "_", "")
	if len(strings.TrimLeft(s, "+-")) > 4300 {
		return nil, false
	}
	return new(big.Int).SetString(s, 10)
}

func ParseFloat(s string) (float64, bool) {
	s = strings.Trim(decimalText(s), " \t\n\r\v\f")
	if !floatSyntax.MatchString(s) {
		return 0, false
	}
	s = strings.ReplaceAll(s, "_", "")
	if strings.EqualFold(strings.TrimLeft(s, "+-"), "nan") {
		return math.NaN(), true
	}
	n, err := strconv.ParseFloat(s, 64)
	return n, err == nil || errors.Is(err, strconv.ErrRange)
}

// NumberText is the canonical spelling a parsed number's text is kept in (Result.Values); the
// typed value is in Result.Numbers.
func NumberText(v any) string {
	if n, ok := v.(*big.Int); ok {
		return n.String()
	}
	return strconv.FormatFloat(v.(float64), 'g', -1, 64)
}
