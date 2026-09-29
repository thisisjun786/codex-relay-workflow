package execution

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
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

// PolicyDepth is the C JSON scanner's container budget at the policy parse (measured against
// CPython 3.13 through the relay CLI: an array at depth 9999, or an object closing at 9997,
// raises RecursionError, which from_bytes does not catch; the relay's rolepolicy answers it).
const PolicyDepth = 9998

// decode parses JSON like json.loads(raw, object_pairs_hook=_no_duplicates): the bytes decoded
// as json.loads decodes bytes (UTF-8, UTF-16 or UTF-32 by pyjson.DecodeBytes, a byte order
// mark read as the codec's own), then scanned as CPython scans them, so every refusal is the
// one Python meets first, in its words: the codec error, the JSONDecodeError, the 4300-digit
// integer limit, or the hook's duplicate key at the close of the object that repeats it.
// NaN, Infinity and -Infinity are numbers, as json.loads reads them.
func decode(raw []byte) (any, error) {
	text, err := pyjson.DecodeBytes(raw)
	if err != nil {
		return nil, &syntaxError{err.Error()}
	}
	if message, duplicate, _ := pyjson.HookedError(text, PolicyDepth); message != "" && !duplicate {
		return nil, &syntaxError{message}
	}
	// The scan read everything up to the first repeated key, which the build refuses as the
	// hook does.
	b := &builder{s: text}
	return b.value()
}

type syntaxError struct{ detail string }

func (e *syntaxError) Error() string { return e.detail }

// builder reads a document pyjson has already scanned into the policy's values: *object in
// document order, []any, string, json.Number, float64 for the three constants, bool and nil.
type builder struct {
	s string
	i int
}

func (b *builder) ws() {
	for b.i < len(b.s) && strings.IndexByte(" \t\n\r", b.s[b.i]) >= 0 {
		b.i++
	}
}

func (b *builder) value() (any, error) {
	b.ws()
	rest := b.s[b.i:]
	switch {
	case rest == "":
		return nil, &syntaxError{"unexpected end of document"}
	case rest[0] == '{':
		b.i++
		return b.object()
	case rest[0] == '[':
		b.i++
		list := []any{}
		for b.ws(); b.i < len(b.s) && b.s[b.i] != ']'; b.ws() {
			item, err := b.value()
			if err != nil {
				return nil, err
			}
			list = append(list, item)
			if b.ws(); b.i < len(b.s) && b.s[b.i] == ',' {
				b.i++
			}
		}
		b.i++
		return list, nil
	case rest[0] == '"':
		return b.str()
	}
	for _, literal := range []struct {
		text  string
		value any
	}{{"null", nil}, {"true", true}, {"false", false}, {"NaN", math.NaN()}, {"Infinity", math.Inf(1)}, {"-Infinity", math.Inf(-1)}} {
		if strings.HasPrefix(rest, literal.text) {
			b.i += len(literal.text)
			return literal.value, nil
		}
	}
	end := strings.IndexFunc(rest, func(r rune) bool { return !strings.ContainsRune("+-.0123456789eE", r) })
	if end < 0 {
		end = len(rest)
	}
	b.i += end
	return json.Number(rest[:end]), nil
}

// str reads one string token, escapes decoded as encoding/json decodes them (a lone surrogate
// escape is U+FFFD, decision 21).
func (b *builder) str() (string, error) {
	start := b.i
	for b.i++; b.i < len(b.s) && b.s[b.i] != '"'; b.i++ {
		if b.s[b.i] == '\\' {
			b.i++
		}
	}
	b.i++
	var out string
	if err := json.Unmarshal([]byte(b.s[start:b.i]), &out); err != nil {
		return "", &syntaxError{err.Error()}
	}
	return out, nil
}

// object reads the members after '{' and refuses a repeated key when the object closes, as
// _no_duplicates does, so an inner duplicate is reported before an outer one.
func (b *builder) object() (any, error) {
	o := &object{values: map[string]any{}}
	repeated := ""
	for b.ws(); b.i < len(b.s) && b.s[b.i] != '}'; b.ws() {
		key, err := b.str()
		if err != nil {
			return nil, err
		}
		b.ws()
		b.i++ // ':'
		value, err := b.value()
		if err != nil {
			return nil, err
		}
		if o.has(key) {
			if repeated == "" {
				repeated = key
			}
		} else {
			o.keys = append(o.keys, key)
			o.values[key] = value
		}
		if b.ws(); b.i < len(b.s) && b.s[b.i] == ',' {
			b.i++
		}
	}
	b.i++
	if repeated != "" {
		return nil, &PolicyError{fmt.Sprintf("duplicate key %s in the execution policy", repr(repeated))}
	}
	return o, nil
}

// pyStrip is str.strip(): str.isspace also covers U+001C..U+001F, which unicode.IsSpace
// does not.
func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f })
}

// isBlank is Python's `not value.strip()`.
func isBlank(s string) bool { return pyStrip(s) == "" }

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
