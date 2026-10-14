package adapter

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// CRW-1371 (CRW-1314 lane D3, design §5 and §7), end to end over the scripted App Server and
// continuing the D1c cases: a management session binds a project owner, the project raises a
// delegated-management decision and sends its decision request up, the idle management thread is
// given a turn, the management session answers in that turn (the other management session cannot),
// and the project side reads the answer back and goes on.

// relayRaw runs one relay command line against the world's store and returns its exit code and
// stdout, for a line that may be refused or answers an array.
func (w *mgmtWorld) relayRaw(argv ...string) (int, []byte) {
	w.t.Helper()
	var stdout, stderr bytes.Buffer
	code := dispatch.Execute(w.ctx, "codex-session-relay", append([]string{"--state", w.state}, argv...), &stdout, &stderr)
	return code, stdout.Bytes()
}

// TestManagementProjectE2EDecisionIsAnsweredByTheManagementAbove: the decision a project raises is
// recorded for the management seat above it, its request opens a turn in that idle management
// thread only, the other management session's answer is refused authority_unverified, the owning
// session's answer is recorded, and the project owner reads it and sends its next report, which
// reaches the same management thread.
func TestManagementProjectE2EDecisionIsAnsweredByTheManagementAbove(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtB, "--task", mgmtB, "--host", "host")
	for _, bound := range []map[string]any{w.bindOwner("P-A", ownerA, mgmtA), w.bindOwner("P-B", ownerB, mgmtB)} {
		if bound["managementLink"] == nil {
			t.Fatalf("the project owner binding wrote no management link: %v", bound)
		}
	}

	// The project side raises the question and sends the request up.
	raised := w.relay("decision-raise", "--kind", "policy", "--context", "Ship P-A before the review?",
		"--option", "ship=ship:ship before the review", "--option", "wait=wait:wait for the review",
		"--origin-project", "P-A", "--origin-issue", "CRW-A1", "--source", "report=rep_a1",
		"--authority", "delegated-management", "--raised-via", "project-owner")
	decision, _ := raised["decisionId"].(string)
	if decision == "" || raised["authorityWarning"] != nil {
		t.Fatalf("the raise answered %v", raised)
	}
	w.open(mgmtA, mgmtB)
	request := w.stageProject(supervisor.KindFlowProjectDecisionRequest, "P-A", decision, "ship P-A before the review?",
		"owner A asks whether to ship before the review: answer decision "+decision)
	if request["recipient"] != mgmtA || request["recipientScopeKind"] != "management" {
		t.Fatalf("the decision request was staged %v, want it for %s", request, mgmtA)
	}
	w.tick(4)
	a := w.turnsOf(mgmtA)
	if len(a) != 1 || !strings.Contains(a[0], decision) {
		t.Fatalf("management A's turns %q\n%s", a, w.dump())
	}
	if len(w.turnsOf(mgmtB))+len(w.turnsOf(ownerA))+len(w.turnsOf(ownerB)) != 0 {
		t.Fatalf("the decision request reached another thread\n%s", w.dump())
	}
	if state := w.messageState(request["messageId"].(string)); state != "dispatched" {
		t.Fatalf("the decision request is %s after the ticks, want it sent\n%s", state, w.dump())
	}

	// In that turn the management session answers. The other session cannot.
	answer := func(by string) (int, []byte) {
		return w.relayRaw("decision-answer", "--decision", decision, "--option", "ship", "--by", by,
			"--via", "management-message", "--authority", "delegated-management")
	}
	code, out := answer(mgmtB)
	var refusal map[string]any
	if err := json.Unmarshal(out, &refusal); err != nil || code != 2 || refusal["reason"] != "authority_unverified" {
		t.Fatalf("management B's answer: exit %d %s (%v), want authority_unverified", code, out, err)
	}
	if code, out := answer(mgmtA); code != 0 {
		t.Fatalf("management A's answer: exit %d %s", code, out)
	}

	// The project side reads the answer and goes on.
	code, out = w.relayRaw("decision-list", "--project", "P-A")
	var records []map[string]any
	if err := json.Unmarshal(out, &records); err != nil || code != 0 || len(records) != 1 {
		t.Fatalf("decision-list --project P-A: exit %d %s (%v)", code, out, err)
	}
	record := records[0]
	authority, _ := record["authority"].(map[string]any)
	if record["state"] != "answered" || record["answered_by"] != mgmtA || record["answer_text"] != "ship" || authority["ref"] != mgmtA {
		t.Fatalf("the record the project reads back: %v", record)
	}
	next := w.stageProject(supervisor.KindFlowMilestoneReport, "P-A", "rep_a2", "milestone M1 closed",
		"owner A shipped before the review as decision "+decision+" answered, and closed milestone M1")
	if next["recipient"] != mgmtA {
		t.Fatalf("the next report was staged %v, want it for %s", next, mgmtA)
	}
	w.tick(3)
	if a := w.turnsOf(mgmtA); len(a) != 2 || !strings.Contains(a[1], "closed milestone M1") {
		t.Fatalf("management A's turns after the answer %q\n%s", a, w.dump())
	}
	if len(w.turnsOf(mgmtB)) != 0 {
		t.Fatalf("management B was sent %q", w.turnsOf(mgmtB))
	}
}
