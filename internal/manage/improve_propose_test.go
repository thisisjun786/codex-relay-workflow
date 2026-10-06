package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The propose tests are hermetic: every path is temporary, the configuration is a file
// this file writes, and no test reaches a real store, App Server, network or home.

// improveProposeTestWorld is a temporary world: the state directory the drafts live
// under and the crw configuration file that names it.
type improveProposeTestWorld struct {
	root     string
	stateDir string
	config   string
}

// improveProposeTestSetup points HOME, CODEX_HOME, CRW_HOME, XDG_STATE_HOME and
// XDG_CONFIG_HOME at one temporary tree, so no test reaches a real one.
func improveProposeTestSetup(t *testing.T) *improveProposeTestWorld {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	state := filepath.Join(root, "state")
	for _, dir := range []string{home, state, filepath.Join(root, "config")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CRW_HOME", filepath.Join(home, "crw"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("CRW_CONFIG", "")
	return &improveProposeTestWorld{root: root, stateDir: state, config: filepath.Join(root, "config", "crw", "config.json")}
}

// improveProposeTestConfigure writes the crw configuration file with a state_dir and the
// improve section, and points CRW_CONFIG at it.
func improveProposeTestConfigure(t *testing.T, w *improveProposeTestWorld, improve map[string]any) {
	t.Helper()
	doc := map[string]any{"manage": map[string]any{"state_dir": w.stateDir, "improve": improve}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(w.config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", w.config)
}

// improveProposeTestEnv is the Env the propose tests run with.
func improveProposeTestEnv(w *improveProposeTestWorld, stdout, stderr *strings.Builder) *Env {
	return &Env{
		Stdin:      strings.NewReader(""),
		Stdout:     stdout,
		Stderr:     stderr,
		Getenv:     os.Getenv,
		Now:        func() time.Time { return time.Unix(0, 0).UTC() },
		Executable: filepath.Join(w.root, "crw"),
	}
}

// improveProposeTestRun runs crw manage improve propose and returns the exit status and
// both streams.
func improveProposeTestRun(t *testing.T, w *improveProposeTestWorld, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := improveRunPropose(context.Background(), improveProposeTestEnv(w, &stdout, &stderr), args)
	return code, stdout.String(), stderr.String()
}

// improveProposeTestSplit is one split record of the improvement bundle: the project it
// happened in, the reason the receipt carried, and the relationship it happened at.
func improveProposeTestSplit(project, reason, relationship string) improveRecord {
	return improveRecord{
		Kind: improveKindSplit, Key: project, Where: relationship, What: reason, Count: 1,
		FirstAt: "2026-10-06T01:00:00Z", LastAt: "2026-10-06T01:00:00Z",
		Evidence: []string{"events:" + relationship},
	}
}

// improveProposeTestEightCases is the fixed 2026-10-06 input the issue's C6 names: the eight
// issue and project keys of the 721 fixture's case list, all of them the same repeated
// friction (the body size estimate the merged size ran past), so the eight records across
// three projects are one candidate. The parent's addition says the C6 fixed input rewrites the
// 721 helper, and the issue's own title-key wording makes a candidate one (kind, title) pair.
func improveProposeTestEightCases() []improveRecord {
	records := make([]improveRecord, 0, len(improveTestEightCases))
	for _, c := range improveTestEightCases {
		records = append(records, improveProposeTestSplit(c.project, improveProposeTestFriction, "rel-"+c.issue))
	}
	return records
}

// TestImproveProposeSuppressedCandidateStillGrowsAnExistingDraft covers the repeated-friction
// rule: an item already in the issue list creates no draft, but a draft that already exists
// for that fingerprint still grows its seen list.
func TestImproveProposeSuppressedCandidateStillGrowsAnExistingDraft(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	first := improveProposeTestBundle(t, w, improveProposeTestEightCases())
	if code, _, stderr := improveProposeTestRun(t, w, "--bundle", first); code != 0 {
		t.Fatalf("the first propose: exit %d, stderr %s", code, stderr)
	}
	// The second bundle carries the same friction, now on a new relationship, and the exported
	// issue list names it, so no new draft may be created.
	records := append(improveProposeTestEightCases(),
		improveProposeTestSplit("project-d", improveProposeTestFriction, "rel-new"),
		improveProposeTestIssue("CRW-739", improveProposeTestFriction))
	second := improveProposeTestBundle(t, w, records)
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", second)
	if code != 0 {
		t.Fatalf("the second propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Created) != 0 {
		t.Fatalf("created = %+v, want none", report.Created)
	}
	if len(report.Suppressed) != 1 {
		t.Fatalf("suppressed = %+v, want one", report.Suppressed)
	}
	if len(report.Updated) != 1 {
		t.Fatalf("updated = %+v, want the existing draft to grow", report.Updated)
	}
	doc := improveProposeTestDraft(t, w, report.Updated[0].Fingerprint)
	if len(doc.Seen) != 9 {
		t.Errorf("the draft carries %d seen entries, want nine: %+v", len(doc.Seen), doc.Seen)
	}
}

// TestImproveProposeKeepsDistinctFrictionsApart pins the literal 721 fixture: the same eight
// issues and projects, each carrying the reason its receipt held. Two different reasons are
// two different frictions, so they are two candidates, and each still merges across every
// project it reached.
func TestImproveProposeKeepsDistinctFrictionsApart(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	bundle := improveProposeTestBundle(t, w, improveProposeTestLiteralCases())
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Candidates) != 2 {
		t.Fatalf("candidates = %+v, want two distinct frictions", report.Candidates)
	}
	byTitle := map[string]improveProposeCandidate{}
	for _, candidate := range report.Candidates {
		byTitle[candidate.Title] = candidate
	}
	sized, ok := byTitle["size overrun"]
	if !ok {
		t.Fatalf("the size friction is missing: %+v", report.Candidates)
	}
	if sized.Count != 5 || len(sized.Projects) != 3 {
		t.Errorf("the size friction = count %d projects %+v, want 5 across 3", sized.Count, sized.Projects)
	}
	named, ok := byTitle["name collision"]
	if !ok {
		t.Fatalf("the name-collision friction is missing: %+v", report.Candidates)
	}
	if named.Count != 3 || len(named.Projects) != 2 {
		t.Errorf("the name-collision friction = count %d projects %+v, want 3 across 2", named.Count, named.Projects)
	}
}

// improveProposeTestLiteralCases is the 721 fixture's case list verbatim: the same eight
// issues and projects, each carrying the reason its receipt actually held.
func improveProposeTestLiteralCases() []improveRecord {
	records := make([]improveRecord, 0, len(improveTestEightCases))
	for _, c := range improveTestEightCases {
		records = append(records, improveProposeTestSplit(c.project, c.reason, "rel-"+c.issue))
	}
	return records
}

// improveProposeTestFriction is the one repeated friction the fixed eight cases share: the
// body size estimate the merged size ran past.
const improveProposeTestFriction = "size estimate understated"

// improveProposeTestBundle writes a crw-improve-bundle/1 document and returns its path.
func improveProposeTestBundle(t *testing.T, w *improveProposeTestWorld, records []improveRecord) string {
	t.Helper()
	path := filepath.Join(w.root, "bundle.json")
	doc := improveBundle{Schema: improveBundleSchema, Sources: []improveSourceRow{}, Records: records}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// improveProposeTestIssue is one issue record of the management session's exported list.
func improveProposeTestIssue(key, title string) improveRecord {
	return improveRecord{Kind: improveKindIssue, Key: key, What: title, Count: 1,
		Evidence: []string{"issue:" + key}}
}

// improveProposeTestDrafts lists the draft documents the drafts directory holds: the
// fingerprint-named files, without the lock file or the index.
func improveProposeTestDrafts(t *testing.T, w *improveProposeTestWorld) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(w.stateDir, "drafts"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || name == auditDraftIndexFile {
			continue
		}
		names = append(names, name)
	}
	return names
}

// improveProposeTestDraft loads one draft from the drafts directory.
func improveProposeTestDraft(t *testing.T, w *improveProposeTestWorld, key string) *auditDraft {
	t.Helper()
	doc, err := auditDraftLoad(filepath.Join(w.stateDir, "drafts", key+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// improveProposeTestReport decodes the report the command printed.
func improveProposeTestReport(t *testing.T, stdout string) improveProposeReport {
	t.Helper()
	var report improveProposeReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("the report is not %s JSON: %v\n%s", improveProposeReportSchema, err, stdout)
	}
	return report
}

// TestImproveProposeWritesNothingWhenTheContextEnds covers the cancellation rule: a context
// that ended before the drafts were written produces no draft at all.
func TestImproveProposeWritesNothingWhenTheContextEnds(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	bundle := improveProposeTestBundle(t, w, improveProposeTestEightCases())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr strings.Builder
	code := improveRunPropose(ctx, improveProposeTestEnv(w, &stdout, &stderr), []string{"--bundle", bundle})
	if code != 1 {
		t.Fatalf("a cancelled propose: exit %d, stderr %q", code, stderr.String())
	}
	if drafts := improveProposeTestDrafts(t, w); len(drafts) != 0 {
		t.Errorf("a cancelled run wrote %v", drafts)
	}
}

// improveProposeTestRecord is one bundle record of any kind.
func improveProposeTestRecord(kind, key, where, what string, count int, evidence ...string) improveRecord {
	return improveRecord{Kind: kind, Key: key, Where: where, What: what, Count: count,
		FirstAt: "2026-10-06T01:00:00Z", LastAt: "2026-10-06T01:00:00Z", Evidence: evidence}
}

// TestImproveProposeSkipsNonFrictionRecords covers the review finding that a criteria set's
// size, an audit grading outcome, a DAG metric and an existing draft are not repeated
// friction: they never become candidates.
func TestImproveProposeSkipsNonFrictionRecords(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	records := []improveRecord{
		improveProposeTestRecord(improveKindCriteria, "rel-a", "digest", "registered", 3, "journal:1"),
		improveProposeTestRecord(improveKindAudit, "CRW-1", "subj", "ok", 1, "ledger:1"),
		improveProposeTestRecord(improveKindDag, "p1:parallelism", "p1", "{}", 2, "relay:dag"),
		improveProposeTestRecord(improveKindDraft, "CRW-2", "proj", "a draft title", 1, "draft.json"),
	}
	bundle := improveProposeTestBundle(t, w, records)
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Candidates) != 0 {
		t.Errorf("candidates = %+v, want none from non-friction records", report.Candidates)
	}
	if drafts := improveProposeTestDrafts(t, w); len(drafts) != 0 {
		t.Errorf("the drafts directory holds %v, want nothing", drafts)
	}
}

// TestImproveProposeSeverityIsP1OnlyForBlockages covers the review finding that a medium-
// impact fault is not a P1: only a blockage or a needs_changes is P1.
func TestImproveProposeSeverityIsP1OnlyForBlockages(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	bundle := improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestRecord(improveKindFault, "observation_stalled", "project-a", "a signature", 5, "fault:f1"),
	})
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Created) != 1 {
		t.Fatalf("created = %+v, want one", report.Created)
	}
	if got := report.Created[0].Severity; got != "P2" {
		t.Errorf("a medium-impact fault was drafted %q, want P2", got)
	}
}

// TestImproveProposeOwnerSkipsTheUnknownProject covers the review finding that an unknown
// project must not take the owner away from a known one.
func TestImproveProposeOwnerSkipsTheUnknownProject(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	bundle := improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestSplit("", "size overrun", "rel-a"),
		improveProposeTestSplit("project-z", "size overrun", "rel-b"),
	})
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Created) != 1 {
		t.Fatalf("created = %+v, want one", report.Created)
	}
	doc := improveProposeTestDraft(t, w, report.Created[0].Fingerprint)
	if doc.Project != "project-z" {
		t.Errorf("the draft project = %q, want project-z", doc.Project)
	}
}

