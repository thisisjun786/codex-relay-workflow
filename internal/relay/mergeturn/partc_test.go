package mergeturn

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// Part C batch 1: MTN-11..15, MTN-18, MTN-21..24 of test_merge_turn.py, each replayed against
// the live Python answers in testdata/python_mergeturn.json.

func malformedReviews() []any {
	with := func(key string, value any) contract.OrderedObject {
		o := contract.OrderedObject{{Key: "hasNextPage", Value: false}, {Key: "pagesRead", Value: json.Number("1")}, {Key: "totalCount", Value: json.Number("1")}, {Key: "threadsSeen", Value: []any{"thread-1"}}, {Key: "unresolved", Value: json.Number("0")}}
		for i := range o {
			if o[i].Key == key {
				o[i].Value = value
			}
		}
		return o
	}
	two := with("threadsSeen", "ab")
	two[2].Value = json.Number("2")
	fractional := with("totalCount", 1.5)
	fractional[0].Value = nil
	return []any{
		with("threadsSeen", json.Number("1")), two, with("threadsSeen", []any{json.Number("1")}),
		with("unresolved", []any{}), with("unresolved", []any{"thread-1"}), with("pagesRead", "1"),
		with("hasNextPage", "false"), []any{}, "review", with("pagesRead", json.Number("-1")), fractional,
	}
}

func Test26_MTN_11_malformed_review_refused_before_anything_is_read_or_recorded(t *testing.T) {
	w := newFx(t)
	fragments := []string{"threadsSeen is a list of thread identifiers, not a int", "threadsSeen is a list of thread identifiers, not a str", "threadsSeen entry 0 is a thread identifier string, not a int", "unresolved is a whole number, not a list", "unresolved is a whole number, not a list", "pagesRead is a whole number, not a str", "hasNextPage is true or false, not a str", "the review record is an object stating", "the review record is an object stating"}
	for index, stated := range malformedReviews() {
		branch := fmt.Sprintf("dev-%d", index)
		w.target.set(fxRepo, branch, "base-0")
		turn := w.heldOn(alpha, fxA, "head-a", branch)
		w.reads()
		b := defaults()
		b.review = stated
		_, err := w.begin(turn, b)
		w.step(nil, err)
		if index < len(fragments) && (reasonOf(err) != "merge_evidence_malformed" || !strings.Contains(err.Error(), fragments[index])) {
			t.Fatalf("case %d: %v", index, err)
		}
		w.reads()
		w.checksRows(turn)
		w.step(w.begin(turn, defaults()))
	}
	key := "dev-0"
	target, err := w.m.Target(w.ctx, fxRepo, key)
	if err != nil {
		t.Fatal(err)
	}
	w.checksRows(target["holder"].(map[string]any)["turnId"].(string))
	w.sameAsPython("mtn11_malformed")
}

func Test26_MTN_10_review_rows_refused_and_two_distinct_threads_merge(t *testing.T) {
	w := newFx(t)
	turn := w.held()
	for _, stated := range []any{
		review("pagesRead", json.Number("1")),
		review("hasNextPage", true, "pagesRead", json.Number("1"), "totalCount", json.Number("2"), "threadsSeen", []any{"one"}, "unresolved", json.Number("0")),
		review("hasNextPage", false, "pagesRead", json.Number("2"), "totalCount", json.Number("1"), "threadsSeen", []any{"one"}, "unresolved", json.Number("1")),
		review("hasNextPage", false, "pagesRead", json.Number("2"), "totalCount", json.Number("2"), "threadsSeen", []any{"thread-1", "thread-1"}, "unresolved", json.Number("0")),
		review("hasNextPage", false, "pagesRead", json.Number("0"), "totalCount", json.Number("3"), "threadsSeen", []any{"a", " ", ""}, "unresolved", json.Number("0")),
		nil,
	} {
		b := defaults()
		b.review = stated
		w.step(w.begin(turn, b))
	}
	w.checksRows(turn)
	b := defaults()
	b.review = review("hasNextPage", false, "pagesRead", json.Number("2"), "totalCount", json.Number("2"), "threadsSeen", []any{"t-1", "t-2"}, "unresolved", json.Number("0"))
	w.step(w.begin(turn, b))
	w.sameAsPython("mtn10_review_rows")
}

