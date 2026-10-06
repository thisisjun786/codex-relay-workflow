package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-737: the relay user-decision commands. decision-raise records a question, decision-list
// reads the records back, decision-answer records an answer with its provenance, decision-apply
// marks it applied once the decision_reply event of a relationship the decision blocks has been
// observed, and decision-withdraw retracts it.

// crw737Answer is one run of the relay CLI in process, as codex-session-relay.
type crw737Answer struct {
	code           int
	stdout, stderr string
}

func crw737Run(t *testing.T, argv ...string) crw737Answer {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.ExecuteAs(context.Background(), "codex-session-relay", argv, &stdout, &stderr)
	return crw737Answer{code, stdout.String(), stderr.String()}
}

// crw737Store is a state directory holding a store this runtime owns and may write. It is created
// by the frozen v1 store alone (testsupport.Create), so it carries no DAG zone until a write open
// installs one - which is what the C9 case needs.
func crw737Store(t *testing.T) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	testsupport.Create(t, filepath.Join(state, "relay.sqlite3"), "", "go")
	return state
}

// crw737JSON decodes a command's answer, failing the test when it is not one JSON document.
func crw737JSON(t *testing.T, got crw737Answer) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &value); err != nil {
		t.Fatalf("the answer is not one JSON object: %v\nstdout %s\nstderr %s", err, got.stdout, got.stderr)
	}
	return value
}

// crw737Refused requires the answer to be the relay's refusal envelope (exit 2), with the reason
// the caller expects and a non-empty detail.
func crw737Refused(t *testing.T, got crw737Answer, reason string) map[string]any {
	t.Helper()
	if got.code != 2 {
		t.Fatalf("exit %d, want 2\nstdout %s\nstderr %s", got.code, got.stdout, got.stderr)
	}
	value := crw737JSON(t, got)
	if value["ok"] != false || value["reason"] != reason {
		t.Fatalf("refusal %v, want ok false and reason %q", value, reason)
	}
	if detail, _ := value["detail"].(string); detail == "" {
		t.Fatalf("refusal %v carries no detail", value)
	}
	return value
}

// crw737Raise is one decision-raise line, with the question the case names.
func crw737Raise(t *testing.T, state, project, question string, extra ...string) crw737Answer {
	t.Helper()
	argv := append([]string{"--state", state, "decision-raise",
		"--kind", "policy", "--context", question,
		"--option", "now=now:the window opens at once", "--option", "later=later:the chain stays blocked",
		"--origin-project", project, "--source", "report=1", "--authority", "user"}, extra...)
	return crw737Run(t, argv...)
}

// crw737List runs decision-list and returns the records array.
func crw737List(t *testing.T, state string, extra ...string) []any {
	t.Helper()
	got := crw737Run(t, append([]string{"--state", state, "decision-list"}, extra...)...)
	if got.code != 0 {
		t.Fatalf("decision-list exit %d: %s %s", got.code, got.stdout, got.stderr)
	}
	var records []any
	if err := json.Unmarshal([]byte(got.stdout), &records); err != nil {
		t.Fatalf("decision-list did not answer an array: %v\n%s", err, got.stdout)
	}
	return records
}

// C1: the five commands are registered and read by a declared parser; before they existed every
// one of these lines was refused with "invalid choice".
func TestCRW737C1TheDecisionCommandsExist(t *testing.T) {
	for _, name := range []string{"decision-raise", "decision-list", "decision-answer", "decision-apply", "decision-withdraw"} {
		if _, ok := dispatch.Lookup(name); !ok {
			t.Errorf("%s is not a registered relay command", name)
		}
		if _, ok := argparse.Specs[name]; !ok {
			t.Errorf("%s has no argparse spec", name)
		}
		got := crw737Run(t, name, "--help")
		if got.code != 0 || got.stderr != "" || !bytes.HasPrefix([]byte(got.stdout), []byte("usage: codex-session-relay "+name)) {
			t.Errorf("%s --help: exit %d stdout %q stderr %q", name, got.code, got.stdout, got.stderr)
		}
	}
}

