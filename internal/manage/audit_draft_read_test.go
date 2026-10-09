package manage

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CRW-1050 tests pin how the draft readers protect what they read. A SQLite input is read by name
// (the decision below), so its protection is the identity comparison before and after the read. A draft
// is a plain file, so its reader reads from the descriptor it opened and then checks that the name still
// reaches that file.

// auditDraftReadTestDraft writes one valid draft below the world's drafts directory and returns its path.
func auditDraftReadTestDraft(t *testing.T, w *improveProposeTestWorld, fingerprint, title string) string {
	t.Helper()
	path := filepath.Join(w.stateDir, "drafts", fingerprint+".json")
	doc := &auditDraft{
		Schema: auditDraftSchema, Fingerprint: fingerprint, Source: improveProposeSource,
		Title: title, Severity: "P2", Body: "## What\n\n" + title + "\n", Labels: []string{"improve", "P2"},
		Seen:  []auditDraftSeen{{Mode: improveProposeSource, Subject: "s", Head: "events:" + fingerprint, At: "t"}},
		State: auditDraftStateDraft,
	}
	if err := auditDraftSave(path, doc); err != nil {
		t.Fatal(err)
	}
	return path
}

// auditDraftReadTestSwapOnce makes the next read of path see a different draft: a draft with another title
// is renamed over the path while the reader is inside its read. The original stays reachable at keep.
func auditDraftReadTestSwapOnce(t *testing.T, w *improveProposeTestWorld, path, title string) (swapped *bool, substitute string) {
	t.Helper()
	fingerprint := strings.TrimSuffix(filepath.Base(path), ".json")
	other := auditDraftReadTestDraft(t, w, "zz"+fingerprint, title)
	staged := filepath.Join(filepath.Dir(path), fingerprint+".json.swap")
	data, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	// The substitute carries the fingerprint of the path it replaces, so it is a draft the reader would accept.
	data = []byte(strings.ReplaceAll(string(data), "zz"+fingerprint, fingerprint))
	if err := os.WriteFile(staged, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	done := false
	previous := auditDraftReadHook
	auditDraftReadHook = func(p string) {
		if p != path || done {
			return
		}
		done = true
		if err := os.Rename(staged, path); err != nil {
			t.Errorf("swapping the draft: %v", err)
		}
	}
	t.Cleanup(func() { auditDraftReadHook = previous })
	return &done, staged
}

// TestAuditDraftLoadRefusesADraftSwappedDuringTheRead is the identity comparison for a draft: the reader
// reads from the file it opened, and a name that reaches another file once the read is done refuses the
// read by name instead of returning either file's content as the draft's.
func TestAuditDraftLoadRefusesADraftSwappedDuringTheRead(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	path := auditDraftReadTestDraft(t, w, "aaaa", "original")
	swapped, _ := auditDraftReadTestSwapOnce(t, w, path, "substitute")
	doc, err := auditDraftLoad(path)
	if !*swapped {
		t.Fatal("the read seam never ran")
	}
	if err == nil || !strings.Contains(err.Error(), auditDraftReasonChanged) {
		t.Fatalf("a draft swapped during the read: doc %+v err %v, want the refusal %s", doc, err, auditDraftReasonChanged)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refusal reads as an absent draft: %v", err)
	}
}

// TestAuditDraftLoadRefusesADraftRemovedDuringTheRead keeps a draft that vanished mid-read from reading as
// a draft that never existed: the propose command creates a draft for an absent one.
func TestAuditDraftLoadRefusesADraftRemovedDuringTheRead(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	path := auditDraftReadTestDraft(t, w, "aaaa", "original")
	removed := false
	previous := auditDraftReadHook
	auditDraftReadHook = func(p string) {
		if p == path && !removed {
			removed = true
			if err := os.Remove(path); err != nil {
				t.Errorf("removing the draft: %v", err)
			}
		}
	}
	t.Cleanup(func() { auditDraftReadHook = previous })
	_, err := auditDraftLoad(path)
	if !removed {
		t.Fatal("the read seam never ran")
	}
	if err == nil || !strings.Contains(err.Error(), auditDraftReasonChanged) || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a draft removed during the read: err %v, want %s that is not an absent draft", err, auditDraftReasonChanged)
	}
}

// TestAuditDraftLoadStillReadsAnOrdinaryDraftAndNamesAMissingOne keeps what the reader did before: a draft
// that stays put is read, and a draft that is not there is an absent file the callers can tell apart.
func TestAuditDraftLoadStillReadsAnOrdinaryDraftAndNamesAMissingOne(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	path := auditDraftReadTestDraft(t, w, "aaaa", "original")
	doc, err := auditDraftLoad(path)
	if err != nil || doc.Title != "original" {
		t.Fatalf("an ordinary draft: doc %+v err %v", doc, err)
	}
	if _, err := auditDraftLoad(filepath.Join(filepath.Dir(path), "bbbb.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing draft: err %v, want an absent file", err)
	}
}

