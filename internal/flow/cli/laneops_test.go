package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStartRefusesWorkflowsThatNeedLaneOperationsTheBuildLacks: a build that installs no `crw.*` operations (OpsFor is nil) starts no
// run of a workflow that calls one, the built-in project lane and night batch among them: the run would end in a script error at its
// first call after it had taken a run id, a lock and a detached process. The refusal is not a preflight fact, so --force does not
// override it, and the preview says it; a workflow that calls no such operation starts (CRW-1205 d2).
func TestStartRefusesWorkflowsThatNeedLaneOperationsTheBuildLacks(t *testing.T) {
	w := newWorld(t)
	nightDecision(t, w, "ud-night", batchJSON(t, nil), NightRun)
	td := w.deps()
	td.LaneOps = func() bool { return false }
	agentOnly := filepath.Join(t.TempDir(), "agent-only.js")
	if err := os.WriteFile(agentOnly, []byte("export const meta = { name: 'agent-only' }\nconst r = await agent.call('say hi', { agentType: 'executor' })\nreturn r.status\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, argv := range map[string][]string{
		"project lane":       {"start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--args", `{"plan":"plan-1","milestone":"M1"}`},
		"project lane force": {"start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--args", `{"plan":"plan-1","milestone":"M1"}`, "--force"},
		"night batch":        {"start", "night-batch", "--decision", "ud-night", "--thread", "thread-owner", "--state", w.state},
		"night batch force":  {"start", "night-batch", "--decision", "ud-night", "--thread", "thread-owner", "--state", w.state, "--force"},
		"preview":            {"start", "project-lane", "--preview", "--thread", "thread-owner", "--state", w.state, "--args", `{"plan":"plan-1","milestone":"M1"}`},
	} {
		before := len(td.spawned)
		code, out, errOut := td.do(argv...)
		if code == 0 || len(td.spawned) != before || !strings.Contains(out+errOut, "crw.milestone") || !strings.Contains(out+errOut, "installs no") {
			t.Errorf("%s: exit %d, %d processes\n%s%s", name, code, len(td.spawned)-before, out, errOut)
		}
	}
	if code, out, errOut := td.do("start", agentOnly, "--thread", "thread-owner", "--state", w.state); code != 0 || len(td.spawned) != 1 {
		t.Errorf("a workflow that calls no lane operation: exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}
}
