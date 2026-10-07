package manage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The review789 tests cover the six P1 defects of the merged PR #789 (CRW-722): crw manage
// improve must count "how many times, and where, the same friction was seen" exactly. They run
// collect and propose (and, for a repeated ref, run) end to end over the same hermetic temporary
// store, configuration and homes the 721 helpers build; nothing reaches a real store, App Home or
// relay state.

// improveReview789Configure writes the crw configuration file for these tests: the manage state
// directory collect and propose write below, and the improve section with the synthetic relay
// source. It mirrors the run tests' helper, so a test can set an extra key such as issue_list.
func improveReview789Configure(t *testing.T, s *improveTestState, manageState string, improve map[string]any) {
	t.Helper()
	improveReview789ConfigureSource(t, s, manageState, s.stateDir, improve)
}

// improveReview789ConfigureSource is improveReview789Configure with the relay source directory
// named explicitly, so one test can collect from more than one synthetic store.
func improveReview789ConfigureSource(t *testing.T, s *improveTestState, manageState, relayDir string, improve map[string]any) {
	t.Helper()
	section := map[string]any{}
	for key, value := range improve {
		section[key] = value
	}
	if _, ok := section["sources"]; !ok {
		section["sources"] = map[string]any{"relay": map[string]any{"path": relayDir}}
	}
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{
		"state_dir": manageState,
		"improve":   section,
	}})
}

// improveReview789BundlePath is the bundle path improveReview789Collect writes.
func improveReview789BundlePath(s *improveTestState) string {
	return filepath.Join(s.root, "bundle.json")
}

// improveReview789Collect runs crw manage improve collect into one file and reads the bundle.
func improveReview789Collect(t *testing.T, s *improveTestState) improveBundle {
	t.Helper()
	out := improveReview789BundlePath(s)
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	return improveTestReadBundle(t, out)
}

// improveReview789Propose runs crw manage improve propose over one bundle and reads the report.
func improveReview789Propose(t *testing.T, s *improveTestState, bundlePath string) improveProposeReport {
	t.Helper()
	var stdout, stderr strings.Builder
	code := improveRunPropose(context.Background(), improveTestEnv(s, &stdout, &stderr), []string{"--bundle", bundlePath})
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr.String())
	}
	return improveProposeTestReport(t, stdout.String())
}

// improveReview789SplitReceipt is the parent's split decision carrying the reason, as the event
// receipt JSON.
func improveReview789SplitReceipt(reason string) string {
	return fmt.Sprintf("{\"decision\":\"split_approval\",\"reason\":%q}", reason)
}

// improveReview789SeedSplit inserts one relationship with its project scope and one final
// decision_reply carrying the reason, so collect yields one split record for it.
func improveReview789SeedSplit(t *testing.T, db *sql.DB, relationship, issue, project, reason, event string) {
	t.Helper()
	improveTestInsert(t, db, "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?,?,'active','parent','host','child','host',1,'[]','[]','2026-10-06T00:00:00Z','2026-10-06T00:00:00Z')", relationship, issue)
	improveTestInsert(t, db, "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at) VALUES (?,?,'2026-10-06T00:00:00Z')", relationship, project)
	improveTestInsert(t, db, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES (?,?,1,?,'decision_reply','parent','t','turn-1','completed',?,'final','2026-10-06T01:00:00Z','2026-10-06T01:00:00Z')",
		event, relationship, strings.Repeat("0", 64), improveReview789SplitReceipt(reason))
}

// improveReview789IssueList is one exported issue list item as JSON: its identifier, its title and,
// when it carries one, its fingerprint.
func improveReview789IssueList(identifier, title, fingerprint string) string {
	if fingerprint == "" {
		return fmt.Sprintf("[{\"identifier\":%q,\"title\":%q,\"state\":\"Todo\"}]", identifier, title)
	}
	return fmt.Sprintf("[{\"identifier\":%q,\"title\":%q,\"fingerprint\":%q,\"state\":\"Todo\"}]", identifier, title, fingerprint)
}

