package cli_test

import (
	"context"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-903: the findings the reviewer raised on the CRW-737 bundle pull request, at the command
// level. The reply a decision is applied on must match the chosen option's reply, a user-grade
// answer must come from the store-scope supervisor seat, and the applied generation is the reply
// event's own execution generation.

// review818SeedSupervisor binds taskID as the relay's store-scope supervisor: the seat a
// user-grade or delegated-management-grade answer must come from (registry.StoreScopeSupervisor).
func review818SeedSupervisor(t *testing.T, state, taskID string) {
	t.Helper()
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if _, err := opened.Querier(ctx).ExecContext(ctx,
		"INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, cwd, cxc_session, status, revision, supersedes, superseded_by, handover_note, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"sb-903", "supervisor", "store", "store", taskID, "host", nil, nil, "active", int64(1), nil, nil, nil, "2026-10-06T00:00:00Z", "2026-10-06T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
}

// review818SeedReply writes the relationship, its generation and the relay's own decision_reply
// event for answersEvent, with the decision and execution generation the case names, and returns
// the event id delivery.DecisionEventID computes for that pair.
func review818SeedReply(t *testing.T, state, relationship, answersEvent, decision string, generation int64) string {
	t.Helper()
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	event := delivery.DecisionEventID(relationship, answersEvent)
	receipt := `{"eventId":"` + event + `","relationshipId":"` + relationship + `","executionGeneration":` + strconv.FormatInt(generation, 10) +
		`,"kind":"decision_reply","decision":"` + decision + `","answersEvent":"` + answersEvent +
		`","note":"n","generationEffect":"stays","anchorTurnId":"turn-1","childTaskId":"child","decidedAt":"2026-10-06T00:00:00Z"}`
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
			[]any{relationship, "CRW-903", "active", "parent", "host", "child", "host", generation, "[]", "[\"child\"]", "2026-10-06T00:00:00Z", "2026-10-06T00:00:00Z"}},
		{"INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, opened_at) VALUES (?,?,?,?,?,?)",
			[]any{relationship, int64(1), "dispatch-1", "bound", "turn-1", "2026-10-06T00:00:00Z"}},
		{"INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
			[]any{event, relationship, generation, "no_deliverable", "decision_reply", "relay", "thread", "turn-1", "completed", receipt, "final", "2026-10-06T00:00:00Z", "2026-10-06T00:00:00Z"}},
		// The relay queues the reply's delivery when it records the event; a decision applies only
		// once that delivery is accepted by the child, so the fixture records it as dispatched.
		{"INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, created_at, updated_at) VALUES (?,?,?,?,?,?,0,?,?)",
			[]any{event, relationship, delivery.Revision, "child", "child", delivery.Dispatched, "2026-10-06T00:00:00Z", "2026-10-06T00:00:00Z"}},
	} {
		if _, err := opened.Querier(ctx).ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.query, err)
		}
	}
	return event
}

// review818Raise is one raise of the plain question these cases answer, and the decision id the
// raise answered.
func review818Raise(t *testing.T, state, question string) string {
	t.Helper()
	raised := crw737JSON(t, crw737Raise(t, state, "PRJ-A", question))
	decision, _ := raised["decisionId"].(string)
	if decision == "" {
		t.Fatalf("the raise answered %v", raised)
	}
	return decision
}

// review818RaiseBlocker raises the relationship-blocking question the apply cases use: the hold
// option replies stop and the merge option replies answer, so a reply event that decided stop is
// the one that applies the record.
func review818RaiseBlocker(t *testing.T, state string) string {
	t.Helper()
	raised := crw737JSON(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "merge_approval",
		"--context", "Hold the merge until the retention decision?", "--option", "hold=hold:hold the merge",
		"--option", "merge=merge:merge now", "--option-reply", "hold=stop", "--option-reply", "merge=answer",
		"--blocking", "relationship=rel-903", "--origin-project", "PRJ-A", "--source", "event=ev-blocked",
		"--authority", "user"))
	decision, _ := raised["decisionId"].(string)
	if decision == "" {
		t.Fatalf("the raise answered %v", raised)
	}
	return decision
}

