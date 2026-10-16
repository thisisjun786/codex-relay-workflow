package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/audit"
	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
)

// The close of a milestone (CRW-1206, F13) on the whole stack of the scenario harness: the real agent backend on the fake App Server
// host, a temporary store and repository, the lane of a project from its architect to its report.

type closeResult struct {
	State  string `json:"state"`
	Tip    string `json:"tip"`
	Source string `json:"source"`
	Reason string `json:"reason"`
	Gate   string `json:"gate"`
	// Crosscheck is the word of the crosscheck's verdict the milestone closed over (CRW-1473).
	Crosscheck any `json:"crosscheck"`
	// Ask is the question an escalated close asks (crosscheck_pending for a crosscheck that needs changes).
	Ask      string `json:"ask"`
	Archived int    `json:"archived"`
	Audit    struct {
		OK         int `json:"ok"`
		Mismatch   int `json:"mismatch"`
		Unobserved int `json:"unobserved"`
	} `json:"audit"`
}

func closeOf(t *testing.T, l *Lab) closeResult {
	t.Helper()
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s: %s\ncalls: %v", res.Status, res.ErrorKind, res.Error, l.Labels())
	}
	return Value[closeResult](t, res)
}

func escalationReasons(l *Lab) []string {
	var out []string
	for _, ev := range l.FlowRows(engine.EvEscalation) {
		out = append(out, str(ev.Data, "reason"))
	}
	return out
}

