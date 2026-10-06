package manage

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests here pin the reading rules CRW-873 fixes: a draft is a crw-issue-draft/1 document
// keyed by its fingerprint, a blocked receipt's reason comes from the decision reply that
// answered it, and stored times compare as instants. They are hermetic like the rest of this
// package's tests: temporary paths, a synthetic store, no real relay state.

// improveParseDraft is a crw-issue-draft/1 document in the shape auditDraftLoad accepts.
func improveParseDraft(fingerprint, project, title string, seen []auditDraftSeen) auditDraft {
	return auditDraft{
		Schema:      auditDraftSchema,
		Fingerprint: fingerprint,
		Source:      auditDraftSource,
		Project:     project,
		Title:       title,
		Severity:    auditDraftDefaultSeverity,
		Body:        "the defect this draft carries",
		Labels:      []string{"bug"},
		Seen:        seen,
		State:       auditDraftStateDraft,
	}
}

// improveParseDraftJSON is a draft document as a file holds it.
func improveParseDraftJSON(t *testing.T, doc auditDraft) string {
	t.Helper()
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(append(data, '\n'))
}

// improveParseWriteDraft writes one draft under the name its fingerprint gives it.
func improveParseWriteDraft(t *testing.T, dir string, doc auditDraft) string {
	t.Helper()
	path := filepath.Join(dir, doc.Fingerprint+".json")
	improveTestWrite(t, path, improveParseDraftJSON(t, doc))
	return path
}

// improveParseWriteIndex writes the drafts directory's index.json, which is a listing and not a
// draft.
func improveParseWriteIndex(t *testing.T, dir string, fingerprints ...string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"schema": auditDraftIndexSchema, "drafts": fingerprints})
	if err != nil {
		t.Fatal(err)
	}
	improveTestWrite(t, filepath.Join(dir, auditDraftIndexFile), string(data))
}

// improveParseDraftsDir makes the drafts directory and returns it.
func improveParseDraftsDir(t *testing.T, s *improveTestState) string {
	t.Helper()
	dir := filepath.Join(s.root, "drafts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// improveParseConfig configures the relay store and the drafts directory and returns the bundle
// path a run writes to.
func improveParseConfig(t *testing.T, s *improveTestState, drafts string) string {
	t.Helper()
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"draft": map[string]any{"path": drafts},
		},
	}}})
	return filepath.Join(s.root, "bundle.json")
}

// improveParseCollect runs the collector and returns the bundle it wrote.
func improveParseCollect(t *testing.T, s *improveTestState, out string) improveBundle {
	t.Helper()
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	return improveTestReadBundle(t, out)
}

// TestImproveParseDraftsAreReadByFingerprint covers C1: only a crw-issue-draft/1 document is a
// draft, keyed by its fingerprint, and the directory's index.json is a listing rather than a
// draft, so it is neither read nor counted.
func TestImproveParseDraftsAreReadByFingerprint(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	dir := improveParseDraftsDir(t, s)
	improveParseWriteDraft(t, dir, improveParseDraft("aaaa1111bbbb2222", "project-a", "first draft",
		[]auditDraftSeen{{At: "2026-10-06T02:00:00Z"}, {At: "2026-10-06T01:00:00Z"}}))
	improveParseWriteDraft(t, dir, improveParseDraft("cccc3333dddd4444", "project-b", "second draft",
		[]auditDraftSeen{{At: "2026-10-06T05:00:00Z"}}))
	improveParseWriteIndex(t, dir, "aaaa1111bbbb2222", "cccc3333dddd4444")
	out := improveParseConfig(t, s, dir)

	bundle := improveParseCollect(t, s, out)
	if got := improveTestSourceOf(t, bundle, improveKindDraft); got.State != improveStateRead || got.Rows != 2 {
		t.Errorf("the draft source = %+v, want read with 2 rows: index.json is not a draft", got)
	}
	records := improveTestRecordsOf(bundle, improveKindDraft)
	if len(records) != 2 {
		t.Fatalf("draft records = %d, want 2 keyed by fingerprint: %+v", len(records), records)
	}
	byKey := map[string]improveRecord{}
	for _, record := range records {
		byKey[record.Key] = record
	}
	first, ok := byKey["aaaa1111bbbb2222"]
	if !ok {
		t.Fatalf("no record is keyed by the first fingerprint: %+v", records)
	}
	if first.Where != "project-a" || first.What != "first draft" {
		t.Errorf("the first draft record = %+v, want its project and title", first)
	}
	if first.FirstAt != "2026-10-06T01:00:00Z" || first.LastAt != "2026-10-06T02:00:00Z" {
		t.Errorf("the first draft times = %q..%q, want the earliest and latest seen[].at", first.FirstAt, first.LastAt)
	}
	if len(first.Evidence) != 1 || !strings.HasSuffix(first.Evidence[0], "aaaa1111bbbb2222.json") {
		t.Errorf("the first draft evidence = %v, want its file path", first.Evidence)
	}
	second, ok := byKey["cccc3333dddd4444"]
	if !ok || second.Where != "project-b" || second.What != "second draft" {
		t.Errorf("the second draft record = %+v, want the other fingerprint", second)
	}
}

