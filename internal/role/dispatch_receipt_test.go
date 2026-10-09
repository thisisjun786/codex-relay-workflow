package role

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// dispatchReceiptRow is one thread of a native database with the columns the host writes for a spawned child.
type dispatchReceiptRow struct {
	id, parent, first, model, effort string
}

// dispatchReceiptNative seeds a native database holding rows and returns the environment that reads it.
func dispatchReceiptNative(t *testing.T, env host.LookupEnv, rows ...dispatchReceiptRow) host.LookupEnv {
	t.Helper()
	native := t.TempDir()
	db := must(sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(native, "state_5.sqlite")}).String()))
	defer db.Close()
	_, err := db.Exec("CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT, source TEXT, archived INTEGER, first_user_message TEXT, model TEXT, reasoning_effort TEXT)")
	check(t, err)
	for _, r := range rows {
		source := string(must(json.Marshal(map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": r.parent, "depth": 1}}})))
		_, err = db.Exec("INSERT INTO threads VALUES (?,?,?,0,?,?,?)", r.id, "", source, r.first, r.model, r.effort)
		check(t, err)
	}
	return func(k string) (string, bool) {
		if k == "CODEX_HOME" {
			return native, true
		}
		return env(k)
	}
}

// dispatchReceiptClaim starts and claims a dispatch of role and returns its attempt and the marker the claim handed out.
func dispatchReceiptClaim(t *testing.T, ws string, env host.LookupEnv, role RoleName, dispatch string) (attempt, marker string) {
	t.Helper()
	start := dispatchTestCall(t, ws, env, map[string]any{"action": "start", "role": string(role), "dispatchId": dispatch})
	claim := dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "dispatchId": dispatch, "attemptId": start.AttemptID})
	return start.AttemptID, claim.Marker
}

// dispatchReceiptIssue is the spawn hook's issuance of the claimed attempt for the native call tool.
func dispatchReceiptIssue(t *testing.T, ws, marker, tool string) {
	t.Helper()
	if _, err := IssueManagedSpawn(ws, "session-test", marker+"\nTASK", &tool); err != nil {
		t.Fatal(err)
	}
}

// dispatchReceiptIssueSeeing is the issuance the hook makes with the host's thread database readable through env, which
// records the marked children the host already shows.
func dispatchReceiptIssueSeeing(t *testing.T, ws string, env host.LookupEnv, marker, tool string) {
	t.Helper()
	if _, err := IssueManagedSpawnEnv(ws, "session-test", marker+"\nTASK", &tool, env); err != nil {
		t.Fatal(err)
	}
}

func dispatchReceiptCreated(dispatch, attempt, agent string) map[string]any {
	return map[string]any{"action": "report", "sessionId": "session-test", "dispatchId": dispatch, "attemptId": attempt, "outcome": "created", "agentId": agent}
}

func dispatchReceiptFile(ws, dispatch string) string {
	return filepath.Join(ws, ".crw", "dispatches", "session-test", dispatch+".json")
}

// A created report before the spawn hook issued the attempt is refused and writes nothing, though the child is a real child of
// this session.
func TestDispatchReceiptCreatedRequiresIssuance(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	attempt, marker := dispatchReceiptClaim(t, ws, env, Executor, "task-test")
	env = dispatchReceiptNative(t, env, dispatchReceiptRow{id: "child-a", parent: "session-test", first: marker + "\nTASK"})
	file := dispatchReceiptFile(ws, "task-test")
	before := must(os.ReadFile(file))
	_, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("task-test", attempt, "child-a"), env, nil)
	if err == nil || !strings.Contains(err.Error(), "created requires this attempt's managed spawn issuance") {
		t.Fatalf("created before issuance = %v", err)
	}
	if string(before) != string(must(os.ReadFile(file))) {
		t.Fatal("the refused created report wrote state")
	}
}

