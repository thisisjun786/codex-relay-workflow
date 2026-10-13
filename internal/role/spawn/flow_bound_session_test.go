package spawn

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
)

// CRW-1348 (CRW-1316 design §9): the decision table of the orchestrator thread of a session run. Its line carries the pairs of
// the six roles. Two spawns are refused: a spawn by a subagent (LEAF) and a full-history fork. Every other spawn is allowed with
// the pair overwritten from the line, and the ledger records the role and the op of the step it names, with a warning where the
// table says so. A ledger that cannot be written, or a binding that cannot be read, allows with a warning on stderr.

// captureFlowBoundWarnings collects what the hook writes to stderr.
func captureFlowBoundWarnings(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	old := flowBoundWarn
	flowBoundWarn = func(msg string) {
		mu.Lock()
		got = append(got, msg)
		mu.Unlock()
	}
	t.Cleanup(func() { flowBoundWarn = old })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func stepMessage(op, role string) string {
	return "crw-flow-op: " + op + "\nCRW-ROLE: " + role + "\n\nYour shell starts in a directory that is NOT your workspace. Read the task packet /p/packet-t1-a1.md in full."
}

func spawnInput(agentType, message string, extra string) string {
	in := `{"agent_type":` + strconv.Quote(agentType) + `,"message":` + strconv.Quote(message) + `,"fork_context":false`
	if extra != "" {
		in += "," + extra
	}
	return in + "}"
}

func TestSessionOrchestratorDecisionTable(t *testing.T) {
	r := newFBRig(t)
	r.bindOrchestrator(fbThreadA)
	warnings := captureFlowBoundWarnings(t)
	cases := []struct {
		name, input string
		want        bound.Pair
		role, op    string
		warning     string
	}{
		{"reviewer-final step", spawnInput("reviewer", stepMessage("c_1/t1-a1", "reviewer-final"), ""), fbReviewerFinal, "reviewer-final", "c_1/t1-a1", ""},
		{"executor-t1 step", spawnInput("executor", stepMessage("c_2/t1-a2", "executor-t1"), ""), fbExecutorT1, "executor-t1", "c_2/t1-a2", ""},
		{"worker is an executor", spawnInput("worker", stepMessage("c_3/t2-a1", "executor"), ""), fbExecutor, "executor", "c_3/t2-a1", ""},
		{"architect step", spawnInput("architect", stepMessage("c_4/t1-a1", "architect"), ""), fbArchitect, "architect", "c_4/t1-a1", ""},
		{"explorer step", spawnInput("explorer", stepMessage("c_5/t1-a1", "explorer"), ""), fbExplorer, "explorer", "c_5/t1-a1", ""},
		{"reviewer step with a pair of its own", spawnInput("reviewer", stepMessage("c_6/t1-a1", "reviewer"), `"model":"m","reasoning_effort":"low"`), fbReviewer, "reviewer", "c_6/t1-a1", ""},
		{"role and agent_type do not pair", spawnInput("executor", stepMessage("c_7/t1-a1", "reviewer-final"), ""), fbReviewerFinal, "reviewer-final", "c_7/t1-a1", "role_agent_type_mismatch"},
		{"no marker, explorer", spawnInput("explorer", "TASK: where is the parser", ""), fbExplorer, "explorer", "", "unrecorded_spawn"},
		{"no marker, worker", spawnInput("worker", "TASK: fix it", ""), fbExecutor, "executor", "", "unrecorded_author"},
		{"no marker, architect", spawnInput("architect", "TASK: design it", ""), fbArchitect, "architect", "", "unrecorded_author"},
		{"no marker, reviewer", spawnInput("reviewer", "please review this diff", ""), fbReviewer, "reviewer", "", "unrecorded_spawn"},
		{"a header value the flow parser does not take", spawnInput("reviewer", "crw-flow-op: c_8/t1-a1\nCRW-ROLE: Reviewer-Final\n\nx", ""), fbReviewer, "reviewer", "c_8/t1-a1", ""},
		{"a header value glued to a word", spawnInput("reviewer", "crw-flow-op: c_9/t1-a1\nCRW-ROLE: reviewer-finalx\n\nx", ""), fbReviewer, "reviewer", "c_9/t1-a1", ""},
		{"no agent type and no marker", `{"message":"TASK: x"}`, fbExplorer, "explorer", "", "unrecorded_spawn"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool := fmt.Sprintf("tool-%d", i)
			out := r.spawn(fbThreadA, tool, c.input)
			if got := fbPairOf(r.expectAllowed(out)); got != c.want {
				t.Fatalf("pair %+v, want %+v", got, c.want)
			}
			var d *bound.Decision
			for _, x := range r.decisions(fbThreadA) {
				if x.ToolUseID == tool {
					x := x
					d = &x
				}
			}
			if d == nil {
				t.Fatal("no ledger line")
			}
			if d.Decision != bound.DecisionAllowed || d.Role != c.role || d.Op != c.op || d.AppliedModel != c.want.Model || d.AppliedEffort != c.want.Effort ||
				d.ThreadKind != bound.KindOrchestrator || d.RunID != "run-fb" {
				t.Fatalf("ledger line %+v", *d)
			}
			if c.warning == "" && d.Warning != "" || c.warning != "" && !strings.HasPrefix(d.Warning, c.warning) {
				t.Fatalf("warning %q, want %q", d.Warning, c.warning)
			}
		})
	}
	if r.claimCount(fbThreadA) != 0 {
		t.Fatal("the orchestrator's spawns were claimed")
	}
	if w := warnings(); len(w) != 0 {
		t.Fatalf("stderr warnings for spawns whose ledger was written: %v", w)
	}

	// the two refusals
	child := `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"` + fbThreadA + `","agent_id":"child","agent_type":"executor","cwd":` +
		strconv.Quote(r.hook.ws) + `,"tool_use_id":"child-1","tool_input":` + spawnInput("explorer", stepMessage("c_1/t1-a1", "explorer"), "") + `}`
	if _, reason, _ := fbDecision(t, r.run(child)); !strings.Contains(reason, "LEAF-TOPOLOGY-01") {
		t.Errorf("a subagent's spawn: %q", reason)
	}
	for i, in := range []string{
		`{"agent_type":"reviewer","message":` + strconv.Quote(stepMessage("c_1/t1-a1", "reviewer")) + `,"fork_context":true}`,
		`{"agent_type":"explorer","task_name":"t","fork_turns":"all","message":"TASK: q"}`,
	} {
		r.expectRefused(r.spawn(fbThreadA, fmt.Sprintf("fork-%d", i), in), bound.ReasonFullHistoryFork)
	}
}

