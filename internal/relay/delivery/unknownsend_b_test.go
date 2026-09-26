package delivery

import (
	"context"
	"database/sql"
	"math"
	"regexp"
	"sort"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
)

// test_unknown_send_lost.py USL-12..USL-22 (method: hostloss_harness_test.go). USL-18 is
// python-internal (cli.Services wiring by identity) and not ported.

func Test21_USL12_a_persisting_hold_opens_its_fault_as_broken(t *testing.T) {
	const cls = "APersistingHoldReachesTheOperator."
	t.Run("lost", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_lost_hold_opens_its_fault_instead_of_staying_observed", func(h *hl) {
			h.unknownSend(true, true)
			h.ticks(1, 120)
			h.eq(h.faultIn("open", "broken"))
		})
	})
	t.Run("undecided", func(t *testing.T) {
		mirror(t, usl, cls+"test_an_undecided_hold_opens_its_fault_instead_of_staying_observed", func(h *hl) {
			h.unknownSend(false, true)
			h.ticks(1, 120)
			h.eq(h.faultIn("open", "broken"))
		})
	})
	t.Run("within the allowance", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_wait_the_daemon_still_ends_stays_below_the_threshold", func(h *hl) {
			h.unknownSend(true, true)
			h.ticks(1, 20)
			h.eq(h.faultIn("open", "broken"))
		})
	})
}

func Test21_USL13_a_revision_request_left_without_trace_is_held_not_resent(t *testing.T) {
	mirror(t, usl, "ACorrectionLostTheSameWayIsTheParentsToRecover.test_a_revision_request_left_without_trace_is_held_not_resent", func(h *hl) {
		_, correction := h.correctionAfterNeedsChanges()
		h.host.startTurn(child, "child-earlier", "completed", "")
		h.clock.Advance(300)
		h.host.script = []string{"transport_unknown"}
		record := h.attemptOn(correction, h.host, nil)
		h.eq(h.row(correction).S("state"))
		h.host.threads[child].status = "notLoaded"
		h.clock.Advance(120)
		outcome := h.reconcile(str(record, "requestId"), h.host)
		h.eq(field(outcome, "nextExpectedAction"))
		h.eq(argvTail(field(sub(outcome, "recovery"), "command")))
		row := h.row(correction)
		h.eq([]any{row.S("state"), row.Opt("hold_reason")})
		h.ticks(3, 700)
		h.eq(h.sendsTo(child))
		h.eq(h.nextAction())
		h.eq(argvTail(field(sub(h.assignment(), "recovery"), "command")))
	})
}

func (h *hl) reading(event string) []any {
	return []any{h.attemptStates(event), field(h.statusOf(event), "phase"), h.nextAction()}
}

// fillWindow is UnknownSendCase.fill_window.
func (h *hl) fillWindow(now float64) float64 {
	window := math.Floor(now/3600) * 3600
	h.exec("INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at) VALUES (?,?,?,?)", parent, int64(window), h.delivery.Policy.MaxSendsPerRecipientPerHour, now-600)
	return window
}

