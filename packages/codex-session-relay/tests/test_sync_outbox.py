"""The coordination-document outbox: durable, separately retried, and honest about idempotency."""

import hashlib
import json

from codex_session_relay import identity
from codex_session_relay.criteria import CriteriaService, set_digest
from codex_session_relay.errors import AckRefused, RefusalReason
from codex_session_relay.sync import (
    CONFIRMED,
    FAILED,
    PENDING,
    SyncOutbox,
    WRITTEN,
    canonical_summary,
    end_marker,
    identity_digest,
    parse_blocks,
    parse_document,
    render_block,
    summary_fence,
    sync_id,
)

from .support import CHILD, PARENT, DeliveryTestCase
from .linear_readback_fixtures import (
    CONFIRMED_BLOCK,
    MANGLED_BLOCK,
    RAW_SUMMARY,
    SYNC_ID,
)

FENCE = chr(96)

DOC = "https://linear.app/example/document/coordination-000000000000"
OTHER_DOC = "https://linear.app/example/document/somewhere-else-0000"

SOURCE = "https://linear.app/example/document/criteria-0000"
SET = [
    {"id": "c1", "title": "the endpoint returns the agreed shape"},
    {"id": "c2", "title": "a malformed request is refused"},
]
# Same ids, one different wording, exactly as a real edit would arrive. Identical ids are the
# point: a ruling recorded against the old text must not certify the new one unread.
EDITED = [
    {"id": "c1", "title": "the endpoint returns a COMPLETELY different shape"},
    {"id": "c2", "title": "a malformed request is refused"},
]
PASSING = [{"id": "c1", "verdict": "verified"}, {"id": "c2", "verdict": "verified"}]


class OutboxTestCase(DeliveryTestCase):
    def setUp(self):
        super().setUp()
        self.sync = SyncOutbox(self.store, self.clock)
        self.ack.sync = self.sync

    def acknowledged(self, text="the deliverable"):
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD], text=text)
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        return event_id

    def targeted(self, target_ref=DOC, text="the deliverable"):
        event_id = self.acknowledged(text=text)
        self.sync.set_target(self._rid, "coordination_document", target_ref)
        return event_id

    def verdict_job(self, verdict="verified", **kw):
        event_id = self.targeted(**kw)
        self.ack.record_verdict(
            event_id, verdict=verdict, verdict_turn_id="v1",
            findings=[{"id": "c1", "verdict": "needs_changes", "note": "fix it"}]
            if verdict == "needs_changes" else None,
        )
        jobs = self.sync.snapshot(relationship_id=self._rid)["jobs"]
        return event_id, jobs[0]["syncId"]

    def document(self, *identifiers, mutate=None):
        """A simulated coordination document holding the given jobs' blocks."""
        parts = ["# Coordination", ""]
        for identifier in identifiers:
            block = render_block(self.sync.get(identifier))
            parts.append(mutate(block) if mutate else block)
            parts.append("")
        return "\n".join(parts)


class OutboxIdentity(OutboxTestCase):
    def test_a_verdict_enqueues_exactly_one_job_carrying_its_identity(self):
        event_id, identifier = self.verdict_job()
        row = self.sync.get(identifier)
        self.assertEqual(row["event_id"], event_id)
        self.assertEqual(row["verdict"], "verified")
        self.assertEqual(row["target_ref"], DOC)
        self.assertEqual(row["revision_hash"], self.intake.row(event_id)["revision_hash"])

    def test_the_same_verdict_for_two_documents_is_two_jobs(self):
        """Identity includes the target document, not just the target kind."""
        first = sync_id("coordination_document", DOC, "verdict", "rel-1", "e1", 1, "r1", "verified")
        second = sync_id(
            "coordination_document", OTHER_DOC, "verdict", "rel-1", "e1", 1, "r1", "verified"
        )
        self.assertNotEqual(first, second)

    def test_a_replayed_verdict_does_not_create_a_second_job(self):
        event_id, identifier = self.verdict_job()
        self.ack.record_verdict(event_id, verdict="verified", verdict_turn_id="v2")
        self.assertEqual(len(self.sync.snapshot(relationship_id=self._rid)["jobs"]), 1)

    def test_no_target_means_no_job_and_no_failure(self):
        event_id = self.acknowledged()
        record = self.ack.record_verdict(event_id, verdict="verified", verdict_turn_id="v1")
        self.assertEqual(record["verdict"], "verified")
        self.assertEqual(self.sync.snapshot(relationship_id=self._rid)["jobs"], [])


