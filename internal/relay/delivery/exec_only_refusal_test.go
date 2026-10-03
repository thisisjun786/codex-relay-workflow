package delivery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
)

// CRW-500: the way out of a manifest_forbidden refusal, followed through the built `crw relay`.
// A child that attaches a file (--artifact) to an execution-only outcome (blocked_needs_input,
// failed, interrupted) is refused manifest_forbidden. It used to be told only the rule, read that
// as "receipt not emitted" and ended its turn with nothing sent to its parent. The refusal now
// names the correction, and the corrected emit is the same command without the file.

func TestCRW500_an_execution_only_emit_that_carries_a_file_is_told_how_to_correct_it(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, outcome string
		status        []string
	}{
		{name: "blocked_needs_input", outcome: "blocked_needs_input", status: []string{"--turn-status", "inProgress"}},
		// The form the child packet gives: emit takes --turn-status inProgress when it is left out.
		{name: "blocked_needs_input_without_a_turn_status", outcome: "blocked_needs_input"},
		{name: "failed", outcome: "failed", status: []string{"--turn-status", "failed"}},
		{name: "interrupted", outcome: "interrupted", status: []string{"--turn-status", "interrupted"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			work := filepath.Join(t.TempDir(), "work")
			side := newSide(t, work)
			blocked := filepath.Join(work, "blocked.md")
			mustDo(t, os.WriteFile(blocked, []byte("what stopped this child, and what it needs"), 0o644))
			rid := strings.Trim(sqliteDump(t, side, "SELECT relationship_id FROM relationships"), "[]\"\n ")
			emitted := func() string { return sqliteDump(t, side, "SELECT count(*) FROM events") }
			base := append([]string{"emit", "--relationship", rid, "--generation", "1", "--outcome", c.outcome, "--turn-thread", child, "--turn-id", dispatchTurn}, c.status...)

			// The child attaches its blocked file: refused, and told how to correct the emit.
			refused, code := runJSON(t, side, append(append([]string(nil), base...), "--artifact", blocked)...)
			detail, _ := refused["detail"].(string)
			if code != 2 || refused["reason"] != "manifest_forbidden" {
				t.Fatalf("an %s emit carrying a file: %d %v", c.outcome, code, refused)
			}
			if !strings.Contains(detail, "carries no manifest") {
				t.Fatalf("the refusal no longer states the rule: %q", detail)
			}
			for _, want := range []string{"without --artifact", "blocked file", "final message", "ready_for_review"} {
				if !strings.Contains(detail, want) {
					t.Fatalf("the refusal does not tell the child %q:\n%s", want, detail)
				}
			}
			help := argparse.Help("crw relay", "emit")
			for _, flag := range flagToken.FindAllString(detail, -1) {
				if !strings.Contains(help, flag) {
					t.Fatalf("the refusal names %s, which emit does not take:\n%s", flag, help)
				}
			}
			if got := emitted(); got != "[[0]]\n" {
				t.Fatalf("a refused emit stored %s receipt events", got)
			}
			if got := sqliteDump(t, side, "SELECT reason FROM refusals"); got != `[["manifest_forbidden"]]`+"\n" {
				t.Fatalf("the refusal ledger holds %s", got)
			}

			// The correction the refusal names: the same emit without the file is accepted.
			accepted, code := runJSON(t, side, base...)
			receipt, _ := accepted["receipt"].(map[string]any)
			if code != 0 || receipt["outcome"] != c.outcome || receipt["revisionHash"] != strings.Repeat("0", 64) || receipt["manifest"] != nil {
				t.Fatalf("the emit without the file: %d %v", code, accepted)
			}
			if got := emitted(); got != "[[1]]\n" {
				t.Fatalf("the corrected emit stored %s receipt events", got)
			}
		})
	}
}

// The other way out the refusal names: a file that must travel goes with ready_for_review.
func TestCRW500_a_file_that_must_travel_goes_with_ready_for_review(t *testing.T) {
	t.Parallel()
	work := filepath.Join(t.TempDir(), "work")
	side := newSide(t, work)
	handoff := filepath.Join(work, "handoff.md")
	mustDo(t, os.WriteFile(handoff, []byte("the deliverable"), 0o644))
	rid := strings.Trim(sqliteDump(t, side, "SELECT relationship_id FROM relationships"), "[]\"\n ")
	accepted, code := runJSON(t, side, "emit", "--relationship", rid, "--generation", "1", "--outcome", "ready_for_review", "--turn-thread", child, "--turn-id", dispatchTurn, "--artifact", handoff)
	receipt, _ := accepted["receipt"].(map[string]any)
	if code != 0 || accepted["stage"] != "staged" || receipt["outcome"] != "ready_for_review" || receipt["manifest"] == nil {
		t.Fatalf("ready_for_review carrying the file: %d %v", code, accepted)
	}
}
