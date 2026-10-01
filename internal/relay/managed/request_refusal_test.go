package managed

import (
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// A managed request's missing and unknown fields are named, each quoted, so a key holding a
// quote, a backslash or a character that does not print reads unambiguously. A request holding a
// lone surrogate escape anywhere is refused before any field is judged. Each refusal's text is the
// golden.
func TestAnUnknownRequestFieldIsNamed(t *testing.T) {
	var documents []string
	for _, key := range []string{"it's", `a\b`, "x\u00a0y", "x\u2028y", "x\u200by", `say "it's"`} {
		raw, err := json.Marshal(map[string]any{key: 1, "b'": 2})
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, string(raw))
	}
	documents = append(documents,
		`{"s\udcff": 1, "b'": 2}`,
		`{"a": "x", "b": ["\ud800\udc00", "y\udcff\udcfe"]}`,
		`{"k\n\u00e9\"": 1e300, "v": [true, null, -0, 12345678901234567890], "w": "\u0001\udfff"}`,
		`{"schema": "managed-start/1", "requestId": "\udcff", "requestId": "r"}`)
	var refusals []string
	for _, document := range documents {
		_, err := ParseRequest([]byte(document))
		if err == nil {
			t.Fatalf("%s was accepted", document)
		}
		refusals = append(refusals, err.Error())
	}
	golden.CheckJSON(t, "parse_request", refusals)
}