// review818AnswerUser records the supervisor's own user-grade answer choosing option.
func review818AnswerUser(t *testing.T, state, decision, option string) {
	t.Helper()
	if got := crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", option,
		"--by", "task-sup", "--via", "dots", "--authority", "user"); got.code != 0 {
		t.Fatalf("decision-answer: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
}

// review818Exec runs one statement against the case's store, so a fixture can write the rows the
// relay would hold: a delivery's state, an event's first_seen_at, a field an older build wrote.
func review818Exec(t *testing.T, state, query string, args ...any) {
	t.Helper()
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if _, err := opened.Querier(ctx).ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// review818SetDeliveryState moves the reply event's delivery to deliveryState, so a case can show
// the apply waiting for the child to receive the reply.
func review818SetDeliveryState(t *testing.T, state, event, deliveryState string) {
	t.Helper()
	review818Exec(t, state, "UPDATE deliveries SET state = ? WHERE event_id = ?", deliveryState, event)
}

// review818SetFirstSeenAt moves the reply event's first_seen_at, the instant the relay recorded it.
func review818SetFirstSeenAt(t *testing.T, state, event, at string) {
	t.Helper()
	review818Exec(t, state, "UPDATE events SET first_seen_at = ? WHERE event_id = ?", at, event)
}

// review818SetRaisedAt writes raised_at straight into the record: the value an older build could
// write, which the reader preserves and a re-raise must not fail on.
func review818SetRaisedAt(t *testing.T, state, decisionID, value string) {
	t.Helper()
	review818Exec(t, state, "UPDATE dag_user_decisions SET raised_at = ? WHERE decision_id = ?", value, decisionID)
}

// The option-reply vocabulary is the decision vocabulary the delivery package records on the
// decision_reply event. The apply compares the two by string, and the decisions package cannot
// import delivery (delivery imports store, store imports decisions), so the two lists are pinned
// to each other here: a rename on either side is a failing test rather than a silent drift that
// stops every decision applying.
func TestReview818OptionReplyVocabularyMatchesTheDecisionKinds(t *testing.T) {
	want := []string{delivery.DecisionAnswer, delivery.DecisionStop, delivery.DecisionSplitApproval, delivery.DecisionScopeChange}
	if got := decisions.Replies(); !slices.Equal(got, want) {
		t.Fatalf("decisions.Replies() = %v, want the delivery decision kinds %v", got, want)
	}
}

// A reply whose decision contradicts the chosen option's reply is refused and leaves the record
// answered; the reply that matches it applies the record.
func TestReview818OppositeReplyIsRefused(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	event := review818SeedReply(t, state, "rel-903", "ev-blocked", "answer", 1)
	decision := review818RaiseBlocker(t, state)
	review818AnswerUser(t, state, decision, "hold")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-apply", "--decision", decision, "--event", event), "reply_contradicts_answer")
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "answered" || record["applied_generation"] != float64(0) {
		t.Fatalf("a contradicting reply moved the record: %v", record)
	}
	// The contrast: the reply that matches the chosen option's reply applies it.
	other := crw737Store(t)
	review818SeedSupervisor(t, other, "task-sup")
	matching := review818SeedReply(t, other, "rel-903", "ev-blocked", "stop", 1)
	otherDecision := review818RaiseBlocker(t, other)
	review818AnswerUser(t, other, otherDecision, "hold")
	if got := crw737Run(t, "--state", other, "decision-apply", "--decision", otherDecision, "--event", matching); got.code != 0 {
		t.Fatalf("a matching reply: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	if record := crw737List(t, other)[0].(map[string]any); record["state"] != "applied" {
		t.Fatalf("the applied record is %v", record)
	}
}

// An answer that gives both a choice and its explanation is refused, because the stored answer
// can only carry one of them and the choice is the one the apply reads.
func TestReview818OptionAndTextTogetherIsRefused(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	decision := review818Raise(t, state, "Which window does the host update take?")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision,
		"--option", "now", "--text", "because it is idle", "--by", "task-sup", "--via", "dots", "--authority", "user"), "bad_invocation")
	// A lone option still answers, so the refusal is about giving both.
	if got := crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now",
		"--by", "task-sup", "--via", "dots", "--authority", "user"); got.code != 0 {
		t.Fatalf("a lone option: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
}

// Giving both flags is what is refused, not giving both with text: an empty or blank --text is
// still --text on the line, and accepting it would store the option and let the answer stand
// without the prose the caller asked to record.
func TestReview818AnEmptyTextBesideAnOptionIsStillBoth(t *testing.T) {
	for _, text := range []string{"", "   "} {
		state := crw737Store(t)
		review818SeedSupervisor(t, state, "task-sup")
		decision := review818Raise(t, state, "Which window does the host update take?")
		crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision,
			"--option", "now", "--text", text, "--by", "task-sup", "--via", "dots", "--authority", "user"), "bad_invocation")
		// The refusal wrote nothing: the record is still raised and still answerable.
		if record := crw737List(t, state)[0].(map[string]any); record["state"] != "raised" {
			t.Fatalf("--text %q left the record %v", text, record["state"])
		}
	}
	// A lone blank --text with no option is refused as an answer that names nothing.
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	decision := review818Raise(t, state, "Which window does the host update take?")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision,
		"--text", "   ", "--by", "task-sup", "--via", "dots", "--authority", "user"), "bad_invocation")
}

