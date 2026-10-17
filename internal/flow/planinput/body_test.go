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

func TestSetextContextKeepsExcludedContents(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"whole title", "Out of scope\n(research)\n---"},
		{"split scope words", "Out of\nscope\n(research)\n---"},
		{"list new paragraph", "- item\n\n  continued paragraph\nIn scope\n---"},
		{"numbered list new paragraph", "1. item\n\n   continued paragraph\nIn scope\n---"},
		{"quote continuation", "> example\nIn scope\n---"},
		{"quote multiple continuations", "> example\ncontinued paragraph\nIn scope\n---"},
		{"comment", "<!--\nIn scope\n---\n-->"},
		{"comment with blank and fence", "<!--\n\n```\nIn scope\n---\n-->"},
		{"comment with ATX", "<!--\n## In scope\n-->"},
		{"single line comment", "<!-- In scope -->\n---"},
	} {
		for _, resume := range []bool{false, true} {
			name := tc.name + "/excluded"
			if resume {
				name = tc.name + "/resume"
			}
			t.Run(name, func(t *testing.T) {
				body := "## Criteria\n- keep\n## Edit regions\n- `keep.go`\n## Out of scope\n" +
					tc.body + "\n### Criteria\n- hidden\n### Edit regions\n- `hidden.go`\n"
				wantCriteria := []string{"keep"}
				wantRegions := []compile.Region{{Path: "keep.go", Kind: "file"}}
				if resume {
					body += "\nBack\nin scope\n---\n### Criteria\n- after\n### Edit regions\n- `after.go`\n"
					wantCriteria = append(wantCriteria, "after")
					wantRegions = append(wantRegions, compile.Region{Path: "after.go", Kind: "file"})
				}
				res := build(t, milestone(open(1, body)), Options{})
				is := only(t, res)
				if !reflect.DeepEqual(is.Criteria, wantCriteria) || !reflect.DeepEqual(is.Regions, wantRegions) {
					t.Errorf("context: criteria=%q regions=%+v; want %q %+v", is.Criteria, is.Regions, wantCriteria, wantRegions)
				}
				if len(res.Warnings) != 0 {
					t.Errorf("warnings = %+v; want none", res.Warnings)
				}
			})
		}
	}
}

func TestSetextContextEndsAtRealBlockBoundaries(t *testing.T) {
	for _, preamble := range []string{
		"Out of scope prose\n\n",
		"- item\n\n  continued paragraph\n\n",
		"- item\n\n  continued paragraph\n***\n",
		"> quote\n\n",
		"> quote\n>\n",
		"> quote\n***\n",
		"<!--\nIn scope\n---\n-->\n",
	} {
		t.Run(preamble, func(t *testing.T) {
			body := "## Out of scope\n" + preamble +
				"In scope\n---\n### Criteria\n- recovered\n### Edit regions\n- `recovered.go`\n"
			is := only(t, build(t, milestone(open(1, body)), Options{}))
			if !reflect.DeepEqual(is.Criteria, []string{"recovered"}) ||
				!reflect.DeepEqual(is.Regions, []compile.Region{{Path: "recovered.go", Kind: "file"}}) {
				t.Errorf("context did not end at block boundary: %+v", is)
			}
		})
	}
}

func TestSetextContextReadsWholeTitleAndFlushesBody(t *testing.T) {
	body := "Acceptance\ncriteria\n---\n- required behavior\n\nEdit\nregions\n---\n- `keep.go`"
	is := only(t, build(t, milestone(open(1, body)), Options{}))
	if !reflect.DeepEqual(is.Criteria, []string{"required behavior"}) ||
		!reflect.DeepEqual(is.Regions, []compile.Region{{Path: "keep.go", Kind: "file"}}) {
		t.Errorf("whole Setext titles or final body not read: %+v", is)
	}
	body = "## Done when\n- first\n  detail\n\nordinary\nparagraph\n\n- final\n  continuation"
	is = only(t, build(t, milestone(open(1, body)), Options{}))
	if !reflect.DeepEqual(is.Criteria, []string{"first detail", "final continuation"}) {
		t.Errorf("non-heading body changed: %q", is.Criteria)
	}
}

