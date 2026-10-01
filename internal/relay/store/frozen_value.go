package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The fence reads a manifest record's fields as whatever json.loads made of them and compares,
// prints and hashes them as Python does. These are those values in Go: nil (None), bool, an int as
// int64 or *big.Int, a float as float64 (NaN and the infinities included), a str as a string that
// keeps a lone surrogate in its three-byte generalized UTF-8 form, a list as []any and a dict as
// contract.OrderedObject. A json.Number is read as the int or float its spelling is, so a decoder
// that keeps an over-long integer that way (hook.Decode) passes its values unchanged.

// PythonEntry is manifest.Entry as Entry.from_record builds it: each field the value json.loads
// gave it, and Bytes nil for a "bytes" that is null or absent.
type PythonEntry struct{ Path, SHA256, Bytes any }

// PythonEntries is the Entry each ManifestEntry stands for; nil stays nil (the fence's None).
func PythonEntries(entries []ManifestEntry) []PythonEntry {
	if entries == nil {
		return nil
	}
	out := make([]PythonEntry, len(entries))
	for i, entry := range entries {
		out[i] = PythonEntry{Path: entry.Path, SHA256: entry.SHA256}
		if entry.Bytes != nil {
			out[i].Bytes = *entry.Bytes
		}
	}
	return out
}

// PythonManifestEntries is [Entry.from_record(record) for record in records]: each record's
// ["path"] and ["sha256"] and its .get("bytes"), raising the KeyError or TypeError the first
// record that is not a dict holding both raises.
func PythonManifestEntries(records []any) ([]PythonEntry, error) {
	out := make([]PythonEntry, 0, len(records))
	for _, record := range records {
		path, err := pythonSubscript(record, "path")
		if err != nil {
			return nil, err
		}
		digest, err := pythonSubscript(record, "sha256")
		if err != nil {
			return nil, err
		}
		size, _ := pythonGet(record.(contract.OrderedObject), "bytes")
		out = append(out, PythonEntry{Path: path, SHA256: digest, Bytes: size})
	}
	return out, nil
}

// PythonRevisionHash is manifest.revision_hash: each path encoded to UTF-8 in record order (an
// AttributeError for a path that is not a str, a UnicodeEncodeError for a lone surrogate), the
// entries sorted stably by those bytes, and each then checked as canonical_payload checks it,
// with a digest that is not a str named as its repr.
func PythonRevisionHash(entries []PythonEntry) (string, error) {
	type keyed struct {
		path   string
		digest any
	}
	ordered := make([]keyed, 0, len(entries))
	for _, entry := range entries {
		path, ok := entry.Path.(string)
		if !ok {
			return "", &ManifestException{Class: "AttributeError", text: "'" + pythonTypeName(entry.Path) + "' object has no attribute 'encode'"}
		}
		if err := utf8EncodeError(path); err != nil {
			return "", err
		}
		ordered = append(ordered, keyed{path, entry.SHA256})
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].path < ordered[j].path })
	lines := make([]string, 0, len(ordered))
	for _, entry := range ordered {
		declared, err := NormalizeDeclaredPath(entry.path)
		if err != nil {
			return "", err
		}
		digest, ok := entry.digest.(string)
		if !ok || !lowerDigest.MatchString(digest) {
			return "", refuse(ReasonManifestUnverified, "entry %s has a digest that is not 64 lowercase hex characters: %s", PythonRepr(declared), pythonReprValue(entry.digest))
		}
		lines = append(lines, declared+":"+digest)
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

// utf8EncodeError is the UnicodeEncodeError str.encode("utf-8") raises for the first run of lone
// surrogates in text, positions counted in code points.
func utf8EncodeError(text string) error {
	position := 0
	for i := 0; i < len(text); position++ {
		r, size := settings.CodePoint(text, i)
		i += size
		if !isSurrogate(r) {
			continue
		}
		start, end, first := position, position+1, r
		for i < len(text) {
			next, size := settings.CodePoint(text, i)
			if !isSurrogate(next) {
				break
			}
			i += size
			end++
		}
		message := fmt.Sprintf("'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed", start, end-1)
		if end == start+1 {
			message = fmt.Sprintf("'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", first, start)
		}
		return &ManifestException{Class: "UnicodeEncodeError", text: message}
	}
	return nil
}

func isSurrogate(r rune) bool { return r >= 0xd800 && r <= 0xdfff }

// PythonStr is str(value): a str as itself and anything else as its repr.
func PythonStr(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return pythonReprValue(value)
}

// pythonReprValue is repr(value).
func pythonReprValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return PythonRepr(v)
	case []any:
		items := make([]string, len(v))
		for i, item := range v {
			items[i] = pythonReprValue(item)
		}
		return "[" + strings.Join(items, ", ") + "]"
	case contract.OrderedObject:
		items := make([]string, len(v))
		for i, field := range v {
			items[i] = PythonRepr(field.Key) + ": " + pythonReprValue(field.Value)
		}
		return "{" + strings.Join(items, ", ") + "}"
	}
	number, ok := pythonNumberOf(value)
	if !ok {
		return fmt.Sprint(value)
	}
	if !number.float {
		return number.integer.String()
	}
	switch {
	case math.IsNaN(number.real):
		return "nan"
	case math.IsInf(number.real, 1):
		return "inf"
	case math.IsInf(number.real, -1):
		return "-inf"
	}
	return pyjson.Float(number.real)
}

// pythonTypeName is type(value).__name__.
func pythonTypeName(value any) string {
	switch value.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case []any:
		return "list"
	case contract.OrderedObject:
		return "dict"
	}
	if number, ok := pythonNumberOf(value); ok {
		if number.float {
			return "float"
		}
		return "int"
	}
	return fmt.Sprintf("%T", value)
}