// A milestone with a web criterion at C3: the run makes the test evidence at the integrated head, the QA capture call and both
// oracle passes, the synthesis on the capture call's thread, and the crosscheck with the passed gate. Every acceptance rests on a unit
// check of its node (CRW-1465): the audit vouches for it on its crw.unitCheck row and the executor's pair, reads no thread from the
// host, and archives nothing, since no reviewer thread was started.
func TestCloseWithOraclePassesAuditsAndArchives(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	l.Relay.Surfaces, l.Relay.Grade = []string{"logic", "web"}, "C3"
	got := closeOf(t, l)
	if got.State != "closed" || got.Gate != "pass" || got.Crosscheck != "verified" || got.Tip != l.Tip() {
		t.Fatalf("%+v; calls %v; escalations %v", got, l.Labels(), escalationReasons(l))
	}
	labels := l.Labels()
	tail := labels[len(labels)-5:]
	if tail[0] != "qa:M1" || tail[3] != "qa-synthesis:M1" || tail[4] != "reflection:M1" || !slices.Contains(tail[1:3], "qa-pass:A:M1") || !slices.Contains(tail[1:3], "qa-pass:B:M1") {
		t.Fatalf("calls %v", labels)
	}
	roles := map[string]string{}
	for _, c := range l.Calls() {
		roles[c.Label] = c.Role
	}
	if roles["qa:M1"] != "executor" || roles["qa-pass:A:M1"] != "explorer" || roles["qa-pass:B:M1"] != "explorer" || slices.Contains(l.Roles(), "reviewer-final") {
		t.Errorf("roles %v", roles)
	}
	// the crosscheck judged the range at the gate's head: one verified round of two passes, in checkouts under the run's directory
	cc := l.Rows("crw.crosscheck")
	if len(cc) != 1 || str(cc[0].Data, "state") != "verdict" || str(cc[0].Data, "head") != l.Tip() {
		t.Errorf("crosscheck rows %+v", cc)
	}
	if calls := l.Crosschecker.Calls(); len(calls) != 2 {
		t.Errorf("%d crosscheck passes, want A and B", len(calls))
	}
	// the QA call is one thread: the capture and the synthesis are its two turns, in a call-private scratch worktree at the head
	qa, synth := callNamed(t, l, "qa:M1"), callNamed(t, l, "qa-synthesis:M1")
	if synth.Resume != qa.ID {
		t.Errorf("the synthesis continues %q, want the QA call %s", synth.Resume, qa.ID)
	}
	prep := l.Prepared(qa.ID)
	alloc, _ := prep.Data["allocation"].(map[string]any)
	if str(alloc, "kind") != engine.PlaceCall || str(alloc, "start_commit") != l.Tip() || !strings.Contains(str(alloc, "path"), "/calls/") || prep.Data["qa"] != true {
		t.Errorf("the QA call's allocation %v, row %v", alloc, prep.Data)
	}
	// the gate: first pending over missing evidence, then pass; one receipt; the verification of the head
	gates := l.Rows("crw.finalGate")
	if len(gates) != 2 || str(gates[0].Data, "state") != "pending" || str(gates[1].Data, "state") != "pass" || dataBoolOf(gates[1], "ok") != true {
		t.Errorf("gate rows %+v", gates)
	}
	if rows := l.Rows("crw.qaReceipt"); len(rows) != 1 || str(rows[0].Data, "state") != "PASS" || str(rows[0].Data, "head") != l.Tip() {
		t.Errorf("receipt rows %+v", rows)
	}
	var milestoneVerify int
	for _, c := range l.Verify.Calls {
		if c.Node == "" {
			milestoneVerify++
			if c.Head != l.Tip() || c.Worktree != l.Repo {
				t.Errorf("the verification of the integrated head: %+v", c)
			}
		}
	}
	if milestoneVerify != 1 {
		t.Errorf("%d verifications of the integrated head", milestoneVerify)
	}
	// SP-QA-1 and SP-QA-2 twice are decided; the crosscheck is no spawn, and reviewer-final (SP-REVF-1) is not called
	rules := map[string]int{}
	for _, r := range l.SpawnRows() {
		rules[r.Rule]++
	}
	if rules["SP-QA-1"] != 1 || rules["SP-QA-2"] != 2 || rules["SP-REVF-1"] != 0 {
		t.Errorf("rules %v", rules)
	}
	// the audit vouched for both acceptances on their unit-check rows; no reviewer thread was started, read or archived
	if got.Audit.OK != 2 || got.Audit.Mismatch != 0 || got.Audit.Unobserved != 0 || got.Archived != 0 {
		t.Fatalf("audit %+v, archived %d", got.Audit, got.Archived)
	}
	for _, c := range l.Calls() {
		if c.Role == "reviewer" || strings.HasPrefix(c.Label, "review:") {
			t.Errorf("a reviewer call %q", c.Label)
		}
	}
	if archived := l.Archived(); len(archived) != 0 {
		t.Errorf("archived %v, want nothing: a unit check has no thread", archived)
	}
	// the audit rows are the kind of the row the audit leaves; each result names the unit-check row and no thread
	rows := l.Rows(audit.Op)
	if len(rows) != 1 || len(audit.Passed(l.Events())) != 2 {
		t.Errorf("audit rows %d, passed %v", len(rows), audit.Passed(l.Events()))
	}
	for _, r := range audit.Passed(l.Events()) {
		if r.Subject.Basis != audit.BasisUnitCheck || r.Thread != "" || len(r.Checks) == 0 || r.Checks[0].Name != audit.CheckUnitCheck {
			t.Errorf("an audit result %+v, want one on the unit-check row with no thread", r)
		}
	}
}

// An acceptance the audit cannot fully observe is unobserved: not a pass, not a mismatch. On a unit check (CRW-1465) what can go
// unobserved is the pair of the executor that wrote the head: here the thread of B's executor records no model. The milestone still
// closes, B's acceptance is unobserved, A's is ok, nothing is escalated, and nothing is archived (a unit check has no thread to keep or
// to archive).
func TestCloseKeepsTheThreadOfAnAcceptanceItCouldNotObserve(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	l.Rewrite(func(ev engine.Event) engine.Event {
		if ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Event == engine.EvThreadStarted && ev.Label == "build:B:executor" {
			d := map[string]any{}
			for k, v := range ev.Data {
				if k != "model" {
					d[k] = v
				}
			}
			ev.Data = d
		}
		return ev
	})
	got := closeOf(t, l)
	if got.State != "closed" || got.Audit.OK != 1 || got.Audit.Unobserved != 1 || got.Audit.Mismatch != 0 || got.Archived != 0 {
		t.Fatalf("%+v", got)
	}
	if archived := l.Archived(); len(archived) != 0 {
		t.Errorf("archived %v, want nothing", archived)
	}
	if reasons := escalationReasons(l); slices.Contains(reasons, "audit_mismatch") {
		t.Errorf("escalations %v: an unobserved acceptance is not a mismatch", reasons)
	}
}

