package delivery

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// test_host_lost_turn.py HLT-18..HLT-27 (see hostloss_a_test.go for the method).

func Test21_HLT18_an_ack_before_the_send_is_confirmed_is_kept_and_completed(t *testing.T) {
	t.Run("an ACK in the delivery turn confirms the send", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_an_ack_in_the_delivery_turn_confirms_the_send_and_the_verdict_follows", func(h *hl) {
			event, _, turn := h.uncertainDelivery(true)
			claim, err := h.ack.ClaimVerification(h.ctx, event, turn)
			mustDo(t, err)
			h.eq(claim)
			h.eq(field(h.cliAck(event, turn, nil), "_verified"))
			h.eq(h.row(event).S("state"))
			h.eq(h.attemptStates(event))
			h.eq(h.evidenceOf(event))
			h.eq(field(h.cliVerdict(event, turn), "verdict"))
			h.eq(len(h.host.sends))
		})
	})
	t.Run("an unconfirmable ACK is kept", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_an_ack_the_relay_cannot_confirm_yet_is_kept_not_refused", func(h *hl) {
			event, _, turn := h.uncertainDelivery(false)
			_, err := h.ack.ClaimVerification(h.ctx, event, turn)
			mustDo(t, err)
			h.eq(field(h.cliAck(event, turn, nil), "_verified"))
			row := h.ackRow(event)
			h.eq([]any{row.Opt("verified"), row.Opt("last_reason")})
			h.eq(h.row(event).S("state"))
			h.eq(h.nextAction())
		})
	})
	t.Run("a kept ACK completes once the delivery is confirmed", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_a_kept_ack_completes_once_the_delivery_is_confirmed", func(h *hl) {
			event, request, turn := h.uncertainDelivery(false)
			_, err := h.ack.ClaimVerification(h.ctx, event, turn)
			mustDo(t, err)
			h.cliAck(event, turn, nil)
			h.clock.Advance(1)
			h.tick()
			h.eq(h.row(event).S("state"))
			h.item(parent, turn, "requestId: "+request, "")
			h.clock.Advance(5)
			h.tick()
			h.eq(h.row(event).S("state"))
			h.eq(h.nextAction())
			h.eq(len(h.host.sends))
		})
	})
	t.Run("a confirmed delivery with a kept ACK", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_a_confirmed_delivery_with_a_kept_ack_is_the_daemons_to_verify", func(h *hl) {
			event, request, turn := h.uncertainDelivery(false)
			_, err := h.ack.ClaimVerification(h.ctx, event, turn)
			mustDo(t, err)
			h.cliAck(event, turn, nil)
			h.item(parent, turn, "requestId: "+request, "")
			h.reconcile(request, h.host)
			h.eq(h.row(event).S("state"))
			h.eq(h.nextAction())
		})
	})
	t.Run("a verdict completes a kept ACK first", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_a_verdict_completes_a_kept_ack_first", func(h *hl) {
			event, request, turn := h.uncertainDelivery(false)
			_, err := h.ack.ClaimVerification(h.ctx, event, turn)
			mustDo(t, err)
			h.cliAck(event, turn, nil)
			h.item(parent, turn, "requestId: "+request, "")
			h.eq(field(h.cliVerdict(event, turn), "verdict"))
			h.eq(h.row(event).S("state"))
		})
	})
	t.Run("the daemon confirms a kept ACK through its turn", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_a_kept_ack_is_confirmed_by_the_daemon_through_its_turn", func(h *hl) {
			event, request, turn := h.uncertainDelivery(false)
			h.cliAck(event, turn, nil)
			thread := h.host.threads[parent]
			position := 0
			for i, item := range thread.items {
				if item[0] == turn {
					position = i
					break
				}
			}
			thread.items = append(thread.items[:position], append([][3]string{{turn, "requestId: " + request, "userMessage"}}, thread.items[position:]...)...)
			for n := 0; n < 250; n++ {
				h.item(parent, turn, fmt.Sprintf("work item %d", n), "commandExecution")
			}
			h.clock.Advance(5)
			h.tick()
			h.eq(h.row(event).S("state"))
		})
	})
	t.Run("verify-acks confirms a kept ACK's delivery", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementIsJudgedAgainstTheDeliveryItRead.test_manual_verify_acks_confirms_a_kept_acks_delivery_through_its_turn", func(h *hl) {
			event, _ := h.foldedDelivery(true, 10, 250)
			_, err := h.ack.Acknowledge(h.ctx, event, "folded", AckProof(event, "folded"), true, nil, nil)
			mustDo(t, err)
			_, err = VerifyAcksCommand(h.ctx, h.ack, h.rc, h.host, 8)
			mustDo(t, err)
			h.eq(h.row(event).S("state"))
		})
	})
}

