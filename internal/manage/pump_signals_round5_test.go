package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fifth round of CRW-692: the refusal a first DAG reading collects is never lost to a pending
// save that fails or never happens (the boundary that suppresses history is written before the
// reading, and everything else that suppresses a refusal reaches disk only in the write that holds
// the pending event), and a capacity evaluation that ran out of its time is unmeasured instead of
// being taken for a judgement.

// A whole pump round collects a refusal the parent appended during the first reading of its
// rollout, and its state save fails. The next round, a fresh load of what is on disk, derives the
// same event id for the same refusal and saves it; a later round neither repeats it nor keeps the
// boundary.
func TestPumpSignalDagRefusalOfAFailedRoundIsSavedUnderTheSameId(t *testing.T) {
	f, rollout, _ := pumpSignalHostDag(t, dagHostResponseItem(t, map[string]any{"type": "message", "role": "user"}))
	saved := dagHostAfterBoundary
	t.Cleanup(func() { dagHostAfterBoundary = saved })
	appended := false
	dagHostAfterBoundary = func(path string) {
		if path == rollout && !appended {
			appended = true
			dagHostAppendRollout(t, path, pumpSignalRefusalCall(t, "call-new", "dag-release --plan p1", "stale")...)
		}
	}
	// The id the failed round derived: the first refusal event of a state that holds none.
	want := pumpSignalRefusalsName + "#1"
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
	if !appended {
		t.Fatal("setup: the failed round did not read the rollout")
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	st := f.round()
	if len(st.Pending) != 1 || st.Pending[0].ID != want || !strings.Contains(st.Pending[0].Text, "dag-release") {
		t.Fatalf("pending = %v (%q), want the refusal of the failed round under %q", pumpSignalIDs(st.Pending), pumpSignalPendingText(st), want)
	}
	f.advance(time.Duration(pumpDefaultDagReviewSeconds) * time.Second)
	st = f.round()
	if len(st.Pending) != 1 || st.Pending[0].ID != want {
		t.Fatalf("pending = %v, want the one refusal, not reported again", pumpSignalIDs(st.Pending))
	}
	if pins := pumpSignalTestPins(t, f); len(pins) != 0 {
		t.Fatalf("pins = %v, want the boundary dropped once the offsets are saved", pins)
	}
}

// A kept boundary beyond the end of the rollout belongs to a file that was replaced or cut short:
// it would take the new file's refusals up to the old size for history. Nothing in the file can be
// told to be older than the pump's first sight of the path, so the boundary becomes 0 (a boundary
// file written before identities were holds a bare number, which still reads), and a refusal
// appended after the first reading of the new file is reported.
func TestPumpSignalDagBoundaryPastTheEndIsTakenAgain(t *testing.T) {
	f, rollout, _ := pumpSignalHostDag(t, dagHostResponseItem(t, map[string]any{"type": "message", "role": "user"}))
	if err := os.MkdirAll(f.cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale, err := json.Marshal(map[string]any{"pins": map[string]int64{rollout: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.cfg.StateDir, pumpSignalDagPinFile), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	first := pumpNewState()
	if res := (pumpSignalDagSource{}).Collect(context.Background(), f.e, f.cfg, &first, pumpSettingsFrom(f.cfg)); len(res.Events) != 0 {
		t.Fatalf("first sight reported the history: %+v", res.Events)
	}
	if got, ok := pumpSignalTestPins(t, f)[rollout]; !ok || got != 0 {
		t.Fatalf("boundary = %d (kept %v), want 0 for the replaced file", got, ok)
	}
	// The pending save never happened; the refusal arrives before the next process reads.
	dagHostAppendRollout(t, rollout, pumpSignalRefusalCall(t, "call-new", "dag-release --plan p1", "stale")...)
	restarted := pumpNewState()
	res := (pumpSignalDagSource{}).Collect(context.Background(), f.e, f.cfg, &restarted, pumpSettingsFrom(f.cfg))
	if len(res.Events) != 1 || !strings.Contains(res.Events[0].Text, "dag-release") {
		t.Fatalf("after the restart = %+v, want the refusal appended to the new file", res.Events)
	}
}

// pumpSignalExpiringHTTP makes the Actions status read wait for its context and fail, as a stuck
// HTTP read does when the pump's capacity budget runs out; the capacity judgement absorbs that
// failure as an unknown signal and still returns a report.
func pumpSignalExpiringHTTP(t *testing.T) {
	t.Helper()
	savedGet, savedBudget := capacityHTTPGet, pumpSignalCapacityBudget
	t.Cleanup(func() { capacityHTTPGet, pumpSignalCapacityBudget = savedGet, savedBudget })
	pumpSignalCapacityBudget = 50 * time.Millisecond
	capacityHTTPGet = func(ctx context.Context, _ string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// The real capacity evaluation of an alerting plan returns only after its budget ran out (a step
// of it does not honour its context, and the evaluation absorbs a failed signal read as unknown, so
// it still returns an alerting report and a commit). It is unmeasured: no event, no read time, no
// alert record, no commit (the capacity state file is as it was), and the next round judges again
// and alerts.
func TestPumpSignalTimedOutCapacityEvaluationIsUnmeasured(t *testing.T) {
	f, cf, _ := pumpSignalRealCapacity(t)
	statePath := filepath.Join(cf.stateDir, capacityStateFile)
	// The state carries a lineage, as every state the pump has read once does (round 9), so the
	// read writes nothing before its evaluation and the file must be byte for byte as it was.
	if err := capacityStateEstablishLineage(statePath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	savedRead, savedBudget := pumpSignalCapacity, pumpSignalCapacityBudget
	t.Cleanup(func() { pumpSignalCapacity, pumpSignalCapacityBudget = savedRead, savedBudget })
	pumpSignalCapacityBudget = 50 * time.Millisecond
	late := true
	pumpSignalCapacity = func(ctx context.Context, e *Env, cfg *Config, dry bool) (CapacityReport, func() error, error) {
		report, commit, err := pumpSignalCapacityRead(ctx, e, cfg, dry)
		if late {
			<-ctx.Done()
		}
		return report, commit, err
	}
	st := f.round()
	if st.Sources[pumpSignalCapacityName] != pumpSourceUnmeasured {
		t.Fatalf("sources = %v, want the timed-out evaluation unmeasured", st.Sources)
	}
	if len(st.Pending) != 0 {
		t.Fatalf("pending = %v, want no alert from a timed-out evaluation", pumpSignalIDs(st.Pending))
	}
	for key := range st.Cursors {
		if key == pumpSignalCapacityReadKey || strings.HasPrefix(key, pumpSignalCapacityEpochPrefix) {
			t.Fatalf("cursors = %v, the timed-out evaluation was recorded", st.Cursors)
		}
	}
	if after, err := os.ReadFile(statePath); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the capacity state changed after a timed-out evaluation (err %v):\nbefore %s\nafter  %s", err, before, after)
	}
	// The next round is a real read that must not run past a budget: it has the default one, since
	// 50 ms is shorter than a real evaluation can take on a loaded host under -race.
	late, pumpSignalCapacityBudget = false, savedBudget
	st = f.round()
	if st.Sources[pumpSignalCapacityName] != pumpSourceOK || len(st.Pending) != 1 || !strings.HasPrefix(st.Pending[0].ID, pumpSignalCapacityName) {
		t.Fatalf("sources = %v pending = %v, want the next round to judge again and alert", st.Sources, pumpSignalIDs(st.Pending))
	}
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; len(got.Alerted) != 2 || got.AlertEpoch != 1 {
		t.Fatalf("stored = %+v, want the retried alert committed at epoch 1", got)
	}
}

// With no plan configured the judgement has nothing to wait on after the signals, so a read past
// the budget would come back as an ordinary empty report. It is unmeasured all the same.
func TestPumpSignalTimedOutCapacityReadWithoutPlansIsUnmeasured(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalCapacitySource{})
	saved := pumpSignalCapacity
	t.Cleanup(func() { pumpSignalCapacity = saved })
	pumpSignalCapacity = pumpSignalCapacityRead
	pumpSignalExpiringHTTP(t)
	st := f.round()
	if st.Sources[pumpSignalCapacityName] != pumpSourceUnmeasured {
		t.Fatalf("sources = %v, want the timed-out read unmeasured", st.Sources)
	}
	if _, ok := st.Cursors[pumpSignalCapacityReadKey]; ok {
		t.Fatalf("cursors = %v, a timed-out read was recorded as made", st.Cursors)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.StateDir, capacityStateFile)); !os.IsNotExist(err) {
		t.Fatalf("a timed-out read committed a capacity state: %v", err)
	}
}
