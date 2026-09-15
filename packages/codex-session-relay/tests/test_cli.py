"""The command surface, driven end to end with no host and no socket."""

import json
import os
import subprocess
import sys
import unittest

from .support import CHILD, DISPATCH_TURN, HOST, ISSUE, PARENT, RelayTestCase

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


class CliBase(RelayTestCase):
    def run_cli(self, *args, expect=0):
        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", self.tmp, *args],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(
            completed.returncode, expect,
            f"exit {completed.returncode}: {completed.stdout}{completed.stderr}",
        )
        return json.loads(completed.stdout)

    def register(self, **_kwargs):
        return self.run_cli(
            "register", "--parent-task", PARENT, "--parent-host", HOST,
            "--child-task", CHILD, "--child-host", HOST, "--issue", ISSUE,
            "--artifact-root", self.root, "--allowed-recipient", PARENT,
            "--dispatch-request-id", "dispatch-1", "--dispatch-turn-id", DISPATCH_TURN,
        )


class CommandLine(CliBase):
    def test_register_emit_and_status_round_trip(self):
        relationship = self.register()
        self.assertTrue(relationship["relationshipId"].startswith("rel-"))
        self.assertEqual(relationship["authorizedScope"]["artifactRoots"], [self.root])

        path = self.artifact("out.txt", "the deliverable")
        emitted = self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", DISPATCH_TURN, "--turn-status", "completed", "--artifact", path,
        )
        # With no host to confirm the turn ended, the claim is staged rather than queued.
        self.assertEqual(emitted["stage"], "staged")
        self.assertEqual(emitted["receipt"]["outcome"], "ready_for_review")
        self.assertEqual(emitted["terminalProof"], "unverified_staged")
        self.assertEqual(self.run_cli("status")["deliveries"], [])

    def test_a_refusal_exits_two_with_a_machine_readable_reason(self):
        relationship = self.register()
        outside = os.path.join(self.tmp, "outside.txt")
        with open(outside, "w", encoding="utf-8") as handle:
            handle.write("not yours")
        refused = self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", DISPATCH_TURN, "--turn-status", "completed", "--artifact", outside,
            expect=2,
        )
        self.assertEqual(refused["error"], "refused")
        self.assertEqual(refused["reason"], "scope_escape")

    def test_emitting_from_a_live_turn_stages_without_queueing(self):
        relationship = self.register()
        path = self.artifact("out.txt", "still working")
        emitted = self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", DISPATCH_TURN, "--turn-status", "inProgress", "--artifact", path,
        )
        self.assertEqual(emitted["stage"], "staged")
        self.assertNotIn("delivery", emitted)

    def test_a_later_turn_needs_a_continuation_and_the_emit_carries_it(self):
        relationship = self.register()
        path = self.artifact("out.txt", "finished later")
        refused = self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", "turn-loop-5", "--turn-status", "completed", "--artifact", path,
            expect=2,
        )
        self.assertEqual(refused["reason"], "unassigned_turn")

        accepted = self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", "turn-loop-5", "--turn-status", "completed", "--artifact", path,
            "--continues-anchor", DISPATCH_TURN, "--continuation-actor", "child-loop",
            "--continuation-reason", "cycle 5 of this execution",
        )
        self.assertEqual(accepted["receipt"]["outcome"], "ready_for_review")
        self.assertEqual(accepted["stage"], "staged")

    def test_ack_proof_is_computed_by_the_caller_not_the_relay(self):
        import hashlib

        event = "a" * 32
        proof = self.run_cli("ack-proof", "--event", event, "--turn", "parent-turn-9")
        self.assertEqual(
            proof["ackProof"],
            hashlib.sha256(f"{event}|parent-turn-9".encode()).hexdigest(),
        )

    def test_the_ack_command_requires_a_supplied_proof(self):
        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", self.tmp,
             "ack", "--event", "a" * 32, "--ack-turn", "t"],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("--ack-proof", completed.stderr)

    def test_a_command_needing_the_host_says_so_rather_than_guessing(self):
        result = self.run_cli("deliver", expect=4)
        self.assertEqual(result["error"], "usage")
        self.assertIn("--socket", result["detail"])

    def test_doctor_reports_the_environment(self):
        report = self.run_cli("doctor")
        self.assertTrue(report["procAvailable"])
        self.assertEqual(report["adapter"], "none (read-only, no --socket)")

    def test_pause_refuses_and_resume_requires_the_restated_scope(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.run_cli("relationship-status", "--relationship", rid, "--status", "paused",
                     "--actor", "user")
        refused = self.run_cli(
            "relationship-resume", "--relationship", rid, "--expect-generation", "9",
            "--expect-artifact-root", self.root, "--expect-allowed-recipient", PARENT,
            "--actor", "user", expect=2,
        )
        self.assertEqual(refused["reason"], "relationship_not_active")
        resumed = self.run_cli(
            "relationship-resume", "--relationship", rid, "--expect-generation", "1",
            "--expect-artifact-root", self.root, "--expect-allowed-recipient", PARENT,
            "--actor", "user",
        )
        self.assertEqual(resumed["status"], "active")


if __name__ == "__main__":
    unittest.main()


class TerminalProof(CliBase):
    """A caller cannot manufacture the terminal proof that makes a receipt deliverable."""

    def test_an_offline_readiness_claim_is_staged_not_finalized(self):
        relationship = self.register()
        path = self.artifact("out.txt", "claimed complete with nobody to check")
        emitted = self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", DISPATCH_TURN, "--turn-status", "completed", "--artifact", path,
        )
        self.assertEqual(emitted["terminalProof"], "unverified_staged")
        self.assertEqual(emitted["observedTurnStatus"], "inProgress")
        self.assertEqual(emitted["stage"], "staged")
        self.assertNotIn("delivery", emitted, "nothing is queued on an unverified claim")

    def test_a_staged_claim_is_visible_and_not_deliverable(self):
        relationship = self.register()
        path = self.artifact("out.txt", "work")
        self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", DISPATCH_TURN, "--turn-status", "completed", "--artifact", path,
        )
        self.assertEqual(self.run_cli("status")["deliveries"], [])


