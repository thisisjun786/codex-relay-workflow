package manage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// The third round of CRW-692: the DAG source tells one relay refusal from another (the real host
// reader gives every refusal of a parent the same four fields), and the capacity read cannot hold
// the pump round past a bound.

// pumpSignalHostDag runs the pump's DAG source over the real DagReview, a fixture store, a fake
// App Server and one parent rollout, and returns the rollout and the review's own state directory.
func pumpSignalHostDag(t *testing.T, lines ...string) (*pumpSignalFixture, string, string) {
	t.Helper()
	f := pumpSignalTestFixture(t, pumpSignalDagSource{})
	rf := dagReviewNewFixture(t)
	rollout := dagHostWriteRollout(t, rf.dir, "parent.jsonl", lines...)
	rf.close()
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"status": map[string]any{"type": "idle"}, "path": rollout,
	}}})
	reviewState := filepath.Join(t.TempDir(), "review-state")
	reviewCfg := dagHostConfig(t, rf, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, reviewState, 0)
	saved := pumpSignalDagReview
	pumpSignalDagReview = func(ctx context.Context, _ *Env, _ *Config) (Review, error) {
		return DagReview(ctx, dagReviewEnv(t), reviewCfg)
	}
	t.Cleanup(func() { pumpSignalDagReview = saved })
	return f, rollout, reviewState
}

func pumpSignalPendingText(st pumpState) string {
	var all []string
	for _, ev := range st.Pending {
		all = append(all, ev.Text)
	}
	return strings.Join(all, "\n")
}

func pumpSignalRefusalCall(t *testing.T, callID, command, reason string) []string {
	t.Helper()
	return []string{
		dagHostToolCall(t, callID, "crw relay "+command),
		dagHostToolOutput(t, callID, `{"error":"refused","reason":"`+reason+`","detail":"x"}`),
	}
}

// D1: two different refusals of the same parent are two reports. The host reader gives them the
// same kind, issue and empty plan and node, so only their own identity tells them apart, and the
// first refusal's standing in a rescanned rollout cannot hide the second.
func TestPumpSignalDagLaterRefusalOfTheSameParentIsReported(t *testing.T) {
	f, rollout, reviewState := pumpSignalHostDag(t, dagHostResponseItem(t, map[string]any{"type": "message", "role": "user"}))
	if st := f.round(); len(st.Pending) != 0 {
		t.Fatalf("first sight reported the rollout's history: %v", pumpSignalIDs(st.Pending))
	}
	dagHostAppendRollout(t, rollout, pumpSignalRefusalCall(t, "call-1", "dag-release --plan p1", "stale_coordinator_epoch")...)
	f.advance(10 * time.Minute)
	st := f.round()
	if text := pumpSignalPendingText(st); !strings.Contains(text, "dag-release") || !strings.Contains(text, "stale_coordinator_epoch") {
		t.Fatalf("pending = %q, want the first refusal", text)
	}
	f.deliverAll()

	dagHostAppendRollout(t, rollout, pumpSignalRefusalCall(t, "call-2", "dag-accept --plan p2", "plan_not_found")...)
	f.advance(10 * time.Minute)
	st = f.round()
	text := pumpSignalPendingText(st)
	if !strings.Contains(text, "dag-accept") || !strings.Contains(text, "plan_not_found") {
		t.Fatalf("pending = %q, want the later, different refusal", text)
	}
	if strings.Contains(text, "dag-release") {
		t.Fatalf("pending = %q, repeats the refusal already reported", text)
	}
	f.deliverAll()

	f.advance(10 * time.Minute)
	if st = f.round(); len(st.Pending) != 0 {
		t.Fatalf("pending = %q, want nothing new while no refusal is new", pumpSignalPendingText(st))
	}
	if _, err := os.Stat(filepath.Join(reviewState, dagHostStateFile)); !os.IsNotExist(err) {
		t.Fatalf("the review wrote its own offset file beside the pump's: %v", err)
	}
}

// The same refusal command refused twice with the same reason is two refusals: each call is one
// report, whatever its words.
func TestPumpSignalDagIdenticalRefusalsAreEachReported(t *testing.T) {
	f, rollout, _ := pumpSignalHostDag(t, dagHostResponseItem(t, map[string]any{"type": "message", "role": "user"}))
	f.round()
	dagHostAppendRollout(t, rollout, pumpSignalRefusalCall(t, "call-1", "dag-release --plan p1", "stale")...)
	f.advance(10 * time.Minute)
	first := f.round()
	f.deliverAll()
	dagHostAppendRollout(t, rollout, pumpSignalRefusalCall(t, "call-2", "dag-release --plan p1", "stale")...)
	f.advance(10 * time.Minute)
	second := f.round()
	if len(first.Pending) != 1 || len(second.Pending) != 1 || first.Pending[0].ID == second.Pending[0].ID {
		t.Fatalf("pending after the first %v, after the second %v; want two events of different ids", pumpSignalIDs(first.Pending), pumpSignalIDs(second.Pending))
	}
}

