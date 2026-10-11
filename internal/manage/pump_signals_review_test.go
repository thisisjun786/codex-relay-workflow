package manage

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The verification round of CRW-692: the identity of a capacity alert survives the plan leaving
// the settings and the state file going away, a late commit never overwrites a newer state, and a
// replaced audit queue file is told from the same file grown.

// pumpSignalTestReplan swaps the plans of the capacity fixture and keeps the pump fixture on the
// new configuration.
func pumpSignalTestReplan(t *testing.T, f *pumpSignalFixture, cf *capacityFixture, plans []map[string]any) {
	t.Helper()
	capacityTestSection(t, cf, "plans", plans)
	f.cfg = cf.cfg
	f.cfg.ManagementThread = ""
}

// A plan removed from the settings and registered again alerts under a new id, even when its
// waiting set is the one already sent.
func TestPumpSignalCapacityRemovedAndReaddedPlanAlertsAgain(t *testing.T) {
	f, cf, _ := pumpSignalRealCapacity(t)
	st := f.round()
	if len(st.Pending) != 1 {
		t.Fatalf("pending = %+v, want the first alert", st.Pending)
	}
	first := st.Pending[0].ID
	f.deliverAll()

	f.advance(31 * time.Minute)
	pumpSignalTestReplan(t, f, cf, []map[string]any{})
	f.round()
	f.advance(31 * time.Minute)
	pumpSignalTestReplan(t, f, cf, capacityTestPlans)
	f.round() // the persistence clock of the registered plan starts here
	f.advance(31 * time.Minute)
	st = f.round()
	if len(st.Pending) != 1 || st.Pending[0].ID == first {
		t.Fatalf("pending = %v after the plan was registered again, want a new id besides %s", pumpSignalIDs(st.Pending), first)
	}
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; len(got.Alerted) != 2 {
		t.Fatalf("stored = %+v, want the new alert committed", got)
	}
	// The new alert is the same one if the commit is lost and the judgement repeated.
	second := st.Pending[0].ID
	f.advance(31 * time.Minute)
	if st = f.round(); len(st.Pending) != 1 || st.Pending[0].ID != second {
		t.Fatalf("pending = %v, want only %s", pumpSignalIDs(st.Pending), second)
	}
}

// A capacity state file that is gone resets the epoch to 1; the pump's own record keeps the id of
// the next alert apart from the one already sent.
func TestPumpSignalCapacityLostStateFileAlertsUnderANewId(t *testing.T) {
	f, cf, _ := pumpSignalRealCapacity(t)
	first := f.round().Pending[0].ID
	f.deliverAll()

	f.advance(31 * time.Minute)
	if err := os.Remove(filepath.Join(cf.stateDir, capacityStateFile)); err != nil {
		t.Fatal(err)
	}
	f.round() // the persistence clock restarts with the lost state
	f.advance(31 * time.Minute)
	st := f.round()
	if len(st.Pending) != 1 || st.Pending[0].ID == first {
		t.Fatalf("pending = %v after the state file was lost, want a new id besides %s", pumpSignalIDs(st.Pending), first)
	}
}

// A commit that was made against an older state than the file holds is refused and writes nothing.
func TestPumpSignalCapacityStaleCommitKeepsTheNewerState(t *testing.T) {
	f := capacityTestReady(t, capacityTestPlans)
	held, err := capacityEvaluate(context.Background(), f.env, f.cfg)
	if err != nil || held.Report.Plans[0].AlertEpoch != 1 {
		t.Fatalf("held evaluation = %+v, %v; want epoch 1", held.Report.Plans, err)
	}
	capacityTestRun(t, f, false) // the command line saves epoch 1
	later := capacityTestNow.Add(4 * time.Hour)
	f.env.Now = func() time.Time { return later }
	if plan := capacityTestRun(t, f, false).Plans[0]; plan.AlertEpoch != 2 { // and then epoch 2
		t.Fatalf("epoch = %d, want a re-alert of epoch 2", plan.AlertEpoch)
	}
	if err := held.Commit(); err == nil {
		t.Fatal("the stale commit succeeded")
	}
	if got := pumpSignalTestCapacityState(t, f).Plans["p-crw-129"]; got.AlertEpoch != 2 {
		t.Fatalf("epoch = %d after the stale commit, want 2", got.AlertEpoch)
	}
}

