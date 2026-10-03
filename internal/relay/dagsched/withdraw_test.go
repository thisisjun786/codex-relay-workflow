package dagsched

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// What the relay can show was used keeps a generation from being withdrawn, and a refusal writes nothing: no record, the pointer where it was, no journal row.
func TestWithdrawalRefusesAGenerationThatWasUsed(t *testing.T) {
	now := "2026-10-02T00:00:00Z"
	cases := []struct {
		name, reason, detail string
		generation           int64
		node, actor, why     string
		epoch                int64
		setup                func(k *withdrawKit)
	}{
		{name: "bound to a dispatch turn", reason: "disposition_conflict", detail: "bound to a dispatch turn", setup: func(k *withdrawKit) {
			if _, err := (&registry.Registry{Store: k.s}).BindAnchor(context.Background(), k.rid, 2, "turn-of-generation-2", "dispatch_receipt"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a report of the child", reason: "disposition_conflict", detail: "carries an event", setup: func(k *withdrawKit) { k.rvReportGeneration(k.rid, "I", 2, k.criteria) }},
		{name: "a reply or a request of the relay", reason: "disposition_conflict", detail: "carries an event", setup: func(k *withdrawKit) {
			k.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
				" VALUES ('evt-reply', ?, 2, ?, 'decision_reply', 'relay', 'parent', 'turn-r', 'completed', '{}', 'final', ?, ?)", k.rid, dig("reply"), now, now)
		}},
		{name: "a turn admitted to it", reason: "disposition_conflict", detail: "an admitted turn", setup: func(k *withdrawKit) {
			k.exec("INSERT INTO generation_turns (relationship_id, execution_generation, turn_id, evidence, actor, detail, admitted_at) VALUES (?, 2, 'turn-admitted', 'continuation', 'child', NULL, ?)", k.rid, now)
		}},
		{name: "an execution of the node recorded on it (dag-correct, dag-adopt)", reason: "disposition_conflict", detail: "a recorded execution of a node", setup: func(k *withdrawKit) {
			k.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind) SELECT plan_id, node_id, relationship_id, 2, manifest_digest, 'parent_handover' FROM dag_node_executions WHERE relationship_id = ?", k.rid)
		}},
		{name: "opened by a needs_changes ruling", reason: "disposition_conflict", detail: "a needs_changes ruling that opened it", setup: func(k *withdrawKit) {
			k.exec("UPDATE verdicts SET next_generation = 2 WHERE event_id = ?", k.event1)
		}},
		{name: "not the generation the relationship stands on", reason: "stale_generation", generation: 3, detail: "stands on"},
		{name: "generation 1 is the assignment", reason: "malformed_receipt", generation: 1},
		{name: "a reason is required", reason: "malformed_receipt", why: "  "},
		{name: "another task", reason: "scope_role_mismatch", actor: "intruder"},
		{name: "a node the plan does not have", reason: "unregistered_scope", node: "Z"},
		{name: "a relationship that is paused", reason: "relationship_not_active", setup: func(k *withdrawKit) {
			k.exec("UPDATE relationships SET status = 'paused' WHERE relationship_id = ?", k.rid)
		}},
		{name: "an epoch the session does not hold", reason: "stale_coordinator_epoch", epoch: 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newWithdrawKit(t)
			k.openUnsent()
			if c.setup != nil {
				c.setup(k)
			}
			journal := k.count("SELECT COUNT(*) FROM journal")
			rows := k.rows()
			generations := k.generations()
			k.sched.ExpectedEpoch = c.epoch
			in := WithdrawInput{Generation: 2, Reason: "refreshed the base myself"}
			if c.generation != 0 {
				in.Generation = c.generation
			}
			if c.why != "" {
				in.Reason = c.why
			}
			node, actor := "I", "parent"
			if c.node != "" {
				node = c.node
			}
			if c.actor != "" {
				actor = c.actor
			}
			_, err := k.sched.WithdrawGeneration(context.Background(), "g", node, actor, in)
			if refusalReason(err) != c.reason || (c.detail != "" && !strings.Contains(err.Error(), c.detail)) {
				t.Fatalf("withdraw = %v, want %s naming %q", err, c.reason, c.detail)
			}
			if k.withdrawals() != 0 || k.pointer() != 2 || k.generations() != generations || k.count("SELECT COUNT(*) FROM journal") != journal || k.rows() != rows {
				t.Fatalf("a refusal wrote: %d withdrawals, pointer %d, %d generations, journal %d -> %d", k.withdrawals(), k.pointer(), k.generations(), journal, k.count("SELECT COUNT(*) FROM journal"))
			}
		})
	}
}