func Test21_USL14_the_four_K5_cases_read_apart(t *testing.T) {
	const cls = "TheFourCasesReadApart."
	t.Run("death before the answer, turn lost", func(t *testing.T) {
		mirror(t, usl, cls+"test_death_before_the_answer_with_the_turn_lost", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.clock.Advance(120)
			h.reconcile(first, h.host)
			h.eq(h.reading(event))
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("death before the answer, turn kept", func(t *testing.T) {
		mirror(t, usl, cls+"test_death_before_the_answer_with_the_turn_run_and_kept", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.clock.Advance(3)
			h.host.startTurn(parent, "", "completed", uslMessage(first))
			h.clock.Advance(120)
			h.reconcile(first, h.host)
			h.eq(h.reading(event))
			h.eq(h.evidenceOf(event))
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("death after the answer, accepted turn lost", func(t *testing.T) {
		mirror(t, usl, cls+"test_death_after_the_answer_with_the_accepted_turn_lost", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostLoses(turn, true)
			h.clock.Advance(120)
			h.reconcile(first, h.host)
			h.eq(h.reading(event))
			row := h.row(event)
			h.eq([]any{row.S("state"), row.Opt("hold_reason"), row.Opt("dispatch_evidence")})
		})
	})
	t.Run("the hourly cap", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_delivery_waiting_on_the_hourly_cap", func(h *hl) {
			event := h.queuedEvent(regOpts{})
			now := h.clock.Now()
			h.fillWindow(now)
			h.eq(h.attemptOn(event, h.host, &now))
			h.eq(h.reading(event))
			h.eq(field(sub(h.statusOf(event), "pacing"), "reopensAt"))
			h.eq(sendsJSON(h.host))
		})
	})
}

func (h *hl) capped() (string, float64, float64) {
	event := h.queuedEvent(regOpts{})
	now := h.clock.Now()
	return event, now, h.fillWindow(now)
}

func pacingOf(item Obj) Obj { return sub(item, "pacing") }

func Test21_USL15_the_hourly_cap_is_named_with_its_reopen_time(t *testing.T) {
	const cls = "TheHourlyCapIsNamedWithItsReopenTime."
	t.Run("status names the cap and a reopen time that does not move", func(t *testing.T) {
		mirror(t, usl, cls+"test_status_names_the_cap_and_a_reopen_time_that_does_not_move", func(h *hl) {
			event, now, _ := h.capped()
			h.eq(h.attemptOn(event, h.host, &now))
			item := h.statusOf(event)
			p := pacingOf(item)
			h.eq([]any{field(p, "reason"), field(p, "reopensAt")})
			h.eq([]any{field(p, "sends"), field(p, "cap")})
			h.eq(field(item, "nextEligibleAt"))
			h.eq(field(item, "holdReason"))
			h.clock.Advance(30)
			h.eq(h.attemptOn(event, h.host, at(h.clock.Now())))
			h.eq(field(pacingOf(h.statusOf(event)), "reopensAt"))
			h.clock.Advance(60)
			h.eq(h.attemptOn(event, h.host, at(h.clock.Now())))
			h.eq(field(pacingOf(h.statusOf(event)), "reopensAt"))
			h.eq(sendsJSON(h.host))
		})
	})
	t.Run("assignment-show names the cap", func(t *testing.T) {
		mirror(t, usl, cls+"test_assignment_show_names_the_cap_and_its_reopen_time", func(h *hl) {
			event, now, _ := h.capped()
			h.eq(h.attemptOn(event, h.host, &now))
			p := sub(h.completionDelivery(), "pacing")
			h.eq([]any{field(p, "reason"), field(p, "reopensAt")})
			h.eq(h.nextAction())
		})
	})
	t.Run("a claim refused on the cap reads the budget again within a minute", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_claim_refused_on_the_cap_reads_the_budget_again_within_a_minute", func(h *hl) {
			event, now, _ := h.capped()
			h.delivery.RateLimited = func(string, float64) bool { return false }
			h.eq(h.attemptOn(event, h.host, &now))
			h.eq(h.row(event).Opt("next_eligible_at"))
		})
	})
	t.Run("a spent cap is named before the gap", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_spent_cap_is_named_before_the_gap_when_both_hold", func(h *hl) {
			event := h.queuedEvent(regOpts{})
			now := h.clock.Now()
			window := math.Floor(now/3600) * 3600
			h.exec("INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at) VALUES (?,?,?,?)", parent, int64(window), h.delivery.Policy.MaxSendsPerRecipientPerHour, now-1)
			h.eq(h.attemptOn(event, h.host, &now))
			item := h.statusOf(event)
			p := pacingOf(item)
			h.eq([]any{field(p, "reason"), field(p, "reopensAt"), field(item, "phase"), field(item, "nextEligibleAt")})
		})
	})
	t.Run("the delivery goes out when the window reopens", func(t *testing.T) {
		mirror(t, usl, cls+"test_the_delivery_goes_out_when_the_window_reopens", func(h *hl) {
			event, now, window := h.capped()
			h.eq(h.attemptOn(event, h.host, &now))
			h.clock.Advance(window + 3600 - now)
			record := h.attemptOn(event, h.host, nil)
			h.eq(field(record, "deliveryState"))
			h.eq(field(h.statusOf(event), "pacing"))
		})
	})
}

