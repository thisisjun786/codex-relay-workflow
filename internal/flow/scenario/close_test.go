package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	Ask   string `json:"ask"`
	Audit struct {
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
	if got.Audit.OK != 2 || got.Audit.Mismatch != 0 || got.Audit.Unobserved != 0 {
		t.Fatalf("audit %+v", got.Audit)
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
	if got.State != "closed" || got.Audit.OK != 1 || got.Audit.Unobserved != 1 || got.Audit.Mismatch != 0 {
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
	if got.State != "closed" || got.Audit.Mismatch != 1 || got.Audit.OK != 0 || len(l.Archived()) != 0 {
		t.Fatalf("%+v; archived %v", got, l.Archived())
	}
	if reasons := escalationReasons(l); !slices.Contains(reasons, "audit_mismatch") {
		t.Errorf("escalations %v", reasons)
	}
}

// TestCloseAuditsAnUpperCaseUnitCheckAcceptanceWithoutAMismatch (CRW-1480 item 4): crw.accept takes a unit check whose sha is written in
// upper case (a sha names the same object in either case), and the close's audit of that acceptance reads the check the same way: the
// node's check names the tree of the head in upper case, the head is accepted on it, and the close audits the acceptance ok, with no
// mismatch and no audit_mismatch escalation.
func TestCloseAuditsAnUpperCaseUnitCheckAcceptanceWithoutAMismatch(t *testing.T) {
	l := NewLab(t, oneNode()...)
	upper := 0
	l.Rewrite(func(ev engine.Event) engine.Event {
		if ev.Kind == engine.KindCall && ev.Op == ops.OpUnitCheck && ev.Event == engine.EvCompleted && str(ev.Data, "node") == "A" {
			d := map[string]any{}
			for k, v := range ev.Data {
				d[k] = v
			}
			for _, key := range []string{"commit", "tree"} {
				if sha := str(d, key); sha != "" {
					d[key] = strings.ToUpper(sha)
					upper++
				}
			}
			ev.Data = d
		}
		return ev
	})
	got := closeOf(t, l)
	if upper == 0 {
		t.Fatal("the unit check of A names no commit or tree to write in upper case")
	}
	if got.State != "closed" || got.Audit.OK != 1 || got.Audit.Mismatch != 0 {
		t.Fatalf("the close of an acceptance on an upper-case unit check: %+v", got)
	}
	if reasons := escalationReasons(l); slices.Contains(reasons, "audit_mismatch") {
		t.Errorf("escalations %v: an upper-case sha of the check is no mismatch", reasons)
	}
}

// TestCloseAuditsTheAcceptancesAndArchivesNothing (CRW-1447): the close audits the acceptances of the milestone and archives nothing. A
// close of a unit-check run writes one crw.audit row before its report and no crw.archive row, and the milestone's result holds the
// audit and no archive member; an audit mismatch still tells the orchestrator (audit_mismatch), and nothing is archived either.
func TestCloseAuditsTheAcceptancesAndArchivesNothing(t *testing.T) {
	audited := func(t *testing.T, l *Lab) map[string]any {
		t.Helper()
		res := l.Run()
		if res.Status != engine.RunCompleted {
			t.Fatalf("run %s: %s: %s\ncalls: %v", res.Status, res.ErrorKind, res.Error, l.Labels())
		}
		v := Value[map[string]any](t, res)
		if v["state"] != "closed" {
			t.Fatalf("the milestone %v", v)
		}
		if _, ok := v["archived"]; ok {
			t.Errorf("the milestone's result has an archive member: %v", v)
		}
		if _, ok := v["audit"].(map[string]any); !ok {
			t.Errorf("the milestone's result has no audit: %v", v)
		}
		if rows := l.Rows(audit.Op); len(rows) != 1 {
			t.Errorf("%d crw.audit rows, want one", len(rows))
		}
		if rows := l.Rows(ops.OpArchive); len(rows) != 0 {
			t.Errorf("%d crw.archive rows, want none: the close archives nothing", len(rows))
		}
		if archived := l.Archived(); len(archived) != 0 {
			t.Errorf("archived %v, want nothing", archived)
		}
		var auditAt, reportAt int = -1, -1
		for i, ev := range l.Events() {
			if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted && ev.Op == audit.Op {
				auditAt = i
			}
			if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted && ev.Op == ops.OpReportUp {
				reportAt = i
			}
		}
		if auditAt < 0 || reportAt < auditAt {
			t.Errorf("the audit row at %d, the report row at %d: the audit comes before the report", auditAt, reportAt)
		}
		return v
	}
	t.Run("a close", func(t *testing.T) {
		l := NewLab(t, twoNodes()...)
		v := audited(t, l)
		if a := v["audit"].(map[string]any); a["ok"] != float64(2) || a["mismatch"] != float64(0) {
			t.Errorf("the audit %v", a)
		}
		if reasons := escalationReasons(l); slices.Contains(reasons, "audit_mismatch") {
			t.Errorf("escalations %v", reasons)
		}
	})
	t.Run("an audit mismatch", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
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
		v := audited(t, l)
		if a := v["audit"].(map[string]any); a["mismatch"] != float64(1) {
			t.Errorf("the audit %v", a)
		}
		if reasons := escalationReasons(l); !slices.Contains(reasons, "audit_mismatch") {
			t.Errorf("escalations %v", reasons)
		}
	})
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

// A crosscheck verdict that does not verify the range keeps the milestone open (CRW-1471, CRW-1473, CRW-1490): the crosscheck's pass A
// finds a P1 core_function finding over a passed gate, its V is needs_changes, the close's one repair commits over the failed head and
// the second round needs changes again, and the run escalates the milestone crosscheck_pending with ask crosscheck_pending (the 자식
// hands it to the 부모 with V and the fix packet). Nothing reads it as a close: no crw.milestoneClose, no reflection of the architect,
// no report up, no hold, and the unpublished repair moves no ref.
func TestNeedsChangesFinalDoesNotCloseTheMilestone(t *testing.T) {
	l := NewLab(t, oneNode()...)
	blocker := Crosscheck{Verdict: "needs_changes", Findings: []reviewadapter.Finding{Finding("P1", "the range breaks the criterion")}}
	l.Crosschecks("M1", 1, 0, "A", 1, blocker)
	l.Crosschecks("M1", 2, 0, "A", 1, blocker)
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
	if len(cc) != 2 || str(cc[0].Data, "state") != "verdict" || str(cc[1].Data, "state") != "verdict" {
		t.Fatalf("crosscheck rows %+v, want the verdicts of the two rounds", cc)
	}
	for k, row := range cc {
		if v, _ := row.Data["verdict"].(map[string]any); str(v, "verdict") != "needs_changes" {
			t.Errorf("round %d's V %v, want needs_changes", k+1, row.Data["verdict"])
		}
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
	fixes := 0
	for _, label := range l.Labels() {
		if label == "reflection:M1" || strings.HasPrefix(label, "reviewer-final:") {
			t.Errorf("the call %s ran over a range the crosscheck did not verify: %v", label, l.Labels())
		}
		if label == "close-fix:M1" {
			fixes++
		}
	}
	if fixes != 1 {
		t.Fatalf("%d close-fix calls, want the one of the first round's repair: %v", fixes, l.Labels())
	}
	repairs := closeRepairCommits(l)
	if len(repairs) != 1 {
		t.Fatalf("repair commits %v, want the one the fix made", repairs)
	}
	if str(cc[0].Data, "head") != str(repairs[0].Data, "parent") || str(cc[1].Data, "head") != str(repairs[0].Data, "commit") {
		t.Errorf("round 1 judged %s and round 2 %s, want the failed head and its repair", str(cc[0].Data, "head"), str(cc[1].Data, "head"))
	}
	if l.Tip() != str(repairs[0].Data, "parent") {
		t.Fatalf("tip %q: the failed candidate's repair must not publish without a verifying round", l.Tip())
	}
	if len(l.Relay.Repairs) != 0 {
		t.Errorf("the relay published %d repairs for a close that never verified", len(l.Relay.Repairs))
	}
	if n := len(l.Crosschecker.Calls()); n != 4 {
		t.Errorf("%d crosscheck passes, want the two of each of the two rounds", n)
	}
}

// A crosscheck pass that answers needs_changes with no finding does not verify the range (CRW-1473, CRW-1490): V is needs_changes
// though no merged finding blocks, so after the close's one repair and a second round that still needs changes the run escalates the
// milestone crosscheck_pending and does not close it (no crw.milestoneClose, no report up, no reflection).
func TestANeedsChangesPassWithNoFindingDoesNotCloseTheMilestone(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Crosschecks("M1", 1, 0, "B", 1, Crosscheck{Verdict: "needs_changes"})
	l.Crosschecks("M1", 2, 0, "B", 1, Crosscheck{Verdict: "needs_changes"})
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s: %s\ncalls: %v", res.Status, res.ErrorKind, res.Error, l.Labels())
	}
	got := Value[closeResult](t, res)
	if got.State != "escalated" || got.Source != "crosscheck" || got.Reason != "crosscheck_pending" || got.Ask != "crosscheck_pending" {
		t.Fatalf("%+v; calls %v; escalations %v", got, l.Labels(), escalationReasons(l))
	}
	cc := l.Rows("crw.crosscheck")
	if len(cc) != 2 || str(cc[0].Data, "state") != "verdict" || str(cc[1].Data, "state") != "verdict" {
		t.Fatalf("crosscheck rows %+v, want the verdicts of the two rounds", cc)
	}
	for k, row := range cc {
		if fmt.Sprint(row.Data["round"]) != fmt.Sprint(k+1) {
			t.Errorf("row %d is round %v, want %d", k, row.Data["round"], k+1)
		}
		v, _ := row.Data["verdict"].(map[string]any)
		if findings, _ := v["findings"].([]any); str(v, "verdict") != "needs_changes" || len(findings) != 0 {
			t.Errorf("round %d's V %v, want needs_changes with no finding", k+1, row.Data["verdict"])
		}
	}
	if n := len(l.Rows("crw.milestoneClose")); n != 0 {
		t.Errorf("%d crw.milestoneClose rows: the milestone closed over a pass that needs changes", n)
	}
	if n := len(l.Rows("crw.reportUp")); n != 0 {
		t.Errorf("%d crw.reportUp rows: a milestone that did not close was reported up", n)
	}
	fixes := 0
	for _, label := range l.Labels() {
		if label == "reflection:M1" {
			t.Errorf("the reflection ran over a range the crosscheck did not verify: %v", l.Labels())
		}
		if label == "close-fix:M1" {
			fixes++
		}
	}
	if fixes != 1 {
		t.Fatalf("%d close-fix calls, want the one of the first round's repair: %v", fixes, l.Labels())
	}
	repairs := closeRepairCommits(l)
	if len(repairs) != 1 || l.Tip() != str(repairs[0].Data, "parent") {
		t.Errorf("repair commits %v over tip %s, want the one unpublished fix", repairs, l.Tip())
	}
	if len(repairs) == 1 && (str(cc[0].Data, "head") != str(repairs[0].Data, "parent") || str(cc[1].Data, "head") != str(repairs[0].Data, "commit")) {
		t.Errorf("round 1 judged %s and round 2 %s, want the failed head and its repair", str(cc[0].Data, "head"), str(cc[1].Data, "head"))
	}
	if len(l.Relay.Repairs) != 0 {
		t.Errorf("the relay published %d repairs for a close that never verified", len(l.Relay.Repairs))
	}
	if n := len(l.Crosschecker.Calls()); n != 4 {
		t.Errorf("%d crosscheck passes, want the two of each of the two rounds", n)
	}
}

// The escalation of a crosscheck that needs changes follows the latest adjudicated head (CRW-1473, CRW-1490): the run escalates
// crosscheck_pending over the verdict of the repair committed on H1, another actor moves the target to H2, and the resume gates H2 and
// its repair's round-2 recheck needs changes again. The escalation the run's value names holds that latest verdict - of the candidate
// over the moved head - and is a new question of the milestone (an escalation of its own, the current one); the escalation of the first
// run keeps its own verdict.
func TestACrosscheckEscalationOnAResumeAtANewHeadHoldsTheNewVerdict(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Crosschecks("M1", 1, 0, "A", 1,
		Crosscheck{Verdict: "needs_changes", Findings: []reviewadapter.Finding{Finding("P1", "the first head breaks the criterion")}},
		Crosscheck{Verdict: "needs_changes", Findings: []reviewadapter.Finding{Finding("P1", "the moved head breaks the criterion")}})
	l.Crosschecks("M1", 2, 0, "A", 1,
		Crosscheck{Verdict: "needs_changes", Findings: []reviewadapter.Finding{Finding("P1", "the first repair still fails")}},
		Crosscheck{Verdict: "needs_changes", Findings: []reviewadapter.Finding{Finding("P1", "the moved repair still fails")}})
	first := l.Run()
	v1 := Value[map[string]any](t, first)
	h1 := l.Tip()
	if v1["state"] != "escalated" || v1["reason"] != "crosscheck_pending" {
		t.Fatalf("the first run: %s", first.Value)
	}
	if repairs := closeRepairCommits(l); len(repairs) != 1 || str(repairs[0].Data, "parent") != h1 {
		t.Fatalf("repair commits of the first run %v, want the one over %s", repairs, h1)
	}
	repair1 := str(closeRepairCommits(l)[0].Data, "commit")
	moved := l.Git("commit-tree", l.Git("rev-parse", h1+"^{tree}"), "-p", h1, "-m", "another actor moved the target")
	l.Git("update-ref", IntegrationRef, moved)

	l.NewProcess("proc-2")
	second := l.Resume()
	v2 := Value[map[string]any](t, second)
	cc2, _ := v2["crosscheck"].(map[string]any)
	if v2["state"] != "escalated" || v2["reason"] != "crosscheck_pending" || cc2 == nil {
		t.Fatalf("the resume at the new head %s: %s", moved, second.Value)
	}
	repairs := closeRepairCommits(l)
	if len(repairs) != 2 || str(repairs[1].Data, "parent") != moved {
		t.Fatalf("repair commits %+v, want second parent %s", repairs, moved)
	}
	repair2 := str(repairs[1].Data, "commit")
	cc1, _ := v1["crosscheck"].(map[string]any)
	if cc1 == nil || str(cc1, "head") != repair1 || str(cc1, "prior_head") != h1 {
		t.Errorf("the first escalation's V %v, want the verdict of the first repair %s over %s", v1["crosscheck"], repair1, h1)
	}
	if str(cc2, "head") != repair2 || str(cc2, "prior_head") != moved {
		t.Errorf("the resume's V %v, want the verdict of the repair %s at the moved head %s", v2["crosscheck"], repair2, moved)
	}
	if l.Tip() != moved {
		t.Errorf("tip %q, want %s: a repair that never verified is not published", l.Tip(), moved)
	}
	if rows := l.Rows("crw.crosscheck"); len(rows) != 4 {
		t.Fatalf("crosscheck rows %d, want the two rounds of each of the two runs", len(rows))
	}
	for _, label := range l.Labels() {
		if label == "reflection:M1" || strings.HasPrefix(label, "reviewer-final:") {
			t.Errorf("the call %s ran over a range the crosscheck did not verify: %v", label, l.Labels())
		}
	}
	if n := len(l.Rows("crw.milestoneClose")); n != 0 {
		t.Errorf("%d crw.milestoneClose rows", n)
	}
	if n := len(l.Rows("crw.reportUp")); n != 0 {
		t.Errorf("%d crw.reportUp rows", n)
	}
	if n := len(l.Crosschecker.Calls()); n != 8 {
		t.Errorf("%d crosscheck passes, want the two of each round of each run", n)
	}
	fixes := 0
	for _, label := range l.Labels() {
		if label == "close-fix:M1" {
			fixes++
		}
	}
	if fixes != 2 {
		t.Errorf("%d close-fix calls, want the one repair of each run", fixes)
	}
	if len(l.Relay.Repairs) != 0 {
		t.Errorf("the relay published %d repairs over a moved head that never verified", len(l.Relay.Repairs))
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
	if !jsonSame(t, evidence["crosscheck"], cc2) {
		t.Errorf("the escalation's crosscheck is not the value's V2:\n%v\n%v", evidence["crosscheck"], cc2)
	}
	if len(order) != 2 || order[1] != str(v2, "escalation") || order[0] != str(v1, "escalation") {
		t.Fatalf("crosscheck_pending escalations %v, want the one of H1 (%v) then the one of H2 (%v)", order, v1["escalation"], v2["escalation"])
	}
	old, _ := byID[order[0]]["evidence"].(map[string]any)
	if oc, _ := old["crosscheck"].(map[string]any); oc == nil || oc["head"] != repair1 {
		t.Errorf("the escalation of the first run holds %v, want its repair's V %s", old["crosscheck"], repair1)
	}
}

// A crosscheck verdict that needs changes gets one in-run repair, and a round two that verifies
// it closes the milestone (CRW-1490): the repair commits over the failed gate's head, the close
// publishes that candidate with the repair integrator, and the tip becomes it.
func TestNeedsChangesFinalRepairClosesTheMilestone(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Crosschecks("M1", 1, 0, "A", 1, Crosscheck{Verdict: "needs_changes", Findings: []reviewadapter.Finding{Finding("P1", "the range breaks the criterion")}})
	got := closeOf(t, l)
	if got.State != "closed" {
		t.Fatalf("%+v; calls %v", got, l.Labels())
	}
	if got.Crosscheck != "verified" {
		t.Fatalf("the close's crosscheck %v, want verified", got.Crosscheck)
	}
	cc := l.Rows("crw.crosscheck")
	if len(cc) != 2 || str(cc[0].Data, "state") != "verdict" || str(cc[1].Data, "state") != "verdict" {
		t.Fatalf("crosscheck rows %+v, want the verdicts of round 1 and round 2", cc)
	}
	for k, row := range cc {
		if fmt.Sprint(row.Data["round"]) != fmt.Sprint(k+1) {
			t.Errorf("row %d is round %v, want %d", k, row.Data["round"], k+1)
		}
	}
	if v, _ := cc[0].Data["verdict"].(map[string]any); str(v, "verdict") != "needs_changes" {
		t.Errorf("round 1 %v, want needs_changes", v)
	}
	if v, _ := cc[1].Data["verdict"].(map[string]any); str(v, "verdict") != "verified" {
		t.Errorf("round 2 %v, want verified", v)
	}
	if n := len(l.Crosschecker.Calls()); n != 4 {
		t.Errorf("%d crosscheck passes, want the two of each of the two rounds", n)
	}
	var fixes []string
	for _, label := range l.Labels() {
		if label == "close-fix:M1" {
			fixes = append(fixes, label)
		}
	}
	if len(fixes) != 1 {
		t.Fatalf("%d close-fix calls, want the one of the repair: %v", len(fixes), l.Labels())
	}
	repairs := closeRepairCommits(l)
	if len(repairs) != 1 {
		t.Fatalf("repair commits %v, want the one of the fix", repairs)
	}
	repaired, first := str(repairs[0].Data, "commit"), str(repairs[0].Data, "parent")
	if l.Tip() != repaired || got.Tip != repaired {
		t.Fatalf("tip %q / reported %q, want the published repair %q", l.Tip(), got.Tip, repaired)
	}
	if repaired == first {
		t.Fatalf("the repair %q is the failed head, not a commit over it", repaired)
	}
	if got := l.Git("merge-base", first, repaired); got != first {
		t.Fatalf("the repair %q is not over the failed head %q (merge-base %s)", repaired, first, got)
	}
	if str(cc[0].Data, "head") != first || str(cc[1].Data, "head") != repaired {
		t.Errorf("the verdicts name %q then %q, want the failed head then the repair", str(cc[0].Data, "head"), str(cc[1].Data, "head"))
	}
	if n := len(l.Rows("crw.milestoneClose")); n != 1 {
		t.Errorf("%d crw.milestoneClose rows", n)
	}
	if n := len(l.Rows("crw.reportUp")); n != 1 {
		t.Errorf("%d crw.reportUp rows", n)
	}
	if len(l.Relay.Repairs) != 1 {
		t.Fatalf("repairs %v, want the one publication of the repair", l.Relay.Repairs)
	}
	req := l.Relay.Repairs[0]
	if req.Base != first || req.Head != repaired || req.VerificationDigest == "" {
		t.Errorf("repair request %+v, want base %s head %s and a verification digest", req, first, repaired)
	}
	if reasons := escalationReasons(l); slices.Contains(reasons, "crosscheck_pending") {
		t.Errorf("a verified repair escalated: %v", reasons)
	}
}

// closeRepairCommits is the crw.commit rows the close's repair executor made for milestone M1.
func closeRepairCommits(l *Lab) []engine.Event {
	var out []engine.Event
	for _, ev := range l.Rows("crw.commit") {
		if str(ev.Data, "node") == "milestone:M1" {
			out = append(out, ev)
		}
	}
	return out
}

// A repair of a moved target publishes over the moved head (CRW-1490): the resume's close-fix
// executor commits its repair over the new head the gate judged, the round-2 recheck verifies
// it, and the relay's repair publication moves the target to it. The other actor's landing is
// kept under it.
func TestCloseRepairAfterMovedTargetCanClose(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Crosschecks("M1", 1, 0, "A", 1,
		Crosscheck{Verdict: "needs_changes", Findings: []reviewadapter.Finding{Finding("P1", "the head breaks the criterion")}},
		Crosscheck{Verdict: "needs_changes", Findings: []reviewadapter.Finding{Finding("P1", "the moved head breaks the criterion")}})
	l.Crosschecks("M1", 2, 0, "A", 1, Crosscheck{Verdict: "needs_changes", Findings: []reviewadapter.Finding{Finding("P1", "the first repair still fails")}})
	first := l.Run()
	v1 := Value[map[string]any](t, first)
	if v1["state"] != "escalated" || v1["reason"] != "crosscheck_pending" {
		t.Fatalf("the first run: %s", first.Value)
	}
	if repairs := closeRepairCommits(l); len(repairs) != 1 {
		t.Fatalf("repair commits of the first run %v, want the one unpublished fix", repairs)
	}
	firstTip := l.Tip()
	moved := l.AdvanceIntegration("outside.txt")

	l.NewProcess("proc-2")
	second := l.Resume()
	got := Value[closeResult](t, second)
	if got.State != "closed" {
		t.Fatalf("the resume %s: %+v; calls %v", got.State, got, l.Labels())
	}
	if got.Crosscheck != "verified" {
		t.Fatalf("the close's crosscheck %v, want verified", got.Crosscheck)
	}
	repairs := closeRepairCommits(l)
	if len(repairs) != 2 {
		t.Fatalf("repair commits %v, want the first run's and the resume's", repairs)
	}
	secondCommit := str(repairs[1].Data, "commit")
	if str(repairs[1].Data, "parent") != moved {
		t.Fatalf("the resume's repair is over %s, want the moved head %s", str(repairs[1].Data, "parent"), moved)
	}
	if got.Tip != secondCommit || l.Tip() != secondCommit {
		t.Fatalf("tip %q / reported %q, want the published repair %s", l.Tip(), got.Tip, secondCommit)
	}
	cc := l.Rows("crw.crosscheck")
	if len(cc) != 4 {
		t.Fatalf("crosscheck rows %d, want the two rounds of each run", len(cc))
	}
	last, _ := cc[3].Data["verdict"].(map[string]any)
	if str(last, "verdict") != "verified" || str(last, "prior_head") != moved || str(last, "head") != secondCommit {
		t.Errorf("the last verdict %v, want verified of the repair over %s", last, moved)
	}
	if got := l.Git("show", got.Tip+":outside.txt"); got != "outside landing of outside.txt" {
		t.Errorf("the published tip lost the other actor's landing: %q", got)
	}
	if n := len(l.Crosschecker.Calls()); n != 8 {
		t.Errorf("%d crosscheck passes, want the two of each round of each run", n)
	}
	fixes := 0
	for _, label := range l.Labels() {
		if label == "close-fix:M1" {
			fixes++
		}
	}
	if fixes != 2 {
		t.Errorf("%d close-fix calls, want one per run", fixes)
	}
	if len(l.Relay.Repairs) != 1 {
		t.Fatalf("repairs %v, want the one publication", l.Relay.Repairs)
	}
	if req := l.Relay.Repairs[0]; req.Base != moved || req.Head != secondCommit || req.VerificationDigest == "" {
		t.Errorf("repair request %+v, want base %s head %s and a digest", req, moved, secondCommit)
	}
	if n := len(l.Rows("crw.milestoneClose")); n != 1 {
		t.Errorf("%d crw.milestoneClose rows", n)
	}
	if n := len(l.Rows("crw.reportUp")); n != 1 {
		t.Errorf("%d crw.reportUp rows", n)
	}
	// the restored worktree of the first run's attempt, skipped over the moved pin, kept its
	// base: its retired ref's commit is parented on the head it was made at
	ref := l.alloc.RetiredRef("milestone:M1", 2)
	if got := l.Git("rev-parse", ref+"^"); got != firstTip {
		t.Errorf("the skipped restore's retired ref %s is parented on %s, want %s", ref, got, firstTip)
	}
	var pending []string
	for _, ev := range l.FlowRows(engine.EvEscalation) {
		if str(ev.Data, "ask") == "crosscheck_pending" {
			pending = append(pending, str(ev.Data, "escalation_id"))
		}
	}
	if len(pending) != 1 || pending[0] != str(v1, "escalation") {
		t.Errorf("crosscheck_pending escalations %v, want the first run's %v only", pending, v1["escalation"])
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

// A resume of a run that closed does nothing: the gate, the crosscheck, the close, the audit and the report are all replayed
// from the journal, and the host is asked for nothing - no call, no read of a thread, no archive.
func TestResumeAfterAClosedMilestoneRunsNothing(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Relay.Surfaces, l.Relay.Grade = []string{"web"}, "C2"
	if got := closeOf(t, l); got.State != "closed" || got.Audit.OK != 1 {
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

func TestCRW1610CloseFixResearchPreservesTheRepair(t *testing.T) {
	l := NewLab(t, oneNode()...)
	gap := Finding("P2", "the regression assertion is missing")
	gap.Kind = "test_gap"
	missing := Finding("P2", "the deadline evidence needs an answer")
	missing.Kind = "evidence_missing"
	missing.EvidenceQuestion = &reviewadapter.EvidenceQuestion{Question: "which deadline applies?", FilesNeeded: []string{"config/deadline.md"}}
	l.Crosschecks("M1", 1, 0, "A", 1, Crosscheck{Verdict: "needs_changes", Findings: []reviewadapter.Finding{gap, missing}})
	const repair = "the regression assertion was added before research\n"
	l.On("^close-fix:M1$", func(c *Call) *fakehostPlan {
		c.EditPath("a/close-fix.txt", repair)
		p := Say(ReceiptWith("needs_research", map[string]any{"research_question": "which deadline applies?"}))
		return &p
	})
	l.On("^explore:milestone:M1:", func(*Call) *fakehostPlan {
		p := Say(map[string]any{"answer": "the deadline is 30 seconds", "evidence": []string{"config/deadline.md:1"}, "verified": []string{"config/deadline.md"}, "inferred": []string{}, "unknown": []string{}})
		return &p
	})
	l.On("^research:milestone:M1:executor:r1$", func(c *Call) *fakehostPlan {
		b, err := os.ReadFile(filepath.Join(c.Cwd, "a/close-fix.txt"))
		if err != nil || string(b) != repair {
			t.Errorf("research lost the uncommitted repair: %q, %v", b, err)
		}
		p := Say(Receipt("ready_for_review"))
		return &p
	})
	got := closeOf(t, l)
	if got.State != "closed" || got.Crosscheck != "verified" {
		t.Fatalf("close %+v; calls %v", got, l.Labels())
	}
	fix, resumed := callNamed(t, l, "close-fix:M1"), callNamed(t, l, "research:milestone:M1:executor:r1")
	if resumed.Resume != fix.ID {
		t.Errorf("research resumed %q, want close-fix %s", resumed.Resume, fix.ID)
	}
	if text := textOfCall(l, resumed.ID); !strings.Contains(text, "the deadline is 30 seconds") {
		t.Errorf("research answer missing: %s", text)
	}
	if actual := l.Git("show", l.Tip()+":a/close-fix.txt"); actual != strings.TrimSpace(repair) {
		t.Errorf("published repair %q", actual)
	}
	commits := closeRepairCommits(l)
	if len(commits) != 1 || str(commits[0].Data, "commit") != got.Tip {
		t.Errorf("repair commits %+v, tip %s", commits, got.Tip)
	}
	if len(l.Rows("crw.crosscheck")) != 2 || len(l.Relay.Repairs) != 1 {
		t.Errorf("the repaired head was not rechecked and published once")
	}
	var researchRoutes int
	for _, row := range l.Rows("crw.exploreRoute") {
		if str(row.Data, "kind") == "research" {
			researchRoutes++
		}
	}
	if researchRoutes != 1 {
		t.Errorf("research routes %d, want one", researchRoutes)
	}
	noTierStep(t, l)
}

func TestCRW1610CloseFixResearchUsesTheExistingCap(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Crosschecks("M1", 1, 0, "A", 1, Crosscheck{Verdict: "needs_changes", Findings: []reviewadapter.Finding{Finding("P1", "the range needs a correction")}})
	l.On("^(close-fix:M1|research:milestone:M1:executor:r[0-9]+)$", func(*Call) *fakehostPlan {
		p := Say(ReceiptWith("needs_research", map[string]any{"research_question": "which deadline applies?"}))
		return &p
	})
	got := closeOf(t, l)
	if got.State != "escalated" || got.Reason != "blocked_scope" {
		t.Fatalf("close %+v; calls %v", got, l.Labels())
	}
	var capSeen bool
	for _, row := range l.SpawnRows() {
		if row.Reason == "research_cap" {
			capSeen = true
		}
	}
	if !capSeen {
		t.Errorf("no existing research_cap decision; calls %v", l.Labels())
	}
	if len(closeRepairCommits(l)) != 0 || len(l.Relay.Repairs) != 0 {
		t.Error("an unresolved research turn published a repair")
	}
	noTierStep(t, l)
}

// CRW-1609: an explicit delivery_criteria annotation moves a whole criterion to
// post-publication readback. The build crosscheck never sees it, the close stays
// pending for crw flow audit --delivery, and the run's result says so.
type crw1609Check struct {
	t      *testing.T
	inner  ops.UnitChecker
	prompt *string
}

func (c *crw1609Check) Check(ctx context.Context, req reviewadapter.Request) (reviewadapter.Result, error) {
	res, err := c.inner.Check(ctx, req)
	if err != nil {
		return res, err
	}
	if c.prompt != nil {
		*c.prompt = req.Prompt
	}
	section := req.Prompt
	if i := strings.Index(section, "CRITERIA OF THE MILESTONE:\n"); i >= 0 {
		section = section[i:]
		if j := strings.Index(section, "\n\n"); j >= 0 {
			section = section[:j]
		}
	}
	if strings.Contains(section, "delivery evidence the owner reads back after publication") {
		f := reviewadapter.Finding{ID: reviewadapter.FindingID("", "delivery criterion still judged"), Severity: "P2", Text: "a criterion left for post-publication readback was judged", Kind: "evidence_missing", EvidenceQuestion: &reviewadapter.EvidenceQuestion{Question: "which delivery evidence applies?", FilesNeeded: []string{}}}
		res.Verdict = "needs_changes"
		res.Findings = append(res.Findings, f)
	}
	return res, nil
}

func TestCRW1609DeliveryCriteriaCloseBeforePublication(t *testing.T) {
	for _, tc := range []struct {
		name     string
		criteria []string
		deferred []string
	}{
		{"mixed", []string{"delivery evidence the owner reads back after publication", "the functional requirement holds"}, []string{"c2"}},
		{"all_delivery", []string{"delivery evidence the owner reads back after publication"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := twoNodes()
			nodes[0].Issue = "acme:work:11"
			nodes[1].Issue = "acme:work:12"
			l := NewLab(t, nodes...)
			l.Relay.MilestoneEdit = func(ms *ops.Milestone) {
				raw, err := json.Marshal(ms)
				if err != nil {
					t.Fatal(err)
				}
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				m["criteria"] = tc.criteria
				m["delivery_criteria"] = []any{map[string]any{"id": "c1", "checks": []any{map[string]any{"kind": "remote_push"}, map[string]any{"kind": "issues_closed"}}}}
				raw, err = json.Marshal(m)
				if err != nil || json.Unmarshal(raw, ms) != nil {
					t.Fatalf("milestone edit: %v", err)
				}
			}
			l.Crosschecks("M1", 1, 0, "A", 1, Crosscheck{Verdict: "verified"})
			var prompt string
			l.crosscheck.Checker = &crw1609Check{t: t, inner: l.Crosschecker, prompt: &prompt}
			res := l.Run()
			if res.Status != engine.RunCompleted {
				t.Fatalf("run %s: %s: %s\ncalls: %v", res.Status, res.ErrorKind, res.Error, l.Labels())
			}
			got := Value[map[string]any](t, res)
			if got["state"] != "closed" || got["crosscheck"] != "verified" {
				t.Fatalf("close %v; calls %v", got, l.Labels())
			}
			delivery, _ := got["delivery"].(map[string]any)
			if delivery["state"] != "pending" {
				t.Fatalf("delivery %v, want pending; close %v", delivery, got)
			}
			section := prompt
			if i := strings.Index(section, "CRITERIA OF THE MILESTONE:\n"); i >= 0 {
				section = section[i:]
				if j := strings.Index(section, "\n\n"); j >= 0 {
					section = section[:j]
				}
			}
			if strings.Contains(section, "c1:") {
				t.Errorf("deferred criterion c1 stayed in the crosscheck criteria: %s", section)
			}
			for _, id := range tc.deferred {
				if !strings.Contains(section, id+":") {
					t.Errorf("criteria %s lost its original id: %s", id, section)
				}
			}
		})
	}
}

// crw1609Audit runs `crw flow audit --run --delivery` against the lab's real
// journal with a real temporary Git remote and fake tracking reads.
func crw1609Audit(t *testing.T, l *Lab, issues map[string][2]string) (int, map[string]any) {
	t.Helper()
	reader := flowcli.DefaultDeliveryReader()
	reader.Issue = func(_ context.Context, ref string) (string, string, error) {
		got, ok := issues[ref]
		if !ok {
			return "", "", fmt.Errorf("unknown issue %s", ref)
		}
		return got[0], got[1], nil
	}
	reader.Milestone = func(context.Context, string) (string, error) { return "", fmt.Errorf("no milestone") }
	oldEnv := flowcli.AuditEnv
	flowcli.AuditEnv = func(context.Context, flowcli.AuditRequest) (flowcli.AuditSetup, error) {
		return flowcli.AuditSetup{RunID: l.RunID, Journal: l.journal, Finished: true, DeliveryReader: reader,
			DeliveryTarget: func(string) (string, string, error) {
				return l.Repo, strings.TrimPrefix(IntegrationRef, "refs/heads/"), nil
			}}, nil
	}
	t.Cleanup(func() { flowcli.AuditEnv = oldEnv })
	var out, errOut bytes.Buffer
	code := flowcli.RunAudit(t.Context(), []string{"audit", "--run", l.RunID, "--delivery", "--json"}, &out, &errOut)
	var report map[string]any
	_ = json.Unmarshal(out.Bytes(), &report)
	if report == nil && errOut.Len() > 0 {
		t.Logf("audit stderr: %s", errOut.String())
	}
	return code, report
}

func TestCRW1609DeliveryAuditReadback(t *testing.T) {
	nodes := twoNodes()
	nodes[0].Issue = "acme:work:11"
	nodes[1].Issue = "acme:work:12"
	l := NewLab(t, nodes...)
	l.Relay.MilestoneEdit = func(ms *ops.Milestone) {
		raw, _ := json.Marshal(ms)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["criteria"] = []string{"delivery evidence the owner reads back after publication", "the functional requirement holds"}
		m["delivery_criteria"] = []any{map[string]any{"id": "c1", "checks": []any{map[string]any{"kind": "remote_push"}, map[string]any{"kind": "issues_closed"}}}}
		raw, _ = json.Marshal(m)
		_ = json.Unmarshal(raw, ms)
	}
	l.Crosschecks("M1", 1, 0, "A", 1, Crosscheck{Verdict: "verified"})
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s", res.Status, res.Error)
	}
	got := Value[map[string]any](t, res)
	if got["state"] != "closed" {
		t.Fatalf("close %v", got)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	l.GitIn(t.TempDir(), "init", "--bare", remote)
	l.Git("remote", "add", "origin", remote)
	// Before publication the remote branch cannot be observed and the issues are open.
	issues := map[string][2]string{"acme/work#11": {"open", ""}, "acme/work#12": {"open", ""}}
	if code, report := crw1609Audit(t, l, issues); code != 1 || report["state"] == "passed" {
		t.Fatalf("unpublished audit %d %v", code, report)
	}
	l.Git("push", "origin", IntegrationRef)
	issues["acme/work#11"] = [2]string{"closed", "completed"}
	issues["acme/work#12"] = [2]string{"closed", "completed"}
	code, report := crw1609Audit(t, l, issues)
	if code != 0 || report["state"] != "passed" || report["head"] != l.Tip() {
		t.Fatalf("published audit %d %v, tip %s", code, report, l.Tip())
	}
	var checked []engine.Event
	for _, ev := range l.journal.Events() {
		if ev.Kind == engine.KindRun && ev.Event == "delivery_checked" {
			checked = append(checked, ev)
		}
	}
	if len(checked) != 2 || str(checked[1].Data, "state") != "passed" || str(checked[1].Data, "head") != l.Tip() {
		t.Errorf("delivery_checked rows %+v, want the passed readback at %s", checked, l.Tip())
	}
	// A not_planned issue is not a delivery.
	issues["acme/work#12"] = [2]string{"closed", "not_planned"}
	if code, _ := crw1609Audit(t, l, issues); code != 1 {
		t.Errorf("not_planned audit %d, want 1", code)
	}
}