// improveReview789Draft loads one draft below the manage state directory.
func improveReview789Draft(t *testing.T, manageState, fingerprint string) *auditDraft {
	t.Helper()
	doc, err := auditDraftLoad(filepath.Join(manageState, "drafts", fingerprint+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// improveReview789HasEvidence reports whether one record carries an evidence location.
func improveReview789HasEvidence(record improveRecord, want string) bool {
	for _, evidence := range record.Evidence {
		if evidence == want {
			return true
		}
	}
	return false
}

// improveReview789LatestRoadmap reads the newest roadmap document of one manage state directory.
func improveReview789LatestRoadmap(t *testing.T, manageState string) string {
	t.Helper()
	roadmaps := improveRoadmapTestRoadmaps(t, manageState)
	if len(roadmaps) == 0 {
		t.Fatal("the improve directory holds no roadmap document")
	}
	data, err := os.ReadFile(filepath.Join(manageState, "improve", roadmaps[len(roadmaps)-1]))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestImproveReview789EightCasesAreOneCandidate covers decided answers 1 and 2: the fixed eight
// 2026-10-06 cases, all reason "size overrun", become one candidate of three projects and count 8
// through collect then propose, and CRW-739 in the issue list leaves no new draft.
func TestImproveReview789EightCasesAreOneCandidate(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) { improveTestSeedEightCases(t, db) })
	improveReview789Configure(t, s, manageState, map[string]any{})

	bundle := improveReview789Collect(t, s)
	splits := improveTestRecordsOf(bundle, improveKindSplit)
	if len(splits) != 8 {
		t.Fatalf("split records = %d, want one per relationship (8): %+v", len(splits), splits)
	}
	total := 0
	for _, record := range splits {
		if record.What != "size overrun" {
			t.Errorf("the record at %s carries reason %q, want %q", record.Where, record.What, "size overrun")
		}
		if record.Count != 1 {
			t.Errorf("the record at %s merges %d rows, want one: a blockage and its answer are one record", record.Where, record.Count)
		}
		total += record.Count
	}
	if total != 8 {
		t.Errorf("the eight cases count %d, want 8", total)
	}

	// The issue's fixed input alternates the other way from a first-guess reading: its 2nd, 4th,
	// 6th and 8th entries are a blocked receipt plus the answer that resolved it, and the others are
	// the parent's own split decision. Only an answered blockage cites the reply, so the answer:
	// evidence marks exactly those four.
	answered := map[string]bool{}
	for _, record := range splits {
		for _, evidence := range record.Evidence {
			if strings.HasPrefix(evidence, improveEvidenceAnswerPrefix) {
				answered[record.Where] = true
			}
		}
	}
	for i, c := range improveTestEightCases {
		// A 1-based even entry is a 0-based odd index.
		wantAnswered := i%2 == 1
		if answered["rel-"+c.issue] != wantAnswered {
			t.Errorf("the case %s at 1-based position %d: answered=%v, want %v", c.issue, i+1, answered["rel-"+c.issue], wantAnswered)
		}
	}

	report := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(report.Candidates) != 1 {
		t.Fatalf("candidates = %+v, want one", report.Candidates)
	}
	candidate := report.Candidates[0]
	if candidate.Count != 8 || len(candidate.Projects) != 3 {
		t.Errorf("the candidate = count %d projects %+v, want 8 across three", candidate.Count, candidate.Projects)
	}
	if len(report.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", report.Created)
	}

	// CRW-739 already covers the friction. The run below collects again with the issue list in
	// place and proposes into an empty drafts directory, so the zero drafts come from the
	// suppression path rather than from the draft the earlier run already wrote.
	//
	// The exported item carries the fingerprint the draft was registered under, which is the
	// linkage the real export has; its title is deliberately not the candidate's, so the title
	// comparison cannot be what suppresses it.
	issues := filepath.Join(s.root, "issues.json")
	improveTestWrite(t, issues, improveReview789IssueList("CRW-739", "size estimate correction", auditDraftFingerprint(improveKindSplit, "size overrun")))
	empty := filepath.Join(s.root, "manage-state-suppressed")
	improveReview789Configure(t, s, empty, map[string]any{"issue_list": issues})
	improveReview789Collect(t, s)
	report = improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(report.Suppressed) != 1 {
		t.Errorf("suppressed = %+v, want one for the registered fingerprint", report.Suppressed)
	}
	if len(report.Created) != 0 {
		t.Errorf("created = %+v, want none with CRW-739 in the issue list", report.Created)
	}
	if drafts := improveRoadmapTestDrafts(t, empty); len(drafts) != 0 {
		t.Errorf("the suppression run wrote %v into empty drafts", drafts)
	}
}

// TestImproveReview789OccurrencesAreSeenEach covers decided answer 3: one record with two
// occurrences (two evidence locations) makes one sighting per occurrence, so a friction seen twice
// makes a draft with two seen entries.
func TestImproveReview789OccurrencesAreSeenEach(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a','manifest_forbidden','first')")
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T02:00:00Z','rel-b','ev-b','manifest_forbidden','second')")
	})
	improveReview789Configure(t, s, manageState, map[string]any{})

	improveReview789Collect(t, s)
	report := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(report.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", report.Created)
	}
	doc := improveReview789Draft(t, manageState, report.Created[0].Fingerprint)
	if len(doc.Seen) != 2 {
		t.Errorf("the draft carries %d seen entries, want one per occurrence (2): %+v", len(doc.Seen), doc.Seen)
	}
}

// TestImproveReview789IssueFingerprintSuppresses covers decided answer 4: the issue list item's
// fingerprint reaches the bundle, and a candidate whose fingerprint an issue carries is suppressed
// even when the issue's title differs.
func TestImproveReview789IssueFingerprintSuppresses(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveReview789SeedSplit(t, db, "rel-a", "CRW-1", "project-a", "size overrun", "ev-a")
	})
	fingerprint := auditDraftFingerprint(improveKindSplit, "size overrun")
	issues := filepath.Join(s.root, "issues.json")
	improveTestWrite(t, issues, improveReview789IssueList("CRW-900", "renamed follow-up", fingerprint))
	improveReview789Configure(t, s, manageState, map[string]any{"issue_list": issues})

	bundle := improveReview789Collect(t, s)
	carried := false
	for _, record := range improveTestRecordsOf(bundle, improveKindIssue) {
		if record.Fingerprint == fingerprint {
			carried = true
		}
	}
	if !carried {
		t.Errorf("the issue record lost its fingerprint %s: %+v", fingerprint, improveTestRecordsOf(bundle, improveKindIssue))
	}

	report := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(report.Created) != 0 {
		t.Errorf("created = %+v, want none for a registered fingerprint", report.Created)
	}
	if len(report.Suppressed) != 1 {
		t.Errorf("suppressed = %+v, want one", report.Suppressed)
	}
}

// TestImproveReview789SameRefRepeatCreatesNone covers decided answer 5: a run whose boundary and
// ref already have a roadmap document proposes with a cap of 0, so a repeated run creates no new
// draft and the cut candidate stays for a later run.
func TestImproveReview789SameRefRepeatCreatesNone(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveReview789SeedSplit(t, db, "rel-a", "CRW-1", "project-a", "size overrun", "ev-a")
		improveReview789SeedSplit(t, db, "rel-b", "CRW-2", "project-b", "name collision", "ev-b")
		improveReview789SeedSplit(t, db, "rel-c", "CRW-3", "project-c", "test gap", "ev-c")
	})
	improveRoadmapTestConfigure(t, s, manageState, map[string]any{"max_new_drafts": 2})
	var stdout, stderr strings.Builder
	e := improveRoadmapTestEnv(s, &stdout, &stderr)
	if code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", "M2"); code != 0 {
		t.Fatalf("the first run: exit %d, stderr %s", code, errOut)
	}
	before := improveRoadmapTestDrafts(t, manageState)
	if len(before) != 2 {
		t.Fatalf("the first run left %v, want two drafts at the cap", before)
	}

	if code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", "M2"); code != 0 {
		t.Fatalf("the repeated run: exit %d, stderr %s", code, errOut)
	}
	after := improveRoadmapTestDrafts(t, manageState)
	if len(after) != len(before) {
		t.Errorf("the repeated run changed the drafts from %v to %v, want no new draft", before, after)
	}
	body := improveReview789LatestRoadmap(t, manageState)
	if !strings.Contains(body, "- left for a later run: 1") {
		t.Errorf("the repeated roadmap does not leave one candidate for a later run:\n%s", body)
	}
}