// TestImproveProposeUpdateKeepsTheNewProjectsAndEvidence covers the review finding that an
// updated draft must carry the projects and evidence a later run added, not only the ones it
// was first written with.
func TestImproveProposeUpdateKeepsTheNewProjectsAndEvidence(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	first := improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestSplit("project-a", "size overrun", "rel-a"),
	})
	if code, _, stderr := improveProposeTestRun(t, w, "--bundle", first); code != 0 {
		t.Fatalf("the first propose: exit %d, stderr %s", code, stderr)
	}
	second := filepath.Join(w.root, "bundle2.json")
	doc := improveBundle{Schema: improveBundleSchema, Sources: []improveSourceRow{}, Records: []improveRecord{
		improveProposeTestSplit("project-a", "size overrun", "rel-a"),
		improveProposeTestSplit("project-b", "size overrun", "rel-b"),
	}}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", second)
	if code != 0 {
		t.Fatalf("the second propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Updated) != 1 {
		t.Fatalf("updated = %+v, want one", report.Updated)
	}
	updated := improveProposeTestDraft(t, w, report.Updated[0].Fingerprint)
	if !strings.Contains(updated.Body, "project-b") {
		t.Errorf("the updated draft omits the new project:\n%s", updated.Body)
	}
	if !strings.Contains(updated.Body, "events:rel-b") {
		t.Errorf("the updated draft omits the new evidence:\n%s", updated.Body)
	}
	if !strings.Contains(updated.Body, "project-a") {
		t.Errorf("the updated draft lost the original project:\n%s", updated.Body)
	}
}

