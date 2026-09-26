package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// test_rereview_deadlock.py RRD-1..RRD-9, mirrored test for test (hostloss_harness_test.go mirror
// + testdata/capture.py). The re-review itself (claim_verification's re-claim, record_verdict's
// decided-not-replayed path, the verdict_superseded journal) is delivery's ack.go. The assignment
// view the tests read state through is todo 25's (registry.AssignmentView on codex/crw-154-registry);
// rrState below reads the same rows by assignment.py's _resolve for the states these tests reach,
// and rrMark writes AssignmentView.mark's rows (the journal row is compared with Python's).

const rrd = "test_rereview_deadlock"

func rrCriteria(titles ...string) []any {
	out := []any{}
	for i, title := range titles {
		out = append(out, Obj{{Key: "id", Value: []string{"c1", "c2"}[i]}, {Key: "title", Value: title}})
	}
	return out
}

var (
	rrSet     = rrCriteria("the endpoint returns the agreed shape", "a malformed request is refused")
	rrEdited  = rrCriteria("the endpoint returns a COMPLETELY different shape", "a malformed request is refused")
	rrPassing = []any{Obj{{Key: "id", Value: "c1"}, {Key: "verdict", Value: "verified"}}, Obj{{Key: "id", Value: "c2"}, {Key: "verdict", Value: "verified"}}}
)

func rrFinding(verdict, note string) []any {
	return []any{Obj{{Key: "id", Value: "c1"}, {Key: "verdict", Value: verdict}, {Key: "note", Value: note}}}
}

func (h *hl) registerCriteria(set []any) {
	h.t.Helper()
	_, err := h.ack.Criteria.Register(h.ctx, h.rid, set, "https://linear.app/doc/1")
	mustDo(h.t, err)
}

func (h *hl) setDigest() any {
	got, err := h.ack.Criteria.Get(h.ctx, h.rid)
	mustDo(h.t, err)
	return field(got, "setDigest")
}

func (h *hl) claim(event, turn string) string {
	h.t.Helper()
	out, err := h.ack.ClaimVerification(h.ctx, event, turn)
	mustDo(h.t, err)
	return out
}

func (h *hl) rule(event, verdict, turn string, findings []any, reason, expected any) (Obj, error) {
	return h.ack.RecordVerdict(h.ctx, event, verdict, turn, nil, findings, reason, expected)
}

func (h *hl) mustRule(event, verdict, turn string, findings []any, reason, expected any) Obj {
	h.t.Helper()
	out, err := h.rule(event, verdict, turn, findings, reason, expected)
	mustDo(h.t, err)
	return out
}

// claimed is ReReviewTestCase.claimed.
func (h *hl) claimed(register bool) string {
	event := h.queuedEvent(regOpts{recipients: []string{parent, child}})
	if register {
		h.registerCriteria(rrSet)
	}
	h.attemptOn(event, h.host, nil)
	h.clock.Advance(5)
	turn := h.host.startTurn(parent, "ack-turn", "inProgress", "")
	_, err := h.ack.Acknowledge(h.ctx, event, turn.TurnID, AckProof(event, turn.TurnID), true, nil, h.host)
	mustDo(h.t, err)
	h.claim(event, turn.TurnID)
	return event
}

func (h *hl) managedVerified() string {
	event := h.claimed(true)
	h.mustRule(event, "verified", "v1", rrPassing, nil, nil)
	return event
}

func (h *hl) editCriteria() { h.registerCriteria(rrEdited) }

// reReview is ReReviewTestCase.re_review.
func (h *hl) reReview(event, verdict string, findings []any) Obj {
	h.claim(event, "re-review-turn")
	if findings == nil {
		findings = rrPassing
	}
	return h.mustRule(event, verdict, "v2", findings, nil, h.setDigest())
}

