package manage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The tests here are hermetic: every path is temporary, the relay store is a synthetic
// SQLite file this file builds, and the relay CLI is a shell script this file writes.
// Nothing reaches a real store, App Server, network or home.

// improveTestState is a temporary world: the state directory the synthetic store and the
// configuration live in, and the paths the configuration names.
type improveTestState struct {
	root     string
	stateDir string
	dbPath   string
	config   string
	fake     string
	record   string
}

// improveTestSetup points HOME, CODEX_HOME, CRW_HOME, XDG_STATE_HOME and XDG_CONFIG_HOME
// at one temporary tree and returns its paths, so no test reaches a real one.
func improveTestSetup(t *testing.T) *improveTestState {
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
	return &improveTestState{
		root:     root,
		stateDir: state,
		dbPath:   filepath.Join(state, "relay.sqlite3"),
		config:   filepath.Join(root, "config", "crw", "config.json"),
		fake:     filepath.Join(root, "crw"),
		record:   filepath.Join(root, "crw.argv"),
	}
}

// improveTestFakeCRW writes a script that records its arguments and answers the two relay
// forms the collector uses: doctor, and dag-measurements. The answers travel in the
// environment, so no path is ever quoted into a shell string.
func improveTestFakeCRW(t *testing.T, s *improveTestState, measurements string) {
	t.Helper()
	doctor, err := json.Marshal(map[string]any{"stateSelection": map[string]any{"path": s.stateDir, "dbPath": s.dbPath}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(improveTestRecordEnv, s.record)
	t.Setenv(improveTestDoctorEnv, string(doctor))
	t.Setenv(improveTestMeasureEnv, measurements)
	script := strings.Join([]string{
		"#!/bin/sh",
		"echo \"$*\" >> \"$" + improveTestRecordEnv + "\"",
		"case \"$*\" in",
		"  *dag-measurements*) echo \"$" + improveTestMeasureEnv + "\" ;;",
		"  *doctor*) echo \"$" + improveTestDoctorEnv + "\" ;;",
		"esac",
		"exit 0",
	}, "\n") + "\n"
	if err := os.WriteFile(s.fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

const (
	improveTestRecordEnv  = "CRW_MANAGE_TEST_RECORD"
	improveTestDoctorEnv  = "CRW_MANAGE_TEST_DOCTOR"
	improveTestMeasureEnv = "CRW_MANAGE_TEST_MEASUREMENTS"
)

// improveTestConfig writes the crw configuration file and points CRW_CONFIG at it.
func improveTestConfig(t *testing.T, s *improveTestState, doc map[string]any) {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", s.config)
}

// improveTestEnv is the Env the collector runs with: the temporary streams, the process
// environment, and the fake crw as the executable the relay helper runs again.
func improveTestEnv(s *improveTestState, stdout, stderr *strings.Builder) *Env {
	return &Env{
		Stdin:      strings.NewReader(""),
		Stdout:     stdout,
		Stderr:     stderr,
		Getenv:     os.Getenv,
		Now:        func() time.Time { return time.Unix(0, 0).UTC() },
		Executable: s.fake,
	}
}

// improveTestStore opens the synthetic relay store, lets f insert rows, and closes it, so
// its write-ahead log is checkpointed away and the read-only open sees a clean file.
func improveTestStore(t *testing.T, s *improveTestState, f func(t *testing.T, db *sql.DB)) {
	t.Helper()
	ctx := context.Background()
	opened, err := store.Open(ctx, s.dbPath, "")
	if err != nil {
		t.Fatalf("the synthetic store: %v", err)
	}
	f(t, opened.DB)
	if err := opened.Close(); err != nil {
		t.Fatalf("closing the synthetic store: %v", err)
	}
}

// improveTestInsert runs one statement against the synthetic store.
func improveTestInsert(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("insert %q: %v", query, err)
	}
}

// improveTestRun runs crw manage improve collect with the given arguments and returns the
// exit status and both streams.
func improveTestRun(t *testing.T, s *improveTestState, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := improveRunCollect(context.Background(), improveTestEnv(s, &stdout, &stderr), args)
	return code, stdout.String(), stderr.String()
}

// improveTestReadBundle reads a written bundle document.
func improveTestReadBundle(t *testing.T, path string) improveBundle {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var bundle improveBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		t.Fatalf("the bundle is not the crw-improve-bundle/1 document: %v\n%s", err, data)
	}
	return bundle
}

// improveTestRecordsOf returns the records of one kind, in bundle order.
func improveTestRecordsOf(bundle improveBundle, kind string) []improveRecord {
	var out []improveRecord
	for _, record := range bundle.Records {
		if record.Kind == kind {
			out = append(out, record)
		}
	}
	return out
}

// improveTestSourceOf returns the source row of one kind.
func improveTestSourceOf(t *testing.T, bundle improveBundle, kind string) improveSourceRow {
	t.Helper()
	for _, source := range bundle.Sources {
		if source.Kind == kind {
			return source
		}
	}
	t.Fatalf("the bundle names no %s source: %+v", kind, bundle.Sources)
	return improveSourceRow{}
}

// improveTestMeasurementDoc is a dag-measurements/1 document the fake crw prints.
const improveTestMeasurementDoc = `{"ok":true,"schema":"dag-measurements/1","plan_id":"p1","plan_revision":3,"landed_nodes":2,` +
	`"parallelism":{"samples":4,"mean":2.5,"max":3},"conflicts_by_grade":{"samples":2,"by_grade":{"mechanical":1}},` +
	`"conflict_handling":{"samples":1,"seconds":30},"base_refresh":{"samples":1,"count":1},` +
	`"post_merge":{"samples":1,"ok":1},"duplicated_or_discarded":{"samples":0},` +
	`"cancelled_after_release":{"samples":1,"count":1},"landings":[]}`

// improveTestEightCases is the fixed 2026-10-06 input: issue key, project key, and the
// reason the receipt body carries.
var improveTestEightCases = []struct {
	issue   string
	project string
	reason  string
}{
	{"CRW-624", "project-cxc-port", "size overrun"},
	{"CRW-369", "project-cxc-port", "size overrun"},
	{"CRW-376", "project-cxc-port", "name collision"},
	{"CRW-378", "project-cxc-port", "size overrun"},
	{"CRW-382", "project-cxc-port", "name collision"},
	{"CRW-664", "project-dag", "size overrun"},
	{"CRW-685", "project-manage", "size overrun"},
	{"CRW-716", "project-manage", "name collision"},
}

// improveTestReceiptBody is a receipt body: a blocked_needs_input receipt, or a split
// decision, carrying the reason.
func improveTestReceiptBody(t *testing.T, split bool, reason string) string {
	t.Helper()
	body := map[string]any{"reason": reason}
	if split {
		body["decision"] = "split_approval"
	} else {
		body["outcome"] = "blocked_needs_input"
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// improveTestSeedEightCases inserts the eight relationships, their project keys, and one
// receipt each: a blocked_needs_input receipt or a split decision, alternating.
func improveTestSeedEightCases(t *testing.T, db *sql.DB) {
	t.Helper()
	for i, c := range improveTestEightCases {
		rid := "rel-" + c.issue
		improveTestInsert(t, db, "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?,?,'active','parent','host','child','host',1,'[]','[]','2026-10-06T00:00:00Z','2026-10-06T00:00:00Z')", rid, c.issue)
		improveTestInsert(t, db, "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at) VALUES (?,?,'2026-10-06T00:00:00Z')", rid, c.project)
		at := fmt.Sprintf("2026-10-06T%02d:00:00Z", i+1)
		outcome, producer := "blocked_needs_input", "child"
		if i%2 == 1 {
			outcome, producer = "decision_reply", "parent"
		}
		improveTestInsert(t, db, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES (?,?,1,?,?,?, 't','turn-1','completed',?,'final',?,?)",
			"ev-"+c.issue, rid, strings.Repeat("0", 64), outcome, producer, improveTestReceiptBody(t, i%2 == 1, c.reason), at, at)
	}
}

// improveTestWrite writes a file the configuration names.
func improveTestWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// improveTestFileState is a file's size and mtime, so a read that rewrote it is visible.
func improveTestFileState(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s:%d", info.ModTime().UTC().Format(time.RFC3339Nano), info.Size())
}

// improveTestTree lists every path under root, so a run that created or removed one is
// visible.
func improveTestTree(t *testing.T, root string) string {
	t.Helper()
	var paths []string
	err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, strings.TrimPrefix(path, root))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	return strings.Join(paths, ",")
}

// TestImproveCollectWritesTheBundleFormat is the red-first test: before this issue there
// is no crw-improve-bundle/1 document at all.
func TestImproveCollectWritesTheBundleFormat(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a','manifest_forbidden','detail a')")
	})
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	out := filepath.Join(s.root, "bundle.json")
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	if bundle.Schema != improveBundleSchema {
		t.Errorf("schema = %q, want %q", bundle.Schema, improveBundleSchema)
	}
	if bundle.Sources == nil || bundle.Records == nil {
		t.Errorf("the bundle carries no sources or records arrays: %+v", bundle)
	}
}

// TestImproveCollectNormalizesRelayRowsIntoRecords covers C2: refusal, fault and
// generation rows become bundle records.
func TestImproveCollectNormalizesRelayRowsIntoRecords(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a','manifest_forbidden','first')")
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T02:00:00Z','rel-b','ev-b','manifest_forbidden','second')")
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T03:00:00Z','rel-c','ev-c','unassigned_turn','third')")
		improveTestInsert(t, db, "INSERT INTO fault_ledger (fault_id, product, fault_class, component, severity, signature, scope, scope_key, state, occurrence_count, first_seen_at, last_seen_at, updated_at) VALUES ('f1','relay','observation_stalled','observation','medium','sig','project','project-a','open',5,'2026-10-06T01:00:00Z','2026-10-06T04:00:00Z','2026-10-06T04:00:00Z')")
		improveTestInsert(t, db, "INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, reason, opened_at, bound_at) VALUES ('rel-a',2,'revision-abc','bound','needs_changes_revision','2026-10-06T05:00:00Z','2026-10-06T05:00:00Z')")
		improveTestInsert(t, db, "INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, reason, opened_at, bound_at) VALUES ('rel-b',1,'managed-business-xyz','bound','initial_assignment','2026-10-06T06:00:00Z','2026-10-06T06:00:00Z')")
	})
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	if got := improveTestSourceOf(t, bundle, improveKindRelay); got.State != improveStateRead || got.Rows != 6 {
		t.Errorf("relay source = %+v, want read with 6 rows", got)
	}
	refusals := improveTestRecordsOf(bundle, improveKindRefusal)
	if len(refusals) != 2 {
		t.Fatalf("refusal records = %d, want 2 grouped by reason: %+v", len(refusals), refusals)
	}
	if refusals[0].Key != "manifest_forbidden" || refusals[0].Count != 2 || refusals[0].FirstAt != "2026-10-06T01:00:00Z" || refusals[0].LastAt != "2026-10-06T02:00:00Z" {
		t.Errorf("the manifest_forbidden record = %+v", refusals[0])
	}
	if refusals[1].Key != "unassigned_turn" || refusals[1].Count != 1 {
		t.Errorf("the unassigned_turn record = %+v", refusals[1])
	}
	faults := improveTestRecordsOf(bundle, improveKindFault)
	if len(faults) != 1 || faults[0].Key != "observation_stalled" || faults[0].Count != 5 || faults[0].Where != "project-a" {
		t.Errorf("fault records = %+v", faults)
	}
	generations := improveTestRecordsOf(bundle, improveKindGeneration)
	if len(generations) != 2 {
		t.Fatalf("generation records = %d, want 2: %+v", len(generations), generations)
	}
	if generations[0].Key != "initial_assignment" || generations[1].Key != "needs_changes_revision" {
		t.Errorf("generation records = %+v", generations)
	}
	if !sort.SliceIsSorted(bundle.Records, func(i, j int) bool {
		if bundle.Records[i].Kind != bundle.Records[j].Kind {
			return bundle.Records[i].Kind < bundle.Records[j].Kind
		}
		if bundle.Records[i].Key != bundle.Records[j].Key {
			return bundle.Records[i].Key < bundle.Records[j].Key
		}
		return bundle.Records[i].Where < bundle.Records[j].Where
	}) {
		t.Errorf("the records are not sorted by (kind, key, where): %+v", bundle.Records)
	}
}

