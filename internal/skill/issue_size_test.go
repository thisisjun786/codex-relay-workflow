package skill

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The size check: reads an issue, counts the signals, answers ok or split_recommended and, for the
// second, drafts a split. The tests go through Run (the call helper), the way crw-plan and crw-run
// invoke the command.

const issueSizeFixtures = "testdata/fixtures/issue-size"

// sizeCall runs `crw skill issue-size check` with input on stdin.
func sizeCall(input string, args ...string) (int, string, string) {
	return call(append([]string{"issue-size", "check"}, args...), input)
}

// fixtureIssue is the recorded issue id as the JSON the command reads: a Linear-shaped object.
func fixtureIssue(t testing.TB, id string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(issueSizeFixtures, id+".md"))
	if err != nil {
		t.Fatal(err)
	}
	title, body, _ := strings.Cut(string(raw), "\n")
	out, err := json.Marshal(map[string]string{"id": id, "title": strings.TrimPrefix(title, "# "), "description": body})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// structuredIssue is an issue given as fields: c completion criteria and r research-reinforcement ones.
func structuredIssue(id string, c, r int, extra map[string]any) string {
	item := func(kind string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s criterion %d", kind, i+1)
		}
		return out
	}
	m := map[string]any{"id": id, "title": "synthetic " + id, "criteria": item("completion", c)}
	if r > 0 {
		m["research_criteria"] = item("research", r)
	}
	for k, v := range extra {
		m[k] = v
	}
	out, _ := json.Marshal(m)
	return string(out)
}

// descriptionIssue is an issue given as a markdown body.
func descriptionIssue(body string) string {
	out, _ := json.Marshal(map[string]string{"id": "CRW-SYN", "title": "synthetic", "description": body})
	return string(out)
}

func decodeReport(t testing.TB, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("the report is not JSON (%v):\n%s", err, out)
	}
	return m
}

// at reads a path of keys out of a decoded report; a missing key is nil.
func at(m map[string]any, path ...string) any {
	var v any = m
	for _, k := range path {
		o, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = o[k]
	}
	return v
}

func atNum(t testing.TB, m map[string]any, path ...string) int {
	t.Helper()
	f, ok := at(m, path...).(float64)
	if !ok {
		t.Fatalf("%v is not a number in %v", path, m)
	}
	return int(f)
}

func atList(m map[string]any, path ...string) []any {
	l, _ := at(m, path...).([]any)
	return l
}

func sizeSignals(t testing.TB, input string) (criteria, research int) {
	t.Helper()
	code, out, errOut := sizeCall(input)
	if code != 0 && code != 1 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	r := decodeReport(t, out)
	return atNum(t, r, "signals", "criteria"), atNum(t, r, "signals", "research_criteria")
}

func TestIssueSizeIsDeterministic(t *testing.T) {
	for _, id := range []string{"CRW-184", "CRW-183", "CRW-265"} {
		input := fixtureIssue(t, id)
		code1, out1, err1 := sizeCall(input)
		code2, out2, err2 := sizeCall(input)
		if out1 == "" || out1 != out2 || code1 != code2 || err1 != err2 {
			t.Errorf("%s: two runs on one input differ: exit %d/%d\n%s\n---\n%s", id, code1, code2, out1, out2)
		}
	}
}

func TestIssueSizeAnswersOkAndSplitRecommended(t *testing.T) {
	for _, test := range []struct {
		id         string
		exit       int
		decision   string
		assignable bool
	}{
		{"CRW-265", 0, "ok", true},
		{"CRW-184", 1, "split_recommended", false},
		{"CRW-183", 1, "split_recommended", false},
	} {
		code, out, errOut := sizeCall(fixtureIssue(t, test.id))
		if code != test.exit || errOut != "" {
			t.Fatalf("%s: exit %d, stderr %q\n%s", test.id, code, errOut, out)
		}
		r := decodeReport(t, out)
		if at(r, "schema") != "crw-issue-size-check/1" || at(r, "issue") != test.id || at(r, "decision") != test.decision || at(r, "assignable") != test.assignable {
			t.Errorf("%s: %s", test.id, out)
		}
		reasons := atList(r, "reasons")
		if (test.decision == "ok") != (len(reasons) == 0) {
			t.Errorf("%s: reasons %v do not match the decision %s", test.id, reasons, test.decision)
		}
		if _, has := r["proposal"]; has != (test.decision == "split_recommended") {
			t.Errorf("%s: proposal present = %v for %s", test.id, has, test.decision)
		}
		if atNum(t, r, "limits", "criteria_total") <= 0 || atNum(t, r, "limits", "research_criteria") <= 0 || atNum(t, r, "limits", "deliverables") <= 0 {
			t.Errorf("%s: the report does not print the limits it applied: %s", test.id, out)
		}
	}
}

