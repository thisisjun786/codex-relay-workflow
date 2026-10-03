package delivery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
)

// CRW-255: the way out of an unassigned_turn refusal, followed through the built `crw relay`.
// A child that finishes on a turn the relay never admitted (a goal-continuation turn of a Loop, a
// turn a restarted App Server opened) is refused unassigned_turn; the refusal names the flags
// that state a continuation claim, the anchor to name is read from the relay (`status`), and the
// claim admits the turn. A foreign thread is refused whatever it claims and is not told to claim.

var flagToken = regexp.MustCompile(`--[a-z][a-z-]*`)

func runJSON(t *testing.T, side *cliSide, args ...string) (map[string]any, int) {
	t.Helper()
	out, code := side.run(args...)
	var parsed map[string]any
	if strings.TrimSpace(out) != "" {
		mustDo(t, json.Unmarshal([]byte(out), &parsed))
	}
	return parsed, code
}

func TestCRW255_a_later_turn_of_the_child_is_told_how_to_continue_and_is_admitted_by_the_claim(t *testing.T) {
	t.Parallel()
	work := filepath.Join(t.TempDir(), "work")
	side := newSide(t, work)
	artifact := filepath.Join(work, "out.txt")
	mustDo(t, os.WriteFile(artifact, []byte("finished in a later turn"), 0o644))
	rid := strings.Trim(sqliteDump(t, side, "SELECT relationship_id FROM relationships"), "[]\"\n ")
	admissions := func() string {
		return sqliteDump(t, side, "SELECT turn_id, evidence FROM generation_turns ORDER BY turn_id")
	}
	emit := func(generation, thread, turn string, extra ...string) (map[string]any, int) {
		args := append([]string{"emit", "--relationship", rid, "--generation", generation, "--outcome", "ready_for_review", "--turn-thread", thread, "--turn-id", turn, "--artifact", artifact}, extra...)
		return runJSON(t, side, args...)
	}
	claim := func(anchor string) []string {
		return []string{"--continues-anchor", anchor, "--continuation-actor", child, "--continuation-reason", "goal-continuation turn of the same managed child"}
	}
	// The anchor is read from the relay, for the generation the child emits under.
	anchorOf := func() string {
		status, code := runJSON(t, side, "status", "--relationship", rid)
		anchor, _ := status["observation"].(map[string]any)["anchors"].(map[string]any)[rid].(map[string]any)["turnId"].(string)
		if code != 0 || anchor == "" {
			t.Fatalf("status gave no anchor for %s: %d %v", rid, code, status)
		}
		return anchor
	}
	// namesAnchor: the refusal's instruction (not its description of the anchor) carries it.
	namesAnchor := func(detail, anchor string) bool {
		return regexp.MustCompile(`--continues-anchor\s+'?` + regexp.QuoteMeta(anchor) + `'?`).MatchString(detail)
	}

	anchor := anchorOf()
	if anchor != dispatchTurn {
		t.Fatalf("generation 1 is anchored to %q, want %q", anchor, dispatchTurn)
	}

	// The refusal tells the child how to continue, with flags emit really has and the anchor the
	// relay reports.
	refused, code := emit("1", child, "turn-loop-5")
	detail, _ := refused["detail"].(string)
	if code != 2 || refused["reason"] != "unassigned_turn" || !namesAnchor(detail, anchor) {
		t.Fatalf("a later turn: %d %v", code, refused)
	}
	help := argparse.Help("crw relay", "emit")
	for _, flag := range flagToken.FindAllString(detail, -1) {
		if !strings.Contains(help, flag) {
			t.Fatalf("the refusal names %s, which emit does not take:\n%s", flag, help)
		}
	}
	if admissions() != "[]\n" {
		t.Fatalf("a refused emit admitted %s", admissions())
	}

	// A foreign thread is refused whatever it claims, is not told to claim and admits nothing.
	foreign, code := emit("1", "someone-else", "turn-loop-5", claim(anchor)...)
	foreignDetail, _ := foreign["detail"].(string)
	if code != 2 || foreign["reason"] != "unassigned_turn" || strings.Contains(foreignDetail, "--continu") || admissions() != "[]\n" {
		t.Fatalf("a foreign thread: %d %v, admissions %s", code, foreign, admissions())
	}

	// A claim naming another anchor, from a turn not yet admitted, is refused and answered by the
	// anchor the generation really has; it is not told to claim again.
	wrong, code := emit("1", child, "turn-loop-6", claim("some-other-execution")...)
	wrongDetail, _ := wrong["detail"].(string)
	if code != 2 || wrong["reason"] != "unassigned_turn" || !strings.Contains(wrongDetail, "is anchored to '"+anchor+"'") || strings.Contains(wrongDetail, "--continuation-actor") || admissions() != "[]\n" {
		t.Fatalf("a wrong anchor: %d %v, admissions %s", code, wrong, admissions())
	}

	// The claim the relay reported admits the turn, which the relay then records as unverified.
	accepted, code := emit("1", child, "turn-loop-5", claim(anchor)...)
	if code != 0 || accepted["stage"] != "staged" || accepted["receipt"].(map[string]any)["outcome"] != "ready_for_review" {
		t.Fatalf("the claim: %d %v", code, accepted)
	}
	if got := admissions(); got != `[["turn-loop-5", "explicit_admission_bound:`+anchor+`"]]`+"\n" {
		t.Fatalf("admissions %s", got)
	}
	if detail := sqliteDump(t, side, "SELECT detail FROM generation_turns"); !strings.Contains(detail, "corroboration=not_corroborated") {
		t.Fatalf("the claim was recorded as %s", detail)
	}

	// An admitted turn needs no claim again within its generation, whatever it emits next.
	secondFile := filepath.Join(work, "second.txt")
	mustDo(t, os.WriteFile(secondFile, []byte("a second deliverable"), 0o644))
	again, code := emit("1", child, "turn-loop-5", "--artifact", secondFile)
	if code != 0 || again["stage"] != "staged" {
		t.Fatalf("an admitted turn emitting again: %d %v", code, again)
	}

	// A needs_changes generation has its own anchor: status reports it, the claim must name it,
	// and neither the first generation's anchor nor a turn admitted for the first generation
	// carries over.
	opened, code := runJSON(t, side, "generation-open", "--relationship", rid, "--dispatch-request-id", "revision-1", "--reason", "needs_changes_revision", "--dispatch-turn-id", "turn-revision-1")
	if code != 0 {
		t.Fatalf("generation-open: %d %v", code, opened)
	}
	if anchor2 := anchorOf(); anchor2 != "turn-revision-1" {
		t.Fatalf("generation 2 is anchored to %q", anchor2)
	}
	stale, code := emit("2", child, "turn-loop-9", claim(anchor)...)
	staleDetail, _ := stale["detail"].(string)
	if code != 2 || !strings.Contains(staleDetail, "generation 2 is anchored to 'turn-revision-1'") {
		t.Fatalf("generation 1's anchor in generation 2: %d %v", code, stale)
	}
	carried, code := emit("2", child, "turn-loop-5")
	carriedDetail, _ := carried["detail"].(string)
	if code != 2 || !namesAnchor(carriedDetail, "turn-revision-1") {
		t.Fatalf("a turn admitted for generation 1 in generation 2: %d %v", code, carried)
	}
	second, code := emit("2", child, "turn-loop-9", claim("turn-revision-1")...)
	if code != 0 || second["stage"] != "staged" {
		t.Fatalf("generation 2's claim: %d %v", code, second)
	}
}
