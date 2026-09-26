package mergeturn

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

func fStatus(w *fx, project, state string) {
	w.exec("UPDATE scope_bindings SET status=? WHERE scope_key=? AND role='parent'", state, project)
}
func fClaim(w *fx, endpoint registry.Endpoint, project, head string, ready bool) string {
	return w.must(w.claimOn(endpoint, project, head, fxBase, ready))["turnId"].(string)
}
func fReady(w *fx, id, actor string, ready bool, cause, head string) (map[string]any, error) {
	return w.m.Ready(w.ctx, id, actor, ready, head, cause)
}
func fRelease(w *fx, id, actor, reason, disposition, evidence string) (map[string]any, error) {
	return w.m.Release(w.ctx, id, actor, disposition, reason, evidence)
}
func fAttest(w *fx, id, kind, key, evidence string) (map[string]any, error) {
	return w.m.Attest(w.ctx, id, kind, key, beta.TaskID, evidence)
}

func Test26_MTN_3_whole_causes(t *testing.T) {
	t.Run("paused", func(t *testing.T) {
		w := newFx(t)
		w.claim(alpha, fxA, "head-a")
		w.claim(beta, fxB, "head-b")
		fStatus(w, fxA, "paused")
		w.step(w.m.Target(w.ctx, fxRepo, fxBase))
		w.sameAsPython("mtn3_holder_paused")
	})
	t.Run("merging", func(t *testing.T) {
		w := newFx(t)
		id := w.held()
		w.step(w.begin(id, defaults()))
		w.step(w.m.Target(w.ctx, fxRepo, fxBase))
		w.sameAsPython("mtn3_merge_in_flight")
	})
	t.Run("other causes", func(t *testing.T) {
		w := newFx(t)
		for i, cause := range []string{"required", "review", "unready", "disagree", "lost", "withheld", "old", "moved", "repeat"} {
			base := fmt.Sprintf("cause-%d", i)
			w.target.set(fxRepo, base, "base-0")
			id := w.must(w.claimOn(alpha, fxA, "head-a", base, cause != "unready"))["turnId"].(string)
			if cause == "required" || cause == "lost" || cause == "withheld" {
				w.must(w.claimOn(beta, fxB, "head-b", base, true))
			}
			if cause == "required" || cause == "review" || cause == "disagree" || cause == "old" || cause == "moved" || cause == "repeat" {
				w.answer(id, alpha.TaskID)
			}
			if cause == "required" || cause == "disagree" || cause == "old" || cause == "repeat" {
				b := defaults()
				b.checks = runChecks("head-a", "failure", 1, "dev-gate", "run-1")
				w.step(w.begin(id, b))
			}
			if cause == "review" || cause == "disagree" {
				b := defaults()
				b.review = review("hasNextPage", true, "pagesRead", json.Number("1"), "totalCount", json.Number("4"), "threadsSeen", []any{"thread-1"}, "unresolved", json.Number("0"))
				w.step(w.begin(id, b))
			}
			if cause == "lost" {
				w.exec("UPDATE scope_bindings SET task_id='task-alpha-2' WHERE scope_key=? AND role='parent'", fxA)
			}
			if cause == "withheld" {
				fStatus(w, fxB, "paused")
			}
			if cause == "old" {
				w.step(fReady(w, id, alpha.TaskID, true, "", "head-b"))
				w.step(fReady(w, id, alpha.TaskID, true, "", ""))
			}
			if cause == "moved" {
				b := defaults()
				b.head = "head-b"
				b.checks = runChecks("head-b", "success", 1, "dev-gate", "run-1")
				w.step(w.begin(id, b))
			}
			if cause == "repeat" {
				b := defaults()
				b.checks = runChecks("head-a", "failure", 1, "dev-gate", "run-1")
				w.step(w.begin(id, b))
			}
			w.step(w.m.Target(w.ctx, fxRepo, base))
			if cause == "lost" {
				w.exec("UPDATE scope_bindings SET task_id='task-alpha' WHERE scope_key=? AND role='parent'", fxA)
			}
			if cause == "withheld" {
				fStatus(w, fxB, "active")
			}
		}
		w.step(w.m.Target(w.ctx, fxRepo, "empty"))
		w.sameAsPython("mtn3_causes")
	})
}

