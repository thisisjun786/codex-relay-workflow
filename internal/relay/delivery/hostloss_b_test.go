package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// test_host_lost_turn.py HLT-4, HLT-8..HLT-17 (see hostloss_a_test.go for the method).

func Test21_HLT04_an_undecided_turn_check_is_named_and_kept(t *testing.T) {
	t.Run("re-confirm from the token keeps the name", func(t *testing.T) {
		mirror(t, hlt, "TheHostLostTheAcceptedTurn.test_a_manual_reconcile_keeps_the_name_on_a_token_confirmed_send", func(h *hl) {
			event, request, turn := h.tokenConfirmed()
			h.hostLoses(turn, false)
			h.clock.Advance(120)
			h.tick()
			h.eq(field(h.completionDelivery(), "turnCheck"))
			outcome := h.reconcile(request, h.host)
			h.eq(field(outcome, "evidence"))
			h.eq(field(sub(outcome, "recipientTurn"), "undecided"))
			h.eq(field(h.completionDelivery(), "turnCheck"))
			h.eq(field(h.statusOf(event), "phase"))
			h.eq(len(h.host.sends))
		})
	})
	t.Run("token_without_turn", func(t *testing.T) {
		mirror(t, hlt, "ReconcileReportsTheRecipientTurn.test_a_delivery_turn_gone_from_the_list_with_its_token_kept_is_named", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostLoses(turn, false)
			h.clock.Advance(120)
			h.eq(field(h.tick().asDict(), "turnsUndecided"))
			h.eq(h.attemptStates(event))
			h.eq(len(h.host.sends))
			h.eq(field(h.statusOf(event), "phase"))
			h.eq(field(h.completionDelivery(), "turnCheck"))
			outcome := h.reconcile(first, h.host)
			reading := sub(outcome, "recipientTurn")
			h.eq([]any{field(reading, "finding"), field(reading, "undecided")})
			h.eq(field(sub(sub(outcome, "record"), "reconciliation"), "recipientTurnsChecked"))
			h.eq(field(h.completionDelivery(), "turnCheck"))
			h.eq(len(h.host.sends))
		})
	})
	t.Run("token deeper than the scan", func(t *testing.T) {
		mirror(t, hlt, "ReconcileReportsTheRecipientTurn.test_a_token_deeper_than_the_scan_in_a_later_turn_is_undecided_not_lost", func(h *hl) {
			event, first, turn := h.dispatched()
			message := h.messageOf(turn)
			h.hostLoses(turn, true)
			h.host.startTurn(parent, "parent-later", "completed", "")
			h.item(parent, "parent-later", message, "userMessage")
			for n := 0; n < 201; n++ {
				h.item(parent, "parent-later", fmt.Sprintf("work item %d", n), "userMessage")
			}
			h.clock.Advance(120)
			reading := sub(h.reconcile(first, h.host), "recipientTurn")
			h.eq([]any{field(reading, "finding"), field(reading, "undecided")})
			h.eq(h.row(event).S("state"))
			h.eq(h.attemptStates(event))
			h.eq(len(h.host.sends))
			h.eq(field(h.statusOf(event), "phase"))
			h.eq(field(h.completionDelivery(), "turnCheck"))
		})
	})
	t.Run("listing bounded before the send", func(t *testing.T) {
		mirror(t, hlt, "ReconcileReportsTheRecipientTurn.test_a_listing_too_long_to_reach_the_send_is_recorded_as_undecided", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostLoses(turn, true)
			h.clock.Advance(120)
			never := &hooked{Adapter: h.host, findDispatched: func(string, string, float64) (TurnPresence, error) {
				return TurnPresence{}, &ListingBounded{"1000 newer turns and the send not reached"}
			}}
			reading := sub(h.reconcile(first, never), "recipientTurn")
			h.eq([]any{field(reading, "finding"), field(reading, "undecided")})
			h.eq(h.row(event).S("state"))
			h.eq(field(h.statusOf(event), "phase"))
		})
	})
	t.Run("empty listing", func(t *testing.T) {
		mirror(t, hlt, "ReconcileReportsTheRecipientTurn.test_a_parent_whose_only_turn_was_lost_names_its_empty_listing", func(h *hl) {
			event := h.queuedEvent(regOpts{})
			record := h.attemptOn(event, h.host, nil)
			h.eq(str(record, "deliveryState"))
			h.hostLoses(str(record, "turnId"), true)
			h.eq(append([]any{}, turnIDs(h.host.threads[parent].turns)...))
			h.clock.Advance(5)
			h.tick()
			h.eq(field(h.statusOf(event), "phase"))
			h.clock.Advance(120)
			h.eq(field(h.tick().asDict(), "turnsUndecided"))
			h.eq(field(h.statusOf(event), "phase"))
			h.eq(field(h.completionDelivery(), "turnCheck"))
			outcome := h.reconcile(str(record, "requestId"), h.host)
			reading := sub(outcome, "recipientTurn")
			h.eq([]any{field(reading, "finding"), field(reading, "undecided")})
			h.eq(field(sub(sub(outcome, "record"), "reconciliation"), "recipientTurnsChecked"))
			h.eq(len(h.host.sends))
			h.eq(h.row(event).S("state"))
		})
	})
	t.Run("a found turn clears the name", func(t *testing.T) {
		mirror(t, hlt, "ReconcileReportsTheRecipientTurn.test_an_undecided_reading_is_cleared_once_the_turn_is_found", func(h *hl) {
			event, first, _ := h.dispatched()
			h.exec("UPDATE attempts SET recipient_scan = ? WHERE request_id = ?", "turn_check_undecided:listing_bounded", first)
			h.clock.Advance(120)
			h.tick()
			h.eq(field(h.statusOf(event), "phase"))
		})
	})
	t.Run("a failed read keeps the name", func(t *testing.T) {
		mirror(t, hlt, "ReconcileReportsTheRecipientTurn.test_an_undecided_name_survives_a_reconcile_whose_read_fails", func(h *hl) {
			event, first, _ := h.dispatched()
			h.exec("UPDATE attempts SET recipient_scan = ? WHERE request_id = ?", "turn_check_undecided:listing_bounded", first)
			h.clock.Advance(120)
			h.host.readFailures["find_dispatched_turn"] = true
			h.eq(field(sub(h.reconcile(first, h.host), "recipientTurn"), "finding"))
			h.eq(field(h.statusOf(event), "phase"))
			h.eq(field(h.completionDelivery(), "turnCheck"))
		})
	})
	t.Run("the daemon records an undecided reading once", func(t *testing.T) {
		mirror(t, hlt, "ReconcileReportsTheRecipientTurn.test_the_daemon_records_an_undecided_reading_once", func(h *hl) {
			_, _, turn := h.dispatched()
			message := h.messageOf(turn)
			h.hostLoses(turn, true)
			h.host.startTurn(parent, "parent-later", "completed", "")
			h.item(parent, "parent-later", message, "userMessage")
			for n := 0; n < 201; n++ {
				h.item(parent, "parent-later", fmt.Sprintf("work item %d", n), "userMessage")
			}
			h.clock.Advance(120)
			first := h.tick().asDict()
			h.eq([]any{field(first, "turnsUndecided"), field(first, "turnsLost")})
			h.clock.Advance(20)
			h.eq(field(h.tick().asDict(), "turnsUndecided"))
			h.eq(len(h.host.sends))
		})
	})
	t.Run("a message under an unfamiliar item type", func(t *testing.T) {
		mirror(t, hlt, "TheHostListsALostTurnAfterAReload.test_a_message_the_host_types_unfamiliarly_is_named_and_not_sent_again", func(h *hl) {
			event, first, turn := h.dispatched()
			message := h.messageOf(turn)
			h.hostReloadsLosing(turn)
			h.item(parent, turn, message, "hookPrompt")
			h.clock.Advance(120)
			h.tick()
			h.eq(h.attemptStates(event))
			h.eq(len(h.host.sends))
			h.eq(h.attemptsFor(event)[0].Opt("recipient_scan"))
			reading := sub(h.reconcile(first, h.host), "recipientTurn")
			h.eq([]any{field(reading, "finding"), field(reading, "undecided")})
			h.eq(strings.Contains(str(reading, "detail"), "hookPrompt"))
		})
	})
}

