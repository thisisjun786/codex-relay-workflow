// Package pyjsontest is the corpus the Python-shaped JSON readers and writers are held to: JSON
// documents and Go values that reach every edge where json.loads, json.dumps and repr() differ
// from what encoding/json and fmt would do.
//
// Three sources make it up:
//
//   - Edge: generated documents and values (unicode, lone surrogates, bytes that are not UTF-8,
//     integers at and past int64, floats at the edges of their repr, NaN and the infinities,
//     repeated keys, nesting, whitespace, and documents a reader must refuse);
//   - the contract fixtures under contract/fixtures, and every JSON document held as a string
//     inside them;
//   - Sample: documents the Python reference implementation printed (the recorded answers its
//     parity tests replayed), the ones of at most 2000 bytes, kept in testdata/sample.json.gz
//     since the recordings themselves are gone.
//
// Recorded adds every document the goldens and fixtures of the checkout's tests hold (the
// expected outputs and the inputs that replaced the recorded Python answers), for the proofs that
// ran while a package's own copy of a reader or writer was being folded into internal/pyjson.
package pyjsontest

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Edge is the generated documents, well-formed and malformed.
func Edge() []string {
	docs := []string{
		// scalars
		`null`, `true`, `false`, `0`, `-0`, `1`, `-1`, `9223372036854775807`, `9223372036854775808`,
		`-9223372036854775808`, `-9223372036854775809`, `123456789012345678901234567890`,
		`-123456789012345678901234567890`, `0.0`, `-0.0`, `1.0`, `-1.5`, `1e16`, `1E16`, `1e+16`,
		`1e15`, `9999999999999998.0`, `1e-4`, `0.0001`, `1e-5`, `0.00009999999999999999`, `0.1`,
		`1.5e300`, `1e400`, `-1e400`, `1e-400`, `-1e-400`, `5e-324`, `1.7976931348623157e308`, `2.5`,
		`100.0`, `1e22`, `1e21`, `123456789.123`, `1E5`, `1e0`, `1.0e0`, `0e0`, `-0e-0`, `12345678901234567890e-5`,
		`NaN`, `Infinity`, `-Infinity`, `[NaN, Infinity, -Infinity]`, `{"n": NaN}`, `[1e400, 1]`,
		// strings
		`""`, `"a"`, `"\"\\\/\b\f\n\r\t"`, `"\u0000\u001f\u007f\u0080"`, "\"\u00e9\"", `"\u00e9"`,
		"\"\u2028\u2029\"", `"\ud83d\ude00"`, "\"\U0001f600\"", `"\ud800"`, `"\udfff"`, `"\udc00\ud800"`,
		`"\ud800\ud800"`, `"\ud800x"`, `"\ud800\u0041"`, `"\uDBFF\uDFFF"`, `"<>&'"`, `"\u003c\u003e\u0026"`,
		`"\\u003c"`, `"a\u0000b"`, `"\uffff\ufffe\ufffd"`, "\"\u65e5\u672c\u8a9e\"", `"\ud834\udd1e"`, `"tab\there"`,
		"\"raw\xffbyte\"", "\"raw\xed\xa0\x80wtf8\"", "\"trunc\xc3\"", "\"trunc\xe2\x82\"", "\"\x7f\"",
		`{"\ud800": 1, "\u00e9": 2, "<": 3}`, "{\"b\": 1, \"a\": 2, \"A\": 3, \"_\": 4, \"\u00e9\": 5, \"\\u00e9\": 6}",
		// containers
		`{}`, `[]`, `[[]]`, `[{}]`, `{"a": {}}`, `{"a": []}`, `{"a": 1}`, `{"a": 1, "a": 2}`,
		`{"b": 1, "a": 2, "b": 3}`, `{"a": {"b": 1, "b": 2}, "a": [1]}`, `[1, 2.5, "x", null, true, false, [], {}]`,
		` \t\n\r{ "a" : [ 1 , 2 ] } \t\n\r`, `{"x":{"y":{"z":[1,{"w":null}]}}}`,
		// malformed
		``, ` `, `{`, `[`, `}`, `]`, `[1,]`, `{"a":1,}`, `{"a" 1}`, `{"a":}`, `{1:2}`, `[1 2]`, `1 2`,
		`1 ]`, `1 }`, `{} x`, `{}}`, `[]]`, `12x`, `-`, `01`, `-01`, `1.`, `.5`, `1e`, `1e+`, `+1`,
		"\"\t\"", `"\x"`, `"\u12"`, `"\u12G4"`, `"abc`, "\ufeff{}", "\ufeff", `NaN x`, `nan`, `inf`,
		`-Inf`, `tru`, `nul`, `fals`, `True`, `None`, `'a'`, `[1e400, NaN]`, `{"a":NaN,}`, `[1,,2]`,
		`{"a":1 "b":2}`, `[` + strings.Repeat("9", 4301) + `]`, strings.Repeat("9", 4300),
	}
	// Many keys, one repeated: the reader's index of keys past its first few.
	var many strings.Builder
	many.WriteString("{")
	for i := range 40 {
		many.WriteString(`"k` + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + `": ` + string(rune('0'+i%10)) + `, `)
	}
	many.WriteString(`"kxa": "last"}`)
	docs = append(docs, many.String())
	// Nesting.
	docs = append(docs, strings.Repeat("[", 200)+strings.Repeat("]", 200))
	docs = append(docs, strings.Repeat(`{"a":`, 100)+"1"+strings.Repeat("}", 100))
	docs = append(docs, strings.Repeat("[", 50)+"1"+strings.Repeat("]", 49))
	// encoding/json's nesting limit (pyjson.MaxDepth), met by an array, an object and a scalar.
	for _, depth := range []int{pyjson.MaxDepth, pyjson.MaxDepth + 1} {
		docs = append(docs, strings.Repeat("[", depth)+strings.Repeat("]", depth),
			strings.Repeat(`{"a":`, depth)+"1"+strings.Repeat("}", depth),
			`{"x": NaN, "y": `+strings.Repeat("[", depth)+strings.Repeat("]", depth)+`}`,
			`{"y": `+strings.Repeat("[", depth)+strings.Repeat("]", depth)+`, "x": NaN}`,
			strings.Repeat("[", depth)+"1e400"+strings.Repeat("]", depth))
	}
	return docs
}

