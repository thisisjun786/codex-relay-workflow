package execution

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// object is a decoded JSON object in document order, because Python iterates the policy's dicts
// in that order and reports the first failure it meets.
type object = pyjson.Object

// keys is an object's keys in document order.
func keys(o object) []string {
	out := make([]string, len(o))
	for i, field := range o {
		out[i] = field.Key
	}
	return out
}

func has(o object, key string) bool { _, ok := o.Lookup(key); return ok }

// present returns the given keys found in o, sorted like Python sorted(set & set).
func present(o object, keys ...string) []string {
	var found []string
	for _, k := range keys {
		if has(o, k) {
			found = append(found, k)
		}
	}
	slices.Sort(found)
	return found
}

func absent(o object, keys ...string) []string {
	var missing []string
	for _, k := range keys {
		if !has(o, k) {
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
// integer limit, or the hook's duplicate key at the close of the object that repeats it. The
// values are pyjson's: objects in document order, every number a json.Number as spelled, NaN,
// Infinity and -Infinity floats, and a lone surrogate escape kept.
func decode(raw []byte) (any, error) {
	text, err := pyjson.DecodeBytes(raw)
	if err != nil {
		return nil, &syntaxError{err.Error()}
	}
	if message, duplicate, _ := pyjson.HookedError(text, PolicyDepth); message != "" && !duplicate {
		return nil, &syntaxError{message}
	}
	value, err := pyjson.Loads(text, pyjson.LoadOptions{Constants: true, Surrogates: true, Numbers: pyjson.SpelledNumbers, Unique: true, Deep: true})
	var repeated *pyjson.RepeatedKey
	if errors.As(err, &repeated) {
		return nil, &PolicyError{fmt.Sprintf("duplicate key %s in the execution policy", repr(repeated.Key))}
	}
	return value, err
}

type syntaxError struct{ detail string }

func (e *syntaxError) Error() string { return e.detail }

// isBlank is a value with nothing but white space in it (Python's `not value.strip()`, which
// the policy's acceptance keeps).
func isBlank(s string) bool { return pyvalue.Strip(s) == "" }

// repr is a value the policy's messages quote: a string Go-quoted, so a lone surrogate the
// policy holds stays visible and two such names stay apart, anything else as its JSON text.
func repr(value any) string {
	if s, ok := value.(string); ok {
		return strconv.Quote(s)
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Sprint(value)
	}
	return strings.TrimSuffix(out.String(), "\n")
}
