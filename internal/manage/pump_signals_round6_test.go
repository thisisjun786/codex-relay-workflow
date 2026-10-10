package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The sixth round of CRW-692: a rollout line without its newline is not read yet, the first-sight
// boundary, the file identity and the scan come from one open descriptor so a replaced rollout
// never takes the old file's boundary, and a capacity alert's retry identity follows the plan's
// lifecycle rather than every write of the capacity state file.

// pumpSignalTestAppendRaw appends text to a file as it stands, without a newline of its own.
func pumpSignalTestAppendRaw(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// pumpSignalTestRefusalOffsets is the refusal offsets the saved pump state holds.
func pumpSignalTestRefusalOffsets(t *testing.T, st pumpState) pumpSignalDagOffsets {
	t.Helper()
	var held pumpSignalDagOffsets
	if raw := st.Cursors[pumpSignalDagOffsetsKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &held); err != nil {
			t.Fatal(err)
		}
	}
	return held
}

// pumpSignalTestRefused is a relay refusal envelope with reason.
func pumpSignalTestRefused(reason string) string {
	return `{"error":"refused","reason":"` + reason + `","detail":"x"}`
}

// pumpSignalTestOneRefusal fails unless the pending events are one refusal event that names want
// and not refuse.
func pumpSignalTestOneRefusal(t *testing.T, what string, st pumpState, want, refuse string) string {
	t.Helper()
	text := pumpSignalPendingText(st)
	if len(st.Pending) != 1 || !strings.Contains(text, want) || (refuse != "" && strings.Contains(text, refuse)) {
		t.Fatalf("%s: pending = %v (%q), want one refusal event naming %q and not %q", what, pumpSignalIDs(st.Pending), text, want, refuse)
	}
	return st.Pending[0].ID
}

// A complete refusal answer whose line has no newline yet is still being written: the reading
// does not take it, the call it answers stays pending so the next reading resumes at the call line,
// and once the newline arrives the refusal is reported exactly once.
func TestPumpSignalDagRefusalWithoutItsNewlineWaitsForIt(t *testing.T) {
	f, rollout, _ := pumpSignalHostDag(t, dagHostToolCall(t, "call-new", "crw relay dag-release --plan p1"))
	pumpSignalTestAppendRaw(t, rollout, dagHostToolOutput(t, "call-new", pumpSignalTestRefused("new_arrival")))
	st := f.round()
	if len(st.Pending) != 0 {
		t.Fatalf("pending = %v (%q), want nothing from a line without its newline", pumpSignalIDs(st.Pending), pumpSignalPendingText(st))
	}
	if got := pumpSignalTestRefusalOffsets(t, st).Offsets[rollout]; got != 0 {
		t.Fatalf("resume offset = %d, want 0: the call line whose answer is unfinished is read again", got)
	}
	pumpSignalTestAppendRaw(t, rollout, "\n")
	f.advance(time.Duration(pumpDefaultDagReviewSeconds) * time.Second)
	id := pumpSignalTestOneRefusal(t, "after the newline", f.round(), "new_arrival", "")
	f.advance(time.Duration(pumpDefaultDagReviewSeconds) * time.Second)
	st = f.round()
	if len(st.Pending) != 1 || st.Pending[0].ID != id {
		t.Fatalf("pending = %v, want only %s", pumpSignalIDs(st.Pending), id)
	}
}

