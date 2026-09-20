"""The role a task holds, checked against the authorization recorded for it.

The bridge can ask whether a stated pair is a role's pair. Only this package can ask whether a
task IS that role, because only it holds the bindings. These cover the half that needs one.

Two real failures are reproduced here rather than described. A task created citing one role and
registered as another passed every check that existed. And a child reported to its parent under
the pair recorded when that parent was created, after the user had changed it, so a correct
message was withheld with a diagnosis that pointed at the settings rather than at the record
being out of date.
"""

import json
import unittest

from codex_session_relay import rolepolicy
from codex_session_relay.errors import RefusalReason, RegistrationError
from codex_session_relay.registry import record_settings
from codex_session_relay.transport import WITHHELD_PRE_SEND

from .support import CHILD, PARENT, DeliveryTestCase, task_settings

POLICY = {
    "roles": {
        "supervisor": {"expectation": "record"},
        "parent": {"model": "devin/swe-2", "reasoningEffort": "max"},
        "child": {"model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"},
    }
}


def write_policy(directory, mapping=None) -> str:
    path = directory / "execution-policy.json"
    path.write_text(json.dumps(mapping if mapping is not None else POLICY), encoding="utf-8")
    return str(path)


class RoleVocabularyIsShared(unittest.TestCase):
    def test_both_packages_spell_the_three_roles_the_same_way(self):
        """Neither package can import the other's vocabulary in both directions.

        A divergence would mean a role enforced on one side and unknown on the other, which is
        the kind of thing that looks fine in each file on its own.
        """
        from codex_session_relay.linkage import ROLE_SCOPE
        from codex_thread_bridge import roles

        self.assertEqual(set(roles.ROLES), set(ROLE_SCOPE))


class PolicyResolution(unittest.TestCase):
    def test_an_unset_variable_refuses_rather_than_passing_quietly(self):
        resolved = rolepolicy.declared({})
        self.assertFalse(resolved)
        self.assertIn("not set", resolved.detail)

    def test_a_policy_declaring_no_roles_is_also_unresolved(self, ):
        import tempfile
        from pathlib import Path

        with tempfile.TemporaryDirectory() as directory:
            path = write_policy(
                Path(directory),
                {"allowed": [{"model": "anthropic/claude-opus-5", "efforts": ["xhigh"]}]},
            )
            resolved = rolepolicy.declared({rolepolicy.ENVIRONMENT_VARIABLE: path})
        self.assertFalse(resolved)
        self.assertIn("declares no roles", resolved.detail)

    def test_the_declared_pairs_keep_max_and_xhigh_apart(self):
        import tempfile
        from pathlib import Path

        with tempfile.TemporaryDirectory() as directory:
            path = write_policy(Path(directory))
            resolved = rolepolicy.declared({rolepolicy.ENVIRONMENT_VARIABLE: path})
        self.assertTrue(resolved)
        self.assertEqual(resolved.expectation("parent").reasoning_effort, "max")
        self.assertEqual(resolved.expectation("child").reasoning_effort, "xhigh")
        self.assertIsNone(resolved.expectation("supervisor").model)
        self.assertIsNotNone(resolved.digest)