func Test21_HLT19_an_ack_while_the_relay_is_still_sending_is_kept(t *testing.T) {
	mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_an_ack_while_the_relay_is_still_sending_is_kept_and_completes_after", func(h *hl) {
		h.parentHistory()
		event := h.queuedEvent(regOpts{})
		var reads []any
		counting := countingReads(h.host, &reads)
		var keptState string
		var keptRecord Obj
		during := &hooked{Adapter: h.host}
		during.send = func(requestID, thread, message string, settings *TaskSettings) (Obj, error) {
			receipt, err := h.host.SendMessage(requestID, thread, message, settings)
			keptState = h.row(event).S("state")
			keptRecord = h.cliAck(event, str(receipt, "turnId"), counting)
			return receipt, err
		}
		h.attemptOn(event, during, nil)
		h.eq(keptState)
		h.eq(field(keptRecord, "_verified"))
		h.eq(append([]any{}, reads...))
		h.eq(h.row(event).S("state"))
		h.clock.Advance(5)
		h.tick()
		h.eq(h.row(event).S("state"))
	})
}

func Test21_HLT20_host_reads_only_when_needed(t *testing.T) {
	t.Run("an ordinary ACK", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_an_ordinary_ack_makes_no_confirmation_reads", func(h *hl) {
			event, _, _ := h.dispatched()
			h.clock.Advance(5)
			turn := h.host.startTurn(parent, "ack-turn", "inProgress", "")
			var reads []any
			h.eq(field(h.cliAck(event, turn.TurnID, countingReads(h.host, &reads)), "_verified"))
			h.eq(append([]any{}, reads...))
			h.eq(h.row(event).S("state"))
		})
	})
	t.Run("a wrong proof", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_a_wrong_proof_makes_no_host_reads", func(h *hl) {
			event, _, turn := h.uncertainDelivery(true)
			var reads []any
			counting := countingReads(h.host, &reads)
			_, err := AckCommand(h.ctx, h.ack, h.rc, counting, event, turn, "0000000000000000000000000000000000000000000000000000000000000000", nil)
			requireReason(t, err, AckProofMismatch)
			h.eq(append([]any{}, reads...))
			h.eq(h.row(event).S("state"))
		})
	})
}

func Test21_HLT21_the_message_is_found_through_the_ack_turn_and_an_echo_confirms_nothing(t *testing.T) {
	t.Run("deep in a long delivery turn", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_a_message_deep_in_a_long_delivery_turn_is_confirmed_through_the_ack_turn", func(h *hl) {
			event, request, turn := h.uncertainDelivery(true)
			for n := 0; n < 250; n++ {
				h.item(parent, turn, fmt.Sprintf("work item %d", n), "commandExecution")
			}
			scan, err := h.host.FindToken(parent, request, 200, false)
			mustDo(t, err)
			h.eq(scan.Found)
			h.eq(field(h.cliAck(event, turn, nil), "_verified"))
			h.eq(h.row(event).S("state"))
		})
	})
	t.Run("deep in a folded turn", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_a_message_deep_in_a_folded_turn_is_found_through_the_ack_turn", func(h *hl) {
			event, request := h.foldedDelivery(true, 10, 250)
			h.eq(field(h.cliAck(event, "folded", nil), "_verified"))
			h.eq(h.row(event).S("state"))
			h.host.finishTurn(parent, "folded", "completed")
			h.clock.Advance(120)
			outcome, err := h.rc.CheckDispatchedTurn(h.ctx, request, h.host)
			mustDo(t, err)
			reading := sub(outcome, "recipientTurn")
			h.eq([]any{field(reading, "finding"), field(reading, "status"), field(reading, "undecided")})
		})
	})
	t.Run("an echo in the ACK turn", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_an_echo_in_the_ack_turn_does_not_confirm_the_send", func(h *hl) {
			event, request, turn := h.uncertainDelivery(false)
			h.echo(turn, dumps(Obj{{Key: "delivery", Value: Obj{{Key: "requestId", Value: request}}}}))
			h.eq(field(h.cliAck(event, turn, nil), "_verified"))
			h.eq(h.row(event).S("state"))
			h.eq(h.evidenceOf(event))
		})
	})
	t.Run("an echo alone", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_an_echo_alone_never_confirms_an_uncertain_send", func(h *hl) {
			event, request, turn := h.uncertainDelivery(false)
			h.echo(turn, "status: attempt "+request+" held_uncertain")
			h.eq(field(h.reconcile(request, h.host), "state"))
			h.eq(h.row(event).S("state"))
		})
	})
}

