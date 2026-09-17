"""Relationships, generations, anchors and lifecycle."""

import unittest

from codex_session_relay.errors import RefusalReason
from codex_session_relay.identity import relationship_id
from codex_session_relay.models import Endpoint
from codex_session_relay.registry import contract_record

from .support import CHILD, HOST, ISSUE, PARENT, RelayTestCase


class Identity(RelayTestCase):
    def test_routing_key_is_the_pair_of_actual_task_ids(self):
        relationship = self.register()
        self.assertEqual(
            relationship["relationshipId"], relationship_id(PARENT, CHILD, ISSUE)
        )
        self.assertEqual(relationship["parent"]["taskId"], PARENT)
        self.assertEqual(relationship["child"]["taskId"], CHILD)

    def test_no_title_or_latest_session_field_exists_on_the_routing_path(self):
        record = contract_record(self.register())
        flat = str(record).lower()
        for forbidden in ("title", "latest", "name"):
            self.assertNotIn(forbidden, flat)

    def test_reregistration_is_idempotent_and_opens_no_new_generation(self):
        first = self.register()
        second = self.register()
        self.assertEqual(first["relationshipId"], second["relationshipId"])
        self.assertEqual(second["executionGeneration"], 1)
        self.assertEqual(len(second["generations"]), 1)

    def test_the_same_identity_with_a_different_scope_is_a_conflict(self):
        self.register()
        self.assertRefused(
            RefusalReason.RELATIONSHIP_CONFLICT, self.register, roots=["/somewhere/else"]
        )

    def test_cxc_bindings_are_preserved_and_never_written_back(self):
        relationship = self.register()
        self.assertEqual(relationship["_bindings"]["parentCxcSession"], "cxc-parent")
        self.assertEqual(relationship["_bindings"]["childCxcSession"], "cxc-child")
        # The contract record carries no harness fields at all.
        self.assertNotIn("_bindings", contract_record(relationship))

    def test_an_unregistered_relationship_is_refused(self):
        self.assertRefused(
            RefusalReason.UNREGISTERED_RELATIONSHIP, self.registry.get, "rel-0000000000000000"
        )