// C2: one raise makes one record; the same question raised again folds into it (merged true) and
// appends the second observation instead of adding a row.
func TestCRW737C2ARaiseMakesOneRecordAndTheSecondMerges(t *testing.T) {
	state := crw737Store(t)
	first := crw737JSON(t, crw737Raise(t, state, "PRJ-A", "Which window does the host update take?"))
	if first["merged"] != false || first["state"] != "raised" {
		t.Fatalf("first raise %v", first)
	}
	if id, _ := first["decisionId"].(string); id == "" {
		t.Fatalf("first raise named no decision id: %v", first)
	}
	if fingerprint, _ := first["fingerprint"].(string); fingerprint == "" {
		t.Fatalf("first raise named no fingerprint: %v", first)
	}

	second := crw737JSON(t, crw737Raise(t, state, "PRJ-A", "which   WINDOW does the host update take?"))
	if second["merged"] != true {
		t.Fatalf("the second raise of one question did not merge: %v", second)
	}
	if second["decisionId"] != first["decisionId"] {
		t.Fatalf("the second raise answered %v, want the stored %v", second["decisionId"], first["decisionId"])
	}
	records := crw737List(t, state)
	if len(records) != 1 {
		t.Fatalf("decision-list returned %d records, want 1", len(records))
	}
	record := records[0].(map[string]any)
	seen, _ := record["seen"].([]any)
	if len(seen) != 2 {
		t.Fatalf("the folded record holds %d observations, want 2: %v", len(seen), record)
	}
}

// C3: an unknown kind, an empty context and an option set of one or four are refused.
func TestCRW737C3RaiseRefusesWhatItCannotRecord(t *testing.T) {
	state := crw737Store(t)
	crw737Refused(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "nonsense", "--context", "c",
		"--option", "a=b:c", "--option", "d=e:f", "--origin-project", "PRJ-A", "--source", "report=1", "--authority", "user"), "bad_invocation")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "policy", "--context", "   ",
		"--option", "a=b:c", "--option", "d=e:f", "--origin-project", "PRJ-A", "--source", "report=1", "--authority", "user"), "bad_invocation")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "policy", "--context", "c",
		"--option", "a=b:c", "--origin-project", "PRJ-A", "--source", "report=1", "--authority", "user"), "bad_invocation")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "policy", "--context", "c",
		"--option", "a=b:c", "--option", "d=e:f", "--option", "g=h:i", "--option", "j=k:l",
		"--origin-project", "PRJ-A", "--source", "report=1", "--authority", "user"), "bad_invocation")
	if records := crw737List(t, state); len(records) != 0 {
		t.Fatalf("a refused raise wrote %d records", len(records))
	}
}

// C4: decision-list returns the record the raise made, and the filters select it.
func TestCRW737C4DecisionListReturnsTheRaisedRecord(t *testing.T) {
	state := crw737Store(t)
	raised := crw737JSON(t, crw737Raise(t, state, "PRJ-A", "Which window does the host update take?"))
	records := crw737List(t, state)
	if len(records) != 1 {
		t.Fatalf("decision-list returned %d records, want 1", len(records))
	}
	record := records[0].(map[string]any)
	if record["decision_id"] != raised["decisionId"] || record["fingerprint"] != raised["fingerprint"] ||
		record["state"] != "raised" || record["schema"] != "crw-user-decision/1" {
		t.Fatalf("decision-list answered %v, want the raised record %v", record, raised)
	}
	if origin, _ := record["origin"].(map[string]any); origin["project"] != "PRJ-A" {
		t.Fatalf("the record's origin is %v", record["origin"])
	}
	if len(crw737List(t, state, "--state", "raised")) != 1 || len(crw737List(t, state, "--state", "applied")) != 0 {
		t.Fatal("the state filter does not select the record")
	}
	if len(crw737List(t, state, "--project", "PRJ-A")) != 1 || len(crw737List(t, state, "--project", "PRJ-B")) != 0 {
		t.Fatal("the project filter does not select the record")
	}
}

// C5: the same question raised from two projects is one folded record, and each project's
// decision-list --project returns it.
func TestCRW737C5AFoldedRecordIsVisibleFromBothProjects(t *testing.T) {
	state := crw737Store(t)
	const question = "Which window does the host update take?"
	first := crw737JSON(t, crw737Raise(t, state, "PRJ-A", question))
	second := crw737JSON(t, crw737Raise(t, state, "PRJ-B", question))
	if second["merged"] != true || second["decisionId"] != first["decisionId"] {
		t.Fatalf("the second project's raise answered %v, want the folded record %v", second, first)
	}
	if records := crw737List(t, state); len(records) != 1 {
		t.Fatalf("the two raises made %d records, want 1", len(records))
	}
	for _, project := range []string{"PRJ-A", "PRJ-B"} {
		records := crw737List(t, state, "--project", project)
		if len(records) != 1 {
			t.Fatalf("decision-list --project %s returned %d records, want the folded one", project, len(records))
		}
	}
}