func Test21_HLT22_an_ack_from_the_turn_the_send_was_folded_into(t *testing.T) {
	t.Run("verified", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_an_ack_from_the_turn_the_send_was_folded_into_is_verified", func(h *hl) {
			event, _ := h.foldedDelivery(true, 0, 0)
			h.eq(field(h.cliAck(event, "folded", nil), "_verified"))
			row := h.row(event)
			h.eq([]any{row.S("state"), row.Opt("dispatch_turn_id")})
			h.eq(len(h.host.sends))
		})
	})
	t.Run("kept until the send is confirmed", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_an_ack_from_a_folded_turn_is_kept_until_the_send_is_confirmed", func(h *hl) {
			event, request := h.foldedDelivery(false, 0, 0)
			h.eq(field(h.cliAck(event, "folded", nil), "_verified"))
			h.eq(h.ackRow(event).Opt("last_reason"))
			h.item(parent, "folded", "requestId: "+request, "")
			h.clock.Advance(5)
			h.tick()
			h.eq(h.row(event).S("state"))
			h.eq(len(h.host.sends))
		})
	})
}

func Test21_HLT23_an_ack_answers_only_attempts_sent_before_it_was_authored(t *testing.T) {
	t.Run("a kept ACK does not hide the next attempt's loss", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_an_ack_kept_for_an_attempt_that_never_sent_does_not_hide_the_next_attempts_loss", func(h *hl) {
			h.parentHistory()
			event := h.queuedEvent(regOpts{})
			h.host.script = []string{"in_progress"}
			first := str(h.attemptOn(event, h.host, nil), "requestId")
			h.clock.Advance(2)
			h.host.startTurn(parent, "ack-turn", "completed", "")
			h.eq(field(h.cliAck(event, "ack-turn", nil), "_verified"))
			h.host.ledger[first] = preSendRejection(first)
			h.reconcile(first, h.host)
			h.clock.Advance(100000)
			second := h.attemptOn(event, h.host, at(h.clock.Now()))
			h.eq(str(second, "deliveryState"))
			h.hostReloadsLosing(str(second, "turnId"))
			h.clock.Advance(120)
			h.tick()
			h.eq(h.attemptStates(event))
			h.eq(len(h.host.sends))
		})
	})
	t.Run("a kept ACK is not promoted for a later folded attempt", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementIsJudgedAgainstTheDeliveryItRead.test_a_kept_ack_is_not_promoted_for_a_later_attempt_folded_into_the_same_turn", func(h *hl) {
			event, first := h.foldedDelivery(false, 0, 0)
			_, err := h.ack.Acknowledge(h.ctx, event, "folded", AckProof(event, "folded"), true, nil, nil)
			mustDo(t, err)
			h.host.ledger[first] = preSendRejection(first)
			h.reconcile(first, h.host)
			h.clock.Advance(100000)
			h.host.script = []string{"in_progress"}
			second := str(h.attemptOn(event, h.host, at(h.clock.Now())), "requestId")
			h.item(parent, "folded", "requestId: "+second, "")
			h.clock.Advance(5)
			h.tick()
			h.eq(h.row(event).S("state"))
			row := h.ackRow(event)
			h.eq([]any{row.Opt("verified"), row.Opt("last_reason")})
			h.eq(h.nextAction())
			h.eq(field(h.cliAck(event, "folded", nil), "_verified"))
			h.eq(h.row(event).S("state"))
		})
	})
	t.Run("an ACK authored after the send keeps the delivery out of the pass", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementBeforeTheSendIsConfirmed.test_an_ack_authored_after_the_send_keeps_the_delivery_out_of_the_turn_pass", func(h *hl) {
			event, _, turn := h.dispatched()
			h.clock.Advance(5)
			_, err := h.ack.Acknowledge(h.ctx, event, "ack-later", AckProof(event, "ack-later"), true, nil, nil)
			mustDo(t, err)
			var reads []string
			counting := countingLookups(h.host, &reads)
			h.hostLoses(turn, true)
			h.clock.Advance(120)
			h.adapter = counting
			h.tick()
			h.eq(anySlice(reads))
			h.eq(h.attemptStates(event))
			h.eq(len(h.host.sends))
		})
	})
}