// Floats is the floats every repr of a float is held to: the edges of fixed notation, signed
// zeros, subnormals, the extremes, NaN and the infinities, and a deterministic spread of bit
// patterns.
func Floats() []float64 {
	floats := []float64{0, math.Copysign(0, -1), 1, -1, 0.1, 0.5, 1.5, 2.5, 100, 1e15, 1e16, 1e17,
		9999999999999998, 9007199254740993, 1e-4, 1e-5, 0.00009999999999999999, 0.00010000000000000002,
		1e22, 1e21, 1e100, 1e-100, 5e-324, math.SmallestNonzeroFloat64, math.MaxFloat64,
		-math.MaxFloat64, 1.7976931348623157e308, 123456789.123, 1.0 / 3, 2.0 / 3, 1e300 * 10,
		math.NaN(), math.Inf(1), math.Inf(-1), math.Nextafter(1e16, 0), math.Nextafter(1e-4, 0),
		math.Nextafter(1e-4, 1), 1234567, 12345678901234567890, 0.30000000000000004}
	seed := uint64(0x9e3779b97f4a7c15)
	for range 4000 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		f := math.Float64frombits(seed)
		if !math.IsNaN(f) {
			floats = append(floats, f)
		}
		// Short decimals with every exponent the repr switches notation around.
		exponent := int(seed%40) - 20
		floats = append(floats, float64(seed%100000)*math.Pow(10, float64(exponent)))
	}
	return floats
}

// Strings is the strings every writer of a string is held to: Unicode, controls, the characters
// json.dumps escapes, WTF-8 surrogates and bytes that are not UTF-8.
func Strings() []string {
	return []string{"", "a", "plain text", `"quoted" \ back/slash`, "\b\f\n\r\t\v\x00\x01\x1f\x7f",
		"\u00e9", "\u65e5\u672c\u8a9e", "\U0001f600", "\u2028\u2029", "\ufeff", "\uffff", "\u0080\u00a0\u00ad", "<>&'",
		`\u003c`, "\xff", "a\xffb", "\xed\xa0\x80", "\xed\xbf\xbf", "x\xed\xb2\x80y", "\xc3", "\xe2\x82",
		"\xf0\x9f\x98", "\xed\x9f\xbf", "\xf4\x90\x80\x80", "mixed \u00e9\xff\U0001f600\xed\xa0\x80\n", "\u0378", "\U000e0001",
		"\u017f", "\u00df", "\u0130", "\u00b5", "\U0010ffff", "\x1c\x1d\x1e\x1f\x85", " \t spaced \n"}
}

