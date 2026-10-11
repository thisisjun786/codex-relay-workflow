package manage

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tenth round of CRW-692: a capacity state the pump cannot use (missing and not writable, a
// directory at its path, a file it may not read) does not silence the plan's alert. The alert is
// judged on the state as far as it could be read, under the pump's own identity for a state it
// cannot use, and delivered once however often the state is read again; once the state can be used,
// the alert continues under the state's lineage and is not delivered again.

// pumpSignalTestBridge sends the fixture's batches through the real delivery path (Deliver, the
// outbox, the batch's logical id) to bridge. It is applied again after every change of the
// settings, which makes a new configuration.
func pumpSignalTestBridge(t *testing.T, f *pumpSignalFixture, bridge string) {
	t.Helper()
	f.cfg.ManagementThread = "mgmt-thread"
	f.cfg.Bridge = coreBridge{Binary: bridge, ExecutionPolicy: "policy.json"}
}

// pumpSignalTestAcceptingBridge is a fake bridge that accepts every message, and its call log.
func pumpSignalTestAcceptingBridge(t *testing.T) (string, string) {
	t.Helper()
	return deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
}

// pumpSignalTestSends is the number of messages the fake bridge was asked to send.
func pumpSignalTestSends(t *testing.T, log string) int {
	t.Helper()
	sends := 0
	for _, tool := range deliverSendToolsOf(t, log) {
		if tool == deliverToolSend {
			sends++
		}
	}
	return sends
}

// pumpSignalTestRounds runs n rounds, each followed by the next capacity period, and adds the id of
// every capacity event the rounds made pending or delivered to ids.
func pumpSignalTestRounds(f *pumpSignalFixture, n int, ids map[string]bool) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		st := f.round()
		for _, ev := range st.Pending {
			ids[ev.ID] = true
		}
		for id := range st.Sent {
			if strings.HasPrefix(id, pumpSignalCapacityName+"#") {
				ids[id] = true
			}
		}
		f.advance(31 * time.Minute)
	}
}

// pumpSignalTestFault is one way the capacity state cannot be used: set makes it, check proves the
// failure is the real one (the errno a read or the commit's lock open meets), and repair ends it.
type pumpSignalTestFault struct {
	name    string
	nonRoot bool
	set     func(t *testing.T, path string)
	check   func(t *testing.T, path string)
	repair  func(t *testing.T, path string)
}

func pumpSignalTestWantErrno(t *testing.T, err error, errno syscall.Errno, what string) {
	t.Helper()
	if !errors.Is(err, errno) {
		t.Fatalf("setup: %s = %v, want %v", what, err, errno)
	}
}

func pumpSignalTestRemoveState(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

var pumpSignalTestFaults = []pumpSignalTestFault{
	{name: "missing_with_a_lock_that_is_a_directory", set: func(t *testing.T, path string) {
		pumpSignalTestRemoveState(t, path)
		capacityTestMust(t, os.Mkdir(path+".lock", 0o755))
	}, check: func(t *testing.T, path string) {
		_, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
		pumpSignalTestWantErrno(t, err, syscall.EISDIR, "opening the lock")
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("setup: the state file = %v, want it missing", err)
		}
	}, repair: func(t *testing.T, path string) {
		capacityTestMust(t, os.Remove(path+".lock"))
	}},
	{name: "missing_with_a_lock_it_may_not_write", nonRoot: true, set: func(t *testing.T, path string) {
		pumpSignalTestRemoveState(t, path)
		if err := os.WriteFile(path+".lock", nil, 0o444); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
		capacityTestMust(t, os.Chmod(path+".lock", 0o444))
	}, check: func(t *testing.T, path string) {
		_, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
		pumpSignalTestWantErrno(t, err, syscall.EACCES, "opening the lock")
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("setup: the state file = %v, want it missing", err)
		}
	}, repair: func(t *testing.T, path string) {
		capacityTestMust(t, os.Chmod(path+".lock", 0o644))
	}},
	{name: "directory_at_the_state_path", set: func(t *testing.T, path string) {
		pumpSignalTestRemoveState(t, path)
		capacityTestMust(t, os.Mkdir(path, 0o755))
	}, check: func(t *testing.T, path string) {
		_, err := os.ReadFile(path)
		pumpSignalTestWantErrno(t, err, syscall.EISDIR, "reading the state")
	}, repair: func(t *testing.T, path string) {
		capacityTestMust(t, os.Remove(path))
	}},
	{name: "file_it_may_not_read", nonRoot: true, set: func(t *testing.T, path string) {
		capacityTestMust(t, os.Chmod(path, 0))
	}, check: func(t *testing.T, path string) {
		_, err := os.ReadFile(path)
		pumpSignalTestWantErrno(t, err, syscall.EACCES, "reading the state")
	}, repair: func(t *testing.T, path string) {
		capacityTestMust(t, os.Chmod(path, 0o600))
	}},
}