func (h *hl) confirmsDuringTheScan(event, turn string) *hooked {
	fired := false
	w := &hooked{Adapter: h.host}
	w.findToken = func(thread, token string, limit int, messageOnly bool) (TokenScan, error) {
		scan, err := h.host.FindToken(thread, token, limit, messageOnly)
		if !fired {
			fired = true
			_, cerr := h.rc.ConfirmDelivery(h.ctx, event, h.host, turn)
			mustDo(h.t, cerr)
		}
		return scan, err
	}
	return w
}

func Test21_HLT24_every_settlement_is_a_compare_and_set(t *testing.T) {
	t.Run("a replacement ACK during the read", func(t *testing.T) {
		mirror(t, hlt, "EverySettlementIsACompareAndSet.test_a_replacement_ack_written_during_the_read_is_not_promoted_with_the_old_evidence", func(h *hl) {
			event, request, turn := h.uncertainDelivery(false)
			h.cliAck(event, turn, nil)
			h.item(parent, turn, "requestId: "+request, "")
			h.reconcile(request, h.host)
			h.eq(h.row(event).S("state"))
			later := h.host.startTurn(parent, "later-turn", "inProgress", "")
			fired := false
			replaces := &hooked{Adapter: h.host}
			replaces.readTurn = func(thread, turn string) (*TurnInfo, error) {
				if !fired {
					fired = true
					_, err := h.ack.Acknowledge(h.ctx, event, later.TurnID, AckProof(event, later.TurnID), true, nil, nil)
					mustDo(t, err)
				}
				return h.host.ReadTurn(thread, turn)
			}
			results, err := h.ack.VerifyPendingAcks(h.ctx, replaces, 8, nil)
			mustDo(t, err)
			h.eq(outcomes(results))
			row := h.one("SELECT ack_turn_id, verified FROM acks WHERE event_id = ?", event)
			h.eq([]any{row.Opt("ack_turn_id"), row.Opt("verified")})
			h.eq(h.row(event).S("state"))
		})
	})
	t.Run("a reader that found nothing cannot undo a confirmation", func(t *testing.T) {
		mirror(t, hlt, "EverySettlementIsACompareAndSet.test_a_reader_that_found_nothing_cannot_undo_a_confirmation_made_meanwhile", func(h *hl) {
			event, request, turn := h.uncertainDelivery(true)
			for n := 0; n < 250; n++ {
				h.item(parent, turn, fmt.Sprintf("work item %d", n), "commandExecution")
			}
			outcome := h.reconcile(request, h.confirmsDuringTheScan(event, turn))
			h.eq(field(outcome, "evidence"))
			h.eq(h.evidenceOf(event))
			h.eq(h.row(event).S("state"))
		})
	})
	t.Run("a reader that found nothing cannot undo a pre-send rejection", func(t *testing.T) {
		mirror(t, hlt, "EverySettlementIsACompareAndSet.test_a_reader_that_found_nothing_cannot_undo_a_pre_send_rejection_made_meanwhile", func(h *hl) {
			h.parentHistory()
			event := h.queuedEvent(regOpts{})
			h.host.script = []string{"in_progress"}
			request := str(h.attemptOn(event, h.host, nil), "requestId")
			fired := false
			rejects := &hooked{Adapter: h.host}
			rejects.findToken = func(thread, token string, limit int, messageOnly bool) (TokenScan, error) {
				scan, err := h.host.FindToken(thread, token, limit, messageOnly)
				if !fired {
					fired = true
					h.host.ledger[request] = preSendRejection(request)
					h.reconcile(request, h.host)
				}
				return scan, err
			}
			outcome := h.reconcile(request, rejects)
			h.eq(truthy(field(outcome, "changed")))
			h.eq(h.evidenceOf(event))
			h.eq(h.row(event).S("state"))
			h.clock.Advance(100000)
			h.eq(field(h.attemptOn(event, h.host, at(h.clock.Now())), "attemptNo"))
		})
	})
	t.Run("a refused stale write leaves the attempt owed", func(t *testing.T) {
		mirror(t, hlt, "EverySettlementIsACompareAndSet.test_a_refused_stale_write_leaves_the_attempt_owed_to_the_next_tick", func(h *hl) {
			event, request, turn := h.uncertainDelivery(true)
			for n := 0; n < 250; n++ {
				h.item(parent, turn, fmt.Sprintf("work item %d", n), "commandExecution")
			}
			policy := defaultTick()
			policy.maxSendsTick = 0
			h.tickWith(policy, h.confirmsDuringTheScan(event, turn), h.checks)
			h.eq(h.one("SELECT retry_required FROM reconcile_gate WHERE request_id = ?", request).Opt("retry_required"))
			h.eq(h.evidenceOf(event))
		})
	})
	t.Run("a later unreadable receipt keeps a pre-send rejection", func(t *testing.T) {
		mirror(t, hlt, "EverySettlementIsACompareAndSet.test_a_later_reconcile_that_cannot_read_the_receipt_keeps_a_pre_send_rejection", func(h *hl) {
			h.parentHistory()
			event := h.queuedEvent(regOpts{})
			h.host.script = []string{"in_progress"}
			request := str(h.attemptOn(event, h.host, nil), "requestId")
			h.host.ledger[request] = preSendRejection(request)
			h.reconcile(request, h.host)
			h.eq(h.evidenceOf(event))
			delete(h.host.ledger, request)
			h.reconcile(request, h.host)
			h.eq(h.evidenceOf(event))
			h.eq(h.row(event).S("state"))
		})
	})
}

