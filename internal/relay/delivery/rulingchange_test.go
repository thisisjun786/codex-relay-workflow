package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// A different ruling on an event that already has one. Before this change the verdict writer
// answered every second ruling on a settled event with the settled record marked _replay and exit
// 0, whatever verdict was asked for, so a parent that gave needs_changes after verified read a
// success that changed nothing. The contract (docs/relay/README.md, "Ruling an event that is
// already ruled"): the same verdict again is a replay; a verified ruling that nothing has built on
// yet is replaced by needs_changes, which opens the next generation as a first needs_changes ruling
// does; every other different verdict is refused with disposition_conflict and writes nothing.
// These tests use temporary synthetic stores only.

func newRulingHL(t *testing.T) *hl {
	t.Helper()
	f := newFixture(t, "")
	f.rid = ""
	h := &hl{fixture: f, name: t.Name(), ack: NewAck(f.delivery), rc: NewReconciler(f.delivery), adapter: f.host, policy: defaultTick()}
	h.checks = &TurnChecks{Reconciler: h.rc, Budget: 4}
	return h
}

// rcRestoration is a needs_changes finding that declares the restoration block, as a base-refresh
// correction does.
func rcRestoration() []any {
	return []any{Obj{{Key: "id", Value: "c1"}, {Key: "verdict", Value: "needs_changes"}, {Key: "note", Value: "the base moved and conflicts: merge origin/dev and rerun the checks that merge invalidates"}, {Key: "restoration", Value: true}}}
}

// rcFootprint is every row of every non-empty table, so a refused ruling can be shown to have left
// the store exactly as it was.
func rcFootprint(t *testing.T, f *fixture) string {
	t.Helper()
	raw, err := json.Marshal(f.tables())
	mustDo(t, err)
	return string(raw)
}

func (h *hl) rcFootprint() string { return rcFootprint(h.t, h.fixture) }

func rcExec(t *testing.T, f *fixture, query string, args ...any) {
	t.Helper()
	mustDo(t, f.store.Transaction(f.ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := execSQL(ctx, f.store, query, args...)
		return err
	}))
}

func (h *hl) rcExec(query string, args ...any) { rcExec(h.t, h.fixture, query, args...) }

// rcAccept writes the plan acceptance of the event as dag-accept does, with synthetic values.
func (h *hl) rcAccept(event string) {
	h.t.Helper()
	r, err := LoadRelationship(h.ctx, h.store, h.rid)
	mustDo(h.t, err)
	head, err := HeadRevision(h.ctx, h.store, h.rid, r.Generation)
	mustDo(h.t, err)
	h.rcExec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"acceptance-1", "plan-1", "node-1", "manifest-1", h.rid, r.Generation, event, field(head, "revisionHash"), "criteria-digest", "verified", "host_read", "v1", "{}", parent, 0, h.clock.ISO(), "active")
}