// pumpSignalTestUnusable is the pump over the real judgement with a plan that alerts on its first
// reading (persist_minutes 0), whose batches go through the real delivery path to an accepting fake
// bridge. It returns the fixture, the capacity fixture, the crash switch, the state path and the
// bridge's path and call log.
func pumpSignalTestUnusable(t *testing.T) (*pumpSignalFixture, *capacityFixture, *bool, string, string, string) {
	t.Helper()
	f, cf, crash := pumpSignalRealCapacity(t)
	capacityTestSection(t, cf, "persist_minutes", 0)
	pumpSignalTestReplan(t, f, cf, capacityTestPlans)
	bridge, log := pumpSignalTestAcceptingBridge(t)
	pumpSignalTestBridge(t, f, bridge)
	return f, cf, crash, filepath.Join(cf.stateDir, capacityStateFile), bridge, log
}

// pumpSignalTestWantOnce checks that the plan's alert went out under one id, in one message.
func pumpSignalTestWantOnce(t *testing.T, ids map[string]bool, log string, what string) {
	t.Helper()
	if len(ids) != 1 {
		t.Fatalf("%s: the plan's alert is under %d ids %v, want exactly one", what, len(ids), ids)
	}
	if sends := pumpSignalTestSends(t, log); sends != 1 {
		t.Fatalf("%s: the bridge was asked to send %d messages, want exactly one", what, sends)
	}
}

// pumpSignalTestWantContinued checks that the alert is committed in the state that can be used again
// and that the pump's record of it took that state's lifecycle and is settled.
func pumpSignalTestWantContinued(t *testing.T, f *pumpSignalFixture, cf *capacityFixture, what string) {
	t.Helper()
	stored := pumpSignalTestCapacityState(t, cf)
	entry := stored.Plans["p-crw-129"]
	if stored.Lineage == "" || len(entry.Alerted) != 2 || entry.AlertEpoch != 1 || entry.Lifecycle == "" {
		t.Fatalf("%s: stored = %+v, want the alert committed at epoch 1 in a state with a lineage", what, stored)
	}
	st := pumpTestReadState(t, f.cfg)
	if rec, ok := pumpSignalCapacityLoad(&st, "p-crw-129"); !ok || !rec.Committed || rec.Lifecycle != entry.Lifecycle {
		t.Fatalf("%s: record = %+v (%v), want it settled under the state's lifecycle %q", what, rec, ok, entry.Lifecycle)
	}
}

