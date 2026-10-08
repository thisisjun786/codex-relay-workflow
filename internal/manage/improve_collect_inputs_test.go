package manage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"golang.org/x/sys/unix"
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

// improveTestTemporaryNames lists the temporary files a run may have left in a directory.
func improveTestTemporaryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "improve-bundle-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// improveForceUnnamed makes the unnamed temporary file's creation answer err and counts the calls.
func improveForceUnnamed(t *testing.T, err error) *int {
	t.Helper()
	calls := new(int)
	previous := improveUnnamedCreate
	improveUnnamedCreate = func(dirfd int) (int, error) {
		*calls++
		return -1, err
	}
	t.Cleanup(func() { improveUnnamedCreate = previous })
	return calls
}

// TestImproveUnnamedTemporaryUnsupportedFallsBackToNamed covers C5: a platform or filesystem whose
// unnamed temporary file creation answers an errno meaning "unsupported" still writes the bundle,
// as a named temporary file, and leaves no temporary file behind.
func TestImproveUnnamedTemporaryUnsupportedFallsBackToNamed(t *testing.T) {
	for _, errno := range []unix.Errno{unix.EOPNOTSUPP, unix.EINVAL, unix.EISDIR, unix.ENOSYS} {
		t.Run(errno.Error(), func(t *testing.T) {
			s := improveTestSetup(t)
			improveReview799Store(t, s)
			improveInputRelayConfig(t, s)
			out := filepath.Join(improveReview799OutDir(t, s), "bundle.json")
			calls := improveForceUnnamed(t, errno)
			code, _, stderr := improveTestRun(t, s, "--out", out)
			if *calls == 0 {
				t.Fatal("the run never tried the unnamed temporary file")
			}
			if code != 0 {
				t.Fatalf("a run on a filesystem without unnamed files: exit %d, stderr %q, want the bundle", code, stderr)
			}
			if bundle := improveTestReadBundle(t, out); bundle.Schema != improveBundleSchema {
				t.Errorf("the fallback did not write the bundle: %+v", bundle)
			}
			if left := improveTestTemporaryNames(t, filepath.Dir(out)); len(left) != 0 {
				t.Errorf("the run left temporary files %v", left)
			}
		})
	}
}

// TestImproveUnnamedTemporaryRealErrorStaysARefusal covers the other side of C5: an error that
// would also stop a named temporary file, here ENOSPC, is still a refusal and writes nothing.
func TestImproveUnnamedTemporaryRealErrorStaysARefusal(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	improveInputRelayConfig(t, s)
	outDir := improveReview799OutDir(t, s)
	out := filepath.Join(outDir, "bundle.json")
	improveForceUnnamed(t, unix.ENOSPC)
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if code == 0 {
		t.Fatalf("a run whose temporary file cannot be created for lack of space succeeded: stderr %q", stderr)
	}
	if _, err := os.Stat(out); err == nil {
		t.Errorf("the refused run wrote %s", out)
	}
	if left := improveTestTemporaryNames(t, outDir); len(left) != 0 {
		t.Errorf("the refused run left temporary files %v", left)
	}
}

// TestImproveNamedTemporaryLeavesNothingOnRefusal covers C6 on the named path: a refusal after the
// named temporary file exists removes that file through its descriptor.
func TestImproveNamedTemporaryLeavesNothingOnRefusal(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	ledger := filepath.Join(s.root, "audit.jsonl")
	improveTestWrite(t, ledger, "{\"schema\":\"crw-audit/1\",\"issue\":\"CRW-1\",\"grade\":\"A\"}\n")
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"audit": map[string]any{"path": ledger},
		},
	}}})
	improveForceUnnamed(t, unix.EOPNOTSUPP)
	outDir := improveReview799OutDir(t, s)
	replaced := false
	previous := improveInputBeforeRename
	improveInputBeforeRename = func(improveOutputPlan) {
		if replaced {
			return
		}
		replaced = true
		if err := os.Remove(ledger); err != nil {
			t.Errorf("removing the ledger: %v", err)
			return
		}
		improveTestWrite(t, ledger, "{\"schema\":\"crw-audit/1\",\"issue\":\"CRW-2\",\"grade\":\"B\"}\n")
	}
	t.Cleanup(func() { improveInputBeforeRename = previous })
	code, _, stderr := improveTestRun(t, s, "--out", filepath.Join(outDir, "bundle.json"))
	if !replaced {
		t.Fatalf("the rename seam never ran: exit %d, stderr %q", code, stderr)
	}
	if code != 1 || !strings.Contains(stderr, improveReasonInputChanged) {
		t.Fatalf("a named-file run whose input changed: exit %d, stderr %q, want %s", code, stderr, improveReasonInputChanged)
	}
	if left := improveTestTemporaryNames(t, outDir); len(left) != 0 {
		t.Errorf("the refused run left temporary files %v", left)
	}
}