// TestImproveCollectSplitRecordsCarryTheProjectKey covers C5: the eight fixed 2026-10-06
// cases become eight kind split records carrying the project key.
func TestImproveCollectSplitRecordsCarryTheProjectKey(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) { improveTestSeedEightCases(t, db) })
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	splits := improveTestRecordsOf(bundle, improveKindSplit)
	if len(splits) != 8 {
		t.Fatalf("split records = %d, want 8: %+v", len(splits), splits)
	}
	projects := map[string]int{}
	for _, record := range splits {
		projects[record.Key]++
		if record.What == "" {
			t.Errorf("a split record carries no reason: %+v", record)
		}
		if len(record.Evidence) == 0 {
			t.Errorf("a split record carries no evidence: %+v", record)
		}
	}
	if projects["project-cxc-port"] != 5 || projects["project-dag"] != 1 || projects["project-manage"] != 2 {
		t.Errorf("the split records name the wrong projects: %+v", projects)
	}
}

// TestImproveCollectMissingAndUnreadableSources covers C3.
func TestImproveCollectMissingAndUnreadableSources(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"audit": map[string]any{"path": filepath.Join(s.root, "absent.jsonl")},
		},
	}}})
	code, stdout, stderr := improveTestRun(t, s, "--out", filepath.Join(s.root, "bundle.json"))
	if code != 1 || !strings.Contains(stderr, improveReasonSourceUnreadable) {
		t.Fatalf("a configured unreadable path: exit %d, stderr %q, want exit 1 naming %s", code, stderr, improveReasonSourceUnreadable)
	}
	if stdout != "" {
		t.Errorf("a refused run wrote to stdout: %q", stdout)
	}
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect without the audit source: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	if got := improveTestSourceOf(t, bundle, improveKindAudit); got.State != improveStateMissing {
		t.Errorf("an unconfigured source = %+v, want state %s", got, improveStateMissing)
	}
}

