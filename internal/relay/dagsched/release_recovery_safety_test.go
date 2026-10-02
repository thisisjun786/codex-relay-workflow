package dagsched

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests of the safety properties of CRW-282: what the cleanup may and may not remove, how its failures end, and what the recovery does when the world moves under it.

// c1: a failure of the intent transaction itself (here the store fails after the reserve) removes the copy the call froze.
func TestFailedIntentTransactionRemovesTheFrozenCopy(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	k.sched.testAfterReserve = func() error { return errors.New("the store failed") }
	before := k.rows()
	if _, err := k.release("rp", "B"); err == nil {
		t.Fatal("the release was expected to fail")
	}
	if got := k.copies(); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
	if removed := k.journal("dag_manifest_copy_removed"); len(removed) != 1 || removed[0]["why"] != "error" {
		t.Fatalf("journal = %v", removed)
	}
	if k.rows() != before || k.heldSlots() != 0 {
		t.Fatalf("a failed release left rows %+v -> %+v, held %d", before, k.rows(), k.heldSlots())
	}
}

// c2: a close names its request and answers for that request: the retry of a first close does not close the newer abandoned intent of the same manifest, and closing the newer one needs its own request id.
func TestCloseNamingTheRequestAnswersForThatRequest(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	digest, first := k.abandon("rp", "A")
	firstClose := k.mustClose("rp", "A", digest)
	_, second := k.abandon("rp", "A")
	late, err := k.sched.CloseRelease(context.Background(), "rp", "A", "parent", digest, "the retry of the first close", first)
	if err != nil || !late.Replayed || late.RequestID != first || late.SuccessorRequestID != second || late.SlotID != firstClose.SlotID {
		t.Fatalf("late retry = %+v %v, want the first closure again, naming %s as its successor", late, err, second)
	}
	if open, found, err := latestRelease(context.Background(), k.s.Q(context.Background()), "rp", "A"); err != nil || !found || open.Request != second || k.recoveries("closed") != 1 {
		t.Fatalf("the retry closed something: open %+v %v %v, closed rows %d", open, found, err, k.recoveries("closed"))
	}
	if _, err := k.sched.CloseRelease(context.Background(), "rp", "A", "parent", digest, "a request that never was", "dag-never"); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("an unknown request = %v", err)
	}
	now, err := k.sched.CloseRelease(context.Background(), "rp", "A", "parent", digest, "the successor is abandoned too", second)
	if err != nil || now.Replayed || now.RequestID != second || k.recoveries("closed") != 2 {
		t.Fatalf("close naming the successor = %+v %v, want the abandoned successor closed", now, err)
	}
}

// c2: a successor whose start did not finish is continued from its own frozen request (its own id and bytes), and a frozen request that changed is refused.
func TestSuccessorStartContinuesFromItsOwnFrozenRequest(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	digest, abandoned := k.abandon("rp", "A")
	k.mustClose("rp", "A", digest)
	k.host.loseFirstCreation = true
	res, err := k.release("rp", "A")
	if err != nil || res.Bound || res.RequestID != RecoveryRequestID("rp", "A", digest, abandoned) {
		t.Fatalf("first call = %v %+v", err, res)
	}
	var raw string
	if err := k.s.DB.QueryRow("SELECT request_json FROM dag_release_recoveries WHERE action = 'rereleased'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	frozen := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &frozen); err != nil || frozen["requestId"] != res.RequestID {
		t.Fatalf("the successor froze a request that carries id %v, want %s (%v)", frozen["requestId"], res.RequestID, err)
	}
	again := k.mustRelease("rp", "A")
	if !again.Bound || !again.Replayed || again.RequestID != res.RequestID || again.ChildTaskID == "" {
		t.Fatalf("repeat = %+v", again)
	}
	if k.children() != 1 || k.rows().executions != 1 {
		t.Fatalf("children %d, executions %d", k.children(), k.rows().executions)
	}
}

func TestSuccessorReplayRefusesAChangedFrozenRequest(t *testing.T) {
	cases := map[string]struct{ change, want string }{
		"the frozen bytes":     {"UPDATE dag_release_recoveries SET request_json = request_json || ' ' WHERE action = 'rereleased'", "revision_mismatch"},
		"the frozen selectors": {"UPDATE dag_release_recoveries SET socket = 'elsewhere' WHERE action = 'rereleased'", "disposition_conflict"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			digest, _ := k.abandon("rp", "A")
			k.mustClose("rp", "A", digest)
			k.host.loseFirstCreation = true
			if res, err := k.release("rp", "A"); err != nil || res.Bound {
				t.Fatalf("first call = %v %+v", err, res)
			}
			created := k.children()
			k.exec(c.change)
			if _, err := k.release("rp", "A"); refusalReason(err) != c.want {
				t.Fatalf("replay = %v, want %s", err, c.want)
			}
			if k.children() != created {
				t.Fatal("a changed frozen request started a child")
			}
		})
	}
}