// TestImproveReview789ProjectCountsAccumulate covers decided answer 6: a merged draft's project
// count is the stored count plus the sightings this run newly added, so it grows by what is new and
// stays put when the same bundle runs again.
func TestImproveReview789ProjectCountsAccumulate(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	first := make([]improveRecord, 0, 5)
	for i := 0; i < 5; i++ {
		first = append(first, improveProposeTestSplit("project-a", "size overrun", fmt.Sprintf("rel-%d", i)))
	}
	if code, _, stderr := improveProposeTestRun(t, w, "--bundle", improveProposeTestBundle(t, w, first)); code != 0 {
		t.Fatalf("the first propose: exit %d, stderr %s", code, stderr)
	}

	// One new relationship in the same project: the stored count grows by that sighting only.
	second := improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestSplit("project-a", "size overrun", "rel-new"),
	})
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", second)
	if code != 0 {
		t.Fatalf("the second propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Updated) != 1 {
		t.Fatalf("updated = %+v, want the existing draft to grow", report.Updated)
	}
	doc := improveProposeTestDraft(t, w, report.Updated[0].Fingerprint)
	if !strings.Contains(doc.Body, "- project-a (6)") {
		t.Errorf("the merged draft does not count five stored sightings plus one new one:\n%s", doc.Body)
	}

	if code, _, stderr := improveProposeTestRun(t, w, "--bundle", second); code != 0 {
		t.Fatalf("the repeated propose: exit %d, stderr %s", code, stderr)
	}
	again := improveProposeTestDraft(t, w, report.Updated[0].Fingerprint)
	if !strings.Contains(again.Body, "- project-a (6)") {
		t.Errorf("rerunning the same bundle changed the project count:\n%s", again.Body)
	}
}

// TestImproveReview789SplitWithoutScopeHasNoProject covers decided answer 7: a split record with no
// relationship_scope leaves the project empty and keeps the issue key in its evidence, so a draft
// never takes an issue key for its project.
func TestImproveReview789SplitWithoutScopeHasNoProject(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES ('rel-a','CRW-900','active','parent','host','child','host',1,'[]','[]','2026-10-06T00:00:00Z','2026-10-06T00:00:00Z')")
		improveTestInsert(t, db, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES ('ev-a','rel-a',1,?,'decision_reply','parent','t','turn-1','completed',?,'final','2026-10-06T01:00:00Z','2026-10-06T01:00:00Z')",
			strings.Repeat("0", 64), improveReview789SplitReceipt("size overrun"))
	})
	improveReview789Configure(t, s, manageState, map[string]any{})

	bundle := improveReview789Collect(t, s)
	splits := improveTestRecordsOf(bundle, improveKindSplit)
	if len(splits) != 1 {
		t.Fatalf("split records = %+v, want one", splits)
	}
	if splits[0].Key != "" {
		t.Errorf("the split key = %q, want empty: an issue key is not a project", splits[0].Key)
	}
	if !improveReview789HasEvidence(splits[0], "issue:CRW-900") {
		t.Errorf("the split record lost the issue key in its evidence: %+v", splits[0].Evidence)
	}

	report := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(report.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", report.Created)
	}
	doc := improveReview789Draft(t, manageState, report.Created[0].Fingerprint)
	if doc.Project == "CRW-900" {
		t.Errorf("the draft project = %q, want it not to be the issue key", doc.Project)
	}
}

// TestImproveReview789BlankEvidenceMakesNoSighting covers decided answer 3's edge: an origin
// location that is blank is not an origin, so a record whose evidence list is blank entries plus
// one real location makes the one sighting it has rather than a sighting with an empty head. The
// bundle carries the blank entries directly, because no collector writes one.
func TestImproveReview789BlankEvidenceMakesNoSighting(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	bundle := improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestRecord(improveKindSplit, "project-a", "rel-a", "size overrun", 1, "", "   ", "events:rel-a"),
	})
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", report.Created)
	}
	doc := improveProposeTestDraft(t, w, report.Created[0].Fingerprint)
	for _, sighting := range doc.Seen {
		if strings.TrimSpace(sighting.Head) == "" {
			t.Errorf("the draft carries a sighting with no origin location: %+v", doc.Seen)
		}
	}
	if len(doc.Seen) != 1 {
		t.Errorf("the draft carries %d seen entries, want one: %+v", len(doc.Seen), doc.Seen)
	}
}

// TestImproveReview789NewOccurrenceCountsOnce covers the review finding that a new occurrence must
// count once: one refusal record already recorded twice, then seen a third time, must add exactly
// one sighting and one to the project's count rather than re-adding every earlier occurrence.
func TestImproveReview789NewOccurrenceCountsOnce(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveReview789Configure(t, s, manageState, map[string]any{})

	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a','manifest_forbidden','first')")
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T02:00:00Z','rel-b','ev-b','manifest_forbidden','second')")
	})
	improveReview789Collect(t, s)
	first := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(first.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", first.Created)
	}
	fingerprint := first.Created[0].Fingerprint
	if doc := improveReview789Draft(t, manageState, fingerprint); len(doc.Seen) != 2 {
		t.Fatalf("the first draft carries %d seen entries, want two: %+v", len(doc.Seen), doc.Seen)
	}

	// A third refusal of the same reason arrives, so the record is seen once more.
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T03:00:00Z','rel-c','ev-c','manifest_forbidden','third')")
	})
	improveReview789Collect(t, s)
	second := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(second.Updated) != 1 {
		t.Fatalf("updated = %+v, want the existing draft to grow once", second.Updated)
	}
	doc := improveReview789Draft(t, manageState, fingerprint)
	if len(doc.Seen) != 3 {
		t.Errorf("the draft carries %d seen entries, want three (one per occurrence): %+v", len(doc.Seen), doc.Seen)
	}
	if !strings.Contains(doc.Body, "- owner_unknown (3)") {
		t.Errorf("the merged draft does not count three occurrences:\n%s", doc.Body)
	}
}

