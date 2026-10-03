package mergeturn

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// CRW-408 criteria c1 and c2: the limit's derivation, the order of signs of life, the clock's
// edges and the pass's atomicity.

func Test408_c2_the_limit_is_the_longest_ci_job_plus_a_margin(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	longest := 0
	for _, m := range regexp.MustCompile(`timeout-minutes:\s*(\d+)`).FindAllStringSubmatch(string(raw), -1) {
		minutes, _ := strconv.Atoi(m[1])
		longest = max(longest, minutes)
	}
	if longest*60 != LongestCISeconds || HoldingLimitSeconds != LongestCISeconds+HoldingMarginSeconds {
		t.Fatalf("ci.yml allows a job %d s but the holding limit is derived from %d s plus %d s: update the constants in progress.go with the workflow", longest*60, LongestCISeconds, HoldingMarginSeconds)
	}
}

func Test408_c1_the_last_sign_of_life_is_decided_by_time_then_rank_then_sequence(t *testing.T) {
	w, clock := clockedFx(t)
	turn := w.held()
	// The grant was acknowledged at the same instant it was granted; progress records at that
	// same instant outrank it, and a later sequence outranks an earlier one.
	w.must(w.m.Progress(w.ctx, turn, alpha.TaskID, "base_refresh", "first"))
	w.must(w.m.Progress(w.ctx, turn, alpha.TaskID, "ci_started", "second"))
	last := asMap(t, w.reading()["lastProgress"])
	if last["evidenceKind"] != "progress_recorded" || last["step"] != "ci_started" || last["sequence"] != float64(2) {
		t.Fatalf("progress records outrank the acknowledgement of the same instant, and the newer sequence outranks the older: %v", last)
	}
	clock.advance(time.Minute)
	w.must(w.m.Progress(w.ctx, turn, alpha.TaskID, "ci_polled", "third"))
	if asMap(t, w.reading()["lastProgress"])["step"] != "ci_polled" {
		t.Fatal("the newest record wins")
	}
}

func Test408_c2_a_clock_that_went_backwards_or_an_unreadable_stored_time_never_stalls_a_turn(t *testing.T) {
	w, clock := clockedFx(t)
	w.held()
	w.must(w.claimOn(beta, fxB, "head-b", fxBase, true))
	clock.advance(-2 * time.Hour)
	if shown := w.reading(); shown["stalled"] != false {
		t.Fatalf("a clock behind the last record reads as no silence at all: %v", shown)
	}
	clock.advance(2*time.Hour + 3*time.Hour)
	if w.reading()["stalled"] != true {
		t.Fatal("control: the same turn does stall once enough time has passed")
	}
	w.exec("UPDATE merge_turns SET held_at = 'garbage' WHERE state = 'holding'")
	w.exec("DELETE FROM merge_turn_ledger WHERE evidence_kind = 'grant_acknowledged'")
	if shown := w.reading(); shown["stalled"] != false {
		t.Fatalf("a stored time that cannot be read is not evidence of silence: %v", shown)
	}
}

// failingQueue is a delivery that addresses a notice and then cannot queue it.
type failingQueue struct{ StoreDelivery }

func (failingQueue) Queue(context.Context, string, string, string, string, string, string) error {
	return errors.New("the delivery store is unavailable")
}

func Test408_c2_a_pass_whose_promotion_cannot_queue_its_wake_changes_nothing(t *testing.T) {
	w, clock := clockedFx(t)
	w.exec("INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES ('rel-b','ISS-2','active','task-beta','host-b','child-b','child-host',1,'[]','[\"task-beta\"]','2023-11-14T22:13:20.000000+00:00','2023-11-14T22:13:20.000000+00:00')")
	turn := w.held()
	w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxB, beta.TaskID, beta.HostID, "head-b", true, ClaimOptions{Relationship: sql.NullString{String: "rel-b", Valid: true}}))
	w.m.Delivery = failingQueue{StoreDelivery{Store: w.s}}
	clock.advance(30 * time.Minute)
	if _, err := w.m.Pass(w.ctx, turn, beta.TaskID, "alpha is gone"); err == nil || !strings.Contains(err.Error(), "delivery store is unavailable") {
		t.Fatalf("the pass fails on the next waiter's grant that cannot be queued, not before or after it: %v", err)
	}
	after := w.must(w.m.Turn(w.ctx, turn))
	if after["state"] != "holding" {
		t.Fatalf("the failed pass left the turn holding: %v", after["state"])
	}
	for _, entry := range jsonValue(t, after["ledger"]).([]any) {
		if kind := asMap(t, entry)["evidenceKind"]; kind == "turn_passed" || kind == "close" {
			t.Fatalf("the failed pass wrote %v", kind)
		}
	}
}

func Test408_c2_a_repeated_pass_is_refused_and_changes_nothing(t *testing.T) {
	w, clock := clockedFx(t)
	turn := w.held()
	waiting := w.must(w.claimOn(beta, fxB, "head-b", fxBase, true))["turnId"].(string)
	clock.advance(25 * time.Minute)
	w.must(w.m.Pass(w.ctx, turn, beta.TaskID, "alpha is gone"))
	_, err := w.m.Pass(w.ctx, turn, beta.TaskID, "alpha is gone")
	if reasonOf(err) != "merge_turn_not_held" || !strings.Contains(err.Error(), "passed") {
		t.Fatalf("passing a passed turn: %v", err)
	}
	if got := w.must(w.m.Turn(w.ctx, waiting)); got["state"] != "holding" {
		t.Fatalf("the next waiter still holds: %v", got["state"])
	}
	passes := 0
	for _, entry := range jsonValue(t, w.must(w.m.Turn(w.ctx, turn))["ledger"]).([]any) {
		if asMap(t, entry)["evidenceKind"] == "turn_passed" {
			passes++
		}
	}
	if passes != 1 {
		t.Fatalf("one pass, one record: %d", passes)
	}
}