func outcomes(results []any) []any {
	out := []any{}
	for _, r := range results {
		out = append(out, field(r.(Obj), "outcome"))
	}
	return out
}

func Test21_HLT25_the_sender_and_a_concurrent_reconcile_never_undo_each_other(t *testing.T) {
	t.Run("a reconcile during the send that found nothing", func(t *testing.T) {
		mirror(t, hlt, "EverySettlementIsACompareAndSet.test_a_reconcile_during_the_send_that_found_nothing_leaves_the_accepted_send_to_the_next_tick", func(h *hl) {
			h.parentHistory()
			event := h.queuedEvent(regOpts{})
			var seen Obj
			w := &hooked{Adapter: h.host}
			w.send = func(requestID, thread, message string, settings *TaskSettings) (Obj, error) {
				seen = h.reconcile(requestID, h.host)
				return h.host.SendMessage(requestID, thread, message, settings)
			}
			result := h.attemptOn(event, w, nil)
			h.eq(field(seen, "state"))
			h.eq(truthy(field(result, "_settledElsewhere")))
			h.eq(h.row(event).S("state"))
			h.clock.Advance(20)
			policy := defaultTick()
			policy.maxSendsTick = 0
			h.tickWith(policy, h.host, h.checks)
			h.eq(h.row(event).S("state"))
			h.eq(len(h.host.sends))
		})
	})
	t.Run("a reconcile during the send that confirmed it", func(t *testing.T) {
		mirror(t, hlt, "EverySettlementIsACompareAndSet.test_a_reconcile_during_the_send_that_confirmed_it_survives_the_senders_unknown_result", func(h *hl) {
			h.parentHistory()
			event := h.queuedEvent(regOpts{})
			h.host.script = []string{"in_progress"}
			w := &hooked{Adapter: h.host}
			w.send = func(requestID, thread, message string, settings *TaskSettings) (Obj, error) {
				receipt, err := h.host.SendMessage(requestID, thread, message, settings)
				h.host.startTurn(parent, "", "inProgress", "requestId: "+requestID)
				h.reconcile(requestID, h.host)
				return receipt, err
			}
			result := h.attemptOn(event, w, nil)
			h.eq(truthy(field(result, "_settledElsewhere")))
			h.eq(field(result, "_deliveryState"))
			row := h.row(event)
			h.eq([]any{row.S("state"), row.Opt("dispatch_turn_id")})
			h.eq(h.evidenceOf(event))
		})
	})
	t.Run("a reconcile in another process that read the send in flight", func(t *testing.T) {
		mirror(t, hlt, "EverySettlementIsACompareAndSet.test_a_reconcile_that_read_the_send_in_flight_cannot_undo_the_senders_settlement", func(h *hl) {
			h.parentHistory()
			event := h.queuedEvent(regOpts{})
			readDone, settled := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var seen Obj
			var failure error
			var wg sync.WaitGroup
			reconcileElsewhere := func(requestID string) {
				defer wg.Done()
				other, err := store.Open(context.Background(), h.store.Path, "")
				if err != nil {
					failure = err
					once.Do(func() { close(readDone) })
					return
				}
				defer func() { _ = other.Close() }()
				waits := &hooked{Adapter: h.host}
				waits.findToken = func(thread, token string, limit int, messageOnly bool) (TokenScan, error) {
					scan, err := h.host.FindToken(thread, token, limit, messageOnly)
					once.Do(func() { close(readDone) })
					<-settled
					return scan, err
				}
				seen, failure = NewReconciler(NewService(other, h.clock)).ReconcileAttempt(context.Background(), requestID, waits, nil)
			}
			w := &hooked{Adapter: h.host}
			w.send = func(requestID, thread, message string, settings *TaskSettings) (Obj, error) {
				wg.Add(1)
				go reconcileElsewhere(requestID)
				<-readDone
				return h.host.SendMessage(requestID, thread, message, settings)
			}
			result := h.attemptOn(event, w, nil)
			close(settled)
			wg.Wait()
			h.eq(anyErrors(failure))
			h.eq(str(result, "deliveryState"))
			h.eq(truthy(field(seen, "changed")))
			h.eq(h.attemptStates(event))
			row := h.row(event)
			h.eq([]any{row.S("state"), row.Opt("dispatch_turn_id")})
		})
	})
}

