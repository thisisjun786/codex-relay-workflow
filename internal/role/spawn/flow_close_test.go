package spawn

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// CRW-1206 (F13): the final gate of a milestone close is the run's own, and the native final-gate hook beside it is not touched.

// The QA capture call (SP-QA-1) is a thread of the run that the run binds with an allowance of 0: the run starts the two oracle
// passes itself, so the call must not start them. The hook refuses every native spawn of the thread - the capture turn and the
// synthesis turn that continues it - and the ledger of the thread holds refusals only: no native spawn was decided, none was made.
// A build call of the same run beside it is still allowed its one helper.
func TestQACallSpawnsNoNativeOracle(t *testing.T) {
	r := newFBRig(t)
	h := newFBHost(t)
	b := h.backend()
	qa := func(call string) string {
		spec := h.policy.Roles["executor"]
		info, err := b.StartThread(context.Background(), engine.ThreadSpec{CallID: call, Role: "executor", Pair: spec.Pair, Sandbox: spec.Sandbox, Cwd: h.cwd, QA: true})
		spawnHookMust(t, err)
		op := call + "/t1"
		_, err = b.RunTurn(context.Background(), engine.TurnSpec{CallID: call, Op: op, K: 1, ThreadID: info.ID, Role: "executor", Pair: spec.Pair, Input: "crw-flow-op: " + op + "\n\ncapture", QA: true})
		spawnHookMust(t, err)
		return info.ID
	}
	thread := qa(fbCall1)

	// the QA call tries to run the oracle passes itself
	for i, pass := range []string{"A", "B"} {
		reason := r.expectRefused(r.spawn(thread, "qa-"+pass, `{"agent_type":"explorer","message":"TASK: reviewer pass `+pass+`"}`), bound.ReasonNativeLimitRefused)
		if reason == "" {
			t.Fatalf("pass %d: no reason", i)
		}
	}
	// the synthesis is the next turn of the same thread, and binds it again with the same allowance
	synth := fbCall2
	op := synth + "/t1"
	_, err := b.RunTurn(context.Background(), engine.TurnSpec{CallID: synth, Op: op, K: 1, ThreadID: thread, Role: "executor", Pair: h.policy.Roles["executor"].Pair, Input: "crw-flow-op: " + op + "\n\nsynthesise", QA: true})
	spawnHookMust(t, err)
	r.expectRefused(r.spawn(thread, "qa-C", `{"agent_type":"explorer","message":"TASK: reviewer pass C"}`), bound.ReasonNativeLimitRefused)

	if n := r.claimCount(thread); n != 0 {
		t.Errorf("%d claims: a call that may spawn nothing used no allowance", n)
	}
	d := r.decisions(thread)
	if len(d) != 3 {
		t.Fatalf("ledger %+v", d)
	}
	for _, e := range d {
		if e.Decision != bound.DecisionRefused || e.Reason != bound.ReasonNativeLimitRefused || e.ThreadKind != bound.KindEngine {
			t.Errorf("ledger entry %+v: the QA thread has refusals only", e)
		}
	}
	raw, err := os.ReadFile(filepath.Join(r.dir, thread+bound.FileSuffix))
	spawnHookMust(t, err)
	var lines []bound.Line
	for _, text := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var l bound.Line
		spawnHookMust(t, json.Unmarshal([]byte(text), &l))
		lines = append(lines, l)
	}
	if len(lines) != 2 {
		t.Fatalf("%d binding lines", len(lines))
	}
	for _, l := range lines {
		if l.NativeAllowance == nil || *l.NativeAllowance != 0 {
			t.Errorf("line %+v", l)
		}
	}

	// the run's other threads are not changed by it: a build call may spawn its one helper
	_, build := h.boundThread("executor", "c_000000000003")
	r.expectAllowed(r.spawn(build, "build-1", `{"agent_type":"explorer","message":"TASK: where is the parser?"}`))
}

// The native final-gate hook (CheckFinalGatePrereqs) is what it was: a marked spawn is denied while the test receipt, or for a web
// criterion the QA receipt, is missing or stale, and allowed when both stand on the tree it is asked about. The final gate of a flow
// run judges a commit with the same rule (source.Compare), and neither reads the other's records.
func TestNativeFinalGateHookUnchanged(t *testing.T) {
	cwd := spawnFinalGateTestTree(t)
	first := spawnFinalGateTestCommit(t, cwd, "first.txt")
	identityOf := func() source.Identity { return source.Capture(cwd, source.Options{ExcludeStateArtifacts: true}) }
	firstID := identityOf()
	second := spawnFinalGateTestCommit(t, cwd, "second.txt")
	secondID := identityOf()
	if first == second {
		t.Fatal("two commits")
	}
	write := func(test, qa source.Identity) {
		spawnFinalGateTestWrite(t, cwd, ".crw/sessions/sess-1.json", `{"sessionId":"sess-1","slug":"demo"}`)
		spawnFinalGateTestWrite(t, cwd, ".crw/goalplans/demo/goalplan.json",
			`{"criteria":[{"id":"c1","surface":"web"}],"finalGate":{"testReceiptPath":".crw/evidence/test.json","qaReceiptPath":".crw/evidence/qa.json"}}`)
		for path, id := range map[string]source.Identity{".crw/evidence/test.json": test, ".crw/evidence/qa.json": qa} {
			spawnFinalGateTestWrite(t, cwd, path, mustJSONString(t, map[string]any{"sourceIdentity": id}))
		}
	}
	check := func() FinalGateCheck { return CheckFinalGatePrereqs(spawnFinalGateTestPacket, "sess-1", cwd, nil) }

	// both receipts stand on the first commit; the tree is at the second: both stale
	write(firstID, firstID)
	got := check()
	if got.OK || !strings.Contains(got.Reason, spawnFinalGateHeader) || !strings.Contains(got.Reason, "test receipt was produced against "+firstID.CommitSha[:7]+", but the tree is now "+secondID.CommitSha[:7]) ||
		!strings.Contains(got.Reason, "QA receipt was produced against "+firstID.CommitSha[:7]) || !strings.HasSuffix(got.Reason, spawnFinalGateFooter) {
		t.Fatalf("stale receipts: %+v", got)
	}
	// the test receipt is fresh, the QA receipt of a web criterion is not
	write(secondID, firstID)
	if got := check(); got.OK || strings.Contains(got.Reason, "test receipt was produced") || !strings.Contains(got.Reason, "QA receipt was produced against") {
		t.Fatalf("a stale QA receipt: %+v", got)
	}
	// both fresh
	write(secondID, secondID)
	if got := check(); !got.OK || got.Reason != "" {
		t.Fatalf("fresh receipts: %+v", got)
	}
	// a packet without the marker is not the gate's business
	if got := CheckFinalGatePrereqs("please review", "sess-1", cwd, nil); !got.OK {
		t.Errorf("an unmarked packet: %+v", got)
	}
	// the final gate of a flow run judges a commit with the same rule: its identity of the tree's commit is the native one
	if cmp := source.Compare(source.CommitIdentity(secondID.CommitSha, nil), secondID); cmp.Kind != source.ComparisonSame {
		t.Errorf("the identity of the commit the flow gate pins against the native capture of its clean tree: %+v", cmp)
	}
	if cmp := source.Compare(source.CommitIdentity(firstID.CommitSha, nil), secondID); cmp.Kind != source.ComparisonDifferent {
		t.Errorf("a receipt of the first commit against the second: %+v", cmp)
	}
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	spawnHookMust(t, err)
	return string(b)
}
