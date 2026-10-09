package manage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
)

// C1: a clean merge gives a record the contract's validator accepts, with the head, dev, node,
// criteria digest, grader, criteria and defects d1.., and no disposition; the bundle holds the
// merged tree; the checkout is not changed and no ref is made; the audit state is not touched.
func TestPremergeEvalCleanMergeWritesARecordTheContractAccepts(t *testing.T) {
	f := premergeTestNew(t, premergeTestOptions{})
	refsBefore := premergeTestGit(t, f.repo.checkout, "for-each-ref")
	statusBefore := premergeTestGit(t, f.repo.checkout, "status", "--porcelain")
	headBefore := premergeTestGit(t, f.repo.checkout, "rev-parse", "HEAD")

	result, err := f.eval(PremergeEvalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Record != f.recordPath() || result.Grade != filepath.Join(f.bundle(), "grade.json") {
		t.Errorf("paths = %q %q", result.Record, result.Grade)
	}
	raw, err := os.ReadFile(result.Record)
	if err != nil {
		t.Fatal(err)
	}
	record, err := premerge.Decode(raw)
	if err != nil {
		t.Fatalf("the contract's validator refuses the record: %v\n%s", err, raw)
	}
	promptSum := sha256.Sum256([]byte(premergePrompt))
	if record.Schema != "premerge-record/1" || record.Issue != premergeTestIssue || record.Node != premergeTestNode ||
		record.Head != f.repo.head || record.Dev != f.repo.dev || record.CriteriaDigest != premergeTestNodeDigest(premergeTestNode) {
		t.Errorf("subject = %+v", record)
	}
	if want := (premerge.Grader{Model: premergeTestGrader, Effort: premergeTestEffort, PromptDigest: "sha256:" + hex.EncodeToString(promptSum[:])}); record.Grader != want {
		t.Errorf("grader = %+v, want %+v", record.Grader, want)
	}
	if record.GradedAt != "2026-10-09T01:02:03Z" || record.Score == nil || *record.Score != 6 || record.Summary != "needs one more round" {
		t.Errorf("graded at %q score %v summary %q", record.GradedAt, record.Score, record.Summary)
	}
	if want := map[string]premerge.Criterion{"c1": {Verdict: "PASS", Evidence: "a.go:3"}, "c2": {Verdict: "PARTIAL", Evidence: "feature.go:3"}}; !reflect.DeepEqual(record.Criteria, want) {
		t.Errorf("criteria = %+v", record.Criteria)
	}
	if want := []premerge.Defect{
		{ID: "d1", Severity: "P1", Impact: "criterion_unmet", Introduced: true, InPromise: true, What: "first"},
		{ID: "d2", Severity: "P3", Impact: "minor_separable", What: "second"},
	}; !reflect.DeepEqual(record.Defects, want) {
		t.Errorf("defects = %+v", record.Defects)
	}
	if record.Dispositions.By == "" || len(record.Dispositions.Items) != 0 {
		t.Errorf("dispositions = %+v, want a name and no item", record.Dispositions)
	}
	// What the contract has no place for stays in the bundle's grade.json, which the output names.
	if grade, err := os.ReadFile(result.Grade); err != nil || !strings.Contains(string(grade), `"trigger":"run it"`) {
		t.Errorf("grade.json = %q, %v", grade, err)
	}
	// the output
	if result.PR != premergeTestPR || result.Issue != premergeTestIssue || result.Node != premergeTestNode ||
		result.Head != f.repo.head || result.Dev != f.repo.dev || result.Score != 6 {
		t.Errorf("result = %+v", result)
	}
	if !reflect.DeepEqual(result.NotPass, []string{"c2"}) {
		t.Errorf("not_pass = %v", result.NotPass)
	}
	if want := []PremergeDefect{{"d1", "P1", "criterion_unmet", true, true}, {"d2", "P3", "minor_separable", false, false}}; !reflect.DeepEqual(result.Defects, want) {
		t.Errorf("defects = %+v", result.Defects)
	}
	// the bundle: the merged tree has the pull request's files and dev's, the diff is the pull request's change
	tree := filepath.Join(f.bundle(), "candidate", "tree")
	for path, want := range map[string]string{"a.go": "var A = 2", "feature.go": "Feature", "dev_only.go": "DevOnly", "README.md": "readme"} {
		if body, err := os.ReadFile(filepath.Join(tree, path)); err != nil || !strings.Contains(string(body), want) {
			t.Errorf("tree/%s = %q, %v; want it to hold %q", path, body, err, want)
		}
	}
	diff, err := os.ReadFile(filepath.Join(f.bundle(), "candidate", "diff.patch"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diff), "+var A = 2") || !strings.Contains(string(diff), "feature.go") || strings.Contains(string(diff), "DevOnly") {
		t.Errorf("diff.patch is not the pull request's change against dev:\n%s", diff)
	}
	pr, err := os.ReadFile(filepath.Join(f.bundle(), "candidate", "pr.md"))
	if err != nil || !strings.Contains(string(pr), premergeTestIssue+": the pre-merge evaluation") || !strings.Contains(string(pr), "the body") {
		t.Errorf("pr.md = %q, %v", pr, err)
	}
	criteria, err := os.ReadFile(filepath.Join(f.bundle(), "criteria.md"))
	if err != nil || !strings.Contains(string(criteria), "c1: the first thing") || !strings.Contains(string(criteria), "c2: the second thing") ||
		strings.Contains(string(criteria), "plugin manifest") {
		t.Errorf("criteria.md = %q, %v", criteria, err)
	}
	if prompt, err := os.ReadFile(filepath.Join(f.bundle(), "prompt.md")); err != nil || string(prompt) != premergePrompt {
		t.Errorf("the grader was not given the embedded prompt: %v", err)
	}
	// gh was asked once, for the four fields
	wantGh := [][]string{{"pr", "view", "7", "--repo", "example/repository", "--json", "number,title,body,headRefOid,state"}}
	if !reflect.DeepEqual(*f.gh, wantGh) {
		t.Errorf("gh calls = %v", *f.gh)
	}
	// the checkout: working tree, HEAD and refs are as they were, and nothing holds the merge commit
	if got := premergeTestGit(t, f.repo.checkout, "for-each-ref"); got != refsBefore {
		t.Errorf("the checkout's refs changed:\n%s\n--\n%s", refsBefore, got)
	}
	if got := premergeTestGit(t, f.repo.checkout, "status", "--porcelain"); got != statusBefore {
		t.Errorf("the checkout's working tree changed: %q", got)
	}
	if got := premergeTestGit(t, f.repo.checkout, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("HEAD moved to %s", got)
	}
	// the audit ledger and alerts are not written
	if _, err := os.Stat(filepath.Join(f.state, "audit")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the audit state exists: %v", err)
	}
}

// C2: a conflict only in the plugin manifest is graded on dev's manifest, and the criteria say so.
func TestPremergeEvalManifestOnlyConflictGradesTheDevManifest(t *testing.T) {
	const manifest = "plugins/crw/.codex-plugin/plugin.json"
	spec := premergeTestCleanSpec()
	spec.prFiles[manifest] = "{\n  \"name\": \"crw\",\n  \"version\": \"0.4.0+bbbbbbbbbbbb\"\n}\n"
	spec.devFiles[manifest] = "{\n  \"name\": \"crw\",\n  \"version\": \"0.4.0+cccccccccccc\"\n}\n"
	f := premergeTestNew(t, premergeTestOptions{spec: &spec})
	if _, err := f.eval(PremergeEvalOptions{}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(f.bundle(), "candidate", "tree", filepath.FromSlash(manifest)))
	if err != nil || !strings.Contains(string(body), "cccccccccccc") || strings.Contains(string(body), "<<<<<<<") {
		t.Errorf("the manifest in the tree = %q, %v; want dev's", body, err)
	}
	criteria, err := os.ReadFile(filepath.Join(f.bundle(), "criteria.md"))
	if err != nil || !strings.Contains(string(criteria), "Evaluation note") || !strings.Contains(string(criteria), "version line") {
		t.Errorf("criteria.md carries no manifest note: %q, %v", criteria, err)
	}
	// the tree is the merge's: the pull request's other change is still there
	if body, err := os.ReadFile(filepath.Join(f.bundle(), "candidate", "tree", "feature.go")); err != nil || !strings.Contains(string(body), "Feature") {
		t.Errorf("feature.go = %q, %v", body, err)
	}
	if _, err := os.Stat(f.recordPath()); err != nil {
		t.Errorf("no record: %v", err)
	}
	// the temporary index is gone
	entries, err := os.ReadDir(f.bundles)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(f.bundle()) {
			t.Errorf("%s was left under the bundle root", entry.Name())
		}
	}
}

