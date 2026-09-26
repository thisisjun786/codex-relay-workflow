package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Every DSP test replays its cli-shape fixtures (contract/fixtures/cli-shape/test_dispositions__*)
// through the Go relay CLI and compares exit code and whole stdout with what the Python CLI
// printed for the same fixture (testdata/python_dispositions.json), then asserts the property's
// own values on the Go answer.

// goDisposition runs one fixture step and decodes its stdout.
func goDisposition(t *testing.T, name, step string) map[string]any {
	t.Helper()
	got := runDispositionsFixture(t, "test_dispositions__"+name+".json")
	var out map[string]any
	if err := json.Unmarshal([]byte(got[step][1]), &out); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, got[step][1])
	}
	return out
}

func firstChild(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	children := answer["children"].([]any)
	if len(children) == 0 {
		t.Fatalf("no children: %v", answer)
	}
	return children[0].(map[string]any)
}

func firstEventDelivery(t *testing.T, name string) map[string]any {
	t.Helper()
	child := firstChild(t, goDisposition(t, name, "0"))
	return child["events"].([]any)[0].(map[string]any)["delivery"].(map[string]any)
}

// DSP-1: the turn disposition for the current generation (none, sole, reviewable never chosen,
// newest of the same outcome, contested, suppressed and staged never chosen, first_seen_at order).
func Test25_DSP1_the_turn_disposition_for_the_current_generation(t *testing.T) {
	sameDispositionsAsPython(t, "reported_nothing", "single_blocked", "reviewable_event", "several_reviewable",
		"same_outcome", "disagreeing_outcomes", "suppressed_claim_is_listed", "staged_claim_is_listed", "no_finalized_at")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	basis := func(name string) map[string]any {
		return firstChild(t, goDisposition(t, name, "0"))["turnDisposition"].(map[string]any)
	}
	if d := basis("test_a_child_that_reported_nothing_is_not_a_child_that_is_fine"); d["basis"] != "none" || d["outcome"] != nil || !strings.Contains(d["detail"].(string), "not") {
		t.Fatal(d)
	}
	if d := basis("test_a_single_blocked_event_is_the_disposition"); d["basis"] != "sole" {
		t.Fatal(d)
	}
	child := firstChild(t, goDisposition(t, "test_a_reviewable_event_is_counted_and_never_becomes_the_disposition", "0"))
	if r := child["reviewable"].(map[string]any); child["turnDisposition"].(map[string]any)["basis"] != "none" || r["head"] != "not_derived_here" {
		t.Fatal(child)
	}
	if d := basis("test_two_events_with_the_same_outcome_anchor_to_the_newest_and_keep_both"); d["basis"] != "latest_of_same_outcome" || d["eventId"] != "e2" {
		t.Fatal(d)
	}
	for _, v := range []string{"equal", "newer"} {
		if d := basis("test_disagreeing_outcomes_are_a_contest_even_when_one_is_newer__" + v); d["basis"] != "contested" || d["outcome"] != nil {
			t.Fatal(d)
		}
	}
	if d := basis("test_a_directly_final_event_with_no_finalized_at_still_orders"); d["eventId"] != "e1" {
		t.Fatal(d)
	}
}

// DSP-2: earlier-generation events are counted, not listed.
func Test25_DSP2_earlier_generations_are_counted_not_listed(t *testing.T) {
	sameDispositionsAsPython(t, "earlier_generations_are_counted")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	child := firstChild(t, goDisposition(t, "test_earlier_generations_are_counted_and_not_listed", "0"))
	if child["earlierGenerationEvents"] != float64(2) || len(child["events"].([]any)) != 1 {
		t.Fatal(child)
	}
}