// TestImproveCollectReadsTheStoreInPlaceWithoutSidecars covers C6.
func TestImproveCollectReadsTheStoreInPlaceWithoutSidecars(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a','manifest_forbidden','detail')")
	})
	before := improveTestFileState(t, s.dbPath)
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	after := improveTestFileState(t, s.dbPath)
	if before != after {
		t.Errorf("the store changed: %s -> %s", before, after)
	}
	for _, sidecar := range []string{s.dbPath + "-wal", s.dbPath + "-shm"} {
		if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
			t.Errorf("the read left the sidecar %s (stat err %v)", sidecar, err)
		}
	}
}

// TestImproveCollectNormalizesDagMeasurements covers the dag source.
func TestImproveCollectNormalizesDagMeasurements(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	improveTestFakeCRW(t, s, improveTestMeasurementDoc)
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"dag":   map[string]any{"path": s.stateDir, "pattern": "p1"},
		},
	}}})
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	records := improveTestRecordsOf(bundle, improveKindDag)
	if len(records) != 7 {
		t.Fatalf("dag records = %d, want 7: %+v", len(records), records)
	}
	byKey := map[string]improveRecord{}
	for _, record := range records {
		byKey[record.Key] = record
	}
	if got, ok := byKey["p1:cancelled_after_release"]; !ok || got.Count != 1 {
		t.Errorf("the cancelled_after_release record = %+v", got)
	}
	if got, ok := byKey["p1:parallelism"]; !ok || got.Count != 4 {
		t.Errorf("the parallelism record = %+v", got)
	}
	recorded, err := os.ReadFile(s.record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recorded), "dag-measurements --plan p1") {
		t.Errorf("the relay CLI was not called with the plan: %s", recorded)
	}
}