// c2: a foreign child that takes the issue after the successor intent was recorded makes the engine refuse the start; the intent waits with its slot (D-15) and the same release continues once the issue is free. No child is
// ever created while the foreign one lives.
func TestReopenedReleasePreemptedByAForeignChildWaits(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	digest, _ := k.abandon("rp", "A")
	k.mustClose("rp", "A", digest)
	real := k.sched.Start
	k.sched.Start = func(ctx context.Context, raw []byte) (StartAnswer, error) {
		k.foreignRelationship("CRW-A")
		return real(ctx, raw)
	}
	res, err := k.release("rp", "A")
	if refusalReason(err) != "duplicate_assignment" || res.Bound {
		t.Fatalf("release = %v %+v, want the engine's duplicate_assignment and no child", err, res)
	}
	if k.count("SELECT COUNT(*) FROM managed_start_requests WHERE request_id = ?", res.RequestID) != 0 {
		t.Fatal("the refused start wrote a managed row")
	}
	if k.children() != 0 || k.heldSlots() != 1 || k.recoveries("rereleased") != 1 {
		t.Fatalf("children %d, held %d, rereleased %d", k.children(), k.heldSlots(), k.recoveries("rereleased"))
	}
	if n := k.read("rp").node("A"); n.Reason != SkipAlreadyOwned {
		t.Fatalf("the waiting intent reads %+v", n)
	}
	k.sched.Start = real
	k.exec("UPDATE relationships SET status = 'archived' WHERE relationship_id = 'rel-foreign-CRW-A'")
	bound := k.mustRelease("rp", "A")
	if !bound.Bound || k.children() != 1 || k.heldSlots() != 1 || k.recoveries("rereleased") != 1 {
		t.Fatalf("after the foreign child ended: %+v, children %d, held %d, rereleased %d", bound, k.children(), k.heldSlots(), k.recoveries("rereleased"))
	}
}

// c2: a store the new table has not reached (a read-only open installs nothing) still reads: the node is abandoned, as before.
func TestReadingAStoreThatPredatesTheRecoveryTable(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.abandon("rp", "A")
	k.exec("DROP INDEX dag_release_recoveries_successor")
	k.exec("DROP TABLE dag_release_recoveries")
	if n := k.read("rp").node("A"); n.State != StateReleasing || n.Reason != BlockedReleaseAbandoned {
		t.Fatalf("the reading of a store without the table says %+v", n)
	}
}

// c1: while a live child owns the issue, a stored manifest that holds the copy's bytes may be what a prepared correction rests on, so close keeps the copy.
func TestCloseKeepsTheCopyWhileALiveChildOwnsTheIssue(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	digest, _ := k.abandon("rp", "B")
	copies := k.copies()
	k.foreignRelationship("CRW-B")
	res := k.mustClose("rp", "B", digest)
	if res.Copy == nil || res.Copy.Removed || len(k.copies()) != 1 || k.copies()[0] != copies[0] {
		t.Fatalf("close = %+v, copies %v", res, k.copies())
	}
	if kept := k.journal("dag_manifest_copy_kept"); len(kept) != 1 || kept[0]["why"] != "relied_on" {
		t.Fatalf("journal = %v", kept)
	}
}

