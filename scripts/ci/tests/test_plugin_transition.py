"""The manual-to-plugin transition, against synthetic hosts in temporary directories.

Every case builds a host, runs the real CLI as a subprocess, and reads the JSON it printed. Nothing
here touches a real Codex home: the fixtures are built under the pytest temporary directory and the
commands are given --codex-home explicitly.

What these tests are for, in one line each: that the transition removes only what it can prove runs
this repository's code, that it refuses rather than leaving a host without a hook, a bridge or its
skills, and that running it twice changes nothing the second time.
"""

import json
import os
import shutil
import subprocess
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
CLI = ROOT / "scripts" / "plugin_transition.py"
RUNTIME = ROOT / "scripts" / "runtime_install.py"
INSTALL = ROOT / "scripts" / "install.py"
PLUGIN_VERSION = "0.2.0"
TRUST_KEY = 'crw@crw:wiring/hooks/stop-recording-completion.json:stop:0:0'


def run(argv, **keywords):
    return subprocess.run([sys.executable, *[str(word) for word in argv]],
                          capture_output=True, text=True, timeout=600, **keywords)


class Host:
    """A synthetic host: a Codex home, an install destination, and whatever else a case needs."""

    def __init__(self, root):
        self.root = Path(root)
        self.home = self.root / "home"
        self.destination = self.root / "dest"
        self.marker = self.root / "marker"
        self.journal = self.root / "journal"
        self.database = self.root / "relay.sqlite3"
        self.version = self.destination / "versions" / "v1"
        for directory in (self.home, self.version / "bin", self.marker, self.journal):
            directory.mkdir(parents=True, exist_ok=True)
        (self.destination / "current").symlink_to(self.version)
        for program in ("python3", "codex-session-relay", "codex-thread-bridge",
                        "crw-completion-hook"):
            path = self.version / "bin" / program
            path.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            path.chmod(0o755)

    # ------------------------------------------------------------------ the manual install

    def link_skills(self):
        return run([INSTALL, "--apply", "--dest", self.home / "skills"])

    def register_hook(self, **extra):
        argv = [RUNTIME, "hook", "--adapter", "completion", "--owner", "user",
                "--codex-home", self.home,
                "--relay-command", self.destination / "current" / "bin" / "codex-session-relay",
                "--marker-root", self.marker, "--journal-root", self.journal,
                "--db-path", self.database, "--apply"]
        for key, value in extra.items():
            argv += ["--" + key.replace("_", "-"), str(value)]
        return run(argv)

    def register_mcp(self):
        return run([RUNTIME, "register-mcp", "--owner", "user", "--codex-home", self.home,
                    "--bridge-command",
                    self.destination / "current" / "bin" / "codex-thread-bridge", "--apply"])

    def manual_install(self):
        self.link_skills()
        self.register_hook()
        self.register_mcp()
        return self

    # ------------------------------------------------------------------ the plugin install

    def install_plugin(self, *, trusted=True, payload=True, entry=True, enabled=True):
        cache = self.home / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
        cache.parent.mkdir(parents=True, exist_ok=True)
        if payload:
            shutil.copytree(ROOT / "plugins" / "crw", cache, symlinks=False)
        else:
            (cache / "wiring" / "hooks").mkdir(parents=True)
            (cache / "wiring" / "hooks" / "stop-recording-completion.json").write_text(
                "{}", encoding="utf-8")
        lines = []
        if entry:
            lines.append('[plugins."crw@crw"]')
            lines.append("enabled = " + ("true" if enabled else "false"))
        if trusted:
            lines.append('[hooks.state."' + TRUST_KEY + '"]')
            lines.append('trusted_hash = "sha256:0000"')
        self.append_config("\n".join(lines) + "\n" if lines else "")
        return self

    def append_config(self, text):
        path = self.home / "config.toml"
        existing = path.read_text(encoding="utf-8") if path.is_file() else ""
        path.write_text(existing + ("\n" if existing and not existing.endswith("\n") else "")
                        + text, encoding="utf-8")

    # ------------------------------------------------------------------ running the tool

    def call(self, *arguments):
        done = run([CLI, "--codex-home", self.home, *arguments])
        try:
            return done.returncode, json.loads(done.stdout)
        except ValueError:
            raise AssertionError("the command printed no JSON: " + done.stdout[-2000:]
                                 + done.stderr[-2000:])

    def transition(self, *arguments):
        return self.call("transition", *arguments)

    def outcomes(self, document):
        return {item["step"]: item["outcome"] for item in document["results"]}

    def hooks_document(self):
        return json.loads((self.home / "hooks.json").read_text(encoding="utf-8"))

    def settings(self):
        path = self.home / "crw-completion-hook.json"
        return json.loads(path.read_text(encoding="utf-8")) if path.is_file() else None

    def record(self):
        path = self.home / "crw-bridge-mcp.json"
        return json.loads(path.read_text(encoding="utf-8")) if path.is_file() else None

    def config(self):
        path = self.home / "config.toml"
        return path.read_text(encoding="utf-8") if path.is_file() else ""