// TestImproveCollectReadsTheAuditLedgerAndIssueList covers the file sources.
func TestImproveCollectReadsTheAuditLedgerAndIssueList(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	ledger := filepath.Join(s.root, "ledger.jsonl")
	improveTestWrite(t, ledger, "{\"mode\":\"pr\",\"subject\":\"pr-1\",\"issue\":\"CRW-1\",\"status\":\"ok\",\"graded_at\":\"2026-10-06T01:00:00Z\"}\n{\"mode\":\"pr\",\"subject\":\"pr-2\",\"issue\":\"CRW-2\",\"status\":\"invalid\",\"graded_at\":\"2026-10-06T02:00:00Z\"}\n")
	issues := filepath.Join(s.root, "issues.json")
	improveTestWrite(t, issues, "[{\"identifier\":\"CRW-1\",\"title\":\"one\",\"state\":\"In Progress\",\"updatedAt\":\"2026-10-06T03:00:00Z\"}]")
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"audit": map[string]any{"path": ledger},
		},
		"issue_list": issues,
	}}})
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	if got := improveTestRecordsOf(bundle, improveKindAudit); len(got) != 2 {
		t.Errorf("audit records = %+v, want 2", got)
	}
	if got := improveTestRecordsOf(bundle, improveKindIssue); len(got) != 1 || got[0].Key != "CRW-1" {
		t.Errorf("issue records = %+v, want one CRW-1", got)
	}
}

// TestImproveCollectTopLevelImproveSectionIsAccepted covers the issue body's naming: a
// file with no manage section carries the improve section at the top level.
func TestImproveCollectTopLevelImproveSectionIsAccepted(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	improveTestConfig(t, s, map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}})
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect with a top-level improve section: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	if got := improveTestSourceOf(t, bundle, improveKindRelay); got.State != improveStateRead {
		t.Errorf("relay source = %+v, want read", got)
	}
}

// TestImproveCollectLeavesTheTemporaryHomeAlone covers C4's observable part: the run
// writes only the file it was asked for and creates nothing under the temporary home.
func TestImproveCollectLeavesTheTemporaryHomeAlone(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a','manifest_forbidden','detail')")
	})
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	home := os.Getenv("HOME")
	before := improveTestTree(t, home)
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	after := improveTestTree(t, home)
	if before != after {
		t.Errorf("the run changed the temporary home:\nbefore %s\nafter  %s", before, after)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("the run did not write the file it was asked for: %v", err)
	}
}

// TestImproveCollectRefusesAnUnknownOption covers the command line.
func TestImproveCollectRefusesAnUnknownOption(t *testing.T) {
	s := improveTestSetup(t)
	code, _, stderr := improveTestRun(t, s, "--nope")
	if code != usageExit || !strings.Contains(stderr, "--nope") {
		t.Errorf("an unknown option: exit %d, stderr %q", code, stderr)
	}
}