// c1: the removal is made by descriptor. A directory swapped for a link to somewhere else between the verification and the unlink cannot lead it out: the file outside is untouched, and the unlink lands on the
// directory that was read, so the copy that was verified is the one that goes.
func TestCloseRemovalNeverLeavesTheArtifactRoot(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	digest, _ := k.abandon("rp", "B")
	copyPath := k.copies()[0]
	raw, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	victim := filepath.Join(outside, filepath.Base(copyPath))
	if err := os.WriteFile(victim, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(copyPath)
	testBeforeUnlink = func() error {
		testBeforeUnlink = nil
		if err := os.Rename(dir, dir+".moved"); err != nil {
			return err
		}
		return os.Symlink(outside, dir)
	}
	t.Cleanup(func() { testBeforeUnlink = nil })
	res := k.mustClose("rp", "B", digest)
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("the removal left the artifact root: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir+".moved", filepath.Base(copyPath))); !os.IsNotExist(err) {
		t.Fatalf("the copy that was verified was not the one removed: %v", err)
	}
	if res.Copy == nil || len(k.journal("dag_manifest_copy_removed")) != 1 {
		t.Fatalf("close = %+v, journal %v", res, k.journal("dag_manifest_copy_removed"))
	}
}

// c1: a failure after the unlink and before the commit loses the journal row of an unreferenced file, nothing else: the closure was committed first, so it stands and a repeat answers it.
func TestCloseSurvivesAFailureAfterTheUnlink(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	digest, _ := k.abandon("rp", "B")
	testAfterUnlink = func() error {
		testAfterUnlink = nil
		return errors.New("the commit failed")
	}
	t.Cleanup(func() { testAfterUnlink = nil })
	res := k.mustClose("rp", "B", digest)
	if k.recoveries("closed") != 1 || k.heldSlots() != 0 || len(k.copies()) != 0 || res.Copy == nil || !res.Copy.Removed {
		t.Fatalf("close = %+v, closed rows %d, held %d, copies %v", res, k.recoveries("closed"), k.heldSlots(), k.copies())
	}
	if len(k.journal("dag_manifest_copy_removed")) != 0 {
		t.Fatal("the journal row of a transaction that did not commit is there")
	}
	again := k.mustClose("rp", "B", digest)
	if !again.Replayed || again.Copy == nil || !again.Copy.Removed || again.RequestID != res.RequestID {
		t.Fatalf("repeat = %+v", again)
	}
}

// c1: a removal that did not happen leaves the file and says so; a repeat of the close takes it.
func TestCloseRepeatFinishesARemovalThatDidNotHappen(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	digest, _ := k.abandon("rp", "B")
	testBeforeUnlink = func() error {
		testBeforeUnlink = nil
		return errors.New("the unlink was refused")
	}
	t.Cleanup(func() { testBeforeUnlink = nil })
	res := k.mustClose("rp", "B", digest)
	if res.Copy == nil || res.Copy.Removed || len(k.copies()) != 1 {
		t.Fatalf("close = %+v, copies %v", res, k.copies())
	}
	if kept := k.journal("dag_manifest_copy_kept"); len(kept) != 1 || kept[0]["why"] != "remove_failed" {
		t.Fatalf("journal = %v", kept)
	}
	again := k.mustClose("rp", "B", digest)
	if !again.Replayed || again.Copy == nil || !again.Copy.Removed || len(k.copies()) != 0 || len(k.journal("dag_manifest_copy_removed")) != 1 {
		t.Fatalf("repeat = %+v, copies %v", again, k.copies())
	}
	if !strings.HasSuffix(again.Copy.Path, ".json") {
		t.Fatalf("path = %s", again.Copy.Path)
	}
}

// c1: an artifact root that has become a link (here to a directory that holds a file of the same name) is refused: the removal acquires the root as the freeze does, with no link in its path, and touches nothing.
func TestCloseRemovalRefusesALinkedArtifactRoot(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	digest, _ := k.abandon("rp", "B")
	copyPath := k.copies()[0]
	raw, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "dag-input-manifests"), 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "dag-input-manifests", filepath.Base(copyPath))
	if err := os.WriteFile(victim, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(k.root, k.root+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, k.root); err != nil {
		t.Fatal(err)
	}
	res := k.mustClose("rp", "B", digest)
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("the removal followed the link out of the artifact root: %v", err)
	}
	if res.Copy == nil || res.Copy.Removed {
		t.Fatalf("close = %+v", res)
	}
	if kept := k.journal("dag_manifest_copy_kept"); len(kept) != 1 || kept[0]["why"] != "link_in_path" {
		t.Fatalf("journal = %v", kept)
	}
}

