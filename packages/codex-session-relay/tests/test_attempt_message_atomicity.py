"""The message a send carries must be the message that send's attempt froze.

Reproduces the interleaving the coordinator found: an earlier slow caller settles a busy
attempt in the window between a later caller preparing its message and claiming its attempt.
Before the fix the later caller sent transport request -a2 carrying a message that said -a1,
and lost-response reconciliation searches the recipient for the token the message carried.
"""

from codex_session_relay.delivery import DeliveryService
from codex_session_relay.transport import DISPATCHED

from .support import PARENT, DeliveryTestCase


class _Args:
    def __init__(self, event, message=True):
        self.event = event
        self.message = message


class _Services:
    """Only what cmd_show reads, so the command surface is exercised, not re-implemented."""

    def __init__(self, case):
        self.store = case.store
        self.intake = case.intake
        self.delivery = case.delivery


def token(message):
    """The requestId the recipient would actually see."""
    for line in message.splitlines():
        if line.startswith("requestId: "):
            return line.split(": ", 1)[1]
    raise AssertionError("message carries no requestId")


class AttemptMessageAtomicity(DeliveryTestCase):
    def test_ordinary_send_carries_its_own_request_id(self):
        _relationship, event_id = self.queued_event()
        record = self.attempt(event_id)
        request_id, _thread, message, _outcome = self.adapter.sends[-1]
        self.assertEqual(request_id, record["requestId"])
        self.assertEqual(token(message), request_id)

    def test_retry_carries_the_retry_request_id(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("busy")
        first = self.attempt(event_id)
        self.assertEqual(first["deliveryState"], "deferred_busy")
        later = self.clock.now() + 10_000
        record = self.attempt(event_id, now=later)
        self.assertEqual(record["attemptNo"], first["attemptNo"] + 1)
        self.assertNotEqual(record["requestId"], first["requestId"])
        request_id, _thread, message, _outcome = self.adapter.sends[-1]
        self.assertEqual(token(message), request_id)

    def test_interleaved_earlier_caller_cannot_shift_the_token(self):
        """The reproduced race, driven through the real pre-claim seam.

        The earlier caller is a second DeliveryService over the same store whose captured now
        is 60 seconds older, so its busy deferral is already due when the later caller claims.
        It runs in the last window before the later caller's claim.
        """
        _relationship, event_id = self.queued_event()
        earlier = DeliveryService(self.store, self.registry, self.intake, self.clock)
        original = self.adapter.list_turn_ids
        fired = []

        def settle_earlier_attempt_first(thread_id, limit=20):
            ids = original(thread_id, limit=limit)
            if not fired:
                fired.append(True)
                self.adapter.script("busy")
                earlier.attempt(
                    event_id, self.adapter, now=self.clock.now() - 60,
                    owner="earlier-slow-caller",
                )
            return ids

        self.adapter.list_turn_ids = settle_earlier_attempt_first
        try:
            record = self.attempt(event_id)
        finally:
            self.adapter.list_turn_ids = original

        self.assertTrue(fired, "the interleaving never ran; the test proves nothing")
        self.assertIsNotNone(record, "the later caller should still claim its own attempt")
        request_id, _thread, message, _outcome = self.adapter.sends[-1]
        self.assertEqual(
            token(message), request_id,
            "the sent bytes must carry the request id the transport actually used",
        )
        self.assertEqual(record["requestId"], request_id)
        for sent_request, _t, sent_message, _o in self.adapter.sends:
            self.assertEqual(token(sent_message), sent_request)

    def test_persisted_bytes_belong_to_their_own_attempt(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("busy")
        self.attempt(event_id)
        self.attempt(event_id, now=self.clock.now() + 10_000)
        rows = self.delivery.attempt_messages(event_id)
        self.assertEqual(len(rows), 2)
        for row in rows:
            self.assertEqual(token(row["message"]), row["requestId"])
            self.assertEqual(
                self.delivery.sent_message(row["requestId"]), row["message"],
            )

    def test_inspection_after_send_reports_the_sent_attempt_not_the_next_one(self):
        _relationship, event_id = self.queued_event()
        record = self.attempt(event_id)
        rows = self.delivery.attempt_messages(event_id)
        self.assertEqual(len(rows), 1)
        entry = rows[0]
        self.assertEqual(entry["requestId"], record["requestId"])
        self.assertEqual(entry["status"], "dispatched")
        self.assertEqual(token(entry["message"]), record["requestId"])
        preview = self.delivery.preview_message(event_id)
        self.assertNotEqual(
            token(preview), record["requestId"],
            "a preview describes the NEXT attempt and must not be mistaken for the sent one",
        )

    def test_lost_response_reconciliation_searches_the_token_that_was_sent(self):
        """A dropped response is recovered by the token the recipient actually received."""
        _relationship, event_id = self.queued_event()
        self.adapter.script("transport_unknown")
        record = self.attempt(event_id)
        request_id = record["requestId"]
        _sent_request, _thread, message, _outcome = self.adapter.sends[-1]
        self.assertEqual(token(message), request_id)
        self.assertEqual(self.delivery.sent_message(request_id), message)
        self.assertEqual(record["deliveryState"], "held_uncertain")
        # The recipient did receive it; only our response was lost. Reconciliation must find
        # the token the MESSAGE carried, which is why it has to equal the request id.
        self.adapter.start_turn(PARENT, status="completed", text=message)
        resolved = self.reconciler.reconcile_attempt(
            request_id, self.adapter, now=self.clock.now(),
        )
        self.assertEqual(resolved["state"], DISPATCHED)
        self.assertEqual(resolved["evidence"], "turn_found")
        # The scan succeeds only because the bytes in the recipient carry this exact id. A
        # message rendered for a different attempt would leave this token nowhere to be found.
        self.assertIn(request_id, message)

    def test_bytes_prepared_but_never_dispatched_are_not_called_sent(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("busy")
        record = self.attempt(event_id)
        entry = self.delivery.attempt_messages(event_id)[-1]
        self.assertEqual(entry["requestId"], record["requestId"])
        self.assertEqual(entry["status"], "confirmed_unsent")

    def test_an_unknown_transport_outcome_is_uncertain_not_sent(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("transport_unknown")
        record = self.attempt(event_id)
        entry = self.delivery.attempt_messages(event_id)[-1]
        self.assertEqual(entry["requestId"], record["requestId"])
        self.assertEqual(entry["status"], "uncertain")

    def test_attempt_older_than_the_table_is_unavailable_not_reinvented(self):
        _relationship, event_id = self.queued_event()
        record = self.attempt(event_id)
        with self.store.transaction() as db:
            db.execute("DELETE FROM attempt_messages WHERE request_id = ?", (record["requestId"],))
        entry = self.delivery.attempt_messages(event_id)[-1]
        self.assertEqual(entry["status"], "unavailable")
        self.assertIsNone(entry["message"])

    def test_dispatched_state_is_unchanged_by_the_new_bookkeeping(self):
        _relationship, event_id = self.queued_event()
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)

    def test_show_message_returns_the_sent_attempt_after_a_send(self):
        from codex_session_relay.cli import cmd_show

        _relationship, event_id = self.queued_event()
        record = self.attempt(event_id)
        payload = cmd_show(_Services(self), _Args(event_id))
        entries = payload["attemptMessages"]
        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0]["requestId"], record["requestId"])
        self.assertEqual(entries[0]["status"], "dispatched")
        self.assertEqual(token(entries[0]["message"]), record["requestId"])
        # The old surface answered with attempt_count + 1 and would have shown -a2 here.
        self.assertNotIn("previewMessage", payload)

    def test_show_message_offers_a_preview_only_before_anything_is_prepared(self):
        from codex_session_relay.cli import cmd_show

        _relationship, event_id = self.queued_event()
        payload = cmd_show(_Services(self), _Args(event_id))
        self.assertEqual(payload["attemptMessages"], [])
        self.assertIn("previewMessage", payload)
        # Nothing has been claimed, so the preview describes attempt 1 and is the only thing
        # that can honestly be shown.
        self.assertTrue(token(payload["previewMessage"]).endswith("-a1"))
        record = self.attempt(event_id)
        after = cmd_show(_Services(self), _Args(event_id))
        self.assertNotIn("previewMessage", after)
        self.assertEqual(after["attemptMessages"][0]["requestId"], record["requestId"])

    def test_show_message_after_a_retry_lists_both_attempts_distinctly(self):
        from codex_session_relay.cli import cmd_show

        _relationship, event_id = self.queued_event()
        self.adapter.script("busy")
        first = self.attempt(event_id)
        second = self.attempt(event_id, now=self.clock.now() + 10_000)
        entries = cmd_show(_Services(self), _Args(event_id))["attemptMessages"]
        self.assertEqual(
            [e["requestId"] for e in entries], [first["requestId"], second["requestId"]],
        )
        self.assertEqual(
            [e["status"] for e in entries], ["confirmed_unsent", "dispatched"],
        )
        for entry in entries:
            self.assertEqual(token(entry["message"]), entry["requestId"])