// TestImproveReview789UnscopedBlockageCountsOnce covers the review finding that the issue key a
// scopeless split carries as evidence is context, not a second occurrence: one blockage is one
// sighting and one occurrence.
func TestImproveReview789UnscopedBlockageCountsOnce(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES ('rel-a','CRW-900','active','parent','host','child','host',1,'[]','[]','2026-10-06T00:00:00Z','2026-10-06T00:00:00Z')")
		improveTestInsert(t, db, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES ('ev-a','rel-a',1,?,'decision_reply','parent','t','turn-1','completed',?,'final','2026-10-06T01:00:00Z','2026-10-06T01:00:00Z')",
			strings.Repeat("0", 64), improveReview789SplitReceipt("size overrun"))
	})
	improveReview789Configure(t, s, manageState, map[string]any{})

	improveReview789Collect(t, s)
	report := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(report.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", report.Created)
	}
	doc := improveReview789Draft(t, manageState, report.Created[0].Fingerprint)
	if len(doc.Seen) != 1 {
		t.Errorf("the draft carries %d seen entries, want one: %+v", len(doc.Seen), doc.Seen)
	}
}

// TestImproveReview789ConcurrentRunIsRefused covers the review finding that two runs of one
// boundary and ref must not both pass the roadmap check: while one run holds the pass, another is
// refused by name rather than proposing with a stale cap.
func TestImproveReview789ConcurrentRunIsRefused(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveReview789SeedSplit(t, db, "rel-a", "CRW-1", "project-a", "size overrun", "ev-a")
	})
	improveRoadmapTestConfigure(t, s, manageState, map[string]any{})
	dir := filepath.Join(manageState, "improve")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := improveRoadmapLock(dir, "milestone", "M2")
	if err != nil {
		t.Fatalf("taking the pass: %v", err)
	}
	defer release()

	var stdout, stderr strings.Builder
	e := improveRoadmapTestEnv(s, &stdout, &stderr)
	code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", "M2")
	if code != 1 || !strings.Contains(errOut, improveReasonRoadmapLocked) {
		t.Fatalf("a concurrent run: exit %d, stderr %q, want the named refusal %s", code, errOut, improveReasonRoadmapLocked)
	}
	if drafts := improveRoadmapTestDrafts(t, manageState); len(drafts) != 0 {
		t.Errorf("the refused run wrote %v", drafts)
	}
}

// improveReview789StoreAt builds one synthetic relay store at an explicit path, so a test can
// collect from more than one store.
func improveReview789StoreAt(t *testing.T, dbPath string, f func(t *testing.T, db *sql.DB)) {
	t.Helper()
	opened, err := store.Open(context.Background(), dbPath, "")
	if err != nil {
		t.Fatalf("the synthetic store at %s: %v", dbPath, err)
	}
	f(t, opened.DB)
	if err := opened.Close(); err != nil {
		t.Fatalf("closing the synthetic store at %s: %v", dbPath, err)
	}
}

// TestImproveReview789DistinctStoresCountEachOccurrence covers the review finding that an
// occurrence's origin has to name the store it was read from: two stores can carry the same
// reason at the same row number, so without the store in the origin the second store's row reads
// as an occurrence already seen and its sighting and project count are lost.
func TestImproveReview789DistinctStoresCountEachOccurrence(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	secondDir := filepath.Join(s.root, "state-b")
	if err := os.MkdirAll(secondDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secondDB := filepath.Join(secondDir, "relay.sqlite3")
	improveReview789StoreAt(t, s.dbPath, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a','manifest_forbidden','first')")
	})
	improveReview789StoreAt(t, secondDB, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T02:00:00Z','rel-b','ev-b','manifest_forbidden','second')")
	})

	// The first store's bundle, proposed into the drafts directory.
	improveReview789ConfigureSource(t, s, manageState, s.stateDir, map[string]any{})
	improveReview789Collect(t, s)
	first := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(first.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", first.Created)
	}
	fingerprint := first.Created[0].Fingerprint
	if doc := improveReview789Draft(t, manageState, fingerprint); len(doc.Seen) != 1 {
		t.Fatalf("the first draft carries %d seen entries, want one: %+v", len(doc.Seen), doc.Seen)
	}

	// The second store's bundle. Its refusal is at the same row number as the first store's, so
	// only the store token tells the two occurrences apart.
	improveReview789ConfigureSource(t, s, manageState, secondDir, map[string]any{})
	improveReview789Collect(t, s)
	second := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(second.Updated) != 1 {
		t.Fatalf("updated = %+v, want the existing draft to grow", second.Updated)
	}
	doc := improveReview789Draft(t, manageState, fingerprint)
	if len(doc.Seen) != 2 {
		t.Errorf("the draft carries %d seen entries, want one per store occurrence (2): %+v", len(doc.Seen), doc.Seen)
	}
	if !strings.Contains(doc.Body, "- owner_unknown (2)") {
		t.Errorf("the merged draft does not count the second store's occurrence:\n%s", doc.Body)
	}
}

// TestImproveReview789ContextEvidenceIsPerKind covers the review finding that the context exclusion
// must look at the record kind, not only at the evidence prefix: only a split record attaches the
// issue: and answer: context, so for every other kind each non-empty evidence entry is an origin
// location. An intervention whose real locations begin with answer: keeps both of them.
func TestImproveReview789ContextEvidenceIsPerKind(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	bundle := improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestRecord(improveKindIntervention, "stalled", "", "a signal", 1,
			"answer:first", "answer:second"),
	})
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
		t.Errorf("the intervention draft carries %d seen entries, want one per location (2): %+v", len(doc.Seen), doc.Seen)
	}
}