// C2: a conflict in any other file stops before grading, with the paths, and writes no record.
func TestPremergeEvalOtherConflictIsBaseRefreshRequired(t *testing.T) {
	const manifest = "plugins/crw/.codex-plugin/plugin.json"
	for name, spec := range map[string]premergeTestRepoSpec{
		"a source file": {prFiles: map[string]string{"a.go": "package a\n\nvar A = 2\n"}, devFiles: map[string]string{"a.go": "package a\n\nvar A = 3\n"}},
		"the manifest and a source file": {
			prFiles:  map[string]string{"a.go": "package a\n\nvar A = 2\n", manifest: "{\"version\": \"0.4.0+bbbbbbbbbbbb\"}\n"},
			devFiles: map[string]string{"a.go": "package a\n\nvar A = 3\n", manifest: "{\"version\": \"0.4.0+cccccccccccc\"}\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := premergeTestNew(t, premergeTestOptions{spec: &spec})
			_, err := f.eval(PremergeEvalOptions{})
			pe := premergeTestErr(t, err, 3, "base_refresh_required")
			if !strings.Contains(pe.Detail, "a.go") {
				t.Errorf("the refusal does not name the conflicting path: %s", pe.Detail)
			}
			premergeTestNoRecords(t, f.records)
			if f.graderRuns() != 0 {
				t.Error("the grader ran on a conflicting merge")
			}
		})
	}
}

// C3: a grader that leaves nothing, a document the format refuses, or a run past its limit is exit 1
// and writes no record.
func TestPremergeEvalGradeFailuresWriteNoRecord(t *testing.T) {
	good := map[string]any{}
	if err := json.Unmarshal([]byte(premergeTestGrade), &good); err != nil {
		t.Fatal(err)
	}
	variant := func(change func(map[string]any)) string {
		doc := map[string]any{}
		if err := json.Unmarshal([]byte(premergeTestGrade), &doc); err != nil {
			t.Fatal(err)
		}
		change(doc)
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	defect := func(doc map[string]any, change func(map[string]any)) {
		change(doc["defects"].([]any)[0].(map[string]any))
	}
	for _, tc := range []struct {
		name, mode, grade string
	}{
		{"no file", "nofile", ""},
		{"malformed", "json", "{oops"},
		{"not an object", "json", "[]"},
		{"no criteria", "json", variant(func(d map[string]any) { d["criteria"] = map[string]any{} })},
		{"criteria as a list", "json", variant(func(d map[string]any) { d["criteria"] = []any{} })},
		{"an unknown verdict", "json", variant(func(d map[string]any) { d["criteria"].(map[string]any)["c1"].(map[string]any)["verdict"] = "MAYBE" })},
		{"no verdict", "json", variant(func(d map[string]any) { delete(d["criteria"].(map[string]any)["c1"].(map[string]any), "verdict") })},
		{"a registered criterion left out", "json", variant(func(d map[string]any) { delete(d["criteria"].(map[string]any), "c2") })},
		{"only a criterion nobody registered", "json", variant(func(d map[string]any) {
			d["criteria"] = map[string]any{"invented": map[string]any{"verdict": "PASS", "evidence": "x"}}
		})},
		{"an extra criterion nobody registered", "json", variant(func(d map[string]any) {
			d["criteria"].(map[string]any)["invented"] = map[string]any{"verdict": "PASS", "evidence": "x"}
		})},
		{"defects not a list", "json", variant(func(d map[string]any) { d["defects"] = "none" })},
		{"an unknown severity", "json", variant(func(d map[string]any) { defect(d, func(x map[string]any) { x["severity"] = "P4" }) })},
		{"no severity", "json", variant(func(d map[string]any) { defect(d, func(x map[string]any) { delete(x, "severity") }) })},
		{"an unknown impact", "json", variant(func(d map[string]any) { defect(d, func(x map[string]any) { x["impact"] = "huge" }) })},
		{"no impact", "json", variant(func(d map[string]any) { defect(d, func(x map[string]any) { delete(x, "impact") }) })},
		{"introduced as text", "json", variant(func(d map[string]any) { defect(d, func(x map[string]any) { x["introduced"] = "yes" }) })},
		{"no introduced", "json", variant(func(d map[string]any) { defect(d, func(x map[string]any) { delete(x, "introduced") }) })},
		{"in_promise as a number", "json", variant(func(d map[string]any) { defect(d, func(x map[string]any) { x["in_promise"] = 1 }) })},
		{"no in_promise", "json", variant(func(d map[string]any) { defect(d, func(x map[string]any) { delete(x, "in_promise") }) })},
		{"score above ten", "json", variant(func(d map[string]any) { d["score"] = 11 })},
		{"score below zero", "json", variant(func(d map[string]any) { d["score"] = -1 })},
		{"a fractional score", "json", variant(func(d map[string]any) { d["score"] = 5.5 })},
		{"a score as text", "json", variant(func(d map[string]any) { d["score"] = "7" })},
		{"no score", "json", variant(func(d map[string]any) { delete(d, "score") })},
		{"summary as a number", "json", variant(func(d map[string]any) { d["summary"] = 3 })},
		{"past the time limit", "timeout", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := premergeTestNew(t, premergeTestOptions{audit: map[string]any{"grader_timeout_seconds": 1}})
			f.setGrade(tc.mode, tc.grade)
			_, err := f.eval(PremergeEvalOptions{})
			var pe *PremergeError
			if !errors.As(err, &pe) || pe.Exit != 1 {
				t.Fatalf("error = %v, want exit 1", err)
			}
			premergeTestNoRecords(t, f.records)
			if f.graderRuns() != 1 {
				t.Errorf("the grader ran %d times, want 1", f.graderRuns())
			}
		})
	}
}

