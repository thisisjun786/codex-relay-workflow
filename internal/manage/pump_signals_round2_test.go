package manage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The second verification round of CRW-692: an uncommitted alert record is reused only by a retry
// of the same judgement, a capacity commit is refused against any other generation of the state
// file, and a busy capacity lock fails the commit instead of holding up the round.

// An alert whose commit failed is not merged into a later, separate alert of the same plan: the
// plan leaving the settings and coming back, or the state file going away, is a new generation and
// takes a new id. A plain retry of the failed commit still converges on the first id
// (TestPumpSignalCapacityCrashBetweenPendingAndCommit).
func TestPumpSignalCapacityUncommittedAlertIsNotReusedByALaterGeneration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset func(t *testing.T, f *pumpSignalFixture, cf *capacityFixture)
	}{
		{"removed and registered again", func(t *testing.T, f *pumpSignalFixture, cf *capacityFixture) {
			pumpSignalTestReplan(t, f, cf, []map[string]any{})
			f.round()
			f.advance(31 * time.Minute)
			pumpSignalTestReplan(t, f, cf, capacityTestPlans)
		}},
		{"state file lost", func(t *testing.T, f *pumpSignalFixture, cf *capacityFixture) {
			if err := os.Remove(filepath.Join(cf.stateDir, capacityStateFile)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, cf, crash := pumpSignalRealCapacity(t)
			*crash = true
			st := f.round()
			if len(st.Pending) != 1 {
				t.Fatalf("pending = %+v, want the first alert", st.Pending)
			}
			first := st.Pending[0].ID
			f.deliverAll()
			*crash = false
			f.advance(31 * time.Minute)
			tc.reset(t, f, cf)
			f.round() // the persistence clock of the new generation starts here
			f.advance(31 * time.Minute)
			st = f.round()
			if len(st.Pending) != 1 || st.Pending[0].ID == first {
				t.Fatalf("pending = %v, want the new generation's alert under an id besides %s", pumpSignalIDs(st.Pending), first)
			}
			if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; len(got.Alerted) != 2 {
				t.Fatalf("stored = %+v, want the new alert committed", got)
			}
		})
	}
}

// A capacity state file that was lost and written again from nothing reaches the version an older
// evaluation read; that evaluation's commit is still refused and the newer state stays.
func TestCapacityCommitIsRefusedAcrossAStateFileGeneration(t *testing.T) {
	f := capacityTestReady(t, capacityTestPlans)
	statePath := filepath.Join(f.stateDir, capacityStateFile)
	capacityTestRun(t, f, false) // the state file's first write
	held, err := capacityEvaluate(context.Background(), f.env, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	capacityTestRun(t, f, false) // the replacement's first write
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Commit(); err == nil {
		t.Fatal("the commit of an evaluation of the lost state succeeded over its replacement")
	}
	if after, err := os.ReadFile(statePath); err != nil || string(after) != string(before) {
		t.Fatalf("the refused commit changed the state: %v\nbefore %s\nafter  %s", err, before, after)
	}
}

// A state file that cannot be decoded refuses the commit of an evaluation that read another state,
// and is replaced by the commit of one that read it as it stands.
func TestCapacityCommitOverAnUnreadableState(t *testing.T) {
	f := capacityTestReady(t, capacityTestPlans)
	statePath := filepath.Join(f.stateDir, capacityStateFile)
	held, err := capacityEvaluate(context.Background(), f.env, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := held.Commit(); err == nil {
		t.Fatal("the commit of an evaluation of another state replaced the unreadable state")
	}
	if got, _ := os.ReadFile(statePath); string(got) != "{not json" {
		t.Fatalf("state = %q, want the unreadable file untouched", got)
	}
	capacityTestRun(t, f, false)
	if got, _ := capacityLoadState(statePath); got.Version != 1 || len(got.Plans["p-crw-129"].Waiting) != 2 {
		t.Fatalf("stored = %+v, want a run that read the unreadable state to replace it", got)
	}
}

// pumpSignalTestHoldCapacityLock takes the capacity state's lock from another descriptor, as another
// process would, and returns its release.
func pumpSignalTestHoldCapacityLock(t *testing.T, stateDir string) func() {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(stateDir, capacityStateFile)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			lock.Close()
		}
	}
	t.Cleanup(release)
	return release
}

// A capacity lock another process holds fails the commit within a bound: the round goes on to the
// other sources' events and the rest of the round, the alert stays pending, and the next round,
// with the lock free, converges on the same id and commits. A state without a lineage cannot be
// given one behind the lock, so its alert is judged without a usable state (round 10): it is
// pending under the pump's own id, and the next round, with the lock free, takes that id over and
// commits.
func TestPumpSignalBusyCapacityLockDoesNotHoldUpTheRound(t *testing.T) {
	for _, lineage := range []bool{true, false} {
		name := "state_with_a_lineage"
		if !lineage {
			name = "state_without_a_lineage"
		}
		t.Run(name, func(t *testing.T) {
			f, cf, _ := pumpSignalRealCapacity(t)
			if lineage {
				if err := capacityStateEstablishLineage(filepath.Join(cf.stateDir, capacityStateFile)); err != nil {
					t.Fatal(err)
				}
			}
			pumpTestSwapSources(t, pumpSignalCapacitySource{}, pumpSignalAuditSource{})
			pumpSignalTestWriteAlerts(t, f, pumpSignalTestAlertA+"\n")
			release := pumpSignalTestHoldCapacityLock(t, cf.stateDir)

			done := make(chan error, 1)
			go func() {
				_, err := pumpRound(context.Background(), f.e, f.cfg, pumpSettingsFrom(f.cfg), false)
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("round: %v", err)
				}
			case <-time.After(60 * time.Second):
				// A round that waited on the lock would wait until it is released, i.e. for good;
				// the bound only has to outlast a round on a loaded host (one took 30 s at load 40).
				release()
				<-done
				t.Fatal("the round waited on the busy capacity lock")
			}
			st := pumpTestReadState(t, f.cfg)
			if len(st.Pending) != 2 || st.Sources[pumpSignalCapacityName] != pumpSourceOK {
				t.Fatalf("pending = %v sources = %v, want the capacity alert and the audit line", pumpSignalIDs(st.Pending), st.Sources)
			}
			first := st.Pending[0].ID
			if !strings.HasPrefix(first, pumpSignalCapacityName) {
				t.Fatalf("pending = %v, want the capacity alert first", pumpSignalIDs(st.Pending))
			}
			if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; len(got.Alerted) != 0 {
				t.Fatalf("stored = %+v, want nothing committed behind the busy lock", got)
			}
			want := "commit"
			if !lineage {
				want = "lineage"
			}
			if logged, err := os.ReadFile(filepath.Join(f.cfg.StateDir, pumpLogFile)); err != nil || !strings.Contains(string(logged), want) {
				t.Fatalf("pump.log = %q, %v; want the failed write logged", logged, err)
			}

			release()
			f.advance(31 * time.Minute)
			st = f.round()
			if len(st.Pending) != 2 || st.Pending[0].ID != first {
				t.Fatalf("pending = %v, want the same capacity id %s after the lock was freed", pumpSignalIDs(st.Pending), first)
			}
			if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; len(got.Alerted) != 2 {
				t.Fatalf("stored = %+v, want the alert committed once the lock is free", got)
			}
		})
	}
}