// Once issued, an old child whose first message carries another attempt's marker is refused; the child the issued call created
// is adopted once with its receipt, a replay changes nothing, and a report that names another native call is refused.
func TestDispatchReceiptCorrelatesTheIssuedChild(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	attempt, marker := dispatchReceiptClaim(t, ws, env, Executor, "task-test")
	dispatchReceiptIssue(t, ws, marker, "call-1")
	env = dispatchReceiptNative(t, env,
		dispatchReceiptRow{id: "child-old", parent: "session-test", first: "[CRW-DISPATCH:task-old:attempt-old]\nOLD TASK"},
		dispatchReceiptRow{id: "child-plain", parent: "session-test", first: "an unmanaged task"},
		dispatchReceiptRow{id: "child-a", parent: "session-test", first: "ROLE PROMPT\n" + marker + "\nTASK", model: "xai/grok-4.6", effort: "high"},
	)
	file := dispatchReceiptFile(ws, "task-test")
	before := must(os.ReadFile(file))
	for _, agent := range []string{"child-old", "child-plain"} {
		if _, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("task-test", attempt, agent), env, nil); err == nil || !strings.Contains(err.Error(), "not the child the issued spawn created") {
			t.Fatalf("created of %s = %v", agent, err)
		}
	}
	if string(before) != string(must(os.ReadFile(file))) {
		t.Fatal("a refused created report wrote state")
	}
	report := dispatchReceiptCreated("task-test", attempt, "child-a")
	report["observedModel"] = "claimed/model"
	report["toolUseId"] = "call-1"
	out, err := CheckedDispatch(context.Background(), ws, report, env, nil)
	check(t, err)
	if out.Action != "wait" || out.Reason != "" {
		t.Fatalf("created = %q %q", out.Action, out.Reason)
	}
	r := out.Attempts[0].Receipt
	if r == nil || r.Correlation != "attempt-marker" || !r.Issuance.Recorded || r.Issuance.ToolUseID == nil || *r.Issuance.ToolUseID != "call-1" ||
		r.Child.AgentID != "child-a" || r.Child.Parent != "session-test" || r.Child.Witness != "native-thread-database" ||
		!dispatchEqual(r.Candidate.Model, out.Attempts[0].Candidate.Model) || !dispatchEqual(r.Candidate.Effort, out.Attempts[0].Candidate.Effort) {
		t.Fatalf("receipt = %+v", r)
	}
	if r.ObservedModel == nil || *r.ObservedModel != "claimed/model" || r.Host.Source != "native-thread-database" || r.Host.Model == nil || *r.Host.Model != "xai/grok-4.6" || r.Host.Effort == nil || *r.Host.Effort != "high" {
		t.Fatalf("receipt settings = %+v", r)
	}
	after := must(os.ReadFile(file))
	if again, err := CheckedDispatch(context.Background(), ws, report, env, nil); err != nil || again.Action != "wait" {
		t.Fatalf("replayed created = %q %v", again.Action, err)
	}
	if string(after) != string(must(os.ReadFile(file))) {
		t.Fatal("the replayed created report changed the record")
	}
	report["toolUseId"] = "call-2"
	if _, err := CheckedDispatch(context.Background(), ws, report, env, nil); err == nil || !strings.Contains(err.Error(), "toolUseId is not the native call this attempt was issued to") {
		t.Fatalf("created with another native call = %v", err)
	}
	if string(after) != string(must(os.ReadFile(file))) {
		t.Fatal("the refused created report wrote state")
	}
}

