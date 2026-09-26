//go:build dev

package ci

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode/utf16"
)

// jsonKV is one member of an ordered JSON object.
type jsonKV struct {
	Key   string
	Value any
}

// jsonObject keeps insertion order, as a Python dict does when json.dumps prints it.
type jsonObject []jsonKV

// pyJSON is json.dumps(value) with Python's defaults (ensure_ascii=True) and the given
// separators; compact uses (",", ":"), otherwise (", ", ": ").
func pyJSON(value any, compact bool) string {
	var b strings.Builder
	writePyJSON(&b, value, compact, -1, 0)
	return b.String()
}

// pyJSONIndent is json.dumps(value, indent=indent) (separators "," and ": ").
func pyJSONIndent(value any, indent int) string {
	var b strings.Builder
	writePyJSON(&b, value, false, indent, 0)
	return b.String()
}

func writePyJSON(b *strings.Builder, value any, compact bool, indent, depth int) {
	item, key := ", ", ": "
	if compact {
		item, key = ",", ":"
	}
	if indent >= 0 {
		item = ","
	}
	open := func(n int) {
		if indent >= 0 && n > 0 {
			b.WriteString("\n" + strings.Repeat(" ", indent*(depth+1)))
		}
	}
	sep := func() {
		b.WriteString(item)
		if indent >= 0 {
			b.WriteString("\n" + strings.Repeat(" ", indent*(depth+1)))
		}
	}
	closing := func(n int) {
		if indent >= 0 && n > 0 {
			b.WriteString("\n" + strings.Repeat(" ", indent*depth))
		}
	}
	switch v := value.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case int:
		b.WriteString(strconv.Itoa(v))
	case float64:
		b.WriteString(pyFloatRepr(v))
	case string:
		b.WriteString(pyJSONString(v))
	case json.Number:
		b.WriteString(pyJSONNumber(v))
	case *pyDict:
		// Only the plugin report prints decoded objects, and it uses sort_keys=True.
		var object jsonObject
		for _, key := range v.sortedKeys() {
			object = append(object, jsonKV{key, v.vals[key]})
		}
		writePyJSON(b, object, compact, indent, depth)
	case []string:
		b.WriteString("[")
		open(len(v))
		for i, s := range v {
			if i > 0 {
				sep()
			}
			b.WriteString(pyJSONString(s))
		}
		closing(len(v))
		b.WriteString("]")
	case []any:
		b.WriteString("[")
		open(len(v))
		for i, s := range v {
			if i > 0 {
				sep()
			}
			writePyJSON(b, s, compact, indent, depth+1)
		}
		closing(len(v))
		b.WriteString("]")
	case jsonObject:
		b.WriteString("{")
		open(len(v))
		for i, kv := range v {
			if i > 0 {
				sep()
			}
			b.WriteString(pyJSONString(kv.Key))
			b.WriteString(key)
			writePyJSON(b, kv.Value, compact, indent, depth+1)
		}
		closing(len(v))
		b.WriteString("}")
	default:
		panic(fmt.Sprintf("pyJSON: unsupported %T", value))
	}
}

// pyJSONString is a JSON string literal with ensure_ascii=True escaping.
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(&b, `\u%04x`, r)
			case r < 0x7f || r == 0x7f:
				b.WriteRune(r)
			case r > 0xffff:
				r1, r2 := utf16.EncodeRune(r)
				fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
			default:
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// pyReprList is repr() of a list of str.
func pyReprList(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = pyRepr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// errorWriter is the stream a check prints its refusal on.
func failf(w io.Writer, format string, args ...any) int {
	fmt.Fprintf(w, format+"\n", args...)
	return 1
}

// appendFile opens path for appending like open(path, "a").
func appendFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
}

// pyJSONNumber is json.dumps of the int or float a JSON number decoded to.
func pyJSONNumber(v json.Number) string {
	if n, ok := pyInt(v); ok {
		return n.String()
	}
	f := pyFloat(v)
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return pyFloatRepr(f)
}