// TestImproveProposeWritesTheDraftFormat is the red-first test: before this issue there is
// no crw manage improve propose and no crw-issue-draft/1 draft with source improve.
func TestImproveProposeWritesTheDraftFormat(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	bundle := improveProposeTestBundle(t, w, []improveRecord{improveProposeTestSplit("project-a", "size overrun", "rel-a")})
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if report.Schema != improveProposeReportSchema {
		t.Errorf("schema = %q, want %q", report.Schema, improveProposeReportSchema)
	}
	if len(report.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", report.Created)
	}
	doc := improveProposeTestDraft(t, w, report.Created[0].Fingerprint)
	if doc.Schema != auditDraftSchema || doc.Source != improveProposeSource {
		t.Errorf("the draft is %+v, want schema %s source %s", doc, auditDraftSchema, improveProposeSource)
	}
	if doc.State != auditDraftStateDraft {
		t.Errorf("the draft state = %q, want %q", doc.State, auditDraftStateDraft)
	}
	if doc.Project != "project-a" {
		t.Errorf("the draft project = %q, want project-a", doc.Project)
	}
}

// TestImproveProposeUsesTheAuditDraftFingerprint covers C2: the candidate's key is the shared
// audit draft fingerprint.
func TestImproveProposeUsesTheAuditDraftFingerprint(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	bundle := improveProposeTestBundle(t, w, []improveRecord{improveProposeTestSplit("project-a", "size overrun", "rel-a")})
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	want := auditDraftFingerprint(improveKindSplit, "size overrun")
	if len(report.Created) != 1 || report.Created[0].Fingerprint != want {
		t.Fatalf("the fingerprint = %+v, want %s", report.Created, want)
	}
	if doc := improveProposeTestDraft(t, w, want); doc.Fingerprint != want {
		t.Errorf("the draft fingerprint = %q, want %s", doc.Fingerprint, want)
	}
}