// TestImproveNamedTemporaryRemovalFailureNamesTheFile covers the report half of C6: when the
// removal of a named temporary file fails, the refusal names that file and the reason it stayed.
func TestImproveNamedTemporaryRemovalFailureNamesTheFile(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	ledger := filepath.Join(s.root, "audit.jsonl")
	improveTestWrite(t, ledger, "{\"schema\":\"crw-audit/1\",\"issue\":\"CRW-1\",\"grade\":\"A\"}\n")
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"audit": map[string]any{"path": ledger},
		},
	}}})
	improveForceUnnamed(t, unix.EOPNOTSUPP)
	outDir := improveReview799OutDir(t, s)
	previousRename := improveInputBeforeRename
	improveInputBeforeRename = func(improveOutputPlan) {
		_ = os.Remove(ledger)
		improveTestWrite(t, ledger, "{\"schema\":\"crw-audit/1\",\"issue\":\"CRW-2\",\"grade\":\"B\"}\n")
	}
	t.Cleanup(func() { improveInputBeforeRename = previousRename })
	previousUnlink := improveUnlinkTemporary
	improveUnlinkTemporary = func(int, string) error { return unix.EACCES }
	t.Cleanup(func() { improveUnlinkTemporary = previousUnlink })
	code, _, stderr := improveTestRun(t, s, "--out", filepath.Join(outDir, "bundle.json"))
	left := improveTestTemporaryNames(t, outDir)
	for _, name := range left {
		_ = os.Remove(filepath.Join(outDir, name))
	}
	if code != 1 || !strings.Contains(stderr, improveReasonInputChanged) {
		t.Fatalf("the refusal: exit %d, stderr %q, want %s", code, stderr, improveReasonInputChanged)
	}
	if len(left) != 1 || !strings.Contains(stderr, left[0]) {
		t.Errorf("the left file %v is not named in the refusal %q", left, stderr)
	}
}

// TestImproveAuditReadUsesThePinnedFile covers C7: the audit ledger is read from the descriptor the
// collection pinned, so a ledger swapped during the read and restored before the post-read check
// contributes the content that was pinned, not the swapped content.
func TestImproveAuditReadUsesThePinnedFile(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	ledger := filepath.Join(s.root, "audit.jsonl")
	keep := ledger + ".keep"
	improveTestWrite(t, ledger, "{\"schema\":\"crw-audit/1\",\"issue\":\"CRW-1\",\"grade\":\"A\"}\n")
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"audit": map[string]any{"path": ledger},
		},
	}}})
	moved := false
	previousRead := improveIdentityReadHook
	improveIdentityReadHook = func(path string) {
		if path != ledger || moved {
			return
		}
		moved = true
		if err := os.Link(ledger, keep); err != nil {
			t.Errorf("keeping the original ledger: %v", err)
			return
		}
		improveTestWrite(t, ledger+".new", "{\"schema\":\"crw-audit/1\",\"issue\":\"CRW-2\",\"grade\":\"B\"}\n")
		if err := os.Rename(ledger+".new", ledger); err != nil {
			t.Errorf("swapping the ledger: %v", err)
		}
	}
	t.Cleanup(func() { improveIdentityReadHook = previousRead })
	restored := false
	previousAfter := improveInputAfterRead
	improveInputAfterRead = func() {
		if !moved || restored {
			return
		}
		restored = true
		if err := os.Rename(keep, ledger); err != nil {
			t.Errorf("restoring the original ledger: %v", err)
		}
	}
	t.Cleanup(func() { improveInputAfterRead = previousAfter })
	out := filepath.Join(improveReview799OutDir(t, s), "bundle.json")
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if !moved {
		t.Fatalf("the read seam never ran on the ledger: exit %d, stderr %q", code, stderr)
	}
	if code != 0 {
		t.Fatalf("a ledger swapped back before the post-read check: exit %d, stderr %q, want the bundle", code, stderr)
	}
	records := improveTestRecordsOf(improveTestReadBundle(t, out), improveKindAudit)
	if len(records) != 1 || records[0].Key != "CRW-1" {
		t.Errorf("the bundle holds the swapped ledger, not the one the collection pinned: %+v", records)
	}
}