// rrState is AssignmentView.state's state, nextExpectedAction, head, mark and criteria.
func (h *hl) rrState() Obj {
	h.t.Helper()
	r, err := LoadRelationship(h.ctx, h.store, h.rid)
	mustDo(h.t, err)
	head, err := HeadRevision(h.ctx, h.store, h.rid, r.Generation)
	mustDo(h.t, err)
	headID := field(head, "eventId")
	var verdict Row
	if headID != nil {
		verdict = h.one("SELECT v.verdict, c.set_digest FROM verdicts v LEFT JOIN verdict_context c ON c.event_id = v.event_id WHERE v.event_id = ?", headID)
	}
	digest := h.setDigest()
	current := verdict == nil || verdict.Opt("set_digest") == digest
	var mark Obj
	if current && headID != nil && verdict != nil && verdict.S("verdict") == "verified" {
		if m := h.one("SELECT * FROM assignment_marks WHERE relationship_id = ? AND event_id = ? AND execution_generation = ? AND revision_hash = ? ORDER BY marked_at LIMIT 1", h.rid, headID, r.Generation, field(head, "revisionHash")); m != nil {
			mark = Obj{{Key: "mark", Value: m.S("mark")}, {Key: "eventId", Value: m.S("event_id")}}
		}
	}
	previous := h.one("SELECT v.event_id FROM verdicts v JOIN events e ON e.event_id = v.event_id WHERE e.relationship_id = ? AND e.execution_generation < ? AND v.verdict = 'needs_changes' ORDER BY e.execution_generation DESC LIMIT 1", h.rid, r.Generation)
	state := "requested"
	switch {
	case r.Status == "paused":
		state = "paused"
	case slices.Contains(ambiguousEvidence, str(head, "evidence")):
		state = "ambiguous"
	case verdict != nil && verdict.S("verdict") == "verified" && !current:
		state = "re_review_needed"
	case mark != nil:
		state = str(mark, "mark")
	case verdict != nil && verdict.S("verdict") == "verified":
		state = "verified"
	case headID != nil && previous != nil:
		state = "corrected"
	case headID != nil && h.one("SELECT 1 FROM verification_claims WHERE event_id = ?", headID) != nil:
		state = "verifying"
	case headID != nil:
		state = "received"
	case previous != nil:
		state = "needs_changes"
	}
	action := map[string]string{"re_review_needed": "parent_verifies", "verified": "coordinator_integrates", "merged": "none", "paused": "owner_resumes"}[state]
	if action == "" {
		action = h.nextAction()
	}
	var reviewed any
	if verdict != nil {
		reviewed = verdict.Opt("set_digest")
	}
	var markValue any
	if mark != nil {
		markValue = mark
	}
	return Obj{{Key: "state", Value: state}, {Key: "nextExpectedAction", Value: action}, {Key: "head", Value: Obj{{Key: "eventId", Value: headID}}}, {Key: "mark", Value: markValue},
		{Key: "criteria", Value: Obj{{Key: "setDigest", Value: digest}, {Key: "reviewedSetDigest", Value: reviewed}, {Key: "current", Value: current}}}}
}

// rrMark is AssignmentView.mark(rid, "merged", ...) for a verified, current head: its rows.
func (h *hl) rrMark(event, evidence, actor string) {
	h.t.Helper()
	r, err := LoadRelationship(h.ctx, h.store, h.rid)
	mustDo(h.t, err)
	head, err := HeadRevision(h.ctx, h.store, h.rid, r.Generation)
	mustDo(h.t, err)
	if field(head, "eventId") != event {
		h.t.Fatalf("the mark names %s but the head is %v", event, field(head, "eventId"))
	}
	now := h.clock.ISO()
	mustDo(h.t, h.store.Transaction(h.ctx, func(ctx context.Context, _ *sql.Conn) error {
		if _, err := execSQL(ctx, h.store, "INSERT INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(relationship_id, mark, event_id) DO UPDATE SET evidence = excluded.evidence, actor = excluded.actor, marked_at = excluded.marked_at",
			h.rid, "merged", event, r.Generation, field(head, "revisionHash"), evidence, actor, now); err != nil {
			return err
		}
		return journal(ctx, h.store, "assignment_marked", h.rid, Obj{{Key: "mark", Value: "merged"}, {Key: "eventId", Value: event}, {Key: "generation", Value: r.Generation}}, now)
	}))
}

