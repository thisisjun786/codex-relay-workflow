package mergeturn

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// CRW-408 criteria c1..c3: progress records, the holding limit, passing a stalled turn and
// the refusals that follow. The clock is the tests' own: nothing here waits.

type stepClock struct{ at time.Time }

func (c *stepClock) ISO() string             { return registry.ISO(c.at) }
func (c *stepClock) advance(d time.Duration) { c.at = c.at.Add(d) }

// clockedFx is the standard two-parent fixture with a clock the test moves.
func clockedFx(t *testing.T) (*fx, *stepClock) {
	t.Helper()
	w := newFx(t)
	c := &stepClock{at: time.Unix(1_700_000_000, 0)}
	w.m.Now = c.ISO
	w.r.Now = c.ISO
	return w, c
}

// reading is merge-turn-show's target answer as the CLI prints it.
func (w *fx) reading() map[string]any {
	w.t.Helper()
	target := w.must(w.m.Target(w.ctx, fxRepo, fxBase))
	out, _ := jsonValue(w.t, target).(map[string]any)
	return out
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %#v", v)
	}
	return m
}

// ledgerEntry is the newest ledger entry of one evidence kind, its evidence decoded when JSON.
func ledgerEntry(t *testing.T, turn map[string]any, kind string) (map[string]any, map[string]any) {
	t.Helper()
	var found map[string]any
	for _, item := range jsonValue(t, turn["ledger"]).([]any) {
		entry := asMap(t, item)
		if entry["evidenceKind"] == kind {
			found = entry
		}
	}
	if found == nil {
		t.Fatalf("no %s entry in the ledger of %v", kind, turn["turnId"])
	}
	var envelope map[string]any
	_ = json.Unmarshal([]byte(found["evidence"].(string)), &envelope)
	return found, envelope
}

func Test408_c1_a_holder_records_progress_and_the_target_shows_the_last_time(t *testing.T) {
	w, clock := clockedFx(t)
	start := clock.ISO()
	turn := w.held()
	w.must(w.claimOn(beta, fxB, "head-b", fxBase, true))

	before := w.reading()
	if before["lastProgressAt"] != start || before["holdingLimitSeconds"] != float64(1200) || before["stalled"] != false {
		t.Fatalf("a freshly granted and acknowledged turn reads its grant time: %v", before)
	}

	steps := []struct {
		after time.Duration
		step  string
	}{{2 * time.Minute, "base_refresh"}, {1 * time.Minute, "ci_started"}, {6 * time.Minute, "ci_polled"}, {5 * time.Minute, "ci_result"}, {1 * time.Minute, "merge_attempt"}}
	for i, s := range steps {
		clock.advance(s.after)
		answer := w.must(w.m.Progress(w.ctx, turn, alpha.TaskID, s.step, "step "+s.step))
		progress := asMap(t, jsonValue(t, answer["progress"]))
		if progress["step"] != s.step || progress["recordedAt"] != clock.ISO() || progress["sequence"] != float64(i+1) {
			t.Fatalf("answer for %s: %v", s.step, progress)
		}
		shown := w.reading()
		last := asMap(t, shown["lastProgress"])
		if shown["lastProgressAt"] != clock.ISO() || last["step"] != s.step || last["evidenceKind"] != "progress_recorded" {
			t.Fatalf("merge-turn-show after %s: %v", s.step, shown)
		}
	}
	end := w.reading()
	if end["stalled"] != false || end["stallsAt"] != registry.ISO(clock.at.Add(1200*time.Second)) {
		t.Fatalf("a turn that keeps recording does not stall: %v", end)
	}
}

func Test408_c1_the_holder_alone_records_progress_while_it_holds(t *testing.T) {
	w, _ := clockedFx(t)
	turn := w.held()
	waiting := w.must(w.claimOn(beta, fxB, "head-b", fxBase, true))["turnId"].(string)

	_, err := w.m.Progress(w.ctx, turn, beta.TaskID, "ci_started", "not mine")
	if reasonOf(err) != "merge_turn_not_held" {
		t.Fatalf("a task that is not the holder: %v", err)
	}
	_, err = w.m.Progress(w.ctx, waiting, beta.TaskID, "ci_started", "not holding yet")
	if reasonOf(err) != "merge_turn_not_held" || !strings.Contains(err.Error(), "waiting") {
		t.Fatalf("a waiting claim holds nothing to record progress on: %v", err)
	}
	_, err = w.m.Progress(w.ctx, turn, alpha.TaskID, "lunch", "not a step")
	if reasonOf(err) != "merge_evidence_required" {
		t.Fatalf("a step outside the closed list: %v", err)
	}
	w.must(w.m.Release(w.ctx, turn, alpha.TaskID, "returned", "done", ""))
	_, err = w.m.Progress(w.ctx, turn, alpha.TaskID, "ci_result", "too late")
	if reasonOf(err) != "merge_turn_not_held" {
		t.Fatalf("a released turn takes no progress: %v", err)
	}
	conflicts, _ := jsonValue(t, mustConflicts(w)).([]any)
	if len(conflicts) < 3 {
		t.Fatalf("each refusal about a turn is recorded: %v", conflicts)
	}
}