// TestImproveCollectNormalizesCriteriaAndDrafts covers the criteria-refresh round trip and
// the audit drafts: both become records, and both stay missing when unconfigured.
func TestImproveCollectNormalizesCriteriaAndDrafts(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO canonical_criteria (relationship_id, criterion_id, title, required, set_digest, recorded_at) VALUES ('rel-a','c1','first',1,'digest-1','2026-10-06T01:00:00Z')")
		improveTestInsert(t, db, "INSERT INTO canonical_criteria (relationship_id, criterion_id, title, required, set_digest, recorded_at) VALUES ('rel-a','c2','second',1,'digest-1','2026-10-06T02:00:00Z')")
		improveTestInsert(t, db, "INSERT INTO canonical_criteria (relationship_id, criterion_id, title, required, set_digest, recorded_at) VALUES ('rel-a','c3','third',1,'digest-2','2026-10-06T03:00:00Z')")
	})
	drafts := filepath.Join(s.root, "drafts")
	if err := os.MkdirAll(drafts, 0o755); err != nil {
		t.Fatal(err)
	}
	improveTestWrite(t, filepath.Join(drafts, "one.json"), "{\"schema\":\"crw-issue-draft/1\",\"fingerprint\":\"one\",\"project\":\"project-a\",\"title\":\"one\"}")
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"draft": map[string]any{"path": drafts},
		},
	}}})
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	criteria := improveTestRecordsOf(bundle, improveKindCriteria)
	if len(criteria) != 2 {
		t.Fatalf("criteria records = %+v, want two set digests", criteria)
	}
	if criteria[0].Where != "digest-1" || criteria[0].Count != 2 || criteria[0].FirstAt != "2026-10-06T01:00:00Z" || criteria[0].LastAt != "2026-10-06T02:00:00Z" {
		t.Errorf("the digest-1 record = %+v", criteria[0])
	}
	if got := improveTestRecordsOf(bundle, improveKindDraft); len(got) != 1 || got[0].Key != "one" || got[0].Where != "project-a" {
		t.Errorf("draft records = %+v, want the one fingerprint at project-a", got)
	}
	if got := improveTestSourceOf(t, bundle, improveKindIntervention); got.State != improveStateMissing {
		t.Errorf("the unconfigured intervention source = %+v, want missing", got)
	}
}

// TestImproveCollectLeavesNoTemporaryFileBesideTheOutput covers the atomic write: a
// successful run leaves the bundle and nothing else in the output directory.
func TestImproveCollectLeavesNoTemporaryFileBesideTheOutput(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a','manifest_forbidden','detail')")
	})
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	dir := filepath.Join(s.root, "out")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "bundle.json" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("the output directory holds %v, want only bundle.json", names)
	}
}

// TestImproveCollectWritesNothingWhenTheContextEnds covers cancellation: a context that
// ended while the sources were read leaves no bundle behind.
func TestImproveCollectWritesNothingWhenTheContextEnds(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a','manifest_forbidden','detail')")
	})
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	out := filepath.Join(s.root, "bundle.json")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr strings.Builder
	code := improveRunCollect(ctx, improveTestEnv(s, &stdout, &stderr), []string{"--out", out})
	if code == 0 {
		t.Fatalf("an ended context reported success")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("an ended context left a bundle behind (stat err %v)", err)
	}
}

// TestImproveCollectKeepsDelimitedValuesApart covers the record identity: a value that
// carries a delimiter stays its own record rather than merging with another.
func TestImproveCollectKeepsDelimitedValuesApart(t *testing.T) {
	s := improveTestSetup(t)
	hostile := "quoted \" and newline \n inside"
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a',?,'hostile')", hostile)
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T02:00:00Z','rel-b','ev-b','plain','plain')")
	})
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	refusals := improveTestRecordsOf(bundle, improveKindRefusal)
	if len(refusals) != 2 {
		t.Fatalf("refusal records = %d, want 2 (the hostile reason stays its own record): %+v", len(refusals), refusals)
	}
	found := false
	for _, record := range refusals {
		if record.Key == hostile {
			found = true
		}
	}
	if !found {
		t.Errorf("the hostile reason did not round-trip: %+v", refusals)
	}
}

// TestImproveCollectRefusesAnOutputThatIsAConfiguredSource covers the overwrite guard: a
// bundle never replaces the evidence it was read from.
func TestImproveCollectRefusesAnOutputThatIsAConfiguredSource(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	ledger := filepath.Join(s.root, "ledger.jsonl")
	improveTestWrite(t, ledger, "{\"mode\":\"pr\",\"issue\":\"CRW-1\",\"status\":\"ok\",\"graded_at\":\"2026-10-06T01:00:00Z\"}\n")
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"audit": map[string]any{"path": ledger},
		},
	}}})
	before, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr := improveTestRun(t, s, "--out", ledger)
	if code == 0 || !strings.Contains(stderr, "never overwrites") {
		t.Fatalf("an output equal to a configured source: exit %d, stderr %q", code, stderr)
	}
	after, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the refused run changed the ledger: %q -> %q", before, after)
	}
}