// rcTurn writes a merge turn of the relationship in the given state, with synthetic values.
func rcTurn(t *testing.T, f *fixture, rid, state string) {
	t.Helper()
	rcExec(t, f, "INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, relationship_id, pr_number, candidate_head, declared_ready, state, tenure, requested_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"turn-"+state, "owner/repo@dev", "owner/repo", "dev", "project-1", parent, host, rid, 7, "0123456789abcdef0123456789abcdef01234567", 1, state, 1, f.clock.ISO(), f.clock.ISO())
}

// rcRefused rules, expects a refusal with reason want and that nothing was written, and returns
// the refusal's text.
func (h *hl) rcRefused(event, verdict, turn string, findings []any, reason any, want string) string {
	h.t.Helper()
	before := h.rcFootprint()
	record, err := h.rule(event, verdict, turn, findings, reason, nil)
	if err == nil {
		h.t.Fatalf("a %s ruling on %s answered %v (replayed %v), want a refusal", verdict, event, record, replayed(record))
	}
	if Reason(err) != want {
		h.t.Fatalf("a %s ruling on %s was refused %q (%v), want %q", verdict, event, Reason(err), err, want)
	}
	if after := h.rcFootprint(); after != before {
		h.t.Fatalf("a refused ruling wrote:\nbefore %s\nafter  %s", before, after)
	}
	return err.Error()
}

func rcMentions(t *testing.T, text string, words ...string) {
	t.Helper()
	for _, w := range words {
		if !strings.Contains(text, w) {
			t.Fatalf("the refusal does not say %q: %s", w, text)
		}
	}
}

// c1: needs_changes after verified, with nothing accepted, is a ruling and not a replay.
func TestRC01_needs_changes_after_verified_replaces_the_ruling_and_opens_the_correction(t *testing.T) {
	t.Parallel()
	h := newRulingHL(t)
	event := h.managedVerified()
	record, err := h.rule(event, "needs_changes", "v2", rcRestoration(), nil, nil)
	if err != nil {
		t.Fatalf("needs_changes after verified: %v", err)
	}
	if replayed(record) {
		t.Fatalf("exit 0 replayed the old verdict: %v", record)
	}
	if field(record, "verdict") != "needs_changes" || field(record, "verdictTurnId") != "v2" || fmt.Sprint(field(record, "nextExecutionGeneration")) != "2" {
		t.Fatalf("the answer is not the new ruling and its generation: %v", record)
	}
	if got := h.one("SELECT verdict, next_generation, verdict_turn_id FROM verdicts WHERE event_id = ?", event); got.S("verdict") != "needs_changes" || got.I("next_generation") != 2 || got.S("verdict_turn_id") != "v2" {
		t.Fatalf("the stored ruling = %v", got)
	}
	if n := h.count("SELECT COUNT(*) AS c FROM generations WHERE relationship_id = ?", h.rid); n != 2 {
		t.Fatalf("generations = %d, want the correction's", n)
	}
	revision := h.one("SELECT e.event_id AS event_id, d.recipient_thread_id AS recipient FROM events e JOIN deliveries d ON d.event_id = e.event_id WHERE e.outcome = 'revision_request'")
	if revision == nil || revision.S("recipient") != child {
		t.Fatalf("the revision request was not queued to the child: %v", revision)
	}
	if state := pyjson.Text(h.rrState().Get("state")); state != "needs_changes" {
		t.Fatalf("the assignment reads %q after the ruling, want needs_changes", state)
	}
	// the replaced ruling is kept: the journal names it and the answer says it was replaced
	entry := h.one("SELECT detail FROM journal WHERE kind = 'verdict_superseded' AND subject = ?", event)
	if entry == nil || !strings.Contains(entry.S("detail"), "ruling_changed") || !strings.Contains(entry.S("detail"), "\"verified\"") {
		t.Fatalf("the replaced ruling is not journalled: %v", entry)
	}
	replaced, _ := record.Lookup("_supersedes")
	if o, ok := replaced.(Obj); !ok || pyjson.Text(o.Get("verdict")) != "verified" || pyjson.Text(o.Get("verdictTurnId")) != "v1" {
		t.Fatalf("the answer does not say it replaced the verified ruling: %v", record)
	}
	// c2 on the new ruling: giving it again is a replay and opens nothing further
	before := h.rcFootprint()
	again, err := h.rule(event, "needs_changes", "v3", rcRestoration(), nil, nil)
	if err != nil || !replayed(again) || field(again, "verdictTurnId") != "v2" {
		t.Fatalf("the same ruling again = %v %v, want a replay of the new ruling", again, err)
	}
	if _, has := again.Lookup("_supersedes"); has {
		t.Fatalf("a replay claims to replace a ruling: %v", again)
	}
	if after := h.rcFootprint(); after != before {
		t.Fatalf("a replay wrote:\nbefore %s\nafter  %s", before, after)
	}
}

// c2: the same verdict again stays an idempotent replay and writes nothing.
func TestRC02_the_same_verdict_again_is_an_idempotent_replay(t *testing.T) {
	t.Parallel()
	for _, verdict := range []string{"verified", "needs_changes", "unverified", "aborted"} {
		t.Run(verdict, func(t *testing.T) {
			h := newRulingHL(t)
			event := h.claimed(true)
			findings, reason := rrPassing, any(nil)
			switch verdict {
			case "needs_changes":
				findings = rrFinding("needs_changes", "fix it")
			case "unverified":
				findings = rrFinding("unverified", "could not reach it")
			case "aborted":
				findings, reason = nil, "stopping"
			}
			h.mustRule(event, verdict, "v1", findings, reason, nil)
			before := h.rcFootprint()
			again, err := h.rule(event, verdict, "v2", findings, reason, nil)
			if err != nil || !replayed(again) || field(again, "verdict") != verdict || field(again, "verdictTurnId") != "v1" {
				t.Fatalf("%s again = %v %v, want the recorded ruling marked as a replay", verdict, again, err)
			}
			if after := h.rcFootprint(); after != before {
				t.Fatalf("a replay wrote:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// c1: every other different verdict is refused and writes nothing, never answered with the old one.
func TestRC03_a_different_verdict_that_cannot_replace_the_recorded_one_is_refused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		recorded, asked string
		route           []string
	}{
		{"verified", "unverified", []string{"only needs_changes", "relationship"}},
		{"verified", "aborted", []string{"only needs_changes", "relationship"}},
		{"needs_changes", "verified", []string{"generation 2"}},
		{"needs_changes", "unverified", []string{"generation 2"}},
		{"unverified", "verified", []string{"generation-open"}},
		{"unverified", "needs_changes", []string{"generation-open"}},
		{"aborted", "verified", []string{"generation-open"}},
	}
	for _, tc := range cases {
		t.Run(tc.recorded+" then "+tc.asked, func(t *testing.T) {
			h := newRulingHL(t)
			event := h.claimed(true)
			switch tc.recorded {
			case "verified":
				h.mustRule(event, "verified", "v1", rrPassing, nil, nil)
			case "needs_changes":
				h.mustRule(event, "needs_changes", "v1", rrFinding("needs_changes", "fix it"), nil, nil)
			case "unverified":
				h.mustRule(event, "unverified", "v1", rrFinding("unverified", "could not reach it"), nil, nil)
			case "aborted":
				h.mustRule(event, "aborted", "v1", nil, "stopping", nil)
			}
			findings, reason := rrPassing, any(nil)
			switch tc.asked {
			case "needs_changes":
				findings = rrFinding("needs_changes", "on reflection, fix it")
			case "unverified":
				findings = rrFinding("unverified", "could not reach it")
			case "aborted":
				findings, reason = nil, "stopping"
			}
			detail := h.rcRefused(event, tc.asked, "v2", findings, reason, DispositionConflict)
			rcMentions(t, detail, append([]string{"already ruled " + tc.recorded, tc.asked}, tc.route...)...)
		})
	}
}

// c1: once the plan accepted the verified result, or the work was marked merged, a second ruling
// is refused with the route that remains, which depends on the node's own reading. For an accepted
// result that is current, a base that moved after the acceptance has a route (dag-base-refresh)
// and every other current result keeps the report-it sentence.
func TestRC04_acceptance_and_a_merge_mark_close_the_change(t *testing.T) {
	t.Parallel()
	t.Run("accepted", func(t *testing.T) {
		h := newRulingHL(t)
		event := h.managedVerified()
		h.rcAccept(event)
		detail := h.rcRefused(event, "needs_changes", "v2", rcRestoration(), nil, DispositionConflict)
		rcMentions(t, detail, "acceptance-1", "plan-1", "node-1", "dag-correct --prepare", "stale", "no recorded correction route", "generation dag-correct will refuse")
		rcMentions(t, detail, "a base that moved after the acceptance", "dag-base-refresh", "merges of the base")
		if strings.Contains(detail, "a base that moved after the acceptance is such a case") {
			t.Fatalf("the refusal still files the moved base under the case that has no route: %s", detail)
		}
	})
	t.Run("merged", func(t *testing.T) {
		h := newRulingHL(t)
		event := h.managedVerified()
		h.rrMark(event, "merged at abc123", "parent")
		detail := h.rcRefused(event, "needs_changes", "v2", rcRestoration(), nil, DispositionConflict)
		rcMentions(t, detail, "marked merged", "new work")
	})
}

// c1: the ruling is replaced only on the head of the generation the relationship stands on, and a
// refusal of the existing path keeps its reason and says what to do.
func TestRC05_a_ruling_on_an_event_the_generation_left_behind_is_refused_with_the_route(t *testing.T) {
	t.Parallel()
	h := newRulingHL(t)
	event := h.managedVerified()
	h.openGeneration("later", "needs_changes_revision", "later-turn")
	detail := h.rcRefused(event, "needs_changes", "v2", rcRestoration(), nil, StaleGeneration)
	rcMentions(t, detail, "already ruled verified", "assignment-show", "head")
}

func TestRC05_the_other_currency_refusals_keep_their_reason_and_name_the_route(t *testing.T) {
	t.Parallel()
	refused := func(t *testing.T, v *vcu, event, want string) string {
		t.Helper()
		before := rcFootprint(t, v.fixture)
		got := v.verdict(event, "needs_changes", "v2", nil, nil, nil)
		if got["reason"] != want {
			t.Fatalf("a needs_changes ruling after verified was answered %v, want a %s refusal", got, want)
		}
		if after := rcFootprint(t, v.fixture); after != before {
			t.Fatalf("a refused ruling wrote:\nbefore %s\nafter  %s", before, after)
		}
		return fmt.Sprint(got["detail"])
	}
	t.Run("superseded revision", func(t *testing.T) {
		v := newVCU(t, "")
		e := v.acknowledged("")
		if got := v.verdict(e, "verified", "v1", nil, nil, nil); got["ok"] == nil {
			t.Fatalf("verified = %v", got)
		}
		hash := v.revisionHash(e)
		v.accepted("second revision", &hash)
		rcMentions(t, refused(t, v, e, SupersededRevision), "already ruled verified", "assignment-show", "newer revision")
	})
	t.Run("ambiguous head", func(t *testing.T) {
		v := newVCU(t, "")
		e := v.acknowledged("")
		if got := v.verdict(e, "verified", "v1", nil, nil, nil); got["ok"] == nil {
			t.Fatalf("verified = %v", got)
		}
		v.accepted("a different revision", nil)
		rcMentions(t, refused(t, v, e, RevisionAmbiguous), "already ruled verified", "no single head", "generation-open", "plan node")
	})
	t.Run("relationship not active", func(t *testing.T) {
		v := newVCU(t, "")
		e := v.acknowledged("")
		if got := v.verdict(e, "verified", "v1", nil, nil, nil); got["ok"] == nil {
			t.Fatalf("verified = %v", got)
		}
		rcExec(t, v.fixture, "UPDATE relationships SET status = 'paused' WHERE relationship_id = ?", v.rid)
		rcMentions(t, refused(t, v, e, RelationshipNotActive), "already ruled verified", "relationship-resume", "successor")
	})
}

// c1: the relationship must be able to carry the correction, as for a first needs_changes ruling,
// and a refusal keeps the verified ruling.
func TestRC06_a_correction_nobody_can_receive_is_refused_and_keeps_the_verified_ruling(t *testing.T) {
	t.Parallel()
	h := newRulingHL(t)
	event := h.managedVerified()
	h.rcExec("UPDATE relationships SET allowed_recipients = ? WHERE relationship_id = ?", "[\""+parent+"\"]", h.rid)
	h.rcRefused(event, "needs_changes", "v2", rcRestoration(), nil, RecipientNotAuthorized)
	if got := h.one("SELECT verdict FROM verdicts WHERE event_id = ?", event); got.S("verdict") != "verified" {
		t.Fatalf("the verified ruling did not survive the refusal: %v", got)
	}
}

// The coordination summary owed for the changed ruling is a new job that says it is the second.
func TestRC07_the_changed_ruling_enqueues_its_own_summary_with_the_ordinal(t *testing.T) {
	t.Parallel()
	t.Run("managed", func(t *testing.T) {
		h := newRulingHL(t)
		event := h.claimed(true)
		h.rcExec("INSERT INTO sync_targets (relationship_id, target, target_ref, recorded_at) VALUES (?,?,?,?)", h.rid, "coordination_document", "DOC-1", h.clock.ISO())
		h.ack.Sync = VerdictSync(h.store, h.clock)
		h.mustRule(event, "verified", "v1", rrPassing, nil, nil)
		h.mustRule(event, "needs_changes", "v2", rcRestoration(), nil, nil)
		rows := h.store.DB
		var first, second, summary string
		if err := rows.QueryRow("SELECT sync_id FROM sync_outbox WHERE verdict = 'verified'").Scan(&first); err != nil {
			t.Fatal(err)
		}
		if err := rows.QueryRow("SELECT sync_id, summary FROM sync_outbox WHERE verdict = 'needs_changes'").Scan(&second, &summary); err != nil {
			t.Fatalf("no summary was owed for the changed ruling: %v", err)
		}
		if first == second {
			t.Fatalf("the changed ruling reuses the sync identity %s", first)
		}
		rcMentions(t, summary, "needs_changes", "ruling 2", "a revision request was queued")
	})
	t.Run("without canonical criteria", func(t *testing.T) {
		v := newVCU(t, "")
		e := v.acknowledged("")
		rcExec(t, v.fixture, "INSERT INTO sync_targets (relationship_id, target, target_ref, recorded_at) VALUES (?,?,?,?)", v.rid, "coordination_document", "DOC-1", v.clock.ISO())
		v.ack.Sync = VerdictSync(v.store, v.clock)
		if got := v.verdict(e, "verified", "v1", nil, nil, nil); got["ok"] == nil {
			t.Fatalf("verified = %v", got)
		}
		if got := v.verdict(e, "needs_changes", "v2", nil, nil, nil); got["ok"] == nil {
			t.Fatalf("needs_changes = %v", got)
		}
		var firstSummary, second, summary string
		if err := v.store.DB.QueryRow("SELECT summary FROM sync_outbox WHERE verdict = 'verified'").Scan(&firstSummary); err != nil {
			t.Fatal(err)
		}
		if err := v.store.DB.QueryRow("SELECT sync_id, summary FROM sync_outbox WHERE verdict = 'needs_changes'").Scan(&second, &summary); err != nil {
			t.Fatalf("no summary was owed for the changed ruling: %v", err)
		}
		rcMentions(t, summary, "ruling 2")
		if strings.Contains(firstSummary, "ruling") {
			t.Fatalf("the first ruling of a relationship with no criteria must read as it always did: %s", firstSummary)
		}
	})
}

// c1: a merge turn that is merging, of unknown effect or landed means the verified work is on its
// way to or already on the target; one that only holds the lane does not (the parent that found
// the base conflict holds it).
func TestRC08_a_merge_turn_that_acts_on_the_verified_work_closes_the_change(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"merging", "unknown", "landed"} {
		t.Run(state, func(t *testing.T) {
			h := newRulingHL(t)
			event := h.managedVerified()
			rcTurn(t, h.fixture, h.rid, state)
			detail := h.rcRefused(event, "needs_changes", "v2", rcRestoration(), nil, DispositionConflict)
			rcMentions(t, detail, "turn-"+state, state, "0123456789ab")
			if state == "landed" {
				rcMentions(t, detail, "new work")
			} else {
				rcMentions(t, detail, "merge-turn-resolve")
			}
		})
	}
	for _, state := range []string{"holding", "waiting"} {
		t.Run(state+" does not refuse", func(t *testing.T) {
			h := newRulingHL(t)
			event := h.managedVerified()
			rcTurn(t, h.fixture, h.rid, state)
			record, err := h.rule(event, "needs_changes", "v2", rcRestoration(), nil, nil)
			if err != nil || replayed(record) || field(record, "verdict") != "needs_changes" {
				t.Fatalf("a %s turn must not stop the correction: %v %v", state, record, err)
			}
		})
	}
	t.Run("another relationship's turn is not this one's", func(t *testing.T) {
		h := newRulingHL(t)
		event := h.managedVerified()
		rcTurn(t, h.fixture, "another-relationship", "merging")
		if record, err := h.rule(event, "needs_changes", "v2", rcRestoration(), nil, nil); err != nil || replayed(record) {
			t.Fatalf("a turn of another relationship stopped the correction: %v %v", record, err)
		}
	})
}

// A re-review is the DAG's ruling route for an accepted head: it runs before the transition rule and
// is not stopped by the acceptance it exists to re-examine.
func TestRC09_an_open_re_review_still_takes_a_needs_changes_ruling_on_an_accepted_head(t *testing.T) {
	t.Parallel()
	h := newRulingHL(t)
	event := h.managedVerified()
	h.rcAccept(event)
	h.editCriteria()
	record := h.reReview(event, "needs_changes", rrFinding("needs_changes", "the new wording is not met"))
	if replayed(record) || field(record, "verdict") != "needs_changes" || fmt.Sprint(field(record, "nextExecutionGeneration")) != "2" {
		t.Fatalf("the re-review of an accepted head = %v", record)
	}
	entry := h.one("SELECT detail FROM journal WHERE kind = 'verdict_superseded' AND subject = ?", event)
	if entry == nil || strings.Contains(entry.S("detail"), "ruling_changed") {
		t.Fatalf("a re-review is journalled as a re-review: %v", entry)
	}
}

// The parent may give both rulings in one turn: the verified ruling opened no revision request, so
// the derived ids cannot collide.
func TestRC10_both_rulings_from_one_verdict_turn_replace_cleanly(t *testing.T) {
	t.Parallel()
	h := newRulingHL(t)
	event := h.managedVerified()
	record, err := h.rule(event, "needs_changes", "v1", rcRestoration(), nil, nil)
	if err != nil || replayed(record) || fmt.Sprint(field(record, "nextExecutionGeneration")) != "2" {
		t.Fatalf("needs_changes from the same verdict turn = %v %v", record, err)
	}
	if n := h.count("SELECT COUNT(*) AS c FROM events WHERE outcome = 'revision_request'"); n != 1 {
		t.Fatalf("revision requests = %d, want one", n)
	}
}

// A failure after the ruling was written, such as the summary owed to the coordination document,
// rolls the whole replacement back: the verified ruling, the generation and the queues stay.
func TestRC11_a_failure_inside_the_ruling_keeps_the_verified_ruling(t *testing.T) {
	t.Parallel()
	h := newRulingHL(t)
	event := h.managedVerified()
	h.ack.Sync = func(context.Context, Relationship, Row, string, []any, Obj, any, int64) error {
		return errors.New("the summary could not be queued")
	}
	before := h.rcFootprint()
	if _, err := h.rule(event, "needs_changes", "v2", rcRestoration(), nil, nil); err == nil {
		t.Fatal("the ruling reported success although its summary failed")
	}
	if after := h.rcFootprint(); after != before {
		t.Fatalf("a failed ruling wrote:\nbefore %s\nafter  %s", before, after)
	}
}