// The recorded report for CRW-184 is the draft the pull request shows. It is a golden under another
// name: CRW_GOLDEN=update does not touch it. When the proposal rules change on purpose, regenerate it
// by hand (crw skill issue-size check < the CRW-184 input), review the diff and say why in the PR.
func TestIssueSizeReportForCRW184IsTheRecordedDraft(t *testing.T) {
	_, out, _ := sizeCall(fixtureIssue(t, "CRW-184"))
	want, err := os.ReadFile(filepath.Join(issueSizeFixtures, "CRW-184.report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("the CRW-184 report changed:\n%s", out)
	}
}

func TestIssueSizeLimitsAreInclusive(t *testing.T) {
	for _, test := range []struct {
		c, r    int
		exit    int
		reasons int
	}{
		{8, 0, 0, 0}, {9, 0, 1, 1}, {5, 3, 0, 0}, {1, 3, 0, 0}, {1, 4, 1, 1}, {9, 4, 1, 2}, {1, 0, 0, 0},
	} {
		code, out, errOut := sizeCall(structuredIssue("CRW-X", test.c, test.r, nil))
		if code != test.exit || errOut != "" {
			t.Errorf("%d+%d: exit %d, want %d: %s%s", test.c, test.r, code, test.exit, out, errOut)
			continue
		}
		r := decodeReport(t, out)
		if got := len(atList(r, "reasons")); got != test.reasons {
			t.Errorf("%d+%d: %d reasons, want %d: %s", test.c, test.r, got, test.reasons, out)
		}
		if atNum(t, r, "signals", "criteria_total") != test.c+test.r {
			t.Errorf("%d+%d: criteria_total %v", test.c, test.r, at(r, "signals", "criteria_total"))
		}
	}
}

func TestIssueSizeException(t *testing.T) {
	exception := func(m map[string]any) string {
		base := map[string]any{"issue": "CRW-X", "approved_by": "Reviewer", "approved_on": "2026-10-03", "statement": "Keep it as one issue; the parts cannot pass verification apart."}
		for k, v := range m {
			base[k] = v
		}
		return structuredIssue("CRW-X", 14, 0, map[string]any{"exception": base})
	}
	code, out, errOut := sizeCall(exception(nil))
	if code != 0 || errOut != "" {
		t.Fatalf("a valid exception: exit %d: %s%s", code, out, errOut)
	}
	r := decodeReport(t, out)
	record, _ := at(r, "exception_record").(string)
	if at(r, "decision") != "split_recommended" || at(r, "assignable") != true || at(r, "exception", "approved_by") != "Reviewer" ||
		!strings.Contains(record, "CRW-X") || !strings.Contains(record, "Reviewer") || !strings.Contains(record, "2026-10-03") || !strings.Contains(record, "criteria_total 14 exceeds") {
		t.Errorf("an approved exception keeps the finding and records the approval: %s", out)
	}
	for name, m := range map[string]map[string]any{
		"another issue's exception": {"issue": "CRW-Y"},
		"an impossible date":        {"approved_on": "2026-13-40"},
		"a date in another form":    {"approved_on": "10/03/2026"},
		"no approver":               {"approved_by": ""},
		"a blank statement":         {"statement": "   "},
		"a number for the approver": {"approved_by": 7},
	} {
		if code, out, errOut := sizeCall(exception(m)); code != 2 || out != "" || errOut == "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errOut)
		}
	}
	noID, _ := json.Marshal(map[string]any{"criteria": []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, "exception": map[string]any{"issue": "", "approved_by": "Reviewer", "approved_on": "2026-10-03", "statement": "go"}})
	if code, _, _ := sizeCall(string(noID)); code != 2 {
		t.Errorf("an exception for an issue with no id: exit %d, want 2", code)
	}
	// Nothing needed approving: a well-formed exception on an ok answer is ignored, a malformed one is not.
	okWith := structuredIssue("CRW-X", 3, 0, map[string]any{"exception": map[string]any{"issue": "CRW-X", "approved_by": "Reviewer", "approved_on": "2026-10-03", "statement": "unneeded"}})
	code, out, _ = sizeCall(okWith)
	r = decodeReport(t, out)
	if _, has := r["exception"]; code != 0 || has || r["exception_record"] != nil || r["decision"] != "ok" {
		t.Errorf("an exception on an ok answer is not echoed: exit %d %s", code, out)
	}
	if code, _, _ := sizeCall(structuredIssue("CRW-X", 3, 0, map[string]any{"exception": map[string]any{"issue": "CRW-X"}})); code != 2 {
		t.Errorf("a malformed exception on an ok answer: exit %d, want 2", code)
	}
}