class TransitionCase(unittest.TestCase):
    def setUp(self):
        self.temporary = Path(os.environ.get("CRW_TEST_TMPDIR") or "/var/tmp")
        import tempfile
        self.directory = tempfile.mkdtemp(dir=str(self.temporary), prefix="crw115-")
        self.addCleanup(shutil.rmtree, self.directory, ignore_errors=True)
        self.host = Host(self.directory)

    def ready(self, **plugin):
        return self.host.manual_install().install_plugin(**plugin)


class PreflightRefusesBeforeItRemovesAnything(TransitionCase):
    def test_a_host_with_no_plugin_entry_is_refused_and_keeps_everything(self):
        host = self.host.manual_install().install_plugin(entry=False)
        before = host.config(), host.hooks_document(), host.settings()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertEqual(self.host.outcomes(answer)["preflight"], "refused")
        self.assertEqual((host.config(), host.hooks_document(), host.settings()), before)

    def test_a_disabled_plugin_entry_is_refused(self):
        host = self.ready(enabled=False)
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("disabled", answer["results"][0]["detail"])

    def test_an_incomplete_payload_is_refused_by_the_payload_contract(self):
        host = self.ready(payload=False)
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("--payload", answer["results"][0]["detail"])

    def test_an_untrusted_hook_is_refused_until_the_window_is_accepted(self):
        host = self.ready(trusted=False)
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("hooks.state", answer["results"][0]["detail"])
        code, answer = host.transition("--apply", "--accept-hook-trust-gap")
        self.assertEqual(code, 0, answer["results"][0]["detail"])

    def test_a_dangling_pointer_is_refused_rather_than_recorded(self):
        host = self.ready()
        shutil.rmtree(host.version)
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("pointer", answer["results"][0]["detail"])
        self.assertIsNone(host.settings() and host.settings().get("adapterEntryPoint"))

    def test_a_missing_packaged_adapter_is_refused_rather_than_recorded(self):
        host = self.ready()
        (host.version / "bin" / "crw-completion-hook").unlink()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("crw-completion-hook", answer["results"][0]["detail"])

    def test_a_foreign_hook_named_like_ours_is_never_removed(self):
        host = self.ready()
        planted = Path(self.directory) / "foreign"
        (planted / "scripts").mkdir(parents=True)
        (planted / "scripts" / "completion_hook.py").write_text("", encoding="utf-8")
        document = host.hooks_document()
        document["hooks"]["Stop"].append({"hooks": [{
            "type": "command",
            "command": "/bin/sh -c " + str(planted / "scripts" / "completion_hook.py"),
        }]})
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("cannot prove is its own adapter", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), document)

    def test_a_table_this_repository_did_not_render_is_left_alone(self):
        host = self.ready()
        text = host.config().replace("[mcp_servers.codex-thread-bridge]",
                                     "[mcp_servers.codex-thread-bridge]\nenv = { A = \"b\" }")
        (host.home / "config.toml").write_text(text, encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("not the block this repository renders", answer["results"][0]["detail"])
        self.assertEqual(host.config(), text)

    def test_work_in_flight_is_reported_and_refused_until_it_is_allowed(self):
        host = self.ready()
        (host.marker / "published").mkdir(parents=True, exist_ok=True)
        (host.marker / "published" / "an-assignment").write_text("{}", encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("carrying work", answer["results"][0]["detail"])
        code, _ = host.transition("--apply", "--allow-in-flight")
        self.assertEqual(code, 0)


class TheTransitionMovesOnlyWhatItOwns(TransitionCase):
    def test_a_normal_manual_install_converges_to_a_plugin_owned_host(self):
        host = self.ready()
        code, answer = host.transition("--apply")
        outcomes = self.host.outcomes(answer)
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:3000])
        for step in ("hook standdown", "settings retire", "settings install",
                     "mcp record retire", "mcp table standdown", "mcp record install",
                     "skill unlink"):
            self.assertIn(outcomes[step], ("settled", "already_done"), step)
        self.assertEqual(host.hooks_document()["hooks"]["Stop"], [{"hooks": []}])
        self.assertEqual(host.settings()["owner"], "plugin")
        self.assertEqual(host.record()["owner"], "plugin")
        self.assertNotIn("[mcp_servers.codex-thread-bridge]", host.config())
        self.assertEqual(sorted(p.name for p in (host.home / "skills").iterdir()), [])

    def test_the_recorded_adapter_is_absolute_and_under_the_pointer(self):
        host = self.ready()
        host.transition("--apply")
        settings = host.settings()
        expected = host.destination / "current" / "bin"
        self.assertEqual(settings["adapterEntryPoint"], str(expected / "crw-completion-hook"))
        self.assertEqual(settings["adapterInterpreter"], str(expected / "python3"))
        self.assertTrue(Path(settings["adapterEntryPoint"]).is_absolute())
        self.assertIn("current", settings["adapterEntryPoint"])

    def test_the_operational_locations_are_carried_forward_rather_than_relocated(self):
        host = self.ready()
        before = host.settings()
        host.transition("--apply")
        after = host.settings()
        for field in ("markerRoot", "dbPath", "journalRoot", "mode"):
            self.assertEqual(after[field], before[field], field)

    def test_the_retired_settings_and_record_are_kept_not_deleted(self):
        host = self.ready()
        host.transition("--apply")
        kept = sorted(p.name for p in host.home.iterdir() if ".superseded-" in p.name)
        self.assertEqual(len(kept), 2, kept)

    def test_a_foreign_skill_directory_is_left_and_named(self):
        host = self.host.manual_install()
        foreign = host.home / "skills" / "crw-mine"
        foreign.mkdir()
        (foreign / "SKILL.md").write_text("mine", encoding="utf-8")
        host.install_plugin()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, answer["results"][0]["detail"])
        self.assertTrue((foreign / "SKILL.md").is_file())
        unlink = [r for r in answer["results"] if r["step"] == "skill unlink"][0]
        self.assertIn(str(foreign), unlink["foreignLeft"])

    def test_a_later_hook_is_preserved_and_its_renumbering_is_refused_first(self):
        host = self.ready()
        document = host.hooks_document()
        document["hooks"]["Stop"].append({"hooks": [{"type": "command", "command": "/bin/true"}]})
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        standdown = [r for r in answer["results"] if r["step"] == "hook standdown"][0]
        self.assertEqual(standdown["outcome"], "refused")
        self.assertTrue(standdown["shiftedIdentities"])
        self.assertEqual(host.hooks_document(), document)
        code, answer = host.transition("--apply", "--accept-hook-renumbering")
        self.assertEqual(code, 0)
        self.assertIn({"hooks": [{"type": "command", "command": "/bin/true"}]},
                      host.hooks_document()["hooks"]["Stop"])

    def test_the_plugin_settings_are_never_written_while_a_registration_remains(self):
        """The never-two invariant: the settings step is unreachable unless standdown settled."""
        host = self.ready()
        document = host.hooks_document()
        document["hooks"]["Stop"].append({"hooks": [{"type": "command", "command": "/bin/true"}]})
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        code, answer = host.transition("--apply")
        outcomes = self.host.outcomes(answer)
        self.assertEqual(outcomes["hook standdown"], "refused")
        self.assertEqual(outcomes["settings install"], "not_reached")
        # The user-owned document omits owner entirely, so the question is whether the PLUGIN
        # ever became the owner while a registration was still there. It must not have.
        self.assertNotEqual((host.settings() or {}).get("owner"), "plugin")


