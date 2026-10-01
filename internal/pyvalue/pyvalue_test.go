package pyvalue_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// What repr, str, type, bool and == answer for the corpus is a golden: the values the readers
// build from the generated documents and the sample, and every generated Go value. Each answer
// was proven against the copy it replaced (the pyvalue_fold_test.go of the fold's first step)
// before that copy was deleted.
func TestValues_are_the_goldens(t *testing.T) {
	read := func(docs []string) []any {
		var values []any
		for _, options := range []pyjson.LoadOptions{
			{Python: true, Constants: true, Surrogates: true, Numbers: pyjson.BigNumbers},
			{Python: true, Constants: true},
			{Numbers: pyjson.Int64FloatNumbers, Map: true},
		} {
			for _, doc := range docs {
				if len(doc) > 1<<14 {
					continue
				}
				if value, err := pyjson.Loads(doc, options); err == nil {
					values = append(values, value)
				}
			}
		}
		return values
	}
	edge := append(read(pyjsontest.Edge()), pyjsontest.Values()...)
	sample := read(pyjsontest.Sample(t))
	answer := func(v any) string {
		return fmt.Sprintf("%s %s %s %v", pyvalue.Repr(v), strconv.Quote(pyvalue.Str(v)), pyvalue.TypeName(v), pyvalue.Truthy(v))
	}
	golden.Check(t, "repr str type truth/edge", []byte(lines(edge, answer)))
	golden.Check(t, "repr str type truth/sample", []byte(digest(sample, answer)))
	equal := func(pair [2]any) string {
		return fmt.Sprintf("%s == %s: %v, as items %v", pyvalue.Repr(pair[0]), pyvalue.Repr(pair[1]), pyvalue.Equal(pair[0], pair[1]), pyvalue.ItemEqual(pair[0], pair[1]))
	}
	golden.Check(t, "equal/edge", []byte(lines(pyjsontest.Pairs(edge), equal)))
	golden.Check(t, "equal/sample", []byte(digest(pyjsontest.Pairs(sample), equal)))
	text := func(s string) string {
		path, ok := pyvalue.FSEncode(pyvalue.FSDecode(s))
		return fmt.Sprintf("%q strip %q fsdecode %q fsencode %q %v sha256 %s", s, pyvalue.Strip(s), pyvalue.FSDecode(s), path, ok, pyvalue.SHA256Hex(s))
	}
	golden.Check(t, "text", []byte(lines(pyjsontest.Strings(), text)))
}

func lines[T any](items []T, render func(T) string) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString(render(item))
		b.WriteByte('\n')
	}
	return b.String()
}

func digest[T any](items []T, render func(T) string) string {
	hash := sha256.New()
	for _, item := range items {
		hash.Write([]byte(render(item) + "\n"))
	}
	return fmt.Sprintf("%d items, sha256 %s\n", len(items), hex.EncodeToString(hash.Sum(nil)))
}