func TestIssueSizeUnreadableInput(t *testing.T) {
	dir := t.TempDir()
	notUTF8 := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(notUTF8, []byte("{\"criteria\":[\"\xff\"]}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		input string
		args  []string
		exit  int
	}{
		"not JSON":                   {"criteria: 3", nil, 2},
		"an empty input":             {"", nil, 2},
		"an array":                   {"[]", nil, 2},
		"a number for criteria":      {`{"criteria": 5}`, nil, 2},
		"a number among criteria":    {`{"criteria": ["a", 1]}`, nil, 2},
		"a list for the description": {`{"description": ["a"]}`, nil, 2},
		"no criteria at all":         {`{"id": "CRW-X", "title": "t"}`, nil, 2},
		"a body with no criteria":    {descriptionIssue("## 배경\n1. not a criterion\n"), nil, 2},
		"empty criteria":             {`{"criteria": []}`, nil, 2},
		"not UTF-8":                  {"", []string{notUTF8}, 2},
		"a file that is not there":   {"", []string{filepath.Join(dir, "absent.json")}, 3},
	} {
		code, out, errOut := sizeCall(test.input, test.args...)
		if code != test.exit || out != "" || errOut == "" {
			t.Errorf("%s: exit %d (want %d), stdout %q, stderr %q", name, code, test.exit, out, errOut)
		}
	}
	file := filepath.Join(dir, "issue.json")
	if err := os.WriteFile(file, []byte(structuredIssue("CRW-X", 3, 0, nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := sizeCall("", file); code != 0 || !strings.Contains(out, "CRW-X") {
		t.Errorf("a file argument: exit %d %s", code, out)
	}
	if code, _, errOut := sizeCall("", file, "extra"); code != 2 || !strings.Contains(errOut, "unexpected arguments: extra") {
		t.Errorf("a second file: exit %d %s", code, errOut)
	}
}

func TestIssueSizeReadsTheBodyThroughItsHeadings(t *testing.T) {
	for _, test := range []struct {
		name                       string
		body                       string
		criteria, research, region int
	}{
		{"nested bullets belong to their criterion and other sections are not counted",
			"## 배경\n배경 문장 `docs/background.md`\n\n## 범위\n* `internal/x/y.go`를 고친다.\n\n## 완료 기준\n1. 첫째.\n   * 중첩 하나\n   * 중첩 둘\n2. 둘째.\n3. 셋째 `internal/z/w.go`.\n\n## 주의\n4. 이것은 세지 않는다.\n",
			3, 0, 2},
		{"a research table counts its data rows, and a heading nested in it stays research",
			"## Completion criteria\n- one\n- two\n\n## 2026-10-01 연구 보강: 추가 완료 기준 제안\n설명\n\n### 추가 완료 기준\n1. nested item\n\n| 제안 기준 | 판정 방법 | 지표 |\n| -- | -- | -- |\n| row one | a | b |\n| row two | c | d |\n\n끝\n",
			2, 3, 0},
		{"fenced code is skipped",
			"## 완료 기준\n1. real\n\n```\n## 검증\n2. fake\n```\n3. also real\n",
			2, 0, 0},
		{"English headings are the variant",
			"## Acceptance criteria\n1. a\n2. b\n\n## Verification\nrun `make test` and `internal/q/r_test.go`\n\n## Research reinforcement\n- x\n",
			2, 1, 1},
		{"links are reduced to their text before anything is counted",
			"## 완료 기준\n1. see <issue id=\"x\" href=\"https://example.test/a/b\">CRW-1</issue> and [doc](<https://example.test/doc/path>) then `docs/a.md`\n",
			1, 0, 1},
		{"absolute, home, variable and URL tokens are not regions",
			"## 완료 기준\n1. `~/.codex/x.toml` `/etc/hosts` `$HOME/y` `https://a/b/c` `GOOS=darwin` `make test`\n",
			1, 0, 0},
		{"a table without a delimiter row is not a table",
			"## 완료 기준\n1. one\n\n| a | b |\n| c | d |\n| e | f |\n",
			1, 0, 0},
		{"several tables and a bullet list in one research section",
			"## 완료 기준\n1. one\n\n## 연구 보강\n| h | h |\n|---|---|\n| r1 | x |\n\n- bullet\n\n| h | h |\n|---|---|\n| r2 | y |\n",
			1, 3, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, out, errOut := sizeCall(descriptionIssue(test.body))
			if (code != 0 && code != 1) || errOut != "" {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			r := decodeReport(t, out)
			got := [3]int{atNum(t, r, "signals", "criteria"), atNum(t, r, "signals", "research_criteria"), atNum(t, r, "signals", "edit_regions")}
			if got != [3]int{test.criteria, test.research, test.region} {
				t.Errorf("criteria, research, regions = %v, want %v: %s", got, [3]int{test.criteria, test.research, test.region}, out)
			}
		})
	}
}

func TestIssueSizeFieldsReplaceWhatTheBodyGives(t *testing.T) {
	body := "## 완료 기준\n1. a\n2. b\n3. c\n\n## 연구 보강\n- r\n"
	for name, test := range map[string]struct {
		extra            map[string]any
		criteria, search int
	}{
		"the body alone":               {nil, 3, 1},
		"criteria as a list":           {map[string]any{"criteria": []string{"1", "2", "3", "4", "5"}}, 5, 1},
		"criteria as marked text":      {map[string]any{"criteria": "1. one\n2. two\n\n- three\n"}, 3, 1},
		"research as a list":           {map[string]any{"research_criteria": []string{"x", "y"}}, 3, 2},
		"research as an empty list":    {map[string]any{"research_criteria": []string{}}, 3, 0},
		"blank list items are not any": {map[string]any{"criteria": []string{"1", " ", ""}}, 1, 1},
	} {
		m := map[string]any{"id": "CRW-X", "description": body}
		for k, v := range test.extra {
			m[k] = v
		}
		input, _ := json.Marshal(m)
		if c, r := sizeSignals(t, string(input)); c != test.criteria || r != test.search {
			t.Errorf("%s: criteria %d research %d, want %d and %d", name, c, r, test.criteria, test.search)
		}
	}
}

// The proposal: its shape holds for any size, and its edges follow the regions the bundles name.
func TestIssueSizeProposalShape(t *testing.T) {
	for total := 9; total <= 40; total++ {
		research := total / 3
		code, out, _ := sizeCall(structuredIssue("CRW-X", total-research, research, nil))
		if code != 1 {
			t.Fatalf("%d criteria: exit %d", total, code)
		}
		r := decodeReport(t, out)
		bundles := atList(r, "proposal", "bundles")
		want := (total + 3) / 4
		want = max(2, min(5, want))
		if len(bundles) != want {
			t.Errorf("%d criteria: %d bundles, want %d", total, len(bundles), want)
		}
		seen := map[string]int{}
		for _, b := range bundles {
			for _, c := range b.(map[string]any)["criteria"].([]any) {
				seen[c.(map[string]any)["id"].(string)]++
			}
		}
		if len(seen) != total {
			t.Errorf("%d criteria: %d distinct ids in the bundles", total, len(seen))
		}
		for id, n := range seen {
			if n != 1 {
				t.Errorf("%d criteria: %s is in %d bundles", total, id, n)
			}
		}
		for _, e := range atList(r, "proposal", "order") {
			edge := e.(map[string]any)
			from, to := edge["from"].(string), edge["to"].(string)
			if from >= to {
				t.Errorf("%d criteria: edge %s -> %s does not follow the bundle order", total, from, to)
			}
		}
	}
}

func TestIssueSizeProposalEdgesFollowRegions(t *testing.T) {
	criteria := func(paths ...string) []string {
		out := make([]string, len(paths))
		for i, p := range paths {
			out[i] = fmt.Sprintf("criterion %d changes `%s`", i+1, p)
		}
		return out
	}
	edges := func(paths ...string) []map[string]any {
		input, _ := json.Marshal(map[string]any{"id": "CRW-X", "criteria": criteria(paths...)})
		code, out, _ := sizeCall(string(input))
		if code != 1 {
			t.Fatalf("exit %d: %s", code, out)
		}
		var got []map[string]any
		for _, e := range atList(decodeReport(t, out), "proposal", "order") {
			got = append(got, e.(map[string]any))
		}
		return got
	}
	// 9 criteria in 3 bundles of 3, every bundle in its own directory: nothing to order.
	disjoint := edges("cmd/x/a.go", "cmd/x/b.go", "cmd/x/c.go", "docs/y/a.md", "docs/y/b.md", "docs/y/c.md", "plugins/z/a.md", "plugins/z/b.md", "plugins/z/c.md")
	if len(disjoint) != 0 {
		t.Errorf("disjoint regions are ordered: %v", disjoint)
	}
	// Bundles 1 and 2 both touch internal/a (one by a parent directory): ordered; bundle 3 is elsewhere.
	shared := edges("internal/a/f.go", "internal/a/g.go", "internal/b/f.go", "internal/a/h.go", "internal/a/i.go", "internal/b/g.go", "docs/y/a.md", "docs/y/b.md", "docs/y/c.md")
	if len(shared) != 1 || shared[0]["from"] != "B1" || shared[0]["to"] != "B2" || shared[0]["reason"] != "shared_prefix" {
		t.Errorf("shared region: %v", shared)
	}
	// A bundle that names no path is treated as overlapping every other one, the way the scheduler reads an unknown region.
	unknown := edges("cmd/x/a.go", "cmd/x/b.go", "cmd/x/c.go", "docs/y/a.md", "docs/y/b.md", "docs/y/c.md", "", "", "")
	reasons := map[string]string{}
	for _, e := range unknown {
		reasons[e["from"].(string)+">"+e["to"].(string)] = e["reason"].(string)
	}
	if len(unknown) != 2 || reasons["B1>B3"] != "unknown_regions" || reasons["B2>B3"] != "unknown_regions" {
		t.Errorf("unknown region: %v", unknown)
	}
	// Edges that follow from others are not repeated: four bundles that all overlap are a chain.
	chain := edges("internal/a/1.go", "internal/a/2.go", "internal/a/3.go", "internal/a/4.go", "internal/a/5.go", "internal/a/6.go", "internal/a/7.go", "internal/a/8.go", "internal/a/9.go", "internal/a/10.go", "internal/a/11.go", "internal/a/12.go", "internal/a/13.go")
	if len(chain) != 3 {
		t.Errorf("a chain of 4 bundles has 3 edges: %v", chain)
	}
}

func TestIssueSizeProposalOfCRW184(t *testing.T) {
	_, out, _ := sizeCall(fixtureIssue(t, "CRW-184"))
	r := decodeReport(t, out)
	bundles := atList(r, "proposal", "bundles")
	if len(bundles) < 3 || len(bundles) > 5 {
		t.Fatalf("CRW-184 is divided into %d bundles, want 3 to 5: %s", len(bundles), out)
	}
	if got := atNum(t, r, "signals", "criteria"); got != 7 {
		t.Errorf("completion criteria %d, want 7", got)
	}
	if got := atNum(t, r, "signals", "research_criteria"); got != 7 {
		t.Errorf("research criteria %d, want 7", got)
	}
	// The body names no file, so every bundle's region is unknown and the bundles run in the issue's order.
	order := atList(r, "proposal", "order")
	if len(order) != len(bundles)-1 {
		t.Errorf("order edges %d, want a chain of %d", len(order), len(bundles)-1)
	}
	for _, e := range order {
		if e.(map[string]any)["reason"] != "unknown_regions" {
			t.Errorf("edge %v", e)
		}
	}
}

// shapeOf checks a proposal: its bundles hold every item once. It answers the bundles' item ids.
func shapeOf(t testing.TB, r map[string]any, total int) [][]string {
	t.Helper()
	var bundles [][]string
	seen := map[string]int{}
	for _, b := range atList(r, "proposal", "bundles") {
		var ids []string
		for _, c := range b.(map[string]any)["criteria"].([]any) {
			id := c.(map[string]any)["id"].(string)
			ids = append(ids, id)
			seen[id]++
		}
		bundles = append(bundles, ids)
	}
	if len(seen) != total {
		t.Errorf("%d distinct items in the bundles, want %d: %v", len(seen), total, bundles)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s is in %d bundles", id, n)
		}
	}
	return bundles
}

func orderOf(r map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range atList(r, "proposal", "order") {
		out = append(out, e.(map[string]any))
	}
	return out
}

func TestIssueSizeDeliverables(t *testing.T) {
	list := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("output %d", i+1)
		}
		return out
	}
	for _, test := range []struct{ n, exit int }{{0, 0}, {7, 0}, {8, 1}} {
		code, out, _ := sizeCall(structuredIssue("CRW-X", 3, 0, map[string]any{"deliverables": list(test.n)}))
		r := decodeReport(t, out)
		if code != test.exit || atNum(t, r, "signals", "deliverables") != test.n || len(atList(r, "reasons")) != test.exit {
			t.Errorf("%d deliverables: exit %d (want %d) %s", test.n, code, test.exit, out)
		}
	}
	// Not declared is not zero: the signal is null and no limit applies.
	_, out, _ := sizeCall(structuredIssue("CRW-X", 3, 0, nil))
	if v, has := at(decodeReport(t, out), "signals").(map[string]any)["deliverables"]; !has || v != nil {
		t.Errorf("an issue declaring none: signals.deliverables = %v (present %v), want null", v, has)
	}
	body := "## 완료 기준\n1. a\n\n## 산출물\n- a new package\n- a command family\n- a skill document\n"
	_, out, _ = sizeCall(descriptionIssue(body))
	if got := atNum(t, decodeReport(t, out), "signals", "deliverables"); got != 3 {
		t.Errorf("a Deliverables section gives %d, want 3: %s", got, out)
	}
	// A heading under another classified section is part of that section, so it declares nothing.
	nested := "## 완료 기준\n1. a\n\n## 범위\n* x\n\n### Deliverables\n- y\n- z\n"
	_, out, _ = sizeCall(descriptionIssue(nested))
	if v := at(decodeReport(t, out), "signals").(map[string]any)["deliverables"]; v != nil {
		t.Errorf("a nested Deliverables heading declared %v", v)
	}
}

func TestIssueSizeDependsOn(t *testing.T) {
	paths := []string{"cmd/x/a.go", "cmd/x/b.go", "cmd/x/c.go", "docs/y/a.md", "docs/y/b.md", "docs/y/c.md", "plugins/z/a.md", "plugins/z/b.md", "plugins/z/c.md"}
	criteria := make([]string, len(paths))
	for i, p := range paths {
		criteria[i] = fmt.Sprintf("criterion %d changes `%s`", i+1, p)
	}
	issue := func(depends any, research ...string) string {
		m := map[string]any{"id": "CRW-X", "criteria": criteria, "depends_on": depends}
		if len(research) > 0 {
			m["research_criteria"] = research
		}
		out, _ := json.Marshal(m)
		return string(out)
	}
	// Regions are disjoint, so only what the planner declared orders the bundles.
	code, out, _ := sizeCall(issue(map[string][]string{"C4": {"C1"}, "C9": {"C4"}}))
	r := decodeReport(t, out)
	order := orderOf(r)
	if code != 1 || len(order) != 2 || order[0]["from"] != "B1" || order[0]["to"] != "B2" || order[0]["reason"] != "prerequisite" ||
		fmt.Sprint(order[0]["needs"]) != "[C4 needs C1]" || order[1]["from"] != "B2" || order[1]["to"] != "B3" || fmt.Sprint(order[1]["needs"]) != "[C9 needs C4]" {
		t.Errorf("declared prerequisites between disjoint bundles: exit %d %v", code, order)
	}
	// A declared prerequisite stays even where the edges around it already reach: it is the planner's statement.
	_, out, _ = sizeCall(issue(map[string][]string{"C4": {"C1"}, "C7": {"C4"}, "C8": {"C1"}}))
	if got := orderOf(decodeReport(t, out)); len(got) != 3 {
		t.Errorf("three declared pairs, %d edges: %v", len(got), got)
	}
	// A research row that declares a need joins the bundle of that need, whatever its wording says.
	_, out, _ = sizeCall(issue(map[string][]string{"R1": {"C8"}}, "unrelated words here", "other words entirely", "more words again"))
	r = decodeReport(t, out)
	bundles := shapeOf(t, r, 12)
	for _, ids := range bundles {
		has := func(id string) bool {
			for _, x := range ids {
				if x == id {
					return true
				}
			}
			return false
		}
		if has("C8") != has("R1") {
			t.Errorf("R1 is not with C8: %v", bundles)
		}
	}
	for name, depends := range map[string]any{
		"a need on a later criterion":    map[string][]string{"C1": {"C5"}},
		"a need on itself":               map[string][]string{"C9": {"C9"}},
		"a need on a research row":       map[string][]string{"C3": {"R1"}},
		"a key outside the issue":        map[string][]string{"C10": {"C1"}},
		"a key that is not an item":      map[string][]string{"X1": {"C1"}},
		"a need outside the issue":       map[string][]string{"C2": {"C99"}},
		"a research key without its row": map[string][]string{"R1": {"C1"}},
		"a need that is not a list":      map[string]string{"C2": "C1"},
	} {
		if code, out, errOut := sizeCall(issue(depends)); code != 2 || out != "" || !strings.Contains(errOut, "depends_on") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errOut)
		}
	}
}

