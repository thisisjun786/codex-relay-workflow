package delivery

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// test_host_lost_turn.py HLT-1..HLT-29. Each Test21_HLT<n> runs the Go twin of every Python test
// the property lists (as a subtest named after it), in the Python test's own tree, and compares
// every asserted value, the delivery tables and the sends with what Python produced
// (hostloss_harness_test.go).

const hlt = "test_host_lost_turn"

func Test21_HLT01_a_lost_accepted_turn_is_redelivered_once_under_the_next_attempt(t *testing.T) {
	t.Run("turn missing from the list", func(t *testing.T) {
		mirror(t, hlt, "TheHostLostTheAcceptedTurn.test_the_daemon_redelivers_the_same_event_once_under_the_next_attempt", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostLoses(turn, true)
			h.clock.Advance(120)
			report := h.tick()
			attempts := h.attemptsFor(event)
			h.eq(h.attemptStates(event))
			h.eq([]any{attempts[0].S("event_id"), attempts[1].S("event_id")})
			h.eq(sendIDs(h))
			h.eq(first)
			h.eq(field(report.asDict(), "turnsLost"))
			h.eq(h.journalled(HostLostTurn))
			h.eq(field(h.statusOf(event), "phase"))
			for i := 0; i < 3; i++ {
				h.clock.Advance(120)
				h.tick()
			}
			h.eq(len(h.host.sends))
			h.eq(h.journalled(HostLostTurn))
		})
	})
	t.Run("turn in progress then missing", func(t *testing.T) {
		mirror(t, hlt, "TheHostLostTheAcceptedTurn.test_a_turn_first_read_in_progress_and_lost_later_is_still_caught", func(h *hl) {
			event, _, turn := h.dispatched()
			h.clock.Advance(120)
			h.tick()
			h.eq(h.attemptStates(event))
			h.hostLoses(turn, true)
			h.clock.Advance(120)
			h.tick()
			h.eq(h.attemptStates(event)[0])
		})
	})
	t.Run("turn relisted interrupted without its message", func(t *testing.T) {
		mirror(t, hlt, "TheHostListsALostTurnAfterAReload.test_a_turn_reloaded_interrupted_without_its_message_is_lost_and_redelivered_once", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostReloadsLosing(turn)
			h.clock.Advance(5)
			h.tick()
			h.eq(h.attemptStates(event))
			h.clock.Advance(120)
			h.tick()
			h.eq(h.attemptStates(event))
			h.eq(len(h.host.sends))
			h.eq(h.journalled(HostLostTurn))
			reading := sub(h.reconcile(first, h.host), "recipientTurn")
			h.eq(field(reading, "finding"))
			h.eq(strings.Contains(str(reading, "detail"), "interrupted"))
		})
	})
}

func sendIDs(h *hl) []any {
	out := []any{}
	for _, s := range h.host.sends {
		out = append(out, s.requestID)
	}
	return out
}

func Test21_HLT02_a_second_loss_holds_the_obligation_under_its_name(t *testing.T) {
	mirror(t, hlt, "TheHostLostTheAcceptedTurn.test_a_second_loss_holds_the_obligation_under_its_name_instead_of_a_third_send", func(h *hl) {
		event, _, turn := h.dispatched()
		h.hostLoses(turn, true)
		h.clock.Advance(120)
		h.tick()
		second := h.attemptsFor(event)[1]
		h.hostLoses(str(loadsObj(second.S("record")), "turnId"), true)
		for i := 0; i < 4; i++ {
			h.clock.Advance(120)
			h.tick()
		}
		h.eq(h.attemptStates(event))
		h.eq(len(h.host.sends))
		h.eq(h.row(event).Opt("hold_reason"))
		h.eq(h.statusPair(event))
	})
}