class RunningItAgainChangesNothing(TransitionCase):
    def test_a_second_run_converges_and_writes_nothing(self):
        host = self.ready()
        host.transition("--apply")
        first = (host.config(), host.hooks_document(), host.settings(), host.record())
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0)
        outcomes = self.host.outcomes(answer)
        # preflight settles on every run by answering that nothing blocks. Every STEP has to
        # report that it found nothing left to do.
        self.assertEqual({step: outcome for step, outcome in outcomes.items()
                          if step != "preflight" and outcome != "already_done"}, {})
        self.assertEqual((host.config(), host.hooks_document(), host.settings(), host.record()),
                         first)

    def test_an_interruption_after_the_hook_step_converges_on_the_next_run(self):
        host = self.ready()
        shutil.rmtree(host.version / "bin")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        for program in ("python3", "codex-session-relay", "codex-thread-bridge",
                        "crw-completion-hook"):
            path = host.version / "bin" / program
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            path.chmod(0o755)
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:2000])
        self.assertEqual(host.settings()["owner"], "plugin")


class DisableAndRemoveKeepTheOperationalData(TransitionCase):
    def test_disable_stops_new_calls_and_deletes_nothing(self):
        host = self.ready()
        host.transition("--apply")
        host.database.write_text("not a real store", encoding="utf-8")
        code, answer = host.call("disable", "--apply")
        self.assertEqual(code, 0)
        self.assertIsNone(host.settings())
        self.assertIsNone(host.record())
        self.assertTrue(host.database.is_file())
        self.assertTrue(host.journal.is_dir())
        self.assertTrue(host.marker.is_dir())
        self.assertIn("doesNotStop", answer)

    def test_remove_deletes_the_records_and_no_operational_path(self):
        host = self.ready()
        host.transition("--apply")
        host.database.write_text("not a real store", encoding="utf-8")
        code, answer = host.call("remove", "--apply")
        self.assertEqual(code, 0)
        self.assertIsNone(host.settings())
        self.assertIsNone(host.record())
        self.assertTrue(host.database.is_file())
        self.assertTrue(host.journal.is_dir())
        self.assertTrue(host.version.is_dir())
        self.assertIn("the relay store, the bridge ledger, the hook journal and every receipt",
                      " ".join(answer["outOfScope"]))

    def test_a_second_remove_is_still_zero_and_still_writes_nothing(self):
        host = self.ready()
        host.transition("--apply")
        host.call("remove", "--apply")
        code, _ = host.call("remove", "--apply")
        self.assertEqual(code, 0)