func refusalThen(t *testing.T, name string, change func(*begin)) {
	w := newFx(t)
	turn := w.held()
	b := defaults()
	change(&b)
	w.step(w.begin(turn, b))
	w.checksRows(turn)
	w.turn(turn)
	w.sameAsPython(name)
}

func Test26_MTN_12_required_checks_decide_currency(t *testing.T) {
	ok := func(head, conclusion string, attempt int, name, run string) []any {
		return runChecks(head, conclusion, attempt, name, run)
	}
	cases := map[string]func(*begin){
		"mtn12_required_missing": func(b *begin) { b.required = []string{"dev-gate", "devin"} },
		"mtn12_newest_failing": func(b *begin) {
			b.checks = append(ok("head-a", "success", 1, "dev-gate", "run-1"), ok("head-a", "failure", 2, "dev-gate", "run-1")...)
		},
		"mtn12_newest_passing": func(b *begin) {
			b.checks = append(ok("head-a", "failure", 1, "dev-gate", "run-1"), ok("head-a", "success", 2, "dev-gate", "run-1")...)
		},
		"mtn12_optional_failing": func(b *begin) {
			b.checks = append(ok("head-a", "success", 1, "dev-gate", "run-1"), ok("head-a", "failure", 1, "lint", "run-2")...)
		},
		"mtn12_nothing_required": func(b *begin) {
			b.required, b.checks = []string{}, ok("head-a", "failure", 1, "lint", "run-2")
		},
		"mtn12_other_head":    func(b *begin) { b.checks = ok("head-other", "success", 1, "dev-gate", "run-1") },
		"mtn12_no_checks":     func(b *begin) { b.checks, b.required = []any{}, []string{} },
		"mtn12_ci_unfinished": func(b *begin) { b.checks = runChecks("head-a", nil, 1, "dev-gate", "run-1") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) { refusalThen(t, name, change) })
	}
	t.Run("mtn12_no_identity", func(t *testing.T) {
		w := newFx(t)
		turn := w.held()
		for _, id := range [][2]string{{"", "dev-gate"}, {"run-1", ""}} {
			entry := review("runId", id[0], "name", id[1], "headSha", "head-a", "conclusion", "success", "attempt", json.Number("1"))
			b := defaults()
			b.checks, b.required = []any{entry}, []string{}
			w.step(w.begin(turn, b))
		}
		w.sameAsPython("mtn12_no_identity")
	})
	t.Run("mtn12_base_moved_since_landing", func(t *testing.T) {
		w := newFx(t)
		first := w.held()
		w.must(w.begin(first, defaults()))
		w.merged(fxPost)
		w.step(w.m.Land(w.ctx, first, alpha.TaskID, "merge-1", fxPost, "landed", w.target))
		second := w.heldOn(alpha, fxA, "head-c", fxBase)
		b := defaults()
		b.head, b.checks, b.base = "head-c", runChecks("head-c", "success", 1, "dev-gate", "run-1"), "base-0"
		w.step(w.begin(second, b))
		b.base = fxPost
		w.step(w.begin(second, b))
		w.sameAsPython("mtn12_base_moved_since_landing")
	})
}

