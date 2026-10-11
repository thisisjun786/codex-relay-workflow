package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1040 part a: `crw manage batch plan` selects the Backlog issues that carry the batch label and
// pass the release gate, and holds the rest with reasons; `crw manage batch report` writes the morning
// report in the crw-status format into the management state directory. Both are pure: the caller
// supplies the issue JSON and the run digest, and nothing is read from or written to Linear or the relay.

const batchReadyBody = "## 기준\n\n* c1: 한 가지가 된다.\n\n## 편집 영역\n\n* `internal/manage/batch.go`\n\n" +
	"## 정한 답\n\n판정은 한 곳에서 한다.\n\n## 먼저 실패하게 쓸 시험\n\n* 빈 입력은 거절된다.\n\n" +
	"## 끝 조건\n\n* `go test ./internal/manage -run Batch`가 통과한다.\n\n## 열린 결정\n\n없음\n"

func batchIssue(id, state string, labels []string, body string) map[string]any {
	return map[string]any{"id": id, "title": "title " + id, "description": body, "status": state, "labels": labels}
}

func batchRun(t *testing.T, e *Env, stdin string, args ...string) (int, string, string) {
	t.Helper()
	out, errOut := &strings.Builder{}, &strings.Builder{}
	e.Stdin, e.Stdout, e.Stderr = strings.NewReader(stdin), out, errOut
	code := Run(context.Background(), append([]string{"batch"}, args...), e.Stdin, out, errOut)
	return code, out.String(), errOut.String()
}

func batchJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBatchPlanSelectsOnlyBacklogLabelledReadyIssues(t *testing.T) {
	auditListHome(t)
	e, _, _ := auditTestEnv(t)
	issues := []map[string]any{
		batchIssue("CRW-1", "Backlog", []string{"night", "codex-relay-workflow"}, batchReadyBody),
		batchIssue("CRW-2", "In Progress", []string{"night"}, batchReadyBody),
		batchIssue("CRW-3", "Backlog", []string{"other"}, batchReadyBody),
		batchIssue("CRW-4", "Backlog", []string{"night"}, "## 배경\n\n본문만 있다.\n"),
		batchIssue("CRW-5", "Backlog", []string{"night", "design-first"}, batchReadyBody),
		batchIssue("CRW-6", "Backlog", []string{"NIGHT"}, batchReadyBody),
	}
	code, out, errOut := batchRun(t, e, batchJSON(t, issues), "plan", "--label", "night")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	var plan struct {
		Schema   string `json:"schema"`
		Label    string `json:"label"`
		Selected []struct {
			Issue string `json:"issue"`
			Title string `json:"title"`
		} `json:"selected"`
		Held []struct {
			Issue   string   `json:"issue"`
			Gate    string   `json:"gate"`
			Reasons []string `json:"reasons"`
		} `json:"held"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if plan.Schema != "crw-batch-plan/1" || plan.Label != "night" {
		t.Errorf("plan header: %s", out)
	}
	var selected []string
	for _, s := range plan.Selected {
		selected = append(selected, s.Issue)
	}
	if strings.Join(selected, ",") != "CRW-1,CRW-6" {
		t.Errorf("selected %v, want CRW-1,CRW-6 (the label matches without case)", selected)
	}
	gates := map[string]string{}
	reasons := map[string]string{}
	for _, h := range plan.Held {
		gates[h.Issue] = h.Gate
		reasons[h.Issue] = strings.Join(h.Reasons, " | ")
		if len(h.Reasons) == 0 {
			t.Errorf("%s is held with no reason", h.Issue)
		}
	}
	want := map[string]string{"CRW-2": "state", "CRW-3": "label", "CRW-4": "ready", "CRW-5": "ready"}
	for id, gate := range want {
		if gates[id] != gate {
			t.Errorf("%s held by gate %q, want %q (%s)", id, gates[id], gate, out)
		}
	}
	if !strings.Contains(reasons["CRW-2"], "In Progress") || !strings.Contains(reasons["CRW-3"], "night") {
		t.Errorf("state and label reasons name what they found: %v", reasons)
	}
	if !strings.Contains(reasons["CRW-4"], "missing") || !strings.Contains(reasons["CRW-5"], "design first") {
		t.Errorf("ready reasons come from the issue-ready gate: %v", reasons)
	}
}

func TestBatchPlanReadsLinearShapes(t *testing.T) {
	auditListHome(t)
	e, _, _ := auditTestEnv(t)
	// get_issue gives labels as names and the state as status/statusType; other tools give objects.
	in := `[
	 {"identifier":"CRW-7","title":"t","description":` + batchJSON(t, batchReadyBody) + `,"state":{"name":"Backlog","type":"backlog"},"labels":[{"name":"night"}]},
	 {"id":"CRW-8","title":"t","description":` + batchJSON(t, batchReadyBody) + `,"status":"Backlog","statusType":"backlog","labels":["night"]},
	 {"id":"CRW-9","title":"t","description":` + batchJSON(t, batchReadyBody) + `,"labels":["night"]}
	]`
	code, out, errOut := batchRun(t, e, in, "plan", "--label", "night")
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	var plan struct {
		Selected []struct{ Issue string } `json:"selected"`
		Held     []struct {
			Issue   string
			Reasons []string
		} `json:"held"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Selected) != 2 || plan.Selected[0].Issue != "CRW-7" || plan.Selected[1].Issue != "CRW-8" {
		t.Errorf("selected: %s", out)
	}
	if len(plan.Held) != 1 || plan.Held[0].Issue != "CRW-9" || !strings.Contains(strings.Join(plan.Held[0].Reasons, " "), "no state") {
		t.Errorf("an issue with no state is held, not guessed to be Backlog: %s", out)
	}
}