// c1: a copy replaced by a FIFO is not a copy and must not hold the write transaction: the call returns and keeps it.
func TestCloseRemovalDoesNotBlockOnAFifo(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	digest, _ := k.abandon("rp", "B")
	copyPath := k.copies()[0]
	if err := os.Remove(copyPath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(copyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		res CloseResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := k.closeRelease("rp", "B", digest)
		done <- answer{res, err}
	}()
	select {
	case a := <-done:
		if a.err != nil || a.res.Copy == nil || a.res.Copy.Removed {
			t.Fatalf("close = %+v %v", a.res, a.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the removal blocked on a FIFO")
	}
	if info, err := os.Lstat(copyPath); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the FIFO was touched: %v", err)
	}
	if kept := k.journal("dag_manifest_copy_kept"); len(kept) != 1 || kept[0]["why"] != "not_a_copy" {
		t.Fatalf("journal = %v", kept)
	}
}

// abandonWith is abandon for a release request the test supplies.
func (k *releaseKit) abandonWith(plan, node string, req ReleaseRequest) (digest, request string) {
	k.t.Helper()
	real := k.sched.Start
	k.sched.Start = func(context.Context, []byte) (StartAnswer, error) { return StartAnswer{}, context.DeadlineExceeded }
	_, err := k.sched.Release(context.Background(), plan, node, "parent", req)
	k.sched.Start = real
	if err == nil {
		k.t.Fatal("the start was expected to fail")
	}
	row, found, lerr := latestRelease(context.Background(), k.s.Q(context.Background()), plan, node)
	if lerr != nil || !found {
		k.t.Fatalf("no open intent after the failed start: %v %v", found, lerr)
	}
	n, _ := nodeOf(k.snapshot(plan), node)
	k.managedRow(row.Request, n.IssueKey, "released", "")
	return row.Digest, row.Request
}

// c1: the path of a copy is read from the line the relay wrote at the head of the prompt, never from the operator's instructions: a manifest that travelled inline names no copy, whatever the instructions say.
func TestCloseNeverDeletesAFileOnlyTheInstructionsName(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	canonical := []byte("an unrelated file that looks like a copy of a manifest")
	path, _, err := freezeManifestCopy(k.root, canonical)
	if err != nil {
		t.Fatal(err)
	}
	req := k.request(false)
	req.Instructions = "Read the notes stored at " + path + " (sha256 " + shaOf(canonical) + ") before you start."
	digest, _ := k.abandonWith("rp", "A", req)
	res := k.mustClose("rp", "A", digest)
	if res.Copy != nil {
		t.Fatalf("close = %+v: an inline manifest has no copy to take back", res)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a file only the instructions named was removed: %v", err)
	}
}

// c1: at close the file must be the closed intent's own manifest: a copy-shaped file of another manifest that a (tampered) frozen request names is kept.
func TestCloseKeepsAFileThatIsNotTheClosedManifest(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	digest, _ := k.abandon("rp", "B")
	other := []byte("{\"manifest_digest\":\"" + dig("another manifest") + "\"}")
	path, _, err := freezeManifestCopy(k.root, other)
	if err != nil {
		t.Fatal(err)
	}
	prompt := "Assignment CRW-B: node B of DAG plan rp, released under input manifest " + digest + " (stored at " + path + " (sha256 " + shaOf(other) + ")).\n\nx"
	raw, err := json.Marshal(map[string]any{"prompt": prompt, "artifactRoots": []string{k.root}})
	if err != nil {
		t.Fatal(err)
	}
	k.exec("UPDATE dag_release_requests SET request_json = ? WHERE plan_id = 'rp' AND node_id = 'B'", string(raw))
	res := k.mustClose("rp", "B", digest)
	if res.Copy == nil || res.Copy.Removed || res.Copy.Path != path {
		t.Fatalf("close = %+v", res)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a file that is not the closed manifest was removed: %v", err)
	}
	if kept := k.journal("dag_manifest_copy_kept"); len(kept) != 1 || kept[0]["why"] != "not_a_copy" {
		t.Fatalf("journal = %v", kept)
	}
}

// c1: a copy this call only reused, taken back by its creator and frozen again inside the intent transaction, is this call's: when the transaction then fails, no intent names it and it goes.
func TestFailedIntentAfterARefreezeRemovesTheCopyItRecreated(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	k.sched.Now = func() string { return "2026-10-02T01:00:00.000000+00:00" }
	k.holdSlots(6)
	var saved []byte
	var savedPath string
	testBeforeUnlink = func() error {
		testBeforeUnlink = nil
		savedPath = k.copies()[0]
		var err error
		saved, err = os.ReadFile(savedPath)
		return err
	}
	t.Cleanup(func() { testBeforeUnlink = nil })
	first, err := k.release("rp", "B")
	if refusalReason(err) != "capacity_exhausted" || savedPath == "" {
		t.Fatalf("first attempt = %v %+v", err, first)
	}
	// the creator took the copy back; put it where the next attempt will find it, as a file that already exists
	if err := os.WriteFile(savedPath, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	k.exec("DELETE FROM execution_slots WHERE subject_key LIKE 'held-%'")
	// the store refuses the intent after the copy was frozen again: a request row of the same key that no release row accompanies
	k.exec("INSERT INTO dag_release_requests (plan_id, node_id, manifest_digest, request_sha256, request_json, marker_root, socket, state_selector, recorded_at) VALUES ('rp', 'B', ?, 'x', '{}', 'm', 's', 'x', 't')", first.ManifestDigest)
	k.sched.testAfterReserve = func() error { return os.Remove(savedPath) }
	if _, err := k.release("rp", "B"); err == nil {
		t.Fatal("the release was expected to fail")
	}
	if got := k.copies(); len(got) != 0 {
		t.Fatalf("a copy that no intent names is left: %v", got)
	}
}
