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
	f := premergeTestNew(t, premergeTestOptions{nodes: two})
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
	f = premergeTestNew(t, premergeTestOptions{nodes: two, changes: [][]map[string]any{{{"op": "cancel_node", "node_id": "n-953"}}}})
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

// A link in the merged tree is not made in the bundle: it could point out of it.
func TestPremergeEvalDoesNotMakeLinksInTheBundle(t *testing.T) {
	spec := premergeTestCleanSpec()
	spec.prFiles["escape"] = "link:/etc"
	f := premergeTestNew(t, premergeTestOptions{spec: &spec})
	if _, err := f.eval(PremergeEvalOptions{}); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(f.bundle(), func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type()&fs.ModeSymlink != 0 {
			t.Errorf("%s is a link", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.bundle(), "candidate", "tree", "escape")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the link entry was materialised: %v", err)
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
