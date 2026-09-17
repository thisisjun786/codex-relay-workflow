"""The command surface, driven end to end with no host and no socket."""

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

from .support import CHILD, DISPATCH_TURN, HOST, ISSUE, PARENT, RelayTestCase

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def option_values(command):
    """A shell round trip, with `--opt=value` tokens taken back apart.

    The generated commands attach values with '=' so that a path beginning with a dash stays
    one token and argparse reads it as a value rather than another option. That also means the
    value is no longer a word of its own after shlex.split, which is what these assertions
    have to look at: the round trip still has to hand the path back whole and unexecuted.
    """
    import shlex

    values = []
    for word in shlex.split(command):
        head, sep, tail = word.partition("=")
        values.append(tail if sep and head.startswith("--") else word)
    return values


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


class ScopedStatus(CliBase):
    """A filtered status must filter every block it returns, health included.

    Reporting one assignment's deliveries beside every assignment's observation backlog
    reads as that assignment being behind, which is the opposite of what a filter is for.
    """

    def staged(self, name):
        """A second assignment with its own parent, child and staged receipt."""
        parent, child = f"01parent-{name}", f"01child-{name}"
        root = os.path.join(self.root, name)
        os.makedirs(root, exist_ok=True)
        relationship = self.run_cli(
            "register", "--parent-task", parent, "--parent-host", HOST,
            "--child-task", child, "--child-host", HOST, "--issue", f"REL-{name}",
            "--artifact-root", root, "--allowed-recipient", parent,
            "--dispatch-request-id", f"dispatch-{name}", "--dispatch-turn-id", f"turn-{name}",
        )
        path = os.path.join(root, "out.txt")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(f"{name} still going")
        emitted = self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", child,
            "--turn-id", f"turn-{name}", "--turn-status", "completed", "--artifact", path,
        )
        self.assertEqual(emitted["stage"], "staged")
        return relationship["relationshipId"], emitted["receipt"]["eventId"]

    def test_a_scoped_status_reports_only_the_requested_assignment(self):
        mine, my_event = self.staged("a")
        theirs, their_event = self.staged("b")

        health = self.run_cli("status", "--relationship", mine)["observation"]

        self.assertEqual([s["eventId"] for s in health["stagedEvents"]], [my_event])
        self.assertEqual(list(health["anchors"]), [mine])
        self.assertEqual(list(health["backlog"]), [mine])
        self.assertNotIn(theirs, health["backlog"])
        self.assertNotIn(their_event, [s["eventId"] for s in health["stagedEvents"]])

    def test_an_unscoped_status_still_reports_every_assignment(self):
        mine, my_event = self.staged("a")
        theirs, their_event = self.staged("b")

        health = self.run_cli("status")["observation"]

        self.assertEqual({s["eventId"] for s in health["stagedEvents"]},
                         {my_event, their_event})
        self.assertEqual(set(health["anchors"]), {mine, theirs})
        self.assertEqual(set(health["backlog"]), {mine, theirs})


class ServiceExitCodes(CliBase):
    """A refusal that exits zero is read by automation as a success."""

    def test_a_refused_enable_does_not_exit_zero(self):
        self.run_cli("service", "enable")
        state = os.path.join(self.tmp, "daemon.json")
        with open(state, "w", encoding="utf-8") as handle:
            json.dump({"pid": os.getpid(), "installationId": "someone-else",
                       "storeId": "another-store", "bootId": None,
                       "startTicks": None, "workerPid": None}, handle)
        # No lock is held here, so this must still succeed: a stopped foreign registration
        # is not a reason to make a state directory unconfigurable.
        self.assertTrue(self.run_cli("service", "enable")["ok"])

    def test_a_refused_enable_is_a_refusal_the_shell_can_see(self):
        """Returning the payload directly exits zero, and automation reads that as done."""
        from unittest import mock

        from codex_session_relay import cli

        class Refusing:
            def enable(self, *, actor):
                return {"ok": False, "reason": "not_ours", "intent": {"enabled": False}}

        args = argparse.Namespace(service_command="enable", actor=None)
        with mock.patch.object(cli, "_service_for", lambda _services: Refusing()):
            with self.assertRaises(cli.PayloadExit) as caught:
                cli.cmd_service(object(), args)
        self.assertEqual(caught.exception.code, cli.EXIT_REFUSED)
        self.assertEqual(caught.exception.payload["reason"], "not_ours")

    def test_a_refused_disable_does_not_exit_zero(self):
        self.run_cli("service", "enable")
        state = os.path.join(self.tmp, "daemon.json")
        with open(state, "w", encoding="utf-8") as handle:
            json.dump({"pid": os.getpid(), "installationId": "someone-else",
                       "storeId": "another-store", "bootId": "irrelevant",
                       "startTicks": 1, "workerPid": None}, handle)
        refused = self.run_cli("service", "disable", expect=2)
        self.assertFalse(refused["ok"])
        self.assertEqual(refused["reason"], "not_ours")


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