func Test26_MTN_13_only_the_current_ready_holder_begins_a_merge(t *testing.T) {
	t.Run("former parent", func(t *testing.T) {
		w := newFx(t)
		turn := w.held()
		w.exec("UPDATE scope_bindings SET status = 'archived' WHERE scope_key = ? AND task_id = ?", fxA, alpha.TaskID)
		b := defaults()
		b.required = []string{}
		w.step(w.begin(turn, b))
		w.contests()
		w.sameAsPython("mtn13_former_parent")
	})
	t.Run("unready", func(t *testing.T) {
		w := newFx(t)
		turn := w.must(w.claimOn(alpha, fxA, "head-a", fxBase, false))["turnId"].(string)
		w.answer(turn, alpha.TaskID)
		b := defaults()
		b.required = []string{}
		w.step(w.begin(turn, b))
		w.sameAsPython("mtn13_unready")
	})
	t.Run("not holder", func(t *testing.T) {
		w := newFx(t)
		turn := w.held()
		b := defaults()
		b.actor = beta.TaskID
		w.step(w.begin(turn, b))
		w.contests()
		w.sameAsPython("mtn13_not_holder")
	})
	t.Run("unanswered grant and paused", func(t *testing.T) {
		w := newFx(t)
		turn := w.claim(alpha, fxA, "head-a")["turnId"].(string)
		w.step(w.begin(turn, defaults()))
		w.exec("UPDATE scope_bindings SET status = 'paused' WHERE scope_key = ?", fxA)
		b := defaults()
		b.head = "head-z"
		w.step(w.begin(turn, b))
		w.sameAsPython("mtn13_unanswered_and_paused")
	})
}

func Test26_MTN_14_acting_on_a_turn_you_do_not_hold_needs_authority(t *testing.T) {
	w := newFx(t)
	held := w.claim(alpha, fxA, "head-a")["turnId"].(string)
	w.step(w.m.Release(w.ctx, held, "task-stranger", "cancelled", "I want it", "none of my business"))
	w.turn(held)
	other := w.must(w.claimOn(beta, fxB, "head-b", "dev-b", true))["turnId"].(string)
	w.step(w.m.Release(w.ctx, other, overseer.TaskID, "cancelled", "stuck", "the parent stopped answering"))
	w.answer(held, alpha.TaskID)
	w.must(w.check(held, "head-a", "base-0", ""))
	w.step(w.m.Unknown(w.ctx, held, "task-stranger", "I say so"))
	w.step(w.m.Unknown(w.ctx, held, alpha.TaskID, "lost the connection"))
	w.step(w.resolve(held, "base-9", "merged", "I looked", "task-stranger"))
	w.turn(held)
	w.step(w.resolve(held, "base-0", "open", "the pull request is still open on the same base", alpha.TaskID))
	w.contests()
	w.sameAsPython("mtn14_authority")
}

func Test26_MTN_15_the_pull_request_state_decides_an_unknown_outcome(t *testing.T) {
	t.Run("open unchanged", func(t *testing.T) {
		w := newFx(t)
		turn := w.unknown()
		w.step(w.resolve(turn, "base-0", "open", "still open, base unchanged", ""))
		w.sameAsPython("mtn15_open_unchanged")
	})
	t.Run("open moved", func(t *testing.T) {
		w := newFx(t)
		turn := w.unknown()
		w.merged("base-9")
		w.step(w.resolve(turn, "base-9", "open", "somebody else pushed; this one is still open", ""))
		w.sameAsPython("mtn15_open_moved")
	})
	t.Run("unreadable state", func(t *testing.T) {
		w := newFx(t)
		turn := w.unknown()
		w.step(w.resolve(turn, "base-9", "unknown", "could not read the pull request", ""))
		for _, seen := range []string{"base-0", "base-9"} {
			w.step(w.resolve(turn, seen, "mergd", "I think it merged", ""))
		}
		w.turn(turn)
		w.sameAsPython("mtn15_unreadable_state")
	})
	t.Run("merged", func(t *testing.T) {
		w := newFx(t)
		turn := w.unknown()
		w.step(w.resolve(turn, "base-0", "merged", "the pull request reads merged", ""))
		w.sameAsPython("mtn15_merged")
	})
}

// crossed is tests/fixtures/merge_turn_crossed_handoff.json, loaded from the Python tree.
type crossed struct {
	Repository string `json:"repository"`
	BaseRef    string `json:"baseRef"`
	Parents    []struct {
		Project, Task, Host, Head string
		PR                        int64 `json:"pr"`
	} `json:"parents"`
}