// The verifier's P2 on 65452a0fe: while the capacity state is missing and cannot be written, or
// cannot be read at all, the round-9 pump judged nothing, so the plan's alert reached nobody until
// the state could be used again. The alert is delivered once while the state stays unusable (four
// reads, the delivery through the real path) and is not delivered again once it can be used: the
// state then commits it and the pump's record follows the state's lifecycle.
func TestPumpSignalCapacityUnusableStateAlertsExactlyOnce(t *testing.T) {
	for _, fault := range pumpSignalTestFaults {
		t.Run(fault.name, func(t *testing.T) {
			if fault.nonRoot && os.Geteuid() == 0 {
				t.Skip("root is not refused by file permissions")
			}
			f, cf, _, path, _, log := pumpSignalTestUnusable(t)
			fault.set(t, path)
			fault.check(t, path)
			ids := map[string]bool{}
			pumpSignalTestRounds(f, 4, ids)
			pumpSignalTestWantOnce(t, ids, log, "while the state cannot be used")
			if st := pumpTestReadState(t, f.cfg); st.Sources[pumpSignalCapacityName] != pumpSourceOK {
				t.Fatalf("status = %q, want the judgement made", st.Sources[pumpSignalCapacityName])
			}
			if logged, err := os.ReadFile(filepath.Join(f.cfg.StateDir, pumpLogFile)); err != nil || !strings.Contains(string(logged), "capacity state") {
				t.Fatalf("pump.log = %q, %v; want the unusable state logged", logged, err)
			}
			// Nothing was committed over the fault: it is still the one the test made.
			fault.check(t, path)
			fault.repair(t, path)
			pumpSignalTestRounds(f, 2, ids)
			pumpSignalTestWantOnce(t, ids, log, "after the state could be used again")
			pumpSignalTestWantContinued(t, f, cf, "after the state could be used again")
		})
	}
}

// An alert the pump made before the state became unusable, committed or with its commit still
// failing, is the alert the unusable state's judgement finds: it is not delivered again while the
// state cannot be read, nor after.
func TestPumpSignalCapacityAlertMadeBeforeTheStateBecameUnusableIsNotSentAgain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not refused by file permissions")
	}
	fault := pumpSignalTestFaults[3]
	for _, committed := range []bool{true, false} {
		name := "committed"
		if !committed {
			name = "commit_still_failing"
		}
		t.Run(name, func(t *testing.T) {
			f, cf, crash, path, bridge, log := pumpSignalTestUnusable(t)
			// The rounds below span more than the default realert_hours, whose periodic re-alert is
			// not what this test is about.
			capacityTestSection(t, cf, "realert_hours", 24)
			pumpSignalTestReplan(t, f, cf, capacityTestPlans)
			pumpSignalTestBridge(t, f, bridge)
			*crash = !committed
			ids := map[string]bool{}
			pumpSignalTestRounds(f, 2, ids)
			pumpSignalTestWantOnce(t, ids, log, "before the fault")
			fault.set(t, path)
			fault.check(t, path)
			pumpSignalTestRounds(f, 4, ids)
			pumpSignalTestWantOnce(t, ids, log, "while the state cannot be read")
			fault.repair(t, path)
			*crash = false
			pumpSignalTestRounds(f, 2, ids)
			pumpSignalTestWantOnce(t, ids, log, "after the state could be read again")
			pumpSignalTestWantContinued(t, f, cf, "after the state could be read again")
		})
	}
}