func TestBatchPlanHoldsDuplicatesAndRefusesUnusableInput(t *testing.T) {
	auditListHome(t)
	e, _, _ := auditTestEnv(t)
	dup := []map[string]any{
		batchIssue("CRW-1", "Backlog", []string{"night"}, batchReadyBody),
		batchIssue("CRW-1", "Backlog", []string{"night"}, batchReadyBody),
	}
	code, out, _ := batchRun(t, e, batchJSON(t, dup), "plan", "--label", "night")
	if code != 0 || strings.Count(out, `"issue":"CRW-1"`) != 2 || !strings.Contains(out, "duplicate") {
		t.Errorf("a repeated issue is selected once and the repeat is held: exit %d %s", code, out)
	}
	for name, tc := range map[string]struct {
		stdin string
		args  []string
	}{
		"no label":      {"[]", []string{"plan"}},
		"empty label":   {"[]", []string{"plan", "--label", " "}},
		"not json":      {"{", []string{"plan", "--label", "night"}},
		"not a list":    {`{"id":"x"}`, []string{"plan", "--label", "night"}},
		"no identifier": {`[{"title":"t","status":"Backlog"}]`, []string{"plan", "--label", "night"}},
		"bad labels":    {`[{"id":"CRW-1","labels":"night"}]`, []string{"plan", "--label", "night"}},
		"unknown flag":  {"[]", []string{"plan", "--label", "night", "--bogus", "x"}},
		"no subcommand": {"[]", nil},
	} {
		if code, out, _ := batchRun(t, e, tc.stdin, tc.args...); code != usageExit || strings.Contains(out, "crw-batch-plan") {
			t.Errorf("%s: exit %d, want %d with no plan: %s", name, code, usageExit, out)
		}
	}
}