// The unit check an acceptance rests on (CRW-1465) must have judged the criteria the node was released with: a crw.unitCheck row that
// names another criteria digest than the node's release is a mismatch. The orchestrator is told, and nothing is archived.
func TestCloseEscalatesAnAuditMismatchAndKeepsTheThread(t *testing.T) {
	l := NewLab(t, oneNode()...)
	// the check's row names a criteria digest the node was not released with
	l.Rewrite(func(ev engine.Event) engine.Event {
		if ev.Kind == engine.KindCall && ev.Op == ops.OpUnitCheck && ev.Event == engine.EvCompleted && str(ev.Data, "node") == "A" {
			d := map[string]any{}
			for k, v := range ev.Data {
				d[k] = v
			}
			d["criteria_digest"] = strings.Repeat("0", 64)
			ev.Data = d
		}
		return ev
	})
	got := closeOf(t, l)
	if got.State != "closed" || got.Audit.Mismatch != 1 || got.Audit.OK != 0 || got.Archived != 0 || len(l.Archived()) != 0 {
		t.Fatalf("%+v; archived %v", got, l.Archived())
	}
	if reasons := escalationReasons(l); !slices.Contains(reasons, "audit_mismatch") {
		t.Errorf("escalations %v", reasons)
	}
}

// A QA that did not reach a verdict stops the close: a pass that is held is INCONCLUSIVE, the gate does not pass, the milestone is
// escalated, and the crosscheck does not run.
func TestCloseWithAHeldPassIsInconclusiveAndEscalates(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Relay.Surfaces, l.Relay.Grade = []string{"web"}, "C3"
	l.Sequence("^qa-pass:B:M1$", func(*Call) fakehostPlan { return Fail("serverOverloaded") })
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("%s %s", res.Status, res.Error)
	}
	got := Value[closeResult](t, res)
	if got.State != "escalated" || got.Source != "qa" || got.Reason != "qa_inconclusive" {
		t.Fatalf("%+v; calls %v", got, l.Labels())
	}
	for _, label := range l.Labels() {
		if label == "qa-synthesis:M1" {
			t.Errorf("the call %q ran after a pass that did not complete", label)
		}
	}
	if n := len(l.Rows("crw.crosscheck")); n != 0 {
		t.Errorf("%d crosscheck rows after a pass that did not complete", n)
	}
	if reasons := escalationReasons(l); !slices.Contains(reasons, "qa_inconclusive") {
		t.Errorf("escalations %v", reasons)
	}
	rows := l.Rows("crw.qaReceipt")
	if len(rows) != 1 || str(rows[0].Data, "state") != "INCONCLUSIVE" || str(rows[0].Data, "reason") != "pass_B_held" {
		t.Errorf("receipt rows %+v", rows)
	}
}

// A failed verification of the integrated head fails the gate: the milestone is escalated, the crosscheck does not run and nothing
// is archived. A milestone whose verification cannot run is held, and a resume that finds the verifier back goes on.
func TestCloseGateFailsOnAFailedVerificationAndHoldsOnAMissingVerifier(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Verify.Fail = map[string][]int{"": {1}}
	res := l.Run()
	got := Value[closeResult](t, res)
	if got.State != "escalated" || got.Source != "final_gate" || got.Reason != "test_failed" {
		t.Fatalf("%+v", got)
	}
	if len(l.Rows("crw.crosscheck")) != 0 || len(l.Archived()) != 0 {
		t.Errorf("calls %v, archived %v", l.Labels(), l.Archived())
	}

	l = NewLab(t, oneNode()...)
	l.Verify.Unavailable = map[string]bool{"": true}
	res = l.Run()
	held := Value[closeResult](t, res)
	if held.State != "held" || held.Source != "final_gate" || held.Reason != "verify_unavailable" {
		t.Fatalf("%+v", held)
	}
	holds := l.Rows("crw.hold")
	if len(holds) != 1 || str(holds[0].Data, "reason") != "verify_unavailable" {
		t.Errorf("hold rows %+v", holds)
	}
	before := l.LastSeq()
	l.Verify.Unavailable = nil
	l.NewProcess("proc-2")
	final := Value[closeResult](t, l.Resume())
	if final.State != "closed" {
		t.Fatalf("resume: %+v", final)
	}
	for _, c := range l.LiveCalls(before) {
		if strings.HasPrefix(c.Label, "build:") || strings.HasPrefix(c.Label, "review:") {
			t.Errorf("the resume ran %q again", c.Label)
		}
	}
}