func Test21_USL16_a_raised_or_lifted_cap_releases_the_delivery_within_a_minute(t *testing.T) {
	const cls = "TheHourlyCapIsNamedWithItsReopenTime."
	t.Run("cap raised", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_raised_cap_releases_the_delivery_within_a_minute", func(h *hl) {
			event, now, window := h.capped()
			h.eq(h.attemptOn(event, h.host, &now))
			raised := NewService(h.store, h.clock)
			raised.Policy.MaxSendsPerRecipientPerHour = 24
			h.clock.Advance(60)
			h.eq(h.clock.Now() < window+3600)
			record, err := raised.Attempt(h.ctx, event, h.host, at(h.clock.Now()), "")
			mustDo(t, err)
			h.eq(field(record, "deliveryState"))
		})
	})
	t.Run("zero cap lifted", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_lifted_zero_cap_releases_the_delivery_within_a_minute", func(h *hl) {
			zero := NewService(h.store, h.clock)
			zero.Policy.MaxSendsPerRecipientPerHour = 0
			event := h.queuedEvent(regOpts{})
			now := h.clock.Now()
			record, err := zero.Attempt(h.ctx, event, h.host, &now, "")
			mustDo(t, err)
			h.eq(record)
			h.clock.Advance(60)
			h.eq(field(h.attemptOn(event, h.host, at(h.clock.Now())), "deliveryState"))
		})
	})
}

func Test21_USL17_a_cap_of_zero_names_the_operator(t *testing.T) {
	const cls = "TheHourlyCapIsNamedWithItsReopenTime."
	t.Run("pacing says only a changed policy reopens it", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_cap_of_zero_says_nothing_reopens_it_but_a_changed_policy", func(h *hl) {
			policy := DefaultPolicy()
			policy.MaxSendsPerRecipientPerHour = 0
			p := policy.Pacing(h.clock.Now(), 0, nil)
			h.eq([]any{field(p, "reason"), field(p, "reopensAt")})
			h.eq(regexp.MustCompile("changed policy").MatchString(str(p, "detail")))
		})
	})
	t.Run("completion", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_cap_of_zero_is_read_again_each_minute_and_names_the_operator", func(h *hl) {
			zero := NewService(h.store, h.clock)
			zero.Policy.MaxSendsPerRecipientPerHour = 0
			event := h.queuedEvent(regOpts{})
			now := h.clock.Now()
			record, err := zero.Attempt(h.ctx, event, h.host, &now, "")
			mustDo(t, err)
			h.eq(record)
			h.eq(h.row(event).Opt("next_eligible_at"))
			h.delivery = zero
			h.eq(h.nextAction())
			h.eq(sendsJSON(h.host))
		})
	})
	t.Run("correction", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_zero_cap_names_the_operator_for_a_correction", func(h *hl) {
			h.correctionAfterNeedsChanges()
			h.delivery.Policy.MaxSendsPerRecipientPerHour = 0
			h.eq(h.nextAction())
		})
	})
}

func Test21_USL19_a_delivery_held_by_its_own_later_backoff_is_not_paced(t *testing.T) {
	mirror(t, usl, "TheHourlyCapIsNamedWithItsReopenTime.test_a_delivery_held_by_its_own_later_backoff_is_not_reported_as_paced", func(h *hl) {
		event, _, window := h.capped()
		later := window + 3600 + 1800
		h.exec("UPDATE deliveries SET state = 'deferred_busy', next_eligible_at = ? WHERE event_id = ?", later, event)
		item := h.statusOf(event)
		h.eq([]any{field(item, "pacing"), field(item, "phase"), field(item, "nextEligibleAt")})
		h.eq(field(h.completionDelivery(), "pacing"))
	})
}

// ---------------------------------------------------------------- USL-20, USL-21: the fault

func (h *hl) stall() Row {
	return h.one("SELECT * FROM fault_ledger WHERE fault_class = 'delivery_stalled'")
}

var namingSeq = regexp.MustCompile(`:held:(\w+):\d+$`)