// A grade the grader left in an earlier run is not this run's: the bundle is rebuilt and a grader that
// writes nothing leaves no record.
func TestPremergeEvalIgnoresAGradeFromAnEarlierRun(t *testing.T) {
	f := premergeTestNew(t, premergeTestOptions{})
	if _, err := f.eval(PremergeEvalOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.recordPath()); err != nil {
		t.Fatal(err)
	}
	f.setGrade("nofile", "")
	_, err := f.eval(PremergeEvalOptions{})
	var pe *PremergeError
	if !errors.As(err, &pe) || pe.Exit != 1 {
		t.Fatalf("error = %v, want exit 1", err)
	}
	if _, err := os.Stat(f.recordPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a record was written from an old grade: %v", err)
	}
}

// C4: an existing record for the head is not overwritten without --replace and the grader does not
// run; with it, the old record is kept beside the new one.
func TestPremergeEvalExistingRecordNeedsReplace(t *testing.T) {
	f := premergeTestNew(t, premergeTestOptions{})
	if _, err := f.eval(PremergeEvalOptions{}); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(f.recordPath())
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.eval(PremergeEvalOptions{})
	premergeTestErr(t, err, 3, "record_exists")
	if now, _ := os.ReadFile(f.recordPath()); !bytes.Equal(now, old) {
		t.Error("the record was changed without --replace")
	}
	if f.graderRuns() != 1 {
		t.Errorf("the grader ran %d times, want only the first", f.graderRuns())
	}
	f.setGrade("json", premergeTestCleanGrade)
	if _, err := f.eval(PremergeEvalOptions{Replace: true}); err != nil {
		t.Fatal(err)
	}
	kept, err := os.ReadFile(filepath.Join(f.records, "pr7-"+f.repo.head[:8]+".replaced-20261009T010203Z.json"))
	if err != nil || !bytes.Equal(kept, old) {
		t.Errorf("the old record was not kept: %v", err)
	}
	if now := premergeTestReadRecord(t, f.recordPath()); now["score"].(float64) != 9 {
		t.Errorf("the new record's score = %v", now["score"])
	}
	entries, err := os.ReadDir(f.records)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("the record directory holds %d entries, want the record and the one kept", len(entries))
	}
}

// C4: --head must be the head the pull request is at now.
func TestPremergeEvalHeadMovedIsExitTwo(t *testing.T) {
	f := premergeTestNew(t, premergeTestOptions{})
	_, err := f.eval(PremergeEvalOptions{Head: strings.Repeat("0", 40)})
	premergeTestErr(t, err, 2, "head_moved")
	premergeTestNoRecords(t, f.records)
	if f.graderRuns() != 0 {
		t.Error("the grader ran on a head that moved")
	}
	for _, head := range []string{f.repo.head, strings.ToUpper(f.repo.head), f.repo.head[:12]} {
		f2 := premergeTestNew(t, premergeTestOptions{})
		if _, err := f2.eval(PremergeEvalOptions{Head: head}); err != nil {
			t.Errorf("--head %s: %v", head, err)
		}
	}
}

// C4: a pull request that is not open is not graded.
func TestPremergeEvalPullRequestNotOpen(t *testing.T) {
	for _, state := range []string{"MERGED", "CLOSED"} {
		f := premergeTestNew(t, premergeTestOptions{prState: state})
		_, err := f.eval(PremergeEvalOptions{})
		premergeTestErr(t, err, 3, "pr_not_open")
		if f.graderRuns() != 0 {
			t.Error("the grader ran")
		}
	}
}

// C4: the issue key is the first match of the audit issue pattern in the title; none is exit 3.
func TestPremergeEvalIssueKey(t *testing.T) {
	f := premergeTestNew(t, premergeTestOptions{title: "no key in this title"})
	_, err := f.eval(PremergeEvalOptions{})
	premergeTestErr(t, err, 3, "issue_unknown")
	// the first match wins, and the audit issue_pattern is the one that reads the title
	f = premergeTestNew(t, premergeTestOptions{title: "Follow up of CRW-1 for CRW-953", audit: map[string]any{"issue_pattern": `CRW-953`}})
	if _, err := f.eval(PremergeEvalOptions{}); err != nil {
		t.Fatal(err)
	}
	f = premergeTestNew(t, premergeTestOptions{title: "CRW-12: and CRW-953"})
	_, err = f.eval(PremergeEvalOptions{})
	premergeTestErr(t, err, 3, "node_unknown")
}

// C4: the node is the live node of the issue in a plan; a second live node needs --node, none is
// exit 3, and a node that is cancelled or is not an implementation node is not one.
func TestPremergeEvalNodeResolution(t *testing.T) {
	two := []premergeTestNodeSpec{{"n-953", premergeTestIssue, "implementation"}, {"n-953b", premergeTestIssue, "implementation"}}
	f := premergeTestNew(t, premergeTestOptions{nodes: two, criteriaNode: "n-953b"})
	_, err := f.eval(PremergeEvalOptions{})
	pe := premergeTestErr(t, err, 3, "node_unknown")
	if !strings.Contains(pe.Detail, "n-953") || !strings.Contains(pe.Detail, "n-953b") {
		t.Errorf("the refusal does not name the nodes: %s", pe.Detail)
	}
	if f.graderRuns() != 0 {
		t.Error("the grader ran")
	}
	result, err := f.eval(PremergeEvalOptions{Node: "n-953b"})
	if err != nil || result.Node != "n-953b" {
		t.Fatalf("--node n-953b: %+v, %v", result, err)
	}
	if record := premergeTestReadRecord(t, f.recordPath()); record["node"] != "n-953b" || record["criteria_digest"] != premergeTestNodeDigest("n-953b") {
		t.Errorf("the record is bound to %v / %v", record["node"], record["criteria_digest"])
	}
	f = premergeTestNew(t, premergeTestOptions{nodes: two})
	_, err = f.eval(PremergeEvalOptions{Node: "n-other"})
	premergeTestErr(t, err, 3, "node_unknown")

	f = premergeTestNew(t, premergeTestOptions{nodes: []premergeTestNodeSpec{{"n-other", "CRW-12", "implementation"}}})
	_, err = f.eval(PremergeEvalOptions{})
	premergeTestErr(t, err, 3, "node_unknown")

	f = premergeTestNew(t, premergeTestOptions{nodes: []premergeTestNodeSpec{{"n-953", premergeTestIssue, "non_pr"}}})
	_, err = f.eval(PremergeEvalOptions{})
	premergeTestErr(t, err, 3, "node_unknown")

	// a node the plan cancelled is not alive: the reopened issue has one live node again
	f = premergeTestNew(t, premergeTestOptions{nodes: two, criteriaNode: "n-953b", changes: [][]map[string]any{{{"op": "cancel_node", "node_id": "n-953"}}}})
	result, err = f.eval(PremergeEvalOptions{})
	if err != nil || result.Node != "n-953b" {
		t.Fatalf("with the first node cancelled: %+v, %v", result, err)
	}
}

// C4: the project key narrows the plans the node is looked for in.
func TestPremergeEvalProjectNarrowsThePlans(t *testing.T) {
	f := premergeTestNew(t, premergeTestOptions{premerge: map[string]any{"project": "another-project"}})
	_, err := f.eval(PremergeEvalOptions{})
	premergeTestErr(t, err, 3, "node_unknown")
	f = premergeTestNew(t, premergeTestOptions{premerge: map[string]any{"project": "project-1"}})
	if _, err := f.eval(PremergeEvalOptions{}); err != nil {
		t.Fatal(err)
	}
}

