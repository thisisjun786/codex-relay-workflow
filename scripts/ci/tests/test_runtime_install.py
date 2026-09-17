"""Runtime installer and diagnosis behaviour, exercised against temporary destinations.

Every case here writes only into a temporary directory. None of it reads or changes a real
Codex home, an installed runtime, an MCP registration or an operational database.
"""

import argparse
import ast
import builtins
import errno
import json
import os
import stat
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))

from crw_runtime import (check, codexconfig, definition, hooks, hostrecord, ownership,
                         reading, scope)

RUNTIME = ROOT / "scripts" / "runtime_install.py"

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
            "installDigest": "abc", "method": "doctor",
            "codexCli": "0.1.0", "host": "a-host",
        })
        matching = dict(location="/env/pkg", interpreter="3.13.1", install_digest="abc",
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
            "installDigest": "abc", "codexCli": "0.154.0", "host": "one",
        })
        asked = dict(location="/env/pkg", interpreter="3.13.1", install_digest="abc")
        self.assertEqual(len(hostrecord.points_for(record, "codex-session-relay", **asked,
                                                   codex_cli="0.154.0", host="one")), 1)
        self.assertEqual(hostrecord.points_for(record, "codex-session-relay", **asked,
                                               codex_cli="0.200.0", host="one"), [])
        self.assertEqual(hostrecord.points_for(record, "codex-session-relay", **asked,
                                               codex_cli="0.154.0", host="other"), [])

    def test_a_refusal_payload_does_not_replace_a_reading_that_answered(self):
        readings = {
            "discovery": {"ok": True, "payload": {"stateSelection": {"path": "/s/scope"},
                                                  "actorReachability": {"socketConnect": "ok"}}},
            "selected": {"ok": False, "payload": {"error": "refused", "stateDirectory": "/s/other"}},
        }
        summary = scope.summarise(readings, env={"XDG_STATE_HOME": "/nowhere"})
        self.assertEqual(summary["stateDirectory"], "/s/scope")
        self.assertEqual(summary["socketConnect"], "ok")


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
                         ["assignment-find", "settings-record", "register", "generation-open",
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
                         ["assignment-find", "settings-record", "register", "generation-open",
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
            result = runtime_install._trial(Args(), "/opt/relay")
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
            return runtime_install._trial(args, "/opt/relay"), sent
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
            install_digest="abc"), [], "a point recorded before this existed says nothing"
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
                                          issue=None)
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
            result = runtime_install._trial(args, "/usr/bin/relay")
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
            result = runtime_install._trial(args, "/usr/bin/relay")
        self.assertEqual(result["value"], "not_verified")
        self.assertIn("already belongs to child", result["evidence"])
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
            result = runtime_install._trial(args, "/usr/bin/relay")
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
            ["diagnose", "hook", "install", "measure", "register-mcp", "verify-definition"],
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

            record_path = Path(temporary) / "record.json"
            hostrecord.save(record_path, hostrecord.empty(1))
            before = record_path.read_bytes()

            done = run("install", "--dest", str(destination), "--record", str(record_path),
                       "--apply")
            self.assertEqual(done.returncode, 1, done.stdout + done.stderr)
            payload = json.loads(done.stdout)
            self.assertIn("already exists", payload["refused"])
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
            ["assignment-find", "settings-record", "register", "generation-open",
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
                result = runtime_install._trial(self._args(), "/usr/bin/relay")

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
            result = runtime_install._trial(self._args(), "/usr/bin/relay")
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
                                      issue="JUN-104")
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
                                  socket=None, state=None, issue="JUN-104")
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
            result = runtime_install._trial(args, "/usr/bin/relay")
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
                    self.assertTrue(runtime_install._unusable_artifacts([path], str(root)),
                                    label + " should be refused")
            self.assertEqual(runtime_install._unusable_artifacts([str(real)], str(root)), [])

    def test_a_refused_artifact_stops_the_trial_before_the_first_command(self):
        import runtime_install

        args = argparse.Namespace(
            issue="JUN-104", parent_task="p", child_task="c", recipient="p",
            artifact_root="/tmp/jun104-nowhere", turn_thread="c", turn_id="ti",
            artifact=["/tmp/jun104-nowhere/missing.txt"], dispatch_turn_id="d",
            turn_status="completed", recipient_settings=None,
            settings_already_recorded=True, expect_relationship=None, socket=None, state=None)
        with mock.patch.object(runtime_install.scope, "relay") as relay:
            result = runtime_install._trial(args, "/usr/bin/relay")
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
            "installDigest": "abc", "codexCli": "0.1.0", "host": "a-host",
        })
        asked = dict(location="/env/pkg", interpreter="3.13.1", install_digest="abc",
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
            "installDigest": "abc", "codexCli": "0.1.0", "host": "a-host",
            "digestMatchesDefinition": False,
        })
        found = hostrecord.points_for(
            record, "codex-session-relay", location="/env/pkg", interpreter="3.13.1",
            install_digest="abc", codex_cli="0.1.0", host="a-host")
        self.assertEqual(found, [])

    def test_a_point_written_before_the_field_existed_is_still_readable(self):
        # "not false" rather than "true": such a point was already non-qualifying evidence,
        # not a malformed record.
        record = hostrecord.empty(1)
        hostrecord.add_point(record, "codex-session-relay", {
            "exercised": True, "install": "/env/pkg", "interpreter": "3.13.1",
            "installDigest": "abc", "codexCli": "0.1.0", "host": "a-host",
        })
        found = hostrecord.points_for(
            record, "codex-session-relay", location="/env/pkg", interpreter="3.13.1",
            install_digest="abc", codex_cli="0.1.0", host="a-host")
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

    def test_a_measurement_refuses_when_a_mandatory_dimension_cannot_be_read(self):
        import runtime_install

        data = definition.load()
        with tempfile.TemporaryDirectory() as temporary:
            environment = Path(temporary) / "env"
            record = hostrecord.empty(1)
            bound = {c["component"]: {
                "install": {"location": "/l",
                            "entryPoint": str(environment / "bin" / c["consoleScript"])},
                "digest": c["sourceDigest"], "location": "/l"} for c in data["components"]}
            with mock.patch.object(runtime_install, "_interpreter_prefix",
                                   return_value=str(environment)), \
                 mock.patch.object(runtime_install, "_bind_installs",
                                   return_value=(bound, None)), \
                 mock.patch.object(runtime_install, "codex_cli_version", return_value=None), \
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
        self.assertFalse(measurement["qualifyingPoint"])
        self.assertIn("Codex CLI", measurement["refused"])

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
                "installDigest": "abc", "codexCli": "0.1.0", "host": "a-host",
                "appServer": "a-server"}

    def _asked(self):
        return {"location": "/env/pkg", "interpreter": "3.13.1", "install_digest": "abc",
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
                    self.assertTrue(runtime_install._unusable_artifacts([path], str(root)),
                                    label + " must be refused before anything is written")
            self.assertEqual(runtime_install._unusable_artifacts([str(real)], str(root)), [],
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


if __name__ == "__main__":
    unittest.main()