func Test21_HLT08_reconcile_names_the_loss_and_queues_the_redelivery_without_sending(t *testing.T) {
	plain := func(name string, stage func(h *hl, turn string), full bool) {
		t.Run(name, func(t *testing.T) {
			mirror(t, hlt, name, func(h *hl) {
				event, first, turn := h.dispatched()
				stage(h, turn)
				h.clock.Advance(120)
				outcome := h.reconcile(first, h.host)
				reading := sub(outcome, "recipientTurn")
				if full {
					h.eq([]any{field(reading, "turnId"), field(reading, "finding")})
					h.eq(field(sub(sub(outcome, "record"), "reconciliation"), "recipientTurnsChecked"))
					h.eq(field(outcome, "state"))
					h.eq(field(outcome, "redelivery"))
					h.eq(h.row(event).S("state"))
					h.eq(h.attemptStates(event))
					h.eq(len(h.host.sends))
					return
				}
				h.eq(field(reading, "finding"))
				h.eq(h.row(event).S("state"))
			})
		})
	}
	plain("ReconcileReportsTheRecipientTurn.test_reconcile_names_the_loss_and_queues_the_redelivery_without_sending", func(h *hl, turn string) { h.hostLoses(turn, true) }, true)
	plain("ReconcileReportsTheRecipientTurn.test_a_short_turn_after_the_send_is_read_through_and_the_loss_still_found", func(h *hl, turn string) {
		h.hostLoses(turn, true)
		h.host.startTurn(parent, "parent-later", "completed", "unrelated work")
	}, false)
	plain("ReconcileReportsTheRecipientTurn.test_the_items_of_a_turn_listed_before_the_send_end_the_scan", func(h *hl, turn string) {
		t := h.host.threads[parent]
		var earlier [][3]string
		for n := 0; n < 250; n++ {
			earlier = append(earlier, [3]string{"parent-earlier", fmt.Sprintf("earlier item %d", n), "userMessage"})
		}
		t.items = append(earlier, t.items...)
		h.hostLoses(turn, true)
	}, false)
}