// TestImproveCollectRefusesAnOutputInsideAConfiguredDirectory covers the guard's prefix
// rule: a bundle is not written under a configured source directory either.
func TestImproveCollectRefusesAnOutputInsideAConfiguredDirectory(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	drafts := filepath.Join(s.root, "drafts")
	if err := os.MkdirAll(drafts, 0o755); err != nil {
		t.Fatal(err)
	}
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"draft": map[string]any{"path": drafts},
		},
	}}})
	code, _, stderr := improveTestRun(t, s, "--out", filepath.Join(drafts, "bundle.json"))
	if code == 0 || !strings.Contains(stderr, "never overwrites") {
		t.Fatalf("an output inside a configured directory: exit %d, stderr %q", code, stderr)
	}
}

// TestImproveCollectRefusesAnEmptyOutValue covers both --out forms.
func TestImproveCollectRefusesAnEmptyOutValue(t *testing.T) {
	s := improveTestSetup(t)
	for _, args := range [][]string{{"--out", ""}, {"--out="}} {
		code, stdout, stderr := improveTestRun(t, s, args...)
		if code != usageExit || stdout != "" || !strings.Contains(stderr, "--out") {
			t.Errorf("args %q: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
}

// TestImproveCollectDagKeepsMetricValues covers the dag record's description: the metric's
// values and absence reason survive, so equal sample counts stay distinguishable.
func TestImproveCollectDagKeepsMetricValues(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	doc := "{\"ok\":true,\"schema\":\"dag-measurements/1\",\"plan_id\":\"p1\",\"parallelism\":{\"samples\":0,\"absent\":\"no_recorded_pass\"},\"conflicts_by_grade\":{\"samples\":3,\"by_grade\":{\"mechanical\":3}}}"
	improveTestFakeCRW(t, s, doc)
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"dag":   map[string]any{"path": s.stateDir, "pattern": "p1"},
		},
	}}})
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	records := improveTestRecordsOf(bundle, improveKindDag)
	byKey := map[string]improveRecord{}
	for _, record := range records {
		byKey[record.Key] = record
	}
	if got, ok := byKey["p1:parallelism"]; !ok || !strings.Contains(got.What, "no_recorded_pass") {
		t.Errorf("the absent parallelism metric lost its reason: %+v", got)
	}
	if got, ok := byKey["p1:conflicts_by_grade"]; !ok || !strings.Contains(got.What, "mechanical") {
		t.Errorf("the conflicts metric lost its values: %+v", got)
	}
}

// TestImproveCollectDagWithoutAPatternIsAnError covers the misconfigured dag source.
func TestImproveCollectDagWithoutAPatternIsAnError(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	improveTestFakeCRW(t, s, improveTestMeasurementDoc)
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"dag":   map[string]any{"path": s.stateDir},
		},
	}}})
	code, _, stderr := improveTestRun(t, s, "--out", filepath.Join(s.root, "bundle.json"))
	if code != 1 || !strings.Contains(stderr, improveReasonSourceUnreadable) {
		t.Fatalf("a dag source with no pattern: exit %d, stderr %q", code, stderr)
	}
}

// TestImproveCollectCountsOnlyContributingSplitRows covers the source row count: an answer
// decision is read but contributes no record and is not counted.
func TestImproveCollectCountsOnlyContributingSplitRows(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES ('ev-a','rel-a',1,?,'decision_reply','parent','t','turn-1','completed',?,'final','2026-10-06T01:00:00Z','2026-10-06T01:00:00Z')", strings.Repeat("0", 64), "{\"decision\":\"answer\",\"note\":\"a note\"}")
	})
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	if got := improveTestSourceOf(t, bundle, improveKindRelay); got.Rows != 0 {
		t.Errorf("the relay source counted a non-contributing decision: %+v", got)
	}
	if got := improveTestRecordsOf(bundle, improveKindSplit); len(got) != 0 {
		t.Errorf("an answer decision produced split records: %+v", got)
	}
}

// TestImproveCollectManageSectionWithoutImproveIsEmpty covers the fallback rule: a stale
// top-level improve section is not read once the file carries a manage section.
func TestImproveCollectManageSectionWithoutImproveIsEmpty(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a','manifest_forbidden','detail')")
	})
	improveTestConfig(t, s, map[string]any{
		"manage":  map[string]any{"audit": map[string]any{}},
		"improve": map[string]any{"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}}},
	})
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	if got := improveTestSourceOf(t, bundle, improveKindRelay); got.State != improveStateMissing {
		t.Errorf("a stale top-level improve section was read: %+v", got)
	}
}