// A crosscheck verdict that does not verify the range keeps the milestone open (CRW-1471, CRW-1473): the crosscheck's pass A finds a
// P1 core_function finding over a passed gate, its V is needs_changes, and the run escalates the milestone crosscheck_pending with ask
// crosscheck_pending (the 자식 hands it to the 부모 with V and the fix packet). Nothing reads it as a close: no crw.milestoneClose, no
// reflection of the architect, no report up, no hold, and the crosscheck is not asked again.
func TestNeedsChangesFinalDoesNotCloseTheMilestone(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Crosschecks("M1", 1, 0, "A", 1, Crosscheck{Verdict: "needs_changes", Findings: []reviewadapter.Finding{Finding("P1", "the range breaks the criterion")}})
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s: %s\ncalls: %v", res.Status, res.ErrorKind, res.Error, l.Labels())
	}
	got := Value[closeResult](t, res)
	if got.State != "escalated" || got.Source != "crosscheck" || got.Reason != "crosscheck_pending" || got.Ask != "crosscheck_pending" {
		t.Fatalf("%+v; calls %v; escalations %v", got, l.Labels(), escalationReasons(l))
	}
	if reasons := escalationReasons(l); !slices.Contains(reasons, "crosscheck_pending") {
		t.Errorf("escalations %v, want crosscheck_pending", reasons)
	}
	cc := l.Rows("crw.crosscheck")
	if len(cc) != 1 || str(cc[0].Data, "state") != "verdict" {
		t.Fatalf("crosscheck rows %+v, want one verdict", cc)
	}
	if v, _ := cc[0].Data["verdict"].(map[string]any); str(v, "verdict") != "needs_changes" {
		t.Errorf("the crosscheck's V %v, want needs_changes", cc[0].Data["verdict"])
	}
	if n := len(l.Rows("crw.milestoneClose")); n != 0 {
		t.Errorf("%d crw.milestoneClose rows: the milestone closed over a crosscheck that needs changes", n)
	}
	if n := len(l.Rows("crw.reportUp")); n != 0 {
		t.Errorf("%d crw.reportUp rows: a milestone that did not close was reported up", n)
	}
	if n := len(l.Rows("crw.hold")); n != 0 {
		t.Errorf("%d crw.hold rows: a verdict is not held", n)
	}
	for _, label := range l.Labels() {
		if label == "reflection:M1" || strings.HasPrefix(label, "reviewer-final:") {
			t.Errorf("the call %s ran over a range the crosscheck did not verify: %v", label, l.Labels())
		}
	}
	if n := len(l.Crosschecker.Calls()); n != 2 {
		t.Errorf("%d crosscheck passes, want the two of one round: a verdict is not asked again", n)
	}
}

// A crosscheck pass that answers needs_changes with no finding does not verify the range (CRW-1473): V is needs_changes though no
// merged finding blocks, so the run escalates the milestone crosscheck_pending and does not close it (no crw.milestoneClose, no report
// up, no reflection).
func TestANeedsChangesPassWithNoFindingDoesNotCloseTheMilestone(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Crosschecks("M1", 1, 0, "B", 1, Crosscheck{Verdict: "needs_changes"})
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s: %s\ncalls: %v", res.Status, res.ErrorKind, res.Error, l.Labels())
	}
	got := Value[closeResult](t, res)
	if got.State != "escalated" || got.Source != "crosscheck" || got.Reason != "crosscheck_pending" || got.Ask != "crosscheck_pending" {
		t.Fatalf("%+v; calls %v; escalations %v", got, l.Labels(), escalationReasons(l))
	}
	cc := l.Rows("crw.crosscheck")
	if len(cc) != 1 || str(cc[0].Data, "state") != "verdict" {
		t.Fatalf("crosscheck rows %+v, want one verdict", cc)
	}
	v, _ := cc[0].Data["verdict"].(map[string]any)
	if findings, _ := v["findings"].([]any); str(v, "verdict") != "needs_changes" || len(findings) != 0 {
		t.Errorf("the crosscheck's V %v, want needs_changes with no finding", cc[0].Data["verdict"])
	}
	if n := len(l.Rows("crw.milestoneClose")); n != 0 {
		t.Errorf("%d crw.milestoneClose rows: the milestone closed over a pass that needs changes", n)
	}
	if n := len(l.Rows("crw.reportUp")); n != 0 {
		t.Errorf("%d crw.reportUp rows: a milestone that did not close was reported up", n)
	}
	for _, label := range l.Labels() {
		if label == "reflection:M1" {
			t.Errorf("the reflection ran over a range the crosscheck did not verify: %v", l.Labels())
		}
	}
}

