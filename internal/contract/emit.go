package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Field is one entry in a Python insertion-ordered JSON object.
type Field struct {
	Key   string
	Value any
}

// OrderedObject preserves field order at every object nesting level.
type OrderedObject []Field

// Result is the JSON envelope returned by a relay command.
type Result = OrderedObject

// Emit writes Python json.dumps(..., indent=2) output and its trailing newline.
// Objects must be OrderedObject rather than maps: Go maps cannot represent insertion order.
func Emit(w io.Writer, value any) error {
	var buf bytes.Buffer
	if err := appendValue(&buf, value, 0); err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	buf.WriteByte('\n')
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}

func appendValue(buf *bytes.Buffer, value any, depth int) error {
	switch v := value.(type) {
	case OrderedObject:
		buf.WriteByte('{')
		for i, field := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
			indent(buf, depth+1)
			appendString(buf, field.Key)
			buf.WriteString(": ")
			if err := appendValue(buf, field.Value, depth+1); err != nil {
				return err
			}
		}
		if len(v) > 0 {
			buf.WriteByte('\n')
			indent(buf, depth)
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
			indent(buf, depth+1)
			if err := appendValue(buf, item, depth+1); err != nil {
				return err
			}
		}
		if len(v) > 0 {
			buf.WriteByte('\n')
			indent(buf, depth)
		}
		buf.WriteByte(']')
	case string:
		appendString(buf, v)
	case float64:
		switch {
		case math.IsNaN(v):
			buf.WriteString("NaN")
		case math.IsInf(v, 1):
			buf.WriteString("Infinity")
		case math.IsInf(v, -1):
			buf.WriteString("-Infinity")
		default:
			// Python repr uses fixed notation from 1e-4 through (but not including) 1e16.
			format := byte('e')
			magnitude := math.Abs(v)
			if magnitude == 0 || magnitude >= 1e-4 && magnitude < 1e16 {
				format = 'f'
			}
			text := strconv.FormatFloat(v, format, -1, 64)
			if format == 'f' && !strings.Contains(text, ".") {
				text += ".0"
			}
			buf.WriteString(text)
		}
	case nil, bool, int, int64, json.Number:
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("encode scalar: %w", err)
		}
		buf.Write(encoded)
	default:
		return fmt.Errorf("unsupported ordered JSON value %T", value)
	}
	return nil
}

func indent(buf *bytes.Buffer, depth int) {
	for range depth * 2 {
		buf.WriteByte(' ')
	}
}

func appendString(buf *bytes.Buffer, value string) {
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