func anyErrors(err error) []any {
	if err == nil {
		return []any{}
	}
	return []any{err.Error()}
}

func Test21_HLT26_a_typed_item_changes_the_fingerprint_and_readback(t *testing.T) {
	mirror(t, hlt, "EverySettlementIsACompareAndSet.test_a_typed_item_leaves_the_fingerprint_and_readback_working", func(h *hl) {
		h.item(parent, "t-x", "plain", "")
		_, err := h.host.RecipientFingerprint(parent)
		mustDo(t, err)
		h.item(parent, "t-x", "del-echo-a1", "commandExecution")
		after, err := h.host.RecipientFingerprint(parent)
		mustDo(t, err)
		h.eq(after)
		anyItem, err := h.host.FindToken(parent, "del-echo-a1", 200, false)
		mustDo(t, err)
		h.eq(anyItem.Found)
		only, err := h.host.FindToken(parent, "del-echo-a1", 200, true)
		mustDo(t, err)
		h.eq(only.Found)
	})
}

func Test21_HLT27_an_ack_is_judged_against_the_delivery_it_read(t *testing.T) {
	loseFirstTurnAndRequeue := func(h *hl) (string, string) {
		event, _, lost := h.dispatched()
		h.hostReloadsLosing(lost)
		h.clock.Advance(120)
		policy := defaultTick()
		policy.maxSendsTick = 0
		h.tickWith(policy, h.host, h.checks)
		h.eq(h.attemptStates(event))
		h.clock.Advance(10)
		return event, lost
	}
	t.Run("a folded send confirmed during the read", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementIsJudgedAgainstTheDeliveryItRead.test_a_folded_send_confirmed_during_the_read_keeps_the_ack_and_completes_it", func(h *hl) {
			event, request := h.foldedDelivery(true, 0, 0)
			record, err := h.ack.Acknowledge(h.ctx, event, "folded", AckProof(event, "folded"), true, nil, actsDuringTheTurnRead(h.host, func() { h.reconcile(request, h.host) }))
			mustDo(t, err)
			h.eq(field(record, "_verified"))
			h.eq(h.row(event).S("state"))
			h.clock.Advance(5)
			h.tick()
			h.eq(h.row(event).S("state"))
		})
	})
	t.Run("an ACK naming a lost turn", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementIsJudgedAgainstTheDeliveryItRead.test_an_ack_naming_a_lost_turn_cannot_close_the_redelivery_confirmed_during_its_read", func(h *hl) {
			event, lost := loseFirstTurnAndRequeue(h)
			var acked Obj
			w := &hooked{Adapter: h.host}
			w.send = func(requestID, thread, message string, settings *TaskSettings) (Obj, error) {
				receipt, err := h.host.SendMessage(requestID, thread, message, settings)
				var aerr error
				acked, aerr = h.ack.Acknowledge(h.ctx, event, lost, AckProof(event, lost), true, nil, actsDuringTheTurnRead(h.host, func() { h.reconcile(requestID, h.host) }))
				mustDo(t, aerr)
				return receipt, err
			}
			h.attemptOn(event, w, nil)
			h.eq(field(acked, "_verified"))
			h.eq(h.row(event).S("state"))
			h.clock.Advance(5)
			h.tick()
			h.eq(h.row(event).S("state"))
			h.eq(h.ackRow(event).Opt("last_reason"))
			h.eq(h.nextAction())
		})
	})
	t.Run("an ACK is not promoted onto an attempt sent during its read", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementIsJudgedAgainstTheDeliveryItRead.test_an_ack_is_not_promoted_onto_an_attempt_sent_during_its_read", func(h *hl) {
			event, first, lost := h.dispatched()
			h.hostReloadsLosing(lost)
			h.clock.Advance(120)
			record, err := h.ack.Acknowledge(h.ctx, event, lost, AckProof(event, lost), true, nil, actsDuringTheTurnRead(h.host, func() {
				_, err := h.rc.CheckDispatchedTurn(h.ctx, first, h.host)
				mustDo(t, err)
				h.attemptOn(event, h.host, nil)
			}))
			mustDo(t, err)
			h.eq(h.attemptStates(event))
			h.eq(field(record, "_verified"))
			h.eq(h.row(event).S("state"))
		})
	})
	t.Run("a pending ACK read against a delivery that moved", func(t *testing.T) {
		mirror(t, hlt, "AnAcknowledgementIsJudgedAgainstTheDeliveryItRead.test_a_pending_ack_read_against_a_delivery_that_moved_is_checked_again", func(h *hl) {
			event, request := h.foldedDelivery(true, 0, 0)
			_, err := h.ack.Acknowledge(h.ctx, event, "folded", AckProof(event, "folded"), true, nil, nil)
			mustDo(t, err)
			results, err := h.ack.VerifyPendingAcks(h.ctx, actsDuringTheTurnRead(h.host, func() { h.reconcile(request, h.host) }), 8, nil)
			mustDo(t, err)
			h.eq(outcomes(results))
			h.eq(h.row(event).S("state"))
			again, err := h.ack.VerifyPendingAcks(h.ctx, h.host, 8, nil)
			mustDo(t, err)
			h.eq(outcomes(again))
			h.eq(h.row(event).S("state"))
		})
	})
}