// pythonTruthy is bool(value).
func pythonTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case contract.OrderedObject:
		return len(v) > 0
	}
	number, ok := pythonNumberOf(value)
	if !ok {
		return true
	}
	if number.float {
		return number.real != 0
	}
	return number.integer.Sign() != 0
}

// pythonHashable reports whether hash(value) succeeds: a list and a dict are unhashable.
func pythonHashable(value any) bool {
	switch value.(type) {
	case []any, contract.OrderedObject:
		return false
	}
	return true
}

// PythonEqual is a == b. json.loads builds every NaN as one object (json.decoder.NaN), so NaN is
// never equal to NaN itself while two lists or dicts holding it compare equal, because a
// container compares an item by identity before equality.
func PythonEqual(a, b any) bool { return pythonEqual(a, b, false) }

func pythonEqual(a, b any, item bool) bool {
	x, xNumber := pythonNumberOf(a)
	y, yNumber := pythonNumberOf(b)
	if xNumber || yNumber {
		if !xNumber || !yNumber {
			return false
		}
		if item && x.float && y.float && math.IsNaN(x.real) && math.IsNaN(y.real) {
			return true
		}
		return x.equal(y)
	}
	switch v := a.(type) {
	case nil:
		return b == nil
	case string:
		w, ok := b.(string)
		return ok && v == w
	case []any:
		w, ok := b.([]any)
		if !ok || len(v) != len(w) {
			return false
		}
		for i := range v {
			if !pythonEqual(v[i], w[i], true) {
				return false
			}
		}
		return true
	case contract.OrderedObject:
		w, ok := b.(contract.OrderedObject)
		if !ok || len(v) != len(w) {
			return false
		}
		for _, field := range v {
			other, found := pythonGet(w, field.Key)
			if !found || !pythonEqual(field.Value, other, true) {
				return false
			}
		}
		return true
	}
	return false
}

// pythonNumber is an int (bool included, which Python compares as 0 and 1) or a float.
type pythonNumber struct {
	integer *big.Int
	real    float64
	float   bool
}

func pythonNumberOf(value any) (pythonNumber, bool) {
	switch v := value.(type) {
	case bool:
		if v {
			return pythonNumber{integer: big.NewInt(1)}, true
		}
		return pythonNumber{integer: big.NewInt(0)}, true
	case int:
		return pythonNumber{integer: big.NewInt(int64(v))}, true
	case int64:
		return pythonNumber{integer: big.NewInt(v)}, true
	case *big.Int:
		return pythonNumber{integer: v}, true
	case float64:
		return pythonNumber{real: v, float: true}, true
	case json.Number:
		return pythonNumberSpelled(string(v))
	}
	return pythonNumber{}, false
}

// pythonNumberSpelled is json.loads's number: float(text) when it has a fraction or an exponent
// (an overflow is an infinity, as float() makes it), int(text) otherwise.
func pythonNumberSpelled(text string) (pythonNumber, bool) {
	if strings.ContainsAny(text, ".eE") {
		value, err := strconv.ParseFloat(text, 64)
		if err != nil && !isRangeError(err) {
			return pythonNumber{}, false
		}
		return pythonNumber{real: value, float: true}, true
	}
	value, ok := new(big.Int).SetString(text, 10)
	return pythonNumber{integer: value}, ok
}

func isRangeError(err error) bool {
	numeric, ok := err.(*strconv.NumError)
	return ok && numeric.Err == strconv.ErrRange
}

// equal compares exactly, an int with a float too, as Python does.
func (x pythonNumber) equal(y pythonNumber) bool {
	switch {
	case !x.float && !y.float:
		return x.integer.Cmp(y.integer) == 0
	case x.float && y.float:
		return x.real == y.real
	case x.float:
		return floatEqualsInt(x.real, y.integer)
	}
	return floatEqualsInt(y.real, x.integer)
}

func floatEqualsInt(real float64, integer *big.Int) bool {
	if math.IsNaN(real) || math.IsInf(real, 0) {
		return false
	}
	return new(big.Float).SetFloat64(real).Cmp(new(big.Float).SetInt(integer)) == 0
}

// pythonGet is dict.get(key).
func pythonGet(object contract.OrderedObject, key string) (any, bool) {
	for _, field := range object {
		if field.Key == key {
			return field.Value, true
		}
	}
	return nil, false
}

// pythonSubscript is value[key] for a str key.
func pythonSubscript(value any, key string) (any, error) {
	switch v := value.(type) {
	case contract.OrderedObject:
		if item, ok := pythonGet(v, key); ok {
			return item, nil
		}
		return nil, &ManifestException{Class: "KeyError", text: PythonRepr(key)}
	case []any:
		return nil, &ManifestException{Class: "TypeError", text: "list indices must be integers or slices, not str"}
	case string:
		return nil, &ManifestException{Class: "TypeError", text: "string indices must be integers, not 'str'"}
	}
	return nil, &ManifestException{Class: "TypeError", text: "'" + pythonTypeName(value) + "' object is not subscriptable"}
}

// decodePythonJSON is json.loads over text that PythonJSONError has already accepted (as deep as
// frozenJSONDepth): NaN, Infinity and -Infinity are floats, an integer is exact (an int64, or a
// *big.Int past it), a lone surrogate escape is kept, and a repeated key keeps its first place
// and its last value.
func decodePythonJSON(text string) (any, error) {
	return pyjson.Loads(text, pyjson.LoadOptions{Constants: true, Surrogates: true, Numbers: pyjson.BigNumbers, Deep: true})
}