// A host that does not show the child's first message leaves the child unverified: it is recorded and waited on, but it
// cannot complete an independent review until a later created report correlates it; settings the host does not show are
// unobservable.
func TestDispatchReceiptUnverifiedChildCannotCompleteAReview(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	attempt, marker := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
	dispatchReceiptIssue(t, ws, marker, "call-1")
	native := dispatchReceiptNative(t, env, dispatchReceiptRow{id: "child-a", parent: "session-test"})
	out, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, "child-a"), native, nil)
	check(t, err)
	if r := out.Attempts[0].Receipt; out.Action != "wait" || !strings.Contains(out.Reason, "cannot satisfy independent review") || r == nil || r.Correlation != "unverified" || r.Host.Source != "unobservable" || r.Host.Model != nil {
		t.Fatalf("unverified created = %q %q %+v", out.Action, out.Reason, out.Attempts[0].Receipt)
	}
	complete := dispatchReceiptCreated("review-test", attempt, "child-a")
	complete["outcome"] = "complete"
	file := dispatchReceiptFile(ws, "review-test")
	before := must(os.ReadFile(file))
	if _, err := CheckedDispatch(context.Background(), ws, complete, native, nil); err == nil || !strings.Contains(err.Error(), "independent review") {
		t.Fatalf("complete of an unverified review child = %v", err)
	}
	if string(before) != string(must(os.ReadFile(file))) {
		t.Fatal("the refused complete wrote state")
	}
	// The host now shows the first message: the replayed created report correlates the child and the review completes.
	native = dispatchReceiptNative(t, env, dispatchReceiptRow{id: "child-a", parent: "session-test", first: marker + "\nREVIEW"})
	out, err = CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, "child-a"), native, nil)
	if check(t, err); out.Attempts[0].Receipt.Correlation != "attempt-marker" {
		t.Fatalf("correlated receipt = %+v", out.Attempts[0].Receipt)
	}
	out, err = CheckedDispatch(context.Background(), ws, complete, native, nil)
	if check(t, err); out.Action != "complete" {
		t.Fatalf("complete = %q", out.Action)
	}
}

// A child spawned while the hook was off is recorded only through the explicit reconciliation path: the created report names
// its evidence, nothing is deleted or spawned again, and the child cannot complete an independent review.
func TestDispatchReceiptUnissuedChildIsReconciled(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	attempt, marker := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
	env = dispatchReceiptNative(t, env, dispatchReceiptRow{id: "child-a", parent: "session-test", first: marker + "\nREVIEW"})
	report := dispatchReceiptCreated("review-test", attempt, "child-a")
	report["reconciliation"] = "spawned while the spawn hook was off; the child is real and its work is kept"
	out, err := CheckedDispatch(context.Background(), ws, report, env, nil)
	check(t, err)
	if r := out.Attempts[0].Receipt; out.Action != "wait" || r == nil || r.Correlation != "unissued" || r.Issuance.Recorded || r.Issuance.Reconciliation == nil {
		t.Fatalf("unissued created = %q %+v", out.Action, out.Attempts[0].Receipt)
	}
	complete := dispatchReceiptCreated("review-test", attempt, "child-a")
	complete["outcome"] = "complete"
	if _, err := CheckedDispatch(context.Background(), ws, complete, env, nil); err == nil || !strings.Contains(err.Error(), "independent review") {
		t.Fatalf("complete of an unissued review child = %v", err)
	}
	if out, err := CheckedDispatch(context.Background(), ws, map[string]any{"action": "claim", "sessionId": "session-test", "dispatchId": "review-test", "attemptId": attempt}, env, nil); err != nil || out.Action != "reconcile" {
		t.Fatalf("claim after the reconciled child = %q %v", out.Action, err)
	}
}

