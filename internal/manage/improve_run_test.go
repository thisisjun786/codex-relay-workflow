package manage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The run tests are hermetic too: the synthetic store and the fake relay the 721 helpers
// build are reused, and every path is temporary. The manage state directory the run writes
// under is kept apart from the relay source directory it reads, as the two are on a host.

// improveRoadmapTestConfigure writes the crw configuration file: the manage state directory
// the run writes under, and the relay source it reads.
func improveRoadmapTestConfigure(t *testing.T, s *improveTestState, manageState string, improve map[string]any) {
	t.Helper()
	section := map[string]any{}
	for k, v := range improve {
		section[k] = v
	}
	if _, ok := section["sources"]; !ok {
		section["sources"] = map[string]any{"relay": map[string]any{"path": s.stateDir}}
	}
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{
		"state_dir": manageState,
		"improve":   section,
	}})
}

// improveRoadmapTestEnv is the Env the run tests use: the streams, the process environment,
// the fake executable, and a clock that advances on every read, so two runs of one test do
// not write the same roadmap file name.
func improveRoadmapTestEnv(s *improveTestState, stdout, stderr *strings.Builder) *Env {
	tick := int64(0)
	return &Env{
		Stdin:      strings.NewReader(""),
		Stdout:     stdout,
		Stderr:     stderr,
		Getenv:     os.Getenv,
		Now:        func() time.Time { tick++; return time.Unix(tick, 0).UTC() },
		Executable: s.fake,
	}
}

// improveRoadmapTestRun runs crw manage improve run on one Env and returns the exit status
// and both streams. The Env is passed in, so a test that runs twice shares one clock.
func improveRoadmapTestRun(t *testing.T, e *Env, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	e.Stdout, e.Stderr = &stdout, &stderr
	code := improveRunRoadmap(context.Background(), e, args)
	return code, stdout.String(), stderr.String()
}

// TestImproveRoadmapRefusesARefThatEscapesTheStateDirectory covers the security finding that
// a --ref carrying a path separator could move the bundle outside the managed state.
func TestImproveRoadmapRefusesARefThatEscapesTheStateDirectory(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveRoadmapTestConfigure(t, s, manageState, map[string]any{})
	var stdout, stderr strings.Builder
	e := improveRoadmapTestEnv(s, &stdout, &stderr)
	for _, ref := range []string{"../escape", "a/b", "..", ".", ""} {
		code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", ref)
		if code != usageExit {
			t.Errorf("the ref %q: exit %d, want %d (stderr %q)", ref, code, usageExit, errOut)
		}
	}
	if entries, err := os.ReadDir(s.root); err == nil {
		for _, entry := range entries {
			if entry.Name() == "escape" {
				t.Errorf("a ref escaped the state directory: %s", filepath.Join(s.root, entry.Name()))
			}
		}
	}
}

// improveRoadmapTestSeedRepeatedFriction inserts the fixed eight 2026-10-06 cases, all of
// them the same repeated friction (a body size estimate the merged size ran past), so collect
// yields eight kind split records that name one kind of blockage across three projects.
func improveRoadmapTestSeedRepeatedFriction(t *testing.T, db *sql.DB) {
	t.Helper()
	for i, c := range improveTestEightCases {
		rid := "rel-" + c.issue
		improveTestInsert(t, db, "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?,?,'active','parent','host','child','host',1,'[]','[]','2026-10-06T00:00:00Z','2026-10-06T00:00:00Z')", rid, c.issue)
		improveTestInsert(t, db, "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at) VALUES (?,?,'2026-10-06T00:00:00Z')", rid, c.project)
		at := fmt.Sprintf("2026-10-06T%02d:00:00Z", i+1)
		improveTestInsert(t, db, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES (?,?,1,?,?,?, 't','turn-1','completed',?,'final',?,?)",
			"ev-"+c.issue, rid, strings.Repeat("0", 64), "blocked_needs_input", "child", improveTestReceiptBody(t, false, "size estimate understated"), at, at)
	}
}

// improveRoadmapTestFiles lists the entries of one directory, sorted.
func improveRoadmapTestFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// improveRoadmapTestRoadmaps lists the roadmap documents below one manage state directory.
func improveRoadmapTestRoadmaps(t *testing.T, manageState string) []string {
	t.Helper()
	var out []string
	for _, name := range improveRoadmapTestFiles(t, filepath.Join(manageState, "improve")) {
		if strings.HasPrefix(name, "roadmap-") && strings.HasSuffix(name, ".md") {
			out = append(out, name)
		}
	}
	return out
}

// improveRoadmapTestBundles lists the per-run bundle documents one ref directory holds.
func improveRoadmapTestBundles(t *testing.T, manageState, ref string) []string {
	t.Helper()
	var out []string
	for _, name := range improveRoadmapTestFiles(t, filepath.Join(manageState, "improve", ref)) {
		if strings.HasPrefix(name, "bundle-") && strings.HasSuffix(name, ".json") {
			out = append(out, name)
		}
	}
	return out
}