func (h *hl) keys() []any {
	rows, err := all(h.ctx, h.store, "SELECT o.occurrence_key FROM fault_occurrences o  JOIN fault_ledger f ON f.fault_id = o.fault_id WHERE f.fault_class = 'delivery_stalled'")
	mustDo(h.t, err)
	var keys []string
	for _, r := range rows {
		keys = append(keys, namingSeq.ReplaceAllString(r.S("occurrence_key"), ":held:$1:#"))
	}
	sort.Strings(keys)
	return anySlice(keys)
}

func (h *hl) publications() []any {
	rows, err := all(h.ctx, h.store, "SELECT p.trigger_key, p.summary FROM fault_publications p  JOIN fault_ledger f ON f.fault_id = p.fault_id WHERE f.fault_class = 'delivery_stalled' ORDER BY p.rowid")
	mustDo(h.t, err)
	out := []any{}
	for _, r := range rows {
		out = append(out, []any{r.S("trigger_key"), r.S("summary")})
	}
	return out
}

// assertBrokenNaming is assert_broken_naming: the fault states, then "is held: <hold>" in the
// stall's detail.
func (h *hl) assertBrokenNaming(hold string) {
	h.eq(h.faultStates())
	h.eq(regexp.MustCompile(regexp.QuoteMeta("is held: " + hold)).MatchString(h.stall().S("detail")))
}

func publishedWith(pubs []any, trigger string) []string {
	var out []string
	for _, p := range pubs {
		pair := p.([]any)
		if pair[0] == trigger {
			out = append(out, pair[1].(string))
		}
	}
	return out
}

func Test21_USL20_a_hold_reaches_its_fault_whatever_the_sweep_saw_first(t *testing.T) {
	const cls = "AHoldReachesItsFaultWhateverTheSweepSawFirst."
	t.Run("lost after the sweep", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_hold_named_after_the_sweep_recorded_its_attempt_breaks_the_fault", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.clock.Advance(20)
			h.eq(h.faultStates())
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.assertBrokenNaming(UnknownSendLost)
			h.eq(h.keys())
			h.eq(h.stall().I("occurrence_count"))
			opened := publishedWith(h.publications(), "open")
			h.eq(len(opened) > 0)
			h.eq(regexp.MustCompile("severity: broken").MatchString(opened[len(opened)-1]))
			h.eq(regexp.MustCompile("is held: " + UnknownSendLost).MatchString(opened[len(opened)-1]))
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("undecided after the sweep", func(t *testing.T) {
		mirror(t, usl, cls+"test_an_undecided_hold_named_after_the_sweep_breaks_the_fault_too", func(h *hl) {
			event, first := h.unknownSend(false, true)
			h.clock.Advance(20)
			h.eq(h.faultStates())
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.assertBrokenNaming(UnknownSendUndecided)
		})
	})
	t.Run("an open degraded fault escalates", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_fault_already_open_degraded_escalates_when_the_hold_is_named", func(h *hl) {
			h.unknownSend(true, true)
			h.clock.Advance(20)
			sw := h.sweeper()
			batch, err := sw.Sweep(h.ctx, "crw")
			mustDo(t, err)
			var seen []faults.Observation
			for _, o := range batch.Observations {
				if o.FaultClass == "delivery_stalled" {
					seen = append(seen, o)
				}
			}
			keys := []any{}
			for _, o := range seen {
				keys = append(keys, o.OccurrenceKey)
			}
			h.eq(keys)
			ledger := &faults.Ledger{Store: h.store, Clock: h.clock}
			mustDo(t, sw.RecordAll(h.ctx, ledger, batch))
			for _, n := range []int{1, 2} {
				o := seen[0]
				o.OccurrenceKey = "delivery:earlier-" + string(rune('0'+n))
				_, err := ledger.Record(h.ctx, o)
				mustDo(t, err)
			}
			h.eq(h.faultStates())
			h.ticks(1, 120)
			h.assertBrokenNaming(UnknownSendLost)
			escalated := publishedWith(h.publications(), "escalate:broken")
			h.eq(len(escalated) > 0)
			h.eq(regexp.MustCompile("is held: " + UnknownSendLost).MatchString(escalated[len(escalated)-1]))
		})
	})
	t.Run("named before any sweep", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_hold_named_before_any_sweep_keeps_its_name_through_later_sweeps", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.assertBrokenNaming(UnknownSendLost)
			h.clock.Advance(700)
			h.assertBrokenNaming(UnknownSendLost)
			h.eq(h.keys())
			h.eq(h.stall().I("occurrence_count"))
		})
	})
	t.Run("two host losses", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_host_lost_turn_held_after_two_losses_keeps_its_keys", func(h *hl) {
			event, _, turn := h.dispatched()
			h.hostLoses(turn, true)
			h.ticks(1, 120)
			second := h.attemptsFor(event)[1]
			h.hostLoses(str(loadsObj(second.S("record")), "turnId"), true)
			h.ticks(4, 120)
			h.eq(h.row(event).Opt("hold_reason"))
			h.faultStates()
			h.eq(h.keys())
		})
	})
	t.Run("an unknown send after a host loss", func(t *testing.T) {
		mirror(t, usl, cls+"test_an_unknown_send_held_after_a_host_loss_reads_both_attempts", func(h *hl) {
			event, _, turn := h.dispatched()
			h.hostLoses(turn, true)
			h.host.script = []string{"transport_unknown"}
			h.ticks(1, 120)
			second := h.attemptsFor(event)[1].S("request_id")
			h.host.threads[parent].status = "notLoaded"
			h.ticks(3, 120)
			h.assertHeld(event, second)
			h.faultStates()
			h.eq(h.keys())
		})
	})
}