// The escalation of a crosscheck that needs changes follows the latest crosscheck (CRW-1473): the run escalates crosscheck_pending
// over V1 of the head H1, another actor moves the target to H2, and the resume gates H2 again and its crosscheck needs changes with
// another finding (V2). The escalation the run's value names holds V2 of H2 in its evidence - the same V as the value's crosscheck -
// and is a new question of the milestone (an escalation of its own, the current one); the escalation of H1 keeps V1.
func TestACrosscheckEscalationOnAResumeAtANewHeadHoldsTheNewVerdict(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Crosschecks("M1", 1, 0, "A", 1,
		Crosscheck{Verdict: "needs_changes", Findings: []reviewadapter.Finding{Finding("P1", "the first head breaks the criterion")}},
		Crosscheck{Verdict: "needs_changes", Findings: []reviewadapter.Finding{Finding("P1", "the moved head breaks the criterion")}})
	first := l.Run()
	v1 := Value[map[string]any](t, first)
	h1 := l.Tip()
	if v1["state"] != "escalated" || v1["reason"] != "crosscheck_pending" {
		t.Fatalf("the first run: %s", first.Value)
	}
	moved := l.Git("commit-tree", l.Git("rev-parse", h1+"^{tree}"), "-p", h1, "-m", "another actor moved the target")
	l.Git("update-ref", IntegrationRef, moved)

	l.NewProcess("proc-2")
	second := l.Resume()
	v2 := Value[map[string]any](t, second)
	cc, _ := v2["crosscheck"].(map[string]any)
	if v2["state"] != "escalated" || v2["reason"] != "crosscheck_pending" || cc == nil || cc["head"] != moved {
		t.Fatalf("the resume at the new head %s: %s", moved, second.Value)
	}
	if rows := l.Rows("crw.crosscheck"); len(rows) < 2 {
		t.Fatalf("crosscheck rows %d, want the one of H1 and the one of H2", len(rows))
	}
	byID := map[string]map[string]any{}
	var order []string
	for _, ev := range l.FlowRows(engine.EvEscalation) {
		if str(ev.Data, "ask") != "crosscheck_pending" {
			continue
		}
		id := str(ev.Data, "escalation_id")
		if byID[id] != nil {
			t.Errorf("two escalation rows of the id %s", id)
		}
		byID[id] = ev.Data
		order = append(order, id)
	}
	row := byID[str(v2, "escalation")]
	if row == nil {
		t.Fatalf("no escalation row of the value's escalation %v; rows %v", v2["escalation"], order)
	}
	evidence, _ := row["evidence"].(map[string]any)
	if !jsonSame(t, evidence["crosscheck"], cc) {
		t.Errorf("the escalation's crosscheck is not the value's V2:\n%v\n%v", evidence["crosscheck"], cc)
	}
	if len(order) != 2 || order[1] != str(v2, "escalation") || order[0] != str(v1, "escalation") {
		t.Fatalf("crosscheck_pending escalations %v, want the one of H1 (%v) then the one of H2 (%v)", order, v1["escalation"], v2["escalation"])
	}
	old, _ := byID[order[0]]["evidence"].(map[string]any)
	if oc, _ := old["crosscheck"].(map[string]any); oc == nil || oc["head"] != h1 {
		t.Errorf("the escalation of H1 holds %v, want V1 of %s", old["crosscheck"], h1)
	}
}

// jsonSame says whether a and b are the same JSON value.
func jsonSame(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(x) == string(y)
}