func TestHeadingBlockTransitions(t *testing.T) {
	const kept = "### Criteria\n- keep\n### Edit regions\n- `keep.go`\n"
	const hidden = "### Criteria\n- hidden\n### Edit regions\n- `hidden.go`\n"
	for _, tc := range []struct{ name, body string }{
		{"comment data", "## Criteria\n- keep\n<!--\n## Notes\n- hidden\n-->\n" +
			"## Edit regions\n- `keep.go`\n<!--\n## Notes\n- `hidden.go`\n-->\n"},
		{"comment without headings", kept + "<!--\n- `hidden.go`\n-->\n"},
		{"unclosed comment", kept + "<!--\n" + hidden},
		{"comment closes and reopens", kept + "<!-- first --> <!-- second\n" + hidden + "-->\n"},
		{"reference definitions", "[spec]: https://example.test/spec\nCriteria\n---\n- keep\n\n" +
			"[edit]: /edit\nEdit regions\n---\n- `keep.go`\n"},
		{"reference destination and title lines", "[spec]:\n  <https://example.test/spec>\n  \"a title\"\nCriteria\n---\n- keep\n\n" +
			"[edit]: /edit(a) 'edit title'\nEdit regions\n---\n- `keep.go`\n"},
		{"reference multiline title", "[spec]: /spec \"a title\ncontinued\"\nCriteria\n---\n- keep\n\n" +
			"## Edit regions\n- `keep.go`\n"},
		{"reference title stops at blank", "## Out of scope\n[spec]: /spec \"unfinished\n\nIn scope\n---\n" + kept + "\nend\"\n"},
		{"reference does not interrupt paragraph", kept + "## Out of scope\nOut of scope\n[spec]: /spec\n(research)\n---\n" + hidden},
		{"invalid reference remains title text", kept + "## Out of scope\n[Out of scope]: /unbalanced(\n(research)\n---\n" + hidden},
		{"quoted code", "## Out of scope\n>     example\nIn scope\n---\n" + kept},
		{"quoted code with tabs", "## Out of scope\n>\t  example\nIn scope\n---\n" + kept},
		{"nested quoted code", "## Out of scope\n> >     example\nIn scope\n---\n" + kept},
		{"indented quote continuation", kept + "## Out of scope\n> paragraph\n>     continuation\nIn scope\n---\n" + hidden},
		{"raw HTML ends quote", "## Out of scope\n> quote\n<script></script>\nIn scope\n---\n" + kept},
		{"raw HTML hides data and fences", "<SCRIPT>\n```\n" + hidden + "</SCRIPT>\n" + kept},
		{"HTML block ends at blank", "## Out of scope\n> quote\n<div>\n" + hidden + "</div>\n\nIn scope\n---\n" + kept},
		{"HTML block stays open until blank", kept + "<div>\n" + hidden + "</div>\n" + hidden + "\n"},
		{"processing instruction", "<?instruction\n" + hidden + "?>\n" + kept},
		{"declaration", "<!DOCTYPE\n" + hidden + ">\n" + kept},
		{"CDATA", "<![CDATA[\n" + hidden + "]]>\n" + kept},
		{"complete custom tag", "<custom attr=\"value\">\n" + hidden + "\n" + kept},
		{"inline tag does not interrupt quote", kept + "## Out of scope\n> quote\n<span>\nIn scope\n---\n" + hidden},
		{"quoted HTML is not a paragraph", "## Out of scope\n> <script></script>\nIn scope\n---\n" + kept},
	} {
		for _, ending := range []struct{ name, value string }{{"LF", "\n"}, {"CRLF", "\r\n"}} {
			t.Run(tc.name+"/"+ending.name, func(t *testing.T) {
				body := strings.ReplaceAll(tc.body, "\n", ending.value)
				res := build(t, milestone(open(1, body)), Options{})
				is := only(t, res)
				if !reflect.DeepEqual(is.Criteria, []string{"keep"}) ||
					!reflect.DeepEqual(is.Regions, []compile.Region{{Path: "keep.go", Kind: "file"}}) {
					t.Errorf("block transition: criteria=%q regions=%+v; want keep and keep.go", is.Criteria, is.Regions)
				}
				if len(res.Warnings) != 0 {
					t.Errorf("warnings = %+v; want none", res.Warnings)
				}
			})
		}
	}
}

