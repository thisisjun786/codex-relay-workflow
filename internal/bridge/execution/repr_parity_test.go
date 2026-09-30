package execution

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// A policy refusal quotes a name from the file as Python's f"{name!r}" does: its quote choice,
// a backslash doubled, and every character str.isprintable() refuses (U+00A0, U+2028, U+200B)
// escaped. A name escaped as a lone surrogate is that code point, as json.loads keeps it, so it
// is quoted as '\udcff' and two names escaped as different surrogates stay two names. The
// refusals are the golden, which began as ExecutionPolicy.from_bytes's ExecutionPolicyError for
// the same bytes.
func TestAPolicyRefusalQuotesANameAsPythonReprsIt(t *testing.T) {
	documents := []string{
		`{"roles": {"x\u00a0y": {}}}`,
		`{"roles": {"it's": {}}}`,
		`{"roles": {"a\\b": {}}}`,
		`{"roles": {"l\u2028s\u200b": {}}}`,
		`{"allowed": {}, "a\\b\u00a0": 1, "a\\b\u00a0": 2}`,
		`{"roles": {"s\udcff": {}}}`,
		`{"roles": {"s\udcff": {}, "s\udcfe": {}}}`,
		`{"roles": {"\ud800x\udfff": {}}}`,
		`{"allowed": {}, "\udcff": 1, "\udcff": 2}`,
	}
	refusals := make([]*string, len(documents))
	for i, document := range documents {
		_, err := FromBytes([]byte(document), "p")
		if err == nil {
			t.Errorf("%s: accepted", document)
			continue
		}
		refusal := err.Error()
		refusals[i] = &refusal
	}
	golden.CheckJSON(t, "refusals", refusals)
}