// improveLowerDescriptorLimit lowers the soft descriptor limit for one test and restores it after.
func improveLowerDescriptorLimit(t *testing.T, cur uint64) {
	t.Helper()
	var previous unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &previous); err != nil {
		t.Skipf("the descriptor limit is unreadable here: %v", err)
	}
	if previous.Cur <= cur {
		t.Skipf("the descriptor limit %d is already at or below %d", previous.Cur, cur)
	}
	lowered := unix.Rlimit{Cur: cur, Max: previous.Max}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &lowered); err != nil {
		t.Skipf("the descriptor limit cannot be lowered here: %v", err)
	}
	t.Cleanup(func() { _ = unix.Setrlimit(unix.RLIMIT_NOFILE, &previous) })
}

// improveTestManyDrafts writes n drafts into a drafts directory and configures it as the source.
func improveTestManyDrafts(t *testing.T, s *improveTestState, n int) string {
	t.Helper()
	drafts := filepath.Join(s.root, "drafts")
	if err := os.MkdirAll(drafts, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("d-%03d", i)
		improveTestWrite(t, filepath.Join(drafts, name+".json"), fmt.Sprintf("{\"schema\":\"crw-issue-draft/1\",\"fingerprint\":%q,\"project\":\"p\",\"title\":\"t\"}", name))
	}
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"draft": map[string]any{"path": drafts},
		},
	}}})
	return drafts
}

// TestImproveDraftsOverDescriptorLimitStillCollect covers C8: a drafts directory with more entries
// than the descriptor limit is read in full without a refusal.
func TestImproveDraftsOverDescriptorLimitStillCollect(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	improveLowerDescriptorLimit(t, 64)
	improveTestManyDrafts(t, s, 100)
	out := filepath.Join(improveReview799OutDir(t, s), "bundle.json")
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if code != 0 {
		t.Fatalf("a drafts directory over the descriptor limit: exit %d, stderr %q, want the bundle", code, stderr)
	}
	if got := improveTestRecordsOf(improveTestReadBundle(t, out), improveKindDraft); len(got) != 100 {
		t.Errorf("draft records = %d, want 100", len(got))
	}
}

// TestImproveDraftsOverDescriptorLimitRefusesAMovedEntry covers the refusal half of C8: an entry
// past the descriptor limit, which the collection holds by identity rather than by descriptor, is
// refused when it is moved onto the output's place.
func TestImproveDraftsOverDescriptorLimitRefusesAMovedEntry(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	improveLowerDescriptorLimit(t, 64)
	drafts := improveTestManyDrafts(t, s, 100)
	out := filepath.Join(improveReview799OutDir(t, s), "bundle.json")
	moved := false
	previous := improveInputBeforeRename
	improveInputBeforeRename = func(improveOutputPlan) {
		if moved {
			return
		}
		moved = true
		if err := os.Rename(filepath.Join(drafts, "d-099.json"), out); err != nil {
			t.Errorf("moving the draft: %v", err)
		}
	}
	t.Cleanup(func() { improveInputBeforeRename = previous })
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if !moved {
		t.Fatalf("the rename seam never ran: exit %d, stderr %q", code, stderr)
	}
	if code != 1 || !strings.Contains(stderr, improveReasonOutputIsInput) {
		t.Fatalf("an output moved over an entry past the limit: exit %d, stderr %q, want %s", code, stderr, improveReasonOutputIsInput)
	}
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