class FailureIsolation(OutboxTestCase):
    def test_an_external_failure_leaves_the_verdict_committed(self):
        event_id, identifier = self.verdict_job()
        claim = self.sync.claim(identifier, owner="main")
        self.sync.fail(identifier, claim_token=claim["claimToken"], error="connector timed out")

        verdict = self.store.one("SELECT * FROM verdicts WHERE event_id = ?", (event_id,))
        self.assertIsNotNone(verdict)
        self.assertEqual(verdict["verdict"], "verified")

    def test_an_external_failure_leaves_exactly_one_retryable_job(self):
        _event_id, identifier = self.verdict_job()
        claim = self.sync.claim(identifier, owner="main")
        self.sync.fail(identifier, claim_token=claim["claimToken"], error="connector timed out")
        jobs = self.sync.snapshot(relationship_id=self._rid)["jobs"]
        self.assertEqual(len(jobs), 1)
        self.assertEqual(jobs[0]["state"], PENDING)
        self.assertEqual(jobs[0]["attempts"], 1)

    def test_an_external_failure_does_not_requeue_the_revision_request(self):
        """No parent review and no child correction is repeated by a synchronisation retry."""
        _event_id, identifier = self.verdict_job(verdict="needs_changes")
        before = self.store.all("SELECT * FROM deliveries")
        claim = self.sync.claim(identifier, owner="main")
        self.sync.fail(identifier, claim_token=claim["claimToken"], error="connector timed out")
        self.sync.retry(identifier)
        after = self.store.all("SELECT * FROM deliveries")
        self.assertEqual(len(before), len(after))

    def test_a_retry_updates_the_same_job(self):
        _event_id, identifier = self.verdict_job()
        claim = self.sync.claim(identifier, owner="main")
        self.sync.fail(identifier, claim_token=claim["claimToken"], error="first failure")
        self.sync.retry(identifier)
        second = self.sync.claim(identifier, owner="main")
        self.sync.fail(identifier, claim_token=second["claimToken"], error="second failure")
        jobs = self.sync.snapshot(relationship_id=self._rid)["jobs"]
        self.assertEqual(len(jobs), 1)
        self.assertEqual(jobs[0]["attempts"], 2)
        self.assertEqual(jobs[0]["lastError"], "second failure")

    def test_repeated_failure_stops_generating_work_without_losing_the_summary(self):
        _event_id, identifier = self.verdict_job()
        for _ in range(8):
            self.sync.retry(identifier)
            claim = self.sync.claim(identifier, owner="main")
            self.sync.fail(identifier, claim_token=claim["claimToken"], error="still down")
        row = self.sync.get(identifier)
        self.assertEqual(row["state"], FAILED)
        self.assertTrue(row["summary"])

    def test_a_local_enqueue_failure_rolls_the_verdict_back(self):
        """The enqueue is a local write inside the verdict's transaction, so it cannot be silent."""
        event_id = self.targeted()

        def explode(*args, **kwargs):
            raise RuntimeError("local sqlite failure")

        self.ack.sync.enqueue_verdict_in = explode
        with self.assertRaises(RuntimeError):
            self.ack.record_verdict(event_id, verdict="verified", verdict_turn_id="v1")
        self.assertIsNone(
            self.store.one("SELECT * FROM verdicts WHERE event_id = ?", (event_id,))
        )


class Reconciliation(OutboxTestCase):
    def test_a_lost_response_reconciles_instead_of_writing_again(self):
        """The write landed; only the response was lost. The marker says so."""
        _event_id, identifier = self.verdict_job()
        observed = self.document(identifier)
        result = self.sync.reconcile(identifier, observed)
        self.assertEqual(result["outcome"], "already_written")

    def test_an_absent_marker_means_nothing_landed(self):
        _event_id, identifier = self.verdict_job()
        result = self.sync.reconcile(identifier, "# Coordination\n\nnothing here yet\n")
        self.assertEqual(result["outcome"], "absent")

    def test_a_block_describing_something_else_is_stale(self):
        _event_id, identifier = self.verdict_job()
        observed = self.document(
            identifier, mutate=lambda b: b.replace("disposition: verified", "disposition: unverified")
        )
        result = self.sync.reconcile(identifier, observed)
        self.assertEqual(result["outcome"], "stale")
        self.assertTrue(result["previousBlock"])

    def test_an_unterminated_block_is_malformed_not_absent(self):
        """A partial marker is not proof that nothing landed, so it is never appended over."""
        _event_id, identifier = self.verdict_job()
        block = render_block(self.sync.get(identifier))
        truncated = block.rsplit("<!-- /relay-sync:", 1)[0]
        self.assertEqual(self.sync.reconcile(identifier, truncated)["outcome"], "malformed")

    def test_two_identical_blocks_are_a_duplicate_not_a_success(self):
        _event_id, identifier = self.verdict_job()
        doubled = self.document(identifier) + self.document(identifier)
        self.assertEqual(self.sync.reconcile(identifier, doubled)["outcome"], "duplicate")

    def test_a_block_without_the_summary_is_stale(self):
        """Identity headers alone do not establish that the record arrived."""
        _event_id, identifier = self.verdict_job()
        row = self.sync.get(identifier)
        stripped = self.document(identifier).replace(row["summary"], "")
        self.assertEqual(self.sync.reconcile(identifier, stripped)["outcome"], "stale")

    def test_the_packet_requires_conditional_container_replacement(self):
        _event_id, identifier = self.verdict_job()
        packet = self.sync.operation(identifier)
        self.assertIn("relay-sync-container:", packet["containerStartMarker"])
        self.assertTrue(any("Never a bare append" in step for step in packet["protocol"]))

    def test_the_operation_packet_carries_the_protocol_not_a_verb(self):
        _event_id, identifier = self.verdict_job()
        packet = self.sync.operation(identifier)
        self.assertEqual(packet["targetRef"], DOC)
        self.assertIn("relay-sync:", packet["startMarker"])
        self.assertTrue(any("already landed" in step for step in packet["protocol"]))
        self.assertIn("no idempotency key", packet["note"])