// Values is the Go values every writer is held to beyond what a reader makes of the documents:
// every Go type a caller hands a writer, at its edges.
func Values() []any {
	big1, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	values := []any{nil, true, false, 0, 1, -1, math.MaxInt64, math.MinInt64, int64(0), int64(math.MaxInt64),
		int64(math.MinInt64), big1, new(big.Int).Neg(big1), json.Number("0"), json.Number("-0"),
		json.Number("12"), json.Number("-12"), json.Number("1.0"), json.Number("1E5"), json.Number("1e400"),
		json.Number("2.50"), json.Number("123456789012345678901234567890"), []string{}, []string{"a", "\u00e9", "\xff"},
		[]any{}, map[string]any{}, map[string]any{"b": 1, "a": []any{"x", int64(2)}, "\u00e9": nil, "\xff": 1.5},
		[]map[string]any{{"z": 1, "y": 2}}, pyjson.Object{}, pyjson.Object{{Key: "b", Value: 1}, {Key: "a", Value: 2},
			{Key: "b", Value: 3}, {Key: "A", Value: pyjson.Object{{Key: "y", Value: []any{1, 2.5}}}}},
		[]any{[]any{[]any{}}, pyjson.Object{{Key: "k", Value: pyjson.Object{}}}}}
	for _, f := range Floats()[:60] {
		values = append(values, f)
	}
	for _, s := range Strings() {
		values = append(values, s, pyjson.Object{{Key: s, Value: s}}, []any{s})
	}
	return values
}