// C6: an answer without --by or --via, one that cites a class above the decision's, and one after
// the decision was withdrawn are refused.
func TestCRW737C6AnswerRefusals(t *testing.T) {
	state := crw737Store(t)
	raised := crw737JSON(t, crw737Raise(t, state, "PRJ-A", "Which window does the host update take?"))
	decision, _ := raised["decisionId"].(string)

	if got := crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now", "--via", "direct-ask"); got.code != 2 {
		t.Fatalf("an answer without --by: exit %d %s", got.code, got.stdout)
	}
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now",
		"--by", "task-a", "--via", "   "), "bad_invocation")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now",
		"--by", "task-a", "--via", "carrier-pigeon"), "bad_invocation")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "nosuch",
		"--by", "task-a", "--via", "direct-ask"), "bad_invocation")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision,
		"--by", "task-a", "--via", "direct-ask"), "bad_invocation")
	// A user-grade question answered by a weaker class is an answer below the grade.
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision,
		"--option", "now", "--by", "task-a", "--via", "direct-ask", "--authority", "delegated-management"), "bad_invocation")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision,
		"--option", "now", "--by", "task-a", "--via", "direct-ask", "--authority", "parent=plan-1"), "bad_invocation")
	// A parent-class answer without its reference is refused.
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision,
		"--option", "now", "--by", "task-a", "--via", "direct-ask", "--authority", "parent"), "bad_invocation")

	if got := crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now",
		"--by", "task-a", "--via", "direct-ask"); got.code != 0 {
		t.Fatalf("a well-formed answer: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	if got := crw737Run(t, "--state", state, "decision-withdraw", "--decision", decision, "--reason", "the plan moved on"); got.code != 0 {
		t.Fatalf("decision-withdraw: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "later",
		"--by", "task-a", "--via", "direct-ask"), "disposition_conflict")
}

