package execution

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// A policy refusal quotes a name from the file as JSON: a quote kept, a backslash doubled. A name
// escaped as a lone surrogate is that code point, as the policy keeps it, and two names escaped
// as different surrogates stay two names. The refusals are the golden.
func TestAPolicyRefusalQuotesANameAsJSON(t *testing.T) {
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
