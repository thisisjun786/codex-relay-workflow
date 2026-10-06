package manage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The tests here cover the identity of the files the collection actually opens: the relay
// store inside a relay source directory, reached through a link, a hard link, or a swap
// between the first comparison and the rename; and the DAG source's own in-place store read,
// which must leave the store's bytes and mtime alone and create no SQLite sidecar.
//
// Every path is temporary and the store is a synthetic file this package builds, so nothing
// reaches a real store, App Server or home.

// improveInputDigest is a stand-in for a plan node's criteria digest: 64 lowercase hex
// characters derived from a name, the shape the DAG reader requires.
func improveInputDigest(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

// improveInputSeedPlan registers one plan carrying the given implementation nodes in the
// synthetic store under the temporary state directory.
func improveInputSeedPlan(t *testing.T, s *improveTestState, plan string, nodes ...string) {
	t.Helper()
	improveInputSeedPlanAt(t, s.dbPath, plan, nodes...)
}

// improveInputSeedPlanAt registers one plan at a store path, written through the DAG package's
// own exported Repo so the rows are exactly the ones the product reads, and then closes the
// store so its write-ahead log is checkpointed away and the read-only open sees a clean file.
func improveInputSeedPlanAt(t *testing.T, dbPath, plan string, nodes ...string) {
	t.Helper()
	ctx := context.Background()
	opened, err := store.Open(ctx, dbPath, "")
	if err != nil {
		t.Fatalf("the synthetic store: %v", err)
	}
	changes := make([]any, 0, len(nodes))
	for _, node := range nodes {
		changes = append(changes, map[string]any{
			"op": dag.OpAddNode,
			"node": map[string]any{
				"node_id":             node,
				"issue_key":           "CRW-" + node,
				"kind":                dag.NodeImplementation,
				"criteria_set_digest": improveInputDigest(node),
			},
		})
	}
	document, err := json.Marshal(map[string]any{
		"schema":                   dag.SchemaRevision,
		"plan_id":                  plan,
		"project_key":              "PRJ-INPUT",
		"request_id":               "improve-input-" + plan,
		"expected_parent_revision": 0,
		"author_task_id":           "task-improve-input",
		"changes":                  changes,
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := dag.DecodeRevision(document)
	if err != nil {
		t.Fatalf("the plan revision: %v", err)
	}
	if _, err := (&dag.Repo{Store: opened}).Put(ctx, revision); err != nil {
		t.Fatalf("registering plan %s: %v", plan, err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("closing the synthetic store: %v", err)
	}
}

// improveInputDagConfig is the configuration of a relay source and a DAG source over the
// temporary state directory, which is the shape the DAG tests here use.
func improveInputDagConfig(t *testing.T, s *improveTestState, pattern string) {
	t.Helper()
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"dag":   map[string]any{"path": s.stateDir, "pattern": pattern},
		},
	}}})
}

// improveInputRelayConfig is the configuration of the relay source alone.
func improveInputRelayConfig(t *testing.T, s *improveTestState) {
	t.Helper()
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.stateDir}},
	}}})
}