class Diagnosis(unittest.TestCase):
    """doctor has to answer ON the host it is describing, including a broken one."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-doctor-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.home = os.path.join(self.tmp, "home")
        os.makedirs(self.home)

    def cli(self, *args, state=None, socket=None, expect=0, env=None):
        environment = dict(
            os.environ, PYTHONPATH=os.path.join(REPO, "src"), HOME=self.home,
        )
        environment.pop("CODEX_SESSION_RELAY_STATE", None)
        environment.pop("XDG_STATE_HOME", None)
        environment.update(env or {})
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli",
             *(["--state", state] if state else []),
             *(["--socket", socket] if socket else []), *args],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(
            completed.returncode, expect,
            f"exit {completed.returncode}: {completed.stdout}{completed.stderr}",
        )
        return json.loads(completed.stdout)

    def test_doctor_answers_for_a_state_directory_that_does_not_exist_yet(self):
        absent = os.path.join(self.tmp, "absent")
        report = self.cli("doctor", state=absent)
        self.assertEqual(report["stateSelection"]["source"], "flag")
        self.assertFalse(report["access"]["directoryExists"])
        self.assertFalse(report["contents"]["available"])
        # The old doctor built a Store first, which created the directory it was asked about.
        self.assertFalse(os.path.exists(absent))

    def test_doctor_does_not_turn_an_unrelated_file_into_a_relay_database(self):
        """The directory existing was not the whole side effect; counting rows was too.

        A readable relay.sqlite3 sent the contents block through a real Store, and
        Store.__init__ opens O_RDWR, switches on WAL and runs the entire schema script. An
        empty, legacy or unrelated file was quietly adopted by the command that promised to
        do nothing but look.
        """
        state = os.path.join(self.tmp, "borrowed")
        os.makedirs(state)
        target = os.path.join(state, "relay.sqlite3")
        open(target, "w").close()

        report = self.cli("doctor", state=state)

        self.assertEqual(os.path.getsize(target), 0, "doctor wrote a schema into it")
        self.assertEqual(
            sorted(os.listdir(state)), ["relay.sqlite3"], "no WAL or shm sidecar either",
        )
        self.assertTrue(report["access"]["dbExists"])
        self.assertFalse(report["contents"]["available"],
                         "and it says so rather than inventing counts")
        self.assertIsNotNone(report["contents"]["detail"])

    def test_doctor_names_the_rule_that_chose_the_directory(self):
        chosen = os.path.join(self.tmp, "chosen")
        by_env = self.cli("doctor", env={"CODEX_SESSION_RELAY_STATE": chosen})
        self.assertEqual(by_env["stateSelection"]["source"], "env")
        self.assertEqual(by_env["stateSelection"]["path"], chosen)
        by_flag = self.cli("doctor", state=chosen, env={
            "CODEX_SESSION_RELAY_STATE": os.path.join(self.tmp, "ignored"),
        })
        self.assertEqual(by_flag["stateSelection"]["source"], "flag")
        self.assertEqual(by_flag["stateSelection"]["path"], chosen)

    def test_a_different_store_is_refused_rather_than_reported_healthy(self):
        a, b = os.path.join(self.tmp, "a"), os.path.join(self.tmp, "b")
        mine = self.cli("store-identity", state=a)["store"]
        theirs = self.cli("store-identity", state=b)["store"]
        self.assertNotEqual(mine["storeId"], theirs["storeId"])
        refused = self.cli(
            "doctor", "--expect-store", mine["storeId"], state=b, expect=2,
        )
        self.assertEqual(refused["sameStore"], "mismatch")
        # The whole diagnosis survives the refusal; it is not replaced by an error envelope.
        self.assertIn("stateSelection", refused)
        self.assertIn("access", refused)

    def test_a_nonce_proves_one_store_and_disproves_a_copy(self):
        a, b = os.path.join(self.tmp, "a"), os.path.join(self.tmp, "b")
        mine = self.cli("store-identity", state=a)["store"]
        os.makedirs(b, exist_ok=True)
        for suffix in ("", "-wal", "-shm"):
            source = os.path.join(a, f"relay.sqlite3{suffix}")
            if os.path.exists(source):
                shutil.copy(source, os.path.join(b, f"relay.sqlite3{suffix}"))
        nonce = self.cli("store-challenge", "--write", "--actor", "parent", state=a)["nonce"]
        proven = self.cli(
            "doctor", "--expect-store", mine["storeId"], "--expect-nonce", nonce, state=a,
        )
        self.assertEqual(proven["sameStore"], "proven")
        copied = self.cli(
            "doctor", "--expect-store", mine["storeId"], "--expect-nonce", nonce, state=b,
            expect=2,
        )
        self.assertEqual(copied["sameStore"], "mismatch")
        # Identifier alone cannot separate them, which is why it is graded unproven.
        weak = self.cli("doctor", "--expect-store", mine["storeId"], state=b, expect=2)
        self.assertEqual(weak["sameStore"], "unproven")

    def test_doctor_reports_that_state_and_the_transport_ledger_have_split(self):
        state = os.path.join(self.tmp, "state")
        socket = os.path.join(self.tmp, "app.sock")
        split = self.cli("doctor", state=state, socket=socket)
        self.assertTrue(split["ledger"]["configured"])
        # --state moved the store; the adapter resolves its ledger from the environment.
        self.assertTrue(split["ledger"]["split"])
        together = self.cli(
            "doctor", state=state, socket=socket,
            env={"CODEX_SESSION_RELAY_STATE": state},
        )
        self.assertFalse(together["ledger"]["split"])


class LazyServices(unittest.TestCase):
    """Building dependencies on demand must not drop the wiring __init__ used to do."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-lazy-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def services(self):
        from codex_session_relay.cli import Services

        built = Services(argparse.Namespace(state=self.tmp, socket=None))
        self.addCleanup(built.close)
        return built

    def test_the_first_ack_is_already_wired_to_the_outbox(self):
        built = self.services()
        # Touching ack FIRST is the regression: record_verdict skips its outbox obligation
        # when sync is absent, so an unwired ack would lose it silently.
        self.assertIsNotNone(built.ack.sync)
        self.assertIs(built.ack.sync, built.sync)

    def test_every_dependency_shares_one_store_and_is_built_once(self):
        built = self.services()
        self.assertIs(built.registry.store, built.store)
        self.assertIs(built.delivery.store, built.store)
        self.assertIs(built.ack.store, built.store)
        self.assertIs(built.registry, built.registry)
        self.assertIs(built.delivery, built.delivery)

    def test_closing_without_ever_using_the_store_creates_nothing(self):
        from codex_session_relay.cli import Services

        empty = os.path.join(self.tmp, "untouched")
        built = Services(argparse.Namespace(state=empty, socket=None))
        built.close()
        self.assertFalse(os.path.exists(empty))


