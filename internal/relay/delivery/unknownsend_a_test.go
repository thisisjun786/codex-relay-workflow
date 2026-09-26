package delivery

import (
	"fmt"
	"strings"
	"testing"
)

// test_unknown_send_lost.py USL-1..USL-11 (method: hostloss_harness_test.go).

const usl = "test_unknown_send_lost"

func uslMessage(request string) string {
	return "[codex-session-relay] verification request\nrequestId: " + request
}

// unknownSend is UnknownSendCase.unknown_send.
func (h *hl) unknownSend(history, restarted bool) (string, string) {
	if history {
		h.parentHistory()
	}
	event := h.queuedEvent(regOpts{})
	h.host.script = []string{"transport_unknown"}
	record := h.attemptOn(event, h.host, nil)
	h.eq(field(record, "transportReceiptStatus"))
	h.eq(field(record, "turnId"))
	h.eq(h.row(event).S("state"))
	if restarted {
		h.host.threads[parent].status = "notLoaded"
	}
	return event, str(record, "requestId")
}

func (h *hl) ticks(count int, seconds float64) {
	for i := 0; i < count; i++ {
		h.clock.Advance(seconds)
		h.tick()
	}
}

func (h *hl) sendsTo(recipient string) []any {
	out := []any{}
	for _, s := range h.host.sends {
		if s.thread == recipient {
			out = append(out, s.requestID)
		}
	}
	return out
}

func (h *hl) mark(request string) any {
	return h.one("SELECT recipient_scan FROM attempts WHERE request_id = ?", request).Opt("recipient_scan")
}

// argvTail is shlex.split(command)[-5:] for the commands recoveryCommand renders.
func argvTail(command any) []any {
	text, _ := command.(string)
	var argv []string
	for _, word := range strings.Fields(text) {
		argv = append(argv, strings.Trim(word, "'"))
	}
	out := []any{}
	for _, w := range argv[max(0, len(argv)-5):] {
		out = append(out, w)
	}
	return out
}

// assertHeld is UnknownSendCase.assert_held: the values each of its assertions reads.
func (h *hl) assertHeld(event, request string) {
	row := h.row(event)
	h.eq([]any{row.S("state"), row.Opt("hold_reason")})
	h.eq([]any{row.Opt("dispatch_evidence"), row.Opt("dispatch_turn_id")})
	h.eq(h.mark(request))
	states := h.attemptStates(event)
	h.eq(states[len(states)-1])
	h.eq(h.statusPair(event))
	h.eq(h.nextAction())
	h.eq(field(h.completionDelivery(), "turnCheck"))
	recovery := sub(h.assignment(), "recovery")
	h.eq([]any{field(recovery, "actor"), field(recovery, "reason")})
	h.eq(argvTail(field(recovery, "command")))
}

