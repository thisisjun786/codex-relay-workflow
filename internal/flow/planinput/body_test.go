package planinput

import (
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
)

// The five release-gate sections carry two criteria, the chosen implementation,
// the test to write first, the completion command, and the edit regions.
func TestReleaseGateBody(t *testing.T) {
	for _, headings := range [][]string{
		{"기준", "편집 영역", "정한 답", "먼저 실패하게 쓸 시험", "끝 조건"},
		{"Acceptance criteria", "Edit regions", "Decided answer", "Red tests", "Done condition"},
		{"등록 기준", "수정 영역", "확정된 답", "실패하는 시험", "종료 조건"},
		{"Completion criteria", "Edit surfaces", "Design result", "Tests first", "Exit condition"},
	} {
		t.Run(headings[0], func(t *testing.T) {
			body := "## " + headings[0] + "\n- a recorded file is counted\n- an absent file is not counted\n" +
				"## " + headings[1] + "\n- `internal/example/read.go`\n- `internal/example/` tests\n" +
				"## " + headings[2] + "\n- inspect the recorded object type\n" +
				"## " + headings[3] + "\n- a directory must not count as a file\n" +
				"## " + headings[4] + "\n- `go test ./internal/example/ -count=1` passes\n"
			res := build(t, milestone(open(1, body)), Options{})
			is := only(t, res)
			want := []string{
				"a recorded file is counted", "an absent file is not counted",
				"inspect the recorded object type", "a directory must not count as a file",
				"`go test ./internal/example/ -count=1` passes",
			}
			if !reflect.DeepEqual(is.Criteria, want) {
				t.Errorf("criteria = %q; want %q", is.Criteria, want)
			}
			regions := []compile.Region{{Path: "internal/example/read.go", Kind: "file"}, {Path: "internal/example/", Kind: "tree"}}
			if !reflect.DeepEqual(is.Regions, regions) {
				t.Errorf("regions = %+v; want %+v", is.Regions, regions)
			}
			if len(res.Warnings) != 0 {
				t.Errorf("warnings = %+v; want none", res.Warnings)
			}
		})
	}
}

func TestReleaseGateHeadingAliases(t *testing.T) {
	for _, heading := range []string{
		"기준", "완료 기준", "등록 기준", "Criteria", "Acceptance criteria", "Completion criteria",
		"정한 답", "확정한 답", "확정된 답", "Decided answer", "Settled answer", "Design answer", "Design result",
		"빨간 시험", "빨간 테스트", "먼저 실패하게 쓸 시험", "실패하는 시험", "실패 시험", "Red tests", "Failing tests", "Test first", "Tests first",
		"끝 조건", "종료 조건", "Done condition", "Done when", "Finish condition", "End condition", "Exit condition",
		"Acceptance", // legacy alias
	} {
		t.Run(heading, func(t *testing.T) {
			res := build(t, milestone(open(1, "### **"+heading+"**:\n- keep this\n")), Options{})
			if got := only(t, res).Criteria; !reflect.DeepEqual(got, []string{"keep this"}) {
				t.Errorf("criteria = %q", got)
			}
		})
	}
	for _, heading := range []string{"편집 영역", "수정 영역", "Edit regions", "Edit areas", "Edit surfaces", "수정 범위", "Regions"} {
		t.Run(heading, func(t *testing.T) {
			res := build(t, milestone(open(1, "## 기준\n- works\n### **"+heading+"**:\n- `internal/example/`\n")), Options{})
			want := []compile.Region{{Path: "internal/example/", Kind: "tree"}}
			if got := only(t, res).Regions; !reflect.DeepEqual(got, want) {
				t.Errorf("regions = %+v; want %+v", got, want)
			}
		})
	}
}

func TestReleaseGateExcludesUnrelatedSections(t *testing.T) {
	body := "## 기준\n- keep this\n## 편집 영역\n- `internal/example/`\n" +
		"### 편집 영역 (범위 밖)\n- `internal/unrelated/`\n" +
		"## Out-of-scope acceptance criteria\n- not a criterion\n" +
		"## Open decisions\n- not an answer\n## Notes\n- not a test\n" +
		"```markdown\n## Red tests\n- not a real section\n```\n"
	res := build(t, milestone(open(1, body)), Options{})
	is := only(t, res)
	if !reflect.DeepEqual(is.Criteria, []string{"keep this"}) {
		t.Errorf("criteria = %q", is.Criteria)
	}
	if want := []compile.Region{{Path: "internal/example/", Kind: "tree"}}; !reflect.DeepEqual(is.Regions, want) {
		t.Errorf("regions = %+v; want %+v", is.Regions, want)
	}
}