// A resume of a run that closed does nothing: the gate, the crosscheck, the close, the audit and the archive are all replayed
// from the journal, and the host is asked for nothing - no call, no read of a thread, no archive.
func TestResumeAfterAClosedMilestoneRunsNothing(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Relay.Surfaces, l.Relay.Grade = []string{"web"}, "C2"
	if got := closeOf(t, l); got.State != "closed" || got.Archived != 0 || got.Audit.OK != 1 {
		t.Fatalf("%+v", got)
	}
	archivedBefore := len(l.Archived())
	before := l.LastSeq()
	listed := len(l.Requests("thread/turns/list"))
	l.NewProcess("proc-2")
	if got := Value[closeResult](t, l.Resume()); got.State != "closed" {
		t.Fatalf("resume: %+v", got)
	}
	var live []string
	for _, ev := range rowsAfter(l, before) {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted && strings.HasPrefix(ev.Op, "crw.") && ev.Op != "crw.classify" {
			live = append(live, ev.Op)
		}
	}
	if len(live) != 0 {
		t.Errorf("live operations %v", live)
	}
	if turns := turnsSince(l, before); len(turns) != 0 {
		t.Errorf("live agent turns %+v", turns)
	}
	if n := len(l.Requests("thread/turns/list")); n != listed || len(l.Archived()) != archivedBefore {
		t.Errorf("the resume read %d more turns lists and archived %d more threads", n-listed, len(l.Archived())-archivedBefore)
	}
}

