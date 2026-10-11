package manage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The seventh round of CRW-692: a plan's lifecycle in the capacity state is a random name drawn
// once when the plan first appears, never one derived from a stamp of the state file, and the pump
// keeps the name it proposed for a plan the state does not hold yet until a commit stores it; the
// capacity state write is durable when it returns; and the DAG review's relay doctor call cannot
// hold the pump round.

// pumpSignalTestLoseCommitMarker makes the next pump commit land while the pump's save after it is
// lost: the returned function puts the pump state back to what the pending save wrote, as a process
// that ended right after the commit leaves it.
func pumpSignalTestLoseCommitMarker(t *testing.T, f *pumpSignalFixture) func() {
	t.Helper()
	inner := pumpSignalCapacity
	t.Cleanup(func() { pumpSignalCapacity = inner })
	statePath := filepath.Join(f.cfg.StateDir, pumpStateFile)
	var saved []byte
	armed := true
	pumpSignalCapacity = func(ctx context.Context, e *Env, cfg *Config, dry bool) (CapacityReport, func() error, error) {
		report, commit, err := inner(ctx, e, cfg, dry)
		if commit == nil || !armed {
			return report, commit, err
		}
		armed = false
		return report, func() error {
			data, readErr := os.ReadFile(statePath)
			if readErr != nil {
				t.Fatalf("the pending save: %v", readErr)
			}
			if err := commit(); err != nil {
				t.Fatalf("setup: the commit failed: %v", err)
			}
			saved = data
			return nil
		}, err
	}
	return func() {
		t.Helper()
		if saved == nil {
			t.Fatal("setup: no commit was made")
		}
		if err := os.WriteFile(statePath, saved, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The verifier's first reproduction on 4d7b5e111: with the default persistence the plan's entry is
// made on an absent state file, the alert's commit lands but the pump's record of it is lost, the
// alert is delivered after a restart, and the state file is then lost once. The plan's entry made
// on the recreated file is another lifecycle, so the alert it reaches after its persistence is a
// new id rather than the one already sent.
func TestPumpSignalCapacityLostStateAfterALostCommitMarkerAlertsUnderANewId(t *testing.T) {
	f, cf, _ := pumpSignalRealCapacity(t)
	statePath := filepath.Join(cf.stateDir, capacityStateFile)
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if st := f.round(); len(st.Pending) != 0 {
		t.Fatalf("pending = %v on the plan's first reading, want nothing", pumpSignalIDs(st.Pending))
	}
	f.advance(31 * time.Minute)
	restore := pumpSignalTestLoseCommitMarker(t, f)
	st := f.round()
	if len(st.Pending) != 1 {
		t.Fatalf("pending = %v, want the alert", pumpSignalIDs(st.Pending))
	}
	first := st.Pending[0].ID
	restore()
	if rec, ok := pumpSignalCapacityLoad(pumpTestReadStatePtr(t, f.cfg), "p-crw-129"); !ok || rec.Committed {
		t.Fatalf("setup: record = %+v, want the uncommitted record of the pending save", rec)
	}
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; got.AlertEpoch != 1 {
		t.Fatalf("setup: stored = %+v, want the alert committed", got)
	}
	f.advance(16 * time.Minute)
	f.round() // the restart; the capacity read is not due
	f.deliverAll()
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	f.advance(15 * time.Minute)
	if st = f.round(); len(st.Pending) != 0 {
		t.Fatalf("pending = %v when the persistence clock restarts, want nothing", pumpSignalIDs(st.Pending))
	}
	f.advance(31 * time.Minute)
	st = f.round()
	if len(st.Pending) != 1 || st.Pending[0].ID == first {
		t.Fatalf("pending = %v after the state file was lost, want a new id besides %s", pumpSignalIDs(st.Pending), first)
	}
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; got.AlertEpoch != 1 || len(got.Alerted) != 2 {
		t.Fatalf("stored = %+v, want the new alert committed", got)
	}
}

// The verifier's second reproduction on 4d7b5e111: a plan the state does not hold yet alerts on
// its first reading (no persistence window), and a command-line run that judged before the plan was
// registered commits its unchanged state before the pump commits. The pump's refused commit is
// retried under the lifecycle its pending record proposed, so the next read converges on the first
// id instead of adding the same unreflected alert under another one.
func TestPumpSignalCapacityNewPlanKeepsItsAlertIdAcrossANoopCommandLineCommit(t *testing.T) {
	f, cf, _ := pumpSignalRealCapacity(t)
	capacityTestSection(t, cf, "persist_minutes", 0)
	if err := os.WriteFile(filepath.Join(cf.stateDir, capacityStateFile), []byte(`{"plans":{}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The state carries a lineage, as every state the pump has read once does (round 9): the
	// pump's read then writes nothing before its evaluation, and the command line's judgement made
	// before it can still be committed in between.
	if err := capacityStateEstablishLineage(filepath.Join(cf.stateDir, capacityStateFile)); err != nil {
		t.Fatal(err)
	}
	pumpSignalTestReplan(t, f, cf, []map[string]any{})
	held, err := capacityEvaluate(context.Background(), cf.env, cf.cfg)
	if err != nil {
		t.Fatal(err)
	}
	pumpSignalTestReplan(t, f, cf, capacityTestPlans)
	inner := pumpSignalCapacity
	t.Cleanup(func() { pumpSignalCapacity = inner })
	var pumpCommit error
	interleaved := false
	pumpSignalCapacity = func(ctx context.Context, e *Env, cfg *Config, dry bool) (CapacityReport, func() error, error) {
		report, commit, err := inner(ctx, e, cfg, dry)
		if commit == nil || interleaved {
			return report, commit, err
		}
		interleaved = true
		return report, func() error {
			before := pumpSignalTestCapacityState(t, cf).Plans
			if err := held.Commit(); err != nil {
				t.Fatalf("the command line's commit: %v", err)
			}
			if after := pumpSignalTestCapacityState(t, cf).Plans; !capacityTestSamePlans(before, after) || len(after) != 0 {
				t.Fatalf("setup: the command line's commit changed the plans:\nbefore %+v\nafter  %+v", before, after)
			}
			pumpCommit = commit()
			return pumpCommit
		}, err
	}
	st := f.round()
	if len(st.Pending) != 1 {
		t.Fatalf("pending = %v, want the alert", pumpSignalIDs(st.Pending))
	}
	first := st.Pending[0].ID
	if !errors.Is(pumpCommit, errCapacityStateMoved) {
		t.Fatalf("setup: the pump's commit = %v, want it refused after the command line saved", pumpCommit)
	}
	f.advance(31 * time.Minute)
	st = f.round()
	pumpSignalWant(t, "after the command line's save", st.Pending, first)
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; len(got.Alerted) != 2 || got.AlertEpoch != 1 {
		t.Fatalf("stored = %+v, want the alert committed at epoch 1", got)
	}
}

// capacityTestPowerModel records the capacity state write's file operations in order, so a test
// can say what a power loss right after the write keeps: a renamed file's content survives only
// when the file was synced after its last write and before the rename, and its directory after the
// rename. failSync makes every file sync fail.
type capacityTestPowerModel struct {
	ops      []string
	failSync bool
}

type capacityTestPowerFile struct {
	capacityTempFile
	m *capacityTestPowerModel
}

func (f capacityTestPowerFile) Write(p []byte) (int, error) {
	f.m.ops = append(f.m.ops, "write "+f.Name())
	return f.capacityTempFile.Write(p)
}

func (f capacityTestPowerFile) Sync() error {
	if f.m.failSync {
		return errors.New("injected sync failure")
	}
	f.m.ops = append(f.m.ops, "sync "+f.Name())
	return f.capacityTempFile.Sync()
}

func capacityTestPowerLoss(t *testing.T) *capacityTestPowerModel {
	t.Helper()
	m := &capacityTestPowerModel{}
	saved := capacityStateIO
	t.Cleanup(func() { capacityStateIO = saved })
	capacityStateIO.createTemp = func(dir, pattern string) (capacityTempFile, error) {
		file, err := saved.createTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return capacityTestPowerFile{capacityTempFile: file, m: m}, nil
	}
	capacityStateIO.rename = func(from, to string) error {
		m.ops = append(m.ops, "rename "+from+" "+to)
		return saved.rename(from, to)
	}
	capacityStateIO.syncDir = func(dir string) error {
		m.ops = append(m.ops, "syncdir "+dir)
		return saved.syncDir(dir)
	}
	return m
}

// durable reports whether a power loss now keeps what the last rename put at path.
func (m *capacityTestPowerModel) durable(path string) bool {
	renamed, temp := -1, ""
	for i, op := range m.ops {
		if fields := strings.Fields(op); len(fields) == 3 && fields[0] == "rename" && fields[2] == path {
			renamed, temp = i, fields[1]
		}
	}
	if renamed < 0 {
		return false
	}
	synced, dirSynced := false, false
	for _, op := range m.ops[:renamed] {
		switch op {
		case "write " + temp:
			synced = false
		case "sync " + temp:
			synced = true
		}
	}
	for _, op := range m.ops[renamed+1:] {
		if op == "syncdir "+filepath.Dir(path) {
			dirSynced = true
		}
	}
	return synced && dirSynced
}

// The verifier's power-loss candidate on 4d7b5e111: a committed capacity state must survive a
// power loss right after the commit returns, as the pump state does; otherwise the pump's durable
// record of the commit sits beside a rolled-back alerted state and the same alert is judged again
// under a higher epoch.
func TestCapacityCommitIsDurableWhenItReturns(t *testing.T) {
	cf := capacityTestReady(t, capacityTestPlans)
	ev, err := capacityEvaluate(context.Background(), cf.env, cf.cfg)
	if err != nil {
		t.Fatal(err)
	}
	model := capacityTestPowerLoss(t)
	if err := ev.Commit(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(cf.stateDir, capacityStateFile)
	if !model.durable(statePath) {
		t.Fatalf("a power loss right after the commit loses it: operations %q", model.ops)
	}
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; got.AlertEpoch != 1 {
		t.Fatalf("stored = %+v, want the alert committed", got)
	}
}

// A state write whose sync fails is an error and leaves the previous state and no temporary file.
func TestCapacityStateWriteWithAFailedSyncKeepsThePreviousState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, capacityStateFile)
	previous := []byte(`{"plans":{},"version":3,"revision":"r3"}` + "\n")
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatal(err)
	}
	model := capacityTestPowerLoss(t)
	model.failSync = true
	err := capacityWriteState(path, capacityState{Plans: map[string]capacityPlanState{"p": {Since: 1}}, Version: 4, Revision: "r4"})
	if err == nil {
		t.Fatalf("the write succeeded without its sync: operations %q", model.ops)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != string(previous) {
		t.Fatalf("state = %q, %v; want the previous state", got, readErr)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 {
		t.Fatalf("directory = %v, want only the state file", entries)
	}
}

// The verifier's DAG candidate on 4d7b5e111: a relay doctor that never answers (here one whose
// child keeps the output open) cannot hold the pump round. The review is unmeasured, nothing of
// the DAG source moves, and the audit line read in the same round is saved.
func TestPumpSignalDagHangingDoctorDoesNotHoldUpTheRound(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalDagSource{}, pumpSignalAuditSource{})
	quiet := pumpSignalDagReview
	pumpSignalDagReview = pumpSignalDagReviewRead
	t.Cleanup(func() { pumpSignalDagReview = quiet })
	helper := filepath.Join(t.TempDir(), "crw")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nsleep 60 &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.e.Executable = helper
	f.cfg.Relay.State = ""
	f.cfg.Relay.Socket = filepath.Join(t.TempDir(), "relay.sock")
	pumpSignalTestWriteAlerts(t, f, pumpSignalTestAlertA+"\n")
	savedBudget := pumpSignalDagDoctorBudget
	pumpSignalDagDoctorBudget = 200 * time.Millisecond
	t.Cleanup(func() { pumpSignalDagDoctorBudget = savedBudget })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := pumpRound(ctx, f.e, f.cfg, pumpSettingsFrom(f.cfg), false)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("round: %v", err)
		}
	// The round takes about a second (the 200 ms budget and the helper's one-second wait delay); the
	// margin up to the doctor's minute is for a loaded host, whose fsyncs alone can take seconds.
	case <-time.After(30 * time.Second):
		cancel()
		<-done
		t.Fatalf("the round was held %v by a doctor that never answers", time.Since(start))
	}
	st := pumpTestReadState(t, f.cfg)
	if len(st.Pending) != 1 || !strings.HasPrefix(st.Pending[0].ID, pumpSignalAuditName) {
		t.Fatalf("pending = %v, want the audit line saved", pumpSignalIDs(st.Pending))
	}
	if st.Sources[pumpSignalDagName] != pumpSourceUnmeasured {
		t.Fatalf("sources = %v, want the DAG review unmeasured", st.Sources)
	}
	if _, ok := st.Cursors[pumpSignalDagReadKey]; ok {
		t.Fatalf("a review that never ran was recorded as made: %v", st.Cursors)
	}
}