// DSP-3: the work report carries the CXC status; no report is recorded false and counted.
func Test25_DSP3_the_work_report_separates_what_one_outcome_collapses(t *testing.T) {
	sameDispositionsAsPython(t, "recorded_report", "no_report_says")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	report := firstChild(t, goDisposition(t, "test_a_recorded_report_carries_the_status_that_separates_them", "0"))["events"].([]any)[0].(map[string]any)["workReport"].(map[string]any)
	if report["recorded"] != true || report["cxcStatus"] != "NEEDS_HUMAN" || report["submissionNo"] != float64(2) {
		t.Fatal(report)
	}
	answer := goDisposition(t, "test_no_report_says_the_separation_is_unavailable_rather_than_guessing", "0")
	missing := firstChild(t, answer)["events"].([]any)[0].(map[string]any)["workReport"].(map[string]any)
	if missing["recorded"] != false || missing["cxcStatus"] != nil || answer["counts"].(map[string]any)["workReportMissing"] != float64(1) {
		t.Fatal(answer)
	}
}

// DSP-4: the send axis word per event, with its exact detail.
func Test25_DSP4_the_send_axis_names_what_the_store_observed(t *testing.T) {
	sameDispositionsAsPython(t, "no_delivery_and_no_intent", "intent_without", "queued_delivery", "dispatched_delivery_says",
		"inbox_only", "uncertain_send", "supersession_note", "not_folded", "staged_event_is_never", "suppressed_event", "acknowledgement_with_no_delivery")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for name, word := range map[string]string{
		"test_no_delivery_and_no_intent_is_unmeasured_not_undelivered":                  obsUnmeasured,
		"test_an_intent_without_a_delivery_is_a_refusal_that_may_not_last":              obsRefusedPreQueue,
		"test_a_queued_delivery_is_not_sent_and_carries_the_stores_own_word":            obsNotSent,
		"test_a_dispatched_delivery_says_dispatched_and_nothing_more":                   obsSent,
		"test_an_inbox_only_delivery_is_stored_not_woken":                               obsStoredNotWoken,
		"test_an_uncertain_send_is_not_evidence_of_non_delivery__sending":               obsSendUncertain,
		"test_an_uncertain_send_is_not_evidence_of_non_delivery__held_uncertain":        obsSendUncertain,
		"test_a_supersession_note_outranks_the_state_but_never_erases_it":               obsSuperseded,
		"test_a_state_this_reader_has_no_word_for_is_not_folded_into_one":               obsUnrecognised,
		"test_a_staged_event_is_never_reported_as_delivered":                            "not_deliverable:staged",
		"test_a_suppressed_event_is_called_suppressed_rather_than_staged":               obsSuppressed,
		"test_an_acknowledgement_with_no_delivery_row_is_a_disagreement_not_unmeasured": obsDisagree,
	} {
		if got := firstEventDelivery(t, name)["observation"]; got != word {
			t.Fatalf("%s: %v, want %s", name, got, word)
		}
	}
	if d := firstEventDelivery(t, "test_no_delivery_and_no_intent_is_unmeasured_not_undelivered"); d["detail"] != UnmeasuredDetail || !strings.Contains(UnmeasuredDetail, "--no-enqueue") {
		t.Fatal(d)
	}
}

// DSP-5: every delivery state has a word and every word has a detail entry.
func Test25_DSP5_every_delivery_state_has_a_word_with_a_detail(t *testing.T) {
	sameDispositionsAsPython(t, "every_delivery_state", "every_word")
	states := []string{"queued", "deferred_busy", "withheld_pre_send", "sending", "held_uncertain", "inbox_only", "dispatched", "acknowledged", "superseded"}
	if len(observationByState) != len(states) {
		t.Fatalf("%d states mapped, transport defines %d", len(observationByState), len(states))
	}
	for _, state := range states {
		word, ok := observationByState[state]
		if !ok {
			t.Fatalf("state %s has no word", state)
		}
		if _, ok := observationDetail[word]; !ok {
			t.Fatalf("word %s has no detail entry", word)
		}
	}
}

