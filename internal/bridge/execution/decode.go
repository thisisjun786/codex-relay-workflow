package execution

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// object is a decoded JSON object that keeps document order, because Python iterates the
// policy's dicts in that order and reports the first failure it meets.
type object struct {
	keys   []string
	values map[string]any
}

func (o *object) has(key string) bool { _, ok := o.values[key]; return ok }

// present returns the given keys found in o, sorted like Python sorted(set & set).
func (o *object) present(keys ...string) []string {
	var found []string
	for _, k := range keys {
		if o.has(k) {
			found = append(found, k)
		}
	}
	slices.Sort(found)
	return found
}

func (o *object) absent(keys ...string) []string {
	var missing []string
	for _, k := range keys {
		if !o.has(k) {
			missing = append(missing, k)
		}
	}
	slices.Sort(missing)
	return missing
}

// decode parses JSON like json.loads(raw, object_pairs_hook=_no_duplicates): a repeated key is
// refused when its object closes, so an inner duplicate is reported before an outer one.
func decode(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	value, err := decodeValue(d)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, &syntaxError{"extra data after the document"}
	}
	return value, nil
}

type syntaxError struct{ detail string }

func (e *syntaxError) Error() string { return e.detail }

func decodeValue(d *json.Decoder) (any, error) {
	token, err := d.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &syntaxError{"unexpected end of document"}
		}
		return nil, &syntaxError{err.Error()}
	}
	switch t := token.(type) {
	case json.Delim:
		if t == '[' {
			list := []any{}
			for d.More() {
				item, err := decodeValue(d)
				if err != nil {
					return nil, err
				}
				list = append(list, item)
			}
			_, err := d.Token()
			return list, wrapSyntax(err)
		}
		return decodeObject(d)
	default:
		return t, nil
	}
}

func decodeObject(d *json.Decoder) (any, error) {
	type pair struct {
		key   string
		value any
	}
	var pairs []pair
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, wrapSyntax(err)
		}
		value, err := decodeValue(d)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, pair{key.(string), value})
	}
	if _, err := d.Token(); err != nil {
		return nil, wrapSyntax(err)
	}
	o := &object{values: map[string]any{}}
	for _, p := range pairs {
		if o.has(p.key) {
			return nil, &PolicyError{fmt.Sprintf("duplicate key %s in the execution policy", repr(p.key))}
		}
		o.keys = append(o.keys, p.key)
		o.values[p.key] = p.value
	}
	return o, nil
}

func wrapSyntax(err error) error {
	if err == nil {
		return nil
	}
	return &syntaxError{err.Error()}
}

// isBlank is Python's `not value.strip()`: str.isspace also covers U+001C..U+001F.
func isBlank(s string) bool {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f }) == ""
}

// repr renders the Python repr() of the values the policy's messages quote.
func repr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case string:
		quote := "'"
		if strings.Contains(v, "'") && !strings.Contains(v, `"`) {
			quote = `"`
		}
		var b strings.Builder
		b.WriteString(quote)
		for _, r := range v {
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
	case []string:
		parts := make([]string, len(v))
		for i, s := range v {
			parts[i] = repr(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case json.Number:
		return v.String()
	case []any:
		parts := make([]string, len(v))
		for i, s := range v {
			parts[i] = repr(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return strconv.Quote(fmt.Sprint(v))
	}
}