class Generations(RelayTestCase):
    def test_a_replayed_dispatch_request_id_opens_no_new_generation(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        first = self.registry.open_generation(
            rid, dispatch_request_id="dispatch-2", reason="needs_changes_revision"
        )
        again = self.registry.open_generation(
            rid, dispatch_request_id="dispatch-2", reason="needs_changes_revision"
        )
        self.assertEqual(first["executionGeneration"], again["executionGeneration"])
        self.assertEqual(self.registry.get(rid)["executionGeneration"], 2)

    def test_anchor_pending_never_auto_binds(self):
        relationship = self.register(dispatch_turn_id=None)
        generation = relationship["generations"][0]
        self.assertEqual(generation["anchorState"], "anchor_pending")
        self.assertIsNone(generation["dispatchTurnId"])
        # Nothing observes a later turn and adopts it; there is no such code path at all.
        self.assertEqual(
            self.registry.generation(relationship["relationshipId"], 1)["anchorState"],
            "anchor_pending",
        )

    def test_binding_requires_a_dispatch_receipt_and_an_exact_turn(self):
        relationship = self.register(dispatch_turn_id=None)
        rid = relationship["relationshipId"]
        self.assertRefused(
            RefusalReason.UNBOUND_GENERATION,
            self.registry.bind_anchor, rid, 1, dispatch_turn_id="t", source="latest_turn",
        )
        self.assertRefused(
            RefusalReason.UNBOUND_GENERATION,
            self.registry.bind_anchor, rid, 1, dispatch_turn_id="", source="dispatch_receipt",
        )
        bound = self.registry.bind_anchor(
            rid, 1, dispatch_turn_id="turn-exact", source="dispatch_receipt"
        )
        self.assertEqual(bound["anchorState"], "bound")
        self.assertEqual(bound["dispatchTurnId"], "turn-exact")

    def test_rebinding_to_a_different_turn_is_refused(self):
        relationship = self.register()
        self.assertRefused(
            RefusalReason.ANCHOR_ALREADY_BOUND,
            self.registry.bind_anchor,
            relationship["relationshipId"], 1,
            dispatch_turn_id="some-other-turn", source="dispatch_receipt",
        )

    def test_every_generation_is_retained(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.registry.open_generation(
            rid, dispatch_request_id="d2", reason="needs_changes_revision",
            dispatch_turn_id="t2",
        )
        self.registry.open_generation(
            rid, dispatch_request_id="d3", reason="needs_changes_revision",
            dispatch_turn_id="t3",
        )
        record = self.registry.get(rid)
        self.assertEqual(record["executionGeneration"], 3)
        self.assertEqual([g["executionGeneration"] for g in record["generations"]], [1, 2, 3])
        self.assertEqual(record["generations"][0]["dispatchTurnId"], "turn-dispatch-1")


class Lifecycle(RelayTestCase):
    def test_paused_cancelled_and_archived_are_never_active(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        for status in ("paused", "cancelled", "archived"):
            self.registry.set_status(rid, status, actor="test")
            self.assertRefused(
                RefusalReason.RELATIONSHIP_NOT_ACTIVE, self.registry.require_active, rid
            )

    def test_an_inactive_relationship_opens_no_generation_until_it_is_resumed(self):
        """The sequence skills/crw-run/references/relay.md prescribes, in order.

        A criteria edit closes the same-event route while the relationship is down, and it
        closes the other one too: opening a generation is not a way around a pause, so the
        instructions send the reader through resume first. If that stopped being true the
        documented procedure would fail at the first command.
        """
        relationship = self.register()
        rid = relationship["relationshipId"]
        for status in ("paused", "cancelled", "archived"):
            self.registry.set_status(rid, status, actor="test")
            self.assertRefused(
                RefusalReason.RELATIONSHIP_NOT_ACTIVE,
                lambda: self.registry.open_generation(
                    rid, dispatch_request_id="after-the-edit",
                    reason="needs_changes_revision",
                    dispatch_turn_id="turn-after-the-edit",
                ),
            )
            self.registry.resume(
                rid, expect_generation=1, expect_artifact_roots=[self.root],
                expect_allowed_recipients=[PARENT], actor="test",
            )
        opened = self.registry.open_generation(
            rid, dispatch_request_id="after-the-edit", reason="needs_changes_revision",
            dispatch_turn_id="turn-after-the-edit",
        )
        self.assertEqual(opened["executionGeneration"], 2)

    def test_reactivating_is_not_a_status_flip(self):
        """relationship-status takes deactivations only, which is why resume restates scope."""
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.registry.set_status(rid, "paused", actor="test")
        self.assertRefused(
            RefusalReason.RELATIONSHIP_NOT_ACTIVE,
            self.registry.set_status, rid, "active", actor="test",
        )

    def test_resume_requires_restating_the_generation_and_the_scope(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.registry.set_status(rid, "paused", actor="user")
        self.assertRefused(
            RefusalReason.RELATIONSHIP_NOT_ACTIVE,
            self.registry.resume, rid,
            expect_generation=99, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="test",
        )
        self.assertRefused(
            RefusalReason.RELATIONSHIP_NOT_ACTIVE,
            self.registry.resume, rid,
            expect_generation=1, expect_artifact_roots=["/elsewhere"],
            expect_allowed_recipients=[PARENT], actor="test",
        )
        resumed = self.registry.resume(
            rid, expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="test",
        )
        self.assertEqual(resumed["status"], "active")

    def test_parent_replacement_supersedes_and_preserves_the_original(self):
        original = self.register()
        replacement = self.registry.register(
            parent=Endpoint("01new-parent", HOST), child=Endpoint(CHILD, HOST),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=["01new-parent"],
            dispatch_request_id="dispatch-replacement",
            dispatch_turn_id="turn-replacement",
            supersedes=original["relationshipId"],
        )
        old = self.registry.get(original["relationshipId"])
        self.assertEqual(old["status"], "archived")
        self.assertEqual(old["supersededBy"], replacement["relationshipId"])
        # The original record is preserved, not rewritten.
        self.assertEqual(old["parent"]["taskId"], PARENT)
        self.assertEqual(old["generations"][0]["dispatchTurnId"], "turn-dispatch-1")
        self.assertEqual(replacement["supersedes"], original["relationshipId"])

    def test_a_superseded_relationship_cannot_be_resumed(self):
        original = self.register()
        self.registry.supersede(original["relationshipId"], new_relationship_id="rel-newnewnewnew00")
        self.assertRefused(
            RefusalReason.RELATIONSHIP_NOT_ACTIVE,
            self.registry.resume, original["relationshipId"],
            expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="test",
        )


if __name__ == "__main__":
    unittest.main()


class ResumeOwnership(RelayTestCase):
    """Cancelling RELEASES an issue, so coming back afterwards is not a status flip."""

    def replacement(self, child="different-child", **kw):
        return self.registry.register(
            parent=Endpoint(PARENT, HOST),
            child=Endpoint(child, HOST),
            issue_key=ISSUE,
            artifact_roots=[self.root],
            allowed_recipients=[PARENT],
            dispatch_request_id=kw.pop("dispatch_request_id", "replacement-after-cancel"),
            dispatch_turn_id="new-child-turn",
            **kw,
        )

    def owners(self):
        return self.store.all(
            "SELECT relationship_id, child_task_id, status FROM relationships"
            "  WHERE issue_key = ? AND status IN ('active','paused') AND superseded_by IS NULL",
            (ISSUE,),
        )

    def test_resume_after_the_issue_was_reassigned_is_refused(self):
        original = self.register()["relationshipId"]
        self.registry.set_status(original, "cancelled", actor="authorized cancel")
        replacement = self.replacement()["relationshipId"]

        self.assertRefused(
            RefusalReason.DUPLICATE_ASSIGNMENT,
            self.registry.resume, original,
            expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="old assignment resume",
        )
        # The refusal rolled back before writing, so NEITHER relationship moved.
        self.assertEqual(self.registry.get(original)["status"], "cancelled")
        self.assertEqual(self.registry.get(replacement)["status"], "active")
        self.assertEqual([row["relationship_id"] for row in self.owners()], [replacement])

    def test_a_paused_replacement_also_still_owns_the_issue(self):
        original = self.register()["relationshipId"]
        self.registry.set_status(original, "cancelled", actor="authorized cancel")
        replacement = self.replacement()["relationshipId"]
        self.registry.set_status(replacement, "paused", actor="user pause")
        self.assertRefused(
            RefusalReason.DUPLICATE_ASSIGNMENT,
            self.registry.resume, original,
            expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="old assignment resume",
        )

    def test_resume_still_works_when_the_issue_is_free(self):
        original = self.register()["relationshipId"]
        self.registry.set_status(original, "cancelled", actor="authorized cancel")
        resumed = self.registry.resume(
            original, expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="nobody else took it",
        )
        self.assertEqual(resumed["status"], "active")

    def test_a_superseded_replacement_does_not_block_the_original(self):
        original = self.register()["relationshipId"]
        self.registry.set_status(original, "cancelled", actor="authorized cancel")
        replacement = self.replacement()["relationshipId"]
        self.registry.supersede(replacement, new_relationship_id="rel-0000000000000000")
        resumed = self.registry.resume(
            original, expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="the replacement was superseded",
        )
        self.assertEqual(resumed["status"], "active")


class ResumeConcurrency(RelayTestCase):
    """A stale authorization must never outlive the world it was granted for.

    Two INDEPENDENT sqlite connections and two threads, no callback anywhere. A reentrant call
    on a connection that already holds the transaction is not a concurrent writer, so it cannot
    prove anything about serialisation; this races real writers and lets SQLite order them.
    """

    def test_a_stale_resume_never_leaves_the_newer_generation_active(self):
        import threading

        from codex_session_relay.registry import Registry
        from codex_session_relay.store import Store

        rid = self.register()["relationshipId"]
        self.registry.set_status(rid, "paused", actor=PARENT)

        barrier = threading.Barrier(2)
        errors = {}

        def stale_resume():
            store = Store(self.store.path)
            try:
                registry = Registry(store, self.clock)
                barrier.wait(timeout=20)
                registry.resume(
                    rid, expect_generation=1, expect_artifact_roots=[self.root],
                    expect_allowed_recipients=[PARENT], actor="stale generation-one resume",
                )
            except Exception as error:  # noqa: BLE001 - asserted below
                errors["stale"] = error
            finally:
                store.close()

        def advance_and_pause():
            store = Store(self.store.path)
            try:
                registry = Registry(store, self.clock)
                barrier.wait(timeout=20)
                registry.resume(
                    rid, expect_generation=1, expect_artifact_roots=[self.root],
                    expect_allowed_recipients=[PARENT], actor="other authorized resume",
                )
                registry.open_generation(
                    rid, dispatch_request_id="g2", reason="needs_changes_revision",
                    dispatch_turn_id="generation-two-turn",
                )
                registry.set_status(rid, "paused", actor="later user pause of generation two")
            except Exception as error:  # noqa: BLE001 - asserted below
                errors["advance"] = error
            finally:
                store.close()

        threads = [threading.Thread(target=stale_resume),
                   threading.Thread(target=advance_and_pause)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=60)

        self.assertNotIn("advance", errors, f"the legitimate sequence must succeed: {errors}")
        final = self.registry.get(rid)
        # Both legal orderings end here: either the stale resume was refused as stale, or it
        # activated generation 1 first and the later pause of generation 2 came after it.
        self.assertEqual(
            (final["executionGeneration"], final["status"]), (2, "paused"),
            f"final state {final['executionGeneration']}/{final['status']} with errors {errors}",
        )
        self.assertFalse(
            final["executionGeneration"] == 2 and final["status"] == "active",
            "a generation-1 authorization must never leave generation 2 active",
        )
        if "stale" in errors:
            self.assertEqual(errors["stale"].reason, RefusalReason.RELATIONSHIP_NOT_ACTIVE)