func (h *hl) dropTurn(turn string) {
	t := h.host.threads[parent]
	kept := t.turns[:0:0]
	for _, one := range t.turns {
		if one.TurnID != turn {
			kept = append(kept, one)
		}
	}
	t.turns = kept
	h.dropItems(turn)
}

func Test21_USL21_a_hold_that_changes_name_stays_on_its_one_fault(t *testing.T) {
	const cls = "AHoldReachesItsFaultWhateverTheSweepSawFirst."
	t.Run("undecided then lost", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_hold_that_changes_name_updates_the_one_fault_it_is_on", func(h *hl) {
			event, first := h.unknownSend(false, true)
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.assertBrokenNaming(UnknownSendUndecided)
			h.publications()
			h.host.startTurn(parent, "", "completed", "the operator asked something")
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.assertBrokenNaming(UnknownSendLost)
			h.eq(h.keys())
			h.eq(h.count("SELECT COUNT(*) AS c FROM fault_ledger WHERE fault_class = 'delivery_stalled'"))
			h.eq(h.publications())
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("returns to an earlier name", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_hold_that_returns_to_an_earlier_name_names_it_again", func(h *hl) {
			event, first := h.unknownSend(false, true)
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.assertBrokenNaming(UnknownSendUndecided)
			turn := h.host.startTurn(parent, "", "completed", "the operator asked something")
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.assertBrokenNaming(UnknownSendLost)
			h.dropTurn(turn.TurnID)
			h.ticks(1, 700)
			h.assertHeld(event, first)
			h.assertBrokenNaming(UnknownSendUndecided)
			h.eq(h.keys())
			h.eq(h.stall().I("occurrence_count"))
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("namings between two sweeps", func(t *testing.T) {
		mirror(t, usl, cls+"test_namings_between_two_sweeps_leave_the_standing_name", func(h *hl) {
			event, first := h.unknownSend(false, true)
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.assertBrokenNaming(UnknownSendUndecided)
			turn := h.host.startTurn(parent, "", "completed", "the operator asked something")
			h.reconcile(first, h.host)
			h.assertHeld(event, first)
			h.dropTurn(turn.TurnID)
			h.reconcile(first, h.host)
			h.assertHeld(event, first)
			h.assertBrokenNaming(UnknownSendUndecided)
			h.eq(h.keys())
			h.eq(h.stall().I("occurrence_count"))
			rows, err := all(h.ctx, h.store, "SELECT detail FROM journal WHERE kind = 'unknown_send_hold_named' AND subject = ? ORDER BY seq", first)
			mustDo(t, err)
			named := []any{}
			for _, r := range rows {
				named = append(named, field(loadsObj(r.S("detail")), "hold"))
			}
			h.eq(named)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("readings that keep the name", func(t *testing.T) {
		mirror(t, usl, cls+"test_readings_that_keep_the_name_record_nothing_new", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.assertBrokenNaming(UnknownSendLost)
			h.keys()
			h.ticks(3, 700)
			h.assertHeld(event, first)
			h.assertBrokenNaming(UnknownSendLost)
			h.eq(h.keys())
			h.eq(h.stall().I("occurrence_count"))
			h.eq(h.sendsTo(parent))
		})
	})
}

func Test21_USL22_a_superseded_hold_names_the_supersession(t *testing.T) {
	const cls = "ASupersededHoldNamesTheSupersession."
	openGenerationTwo := func(h *hl) {
		h.host.startTurn(child, "turn-dispatch-2", "inProgress", "")
		h.openGeneration("dispatch-2", "needs_changes_revision", "turn-dispatch-2")
	}
	t.Run("held then a new generation", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_held_send_a_new_generation_replaced_names_the_supersession", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.ticks(1, 120)
			h.assertHeld(event, first)
			openGenerationTwo(h)
			outcome := h.reconcile(first, h.host)
			h.eq([]any{field(outcome, "nextExpectedAction"), field(outcome, "reason")})
			_, has := get(outcome, "recovery")
			h.eq(has)
			h.eq(h.statusPair(event))
			h.ticks(3, 700)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("superseded before a hold is named", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_send_superseded_before_its_hold_is_named_reports_the_supersession", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.clock.Advance(20)
			openGenerationTwo(h)
			h.eq(h.statusPair(event))
			outcome := h.reconcile(first, h.host)
			h.eq([]any{field(outcome, "nextExpectedAction"), field(outcome, "reason")})
			_, has := get(outcome, "recovery")
			h.eq(has)
			h.eq(h.sendsTo(parent))
		})
	})
	answered := func(h *hl) (string, string) {
		_, correction := h.correctionAfterNeedsChanges()
		h.host.startTurn(child, "child-earlier", "completed", "")
		h.clock.Advance(300)
		h.host.script = []string{"transport_unknown"}
		record := h.attemptOn(correction, h.host, nil)
		h.host.threads[child].status = "notLoaded"
		h.clock.Advance(120)
		h.reconcile(str(record, "requestId"), h.host)
		row := h.row(correction)
		h.eq([]any{row.S("state"), row.Opt("hold_reason")})
		h.host.startTurn(child, "child-late", "failed", "")
		_, err := BindAnchor(h.ctx, h.store, h.clock, h.rid, 2, "child-late")
		mustDo(h.t, err)
		payload := h.executionPayload(h.rid, 2, "failed", 1, turnRef{child, "child-late", "failed"})
		_, err = h.accept(payload, storeAcceptNone)
		mustDo(h.t, err)
		_, err = h.delivery.Enqueue(h.ctx, str(payload, "eventId"), "", "")
		mustDo(h.t, err)
		return correction, str(record, "requestId")
	}
	t.Run("an answered correction", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_held_correction_its_generation_answered_names_the_child_disposition", func(h *hl) {
			_, request := answered(h)
			outcome := h.reconcile(request, h.host)
			h.eq([]any{field(outcome, "nextExpectedAction"), field(outcome, "reason")})
			_, has := get(outcome, "recovery")
			h.eq(has)
			h.eq(h.sendsTo(child))
		})
	})
	t.Run("answered, then generation 3", func(t *testing.T) {
		mirror(t, usl, cls+"test_an_answered_correction_a_later_generation_passed_keeps_its_answer", func(h *hl) {
			correction, request := answered(h)
			h.host.startTurn(child, "turn-dispatch-3", "inProgress", "")
			h.openGeneration("dispatch-3", "needs_changes_revision", "turn-dispatch-3")
			outcome := h.reconcile(request, h.host)
			h.eq([]any{field(outcome, "nextExpectedAction"), field(outcome, "reason")})
			_, has := get(outcome, "recovery")
			h.eq(has)
			h.eq(h.statusPair(correction))
		})
	})
}

// openGeneration is registry.open_generation (todo 25 ports the registry; the transaction body is
// delivery's OpenGenerationIn).
func (h *hl) openGeneration(dispatch, reason, turn string) {
	h.t.Helper()
	mustDo(h.t, h.store.Transaction(h.ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := OpenGenerationIn(ctx, h.store, h.clock, h.rid, dispatch, reason, turn)
		return err
	}))
}