class DeliveryUnderARolePolicy(DeliveryTestCase):
    """The parent is bound as a parent, so the policy for that role applies to its record."""

    def setUp(self):
        super().setUp()
        import os
        from pathlib import Path

        from codex_session_relay.models import Endpoint

        self.policy_path = write_policy(Path(self.tmp))
        self._previous = os.environ.get(rolepolicy.ENVIRONMENT_VARIABLE)
        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = self.policy_path
        self.addCleanup(self._restore_environment)
        self.registry.linkage.bind_scope(
            role="parent", scope_key="PROJ-1",
            endpoint=Endpoint(PARENT, "host-a", cwd="/parent", cxc_session="cxc-parent"),
        )

    def _restore_environment(self):
        import os

        if self._previous is None:
            os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
        else:
            os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = self._previous

    def test_a_record_left_behind_by_a_user_transition_is_refused_before_any_send(self):
        """The EQP-10 shape. The record is not corrupt; it is out of date, and nothing looked.

        In production this reached the transport and came back as settings_not_preserved, which
        described the host's answer rather than the reason, and sent a coordinator looking at the
        wrong thing. Here it is decided before anything is sent, and the refusal names the record.

        The record is staged raw, which is how it arises in life: it was written before the
        policy said anything about this role, and going through the recorder now would be
        refused at registration by the very check this one backs up.
        """
        _relationship, event_id = self.queued_event(settings=task_settings("/parent"))
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [], "nothing may reach the host")
        self.assertEqual(self.adapter.settings_seen, [])
        self.assertEqual(self.attempts_for(event_id), [], "no attempt was claimed")
        self.assertEqual(self.delivery_row(event_id)["state"], WITHHELD_PRE_SEND)
        entry = self.store.all(
            "SELECT detail FROM journal WHERE kind = ? ORDER BY rowid DESC LIMIT 1",
            ("delivery_withheld",),
        )[0]
        self.assertIn(RefusalReason.SETTINGS_RECORD_STALE_FOR_ROLE.value, entry["detail"])
        self.assertIn("devin/swe-2", entry["detail"])
        self.assertIn("user_transition", entry["detail"])

    def test_re_recording_the_transition_lets_the_same_delivery_through(self):
        """The refusal is a withhold on the ordinary cadence, so nothing is lost meanwhile."""
        _relationship, event_id = self.queued_event(settings=task_settings("/parent"))
        self.assertIsNone(self.attempt(event_id))
        record_settings(
            self.store, self.clock, PARENT,
            task_settings("/parent", model="devin/swe-2", reasoningEffort="max"),
            source="user_transition", role="parent",
        )
        row = self.delivery_row(event_id)
        self.assertIsNotNone(self.attempt(event_id, now=row["next_eligible_at"]))
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_process_with_no_role_policy_withholds_instead_of_skipping_the_check(self):
        """Unenforced-but-quiet is the original failure with a green suite on top."""
        import os

        os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
        _relationship, event_id = self.queued_event(settings=task_settings("/parent"))
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [])
        self.assertEqual(self.delivery_row(event_id)["state"], WITHHELD_PRE_SEND)
        entry = self.store.all(
            "SELECT detail FROM journal WHERE kind = ? ORDER BY rowid DESC LIMIT 1",
            ("delivery_withheld",),
        )[0]
        self.assertIn(RefusalReason.ROLE_POLICY_UNCONFIGURED.value, entry["detail"])

    def test_a_task_bound_to_no_scope_is_outside_this_policy_entirely(self):
        """Declaring roles must not reach work that has nothing to do with these levels."""
        self.store.db.execute("DELETE FROM scope_bindings WHERE task_id = ?", (PARENT,))
        _relationship, event_id = self.queued_event(settings=task_settings("/parent"))
        self.assertIsNotNone(self.attempt(event_id))
        self.assertEqual(len(self.adapter.sends), 1)