func Test21_HLT09_an_unreadable_turn_list_is_unchecked_and_changes_nothing(t *testing.T) {
	mirror(t, hlt, "ReconcileReportsTheRecipientTurn.test_an_unreadable_turn_list_is_reported_as_unchecked_and_changes_nothing", func(h *hl) {
		event, first, turn := h.dispatched()
		h.hostLoses(turn, true)
		h.clock.Advance(120)
		h.host.readFailures["find_dispatched_turn"] = true
		outcome := h.reconcile(first, h.host)
		h.eq(field(sub(outcome, "recipientTurn"), "finding"))
		h.eq(field(sub(sub(outcome, "record"), "reconciliation"), "recipientTurnsChecked"))
		h.eq(h.row(event).S("state"))
	})
}

func Test21_HLT10_the_delivery_token_vetoes_a_missing_turn_row(t *testing.T) {
	t.Run("token in the parent's items", func(t *testing.T) {
		mirror(t, hlt, "ReconcileReportsTheRecipientTurn.test_the_delivery_token_in_the_parents_items_vetoes_a_missing_turn_row", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostLoses(turn, false)
			h.clock.Advance(120)
			h.eq(field(sub(h.reconcile(first, h.host), "recipientTurn"), "finding"))
			h.eq(h.row(event).S("state"))
			h.eq(h.attemptStates(event))
		})
	})
	t.Run("token behind other dropped turns' items", func(t *testing.T) {
		mirror(t, hlt, "ReconcileReportsTheRecipientTurn.test_a_token_behind_the_items_of_other_dropped_turns_still_vetoes_the_loss", func(h *hl) {
			event, first, turn := h.dispatched()
			h.clock.Advance(5)
			h.host.startTurn(parent, "parent-later", "completed", "later work")
			h.host.startTurn(parent, "parent-later-2", "completed", "more later work")
			for _, dropped := range []string{turn, "parent-later", "parent-later-2"} {
				h.hostLoses(dropped, false)
			}
			h.clock.Advance(120)
			h.tick()
			h.eq(h.attemptStates(event))
			h.eq(len(h.host.sends))
			h.eq(h.journalled(HostLostTurn))
			reading := sub(h.reconcile(first, h.host), "recipientTurn")
			h.eq([]any{field(reading, "finding"), field(reading, "status")})
		})
	})
	t.Run("a steered turn begun before the send", func(t *testing.T) {
		mirror(t, hlt, "ReconcileReportsTheRecipientTurn.test_a_start_that_steered_a_turn_begun_before_the_send_is_present", func(h *hl) {
			h.host.startTurn(parent, "parent-running", "inProgress", "")
			h.clock.Advance(300)
			event := h.queuedEvent(regOpts{})
			h.host.script = []string{"steer_existing"}
			record := h.attemptOn(event, h.host, nil)
			h.eq(field(record, "turnId"))
			h.clock.Advance(120)
			h.eq(field(sub(h.reconcile(str(record, "requestId"), h.host), "recipientTurn"), "finding"))
			h.eq(h.row(event).S("state"))
		})
	})
}