// TestAuditListDraftsReadsEachDraftOnceUnderTheIdentityCheck covers the listing: it used to read a draft
// twice, once for its schema and once to load it, so a draft swapped between the two was judged by one file
// and listed from another. It now reads once, and a swap during that read is refused.
func TestAuditListDraftsReadsEachDraftOnceUnderTheIdentityCheck(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	path := auditDraftReadTestDraft(t, w, "aaaa", "original")
	e := improveProposeTestEnv(w, &strings.Builder{}, &strings.Builder{})
	listed, err := auditListDrafts(e, coreDefaults(e))
	if err != nil || len(listed) != 1 {
		t.Fatalf("the listing: %+v err %v, want the one draft", listed, err)
	}
	swapped, _ := auditDraftReadTestSwapOnce(t, w, path, "substitute")
	_, err = auditListDrafts(e, coreDefaults(e))
	if !*swapped {
		t.Fatal("the read seam never ran during the listing")
	}
	if err == nil || !strings.Contains(err.Error(), auditDraftReasonChanged) {
		t.Errorf("a draft swapped during the listing: err %v, want %s", err, auditDraftReasonChanged)
	}
}

// TestImproveProposeLeavesADraftSwappedDuringTheReadAlone covers the writers: propose reads a draft before
// it rewrites it, and a draft that is another file by the time the read is done is not merged into and
// overwritten; the run stops with the named refusal and the file stays as the other writer left it.
func TestImproveProposeLeavesADraftSwappedDuringTheReadAlone(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	bundle := improveProposeTestBundle(t, w, []improveRecord{improveProposeTestSplit("project-a", "size overrun", "rel-a")})
	first := improveReview988Propose(t, w, bundle)
	path := improveReview988DraftPath(w, first.Created[0].Fingerprint)
	swapped, staged := auditDraftReadTestSwapOnce(t, w, path, "substitute")
	substitute, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	grown := improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestSplit("project-a", "size overrun", "rel-a"),
		improveProposeTestSplit("project-b", "size overrun", "rel-b"),
	})
	code, _, stderr := improveProposeTestRun(t, w, "--bundle", grown)
	if !*swapped {
		t.Fatalf("the read seam never ran: exit %d, stderr %s", code, stderr)
	}
	if code != 1 || !strings.Contains(stderr, auditDraftReasonChanged) {
		t.Errorf("propose over a draft swapped during the read: exit %d, stderr %s, want the refusal %s", code, stderr, auditDraftReasonChanged)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(substitute) {
		t.Errorf("the swapped-in draft was rewritten (err %v)", err)
	}
}

// TestImproveDagStoreReplacedRightAfterItsReadIsRefused covers the SQLite input the decision keeps on the
// identity comparison: the DAG source's store, read by name through store.OpenInPlace, is replaced right
// after its reader returns, and the collection refuses by name instead of writing a bundle of rows from a
// file the path no longer reaches. The relay source reads a store of its own, so this is the DAG store's
// own recorded identity.
func TestImproveDagStoreReplacedRightAfterItsReadIsRefused(t *testing.T) {
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
	calls := 0
	previous := improveInputAfterRead
	improveInputAfterRead = func() {
		calls++
		// The relay reader returns first and the DAG reader second.
		if calls != 2 {
			return
		}
		// The same bytes under a new inode: the path still reads the same rows, but it is not the file
		// the collection read.
		data, err := os.ReadFile(dagStore)
		if err != nil {
			t.Errorf("reading the DAG store: %v", err)
			return
		}
		if err := os.Rename(dagStore, dagStore+".moved"); err != nil {
			t.Errorf("moving the DAG store: %v", err)
			return
		}
		if err := os.WriteFile(dagStore, data, 0o600); err != nil {
			t.Errorf("copying the DAG store: %v", err)
		}
	}
	t.Cleanup(func() { improveInputAfterRead = previous })
	out := filepath.Join(improveReview799OutDir(t, s), "bundle.json")
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if calls < 2 {
		t.Fatalf("the DAG reader never returned: exit %d, stderr %q", code, stderr)
	}
	if code != 1 || !strings.Contains(stderr, improveReasonInputChanged) {
		t.Fatalf("the DAG store replaced right after its read: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonInputChanged)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the refused run wrote a bundle (stat err %v)", err)
	}
}