func TestReferenceAndContainerBoundaries(t *testing.T) {
	const kept = "## Criteria\n- keep\n## Edit regions\n- `keep.go`\n"
	const hidden = "### Criteria\n- hidden\n### Edit regions\n- `hidden.go`\n"
	for _, tc := range []struct{ name, body string }{
		{"reference title ATX", "[spec]: /spec \"start\n## Criteria\n- keep\nend\"\n## Edit regions\n- `keep.go`\n"},
		{"reference label ATX", "[spec\n## Criteria\n- keep\nend]: /spec\n## Edit regions\n- `keep.go`\n"},
		{"reference destination ATX", kept + "\n[spec]:\n## \"Out of scope\"\n" + hidden},
		{"reference cannot hide exclusion", kept + "\n[spec]: /spec \"start\n## Out of scope\nend\"\n" + hidden},
		{"valid multiline reference", "[long\nlabel]: /spec \"first\nsecond\"\n" + kept},
		{"fragment destination is not ATX", "[spec]:\n#fragment\nCriteria\n---\n- keep\n## Edit regions\n- `keep.go`\n"},
		{"list HTML exits at ATX", "- item\n  <div>\n  example\n" + kept},
		{"list HTML exits at Setext", "- item\n  <div>\n  example\nCriteria\n---\n- keep\n## Edit regions\n- `keep.go`\n"},
		{"numbered list HTML", "1. item\n   <div>\n   example\n" + kept},
		{"list raw HTML", "- item\n  <script>\n  example\n" + kept},
		{"list HTML stays inside", "- item\n  <div>\n  ### Criteria\n  - hidden\n  ### Edit regions\n  - `hidden.go`\n" + kept},
		{"root HTML has no list owner", "  <div>\n" + hidden + "\n" + kept},
		{"leaving list does not end exclusion", kept + "## Out of scope\n- item\n  <div>\n  example\n" + hidden},
		{"indented HTML continues quote", kept + "## Out of scope\n> quote\n>     <div>\nIn scope\n---\n" + hidden},
		{"indented fence continues quote", kept + "## Out of scope\n> quote\n>     ```\nIn scope\n---\n" + hidden},
		{"indented ATX continues quote", kept + "## Out of scope\n> quote\n>     ## Notes\nIn scope\n---\n" + hidden},
		{"new nested quote code is not continuation", "## Out of scope\n> quote\n> >     <div>\nIn scope\n---\n" + kept},
		{"shallower quote retains paragraph", kept + "## Out of scope\n> > quote\n>     <div>\nIn scope\n---\n" + hidden},
	} {
		for _, ending := range []struct{ name, value string }{{"LF", "\n"}, {"CRLF", "\r\n"}} {
			t.Run(tc.name+"/"+ending.name, func(t *testing.T) {
				res := build(t, milestone(open(1, strings.ReplaceAll(tc.body, "\n", ending.value))), Options{})
				is := only(t, res)
				if !reflect.DeepEqual(is.Criteria, []string{"keep"}) ||
					!reflect.DeepEqual(is.Regions, []compile.Region{{Path: "keep.go", Kind: "file"}}) {
					t.Errorf("boundary: criteria=%q regions=%+v; want keep and keep.go", is.Criteria, is.Regions)
				}
				if len(res.Warnings) != 0 {
					t.Errorf("warnings = %+v; want none", res.Warnings)
				}
			})
		}
	}
}

func TestLazyQuoteDepth(t *testing.T) {
	for _, quote := range []string{
		"> > paragraph\n>     continuation",
		"> > paragraph\n>     continuation\n> >     <div>",
		"> > > paragraph\n> >     continuation\n>     more\n> > >     <div>",
	} {
		t.Run(quote, func(t *testing.T) {
			body := "## Criteria\n- keep\n## Edit regions\n- `keep.go`\n## Out of scope\n" +
				quote + "\nIn scope\n---\n### Criteria\n- hidden\n### Edit regions\n- `hidden.go`\n"
			is := only(t, build(t, milestone(open(1, body)), Options{}))
			if !reflect.DeepEqual(is.Criteria, []string{"keep"}) ||
				!reflect.DeepEqual(is.Regions, []compile.Region{{Path: "keep.go", Kind: "file"}}) {
				t.Errorf("lazy quote released exclusion: %+v", is)
			}
		})
	}
}

func TestReferenceTitleIndentedFences(t *testing.T) {
	for _, indent := range []string{"    ", "        ", "\t"} {
		for _, fence := range []string{"```", "~~~"} {
			t.Run(indent+fence, func(t *testing.T) {
				body := "[spec]: /spec \"start\n" + indent + fence + "\nend\"\n" +
					"Criteria\n---\n- keep\n\nEdit regions\n---\n- `keep.go`\n"
				res := build(t, milestone(open(1, body)), Options{})
				is := only(t, res)
				if !reflect.DeepEqual(is.Criteria, []string{"keep"}) ||
					!reflect.DeepEqual(is.Regions, []compile.Region{{Path: "keep.go", Kind: "file"}}) ||
					len(res.Warnings) != 0 {
					t.Errorf("indented reference text swallowed sections: issue=%+v warnings=%+v", is, res.Warnings)
				}
			})
		}
	}
}

func TestFenceIndentation(t *testing.T) {
	for _, indent := range []string{"", "   "} {
		for _, fence := range []string{"```", "~~~"} {
			t.Run(indent+fence, func(t *testing.T) {
				body := "## Criteria\n- keep\n" + indent + fence + "\n    " + fence +
					"\n## Criteria\n- hidden\n" + indent + fence +
					"\n## Edit regions\n- `keep.go`\n"
				is := only(t, build(t, milestone(open(1, body)), Options{}))
				if !reflect.DeepEqual(is.Criteria, []string{"keep"}) ||
					!reflect.DeepEqual(is.Regions, []compile.Region{{Path: "keep.go", Kind: "file"}}) {
					t.Errorf("four-space marker closed a fence: %+v", is)
				}
			})
		}
	}
}
