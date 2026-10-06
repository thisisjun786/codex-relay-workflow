package delivery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-826: emit judges, inside the intake transaction, whether a ready_for_review receipt would
// leave its generation with no single head, and refuses the existing revision_ambiguous when it
// would, writing no event and no lineage row and naming the revision the generation reads now. The
// child knows whether it passed --supersedes-revision, so nothing here refuses what it could not
// have known (CRW-470). A first receipt, a duplicate, and a generation that already reads no single
// head are answered as they were: this judgment never makes an ambiguous generation worse.

// crw826Emit is one ready_for_review emit of the seed relationship into generation 1. With no
// socket the readiness claim stages, which is still a revision the head reads.
func crw826Emit(rid, turn, artifact string, extra ...string) []string {
	args := []string{"emit", "--relationship", rid, "--generation", "1", "--outcome", "ready_for_review",
		"--turn-thread", "01child-task", "--turn-id", turn, "--artifact", artifact}
	return append(args, extra...)
}

// crw826Side is the built crw over a copy of the seed with one artifact tree and the relationship
// id the seed registered.
func crw826Side(t *testing.T) (*cliSide, string, string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	side := newSide(t, work)
	rid := strings.Trim(sqliteDump(t, side, "SELECT relationship_id FROM relationships"), "[]\"\n ")
	return side, rid, work
}

// crw826Artifact writes one artifact under the side's tree.
func crw826Artifact(t *testing.T, work, name, text string) string {
	t.Helper()
	path := filepath.Join(work, name)
	mustDo(t, os.WriteFile(path, []byte(text), 0o644))
	return path
}

// crw826Receipt is the receipt of an accepted emit answer.
func crw826Receipt(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	receipt, ok := answer["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("the answer carries no receipt: %v", answer)
	}
	return receipt
}

// crw826Counts is the rows a refusal must not have added.
func crw826Counts(t *testing.T, side *cliSide) string {
	t.Helper()
	return sqliteDump(t, side, "SELECT (SELECT count(*) FROM events), (SELECT count(*) FROM revision_lineage)")
}

// crw826SeedRoot writes a reviewable root receipt of one generation straight into the store, the
// shape a child that emitted two roots before this change left behind. It is a read-write open of
// the side's own store file, never the live relay.
func crw826SeedRoot(t *testing.T, side *cliSide, eventID, revision string, generation int64, stage string, suppressed string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(side.state, "relay.sqlite3"), "")
	mustDo(t, err)
	defer func() { mustDo(t, s.Close()) }()
	rid := strings.Trim(sqliteDump(t, side, "SELECT relationship_id FROM relationships"), "[]\"\n ")
	var reason any
	if suppressed != "" {
		reason = suppressed
	}
	_, err = s.Querier(ctx).ExecContext(ctx, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, suppressed_reason, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?, 'ready_for_review', 'child', '01child-task', ?, 'completed', '{}', ?, ?, 'z', 'z')", eventID, rid, generation, revision, eventID, stage, reason)
	mustDo(t, err)
	_, err = s.Querier(ctx).ExecContext(ctx, "INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, declared_by, recorded_at) VALUES (?, ?, ?, ?, 'undeclared', 'z')", rid, generation, eventID, revision)
	mustDo(t, err)
}

// Red before the change: the second root is accepted and the generation reads a fork. After it: the
// receipt is refused revision_ambiguous, nothing is written, and the detail names the revision the
// generation reads now.
func TestCRW826_emit_refuses_a_second_root_that_would_fork_the_generation(t *testing.T) {
	t.Parallel()
	side, rid, work := crw826Side(t)
	first := crw826Artifact(t, work, "first.txt", "the first deliverable")
	second := crw826Artifact(t, work, "second.txt", "a second root")

	accepted, code := runJSON(t, side, crw826Emit(rid, dispatchTurn, first)...)
	if code != 0 || accepted["stage"] != "staged" {
		t.Fatalf("the first receipt: %d %v", code, accepted)
	}
	head := crw826Receipt(t, accepted)["revisionHash"].(string)
	if head == "" {
		t.Fatal("the first receipt carries no revision")
	}
	before := crw826Counts(t, side)

	refused, code := runJSON(t, side, crw826Emit(rid, dispatchTurn, second)...)
	if code != 2 || refused["reason"] != "revision_ambiguous" {
		t.Fatalf("a second root: %d %v, want exit 2 revision_ambiguous", code, refused)
	}
	detail, _ := refused["detail"].(string)
	for _, want := range []string{head, "--supersedes-revision"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("the refusal does not say %q:\n%s", want, detail)
		}
	}
	if after := crw826Counts(t, side); after != before {
		t.Fatalf("a refused emit wrote %s, want %s", after, before)
	}
}

