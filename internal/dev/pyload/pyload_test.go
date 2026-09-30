//go:build dev

package pyload

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

// same is value equality in which NaN equals NaN: the two decoders must agree on the value,
// and a NaN they both produce is agreement.
func same(a, b any) bool {
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		return ok && (x == y || (math.IsNaN(x) && math.IsNaN(y))) && math.Signbit(x) == math.Signbit(y)
	case contract.OrderedObject:
		y, ok := b.(contract.OrderedObject)
		if !ok || len(x) != len(y) || (x == nil) != (y == nil) {
			return false
		}
		for i := range x {
			if x[i].Key != y[i].Key || !same(x[i].Value, y[i].Value) {
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
			if !same(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return reflect.TypeOf(a) == reflect.TypeOf(b) && a == b
}

// corpus is every JSON document the repository keeps as test data or contract, each line of the
// JSONL ones, the ledger cases' start records and ledger lines (surrogates, constants, refusals
// among them), and the edges hook.Decode's own adaptations exist for.
func corpus(t *testing.T) map[string][]byte {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string][]byte{}
	// internal/dev/pyload/testdata holds copies of the relay package's test fixtures, which leave
	// the repository with the Python implementation (todo 44).
	for _, dir := range []string{"internal/relay/hook/testdata", "internal/dev/trialledger/testdata", "contract", "internal/dev/pyload/testdata"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() && d.Name() == "python-oracle" {
				return filepath.SkipDir // recorded answers (internal/testsupport/pyoracle), not test data
			}
			if err != nil || d.IsDir() || !(strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".jsonl")) {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			docs[path] = raw
			for i, line := range strings.Split(string(raw), "\n") {
				docs[fmt.Sprintf("%s:%d", path, i+1)] = []byte(line)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "internal/dev/trialledger/testdata/cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases, err := hook.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range cases.([]any) {
		c := item.(contract.OrderedObject)
		for _, key := range []string{"start", "ledger"} {
			if text, ok := evidence.Get(c, key).(string); ok {
				docs["case start "+text] = []byte(text)
				for _, line := range strings.Split(text, "\n") {
					docs["case line "+line] = []byte(line)
				}
			}
		}
	}
	for _, doc := range []string{
		`NaN`, `[NaN, Infinity, -Infinity, "NaN", "-Infinity"]`, `{"a": 1, "a": 2, "b": {"a": 3, "a": [4]}}`,
		`"\ud800"`, `{"\udcff": "😀 \ud83d"}`, `[9223372036854775807, 9223372036854775808, -9223372036854775809]`,
		`[1e400, -1e400, 1e-400, -0, -0.0, 0.5e+3, 1E2]`, " \t\r\n{ \"a\" : [ ] , \"b\" : { } }\n ", `[]`, `{}`, `""`,
		`[true, false, null]`, `"a\"b\\c\/d\b\f\n\r\t\u0000"`, `{"a": "b"} x`, `[1, 2`, `{"a" 1}`, "\ufeff{}",
		`["` + "\x01" + `"]`, `1` + strings.Repeat("0", 4300), `1` + strings.Repeat("0", 4299), `[` + strings.Repeat(`[`, 9990) + strings.Repeat(`]`, 9990) + `]`,
		"\xff", "[\"\xed\xa0\x80\"]",
	} {
		docs["edge "+doc] = []byte(doc)
	}
	return docs
}

// Loads reads every document hook.Decode reads, as the same value, and refuses every one it
// refuses with the same message: the judges changed decoders for depth and nothing else.
func TestLoadsIsHookDecodeWithinEncodingJSONsDepth(t *testing.T) {
	docs := corpus(t)
	if len(docs) < 1000 {
		t.Fatalf("only %d documents", len(docs))
	}
	for name, raw := range docs {
		want, wantErr := hook.Decode(raw)
		got, err := Loads(raw)
		if (wantErr == nil) != (err == nil) {
			t.Fatalf("%.120q: hook.Decode %v, Loads %v", name, wantErr, err)
		}
		if err != nil {
			if err.Error() != wantErr.Error() {
				t.Fatalf("%.120q: hook.Decode %q, Loads %q", name, wantErr, err)
			}
			continue
		}
		if !same(want, got) {
			t.Fatalf("%.120q: hook.Decode %#v, Loads %#v", name, want, got)
		}
	}
}

func nested(open, fill, close string, depth int) string {
	return strings.Repeat(open, depth) + fill + strings.Repeat(close, depth)
}

func depthOf(v any) int {
	switch x := v.(type) {
	case []any:
		deepest := 0
		for _, item := range x {
			deepest = max(deepest, depthOf(item))
		}
		return 1 + deepest
	case contract.OrderedObject:
		deepest := 0
		for _, f := range x {
			deepest = max(deepest, depthOf(f.Value))
		}
		return 1 + deepest
	}
	return 0
}

// Past encoding/json's 10000 and up to CPython 3.14's own edge, a record reads; one container
// deeper it is the RecursionError the interpreter raises there, however deep the document goes.
func TestLoadsReadsAsDeepAsPythonAndRaisesPastIt(t *testing.T) {
	for _, depth := range []int{10000, 20000, Nesting} {
		doc := `{"observation": ` + nested("[", "", "]", depth-1) + `}`
		if _, err := hook.Decode([]byte(doc)); err == nil && depth > 10000 {
			t.Fatalf("encoding/json now reads %d deep; this package may no longer be needed", depth)
		}
		v, err := Loads([]byte(doc))
		if err != nil || depthOf(v) != depth {
			t.Fatalf("%d deep: %v (depth %d)", depth, err, depthOf(v))
		}
		v, err = Loads([]byte(nested(`{"a": `, "1", "}", depth)))
		if err != nil || depthOf(v) != depth {
			t.Fatalf("%d objects deep: %v", depth, err)
		}
	}
	for _, c := range []struct {
		doc, kind string
	}{
		{nested("[", "", "]", Nesting+1), "array"},
		{nested(`{"a": `, "1", "}", Nesting+1), "object"},
		{`{"a": ` + nested("[", "", "]", Nesting) + `}`, "array"},
		{nested("[", "", "]", 2_000_000), "array"}, // deep enough to exhaust Go's stack if recursed
		{`[` + nested("[", "", "]", Nesting) + `, "x" y]`, "array"},
	} {
		_, err := Loads([]byte(c.doc))
		python, deep := Recursion(err)
		if !deep || python.Detail != "maximum recursion depth exceeded while decoding a JSON "+c.kind+" from a unicode string" {
			t.Fatalf("%d deep: %v", len(c.doc)/2, err)
		}
	}
	// A syntax error before the edge is reached is the JSONDecodeError, as the scanner meets it.
	_, err := Loads([]byte(`[1 2, ` + nested("[", "", "]", Nesting+5) + `]`))
	if _, deep := Recursion(err); deep || err == nil || !strings.HasPrefix(err.Error(), "Expecting ',' delimiter") {
		t.Fatalf("%v", err)
	}
}