// The round-8 guarantees hold across a state the pump cannot use. A plan that leaves the settings
// while the state is unusable, or whose entry the command line makes and removes once it can be
// used, alerts again under a new id; a command-line save that leaves the plan's entry alone keeps
// the id; a waiting set that changes is a new alert, and a set that comes back after another one is
// alerted again, as on a usable state; the alert is delivered once however long the state stays
// unusable, while the periodic re-alert falls due at the first usable read past realert_hours; and an
// unusable state's alert that a usable state outlived past that window does not hold back the same
// set's alert on the next state the pump cannot use.
func TestPumpSignalCapacityUnusableStateKeepsTheAlertGenerations(t *testing.T) {
	fault := pumpSignalTestFaults[2]
	first, second := []string{"CRW-1", "CRW-2"}, []string{"CRW-3", "CRW-4"}
	for _, c := range []struct {
		name string
		run  func(t *testing.T, f *pumpSignalFixture, cf *capacityFixture, path, bridge string, ids map[string]bool)
		want int
	}{
		{"plan_left_the_settings_while_unusable", func(t *testing.T, f *pumpSignalFixture, cf *capacityFixture, path, bridge string, ids map[string]bool) {
			pumpSignalTestReplan(t, f, cf, []map[string]any{})
			pumpSignalTestBridge(t, f, bridge)
			pumpSignalTestRounds(f, 1, ids)
			pumpSignalTestReplan(t, f, cf, capacityTestPlans)
			pumpSignalTestBridge(t, f, bridge)
			pumpSignalTestRounds(f, 2, ids)
		}, 2},
		{"entry_made_and_removed_by_the_command_line_once_usable", func(t *testing.T, f *pumpSignalFixture, cf *capacityFixture, path, bridge string, ids map[string]bool) {
			fault.repair(t, path)
			pumpSignalTestCommandLine(t, f, cf, capacityTestPlans)
			f.advance(time.Minute)
			pumpSignalTestCommandLine(t, f, cf, []map[string]any{})
			pumpSignalTestReplan(t, f, cf, capacityTestPlans)
			pumpSignalTestBridge(t, f, bridge)
			pumpSignalTestRounds(f, 2, ids)
		}, 2},
		{"noop_command_line_save_once_usable", func(t *testing.T, f *pumpSignalFixture, cf *capacityFixture, path, bridge string, ids map[string]bool) {
			fault.repair(t, path)
			pumpSignalTestCommandLine(t, f, cf, []map[string]any{})
			pumpSignalTestReplan(t, f, cf, capacityTestPlans)
			pumpSignalTestBridge(t, f, bridge)
			pumpSignalTestRounds(f, 2, ids)
		}, 1},
		{"waiting_set_changes_and_comes_back_while_unusable", func(t *testing.T, f *pumpSignalFixture, cf *capacityFixture, path, bridge string, ids map[string]bool) {
			capacityTestRelay(t, cf, capacityTestReading(second, nil, 12, 12, "within"))
			pumpSignalTestRounds(f, 2, ids)
			capacityTestRelay(t, cf, capacityTestReading(first, nil, 12, 12, "within"))
			pumpSignalTestRounds(f, 2, ids)
			fault.repair(t, path)
			pumpSignalTestRounds(f, 2, ids)
		}, 3},
		{"unusable_alert_outlived_by_a_hold_then_unusable_again", func(t *testing.T, f *pumpSignalFixture, cf *capacityFixture, path, bridge string, ids map[string]bool) {
			// The state can be used again while the plan holds, past the first alert's realert
			// window; the next unusable state then alerts the same set again.
			capacityTestRelay(t, cf, capacityTestReading(first, nil, 12, 12, "deferring"))
			fault.repair(t, path)
			pumpSignalTestRounds(f, 5, ids)
			fault.set(t, path)
			capacityTestRelay(t, cf, capacityTestReading(first, nil, 12, 12, "within"))
			pumpSignalTestRounds(f, 2, ids)
		}, 2},
		{"unusable_past_realert_hours", func(t *testing.T, f *pumpSignalFixture, cf *capacityFixture, path, bridge string, ids map[string]bool) {
			pumpSignalTestRounds(f, 6, ids)
			if len(ids) != 1 {
				t.Fatalf("while the state stays unusable past realert_hours: %d ids %v, want one", len(ids), ids)
			}
			fault.repair(t, path)
			pumpSignalTestRounds(f, 2, ids)
		}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, cf, _, path, bridge, log := pumpSignalTestUnusable(t)
			fault.set(t, path)
			ids := map[string]bool{}
			pumpSignalTestRounds(f, 2, ids)
			pumpSignalTestWantOnce(t, ids, log, "while the state cannot be used")
			c.run(t, f, cf, path, bridge, ids)
			if len(ids) != c.want {
				t.Fatalf("ids = %v, want %d", ids, c.want)
			}
			if sends := pumpSignalTestSends(t, log); sends != c.want {
				t.Fatalf("the bridge was asked to send %d messages, want %d", sends, c.want)
			}
			if c.name == "waiting_set_changes_and_comes_back_while_unusable" {
				if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; !slices.Equal(got.Alerted, first) {
					t.Fatalf("stored = %+v, want the set that came back committed", got)
				}
			}
		})
	}
}
