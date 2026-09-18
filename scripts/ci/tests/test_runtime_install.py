"""Runtime installer and diagnosis behaviour, exercised against temporary destinations.

Every case here writes only into a temporary directory. None of it reads or changes a real
Codex home, an installed runtime, an MCP registration or an operational database.
"""

import argparse
import ast
import builtins
import contextlib
import errno
import json
import os
import stat
from pathlib import Path
import shutil
import subprocess
import sys
import hashlib
import sqlite3
import tempfile
import threading
import time
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))

from crw_runtime import (check, codexconfig, completion, definition, hooks, hostrecord,
                         ownership, pointer, reading, scope, staging, swapgate)

RUNTIME = ROOT / "scripts" / "runtime_install.py"

import runtime_install as runtime_install_module

try:
    import tomllib as _tomllib
    HAS_READER = True
except ImportError:
    HAS_READER = False

# Reading a Codex configuration needs tomllib, so on an interpreter without it there is no
# reader to exercise -- that is the design, not a gap. What 3.10 must still prove is that every
# non-empty configuration is refused with an actionable message, and
# ReaderDomainTests.test_without_tomllib_every_non_empty_configuration_is_refused does exactly
# that by simulating the absence on an interpreter that has it, so the property is checked on
# both jobs rather than only where it bites.
needs_reader = unittest.skipUnless(
    HAS_READER, "reading a configuration needs tomllib; this interpreter refuses instead")
_BRIDGE_TOOL = next(
    c["identityTool"] for c in definition.load()["components"]
    if c["component"] == "codex-thread-bridge")

# Trial fixtures now pass through the same preflight the relay enforces, so they need an
# artifact that is really there: an absolute, normalised, non-symlink regular file inside the
# declared root. Created once, under the system temporary directory, never in the worktree.
TRIAL_ROOT = str(Path(tempfile.gettempdir()) / "crw-jun104-trial-fixtures")
Path(TRIAL_ROOT).mkdir(parents=True, exist_ok=True)
TRIAL_ARTIFACT = str(Path(TRIAL_ROOT) / "deliverable.txt")
Path(TRIAL_ARTIFACT).write_text("a deliverable", encoding="utf-8")

# A stand-in for the interpreter of a SELECTED relay installation: this interpreter, with the
# relay importable. The preflight probes now run the runtime that will act on their answers
# rather than this checkout, so a case that wants the relay's real rule has to hand them a
# runtime that has the relay in it. A wrapper rather than a virtual environment, because the
# property under test is which runtime is asked, not how it was built.
_RELAY_RUNTIME = Path(tempfile.mkdtemp(prefix="crw-jun104-relay-runtime-")) / "python"
_RELAY_RUNTIME.write_text(
    "#!/bin/sh\n"
    'PYTHONPATH="' + str(ROOT / "packages" / "codex-session-relay" / "src") + '" '
    'exec "' + sys.executable + '" "$@"\n',
    encoding="utf-8")
_RELAY_RUNTIME.chmod(0o755)
RELAY_RUNTIME = str(_RELAY_RUNTIME)


def usable_settings():
    """Stub the relay's settings check for cases that are not about settings validity.

    The real check runs the relay's own reader and its own predicate in the relay's interpreter,
    and a case about step ordering or step gating has no relay to provide one. The cases that
    ARE about settings validity use the real thing.
    """
    import runtime_install

    return mock.patch.object(runtime_install, "settings_usable",
                             return_value={"usable": True, "detail": "stubbed for this case"})


def run(*args):
    return subprocess.run([sys.executable, str(RUNTIME), *args],
                          capture_output=True, text=True, timeout=180)


class DefinitionTests(unittest.TestCase):
    def test_the_committed_definition_describes_this_checkout(self):
        self.assertEqual(definition.verify(ROOT), [])
        done = run("verify-definition")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertTrue(json.loads(done.stdout)["ok"])

    def test_a_changed_digest_is_a_finding_rather_than_a_pass(self):
        data = definition.load()
        data["components"][0]["sourceDigest"] = "0" * 64
        findings = definition.verify(ROOT, definition=data)
        self.assertTrue(any("sourceDigest" in f for f in findings), findings)

    def test_an_upstream_revision_missing_from_the_provenance_narrative_is_a_finding(self):
        data = definition.load()
        data["components"][0]["upstream"]["revision"] = "f" * 40
        findings = definition.verify(ROOT, definition=data)
        self.assertTrue(any("upstream revision" in f for f in findings), findings)

    def test_the_digest_is_the_documented_walk(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "pkg"
            (root / "sub" / "__pycache__").mkdir(parents=True)
            (root / "a.py").write_text("a", encoding="utf-8")
            (root / "sub" / "b.py").write_text("b", encoding="utf-8")
            (root / "sub" / "__pycache__" / "junk.pyc").write_text("junk", encoding="utf-8")
            first = definition.ops12_digest(root)
            # Bytecode caches are excluded, so adding one cannot change the answer.
            (root / "sub" / "__pycache__" / "more.pyc").write_text("more", encoding="utf-8")
            self.assertEqual(first, definition.ops12_digest(root))
            (root / "sub" / "b.py").write_text("changed", encoding="utf-8")
            self.assertNotEqual(first, definition.ops12_digest(root))


class OwnershipTests(unittest.TestCase):
    def signals(self, **overrides):
        base = dict(entry_point_recorded=True, commit_matches=True, tree_matches=True,
                    working_tree_clean=True, digest_matches=True, has_point=True)
        base.update(overrides)
        return ownership.Signals(**base)

    def test_every_class_and_its_precedence(self):
        cases = (
            ("own", {}),
            ("unmeasured", {"has_point": False}),
            ("foreign", {"entry_point_recorded": False}),
            ("fork", {"working_tree_clean": False}),
            ("conflict", {"registration_conflict": "registered with another command"}),
        )
        for expected, overrides in cases:
            with self.subTest(expected=expected):
                self.assertEqual(ownership.classify(self.signals(**overrides))[0], expected)

    def test_a_dirty_checkout_is_a_fork_even_when_every_digest_matches(self):
        # This is the case precedence exists for: without it, the matching digest would read
        # as own and the user's uncommitted work would be reused as though it were recorded.
        classification, reasons = ownership.classify(
            self.signals(working_tree_clean=False, digest_matches=True, has_point=True)
        )
        self.assertEqual(classification, "fork")
        self.assertTrue(any("uncommitted" in r for r in reasons), reasons)

    def test_an_unreadable_signal_never_counts_as_agreement(self):
        classification, reasons = ownership.classify(
            ownership.Signals(entry_point_recorded=True, unreadable=["the Codex registration"])
        )
        self.assertEqual(classification, "unreadable")
        self.assertFalse(ownership.reusable(classification))
        self.assertIn("the Codex registration", reasons[0])

    def test_only_own_is_reusable(self):
        for name in ownership.CLASSES:
            self.assertEqual(ownership.reusable(name), name == "own")


class ConfigScannerTests(unittest.TestCase):
    REAL_SHAPE = (
        'model = "gpt-5"\n\n'
        '[mcp_servers.oracle]\ncommand = "/opt/oracle"\n\n'
        '[mcp_servers.oracle.env]\nKEY = "value"\n\n'
        '[mcp_servers.codex-thread-bridge]\n'
        'command = "/opt/bridge"\nargs = ["--socket", "/tmp/s.sock"]\n\n'
        '[mcp_servers.codex-thread-bridge.tools.create_thread]\nenabled = true\n'
    )

    @needs_reader
    def test_a_server_name_is_the_first_segment_and_sub_tables_belong_to_it(self):
        view = codexconfig.scan(self.REAL_SHAPE)
        self.assertTrue(view.readable, view.unreadable)
        self.assertEqual(sorted(view.servers), ["codex-thread-bridge", "oracle"])
        self.assertEqual(view.servers["codex-thread-bridge"]["command"], "/opt/bridge")
        self.assertEqual(view.servers["codex-thread-bridge"]["args"], ["--socket", "/tmp/s.sock"])

    @needs_reader
    def test_a_registration_written_another_way_is_never_appended_to_twice(self):
        """The property these shapes were protecting: a server that is there is found.

        Which reader finds it changed. Where tomllib exists the dotted and inline spellings
        are read correctly and register reports the conflict; where it does not the fallback
        refuses them. Both answers are safe, and neither appends a second definition, which
        is the only outcome that was ever dangerous.
        """
        cases = {
            "array of tables": '[[mcp_servers.x]]\ncommand = "a"\n',
            "dotted assignment": 'mcp_servers.x.command = "a"\n',
            "inline assignment": 'mcp_servers = { x = { command = "a" } }\n',
            "unterminated string": '[mcp_servers.x]\ncommand = "oops\n',
            "duplicate server": '[mcp_servers.x]\ncommand = "a"\n\n[mcp_servers.x]\ncommand = "b"\n',
        }
        for name, text in cases.items():
            with self.subTest(case=name):
                after, outcome, detail = codexconfig.register(text, "x", "/opt/new", [])
                self.assertIn(outcome, ("UNREADABLE", "CONFLICT"), name + ": " + detail)
                self.assertEqual(after, text, "nothing is appended to any of these")

    @needs_reader
    def test_a_table_header_inside_a_multiline_string_is_not_a_registration(self):
        text = 'note = """\n[mcp_servers.ghost]\n"""\n'
        view = codexconfig.scan(text)
        self.assertTrue(view.readable, view.unreadable)
        self.assertEqual(view.servers, {})

    @needs_reader
    def test_quoted_and_bare_spellings_are_one_server(self):
        view = codexconfig.scan('[mcp_servers."codex-thread-bridge"]\ncommand = "/opt/bridge"\n')
        self.assertEqual(sorted(view.servers), ["codex-thread-bridge"])

    @needs_reader
    def test_registration_is_idempotent_and_refuses_to_replace(self):
        created, outcome, _ = codexconfig.register(self.REAL_SHAPE, "new", "/opt/new", ["--x"])
        self.assertEqual(outcome, "CREATED")
        self.assertTrue(created.startswith(self.REAL_SHAPE),
                        "the prior content must survive byte for byte")

        again, outcome, _ = codexconfig.register(created, "new", "/opt/new", ["--x"])
        self.assertEqual(outcome, "LINKED")
        self.assertEqual(again, created, "an identical registration must write nothing")

        conflicted, outcome, detail = codexconfig.register(created, "new", "/opt/other", ["--x"])
        self.assertEqual(outcome, "CONFLICT")
        self.assertEqual(conflicted, created, "a conflicting registration must write nothing")
        self.assertIn("/opt/other", detail)

    @needs_reader
    def test_a_multiline_array_value_is_read_rather_than_guessed(self):
        text = '[mcp_servers.x]\ncommand = "/opt/x"\nargs = [\n  "--a",\n  "--b",\n]\n'
        view = codexconfig.scan(text)
        self.assertTrue(view.readable, view.unreadable)
        self.assertEqual(view.servers["x"]["args"], ["--a", "--b"])


class HookTests(unittest.TestCase):
    EXISTING = {"hooks": {"SessionStart": [
        {"hooks": [{"type": "command", "command": "echo existing", "timeout": 5}]}
    ]}}

    def test_installation_appends_and_preserves_every_existing_identity(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "hooks.json"
            path.write_text(json.dumps(self.EXISTING), encoding="utf-8")
            hook = {"type": "command", "command": "crw", "timeout": 10}

            planned = hooks.install(path, "SessionStart", hook, issue="JUN-104")
            self.assertEqual(planned["outcome"], "MISSING")
            self.assertEqual(planned["plan"]["identity"], "user:SessionStart:1:0")
            self.assertEqual(json.loads(path.read_text(encoding="utf-8")), self.EXISTING,
                             "a plan must write nothing")

            created = hooks.install(path, "SessionStart", hook, issue="JUN-104", apply=True)
            self.assertEqual(created["outcome"], "CREATED")
            self.assertEqual(created["identity"], "user:SessionStart:1:0")
            self.assertTrue(created["readBack"])
            self.assertTrue(created["existingIdentitiesPreserved"])
            # The existing hook keeps index 0, so its trusted hash stays attached to it.
            written = json.loads(path.read_text(encoding="utf-8"))
            self.assertEqual(written["hooks"]["SessionStart"][0], self.EXISTING["hooks"]["SessionStart"][0])

            again = hooks.install(path, "SessionStart", hook, issue="JUN-104", apply=True)
            self.assertEqual(again["outcome"], "LINKED")
            self.assertEqual(again["identity"], "user:SessionStart:1:0",
                             "LINKED must name the hook that is there, not the next free slot")

    def test_installation_is_not_activation(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "hooks.json"
            path.write_text(json.dumps(self.EXISTING), encoding="utf-8")
            created = hooks.install(path, "SessionStart", {"type": "command", "command": "crw"},
                                    issue="JUN-104", apply=True)
            self.assertTrue(created["installed"])
            self.assertEqual(created["enabled"], "unknown")
            self.assertEqual(created["observedFired"], "unknown")

    def test_removal_is_refused_because_it_renumbers_later_identities(self):
        outcome = hooks.disable(self.EXISTING, "SessionStart", 0, 0)
        self.assertEqual(outcome["outcome"], "REFUSED")
        self.assertIn("renumbers", outcome["detail"])


class CheckRecordTests(unittest.TestCase):
    def fields(self):
        return {name: check.unknown("not observed") for name in check.FIELDS}

    def test_every_result_must_be_stated(self):
        partial = self.fields()
        partial.pop("connected")
        with self.assertRaises(ValueError):
            check.record(partial, destination="/tmp/x", destination_kind="temporary")

    def test_a_result_without_a_timed_observation_reports_unknown_rather_than_a_plausible_time(self):
        self.assertEqual(check.unknown("nothing was observed")["measuredAt"], "unknown")

    def test_a_temporary_destination_is_recorded_as_one(self):
        record = check.record(self.fields(), destination="/tmp/x", destination_kind="temporary")
        self.assertEqual(record["destinationKind"], "temporary")
        self.assertEqual(sorted(record["results"]), sorted(check.FIELDS))
        with self.assertRaises(ValueError):
            check.record(self.fields(), destination="/tmp/x", destination_kind="production")

    def test_import_and_settings_preservation_are_their_own_results(self):
        for name in ("imported", "settingsPreserved"):
            self.assertIn(name, check.FIELDS)


class HostRecordTests(unittest.TestCase):
    def test_a_point_covers_only_its_own_install_interpreter_and_bytes(self):
        record = hostrecord.empty(1)
        hostrecord.add_point(record, "codex-session-relay", {
            "exercised": True, "install": "/env/pkg", "interpreter": "3.13.1",
            "installDigest": "abc", "exerciseDigest": "an-instrument", "method": "doctor",
            "codexCli": "0.1.0", "host": "a-host",
        })
        matching = dict(location="/env/pkg", interpreter="3.13.1", install_digest="abc", exercise_digest="an-instrument",
                        codex_cli="0.1.0", host="a-host")
        self.assertEqual(len(hostrecord.points_for(record, "codex-session-relay", **matching)), 1)
        for change in (dict(location="/other"), dict(interpreter="3.11.0"), dict(install_digest="def")):
            with self.subTest(change=change):
                asked = dict(matching)
                asked.update(change)
                self.assertEqual(hostrecord.points_for(record, "codex-session-relay", **asked), [])

    def test_points_are_appended_rather_than_replaced(self):
        record = hostrecord.empty(1)
        for index in range(2):
            hostrecord.add_point(record, "codex-session-relay", {"exercised": True, "n": index})
        self.assertEqual(len(record["components"]["codex-session-relay"]["measuredPoints"]), 2)

    def test_the_record_round_trips_through_an_atomic_write(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "nested" / "host-record.json"
            record = hostrecord.empty(1)
            hostrecord.put_install(record, "codex-session-relay", {"location": "/env/pkg"})
            hostrecord.save(path, record)
            read = hostrecord.load(path, 1)
            self.assertEqual(read.state, reading.PRESENT)
            self.assertEqual(read.value["components"], record["components"])


class ScopeReadingTests(unittest.TestCase):
    def test_an_unreported_sibling_inventory_is_not_an_empty_one(self):
        # An older installed relay emits no siblingStores at all. Rendering that as "none
        # found" would hide exactly the conflict the inventory exists to surface.
        self.assertIn("not reported", scope.sibling_reading({"stateDirectory": "/s"}))
        self.assertIn("not checked", scope.sibling_reading(
            {"siblingStores": {"checked": False, "reason": "the state directory was chosen explicitly"}}
        ))
        self.assertEqual(scope.sibling_reading({"siblingStores": {"checked": True}}), "checked")

    def test_the_state_directory_is_read_from_either_payload_shape(self):
        self.assertEqual(scope.state_directory({"stateDirectory": "/a"}), "/a")
        self.assertEqual(scope.state_directory({"stateSelection": {"path": "/b"}}), "/b")

    def test_every_store_is_listed_once_and_none_is_adopted(self):
        readings = {
            "discovery": {"ok": True, "payload": {"stateSelection": {"path": "/s/scope"},
                "siblingStores": {"checked": True, "withoutProvenance": ["/s/default"],
                                  "claimingThisSocket": []}}},
            "selected": {"ok": True, "payload": {"stateSelection": {"path": "/s/scope"}}},
            "rootCandidate": {"ok": True, "payload": {"store": {"exists": True}}},
        }
        seen = scope.stores_seen(readings, env={"XDG_STATE_HOME": "/s"})
        paths = [entry["path"] for entry in seen]
        self.assertEqual(len(paths), len(set(paths)), "each store is listed once")
        self.assertIn("/s/default", paths)
        self.assertIn("/s/codex-session-relay", paths)

    def test_nothing_on_disk_is_hidden_when_the_relay_cannot_report_siblings(self):
        # The host case this exists for: an installed relay too old to emit siblingStores.
        # Listing files is not rediscovery; no database is opened and no candidate is chosen.
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "codex-session-relay"
            (root / "default").mkdir(parents=True)
            (root / "scope").mkdir()
            (root / "relay.sqlite3").write_text("", encoding="utf-8")
            (root / "default" / "relay.sqlite3").write_text("", encoding="utf-8")
            (root / "scope" / "operations-scope.sqlite3").write_text("", encoding="utf-8")

            env = {"XDG_STATE_HOME": temporary}
            listed = scope.filesystem_candidates(env)
            databases = sorted(entry["database"] for entry in listed)
            self.assertEqual(databases, [
                str(root / "default" / "relay.sqlite3"),
                str(root / "relay.sqlite3"),
                str(root / "scope" / "operations-scope.sqlite3"),
            ])
            self.assertTrue(all("listed" in entry["foundBy"] for entry in listed))

            # A relay that reports nothing must still not produce an empty inventory.
            seen = scope.stores_seen(
                {"discovery": {"ok": True, "payload": {"stateDirectory": str(root)}}}, env)
            self.assertIn(str(root / "default" / "relay.sqlite3"),
                          [entry.get("database") for entry in seen])


class EntryPointTests(unittest.TestCase):
    def test_the_skill_installer_is_untouched_and_still_standard_library_only(self):
        source = (ROOT / "scripts" / "install.py").read_text(encoding="utf-8")
        for forbidden in ("import requests", "subprocess", "crw_runtime"):
            self.assertNotIn(forbidden, source)

    @needs_reader
    def test_registration_through_the_cli_is_idempotent_and_preserves_other_servers(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary) / "codex"
            home.mkdir()
            original = '[mcp_servers.cxc]\ncommand = "/opt/cxc"\n\n[mcp_servers.cxc.env]\nA = "b"\n'
            (home / "config.toml").write_text(original, encoding="utf-8")
            args = ["register-mcp", "--codex-home", str(home),
                    "--bridge-command", "/opt/bridge", "--bridge-arg=--socket",
                    "--bridge-arg=/tmp/s.sock", "--apply"]

            first = run(*args)
            self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
            self.assertEqual(json.loads(first.stdout)["outcome"], "CREATED")
            after_first = (home / "config.toml").read_text(encoding="utf-8")
            self.assertTrue(after_first.startswith(original))

            second = run(*args)
            self.assertEqual(json.loads(second.stdout)["outcome"], "LINKED")
            self.assertEqual((home / "config.toml").read_text(encoding="utf-8"), after_first,
                             "a rerun must not duplicate or replace the registration")

            clash = run(*[a if a != "/opt/bridge" else "/opt/elsewhere" for a in args])
            self.assertEqual(clash.returncode, 1)
            self.assertEqual(json.loads(clash.stdout)["outcome"], "CONFLICT")
            self.assertEqual((home / "config.toml").read_text(encoding="utf-8"), after_first,
                             "a conflict must leave the file untouched")

    def test_diagnosis_creates_no_work_without_an_explicit_trial(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary) / "codex"
            home.mkdir()
            done = run("diagnose", "--codex-home", str(home), "--temporary",
                       "--record", str(Path(temporary) / "record.json"),
                       "--bridge-command", "/opt/bridge")
            self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
            payload = json.loads(done.stdout)
            results = payload["checks"]["results"]
            self.assertEqual(results["deliveryAccepted"]["value"], "not_applicable")
            self.assertIn("--trial", results["deliveryAccepted"]["evidence"])
            self.assertEqual(payload["checks"]["destinationKind"], "temporary")
            # A configuration entry alone never establishes exposure, and installation is
            # never read from any of the others.
            self.assertEqual(results["mcpExposed"]["value"], "not_verified")
            self.assertEqual(results["alwaysActive"]["value"], "not_verified")
            self.assertEqual(results["verificationComplete"]["value"], "not_applicable")

    def test_install_plans_before_it_applies_and_never_overwrites_an_environment(self):
        with tempfile.TemporaryDirectory() as temporary:
            done = run("install", "--dest", temporary, "--record",
                       str(Path(temporary) / "record.json"))
            payload = json.loads(done.stdout)
            if "refused" in payload and "interpreter" in payload["refused"]:
                # A host with no interpreter satisfying requires-python refuses and names
                # the requirement. The controller never selects itself for a newer runtime.
                self.assertEqual(done.returncode, 1)
                self.assertIn("requiresPython", payload)
                return
            self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
            self.assertFalse(payload["applied"])
            steps = [step["step"] for step in payload["plan"]]
            self.assertLess(steps.index("measure the candidate"),
                            steps.index("promote the recorded pointer"),
                            "promotion must follow the qualifying measurement")


class ReviewFixTests(unittest.TestCase):
    """Behaviour corrected after the first review round, each kept covered."""

    def test_an_installation_recorded_on_this_host_counts_as_a_recorded_root(self):
        # Without this, a runtime installed outside the checkout classifies foreign for
        # ever and nothing the installer produces could ever be reused.
        import runtime_install

        record = hostrecord.empty(1)
        hostrecord.put_install(record, "codex-session-relay",
                               {"location": "/opt/env/lib/codex_session_relay",
                                "environment": "/opt/env"})
        roots = [str(r) for r in runtime_install.recorded_roots(record, "codex-session-relay")]
        self.assertIn("/opt/env", roots)
        self.assertIn("/opt/env/lib/codex_session_relay", roots)
        self.assertEqual(runtime_install.recorded_roots(record, "codex-thread-bridge"),
                         [ROOT.resolve()])

    @needs_reader
    def test_diagnosis_without_an_expected_command_compares_nothing(self):
        # Comparing a correct registration against an invented empty command reported
        # CONFLICT for a host that was registered exactly right.
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            (home / "config.toml").write_text(
                '[mcp_servers.codex-thread-bridge]\ncommand = "/opt/bridge"\n', encoding="utf-8")
            state = runtime_install.registration_state(home, None, [])
            self.assertEqual(state["outcome"], "PRESENT")
            self.assertFalse(state["wouldWrite"])
            self.assertEqual(state["registered"]["command"], "/opt/bridge")

            absent = runtime_install.registration_state(Path(temporary) / "empty", None, [])
            self.assertEqual(absent["outcome"], "ABSENT")

    def test_an_unrelated_tool_list_does_not_prove_this_bridge_is_exposed(self):
        import runtime_install

        registration = {"outcome": "LINKED", "detail": "", "wouldWrite": False}
        unrelated = runtime_install._mcp_exposed(registration, ["some_other_tool"])
        self.assertEqual(unrelated["value"], "not_verified")
        self.assertIn("get_capabilities", unrelated["evidence"])

        real = runtime_install._mcp_exposed(registration, ["get_capabilities", "create_thread"])
        self.assertEqual(real["value"], "verified")

    def test_an_identical_hook_under_another_event_does_not_satisfy_this_one(self):
        hook = {"type": "command", "command": "crw", "timeout": 10}
        document = {"hooks": {"Stop": [{"hooks": [hook]}]}}
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "hooks.json"
            path.write_text(json.dumps(document), encoding="utf-8")
            result = hooks.install(path, "SessionStart", hook, issue="JUN-104", apply=True)
            self.assertEqual(result["outcome"], "CREATED")
            self.assertEqual(result["identity"], "user:SessionStart:0:0")
            written = json.loads(path.read_text(encoding="utf-8"))
            self.assertIn("SessionStart", written["hooks"])
            self.assertEqual(len(written["hooks"]["Stop"]), 1, "the other event is untouched")

    def test_a_point_from_another_codex_or_host_does_not_authorize_reuse(self):
        record = hostrecord.empty(1)
        hostrecord.add_point(record, "codex-session-relay", {
            "exercised": True, "install": "/env/pkg", "interpreter": "3.13.1",
            "installDigest": "abc", "exerciseDigest": "an-instrument", "codexCli": "0.154.0", "host": "one",
        })
        asked = dict(location="/env/pkg", interpreter="3.13.1", install_digest="abc", exercise_digest="an-instrument")
        self.assertEqual(len(hostrecord.points_for(record, "codex-session-relay", **asked,
                                                   codex_cli="0.154.0", host="one")), 1)
        self.assertEqual(hostrecord.points_for(record, "codex-session-relay", **asked,
                                               codex_cli="0.200.0", host="one"), [])
        self.assertEqual(hostrecord.points_for(record, "codex-session-relay", **asked,
                                               codex_cli="0.154.0", host="other"), [])

    def test_a_refusal_payload_does_not_replace_a_reading_that_answered(self):
        """Two properties, and the second one changed.

        A structured refusal still parses as JSON, so its payload must never be read as an
        answer. That is unchanged. What changed is what happens next: an explicitly selected
        store whose doctor refused does not fall back to the discovered store, because they are
        answers to different questions and every other command is acting on the selected one.
        """
        readings = {
            "discovery": {"ok": True, "payload": {"stateSelection": {"path": "/s/scope"},
                                                  "actorReachability": {"socketConnect": "ok"}}},
            "selected": {"ok": False, "payload": {"error": "refused", "stateDirectory": "/s/other"}},
        }
        summary = scope.summarise(readings, env={"XDG_STATE_HOME": "/nowhere"})
        self.assertNotEqual(summary["stateDirectory"], "/s/other",
                            "a refusal payload is not a reading")
        self.assertIsNone(summary["stateDirectory"],
                          "the selected store did not answer, so no scope is reported")
        self.assertIsNone(summary["socketConnect"])
        self.assertIn("did not answer", summary["scopeAnsweredBy"])

        # With no explicit selection, discovery IS the answer to this question, and the skipped
        # reading does not interfere.
        readings["selected"] = {"ok": False, "skipped": "no store is explicitly selected"}
        summary = scope.summarise(readings, env={"XDG_STATE_HOME": "/nowhere"})
        self.assertEqual(summary["stateDirectory"], "/s/scope")
        self.assertEqual(summary["socketConnect"], "ok")
        self.assertIn("no store is explicitly selected", summary["scopeAnsweredBy"])


RELAY_CLI = ROOT / "packages/codex-session-relay/src/codex_session_relay/cli.py"


def relay_required_arguments():
    """Each relay subcommand's required options, read from the relay's own parser.

    Read with `ast` over the source rather than by importing it. The relay declares a newer
    `requires-python` than the interpreter this repository runs its own checks with, and
    `scripts/ci/validate.py` already parses every tracked Python file this way on that
    interpreter, so a static read is the version-safe way to ask the parser what it requires.
    """
    tree = ast.parse(RELAY_CLI.read_text(encoding="utf-8"))
    builder = next(node for node in ast.walk(tree)
                   if isinstance(node, ast.FunctionDef) and node.name == "build_parser")
    names, required = {}, {}
    for node in ast.walk(builder):
        if (isinstance(node, ast.Assign) and len(node.targets) == 1
                and isinstance(node.targets[0], ast.Name)
                and isinstance(node.value, ast.Call)
                and isinstance(node.value.func, ast.Attribute)
                and node.value.func.attr == "add_parser"
                and node.value.args
                and isinstance(node.value.args[0], ast.Constant)):
            names[node.targets[0].id] = node.value.args[0].value
            required.setdefault(node.value.args[0].value, set())
    for node in ast.walk(builder):
        if (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                and node.func.attr == "add_argument"
                and isinstance(node.func.value, ast.Name)
                and node.func.value.id in names
                and node.args and isinstance(node.args[0], ast.Constant)
                and str(node.args[0].value).startswith("--")):
            if any(kw.arg == "required" and getattr(kw.value, "value", False) is True
                   for kw in node.keywords):
                required[names[node.func.value.id]].add(node.args[0].value)
    return required


class TrialArgumentTests(unittest.TestCase):
    """The trial must satisfy the relay's own required arguments.

    This is the check that was missing: the earlier test only asserted the wording of the
    `not_applicable` message, so it never executed the trial path and a missing required
    argument reached a green gate and twenty-six review threads untouched.
    """

    def steps(self):
        """Every invocation the trial can make, so the comparison covers all of them.

        The fixture supplies every optional input on purpose. Built without them,
        trial_steps omits settings-record, and a comparison run over the shortened list
        would report success while that command's required arguments were never checked.
        """
        import runtime_install

        return runtime_install.trial_steps(
            issue="JUN-104", parent_task="parent", child_task="child",
            recipient="parent", artifact_root="/tmp/artifacts",
            turn_thread="thread-1", turn_id="turn-1", host="a-host",
            artifacts=["/tmp/artifacts/result.txt"], dispatch_turn_id="anchor-1",
            recipient_settings="@/tmp/settings.json",
        )

    def test_the_comparison_covers_every_command_the_trial_can_send(self):
        # Guards the fixture itself. If trial_steps grows a command, or the fixture stops
        # producing one, the comparison silently stops covering it.
        self.assertEqual([argv[0] for argv in self.steps()],
                         ["assignment-find", "register", "settings-record", "generation-open",
                          "generation-bind", "admit-turn", "emit", "deliver"])

    def test_the_relay_parser_is_readable_and_names_what_it_requires(self):
        required = relay_required_arguments()
        # Guards the reader itself: if this ever comes back empty the comparison below would
        # pass vacuously, which is exactly the shape of failure this class exists to stop.
        self.assertIn("generation-open", required)
        self.assertIn("--dispatch-request-id", required["generation-open"])
        self.assertIn("--relationship", required["generation-open"])
        self.assertTrue(required["register"], "register declares required arguments")

    def test_every_trial_invocation_supplies_every_required_argument(self):
        required = relay_required_arguments()
        for argv in self.steps():
            subcommand = argv[0]
            with self.subTest(subcommand=subcommand):
                self.assertIn(subcommand, required,
                              subcommand + " is not a relay subcommand")
                supplied = {token for token in argv if str(token).startswith("--")}
                missing = sorted(required[subcommand] - supplied)
                self.assertEqual(missing, [],
                                 subcommand + " omits required " + ", ".join(missing))

    def test_the_trial_performs_the_whole_sequence_a_delivery_needs(self):
        # Measured against a running App Server: a send is withheld until the recipient's
        # settings are on record, a generation opened by register is unbound until it is
        # bound to the dispatch turn, and a turn other than the anchor needs admission.
        # Dropping any of these silently returns the trial to never being able to deliver.
        import runtime_install

        with_settings = runtime_install.trial_steps(
            issue="JUN-104", parent_task="parent", child_task="child", recipient="parent",
            artifact_root="/tmp/artifacts", turn_thread="thread-1", turn_id="turn-1",
            host="a-host", artifacts=["/tmp/artifacts/result.txt"],
            dispatch_turn_id="anchor-1", recipient_settings="@/tmp/settings.json",
        )
        self.assertEqual([argv[0] for argv in with_settings],
                         ["assignment-find", "register", "settings-record", "generation-open",
                          "generation-bind", "admit-turn", "emit", "deliver"])
        emit = next(argv for argv in with_settings if argv[0] == "emit")
        self.assertIn("--artifact", emit,
                      "a reviewable receipt whose manifest is empty is refused")

    def test_the_settings_step_is_omitted_rather_than_sent_empty(self):
        import runtime_install

        without = runtime_install.trial_steps(
            issue="JUN-104", parent_task="parent", child_task="child", recipient="parent",
            artifact_root="/tmp/artifacts", turn_thread="thread-1", turn_id="turn-1",
            host="a-host",
        )
        self.assertNotIn("settings-record", [argv[0] for argv in without])

    def test_the_trial_reads_the_generation_field_the_relay_returns(self):
        """The generation the relay reports must reach the commands that need it.

        register and generation-open both report it as executionGeneration; reading
        generation or generationId yields None and sends the literal "--generation None".
        Checked by driving _trial with stubbed relay responses rather than by looking for
        the field name in the source, which a comment alone would satisfy.
        """
        import runtime_install

        payloads = {
            "assignment-find": {"ok": True, "payload": {"relationships": []}},
            "settings-record": {"ok": True, "payload": {"recorded": True}},
            "register": {"ok": True, "payload": {"relationshipId": "rel-1",
                                                 "executionGeneration": 7}},
            "generation-open": {"ok": True, "payload": {"executionGeneration": 7}},
            "generation-bind": {"ok": True, "payload": {"bound": True}},
            "admit-turn": {"ok": True, "payload": {"admitted": True}},
            "emit": {"ok": True, "payload": {"receipt": {"eventId": "ev-1"}}},
            "deliver": {"ok": True, "payload": {"attempt": {"turnId": "turn-9"}}},
        }
        sent = []

        def fake_relay(command, **kwargs):
            sent.append(list(command))
            reading = dict(payloads[command[0]])
            reading["command"] = list(command)
            return reading

        class Args:
            issue = "JUN-104"
            parent_task = recipient = "parent"
            child_task = "child"
            artifact_root = TRIAL_ROOT
            artifact = [TRIAL_ARTIFACT]
            dispatch_turn_id = "anchor-1"
            recipient_settings = "@/tmp/settings.json"
            # The relay requires a receipt's thread to be the relationship's child task, and
            # the preflight now requires it before anything is written.
            turn_thread = "child"
            turn_id = "turn-1"
            turn_status = "completed"
            settings_already_recorded = False
            expect_relationship = None
            socket = state = None

        original = runtime_install.scope.relay
        runtime_install.scope.relay = fake_relay
        try:
            with usable_settings():
                result = runtime_install._trial(Args(), "/opt/relay", RELAY_RUNTIME)
        finally:
            runtime_install.scope.relay = original

        self.assertEqual(result["value"], "verified", result["evidence"])
        self.assertIn("turn-9", result["evidence"])
        emitted = next(argv for argv in sent if argv[0] == "emit")
        self.assertIn("7", emitted, "the reported generation must reach emit")
        self.assertNotIn("None", emitted)


class AuthorizedRepairTests(unittest.TestCase):
    """The six areas the coordinator authorized after escalation."""

    TRI = '"' * 3

    # (a) the reader reads what it wrote, and refuses what it does not model ---------

    @needs_reader
    def test_what_render_writes_scan_reads_back_and_register_calls_linked(self):
        awkward = ["/opt/x", "a" + chr(92) + "b", 'say "hi"', "has " + self.TRI + " seq",
                   "tab" + chr(9) + "char", chr(92) + chr(92) + '"' + chr(92)]
        for value in awkward:
            with self.subTest(value=value):
                created, outcome, _ = codexconfig.register("", "srv", value, ["--a", value])
                self.assertEqual(outcome, "CREATED")
                view = codexconfig.scan(created)
                self.assertTrue(view.readable, view.unreadable)
                self.assertEqual(view.servers["srv"]["command"], value)
                self.assertEqual(view.servers["srv"]["args"], ["--a", value])
                # The round trip is the property, not the three examples: a value this
                # module wrote must never come back as a different registration.
                again, rerun, _ = codexconfig.register(created, "srv", value, ["--a", value])
                self.assertEqual(rerun, "LINKED")
                self.assertEqual(again, created)

    @needs_reader
    def test_a_literal_value_holding_the_fence_sequence_hides_nothing_after_it(self):
        text = ("command = " + chr(39) + "say " + self.TRI + " hi" + chr(39) + chr(10)
                + "[mcp_servers.x]" + chr(10) + 'command = "/opt/x"' + chr(10))
        view = codexconfig.scan(text)
        self.assertTrue(view.readable, view.unreadable)
        self.assertEqual(sorted(view.servers), ["x"])

    @needs_reader
    def test_a_member_assignment_inside_the_parent_table_is_never_duplicated(self):
        # Bare and quoted spellings both. The quoted one was blanked before the assignment
        # regex saw it, so the fallback read the server as absent and appended a second
        # definition; now it refuses, and tomllib reads it correctly.
        for spelling in ('x = { command = "/opt/x" }', '"x" = { command = "/opt/x" }'):
            with self.subTest(spelling):
                text = "[mcp_servers]" + chr(10) + spelling + chr(10)
                after, outcome, detail = codexconfig.register(text, "x", "/opt/x", [])
                self.assertIn(outcome, ("UNREADABLE", "LINKED"), detail)
                self.assertEqual(after, text)

    @needs_reader
    def test_an_escape_this_reader_does_not_model_is_reported_not_guessed(self):
        # Invalid TOML either way: tomllib rejects the escape and the fallback names it.
        text = '[mcp_servers.x]' + chr(10) + 'command = "a' + chr(92) + 'q"' + chr(10)
        self.assertFalse(codexconfig.scan(text).readable)

    # (b) nothing mutates before the inputs are complete -----------------------------

    def trial_args(self, **overrides):
        base = dict(issue="JUN-104", parent_task="parent", child_task="child",
                    recipient="parent", artifact_root=TRIAL_ROOT, artifact=[TRIAL_ARTIFACT],
                    dispatch_turn_id="anchor-1", recipient_settings="@/tmp/s.json",
                    turn_thread="child", turn_id="u", turn_status="completed",
                    settings_already_recorded=False, expect_relationship=None,
                    socket=None, state=None)
        base.update(overrides)
        return type("Args", (), base)()

    def run_trial(self, args):
        import runtime_install

        sent = []

        def fake_relay(command, **kwargs):
            sent.append(list(command))
            return {"ok": True, "payload": {}, "command": list(command)}

        original = runtime_install.scope.relay
        runtime_install.scope.relay = fake_relay
        try:
            with usable_settings():
                return runtime_install._trial(args, "/opt/relay", RELAY_RUNTIME), sent
        finally:
            runtime_install.scope.relay = original

    def test_an_incomplete_trial_sends_no_relay_command_at_all(self):
        for missing in ("artifact", "dispatch_turn_id", "turn_id", "recipient_settings"):
            with self.subTest(missing=missing):
                overrides = {missing: None}
                if missing == "recipient_settings":
                    overrides["settings_already_recorded"] = False
                result, sent = self.run_trial(self.trial_args(**overrides))
                self.assertEqual(result["value"], "not_verified")
                self.assertEqual(sent, [], "an incomplete trial must write nothing")

    def test_a_recipient_that_is_not_the_parent_is_refused_before_any_write(self):
        result, sent = self.run_trial(self.trial_args(recipient="somebody-else"))
        self.assertEqual(result["value"], "not_verified")
        self.assertIn("not the parent task", result["evidence"])
        self.assertEqual(sent, [])

    def test_recorded_settings_stay_reusable_through_an_explicit_claim(self):
        # The acknowledgement is the caller's, and it is not verified here. It exists so a
        # host whose settings are already authorized is not forced to resupply them.
        args = self.trial_args(recipient_settings=None, settings_already_recorded=True)
        result, sent = self.run_trial(args)
        self.assertNotEqual(sent, [], "the trial should proceed on the acknowledgement")
        self.assertNotIn("settings-record", [argv[0] for argv in sent])

    def test_the_assignment_lookup_runs_before_anything_is_written(self):
        _result, sent = self.run_trial(self.trial_args())
        self.assertEqual(sent[0][0], "assignment-find",
                         "a lookup after register could find what the trial itself wrote")

    def test_a_store_without_the_expected_relationship_stops_the_trial(self):
        result, sent = self.run_trial(self.trial_args(expect_relationship="rel-expected"))
        self.assertEqual(result["value"], "not_verified")
        self.assertIn("rel-expected", result["evidence"])
        self.assertEqual([argv[0] for argv in sent], ["assignment-find"])

    # (c) a point describes the bytes that ran ---------------------------------------

    def test_a_point_without_an_exercised_digest_never_qualifies(self):
        record = hostrecord.empty(1)
        hostrecord.add_point(record, "codex-session-relay", {
            "exercised": True, "install": "/env/pkg", "interpreter": "3.13.1",
            "definitionDigest": "abc",
        })
        self.assertEqual(hostrecord.points_for(
            record, "codex-session-relay", location="/env/pkg", interpreter="3.13.1",
            install_digest="abc", exercise_digest="an-instrument"), [], "a point recorded before this existed says nothing"
            " about which bytes ran")

    def test_measuring_refuses_when_the_interpreter_belongs_to_another_environment(self):
        import runtime_install

        record = hostrecord.empty(1)
        data = definition.load()
        original = runtime_install._interpreter_prefix
        runtime_install._interpreter_prefix = lambda python: "/envs/A"
        try:
            outcome = runtime_install.measure_candidate(
                data, record, python="/envs/A/bin/python", environment="/envs/B",
                socket_path=None, state=None, relay_command="/envs/B/bin/relay")
        finally:
            runtime_install._interpreter_prefix = original
        self.assertFalse(outcome["qualifyingPoint"])
        self.assertIn("/envs/B", outcome["refused"])
        self.assertEqual(outcome["operations"], [], "nothing is exercised before the refusal")

    def test_an_import_from_somewhere_other_than_the_recorded_install_is_refused(self):
        import runtime_install

        record = hostrecord.empty(1)
        data = definition.load()
        for component in data["components"]:
            hostrecord.put_install(record, component["component"], {
                "location": "/envs/A/lib/" + component["module"], "environment": "/envs/A"})
        original = runtime_install.module_location
        runtime_install.module_location = lambda python, module: ("/elsewhere/" + module, None, [])
        try:
            bound, mismatch = runtime_install._bind_installs(
                record, data, "/envs/A/bin/python", "/envs/A")
        finally:
            runtime_install.module_location = original
        self.assertIsNone(bound)
        self.assertIn("/elsewhere/", mismatch)

    # (f) recovery -------------------------------------------------------------------

    def test_the_environment_name_covers_every_component(self):
        # Derived from the first component alone, a relay-only change produced the same
        # directory and the existence check then refused to install it. Asserted as
        # behaviour: reading the source for a expression proves the expression is present,
        # not that the name changes when a component does.
        import runtime_install

        base = definition.load()
        names = set()
        with tempfile.TemporaryDirectory() as temporary:
            for digests in (("aa", "bb"), ("aa", "cc"), ("dd", "bb")):
                data = json.loads(json.dumps(base))
                for component, digest in zip(data["components"], digests):
                    component["sourceDigest"] = digest * 32
                args = argparse.Namespace(dest=temporary, apply=False, record=None,
                                          python=sys.executable, socket=None, state=None,
                                          issue=None, codex_home=temporary)
                with mock.patch.object(runtime_install.definition, "load", return_value=data), \
                     mock.patch.object(runtime_install.definition, "verify", return_value=[]), \
                     mock.patch.object(runtime_install, "emit",
                                       side_effect=lambda p: names.add(p["environment"])):
                    self.assertEqual(runtime_install.cmd_install(args), 0)
        self.assertEqual(len(names), 3,
                         "a change in any component must name a different environment")

    def test_a_failed_run_releases_only_the_directory_it_created(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            record_path = Path(temporary) / "record.json"
            record = hostrecord.empty(1)
            hostrecord.put_install(record, "codex-session-relay",
                                   {"location": "/c/pkg", "environment": str(Path(temporary) / "mine")})
            mine = Path(temporary) / "mine"
            mine.mkdir()
            theirs = Path(temporary) / "theirs"
            theirs.mkdir()

            record["selected"] = {"codex-session-relay": "/promoted-by-another-run"}
            hostrecord.save(record_path, record)

            code = runtime_install._install_failed(record_path, 1, [], str(mine), mine)
            self.assertEqual(code, 1)
            self.assertFalse(mine.exists(), "the destination this run created must be retryable")
            self.assertTrue(theirs.exists(), "a directory this run did not create is untouched")
            written = hostrecord.load(record_path, 1).value
            self.assertEqual(
                written["selected"], {"codex-session-relay": "/promoted-by-another-run"},
                "recovery leaves the selection as found; another run's promotion is not"
                " this run's to undo")
            self.assertEqual(
                written["components"]["codex-session-relay"]["installs"], [],
                "records for a removed candidate are dropped")



# =========================================================================================
# Check 1 - round-trip symmetry over names AND values, judged by an independent parser
# =========================================================================================

ADVERSARIAL_NAMES = [
    "plain", "dotted.name", 'has"quote', "has'literal", "back\\slash", "tab\there",
    "bracket[inside]", 'triple"""quote', "u2028\u2028sep", "nel\u0085sep", "space in name",
    "\u00e9\u4e2d", "equals=sign", "hash#mark",
]
ADVERSARIAL_VALUES = [
    "/usr/bin/plain", 'a"b', "a'b", "a\\b", "a\tb", 'a"""b', "a\u2028b", "a\u0085b",
    "[bracketed]", "# not a comment", "trailing\\", "\u00e9\u4e2d", "",
]


class RoundTripSymmetryTests(unittest.TestCase):
    """The reader and the writer agree, and tomllib is the judge of both.

    A chosen list of characters is still a list, and the two defects this closed were both
    outside whatever list came to mind: a quoted name containing a bracket, which the header
    recognizer rejected while tomllib accepted it, and a value containing U+2028, which
    str.splitlines tore in half. Generating the corpus and comparing against an independent
    parser is what makes this a property rather than a longer list.
    """

    @needs_reader
    def test_every_generated_registration_round_trips_and_agrees_with_tomllib(self):
        # The reader half runs everywhere, because the reader is meant to work on an
        # interpreter with no tomllib -- that is the whole premise of the module. The oracle
        # comparison runs where the oracle exists. Skipping the entire property on 3.10 would
        # leave the interpreter this repository checks itself with exercising none of it.
        try:
            import tomllib
        except ImportError:
            tomllib = None
        failures = []
        for name in ADVERSARIAL_NAMES:
            for value in ADVERSARIAL_VALUES:
                args = [value, "second"]
                text = codexconfig.render(name, value, args)
                view = codexconfig.scan(text)
                if not view.readable:
                    failures.append((name, value, "unreadable: " + "; ".join(view.unreadable)))
                    continue
                mine = view.servers.get(name) or {}
                if name not in view.servers:
                    failures.append((name, value, "the name was lost: "
                                     + repr(sorted(view.servers))))
                    continue
                if mine.get("command") != value:
                    failures.append((name, value, "command differs: "
                                     + repr(mine.get("command"))))
                    continue
                if list(mine.get("args") or []) != args:
                    failures.append((name, value, "args differ: " + repr(mine.get("args"))))
                    continue
                if tomllib is not None:
                    try:
                        parsed = tomllib.loads(text).get("mcp_servers", {})
                    except Exception as error:
                        failures.append((name, value, "tomllib rejected: " + repr(error)))
                        continue
                    theirs = parsed.get(name) or {}
                    if set(parsed) != set(view.servers):
                        failures.append((name, value, "names differ: "
                                         + repr(sorted(view.servers)) + " vs "
                                         + repr(sorted(parsed))))
                        continue
                    if theirs.get("command") != value:
                        failures.append((name, value, "the oracle read command "
                                         + repr(theirs.get("command"))))
                        continue
                    if list(theirs.get("args") or []) != args:
                        failures.append((name, value, "the oracle read args "
                                         + repr(theirs.get("args"))))
                        continue
                _, outcome, detail = codexconfig.register(text, name, value, args)
                if outcome != "LINKED":
                    failures.append((name, value, "rerun reported " + outcome + ": " + detail))
        self.assertEqual(failures, [], "write -> read must return the name and the value"
                                       " unchanged and a rerun must report LINKED")

    @needs_reader
    def test_a_dotted_name_is_a_server_rather_than_a_sub_table(self):
        text = codexconfig.render("codex.thread.bridge", "/usr/bin/bridge", [])
        self.assertIn('[mcp_servers."codex.thread.bridge"]', text)
        self.assertEqual(sorted(codexconfig.scan(text).servers), ["codex.thread.bridge"])

    @needs_reader
    def test_a_quoted_name_containing_a_bracket_is_read_rather_than_skipped(self):
        text = '[mcp_servers."a[b]"]\ncommand = "/bin/x"\n'
        view = codexconfig.scan(text)
        self.assertTrue(view.readable, view.unreadable)
        self.assertEqual(sorted(view.servers), ["a[b]"])

    @needs_reader
    def test_a_value_carrying_a_unicode_separator_is_not_torn_in_half(self):
        text = '[mcp_servers.one]\ncommand = "a\u2028b"\n\n[mcp_servers.two]\ncommand = "/x"\n'
        view = codexconfig.scan(text)
        self.assertTrue(view.readable, view.unreadable)
        self.assertEqual(sorted(view.servers), ["one", "two"])
        self.assertEqual(view.servers["one"]["command"], "a\u2028b")

    @needs_reader
    def test_an_escaped_quote_in_a_server_name_is_one_server(self):
        # The property a hand-written key splitter kept getting wrong, now the parser's.
        text = '[mcp_servers."a\\"b"]' + chr(10) + 'command = "/x"' + chr(10)
        view = codexconfig.scan(text)
        self.assertTrue(view.readable, view.unreadable)
        self.assertEqual(sorted(view.servers), ['a"b'])

    @needs_reader
    def test_the_oracle_comparison_covers_arguments_as_well_as_the_command(self):
        # The comparison decides LINKED against CONFLICT, and arguments are half of that.
        try:
            import tomllib  # noqa: F401
        except ImportError:
            # cross_check has no oracle to disagree with on this interpreter, so it reports
            # nothing by design. The 3.13 job is where this property is actually checked.
            self.skipTest("tomllib is the oracle for this property")
        source = '[mcp_servers.one]\ncommand = "/x"\nargs = ["a"]\n'
        view = codexconfig.scan(source)
        view.servers["one"]["args"] = ["b"]
        disagreement = codexconfig.cross_check(source, view)
        self.assertIsNotNone(disagreement)
        self.assertIn("args", disagreement)


# =========================================================================================
# Check 2 - identity decisions compare identifiers, never serialized text or path prefixes
# =========================================================================================

PREFIX_METHODS = {"startswith", "endswith"}
# The one place a prefix test is allowed to live. Everything else asks the question through a
# named helper, so an identity decision cannot be spelled as a string prefix by accident.
PREFIX_HELPER = "text_prefix"


def _raw_prefix_tests(tree):
    """Prefix tests written directly rather than through the textual helper.

    A membership test against serialized text and a prefix test against a path are the same
    defect in two spellings, and the first scanner could only see one of them. This is a
    syntactic guard and says so: it cannot see a membership test between two stringified
    paths, slicing, or an unsafe comparison routed through the helper. It is paired with the
    behavioural sibling-path cases at the real identity decisions, which are what actually
    protect the property.
    """
    found = []
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        called = node.func
        if isinstance(called, ast.Attribute) and called.attr in PREFIX_METHODS:
            found.append(called.attr + " at line " + str(node.lineno))
    return found


def _substring_identity_comparisons(tree):
    """Membership tests whose right-hand side is a serialized payload."""
    found = set()
    for scope_node in ast.walk(tree):
        if not isinstance(scope_node, (ast.Module, ast.FunctionDef, ast.AsyncFunctionDef)):
            continue
        serialized = set()
        for node in ast.walk(scope_node):
            if isinstance(node, ast.Assign) and isinstance(node.value, ast.Call):
                called = node.value.func
                if isinstance(called, ast.Attribute) and called.attr == "dumps":
                    for target in node.targets:
                        if isinstance(target, ast.Name):
                            serialized.add(target.id)
        for node in ast.walk(scope_node):
            if not isinstance(node, ast.Compare):
                continue
            if not any(isinstance(op, (ast.In, ast.NotIn)) for op in node.ops):
                continue
            for comparator in node.comparators:
                if (isinstance(comparator, ast.Call)
                        and isinstance(comparator.func, ast.Attribute)
                        and comparator.func.attr == "dumps"):
                    found.add(node.lineno)
                if isinstance(comparator, ast.Name) and comparator.id in serialized:
                    found.add(node.lineno)
    return sorted(found)


def _source_modules():
    paths = [ROOT / "scripts" / "runtime_install.py"]
    return paths + sorted((ROOT / "scripts" / "crw_runtime").glob("*.py"))


class IdentityComparisonTests(unittest.TestCase):
    """One inventory over the decisions, plus one case per dimension each must not ignore.

    The guard that started this compared an expected relationship against the whole
    serialized lookup answer, so an archived assignment sitting anywhere in the payload read
    as agreement. Two more of the same shape were path prefixes. They are one class: an
    identity compared by something wider than the identity itself.
    """

    def test_no_identifier_is_compared_against_a_serialized_payload(self):
        offenders = []
        for path in _source_modules():
            lines = _substring_identity_comparisons(ast.parse(path.read_text(encoding="utf-8")))
            offenders += [str(path.relative_to(ROOT)) + ":" + str(line) for line in lines]
        self.assertEqual(offenders, [], "compare the field, not the serialized answer")

    def test_environment_membership_rejects_a_sibling_sharing_a_prefix(self):
        import runtime_install

        self.assertTrue(runtime_install.within(Path("/opt/env/lib/pkg"), Path("/opt/env")))
        self.assertTrue(runtime_install.within(Path("/opt/env"), Path("/opt/env")))
        self.assertFalse(runtime_install.within(Path("/opt/env-other/lib"), Path("/opt/env")))

    def test_a_recorded_root_rejects_a_sibling_sharing_a_prefix(self):
        import runtime_install

        record = hostrecord.empty(1)
        hostrecord.put_install(record, "codex-session-relay",
                               {"location": "/opt/env/pkg", "environment": "/opt/env"})
        roots = runtime_install.recorded_roots(record, "codex-session-relay")
        self.assertTrue(any(runtime_install.within(Path("/opt/env/bin/x"), r) for r in roots))
        self.assertFalse(any(runtime_install.within(Path("/opt/env-other/bin/x"), r)
                             for r in roots))

    def test_an_identical_hook_under_another_matcher_is_not_already_installed(self):
        document = {"hooks": {"Stop": [{"matcher": "other", "hooks": [{"command": "/bin/x"}]}]}}
        entries = hooks.inventory(document, "Stop")
        self.assertEqual([e["matcher"] for e in entries], ["other"])
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "hooks.json"
            path.write_text(json.dumps(document), encoding="utf-8")
            result = hooks.install(path, "Stop", {"command": "/bin/x"}, issue="JUN-104",
                                   apply=True)
        self.assertEqual(result["outcome"], "CREATED",
                         "a matcher-less group is a different registration from a matched one")

    def test_an_archived_relationship_elsewhere_in_the_payload_is_refused(self):
        import runtime_install

        payload = {"issueKey": "JUN-104", "responsibleRelationship": None,
                   "assignments": [{"relationshipId": "rel-archived", "state": "closed"}]}
        args = argparse.Namespace(
            issue="JUN-104", parent_task="p", child_task="c", recipient="p",
            artifact_root=TRIAL_ROOT, turn_thread="c", turn_id="ti", artifact=[TRIAL_ARTIFACT],
            dispatch_turn_id="d", turn_status="completed", recipient_settings=None,
            settings_already_recorded=True, expect_relationship="rel-archived",
            socket=None, state=None)
        with mock.patch.object(runtime_install.scope, "relay",
                               return_value={"ok": True, "payload": payload,
                                             "command": ["assignment-find"]}):
            result = runtime_install._trial(args, "/usr/bin/relay", RELAY_RUNTIME)
        self.assertEqual(result["value"], "not_verified")
        self.assertIn("responsible relationship", result["evidence"])


    def test_an_issue_owned_by_another_child_stops_the_trial_before_any_write(self):
        import runtime_install

        payload = {"issueKey": "JUN-104", "responsibleRelationship": "rel-old",
                   "responsibleChild": "some-other-child"}
        args = argparse.Namespace(
            issue="JUN-104", parent_task="p", child_task="c", recipient="p",
            artifact_root=TRIAL_ROOT, turn_thread="c", turn_id="ti", artifact=[TRIAL_ARTIFACT],
            dispatch_turn_id="d", turn_status="completed", recipient_settings=None,
            settings_already_recorded=True, expect_relationship=None, socket=None, state=None)
        sent = []

        def relay(command, **kwargs):
            sent.append(command[0])
            return {"ok": True, "command": list(command), "payload": payload}

        with mock.patch.object(runtime_install.scope, "relay", side_effect=relay):
            result = runtime_install._trial(args, "/usr/bin/relay", RELAY_RUNTIME)
        self.assertEqual(result["value"], "not_verified")
        self.assertIn("already belongs to child", result["evidence"])
        self.assertIn("childTaskId", result["evidence"],
                      "the refusal names which identity field differs, not merely that one does")
        self.assertEqual(sent, ["assignment-find"],
                         "the lookup is the pre-mutation check, so nothing runs after it")

    def test_an_ordinary_repeat_by_the_same_child_still_proceeds(self):
        # Replay is the identity the trial would register, not an optional flag. Gating on the
        # flag would have broken the repeat case this branch already established.
        import runtime_install

        payload = {"issueKey": "JUN-104", "responsibleRelationship": "rel-1",
                   "responsibleChild": "c"}
        args = argparse.Namespace(
            issue="JUN-104", parent_task="p", child_task="c", recipient="p",
            artifact_root=TRIAL_ROOT, turn_thread="c", turn_id="ti", artifact=[TRIAL_ARTIFACT],
            dispatch_turn_id="d", turn_status="completed", recipient_settings=None,
            settings_already_recorded=True, expect_relationship=None, socket=None, state=None)
        sent = []

        def relay(command, **kwargs):
            sent.append(command[0])
            return {"ok": True, "command": list(command),
                    "payload": payload if command[0] == "assignment-find"
                    else _trial_payload(command[0])}

        with mock.patch.object(runtime_install.scope, "relay", side_effect=relay):
            result = runtime_install._trial(args, "/usr/bin/relay", RELAY_RUNTIME)
        self.assertEqual(result["value"], "verified", result["evidence"][:300])
        self.assertIn("deliver", sent)



# =========================================================================================
# Check 3 - every write to a host-owned file happens under the lock that guards it
# =========================================================================================

WRITE_CALLS = {"save", "atomic_write", "write_text", "write_bytes"}


def _lock_target(item):
    """The expression a with-Locked is guarding, or None if it is not a lock.

    Returning the TARGET rather than a boolean is the fix: a lock taken on some other path
    satisfies "a lock appears" while guarding nothing relevant.
    """
    call = item.context_expr
    if not isinstance(call, ast.Call):
        return None
    called = call.func
    name = called.attr if isinstance(called, ast.Attribute) else getattr(called, "id", None)
    if name != "Locked" or not call.args:
        return None
    return ast.dump(call.args[0])


def _write_target(node):
    """The path a write is aimed at: its first argument, or the receiver of a Path method."""
    called = node.func
    if isinstance(called, ast.Attribute) and called.attr in ("write_text", "write_bytes"):
        return ast.dump(called.value)
    return ast.dump(node.args[0]) if node.args else None


def _unguarded_writes(tree):
    found = []

    def walk(node, held):
        for child in ast.iter_child_nodes(node):
            if isinstance(child, ast.With):
                targets = [t for t in (_lock_target(i) for i in child.items) if t]
                for item in child.items:
                    walk(item.context_expr, held)
                for statement in child.body:
                    walk(statement, held + targets)
                continue
            if isinstance(child, ast.Call):
                called = child.func
                name = (called.attr if isinstance(called, ast.Attribute)
                        else getattr(called, "id", None))
                if name in WRITE_CALLS:
                    # The lock has to be on the thing being written. A lock held over some
                    # other path is not a guard, it is decoration that passes a lexical test.
                    if _write_target(child) not in held:
                        found.append(name + " at line " + str(child.lineno))
            walk(child, held)

    walk(tree, [])
    return found


class LockCoverageTests(unittest.TestCase):
    """The inventory is writes to host-owned files; the property is that each is guarded.

    A lexical check would pass a load that happened before the lock, so the host record has
    exactly one writer: a helper that loads inside the lock and applies the caller's delta to
    what it finds there. The inventory then only has to establish that nothing else writes.
    """

    def test_no_host_owned_file_is_written_outside_a_lock(self):
        offenders = []
        for path in _source_modules():
            for finding in _unguarded_writes(ast.parse(path.read_text(encoding="utf-8"))):
                offenders.append(str(path.relative_to(ROOT)) + ": " + finding)
        self.assertEqual(offenders, [], "every write goes through a locked read-modify-write")

    def test_the_helper_refuses_a_whole_record_and_takes_deltas_only(self):
        import inspect

        accepted = set(inspect.signature(hostrecord.update).parameters)
        self.assertNotIn("record", accepted,
                         "a caller handing back a record it loaded before slow work is the"
                         " staleness this helper exists to prevent")
        self.assertTrue({"installs", "points", "select", "drop_environment"} <= accepted)

    def test_a_promotion_committed_during_a_slow_run_survives_that_run_failing(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "record.json"
            record = hostrecord.empty(1)
            hostrecord.put_install(record, "codex-session-relay",
                                   {"location": "/a/pkg", "environment": "/a"})
            hostrecord.save(path, record)

            # A stages into /a; B promotes and appends a point; then A fails.
            hostrecord.update(path, 1, select={"codex-thread-bridge": "/b/pkg"},
                              points=[("codex-thread-bridge", {"exercised": True, "n": 1})])
            with mock.patch.object(runtime_install, "emit"):
                runtime_install._install_failed(path, 1, [], "/a", None)

            after = hostrecord.load(path, 1).value
        self.assertEqual(after["selected"], {"codex-thread-bridge": "/b/pkg"},
                         "B's promotion survives A's recovery")
        self.assertEqual(
            len(after["components"]["codex-thread-bridge"]["measuredPoints"]), 1,
            "B's point survives A's recovery")
        self.assertEqual(after["components"]["codex-session-relay"]["installs"], [],
                         "A drops only the installs it created")

    def test_a_point_appended_during_a_measurement_is_not_lost_by_its_commit(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "record.json"
            hostrecord.save(path, hostrecord.empty(1))
            # A reads here, then measures slowly. B appends in the meantime.
            hostrecord.update(path, 1, points=[("codex-session-relay",
                                                {"exercised": True, "who": "B"})])
            # A commits what IT measured, as a delta rather than as a whole record.
            hostrecord.update(path, 1, points=[("codex-session-relay",
                                                {"exercised": True, "who": "A"})])
            entry = hostrecord.load(path, 1).value["components"]["codex-session-relay"]
        self.assertEqual([p["who"] for p in entry["measuredPoints"]], ["B", "A"])


# =========================================================================================
# Check 4 - the reading boundary: a record that cannot be read is an answer, not a crash
# =========================================================================================

MALFORMED_RECORD = '{"components": {"codex-session-relay": {"installs": "not a list"}}}'


def _cli_entry_points():
    """Derived from the parser's own registrations, not from a list kept by hand.

    A hand-kept list cannot notice a command somebody adds without a boundary, which is the
    failure this inventory exists to catch. The same technique the relay argv check uses.
    """
    import runtime_install

    parser = runtime_install.build_parser()
    found = {}
    for action in parser._actions:
        if isinstance(action, argparse._SubParsersAction):
            for name, sub in action.choices.items():
                found[name] = sub.get_default("handler")
    return found


class ReadingBoundaryTests(unittest.TestCase):
    """What the boundary guarantees, and what it deliberately does not.

    It guarantees the worst case is a named refusal rather than a traceback, and that a
    refusal says which exception and which source location produced it, so a defect reaching
    it stays a reportable defect instead of being filed as bad data. It does not guarantee
    that a record which could have been read is never refused; that residue is conservative
    and carries its reason. It is stated in docs/runtime-install.md rather than implied.
    """

    def test_every_registered_command_has_a_boundary_fixture(self):
        self.assertEqual(
            sorted(_cli_entry_points()),
            ["diagnose", "hook", "hook-status", "install", "measure", "register-mcp",
             "verify-definition"],
            "a command added without a boundary fixture fails this check")

    def _assert_named_refusal(self, done, where):
        self.assertEqual(done.returncode, 1, where + ": " + done.stdout + done.stderr)
        self.assertNotIn("Traceback", done.stderr, where + " raised instead of refusing")
        payload = json.loads(done.stdout)
        self.assertIn(payload["reading"]["state"], (reading.UNREADABLE, reading.ACCESS_ERROR))
        self.assertTrue(payload["reading"]["exception"], where + " named no exception")
        self.assertTrue(payload["reading"]["raisedAt"], where + " named no source location")
        return payload

    def test_a_malformed_host_record_refuses_in_every_command_that_reads_one(self):
        with tempfile.TemporaryDirectory() as temporary:
            record = Path(temporary) / "record.json"
            record.write_text(MALFORMED_RECORD, encoding="utf-8")
            home = Path(temporary) / "home"
            home.mkdir()
            # A command that would WRITE refuses outright. A read-only diagnosis reports the
            # failed reading beside everything else it could still observe, because refusing
            # the whole diagnosis would discard the readings that did answer. Neither of them
            # may read an unreadable record as a clean host, and neither may write.
            for command, extra in (("install", ["--dest", str(Path(temporary) / "dest")]),
                                   ("measure", [])):
                done = run(command, "--record", str(record), *extra)
                self._assert_named_refusal(done, command)
                self.assertEqual(record.read_text(encoding="utf-8"), MALFORMED_RECORD,
                                 command + " wrote to a record it could not read")

            done = run("diagnose", "--record", str(record), "--codex-home", str(home))
            self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
            self.assertNotIn("Traceback", done.stderr, "diagnose raised instead of reporting")
            payload = json.loads(done.stdout)
            self.assertEqual(payload["hostRecordState"], reading.UNREADABLE)
            self.assertTrue(payload["hostRecordReading"]["exception"])
            self.assertTrue(payload["hostRecordReading"]["raisedAt"])
            for entry in payload["components"].values():
                self.assertEqual(entry["class"], "unreadable")
                self.assertTrue(any("host record" in reason for reason in entry["reasons"]),
                                "a classification may never read an unreadable record as"
                                " a host with no history: " + json.dumps(entry["reasons"]))
            self.assertEqual(record.read_text(encoding="utf-8"), MALFORMED_RECORD)

    def test_a_configuration_that_is_not_text_refuses_rather_than_raising(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            config = home / "config.toml"
            config.write_bytes(b"[mcp_servers.one]\ncommand = \"\xff\xfe\"\n")
            before = config.read_bytes()
            done = run("register-mcp", "--codex-home", str(home),
                       "--bridge-command", "/usr/bin/bridge", "--apply")
            self._assert_named_refusal(done, "register-mcp")
            self.assertEqual(config.read_bytes(), before,
                             "a refusal before the first mutating step writes nothing")

    def test_a_hook_file_of_the_wrong_shape_refuses_rather_than_raising(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            document = home / "hooks.json"
            document.write_text('{"hooks": {"Stop": "not a list"}}', encoding="utf-8")
            before = document.read_bytes()
            done = run("hook", "--codex-home", str(home), "--event", "Stop",
                       "--hook-command", "/bin/true", "--apply")
            self.assertEqual(done.returncode, 1, done.stdout + done.stderr)
            self.assertNotIn("Traceback", done.stderr)
            result = json.loads(done.stdout)["result"]
            self.assertIn(result["outcome"], (reading.UNREADABLE, reading.ACCESS_ERROR))
            self.assertFalse(result["wrote"])
            self.assertEqual(document.read_bytes(), before,
                             "a refusal before the first mutating step writes nothing")

    def test_a_malformed_definition_refuses_in_verify_definition(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            broken = Path(temporary) / "components.json"
            broken.write_text("{not json", encoding="utf-8")
            emitted = []
            with mock.patch.object(runtime_install.definition, "DEFINITION_PATH", broken), \
                 mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_verify_definition(argparse.Namespace())
        self.assertEqual(code, 1)
        self.assertEqual(emitted[0]["reading"]["state"], reading.UNREADABLE)
        self.assertEqual(emitted[0]["reading"]["exception"], "JSONDecodeError")

    def test_the_reader_apis_refuse_directly_as_well_as_through_a_handler(self):
        # The registration inventory proves a command exists; it cannot prove the boundary
        # sits inside it. Each reader is therefore exercised on its own.
        with tempfile.TemporaryDirectory() as temporary:
            record = Path(temporary) / "record.json"
            record.write_text(MALFORMED_RECORD, encoding="utf-8")
            self.assertEqual(hostrecord.load(record, 1).state, reading.UNREADABLE)

            document = Path(temporary) / "hooks.json"
            document.write_text('{"hooks": []}', encoding="utf-8")
            self.assertEqual(hooks.read(document).state, reading.UNREADABLE)

            config = Path(temporary) / "config.toml"
            config.write_bytes(b"\xff\xfe")
            self.assertEqual(
                reading.read_text(config, "the Codex configuration").state, reading.UNREADABLE)

            missing = Path(temporary) / "components.json"
            with self.assertRaises(reading.Refused):
                with reading.region(missing, "the component definition"):
                    definition.load(missing)

    def test_an_ordinary_defect_outside_a_reading_region_is_not_disguised_as_a_refusal(self):
        # The catch set is wide because the lattice made it wide. That only stays honest
        # while the region stays narrow: a ValueError from assembling a result is a defect in
        # this command and must keep raising rather than being reported as a bad record.
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(
                codex_home=str(home), record=str(home / "record.json"), socket=None,
                state=None, issue=None, relay_command=None, bridge_command=None,
                bridge_arg=None, observed_tool=None, trial=False, assignment_lookup=False,
                temporary=True, parent_task=None, child_task=None, recipient=None,
                artifact_root=None, turn_thread=None, turn_id=None, artifact=None,
                dispatch_turn_id=None, turn_status="completed", recipient_settings=None,
                settings_already_recorded=False, expect_relationship=None)
            for error in (ValueError("a defect, not a record"), KeyError("absent")):
                with mock.patch.object(runtime_install.check, "record", side_effect=error):
                    with self.assertRaises(type(error)):
                        runtime_install.cmd_diagnose(args)


class FilesystemPartitionTests(unittest.TestCase):
    """Four states, ordered over one observation, with nothing left unclassified.

    ABSENT is only established absence. Anything that failed to establish anything is an
    access error, including a symlink whose target cannot be resolved: that is neither a
    dangling link nor a loop, and calling it unreadable would report a permission problem as
    a malformed record.
    """

    def _state(self, path):
        return hostrecord.load(path, 1).state

    def test_a_missing_path_is_absent_and_carries_a_usable_empty_record(self):
        with tempfile.TemporaryDirectory() as temporary:
            found = hostrecord.load(Path(temporary) / "nothing.json", 1)
        self.assertEqual(found.state, reading.ABSENT)
        self.assertTrue(found.usable)
        self.assertEqual(found.value["components"], {})

    def test_an_existing_empty_record_is_present_rather_than_absent(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "record.json"
            hostrecord.save(path, hostrecord.empty(1))
            found = hostrecord.load(path, 1)
        self.assertEqual(found.state, reading.PRESENT)

    def test_a_directory_is_unreadable_rather_than_absent(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "record.json"
            path.mkdir()
            found = hostrecord.load(path, 1)
        self.assertEqual(found.state, reading.UNREADABLE)
        self.assertIn("directory", found.detail)

    def test_a_named_pipe_is_unreadable_rather_than_absent(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "record.json"
            os.mkfifo(path)
            self.assertEqual(self._state(path), reading.UNREADABLE)

    def test_a_dangling_symlink_is_unreadable_rather_than_absent(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "record.json"
            path.symlink_to(Path(temporary) / "gone.json")
            found = hostrecord.load(path, 1)
        self.assertEqual(found.state, reading.UNREADABLE)
        self.assertIn("target does not exist", found.detail)

    def test_a_symlink_loop_is_unreadable_rather_than_an_access_error(self):
        with tempfile.TemporaryDirectory() as temporary:
            first, second = Path(temporary) / "a.json", Path(temporary) / "b.json"
            first.symlink_to(second)
            second.symlink_to(first)
            found = hostrecord.load(first, 1)
        self.assertEqual(found.state, reading.UNREADABLE)
        self.assertIn("loops", found.detail)

    def test_a_symlink_whose_target_cannot_be_resolved_is_an_access_error(self):
        if os.geteuid() == 0:
            self.skipTest("permissions do not restrict root")
        with tempfile.TemporaryDirectory() as temporary:
            closed = Path(temporary) / "closed"
            closed.mkdir()
            (closed / "record.json").write_text("{}", encoding="utf-8")
            link = Path(temporary) / "record.json"
            link.symlink_to(closed / "record.json")
            closed.chmod(0o000)
            try:
                found = hostrecord.load(link, 1)
            finally:
                closed.chmod(0o700)
        self.assertEqual(found.state, reading.ACCESS_ERROR)
        self.assertIn("could not be resolved", found.detail)

    def test_a_path_under_an_unreadable_directory_is_an_access_error_not_an_absence(self):
        if os.geteuid() == 0:
            self.skipTest("permissions do not restrict root")
        with tempfile.TemporaryDirectory() as temporary:
            closed = Path(temporary) / "closed"
            closed.mkdir()
            closed.chmod(0o000)
            try:
                found = hostrecord.load(closed / "record.json", 1)
            finally:
                closed.chmod(0o700)
        self.assertEqual(found.state, reading.ACCESS_ERROR)
        self.assertFalse(found.usable, "an unestablished absence is never a clean host")

    def test_a_file_that_cannot_be_opened_is_an_access_error(self):
        if os.geteuid() == 0:
            self.skipTest("permissions do not restrict root")
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "record.json"
            path.write_text("{}", encoding="utf-8")
            path.chmod(0o000)
            try:
                found = hostrecord.load(path, 1)
            finally:
                path.chmod(0o600)
        self.assertEqual(found.state, reading.ACCESS_ERROR)
        self.assertEqual(found.exception, "PermissionError")

    def test_an_empty_file_and_a_wrong_shape_are_both_unreadable(self):
        with tempfile.TemporaryDirectory() as temporary:
            empty = Path(temporary) / "empty.json"
            empty.write_text("", encoding="utf-8")
            self.assertEqual(self._state(empty), reading.UNREADABLE)

            shaped = Path(temporary) / "shaped.json"
            shaped.write_text('{"components": {"relay": {"installs": [42]}}}', encoding="utf-8")
            found = hostrecord.load(shaped, 1)
        self.assertEqual(found.state, reading.UNREADABLE)
        self.assertEqual(found.exception, "TypeError")

    def test_a_null_bearing_recorded_path_refuses_instead_of_raising(self):
        import runtime_install

        record = hostrecord.empty(1)
        hostrecord.put_install(record, "codex-session-relay",
                               {"location": "/a/pkg", "environment": "/a\u0000b"})
        with self.assertRaises(reading.Refused) as caught:
            runtime_install.recorded_roots(record, "codex-session-relay")
        self.assertEqual(caught.exception.reading.state, reading.UNREADABLE)
        self.assertEqual(caught.exception.reading.exception, "ValueError")

    def test_the_four_states_are_reached_by_four_different_observations(self):
        # Asserting that four labels differ proves nothing about which path reaches which.
        if os.geteuid() == 0:
            self.skipTest("permissions do not restrict root")
        with tempfile.TemporaryDirectory() as temporary:
            present = Path(temporary) / "present.json"
            hostrecord.save(present, hostrecord.empty(1))
            broken = Path(temporary) / "broken.json"
            broken.write_text("{not json", encoding="utf-8")
            closed = Path(temporary) / "closed"
            closed.mkdir()
            closed.chmod(0o000)
            try:
                observed = {
                    reading.PRESENT: self._state(present),
                    reading.ABSENT: self._state(Path(temporary) / "missing.json"),
                    reading.UNREADABLE: self._state(broken),
                    reading.ACCESS_ERROR: self._state(closed / "record.json"),
                }
            finally:
                closed.chmod(0o700)
        for expected, got in observed.items():
            self.assertEqual(expected, got)
        self.assertEqual(len(set(observed.values())), 4)


class ServiceStateTests(unittest.TestCase):
    """The fourth outcome is about a daemon, and it must not be reachable by failing to ask."""

    def test_a_failed_invocation_is_an_access_error_even_with_no_payload(self):
        # The overlapping envelope: ok false AND payload missing. The invocation wins,
        # because a command that did not run says nothing about the daemon.
        envelope = {"ok": False, "command": ["relay", "service", "status"], "payload": None,
                    "unreadable": "FileNotFoundError: no such executable"}
        self.assertEqual(scope.service_state(envelope)["state"], reading.ACCESS_ERROR)

    def test_an_answer_with_no_readable_status_is_unreadable(self):
        self.assertEqual(
            scope.service_state({"ok": True, "payload": None})["state"], reading.UNREADABLE)
        self.assertEqual(
            scope.service_state({"ok": True, "payload": {"running": "yes"}})["state"],
            reading.UNREADABLE)

    def test_a_daemon_that_answered_is_running_or_stopped(self):
        self.assertEqual(scope.service_state({"ok": True, "payload": {"running": False}}),
                         {"state": scope.STOPPED, "running": False,
                          "detail": "the service answered and reports itself not running"})
        self.assertEqual(
            scope.service_state({"ok": True, "payload": {"running": True}})["state"],
            scope.RUNNING)

    def test_the_four_service_answers_are_four_different_values(self):
        states = {
            scope.service_state({"ok": False, "payload": None})["state"],
            scope.service_state({"ok": True, "payload": None})["state"],
            scope.service_state({"ok": True, "payload": {"running": False}})["state"],
            scope.service_state({"ok": True, "payload": {"running": True}})["state"],
        }
        self.assertEqual(len(states), 4)


class PartialApplicationTests(unittest.TestCase):
    """A write that landed is never reported as a refusal that wrote nothing.

    The hash comparison establishes that the TARGET file's bytes are unchanged. A lock file
    is created and removed beside it, and that is said rather than implied away.
    """

    def test_a_hook_that_was_appended_and_could_not_be_read_back_says_so_and_exits_nonzero(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            (home / "hooks.json").write_text('{"hooks": {}}', encoding="utf-8")
            real, calls = hooks.read, []

            def flaky(path):
                calls.append(path)
                if len(calls) >= 3:
                    return reading.Reading(state=reading.UNREADABLE, source=path,
                                           exception="TypeError", at="hooks.py:1",
                                           detail="the readback could not be read")
                return real(path)

            emitted = []
            args = argparse.Namespace(codex_home=str(home), event="Stop",
                                      hook_command="/bin/true", timeout=5, issue="JUN-104",
                                      apply=True)
            with mock.patch.object(runtime_install.hooks, "read", side_effect=flaky), \
                 mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_hook(args)
            written = json.loads((home / "hooks.json").read_text(encoding="utf-8"))

        self.assertEqual(code, 1, "a partial application is never a success")
        result = emitted[0]["result"]
        self.assertEqual(result["outcome"], "APPLIED_UNVERIFIED")
        self.assertTrue(result["applied"])
        self.assertTrue(result["wrote"])
        self.assertFalse(result["readBack"])
        self.assertEqual(len(written["hooks"]["Stop"]), 1,
                         "the append really did land, which is why it is reported")

    @needs_reader
    def test_a_registration_that_was_written_and_could_not_be_read_back_exits_nonzero(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            (home / "config.toml").write_text("[other]\nkeep = true\n", encoding="utf-8")
            real, calls = reading.read_text, []

            def flaky(path, what, **kwargs):
                calls.append(path)
                if len(calls) >= 3:
                    return reading.Reading(state=reading.UNREADABLE, source=path,
                                           exception="UnicodeDecodeError", at="reading.py:1",
                                           detail="the readback could not be decoded")
                return real(path, what, **kwargs)

            emitted = []
            args = argparse.Namespace(codex_home=str(home), name="codex-thread-bridge",
                                      bridge_command="/usr/bin/bridge", bridge_arg=None,
                                      apply=True)
            with mock.patch.object(runtime_install.reading, "read_text", side_effect=flaky), \
                 mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_register_mcp(args)
            after = (home / "config.toml").read_text(encoding="utf-8")

        self.assertEqual(code, 1, "exit 0 here would record a registration nobody read back")
        self.assertEqual(emitted[0]["outcome"], "APPLIED_UNVERIFIED")
        self.assertTrue(emitted[0]["wrote"])
        self.assertIn("[mcp_servers.codex-thread-bridge]", after)


# =========================================================================================
# The remaining authorized repairs, each as the behaviour it restores
# =========================================================================================

class TrialIdentityTests(unittest.TestCase):
    def test_a_dispatch_request_id_is_stable_per_dispatch_and_distinct_across_dispatches(self):
        import runtime_install

        first = runtime_install.trial_request_id("JUN-104", "turn-a")
        self.assertEqual(first, runtime_install.trial_request_id("JUN-104", "turn-a"),
                         "a retry of one dispatch must replay, not open a second generation")
        self.assertNotEqual(first, runtime_install.trial_request_id("JUN-104", "turn-b"),
                            "a second dispatch keyed on the issue alone replays the first"
                            " generation, and the bind of a new anchor is then refused")
        self.assertNotEqual(first, runtime_install.trial_request_id("JUN-105", "turn-a"))

    def test_the_request_id_reaches_the_argv_the_trial_sends(self):
        import runtime_install

        steps = runtime_install.trial_steps(
            issue="JUN-104", parent_task="p", child_task="c", recipient="p",
            artifact_root="/tmp", turn_thread="t", turn_id="ti", host="h",
            artifacts=["/tmp/a"], dispatch_turn_id="turn-a")
        expected = runtime_install.trial_request_id("JUN-104", "turn-a")
        for argv in steps:
            if "--dispatch-request-id" in argv:
                self.assertEqual(argv[argv.index("--dispatch-request-id") + 1], expected)


class InstallOrderTests(unittest.TestCase):
    def test_a_refused_destination_leaves_the_record_untouched(self):
        # The outgoing reading was saved before the exclusive mkdir proved this run owns the
        # destination, so a run that was about to be refused had already written.
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "dest"
            data = definition.load()
            combined = __import__("hashlib").sha256(
                "".join(c["sourceDigest"] for c in data["components"]).encode()).hexdigest()[:12]
            environment = destination / ("env-" + str(data["definitionVersion"]) + "-" + combined)
            environment.mkdir(parents=True)
            # Somebody else's files, and no claim from this command. An EMPTY directory is a
            # different case now: it carries nothing to lose, so it is reclaimed rather than
            # refused, which is what stops a hard-killed run refusing its own destination for
            # ever. What must still be refused, and refused before anything is written, is a
            # directory holding work this command cannot account for.
            (environment / "somebody-elses-file").write_text("not ours", encoding="utf-8")

            record_path = Path(temporary) / "record.json"
            hostrecord.save(record_path, hostrecord.empty(1))
            before = record_path.read_bytes()

            done = run("install", "--dest", str(destination), "--record", str(record_path),
                       "--apply")
            self.assertEqual(done.returncode, 1, done.stdout + done.stderr)
            payload = json.loads(done.stdout)
            self.assertEqual(payload["stagingDecision"], staging.FOREIGN)
            self.assertIn("belongs to somebody else", payload["refused"])
            self.assertEqual(record_path.read_bytes(), before,
                             "a run refused for want of a destination writes nothing")

    def test_the_outgoing_runtime_is_read_before_anything_stages_over_it(self):
        import runtime_install

        record = hostrecord.empty(1)
        record["selected"] = {"codex-session-relay": "/gone/pkg"}
        observed = runtime_install._outgoing_runtime(record, definition.load())
        self.assertEqual(observed["codex-session-relay"]["selected"], "/gone/pkg")
        self.assertFalse(observed["codex-session-relay"]["present"])
        self.assertIsNone(observed["codex-session-relay"]["digest"])

    def test_an_install_mode_is_decided_by_containment_not_by_a_shared_prefix(self):
        import runtime_install

        self.assertFalse(
            runtime_install.within(Path("/opt/env-other/lib/pkg"), Path("/opt/env")),
            "a neighbouring environment's install is not this environment's copy")
        self.assertTrue(
            runtime_install.within(Path("/opt/env/lib/python3/site-packages/pkg"),
                                   Path("/opt/env")))




# =========================================================================================
# Check 5 - a failing step stops the trial, for every step the trial can send
# =========================================================================================

class TrialStepGatingTests(unittest.TestCase):
    """The inventory is the trial's own step list; the property is that a failure stops it.

    This is the sibling that the first four checks did not cover. Six of the seven steps had
    their result checked and assignment-find did not, so a lookup that never ran still let
    settings-record and register write rows into a store this process could not read. The
    check is driven from trial_steps rather than a list here, so a step added without a guard
    fails it.
    """

    def _args(self):
        return argparse.Namespace(
            issue="JUN-104", parent_task="p", child_task="c", recipient="p",
            artifact_root=TRIAL_ROOT, turn_thread="c", turn_id="ti", artifact=[TRIAL_ARTIFACT],
            dispatch_turn_id="d", turn_status="completed", recipient_settings="@/tmp/s.json",
            settings_already_recorded=True, expect_relationship=None, socket=None, state=None)

    def _steps(self):
        import runtime_install

        args = self._args()
        return [argv[0] for argv in runtime_install.trial_steps(
            issue=args.issue, parent_task=args.parent_task, child_task=args.child_task,
            recipient=args.recipient, artifact_root=args.artifact_root,
            turn_thread=args.turn_thread, turn_id=args.turn_id, host="h",
            artifacts=args.artifact, dispatch_turn_id=args.dispatch_turn_id,
            turn_status=args.turn_status, recipient_settings=args.recipient_settings)]

    def test_the_inventory_is_the_trials_own_steps(self):
        self.assertEqual(
            self._steps(),
            ["assignment-find", "register", "settings-record", "generation-open",
             "generation-bind", "admit-turn", "emit", "deliver"])

    def test_a_failure_at_any_step_stops_the_trial_at_that_step(self):
        import runtime_install

        steps = self._steps()
        for index, failing in enumerate(steps):
            performed = []

            def relay(command, **kwargs):
                name = command[0]
                performed.append(name)
                if name == failing:
                    return {"ok": False, "command": ["relay", *command], "exitCode": 1,
                            "stderr": "this step was made to fail", "payload": None}
                return {"ok": True, "command": ["relay", *command], "exitCode": 0,
                        "payload": _trial_payload(name)}

            with mock.patch.object(runtime_install.scope, "relay", side_effect=relay):
                with usable_settings():
                    result = runtime_install._trial(self._args(), "/usr/bin/relay", RELAY_RUNTIME)

            self.assertEqual(result["value"], "not_verified", failing)
            self.assertEqual(
                performed, steps[:index + 1],
                "a failing " + failing + " must stop the trial rather than write past it")

    def test_a_lookup_that_found_nothing_is_still_an_observation_the_trial_proceeds_past(self):
        # The distinction this check must not lose: an answer that found no assignment is a
        # reading, and a command that did not run is not.
        import runtime_install

        performed = []

        def relay(command, **kwargs):
            performed.append(command[0])
            payload = _trial_payload(command[0])
            if command[0] == "assignment-find":
                payload = {"issueKey": "JUN-104", "assignments": [],
                           "responsibleChild": None, "responsibleRelationship": None}
            return {"ok": True, "command": ["relay", *command], "exitCode": 0, "payload": payload}

        with mock.patch.object(runtime_install.scope, "relay", side_effect=relay):
            with usable_settings():
                result = runtime_install._trial(self._args(), "/usr/bin/relay", RELAY_RUNTIME)
        self.assertEqual(result["value"], "verified", result["evidence"][:400])
        self.assertEqual(performed, self._steps())


def _trial_payload(name):
    """The smallest answer each step needs to let the next one run."""
    return {
        "register": {"relationshipId": "rel-test"},
        "generation-open": {"executionGeneration": 1},
        "emit": {"receipt": {"eventId": "ev-test"}},
        "deliver": {"attempt": {"turnId": "turn-test"}},
    }.get(name, {"ok": True})


# =========================================================================================
# Check 6 - past the exclusive mkdir, every exit releases what this run created
# =========================================================================================

def _unreleased_exits(tree):
    """Returns inside cmd_install after the exclusive mkdir that do not go through release.

    The exclusive mkdir is what proves this run owns the directory, and owning it is what
    obliges the run to release it. A refusal that returns straight out leaves a deterministic
    directory name behind, and every retry of that destination then refuses for ever.
    """
    for node in ast.walk(tree):
        if not (isinstance(node, ast.FunctionDef) and node.name == "cmd_install"):
            continue
        owns_from = None
        for statement in ast.walk(node):
            if (isinstance(statement, ast.Assign)
                    and any(getattr(t, "id", None) == "owned" for t in statement.targets)):
                owns_from = statement.lineno
        if owns_from is None:
            return ["cmd_install never records owning a directory"]
        offenders, successes = [], []
        for statement in ast.walk(node):
            if not isinstance(statement, ast.Return) or statement.lineno <= owns_from:
                continue
            value = statement.value
            if isinstance(value, ast.Call):
                called = value.func
                name = (called.attr if isinstance(called, ast.Attribute)
                        else getattr(called, "id", None))
                if name == "_install_failed":
                    continue
            # The success return is identified by its VALUE, not by its position. The last
            # return in this function is an exception handler, so exempting the last one
            # exempted a failure path and flagged the success.
            if isinstance(value, ast.Name) and value.id == "EXIT_OK":
                successes.append("line " + str(statement.lineno))
                continue
            offenders.append("line " + str(statement.lineno))
        if len(successes) > 1:
            offenders.append("more than one success return: " + ", ".join(successes))
        return offenders
    return ["cmd_install was not found"]


class OwnershipReleaseTests(unittest.TestCase):
    def test_every_exit_after_the_exclusive_mkdir_releases_the_directory(self):
        tree = ast.parse((ROOT / "scripts" / "runtime_install.py").read_text(encoding="utf-8"))
        self.assertEqual(_unreleased_exits(tree), [],
                         "a refusal that returns without releasing blocks every retry of"
                         " the same destination")

    def test_a_record_failure_after_the_directory_exists_still_leaves_it_retriable(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "dest"
            record_path = Path(temporary) / "record.json"
            hostrecord.save(record_path, hostrecord.empty(1))

            emitted = []
            real = hostrecord.update

            def failing(path, version, **deltas):
                return reading.Reading(state=reading.ACCESS_ERROR, source=path,
                                       exception="PermissionError", at="hostrecord.py:1",
                                       detail="the record could not be written")

            args = argparse.Namespace(dest=str(destination), apply=True, record=str(record_path),
                                      python=sys.executable, socket=None, state=None,
                                      issue="JUN-104", codex_home=str(destination.parent))
            with mock.patch.object(runtime_install.hostrecord, "update", side_effect=failing), \
                 mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_install(args)
            leftover = sorted(p.name for p in destination.iterdir()) if destination.is_dir() else []

        self.assertEqual(code, 1)
        self.assertEqual(leftover, [], "the destination this run created must be retriable")
        self.assertIn("could not be written", json.dumps(emitted[-1]))




# =========================================================================================
# Check 1 - the reader's domain is not the writer's range
# =========================================================================================

# Generated from TOML's own shapes, not from render(). The reader has to meet configurations
# this module would never write, and the two defects that reached review were both outside
# anything the writer emits.
READABLE_FIXTURES = [
    ("empty file", ""),
    ("comments only", "# just a comment\n\n   # another\n"),
    ("one server", '[mcp_servers.one]\ncommand = "/bin/one"\n'),
    ("quoted name", '[mcp_servers."codex-thread-bridge"]\ncommand = "/bin/bridge"\n'),
    ("dotted name quoted", '[mcp_servers."a.b"]\ncommand = "/bin/ab"\n'),
    ("bracket in a quoted name", '[mcp_servers."a[b]"]\ncommand = "/bin/x"\n'),
    ("args on one line", '[mcp_servers.one]\ncommand = "/bin/one"\nargs = ["a", "b"]\n'),
    ("args over lines", '[mcp_servers.one]\ncommand = "/c"\nargs = [\n  "a",\n  "b",\n]\n'),
    ("a server sub-table that is not command or args",
     '[mcp_servers.one]\ncommand = "/c"\n\n[mcp_servers.one.env]\nTOKEN = "t"\n'),
    ("other tables around it",
     '[tui]\ntheme = "dark"\n\n[mcp_servers.one]\ncommand = "/c"\n\n[history]\nmax = 10\n'),
    ("a multi-line array in another table",
     '[other]\nvalues = [\n  "a",\n  "b",\n]\n\n[mcp_servers.one]\ncommand = "/c"\n'),
    ("a multi-line string in another table",
     '[other]\nnote = """\nline\n"""\n\n[mcp_servers.one]\ncommand = "/c"\n'),
    ("an inline table in another table", '[other]\nenv = { A = "1" }\n'),
    ("array-of-tables elsewhere", '[[jobs]]\nname = "a"\n\n[[jobs]]\nname = "b"\n'),
    ("CRLF", '[mcp_servers.one]\r\ncommand = "/c"\r\n'),
    ("literal strings", "[mcp_servers.one]\ncommand = '/c'\nargs = ['a']\n"),
    ("escapes in a value", '[mcp_servers.one]\ncommand = "a\\tb\\"c"\n'),
    ("a comment after a value", '[mcp_servers.one]\ncommand = "/c"  # why\n'),
    ("no mcp_servers at all", '[tui]\ntheme = "dark"\n'),
]

# Valid TOML written in spellings this module never emits. The reader has to meet them, which
# is the whole reason it is tomllib and not something written here.
OTHER_SPELLINGS = [
    ("a member of the parent table", '[mcp_servers]\none = { command = "/c" }\n'),
    ("a quoted member of the parent table", '[mcp_servers]\n"one" = { command = "/c" }\n'),
    ("a dotted root assignment", 'mcp_servers.one.command = "/c"\n'),
    ("a root inline table", 'mcp_servers = { one = { command = "/c" } }\n'),
    ("a multi-line string command", '[mcp_servers.one]\ncommand = """\nrun"""\n'),
]

# Registration shapes that are not a registration at all. Both readers refuse these: one
# because it validates the shape it parsed, the other because it does not model it.
REFUSED_SHAPES = [
    ("args as a string", '[mcp_servers.one]\ncommand = "/c"\nargs = "ab"\n'),
    ("command as a list", '[mcp_servers.one]\ncommand = ["/c"]\n'),
    ("args as a sub-table", '[mcp_servers.one]\ncommand = "/c"\n\n[mcp_servers.one.args]\nx = "a"\n'),
    ("args as a dotted key", '[mcp_servers.one]\ncommand = "/c"\nargs.x = "a"\n'),
    ("array-of-tables under mcp_servers", '[[mcp_servers]]\ncommand = "/c"\n'),
    ("an escape-encoded mcp_servers array", '[["mcp_\u0073ervers"]]\n'),
]

INVALID_TOML = [
    ("an unterminated header", "[broken\n"),
    ("an unterminated string", '[mcp_servers.one]\ncommand = "/c\n'),
    ("a stray token", "[mcp_servers.one]\ncommand = /c\n"),
    ("an unterminated array", '[mcp_servers.one]\nargs = [\n'),
]


def _oracle(text):
    """tomllib's view of mcp_servers, projected to the two fields registration compares."""
    import tomllib

    return codexconfig.registration_view(tomllib.loads(text).get("mcp_servers", {}))


class ReaderDomainTests(unittest.TestCase):
    """Correct, or unreadable. Readable-and-different is the state that cannot occur.

    tomllib is the reader on 3.11 and newer, so the interesting subject here is the fallback,
    and it is exercised on every interpreter rather than only on the one that has no oracle to
    judge it. On 3.11+ the judge is tomllib; on 3.10 it is the expectations written above,
    which were produced independently of both readers.
    """

    def _has_oracle(self):
        try:
            import tomllib  # noqa: F401
        except ImportError:
            return False
        return True

    @needs_reader
    def test_the_reader_is_correct_or_unreadable_over_the_whole_corpus(self):
        if not self._has_oracle():
            self.skipTest("the differential comparison needs tomllib")
        wrong = []
        for label, text in READABLE_FIXTURES + OTHER_SPELLINGS + REFUSED_SHAPES:
            view = codexconfig.scan(text)
            if not view.readable:
                continue
            try:
                expected = _oracle(text)
            except Exception:
                wrong.append((label, "readable, but the file is not valid TOML"))
                continue
            if view.servers != expected:
                wrong.append((label, "read " + repr(view.servers) + " but tomllib reads "
                              + repr(expected)))
        self.assertEqual(wrong, [], "the fallback may refuse, and may agree; it may never"
                                    " read something different")

    @needs_reader
    def test_the_readable_fixtures_are_actually_read(self):
        # Without this, refusing everything would satisfy the property above.
        for label, text in READABLE_FIXTURES + OTHER_SPELLINGS:
            with self.subTest(label):
                view = codexconfig.scan(text)
                self.assertTrue(view.readable, label + ": " + str(view.unreadable))

    @needs_reader
    def test_invalid_toml_is_never_appended_to(self):
        for label, text in INVALID_TOML:
            with self.subTest(label):
                new_text, outcome, detail = codexconfig.register(
                    text, "codex-thread-bridge", "/usr/bin/bridge", [])
                self.assertEqual(new_text, text, label)
                self.assertIn(outcome, ("UNREADABLE", "CONFLICT"), label + ": " + detail)

    @needs_reader
    def test_appending_is_read_back_before_it_is_written(self):
        # A root inline table cannot be extended, and under an array-of-tables the appended
        # table attaches to the last element rather than to a root mapping. Both are refused
        # without writing.
        for label, text in (("closed inline table", "mcp_servers = {}\n"),
                            ("array of tables", "[[mcp_servers]]\n")):
            with self.subTest(label):
                new_text, outcome, detail = codexconfig.register(text, "one", "/c", [])
                self.assertEqual(new_text, text)
                self.assertIn(outcome, ("UNREADABLE", "CONFLICT"), detail)

    @needs_reader
    def test_a_registration_shape_that_parses_is_still_validated(self):
        # The parse is fine; list("ab") == ["a", "b"] is the trap.
        text = '[mcp_servers.one]\ncommand = "run"\nargs = "ab"\n'
        _, outcome, _ = codexconfig.register(text, "one", "run", ["a", "b"])
        self.assertNotEqual(outcome, "LINKED",
                            "a malformed registration must never compare equal to a"
                            " well-formed request")




# =========================================================================================
# Check 4 - the failure contract: no input leaves a traceback
# =========================================================================================

class FailureContractTests(unittest.TestCase):
    """A matrix cannot prove "no input", so the guarantee is structural and the matrix checks
    that the modelled channels stay modelled.

    main() converts anything that escapes a handler into an internalError result naming the
    exception and the line that raised it. That is not a second reading boundary and does not
    share its vocabulary: a reading refusal carries a state from the four-state partition and
    says something about a record; internalError says a defect in this command reached the
    top. A defect stays reportable, and the process never prints a traceback.
    """

    def test_a_defect_that_escapes_a_handler_is_named_rather_than_raised(self):
        import runtime_install

        for error in (ValueError("a defect"), KeyError("absent"), RuntimeError("boom")):
            with self.subTest(type(error).__name__):
                emitted = []
                with mock.patch.object(runtime_install, "cmd_verify_definition",
                                       side_effect=error), \
                     mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                    code = runtime_install.main(["verify-definition"])
                self.assertEqual(code, 1)
                self.assertEqual(emitted[0]["internalError"]["exception"], type(error).__name__)
                self.assertTrue(emitted[0]["internalError"]["raisedAt"])
                self.assertNotIn("reading", emitted[0],
                                 "a defect is never filed as a statement about a record")

    def test_hostile_inputs_produce_modelled_refusals_rather_than_internal_errors(self):
        # What the matrix is for, now that the contract covers the rest: the channels it
        # exercises must still be answered by the readers rather than by the safety net.
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary) / "home"
            home.mkdir()
            (home / "config.toml").write_bytes(b"\xff\xfe")
            (home / "hooks.json").write_text('{"hooks": {"Stop": 5}}', encoding="utf-8")
            record = Path(temporary) / "record.json"
            record.write_text('{"components": {"a": {"installs": 7}}}', encoding="utf-8")
            directory = Path(temporary) / "as-a-directory.json"
            directory.mkdir()

            runs = [
                ("install", ["--dest", str(Path(temporary) / "dest"), "--record", str(record)]),
                ("install", ["--dest", str(Path(temporary) / "dest2"), "--record", str(directory)]),
                ("measure", ["--record", str(record)]),
                ("register-mcp", ["--codex-home", str(home), "--bridge-command", "/usr/bin/b"]),
                ("hook", ["--codex-home", str(home), "--event", "Stop",
                          "--hook-command", "/bin/true"]),
            ]
            for command, extra in runs:
                with self.subTest(command + " " + " ".join(extra[:2])):
                    done = run(command, *extra)
                    self.assertNotIn("Traceback", done.stderr, done.stderr[-400:])
                    payload = json.loads(done.stdout)
                    self.assertNotIn("internalError", payload,
                                     "a modelled channel must be answered by a reader")

    def test_diagnosis_reports_unreadability_and_still_exits_zero(self):
        # The contract is about tracebacks, not about forcing every command to refuse.
        with tempfile.TemporaryDirectory() as temporary:
            record = Path(temporary) / "record.json"
            record.write_text('{"components": {"a": {"installs": 7}}}', encoding="utf-8")
            home = Path(temporary) / "home"
            home.mkdir()
            done = run("diagnose", "--record", str(record), "--codex-home", str(home))
        self.assertEqual(done.returncode, 0, done.stderr[-400:])
        self.assertNotIn("Traceback", done.stderr)
        self.assertEqual(json.loads(done.stdout)["hostRecordState"], reading.UNREADABLE)

    def test_an_unreadable_installed_file_refuses_instead_of_escaping_diagnosis(self):
        # ops12_digest reads installed bytes; it used to do so outside every region.
        if os.geteuid() == 0:
            self.skipTest("permissions do not restrict root")
        with tempfile.TemporaryDirectory() as temporary:
            package = Path(temporary) / "pkg"
            package.mkdir()
            secret = package / "mod.py"
            secret.write_text("x = 1", encoding="utf-8")
            secret.chmod(0o000)
            try:
                with self.assertRaises(reading.Refused) as caught:
                    with reading.region(package, "the installed bytes"):
                        definition.ops12_digest(package)
            finally:
                secret.chmod(0o600)
        self.assertEqual(caught.exception.reading.state, reading.ACCESS_ERROR)


# =========================================================================================
# Check 6 - after a failed run the destination is retriable
# =========================================================================================

class RetriableDestinationTests(unittest.TestCase):
    """The assertion is on the filesystem, not on the call.

    rmtree(ignore_errors=True) reporting removal from pre-attempt existence satisfied "the
    release path was called" while leaving a directory that refused every retry. So removal
    is verified, and a cleanup that could not finish reports NOT retriable with what is left
    rather than claiming a release it did not achieve.
    """

    def _install(self, temporary, **patches):
        import runtime_install

        record_path = Path(temporary) / "record.json"
        hostrecord.save(record_path, hostrecord.empty(1))
        emitted = []
        args = argparse.Namespace(dest=str(Path(temporary) / "dest"), apply=True,
                                  record=str(record_path), python=sys.executable,
                                  socket=None, state=None, issue="JUN-104",
                                  codex_home=temporary)
        stack = [mock.patch.object(runtime_install, "emit", side_effect=emitted.append)]
        for target, kwargs in patches.items():
            stack.append(mock.patch.object(runtime_install, target, **kwargs))
        for entered in stack:
            entered.__enter__()
        try:
            code = runtime_install.cmd_install(args)
        finally:
            for entered in reversed(stack):
                entered.__exit__(None, None, None)
        return code, emitted, Path(temporary) / "dest", record_path

    def test_every_failure_point_leaves_the_destination_retriable(self):
        raising = reading.Reading(state=reading.ACCESS_ERROR, source="record",
                                  exception="PermissionError", at="hostrecord.py:1",
                                  detail="the record could not be written")
        injections = {
            "a returned record failure": {"hostrecord": None},
            "a raised record failure": {"hostrecord": None},
            "a failed subprocess step": {"perform": None},
        }
        import runtime_install

        for label in injections:
            with self.subTest(label):
                with tempfile.TemporaryDirectory() as temporary:
                    if label == "a returned record failure":
                        patch = {"hostrecord": mock.DEFAULT}
                        with mock.patch.object(runtime_install.hostrecord, "update",
                                               return_value=raising):
                            code, emitted, dest, _ = self._install(temporary)
                    elif label == "a raised record failure":
                        with mock.patch.object(runtime_install.hostrecord, "update",
                                               side_effect=PermissionError("denied")):
                            code, emitted, dest, _ = self._install(temporary)
                    else:
                        with mock.patch.object(runtime_install.subprocess, "run",
                                               side_effect=OSError("no interpreter")):
                            code, emitted, dest, _ = self._install(temporary)
                    self.assertEqual(code, 1, label)
                    leftover = sorted(p.name for p in dest.iterdir()) if dest.is_dir() else []
                    self.assertEqual(leftover, [], label + ": the destination must be retriable")

    def test_a_cleanup_that_cannot_finish_says_not_retriable(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            record_path = Path(temporary) / "record.json"
            hostrecord.save(record_path, hostrecord.empty(1))
            owned = Path(temporary) / "env"
            owned.mkdir()
            emitted = []
            with mock.patch.object(runtime_install.shutil, "rmtree",
                                   side_effect=PermissionError("in use")), \
                 mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install._install_failed(record_path, 1, [], str(owned), owned)
            self.assertTrue(owned.exists(), "the cleanup was made to fail")
        self.assertEqual(code, 1)
        result = emitted[0]
        self.assertFalse(result["retriable"])
        self.assertEqual(result["residualPaths"], [str(owned)])
        self.assertIn("by hand", result["recoveryRequires"])
        self.assertIn("PermissionError", result["cleanupError"])

    def test_a_promoted_environment_survives_a_failure_after_the_promotion_committed(self):
        # update() saves inside the lock and releasing the lock can still raise, so a run can
        # commit its promotion and raise anyway. Recovery reads the selection rather than
        # trusting a flag.
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            record_path = Path(temporary) / "record.json"
            owned = Path(temporary) / "env"
            owned.mkdir()
            record = hostrecord.empty(1)
            record["selected"] = {"codex-session-relay": str(owned / "lib" / "pkg")}
            hostrecord.put_install(record, "codex-session-relay",
                                   {"location": str(owned / "lib" / "pkg"),
                                    "environment": str(owned)})
            hostrecord.save(record_path, record)

            emitted = []
            with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                runtime_install._install_failed(record_path, 1, [], str(owned), owned)
            after = hostrecord.load(record_path, 1).value

            self.assertTrue(owned.exists(), "a selected environment is never deleted")
            self.assertIn("selected", emitted[0]["candidate"])
            self.assertEqual(
                len(after["components"]["codex-session-relay"]["installs"]), 1,
                "and its records are kept with it")

    def test_an_unreadable_record_is_not_permission_to_delete(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            record_path = Path(temporary) / "record.json"
            record_path.write_text("{not json", encoding="utf-8")
            owned = Path(temporary) / "env"
            owned.mkdir()
            emitted = []
            with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                runtime_install._install_failed(record_path, 1, [], str(owned), owned)
            self.assertTrue(owned.exists(),
                            "absence of evidence about the selection is not permission")
            self.assertIn("could not be read", emitted[0]["candidate"])


# =========================================================================================
# The preflight corpus, filled in
# =========================================================================================

class PreflightCorpusTests(unittest.TestCase):
    def test_a_turn_status_outside_the_relays_choices_is_refused_by_the_parser(self):
        import runtime_install

        parser = runtime_install.build_parser()
        with self.assertRaises(SystemExit):
            parser.parse_args(["diagnose", "--trial", "--turn-status", "almost"])
        parsed = parser.parse_args(["diagnose", "--trial", "--turn-status", "interrupted"])
        self.assertEqual(parsed.turn_status, "interrupted")

    def test_a_turn_thread_that_is_not_the_child_task_is_refused_before_any_write(self):
        import runtime_install

        args = argparse.Namespace(
            issue="JUN-104", parent_task="p", child_task="c", recipient="p",
            artifact_root="/tmp", turn_thread="not-c", turn_id="ti", artifact=["/tmp/a"],
            dispatch_turn_id="d", turn_status="completed", recipient_settings=None,
            settings_already_recorded=True, expect_relationship=None, socket=None, state=None)
        with mock.patch.object(runtime_install.scope, "relay") as relay:
            result = runtime_install._trial(args, "/usr/bin/relay", RELAY_RUNTIME)
        self.assertEqual(result["value"], "not_verified")
        self.assertIn("is not the child task", result["evidence"])
        relay.assert_not_called()

    def test_an_artifact_the_relay_would_refuse_is_refused_before_any_write(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "root"
            root.mkdir()
            real = root / "result.txt"
            real.write_text("done", encoding="utf-8")
            outside = Path(temporary) / "outside.txt"
            outside.write_text("done", encoding="utf-8")
            link = root / "link.txt"
            link.symlink_to(real)

            cases = {
                "missing": str(root / "gone.txt"),
                "outside the root": str(outside),
                "a symlink": str(link),
                "not normalised": str(root) + "/./result.txt",
                "relative": "result.txt",
                "a directory": str(root),
            }
            for label, path in cases.items():
                with self.subTest(label):
                    self.assertTrue(runtime_install._unusable_artifacts([path], str(root), RELAY_RUNTIME),
                                    label + " should be refused")
            self.assertEqual(runtime_install._unusable_artifacts([str(real)], str(root), RELAY_RUNTIME), [])

    def test_a_refused_artifact_stops_the_trial_before_the_first_command(self):
        import runtime_install

        args = argparse.Namespace(
            issue="JUN-104", parent_task="p", child_task="c", recipient="p",
            artifact_root="/tmp/jun104-nowhere", turn_thread="c", turn_id="ti",
            artifact=["/tmp/jun104-nowhere/missing.txt"], dispatch_turn_id="d",
            turn_status="completed", recipient_settings=None,
            settings_already_recorded=True, expect_relationship=None, socket=None, state=None)
        with mock.patch.object(runtime_install.scope, "relay") as relay:
            result = runtime_install._trial(args, "/usr/bin/relay", RELAY_RUNTIME)
        self.assertEqual(result["value"], "not_verified")
        self.assertIn("manifest entry", result["evidence"])
        relay.assert_not_called()


class UnreadableDimensionTests(unittest.TestCase):
    def test_a_codex_version_that_cannot_be_read_never_lifts_a_classification(self):
        import runtime_install

        component = definition.load()["components"][0]
        record = hostrecord.empty(1)
        with mock.patch.object(runtime_install, "codex_cli_version", return_value=None):
            classified = runtime_install.classify_component(component, record=record)
        self.assertEqual(classified["class"], "unreadable")
        self.assertTrue(any("Codex CLI" in reason for reason in classified["reasons"]),
                        classified["reasons"])
        self.assertFalse(classified["reusable"])

    def test_a_bare_relay_override_is_resolved_the_way_it_is_run(self):
        import runtime_install

        resolved = runtime_install.resolve_entry_point("python3", "python3")
        self.assertIsNotNone(resolved)
        self.assertTrue(Path(resolved).is_absolute(),
                        "classification must describe the executable that would run")

    def test_every_unreadable_signal_is_recorded_rather_than_only_the_first(self):
        """Two signals failing at once must both be named.

        Found the hard way: a host with no codex binary reported only the Codex version and
        stopped, so an unreadable host record went unmentioned on exactly the machines that
        have neither. One unreadable signal must not hide another.
        """
        import runtime_install

        component = definition.load()["components"][0]
        with mock.patch.object(runtime_install, "codex_cli_version", return_value=None):
            classified = runtime_install.classify_component(
                component, record=None, record_state=reading.ACCESS_ERROR)
        reasons = " ".join(classified["reasons"])
        self.assertEqual(classified["class"], "unreadable")
        self.assertIn("Codex CLI", reasons)
        self.assertIn("host record", reasons)
        self.assertIn(reading.ACCESS_ERROR, reasons,
                      "and the record's failure keeps its own state")




# =========================================================================================
# Check 7 - a scanner proves nothing until it is shown a violation
# =========================================================================================

# An independent review fed known violations to four of these scanners and got empty offender
# lists from all four. An empty list has two meanings -- nothing is wrong, or the scanner
# cannot look -- and it is the value that OPENS the gate, so it is never trusted again without
# this table. Each entry is source the scanner must flag.
SCANNER_VIOLATIONS = [
    ("prefix test on a path",
     "def decide(a, b):\n    return str(a).startswith(str(b))\n",
     lambda tree: _raw_prefix_tests(tree)),
    ("suffix test written raw",
     "def decide(a):\n    return a.endswith('/pkg')\n",
     lambda tree: _raw_prefix_tests(tree)),
    ("identity compared against serialized text",
     "import json\ndef guard(expected, payload):\n    return expected in json.dumps(payload)\n",
     lambda tree: _substring_identity_comparisons(tree)),
    ("identity compared against a serialized name",
     "import json\ndef guard(expected, payload):\n    seen = json.dumps(payload)\n"
     "    return expected in seen\n",
     lambda tree: _substring_identity_comparisons(tree)),
    ("write_text with no lock",
     "def write(path, data):\n    path.write_text(data)\n",
     lambda tree: _unguarded_writes(tree)),
    ("write_bytes with no lock",
     "def write(path, data):\n    path.write_bytes(data)\n",
     lambda tree: _unguarded_writes(tree)),
    ("save with no lock",
     "def write(path, record):\n    save(path, record)\n",
     lambda tree: _unguarded_writes(tree)),
    ("a lock held over a different target",
     "def write(path, other, record):\n    with Locked(other):\n        save(path, record)\n",
     lambda tree: _unguarded_writes(tree)),
    ("a bare refusal return after ownership",
     "def cmd_install(args):\n    owned = environment\n    if bad:\n        return EXIT_REFUSED\n"
     "    return EXIT_OK\n",
     lambda tree: _unreleased_exits(tree)),
    ("a named refusal return after ownership",
     "def cmd_install(args):\n    owned = environment\n    if bad:\n"
     "        return refused('install', why)\n    return EXIT_OK\n",
     lambda tree: _unreleased_exits(tree)),
    ("two success returns after ownership",
     "def cmd_install(args):\n    owned = environment\n    if ok:\n        return EXIT_OK\n"
     "    return EXIT_OK\n",
     lambda tree: _unreleased_exits(tree)),
]


class ScannerSightTests(unittest.TestCase):
    """Every scanner is shown a violation it must catch.

    This is the check that was missing when four scanners returned empty lists against known
    violations and the emptiness was read as cleanliness. A scanner that stops seeing now
    fails here rather than passing quietly everywhere.
    """

    def test_every_scanner_flags_the_violation_written_for_it(self):
        blind = []
        for label, source, scan in SCANNER_VIOLATIONS:
            if not scan(ast.parse(source)):
                blind.append(label)
        self.assertEqual(blind, [], "these scanners cannot see the defect they exist for")

    def test_every_scanner_is_quiet_on_source_that_does_not_violate(self):
        # The other half: a scanner that flags everything is as useless as one that flags
        # nothing, and would make the inventories unusable.
        clean = [
            ("a textual test through the helper",
             "def decide(a):\n    return text_prefix(a, '#!')\n",
             lambda tree: _raw_prefix_tests(tree)),
            ("a field comparison",
             "def guard(expected, payload):\n    return expected == payload.get('rel')\n",
             lambda tree: _substring_identity_comparisons(tree)),
            ("a write under the lock for its own target",
             "def write(path, record):\n    with Locked(path):\n        save(path, record)\n",
             lambda tree: _unguarded_writes(tree)),
            ("a release return after ownership",
             "def cmd_install(args):\n    owned = environment\n    if bad:\n"
             "        return _install_failed(p, v, s, e, owned)\n    return EXIT_OK\n",
             lambda tree: _unreleased_exits(tree)),
        ]
        noisy = []
        for label, source, scan in clean:
            found = scan(ast.parse(source))
            if found:
                noisy.append((label, found))
        self.assertEqual(noisy, [])

    def test_no_raw_prefix_test_survives_outside_the_helper(self):
        offenders = []
        for path in _source_modules():
            if path.name == "text.py":
                continue  # where the helper itself lives
            for finding in _raw_prefix_tests(ast.parse(path.read_text(encoding="utf-8"))):
                offenders.append(str(path.relative_to(ROOT)) + ": " + finding)
        self.assertEqual(offenders, [], "a prefix test is either textual, and says so through"
                                        " text_prefix, or it is an identity decision wearing"
                                        " the wrong spelling")


# =========================================================================================
# Check 8 - the producer keeps the consumer's promise
# =========================================================================================

class WriteSidePromiseTests(unittest.TestCase):
    """A value this command writes must satisfy the predicate its own reader applies.

    Every check before this one enforced a predicate where a value is CONSUMED. None of them
    asked whether the value this command WRITES would survive the same question, and four
    defects lived in exactly that gap.
    """

    def test_an_unreadable_dimension_matches_nothing_rather_than_everything(self):
        """Both directions, because only one of them was ever handled.

        A caller that could not read the Codex CLI used to have that dimension skipped
        entirely, so the point matched every CLI: the widest possible answer from the least
        possible evidence. A point that recorded nothing was already rejected.
        """
        record = hostrecord.empty(1)
        hostrecord.add_point(record, "codex-session-relay", {
            "exercised": True, "install": "/env/pkg", "interpreter": "3.13.1",
            "installDigest": "abc", "exerciseDigest": "an-instrument", "codexCli": "0.1.0", "host": "a-host",
        })
        asked = dict(location="/env/pkg", interpreter="3.13.1", install_digest="abc", exercise_digest="an-instrument",
                     host="a-host")
        self.assertEqual(
            hostrecord.points_for(record, "codex-session-relay", codex_cli=None, **asked), [],
            "a caller who could not read the dimension must match nothing, not everything")

        record["components"]["codex-session-relay"]["measuredPoints"][0]["codexCli"] = None
        self.assertEqual(
            hostrecord.points_for(record, "codex-session-relay", codex_cli="0.1.0", **asked), [],
            "and a point that recorded nothing about it matches nothing either")

    def test_a_point_measured_against_forked_bytes_can_never_be_read_back(self):
        record = hostrecord.empty(1)
        hostrecord.add_point(record, "codex-session-relay", {
            "exercised": True, "install": "/env/pkg", "interpreter": "3.13.1",
            "installDigest": "abc", "exerciseDigest": "an-instrument", "codexCli": "0.1.0", "host": "a-host",
            "digestMatchesDefinition": False,
        })
        found = hostrecord.points_for(
            record, "codex-session-relay", location="/env/pkg", interpreter="3.13.1",
            install_digest="abc", exercise_digest="an-instrument", codex_cli="0.1.0", host="a-host")
        self.assertEqual(found, [])

    def test_a_point_written_before_the_field_existed_is_still_readable(self):
        # "not false" rather than "true": such a point was already non-qualifying evidence,
        # not a malformed record.
        record = hostrecord.empty(1)
        hostrecord.add_point(record, "codex-session-relay", {
            "exercised": True, "install": "/env/pkg", "interpreter": "3.13.1",
            "installDigest": "abc", "exerciseDigest": "an-instrument", "codexCli": "0.1.0", "host": "a-host",
        })
        found = hostrecord.points_for(
            record, "codex-session-relay", location="/env/pkg", interpreter="3.13.1",
            install_digest="abc", exercise_digest="an-instrument", codex_cli="0.1.0", host="a-host")
        self.assertEqual(len(found), 1)

    def test_a_measurement_refuses_to_record_a_point_its_own_reader_would_reject(self):
        import runtime_install

        data = definition.load()
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            record = hostrecord.empty(1)
            bound = {}
            for component in data["components"]:
                location = str(environment / "lib" / component["module"])
                hostrecord.put_install(record, component["component"], {
                    "location": location, "environment": str(environment),
                    "entryPoint": str(environment / "bin" / component["consoleScript"]),
                })
                bound[component["component"]] = {
                    "install": {"location": location,
                                "entryPoint": str(environment / "bin" / component["consoleScript"])},
                    "digest": "f" * 64, "location": location}

            with mock.patch.object(runtime_install, "_interpreter_prefix",
                                   return_value=str(environment)), \
                 mock.patch.object(runtime_install, "_bind_installs",
                                   return_value=(bound, None)), \
                 mock.patch.object(runtime_install, "codex_cli_version", return_value="0.1.0"), \
                 mock.patch.object(runtime_install.scope, "relay",
                                   return_value={"ok": True, "command": ["doctor"], "payload": {
                                       "actorReachability": {"socketConnect": "ok"}}}), \
                 mock.patch.object(runtime_install.subprocess, "run",
                                   return_value=type("R", (), {
                                       "returncode": 0,
                                       "stdout": json.dumps({"tools": [_BRIDGE_TOOL],
                                                             "connection": {"ok": True}}),
                                       "stderr": ""})()):
                measurement = runtime_install.measure_candidate(
                    data, record, python=str(environment / "bin" / "python"),
                    environment=str(environment), socket_path=None, state=None,
                    relay_command=str(environment / "bin" / "codex-session-relay"))

        self.assertFalse(measurement["qualifyingPoint"],
                         "bytes that disagree with the definition classify as a fork, so no"
                         " point measured against them can authorize reuse")
        self.assertEqual(measurement["points"], [])
        self.assertIn("disagree with the definition", measurement["refused"])

    def test_a_measurement_refuses_when_any_dimension_cannot_be_read(self):
        """Every dimension, not the one that was checked by name.

        The gate used to name the Codex CLI and nothing else, so an unread interpreter reached a
        qualifying point as a null mandatory value and an unread App Server reached one that
        classification refuses to proceed on at all. Both are points the consumer can never
        match, which is worse than no point: install promotes on them and the next diagnosis
        rejects the very evidence that authorized the promotion.
        """
        import runtime_install

        data = definition.load()
        # How each declared dimension's observation is broken here. The coverage is derived from
        # the map, so a dimension added to it without a case fails this test instead of quietly
        # going unchecked.
        broken = {
            "codexCli": "codex_cli_version answers None",
            "interpreter": "interpreter_version answers None",
            "host": "gethostname answers None",
            "appServer": "the smoke check reports no connection",
            "install": "bound by _bind_installs, which refuses before this gate is reached",
            "installDigest": "bound by _bind_installs, which refuses before this gate is reached",
            "exerciseDigest": "instrument_digest cannot read the smoke check it would run",
        }
        self.assertEqual(sorted(broken), sorted(hostrecord.DIMENSIONS),
                         "every declared dimension needs a case that breaks its observation")
        patched = {
            "codexCli": lambda: mock.patch.object(runtime_install, "codex_cli_version",
                                                  return_value=None),
            "interpreter": lambda: mock.patch.object(runtime_install, "interpreter_version",
                                                     return_value=None),
            "host": lambda: mock.patch.object(runtime_install.socket, "gethostname",
                                              return_value=None),
            # The instrument a claim rests on, unreadable. A point recorded here would name
            # clean installed bytes and say nothing about what produced the exercise.
            "exerciseDigest": lambda: mock.patch.object(runtime_install, "instrument_digest",
                                                        return_value=None),
        }

        def measure(extra=None, connection=True):
            with tempfile.TemporaryDirectory() as temporary:
                environment = Path(temporary) / "env"
                record = hostrecord.empty(1)
                bound = {c["component"]: {
                    "install": {"location": "/l",
                                "entryPoint": str(environment / "bin" / c["consoleScript"])},
                    "digest": c["sourceDigest"], "location": "/l"} for c in data["components"]}
                payload = {"tools": [_BRIDGE_TOOL]}
                if connection:
                    payload["connection"] = {"ok": True}
                stack = [
                    mock.patch.object(runtime_install, "_interpreter_prefix",
                                      return_value=str(environment)),
                    mock.patch.object(runtime_install, "_bind_installs",
                                      return_value=(bound, None)),
                    mock.patch.object(runtime_install, "interpreter_version",
                                      return_value="3.13.1"),
                    mock.patch.object(runtime_install, "codex_cli_version",
                                      return_value="0.1.0"),
                    mock.patch.object(runtime_install.scope, "relay",
                                      return_value={"ok": True, "command": ["doctor"], "payload": {
                                          "actorReachability": {"socketConnect": "ok"}}}),
                    mock.patch.object(runtime_install.subprocess, "run",
                                      return_value=type("R", (), {
                                          "returncode": 0,
                                          "stdout": json.dumps(payload),
                                          "stderr": ""})()),
                ]
                if extra is not None:
                    stack.append(extra)
                with contextlib.ExitStack() as entered:
                    for patch in stack:
                        entered.enter_context(patch)
                    return runtime_install.measure_candidate(
                        data, record, python=str(environment / "bin" / "python"),
                        environment=str(environment), socket_path=None, state=None,
                        relay_command=str(environment / "bin" / "codex-session-relay"))

        # The baseline qualifies, so every refusal below means something.
        self.assertTrue(measure()["qualifyingPoint"])

        for dimension in sorted(patched):
            with self.subTest(dimension):
                measurement = measure(patched[dimension]())
                self.assertFalse(measurement["qualifyingPoint"], dimension)
                self.assertEqual(measurement["points"], [])
                self.assertIn(dimension, measurement["refused"])

        with self.subTest("appServer"):
            measurement = measure(connection=False)
            self.assertFalse(measurement["qualifyingPoint"])
            self.assertEqual(measurement["points"], [])
            self.assertIn("appServer", measurement["refused"])

        with self.subTest("install and installDigest come from the bound install"):
            # The two per-component dimensions are observed when the installs are bound, and a
            # binding that produced neither is already refused there. What this asserts is that
            # the point carries the bound values rather than observing them a second time.
            for _, point in measure()["points"]:
                self.assertEqual(point["install"], "/l")
                self.assertIsNotNone(point["installDigest"])

    def test_the_doctor_runs_the_relay_recorded_for_this_environment(self):
        import runtime_install

        data = definition.load()
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            bound = {c["component"]: {
                "install": {"location": "/l",
                            "entryPoint": str(environment / "bin" / c["consoleScript"])},
                "digest": c["sourceDigest"], "location": "/l"} for c in data["components"]}
            asked = []

            def relay(command, **kwargs):
                asked.append(kwargs.get("executable"))
                return {"ok": True, "command": ["doctor"],
                        "payload": {"actorReachability": {"socketConnect": "ok"}}}

            with mock.patch.object(runtime_install, "_interpreter_prefix",
                                   return_value=str(environment)), \
                 mock.patch.object(runtime_install, "_bind_installs",
                                   return_value=(bound, None)), \
                 mock.patch.object(runtime_install.scope, "relay", side_effect=relay), \
                 mock.patch.object(runtime_install.subprocess, "run",
                                   return_value=type("R", (), {
                                       "returncode": 1, "stdout": "", "stderr": "no"})()):
                measurement = runtime_install.measure_candidate(
                    data, hostrecord.empty(1), python=str(environment / "bin" / "python"),
                    environment=str(environment), socket_path=None, state=None,
                    relay_command="/somewhere/else/codex-session-relay")
        self.assertFalse(measurement["qualifyingPoint"])
        self.assertIn("would describe a different runtime", measurement["refused"])
        self.assertEqual(asked, [], "an unbound relay is refused before it is run, not after")

    def test_retriable_means_the_destination_can_actually_be_used_again(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            record_path = Path(temporary) / "record.json"
            record_path.write_text("{not json", encoding="utf-8")
            owned = Path(temporary) / "env"
            owned.mkdir()
            emitted = []
            with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                runtime_install._install_failed(record_path, 1, [], str(owned), owned)
            self.assertTrue(owned.exists(), "kept, because the selection could not be read")
            result = emitted[0]
            self.assertFalse(result["retriable"],
                             "the directory is still there, so the next install refuses")
            self.assertEqual(result["residualPaths"], [str(owned)])
            self.assertTrue(result["recoveryRequires"])




# =========================================================================================
# Check 9 - the predicate is applied to the SET, and the set comes from the source
# =========================================================================================

RELAY_SRC = ROOT / "packages" / "codex-session-relay" / "src" / "codex_session_relay"


def _point_accesses(tree):
    """Every access to a point inside points_for, classified or reported as a violation.

    A key is acceptable when it is a string literal, or when it is the variable a loop over the
    declared DIMENSIONS map binds - that second form IS the derivation, and forbidding it would
    forbid the only implementation that cannot drift. Any other dynamic key, a point handed to
    a helper, a comprehension over it, or an unmodelled method is a violation, because each
    would let a comparison exist that this scan cannot see.

    Appending the whole point to the result is not a comparison and is exempt by name.
    """
    for node in ast.walk(tree):
        if isinstance(node, ast.FunctionDef) and node.name == "points_for":
            function = node
            break
    else:
        return ["points_for was not found"], []

    # Names bound by iterating the declared map. A key taken from one of these is the map
    # driving the comparison, which is the closed form this check exists to require.
    derived = set()
    for node in ast.walk(function):
        if isinstance(node, ast.For):
            iterated = node.iter
            if isinstance(iterated, ast.Call) and isinstance(iterated.func, ast.Attribute):
                iterated = iterated.func.value
            if getattr(iterated, "id", None) == "DIMENSIONS":
                target = node.target
                names = target.elts if isinstance(target, ast.Tuple) else [target]
                for name in names:
                    if isinstance(name, ast.Name):
                        derived.add(name.id)

    keys, violations = [], []
    for node in ast.walk(function):
        if isinstance(node, ast.Subscript) and getattr(node.value, "id", None) == "point":
            index = node.slice
            if isinstance(index, ast.Constant) and isinstance(index.value, str):
                keys.append(index.value)
            elif not (isinstance(index, ast.Name) and index.id in derived):
                violations.append("a point key that is neither a literal nor taken from the"
                                  " declared map, at line " + str(node.lineno))
        if isinstance(node, ast.Call):
            called = node.func
            if isinstance(called, ast.Attribute) and getattr(called.value, "id", None) == "point":
                if called.attr != "get":
                    violations.append("an unmodelled point method at line " + str(node.lineno))
                elif node.args and isinstance(node.args[0], ast.Constant) \
                        and isinstance(node.args[0].value, str):
                    keys.append(node.args[0].value)
                elif not (node.args and isinstance(node.args[0], ast.Name)
                          and node.args[0].id in derived):
                    violations.append("a point key that is neither a literal nor taken from the"
                                      " declared map, at line " + str(node.lineno))
            accumulating = (isinstance(called, ast.Attribute) and called.attr == "append")
            for argument in list(node.args) + [k.value for k in node.keywords]:
                if getattr(argument, "id", None) == "point" and not accumulating:
                    violations.append("point passed to a call at line " + str(node.lineno))
        if isinstance(node, (ast.ListComp, ast.SetComp, ast.DictComp, ast.GeneratorExp)):
            for generator in node.generators:
                if getattr(generator.iter, "id", None) == "point":
                    violations.append("a comprehension over point at line " + str(node.lineno))
    return violations, keys


def _relay_normalization_rules():
    """The refusal predicates normalize_declared_path enforces, read from the relay's source.

    Derived rather than listed, so a rule the relay adds shows up with no fixture and fails the
    check instead of quietly not being checked.
    """
    tree = ast.parse((RELAY_SRC / "scope.py").read_text(encoding="utf-8"))
    for node in ast.walk(tree):
        if isinstance(node, ast.FunctionDef) and node.name == "normalize_declared_path":
            return [ast.unparse(branch.test) for branch in ast.walk(node)
                    if isinstance(branch, ast.If)]
    return []


def _relay_state_selectors():
    """The explicit store selections the relay resolves, read from its own resolver."""
    source = (RELAY_SRC / "store.py").read_text(encoding="utf-8")
    found = set()
    if "STATE_ENV" in source:
        found.add("environment")
    if "explicit" in source:
        found.add("flag")
    return found


class DimensionCoverageTests(unittest.TestCase):
    """Every dimension, mutated on its own, must stop a point matching.

    The defect this closes is not a wrong value - every dimension already rejected a mismatch.
    It is PRESENCE. A caller that observed no App Server matched a point that had observed one,
    because a missing value was read as "no constraint": the widest possible answer from the
    least possible evidence.
    """

    def _point(self):
        return {"exercised": True, "install": "/env/pkg", "interpreter": "3.13.1",
                "installDigest": "abc", "exerciseDigest": "an-instrument", "codexCli": "0.1.0", "host": "a-host",
                "appServer": "a-server"}

    def _asked(self):
        return {"location": "/env/pkg", "interpreter": "3.13.1", "install_digest": "abc", "exercise_digest": "an-instrument",
                "codex_cli": "0.1.0", "host": "a-host", "app_server": "a-server"}

    def _match(self, point, asked):
        record = hostrecord.empty(1)
        hostrecord.add_point(record, "codex-session-relay", point)
        return hostrecord.points_for(record, "codex-session-relay", **asked)

    def test_the_inventory_is_the_comparison_itself(self):
        violations, keys = _point_accesses(
            ast.parse((ROOT / "scripts" / "crw_runtime" / "hostrecord.py")
                      .read_text(encoding="utf-8")))
        self.assertEqual(violations, [])
        declared = set(hostrecord.DIMENSIONS) | set(hostrecord.GATES)
        self.assertEqual(set(keys) - declared, set(),
                         "a field compared here and declared in neither map")

    def test_the_baseline_matches_so_the_mutations_mean_something(self):
        self.assertEqual(len(self._match(self._point(), self._asked())), 1)

    def test_every_dimension_mutated_alone_stops_the_match(self):
        for field, (argument, policy) in hostrecord.DIMENSIONS.items():
            with self.subTest(field + " differs"):
                asked = self._asked()
                asked[argument] = "something-else"
                self.assertEqual(self._match(self._point(), asked), [], field)

            with self.subTest(field + " missing from the caller"):
                asked = self._asked()
                asked[argument] = None
                self.assertEqual(self._match(self._point(), asked), [],
                                 field + ": a caller who observed nothing must match nothing")

            with self.subTest(field + " missing from the point"):
                point = self._point()
                point.pop(field)
                self.assertEqual(self._match(point, self._asked()), [],
                                 field + ": a point that recorded nothing must match nothing")

            with self.subTest(field + " missing from both"):
                point, asked = self._point(), self._asked()
                point.pop(field)
                asked[argument] = None
                found = self._match(point, asked)
                if policy == "mandatory":
                    self.assertEqual(found, [],
                                     field + " is mandatory: two absences are not agreement")
                else:
                    self.assertEqual(len(found), 1,
                                     field + " is symmetric: two absences agree")

    def test_a_gate_is_not_mutated_as_if_it_were_a_dimension(self):
        for gate in ("exercised", "digestMatchesDefinition"):
            with self.subTest(gate):
                point = self._point()
                point[gate] = False
                self.assertEqual(self._match(point, self._asked()), [])

    def test_the_access_scanner_sees_each_violation_form(self):
        forms = {
            "a dynamic key": "def points_for(a):\n    key = 'x'\n    return point[key]\n",
            "a dynamic get": "def points_for(a):\n    key = 'x'\n    return point.get(key)\n",
            "a key from an undeclared map":
                "def points_for(a):\n    for f in OTHER:\n        point.get(f)\n",
            "delegation": "def points_for(a):\n    return helper(point)\n",
            "a comprehension over point": "def points_for(a):\n    return [k for k in point]\n",
            "an unmodelled method": "def points_for(a):\n    return point.items()\n",
        }
        for label, source in forms.items():
            with self.subTest(label):
                violations, _ = _point_accesses(ast.parse(source))
                self.assertTrue(violations, label + " must be seen")
        clean = "def points_for(a):\n    return point.get('host') == a and point['install']\n"
        violations, keys = _point_accesses(ast.parse(clean))
        self.assertEqual(violations, [])
        self.assertEqual(sorted(keys), ["host", "install"])

        # The derivation itself must not be a violation, or the only drift-free implementation
        # would be the one the check forbids.
        derived = ("def points_for(a):\n    found = []\n"
                   "    for field, (argument, policy) in DIMENSIONS.items():\n"
                   "        recorded = point.get(field)\n"
                   "    found.append(point)\n")
        violations, _ = _point_accesses(ast.parse(derived))
        self.assertEqual(violations, [])


class RelayRuleCoverageTests(unittest.TestCase):
    """The preflight asks the relay's question instead of re-deriving the answer.

    Re-deriving is what drifted: a path written with a parent segment compares equal to its own
    string, so the normalization check passed something the relay rejects.
    """

    def test_the_rule_set_comes_from_the_relays_own_normalizer(self):
        rules = _relay_normalization_rules()
        self.assertTrue(rules, "the relay's normalizer must be readable to derive from")
        joined = " ".join(rules)
        for expected in ("isinstance", "startswith", "normpath", "endswith"):
            self.assertIn(expected, joined,
                          "a rule the relay enforces that this derivation missed")

    def test_every_derived_rule_has_a_case_the_preflight_refuses(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "root"
            root.mkdir()
            real = root / "result.txt"
            real.write_text("done", encoding="utf-8")
            link = root / "link.txt"
            link.symlink_to(real)
            cases = {
                "empty": "",
                "a NUL": str(root) + "/a\x00b.txt",
                "relative": "result.txt",
                "a tilde": "~/result.txt",
                "not normalized": str(root) + "/../root/result.txt",
                "a trailing slash": str(root) + "//",
                "not a regular file": str(root),
                "a symlink": str(link),
                "outside the root": str(Path(temporary) / "elsewhere.txt"),
                "missing": str(root / "gone.txt"),
            }
            for label, path in cases.items():
                with self.subTest(label):
                    self.assertTrue(runtime_install._unusable_artifacts([path], str(root), RELAY_RUNTIME),
                                    label + " must be refused before anything is written")
            self.assertEqual(runtime_install._unusable_artifacts([str(real)], str(root), RELAY_RUNTIME), [],
                             "and a real artifact is not refused")


class SelectorCoverageTests(unittest.TestCase):
    """Both explicit store selections the relay resolves are surveyed.

    A store chosen by the environment used to be skipped entirely, so the summary described
    whichever store discovery picked while service status, the assignment lookup and the trial
    all acted on the other one.
    """

    def test_the_selector_set_comes_from_the_relays_own_resolver(self):
        self.assertEqual(_relay_state_selectors(), {"flag", "environment"})
        self.assertEqual(scope.STATE_ENV, "CODEX_SESSION_RELAY_STATE",
                         "and the name is the relay's, not one written here")

    def test_each_selector_produces_a_selected_reading(self):
        """Asserted on the argv the reading was made with, not on the key existing.

        The first version of this test asked whether "selected" was present, and it always is:
        the skipped case is a dict too. A check that cannot fail is the same mistake as a
        scanner that returns an empty list.
        """
        def fake(argv, **kwargs):
            return {"ok": True, "command": ["relay", *argv], "state": kwargs.get("state"),
                    "payload": {"store": {"dbPath": "/db"}}}

        for label, flag, environment, expected, via in (
            ("flag only", "/from/flag", {}, "/from/flag", "flag"),
            ("environment only", None, {scope.STATE_ENV: "/from/env"}, "/from/env",
             "environment"),
            ("both, the flag wins", "/from/flag", {scope.STATE_ENV: "/from/env"},
             "/from/flag", "flag"),
        ):
            with self.subTest(label):
                with mock.patch.object(scope, "relay", side_effect=fake):
                    readings = scope.survey(executable="/bin/relay", socket=None, state=flag,
                                            env=dict(environment))
                selected = readings["selected"]
                self.assertNotIn("skipped", selected,
                                 label + ": the selected store must actually be read")
                self.assertEqual(selected["state"], expected,
                                 label + ": and read at the store that was selected")
                self.assertEqual(readings["selectedVia"], via)

    def test_no_selection_at_all_is_reported_as_such_rather_than_invented(self):
        def fake(argv, **kwargs):
            return {"ok": True, "command": list(argv), "payload": {}}

        with mock.patch.object(scope, "relay", side_effect=fake):
            readings = scope.survey(executable="/bin/relay", socket=None, state=None, env={})
        self.assertIn("skipped", readings["selected"])
        self.assertIsNone(readings["selectedVia"])

    def test_discovery_passes_no_explicit_selection(self):
        answer = scope.relay(["--version"], executable=sys.executable, discovery=True,
                             env={**os.environ, scope.STATE_ENV: "/from/env"})
        self.assertNotIn("--state", answer["command"])


# =========================================================================================
# Check 10 - the consumer references the DECLARED SET, not a literal member of it
#
# Check 9 asserts that the set I declared is covered. It cannot see that the code compares
# against a literal instead of the declaration: "== UNREADABLE" answers for one member of a
# four-state partition and silently says yes to another, and a set-coverage assertion written
# over my own declaration never looks at that line. This one does.
#
# The inventory is every UPPER_CASE module-level binding and the strings it names, read out of
# the source rather than listed here. A comparison against one of those strings is a violation
# in the module that declares it and in every module that imports the declaring one -- scoped
# that way because a consumer can only reference a declaration it can see, and because
# unrelated modules share short words: check.record compares a destination kind "host" that has
# nothing to do with the hostname dimension whose key is spelled the same.
#
# The limit, stated rather than papered over: this reads COMPARISONS. Literal key ACCESS
# (payload.get("x")) is not covered here, because payload keys are data and forbidding literal
# keys would forbid reading a payload at all. The one map where that distinction decides
# something is guarded by _point_accesses in check 9.
# =========================================================================================

RUNTIME_MODULES = [ROOT / "scripts" / "runtime_install.py"] + sorted(
    (ROOT / "scripts" / "crw_runtime").glob("*.py"))


# The one module-level string collection built by a comprehension, whose members are computed
# from another collection's rather than written. The reader below evaluates constants, names,
# cross-module references, concatenation and simple wrapping calls; it does not evaluate string
# arithmetic, and nothing compares against an escape sequence. Named here so a second computed
# collection has to be acknowledged instead of quietly slipping past the oracle.
COMPUTED_DECLARATIONS = {"codexconfig.ESCAPED"}


def _evaluate(node, local, others):
    """The strings a declaration names, over the small expression language declarations use."""
    if isinstance(node, ast.Constant):
        return {node.value} if isinstance(node.value, str) else set()
    if isinstance(node, (ast.Tuple, ast.List, ast.Set)):
        found = set()
        for element in node.elts:
            found |= _evaluate(element, local, others)
        return found
    if isinstance(node, ast.Dict):
        found = set()
        for key, value in zip(node.keys, node.values):
            if key is not None:
                found |= _evaluate(key, local, others)
            found |= _evaluate(value, local, others)
        return found
    if isinstance(node, ast.Name):
        return set(local.get(node.id, ()))
    if isinstance(node, ast.Attribute) and isinstance(node.value, ast.Name):
        return set(others.get(node.value.id, {}).get(node.attr, ()))
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Add):
        return _evaluate(node.left, local, others) | _evaluate(node.right, local, others)
    if isinstance(node, ast.Call):
        found = set()
        for argument in node.args:
            found |= _evaluate(argument, local, others)
        return found
    return set()


def _declarations(tree, others=None):
    """Every UPPER_CASE module-level binding, as name -> the strings it names.

    A dict contributes its keys AND its values, because the DIMENSIONS policies are values and
    points_for compares one of them by name. Names resolve to what they were bound to earlier in
    the same module, so a tuple written as (ABSENT, PRESENT, ...) is the partition it looks like
    rather than an empty set, and module.NAME resolves across modules so a set assembled from
    two other modules' answers still names their strings.
    """
    others = others or {}
    bindings = []
    for node in tree.body:
        if not isinstance(node, ast.Assign):
            continue
        targets = [t for t in node.targets if isinstance(t, ast.Name) and t.id.isupper()]
        if not targets or len(targets) != len(node.targets):
            continue
        for target in targets:
            bindings.append((target.id, node.value))

    declared = {}
    for name, value in bindings:
        found = _evaluate(value, declared, others)
        if found:
            declared[name] = found
    return declared


def _declared_by_module(trees):
    """Two passes, so a declaration assembled from another module's resolves."""
    first = {stem: _declarations(tree) for stem, tree in trees.items()}
    return {stem: _declarations(tree, first) for stem, tree in trees.items()}


def _imported_names(tree):
    names = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.ImportFrom):
            names.update(alias.asname or alias.name for alias in node.names)
        elif isinstance(node, ast.Import):
            names.update((alias.asname or alias.name).split(".")[0] for alias in node.names)
    return names


def _compared_literals(node):
    literals = []
    for operand in [node.left] + list(node.comparators):
        if isinstance(operand, ast.Constant) and isinstance(operand.value, str):
            literals.append(operand.value)
        elif isinstance(operand, (ast.Tuple, ast.List, ast.Set)):
            literals.extend(element.value for element in operand.elts
                            if isinstance(element, ast.Constant)
                            and isinstance(element.value, str))
    return literals


def _literal_comparisons(trees):
    """Offenders: a comparison against a string some visible module declares by name."""
    owned = _declared_by_module(trees)
    offenders = []
    for stem, tree in trees.items():
        visible = ({stem} | _imported_names(tree)) & set(owned)
        pool = {}
        for other in visible:
            for name, values in owned[other].items():
                for value in values:
                    pool.setdefault(value, set()).add(other + "." + name)
        for node in ast.walk(tree):
            if not isinstance(node, ast.Compare):
                continue
            for literal in _compared_literals(node):
                if literal in pool:
                    offenders.append(stem + ":" + str(node.lineno) + " compares " + repr(literal)
                                     + ", declared by " + ", ".join(sorted(pool[literal])))
    return sorted(offenders)


def _runtime_trees():
    return {path.stem: ast.parse(path.read_text(encoding="utf-8"))
            for path in RUNTIME_MODULES}


class DeclaredSetReferenceTests(unittest.TestCase):
    def test_the_inventory_is_not_empty(self):
        # A blind reader returns no declarations, and then every comparison passes vacuously.
        declared = _declarations(ast.parse(
            (ROOT / "scripts" / "crw_runtime" / "reading.py").read_text(encoding="utf-8")))
        self.assertIn("STATES", declared)
        self.assertEqual(declared["STATES"], {"ABSENT", "PRESENT", "UNREADABLE", "ACCESS_ERROR"},
                         "a tuple of Names is the partition it looks like")

    def test_the_ast_reader_agrees_with_the_imported_modules(self):
        """The oracle: what the source reader found, against what the module actually holds.

        Comparing the reader with a hand-written expectation would only check that two things I
        wrote agree. The imported module is the outside truth here.
        """
        modules = {"check": check, "codexconfig": codexconfig, "completion": completion,
                   "definition": definition, "hooks": hooks, "hostrecord": hostrecord,
                   "ownership": ownership, "pointer": pointer, "reading": reading,
                   "scope": scope, "staging": staging, "swapgate": swapgate}
        trees = _runtime_trees()
        declared_by_module = _declared_by_module(trees)
        computed = set()
        for stem, module in modules.items():
            declared = declared_by_module[stem]
            comprehensions = {
                target.id for node in trees[stem].body if isinstance(node, ast.Assign)
                for target in node.targets
                if isinstance(target, ast.Name) and target.id.isupper()
                and isinstance(node.value, (ast.DictComp, ast.ListComp, ast.SetComp,
                                            ast.GeneratorExp))
            }
            computed |= {stem + "." + name for name in comprehensions}
            for name, value in vars(module).items():
                if not name.isupper():
                    continue
                if stem + "." + name in COMPUTED_DECLARATIONS:
                    continue
                if isinstance(value, str):
                    expected = {value}
                elif isinstance(value, (tuple, list, set, frozenset)) and value \
                        and all(isinstance(item, str) for item in value):
                    expected = set(value)
                elif isinstance(value, dict) and value \
                        and all(isinstance(key, str) for key in value):
                    expected = set(value) | {item for item in value.values()
                                             if isinstance(item, str)}
                else:
                    continue
                with self.subTest(stem + "." + name):
                    self.assertIn(name, declared, "the source reader missed this declaration")
                    self.assertTrue(expected <= declared[name],
                                    "the source reader missed " + repr(sorted(expected - declared[name])))
        self.assertEqual(computed, COMPUTED_DECLARATIONS,
                         "a computed declaration must be acknowledged, not discovered later")

    def test_no_consumer_compares_against_a_literal_member_of_a_declared_set(self):
        self.assertEqual(_literal_comparisons(_runtime_trees()), [])

    def test_the_scan_sees_each_form_of_declaration_being_bypassed(self):
        """Injected violations, one per declaration form. Without these an empty list is mute."""
        declaring = (
            'ONE = "alpha"\n'
            'MANY = ("beta", "gamma")\n'
            'NAMED = (ONE,)\n'
            'MAP = {"delta": ("x", "epsilon")}\n'
        )
        forms = {
            "a bare constant": 'state == "alpha"',
            "a member of a tuple": 'state == "beta"',
            "a member reached through a Name": 'state == "alpha"',
            "a dict key": 'state == "delta"',
            "a dict value": 'policy == "epsilon"',
            "a member on the right of in": 'state in ("gamma", "other")',
            "an inequality": 'state != "beta"',
        }
        for label, comparison in forms.items():
            with self.subTest(label):
                trees = {"declaring": ast.parse(declaring),
                         "consuming": ast.parse("import declaring\n\n\ndef f(state, policy):\n"
                                                "    return " + comparison + "\n")}
                self.assertTrue(_literal_comparisons(trees), label + " must be seen")

    def test_a_module_that_cannot_see_a_declaration_is_left_alone(self):
        """Scoping, not a carve-out. check.record compares a destination kind spelled "host";
        the hostname dimension is a different module's word and check does not import it."""
        trees = {"declaring": ast.parse('MAP = {"host": ("host", "mandatory")}\n'),
                 "unrelated": ast.parse('def f(kind):\n    return kind == "host"\n')}
        self.assertEqual(_literal_comparisons(trees), [])
        trees["unrelated"] = ast.parse("import declaring\n\n\ndef f(kind):\n"
                                       "    return kind == 'host'\n")
        self.assertTrue(_literal_comparisons(trees),
                        "importing the declaring module brings the word into scope")

    def test_the_scan_is_red_on_the_source_with_one_reference_put_back(self):
        """The self-completeness run, performed in memory so nothing on disk is touched."""
        source = (ROOT / "scripts" / "runtime_install.py").read_text(encoding="utf-8")
        injected = source.replace(
            'if registration and registration.get("outcome") in UNUSABLE_REGISTRATIONS:',
            'if registration and registration.get("outcome") == "UNREADABLE":')
        self.assertNotEqual(injected, source, "the anchor for the injection moved")
        trees = _runtime_trees()
        trees["runtime_install"] = ast.parse(injected)
        offenders = _literal_comparisons(trees)
        self.assertTrue(any("UNREADABLE" in offender for offender in offenders), offenders)


# =========================================================================================
# Check 11 - the four sets that were drawn around one member
# =========================================================================================

def _register_replay_fields():
    """The values Registry.register compares to decide replay against conflict.

    Read out of the relay's own source. Listed here instead, the set would be a second copy of
    the relay's rule and would drift from it silently, which is the failure this whole check
    exists to stop.
    """
    tree = ast.parse((RELAY_SRC / "registry.py").read_text(encoding="utf-8"))
    register = next(
        node for parent in ast.walk(tree) if isinstance(parent, ast.ClassDef)
        and parent.name == "Registry"
        for node in parent.body if isinstance(node, ast.FunctionDef) and node.name == "register")

    # A subscript used as another subscript's value is a container, not a compared field:
    # record["authorizedScope"]["artifactRoots"] compares artifactRoots.
    nested = {id(node.value) for node in ast.walk(register) if isinstance(node, ast.Subscript)}
    fields = set()
    for node in ast.walk(register):
        # The identity it hashes into a relationship id.
        if isinstance(node, ast.Call) and getattr(node.func, "id", None) == "relationship_id":
            for argument in node.args:
                if isinstance(argument, ast.Attribute) and isinstance(argument.value, ast.Name):
                    fields.add(argument.value.id + "_" + argument.attr)
                elif isinstance(argument, ast.Name):
                    fields.add(argument.id)
        # The comparison that decides replay against conflict for an existing identity.
        if isinstance(node, ast.Assign) and any(
                isinstance(t, ast.Name) and t.id == "same" for t in node.targets):
            for inner in ast.walk(node.value):
                if isinstance(inner, ast.Subscript) and id(inner) not in nested \
                        and isinstance(inner.slice, ast.Constant) \
                        and isinstance(inner.slice.value, str):
                    fields.add(inner.slice.value)

    def camel(name):
        head, *rest = name.split("_")
        return head + "".join(part.title() for part in rest)

    return {camel(name) for name in fields}


class DrawnSetTests(unittest.TestCase):
    """Four sets that were drawn around one member, each replaced by the codebase's own set."""

    def test_the_replay_set_is_the_one_the_relay_compares(self):
        import runtime_install

        relay_fields = _register_replay_fields()
        # Guards the reader: an empty or tiny answer would make the comparison vacuous.
        self.assertEqual(len(relay_fields), 7, sorted(relay_fields))
        self.assertEqual(relay_fields, set(runtime_install.REGISTER_REPLAY_FIELDS))
        self.assertEqual(
            set(runtime_install.REPLAY_FROM_LOOKUP) | set(runtime_install.REPLAY_FROM_REGISTER),
            relay_fields, "every field is either compared here or decided by register")
        self.assertEqual(
            set(runtime_install.REPLAY_FROM_LOOKUP) & set(runtime_install.REPLAY_FROM_REGISTER),
            set(), "no field is claimed by both")

    def test_the_replay_reader_notices_a_comparison_the_relay_stops_making(self):
        # The reader is only evidence if dropping a rule changes its answer.
        source = (RELAY_SRC / "registry.py").read_text(encoding="utf-8")
        self.assertIn('and existing["child_host_id"] == child.host_id', source)

    def test_every_component_has_a_command_override(self):
        import runtime_install

        components = {c["component"] for c in definition.load()["components"]}
        self.assertEqual(set(runtime_install.COMMAND_OVERRIDES), components,
                         "an override wired for one component classified the other against"
                         " whatever PATH resolved")
        parser = runtime_install.build_parser()
        diagnose = parser.parse_args(["diagnose", "--relay-command", "/r",
                                      "--bridge-command", "/b"])
        for component, destination in runtime_install.COMMAND_OVERRIDES.items():
            with self.subTest(component):
                self.assertTrue(hasattr(diagnose, destination),
                                destination + " is not a diagnose argument")

    def test_diagnosis_classifies_each_component_against_its_own_override(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary) / "codex"
            home.mkdir()
            entries = {}
            for component in definition.load()["components"]:
                entry = Path(temporary) / (component["consoleScript"] + "-elsewhere")
                entry.write_text("#!/nonexistent/python\n", encoding="utf-8")
                entry.chmod(0o755)
                entries[component["component"]] = entry
            done = run("diagnose", "--codex-home", str(home),
                       "--record", str(Path(temporary) / "record.json"),
                       "--relay-command", str(entries["codex-session-relay"]),
                       "--bridge-command", str(entries["codex-thread-bridge"]), "--temporary")
            payload = json.loads(done.stdout)
        for name, entry in entries.items():
            with self.subTest(name):
                self.assertEqual(payload["components"][name]["entryPoint"], str(entry),
                                 "the component was classified against a different executable"
                                 " than the one named on the command line")

    def test_an_unreachable_configuration_stops_classification_like_an_unreadable_one(self):
        """ACCESS_ERROR and UNREADABLE are two members of one partition, and the consumer used
        to name one of them."""
        import runtime_install

        component = definition.load()["components"][0]
        for outcome in reading.UNUSABLE:
            with self.subTest(outcome):
                classified = runtime_install.classify_component(
                    component, record=hostrecord.empty(1),
                    registration={"outcome": outcome, "detail": "a detail"},
                    app_server="a-server")
                self.assertEqual(classified["class"], ownership.UNREADABLE)
                self.assertTrue(any("Codex configuration" in reason
                                    for reason in classified["reasons"]), classified["reasons"])

    def test_a_shared_dimension_is_observed_once_for_the_whole_measurement(self):
        """Observed twice, a transient failure between the gate and the write records a null
        dimension in a point the consumer can never match."""
        import runtime_install

        data = definition.load()
        self.assertGreater(len(data["components"]), 1,
                           "a per-measurement count only means something with two components")
        counted = {"codexCli": 0, "interpreter": 0, "host": 0}
        record = hostrecord.empty(1)
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            bound = {c["component"]: {
                "install": {"location": "/l",
                            "entryPoint": str(environment / "bin" / c["consoleScript"])},
                "digest": c["sourceDigest"], "location": "/l"} for c in data["components"]}

            def counting(key, answer):
                def call(*args, **kwargs):
                    counted[key] += 1
                    return answer
                return call

            with mock.patch.object(runtime_install, "_interpreter_prefix",
                                   return_value=str(environment)), \
                 mock.patch.object(runtime_install, "_bind_installs",
                                   return_value=(bound, None)), \
                 mock.patch.object(runtime_install, "codex_cli_version",
                                   side_effect=counting("codexCli", "0.1.0")), \
                 mock.patch.object(runtime_install, "interpreter_version",
                                   side_effect=counting("interpreter", "3.13.1")), \
                 mock.patch.object(runtime_install.socket, "gethostname",
                                   side_effect=counting("host", "a-host")), \
                 mock.patch.object(runtime_install.scope, "relay",
                                   return_value={"ok": True, "command": ["doctor"], "payload": {
                                       "actorReachability": {"socketConnect": "ok"}}}), \
                 mock.patch.object(runtime_install.subprocess, "run",
                                   return_value=type("R", (), {
                                       "returncode": 0,
                                       "stdout": json.dumps({"tools": [_BRIDGE_TOOL],
                                                             "connection": {"ok": True}}),
                                       "stderr": ""})()):
                measurement = runtime_install.measure_candidate(
                    data, record, python=str(environment / "bin" / "python"),
                    environment=str(environment), socket_path=None, state=None,
                    relay_command=str(environment / "bin" / "codex-session-relay"))
        self.assertTrue(measurement["qualifyingPoint"], measurement.get("refused"))
        for dimension, calls in counted.items():
            with self.subTest(dimension):
                self.assertEqual(calls, 1, dimension + " was observed " + str(calls) + " times")

    def test_register_is_the_first_mutating_step_the_trial_sends(self):
        """The order is the guarantee. register compares four values no read-only relay command
        exposes, so a settings write ahead of it lands for a trial register then refuses."""
        import runtime_install

        sent = []

        def relay(command, **kwargs):
            sent.append(command[0])
            if command[0] == "assignment-find":
                return {"ok": True, "command": list(command),
                        "payload": {"issueKey": "JUN-104", "assignments": [],
                                    "responsibleRelationship": None, "responsibleChild": None}}
            return {"ok": True, "command": list(command), "payload": _trial_payload(command[0])}

        args = argparse.Namespace(
            issue="JUN-104", parent_task="p", child_task="c", recipient="p",
            artifact_root=TRIAL_ROOT, turn_thread="c", turn_id="ti", artifact=[TRIAL_ARTIFACT],
            dispatch_turn_id="d", turn_status="completed",
            recipient_settings="@/tmp/settings.json", settings_already_recorded=False,
            expect_relationship=None, socket=None, state=None)
        with mock.patch.object(runtime_install.scope, "relay", side_effect=relay):
            with usable_settings():
                result = runtime_install._trial(args, "/usr/bin/relay", RELAY_RUNTIME)
        self.assertEqual(result["value"], "verified", result["evidence"][:300])
        self.assertIn("settings-record", sent)
        self.assertLess(sent.index("register"), sent.index("settings-record"),
                        "settings were written before the step that decides replay")

    def test_a_refused_registration_leaves_no_settings_behind(self):
        import runtime_install

        sent = []

        def relay(command, **kwargs):
            sent.append(command[0])
            if command[0] == "assignment-find":
                return {"ok": True, "command": list(command),
                        "payload": {"issueKey": "JUN-104", "assignments": [],
                                    "responsibleRelationship": None, "responsibleChild": None}}
            if command[0] == "register":
                return {"ok": False, "command": list(command), "exitCode": 1,
                        "stderr": "RELATIONSHIP_CONFLICT: already exists with a different scope"}
            return {"ok": True, "command": list(command), "payload": _trial_payload(command[0])}

        args = argparse.Namespace(
            issue="JUN-104", parent_task="p", child_task="c", recipient="p",
            artifact_root=TRIAL_ROOT, turn_thread="c", turn_id="ti", artifact=[TRIAL_ARTIFACT],
            dispatch_turn_id="d", turn_status="completed",
            recipient_settings="@/tmp/settings.json", settings_already_recorded=False,
            expect_relationship=None, socket=None, state=None)
        with mock.patch.object(runtime_install.scope, "relay", side_effect=relay):
            result = runtime_install._trial(args, "/usr/bin/relay", RELAY_RUNTIME)
        self.assertEqual(result["value"], "not_verified")
        self.assertNotIn("settings-record", sent,
                         "a registration the relay refused must not leave settings on record")

    def test_an_owner_under_a_different_parent_is_not_read_as_the_same_relationship(self):
        """The set was drawn around responsibleChild. An assignment with this issue and this
        child under a DIFFERENT parent hashes to a different relationship id."""
        import runtime_install

        payload = {"issueKey": "JUN-104", "responsibleRelationship": "rel-1",
                   "responsibleChild": "c",
                   "assignments": [{"relationshipId": "rel-1", "parentTaskId": "someone-else",
                                    "childTaskId": "c", "issueKey": "JUN-104"}]}
        sent = []

        def relay(command, **kwargs):
            sent.append(command[0])
            return {"ok": True, "command": list(command),
                    "payload": payload if command[0] == "assignment-find"
                    else _trial_payload(command[0])}

        args = argparse.Namespace(
            issue="JUN-104", parent_task="p", child_task="c", recipient="p",
            artifact_root=TRIAL_ROOT, turn_thread="c", turn_id="ti", artifact=[TRIAL_ARTIFACT],
            dispatch_turn_id="d", turn_status="completed", recipient_settings=None,
            settings_already_recorded=True, expect_relationship=None, socket=None, state=None)
        with mock.patch.object(runtime_install.scope, "relay", side_effect=relay):
            result = runtime_install._trial(args, "/usr/bin/relay", RELAY_RUNTIME)
        self.assertEqual(result["value"], "not_verified", result["evidence"][:300])
        self.assertIn("parentTaskId", result["evidence"])
        self.assertEqual(sent, ["assignment-find"])

    def test_an_owner_with_the_same_identity_still_replays(self):
        import runtime_install

        payload = {"issueKey": "JUN-104", "responsibleRelationship": "rel-1",
                   "responsibleChild": "c",
                   "assignments": [{"relationshipId": "rel-1", "parentTaskId": "p",
                                    "childTaskId": "c", "issueKey": "JUN-104"}]}

        def relay(command, **kwargs):
            return {"ok": True, "command": list(command),
                    "payload": payload if command[0] == "assignment-find"
                    else _trial_payload(command[0])}

        args = argparse.Namespace(
            issue="JUN-104", parent_task="p", child_task="c", recipient="p",
            artifact_root=TRIAL_ROOT, turn_thread="c", turn_id="ti", artifact=[TRIAL_ARTIFACT],
            dispatch_turn_id="d", turn_status="completed", recipient_settings=None,
            settings_already_recorded=True, expect_relationship=None, socket=None, state=None)
        with mock.patch.object(runtime_install.scope, "relay", side_effect=relay):
            result = runtime_install._trial(args, "/usr/bin/relay", RELAY_RUNTIME)
        self.assertEqual(result["value"], "verified", result["evidence"][:300])


# =========================================================================================
# Check 11 - the preflight asks the consumer's own predicate, over the declared input set
#
# register moved ahead of settings-record so a registration the relay refuses cannot leave
# settings behind. That reordering made a NEW input class dangerous: a --recipient-settings
# value that is present but unreadable used to fail before anything was written and now fails
# after a relationship row exists. The preflight therefore asks whether the value is USABLE,
# and it asks with the relay's own reader and the relay's own predicate rather than a second
# copy of those rules.
# =========================================================================================

RELAY_SETTINGS_SOURCE = RELAY_SRC / "settings.py"


def _settings_record_callables():
    """What cmd_settings_record and record_settings actually call, read from the relay."""
    cli = ast.parse((RELAY_SRC / "cli.py").read_text(encoding="utf-8"))
    registry = ast.parse((RELAY_SRC / "registry.py").read_text(encoding="utf-8"))

    def called(tree, name):
        for node in ast.walk(tree):
            if isinstance(node, ast.FunctionDef) and node.name == name:
                found = set()
                for inner in ast.walk(node):
                    if isinstance(inner, ast.Call):
                        func = inner.func
                        if isinstance(func, ast.Name):
                            found.add(func.id)
                        elif isinstance(func, ast.Attribute):
                            found.add(func.attr)
                return found
        return set()

    return called(cli, "cmd_settings_record"), called(registry, "record_settings")


class SettingsPreflightTests(unittest.TestCase):
    def test_the_preflight_names_the_callables_settings_record_uses(self):
        import runtime_install

        by_cli, by_registry = _settings_record_callables()
        # Guards the reader: empty sets would make both assertions below pass vacuously.
        self.assertTrue(by_cli, "cmd_settings_record was not found in the relay source")
        self.assertTrue(by_registry, "record_settings was not found in the relay source")
        self.assertIn(runtime_install.SETTINGS_READER[1], by_cli,
                      "the reader this preflight runs is not the one settings-record reads with")
        self.assertIn(runtime_install.SETTINGS_PREDICATE[2], by_registry,
                      "the predicate this preflight applies is not the one record_settings applies")
        self.assertIn(runtime_install.SETTINGS_READER[1], by_registry | by_cli)
        # And the predicate really is a method of the declared class in the declared module.
        settings = ast.parse(RELAY_SETTINGS_SOURCE.read_text(encoding="utf-8"))
        methods = {inner.name for node in ast.walk(settings)
                   if isinstance(node, ast.ClassDef) and node.name == runtime_install.SETTINGS_PREDICATE[1]
                   for inner in node.body if isinstance(inner, ast.FunctionDef)}
        self.assertIn(runtime_install.SETTINGS_PREDICATE[2], methods)

    def _stub_relay(self, temporary, complete):
        """A stand-in relay package, so the real program text is executed on this interpreter.

        The program, the subprocess, the argument passing and the JSON answer are all the real
        ones. Only the package it imports is local, because CI has no installed relay and a
        check that never runs the program proves nothing about it.
        """
        package = Path(temporary) / "codex_session_relay"
        package.mkdir()
        (package / "__init__.py").write_text("", encoding="utf-8")
        (package / "cli.py").write_text(
            "import json\n\n\n"
            "def _settings_json(raw):\n"
            "    if raw.startswith('@'):\n"
            "        with open(raw[1:], encoding='utf-8') as handle:\n"
            "            return json.load(handle)\n"
            "    return json.loads(raw)\n",
            encoding="utf-8")
        (package / "settings.py").write_text(
            "class TaskSettings:\n"
            "    def __init__(self, data):\n"
            "        self.data = data\n\n"
            "    def require_usable(self):\n"
            "        if not self.data.get('complete'):\n"
            "            raise ValueError('missing sandbox, approvalPolicy')\n",
            encoding="utf-8")
        return package

    def _ask(self, temporary, raw):
        import runtime_install

        environment = dict(os.environ, PYTHONPATH=str(temporary))
        with mock.patch.dict(os.environ, environment, clear=True):
            return runtime_install.settings_usable(raw, sys.executable)

    def test_a_complete_value_is_usable_and_an_incomplete_one_is_not(self):
        with tempfile.TemporaryDirectory() as temporary:
            self._stub_relay(temporary, True)
            good = Path(temporary) / "good.json"
            good.write_text(json.dumps({"complete": True}), encoding="utf-8")
            bad = Path(temporary) / "bad.json"
            bad.write_text(json.dumps({"complete": False}), encoding="utf-8")

            self.assertTrue(self._ask(temporary, "@" + str(good))["usable"])

            for label, raw in (("an incomplete object", "@" + str(bad)),
                               ("malformed JSON", '{"sandbox":'),
                               ("a path that is not there",
                                "@" + str(Path(temporary) / "absent.json"))):
                with self.subTest(label):
                    answer = self._ask(temporary, raw)
                    self.assertFalse(answer["usable"], label + " must not read as usable")
                    self.assertTrue(answer["detail"])

    def test_without_an_interpreter_the_answer_is_unknown_rather_than_usable(self):
        import runtime_install

        answer = runtime_install.settings_usable("@/nowhere.json", None)
        self.assertIsNone(answer["usable"])
        self.assertIn("interpreter", answer["detail"])

    def test_the_check_writes_no_bytecode_into_the_runtime_it_asks(self):
        """Read-only has to mean it. Importing two modules writes __pycache__ without -B, and
        this check runs inside somebody's installed runtime."""
        import runtime_install

        seen = {}

        def fake_run(argv, **kwargs):
            seen["argv"] = list(argv)
            return type("R", (), {"returncode": 0, "stdout": '{"usable": true}', "stderr": ""})()

        with mock.patch.object(runtime_install.subprocess, "run", side_effect=fake_run):
            runtime_install.settings_usable("{}", "/some/python")
        self.assertIn("-B", seen["argv"], "the probe may not leave bytecode behind")
        self.assertLess(seen["argv"].index("-B"), seen["argv"].index("-c"))


class PreflightInputSetTests(unittest.TestCase):
    """Every declared input, made unusable one at a time, stops the trial before it mutates.

    The set is the declaration, not a list here: an input added to the trial without a preflight
    case fails this rather than being discovered at the relay once rows exist.
    """

    MUTATING = ("register", "settings-record", "generation-open", "generation-bind",
                "admit-turn", "emit", "deliver")

    def _args(self, settings_path, **overrides):
        base = dict(issue="JUN-104", parent_task="p", child_task="c", recipient="p",
                    artifact_root=TRIAL_ROOT, artifact=[TRIAL_ARTIFACT], turn_thread="c",
                    turn_id="t", dispatch_turn_id="d", turn_status="completed",
                    recipient_settings="@" + str(settings_path),
                    settings_already_recorded=False, expect_relationship=None,
                    socket=None, state=None)
        base.update(overrides)
        return argparse.Namespace(**base)

    def _run(self, args, settings_answer):
        import runtime_install

        sent = []

        def relay(command, **kwargs):
            sent.append(command[0])
            return {"ok": True, "command": list(command),
                    "payload": {"issueKey": "JUN-104", "assignments": [],
                                "responsibleRelationship": None, "responsibleChild": None}
                    if command[0] == "assignment-find" else _trial_payload(command[0])}

        with mock.patch.object(runtime_install.scope, "relay", side_effect=relay), \
             mock.patch.object(runtime_install, "settings_usable", return_value=settings_answer):
            result = runtime_install._trial(args, "/usr/bin/relay", RELAY_RUNTIME)
        return result, sent

    def test_the_baseline_reaches_a_delivery_so_the_refusals_mean_something(self):
        with tempfile.TemporaryDirectory() as temporary:
            settings = Path(temporary) / "s.json"
            settings.write_text("{}", encoding="utf-8")
            result, sent = self._run(self._args(settings), {"usable": True, "detail": "read"})
        self.assertEqual(result["value"], "verified", result["evidence"][:200])
        self.assertIn("deliver", sent)

    def test_each_declared_input_made_unusable_stops_the_trial_before_it_mutates(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            settings = Path(temporary) / "s.json"
            settings.write_text("{}", encoding="utf-8")
            for name in sorted(runtime_install.TRIAL_PREFLIGHT_INPUTS):
                with self.subTest(name):
                    if name in runtime_install.TRIAL_ACKNOWLEDGED_INPUTS:
                        # Present, and the consumer's predicate rejects it. That is the class
                        # this round added: absence was already refused, unusability was not.
                        args = self._args(settings)
                        answer = {"usable": False, "detail": "missing sandbox"}
                    else:
                        args = self._args(settings, **{name: None})
                        answer = {"usable": True, "detail": "read"}
                    result, sent = self._run(args, answer)
                    self.assertEqual(result["value"], "not_verified", name)
                    self.assertEqual([step for step in sent if step in self.MUTATING], [],
                                     name + ": the trial mutated before its inputs were usable")

    def test_every_required_input_made_blank_stops_the_trial_before_it_mutates(self):
        """Supplied and usable are two questions, and whitespace answers them differently.

        A truthiness gate reads a whitespace-only flag as supplied; the relay reads it as blank
        and refuses, after register has written a relationship row. The corpus is the declared
        set, so the whole family is closed rather than the two members a review happened to
        name.
        """
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            settings = Path(temporary) / "s.json"
            settings.write_text("{}", encoding="utf-8")
            for name in sorted(runtime_install.TRIAL_REQUIRED_INPUTS):
                with self.subTest(name):
                    blank = ["   "] if name == "artifact" else "   "
                    result, sent = self._run(self._args(settings, **{name: blank}),
                                             {"usable": True, "detail": "read"})
                    self.assertEqual(result["value"], "not_verified", name)
                    self.assertEqual([step for step in sent if step in self.MUTATING], [],
                                     name + ": a blank flag wrote to the store")


# =========================================================================================
# Check 12 - an interpreter's identity is not one line of a script
#
# pip writes a direct '#!<python>' shebang when the destination allows it and a '#!/bin/sh'
# trampoline that execs the interpreter on a following line when it does not, which is what a
# path containing a space produces. Reading the first line answered '/bin/sh' for the second
# shape, so nothing could be asked of the interpreter, the component classified 'unreadable',
# and install deleted the environment it had just built.
#
# The forms are BUILT here rather than asserted about, because the premise is a fact about the
# host's packaging tools and not about this repository.
# =========================================================================================

class InterpreterIdentityTests(unittest.TestCase):
    @staticmethod
    def _venv(root, name):
        environment = Path(root) / name / "env"
        environment.parent.mkdir(parents=True, exist_ok=True)
        done = subprocess.run([sys.executable, "-m", "venv", str(environment)],
                              capture_output=True, text=True, timeout=300)
        return environment, done

    def _record(self, environment, *, with_path):
        install = {"location": str(environment), "environment": str(environment),
                   "entryPoint": str(environment / "bin" / "pip")}
        if with_path:
            install["interpreterPath"] = str(environment / "bin" / "python")
        record = hostrecord.empty(1)
        hostrecord.put_install(record, "codex-session-relay", install)
        return record

    def test_both_console_script_shapes_resolve_to_a_runnable_interpreter(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            plain, made_plain = self._venv(temporary, "plain")
            spaced, made_spaced = self._venv(temporary, "with space")
            if made_plain.returncode != 0 or made_spaced.returncode != 0:
                self.skipTest("this host cannot create a virtual environment here")
            entries = {"plain": plain / "bin" / "pip", "spaced": spaced / "bin" / "pip"}
            for label, entry in entries.items():
                if not entry.exists():
                    self.skipTest("this host's venv module installs no console script")

            first = {label: runtime_install.interpreter_of(entry)
                     for label, entry in entries.items()}
            # The premise. If this host writes one shape for both paths there is nothing here to
            # be wrong about, and the case would otherwise pass without testing anything.
            if first["plain"] == first["spaced"]:
                self.skipTest("this host writes one console-script shape for both paths")
            self.assertTrue(interpreter_version_of(first["spaced"]) is None,
                            "the premise is that the first line of the second shape names"
                            " something that cannot answer as a Python interpreter")

            for recorded in (True, False):
                for label, entry in entries.items():
                    with self.subTest(shape=label, interpreterPath=recorded):
                        record = self._record(entries[label].parent.parent, with_path=recorded)
                        found, source = runtime_install.interpreter_for(
                            record, "codex-session-relay", entry)
                        self.assertIsNotNone(found, label)
                        self.assertIsNotNone(
                            runtime_install.interpreter_version(found),
                            label + ": the resolved interpreter did not answer its version")
                        self.assertNotEqual(source, "the script's first line")

    def test_a_script_outside_every_recorded_environment_still_uses_its_first_line(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            entry = Path(temporary) / "somebody-elses-script"
            entry.write_text("#!" + sys.executable + chr(10), encoding="utf-8")
            found, source = runtime_install.interpreter_for(
                hostrecord.empty(1), "codex-session-relay", entry)
        self.assertEqual(found, sys.executable)
        self.assertEqual(source, "the script's first line")

    def test_an_install_records_the_interpreter_it_installed_with(self):
        # The producer keeps the promise the consumer now relies on.
        source = RUNTIME.read_text(encoding="utf-8")
        self.assertIn('"interpreterPath": str(python)', source)
        tree = ast.parse(source)
        recorded = [node for node in ast.walk(tree)
                    if isinstance(node, ast.Dict)
                    and any(isinstance(k, ast.Constant) and k.value == "interpreterPath"
                            for k in node.keys)
                    and any(isinstance(k, ast.Constant) and k.value == "environment"
                            for k in node.keys)]
        self.assertTrue(recorded, "the install record must carry interpreterPath beside environment")


def interpreter_version_of(path):
    import runtime_install

    return runtime_install.interpreter_version(path) if path else None


class RecordedEnvironmentBindingTests(unittest.TestCase):
    """The record binds an entry point to ONE environment, or to none.

    --dest is arbitrary, so an environment can legally sit inside another environment's
    destination and an entry point under the inner one is contained by both. First match would
    then answer with the outer interpreter, which installed something else.
    """

    def _record(self, *installs):
        record = hostrecord.empty(1)
        for install in installs:
            hostrecord.put_install(record, "codex-session-relay", install)
        return record

    def test_the_recorded_entry_point_wins_over_any_containment(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            outer = Path(temporary) / "outer"
            inner = outer / "nested" / "env"
            inner.mkdir(parents=True)
            entry = inner / "bin" / "codex-session-relay"
            entry.parent.mkdir(parents=True)
            entry.write_text("#!/bin/sh" + chr(10), encoding="utf-8")
            record = self._record(
                {"location": str(outer), "environment": str(outer),
                 "entryPoint": str(outer / "bin" / "codex-session-relay"),
                 "interpreterPath": "/outer/python"},
                {"location": str(inner), "environment": str(inner),
                 "entryPoint": str(entry), "interpreterPath": "/inner/python"},
            )
            found, source = runtime_install.interpreter_for(
                record, "codex-session-relay", entry)
        self.assertEqual(found, "/inner/python")
        self.assertEqual(source, "recorded with the install")

    def test_a_nested_environment_binds_to_the_innermost_one(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            outer = Path(temporary) / "outer"
            inner = outer / "nested" / "env"
            inner.mkdir(parents=True)
            entry = inner / "bin" / "codex-session-relay"
            entry.parent.mkdir(parents=True)
            entry.write_text("#!/bin/sh" + chr(10), encoding="utf-8")
            # Neither record carries entryPoint: this is the compatibility path.
            record = self._record(
                {"location": str(outer), "environment": str(outer)},
                {"location": str(inner), "environment": str(inner)},
            )
            found, source = runtime_install.interpreter_for(
                record, "codex-session-relay", entry)
        self.assertEqual(found, str(inner / "bin" / "python"))
        self.assertEqual(source, "the recorded environment")

    def test_two_equally_specific_environments_answer_with_no_interpreter(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            one = Path(temporary) / "a" / "env"
            two = Path(temporary) / "b" / "env"
            for path in (one, two):
                (path / "bin").mkdir(parents=True)
            entry = one / "bin" / "codex-session-relay"
            entry.write_text("#!/bin/sh" + chr(10), encoding="utf-8")
            # Two installs recorded against the SAME environment naming different
            # interpreters. Containment cannot separate them and order is not evidence.
            record = self._record(
                {"location": str(one), "environment": str(one), "interpreterPath": "/one/python"},
                {"location": str(one) + "/.", "environment": str(one) + "/.",
                 "interpreterPath": "/two/python"},
            )
            found, source = runtime_install.interpreter_for(
                record, "codex-session-relay", entry)
        self.assertIsNone(found)
        self.assertIn("different interpreters", source)

    def test_an_ambiguous_record_stops_classification_rather_than_choosing(self):
        import runtime_install

        component = definition.load()["components"][0]
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            entry = environment / "bin" / component["consoleScript"]
            entry.parent.mkdir(parents=True)
            entry.write_text("#!/bin/sh" + chr(10), encoding="utf-8")
            entry.chmod(0o755)
            record = self._record(
                {"location": str(environment), "environment": str(environment),
                 "interpreterPath": "/one/python"},
                {"location": str(environment) + "/.", "environment": str(environment) + "/.",
                 "interpreterPath": "/two/python"},
            )
            record["components"][component["component"]] = record["components"].pop(
                "codex-session-relay")
            classified = runtime_install.classify_component(
                component, record=record, entry_override=str(entry), app_server="a-server")
        self.assertEqual(classified["class"], ownership.UNREADABLE)
        self.assertTrue(any("different interpreters" in reason for reason in classified["reasons"]),
                        classified["reasons"])


class SpacedDestinationTests(unittest.TestCase):
    """A destination containing a space reaches a runnable interpreter.

    The full install-promote-classify-own path for such a destination is proved by running the
    real command, which needs a network install of both packages and is too slow to belong
    here. What this covers is the mechanism that broke it: the interpreter for a spaced
    environment answered nothing, so the component could report no version and no module
    location and classified unreadable.
    """

    def test_a_spaced_environment_reports_its_interpreter_version(self):
        import runtime_install

        component = definition.load()["components"][0]
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "with space" / "env"
            environment.parent.mkdir(parents=True)
            made = subprocess.run([sys.executable, "-m", "venv", str(environment)],
                                  capture_output=True, text=True, timeout=300)
            if made.returncode != 0:
                self.skipTest("this host cannot create a virtual environment here")
            entry = environment / "bin" / component["consoleScript"]
            template = environment / "bin" / "pip"
            if not template.exists():
                self.skipTest("this host's venv module installs no console script")
            entry.write_text(template.read_text(encoding="utf-8"), encoding="utf-8")
            entry.chmod(0o755)

            # The premise: this destination really did produce the shape whose first line names
            # something that is not a Python interpreter.
            self.assertIsNone(
                runtime_install.interpreter_version(runtime_install.interpreter_of(entry)),
                "this host wrote a directly executable shebang, so there is nothing to fix here")

            record = hostrecord.empty(1)
            hostrecord.put_install(record, component["component"], {
                "location": str(environment), "environment": str(environment),
                "entryPoint": str(entry),
                "interpreterPath": str(environment / "bin" / "python")})
            classified = runtime_install.classify_component(
                component, record=record, entry_override=str(entry), app_server="a-server")
        self.assertIsNotNone(classified["interpreter"],
                             "the interpreter for a spaced environment answered nothing")
        self.assertEqual(classified["interpreterFrom"], "recorded with the install")


# =========================================================================================
# Check 13 - a judgment cell is filled only by the reading its own question produced
#
# The failure this closes is quiet. definition.git answers None when it cannot read, None
# compared with a recorded tree hash is False, and classification reads that False as a
# disagreement: an installation this command owns was reported as somebody's fork, from a
# reading nobody performed. The sibling three lines above, commit_matches, was already correct,
# which is what writing the comparison out at each site buys you.
#
# The cells are ownership.Signals' own parameters, and every one has to say which observation
# answers it and what happens when that observation does not. Four outcomes, not one: a reading
# that returns nothing must stop the classification, a reading that raises is a named refusal at
# the boundary, absence is sometimes a real "no", and some cells are answered by no observation
# this command makes.
# =========================================================================================

def _signal_cells():
    """The judgment cells, read from ownership.Signals rather than listed here."""
    tree = ast.parse((ROOT / "scripts" / "crw_runtime" / "ownership.py")
                     .read_text(encoding="utf-8"))
    for node in ast.walk(tree):
        if isinstance(node, ast.ClassDef) and node.name == "Signals":
            for inner in node.body:
                if isinstance(inner, ast.FunctionDef) and inner.name == "__init__":
                    return [a.arg for a in inner.args.kwonlyargs]
    return []


class OwnReadingTests(unittest.TestCase):
    def _fixture(self, temporary):
        """A component that classifies as something other than unreadable, so the cases mean
        something. The installed bytes are a stub, so the baseline is a fork."""
        component = definition.load()["components"][0]
        environment = Path(temporary) / "env"
        (environment / "bin").mkdir(parents=True)
        entry = environment / "bin" / component["consoleScript"]
        entry.write_text("#!" + sys.executable + chr(10), encoding="utf-8")
        entry.chmod(0o755)
        stub = Path(temporary) / "site" / component["module"]
        stub.mkdir(parents=True)
        (stub / "__init__.py").write_text("", encoding="utf-8")
        record = hostrecord.empty(1)
        hostrecord.put_install(record, component["component"], {
            "location": str(stub), "environment": str(environment),
            "entryPoint": str(entry), "interpreterPath": sys.executable})
        return component, entry, record, str(Path(temporary) / "site")

    def _classify(self, component, entry, record, site, patches=()):
        import runtime_install

        with contextlib.ExitStack() as entered:
            entered.enter_context(mock.patch.dict(
                os.environ, dict(os.environ, PYTHONPATH=site), clear=True))
            # The Codex CLI is a dimension of the combination, and a host without it on PATH
            # makes every classification unreadable before any case here has said anything.
            # Supplied as a baseline so the cases decide the outcome; the case that breaks this
            # reading enters its own patch afterwards and wins.
            entered.enter_context(mock.patch.object(
                runtime_install, "codex_cli_version", return_value="0.0.0-for-this-case"))
            for patch in patches:
                entered.enter_context(patch)
            return runtime_install.classify_component(
                component, record=record, entry_override=str(entry), app_server="a-server")

    def _broken(self, where, name, only_for=None):
        """Make one reading answer nothing, and only that one.

        definition.git reads the component tree, the repository commit and, through
        working_tree_clean, the status. Breaking the callable outright breaks all three, and a
        case that cannot isolate one cell cannot say which reading the classification rested
        on: the tree case would pass on the strength of the status being unread.
        """
        import runtime_install

        target = runtime_install if where == "runtime_install" else getattr(runtime_install, where)
        answer = (None, "no interpreter to ask", None) if name == "module_location" else None
        if only_for is None:
            return mock.patch.object(target, name, return_value=answer)
        real = getattr(target, name)

        def selective(*args, **kwargs):
            if args and any(only_for in str(part) for part in (args[0] or [])):
                return answer
            return real(*args, **kwargs)

        return mock.patch.object(target, name, side_effect=selective)

    def test_every_declared_cell_says_which_reading_answers_it(self):
        import runtime_install

        cells = _signal_cells()
        # Guards the reader: an empty list would make the comparison below vacuous.
        self.assertIn("tree_matches", cells)
        self.assertEqual(set(cells), set(runtime_install.SIGNAL_READINGS),
                         "a signal without an entry is a cell nobody checks")
        outcomes = {runtime_install.SIGNAL_UNREADABLE, runtime_install.SIGNAL_REFUSED,
                    runtime_install.SIGNAL_NEGATIVE, runtime_install.SIGNAL_NOT_A_READING}
        for cell, (outcome, observations) in runtime_install.SIGNAL_READINGS.items():
            with self.subTest(cell):
                self.assertIn(outcome, outcomes)
                if outcome in (runtime_install.SIGNAL_UNREADABLE, runtime_install.SIGNAL_REFUSED):
                    self.assertTrue(observations, cell + " names no observation")
                elif outcome == runtime_install.SIGNAL_NOT_A_READING:
                    self.assertEqual(observations, (), cell + " is not a reading of this command")
                for observation in observations:
                    self.assertEqual(len(observation), 3,
                                     cell + ": an observation is (module, attribute, only_for)")

    def test_the_baseline_is_not_unreadable_so_the_cases_mean_something(self):
        with tempfile.TemporaryDirectory() as temporary:
            classified = self._classify(*self._fixture(temporary))
        if classified["class"] == ownership.UNREADABLE:
            self.skipTest("this checkout cannot be read, so nothing here would be distinguishable")
        self.assertEqual(classified["class"], ownership.FORK, classified["reasons"])

    def test_a_reading_that_does_not_answer_stops_the_classification(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            fixture = self._fixture(temporary)
            if self._classify(*fixture)["class"] == ownership.UNREADABLE:
                self.skipTest("this checkout cannot be read")
            for cell, (outcome, observations) in sorted(runtime_install.SIGNAL_READINGS.items()):
                if outcome != runtime_install.SIGNAL_UNREADABLE:
                    continue
                for where, name, only_for in observations:
                    with self.subTest(cell=cell, reading=where + "." + name):
                        classified = self._classify(
                            *fixture, patches=(self._broken(where, name, only_for),))
                        self.assertEqual(
                            classified["class"], ownership.UNREADABLE,
                            name + " answered nothing and the classification still decided: "
                            + json.dumps(classified["reasons"]))

    def test_a_reading_that_raises_is_a_named_refusal_not_a_classification(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            fixture = self._fixture(temporary)
            for cell, (outcome, observations) in sorted(runtime_install.SIGNAL_READINGS.items()):
                if outcome != runtime_install.SIGNAL_REFUSED:
                    continue
                for where, name, _only_for in observations:
                    with self.subTest(cell=cell, reading=where + "." + name):
                        broken = mock.patch.object(
                            getattr(runtime_install, where), name,
                            side_effect=OSError("cannot read"))
                        with self.assertRaises(reading.Refused):
                            self._classify(*fixture, patches=(broken,))

    def test_a_repository_commit_nobody_read_is_not_a_commit_that_drifted(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            component, entry, record, site = self._fixture(temporary)
            record["components"][component["component"]]["repositoryCommit"] = "recorded-commit"
            classified = self._classify(
                component, entry, record, site,
                patches=(mock.patch.object(runtime_install.definition, "git",
                                           return_value=None),))
        self.assertIsNone(classified["repositoryCommitDrift"],
                          "a commit that could not be read is not a commit that differs")


# =========================================================================================
# Check 14 - no cell is filled by joining two answers to different questions
#
# scope.summarise read primary = selected or discovery. They are two doctor invocations against
# two different stores, and 'or' made a selected store that did not answer borrow the discovered
# store's path, store id and socketConnect, while every other command kept acting on the
# selected one. The scan below looks for that exact shape: two locals bound from the same reader
# with different arguments, then joined.
# =========================================================================================

def _survey_reading_names():
    """The names survey gives its readings, read from scope.survey rather than listed here."""
    tree = ast.parse((ROOT / "scripts" / "crw_runtime" / "scope.py").read_text(encoding="utf-8"))
    found = set()
    for node in ast.walk(tree):
        if not (isinstance(node, ast.FunctionDef) and node.name == "survey"):
            continue
        for inner in ast.walk(node):
            # The readings map itself, not every table inside it: a reading's own payload
            # carries keys like ok and skipped, which name no question.
            if (isinstance(inner, ast.Assign) and len(inner.targets) == 1
                    and getattr(inner.targets[0], "id", None) == "readings"
                    and isinstance(inner.value, ast.Dict)):
                found |= {k.value for k in inner.value.keys
                          if isinstance(k, ast.Constant) and isinstance(k.value, str)}
            if isinstance(inner, ast.Subscript) and isinstance(inner.slice, ast.Constant) \
                    and getattr(inner.value, "id", None) == "readings":
                found.add(inner.slice.value)
    return {name for name in found if not name.endswith("Via")}


def _borrowed_answers(tree, names):
    """Every place two answers to DIFFERENT reading questions are joined into one value.

    The question is named by the key: readings["selected"] and readings["discovery"] are two
    doctor invocations against two different stores. Joining them with or, or with a conditional,
    lets one borrow the other's answer. Two field names of ONE payload are not this shape and
    are left alone, which is why the names come from survey rather than from a guess about
    receivers.
    """
    offenders = []
    for function in ast.walk(tree):
        if not isinstance(function, ast.FunctionDef):
            continue
        bound = {}
        for node in ast.walk(function):
            if (isinstance(node, ast.Assign) and len(node.targets) == 1
                    and isinstance(node.targets[0], ast.Name)
                    and isinstance(node.value, ast.Call) and node.value.args
                    and isinstance(node.value.args[0], ast.Constant)
                    and node.value.args[0].value in names):
                bound[node.targets[0].id] = node.value.args[0].value
        # A join is allowed when the code also records which question answered, the way
        # interpreter_for carries its source. Named here so the exemption is visible.
        carries = any(
            isinstance(node, ast.Name) and (
                node.id.endswith("answered_by") or node.id.endswith("_from")
                or node.id == "answering")
            for node in ast.walk(function))
        for node in ast.walk(function):
            if isinstance(node, ast.BoolOp) and isinstance(node.op, ast.Or):
                operands = node.values
            elif isinstance(node, ast.IfExp):
                operands = [node.body, node.orelse]
            else:
                continue
            asked = set()
            for operand in operands:
                for inner in ast.walk(operand):
                    if isinstance(inner, ast.Constant) and inner.value in names:
                        asked.add(inner.value)
                    elif isinstance(inner, ast.Name) and inner.id in bound:
                        asked.add(bound[inner.id])
            if len(asked) > 1 and not carries:
                offenders.append(function.name + ":" + str(node.lineno) + " joins "
                                 + ", ".join(sorted(asked)))
    return sorted(set(offenders))


class BorrowedAnswerTests(unittest.TestCase):
    def test_the_reading_names_come_from_survey(self):
        names = _survey_reading_names()
        self.assertIn("discovery", names)
        self.assertIn("selected", names)

    def test_no_module_joins_two_answers_to_different_questions(self):
        names = _survey_reading_names()
        for path in RUNTIME_MODULES:
            with self.subTest(path.name):
                tree = ast.parse(path.read_text(encoding="utf-8"))
                self.assertEqual(_borrowed_answers(tree, names), [])

    def test_the_scan_sees_both_spellings_of_the_shape(self):
        names = _survey_reading_names()
        bound = ("def summarise(readings):\n"
                 "    discovery = usable('discovery')\n"
                 "    selected = usable('selected')\n"
                 "    primary = selected or discovery\n"
                 "    return primary\n")
        inline = ("def cmd_diagnose(readings):\n"
                  "    return (readings.get('selected') or readings.get('discovery') or {})\n")
        conditional = bound.replace("selected or discovery",
                                    "selected if selected else discovery")
        for label, source in (("through locals", bound), ("inline", inline),
                              ("conditional", conditional)):
            with self.subTest(label):
                self.assertTrue(_borrowed_answers(ast.parse(source), names),
                                label + " must be seen")

    def test_two_field_names_of_one_payload_are_not_this_shape(self):
        names = _survey_reading_names()
        source = ("def summarise(store):\n"
                  "    return store.get('dbPath') or store.get('realPath')\n")
        self.assertEqual(_borrowed_answers(ast.parse(source), names), [])

    def test_a_join_that_says_which_question_answered_is_allowed(self):
        names = _survey_reading_names()
        source = ("def summarise(readings):\n"
                  "    discovery = usable('discovery')\n"
                  "    selected = usable('selected')\n"
                  "    answered_by = 'selected' if selected else 'discovery'\n"
                  "    primary = selected or discovery\n"
                  "    return primary, answered_by\n")
        self.assertEqual(_borrowed_answers(ast.parse(source), names), [])


class ClaimedComparisonTests(unittest.TestCase):
    """A field may not report a comparison this run did not make.

    Without --bridge-command nothing is compared, deliberately: comparing an existing correct
    registration against an empty string reports CONFLICT for a host registered exactly right.
    What was wrong was the result built on top of that. PRESENT and LINKED were one set, so
    mcpExposed reached verified and its evidence read "the configuration registers this exact
    command" for a host registering something else entirely.
    """

    def _diagnose(self, command, *extra):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary) / "codex"
            home.mkdir()
            (home / "config.toml").write_text(
                "[mcp_servers.codex-thread-bridge]" + chr(10)
                + 'command = "' + command + '"' + chr(10), encoding="utf-8")
            done = run("diagnose", "--codex-home", str(home),
                       "--record", str(Path(temporary) / "record.json"), "--temporary", *extra)
            return json.loads(done.stdout)

    @needs_reader
    def test_an_uncompared_registration_never_reports_the_tools_as_exposed(self):
        payload = self._diagnose("/somewhere/else/impostor",
                                 "--observed-tool", _BRIDGE_TOOL,
                                 "--observed-tool", "create_thread")
        exposed = payload["checks"]["results"]["mcpExposed"]
        self.assertEqual(exposed["value"], "not_verified")
        self.assertNotIn("registers this exact command", exposed["evidence"],
                         "no command was supplied, so nothing compared anything")
        self.assertIn("no expected command was supplied", exposed["evidence"])
        self.assertEqual(payload["mcpRegistration"]["outcome"], reading.PRESENT)

    @needs_reader
    def test_a_compared_registration_still_reports_exposure(self):
        payload = self._diagnose("/opt/bridge/bin/codex-thread-bridge",
                                 "--bridge-command", "/opt/bridge/bin/codex-thread-bridge",
                                 "--observed-tool", _BRIDGE_TOOL)
        exposed = payload["checks"]["results"]["mcpExposed"]
        self.assertEqual(payload["mcpRegistration"]["outcome"], codexconfig.LINKED)
        self.assertEqual(exposed["value"], "verified", exposed["evidence"])


class RecordShapeTests(unittest.TestCase):
    """What the boundary accepts and what the helpers require are one record.

    shape validated the containers it was given and said nothing about the ones it was not, so a
    record it accepted still raised through the top of put_install as an internal error rather
    than as the named refusal this command documents.
    """

    def test_a_container_that_is_there_and_is_not_a_table_is_refused(self):
        for label, record in (
            ("components is null", {"recordVersion": 1, "components": None}),
            ("installs is null",
             {"recordVersion": 1, "components": {"codex-session-relay": {"installs": None}}}),
            ("measuredPoints is null",
             {"recordVersion": 1,
              "components": {"codex-session-relay": {"measuredPoints": None}}}),
        ):
            with self.subTest(label):
                with self.assertRaises(TypeError):
                    hostrecord.shape(record)

    def test_an_absent_container_keeps_its_defined_meaning(self):
        record = hostrecord.shape({"recordVersion": 1})
        self.assertEqual(hostrecord.component(record, "codex-session-relay"),
                         {"installs": [], "measuredPoints": []})

    def test_every_record_shape_accepts_can_be_used_by_the_helpers(self):
        for label, record in (
            ("no components", {"recordVersion": 1}),
            ("an entry with neither list",
             {"recordVersion": 1, "components": {"codex-session-relay": {}}}),
            ("an entry with one list",
             {"recordVersion": 1,
              "components": {"codex-session-relay": {"installs": []}}}),
        ):
            with self.subTest(label):
                accepted = hostrecord.shape(record)
                hostrecord.put_install(accepted, "codex-session-relay", {"location": "/l"})
                hostrecord.add_point(accepted, "codex-session-relay", {"exercised": True})
                self.assertEqual(
                    len(accepted["components"]["codex-session-relay"]["installs"]), 1)
                self.assertEqual(
                    len(accepted["components"]["codex-session-relay"]["measuredPoints"]), 1)


# =========================================================================================
# Check 15 - "no reading answers this cell" is a claim, and the claim is checked
#
# Check 13 verifies that every cell NAMES its reading. It cannot see whether the naming is
# true, so a cell declared to have no reading is simply skipped -- and that is the path this
# defect took. link_conflict sat empty while skill_links() was answering the very question,
# because the declaration said "the skill installer's reading, not this command's" and nothing
# tested that sentence.
#
# The subject of a cell is recovered from its own name by stripping the suffixes the
# declaration lists, so the correspondence is mechanical rather than a table somebody keeps in
# step by hand.
# =========================================================================================

def _command_producers():
    """Every function this command defines, read from its source."""
    tree = ast.parse(RUNTIME.read_text(encoding="utf-8"))
    return {node.name for node in tree.body if isinstance(node, ast.FunctionDef)}


def _cell_subject(cell, prefixes, suffixes):
    name = cell
    for prefix in prefixes:
        if name.startswith(prefix):
            name = name[len(prefix):]
    for suffix in suffixes:
        if name.endswith(suffix):
            name = name[: -len(suffix)]
    return (name.rstrip("s") or cell)


def _unclaimed_readings(cells, producers, prefixes, suffixes):
    """Cells that say no reading answers them while a producer names their subject."""
    offenders = []
    for cell, (_outcome, observations) in sorted(cells.items()):
        if observations:
            continue
        subject = _cell_subject(cell, prefixes, suffixes)
        matching = sorted(name for name in producers if subject in name)
        if matching:
            offenders.append(cell + " names no reading, but this command defines "
                             + ", ".join(matching))
    return offenders


class UnclaimedReadingTests(unittest.TestCase):
    def test_the_producer_inventory_is_not_empty(self):
        producers = _command_producers()
        # Guards the reader: an empty set would make every claim below pass vacuously.
        self.assertIn("skill_links", producers)
        self.assertIn("registration_state", producers)

    def test_no_cell_claims_to_have_no_reading_while_one_answers_it(self):
        import runtime_install

        self.assertEqual(
            _unclaimed_readings(runtime_install.SIGNAL_READINGS, _command_producers(),
                                runtime_install.SIGNAL_SUBJECT_PREFIXES,
                                runtime_install.SIGNAL_SUBJECT_SUFFIXES),
            [])

    def test_the_subject_of_a_cell_comes_off_its_own_name(self):
        import runtime_install

        subject = lambda cell: _cell_subject(cell, runtime_install.SIGNAL_SUBJECT_PREFIXES,
                                             runtime_install.SIGNAL_SUBJECT_SUFFIXES)
        self.assertEqual(subject("link_conflict"), "link")
        self.assertEqual(subject("has_point"), "point")
        self.assertEqual(subject("tree_matches"), "tree")
        self.assertEqual(subject("commit_matches"), "commit")

    def test_the_scan_sees_a_false_claim(self):
        import runtime_install

        cells = dict(runtime_install.SIGNAL_READINGS)
        cells["link_conflict"] = (runtime_install.SIGNAL_NOT_A_READING, ())
        offenders = _unclaimed_readings(cells, _command_producers(),
                                        runtime_install.SIGNAL_SUBJECT_PREFIXES,
                                        runtime_install.SIGNAL_SUBJECT_SUFFIXES)
        self.assertTrue(any("link_conflict" in o and "skill_links" in o for o in offenders),
                        offenders)


class LinkConflictTests(unittest.TestCase):
    """A foreign skill path reaches the conflict signal, and a caller that makes no such
    reading says so rather than reporting no conflict."""

    def _classified(self, links):
        import runtime_install

        component = definition.load()["components"][0]
        # Every other signal is supplied or absent by construction, so the link reading is what
        # decides. Without this the case passed only on a host with the Codex CLI on PATH and
        # failed in CI, which is the same "answered by the host rather than by the case" the
        # checks here exist to stop.
        with mock.patch.object(runtime_install, "codex_cli_version",
                               return_value="0.0.0-for-this-case"):
            return runtime_install.classify_component(
                component, record=hostrecord.empty(1), app_server="a-server", links=links)

    def test_a_foreign_skill_path_makes_the_component_a_conflict(self):
        classified = self._classified({"conflict": ["/home/someone/.codex/skills/crw-run"],
                                       "linked": [], "missing": [], "legacy": []})
        self.assertEqual(classified["class"], ownership.CONFLICT, classified["reasons"])
        self.assertTrue(any("foreign skill path" in reason for reason in classified["reasons"]),
                        classified["reasons"])

    def test_a_skill_layer_that_could_not_be_read_stops_the_classification(self):
        classified = self._classified({"unreadable": "TimeoutError: expired"})
        self.assertEqual(classified["class"], ownership.UNREADABLE, classified["reasons"])
        self.assertTrue(any("skill-link layer" in reason for reason in classified["reasons"]),
                        classified["reasons"])

    def test_a_caller_that_made_no_reading_says_so(self):
        classified = self._classified(None)
        self.assertIsNone(classified["linkConflict"])
        self.assertFalse(classified["conflictsRead"]["links"],
                         "no conflict found and nobody looked are different answers")

    def test_diagnosis_reads_the_skill_layer_before_it_classifies(self):
        """Order is the defect: the reading was made, reported, and thrown away."""
        source = RUNTIME.read_text(encoding="utf-8")
        tree = ast.parse(source)
        diagnose = next(node for node in ast.walk(tree)
                        if isinstance(node, ast.FunctionDef) and node.name == "cmd_diagnose")
        reads = [node.lineno for node in ast.walk(diagnose)
                 if isinstance(node, ast.Call) and getattr(node.func, "id", None) == "skill_links"]
        classifies = [node.lineno for node in ast.walk(diagnose)
                      if isinstance(node, ast.Call)
                      and getattr(node.func, "id", None) == "classify_component"]
        self.assertTrue(reads and classifies)
        self.assertLess(min(reads), min(classifies),
                        "the skill layer is read after the classes are decided")


class BusyLockTests(unittest.TestCase):
    """A lock this run could not take established nothing, and that is an answer.

    Letting the TimeoutError out made the cleanup path of an already-failing install raise, so
    the run reported an internal error instead of whether its destination is retriable.
    """

    def test_a_busy_lock_keeps_the_candidate_and_says_why(self):
        with tempfile.TemporaryDirectory() as temporary:
            record = Path(temporary) / "host-record.json"
            hostrecord.save(record, hostrecord.empty(1))
            held = Path(str(record) + hostrecord.LOCK_SUFFIX)
            held.write_text("999999", encoding="utf-8")
            try:
                with mock.patch.object(hostrecord, "STALE_LOCK_SECONDS", 10 ** 6), \
                     mock.patch.object(hostrecord, "LOCK_TIMEOUT_SECONDS", 0.05):
                    answer, decision = hostrecord.release_candidate(
                        record, 1, Path(temporary) / "env")
            finally:
                held.unlink()
        self.assertIn("kept", decision)
        self.assertIn("lock", decision)
        self.assertFalse(answer.usable)
        self.assertEqual(answer.state, reading.ACCESS_ERROR)

    def test_a_failing_install_reports_retriability_rather_than_an_internal_error(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            record = Path(temporary) / "host-record.json"
            hostrecord.save(record, hostrecord.empty(1))
            environment = Path(temporary) / "env"
            environment.mkdir()
            held = Path(str(record) + hostrecord.LOCK_SUFFIX)
            held.write_text("999999", encoding="utf-8")
            printed = []
            try:
                with mock.patch.object(hostrecord, "STALE_LOCK_SECONDS", 10 ** 6), \
                     mock.patch.object(hostrecord, "LOCK_TIMEOUT_SECONDS", 0.05), \
                     mock.patch.object(runtime_install, "emit", side_effect=printed.append):
                    code = runtime_install._install_failed(
                        record, 1, [], environment, owned=environment)
            finally:
                held.unlink()
        self.assertEqual(code, runtime_install.EXIT_REFUSED)
        payload = printed[-1]
        self.assertIsNone(payload["internalError"])
        self.assertIn("lock", payload["candidate"])
        self.assertFalse(payload["retriable"])
        self.assertTrue(payload["recoveryRequires"])


# =========================================================================================
# Check 16 - the inventory for a conflict cell is the CALLER set
#
# Every cell was declared, every declaration was verified, and install still promoted over a
# conflict, because the checks looked at cells and this defect lives one dimension up: the
# command that moves the selection passed neither conflict reading. A cell may legitimately be
# None for a caller -- the MCP registration is the bridge's and says nothing about the relay --
# but the caller says so by passing the keyword, and the classification reports which readings
# were made.
#
# Only the production module is scanned. The cases in this file construct classify_component
# calls with deliberately isolated signals, and scanning them would forbid the isolation the
# other checks are built on.
# =========================================================================================

def _classify_callers(tree):
    """Every classify_component call in the command, as (function, line, keywords)."""
    callers = []
    for function in ast.walk(tree):
        if not isinstance(function, ast.FunctionDef):
            continue
        for node in ast.walk(function):
            if isinstance(node, ast.Call) and getattr(node.func, "id", None) == "classify_component":
                callers.append((function.name, node.lineno,
                                {k.arg for k in node.keywords if k.arg}))
    return callers


class ConflictCallerTests(unittest.TestCase):
    def test_the_caller_inventory_is_not_empty(self):
        callers = _classify_callers(ast.parse(RUNTIME.read_text(encoding="utf-8")))
        # Guards the reader: no callers would make the assertion below pass vacuously.
        self.assertGreaterEqual(len(callers), 2, callers)
        self.assertEqual({name for name, _line, _kw in callers}, {"cmd_diagnose", "cmd_install"})

    def test_every_caller_supplies_every_conflict_reading(self):
        import runtime_install

        missing = []
        for name, line, keywords in _classify_callers(
                ast.parse(RUNTIME.read_text(encoding="utf-8"))):
            absent = sorted(set(runtime_install.CONFLICT_READINGS) - keywords)
            if absent:
                missing.append(name + ":" + str(line) + " omits " + ", ".join(absent))
        self.assertEqual(missing, [])

    def test_the_scan_sees_a_caller_that_omits_one(self):
        import runtime_install

        source = ("def cmd_install(args):\n"
                  "    return classify_component(c, record=r, links=l)\n")
        callers = _classify_callers(ast.parse(source))
        absent = set(runtime_install.CONFLICT_READINGS) - callers[0][2]
        self.assertEqual(sorted(absent), ["pointer", "registration"])

    def test_the_classification_reports_which_readings_were_made(self):
        import runtime_install

        component = definition.load()["components"][0]
        with mock.patch.object(runtime_install, "codex_cli_version",
                               return_value="0.0.0-for-this-case"):
            both = runtime_install.classify_component(
                component, record=hostrecord.empty(1), app_server="a-server",
                registration={"outcome": reading.PRESENT, "detail": "read", "registered": {}},
                links={"conflict": [], "linked": [], "missing": [], "legacy": []},
                pointer={"state": pointer.NO_POINTER, "target": None, "agrees": None,
                         "detail": "no pointer is placed here"})
            neither = runtime_install.classify_component(
                component, record=hostrecord.empty(1), app_server="a-server")
        self.assertEqual(both["conflictsRead"],
                         {name: True for name in runtime_install.CONFLICT_READINGS})
        self.assertEqual(neither["conflictsRead"],
                         {name: False for name in runtime_install.CONFLICT_READINGS})

    def test_install_can_be_pointed_at_a_codex_home(self):
        import runtime_install

        parsed = runtime_install.build_parser().parse_args(
            ["install", "--dest", "/tmp/x", "--codex-home", "/tmp/home"])
        self.assertEqual(parsed.codex_home, "/tmp/home")

    def test_the_promoting_caller_compares_the_command_and_not_the_arguments(self):
        """The mechanism is only half of it; the caller has to ask for it.

        install knows which entry point it installed and nothing about the arguments a host
        chose, so it compares the command alone. Read from the call rather than from the
        behaviour because reaching this through a whole installation would cost minutes.
        """
        tree = ast.parse(RUNTIME.read_text(encoding="utf-8"))
        install = next(node for node in ast.walk(tree)
                       if isinstance(node, ast.FunctionDef) and node.name == "cmd_install")
        calls = [node for node in ast.walk(install)
                 if isinstance(node, ast.Call)
                 and getattr(node.func, "id", None) == "registration_state"]
        self.assertTrue(calls, "install makes no registration reading")
        for call in calls:
            asked = {k.arg: k.value for k in call.keywords if k.arg}
            self.assertIn("compare_args", asked,
                          "install must say it has no expectation about the arguments")
            self.assertIs(asked["compare_args"].value, False)

    @needs_reader
    def test_a_registration_with_arguments_is_not_a_conflict_for_a_caller_expecting_none(self):
        """install knows which entry point it installed and nothing about the arguments a host
        chose. An empty list is an expectation, not the absence of one."""
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary) / "codex"
            home.mkdir()
            (home / "config.toml").write_text(
                "[mcp_servers." + runtime_install.MCP_NAME + "]" + chr(10)
                + 'command = "/opt/env/bin/codex-thread-bridge"' + chr(10)
                + 'args = ["--socket", "/tmp/s.sock"]' + chr(10), encoding="utf-8")
            same = runtime_install.registration_state(
                home, "/opt/env/bin/codex-thread-bridge", [], compare_args=False)
            other = runtime_install.registration_state(
                home, "/somewhere/else/bridge", [], compare_args=False)
            strict = runtime_install.registration_state(
                home, "/opt/env/bin/codex-thread-bridge", [])
        self.assertEqual(same["outcome"], codexconfig.LINKED, same["detail"])
        self.assertFalse(same["comparedArguments"])
        self.assertEqual(other["outcome"], codexconfig.CONFLICT)
        self.assertEqual(strict["outcome"], codexconfig.CONFLICT,
                         "the strict comparison is unchanged and still expects the arguments")


# =========================================================================================
# Check 17 - the inventory for a store on disk is the PLACE set
#
# This listing is the whole inventory when the relay cannot answer, which is exactly when
# hiding a store matters. The state root had its own branch for relay.sqlite3 and everything
# else was looked for in child directories, so an operations ledger beside the root database
# was in neither and was never listed. A third branch would reopen at the next place, so the
# places are a rule and every pattern is looked for in every one of them.
# =========================================================================================

class StorePlaceTests(unittest.TestCase):
    def _root(self, temporary):
        root = Path(temporary) / "state" / "codex-session-relay"
        (root / "scope-a").mkdir(parents=True)
        return root

    def test_every_pattern_is_found_in_every_place(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = self._root(temporary)
            expected = set()
            for place in (root, root / "scope-a"):
                for pattern, _kind in scope.STORE_PATTERNS:
                    name = pattern.replace("*", "0123456789abcdef")
                    (place / name).write_bytes(b"")
                    expected.add(str(place / name))
            found = scope.filesystem_candidates({"XDG_STATE_HOME": str(Path(temporary) / "state")})
        self.assertEqual({entry["database"] for entry in found}, expected)

    def test_the_places_are_a_rule_and_the_root_comes_first(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = self._root(temporary)
            places = scope.store_places(root)
        self.assertEqual(places[0], root)
        self.assertIn(root / "scope-a", places)

    def test_an_unreadable_listing_loses_the_scopes_and_not_the_root(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = self._root(temporary)
            (root / "relay.sqlite3").write_bytes(b"")
            with mock.patch.object(Path, "iterdir", side_effect=OSError("denied")):
                places = scope.store_places(root)
        self.assertEqual(places, [root])

    def test_the_patterns_are_the_ones_the_packages_write(self):
        names = {pattern for pattern, _kind in scope.STORE_PATTERNS}
        self.assertIn("relay.sqlite3", names)
        self.assertIn("operations-*.sqlite3", names)


# =========================================================================================
# Check 18 - the members of a declared set are PAIRS
#
# Four findings in one round, two shapes, one layer. A declared set fixed its MEMBERS and left
# what is attached to each member unfixed, so the gate over the set applied whatever predicate
# it happened to write and the probe over the set asked whatever runtime was nearest.
#
#   a trial input, paired with the predicate its CONSUMER applies
#   a preflight question, paired with the RUNTIME that will act on the answer
#   a presence question, paired with the READER whose sentinel decides it
#   a recorded claim, paired with every ARTIFACT the claim rests on
#
# Layer 4 made a producer keep a consumer's predicate. Layer 6 made a consumer reference a
# declared set rather than a literal. Neither of them says that a member of that set carries
# its own predicate and its own provenance, which is what these check.
# =========================================================================================


def _function_named(path, name):
    tree = ast.parse(Path(path).read_text(encoding="utf-8"))
    for node in ast.walk(tree):
        if isinstance(node, ast.FunctionDef) and node.name == name:
            return node
    return None


def _relay_defines(predicate):
    """Whether the relay's own source defines the callable a predicate names.

    Read from the relay rather than imported, because this runs on interpreters that have no
    relay installed and the question is about the declaration, not about this host.
    """
    module, name, rest = predicate[0], predicate[1], tuple(predicate[2:])
    path = RELAY_SRC / (module.rsplit(".", 1)[-1] + ".py")
    if not path.exists():
        return False
    tree = ast.parse(path.read_text(encoding="utf-8"))
    owner = next((node for node in tree.body
                  if isinstance(node, (ast.FunctionDef, ast.ClassDef)) and node.name == name),
                 None)
    if owner is None:
        return False
    if not rest:
        return True
    return any(isinstance(node, ast.FunctionDef) and node.name == rest[0]
               for node in owner.body)


class PairedMemberTests(unittest.TestCase):
    def test_every_trial_input_carries_the_predicate_its_consumer_applies(self):
        """A member is (flag, predicate). Carrying only the flag is what left the gate free to
        invent truthiness for an input whose consumer refuses a blank string."""
        import runtime_install

        for name, pair in runtime_install.TRIAL_PREFLIGHT_INPUTS.items():
            with self.subTest(name):
                self.assertIsInstance(pair, tuple, name + " must carry (flag, predicate)")
                self.assertGreaterEqual(len(pair), 2, name)
                flag, predicate = pair[0], pair[1]
                self.assertTrue(str(flag).startswith("--"), name + " must name its flag")
                if predicate == runtime_install.NON_BLANK:
                    continue
                self.assertIsInstance(predicate, tuple, name + " names a consumer's callable")
                self.assertTrue(
                    _relay_defines(predicate),
                    name + " is governed by " + str(predicate) + ", which the relay's own"
                    " source does not define: the pair names a predicate nobody applies")

    def test_the_gate_applies_each_declared_predicate_rather_than_one_of_its_own(self):
        """The declared predicates have to reach the trial. Read from the gate's source, so a
        pair declared and then ignored fails here instead of at the relay."""
        import runtime_install

        gate = ast.unparse(_function_named(RUNTIME, "_trial"))
        self.assertIn("TRIAL_REQUIRED_INPUTS", gate)
        self.assertIn("values_usable", gate,
                      "a declared consumer predicate is asked of the consumer, not restated")
        self.assertIn("_supplied", gate,
                      "every member carries this command's own minimum for a supplied flag")

    def test_every_preflight_probe_runs_the_runtime_it_was_handed(self):
        """The other half of the pair: a question and the runtime that will act on the answer.

        Asked of this checkout, a selected installation whose rule differs accepts here and
        refuses after four mutating steps, which is the failure the pair exists to stop.
        """
        import runtime_install

        for name in runtime_install.PREFLIGHT_PROBES:
            with self.subTest(name):
                node = _function_named(RUNTIME, name)
                self.assertIsNotNone(node, name + " is declared a preflight probe and is gone")
                body = ast.unparse(node)
                self.assertIn("interpreter", [argument.arg for argument in node.args.args],
                              name + " must be handed the runtime it asks")
                self.assertNotIn(
                    "sys.executable", body,
                    name + " runs this controller, so it asks this checkout's copy of a rule"
                    " the selected installation owns")
                self.assertNotIn(
                    "sys.path.insert", body,
                    name + " puts this checkout on the probe's path, which is the same thing"
                    " by another route")
                self.assertIn("str(interpreter)", body,
                              name + " must build its argv from the runtime it was handed")

    def test_a_blank_value_is_refused_by_the_consumers_own_predicate(self):
        """The defect itself: whitespace is truthy here and blank in the relay."""
        import runtime_install

        answer = runtime_install.values_usable(
            runtime_install.RELAY_TURN_ID, {"--turn-id": "   "}, RELAY_RUNTIME)
        self.assertFalse(answer["usable"], answer["detail"])
        self.assertIn("--turn-id", answer["detail"])

        accepted = runtime_install.values_usable(
            runtime_install.RELAY_TURN_ID, {"--turn-id": "t-1"}, RELAY_RUNTIME)
        self.assertTrue(accepted["usable"], accepted["detail"])

    def test_a_question_that_could_not_be_asked_is_a_refusal_and_not_a_fallback(self):
        import runtime_install

        answer = runtime_install.values_usable(runtime_install.RELAY_TURN_ID, {"--turn-id": "t"},
                                               None)
        self.assertIsNone(answer["usable"])
        self.assertIn("interpreter", answer["detail"])

        refusals, reason = runtime_install._relay_normalizes(["/tmp/x"], None)
        self.assertEqual(refusals, {})
        self.assertIn("interpreter", reason)

    @needs_reader
    def test_an_empty_server_table_is_present_and_not_absent(self):
        """Empty is not absent. Deciding it by testing the entry for truth denied a
        registration that is on disk, which is the reading half of the same shape."""
        import runtime_install

        name = runtime_install.MCP_NAME
        view = codexconfig.scan("[mcp_servers." + name + "]\n")
        self.assertEqual(codexconfig.registration_of(view, name), (True, {}))
        self.assertEqual(codexconfig.registration_of(view, "not-registered"),
                         (False, codexconfig.ABSENT))

        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary) / "codex"
            home.mkdir()
            (home / "config.toml").write_text("[mcp_servers." + name + "]\n",
                                              encoding="utf-8")
            state = runtime_install.registration_state(home, "", [])
        self.assertEqual(state["outcome"], "PRESENT",
                         "a table that exists on disk is not an absent registration")

    def test_every_presence_reading_is_asked_through_its_own_reader(self):
        import runtime_install

        source = RUNTIME.read_text(encoding="utf-8")
        for question, (module, callable_name) in runtime_install.PRESENCE_READINGS.items():
            with self.subTest(question):
                self.assertIn(module + "." + callable_name + "(", source,
                              question + " is declared to be answered by " + callable_name
                              + ", and this module decides it some other way")

    def test_a_claim_names_the_instrument_it_rests_on(self):
        """The bytes that produced 'exercised' are a dimension, because they decide the claim."""
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "check.py").write_text("print('one')", encoding="utf-8")
            with mock.patch.object(runtime_install, "ROOT", root):
                first = runtime_install.instrument_digest({"exerciseScript": "check.py"})
                (root / "check.py").write_text("print('two')", encoding="utf-8")
                second = runtime_install.instrument_digest({"exerciseScript": "check.py"})
                (root / "check.py").unlink()
                gone = runtime_install.instrument_digest({"exerciseScript": "check.py"})
                none = runtime_install.instrument_digest({"component": "no-instrument"})
        self.assertNotEqual(first, second, "a changed instrument is a changed claim")
        self.assertIsNone(gone, "an instrument that could not be read is not a value")
        self.assertEqual(none, runtime_install.NO_CHECKOUT_INSTRUMENT,
                         "no instrument is an answer, not a missing dimension")

    def test_the_writer_and_the_reader_ask_one_helper(self):
        """Two derivations drift. The value a point carries has to be the value a later run
        asks with, so both sides call the same function."""
        for name in ("measure_candidate", "classify_component"):
            with self.subTest(name):
                self.assertIn("instrument_digest", ast.unparse(_function_named(RUNTIME, name)),
                              name + " must ask the shared helper")

    def test_the_bridges_instrument_sits_outside_its_installed_package(self):
        """The premise of the dimension. If the smoke check ever moves inside the package its
        bytes are already covered by installDigest, and this says so rather than leaving the
        dimension standing on a reason nobody rechecked."""
        bridge = next(component for component in definition.load()["components"]
                      if component["component"] == "codex-thread-bridge")
        self.assertNotIn(bridge["packageLocation"], bridge["exerciseScript"],
                         "the instrument is inside the installed package now")

    def test_a_point_measured_with_another_instrument_does_not_authorize_reuse(self):
        record = hostrecord.empty(1)
        hostrecord.add_point(record, "codex-thread-bridge", {
            "exercised": True, "install": "/env/pkg", "interpreter": "3.13.1",
            "installDigest": "abc", "codexCli": "0.1.0", "host": "a-host",
            "appServer": "a-server", "exerciseDigest": "the-check-as-it-was",
        })
        asked = dict(location="/env/pkg", interpreter="3.13.1", install_digest="abc",
                     codex_cli="0.1.0", host="a-host", app_server="a-server")
        self.assertEqual(
            len(hostrecord.points_for(record, "codex-thread-bridge",
                                      exercise_digest="the-check-as-it-was", **asked)), 1)
        self.assertEqual(
            hostrecord.points_for(record, "codex-thread-bridge",
                                  exercise_digest="the-check-restored", **asked), [],
            "a point made by a modified smoke check cannot be read back once it is restored")



# =========================================================================================
# CRW-49 - an update that fails keeps the previous installation and the store
# =========================================================================================


def _claim_written(environment, state):
    """A claim as a run would have left it, without holding its lock."""
    Path(environment).mkdir(parents=True, exist_ok=True)
    staging.write_claim(environment, state, issue="CRW-49", run="1")


class StagingClaimTests(unittest.TestCase):
    """Who owns a directory this command may be about to remove.

    The environment name is deterministic, so getting this wrong is not a near miss: reading a
    live run's directory as abandoned deletes a runtime somebody is building, and reading an
    abandoned one as live refuses that destination for ever.
    """

    def test_a_directory_abandoned_by_a_dead_run_is_reclaimed(self):
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            _claim_written(environment, staging.STAGING)
            (environment / "half-built").write_text("x", encoding="utf-8")
            liveness, detail = staging.owner_liveness(environment)
            self.assertEqual(liveness, staging.DEAD, detail)
            decision, why = staging.decide(
                staging.read_claim(environment), liveness,
                occupied=staging.directory_occupied(environment)[0], protected=False, selected=False)
        self.assertEqual(decision, staging.RECLAIM, why)

    def test_a_directory_a_live_run_still_holds_is_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            _claim_written(environment, staging.STAGING)
            held = staging.Held(environment).take()
            try:
                liveness, detail = staging.owner_liveness(environment)
                self.assertEqual(liveness, staging.LIVE, detail)
                decision, why = staging.decide(
                    staging.read_claim(environment), liveness,
                    occupied=True, protected=False, selected=False)
            finally:
                held.__exit__()
        self.assertEqual(decision, staging.OCCUPIED, why)
        self.assertNotIn(decision, staging.REMOVES)

    def test_an_owner_that_could_not_be_established_keeps_the_directory(self):
        """The asymmetry is the point: a directory kept is a residual path somebody can read
        about, and a directory removed while its owner was still building is gone."""
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            _claim_written(environment, staging.STAGING)
            decision, why = staging.decide(
                staging.read_claim(environment), staging.UNKNOWN,
                occupied=True, protected=False, selected=False)
        self.assertEqual(decision, staging.KEEP, why)
        self.assertNotIn(decision, staging.REMOVES)

    def test_liveness_is_unknown_and_not_dead_where_it_cannot_be_asked(self):
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            environment.mkdir()
            with mock.patch.object(staging, "fcntl", None):
                liveness, detail = staging.owner_liveness(environment)
        self.assertEqual(liveness, staging.UNKNOWN, detail)

    def test_an_unreadable_claim_keeps_the_directory(self):
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            environment.mkdir()
            staging.claim_path(environment).write_text("{ not json", encoding="utf-8")
            claim = staging.read_claim(environment)
            self.assertFalse(claim.usable)
            decision, why = staging.decide(claim, staging.DEAD, occupied=True, protected=False, selected=False)
        self.assertEqual(decision, staging.KEEP, why)

    def test_a_claim_whose_state_is_not_one_of_the_two_is_unreadable(self):
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            environment.mkdir()
            staging.claim_path(environment).write_text(
                json.dumps({"state": "HALFWAY"}), encoding="utf-8")
            claim = staging.read_claim(environment)
        self.assertFalse(claim.usable)
        self.assertEqual(claim.state, reading.UNREADABLE)

    def test_a_directory_in_use_is_never_removed_however_its_claim_reads(self):
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            _claim_written(environment, staging.STAGING)
            decision, why = staging.decide(
                staging.read_claim(environment), staging.DEAD, occupied=True, protected=True, selected=True)
        self.assertNotIn(decision, staging.REMOVES, why)

    def test_an_installed_and_selected_environment_is_already_done(self):
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            _claim_written(environment, staging.COMPLETE)
            decision, why = staging.decide(
                staging.read_claim(environment), staging.DEAD, occupied=True, protected=True, selected=True)
        self.assertEqual(decision, staging.SETTLED, why)


class PointerSwapTests(unittest.TestCase):
    def test_the_pointer_swaps_atomically_and_resolves_into_the_environment(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "envA").mkdir()
            (root / "envB").mkdir()
            path = pointer.pointer_path(root)
            self.assertEqual(pointer.read(path)["state"], pointer.NO_POINTER)

            pointer.place(path, root / "envA")
            self.assertEqual(pointer.read(path)["state"], pointer.LINK)
            self.assertTrue(pointer.names(path, root / "envA"))
            self.assertFalse(pointer.names(path, root / "envB"))

            pointer.place(path, root / "envB")
            self.assertTrue(pointer.names(path, root / "envB"),
                            "a pointer is repointed, never appended to")

    def test_a_real_directory_where_the_pointer_goes_is_not_a_pointer(self):
        with tempfile.TemporaryDirectory() as temporary:
            real = Path(temporary) / pointer.POINTER_NAME
            real.mkdir()
            answer = pointer.read(real)
        self.assertEqual(answer["state"], pointer.NOT_A_LINK)
        self.assertFalse(pointer.usable(answer["state"]),
                         "somebody's real directory is never placed over")

    def test_a_pointer_that_could_not_be_read_answers_neither_yes_nor_no(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "current"
            with mock.patch.object(pointer.os, "lstat", side_effect=PermissionError("denied")):
                self.assertEqual(pointer.read(path)["state"], pointer.UNREACHABLE)
                self.assertIsNone(pointer.names(path, temporary),
                                  "an unread pointer says nothing about what it names")

    def test_no_pointer_is_an_established_no_rather_than_an_unknown(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "current"
            self.assertIs(pointer.names(path, temporary), False)


class SwapGateTests(unittest.TestCase):
    """OPS-4.4 read as three cells. Two of the three verdicts keep the installation."""

    def _cells(self, *, running=False, open_attempts=0, store=None, candidate=None):
        same = {"a": "CREATE TABLE a (x TEXT)", "b": "CREATE TABLE b (y TEXT)"}
        store = store if store is not None else {"readable": True, "present": True,
                                                 "tables": dict(same), "dbPath": "/d"}
        candidate = candidate if candidate is not None else {"readable": True,
                                                             "tables": dict(same)}
        return {
            "daemon": swapgate.daemon_cell(
                {"ok": True, "payload": {"running": running}, "command": ["service", "status"]}),
            "inFlight": swapgate.inflight_cell(
                {"ok": True, "command": ["doctor"],
                 "payload": {"contents": {"available": True, "openAttempts": open_attempts}}}),
            "storeTables": swapgate.tables_cell(store, candidate),
        }

    def test_a_stopped_daemon_with_nothing_in_flight_and_agreeing_tables_is_allowed(self):
        self.assertEqual(swapgate.decide(self._cells())["verdict"], swapgate.ALLOWED)

    def test_a_running_daemon_blocks(self):
        answer = swapgate.decide(self._cells(running=True))
        self.assertEqual(answer["verdict"], swapgate.BLOCKED)
        self.assertTrue(any("daemon" in reason for reason in answer["blockedBy"]))

    def test_an_attempt_still_in_flight_blocks(self):
        answer = swapgate.decide(self._cells(open_attempts=2))
        self.assertEqual(answer["verdict"], swapgate.BLOCKED)
        self.assertTrue(any("inFlight" in reason for reason in answer["blockedBy"]))

    def test_a_store_holding_tables_the_candidate_does_not_declare_blocks(self):
        cells = self._cells(store={"readable": True, "present": True, "dbPath": "/d",
                                   "tables": {"a": "CREATE TABLE a (x TEXT)",
                                              "b": "CREATE TABLE b (y TEXT)",
                                              "verdicts": "CREATE TABLE verdicts (v TEXT)"}},
                            candidate={"readable": True,
                                       "tables": {"a": "CREATE TABLE a (x TEXT)",
                                                  "b": "CREATE TABLE b (y TEXT)"}})
        answer = swapgate.decide(cells)
        self.assertEqual(cells["storeTables"]["answer"], swapgate.NARROWS)
        self.assertEqual(answer["verdict"], swapgate.BLOCKED)
        self.assertIn("verdicts", cells["storeTables"]["detail"],
                      "the refusal names the table that would be stranded")

    def test_a_candidate_that_adds_tables_refuses_and_says_which_tables(self):
        """Additive is still a schema change. The relay runs its whole DDL on every write-open,
        so allowing this would have the new daemon perform the migration OPS-4.5 reserves for
        its own issue with its own backup."""
        cells = self._cells(store={"readable": True, "present": True, "dbPath": "/d",
                                   "tables": {"a": "CREATE TABLE a (x TEXT)"}},
                            candidate={"readable": True,
                                       "tables": {"a": "CREATE TABLE a (x TEXT)",
                                                  "b": "CREATE TABLE b (y TEXT)"}})
        self.assertEqual(cells["storeTables"]["answer"], swapgate.EXTENDS)
        self.assertEqual(swapgate.decide(cells)["verdict"], swapgate.BLOCKED)
        self.assertIn("b", cells["storeTables"]["detail"])
        self.assertNotEqual(cells["storeTables"]["answer"], swapgate.NARROWS,
                            "adding is reported as its own answer, not as a downgrade")

    def test_a_table_defined_differently_refuses_even_though_the_names_agree(self):
        """Names alone agreed while a column differed, which is the schema change a name
        comparison cannot see."""
        cells = self._cells(store={"readable": True, "present": True, "dbPath": "/d",
                                   "tables": {"a": "CREATE TABLE a (x TEXT)"}},
                            candidate={"readable": True,
                                       "tables": {"a": "CREATE TABLE a (x TEXT, y INT)"}})
        self.assertEqual(cells["storeTables"]["answer"], swapgate.DIFFERS)
        self.assertEqual(swapgate.decide(cells)["verdict"], swapgate.BLOCKED)

    def test_whitespace_is_not_a_schema_change(self):
        cells = self._cells(store={"readable": True, "present": True, "dbPath": "/d",
                                   "tables": {"a": "CREATE TABLE a (x TEXT)"}},
                            candidate={"readable": True,
                                       "tables": {"a": "CREATE  TABLE   a (x TEXT)"}})
        self.assertEqual(cells["storeTables"]["answer"], swapgate.AGREES)

    def test_no_store_is_absence_and_not_agreement(self):
        cells = self._cells(store={"readable": True, "present": False, "dbPath": "/d",
                                   "tables": None})
        self.assertEqual(cells["storeTables"]["answer"], swapgate.NO_STORE)
        self.assertEqual(swapgate.decide(cells)["verdict"], swapgate.ALLOWED)

    def test_each_cell_that_cannot_be_read_keeps_the_installation(self):
        unreadable = {
            "daemon": {"daemon": swapgate.daemon_cell({"ok": False, "unreadable": "no binary"})},
            "inFlight": {"inFlight": swapgate.inflight_cell({"ok": False, "stderr": "boom"})},
            "inFlight contents": {"inFlight": swapgate.inflight_cell(
                {"ok": True, "payload": {"contents": {"available": False,
                                                      "detail": "not readable"}}})},
            "storeTables": {"storeTables": swapgate.tables_cell(
                {"readable": False, "detail": "denied"},
                {"readable": True, "tables": {"a": "CREATE TABLE a (x TEXT)"}})},
            "candidate tables": {"storeTables": swapgate.tables_cell(
                {"readable": True, "present": True, "tables": {"a": "CREATE TABLE a (x TEXT)"}},
                {"readable": False, "detail": "the candidate could not be asked"})},
        }
        for label, override in unreadable.items():
            with self.subTest(label):
                cells = dict(self._cells(), **override)
                answer = swapgate.decide(cells)
                self.assertEqual(answer["verdict"], swapgate.UNESTABLISHED,
                                 label + ": a check that could not be made is not one that"
                                 " passed")
                self.assertNotEqual(answer["verdict"], swapgate.ALLOWED, label)

    def test_a_daemon_that_could_not_be_asked_is_never_reported_stopped(self):
        cell = swapgate.daemon_cell({"ok": False, "unreadable": "the command failed"})
        self.assertFalse(cell["readable"])
        self.assertNotEqual(cell["answer"], scope.STOPPED)

    def test_an_established_refusal_is_named_even_when_another_cell_was_unread(self):
        cells = dict(self._cells(running=True),
                     storeTables=swapgate.tables_cell(
                         {"readable": False, "detail": "denied"},
                         {"readable": True, "tables": {"a": "CREATE TABLE a (x TEXT)"}}))
        answer = swapgate.decide(cells)
        self.assertEqual(answer["verdict"], swapgate.BLOCKED)
        self.assertTrue(answer["blockedBy"], "the actionable blocker is still named")
        self.assertTrue(answer["unreadable"], "and the unread cell is still reported")


class GateCellCoverageTests(unittest.TestCase):
    """The inventory: a cell carries the reading that answers it AND the predicate that judges
    it, so a cell cannot be declared with one rule and decided by another."""

    def test_every_declared_cell_carries_a_reading_and_a_predicate(self):
        for name, member in swapgate.GATE_CELLS.items():
            with self.subTest(name):
                self.assertIsInstance(member, tuple)
                self.assertEqual(len(member), 2, name + " is (reading, predicate)")
                where, predicate = member
                self.assertEqual(len(where), 2, name + " names (module, attribute)")
                module = {"scope": scope, "swapgate": swapgate}[where[0]]
                self.assertTrue(hasattr(module, where[1]),
                                name + " names a reading that does not exist: " + str(where))
                self.assertTrue(callable(predicate), name + " must carry its predicate")

    def test_the_verdict_decides_on_every_declared_cell_and_no_others(self):
        answer = swapgate.decide({})
        self.assertEqual(set(answer["cells"]), set(swapgate.GATE_CELLS))
        self.assertEqual(answer["verdict"], swapgate.UNESTABLISHED,
                         "a cell nobody read is not a cell that agreed")

    def test_the_gate_applies_the_declared_predicate_rather_than_one_of_its_own(self):
        source = ast.unparse(_function_named(
            ROOT / "scripts" / "crw_runtime" / "swapgate.py", "blocking"))
        self.assertIn("GATE_CELLS", source,
                      "the predicate comes off the declaration, not out of this function")



class _Host:
    """A host that already has one installation, a pointer, a registration and a live store.

    Everything an update could destroy, in the state an update finds it in, so a test can
    assert on what SURVIVED rather than on what the command said it did.
    """

    def __init__(self, temporary):
        import runtime_install

        self.root = Path(temporary)
        self.data = definition.load()
        combined = hashlib.sha256(
            "".join(c["sourceDigest"] for c in self.data["components"]).encode()).hexdigest()[:12]
        self.destination = self.root / "dest"
        self.candidate = self.destination / (
            "env-" + str(self.data["definitionVersion"]) + "-" + combined)
        self.previous = self.destination / "env-previous"
        (self.previous / "bin").mkdir(parents=True)
        self.previous_site = self.previous / "site"
        self.previous_site.mkdir()

        self.record_path = self.root / "record.json"
        record = hostrecord.empty(self.data["definitionVersion"])
        for component in self.data["components"]:
            hostrecord.put_install(record, component["component"], {
                "location": str(self.previous_site / component["module"]),
                "environment": str(self.previous),
                "entryPoint": str(self.previous / "bin" / component["consoleScript"]),
                "interpreterPath": str(self.previous / "bin" / "python"),
            })
        record["selected"] = {c["component"]: str(self.previous_site / c["module"])
                              for c in self.data["components"]}
        self.pointer_path = pointer.pointer_path(self.destination)
        # Recorded as well as placed, which is what a previous run of this command does. A
        # pointer on disk that the record never recorded is somebody else's link, and the
        # install refuses to replace one -- so a fixture that placed it without recording it
        # would be modelling a state this command never produces.
        record["pointer"] = {"path": str(self.pointer_path), "recordedAt": "2026-09-18T00:00:00Z",
                             "recordedBy": "CRW-49"}
        hostrecord.save(self.record_path, record)
        pointer.place(self.pointer_path, self.previous)

        self.codex_home = self.root / "codex"
        self.codex_home.mkdir()
        self.config = self.codex_home / "config.toml"
        bridge = component_of_for_test(self.data, runtime_install.BRIDGE)
        self.config.write_text(
            '[mcp_servers.' + runtime_install.MCP_NAME + ']\ncommand = "'
            + str(self.pointer_path / "bin" / bridge["consoleScript"]) + '"\n',
            encoding="utf-8")

        # A populated store, exactly where an install must never reach.
        self.state = self.root / "state"
        self.state.mkdir()
        self.store = self.state / "relay.sqlite3"
        connection = sqlite3.connect(self.store)
        connection.execute("CREATE TABLE relationships (relationship_id TEXT PRIMARY KEY)")
        connection.execute("CREATE TABLE attempts (event_id TEXT)")
        connection.execute("INSERT INTO relationships VALUES ('rel-kept')")
        connection.execute("INSERT INTO attempts VALUES ('event-kept')")
        connection.commit()
        connection.close()

    def snapshot(self):
        """Everything a failed update promised not to change."""
        info = self.store.stat()
        connection = sqlite3.connect(self.store)
        rows = sorted(r[0] for r in connection.execute("SELECT relationship_id FROM relationships"))
        rows += sorted(r[0] for r in connection.execute("SELECT event_id FROM attempts"))
        connection.close()
        return {
            "selected": (hostrecord.load(self.record_path,
                                         self.data["definitionVersion"]).value or {}).get("selected"),
            "pointerTarget": pointer.read(self.pointer_path).get("target"),
            "config": self.config.read_bytes(),
            "storeBytes": self.store.read_bytes(),
            "storeInode": (info.st_dev, info.st_ino),
            "storeRows": rows,
        }


def component_of_for_test(data, name):
    return next(c for c in data["components"] if c["component"] == name)


# Only the two build commands are simulated below, and which one an argv is has to be read from
# the command it runs -- the module the interpreter is told to run, and the operation handed to
# that module -- never from the text of the paths in it. A temporary destination is named by the
# host and is free to spell `pip` or `venv`; searching the joined argv for those words then made
# the environment command answer to the package injection and stubbed out
# `scripts/install.py --check` instead of running it, on the hosts whose temporary name happened
# to spell it and nowhere else (CRW-107).
def module_invocation(argv):
    """Return `(module, operands)` for `<interpreter> [options] -m <module> [operands]`.

    `-m` counts only inside the leading option block, where the interpreter reads it: a `-c`, a
    bare `--` and the first operand all end option processing, so a `-m` after any of them is an
    argument to the program rather than a module selector. An interpreter option that takes a
    SEPARATE operand -- `-X dev`, `-W error` -- ends the walk early and reads as no module at
    all. Neither the installer nor this file emits one, and that is the safe direction to be
    wrong in: a command this cannot name runs for real instead of being silently simulated.
    """
    parts = [str(a) for a in argv]
    for index in range(1, len(parts)):
        token = parts[index]
        if token == "-m":
            return (parts[index + 1] if index + 1 < len(parts) else None), parts[index + 2:]
        if token in ("-c", "--") or not token.startswith("-"):
            break
    return None, []


def build_step(argv):
    """Name the installer build step an argv performs, or None when it performs neither.

    The shapes are the installer's own: `<interpreter> -m venv <environment>` creates the
    environment and `<python> -m pip install --quiet <package>...` installs the packages. Git,
    the `-c` import probes and `scripts/install.py --check` are not build steps and must run.
    """
    module, operands = module_invocation(argv)
    if module == "venv":
        return "create environment"
    if module == "pip":
        operation = next((o for o in operands if not o.startswith("-")), None)
        if operation == "install":
            return "install packages"
    return None


# What each simulated build step says when it is made to fail. The wording is this fixture's;
# what the assertions read is which step carries it.
BUILD_REFUSALS = {
    "create environment": "venv refused to build",
    "install packages": "no matching distribution",
}


class BuildStepNamingTests(unittest.TestCase):
    """The naming the failure injection rests on, checked against the commands themselves.

    A destination directory is named by the host, so a name that happens to contain `pip` or
    `venv` must not turn an unrelated command into a build step, and must not stop one of the
    two real build commands from being recognised as itself.
    """

    CONTAMINATED = "/var/tmp/crw107-pip-venv-42"

    def test_the_two_build_commands_are_named_from_module_and_operation(self):
        environment = Path(self.CONTAMINATED) / "dest" / "env-1-a8ffcbfd23f0"
        cases = (
            ([sys.executable, "-m", "venv", str(environment)], "create environment"),
            ([str(environment / "bin" / "python"), "-m", "pip", "install", "--quiet",
              str(ROOT / "packages" / "codex-thread-bridge"),
              str(ROOT / "packages" / "codex-session-relay")], "install packages"),
        )
        for argv, expected in cases:
            with self.subTest(expected):
                self.assertEqual(build_step(argv), expected)

    def test_a_command_whose_path_spells_a_build_tool_is_not_a_build_step(self):
        environment = Path(self.CONTAMINATED) / "dest" / "env-1-a8ffcbfd23f0"
        cases = {
            # The one the joined-argv match stubbed out: a real check that quietly stopped
            # running in any destination whose name spelled a build tool.
            "the installed-skill check": [sys.executable, str(ROOT / "scripts" / "install.py"),
                                          "--check", "--dest", self.CONTAMINATED + "/skills"],
            "an import probe": [str(environment / "bin" / "python"), "-c",
                                "import codex_session_relay"],
            "the settings probe": [str(environment / "bin" / "python"), "-B", "-c",
                                   "import json, codex_session_relay"],
            "a git read": ["git", "-C", self.CONTAMINATED, "rev-parse", "HEAD"],
            "an argument that only looks like one": [sys.executable, "--", "-m", "pip",
                                                     "install"],
        }
        for label, argv in cases.items():
            with self.subTest(label):
                self.assertIsNone(build_step(argv), label + " is not a build step")

    def test_pip_names_the_install_step_only_when_it_installs(self):
        self.assertIsNone(build_step([sys.executable, "-m", "pip", "--version"]))
        self.assertEqual(build_step([sys.executable, "-m", "pip", "--quiet", "install", "/pkg"]),
                         "install packages")

    def test_the_naming_covers_every_build_command_the_installer_performs(self):
        """Read the build commands back out of the installer and name them from here.

        build_step mirrors argv that lives in another file. If the installer ever builds a
        different way, or adds a third build step, a fixture that cannot name the new shape
        stops simulating it: the injection passes straight through and the boundary case
        reports a step nothing failed at. So the shapes are read from the source rather than
        trusted to stay where they were, and every non-literal argument is substituted with a
        path that spells both build tools, which is the one thing the naming may not read.
        """
        contaminated = self.CONTAMINATED + "/dest"
        performed = {}
        for node in ast.walk(ast.parse(RUNTIME.read_text(encoding="utf-8"))):
            if (isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                    and node.func.id == "perform" and len(node.args) >= 2
                    and isinstance(node.args[0], ast.Constant)
                    and isinstance(node.args[1], ast.List)):
                performed[node.args[0].value] = node.args[1].elts
        self.assertEqual(sorted(performed), sorted(BUILD_REFUSALS),
                         "the installer performs a build step this fixture cannot inject into")
        for step, elements in performed.items():
            argv = []
            for element in elements:
                if isinstance(element, ast.Constant):
                    argv.append(element.value)
                elif isinstance(element, ast.Starred):
                    argv.append(contaminated + "/a-package")
                else:
                    argv.append(contaminated + "/bin/python")
            with self.subTest(step):
                self.assertEqual(build_step(argv), step,
                                 "scripts/runtime_install.py performs " + step + " as "
                                 + " ".join(argv) + ", which this fixture must recognise")


class UpdateRecoveryTests(unittest.TestCase):
    """Failure injected at each boundary an update crosses.

    The assertion is never "it said it failed". It is that the previous runtime is still
    selected, the pointer still reaches it, the registration is byte-identical, and the store is
    the same file with the same rows in it.
    """

    # Every failure this fixture injects, with the step the result has to name. The boundary
    # case and the destination-name contrast both read it, so an injection added here is
    # covered by both rather than by whichever one was remembered.
    BOUNDARY_STEPS = {
        "create environment": "create environment",
        "install packages": "install packages",
        "replace the owned pointer": "replace the owned pointer",
        "read the owned pointer back": "read the owned pointer back",
    }

    def _run(self, host, *, breaking=None, gate=None, interpose=None, probes=None,
             clean_store=False):
        import runtime_install

        emitted = []
        args = argparse.Namespace(
            dest=str(host.destination), apply=True, record=str(host.record_path),
            python=sys.executable, socket=None, state=str(host.state), issue="CRW-49",
            codex_home=str(host.codex_home))

        # Only the two build steps are simulated. Everything else -- git above all, which
        # verify-definition re-derives the component trees with -- runs for real, because a
        # blanket fake makes every derived field empty and the command refuses for a reason
        # that has nothing to do with the boundary under test.
        real_run = runtime_install.subprocess.run

        def fake_run(argv, **kwargs):
            step = build_step(argv)
            if step is None:
                return real_run(argv, **kwargs)
            if step == breaking:
                return subprocess.CompletedProcess(argv, 1, "", BUILD_REFUSALS[step])
            return subprocess.CompletedProcess(argv, 0, "", "")

        def fake_location(python, module):
            site = host.candidate / "site" / module
            site.mkdir(parents=True, exist_ok=True)
            return str(site), None, [str(python), "-c", "import " + module]

        digests = {c["module"]: c["sourceDigest"] for c in host.data["components"]}

        def fake_digest(location):
            return digests[Path(location).name]

        def fake_relay(command, **kwargs):
            if "doctor" in command:
                if clean_store:
                    # What the relay actually reports for a store that is not there: contents
                    # unavailable, with no count. Measured, not invented.
                    return {"ok": True, "command": list(command),
                            "payload": {"contents": {
                                "available": False, "relationships": None,
                                "openAttempts": None,
                                "detail": "the database is not readable from this process"}}}
                return {"ok": True, "command": list(command),
                        "payload": {"contents": {"available": True, "openAttempts": 0}}}
            return {"ok": True, "command": list(command), "payload": {"running": False}}

        schema = {"relationships": "CREATE TABLE relationships (relationship_id TEXT PRIMARY KEY)",
                  "attempts": "CREATE TABLE attempts (event_id TEXT)"}
        tables = {"readable": True, "present": not clean_store,
                  "tables": None if clean_store else dict(schema),
                  "dbPath": str(host.store)}
        if gate == "running daemon":
            def fake_relay(command, **kwargs):                        # noqa: F811
                if "doctor" in command:
                    return {"ok": True, "command": list(command),
                            "payload": {"contents": {"available": True, "openAttempts": 0}}}
                return {"ok": True, "command": list(command), "payload": {"running": True}}
        if gate == "handover in flight":
            def fake_relay(command, **kwargs):                        # noqa: F811
                if "doctor" in command:
                    return {"ok": True, "command": list(command),
                            "payload": {"contents": {"available": True, "openAttempts": 3}}}
                return {"ok": True, "command": list(command), "payload": {"running": False}}
        if gate == "unreadable daemon":
            def fake_relay(command, **kwargs):                        # noqa: F811
                if "doctor" in command:
                    return {"ok": True, "command": list(command),
                            "payload": {"contents": {"available": True, "openAttempts": 0}}}
                return {"ok": False, "command": list(command), "unreadable": "no such binary"}
        candidate_declares = dict(schema)
        if gate == "store would be downgraded":
            candidate_declares = {"relationships": schema["relationships"]}

        patches = [
            mock.patch.object(runtime_install, "emit", side_effect=emitted.append),
            mock.patch.object(runtime_install.subprocess, "run", side_effect=fake_run),
            mock.patch.object(runtime_install, "module_location", side_effect=fake_location),
            mock.patch.object(runtime_install, "interpreter_version", return_value="3.12.0"),
            mock.patch.object(runtime_install.definition, "ops12_digest",
                              side_effect=fake_digest),
            mock.patch.object(runtime_install, "measure_candidate",
                              side_effect=lambda *a, **k: (
                                  interpose() if interpose else None,
                                  {"qualifyingPoint": True, "points": [],
                                   "appServer": "a-server"})[1]),
            mock.patch.object(runtime_install.scope, "relay", side_effect=fake_relay),
            mock.patch.object(runtime_install, "store_presence", create=True,
                              return_value={"readable": True, "present": not clean_store,
                                            "dbPath": str(host.store),
                                            "command": ["store-presence"]}),
            mock.patch.object(runtime_install, "store_tables",
                              side_effect=lambda interpreter, *a, **k: (
                                  probes.append(str(interpreter)) if probes is not None else None,
                                  tables)[1]),
            mock.patch.object(runtime_install, "candidate_tables",
                              return_value={"readable": True, "tables": candidate_declares}),
            mock.patch.object(runtime_install, "classify_component",
                              return_value={"class": ownership.OWN, "reasons": ["for this case"]}),
        ]
        if breaking == "replace the owned pointer":
            # The link LANDS and then the call fails, which is the case the code claims to
            # handle: place() can raise with the replacement already made. Raising before it
            # touches anything would leave the restoration with nothing to undo and the test
            # would pass without exercising it.
            real_place = runtime_install.pointer.place

            def place_then_fail(path, target):
                real_place(path, target)
                raise OSError("read-only filesystem")

            patches.append(mock.patch.object(runtime_install.pointer, "place",
                                             side_effect=place_then_fail))
        if breaking == "read the owned pointer back":
            # Transient, which is the reported scenario: the verification cannot establish the
            # target once. A permanent failure would also stop the RESTORATION from confirming
            # itself, and then the honest outcome is a reported residual rather than a rollback
            # -- which the restoration test injects directly instead.
            real_names = runtime_install.pointer.names
            failed_once = []

            def flaky_names(path, environment):
                if not failed_once:
                    failed_once.append(True)
                    return False
                return real_names(path, environment)

            patches.append(mock.patch.object(runtime_install.pointer, "names",
                                             side_effect=flaky_names))
        for entered in patches:
            entered.__enter__()
        try:
            code = runtime_install.cmd_install(args)
        finally:
            for entered in reversed(patches):
                entered.__exit__(None, None, None)
        return code, emitted[-1]

    def test_an_update_that_fails_at_any_boundary_keeps_everything_it_found(self):
        boundaries = [
            ("create environment", None),
            ("install packages", None),
            (None, "running daemon"),
            (None, "handover in flight"),
            (None, "unreadable daemon"),
            (None, "store would be downgraded"),
            ("replace the owned pointer", None),
            ("read the owned pointer back", None),
        ]
        for breaking, gate in boundaries:
            label = breaking or gate
            with self.subTest(label):
                with tempfile.TemporaryDirectory() as temporary:
                    host = _Host(temporary)
                    before = host.snapshot()
                    code, payload = self._run(host, breaking=breaking, gate=gate)
                    after = host.snapshot()

                self.assertEqual(code, 1, label + ": a failed update must not report success")
                self.assertEqual(after["selected"], before["selected"],
                                 label + ": the previous runtime stays selected")
                self.assertEqual(after["pointerTarget"], before["pointerTarget"],
                                 label + ": the command a host reaches is unchanged")
                self.assertEqual(after["config"], before["config"],
                                 label + ": the owned configuration is byte-identical")
                self.assertEqual(after["storeBytes"], before["storeBytes"],
                                 label + ": the store file is untouched")
                self.assertEqual(after["storeInode"], before["storeInode"],
                                 label + ": the store was not moved or recreated")
                self.assertEqual(after["storeRows"], before["storeRows"],
                                 label + ": relationships and attempts survive")
                self.assertEqual(after["pointerTarget"], str(host.previous),
                                 label + ": the pointer still names the previous runtime")

    def test_each_failure_names_the_boundary_it_stopped_at(self):
        for breaking, step in self.BOUNDARY_STEPS.items():
            with self.subTest(breaking):
                with tempfile.TemporaryDirectory() as temporary:
                    host = _Host(temporary)
                    code, payload = self._run(host, breaking=breaking)
                self.assertEqual(code, 1)
                self.assertEqual(payload["failedStep"], step,
                                 "a reader must not have to infer where it stopped")

    def test_the_boundary_is_read_from_the_command_and_not_the_path_it_ran_in(self):
        """The same injections again, in destinations that spell the build tools.

        A temporary directory is named by the host. When that name contained `pip`, the
        joined-argv match made the environment command answer to the package injection: the run
        stopped at the first boundary and reported the second one. Both spellings exit 1 either
        way, so only an assertion that names the step sees it, and only on the hosts whose
        temporary name happens to spell it -- which is why it surfaced as an unrelated PR's CI
        failing and not as a failure here.
        """
        parent = Path(tempfile.mkdtemp(prefix="crw107-contrast-"))
        try:
            self.assertFalse(
                [t for t in ("pip", "venv") if t in str(parent)],
                "the system temporary directory " + str(parent) + " spells a build tool, so the"
                " neutral destination here would not be neutral: point TMPDIR at one that does"
                " not, because this contrast means nothing without a baseline")
            for name in ("plain", "pip", "venv", "pip-venv"):
                destination = parent / ("crw107-" + name)
                for token in ("pip", "venv"):
                    self.assertEqual(
                        token in str(destination), token in name,
                        "a case only means something if its destination really does or does"
                        " not spell " + token)
                for breaking, step in self.BOUNDARY_STEPS.items():
                    with self.subTest(destination=name, breaking=breaking):
                        destination.mkdir()
                        try:
                            code, payload = self._run(_Host(str(destination)), breaking=breaking)
                        finally:
                            shutil.rmtree(destination, ignore_errors=True)
                        self.assertEqual(code, 1)
                        self.assertEqual(
                            payload["failedStep"], step,
                            "a destination named " + destination.name + " must not move the"
                            " boundary the run stopped at")
        finally:
            shutil.rmtree(parent, ignore_errors=True)

    def test_a_gate_refusal_reports_the_gate_that_refused(self):
        for gate in ("running daemon", "handover in flight", "store would be downgraded"):
            with self.subTest(gate):
                with tempfile.TemporaryDirectory() as temporary:
                    host = _Host(temporary)
                    code, payload = self._run(host, gate=gate)
                self.assertEqual(code, 1)
                self.assertEqual(payload["failedStep"], "read whether it is safe to swap")
                self.assertEqual(payload["swapGate"]["verdict"], swapgate.BLOCKED)
                self.assertTrue(payload["swapGate"]["blockedBy"], gate)

    def test_a_gate_that_could_not_be_read_keeps_the_installation_too(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            before = host.snapshot()
            code, payload = self._run(host, gate="unreadable daemon")
            after = host.snapshot()
        self.assertEqual(code, 1)
        self.assertEqual(payload["swapGate"]["verdict"], swapgate.UNESTABLISHED)
        self.assertEqual(after["selected"], before["selected"])
        self.assertEqual(after["pointerTarget"], before["pointerTarget"])

    def test_a_pointer_failure_after_the_selection_committed_puts_the_selection_back(self):
        """The two truths are written one after the other. Whichever one lands first, the pair
        has to end up agreeing, or recovery reads one of them and deletes what the other uses."""
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            before = host.snapshot()
            code, payload = self._run(host, breaking="replace the owned pointer")
            after = host.snapshot()
        self.assertEqual(code, 1)
        self.assertEqual(after["selected"], before["selected"],
                         "the selection was put back when the pointer would not move")
        self.assertEqual(payload["pointer"]["restored"]["restored"],
                         sorted(before["selected"]),
                         "and the result says which components it put back")

    def test_a_successful_update_moves_the_pointer_and_settles_its_claim(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            code, payload = self._run(host)
            target = pointer.read(host.pointer_path)["target"]
            claim = staging.read_claim(host.candidate)
            after = host.snapshot()

            self.assertEqual(code, 0, json.dumps(payload)[:2000])
            self.assertTrue(payload["promoted"])
            self.assertEqual(target, str(host.candidate),
                             "the pointer now reaches the new runtime")
            self.assertTrue(staging.settled(claim), "the claim settles only after both moved")
            self.assertEqual(after["storeRows"], ["rel-kept", "event-kept"],
                             "a SUCCESSFUL update leaves the store alone as well")
            self.assertEqual(after["config"], host.config.read_bytes(),
                             "and writes nothing to the configuration, because the"
                             " registration already names the pointer")
            self.assertTrue(host.previous.is_dir(),
                            "the predecessor survives, which is what keeps a process that is"
                            " already running from it alive")



class IdempotentRepeatTests(unittest.TestCase):
    """Running it again, and running it again after a kill.

    The environment name is deterministic, so "run it again" used to mean "refused for ever".
    Nothing here may append a second MCP registration or a second hook identity either: those
    are append-only surfaces, so a duplicate is not a cosmetic defect, it is a second server
    Codex would spawn and a trusted hash detached from its hook.
    """

    def test_a_second_run_over_a_settled_installation_builds_nothing(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            code, payload = UpdateRecoveryTests()._run(host)
            self.assertEqual(code, 0, "the first run installs")

            before = host.snapshot()
            again, second = UpdateRecoveryTests()._run(host)
            after = host.snapshot()

        self.assertEqual(again, 0, json.dumps(second)[:1500])
        self.assertTrue(second["alreadyInstalled"])
        self.assertEqual(second["stagingDecision"], staging.SETTLED)
        self.assertFalse(second["applied"], "a settled installation is not rebuilt")
        self.assertEqual(after["selected"], before["selected"])
        self.assertEqual(after["pointerTarget"], before["pointerTarget"])
        self.assertEqual(after["config"], before["config"])

    def test_a_run_resumed_after_an_interrupted_one_reclaims_its_own_staging(self):
        """The kill this models is the one that leaves no chance to clean up: the directory and
        its claim are there, and nothing holds the lock."""
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            host.candidate.mkdir(parents=True)
            (host.candidate / "half-built").write_text("interrupted", encoding="utf-8")
            staging.write_claim(host.candidate, staging.STAGING, issue="CRW-49", run="killed")

            before = host.snapshot()
            code, payload = UpdateRecoveryTests()._run(host)
            after = host.snapshot()
            leftovers = sorted(p.name for p in host.destination.iterdir())
            rebuilt = sorted(p.name for p in host.candidate.iterdir())

        self.assertEqual(code, 0, json.dumps(payload)[:1500])
        reclaimed = [step for step in payload["steps"]
                     if step.get("step") == "reclaim abandoned staging"]
        self.assertTrue(reclaimed, "the abandoned staging is recognised as this command's own")
        self.assertNotIn("half-built", rebuilt,
                         "and it is rebuilt rather than resumed on top of half a build")
        self.assertEqual(sorted(leftovers),
                         sorted([host.candidate.name, host.previous.name,
                                 pointer.POINTER_NAME]),
                         "no orphan staging is left beside it")
        self.assertEqual(after["storeRows"], before["storeRows"])

    def test_a_directory_that_is_not_this_commands_is_never_reclaimed(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            host.candidate.mkdir(parents=True)
            (host.candidate / "somebody-elses-work").write_text("keep me", encoding="utf-8")

            before = host.snapshot()
            code, payload = UpdateRecoveryTests()._run(host)
            after = host.snapshot()
            survived = (host.candidate / "somebody-elses-work").read_text(encoding="utf-8")

        self.assertEqual(code, 1)
        self.assertEqual(payload["stagingDecision"], staging.FOREIGN)
        self.assertEqual(survived, "keep me", "only this command's own staging is cleaned up")
        self.assertEqual(after["selected"], before["selected"])
        self.assertEqual(after["pointerTarget"], before["pointerTarget"])

    def test_repeating_the_registration_appends_no_second_server(self):
        """One registration, however many times it is run.

        On an interpreter without tomllib the guarantee is kept by refusing rather than by
        appending: this module will not approximate TOML, so it writes nothing at all. Both
        outcomes are asserted, because the property is about never producing a second
        registration and both interpreters have to satisfy it.
        """
        import runtime_install

        readable = codexconfig.scan('[mcp_servers.x]' + chr(10) + 'command = "/x"' + chr(10))
        with tempfile.TemporaryDirectory() as temporary:
            codex_home = Path(temporary)
            config = codex_home / "config.toml"
            command = str(Path(temporary) / "dest" / "current" / "bin" / "codex-thread-bridge")
            args = argparse.Namespace(codex_home=str(codex_home),
                                      name=runtime_install.MCP_NAME,
                                      bridge_command=command, bridge_arg=None, apply=True)
            emitted = []
            with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                first = runtime_install.cmd_register_mcp(args)
                after_first = config.read_bytes() if config.exists() else None
                second = runtime_install.cmd_register_mcp(args)
                after_second = config.read_bytes() if config.exists() else None

        if not readable.readable:
            self.assertEqual((first, second), (1, 1),
                             "without tomllib every registration is refused rather than"
                             " approximated")
            self.assertIsNone(after_second, "and a refusal writes nothing at all")
            return

        self.assertEqual((first, second), (0, 0))
        self.assertEqual(emitted[0]["outcome"], codexconfig.CREATED)
        self.assertEqual(emitted[1]["outcome"], codexconfig.LINKED)
        self.assertEqual(after_first, after_second, "the second run wrote nothing")
        self.assertEqual(after_second.decode("utf-8").count("[mcp_servers."), 1,
                         "one registration, however many times it is run")

    def test_repeating_the_hook_appends_no_second_identity(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "hooks.json"
            hook = {"type": "command", "command": "crw-next-step", "timeout": 10}
            first = hooks.install(path, "SessionStart", hook, issue="CRW-49", apply=True)
            after_first = path.read_bytes()
            second = hooks.install(path, "SessionStart", hook, issue="CRW-49", apply=True)
            after_second = path.read_bytes()
            inventory = hooks.inventory(hooks.read(path).value)

        self.assertEqual(first["outcome"], hooks.CREATED)
        self.assertEqual(second["outcome"], hooks.LINKED)
        self.assertEqual(second["identity"], first["identity"],
                         "the identity reported is the one that is there, not the next slot")
        self.assertEqual(after_first, after_second, "the second run wrote nothing")
        self.assertEqual(len(inventory), 1, "one hook identity, however many times it is run")



class ClaimOwnershipTests(unittest.TestCase):
    """The two defects an independent review found here were both about deleting things.

    Both are the same shape: something that looked like proof of ownership was not. The lock was
    taken on a file that was then replaced by rename, and the claim was any readable JSON.
    """

    def test_writing_the_claim_does_not_release_the_lock(self):
        """The lock follows the inode, so locking a file that is later replaced by rename
        unlocks it silently. That reported a live build as abandoned, and the next run deleted
        the directory somebody was still building."""
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            environment.mkdir()
            held = staging.Held(environment).take()
            try:
                self.assertEqual(staging.owner_liveness(environment)[0], staging.LIVE)
                # Production order: take the lock, then write the claim.
                staging.write_claim(environment, staging.STAGING, issue="CRW-49", run="1")
                after, detail = staging.owner_liveness(environment)
            finally:
                held.__exit__()
            self.assertEqual(after, staging.LIVE,
                             "writing the claim must not hand the directory to a racing run: "
                             + detail)
            self.assertEqual(staging.owner_liveness(environment)[0], staging.DEAD,
                             "and releasing it really does release it")

    def test_the_lock_and_the_claim_are_separate_files(self):
        self.assertNotEqual(staging.CLAIM_NAME, staging.LOCK_NAME,
                            "the file that is rewritten cannot be the file that is locked")

    def test_a_claim_this_command_did_not_write_is_not_permission_to_delete(self):
        intruders = {
            "an empty object": {},
            "a state and nothing else": {"state": staging.STAGING},
            "somebody else's writer": {"claimVersion": 1, "state": staging.STAGING,
                                       "writtenBy": "some-other-tool"},
            "an unknown state": {"claimVersion": 1, "writtenBy": staging.WRITTEN_BY,
                                 "state": "HALFWAY"},
            "a future claim version": {"claimVersion": 99, "writtenBy": staging.WRITTEN_BY,
                                       "state": staging.STAGING},
        }
        for label, content in intruders.items():
            with self.subTest(label):
                with tempfile.TemporaryDirectory() as temporary:
                    environment = Path(temporary) / "env"
                    environment.mkdir()
                    staging.claim_path(environment).write_text(
                        json.dumps(content), encoding="utf-8")
                    (environment / "somebody-elses-work").write_text("keep", encoding="utf-8")
                    claim = staging.read_claim(environment)
                    decision, why = staging.decide(
                        claim, staging.DEAD,
                        occupied=staging.directory_occupied(environment)[0], protected=False, selected=False)
                self.assertFalse(claim.usable, label)
                self.assertNotIn(decision, staging.REMOVES,
                                 label + ": a file at that path is not proof of ownership")

    def test_a_promoted_environment_is_never_deleted_once_the_selection_moves_on(self):
        """A finished environment is a runtime that was promoted. A process may still be
        running out of it, which is the process liveness criterion 4 asks for."""
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            environment.mkdir()
            staging.write_claim(environment, staging.COMPLETE, issue="CRW-49", run="1")
            decision, why = staging.decide(
                staging.read_claim(environment), staging.DEAD, occupied=True, protected=False, selected=False)
        self.assertEqual(decision, staging.KEEP, why)
        self.assertNotIn(decision, staging.REMOVES)

    def test_an_empty_directory_is_taken_over_rather_than_deleted(self):
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            environment.mkdir()
            decision, why = staging.decide(
                staging.read_claim(environment), staging.DEAD,
                occupied=staging.directory_occupied(environment)[0], protected=False, selected=False)
        self.assertEqual(decision, staging.ADOPT, why)
        self.assertNotIn(decision, staging.REMOVES,
                         "nothing is removed for a directory that holds nothing")

    def test_an_interrupted_promotion_is_resumed_rather_than_rebuilt(self):
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            environment.mkdir()
            staging.write_claim(environment, staging.STAGING, issue="CRW-49", run="killed")
            decision, why = staging.decide(
                staging.read_claim(environment), staging.DEAD, occupied=True, protected=True, selected=True)
        self.assertEqual(decision, staging.RESUME, why)
        self.assertNotIn(decision, staging.REMOVES)

    def test_a_resume_needs_a_positive_selection_and_not_merely_protection(self):
        """protected is deliberately conservative: it says yes when a reading FAILED, because
        keeping a directory costs a report and removing a live one is unrecoverable. Acting on
        it would write a pointer on the strength of a reading nobody made."""
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            environment.mkdir()
            staging.write_claim(environment, staging.STAGING, issue="CRW-49", run="killed")
            claim = staging.read_claim(environment)
            resumed, _why = staging.decide(claim, staging.DEAD, occupied=True,
                                           protected=True, selected=True)
            unread, why = staging.decide(claim, staging.DEAD, occupied=True,
                                         protected=True, selected=None)
        self.assertEqual(resumed, staging.RESUME)
        self.assertEqual(unread, staging.KEEP, why)
        self.assertNotIn(unread, staging.REMOVES)

    def test_finishing_a_promotion_re_reads_the_selection_under_its_own_lock(self):
        source = RUNTIME.read_text(encoding="utf-8")
        body = source[source.index("def _finish_promotion("):source.index("def _names_environment(")]
        self.assertIn("hostrecord.Exclusive(record_path)", body)
        self.assertIn("hostrecord.load(record_path", body)
        self.assertLess(body.index("hostrecord.Exclusive(record_path)"),
                        body.index("hostrecord.load(record_path"),
                        "the selection is re-read inside the lock, not trusted from before it")
        self.assertLess(body.index("_names_environment"), body.index("pointer.place("),
                        "and the pointer is only aimed where the record is read to select")


class InterruptedPromotionTests(unittest.TestCase):
    """A kill inside the one window where a runtime is selected and unreachable."""

    def test_a_run_killed_between_the_selection_and_the_pointer_is_finished_not_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            # Exactly the state a kill in that window leaves: the record selects the candidate,
            # the pointer still reaches the predecessor, and the claim never settled.
            host.candidate.mkdir(parents=True)
            (host.candidate / "site").mkdir()
            staging.write_claim(host.candidate, staging.STAGING, issue="CRW-49", run="killed")
            hostrecord.update(host.record_path, host.data["definitionVersion"],
                              select={c["component"]: str(host.candidate / "site" / c["module"])
                                      for c in host.data["components"]})
            self.assertEqual(pointer.read(host.pointer_path)["target"], str(host.previous),
                             "the pointer is still on the predecessor")

            before = host.snapshot()
            code, payload = UpdateRecoveryTests()._run(host)
            after = host.snapshot()
            rebuilt = sorted(p.name for p in host.candidate.iterdir())
            predecessor_survived = host.previous.is_dir()

        self.assertEqual(code, 0, json.dumps(payload)[:1200])
        self.assertEqual(payload["stagingDecision"], staging.RESUME)
        self.assertTrue(payload["resumed"])
        self.assertEqual(after["pointerTarget"], str(host.candidate),
                         "the pointer is brought into agreement with the selection")
        self.assertEqual(after["selected"], before["selected"],
                         "and the selection it agrees with is the one already committed")
        self.assertIn("site", rebuilt, "nothing was rebuilt")
        self.assertEqual(after["storeRows"], before["storeRows"])
        self.assertTrue(predecessor_survived, "and nothing was removed")

    def test_the_promotion_holds_one_lock_across_both_writes(self):
        """The two truths are written inside one critical section, so no other run of this
        command can interleave and leave the record naming B while the pointer reaches A."""
        # Read from the source text, not from an unparsed tree: the ORDER of the two writes
        # inside one with-block is the property, and it is a property of the statements.
        source = RUNTIME.read_text(encoding="utf-8")
        promotion = source[source.index("        landed = None"):
                           source.index("                # Read back rather than trusted.")]
        lock = promotion.index("Exclusive(record_path)")
        commit = promotion.index("hostrecord.update(")
        place = promotion.index("pointer.place(")
        self.assertLess(lock, commit, "the lock opens before the selection is committed")
        self.assertLess(commit, place,
                        "and the selection is committed before the pointer moves, because a"
                        " pointer moved first can be deleted by recovery reading the other"
                        " truth")



class ReclaimRaceTests(unittest.TestCase):
    """Deciding and acting are one step, or a stale decision deletes a live build.

    The scenario an independent review reproduced: two retries both read an abandoned staging
    and both decide to reclaim it. The first deletes it, recreates it and starts building; the
    second then deletes that live directory on the strength of a decision it made before any of
    it happened. This exercises it concurrently rather than asserting on source text.
    """

    def test_a_stale_decision_cannot_delete_a_directory_somebody_has_taken_over(self):
        """The reproduced scenario, made deterministic.

        A first reading finds the staging abandoned. Before anything acts on it, another run
        takes the directory over and starts building. The reading that said DEAD is now stale,
        and acting on it deletes a live build. The decision is therefore taken again inside the
        lock, immediately before the removal, so what gets acted on is what is there now.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            host.candidate.mkdir(parents=True)
            (host.candidate / "leftover").write_text("abandoned", encoding="utf-8")
            staging.write_claim(host.candidate, staging.STAGING, issue="CRW-49", run="dead")

            # The stale reading: at this moment reclaiming really would be correct.
            stale, _why = staging.decide(
                staging.read_claim(host.candidate),
                staging.owner_liveness(host.candidate)[0],
                occupied=True, protected=False, selected=False)
            self.assertEqual(stale, staging.RECLAIM, "the first reading does say reclaim")

            # Another run now takes it over and is building in it.
            held = staging.Held(host.candidate).take()
            staging.write_claim(host.candidate, staging.STAGING, issue="CRW-49", run="rival")
            (host.candidate / "rival-work").write_text("building", encoding="utf-8")
            try:
                code, payload = UpdateRecoveryTests()._run(host)
            finally:
                held.__exit__()
            survived = (host.candidate / "rival-work").exists()

        self.assertEqual(code, 1, json.dumps(payload)[:900])
        self.assertEqual(payload["stagingDecision"], staging.OCCUPIED,
                         "the run must act on what it reads now, not on the earlier answer")
        self.assertTrue(survived, "a live build must never be deleted by a competing run")

    def test_a_directory_another_run_is_deciding_about_is_left_alone(self):
        """Two runs cannot decide at once. The loser reports that and touches nothing."""
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            host.candidate.mkdir(parents=True)
            (host.candidate / "leftover").write_text("abandoned", encoding="utf-8")
            staging.write_claim(host.candidate, staging.STAGING, issue="CRW-49", run="dead")
            decision_lock = hostrecord.Locked(host.candidate, timeout=0.1).__enter__()
            try:
                with mock.patch.object(runtime_install.hostrecord, "LOCK_TIMEOUT_SECONDS", 0.1):
                    code, payload = UpdateRecoveryTests()._run(host)
            finally:
                decision_lock.__exit__()
            survived = (host.candidate / "leftover").exists()

        self.assertEqual(code, 1)
        self.assertIn("another run is deciding", payload["refused"])
        self.assertTrue(survived, "a lock this run could not take establishes nothing")

    def test_deciding_creating_and_claiming_are_all_inside_one_lock(self):
        """Serialising the decision alone only moves the window.

        Between the exclusive mkdir and the claim, the directory is empty and carries no claim,
        which is exactly what another run reads as adoptable: it would remove it, recreate it
        and start building, and one of the two runs would then clean up the other's live build.
        So the span covers deciding, creating and claiming.
        """
        source = RUNTIME.read_text(encoding="utf-8")
        body = source[source.index("        taking = hostrecord.Locked(environment).__enter__()"):
                      source.index("        taking.__exit__()")]
        for step in ("staging.decide(", "shutil.rmtree(", "os.rmdir(", "environment.mkdir()",
                     "staging.Held(environment).take()", "staging.write_claim("):
            self.assertIn(step, body, step + " must be inside the span")

    def test_a_directory_being_created_is_not_adopted_out_from_under_its_creator(self):
        """The creator holds the span, so a competing run cannot see the empty window at all."""
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            # A creator that has made the directory and not yet claimed it, holding the span.
            host.candidate.mkdir(parents=True)
            span = hostrecord.Locked(host.candidate, timeout=0.1).__enter__()
            try:
                with mock.patch.object(runtime_install_module.hostrecord,
                                       "LOCK_TIMEOUT_SECONDS", 0.1):
                    code, payload = UpdateRecoveryTests()._run(host)
            finally:
                span.__exit__()
            still_there = host.candidate.is_dir()

        self.assertEqual(code, 1)
        self.assertIn("another run is deciding", payload["refused"])
        self.assertTrue(still_there,
                        "the empty window belongs to whoever holds the span, and a run that"
                        " cannot take it removes nothing")


class SchemaComparisonTests(unittest.TestCase):
    """A schema difference reported as agreement is the one direction this cell must not fail
    in, so normalisation stops exactly where meaning starts."""

    def _answer(self, store, candidate):
        return swapgate.tables_cell(
            {"readable": True, "present": True, "tables": store, "dbPath": "/d"},
            {"readable": True, "tables": candidate})["answer"]

    def test_a_literal_that_differs_only_in_case_is_a_difference(self):
        self.assertEqual(
            self._answer({"a": "CREATE TABLE a (x TEXT DEFAULT 'A')"},
                         {"a": "CREATE TABLE a (x TEXT DEFAULT 'a')"}),
            swapgate.DIFFERS, "lowercasing the whole statement hid this")

    def test_whitespace_inside_a_literal_is_a_difference(self):
        self.assertEqual(
            self._answer({"a": "CREATE TABLE a (x TEXT DEFAULT 'a  b')"},
                         {"a": "CREATE TABLE a (x TEXT DEFAULT 'a b')"}),
            swapgate.DIFFERS, "collapsing whitespace inside quotes hid this")

    def test_formatting_outside_quotes_is_not_a_difference(self):
        self.assertEqual(
            self._answer({"a": "CREATE TABLE a (x TEXT)"},
                         {"a": "CREATE  TABLE" + chr(10) + "  a (x TEXT)"}),
            swapgate.AGREES, "SQLite keeps the original text, so formatting drifts")

    def test_a_reading_carrying_only_names_cannot_answer_this_cell(self):
        cell = swapgate.tables_cell(
            {"readable": True, "present": True, "tables": ["a"], "dbPath": "/d"},
            {"readable": True, "tables": ["a"]})
        self.assertFalse(cell["readable"],
                         "two name-only readings agree while a column differs, so answering on"
                         " names is answering a different question")
        self.assertEqual(
            swapgate.decide(
                {"daemon": swapgate.daemon_cell({"ok": True, "payload": {"running": False}}),
                 "inFlight": swapgate.inflight_cell(
                     {"ok": True, "payload": {"contents": {"available": True,
                                                           "openAttempts": 0}}}),
                 "storeTables": cell})["verdict"],
            swapgate.UNESTABLISHED)


class NarrowReadingTests(unittest.TestCase):
    """Two questions that must not be answered by the conservative reading."""

    def test_already_installed_is_never_reported_from_a_reading_that_failed(self):
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            environment.mkdir()
            staging.write_claim(environment, staging.COMPLETE, issue="CRW-49", run="1")
            claim = staging.read_claim(environment)
            settled, _ = staging.decide(claim, staging.DEAD, occupied=True,
                                        protected=True, selected=True)
            unread, why = staging.decide(claim, staging.DEAD, occupied=True,
                                         protected=True, selected=None)
        self.assertEqual(settled, staging.SETTLED)
        self.assertEqual(unread, staging.KEEP, why)
        self.assertNotEqual(unread, staging.SETTLED,
                            "reporting success from a reading nobody made is worse than"
                            " reporting that nobody could read it")

    def test_a_partial_selection_does_not_authorise_moving_the_shared_pointer(self):
        import runtime_install

        data = definition.load()
        environment = Path("/somewhere/env")
        whole = {c["component"]: str(environment / "site" / c["module"])
                 for c in data["components"]}
        partial = dict(whole)
        partial.pop(sorted(partial)[0])
        self.assertTrue(runtime_install._names_environment({"selected": whole}, environment,
                                                           data))
        self.assertFalse(
            runtime_install._names_environment({"selected": partial}, environment, data),
            "an update moves a whole verified combination (OPS-2.4), so half a record is not"
            " a promotion to finish")


class RollbackRaceTests(unittest.TestCase):
    def test_a_rollback_never_undoes_a_promotion_another_run_committed(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            record_path = Path(temporary) / "record.json"
            hostrecord.save(record_path, hostrecord.empty(1))
            previous = {"codex-session-relay": "/old/pkg"}
            installs = {"codex-session-relay": {"location": "/mine/pkg"}}

            # Another run has since promoted something else entirely.
            hostrecord.update(record_path, 1, select={"codex-session-relay": "/theirs/pkg"})
            answer = runtime_install._restore_selection(record_path, 1, previous, installs)
            after = hostrecord.load(record_path, 1).value["selected"]

        self.assertEqual(after, {"codex-session-relay": "/theirs/pkg"},
                         "a rollback on top of somebody else's success is worse than the"
                         " failure being rolled back")
        self.assertEqual(answer["restored"], [])
        self.assertEqual(answer["movedOnByAnotherRun"], ["codex-session-relay"])

    def test_a_rollback_does_undo_its_own_write(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            record_path = Path(temporary) / "record.json"
            hostrecord.save(record_path, hostrecord.empty(1))
            previous = {"codex-session-relay": "/old/pkg"}
            installs = {"codex-session-relay": {"location": "/mine/pkg"}}

            hostrecord.update(record_path, 1, select={"codex-session-relay": "/mine/pkg"})
            answer = runtime_install._restore_selection(record_path, 1, previous, installs)
            after = hostrecord.load(record_path, 1).value["selected"]

        self.assertEqual(after, {"codex-session-relay": "/old/pkg"})
        self.assertEqual(answer["restored"], ["codex-session-relay"])



class InheritedRegistrationTests(unittest.TestCase):
    """The installed base this change exists to unpin must be able to take it.

    A host installed before the pointer existed registers a concrete entry point. Compared with
    the pointer that reads as a conflict, and a conflict refuses the update AND deletes the
    candidate -- so the very hosts whose pinned registration the pointer fixes could never
    receive the fix.
    """

    def test_a_predecessors_own_registration_does_not_refuse_the_update(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            bridge = component_of_for_test(host.data, "codex-thread-bridge")
            legacy = str(host.previous / "bin" / bridge["consoleScript"])
            # Exactly what a host installed by the parent version carries.
            host.config.write_text(
                '[mcp_servers.codex-thread-bridge]' + chr(10)
                + 'command = "' + legacy + '"' + chr(10), encoding="utf-8")

            code, payload = UpdateRecoveryTests()._run(host)
            survived = host.previous.is_dir()
            config_after = host.config.read_bytes()

        self.assertEqual(code, 0, json.dumps(payload)[:1200])
        self.assertTrue(payload["promoted"], "the installed base can take the update")
        self.assertTrue(survived, "and the runtime its configuration still names is preserved")
        self.assertIn(b"command", config_after,
                      "the configuration is not rewritten by an install")

    def test_a_registration_nobody_recorded_is_still_a_conflict(self):
        import runtime_install

        data = definition.load()
        record = hostrecord.empty(1)
        hostrecord.put_install(record, "codex-thread-bridge",
                               {"location": "/ours/pkg", "environment": "/ours",
                                "entryPoint": "/ours/bin/codex-thread-bridge"})
        conflict = {"outcome": codexconfig.CONFLICT, "detail": "differs",
                    "registered": {"command": "/somebody/else/bin/codex-thread-bridge"}}
        self.assertIsNone(runtime_install._inherited_registration(conflict, record, data),
                          "a path that merely looks like ours proves nothing")

        ours = {"outcome": codexconfig.CONFLICT, "detail": "differs",
                "registered": {"command": "/ours/bin/codex-thread-bridge"}}
        found = runtime_install._inherited_registration(ours, record, data)
        self.assertIsNotNone(found)
        self.assertIn("separate operation", found["detail"],
                      "recognising an inherited registration is not migrating it")


class SettledPointerTests(unittest.TestCase):
    def test_already_installed_is_not_reported_when_the_pointer_does_not_reach_it(self):
        """The claim and the selection say it is installed. Neither says anything about the
        path a host actually reaches it through."""
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            code, _ = UpdateRecoveryTests()._run(host)
            self.assertEqual(code, 0, "the first run installs")

            # The pointer is repointed away from the installed runtime.
            pointer.place(host.pointer_path, host.previous)
            again, payload = UpdateRecoveryTests()._run(host)

        self.assertEqual(again, 1)
        self.assertFalse(payload["alreadyInstalled"],
                         "reporting an installation a host cannot reach is a success claim"
                         " about something nobody read")
        self.assertIn("does not name it", payload["refused"])


class PointerOwnershipTests(unittest.TestCase):
    def test_a_link_this_command_never_recorded_is_not_replaced(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            # A link the user made, which this record has never recorded placing.
            record = hostrecord.load(host.record_path, host.data["definitionVersion"]).value
            record.pop("pointer", None)
            hostrecord.save(host.record_path, record)

            code, payload = UpdateRecoveryTests()._run(host)
            still_theirs = pointer.read(host.pointer_path)["target"]

        self.assertEqual(code, 1)
        self.assertEqual(payload["failedStep"], "establish the pointer is this command's")
        self.assertEqual(still_theirs, str(host.previous),
                         "renaming over a link succeeds whoever made it, so ownership is"
                         " established from the record rather than from the shape of the path")



# =========================================================================================
# CRW-49 final round - a judgment inside the promotion reads its own state
# =========================================================================================


def _promotion_section(tree):
    """The with-block guarding the promotion, found by the lock it takes rather than by line."""
    for node in ast.walk(tree):
        if not (isinstance(node, ast.FunctionDef) and node.name == "cmd_install"):
            continue
        for inner in ast.walk(node):
            if not isinstance(inner, ast.With):
                continue
            for item in inner.items:
                call = item.context_expr
                if not isinstance(call, ast.Call):
                    continue
                called = call.func
                name = (called.attr if isinstance(called, ast.Attribute)
                        else getattr(called, "id", None))
                if name == "Exclusive" and call.args:
                    first = call.args[0]
                    if isinstance(first, ast.Name) and first.id == "record_path":
                        return inner
    return None


def _stale_promotion_reads(tree, declared):
    """Declared members READ in the promotion section without being READ FRESH there first.

    This is the check the previous three findings needed and did not have. Each of them was a
    value assigned outside the critical section and decided on inside it, and each arrived as
    its own review round because nothing looked at the class.
    """
    section = _promotion_section(tree)
    if section is None:
        return ["the promotion critical section was not found"]
    stored, loaded = {}, {}
    for node in ast.walk(section):
        if not isinstance(node, ast.Name):
            continue
        where = stored if isinstance(node.ctx, ast.Store) else loaded
        where[node.id] = min(where.get(node.id, node.lineno), node.lineno)
    findings = []
    for member in declared:
        if member not in stored and member not in loaded:
            findings.append(member + ": declared fresh and used nowhere in the promotion")
        elif member not in stored:
            findings.append(member + ": decided on inside the promotion, read outside it")
        elif member in loaded and loaded[member] < stored[member]:
            findings.append(member + ": read at line " + str(loaded[member])
                            + " before it is read fresh at line " + str(stored[member]))
    return findings


class PromotionFreshnessTests(unittest.TestCase):
    """One class, three instances, one boundary.

    The swap gate ran against the record loaded before the build; the rollback baseline was
    captured before the build; the classification read a pointer at the destination rather than
    the one the record names and the swap replaces. Fixing those one at a time would have left
    the fourth, so the rule is declared and checked instead.
    """

    def test_every_value_the_promotion_decides_on_is_read_inside_it(self):
        import runtime_install

        tree = ast.parse(RUNTIME.read_text(encoding="utf-8"))
        self.assertEqual(
            _stale_promotion_reads(tree, runtime_install.PROMOTION_FRESH), [],
            "a value read before the lock is one another run may have replaced, so a judgment"
            " made on it is about a state that no longer exists")

    def test_the_declared_set_is_not_empty_and_names_the_three_that_reopened(self):
        import runtime_install

        declared = set(runtime_install.PROMOTION_FRESH)
        self.assertTrue({"gate", "previous_selection", "pointer_read"} <= declared,
                        "the set has to name the three findings that were one defect")

    def test_the_scan_sees_a_value_read_outside_and_judged_inside(self):
        """Guards the checker: without this, an empty finding list proves nothing."""
        source = (
            "def cmd_install(args):\n"
            "    gate = _swap_gate(data, record)\n"
            "    with hostrecord.Exclusive(record_path):\n"
            "        fresh = hostrecord.load(record_path, version)\n"
            "        previous_selection = dict(fresh.value.get('selected') or {})\n"
            "        before = pointer.read(pointer_path)\n"
            "        pointer_read = pointer_state(pointer_path, fresh.value, data)\n"
            "        if gate['verdict'] != ALLOWED:\n"
            "            return 1\n")
        findings = _stale_promotion_reads(ast.parse(source), ("gate", "fresh"))
        self.assertEqual(len(findings), 1, findings)
        self.assertIn("gate", findings[0])
        self.assertIn("read outside it", findings[0])

    def test_the_scan_sees_a_member_read_before_it_is_refreshed(self):
        source = (
            "def cmd_install(args):\n"
            "    with hostrecord.Exclusive(record_path):\n"
            "        if gate['verdict'] != ALLOWED:\n"
            "            return 1\n"
            "        gate = _swap_gate(data, fresh.value)\n")
        findings = _stale_promotion_reads(ast.parse(source), ("gate",))
        self.assertEqual(len(findings), 1, findings)
        self.assertIn("before it is read fresh", findings[0])


class OverlappingUpdateTests(unittest.TestCase):
    """Two updates overlapping, exercised rather than asserted from source text.

    The interleaving is committed at the exact seam it would occur at: while this run is
    measuring its candidate, a competing run promotes something else. What follows has to be
    decided on that, not on what was true when this run started.
    """

    def _competitor(self, host, name):
        """A rival promotion: its own environment, its own relay interpreter, selected."""
        rival = host.destination / ("env-" + name)
        (rival / "bin").mkdir(parents=True)
        site = rival / "site"
        site.mkdir()
        interpreter = str(rival / "bin" / "python")

        def promote():
            record = hostrecord.load(host.record_path,
                                     host.data["definitionVersion"]).value
            for component in host.data["components"]:
                hostrecord.put_install(record, component["component"], {
                    "location": str(site / component["module"]),
                    "environment": str(rival),
                    "entryPoint": str(rival / "bin" / component["consoleScript"]),
                    "interpreterPath": interpreter})
            record["selected"] = {c["component"]: str(site / c["module"])
                                  for c in host.data["components"]}
            hostrecord.save(host.record_path, record)

        return promote, interpreter, {c["component"]: str(site / c["module"])
                                      for c in host.data["components"]}

    def test_the_gate_is_asked_about_the_runtime_that_is_selected_now(self):
        """A rival promotes while this run builds. The daemon and the store belong to ITS
        runtime by the time anything moves, so that is the one the gate has to ask."""
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            promote, rival_interpreter, _selected = self._competitor(host, "rival")
            asked = []
            code, payload = UpdateRecoveryTests()._run(host, interpose=promote, probes=asked)

        self.assertEqual(code, 0, json.dumps(payload)[:900])
        self.assertTrue(asked, "the store probe was never run")
        self.assertEqual(asked[-1], rival_interpreter,
                         "the gate asked the runtime this run saw at the start instead of the"
                         " one selected by the time it promoted")

    def test_the_rollback_restores_the_selection_the_promotion_replaced(self):
        """Not the one that was there before the build. If a rival promoted in between, putting
        back the pre-rival selection leaves the record and the pointer describing different
        runtimes."""
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            promote, _interpreter, rival_selection = self._competitor(host, "rival")
            started_with = host.snapshot()["selected"]
            code, payload = UpdateRecoveryTests()._run(
                host, breaking="replace the owned pointer", interpose=promote)
            after = host.snapshot()["selected"]

        self.assertEqual(code, 1)
        self.assertNotEqual(rival_selection, started_with, "the rival really did move it")
        self.assertEqual(after, rival_selection,
                         "the baseline is the selection read inside the promotion lock, so the"
                         " rollback puts back what this promotion replaced")
        self.assertEqual(payload["pointer"]["restored"]["restored"],
                         sorted(rival_selection),
                         "and the result names what it put back")

    def test_the_run_reports_that_the_selection_moved_while_it_was_building(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            promote, _i, _s = self._competitor(host, "rival")
            code, payload = UpdateRecoveryTests()._run(host, interpose=promote)
            steps = {s["step"]: s for s in payload["steps"]}

        self.assertEqual(code, 0)
        self.assertTrue(steps["read whether it is safe to swap"]["selectionMovedWhileBuilding"],
                        "a reader should be able to see that this was not the quiet case")


class InheritedRegistrationScopeTests(unittest.TestCase):
    def test_a_relay_entry_point_registered_as_the_bridge_stays_a_conflict(self):
        """The exception exists for the BRIDGE's pre-pointer registration. A relay path that
        happens to sit in the same record is not evidence that registering it as the bridge
        server is this command's own doing."""
        import runtime_install

        data = definition.load()
        record = hostrecord.empty(1)
        hostrecord.put_install(record, runtime_install.RELAY,
                               {"location": "/ours/relay", "environment": "/ours",
                                "entryPoint": "/ours/bin/codex-session-relay"})
        hostrecord.put_install(record, runtime_install.MCP_NAME,
                               {"location": "/ours/bridge", "environment": "/ours",
                                "entryPoint": "/ours/bin/codex-thread-bridge"})

        relay_registered = {"outcome": codexconfig.CONFLICT, "detail": "differs",
                            "registered": {"command": "/ours/bin/codex-session-relay"}}
        self.assertIsNone(
            runtime_install._inherited_registration(relay_registered, record, data),
            "Codex would go on launching the relay CLI as the bridge server")

        bridge_registered = {"outcome": codexconfig.CONFLICT, "detail": "differs",
                             "registered": {"command": "/ours/bin/codex-thread-bridge"}}
        self.assertIsNotNone(
            runtime_install._inherited_registration(bridge_registered, record, data),
            "the bridge's own earlier registration still qualifies")


class PointerPathShapeTests(unittest.TestCase):
    """CRW-13's property, applied to a field this change newly consumes as a path."""

    def test_a_pointer_path_that_is_not_a_string_is_unreadable_not_a_crash(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "record.json"
            path.write_text(json.dumps({"components": {}, "pointer": {"path": ["invalid"]}}),
                            encoding="utf-8")
            answer = hostrecord.load(path, 1)
        self.assertFalse(answer.usable)
        self.assertEqual(answer.state, reading.UNREADABLE)
        self.assertIn("pointer.path is a string", str(answer.detail))

    def test_install_refuses_such_a_record_rather_than_reporting_its_own_defect(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "dest"
            record_path = Path(temporary) / "record.json"
            record_path.write_text(
                json.dumps({"components": {}, "pointer": {"path": ["invalid"]}}),
                encoding="utf-8")
            emitted = []
            args = argparse.Namespace(dest=str(destination), apply=True,
                                      record=str(record_path), python=sys.executable,
                                      socket=None, state=None, issue="CRW-49",
                                      codex_home=temporary)
            with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_install(args)

        self.assertEqual(code, 1)
        payload = emitted[-1]
        self.assertIsNone(payload.get("internalError"),
                          "a record that cannot be read is not a defect in this command")
        self.assertEqual(payload["reading"]["state"], reading.UNREADABLE)
        self.assertIn("pointer.path is a string", payload["refused"])



# =========================================================================================
# CRW-49 regression round - four defects this PR introduced
# =========================================================================================


class CleanHostFirstInstallTests(unittest.TestCase):
    """(A) A store that is not there is not a store nobody could read.

    The relay reports contents unavailable for both, and the in-flight cell had no way to say
    'established absent' -- so a first install on a clean host could never promote, while the
    schema cell, which does look at the path, answered NO_STORE about the very same store.
    """

    def test_an_absent_store_means_no_attempt_can_be_open(self):
        doctor = {"ok": True, "command": ["doctor"],
                  "payload": {"contents": {"available": False, "openAttempts": None,
                                           "detail": "the database is not readable from this"
                                                     " process"}}}
        absent = {"readable": True, "present": False, "dbPath": "/nowhere/relay.sqlite3",
                  "command": ["store-presence"]}
        cell = swapgate.inflight_cell(doctor, absent)
        self.assertTrue(cell["readable"], cell["detail"])
        self.assertEqual(cell["answer"], 0)
        self.assertFalse(swapgate.blocking("inFlight", cell))

    def test_a_store_that_exists_and_cannot_be_read_is_still_unreadable(self):
        """The half that must not regress: mixing the readings would have answered zero here."""
        doctor = {"ok": True, "command": ["doctor"],
                  "payload": {"contents": {"available": False, "openAttempts": None}}}
        present = {"readable": True, "present": True, "dbPath": "/d/relay.sqlite3"}
        cell = swapgate.inflight_cell(doctor, present)
        self.assertFalse(cell["readable"])
        self.assertIsNone(swapgate.blocking("inFlight", cell))

    def test_a_presence_reading_that_failed_is_not_absence(self):
        doctor = {"ok": True, "command": ["doctor"],
                  "payload": {"contents": {"available": False, "openAttempts": None}}}
        unknown = {"readable": False, "present": None, "detail": "permission denied"}
        self.assertFalse(swapgate.inflight_cell(doctor, unknown)["readable"])

    def test_two_readings_of_the_same_question_disagreeing_is_not_an_answer(self):
        doctor = {"ok": True, "command": ["doctor"],
                  "payload": {"contents": {"available": True, "openAttempts": 0}}}
        absent = {"readable": True, "present": False, "dbPath": "/nowhere/relay.sqlite3"}
        self.assertFalse(swapgate.inflight_cell(doctor, absent)["readable"])

    def test_a_first_install_on_a_clean_host_promotes(self):
        """End to end, which is the criterion: the installer has to be able to install."""
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            code, payload = UpdateRecoveryTests()._run(host, clean_store=True)
            reached = pointer.read(host.pointer_path)["target"]

        self.assertEqual(code, 0, json.dumps(payload)[:1200])
        self.assertTrue(payload["promoted"])
        self.assertEqual(payload["swapGate"]["verdict"], swapgate.ALLOWED)
        self.assertEqual(payload["swapGate"]["cells"]["storeTables"]["answer"],
                         swapgate.NO_STORE)
        self.assertEqual(reached, str(host.candidate))

    def test_the_presence_probe_asks_the_relay_and_opens_nothing(self):
        import runtime_install

        source = ast.unparse(_function_named(RUNTIME, "store_presence"))
        self.assertIn("str(interpreter)", source)
        self.assertNotIn("sys.executable", source)
        self.assertIn("store_presence", runtime_install.PREFLIGHT_PROBES)
        self.assertNotIn("read_only_rows", runtime_install._STORE_PRESENCE_PROGRAM,
                         "presence is settled by looking at the path, not by opening it")
        self.assertIn("os.lstat", runtime_install._STORE_PRESENCE_PROGRAM)


class PromotionExclusionTests(unittest.TestCase):
    """(B) One set, not three defects.

    hostrecord.Locked excluded by FILENAME, on a path a caller derived, with a 300-second mtime
    rule deciding validity. So two installs with different destinations locked different files
    and never met, and a promotion that outstayed the window had its live lock unlinked by a
    waiter. The promotion now excludes by advisory lock on one host-wide path.
    """

    def test_the_lock_is_the_same_one_whatever_destination_is_used(self):
        with tempfile.TemporaryDirectory() as temporary:
            record = Path(temporary) / "host-record.json"
            first = hostrecord.Exclusive(record)
            second = hostrecord.Exclusive(record)
        self.assertEqual(first.path, second.path,
                         "its identity is the host record, not a path a caller derived")

    def test_a_second_promotion_cannot_start_while_one_is_held(self):
        with tempfile.TemporaryDirectory() as temporary:
            record = Path(temporary) / "host-record.json"
            held = hostrecord.Exclusive(record).__enter__()
            try:
                with self.assertRaises(TimeoutError):
                    hostrecord.Exclusive(record, timeout=0.2).__enter__()
            finally:
                held.__exit__()
            hostrecord.Exclusive(record, timeout=0.2).__enter__().__exit__()

    def test_a_long_promotion_keeps_its_lock(self):
        """The regression itself: age decided validity, so a waiter could unlink a live lock."""
        with tempfile.TemporaryDirectory() as temporary:
            record = Path(temporary) / "host-record.json"
            held = hostrecord.Exclusive(record).__enter__()
            try:
                old = time.time() - (hostrecord.STALE_LOCK_SECONDS * 4)
                os.utime(held.path, (old, old))
                with self.assertRaises(TimeoutError):
                    hostrecord.Exclusive(record, timeout=0.2).__enter__()
                self.assertTrue(held.path.exists(),
                                "a waiter must not unlink a lock somebody still holds")
            finally:
                held.__exit__()

    def test_releasing_leaves_the_lock_file_behind(self):
        """Unlinking it is what hands a second holder a lock nobody else is on."""
        with tempfile.TemporaryDirectory() as temporary:
            record = Path(temporary) / "host-record.json"
            lock = hostrecord.Exclusive(record)
            lock.__enter__()
            lock.__exit__()
            self.assertTrue(lock.path.exists())

    def test_the_mechanism_the_promotion_left_behind_had_both_defects(self):
        """Characterises what was wrong, so the reason this changed cannot quietly rot.

        hostrecord.Locked still exists and is still right for the short staging decision, where
        the span is bounded and the path is the thing being decided about. It was wrong for the
        promotion, and these are the two reasons, measured rather than described.
        """
        with tempfile.TemporaryDirectory() as temporary:
            # Identity: a path the caller derives. Two destinations, two locks, no exclusion.
            first = hostrecord.Locked(Path(temporary) / "a" / "current")
            second = hostrecord.Locked(Path(temporary) / "b" / "current")
            self.assertNotEqual(first.path, second.path)

            # Validity: age rather than liveness. A waiter takes a lock its owner still holds.
            target = Path(temporary) / "held"
            holder = hostrecord.Locked(target).__enter__()
            try:
                old = time.time() - (hostrecord.STALE_LOCK_SECONDS * 4)
                os.utime(holder.path, (old, old))
                stolen = hostrecord.Locked(target, timeout=0.2).__enter__()
                stolen.__exit__()
            finally:
                holder.__exit__()

        source = RUNTIME.read_text(encoding="utf-8")
        promotion = source[source.index("        landed = None"):
                           source.index("                # Read back rather than trusted.")]
        self.assertNotIn("Locked(", promotion,
                         "the promotion excludes by advisory lock on one host-wide path now,"
                         " not by filename on a path a caller derived")

    def test_two_installs_with_different_destinations_serialize(self):
        """The reported defect, end to end: they derived different pointer paths and never met."""
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            rival = hostrecord.Exclusive(host.record_path).__enter__()
            try:
                with mock.patch.object(runtime_install.hostrecord,
                                       "PROMOTION_TIMEOUT_SECONDS", 0.2):
                    code, payload = UpdateRecoveryTests()._run(host)
            finally:
                rival.__exit__()

        self.assertEqual(code, 1)
        self.assertEqual(payload["failedStep"], "take the promotion lock",
                         "a promotion held elsewhere on this host stops this one whatever"
                         " destination it was invoked with")


class StagingLeftoverTests(unittest.TestCase):
    """(C) The permanent refusal this PR exists to remove, reintroduced by a narrower door."""

    def test_a_leftover_lock_file_does_not_block_the_destination(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            # What a run whose claim write failed leaves: the directory and the lock, no claim.
            host.candidate.mkdir(parents=True)
            staging.lock_path(host.candidate).write_text("", encoding="utf-8")

            code, payload = UpdateRecoveryTests()._run(host)
            steps = {s["step"]: s for s in payload.get("steps", [])}

        self.assertEqual(code, 0, json.dumps(payload)[:1200])
        self.assertIn("take over an empty staging directory", steps,
                      "the leftover directory has to be taken over, not refused")
        self.assertIn(staging.LOCK_NAME,
                      steps["take over an empty staging directory"]["clearedOwnFiles"],
                      "and the lock file rmdir would have tripped over has to be cleared")

    def test_clearing_own_files_touches_nothing_else(self):
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            environment.mkdir()
            staging.lock_path(environment).write_text("", encoding="utf-8")
            staging.claim_path(environment).write_text("{}", encoding="utf-8")
            (environment / "somebody-elses-file").write_text("keep", encoding="utf-8")

            removed = staging.clear_own(environment)
            left = sorted(p.name for p in environment.iterdir())

        self.assertEqual(sorted(removed), sorted([staging.CLAIM_NAME, staging.LOCK_NAME]))
        self.assertEqual(left, ["somebody-elses-file"])

    def test_a_claim_that_could_not_be_written_releases_the_directory(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            with mock.patch.object(runtime_install.staging, "write_claim",
                                   side_effect=OSError("no space left on device")):
                code, payload = UpdateRecoveryTests()._run(host)
            leftover = sorted(p.name for p in host.destination.iterdir())

        self.assertEqual(code, 1)
        self.assertEqual(payload["failedStep"], "claim the staging directory")
        self.assertTrue(payload["retriable"])
        self.assertNotIn(host.candidate.name, leftover,
                         "the directory it created goes with the failure, or the next run meets"
                         " a lock file rmdir will not remove")

    def test_a_lock_held_with_no_claim_is_somebody_building(self):
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            environment.mkdir()
            held = staging.Held(environment).take()
            try:
                decision, why = staging.decide(
                    staging.read_claim(environment),
                    staging.owner_liveness(environment)[0],
                    occupied=staging.directory_occupied(environment)[0],
                    protected=False, selected=False)
            finally:
                held.__exit__()
        self.assertEqual(decision, staging.OCCUPIED, why)
        self.assertNotIn(decision, staging.REMOVES)


class ResumeConflictTests(unittest.TestCase):
    """(D) The resume path inherited the selection check and not the conflict check."""

    def _interrupted(self, host):
        host.candidate.mkdir(parents=True)
        (host.candidate / "site").mkdir()
        staging.write_claim(host.candidate, staging.STAGING, issue="CRW-49", run="killed")
        hostrecord.update(host.record_path, host.data["definitionVersion"],
                          select={c["component"]: str(host.candidate / "site" / c["module"])
                                  for c in host.data["components"]})

    def test_a_link_repointed_during_the_interruption_is_not_overwritten(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            self._interrupted(host)
            stranger = Path(temporary) / "somewhere-else"
            stranger.mkdir()
            pointer.place(host.pointer_path, stranger)

            code, payload = UpdateRecoveryTests()._run(host)
            still = pointer.read(host.pointer_path)["target"]

        self.assertEqual(code, 1)
        self.assertEqual(payload["stagingDecision"], staging.RESUME)
        self.assertIn("does not account for", payload["refused"])
        self.assertEqual(still, str(stranger),
                         "a pointer this record cannot account for is left exactly as it is")

    def test_the_interrupted_promotion_it_exists_for_is_still_finished(self):
        """The narrow rule has to stay narrow: a resume necessarily finds the pointer
        disagreeing with the selection, and that is the case it repairs."""
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            self._interrupted(host)
            code, payload = UpdateRecoveryTests()._run(host)
            reached = pointer.read(host.pointer_path)["target"]

        self.assertEqual(code, 0, json.dumps(payload)[:900])
        self.assertTrue(payload["resumed"])
        self.assertEqual(reached, str(host.candidate))



class AbsentPointerRollbackTests(unittest.TestCase):
    """The rollback could put a target back and could not put ABSENCE back.

    A first or legacy install has no pointer, so place() creates one. A failed read-back then
    left that link naming a candidate the selection had just been taken away from -- and the
    candidate was kept precisely BECAUSE the pointer named it, so the staging could never be
    reclaimed. The permanent refusal this command exists to remove, reached from the other side,
    on the path the previous round had just made work.
    """

    def _first_install(self, host):
        """A host with a runtime selected and no pointer: what a pre-pointer install looks like."""
        record = hostrecord.load(host.record_path, host.data["definitionVersion"]).value
        record.pop("pointer", None)
        hostrecord.save(host.record_path, record)
        host.pointer_path.unlink()
        self.assertEqual(pointer.read(host.pointer_path)["state"], pointer.NO_POINTER)

    def test_a_first_install_that_fails_at_the_read_back_leaves_no_pointer(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            self._first_install(host)
            code, payload = UpdateRecoveryTests()._run(
                host, breaking="read the owned pointer back")
            state = pointer.read(host.pointer_path)["state"]
            leftover = sorted(p.name for p in host.destination.iterdir())

        self.assertEqual(code, 1)
        self.assertEqual(state, pointer.NO_POINTER,
                         "the pointer this run placed goes with the run that placed it")
        self.assertEqual(payload["pointer"]["pointerRestored"]["restoredTo"], "absent")
        self.assertTrue(payload["pointer"]["pointerRestored"]["verified"])
        self.assertTrue(payload["retriable"], json.dumps(payload)[:900])
        self.assertNotIn(host.candidate.name, leftover,
                         "and the destination is retriable, not held by a link nothing selects")

    def test_a_first_install_that_fails_at_the_swap_leaves_no_pointer(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            self._first_install(host)
            code, payload = UpdateRecoveryTests()._run(
                host, breaking="replace the owned pointer")
            state = pointer.read(host.pointer_path)["state"]

        self.assertEqual(code, 1)
        self.assertEqual(state, pointer.NO_POINTER)
        self.assertTrue(payload["retriable"])

    def test_an_update_still_puts_the_previous_target_back(self):
        """The half that must not regress while the other half is added."""
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            code, payload = UpdateRecoveryTests()._run(
                host, breaking="read the owned pointer back")
            target = pointer.read(host.pointer_path)["target"]

        self.assertEqual(code, 1)
        self.assertEqual(target, str(host.previous))
        self.assertEqual(payload["pointer"]["pointerRestored"]["restoredTo"],
                         str(host.previous))

    def test_a_restoration_that_cannot_be_verified_keeps_the_candidate(self):
        """Reporting a residual is the honest outcome; claiming a completed rollback is not."""
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            self._first_install(host)
            with mock.patch.object(runtime_install.pointer, "remove",
                                   return_value=(False, "the pointer could not be removed")):
                code, payload = UpdateRecoveryTests()._run(
                    host, breaking="read the owned pointer back")

        self.assertEqual(code, 1)
        self.assertFalse(payload["pointer"]["pointerRestored"]["verified"])
        self.assertEqual(payload["pointer"]["pointerRestored"]["restoredTo"], None)
        self.assertIn(str(host.pointer_path), payload["residualPaths"])
        self.assertFalse(payload["retriable"],
                         "a candidate the pointer may still name is kept, not reported gone")

    def test_removing_a_pointer_is_guarded_the_way_placing_one_is(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "mine").mkdir()
            (root / "theirs").mkdir()
            path = pointer.pointer_path(root)

            self.assertEqual(pointer.remove(path, root / "mine")[0], True,
                             "absence is already the state this restores to")
            pointer.place(path, root / "theirs")
            removed, detail = pointer.remove(path, root / "mine")
            self.assertFalse(removed, detail)
            self.assertEqual(pointer.read(path)["target"], str(root / "theirs"),
                             "a link that has stopped naming this run's environment is"
                             " somebody else's to remove")

            real = root / "real"
            real.mkdir()
            self.assertFalse(pointer.remove(real, root / "mine")[0])
            self.assertTrue(real.is_dir(), "a real directory is never unlinked")


# =========================================================================================
# CRW-49 class-closing round - the answers that could not say "there was nothing"
# =========================================================================================

# The parameter names by which the three instances received the state they found, written here
# as the observation the derivation is made FROM. The source declares the same set and the
# agreement between the two is checked below, so neither side can quietly shrink: a check that
# read only the declaration could be disabled by editing it.
PRIOR_STATE_SEEN = ("previous", "before", "presence")


def _prior_state_functions(trees, arguments):
    """Every function handed the state it FOUND, as module.name, derived from the source.

    Derived rather than listed, because the list is precisely what was missing. Three review
    rounds each contributed one instance of the same thing -- an answer set with no value for
    "there was nothing there" -- and each was patched on its own because nobody had drawn the
    set they belonged to.
    """
    found = {}
    for stem, tree in trees.items():
        for node in ast.walk(tree):
            if not isinstance(node, ast.FunctionDef):
                continue
            named = [argument.arg for argument in node.args.args + node.args.kwonlyargs]
            taken = sorted(set(named) & set(arguments))
            if taken:
                found[stem + "." + node.name] = taken
    return found


def _module_offers(tree):
    """What a module offers by name: its bindings, its functions, and its parameters.

    A delta the single writer accepts is an operation the module offers exactly as much as a
    function is, and it is named in the same file. Both count as the operation existing, which
    is the question this asks; whether it is USED is a different question, asked below.
    """
    offered = set()
    for node in ast.walk(tree):
        if isinstance(node, (ast.FunctionDef, ast.ClassDef)):
            offered.add(node.name)
        if isinstance(node, ast.FunctionDef):
            offered.update(argument.arg for argument
                           in node.args.args + node.args.kwonlyargs)
        if isinstance(node, ast.Assign):
            offered.update(target.id for target in node.targets
                           if isinstance(target, ast.Name))
    return offered


def _names_used_in(tree, function):
    """Every identifier a function mentions: attributes, names and keyword arguments.

    Keywords count because a delta is invoked by naming it. None when there is no such
    function, which is itself a finding rather than an empty answer.
    """
    for node in ast.walk(tree):
        if not (isinstance(node, ast.FunctionDef) and node.name == function):
            continue
        used = set()
        for inner in ast.walk(node):
            if isinstance(inner, ast.Attribute):
                used.add(inner.attr)
            elif isinstance(inner, ast.Name):
                used.add(inner.id)
            elif isinstance(inner, ast.keyword) and inner.arg:
                used.add(inner.arg)
        return used
    return None


def _absence_findings(trees, declared, arguments):
    """Places that answer about a state they found and cannot say it was not there.

    Two directions, because either one alone passes vacuously. A place derived from the source
    and not declared is a place where absence has no value yet. A declared operation that does
    not exist, or that exists and is never called where the answer is given, is a capability
    nobody uses -- the same silence as no capability at all, which is how three rounds of this
    class reached review.
    """
    findings = []
    for place, taken in sorted(_prior_state_functions(trees, arguments).items()):
        if place not in declared:
            findings.append(place + ": receives " + ", ".join(taken) + " and declares no way"
                            " to answer that there was nothing there")
    for place, operations in sorted(declared.items()):
        stem, _, function = place.rpartition(".")
        if stem not in trees:
            findings.append(place + ": names no module this command owns")
            continue
        used = _names_used_in(trees[stem], function)
        if used is None:
            findings.append(place + ": declared and no such function exists")
            continue
        for operation in operations:
            owner, _, attribute = operation.rpartition(".")
            if owner not in trees:
                findings.append(operation + ": names no module this command owns")
            elif attribute not in _module_offers(trees[owner]):
                findings.append(operation + ": declared as an absence answer and does not"
                                " exist")
            elif attribute not in used:
                findings.append(operation + ": declared for " + place + " and never used"
                                " there")
    return findings


class AbsenceAnswerTests(unittest.TestCase):
    """The class three rounds kept reopening, stated and checked at its own layer.

    Each round produced one instance and each was read as its own defect: the in-flight cell
    could not say "established absent" and a clean host could never promote; the pointer
    rollback could not restore absence and a failed first install left a link nothing selected;
    the selection rollback could not remove a selection that had no previous value and the same
    install left its own candidate selected. The check derives the places from the source, so
    the next one is a failure here rather than a fourth round.
    """

    def test_every_place_handed_the_state_it_found_can_say_there_was_nothing(self):
        import runtime_install

        # Read with a default rather than by attribute, so a source that declares nothing at
        # all fails with the PLACES it leaves unanswered instead of with a missing name. The
        # set being undrawn is the defect, and the failure has to say which places it hit.
        declared = getattr(runtime_install, "ABSENCE_ANSWERS", {})
        self.assertEqual(
            _absence_findings(_runtime_trees(), declared, PRIOR_STATE_SEEN), [],
            "a place that receives the state it found is answering about something that may"
            " not have been there, and its answer set is incomplete until it can say so")

    def test_the_source_declares_the_same_places_the_derivation_finds(self):
        import runtime_install

        derived = _prior_state_functions(_runtime_trees(), PRIOR_STATE_SEEN)
        self.assertEqual(set(derived), {"runtime_install._restore_pointer",
                                        "runtime_install._restore_selection",
                                        "swapgate.inflight_cell",
                                        "completion.config_outcome"})
        self.assertEqual(tuple(runtime_install.PRIOR_STATE_ARGUMENTS), PRIOR_STATE_SEEN,
                         "the source and this check derive from the same observation")
        self.assertEqual(set(derived), set(runtime_install.ABSENCE_ANSWERS),
                         "a declaration for a place that no longer receives prior state is a"
                         " dead entry, and one missing is an unanswered place")

    def test_the_scan_sees_a_place_with_no_declared_absence_answer(self):
        """Guards the derivation: without this an empty finding list proves nothing."""
        trees = {"module": ast.parse("def restore(previous):\n    return previous\n")}
        findings = _absence_findings(trees, {}, ("previous",))
        self.assertEqual(len(findings), 1, findings)
        self.assertIn("nothing there", findings[0])

    def test_the_scan_sees_a_declared_answer_that_does_not_exist(self):
        trees = {"module": ast.parse("def restore(previous):\n    return previous\n")}
        findings = _absence_findings(trees, {"module.restore": ("module.vanish",)},
                                     ("previous",))
        self.assertEqual(len(findings), 1, findings)
        self.assertIn("does not exist", findings[0])

    def test_the_scan_sees_a_declared_answer_nothing_calls(self):
        trees = {"module": ast.parse("def vanish(target):\n    return target\n\n\n"
                                     "def restore(previous):\n    return previous\n")}
        findings = _absence_findings(trees, {"module.restore": ("module.vanish",)},
                                     ("previous",))
        self.assertEqual(len(findings), 1, findings)
        self.assertIn("never used there", findings[0])

    def test_the_scan_accepts_an_answer_that_exists_and_is_used(self):
        trees = {"module": ast.parse("def vanish(target):\n    return target\n\n\n"
                                     "def restore(previous):\n    return vanish(previous)\n")}
        self.assertEqual(_absence_findings(trees, {"module.restore": ("module.vanish",)},
                                           ("previous",)), [])

    def test_a_delta_the_single_writer_accepts_counts_as_an_operation(self):
        """The form two of the three answers take: a keyword on the one way to write."""
        trees = {"writer": ast.parse("def update(path, *, drop=None):\n    return drop\n"),
                 "module": ast.parse("import writer\n\n\ndef restore(previous):\n"
                                     "    return writer.update(previous, drop=previous)\n")}
        self.assertEqual(_absence_findings(trees, {"module.restore": ("writer.drop",)},
                                           ("previous",)), [])


class SelectionRollbackTests(unittest.TestCase):
    """The instance still open when the class was drawn.

    A select delta says "this is selected now" and had no way to say "nothing was selected
    before this run". So a first install that failed put the pointer back to absence, left its
    own candidate selected, and was then kept from releasing that candidate BECAUSE it was
    selected. The destination could never be retried: the permanent refusal this command exists
    to remove, reached through the selection instead of through the pointer.
    """

    def _record(self, path, selected=None):
        record = hostrecord.empty(1)
        if selected:
            record["selected"] = dict(selected)
        hostrecord.save(path, record)
        return path

    def test_a_selection_this_run_wrote_with_no_previous_value_is_taken_away(self):
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            path = self._record(Path(temporary) / "record.json",
                                {"bridge": "/dest/env-new/site/bridge"})
            answer = runtime_install._restore_selection(
                path, 1, {}, {"bridge": {"location": "/dest/env-new/site/bridge"}})
            left = (hostrecord.load(path, 1).value or {}).get("selected")

        self.assertEqual(left, {}, "a rollback with no way to unselect leaves its own candidate"
                                   " selected, and a selected candidate is never released")
        self.assertEqual(answer["removed"], ["bridge"])

    def test_an_entry_another_run_has_moved_on_is_left_exactly_as_it_is(self):
        """Compare-and-remove, not remove: undoing a promotion this run never made is worse
        than the failure being rolled back."""
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            path = self._record(Path(temporary) / "record.json",
                                {"bridge": "/dest/env-rival/site/bridge"})
            answer = runtime_install._restore_selection(
                path, 1, {}, {"bridge": {"location": "/dest/env-new/site/bridge"}})
            left = (hostrecord.load(path, 1).value or {}).get("selected")

        self.assertEqual(left, {"bridge": "/dest/env-rival/site/bridge"})
        self.assertEqual(answer["movedOnByAnotherRun"], ["bridge"])

    def test_both_shapes_are_put_back_in_one_write(self):
        """One component had a previous value and one had none. Two answers, one write, because
        two writes would leave a window where half the rollback had landed."""
        import runtime_install

        with tempfile.TemporaryDirectory() as temporary:
            path = self._record(Path(temporary) / "record.json",
                                {"bridge": "/dest/env-new/site/bridge",
                                 "relay": "/dest/env-new/site/relay"})
            answer = runtime_install._restore_selection(
                path, 1, {"bridge": "/dest/env-old/site/bridge"},
                {"bridge": {"location": "/dest/env-new/site/bridge"},
                 "relay": {"location": "/dest/env-new/site/relay"}})
            left = (hostrecord.load(path, 1).value or {}).get("selected")

        self.assertEqual(left, {"bridge": "/dest/env-old/site/bridge"})
        self.assertEqual(answer["restored"], ["bridge"])
        self.assertEqual(answer["removed"], ["relay"])

    def test_a_first_install_that_fails_leaves_nothing_selected_and_a_retriable_destination(self):
        """End to end on a host with nothing installed, which is the case that produced it."""
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            hostrecord.save(host.record_path,
                            hostrecord.empty(host.data["definitionVersion"]))
            host.pointer_path.unlink()

            code, payload = UpdateRecoveryTests()._run(
                host, breaking="read the owned pointer back")
            selected = (hostrecord.load(host.record_path,
                                        host.data["definitionVersion"]).value or {}).get("selected")
            state = pointer.read(host.pointer_path)["state"]
            leftover = sorted(p.name for p in host.destination.iterdir())

        self.assertEqual(code, 1)
        self.assertEqual(selected, {}, "nothing was selected before this run, so nothing is"
                                       " selected after it failed")
        self.assertEqual(state, pointer.NO_POINTER)
        self.assertTrue(payload["retriable"], json.dumps(payload)[:900])
        self.assertNotIn(host.candidate.name, leftover,
                         "a candidate nothing selects and no pointer names is released, so the"
                         " deterministic destination can be retried")


class PointerOwnershipRollbackTests(unittest.TestCase):
    """Putting a link back to absence puts its ownership record back too.

    The record is what makes a link this command's: the promotion refuses to replace one this
    record never recorded placing. Left behind for a path where the link was removed, it arms
    that guard in favour of whatever appears there next, and the guard exists to protect the
    host from exactly that.
    """

    def _first_install(self, host):
        record = hostrecord.load(host.record_path, host.data["definitionVersion"]).value
        record.pop("pointer", None)
        hostrecord.save(host.record_path, record)
        host.pointer_path.unlink()

    def test_the_ownership_record_goes_with_the_link(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            self._first_install(host)
            code, payload = UpdateRecoveryTests()._run(
                host, breaking="read the owned pointer back")
            record = hostrecord.load(host.record_path,
                                     host.data["definitionVersion"]).value or {}

        self.assertEqual(code, 1)
        self.assertIsNone(record.get("pointer"),
                          "the record must not claim a link this run took away")
        self.assertTrue(payload["pointer"]["pointerRestored"]["ownershipDropped"])

    def test_a_link_this_command_did_not_place_is_refused_after_the_rollback(self):
        """The consequence, end to end. The leftover claim made a stranger's link ours."""
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            self._first_install(host)
            UpdateRecoveryTests()._run(host, breaking="read the owned pointer back")

            # Something other than this command puts a link at that path. Its target is a
            # directory the record does account for, so nothing about the TARGET refuses it:
            # the only thing that does is that this record never recorded placing it.
            pointer.place(host.pointer_path, host.previous)
            code, payload = UpdateRecoveryTests()._run(host)
            still = pointer.read(host.pointer_path)["target"]

        self.assertEqual(code, 1, json.dumps(payload)[:900])
        self.assertEqual(payload["failedStep"], "establish the pointer is this command's")
        self.assertEqual(still, str(host.previous),
                         "a link nobody recorded is left exactly as it is")

    def test_the_record_is_left_alone_for_a_path_this_run_did_not_record(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "record.json"
            hostrecord.save(path, hostrecord.empty(1))
            hostrecord.update(path, 1, pointer={"path": "/dest/current"})
            hostrecord.update(path, 1, drop_pointer="/elsewhere/current")
            kept = (hostrecord.load(path, 1).value or {}).get("pointer")
            hostrecord.update(path, 1, drop_pointer="/dest/current")
            gone = (hostrecord.load(path, 1).value or {}).get("pointer")

        self.assertEqual((kept or {}).get("path"), "/dest/current")
        self.assertIsNone(gone)


class LegacyInstallTests(unittest.TestCase):
    """An installation older than claims is not somebody else's directory.

    Claims are newer than the installations they describe, so every install made before them is
    populated and carries nothing saying who made it. Read as foreign it was refused, and the
    environment name is derived from the sources, so the refusal is permanent for that
    combination: there was no installed host this updater could move forward, which makes it
    not an updater.
    """

    def _legacy(self, host):
        """What an installer that never wrote claims leaves at the deterministic path.

        Populated, selected by the host record, and carrying no claim, no staging lock and no
        pointer. The marker file is here so a test can prove nothing was rebuilt.
        """
        (host.candidate / "bin").mkdir(parents=True)
        (host.candidate / "bin" / "python").write_text("#!legacy", encoding="utf-8")
        record = hostrecord.load(host.record_path, host.data["definitionVersion"]).value
        record.pop("pointer", None)
        host.pointer_path.unlink()
        for component in host.data["components"]:
            site = host.candidate / "site" / component["module"]
            site.mkdir(parents=True, exist_ok=True)
            hostrecord.put_install(record, component["component"], {
                "location": str(site),
                "environment": str(host.candidate),
                "entryPoint": str(host.candidate / "bin" / component["consoleScript"]),
                "interpreterPath": str(host.candidate / "bin" / "python")})
        record["selected"] = {c["component"]: str(host.candidate / "site" / c["module"])
                              for c in host.data["components"]}
        hostrecord.save(host.record_path, record)
        return record["selected"]

    def _decide(self, **overrides):
        inputs = {"occupied": True, "protected": True, "selected": True}
        inputs.update(overrides)
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            environment.mkdir()
            (environment / "site").mkdir()
            return staging.decide(staging.read_claim(environment),
                                  staging.owner_liveness(environment)[0], **inputs)

    def test_an_occupied_directory_the_record_selects_is_not_foreign(self):
        decision, why = self._decide()
        self.assertNotEqual(decision, staging.FOREIGN,
                            "the host record positively says this is the runtime it selects")
        self.assertNotIn(decision, staging.REMOVES,
                         "positive ownership is not a licence to delete: " + why)
        self.assertIn(decision, staging.DECISIONS)

    def test_an_occupied_directory_nothing_selects_is_still_foreign(self):
        decision, why = self._decide(selected=False)
        self.assertEqual(decision, staging.FOREIGN, why)

    def test_a_selection_reading_that_failed_does_not_authorise_reuse(self):
        """The substitution this whole module refuses: an unread answer authorises nothing."""
        decision, why = self._decide(selected=None)
        self.assertEqual(decision, staging.FOREIGN, why)
        self.assertNotIn(decision, staging.REMOVES)

    def test_a_legacy_installation_is_adopted_rather_than_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            selected = self._legacy(host)
            before = host.snapshot()

            code, payload = UpdateRecoveryTests()._run(host)
            record = hostrecord.load(host.record_path,
                                     host.data["definitionVersion"]).value or {}
            claim = staging.read_claim(host.candidate)
            after = host.snapshot()
            marker = (host.candidate / "bin" / "python").read_text(encoding="utf-8")
            reached = pointer.read(host.pointer_path)["target"]

        self.assertEqual(code, 0, json.dumps(payload)[:1200])
        self.assertTrue(payload["adopted"])
        self.assertEqual(payload["stagingDecision"], staging.RECORDED)
        self.assertEqual(marker, "#!legacy", "nothing was rebuilt")
        self.assertEqual(record.get("selected"), selected, "and nothing was reselected")
        self.assertEqual((claim.value or {}).get("state"), staging.COMPLETE)
        self.assertEqual(record.get("pointer", {}).get("path"), str(host.pointer_path),
                         "the pointer it placed is recorded, or the next update refuses it")
        self.assertEqual(reached, str(host.candidate))
        self.assertEqual(after["storeRows"], before["storeRows"])
        self.assertEqual(after["config"], before["config"])

    def test_the_update_after_an_adoption_lands(self):
        """The reason the adoption is worth anything: the next combination can be promoted."""
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            self._legacy(host)
            adopted, _ = UpdateRecoveryTests()._run(host)
            # The next combination builds at its own path. Standing in for a source change,
            # which would give a different directory name for the same reason.
            shutil.rmtree(host.candidate)
            code, payload = UpdateRecoveryTests()._run(host)
            reached = pointer.read(host.pointer_path)["target"]

        self.assertEqual(adopted, 0)
        self.assertEqual(code, 0, json.dumps(payload)[:1200])
        self.assertTrue(payload["promoted"])
        self.assertEqual(reached, str(host.candidate))

    def test_repeating_the_adoption_writes_nothing_twice(self):
        """Criterion 5 for this entry: a rerun reports the installation and adds nothing."""
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            self._legacy(host)
            adopted, _ = UpdateRecoveryTests()._run(host)
            claim_before = staging.claim_path(host.candidate).read_bytes()
            config_before = host.config.read_bytes()

            code, payload = UpdateRecoveryTests()._run(host)
            claim_after = staging.claim_path(host.candidate).read_bytes()
            config_after = host.config.read_bytes()
            entries = sorted(p.name for p in host.destination.iterdir())

        self.assertEqual(adopted, 0)
        self.assertEqual(code, 0, json.dumps(payload)[:1200])
        self.assertTrue(payload["alreadyInstalled"])
        self.assertEqual(payload["stagingDecision"], staging.SETTLED)
        self.assertEqual(claim_after, claim_before, "the settled claim is not rewritten")
        self.assertEqual(config_after, config_before, "and the registration is not duplicated")
        self.assertEqual(entries, sorted([host.previous.name, host.candidate.name, "current"]),
                         "no orphan staging directory is left behind")


class RecordedTargetTests(unittest.TestCase):
    """A target that CONTAINS a recorded path is not a recorded runtime.

    The destination root is the parent of every environment under it, so a link repointed at
    the destination read as accounted for and was replaced. The containment helper asks the
    opposite question -- is this path inside that root -- and is right everywhere it is used;
    it was the wrong question here.
    """

    def _record(self, environment):
        record = hostrecord.empty(1)
        for component in definition.load()["components"]:
            hostrecord.put_install(record, component["component"], {
                "location": str(Path(environment) / "site" / component["module"]),
                "environment": str(environment)})
        return record

    def test_an_ancestor_of_a_recorded_environment_is_not_accounted_for(self):
        import runtime_install

        data = definition.load()
        record = self._record("/dest/env-known")
        self.assertFalse(runtime_install._target_is_recorded(record, "/dest", data),
                         "the destination root contains every environment under it")
        self.assertFalse(runtime_install._target_is_recorded(record, "/", data))

    def test_a_recorded_environment_and_location_still_are(self):
        import runtime_install

        data = definition.load()
        record = self._record("/dest/env-known")
        module = data["components"][0]["module"]
        self.assertTrue(runtime_install._target_is_recorded(record, "/dest/env-known", data))
        self.assertTrue(runtime_install._target_is_recorded(
            record, "/dest/env-known/site/" + module, data))
        self.assertFalse(runtime_install._target_is_recorded(
            record, "/dest/env-known-other", data))

    def test_a_resume_does_not_replace_a_link_aimed_at_the_destination_root(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            host.candidate.mkdir(parents=True)
            (host.candidate / "site").mkdir()
            staging.write_claim(host.candidate, staging.STAGING, issue="CRW-49", run="killed")
            hostrecord.update(host.record_path, host.data["definitionVersion"],
                              select={c["component"]: str(host.candidate / "site" / c["module"])
                                      for c in host.data["components"]})
            pointer.place(host.pointer_path, host.destination)

            code, payload = UpdateRecoveryTests()._run(host)
            still = pointer.read(host.pointer_path)["target"]

        self.assertEqual(code, 1, json.dumps(payload)[:900])
        self.assertEqual(payload["stagingDecision"], staging.RESUME)
        self.assertIn("does not account for", payload["refused"])
        self.assertEqual(still, str(host.destination))


class PromotionPointerPathTests(unittest.TestCase):
    """The path the swap replaces was derived before the lock and never refreshed.

    A member of the promotion's declared fresh set that nothing had put in it. Proved by
    overlapping the two runs rather than by reading the source: while this run measures its
    candidate, another run records the owned pointer somewhere else, and what this run then
    reads, guards and replaces has to be the link a host now reaches through.
    """

    def test_the_path_is_a_declared_member_of_the_promotions_fresh_set(self):
        import runtime_install

        self.assertIn("pointer_path", runtime_install.PROMOTION_FRESH)

    def test_a_pointer_path_recorded_while_building_is_the_one_replaced(self):
        with tempfile.TemporaryDirectory() as temporary:
            host = _Host(temporary)
            moved = host.destination / "moved-by-another-run" / "current"

            def competitor():
                hostrecord.update(host.record_path, host.data["definitionVersion"],
                                  pointer={"path": str(moved), "recordedAt": "2026-09-18T01:00:00Z",
                                           "recordedBy": "another run"})

            code, payload = UpdateRecoveryTests()._run(host, interpose=competitor)
            reached = pointer.read(moved)
            left = pointer.read(host.pointer_path)

        self.assertEqual(code, 0, json.dumps(payload)[:1200])
        self.assertEqual(payload["pointer"]["path"], str(moved),
                         "the link a host reaches through is the one the record names now")
        self.assertEqual(reached["target"], str(host.candidate))
        self.assertEqual(left["target"], str(host.previous),
                         "and the path nobody records any more is not touched")


if __name__ == "__main__":
    unittest.main()
