package bound

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1348 (CRW-1316 design §9): the orchestrator line of a session run carries the pairs of the six roles, and its life is the
// run's: BindOrchestrator writes it when the run starts or resumes, CloseOrchestrator closes it when the run ends, a thread holds
// the line of one live run (a run whose process holds its run lock), and the open line of a run without the lock is closed first.

const threadB = "0190a000-0000-7000-8000-00000000000b"

func sixPairs(model string) map[string]Pair {
	out := map[string]Pair{}
	for _, role := range OrchestratorRoles {
		out[role] = Pair{Model: model + "-" + role, Effort: "high"}
	}
	return out
}

func TestOrchestratorLineCarriesTheSixRolePairs(t *testing.T) {
	if strings.Join(OrchestratorRoles, ",") != "architect,executor,executor-t1,reviewer,reviewer-final,explorer" {
		t.Fatalf("the six roles: %v", OrchestratorRoles)
	}
	good := Line{RunID: "run-1", ThreadID: threadA, Kind: KindOrchestrator, HelperPairs: sixPairs("m"), PolicyDigest: "sha256:x"}
	if err := good.Validate(threadA); err != nil {
		t.Fatalf("a line with the six pairs: %v", err)
	}
	for _, role := range OrchestratorRoles {
		l := good
		l.HelperPairs = sixPairs("m")
		delete(l.HelperPairs, role)
		if err := l.Validate(threadA); err == nil || !strings.Contains(err.Error(), role) {
			t.Errorf("a line without the %s pair: %v", role, err)
		}
		l.HelperPairs = sixPairs("m")
		l.HelperPairs[role] = Pair{Model: "m"}
		if err := l.Validate(threadA); err == nil {
			t.Errorf("a line whose %s pair has no effort validates", role)
		}
	}
	// an undecided role (CRW-1330) has the empty pair
	undecided := good
	undecided.HelperPairs = sixPairs("m")
	undecided.HelperPairs["executor-t1"] = Pair{}
	if err := undecided.Validate(threadA); err != nil {
		t.Fatalf("a line with an undecided role: %v", err)
	}
	// the two pairs an orchestrator line of the App Server path had are not enough
	two := Line{RunID: "run-1", ThreadID: threadA, Kind: KindOrchestrator, HelperPairs: map[string]Pair{HelperExplorer: {"m", "high"}, HelperReviewer: {"r", "high"}}}
	if err := two.Validate(threadA); err == nil {
		t.Fatal("an orchestrator line with two pairs validates")
	}
}