// TestImproveInputRefusesTheLinkedStoreInsideTheSourceDirectory covers the P0: the relay
// source is a directory and its relay.sqlite3 is a link to a database elsewhere, so the file
// the collection actually opens is not the configured directory. Before this issue the guard
// compared the configured paths only, the run succeeded, and the rename replaced the linked
// database with the bundle.
func TestImproveInputRefusesTheLinkedStoreInsideTheSourceDirectory(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	target := filepath.Join(s.root, "db", "actual.sqlite3")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(s.dbPath, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(s.stateDir, improveStoreFile)); err != nil {
		t.Skipf("symbolic links are unavailable here: %v", err)
	}
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	tree := improveTestTree(t, s.stateDir)
	improveInputRelayConfig(t, s)
	code, _, stderr := improveTestRun(t, s, "--out", target)
	if code != 1 || !strings.Contains(stderr, improveReasonOutputIsInput) {
		t.Fatalf("an output naming the linked store: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonOutputIsInput)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the refused run removed the store: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("the refused run replaced the store the link points at (%d bytes -> %d bytes)", len(before), len(after))
	}
	if got := improveTestTree(t, s.stateDir); got != tree {
		t.Errorf("the refused run left a file in the source directory: %s -> %s", tree, got)
	}
}

// TestImproveInputRefusesAHardLinkedDestination covers the second identity: the destination
// names the store file the relay source opens by a second name, outside the configured
// directory, so only the file identity itself catches it.
func TestImproveInputRefusesAHardLinkedDestination(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	out := filepath.Join(s.root, "bundle.json")
	if err := os.Link(s.dbPath, out); err != nil {
		t.Skipf("hard links are unavailable here: %v", err)
	}
	before := improveTestFileState(t, s.dbPath)
	improveInputRelayConfig(t, s)
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if code != 1 || !strings.Contains(stderr, improveReasonOutputIsInput) {
		t.Fatalf("an output hard-linked to the store: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonOutputIsInput)
	}
	if after := improveTestFileState(t, s.dbPath); after != before {
		t.Errorf("the store changed: %s -> %s", before, after)
	}
}

// TestImproveInputRefusesADestinationSwappedBeforeTheRename covers the race: the destination
// is an ordinary absent file when the first comparison runs and names the store the
// collection just read immediately before the rename. The comparison repeated there has to
// catch it.
func TestImproveInputRefusesADestinationSwappedBeforeTheRename(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	dir := filepath.Join(s.root, "out")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "bundle.json")
	improveInputRelayConfig(t, s)
	swapped := false
	previous := improveInputBeforeRename
	improveInputBeforeRename = func(improveOutputPlan) {
		swapped = true
		if err := os.Link(s.dbPath, out); err != nil {
			t.Errorf("linking the store onto the destination: %v", err)
		}
	}
	t.Cleanup(func() { improveInputBeforeRename = previous })
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if !swapped {
		t.Fatalf("the pre-rename comparison ran without the seam: exit %d, stderr %q", code, stderr)
	}
	if code != 1 || !strings.Contains(stderr, improveReasonOutputIsInput) {
		t.Fatalf("a destination swapped before the rename: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonOutputIsInput)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("the refused run removed the destination: %v", err)
	}
	stored, err := os.Stat(s.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, stored) {
		t.Errorf("the refused run wrote the bundle over the destination: %s no longer names the store", out)
	}
}

// TestImproveInputDagReadLeavesTheStoreUnchanged covers the P1: the DAG source reads the
// store in place, so the store's bytes and mtime are the same before and after and no -wal or
// -shm appears beside it. The relay CLI is offered as a fake that answers and records its
// arguments, so a run that still shelled out would both leave a record and depend on it.
func TestImproveInputDagReadLeavesTheStoreUnchanged(t *testing.T) {
	s := improveTestSetup(t)
	improveInputSeedPlan(t, s, "p1", "a", "b")
	improveTestFakeCRW(t, s, improveTestMeasurementDoc)
	improveInputDagConfig(t, s, "p1")
	before := improveTestFileState(t, s.dbPath)
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	if after := improveTestFileState(t, s.dbPath); after != before {
		t.Errorf("the DAG read changed the store: %s -> %s", before, after)
	}
	for _, sidecar := range []string{s.dbPath + "-wal", s.dbPath + "-shm"} {
		if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
			t.Errorf("the DAG read left the sidecar %s (stat err %v)", sidecar, err)
		}
	}
	if _, err := os.Stat(s.record); !os.IsNotExist(err) {
		t.Errorf("the DAG read ran the relay CLI (stat err %v)", err)
	}
	bundle := improveTestReadBundle(t, out)
	if records := improveTestRecordsOf(bundle, improveKindDag); len(records) == 0 {
		t.Errorf("the DAG source contributed no record: %+v", bundle.Sources)
	}
}

// TestImproveInputRefusesAHardLinkedDagStore covers the same identity rule for the store the
// DAG source opens, with the relay source reading a store of its own. The destination is a hard
// link to the DAG source's store, outside both configured directories: neither the
// configured-path comparison nor the directory prefix sees it, so only the file identity
// refuses it.
func TestImproveInputRefusesAHardLinkedDagStore(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	dagDir := filepath.Join(s.root, "dag-state")
	if err := os.MkdirAll(dagDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dagStore := filepath.Join(dagDir, improveStoreFile)
	improveInputSeedPlanAt(t, dagStore, "p1", "a")
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"dag":   map[string]any{"path": dagDir, "pattern": "p1"},
		},
	}}})
	out := filepath.Join(s.root, "dag-store-link.sqlite3")
	if err := os.Link(dagStore, out); err != nil {
		t.Skipf("hard links are unavailable here: %v", err)
	}
	before := improveTestFileState(t, dagStore)
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if code != 1 || !strings.Contains(stderr, improveReasonOutputIsInput) {
		t.Fatalf("an output naming the DAG store: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonOutputIsInput)
	}
	if after := improveTestFileState(t, dagStore); after != before {
		t.Errorf("the DAG store changed: %s -> %s", before, after)
	}
}

// TestImproveInputStillWritesAnOrdinaryOutput is the control: the guard does not refuse a
// destination that is none of the files the collection opens.
func TestImproveInputStillWritesAnOrdinaryOutput(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a','manifest_forbidden','detail')")
	})
	improveInputRelayConfig(t, s)
	out := filepath.Join(s.root, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("collect: exit %d, stderr %s", code, stderr)
	}
	bundle := improveTestReadBundle(t, out)
	if bundle.Schema != improveBundleSchema {
		t.Errorf("the ordinary output was not written: %+v", bundle)
	}
}