// A poll the daemon stored under the generation the relationship stands on for an older turn says nothing about that generation: the generation is withdrawn all the same, and nothing is sent or
// changed for the deliveries that were already there.
func TestWithdrawalIgnoresAPollOfAnOlderTurnLabelledWithTheGeneration(t *testing.T) {
	k := newWithdrawKit(t)
	k.openUnsent()
	k.exec("INSERT INTO poll_observations (relationship_id, execution_generation, turn_id, last_status, last_polled_at, last_attempt_at, last_error) VALUES (?, 2, 'turn-of-generation-1', 'completed', ?, ?, NULL)",
		k.rid, "2026-10-02T00:00:00Z", "2026-10-02T00:00:00Z")
	deliveries, notes := k.count("SELECT COUNT(*) FROM deliveries"), k.count("SELECT COUNT(*) FROM delivery_supersession")
	if res, err := k.withdraw(2, "never sent"); err != nil || res.RestoredGeneration != 1 {
		t.Fatalf("withdraw = %v %+v", err, res)
	}
	if k.count("SELECT COUNT(*) FROM deliveries") != deliveries || k.count("SELECT COUNT(*) FROM delivery_supersession") != notes {
		t.Fatal("the withdrawal touched a delivery")
	}
	if k.count("SELECT COUNT(*) FROM journal WHERE kind = 'generation_withdrawn' AND subject = ?", k.rid) != 1 {
		t.Fatal("no journal row for the withdrawal")
	}
}

// A withdrawn generation is final: opening again under its request id does not hand it back, nothing binds it to a turn (whichever writer is asked), and a new generation takes the next number.
// Withdrawing the newest one again goes back to the nearest generation that was not withdrawn.
func TestAWithdrawnGenerationStaysClosedAndItsNumberIsNeverReused(t *testing.T) {
	k := newWithdrawKit(t)
	ctx := context.Background()
	reg := &registry.Registry{Store: k.s}
	k.openUnsent()
	if _, err := k.withdraw(2, "first"); err != nil {
		t.Fatal(err)
	}
	refused := func(what string, err error) {
		t.Helper()
		if refusalReason(err) != "stale_generation" || !strings.Contains(err.Error(), "was withdrawn") {
			t.Fatalf("%s = %v, want stale_generation naming the withdrawal", what, err)
		}
	}
	_, err := reg.OpenGeneration(ctx, k.rid, "refresh-"+k.rid, "needs_changes_revision", sql.NullString{})
	refused("registry generation-open under the old request id", err)
	refused("registry open inside a transaction", k.s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := reg.OpenGenerationIn(ctx, k.rid, "refresh-"+k.rid, "needs_changes_revision", sql.NullString{})
		return err
	}))
	_, err = reg.BindAnchor(ctx, k.rid, 2, "turn-late", "dispatch_receipt")
	refused("registry generation-bind", err)
	_, err = delivery.BindAnchor(ctx, k.s, delivery.SystemClock{}, k.rid, 2, "turn-late")
	refused("delivery bind", err)
	_, err = delivery.OpenGenerationIn(ctx, k.s, delivery.SystemClock{}, k.rid, "refresh-"+k.rid, "needs_changes_revision", nil)
	refused("delivery open under the old request id", err)
	if outcome, err := delivery.BindAnchorIn(ctx, k.s, delivery.SystemClock{}, k.rid, 2, "turn-late"); err != nil || outcome != "ineligible" {
		t.Fatalf("the daemon's binder = %q %v, want ineligible", outcome, err)
	}
	var state string
	var turn sql.NullString
	if err := k.s.DB.QueryRow("SELECT anchor_state, dispatch_turn_id FROM generations WHERE relationship_id = ? AND execution_generation = 2", k.rid).Scan(&state, &turn); err != nil || state != "anchor_pending" || turn.Valid {
		t.Fatalf("generation 2 = %s %v %v, want it still pending", state, turn, err)
	}

	g3, err := reg.OpenGeneration(ctx, k.rid, "second-"+k.rid, "needs_changes_revision", sql.NullString{})
	if err != nil || g3.Number != 3 || k.pointer() != 3 {
		t.Fatalf("the next generation = %v %+v, pointer %d, want 3", err, g3, k.pointer())
	}
	if res, err := k.withdraw(3, "second"); err != nil || res.RestoredGeneration != 1 || k.pointer() != 1 || k.withdrawals() != 2 {
		t.Fatalf("withdrawing 3 = %v %+v, pointer %d, %d withdrawals: want it to stand on 1 again", err, res, k.pointer(), k.withdrawals())
	}
	if g, err := reg.OpenGeneration(ctx, k.rid, "third-"+k.rid, "needs_changes_revision", sql.NullString{}); err != nil || g.Number != 4 {
		t.Fatalf("the generation after two withdrawals = %v %+v, want 4", err, g)
	}
	// the first withdrawal still answers as itself once the relationship moved on
	if again, err := k.withdraw(2, "first, again"); err != nil || !again.Replayed || again.Reason != "first" || again.RestoredGeneration != 1 {
		t.Fatalf("repeating the first withdrawal = %v %+v", err, again)
	}
	if n, err := store.NextGeneration(ctx, k.s.Q(ctx), k.rid); err != nil || n != 5 {
		t.Fatalf("NextGeneration = %d %v, want 5", n, err)
	}
	if live, err := store.LiveGenerationBefore(ctx, k.s.Q(ctx), k.rid, 4); err != nil || live != 1 {
		t.Fatalf("the generation 4 follows = %d %v, want 1", live, err)
	}
}

