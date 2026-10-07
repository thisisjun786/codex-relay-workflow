package cli_test

import (
	"context"
	"path/filepath"
	"slices"
	"strconv"
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
		"--blocking", "relationship=rel-903", "--origin-project", "PRJ-A", "--source", "receipt=ev-blocked",
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
	crw737Refused(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "merge_approval",
		"--context", "Hold the other merge?", "--option", "hold=hold it", "--option", "merge=merge now",
		"--option-reply", "nosuch=stop", "--blocking", "relationship=rel-903", "--origin-project", "PRJ-A",
		"--source", "receipt=ev-blocked", "--authority", "user"), "bad_invocation")
	// A raise with no reply at all on a relationship-blocking decision is refused.
	crw737Refused(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "merge_approval",
		"--context", "Hold the third merge?", "--option", "hold=hold it", "--option", "merge=merge now",
		"--blocking", "relationship=rel-903", "--origin-project", "PRJ-A",
		"--source", "receipt=ev-blocked", "--authority", "user"), "bad_invocation")
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