// reviewTables are the review-side tables the re-review paths write, beyond mirror's delivery tables.
var reviewTables = []string{"verification_claims", "claim_context", "verdict_context", "canonical_criteria", "verification_mode", "assignment_marks", "relationships"}

// rrMirror is mirror plus a row-for-row comparison of reviewTables with Python's.
func rrMirror(t *testing.T, name string, body func(h *hl)) {
	t.Helper()
	mirror(t, rrd, name, func(h *hl) {
		body(h)
		raw, err := os.ReadFile(filepath.Join(pythonCaptures(t, rrd), name, "capture.json"))
		mustDo(t, err)
		var python pyCapture
		mustDo(t, json.Unmarshal(raw, &python))
		got := normalizeJSON(t, h.tables()).(map[string]any)
		want := normalizeJSON(t, python.Tables).(map[string]any)
		for _, n := range reviewTables {
			requireSameJSON(t, "table "+n, got[n], want[n])
		}
	})
}

func replayed(record Obj) bool { return truthy(field(record, "_replay")) }

func Test21_RRD01_a_criteria_edit_after_a_verified_verdict_needs_a_re_review(t *testing.T) {
	t.Run("managed verified then edit", func(t *testing.T) {
		rrMirror(t, "ReReviewIsReachable.test_the_starting_point_is_the_state_the_view_already_reports", func(h *hl) {
			h.managedVerified()
			h.eq(str(h.rrState(), "state"))
			h.editCriteria()
			record := h.rrState()
			h.eq(str(record, "state"))
			h.eq(str(record, "nextExpectedAction"))
		})
	})
	t.Run("legacy verdict then first registration", func(t *testing.T) {
		rrMirror(t, "ReviewClaimedBeforeTheEdit.test_registering_criteria_after_a_legacy_verdict_opens_a_re_review", func(h *hl) {
			event := h.claimed(false)
			h.mustRule(event, "verified", "v1", nil, nil, nil)
			h.eq(str(h.rrState(), "state"))
			h.registerCriteria(rrSet)
			h.eq(str(h.rrState(), "state"))
			h.eq(h.claim(event, "re-review-turn"))
			h.refusal(h.rule(event, "verified", "v2", rrPassing, nil, nil))
			record := h.mustRule(event, "verified", "v2", rrPassing, nil, h.setDigest())
			h.eq(replayed(record))
			h.eq(str(h.rrState(), "state"))
		})
	})
}

func (h *hl) claimHolder(event string) string {
	return h.one("SELECT * FROM verification_claims WHERE event_id = ?", event).S("claim_turn_id")
}

func (h *hl) boundDigest(event string) any {
	got, err := h.ack.Criteria.BoundDigest(h.ctx, event)
	mustDo(h.t, err)
	return got
}