// A revision that names the head is accepted and becomes the head; a later revision that names the
// revision the head already replaced is refused, because two revisions would declare one
// predecessor.
func TestCRW826_emit_refuses_a_revision_that_names_an_already_replaced_revision(t *testing.T) {
	t.Parallel()
	side, rid, work := crw826Side(t)
	first := crw826Artifact(t, work, "first.txt", "the first deliverable")
	second := crw826Artifact(t, work, "second.txt", "the second deliverable")
	third := crw826Artifact(t, work, "third.txt", "a third deliverable")

	accepted, code := runJSON(t, side, crw826Emit(rid, dispatchTurn, first)...)
	if code != 0 {
		t.Fatalf("the first receipt: %d %v", code, accepted)
	}
	head := crw826Receipt(t, accepted)["revisionHash"].(string)
	named, code := runJSON(t, side, crw826Emit(rid, dispatchTurn, second, "--supersedes-revision", head)...)
	if code != 0 {
		t.Fatalf("a revision naming the head: %d %v", code, named)
	}
	second_hash := crw826Receipt(t, named)["revisionHash"].(string)
	if second_hash == head {
		t.Fatal("the two deliverables share a revision")
	}
	refused, code := runJSON(t, side, crw826Emit(rid, dispatchTurn, third, "--supersedes-revision", head)...)
	if code != 2 || refused["reason"] != "revision_ambiguous" {
		t.Fatalf("a second revision naming a replaced one: %d %v", code, refused)
	}
}

// The same receipt again is a duplicate, answered as it was and never refused.
func TestCRW826_emit_answers_a_duplicate_without_refusing_it(t *testing.T) {
	t.Parallel()
	side, rid, work := crw826Side(t)
	first := crw826Artifact(t, work, "first.txt", "the first deliverable")
	args := crw826Emit(rid, dispatchTurn, first)
	if _, code := runJSON(t, side, args...); code != 0 {
		t.Fatalf("the first receipt: %d", code)
	}
	again, code := runJSON(t, side, args...)
	if code != 0 || again["duplicate"] != true {
		t.Fatalf("a duplicate emit: %d %v", code, again)
	}
}

// A generation that already reads no single head is left as it is: this judgment never makes an
// ambiguous generation worse.
func TestCRW826_emit_accepts_a_receipt_into_an_already_ambiguous_generation(t *testing.T) {
	t.Parallel()
	side, rid, work := crw826Side(t)
	crw826SeedRoot(t, side, "evt-crw826-root-a", strings.Repeat("a", 64), 1, "final", "")
	crw826SeedRoot(t, side, "evt-crw826-root-b", strings.Repeat("b", 64), 1, "final", "")
	third := crw826Artifact(t, work, "third.txt", "a third root")
	accepted, code := runJSON(t, side, crw826Emit(rid, dispatchTurn, third)...)
	if code != 0 {
		t.Fatalf("a receipt into an already ambiguous generation: %d %v", code, accepted)
	}
}

// A receipt that names a suppressed receipt of its own generation is accepted: the head reads the
// naming through the suppressed receipt (CRW-470), so the receipt is a root and not a fork.
func TestCRW826_emit_accepts_a_receipt_that_names_a_suppressed_receipt(t *testing.T) {
	t.Parallel()
	side, rid, work := crw826Side(t)
	held := strings.Repeat("c", 64)
	crw826SeedRoot(t, side, "evt-crw826-suppressed", held, 1, "suppressed", "the turn ended interrupted")
	first := crw826Artifact(t, work, "first.txt", "the deliverable after a restart")
	accepted, code := runJSON(t, side, crw826Emit(rid, dispatchTurn, first, "--supersedes-revision", held)...)
	if code != 0 {
		t.Fatalf("a receipt naming a suppressed receipt: %d %v", code, accepted)
	}
}

// The first receipt of a generation, and the first receipt of a correction generation naming its
// requested predecessor, are accepted: neither generation holds a revision this receipt could fork.
func TestCRW826_emit_accepts_a_first_receipt_of_a_generation(t *testing.T) {
	t.Parallel()
	side, rid, work := crw826Side(t)
	first := crw826Artifact(t, work, "first.txt", "the first deliverable")
	accepted, code := runJSON(t, side, crw826Emit(rid, dispatchTurn, first)...)
	if code != 0 || accepted["stage"] != "staged" {
		t.Fatalf("the first receipt of generation 1: %d %v", code, accepted)
	}
	head := crw826Receipt(t, accepted)["revisionHash"].(string)
	opened, code := runJSON(t, side, "generation-open", "--relationship", rid, "--dispatch-request-id", "revision-crw826", "--reason", "needs_changes_revision", "--dispatch-turn-id", "turn-revision-crw826")
	if code != 0 {
		t.Fatalf("generation-open: %d %v", code, opened)
	}
	second := crw826Artifact(t, work, "second.txt", "the corrected deliverable")
	corrected, code := runJSON(t, side, "emit", "--relationship", rid, "--generation", "2", "--outcome", "ready_for_review",
		"--turn-thread", "01child-task", "--turn-id", "turn-revision-crw826", "--artifact", second, "--supersedes-revision", head)
	if code != 0 {
		t.Fatalf("the first receipt of a correction generation: %d %v", code, corrected)
	}
}