// A correction after a withdrawal follows the generation that was not withdrawn (W-6 of the plan): the ruling that opens it takes the number after the withdrawn one, the instruction line the relay
// prints names that number, dag-correct records it although the chain skips a number, and the child's first report there may declare the generation-1 revision as the one it replaces, which the
// currency of both the delivery and the registry packages resolve.
func TestACorrectionAfterAWithdrawalFollowsTheLiveGeneration(t *testing.T) {
	k := newWithdrawKit(t)
	ctx := context.Background()
	k.openUnsent()
	if _, err := k.withdraw(2, "never sent"); err != nil {
		t.Fatal(err)
	}
	roots, err := relationshipRoots(ctx, k.s.Q(ctx), k.rid)
	if err != nil || len(roots) == 0 {
		t.Fatalf("roots = %v %v", roots, err)
	}
	notes := writeFile(t, roots[0], "notes.md", "the notes of the rework")
	prepared, err := k.sched.PrepareCorrection(ctx, "g", "I", "parent", ManifestInput{Base: &BaseRef{Repository: k.repo.path, Ref: "dev", SHA: k.repo.git("rev-parse", "dev")}, RuleVersion: k.request(true).RuleVersion,
		Volatile: []Volatile{{Source: "linear:comment", SnapshotURI: notes, SHA256: shaOf([]byte("the notes of the rework")), CapturedAt: "2026-10-02T00:00:00Z"}}}, VerifyOptions{ArtifactRoots: roots})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !strings.Contains(prepared.Instruction, "Correction generation 3 of CRW-I") || prepared.DispatchRequestID != CorrectionRequestID("g", "I", prepared.ManifestDigest, 3) {
		t.Fatalf("prepared = %+v, want the instruction and the request id of generation 3", prepared)
	}
	k.rvRule(k.rid, "needs_changes", k.criteria, restoration(prepared.Instruction))
	if k.pointer() != 3 || k.generations() != 3 {
		t.Fatalf("the ruling opened pointer %d with %d generations, want generation 3", k.pointer(), k.generations())
	}
	res, err := k.sched.RecordCorrection(ctx, "g", "I", "parent", "")
	if err != nil || res.Generation != 3 || res.OpenedBy != OpenedByRuling || res.ManifestDigest != prepared.ManifestDigest {
		t.Fatalf("dag-correct = %v %+v", err, res)
	}
	var revision1 string
	if err := k.s.DB.QueryRow("SELECT revision_hash FROM events WHERE event_id = ?", k.event1).Scan(&revision1); err != nil {
		t.Fatal(err)
	}
	e3 := k.rvReportGeneration(k.rid, "I", 3, k.criteria)
	for _, declared := range []any{revision1, nil} {
		k.exec("UPDATE revision_lineage SET supersedes_hash = ? WHERE event_id = ?", declared, e3)
		head, err := delivery.HeadRevision(ctx, k.s, k.rid, 3)
		if event, _ := objString(head, "eventId"); err != nil || event != e3 {
			t.Fatalf("delivery head with predecessor %v = %v %v, want %s", declared, head, err, e3)
		}
		if h, err := registry.HeadRevision(ctx, k.s, k.rid, 3); err != nil || h.EventID != e3 {
			t.Fatalf("registry head with predecessor %v = %+v %v, want %s", declared, h, err, e3)
		}
	}
}