// A relationship-blocking decision answered with free text alone is refused.
func TestReview818RelationshipDecisionNeedsAnOption(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	decision := review818RaiseBlocker(t, state)
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision,
		"--text", "hold it", "--by", "task-sup", "--via", "dots", "--authority", "user"), "bad_invocation")
}

// The reply a raise names is stored on the option it belongs to and read back by decision-list,
// and a --option-reply that names no option of the raise is refused.
func TestReview818TheOptionReplyIsStoredAndReadBack(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	review818RaiseBlocker(t, state)
	record := crw737List(t, state)[0].(map[string]any)
	options, _ := record["options"].([]any)
	if len(options) != 2 {
		t.Fatalf("the record carries %d options: %v", len(options), record)
	}
	replies := map[string]string{}
	for _, raw := range options {
		option, _ := raw.(map[string]any)
		id, _ := option["id"].(string)
		reply, _ := option["reply"].(string)
		replies[id] = reply
	}
	if replies["hold"] != "stop" || replies["merge"] != "answer" {
		t.Fatalf("the stored option replies are %v", replies)
	}
	// A reply that names no option of the raise is refused.
	review818RefusedFor(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "merge_approval",
		"--context", "Hold the other merge?", "--option", "hold=hold it:the merge waits", "--option", "merge=merge now:the merge runs",
		"--option-reply", "nosuch=stop", "--blocking", "relationship=rel-903", "--origin-project", "PRJ-A",
		"--source", "receipt=ev-blocked", "--authority", "user"), "bad_invocation", "names no option of this raise")
	// A raise with no reply at all on a relationship-blocking decision is refused.
	review818RefusedFor(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "merge_approval",
		"--context", "Hold the third merge?", "--option", "hold=hold it:the merge waits", "--option", "merge=merge now:the merge runs",
		"--blocking", "relationship=rel-903", "--origin-project", "PRJ-A",
		"--source", "receipt=ev-blocked", "--authority", "user"), "bad_invocation", "every option of a relationship-blocking decision names its reply")
}

// review818RefusedFor requires the refusal envelope to carry the reason and a detail naming the
// check that refused it: two refusals that share a reason are told apart by what they say, so a
// test cannot pass because an earlier parser rejected the line.
func review818RefusedFor(t *testing.T, got crw737Answer, reason, detail string) {
	t.Helper()
	value := crw737Refused(t, got, reason)
	if text, _ := value["detail"].(string); !strings.Contains(text, detail) {
		t.Fatalf("refusal detail %q does not name %q", text, detail)
	}
}

