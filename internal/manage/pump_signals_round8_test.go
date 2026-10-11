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

// The eighth round of CRW-692: the lifecycle the pump proposed for a plan's entry is reused only
// against the generation of the capacity state it was proposed on, or a later write of that state
// that left the plan's entry alone. A write that made and removed the entry, a lost state file and
// one written again from nothing each end the proposal, so the plan's next alert is a new id rather
// than one already sent. A capacity read is built on only once what it read is durable.

// pumpSignalTestCommandLine is a command-line capacity run (judge and commit) with plans as its
// settings; the pump fixture follows the settings.
func pumpSignalTestCommandLine(t *testing.T, f *pumpSignalFixture, cf *capacityFixture, plans []map[string]any) {
	t.Helper()
	pumpSignalTestReplan(t, f, cf, plans)
	if _, err := Capacity(context.Background(), cf.env, cf.cfg, false); err != nil {
		t.Fatalf("the command line's run: %v", err)
	}
}

// pumpSignalTestEmptyStates are the empty capacity states a new plan is first judged on: one a
// command-line run with no plan committed over a missing file, and one written before the state
// carried a revision.
var pumpSignalTestEmptyStates = []struct {
	name  string
	write func(*testing.T, *pumpSignalFixture, *capacityFixture)
}{
	{"committed", func(t *testing.T, f *pumpSignalFixture, cf *capacityFixture) {
		t.Helper()
		if err := os.Remove(filepath.Join(cf.stateDir, capacityStateFile)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		pumpSignalTestCommandLine(t, f, cf, []map[string]any{})
		pumpSignalTestReplan(t, f, cf, capacityTestPlans)
	}},
	{"legacy", func(t *testing.T, f *pumpSignalFixture, cf *capacityFixture) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cf.stateDir, capacityStateFile), []byte(`{"plans":{}}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}},
}

// pumpSignalTestNewPlan is the pump fixture over the real judgement with a plan the state does not
// hold that alerts on its first reading (persist_minutes 0), on the empty state write makes.
func pumpSignalTestNewPlan(t *testing.T, write func(*testing.T, *pumpSignalFixture, *capacityFixture)) (*pumpSignalFixture, *capacityFixture, *bool) {
	t.Helper()
	f, cf, crash := pumpSignalRealCapacity(t)
	capacityTestSection(t, cf, "persist_minutes", 0)
	pumpSignalTestReplan(t, f, cf, capacityTestPlans)
	write(t, f, cf)
	if _, held := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; held {
		t.Fatal("setup: the state holds the plan")
	}
	return f, cf, crash
}

// pumpSignalTestMakeAndRemove is the command line making the plan's entry under its own lifecycle,
// then, with the plan out of its settings, removing it; the plan is then registered again. The
// pump does not run in between.
func pumpSignalTestMakeAndRemove(t *testing.T, f *pumpSignalFixture, cf *capacityFixture) {
	t.Helper()
	f.advance(5 * time.Minute)
	pumpSignalTestCommandLine(t, f, cf, capacityTestPlans)
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; got.Lifecycle == "" || got.AlertEpoch != 1 {
		t.Fatalf("setup: the command line's entry = %+v, want an alerted entry with a lifecycle", got)
	}
	f.advance(5 * time.Minute)
	pumpSignalTestCommandLine(t, f, cf, []map[string]any{})
	if _, held := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; held {
		t.Fatal("setup: the command line kept the plan's entry")
	}
	pumpSignalTestReplan(t, f, cf, capacityTestPlans)
}

// pumpSignalTestWantNewAlert checks that the round alerts under an id other than first and commits
// the alert.
func pumpSignalTestWantNewAlert(t *testing.T, cf *capacityFixture, st pumpState, first, what string) {
	t.Helper()
	if len(st.Pending) != 1 || st.Pending[0].ID == first {
		t.Fatalf("pending = %v %s, want a new id besides the delivered %s", pumpSignalIDs(st.Pending), what, first)
	}
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; len(got.Alerted) != 2 || got.AlertEpoch != 1 {
		t.Fatalf("stored = %+v, want the new alert committed", got)
	}
}

// The verifier's reproduction on 0b556623c: the pump's first alert of a new plan is saved and
// delivered but its commit is not made; the command line then makes the plan's entry and removes
// it, and the plan is registered again. The entry the pump proposed a lifecycle for was made and
// removed in between, so the proposal is not reused: the alert of the plan registered again is a
// new id, not the one already sent.
func TestPumpSignalCapacityEntryMadeAndRemovedByTheCommandLineEndsTheProposal(t *testing.T) {
	for _, base := range pumpSignalTestEmptyStates {
		t.Run(base.name, func(t *testing.T) {
			f, cf, crash := pumpSignalTestNewPlan(t, base.write)
			*crash = true
			st := f.round()
			if len(st.Pending) != 1 {
				t.Fatalf("pending = %v, want the alert", pumpSignalIDs(st.Pending))
			}
			first := st.Pending[0].ID
			f.deliverAll()
			*crash = false
			pumpSignalTestMakeAndRemove(t, f, cf)
			f.advance(31 * time.Minute)
			pumpSignalTestWantNewAlert(t, cf, f.round(), first, "after the command line made and removed the entry")
		})
	}
}

// The same with the real conflict the verifier used: the pump's commit is refused because a
// command-line run saved the state, unchanged, in between; the command line then makes and removes
// the entry before the pump's next read.
func TestPumpSignalCapacityRefusedCommitThenEntryMadeAndRemovedEndsTheProposal(t *testing.T) {
	for _, base := range pumpSignalTestEmptyStates {
		t.Run(base.name, func(t *testing.T) {
			f, cf, _ := pumpSignalTestNewPlan(t, base.write)
			pumpSignalTestReplan(t, f, cf, []map[string]any{})
			heldCfg := cf.cfg
			held, err := capacityEvaluate(context.Background(), cf.env, heldCfg)
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
					// The pump's read gives a state without a lineage one before it judges (round 9),
					// which refuses a judgement made before it; the command line then judges again
					// and commits, as Capacity does, still between the pump's evaluation and commit.
					err := held.Commit()
					if errors.Is(err, errCapacityStateMoved) {
						_, err = Capacity(context.Background(), cf.env, heldCfg, false)
					}
					if err != nil {
						t.Fatalf("the command line's commit: %v", err)
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
			f.deliverAll()
			pumpSignalTestMakeAndRemove(t, f, cf)
			f.advance(31 * time.Minute)
			pumpSignalTestWantNewAlert(t, cf, f.round(), first, "after the refused commit and the command line's entry")
		})
	}
}

// The verifier's state-loss reproduction on 0b556623c: the pump's first alert of a new plan is
// saved and delivered but its commit is not made, and the capacity state file is then lost (and,
// in the second case, written again from nothing by a command-line run without the plan). A
// proposal never outlives its state file, so the alert judged on the new state is a new id.
func TestPumpSignalCapacityStateLostBeforeTheFirstCommitEndsTheProposal(t *testing.T) {
	for _, base := range pumpSignalTestEmptyStates {
		for _, rewrite := range []bool{false, true} {
			name := base.name + "/lost"
			if rewrite {
				name += "_and_written_again"
			}
			t.Run(name, func(t *testing.T) {
				f, cf, crash := pumpSignalTestNewPlan(t, base.write)
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
				if rewrite {
					f.advance(5 * time.Minute)
					pumpSignalTestCommandLine(t, f, cf, []map[string]any{})
					pumpSignalTestReplan(t, f, cf, capacityTestPlans)
				}
				f.advance(31 * time.Minute)
				pumpSignalTestWantNewAlert(t, cf, f.round(), first, "after the state file was lost")
			})
		}
	}
}

// What the proposal is for still holds: writes that leave the plan's entry alone (a command-line
// run without the plan that changes nothing, and one that makes and removes another plan's entry)
// do not end it, so the pump's retry stores the lifecycle its pending alert carries and converges on
// the id already sent.
func TestPumpSignalCapacityProposalOutlivesWritesThatLeaveTheEntryAlone(t *testing.T) {
	other := []map[string]any{capacityTestPlan("p-other", "P-OTHER", "parent-2", "P-OTHER")}
	for _, base := range pumpSignalTestEmptyStates {
		t.Run(base.name, func(t *testing.T) {
			f, cf, crash := pumpSignalTestNewPlan(t, base.write)
			*crash = true
			st := f.round()
			if len(st.Pending) != 1 {
				t.Fatalf("pending = %v, want the alert", pumpSignalIDs(st.Pending))
			}
			first := st.Pending[0].ID
			f.deliverAll()
			*crash = false
			f.advance(5 * time.Minute)
			pumpSignalTestCommandLine(t, f, cf, []map[string]any{})
			pumpSignalTestCommandLine(t, f, cf, other)
			if _, held := pumpSignalTestCapacityState(t, cf).Plans["p-other"]; !held {
				t.Fatal("setup: the command line made no entry for the other plan")
			}
			pumpSignalTestCommandLine(t, f, cf, []map[string]any{})
			pumpSignalTestReplan(t, f, cf, capacityTestPlans)
			f.advance(31 * time.Minute)
			if st = f.round(); len(st.Pending) != 0 {
				t.Fatalf("pending = %v, want the retry to converge on the delivered %s", pumpSignalIDs(st.Pending), first)
			}
			if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; len(got.Alerted) != 2 || got.AlertEpoch != 1 {
				t.Fatalf("stored = %+v, want the alert committed at epoch 1", got)
			}
		})
	}
}

// The verifier's power-loss candidate on 0b556623c: the pump's commit renamed the new capacity
// state into place but the directory sync after it failed, so a power loss can still take the
// rename back. The next read must not take that state as settled (and drop the proposal and the
// uncommitted record the retry needs) unless it is durable by then: the read syncs the directory
// first, and a sync that fails again leaves the read unmeasured. Whether or not the sync recovers,
// the alert reaches the thread once.
func TestPumpSignalCapacityUnsyncedCommitIsSettledOnlyOnceDurable(t *testing.T) {
	for _, recovers := range []bool{false, true} {
		name := "sync_keeps_failing"
		if recovers {
			name = "sync_recovers"
		}
		t.Run(name, func(t *testing.T) {
			f, cf, crash := pumpSignalTestNewPlan(t, pumpSignalTestEmptyStates[0].write)
			statePath := filepath.Join(cf.stateDir, capacityStateFile)
			before, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			model := capacityTestPowerLoss(t)
			failDir := false
			recorded := capacityStateIO.syncDir
			capacityStateIO.syncDir = func(dir string) error {
				if failDir {
					return errors.New("injected directory sync failure")
				}
				return recorded(dir)
			}
			// The directory sync starts failing with the first commit's write.
			inner := pumpSignalCapacity
			t.Cleanup(func() { pumpSignalCapacity = inner })
			armed := true
			pumpSignalCapacity = func(ctx context.Context, e *Env, cfg *Config, dry bool) (CapacityReport, func() error, error) {
				report, commit, err := inner(ctx, e, cfg, dry)
				if commit == nil || !armed {
					return report, commit, err
				}
				armed = false
				return report, func() error {
					failDir = true
					return commit()
				}, err
			}
			st := f.round()
			if len(st.Pending) != 1 {
				t.Fatalf("pending = %v, want the alert", pumpSignalIDs(st.Pending))
			}
			first := st.Pending[0].ID
			if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; got.AlertEpoch != 1 || model.durable(statePath) {
				t.Fatalf("setup: stored = %+v durable = %v, want the commit renamed into place and not durable", got, model.durable(statePath))
			}
			f.deliverAll()
			// The next read sees the renamed state; the process then ends before that round's commit.
			failDir = !recovers
			*crash = true
			f.advance(31 * time.Minute)
			f.round()
			// A power loss: a rename no directory sync followed is taken back.
			if !model.durable(statePath) {
				if err := os.WriteFile(statePath, before, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			failDir, *crash = false, false
			f.advance(31 * time.Minute)
			if st = f.round(); len(st.Pending) != 0 {
				t.Fatalf("pending = %v after the power loss, want only the delivered %s", pumpSignalIDs(st.Pending), first)
			}
			if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; len(got.Alerted) != 2 || got.AlertEpoch != 1 {
				t.Fatalf("stored = %+v, want the alert committed at epoch 1", got)
			}
		})
	}
}

// A commit carries the file's lineage on and records the plans whose entries it removes; a write
// over no file, or over one written before files carried a lineage, starts a lineage and names the
// file it started over.
func TestCapacityCommitKeepsTheLineageAndRecordsRemovedEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), capacityStateFile)
	commit := func(plans ...string) capacityState {
		t.Helper()
		_, base := capacityLoadState(path)
		next := capacityState{Plans: map[string]capacityPlanState{}}
		for _, name := range plans {
			next.Plans[name] = capacityPlanState{Lifecycle: "l-" + name}
		}
		if err := capacityCommitState(path, base, next); err != nil {
			t.Fatal(err)
		}
		state, _ := capacityLoadState(path)
		return state
	}
	first := commit("a")
	if first.Lineage == "" || first.Origin != capacityStampAbsent || first.Version != 1 || first.Removed != nil {
		t.Fatalf("over no file = %+v, want a new lineage from %q at version 1 and no removal", first, capacityStampAbsent)
	}
	if got := commit("b"); got.Lineage != first.Lineage || got.Origin != first.Origin || got.Version != 2 || len(got.Removed) != 1 || got.Removed["a"] != 2 {
		t.Fatalf("removing a = %+v, want the lineage kept and a removed at version 2", got)
	}
	if got := commit("a"); len(got.Removed) != 1 || got.Removed["b"] != 3 {
		t.Fatalf("making a again = %+v, want only b's removal at version 3", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := commit("a"); got.Lineage == "" || got.Lineage == first.Lineage || got.Version != 1 || got.Removed != nil {
		t.Fatalf("over a lost file = %+v, want another lineage from version 1", got)
	}
	if err := os.WriteFile(path, []byte(`{"plans":{"a":{"since":1}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, legacy := capacityLoadState(path)
	if !strings.HasPrefix(legacy, "content:") {
		t.Fatalf("stamp of a file without a revision = %q, want its content", legacy)
	}
	if got := commit(); got.Lineage == "" || got.Origin != legacy || got.Removed["a"] != 1 {
		t.Fatalf("over a file without a lineage = %+v, want a new lineage from %q and a removed at version 1", got, legacy)
	}
}

// When a proposal holds: on its own generation, and on a later write of its lineage (or the lineage
// that started over the file it was given on) that left the plan's entry alone. Never when it was
// given on no file, once the entry was removed since, on another lineage, on an older version or for
// the other case of held.
func TestCapacityLifecycleProposalHolds(t *testing.T) {
	onLineage := capacityLifecycleProposal{Lifecycle: "L1", Stamp: "4/r4", Lineage: "X", Version: 4}
	onLegacy := capacityLifecycleProposal{Lifecycle: "L1", Stamp: "content:c0"}
	state := func(lineage, origin string, version int, removed map[string]int) capacityState {
		return capacityState{Plans: map[string]capacityPlanState{}, Lineage: lineage, Origin: origin, Version: version, Removed: removed}
	}
	for _, c := range []struct {
		name  string
		p     capacityLifecycleProposal
		held  bool
		state capacityState
		stamp string
		want  bool
	}{
		{"same generation", onLineage, false, state("X", "absent", 4, nil), "4/r4", true},
		{"later write leaving the entry alone", onLineage, false, state("X", "absent", 6, map[string]int{"other": 5}), "6/r6", true},
		{"removed on the generation it was given on", onLineage, false, state("X", "absent", 6, map[string]int{"p": 4}), "6/r6", true},
		{"made and removed since", onLineage, false, state("X", "absent", 6, map[string]int{"p": 6}), "6/r6", false},
		{"another lineage", onLineage, false, state("Y", "absent", 6, nil), "6/r6", false},
		{"an older version", onLineage, false, state("X", "absent", 3, nil), "3/r3", false},
		{"no file now", onLineage, false, state("", "", 0, nil), capacityStampAbsent, false},
		{"the other case of held", onLineage, true, state("X", "absent", 4, nil), "4/r4", false},
		{"unchanged legacy file", onLegacy, false, state("", "", 0, nil), "content:c0", true},
		{"lineage started over it", onLegacy, false, state("Z", "content:c0", 2, nil), "2/r2", true},
		{"lineage started over it, entry removed", onLegacy, false, state("Z", "content:c0", 2, map[string]int{"p": 2}), "2/r2", false},
		{"lineage started over another file", onLegacy, false, state("Z", "content:c9", 2, nil), "2/r2", false},
		{"legacy file rewritten", onLegacy, false, state("", "", 0, nil), "content:c1", false},
		{"given on no file", capacityLifecycleProposal{Lifecycle: "L1", Stamp: capacityStampAbsent}, false, state("", "", 0, nil), capacityStampAbsent, false},
		{"given on no file, lineage started over none", capacityLifecycleProposal{Lifecycle: "L1", Stamp: capacityStampAbsent}, false, state("Z", "absent", 1, nil), "1/r1", false},
		{"given before proposals named a generation", capacityLifecycleProposal{Lifecycle: "L1"}, false, state("X", "absent", 4, nil), "4/r4", false},
	} {
		if got := c.p.holds("p", c.held, c.state, c.stamp); got != c.want {
			t.Errorf("%s: holds = %v, want %v", c.name, got, c.want)
		}
	}
}
