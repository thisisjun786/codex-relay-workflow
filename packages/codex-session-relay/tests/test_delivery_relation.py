"""Recipient resolution read from the linkage instead of the relationship's frozen parent.

The defect: `delivery.enqueue` took its recipient from the relationship row, which froze its
parent at registration. A completion arriving after the project changed hands was therefore
addressed to the owner that stepped down, and passed `assert_assignment_delivery` because that
check compares against the same frozen row.

These cases are about the CONSUMER's refusal discipline, so the ones that need a store the real
reader cannot produce on demand - an unreadable store, two live owners - drive a stub reader
returning the documented shapes. `linkage.up` returning those shapes is asserted by linkage's
own suite; what is asserted here is that this consumer keeps the three answers apart instead of
collapsing them into a recipient.
"""

import unittest

from codex_session_relay import linkage as linkage_module
from codex_session_relay.delivery import (
    COMPLETION,
    ISSUE_SCOPE,
    PROJECT_SCOPE,
    REVISION,
    DeliveryService,
)
from codex_session_relay.errors import DeliveryRefused, RefusalReason

from .test_linkage import LinkageTestCase


class _Reader:
    """A linkage reader that answers exactly what it was told to, and records being asked."""

    def __init__(self, answer):
        self.answer = answer
        self.asked = []

    def up(self, **kwargs):
        self.asked.append(kwargs)
        return self.answer


def _relationship(parent="parent-task", child="child-task", rid="rel-1"):
    return {
        "relationshipId": rid,
        "parent": {"taskId": parent},
        "child": {"taskId": child},
    }


class TheScopeVocabularyIsShared(unittest.TestCase):
    def test_the_local_scope_constants_still_equal_the_linkage_ones(self):
        """Spelled locally to avoid an import ring, so drift has to be caught here."""
        self.assertEqual(PROJECT_SCOPE, linkage_module.PROJECT)
        self.assertEqual(ISSUE_SCOPE, linkage_module.ISSUE)


class WithoutALinkageNothingChanges(LinkageTestCase):
    def test_an_unwired_service_still_uses_the_relationship_row(self):
        """COMPATIBILITY: every caller written before the linkage keeps its behaviour."""
        service = DeliveryService(self.store, self.registry, self.intake, self.clock)
        self.assertIsNone(service.linkage)
        who, how = service.resolve_recipient(_relationship(), COMPLETION)
        self.assertEqual(who, "parent-task")
        self.assertEqual(how, {"source": "relationship_row", "verified": False})
        who, how = service.resolve_recipient(_relationship(), REVISION)
        self.assertEqual(who, "child-task")