// TestImproveCollectRefusesANullJSONLine covers the JSONL reader: a null line is not an
// object and is refused rather than becoming a phantom record.
func TestImproveCollectRefusesANullJSONLine(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	ledger := filepath.Join(s.root, "ledger.jsonl")
	improveTestWrite(t, ledger, "null\n")
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"audit": map[string]any{"path": ledger},
		},
	}}})
	code, _, stderr := improveTestRun(t, s, "--out", filepath.Join(s.root, "bundle.json"))
	if code != 1 || !strings.Contains(stderr, "not a JSON object") {
		t.Fatalf("a null ledger line: exit %d, stderr %q", code, stderr)
	}
}

// TestImproveCollectRefusesAnIssueListWithoutIssues covers the issue-list shape.
func TestImproveCollectRefusesAnIssueListWithoutIssues(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	issues := filepath.Join(s.root, "issues.json")
	improveTestWrite(t, issues, "{\"error\":\"unauthorized\"}")
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources":    map[string]any{"relay": map[string]any{"path": s.stateDir}},
		"issue_list": issues,
	}}})
	code, _, stderr := improveTestRun(t, s, "--out", filepath.Join(s.root, "bundle.json"))
	if code != 1 || !strings.Contains(stderr, improveReasonSourceUnreadable) {
		t.Fatalf("an object without issues: exit %d, stderr %q", code, stderr)
	}
}

// TestImproveCollectSplitPrefersTheDecisionNote covers the merge rule: a specific note
// beats the bare outcome name a blocked receipt carries.
func TestImproveCollectSplitPrefersTheDecisionNote(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES ('rel-a','CRW-1','active','parent','host','child','host',1,'[]','[]','2026-10-06T00:00:00Z','2026-10-06T00:00:00Z')")
		improveTestInsert(t, db, "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at) VALUES ('rel-a','project-a','2026-10-06T00:00:00Z')")
		// The blocked receipt sorts first by event id, and carries only the bare outcome name.
		improveTestInsert(t, db, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES ('ev-a',?,1,?,'blocked_needs_input','child','t','turn-1','completed',?,'final','2026-10-06T01:00:00Z','2026-10-06T01:00:00Z')", "rel-a", strings.Repeat("0", 64), "{\"outcome\":\"blocked_needs_input\"}")
		improveTestInsert(t, db, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES ('ev-z',?,1,?,'decision_reply','parent','t','turn-1','completed',?,'final','2026-10-06T02:00:00Z','2026-10-06T02:00:00Z')", "rel-a", strings.Repeat("0", 64), "{\"decision\":\"split_approval\",\"reason\":\"size overrun\"}")
	})
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	splits := improveTestRecordsOf(bundle, improveKindSplit)
	if len(splits) != 1 {
		t.Fatalf("split records = %+v, want one merged record", splits)
	}
	if splits[0].What != "size overrun" {
		t.Errorf("the merged split record kept %q, want the decision's reason", splits[0].What)
	}
}

// TestImproveCollectRecordsTheCriteriaRefreshHistory covers the criteria-registration
// round trip: the relay journals every registration with its set digest, so a relationship
// whose criteria were re-registered shows both sets, not only the current one.
func TestImproveCollectRecordsTheCriteriaRefreshHistory(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO canonical_criteria (relationship_id, criterion_id, title, required, set_digest, recorded_at) VALUES ('rel-a','c1','only',1,'digest-b','2026-10-06T02:00:00Z')")
		improveTestInsert(t, db, "INSERT INTO journal (at, kind, subject, detail) VALUES ('2026-10-06T01:00:00Z','criteria_registered','rel-a','{\"setDigest\":\"digest-a\",\"count\":2}')")
		improveTestInsert(t, db, "INSERT INTO journal (at, kind, subject, detail) VALUES ('2026-10-06T02:00:00Z','criteria_registered','rel-a','{\"setDigest\":\"digest-b\",\"count\":1}')")
	})
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	criteria := improveTestRecordsOf(bundle, improveKindCriteria)
	if len(criteria) != 2 {
		t.Fatalf("criteria records = %+v, want the registered set digest-a and the current digest-b", criteria)
	}
	seen := map[string]improveRecord{}
	for _, record := range criteria {
		seen[record.Where] = record
	}
	if got, ok := seen["digest-a"]; !ok || got.Count != 2 {
		t.Errorf("the registered set digest-a = %+v, want the journal's count 2", got)
	}
	if _, ok := seen["digest-b"]; !ok {
		t.Errorf("the current set digest-b is missing: %+v", criteria)
	}
}