func mustConflicts(w *fx) any {
	key, _ := TargetKey(fxRepo, fxBase)
	conflicts, err := w.r.CoordinationConflicts(w.ctx, registry.DomainMergeTarget, key)
	if err != nil {
		w.t.Fatal(err)
	}
	return conflicts
}

func Test408_c1_progress_while_merging_counts_and_a_merging_turn_still_reads_stalled(t *testing.T) {
	w, clock := clockedFx(t)
	turn := w.merging("head-a")
	clock.advance(10 * time.Minute)
	w.must(w.m.Progress(w.ctx, turn, alpha.TaskID, "merge_attempt", "merge requested on the forge"))
	if shown := w.reading(); shown["lastProgressAt"] != clock.ISO() || shown["stalled"] != false {
		t.Fatalf("progress is accepted while merging: %v", shown)
	}
	clock.advance(time.Duration(HoldingLimitSeconds) * time.Second)
	shown := w.reading()
	blocked := asMap(t, shown["blocked"])
	if shown["stalled"] != true || blocked["cause"] != "merge_in_flight" {
		t.Fatalf("a silent merging turn reads stalled but keeps its cause: %v", shown)
	}
}

func Test408_c2_a_silent_holding_turn_reads_stalled_and_passes_to_the_next_waiter(t *testing.T) {
	w, clock := clockedFx(t)
	start := clock.ISO()
	turn := w.held()
	waiting := w.must(w.claimOn(beta, fxB, "head-b", fxBase, true))["turnId"].(string)

	clock.advance(1199 * time.Second)
	if shown := w.reading(); shown["stalled"] != false {
		t.Fatalf("one second inside the limit: %v", shown)
	}
	_, err := w.m.Pass(w.ctx, turn, beta.TaskID, "alpha has been silent")
	if reasonOf(err) != "merge_turn_not_held" || !strings.Contains(err.Error(), "holding limit") {
		t.Fatalf("a turn inside its limit is not passed: %v", err)
	}
	if got := w.must(w.m.Turn(w.ctx, turn)); got["state"] != "holding" {
		t.Fatalf("the refused pass left the turn alone: %v", got["state"])
	}

	clock.advance(1 * time.Second)
	shown := w.reading()
	blocked := asMap(t, shown["blocked"])
	if shown["stalled"] != true || blocked["cause"] != "holder_stalled" || shown["lastProgressAt"] != start {
		t.Fatalf("at the limit the turn reads stalled: %v", shown)
	}

	passedAt := clock.ISO()
	answer := w.must(w.m.Pass(w.ctx, turn, beta.TaskID, "alpha has been silent for 1200 s; its session is gone"))
	released := asMap(t, jsonValue(t, answer["released"]))
	if released["state"] != "passed" || released["closedAt"] != passedAt || released["holderTaskId"] != alpha.TaskID || released["candidateHead"] != "head-a" {
		t.Fatalf("the pass closes the turn and keeps its original values: %v", released)
	}
	next := asMap(t, jsonValue(t, answer["promoted"]))
	if next["turnId"] != waiting || next["state"] != "holding" || next["grant"] == nil {
		t.Fatalf("the next waiter holds the lane with a grant: %v", next)
	}
	_, envelope := ledgerEntry(t, released, "turn_passed")
	for key, want := range map[string]any{"passedBy": beta.TaskID, "holder": alpha.TaskID, "candidateHead": "head-a", "heldAt": start, "lastProgressAt": start, "lastProgressKind": "grant_acknowledged", "limitSeconds": float64(1200), "elapsedSeconds": float64(1200), "turnId": turn, "evidence": "alpha has been silent for 1200 s; its session is gone"} {
		if envelope[key] != want {
			t.Fatalf("turn_passed %s = %v, want %v (%v)", key, envelope[key], want, envelope)
		}
	}
	if reason, _ := released["closeReason"].(string); !strings.Contains(reason, beta.TaskID) || !strings.Contains(reason, "1200") {
		t.Fatalf("the close reason names who passed it and for how long: %q", reason)
	}
}

func Test408_c2_a_turn_with_progress_is_not_passed(t *testing.T) {
	w, clock := clockedFx(t)
	turn := w.held()
	w.must(w.claimOn(beta, fxB, "head-b", fxBase, true))

	clock.advance(19 * time.Minute)
	w.must(w.m.Progress(w.ctx, turn, alpha.TaskID, "ci_polled", "CI still running"))
	clock.advance(20*time.Minute - time.Second)
	_, err := w.m.Pass(w.ctx, turn, beta.TaskID, "silent")
	if reasonOf(err) != "merge_turn_not_held" {
		t.Fatalf("39 minutes after the grant but one second inside the limit after the last record: %v", err)
	}
	if w.reading()["stalled"] != false {
		t.Fatal("the turn still reads alive")
	}
	clock.advance(1 * time.Second)
	w.must(w.m.Pass(w.ctx, turn, beta.TaskID, "silent since the last ci_polled"))
}

