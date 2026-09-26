package registry

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ASG-1: state and next action follow the head: requested/child_emits, received, verifying,
// verified/coordinator_integrates, needs_changes then corrected, ambiguous. Every answer is
// compared whole with Python's AssignmentView.state.
func Test25_ASG1_state_and_next_action_follow_the_head(t *testing.T) {
	cases := []struct {
		scenario string
		states   []string
		actions  []string
	}{
		{"requested", []string{"requested"}, []string{"child_emits"}},
		{"received", []string{"received"}, nil},
		{"verifying", []string{"verifying"}, nil},
		{"verified", []string{"verified"}, []string{"coordinator_integrates"}},
		{"needs_changes_then_corrected", []string{"needs_changes", "needs_changes", "corrected"}, []string{"daemon_delivers_correction", "child_corrects"}},
		{"ambiguous", []string{"ambiguous"}, nil},
	}
	for _, c := range cases {
		t.Run(c.scenario, func(t *testing.T) {
			answers := sameScenario(t, c.scenario)
			for i, want := range c.states {
				if got := okOf(t, answers[i])["state"]; got != want {
					t.Fatalf("step %d state %v, want %s", i, got, want)
				}
			}
			for i, want := range c.actions {
				if got := okOf(t, answers[i])["nextExpectedAction"]; got != want {
					t.Fatalf("step %d nextExpectedAction %v, want %s", i, got, want)
				}
			}
		})
	}
}

// ASG-2: a pause outranks an older verdict, and a pause after a merge mark too.
func Test25_ASG2_a_pause_outranks_an_older_verdict(t *testing.T) {
	for _, name := range []string{"paused", "paused_after_mark"} {
		t.Run(name, func(t *testing.T) {
			if got := okOf(t, samePoint(t, name, 0))["state"]; got != StatePaused {
				t.Fatalf("state %v", got)
			}
		})
	}
}

// ASG-3: marking the verified head gives merged with the head's revision hash; marking an
// unverified head is refused not_acknowledged.
func Test25_ASG3_a_mark_on_the_verified_head_is_merged(t *testing.T) {
	record := okOf(t, samePoint(t, "mark_merged", 0))
	mark := record["mark"].(map[string]any)
	if record["state"] != StateMerged || mark["revisionHash"] != record["head"].(map[string]any)["revisionHash"] {
		t.Fatal(record)
	}
	if got := refusedOf(samePoint(t, "mark_unverified", 0)); got != "not_acknowledged" {
		t.Fatal(got)
	}
}

// ASG-4: a merge mark does not survive a new generation or a new revision; history keeps it.
func Test25_ASG4_a_mark_does_not_survive_a_new_generation_or_revision(t *testing.T) {
	for _, name := range []string{"mark_then_new_generation", "mark_then_new_revision"} {
		t.Run(name, func(t *testing.T) {
			record := okOf(t, samePoint(t, name, 0))
			if record["state"] == StateMerged || record["mark"] != nil || len(record["markHistory"].([]any)) != 1 {
				t.Fatal(record)
			}
		})
	}
}

// ASG-5: a mark naming a superseded revision, or no event at all, is stale_mark_context.
func Test25_ASG5_a_mark_must_name_the_current_event(t *testing.T) {
	for _, name := range []string{"mark_stale", "mark_without_expected_event"} {
		t.Run(name, func(t *testing.T) {
			if got := refusedOf(samePoint(t, name, 0)); got != "stale_mark_context" {
				t.Fatal(got)
			}
		})
	}
}