// A user-grade answer names its class and comes from the store-scope supervisor seat; with no
// binding at all it is refused rather than accepted.
func TestReview818UserAnswerNeedsTheSupervisor(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	decision := review818Raise(t, state, "Which window does the host update take?")
	// The class is not inherited: a user-grade answer names it.
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now",
		"--by", "task-sup", "--via", "dots"), "bad_invocation")
	// The recorder is the task the store-scope supervisor binding holds.
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now",
		"--by", "task-other", "--via", "dots", "--authority", "user"), "authority_unverified")
	// supervisor-readback reads the parent's report, which is not the user's answer.
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now",
		"--by", "task-sup", "--via", "supervisor-readback", "--authority", "user"), "bad_invocation")
	// The contrast: the supervisor answers its own grade.
	if got := crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now",
		"--by", "task-sup", "--via", "dots", "--authority", "user"); got.code != 0 {
		t.Fatalf("the supervisor's answer: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	// No binding at all: refused, fail closed.
	bare := crw737Store(t)
	other := review818Raise(t, bare, "Which window does the host update take?")
	crw737Refused(t, crw737Run(t, "--state", bare, "decision-answer", "--decision", other, "--option", "now",
		"--by", "task-sup", "--via", "dots", "--authority", "user"), "authority_unverified")
}

// A relationship-blocking question written before the reply field existed is still readable: the
// reader accepts it (Validate, not ValidateRaise), so it can be listed and withdrawn, while it can
// never be applied because its options name no reply for the reply event's decision to match.
func TestReview818ALegacyRelationshipDecisionIsStillReadable(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	decision := review818RaiseBlocker(t, state)
	// Strip the replies the raise stored, as a row written by the previous runtime has none.
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if _, err := opened.Querier(ctx).ExecContext(ctx,
		"UPDATE dag_user_decisions SET options_json = ? WHERE decision_id = ?",
		`[{"id":"hold","label":"hold","effect":"hold the merge"},{"id":"merge","label":"merge","effect":"merge now"}]`, decision); err != nil {
		t.Fatal(err)
	}
	// decision-list reads it, and it can be withdrawn.
	if records := crw737List(t, state); len(records) != 1 || records[0].(map[string]any)["state"] != "raised" {
		t.Fatalf("decision-list read back %v", records)
	}
	if got := crw737Run(t, "--state", state, "decision-withdraw", "--decision", decision, "--reason", "superseded"); got.code != 0 {
		t.Fatalf("decision-withdraw: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
}

// A parent-class question carries the ref its answer must cite, so a raise that names the class
// without one is refused rather than stored as a question whose parent answer cannot be checked.
func TestReview818AParentGradeQuestionNamesItsRef(t *testing.T) {
	state := crw737Store(t)
	crw737Refused(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "policy",
		"--context", "Which parent authority decides the window?", "--option", "now=now:at once",
		"--option", "later=later:deferred", "--origin-project", "PRJ-A", "--source", "report=1",
		"--authority", "parent"), "bad_invocation")
	// With its ref, the raise is accepted and an answer citing another ref is refused.
	raised := crw737JSON(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "policy",
		"--context", "Which parent authority decides the window?", "--option", "now=now:at once",
		"--option", "later=later:deferred", "--origin-project", "PRJ-A", "--source", "report=1",
		"--authority", "parent=plan-1"))
	decision, _ := raised["decisionId"].(string)
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision,
		"--option", "now", "--by", "task-a", "--via", "direct-ask", "--authority", "parent=plan-2"), "bad_invocation")
	if got := crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now",
		"--by", "task-a", "--via", "direct-ask", "--authority", "parent=plan-1"); got.code != 0 {
		t.Fatalf("an answer citing the question's own ref: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "answered" {
		t.Fatalf("the answered record is %v", record)
	}
}