func Test408_c2_who_may_pass_a_stalled_turn(t *testing.T) {
	w, clock := clockedFx(t)
	turn := w.held()
	clock.advance(30 * time.Minute)

	_, err := w.m.Pass(w.ctx, turn, alpha.TaskID, "the holder uses release")
	if reasonOf(err) != "scope_role_mismatch" {
		t.Fatalf("the holder itself: %v", err)
	}
	_, err = w.m.Pass(w.ctx, turn, "task-stranger", "nobody")
	if reasonOf(err) != "scope_role_mismatch" {
		t.Fatalf("a task with no claim on the target and no supervision: %v", err)
	}
	_, err = w.m.Pass(w.ctx, turn, overseer.TaskID, "  ")
	if reasonOf(err) != "merge_evidence_required" {
		t.Fatalf("a pass states why: %v", err)
	}
	answer := w.must(w.m.Pass(w.ctx, turn, overseer.TaskID, "the supervisor passes a silent turn with nobody waiting"))
	if asMap(t, jsonValue(t, answer["released"]))["state"] != "passed" || answer["promoted"] != nil {
		t.Fatalf("the supervisor may pass; with nobody ready the target is free: %v", answer)
	}
	if w.reading()["occupied"] != false {
		t.Fatal("the lane is free")
	}
}

func Test408_c2_a_waiting_claim_that_is_not_ready_may_still_pass(t *testing.T) {
	w, clock := clockedFx(t)
	turn := w.held()
	w.must(w.claimOn(beta, fxB, "head-b", fxBase, false))
	clock.advance(21 * time.Minute)
	answer := w.must(w.m.Pass(w.ctx, turn, beta.TaskID, "silent"))
	if answer["promoted"] != nil {
		t.Fatalf("a claim that never declared ready is not promoted by the pass: %v", answer["promoted"])
	}
}

func Test408_c2_a_merging_turn_is_never_passed(t *testing.T) {
	w, clock := clockedFx(t)
	turn := w.merging("head-a")
	w.must(w.claimOn(beta, fxB, "head-b", fxBase, true))
	clock.advance(time.Hour)
	_, err := w.m.Pass(w.ctx, turn, beta.TaskID, "silent")
	if reasonOf(err) != "merge_turn_unresolved" {
		t.Fatalf("its holder may already have merged, so elapsed time releases nothing: %v", err)
	}
	if got := w.must(w.m.Turn(w.ctx, turn)); got["state"] != "merging" {
		t.Fatalf("still merging: %v", got["state"])
	}
}

func Test408_c3_the_original_holder_is_refused_everywhere_after_the_pass(t *testing.T) {
	w, clock := clockedFx(t)
	turn := w.held()
	grant := asMap(t, jsonValue(t, w.must(w.m.Turn(w.ctx, turn))["grant"]))["grantId"].(string)
	w.must(w.claimOn(beta, fxB, "head-b", fxBase, true))
	clock.advance(25 * time.Minute)
	w.must(w.m.Pass(w.ctx, turn, beta.TaskID, "alpha is gone"))

	refused := map[string]error{}
	_, refused["land"] = w.land(turn, "merge-1", "", "")
	_, refused["release"] = w.m.Release(w.ctx, turn, alpha.TaskID, "returned", "done", "")
	_, refused["ready"] = w.m.Ready(w.ctx, turn, alpha.TaskID, true, "", "")
	_, refused["check"] = w.begin(turn, defaults())
	_, refused["acknowledge"] = w.m.Acknowledge(w.ctx, turn, alpha.TaskID, grant, "read the grant")
	_, refused["progress"] = w.m.Progress(w.ctx, turn, alpha.TaskID, "ci_result", "late")
	_, refused["withdraw"] = w.m.Withdraw(w.ctx, turn, alpha.TaskID)
	for name, err := range refused {
		if err == nil {
			t.Fatalf("%s was accepted after the pass", name)
		}
		if reasonOf(err) != "merge_turn_not_held" {
			t.Fatalf("%s: reason %q", name, reasonOf(err))
		}
		for _, fragment := range []string{"passed", beta.TaskID, "1500", "claim"} {
			if !strings.Contains(err.Error(), fragment) {
				t.Fatalf("%s: the refusal does not say %q: %v", name, fragment, err)
			}
		}
	}
	recorded, _ := json.Marshal(jsonValue(t, mustConflicts(w)))
	if strings.Count(string(recorded), "passed") < len(refused) {
		t.Fatalf("every refusal is recorded as a contest on the target: %s", recorded)
	}
	// The original holder is a claimant like any other afterwards.
	again := w.must(w.claimOn(alpha, fxA, "head-a2", fxBase, true))
	if again["state"] != "waiting" || again["turnId"] == turn {
		t.Fatalf("a new tenure queues behind the lane's holder: %v", again)
	}
}
