package manage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The pump's signal sources are driven through injected readings, the injected clock and temporary
// state; no test opens a relay store of the host or reaches the network.

// pumpSignalTestCapacityState reads the capacity state file of a fixture.
func pumpSignalTestCapacityState(t *testing.T, f *capacityFixture) capacityState {
	t.Helper()
	return capacityReadState(filepath.Join(f.stateDir, capacityStateFile))
}

// The evaluation is pure: the alert it reports and the alerted state it would write are returned,
// and the state file is left as it was until Commit.
func TestPumpSignalCapacityEvaluateWritesNothingUntilCommit(t *testing.T) {
	f := capacityTestReady(t, capacityTestPlans)
	statePath := filepath.Join(f.stateDir, capacityStateFile)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := capacityEvaluate(context.Background(), f.env, f.cfg)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(ev.Report.Plans) != 1 || !ev.Report.Plans[0].Alert || ev.Report.Plans[0].AlertEpoch != 1 {
		t.Fatalf("plans = %+v, want one alerting plan of epoch 1", ev.Report.Plans)
	}
	after, err := os.ReadFile(statePath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("the evaluation changed the state file: %v\nbefore %s\nafter  %s", err, before, after)
	}
	// Two evaluations of the same uncommitted state judge the same alert under the same epoch.
	again, err := capacityEvaluate(context.Background(), f.env, f.cfg)
	if err != nil || !again.Report.Plans[0].Alert || again.Report.Plans[0].AlertEpoch != 1 {
		t.Fatalf("second evaluation = %+v, %v; want the same alert of epoch 1", again.Report.Plans, err)
	}
	if err := ev.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	stored := pumpSignalTestCapacityState(t, f).Plans["p-crw-129"]
	if len(stored.Alerted) != 2 || stored.AlertedAt == 0 || stored.AlertEpoch != 1 {
		t.Fatalf("stored = %+v, want the alerted set and epoch 1", stored)
	}
	third, err := capacityEvaluate(context.Background(), f.env, f.cfg)
	if err != nil || third.Report.Plans[0].Alert {
		t.Fatalf("after the commit the same set alerts again: %+v, %v", third.Report.Plans, err)
	}
}

// A realert_hours re-alert of the same waiting set is a new epoch.
func TestPumpSignalCapacityRealertIsANewEpoch(t *testing.T) {
	f := capacityTestReady(t, capacityTestPlans)
	ev, err := capacityEvaluate(context.Background(), f.env, f.cfg)
	if err != nil || ev.Commit() != nil {
		t.Fatalf("first alert: %v", err)
	}
	later := capacityTestNow.Add(4 * time.Hour)
	f.env.Now = func() time.Time { return later }
	// The set has waited the whole time: keep the persistence clock as the first run left it.
	again, err := capacityEvaluate(context.Background(), f.env, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	plan := again.Report.Plans[0]
	if !plan.Alert || plan.AlertEpoch != 2 {
		t.Fatalf("plan = alert %v epoch %d, want a re-alert of epoch 2", plan.Alert, plan.AlertEpoch)
	}
}

// Capacity itself keeps its contract: it writes the state unless dry.
func TestPumpSignalCapacityStillWritesUnlessDry(t *testing.T) {
	f := capacityTestReady(t, capacityTestPlans)
	capacityTestRun(t, f, true)
	if got := pumpSignalTestCapacityState(t, f).Plans["p-crw-129"]; len(got.Alerted) != 0 {
		t.Fatalf("a dry run wrote the alerted state: %+v", got)
	}
	capacityTestRun(t, f, false)
	if got := pumpSignalTestCapacityState(t, f).Plans["p-crw-129"]; len(got.Alerted) != 2 || got.AlertEpoch != 1 {
		t.Fatalf("a real run left %+v, want the alerted set at epoch 1", got)
	}
}
