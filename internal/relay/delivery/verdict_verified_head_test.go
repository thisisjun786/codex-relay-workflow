package delivery

// CRW-742: verdict --verified-head. A verified ruling fixes the head it handled, in the ruling's own
// transaction, so a later dag-accept proves a parent-made refresh against that record instead of the
// free head the forge shows at accept time (the post-merge finding P1-1 of the CRW-666 pull request).
// The row lives in the DAG zone's dag_verified_heads, which CRW-728 shipped; this file drives the
// writer and never appends or edits a zone statement.

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// vhHead is one well-formed 40-hex head; vhOther is a different head for the same event.
const (
	vhHead  = "0123456789abcdef0123456789abcdef01234567"
	vhOther = "89abcdef0123456789abcdef0123456789abcdef"
)

type vh struct {
	*fixture
	ack *Ack
}

func newVH(t *testing.T) *vh {
	f := newFixture(t, "")
	return &vh{fixture: f, ack: NewAck(f.delivery)}
}

// ruled brings the fixture to the state a verdict needs: a registered relationship whose current
// revision is a final, unsuppressed ready_for_review receipt the host acknowledged and verified.
func (v *vh) ruled() (string, string) {
	v.t.Helper()
	rid := v.register(regOpts{recipients: []string{parent, child}})
	payload := v.readyPayload(rid, 1, []string{v.artifact("out.txt", "the deliverable")}, 1, assigned("completed"))
	_, err := v.accept(payload, store.AcceptOptions{})
	mustDo(v.t, err)
	event := pyjson.Text(payload.Get("eventId"))
	_, err = v.delivery.Enqueue(v.ctx, event, "", "")
	mustDo(v.t, err)
	v.mustAttempt(event, nil)
	v.clock.Advance(5)
	turn := v.host.startTurn(parent, "ack-turn", "inProgress", "")
	_, err = v.ack.Acknowledge(v.ctx, event, turn.TurnID, AckProof(event, turn.TurnID), true, nil, v.host)
	mustDo(v.t, err)
	return rid, event
}

// heads reads the dag_verified_heads rows of an event, oldest first.
func (v *vh) heads(event string) []Row {
	v.t.Helper()
	rows, err := all(v.ctx, v.store, "SELECT * FROM dag_verified_heads WHERE event_id = ?", event)
	mustDo(v.t, err)
	return rows
}

// advance opens the next execution generation, so the event is no longer the head of the one the
// relationship stands on and the currency check refuses a verified ruling.
func (v *vh) advance() {
	v.t.Helper()
	mustDo(v.t, v.store.Transaction(v.ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := OpenGenerationIn(ctx, v.store, v.clock, v.rid, "vh-newer-execution", "needs_changes_revision", "vh-newer-turn")
		return err
	}))
}