// DSP-6: the read selects exactly the four outcomes; the execution-only three are dispositions.
func Test25_DSP6_the_read_selects_the_stores_four_outcomes(t *testing.T) {
	sameDispositionsAsPython(t, "execution_only_outcomes", "selects_exactly")
	if strings.Join(executionOnly, ",") != "failed,interrupted,blocked_needs_input" {
		t.Fatal(executionOnly)
	}
	for _, outcome := range []string{"ready_for_review", "failed", "interrupted", "blocked_needs_input"} {
		if !strings.Contains(dispositionsSQL, "'"+outcome+"'") {
			t.Fatal(outcome)
		}
	}
}

// DSP-7: the receiving axis, and the acknowledgement block's fields.
func Test25_DSP7_the_receiving_axis_is_measured_apart(t *testing.T) {
	sameDispositionsAsPython(t, "receiving_side_unmeasured", "host_read_acknowledgement", "unverified_acknowledgement",
		"evidence_with_no_acknowledgement", "acknowledgement_block", "no_acknowledgement_row")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for name, word := range map[string]string{
		"test_a_dispatched_send_still_leaves_the_receiving_side_unmeasured":         obsUnmeasured,
		"test_a_host_read_acknowledgement_is_an_observation":                        obsHostRead,
		"test_an_unverified_acknowledgement_is_a_claim_rather_than_an_observation":  obsClaimed,
		"test_evidence_with_no_acknowledgement_beside_it_has_no_supportable_answer": obsDisagree,
	} {
		if got := firstEventDelivery(t, name)["recipientObservation"]; got != word {
			t.Fatalf("%s: %v", name, got)
		}
	}
	ack := firstChild(t, goDisposition(t, "test_no_acknowledgement_row_is_unrecorded_rather_than_unverified", "0"))["events"].([]any)[0].(map[string]any)["acknowledgement"].(map[string]any)
	if ack["recorded"] != false || ack["evidenceTier"] != "unrecorded" || ack["settlement"] != nil {
		t.Fatal(ack)
	}
}

// DSP-8: counts agree with the list they summarise.
func Test25_DSP8_the_counts_agree_with_the_list(t *testing.T) {
	sameDispositionsAsPython(t, "counts_cannot_disagree")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	answer := goDisposition(t, "test_the_counts_cannot_disagree_with_the_list_they_summarise", "0")
	children := answer["children"].([]any)
	counts := answer["counts"].(map[string]any)
	blocked := 0
	for _, c := range children {
		if c.(map[string]any)["turnDisposition"].(map[string]any)["outcome"] == "blocked_needs_input" {
			blocked++
		}
	}
	if counts["children"] != float64(len(children)) || counts["blocked"] != float64(blocked) {
		t.Fatal(counts)
	}
}