// TestImproveCollectRefusesAnOutputUnderASymlinkedSourceDirectory covers the correction's
// first case: a new file whose parent is a symbolic link into a configured source directory
// is refused, and nothing appears in the source directory. The parent is resolved with
// filepath.EvalSymlinks, so an output that does not exist yet cannot slip past the prefix
// check through an unresolved spelling.
func TestImproveCollectRefusesAnOutputUnderASymlinkedSourceDirectory(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	link := filepath.Join(s.root, "link")
	if err := os.Symlink(s.stateDir, link); err != nil {
		t.Fatal(err)
	}
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	code, _, stderr := improveTestRun(t, s, "--out", filepath.Join(link, "bundle.json"))
	if code == 0 || !strings.Contains(stderr, "never overwrites") {
		t.Fatalf("an output under a symlinked source directory: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(s.stateDir, "bundle.json")); !os.IsNotExist(err) {
		t.Errorf("the refused run left a file in the source directory (stat err %v)", err)
	}
}

// TestImproveCollectRefusesASymlinkDestination covers the correction's second case: an
// output whose last name is a symbolic link is refused, because the rename would replace
// whatever it points at.
func TestImproveCollectRefusesASymlinkDestination(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	ledger := filepath.Join(s.root, "ledger.jsonl")
	improveTestWrite(t, ledger, "{\"mode\":\"pr\",\"issue\":\"CRW-1\",\"status\":\"ok\",\"graded_at\":\"2026-10-06T01:00:00Z\"}\n")
	link := filepath.Join(s.root, "out-link")
	if err := os.Symlink(ledger, link); err != nil {
		t.Fatal(err)
	}
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"audit": map[string]any{"path": ledger},
		},
	}}})
	code, _, stderr := improveTestRun(t, s, "--out", link)
	if code != 1 || !strings.Contains(stderr, improveReasonOutputSymlink) {
		t.Fatalf("a symlink destination: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonOutputSymlink)
	}
	before, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), "\"mode\"") {
		t.Errorf("the ledger behind the link was replaced: %q", before)
	}
}

// TestImproveCollectWritesAnOrdinaryOutput is the control: a plain --out with an existing
// parent still writes the bundle.
func TestImproveCollectWritesAnOrdinaryOutput(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a','manifest_forbidden','detail')")
	})
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	dir := filepath.Join(s.root, "out")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	if bundle := improveTestReadBundle(t, out); bundle.Schema != improveBundleSchema {
		t.Errorf("the ordinary output was not written: %+v", bundle)
	}
}

// TestImproveCollectRefusesAnOutputWithNoParentDirectory covers the named refusal of a
// destination whose parent does not exist.
func TestImproveCollectRefusesAnOutputWithNoParentDirectory(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
	code, _, stderr := improveTestRun(t, s, "--out", filepath.Join(s.root, "absent-dir", "bundle.json"))
	if code != 1 || !strings.Contains(stderr, improveReasonOutputParent) {
		t.Fatalf("an output with no parent directory: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonOutputParent)
	}
}

// TestImproveCollectRefusesAHardLinkedOutput covers the os.SameFile guard: an --out that is
// a hard link to a configured source file names the same file by a different spelling, so
// the prefix comparison alone would not catch it.
func TestImproveCollectRefusesAHardLinkedOutput(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	ledger := filepath.Join(s.root, "ledger.jsonl")
	improveTestWrite(t, ledger, "{\"mode\":\"pr\",\"issue\":\"CRW-1\",\"status\":\"ok\",\"graded_at\":\"2026-10-06T01:00:00Z\"}\n")
	out := filepath.Join(s.root, "out.json")
	if err := os.Link(ledger, out); err != nil {
		t.Skipf("hard links are unavailable here: %v", err)
	}
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"audit": map[string]any{"path": ledger},
		},
	}}})
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if code != 1 || !strings.Contains(stderr, "never overwrites") {
		t.Fatalf("a hard-linked output: exit %d, stderr %q", code, stderr)
	}
	data, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\"mode\"") {
		t.Errorf("the ledger behind the hard link was replaced: %q", data)
	}
}

// TestImproveCollectRefusesAnOutputUnderARootSource covers the containment predicate at the
// root: a source resolving to the filesystem root must refuse every destination, rather than
// building a // prefix that no cleaned path carries.
func TestImproveCollectRefusesAnOutputUnderARootSource(t *testing.T) {
	s := improveTestSetup(t)
	section := improveSection{Sources: map[string]improveSourceConfig{"audit": {Path: string(filepath.Separator)}}}
	out := filepath.Join(s.root, "bundle.json")
	if err := improveRefuseInputOutput(out, section); err == nil {
		t.Errorf("a destination under a root source was allowed: %s", out)
	}
	if err := improveRefuseInputOutput(out, improveSection{}); err != nil {
		t.Errorf("a destination with no configured source was refused: %v", err)
	}
}