// Criteria the relay holds none of are no evaluation: the record binds a digest of criteria the grader
// never saw.
func TestPremergeEvalWithoutRegisteredCriteriaIsRefused(t *testing.T) {
	f := premergeTestNew(t, premergeTestOptions{noAssignment: true})
	_, err := f.eval(PremergeEvalOptions{})
	premergeTestErr(t, err, 3, "criteria_unavailable")
	f = premergeTestNew(t, premergeTestOptions{criteria: `{"relationshipId":"rel-1","criteria":[],"setDigest":null}`})
	_, err = f.eval(PremergeEvalOptions{})
	premergeTestErr(t, err, 3, "criteria_unavailable")
	if f.graderRuns() != 0 {
		t.Error("the grader ran")
	}
}

// C4/C9: the grader and the checkout come from the configuration only.
func TestPremergeEvalConfigurationRefusals(t *testing.T) {
	for name, opts := range map[string]premergeTestOptions{
		"no grader model":  {premerge: map[string]any{"grader_model": nil}},
		"no grader effort": {premerge: map[string]any{"grader_effort": nil}},
		"no grader":        {audit: map[string]any{"grader": []string{}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := premergeTestNew(t, opts)
			_, err := f.eval(PremergeEvalOptions{})
			premergeTestErr(t, err, 3, "grader_unconfigured")
			premergeTestNoRecords(t, f.records)
		})
	}
	t.Run("no checkout", func(t *testing.T) {
		f := premergeTestNew(t, premergeTestOptions{})
		delete(f.cfg.raw, "checkout")
		_, err := f.eval(PremergeEvalOptions{})
		premergeTestErr(t, err, 3, "checkout_unconfigured")
	})
}