func Test21_RRD02_a_re_claim_rebinds_once_and_only_once(t *testing.T) {
	t.Run("first re-claim", func(t *testing.T) {
		rrMirror(t, "ReReviewIsReachable.test_the_review_can_be_claimed_again_onto_the_current_set", func(h *hl) {
			event := h.managedVerified()
			h.editCriteria()
			h.eq(h.claim(event, "re-review-turn"))
			h.eq(h.boundDigest(event))
			h.eq(h.claimHolder(event))
		})
	})
	t.Run("second re-claim", func(t *testing.T) {
		rrMirror(t, "AReClaimIsNotAFreePass.test_re_claiming_does_not_become_an_unlimited_claim", func(h *hl) {
			event := h.managedVerified()
			h.editCriteria()
			h.eq(h.claim(event, "re-review-turn"))
			h.eq(h.claim(event, "third-reviewer-turn"))
			h.eq(h.claimHolder(event))
		})
	})
	t.Run("after a landed re-review", func(t *testing.T) {
		rrMirror(t, "AReClaimIsNotAFreePass.test_a_landed_re_review_does_not_leave_the_claim_open", func(h *hl) {
			event := h.managedVerified()
			h.editCriteria()
			h.reReview(event, "verified", nil)
			h.eq(h.claim(event, "fourth-turn"))
		})
	})
	t.Run("after an attested re-review", func(t *testing.T) {
		rrMirror(t, "AReClaimIsNotAFreePass.test_an_attested_re_review_does_not_leave_the_claim_open_either", func(h *hl) {
			event := h.claimed(false)
			h.mustRule(event, "verified", "v1", nil, nil, nil)
			h.registerCriteria(rrSet)
			record := h.mustRule(event, "verified", "v2", rrPassing, nil, h.setDigest())
			h.eq(replayed(record))
			h.eq(str(h.rrState(), "state"))
			h.eq(h.boundDigest(event))
			h.eq(h.claim(event, "fifth-turn"))
		})
	})
	t.Run("unchanged set", func(t *testing.T) {
		rrMirror(t, "ProtectionsThatMustSurvive.test_an_unchanged_criteria_set_still_refuses_a_second_claim", func(h *hl) {
			event := h.managedVerified()
			h.eq(h.claim(event, "another-turn"))
			h.eq(h.count("SELECT COUNT(*) AS c FROM verification_claims"))
		})
	})
}

func Test21_RRD03_a_re_review_is_decided_not_replayed(t *testing.T) {
	rrMirror(t, "ReReviewIsReachable.test_a_re_review_is_decided_rather_than_replayed", func(h *hl) {
		event := h.managedVerified()
		h.editCriteria()
		record := h.reReview(event, "verified", nil)
		h.eq(replayed(record))
		h.eq(field(record, "verdictTurnId"))
	})
	rrMirror(t, "ReReviewIsReachable.test_a_re_review_returns_the_assignment_to_verified", func(h *hl) {
		event := h.managedVerified()
		h.editCriteria()
		h.reReview(event, "verified", nil)
		record := h.rrState()
		h.eq(str(record, "state"))
		h.eq(field(sub(record, "criteria"), "current"))
		h.eq(field(sub(record, "criteria"), "reviewedSetDigest"))
	})
	rrMirror(t, "ReReviewIsReachable.test_the_child_never_has_to_touch_an_unrelated_byte", func(h *hl) {
		event := h.managedVerified()
		before := h.count("SELECT COUNT(*) AS c FROM events")
		h.editCriteria()
		record := h.reReview(event, "verified", nil)
		h.eq(replayed(record))
		h.eq(h.count("SELECT COUNT(*) AS c FROM events"))
		h.eq(field(sub(h.rrState(), "head"), "eventId"))
		_ = before
	})
	rrMirror(t, "ReReviewIsReachable.test_a_re_review_can_also_ask_for_changes", func(h *hl) {
		event := h.managedVerified()
		h.editCriteria()
		record := h.reReview(event, "needs_changes", rrFinding("needs_changes", "the new shape is missing"))
		h.eq(field(record, "nextExecutionGeneration"))
		r, err := LoadRelationship(h.ctx, h.store, h.rid)
		mustDo(t, err)
		h.eq(r.Generation)
	})
}

func Test21_RRD04_the_superseded_verdict_stays_on_the_record(t *testing.T) {
	rrMirror(t, "ReReviewIsReachable.test_the_superseded_verdict_stays_on_the_record", func(h *hl) {
		event := h.managedVerified()
		h.editCriteria()
		h.reReview(event, "verified", nil)
		entry := h.one("SELECT detail FROM journal WHERE kind = 'verdict_superseded' AND subject = ?", event)
		h.eq(entry != nil)
		superseded := sub(loadsObj(entry.S("detail")), "supersededVerdict")
		h.eq(field(superseded, "verdictTurnId"))
		h.eq(field(superseded, "verdict"))
	})
}