// TestImproveReview789RefWithAControlCharacterIsRefused covers the review finding that a --ref
// holding a control character must be refused as a usage error: the roadmap writes the ref
// verbatim and the header reader compares line by line, so an embedded newline could make one ref's
// document stand in for another's.
func TestImproveReview789RefWithAControlCharacterIsRefused(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveReview789SeedSplit(t, db, "rel-a", "CRW-1", "project-a", "size overrun", "ev-a")
	})
	improveRoadmapTestConfigure(t, s, manageState, map[string]any{})
	var stdout, stderr strings.Builder
	e := improveRoadmapTestEnv(s, &stdout, &stderr)
	for _, ref := range []string{"a\nb", "a\rb", "a\tb", "a\x00b", "a\x7fb"} {
		code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", ref)
		if code != usageExit {
			t.Errorf("the ref %q: exit %d, want %d (stderr %q)", ref, code, usageExit, errOut)
		}
	}
	// A plain ref is unchanged.
	if code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", "M2"); code != 0 {
		t.Fatalf("a plain ref: exit %d, stderr %q", code, errOut)
	}
	if roadmaps := improveRoadmapTestRoadmaps(t, manageState); len(roadmaps) != 1 {
		t.Errorf("the improve directory holds %v, want one roadmap for the plain ref", roadmaps)
	}
}

// TestImproveReview789StoredIssueKeyProjectIsDropped covers the review finding that a draft an
// earlier build wrote for a scopeless split kept the issue key as its project: the next run drops
// that false project and moves its occurrences to the unknown owner, because an issue key is never
// a project.
func TestImproveReview789StoredIssueKeyProjectIsDropped(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})

	// A draft as the earlier build wrote it: the issue key in the project list and body.
	fingerprint := auditDraftFingerprint(improveKindSplit, "size overrun")
	stale := &auditDraft{
		Schema: auditDraftSchema, Fingerprint: fingerprint, Source: improveProposeSource,
		Project: "CRW-900", Title: "size overrun", Severity: "P1", State: auditDraftStateDraft,
		Body: "## What\n\nsize overrun\n\n## Where\n\n- CRW-900 (1)\n\n## Seen\n\n" +
			"- source=improve subject= where=rel-a at=2026-10-06T01:00:00Z\n\n## Evidence\n\n- issue:CRW-900\n",
		Seen: []auditDraftSeen{{Mode: improveProposeSource, Head: "events:rel-a", At: "2026-10-06T01:00:00Z"}},
	}
	if err := auditDraftSave(filepath.Join(w.stateDir, "drafts", fingerprint+".json"), stale); err != nil {
		t.Fatal(err)
	}

	bundle := improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestRecord(improveKindSplit, "", "rel-a", "size overrun", 1, "events:rel-a", "issue:CRW-900"),
	})
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Updated) != 1 {
		t.Fatalf("updated = %+v, want the stored draft to be corrected", report.Updated)
	}
	doc := improveProposeTestDraft(t, w, fingerprint)
	if strings.Contains(doc.Body, "- CRW-900 (") {
		t.Errorf("the corrected draft still names the issue key as a project:\n%s", doc.Body)
	}
	if doc.Project == "CRW-900" {
		t.Errorf("the corrected draft project = %q, want it not to be the issue key", doc.Project)
	}
	if !strings.Contains(doc.Body, "- owner_unknown (1)") {
		t.Errorf("the corrected draft lost the occurrence with the false project name:\n%s", doc.Body)
	}
}

// TestImproveReview789LongReasonSurvivesARerun covers the review finding that a reason longer than
// the display title limit was cut in the body on a rerun: the body carries the whole reason, so a
// rerun with no new occurrence leaves it whole.
func TestImproveReview789LongReasonSurvivesARerun(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	reason := strings.Repeat("overrun-", 12) + "end"
	if len([]rune(reason)) <= auditDraftTitleLimit {
		t.Fatalf("the test reason is %d runes, want it over the %d-rune title limit", len([]rune(reason)), auditDraftTitleLimit)
	}
	bundle := improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestRecord(improveKindRefusal, reason, reason, reason, 1, "refusals:1"),
	})
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("the first propose: exit %d, stderr %s", code, stderr)
	}
	first := improveProposeTestReport(t, stdout)
	if len(first.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", first.Created)
	}
	doc := improveProposeTestDraft(t, w, first.Created[0].Fingerprint)
	if got := improveReview789WhatSection(t, doc.Body); got != reason {
		t.Fatalf("the first draft's What section is %q, want the whole reason", got)
	}

	// The same bundle again: no new occurrence, and the whole reason stays in the body.
	code, _, stderr = improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("the repeated propose: exit %d, stderr %s", code, stderr)
	}
	again := improveProposeTestDraft(t, w, first.Created[0].Fingerprint)
	if got := improveReview789WhatSection(t, again.Body); got != reason {
		t.Errorf("the rerun shortened the reason in the body: What is %q, want the whole reason", got)
	}
}