// C5: the scrub strings are in none of pr.md, the diff, the criteria, the changed files and their
// path names; a name the scrub changes is written under the changed name.
func TestPremergeEvalScrubsTheBundle(t *testing.T) {
	spec := premergeTestRepoSpec{
		prFiles: map[string]string{
			"a.go": "package a\n\n// " + premergeTestScrub + " was here\nvar A = 2\n",
			"docs/" + premergeTestScrub + "-guide.md": "a guide to " + premergeTestScrub + "\n",
		},
		devFiles: map[string]string{"dev_only.go": "package a\n"},
	}
	criteria := strings.Replace(premergeTestCriteriaJSON, "the first thing", "the "+premergeTestScrub+" thing", 1)
	f := premergeTestNew(t, premergeTestOptions{spec: &spec, title: premergeTestIssue + ": add " + premergeTestScrub, body: "uses " + premergeTestScrub, criteria: criteria})
	if _, err := f.eval(PremergeEvalOptions{}); err != nil {
		t.Fatal(err)
	}
	walked := 0
	err := filepath.WalkDir(f.bundle(), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(f.bundle(), path)
		if strings.Contains(rel, premergeTestScrub) {
			t.Errorf("the path %s carries the scrub string", rel)
		}
		if entry.IsDir() {
			return nil
		}
		walked++
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if rel != "grade.json" && strings.Contains(string(body), premergeTestScrub) {
			t.Errorf("%s carries the scrub string", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(f.bundle(), "candidate", "tree", "docs", "[redacted]-guide.md")); err != nil || !strings.Contains(string(body), "[redacted]") {
		t.Errorf("the scrubbed file is not under its scrubbed name: %q, %v", body, err)
	}
	if walked < 6 {
		t.Errorf("only %d files were looked at", walked)
	}
}

// C4: the inputs directory brings its regular files; a link or a directory in it is refused.
func TestPremergeEvalInputs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "issue.md"), []byte("the issue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := premergeTestNew(t, premergeTestOptions{})
	if _, err := f.eval(PremergeEvalOptions{Inputs: dir}); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(f.bundle(), "inputs", "issue.md")); err != nil || string(body) != "the issue\n" {
		t.Errorf("inputs/issue.md = %q, %v", body, err)
	}
	for name, setup := range map[string]func(string){
		"a symbolic link": func(d string) { _ = os.Symlink(filepath.Join(d, "issue.md"), filepath.Join(d, "link.md")) },
		"a directory":     func(d string) { _ = os.Mkdir(filepath.Join(d, "sub"), 0o755) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := t.TempDir()
			if err := os.WriteFile(filepath.Join(bad, "issue.md"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			setup(bad)
			f := premergeTestNew(t, premergeTestOptions{})
			_, err := f.eval(PremergeEvalOptions{Inputs: bad})
			premergeTestErr(t, err, 3, "inputs_rejected")
			premergeTestNoRecords(t, f.records)
			if f.graderRuns() != 0 {
				t.Error("the grader ran")
			}
		})
	}
	f = premergeTestNew(t, premergeTestOptions{})
	_, err := f.eval(PremergeEvalOptions{Inputs: filepath.Join(dir, "absent")})
	premergeTestErr(t, err, 3, "inputs_rejected")
}

// A link in the merged tree is refused with its path before the grader runs: it could point out of the
// bundle, and leaving it out would grade a tree that is not the merge. No record is written.
func TestPremergeEvalRefusesALinkInTheMergedTree(t *testing.T) {
	spec := premergeTestCleanSpec()
	spec.prFiles["escape"] = "link:/etc"
	f := premergeTestNew(t, premergeTestOptions{spec: &spec})
	_, err := f.eval(PremergeEvalOptions{})
	pe := premergeTestErr(t, err, 3, "bundle_rejected")
	if !strings.Contains(pe.Detail, "escape") {
		t.Errorf("the refusal does not name the link: %s", pe.Detail)
	}
	if f.graderRuns() != 0 {
		t.Error("the grader ran")
	}
	premergeTestNoRecords(t, f.records)
	if _, err := os.Lstat(f.bundle()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a bundle was left: %v", err)
	}
}

// A submodule entry has no content in the bundle either: the tree would not be the merge, so it is refused.
func TestPremergeEvalRefusesASubmoduleInTheMergedTree(t *testing.T) {
	spec := premergeTestCleanSpec()
	spec.prFiles["vendor/inner"] = "gitlink:"
	f := premergeTestNew(t, premergeTestOptions{spec: &spec})
	_, err := f.eval(PremergeEvalOptions{})
	pe := premergeTestErr(t, err, 3, "bundle_rejected")
	if !strings.Contains(pe.Detail, "vendor/inner") {
		t.Errorf("the refusal does not name the submodule: %s", pe.Detail)
	}
	if f.graderRuns() != 0 {
		t.Error("the grader ran")
	}
	premergeTestNoRecords(t, f.records)
}

// The bundle's tree keeps the merge's executable bits: a file the change adds as 100755, a file whose
// only change is the bit, and an executable file from dev's side are executable (0700), the other files
// are not (0600). Without it a change to the bit is invisible and an executable helper cannot run.
func TestPremergeEvalBundleKeepsTheExecutableBit(t *testing.T) {
	spec := premergeTestCleanSpec()
	spec.prFiles["run.sh"] = "exec:#!/bin/sh\necho ok\n"
	spec.prFiles["README.md"] = "exec:readme\n" // the base's content: only the bit changes
	spec.devFiles["tool.sh"] = "exec:#!/bin/sh\necho dev\n"
	f := premergeTestNew(t, premergeTestOptions{spec: &spec})
	if mode := premergeTestGit(t, f.repo.checkout, "ls-tree", f.repo.head, "README.md"); !strings.HasPrefix(mode, "100755 ") {
		t.Fatalf("the fixture did not make README.md executable: %s", mode)
	}
	if _, err := f.eval(PremergeEvalOptions{}); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(f.bundle(), "candidate", "tree")
	for name, want := range map[string]os.FileMode{
		"run.sh": 0o700, "README.md": 0o700, "tool.sh": 0o700,
		"a.go": 0o600, "feature.go": 0o600, "dev_only.go": 0o600, "docs/old.md": 0o600,
	} {
		info, err := os.Lstat(filepath.Join(tree, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", name, info.Mode(), want)
		}
	}
}

// A grade must answer exactly the criteria the relay registered: the one a record binds a digest of.
// (The cases of a criterion left out and one nobody registered are rows of the grade-failure table.)
func TestPremergeEvalGradeCoversTheRegisteredCriteria(t *testing.T) {
	f := premergeTestNew(t, premergeTestOptions{})
	f.setGrade("json", premergeTestCleanGrade)
	if _, err := f.eval(PremergeEvalOptions{}); err != nil {
		t.Fatalf("a grade of exactly c1 and c2: %v", err)
	}
}

// The criteria the relay holds for the relationship must be the ones the chosen node fixed: a digest of
// other criteria is not the node's.
func TestPremergeEvalCriteriaOfAnotherNodeAreRefused(t *testing.T) {
	two := []premergeTestNodeSpec{{"n-953", premergeTestIssue, "implementation"}, {"n-953b", premergeTestIssue, "implementation"}}
	// the relationship's criteria are n-953's; the caller chooses n-953b
	f := premergeTestNew(t, premergeTestOptions{nodes: two, criteriaNode: "n-953"})
	_, err := f.eval(PremergeEvalOptions{Node: "n-953b"})
	pe := premergeTestErr(t, err, 3, "criteria_mismatch")
	for _, want := range []string{"n-953b", premergeTestNodeDigest("n-953b"), premergeTestNodeDigest("n-953")} {
		if !strings.Contains(pe.Detail, want) {
			t.Errorf("the refusal lacks %q: %s", want, pe.Detail)
		}
	}
	if f.graderRuns() != 0 {
		t.Error("the grader ran")
	}
	premergeTestNoRecords(t, f.records)
	// the node whose criteria they are is graded
	if _, err := f.eval(PremergeEvalOptions{Node: "n-953"}); err != nil {
		t.Fatalf("--node n-953: %v", err)
	}
	// a criteria answer with no digest binds nothing either
	f = premergeTestNew(t, premergeTestOptions{criteria: strings.Replace(premergeTestCriteriaJSON, `"setDigest":"d"`, `"setDigest":null`, 1)})
	_, err = f.eval(PremergeEvalOptions{})
	premergeTestErr(t, err, 3, "criteria_mismatch")
}

// A bundle_dir that is a relative path works from a working directory other than the checkout, also when a
// manifest conflict needs the temporary index below it.
func TestPremergeEvalRelativeBundleDirWithManifestConflict(t *testing.T) {
	const manifest = "plugins/crw/.codex-plugin/plugin.json"
	spec := premergeTestCleanSpec()
	spec.prFiles[manifest] = "{\n  \"name\": \"crw\",\n  \"version\": \"0.4.0+bbbbbbbbbbbb\"\n}\n"
	spec.devFiles[manifest] = "{\n  \"name\": \"crw\",\n  \"version\": \"0.4.0+cccccccccccc\"\n}\n"
	work := t.TempDir()
	t.Chdir(work)
	f := premergeTestNew(t, premergeTestOptions{spec: &spec, premerge: map[string]any{"bundle_dir": "rel-bundles"}})
	result, err := f.eval(PremergeEvalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(work, "rel-bundles", "pr-7-"+f.repo.head[:8], "candidate", "tree", filepath.FromSlash(manifest)))
	if err != nil || !strings.Contains(string(body), "cccccccccccc") {
		t.Errorf("the manifest in the tree = %q, %v; want dev's", body, err)
	}
	if _, err := os.Stat(result.Record); err != nil {
		t.Errorf("no record: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.repo.checkout, "rel-bundles")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the bundle root was made below the checkout: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(work, "rel-bundles"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "pr-7-"+f.repo.head[:8] {
			t.Errorf("%s was left under the bundle root", entry.Name())
		}
	}
}

// The bundle is made new for every run: nothing an earlier run left in it is there.
func TestPremergeEvalRebuildsTheBundle(t *testing.T) {
	f := premergeTestNew(t, premergeTestOptions{})
	stale := filepath.Join(f.bundle(), "candidate", "tree", "stale.txt")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.eval(PremergeEvalOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an earlier run's file is still in the bundle: %v", err)
	}
}

// C8: the command prints one JSON document and exits 0; a refusal prints nothing on stdout and the
// named reason on stderr; an output that cannot be written is exit 1 with one line on stderr.
func TestPremergeCommandOutputAndExitStatus(t *testing.T) {
	f := premergeTestNew(t, premergeTestOptions{})
	code := premergeRunWith(context.Background(), f.e, f.cfg, []string{"eval", "7", "--head", f.repo.head})
	if code != 0 || f.errOut.Len() != 0 {
		t.Fatalf("exit %d: %s", code, f.errOut)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(f.out.String()), &doc); err != nil || !strings.HasSuffix(f.out.String(), "}\n") || strings.Count(f.out.String(), "\n") != 1 {
		t.Fatalf("stdout is not one JSON document: %q, %v", f.out, err)
	}
	keys := make([]string, 0, len(doc))
	for key := range doc {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := []string{"defects", "dev", "grade", "head", "issue", "node", "not_pass", "pr", "record", "score"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}
	// the refusal
	f = premergeTestNew(t, premergeTestOptions{prState: "MERGED"})
	code = premergeRunWith(context.Background(), f.e, f.cfg, []string{"eval", "7"})
	if code != 3 || f.out.Len() != 0 || !strings.Contains(f.errOut.String(), "pr_not_open") || strings.Count(f.errOut.String(), "\n") != 1 {
		t.Errorf("a refusal: exit %d, stdout %q, stderr %q", code, f.out, f.errOut)
	}
	// head moved is a usage error
	f = premergeTestNew(t, premergeTestOptions{})
	code = premergeRunWith(context.Background(), f.e, f.cfg, []string{"eval", "7", "--head", strings.Repeat("1", 40)})
	if code != 2 || !strings.Contains(f.errOut.String(), "head_moved") {
		t.Errorf("head moved: exit %d, stderr %q", code, f.errOut)
	}
	// a grade that is not one
	f = premergeTestNew(t, premergeTestOptions{})
	f.setGrade("nofile", "")
	if code = premergeRunWith(context.Background(), f.e, f.cfg, []string{"eval", "7"}); code != 1 || f.out.Len() != 0 {
		t.Errorf("no grade: exit %d, stdout %q", code, f.out)
	}
	// stdout cannot be written
	f = premergeTestNew(t, premergeTestOptions{})
	f.e.Stdout = coreFailWriter{}
	code = premergeRunWith(context.Background(), f.e, f.cfg, []string{"eval", "7"})
	if code != 1 || strings.Count(f.errOut.String(), "\n") != 1 || !strings.Contains(f.errOut.String(), "the output is closed") {
		t.Errorf("a failed write: exit %d, stderr %q", code, f.errOut)
	}
}

// C1/C8: the command line is read strictly.
func TestPremergeCommandUsage(t *testing.T) {
	f := premergeTestNew(t, premergeTestOptions{})
	for _, args := range [][]string{
		nil,
		{"nope"},
		{"eval"},
		{"eval", "x"},
		{"eval", "0"},
		{"eval", "-3"},
		{"eval", "7", "8"},
		{"eval", "7", "--unknown", "x"},
		{"eval", "7", "--head"},
		{"eval", "7", "--head", "a", "--head", "b"},
		{"eval", "7", "--replace", "--replace"},
		{"eval", "7", "--head", "--node", "n"},
		{"dispose"},
		{"dispose", "record.json"},
		{"dispose", "record.json", "--ref", "c1", "--class", "blocking"},
		{"dispose", "record.json", "--ref", "c1", "--class", "blocking", "--note", "x", "--unknown", "y"},
		{"dispose", "record.json", "--ref", "c1", "--ref", "c2", "--class", "blocking", "--note", "x"},
		{"dispose", "record.json", "--ref", "c1", "--class", "blocking", "--note", "x", "--follow-up", "LIN-1"},
	} {
		f.out.Reset()
		f.errOut.Reset()
		if code := premergeRunWith(context.Background(), f.e, f.cfg, args); code != 2 || f.out.Len() != 0 || !strings.Contains(f.errOut.String(), "usage: crw manage premerge") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, f.out, f.errOut)
		}
	}
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}, {"eval", "--help"}, {"dispose", "-h"}} {
		f.out.Reset()
		f.errOut.Reset()
		if code := premergeRunWith(context.Background(), f.e, f.cfg, args); code != 0 || !strings.Contains(f.out.String(), "usage: crw manage premerge eval") || !strings.Contains(f.out.String(), "dispose") {
			t.Errorf("%q: exit %d, stdout %q", args, code, f.out)
		}
	}
}