func TestIssueSizeProposalBranches(t *testing.T) {
	// Fewer than two completion criteria: every item is a seed, and each is in exactly one bundle.
	code, out, _ := sizeCall(structuredIssue("CRW-X", 1, 4, nil))
	r := decodeReport(t, out)
	if bundles := shapeOf(t, r, 5); code != 1 || len(bundles) != 2 {
		t.Errorf("one completion criterion and four research rows: exit %d, bundles %v", code, bundles)
	}
	// Capacity: seven rows that all read like C1 cannot all join its bundle.
	rows := make([]string, 7)
	for i := range rows {
		rows[i] = fmt.Sprintf("alpha beta gamma delta epsilon row %d", i+1)
	}
	input, _ := json.Marshal(map[string]any{"id": "CRW-X", "criteria": []string{"alpha beta gamma delta epsilon", "zzz qqq www"}, "research_criteria": rows})
	_, out, _ = sizeCall(string(input))
	bundles := shapeOf(t, decodeReport(t, out), 9)
	sizes := []int{len(bundles[0]), len(bundles[1])}
	if len(bundles) != 2 || sizes[0] != 5 || sizes[1] != 4 {
		t.Errorf("capacity ceil(9/2) = 5: bundles %v", bundles)
	}
	// Known and unknown regions together: the edge between the two bundles that share a region is implied by the other two.
	criteria := []string{"a `internal/a/x.go`", "b `internal/a/y.go`", "c `internal/a/z.go`", "d", "e", "f", "g `internal/a/p.go`", "h `internal/a/q.go`", "i `internal/a/r.go`"}
	input, _ = json.Marshal(map[string]any{"id": "CRW-X", "criteria": criteria})
	_, out, _ = sizeCall(string(input))
	order := orderOf(decodeReport(t, out))
	if len(order) != 2 || order[0]["reason"] != "unknown_regions" || order[1]["reason"] != "unknown_regions" || order[0]["from"] != "B1" || order[1]["to"] != "B3" {
		t.Errorf("mixed regions: %v", order)
	}
}