// TestImproveProposeMergesTheRepeatedFrictionAcrossProjects covers C6: the eight fixed
// 2026-10-06 cases across three projects are one candidate.
func TestImproveProposeMergesTheRepeatedFrictionAcrossProjects(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	bundle := improveProposeTestBundle(t, w, improveProposeTestEightCases())
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Candidates) != 1 {
		t.Fatalf("candidates = %+v, want one", report.Candidates)
	}
	candidate := report.Candidates[0]
	if candidate.Count != 8 {
		t.Errorf("the candidate count = %d, want 8", candidate.Count)
	}
	if len(candidate.Projects) != 3 {
		t.Errorf("the candidate projects = %+v, want three", candidate.Projects)
	}
	if len(candidate.Evidence) != 8 {
		t.Errorf("the candidate evidence = %d locations, want eight", len(candidate.Evidence))
	}
}

// TestImproveProposeSuppressesCandidatesAlreadyInTheIssueList covers C4 and the second half
// of C6: a candidate whose title key is already in the exported issue list is suppressed and
// no draft appears.
func TestImproveProposeSuppressesCandidatesAlreadyInTheIssueList(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	records := append(improveProposeTestEightCases(), improveProposeTestIssue("CRW-739", improveProposeTestFriction))
	bundle := improveProposeTestBundle(t, w, records)
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Created) != 0 {
		t.Fatalf("created = %+v, want none", report.Created)
	}
	if len(report.Suppressed) != 1 {
		t.Errorf("suppressed = %+v, want one", report.Suppressed)
	}
	if drafts := improveProposeTestDrafts(t, w); len(drafts) != 0 {
		t.Errorf("the drafts directory holds %v, want nothing", drafts)
	}
}

