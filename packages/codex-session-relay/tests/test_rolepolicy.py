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

# The pair the project parent runs on. It moved from devin/swe-2 at max to xai/grok-4.6 at
# xhigh on 2026-09-21, and moving it was an edit to the policy file and a restart: no pair is
# written in code, so nothing here had to change except the fixture that names one.
PARENT_MODEL = "xai/grok-4.6"
PARENT_EFFORT = "xhigh"
# What it ran on before. Kept as a fixture proving a superseded pair is refused for its role
# like any other wrong pair, not carried as a second answer the checks still accept.
SUPERSEDED_PARENT = ("devin/swe-2", "max")

POLICY = {
    "roles": {
        "supervisor": {"expectation": "record"},
        "parent": {"model": PARENT_MODEL, "reasoningEffort": PARENT_EFFORT},
        "child": {"model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"},
    }
}


def write_policy(directory, mapping=None) -> str:
    path = directory / "execution-policy.json"
    path.write_text(json.dumps(mapping if mapping is not None else POLICY), encoding="utf-8")
    # The policy is a per-process snapshot, exactly as the bridge's is, so a suite that stages
    # more than one has to say which one it means.
    rolepolicy.reset()
    return str(path)


def write_policy_without_reset(directory, mapping=None) -> str:
    """write_policy without dropping the snapshot, for the test that asks what a snapshot is.

    Every other test stages a policy and wants the next read to see it. This one stages one and
    wants the next read NOT to see it, which is the whole property being checked, so it cannot
    borrow a helper whose last act is to clear the cache.
    """
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

    def test_each_roles_effort_is_read_as_its_own_catalog_value(self):
        import tempfile
        from pathlib import Path

        with tempfile.TemporaryDirectory() as directory:
            path = write_policy(Path(directory))
            resolved = rolepolicy.declared({rolepolicy.ENVIRONMENT_VARIABLE: path})
        self.assertTrue(resolved)
        self.assertEqual(resolved.expectation("parent").model, PARENT_MODEL)
        self.assertEqual(resolved.expectation("parent").reasoning_effort, PARENT_EFFORT)
        self.assertEqual(resolved.expectation("child").reasoning_effort, "xhigh")
        self.assertIsNone(resolved.expectation("supervisor").model)
        self.assertIsNotNone(resolved.digest)
        # The two roles happen to share an effort NAME under different models, which is not what
        # makes the comparison work: the superseded parent pair is the case where they differ.
        self.assertNotEqual(resolved.expectation("parent").model,
                            resolved.expectation("child").model)
        self.assertNotEqual(SUPERSEDED_PARENT[1], PARENT_EFFORT)


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
        rolepolicy.reset()

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
        self.assertIn(PARENT_MODEL, entry["detail"])
        self.assertIn("user_transition", entry["detail"])

    def test_re_recording_the_transition_lets_the_same_delivery_through(self):
        """The refusal is a withhold on the ordinary cadence, so nothing is lost meanwhile."""
        _relationship, event_id = self.queued_event(settings=task_settings("/parent"))
        self.assertIsNone(self.attempt(event_id))
        record_settings(
            self.store, self.clock, PARENT,
            task_settings("/parent", model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT),
            source="user_transition", role="parent",
        )
        row = self.delivery_row(event_id)
        self.assertIsNotNone(self.attempt(event_id, now=row["next_eligible_at"]))
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_process_with_no_role_policy_withholds_instead_of_skipping_the_check(self):
        """Unenforced-but-quiet is the original failure with a green suite on top."""
        import os

        os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
        rolepolicy.reset()
        _relationship, event_id = self.queued_event(settings=task_settings("/parent"))
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [])
        self.assertEqual(self.delivery_row(event_id)["state"], WITHHELD_PRE_SEND)
        entry = self.store.all(
            "SELECT detail FROM journal WHERE kind = ? ORDER BY rowid DESC LIMIT 1",
            ("delivery_withheld",),
        )[0]
        self.assertIn(RefusalReason.ROLE_POLICY_UNCONFIGURED.value, entry["detail"])

    def test_the_pair_this_role_used_to_run_on_is_recognised_as_superseded(self):
        """A pair change costs a file edit, and the old pair stops being an answer.

        The parent moved from devin/swe-2 at max to xai/grok-4.6 at xhigh. A record still
        carrying the former is stale for its role in exactly the way any other wrong pair is,
        and it is kept here as a fixture rather than as an alternative that still passes.
        """
        from pathlib import Path

        superseded = task_settings(
            str(Path(self.tmp).resolve()),
            model=SUPERSEDED_PARENT[0], reasoningEffort=SUPERSEDED_PARENT[1],
        )
        policy = rolepolicy.declared()
        finding = rolepolicy.check_record(superseded, "parent", policy)
        self.assertIsNotNone(finding, "the superseded pair must not read as current")
        self.assertEqual(
            finding["code"], RefusalReason.SETTINGS_RECORD_STALE_FOR_ROLE.value
        )
        self.assertEqual(finding["expected"]["model"], PARENT_MODEL)
        self.assertEqual(finding["recorded"]["model"], SUPERSEDED_PARENT[0])
        # And the current pair is clean, so the fixture is testing the change rather than a
        # policy that refuses everything.
        current = task_settings(
            str(Path(self.tmp).resolve()), model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT,
        )
        self.assertIsNone(rolepolicy.check_record(current, "parent", policy))

    def test_a_task_bound_to_no_scope_is_outside_this_policy_entirely(self):
        """Declaring roles must not reach work that has nothing to do with these levels."""
        # CHILD is bound to nothing in this fixture, so it is the unbound case, and its record
        # carries a pair no role declares. The gate returns it untouched rather than measuring
        # it against a role it does not hold.
        record_settings(
            self.store, self.clock, CHILD, task_settings("/child"), source="creation_result",
        )
        self.assertIsNone(rolepolicy.bound_role(self.store, CHILD))
        settings = self.delivery._settings_for(CHILD, "idle")
        self.assertEqual(settings.data["model"], "anthropic/claude-opus-5")
        self.assertFalse(settings.refuse_when_unloaded)

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
        rolepolicy.reset()

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
            task_settings("/parent", model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT),
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
            task_settings("/parent", model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT),
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
        rolepolicy.reset()

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
        self._use({"roles": {"parent": {"model": PARENT_MODEL, "reasoningEffort": PARENT_EFFORT}}})
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
            task_settings("/parent", model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT),
            source="creation_result", role="parent",
        )
        record_settings(
            self.store, self.clock, PARENT,
            task_settings("/parent", model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT),
            source="user_transition",
        )
        stored = json.loads(
            self.store.one(
                "SELECT settings FROM authorized_settings WHERE task_id = ?", (PARENT,)
            )["settings"]
        )
        self.assertEqual(stored["citedRole"], "parent")
        from codex_session_relay.errors import LinkageError

        with self.assertRaises(LinkageError) as raised:
            self._bind("supervisor", "INIT-1", PARENT)
        self.assertEqual(raised.exception.reason, RefusalReason.ROLE_BINDING_MISMATCH)

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