// Sample is Edge, the documents the contract fixtures hold and the recorded sample: the
// documents the goldens of the readers and writers are taken over.
func Sample(t testing.TB) []string {
	t.Helper()
	docs := Edge()
	docs = append(docs, fixtures(t)...)
	raw, err := os.ReadFile(filepath.Join(here(), "testdata", "sample.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	var sample []string
	if err := json.Unmarshal(gunzip(t, raw), &sample); err != nil {
		t.Fatal(err)
	}
	return unique(append(docs, sample...))
}

// Recorded is Sample and every document the goldens and fixtures in this checkout hold: each
// value of a golden (testdata/golden, internal/testsupport/golden's files) and each fixture
// (testdata/fixtures, gzip-compressed or not) that is a JSON document or holds documents as
// strings or lines. The goldens of internal/pyjson and internal/pyvalue are left out: they are
// what the corpus itself gives.
func Recorded(t testing.TB) []string {
	t.Helper()
	docs := Sample(t)
	root := moduleRoot()
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if rel, _ := filepath.Rel(root, path); rel == filepath.Join("internal", "pyjson") || rel == filepath.Join("internal", "pyvalue") {
				return filepath.SkipDir
			}
			return nil
		}
		kind := testdataKind(path)
		if kind == "" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".gz") {
			if raw, err = gunzipped(raw); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
		if kind == "fixtures" {
			docs = append(docs, held(string(raw))...)
			return nil
		}
		var stored struct {
			Values map[string]string `json:"values"`
		}
		if err := json.Unmarshal(raw, &stored); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		keys := make([]string, 0, len(stored.Values))
		for key := range stored.Values {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			value := stored.Values[key]
			if text, ok := strings.CutPrefix(value, "txt:"); ok {
				docs = append(docs, held(text)...)
			} else if encoded, ok := strings.CutPrefix(value, "b64:"); ok {
				if data, err := base64.StdEncoding.DecodeString(encoded); err == nil {
					docs = append(docs, held(string(data))...)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return unique(docs)
}

// testdataKind is "golden" or "fixtures" for a file under a testdata/golden or testdata/fixtures
// directory, at any depth below it, and "" for any other file.
func testdataKind(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := len(parts) - 2; i > 0; i-- {
		if parts[i-1] == "testdata" && (parts[i] == "golden" || parts[i] == "fixtures") {
			return parts[i]
		}
	}
	return ""
}

// held is the documents text is or holds: documentsIn of the text, and every document held as a
// string inside it when it is JSON itself.
func held(text string) []string {
	docs := documentsIn(text)
	var value any
	if json.Unmarshal([]byte(text), &value) == nil {
		docs = append(docs, stringsIn(value)...)
	}
	return docs
}

func gunzipped(raw []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(reader)
}

// fixtures is every JSON file under contract/fixtures and every document held as a string in one.
func fixtures(t testing.TB) []string {
	var docs []string
	err := filepath.WalkDir(filepath.Join(moduleRoot(), "contract", "fixtures"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		docs = append(docs, string(raw))
		var value any
		if json.Unmarshal(raw, &value) == nil {
			docs = append(docs, stringsIn(value)...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

// stringsIn is every document held as a string anywhere in value.
func stringsIn(value any) []string {
	var docs []string
	switch v := value.(type) {
	case string:
		docs = append(docs, documentsIn(v)...)
	case []any:
		for _, item := range v {
			docs = append(docs, stringsIn(item)...)
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			docs = append(docs, stringsIn(v[key])...)
		}
	}
	return docs
}

// documentsIn is text when it is a JSON object or array, else each of its lines that is one.
func documentsIn(text string) []string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !strings.ContainsAny(trimmed[:1], "{[") {
		return nil
	}
	if json.Valid([]byte(trimmed)) || pyjson.Error(trimmed) == "" {
		return []string{text}
	}
	var docs []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && strings.ContainsAny(line[:1], "{[") && pyjson.Error(line) == "" {
			docs = append(docs, line)
		}
	}
	return docs
}

func unique(docs []string) []string {
	seen := map[string]bool{}
	out := docs[:0:0]
	for _, doc := range docs {
		if !seen[doc] {
			seen[doc] = true
			out = append(out, doc)
		}
	}
	return out
}

func gunzip(t testing.TB, raw []byte) []byte {
	data, err := gunzipped(raw)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func here() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

func moduleRoot() string {
	dir := here()
	for !slices.Contains([]string{"", "/"}, dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return here()
}

// Same reports whether two decoded values are the same Go values: the same types throughout, an
// Object's fields in the same order, a float64 with the same bits (NaN matching NaN, -0.0 not
// matching 0.0).
func Same(a, b any) bool {
	switch x := a.(type) {
	case pyjson.Object:
		y, ok := b.(pyjson.Object)
		if !ok || len(x) != len(y) || (x == nil) != (y == nil) {
			return false
		}
		for i := range x {
			if x[i].Key != y[i].Key || !Same(x[i].Value, y[i].Value) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) || (x == nil) != (y == nil) {
			return false
		}
		for key, value := range x {
			other, present := y[key]
			if !present || !Same(value, other) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) || (x == nil) != (y == nil) {
			return false
		}
		for i := range x {
			if !Same(x[i], y[i]) {
				return false
			}
		}
		return true
	case float64:
		y, ok := b.(float64)
		return ok && (math.Float64bits(x) == math.Float64bits(y) || math.IsNaN(x) && math.IsNaN(y))
	case *big.Int:
		y, ok := b.(*big.Int)
		return ok && x.Cmp(y) == 0
	case nil:
		return b == nil
	case string, bool, json.Number, int64, int:
		return a == b
	}
	return false
}

// Decoded is what read makes of each document it accepts, and then Values: the values a writer
// is held to.
func Decoded(docs []string, read func(string) (any, error)) []any {
	var values []any
	for _, doc := range docs {
		if value, err := read(doc); err == nil {
			values = append(values, value)
		}
	}
	return append(values, Values()...)
}

// Errors compares two readers' errors: both nil, or both errors with the same text.
func Errors(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Error() == b.Error()
}

// Within reports whether every value inside v, v included, is of one of the given Go types
// (named as fmt's %T names them: "pyjson.Object", "[]interface {}", "string", ...): the values
// a writer whose callers build only those types is held to.
func Within(v any, types ...string) bool {
	if !slices.Contains(types, typeName(v)) {
		return false
	}
	switch x := v.(type) {
	case pyjson.Object:
		for _, f := range x {
			if !Within(f.Value, types...) {
				return false
			}
		}
	case map[string]any:
		for _, item := range x {
			if !Within(item, types...) {
				return false
			}
		}
	case []any:
		for _, item := range x {
			if !Within(item, types...) {
				return false
			}
		}
	case []map[string]any:
		for _, item := range x {
			if !Within(item, types...) {
				return false
			}
		}
	}
	return true
}

func typeName(v any) string {
	if v == nil {
		return "nil"
	}
	return reflect.TypeOf(v).String()
}