func Test21_RRD05_an_earlier_merge_mark_is_state_again_once_the_re_review_lands(t *testing.T) {
	rrMirror(t, "ReReviewIsReachable.test_an_earlier_merge_mark_is_state_again_once_the_re_review_lands", func(h *hl) {
		event := h.managedVerified()
		h.rrMark(event, "merged as abc1234", parent)
		h.eq(str(h.rrState(), "state"))
		h.editCriteria()
		h.eq(str(h.rrState(), "state"))
		h.reReview(event, "verified", nil)
		record := h.rrState()
		h.eq(str(record, "state"))
		h.eq(field(sub(record, "mark"), "eventId"))
	})
}

func Test21_RRD06_a_review_claimed_before_the_edit_finishes_once_claimed_again(t *testing.T) {
	rrMirror(t, "ReviewClaimedBeforeTheEdit.test_the_ruling_is_still_refused_until_the_review_is_claimed_again", func(h *hl) {
		event := h.claimed(true)
		h.editCriteria()
		h.refusal(h.rule(event, "verified", "v1", rrPassing, nil, nil))
	})
	rrMirror(t, "ReviewClaimedBeforeTheEdit.test_claiming_it_again_lets_the_review_finish", func(h *hl) {
		event := h.claimed(true)
		h.editCriteria()
		h.eq(h.claim(event, "re-review-turn"))
		record := h.mustRule(event, "verified", "v1", rrPassing, nil, nil)
		h.eq(replayed(record))
		h.eq(str(h.rrState(), "state"))
	})
	rrMirror(t, "ReviewClaimedBeforeTheEdit.test_an_unruled_re_review_needs_no_stated_digest", func(h *hl) {
		event := h.claimed(true)
		h.editCriteria()
		h.eq(h.claim(event, "re-review-turn"))
		record := h.mustRule(event, "verified", "v1", rrPassing, nil, nil)
		h.eq(field(record, "verdict"))
		h.eq(replayed(record))
	})
}

func Test21_RRD07_a_recorded_verdict_or_old_findings_cannot_certify_the_new_set(t *testing.T) {
	rrMirror(t, "AReClaimIsNotAFreePass.test_a_recorded_verdict_cannot_be_re_submitted_as_its_own_re_review", func(h *hl) {
		event := h.managedVerified()
		old := h.setDigest()
		h.editCriteria()
		h.claim(event, "re-review-turn")
		h.refusal(h.rule(event, "verified", "v1", rrPassing, nil, nil))
		h.refusal(h.rule(event, "verified", "v1", rrPassing, nil, old))
		h.eq(str(h.rrState(), "state"))
	})
	rrMirror(t, "ProtectionsThatMustSurvive.test_findings_made_against_the_old_wording_are_still_refused", func(h *hl) {
		event := h.managedVerified()
		h.editCriteria()
		h.refusal(h.rule(event, "verified", "v2", rrPassing, nil, nil))
	})
}

func Test21_RRD08_a_re_review_cannot_replace_a_certification_with_no_state(t *testing.T) {
	t.Run("unverified", func(t *testing.T) {
		rrMirror(t, "AReClaimIsNotAFreePass.test_a_re_review_cannot_strand_the_assignment_as_unverified", func(h *hl) {
			event := h.managedVerified()
			h.editCriteria()
			h.claim(event, "re-review-turn")
			h.refusal(h.rule(event, "unverified", "v2", rrFinding("unverified", "could not reach it"), nil, h.setDigest()))
			h.eq(str(h.rrState(), "state"))
			h.eq(str(h.rrState(), "nextExpectedAction"))
		})
	})
	t.Run("aborted", func(t *testing.T) {
		rrMirror(t, "AReClaimIsNotAFreePass.test_a_re_review_cannot_strand_the_assignment_as_aborted", func(h *hl) {
			event := h.managedVerified()
			h.editCriteria()
			h.claim(event, "re-review-turn")
			h.refusal(h.rule(event, "aborted", "v2", nil, "stopping", h.setDigest()))
			h.eq(str(h.rrState(), "state"))
		})
	})
	t.Run("first ruling unverified", func(t *testing.T) {
		rrMirror(t, "AReClaimIsNotAFreePass.test_an_event_with_no_ruling_can_still_be_recorded_unverified", func(h *hl) {
			event := h.claimed(true)
			record := h.mustRule(event, "unverified", "v1", rrFinding("unverified", "could not reach it"), nil, nil)
			h.eq(field(record, "verdict"))
		})
	})
}