class TheRelayHasItsOwnTransportAndMustApplyTheSameRule(DeliveryTestCase):
    """The bridge's tool path refuses some resumes. A relay delivery never takes that path."""

    def setUp(self):
        super().setUp()
        import os
        from pathlib import Path

        from codex_session_relay.models import Endpoint

        self._previous = os.environ.get(rolepolicy.ENVIRONMENT_VARIABLE)
        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp))
        self.addCleanup(self._restore_environment)
        self.registry.linkage.bind_scope(
            role="supervisor", scope_key="INIT-1",
            endpoint=Endpoint(PARENT, "host-a", cwd="/parent", cxc_session="cxc-parent"),
        )

    def _restore_environment(self):
        import os

        if self._previous is None:
            os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
        else:
            os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = self._previous
        rolepolicy.reset()

    def test_an_unloaded_supervisor_is_not_resumed_with_a_pair_policy_never_derived(self):
        """Its pair is the user's own selection and the transmitted pair is whatever was
        recorded, which is exactly what goes stale when they change it. A resume may apply what
        it transmits to a thread the host has to load first.
        """
        self.adapter.set_status(PARENT, "notLoaded")
        _relationship, event_id = self.queued_event(settings=task_settings("/parent"))
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [], "nothing may reach the host")
        self.assertEqual(self.delivery_row(event_id)["state"], WITHHELD_PRE_SEND)
        detail = self.store.all(
            "SELECT detail FROM journal WHERE kind = ? ORDER BY rowid DESC LIMIT 1",
            ("delivery_withheld",),
        )[0]["detail"]
        self.assertIn("notLoaded", detail)

    def test_the_same_supervisor_is_delivered_to_once_the_host_has_it_loaded(self):
        """Where the resume reports the thread's own state, the comparison means something."""
        self.adapter.set_status(PARENT, "idle")
        _relationship, event_id = self.queued_event(settings=task_settings("/parent"))
        self.assertIsNotNone(self.attempt(event_id))
        self.assertEqual(len(self.adapter.sends), 1)


    def test_a_recipient_that_unloads_after_the_observation_is_still_refused(self):
        """The gate's status is older than the resume by a turn listing and a claim.

        Deciding only on that older read let a recipient unload in between and be resumed under
        exactly the pair the decision meant never to transmit, so the transport applies the same
        rule on the read it takes immediately before resuming.
        """
        from codex_session_relay.registry import load_settings

        self.adapter.set_status(PARENT, "idle")
        _relationship, event_id = self.queued_event(settings=task_settings("/parent"))
        settings = self.delivery._settings_for(PARENT, "idle")
        self.assertTrue(
            settings.refuse_when_unloaded,
            "a supervisor's pair is never policy-derived, so the transport must be told",
        )
        # And a recipient whose pair IS the declared one carries no such instruction.
        record_settings(
            self.store, self.clock, CHILD,
            task_settings("/child", model="anthropic/claude-opus-5", reasoningEffort="xhigh"),
            source="creation_result", role="child",
        )
        from codex_session_relay.models import Endpoint

        self.registry.linkage.bind_scope(
            role="child", scope_key="ISSUE-77",
            endpoint=Endpoint(CHILD, "host-a", cwd="/child", cxc_session="cxc-child"),
        )
        self.assertFalse(load_settings(self.store, CHILD) is None)
        child = self.delivery._settings_for(CHILD, "idle")
        self.assertFalse(child.refuse_when_unloaded)



    def test_a_legacy_record_citing_an_unauthorized_exception_is_withheld_not_crashed(self):
        """The send-time gate exists to revalidate records an earlier writer admitted.

        Formatting every finding as though it were a stale record read fields a citation finding
        does not carry, raised out of the gate, and left the delivery queued instead of withheld
        -- a revalidation path failing open on exactly the records it exists to catch.
        """
        from pathlib import Path

        settings = task_settings(
            str(Path(self.tmp).resolve()), model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT,
            citedException="not-written",
        )
        _relationship, event_id = self.queued_event(settings=settings)
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [], "nothing may reach the host")
        self.assertEqual(self.delivery_row(event_id)["state"], WITHHELD_PRE_SEND)
        detail = self.store.all(
            "SELECT detail FROM journal WHERE kind = ? ORDER BY rowid DESC LIMIT 1",
            ("delivery_withheld",),
        )[0]["detail"]
        self.assertIn("not-written", detail)
        self.assertIn(RefusalReason.ROLE_BINDING_MISMATCH.value, detail)



