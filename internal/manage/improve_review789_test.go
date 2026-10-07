package manage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	section := map[string]any{}
	for key, value := range improve {
		section[key] = value
	}
	if _, ok := section["sources"]; !ok {
		section["sources"] = map[string]any{"relay": map[string]any{"path": s.stateDir}}
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
	issues := filepath.Join(s.root, "issues.json")
	improveTestWrite(t, issues, improveReview789IssueList("CRW-739", "size overrun", ""))
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
// one real location makes the one sighting it has rather than a sighting with an empty head.
func TestImproveReview789BlankEvidenceMakesNoSighting(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveReview789SeedSplit(t, db, "rel-a", "CRW-1", "project-a", "size overrun", "ev-a")
	})
	improveReview789Configure(t, s, manageState, map[string]any{})

	improveReview789Collect(t, s)
	report := improveReview789Propose(t, s, improveReview789BundlePath(s))
	if len(report.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", report.Created)
	}
	doc := improveReview789Draft(t, manageState, report.Created[0].Fingerprint)
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

// improveReview789FaultRecord is one fault ledger record of a bundle: the class it groups by, the
// project scope it names, its signature, how many occurrences the ledger row holds, when it was
// last seen, and the ledger row it came from.
func improveReview789FaultRecord(count int, lastSeen string) improveRecord {
	return improveRecord{Kind: improveKindFault, Key: "observation_stalled", Where: "project-a",
		What: "a signature", Count: count, FirstAt: "2026-10-06T01:00:00Z", LastAt: lastSeen,
		Evidence: []string{"fault:f1"}}
}

// TestImproveReview789AggregateCountIsNotUnderstated covers the review finding that a record which
// aggregates its occurrences must not be understated when its count moves with no new origin
// location: a fault ledger row whose occurrence count grew reports the whole count in the draft,
// not the count the draft first recorded plus the one sighting the new last-seen time produced.
func TestImproveReview789AggregateCountIsNotUnderstated(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	first := improveProposeTestBundle(t, w, []improveRecord{improveReview789FaultRecord(5, "2026-10-06T01:00:00Z")})
	if code, _, stderr := improveProposeTestRun(t, w, "--bundle", first); code != 0 {
		t.Fatalf("the first propose: exit %d, stderr %s", code, stderr)
	}

	// The same ledger row now holds eight occurrences, with no new origin location.
	second := improveProposeTestBundle(t, w, []improveRecord{improveReview789FaultRecord(8, "2026-10-06T02:00:00Z")})
	code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", second)
	if code != 0 {
		t.Fatalf("the second propose: exit %d, stderr %s", code, stderr)
	}
	report := improveProposeTestReport(t, stdout)
	if len(report.Updated) != 1 {
		t.Fatalf("updated = %+v, want the existing draft to grow", report.Updated)
	}
	doc := improveProposeTestDraft(t, w, report.Updated[0].Fingerprint)
	if !strings.Contains(doc.Body, "- owner_unknown (8)") {
		t.Errorf("the merged draft understates the aggregated count:\n%s", doc.Body)
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