// Two parents' duplicated tool results of different calls are told apart in the standing set: a
// call added later is a change of the set.
func TestPumpSignalDagLaterDuplicateOutputIsReported(t *testing.T) {
	duplicate := func(call string) []string {
		return []string{
			dagHostToolCall(t, call, "crw relay status"),
			dagHostToolOutput(t, call, "first"),
			dagHostToolOutput(t, call, "second"),
		}
	}
	f, rollout, _ := pumpSignalHostDag(t, duplicate("call-a")...)
	st := f.round()
	if text := pumpSignalPendingText(st); !strings.Contains(text, "call-a") {
		t.Fatalf("pending = %q, want the first duplicated call", text)
	}
	f.deliverAll()
	dagHostAppendRollout(t, rollout, duplicate("call-b")...)
	f.advance(10 * time.Minute)
	st = f.round()
	if text := pumpSignalPendingText(st); !strings.Contains(text, "call-b") {
		t.Fatalf("pending = %q, want the later duplicated call", text)
	}
}

// D2: a capacity read that does not return holds the round at most the bound; the audit line, the
// pending write and the other sources go on.
func TestPumpSignalStuckCapacityReadDoesNotHoldUpTheRound(t *testing.T) {
	f, _, _ := pumpSignalRealCapacity(t)
	pumpTestSwapSources(t, pumpSignalCapacitySource{}, pumpSignalAuditSource{})
	pumpSignalTestWriteAlerts(t, f, pumpSignalTestAlertA+"\n")
	savedBudget := pumpSignalCapacityBudget
	pumpSignalCapacityBudget = 150 * time.Millisecond
	t.Cleanup(func() { pumpSignalCapacityBudget = savedBudget })
	saved := pumpSignalCapacity
	pumpSignalCapacity = func(ctx context.Context, _ *Env, _ *Config, _ bool) (CapacityReport, func() error, error) {
		select {
		case <-ctx.Done():
			return CapacityReport{}, nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return CapacityReport{}, nil, errors.New("the read was never bounded")
		}
	}
	t.Cleanup(func() { pumpSignalCapacity = saved })

	start := time.Now()
	st := f.round()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the round took %v behind a stuck capacity read", elapsed)
	}
	if len(st.Pending) != 1 || !strings.HasPrefix(st.Pending[0].ID, pumpSignalAuditName) {
		t.Fatalf("pending = %v, want the audit line saved", pumpSignalIDs(st.Pending))
	}
	if st.Sources[pumpSignalCapacityName] != pumpSourceUnmeasured {
		t.Fatalf("sources = %v, want the capacity read unmeasured", st.Sources)
	}
	// The read is made again on the next round.
	if _, ok := st.Cursors[pumpSignalCapacityReadKey]; ok {
		t.Fatalf("a read that never returned was recorded as made: %v", st.Cursors)
	}
}

// D2: the merged-PR query of the capacity read is bounded by its own deadline, so a gh that never
// answers is an unread signal (the lane count stays unknown) and the judgement still returns.
func TestCapacityMergeCountIsBoundedByItsOwnDeadline(t *testing.T) {
	savedTimeout := capacityExecTimeout
	capacityExecTimeout = 100 * time.Millisecond
	t.Cleanup(func() { capacityExecTimeout = savedTimeout })
	saved := capacityExec
	capacityExec = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return nil, errors.New("gh was never bounded")
		}
	}
	t.Cleanup(func() { capacityExec = saved })
	start := time.Now()
	count, err := capacityMergeCount(context.Background(), &Config{Repository: "owner/repo"}, time.Now())
	if err == nil || count != nil {
		t.Fatalf("count = %v, err = %v; want the stuck read to fail", count, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the read took %v", elapsed)
	}
}

// D2: the real gh child is killed at the deadline even when it was stopped and has a grandchild
// that keeps its output open.
func TestCapacityExecKillsAStuckChildAndItsOutputPipe(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nsleep 30 &\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	savedTimeout, savedDelay := capacityExecTimeout, capacityExecWaitDelay
	capacityExecTimeout, capacityExecWaitDelay = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { capacityExecTimeout, capacityExecWaitDelay = savedTimeout, savedDelay })
	start := time.Now()
	count, err := capacityMergeCount(context.Background(), &Config{Repository: "owner/repo"}, time.Now())
	if err == nil || count != nil {
		t.Fatalf("count = %v, err = %v; want the stuck gh to fail", count, err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the read took %v behind a gh with an open pipe", elapsed)
	}
}

// A review that fails after it moved the refusal offsets gives them back: the refusals it read are
// read again by the next round instead of being lost with the failed review.
func TestPumpSignalDagFailedReviewKeepsTheRefusalOffsets(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalDagSource{})
	dag := pumpSignalTestDag(t)
	saved := pumpSignalDagReview
	t.Cleanup(func() { pumpSignalDagReview = saved })
	fail := true
	pumpSignalDagReview = func(ctx context.Context, e *Env, cfg *Config) (Review, error) {
		store := dagHostOffsetStoreOf(ctx)
		state, err := store.load()
		if err != nil {
			return Review{}, err
		}
		state.Offsets["rollout"] = 42
		if err := store.save(state); err != nil {
			return Review{}, err
		}
		if fail {
			return Review{}, errors.New("a child read failed")
		}
		return dag.review, nil
	}
	st := f.round()
	if raw, ok := st.Cursors[pumpSignalDagOffsetsKey]; ok {
		t.Fatalf("a failed review kept its offsets: %q", raw)
	}
	fail = false
	f.advance(10 * time.Minute)
	st = f.round()
	if raw := st.Cursors[pumpSignalDagOffsetsKey]; !strings.Contains(raw, `"rollout":42`) {
		t.Fatalf("offsets = %q, want the successful review's offsets saved with the state", raw)
	}
}
