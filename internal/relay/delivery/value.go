package delivery

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// object is a decoded JSON object (a Python dict): key order kept, a repeated key keeping its
// first position and last value, as json.loads does.
type object []field

type field struct {
	key   string
	value any
}

func (o object) get(key string) any {
	for _, f := range o {
		if f.key == key {
			return f.value
		}
	}
	return nil
}

// decode is json.loads: nil for text that is not JSON. Integers stay json.Number, other
// numbers become float64, so str() can tell 1 from 1.0 as Python does.
func decode(text string) any {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	value, err := decodeValue(decoder)
	if err != nil {
		return nil
	}
	if decoder.More() {
		return nil
	}
	return value
}

func decodeValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch v := token.(type) {
	case json.Delim:
		if v == '[' {
			items := []any{}
			for decoder.More() {
				item, err := decodeValue(decoder)
				if err != nil {
					return nil, err
				}
				items = append(items, item)
			}
			_, err := decoder.Token()
			return items, err
		}
		out := object{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			item, err := decodeValue(decoder)
			if err != nil {
				return nil, err
			}
			name, _ := key.(string)
			replaced := false
			for i := range out {
				if out[i].key == name {
					out[i].value, replaced = item, true
				}
			}
			if !replaced {
				out = append(out, field{name, item})
			}
		}
		_, err := decoder.Token()
		return out, err
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			f, _ := strconv.ParseFloat(string(v), 64)
			return f, nil
		}
		return v, nil
	}
	return token, nil
}

// truthy is bool(value).
func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case json.Number:
		return strings.TrimLeft(string(v), "-0") != ""
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case object:
		return len(v) > 0
	}
	return true
}

// pyStr is str(value) for a decoded JSON value, as an f-string renders it.
func pyStr(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return pyRepr(value)
}

// pyRepr is repr(value) for a decoded JSON value.
func pyRepr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return reprString(v)
	case json.Number:
		return string(v) + ""
	case float64:
		return reprFloat(v)
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case object:
		parts := make([]string, len(v))
		for i, f := range v {
			parts[i] = reprString(f.key) + ": " + pyRepr(f.value)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return ""
}

// reprFloat is float.__repr__.
func reprFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	magnitude := math.Abs(f)
	if magnitude == 0 || magnitude >= 1e-4 && magnitude < 1e16 {
		text := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	text := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, exponent, _ := strings.Cut(text, "e")
	sign, digits := exponent[:1], strings.TrimLeft(exponent[1:], "0")
	if len(digits) < 2 {
		digits = strings.Repeat("0", 2-len(digits)) + digits
	}
	return mantissa + "e" + sign + digits
}

// reprString is repr() of a str: single quotes unless it holds one and no double quote.
func reprString(text string) string {
	quote := byte('\'')
	if strings.Contains(text, "'") && !strings.Contains(text, `"`) {
		quote = '"'
	}
	var b bytes.Buffer
	b.WriteByte(quote)
	for _, r := range text {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteByte(quote)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\x` + hex2(int(r)))
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

func hex2(n int) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[n>>4], digits[n&15]})
}