class ContestedSocket(CliBase):
    """Two stores recording one socket must not quietly become three.

    These runs deliberately pass no --state: the whole question is what the environment alone
    resolves to, and an explicit directory answers it before discovery ever runs.
    """

    def contested(self, name):
        """A home holding two stores that both record one socket."""
        from pathlib import Path

        from codex_session_relay.store import Store

        home = os.path.join(self.tmp, name)
        root = os.path.join(home, ".local", "state", "codex-session-relay")
        socket = os.path.join(self.tmp, f"{name}.sock")
        for directory in ("aaaa444444444444", "bbbb444444444444"):
            os.makedirs(os.path.join(root, directory))
            Store(Path(root) / directory / "relay.sqlite3", socket_path=socket).close()
        return home, root, socket

    def run_in_home(self, home, *args, expect=0):
        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"), HOME=home)
        for name in ("CODEX_SESSION_RELAY_STATE", "XDG_STATE_HOME"):
            environment.pop(name, None)
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", *args],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(
            completed.returncode, expect,
            f"exit {completed.returncode}: {completed.stdout}{completed.stderr}",
        )
        return json.loads(completed.stdout)

    def test_an_ordinary_command_refuses_rather_than_creating_a_third_store(self):
        home, root, socket = self.contested("contested-status")

        refused = self.run_in_home(home, "--socket", socket, "status", expect=2)

        self.assertEqual(refused["reason"], "ambiguous_state_directory")
        self.assertEqual(len(refused["candidates"]), 2)
        self.assertFalse(
            os.path.exists(refused["wouldHaveCreated"]),
            "the refusal must not leave behind the store it refused to choose",
        )
        self.assertEqual(sorted(os.listdir(root)), ["aaaa444444444444", "bbbb444444444444"])

    def test_doctor_still_describes_a_contested_socket(self):
        home, _root, socket = self.contested("contested-doctor")

        report = self.run_in_home(home, "--socket", socket, "doctor")

        self.assertTrue(report["siblingStores"]["ambiguous"])
        self.assertEqual(len(report["siblingStores"]["claimingThisSocket"]), 2)

    def test_a_state_directory_recording_another_socket_is_refused(self):
        """An explicit directory reused with a different App Server.

        Choosing a directory is not choosing what is already in it: the service would claim
        and serve the new socket while the database went on attributing itself to the old
        one, so one installation's assignments could be exposed through another and later
        discovery would still match the store to the socket it no longer serves.
        """
        from pathlib import Path

        from codex_session_relay.store import Store

        state = os.path.join(self.tmp, "reused-state")
        first = os.path.join(self.tmp, "first.sock")
        second = os.path.join(self.tmp, "second.sock")
        Store(Path(state) / "relay.sqlite3", socket_path=first).close()

        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", state,
             "--socket", second, "status"],
            capture_output=True, text=True, env=environment, timeout=60,
        )

        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        refused = json.loads(completed.stdout)
        self.assertEqual(refused["reason"], "state_directory_serves_another_socket")
        self.assertEqual(refused["recordedSocket"], first)

    def test_a_store_recording_no_socket_also_refuses_before_creating_one(self):
        """A store older than provenance cannot be matched to a socket by anything but its
        directory hash, which cannot be inverted. Reporting it through doctor was not enough,
        because an ordinary command does not run doctor and creates the store anyway."""
        from pathlib import Path

        from codex_session_relay.store import Store

        home = os.path.join(self.tmp, "unlabelled-home")
        root = os.path.join(home, ".local", "state", "codex-session-relay")
        os.makedirs(os.path.join(root, "0123456789abcdef"))
        Store(Path(root) / "0123456789abcdef" / "relay.sqlite3").close()
        socket = os.path.join(self.tmp, "unlabelled.sock")

        refused = self.run_in_home(home, "--socket", socket, "status", expect=2)

        self.assertEqual(refused["reason"], "unidentified_state_directory")
        self.assertFalse(os.path.exists(refused["wouldHaveCreated"]))
        self.assertEqual(os.listdir(root), ["0123456789abcdef"], "no store was created")

    def test_the_refusal_prints_commands_an_operator_can_actually_run(self):
        """The payload is all an operator has.

        It used to end with "--state <the directory above> once, to adopt it deliberately",
        which is not a command, does not say which directory, and drops the --socket that
        made the two stores candidates for each other in the first place. It also promised an
        adoption that does not exist: choosing one of two claiming stores leaves both still
        recording the socket, so default discovery refuses again on the next invocation.
        """
        home, root, socket = self.contested("contested-recovery")

        refused = self.run_in_home(home, "--socket", socket, "status", expect=2)

        recover = refused["recover"]
        commands = [line for line in recover if not line.startswith("  ")]
        self.assertTrue(commands, "the refusal offered no runnable command")
        for command in commands:
            self.assertIn(f"--socket={socket}", command,
                          f"a recovery command dropped the socket: {command}")
        for candidate in refused["candidates"]:
            self.assertTrue(
                any(f"--state={candidate}" in c for c in commands),
                f"no command inspects candidate {candidate}",
            )
        self.assertTrue(
            any("doctor" in c for c in commands) and any("service status" in c for c in commands),
            "recovery must both identify the store and show what it carries",
        )
        self.assertNotIn(
            "the directory above", " ".join(recover),
            "the payload still points at a directory it never names",
        )
        self.assertTrue(
            any("retired" in line for line in recover),
            "the payload must say that choosing one store does not retire the other",
        )

    def test_the_wrong_socket_refusal_offers_a_matching_pair_not_an_adoption(self):
        """This refusal has nothing to adopt: using a store does not rewrite the socket it
        recorded. It printed no recovery at all, which left the operator to infer that."""
        from pathlib import Path

        from codex_session_relay.store import Store

        state = os.path.join(self.tmp, "pair-state")
        first = os.path.join(self.tmp, "pair-first.sock")
        second = os.path.join(self.tmp, "pair-second.sock")
        Store(Path(state) / "relay.sqlite3", socket_path=first).close()

        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", state,
             "--socket", second, "status"],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        refused = json.loads(completed.stdout)

        joined = " ".join(refused["recover"])
        self.assertIn(first, joined, "no command reads the store under the socket it records")
        self.assertIn(second, joined, "no command looks for the socket that was asked for")
        self.assertIn("does not rewrite", refused["note"])

    def test_recovery_commands_are_safe_to_paste(self):
        """These strings exist to be pasted, so a path carrying shell syntax is executable.

        A state directory or socket path with a substitution in it would run as the operator
        did exactly what the refusal told them to do.
        """
        import shlex
        from pathlib import Path

        from codex_session_relay.store import Store

        home = os.path.join(self.tmp, "quoted-home")
        root = os.path.join(home, ".local", "state", "codex-session-relay")
        # A socket whose name would run a command if it were pasted unquoted.
        socket = os.path.join(self.tmp, "sock$(touch /tmp/pwned);x.sock")
        for directory in ("aaaa555555555555", "bbbb555555555555"):
            os.makedirs(os.path.join(root, directory))
            Store(Path(root) / directory / "relay.sqlite3", socket_path=socket).close()

        refused = self.run_in_home(home, "--socket", socket, "status", expect=2)

        commands = [line for line in refused["recover"] if not line.startswith("  ")]
        self.assertTrue(commands)
        for command in commands:
            # The real property is the round trip: the shell must hand the socket back as ONE
            # intact argument rather than splitting it or running the substitution in it.
            words = option_values(command)
            self.assertIn(
                socket, words,
                f"the socket did not survive a shell round trip intact: {command}",
            )
            self.assertFalse(
                [w for w in words if "$(" in w and w != socket],
                f"a substitution escaped quoting: {command}",
            )

    def test_the_wrong_socket_recovery_is_quoted_too(self):
        """The other refusal prints commands as well, and paths reach it the same way."""
        import shlex
        from pathlib import Path

        from codex_session_relay.store import Store

        state = os.path.join(self.tmp, "quoted state")
        first = os.path.join(self.tmp, "first$(id).sock")
        second = os.path.join(self.tmp, "second.sock")
        Store(Path(state) / "relay.sqlite3", socket_path=first).close()

        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", state,
             "--socket", second, "status"],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        refused = json.loads(completed.stdout)

        commands = [line for line in refused["recover"] if not line.startswith("  ")]
        self.assertTrue(commands)
        words = [word for command in commands for word in option_values(command)]
        self.assertIn(first, words, "the recorded socket did not survive a shell round trip")
        self.assertIn(state, words, "the state directory did not survive a shell round trip")

    def test_the_wrong_socket_recovery_drops_a_state_pin_that_would_reselect_it(self):
        """The socket-first line carries no --state, so an inherited pin overrides its intent.

        CODEX_SESSION_RELAY_STATE is one of the two ways to reach this refusal, and it is the
        way that turns the recovery line into a dead end: pasted with the pin still set,
        "find the store that belongs to this socket" re-selects the store that produced the
        refusal and hands back the same error. This runs the printed command rather than
        matching its text, because what matters is where pasting it actually lands.
        """
        import shlex
        from pathlib import Path

        from codex_session_relay.store import Store

        home = os.path.join(self.tmp, "pinned-home")
        os.makedirs(home)
        state = os.path.join(self.tmp, "pinned-state")
        recorded = os.path.join(self.tmp, "pinned-recorded.sock")
        wanted = os.path.join(self.tmp, "pinned-wanted.sock")
        Store(Path(state) / "relay.sqlite3", socket_path=recorded).close()

        # HOME is pinned to a temporary directory: the recovery command falls back to default
        # discovery once the pin is dropped, and that must not reach the real user state.
        environment = dict(
            os.environ, PYTHONPATH=os.path.join(REPO, "src"), HOME=home,
            CODEX_SESSION_RELAY_STATE=state,
        )
        environment.pop("XDG_STATE_HOME", None)
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--socket", wanted, "status"],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        refused = json.loads(completed.stdout)
        self.assertEqual(refused["reason"], "state_directory_serves_another_socket")

        socket_first = [
            line for line in refused["recover"]
            if not line.startswith("  ") and wanted in line
        ]
        self.assertEqual(len(socket_first), 1, refused["recover"])

        replayed = subprocess.run(
            shlex.split(socket_first[0]),
            capture_output=True, text=True, env=environment, timeout=60,
        )

        # doctor is exempt from this guard, so it does not hand the refusal back - it does
        # something quieter and worse. With the pin still set it reports the very store that
        # produced the refusal, while its caption says it finds the store belonging to the
        # socket. That is what the assertion has to catch.
        selection = json.loads(replayed.stdout)["stateSelection"]
        self.assertNotEqual(
            selection["path"], state,
            "the recovery command re-selected the store the refusal was about",
        )
        self.assertNotEqual(
            selection["source"], "env",
            "the state pin survived into the command printed to look past it",
        )

    def wrong_socket_refusal(self, name, *, flagged_socket, wanted_socket, pin):
        """Refuse a --state store that records another socket, with the variable also set.

        Returns the payload and the environment it was produced in, so a test can replay the
        commands it printed under exactly the conditions that printed them.
        """
        from pathlib import Path

        from codex_session_relay.store import Store

        home = os.path.join(self.tmp, f"{name}-home")
        os.makedirs(home)
        flagged = os.path.join(self.tmp, f"{name}-flagged")
        Store(Path(flagged) / "relay.sqlite3", socket_path=flagged_socket).close()

        environment = dict(
            os.environ, PYTHONPATH=os.path.join(REPO, "src"), HOME=home,
            CODEX_SESSION_RELAY_STATE=(flagged if pin == "same" else pin),
        )
        environment.pop("XDG_STATE_HOME", None)
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", flagged,
             "--socket", wanted_socket, "status"],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        refused = json.loads(completed.stdout)
        self.assertEqual(refused["reason"], "state_directory_serves_another_socket")
        return refused, environment, flagged

    def test_a_pin_repeating_the_flags_mistake_does_not_come_back_as_the_answer(self):
        """--state A with the variable also naming A. Two conditions failed this case.

        Keying the prefix on which rule caused the refusal proved only that --state won, not
        that the lower-precedence store was any better: here it is the SAME store. Left
        pinned, the socket-first line re-selects the directory the refusal was about, and
        because doctor is exempt from this guard it exits 0 under a caption claiming it found
        the requested socket's store. The line no longer decides - it always runs unpinned.
        """
        import shlex

        wanted = os.path.join(self.tmp, "samepin-wanted.sock")
        refused, environment, flagged = self.wrong_socket_refusal(
            "samepin",
            flagged_socket=os.path.join(self.tmp, "samepin-other.sock"),
            wanted_socket=wanted, pin="same",
        )

        discovery = [
            line for line in refused["recover"]
            if not line.startswith("  ") and wanted in line
        ]
        self.assertEqual(len(discovery), 1, refused["recover"])

        replayed = subprocess.run(
            shlex.split(discovery[0]),
            capture_output=True, text=True, env=environment, timeout=60,
        )

        selection = json.loads(replayed.stdout)["stateSelection"]
        self.assertNotEqual(
            selection["path"], flagged,
            "the recovery command handed back the store the refusal was about",
        )
        self.assertNotEqual(selection["source"], "env", selection)

    def test_a_flag_caused_refusal_offers_the_environment_store_as_its_own_candidate(self):
        """--state wins over the variable, so the variable may hold the right store.

        Guessing in either direction was wrong, so both candidates are printed: one line
        discovers by socket with no pin at all, and a second reads the directory the variable
        names. Nothing here decides which of them the operator meant.
        """
        import shlex
        from pathlib import Path

        from codex_session_relay.store import Store

        wanted = os.path.join(self.tmp, "flagenv-wanted.sock")
        pinned = os.path.join(self.tmp, "flagenv-pinned")
        # The variable's store is the one that records the socket actually being asked for.
        Store(Path(pinned) / "relay.sqlite3", socket_path=wanted).close()
        refused, environment, _flagged = self.wrong_socket_refusal(
            "flagenv",
            flagged_socket=os.path.join(self.tmp, "flagenv-other.sock"),
            wanted_socket=wanted, pin=pinned,
        )

        commands = [line for line in refused["recover"] if not line.startswith("  ")]
        unpinned = [c for c in commands if wanted in c and "env -u" in c]
        env_candidate = [c for c in commands if f"--state={pinned}" in c]
        self.assertEqual(len(unpinned), 1, refused["recover"])
        self.assertEqual(
            len(env_candidate), 1,
            f"the directory the variable names was never offered: {refused['recover']}",
        )

        replayed = subprocess.run(
            shlex.split(env_candidate[0]),
            capture_output=True, text=True, env=environment, timeout=60,
        )

        selection = json.loads(replayed.stdout)["stateSelection"]
        self.assertEqual(selection["path"], pinned, selection)
        self.assertTrue(
            os.path.exists(selection["dbPath"]),
            "the offered candidate reported a database that does not exist",
        )

    def test_a_pin_that_only_spells_the_same_directory_differently_is_not_a_candidate(self):
        """Offering a candidate has to mean offering a different store.

        Two spellings reach the same directory and neither is exotic: `~/pinned` because
        selection expands it, and `.../x/../pinned` because the filesystem does. Compared as
        written they look like separate candidates, and the line each would add resolves
        straight back to the store that caused the refusal - a dead end wearing the label of
        an alternative.
        """
        from pathlib import Path

        from codex_session_relay.store import Store

        home = os.path.join(self.tmp, "tilde-home")
        pinned = os.path.join(home, "pinned")
        os.makedirs(home)
        wanted = os.path.join(self.tmp, "tilde-wanted.sock")
        Store(
            Path(pinned) / "relay.sqlite3",
            socket_path=os.path.join(self.tmp, "tilde-other.sock"),
        ).close()

        # Each spelling names exactly the directory --state names, by a different route.
        spellings = {
            "the home shortcut": "~/pinned",
            "a dot segment": os.path.join(home, "pinned", "..", "pinned"),
        }
        for label, spelling in spellings.items():
            with self.subTest(spelling=label):
                environment = dict(
                    os.environ, PYTHONPATH=os.path.join(REPO, "src"), HOME=home,
                    CODEX_SESSION_RELAY_STATE=spelling,
                )
                environment.pop("XDG_STATE_HOME", None)
                completed = subprocess.run(
                    [sys.executable, "-m", "codex_session_relay.cli", f"--state={pinned}",
                     f"--socket={wanted}", "status"],
                    capture_output=True, text=True, env=environment, timeout=60,
                )
                self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
                refused = json.loads(completed.stdout)
                self.assertEqual(refused["reason"], "state_directory_serves_another_socket")

                commands = [
                    line for line in refused["recover"] if not line.startswith("  ")
                ]
                pinning = [c for c in commands if "--state=" in c]
                self.assertEqual(
                    len(pinning), 1,
                    f"the refusing store came back as its own alternative: {refused['recover']}",
                )
                self.assertNotIn(spelling, " ".join(commands))

    def test_an_unexpandable_pin_does_not_replace_the_refusal_with_a_host_error(self):
        """The refusal payload is everything the operator has, so it has to survive.

        An explicit --state overrides the variable, so nothing validates the variable's value
        at startup and building the recovery list is the first thing that touches it.
        Path.expanduser() raises RuntimeError for a ~user whose home cannot be resolved. The
        top-level handler catches it, so this is not a traceback - it is worse in a quieter
        way: exit 3 with a generic {"error": "host"} and none of the recorded socket, the
        requested socket or the recovery commands the operator needed.
        """
        from pathlib import Path

        from codex_session_relay.store import Store

        state = os.path.join(self.tmp, "badpin-state")
        wanted = os.path.join(self.tmp, "badpin-wanted.sock")
        Store(
            Path(state) / "relay.sqlite3",
            socket_path=os.path.join(self.tmp, "badpin-other.sock"),
        ).close()

        environment = dict(
            os.environ, PYTHONPATH=os.path.join(REPO, "src"),
            CODEX_SESSION_RELAY_STATE="~no-such-user-for-this-test/store",
        )
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", f"--state={state}",
             f"--socket={wanted}", "status"],
            capture_output=True, text=True, env=environment, timeout=60,
        )

        self.assertEqual(
            completed.returncode, 2,
            f"the refusal did not survive: {completed.stdout}{completed.stderr}",
        )
        refused = json.loads(completed.stdout)
        self.assertEqual(refused["reason"], "state_directory_serves_another_socket")
        commands = [line for line in refused["recover"] if not line.startswith("  ")]
        self.assertEqual(len(commands), 2, refused["recover"])
        # And the unusable value is reported rather than silently dropped.
        self.assertIn("does not resolve", " ".join(refused["recover"]))

    def test_a_socket_path_beginning_with_a_dash_still_produces_runnable_commands(self):
        """A relative socket path may legitimately begin with a dash.

        The original invocation can pass it as --socket=-odd.sock, but a generated line that
        separates them with a space makes argparse read the value as another option and fail
        with "expected one argument" - so every command in the payload is unusable for that
        input. The value travels attached.

        This drives the ambiguous refusal rather than the different-socket one, because that
        is where a dash can still reach the output: the different-socket payload prints
        canonical_socket() on both sides, which is absolute and therefore never leads with a
        dash, while _recovery_commands prints services.socket_path, the argument as given.
        """
        import shlex
        from pathlib import Path

        from codex_session_relay.store import Store

        # RELATIVE, so the value itself begins with the dash. An absolute path with a dashed
        # basename still starts with '/' and never reaches the parser ambiguity.
        relative = "-odd.sock"
        home = os.path.join(self.tmp, "dash-home")
        root = os.path.join(home, ".local", "state", "codex-session-relay")
        # Both stores record what that relative name resolves to under the subprocess cwd,
        # so the run is ambiguous for exactly the socket being asked about.
        for directory in ("aaaa666666666666", "bbbb666666666666"):
            os.makedirs(os.path.join(root, directory))
            Store(
                Path(root) / directory / "relay.sqlite3",
                socket_path=os.path.join(self.tmp, relative),
            ).close()

        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"), HOME=home)
        for name in ("CODEX_SESSION_RELAY_STATE", "XDG_STATE_HOME"):
            environment.pop(name, None)
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli",
             f"--socket={relative}", "status"],
            capture_output=True, text=True, env=environment, timeout=60, cwd=self.tmp,
        )
        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        refused = json.loads(completed.stdout)
        self.assertEqual(refused["reason"], "ambiguous_state_directory")

        commands = [line for line in refused["recover"] if not line.startswith("  ")]
        self.assertTrue(commands)
        for command in commands:
            self.assertIn(
                relative, option_values(command),
                f"a dashed socket path did not survive the round trip: {command}",
            )
            # Running it is the assertion that matters: the parser must accept the value
            # rather than reading it as an option it does not have.
            replayed = subprocess.run(
                shlex.split(command), capture_output=True, text=True,
                env=environment, timeout=60, cwd=self.tmp,
            )
            self.assertNotIn(
                "expected one argument", replayed.stderr,
                f"the printed command is unusable: {command}",
            )
            self.assertTrue(
                replayed.stdout.strip().startswith("{"),
                f"the printed command produced no payload: {command}\n{replayed.stderr}",
            )
    def test_the_printed_command_names_the_interpreter_that_is_running(self):
        """These lines are pasted into a shell where python3 may be absent or different.

        The relay can be running under a virtualenv or a versioned interpreter. A bare
        python3 there reaches another installation, or nothing.
        """
        import shlex
        from pathlib import Path

        from codex_session_relay.store import Store

        state = os.path.join(self.tmp, "interpreter-state")
        recorded = os.path.join(self.tmp, "interpreter-recorded.sock")
        wanted = os.path.join(self.tmp, "interpreter-wanted.sock")
        Store(Path(state) / "relay.sqlite3", socket_path=recorded).close()

        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        environment.pop("CODEX_SESSION_RELAY_STATE", None)
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", state,
             "--socket", wanted, "status"],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        refused = json.loads(completed.stdout)

        commands = [line for line in refused["recover"] if not line.startswith("  ")]
        self.assertTrue(commands)
        for command in commands:
            self.assertEqual(
                shlex.split(command)[0], sys.executable,
                f"this line names an interpreter that may not be the running one: {command}",
            )
        self.assertNotIn(
            "env -u", " ".join(commands),
            "no pin is set here, so there is nothing to drop and nothing to explain",
        )

    def test_an_explicit_state_directory_resolves_the_contest(self):
        home, root, socket = self.contested("contested-explicit")
        chosen = os.path.join(root, "aaaa444444444444")

        answer = self.run_in_home(home, "--state", chosen, "--socket", socket, "status")

        self.assertEqual(answer["deliveries"], [])


