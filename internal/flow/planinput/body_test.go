package planinput

import (
	"encoding/json"
	"reflect"
	"strings"
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

func TestOutOfScopeDescendantsAreExcluded(t *testing.T) {
	for _, heading := range []string{"범위 밖", "Out of scope", "Out-of-scope", "**NON-GOALS**:", "편집 영역 (범위 밖)"} {
		for _, resume := range []string{"## 기준", "# 기준"} {
			t.Run(heading+"/"+resume, func(t *testing.T) {
				body := "## 기준\n- before\n## 편집 영역\n- `keep.go`\n" +
					"## " + heading + "\n- excluded prose\n" +
					"### Notes\n#### 편집 영역\n- `hidden.go`\n" +
					"### Red tests\n- hidden red test\n" +
					"### 범위 밖\n#### 기준\n- hidden criterion\n" +
					"```markdown\n# 기준\n- fake boundary\n```\n" +
					"### Done when\n- hidden done condition\n" +
					resume + "\n- after\n## 편집 영역\n- `after.go`\n"
				res := build(t, milestone(open(1, body)), Options{})
				is := only(t, res)
				if want := []string{"before", "after"}; !reflect.DeepEqual(is.Criteria, want) {
					t.Errorf("criteria = %q; want %q", is.Criteria, want)
				}
				wantRegions := []compile.Region{{Path: "keep.go", Kind: "file"}, {Path: "after.go", Kind: "file"}}
				if !reflect.DeepEqual(is.Regions, wantRegions) {
					t.Errorf("regions = %+v; want %+v", is.Regions, wantRegions)
				}
				if len(res.Warnings) != 0 {
					t.Errorf("warnings = %+v; want none", res.Warnings)
				}
			})
		}
	}
}

func TestOutOfScopeAtEndAndMissingCriteria(t *testing.T) {
	// Dropping an excluded heading must not attach its plain lines to the prior section.
	body := "## 기준\n- keep\n## 범위 밖\n- discard\n"
	if got := only(t, build(t, milestone(open(1, body)), Options{})).Criteria; !reflect.DeepEqual(got, []string{"keep"}) {
		t.Errorf("criteria = %q; want only keep", got)
	}
	res := build(t, milestone(open(1, "## Out of scope\n### Criteria\n- hidden\n### Edit regions\n- `hidden.go`\n")), Options{})
	is := only(t, res)
	if !reflect.DeepEqual(is.Criteria, []string{"issue 1"}) || len(is.Regions) != 0 {
		t.Errorf("excluded-only body = %+v; want title fallback and no regions", is)
	}
	if got := rules(res.Warnings); !reflect.DeepEqual(got, []string{"criteria_missing"}) {
		t.Fatalf("warnings = %+v; want criteria_missing", res.Warnings)
	}
	for _, want := range []string{"기준", "끝 조건"} {
		if !strings.Contains(res.Warnings[0].Detail, want) {
			t.Errorf("missing-criteria warning lacks %q: %s", want, res.Warnings[0].Detail)
		}
	}
}

func TestInScopeDescendantsAndFencedExclusions(t *testing.T) {
	body := "# Notes\n## 기준\n- before\n" +
		"```markdown\n# Out of scope\n```\n" +
		"### 편집 영역\n- `keep.go`\n### 기준\n- after\n"
	is := only(t, build(t, milestone(open(1, body)), Options{}))
	if !reflect.DeepEqual(is.Criteria, []string{"before", "after"}) ||
		!reflect.DeepEqual(is.Regions, []compile.Region{{Path: "keep.go", Kind: "file"}}) {
		t.Errorf("ordinary parent or fenced exclusion hid in-scope content: %+v", is)
	}
}

func TestReleaseGateCriteriaKindOrder(t *testing.T) {
	body := "## 끝 조건\n- done one\n## Red tests\n- red one\n" +
		"## 정한 답\n- decided one\n## Acceptance\n- done two\n" +
		"## 기준\n- criterion one\n## Criteria\n- criterion two\n" +
		"## Decided answer\n- decided two\n## 먼저 실패하게 쓸 시험\n- red two\n" +
		"## Done when\n- done three\n"
	is := only(t, build(t, milestone(open(1, body)), Options{}))
	want := []string{"criterion one", "criterion two", "decided one", "decided two", "red one", "red two", "done one", "done two", "done three"}
	if !reflect.DeepEqual(is.Criteria, want) {
		t.Errorf("criteria = %q; want kind order %q", is.Criteria, want)
	}
}

func TestLegacyHeadingInputBytes(t *testing.T) {
	// This fixture passes before CRW-1648 and pins the complete serialized input,
	// including the historical order when a done section precedes acceptance criteria.
	body := "# Notes\n\n### **Done when**:\n- done first\n  - detail\n" +
		"\n## Acceptance\n- acceptance second\n\n## Acceptance criteria\n- criterion third\n" +
		"\n## Edit regions\n- `old/path.go`\n\n## Size\nsize_fit: executor\nissue_size: ok\n"
	res := build(t, milestone(open(1, body)), Options{})
	if len(res.Warnings) != 0 {
		t.Fatalf("legacy fixture warnings: %+v", res.Warnings)
	}
	got, err := json.Marshal(res.Input)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"schema":"crw-flow-compile-input/1","projects":[{"key":"o:r:3","issues":[{"key":"o:r:1","title":"issue 1","criteria":["done first detail","acceptance second","criterion third"],"regions":[{"path":"old/path.go","kind":"file"}],"size_fit":"executor","issue_size":"ok"}]}]}`
	if string(got) != want {
		t.Errorf("legacy input bytes changed:\ngot  %s\nwant %s", got, want)
	}
	t.Logf("legacy input bytes: %s", got)
}

func TestSetextScopeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, excluded, boundary string
		resumes                  bool
	}{
		{"peer", "## Out of scope", "In scope\n--------", true},
		{"shallower", "## Out of scope", "In scope\n========", true},
		{"level one peer", "# Out of scope", "In scope\n========", true},
		{"deeper does not resume", "# Out of scope", "In scope\n--------", false},
		{"setext exclusion", "범위 밖\n--------", "In scope\n--------", true},
		{"setext level one exclusion", "Out of scope\n========", "In scope\n--------", false},
		{"decorated and indented", "## Out of scope", "### Notes\n   **In scope**:\n   --------  ", true},
		{"blank after list", "## Out of scope", "- excluded item\n\nIn scope\n--------", true},
		{"after rule", "## Out of scope", "- excluded item\n***\nIn scope\n--------", true},
		{"after fence", "## Out of scope", "```\nexample\n```\nIn scope\n--------", true},
		{"after tilde fence", "## Out of scope", "~~~\nexample\n~~~\nIn scope\n========", true},
	} {
		for _, newline := range []string{"\n", "\r\n"} {
			t.Run(tc.name+"/"+strings.ReplaceAll(newline, "\n", "LF"), func(t *testing.T) {
				body := "## Criteria\n- before\n## Edit regions\n- `before.go`\n\n" +
					tc.excluded + "\n### Criteria\n- hidden\n### Edit regions\n- `hidden.go`\n\n" +
					tc.boundary + "\n### Criteria\n- after\n### Edit regions\n- `after.go`\n"
				body = strings.ReplaceAll(body, "\n", newline)
				res := build(t, milestone(open(1, body)), Options{})
				is := only(t, res)
				wantCriteria := []string{"before"}
				wantRegions := []compile.Region{{Path: "before.go", Kind: "file"}}
				if tc.resumes {
					wantCriteria = append(wantCriteria, "after")
					wantRegions = append(wantRegions, compile.Region{Path: "after.go", Kind: "file"})
				}
				if !reflect.DeepEqual(is.Criteria, wantCriteria) || !reflect.DeepEqual(is.Regions, wantRegions) {
					t.Errorf("scope boundary: criteria=%q regions=%+v; want %q %+v", is.Criteria, is.Regions, wantCriteria, wantRegions)
				}
				if len(res.Warnings) != 0 {
					t.Errorf("warnings = %+v; want none", res.Warnings)
				}
			})
		}
	}
}

func TestSetextDoesNotInventScopeBoundaries(t *testing.T) {
	for _, candidate := range []string{
		"```\nIn scope\n--------\n```",
		"~~~\nIn scope\n========\n~~~",
		"- In scope\n--------",
		"- item\nIn scope\n--------",
		"- item\n\n  In scope\n  --------",
		"1. item\n\n   In scope\n   --------",
		"    In scope\n--------",
		"In scope\n    --------",
		"> In scope\n--------",
		"| In scope |\n--------",
		"--------\n--------",
		"***\n--------",
		"In scope\n\n--------",
	} {
		t.Run(candidate, func(t *testing.T) {
			body := "## Criteria\n- keep\n## Out of scope\n" + candidate +
				"\n### Criteria\n- hidden\n### Edit regions\n- `hidden.go`\n"
			is := only(t, build(t, milestone(open(1, body)), Options{}))
			if !reflect.DeepEqual(is.Criteria, []string{"keep"}) || len(is.Regions) != 0 {
				t.Errorf("not a top-level Setext heading: %+v", is)
			}
		})
	}
}

func TestSetextHeadingsSupplySections(t *testing.T) {
	body := "Criteria\n========\n- required behavior\n\nEdit regions\n--------\n- `keep.go`\n" +
		"\nRed tests\n--------\n- regression case\n\nDone when\n--------\n- tests pass\n"
	res := build(t, milestone(open(1, body)), Options{})
	is := only(t, res)
	if !reflect.DeepEqual(is.Criteria, []string{"required behavior", "regression case", "tests pass"}) ||
		!reflect.DeepEqual(is.Regions, []compile.Region{{Path: "keep.go", Kind: "file"}}) {
		t.Errorf("Setext sections = %+v", is)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %+v; want none", res.Warnings)
	}
}

func TestSetextDetectionKeepsATXAndListData(t *testing.T) {
	body := "## Done when\n- first\n---\n- second\n\n## Edit regions\n- `keep.go`\n"
	is := only(t, build(t, milestone(open(1, body)), Options{}))
	if !reflect.DeepEqual(is.Criteria, []string{"first", "second"}) ||
		!reflect.DeepEqual(is.Regions, []compile.Region{{Path: "keep.go", Kind: "file"}}) {
		t.Errorf("ATX/list data changed: %+v", is)
	}
}