// The same inside the pump: a commit delayed past the command line's own runs is refused and logged,
// the saved pending event stays, and the epoch only grows.
func TestPumpSignalCapacityDelayedCommitDoesNotRollBackTheCommandLine(t *testing.T) {
	f, cf, _ := pumpSignalRealCapacity(t)
	saved := pumpSignalCapacity
	pumpSignalCapacity = func(ctx context.Context, e *Env, cfg *Config, dry bool) (CapacityReport, func() error, error) {
		report, commit, err := pumpSignalCapacityRead(ctx, e, cfg, dry)
		if commit == nil {
			return report, commit, err
		}
		return report, func() error {
			capacityTestRun(t, cf, false)
			f.now = f.now.Add(4 * time.Hour)
			capacityTestRun(t, cf, false)
			return commit()
		}, err
	}
	t.Cleanup(func() { pumpSignalCapacity = saved })
	st := f.round()
	if len(st.Pending) != 1 {
		t.Fatalf("pending = %+v, want the alert saved before the commit", st.Pending)
	}
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; got.AlertEpoch != 2 {
		t.Fatalf("epoch = %d, want 2 as the command line left it", got.AlertEpoch)
	}
	if logged, err := os.ReadFile(filepath.Join(f.cfg.StateDir, "pump.log")); err != nil || !strings.Contains(string(logged), "commit") {
		t.Fatalf("pump.log = %q, %v; want the refused commit logged", logged, err)
	}
}

func pumpSignalTestReplaceAlerts(t *testing.T, f *pumpSignalFixture, content string) {
	t.Helper()
	path := pumpSignalTestAlertsPath(f)
	temp := path + ".new"
	if err := os.WriteFile(temp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, path); err != nil {
		t.Fatal(err)
	}
}

// A queue file replaced by another of the same size, or a larger one, is read from its start under a
// new epoch; the lines of the old file are not read again.
func TestPumpSignalAuditReplacedFileIsReadFromItsStart(t *testing.T) {
	for _, tc := range []struct {
		name, replacement string
	}{
		{"same size", strings.Replace(pumpSignalTestAlertA, "CRW-1", "CRW-2", 1) + "\n"},
		{"larger", strings.Replace(pumpSignalTestAlertA, "CRW-1", "CRW-2", 1) + "\n" + pumpSignalTestAlertA + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := pumpSignalTestFixture(t, pumpSignalAuditSource{})
			pumpSignalTestWriteAlerts(t, f, pumpSignalTestAlertA+"\n")
			st := f.round()
			pumpSignalWant(t, "first file", st.Pending, "signal_audit#0")
			f.deliverAll()
			pumpSignalTestReplaceAlerts(t, f, tc.replacement)
			st = f.round()
			if len(st.Pending) == 0 || st.Pending[0].ID != "signal_audit@1#0" || !strings.Contains(st.Pending[0].Text, "CRW-2") {
				t.Fatalf("pending = %+v, want the replacement's first line under epoch 1", st.Pending)
			}
		})
	}
}

// A state written before the file identity was kept adopts the file it finds and reads on.
func TestPumpSignalAuditStateWithoutIdentityKeepsItsOffset(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalAuditSource{})
	pumpSignalTestWriteAlerts(t, f, pumpSignalTestAlertA+"\n")
	f.round()
	f.deliverAll()
	st := pumpTestReadState(t, f.cfg)
	delete(st.Cursors, pumpSignalAuditFileKey)
	if err := st.pumpSave(f.cfg); err != nil {
		t.Fatal(err)
	}
	pumpSignalTestAppendAlerts(t, f, pumpSignalTestAlertA+"\n")
	st = f.round()
	if len(st.Pending) != 1 || st.Pending[0].ID != "signal_audit#"+strconv.Itoa(len(pumpSignalTestAlertA)+1) {
		t.Fatalf("pending = %v, want only the appended line", pumpSignalIDs(st.Pending))
	}
}