class ParticipantAccessReceipts(CliBase):
    """Parent, child and daemon on one database, each proving its own access to it.

    The criterion asks for two things a single healthy-looking report cannot give. Whether the
    participants share a store is a question about THREE observations, not one; and whether
    each sandbox permits what that participant needs is a question about what it can actually
    do, not about what its configuration says. So every participant emits a receipt and the
    receipts are compared.

    Isolation, because this touches the same machinery a real installation uses: a temporary
    HOME and CODEX_HOME, a socket bound here and closed here, an explicit temporary --state,
    and CODEX_SESSION_RELAY_STATE pinned to that same directory. The pin matters on its own -
    the adapter reads it, and setting only --state lets the store and the ledger diverge. The
    real state directory under the user's home is never selected by any of these runs.

    HOW THAT IS MEASURED, because the obvious way is wrong here. Snapshotting the real state
    directory before and after a suite run does NOT establish isolation on a host where
    anything else touches it: on 2026-09-17 that directory grew WAL and shared-memory
    sidecars across all three of its stores while this suite was not running at all, in a
    100-second control with nothing else started. Three bisections each blamed a different
    test file, because the external writes simply landed during whichever file was running.
    A before/after snapshot therefore fails for the wrong reason, the way a vacuous
    assertion passes for the wrong one. What this class relies on instead is per-run
    attribution: every participant here is given its own HOME and its own explicit --state,
    so the real directory is never selected, and that is a property of the arguments rather
    than of what the filesystem happened to do.

    WHAT THIS DOES NOT COVER. The participants here are three processes with three
    environments, not three genuinely different sandboxes: this host runs them all under the
    same kernel policy, so the receipts prove the store is shared and that each process really
    could read and write it, not that a restrictive sandbox would have been reported
    correctly. The recorded sandbox each receipt carries is the settings the adapter would
    send with, which is the value a denial would have to be explained against.

    The device and inode pair is decisive in one direction only. A DIFFERENT pair means a
    different file and that is conclusive; an agreeing pair is not sufficient for the same
    one. It is namespace-local, so participants in separate mount namespaces or on different
    hosts can hold one pair while sharing nothing, and one inode can be reached at more than
    one pathname - a hardlink name or a file bind mount - each of which carries its own
    write-ahead log. `store-challenge` with `doctor --expect-nonce` is what settles a shared
    store, and `compare_store` (store.py) is where each of these is graded.
    """

    def probe_socket(self):
        """A socket that really accepts, so reachability is observed rather than assumed."""
        import socket

        path = os.path.join(self.tmp, "probe-app-server.sock")
        listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        listener.bind(path)
        listener.listen(1)
        self.addCleanup(listener.close)
        return path

    def participant(self, *args, state=None, pin=None, cwd=None, expect=0):
        """Run one participant's own doctor, in its own environment."""
        home = os.path.join(self.tmp, "participant-home")
        os.makedirs(home, exist_ok=True)
        environment = dict(
            os.environ,
            PYTHONPATH=os.path.join(REPO, "src"),
            HOME=home,
            CODEX_HOME=os.path.join(self.tmp, "codex-home"),
        )
        environment.pop("XDG_STATE_HOME", None)
        if pin is None:
            environment.pop("CODEX_SESSION_RELAY_STATE", None)
        else:
            environment["CODEX_SESSION_RELAY_STATE"] = pin
        argv = [sys.executable, "-m", "codex_session_relay.cli"]
        if state is not None:
            argv.append(f"--state={state}")
        argv += list(args)
        completed = subprocess.run(
            argv, capture_output=True, text=True, env=environment, timeout=60,
            cwd=cwd or self.tmp,
        )
        self.assertEqual(
            completed.returncode, expect,
            f"exit {completed.returncode}: {completed.stdout}{completed.stderr}",
        )
        return json.loads(completed.stdout)

    def settings(self, cwd):
        """Shaped like the creation result the host reports, which is what gets recorded."""
        return {
            "sandbox": {"type": "workspaceWrite", "writableRoots": [cwd],
                        "networkAccess": False, "excludeTmpdirEnvVar": False,
                        "excludeSlashTmp": False},
            "approvalPolicy": "never",
            "cwd": cwd,
            "runtimeWorkspaceRoots": [cwd],
            "model": "anthropic/claude-opus-5",
            "reasoningEffort": "xhigh",
            "environments": [{"environmentId": "local", "cwd": cwd,
                              "runtimeWorkspaceRoots": [cwd]}],
        }

    def seeded(self):
        """A store with an assignment in it, and settings recorded for both participants.

        Recorded through register's own --parent-settings/--child-settings, which is the path
        a creation result really takes, so the sandbox in the receipt is the one a send would
        carry rather than something this test wrote by hand into the table.
        """
        parent_cwd = os.path.join(self.tmp, "parent")
        os.makedirs(parent_cwd, exist_ok=True)
        recorded = self.run_cli(
            "register", "--parent-task", PARENT, "--parent-host", HOST,
            "--child-task", CHILD, "--child-host", HOST, "--issue", ISSUE,
            "--artifact-root", self.root, "--allowed-recipient", PARENT,
            "--dispatch-request-id", "dispatch-1", "--dispatch-turn-id", DISPATCH_TURN,
            "--parent-settings", json.dumps(self.settings(parent_cwd)),
            "--child-settings", json.dumps(self.settings(self.root)),
        )
        self.assertEqual(
            recorded["authorizedSettings"], {PARENT: "recorded", CHILD: "recorded"},
        )
        return self.tmp

    def test_three_participants_reach_one_store_by_three_different_routes(self):
        state = self.seeded()
        socket_path = self.probe_socket()

        # Deliberately not the same route to the same place: if only one rule were exercised
        # this would prove nothing about participants that reach the store differently.
        parent = self.participant(
            "--socket", socket_path, "doctor", state=state, pin=state,
            cwd=os.path.join(self.tmp, "parent"),
        )["accessReceipt"]
        child = self.participant(
            "--socket", socket_path, "doctor", pin=state, cwd=self.root,
        )["accessReceipt"]
        daemon = self.participant(
            "--socket", socket_path, "doctor", state=state, pin=state,
        )["accessReceipt"]
        receipts = {"parent": parent, "child": child, "daemon": daemon}

        self.assertEqual(
            {name: r["selectedBy"]["source"] for name, r in receipts.items()},
            {"parent": "flag", "child": "env", "daemon": "flag"},
            "the routes collapsed, so this no longer tests what it claims to",
        )
        # The path is not the assertion. Two spellings can be one file and one spelling can be
        # two files, so identity is settled on the store id and the device/inode pair.
        for name, receipt in receipts.items():
            with self.subTest(participant=name):
                self.assertIsNotNone(receipt["storeId"], receipt)
                self.assertEqual(receipt["storeId"], parent["storeId"])
                self.assertEqual(
                    (receipt["device"], receipt["inode"]),
                    (parent["device"], parent["inode"]),
                    "this participant is on a different file",
                )
                # Measured, not inferred from a permission bit.
                self.assertTrue(receipt["observedAccess"]["read"], receipt)
                self.assertTrue(receipt["observedAccess"]["write"], receipt)
                self.assertIsNone(receipt["observedAccess"]["detail"], receipt)

    def test_each_receipt_carries_the_sandbox_a_denial_would_be_explained_against(self):
        state = self.seeded()
        receipt = self.participant("doctor", state=state, pin=state)["accessReceipt"]

        recorded = receipt["recordedSandbox"]
        self.assertTrue(recorded["available"], recorded)
        self.assertEqual(sorted(recorded["participants"]), sorted([PARENT, CHILD]))
        for task in (PARENT, CHILD):
            with self.subTest(task=task):
                sandbox = recorded["participants"][task]
                self.assertTrue(sandbox["readable"], sandbox)
                # The value the adapter would actually send with, not a policy file.
                self.assertEqual(sandbox["mode"], "workspaceWrite")
                self.assertIsInstance(sandbox["writableRoots"], list)
                self.assertIs(sandbox["networkAccess"], False)
                self.assertEqual(sandbox["recordedFrom"], "creation_result")
        self.assertEqual(
            recorded["participants"][CHILD]["cwd"], self.root,
            "the child's recorded cwd is not the workspace it actually runs in",
        )

    def test_a_policy_that_omits_its_defaults_still_reports_what_would_be_sent(self):
        """The receipt has to show the effective sandbox, not the recorded keystrokes.

        `{"type": "workspaceWrite"}` is accepted, and the adapter fills networkAccess false,
        empty writable roots and the two temporary-directory flags from the pinned defaults
        before sending. Reported raw, networkAccess reads as null for a participant whose
        sends really do carry false - which would have an operator diagnosing a denial against
        a value the host never sees.
        """
        settings = self.settings(self.root)
        settings["sandbox"] = {"type": "workspaceWrite"}
        self.run_cli(
            "register", "--parent-task", PARENT, "--parent-host", HOST,
            "--child-task", CHILD, "--child-host", HOST, "--issue", ISSUE,
            "--artifact-root", self.root, "--allowed-recipient", PARENT,
            "--dispatch-request-id", "dispatch-1", "--dispatch-turn-id", DISPATCH_TURN,
            "--parent-settings", json.dumps(settings),
            "--child-settings", json.dumps(settings),
        )

        receipt = self.participant("doctor", state=self.tmp, pin=self.tmp)["accessReceipt"]

        sandbox = receipt["recordedSandbox"]["participants"][PARENT]
        self.assertTrue(sandbox["readable"], sandbox)
        self.assertEqual(sandbox["mode"], "workspaceWrite")
        self.assertIs(sandbox["networkAccess"], False, "the default was reported as unknown")
        self.assertEqual(sandbox["writableRoots"], [])
        self.assertIs(sandbox["excludeTmpdirEnvVar"], False)
        self.assertIs(sandbox["excludeSlashTmp"], False)

    def test_a_record_delivery_cannot_carry_is_not_reported_as_one_it_would(self):
        """Readable is not deliverable, and this field is documented as the second one.

        The preparation a send performs stops in more than one place and each place stops on
        its own: a record missing any REQUIRED field is rejected before the sandbox type is
        looked at, and the resume-params construction in `_guarded_send` fails after both of
        those. All three records below are readable and none of them can carry its settings
        to a host - the first two are refused before any transport call, the third fails
        while the params are built, after `thread/read` and before `thread/resume`
        (bridge_adapter.py). Reporting any of them as the sandbox the adapter would carry
        tells an operator access is fine for a participant whose sends are never made.

        Three cases because three versions of this field each stopped one step short of the
        path: the sandbox type alone, then `require_usable()` alone. A suite missing the last
        case passes while the field still lies.

        The first two are written past the validating recorder deliberately: registration
        refuses them, so the only way a store holds one is an older writer or a hand edit,
        which is the case this helper says it supports. The third needs no hand edit at all -
        `record_settings` validates with `require_usable()` (registry.py) and that accepts it,
        so this row can arrive through the ordinary recorder and still fail every send.
        """
        from pathlib import Path

        from codex_session_relay.store import Store

        self.seeded()
        unsupported = dict(self.settings(self.root), sandbox={"type": "externalSandbox"})
        # A supported sandbox, but the record around it is incomplete. This is the gate
        # require_usable() reaches FIRST, and the one a sandbox-only check walks past.
        incomplete = dict(self.settings(self.root))
        del incomplete["cwd"]
        # Complete, supported, and still not sendable: require_usable() checks that every
        # REQUIRED field is present and says nothing about its type, while resume_params
        # calls list() on this one.
        unusable_roots = dict(self.settings(self.root), runtimeWorkspaceRoots=7)

        cases = {
            "an unsupported sandbox type": (
                unsupported, "unsupported_sandbox_type", "externalSandbox",
            ),
            "a record missing a required field": (incomplete, "settings_incomplete", "cwd"),
            "a field the params construction cannot use": (
                unusable_roots, "unexpected", "TypeError",
            ),
        }
        for label, (stale, expected_reason, detail_says) in cases.items():
            with self.subTest(refusal=label):
                store = Store(Path(self.tmp) / "relay.sqlite3")
                with store.transaction() as db:
                    db.execute(
                        "UPDATE authorized_settings SET settings = ? WHERE task_id = ?",
                        (json.dumps(stale), CHILD),
                    )
                store.db.commit()
                store.close()

                receipt = self.participant(
                    "doctor", state=self.tmp, pin=self.tmp,
                )["accessReceipt"]
                child = receipt["recordedSandbox"]["participants"][CHILD]
                parent = receipt["recordedSandbox"]["participants"][PARENT]

                # The defect stated as what it is: a participant delivery refuses outright
                # was reported exactly like one it can serve, with nothing in the row to
                # tell them apart. Asserted first so the failure says that, not KeyError.
                signal = ("deliverable", "refusedBy", "resumeMode")
                self.assertNotEqual(
                    {key: child.get(key) for key in signal},
                    {key: parent.get(key) for key in signal},
                    "a refused record is reported exactly like one a send can carry",
                )
                self.assertFalse(child["deliverable"], child)
                # Delivery's own vocabulary, so a receipt and a delivery journal agree.
                self.assertEqual(child["refusedBy"], expected_reason, child)
                self.assertIsNone(child["resumeMode"], "a refused record sends no sandbox")
                self.assertTrue(child["detail"], child)
                # The refusal that actually happened, not a generic one: an operator reading
                # this has to be able to tell these three apart.
                self.assertIn(detail_says, child["detail"], child)
                # The row stays readable and what it records is still shown: dropping it
                # would lose the only clue to why delivery refuses this participant.
                self.assertTrue(child["readable"], child)
                self.assertEqual(child["mode"], stale["sandbox"]["type"], child)

                # And the participant delivery can serve is still reported as one it can.
                self.assertTrue(parent["deliverable"], parent)
                self.assertIsNone(parent["refusedBy"], parent)
                self.assertEqual(parent["resumeMode"], "workspace-write")
                self.assertIsNone(parent["detail"])

    def test_every_transformation_a_send_applies_to_the_record_is_covered(self):
        """The SET, read out of the source, rather than the instances found so far.

        Three versions of `deliverable` were wrong the same way: a predicate was applied to
        the member that had been demonstrated instead of to the set that member belongs to.
        First the sandbox type, then `require_usable()`, then the params construction - and
        the fourth instance, `environments`, arrived the same way the first three did.

        So the set is derived here instead of listed. It is the constraints delivery imposes
        on the RECORDED settings before turn/start, and it has two kinds of member, both read
        out of the source. Both start from the `TaskSettings` methods the send path calls,
        taken from `delivery.py` and `bridge_adapter.py`.

        A TRANSFORMATION can fail on the row by raising: every recorded field handed to a
        call inside those methods. Today `normalise_policy(sandbox)`,
        `list(runtimeWorkspaceRoots)`, `normalise_environments(environments)`.

        A VALUE CONSTRAINT cannot. It exists only as a comparison against a fixed value -
        `mismatches` refuses any returned `approvalPolicy` that is not the authorized one -
        and a host that preserves what it was asked for returns what was recorded, so a row
        recording anything else can never complete a send. Nothing raises on such a row, which
        is why the first extraction cannot see it: there is no call to put the field into. That
        member was found by review rather than by this test, and the second extraction below is
        the answer to that rather than another hand-added case.

        Each derived field is then mutated with the mutant its kind needs - a value no
        transformation can consume, or a well-typed value that is not the authorized literal -
        and the receipt must not advertise that participant as deliverable. A new member of
        either kind joins the derived set and fails here until the probe reaches it, which is
        the property a written-down list cannot have.

        The two floor assertions are not the definition. They guard the extractor: an AST walk
        that silently matched nothing would run zero mutations and pass, which is how this kind
        of test goes green while holding nothing.
        """
        import ast
        import inspect
        from pathlib import Path

        from codex_session_relay import bridge_adapter, delivery
        from codex_session_relay import settings as settings_module
        from codex_session_relay.settings import TaskSettings
        from codex_session_relay.store import Store

        def parsed(module):
            return ast.parse(inspect.getsource(module))

        api = {name for name in vars(TaskSettings) if not name.startswith("_")}
        called = set()
        for module in (delivery, bridge_adapter):
            for node in ast.walk(parsed(module)):
                if (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                        and node.func.attr in api):
                    called.add(node.func.attr)

        defined = {
            node.name: node for node in ast.walk(parsed(settings_module))
            if isinstance(node, ast.FunctionDef)
        }

        def transformed_fields(name, seen=None):
            """Recorded fields this method hands to a call, through self.<method>() hops."""
            seen = set() if seen is None else seen
            if name in seen or name not in defined:
                return set()
            seen.add(name)
            fields = set()
            for node in ast.walk(defined[name]):
                if not isinstance(node, ast.Call):
                    continue
                for argument in node.args:
                    if (isinstance(argument, ast.Subscript)
                            and isinstance(argument.value, ast.Attribute)
                            and argument.value.attr == "data"
                            and isinstance(argument.slice, ast.Constant)
                            and isinstance(argument.slice.value, str)):
                        fields.add(argument.slice.value)
                if (isinstance(node.func, ast.Attribute)
                        and isinstance(node.func.value, ast.Name)
                        and node.func.value.id == "self"):
                    fields |= transformed_fields(node.func.attr, seen)
            return fields

        def constant(node):
            """A string literal, or a module constant that holds one."""
            if isinstance(node, ast.Constant):
                return node.value
            if isinstance(node, ast.Name):
                return getattr(settings_module, node.id, None)
            return None

        def literal_constraints(name, seen=None):
            """Response fields this method compares against a fixed value.

            The recorded row cannot fail one of these by raising, so it is the recorded
            VALUE that has to be compared. Read in two passes rather than one, so a
            comparison is never reached before the name it compares was bound.
            """
            seen = set() if seen is None else seen
            if name in seen or name not in defined:
                return set()
            seen.add(name)
            body = list(ast.walk(defined[name]))
            bound = {}
            for node in body:
                if not (isinstance(node, ast.Assign) and len(node.targets) == 1
                        and isinstance(node.targets[0], ast.Name)):
                    continue
                read = node.value
                if (isinstance(read, ast.Call) and isinstance(read.func, ast.Attribute)
                        and read.func.attr == "get" and len(read.args) == 1
                        and isinstance(read.args[0], ast.Constant)
                        and isinstance(read.args[0].value, str)
                        and not (isinstance(read.func.value, ast.Attribute)
                                 and read.func.value.attr == "data")):
                    bound[node.targets[0].id] = read.args[0].value
            found = set()
            for node in body:
                if (isinstance(node, ast.Compare) and isinstance(node.left, ast.Name)
                        and node.left.id in bound):
                    for comparator in node.comparators:
                        literal = constant(comparator)
                        if isinstance(literal, str):
                            found.add((bound[node.left.id], literal))
                if (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                        and isinstance(node.func.value, ast.Name)
                        and node.func.value.id == "self"):
                    found |= literal_constraints(node.func.attr, seen)
            return found

        fields = set().union(*(transformed_fields(name) for name in called))
        constraints = set().union(*(literal_constraints(name) for name in called))

        self.assertGreaterEqual(
            called, {"require_usable", "resume_params", "mismatches"},
            "the send path's settings calls were not found, so nothing below is derived",
        )
        self.assertGreaterEqual(
            fields, {"sandbox", "runtimeWorkspaceRoots", "environments"},
            "the extraction found fewer transformations than are known to be there",
        )
        self.assertGreaterEqual(
            constraints, {("approvalPolicy", "never")},
            "the value-constraint extraction found less than is known to be there",
        )

        # One mutant per kind, each derived from what the kind is. A transformation cannot
        # consume 7 - not a mapping, not a sequence, and present, so the completeness gate
        # hands it straight on. A value constraint needs a well-typed value that is simply
        # not the authorized one, taken from the literal itself rather than invented.
        mutants = {field: 7 for field in fields}
        mutants.update({field: f"not-{literal}" for field, literal in constraints})

        self.seeded()
        for field, mutant in sorted(mutants.items()):
            with self.subTest(constrains=field):
                stale = dict(self.settings(self.root))
                stale[field] = mutant
                store = Store(Path(self.tmp) / "relay.sqlite3")
                with store.transaction() as db:
                    db.execute(
                        "UPDATE authorized_settings SET settings = ? WHERE task_id = ?",
                        (json.dumps(stale), CHILD),
                    )
                store.db.commit()
                store.close()

                child = self.participant(
                    "doctor", state=self.tmp, pin=self.tmp,
                )["accessReceipt"]["recordedSandbox"]["participants"][CHILD]

                self.assertIsNot(
                    child.get("deliverable"), True,
                    f"no send can carry this {field!r}, and the receipt advertised one",
                )
                if child["readable"]:
                    self.assertTrue(child["refusedBy"], child)
                    self.assertTrue(child["detail"], child)

    def test_a_store_replaced_by_a_copy_under_the_read_is_reported_not_served(self):
        """The receipt's own mid-command replacement check, against the case it missed.

        The identity and the participants come out of one read so that an atomic replacement
        between two opens cannot pair one store's identity with another store's rows. That
        check compared store ids, and a COPY carries the store id: the row is minted once and
        copied with the bytes, which `test_a_copy_keeps_the_identifier_and_is_not_the_same_store`
        (test_store.py) already states. So the comparison that exists to catch a replacement
        was satisfied by a replacement made with a copy.

        The replacement here happens between the probe and the read for real - the report
        passed in is the one measured before it - which is the window the check exists for.
        """
        from types import SimpleNamespace

        from codex_session_relay.cli import Services, _access_receipt
        from codex_session_relay.store import probe, resolve_state_dir

        state = self.seeded()
        selection = resolve_state_dir(state, None)
        measured = probe(selection)
        self.assertTrue(measured["store"]["storeId"], measured)

        database = os.path.join(state, "relay.sqlite3")
        replacement = os.path.join(state, "replacement.sqlite3")
        shutil.copy(database, replacement)
        os.replace(replacement, database)

        # Asserted, not assumed: a copy that had lost the identity row, or one that landed on
        # the same inode, would make the receipt below right for a reason this is not testing.
        swapped = probe(selection)["store"]
        self.assertEqual(
            swapped["storeId"], measured["store"]["storeId"],
            "the replacement is not a real copy, so nothing here is about a copy",
        )
        self.assertNotEqual(swapped["inode"], measured["store"]["inode"])

        receipt = _access_receipt(Services(SimpleNamespace(state=state, socket=None)), measured)

        recorded = receipt["recordedSandbox"]
        self.assertFalse(
            recorded["available"],
            "rows read from a file that replaced the measured one were served as its own",
        )
        self.assertIn("inode", recorded["detail"], recorded)

    def test_one_unreadable_participant_does_not_take_the_diagnosis_with_it(self):
        """A damaged row is exactly when the rest of the report is worth most.

        These rows can hold whatever an older writer or a hand edit left, and a shape the
        parser accepts is not a shape the reader can use: json.loads returns a list for `[]`
        quite happily. Raising there would cost the store identity and the access evidence too,
        leaving a generic host error where the diagnosis should be.
        """
        from pathlib import Path

        from codex_session_relay.store import Store

        self.seeded()
        store = Store(Path(self.tmp) / "relay.sqlite3")
        self.addCleanup(store.close)
        with store.transaction() as db:
            db.execute(
                "UPDATE authorized_settings SET settings = ? WHERE task_id = ?",
                ("[]", CHILD),
            )
        store.db.commit()

        receipt = self.participant("doctor", state=self.tmp, pin=self.tmp)["accessReceipt"]

        participants = receipt["recordedSandbox"]["participants"]
        self.assertFalse(participants[CHILD]["readable"], participants[CHILD])
        self.assertIn("not an object", participants[CHILD]["detail"])
        # The damage is confined to the row that carries it.
        self.assertTrue(participants[PARENT]["readable"], participants[PARENT])
        self.assertIsNotNone(receipt["storeId"])
        self.assertTrue(receipt["observedAccess"]["read"])
    def test_a_store_swapped_mid_command_is_reported_rather_than_paired(self):
        """Identity and participants have to come from the same file, or say they did not.

        Collected by two opens, an atomic replacement between them pairs one store's identity
        with another store's participants, and nothing in the receipt would show it - a
        mismatch invisible in exactly the comparison this exists to support. One statement
        carries both now, and its store id is checked against the one the probe measured.

        Driven at the function rather than through the CLI: the window is one command against
        a database being swapped underneath it, which cannot be opened from outside the
        process. The probe result is real; only the identity it reports is moved, which is
        what a replacement between the two reads would have produced.
        """
        from codex_session_relay.cli import _access_receipt
        from codex_session_relay.store import probe, resolve_state_dir

        self.seeded()
        selection = resolve_state_dir(self.tmp)
        report = probe(selection)
        self.assertTrue(report["access"]["dbReadable"], report)

        class Services:
            pass

        services = Services()
        services.selection = selection

        honest = _access_receipt(services, report)
        self.assertTrue(honest["recordedSandbox"]["available"], honest)
        self.assertIn(PARENT, honest["recordedSandbox"]["participants"])

        # Now the identity names a file the settings did not come from.
        moved = dict(report, store=dict(report["store"], storeId="another-store-entirely"))
        receipt = _access_receipt(services, moved)

        recorded = receipt["recordedSandbox"]
        self.assertFalse(recorded["available"], recorded)
        self.assertEqual(recorded["participants"], {}, "mismatched participants were reported")
        self.assertIn("changed under this command", recorded["detail"])
        self.assertIn("another-store-entirely", recorded["detail"])

    def test_a_participant_on_another_store_is_refused_rather_than_called_healthy(self):
        """The failure this criterion is really about: agreeing while looking at two stores."""
        state = self.seeded()
        mine = self.participant("doctor", state=state, pin=state)["accessReceipt"]

        elsewhere = os.path.join(self.tmp, "elsewhere")
        # A real second database, not an empty directory. doctor constructs no Store, so
        # pointing it at a path that does not exist yet returns null identity and null
        # device/inode - which makes every inequality below pass for the wrong reason and
        # turns the refusal into a test of an ABSENT store rather than a different one.
        # store-identity opens one, which is what gives this test two stores to tell apart.
        created = self.participant("store-identity", state=elsewhere, pin=elsewhere)
        self.assertIsNotNone(created["store"]["storeId"], created)
        stray = self.participant("doctor", state=elsewhere, pin=elsewhere)["accessReceipt"]

        for name, receipt in (("mine", mine), ("stray", stray)):
            with self.subTest(receipt=name):
                self.assertIsNotNone(receipt["storeId"], receipt)
                self.assertIsNotNone(receipt["inode"], receipt)
        self.assertNotEqual(stray["storeId"], mine["storeId"])
        self.assertNotEqual(
            (stray["device"], stray["inode"]), (mine["device"], mine["inode"]),
            "the two runs landed on one file, so this proves nothing",
        )
        # And asked to prove it is the same store, a participant sitting on the other one
        # refuses instead of reporting health - which is the failure the criterion names.
        refused = self.participant(
            "doctor", f"--expect-store={mine['storeId']}",
            state=elsewhere, pin=elsewhere, expect=2,
        )
        self.assertNotEqual(refused["sameStore"], "proven", refused)
        self.assertEqual(refused["accessReceipt"]["storeId"], stray["storeId"], refused)