func TestIssueSizeProposalIsStableAcrossRuns(t *testing.T) {
	inputs := map[string]string{
		"CRW-184 structure": fixtureIssue(t, "CRW-184"),
		"research rows":     structuredIssue("CRW-X", 6, 9, nil),
		"flat criteria":     structuredIssue("CRW-X", 17, 0, nil),
	}
	for name, input := range inputs {
		_, first, _ := sizeCall(input)
		if !strings.Contains(first, "\"proposal\"") {
			t.Fatalf("%s: no proposal to compare:\n%s", name, first)
		}
		for i := 0; i < 20; i++ {
			if _, again, _ := sizeCall(input); again != first {
				t.Fatalf("%s: run %d differs:\n%s\n---\n%s", name, i+2, first, again)
			}
		}
	}
}

func TestIssueSizeNothingToDivide(t *testing.T) {
	deliverables := make([]string, 8)
	for i := range deliverables {
		deliverables[i] = fmt.Sprintf("output %d", i+1)
	}
	code, out, _ := sizeCall(structuredIssue("CRW-X", 1, 0, map[string]any{"deliverables": deliverables}))
	r := decodeReport(t, out)
	bundles, bundlesOK := at(r, "proposal", "bundles").([]any)
	order, orderOK := at(r, "proposal", "order").([]any)
	if code != 1 || at(r, "decision") != "split_recommended" || at(r, "proposal", "status") != "none" || !bundlesOK || len(bundles) != 0 || !orderOK || len(order) != 0 {
		t.Errorf("one criterion and 8 deliverables: exit %d %s", code, out)
	}
	reasons := atList(r, "reasons")
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0].(string), "deliverables 8 exceeds the limit") {
		t.Errorf("reasons %v", reasons)
	}
	// Every list a draft prints is a list, never null: bundles that name no file and bundles that need no order.
	disjoint := make([]string, 9)
	for i := range disjoint {
		disjoint[i] = fmt.Sprintf("criterion %d changes `%s/f%d.go`", i+1, []string{"cmd/a", "docs/b", "plugins/c"}[i/3], i)
	}
	input, _ := json.Marshal(map[string]any{"id": "CRW-X", "criteria": disjoint})
	_, out, _ = sizeCall(string(input))
	_, bare, _ := sizeCall(structuredIssue("CRW-X", 9, 0, nil))
	if !strings.Contains(bare, "\"regions\": []") {
		t.Errorf("a bundle that names no file does not print regions as a list:\n%s", bare)
	}
	for _, key := range []string{"\"order\": []"} {
		if !strings.Contains(out, key) {
			t.Errorf("the draft does not print %s:\n%s", key, out)
		}
	}
	for _, key := range []string{"\"order\": null", "\"regions\": null", "\"criteria\": null", "\"bundles\": null", "\"reasons\": null"} {
		if strings.Contains(out, key) || strings.Contains(bare, key) {
			t.Errorf("%s in a draft:\n%s", key, out)
		}
	}
}

func TestIssueSizeDependsOnCombinations(t *testing.T) {
	unknown := make([]string, 9)
	for i := range unknown {
		unknown[i] = fmt.Sprintf("criterion %d", i+1)
	}
	build := func(m map[string]any) string {
		base := map[string]any{"id": "CRW-X", "criteria": unknown}
		for k, v := range m {
			base[k] = v
		}
		out, _ := json.Marshal(base)
		return string(out)
	}
	// A declared prerequisite stays beside the chain of unknown regions, and the chain keeps its own edges.
	_, out, _ := sizeCall(build(map[string]any{"depends_on": map[string][]string{"C7": {"C1"}}}))
	order := orderOf(decodeReport(t, out))
	got := ""
	for _, e := range order {
		got += fmt.Sprintf("%s>%s:%s ", e["from"], e["to"], e["reason"])
	}
	if got != "B1>B2:unknown_regions B1>B3:prerequisite B2>B3:unknown_regions " {
		t.Errorf("edges: %s", got)
	}
	// A row that needs several criteria joins the latest of their bundles; an empty need list is no need.
	research := []string{"first row", "second row", "third row"}
	_, out, _ = sizeCall(build(map[string]any{"research_criteria": research, "depends_on": map[string][]string{"R1": {"C2", "C8"}}}))
	for _, ids := range shapeOf(t, decodeReport(t, out), 12) {
		has := func(id string) bool {
			for _, x := range ids {
				if x == id {
					return true
				}
			}
			return false
		}
		if has("R1") != has("C8") || has("R1") && has("C2") {
			t.Errorf("R1 needs C2 and C8: %v", ids)
		}
	}
	_, withEmpty, _ := sizeCall(build(map[string]any{"research_criteria": research, "depends_on": map[string][]string{"R1": {}}}))
	_, without, _ := sizeCall(build(map[string]any{"research_criteria": research}))
	if withEmpty != without {
		t.Errorf("an empty need list changed the draft:\n%s\n---\n%s", withEmpty, without)
	}
	// In the fewer-than-two path each need is an edge from the first criterion's bundle.
	rows := make([]string, 8)
	needs := map[string][]string{}
	for i := range rows {
		rows[i] = fmt.Sprintf("row %d", i+1)
		needs[fmt.Sprintf("R%d", i+1)] = []string{"C1"}
	}
	input, _ := json.Marshal(map[string]any{"id": "CRW-X", "criteria": []string{"only criterion"}, "research_criteria": rows, "depends_on": needs})
	code, out, _ := sizeCall(string(input))
	r := decodeReport(t, out)
	edges := orderOf(r)
	// The bundles name no file, so the unknown-region chain comes with the declared prerequisites.
	if bundles := shapeOf(t, r, 9); code != 1 || len(bundles) != 3 || len(edges) != 3 || edges[0]["from"] != "B1" || edges[0]["to"] != "B2" || edges[0]["reason"] != "prerequisite" ||
		edges[1]["from"] != "B1" || edges[1]["to"] != "B3" || edges[1]["reason"] != "prerequisite" || edges[2]["from"] != "B2" || edges[2]["to"] != "B3" || edges[2]["reason"] != "unknown_regions" {
		t.Errorf("one criterion and eight rows that need it: exit %d, bundles %v, edges %v", code, bundles, edges)
	}
	// The declaration is read whatever the verdict: a bad one is refused on an ok issue too.
	if code, _, errOut := sizeCall(structuredIssue("CRW-X", 3, 0, map[string]any{"depends_on": map[string][]string{"C1": {"C3"}}})); code != 2 || !strings.Contains(errOut, "depends_on") {
		t.Errorf("a bad depends_on on an ok issue: exit %d %s", code, errOut)
	}
}