// DSP-9: an unreadable store is not an empty one: exit 2, readable false, nothing created, an
// unrelated file left byte-untouched.
func Test25_DSP9_an_unreadable_store_is_not_an_empty_one(t *testing.T) {
	sameDispositionsAsPython(t, "directory_with_no_database", "not_a_relay_database", "unreadable_store_refuses", "no_unrelated_file")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	home := t.TempDir()
	state := filepath.Join(home, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(state, "relay.sqlite3")
	if err := os.WriteFile(db, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := ExecuteAs(ctx(), "codex-session-relay", []string{"--state", state, "dispositions-show", "--project", "P"}, &stdout, &stderr, nil)
	var answer map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(db)
	entries, _ := os.ReadDir(state)
	if code != 2 || answer["readable"] != false || answer["detail"] == nil || len(answer["children"].([]any)) != 0 || info.Size() != 0 || len(entries) != 1 {
		t.Fatal(code, answer, info.Size(), entries)
	}
}

// DSP-10: a database replaced during the read (readable, no rows, a replacement detail) is
// answered readable false with that detail, never "no children".
func Test25_DSP10_a_replaced_database_is_unreadable_not_empty(t *testing.T) {
	replaced := func(context.Context, store.StateSelection, string, []any, func(store.RowScanner) error) store.RowsRead {
		return store.RowsRead{Readable: true, Detail: "the database was replaced while it was being read"}
	}
	project := "PROJ-CRW-163"
	answer := plain(t, readDispositions(ctx(), store.StateSelection{Path: t.TempDir()}, &project, nil, replaced)).(map[string]any)
	if answer["readable"] != false || len(answer["children"].([]any)) != 0 || answer["detail"] != "the database was replaced while it was being read" {
		t.Fatal(answer)
	}
}

// DSP-11: --project lists live scoped children, --relationship shows any status; the selectors
// are mutually exclusive and one is required (exit 2 on stderr, as argparse).
func Test25_DSP11_the_selectors(t *testing.T) {
	sameDispositionsAsPython(t, "superseded_assignment_is_not", "two_selectors")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	answer := goDisposition(t, "test_a_superseded_assignment_is_not_called_merely_inactive", "0")
	if firstChild(t, answer)["relationshipStatus"] != "archived" {
		t.Fatal(answer)
	}
	blocked := firstChild(t, goDisposition(t, "test_a_single_blocked_event_is_the_disposition", "0"))
	if blocked["relationshipId"] != "rel-1" || blocked["issueKey"] != "CRW-1" || blocked["childTaskId"] != "01child-task" {
		t.Fatal(blocked)
	}
	dir := t.TempDir()
	for _, argv := range [][]string{{"dispositions-show"}, {"dispositions-show", "--project", "P", "--relationship", "r"}} {
		var stdout, stderr bytes.Buffer
		code := ExecuteAs(ctx(), "codex-session-relay", append([]string{"--state", dir}, argv...), &stdout, &stderr, nil)
		if code != 2 || stdout.Len() != 0 {
			t.Fatal(argv, code, stdout.String())
		}
	}
	var stderr bytes.Buffer
	ExecuteAs(ctx(), "codex-session-relay", []string{"--state", dir, "dispositions-show"}, &bytes.Buffer{}, &stderr, nil)
	want := "usage: codex-session-relay dispositions-show [-h] (--project PROJECT | --relationship RELATIONSHIP)\n" +
		"codex-session-relay dispositions-show: error: one of the arguments --project --relationship is required\n"
	if stderr.String() != want {
		t.Fatalf("%q", stderr.String())
	}
}

// DSP-13: the correction block: none, lifecycle withhold named, withheld without a record still
// counted, a busy deferral not withheld, paused, superseded, held, dispatched, unknown state and
// already answered - each whole answer as Python printed it.
func Test25_DSP13_the_correction_block(t *testing.T) {
	sameDispositionsAsPython(t, "without_a_correction", "withheld_by_the_lifecycle", "without_a_lifecycle_record", "busy_deferral",
		"paused_assignment_names", "superseded_assignment", "held_correction_names", "dispatched_correction", "is_said_so", "already_answered")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	answer := goDisposition(t, "test_a_correction_withheld_by_the_lifecycle_is_named", "0")
	reason := firstChild(t, answer)["correction"].(map[string]any)["undeliveredReason"].(map[string]any)
	counts := answer["counts"].(map[string]any)
	if reason["source"] != LifecycleWithholdSource || counts["correctionNotSent"] != float64(1) || counts["correctionWithheld"] != float64(1) {
		t.Fatal(answer)
	}
	if c := firstChild(t, goDisposition(t, "test_a_child_without_a_correction_has_none_and_counts_nothing", "0")); c["correction"] != nil {
		t.Fatal(c)
	}
	answered := goDisposition(t, "test_a_correction_the_child_already_answered_is_superseded_and_not_counted", "0")
	delivery := firstChild(t, answered)["correction"].(map[string]any)["delivery"].(map[string]any)
	if delivery["observation"] != obsSuperseded || answered["counts"].(map[string]any)["correctionNotSent"] != float64(0) {
		t.Fatal(answered)
	}
}