func Test21_USL01_an_unknown_send_without_trace_is_held_for_the_parent_by_name(t *testing.T) {
	const cls = "AnUnknownSendTheHostKeptNoTraceOf."
	t.Run("daemon tick", func(t *testing.T) {
		mirror(t, usl, cls+"test_it_is_held_for_the_parent_by_name_and_never_sent_again", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.eq(h.attemptStates(event))
			h.ticks(6, 700)
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("recipient still held by its host", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_recipient_its_host_still_holds_is_held_the_same_way", func(h *hl) {
			event, first := h.unknownSend(true, false)
			h.ticks(3, 120)
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("reconcile twice", func(t *testing.T) {
		mirror(t, usl, cls+"test_reconciling_it_again_keeps_the_hold_and_sends_nothing", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.clock.Advance(120)
			h.reconcile(first, h.host)
			again := h.reconcile(first, h.host)
			h.eq([]any{field(again, "state"), field(again, "nextExpectedAction")})
			h.assertHeld(event, first)
			h.eq(h.attemptStates(event))
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("an ACK without a trace", func(t *testing.T) {
		mirror(t, usl, cls+"test_an_acknowledgement_without_a_trace_is_held_the_same_way", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.clock.Advance(5)
			turn := h.host.startTurn(parent, "ack-turn", "completed", "")
			_, err := h.ack.Acknowledge(h.ctx, event, turn.TurnID, AckProof(event, turn.TurnID), true, nil, h.host)
			mustDo(t, err)
			h.eq(h.ackRow(event).Opt("last_reason"))
			h.ticks(3, 120)
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
			h.eq(h.faultIn("open", "broken"))
		})
	})
}

func Test21_USL02_reconcile_names_the_parent_the_reason_and_the_command(t *testing.T) {
	mirror(t, usl, "AnUnknownSendTheHostKeptNoTraceOf.test_reconcile_names_the_parent_the_reason_and_the_command_without_sending", func(h *hl) {
		event, first := h.unknownSend(true, true)
		h.clock.Advance(120)
		outcome := h.reconcile(first, h.host)
		h.eq([]any{field(outcome, "state"), field(outcome, "evidence")})
		h.eq([]any{field(outcome, "nextExpectedAction"), field(outcome, "reason")})
		h.eq(field(sub(outcome, "recipientTrace"), "finding"))
		recovery := sub(outcome, "recovery")
		h.eq(field(recovery, "actor"))
		h.eq(argvTail(field(recovery, "command")))
		h.assertHeld(event, first)
		h.eq(h.sendsTo(parent))
	})
}

// withReadUnknownSend replaces the hostloss.read_unknown_send seam for one call (mock.patch).
func withReadUnknownSend(replacement func(Adapter, Clock, Row, Row, string) Reading, body func()) {
	original := readUnknownSend
	readUnknownSend = replacement
	defer func() { readUnknownSend = original }()
	body()
}

func Test21_USL03_a_hold_that_loses_its_race_to_a_confirmation_reports_the_confirmation(t *testing.T) {
	mirror(t, usl, "AnUnknownSendTheHostKeptNoTraceOf.test_a_hold_that_loses_its_race_to_a_confirmation_reports_the_confirmation", func(h *hl) {
		event, first := h.unknownSend(true, true)
		h.clock.Advance(120)
		original := readUnknownSend
		var outcome Obj
		withReadUnknownSend(func(a Adapter, c Clock, attempt, delivery Row, receipt string) Reading {
			reading := original(a, c, attempt, delivery, receipt)
			h.host.startTurn(parent, "", "completed", uslMessage(first))
			out, err := h.rc.ReconcileAttempt(h.ctx, first, h.host, nil)
			mustDo(h.t, err)
			h.eq(field(out, "evidence"))
			return reading
		}, func() { outcome = h.reconcile(first, h.host) })
		h.eq([]any{field(outcome, "deliveryState"), field(outcome, "evidence")})
		_, has := get(outcome, "nextExpectedAction")
		h.eq(has)
		row := h.row(event)
		h.eq([]any{row.S("state"), row.Opt("hold_reason")})
		h.eq(h.nextAction())
		h.eq(h.sendsTo(parent))
	})
}

func Test21_USL04_no_hold_is_named_while_the_daemon_can_still_decide(t *testing.T) {
	const cls = "AnUnknownSendTheHostKeptNoTraceOf."
	t.Run("within the allowance", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_send_too_recent_to_judge_is_read_again_until_it_is_held", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.ticks(1, 20)
			h.eq(h.attemptStates(event))
			h.eq(h.row(event).Opt("hold_reason"))
			h.eq(h.nextAction())
			h.ticks(1, 60)
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("a turn running since the send", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_turn_still_running_since_the_send_holds_the_decision_until_it_ends", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.clock.Advance(5)
			running := h.host.startTurn(parent, "", "inProgress", "")
			h.ticks(1, 120)
			h.eq(h.attemptStates(event))
			h.eq(h.row(event).Opt("hold_reason"))
			h.eq(h.nextAction())
			h.host.finishTurn(parent, running.TurnID, "completed")
			h.ticks(1, 30)
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("an older turn still running", func(t *testing.T) {
		mirror(t, usl, cls+"test_an_older_turn_still_running_holds_the_decision", func(h *hl) {
			event, first := h.foldedUnknownSend(205, true, "")
			t := h.host.threads[parent]
			kept := t.items[:0:0]
			for _, item := range t.items {
				if !strings.Contains(item[1], first) {
					kept = append(kept, item)
				}
			}
			t.items = kept
			h.ticks(1, 120)
			h.eq(h.attemptStates(event))
			h.eq(h.row(event).Opt("hold_reason"))
			h.eq(h.nextAction())
			h.eq(h.sendsTo(parent))
		})
	})
}

// foldedUnknownSend is UnknownSendCase.folded_unknown_send; kind "" is the message itself.
func (h *hl) foldedUnknownSend(later int, running bool, kind string) (string, string) {
	h.parentHistory()
	event := h.queuedEvent(regOpts{})
	h.host.startTurn(parent, "folded", "inProgress", "")
	h.clock.Advance(120)
	h.host.script = []string{"transport_unknown"}
	first := str(h.attemptOn(event, h.host, nil), "requestId")
	if kind == "" {
		h.item(parent, "folded", uslMessage(first), "")
	} else {
		h.item(parent, "folded", "hook saw "+first, kind)
	}
	for n := 0; n < later; n++ {
		h.item(parent, "folded", fmt.Sprintf("later work %d", n), "commandExecution")
	}
	if !running {
		h.host.finishTurn(parent, "folded", "completed")
	}
	return event, first
}

func Test21_USL05_a_message_that_turns_up_later_confirms_the_send_and_clears_the_hold(t *testing.T) {
	t.Run("after a lost hold", func(t *testing.T) {
		mirror(t, usl, "AnUnknownSendTheHostKeptNoTraceOf.test_a_message_that_turns_up_after_the_hold_confirms_the_send_and_clears_it", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.host.startTurn(parent, "", "completed", uslMessage(first))
			h.ticks(1, 120)
			row := h.row(event)
			h.eq([]any{row.S("state"), row.Opt("hold_reason")})
			h.eq(h.evidenceOf(event))
			h.eq(h.nextAction())
			_, has := get(h.assignment(), "recovery")
			h.eq(has)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("after an undecided hold", func(t *testing.T) {
		mirror(t, usl, "AnUndecidedReadingIsHeldByName.test_a_message_found_later_clears_the_undecided_hold", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.clock.Advance(2)
			hook := h.host.startTurn(parent, "", "completed", "")
			h.item(parent, hook.TurnID, "hook saw "+first, "hookPrompt")
			h.ticks(1, 120)
			h.eq(h.row(event).Opt("hold_reason"))
			h.item(parent, hook.TurnID, uslMessage(first), "")
			h.ticks(1, 120)
			row := h.row(event)
			h.eq([]any{row.S("state"), row.Opt("hold_reason")})
			h.eq(h.nextAction())
		})
	})
	t.Run("between the two scans", func(t *testing.T) {
		mirror(t, usl, "AnUnknownSendTheHostKeptNoTraceOf.test_a_message_that_lands_between_the_two_scans_confirms_the_send", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.clock.Advance(3)
			late := h.host.startTurn(parent, "", "completed", "")
			h.clock.Advance(120)
			lands := &hooked{Adapter: h.host}
			lands.findToken = func(thread, token string, limit int, messageOnly bool) (TokenScan, error) {
				scan, err := h.host.FindToken(thread, token, limit, messageOnly)
				h.item(parent, late.TurnID, uslMessage(first), "")
				return scan, err
			}
			outcome := h.reconcile(first, lands)
			h.eq(field(outcome, "evidence"))
			h.eq(h.row(event).S("state"))
			h.eq(h.attemptStates(event))
			h.eq(h.sendsTo(parent))
		})
	})
}

func Test21_USL06_an_unknown_send_after_a_host_loss_is_held_and_claims_no_dispatch(t *testing.T) {
	t.Run("lost: dispatch cleared", func(t *testing.T) {
		mirror(t, usl, "AnUnknownSendTheHostKeptNoTraceOf.test_an_unknown_send_after_a_host_lost_turn_is_held_and_claims_no_dispatch", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostLoses(turn, true)
			h.host.script = []string{"transport_unknown"}
			h.ticks(1, 120)
			h.eq(h.attemptStates(event))
			second := h.attemptsFor(event)[1].S("request_id")
			h.host.threads[parent].status = "notLoaded"
			h.ticks(3, 120)
			h.eq(h.attemptStates(event))
			h.assertHeld(event, second)
			h.eq(h.sendsTo(parent))
			_ = first
		})
	})
	t.Run("undecided: dispatch cleared", func(t *testing.T) {
		mirror(t, usl, "AnUndecidedReadingIsHeldByName.test_an_undecided_redelivery_after_a_host_loss_keeps_no_dispatch_evidence", func(h *hl) {
			event, first, turn := h.dispatched()
			h.hostLoses(turn, true)
			h.clock.Advance(120)
			h.reconcile(first, h.host)
			row := h.row(event)
			h.eq([]any{row.Opt("dispatch_evidence"), row.Opt("dispatch_turn_id")})
			claimed, err := h.delivery.claim(h.ctx, event, h.clock.Now(), "relay", parent)
			mustDo(t, err)
			request := claimed.requestID
			h.host.ledger[request] = Obj{{Key: "requestId", Value: request}, {Key: "operation", Value: "send_message_to_thread"}, {Key: "status", Value: "outcome_unknown"},
				{Key: "threadId", Value: parent}, {Key: "retrySafe", Value: false}, {Key: "error", Value: "TransportError: turn/start: response unavailable; do not resend"}}
			h.clock.Advance(2)
			hook := h.host.startTurn(parent, "", "completed", "")
			h.item(parent, hook.TurnID, "hook saw "+request, "hookPrompt")
			h.clock.Advance(120)
			h.reconcile(request, h.host)
			row = h.row(event)
			h.eq([]any{row.S("state"), row.Opt("hold_reason")})
			h.eq([]any{row.Opt("dispatch_evidence"), row.Opt("dispatch_turn_id")})
		})
	})
}

func Test21_USL07_a_send_folded_into_an_older_turn_is_found_there(t *testing.T) {
	const cls = "AnUnknownSendTheHostKeptNoTraceOf."
	t.Run("folded into an older turn", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_send_folded_into_an_older_turn_is_found_there_and_not_sent_again", func(h *hl) {
			event, first := h.foldedUnknownSend(205, false, "")
			h.ticks(1, 120)
			h.eq(h.evidenceOf(event))
			h.eq(h.row(event).S("state"))
			h.ticks(3, 120)
			h.eq(h.attemptStates(event))
			h.eq(h.sendsTo(parent))
			_ = first
		})
	})
	t.Run("a turn begun just after the send", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_turn_begun_just_after_the_send_does_not_hide_the_folded_one", func(h *hl) {
			event, _ := h.foldedUnknownSend(205, false, "")
			h.clock.Advance(0.5)
			h.host.startTurn(parent, "later", "completed", "unrelated")
			h.ticks(1, 120)
			h.eq(h.evidenceOf(event))
			h.eq(h.row(event).S("state"))
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("a turn begun just before the send", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_send_folded_into_a_turn_begun_just_before_it_is_found_there", func(h *hl) {
			h.parentHistory()
			event := h.queuedEvent(regOpts{})
			h.host.startTurn(parent, "just-before", "inProgress", "")
			h.clock.Advance(10)
			h.host.script = []string{"transport_unknown"}
			first := str(h.attemptOn(event, h.host, nil), "requestId")
			h.item(parent, "just-before", uslMessage(first), "")
			for n := 0; n < 205; n++ {
				h.item(parent, "just-before", fmt.Sprintf("later work %d", n), "commandExecution")
			}
			h.host.finishTurn(parent, "just-before", "completed")
			h.host.threads[parent].status = "notLoaded"
			h.ticks(1, 120)
			h.eq(h.evidenceOf(event))
			h.eq(h.row(event).S("state"))
			h.eq(h.sendsTo(parent))
		})
	})
}

func Test21_USL08_undecided_readings_are_held_for_the_parent_by_name(t *testing.T) {
	heldFolded := func(name string, later func(h *hl)) {
		t.Run(name, func(t *testing.T) {
			mirror(t, usl, "AnUnknownSendTheHostKeptNoTraceOf."+name, func(h *hl) {
				event, first := h.foldedUnknownSend(205, false, "hookPrompt")
				later(h)
				h.host.threads[parent].status = "notLoaded"
				h.ticks(1, 120)
				h.assertHeld(event, first)
				if strings.Contains(name, "of_the_folded_turn") {
					h.ticks(3, 700)
				}
				h.eq(h.sendsTo(parent))
			})
		})
	}
	heldFolded("test_a_token_only_in_a_hook_prompt_of_the_folded_turn_is_held_by_name", func(*hl) {})
	heldFolded("test_a_hook_prompt_in_the_folded_turn_behind_a_later_turn_is_held_by_name", func(h *hl) {
		h.clock.Advance(0.5)
		h.host.startTurn(parent, "later", "completed", "unrelated")
	})
	t.Run("2000+205 items", func(t *testing.T) {
		mirror(t, usl, "AnUnknownSendTheHostKeptNoTraceOf.test_an_older_turn_too_long_to_read_through_holds_the_send_by_name", func(h *hl) {
			event, first := h.foldedUnknownSend(0, false, "")
			t := h.host.threads[parent]
			carried := t.items[len(t.items)-1]
			t.items = t.items[:len(t.items)-1]
			for n := 0; n < 2000; n++ {
				h.item(parent, "folded", fmt.Sprintf("earlier work %d", n), "commandExecution")
			}
			t.items = append(t.items, carried)
			for n := 0; n < 205; n++ {
				h.item(parent, "folded", fmt.Sprintf("later work %d", n), "commandExecution")
			}
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
		})
	})
	const cls = "AnUndecidedReadingIsHeldByName."
	t.Run("empty listing", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_parent_listing_no_turns_holds_the_send_by_name", func(h *hl) {
			event, first := h.unknownSend(false, true)
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.ticks(3, 700)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("another item type", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_token_only_in_another_item_type_is_held_by_name", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.clock.Advance(2)
			hook := h.host.startTurn(parent, "", "completed", "")
			h.item(parent, hook.TurnID, "hook saw "+first, "hookPrompt")
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("scan bounded before the send", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_scan_bounded_before_the_send_is_held_by_name", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.clock.Advance(2)
			busy := h.host.startTurn(parent, "", "completed", "")
			for n := 0; n < 5; n++ {
				h.item(parent, busy.TurnID, fmt.Sprintf("work %d", n), "commandExecution")
			}
			h.host.scanLimit = 3
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("listing never reaches the send", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_listing_that_never_reaches_the_send_is_held_by_name", func(h *hl) {
			event, first := h.unknownSend(true, true)
			h.clock.Advance(120)
			outcome := h.reconcile(first, h.neverReachesTheSend())
			h.eq(field(outcome, "nextExpectedAction"))
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
		})
	})
}

func (h *hl) neverReachesTheSend() *hooked {
	return &hooked{Adapter: h.host, findDispatched: func(string, string, float64) (TurnPresence, error) {
		return TurnPresence{}, &ListingBounded{"the bounded listing never reached the send"}
	}}
}

func Test21_USL09_a_listed_turn_without_an_id_is_not_taken_for_the_sends_turn(t *testing.T) {
	mirror(t, usl, "TheListingSinceASendWithNoTurnId.test_a_listed_turn_without_an_id_is_not_taken_for_the_sends_turn", func(h *hl) {
		hundred, ten := 100.0, 10.0
		presence, err := FindInListing([]ListingPage{{Turns: []TurnInfo{{TurnID: "", Status: "completed", StartedAt: &hundred}, {TurnID: "older", Status: "completed", StartedAt: &ten}}}}, "", 150.0)
		mustDo(t, err)
		h.eq([]any{presence.Finding, presence.Stop, presence.Seen, strs(presence.Older)})
		statuses := []any{}
		for _, turn := range presence.SeenTurns {
			statuses = append(statuses, turn.Status)
		}
		h.eq(statuses)
		h.eq(presence.StopTurn.TurnID)
	})
}

func Test21_USL10_a_send_the_transport_has_not_answered_is_held_by_name(t *testing.T) {
	const cls = "ASendTheTransportHasNotAnsweredIsHeldByName."
	unfinished := func(h *hl) (string, string) {
		h.parentHistory()
		event := h.queuedEvent(regOpts{})
		h.host.script = []string{"in_progress"}
		request := str(h.attemptOn(event, h.host, nil), "requestId")
		h.host.threads[parent].status = "notLoaded"
		return event, request
	}
	t.Run("in_progress receipt", func(t *testing.T) {
		mirror(t, usl, cls+"test_an_unfinished_receipt_is_held_for_the_parent_by_name", func(h *hl) {
			event, first := unfinished(h)
			h.clock.Advance(120)
			outcome := h.reconcile(first, h.host)
			h.eq([]any{field(outcome, "nextExpectedAction"), field(outcome, "reason")})
			h.ticks(3, 120)
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("settles later", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_receipt_that_settles_later_lets_the_rule_decide", func(h *hl) {
			event, first := unfinished(h)
			h.ticks(1, 120)
			h.eq(h.row(event).Opt("hold_reason"))
			receipt := append(Obj(nil), h.host.ledger[first]...)
			receipt = set(receipt, "status", "outcome_unknown")
			receipt = set(receipt, "error", "TransportError: turn/start: no answer")
			h.host.ledger[first] = receipt
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("no receipt", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_claim_with_no_receipt_is_held_for_the_parent_by_name", func(h *hl) {
			h.parentHistory()
			event := h.queuedEvent(regOpts{})
			claimed, err := h.delivery.claim(h.ctx, event, h.clock.Now(), "relay", parent)
			mustDo(t, err)
			h.clock.Advance(120)
			outcome := h.reconcile(claimed.requestID, h.host)
			h.eq([]any{field(outcome, "nextExpectedAction"), field(outcome, "reason")})
			h.eq(h.attemptStates(event))
			h.eq(sendsJSON(h.host))
		})
	})
	t.Run("unreadable receipt", func(t *testing.T) {
		mirror(t, usl, cls+"test_an_unreadable_receipt_takes_no_reading", func(h *hl) {
			event, first := unfinished(h)
			h.clock.Advance(120)
			h.host.readFailures["get_operation"] = true
			outcome := h.reconcile(first, h.host)
			_, has := get(outcome, "recipientTrace")
			h.eq(has)
			h.eq(field(outcome, "nextExpectedAction"))
			h.eq(h.row(event).Opt("hold_reason"))
		})
	})
}

func Test21_USL11_a_deciding_reading_replaces_an_undecided_hold_and_nothing_else_does(t *testing.T) {
	const cls = "AnUndecidedReadingIsHeldByName."
	t.Run("a later deciding reading", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_later_reading_that_decides_replaces_the_undecided_hold", func(h *hl) {
			event, first := h.unknownSend(false, true)
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.host.startTurn(parent, "", "completed", "the operator asked something")
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("a pass that cannot decide", func(t *testing.T) {
		mirror(t, usl, cls+"test_a_pass_that_cannot_decide_keeps_the_undecided_hold", func(h *hl) {
			event, first := h.unknownSend(false, true)
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.host.startTurn(parent, "", "inProgress", "")
			h.clock.Advance(20)
			pending := h.reconcile(first, h.host)
			h.eq(field(sub(pending, "recipientTrace"), "pending"))
			h.eq(field(pending, "nextExpectedAction"))
			h.assertHeld(event, first)
			h.host.readFailures["find_token"] = true
			unread := h.reconcile(first, h.host)
			_, has := get(unread, "recipientTrace")
			h.eq(has)
			h.eq([]any{field(unread, "nextExpectedAction"), field(unread, "reason")})
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("an older reading written last", func(t *testing.T) {
		mirror(t, usl, cls+"test_an_older_reading_does_not_overwrite_a_newer_hold", func(h *hl) {
			event, first := h.unknownSend(false, true)
			h.ticks(1, 120)
			h.assertHeld(event, first)
			original := readUnknownSend
			var raced []Reading
			var outcome Obj
			withReadUnknownSend(func(a Adapter, c Clock, attempt, delivery Row, receipt string) Reading {
				reading := original(a, c, attempt, delivery, receipt)
				if len(raced) == 0 {
					raced = append(raced, reading)
					h.host.startTurn(parent, "", "completed", "the operator asked")
					newer, err := h.rc.ReconcileAttempt(h.ctx, first, h.host, nil)
					mustDo(h.t, err)
					h.eq(field(newer, "reason"))
				}
				return reading
			}, func() { outcome = h.reconcile(first, h.host) })
			h.eq(field(raced[0], "undecided"))
			h.eq(truthy(field(outcome, "changed")))
			h.eq([]any{field(outcome, "nextExpectedAction"), field(outcome, "reason")})
			h.eq(argvTail(field(sub(outcome, "recovery"), "command")))
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
		})
	})
	t.Run("re-read though the items did not change", func(t *testing.T) {
		mirror(t, usl, cls+"test_an_undecided_hold_is_read_again_later_though_the_items_did_not_change", func(h *hl) {
			event, first := h.unknownSend(false, true)
			h.ticks(1, 120)
			h.assertHeld(event, first)
			h.host.startTurn(parent, "", "completed", "")
			h.ticks(1, 30)
			h.assertHeld(event, first)
			h.ticks(1, 600)
			h.assertHeld(event, first)
			h.eq(h.sendsTo(parent))
		})
	})
}