// C8: the answer's provenance is stored on the record.
func TestCRW737C8TheAnswerProvenanceIsStored(t *testing.T) {
	state := crw737Store(t)
	raised := crw737JSON(t, crw737Raise(t, state, "PRJ-A", "Which window does the host update take?"))
	decision, _ := raised["decisionId"].(string)
	if got := crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--text", "between runs",
		"--by", "task-a", "--via", "management-message"); got.code != 0 {
		t.Fatalf("decision-answer: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	records := crw737List(t, state)
	if len(records) != 1 {
		t.Fatalf("decision-list returned %d records", len(records))
	}
	record := records[0].(map[string]any)
	if record["state"] != "answered" || record["answered_by"] != "task-a" ||
		record["answered_via"] != "management-message" || record["answer_text"] != "between runs" {
		t.Fatalf("the answer's provenance is not on the record: %v", record)
	}
	if at, _ := record["answered_at"].(string); at == "" {
		t.Fatalf("the answer carries no answered_at: %v", record)
	}
}

// C7: applied happens only when the decision_reply event of a relationship the decision blocks is
// recorded, and never on the answer alone.
func TestCRW737C7ApplyNeedsTheDecisionReplyEvent(t *testing.T) {
	state := crw737Store(t)
	crw737SeedRelationship(t, state)
	raised := crw737JSON(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "merge_approval",
		"--context", "Hold the merge until the retention decision?", "--option", "hold=h:hold the merge",
		"--option", "merge=m:merge now", "--blocking", "relationship=rel-737",
		"--origin-project", "PRJ-A", "--source", "receipt=ev-blocked", "--authority", "user"))
	decision, _ := raised["decisionId"].(string)
	if got := crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "hold",
		"--by", "task-a", "--via", "direct-ask"); got.code != 0 {
		t.Fatalf("decision-answer: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	// The answer alone does not apply it.
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "answered" || record["applied_at"] != nil {
		t.Fatalf("the answer alone applied the decision: %v", record)
	}
	// An event that is not the decision_reply of the blocked relationship does not apply it.
	crw737Refused(t, crw737Run(t, "--state", state, "decision-apply", "--decision", decision, "--event", "ev-other"), "disposition_conflict")
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "answered" {
		t.Fatalf("a refused apply moved the record: %v", record)
	}
	// A row another producer wrote, even with the right id, does not apply it either.
	crw737SeedChildReply(t, state)
	crw737Refused(t, crw737Run(t, "--state", state, "decision-apply", "--decision", decision, "--event", "ev-child-reply"), "disposition_conflict")
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "answered" {
		t.Fatalf("a child-written row applied the record: %v", record)
	}
	// A reply that answers another receipt of the same relationship does not apply it, even though
	// its id is internally consistent: the decision names the receipt it was raised on.
	crw737SeedOtherReceiptReply(t, state)
	crw737Refused(t, crw737Run(t, "--state", state, "decision-apply", "--decision", decision, "--event", "ev-other-reply"), "disposition_conflict")
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "answered" {
		t.Fatalf("an older reply applied the record: %v", record)
	}
	// The decision_reply event of the blocked relationship does: the relay's own id for the reply
	// to the receipt the question answered is delivery.DecisionEventID(relationship, answersEvent).
	reply := delivery.DecisionEventID("rel-737", "ev-blocked")
	if got := crw737Run(t, "--state", state, "decision-apply", "--decision", decision, "--event", reply); got.code != 0 {
		t.Fatalf("decision-apply: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	record := crw737List(t, state)[0].(map[string]any)
	if record["state"] != "applied" || record["applied_event"] != reply {
		t.Fatalf("the applied record is %v", record)
	}
	if at, _ := record["applied_at"].(string); at == "" {
		t.Fatalf("the applied record carries no applied_at: %v", record)
	}
}

// C9: on a store that carries no dag_user_decisions table, decision-list answers an empty array
// instead of failing.
func TestCRW737C9DecisionListOnAStoreWithoutTheTable(t *testing.T) {
	state := crw737Store(t)
	records := crw737List(t, state)
	if len(records) != 0 {
		t.Fatalf("decision-list returned %d records on a store with no table", len(records))
	}
	if got := crw737Run(t, "--state", state, "decision-list", "--project", "PRJ-A"); got.code != 0 || got.stdout != "[]\n" {
		t.Fatalf("decision-list --project on a store with no table: exit %d %q %q", got.code, got.stdout, got.stderr)
	}
}

// crw737SeedRelationship writes the relationship, its generation and the decision_reply event the
// apply case observes, straight into the store (the fake rows of C7).
func crw737SeedRelationship(t *testing.T, state string) {
	t.Helper()
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	receipt := "{\"eventId\":\"ev-reply\",\"relationshipId\":\"rel-737\",\"executionGeneration\":1,\"kind\":\"decision_reply\"," +
		"\"decision\":\"answer\",\"answersEvent\":\"ev-blocked\",\"note\":\"hold it\",\"generationEffect\":\"stays\"," +
		"\"anchorTurnId\":\"turn-1\",\"childTaskId\":\"child\",\"decidedAt\":\"2026-10-06T00:00:00Z\"}"
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
			[]any{"rel-737", "CRW-737", "active", "parent", "host", "child", "host", int64(1), "[]", "[\"child\"]", "2026-10-06T00:00:00Z", "2026-10-06T00:00:00Z"}},
		{"INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, opened_at) VALUES (?,?,?,?,?,?)",
			[]any{"rel-737", int64(1), "dispatch-1", "bound", "turn-1", "2026-10-06T00:00:00Z"}},
		{"INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
			[]any{delivery.DecisionEventID("rel-737", "ev-blocked"), "rel-737", int64(1), "no_deliverable", "decision_reply", "relay", "thread", "turn-1", "completed", receipt, "final", "2026-10-06T00:00:00Z", "2026-10-06T00:00:00Z"}},
	} {
		if _, err := opened.Querier(ctx).ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.query, err)
		}
	}
}

// crw737SeedChildReply writes an events row that carries the decision_reply outcome but a child
// producer, so the apply case can show a producer check is what keeps it from applying a decision.
func crw737SeedChildReply(t *testing.T, state string) {
	t.Helper()
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if _, err := opened.Querier(ctx).ExecContext(ctx, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"ev-child-reply", "rel-737", int64(1), "no_deliverable", "decision_reply", "child", "thread", "turn-2", "inProgress", `{}`, "final", "2026-10-06T00:00:00Z", "2026-10-06T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
}

// crw737SeedOtherReceiptReply writes the relay's own decision_reply for another receipt of the
// same relationship: its id hashes its own answersEvent, so only the decision's own source ref
// keeps it from applying the record.
func crw737SeedOtherReceiptReply(t *testing.T, state string) {
	t.Helper()
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	event := delivery.DecisionEventID("rel-737", "ev-unrelated")
	receipt := `{"eventId":"` + event + `","relationshipId":"rel-737","executionGeneration":1,"kind":"decision_reply","decision":"answer","answersEvent":"ev-unrelated","note":"n","generationEffect":"stays","anchorTurnId":"turn-1","childTaskId":"child","decidedAt":"2026-10-06T00:00:00Z"}`
	if _, err := opened.Querier(ctx).ExecContext(ctx, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
		event, "rel-737", int64(1), "no_deliverable", "decision_reply", "relay", "thread", "turn-3", "completed", receipt, "final", "2026-10-06T00:00:00Z", "2026-10-06T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
}