func Test21_HLT11_the_lost_attempt_is_the_once_count(t *testing.T) {
	t.Run("reconciling it again changes nothing", func(t *testing.T) {
		mirror(t, hlt, "ReconcileReportsTheRecipientTurn.test_reconciling_a_lost_attempt_again_changes_nothing", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostLoses(turn, true)
			h.clock.Advance(120)
			mustDo(t, h.store.Transaction(h.ctx, func(ctx context.Context, _ *sql.Conn) error {
				if _, err := execSQL(ctx, h.store, "UPDATE attempts SET state = ? WHERE request_id = ?", HostLostTurn, first); err != nil {
					return err
				}
				_, err := execSQL(ctx, h.store, "UPDATE deliveries SET state = ?, dispatch_evidence = ? WHERE event_id = ?", Queued, HostLostTurn, event)
				return err
			}))
			second := h.attemptOn(event, h.host, nil)
			h.eq(str(second, "deliveryState"))
			h.eq(field(sub(h.reconcile(first, h.host), "recipientTurn"), "finding"))
			h.eq(h.attemptStates(event))
			h.eq(h.journalled(HostLostTurn))
			h.hostLoses(str(second, "turnId"), true)
			for i := 0; i < 3; i++ {
				h.clock.Advance(120)
				h.tick()
			}
			h.eq(h.row(event).Opt("hold_reason"))
			h.eq(len(h.host.sends))
		})
	})
	t.Run("a reconcile racing the daemon's settlement", func(t *testing.T) {
		mirror(t, hlt, "ReconcileReportsTheRecipientTurn.test_a_reconcile_racing_the_daemons_settlement_cannot_undo_the_loss", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostLoses(turn, true)
			h.clock.Advance(120)
			fired := 0
			racing := &hooked{Adapter: h.host}
			racing.findDispatched = func(thread, turn string, sentAt float64) (TurnPresence, error) {
				if fired == 0 {
					fired++
					_, err := h.rc.CheckDispatchedTurn(h.ctx, first, h.host)
					mustDo(t, err)
				}
				return h.host.FindDispatchedTurn(thread, turn, sentAt)
			}
			outcome := h.reconcile(first, racing)
			h.eq(fired)
			h.eq(field(outcome, "state"))
			h.eq(field(sub(outcome, "recipientTurn"), "finding"))
			h.eq(field(outcome, "redelivery"))
			h.eq(h.attemptStates(event))
			h.eq(h.row(event).S("state"))
			h.eq(h.journalled(HostLostTurn))
			second := h.attemptOn(event, h.host, nil)
			h.hostLoses(str(second, "turnId"), true)
			for i := 0; i < 3; i++ {
				h.clock.Advance(120)
				h.tick()
			}
			h.eq(h.row(event).Opt("hold_reason"))
			h.eq(len(h.host.sends))
		})
	})
}

func Test21_HLT12_a_recorded_acknowledgement_wins_over_a_missing_turn(t *testing.T) {
	mirror(t, hlt, "ReconcileReportsTheRecipientTurn.test_a_recorded_acknowledgement_wins_over_a_missing_turn", func(h *hl) {
		event, first, turn := h.dispatched()
		_, err := h.ack.Acknowledge(h.ctx, event, "ack-later", AckProof(event, "ack-later"), true, nil, nil)
		mustDo(t, err)
		h.hostLoses(turn, true)
		h.clock.Advance(120)
		outcome := h.reconcile(first, h.host)
		h.eq(field(sub(outcome, "recipientTurn"), "finding"))
		h.eq(field(outcome, "redelivery"))
		h.eq(h.row(event).S("state"))
		h.eq(h.attemptStates(event))
		h.tick()
		h.eq(len(h.host.sends))
	})
}

