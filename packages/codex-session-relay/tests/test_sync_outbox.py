"""The coordination-document outbox: durable, separately retried, and honest about idempotency."""

from codex_session_relay import identity
from codex_session_relay.errors import AckRefused, RefusalReason
from codex_session_relay.sync import (
    CONFIRMED,
    FAILED,
    PENDING,
    SyncOutbox,
    WRITTEN,
    canonical_summary,
    end_marker,
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