// A ledger that cannot be written allows the spawn with its pair and says so on stderr: no flow_bound_unavailable refusal.
func TestSessionOrchestratorLedgerUnwritableAllows(t *testing.T) {
	r := newFBRig(t)
	r.bindOrchestrator(fbThreadA)
	warnings := captureFlowBoundWarnings(t)
	// the ledger's place is taken by a directory, so no line can be appended to it
	spawnHookMust(t, os.MkdirAll(filepath.Join(r.dir, bound.DecisionsDir, fbThreadA+bound.FileSuffix), 0o700))
	out := r.spawn(fbThreadA, "tool-1", spawnInput("reviewer", stepMessage("c_1/t1-a1", "reviewer-final"), ""))
	if got := fbPairOf(r.expectAllowed(out)); got != fbReviewerFinal {
		t.Fatalf("pair %+v", got)
	}
	if w := warnings(); len(w) != 1 || !strings.Contains(w[0], "ledger") || !strings.Contains(w[0], fbThreadA) {
		t.Fatalf("stderr warnings %v", w)
	}
}

// A failure of the hook while it decides an orchestrator's spawn lets the spawn through untouched, with a warning.
func TestSessionOrchestratorHookFailureAllows(t *testing.T) {
	r := newFBRig(t)
	r.bindOrchestrator(fbThreadA)
	warnings := captureFlowBoundWarnings(t)
	spawnFlowBoundBeforeDecide = func() { panic("injected") }
	t.Cleanup(func() { spawnFlowBoundBeforeDecide = func() {} })
	if out := r.spawn(fbThreadA, "tool-1", spawnInput("reviewer", stepMessage("c_1/t1-a1", "reviewer"), "")); out != "" {
		t.Fatalf("the spawn was not let through: %q", out)
	}
	if w := warnings(); len(w) != 1 || !strings.Contains(w[0], "failed") {
		t.Fatalf("stderr warnings %v", w)
	}
}

// The flow-bound CRW-ROLE parser takes exactly the six role values, only in the header (before the first blank line or TASK:
// line), and the general oracle parser is what it was.
func TestFlowBoundRoleHeaderTakesTheSixRoles(t *testing.T) {
	for msg, want := range map[string]string{
		"CRW-ROLE: reviewer-final\n\nx":                     "reviewer-final",
		"crw-flow-op: c/t1-a1\nCRW-ROLE: executor-t1\n\nx":  "executor-t1",
		"scope: x\nCRW-ROLE: architect\nTASK: y":            "architect",
		"CRW-ROLE: explorer  \r\nTASK: y":                   "explorer",
		"CRW-ROLE: worker\n\nx":                             "",
		"CRW-ROLE: Reviewer\n\nx":                           "",
		"CRW-ROLE: reviewer final\n\nx":                     "",
		"TASK: x\nCRW-ROLE: reviewer":                       "",
		"crw-flow-op: c/t1-a1\n\nCRW-ROLE: reviewer\n":      "",
		"no header at all":                                  "",
		"CRW-ROLE:reviewer\n\nx":                            "reviewer",
		"  CRW-ROLE: reviewer\n\nx":                         "",
		"CRW-ROLE: executor\nCRW-ROLE: reviewer-final\n\nx": "executor",
	} {
		if got := flowBoundRoleHeader(msg); got != want {
			t.Errorf("flowBoundRoleHeader(%q) = %q, want %q", msg, got, want)
		}
	}
	// the oracle parser keeps its three values
	if got := spawnClassifyRoleMarker("CRW-ROLE: reviewer-final\n"); got != "" {
		t.Errorf("the oracle parser took reviewer-final: %q", got)
	}
	if got := spawnClassifyRoleMarker("CRW-ROLE: reviewer\n"); got != "reviewer" {
		t.Errorf("the oracle parser: %q", got)
	}
}