// A child of another parent and an id another attempt of the session holds stay refused once issued.
func TestDispatchReceiptWrongParentAndCrossAttempt(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	one, markerOne := dispatchReceiptClaim(t, ws, env, Executor, "task-one")
	two, markerTwo := dispatchReceiptClaim(t, ws, env, Executor, "task-two")
	dispatchReceiptIssue(t, ws, markerOne, "call-1")
	dispatchReceiptIssue(t, ws, markerTwo, "call-2")
	env = dispatchReceiptNative(t, env,
		dispatchReceiptRow{id: "child-a", parent: "session-test", first: markerOne + "\nTASK"},
		dispatchReceiptRow{id: "foreign", parent: "other-session", first: markerTwo + "\nTASK"},
	)
	if _, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("task-two", two, "foreign"), env, nil); err == nil || !strings.Contains(err.Error(), "not a real subagent thread parented by this session") {
		t.Fatalf("created of a foreign child = %v", err)
	}
	_, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("task-one", one, "child-a"), env, nil)
	check(t, err)
	if _, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("task-two", two, "child-a"), env, nil); err == nil || !strings.Contains(err.Error(), "already reported for dispatch task-one") {
		t.Fatalf("cross-attempt created = %v", err)
	}
}

// A stored status must be one of the literal enum strings: a singleton array is an invalid record, and nothing rewrites it.
func TestDispatchReceiptStatusIsALiteralString(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		edit       func(d, a map[string]any)
	}{
		{"dispatch", "invalid dispatch status", func(d, a map[string]any) { d["status"] = []any{"active"} }},
		{"attempt", "invalid attempt status", func(d, a map[string]any) { a["status"] = []any{"ready"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, env, start, file := dispatchTestFixture(t)
			var d map[string]any
			check(t, json.Unmarshal(must(os.ReadFile(file)), &d))
			tc.edit(d, d["attempts"].([]any)[0].(map[string]any))
			before := must(Stringify(d, ""))
			check(t, os.WriteFile(file, before, 0o600))
			for _, input := range []map[string]any{{"action": "status"}, {"action": "claim", "attemptId": start.AttemptID}} {
				input["sessionId"], input["dispatchId"] = "session-test", "task-test"
				if _, err := CheckedDispatch(context.Background(), ws, input, env, nil); err == nil || err.Error() != tc.want {
					t.Fatalf("%v of a coerced status = %v", input["action"], err)
				}
			}
			if string(before) != string(must(os.ReadFile(file))) {
				t.Fatal("the invalid record was rewritten")
			}
		})
	}
}

// dispatchTestClaimIssued is dispatchTestCall for a claim followed by the spawn hook's issuance of the claimed attempt, the
// sequence a created report requires at the checked boundary.
func dispatchTestClaimIssued(t *testing.T, ws string, env host.LookupEnv, fields map[string]any) DispatchResult {
	t.Helper()
	r := dispatchTestCall(t, ws, env, fields)
	dispatchReceiptIssue(t, ws, r.Marker, "call-"+r.AttemptID)
	return r
}