func Test21_RRD09_the_protections_that_must_survive(t *testing.T) {
	t.Run("unchanged set replays", func(t *testing.T) {
		rrMirror(t, "ProtectionsThatMustSurvive.test_an_unchanged_criteria_set_still_replays", func(h *hl) {
			event := h.managedVerified()
			again := h.mustRule(event, "needs_changes", "v2", rrFinding("needs_changes", "on reflection, no"), nil, nil)
			h.eq(replayed(again))
			h.eq(field(again, "verdictTurnId"))
			h.eq(field(again, "verdict"))
		})
	})
	t.Run("second verified on an unchanged set", func(t *testing.T) {
		rrMirror(t, "ProtectionsThatMustSurvive.test_a_second_verified_ruling_on_an_unchanged_set_still_replays", func(h *hl) {
			event := h.managedVerified()
			again := h.mustRule(event, "verified", "v2", rrPassing, nil, nil)
			h.eq(replayed(again))
			h.eq(field(again, "verdictTurnId"))
		})
	})
	t.Run("generation advanced", func(t *testing.T) {
		rrMirror(t, "ProtectionsThatMustSurvive.test_a_criteria_edit_does_not_reopen_a_verdict_the_generation_left_behind", func(h *hl) {
			event := h.managedVerified()
			h.openGeneration("later", "needs_changes_revision", "later-turn")
			h.editCriteria()
			h.eq(h.claim(event, "re-review-turn"))
			again := h.mustRule(event, "unverified", "v2", rrFinding("unverified", "could not reach it"), nil, nil)
			h.eq(replayed(again))
			h.eq(field(again, "verdict"))
		})
	})
	t.Run("prior unverified", func(t *testing.T) {
		rrMirror(t, "ProtectionsThatMustSurvive.test_a_criteria_edit_after_unverified_does_not_reopen_the_old_event", func(h *hl) {
			event := h.claimed(true)
			h.mustRule(event, "unverified", "v1", rrFinding("unverified", "no access"), nil, nil)
			h.editCriteria()
			h.eq(h.claim(event, "re-review-turn"))
			again := h.mustRule(event, "verified", "v2", rrPassing, nil, h.setDigest())
			h.eq(replayed(again))
			h.eq(field(again, "verdict"))
		})
	})
	t.Run("prior needs_changes", func(t *testing.T) {
		rrMirror(t, "ProtectionsThatMustSurvive.test_a_criteria_edit_after_needs_changes_does_not_reopen_the_old_event", func(h *hl) {
			event := h.queuedEvent(regOpts{recipients: []string{parent, child}})
			h.registerCriteria(rrSet)
			h.attemptOn(event, h.host, nil)
			h.clock.Advance(5)
			turn := h.host.startTurn(parent, "ack-turn", "inProgress", "")
			_, err := h.ack.Acknowledge(h.ctx, event, turn.TurnID, AckProof(event, turn.TurnID), true, nil, h.host)
			mustDo(t, err)
			h.claim(event, turn.TurnID)
			h.mustRule(event, "needs_changes", "v1", rrFinding("needs_changes", "fix it"), nil, nil)
			h.editCriteria()
			h.eq(h.claim(event, "re-review-turn"))
			again := h.mustRule(event, "verified", "v2", rrPassing, nil, nil)
			h.eq(replayed(again))
		})
	})
	t.Run("paused", func(t *testing.T) {
		rrMirror(t, "ProtectionsThatMustSurvive.test_a_paused_assignment_is_not_re_reviewable", func(h *hl) {
			event := h.managedVerified()
			h.editCriteria()
			h.setStatusBy("paused", parent)
			h.eq(h.claim(event, "re-review-turn"))
		})
	})
}