func Test26_MTN_1_whole_paused(t *testing.T) {
	t.Run("owner", func(t *testing.T) {
		w := newFx(t)
		fStatus(w, fxA, "paused")
		w.step(w.claimOn(alpha, fxA, "head-a", fxBase, true))
		w.step(w.m.Target(w.ctx, fxRepo, fxBase))
		w.sameAsPython("mtn1_paused")
	})
	t.Run("waiter", func(t *testing.T) {
		w := newFx(t)
		held := fClaim(w, alpha, fxA, "head-a", true)
		id := fClaim(w, beta, fxB, "head-b", false)
		fStatus(w, fxB, "paused")
		w.step(fRelease(w, held, alpha.TaskID, "done", "returned", ""))
		w.step(fReady(w, id, beta.TaskID, true, "", ""))
		w.step(w.m.Target(w.ctx, fxRepo, fxBase))
		w.turn(id)
		fStatus(w, fxB, "active")
		w.step(w.m.Outstanding(w.ctx, beta.TaskID))
		w.step(fReady(w, id, beta.TaskID, true, "", ""))
		fStatus(w, fxB, "paused")
		g := w.must(w.m.Turn(w.ctx, id))["grant"].(map[string]any)["grantId"].(string)
		w.step(w.m.Acknowledge(w.ctx, id, beta.TaskID, g, "back now"))
		b := defaults()
		b.actor = beta.TaskID
		b.head = "head-b"
		b.checks = runChecks("head-b", "success", 1, "dev-gate", "run-1")
		w.step(w.begin(id, b))
		w.sameAsPython("mtn1_waiter")
	})
	t.Run("skip", func(t *testing.T) {
		w := newFx(t)
		held := fClaim(w, alpha, fxA, "head-a", true)
		first := fClaim(w, beta, fxB, "head-b", true)
		gamma := ep("task-gamma", "host-g", "/gamma")
		w.bindParent("PRJ-C", gamma)
		second := fClaim(w, gamma, "PRJ-C", "head-c", true)
		fStatus(w, fxB, "paused")
		w.step(w.m.Target(w.ctx, fxRepo, fxBase))
		w.step(fRelease(w, held, alpha.TaskID, "done", "returned", ""))
		w.turn(first)
		w.turn(second)
		w.sameAsPython("mtn1_skip")
	})
}

func Test26_MTN_2_whole_readiness(t *testing.T) {
	w := newFx(t)
	held := fClaim(w, alpha, fxA, "head-a", true)
	waiter := fClaim(w, beta, fxB, "head-b", true)
	w.step(fReady(w, held, alpha.TaskID, false, "a new finding arrived on the pull request", ""))
	w.step(fRelease(w, held, alpha.TaskID, "my readiness died; handing it on", "returned", ""))
	w.turn(waiter)
	next := fClaim(w, alpha, fxA, "head-c", true)
	w.step(fReady(w, next, alpha.TaskID, true, "", "head-c2"))
	w.turn(next)
	w.sameAsPython("mtn2_readiness")
}

func Test26_MTN_7_whole_transport(t *testing.T) {
	w := newFx(t)
	held := fClaim(w, alpha, fxA, "head-a", true)
	waiter := fClaim(w, beta, fxB, "head-b", true)
	w.step(w.m.Attest(w.ctx, held, "return_requested", "return_requested:"+beta.TaskID, beta.TaskID, "I have a ready candidate"))
	for range 3 {
		w.step(fAttest(w, held, "transport_accepted", "delivery-1", "the relay accepted the message"))
	}
	w.step(w.m.Target(w.ctx, fxRepo, fxBase))
	w.turn(waiter)
	w.rows("SELECT * FROM merge_turn_ledger WHERE turn_id=? ORDER BY recorded_at,entry_id", held)
	w.step(fRelease(w, held, alpha.TaskID, "candidate is not ready", "returned", ""))
	w.sameAsPython("mtn7_transport")
}

func Test26_MTN_8_whole_promotion(t *testing.T) {
	cases := []string{"promotion", "unready", "occupied", "lost_owner", "cancel", "evidence"}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			w := newFx(t)
			held := fClaim(w, alpha, fxA, "head-a", true)
			switch c {
			case "promotion":
				id := fClaim(w, beta, fxB, "head-b", true)
				w.step(fRelease(w, held, alpha.TaskID, "checks are not green yet", "returned", ""))
				w.turn(id)
			case "unready":
				id := fClaim(w, beta, fxB, "head-b", false)
				w.step(fRelease(w, held, alpha.TaskID, "done", "returned", ""))
				w.step(w.m.Target(w.ctx, fxRepo, fxBase))
				w.step(fReady(w, id, beta.TaskID, true, "", ""))
			case "occupied":
				id := fClaim(w, beta, fxB, "head-b", false)
				w.step(fReady(w, id, beta.TaskID, true, "", ""))
			case "lost_owner":
				id := fClaim(w, beta, fxB, "head-b", true)
				fStatus(w, fxB, "archived")
				w.step(fRelease(w, held, alpha.TaskID, "done", "returned", ""))
				w.turn(id)
				w.contests()
			case "cancel":
				w.step(fRelease(w, held, overseer.TaskID, "parent stopped answering", "cancelled", "no turn for two hours, host checked"))
				w.step(fRelease(w, held, alpha.TaskID, "I am back", "returned", ""))
				w.turn(held)
			case "evidence":
				w.step(fRelease(w, held, overseer.TaskID, "it stopped", "cancelled", ""))
			}
			w.sameAsPython("mtn8_" + c)
		})
	}
}

func Test26_MTN_9_whole_clock(t *testing.T) {
	w := newFx(t)
	held := w.merging("head-a")
	waiter := fClaim(w, beta, fxB, "head-b", true)
	w.step(fRelease(w, held, overseer.TaskID, "host stopped", "cancelled", "host checked"))
	w.step(w.m.Unknown(w.ctx, held, alpha.TaskID, "lost"))
	w.m.Now = func() string { return registry.ISO(time.Unix(1_700_000_000+1_000_000, 0)) }
	w.step(w.m.Target(w.ctx, fxRepo, fxBase))
	w.turn(waiter)
	w.step(w.resolve(held, "", "merged", "I looked", ""))
	w.merged(fxPost)
	w.step(w.resolve(held, fxPost, "merged", "the pull request reads merged", ""))
	w.turn(waiter)
	w.sameAsPython("mtn9_no_clock_release")
}