class Readback(OutboxTestCase):
    def confirm(self, identifier, readback, *, target_ref=DOC):
        claim = self.sync.claim(identifier, owner="main")
        return self.sync.complete(
            identifier, claim_token=claim["claimToken"], target_ref=target_ref,
            readback=readback, external_ref="doc-rev-7",
        )

    def test_a_matching_readback_confirms(self):
        _event_id, identifier = self.verdict_job()
        row = self.confirm(identifier, self.document(identifier))
        self.assertEqual(row["state"], CONFIRMED)
        self.assertTrue(row["confirmed_at"])

    def test_a_confirmed_job_is_not_returned_by_next(self):
        _event_id, identifier = self.verdict_job()
        self.confirm(identifier, self.document(identifier))
        self.assertEqual(self.sync.next(), [])

    def test_unverified_does_not_satisfy_a_verified_job(self):
        """Substring matching would accept this, because verified occurs inside unverified."""
        _event_id, identifier = self.verdict_job()
        observed = self.document(
            identifier,
            mutate=lambda b: b.replace("disposition: verified", "disposition: unverified"),
        )
        with self.assertRaises(AckRefused) as caught:
            self.confirm(identifier, observed)
        self.assertEqual(caught.exception.reason, RefusalReason.READBACK_MISMATCH)

    def test_identity_split_across_two_blocks_is_not_a_match(self):
        _event_id, identifier = self.verdict_job()
        block = render_block(self.sync.get(identifier))
        head, _, tail = block.partition("revisionHash:")
        split = head + "revisionHash: \n" + "<!-- /relay-sync:" + identifier + " -->\n" \
            + "some other block mentioning revisionHash:" + tail
        with self.assertRaises(AckRefused) as caught:
            self.confirm(identifier, split)
        self.assertEqual(caught.exception.reason, RefusalReason.READBACK_MISMATCH)

    def test_a_refused_readback_leaves_the_job_written_not_pending(self):
        _event_id, identifier = self.verdict_job()
        with self.assertRaises(AckRefused):
            self.confirm(identifier, "# Coordination\n\nnothing\n")
        self.assertEqual(self.sync.get(identifier)["state"], WRITTEN)

    def test_a_duplicated_block_does_not_confirm(self):
        _event_id, identifier = self.verdict_job()
        doubled = self.document(identifier) + self.document(identifier)
        with self.assertRaises(AckRefused) as caught:
            self.confirm(identifier, doubled)
        self.assertEqual(caught.exception.reason, RefusalReason.READBACK_MISMATCH)

    def test_a_block_missing_the_summary_does_not_confirm(self):
        _event_id, identifier = self.verdict_job()
        row = self.sync.get(identifier)
        stripped = self.document(identifier).replace(row["summary"], "")
        with self.assertRaises(AckRefused):
            self.confirm(identifier, stripped)

    def test_a_bad_readback_cannot_downgrade_a_confirmed_job(self):
        """Confirmation is monotonic, even for the token that confirmed it."""
        _event_id, identifier = self.verdict_job()
        claim = self.sync.claim(identifier, owner="main")
        self.sync.complete(
            identifier, claim_token=claim["claimToken"], target_ref=DOC,
            readback=self.document(identifier),
        )
        again = self.sync.complete(
            identifier, claim_token=claim["claimToken"], target_ref=DOC,
            readback="# Coordination, with the block gone\n",
        )
        self.assertEqual(again["state"], CONFIRMED)
        self.assertEqual(self.sync.get(identifier)["state"], CONFIRMED)

    def test_completing_against_the_wrong_document_is_refused(self):
        _event_id, identifier = self.verdict_job()
        with self.assertRaises(AckRefused) as caught:
            self.confirm(identifier, self.document(identifier), target_ref=OTHER_DOC)
        self.assertEqual(caught.exception.reason, RefusalReason.SYNC_TARGET_MISMATCH)

    def test_a_progress_job_is_validated_without_a_verdict(self):
        self.targeted()
        with self.store.transaction() as db:
            identifier = self.sync.enqueue_in(
                db, relationship_id=self._rid, issue_key="REL-1", subject_kind="progress",
                summary="received, waiting on the parent", event_id=None, generation=1,
                revision=None,
            )
        row = self.confirm(identifier, self.document(identifier))
        self.assertEqual(row["state"], CONFIRMED)
        self.assertIsNone(row["verdict"])


class ClaimFencing(OutboxTestCase):
    def test_a_second_claim_cannot_be_taken_while_a_lease_is_held(self):
        _event_id, identifier = self.verdict_job()
        self.sync.claim(identifier, owner="main", now=100.0)
        with self.assertRaises(AckRefused) as caught:
            self.sync.claim(identifier, owner="other", now=120.0)
        self.assertEqual(caught.exception.reason, RefusalReason.SYNC_NOT_CLAIMABLE)

    def test_an_expired_claim_cannot_complete_what_a_newer_one_confirmed(self):
        _event_id, identifier = self.verdict_job()
        stale = self.sync.claim(identifier, owner="main", now=100.0)
        fresh = self.sync.claim(identifier, owner="other", now=100_000.0)
        self.sync.complete(
            identifier, claim_token=fresh["claimToken"], target_ref=DOC,
            readback=self.document(identifier),
        )
        with self.assertRaises(AckRefused) as caught:
            self.sync.complete(
                identifier, claim_token=stale["claimToken"], target_ref=DOC,
                readback=self.document(identifier),
            )
        self.assertEqual(caught.exception.reason, RefusalReason.SYNC_NOT_CLAIMABLE)

    def test_an_expired_claim_cannot_fail_a_confirmed_job(self):
        _event_id, identifier = self.verdict_job()
        stale = self.sync.claim(identifier, owner="main", now=100.0)
        fresh = self.sync.claim(identifier, owner="other", now=100_000.0)
        self.sync.complete(
            identifier, claim_token=fresh["claimToken"], target_ref=DOC,
            readback=self.document(identifier),
        )
        with self.assertRaises(AckRefused):
            self.sync.fail(identifier, claim_token=stale["claimToken"], error="too late")
        self.assertEqual(self.sync.get(identifier)["state"], CONFIRMED)

    def test_a_confirmed_job_cannot_be_reclaimed(self):
        _event_id, identifier = self.verdict_job()
        claim = self.sync.claim(identifier, owner="main")
        self.sync.complete(
            identifier, claim_token=claim["claimToken"], target_ref=DOC,
            readback=self.document(identifier),
        )
        with self.assertRaises(AckRefused):
            self.sync.claim(identifier, owner="other")

    def test_an_expired_claim_is_discoverable_again(self):
        """A writer that died holding a lease must not make its job invisible."""
        _event_id, identifier = self.verdict_job()
        self.sync.claim(identifier, owner="main", now=100.0)
        self.assertEqual(self.sync.next(now=120.0), [])
        found = self.sync.next(now=100_000.0)
        self.assertEqual([j["sync_id"] for j in found], [identifier])

    def test_claim_refuses_a_failed_job_without_an_explicit_retry(self):
        _event_id, identifier = self.verdict_job()
        for _ in range(8):
            self.sync.retry(identifier)
            claim = self.sync.claim(identifier, owner="main")
            self.sync.fail(identifier, claim_token=claim["claimToken"], error="still down")
        self.assertEqual(self.sync.get(identifier)["state"], FAILED)
        with self.assertRaises(AckRefused) as caught:
            self.sync.claim(identifier, owner="main")
        self.assertEqual(caught.exception.reason, RefusalReason.SYNC_NOT_CLAIMABLE)
        self.sync.retry(identifier)
        self.assertTrue(self.sync.claim(identifier, owner="main")["claimToken"])

    def test_claim_respects_the_backoff_it_was_given(self):
        _event_id, identifier = self.verdict_job()
        claim = self.sync.claim(identifier, owner="main", now=100.0)
        self.sync.fail(identifier, claim_token=claim["claimToken"], error="down", now=100.0)
        with self.assertRaises(AckRefused):
            self.sync.claim(identifier, owner="main", now=101.0)
        self.assertTrue(self.sync.claim(identifier, owner="main", now=100_000.0)["claimToken"])