// The generation the record carries is the reply event's own execution generation.
func TestReview818AppliedGenerationIsRead(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	event := review818SeedReply(t, state, "rel-903", "ev-blocked", "stop", 2)
	decision := review818RaiseBlocker(t, state)
	review818AnswerUser(t, state, decision, "hold")
	if got := crw737Run(t, "--state", state, "decision-apply", "--decision", decision, "--event", event); got.code != 0 {
		t.Fatalf("decision-apply: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	record := crw737List(t, state)[0].(map[string]any)
	if record["state"] != "applied" || record["applied_generation"] != float64(2) {
		t.Fatalf("the applied record is %v", record)
	}
}

// A stored record whose raised_at an older build could write is preserved by the reader and by
// Update; re-raising the same question must append the observation rather than fail on the stored
// value, so a question can still move from open to raised.
func TestReview818ReraiseKeepsAStoredBadRaisedAt(t *testing.T) {
	state := crw737Store(t)
	first := crw737JSON(t, crw737Raise(t, state, "PRJ-A", "Which window does the host update take?"))
	decision, _ := first["decisionId"].(string)
	// A question that has not been shown yet is open; the re-raise is what moves it to raised.
	review818SetState(t, state, decision, "open")
	review818SetRaisedAt(t, state, decision, "not a time")
	second := crw737JSON(t, crw737Raise(t, state, "PRJ-A", "Which window does the host update take?"))
	if second["merged"] != true || second["decisionId"] != decision {
		t.Fatalf("the re-raise answered %v, want the stored record %v", second, first)
	}
	record := crw737List(t, state)[0].(map[string]any)
	if record["state"] != "raised" {
		t.Fatalf("the re-raise left the record %v, want the open question raised", record["state"])
	}
	if record["raised_at"] != "not a time" {
		t.Fatalf("the re-raise rewrote raised_at to %v", record["raised_at"])
	}
	if seen, _ := record["seen"].([]any); len(seen) != 2 {
		t.Fatalf("the re-raise left %d observations, want the appended second one", len(seen))
	}
}

// review818SetState writes state straight into the record, so a case can seed the state a question
// has before it is shown.
func review818SetState(t *testing.T, state, decisionID, value string) {
	t.Helper()
	review818Exec(t, state, "UPDATE dag_user_decisions SET state = ? WHERE decision_id = ?", value, decisionID)
}

// review818StripOptionReplies rewrites a stored record's options without their replies: the shape
// a row written before the reply field existed has.
func review818StripOptionReplies(t *testing.T, state, decisionID string) {
	t.Helper()
	review818Exec(t, state, "UPDATE dag_user_decisions SET options_json = ? WHERE decision_id = ?",
		`[{"id":"hold","label":"hold","effect":"hold the merge"},{"id":"merge","label":"merge","effect":"merge now"}]`, decisionID)
}

// review818SetOptionJSON rewrites a stored record's options wholesale, so a case can seed the exact
// set a row holds.
func review818SetOptionJSON(t *testing.T, state, decisionID, options string) {
	t.Helper()
	review818Exec(t, state, "UPDATE dag_user_decisions SET options_json = ? WHERE decision_id = ?", options, decisionID)
}

// Two option ids the fingerprint calls one identity (HOLD and hold, which Validate admits because
// the trimmed ids differ) each keep their own reply, and an identical repeat raise of that question
// folds: the replies are compared option by option, so the question can still take its second
// observation and move from open to raised.
func TestReview818AnIdenticalReraiseWithOneIdentityFolds(t *testing.T) {
	state := crw737Store(t)
	const question = "Hold the merge until the retention decision?"
	raise := func() crw737Answer {
		return crw737Run(t, "--state", state, "decision-raise", "--kind", "merge_approval",
			"--context", question, "--option", "HOLD=Hold:wait", "--option", "hold=Continue:resume",
			"--option-reply", "HOLD=stop", "--option-reply", "hold=answer",
			"--blocking", "relationship=rel-903", "--origin-project", "PRJ-A", "--source", "report=1",
			"--authority", "user")
	}
	first := crw737JSON(t, raise())
	decision, _ := first["decisionId"].(string)
	if decision == "" {
		t.Fatalf("the first raise answered %v", first)
	}
	second := crw737JSON(t, raise())
	if second["merged"] != true || second["decisionId"] != decision {
		t.Fatalf("the identical re-raise answered %v, want the stored record %s merged", second, decision)
	}
	record := crw737List(t, state)[0].(map[string]any)
	if record["state"] != "raised" {
		t.Fatalf("the re-raise left the record %v, want it raised", record["state"])
	}
	if seen, _ := record["seen"].([]any); len(seen) != 2 {
		t.Fatalf("the re-raise left %d observations, want the appended second one", len(seen))
	}
	if replies := review818OptionReplies(t, record); replies["HOLD"] != "stop" || replies["hold"] != "answer" {
		t.Fatalf("the re-raise changed the stored replies: %v", replies)
	}
}

// A question whose options carry no reply has nothing for the fold to reconcile, so a repeat raise
// of it must still fold and append its observation however the ids are spelled. An option id the
// fingerprint calls one identity is admitted by the raise path (Validate refuses only an exact
// duplicate), and refusing the fold for it would leave a record that can take no second
// observation at all.
func TestReview818ARepeatRaiseWithoutRepliesStillFolds(t *testing.T) {
	state := crw737Store(t)
	const question = "Which window does the host update take?"
	first := crw737JSON(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "policy",
		"--context", question, "--option", "A=Alpha:one", "--option", "a=alpha:two",
		"--origin-project", "PRJ-A", "--source", "report=1", "--authority", "user"))
	decision, _ := first["decisionId"].(string)
	if decision == "" {
		t.Fatalf("the first raise answered %v", first)
	}
	second := crw737JSON(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "policy",
		"--context", question, "--option", "A=Alpha:one", "--option", "a=alpha:two",
		"--origin-project", "PRJ-A", "--source", "report=1", "--authority", "user"))
	if second["merged"] != true || second["decisionId"] != decision {
		t.Fatalf("the repeat raise answered %v, want the stored record %s merged", second, decision)
	}
	if seen, _ := crw737List(t, state)[0].(map[string]any)["seen"].([]any); len(seen) != 2 {
		t.Fatalf("the repeat raise left %d observations, want the appended second one", len(seen))
	}
}