// A configuration file this product cannot use is refused before any work, in Run's words and with
// exit 2, even when an option's value is -h or --help (which makes the shared entry point skip its own
// refusal); a real help request still prints the usage.
func TestPremergeCommandRefusesAnUnusableConfiguration(t *testing.T) {
	f, path := premergeTestEvaluated(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	coreConfigAt(t, coreConfigDocument(t, map[string]any{"management_thread": 123}))
	for _, args := range [][]string{
		{"premerge", "dispose", path, "--ref", "d1", "--class", "blocking", "--note", "-h", "--by", "me"},
		{"premerge", "dispose", path, "--ref", "d1", "--class", "blocking", "--note=--help"},
		{"premerge", "eval", "7", "--node", "-h"},
		{"premerge", "eval", "7"},
	} {
		code, out, errOut := coreRunManage(t, args...)
		if code != usageExit || out != "" || !strings.HasPrefix(errOut, "crw manage: error: ") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the record changed under a configuration the product cannot use")
	}
	if f.graderRuns() != 1 {
		t.Errorf("the grader ran %d times, want only the fixture's own run", f.graderRuns())
	}
	for _, args := range [][]string{{"premerge", "--help"}, {"premerge", "help"}, {"premerge", "dispose", "-h"}, {"premerge", "eval", "help"}} {
		code, out, errOut := coreRunManage(t, args...)
		if code != 0 || !strings.Contains(out, "usage: crw manage premerge eval") || errOut != "" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
}

// The command is registered, and asks for its usage by a wider rule than -h.
func TestPremergeCommandIsRegistered(t *testing.T) {
	found := false
	for _, c := range coreCommands() {
		if c.Name == "premerge" {
			found = true
			if c.HelpRequested == nil || !c.HelpRequested([]string{"help"}) || c.HelpRequested([]string{"eval", "7"}) {
				t.Error("the help rule is wrong")
			}
		}
	}
	if !found {
		t.Fatal("premerge is not registered")
	}
}

// premergeTestEvaluated runs a good evaluation and returns the record's path.
func premergeTestEvaluated(t *testing.T) (*premergeTestFixture, string) {
	t.Helper()
	f := premergeTestNew(t, premergeTestOptions{})
	result, err := f.eval(PremergeEvalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return f, result.Record
}

func premergeTestItems(t *testing.T, path string) []premerge.Item {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	record, err := premerge.Decode(raw)
	if err != nil {
		t.Fatalf("the contract's validator refuses the record: %v", err)
	}
	return record.Dispositions.Items
}

// C7: a disposition of a criterion or a defect of the record is written into the same record and the
// result passes the contract's validator; nothing else in the record changes.
func TestPremergeDisposeWritesTheItem(t *testing.T) {
	f, path := premergeTestEvaluated(t)
	before := premergeTestReadRecord(t, path)
	result, err := PremergeDispose(context.Background(), f.e, f.cfg, PremergeDisposeOptions{Record: path, Ref: "d2", Class: "separable", Note: "independent of this change", FollowUp: "CRW-99", By: "the parent"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Record != path || result.Ref != "d2" || result.Class != "separable" || result.By != "the parent" || result.At != "2026-10-09T01:02:03Z" || result.Replaced != "" {
		t.Errorf("result = %+v", result)
	}
	want := []premerge.Item{{Ref: "d2", Class: "separable", Note: "independent of this change", FollowUp: "CRW-99"}}
	if got := premergeTestItems(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("items = %+v", got)
	}
	after := premergeTestReadRecord(t, path)
	disposition := after["dispositions"].(map[string]any)
	if disposition["by"] != "the parent" {
		t.Errorf("by = %v", disposition["by"])
	}
	delete(before, "dispositions")
	delete(after, "dispositions")
	if !reflect.DeepEqual(before, after) {
		t.Error("a member other than the dispositions changed")
	}
	// a second ref adds an item; the criterion c2 is disposed the same way
	if _, err := PremergeDispose(context.Background(), f.e, f.cfg, PremergeDisposeOptions{Record: path, Ref: "c2", Class: "blocking", Note: "the criterion is not met", By: "the parent"}); err != nil {
		t.Fatal(err)
	}
	if got := premergeTestItems(t, path); len(got) != 2 {
		t.Errorf("items = %+v, want two", got)
	}
	// no classification rule is judged here: that is the relay's. A separable disposition of an unmet
	// criterion is written and left for dag-accept to refuse.
	if _, err := PremergeDispose(context.Background(), f.e, f.cfg, PremergeDisposeOptions{Record: path, Ref: "c2", Class: "separable", Note: "x", By: "the parent"}); err != nil {
		t.Errorf("the classification was judged: %v", err)
	}
}

// C7: a second disposition of the same ref replaces the first, and the record before it is kept.
func TestPremergeDisposeSameRefReplacesAndKeepsTheOldRecord(t *testing.T) {
	f, path := premergeTestEvaluated(t)
	if _, err := PremergeDispose(context.Background(), f.e, f.cfg, PremergeDisposeOptions{Record: path, Ref: "d1", Class: "blocking", Note: "first reading", By: "parent"}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := PremergeDispose(context.Background(), f.e, f.cfg, PremergeDisposeOptions{Record: path, Ref: "d1", Class: "already_resolved", Note: "fixed in the next commit, see a.go", By: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	kept := strings.TrimSuffix(path, ".json") + ".replaced-20261009T010203Z.json"
	if result.Replaced != kept {
		t.Errorf("replaced = %q, want %q", result.Replaced, kept)
	}
	if body, err := os.ReadFile(kept); err != nil || !bytes.Equal(body, first) {
		t.Errorf("the old record was not kept: %v", err)
	}
	got := premergeTestItems(t, path)
	if len(got) != 1 || got[0].Class != "already_resolved" {
		t.Errorf("items = %+v, want the one new disposition", got)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("the record directory holds %d entries (a temporary file left?), want 2", len(entries))
	}
	// a second replacement within the same second keeps its record under another name, not over the first
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err = PremergeDispose(context.Background(), f.e, f.cfg, PremergeDisposeOptions{Record: path, Ref: "d1", Class: "blocking", Note: "back to blocking", By: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(path, ".json") + ".replaced-20261009T010203Z-2.json"; result.Replaced != want {
		t.Errorf("replaced = %q, want %q", result.Replaced, want)
	}
	if body, err := os.ReadFile(result.Replaced); err != nil || !bytes.Equal(body, second) {
		t.Errorf("the second kept record is not the record it replaced: %v", err)
	}
	if body, err := os.ReadFile(kept); err != nil || !bytes.Equal(body, first) {
		t.Errorf("the first kept record was overwritten: %v", err)
	}
}

// C7: a ref the record does not hold, a class the contract does not know, a missing note and a malformed
// follow-up are exit 2 and the record is not changed.
func TestPremergeDisposeRefusals(t *testing.T) {
	f, path := premergeTestEvaluated(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]PremergeDisposeOptions{
		"an unknown ref":      {Ref: "d9", Class: "blocking", Note: "x", By: "p"},
		"a ref of nothing":    {Ref: "", Class: "blocking", Note: "x", By: "p"},
		"an unknown class":    {Ref: "d1", Class: "maybe", Note: "x", By: "p"},
		"no class":            {Ref: "d1", Class: "", Note: "x", By: "p"},
		"no note":             {Ref: "d1", Class: "blocking", Note: "", By: "p"},
		"a malformed follow":  {Ref: "d1", Class: "separable", Note: "x", FollowUp: "next week", By: "p"},
		"a score as the ref":  {Ref: "score", Class: "blocking", Note: "x", By: "p"},
		"a verdict-like ref":  {Ref: "PARTIAL", Class: "blocking", Note: "x", By: "p"},
		"a criterion's field": {Ref: "c1.verdict", Class: "blocking", Note: "x", By: "p"},
	} {
		t.Run(name, func(t *testing.T) {
			opts.Record = path
			_, err := PremergeDispose(context.Background(), f.e, f.cfg, opts)
			var pe *PremergeError
			if !errors.As(err, &pe) || pe.Exit != 2 {
				t.Fatalf("error = %v, want exit 2", err)
			}
			if now, _ := os.ReadFile(path); !bytes.Equal(now, before) {
				t.Error("the record changed")
			}
		})
	}
}

// C7: who disposes is --by or the one configured parent; with neither, it is a usage error.
func TestPremergeDisposeBy(t *testing.T) {
	f, path := premergeTestEvaluated(t)
	opts := PremergeDisposeOptions{Record: path, Ref: "d1", Class: "blocking", Note: "x"}
	result, err := PremergeDispose(context.Background(), f.e, f.cfg, opts)
	if err != nil || result.By != "parent-one" {
		t.Fatalf("the configured parent: %+v, %v", result, err)
	}
	f.cfg.Parents = map[string]string{"t1": "one", "t2": "two"}
	if _, err := PremergeDispose(context.Background(), f.e, f.cfg, opts); err == nil {
		t.Error("two parents and no --by was accepted")
	} else {
		premergeTestErr(t, err, 2, "by_unknown")
	}
	opts.By = "chosen"
	if result, err = PremergeDispose(context.Background(), f.e, f.cfg, opts); err != nil || result.By != "chosen" {
		t.Errorf("--by: %+v, %v", result, err)
	}
	f.cfg.Parents = nil
	opts.By = ""
	_, err = PremergeDispose(context.Background(), f.e, f.cfg, opts)
	premergeTestErr(t, err, 2, "by_unknown")
}

// C7: a record the contract's validator refuses is not disposed, and neither is a file that is no record.
func TestPremergeDisposeRefusesWhatIsNotARecord(t *testing.T) {
	f, path := premergeTestEvaluated(t)
	record := premergeTestReadRecord(t, path)
	delete(record, "criteria_digest")
	broken := filepath.Join(t.TempDir(), "broken.json")
	data, _ := json.Marshal(record)
	if err := os.WriteFile(broken, data, 0o600); err != nil {
		t.Fatal(err)
	}
	opts := PremergeDisposeOptions{Ref: "d1", Class: "blocking", Note: "x", By: "p"}
	for name, p := range map[string]string{"a record without a digest": broken, "no file": filepath.Join(t.TempDir(), "absent.json")} {
		opts.Record = p
		_, err := PremergeDispose(context.Background(), f.e, f.cfg, opts)
		var pe *PremergeError
		if !errors.As(err, &pe) || pe.Exit != 3 {
			t.Errorf("%s: error = %v, want exit 3", name, err)
		}
	}
	notJSON := filepath.Join(t.TempDir(), "x.json")
	if err := os.WriteFile(notJSON, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts.Record = notJSON
	if _, err := PremergeDispose(context.Background(), f.e, f.cfg, opts); err == nil {
		t.Error("a file that is not JSON was disposed")
	}
}

// C7/C8: the dispose command prints one JSON document; a record the contract passes stays one.
func TestPremergeDisposeCommand(t *testing.T) {
	f, path := premergeTestEvaluated(t)
	f.out.Reset()
	code := premergeRunWith(context.Background(), f.e, f.cfg, []string{"dispose", path, "--ref", "d1", "--class", "not_applicable", "--note", "the code shows the reading is wrong", "--by", "me"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, f.errOut)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(f.out.String()), &doc); err != nil || doc["ref"] != "d1" || doc["class"] != "not_applicable" || doc["by"] != "me" || doc["record"] != path {
		t.Errorf("stdout = %q, %v", f.out, err)
	}
	if items := premergeTestItems(t, path); len(items) != 1 || items[0].Note != "the code shows the reading is wrong" {
		t.Errorf("items = %+v", items)
	}
	// the record the evaluation wrote and the disposition completed is one the gate's judgment can read
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := premerge.Decode(raw); err != nil {
		t.Errorf("the validator refuses the disposed record: %v", err)
	}
}

// A clean evaluation passes the relay's own judgment as written: nothing waits for a disposition.
func TestPremergeCleanRecordPassesTheJudgment(t *testing.T) {
	f := premergeTestNew(t, premergeTestOptions{})
	f.setGrade("json", premergeTestCleanGrade)
	result, err := f.eval(PremergeEvalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(result.Record)
	if err != nil {
		t.Fatal(err)
	}
	record, err := premerge.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := premerge.Judge(record, premerge.Options{}); err != nil {
		t.Errorf("the judgment refuses a clean record: %v", err)
	}
	if len(result.NotPass) != 0 || len(result.Defects) != 0 {
		t.Errorf("result = %+v", result)
	}
}

// C9: the product code, its prompt and its tests name no path or thread of the host.
func TestPremergeFilesNameNoHostValue(t *testing.T) {
	files, err := filepath.Glob("premerge*")
	if err != nil || len(files) < 4 {
		t.Fatalf("files = %v, %v", files, err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{"/home/" + "jun", "/state/" + "crw", "/scratch/" + "crw", "/var/" + "tmp", ".omo/" + "mgmt", "pre-" + "eval"} {
			if strings.Contains(string(data), host) {
				t.Errorf("%s names the host value %q", file, host)
			}
		}
	}
}

// C2 (d1): a conflict in the manifest's version line is settled on that line alone: the tree takes dev's
// version and keeps every other change the pull request made to the manifest.
func TestPremergeEvalManifestVersionConflictKeepsTheOtherManifestChanges(t *testing.T) {
	const manifest = "plugins/crw/.codex-plugin/plugin.json"
	spec := premergeTestCleanSpec()
	spec.prFiles[manifest] = "{\n  \"name\": \"crw\",\n  \"description\": \"added by the pull request\",\n  \"version\": \"0.4.0+bbbbbbbbbbbb\"\n}\n"
	spec.devFiles[manifest] = "{\n  \"name\": \"crw\",\n  \"version\": \"0.4.0+cccccccccccc\"\n}\n"
	f := premergeTestNew(t, premergeTestOptions{spec: &spec})
	if _, err := f.eval(PremergeEvalOptions{}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(f.bundle(), "candidate", "tree", filepath.FromSlash(manifest)))
	want := "{\n  \"name\": \"crw\",\n  \"description\": \"added by the pull request\",\n  \"version\": \"0.4.0+cccccccccccc\"\n}\n"
	if err != nil || string(body) != want {
		t.Errorf("the manifest in the tree = %q, %v; want %q", body, err, want)
	}
	if patch, err := os.ReadFile(filepath.Join(f.bundle(), "candidate", "diff.patch")); err != nil || !strings.Contains(string(patch), "added by the pull request") {
		t.Errorf("the diff does not show the pull request's manifest change: %v", err)
	}
	criteria, err := os.ReadFile(filepath.Join(f.bundle(), "criteria.md"))
	if err != nil || !strings.Contains(string(criteria), "version line") || !strings.Contains(string(criteria), "other change") {
		t.Errorf("criteria.md does not say the manifest keeps the pull request's other changes: %q, %v", criteria, err)
	}
}

// C2 (d1): a manifest conflict outside the version line is base_refresh_required, whether or not the version
// conflicts too, and writes no record.
func TestPremergeEvalManifestConflictOutsideTheVersionLineIsRefused(t *testing.T) {
	const manifest = "plugins/crw/.codex-plugin/plugin.json"
	for name, spec := range map[string]premergeTestRepoSpec{
		"the name and the version": {
			prFiles:  map[string]string{manifest: "{\n  \"name\": \"crw-pr\",\n  \"version\": \"0.4.0+bbbbbbbbbbbb\"\n}\n"},
			devFiles: map[string]string{manifest: "{\n  \"name\": \"crw-dev\",\n  \"version\": \"0.4.0+cccccccccccc\"\n}\n"},
		},
		"the name only": {
			prFiles:  map[string]string{manifest: "{\n  \"name\": \"crw-pr\",\n  \"version\": \"0.4.0+aaaaaaaaaaaa\"\n}\n"},
			devFiles: map[string]string{manifest: "{\n  \"name\": \"crw-dev\",\n  \"version\": \"0.4.0+aaaaaaaaaaaa\"\n}\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := premergeTestNew(t, premergeTestOptions{spec: &spec})
			_, err := f.eval(PremergeEvalOptions{})
			pe := premergeTestErr(t, err, 3, "base_refresh_required")
			if !strings.Contains(pe.Detail, "plugin.json") {
				t.Errorf("the refusal does not name the manifest: %s", pe.Detail)
			}
			premergeTestNoRecords(t, f.records)
			if f.graderRuns() != 0 {
				t.Error("the grader ran on a conflicting merge")
			}
		})
	}
}

// C2 (d2): the evaluation works on the configured checkout whatever repository the process's own Git
// variables name, through the diff and the changed-path lookup as well as the merge.
func TestPremergeEvalIgnoresInheritedGitRepositoryVariables(t *testing.T) {
	f := premergeTestNew(t, premergeTestOptions{})
	other := t.TempDir()
	premergeTestGit(t, other, "init", "--quiet", "-b", "other")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	if _, err := f.eval(PremergeEvalOptions{}); err != nil {
		t.Fatalf("the evaluation followed the inherited Git variables: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(f.bundle(), "candidate", "tree", "feature.go")); err != nil || !strings.Contains(string(body), "Feature") {
		t.Errorf("feature.go = %q, %v", body, err)
	}
	if patch, err := os.ReadFile(filepath.Join(f.bundle(), "candidate", "diff.patch")); err != nil || !strings.Contains(string(patch), "feature.go") {
		t.Errorf("diff.patch = %q, %v", patch, err)
	}
}

// C7 (d3): a record that is a symbolic link is refused before anything is written, so the record it names
// is never left unchanged beside a new file that took the link's place.
func TestPremergeDisposeRefusesASymbolicLinkRecord(t *testing.T) {
	f, path := premergeTestEvaluated(t)
	if _, err := PremergeDispose(context.Background(), f.e, f.cfg, PremergeDisposeOptions{Record: path, Ref: "d1", Class: "blocking", Note: "first reading", By: "parent"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(path), "latest.json")
	if err := os.Symlink(filepath.Base(path), link); err != nil {
		t.Fatal(err)
	}
	entriesBefore, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	_, err = PremergeDispose(context.Background(), f.e, f.cfg, PremergeDisposeOptions{Record: link, Ref: "d1", Class: "blocking", Note: "updated parent decision", By: "parent"})
	if err == nil {
		t.Fatal("a disposition through a symbolic link was written")
	}
	var pe *PremergeError
	if !errors.As(err, &pe) || pe.Exit != 3 {
		t.Errorf("error = %v, want exit 3", err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Errorf("the record changed: %v", err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v", err)
	}
	entriesAfter, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesAfter) != len(entriesBefore) {
		t.Errorf("the record directory holds %d entries, was %d", len(entriesAfter), len(entriesBefore))
	}
}

// C3 (d3): --replace does not take the place of a link at the record path either: the record it names, and
// the kept copy, stay what they were.
func TestPremergeEvalReplaceRefusesASymbolicLinkRecord(t *testing.T) {
	f := premergeTestNew(t, premergeTestOptions{})
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.records, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, f.recordPath()); err != nil {
		t.Fatal(err)
	}
	_, err := f.eval(PremergeEvalOptions{Replace: true})
	premergeTestErr(t, err, 1, "record_write_failed")
	if info, err := os.Lstat(f.recordPath()); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v", err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "{}\n" {
		t.Errorf("the target changed: %q, %v", body, err)
	}
	entries, err := os.ReadDir(f.records)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the record directory holds %d entries, want the link alone", len(entries))
	}
}