// The same unfinished answer while the pump's state save fails: the next process reads the rollout
// for the first time again and reports the refusal once its newline is there, not as history.
func TestPumpSignalDagRefusalWithoutItsNewlineSurvivesAFailedSave(t *testing.T) {
	f, rollout, _ := pumpSignalHostDag(t, dagHostToolCall(t, "call-new", "crw relay dag-release --plan p1"))
	pumpSignalTestAppendRaw(t, rollout, dagHostToolOutput(t, "call-new", pumpSignalTestRefused("new_arrival")))
	if err := os.MkdirAll(f.cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(f.cfg.StateDir, pumpStateFile)
	if err := os.Symlink(filepath.Join(f.cfg.StateDir, "not-written.json"), statePath); err != nil {
		t.Fatal(err)
	}
	if _, err := pumpRound(context.Background(), f.e, f.cfg, pumpSettingsFrom(f.cfg), false); err == nil {
		t.Fatal("the round saved its state through a symlink")
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	pumpSignalTestAppendRaw(t, rollout, "\n")
	id := pumpSignalTestOneRefusal(t, "after the restart", f.round(), "new_arrival", "")
	f.advance(time.Duration(pumpDefaultDagReviewSeconds) * time.Second)
	if st := f.round(); len(st.Pending) != 1 || st.Pending[0].ID != id {
		t.Fatalf("pending = %v, want only %s", pumpSignalIDs(st.Pending), id)
	}
}

// pumpSignalTestRace runs fn once, at the first call for rollout, from one of the reading's seams.
func pumpSignalTestRace(t *testing.T, seam *func(string), rollout string, fn func()) {
	t.Helper()
	saved := *seam
	t.Cleanup(func() { *seam = saved })
	done := false
	*seam = func(path string) {
		if path == rollout && !done {
			done = true
			fn()
		}
	}
}

// A rollout with a long history, a refusal among it, that a race replaces by a short new rollout
// whose relay call is then refused. Whatever point of the first reading the race hits (before the
// boundary is kept, after it is kept, or a truncation of the same file in place), the old file's
// boundary is never applied to the new file: the new refusal is reported exactly once, and the
// old history is not.
func TestPumpSignalDagReplacedRolloutNeverTakesTheOldBoundary(t *testing.T) {
	history := pumpSignalRefusalCall(t, "call-old", "dag-release --plan p0", "old_history")
	for i := 0; i < 40; i++ {
		history = append(history, dagHostResponseItem(t, map[string]any{"type": "message", "role": "user", "content": strings.Repeat("h", 200)}))
	}
	for _, tc := range []struct {
		name    string
		seam    *func(string)
		replace func(t *testing.T, rollout string)
	}{
		{"renamed before the pin", &dagHostBeforePin, pumpSignalTestRenameNew},
		{"renamed after the pin", &dagHostAfterPin, pumpSignalTestRenameNew},
		{"truncated after the pin", &dagHostAfterPin, pumpSignalTestTruncateNew},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, failedSave := range []bool{true, false} {
				f, rollout, _ := pumpSignalHostDag(t, history...)
				pumpSignalTestRace(t, tc.seam, rollout, func() { tc.replace(t, rollout) })
				if failedSave {
					if err := os.MkdirAll(f.cfg.StateDir, 0o700); err != nil {
						t.Fatal(err)
					}
					statePath := filepath.Join(f.cfg.StateDir, pumpStateFile)
					if err := os.Symlink(filepath.Join(f.cfg.StateDir, "not-written.json"), statePath); err != nil {
						t.Fatal(err)
					}
					if _, err := pumpRound(context.Background(), f.e, f.cfg, pumpSettingsFrom(f.cfg), false); err == nil {
						t.Fatal("the round saved its state through a symlink")
					}
					if err := os.Remove(statePath); err != nil {
						t.Fatal(err)
					}
				} else if st := f.round(); len(st.Pending) != 0 && !strings.Contains(pumpSignalPendingText(st), "new_arrival") {
					t.Fatalf("first round: pending = %q, want nothing but the new refusal", pumpSignalPendingText(st))
				}
				f.advance(time.Duration(pumpDefaultDagReviewSeconds) * time.Second)
				id := pumpSignalTestOneRefusal(t, "after the replacement (failed save "+map[bool]string{false: "no", true: "yes"}[failedSave]+")", f.round(), "new_arrival", "old_history")
				f.advance(time.Duration(pumpDefaultDagReviewSeconds) * time.Second)
				if st := f.round(); len(st.Pending) != 1 || st.Pending[0].ID != id {
					t.Fatalf("pending = %v, want only %s", pumpSignalIDs(st.Pending), id)
				}
			}
		})
	}
}

// pumpSignalTestNewRollout is the short replacement rollout: one relay call and its refusal.
func pumpSignalTestNewRollout(t *testing.T) string {
	return strings.Join(pumpSignalRefusalCall(t, "call-new", "dag-release --plan p1", "new_arrival"), "\n") + "\n"
}