// What the reviewers' hand-made inputs found: a body can be written in more than one way, and a way the
// command does not read must not let an oversized issue through.
func TestIssueSizeReadsMarkdownTheWayItIsWritten(t *testing.T) {
	for _, test := range []struct {
		name                       string
		body                       string
		criteria, research, region int
	}{
		{"top-level items indented by one space are still top-level",
			"## 완료 기준\n1. a\n 2. b\n 3. c\n 4. d\n 5. e\n 6. f\n 7. g\n 8. h\n 9. i\n",
			9, 0, 0},
		{"a sibling indented by two spaces is a sibling, a deeper bullet is nested",
			"## 완료 기준\n1. a\n  2. b\n     - nested under b\n3. c\n",
			3, 0, 0},
		{"a tab nests",
			"## 완료 기준\n1. a\n\t* nested\n2. b\n",
			2, 0, 0},
		{"a list line indented four columns with no item before it is code",
			"## 완료 기준\nintro text\n\n    1. in a code block\n\n1. real\n",
			1, 0, 0},
		{"setext headings are headings",
			"Completion criteria\n-------------------\n1. a\n2. b\n\nResearch reinforcement\n----------------------\n- x\n- y\n",
			2, 2, 0},
		{"a rule under a list item is not a heading, and does not join the items",
			"## 완료 기준\n1. a\n---\n2. b\n",
			2, 0, 0},
		{"a table inside a list item belongs to the item, and so does the text after it",
			"## 완료 기준\n1. first\n   | h | h |\n   |---|---|\n   | x | y |\n   after the table `docs/z.md`\n2. second\n",
			2, 0, 1},
		{"a long fence is not closed by a short one",
			"## 완료 기준\n1. real\n\n````\n```\n## 검증\n2. fake\n```\n3. fake too\n````\n4. real again\n",
			2, 0, 0},
		{"a fence is not closed by the other character or by a line with an info string",
			"## 완료 기준\n1. real\n\n```\n~~~\n2. fake\n``` text\n3. fake\n```\n4. real again\n5. real too\n",
			3, 0, 0},
		{"a setext heading right after a closed fence is a heading, the fence ended the item's paragraph",
			"## 완료 기준\n1. a\n2. b\n```\ncode\n```\nResearch reinforcement\n----------------------\n- r1\n- r2\n- r3\n- r4\n- r5\n",
			2, 5, 0},
		{"the same after a tilde fence",
			"## 완료 기준\n1. a\n2. b\n~~~\ncode\n~~~\nResearch reinforcement\n======================\n- r1\n- r2\n",
			2, 2, 0},
		{"the same after a fence nested in the item",
			"## 완료 기준\n1. a\n2. b\n   ```\n   code\n   ```\nResearch reinforcement\n----------------------\n- r1\n- r2\n- r3\n",
			2, 3, 0},
		{"a fence nested in an item does not end the list, the next item is still counted",
			"## 완료 기준\n1. a\n   ```\n   code\n   ```\n2. b\n3. c\n",
			3, 0, 0},
		{"a thematic break ends the item's paragraph, so the setext heading after it is read",
			"## 완료 기준\n1. a\n2. b\n---\nResearch reinforcement\n----------------------\n- r1\n- r2\n- r3\n- r4\n- r5\n",
			2, 5, 0},
		{"the same after a rule written with stars",
			"## 완료 기준\n1. a\n2. b\n***\nResearch reinforcement\n----------------------\n- r1\n- r2\n",
			2, 2, 0},
		{"the same after a rule written with underscores and an equals underline",
			"## 완료 기준\n1. a\n2. b\n___\nResearch reinforcement\n======================\n- r1\n- r2\n- r3\n",
			2, 3, 0},
		{"the same after a rule written with spaced dashes",
			"## 완료 기준\n1. a\n2. b\n- - -\nResearch reinforcement\n----------------------\n- r1\n",
			2, 1, 0},
		{"a paragraph continued without an indent belongs to its item, one after a blank line does not",
			"## 완료 기준\n1. first\ncontinues `docs/lazy.md`\n\nplain after a blank `cmd/other.go`\n2. second\n",
			2, 0, 1},
		{"escaped markers are text, not structure",
			"## 완료 기준\n1. real \\## Research and \\- not a bullet\n\\## Research\n\\- not an item\n",
			1, 0, 0},
		{"a link address with parentheses is not read as words",
			"## 완료 기준\n1. see [spec](https://example.test/a_(b)/test/c) and [other](<https://example.test/c d/test>) now\n",
			1, 0, 0},
		{"the same file written two ways is one region, a path leaving the repository is none",
			"## 완료 기준\n1. `internal/../internal/skill/a.go` and `./internal/skill/b.go` and `../outside/c.go`\n",
			1, 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := strings.ReplaceAll(test.body, "\n", "\n") // the body as written
			code, out, errOut := sizeCall(descriptionIssue(body))
			if (code != 0 && code != 1) || errOut != "" {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			r := decodeReport(t, out)
			got := [3]int{atNum(t, r, "signals", "criteria"), atNum(t, r, "signals", "research_criteria"), atNum(t, r, "signals", "edit_regions")}
			if got != [3]int{test.criteria, test.research, test.region} {
				t.Errorf("criteria, research, regions = %v, want %v: %s", got, [3]int{test.criteria, test.research, test.region}, out)
			}
			if strings.Contains(test.name, "address") {
				if kinds := atList(r, "observed", "verification_kinds"); len(kinds) != 0 {
					t.Errorf("words from an address were counted: %v", kinds)
				}
			}
		})
	}
	// Lines written with Windows line endings read the same as with Unix ones.
	body := "## 완료 기준\r\n1. a\r\n2. b\r\n\r\n## 연구 보강\r\n| h | h |\r\n|---|---|\r\n| r | s |\r\n"
	if c, r := sizeSignals(t, descriptionIssue(body)); c != 2 || r != 1 {
		t.Errorf("CRLF: %d criteria, %d research", c, r)
	}
}

