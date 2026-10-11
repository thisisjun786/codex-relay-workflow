package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The ninth round of CRW-692: an alert whose capacity commit keeps failing keeps one id, also when
// the capacity state is missing or cannot be read; two different waiting sets are never taken for
// the same alert; a lineage-less capacity state lost and written again byte for byte is a new
// generation; and a command-line commit whose directory sync failed still reports its alert.

// pumpSignalTestCollect runs n rounds, each followed by a delivery and the next capacity period, and
// adds the id of every event a round made pending to ids.
func pumpSignalTestCollect(f *pumpSignalFixture, n int, ids map[string]bool) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		st := f.round()
		for _, ev := range st.Pending {
			ids[ev.ID] = true
		}
		f.deliverAll()
		f.advance(31 * time.Minute)
	}
}

// pumpSignalTestOneAlert checks that the rounds sent the plan's alert under one id and that the
// alert is committed.
func pumpSignalTestOneAlert(t *testing.T, cf *capacityFixture, ids map[string]bool, what string) {
	t.Helper()
	if len(ids) != 1 {
		t.Fatalf("%s: the same unchanged alert was sent under %d ids %v, want one", what, len(ids), ids)
	}
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; len(got.Alerted) != 2 || got.AlertEpoch != 1 {
		t.Fatalf("%s: stored = %+v, want the alert committed at epoch 1", what, got)
	}
}