// improveReview789WhatSection is the What section of a stored improve body, the friction's own
// text as the body holds it.
func improveReview789WhatSection(t *testing.T, body string) string {
	t.Helper()
	const marker = "## What\n\n"
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("the body has no What section:\n%s", body)
	}
	rest := body[start+len(marker):]
	if end := strings.Index(rest, "\n\n## "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// TestImproveReview789StoreWithoutIdentityIsNamedByItsFile covers the review finding that a store
// whose own identity row is missing must still be named by the file it was read from: the Store a
// snapshot hands its reader carries no path, so deriving one from the reader's directory would
// name two different stores by the same directory and merge their occurrences.
func TestImproveReview789StoreWithoutIdentityIsNamedByItsFile(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	secondDir := filepath.Join(s.root, "state-b")
	if err := os.MkdirAll(secondDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secondDB := filepath.Join(secondDir, "relay.sqlite3")
	for _, dbPath := range []string{s.dbPath, secondDB} {
		improveReview789StoreAt(t, dbPath, func(t *testing.T, db *sql.DB) {
			improveTestInsert(t, db, "DELETE FROM schema_meta WHERE key = 'store_id'")
			improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a','manifest_forbidden','first')")
		})
	}

	improveReview789ConfigureSource(t, s, manageState, s.stateDir, map[string]any{})
	improveReview789Collect(t, s)
	first := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(first.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", first.Created)
	}
	fingerprint := first.Created[0].Fingerprint

	// The second store, with the same reason at the same row number and no identity row either.
	// Only the file the store was opened from tells the two occurrences apart.
	improveReview789ConfigureSource(t, s, manageState, secondDir, map[string]any{})
	improveReview789Collect(t, s)
	second := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(second.Updated) != 1 {
		t.Fatalf("updated = %+v, want the existing draft to grow", second.Updated)
	}
	doc := improveReview789Draft(t, manageState, fingerprint)
	if len(doc.Seen) != 2 {
		t.Errorf("the draft carries %d seen entries, want one per store (2): %+v", len(doc.Seen), doc.Seen)
	}
}

// TestImproveReview789AliasedSourceIsOneLocation covers the review finding that a second source
// spelling reaching the same file through a link must not count as a new occurrence: the origin
// names the resolved file, so recollecting the same file through a link adds no sighting.
func TestImproveReview789AliasedSourceIsOneLocation(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	real := filepath.Join(s.root, "interventions.jsonl")
	improveTestWrite(t, real, "{\"signal\":\"stalled\",\"at\":\"2026-10-06T01:00:00Z\"}\n")
	link := filepath.Join(s.root, "current.jsonl")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})

	improveReview789Configure(t, s, manageState, map[string]any{
		"sources": map[string]any{"intervention": map[string]any{"path": real}},
	})
	improveReview789Collect(t, s)
	first := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(first.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", first.Created)
	}
	fingerprint := first.Created[0].Fingerprint

	// The same file, reached through the link: no new occurrence.
	improveReview789Configure(t, s, manageState, map[string]any{
		"sources": map[string]any{"intervention": map[string]any{"path": link}},
	})
	improveReview789Collect(t, s)
	second := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(second.Created) != 0 || len(second.Updated) != 0 {
		t.Errorf("the aliased recollect created %d and updated %d, want neither", len(second.Created), len(second.Updated))
	}
	doc := improveReview789Draft(t, manageState, fingerprint)
	if len(doc.Seen) != 1 {
		t.Errorf("the draft carries %d seen entries, want one for the one file: %+v", len(doc.Seen), doc.Seen)
	}
}

// TestImproveReview789MultilineReasonSurvivesARerun covers the review finding that a valid reason
// holding a blank line and a Markdown heading was shortened in the body on an identical rerun: the
// rewrite read the rendered body's What section, whose end it could not tell from a heading inside
// the reason, so the second run saved only the first line while the fingerprint and title still
// named the whole reason. The rewrite now carries this run's own reason, which is the same text the
// fingerprint was taken over.
func TestImproveReview789MultilineReasonSurvivesARerun(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	reason := "size overrun\n\n## Details\n\nEstimate ignored generated files"
	bundle := improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestRecord(improveKindSplit, "project-a", "rel-a", reason, 1, "events:rel-a"),
	})
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("the first propose: exit %d, stderr %s", code, stderr)
	}
	first := improveProposeTestReport(t, stdout)
	if len(first.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", first.Created)
	}
	doc := improveProposeTestDraft(t, w, first.Created[0].Fingerprint)
	if !strings.Contains(doc.Body, reason) {
		t.Fatalf("the first draft's body does not carry the whole reason:\n%s", doc.Body)
	}

	// The identical bundle again: no new occurrence, and the whole reason stays in the body.
	code, _, stderr = improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("the repeated propose: exit %d, stderr %s", code, stderr)
	}
	again := improveProposeTestDraft(t, w, first.Created[0].Fingerprint)
	if !strings.Contains(again.Body, reason) {
		t.Errorf("the rerun shortened the reason in the body:\n%s", again.Body)
	}
}

// TestImproveReview789LearnedProjectIsOneSighting covers the review finding that learning the
// project of an existing split counted the same occurrence twice: the split's key is its current
// project and became the sighting's subject, so once the relationship was attached to a project the
// one event origin was counted again, under the unknown owner and under the project. One origin is
// one sighting, and the owner it is counted under follows the latest reading.
func TestImproveReview789LearnedProjectIsOneSighting(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES ('rel-a','CRW-900','active','parent','host','child','host',1,'[]','[]','2026-10-06T00:00:00Z','2026-10-06T00:00:00Z')")
		improveTestInsert(t, db, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES ('ev-a','rel-a',1,?,'decision_reply','parent','t','turn-1','completed',?,'final','2026-10-06T01:00:00Z','2026-10-06T01:00:00Z')",
			strings.Repeat("0", 64), improveReview789SplitReceipt("size overrun"))
	})
	improveReview789Configure(t, s, manageState, map[string]any{})

	improveReview789Collect(t, s)
	first := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(first.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", first.Created)
	}
	fingerprint := first.Created[0].Fingerprint
	if doc := improveReview789Draft(t, manageState, fingerprint); !strings.Contains(doc.Body, "- owner_unknown (1)") {
		t.Fatalf("the scopeless draft does not count the one occurrence under the unknown owner:\n%s", doc.Body)
	}

	// The relationship is attached to a project. No new event: the same origin, seen again.
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at) VALUES ('rel-a','project-a','2026-10-06T02:00:00Z')")
	})
	improveReview789Collect(t, s)
	second := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(second.Updated) != 1 {
		t.Fatalf("updated = %+v, want the existing draft to grow once", second.Updated)
	}
	doc := improveReview789Draft(t, manageState, fingerprint)
	if len(doc.Seen) != 1 {
		t.Errorf("the draft carries %d seen entries, want one: one origin is one sighting: %+v", len(doc.Seen), doc.Seen)
	}
	if !strings.Contains(doc.Body, "- project-a (1)") {
		t.Errorf("the draft does not count the occurrence under the project it learned:\n%s", doc.Body)
	}
	if strings.Contains(doc.Body, "- owner_unknown (") {
		t.Errorf("the draft still counts the occurrence under the unknown owner:\n%s", doc.Body)
	}
}