// TestImproveInputKeepsTheConfiguredDirectoryRule is the control for the unchanged
// directory-level rule: a destination under a configured source directory is still refused,
// and nothing appears there.
func TestImproveInputKeepsTheConfiguredDirectoryRule(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	improveInputRelayConfig(t, s)
	out := filepath.Join(s.stateDir, "bundle.json")
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if code != 1 || !strings.Contains(stderr, "never overwrites") {
		t.Fatalf("an output inside the configured directory: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the refused run left a bundle in the source directory (stat err %v)", err)
	}
}

// TestImproveInputResolvesTheParentOfASourceSpelledThroughALink covers the input side of the
// issue's rule: every input's comparison uses the fully resolved path, and a path that does not
// exist yet is its resolved parent directory joined with its final name. The configured relay
// source is an absent store file spelled through a link to its directory, and the destination
// names the same absent file through that directory itself. Nothing else catches this pair: the
// configured path is a file, so no directory prefix applies, and the file is absent, so no
// (device, inode) comparison can be made.
func TestImproveInputResolvesTheParentOfASourceSpelledThroughALink(t *testing.T) {
	s := improveTestSetup(t)
	real := filepath.Join(s.root, "real-state")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.root, "state-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symbolic links are unavailable here: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, improveStoreFile)); !os.IsNotExist(err) {
		t.Fatalf("the fixture left a store behind (stat err %v)", err)
	}
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": filepath.Join(link, improveStoreFile)}},
	}}})
	out := filepath.Join(real, improveStoreFile)
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if code != 1 || !strings.Contains(stderr, improveReasonOutputIsInput) {
		t.Fatalf("an output naming the source through its directory: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonOutputIsInput)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the refused run left a file at the source's name (stat err %v)", err)
	}
}

// TestImproveInputRefusesALinkedDraftTarget covers an input reached through a link inside a
// configured directory: a drafts source may be a directory the collector enumerates, and it
// reads each entry it selects, following a link. The destination names the file such an entry
// points at, which is outside the configured directory, so the directory prefix does not cover
// it and the configured paths do not name it.
func TestImproveInputRefusesALinkedDraftTarget(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	drafts := filepath.Join(s.root, "drafts")
	archive := filepath.Join(s.root, "archive")
	for _, dir := range []string{drafts, archive} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(archive, "one.json")
	improveTestWrite(t, target, "{\"issue\":\"CRW-1\",\"title\":\"drafted\"}\n")
	if err := os.Symlink(target, filepath.Join(drafts, "one.json")); err != nil {
		t.Skipf("symbolic links are unavailable here: %v", err)
	}
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"draft": map[string]any{"path": drafts},
		},
	}}})
	code, _, stderr := improveTestRun(t, s, "--out", target)
	if code != 1 || !strings.Contains(stderr, improveReasonOutputIsInput) {
		t.Fatalf("an output naming the draft a directory entry links to: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonOutputIsInput)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the refused run removed the draft: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("the refused run replaced the draft its own evidence was read from: %q -> %q", before, after)
	}
}

// TestImproveInputRefusesAStoreSidecar covers the other files a store read depends on. With a
// write-ahead log beside the database, the read examines it and reads its committed frames, so
// it is an input too, and replacing it with the bundle leaves JSON where SQLite expects its
// coordination data. The relay source names the store file itself here, because a source that
// names the state directory is already covered by the directory rule.
func TestImproveInputRefusesAStoreSidecar(t *testing.T) {
	s := improveTestSetup(t)
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {})
	for _, sidecar := range []string{s.dbPath + "-wal", s.dbPath + "-shm"} {
		if err := os.WriteFile(sidecar, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"relay": map[string]any{"path": s.dbPath}},
	}}})
	for _, sidecar := range []string{s.dbPath + "-wal", s.dbPath + "-shm"} {
		code, _, stderr := improveTestRun(t, s, "--out", sidecar)
		if code != 1 || !strings.Contains(stderr, improveReasonOutputIsInput) {
			t.Fatalf("an output naming the store sidecar %s: exit %d, stderr %q, want the named refusal %s", sidecar, code, stderr, improveReasonOutputIsInput)
		}
		info, err := os.Stat(sidecar)
		if err != nil {
			t.Fatalf("the refused run removed the sidecar: %v", err)
		}
		if info.Size() != 0 {
			t.Errorf("the refused run wrote %d bytes over the sidecar %s", info.Size(), sidecar)
		}
	}
}
