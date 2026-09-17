"""Runtime installer and diagnosis behaviour, exercised against temporary destinations.

Every case here writes only into a temporary directory. None of it reads or changes a real
Codex home, an installed runtime, an MCP registration or an operational database.
"""

import ast
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))

from crw_runtime import check, codexconfig, definition, hooks, hostrecord, ownership, scope

RUNTIME = ROOT / "scripts" / "runtime_install.py"


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

    def test_a_server_name_is_the_first_segment_and_sub_tables_belong_to_it(self):
        view = codexconfig.scan(self.REAL_SHAPE)
        self.assertTrue(view.readable, view.unreadable)
        self.assertEqual(sorted(view.servers), ["codex-thread-bridge", "oracle"])
        self.assertEqual(view.servers["codex-thread-bridge"]["command"], "/opt/bridge")
        self.assertEqual(view.servers["codex-thread-bridge"]["args"], ["--socket", "/tmp/s.sock"])

    def test_shapes_it_does_not_model_are_unreadable_and_are_never_appended_to(self):
        cases = {
            "array of tables": '[[mcp_servers.x]]\ncommand = "a"\n',
            "dotted assignment": 'mcp_servers.x.command = "a"\n',
            "inline assignment": 'mcp_servers = { x = { command = "a" } }\n',
            "unterminated string": '[mcp_servers.x]\ncommand = "oops\n',
            "duplicate server": '[mcp_servers.x]\ncommand = "a"\n\n[mcp_servers.x]\ncommand = "b"\n',
        }
        for name, text in cases.items():
            with self.subTest(case=name):
                view = codexconfig.scan(text)
                self.assertFalse(view.readable, name)
                after, outcome, _ = codexconfig.register(text, "x", "/opt/new", [])
                self.assertEqual(outcome, "UNREADABLE")
                self.assertEqual(after, text, "an unreadable file must not be written to")

    def test_a_table_header_inside_a_multiline_string_is_not_a_registration(self):
        text = 'note = """\n[mcp_servers.ghost]\n"""\n'
        view = codexconfig.scan(text)
        self.assertTrue(view.readable, view.unreadable)
        self.assertEqual(view.servers, {})

    def test_quoted_and_bare_spellings_are_one_server(self):
        view = codexconfig.scan('[mcp_servers."codex-thread-bridge"]\ncommand = "/opt/bridge"\n')
        self.assertEqual(sorted(view.servers), ["codex-thread-bridge"])

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
            "definitionDigest": "abc", "method": "doctor",
        })
        matching = dict(location="/env/pkg", interpreter="3.13.1", source_digest="abc")
        self.assertEqual(len(hostrecord.points_for(record, "codex-session-relay", **matching)), 1)
        for change in (dict(location="/other"), dict(interpreter="3.11.0"), dict(source_digest="def")):
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
            self.assertEqual(hostrecord.load(path, 1)["components"], record["components"])


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
            "definitionDigest": "abc", "codexCli": "0.154.0", "host": "one",
        })
        asked = dict(location="/env/pkg", interpreter="3.13.1", source_digest="abc")
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
        import runtime_install

        return runtime_install.trial_steps(
            issue="JUN-104", parent_task="parent", child_task="child",
            recipient="parent", artifact_root="/tmp/artifacts",
            turn_thread="thread-1", turn_id="turn-1", host="a-host",
        )

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
                         ["settings-record", "register", "generation-open", "generation-bind",
                          "admit-turn", "emit", "deliver"])
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
        # register and generation-open both report the generation as executionGeneration.
        # Reading generation or generationId yields None and sends --generation None.
        import runtime_install

        source = Path(runtime_install.__file__).read_text(encoding="utf-8")
        self.assertIn("executionGeneration", source)


if __name__ == "__main__":
    unittest.main()