func Test21_HLT03_a_token_confirmed_completion_is_checked_like_an_accepted_one(t *testing.T) {
	t.Run("checked for host loss", func(t *testing.T) {
		mirror(t, hlt, "TheHostLostTheAcceptedTurn.test_a_completion_confirmed_by_its_token_is_checked_like_an_accepted_one", func(h *hl) {
			h.parentHistory()
			event := h.queuedEvent(regOpts{})
			h.host.script = []string{"in_progress"}
			request := str(h.attemptOn(event, h.host, nil), "requestId")
			turn := h.host.startTurn(parent, "", "completed", "..."+request+"...")
			h.eq(str(h.reconcile(request, h.host), "evidence"))
			h.eq(h.row(event).S("state"))
			h.eq(h.attemptStates(event))
			h.hostLoses(turn.TurnID, true)
			h.clock.Advance(120)
			h.eq(field(h.tick().asDict(), "turnsLost"))
			h.eq(h.attemptStates(event))
			h.eq(len(h.host.sends))
			lost := loadsObj(h.attemptsFor(event)[0].S("record"))
			h.eq(field(sub(lost, "reconciliation"), "affirmativeEvidence"))
			h.eq(field(lost, "deliveryState"))
			for i := 0; i < 3; i++ {
				h.clock.Advance(120)
				h.tick()
			}
			h.eq(len(h.host.sends))
		})
	})
	t.Run("a manual reconcile keeps turn_found", func(t *testing.T) {
		mirror(t, hlt, "TheHostLostTheAcceptedTurn.test_a_manual_reconcile_keeps_a_token_confirmed_send_confirmed", func(h *hl) {
			event, request, turn := h.tokenConfirmed()
			h.hostLoses(turn, true)
			h.clock.Advance(5)
			h.eq(field(h.reconcile(request, h.host), "evidence"))
			h.eq(h.evidenceOf(event))
			h.eq(h.row(event).S("state"))
			h.clock.Advance(120)
			h.tick()
			h.eq(h.attemptStates(event))
			h.eq(h.evidenceOf(event)[0])
			h.eq(field(sub(loadsObj(h.attemptsFor(event)[0].S("record")), "reconciliation"), "affirmativeEvidence"))
		})
	})
	t.Run("a recorded loss reports its own evidence", func(t *testing.T) {
		mirror(t, hlt, "TheHostLostTheAcceptedTurn.test_reconciling_a_lost_token_confirmed_attempt_reports_its_own_evidence", func(h *hl) {
			event, request, turn := h.tokenConfirmed()
			h.hostLoses(turn, true)
			h.clock.Advance(120)
			h.tick()
			h.eq(h.attemptStates(event)[0])
			again := h.reconcile(request, h.host)
			h.eq([]any{field(again, "state"), field(again, "evidence")})
			h.eq(len(h.host.sends))
		})
	})
}

func Test21_HLT05_a_tick_whose_only_change_is_the_loss_is_not_quiet(t *testing.T) {
	mirror(t, hlt, "TheHostLostTheAcceptedTurn.test_a_tick_whose_only_change_is_the_loss_is_not_quiet", func(h *hl) {
		event, _, turn := h.dispatched()
		h.hostLoses(turn, true)
		h.clock.Advance(120)
		policy := defaultTick()
		policy.maxSendsTick = 0
		report := h.tickWith(policy, h.host, h.checks)
		counters := report.asDict()
		h.eq(field(counters, "turnsLost"))
		others := map[string]any{}
		for _, f := range counters {
			if f.Key != "turnsLost" && f.Key != "notes" && f.Key != "skipped" && f.Key != "quiet" {
				others[f.Key] = f.Value
			}
		}
		h.eq(others)
		h.eq(report.quiet())
		h.eq(h.statusPair(event))
		detail := field(h.statusOf(event), "attemptDetail").([]any)
		h.eq(detail[0].(Row).S("state"))
		messages, err := h.delivery.AttemptMessages(h.ctx, event)
		mustDo(t, err)
		statuses := []any{}
		for _, m := range messages {
			statuses = append(statuses, field(m.(Obj), "status"))
		}
		h.eq(statuses)
	})
}