// ASG-6: a criteria edit after a verdict reads re_review_needed/parent_verifies with the old
// verdict kept, blocks the mark (criteria_set_changed) and stops an existing mark being state.
func Test25_ASG6_a_criteria_edit_after_a_verdict_needs_re_review(t *testing.T) {
	answers := sameScenario(t, "criteria_edited")
	if okOf(t, answers[0])["state"] != StateVerified {
		t.Fatal(answers[0])
	}
	edited := okOf(t, answers[1])
	criteria := edited["criteria"].(map[string]any)
	if edited["state"] != StateRereview || edited["nextExpectedAction"] != "parent_verifies" || criteria["current"] != false ||
		edited["lastVerdict"].(map[string]any)["verdict"] != "verified" || criteria["reviewedSetDigest"] == criteria["setDigest"] {
		t.Fatal(edited)
	}
	if refusedOf(answers[2]) != "criteria_set_changed" {
		t.Fatal(answers[2])
	}
	after := okOf(t, samePoint(t, "criteria_edited_after_mark", 0))
	if after["state"] != StateRereview || after["mark"] != nil || len(after["markHistory"].([]any)) != 1 {
		t.Fatal(after)
	}
}

// ASG-7: one issue, one responsible child: a rival is refused duplicate_assignment (also while
// the owner is paused), archiving releases the issue, the same pair is idempotent and
// supersedes replaces deliberately.
func Test25_ASG7_one_issue_one_responsible_child(t *testing.T) {
	for name, refused := range map[string]bool{"duplicate_active": true, "duplicate_paused": true,
		"duplicate_archived": false, "duplicate_same_pair": false, "duplicate_supersedes": false} {
		t.Run(name, func(t *testing.T) {
			answer := samePoint(t, name, 0)
			if (refusedOf(answer) == "duplicate_assignment") != refused {
				t.Fatal(answer)
			}
		})
	}
}

// ASG-9: assignment-find names the responsible child and relationship, a paused owner too, and
// answers null with no assignments for an unknown issue.
func Test25_ASG9_for_issue_names_the_child_to_reuse(t *testing.T) {
	answers := sameScenario(t, "for_issue_owner")
	empty := okOf(t, answers[1])
	if empty["responsibleChild"] != nil || len(empty["assignments"].([]any)) != 0 {
		t.Fatal(empty)
	}
	owner := okOf(t, answers[2])
	if owner["responsibleChild"] != child || owner["responsibleRelationship"] == nil {
		t.Fatal(owner)
	}
	if okOf(t, samePoint(t, "for_issue_paused", 0))["responsibleChild"] != child {
		t.Fatal("a paused owner is not named")
	}
}

// ASG-10: the answer names the store it came from: holds false before and true after
// registration (and for a paused owner), always equal to responsibleRelationship != null; the
// store carries its id, path and identity with identified true and detail null.
func Test25_ASG10_the_answer_names_the_store_it_came_from(t *testing.T) {
	answers := sameScenario(t, "for_issue_owner")
	for i, wantHolds := range []bool{false, false, true, false} {
		found := okOf(t, answers[i])
		relay := found["relay"].(map[string]any)
		if relay["holds"] != wantHolds || relay["holds"] != (found["responsibleRelationship"] != nil) {
			t.Fatalf("step %d: %v", i, relay)
		}
		st := relay["store"].(map[string]any)
		if st["identified"] != true || st["detail"] != nil || st["storeId"] == nil || st["recordedSocket"] != nil {
			t.Fatalf("step %d: %v", i, st)
		}
	}
	paused := okOf(t, samePoint(t, "for_issue_paused", 0))
	if paused["relay"].(map[string]any)["holds"] != true {
		t.Fatal(paused)
	}
}

// ASG-8: two concurrent registrations for one issue, on two stores over one file, produce
// exactly one assignment; the other is refused duplicate_assignment inside its transaction.
func Test25_ASG8_two_concurrent_registrations_produce_one_assignment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	var stores [2]*store.Store
	for i := range stores {
		s, err := store.Open(ctx(), path, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		stores[i] = s
	}
	children := [2]string{child, "01other-child"}
	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &Registry{Store: stores[i], Now: func() string { return fakeISO }}
			in := fixture()
			in.Child = Endpoint{children[i], host, ns(root), sql.NullString{}}
			in.DispatchRequestID, in.DispatchTurnID = "dispatch-"+children[i], ns("turn-"+children[i])
			<-start
			_, errs[i] = r.Register(ctx(), in)
		}()
	}
	close(start)
	wg.Wait()
	won, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case refusalReason(err) == string(contract.RefusalDuplicateAssignment):
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	var owning int
	if err := stores[0].DB.QueryRowContext(ctx(), "SELECT COUNT(*) FROM relationships WHERE issue_key = ? AND status = 'active'", issue).Scan(&owning); err != nil {
		t.Fatal(err)
	}
	if won != 1 || refused != 1 || owning != 1 {
		t.Fatalf("won %d refused %d owning %d: %v", won, refused, owning, errs)
	}
}