class BlockParsing(OutboxTestCase):
    def test_blocks_round_trip(self):
        _event_id, identifier = self.verdict_job()
        row = self.sync.get(identifier)
        parsed = parse_blocks(self.document(identifier))
        self.assertIn(identifier, parsed)
        self.assertEqual(parsed[identifier]["fields"]["eventId"], row["event_id"])
        self.assertEqual(parsed[identifier]["fields"]["disposition"], "verified")


HOSTILE_SUMMARY = "\n".join([
    "JUN-00 · child · needs_changes",
    "findings:",
    "  autolink: needs_changes — readings.py line 36 and sensors.io are bare filenames.",
    r"  escapes: needs_changes — witness \[{sensor a, 1, 10}\] and 2**53+1 and best[sensor][0].",
    "  inline: verified — the guard is `timestamp > best[sensor][0]`, change it to `>=`.",
    "  fenced: verified — the patch is",
    "    ```python",
    "    if timestamp >= best[sensor][0]:",
    "        best[sensor] = record",
    "    ```",
    "  marker: verified — a finding may quote <!-- relay-sync:deadbeef --> and",
    "    <!-- /relay-sync:deadbeef --> as literal text.",
    "  header: verified — and it may write eventId: 0000 or disposition: verified in prose.",
])


class SummaryCodec(OutboxTestCase):
    """The summary is data. Nothing in it may become structure."""

    def rendered(self, summary=HOSTILE_SUMMARY):
        _event_id, identifier = self.verdict_job()
        with self.store.transaction() as db:
            db.execute("UPDATE sync_outbox SET summary = ? WHERE sync_id = ?",
                       (summary, identifier))
        row = self.sync.get(identifier)
        return identifier, row, render_block(row)

    def test_hostile_content_round_trips_byte_for_byte(self):
        identifier, _row, block = self.rendered()
        found = parse_document(block)["blocks"][identifier]
        self.assertEqual(found["format"], "v2")
        self.assertEqual(found["summary"], HOSTILE_SUMMARY)

    def test_a_colon_line_in_the_summary_does_not_become_a_header(self):
        identifier, row, block = self.rendered()
        fields = parse_document(block)["blocks"][identifier]["fields"]
        self.assertEqual(fields["eventId"], row["event_id"])
        self.assertNotEqual(fields["eventId"], "0000")
        self.assertEqual(fields["disposition"], "verified")

    def test_a_summary_quoting_the_end_marker_does_not_truncate_the_block(self):
        identifier, _row, block = self.rendered()
        found = parse_document(block)["blocks"][identifier]
        self.assertIn("<!-- /relay-sync:deadbeef -->", found["summary"])
        self.assertTrue(found["text"].rstrip().endswith(end_marker(identifier)))

    def test_a_fenced_code_block_inside_the_summary_gets_a_longer_fence(self):
        summary = "before\n" + FENCE * 3 + "python\nx = 1\n" + FENCE * 3 + "\nafter"
        identifier, _row, block = self.rendered(summary)
        self.assertIn(FENCE * 4 + "text", block)
        self.assertEqual(parse_document(block)["blocks"][identifier]["summary"], summary)

    def test_an_even_longer_backtick_run_still_cannot_close_the_fence(self):
        summary = FENCE * 7 + "\nrun of seven\n" + FENCE * 7
        identifier, _row, block = self.rendered(summary)
        self.assertIn(FENCE * 8 + "text", block)
        self.assertEqual(parse_document(block)["blocks"][identifier]["summary"], summary)

    def test_an_empty_summary_round_trips(self):
        identifier, _row, block = self.rendered("")
        self.assertEqual(parse_document(block)["blocks"][identifier]["summary"], "")

    def test_carriage_returns_are_canonicalised_and_still_match(self):
        identifier, row, block = self.rendered("one\r\ntwo\r\n")
        found = parse_document(block)["blocks"][identifier]
        self.assertEqual(found["summary"], "one\ntwo")
        self.assertEqual(self.sync._payload_mismatch(row, found), [])