// TestImproveReview789ReasonHeadingIsNotAProjectList covers the review finding that a reason
// quoting a Markdown heading was read back as the draft's own Where section: the parser took the
// first occurrence of the heading, which sits inside the reason, so an identical rerun saved a
// project that never existed and even took it for the draft's owner. Only the section this feature
// wrote is read, so a reason may contain any heading.
func TestImproveReview789ReasonHeadingIsNotAProjectList(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	reason := "size overrun\n\n## Where\n\n- aaa-fake (10)"
	bundle := improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestRecord(improveKindSplit, "project-a", "rel-a", reason, 1, "events:rel-a"),
	})
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("the first propose: exit %d, stderr %s", code, stderr)
	}
	first := improveProposeTestReport(t, stdout)
	if len(first.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", first.Created)
	}
	fingerprint := first.Created[0].Fingerprint

	// The identical bundle again: the reason still holds the heading, and no fake project appears.
	code, _, stderr = improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("the repeated propose: exit %d, stderr %s", code, stderr)
	}
	doc := improveProposeTestDraft(t, w, fingerprint)
	// The reason itself holds the text, so the body legitimately contains it; what must not happen
	// is the reader taking it for a project.
	if projects := improveProposeParseProjects(doc.Body); len(projects) != 1 || projects[0].Project != "project-a" || projects[0].Count != 1 {
		t.Errorf("the rerun read the reason's heading as the Where section: projects = %+v, want only project-a (1)\n%s", projects, doc.Body)
	}
	if doc.Project != "project-a" {
		t.Errorf("the draft project = %q, want project-a", doc.Project)
	}
}

// TestImproveReview789IssueKeyOwnerIsNotMovedTwice covers the review finding that a draft an
// earlier build wrote with the issue key as its project counted its one occurrence twice on the
// next run: the merge already carries that false project's occurrences to the unknown owner, and
// the owner move counted the same occurrence again. One occurrence stays one.
func TestImproveReview789IssueKeyOwnerIsNotMovedTwice(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})

	// A draft as the earlier build wrote it: the issue key as the project and in the sighting.
	fingerprint := auditDraftFingerprint(improveKindSplit, "size overrun")
	stale := &auditDraft{
		Schema: auditDraftSchema, Fingerprint: fingerprint, Source: improveProposeSource,
		Project: "CRW-900", Title: "size overrun", Severity: "P1", State: auditDraftStateDraft,
		Body: "## What\n\nsize overrun\n\n## Where\n\n- CRW-900 (1)\n\n## Seen\n\n" +
			"- source=improve subject=CRW-900 where=rel-a at=2026-10-06T01:00:00Z\n\n## Evidence\n\n- issue:CRW-900\n",
		Seen: []auditDraftSeen{{Mode: improveProposeSource, Subject: "CRW-900", Head: "rel-a", At: "2026-10-06T01:00:00Z"}},
	}
	if err := auditDraftSave(filepath.Join(w.stateDir, "drafts", fingerprint+".json"), stale); err != nil {
		t.Fatal(err)
	}

	// The corrected reading of the same split: no scope, so no project, and the same occurrence.
	bundle := improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestRecord(improveKindSplit, "", "rel-a", "size overrun", 1, "issue:CRW-900"),
	})
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
	if code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Updated) != 1 {
		t.Fatalf("updated = %+v, want the stored draft to be corrected", report.Updated)
	}
	doc := improveProposeTestDraft(t, w, fingerprint)
	if len(doc.Seen) != 1 {
		t.Errorf("the draft carries %d seen entries, want one: %+v", len(doc.Seen), doc.Seen)
	}
	if !strings.Contains(doc.Body, "- owner_unknown (1)") {
		t.Errorf("the corrected draft does not count the one occurrence once:\n%s", doc.Body)
	}
	if strings.Contains(doc.Body, "- CRW-900 (") {
		t.Errorf("the corrected draft still names the issue key as a project:\n%s", doc.Body)
	}
}

// TestImproveReview789RotatedFileCountsItsNewRow covers the review finding that a rotated JSON-line
// source lost its new occurrence: the origin was the resolved file and the line number alone, so a
// different row at the same line number read as the one already seen and the second occurrence was
// never counted. The row's own time is part of the origin, so two rows that share a position are
// still two occurrences.
func TestImproveReview789RotatedFileCountsItsNewRow(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	interventions := filepath.Join(s.root, "interventions.jsonl")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	improveReview789Configure(t, s, manageState, map[string]any{
		"sources": map[string]any{"intervention": map[string]any{"path": interventions}},
	})

	improveTestWrite(t, interventions, "{\"signal\":\"stalled\",\"at\":\"2026-10-06T01:00:00Z\"}\n")
	improveReview789Collect(t, s)
	first := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(first.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", first.Created)
	}
	fingerprint := first.Created[0].Fingerprint

	// The file is rotated: the same path and line number now hold a different occurrence.
	improveTestWrite(t, interventions, "{\"signal\":\"stalled\",\"at\":\"2026-10-07T01:00:00Z\"}\n")
	improveReview789Collect(t, s)
	second := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(second.Updated) != 1 {
		t.Fatalf("updated = %+v, want the existing draft to grow", second.Updated)
	}
	doc := improveReview789Draft(t, manageState, fingerprint)
	if len(doc.Seen) != 2 {
		t.Errorf("the draft carries %d seen entries, want one per occurrence (2): %+v", len(doc.Seen), doc.Seen)
	}
	if !strings.Contains(doc.Body, "- owner_unknown (2)") {
		t.Errorf("the rotated row was not counted as a second occurrence:\n%s", doc.Body)
	}
}