func Test21_HLT13_the_controls_read_as_before(t *testing.T) {
	t.Run("a lost acknowledgement keeps waiting", func(t *testing.T) {
		mirror(t, hlt, "TheControlsReadAsBefore.test_a_lost_acknowledgement_keeps_waiting_and_is_read_as_present", func(h *hl) {
			event, first, turn := h.dispatched()
			h.host.finishTurn(parent, turn, "interrupted")
			h.clock.Advance(120)
			for i := 0; i < 2; i++ {
				h.tick()
				h.clock.Advance(120)
			}
			item := h.statusOf(event)
			h.eq([]any{field(item, "state"), field(item, "reported"), field(item, "phase")})
			h.eq(len(h.host.sends))
			h.eq(h.journalled(HostLostTurn))
			outcome := h.reconcile(first, h.host)
			reading := sub(outcome, "recipientTurn")
			h.eq([]any{field(reading, "finding"), field(reading, "status")})
			h.eq(field(sub(sub(outcome, "record"), "reconciliation"), "recipientTurnsChecked"))
		})
	})
	t.Run("a normal acknowledgement settles before any check", func(t *testing.T) {
		mirror(t, hlt, "TheControlsReadAsBefore.test_a_normal_acknowledgement_settles_before_any_check_is_owed", func(h *hl) {
			event, _, _ := h.dispatched()
			h.clock.Advance(5)
			h.acknowledge(event, "ack-turn")
			var lookups []string
			counting := countingLookups(h.host, &lookups)
			checks := &TurnChecks{Reconciler: h.rc}
			for i := 0; i < 2; i++ {
				h.clock.Advance(120)
				h.tickWith(defaultTick(), counting, checks)
			}
			h.eq(h.row(event).S("state"))
			h.eq(len(h.host.sends))
			h.eq(append([]any{}, anySlice(lookups)...))
			h.eq(h.journalled(HostLostTurn))
		})
	})
}

func anySlice(list []string) []any {
	out := make([]any, len(list))
	for i, v := range list {
		out[i] = v
	}
	return out
}

func Test21_HLT14_assignment_show_names_the_next_actor_for_each_completion_state(t *testing.T) {
	run := func(name string, body func(h *hl)) {
		t.Run(name, func(t *testing.T) {
			mirror(t, hlt, "AssignmentShowNamesTheNextActor."+name, func(h *hl) {
				body(h)
				h.eq(h.nextAction())
			})
		})
	}
	run("test_a_delivered_completion_waits_for_the_parents_acknowledgement", func(h *hl) { h.dispatched() })
	run("test_a_completion_not_yet_sent_is_still_the_daemons", func(h *hl) { h.queuedEvent(regOpts{}) })
	run("test_an_acknowledged_completion_waits_for_the_parents_verification", func(h *hl) {
		event, _, _ := h.dispatched()
		h.clock.Advance(5)
		h.acknowledge(event, "ack-turn")
	})
	run("test_a_recorded_acknowledgement_is_the_daemons_to_verify", func(h *hl) {
		event, _, _ := h.dispatched()
		_, err := h.ack.Acknowledge(h.ctx, event, "ack-later", AckProof(event, "ack-later"), true, nil, nil)
		mustDo(h.t, err)
	})
	run("test_a_refused_acknowledgement_is_the_parents_to_restate", func(h *hl) {
		event, _, _ := h.dispatched()
		_, err := h.ack.Acknowledge(h.ctx, event, "ack-early", AckProof(event, "ack-early"), true, nil, nil)
		mustDo(h.t, err)
		h.exec("UPDATE ack_evidence SET last_reason = ? WHERE event_id = ?", "ack_turn_unverified", event)
	})
	run("test_an_uncertain_send_is_the_daemons_to_reconcile", func(h *hl) {
		h.parentHistory()
		event := h.queuedEvent(regOpts{})
		h.host.script = []string{"transport_unknown"}
		h.attemptOn(event, h.host, nil)
		h.eq(h.row(event).S("state"))
	})
	run("test_an_interrupted_claim_is_the_daemons_to_reconcile", func(h *hl) {
		event := h.queuedEvent(regOpts{})
		h.exec("UPDATE deliveries SET state = 'sending' WHERE event_id = ?", event)
	})
	run("test_an_inbox_only_completion_is_the_parents_to_acknowledge", func(h *hl) {
		event, _, _ := h.dispatched()
		h.exec("UPDATE deliveries SET state = 'inbox_only', hold_reason = 'push_channel_closed' WHERE event_id = ?", event)
	})
	t.Run("test_a_claimed_completion_without_an_ack_is_the_parents_to_acknowledge", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_a_claimed_completion_without_an_ack_is_the_parents_to_acknowledge", func(h *hl) {
			event, _, _ := h.dispatched()
			_, err := h.ack.ClaimVerification(h.ctx, event, "parent-reading")
			mustDo(t, err)
			h.eq(h.nextAction())
		})
	})
}

