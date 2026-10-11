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

// The fourth round of CRW-692: the first-sight boundary of a parent rollout (what is history and
// what is a new refusal) is made durable before the reading that classifies refusals against it, so
// a pump that ends, or whose state save fails, before its pending events reach disk reads the same
// refusals again instead of taking them for history on the next first sight.

func pumpSignalTestPins(t *testing.T, f *pumpSignalFixture) map[string]int64 {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.cfg.StateDir, pumpSignalDagPinFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Pins map[string]pumpSignalDagPin `json:"pins"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("pin file %q: %v", raw, err)
	}
	pins := map[string]int64{}
	for path, pin := range doc.Pins {
		pins[path] = pin.At
	}
	return pins
}

// A refusal the parent appends while the first reading of its rollout runs is not history. The
// pump that collected it stopped before its pending events reached disk; a new process, with the
// state as it was on disk, reports it again.
func TestPumpSignalDagFirstSightRefusalSurvivesALostPendingWrite(t *testing.T) {
	f, rollout, _ := pumpSignalHostDag(t, dagHostResponseItem(t, map[string]any{"type": "message", "role": "user"}))
	saved := dagHostAfterBoundary
	t.Cleanup(func() { dagHostAfterBoundary = saved })
	dagHostAfterBoundary = func(path string) {
		if path == rollout {
			dagHostAppendRollout(t, path, pumpSignalRefusalCall(t, "call-new", "dag-release --plan p1", "stale")...)
		}
	}
	first := pumpNewState()
	res := (pumpSignalDagSource{}).Collect(context.Background(), f.e, f.cfg, &first, pumpSettingsFrom(f.cfg))
	if len(res.Events) != 1 || !strings.Contains(res.Events[0].Text, "dag-release") {
		t.Fatalf("first collection = %+v, want the refusal appended during the reading", res)
	}
	// No pumpSave: the process stopped before its pending events reached disk.
	dagHostAfterBoundary = nil
	restarted := pumpNewState()
	retry := (pumpSignalDagSource{}).Collect(context.Background(), f.e, f.cfg, &restarted, pumpSettingsFrom(f.cfg))
	if len(retry.Events) != 1 || retry.Events[0].ID != res.Events[0].ID || !strings.Contains(retry.Events[0].Text, "dag-release") {
		t.Fatalf("after the restart = %+v, want the same refusal under the same id %q", retry.Events, res.Events[0].ID)
	}
}

// The first round's state save fails while a relay call of the parent waits for its answer. The
// answer, a refusal, arrives before the next round: it is reported, not taken for history.
func TestPumpSignalDagRefusalAfterAFailedFirstSaveIsReported(t *testing.T) {
	f, rollout, _ := pumpSignalHostDag(t, dagHostToolCall(t, "call-pending", "crw relay dag-release --plan p1"))
	statePath := filepath.Join(f.cfg.StateDir, pumpStateFile)
	if err := os.MkdirAll(f.cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.cfg.StateDir, "not-written.json"), statePath); err != nil {
		t.Fatal(err)
	}
	if _, err := pumpRound(context.Background(), f.e, f.cfg, pumpSettingsFrom(f.cfg), false); err == nil {
		t.Fatal("the first round saved its state through a symlink")
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	dagHostAppendRollout(t, rollout, dagHostToolOutput(t, "call-pending", `{"error":"refused","reason":"stale","detail":"x"}`))
	st := f.round()
	if len(st.Pending) != 1 || !strings.Contains(st.Pending[0].Text, "dag-release") {
		t.Fatalf("pending = %v (%q), want the refusal that arrived after the failed save", pumpSignalIDs(st.Pending), pumpSignalPendingText(st))
	}
}

// The boundary that keeps a rollout's history out is the one first made durable: a restart that
// lost the pump state does not replay the history, and does not move the boundary either.
func TestPumpSignalDagHistoryStaysOutAcrossALostState(t *testing.T) {
	f, rollout, _ := pumpSignalHostDag(t, pumpSignalRefusalCall(t, "call-old", "dag-release --plan p1", "old")...)
	first := pumpNewState()
	if res := (pumpSignalDagSource{}).Collect(context.Background(), f.e, f.cfg, &first, pumpSettingsFrom(f.cfg)); len(res.Events) != 0 {
		t.Fatalf("first sight reported the history: %+v", res.Events)
	}
	pins := pumpSignalTestPins(t, f)
	info, err := os.Stat(rollout)
	if err != nil {
		t.Fatal(err)
	}
	if pins[rollout] != info.Size() {
		t.Fatalf("pins = %v, want the rollout pinned at its size %d", pins, info.Size())
	}
	dagHostAppendRollout(t, rollout, pumpSignalRefusalCall(t, "call-new", "dag-accept --plan p2", "new")...)
	restarted := pumpNewState()
	res := (pumpSignalDagSource{}).Collect(context.Background(), f.e, f.cfg, &restarted, pumpSettingsFrom(f.cfg))
	if len(res.Events) != 1 || strings.Contains(res.Events[0].Text, "dag-release") || !strings.Contains(res.Events[0].Text, "dag-accept") {
		t.Fatalf("after the restart = %+v, want the new refusal alone", res.Events)
	}
	if got := pumpSignalTestPins(t, f)[rollout]; got != info.Size() {
		t.Fatalf("the boundary moved from %d to %d", info.Size(), got)
	}
}

// A boundary that cannot be made durable is a refusal reading the round does not take: nothing is
// reported against it and the rollout is not recorded as read.
func TestPumpSignalDagUnwritablePinTakesNoRefusalReading(t *testing.T) {
	f, rollout, _ := pumpSignalHostDag(t, pumpSignalRefusalCall(t, "call-old", "dag-release --plan p1", "old")...)
	if err := os.MkdirAll(f.cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.cfg.StateDir, "elsewhere.json"), filepath.Join(f.cfg.StateDir, pumpSignalDagPinFile)); err != nil {
		t.Fatal(err)
	}
	st := f.round()
	if len(st.Pending) != 0 {
		t.Fatalf("pending = %v, want nothing reported without a durable boundary", pumpSignalIDs(st.Pending))
	}
	if raw := st.Cursors[pumpSignalDagOffsetsKey]; strings.Contains(raw, rollout) {
		t.Fatalf("offsets = %q, the rollout was recorded as read without a durable boundary", raw)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.StateDir, "elsewhere.json")); !os.IsNotExist(err) {
		t.Fatalf("the pin was written through a symlink: %v", err)
	}
}

// A dry run writes no boundary.
func TestPumpSignalDagDryRunWritesNoPin(t *testing.T) {
	f, _, _ := pumpSignalHostDag(t, dagHostResponseItem(t, map[string]any{"type": "message", "role": "user"}))
	if _, err := pumpRound(context.Background(), f.e, f.cfg, pumpSettingsFrom(f.cfg), true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{pumpSignalDagPinFile, pumpStateFile} {
		if _, err := os.Stat(filepath.Join(f.cfg.StateDir, name)); !os.IsNotExist(err) {
			t.Fatalf("a dry run wrote %s: %v", name, err)
		}
	}
}

// Once the offsets of a rollout are in the saved pump state, its boundary is no longer needed and
// is dropped, so the boundary file stays bounded.
func TestPumpSignalDagPinIsDroppedOnceTheOffsetsAreSaved(t *testing.T) {
	f, rollout, _ := pumpSignalHostDag(t, dagHostResponseItem(t, map[string]any{"type": "message", "role": "user"}))
	f.round()
	if _, ok := pumpSignalTestPins(t, f)[rollout]; !ok {
		t.Fatalf("pins = %v, want the rollout pinned by its first round", pumpSignalTestPins(t, f))
	}
	f.advance(time.Duration(pumpDefaultDagReviewSeconds) * time.Second)
	st := f.round()
	if !strings.Contains(st.Cursors[pumpSignalDagOffsetsKey], rollout) {
		t.Fatalf("offsets = %q, want the rollout's offsets saved", st.Cursors[pumpSignalDagOffsetsKey])
	}
	if pins := pumpSignalTestPins(t, f); len(pins) != 0 {
		t.Fatalf("pins = %v, want the boundary dropped once the offsets are saved", pins)
	}
}
