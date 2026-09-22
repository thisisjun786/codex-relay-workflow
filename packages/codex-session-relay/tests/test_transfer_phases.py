"""What the relay does with a transfer phase that expired.

The phases themselves are bounded in the bridge, where the waits actually happen, and they are
tested there against a real unix socket - `scripts/ci/packages.py` runs each package suite from
its own directory, so that fixture is not reachable from here. What this module owns is the
relay's half of the contract: which delivery a phase expiry is allowed to become.
"""

import unittest

from codex_session_relay.bridge_adapter import BridgeHostAdapter, _Transport
from codex_session_relay.transport import (
    HELD_UNCERTAIN,
    WITHHELD_PRE_SEND,
    assert_attempt_invariants,
    attempt_record,
    classify_operation_receipt,
)

from .support import RelayTestCase
from .test_bridge_adapter import AUTHORIZED, authorized_resume_response


class PhaseExpiry(RelayTestCase):
    """One expired phase, carried all the way to the attempt record the schema accepts."""

    TIMEOUT = 0.25

    def setUp(self):
        super().setUp()
        try:
            import codex_thread_bridge.rpc  # noqa: F401
        except ImportError:
            self.skipTest("the pinned bridge is not importable in this interpreter")

    def expiring(self, method, phase):
        """An App Server that answers normally until `method`, which expires in `phase`.

        The exception is built through the bridge's own constructor, so the method prefix the
        classifier reads is produced by the code under test rather than spelled out here.
        """
        from codex_thread_bridge.rpc import PhaseTimeout

        detail = {
            "establish": f"connection establishment exceeded 0.25s; no {method} frame was sent",
            "transmit": "the request frame did not drain within 0.25s and the connection was"
                        " retired; response unavailable; do not resend",
            "ack": "response unavailable; do not resend",
        }[phase]

        class Expiring:
            socket_path = None
            info = {}

            async def call(self, called, params):
                if called == method:
                    raise PhaseTimeout(called, phase, 0.25, detail)
                if called == "thread/read":
                    return {"thread": {"status": {"type": "idle"}}}
                if called == "thread/resume":
                    return authorized_resume_response(thread={"id": params.get("threadId")})
                if called == "turn/start":
                    return {"turn": {"id": "turn-1"}}
                raise AssertionError(f"unexpected {called}")

            async def close(self):
                return None

        return Expiring()

    def receipt_for(self, method, phase, request_id):
        from pathlib import Path

        from codex_thread_bridge.ledger import Ledger

        socket = Path(self.tmp) / "socket"
        built = BridgeHostAdapter(
            str(socket),
            timeout=self.TIMEOUT,
            caller_slack=0.75,
            app_server_factory=lambda canonical: self.expiring(method, phase),
            ledger_factory=lambda: (socket, Ledger(Path(self.tmp) / f"{request_id}.sqlite3")),
        )
        self.addCleanup(built.close)
        return built.send_message(request_id, "thread-1", "hello", AUTHORIZED)

    def record(self, facts, request_id):
        return attempt_record(
            facts, request_id=request_id, event_id="ev-1", attempt_no=1,
            recipient="01parent", status_before="unknown",
            observed_at="2026-09-22T00:00:00Z",
        )

    def test_an_establishment_expiry_before_the_read_is_a_completed_non_delivery(self):
        """No connection was obtained, so no thread/read frame was written.

        This is the case the issue is about. Left to fall through as an unclassified error it
        became held_uncertain, which parks a delivery that demonstrably never left and blocks a
        clean retry - the operational cost of one recipient's connection delay landing on
        another's send.
        """
        receipt = self.receipt_for("thread/read", "establish", "req-establish-read")

        self.assertEqual(receipt["status"], "failed", receipt)
        self.assertEqual(receipt["rpcError"]["code"], "connection_unavailable")

        facts = classify_operation_receipt(receipt)
        self.assertEqual(facts.delivery_state, WITHHELD_PRE_SEND)
        self.assertEqual(facts.send_attempted, "no")
        self.assertTrue(facts.retry_safe)
        self.assertEqual(facts.failed_operation, "thread/read")
        assert_attempt_invariants(self.record(facts, "req-establish-read"))

    def test_an_establishment_expiry_before_the_resume_is_also_a_completed_non_delivery(self):
        """The resume never answered, so there is no resumed block and nothing was transmitted."""
        receipt = self.receipt_for("thread/resume", "establish", "req-establish-resume")

        facts = classify_operation_receipt(receipt)
        self.assertIsNone(receipt.get("resumed"))
        self.assertEqual(facts.delivery_state, WITHHELD_PRE_SEND)
        self.assertEqual(facts.send_attempted, "no")
        self.assertTrue(facts.retry_safe)
        self.assertEqual(facts.failed_operation, "thread/resume")
        assert_attempt_invariants(self.record(facts, "req-establish-resume"))

    def test_an_establishment_expiry_before_the_turn_stays_pessimistic(self):
        """Known and accepted: the receipt knows more than the frozen attempt schema can say.

        No turn/start frame was written either, but assert_attempt_invariants forbids
        sendAttempted "no" on a turn/start operation, so this keeps the uncertain reading. Wrong
        in the safe direction - it costs a retry, never a duplicate turn - and narrowing it means
        changing the frozen schema, which is separate work.
        """
        receipt = self.receipt_for("turn/start", "establish", "req-establish-turn")

        self.assertEqual(receipt["rpcError"]["code"], "connection_unavailable")
        facts = classify_operation_receipt(receipt)
        self.assertEqual(facts.delivery_state, HELD_UNCERTAIN)
        self.assertEqual(facts.send_attempted, "yes")
        self.assertFalse(facts.retry_safe)
        assert_attempt_invariants(self.record(facts, "req-establish-turn"))

    def test_a_transmit_expiry_stays_uncertain(self):
        """The frame was handed to the socket before the bound fired. Never a non-delivery."""
        receipt = self.receipt_for("turn/start", "transmit", "req-transmit")

        self.assertEqual(receipt["status"], "outcome_unknown", receipt)
        facts = classify_operation_receipt(receipt)
        self.assertEqual(facts.delivery_state, HELD_UNCERTAIN)
        self.assertFalse(facts.retry_safe)
        assert_attempt_invariants(self.record(facts, "req-transmit"))

    def test_an_acknowledgement_expiry_stays_uncertain(self):
        """The request was written and the answer was lost. Exactly as before this change."""
        receipt = self.receipt_for("turn/start", "ack", "req-ack")

        self.assertEqual(receipt["status"], "outcome_unknown", receipt)
        facts = classify_operation_receipt(receipt)
        self.assertEqual(facts.delivery_state, HELD_UNCERTAIN)
        self.assertFalse(facts.retry_safe)
        assert_attempt_invariants(self.record(facts, "req-ack"))


