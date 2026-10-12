package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
)

// testHandoff is a crw-plan handoff of one project whose plan is plan, with the issues of each milestone given and an entry for each
// milestone.
func testHandoff(plan string, milestones map[string][]string) compile.Input {
	p := compile.Project{Key: "P", PlanID: plan}
	var keys []string
	for ms := range milestones {
		keys = append(keys, ms)
	}
	sortStrings(keys)
	for _, ms := range keys {
		for _, issue := range milestones[ms] {
			p.Issues = append(p.Issues, compile.Issue{Key: issue, Title: "issue " + issue, Criteria: []string{"criterion of " + issue}, Milestone: ms})
		}
		p.Milestones = append(p.Milestones, compile.Milestone{Key: ms, Title: "milestone " + ms, Criteria: []string{ms + " is done"}, Surfaces: []string{"logic"}, Grade: "C2", RequiredCommands: []string{}})
	}
	return compile.Input{Schema: compile.InputSchema, Projects: []compile.Project{p}}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// writeHandoff writes a handoff to a file of its own and returns the path.
func writeHandoff(t *testing.T, in compile.Input) string {
	t.Helper()
	raw, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "handoff.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// defaultHandoff is the handoff of plan-1 with milestone M1 holding the night batch's issues.
func defaultHandoff(t *testing.T) string {
	t.Helper()
	return writeHandoff(t, testHandoff("plan-1", map[string][]string{"M1": {"CRW-11", "CRW-12", "CRW-13"}}))
}

// TestStartPinsTheHandoff (CRW-1277 a): `crw flow start --handoff FILE` copies the handoff's bytes into the run's directory and names the
// copy and its sha256 in the run's arguments, after the arguments it was given; a night batch pins the handoff its decision names,
// refused when the file changed since the decision. A workflow that reads its milestone with crw.milestone and names no handoff is
// blocked before anything starts, as is a handoff that holds no project of the plan or no entry of the milestone.
func TestStartPinsTheHandoff(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	file := defaultHandoff(t)
	raw, _ := os.ReadFile(file)
	code, out, errOut := td.do("start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", file)
	if code != 0 || len(td.spawned) != 1 {
		t.Fatalf("exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}
	spec := td.spawned[0].Spec
	ref, ok := HandoffOf(spec.Args)
	pinned := filepath.Join(RunDir(w.state, spec.RunID), HandoffFile)
	if !ok || ref.Path != pinned || ref.SHA256 != sha256Hex(raw) || !strings.HasPrefix(string(spec.Args), `{"plan":"plan-1","milestone":"M1","handoff":{`) {
		t.Fatalf("the run's arguments: %s", spec.Args)
	}
	if stored, err := ReadSpec(w.state, spec.RunID); err != nil || string(compactJSON(stored.Args)) != string(spec.Args) {
		t.Errorf("the stored spec: %+v %v", stored, err)
	}
	if copyRaw, in, err := ReadHandoff(ref); err != nil || string(copyRaw) != string(raw) || len(in.Projects) != 1 {
		t.Errorf("the pinned copy: %v", err)
	}
	if !strings.Contains(out, "handoff: sha256 "+sha256Hex(raw)) {
		t.Errorf("the preview does not name the handoff:\n%s", out)
	}
	// the live file changes; the run reads the copy it pinned
	if err := os.WriteFile(file, []byte(`{"schema":"crw-flow-compile-input/1","projects":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadHandoff(ref); err != nil {
		t.Errorf("the pinned copy after the live file changed: %v", err)
	}
	if _, _, err := ReadHandoff(HandoffRef{Path: file, SHA256: ref.SHA256}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("a changed file read under the fixed sha256: %v", err)
	}

	// a night batch pins the handoff its decision names
	night := defaultHandoff(t)
	nightDecision(t, w, "ud-night", batchJSON(t, func(b *NightBatch) { b.Handoff = refOf(t, night) }), NightRun)
	before := len(td.spawned)
	if code, out, errOut := td.do("start", "night-batch", "--decision", "ud-night", "--thread", "thread-owner", "--state", w.state); code != 0 || len(td.spawned) != before+1 {
		t.Fatalf("night batch: exit %d\n%s%s", code, out, errOut)
	}
	nightSpec := td.spawned[len(td.spawned)-1].Spec
	if ref, ok := HandoffOf(nightSpec.Args); !ok || ref.Path != filepath.Join(RunDir(w.state, nightSpec.RunID), HandoffFile) || ref.SHA256 != refOf(t, night).SHA256 {
		t.Errorf("the night batch's arguments: %s", nightSpec.Args)
	}

	refused := []struct {
		name string
		argv []string
		want string
	}{
		{"a lane with no handoff", []string{"start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`}, "--handoff FILE"},
		{"a preview with no handoff", []string{"start", "project-lane", "--preview", "--args", `{"plan":"plan-1","milestone":"M1"}`}, "--handoff FILE"},
		{"a handoff of another plan", []string{"start", "project-lane", "--args", `{"plan":"plan-9","milestone":"M1"}`, "--handoff", defaultHandoff(t)}, "no project of plan"},
		{"a milestone with no entry", []string{"start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M7"}`, "--handoff", defaultHandoff(t)}, "no entry of milestone"},
		{"a handoff that is no handoff", []string{"start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", writeText(t, `{"schema":"x"}`)}, "crw-flow-compile-input/1"},
		{"args that name a handoff", []string{"start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1","handoff":{}}`, "--handoff", defaultHandoff(t)}, "handoff of their own"},
		{"a handoff beside a decision", []string{"start", "night-batch", "--decision", "ud-night", "--handoff", defaultHandoff(t)}, "its decision names"},
	}
	for _, tc := range refused {
		before := len(td.spawned)
		code, out, errOut := td.do(append(tc.argv, "--thread", "thread-owner", "--state", w.state)...)
		if code == 0 || len(td.spawned) != before || !strings.Contains(out+errOut, tc.want) {
			t.Errorf("%s: exit %d, %d processes\n%s%s", tc.name, code, len(td.spawned)-before, out, errOut)
		}
	}
	// the decision's handoff changed since the decision was recorded
	changed := defaultHandoff(t)
	nightDecision(t, w, "ud-changed", batchJSON(t, func(b *NightBatch) { b.Label, b.Handoff = "changed", refOf(t, changed) }), NightRun)
	if err := os.WriteFile(changed, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	before = len(td.spawned)
	if code, _, errOut := td.do("start", "night-batch", "--decision", "ud-changed", "--thread", "thread-owner", "--state", w.state); code == 0 || len(td.spawned) != before || !strings.Contains(errOut, "changed") {
		t.Errorf("a changed handoff of a decision: exit %d %q", code, errOut)
	}
}

func refOf(t *testing.T, path string) *HandoffRef {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return &HandoffRef{Path: path, SHA256: sha256Hex(raw)}
}

func writeText(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file.json")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
