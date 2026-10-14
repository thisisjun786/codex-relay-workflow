package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	if entries, err := os.ReadDir(filepath.Join(w.state, "flow")); err == nil && len(entries) != 0 {
		t.Errorf("a refused start left run directories: %v", entries)
	}
	// the functions the engine itself puts on crw (clock, classify, tierStep) need no operation of the lane
	engineOnly := filepath.Join(t.TempDir(), "engine-only.js")
	if err := os.WriteFile(engineOnly, []byte("export const meta = { name: 'engine-only' }\nconst at = crw.clock()\nconst r = await agent.call('say hi', { agentType: 'executor' })\nreturn [at, crw.classify(r)]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := td.do("start", engineOnly, "--thread", "thread-owner", "--state", w.state); code != 0 || len(td.spawned) != 1 {
		t.Errorf("a workflow that calls only the engine's own crw functions: exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}
	td.spawned = nil
	if code, out, errOut := td.do("start", agentOnly, "--thread", "thread-owner", "--state", w.state); code != 0 || len(td.spawned) != 1 {
		t.Errorf("a workflow that calls no lane operation: exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}
}

// TestEngineCrwFunctionsAreTheEnginesOwn: the names LaneOperations leaves out are exactly the ones the engine puts on the crw object
// itself (scriptvm.go), so a function the engine adds later is not taken for an operation the build has to install, and an operation
// is never left out by mistake.
func TestEngineCrwFunctionsAreTheEnginesOwn(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "engine", "scriptvm.go"))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, m := range regexp.MustCompile(`crw\.Set\("([A-Za-z]+)"`).FindAllStringSubmatch(string(src), -1) {
		found = append(found, m[1])
	}
	var own []string
	for name := range engineCrw {
		own = append(own, name)
	}
	sort.Strings(found)
	sort.Strings(own)
	if strings.Join(found, ",") != strings.Join(own, ",") {
		t.Errorf("the engine sets %v on crw, LaneOperations leaves out %v", found, own)
	}
}