func Test21_HLT15_the_host_loss_is_named_at_every_step_of_its_recovery(t *testing.T) {
	t.Run("every step", func(t *testing.T) {
		mirror(t, hlt, "AssignmentShowNamesTheNextActor.test_the_host_loss_is_named_at_every_step_of_its_recovery", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostLoses(turn, true)
			h.clock.Advance(120)
			h.reconcile(first, h.host)
			h.eq(h.nextAction())
			h.eq(field(h.completionDelivery(), "hostLostAttempts"))
			h.exec("UPDATE deliveries SET state = 'sending' WHERE event_id = ?", event)
			h.eq(h.nextAction())
			h.exec("UPDATE deliveries SET state = 'queued' WHERE event_id = ?", event)
			record := h.attemptOn(event, h.host, nil)
			h.eq(str(record, "deliveryState"))
			h.eq(h.nextAction())
			h.eq(field(h.completionDelivery(), "hostLostAttempts"))
			h.hostLoses(str(record, "turnId"), true)
			h.clock.Advance(120)
			h.reconcile(str(record, "requestId"), h.host)
			h.eq(h.row(event).Opt("hold_reason"))
			h.eq(h.nextAction())
			h.eq(field(h.completionDelivery(), "hostLostAttempts"))
		})
	})
	t.Run("held for another reason after a loss", func(t *testing.T) {
		mirror(t, hlt, "AssignmentShowNamesTheNextActor.test_a_redelivery_held_for_another_reason_is_still_the_parents_after_a_loss", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostLoses(turn, true)
			h.clock.Advance(120)
			h.reconcile(first, h.host)
			h.exec("UPDATE deliveries SET state = 'withheld_pre_send', hold_reason = 'attempt_cap' WHERE event_id = ?", event)
			h.eq(h.nextAction())
		})
	})
	t.Run("an uncertain redelivery after a loss", func(t *testing.T) {
		mirror(t, hlt, "AssignmentShowNamesTheNextActor.test_an_uncertain_redelivery_after_a_loss_is_reconciled_not_resent", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostLoses(turn, true)
			h.clock.Advance(120)
			h.reconcile(first, h.host)
			h.host.script = []string{"transport_unknown"}
			h.attemptOn(event, h.host, nil)
			h.eq(h.row(event).S("state"))
			h.eq(h.nextAction())
		})
	})
	t.Run("a corrected completion reads its own delivery", func(t *testing.T) {
		mirror(t, hlt, "AssignmentShowNamesTheNextActor.test_a_corrected_completion_reads_its_own_delivery", func(h *hl) {
			first := h.queuedEvent(regOpts{recipients: []string{parent, child}})
			h.attemptOn(first, h.host, nil)
			h.clock.Advance(5)
			h.acknowledge(first, "ack-turn")
			_, err := h.ack.RecordVerdict(h.ctx, first, "needs_changes", "v1", nil, []any{Obj{{Key: "id", Value: "c1"}, {Key: "verdict", Value: "needs_changes"}, {Key: "note", Value: "fix the shape"}}}, nil, nil)
			mustDo(t, err)
			correction := h.one("SELECT event_id FROM deliveries WHERE kind = ?", Revision).S("event_id")
			h.attemptOn(correction, h.host, nil)
			h.clock.Advance(300)
			r, err := LoadRelationship(h.ctx, h.store, h.rid)
			mustDo(t, err)
			_, err = BindAnchor(h.ctx, h.store, h.clock, h.rid, r.Generation, "revision-turn")
			mustDo(t, err)
			payload := h.readyPayload(h.rid, r.Generation, []string{h.artifact("revised.txt", "the corrected deliverable")}, 1, turnRef{child, "revision-turn", "completed"})
			_, err = h.accept(payload, storeAcceptNone)
			mustDo(t, err)
			_, err = h.delivery.Enqueue(h.ctx, str(payload, "eventId"), "", "")
			mustDo(t, err)
			h.eq(field(h.assignment(), "state"))
			h.eq(h.nextAction())
			record := h.attemptOn(str(payload, "eventId"), h.host, nil)
			h.eq(str(record, "deliveryState"))
			h.eq(h.nextAction())
			h.hostLoses(str(record, "turnId"), true)
			h.clock.Advance(120)
			h.reconcile(str(record, "requestId"), h.host)
			h.eq(h.nextAction())
		})
	})
}