class TheRoleATaskWasCreatedAsAndTheOneItIsBoundTo(DeliveryTestCase):
    """A task created citing one role and registered as another passed everything before this."""

    def setUp(self):
        super().setUp()
        import os
        from pathlib import Path

        self.policy_path = write_policy(Path(self.tmp))
        self._previous = os.environ.get(rolepolicy.ENVIRONMENT_VARIABLE)
        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = self.policy_path
        self.addCleanup(self._restore_environment)

    def _restore_environment(self):
        import os

        if self._previous is None:
            os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
        else:
            os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = self._previous

    def _bind(self, role, scope_key, task):
        from codex_session_relay.models import Endpoint

        return self.registry.linkage.bind_scope(
            role=role, scope_key=scope_key,
            endpoint=Endpoint(task, "host-a", cwd="/parent", cxc_session="cxc-parent"),
        )

    def test_settings_recorded_after_the_binding_are_refused_when_the_roles_disagree(self):
        self._bind("parent", "PROJ-1", PARENT)
        with self.assertRaises(RegistrationError) as raised:
            record_settings(
                self.store, self.clock, PARENT, task_settings("/parent"),
                source="creation_result", role="child",
            )
        self.assertEqual(raised.exception.reason, RefusalReason.ROLE_BINDING_MISMATCH)
        stored = self.store.one(
            "SELECT settings FROM authorized_settings WHERE task_id = ?", (PARENT,)
        )
        self.assertIsNone(stored, "the refused record must not have been written")

    def test_a_binding_made_after_the_settings_is_refused_the_same_way(self):
        """Whichever of the two arrives second performs the comparison, so neither order slips."""
        record_settings(
            self.store, self.clock, CHILD,
            task_settings("/parent", model="devin/swe-2", reasoningEffort="max"),
            source="creation_result", role="parent",
        )
        from codex_session_relay.errors import LinkageError

        with self.assertRaises(LinkageError) as raised:
            self._bind("child", "ISSUE-9", CHILD)
        self.assertEqual(raised.exception.reason, RefusalReason.ROLE_BINDING_MISMATCH)
        self.assertEqual(
            self.store.all("SELECT * FROM scope_bindings WHERE task_id = ?", (CHILD,)), []
        )

    def test_a_recorded_pair_that_is_not_the_bound_roles_pair_is_refused_at_registration(self):
        """Distinct from a stale record: this one was wrong when it was written.

        Recording it and letting the send-time check catch it later would offer "re-record" as
        the recovery, which here would write one side's answer over the other.
        """
        self._bind("parent", "PROJ-1", PARENT)
        with self.assertRaises(RegistrationError) as raised:
            record_settings(
                self.store, self.clock, PARENT, task_settings("/parent"),
                source="creation_result", role="parent",
            )
        self.assertEqual(raised.exception.reason, RefusalReason.ROLE_BINDING_MISMATCH)

    def test_a_matching_role_and_pair_records_normally(self):
        self._bind("parent", "PROJ-1", PARENT)
        recorded = record_settings(
            self.store, self.clock, PARENT,
            task_settings("/parent", model="devin/swe-2", reasoningEffort="max"),
            source="creation_result", role="parent",
        )
        self.assertEqual(recorded["settings"]["citedRole"], "parent")
        self.assertEqual(rolepolicy.bound_role(self.store, PARENT), "parent")