// TestImproveProposeCapsNewDraftsAndReportsTheRemainder covers C3.
func TestImproveProposeCapsNewDraftsAndReportsTheRemainder(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{"max_new_drafts": 2})
	var records []improveRecord
	for i := 0; i < 5; i++ {
		records = append(records, improveProposeTestSplit("project-a", fmt.Sprintf("friction %d", i), fmt.Sprintf("rel-%d", i)))
	}
	bundle := improveProposeTestBundle(t, w, records)
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Created) != 2 {
		t.Errorf("created = %d, want two", len(report.Created))
	}
	if report.Remaining != 3 {
		t.Errorf("remaining = %d, want three", report.Remaining)
	}
}

// TestImproveProposeSeesTheSameDefectTwice covers C1: two records of one defect make one
// draft with two seen entries.
func TestImproveProposeSeesTheSameDefectTwice(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	records := []improveRecord{
		improveProposeTestSplit("project-a", "size overrun", "rel-a"),
		improveProposeTestSplit("project-b", "size overrun", "rel-b"),
	}
	bundle := improveProposeTestBundle(t, w, records)
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", report.Created)
	}
	doc := improveProposeTestDraft(t, w, report.Created[0].Fingerprint)
	if len(doc.Seen) != 2 {
		t.Fatalf("the draft carries %d seen entries, want two: %+v", len(doc.Seen), doc.Seen)
	}
}

// TestImproveProposeSecondRunCreatesNoDraft is the idempotence the run path relies on.
func TestImproveProposeSecondRunCreatesNoDraft(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	bundle := improveProposeTestBundle(t, w, improveProposeTestEightCases())
	if code, _, stderr := improveProposeTestRun(t, w, "--bundle", bundle); code != 0 {
		t.Fatalf("the first propose: exit %d, stderr %s", code, stderr)
	}
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("the second propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Created) != 0 || len(report.Updated) != 0 {
		t.Fatalf("the second run created %d and updated %d, want neither", len(report.Created), len(report.Updated))
	}
}

// TestImproveProposeDryRunWritesNothing covers --dry-run.
func TestImproveProposeDryRunWritesNothing(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	bundle := improveProposeTestBundle(t, w, improveProposeTestEightCases())
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle, "--dry-run")
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if !report.DryRun {
		t.Errorf("the report does not name the dry run")
	}
	if len(report.Candidates) != 1 {
		t.Errorf("the dry run reported %d candidates, want one", len(report.Candidates))
	}
	if drafts := improveProposeTestDrafts(t, w); len(drafts) != 0 {
		t.Errorf("the dry run wrote %v", drafts)
	}
}

// TestImproveProposeRefusesAnUnknownBundleSchema covers the input guard.
func TestImproveProposeRefusesAnUnknownBundleSchema(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	path := filepath.Join(w.root, "bundle.json")
	if err := os.WriteFile(path, []byte("{\"schema\":\"something/9\",\"records\":[]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := improveProposeTestRun(t, w, "--bundle", path)
	if code != 1 || !strings.Contains(stderr, "schema") {
		t.Fatalf("an unknown schema: exit %d, stderr %q", code, stderr)
	}
}

// TestImproveProposeRefusesAMissingBundleOption covers the argument guard.
func TestImproveProposeRefusesAMissingBundleOption(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	code, _, stderr := improveProposeTestRun(t, w)
	if code != usageExit || !strings.Contains(stderr, "--bundle") {
		t.Fatalf("a missing --bundle: exit %d, stderr %q", code, stderr)
	}
}

// TestImproveProposeLeavesTheTemporaryHomeAlone covers the isolation rule: the run writes only
// below the state directory the configuration names.
func TestImproveProposeLeavesTheTemporaryHomeAlone(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	before := improveTestTree(t, filepath.Join(w.root, "home"))
	bundle := improveProposeTestBundle(t, w, improveProposeTestEightCases())
	if code, _, stderr := improveProposeTestRun(t, w, "--bundle", bundle); code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	after := improveTestTree(t, filepath.Join(w.root, "home"))
	if before != after {
		t.Errorf("the temporary home changed:\n%s\n%s", before, after)
	}
}
