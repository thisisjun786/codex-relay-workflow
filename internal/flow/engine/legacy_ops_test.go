package engine_test

import (
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// TestLegacyReviewerOpsMatchTheOpsConstants (CRW-1429): the engine cannot import ops, so the legacy refusal holds its own copies of the
// retired ops and rule ids; they are the strings ops records under.
func TestLegacyReviewerOpsMatchTheOpsConstants(t *testing.T) {
	for _, c := range []struct{ engine, ops string }{
		{engine.OpLegacyReview, ops.OpReview},
		{engine.OpLegacyReviewerFinal, ops.OpReviewerFinal},
		{engine.OpLegacyArchive, ops.OpArchive},
		{engine.OpLegacyReleaseChild, ops.OpReleaseChild},
	} {
		if c.engine != c.ops {
			t.Errorf("engine %q, ops %q", c.engine, c.ops)
		}
	}
	if want := []string{ops.RuleRev1, ops.RuleRev2, ops.RuleRevF1, ops.RuleExpl3}; !slices.Equal(engine.LegacyRetiredRules, want) {
		t.Errorf("engine.LegacyRetiredRules %v, ops %v", engine.LegacyRetiredRules, want)
	}
}
