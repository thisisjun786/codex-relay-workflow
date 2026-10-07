//go:build dev

package cxcfuzz

import (
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The pyjson target is registered and its oracle needs both node and python3.
func TestPyjsonTargetIsRegistered(t *testing.T) {
	if !slices.Contains(Names(), "pyjson") {
		t.Fatalf("pyjson is not registered: %v", Names())
	}
	target, ok := Lookup("pyjson")
	if !ok {
		t.Fatal("pyjson is not registered")
	}
	if target.Oracle.Command != "node" || !slices.Contains(target.Oracle.Requires, "python3") {
		t.Fatalf("oracle %+v does not need node and python3", target.Oracle)
	}
}

// The Go side answers the json.tool flags the way the oracle does, with no Node and no python3:
// a success re-dumps the value, a refusal is the error text on stderr with exit 1.
func TestPyjsonGoAnswersTheJsonToolFlags(t *testing.T) {
	root := t.TempDir()
	for _, row := range []struct {
		name   string
		input  pyjson.Object
		exit   float64
		stdout string
	}{
		{"compact", pyjson.Object{{Key: "text", Value: `{"b": 1, "a": [1, 2]}`}, {Key: "compact", Value: true}}, 0, `{"b":1,"a":[1,2]}`},
		{"sorted-indent", pyjson.Object{{Key: "text", Value: `{"b": 1, "a": 2}`}, {Key: "sortKeys", Value: true}, {Key: "indent", Value: 2}}, 0, "{\n  \"a\": 2,\n  \"b\": 1\n}"},
		{"no-indent", pyjson.Object{{Key: "text", Value: `{"a": 1}`}, {Key: "indent", Value: 0}}, 0, `{"a": 1}`},
		{"big-integer", pyjson.Object{{Key: "text", Value: `{"a": 123456789012345678901234567890}`}}, 0, "{\n    \"a\": 123456789012345678901234567890\n}"},
	} {
		answer, err := pyjsonGo(row.input, RootEnv(root))
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		exit, _ := field(answer, "exit")
		stdout, _ := field(answer, "stdout")
		if got, _ := exit.(float64); got != row.exit || stdout != row.stdout {
			t.Errorf("%s: exit %v stdout %q, want %v %q", row.name, exit, stdout, row.exit, row.stdout)
		}
	}
	refused, err := pyjsonGo(pyjson.Object{{Key: "text", Value: `{`}}, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	exitValue, _ := field(refused, "exit")
	if exit, err := integer(exitValue); err != nil || exit != 1 {
		t.Fatalf("a broken document answered exit %v (%v)", exitValue, err)
	}
	stderr, _ := field(refused, "stderr")
	if text, _ := stderr.(string); !strings.Contains(text, "Expecting property name") {
		t.Fatalf("the refusal text %q is not json.loads text", stderr)
	}
}

// The generated indents stay inside what json.tool can express.
func TestPyjsonGeneratedIndentsAreExpressible(t *testing.T) {
	for _, indent := range pyjsonIndents {
		if indent < 0 {
			t.Fatalf("indent %d is not a json.tool spelling the port can print", indent)
		}
	}
}

// pyjsonNesting is the deepest container nesting of one document text, counting only the containers
// the document actually opens.
func pyjsonNesting(text string) int {
	depth, deepest := 0, 0
	inText, escaped := false, false
	for _, r := range text {
		switch {
		case inText:
			switch {
			case escaped:
				escaped = false
			case r == '\\':
				escaped = true
			case r == '"':
				inText = false
			}
		case r == '"':
			inText = true
		case r == '{' || r == '[':
			depth++
			if depth > deepest {
				deepest = depth
			}
		case r == '}' || r == ']':
			depth--
		}
	}
	return deepest
}

// pyjsonRecursionBound and pyjsonDocumentByteBound are the two numbers the issue body names for the
// generator's deep shape: Python's default recursion limit (1000), and a byte cap the generator must
// respect by computing a shape's size before it builds one. The test states them itself so it pins the
// requirement rather than the generator's own constants (CRW-708 generation 5, d2).
const (
	pyjsonRecursionBound    = 1000
	pyjsonDocumentByteBound = 64 << 10
)

// The generator reaches the nesting the issue body names: a document nested near Python's recursion
// limit, with its total bytes computed and capped before it is built (CRW-708 generation 5, d2).
func TestPyjsonGenerateReachesTheRecursionBoundary(t *testing.T) {
	rng := rand.New(rand.NewSource(708))
	deepest, widest := 0, 0
	for i := 0; i < 4000 && deepest < pyjsonRecursionBound; i++ {
		input := pyjsonGenerate(rng, 1)
		value, _ := field(input, "text")
		text, _ := value.(string)
		if n := pyjsonNesting(text); n > deepest {
			deepest = n
		}
		if len(text) > widest {
			widest = len(text)
		}
	}
	if deepest < pyjsonRecursionBound {
		t.Fatalf("the deepest generated document nests %d levels, want at least %d", deepest, pyjsonRecursionBound)
	}
	if widest > pyjsonDocumentByteBound {
		t.Fatalf("a generated document holds %d bytes, past the %d-byte cap", widest, pyjsonDocumentByteBound)
	}
}