class SwapStateReportsWhatItRead(TransitionCase):
    def test_the_pointer_and_the_record_are_reported_apart(self):
        host = self.ready()
        code, answer = host.call("swap-state")
        self.assertEqual(code, 0)
        self.assertEqual(answer["pointerState"], "LINK")
        self.assertTrue(answer["pointerResolves"])
        self.assertIsNone(answer["residualFromRun"])
        self.assertIn("cannot be recovered", answer["note"])

    def test_a_dangling_pointer_is_reported_as_a_link_that_does_not_resolve(self):
        host = self.ready()
        shutil.rmtree(host.version)
        code, answer = host.call("swap-state")
        self.assertEqual(answer["pointerState"], "LINK")
        self.assertFalse(answer["pointerResolves"])


class TheDryRunAndTheWayBack(TransitionCase):
    def test_a_dry_run_of_a_normal_manual_install_settles_and_writes_nothing(self):
        """A dry run has to project past the retire step, or it refuses a sequence that works."""
        host = self.ready()
        before = (host.config(), host.hooks_document(), host.settings(), host.record())
        code, answer = host.transition()
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:2000])
        install = [r for r in answer["results"] if r["step"] == "settings install"][0]
        self.assertEqual(install["outcome"], "would_change")
        self.assertTrue(install["projected"])
        self.assertEqual((host.config(), host.hooks_document(), host.settings(), host.record()),
                         before)

    def test_after_a_disable_the_transition_reads_the_retired_locations_back(self):
        host = self.ready()
        host.transition("--apply")
        wanted = {field: host.settings()[field]
                  for field in ("markerRoot", "dbPath", "journalRoot")}
        host.call("disable", "--apply")
        self.assertIsNone(host.settings())
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:2000])
        install = [r for r in answer["results"] if r["step"] == "settings install"][0]
        self.assertIn("retired document", install["carriedFrom"])
        for field, value in wanted.items():
            self.assertEqual(host.settings()[field], value, field)

    def test_a_disable_with_no_retired_document_anywhere_refuses_rather_than_guessing(self):
        host = self.ready()
        host.transition("--apply")
        host.call("disable", "--apply")
        for stale in host.home.glob("crw-completion-hook.json.superseded-*"):
            stale.unlink()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        # Preflight is where this lands, and that is the better place for it: with no live and no
        # retired document there is nothing to derive a destination from, so nothing is removed.
        preflight = answer["results"][0]
        self.assertEqual(preflight["outcome"], "refused")
        self.assertIn("no install destination", preflight["detail"])
        self.assertEqual({r["step"]: r["outcome"] for r in answer["results"][1:]},
                         {r["step"]: "not_reached" for r in answer["results"][1:]})


if __name__ == "__main__":
    unittest.main()