// The marker in a child's first message is not proof that the issued call created it: a child made before the spawn was issued
// (the hook off, an older call) can carry the same claimed marker. The hook records the marked children the host already shows
// when it issues the attempt, so such a child is refused and writes nothing; a new child is tied to the call only when no
// other new child carries the marker (two leave it unverified), and an issuance that could not read the host's database leaves
// every child unverified.
func TestDispatchReceiptMarkerAloneDoesNotProveTheIssuedCall(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	attempt, marker := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
	old := dispatchReceiptRow{id: "child-old", parent: "session-test", first: marker + "\nREVIEW"}
	dispatchReceiptIssueSeeing(t, ws, dispatchReceiptNative(t, env, old), marker, "call-1")
	file := dispatchReceiptFile(ws, "review-test")
	if stored := must(dispatchRead(file, "session-test", "review-test")).Attempts[0]; len(stored.PriorChildren) != 1 || stored.PriorChildren[0] != "child-old" || stored.PriorUnobserved {
		t.Fatalf("issuance recorded prior %v %v", stored.PriorChildren, stored.PriorUnobserved)
	}
	fresh := dispatchReceiptRow{id: "child-new", parent: "session-test", first: marker + "\nREVIEW"}
	twin := dispatchReceiptRow{id: "child-twin", parent: "session-test", first: marker + "\nREVIEW"}
	before := must(os.ReadFile(file))
	if _, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, "child-old"), dispatchReceiptNative(t, env, old, fresh), nil); err == nil || !strings.Contains(err.Error(), "when the spawn was issued") {
		t.Fatalf("created of a child the host showed at issuance = %v", err)
	}
	if string(before) != string(must(os.ReadFile(file))) {
		t.Fatal("the refused created report wrote state")
	}
	out, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, "child-new"), dispatchReceiptNative(t, env, old, fresh, twin), nil)
	check(t, err)
	if r := out.Attempts[0].Receipt; r == nil || r.Correlation != "unverified" {
		t.Fatalf("created with two new marked children = %+v", out.Attempts[0].Receipt)
	}
	out, err = CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, "child-new"), dispatchReceiptNative(t, env, old, fresh), nil)
	check(t, err)
	if r := out.Attempts[0].Receipt; r == nil || r.Correlation != "attempt-marker" {
		t.Fatalf("created of the only new child = %+v", out.Attempts[0].Receipt)
	}

	// A host database that cannot be read at issuance leaves nothing to compare with.
	attempt, marker = dispatchReceiptClaim(t, ws, env, Reviewer, "review-two")
	broken := t.TempDir()
	check(t, os.WriteFile(filepath.Join(broken, "state_5.sqlite"), []byte("not a database"), 0o600))
	dispatchReceiptIssueSeeing(t, ws, func(k string) (string, bool) {
		if k == "CODEX_HOME" {
			return broken, true
		}
		return env(k)
	}, marker, "call-2")
	if stored := must(dispatchRead(dispatchReceiptFile(ws, "review-two"), "session-test", "review-two")).Attempts[0]; !stored.PriorUnobserved {
		t.Fatal("an unreadable database at issuance was not recorded")
	}
	out, err = CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-two", attempt, "child-b"), dispatchReceiptNative(t, env, dispatchReceiptRow{id: "child-b", parent: "session-test", first: marker + "\nREVIEW"}), nil)
	check(t, err)
	if r := out.Attempts[0].Receipt; r == nil || r.Correlation != "unverified" {
		t.Fatalf("created after an unobserved issuance = %+v", out.Attempts[0].Receipt)
	}
}

// A replayed created report never weakens a receipt already confirmed: when the host no longer shows the child's first message
// or its settings, the stored record stays byte-identical and the review still completes.
func TestDispatchReceiptReplayKeepsAConfirmedReceipt(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	attempt, marker := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
	dispatchReceiptIssue(t, ws, marker, "call-1")
	seen := dispatchReceiptNative(t, env, dispatchReceiptRow{id: "child-a", parent: "session-test", first: marker + "\nREVIEW", model: "xai/grok-4.6", effort: "high"})
	out, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, "child-a"), seen, nil)
	check(t, err)
	if out.Attempts[0].Receipt.Correlation != "attempt-marker" {
		t.Fatalf("receipt = %+v", out.Attempts[0].Receipt)
	}
	file := dispatchReceiptFile(ws, "review-test")
	before := must(os.ReadFile(file))
	blind := dispatchReceiptNative(t, env, dispatchReceiptRow{id: "child-a", parent: "session-test"})
	out, err = CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, "child-a"), blind, nil)
	check(t, err)
	if r := out.Attempts[0].Receipt; r.Correlation != "attempt-marker" || out.Reason != "" {
		t.Fatalf("replayed receipt = %+v %q", r, out.Reason)
	}
	if string(before) != string(must(os.ReadFile(file))) {
		t.Fatal("a replayed created report rewrote a confirmed receipt")
	}
	complete := dispatchReceiptCreated("review-test", attempt, "child-a")
	complete["outcome"] = "complete"
	if out, err := CheckedDispatch(context.Background(), ws, complete, blind, nil); err != nil || out.Action != "complete" {
		t.Fatalf("complete after the replay = %q %v", out.Action, err)
	}
}