// The verifier's P2 on 94613cc8d: with persist_minutes 0 and no capacity state file, a capacity
// commit that keeps failing (a writer that holds the lock, a lock that cannot be opened, a process
// that ends before every commit) made every read propose a fresh lifecycle, so the same unchanged
// alert was sent again under @1, @2, @3. It is sent under one id while the commit fails, and that
// id is the one the commit settles once it can be made.
func TestPumpSignalCapacityAbsentStateWhoseCommitKeepsFailingAlertsOnce(t *testing.T) {
	for _, c := range []struct {
		name string
		fail func(t *testing.T, path string, crash *bool) func()
	}{
		{"lock_held_by_a_stuck_writer", func(t *testing.T, path string, _ *bool) func() {
			lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			return func() { syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); lock.Close() }
		}},
		{"lock_that_cannot_be_opened", func(t *testing.T, path string, _ *bool) func() {
			if err := os.Mkdir(path+".lock", 0o755); err != nil {
				t.Fatal(err)
			}
			return func() {
				if err := os.Remove(path + ".lock"); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"process_ends_before_every_commit", func(t *testing.T, _ string, crash *bool) func() {
			*crash = true
			return func() { *crash = false }
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, cf, crash := pumpSignalRealCapacity(t)
			capacityTestSection(t, cf, "persist_minutes", 0)
			pumpSignalTestReplan(t, f, cf, capacityTestPlans)
			path := filepath.Join(cf.stateDir, capacityStateFile)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			repair := c.fail(t, path, crash)
			ids := map[string]bool{}
			pumpSignalTestCollect(f, 4, ids)
			if len(ids) != 1 {
				repair()
				t.Fatalf("while the commit fails: the unchanged alert was sent under %d ids %v, want one", len(ids), ids)
			}
			repair()
			pumpSignalTestCollect(f, 2, ids)
			pumpSignalTestOneAlert(t, cf, ids, "after the commit could be made")
		})
	}
}

// The same with a capacity state that cannot be read (a directory at its path, a file without read
// permission, as a run under another user leaves it). The judgement is made without the state and
// logged, its alert is sent under one id however often the state is read again, and once the state
// can be read that id is the one committed (round 10: the round-9 pump judged nothing until then).
func TestPumpSignalCapacityUnreadableStateAlertsOnce(t *testing.T) {
	for _, c := range []struct {
		name          string
		fail          func(t *testing.T, path string)
		recover       func(t *testing.T, path string)
		needsNonRoot  bool
		removeAtStart bool
	}{
		{"directory_at_the_state_path", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, false, true},
		{"file_without_read_permission", func(t *testing.T, path string) {
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.needsNonRoot && os.Geteuid() == 0 {
				t.Skip("root reads a file without read permission")
			}
			f, cf, _ := pumpSignalRealCapacity(t)
			capacityTestSection(t, cf, "persist_minutes", 0)
			pumpSignalTestReplan(t, f, cf, capacityTestPlans)
			path := filepath.Join(cf.stateDir, capacityStateFile)
			if c.removeAtStart {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			c.fail(t, path)
			t.Cleanup(func() { os.Chmod(path, 0o600) })
			ids := map[string]bool{}
			pumpSignalTestCollect(f, 4, ids)
			if len(ids) != 1 {
				t.Fatalf("while the state cannot be read: the unchanged alert was sent under %d ids %v, want one", len(ids), ids)
			}
			if st := pumpTestReadState(t, f.cfg); st.Sources[pumpSignalCapacityName] != pumpSourceOK {
				t.Fatalf("status = %q, want the judgement made without the state", st.Sources[pumpSignalCapacityName])
			}
			if logged, err := os.ReadFile(filepath.Join(f.cfg.StateDir, pumpLogFile)); err != nil || !strings.Contains(string(logged), "cannot be read") {
				t.Fatalf("pump.log = %q, %v; want the unreadable state logged", logged, err)
			}
			c.recover(t, path)
			pumpSignalTestCollect(f, 2, ids)
			pumpSignalTestOneAlert(t, cf, ids, "after the state could be read")
		})
	}
}

// The verifier's pre-existing loss: the waiting set was compared by the first eight hex digits of
// its digest, and these two sets share them. A changed set whose first alert's commit was lost was
// then taken for that alert and absorbed into the id already sent.
func TestPumpSignalCapacityWaitingSetsSharingADigestPrefixAreTwoAlerts(t *testing.T) {
	first, second := []string{"CRW-109982", "CRW-509982"}, []string{"CRW-117419", "CRW-517419"}
	if a, b := pumpSignalWaitingHash(first), pumpSignalWaitingHash(second); a == b {
		t.Errorf("two different waiting sets have the same digest %s", a)
	}
	f, cf, crash := pumpSignalRealCapacity(t)
	capacityTestSection(t, cf, "persist_minutes", 0)
	pumpSignalTestReplan(t, f, cf, capacityTestPlans)
	capacityTestRelay(t, cf, capacityTestReading(first, nil, 12, 12, "within"))
	*crash = true
	st := f.round()
	if len(st.Pending) != 1 {
		t.Fatalf("pending = %v, want the first set's alert", pumpSignalIDs(st.Pending))
	}
	firstID := st.Pending[0].ID
	f.deliverAll()
	*crash = false
	capacityTestRelay(t, cf, capacityTestReading(second, nil, 12, 12, "within"))
	f.advance(31 * time.Minute)
	st = f.round()
	if len(st.Pending) != 1 || st.Pending[0].ID == firstID || !strings.Contains(st.Pending[0].Text, "CRW-117419") {
		t.Fatalf("pending = %v, want the second set's alert under an id other than the delivered %s", pumpSignalIDs(st.Pending), firstID)
	}
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; !slices.Equal(got.Alerted, second) {
		t.Fatalf("stored = %+v, want the second set alerted", got)
	}
}

// The verifier's first P3 on 94613cc8d: a capacity state without a lineage was told only by its
// content (or the version and revision it was written with), so the same bytes lost and written
// again (a file written before files carried a lineage, an empty file, one written before files
// carried a lineage but with a revision) read as the generation the proposal was made on, and the
// alert judged on the recreated file was absorbed into the id already sent.
func TestPumpSignalCapacityStateWrittenAgainByteForByteEndsTheProposal(t *testing.T) {
	for _, c := range []struct {
		name  string
		bytes string
	}{
		{"without_a_revision", `{"plans":{}}` + "\n"},
		{"empty_file", ""},
		{"revision_without_a_lineage", `{"plans":{},"version":3,"revision":"abcd"}` + "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			write := func(t *testing.T, _ *pumpSignalFixture, cf *capacityFixture) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(cf.stateDir, capacityStateFile), []byte(c.bytes), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			f, cf, crash := pumpSignalTestNewPlan(t, write)
			*crash = true
			st := f.round()
			if len(st.Pending) != 1 {
				t.Fatalf("pending = %v, want the alert", pumpSignalIDs(st.Pending))
			}
			first := st.Pending[0].ID
			f.deliverAll()
			*crash = false
			if err := os.Remove(filepath.Join(cf.stateDir, capacityStateFile)); err != nil {
				t.Fatal(err)
			}
			write(t, f, cf)
			f.advance(31 * time.Minute)
			pumpSignalTestWantNewAlert(t, cf, f.round(), first, "after the state was lost and written again byte for byte")
		})
	}
}

// The verifier's second P3 on 94613cc8d: a command-line commit that renamed the alerted state into
// place but whose directory sync failed exited 3 without its report, while the state already
// recorded the alert, so the pump's next read found it alerted and the alert reached nobody until
// realert_hours. The report is printed with the sync failure as a warning; a write that was refused
// before its rename still prints no report and exits 3.
func TestCapacityCommandLineReportsAnAlertWhoseDirectorySyncFailed(t *testing.T) {
	f := capacityTestReady(t, capacityTestPlans)
	config := capacityConfig
	t.Cleanup(func() { capacityConfig = config })
	capacityConfig = func(*Env) *Config { return f.cfg }
	saved := capacityStateIO
	t.Cleanup(func() { capacityStateIO = saved })

	capacityStateIO.createTemp = func(string, string) (capacityTempFile, error) {
		return nil, errors.New("injected temporary file failure")
	}
	var out, errOut bytes.Buffer
	f.env.Stdout, f.env.Stderr = &out, &errOut
	if code := capacityRun(context.Background(), f.env, nil); code != capacityReadFailureExit || out.Len() != 0 {
		t.Fatalf("a refused write: exit %d stdout %q, want exit %d and no report", code, out.String(), capacityReadFailureExit)
	}

	capacityStateIO = saved
	capacityStateIO.syncDir = func(string) error { return errors.New("injected directory sync failure") }
	out.Reset()
	errOut.Reset()
	code := capacityRun(context.Background(), f.env, nil)
	var report CapacityReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || code != 0 {
		t.Fatalf("exit %d stdout %q stderr %q (%v), want exit 0 and the report", code, out.String(), errOut.String(), err)
	}
	if len(report.Plans) != 1 || !report.Plans[0].Alert {
		t.Fatalf("report = %+v, want the plan's alert", report.Plans)
	}
	if !strings.Contains(errOut.String(), "injected directory sync failure") {
		t.Fatalf("stderr = %q, want the sync failure named", errOut.String())
	}
	if got := pumpSignalTestCapacityState(t, f).Plans["p-crw-129"]; got.AlertEpoch != 1 {
		t.Fatalf("stored = %+v, want the renamed state that records the alert", got)
	}
}

// The lineage the pump's read gives a state without one is a write of the state as it reads, made
// only when a plan is to be judged: a dry run and a read with no plan configured write nothing, and
// the write changes no entry (the persistence clock, the waiting and alerted sets, the epoch and the
// lifecycle stay as they were).
func TestPumpSignalCapacityLineageIsWrittenOnlyBeforeAJudgement(t *testing.T) {
	f, cf, crash := pumpSignalRealCapacity(t)
	path := filepath.Join(cf.stateDir, capacityStateFile)
	legacy, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pumpRound(context.Background(), f.e, f.cfg, pumpSettingsFrom(f.cfg), true); err != nil {
		t.Fatalf("dry round: %v", err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, legacy) {
		t.Fatalf("a dry run wrote the capacity state: %q, %v", after, err)
	}

	// With no plan configured the read writes nothing before its commit (which the process does not
	// reach here): only that commit, after the pending save, makes the file.
	pumpSignalTestReplan(t, f, cf, []map[string]any{})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	*crash = true
	f.round()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("a read with no plan configured made the capacity state before its commit: %v", err)
	}

	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	before := pumpSignalTestCapacityState(t, cf)
	pumpSignalTestReplan(t, f, cf, capacityTestPlans)
	f.advance(31 * time.Minute)
	if st := f.round(); len(st.Pending) != 1 {
		t.Fatalf("pending = %v, want the alert judged on the state given a lineage", pumpSignalIDs(st.Pending))
	}
	after := pumpSignalTestCapacityState(t, cf)
	if after.Lineage == "" || !strings.HasPrefix(after.Origin, "content:") || after.Version != 1 {
		t.Fatalf("state = %+v, want a lineage started over the file without one", after)
	}
	if !capacityTestSamePlans(before.Plans, after.Plans) || after.Plans["p-crw-129"].Lifecycle != "" {
		t.Fatalf("the lineage write changed an entry:\nbefore %+v\nafter  %+v", before.Plans, after.Plans)
	}
}
