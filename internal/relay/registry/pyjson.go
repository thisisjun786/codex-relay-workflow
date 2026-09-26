package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// Decoded JSON keeps Python's shapes: an object is a contract.OrderedObject (a dict keeps its
// insertion order), an integer stays a json.Number (Python int), a number with a fraction or an
// exponent is a float64 (Python float), and strings, booleans, nil and []any are themselves.

//lint:ignore ST1005 json/decoder.py:340 caller-visible message kept byte-identical to Python
var errTrailing = errors.New("Extra data")

// decodeJSON is json.loads for one document.
func decodeJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errTrailing
	}
	return value, nil
}

func decodeValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch t := token.(type) {
	case json.Delim:
		if t == '[' {
			list := []any{}
			for decoder.More() {
				item, err := decodeValue(decoder)
				if err != nil {
					return nil, err
				}
				list = append(list, item)
			}
			_, err := decoder.Token()
			return list, err
		}
		object := contract.OrderedObject{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			value, err := decodeValue(decoder)
			if err != nil {
				return nil, err
			}
			// A repeated key keeps its first position and takes the last value, as a dict does.
			object = setField(object, key.(string), value)
		}
		_, err := decoder.Token()
		return object, err
	case json.Number:
		if strings.ContainsAny(string(t), ".eE") {
			f, err := t.Float64()
			if err != nil && !math.IsInf(f, 0) {
				return nil, err
			}
			return f, nil
		}
		return t, nil
	default:
		return t, nil
	}
}

func setField(object contract.OrderedObject, key string, value any) contract.OrderedObject {
	for i := range object {
		if object[i].Key == key {
			object[i].Value = value
			return object
		}
	}
	return append(object, contract.Field{Key: key, Value: value})
}

func getField(object contract.OrderedObject, key string) (any, bool) {
	for _, field := range object {
		if field.Key == key {
			return field.Value, true
		}
	}
	return nil, false
}

func dropField(object contract.OrderedObject, key string) contract.OrderedObject {
	out := contract.OrderedObject{}
	for _, field := range object {
		if field.Key != key {
			out = append(out, field)
		}
	}
	return out
}

func copyObject(object contract.OrderedObject) contract.OrderedObject {
	return append(contract.OrderedObject{}, object...)
}

// pyDumps is json.dumps(value) with the default separators, ensure_ascii and optional sort_keys.
func pyDumps(value any, sortKeys bool) string {
	var b strings.Builder
	writeDumps(&b, value, sortKeys)
	return b.String()
}

func writeDumps(b *strings.Builder, value any, sortKeys bool) {
	switch v := value.(type) {
	case contract.OrderedObject:
		fields := v
		if sortKeys {
			fields = copyObject(v)
			slices.SortStableFunc(fields, func(x, y contract.Field) int { return strings.Compare(x.Key, y.Key) })
		}
		b.WriteByte('{')
		for i, field := range fields {
			if i > 0 {
				b.WriteString(", ")
			}
			writeJSONString(b, field.Key)
			b.WriteString(": ")
			writeDumps(b, field.Value, sortKeys)
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				b.WriteString(", ")
			}
			writeDumps(b, item, sortKeys)
		}
		b.WriteByte(']')
	case []string:
		items := make([]any, len(v))
		for i, s := range v {
			items[i] = s
		}
		writeDumps(b, items, sortKeys)
	case string:
		writeJSONString(b, v)
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case nil:
		b.WriteString("null")
	case json.Number:
		b.WriteString(v.String())
	case float64:
		switch {
		case math.IsNaN(v):
			b.WriteString("NaN")
		case math.IsInf(v, 1):
			b.WriteString("Infinity")
		case math.IsInf(v, -1):
			b.WriteString("-Infinity")
		default:
			b.WriteString(pyFloat(v))
		}
	case int:
		b.WriteString(strconv.Itoa(v))
	case int64:
		b.WriteString(strconv.FormatInt(v, 10))
	default:
		fmt.Fprintf(b, "%v", v)
	}
}

func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20 || r > 0x7e:
			if r > 0xffff {
				r -= 0x10000
				fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			} else {
				fmt.Fprintf(b, `\u%04x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// pyFloat is float.__repr__: fixed notation for exponents in [-4, 16), shortest round trip.
func pyFloat(v float64) string {
	if v == 0 {
		if math.Signbit(v) {
			return "-0.0"
		}
		return "0.0"
	}
	exponent := math.Floor(math.Log10(math.Abs(v)))
	shortest := strconv.FormatFloat(v, 'e', -1, 64)
	if mantissa, exp, found := strings.Cut(shortest, "e"); found {
		if n, err := strconv.Atoi(exp); err == nil {
			exponent = float64(n)
		}
		_ = mantissa
	}
	if exponent >= -4 && exponent < 16 {
		text := strconv.FormatFloat(v, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	return shortest
}

// canonical is settings._canonical: json.dumps(value, sort_keys=True), where 0 and false differ.
func canonical(value any) string { return pyDumps(value, true) }

// pyTypeName is type(value).__name__ for a decoded JSON value.
func pyTypeName(value any) string {
	switch value.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case json.Number, int, int64:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any, []string:
		return "list"
	case contract.OrderedObject:
		return "dict"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// pyRepr is repr() of a decoded JSON value.
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
		return pyStr(v)
	case json.Number:
		return v.String()
	case float64:
		switch {
		case math.IsNaN(v):
			return "nan"
		case math.IsInf(v, 1):
			return "inf"
		case math.IsInf(v, -1):
			return "-inf"
		}
		return pyFloat(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case []string:
		parts := make([]string, len(v))
		for i, s := range v {
			parts[i] = pyStr(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case contract.OrderedObject:
		parts := make([]string, len(v))
		for i, field := range v {
			parts[i] = pyStr(field.Key) + ": " + pyRepr(field.Value)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(v)
	}
}

// pyStr is repr() of a str.
func pyStr(s string) string {
	quote := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = `"`
	}
	var b strings.Builder
	b.WriteString(quote)
	for _, r := range s {
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
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(quote)
	return b.String()
}

// textList is settings._text_list: a list whose every member is text.
func textList(value any) bool {
	list, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		if _, ok := item.(string); !ok {
			return false
		}
	}
	return true
}

func anyStrings(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// jsonEqual is Python == over decoded JSON, which is what the resume restatement compares.
func jsonEqual(a, b any) bool { return canonical(a) == canonical(b) }
