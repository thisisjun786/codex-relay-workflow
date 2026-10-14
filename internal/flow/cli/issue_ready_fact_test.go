package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
)

// TestFlowPathRecordsANotReadyIssueAsAWarning (CRW-1328): the flow path judges the issues it releases with the release gate when the
// handoff carries their body (an issue's description and labels): a not_ready or design_first issue is the warn fact issue_ready,
// printed and recorded in the run's spec with the decision and the reasons, and the run starts (warn and record, as crw-run's path
// records the gate's warning, CRW-1318). A ready issue, and an issue whose body the handoff does not carry, add no fact.
func TestFlowPathRecordsANotReadyIssueAsAWarning(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1"})
	const ready = "## 기준\n\n* c1: the work is in the node file.\n\n## 편집 영역\n\n* `node-A-2.txt`\n\n## 정한 답\n\nThe node writes its file.\n\n" +
		"## 먼저 실패하게 쓸 시험\n\n* TestNodeFile fails first.\n\n## 끝 조건\n\n* `go test ./internal/x -run '^TestNodeFile$'` passes.\n\n## 열린 결정\n\n없음\n"
	start := func(mutate func(*compile.Input)) Spec {
		t.Helper()
		file := filepath.Join(t.TempDir(), "handoff.json")
		if err := os.WriteFile(file, l.handoffBytes(mutate), 0o600); err != nil {
			t.Fatal(err)
		}
		before := len(l.td.spawned)
		code, out, errOut := l.td.do("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", file, "--thread", labOwner, "--state", l.w.state,
			"--socket", l.host.SocketPath, "--repo", l.repo)
		if code != 0 || len(l.td.spawned) != before+1 {
			t.Fatalf("the start: exit %d\n%s%s", code, out, errOut)
		}
		return l.td.spawned[len(l.td.spawned)-1].Spec
	}
	factOf := func(spec Spec) *Fact {
		for _, f := range spec.Preflight {
			if f.Name == "issue_ready" {
				return &f
			}
		}
		return nil
	}
	spec := start(func(in *compile.Input) {
		in.Projects[0].Issues[0].Description = "## 기준\n\n* c1: the work is in the node file.\n"
		in.Projects[0].Issues[1].Description, in.Projects[0].Issues[1].Labels = ready, []string{"night"}
	})
	f := factOf(spec)
	if f == nil || f.Level != LevelWarn || !strings.Contains(f.Reading, "A-1 is not_ready") || !strings.Contains(f.Reading, "missing decided_answer") || strings.Contains(f.Reading, "A-2") || f.Reason == "" {
		t.Errorf("the issue_ready fact of a not_ready issue: %+v (%+v)", f, spec.Preflight)
	}
	if f := factOf(start(func(in *compile.Input) {
		in.Projects[0].Issues[1].Description, in.Projects[0].Issues[1].Labels = ready, []string{"design-first"}
	})); f == nil ||
		!strings.Contains(f.Reading, "A-2 is design_first") {
		t.Errorf("a design_first issue: %+v", f)
	}
	if f := factOf(start(func(in *compile.Input) { in.Projects[0].Issues[1].Description = ready })); f != nil {
		t.Errorf("a ready issue is a fact: %+v", f)
	}
	if f := factOf(start(nil)); f != nil {
		t.Errorf("issues the handoff carries no body of are a fact: %+v", f)
	}
}
