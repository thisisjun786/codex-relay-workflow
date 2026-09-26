package delivery

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// Obj is a Python dict in insertion order; the records this package returns keep Python's order.
type Obj = contract.OrderedObject

// F is one field of an Obj.
type F = contract.Field

// get returns the value of key in o, and whether it was present.
func get(o Obj, key string) (any, bool) {
	for _, f := range o {
		if f.Key == key {
			return f.Value, true
		}
	}
	return nil, false
}

// set replaces key in o, or appends it, as a Python dict assignment does.
func set(o Obj, key string, value any) Obj {
	for i, f := range o {
		if f.Key == key {
			o[i].Value = value
			return o
		}
	}
	return append(o, F{Key: key, Value: value})
}

func str(o Obj, key string) string {
	v, _ := get(o, key)
	s, _ := v.(string)
	return s
}

// dumps is Python json.dumps(value) with the default separators and ensure_ascii.
func dumps(value any) string {
	var b strings.Builder
	writeJSON(&b, value, false)
	return b.String()
}

// dumpsSorted is json.dumps(value, sort_keys=True).
func dumpsSorted(value any) string {
	var b strings.Builder
	writeJSON(&b, value, true)
	return b.String()
}

func writeJSON(b *strings.Builder, value any, sorted bool) {
	switch v := value.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeString(b, v)
	case int:
		b.WriteString(strconv.Itoa(v))
	case int64:
		b.WriteString(strconv.FormatInt(v, 10))
	case float64:
		b.WriteString(pyFloat(v))
	case json.Number:
		b.WriteString(v.String())
	case Obj:
		fields := v
		if sorted {
			fields = append(Obj(nil), v...)
			sort.SliceStable(fields, func(i, j int) bool { return fields[i].Key < fields[j].Key })
		}
		b.WriteByte('{')
		for i, f := range fields {
			if i > 0 {
				b.WriteString(", ")
			}
			writeString(b, f.Key)
			b.WriteString(": ")
			writeJSON(b, f.Value, sorted)
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				b.WriteString(", ")
			}
			writeJSON(b, item, sorted)
		}
		b.WriteByte(']')
	case []string:
		items := make([]any, len(v))
		for i, s := range v {
			items[i] = s
		}
		writeJSON(b, items, sorted)
	default:
		panic(fmt.Sprintf("delivery: unsupported JSON value %T", value))
	}
}

func pyFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	m := math.Abs(f)
	if m == 0 || m >= 1e-4 && m < 1e16 {
		text := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	text := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, exponent, _ := strings.Cut(text, "e")
	digits := strings.TrimLeft(exponent[1:], "0")
	if len(digits) < 2 {
		digits = strings.Repeat("0", 2-len(digits)) + digits
	}
	return mantissa + "e" + exponent[:1] + digits
}

func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
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
			for _, u := range utf16.Encode([]rune{r}) {
				fmt.Fprintf(b, "\\u%04x", u)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// loads decodes JSON text into Obj/[]any/string/bool/nil/json.Number, keeping key order.
func loads(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := Obj{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := keyTok.(string)
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				o = set(o, key, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			a := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return a, nil
		}
	case json.Number:
		if i, err := t.Int64(); err == nil && !strings.ContainsAny(t.String(), ".eE") {
			return i, nil
		}
		f, err := t.Float64()
		return f, err
	}
	return tok, nil
}

func loadsObj(text string) Obj {
	v, err := loads(text)
	if err != nil {
		return nil
	}
	o, _ := v.(Obj)
	return o
}