class StrictV2Validation(OutboxTestCase):
    """v2 is held to exactly what the renderer emits, headers included."""

    def prepared(self):
        _event_id, identifier = self.verdict_job()
        row = self.sync.get(identifier)
        return identifier, row, render_block(row)

    def test_an_untouched_block_validates(self):
        identifier, row, block = self.prepared()
        found = parse_document(block)["blocks"][identifier]
        self.assertEqual(self.sync._payload_mismatch(row, found), [])

    def test_a_tampered_summary_is_refused(self):
        identifier, row, block = self.prepared()
        tampered = block.replace("needs_changes", "nope") if "needs_changes" in block else block
        tampered = block.replace(row["summary"].splitlines()[0], "something else entirely")
        found = parse_document(tampered)["blocks"][identifier]
        self.assertTrue(self.sync._payload_mismatch(row, found))

    def test_a_tampered_digest_is_refused(self):
        identifier, row, block = self.prepared()
        found = parse_document(
            block.replace("summarySha256: ", "summarySha256: 0")
        )["blocks"][identifier]
        problems = self.sync._payload_mismatch(row, found)
        self.assertTrue(any("summarySha256" in p for p in problems), problems)

    def test_a_missing_rendered_header_is_refused(self):
        identifier, row, block = self.prepared()
        without = "\n".join(
            line for line in block.split("\n") if not line.startswith("relationshipId:")
        )
        problems = self.sync._payload_mismatch(row, parse_document(without)["blocks"][identifier])
        self.assertIn("relationshipId is missing", problems)

    def test_a_wrong_issue_key_is_refused(self):
        identifier, row, block = self.prepared()
        found = parse_document(
            block.replace(f"issueKey: {row['issue_key']}", "issueKey: SOMEONE-ELSE")
        )["blocks"][identifier]
        problems = self.sync._payload_mismatch(row, found)
        self.assertTrue(any("issueKey" in p for p in problems), problems)

    def test_an_extra_header_is_refused(self):
        identifier, row, block = self.prepared()
        injected = block.replace("syncId: ", "smuggled: yes\nsyncId: ", 1)
        problems = self.sync._payload_mismatch(row, parse_document(injected)["blocks"][identifier])
        self.assertTrue(any("unexpected headers" in p for p in problems), problems)

    def test_a_duplicate_header_is_refused(self):
        identifier, row, block = self.prepared()
        doubled = block.replace("syncId: ", "syncId: decoy\nsyncId: ", 1)
        problems = self.sync._payload_mismatch(row, parse_document(doubled)["blocks"][identifier])
        self.assertTrue(any("duplicate header" in p for p in problems), problems)


class RealLinearNormalisation(OutboxTestCase):
    """The two ACTUAL readbacks from the live round trip, not hand-written escaping."""

    def job_matching_the_real_block(self):
        """A row carrying the real summary, rewritten to the real block's identity."""
        _event_id, identifier = self.verdict_job()
        fields = dict(
            line.split(": ", 1)
            for line in MANGLED_BLOCK.split("\n")[1:]
            if ": " in line and not line.startswith(" ")
        )
        with self.store.transaction() as db:
            db.execute(
                "UPDATE sync_outbox SET sync_id = ?, issue_key = ?, relationship_id = ?,"
                " event_id = ?, execution_generation = ?, revision_hash = ?, verdict = ?,"
                " identity_digest = ?, summary = ? WHERE sync_id = ?",
                (
                    SYNC_ID, fields["issueKey"], fields["relationshipId"], fields["eventId"],
                    int(fields["executionGeneration"]), fields["revisionHash"],
                    fields["disposition"], fields["identityDigest"], RAW_SUMMARY, identifier,
                ),
            )
        return self.sync.get(SYNC_ID)

    def test_the_real_mangled_block_is_stale_and_repairable(self):
        self.job_matching_the_real_block()
        outcome = self.sync.reconcile(SYNC_ID, "intro\n\n" + MANGLED_BLOCK + "\n\noutro\n")
        self.assertEqual(outcome["outcome"], "stale")
        # Repair is a conditional replacement of exactly this text; nothing is regenerated.
        self.assertEqual(outcome["previousBlock"], MANGLED_BLOCK)

    def test_the_real_confirmed_fenced_block_still_reads_as_already_written(self):
        self.job_matching_the_real_block()
        outcome = self.sync.reconcile(SYNC_ID, "intro\n\n" + CONFIRMED_BLOCK + "\n\noutro\n")
        self.assertEqual(outcome["outcome"], "already_written")

    def test_the_real_confirmed_block_carries_the_summary_verbatim(self):
        self.job_matching_the_real_block()
        found = parse_document(CONFIRMED_BLOCK)["blocks"][SYNC_ID]
        self.assertEqual(found["format"], "fenced-legacy")
        self.assertEqual(found["summary"], RAW_SUMMARY)
        self.assertIn("readings.py line 36", found["summary"])
        self.assertIn("2**53+1", found["summary"])

    def test_re_rendering_that_job_produces_a_v2_block_that_validates(self):
        row = self.job_matching_the_real_block()
        block = render_block(row)
        found = parse_document(block)["blocks"][SYNC_ID]
        self.assertEqual(found["format"], "v2")
        self.assertEqual(self.sync._payload_mismatch(row, found), [])
        # The identity a repair must keep stable is unchanged by re-rendering.
        self.assertEqual(found["fields"]["eventId"], row["event_id"])
        self.assertEqual(found["fields"]["executionGeneration"], str(row["execution_generation"]))
        self.assertEqual(found["fields"]["disposition"], row["verdict"])


