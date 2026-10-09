package skill

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The release gate: `crw skill issue-ready check` reads one issue and answers whether it may be
// released to a child. An issue that lacks a registered criterion, an edit region, a decided answer,
// a test written to fail first or a done condition is not released, and an issue with a design
// question left is held as design first. The bypass is a record the management session approved.

// readyBody is an issue that carries every item, with the Korean headings of the recorded issues.
const readyBody = "## 배경\n\n하위 에이전트가 설계를 고치며 평가를 반복했다.\n\n" +
	"## 기준\n\n* c1: 준비 항목이 빠진 이슈의 해제가 이유와 함께 거절된다.\n* c2: 설계 먼저 상태가 보인다.\n\n" +
	"## 편집 영역\n\n* `internal/skill/issue_ready.go`\n* `plugins/crw/skills/crw-run/SKILL.md`\n\n" +
	"## 정한 답\n\n해제 판정은 `crw skill issue-ready check`가 한다. 아키텍트 제안과 부모 결정은 이 절에 적는다.\n\n" +
	"## 먼저 실패하게 쓸 시험\n\n* 빈 본문은 not_ready로 나온다.\n* 설계 먼저 라벨은 design_first로 나온다.\n\n" +
	"## 끝 조건\n\n* `go test ./internal/skill -run IssueReady`가 통과한다.\n\n" +
	"## 범위 밖\n\n* 모델 지정\n\n## 열린 결정\n\n없음\n"