class AnOperatorExceptionIsRecognisedRatherThanContradicted(DeliveryTestCase):
    """A role-scoped exception replaces the role-pair comparison by design.

    A task legitimately created under one carries a pair its role's policy does not declare, so
    refusing it here would refuse exactly what the operator approved.
    """

    def setUp(self):
        super().setUp()
        import os
        from pathlib import Path

        self._previous = os.environ.get(rolepolicy.ENVIRONMENT_VARIABLE)
        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp), {
            **POLICY,
            "exceptions": {
                "one-task": {
                    "model": "gpt-6-astra",
                    "reasoningEffort": "high",
                    "cwd": [str(Path(self.tmp).resolve())],
                    "role": "parent",
                }
            },
        })
        self.addCleanup(self._restore_environment)

    def _restore_environment(self):
        import os

        if self._previous is None:
            os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
        else:
            os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = self._previous
        rolepolicy.reset()

    def _bind_parent(self):
        from codex_session_relay.models import Endpoint

        return self.registry.linkage.bind_scope(
            role="parent", scope_key="PROJ-1",
            endpoint=Endpoint(PARENT, "host-a", cwd="/parent", cxc_session="cxc-parent"),
        )

    def _excepted(self):
        # The cwd the exception actually covers. An earlier version of this fixture recorded
        # /parent while the exception named the temporary directory, which the bridge refuses
        # and the relay was accepting -- the test asserted the hole rather than the rule.
        from pathlib import Path

        return task_settings(
            str(Path(self.tmp).resolve()), model="gpt-6-astra", reasoningEffort="high",
        )

    def test_an_exception_does_not_cover_a_checkout_it_was_not_written_for(self):
        """The bridge checks the directory too, so a reader that skips it approves what it
        refuses -- two readers reaching different answers about one document."""
        self._bind_parent()
        elsewhere = task_settings("/somewhere-else", model="gpt-6-astra", reasoningEffort="high")
        with self.assertRaises(RegistrationError) as raised:
            record_settings(
                self.store, self.clock, PARENT, elsewhere,
                source="creation_result", role="parent", exception="one-task",
            )
        self.assertEqual(raised.exception.reason, RefusalReason.ROLE_BINDING_MISMATCH)

    def test_a_record_the_operators_own_exception_authorized_is_recorded_and_bound(self):
        self._bind_parent()
        recorded = record_settings(
            self.store, self.clock, PARENT, self._excepted(),
            source="creation_result", role="parent", exception="one-task",
        )
        self.assertEqual(recorded["settings"]["citedException"], "one-task")

    def test_an_exception_written_for_another_role_exempts_nothing(self):
        """Verified against the same file, not believed because the record says so."""
        self._bind_parent()
        with self.assertRaises(RegistrationError) as raised:
            record_settings(
                self.store, self.clock, PARENT, self._excepted(),
                source="creation_result", role="parent", exception="not-written-by-anyone",
            )
        self.assertEqual(raised.exception.reason, RefusalReason.ROLE_BINDING_MISMATCH)

    def test_a_citation_this_policy_does_not_authorize_is_refused_even_on_the_declared_pair(self):
        """Pair equality returns clean before the citation is ever looked at.

        The citation is not inert when that happens: the unloaded guard reads it and withholds a
        delivery whose pair had perfectly good role provenance, on the strength of an id nobody
        wrote. So a citation is verified whenever one is present.
        """
        from pathlib import Path

        self._bind_parent()
        declared = task_settings(
            str(Path(self.tmp).resolve()), model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT,
        )
        with self.assertRaises(RegistrationError) as raised:
            record_settings(
                self.store, self.clock, PARENT, declared,
                source="creation_result", role="parent", exception="not-written",
            )
        self.assertEqual(raised.exception.reason, RefusalReason.ROLE_BINDING_MISMATCH)
        # The same record with no citation is policy-derived and records normally.
        recorded = record_settings(
            self.store, self.clock, PARENT, declared, source="creation_result", role="parent",
        )
        self.assertNotIn("citedException", recorded["settings"])

    def test_an_exception_that_matches_the_role_pair_is_still_an_exception(self):
        """Provenance, not resemblance.

        An exception authorizing the same values the role pair declares still authorized them AS
        an exception, and the comparison it skipped is the one the unloaded rule depends on.
        Reducing that to pair equality let this record through here while the bridge's own tool
        path refused it.
        """
        from pathlib import Path

        import os

        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp), {
            **POLICY,
            "exceptions": {
                "same-pair": {
                    "model": PARENT_MODEL,
                    "reasoningEffort": PARENT_EFFORT,
                    "cwd": [str(Path(self.tmp).resolve())],
                    "role": "parent",
                }
            },
        })
        settings = task_settings(
            str(Path(self.tmp).resolve()), model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT,
            citedException="same-pair",
        )
        policy = rolepolicy.declared()
        refusal = rolepolicy.check_unloaded_transmission(
            settings, "parent", policy, "notLoaded"
        )
        self.assertIsNotNone(refusal)
        self.assertEqual(
            refusal.reason, RefusalReason.UNVERIFIED_PAIR_FOR_UNLOADED_THREAD
        )
        # And the way out is reachable through the supported recorder rather than by editing a
        # dictionary: re-recording onto the role's declared pair drops the exception, because an
        # exception authorizes one pair and this is no longer that pair.
        self._bind_parent()
        record_settings(
            self.store, self.clock, PARENT, dict(settings),
            source="creation_result", role="parent", exception="same-pair",
        )
        record_settings(
            self.store, self.clock, PARENT,
            task_settings(
                str(Path(self.tmp).resolve()), model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT,
            ),
            source="user_transition",
        )
        stored = json.loads(
            self.store.one(
                "SELECT settings FROM authorized_settings WHERE task_id = ?", (PARENT,)
            )["settings"]
        )
        self.assertNotIn("citedException", stored)
        self.assertIsNone(
            rolepolicy.check_unloaded_transmission(stored, "parent", policy, "notLoaded")
        )

    def test_a_re_record_that_names_no_exception_keeps_the_one_already_recorded(self):
        self._bind_parent()
        record_settings(
            self.store, self.clock, PARENT, self._excepted(),
            source="creation_result", role="parent", exception="one-task",
        )
        record_settings(
            self.store, self.clock, PARENT, self._excepted(), source="user_transition",
        )
        stored = json.loads(
            self.store.one(
                "SELECT settings FROM authorized_settings WHERE task_id = ?", (PARENT,)
            )["settings"]
        )
        self.assertEqual(stored["citedException"], "one-task")


    def test_a_user_transition_can_leave_a_citation_a_supervisor_no_longer_needs(self):
        """A supervisor has no declared pair, so 'the pair now stands on its own' can never
        release its citation. Without an explicit way out the recorder restored the old id and
        then refused its own write, and no documented command could break the loop."""
        from pathlib import Path

        from codex_session_relay.models import Endpoint

        self.registry.linkage.bind_scope(
            role="supervisor", scope_key="INIT-1",
            endpoint=Endpoint(PARENT, "host-a", cwd="/parent", cxc_session="cxc-parent"),
        )
        here = str(Path(self.tmp).resolve())
        import os

        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp), {
            **POLICY,
            "exceptions": {
                "temporary": {
                    "model": "gpt-6-astra", "reasoningEffort": "high",
                    "cwd": [here], "role": "supervisor",
                }
            },
        })
        record_settings(
            self.store, self.clock, PARENT,
            task_settings(here, model="gpt-6-astra", reasoningEffort="high"),
            source="creation_result", role="supervisor", exception="temporary",
        )
        # The user moves it to an effort the exception never covered.
        record_settings(
            self.store, self.clock, PARENT,
            task_settings(here, model="gpt-6-astra", reasoningEffort="max"),
            source="user_transition",
        )
        stored = json.loads(
            self.store.one(
                "SELECT settings FROM authorized_settings WHERE task_id = ?", (PARENT,)
            )["settings"]
        )
        self.assertNotIn("citedException", stored)
        self.assertEqual(stored["citedRole"], "supervisor")


    def test_a_citation_can_be_cleared_when_the_pair_never_moved(self):
        """An exception can stop applying without the pair moving at all.

        The operator removes it and the user confirms the task stays where it is. Keying the
        carry-forward on pair equality restored a citation the policy no longer authorized and
        then refused its own write, leaving the task undeliverable with no command able to
        release it. Clearing is therefore said rather than inferred.
        """
        from pathlib import Path

        from codex_session_relay.registry import CLEAR_EXCEPTION
        from codex_session_relay.models import Endpoint

        self.registry.linkage.bind_scope(
            role="supervisor", scope_key="INIT-2",
            endpoint=Endpoint(PARENT, "host-a", cwd="/parent", cxc_session="cxc-parent"),
        )
        here = str(Path(self.tmp).resolve())
        import os

        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp), {
            **POLICY,
            "exceptions": {
                "temporary": {
                    "model": "gpt-6-astra", "reasoningEffort": "high",
                    "cwd": [here], "role": "supervisor",
                }
            },
        })
        settings = task_settings(here, model="gpt-6-astra", reasoningEffort="high")
        record_settings(
            self.store, self.clock, PARENT, settings,
            source="creation_result", role="supervisor", exception="temporary",
        )
        # The operator removes the exception; the pair itself does not move.
        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp), POLICY)
        record_settings(
            self.store, self.clock, PARENT, settings,
            source="user_transition", exception=CLEAR_EXCEPTION,
        )
        stored = json.loads(
            self.store.one(
                "SELECT settings FROM authorized_settings WHERE task_id = ?", (PARENT,)
            )["settings"]
        )
        self.assertNotIn("citedException", stored)
        self.assertEqual(stored["citedRole"], "supervisor")


    def test_an_exception_actually_named_the_sentinel_is_still_an_identifier(self):
        """Any non-empty string is a legal exception id, so the sentinel cannot be one.

        An operator may declare an exception named "__clear__", and a creation receipt citing
        it used to be indistinguishable from the CLI asking to remove a citation: the recorder
        dropped the citation it was in the middle of writing and then refused the
        exception-authorized pair against the role's declared one. Reaching the end of this
        test at all is the proof, because gpt-6-astra/high is not the parent pair and only the
        surviving citation authorizes it.
        """
        from pathlib import Path
        import os

        from codex_session_relay.registry import CLEAR_EXCEPTION

        # Not a string, so no policy file can spell it. This is the invariant the rest rests on.
        self.assertNotIsInstance(CLEAR_EXCEPTION, str)
        self._bind_parent()
        here = str(Path(self.tmp).resolve())
        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp), {
            **POLICY,
            "exceptions": {
                "__clear__": {
                    "model": "gpt-6-astra", "reasoningEffort": "high",
                    "cwd": [here], "role": "parent",
                }
            },
        })
        record_settings(
            self.store, self.clock, PARENT, self._excepted(),
            source="creation_result", role="parent", exception="__clear__",
        )
        stored = json.loads(
            self.store.one(
                "SELECT settings FROM authorized_settings WHERE task_id = ?", (PARENT,)
            )["settings"]
        )
        self.assertEqual(stored["citedException"], "__clear__")
        # And an ordinary re-record carries it forward rather than reading it as a command.
        record_settings(
            self.store, self.clock, PARENT, self._excepted(), source="user_transition",
        )
        stored = json.loads(
            self.store.one(
                "SELECT settings FROM authorized_settings WHERE task_id = ?", (PARENT,)
            )["settings"]
        )
        self.assertEqual(stored["citedException"], "__clear__")


    def test_a_registration_that_will_bind_a_child_refuses_a_parent_citation_first(self):
        """The citation is checked against the role the write would establish, not itself.

        With --project the registration binds the relationship's child task to its issue scope
        as a child. Comparing the cited role against the cited role answered a different
        question and passed, so the relationship and the binding were both committed and only
        record_settings -- reading the binding that had just landed -- refused. The refusal was
        reported and a live wrong-role assignment stayed behind it.
        """
        import argparse
        import json as _json
        import os
        import types
        from pathlib import Path

        from codex_session_relay.cli import cmd_register
        from codex_session_relay.models import Endpoint

        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp))
        self.registry.linkage.bind_scope(
            role="parent", scope_key="PROJ-1",
            endpoint=Endpoint(PARENT, "host-a", cwd="/parent", cxc_session="cxc-parent"),
        )
        # The parent pair, cited as parent, for the task this registration will bind as a child.
        # Every value here is individually legitimate; only their combination is not.
        child_settings = task_settings(
            self.root, model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT,
        )
        args = argparse.Namespace(
            parent_task=PARENT, parent_host="host-a", parent_cwd="/parent",
            parent_cxc_session="cxc-parent",
            child_task=CHILD, child_host="host-a", child_cwd=self.root,
            child_cxc_session="cxc-child",
            issue="ISS-1", artifact_root=[self.root], allowed_recipient=[PARENT],
            scope_ref=None, dispatch_request_id="dispatch-1", dispatch_turn_id=None,
            supersedes=None, project="PROJ-1",
            parent_settings=None, parent_role=None, parent_exception=None,
            child_settings=_json.dumps(child_settings.data if hasattr(child_settings, "data")
                                       else child_settings),
            child_role="parent", child_exception=None,
        )
        services = types.SimpleNamespace(
            registry=self.registry, store=self.store, clock=self.clock,
        )
        with self.assertRaises(RegistrationError) as raised:
            cmd_register(services, args)
        self.assertEqual(raised.exception.reason, RefusalReason.ROLE_BINDING_MISMATCH)
        # And the point of moving the check earlier: the refusal left nothing behind it.
        self.assertEqual(
            self.store.all("SELECT relationship_id FROM relationships"), [],
            "the registration was committed before the contradiction was noticed",
        )
        self.assertIsNone(
            rolepolicy.bound_role(self.store, CHILD),
            "a refused registration left the child task holding a live binding",
        )
        self.assertIsNone(
            self.store.one(
                "SELECT settings FROM authorized_settings WHERE task_id = ?", (CHILD,)
            ),
        )


    def test_replaying_a_live_binding_survives_a_policy_edit_that_postdates_it(self):
        """Repeating a claim converges on the record; it does not re-adjudicate it.

        The role check ran before the existing binding was read, so a retry of the original
        claim -- same role, same scope, same endpoint, nothing to write -- started failing after
        an unrelated edit to the parent pair. A record going stale against a new policy has its
        own refusal at send time and is not a reason to break an idempotent recovery.
        """
        import os
        from pathlib import Path

        from codex_session_relay.models import Endpoint

        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp))
        endpoint = Endpoint(PARENT, "host-a", cwd="/parent", cxc_session="cxc-parent")
        record_settings(
            self.store, self.clock, PARENT,
            task_settings("/parent", model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT),
            source="creation_result", role="parent",
        )
        first = self.registry.linkage.bind_scope(
            role="parent", scope_key="PROJ-1", endpoint=endpoint,
        )
        # The operator moves the parent pair. The live binding is untouched by that edit.
        superseded_model, superseded_effort = SUPERSEDED_PARENT
        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp), {
            "roles": {
                "supervisor": {"expectation": "record"},
                "parent": {"model": superseded_model, "reasoningEffort": superseded_effort},
                "child": {"model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"},
            }
        })
        again = self.registry.linkage.bind_scope(
            role="parent", scope_key="PROJ-1", endpoint=endpoint,
        )
        self.assertEqual(again["bindingId"], first["bindingId"])
        self.assertEqual(rolepolicy.bound_role(self.store, PARENT), "parent")


    def test_a_new_binding_is_still_refused_under_the_policy_in_force(self):
        """Moving the check to where a binding is established must not remove it. An insert
        adjudicates against the current policy exactly as it did before."""
        import os
        from pathlib import Path

        from codex_session_relay.errors import LinkageError
        from codex_session_relay.models import Endpoint

        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp))
        # Created citing child, now being bound as a parent: the contradiction, on a fresh
        # binding with nothing to replay.
        record_settings(
            self.store, self.clock, PARENT,
            task_settings("/parent", model="anthropic/claude-opus-5", reasoningEffort="xhigh"),
            source="creation_result", role="child",
        )
        with self.assertRaises(LinkageError) as raised:
            self.registry.linkage.bind_scope(
                role="parent", scope_key="PROJ-9",
                endpoint=Endpoint(PARENT, "host-a", cwd="/parent", cxc_session="cxc-parent"),
            )
        self.assertEqual(raised.exception.reason, RefusalReason.ROLE_BINDING_MISMATCH)
        self.assertIsNone(rolepolicy.bound_role(self.store, PARENT))


    def test_settings_show_separates_a_complete_record_from_a_deliverable_one(self):
        """A preflight reads one field, and it was the field that could not see a role.

        "usable" asks whether the record has its required fields, and it is paired with
        "missing"; a record that is complete and still refused has to be able to say both. So
        the role answer is its own field rather than folded into that one, and a consumer
        written before roles existed reads true from "usable" only about what it always meant.
        """
        import argparse
        import os
        import types
        from pathlib import Path

        from codex_session_relay.cli import cmd_settings_show
        from codex_session_relay.models import Endpoint

        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp))
        self.registry.linkage.bind_scope(
            role="parent", scope_key="PROJ-1",
            endpoint=Endpoint(PARENT, "host-a", cwd="/parent", cxc_session="cxc-parent"),
        )
        services = types.SimpleNamespace(
            registry=self.registry, store=self.store, clock=self.clock,
        )
        record_settings(
            self.store, self.clock, PARENT,
            task_settings("/parent", model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT),
            source="creation_result", role="parent",
        )
        shown = cmd_settings_show(services, argparse.Namespace(task=PARENT))
        self.assertTrue(shown["usable"])
        self.assertTrue(shown["deliverable"])
        self.assertIsNone(shown["roleFinding"])
        # The operator moves the parent pair; the record is complete and now stale for its role.
        superseded_model, superseded_effort = SUPERSEDED_PARENT
        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp), {
            "roles": {
                "supervisor": {"expectation": "record"},
                "parent": {"model": superseded_model, "reasoningEffort": superseded_effort},
                "child": {"model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"},
            }
        })
        shown = cmd_settings_show(services, argparse.Namespace(task=PARENT))
        self.assertTrue(shown["usable"], "the record itself did not become incomplete")
        self.assertEqual(shown["missing"], [])
        self.assertFalse(shown["deliverable"])
        self.assertIsNotNone(shown["roleFinding"])


    def test_a_bound_task_is_not_deliverable_when_this_process_cannot_read_a_policy(self):
        """Delivery refuses this state, so reporting it clear would disagree with the only
        consumer that acts on the answer. Not checkable is not the same as checked and fine."""
        import argparse
        import os
        import types

        from codex_session_relay.cli import cmd_settings_show
        from codex_session_relay.errors import RefusalReason as Reason
        from codex_session_relay.models import Endpoint

        os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
        rolepolicy.reset()
        self.registry.linkage.bind_scope(
            role="parent", scope_key="PROJ-1",
            endpoint=Endpoint(PARENT, "host-a", cwd="/parent", cxc_session="cxc-parent"),
        )
        record_settings(
            self.store, self.clock, PARENT,
            task_settings("/parent", model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT),
            source="creation_result",
        )
        services = types.SimpleNamespace(
            registry=self.registry, store=self.store, clock=self.clock,
        )
        shown = cmd_settings_show(services, argparse.Namespace(task=PARENT))
        self.assertTrue(shown["usable"], "the record itself is complete")
        self.assertFalse(shown["deliverable"])
        self.assertEqual(shown["rolePolicy"], "unresolved")
        self.assertEqual(
            shown["roleFinding"]["code"], Reason.ROLE_POLICY_UNCONFIGURED.value,
        )


    def test_the_policy_snapshot_is_this_process_rather_than_this_question(self):
        """A lazy first read made the snapshot whenever a role question first came up.

        A daemon could start under one version of the file, serve unbound work, and then adopt
        an edit the bridge had never seen. main() takes the snapshot before any work, so the
        version in force is the one the process started with.
        """
        import os
        from pathlib import Path

        from codex_session_relay.cli import main

        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp))
        started_on = rolepolicy.declared().digest
        rolepolicy.reset()
        # A command that asks no role question at all, standing in for the daemon's startup.
        main(["--state", self.tmp, "doctor", "--issue", "ISS-404"])
        superseded_model, superseded_effort = SUPERSEDED_PARENT
        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy_without_reset(
            Path(self.tmp), {
                "roles": {
                    "supervisor": {"expectation": "record"},
                    "parent": {"model": superseded_model,
                               "reasoningEffort": superseded_effort},
                    "child": {"model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"},
                }
            },
        )
        self.assertEqual(
            rolepolicy.declared().digest, started_on,
            "the edit was adopted without a restart, so the two processes could disagree",
        )


    def test_an_incomplete_settings_file_refuses_before_the_relationship_commits(self):
        """The partial registration the role check closed, arriving through a second door."""
        import argparse
        import json as _json
        import os
        import types
        from pathlib import Path

        from codex_session_relay.cli import cmd_register

        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp))
        args = argparse.Namespace(
            parent_task=PARENT, parent_host="host-a", parent_cwd="/parent",
            parent_cxc_session="cxc-parent",
            child_task=CHILD, child_host="host-a", child_cwd=self.root,
            child_cxc_session="cxc-child",
            issue="ISS-1", artifact_root=[self.root], allowed_recipient=[PARENT],
            scope_ref=None, dispatch_request_id="dispatch-1", dispatch_turn_id=None,
            supersedes=None, project=None,
            parent_settings=None, parent_role=None, parent_exception=None,
            # No role cited, so only the completeness question can catch this one.
            child_settings=_json.dumps({"model": "anthropic/claude-opus-5"}),
            child_role=None, child_exception=None,
        )
        services = types.SimpleNamespace(
            registry=self.registry, store=self.store, clock=self.clock,
        )
        with self.assertRaises(Exception):
            cmd_register(services, args)
        self.assertEqual(
            self.store.all("SELECT relationship_id FROM relationships"), [],
            "an incomplete settings file committed the relationship before it was refused",
        )


    def test_an_ordinary_re_record_still_keeps_a_citation_that_is_still_doing_work(self):
        """Clearing is the user-attributed transition's privilege, not every write's."""
        from pathlib import Path

        self._bind_parent()
        record_settings(
            self.store, self.clock, PARENT, self._excepted(),
            source="creation_result", role="parent", exception="one-task",
        )
        record_settings(
            self.store, self.clock, PARENT, self._excepted(), source="creation_result",
        )
        stored = json.loads(
            self.store.one(
                "SELECT settings FROM authorized_settings WHERE task_id = ?", (PARENT,)
            )["settings"]
        )
        self.assertEqual(stored["citedException"], "one-task")



    def test_an_exception_written_for_a_role_the_policy_does_not_declare_is_still_honoured(self):
        """The bridge evaluates the exception before it looks the role up, so this creation
        succeeds there. Refusing it here would be one document read two ways."""
        import os
        from pathlib import Path

        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp), {
            "roles": {"parent": {"model": PARENT_MODEL, "reasoningEffort": PARENT_EFFORT}},
            "exceptions": {
                "for-a-child": {
                    "model": "gpt-6-astra",
                    "reasoningEffort": "high",
                    "cwd": [str(Path(self.tmp).resolve())],
                    "role": "child",
                }
            },
        })
        settings = task_settings(
            str(Path(self.tmp).resolve()), model="gpt-6-astra", reasoningEffort="high",
            citedException="for-a-child",
        )
        policy = rolepolicy.declared()
        self.assertIsNone(rolepolicy.check_record(settings, "child", policy))
        self.assertIsNone(
            rolepolicy.check_binding("child", "child", settings, policy)
        )
        # An undeclared role with no exception behind it still refuses.
        plain = task_settings(str(Path(self.tmp).resolve()))
        self.assertEqual(
            rolepolicy.check_record(plain, "child", policy)["code"],
            RefusalReason.ROLE_POLICY_UNCONFIGURED.value,
        )