class WhatTheseChecksRefuseToGuessThrough(DeliveryTestCase):
    """Six ways the first draft of this gate reported a clean answer it had not earned."""

    def setUp(self):
        super().setUp()
        import os
        from pathlib import Path

        self._previous = os.environ.get(rolepolicy.ENVIRONMENT_VARIABLE)
        self.addCleanup(self._restore_environment)
        self.directory = Path(self.tmp)
        self._use(POLICY)

    def _use(self, mapping):
        import os

        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(self.directory, mapping)

    def _restore_environment(self):
        import os

        if self._previous is None:
            os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
        else:
            os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = self._previous

    def _bind(self, role, scope_key, task):
        from codex_session_relay.models import Endpoint

        return self.registry.linkage.bind_scope(
            role=role, scope_key=scope_key,
            endpoint=Endpoint(task, "host-a", cwd="/parent", cxc_session="cxc-parent"),
        )

    def _withheld_detail(self, event_id):
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [])
        self.assertEqual(self.delivery_row(event_id)["state"], WITHHELD_PRE_SEND)
        return self.store.all(
            "SELECT detail FROM journal WHERE kind = ? ORDER BY rowid DESC LIMIT 1",
            ("delivery_withheld",),
        )[0]["detail"]

    def test_a_policy_that_declares_only_some_roles_does_not_exempt_the_others(self):
        """A parent-only policy silently exempted every child on the host.

        Missing is not "nothing to check". The bridge refuses a cited role it has no entry for,
        and a partial policy that quietly passes here is the same hole on the other side.
        """
        self._use({"roles": {"parent": {"model": "devin/swe-2", "reasoningEffort": "max"}}})
        self._bind("child", "ISSUE-9", PARENT)
        _relationship, event_id = self.queued_event(settings=task_settings("/parent"))
        detail = self._withheld_detail(event_id)
        self.assertIn(RefusalReason.ROLE_POLICY_UNCONFIGURED.value, detail)
        self.assertIn("declares no such role", detail)

    def test_a_task_holding_two_roles_at_once_is_refused_rather_than_resolved_by_recency(self):
        """One task holds one role, so two live rows are a store contradicting itself.

        Choosing the most recent would compare the record against an arbitrary role and report a
        clean answer, which is the one outcome worth refusing.
        """
        self.store.db.execute(
            "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id,"
            " host_id, status, revision, created_at, updated_at)"
            " VALUES ('b1','parent','project','PROJ-1',?, 'host-a','active',1,'t','t')",
            (PARENT,),
        )
        self.store.db.execute(
            "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id,"
            " host_id, status, revision, created_at, updated_at)"
            " VALUES ('b2','child','issue','ISSUE-9',?, 'host-a','active',1,'t','t')",
            (PARENT,),
        )
        bound = rolepolicy.bound_role(self.store, PARENT)
        self.assertIsInstance(bound, rolepolicy.Contested)
        self.assertEqual(bound.roles, ["child", "parent"])
        _relationship, event_id = self.queued_event(settings=task_settings("/parent"))
        self.assertIn(
            RefusalReason.ROLE_BINDING_MISMATCH.value, self._withheld_detail(event_id)
        )

    def test_a_superseded_binding_is_not_read_as_the_role_a_task_still_holds(self):
        self.store.db.execute(
            "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id,"
            " host_id, status, revision, superseded_by, created_at, updated_at)"
            " VALUES ('b3','child','issue','ISSUE-9',?, 'host-a','active',1,'b4','t','t')",
            (PARENT,),
        )
        self.assertIsNone(rolepolicy.bound_role(self.store, PARENT))

    def test_a_re_record_that_names_no_role_keeps_the_one_the_creation_cited(self):
        """The cited role is a fact about creation, so a later write must not erase it.

        A re-record after a user transition is the ordinary case and carries no role. Dropping
        it there turned a task with a known creation into one with none, which then bound
        cleanly to any role at all.
        """
        record_settings(
            self.store, self.clock, PARENT,
            task_settings("/parent", model="devin/swe-2", reasoningEffort="max"),
            source="creation_result", role="parent",
        )
        record_settings(
            self.store, self.clock, PARENT,
            task_settings("/parent", model="devin/swe-2", reasoningEffort="max"),
            source="user_transition",
        )
        stored = json.loads(
            self.store.one(
                "SELECT settings FROM authorized_settings WHERE task_id = ?", (PARENT,)
            )["settings"]
        )
        self.assertEqual(stored["citedRole"], "parent")
        with self.assertRaises(Exception) as raised:
            self._bind("supervisor", "INIT-1", PARENT)
        self.assertIn(RefusalReason.ROLE_BINDING_MISMATCH.value, str(raised.exception))

    def test_doctor_reports_its_own_digest_and_does_not_claim_a_comparison_it_cannot_make(self):
        """get_capabilities is an MCP tool of the bridge, not an App Server method.

        Calling it through this adapter reported every ordinary run as unreachable, which reads
        as a broken bridge rather than as a question this surface cannot ask.
        """
        from codex_session_relay.cli import _role_policy_report

        class Services:
            adapter_requested = True

        report = _role_policy_report(Services())
        self.assertEqual(report["state"], "declared")
        self.assertIsNotNone(report["digest"])
        self.assertEqual(report["agreement"], "not_observable_from_here")
        self.assertIn("get_capabilities", report["compareWith"])