func TestIssueSizeSameFileTwoWaysIsOneOrdering(t *testing.T) {
	criteria := []string{
		"a `internal/../internal/skill/a.go`", "b `internal/../internal/skill/a.go`", "c `internal/../internal/skill/a.go`",
		"d `docs/x/b.md`", "e `docs/x/b.md`", "f `docs/x/b.md`",
		"g `internal/skill/a.go`", "h `internal/skill/a.go`", "i `internal/skill/a.go`",
	}
	input, _ := json.Marshal(map[string]any{"id": "CRW-X", "criteria": criteria})
	_, out, _ := sizeCall(string(input))
	order := orderOf(decodeReport(t, out))
	if len(order) != 1 || order[0]["from"] != "B1" || order[0]["to"] != "B3" || order[0]["reason"] != "shared_prefix" || fmt.Sprint(order[0]["regions"]) != "[internal/skill]" {
		t.Errorf("the same file written two ways: %v", order)
	}
}

func TestIssueSizeDependsOnRefusesIdsBeyondTheItems(t *testing.T) {
	for name, depends := range map[string]any{
		"a need that overflows an integer":      map[string][]string{"C2": {"C18446744073709551616"}},
		"a key that overflows an integer":       map[string][]string{"C18446744073709551616": {"C1"}},
		"a research key that overflows":         map[string][]string{"R99999999999999999999": {"C1"}},
		"a need larger than any int but valid":  map[string][]string{"C2": {"C999999999999999999999999"}},
		"a key that is a number with a sign":    map[string][]string{"C-1": {"C1"}},
		"a need with a leading zero":            map[string][]string{"C3": {"C01"}},
		"a key naming no item of a short issue": map[string][]string{"C10": {"C1"}},
	} {
		input, _ := json.Marshal(map[string]any{"id": "CRW-X", "criteria": []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, "depends_on": depends})
		if code, out, errOut := sizeCall(string(input)); code != 2 || out != "" || !strings.Contains(errOut, "depends_on") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errOut)
		}
	}
}

func TestIssueSizeDoesNotLoseCriteriaToLookalikes(t *testing.T) {
	for _, test := range []struct {
		name                       string
		body                       string
		criteria, research, region int
	}{
		{"a line of inline code is not a code fence",
			"## 완료 기준\n1. a\n2. b\n\n```go test``` is the check\n\n3. c\n4. d\n",
			4, 0, 0},
		{"a rule after a lazily continued list paragraph is not a heading",
			"## 완료 기준\n1. a\nlazy line\n---\n2. b\n3. c\n",
			3, 0, 0},
		{"a bare marker is an empty item",
			"## 완료 기준\n1. a\n\n-\n\n2. b\n",
			3, 0, 0},
		{"a thematic break is not an item",
			"## 완료 기준\n1. a\n\n* * *\n\n- - -\n\n2. b\n",
			2, 0, 0},
		{"a heading written without the space is read",
			"## 완료기준\n1. a\n2. b\n",
			2, 0, 0},
		{"the word research inside a criteria heading does not make it research",
			"## Completion criteria (no research needed)\n1. a\n2. b\n",
			2, 0, 0},
		{"a delimiter row may use one dash per cell",
			"## 완료 기준\n1. a\n\n## 연구 보강\n| h | h |\n|-|-|\n| r | s |\n",
			1, 1, 0},
		{"a fence indented four columns is code, not a fence",
			"## 완료 기준\n    ```\n1. a\n2. b\n",
			2, 0, 0},
		{"a table indented four columns under an item belongs to the item",
			"## 완료 기준\n1. one\n\n    | a | b |\n    |---|---|\n    | x | y |\n",
			1, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, out, errOut := sizeCall(descriptionIssue(test.body))
			if (code != 0 && code != 1) || errOut != "" {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			r := decodeReport(t, out)
			got := [3]int{atNum(t, r, "signals", "criteria"), atNum(t, r, "signals", "research_criteria"), atNum(t, r, "signals", "edit_regions")}
			if got != [3]int{test.criteria, test.research, test.region} {
				t.Errorf("criteria, research, regions = %v, want %v: %s", got, [3]int{test.criteria, test.research, test.region}, out)
			}
		})
	}
}

// What the command did not read is in the report: a heading it does not know is how a whole list of
// criteria goes missing, so the report names every heading it left unread.
func TestIssueSizeReportsTheHeadingsItDidNotRead(t *testing.T) {
	body := "## 배경\ntext\n\n## Exit criteria\n1. a\n2. b\n3. c\n\n## 완료 기준\n1. d\n\n### 하위\n- e\n\n## 주의\nx\n"
	_, out, _ := sizeCall(descriptionIssue(body))
	r := decodeReport(t, out)
	unread := atList(r, "observed", "unread_headings")
	if fmt.Sprint(unread) != "[배경 Exit criteria 주의]" || atNum(t, r, "signals", "criteria") != 2 {
		t.Errorf("unread headings %v, criteria %d: %s", unread, atNum(t, r, "signals", "criteria"), out)
	}
	// An issue given as fields has no headings, and the list is still a list.
	_, out, _ = sizeCall(structuredIssue("CRW-X", 3, 0, nil))
	if v, ok := at(decodeReport(t, out), "observed", "unread_headings").([]any); !ok || len(v) != 0 {
		t.Errorf("fields: unread_headings = %v", at(decodeReport(t, out), "observed", "unread_headings"))
	}
}