class RelabelledPreV2Block(OutboxTestCase):
    """issueKey and relationshipId were rendered from the first version and never validated.

    So a block wearing ANOTHER issue's label passed as this job's record and confirmed it. The
    specimen is the real confirmed readback, relabelled, because that is the shape the live
    document actually holds.
    """

    def setUp(self):
        super().setUp()
        self.row = RealLinearNormalisation.job_matching_the_real_block(self)

    def relabel(self, block):
        relabelled = (
            block
            .replace("issueKey: " + self.row["issue_key"], "issueKey: SOMEONE-ELSE-9999")
            .replace(
                "relationshipId: " + self.row["relationship_id"],
                "relationshipId: rel-not-this-one",
            )
        )
        self.assertNotEqual(relabelled, block)
        return relabelled

    @staticmethod
    def observed(block):
        return "intro\n\n" + block + "\n\noutro\n"

    def plain(self):
        """The pre-fence shape: the real headers, then the summary as plain body text."""
        headers, _, _ = MANGLED_BLOCK.partition("\n\n")
        return headers + "\n\n" + RAW_SUMMARY + "\n" + end_marker(SYNC_ID)

    def test_the_relabelled_fenced_block_names_both_wrong_headers(self):
        found = parse_document(self.relabel(CONFIRMED_BLOCK))["blocks"][SYNC_ID]
        self.assertEqual(found["format"], "fenced-legacy")
        problems = self.sync._payload_mismatch(self.row, found)
        self.assertTrue(any("issueKey" in problem for problem in problems), problems)
        self.assertTrue(any("relationshipId" in problem for problem in problems), problems)

    def test_the_relabelled_fenced_block_reconciles_as_stale_and_repairable(self):
        relabelled = self.relabel(CONFIRMED_BLOCK)
        outcome = self.sync.reconcile(SYNC_ID, self.observed(relabelled))
        self.assertEqual(outcome["outcome"], "stale")
        # Repair stays a conditional replacement of exactly this text.
        self.assertEqual(outcome["previousBlock"], relabelled)

    def test_the_relabelled_fenced_block_cannot_confirm_the_job(self):
        claim = self.sync.claim(SYNC_ID, owner="main")
        with self.assertRaises(AckRefused) as caught:
            self.sync.complete(
                SYNC_ID, claim_token=claim["claimToken"],
                target_ref=self.row["target_ref"],
                readback=self.observed(self.relabel(CONFIRMED_BLOCK)),
            )
        self.assertEqual(caught.exception.reason, RefusalReason.READBACK_MISMATCH)
        self.assertEqual(self.sync.get(SYNC_ID)["state"], WRITTEN)

    def test_the_unfenced_shape_is_checked_the_same_way(self):
        """Correct labels still reconcile; relabelled ones do not."""
        plain = self.plain()
        self.assertEqual(parse_document(plain)["blocks"][SYNC_ID]["format"], "legacy")
        self.assertEqual(
            self.sync.reconcile(SYNC_ID, self.observed(plain))["outcome"], "already_written"
        )
        self.assertEqual(
            self.sync.reconcile(SYNC_ID, self.observed(self.relabel(plain)))["outcome"], "stale"
        )


class StructuralIntegrity(OutboxTestCase):
    """A block is held to the structure it declares, not to whatever shape it ended up in.

    Each of these parsed clean before, reconciled as already_written AND confirmed the job, so a
    document that had lost or gained structure could close a real sync job.
    """

    def prepared(self):
        _event_id, identifier = self.verdict_job(verdict="needs_changes")
        row = self.sync.get(identifier)
        return identifier, row, render_block(row)

    @staticmethod
    def observed(block):
        return "# Coordination\n\n" + block + "\n\ntail\n"

    def refuses(self, identifier, block, *, naming):
        """Stale, repairable by conditional replacement, and unable to confirm."""
        outcome = self.sync.reconcile(identifier, self.observed(block))
        self.assertEqual(outcome["outcome"], "stale")
        self.assertEqual(outcome["previousBlock"], block)
        self.assertTrue(any(naming in problem for problem in outcome["mismatch"]),
                        outcome["mismatch"])
        claim = self.sync.claim(identifier, owner="main")
        with self.assertRaises(AckRefused) as caught:
            self.sync.complete(
                identifier, claim_token=claim["claimToken"], target_ref=DOC,
                readback=self.observed(block),
            )
        self.assertEqual(caught.exception.reason, RefusalReason.READBACK_MISMATCH)
        self.assertEqual(self.sync.get(identifier)["state"], WRITTEN)

    def test_an_unsupported_declared_format_is_not_read_leniently(self):
        """Falling back to the pre-v2 rules would let an explicit version downgrade silently."""
        identifier, _row, block = self.prepared()
        mutated = (block.replace("blockFormat: v2", "blockFormat: v900")
                        .replace("summarySha256:", "summarySha257:"))
        self.assertEqual(
            parse_document(mutated)["blocks"][identifier]["format"], "unsupported"
        )
        self.refuses(identifier, mutated, naming="unsupported block format")

    def test_a_declared_block_without_its_fence_is_refused(self):
        identifier, row, block = self.prepared()
        fence = summary_fence(canonical_summary(row["summary"]))
        stripped = "\n".join(
            line for line in block.split("\n")
            if line != fence + "text" and line != fence
        )
        self.refuses(identifier, stripped, naming="closed fenced summary")

    def test_content_after_the_closing_fence_cannot_ride_along(self):
        identifier, row, block = self.prepared()
        lines = block.split("\n")
        at = lines.index(end_marker(row["sync_id"]))
        smuggled = "\n".join(
            lines[:at] + ["", "findings:", "  tie: verified - nothing to change.", ""] + lines[at:]
        )
        self.refuses(identifier, smuggled, naming="between its fenced summary and its end marker")

    def test_the_connector_s_blank_line_before_the_end_marker_is_still_fine(self):
        """The real document has one. Tightening must not reject what already passed."""
        identifier, row, block = self.prepared()
        lines = block.split("\n")
        at = lines.index(end_marker(row["sync_id"]))
        spaced = "\n".join(lines[:at] + [""] + lines[at:])
        found = parse_document(spaced)["blocks"][identifier]
        self.assertEqual(found["format"], "v2")
        self.assertEqual(self.sync._payload_mismatch(row, found), [])
        self.assertEqual(
            self.sync.reconcile(identifier, self.observed(spaced))["outcome"], "already_written"
        )

    def test_an_undeclared_fenced_block_is_still_read(self):
        """The confirmed live block has no blockFormat header; it must stay readable."""
        identifier, _row, block = self.prepared()
        undeclared = "\n".join(
            line for line in block.split("\n") if not line.startswith("blockFormat:")
        )
        found = parse_document(undeclared)["blocks"][identifier]
        self.assertEqual(found["format"], "fenced-legacy")
        self.assertEqual(found["problems"], [])