func Test21_HLT16_only_the_delivered_user_message_vetoes_a_loss(t *testing.T) {
	t.Run("a message only under another turn", func(t *testing.T) {
		mirror(t, hlt, "TheHostListsALostTurnAfterAReload.test_a_message_found_only_under_another_turn_is_read_again_and_its_loss_caught", func(h *hl) {
			event, first, turn := h.dispatched()
			message := h.messageOf(turn)
			h.hostReloadsLosing(turn)
			h.host.startTurn(parent, "parent-later", "completed", "")
			h.item(parent, "parent-later", message, "userMessage")
			h.clock.Advance(120)
			h.tick()
			reading := sub(h.reconcile(first, h.host), "recipientTurn")
			h.eq([]any{field(reading, "finding"), field(reading, "status"), field(reading, "undecided")})
			h.eq(strings.Contains(str(reading, "detail"), "parent-later"))
			h.eq(len(h.host.sends))
			h.hostReloadsLosing("parent-later")
			h.clock.Advance(20)
			h.tick()
			h.eq(h.attemptStates(event))
			h.eq(len(h.host.sends))
		})
	})
	t.Run("a relay command echoing the request id", func(t *testing.T) {
		mirror(t, hlt, "TheHostListsALostTurnAfterAReload.test_a_relay_command_echoing_the_request_id_does_not_veto_the_loss", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostReloadsLosing(turn)
			h.host.startTurn(parent, "parent-later", "completed", "")
			h.echo("parent-later", dumps(Obj{{Key: "delivery", Value: Obj{{Key: "requestId", Value: first}}}}))
			h.clock.Advance(120)
			h.tick()
			h.eq(h.attemptStates(event))
		})
	})
	t.Run("a file the parent wrote", func(t *testing.T) {
		mirror(t, hlt, "TheHostListsALostTurnAfterAReload.test_a_file_the_parent_wrote_with_the_request_id_does_not_veto_the_loss", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostReloadsLosing(turn)
			h.host.startTurn(parent, "parent-later", "completed", "")
			h.item(parent, "parent-later", "+ waiting on "+first, "fileChange")
			h.clock.Advance(120)
			h.tick()
			h.eq(h.attemptStates(event))
			h.eq(len(h.host.sends))
		})
	})
}

func Test21_HLT17_listed_turns_are_read_from_their_own_items(t *testing.T) {
	t.Run("a long finished turn is read from its first item", func(t *testing.T) {
		mirror(t, hlt, "TheHostListsALostTurnAfterAReload.test_a_long_finished_turn_is_read_from_its_first_item", func(h *hl) {
			_, first, turn := h.dispatched()
			for n := 0; n < 300; n++ {
				h.item(parent, turn, fmt.Sprintf("work item %d", n), "commandExecution")
			}
			h.host.finishTurn(parent, turn, "completed")
			h.clock.Advance(120)
			reading := sub(h.reconcile(first, h.host), "recipientTurn")
			h.eq([]any{field(reading, "finding"), field(reading, "status"), field(reading, "undecided")})
		})
	})
	t.Run("an in-progress listed turn is present unread", func(t *testing.T) {
		mirror(t, hlt, "TheHostListsALostTurnAfterAReload.test_an_in_progress_listed_turn_is_present_without_reading_its_items", func(h *hl) {
			_, first, turn := h.dispatched()
			h.dropItems(turn)
			h.clock.Advance(120)
			reading := sub(h.reconcile(first, h.host), "recipientTurn")
			h.eq([]any{field(reading, "finding"), field(reading, "status")})
		})
	})
}

func turnIDs(turns []TurnInfo) []any {
	out := make([]any, len(turns))
	for i, t := range turns {
		out[i] = t.TurnID
	}
	return out
}