func Test21_HLT06_a_send_too_recent_to_judge_is_unknown_until_the_allowance_has_passed(t *testing.T) {
	mirror(t, hlt, "TheHostLostTheAcceptedTurn.test_a_send_too_recent_to_judge_is_unknown_until_the_allowance_has_passed", func(h *hl) {
		event, first, turn := h.dispatched()
		h.hostLoses(turn, true)
		h.clock.Advance(5)
		h.eq(field(sub(h.reconcile(first, h.host), "recipientTurn"), "finding"))
		h.eq(h.row(event).S("state"))
		h.clock.Advance(120)
		h.eq(field(sub(h.reconcile(first, h.host), "recipientTurn"), "finding"))
	})
}

func Test21_HLT07_the_turn_check_budget_reaches_every_delivery_and_reads_finished_turns_once(t *testing.T) {
	t.Run("budget 2 per tick over 5 deliveries", func(t *testing.T) {
		mirror(t, hlt, "TheHostLostTheAcceptedTurn.test_the_budget_bounds_lookups_and_every_delivery_is_reached", func(h *hl) {
			h.parentHistory()
			var turns []string
			for i := 0; i < 5; i++ {
				rid := h.register(regOpts{issue: fmt.Sprintf("REL-%d", i+2), dispatchRequest: fmt.Sprintf("dispatch-%d", i+2)})
				h.rid = rid
				payload := h.readyPayload(rid, 1, []string{h.artifact(fmt.Sprintf("out-%d.txt", i), fmt.Sprintf("deliverable %d", i))}, 1, assigned("completed"))
				_, err := h.accept(payload, storeAcceptNone)
				mustDo(t, err)
				_, err = h.delivery.Enqueue(h.ctx, str(payload, "eventId"), "", "")
				mustDo(t, err)
				record := h.attemptOn(str(payload, "eventId"), h.host, nil)
				h.eq(str(record, "deliveryState"))
				h.eq(field(record, "attemptNo"))
				turns = append(turns, str(record, "turnId"))
				h.clock.Advance(10)
			}
			for _, turn := range turns[:2] {
				h.host.finishTurn(parent, turn, "completed")
			}
			var lookups []string
			counting := countingLookups(h.host, &lookups)
			policy := tickPolicy{2, 0, 8}
			checks := &TurnChecks{Reconciler: h.rc}
			var perTick []any
			for i := 0; i < 3; i++ {
				before := len(lookups)
				h.tickWith(policy, counting, checks)
				perTick = append(perTick, len(lookups)-before)
			}
			h.eq(perTick)
			h.eq(sortedSet(lookups))
			for i := 0; i < 3; i++ {
				h.tickWith(policy, counting, checks)
			}
			h.eq([]any{countOf(lookups, turns[0]), countOf(lookups, turns[1])})
			ok := true
			for _, turn := range turns[2:] {
				ok = ok && countOf(lookups, turn) > 1
			}
			h.eq(ok)
			h.eq(len(h.host.sends))
		})
	})
	t.Run("finished turns read once across pages", func(t *testing.T) {
		mirror(t, hlt, "TheHostLostTheAcceptedTurn.test_a_finished_turn_is_read_once_even_when_the_candidates_span_pages", func(h *hl) {
			h.parentHistory()
			var turns []string
			for i := 0; i < 5; i++ {
				rid := h.register(regOpts{issue: fmt.Sprintf("REL-%d", i+2), dispatchRequest: fmt.Sprintf("dispatch-%d", i+2)})
				h.rid = rid
				payload := h.readyPayload(rid, 1, []string{h.artifact(fmt.Sprintf("out-%d.txt", i), fmt.Sprintf("deliverable %d", i))}, 1, assigned("completed"))
				_, err := h.accept(payload, storeAcceptNone)
				mustDo(t, err)
				_, err = h.delivery.Enqueue(h.ctx, str(payload, "eventId"), "", "")
				mustDo(t, err)
				turns = append(turns, str(h.attemptOn(str(payload, "eventId"), h.host, nil), "turnId"))
				h.clock.Advance(10)
			}
			for _, turn := range turns {
				h.host.finishTurn(parent, turn, "completed")
			}
			var lookups []string
			counting := countingLookups(h.host, &lookups)
			checks := &TurnChecks{Reconciler: h.rc, Page: 2}
			for i := 0; i < 8; i++ {
				h.tickWith(tickPolicy{2, 0, 8}, counting, checks)
			}
			h.eq(sortedList(lookups))
		})
	})
	t.Run("a loss behind two pages of finished turns", func(t *testing.T) {
		mirror(t, hlt, "TheHostLostTheAcceptedTurn.test_a_delivery_behind_two_pages_of_finished_turns_is_still_reached", func(h *hl) {
			delivered := h.completions(5)
			for _, d := range delivered[:4] {
				h.host.finishTurn(parent, d[2], "completed")
			}
			lostEvent, lostTurn := delivered[4][0], delivered[4][2]
			h.hostLoses(lostTurn, true)
			h.clock.Advance(120)
			checks := &TurnChecks{Reconciler: h.rc, Page: 2}
			for i := 0; i < 6; i++ {
				h.tickWith(tickPolicy{4, 4, 8}, h.host, checks)
				h.clock.Advance(20)
			}
			h.eq(h.attemptStates(lostEvent))
			h.eq(len(h.host.sends))
		})
	})
	t.Run("the finished-turn memory is pruned", func(t *testing.T) {
		mirror(t, hlt, "TheHostLostTheAcceptedTurn.test_the_finished_turns_remembered_are_only_those_still_awaiting", func(h *hl) {
			delivered := h.completions(5)
			for _, d := range delivered {
				h.host.finishTurn(parent, d[2], "completed")
			}
			h.clock.Advance(120)
			checks := &TurnChecks{Reconciler: h.rc, Page: 2}
			policy := tickPolicy{4, 0, 8}
			for i := 0; i < 4; i++ {
				h.tickWith(policy, h.host, checks)
			}
			acknowledged := []string{delivered[0][1], delivered[1][1], delivered[2][1]}
			subset := true
			for _, id := range acknowledged {
				subset = subset && contains(checks.Settled(), id)
			}
			h.eq(subset)
			for i, d := range delivered[:3] {
				h.acknowledge(d[0], fmt.Sprintf("ack-%d", i))
			}
			for i := 0; i < 4; i++ {
				h.clock.Advance(20)
				h.tickWith(policy, h.host, checks)
			}
			left := []string{}
			for _, id := range acknowledged {
				if contains(checks.Settled(), id) {
					left = append(left, id)
				}
			}
			h.eq(sortedSet(left))
			h.eq(len(h.host.sends))
		})
	})
}

func countOf(list []string, v string) int {
	n := 0
	for _, x := range list {
		if x == v {
			n++
		}
	}
	return n
}

// sortedSet is plain(set(...)) in capture.py: sorted by JSON text, duplicates dropped.
func sortedSet(list []string) []any {
	seen := map[string]bool{}
	var uniq []string
	for _, v := range list {
		if !seen[v] {
			seen[v] = true
			uniq = append(uniq, v)
		}
	}
	return sortedList(uniq)
}

func sortedList(list []string) []any {
	keyed := make([]string, len(list))
	copy(keyed, list)
	for i := range keyed {
		for j := i + 1; j < len(keyed); j++ {
			a, _ := json.Marshal(keyed[i])
			b, _ := json.Marshal(keyed[j])
			if string(b) < string(a) {
				keyed[i], keyed[j] = keyed[j], keyed[i]
			}
		}
	}
	out := make([]any, len(keyed))
	for i, v := range keyed {
		out[i] = v
	}
	return out
}