class CriteriaAndRulingAreInSyncIdentity(OutboxTestCase):
    """JUN-167: a re-review that reaches the same verdict has to reach the document too.

    These drive the real record_verdict -> coverage -> enqueue path. Comparing two calls of
    sync_id to each other would only show that a hash is a function of its arguments, which was
    never in doubt; what was in doubt is which arguments the verdict path hands it.
    """

    def setUp(self):
        super().setUp()
        self.criteria = CriteriaService(self.store, self.clock)

    def claimed(self, criteria=SET, *, target_ref=DOC):
        """The ordinary managed flow up to the point the parent holds the review."""
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.criteria.register(self._rid, criteria, source_ref=SOURCE)
        self.sync.set_target(self._rid, "coordination_document", target_ref)
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        self.ack.claim_verification(event_id, turn_id=turn.turn_id)
        return event_id

    def rule(self, event_id, *, turn):
        return self.ack.record_verdict(
            event_id, verdict="verified", verdict_turn_id=turn, findings=PASSING,
        )

    def re_rule(self, event_id, *, turn):
        """Re-claiming rebinds to the set in force, and the ruling names the set it read."""
        self.ack.claim_verification(event_id, turn_id=f"claim-{turn}")
        return self.ack.record_verdict(
            event_id, verdict="verified", verdict_turn_id=turn, findings=PASSING,
            expect_criteria_digest=self.criteria.get(self._rid)["setDigest"],
        )

    def jobs(self):
        return self.sync.snapshot(relationship_id=self._rid)["jobs"]

    def digest(self):
        return self.criteria.get(self._rid)["setDigest"]

    # ------------------------------------------------------------- the defect

    def test_a_re_review_against_edited_criteria_enqueues_its_own_job(self):
        """The observation itself: same event, same generation, same revision, same verdict."""
        event_id = self.claimed()
        self.rule(event_id, turn="v1")
        self.assertEqual(len(self.jobs()), 1)

        self.criteria.register(self._rid, EDITED, source_ref=SOURCE)
        self.re_rule(event_id, turn="v2")

        after = self.jobs()
        self.assertEqual(len(after), 2, "a re-review on a changed set owes its own summary")
        self.assertNotEqual(after[0]["syncId"], after[1]["syncId"])
        self.assertEqual([one["disposition"] for one in after], ["verified", "verified"])

    def test_criteria_edited_away_and_back_still_enqueues_the_newest_ruling(self):
        """The set returns to wording already summarised; the RULING is still a new one.

        The digest alone cannot see this: rulings one and three carry the same one. Only the
        ordinal tells the occasions apart.
        """
        event_id = self.claimed()
        self.rule(event_id, turn="v1")
        self.criteria.register(self._rid, EDITED, source_ref=SOURCE)
        self.re_rule(event_id, turn="v2")
        self.criteria.register(self._rid, SET, source_ref=SOURCE)
        self.re_rule(event_id, turn="v3")

        identifiers = [one["syncId"] for one in self.jobs()]
        self.assertEqual(len(identifiers), 3)
        self.assertEqual(len(set(identifiers)), 3, "three rulings are three records")

    def test_a_replay_enqueues_nothing_further(self):
        """An unchanged set does not reach the enqueue at all, so there is no churn to suppress."""
        event_id = self.claimed()
        self.rule(event_id, turn="v1")
        record = self.ack.record_verdict(
            event_id, verdict="verified", verdict_turn_id="v2", findings=PASSING,
        )
        self.assertTrue(record.get("_replay"))
        self.assertEqual(len(self.jobs()), 1)

    # ------------------------------------------------------- what must not move

    def test_a_verdict_with_no_canonical_criteria_keeps_the_identity_it_always_had(self):
        """Through the real path, against the pre-change join computed here rather than by sync."""
        event_id, identifier = self.verdict_job()
        event = self.store.one("SELECT * FROM events WHERE event_id = ?", (event_id,))
        before = "|".join(str(value) for value in (
            "coordination_document", DOC, "verdict", self._rid, event_id,
            event["execution_generation"], event["revision_hash"], "verified",
        ))
        self.assertEqual(
            identifier, hashlib.sha256(before.encode("utf-8")).hexdigest()[:32]
        )

    def test_the_unlabelled_payload_is_the_one_this_code_has_always_produced(self):
        """A fixed vector, expected value computed from the old join and not from sync_id."""
        arguments = ("coordination_document", DOC, "verdict", "rel-1", "e1", 1, "r1", "verified")
        expected = hashlib.sha256("|".join(str(v) for v in arguments).encode("utf-8"))
        self.assertEqual(sync_id(*arguments), expected.hexdigest()[:32])
        self.assertEqual(identity_digest(*arguments), expected.hexdigest())

    def test_a_legacy_ruling_keeps_its_id_when_criteria_are_registered_afterwards(self):
        """Registering a set after a legacy verdict makes the same event a managed re-review."""
        event_id, identifier = self.verdict_job()
        self.criteria.register(self._rid, SET, source_ref=SOURCE)
        self.re_rule(event_id, turn="v2")

        identifiers = [one["syncId"] for one in self.jobs()]
        self.assertEqual(len(identifiers), 2)
        self.assertEqual(identifiers[0], identifier, "the first ruling's record does not move")

    # ------------------------------------------------------ what the record says

    def test_one_payload_gives_both_the_id_and_the_block_digest(self):
        """They are the same hash truncated and whole, so they cannot describe different inputs."""
        event_id = self.claimed()
        self.rule(event_id, turn="v1")
        row = self.sync.get(self.jobs()[0]["syncId"])
        self.assertTrue(row["identity_digest"].startswith(row["sync_id"]))

    def test_the_summary_names_the_criteria_set_the_ruling_rests_on(self):
        """A reader of the document sees the summary, not the identity."""
        event_id = self.claimed()
        self.rule(event_id, turn="v1")
        row = self.sync.get(self.jobs()[0]["syncId"])
        self.assertIn(f"criteria set {self.digest()[:12]}, ruling 1", row["summary"])

    def test_the_summary_is_what_tells_a_reader_which_ruling_stands(self):
        """Three blocks, and the first and third carry the same set and the same findings."""
        event_id = self.claimed()
        self.rule(event_id, turn="v1")
        self.criteria.register(self._rid, EDITED, source_ref=SOURCE)
        self.re_rule(event_id, turn="v2")
        self.criteria.register(self._rid, SET, source_ref=SOURCE)
        self.re_rule(event_id, turn="v3")

        summaries = [self.sync.get(one["syncId"])["summary"] for one in self.jobs()]
        self.assertEqual(
            [line for text in summaries for line in text.split("\n") if "ruling" in line],
            [f"criteria set {set_digest(SET)[:12]}, ruling 1",
             f"criteria set {set_digest(EDITED)[:12]}, ruling 2",
             f"criteria set {set_digest(SET)[:12]}, ruling 3"],
        )

    def test_an_older_job_retried_after_a_newer_one_still_names_its_own_ruling(self):
        """Where a block sits cannot rank it: a job in backoff is passed over and lands later."""
        event_id = self.claimed()
        self.rule(event_id, turn="v1")
        first = self.jobs()[0]["syncId"]
        claim = self.sync.claim(first, owner="main")
        self.sync.fail(first, claim_token=claim["claimToken"], error="connector timed out")

        self.criteria.register(self._rid, EDITED, source_ref=SOURCE)
        self.re_rule(event_id, turn="v2")
        second = [one["syncId"] for one in self.jobs() if one["syncId"] != first][0]

        self.assertEqual(
            [one["sync_id"] for one in self.sync.next()], [second],
            "only the newer ruling is selectable while the older one backs off",
        )
        self.clock.advance(1000)
        self.assertIn(first, [one["sync_id"] for one in self.sync.next()])
        self.assertIn("ruling 1", self.sync.get(first)["summary"])
        self.assertIn("ruling 2", self.sync.get(second)["summary"])

    def test_a_labelled_job_reconciles_and_completes_from_its_stored_identity(self):
        event_id = self.claimed()
        self.rule(event_id, turn="v1")
        identifier = self.jobs()[0]["syncId"]
        document = self.document(identifier)

        self.assertEqual(
            self.sync.reconcile(identifier, document)["outcome"], "already_written"
        )
        claim = self.sync.claim(identifier, owner="main")
        settled = self.sync.complete(
            identifier, claim_token=claim["claimToken"], target_ref=DOC, readback=document,
            external_ref="linear-doc-1",
        )
        self.assertEqual(settled["state"], CONFIRMED)

    def test_the_enqueue_journal_records_the_set_and_whether_the_row_was_created(self):
        """The digest has no column, and verdict_context is overwritten by the next re-review."""
        event_id = self.claimed()
        self.rule(event_id, turn="v1")
        identifier = self.jobs()[0]["syncId"]
        entry = json.loads(self.store.one(
            "SELECT detail FROM journal WHERE kind = ? AND subject = ?",
            ("sync_enqueued", identifier),
        )["detail"])
        self.assertEqual(entry["criteriaDigest"], self.digest())
        self.assertIsNone(entry["ruling"], "a first ruling carries no occurrence label")
        self.assertTrue(entry["inserted"])

    def test_the_journal_says_so_when_the_insert_was_ignored(self):
        """An entry that claims an enqueue either way claims a row that may not exist."""
        event_id = self.claimed()
        self.rule(event_id, turn="v1")
        identifier = self.jobs()[0]["syncId"]
        event = self.store.one("SELECT * FROM events WHERE event_id = ?", (event_id,))
        with self.store.transaction() as db:
            again = self.sync.enqueue_in(
                db, relationship_id=self._rid, issue_key="REL-1", subject_kind="verdict",
                summary="a second attempt at a record that already exists", event_id=event_id,
                generation=event["execution_generation"], revision=event["revision_hash"],
                verdict="verified", criteria_digest=self.digest(),
            )

        self.assertEqual(again, identifier, "the same ruling is the same job")
        entries = [json.loads(one["detail"]) for one in self.store.all(
            "SELECT detail FROM journal WHERE kind = ? AND subject = ? ORDER BY seq",
            ("sync_enqueued", identifier),
        )]
        self.assertEqual([one["inserted"] for one in entries], [True, False])
        self.assertEqual(self.sync.get(identifier)["summary"].count("criteria set"), 1)