// pumpSignalTestRenameNew replaces the rollout by another file renamed over its path.
func pumpSignalTestRenameNew(t *testing.T, rollout string) {
	t.Helper()
	next := rollout + ".next"
	if err := os.WriteFile(next, []byte(pumpSignalTestNewRollout(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, rollout); err != nil {
		t.Fatal(err)
	}
}

// pumpSignalTestTruncateNew cuts the rollout short in place and writes the new content into the
// same file.
func pumpSignalTestTruncateNew(t *testing.T, rollout string) {
	t.Helper()
	if err := os.WriteFile(rollout, []byte(pumpSignalTestNewRollout(t)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A command-line capacity run judges while the plan has waited 29 minutes (a hold, no alert); the
// pump judges two minutes later (an alert), saves its pending event, and the command line commits
// its unchanged judgement before the pump commits. That save changes nothing of the plan, so the
// pump's refused commit is retried under the same alert: the next read converges on the first id
// instead of adding the same unreflected alert under another one.
func TestPumpSignalCapacityNoopCommandLineCommitKeepsTheAlertId(t *testing.T) {
	f, cf, _ := pumpSignalRealCapacity(t)
	capacityTestState(t, cf, "p-crw-129", capacityTestNow.Add(-29*time.Minute), []string{"CRW-1", "CRW-2"}, nil, time.Time{})
	capacityTestStoreLifecycle(t, cf, "p-crw-129", "stored-lifecycle")
	// The state carries a lineage, as every state the pump has read once does (round 9): the
	// pump's read then writes nothing before its evaluation, and the command line's judgement made
	// before it can still be committed in between.
	if err := capacityStateEstablishLineage(filepath.Join(cf.stateDir, capacityStateFile)); err != nil {
		t.Fatal(err)
	}
	held, err := capacityEvaluate(context.Background(), cf.env, cf.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Report.Plans) != 1 || held.Report.Plans[0].Alert {
		t.Fatalf("setup: the command line's judgement = %+v, want a hold", held.Report.Plans)
	}
	f.advance(2 * time.Minute)
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
			if after := pumpSignalTestCapacityState(t, cf).Plans; !capacityTestSamePlans(before, after) {
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
	if pumpCommit == nil {
		t.Fatal("setup: the pump's commit was not refused after the command line saved")
	}
	f.advance(31 * time.Minute)
	st = f.round()
	pumpSignalWant(t, "after the command line's save", st.Pending, first)
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; len(got.Alerted) != 2 || got.AlertEpoch != 1 {
		t.Fatalf("stored = %+v, want the alert committed at epoch 1", got)
	}
}

// capacityTestStoreLifecycle gives the plan's entry in the capacity state a lifecycle, as every
// entry a commit of this version writes carries one.
func capacityTestStoreLifecycle(t *testing.T, cf *capacityFixture, plan, lifecycle string) {
	t.Helper()
	state := pumpSignalTestCapacityState(t, cf)
	entry := state.Plans[plan]
	entry.Lifecycle = lifecycle
	state.Plans[plan] = entry
	if err := capacityWriteState(filepath.Join(cf.stateDir, capacityStateFile), state); err != nil {
		t.Fatal(err)
	}
}

// capacityTestSamePlans compares two states' plans by what they judge: the waiting clock, the
// waiting set and the last alert.
func capacityTestSamePlans(a, b map[string]capacityPlanState) bool {
	type judged struct {
		Since, AlertedAt float64
		Waiting, Alerted []string
		AlertEpoch       int
	}
	project := func(plans map[string]capacityPlanState) string {
		out := map[string]judged{}
		for name, p := range plans {
			out[name] = judged{p.Since, p.AlertedAt, p.Waiting, p.Alerted, p.AlertEpoch}
		}
		data, _ := json.Marshal(out)
		return string(data)
	}
	return project(a) == project(b)
}

// A rollout whose offsets are saved is replaced by a longer file that holds a refusal before the
// old offset. The offset is the old file's: the new file is read from its start and the refusal is
// reported once, rather than skipped because the new file is no shorter than the old offset.
func TestPumpSignalDagReplacedRolloutLongerThanItsOffsetIsReadFromItsStart(t *testing.T) {
	var history []string
	for i := 0; i < 20; i++ {
		history = append(history, dagHostResponseItem(t, map[string]any{"type": "message", "role": "user", "content": strings.Repeat("h", 200)}))
	}
	f, rollout, _ := pumpSignalHostDag(t, history...)
	st := f.round()
	if len(st.Pending) != 0 {
		t.Fatalf("first round: pending = %q, want nothing", pumpSignalPendingText(st))
	}
	old := pumpSignalTestRefusalOffsets(t, st).Offsets[rollout]
	content := pumpSignalTestNewRollout(t)
	for int64(len(content)) <= old {
		content += dagHostResponseItem(t, map[string]any{"type": "message", "role": "user", "content": strings.Repeat("n", 200)}) + "\n"
	}
	next := rollout + ".next"
	if err := os.WriteFile(next, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, rollout); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Duration(pumpDefaultDagReviewSeconds) * time.Second)
	id := pumpSignalTestOneRefusal(t, "after the replacement", f.round(), "new_arrival", "")
	f.advance(time.Duration(pumpDefaultDagReviewSeconds) * time.Second)
	if st := f.round(); len(st.Pending) != 1 || st.Pending[0].ID != id {
		t.Fatalf("pending = %v, want only %s", pumpSignalIDs(st.Pending), id)
	}
}

// pumpSignalTestRecord rewrites the pump's alert record of a plan in the saved pump state.
func pumpSignalTestRecord(t *testing.T, f *pumpSignalFixture, plan string, change func(*pumpSignalCapacityRecord)) {
	t.Helper()
	st := pumpTestReadState(t, f.cfg)
	rec, ok := pumpSignalCapacityLoad(&st, plan)
	if !ok {
		t.Fatalf("no alert record for %s", plan)
	}
	change(&rec)
	pumpSignalCapacityStore(&st, plan, rec)
	if err := st.pumpSave(f.cfg); err != nil {
		t.Fatal(err)
	}
}

// With no persistence window an alert is judged on the plan's first entry, so a state file lost
// twice starts the same lifecycle twice. The first alert's commit landed but the pump's record of
// it was lost (the save after the commit); a later read finds the alert in the state and settles
// the record, so the alert after the second loss is a new id rather than the one already sent.
func TestPumpSignalCapacityCommitFoundInTheStateSettlesTheRecord(t *testing.T) {
	f, cf, _ := pumpSignalRealCapacity(t)
	capacityTestSection(t, cf, "persist_minutes", 0)
	f.cfg = cf.cfg
	f.cfg.ManagementThread = ""
	statePath := filepath.Join(cf.stateDir, capacityStateFile)
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	st := f.round()
	if len(st.Pending) != 1 {
		t.Fatalf("pending = %v, want the first alert", pumpSignalIDs(st.Pending))
	}
	first := st.Pending[0].ID
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; got.AlertEpoch != 1 {
		t.Fatalf("stored = %+v, want the alert committed", got)
	}
	pumpSignalTestRecord(t, f, "p-crw-129", func(rec *pumpSignalCapacityRecord) { rec.Committed = false })
	f.deliverAll()
	f.advance(31 * time.Minute)
	if st = f.round(); len(st.Pending) != 0 {
		t.Fatalf("pending = %v inside realert_hours", pumpSignalIDs(st.Pending))
	}
	if rec, _ := pumpSignalCapacityLoad(&st, "p-crw-129"); !rec.Committed {
		t.Fatalf("record = %+v, want it settled by the alert the state holds", rec)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	f.advance(31 * time.Minute)
	st = f.round()
	if len(st.Pending) != 1 || st.Pending[0].ID == first {
		t.Fatalf("pending = %v after the second loss, want a new id besides %s", pumpSignalIDs(st.Pending), first)
	}
}

// The relay helper the capacity read runs (the same executable) is killed at the read's deadline
// even when a grandchild keeps its output open: the read returns within the helper's wait delay
// instead of holding the pump round until the grandchild ends.
func TestRelayHelperRunReturnsAtItsDeadlineDespiteAnOpenPipe(t *testing.T) {
	bin := t.TempDir()
	helper := filepath.Join(bin, "crw")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nsleep 12 &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, _, err := (&Env{Executable: helper}).relayHelperRun(ctx, "relay", "doctor")
	if err == nil {
		t.Fatal("the stuck helper answered")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the helper held its caller %v past a 50ms deadline", elapsed)
	}
}