func Test26_MTN_18_the_crossed_handoff_replays_refused_where_it_should_be(t *testing.T) {
	raw, err := readFixture("merge_turn_crossed_handoff.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture crossed
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	w := newFx(t)
	w.target.set(fixture.Repository, fixture.BaseRef, "base-0")
	var turns []string
	for _, p := range fixture.Parents {
		w.bindParent(p.Project, ep(p.Task, p.Host, "/"+p.Task))
	}
	for _, p := range fixture.Parents {
		answer := w.step(w.m.Request(w.ctx, fixture.Repository, fixture.BaseRef, p.Project, p.Task, p.Host, p.Head, true, ClaimOptions{PR: nullInt(p.PR)}))
		turns = append(turns, answer.(map[string]any)["turnId"].(string))
	}
	w.answer(turns[0], "task-hierarchy")
	w.step(w.m.Check(w.ctx, turns[0], "task-hierarchy", "head-73", "base-0", runChecks("head-73", nil, 1, "dev-gate", "run-1"), green(), []string{"dev-gate"}, w.target))
	w.step(w.m.Target(w.ctx, fixture.Repository, fixture.BaseRef))
	w.step(w.m.Attest(w.ctx, turns[0], "return_requested", "return_requested:task-status", "task-status", "my candidate is green"))
	w.step(w.m.Attest(w.ctx, turns[0], "transport_accepted", "msg-69", "task-status", "the relay accepted the message"))
	w.step(w.m.Target(w.ctx, fixture.Repository, fixture.BaseRef))
	w.step(w.m.Release(w.ctx, turns[0], "task-hierarchy", "returned", "required CI has not finished and a peer is ready", ""))
	for _, turn := range turns[2:] {
		w.turn(turn)
	}
	w.sameAsPython("mtn18_crossed_handoff")
}

func Test26_MTN_21_land_reads_the_target(t *testing.T) {
	t.Run("records reading", func(t *testing.T) {
		w := newFx(t)
		turn := w.merging("head-a")
		w.merged(fxPost)
		w.step(w.land(turn, "merge-1", "", ""))
		w.sameAsPython("mtn21_landing_records_reading")
	})
	t.Run("stated agrees", func(t *testing.T) {
		w := newFx(t)
		turn := w.merging("head-a")
		w.merged(strings.Repeat("d", 40))
		w.step(w.land(turn, "merge-1", strings.Repeat("D", 40), ""))
		w.sameAsPython("mtn21_stated_agrees")
	})
	t.Run("trial mistake", func(t *testing.T) {
		w := newFx(t)
		turn := w.merging("head-a")
		w.merged("head-a")
		w.step(w.land(turn, "head-a", "base-0", ""))
		w.turn(turn)
		w.contests()
		w.sameAsPython("mtn21_trial_mistake")
	})
	t.Run("not advanced", func(t *testing.T) {
		w := newFx(t)
		turn := w.merging("head-a")
		w.step(w.land(turn, "merge-1", "", ""))
		w.turn(turn)
		w.merged(fxPost)
		w.step(w.land(turn, "merge-1", "", ""))
		w.sameAsPython("mtn21_not_advanced")
	})
	t.Run("already base", func(t *testing.T) {
		w := newFx(t)
		turn := w.held()
		w.target.set(fxRepo, fxBase, "head-a")
		w.step(w.check(turn, "head-a", "head-a", ""))
		w.step(w.land(turn, "head-a", "", ""))
		w.sameAsPython("mtn21_already_base")
	})
	t.Run("unreadable and blind", func(t *testing.T) {
		w := newFx(t)
		turn := w.merging("head-a")
		w.target.forget(fxRepo, fxBase)
		w.step(w.land(turn, "merge-1", "", ""))
		w.step(w.m.Land(w.ctx, turn, alpha.TaskID, "merge-1", "", "merged", nil))
		w.turn(turn)
		w.sameAsPython("mtn21_unreadable_and_blind")
	})
	t.Run("stranger never reads", func(t *testing.T) {
		w := newFx(t)
		turn := w.merging("head-a")
		w.reads()
		w.step(w.land(turn, "merge-1", "", "task-stranger"))
		w.reads()
		w.sameAsPython("mtn21_stranger_never_reads")
	})
	t.Run("r3 lands through resolve", func(t *testing.T) {
		w := newFx(t)
		turn := w.merging("head-a")
		w.exec("UPDATE merge_turns SET checked_base_sha = 'base-y' WHERE turn_id = ?", turn)
		w.exec("UPDATE merge_turn_ledger SET evidence = 'chk-legacy' WHERE turn_id = ? AND evidence_kind = 'currency_confirmed'", turn)
		w.step(w.land(turn, "merge-1", "", ""))
		w.step(w.m.Unknown(w.ctx, turn, alpha.TaskID, "checked before the relay read its base"))
		w.merged(fxPost)
		w.step(w.resolve(turn, fxPost, "merged", "the pull request reads merged", alpha.TaskID))
		w.sameAsPython("mtn21_r3_turn_lands_through_resolve")
	})
	t.Run("forged mark", func(t *testing.T) {
		w := newFx(t)
		turn := w.merging("head-a")
		w.exec("UPDATE merge_turn_ledger SET kind = 'attestation' WHERE turn_id = ? AND evidence_kind = 'currency_confirmed'", turn)
		w.merged(fxPost)
		w.step(w.land(turn, "merge-1", "", ""))
		w.sameAsPython("mtn21_forged_mark")
	})
}

func Test26_MTN_22_resolve_reads_the_target(t *testing.T) {
	t.Run("merged records reading", func(t *testing.T) {
		w := newFx(t)
		turn := w.unknown()
		w.merged(fxPost)
		w.step(w.resolve(turn, fxPost, "merged", "merged", ""))
		w.sameAsPython("mtn22_merged_records_reading")
	})
	t.Run("already contained", func(t *testing.T) {
		w := newFx(t)
		turn := w.unknown()
		w.step(w.resolve(turn, "base-0", "merged", "merged; the base already contained it", ""))
		w.sameAsPython("mtn22_already_contained")
	})
	t.Run("disagrees", func(t *testing.T) {
		w := newFx(t)
		turn := w.unknown()
		w.step(w.resolve(turn, "base-9", "open", "still open", ""))
		w.turn(turn)
		w.sameAsPython("mtn22_disagrees")
	})
	t.Run("unreadable open", func(t *testing.T) {
		w := newFx(t)
		turn := w.unknown()
		w.target.forget(fxRepo, fxBase)
		w.step(w.resolve(turn, "base-0", "open", "still open", ""))
		w.sameAsPython("mtn22_unreadable_open")
	})
	t.Run("unreadable merged", func(t *testing.T) {
		w := newFx(t)
		turn := w.unknown()
		w.target.forget(fxRepo, fxBase)
		w.step(w.resolve(turn, fxPost, "merged", "merged", ""))
		w.turn(turn)
		w.sameAsPython("mtn22_unreadable_merged")
	})
	t.Run("after unreadable return", func(t *testing.T) {
		w := newFx(t)
		turn := w.unknown()
		w.target.forget(fxRepo, fxBase)
		w.step(w.resolve(turn, "base-0", "closed", "closed unmerged", ""))
		second := w.heldOn(alpha, fxA, "head-c", fxBase)
		w.step(w.check(second, "head-c", "base-0", ""))
		w.target.set(fxRepo, fxBase, "base-0")
		w.step(w.check(second, "head-c", "base-0", ""))
		w.sameAsPython("mtn22_after_unreadable_return")
	})
}

func Test26_MTN_23_restate_base_corrects_a_landed_turns_recorded_base(t *testing.T) {
	t.Run("trial shape", func(t *testing.T) {
		w := newFx(t)
		first := w.landed("head-a", fxPost)
		w.r3Landing(first)
		second := w.heldOn(alpha, fxA, "head-c", fxBase)
		w.target.set(fxRepo, fxBase, fxPost)
		w.step(w.check(second, "head-c", fxPost, ""))
		w.step(w.restate(first, "", fxPost, ""))
		w.step(w.check(second, "head-c", fxPost, ""))
		w.merged("merge-2")
		w.step(w.land(second, "merge-2", "", ""))
		w.sameAsPython("mtn23_trial_shape")
	})
	t.Run("keeps original", func(t *testing.T) {
		w := newFx(t)
		first := w.landed("head-a", fxPost)
		w.r3Landing(first)
		w.step(w.restate(first, "", "", "git rev-parse main reads base-1"))
		w.rows("SELECT * FROM merge_turn_ledger WHERE turn_id = ? AND evidence_kind = 'landing_base_restated'", first)
		w.sameAsPython("mtn23_keeps_original")
	})
	t.Run("nothing to write", func(t *testing.T) {
		w := newFx(t)
		first := w.landed("head-a", fxPost)
		w.step(w.restate(first, "", "", ""))
		w.r3Landing(first)
		w.step(w.restate(first, "", "", ""))
		w.step(w.restate(first, "", "", ""))
		w.sameAsPython("mtn23_nothing_to_write")
	})
	t.Run("moved twice", func(t *testing.T) {
		w := newFx(t)
		first := w.landed("head-a", fxPost)
		w.target.set(fxRepo, fxBase, "base-2")
		w.step(w.restate(first, "", "", ""))
		w.target.set(fxRepo, fxBase, fxPost)
		w.step(w.restate(first, "", "", ""))
		w.sameAsPython("mtn23_moved_twice")
	})
	t.Run("supervisor and stranger", func(t *testing.T) {
		w := newFx(t)
		first := w.landed("head-a", fxPost)
		w.r3Landing(first)
		w.reads()
		w.step(w.restate(first, "task-stranger", "", ""))
		w.reads()
		w.step(w.restate(first, overseer.TaskID, "", ""))
		w.sameAsPython("mtn23_supervisor_and_stranger")
	})
	t.Run("value not read", func(t *testing.T) {
		w := newFx(t)
		first := w.landed("head-a", fxPost)
		w.r3Landing(first)
		w.step(w.restate(first, "", "base-0", ""))
		w.turn(first)
		w.sameAsPython("mtn23_value_not_read")
	})
	t.Run("only landed", func(t *testing.T) {
		w := newFx(t)
		turn := w.merging("head-a")
		w.step(w.restate(turn, "", "", ""))
		w.sameAsPython("mtn23_only_landed")
	})
	t.Run("only latest landing", func(t *testing.T) {
		w := newFx(t)
		first := w.landed("head-a", fxPost)
		second := w.heldOn(alpha, fxA, "head-c", fxBase)
		w.must(w.check(second, "head-c", fxPost, ""))
		w.merged("base-2")
		w.must(w.land(second, "merge-1", "", ""))
		w.step(w.restate(first, "", "", ""))
		w.sameAsPython("mtn23_only_latest_landing")
	})
	t.Run("in flight", func(t *testing.T) {
		w := newFx(t)
		first := w.landed("head-a", fxPost)
		second := w.heldOn(alpha, fxA, "head-c", fxBase)
		w.must(w.check(second, "head-c", fxPost, ""))
		w.target.set(fxRepo, fxBase, "base-2")
		w.step(w.restate(first, "", "", ""))
		w.turn(first)
		w.sameAsPython("mtn23_in_flight")
	})
	t.Run("states why", func(t *testing.T) {
		w := newFx(t)
		first := w.landed("head-a", fxPost)
		w.step(w.restate(first, "", "", " "))
		w.sameAsPython("mtn23_states_why")
	})
}

func Test26_MTN_24_landing_order_and_restatement_sequences_survive_odd_stores(t *testing.T) {
	twoLandings := func(w *fx) (string, string) {
		first := w.landed("head-a", fxPost)
		second := w.heldOn(alpha, fxA, "head-c", fxBase)
		w.must(w.check(second, "head-c", fxPost, ""))
		w.merged("base-2")
		w.must(w.land(second, "merge-1", "", ""))
		return first, second
	}
	t.Run("same instant order", func(t *testing.T) {
		w := newFx(t)
		first, _ := twoLandings(w)
		third := w.heldOn(alpha, fxA, "head-d", fxBase)
		w.step(w.check(third, "head-d", "base-2", ""))
		w.step(w.restate(first, "", "", ""))
		w.sameAsPython("mtn24_same_instant_order")
	})
	t.Run("latest restated", func(t *testing.T) {
		w := newFx(t)
		first, second := twoLandings(w)
		w.r3Landing(second)
		w.step(w.restate(second, "", "", ""))
		w.step(w.restate(first, "", "", ""))
		w.sameAsPython("mtn24_latest_restated")
	})
	t.Run("returned does not gate", func(t *testing.T) {
		w := newFx(t)
		w.landed("head-a", fxPost)
		second := w.heldOn(alpha, fxA, "head-c", fxBase)
		w.must(w.check(second, "head-c", fxPost, ""))
		w.must(w.m.Unknown(w.ctx, second, alpha.TaskID, "lost"))
		w.step(w.resolve(second, fxPost, "open", "still open", alpha.TaskID))
		w.target.set(fxRepo, fxBase, "base-3")
		third := w.heldOn(alpha, fxA, "head-d", fxBase)
		w.step(w.check(third, "head-d", "base-3", ""))
		w.sameAsPython("mtn24_returned_does_not_gate")
	})
	t.Run("too long key", func(t *testing.T) {
		w := newFx(t)
		first := w.landed("head-a", fxPost)
		w.r3Landing(first)
		for _, key := range []string{"restate-base:" + strings.Repeat("9", 4301), "restate-base:" + strings.Repeat("9", 18), "restate-base:1" + strings.Repeat("0", 18)} {
			w.legacyRow(first, "attestation", key, "transport_accepted", "old caller row", nil)
		}
		w.step(w.restate(first, "", "", ""))
		w.sameAsPython("mtn24_too_long_key")
	})
	t.Run("nineteen digits", func(t *testing.T) {
		w := newFx(t)
		first := w.landed("head-a", fxPost)
		w.legacyRow(first, "attestation", "restate-base:1000000000000000000", "transport_accepted", "old caller row", nil)
		body := fmt.Sprintf(`{"turnId": %q, "sequence": 1000000000000000001, "from": "base-x", "to": %q, "evidence": "earlier", "source": "fake"}`, first, fxPost)
		w.legacyRow(first, "transition", "restate-base:1000000000000000001", "landing_base_restated", body, "landed")
		w.r3Landing(first)
		w.step(w.restate(first, "", "", ""))
		w.sameAsPython("mtn24_nineteen_digits")
	})
	t.Run("legacy rows", func(t *testing.T) {
		w := newFx(t)
		first := w.landed("head-a", fxPost)
		w.r3Landing(first)
		w.legacyRow(first, "attestation", "restate-base:1", "transport_accepted", "squatted", nil)
		w.legacyRow(first, "attestation", "mine", "landing_base_restated", "free text", nil)
		w.legacyRow(first, "attestation", "restate-base:7", "landing_base_restated", fmt.Sprintf(`{"turnId": %q, "sequence": 7, "from": "x", "to": "y", "evidence": "forged"}`, first), nil)
		w.step(w.restate(first, "", "", ""))
		w.sameAsPython("mtn24_legacy_rows")
	})
}

func Test26_CCL_1_withdraw_matches_python(t *testing.T) {
	w := newFx(t)
	w.claim(alpha, fxA, "head-a")
	waiting := w.claim(beta, fxB, "head-b")["turnId"].(string)
	w.step(w.m.Withdraw(w.ctx, waiting, alpha.TaskID))
	w.step(w.m.Withdraw(w.ctx, waiting, beta.TaskID))
	w.step(w.m.Withdraw(w.ctx, waiting, beta.TaskID))
	w.step(w.m.Withdraw(w.ctx, "mtn-missing", beta.TaskID))
	w.sameAsPython("withdraw_waiting")
}

// committed counts the transactions one call commits (store fault hook: once per opened
// transaction, after its body and before COMMIT).
func (w *fx) committed(call func()) int {
	var mu sync.Mutex
	n := 0
	w.s.SetFaultHook(func() { mu.Lock(); n++; mu.Unlock() })
	defer w.s.SetFaultHook(nil)
	call()
	mu.Lock()
	defer mu.Unlock()
	return n
}

func Test26_CCT_2_one_bounded_write_then_an_answer(t *testing.T) {
	w := newFx(t)
	w.target.set(fxRepo, fxBase, "base-0")
	id := w.held()
	opened := map[string]int{}
	counts := []map[string]any{}
	record := func(n int) { counts = append(counts, map[string]any{"ok": float64(n)}) }
	opened["declare_ready"] = w.committed(func() { w.must(w.m.Ready(w.ctx, id, alpha.TaskID, true, "", "")) })
	record(opened["declare_ready"])
	opened["attest"] = w.committed(func() {
		w.must(w.m.Attest(w.ctx, id, "transport_accepted", "delivery-1", beta.TaskID, "accepted"))
	})
	record(opened["attest"])
	opened["begin_merge"] = w.committed(func() { w.must(w.begin(id, defaults())) })
	record(opened["begin_merge"])
	opened["refused land"] = w.committed(func() {
		if _, err := w.land(id, "merge-1", "", "task-stranger"); reasonOf(err) != "merge_turn_not_held" {
			t.Fatal(err)
		}
	})
	record(opened["refused land"])
	w.merged(fxPost)
	opened["land"] = w.committed(func() { w.must(w.land(id, "merge-1", fxPost, "")) })
	record(opened["land"])

	w2 := newFx(t)
	opened = map[string]int{}
	opened["request"] = w2.committed(func() { w2.claim(alpha, fxA, "head-a") })
	turn := w2.claim(alpha, fxA, "head-a")["turnId"].(string)
	w2.answer(turn, alpha.TaskID)
	opened["refused begin_merge"] = w2.committed(func() {
		b := defaults()
		b.head, b.checks = "head-moved", []any{}
		if _, err := w2.begin(turn, b); reasonOf(err) != "merge_candidate_moved" {
			t.Fatal(err)
		}
	})
	w2.must(w2.begin(turn, defaults()))
	w2.must(w2.m.Unknown(w2.ctx, turn, alpha.TaskID, "lost"))
	w2.merged(fxPost)
	opened["resolve_unknown"] = w2.committed(func() { w2.must(w2.resolve(turn, fxPost, "merged", "the pull request reads merged", alpha.TaskID)) })
	w2.merged("base-2")
	opened["restate_base"] = w2.committed(func() { w2.must(w2.restate(turn, "", "", "the branch moved")) })
	opened["turn"] = w2.committed(func() { w2.must(w2.m.Turn(w2.ctx, turn)) })
	record(opened["turn"])
	opened["target"] = w2.committed(func() { w2.must(w2.m.Target(w2.ctx, fxRepo, fxBase)) })
	opened["ledger"] = w2.committed(func() {
		if _, err := w2.s.All(w2.ctx, "SELECT * FROM merge_turn_ledger WHERE turn_id=?", turn); err != nil {
			t.Fatal(err)
		}
	})
	record(opened["ledger"])
	record(opened["target"])
	record(opened["refused begin_merge"])
	record(opened["request"])
	record(opened["resolve_unknown"])
	record(opened["restate_base"])
	opened["outstanding"] = w2.committed(func() {
		if _, err := w2.m.Outstanding(w2.ctx, alpha.TaskID); err != nil {
			t.Fatal(err)
		}
	})

	// Reading the target twice with the clock advanced answers the same.
	w3 := newFx(t)
	w3.claim(alpha, fxA, "head-a")
	before := jsonValue(t, w3.must(w3.m.Target(w3.ctx, fxRepo, fxBase)))
	w3.m.Now = func() string { return registry.ISO(time.Unix(1_700_000_000+1_000_000, 0)) }
	w3.r.Now = w3.m.Now
	if after := jsonValue(t, w3.must(w3.m.Target(w3.ctx, fxRepo, fxBase))); !reflect.DeepEqual(before, after) {
		t.Fatalf("target moved with the clock:\n%v\n%v", before, after)
	}
	counts = append(counts, map[string]any{"ok": true})
	all, err := pythonC()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(counts, all["cct2_counts"]) {
		t.Fatalf("Python transaction counts: Go %v, Python %v", counts, all["cct2_counts"])
	}
}