class TakingOwnershipAwayIsNeverBlockedByThePolicy(DeliveryTestCase):
    """Archiving and cancelling are how a wrong owner gets removed, so refusing them is backwards.

    Running the reactivation check on every status write left a relationship and its binding both
    live with no way to close or repair them: a check meant to prevent a wrong owner became one
    that prevented removing it.
    """

    def setUp(self):
        super().setUp()
        import os
        from pathlib import Path

        self._previous = os.environ.get(rolepolicy.ENVIRONMENT_VARIABLE)
        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp))
        self.addCleanup(self._restore_environment)

    def _restore_environment(self):
        import os

        if self._previous is None:
            os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
        else:
            os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = self._previous
        rolepolicy.reset()

    def test_a_relationship_whose_child_record_disagrees_can_still_be_archived(self):
        from codex_session_relay.models import Endpoint

        self.registry.linkage.bind_scope(
            role="parent", scope_key="PROJ-1",
            endpoint=Endpoint(PARENT, "host-a", cwd="/parent", cxc_session="cxc-parent"),
        )
        # settings=None so the fixture records none; the child's record is then staged raw, the
        # way one written before this policy existed would look.
        relationship = self.register(project_key="PROJ-1", settings=None)
        self.store.db.execute(
            "INSERT INTO authorized_settings (task_id, settings, source, recorded_at)"
            " VALUES (?,?,?,?)",
            (CHILD, json.dumps(task_settings("/child", citedRole="supervisor")),
                "test-raw", self.clock.iso()),
        )
        self.registry.set_status(relationship["relationshipId"], "archived", actor="test")
        row = self.store.one(
            "SELECT status FROM relationships WHERE relationship_id = ?",
            (relationship["relationshipId"],),
        )
        self.assertEqual(row["status"], "archived")