// ASG-10 (identity rows): two stores are distinguishable by the reading alone, and
// recordedSocket is the socket that CREATED the store (first write wins), null without one.
func Test25_ASG10_two_stores_and_the_recorded_socket(t *testing.T) {
	dir := t.TempDir()
	read := func(s *store.Store) map[string]any {
		found, err := NewAssignmentView(&Registry{Store: s}).ForIssue(ctx(), issue)
		if err != nil {
			t.Fatal(err)
		}
		return plain(t, found).(map[string]any)["relay"].(map[string]any)["store"].(map[string]any)
	}
	open := func(path, socket string) *store.Store {
		s, err := store.Open(ctx(), path, socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	mine, theirs := read(open(filepath.Join(dir, "a", "relay.sqlite3"), "")), read(open(filepath.Join(dir, "b", "relay.sqlite3"), ""))
	if mine["storeId"] == theirs["storeId"] || mine["inode"] == theirs["inode"] || mine["recordedSocket"] != nil {
		t.Fatal(mine, theirs)
	}
	path := filepath.Join(dir, "socketed", "relay.sqlite3")
	first := read(open(path, filepath.Join(dir, "crw125-first.sock")))["recordedSocket"].(string)
	again := read(open(path, filepath.Join(dir, "crw125-second.sock")))["recordedSocket"]
	if !strings.HasSuffix(first, "crw125-first.sock") || again != first {
		t.Fatal(first, again)
	}
}

func projectionOf(t *testing.T, answer map[string]any) map[string]any {
	return okOf(t, answer)["projection"].(map[string]any)
}

// ASG-11: the projection keeps its axes apart: staged has no delivery; queued is final/queued
// with no settlement and tier unrecorded, then dispatched; the request id names the current
// attempt; a generation with no correction answers null with "no such event".
func Test25_ASG11_the_projection_keeps_its_axes_apart(t *testing.T) {
	staged := projectionOf(t, sameScenario(t, "test_a_staged_event_has_no_delivery_at_all")[0])["completion"].(map[string]any)
	if staged["event"].(map[string]any)["stage"] != "staged" || staged["delivery"] != nil {
		t.Fatal(staged)
	}
	life := sameScenario(t, "test_the_axes_keep_their_own_vocabulary_through_the_lifecycle")
	queued := projectionOf(t, life[0])["completion"].(map[string]any)
	ack := queued["ack"].(map[string]any)
	if queued["delivery"].(map[string]any)["state"] != "queued" || ack["settlement"] != nil || ack["evidenceTier"] != "unrecorded" {
		t.Fatal(queued)
	}
	if projectionOf(t, life[1])["completion"].(map[string]any)["delivery"].(map[string]any)["state"] != "dispatched" {
		t.Fatal(life[1])
	}
	current := projectionOf(t, sameScenario(t, "test_the_request_id_names_the_current_attempt_not_the_first")[0])["completion"].(map[string]any)["delivery"].(map[string]any)
	if current["attemptNo"] != float64(1) || current["requestId"] == nil {
		t.Fatal(current)
	}
	none := projectionOf(t, sameScenario(t, "test_a_generation_with_no_correction_answers_null_not_a_borrowed_row")[0])["correction"].(map[string]any)
	if none["eventId"] != nil || none["delivery"] != nil || !strings.Contains(none["detail"].(string), "no such event") {
		t.Fatal(none)
	}
}

// ASG-12: projection.verdict equals lastVerdict and projection.assignment.state equals state.
func Test25_ASG12_the_verdict_and_state_are_referenced(t *testing.T) {
	for _, name := range []string{"verdict_referenced", "verified", "needs_changes_then_corrected"} {
		for _, answer := range sameScenario(t, name) {
			record := okOf(t, answer)
			projection := record["projection"].(map[string]any)
			if !reflect.DeepEqual(projection["verdict"], record["lastVerdict"]) || projection["assignment"].(map[string]any)["state"] != record["state"] {
				t.Fatal(name, record)
			}
		}
	}
}

// ASG-13: a verified rejection reads as a rejection.
func Test25_ASG13_a_verified_rejection_reads_as_a_rejection(t *testing.T) {
	ack := projectionOf(t, sameScenario(t, "test_a_verified_rejection_does_not_read_like_a_verified_acceptance")[0])["completion"].(map[string]any)["ack"].(map[string]any)
	if ack["accepted"] != false || ack["rejectionReason"] != "revision_mismatch" || ack["settlement"] != "verified" {
		t.Fatal(ack)
	}
}

// ASG-14: a historical refusal stops being the reason once a delivery exists.
func Test25_ASG14_a_refusal_stops_being_the_reason_once_delivered(t *testing.T) {
	completion := projectionOf(t, sameScenario(t, "test_a_refusal_stops_being_the_reason_once_a_delivery_exists")[0])["completion"].(map[string]any)
	if completion["delivery"].(map[string]any)["state"] != "dispatched" || completion["undeliveredReason"] != nil {
		t.Fatal(completion)
	}
}

// ASG-15: one anchor's whole lifecycle is read in ONE statement, for a completion and for a
// withheld correction.
func Test25_ASG15_one_anchor_is_read_in_one_statement(t *testing.T) {
	for _, c := range []struct{ scenario, projection string }{
		{"test_the_request_id_names_the_current_attempt_not_the_first", "completion"},
		{"test_a_correction_withheld_for_an_archived_child_names_the_withhold", "correction"},
	} {
		point := assignmentScenario(t, c.scenario)[0]
		dir := filepath.Join(t.TempDir(), "state")
		s, err := store.Open(ctx(), filepath.Join(dir, "relay.sqlite3"), "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if _, err := s.DB.ExecContext(ctx(), "DELETE FROM schema_meta"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx(), point.SQL); err != nil {
			t.Fatal(err)
		}
		want := okOf(t, point.Result)["projection"].(map[string]any)[c.projection].(map[string]any)
		var statements []string
		view := &AssignmentView{R: &Registry{Store: s}, Clock: func() float64 { return point.Now }, Policy: DefaultRetryPolicy,
			statement: func(q string) { statements = append(statements, q) }}
		got, err := view.Anchored(ctx(), want["eventId"], int64(want["executionGeneration"].(float64)))
		if err != nil {
			t.Fatal(err)
		}
		if len(statements) != 1 {
			t.Fatalf("%s: the lifecycle came from %d statements", c.scenario, len(statements))
		}
		if !reflect.DeepEqual(plain(t, got), any(want)) {
			t.Fatalf("%s: go %v\npy %v", c.scenario, plain(t, got), want)
		}
	}
}

func correctionOf(t *testing.T, answer map[string]any) (map[string]any, map[string]any) {
	record := okOf(t, answer)
	return record, record["projection"].(map[string]any)["correction"].(map[string]any)
}

// ASG-16: an unsent correction names why and who acts next, row by row as Python answers.
func Test25_ASG16_an_unsent_correction_names_why_and_who_acts(t *testing.T) {
	type want struct {
		action, source, value string
	}
	for name, w := range map[string]want{
		"test_a_correction_withheld_for_an_archived_child_names_the_withhold":             {"daemon_delivers_correction", LifecycleWithholdSource, "recipient_archived"},
		"test_an_archive_state_that_cannot_be_read_is_named_lifecycle_unknown":            {"daemon_delivers_correction", LifecycleWithholdSource, "lifecycle_unknown"},
		"test_a_freshly_queued_correction_waits_on_the_relay_with_no_reason_yet":          {"daemon_delivers_correction", "", ""},
		"test_a_dispatched_correction_is_the_childs_to_act_on":                            {"child_corrects", "", ""},
		"test_a_later_settings_withhold_is_not_reported_as_the_lifecycle":                 {"operator_restores_recipient_settings", "", ""},
		"test_a_held_correction_is_the_parents_to_recover":                                {"parent_recovers_held_correction", "deliveries.hold_reason", "attempt_cap"},
		"test_a_correction_stored_without_waking_the_child_is_the_parents_to_recover":     {"parent_recovers_held_correction", "deliveries.hold_reason", "push_channel_closed"},
		"test_a_correction_whose_send_is_uncertain_waits_on_the_relay_to_confirm_it":      {"daemon_confirms_correction", "", ""},
		"test_a_correction_claimed_and_not_yet_answered_waits_on_the_relay_to_confirm_it": {"daemon_confirms_correction", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			record, correction := correctionOf(t, sameScenario(t, name)[0])
			reason, _ := correction["undeliveredReason"].(map[string]any)
			if record["nextExpectedAction"] != w.action || (w.source == "") != (reason == nil) ||
				(reason != nil && (reason["source"] != w.source || reason["value"] != w.value)) {
				t.Fatal(record["nextExpectedAction"], correction["undeliveredReason"])
			}
		})
	}
}

// ASG-17: a pause names the relationship; after resume the reason is null until the next
// reading, which names the lifecycle again.
func Test25_ASG17_a_pause_names_the_relationship(t *testing.T) {
	answers := sameScenario(t, "test_a_pause_names_the_relationship_and_a_resume_waits_for_the_next_reading")
	record, paused := correctionOf(t, answers[0])
	reason := paused["undeliveredReason"].(map[string]any)
	if record["state"] != StatePaused || reason["value"] != "relationship_not_active" || reason["relationshipStatus"] != "paused" {
		t.Fatal(record)
	}
	if _, resumed := correctionOf(t, answers[1]); resumed["undeliveredReason"] != nil {
		t.Fatal(resumed)
	}
	if _, again := correctionOf(t, answers[2]); again["undeliveredReason"].(map[string]any)["value"] != "recipient_archived" {
		t.Fatal(again)
	}
}

// ASG-18: a correction the child already answered is not awaiting delivery.
func Test25_ASG18_an_answered_correction_is_the_parents_to_read(t *testing.T) {
	answers := sameScenario(t, "test_a_correction_the_child_already_answered_is_not_awaiting_delivery")
	record, correction := correctionOf(t, answers[0])
	if correction["supersession"] == nil || record["state"] != StateNeedsChanges || record["nextExpectedAction"] != ActionCorrectionAnswered {
		t.Fatal(record)
	}
	record, correction = correctionOf(t, answers[1])
	if correction["delivery"].(map[string]any)["state"] != "superseded" || record["nextExpectedAction"] != ActionCorrectionAnswered {
		t.Fatal(record)
	}
}

// ASG-19: the reason is tied to this event's withhold transition by an identical stamp.
func Test25_ASG19_the_reason_is_tied_to_the_withhold_by_its_stamp(t *testing.T) {
	value := func(answer map[string]any) any {
		_, correction := correctionOf(t, answer)
		reason, _ := correction["undeliveredReason"].(map[string]any)
		if reason == nil {
			return nil
		}
		return reason["value"]
	}
	if got := value(sameScenario(t, "test_another_reading_of_the_same_task_does_not_move_this_events_reason")[0]); got != "recipient_archived" {
		t.Fatal(got)
	}
	if got := value(sameScenario(t, "test_an_identical_stamp_on_another_operation_is_not_guessed")[0]); got != nil {
		t.Fatal(got)
	}
	late := sameScenario(t, "test_a_late_record_of_an_older_attempt_does_not_hide_the_reason")
	if value(late[0]) != "recipient_archived" || value(late[1]) != nil {
		t.Fatal(late)
	}
	if got := value(sameScenario(t, "test_the_reason_needs_the_withhold_and_its_record_to_share_one_stamp")[0]); got != "recipient_archived" {
		t.Fatal(got)
	}
}
