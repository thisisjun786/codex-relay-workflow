package evidence

import (
	"errors"
	"os"
	"testing"
	"time"
)

// A record is removed only by the call that published it, and only while it is still that call's open record (CRW-1121, CRW-1124:
// a delayed delivery of an event, or a call's earlier registration, must never take away a record another delivery's child claimed).

// assignOwnershipClaimed writes over a's record the record a child that claimed it leaves, and returns its bytes.
func assignOwnershipClaimed(t *testing.T, r *assignTestRig, a Assignment) []byte {
	t.Helper()
	claimed := a
	claimed.Status, claimed.AgentID = AssignmentClaimed, "worker"
	assignMust(t, writeRecord(r.recordPath(a.ID), claimed))
	data, err := os.ReadFile(r.recordPath(a.ID))
	assignMust(t, err)
	return data
}

// A delivery that fails before it publishes (its temporary file cannot be synced) leaves the record another delivery wrote and its
// child claimed as it is.
func TestPersistNewFailureBeforePublicationKeepsAnExistingRecord(t *testing.T) {
	r := newAssignTestRig(t)
	a, err := NewAssignment(assignTestSession, r.tree, AssignTree, time.Now())
	assignMust(t, err)
	assignMust(t, a.PersistNew(r.cwd))
	before := assignOwnershipClaimed(t, r, a)
	old := syncFile
	t.Cleanup(func() { syncFile = old })
	syncFile = func(*os.File) error { return errors.New("injected file sync failure before publication") }
	if err := a.PersistNew(r.cwd); err == nil || errors.Is(err, ErrAssignmentExists) {
		t.Fatalf("PersistNew with a failing file sync = %v", err)
	}
	after, err := os.ReadFile(r.recordPath(a.ID))
	if err != nil || string(after) != string(before) {
		t.Fatalf("the claimed record was removed or changed: %v\nbefore=%s\nafter=%s", err, before, after)
	}
}

// A delivery that published and then failed (the directory sync) takes its own open record back, but not a record a child claimed
// in between, and never another delivery's record.
func TestPersistNewFailureAfterPublicationRemovesOnlyItsOwnOpenRecord(t *testing.T) {
	for _, claimedMeanwhile := range []bool{false, true} {
		r := newAssignTestRig(t)
		a, err := NewAssignment(assignTestSession, r.tree, AssignTree, time.Now())
		assignMust(t, err)
		var claimed []byte
		old := syncDirectory
		syncDirectory = func(dir string) error {
			syncDirectory = old // the claim below writes a record of its own
			if claimedMeanwhile {
				claimed = assignOwnershipClaimed(t, r, a)
			}
			return errors.New("injected directory sync failure after publication")
		}
		err = a.PersistNew(r.cwd)
		syncDirectory = old
		if err == nil {
			t.Fatal("PersistNew with a failing directory sync succeeded")
		}
		after, readErr := os.ReadFile(r.recordPath(a.ID))
		switch {
		case !claimedMeanwhile && !errors.Is(readErr, os.ErrNotExist):
			t.Fatalf("the open record this call published stays after its failure: %v %s", readErr, after)
		case claimedMeanwhile && (readErr != nil || string(after) != string(claimed)):
			t.Fatalf("a record claimed after the publication was removed or changed: %v\nclaimed=%s\nafter=%s", readErr, claimed, after)
		}
	}
}

// Remove takes back an open record nobody claimed; a record a child claimed after the caller read it stays.
func TestAssignmentRemoveKeepsAClaimedRecord(t *testing.T) {
	r := newAssignTestRig(t)
	a, err := NewAssignment(assignTestSession, r.tree, AssignTree, time.Now())
	assignMust(t, err)
	assignMust(t, a.PersistNew(r.cwd))
	before := assignOwnershipClaimed(t, r, a)
	a.Remove(r.cwd)
	if after, err := os.ReadFile(r.recordPath(a.ID)); err != nil || string(after) != string(before) {
		t.Fatalf("Remove took a claimed record: %v\nbefore=%s\nafter=%s", err, before, after)
	}
	b, err := NewAssignment(assignTestSession, r.tree, AssignTree, time.Now())
	assignMust(t, err)
	assignMust(t, b.PersistNew(r.cwd))
	b.Remove(r.cwd)
	if _, err := os.Lstat(r.recordPath(b.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove left the open record: %v", err)
	}
}
