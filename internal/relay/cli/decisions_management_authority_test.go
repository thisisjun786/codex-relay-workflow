package cli_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1371 (CRW-1314 lane D3, design §5): with a management seat per session, the answer to a
// user-grade or delegated-management-grade decision belongs to the management seat directly above
// the project the decision was raised from. decision-raise records that seat in authority.ref (the
// upper scope key of the management -> project execution edge a management session wrote when it
// bound the project owner); decision-answer accepts such an answer only from that seat's current
// owner, so a handover of the seat moves the right with it. A record without a management ref keeps
// the store seat's rule, and the grade and via rules are unchanged.

const (
	authSeatA, authSeatB, authSeatNew  = "mgmt-a", "mgmt-b", "mgmt-new"
	authOwnerA, authOwnerB, authOwnerC = "owner-a", "owner-b", "owner-c"
)

// authRelay runs one relay line against state and fails the test unless it exits 0.
func authRelay(t *testing.T, state string, argv ...string) map[string]any {
	t.Helper()
	got := crw737Run(t, append([]string{"--state", state}, argv...)...)
	if got.code != 0 {
		t.Fatalf("%v: exit %d %s %s", argv, got.code, got.stdout, got.stderr)
	}
	return crw737JSON(t, got)
}

// authWorld is a store with two management seats, each the creator of one project owner's binding
// (P-A under mgmt-a, P-B under mgmt-b), a project P-C whose owner bound itself (no management edge),
// and the store seat held by task-sup.
func authWorld(t *testing.T) string {
	t.Helper()
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	for _, seat := range []string{authSeatA, authSeatB} {
		authRelay(t, state, "linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", seat, "--task", seat, "--host", "host")
	}
	for _, bind := range []struct{ project, owner, creator string }{{"P-A", authOwnerA, authSeatA}, {"P-B", authOwnerB, authSeatB}, {"P-C", authOwnerC, ""}} {
		argv := []string{"linkage-bind", "--role", "parent", "--scope", bind.project, "--task", bind.owner, "--host", "host"}
		if bind.creator != "" {
			argv = append(argv, "--creator", bind.creator)
		}
		bound := authRelay(t, state, argv...)
		if (bound["managementLink"] != nil) != (bind.creator != "") {
			t.Fatalf("binding %s answered %v", bind.project, bound)
		}
	}
	return state
}

// authRaise raises one question from project at the grade given (kind[=ref]) and returns its id.
func authRaise(t *testing.T, state, project, question, authority string) (string, map[string]any) {
	t.Helper()
	raised := authRelay(t, state, "decision-raise", "--kind", "policy", "--context", question,
		"--option", "now=now:the window opens at once", "--option", "later=later:the chain stays blocked",
		"--origin-project", project, "--source", "report=1", "--authority", authority)
	decision, _ := raised["decisionId"].(string)
	if decision == "" {
		t.Fatalf("the raise answered %v", raised)
	}
	return decision, raised
}

// authRecord is the record decision-list reads back for decision.
func authRecord(t *testing.T, state, decision string) map[string]any {
	t.Helper()
	for _, raw := range crw737List(t, state) {
		if record, _ := raw.(map[string]any); record["decision_id"] == decision {
			return record
		}
	}
	t.Fatalf("decision-list has no record %s", decision)
	return nil
}

// authRef is the authority.ref the record of decision holds (nil when none).
func authRef(t *testing.T, state, decision string) any {
	t.Helper()
	authority, _ := authRecord(t, state, decision)["authority"].(map[string]any)
	return authority["ref"]
}

// authJournal is the subject and detail of each decision_authority_warning journal row, in order.
func authJournal(t *testing.T, state string) []string {
	t.Helper()
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	rows, err := opened.All(ctx, "SELECT subject, detail FROM journal WHERE kind = 'decision_authority_warning' ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.Text("subject") + " " + row.Text("detail")
	}
	return out
}

// authAnswer is one decision-answer line choosing now, by the task given, at the grade given.
func authAnswer(t *testing.T, state, decision, by, authority string) crw737Answer {
	t.Helper()
	return crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now",
		"--by", by, "--via", "management-message", "--authority", authority)
}