func readyIssue(t testing.TB, body string, extra map[string]any) string {
	t.Helper()
	m := map[string]any{"id": "CRW-SYN", "title": "synthetic", "description": body}
	for k, v := range extra {
		m[k] = v
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func readyCall(input string, args ...string) (int, string, string) {
	return call(append([]string{"issue-ready", "check"}, args...), input)
}

func strs(l []any) []string {
	out := make([]string, 0, len(l))
	for _, v := range l {
		out = append(out, v.(string))
	}
	return out
}

func TestIssueReadyAcceptsAnIssueWithEveryItem(t *testing.T) {
	code, out, errOut := readyCall(readyIssue(t, readyBody, nil))
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	r := decodeReport(t, out)
	if at(r, "decision") != "ready" || at(r, "schema") != "crw-issue-ready-check/1" {
		t.Errorf("report: %s", out)
	}
	if len(atList(r, "missing")) != 0 || len(atList(r, "reasons")) != 0 || len(atList(r, "open_questions")) != 0 {
		t.Errorf("a complete issue reports nothing missing: %s", out)
	}
	if got := strs(atList(r, "present")); strings.Join(got, ",") != "criteria,edit_region,decided_answer,red_test,done_condition" {
		t.Errorf("present: %v", got)
	}
}

func TestIssueReadyAcceptsEnglishHeadings(t *testing.T) {
	body := "## Background\n\nx\n\n## Acceptance criteria\n\n- c1: a thing\n\n## Edit regions\n\n- `internal/skill`\n\n" +
		"## Decided answer\n\nThe check is a command.\n\n## Red tests\n\n- the empty body fails\n\n## Done condition\n\n- `go test ./...` passes\n\n## Open decisions\n\nNone.\n"
	code, out, _ := readyCall(readyIssue(t, body, nil))
	if code != 0 || at(decodeReport(t, out), "decision") != "ready" {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestIssueReadyNamesEachMissingItem(t *testing.T) {
	cut := func(heading string) string {
		start := strings.Index(readyBody, "## "+heading)
		if start < 0 {
			t.Fatalf("no heading %q", heading)
		}
		end := strings.Index(readyBody[start+3:], "\n## ")
		return readyBody[:start] + readyBody[start+3+end+1:]
	}
	for _, c := range []struct{ name, heading, withoutPaths string }{
		{"criteria", "기준", ""},
		{"decided_answer", "정한 답", ""},
		{"red_test", "먼저 실패하게 쓸 시험", ""},
		{"done_condition", "끝 조건", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := readyCall(readyIssue(t, cut(c.heading), nil))
			if code != 1 || errOut != "" {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			r := decodeReport(t, out)
			if at(r, "decision") != "not_ready" {
				t.Errorf("decision: %s", out)
			}
			if got := strs(atList(r, "missing")); len(got) != 1 || got[0] != c.name {
				t.Errorf("missing: %v", got)
			}
			reasons := strs(atList(r, "reasons"))
			if len(reasons) != 1 || !strings.Contains(reasons[0], c.name) {
				t.Errorf("the reason names the item: %v", reasons)
			}
		})
	}
	t.Run("edit_region", func(t *testing.T) {
		body := cut("편집 영역")
		body = strings.ReplaceAll(body, "`internal/skill/issue_ready.go`", "x")
		body = strings.ReplaceAll(body, "`plugins/crw/skills/crw-run/SKILL.md`", "y")
		body = strings.ReplaceAll(body, "해제 판정은 `crw skill issue-ready check`가 한다.", "해제 판정은 명령이 한다.")
		_, out, _ := readyCall(readyIssue(t, body, nil))
		r := decodeReport(t, out)
		if got := strs(atList(r, "missing")); len(got) != 1 || got[0] != "edit_region" {
			t.Errorf("missing: %v: %s", got, out)
		}
	})
}

func TestIssueReadyTakesAPathNamedElsewhereAsTheEditRegion(t *testing.T) {
	start := strings.Index(readyBody, "## 편집 영역")
	end := strings.Index(readyBody, "## 정한 답")
	body := readyBody[:start] + readyBody[end:]
	body = strings.Replace(body, "해제 판정은", "`internal/skill/issue_ready.go`에서 해제 판정은", 1)
	code, out, _ := readyCall(readyIssue(t, body, nil))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestIssueReadyNamesEveryMissingItemOfAnEmptyBody(t *testing.T) {
	code, out, _ := readyCall(readyIssue(t, "## 배경\n\n한 문단 요청.\n", nil))
	r := decodeReport(t, out)
	if code != 1 || at(r, "decision") != "not_ready" {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got := strings.Join(strs(atList(r, "missing")), ","); got != "criteria,edit_region,decided_answer,red_test,done_condition" {
		t.Errorf("missing: %s", got)
	}
	if len(atList(r, "reasons")) != 5 {
		t.Errorf("one reason per item: %s", out)
	}
}

func TestIssueReadyDoneConditionNeedsACommand(t *testing.T) {
	body := strings.Replace(readyBody, "* `go test ./internal/skill -run IssueReady`가 통과한다.", "* 잘 동작한다.", 1)
	code, out, _ := readyCall(readyIssue(t, body, nil))
	r := decodeReport(t, out)
	if code != 1 || strings.Join(strs(atList(r, "missing")), ",") != "done_condition" {
		t.Fatalf("exit %d: %s", code, out)
	}
	fenced := strings.Replace(readyBody, "* `go test ./internal/skill -run IssueReady`가 통과한다.", "통과해야 한다:\n\n```sh\ngo test ./internal/skill\n```", 1)
	if code, out, _ := readyCall(readyIssue(t, fenced, nil)); code != 0 {
		t.Fatalf("a fenced command is a command: exit %d: %s", code, out)
	}
}

func TestIssueReadyPlaceholderIsNotAnItem(t *testing.T) {
	body := strings.Replace(readyBody, "해제 판정은 `crw skill issue-ready check`가 한다. 아키텍트 제안과 부모 결정은 이 절에 적는다.", "TBD", 1)
	code, out, _ := readyCall(readyIssue(t, body, nil))
	r := decodeReport(t, out)
	if code != 1 || strings.Join(strs(atList(r, "missing")), ",") != "decided_answer" {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestIssueReadyHoldsADesignFirstLabel(t *testing.T) {
	for _, labels := range []any{[]string{"codex-relay-workflow", "설계 먼저"}, []map[string]string{{"name": "설계 먼저"}}, []string{"Design-First"}} {
		code, out, errOut := readyCall(readyIssue(t, readyBody, map[string]any{"labels": labels}))
		if code != 1 || errOut != "" {
			t.Fatalf("%v: exit %d: %s%s", labels, code, out, errOut)
		}
		r := decodeReport(t, out)
		if at(r, "decision") != "design_first" || at(r, "design_first") != true {
			t.Errorf("%v: %s", labels, out)
		}
		reasons := strings.Join(strs(atList(r, "reasons")), "\n")
		if !strings.Contains(reasons, "design first") || !strings.Contains(reasons, "decided answer") {
			t.Errorf("the reason says what to do: %s", reasons)
		}
	}
	if code, out, _ := readyCall(readyIssue(t, readyBody, map[string]any{"labels": []string{"codex-relay-workflow"}})); code != 0 {
		t.Fatalf("other labels do not hold: exit %d: %s", code, out)
	}
}

func TestIssueReadyHoldsAnOpenDesignQuestion(t *testing.T) {
	body := strings.Replace(readyBody, "## 열린 결정\n\n없음\n", "## 열린 결정\n\n* 판정을 relay 해제 거절로 할지, 스킬 규칙과 점검으로 할지.\n* 라벨 이름.\n", 1)
	code, out, _ := readyCall(readyIssue(t, body, nil))
	r := decodeReport(t, out)
	if code != 1 || at(r, "decision") != "design_first" {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got := strs(atList(r, "open_questions")); len(got) != 2 || !strings.HasPrefix(got[0], "판정을 relay") {
		t.Errorf("open questions: %v", got)
	}
	for _, none := range []string{"없음", "없음.", "- 없음", "None", "n/a", "해당 없음", ""} {
		b := strings.Replace(readyBody, "## 열린 결정\n\n없음\n", "## 열린 결정\n\n"+none+"\n", 1)
		if code, out, _ := readyCall(readyIssue(t, b, nil)); code != 0 {
			t.Errorf("open decisions %q hold nothing: exit %d: %s", none, code, out)
		}
	}
}

func TestIssueReadyReportsDesignFirstAndMissingTogether(t *testing.T) {
	code, out, _ := readyCall(readyIssue(t, "## 열린 결정\n\n* 무엇을 할지\n", nil))
	r := decodeReport(t, out)
	if code != 1 || at(r, "decision") != "design_first" || len(atList(r, "missing")) != 5 {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestIssueReadyBypassNeedsTheManagementApprovalRecord(t *testing.T) {
	exception := map[string]any{"issue": "CRW-SYN", "approved_by": "management session", "approved_on": "2026-10-09", "statement": "a one-line fix released to land before the freeze"}
	body := "## 배경\n\n한 문단.\n"
	code, out, errOut := readyCall(readyIssue(t, body, map[string]any{"exception": exception}))
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	r := decodeReport(t, out)
	if at(r, "decision") != "bypassed" || len(atList(r, "missing")) != 5 {
		t.Errorf("a bypass still reports what is missing: %s", out)
	}
	rec, _ := at(r, "exception_record").(string)
	for _, want := range []string{"CRW-SYN", "criteria", "done_condition", "management session", "2026-10-09", "one-line fix"} {
		if !strings.Contains(rec, want) {
			t.Errorf("the record lacks %q: %s", want, rec)
		}
	}
	for name, bad := range map[string]map[string]any{
		"no approver":     {"issue": "CRW-SYN", "approved_by": " ", "approved_on": "2026-10-09", "statement": "s"},
		"other issue":     {"issue": "CRW-OTHER", "approved_by": "m", "approved_on": "2026-10-09", "statement": "s"},
		"bad date":        {"issue": "CRW-SYN", "approved_by": "m", "approved_on": "10-09", "statement": "s"},
		"empty statement": {"issue": "CRW-SYN", "approved_by": "m", "approved_on": "2026-10-09", "statement": ""},
	} {
		if code, out, errOut := readyCall(readyIssue(t, body, map[string]any{"exception": bad})); code != 2 || !strings.Contains(errOut, "exception") {
			t.Errorf("%s: exit %d: %s%s", name, code, out, errOut)
		}
	}
	// A design first issue is bypassed the same way.
	if code, out, _ := readyCall(readyIssue(t, readyBody, map[string]any{"labels": []string{"설계 먼저"}, "exception": exception})); code != 0 || at(decodeReport(t, out), "decision") != "bypassed" {
		t.Errorf("design first bypass: exit %d: %s", code, out)
	}
	// An issue that is ready needs no bypass and reports none.
	code, out, _ = readyCall(readyIssue(t, readyBody, map[string]any{"exception": exception}))
	if code != 0 || at(decodeReport(t, out), "decision") != "ready" || at(decodeReport(t, out), "exception_record") != nil {
		t.Errorf("ready with an exception: exit %d: %s", code, out)
	}
}

func TestIssueReadyReadsNothingInsideACodeFence(t *testing.T) {
	body := "## 배경\n\n```\n## 정한 답\n\nfake\n```\n"
	_, out, _ := readyCall(readyIssue(t, body, nil))
	if got := strs(atList(decodeReport(t, out), "missing")); !strings.Contains(strings.Join(got, ","), "decided_answer") {
		t.Errorf("a heading in a fence is no section: %v", got)
	}
}

func TestIssueReadyIsDeterministicAndRejectsUnreadableInput(t *testing.T) {
	in := readyIssue(t, "## 배경\n\nx\n", nil)
	_, first, _ := readyCall(in)
	_, second, _ := readyCall(in)
	if first != second || first == "" {
		t.Errorf("the same issue is the same bytes:\n%s\n%s", first, second)
	}
	for name, input := range map[string]string{
		"not json":       "{",
		"unclosed fence": readyIssue(t, "## 정한 답\n\n```\nx\n", nil),
		"not utf8":       "\xff",
		"bad labels":     readyIssue(t, readyBody, map[string]any{"labels": 3}),
	} {
		if code, out, errOut := readyCall(input); code != 2 || out != "" || !strings.Contains(errOut, "Unreadable issue") {
			t.Errorf("%s: exit %d: %s%s", name, code, out, errOut)
		}
	}
	if code, _, errOut := readyCall("", filepath.Join(t.TempDir(), "missing.json")); code != 3 || !strings.Contains(errOut, "Nothing was decided") {
		t.Errorf("missing file: exit %d: %s", code, errOut)
	}
	file := filepath.Join(t.TempDir(), "issue.json")
	if err := os.WriteFile(file, []byte(readyIssue(t, readyBody, nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := readyCall("", file); code != 0 {
		t.Errorf("file input: exit %d: %s", code, out)
	}
}

// The issue that asked for the gate, as Linear returned it, is itself not ready: it carries none of
// the items and an open decision. The gate holds it as design first.
func TestIssueReadyHoldsTheIssueThatAskedForTheGate(t *testing.T) {
	body := "## 배경\n\n* 10-08 자식 종결에서 취소한 노드 다수가 평가를 10~19번 돌며 같은 부류 결함을 되풀이했다.\n\n" +
		"## 바라는 동작\n\n* 해제 전에 이슈 준비를 판정한다.\n\n## 기준\n\n* c1: 준비 항목이 빠진 이슈의 해제가 이유와 함께 거절된다.\n* c2: \"설계 먼저\" 상태가 Linear에서 보인다.\n\n" +
		"## 범위 밖\n\n* 하위 에이전트의 모델·역할 지정(CXC 몫).\n\n## 열린 결정\n\n* 판정을 relay 해제 거절로 할지, 스킬 규칙과 crw-tidy 점검으로 할지.\n"
	code, out, _ := readyCall(readyIssue(t, body, map[string]any{"id": "CRW-1039", "labels": []string{"codex-relay-workflow"}}))
	r := decodeReport(t, out)
	if code != 1 || at(r, "decision") != "design_first" || strings.Join(strs(atList(r, "missing")), ",") != "edit_region,decided_answer,red_test,done_condition" {
		t.Fatalf("exit %d: %s", code, out)
	}
}

// Every heading the reasons tell the author to write (the quoted words of the hint) is read as the item
// it supplies.
func TestIssueReadyHeadingsNamedInReasonsAreRead(t *testing.T) {
	quoted := regexp.MustCompile(`'([^']+)'`)
	for item, hint := range readyHeadingHint {
		titles := quoted.FindAllStringSubmatch(hint, -1)
		if len(titles) != 2 {
			t.Errorf("%s: the hint quotes %d headings, want the Korean and the English: %s", item, len(titles), hint)
		}
		for _, m := range titles {
			title := strings.TrimPrefix(m[1], "## ")
			if got := readyHeadingKind(title); got != item {
				t.Errorf("heading %q is read as %q, want %q", title, got, item)
			}
		}
	}
}

// The two required checks run one after the other on the same Linear record (crw-run), so a body the
// add-issue skill writes must pass both reads: the criteria heading is one both know.
func TestIssueReadyAndSizeReadTheSameCriteriaHeading(t *testing.T) {
	in := readyIssue(t, readyBody, nil)
	if code, out, errOut := readyCall(in); code != 0 {
		t.Fatalf("issue-ready: exit %d: %s%s", code, out, errOut)
	}
	code, out, errOut := sizeCall(in)
	if code == 2 || strings.Contains(errOut, "no completion criteria") {
		t.Fatalf("issue-size cannot read the criteria heading issue-ready accepts: exit %d: %s%s", code, out, errOut)
	}
	if got := atNum(t, decodeReport(t, out), "signals", "criteria"); got != 2 {
		t.Errorf("issue-size counts %d criteria, want 2: %s", got, out)
	}
	english := "## Criteria\n\n- c1: a thing\n- c2: another\n"
	code, out, errOut = sizeCall(readyIssue(t, english, nil))
	if code == 2 || atNum(t, decodeReport(t, out), "signals", "criteria") != 2 {
		t.Errorf("the English 'Criteria' heading: exit %d: %s%s", code, out, errOut)
	}
}

// The edit-region section supplies the item only with a path the repository could have: a sentence
// with no path in backticks names no region.
func TestIssueReadyEditRegionNeedsAPath(t *testing.T) {
	body := strings.Replace(readyBody, "* `internal/skill/issue_ready.go`\n* `plugins/crw/skills/crw-run/SKILL.md`", "* 관련 파일 몇 개\n* related files", 1)
	code, out, _ := readyCall(readyIssue(t, body, nil))
	r := decodeReport(t, out)
	if code != 1 || strings.Join(strs(atList(r, "missing")), ",") != "edit_region" || len(atList(r, "observed", "edit_regions")) != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
}

// What a section about what stays out does not count as an edit region, in any spelling of its heading.
func TestIssueReadyOutOfScopePathsAreNoEditRegion(t *testing.T) {
	start := strings.Index(readyBody, "## 편집 영역")
	end := strings.Index(readyBody, "## 정한 답")
	for _, heading := range []string{"## Out-of-scope", "## Out of scope", "## 범위 밖", "## Non-goals", "## Non goals"} {
		body := readyBody[:start] + readyBody[end:] + "\n" + heading + "\n\n* `internal/relay/`\n"
		code, out, _ := readyCall(readyIssue(t, body, nil))
		r := decodeReport(t, out)
		if code != 1 || strings.Join(strs(atList(r, "missing")), ",") != "edit_region" || len(atList(r, "observed", "edit_regions")) != 0 {
			t.Errorf("%s: exit %d: %s", heading, code, out)
		}
	}
}

// An entry is a none only when it says nothing is open; a question that starts with a none word is a
// question.
func TestIssueReadyOpenDecisionStartingWithANoneWordIsAQuestion(t *testing.T) {
	for _, q := range []string{
		"* None of the providers supports CAS; which fallback should we choose?",
		"* No retry budget is defined; pick one",
		"* N/A for the cache, but the eviction order is open",
		"* 없다고 가정해도 되는가?",
		"* 없음 처리할지 오류로 볼지 정해야 한다",
		"* Nothing in the spec says who owns the lock",
	} {
		b := strings.Replace(readyBody, "## 열린 결정\n\n없음\n", "## 열린 결정\n\n"+q+"\n", 1)
		code, out, _ := readyCall(readyIssue(t, b, nil))
		r := decodeReport(t, out)
		if code != 1 || at(r, "decision") != "design_first" || len(atList(r, "open_questions")) != 1 {
			t.Errorf("%q: exit %d: %s", q, code, out)
		}
	}
	for _, none := range []string{"None (everything is in the decided answer)", "없음 (정한 답에 반영)", "No open decisions.", "없습니다.", "Nothing."} {
		b := strings.Replace(readyBody, "## 열린 결정\n\n없음\n", "## 열린 결정\n\n"+none+"\n", 1)
		if code, out, _ := readyCall(readyIssue(t, b, nil)); code != 0 {
			t.Errorf("open decisions %q hold nothing: exit %d: %s", none, code, out)
		}
	}
}

// A done condition written only as a block (the command and its expected result) is a done condition.
func TestIssueReadyDoneConditionOnlyInACodeBlock(t *testing.T) {
	block := "## 끝 조건\n\n```sh\ngo test ./internal/skill -run IssueReady\n# Expected: all tests pass, exit 0\n```\n\n"
	start := strings.Index(readyBody, "## 끝 조건")
	end := strings.Index(readyBody, "## 범위 밖")
	body := readyBody[:start] + block + readyBody[end:]
	if code, out, _ := readyCall(readyIssue(t, body, nil)); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	// A block with a heading-shaped line is still no new section, and an empty block is no condition.
	empty := readyBody[:start] + "## 끝 조건\n\n```sh\n```\n\n" + readyBody[end:]
	code, out, _ := readyCall(readyIssue(t, empty, nil))
	if code != 1 || strings.Join(strs(atList(decodeReport(t, out), "missing")), ",") != "done_condition" {
		t.Errorf("an empty block: exit %d: %s", code, out)
	}
}