// A question's identity is its fingerprint, which normalizes an option id (ASCII-lowercased, its
// whitespace runs collapsed). Two raises that share a fingerprint are one question whatever an id's
// case or spacing, so the fold matches option ids the same way the fingerprint does: a stored id
// written as HOLD is the option the incoming hold names. Matching on the raw text would skip the
// fill silently, and a stored reply would never be compared against the incoming one.
func TestReview818TheFoldMatchesOptionIDsAsTheFingerprintDoes(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	event := review818SeedReply(t, state, "rel-903", "ev-blocked", "stop", 1)
	decision := review818RaiseBlocker(t, state)
	// A row an older build wrote: its option ids differ in case and carry no reply.
	review818SetOptionJSON(t, state, decision,
		`[{"id":"HOLD","label":"hold","effect":"hold the merge"},{"id":"MERGE","label":"merge","effect":"merge now"}]`)
	merged := crw737JSON(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "merge_approval",
		"--context", "Hold the merge until the retention decision?", "--option", "hold=hold:hold the merge",
		"--option", "merge=merge:merge now", "--option-reply", "hold=stop", "--option-reply", "merge=answer",
		"--blocking", "relationship=rel-903", "--origin-project", "PRJ-A", "--source", "event=ev-blocked",
		"--authority", "user"))
	if merged["merged"] != true || merged["decisionId"] != decision {
		t.Fatalf("the re-raise answered %v, want the stored record %s merged", merged, decision)
	}
	record := crw737List(t, state)[0].(map[string]any)
	if replies := review818OptionReplies(t, record); replies["HOLD"] != "stop" || replies["MERGE"] != "answer" {
		t.Fatalf("the re-raise left the replies %v, want the named ones filled in", replies)
	}
	// The filled reply is the one the apply matches, so the record can be applied.
	if got := crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "HOLD",
		"--by", "task-sup", "--via", "dots", "--authority", "user"); got.code != 0 {
		t.Fatalf("decision-answer: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	if got := crw737Run(t, "--state", state, "decision-apply", "--decision", decision, "--event", event); got.code != 0 {
		t.Fatalf("decision-apply: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "applied" {
		t.Fatalf("the applied record is %v", record)
	}
}

// The same identity rule is what makes a conflict visible: a stored HOLD=stop and an incoming
// hold=answer are one option, so the fold refuses rather than dropping the second raise's reply.
func TestReview818TheFoldRefusesAConflictAcrossAnIDSpelling(t *testing.T) {
	state := crw737Store(t)
	decision := review818RaiseBlocker(t, state)
	review818SetOptionJSON(t, state, decision,
		`[{"id":"HOLD","label":"hold","effect":"hold the merge","reply":"stop"},{"id":"MERGE","label":"merge","effect":"merge now","reply":"answer"}]`)
	crw737Refused(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "merge_approval",
		"--context", "Hold the merge until the retention decision?", "--option", "hold=hold:hold the merge",
		"--option", "merge=merge:merge now", "--option-reply", "hold=answer", "--option-reply", "merge=answer",
		"--blocking", "relationship=rel-903", "--origin-project", "PRJ-A", "--source", "event=ev-blocked",
		"--authority", "user"), "disposition_conflict")
	record := crw737List(t, state)[0].(map[string]any)
	if replies := review818OptionReplies(t, record); replies["HOLD"] != "stop" {
		t.Fatalf("the refused re-raise rewrote the stored replies: %v", replies)
	}
}