class PhaseBudgetAgreement(unittest.TestCase):
    """The relay's stage count and the bridge's enforced bounds are one definition, not two."""

    def setUp(self):
        try:
            import codex_thread_bridge.rpc  # noqa: F401
        except ImportError:
            self.skipTest("the pinned bridge is not importable in this interpreter")

    def test_the_phase_names_the_relay_sends_are_the_bounds_the_bridge_holds(self):
        """Adding a phase here without a bound over there is a TypeError, not a silent wait."""
        from codex_thread_bridge.rpc import PhaseBounds

        bounds = PhaseBounds(**dict.fromkeys(_Transport.TRANSFER_PHASES, 20.0))

        self.assertEqual(bounds.per_request, 20.0 * _Transport.RPC_STAGES_PER_REQUEST)
        self.assertEqual(
            _Transport.RPC_STAGES_PER_SEND,
            _Transport.RPC_STAGES_PER_REQUEST * _Transport.RPC_REQUESTS_PER_SEND,
        )

    def test_every_declared_phase_is_one_the_bridge_actually_bounds(self):
        from dataclasses import fields

        from codex_thread_bridge.rpc import PhaseBounds

        self.assertEqual(
            set(_Transport.TRANSFER_PHASES), {field.name for field in fields(PhaseBounds)}
        )