func TestBindOrchestratorOneLiveRunPerThread(t *testing.T) {
	dir := home(t)
	w := &Writer{}
	live := map[string]bool{}
	bind := func(run string) (BindResult, error) {
		return BindOrchestrator(OrchestratorBinding{Writer: w, RunID: run, Thread: threadA, Digest: "sha256:" + run, Pairs: sixPairs(run), Live: func(r string) bool { return live[r] }})
	}
	res, err := bind("run-1")
	if err != nil || !res.Bound || res.Holder != "" || res.Closed != "" {
		t.Fatalf("first bind: %+v %v", res, err)
	}
	if got := LookupIn(dir, threadA); got.State != Orchestrator || got.Line.RunID != "run-1" || got.Line.HelperPairs["executor-t1"].Model != "run-1-executor-t1" {
		t.Fatalf("after the bind: %+v", got)
	}
	// the same run again (a resume) writes nothing
	live["run-1"] = true
	if res, err := bind("run-1"); err != nil || res.Bound || !res.Already {
		t.Fatalf("the same run again: %+v %v", res, err)
	}
	// a second run while the first one's process holds its lock: not bound, the holder named
	res, err = bind("run-2")
	if err != nil || res.Bound || res.Holder != "run-1" {
		t.Fatalf("a second live start: %+v %v", res, err)
	}
	if got := LookupIn(dir, threadA); got.Line.RunID != "run-1" {
		t.Fatalf("the second start took the thread: %+v", got.Line)
	}
	// the first run's process is gone (no lock): its open line is closed and the second run is bound
	live["run-1"] = false
	res, err = bind("run-2")
	if err != nil || !res.Bound || res.Closed != "run-1" {
		t.Fatalf("over a stale line: %+v %v", res, err)
	}
	lines := readLines(t, dir, threadA)
	kinds := []string{}
	for _, l := range lines {
		kinds = append(kinds, l.Kind+":"+l.RunID)
	}
	if strings.Join(kinds, ",") != "orchestrator:run-1,closed:run-1,orchestrator:run-2" {
		t.Fatalf("lines %v", kinds)
	}
	// a resume whose pairs changed writes the line again
	if res, err := BindOrchestrator(OrchestratorBinding{Writer: w, RunID: "run-2", Thread: threadA, Digest: "sha256:new", Pairs: sixPairs("new")}); err != nil || !res.Bound {
		t.Fatalf("repinned pairs: %+v %v", res, err)
	}
	// closing: only the run whose line is last closes it, once
	if closed, err := CloseOrchestrator(w, "run-1", threadA); err != nil || closed {
		t.Fatalf("another run closed the thread: %v %v", closed, err)
	}
	if closed, err := CloseOrchestrator(w, "run-2", threadA); err != nil || !closed {
		t.Fatalf("close: %v %v", closed, err)
	}
	if got := LookupIn(dir, threadA); got.State != Standalone {
		t.Fatalf("after the close: %+v", got)
	}
	if closed, err := CloseOrchestrator(w, "run-2", threadA); err != nil || closed {
		t.Fatalf("second close: %v %v", closed, err)
	}
	// a thread that is no thread id is not bound
	if _, err := BindOrchestrator(OrchestratorBinding{Writer: w, RunID: "run-3", Thread: "thread-owner", Pairs: sixPairs("m")}); err == nil {
		t.Fatal("a thread that is no thread id was bound")
	}
	// another thread is untouched by all this
	if got := LookupIn(dir, threadB); got.State != Standalone {
		t.Fatalf("thread B: %+v", got)
	}
}

func TestDecisionRecordsRoleAndOp(t *testing.T) {
	dir := home(t)
	if err := ensureDir(dir); err != nil {
		t.Fatal(err)
	}
	d := Decision{RunID: "run-1", ThreadID: threadA, ThreadKind: KindOrchestrator, ToolUseID: "tool-1", Decision: DecisionAllowed, Role: "reviewer-final",
		Op: "c_1/t1-a1", Warning: "role_agent_type_mismatch: x"}
	if err := AppendDecision(dir, d); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDecisions(dir, threadA)
	if err != nil || len(got) != 1 || got[0].Role != "reviewer-final" || got[0].Op != "c_1/t1-a1" || got[0].Warning == "" {
		t.Fatalf("%+v %v", got, err)
	}
}

func readLines(t *testing.T, dir, thread string) []Line {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, thread+FileSuffix))
	if err != nil {
		t.Fatal(err)
	}
	var out []Line
	for _, s := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		l, err := decodeLine([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

func TestMarkerOpIsTheFirstLine(t *testing.T) {
	for msg, want := range map[string]string{
		"crw-flow-op: c_0123456789ab/t1-a2\nCRW-ROLE: reviewer": "c_0123456789ab/t1-a2",
		"crw-flow-op: c_1/t3\n":                                 "c_1/t3",
		"crw-flow-op: c_1/t3-a1  \r\nx":                         "c_1/t3-a1",
		"x\ncrw-flow-op: c_1/t1-a1":                             "",
		"crw-flow-op: c_1/t0-a1":                                "",
		"crw-flow-op: ../t1-a1":                                 "",
		"crw-flow-op: c_1/t1-a1 extra":                          "",
		"CRW-FLOW-OP: c_1/t1-a1":                                "",
		"":                                                      "",
	} {
		if got := MarkerOp(msg); got != want {
			t.Errorf("MarkerOp(%q) = %q, want %q", msg, got, want)
		}
	}
}