// improveRoadmapTestRoadmapBody reads the one roadmap document a run wrote.
func improveRoadmapTestRoadmapBody(t *testing.T, manageState string) string {
	t.Helper()
	roadmaps := improveRoadmapTestRoadmaps(t, manageState)
	if len(roadmaps) != 1 {
		t.Fatalf("the improve directory holds %v, want one roadmap", roadmaps)
	}
	data, err := os.ReadFile(filepath.Join(manageState, "improve", roadmaps[0]))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// improveRoadmapTestDrafts lists the draft documents below one manage state directory.
func improveRoadmapTestDrafts(t *testing.T, manageState string) []string {
	t.Helper()
	return improveProposeTestDrafts(t, &improveProposeTestWorld{stateDir: manageState})
}

// TestImproveRoadmapRunLeavesBundleDraftsAndRoadmap covers C7: one run leaves a bundle, the
// drafts and a roadmap document.
func TestImproveRoadmapRunLeavesBundleDraftsAndRoadmap(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) { improveRoadmapTestSeedRepeatedFriction(t, db) })
	improveRoadmapTestConfigure(t, s, manageState, map[string]any{})
	var stdout, stderr strings.Builder
	e := improveRoadmapTestEnv(s, &stdout, &stderr)
	code, out, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", "M2")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr %s", code, errOut)
	}
	if path := strings.TrimSpace(out); !strings.HasPrefix(filepath.Base(path), "roadmap-") {
		t.Errorf("the run printed %q, want a roadmap path", out)
	}
	if got := improveRoadmapTestBundles(t, manageState, "M2"); len(got) != 1 {
		t.Errorf("the ref directory holds %v, want one bundle", got)
	}
	if drafts := improveRoadmapTestDrafts(t, manageState); len(drafts) != 1 {
		t.Errorf("the drafts directory holds %v, want one draft", drafts)
	}
	if roadmaps := improveRoadmapTestRoadmaps(t, manageState); len(roadmaps) != 1 {
		t.Errorf("the improve directory holds %v, want one roadmap", roadmaps)
	}
}

// TestImproveRoadmapRunSecondRunCreatesNoDraft covers C8: a second run for the same ref
// creates no new draft.
func TestImproveRoadmapRunSecondRunCreatesNoDraft(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) { improveRoadmapTestSeedRepeatedFriction(t, db) })
	improveRoadmapTestConfigure(t, s, manageState, map[string]any{})
	var stdout, stderr strings.Builder
	e := improveRoadmapTestEnv(s, &stdout, &stderr)
	if code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", "M2"); code != 0 {
		t.Fatalf("the first run: exit %d, stderr %s", code, errOut)
	}
	before := improveRoadmapTestDrafts(t, manageState)
	code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", "M2")
	if code != 0 {
		t.Fatalf("the second run: exit %d, stderr %s", code, errOut)
	}
	after := improveRoadmapTestDrafts(t, manageState)
	if len(after) != len(before) {
		t.Errorf("the second run changed the drafts from %v to %v", before, after)
	}
	if roadmaps := improveRoadmapTestRoadmaps(t, manageState); len(roadmaps) != 2 {
		t.Errorf("the improve directory holds %v, want two roadmaps", roadmaps)
	}
}

// TestImproveRoadmapDocumentCarriesAnEvidenceLocationPerCandidate covers C9: every candidate
// in the roadmap carries at least one evidence location.
func TestImproveRoadmapDocumentCarriesAnEvidenceLocationPerCandidate(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) { improveRoadmapTestSeedRepeatedFriction(t, db) })
	improveRoadmapTestConfigure(t, s, manageState, map[string]any{})
	var stdout, stderr strings.Builder
	e := improveRoadmapTestEnv(s, &stdout, &stderr)
	if code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "project", "--ref", "p1"); code != 0 {
		t.Fatalf("run: exit %d, stderr %s", code, errOut)
	}
	body := improveRoadmapTestRoadmapBody(t, manageState)
	sections := strings.Split(body, "### ")
	if len(sections) < 2 {
		t.Fatalf("the roadmap carries no candidate section:\n%s", body)
	}
	for _, section := range sections[1:] {
		if !strings.Contains(section, "- evidence:") {
			t.Errorf("a candidate carries no evidence section:\n%s", section)
		}
		rest := section[strings.Index(section, "- evidence:"):]
		if !strings.Contains(rest, "  - ") {
			t.Errorf("a candidate carries no evidence location:\n%s", section)
		}
		if strings.Contains(rest, "(the bundle recorded no origin location)") {
			t.Errorf("a candidate carries only the placeholder evidence:\n%s", section)
		}
		if !strings.Contains(rest, "events:") {
			t.Errorf("a candidate carries no receipt location:\n%s", section)
		}
	}
}

// TestImproveRoadmapRunRefusesAnUnreadableIssueList covers C5 through the run path: a
// configured issue list that cannot be read is the named error and no draft or roadmap
// document is created.
func TestImproveRoadmapRunRefusesAnUnreadableIssueList(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) { improveRoadmapTestSeedRepeatedFriction(t, db) })
	missing := filepath.Join(s.root, "issues.json")
	improveRoadmapTestConfigure(t, s, manageState, map[string]any{"issue_list": missing})
	var stdout, stderr strings.Builder
	e := improveRoadmapTestEnv(s, &stdout, &stderr)
	code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", "M2")
	if code != 1 || !strings.Contains(errOut, improveReasonSourceUnreadable) {
		t.Fatalf("an unreadable issue list: exit %d, stderr %q", code, errOut)
	}
	if roadmaps := improveRoadmapTestRoadmaps(t, manageState); len(roadmaps) != 0 {
		t.Errorf("the improve directory holds %v, want nothing", roadmaps)
	}
	if drafts := improveRoadmapTestDrafts(t, manageState); len(drafts) != 0 {
		t.Errorf("the drafts directory holds %v, want nothing", drafts)
	}
}

// TestImproveRoadmapRefusesAnUnknownBoundary covers the argument guard.
func TestImproveRoadmapRefusesAnUnknownBoundary(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveRoadmapTestConfigure(t, s, manageState, map[string]any{})
	var stdout, stderr strings.Builder
	e := improveRoadmapTestEnv(s, &stdout, &stderr)
	code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "week", "--ref", "M2")
	if code != usageExit || !strings.Contains(errOut, "boundary") {
		t.Fatalf("an unknown boundary: exit %d, stderr %q", code, errOut)
	}
}