// `crw flow audit` over the store of a run that closed: it opens the store the run's journal is in, finds every acceptance ok, and
// leaves its result in the journal (a row of kind crw.audit with its own call id). An acceptance given alone is found in the store.
// Every acceptance rests on a unit check (CRW-1465), which the audit vouches for on its row: no reviewer thread is read, so a host
// that cannot be reached changes nothing, and the same command finds every acceptance ok and exits 0.
func TestFlowAuditCommandAuditsARunFromItsStore(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	if got := closeOf(t, l); got.State != "closed" || got.Audit.OK != 2 {
		t.Fatalf("%+v", got)
	}
	prev := flowcli.AuditEnv
	flowcli.AuditEnv = flowcli.OpenAuditStore
	t.Cleanup(func() { flowcli.AuditEnv = prev })
	audit_ := func(args ...string) (int, map[string]any, string) {
		// the command opens the store as a process of its own: the run's process is over, and so is its hold on the store
		_ = l.Store.Close()
		defer func() { l.Store = l.openStore() }()
		var out, errOut bytes.Buffer
		code := flowcli.RunAudit(context.Background(), append([]string{"audit", "--state", l.Root, "--json"}, args...), &out, &errOut)
		var rep map[string]any
		_ = json.Unmarshal(out.Bytes(), &rep)
		return code, rep, errOut.String()
	}
	auditRows := func() int {
		var n int
		if err := l.Store.DB.QueryRow(`SELECT COUNT(*) FROM flow_calls WHERE run_id = ? AND kind = 'crw.audit' AND event = 'completed'`, l.RunID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := auditRows()
	code, rep, errOut := audit_("--run", l.RunID, "--socket", l.Host.SocketPath)
	if code != 0 || errOut != "" || rep["recorded"] != true {
		t.Fatalf("exit %d: %v %s", code, rep, errOut)
	}
	results, _ := rep["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("%v", rep)
	}
	for _, r := range results {
		if r.(map[string]any)["state"] != "ok" {
			t.Errorf("result %v", r)
		}
	}
	if after := auditRows(); after != before+1 {
		t.Errorf("%d audit rows, want one more than %d", after, before)
	}
	var callID string
	if err := l.Store.DB.QueryRow(`SELECT call_id FROM flow_calls WHERE run_id = ? AND kind = 'crw.audit' ORDER BY rowid DESC LIMIT 1`, l.RunID).Scan(&callID); err != nil || !strings.HasPrefix(callID, "audit_") {
		t.Errorf("the row of the command: %q %v", callID, err)
	}

	// one acceptance, named without its run
	acceptance := str(l.Rows("crw.accept")[0].Data, "acceptance_id")
	code, rep, errOut = audit_("--acceptance", acceptance, "--socket", l.Host.SocketPath, "--no-record")
	if results, _ = rep["results"].([]any); code != 0 || len(results) != 1 || rep["recorded"] != false || rep["run"] != l.RunID {
		t.Fatalf("exit %d: %v %s", code, rep, errOut)
	}

	// the host cannot be reached: nobody listens on its socket any more, and the audit of a unit-check acceptance reads nothing there
	l.Host.Close()
	code, rep, _ = audit_("--run", l.RunID, "--socket", l.Host.SocketPath, "--no-record")
	summary, _ := rep["summary"].(map[string]any)
	if code != 0 || summary["unobserved"] != float64(0) || summary["ok"] != float64(2) {
		t.Errorf("an unreachable host: exit %d, %v", code, rep)
	}
}

// The process of a run ends right after the QA call captured, before its self-review: the resume does not capture again, the call
// that gives the receipt continues the QA call (on a fork of its thread, since the thread belongs to the process that ended), and the
// close goes on to a receipt and a closed milestone.
func TestResumeAfterTheQACaptureContinuesTheQACall(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Relay.Surfaces, l.Relay.Grade = []string{"web"}, "C2"
	killed := false
	l.OnRow(func(ev engine.Event) {
		if !killed && ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Event == engine.EvCompleted && ev.Label == "qa:M1" {
			killed = true
			l.Kill()
		}
	})
	l.Run()
	if !killed {
		t.Fatal("the run never reached the QA call")
	}
	if rows := l.Rows("crw.qaReceipt"); len(rows) != 0 {
		t.Fatalf("a receipt before the resume: %+v", rows)
	}
	capture := callNamed(t, l, "qa:M1")
	before := l.LastSeq()
	l.OnRow(nil)
	l.NewProcess("proc-2")
	got := Value[closeResult](t, l.Resume())
	if got.State != "closed" || got.Gate != "pass" {
		t.Fatalf("resume: %+v; calls %v", got, l.Labels())
	}
	for _, c := range l.LiveCalls(before) {
		if c.Label == "qa:M1" || strings.HasPrefix(c.Label, "build:") || strings.HasPrefix(c.Label, "review:") {
			t.Errorf("the resume ran %q again", c.Label)
		}
	}
	review := callNamed(t, l, "qa-review:M1")
	if review.Resume != capture.ID {
		t.Errorf("the self-review continues %q, want the QA call %s", review.Resume, capture.ID)
	}
	rows := l.Rows("crw.qaReceipt")
	if len(rows) != 1 || str(rows[0].Data, "state") != "PASS" {
		t.Errorf("receipt rows %+v", rows)
	}
	// the QA continuation and the reflection of the architect, whose thread the ended process started
	if forks := l.Requests("thread/fork"); len(forks) != 2 {
		t.Errorf("%d thread/fork requests: the continuation of a thread of the ended process runs on a fork", len(forks))
	}
}

// relativeCapture is a capture answer whose artifact is named as the crw-qa skill names it: relative to the worktree of the capture.
func relativeCapture() map[string]any {
	return map[string]any{"captures": []any{map[string]any{"path": ".crw/evidence/s1/qa/home/shot.png", "kind": "screenshot", "shows": "the home screen"}},
		"matrix": []string{"home"}, "teardown": []string{"no resources spawned"}, "summary": "drove the surface"}
}

// The artifacts of a capture are files of the capture call's own scratch worktree; the passes and the self-review read checkouts of
// their own. The packets they get name each artifact by the path it can be opened at - the capture's worktree and the relative path -
// and the file is there (CRW-1206 correction, d3). The C2 self-review that runs after a resume, on a fork in another checkout,
// gets the same path.
func TestQAArtifactsAreHandedOnByThePathTheyCanBeOpenedAt(t *testing.T) {
	for name, c := range map[string]struct {
		grade  string
		resume bool
		labels []string
	}{
		"C3 passes":                {"C3", false, []string{"qa-pass:A:M1", "qa-pass:B:M1"}},
		"C2 self-review":           {"C2", false, []string{"qa-review:M1"}},
		"C2 self-review on resume": {"C2", true, []string{"qa-review:M1"}},
	} {
		t.Run(name, func(t *testing.T) {
			l := NewLab(t, oneNode()...)
			l.Relay.Surfaces, l.Relay.Grade = []string{"web"}, c.grade
			l.On(`^qa:M1$`, func(call *Call) *fakehostPlan {
				call.EditPath(".crw/evidence/s1/qa/home/shot.png", "png")
				p := Say(relativeCapture())
				return &p
			})
			var mu sync.Mutex
			prompts := map[string]string{}
			l.On(`^qa-(pass|review)`, func(call *Call) *fakehostPlan {
				mu.Lock()
				prompts[call.Label] = call.Text
				mu.Unlock()
				return nil
			})
			if c.resume {
				killed := false
				l.OnRow(func(ev engine.Event) {
					if !killed && ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Event == engine.EvCompleted && ev.Label == "qa:M1" {
						killed = true
						l.Kill()
					}
				})
				l.Run()
				if !killed {
					t.Fatal("the run never reached the QA call")
				}
				l.OnRow(nil)
				l.NewProcess("proc-2")
			}
			var got closeResult
			if c.resume {
				got = Value[closeResult](t, l.Resume())
			} else {
				got = closeOf(t, l)
			}
			if got.State != "closed" {
				t.Fatalf("%+v; calls %v; escalations %v", got, l.Labels(), escalationReasons(l))
			}
			alloc, _ := l.Prepared(callNamed(t, l, "qa:M1").ID).Data["allocation"].(map[string]any)
			root := str(alloc, "path")
			if root == "" {
				t.Fatalf("the QA call has no worktree: %v", alloc)
			}
			want := root + "/.crw/evidence/s1/qa/home/shot.png"
			if _, err := os.Stat(want); err != nil {
				t.Fatalf("the artifact is not where the capture left it: %v", err)
			}
			for _, label := range c.labels {
				if !strings.Contains(prompts[label], "screenshot "+want+": the home screen") {
					t.Errorf("the packet of %s does not name the artifact by %s:\n%s", label, want, prompts[label])
				}
			}
			if len(prompts) != len(c.labels) {
				t.Errorf("packets of %v, want %v", prompts, c.labels)
			}
		})
	}
}

// A capture call that edits the tracked code of its scratch worktree to make the surface work exercised another tree than the head
// the gate pinned: its receipt is INCONCLUSIVE, no pass is asked for it, the milestone is escalated and the crosscheck does not run
// (CRW-1206 correction, d2). The artifacts under .crw/ are not an edit.
func TestQAOverEditedSourceIsInconclusiveAndEscalates(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Relay.Surfaces, l.Relay.Grade = []string{"web"}, "C3"
	l.On(`^qa:M1$`, func(call *Call) *fakehostPlan {
		call.EditPath(".crw/evidence/s1/qa/home/shot.png", "png")
		call.EditPath("README.md", "patched so the page renders\n")
		p := Say(relativeCapture())
		return &p
	})
	got := closeOf(t, l)
	if got.State != "escalated" || got.Source != "qa" || got.Reason != "qa_inconclusive" {
		t.Fatalf("%+v; calls %v", got, l.Labels())
	}
	for _, label := range l.Labels() {
		if strings.HasPrefix(label, "qa-pass:") || label == "qa-synthesis:M1" {
			t.Errorf("the call %q ran over a capture of an edited tree", label)
		}
	}
	if n := len(l.Rows("crw.crosscheck")); n != 0 {
		t.Errorf("%d crosscheck rows over a capture of an edited tree", n)
	}
	rows := l.Rows("crw.qaReceipt")
	if len(rows) != 1 || str(rows[0].Data, "state") != "INCONCLUSIVE" || str(rows[0].Data, "reason") != "capture_source_modified" {
		t.Errorf("receipt rows %+v", rows)
	}
	if reasons := escalationReasons(l); !slices.Contains(reasons, "qa_inconclusive") {
		t.Errorf("escalations %v", reasons)
	}
	// the artifacts alone are not an edit: the same capture without the patch passes
	l2 := NewLab(t, oneNode()...)
	l2.Relay.Surfaces, l2.Relay.Grade = []string{"web"}, "C3"
	l2.On(`^qa:M1$`, func(call *Call) *fakehostPlan {
		call.EditPath(".crw/evidence/s1/qa/home/shot.png", "png")
		p := Say(relativeCapture())
		return &p
	})
	if got := closeOf(t, l2); got.State != "closed" {
		t.Errorf("a capture that left artifacts only: %+v; calls %v", got, l2.Labels())
	}
}
