package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
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
// compared with the digest its workflow_opened row recorded by the engine's child gate (engine.ChangedChild, CRW-1461), the one
// refusal of a changed child, before anything is written: under another script of the nested workflow (a binary whose project lane
// changed, and with it the DESIGN schema) the resume would otherwise run the nested workflow's calls live again (the architect and the
// critic) before a later op refused the run. The refusal names the workflow and both digests; no row is written and the backend is
// asked nothing. The same scripts resume as before. An old run's rows (written before the row recorded ref) are refused the same way.
func TestAResumeIsRefusedWhenANestedWorkflowChanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		oldRow bool
	}{{"rows of this build", false}, {"rows written before ref was recorded", true}} {
		t.Run(tc.name, func(t *testing.T) {
			pw := newProcessWorld(t, nestingParent)
			if tc.oldRow {
				withRowsBeforeRef(pw)
			}
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
			if code == 0 || !strings.Contains(errOut, "the child workflow kid is not the one") || !strings.Contains(errOut, recorded) {
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
		})
	}
}

// withRowsBeforeRef makes the resumes of pw read their run's workflow_opened rows as a build before CRW-1461's fix wrote them: without
// ref, so the child gate resolves the child both ways.
func withRowsBeforeRef(pw *processWorld) {
	open := pw.td.World
	pw.td.World = func(ctx context.Context, req WorldRequest) (World, error) {
		w, err := open(ctx, req)
		if err != nil || !req.Resume {
			return w, err
		}
		if j, ok := w.Journal.(*journal.Journal); ok {
			w.Journal = rowsBeforeRef{j}
		}
		return w, nil
	}
}

// rowsBeforeRef is a journal whose workflow_opened rows carry no ref.
type rowsBeforeRef struct{ *journal.Journal }

func (r rowsBeforeRef) Events() []engine.Event {
	events := r.Journal.Events()
	out := make([]engine.Event, len(events))
	for i, ev := range events {
		if ev.Event == "workflow_opened" {
			data := map[string]any{}
			for k, v := range ev.Data {
				if k != "ref" {
					data[k] = v
				}
			}
			ev.Data = data
		}
		out[i] = ev
	}
	return out
}

// A workflow opened by its script path (workflow({scriptPath})) is compared by the same gate with the script that path names (its row
// records ref script): the same file resumes, a changed one is refused.
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
	if code, errOut := pw.execute(spec, true, make(chan os.Signal, 1)); code == 0 || !strings.Contains(errOut, "the child workflow kid.mjs is not the one") {
		t.Errorf("the resume under a changed nested script: exit %d %q", code, errOut)
	}
}