// TestImproveParseDraftWithoutAFingerprintIsUnreadable covers C1's refusal: a document whose
// schema is crw-issue-draft/1 but whose fingerprint is empty is not a draft the collector can
// name, so the source is unreadable rather than silently keyed on nothing.
func TestImproveParseDraftWithoutAFingerprintIsUnreadable(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	dir := improveParseDraftsDir(t, s)
	doc := improveParseDraft("", "project-a", "nameless", []auditDraftSeen{{At: "2026-10-06T01:00:00Z"}})
	improveTestWrite(t, filepath.Join(dir, "nameless.json"), improveParseDraftJSON(t, doc))
	out := improveParseConfig(t, s, dir)

	code, stdout, stderr := improveTestRun(t, s, "--out", out)
	if code != 1 || !strings.Contains(stderr, improveReasonSourceUnreadable) {
		t.Fatalf("a draft with no fingerprint: exit %d, stderr %q, want exit 1 naming %s", code, stderr, improveReasonSourceUnreadable)
	}
	if stdout != "" {
		t.Errorf("a refused run wrote to stdout: %q", stdout)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a refused run left a bundle behind (stat err %v)", err)
	}
}

// TestImproveParseBlockedReasonComesFromTheDecisionReply covers C2: a child receipt carries no
// reason field, so the reason of a blockage is the note of the decision reply that answered it,
// and a blockage no decision answered keeps the bare outcome name.
func TestImproveParseBlockedReasonComesFromTheDecisionReply(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		for _, rel := range []struct{ id, issue, project string }{
			{"rel-a", "CRW-1", "project-a"},
			{"rel-b", "CRW-2", "project-b"},
		} {
			improveTestInsert(t, db, "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?,?,'active','parent','host','child','host',1,'[]','[]','2026-10-06T00:00:00Z','2026-10-06T00:00:00Z')", rel.id, rel.issue)
			improveTestInsert(t, db, "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at) VALUES (?,?,'2026-10-06T00:00:00Z')", rel.id, rel.project)
		}
		// The child's own receipt cannot carry a reason; a planted one must never be read.
		improveTestInsert(t, db, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES ('ev-blocked','rel-a',1,?,'blocked_needs_input','child','t','turn-1','completed',?,'final','2026-10-06T01:00:00Z','2026-10-06T01:00:00Z')", strings.Repeat("0", 64), "{\"outcome\":\"blocked_needs_input\",\"reason\":\"planted\",\"detail\":\"planted\"}")
		// The parent's decision reply answered that event, and its note is the reason.
		improveTestInsert(t, db, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES ('ev-reply','rel-a',1,?,'decision_reply','parent','t','turn-1','completed',?,'final','2026-10-06T02:00:00Z','2026-10-06T02:00:00Z')", strings.Repeat("0", 64), "{\"decision\":\"answer\",\"answersEvent\":\"ev-blocked\",\"note\":\"size over the line\"}")
		// A blockage no decision answered keeps its bare outcome name.
		improveTestInsert(t, db, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES ('ev-lonely','rel-b',1,?,'blocked_needs_input','child','t','turn-1','completed',?,'final','2026-10-06T03:00:00Z','2026-10-06T03:00:00Z')", strings.Repeat("0", 64), "{\"outcome\":\"blocked_needs_input\"}")
	})
	out := improveParseConfig(t, s, improveParseDraftsDir(t, s))

	bundle := improveParseCollect(t, s, out)
	splits := improveTestRecordsOf(bundle, improveKindSplit)
	if len(splits) != 2 {
		t.Fatalf("split records = %d, want one per relationship: %+v", len(splits), splits)
	}
	byWhere := map[string]improveRecord{}
	for _, record := range splits {
		byWhere[record.Where] = record
	}
	answered, ok := byWhere["rel-a"]
	if !ok {
		t.Fatalf("no record for the answered blockage: %+v", splits)
	}
	if answered.What != "size over the line" {
		t.Errorf("the answered blockage kept %q, want the decision reply's note", answered.What)
	}
	lonely, ok := byWhere["rel-b"]
	if !ok {
		t.Fatalf("no record for the unanswered blockage: %+v", splits)
	}
	if lonely.What != "blocked_needs_input" {
		t.Errorf("the unanswered blockage kept %q, want the bare outcome name", lonely.What)
	}
}

// TestImproveParseTimesCompareAsInstants covers C3: a stored time is compared as the instant it
// names, so fractional seconds and offsets cannot reverse the order, and a value that is not an
// instant sorts after one that is while keeping its text.
func TestImproveParseTimesCompareAsInstants(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T00:00:00.5Z','rel-a','ev-a','fraction','half a second in')")
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T00:00:00Z','rel-b','ev-b','fraction','the whole second')")
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-c','ev-c','offset','one hour in')")
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T09:30:00+09:00','rel-d','ev-d','offset','half an hour in, spelled with an offset')")
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('not-a-time','rel-e','ev-e','unparsed','a value that is not an instant')")
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-f','ev-f','unparsed','a value that is an instant')")
	})
	out := improveParseConfig(t, s, improveParseDraftsDir(t, s))

	bundle := improveParseCollect(t, s, out)
	byKey := map[string]improveRecord{}
	for _, record := range improveTestRecordsOf(bundle, improveKindRefusal) {
		byKey[record.Key] = record
	}
	cases := []struct {
		reason, first, last string
	}{
		{"fraction", "2026-10-06T00:00:00Z", "2026-10-06T00:00:00.5Z"},
		{"offset", "2026-10-06T09:30:00+09:00", "2026-10-06T01:00:00Z"},
		{"unparsed", "2026-10-06T01:00:00Z", "not-a-time"},
	}
	for _, want := range cases {
		got, ok := byKey[want.reason]
		if !ok {
			t.Errorf("no record for the %s case: %+v", want.reason, byKey)
			continue
		}
		if got.FirstAt != want.first || got.LastAt != want.last {
			t.Errorf("the %s case = %q..%q, want %q..%q", want.reason, got.FirstAt, got.LastAt, want.first, want.last)
		}
	}
}