func TestIssueSizeRefusesACodeFenceThatIsNeverClosed(t *testing.T) {
	body := "## 완료 기준\n1. a\n2. b\n\n```\n3. c\n4. d\n"
	code, out, errOut := sizeCall(descriptionIssue(body))
	if code != 2 || out != "" || !strings.Contains(errOut, "never closed") {
		t.Errorf("an unclosed fence: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	// Closed at the end of the body, the same text is read.
	if c, _ := sizeSignals(t, descriptionIssue(body+"```\n")); c != 2 {
		t.Errorf("a closed fence: %d criteria", c)
	}
}

func TestIssueSizeLinkAddressesAreBounded(t *testing.T) {
	if end := linkAddressEnd("("+strings.Repeat("x", linkAddressLimit+10)+")", 1); end != -1 {
		t.Errorf("an address longer than the limit closed at %d", end)
	}
	if end := linkAddressEnd("(x(y)z) tail", 1); end != 7 {
		t.Errorf("a short address with balanced parentheses ended at %d, want 7", end)
	}
	// Many unclosed openings are one pass each, not a rescan of the rest of the line.
	line := strings.Repeat("[a](", 40000)
	if got := stripMarkdownLinks(line); got != line {
		t.Errorf("unclosed link openings were changed")
	}
}

func TestIssueSizeReadsDecoratedResearchHeadings(t *testing.T) {
	for _, heading := range []string{
		"2026-10-02 Research reinforcement: proposed completion criteria",
		"1. Research reinforcement",
		"**Research** reinforcement",
		"Research reinforcement",
		"2026-10-01 연구 보강: 추가 완료 기준 제안",
		"연구보강",
	} {
		body := "## 완료 기준\n1. a\n2. b\n\n## " + heading + "\n- r1\n- r2\n- r3\n- r4\n- r5\n"
		code, out, _ := sizeCall(descriptionIssue(body))
		r := decodeReport(t, out)
		if code != 1 || atNum(t, r, "signals", "criteria") != 2 || atNum(t, r, "signals", "research_criteria") != 5 {
			t.Errorf("%q: exit %d, signals %v", heading, code, at(r, "signals"))
		}
	}
	// A code line that looks like a list item is not a list paragraph, so the setext heading after it is read.
	body := "## 완료 기준\n1. a\n\n    1. code\nResearch reinforcement\n----------------------\n- x\n- y\n"
	if c, r := sizeSignals(t, descriptionIssue(body)); c != 1 || r != 2 {
		t.Errorf("a setext heading after an indented code line: %d criteria, %d research", c, r)
	}
}

// The recorded issues: 33 issues of P-CRW-101, 114, 115, 116 and P-CRW-64 M2, with what their pull
// requests turned out to be. The counts are the top-level completion criteria and the research
// reinforcement rows read from the Linear bodies. A row with a body fixture runs through the command
// and must reproduce its counts; every row runs through the command as structured counts, so the 18
// rows without a fixture exercise the limits, not the parser.
var recordedIssues = []struct {
	id, project                            string
	criteria, research                     int
	lines, files, commits, minutes, rounds int // rounds: relay correction generations, -1 where no relay recorded the issue
}{
	{"CRW-242", "P-CRW-101", 4, 0, 133, 1, 2, 36, -1},
	{"CRW-243", "P-CRW-101", 4, 0, 147, 3, 3, 69, -1},
	{"CRW-244", "P-CRW-101", 4, 0, 140, 2, 3, 47, -1},
	{"CRW-245", "P-CRW-101", 5, 0, 32, 1, 3, 47, -1},
	{"CRW-246", "P-CRW-101", 4, 0, 9, 2, 1, 12, -1},
	{"CRW-247", "P-CRW-101", 4, 0, 94, 2, 3, 20, -1},
	{"CRW-248", "P-CRW-114", 6, 0, 902, 4, 3, 117, 1},
	{"CRW-249", "P-CRW-114", 6, 0, 423, 5, 3, 105, 1},
	{"CRW-250", "P-CRW-114", 5, 0, 1207, 10, 6, 68, 0},
	{"CRW-251", "P-CRW-114", 5, 0, 673, 11, 7, 118, 1},
	{"CRW-252", "P-CRW-114", 3, 0, 29, 1, 1, 16, 0},
	{"CRW-253", "P-CRW-115", 6, 0, 720, 7, 4, 29, 0},
	{"CRW-254", "P-CRW-115", 6, 0, 749, 7, 6, 34, 0},
	{"CRW-255", "P-CRW-115", 5, 0, 375, 10, 7, 74, 1},
	{"CRW-256", "P-CRW-115", 5, 0, 465, 5, 4, 46, 0},
	{"CRW-257", "P-CRW-115", 3, 0, 89, 3, 2, 23, 0},
	{"CRW-258", "P-CRW-115", 5, 0, 1517, 15, 5, 43, 0},
	{"CRW-259", "P-CRW-115", 5, 0, 1535, 41, 8, 79, 0},
	{"CRW-260", "P-CRW-116", 6, 0, 2300, 16, 6, 144, 2},
	{"CRW-261", "P-CRW-116", 6, 0, 846, 10, 5, 89, 1},
	{"CRW-262", "P-CRW-116", 5, 0, 439, 4, 4, 67, 1},
	{"CRW-263", "P-CRW-116", 5, 0, 175, 4, 4, 55, 1},
	{"CRW-264", "P-CRW-116", 4, 0, 209, 6, 5, 192, 1},
	{"CRW-265", "P-CRW-116", 4, 0, 113, 2, 2, 28, 0},
	{"CRW-266", "P-CRW-116", 4, 0, 194, 9, 2, 41, 1},
	{"CRW-267", "P-CRW-116", 5, 0, 594, 16, 5, 88, 1},
	{"CRW-268", "P-CRW-116", 3, 0, 25, 6, 7, 163, 1},
	{"CRW-269", "P-CRW-116", 4, 0, 202, 2, 2, 55, 1},
	{"CRW-270", "P-CRW-116", 5, 0, 818, 13, 5, 81, 1},
	{"CRW-271", "P-CRW-116", 5, 0, 491, 4, 4, 73, 1},
	{"CRW-272", "P-CRW-116", 3, 0, 100, 1, 1, 10, 0},
	{"CRW-183", "P-CRW-64 M2", 5, 5, 8047, 82, 21, 179, 2},
	{"CRW-184", "P-CRW-64 M2", 7, 7, 15488, 74, 32, 467, 0},
}

// oversized is what the records call an issue that grew past its design: two orders of magnitude more
// changed lines than the median small issue and more than twice the commits of the busiest one.
func oversized(lines, commits int) bool { return lines > 5000 && commits > 16 }

func TestIssueSizeOnTheRecordedIssues(t *testing.T) {
	if len(recordedIssues) != 33 {
		t.Fatalf("%d recorded issues, want 33", len(recordedIssues))
	}
	for _, row := range recordedIssues {
		t.Run(row.id, func(t *testing.T) {
			wantDecision := "ok"
			if oversized(row.lines, row.commits) {
				wantDecision = "split_recommended"
			}
			if _, err := os.Stat(filepath.Join(issueSizeFixtures, row.id+".md")); err == nil {
				if c, r := sizeSignals(t, fixtureIssue(t, row.id)); c != row.criteria || r != row.research {
					t.Errorf("the body gives %d criteria and %d research rows, the table has %d and %d", c, r, row.criteria, row.research)
				}
				_, out, _ := sizeCall(fixtureIssue(t, row.id))
				if got := at(decodeReport(t, out), "decision"); got != wantDecision {
					t.Errorf("from the body: %v, recorded outcome %s", got, wantDecision)
				}
			}
			_, out, _ := sizeCall(structuredIssue(row.id, row.criteria, row.research, nil))
			if got := at(decodeReport(t, out), "decision"); got != wantDecision {
				t.Errorf("from the counts: %v, recorded outcome %s", got, wantDecision)
			}
		})
	}
	for _, id := range []string{"CRW-184", "CRW-183"} {
		if code, _, _ := sizeCall(fixtureIssue(t, id)); code != 1 {
			t.Errorf("%s is not split_recommended", id)
		}
	}
	for n := 260; n <= 272; n++ {
		id := fmt.Sprintf("CRW-%d", n)
		if code, _, _ := sizeCall(fixtureIssue(t, id)); code != 0 {
			t.Errorf("%s finished small and is not ok", id)
		}
	}
}