// A store that predates the DAG zone has no withdrawal: the numbering is the plain one every reader assumed.
func TestTheNumberingOfAStoreWithoutTheZoneIsThePlainOne(t *testing.T) {
	k := newWithdrawKit(t)
	ctx := context.Background()
	k.exec("DROP TABLE dag_generation_withdrawals")
	q := k.s.Q(ctx)
	if n, err := store.NextGeneration(ctx, q, k.rid); err != nil || n != 2 {
		t.Fatalf("NextGeneration = %d %v, want 2", n, err)
	}
	if live, err := store.LiveGenerationBefore(ctx, q, k.rid, 3); err != nil || live != 2 {
		t.Fatalf("LiveGenerationBefore(3) = %d %v, want 2", live, err)
	}
	if err := store.RefuseWithdrawn(ctx, q, k.rid, 2); err != nil {
		t.Fatalf("RefuseWithdrawn = %v", err)
	}
}

// The command as an operator runs it: parse, dispatch, store, JSON readback, and the exit codes of a refusal and of a usage error.
func TestTheWithdrawCommandAnswersAsAnOperatorReadsIt(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	f := newFixtureAt(t, filepath.Join(state, "relay.sqlite3"))
	forkJoinPlan(f, "p1")
	f.projectParent()
	r := f.reportNode("p1", "research", acceptOpts{})
	if _, err := (&registry.Registry{Store: f.s}).OpenGeneration(context.Background(), r.Acceptance.RelationshipID, "by-hand", "needs_changes_revision", sql.NullString{}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	args := []string{"dag-generation-withdraw", "--plan", "p1", "--node", "research", "--actor", "parent", "--generation", "2", "--reason", "sent nothing"}
	out, code := crw(t, state, args...)
	answer := parseOut(t, out)
	if code != 0 || answer["schema"] != SchemaWithdraw || answer["withdrawn_generation"] != float64(2) || answer["restored_generation"] != float64(1) || answer["relationship_id"] != r.Acceptance.RelationshipID ||
		answer["dispatch_request_id"] != "by-hand" || answer["reason"] != "sent nothing" || answer["replayed"] != false {
		t.Fatalf("answer = %d %s", code, out)
	}
	if out, code := crw(t, state, args...); code != 0 || parseOut(t, out)["replayed"] != true {
		t.Fatalf("repeat = %d %s", code, out)
	}
	if out, code := crw(t, state, "dag-generation-withdraw", "--plan", "p1", "--node", "research", "--actor", "parent", "--generation", "3", "--reason", "x"); code != 2 || parseOut(t, out)["reason"] != "stale_generation" {
		t.Fatalf("another generation = %d %s", code, out)
	}
	// a missing required option is a refusal of the command as the other DAG commands give it (exit 2), and writes nothing
	if out, code := crw(t, state, "dag-generation-withdraw", "--plan", "p1", "--node", "research", "--actor", "parent", "--generation", "2"); code != 2 {
		t.Fatalf("without --reason: exit %d\n%s", code, out)
	}
}