// authAccepted requires the answer to be accepted and the record answered by by.
func authAccepted(t *testing.T, state, decision, by string, got crw737Answer) {
	t.Helper()
	if got.code != 0 {
		t.Fatalf("%s's answer to %s: exit %d %s %s", by, decision, got.code, got.stdout, got.stderr)
	}
	if record := authRecord(t, state, decision); record["state"] != "answered" || record["answered_by"] != by {
		t.Fatalf("the record after %s's answer: %v", by, record)
	}
}

// TestDecisionRaiseRecordsTheManagementSeatAboveTheProject (criterion c1): a user-grade or
// delegated-management-grade question raised from a project a management session bound records that
// session's seat in authority.ref; from a project with no management edge the ref stays as it was
// (empty); a parent-class ref is the raiser's and is kept.
func TestDecisionRaiseRecordsTheManagementSeatAboveTheProject(t *testing.T) {
	state := authWorld(t)
	for _, c := range []struct {
		project, authority string
		want               any
	}{
		{"P-A", "delegated-management", authSeatA},
		{"P-B", "delegated-management", authSeatB},
		{"P-A", "user", authSeatA},
		{"P-B", "user", authSeatB},
		{"P-C", "delegated-management", nil},
		{"P-C", "user", nil},
		{"P-A", "parent=" + authOwnerA, authOwnerA},
	} {
		decision, raised := authRaise(t, state, c.project, "Which window does "+c.project+" take at "+c.authority+"?", c.authority)
		if got := authRef(t, state, decision); got != c.want {
			t.Errorf("%s at %s: authority.ref %v, want %v", c.project, c.authority, got, c.want)
		}
		if raised["authorityWarning"] != nil {
			t.Errorf("%s at %s: an unexpected warning %v", c.project, c.authority, raised["authorityWarning"])
		}
	}
}

// TestDecisionAnswerTwoManagementSeatsEachAnswerTheirOwnProject (criterion c2): each management
// session answers the delegated-management-grade (and user-grade) question of its own project, and
// the other session's answer, and the store seat's, are refused authority_unverified.
func TestDecisionAnswerTwoManagementSeatsEachAnswerTheirOwnProject(t *testing.T) {
	state := authWorld(t)
	for _, grade := range []string{"delegated-management", "user"} {
		fromA, _ := authRaise(t, state, "P-A", "Ship P-A before the review ("+grade+")?", grade)
		fromB, _ := authRaise(t, state, "P-B", "Ship P-B before the review ("+grade+")?", grade)
		review818RefusedFor(t, authAnswer(t, state, fromA, authSeatB, grade), "authority_unverified", authSeatA)
		review818RefusedFor(t, authAnswer(t, state, fromB, authSeatA, grade), "authority_unverified", authSeatB)
		review818RefusedFor(t, authAnswer(t, state, fromA, "task-sup", grade), "authority_unverified", authSeatA)
		review818RefusedFor(t, authAnswer(t, state, fromA, authOwnerA, grade), "authority_unverified", authSeatA)
		authAccepted(t, state, fromA, authSeatA, authAnswer(t, state, fromA, authSeatA, grade))
		// The answer may cite the seat it answers from; it is the record's ref either way.
		authAccepted(t, state, fromB, authSeatB, authAnswer(t, state, fromB, authSeatB, grade+"="+authSeatB))
	}
	// An answer that cites another seat than the record's is the existing ref rule's refusal.
	other, _ := authRaise(t, state, "P-A", "Which window does the restart take?", "delegated-management")
	review818RefusedFor(t, authAnswer(t, state, other, authSeatA, "delegated-management="+authSeatB), "bad_invocation", "the record holds")
}