class SettingsCommands(CliBase):
    """The registration interface JUN-92 populates from Run's creation result."""

    def _settings(self, cwd="/parent"):
        return {
            "sandbox": {"type": "workspaceWrite", "writableRoots": [], "networkAccess": False,
                        "excludeTmpdirEnvVar": False, "excludeSlashTmp": False},
            "approvalPolicy": "never",
            "cwd": cwd,
            "runtimeWorkspaceRoots": [cwd],
            "model": "anthropic/claude-opus-5",
            "reasoningEffort": "xhigh",
            "environments": [{"environmentId": "local", "cwd": cwd,
                              "runtimeWorkspaceRoots": [cwd]}],
        }

    def test_register_records_settings_for_both_endpoints(self):
        payload = self.run_cli(
            "register", "--parent-task", PARENT, "--parent-host", HOST,
            "--child-task", CHILD, "--child-host", HOST, "--issue", ISSUE,
            "--artifact-root", self.root, "--allowed-recipient", PARENT,
            "--dispatch-request-id", "dispatch-1", "--dispatch-turn-id", DISPATCH_TURN,
            "--parent-settings", json.dumps(self._settings()),
            "--child-settings", json.dumps(self._settings(self.root)),
        )
        self.assertEqual(payload["authorizedSettings"], {PARENT: "recorded", CHILD: "recorded"})
        shown = self.run_cli("settings-show", "--task", PARENT)
        self.assertTrue(shown["usable"])
        self.assertEqual(shown["missing"], [])
        self.assertEqual(shown["settings"]["reasoningEffort"], "xhigh")

    def test_registering_without_settings_leaves_them_unrecorded(self):
        payload = self.register()
        self.assertIsNone(payload["authorizedSettings"])
        shown = self.run_cli("settings-show", "--task", PARENT)
        self.assertIsNone(shown["settings"])
        self.assertFalse(shown["usable"])
        self.assertIn("environments", shown["missing"])

    def test_settings_record_accepts_a_file_path(self):
        path = os.path.join(self.tmp, "settings.json")
        with open(path, "w", encoding="utf-8") as handle:
            json.dump(self._settings(), handle)
        recorded = self.run_cli("settings-record", "--task", PARENT, "--settings", f"@{path}")
        self.assertEqual(recorded["taskId"], PARENT)
        self.assertEqual(recorded["source"], "creation_result")
        self.assertTrue(self.run_cli("settings-show", "--task", PARENT)["usable"])

    def test_an_incomplete_record_is_refused_with_a_machine_readable_reason(self):
        broken = self._settings()
        del broken["environments"]
        refused = self.run_cli(
            "settings-record", "--task", PARENT, "--settings", json.dumps(broken), expect=2,
        )
        self.assertEqual(refused["reason"], "settings_incomplete")
        self.assertIn("environments", refused["detail"])
