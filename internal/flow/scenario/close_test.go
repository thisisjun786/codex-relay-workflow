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
)

// The close of a milestone (CRW-1206, F13) on the whole stack of the scenario harness: the real agent backend on the fake App Server
// host, a temporary store and repository, the lane of a project from its architect to its report.

type closeResult struct {
	State    string `json:"state"`
	Tip      string `json:"tip"`
	Source   string `json:"source"`
	Reason   string `json:"reason"`
	Gate     string `json:"gate"`
	Final    string `json:"final"`
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
// oracle passes, the synthesis on the capture call's thread, and reviewer-final with the passed gate. The audit reads every
// reviewer thread back from the host, and the threads of the acceptances that passed are archived - those only.
func TestCloseWithOraclePassesAuditsAndArchives(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	l.Relay.Surfaces, l.Relay.Grade = []string{"logic", "web"}, "C3"
	got := closeOf(t, l)
	if got.State != "closed" || got.Gate != "pass" || got.Final != "ALIGNED" || got.Tip != l.Tip() {
		t.Fatalf("%+v; calls %v; escalations %v", got, l.Labels(), escalationReasons(l))
	}
	labels := l.Labels()
	tail := labels[len(labels)-5:]
	if tail[0] != "qa:M1" || tail[3] != "qa-synthesis:M1" || tail[4] != "reviewer-final:M1" || !slices.Contains(tail[1:3], "qa-pass:A:M1") || !slices.Contains(tail[1:3], "qa-pass:B:M1") {
		t.Fatalf("calls %v", labels)
	}
	roles := map[string]string{}
	for _, c := range l.Calls() {
		roles[c.Label] = c.Role
	}
	if roles["qa:M1"] != "executor" || roles["qa-pass:A:M1"] != "explorer" || roles["qa-pass:B:M1"] != "explorer" || roles["reviewer-final:M1"] != "reviewer-final" {
		t.Errorf("roles %v", roles)
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
	// SP-QA-1, SP-QA-2 twice and SP-REVF-1 are decided
	rules := map[string]int{}
	for _, r := range l.SpawnRows() {
		rules[r.Rule]++
	}
	if rules["SP-QA-1"] != 1 || rules["SP-QA-2"] != 2 || rules["SP-REVF-1"] != 1 {
		t.Errorf("rules %v", rules)
	}
	// the audit read both reviewer threads from the host and found them as recorded; both were archived after it
	if got.Audit.OK != 2 || got.Audit.Mismatch != 0 || got.Audit.Unobserved != 0 || got.Archived != 2 {
		t.Fatalf("audit %+v, archived %d", got.Audit, got.Archived)
	}
	var reviewerThreads []string
	for _, ev := range l.Events() {
		if ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Event == engine.EvThreadStarted && strings.HasPrefix(l.labelOfCall(ev.CallID), "review:") {
			reviewerThreads = append(reviewerThreads, str(ev.Data, "thread_id"))
		}
	}
	archived := l.Archived()
	if len(reviewerThreads) != 2 || !slices.Equal(slices.Sorted(slices.Values(archived)), slices.Sorted(slices.Values(reviewerThreads))) {
		t.Errorf("archived %v, reviewer threads %v", archived, reviewerThreads)
	}
	// the audit rows are the kind of the row the audit leaves
	rows := l.Rows(audit.Op)
	if len(rows) != 1 || len(audit.Passed(l.Events())) != 2 {
		t.Errorf("audit rows %d, passed %v", len(rows), audit.Passed(l.Events()))
	}
}

func (l *Lab) labelOfCall(id string) string {
	for _, c := range l.Calls() {
		if c.ID == id {
			return c.Label
		}
	}
	return ""
}

// A reviewer thread the host does not show in full is unobserved: not a pass, not a mismatch. The milestone still closes, that
// acceptance keeps its thread, and the other one is archived.
func TestCloseKeepsTheThreadOfAnAcceptanceItCouldNotObserve(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	l.On("^review:B:r1$", func(*Call) *fakehostPlan {
		p := Say(Verdict(true))
		p.SummaryOnly = true
		p.ItemsListError = map[string]any{"code": -32600, "message": "items are not available"}
		return &p
	})
	got := closeOf(t, l)
	if got.State != "closed" || got.Audit.OK != 1 || got.Audit.Unobserved != 1 || got.Audit.Mismatch != 0 || got.Archived != 1 {
		t.Fatalf("%+v", got)
	}
	var reviewerA string
	for _, ev := range l.Events() {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvThreadStarted && l.labelOfCall(ev.CallID) == "review:A:r1" {
			reviewerA = str(ev.Data, "thread_id")
		}
	}
	if archived := l.Archived(); !slices.Equal(archived, []string{reviewerA}) || reviewerA == "" {
		t.Errorf("archived %v, want the reviewer thread of A (%s) only", archived, reviewerA)
	}
	if reasons := escalationReasons(l); slices.Contains(reasons, "audit_mismatch") {
		t.Errorf("escalations %v: an unobserved acceptance is not a mismatch", reasons)
	}
}

// What is on the host is the evidence, not what the journal says: a reviewer thread whose last message is not the result the
// journal recorded (the digest differs) is a mismatch. The orchestrator is told, and the thread is kept.
func TestCloseEscalatesAnAuditMismatchAndKeepsTheThread(t *testing.T) {
	l := NewLab(t, oneNode()...)
	// the journal keeps a digest of another result than the thread on the host holds
	l.Rewrite(func(ev engine.Event) engine.Event {
		if ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Event == engine.EvCompleted && strings.HasPrefix(ev.Label, "review:A") {
			d := map[string]any{}
			for k, v := range ev.Data {
				d[k] = v
			}
			d["result_digest"] = strings.Repeat("0", 64)
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
// escalated, and reviewer-final does not run.
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
		if label == "reviewer-final:M1" || label == "qa-synthesis:M1" {
			t.Errorf("the call %q ran after a pass that did not complete", label)
		}
	}
	if reasons := escalationReasons(l); !slices.Contains(reasons, "qa_inconclusive") {
		t.Errorf("escalations %v", reasons)
	}
	rows := l.Rows("crw.qaReceipt")
	if len(rows) != 1 || str(rows[0].Data, "state") != "INCONCLUSIVE" || str(rows[0].Data, "reason") != "pass_B_held" {
		t.Errorf("receipt rows %+v", rows)
	}
}

// A failed verification of the integrated head fails the gate: the milestone is escalated, reviewer-final does not run and nothing
// is archived. A milestone whose verification cannot run is held, and a resume that finds the verifier back goes on.
func TestCloseGateFailsOnAFailedVerificationAndHoldsOnAMissingVerifier(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Verify.Fail = map[string][]int{"": {1}}
	res := l.Run()
	got := Value[closeResult](t, res)
	if got.State != "escalated" || got.Source != "final_gate" || got.Reason != "test_failed" {
		t.Fatalf("%+v", got)
	}
	if slices.Contains(l.Labels(), "reviewer-final:M1") || len(l.Archived()) != 0 {
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

// A resume of a run that closed does nothing: the gate, the final review, the close, the audit and the archive are all replayed
// from the journal, and the host is asked for nothing - no call, no read of a thread, no archive.
func TestResumeAfterAClosedMilestoneRunsNothing(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Relay.Surfaces, l.Relay.Grade = []string{"web"}, "C2"
	if got := closeOf(t, l); got.State != "closed" || got.Archived != 1 {
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

// `crw flow audit` over the store of a run that closed: it opens the store the run's journal is in, reads the reviewer threads back
// from the host the run used, finds every acceptance ok, and leaves its result in the journal (a row of kind crw.audit with its own
// call id). An acceptance given alone is found in the store. A host that cannot be reached is not a pass: the same command finds
// every acceptance unobserved and exits 1.
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

	// the host cannot be reached: nobody listens on its socket any more
	l.Host.Close()
	code, rep, _ = audit_("--run", l.RunID, "--socket", l.Host.SocketPath, "--no-record")
	summary, _ := rep["summary"].(map[string]any)
	if code != 1 || summary["unobserved"] != float64(2) || summary["ok"] != float64(0) {
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
	if forks := l.Requests("thread/fork"); len(forks) != 1 {
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
// the gate pinned: its receipt is INCONCLUSIVE, no pass is asked for it, the milestone is escalated and reviewer-final does not run
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
		if strings.HasPrefix(label, "qa-pass:") || label == "qa-synthesis:M1" || label == "reviewer-final:M1" {
			t.Errorf("the call %q ran over a capture of an edited tree", label)
		}
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