func TestBatchPlanReadsAFileAndWritesNothing(t *testing.T) {
	state := auditListHome(t)
	e, _, _ := auditTestEnv(t)
	file := filepath.Join(t.TempDir(), "issues.json")
	if err := os.WriteFile(file, []byte(batchJSON(t, []map[string]any{batchIssue("CRW-1", "Backlog", []string{"night"}, batchReadyBody)})), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := batchRun(t, e, "", "plan", "--label", "night", "--issues", file)
	if code != 0 || !strings.Contains(out, `"selected":[{"issue":"CRW-1"`) {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("the plan created the state directory (%v)", err)
	}
	if code, _, _ := batchRun(t, e, "", "plan", "--label", "night", "--issues", file+".missing"); code == 0 {
		t.Errorf("a missing issues file exits 0")
	}
}

// The same Linear issue can be given as its UUID and as its identifier (the caller's list may hold both
// shapes); the plan keeps the id-identifier correspondence and judges it as one issue.
func TestBatchPlanJudgesAnIssueGivenAsUUIDAndIdentifierOnce(t *testing.T) {
	const uuid = "0b6f8a2e-1c3d-4e5f-9a7b-8c9d0e1f2a3b"
	body := batchJSON(t, batchReadyBody)
	withBoth := `{"id":"` + uuid + `","identifier":"CRW-1","title":"t","description":` + body + `,"status":"Backlog","labels":["night"]}`
	byUUID := `{"id":"` + uuid + `","title":"t","description":` + body + `,"status":"Backlog","labels":["night"]}`
	byIdentifier := `{"identifier":"CRW-1","title":"t","description":` + body + `,"status":"Backlog","labels":["night"]}`
	other := `{"id":"CRW-2","title":"t","description":` + body + `,"status":"Backlog","labels":["night"]}`
	for name, tc := range map[string]struct {
		list     []string
		selected string
		held     string
	}{
		"both then uuid":        {[]string{withBoth, byUUID}, "CRW-1", uuid},
		"uuid then both":        {[]string{byUUID, withBoth}, uuid, "CRW-1"},
		"both then identifier":  {[]string{withBoth, byIdentifier}, "CRW-1", "CRW-1"},
		"identifier then both":  {[]string{byIdentifier, withBoth}, "CRW-1", "CRW-1"},
		"both, other, uuid":     {[]string{withBoth, other, byUUID}, "CRW-1,CRW-2", uuid},
		"upper-case uuid again": {[]string{withBoth, strings.Replace(byUUID, uuid, strings.ToUpper(uuid), 1)}, "CRW-1", strings.ToUpper(uuid)},
	} {
		auditListHome(t)
		e, _, _ := auditTestEnv(t)
		code, out, errOut := batchRun(t, e, "["+strings.Join(tc.list, ",")+"]", "plan", "--label", "night")
		if code != 0 {
			t.Fatalf("%s: exit %d: %s%s", name, code, out, errOut)
		}
		var plan struct {
			Selected []struct{ Issue string }       `json:"selected"`
			Held     []struct{ Issue, Gate string } `json:"held"`
		}
		if err := json.Unmarshal([]byte(out), &plan); err != nil {
			t.Fatal(err)
		}
		var selected []string
		for _, s := range plan.Selected {
			selected = append(selected, s.Issue)
		}
		if strings.Join(selected, ",") != tc.selected {
			t.Errorf("%s: selected %v, want %s: the issue is selected once", name, selected, tc.selected)
		}
		if len(plan.Held) != 1 || plan.Held[0].Gate != batchGateDuplicate || plan.Held[0].Issue != tc.held {
			t.Errorf("%s: the repeat is held as a duplicate (%s): %s", name, tc.held, out)
		}
	}
}

// An entry that names an issue by an alias only a later entry ties to it is still that issue: the plan
// first collects every id-identifier correspondence of the whole input, so in any order of the UUID-only,
// identifier-only and both-names entries the issue is judged once, by its first entry, and the others are
// held as duplicates of it, also when the first entry is held.
func TestBatchPlanTiesAliasesAcrossTheWholeInput(t *testing.T) {
	const uuid = "0b6f8a2e-1c3d-4e5f-9a7b-8c9d0e1f2a3b"
	body := batchJSON(t, batchReadyBody)
	entry := func(names, status string) string {
		return `{` + names + `,"title":"t","description":` + body + `,"status":"` + status + `","labels":["night"]}`
	}
	u, i, both := `"id":"`+uuid+`"`, `"identifier":"CRW-1"`, `"id":"`+uuid+`","identifier":"CRW-1"`
	other := entry(`"id":"CRW-2"`, "Backlog")
	for name, tc := range map[string]struct {
		list     []string
		selected string
		held     string
	}{
		"uuid, both, identifier":        {[]string{entry(u, "Backlog"), entry(both, "Backlog"), entry(i, "Backlog")}, uuid, "CRW-1:duplicate,CRW-1:duplicate"},
		"identifier, both, uuid":        {[]string{entry(i, "Backlog"), entry(both, "Backlog"), entry(u, "Backlog")}, "CRW-1", "CRW-1:duplicate," + uuid + ":duplicate"},
		"uuid, identifier, both":        {[]string{entry(u, "Backlog"), entry(i, "Backlog"), entry(both, "Backlog")}, uuid, "CRW-1:duplicate,CRW-1:duplicate"},
		"identifier, uuid, both":        {[]string{entry(i, "Backlog"), entry(u, "Backlog"), entry(both, "Backlog")}, "CRW-1", uuid + ":duplicate,CRW-1:duplicate"},
		"both, uuid, identifier":        {[]string{entry(both, "Backlog"), entry(u, "Backlog"), entry(i, "Backlog")}, "CRW-1", uuid + ":duplicate,CRW-1:duplicate"},
		"both, identifier, uuid":        {[]string{entry(both, "Backlog"), entry(i, "Backlog"), entry(u, "Backlog")}, "CRW-1", "CRW-1:duplicate," + uuid + ":duplicate"},
		"uuid, other, identifier, both": {[]string{entry(u, "Backlog"), other, entry(i, "Backlog"), entry(both, "Backlog")}, uuid + ",CRW-2", "CRW-1:duplicate,CRW-1:duplicate"},
		"held first, both, identifier":  {[]string{entry(u, "In Progress"), entry(both, "Backlog"), entry(i, "Backlog")}, "", uuid + ":state,CRW-1:duplicate,CRW-1:duplicate"},
		"held first, identifier, both":  {[]string{entry(u, "In Progress"), entry(i, "Backlog"), entry(both, "Backlog")}, "", uuid + ":state,CRW-1:duplicate,CRW-1:duplicate"},
	} {
		issues, err := BatchParseIssues([]byte("[" + strings.Join(tc.list, ",") + "]"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		plan := BatchPlan("night", issues)
		var selected, held []string
		for _, s := range plan.Selected {
			selected = append(selected, s.Issue)
		}
		for _, h := range plan.Held {
			held = append(held, h.Issue+":"+h.Gate)
		}
		if strings.Join(selected, ",") != tc.selected || strings.Join(held, ",") != tc.held {
			t.Errorf("%s: selected %v held %v, want selected [%s] held [%s]", name, selected, held, tc.selected, tc.held)
		}
		for _, h := range plan.Held {
			if h.Gate == batchGateDuplicate && !strings.Contains(strings.Join(h.Reasons, " "), "first as "+issues[0].ID) {
				t.Errorf("%s: the duplicate %s does not name the first entry: %v", name, h.Issue, h.Reasons)
			}
		}
	}
}
