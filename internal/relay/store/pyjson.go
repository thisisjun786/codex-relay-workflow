package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf16"
)

// jsonValue is a decoded JSON document that keeps object key order, as a Python dict does.
type jsonValue struct {
	object []jsonField // non-nil (possibly empty) for an object
	array  []jsonValue
	scalar any // string, bool, nil, or json.Number
	kind   byte
}

type jsonField struct {
	key   string
	value jsonValue
}

const (
	jsonObject byte = iota + 1
	jsonArray
	jsonScalar
)

var errTrailingJSON = errors.New("trailing data after JSON value")

func decodeOrdered(data []byte) (jsonValue, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeValue(decoder)
	if err != nil {
		return jsonValue{}, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return jsonValue{}, errTrailingJSON
	}
	return value, nil
}

func decodeValue(decoder *json.Decoder) (jsonValue, error) {
	token, err := decoder.Token()
	if err != nil {
		return jsonValue{}, fmt.Errorf("decode JSON: %w", err)
	}
	switch delim := token.(type) {
	case json.Delim:
		if delim == '[' {
			array := []jsonValue{}
			for decoder.More() {
				item, err := decodeValue(decoder)
				if err != nil {
					return jsonValue{}, err
				}
				array = append(array, item)
			}
			_, err := decoder.Token()
			return jsonValue{kind: jsonArray, array: array}, err
		}
		object := []jsonField{}
		positions := map[string]int{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return jsonValue{}, fmt.Errorf("decode key: %w", err)
			}
			key, _ := keyToken.(string)
			item, err := decodeValue(decoder)
			if err != nil {
				return jsonValue{}, err
			}
			// A repeated key keeps its first position and its last value, as json.loads does.
			if at, seen := positions[key]; seen {
				object[at].value = item
				continue
			}
			positions[key] = len(object)
			object = append(object, jsonField{key: key, value: item})
		}
		_, err := decoder.Token()
		return jsonValue{kind: jsonObject, object: object}, err
	default:
		return jsonValue{kind: jsonScalar, scalar: token}, nil
	}
}

func (v jsonValue) field(key string) (jsonValue, bool) {
	for _, f := range v.object {
		if f.key == key {
			return f.value, true
		}
	}
	return jsonValue{}, false
}

func (v jsonValue) isNull() bool { return v.kind == jsonScalar && v.scalar == nil }

func (v jsonValue) text() (string, bool) {
	s, ok := v.scalar.(string)
	return s, v.kind == jsonScalar && ok
}

// integer answers Python's isinstance(x, int) and not bool: a JSON number with no fraction.
func (v jsonValue) integer() (int64, bool) {
	number, ok := v.scalar.(json.Number)
	if !ok || strings.ContainsAny(string(number), ".eE") {
		return 0, false
	}
	value, err := number.Int64()
	return value, err == nil
}

// pythonDumps renders v exactly as Python's json.dumps(v) with default arguments.
func pythonDumps(v jsonValue) (string, error) {
	var buf strings.Builder
	if err := appendPython(&buf, v); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func appendPython(buf *strings.Builder, v jsonValue) error {
	switch v.kind {
	case jsonObject:
		buf.WriteByte('{')
		for i, f := range v.object {
			if i > 0 {
				buf.WriteString(", ")
			}
			appendPythonString(buf, f.key)
			buf.WriteString(": ")
			if err := appendPython(buf, f.value); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case jsonArray:
		buf.WriteByte('[')
		for i, item := range v.array {
			if i > 0 {
				buf.WriteString(", ")
			}
			if err := appendPython(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		return appendPythonScalar(buf, v.scalar)
	}
	return nil
}

func appendPythonScalar(buf *strings.Builder, scalar any) error {
	switch s := scalar.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(s))
	case string:
		appendPythonString(buf, s)
	case json.Number:
		if !strings.ContainsAny(string(s), ".eE") {
			integer, ok := new(big.Int).SetString(string(s), 10)
			if !ok {
				return fmt.Errorf("integer %q", s)
			}
			buf.WriteString(integer.String())
			return nil
		}
		f, err := s.Float64()
		if err != nil && !math.IsInf(f, 0) {
			return fmt.Errorf("float %q: %w", s, err)
		}
		buf.WriteString(pythonFloat(f))
	default:
		return fmt.Errorf("unsupported JSON scalar %T", scalar)
	}
	return nil
}

// pythonFloat is float.__repr__ as json.dumps writes it.
func pythonFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
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
	sign := exponent[0]
	digits := strings.TrimLeft(exponent[1:], "0")
	if len(digits) < 2 {
		digits = strings.Repeat("0", 2-len(digits)) + digits
	}
	return mantissa + "e" + string(sign) + digits
}

func appendPythonString(buf *strings.Builder, value string) {
	buf.WriteByte('"')
	for _, r := range value {
		switch {
		case r == '"' || r == '\\':
			buf.WriteByte('\\')
			buf.WriteRune(r)
		case r == '\n':
			buf.WriteString(`\n`)
		case r == '\r':
			buf.WriteString(`\r`)
		case r == '\t':
			buf.WriteString(`\t`)
		case r == '\b':
			buf.WriteString(`\b`)
		case r == '\f':
			buf.WriteString(`\f`)
		case r < 0x20 || r > 0x7e:
			for _, unit := range utf16.Encode([]rune{r}) {
				fmt.Fprintf(buf, "\\u%04x", unit)
			}
		default:
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
}