// TestDecisionAnswerFollowsTheManagementSeatHandover (criterion c2): a question raised before the
// seat changed hands is answered by the seat's new owner, and the old owner is refused.
func TestDecisionAnswerFollowsTheManagementSeatHandover(t *testing.T) {
	state := authWorld(t)
	decision, _ := authRaise(t, state, "P-A", "Ship P-A before the review?", "delegated-management")
	authRelay(t, state, "linkage-handover", "--role", "supervisor", "--scope-kind", "management", "--scope", authSeatA,
		"--expect-task", authSeatA, "--task", authSeatNew, "--host", "host", "--evidence", "the management session was replaced", "--actor", "test")
	if got := authRef(t, state, decision); got != authSeatA {
		t.Fatalf("the record's ref moved to %v; it names the seat, not its owner", got)
	}
	review818RefusedFor(t, authAnswer(t, state, decision, authSeatA, "delegated-management"), "authority_unverified", authSeatNew)
	authAccepted(t, state, decision, authSeatNew, authAnswer(t, state, decision, authSeatNew, "delegated-management"))
	// A question raised after the handover names the same seat.
	after, _ := authRaise(t, state, "P-A", "Which window does the restart take?", "user")
	if got := authRef(t, state, after); got != authSeatA {
		t.Fatalf("the ref after the handover is %v, want the seat %s", got, authSeatA)
	}
	authAccepted(t, state, after, authSeatNew, authAnswer(t, state, after, authSeatNew, "user"))
}

// TestDecisionAnswerAManagementSeatWithoutAnOwnerIsRefused: a record whose seat has no live owner
// has nobody to answer it; the store seat does not stand in for it.
func TestDecisionAnswerAManagementSeatWithoutAnOwnerIsRefused(t *testing.T) {
	state := authWorld(t)
	decision, _ := authRaise(t, state, "P-A", "Ship P-A before the review?", "delegated-management")
	review818Exec(t, state, "UPDATE scope_bindings SET status = 'archived' WHERE scope_kind = 'management' AND scope_key = ?", authSeatA)
	review818RefusedFor(t, authAnswer(t, state, decision, authSeatA, "delegated-management"), "authority_unverified", authSeatA)
	review818RefusedFor(t, authAnswer(t, state, decision, "task-sup", "delegated-management"), "authority_unverified", authSeatA)
}

// TestDecisionAnswerWithoutAManagementRefKeepsTheStoreSeatRule (criterion c3): a question from a
// project no management session bound, and a record raised before the management edge stood (an
// old row with no ref), are answered by the store seat's owner as before; a management session is
// refused.
func TestDecisionAnswerWithoutAManagementRefKeepsTheStoreSeatRule(t *testing.T) {
	state := authWorld(t)
	unmanaged, _ := authRaise(t, state, "P-C", "Which window does P-C take?", "delegated-management")
	review818RefusedFor(t, authAnswer(t, state, unmanaged, authSeatA, "delegated-management"), "authority_unverified", "store-scope supervisor")
	authAccepted(t, state, unmanaged, "task-sup", authAnswer(t, state, unmanaged, "task-sup", "delegated-management"))

	// An old row: raised from P-D before a management session bound its owner, so it holds no ref.
	old, _ := authRaise(t, state, "P-D", "Which window does P-D take?", "user")
	authRelay(t, state, "linkage-bind", "--role", "parent", "--scope", "P-D", "--task", "owner-d", "--host", "host", "--creator", authSeatA)
	if got := authRef(t, state, old); got != nil {
		t.Fatalf("the old record's ref is %v, want none", got)
	}
	review818RefusedFor(t, authAnswer(t, state, old, authSeatA, "user"), "authority_unverified", "store-scope supervisor")
	authAccepted(t, state, old, "task-sup", authAnswer(t, state, old, "task-sup", "user"))
}