// A second raise that names another reply for an option the stored record already answers with one
// is refused: the reply is what the apply matches, so folding it silently would apply the record
// against a mapping the later raiser never offered. The fingerprint excludes the reply, so refusing
// the fold leaves the question's identity contract as it is, and nothing is written.
func TestReview818ConflictingOptionRepliesAreRefused(t *testing.T) {
	state := crw737Store(t)
	decision := review818RaiseBlocker(t, state)
	// The same question, the same options, another reply for the hold option.
	crw737Refused(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "merge_approval",
		"--context", "Hold the merge until the retention decision?", "--option", "hold=hold:hold the merge",
		"--option", "merge=merge:merge now", "--option-reply", "hold=answer", "--option-reply", "merge=answer",
		"--blocking", "relationship=rel-903", "--origin-project", "PRJ-A", "--source", "event=ev-blocked",
		"--authority", "user"), "disposition_conflict")
	// Nothing was written: one observation, and the stored replies are the first raise's.
	record := crw737List(t, state)[0].(map[string]any)
	if seen, _ := record["seen"].([]any); len(seen) != 1 {
		t.Fatalf("the refused re-raise left %d observations, want the stored one", len(seen))
	}
	if replies := review818OptionReplies(t, record); replies["hold"] != "stop" || replies["merge"] != "answer" {
		t.Fatalf("the refused re-raise rewrote the stored replies: %v", replies)
	}
	// The contrast: the same replies re-raised fold as before.
	same := crw737JSON(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "merge_approval",
		"--context", "Hold the merge until the retention decision?", "--option", "hold=hold:hold the merge",
		"--option", "merge=merge:merge now", "--option-reply", "hold=stop", "--option-reply", "merge=answer",
		"--blocking", "relationship=rel-903", "--origin-project", "PRJ-A", "--source", "event=ev-blocked",
		"--authority", "user"))
	if same["merged"] != true || same["decisionId"] != decision {
		t.Fatalf("the same replies answered %v, want the stored record %s merged", same, decision)
	}
	if seen, _ := crw737List(t, state)[0].(map[string]any)["seen"].([]any); len(seen) != 2 {
		t.Fatalf("the identical re-raise left %d observations, want the appended second one", len(seen))
	}
}