// TestImproveReview789LongRefStillRuns covers the review finding that the pass lock's file name was
// the boundary and ref spelled out, so a long but legal ref pushed the name past the filesystem's
// limit and the run failed before collecting anything. The name is a fixed-length digest, so the
// ref's length no longer decides whether the pass can run.
func TestImproveReview789LongRefStillRuns(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveReview789SeedSplit(t, db, "rel-a", "CRW-1", "project-a", "size overrun", "ev-a")
	})
	improveRoadmapTestConfigure(t, s, manageState, map[string]any{})
	ref := strings.Repeat("a", 240)
	var stdout, stderr strings.Builder
	e := improveRoadmapTestEnv(s, &stdout, &stderr)
	if code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", ref); code != 0 {
		t.Fatalf("a long ref: exit %d, stderr %q", code, errOut)
	}
	if roadmaps := improveRoadmapTestRoadmaps(t, manageState); len(roadmaps) != 1 {
		t.Errorf("the improve directory holds %v, want one roadmap for the long ref", roadmaps)
	}
}

// TestImproveReview789SymlinkedParentDotDotIsOneLocation covers the review finding that a source
// path spelled through a symlinked directory followed by ".." was cleaned before its links were
// resolved, so the origin named a file the read never touched and the same occurrence was counted
// again when the real path was used. The kernel resolves a link and a following ".." in the order
// they are written, so the reader must resolve the caller's own spelling.
func TestImproveReview789SymlinkedParentDotDotIsOneLocation(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	project := filepath.Join(s.root, "data", "project")
	if err := os.MkdirAll(filepath.Join(project, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(project, "interventions.jsonl")
	improveTestWrite(t, real, "{\"signal\":\"stalled\",\"at\":\"2026-10-06T01:00:00Z\"}\n")
	fixtures := filepath.Join(s.root, "fixtures")
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(project, "nested"), filepath.Join(fixtures, "link")); err != nil {
		t.Fatal(err)
	}
	// The caller's own spelling: the link is followed first, so ".." leaves the link's target.
	spelled := fixtures + "/link/../interventions.jsonl"
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})

	improveReview789Configure(t, s, manageState, map[string]any{
		"sources": map[string]any{"intervention": map[string]any{"path": spelled}},
	})
	improveReview789Collect(t, s)
	first := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(first.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", first.Created)
	}
	fingerprint := first.Created[0].Fingerprint

	// The same file, named by its real path: the same occurrence, so nothing new.
	improveReview789Configure(t, s, manageState, map[string]any{
		"sources": map[string]any{"intervention": map[string]any{"path": real}},
	})
	improveReview789Collect(t, s)
	second := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(second.Created) != 0 || len(second.Updated) != 0 {
		t.Errorf("the real-path recollect created %d and updated %d, want neither", len(second.Created), len(second.Updated))
	}
	doc := improveReview789Draft(t, manageState, fingerprint)
	if len(doc.Seen) != 1 {
		t.Errorf("the draft carries %d seen entries, want one for the one file: %+v", len(doc.Seen), doc.Seen)
	}
}

// TestImproveReview789AggregateGrowthIsAdded covers the aggregate the fault ledger reports: one
// row carries the count of every occurrence it merged, at one origin. A later reading that reports
// a larger count at the same origin grows the stored count by that growth, and a rerun of the same
// bundle leaves it alone.
func TestImproveReview789AggregateGrowthIsAdded(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})

	// A source that aggregates its occurrences into one row reports its own total at that one
	// origin: the fault ledger's occurrence_count, at the fault's own id.
	first := improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestRecord(improveKindFault, "observation_stalled", "project-a", "a signature", 5, "fault:store-a:f1"),
	})
	if code, _, stderr := improveProposeTestRun(t, w, "--bundle", first); code != 0 {
		t.Fatalf("the first propose: exit %d, stderr %s", code, stderr)
	}

	// The same origin now reports six occurrences. The origin is the one the draft already
	// carries, so no sighting is added; the count the source itself reports is what grows.
	second := improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestRecord(improveKindFault, "observation_stalled", "project-a", "a signature", 6, "fault:store-a:f1"),
	})
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", second)
	if code != 0 {
		t.Fatalf("the second propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Updated) != 1 {
		t.Fatalf("updated = %+v, want the existing draft to follow the origin's own count", report.Updated)
	}
	doc := improveProposeTestDraft(t, w, report.Updated[0].Fingerprint)
	if !strings.Contains(doc.Body, "- owner_unknown (6)") {
		t.Errorf("the draft does not carry the count its own origin reports:\n%s", doc.Body)
	}

	// The same bundle again leaves the count as it is.
	if code, _, stderr := improveProposeTestRun(t, w, "--bundle", second); code != 0 {
		t.Fatalf("the repeated propose: exit %d, stderr %s", code, stderr)
	}
	again := improveProposeTestDraft(t, w, report.Updated[0].Fingerprint)
	if !strings.Contains(again.Body, "- owner_unknown (6)") {
		t.Errorf("rerunning the same bundle changed the aggregate count:\n%s", again.Body)
	}
}

// TestImproveReview789EvidenceLessObservationsStayApart covers the rule the issue keeps for a
// record that names no origin: such a record falls back to one sighting of its where, and nothing
// else tells its observations apart, so two observations of one where at different times stay two
// occurrences rather than collapsing into one.
func TestImproveReview789EvidenceLessObservationsStayApart(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	first := improveRecord{Kind: improveKindRefusal, Key: "manifest_forbidden", Where: "rel-a",
		What: "manifest_forbidden", Count: 1, FirstAt: "2026-10-06T01:00:00Z", LastAt: "2026-10-06T01:00:00Z"}
	second := improveRecord{Kind: improveKindRefusal, Key: "manifest_forbidden", Where: "rel-a",
		What: "manifest_forbidden", Count: 1, FirstAt: "2026-10-06T02:00:00Z", LastAt: "2026-10-06T02:00:00Z"}
	bundle := improveProposeTestBundle(t, w, []improveRecord{first, second})
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
		t.Errorf("the draft carries %d seen entries, want one per observation (2): %+v", len(doc.Seen), doc.Seen)
	}
	if !strings.Contains(doc.Body, "- owner_unknown (2)") {
		t.Errorf("the draft does not count both observations:\n%s", doc.Body)
	}
}
