package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

const nestingParent = `export const meta = { name: 'two', phases: ['one'] }
phase('one')
const k = await workflow('kid')
return k
`

func nestedKid(prompt string) string {
	return "export const meta = { name: 'kid' }\nconst r = await agent.call('" + prompt + "', { agentType: 'explorer', label: 'kid' })\nreturn r.status\n"
}

// TestAResumeIsRefusedWhenANestedWorkflowChanged (the DI's P2 of CRW-1425): a run is resumed only under the scripts it ran. The run's
// own script is compared with its started row (notTheRuns); a workflow it opened with workflow() (a night batch's project lane) is
// compared with the digest its workflow_opened row recorded, before anything is written: under another script of the nested workflow
// (a binary whose project lane changed, and with it the DESIGN schema) the resume would run the nested workflow's calls live again
// (the architect and the critic) before a later op refused the run. The refusal names the workflow and both digests; no row is
// written and the backend is asked nothing. The same scripts resume as before.
func TestAResumeIsRefusedWhenANestedWorkflowChanged(t *testing.T) {
	pw := newProcessWorld(t, nestingParent)
	kid := filepath.Join(filepath.Dir(pw.script), "kid.js")
	if err := os.WriteFile(kid, []byte(nestedKid("child work")), 0o644); err != nil {
		t.Fatal(err)
	}
	run := "run-nested"
	spec := pw.spec(run, nil)
	if code, errOut := pw.execute(spec, false, make(chan os.Signal, 1)); code != 0 {
		t.Fatalf("the first process: exit %d %s", code, errOut)
	}
	rowsOf := func() int {
		events, _, err := loadRun(pw.world, run)
		if err != nil {
			t.Fatal(err)
		}
		return len(events)
	}
	var recorded string
	events, _, _ := loadRun(pw.world, run)
	for _, ev := range events {
		if ev.Kind == engine.KindFlow && ev.Event == "workflow_opened" {
			recorded = str(ev.Data, "script_digest")
		}
	}
	if recorded == "" {
		t.Fatal("the run recorded no workflow_opened row")
	}

	if err := os.WriteFile(kid, []byte(nestedKid("child work, changed")), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, asked := rowsOf(), len(pw.fake.Requests())
	code, errOut := pw.execute(spec, true, make(chan os.Signal, 1))
	if code == 0 || !strings.Contains(errOut, "the kid workflow") || !strings.Contains(errOut, recorded) {
		t.Errorf("the resume under a changed nested workflow: exit %d %q", code, errOut)
	}
	if got := rowsOf(); got != rows {
		t.Errorf("the refused resume wrote rows: %d -> %d", rows, got)
	}
	if n := len(pw.fake.Requests()) - asked; n != 0 {
		t.Errorf("the refused resume asked the backend %d times", n)
	}

	if err := os.WriteFile(kid, []byte(nestedKid("child work")), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, errOut := pw.execute(spec, true, make(chan os.Signal, 1)); code != 0 {
		t.Errorf("the resume under the same scripts: exit %d %q", code, errOut)
	}
}

// A workflow opened by its script path (workflow({scriptPath})) is compared with the script that path names: the same file resumes,
// a changed one is refused (the label does not say whether it was a name or a path).
func TestAResumeComparesANestedWorkflowOpenedByItsPath(t *testing.T) {
	pw := newProcessWorld(t, strings.Replace(nestingParent, "workflow('kid')", "workflow({ scriptPath: 'kid.mjs' })", 1))
	kid := filepath.Join(filepath.Dir(pw.script), "kid.mjs")
	if err := os.WriteFile(kid, []byte(nestedKid("child work")), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := pw.spec("run-nested-path", nil)
	if code, errOut := pw.execute(spec, false, make(chan os.Signal, 1)); code != 0 {
		t.Fatalf("the first process: exit %d %s", code, errOut)
	}
	if code, errOut := pw.execute(spec, true, make(chan os.Signal, 1)); code != 0 {
		t.Errorf("the resume under the same scripts: exit %d %q", code, errOut)
	}
	if err := os.WriteFile(kid, []byte(nestedKid("child work, changed")), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, errOut := pw.execute(spec, true, make(chan os.Signal, 1)); code == 0 || !strings.Contains(errOut, "the kid.mjs workflow is not the one") {
		t.Errorf("the resume under a changed nested script: exit %d %q", code, errOut)
	}
}