// A relationship-blocking record written before the reply field existed has no replies; a re-raise
// that names them fills them in, and the record then applies with the reply the option makes.
func TestReview818ReraiseFillsTheRepliesOfALegacyRecord(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	event := review818SeedReply(t, state, "rel-903", "ev-blocked", "stop", 1)
	decision := review818RaiseBlocker(t, state)
	review818StripOptionReplies(t, state, decision)
	// The raise names the replies the legacy row lacks, so the merge fills them in.
	merged := crw737JSON(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "merge_approval",
		"--context", "Hold the merge until the retention decision?", "--option", "hold=hold:hold the merge",
		"--option", "merge=merge:merge now", "--option-reply", "hold=stop", "--option-reply", "merge=answer",
		"--blocking", "relationship=rel-903", "--origin-project", "PRJ-A", "--source", "event=ev-blocked",
		"--authority", "user"))
	if merged["merged"] != true || merged["decisionId"] != decision {
		t.Fatalf("the re-raise answered %v, want the stored record %s merged", merged, decision)
	}
	record := crw737List(t, state)[0].(map[string]any)
	if replies := review818OptionReplies(t, record); replies["hold"] != "stop" || replies["merge"] != "answer" {
		t.Fatalf("the re-raise left the replies %v, want the named ones filled in", replies)
	}
	// The filled reply is the one the apply matches.
	review818AnswerUser(t, state, decision, "hold")
	if got := crw737Run(t, "--state", state, "decision-apply", "--decision", decision, "--event", event); got.code != 0 {
		t.Fatalf("decision-apply: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "applied" {
		t.Fatalf("the applied record is %v", record)
	}
}

// review818OptionReplies is a listed record's option replies by option id.
func review818OptionReplies(t *testing.T, record map[string]any) map[string]string {
	t.Helper()
	replies := map[string]string{}
	options, _ := record["options"].([]any)
	for _, raw := range options {
		option, _ := raw.(map[string]any)
		id, _ := option["id"].(string)
		reply, _ := option["reply"].(string)
		replies[id] = reply
	}
	return replies
}

// A decision is applied only once the relay has recorded the reply's delivery as accepted by the
// child: a reply still queued leaves the record answered.
func TestReview818ApplyWaitsForTheDelivery(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	event := review818SeedReply(t, state, "rel-903", "ev-blocked", "stop", 1)
	review818SetDeliveryState(t, state, event, delivery.Queued)
	decision := review818RaiseBlocker(t, state)
	review818AnswerUser(t, state, decision, "hold")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-apply", "--decision", decision, "--event", event), "reply_not_delivered")
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "answered" {
		t.Fatalf("an undelivered reply moved the record: %v", record)
	}
	// The relay records the delivery as accepted by the child: the same reply applies it.
	review818SetDeliveryState(t, state, event, delivery.Dispatched)
	if got := crw737Run(t, "--state", state, "decision-apply", "--decision", decision, "--event", event); got.code != 0 {
		t.Fatalf("decision-apply: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "applied" {
		t.Fatalf("the applied record is %v", record)
	}
}

// A question raised from a report names no receipt, so the receipt identity check does not apply:
// the reply is the relay's own decision_reply on the blocked relationship, recorded at or after the
// record's answered_at. A reply recorded before the answer cannot be the reply that answers it.
func TestReview818ReportSourceApplies(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	event := review818SeedReply(t, state, "rel-903", "ev-report", "stop", 1)
	raised := crw737JSON(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "merge_approval",
		"--context", "Hold the merge the report asks about?", "--option", "hold=hold:hold the merge",
		"--option", "merge=merge:merge now", "--option-reply", "hold=stop", "--option-reply", "merge=answer",
		"--blocking", "relationship=rel-903", "--origin-project", "PRJ-A", "--source", "report=1",
		"--authority", "user"))
	decision, _ := raised["decisionId"].(string)
	review818AnswerUser(t, state, decision, "hold")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-apply", "--decision", decision, "--event", event), "disposition_conflict")
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "answered" {
		t.Fatalf("a reply recorded before the answer applied the record: %v", record)
	}
	// Recorded at the answer, the same reply applies it.
	answeredAt, _ := crw737List(t, state)[0].(map[string]any)["answered_at"].(string)
	if answeredAt == "" {
		t.Fatal("the answered record carries no answered_at")
	}
	review818SetFirstSeenAt(t, state, event, answeredAt)
	if got := crw737Run(t, "--state", state, "decision-apply", "--decision", decision, "--event", event); got.code != 0 {
		t.Fatalf("decision-apply: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "applied" {
		t.Fatalf("the applied record is %v", record)
	}
}