class TheLinkageDecidesWhoReceives(LinkageTestCase):
    def service(self, answer):
        return DeliveryService(self.store, self.registry, self.intake, self.clock,
                               linkage=_Reader(answer))

    def test_an_agreeing_owner_resolves_and_is_marked_verified(self):
        service = self.service({
            "state": "registered", "readable": True, "gaps": [], "contention": [],
            "levels": [
                {"scopeKind": ISSUE_SCOPE, "scopeKey": "ISS-1",
                 "owner": {"taskId": "child-task", "revision": 1}, "depth": 0},
                {"scopeKind": PROJECT_SCOPE, "scopeKey": "PRJ-1",
                 "owner": {"taskId": "parent-task", "revision": 3}, "depth": 1},
            ],
        })
        who, how = service.resolve_recipient(_relationship(), COMPLETION)
        self.assertEqual(who, "parent-task")
        self.assertEqual(how["source"], "linkage")
        self.assertTrue(how["verified"])
        self.assertEqual(how["scopeKind"], PROJECT_SCOPE)
        self.assertEqual(how["scopeKey"], "PRJ-1")
        self.assertEqual(how["revision"], 3)
        self.assertEqual(service.linkage.asked, [{"relationship_id": "rel-1"}])

    def test_a_revision_resolves_against_the_issue_level_not_the_project(self):
        service = self.service({
            "state": "registered", "readable": True, "gaps": [], "contention": [],
            "levels": [
                {"scopeKind": ISSUE_SCOPE, "scopeKey": "ISS-1",
                 "owner": {"taskId": "child-task", "revision": 2}, "depth": 0},
                {"scopeKind": PROJECT_SCOPE, "scopeKey": "PRJ-1",
                 "owner": {"taskId": "parent-task", "revision": 3}, "depth": 1},
            ],
        })
        who, how = service.resolve_recipient(_relationship(), REVISION)
        self.assertEqual(who, "child-task")
        self.assertEqual(how["scopeKind"], ISSUE_SCOPE)

    def test_a_changed_owner_is_refused_rather_than_credited_to_either_task(self):
        """RED: the whole point. A late report must not be filed as the new owner's result."""
        service = self.service({
            "state": "registered", "readable": True, "gaps": [], "contention": [],
            "levels": [
                {"scopeKind": PROJECT_SCOPE, "scopeKey": "PRJ-1",
                 "owner": {"taskId": "replacement-parent", "revision": 4}, "depth": 1},
            ],
        })
        with self.assertRaises(DeliveryRefused) as raised:
            service.resolve_recipient(_relationship(), COMPLETION)
        self.assertEqual(raised.exception.reason, RefusalReason.RELATION_OWNER_DRIFT)
        self.assertIn("replacement-parent", str(raised.exception))
        self.assertIn("parent-task", str(raised.exception))

    def test_an_unreadable_store_never_falls_back_to_the_frozen_row(self):
        """readable false is not nothing-found, and is certainly not a recipient."""
        service = self.service({"state": "unreadable", "readable": False, "levels": [],
                                "gaps": [], "contention": []})
        with self.assertRaises(DeliveryRefused) as raised:
            service.resolve_recipient(_relationship(), COMPLETION)
        self.assertEqual(raised.exception.reason, RefusalReason.RELATION_UNREADABLE)
        self.assertNotIn("parent-task", str(raised.exception).split("relationship")[0])

    def test_nothing_found_is_reported_as_nothing_found(self):
        service = self.service({
            "state": "registered", "readable": True, "contention": [],
            "gaps": [{"gap": "project_without_parent", "scopeKind": PROJECT_SCOPE,
                      "scopeKey": "PRJ-1"}],
            "levels": [{"scopeKind": PROJECT_SCOPE, "scopeKey": "PRJ-1", "owner": None,
                        "depth": 1}],
        })
        with self.assertRaises(DeliveryRefused) as raised:
            service.resolve_recipient(_relationship(), COMPLETION)
        self.assertEqual(raised.exception.reason, RefusalReason.UNREGISTERED_SCOPE)
        self.assertIn("project_without_parent", str(raised.exception))

    def test_an_unregistered_relationship_is_refused_not_resolved(self):
        service = self.service({
            "state": "unregistered", "readable": True, "levels": [], "contention": [],
            "gaps": [{"gap": "unscoped_assignment", "relationshipId": "rel-1"}],
        })
        with self.assertRaises(DeliveryRefused) as raised:
            service.resolve_recipient(_relationship(), COMPLETION)
        self.assertEqual(raised.exception.reason, RefusalReason.UNREGISTERED_SCOPE)

    def test_two_live_owners_are_refused_without_choosing(self):
        service = self.service({
            "state": "ambiguous", "readable": True, "levels": [], "gaps": [],
            "contention": [{"contention": "competing_owners",
                            "candidates": ["parent-task", "other-parent"]}],
        })
        with self.assertRaises(DeliveryRefused) as raised:
            service.resolve_recipient(_relationship(), COMPLETION)
        self.assertEqual(raised.exception.reason, RefusalReason.DUPLICATE_SCOPE_OWNER)
        self.assertIn("competing_owners", str(raised.exception))


    def test_owner_drift_with_a_resolved_state_is_still_refused(self):
        """RED against the first Phase 2 pass: state is ambiguous only for competing owners.

        During a handover the issue-to-project edge can already name the incoming parent while
        the project binding still names the outgoing one. The owner then MATCHES the frozen row,
        so an equality check alone agrees with it and delivers to the parent stepping down.
        """
        service = self.service({
            "state": "registered", "readable": True, "gaps": [],
            "contention": [{"contention": "owner_drift", "scopeKind": PROJECT_SCOPE,
                            "scopeKey": "PRJ-1"}],
            "levels": [
                {"scopeKind": PROJECT_SCOPE, "scopeKey": "PRJ-1",
                 "owner": {"taskId": "parent-task", "revision": 3}, "depth": 1},
            ],
        })
        with self.assertRaises(DeliveryRefused) as raised:
            service.resolve_recipient(_relationship(), COMPLETION)
        self.assertEqual(raised.exception.reason, RefusalReason.RELATION_OWNER_DRIFT)
        self.assertIn("owner_drift", str(raised.exception))

    def test_any_other_reported_inconsistency_also_stops_delivery(self):
        """A resolved state carrying contention is not a resolved owner."""
        service = self.service({
            "state": "registered", "readable": True, "gaps": [],
            "contention": [{"contention": "instruction_conflict", "scopeKind": PROJECT_SCOPE,
                            "scopeKey": "PRJ-1", "digest": "d0"}],
            "levels": [
                {"scopeKind": PROJECT_SCOPE, "scopeKey": "PRJ-1",
                 "owner": {"taskId": "parent-task", "revision": 3}, "depth": 1},
            ],
        })
        with self.assertRaises(DeliveryRefused) as raised:
            service.resolve_recipient(_relationship(), COMPLETION)
        self.assertEqual(raised.exception.reason, RefusalReason.LINK_CONFLICT)
        self.assertIn("instruction_conflict", str(raised.exception))

    def test_an_undefined_direction_resolves_no_recipient(self):
        service = self.service({"state": "registered", "readable": True, "levels": [],
                                "gaps": [], "contention": []})
        with self.assertRaises(DeliveryRefused) as raised:
            service.resolve_recipient(_relationship(), "gossip")
        self.assertEqual(raised.exception.reason, RefusalReason.NOT_CLAIMABLE)


if __name__ == "__main__":
    unittest.main()
