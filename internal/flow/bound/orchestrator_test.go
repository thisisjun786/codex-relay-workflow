package bound

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1348 (CRW-1316 design §9): the orchestrator line of a session run carries the pairs of the role subagents, and its life is the
// run's: BindOrchestrator writes it when the run starts or resumes, CloseOrchestrator closes it when the run ends, a thread holds
// the line of one live run (a run whose process holds its run lock), and the open line of a run without the lock is closed first.

const threadB = "0190a000-0000-7000-8000-00000000000b"

// sixPairs are the pairs of every orchestrator role (six: the critic came with CRW-1434, reviewer-final left with CRW-1450).
func sixPairs(model string) map[string]Pair {
	out := map[string]Pair{}
	for _, role := range OrchestratorRoles {
		out[role] = Pair{Model: model + "-" + role, Effort: "high"}
	}
	return out
}

func TestOrchestratorLineCarriesTheSixRolePairs(t *testing.T) {
	if strings.Join(OrchestratorRoles, ",") != "architect,executor,executor-t1,reviewer,explorer,critic" {
		t.Fatalf("the six roles: %v", OrchestratorRoles)
	}
	good := Line{RunID: "run-1", ThreadID: threadA, Kind: KindOrchestrator, HelperPairs: sixPairs("m"), PolicyDigest: "sha256:x"}
	if err := good.Validate(threadA); err != nil {
		t.Fatalf("a line with the six pairs: %v", err)
	}
	// reviewer-final is a retired role key (CRW-1450): no orchestrator role, no agent_type mapping; a line an earlier binary wrote with
	// its pair still validates, and the pair is never applied.
	if IsOrchestratorRole("reviewer-final") || AgentTypeOf("reviewer-final") != "reviewer-final" {
		t.Fatalf("reviewer-final is still an orchestrator role: %v %q", IsOrchestratorRole("reviewer-final"), AgentTypeOf("reviewer-final"))
	}
	older := good
	older.HelperPairs = sixPairs("m")
	older.HelperPairs["reviewer-final"] = Pair{Model: "m-reviewer-final", Effort: "xhigh"}
	if err := older.Validate(threadA); err != nil {
		t.Fatalf("a line of an earlier binary with a reviewer-final pair: %v", err)
	}
	for _, role := range OrchestratorRoles {
		l := good
		l.HelperPairs = sixPairs("m")
		delete(l.HelperPairs, role)
		// the critic is optional (CRW-1434, TestOrchestratorLineWithoutACriticPairStillValidates); every other role is required
		if err := l.Validate(threadA); role != "critic" && (err == nil || !strings.Contains(err.Error(), role)) {
			t.Errorf("a line without the %s pair: %v", role, err)
		}
		// a role that names a model and leaves its effort undecided (CRW-1330, CRW-1331) has the empty effort, and only the
		// decided part is carried: the line validates
		l.HelperPairs = sixPairs("m")
		l.HelperPairs[role] = Pair{Model: "m"}
		if err := l.Validate(threadA); err != nil {
			t.Errorf("a line whose %s pair names a model and an undecided effort: %v", role, err)
		}
		// an effort without a model is no decided part of any role pair
		l.HelperPairs = sixPairs("m")
		l.HelperPairs[role] = Pair{Effort: "high"}
		if err := l.Validate(threadA); err == nil {
			t.Errorf("a line whose %s pair names an effort and no model validates", role)
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

// CRW-1434 (D-7): the critic is the last role of an orchestrator line, and an optional one. A line the earlier binary wrote
// carries the six old pairs (reviewer-final among them, a retired key since CRW-1450) and no critic; it stays readable, so the spawn hook keeps the run's pairs and ledger rows until the
// run rebinds. A critic pair that is present follows the same rule as every other role's.
func TestOrchestratorLineWithoutACriticPairStillValidates(t *testing.T) {
	if n := len(OrchestratorRoles); n == 0 || OrchestratorRoles[n-1] != "critic" {
		t.Fatalf("the critic is not the last orchestrator role: %v", OrchestratorRoles)
	}
	if got := AgentTypeOf("critic"); got != "reviewer" {
		t.Fatalf("the critic spawns as agent_type %q, not reviewer", got)
	}
	if !IsOrchestratorRole("critic") {
		t.Fatal("the critic is no orchestrator role")
	}
	old := map[string]Pair{}
	for _, role := range []string{"architect", "executor", "executor-t1", "reviewer", "reviewer-final", "explorer"} {
		old[role] = Pair{Model: "m-" + role, Effort: "high"}
	}
	six := Line{RunID: "run-1", ThreadID: threadA, Kind: KindOrchestrator, HelperPairs: old, PolicyDigest: "sha256:x"}
	if err := six.Validate(threadA); err != nil {
		t.Fatalf("a line with the six old pairs and no critic: %v", err)
	}
	current := six
	current.HelperPairs = sixPairs("m")
	if _, ok := current.HelperPairs["critic"]; !ok || len(current.HelperPairs) != 6 {
		t.Fatalf("the pairs of the orchestrator roles: %v", current.HelperPairs)
	}
	if err := current.Validate(threadA); err != nil {
		t.Fatalf("a line with the six pairs: %v", err)
	}
	effortOnly := six
	effortOnly.HelperPairs = sixPairs("m")
	effortOnly.HelperPairs["critic"] = Pair{Effort: "xhigh"}
	if err := effortOnly.Validate(threadA); err == nil || !strings.Contains(err.Error(), "critic") {
		t.Fatalf("a line whose critic pair names an effort and no model: %v", err)
	}
	undecided := six
	undecided.HelperPairs = sixPairs("m")
	undecided.HelperPairs["critic"] = Pair{}
	if err := undecided.Validate(threadA); err != nil {
		t.Fatalf("a line with an undecided critic: %v", err)
	}
	noExecutor := six
	noExecutor.HelperPairs = sixPairs("m")
	delete(noExecutor.HelperPairs, "executor")
	if err := noExecutor.Validate(threadA); err == nil || !strings.Contains(err.Error(), "executor") {
		t.Fatalf("a line without the executor pair: %v", err)
	}
}