// A verified ruling given a head records exactly one dag_verified_heads row, in the ruling's own
// transaction: the event, the relationship, the event's generation, the verdict turn, the head, the
// recorder (the relationship's registered parent) and the ruling's own time.
func TestVerdictVerifiedHeadIsRecordedWithTheRuling(t *testing.T) {
	t.Parallel()
	v := newVH(t)
	rid, event := v.ruled()
	record, err := v.ack.RecordVerdict(v.ctx, event, "verified", "verdict-turn-1", nil, nil, nil, nil, vhHead)
	mustDo(t, err)
	if got := pyjson.Text(record.Get("verdict")); got != "verified" {
		t.Fatalf("verdict = %q", got)
	}
	rows := v.heads(event)
	if len(rows) != 1 {
		t.Fatalf("dag_verified_heads rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if got := row.S("relationship_id"); got != rid {
		t.Fatalf("relationship_id = %q, want %q", got, rid)
	}
	if got := row.I("execution_generation"); got != 1 {
		t.Fatalf("execution_generation = %d, want 1", got)
	}
	if got := row.S("verdict_turn_id"); got != "verdict-turn-1" {
		t.Fatalf("verdict_turn_id = %q", got)
	}
	if got := row.S("head_sha"); got != vhHead {
		t.Fatalf("head_sha = %q, want %q", got, vhHead)
	}
	if got := row.S("recorded_by_task_id"); got != parent {
		t.Fatalf("recorded_by_task_id = %q, want the registered parent %q", got, parent)
	}
	if got, want := row.S("recorded_at"), v.clock.ISO(); got != want {
		t.Fatalf("recorded_at = %q, want the ruling's own time %q", got, want)
	}
}

// The same verified ruling with the same head again is a replay: the recorded record marked _replay,
// and still exactly one row.
func TestVerdictVerifiedHeadSameHeadIsAReplayWithOneRow(t *testing.T) {
	t.Parallel()
	v := newVH(t)
	_, event := v.ruled()
	_, err := v.ack.RecordVerdict(v.ctx, event, "verified", "verdict-turn-1", nil, nil, nil, nil, vhHead)
	mustDo(t, err)
	again, err := v.ack.RecordVerdict(v.ctx, event, "verified", "verdict-turn-1", nil, nil, nil, nil, vhHead)
	mustDo(t, err)
	if !truthy(again.Get("_replay")) {
		t.Fatalf("a repeated verified ruling with the same head is not marked _replay: %v", again)
	}
	rows := v.heads(event)
	if len(rows) != 1 {
		t.Fatalf("dag_verified_heads rows = %d, want 1", len(rows))
	}
	if got := rows[0].S("head_sha"); got != vhHead {
		t.Fatalf("head_sha = %q, want the first %q", got, vhHead)
	}
}

// Another head for the same event is refused disposition_conflict, and the recorded row stays.
func TestVerdictVerifiedHeadAnotherHeadIsRefusedAndTheRowStays(t *testing.T) {
	t.Parallel()
	v := newVH(t)
	_, event := v.ruled()
	_, err := v.ack.RecordVerdict(v.ctx, event, "verified", "verdict-turn-1", nil, nil, nil, nil, vhHead)
	mustDo(t, err)
	_, err = v.ack.RecordVerdict(v.ctx, event, "verified", "verdict-turn-1", nil, nil, nil, nil, vhOther)
	requireReason(t, err, DispositionConflict)
	rows := v.heads(event)
	if len(rows) != 1 || rows[0].S("head_sha") != vhHead {
		t.Fatalf("the recorded head did not stay: %v", rows)
	}
}

// The flag with a verdict that is not verified is refused disposition_conflict, and neither a verdict
// nor a row is recorded. Every other verdict of the frozen enum is covered, not only the one that
// would otherwise open a correction.
func TestVerdictVerifiedHeadOnlyWithAVerifiedRuling(t *testing.T) {
	t.Parallel()
	for _, verdict := range []string{"needs_changes", "unverified", "aborted"} {
		t.Run(verdict, func(t *testing.T) {
			t.Parallel()
			v := newVH(t)
			_, event := v.ruled()
			var findings []any
			var reason any
			if verdict == "needs_changes" {
				findings = []any{finding("c1", "needs_changes", "fix it")}
			} else {
				reason = "the check could not conclude"
			}
			_, err := v.ack.RecordVerdict(v.ctx, event, verdict, "verdict-turn-1", nil, findings, reason, nil, vhHead)
			requireReason(t, err, DispositionConflict)
			if n := v.count("SELECT COUNT(*) AS c FROM verdicts WHERE event_id = ?", event); n != 0 {
				t.Fatalf("a refused %s ruling recorded %d verdicts, want 0", verdict, n)
			}
			if rows := v.heads(event); len(rows) != 0 {
				t.Fatalf("a refused %s ruling recorded %d heads, want 0", verdict, len(rows))
			}
		})
	}
}

// A ruling the existing currency check refuses records no head either: the row is written with the
// ruling that stands, not with one that was turned away.
func TestVerdictVerifiedHeadIsNotRecordedWhenTheRulingIsRefused(t *testing.T) {
	t.Parallel()
	v := newVH(t)
	_, event := v.ruled()
	// a newer generation makes the event not the head, so a verified ruling is refused
	v.advance()
	_, err := v.ack.RecordVerdict(v.ctx, event, "verified", "verdict-turn-1", nil, nil, nil, nil, vhHead)
	requireReason(t, err, StaleGeneration)
	if rows := v.heads(event); len(rows) != 0 {
		t.Fatalf("a ruling refused for currency recorded %d heads, want 0", len(rows))
	}
}

// Without the flag a verified ruling is exactly what it was: the answer carries no new field, and no
// dag_verified_heads row appears.
func TestVerdictWithoutTheHeadIsUnchanged(t *testing.T) {
	t.Parallel()
	v := newVH(t)
	_, event := v.ruled()
	record, err := v.ack.RecordVerdict(v.ctx, event, "verified", "verdict-turn-1", nil, nil, nil, nil)
	mustDo(t, err)
	want := []string{"eventId", "relationshipId", "executionGeneration", "verdict", "verdictTurnId", "decidedAt"}
	var got []string
	for _, f := range record {
		got = append(got, f.Key)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the verdict answer changed: %v, want %v", got, want)
	}
	if rows := v.heads(event); len(rows) != 0 {
		t.Fatalf("dag_verified_heads rows = %d, want none without the flag", len(rows))
	}
}

// A replay cannot attach a head to a ruling that fixed none: the head is recorded with the ruling
// that fixes it, and a replay writes nothing, so the answer is a refusal and no row appears.
func TestVerdictVerifiedHeadCannotBeAddedByAReplay(t *testing.T) {
	t.Parallel()
	v := newVH(t)
	_, event := v.ruled()
	_, err := v.ack.RecordVerdict(v.ctx, event, "verified", "verdict-turn-1", nil, nil, nil, nil)
	mustDo(t, err)
	_, err = v.ack.RecordVerdict(v.ctx, event, "verified", "verdict-turn-1", nil, nil, nil, nil, vhHead)
	requireReason(t, err, DispositionConflict)
	if rows := v.heads(event); len(rows) != 0 {
		t.Fatalf("a refused replay recorded %d heads, want 0", len(rows))
	}
}

// A re-review ruled verified under a re-registered criteria set keeps the head the event already
// records: it writes no second row, and it is refused for another head.
func TestVerdictVerifiedHeadOnAReReviewKeepsTheRecordedHead(t *testing.T) {
	t.Parallel()
	v := newVH(t)
	rid, event := v.ruled()
	criteria := v.ack.Criteria
	first, err := criteria.Register(v.ctx, rid, []any{
		Obj{{Key: "id", Value: "c1"}, {Key: "title", Value: "the endpoint returns the agreed shape"}, {Key: "required", Value: false}},
	}, nil)
	mustDo(t, err)
	if _, err := v.ack.RecordVerdict(v.ctx, event, "verified", "verdict-turn-1", nil, nil, nil, first.Get("setDigest"), vhHead); err != nil {
		t.Fatal(err)
	}
	// the criteria set moves, so the next ruling is a re-review, and it is still a verified ruling
	moved, err := criteria.Register(v.ctx, rid, []any{
		Obj{{Key: "id", Value: "c1"}, {Key: "title", Value: "the endpoint returns the agreed shape"}, {Key: "required", Value: false}},
		Obj{{Key: "id", Value: "c2"}, {Key: "title", Value: "a malformed request is refused"}, {Key: "required", Value: false}},
	}, nil)
	mustDo(t, err)
	if _, err := v.ack.RecordVerdict(v.ctx, event, "verified", "verdict-turn-2", nil, nil, nil, moved.Get("setDigest"), vhHead); err != nil {
		t.Fatal(err)
	}
	rows := v.heads(event)
	if len(rows) != 1 {
		t.Fatalf("a re-review wrote %d heads, want the one the first ruling fixed", len(rows))
	}
	if got := rows[0].S("verdict_turn_id"); got != "verdict-turn-1" {
		t.Fatalf("verdict_turn_id = %q, want the turn that fixed the head", got)
	}
	_, err = v.ack.RecordVerdict(v.ctx, event, "verified", "verdict-turn-3", nil, nil, nil, nil, vhOther)
	requireReason(t, err, DispositionConflict)
	if rows := v.heads(event); len(rows) != 1 || rows[0].S("head_sha") != vhHead {
		t.Fatalf("a refused re-review changed the head: %v", rows)
	}
}

// The flag is on the command line: verdict's help names it, and a value that is not 40 lowercase hex
// characters ends in the option-value rejection path this command already uses (the usage envelope,
// exit 4), never in the writer.
func TestVerdictVerifiedHeadIsACommandLineFlag(t *testing.T) {
	work := filepath.Join(parityTree(t), "work")
	side := newSide(t, work)
	help, code := side.run("verdict", "--help")
	if code != contract.ExitOk || !strings.Contains(help, "--verified-head") {
		t.Fatalf("verdict --help (exit %d) does not name --verified-head: %s", code, help)
	}
	for _, bad := range []string{"", "0123456789abcdef0123456789abcdef0123456", "0123456789ABCDEF0123456789abcdef01234567", "zz23456789abcdef0123456789abcdef01234567", " " + vhHead, vhHead + " "} {
		_, code := side.run("verdict", "--event", "4a7c8d2e7b0b06e7e2b4b71c55f2b7c1", "--verdict", "verified", "--verdict-turn", "v", "--verified-head", bad)
		if code != contract.ExitUsage {
			t.Fatalf("--verified-head %q: exit %d, want %d", bad, code, contract.ExitUsage)
		}
	}
}