// TestDecisionRaiseOfANightBatchNamesTheSeatAboveThePlansProject: crw manage batch decide raises
// its record with the flow plan as the origin project (--origin-project PLAN) and answers it with
// --authority delegated-management and no ref. The plan's project is the project context, so the
// record names the seat above that project and the manager of that project answers with the same
// lines it always ran; the other manager is refused.
func TestDecisionRaiseOfANightBatchNamesTheSeatAboveThePlansProject(t *testing.T) {
	state := authWorld(t)
	review818Exec(t, state, "INSERT INTO dag_plans (plan_id, project_key, created_by_task_id, created_at) VALUES (?,?,?,?)", "plan-b", "P-B", authOwnerB, "2026-10-14T00:00:00Z")
	raised := authRelay(t, state, "decision-raise", "--kind", "policy", "--context", `{"schema":"crw-night-batch/1"}`,
		"--option", "run=Run the night batch:starts the run", "--option", "skip=Skip tonight:nothing is started",
		"--blocking", "project=plan-b", "--needed-by", "2026-10-15T00:00:00Z", "--origin-project", "plan-b",
		"--source", "night_batch=n1", "--authority", "delegated-management", "--raised-via", "crw-manage-batch-decide")
	decision, _ := raised["decisionId"].(string)
	if got := authRef(t, state, decision); got != authSeatB {
		t.Fatalf("the night batch record's ref is %v, want the seat above plan-b's project %s", got, authSeatB)
	}
	answer := func(by string) crw737Answer {
		return crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "run", "--by", by,
			"--via", "management-message", "--authority", "delegated-management")
	}
	review818RefusedFor(t, answer(authSeatA), "authority_unverified", authSeatB)
	if got := answer(authSeatB); got.code != 0 {
		t.Fatalf("the plan's manager answering as batch decide does: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
}

// TestDecisionRaiseWarnsAndKeepsGoing (criterion c5): what the raise cannot settle is warned and
// recorded, never refused. A ref the raiser named that is not the seat above the project is replaced
// by that seat; two live management edges above one project leave the ref as given. Each answer
// carries authorityWarning and the journal one decision_authority_warning row.
func TestDecisionRaiseWarnsAndKeepsGoing(t *testing.T) {
	state := authWorld(t)
	replaced, raised := authRaise(t, state, "P-A", "Ship P-A before the review?", "user="+authSeatB)
	if got := authRef(t, state, replaced); got != authSeatA {
		t.Fatalf("the ref is %v, want the seat above P-A %s", got, authSeatA)
	}
	warning, _ := raised["authorityWarning"].(map[string]any)
	if warning["reason"] != "ref_replaced" || warning["given"] != authSeatB || warning["seat"] != authSeatA {
		t.Fatalf("the warning %v", raised["authorityWarning"])
	}

	// A second live edge above P-C (the one-edge rule is the writer's, so only a hand-written row
	// makes it): the raise is recorded with the ref as given, warned and journaled.
	for _, seat := range []string{authSeatA, authSeatB} {
		review818Exec(t, state, "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key, upper_task_id, lower_kind, lower_key, lower_task_id, status, revision, superseded_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
			"lk-1371-"+seat, "execution", "management", seat, seat, "project", "P-C", authOwnerC, "active", int64(1), nil, "2026-10-14T00:00:00Z", "2026-10-14T00:00:00Z")
	}
	contested, raised := authRaise(t, state, "P-C", "Which window does P-C take?", "delegated-management")
	if got := authRef(t, state, contested); got != nil {
		t.Fatalf("the contested raise's ref is %v, want none", got)
	}
	warning, _ = raised["authorityWarning"].(map[string]any)
	if warning["reason"] != "seat_contested" || !strings.Contains(warning["detail"].(string), authSeatA) || !strings.Contains(warning["detail"].(string), authSeatB) {
		t.Fatalf("the warning %v", raised["authorityWarning"])
	}
	review818RefusedFor(t, authAnswer(t, state, contested, authSeatA, "delegated-management"), "authority_unverified", "store-scope supervisor")
	authAccepted(t, state, contested, "task-sup", authAnswer(t, state, contested, "task-sup", "delegated-management"))

	rows := authJournal(t, state)
	if len(rows) != 2 || !strings.Contains(rows[0], replaced) || !strings.Contains(rows[0], "ref_replaced") || !strings.Contains(rows[1], contested) || !strings.Contains(rows[1], "seat_contested") {
		t.Fatalf("decision_authority_warning rows %q", rows)
	}
}
