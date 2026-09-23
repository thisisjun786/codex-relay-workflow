"""The manual-to-plugin transition, against synthetic hosts in temporary directories.

Every case builds a host, runs the real CLI as a subprocess, and reads the JSON it printed. Nothing
here touches a real Codex home: the fixtures are built under the pytest temporary directory and the
commands are given --codex-home explicitly.

What these tests are for, in one line each: that the transition removes only what it can prove runs
this repository's code, that it refuses rather than leaving a host without a hook, a bridge or its
skills, and that running it twice changes nothing the second time.
"""

import errno
import hashlib
import importlib.util
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
# The payload contract these fixtures have to satisfy is the one the transition itself runs, so
# the rule for deriving a cached package's version is read from that checker rather than copied.
_plugin_spec = importlib.util.spec_from_file_location(
    "crw_plugin_check_for_transition", ROOT / "scripts" / "ci" / "plugin.py")
plugin_check = importlib.util.module_from_spec(_plugin_spec)
_plugin_spec.loader.exec_module(plugin_check)
try:
    import tomllib as _tomllib
except ImportError:
    _tomllib = None
HAS_READER = _tomllib is not None

# Reading a Codex configuration needs tomllib, so on the documented 3.10 floor this transition
# cannot establish whether the host registers the bridge and refuses instead of proceeding. The
# cases that need a readable configuration are marked, and TheFloorRefuses below asserts what
# happens without it, so the behaviour is covered on both interpreters.
needs_reader = unittest.skipUnless(
    HAS_READER, "reading a configuration needs tomllib; this interpreter refuses instead")
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
            if program == "python3":
                # A real interpreter, because preflight asks the candidate to BE a Python before
                # recording it: an executable that is not one would reach no adapter on any Stop.
                path.symlink_to(sys.executable)
                continue
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

    def transition(self, *arguments, trust=True, seal=True):
        """Run the transition, acknowledging the trust gap unless a case is about it.

        This fixture writes a trust key with a made-up hash, which is exactly the state the tool
        refuses to read as trust, so every case that is not about that acknowledges it the way an
        operator would have to.
        """
        if trust and "--accept-hook-trust-gap" not in arguments:
            arguments = arguments + ("--accept-hook-trust-gap",)
        if seal:
            self.seal_cache()
        return self.call("transition", *arguments)

    def seal_cache(self):
        """Let the cached payload declare its own bytes, the way an installed one does.

        Cases here build an alternative package by editing a copy of this repository's, and a
        manifest version that no longer names its payload is refused by the payload contract
        before the declaration under test is ever read. A real installed cache carries a version
        derived from the bytes in it, so the fixture records one too and each case goes on
        refusing for the reason it is named after. A case about the version itself skips this.

        A cache this fixture cannot read is left exactly as it is: a case that made one
        unreadable on purpose is asking what the tool does with it, not what this can record.
        """
        cache = self.home / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
        manifest = cache / ".codex-plugin" / "plugin.json"
        if not manifest.is_file():
            return self
        try:
            document = manifest.read_text(encoding="utf-8")
            payload, _ = plugin_check.directory_payload(cache)
            version = str(json.loads(document).get("version", ""))
        except (OSError, ValueError):
            return self
        recorded = json.dumps(version)
        if document.count(recorded) == 1:
            manifest.write_text(document.replace(
                recorded, json.dumps(plugin_check.payload_version(payload, version))),
                encoding="utf-8")
        return self

    def outcomes(self, document):
        return {item["step"]: item["outcome"] for item in document["results"]}

    def hooks_document(self):
        return json.loads((self.home / "hooks.json").read_text(encoding="utf-8"))

    def settings(self):
        path = self.home / "crw-completion-hook.json"
        return json.loads(path.read_text(encoding="utf-8")) if path.is_file() else None

    def untouched(self):
        """The raw bytes of the two files a refused run must not have moved.

        Parsed documents compare equal across a rewrite that reorders keys or changes spacing,
        and "nothing was taken away" is a claim about the files rather than about their meaning.
        None for an absent file, so a retire that moved one is a difference rather than a crash.
        """
        out = {}
        for name in ("crw-completion-hook.json", "hooks.json"):
            path = self.home / name
            out[name] = path.read_bytes() if path.is_file() else None
        return out

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

    @needs_reader
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

    @needs_reader
    def test_trust_is_never_claimed_and_the_window_is_always_acknowledged(self):
        """A recorded trust key is not proof the declared hook fires, so it is not read as one.

        The hash in a [hooks.state] entry belongs to the hook as it stood when trust was given,
        and nothing here can compute the hash Codex compares it against. A stale record therefore
        looks exactly like a current one, and acting on it turns the stated window into a host
        with no completion hook at all.
        """
        for trusted in (False, True):
            with self.subTest(trustKey=trusted):
                self.setUp()
                host = self.ready(trusted=trusted)
                code, answer = host.transition("--apply", trust=False)
                self.assertEqual(code, 1)
                self.assertIn("cannot be established", answer["results"][0]["detail"])
                self.assertIsNone(host.call("inspect")[1]["host"]["plugin"]["trusted"])
                code, answer = host.transition("--apply")
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

    @needs_reader
    def test_a_table_this_repository_did_not_render_is_left_alone(self):
        host = self.ready()
        text = host.config().replace("[mcp_servers.codex-thread-bridge]",
                                     "[mcp_servers.codex-thread-bridge]\nenv = { A = \"b\" }")
        (host.home / "config.toml").write_text(text, encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("not the block this repository renders", answer["results"][0]["detail"])
        self.assertEqual(host.config(), text)

    @needs_reader
    def test_marker_history_is_reported_and_never_refuses_on_its_own(self):
        """A marker is created once and outlives the work it recorded.

        Refusing on its presence would block every host that has ever run a managed turn, and the
        reading was never about liveness. The history is reported; the refusal is not.
        """
        host = self.ready()
        (host.marker / "published").mkdir(parents=True, exist_ok=True)
        (host.marker / "published" / "an-assignment").write_text("{}", encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:1500])
        work = [note["work"] for note in answer["results"][0]["notes"] if "work" in note]
        self.assertEqual(len(work), 1)
        self.assertIn("published", work[0]["markerHistory"])
        self.assertIsNone(work[0]["liveness"])

    @needs_reader
    def test_a_bridge_registered_under_another_name_is_refused(self):
        """register-mcp takes --name, so the same bridge can sit under a table we do not declare."""
        host = self.ready()
        host.append_config('[mcp_servers.my-bridge]\ncommand = "'
                           + str(host.destination / "current" / "bin" / "codex-thread-bridge")
                           + '"\n')
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("starts the same bridge under another name",
                      answer["results"][0]["detail"])
        self.assertEqual(host.config(), before)


@needs_reader
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

    def test_a_migrated_host_gets_the_fallback_launcher_too(self):
        """Devin review: this command writes the same settings, so it owes the same fallback.

        Without it a migrated host ends with plugin-owned settings and one candidate again, and
        the first package replacement during an open turn lands back in the loop CRW-178 closes.
        """
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import completion as _completion

        host = self.ready()
        placed = Path(host.home) / _completion.LAUNCHER_NAME
        self.assertFalse(placed.exists())
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"])[:700])
        self.assertTrue(placed.is_file(), json.dumps(answer["results"])[:700])
        self.assertEqual(placed.read_bytes(),
                         (ROOT / _completion.LAUNCHER_SOURCE).read_bytes())
        step = [item for item in answer["results"] if item["step"] == "stable launcher install"]
        self.assertEqual(len(step), 1, json.dumps(answer["results"])[:700])
        self.assertIn(step[0]["outcome"], ("settled", "already_done"))

    def test_a_dry_run_migration_places_no_launcher(self):
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import completion as _completion

        host = self.ready()
        code, answer = host.transition()
        self.assertEqual(code, 0, json.dumps(answer["results"])[:700])
        self.assertFalse((Path(host.home) / _completion.LAUNCHER_NAME).exists())
        step = [item for item in answer["results"] if item["step"] == "stable launcher install"]
        self.assertEqual([item["outcome"] for item in step], ["would_change"])


    def test_a_held_launcher_lock_stops_the_sequence_before_the_settings(self):
        """A busy launcher must not let the rest of the sequence run without it.

        transition() owns the Busy handler: it names the step that was running and marks every
        later step not reached. Answering busy inside the step instead would hand the main loop
        an ordinary result it does not break on.

        The step runs first, ahead of everything destructive, so a refusal leaves the host
        exactly as it was. Placed after the retire and the standdown it did the opposite: the
        settings were archived and the manual registration removed with nothing to put them
        back, which is a worse host than the one the command started with.
        """
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import completion as _completion, hostrecord as _hostrecord

        host = self.ready()
        launcher = Path(host.home) / _completion.LAUNCHER_NAME
        lock = Path(str(launcher) + _hostrecord.LOCK_SUFFIX)
        lock.parent.mkdir(parents=True, exist_ok=True)
        lock.write_text("99999999", encoding="utf-8")
        self.addCleanup(lambda: lock.exists() and lock.unlink())
        untouched_before = host.untouched()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        outcomes = {item["step"]: item["outcome"] for item in answer["results"]}
        self.assertEqual(outcomes.get("stable launcher install"), "busy",
                         json.dumps(answer["results"])[:700])
        for later in ("settings retire", "hook standdown", "settings install"):
            self.assertEqual(outcomes.get(later), "not_reached", later)
        self.assertFalse(launcher.exists())
        self.assertEqual(host.untouched(), untouched_before)

    def test_a_foreign_launcher_stops_the_sequence_before_anything_is_taken_away(self):
        """The same ordering question asked with a refusal instead of a contended lock."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import completion as _completion

        host = self.ready()
        launcher = Path(host.home) / _completion.LAUNCHER_NAME
        foreign = "not ours\n"
        launcher.write_text(foreign, encoding="utf-8")
        untouched_before = host.untouched()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        outcomes = {item["step"]: item["outcome"] for item in answer["results"]}
        self.assertEqual(outcomes.get("stable launcher install"), "refused",
                         json.dumps(answer["results"])[:700])
        for later in ("settings retire", "hook standdown", "settings install"):
            self.assertEqual(outcomes.get(later), "not_reached", later)
        self.assertEqual(launcher.read_text(encoding="utf-8"), foreign)
        self.assertEqual(host.untouched(), untouched_before)

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

    def test_a_hook_after_ours_in_the_same_group_is_refused_first_then_preserved(self):
        """Removal pops out of its own group, so only later hooks in THAT group take a new index."""
        host = self.ready()
        document = host.hooks_document()
        foreign = {"type": "command", "command": "/bin/true"}
        document["hooks"]["Stop"][0]["hooks"].append(foreign)
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        # Refused at preflight, before the settings are archived: the settings retire runs first
        # now, so refusing only at the standdown would leave the registration in place with its
        # configuration already moved aside.
        self.assertEqual(answer["results"][0]["outcome"], "refused")
        self.assertIn("user:Stop:0:1", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), document)
        self.assertEqual(sorted(host.home.glob("*.superseded-*")), [])
        code, answer = host.transition("--apply", "--accept-hook-renumbering")
        self.assertEqual(code, 0)
        self.assertEqual(host.hooks_document()["hooks"]["Stop"][0]["hooks"], [foreign])

    def test_a_hook_in_a_later_group_shifts_nothing_and_is_not_refused(self):
        """An emptied group is left in place, so matcher indices never move.

        Counting a later group's hook as shifted refused a transition that moves nothing and told
        the operator its trust had been detached when it had not.
        """
        host = self.ready()
        document = host.hooks_document()
        foreign = {"hooks": [{"type": "command", "command": "/bin/true"}]}
        document["hooks"]["Stop"].append(foreign)
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:1500])
        standdown = [r for r in answer["results"] if r["step"] == "hook standdown"][0]
        self.assertEqual(standdown.get("shiftedIdentities", []), [])
        groups = host.hooks_document()["hooks"]["Stop"]
        self.assertEqual(groups, [{"hooks": []}, foreign])
        # Its identity is what trust is recorded against, and it is unchanged.
        _, seen = host.call("inspect")
        self.assertEqual([item["identity"] for item in seen["host"]["hook"]["entries"]], [])

    def test_the_plugin_settings_are_never_written_while_a_registration_remains(self):
        """The never-two invariant: the settings step is unreachable unless standdown settled."""
        host = self.ready()
        document = host.hooks_document()
        document["hooks"]["Stop"][0]["hooks"].append({"type": "command", "command": "/bin/true"})
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        code, answer = host.transition("--apply")
        outcomes = self.host.outcomes(answer)
        self.assertEqual(outcomes["preflight"], "refused")
        self.assertEqual(outcomes["hook standdown"], "not_reached")
        self.assertEqual(outcomes["settings install"], "not_reached")
        # The user-owned document omits owner entirely, so the question is whether the PLUGIN
        # ever became the owner while a registration was still there. It must not have.
        self.assertNotEqual((host.settings() or {}).get("owner"), "plugin")


@needs_reader
class RunningItAgainChangesNothing(TransitionCase):
    def test_a_second_run_converges_and_writes_nothing(self):
        host = self.ready()
        host.transition("--apply")
        first = (host.config(), host.hooks_document(), host.settings(), host.record())
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0)
        outcomes = self.host.outcomes(answer)
        # preflight and the recheck settle on every run: they are readings, not work. Every STEP
        # has to report that it found nothing left to do.
        readings = ("preflight", "hook recheck")
        self.assertEqual({step: outcome for step, outcome in outcomes.items()
                          if step not in readings and outcome != "already_done"}, {})
        self.assertEqual(outcomes["hook recheck"], "settled")
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
            if program == "python3":
                path.symlink_to(sys.executable)
                continue
            path.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            path.chmod(0o755)
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:2000])
        self.assertEqual(host.settings()["owner"], "plugin")


@needs_reader
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


@needs_reader
class ARecordedPolicySurvivesTheTransition(TransitionCase):
    """A plugin record that names the host's execution policy keeps naming it.

    The transition rebuilds the plugin-owned bridge record, and it used to build it with no policy
    field at all. On a host whose record names one, a rerun then refused its own record, and a
    rebuild after a disable wrote a record that starts a bridge checking no role.
    """

    def with_policy(self):
        host = self.ready()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:2000])
        policy = host.root / "execution-policy.json"
        policy.write_text(json.dumps({"roles": {
            "child": {"model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}}}),
            encoding="utf-8")
        # The repair register-mcp names for a record written before the field: move it aside,
        # then register again with the policy.
        record = host.home / "crw-bridge-mcp.json"
        record.rename(record.with_name("crw-bridge-mcp.json.pre-policy"))
        done = run([RUNTIME, "register-mcp", "--owner", "plugin", "--codex-home", host.home,
                    "--bridge-command",
                    host.destination / "current" / "bin" / "codex-thread-bridge",
                    "--execution-policy", policy, "--apply"])
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        reference = host.record()["executionPolicy"]
        self.assertEqual(reference["path"], str(policy))
        return host, reference

    def test_a_rerun_on_a_host_whose_record_names_a_policy_changes_nothing(self):
        host, reference = self.with_policy()
        before = (host.home / "crw-bridge-mcp.json").read_bytes()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:2000])
        self.assertEqual(host.outcomes(answer)["mcp record install"], "already_done")
        self.assertEqual((host.home / "crw-bridge-mcp.json").read_bytes(), before)

    def test_a_transition_after_a_disable_restores_the_policy_it_retired(self):
        host, reference = self.with_policy()
        code, answer = host.call("disable", "--apply")
        self.assertEqual(code, 0, json.dumps(answer, indent=2)[:2000])
        self.assertIsNone(host.record())
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:2000])
        self.assertEqual(host.record()["executionPolicy"], reference)
        self.assertEqual(host.record()["recordVersion"], 2)

    def _rebuilt_after_disable_with(self, change, reason):
        """Devin, PR #137: the rebuild carried the archived reference without asking the file.

        The launcher hashes the file at every start and refuses a record whose digest no longer
        matches, so a rebuilt stale reference is a record no new thread can start from, reported
        as settled. Dropping the reference would start a bridge that checks no role instead.
        """
        host, reference = self.with_policy()
        code, answer = host.call("disable", "--apply")
        self.assertEqual(code, 0, json.dumps(answer, indent=2)[:2000])
        change(Path(reference["path"]))
        code, answer = host.transition("--apply")
        results = json.dumps(answer["results"])
        self.assertNotEqual(code, 0, results[:2000])
        # Preflight asks the record step first, so the refusal lands before anything is removed.
        self.assertEqual(host.outcomes(answer)["preflight"], "refused", results[:2000])
        self.assertIn(reason, results)
        self.assertIsNone(host.record())

    def test_a_policy_edited_after_a_disable_is_not_restored_as_a_dead_record(self):
        self._rebuilt_after_disable_with(
            lambda policy: policy.write_text(json.dumps({"roles": {"child": {
                "model": "anthropic/claude-opus-5-5", "reasoningEffort": "high"}}}),
                encoding="utf-8"),
            "now hashes to")

    def test_a_policy_removed_after_a_disable_is_not_restored_as_a_dead_record(self):
        self._rebuilt_after_disable_with(lambda policy: policy.unlink(), "could not be opened")

    def test_a_live_record_whose_policy_was_edited_is_not_confirmed(self):
        host, reference = self.with_policy()
        before = (host.home / "crw-bridge-mcp.json").read_bytes()
        Path(reference["path"]).write_text("{}", encoding="utf-8")
        code, answer = host.transition("--apply")
        results = json.dumps(answer["results"])
        self.assertNotEqual(code, 0, results[:2000])
        self.assertEqual(host.outcomes(answer)["preflight"], "refused", results[:2000])
        self.assertIn("now hashes to", results)
        self.assertEqual((host.home / "crw-bridge-mcp.json").read_bytes(), before)

    def test_a_newest_archive_that_cannot_be_read_refuses_rather_than_dropping_the_policy(self):
        """Stepping over it to an older archive would rebuild a record that checks no role."""
        host, reference = self.with_policy()
        code, answer = host.call("disable", "--apply")
        self.assertEqual(code, 0, json.dumps(answer, indent=2)[:2000])
        archives = sorted(host.home.glob("crw-bridge-mcp.json.superseded-*"))
        self.assertTrue(archives)
        newest = max(archives, key=lambda path: path.stat().st_mtime_ns)
        document = json.loads(newest.read_text(encoding="utf-8"))
        self.assertEqual(document["executionPolicy"], reference)
        document["executionPolicy"]["digest"] = "not a digest"
        newest.write_text(json.dumps(document), encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertNotEqual(code, 0, json.dumps(answer["results"], indent=2)[:2000])
        self.assertIsNone(host.record())
        self.assertIn("newest retired bridge record", json.dumps(answer["results"]))

    def _refuses_with_newest_archive_replaced(self, replace):
        host, reference = self.with_policy()
        code, answer = host.call("disable", "--apply")
        self.assertEqual(code, 0, json.dumps(answer, indent=2)[:2000])
        archives = sorted(host.home.glob("crw-bridge-mcp.json.superseded-*"))
        self.assertGreater(len(archives), 1, "an older readable archive has to be there to fall to")
        newest = max(archives, key=lambda path: path.stat().st_mtime_ns)
        self.assertEqual(json.loads(newest.read_text(encoding="utf-8"))["executionPolicy"],
                         reference)
        newest.unlink()
        replace(newest)
        code, answer = host.transition("--apply")
        self.assertNotEqual(code, 0, json.dumps(answer["results"], indent=2)[:2000])
        self.assertIsNone(host.record())
        self.assertIn("newest retired bridge record", json.dumps(answer["results"]))

    def test_a_rebuild_takes_its_whole_identity_from_one_reading_of_the_newest_archive(self):
        """A listing that misses the newest archive for a moment must not supply the record.

        The transition read the archives twice: once to choose the executable and arguments, once
        to check the newest. An archive gone from the first listing and back for the second let
        an older, policy-free record through. Here the legacy listing is made to miss it.
        """
        import importlib
        sys.path.insert(0, str(ROOT / "scripts"))
        steps = importlib.import_module("crw_transition.steps")
        inventory = importlib.import_module("crw_transition.inventory")
        from crw_runtime import bridgerecord
        home = self.host.home
        policy = self.host.root / "execution-policy.json"
        policy.write_text(json.dumps({"roles": {
            "child": {"model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}}}),
            encoding="utf-8")
        reference = {"path": str(policy),
                     "digest": hashlib.sha256(policy.read_bytes()).hexdigest()}
        bridge = str(self.host.destination / "current" / "bin" / "codex-thread-bridge")
        older = bridgerecord.document(command=bridge, name="codex-thread-bridge",
                                      owner=bridgerecord.OWNER_PLUGIN)
        newer = bridgerecord.document(command=bridge, name="codex-thread-bridge",
                                      owner=bridgerecord.OWNER_PLUGIN, execution_policy=reference)
        stem = bridgerecord.RECORD_NAME + ".superseded-"
        (home / (stem + "20200101T000000Z")).write_text(json.dumps(older), encoding="utf-8")
        (home / (stem + "20210101T000000Z")).write_text(json.dumps(newer), encoding="utf-8")
        host = {"codexHome": str(home), "destination": str(self.host.destination),
                "mcp": {"record": None, "registration": None, "recordOwner": None,
                        "recordPath": str(home / bridgerecord.RECORD_NAME)}}
        real = inventory.archives

        def missing_the_newest(where, prefix):
            return [path for path in real(where, prefix) if "20210101" not in path.name]

        original = inventory.archives
        inventory.archives = missing_the_newest
        try:
            answer = steps.mcp_record_install(host, {}, apply=False)
        finally:
            inventory.archives = original
        self.assertEqual(answer["record"]["wanted"].get("executionPolicy"), reference,
                         json.dumps(answer, indent=2)[:2000])

    def test_a_policy_edited_while_the_rebuilt_record_is_written_is_refused(self):
        """The transition writes through the same checks as register-mcp: nothing is written."""
        import importlib
        sys.path.insert(0, str(ROOT / "scripts"))
        steps = importlib.import_module("crw_transition.steps")
        from crw_runtime import bridgerecord, hostrecord
        home = self.host.home
        policy = self.host.root / "execution-policy.json"
        policy.write_text(json.dumps({"roles": {
            "child": {"model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}}}),
            encoding="utf-8")
        reference = {"path": str(policy),
                     "digest": hashlib.sha256(policy.read_bytes()).hexdigest()}
        bridge = str(self.host.destination / "current" / "bin" / "codex-thread-bridge")
        retired = bridgerecord.document(command=bridge, name="codex-thread-bridge",
                                        owner=bridgerecord.OWNER_PLUGIN,
                                        execution_policy=reference)
        (home / (bridgerecord.RECORD_NAME + ".superseded-20210101T000000Z")).write_text(
            json.dumps(retired), encoding="utf-8")
        host = {"codexHome": str(home), "destination": str(self.host.destination),
                "mcp": {"record": None, "registration": None, "recordOwner": None,
                        "recordPath": str(home / bridgerecord.RECORD_NAME)}}
        real = hostrecord.Locked.__enter__

        def entered_then_edited(lock):
            held = real(lock)
            if lock.path.name.startswith(bridgerecord.RECORD_NAME):
                policy.write_text("{}", encoding="utf-8")
            return held

        hostrecord.Locked.__enter__ = entered_then_edited
        try:
            answer = steps.mcp_record_install(host, {}, apply=True)
        finally:
            hostrecord.Locked.__enter__ = real
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer, indent=2)[:2000])
        self.assertIn("now hashes to", json.dumps(answer))
        self.assertFalse((home / bridgerecord.RECORD_NAME).exists())

    def test_a_newest_archive_that_became_a_pipe_refuses(self):
        self._refuses_with_newest_archive_replaced(os.mkfifo)

    def test_an_archive_name_this_tool_does_not_write_refuses_the_rebuild(self):
        """Devin, PR #137: a name archive_order could not parse ranked below every valid one.

        The rebuild stepped over it to an older archive, and an older archive can predate the
        policy: the record rebuilt from it starts a bridge that checks no role.
        """
        import importlib
        sys.path.insert(0, str(ROOT / "scripts"))
        steps = importlib.import_module("crw_transition.steps")
        from crw_runtime import bridgerecord
        home = self.host.home
        policy = self.host.root / "execution-policy.json"
        policy.write_text(json.dumps({"roles": {
            "child": {"model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}}}),
            encoding="utf-8")
        reference = {"path": str(policy),
                     "digest": hashlib.sha256(policy.read_bytes()).hexdigest()}
        bridge = str(self.host.destination / "current" / "bin" / "codex-thread-bridge")
        older = bridgerecord.document(command=bridge, name="codex-thread-bridge",
                                      owner=bridgerecord.OWNER_PLUGIN)
        newer = bridgerecord.document(command=bridge, name="codex-thread-bridge",
                                      owner=bridgerecord.OWNER_PLUGIN, execution_policy=reference)
        stem = bridgerecord.RECORD_NAME + ".superseded-"
        (home / (stem + "20200101T000000Z")).write_text(json.dumps(older), encoding="utf-8")
        (home / (stem + "20210101T000000Z-x1")).write_text(json.dumps(newer), encoding="utf-8")
        host = {"codexHome": str(home), "destination": str(self.host.destination),
                "mcp": {"record": None, "registration": None, "recordOwner": None,
                        "recordPath": str(home / bridgerecord.RECORD_NAME)}}
        answer = steps.mcp_record_install(host, {}, apply=True)
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer, indent=2)[:2000])
        self.assertIn("20210101T000000Z-x1", json.dumps(answer))
        self.assertFalse((home / bridgerecord.RECORD_NAME).exists(),
                         "no policy-free record is installed in its place")

    def test_an_archive_stamp_that_is_no_moment_refuses_the_rebuild(self):
        """Review of e538bfa1: a stamp of the right shape that no clock produces was accepted.

        retire() takes a stamp from the clock or from an archive already there, so a month 99 was
        never written by it; ranked as the newest, it restored a record with no policy.
        """
        import importlib
        sys.path.insert(0, str(ROOT / "scripts"))
        steps = importlib.import_module("crw_transition.steps")
        from crw_runtime import bridgerecord
        home = self.host.home
        policy = self.host.root / "execution-policy.json"
        policy.write_text(json.dumps({"roles": {
            "child": {"model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}}}),
            encoding="utf-8")
        reference = {"path": str(policy),
                     "digest": hashlib.sha256(policy.read_bytes()).hexdigest()}
        bridge = str(self.host.destination / "current" / "bin" / "codex-thread-bridge")
        bearing = bridgerecord.document(command=bridge, name="codex-thread-bridge",
                                        owner=bridgerecord.OWNER_PLUGIN,
                                        execution_policy=reference)
        free = bridgerecord.document(command=bridge, name="codex-thread-bridge",
                                     owner=bridgerecord.OWNER_PLUGIN)
        stem = bridgerecord.RECORD_NAME + ".superseded-"
        (home / (stem + "20210101T000000Z")).write_text(json.dumps(bearing), encoding="utf-8")
        (home / (stem + "99999999T999999Z")).write_text(json.dumps(free), encoding="utf-8")
        host = {"codexHome": str(home), "destination": str(self.host.destination),
                "mcp": {"record": None, "registration": None, "recordOwner": None,
                        "recordPath": str(home / bridgerecord.RECORD_NAME)}}
        answer = steps.mcp_record_install(host, {}, apply=True)
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer, indent=2)[:2000])
        self.assertIn("99999999T999999Z", json.dumps(answer))
        self.assertFalse((home / bridgerecord.RECORD_NAME).exists(),
                         "no policy-free record is installed")

    def test_a_newest_archive_that_became_a_directory_refuses(self):
        self._refuses_with_newest_archive_replaced(lambda path: path.mkdir())

    def test_a_newest_archive_that_became_a_dangling_link_refuses(self):
        self._refuses_with_newest_archive_replaced(
            lambda path: path.symlink_to(path.with_name("nothing-here")))


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


@needs_reader
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


class TheFloorRefuses(TransitionCase):
    """What happens on an interpreter that cannot read a Codex configuration.

    Not a skipped case: the refusal IS the behaviour on that interpreter, and the thing worth
    proving is that it refuses rather than reading an unreadable configuration as one holding no
    registration. Read as absence, the plugin record would be written while the file still
    registers the bridge, and that is two bridges.
    """

    @unittest.skipIf(HAS_READER, "this interpreter can read a configuration")
    def test_without_a_reader_the_transition_refuses_and_removes_nothing(self):
        host = self.ready()
        before = (host.config(), host.hooks_document(), host.settings(), host.record())
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("could not be read", answer["results"][0]["detail"])
        self.assertEqual((host.config(), host.hooks_document(), host.settings(), host.record()),
                         before)


class ReadingsThatMustNotEndTheCommand(TransitionCase):
    """Deliberately outside the reader-gated classes, because these bite on the 3.10 floor.

    A case that can only run where the defect cannot happen proves nothing about the defect. The
    inventory of the skill links needs no configuration reader, so these run on both interpreters.
    """

    def test_a_looping_skill_link_is_classified_rather_than_ending_the_command(self):
        """Before 3.13 a non-strict resolve() answers a symlink loop with RuntimeError."""
        host = self.ready()
        # Named away from crw-loop, which is a real skill this fixture links.
        first = host.home / "skills" / "crw-ouroboros-a"
        second = host.home / "skills" / "crw-ouroboros-b"
        first.symlink_to(second)
        second.symlink_to(first)
        code, answer = host.call("inspect")
        self.assertEqual(code, 0, json.dumps(answer)[:600])
        owned = [item["path"] for item in answer["host"]["skills"]["crwOwned"]]
        self.assertFalse([item for item in owned if "ouroboros" in item], json.dumps(owned))
        foreign = [item["path"] for item in answer["host"]["skills"]["foreign"]]
        self.assertEqual(len([item for item in foreign if "ouroboros" in item]), 2,
                         json.dumps(foreign))


@needs_reader
class TheFindingsFromReview(TransitionCase):
    """One case per defect hosted review found, so none of them comes back quietly."""

    def test_ten_or_more_hooks_in_one_event_leave_no_copy_behind(self):
        """Identity is positional and sorting it as text puts :10: before :2:."""
        host = self.ready()
        document = host.hooks_document()
        groups = document["hooks"]["Stop"]
        ours = groups[0]
        for _ in range(11):
            groups.append({"hooks": [{"type": "command", "command": "/bin/true"}]})
        groups.append(ours)
        document["hooks"]["Stop"] = groups[1:]
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        code, answer = host.transition("--apply", "--accept-hook-renumbering")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:2000])
        left = json.dumps(host.hooks_document())
        self.assertNotIn("completion_hook.py", left)
        self.assertEqual(left.count("/bin/true"), 11)

    def test_disable_before_a_transition_refuses_the_user_owned_records(self):
        host = self.ready()
        before = (host.settings(), host.record())
        code, answer = host.call("disable", "--apply")
        self.assertEqual(code, 1)
        self.assertEqual([r["outcome"] for r in answer["results"]], ["refused", "refused"])
        self.assertIn("belongs to the manual install", answer["results"][0]["detail"])
        self.assertEqual((host.settings(), host.record()), before)

    def test_a_retired_document_that_no_longer_reads_as_settings_is_not_reused(self):
        host = self.ready()
        host.transition("--apply")
        host.call("disable", "--apply")
        for retired in host.home.glob("crw-completion-hook.json.superseded-*"):
            retired.write_text('{"configVersion": 99}', encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("no install destination", answer["results"][0]["detail"])

    def test_two_cached_versions_are_reported_rather_than_guessed_between(self):
        host = self.ready()
        other = host.home / "plugins" / "cache" / "crw" / "crw" / "0.3.0"
        shutil.copytree(host.home / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION, other)
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("more than one cached version", answer["results"][0]["detail"])

    def test_a_later_table_with_an_enabled_key_is_not_read_as_the_plugin(self):
        host = self.ready()
        host.append_config('[some.other.table]\nenabled = false\n')
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:1500])

    def test_a_bridge_table_carrying_another_field_is_refused_and_left_intact(self):
        host = self.ready()
        text = host.config().replace(
            '[mcp_servers.codex-thread-bridge]',
            '[mcp_servers.codex-thread-bridge]\nstartup_timeout_sec = 30')
        (host.home / "config.toml").write_text(text, encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        # The verdict is unchanged -- refused, and the file untouched. What changed with CRW-142
        # is the reason: the key in the way is named, because "more or other than the command and
        # arguments" sent a reader looking at a command and arguments that were exactly right.
        self.assertIn("startup_timeout_sec", answer["results"][0]["detail"])
        self.assertIn("the plugin declaration does not reproduce",
                      answer["results"][0]["detail"])
        self.assertEqual(host.config(), text)

    def test_a_registration_naming_an_unrelated_file_does_not_retire_it(self):
        host = self.ready()
        bystander = Path(self.directory) / "not-settings.json"
        bystander.write_text('{"mine": true}', encoding="utf-8")
        document = host.hooks_document()
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        entry["command"] = entry["command"].rsplit(" ", 1)[0] + " " + str(bystander)
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        host.transition("--apply", "--accept-hook-trust-gap")
        self.assertTrue(bystander.is_file())
        self.assertEqual(json.loads(bystander.read_text(encoding="utf-8")), {"mine": True})

    def test_an_override_would_write_where_no_launcher_reads_so_it_refuses(self):
        host = self.ready()
        done = run([CLI, "--codex-home", host.home, "transition", "--apply",
                    "--accept-hook-trust-gap"],
                   env={**os.environ, "CRW_COMPLETION_HOOK_CONFIG": str(host.root / "e.json")})
        answer = json.loads(done.stdout)
        self.assertEqual(done.returncode, 1)
        # It refuses at preflight, because with the override in force the live settings are read
        # from a path nothing wrote, so no destination can be derived either. What matters is that
        # nothing was written and no document landed where no launcher reads.
        self.assertEqual(answer["results"][0]["outcome"], "refused")
        self.assertFalse((host.root / "e.json").exists())
        self.assertEqual({r["step"]: r["outcome"] for r in answer["results"][1:]},
                         {r["step"]: "not_reached" for r in answer["results"][1:]})

    def test_the_arguments_survive_in_the_retired_record(self):
        host = self.ready()
        host.transition("--apply")
        retired = sorted(host.home.glob("crw-bridge-mcp.json.superseded-*"))
        self.assertTrue(retired)
        self.assertEqual(json.loads(retired[0].read_text(encoding="utf-8"))["owner"], "user")
        self.assertEqual(host.record()["args"],
                         json.loads(retired[0].read_text(encoding="utf-8"))["args"])

    def test_remove_leaves_the_links_when_the_records_were_not_retired(self):
        """Unlinking after a refused disable takes the skills from an install we did not touch."""
        host = self.ready()
        code, answer = host.call("remove", "--apply")
        self.assertEqual(code, 1)
        unlink = [r for r in answer["results"] if r["step"] == "skill unlink"][0]
        self.assertEqual(unlink["outcome"], "not_reached")
        self.assertTrue(sorted((host.home / "skills").iterdir()))

    def test_an_event_this_package_does_not_declare_is_refused_before_anything_is_read(self):
        host = self.ready()
        done = run([CLI, "--codex-home", host.home, "--event", "SessionStart", "transition",
                    "--apply", "--accept-hook-trust-gap"])
        self.assertEqual(done.returncode, 2)
        self.assertIn("declares only the Stop hook", json.loads(done.stdout)["error"])

    def test_a_guard_budget_the_launcher_cannot_outlast_is_refused_at_preflight(self):
        host = self.host.manual_install()
        settings = host.settings()
        settings["timeoutSeconds"] = 9
        (host.home / "crw-completion-hook.json").write_text(json.dumps(settings), encoding="utf-8")
        host.install_plugin()
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("guard budget", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_a_journal_policy_this_command_cannot_carry_is_refused_at_preflight(self):
        host = self.host.manual_install()
        settings = host.settings()
        settings["journalPolicy"] = "faults_only"
        (host.home / "crw-completion-hook.json").write_text(json.dumps(settings), encoding="utf-8")
        host.install_plugin()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("journalPolicy", answer["results"][0]["detail"])

    def test_ten_archives_in_one_second_still_recover_the_newest(self):
        """The archives are recovered by sorting their names, so -10 must not sort before -9."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        host.transition("--apply")
        path = host.home / "crw-completion-hook.json"
        settings = json.loads(path.read_text(encoding="utf-8"))
        for index in range(12):
            # retire() MOVES the file, so each round writes a fresh one, which is also what a host
            # being transitioned repeatedly would present.
            settings["markerRoot"] = str(host.marker / ("round%d" % index))
            path.write_text(json.dumps(settings), encoding="utf-8")
            steps.retire(path)
        document, name = inventory.newest_retired(host.home)
        self.assertEqual(document["markerRoot"], str(host.marker / "round11"),
                         "recovered " + str(name))
        # Past the padding too: the suffix is compared as a number, so 1000 does not sort under 999.
        stem = "crw-completion-hook.json.superseded-"
        self.assertGreater(inventory.archive_order(host.home / (stem + "20260101T000000Z-1000"), stem),
                           inventory.archive_order(host.home / (stem + "20260101T000000Z-999"), stem))

    def test_a_nested_server_table_is_not_proven_and_is_left_alone(self):
        """[mcp_servers.<name>.env] belongs to the same registration even though it is a header."""
        host = self.ready()
        host.append_config('[mcp_servers.codex-thread-bridge.env]\nA = "b"\n')
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("not the block this repository renders", answer["results"][0]["detail"])
        self.assertEqual(host.config(), before)
        self.assertIsNotNone(host.record())

    def test_a_second_run_under_the_lock_does_not_retire_the_first_runs_record(self):
        """The snapshot is taken before the lock, so the MCP surface is re-read inside it."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        stale = inventory.snapshot(host.home, repo_root=ROOT)
        code, _ = host.transition("--apply")
        self.assertEqual(code, 0)
        installed = host.record()
        self.assertEqual(installed["owner"], "plugin")
        # The second run still holds the pre-lock reading, which named the user-owned record and
        # the registrations the first run has since removed. It stops at the hook step, because a
        # stale positional identity is exactly what must not be acted on.
        results = steps.transition(stale, {"accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        # It stops at the first step whose proof has gone stale, which is the retire: the settings
        # on disk are the plugin-owned ones the first run wrote, not the document this reading
        # proved. Archiving them would take a live installation's configuration away.
        self.assertEqual(outcomes.get("settings retire"), "refused", json.dumps(results)[:900])
        self.assertIn("changed after it was read",
                      [item for item in results if item["step"] == "settings retire"][0]["detail"])
        self.assertEqual(outcomes.get("hook standdown"), "not_reached")
        self.assertEqual(host.record(), installed)
        self.assertIsNotNone(host.settings())

        # And with the hook surface refreshed but the MCP reading still stale -- the shape the
        # ownership lock exists for -- the retire step stands down instead of taking the plugin
        # record the first run wrote. Driven through transition(), because the re-read happens
        # there, inside the lock, not in the step.
        # Everything refreshed EXCEPT the MCP reading, which is the one the ownership lock exists
        # to re-take. Leaving the settings proof stale as well would stop the run a step earlier.
        fresh = inventory.snapshot(host.home, repo_root=ROOT)
        current = {**fresh, "mcp": stale["mcp"]}
        results = steps.transition(current, {"accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes.get("mcp record retire"), "already_done",
                         json.dumps(results)[:900])
        self.assertEqual(host.record(), installed)

    def test_a_populated_override_is_refused_before_anything_is_removed(self):
        """A valid document at the override path passes every other reading, so only this catches it.

        The packaged launcher reads one fixed path and ignores this override, so the plugin-owned
        document would be written where no launcher looks. Discovered at the write, that is
        discovered after the working registration has been removed and both settings files moved
        aside: an aborted transition that leaves the host with no completion hook at all.
        """
        host = self.ready()
        elsewhere = host.root / "elsewhere.json"
        elsewhere.write_text(json.dumps(host.settings()), encoding="utf-8")
        before = (host.hooks_document(), host.settings(), host.record(), host.config())
        done = run([CLI, "--codex-home", host.home, "transition", "--apply",
                    "--accept-hook-trust-gap"],
                   env={**os.environ, "CRW_COMPLETION_HOOK_CONFIG": str(elsewhere)})
        answer = json.loads(done.stdout)
        self.assertEqual(done.returncode, 1)
        self.assertEqual(answer["results"][0]["outcome"], "refused")
        self.assertIn("CRW_COMPLETION_HOOK_CONFIG", answer["results"][0]["detail"])
        self.assertEqual({r["step"]: r["outcome"] for r in answer["results"][1:]},
                         {r["step"]: "not_reached" for r in answer["results"][1:]})
        # Nothing removed, nothing moved, and the override file itself untouched.
        self.assertEqual((host.hooks_document(), host.settings(), host.record(), host.config()),
                         before)
        self.assertEqual(sorted(host.home.glob("*.superseded-*")), [])
        self.assertTrue(elsewhere.is_file())

    def test_settings_registered_at_a_custom_path_are_recoverable_after_the_standdown(self):
        """A manual install made with the override records that path in its command forever.

        The variable need not still be set when the transition runs, so the archive has to land
        where the recovery looks. Archived beside itself, a rerun could not carry the marker root,
        the database or the journal forward and refused with the hook already removed.
        """
        host = self.host
        host.link_skills()
        custom = host.root / "custom-settings.json"
        host.register_hook()
        (host.home / "crw-completion-hook.json").rename(custom)
        document = host.hooks_document()
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        entry["command"] = entry["command"].rsplit(" ", 1)[0] + " " + str(custom)
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        host.register_mcp()
        host.install_plugin()
        wanted = json.loads(custom.read_text(encoding="utf-8"))

        # --dest, because the default settings are absent: that is the operator position this
        # finding describes, and it is where preflight accepts and the sequence proceeds.
        code, answer = host.call("--dest", str(host.destination), "transition", "--apply",
                                 "--accept-hook-trust-gap")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:2000])
        self.assertFalse(custom.exists())
        archives = sorted(host.home.glob("crw-completion-hook.json.superseded-*"))
        self.assertTrue(archives, "the archive did not land where the recovery looks")
        retire = [r for r in answer["results"] if r["step"] == "settings retire"][0]
        self.assertEqual(retire["retired"][0]["from"], str(custom))
        for field in ("markerRoot", "dbPath", "journalRoot"):
            self.assertEqual(host.settings()[field], wanted[field], field)

    def test_a_record_and_a_table_that_disagree_are_refused_rather_than_chosen_between(self):
        host = self.ready()
        # A real, runnable program at another path: without the comparison the record is simply
        # believed, a plugin record naming THIS is installed, and the table that current sessions
        # actually run is removed. An unrunnable path would only prove the executable check fires.
        other = host.root / "another-bridge"
        other.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        other.chmod(0o755)
        record = host.record()
        record["bridgeExecutable"] = str(other)
        (host.home / "crw-bridge-mcp.json").write_text(json.dumps(record), encoding="utf-8")
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("disagree about bridgeExecutable", answer["results"][0]["detail"])
        self.assertEqual(host.config(), before)

    def test_disable_reads_ownership_from_the_file_it_retires(self):
        """With the override set, the owner of another document decided this one's fate."""
        host = self.ready()
        host.transition("--apply")
        elsewhere = host.root / "elsewhere.json"
        elsewhere.write_text(json.dumps(
            {**host.settings(), "owner": "user", "adapterInterpreter": None,
             "adapterEntryPoint": None}), encoding="utf-8")
        done = run([CLI, "--codex-home", host.home, "disable", "--apply"],
                   env={**os.environ, "CRW_COMPLETION_HOOK_CONFIG": str(elsewhere)})
        answer = json.loads(done.stdout)
        self.assertEqual(done.returncode, 0, json.dumps(answer["results"], indent=2)[:1200])
        self.assertEqual([r["outcome"] for r in answer["results"]], ["settled", "settled"])
        self.assertIsNone(host.settings())
        self.assertIsNone(host.record())

    def test_positions_are_re_derived_so_a_hook_added_after_the_reading_is_not_popped(self):
        """document and again are both reads taken AFTER a change, so they agree while stale."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        stale = inventory.snapshot(host.home, repo_root=ROOT)
        document = host.hooks_document()
        foreign = {"type": "command", "command": "/bin/true"}
        document["hooks"]["Stop"][0]["hooks"].insert(0, foreign)
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        answer = steps.hook_standdown(stale, {"accept_hook_renumbering": True}, apply=True)
        self.assertEqual(answer["outcome"], "settled", json.dumps(answer)[:500])
        left = host.hooks_document()["hooks"]["Stop"][0]["hooks"]
        self.assertEqual(left, [foreign], "the stale index popped the wrong hook")

    def test_the_settings_the_registration_names_are_what_is_carried_forward(self):
        """A valid document at the fixed path must not override the one the hook actually reads."""
        host = self.host
        host.link_skills()
        host.register_hook()
        custom = host.root / "registered-settings.json"
        fixed = host.home / "crw-completion-hook.json"
        registered = json.loads(fixed.read_text(encoding="utf-8"))
        registered["markerRoot"] = str(host.marker / "registered")
        custom.write_text(json.dumps(registered), encoding="utf-8")
        document = host.hooks_document()
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        entry["command"] = entry["command"].rsplit(" ", 1)[0] + " " + str(custom)
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        # A DIFFERENT, perfectly valid document is left at the fixed path.
        unrelated = dict(registered)
        unrelated["markerRoot"] = str(host.marker / "unrelated")
        fixed.write_text(json.dumps(unrelated), encoding="utf-8")
        host.register_mcp()
        host.install_plugin()

        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:1500])
        self.assertEqual(host.settings()["markerRoot"], str(host.marker / "registered"))

    def test_an_archive_that_cannot_be_renamed_is_copied_across_the_boundary(self):
        """os.replace cannot cross a filesystem, and this one runs after the standdown."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import steps

        source = Path(self.directory) / "settings-on-another-volume.json"
        source.write_text('{"kept": true}', encoding="utf-8")
        home = Path(self.directory) / "home-for-archives"
        home.mkdir()
        real_replace = os.replace

        def refuse_to_rename(src, dst, *args, **keywords):
            raise OSError(errno.EXDEV, "Invalid cross-device link")

        os.replace = refuse_to_rename
        try:
            target = steps.retire(source, into=home, stem="crw-completion-hook.json")
        finally:
            os.replace = real_replace
        self.assertFalse(source.exists())
        self.assertTrue(Path(target).is_file())
        self.assertEqual(json.loads(Path(target).read_text(encoding="utf-8")), {"kept": True})
        self.assertEqual(Path(target).parent, home)

    def test_a_registration_that_reappears_after_the_standdown_is_reported(self):
        """Hook ownership spans two artifacts, so a concurrent install can still append."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        before = host.hooks_document()
        code, _ = host.transition("--apply")
        self.assertEqual(code, 0)
        # Exactly what a concurrent user-owned install leaves behind.
        (host.home / "hooks.json").write_text(json.dumps(before, indent=2), encoding="utf-8")
        answer = steps.hook_recheck(inventory.snapshot(host.home, repo_root=ROOT))
        self.assertEqual(answer["outcome"], "refused")
        self.assertIn("in the hook file again", answer["detail"])
        self.assertEqual(answer["identities"], ["user:Stop:0:0"])

    def test_consent_is_asked_again_for_a_hook_that_landed_after_the_reading(self):
        """The file is not locked when the first reading is taken."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        stale = inventory.snapshot(host.home, repo_root=ROOT)
        self.assertEqual(stale["hook"]["later"], [])
        document = host.hooks_document()
        document["hooks"]["Stop"][0]["hooks"].append({"type": "command", "command": "/bin/true"})
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        answer = steps.hook_standdown(stale, {}, apply=True)
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:400])
        self.assertEqual(answer["shiftedIdentities"], ["user:Stop:0:1"])
        self.assertEqual(host.hooks_document(), document)
        answer = steps.hook_standdown(stale, {"accept_hook_renumbering": True}, apply=True)
        self.assertEqual(answer["outcome"], "settled", json.dumps(answer)[:400])
        self.assertEqual(host.hooks_document()["hooks"]["Stop"][0]["hooks"],
                         [{"type": "command", "command": "/bin/true"}])

    def registered_at(self, host, custom_name, **overrides):
        """A manual install whose hook permanently names a settings file of its own."""
        host.link_skills()
        host.register_hook()
        fixed = host.home / "crw-completion-hook.json"
        registered = json.loads(fixed.read_text(encoding="utf-8"))
        registered.update(overrides)
        custom = host.root / custom_name
        custom.write_text(json.dumps(registered), encoding="utf-8")
        document = host.hooks_document()
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        entry["command"] = entry["command"].rsplit(" ", 1)[0] + " " + str(custom)
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        host.register_mcp()
        return custom, fixed

    def test_the_registered_document_is_what_preflight_validates(self):
        """A policy the plugin document cannot carry must refuse before the standdown."""
        host = self.host
        custom, fixed = self.registered_at(host, "registered.json", journalPolicy="faults_only")
        host.install_plugin()
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("journalPolicy", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)
        self.assertTrue(custom.is_file())

    def test_the_registered_archive_is_the_one_a_rerun_recovers(self):
        """Both files are archived under one stem, so the order decides what a retry rebuilds."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.host
        custom, fixed = self.registered_at(host, "registered.json",
                                           markerRoot=str(self.host.marker / "registered"))
        unrelated = json.loads(fixed.read_text(encoding="utf-8"))
        unrelated["markerRoot"] = str(host.marker / "unrelated")
        fixed.write_text(json.dumps(unrelated), encoding="utf-8")
        host.install_plugin()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        self.assertEqual(steps.hook_standdown(snapshot, {"accept_hook_trust_gap": True},
                                              apply=True)["outcome"], "settled")
        self.assertEqual(steps.settings_retire(snapshot, {}, apply=True)["outcome"], "settled")
        # Interrupted here: the next run has only the archives to work from.
        recovered, name = inventory.newest_retired(host.home)
        self.assertEqual(recovered["markerRoot"], str(host.marker / "registered"),
                         "recovered " + str(name))

    def test_an_alias_registered_while_the_run_is_in_flight_is_refused_in_the_lock(self):
        """The reading taken inside the ownership lock passes the same checks preflight applied."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        stale = inventory.snapshot(host.home, repo_root=ROOT)
        host.append_config('[mcp_servers.late-alias]\ncommand = "'
                           + str(host.destination / "current" / "bin" / "codex-thread-bridge")
                           + '"\n')
        before = host.config()
        results = steps.transition(stale, {"accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes.get("mcp record retire"), "refused", json.dumps(results)[:900])
        self.assertIn("late-alias", [item for item in results
                                     if item["step"] == "mcp record retire"][0]["detail"])
        self.assertIn("[mcp_servers.codex-thread-bridge]", host.config())
        self.assertEqual(host.config(), before)

    def test_the_destination_comes_from_the_registered_settings(self):
        """Deriving it from the fixed file silently moved the host onto another installation."""
        host = self.host
        other = host.root / "other-destination"
        (other / "versions" / "v1" / "bin").mkdir(parents=True)
        (other / "current").symlink_to(other / "versions" / "v1")
        for program in ("python3", "codex-session-relay", "codex-thread-bridge",
                        "crw-completion-hook"):
            path = other / "versions" / "v1" / "bin" / program
            if program == "python3":
                path.symlink_to(sys.executable)
                continue
            path.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            path.chmod(0o755)
        custom, fixed = self.registered_at(host, "registered.json")
        moved = json.loads(fixed.read_text(encoding="utf-8"))
        moved["relayExecutable"] = str(other / "current" / "bin" / "codex-session-relay")
        fixed.write_text(json.dumps(moved), encoding="utf-8")
        host.install_plugin()

        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:1500])
        self.assertEqual(answer["destination"], str(host.destination))
        self.assertTrue(host.settings()["adapterEntryPoint"].startswith(str(host.destination)))
        self.assertNotIn(str(other), json.dumps(host.settings()))

    def test_registrations_naming_documents_that_disagree_are_refused(self):
        """Two installations, and choosing by hook order configures one of them silently."""
        host = self.host
        custom, fixed = self.registered_at(host, "first.json",
                                           markerRoot=str(self.host.marker / "first"))
        second = json.loads(custom.read_text(encoding="utf-8"))
        second["markerRoot"] = str(host.marker / "second")
        other = host.root / "second.json"
        other.write_text(json.dumps(second), encoding="utf-8")
        document = host.hooks_document()
        entry = dict(document["hooks"]["Stop"][0]["hooks"][0])
        entry["command"] = entry["command"].rsplit(" ", 1)[0] + " " + str(other)
        document["hooks"]["Stop"][0]["hooks"].append(entry)
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        host.install_plugin()

        code, answer = host.transition("--apply", "--accept-hook-renumbering")
        self.assertEqual(code, 1)
        self.assertIn("disagree about", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), document)
        self.assertTrue(custom.is_file())
        self.assertTrue(other.is_file())

    def test_another_events_hooks_never_block_a_stop_standdown(self):
        """shifted_identities compares numbers, so its inventory has to be one event."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        document = host.hooks_document()
        document["hooks"]["SessionStart"] = [
            {"hooks": [{"type": "command", "command": "/bin/true"},
                       {"type": "command", "command": "/bin/false"}]}]
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        answer = steps.hook_standdown(snapshot, {}, apply=True)
        self.assertEqual(answer["outcome"], "settled", json.dumps(answer)[:400])
        self.assertEqual(host.hooks_document()["hooks"]["SessionStart"],
                         document["hooks"]["SessionStart"])

    def test_a_table_header_inside_a_string_is_not_a_plugin_registration(self):
        """A line-oriented scan reads the contents of a multiline string as structure."""
        host = self.host.manual_install()
        cache = host.home / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
        cache.parent.mkdir(parents=True, exist_ok=True)
        shutil.copytree(ROOT / "plugins" / "crw", cache, symlinks=False)
        host.append_config('[somewhere]\nnote = """\n[plugins."crw@crw"]\nenabled = true\n"""\n')
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("not registered", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_a_relative_bridge_command_is_refused_before_anything_is_retired(self):
        host = self.host
        host.link_skills()
        host.register_hook()
        run([RUNTIME, "register-mcp", "--owner", "user", "--codex-home", host.home,
             "--bridge-command", "codex-thread-bridge", "--apply"])
        host.install_plugin()
        before = (host.config(), host.settings(), host.record())
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("which is relative", answer["results"][0]["detail"])
        self.assertEqual((host.config(), host.settings(), host.record()), before)

    def test_a_relative_settings_argument_is_not_proven(self):
        """It resolves against whoever runs this command, not the workspace the hook fires from."""
        host = self.ready()
        document = host.hooks_document()
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        entry["command"] = entry["command"].rsplit(" ", 1)[0] + " settings.json"
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("cannot prove is its own adapter", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), document)

    def test_stopping_after_the_retire_still_leaves_the_custom_file_discoverable(self):
        """The custom path is recorded only in the command, so the archive goes first."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.host
        custom, fixed = self.registered_at(host, "registered.json",
                                           markerRoot=str(self.host.marker / "registered"))
        fixed.unlink()
        host.install_plugin()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        # The invariant this case is about is the retire running before the standdown, not
        # the retire being first overall: the non-destructive launcher step now precedes
        # both so that a refusal there takes nothing away.
        order = [name for name, _ in steps.ORDER]
        self.assertLess(order.index("settings retire"), order.index("hook standdown"))
        self.assertLess(order.index("stable launcher install"),
                        order.index("settings retire"))
        self.assertEqual(steps.settings_retire(snapshot, {}, apply=True)["outcome"], "settled")
        # Stopped here: the registration is still in place and the archive is already in the home.
        recovered, name = inventory.newest_retired(host.home)
        self.assertEqual(recovered["markerRoot"], str(host.marker / "registered"),
                         "recovered " + str(name))
        code, answer = host.call("--dest", str(host.destination), "transition", "--apply",
                                 "--accept-hook-trust-gap")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:1500])
        self.assertEqual(host.settings()["markerRoot"], str(host.marker / "registered"))

    def test_swap_state_does_not_report_a_host_record_it_did_not_read(self):
        """load() answers with a Reading, and a Reading is truthy for absent and unreadable alike."""
        host = self.ready()
        state = host.root / "state"
        state.mkdir()
        done = run([CLI, "--codex-home", host.home, "swap-state"],
                   env={**os.environ, "XDG_STATE_HOME": str(state)})
        answer = json.loads(done.stdout)
        self.assertEqual(done.returncode, 0)
        self.assertIn(str(state), answer["hostRecordPath"])
        self.assertIn(answer["recordedHostRecord"], (False, None))
        self.assertIsNotNone(answer["recordedDetail"])

    def test_a_plugin_disabled_after_preflight_stops_the_unlink(self):
        """preflight reads the replacement once; the removals happen afterwards."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        text = host.config().replace('[plugins."crw@crw"]\nenabled = true',
                                     '[plugins."crw@crw"]\nenabled = false')
        (host.home / "config.toml").write_text(text, encoding="utf-8")
        answer = steps.skill_unlink(snapshot, {}, apply=True)
        self.assertEqual(answer["outcome"], "refused")
        self.assertIn("disabled", answer["detail"])
        self.assertTrue(sorted((host.home / "skills").iterdir()))

    def test_a_link_replaced_after_the_inventory_is_left_alone(self):
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        replaced = host.home / "skills" / "crw-run"
        replaced.unlink()
        replaced.symlink_to(host.root)
        answer = steps.skill_unlink(snapshot, {}, apply=True)
        self.assertEqual(answer["outcome"], "refused")
        self.assertIn("crw-run", answer["detail"])
        self.assertTrue(replaced.is_symlink())
        self.assertEqual(replaced.resolve(), host.root.resolve())

    def test_two_registrations_differing_only_in_behaviour_are_refused(self):
        host = self.host
        custom, fixed = self.registered_at(host, "first.json")
        second = json.loads(custom.read_text(encoding="utf-8"))
        second["mode"] = "hold"
        second["isolationAssertedBy"] = "someone"
        other = host.root / "second.json"
        other.write_text(json.dumps(second), encoding="utf-8")
        document = host.hooks_document()
        entry = dict(document["hooks"]["Stop"][0]["hooks"][0])
        entry["command"] = entry["command"].rsplit(" ", 1)[0] + " " + str(other)
        document["hooks"]["Stop"][0]["hooks"].append(entry)
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        host.install_plugin()
        code, answer = host.transition("--apply", "--accept-hook-renumbering")
        self.assertEqual(code, 1)
        self.assertIn("disagree about", answer["results"][0]["detail"])

    def test_an_empty_live_argument_list_is_not_refilled_from_history(self):
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        superseded = host.home / "crw-bridge-mcp.json.superseded-20200101T000000Z"
        record = dict(host.record())
        record["args"] = ["--from-history"]
        superseded.write_text(json.dumps(record), encoding="utf-8")
        (host.home / "crw-bridge-mcp.json").unlink()
        answer = steps.mcp_record_install(inventory.snapshot(host.home, repo_root=ROOT), {},
                                          apply=True)
        self.assertIn(answer["outcome"], ("settled", "already_done"), json.dumps(answer)[:400])
        self.assertEqual(host.record()["args"], [])

    def test_the_preserved_receipt_names_the_carried_document(self):
        host = self.host
        custom, fixed = self.registered_at(host, "registered.json",
                                           markerRoot=str(self.host.marker / "registered"))
        unrelated = json.loads(fixed.read_text(encoding="utf-8"))
        unrelated["markerRoot"] = str(host.marker / "unrelated")
        fixed.write_text(json.dumps(unrelated), encoding="utf-8")
        host.install_plugin()
        code, answer = host.transition()
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:1200])
        self.assertEqual(answer["preserved"]["markerRoot"], str(host.marker / "registered"))

    def test_two_marketplaces_registering_the_plugin_are_refused(self):
        """Codex identifies an installation by plugin@marketplace, and both declarations load."""
        host = self.ready()
        host.append_config('[plugins."crw@another"]\nenabled = true\n')
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("more than one marketplace", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_a_registration_whose_settings_cannot_be_read_is_not_replaced_by_another(self):
        host = self.host
        custom, fixed = self.registered_at(host, "registered.json")
        custom.write_text("{ not json", encoding="utf-8")
        host.install_plugin()
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("could not be read", answer["results"][0]["detail"])
        self.assertIn("registered.json", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)
        self.assertTrue(fixed.is_file())

    def test_settings_written_after_the_reading_are_not_archived(self):
        """The lock serialises the rename; it does not make an older proof current."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        fresh = dict(host.settings())
        fresh["markerRoot"] = str(host.marker / "written-by-somebody-else")
        (host.home / "crw-completion-hook.json").write_text(json.dumps(fresh), encoding="utf-8")
        answer = steps.settings_retire(snapshot, {}, apply=True)
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:400])
        self.assertIn("changed after it was read", answer["detail"])
        self.assertEqual(json.loads(
            (host.home / "crw-completion-hook.json").read_text(encoding="utf-8")), fresh)
        self.assertEqual(sorted(host.home.glob("crw-completion-hook.json.superseded-*")), [])

    def test_a_relative_destination_is_settled_before_anything_is_derived(self):
        """A plugin-owned record needs an absolute path, and --dest feeds every derived path."""
        host = self.host
        host.link_skills()
        host.register_hook()
        host.register_mcp()
        (host.home / "crw-bridge-mcp.json").unlink()
        # No live table and no record at all, which is the state this fallback exists for.
        lines, keep = host.config().splitlines(), []
        skipping = False
        for line in lines:
            if line.strip().startswith("["):
                skipping = line.strip().startswith("[mcp_servers.")
            if not skipping:
                keep.append(line)
        (host.home / "config.toml").write_text("\n".join(keep) + "\n", encoding="utf-8")
        host.install_plugin()
        relative = os.path.relpath(host.destination, host.root)
        done = run([CLI, "--codex-home", host.home, "--dest", relative, "transition", "--apply",
                    "--accept-hook-trust-gap"], cwd=host.root)
        answer = json.loads(done.stdout)
        self.assertEqual(done.returncode, 0, json.dumps(answer["results"], indent=2)[:1500])
        self.assertEqual(answer["destination"], str(host.destination.resolve()))
        self.assertTrue(os.path.isabs(host.record()["bridgeExecutable"]))
        self.assertTrue(os.path.isabs(host.settings()["adapterEntryPoint"]))

    def test_two_live_owners_with_different_settings_are_refused(self):
        """The plugin owns the fixed path while a registration still reads a user-owned document."""
        host = self.host
        custom, fixed = self.registered_at(host, "registered.json",
                                           markerRoot=str(self.host.marker / "manual"))
        plugin_owned = json.loads(fixed.read_text(encoding="utf-8"))
        plugin_owned.update({"owner": "plugin", "markerRoot": str(host.marker / "plugin"),
                             "adapterInterpreter": str(host.destination / "current" / "bin"
                                                       / "python3"),
                             "adapterEntryPoint": str(host.destination / "current" / "bin"
                                                      / "crw-completion-hook")})
        fixed.write_text(json.dumps(plugin_owned), encoding="utf-8")
        host.install_plugin()
        before = host.hooks_document()

        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("Two owners are live here", answer["results"][0]["detail"])
        self.assertIn("markerRoot", answer["results"][0]["detail"])
        # Nothing removed, and the manual document is still there to identify the installation.
        self.assertEqual(host.hooks_document(), before)
        self.assertTrue(custom.is_file())
        self.assertEqual(json.loads(custom.read_text(encoding="utf-8"))["markerRoot"],
                         str(host.marker / "manual"))

    def test_a_later_settings_change_leaves_the_earlier_files_in_place(self):
        """Validating and moving in one pass archived the earlier paths before the refusal."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.host
        custom, fixed = self.registered_at(host, "registered.json")
        host.install_plugin()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        # Two paths are in play: the fixed one and the registered one, in that order, so the
        # registered file is the one validated after the fixed one would already have been
        # archived by the old single pass.
        self.assertEqual(steps.settings_retire(snapshot, {}, apply=False)["paths"],
                         [str(fixed), str(custom)])
        changed = json.loads(custom.read_text(encoding="utf-8"))
        changed["markerRoot"] = str(host.marker / "written-by-somebody-else")
        custom.write_text(json.dumps(changed), encoding="utf-8")
        answer = steps.settings_retire(snapshot, {}, apply=True)
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:400])
        self.assertEqual(answer["retired"], [])
        self.assertTrue(fixed.is_file(), "the earlier file was archived before the refusal")
        self.assertTrue(custom.is_file())
        self.assertEqual(sorted(host.home.glob("crw-completion-hook.json.superseded-*")), [])

    def test_a_plugin_removed_after_preflight_stops_the_first_removal_too(self):
        """The earlier steps take things away as well, so the check is asked before each of them."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        # Disabled rather than deleted, so preflight's own payload check -- which runs live -- is
        # not what catches it: the point is the guard in front of each destructive step.
        text = host.config().replace('[plugins."crw@crw"]\nenabled = true',
                                     '[plugins."crw@crw"]\nenabled = false')
        (host.home / "config.toml").write_text(text, encoding="utf-8")
        before = (host.hooks_document(), host.settings(), host.record(), host.config())
        results = steps.transition(snapshot, {"accept_hook_renumbering": False,
                                              "accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["settings retire"], "refused", json.dumps(results)[:700])
        refusal = [item for item in results if item["step"] == "settings retire"][0]
        self.assertIn("disabled", refusal["detail"])
        self.assertEqual(outcomes["hook standdown"], "not_reached")
        self.assertEqual(outcomes["skill unlink"], "not_reached")
        self.assertEqual((host.hooks_document(), host.settings(), host.record(), host.config()),
                         before)
        self.assertEqual(sorted(host.home.glob("*.superseded-*")), [])

    def test_a_record_that_became_user_owned_after_the_snapshot_is_not_retired(self):
        """disable decides the bridge record from the record it is about to move, not a snapshot."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        self.assertEqual(host.transition("--apply")[0], 0)
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        self.assertEqual(snapshot["mcp"]["recordOwner"], "plugin")
        # A supported register-mcp --owner user landing between the snapshot and the retire.
        path = host.home / "crw-bridge-mcp.json"
        document = json.loads(path.read_text(encoding="utf-8"))
        document["owner"] = "user"
        path.write_text(json.dumps(document), encoding="utf-8")
        before = path.read_text(encoding="utf-8")
        # The transition's own retire of the manual record already archived one, so the question
        # is whether THIS call archived another, not whether the home is free of archives.
        archives = sorted(host.home.glob("crw-bridge-mcp.json.superseded-*"))
        results = steps.disable(snapshot, {}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["bridge record"], "refused", json.dumps(results)[:700])
        self.assertEqual(path.read_text(encoding="utf-8"), before)
        self.assertEqual(sorted(host.home.glob("crw-bridge-mcp.json.superseded-*")), archives)

    def test_disable_takes_the_lock_register_mcp_takes(self):
        """Which lock, by path: a held ownership lock makes the bridge record answer busy.

        The record file's own lock would not catch this. The user-owned side writes the Codex
        configuration, so the lock that serialises the two owners is the shared one, and this is
        the check that says the shared one is the lock this command actually takes.
        """
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import bridgerecord, hostrecord
        from crw_transition import inventory, steps

        host = self.ready()
        self.assertEqual(host.transition("--apply")[0], 0)
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        path = host.home / "crw-bridge-mcp.json"
        before = path.read_text(encoding="utf-8")
        held = Path(str(bridgerecord.ownership_lock_path(host.home)) + hostrecord.LOCK_SUFFIX)
        held.write_text("1", encoding="utf-8")
        self.addCleanup(held.unlink, missing_ok=True)
        # Shrunk the way the constant's own comment invites, so the case costs a fraction of a
        # second rather than the ten this would otherwise wait.
        original = hostrecord.LOCK_TIMEOUT_SECONDS
        hostrecord.LOCK_TIMEOUT_SECONDS = 0.2
        self.addCleanup(setattr, hostrecord, "LOCK_TIMEOUT_SECONDS", original)
        results = steps.disable(snapshot, {}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["bridge record"], "busy", json.dumps(results)[:700])
        self.assertEqual(path.read_text(encoding="utf-8"), before)

    def test_a_dry_run_does_not_report_a_surface_as_stopped(self):
        """The receipt separates what is in effect from what an --apply would do."""
        host = self.ready()
        self.assertEqual(host.transition("--apply")[0], 0)
        code, answer = host.call("disable")
        self.assertEqual(code, 0)
        self.assertEqual(answer["stops"], [])
        self.assertEqual(len(answer["wouldStop"]), 2, json.dumps(answer["wouldStop"]))
        self.assertEqual(answer["stillLive"], [])
        self.assertIsNotNone(host.settings())
        self.assertIsNotNone(host.record())

    def test_a_refused_retire_leaves_its_claim_under_still_live(self):
        """A manual install owns both records, so disable stops nothing and never says it did."""
        host = self.ready()
        code, answer = host.call("disable", "--apply")
        self.assertEqual(code, 1)
        self.assertFalse([item for item in answer["stops"] if "adapter invocations" in item],
                         json.dumps(answer["stops"]))
        self.assertTrue([item for item in answer["stillLive"] if "adapter invocations" in item],
                        json.dumps(answer["stillLive"]))
        self.assertTrue(all("NOT stopped" in item for item in answer["stillLive"]))
        self.assertIsNotNone(host.settings())

    def test_a_field_appended_to_the_table_after_the_snapshot_is_not_orphaned(self):
        """Authorship is equality with the whole table, so the span is re-derived under the lock."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        self.assertTrue(snapshot["mcp"]["tableProven"])
        span = snapshot["mcp"]["tableSpan"].strip()
        self.assertIn(span, host.config())
        # An operator adding a field to the same table between the snapshot and the lock. The old
        # span is still a substring of the file, which is exactly why containment cannot decide it.
        (host.home / "config.toml").write_text(
            host.config().replace(span, span + "\nstartup_timeout_sec = 30", 1), encoding="utf-8")
        before = host.config()
        answer = steps.mcp_table_standdown(snapshot, {}, apply=True)
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:600])
        self.assertEqual(host.config(), before)
        self.assertIn("startup_timeout_sec = 30", host.config())

    def test_a_second_marketplace_entry_after_the_snapshot_stops_the_removal(self):
        """The cardinality preflight refuses on is answered again before anything is removed.

        A behaviour guard rather than a guard over one line. The re-check already refused, through
        the cache cell: a second marketplace leaves no single installed version nameable, so
        read_plugin answers cacheVersion None and plugin_refusals refuses on that. What the
        explicit cardinality line adds is a refusal that says the cause instead of leaving a
        reading about the cache to carry it.
        """
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        host.append_config('[plugins."crw@another"]\nenabled = true\n')
        before = (host.hooks_document(), host.settings(), host.record())
        results = steps.transition(snapshot, {"accept_hook_renumbering": False,
                                              "accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["settings retire"], "refused", json.dumps(results)[:700])
        refusal = [item for item in results if item["step"] == "settings retire"][0]
        self.assertIn("crw@another", refusal["detail"])
        self.assertIn("more than one marketplace", refusal["detail"])
        self.assertEqual(outcomes["skill unlink"], "not_reached")
        self.assertEqual((host.hooks_document(), host.settings(), host.record()), before)

    def test_a_registration_reading_the_default_path_is_compared_too(self):
        """A two-word command names no settings file; it still reads one, and that one counts."""
        host = self.host
        custom, fixed = self.registered_at(host, "registered.json",
                                           markerRoot=str(self.host.marker / "registered"))
        other = json.loads(fixed.read_text(encoding="utf-8"))
        other["markerRoot"] = str(host.marker / "default-path")
        fixed.write_text(json.dumps(other), encoding="utf-8")
        document = host.hooks_document()
        entry = dict(document["hooks"]["Stop"][0]["hooks"][0])
        # The same adapter, registered without the third word: it resolves the document itself.
        entry["command"] = entry["command"].rsplit(" ", 1)[0]
        document["hooks"]["Stop"][0]["hooks"].append(entry)
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        host.install_plugin()
        before = host.hooks_document()
        code, answer = host.transition("--apply", "--accept-hook-renumbering")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("disagree about", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)
        self.assertTrue(custom.is_file())
        self.assertEqual(json.loads(fixed.read_text(encoding="utf-8")), other)

    def test_a_plugin_owned_record_is_compared_with_the_live_table_too(self):
        """Both surfaces are live whoever wrote the record, so both are compared."""
        host = self.ready()
        record = host.home / "crw-bridge-mcp.json"
        document = json.loads(record.read_text(encoding="utf-8"))
        document["owner"] = "plugin"
        document["args"] = ["--alias"]
        record.write_text(json.dumps(document), encoding="utf-8")
        before = (host.config(), host.hooks_document(), host.settings(), host.record())
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("disagree about", answer["results"][0]["detail"])
        self.assertIn("args", answer["results"][0]["detail"])
        self.assertEqual((host.config(), host.hooks_document(), host.settings(), host.record()),
                         before)

    def test_a_bridge_that_stopped_existing_after_preflight_is_not_installed(self):
        """register-mcp writes the command it is given without requiring it to be there."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        missing = str(host.root / "gone" / "codex-thread-bridge")
        was = str(host.destination / "current" / "bin" / "codex-thread-bridge")
        # Both surfaces moved together, so the divergence check is not what answers this.
        record = host.home / "crw-bridge-mcp.json"
        document = json.loads(record.read_text(encoding="utf-8"))
        document["bridgeExecutable"] = missing
        record.write_text(json.dumps(document), encoding="utf-8")
        (host.home / "config.toml").write_text(host.config().replace(was, missing),
                                               encoding="utf-8")
        table, kept = host.config(), host.record()
        results = steps.transition(snapshot, {"accept_hook_renumbering": False,
                                              "accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["mcp record retire"], "refused", json.dumps(results)[:900])
        refusal = [item for item in results if item["step"] == "mcp record retire"][0]
        self.assertIn("is not an executable file", refusal["detail"])
        self.assertEqual(outcomes["mcp table standdown"], "not_reached")
        self.assertEqual((host.config(), host.record()), (table, kept))

    def test_a_payload_replaced_after_preflight_stops_the_first_removal(self):
        """preflight runs the payload contract live, so the window it names starts after it."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        self.assertEqual(steps.plugin_refusals(snapshot), [])
        (Path(snapshot["plugin"]["cacheVersion"]) / "wiring" / "mcp.json").unlink()
        # Stubbed deliberately: preflight re-runs the contract live, so the only way to be in the
        # window this is about is to have passed it before the cache was replaced.
        original = steps.preflight
        steps.preflight = lambda host, options: steps._answer(
            "preflight", steps.SETTLED, "stubbed: this case is about the window after preflight")
        self.addCleanup(setattr, steps, "preflight", original)
        before = (host.hooks_document(), host.settings(), host.config())
        results = steps.transition(snapshot, {"accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["settings retire"], "refused", json.dumps(results)[:700])
        refusal = [item for item in results if item["step"] == "settings retire"][0]
        self.assertIn("--payload", refusal["detail"])
        self.assertEqual((host.hooks_document(), host.settings(), host.config()), before)

    def test_an_adapter_replaced_after_the_snapshot_stops_the_standdown(self):
        """A command string names a file; it does not say what is in it."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        # A registration pointing at a second checkout, so the bytes under it can change without
        # this repository's own adapter being touched.
        other = Path(self.directory) / "checkout"
        (other / "scripts" / "crw_runtime").mkdir(parents=True)
        (other / "plugins" / "crw" / ".codex-plugin").mkdir(parents=True)
        (other / "plugins" / "crw" / ".codex-plugin" / "plugin.json").write_text(
            "{}", encoding="utf-8")
        (other / "scripts" / "crw_runtime" / "completion.py").write_text("", encoding="utf-8")
        adapter = other / "scripts" / "completion_hook.py"
        shutil.copy2(ROOT / "scripts" / "completion_hook.py", adapter)
        document = host.hooks_document()
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        entry["command"] = entry["command"].replace(
            str(ROOT / "scripts" / "completion_hook.py"), str(adapter))
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        self.assertTrue(all(item["proven"] for item in snapshot["hook"]["entries"]),
                        json.dumps(snapshot["hook"]["entries"])[:600])
        adapter.write_text(adapter.read_text(encoding="utf-8") + "\n# somebody else's edit\n",
                           encoding="utf-8")
        before = host.hooks_document()
        answer = steps.hook_standdown(snapshot, {"accept_hook_renumbering": True}, apply=True)
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:600])
        self.assertIn("no longer this repository's own adapter", answer["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_a_link_that_appeared_after_the_snapshot_is_removed_too(self):
        """The skills directory has no lock and had no recheck, so it is read again at the step."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        # A skill the installed package does carry, so what this measures is the re-inventory and
        # not the replacement check below it.
        late = host.home / "skills" / "crw-run"
        target = late.resolve()
        late.unlink()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        late.symlink_to(target)
        answer = steps.skill_unlink(snapshot, {}, apply=True)
        self.assertEqual(answer["outcome"], "settled", json.dumps(answer)[:600])
        self.assertIn(str(late), answer["removed"])
        self.assertFalse(late.is_symlink())
        left = [p.name for p in (host.home / "skills").iterdir()
                if p.name.startswith("crw-")]
        self.assertEqual(left, [], json.dumps(left))

    def test_a_late_link_the_package_does_not_carry_is_not_removed(self):
        """The replacement requirement applies to the set being removed, not to the snapshot."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        late = host.home / "skills" / "crw-elsewhere"
        late.symlink_to(ROOT / "plugins" / "crw" / "skills" / "crw-run")
        before = {p.name for p in (host.home / "skills").iterdir()}
        answer = steps.skill_unlink(snapshot, {}, apply=True)
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:600])
        self.assertIn("crw-elsewhere", answer["detail"])
        # Preserve both the installed set and the late arrival as the catalog grows.
        self.assertEqual({p.name for p in (host.home / "skills").iterdir()}, before)

    def test_an_unreadable_skill_inventory_is_not_read_as_an_empty_one(self):
        """crwOwned == [] means "none there" or "not read", and those are different hosts."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        original = inventory.read_skill_links
        inventory.read_skill_links = lambda home, root: {
            "crwOwned": [], "foreign": [], "unreadable": "TimeoutExpired: install.py --check"}
        self.addCleanup(setattr, inventory, "read_skill_links", original)
        answer = steps.skill_unlink(snapshot, {}, apply=True)
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:400])
        self.assertIn("could not be inventoried", answer["detail"])
        self.assertTrue((host.home / "skills" / "crw-run").is_symlink())

    def test_a_settings_path_that_is_a_symlink_is_refused_before_anything_moves(self):
        """An archive is made by renaming, and renaming a link archives the link."""
        host = self.host
        custom, fixed = self.registered_at(host, "registered.json")
        actual = host.root / "actual.json"
        shutil.move(str(custom), str(actual))
        custom.symlink_to(Path(actual.name))
        host.install_plugin()
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        retire = [item for item in answer["results"] if item["step"] == "settings retire"][0]
        self.assertEqual(retire["outcome"], "refused")
        self.assertIn("is a symlink", retire["detail"])
        self.assertEqual(host.hooks_document(), before)
        self.assertTrue(custom.is_symlink())
        self.assertTrue(actual.is_file())
        self.assertEqual(sorted(host.home.glob("*.superseded-*")), [])

    def test_a_contended_lock_is_reported_against_the_step_that_took_it(self):
        """Every step takes a lock of its own, so the receipt names the step, not the last lock."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import hostrecord
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        held = Path(str(host.home / "crw-completion-hook.json") + hostrecord.LOCK_SUFFIX)
        held.write_text("1", encoding="utf-8")
        self.addCleanup(held.unlink, missing_ok=True)
        original = hostrecord.LOCK_TIMEOUT_SECONDS
        hostrecord.LOCK_TIMEOUT_SECONDS = 0.2
        self.addCleanup(setattr, hostrecord, "LOCK_TIMEOUT_SECONDS", original)
        results = steps.transition(snapshot, {"accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes.get("settings retire"), "busy", json.dumps(results)[:700])
        self.assertNotIn("mcp ownership lock", outcomes)
        self.assertEqual(outcomes["hook standdown"], "not_reached")
        self.assertEqual(outcomes["skill unlink"], "not_reached")
        self.assertIsNotNone(host.settings())

    def test_a_hand_edited_entry_naming_the_packaged_adapter_is_reported(self):
        """The detector walked an inventory that carries no command, so it never fired."""
        host = self.ready()
        document = host.hooks_document()
        document["hooks"]["Stop"].append({"hooks": [{
            "type": "command",
            "command": str(host.version / "bin" / "crw-completion-hook"),
        }]})
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("names the packaged adapter", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), document)

    def test_a_plugin_entry_without_enabled_is_not_read_as_enabled(self):
        """Absent is not true: whether Codex loads the replacement was never established."""
        host = self.ready()
        (host.home / "config.toml").write_text(
            host.config().replace('[plugins."crw@crw"]\nenabled = true',
                                  '[plugins."crw@crw"]'), encoding="utf-8")
        before = (host.hooks_document(), host.settings(), host.record())
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("does not record enabled = true", answer["results"][0]["detail"])
        self.assertEqual((host.hooks_document(), host.settings(), host.record()), before)

    def test_an_installer_that_inspected_nothing_is_not_a_readable_inventory(self):
        """A nonzero exit is ordinary here; a nonzero exit that printed nothing is not."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory

        host = self.ready()
        broken = Path(self.directory) / "fakerepo" / "scripts"
        broken.mkdir(parents=True)
        (broken / "install.py").write_text("import sys\nsys.exit(2)\n", encoding="utf-8")
        answer = inventory.read_skill_links(host.home, broken.parent)
        self.assertTrue(answer["unreadable"], json.dumps(answer)[:400])
        self.assertIn("exited 2", answer["unreadable"])
        # Ownership is still decided by reading the directory, not by the installer's verdict.
        self.assertTrue([item for item in answer["crwOwned"] if item["path"].endswith("crw-run")])

    def test_a_link_replaced_late_leaves_every_other_link_in_place(self):
        """Every proof first, then every removal: a refusal must not dismantle half of it."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        replaced = host.home / "skills" / "crw-run"
        replaced.unlink()
        replaced.symlink_to(host.root)
        before = {p.name for p in (host.home / "skills").iterdir()}
        answer = steps.skill_unlink(snapshot, {}, apply=True)
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:500])
        self.assertEqual(answer["removed"], [])
        self.assertFalse(answer["applied"])
        self.assertEqual({p.name for p in (host.home / "skills").iterdir()}, before)

    def test_a_plugin_record_that_could_not_be_rewritten_stops_before_the_table(self):
        """Absent and empty arguments are one registration here and two to the record writer."""
        host = self.ready()
        path = host.home / "crw-bridge-mcp.json"
        document = json.loads(path.read_text(encoding="utf-8"))
        document["owner"] = "plugin"
        document.pop("args", None)
        path.write_text(json.dumps(document), encoding="utf-8")
        before = (host.config(), host.record(), host.hooks_document(), host.settings())
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("is not the one this would write", answer["results"][0]["detail"])
        self.assertEqual((host.config(), host.record(), host.hooks_document(), host.settings()),
                         before)

    def test_an_adapter_removed_after_preflight_stops_the_first_removal(self):
        """The settings about to be written name it, so the probe runs again before they are."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        self.assertEqual(steps.plugin_refusals(snapshot), [])
        (host.version / "bin" / "crw-completion-hook").unlink()
        # Stubbed for the same reason the payload case stubs it: preflight probes the adapter
        # live, so the window this is about begins after preflight passed.
        original = steps.preflight
        steps.preflight = lambda host, options: steps._answer(
            "preflight", steps.SETTLED, "stubbed: this case is about the window after preflight")
        self.addCleanup(setattr, steps, "preflight", original)
        before = (host.hooks_document(), host.settings())
        results = steps.transition(snapshot, {"accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["settings retire"], "refused", json.dumps(results)[:700])
        refusal = [item for item in results if item["step"] == "settings retire"][0]
        self.assertIn("crw-completion-hook", refusal["detail"])
        self.assertEqual((host.hooks_document(), host.settings()), before)

    def test_a_link_replaced_during_the_removals_is_not_unlinked(self):
        """Nothing locks this directory, so each path is proved again immediately before it goes."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        victim = host.home / "skills" / "crw-run"
        seen = {}
        original = inventory.checkout_of

        def counting(path):
            """Replace a later link the moment the removal pass starts on an earlier one."""
            key = str(path)
            seen[key] = seen.get(key, 0) + 1
            # Three readers ask about each path in turn: the fresh inventory, the proof pass, and
            # the last look before the unlink. The third is the one this case has to land in.
            if seen[key] == 3 and victim.is_symlink() and key != str(victim):
                victim.unlink()
                victim.symlink_to(host.root)
            return original(path)

        inventory.checkout_of = counting
        self.addCleanup(setattr, inventory, "checkout_of", original)
        answer = steps.skill_unlink(snapshot, {}, apply=True)
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:500])
        self.assertIn("was replaced while these links were being removed", answer["detail"])
        self.assertNotIn(str(victim), answer["removed"])
        self.assertTrue(victim.is_symlink())
        self.assertEqual(victim.resolve(), host.root.resolve())

    def test_an_entry_naming_the_packaged_adapter_appended_late_is_reported(self):
        """The recheck asked about our own registrations only, so the other half went unseen."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        self.assertEqual(host.transition("--apply")[0], 0)
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        document = host.hooks_document()
        document["hooks"].setdefault("Stop", []).append({"hooks": [{
            "type": "command",
            "command": str(host.version / "bin" / "crw-completion-hook"),
        }]})
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        answer = steps.hook_recheck(snapshot)
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:500])
        self.assertIn("naming the packaged adapter or the destination", answer["detail"])
        self.assertEqual(host.hooks_document(), document)

    def test_a_standdown_that_refuses_puts_the_settings_back(self):
        """The retire happens for the standdown; if that will not happen, it is undone."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        settings = host.settings()
        # A foreign hook appended into the same matcher group after the snapshot: preflight saw
        # nothing to renumber, and the standdown re-derives it and asks for consent it was never
        # given.
        document = host.hooks_document()
        document["hooks"]["Stop"][0]["hooks"].append({"type": "command", "command": "/bin/true"})
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        results = steps.transition(snapshot, {"accept_hook_renumbering": False,
                                              "accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["hook standdown"], "refused", json.dumps(results)[:800])
        standdown = [item for item in results if item["step"] == "hook standdown"][0]
        # The host first: the document the still-registered adapter reads has to be back at its
        # own path, and the receipt saying so comes after.
        self.assertEqual(host.settings(), settings, json.dumps(standdown)[:600])
        self.assertEqual(sorted(host.home.glob("crw-completion-hook.json.superseded-*")), [])
        self.assertTrue(standdown.get("settingsRestored"), json.dumps(standdown)[:600])
        self.assertEqual(host.hooks_document(), document)

    def test_the_receipt_prints_all_three_windows(self):
        """The documentation promises three; the receipt listed two."""
        host = self.ready()
        code, answer = host.transition()
        self.assertEqual(code, 0)
        self.assertEqual(len(answer["windows"]), 3, json.dumps(answer["windows"]))
        self.assertTrue([item for item in answer["windows"]
                         if "releases the turn without recording" in item],
                        json.dumps(answer["windows"]))

    def test_settings_that_appeared_after_the_reading_are_not_archived(self):
        """A missing prior reading is not permission: it means this file was never proved."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.host
        custom, fixed = self.registered_at(host, "registered.json")
        document = json.loads(fixed.read_text(encoding="utf-8"))
        fixed.unlink()
        host.install_plugin()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        # A supported installer writing the fixed path between the reading and the retire.
        document["markerRoot"] = str(host.marker / "somebody-else")
        fixed.write_text(json.dumps(document), encoding="utf-8")
        answer = steps.settings_retire(snapshot, {}, apply=True)
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:600])
        self.assertIn("appeared after the reading", answer["detail"])
        self.assertEqual(json.loads(fixed.read_text(encoding="utf-8")), document)
        self.assertTrue(custom.is_file())
        self.assertEqual(sorted(host.home.glob("crw-completion-hook.json.superseded-*")), [])

    def test_a_rollback_takes_the_lock_that_path_is_written_under(self):
        """Asking whether the path is free and then writing it is a race with its writer."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import hostrecord
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        retired = steps.settings_retire(snapshot, {}, apply=True)
        self.assertEqual(retired["outcome"], "settled", json.dumps(retired)[:400])
        origin = Path(retired["retired"][0]["from"])
        archive = Path(retired["retired"][0]["to"])
        held = Path(str(origin) + hostrecord.LOCK_SUFFIX)
        held.write_text("1", encoding="utf-8")
        self.addCleanup(held.unlink, missing_ok=True)
        original = hostrecord.LOCK_TIMEOUT_SECONDS
        hostrecord.LOCK_TIMEOUT_SECONDS = 0.2
        self.addCleanup(setattr, hostrecord, "LOCK_TIMEOUT_SECONDS", original)
        restored, kept = steps._restore_retired([retired])
        self.assertEqual(restored, [])
        self.assertTrue([item for item in kept if "Busy" in item], json.dumps(kept))
        self.assertTrue(archive.is_file())

    def test_a_rollback_across_filesystems_still_restores(self):
        """retire crosses a filesystem by copying, and the way back has to as well."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        settings = host.settings()
        retired = steps.settings_retire(snapshot, {}, apply=True)
        origin = Path(retired["retired"][0]["from"])
        self.assertFalse(origin.exists())
        # The layout retire already handles, forced here rather than mounted: the first rename
        # answers EXDEV the way a cross-device rename does.
        genuine, refused = os.replace, []

        def crossing(source, target):
            if not refused:
                refused.append((source, target))
                raise OSError(errno.EXDEV, "Invalid cross-device link")
            return genuine(source, target)

        os.replace = crossing
        self.addCleanup(setattr, os, "replace", genuine)
        restored, kept = steps._restore_retired([retired])
        self.assertEqual(kept, [], json.dumps(kept))
        self.assertEqual(restored, [str(origin)])
        self.assertTrue(refused, "the cross-device path was not the one taken")
        self.assertEqual(host.settings(), settings)
        self.assertEqual(sorted(host.home.glob("crw-completion-hook.json.superseded-*")), [])

    def test_a_recheck_refusal_after_the_retire_puts_the_settings_back(self):
        """The retire is made for the standdown, so every way of not reaching it undoes it."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        settings = host.settings()
        document = host.hooks_document()
        calls = []
        original = steps.plugin_refusals

        def failing(host_view):
            """Usable for the retire, gone by the standdown: the plugin removed in between."""
            calls.append(1)
            return [] if len(calls) == 1 else ["the plugin entry crw@crw is disabled"]

        steps.plugin_refusals = failing
        self.addCleanup(setattr, steps, "plugin_refusals", original)
        results = steps.transition(snapshot, {"accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["settings retire"], "settled", json.dumps(results)[:700])
        self.assertEqual(outcomes["hook standdown"], "refused")
        self.assertEqual(host.settings(), settings)
        self.assertEqual(host.hooks_document(), document)
        self.assertEqual(sorted(host.home.glob("crw-completion-hook.json.superseded-*")), [])

    def test_a_busy_hook_lock_after_the_retire_puts_the_settings_back(self):
        """A lock another run holds is one more way of not reaching the standdown."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import hostrecord
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        settings = host.settings()
        held = Path(str(host.home / "hooks.json") + hostrecord.LOCK_SUFFIX)
        held.write_text("1", encoding="utf-8")
        self.addCleanup(held.unlink, missing_ok=True)
        original = hostrecord.LOCK_TIMEOUT_SECONDS
        hostrecord.LOCK_TIMEOUT_SECONDS = 0.2
        self.addCleanup(setattr, hostrecord, "LOCK_TIMEOUT_SECONDS", original)
        results = steps.transition(snapshot, {"accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["settings retire"], "settled", json.dumps(results)[:700])
        self.assertEqual(outcomes["hook standdown"], "busy")
        self.assertEqual(host.settings(), settings)
        self.assertEqual(sorted(host.home.glob("crw-completion-hook.json.superseded-*")), [])

    def test_a_rollback_copy_that_fails_leaves_no_partial_settings(self):
        """A half-written document at the live path is what the hook would then read."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        retired = steps.settings_retire(snapshot, {}, apply=True)
        origin = Path(retired["retired"][0]["from"])
        archive = Path(retired["retired"][0]["to"])
        genuine_replace, genuine_copy = os.replace, shutil.copy2

        def crossing(source, target):
            raise OSError(errno.EXDEV, "Invalid cross-device link")

        def failing(source, target, **keywords):
            Path(target).write_text("half a doc", encoding="utf-8")
            raise OSError(errno.ENOSPC, "No space left on device")

        os.replace, shutil.copy2 = crossing, failing
        self.addCleanup(setattr, shutil, "copy2", genuine_copy)
        self.addCleanup(setattr, os, "replace", genuine_replace)
        restored, kept = steps._restore_retired([retired])
        os.replace, shutil.copy2 = genuine_replace, genuine_copy
        self.assertEqual(restored, [])
        self.assertTrue([item for item in kept if "ENOSPC" in item or "No space" in item],
                        json.dumps(kept))
        self.assertFalse(origin.exists(), "a partial document was left where the hook reads")
        self.assertTrue(archive.is_file())
        self.assertEqual(sorted(host.home.glob(".crw-restore-*")), [])

    def test_a_registered_path_absent_at_the_reading_is_still_watched(self):
        """Dropping an absent candidate before the locks means never looking at it again.

        The window is inside this one function: the candidate list is built from what is on disk,
        and a file created after that and before the locks is in neither the proof loop nor the
        archive. So the case writes it while the locks are being taken.
        """
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import hostrecord
        from crw_transition import inventory, steps

        host = self.host
        custom, fixed = self.registered_at(host, "registered.json")
        document = json.loads(custom.read_text(encoding="utf-8"))
        custom.unlink()
        host.install_plugin()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        document["markerRoot"] = str(host.marker / "somebody-else")
        original = hostrecord.Locked
        made = []

        class Arriving(original):
            """The installer that owns that registration, writing its settings back right here."""

            def __init__(self, target, timeout=None):
                if not made:
                    made.append(1)
                    custom.write_text(json.dumps(document), encoding="utf-8")
                super().__init__(target, timeout)

        hostrecord.Locked = Arriving
        self.addCleanup(setattr, hostrecord, "Locked", original)
        answer = steps.settings_retire(snapshot, {}, apply=True)
        hostrecord.Locked = original
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:600])
        self.assertIn("was absent when this host was read", answer["detail"])
        self.assertEqual(json.loads(custom.read_text(encoding="utf-8")), document)
        self.assertTrue(fixed.is_file())
        self.assertEqual(sorted(host.home.glob("crw-completion-hook.json.superseded-*")), [])

    def test_a_lock_that_was_never_taken_is_never_released(self):
        """__exit__ unlinks by name, so releasing one this run failed to take frees somebody's."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import bridgerecord, hostrecord
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        held = Path(str(bridgerecord.ownership_lock_path(host.home)) + hostrecord.LOCK_SUFFIX)
        held.write_text("999999", encoding="utf-8")
        self.addCleanup(held.unlink, missing_ok=True)
        original = hostrecord.LOCK_TIMEOUT_SECONDS
        hostrecord.LOCK_TIMEOUT_SECONDS = 0.2
        self.addCleanup(setattr, hostrecord, "LOCK_TIMEOUT_SECONDS", original)
        results = steps.transition(snapshot, {"accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["mcp record retire"], "busy", json.dumps(results)[:700])
        # The other run still holds it, and its contents are untouched.
        self.assertTrue(held.is_file(), "the lock this run could not take was removed anyway")
        self.assertEqual(held.read_text(encoding="utf-8"), "999999")

    def test_an_operational_failure_after_the_retire_still_puts_the_settings_back(self):
        """A full disk is one more way of not reaching the standdown."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        settings = host.settings()
        original = steps.hook_standdown

        def failing(host_view, options, *, apply=False):
            raise OSError(errno.ENOSPC, "No space left on device")

        steps.hook_standdown = failing
        self.addCleanup(setattr, steps, "hook_standdown", original)
        # ORDER holds the function object, so the name has to be rebound there as well.
        steps.ORDER = tuple((name, failing if name == "hook standdown" else step)
                            for name, step in steps.ORDER)
        self.addCleanup(setattr, steps, "ORDER",
                        tuple((name, original if name == "hook standdown" else step)
                              for name, step in steps.ORDER))
        with self.assertRaises(OSError):
            steps.transition(snapshot, {"accept_hook_trust_gap": True}, apply=True)
        self.assertEqual(host.settings(), settings)
        self.assertEqual(sorted(host.home.glob("crw-completion-hook.json.superseded-*")), [])

    def test_a_standdown_that_wrote_before_failing_keeps_the_settings_archived(self):
        """Restoring a user-owned document with the registration gone leaves no hook at all."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import steps

        results = [
            {"step": "settings retire", "outcome": "settled", "detail": "retired",
             "retired": [{"from": "/nowhere/crw-completion-hook.json",
                          "to": "/nowhere/crw-completion-hook.json.superseded-x"}]},
            {"step": "hook standdown", "outcome": "refused", "detail": "the file was written and"
             " could not be read back", "applied": True, "wrote": True, "removed": ["user:Stop:0:0"]},
        ]
        steps._rollback_if_unfinished(results)
        standdown = results[1]
        self.assertEqual(standdown["settingsRestored"], [])
        self.assertEqual(standdown["settingsLeftArchived"],
                         ["/nowhere/crw-completion-hook.json.superseded-x"])
        self.assertIn("stay archived", standdown["detail"])

    def test_a_host_with_nothing_to_carry_forward_is_refused(self):
        """The plugin settings are built from a document; with none, there is nothing to build."""
        host = self.ready()
        (host.home / "crw-completion-hook.json").unlink()
        before = host.hooks_document()
        code, answer = host.call("--dest", str(host.destination), "transition", "--apply",
                                 "--accept-hook-trust-gap")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("carry forward", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_a_usage_failure_is_still_a_receipt(self):
        """Every command here promises one JSON document, including the ones that never ran."""
        done = run([CLI, "--codex-home", self.host.home, "nonsense"])
        self.assertEqual(done.returncode, 2, done.stdout + done.stderr)
        answer = json.loads(done.stdout)
        self.assertEqual(answer["outcome"], "refused")
        self.assertIn("invalid choice", answer["error"])
        self.assertIn("usage", answer)

    def test_an_unrecognised_entry_appearing_before_the_standdown_stops_it(self):
        """The locked reread already has the answer, and refusing there costs nothing."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        settings = host.settings()
        document = host.hooks_document()
        document["hooks"]["Stop"].append({"hooks": [{
            "type": "command",
            "command": str(host.version / "bin" / "crw-completion-hook"),
        }]})
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        results = steps.transition(snapshot, {"accept_hook_renumbering": True,
                                              "accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["hook standdown"], "refused", json.dumps(results)[:800])
        standdown = [item for item in results if item["step"] == "hook standdown"][0]
        self.assertIn("naming the packaged adapter or the destination", standdown["detail"])
        # Nothing removed, and the settings the first step had archived are back.
        self.assertEqual(host.hooks_document(), document)
        self.assertEqual(host.settings(), settings)
        self.assertEqual(sorted(host.home.glob("crw-completion-hook.json.superseded-*")), [])

    def test_an_interrupted_host_still_watches_for_settings_coming_back(self):
        """Every candidate absent is exactly when a writer is most likely to write one back."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import hostrecord
        from crw_transition import inventory, steps

        host = self.host
        custom, fixed = self.registered_at(host, "registered.json")
        document = json.loads(custom.read_text(encoding="utf-8"))
        custom.unlink()
        fixed.unlink()
        host.install_plugin()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        original = hostrecord.Locked
        made = []

        class Arriving(original):
            """The installer that owns the registration, writing its settings back right here."""

            def __init__(self, target, timeout=None):
                if not made:
                    made.append(1)
                    custom.write_text(json.dumps(document), encoding="utf-8")
                super().__init__(target, timeout)

        hostrecord.Locked = Arriving
        self.addCleanup(setattr, hostrecord, "Locked", original)
        answer = steps.settings_retire(snapshot, {}, apply=True)
        hostrecord.Locked = original
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:600])
        self.assertIn("was absent when this host was read", answer["detail"])
        self.assertEqual(json.loads(custom.read_text(encoding="utf-8")), document)

    def test_an_archive_that_fails_halfway_puts_the_earlier_ones_back(self):
        """A later move failing must not leave the earlier registrations without settings."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.host
        custom, fixed = self.registered_at(host, "registered.json")
        host.install_plugin()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        before = (json.loads(fixed.read_text(encoding="utf-8")),
                  json.loads(custom.read_text(encoding="utf-8")))
        genuine = steps.retire
        done = []

        def failing(path, into=None, stem=None):
            if done:
                raise OSError(errno.ENOSPC, "No space left on device")
            done.append(str(path))
            return genuine(path, into=into, stem=stem)

        steps.retire = failing
        self.addCleanup(setattr, steps, "retire", genuine)
        answer = steps.settings_retire(snapshot, {}, apply=True)
        steps.retire = genuine
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:600])
        self.assertIn("already archived were put back", answer["detail"])
        self.assertEqual(answer["settingsRestored"], done)
        self.assertEqual((json.loads(fixed.read_text(encoding="utf-8")),
                          json.loads(custom.read_text(encoding="utf-8"))), before)
        self.assertEqual(sorted(host.home.glob("crw-completion-hook.json.superseded-*")), [])

    def test_settings_recreated_between_the_retire_and_the_standdown_stop_it(self):
        """An identical registration lets a supported install recreate them without touching hooks."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        settings = host.settings()
        document = host.hooks_document()
        genuine = steps.settings_retire

        def recreating(host_view, options, *, apply=False):
            answer = genuine(host_view, options, apply=apply)
            if apply and answer["outcome"] == "settled":
                (host.home / "crw-completion-hook.json").write_text(
                    json.dumps(settings), encoding="utf-8")
            return answer

        steps.settings_retire = recreating
        steps.ORDER = tuple((name, recreating if name == "settings retire" else step)
                            for name, step in steps.ORDER)
        self.addCleanup(setattr, steps, "ORDER",
                        tuple((name, genuine if name == "settings retire" else step)
                              for name, step in steps.ORDER))
        self.addCleanup(setattr, steps, "settings_retire", genuine)
        results = steps.transition(snapshot, {"accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["hook standdown"], "refused", json.dumps(results)[:800])
        standdown = [item for item in results if item["step"] == "hook standdown"][0]
        self.assertIn("written again at", standdown["detail"])
        self.assertEqual(outcomes["settings install"], "not_reached")
        # The registration is still installed and the document it reads is untouched.
        self.assertEqual(host.hooks_document(), document)
        self.assertEqual(host.settings(), settings)

    def test_a_plugin_owned_record_names_the_executable_the_host_chose(self):
        """register-mcp takes --owner plugin with --bridge-command, so that state is supported."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        # The bridge surface already moved on its own, to an executable somebody chose.
        chosen = host.version / "bin" / "codex-thread-bridge-chosen"
        chosen.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        chosen.chmod(0o755)
        path = host.home / "crw-bridge-mcp.json"
        document = json.loads(path.read_text(encoding="utf-8"))
        document["owner"] = "plugin"
        document["bridgeExecutable"] = str(chosen)
        path.write_text(json.dumps(document), encoding="utf-8")
        # The table removed exactly as the transition would remove it, so the rest of the
        # configuration -- the plugin entry among it -- is untouched.
        first = inventory.snapshot(host.home, repo_root=ROOT)
        (host.home / "config.toml").write_text(
            host.config().replace(first["mcp"]["tableSpan"], "", 1), encoding="utf-8")
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        self.assertEqual(steps.bridge_command(snapshot), str(chosen))
        answer = steps.preflight(snapshot, {"accept_hook_trust_gap": True})
        self.assertEqual(answer["outcome"], "settled", json.dumps(answer["refusals"])[:600])

    def test_a_dangling_link_at_the_path_is_left_where_it_was_put(self):
        """exists() follows the link, so a dangling one read as nothing and was overwritten."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        retired = steps.settings_retire(snapshot, {}, apply=True)
        origin = Path(retired["retired"][0]["from"])
        archive = Path(retired["retired"][0]["to"])
        origin.symlink_to(host.root / "nothing-here.json")
        restored, kept = steps._restore_moved(retired["retired"])
        self.assertEqual(restored, [])
        self.assertEqual(kept, [str(archive)])
        self.assertTrue(origin.is_symlink())
        self.assertEqual(os.readlink(str(origin)), str(host.root / "nothing-here.json"))
        self.assertTrue(archive.is_file())

    def test_a_rollback_that_could_not_finish_says_what_is_still_archived(self):
        """Claiming nothing was left half retired is a claim, and it was not always true."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.host
        custom, fixed = self.registered_at(host, "registered.json")
        host.install_plugin()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        genuine = steps.retire
        done = []

        def failing(path, into=None, stem=None):
            if done:
                # Somebody writes the first path back, so its archive cannot return either.
                Path(done[0]).write_text("{}", encoding="utf-8")
                raise OSError(errno.ENOSPC, "No space left on device")
            done.append(str(path))
            return genuine(path, into=into, stem=stem)

        steps.retire = failing
        self.addCleanup(setattr, steps, "retire", genuine)
        answer = steps.settings_retire(snapshot, {}, apply=True)
        steps.retire = genuine
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:600])
        self.assertEqual(answer["settingsRestored"], [])
        self.assertTrue(answer["settingsLeftArchived"], json.dumps(answer)[:600])
        self.assertIn("except", answer["detail"])
        self.assertIn("restored by hand", answer["detail"])

    def test_a_budget_the_launcher_cannot_outlast_is_refused(self):
        """The launcher's cap eats its own margin above ceiling minus margin."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import steps

        host = self.host
        custom, fixed = self.registered_at(host, "registered.json",
                                           timeoutSeconds=steps.MAX_GUARD_SECONDS + 1)
        host.install_plugin()
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("leaves it no margin", answer["results"][0]["detail"])
        self.assertIn(str(steps.MAX_GUARD_SECONDS), answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_the_limit_is_derived_from_the_launcher_this_package_ships(self):
        """Neither file can import the other, so the agreement is asserted rather than assumed."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import completion
        from crw_transition import steps

        source = (ROOT / "plugins" / "crw" / "wiring" / "crw_stop_hook.py").read_text(
            encoding="utf-8")
        numbers = {}
        for line in source.splitlines():
            for name in ("MARGIN_SECONDS", "MAX_SECONDS"):
                if line.startswith(name + " = "):
                    numbers[name] = int(line.split("=", 1)[1].strip())
        self.assertEqual(sorted(numbers), ["MARGIN_SECONDS", "MAX_SECONDS"], json.dumps(numbers))
        self.assertEqual(numbers["MAX_SECONDS"], completion.LAUNCHER_CEILING_SECONDS)
        self.assertEqual(numbers["MARGIN_SECONDS"], steps.LAUNCHER_MARGIN_SECONDS)
        self.assertEqual(steps.MAX_GUARD_SECONDS,
                         numbers["MAX_SECONDS"] - numbers["MARGIN_SECONDS"])

    def test_a_missing_relay_under_the_pointer_is_refused(self):
        """The adapter does not answer a Stop by itself; it runs the relay for the decision."""
        host = self.ready()
        (host.version / "bin" / "codex-session-relay").unlink()
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("the relay at", answer["results"][0]["detail"])
        self.assertIn("codex-session-relay", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_a_cached_package_that_declares_something_else_is_refused(self):
        """A structurally valid declaration is not a declaration of this repository's launcher."""
        host = self.ready()
        declared = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "hooks" / "stop-recording-completion.json")
        document = json.loads(declared.read_text(encoding="utf-8"))
        document["hooks"]["Stop"][0]["hooks"][0]["command"] = "true"
        declared.write_text(json.dumps(document), encoding="utf-8")
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("this checkout declares", answer["results"][0]["detail"])
        self.assertIn("the same surface, once each", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_a_cache_whose_bytes_no_longer_name_its_version_is_refused(self):
        """An edited cache still answers to the version it was filed under, and nothing else."""
        host = self.ready()
        host.seal_cache()
        edited = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                  / "skills" / "crw-run" / "SKILL.md")
        edited.write_text(edited.read_text(encoding="utf-8") + "\nAdded after installation.\n",
                          encoding="utf-8")
        before = host.hooks_document()
        code, answer = host.transition("--apply", seal=False)
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("does not name this payload", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_a_cached_package_without_the_bridge_server_is_refused(self):
        """The same question asked of the surface the record hands over."""
        host = self.ready()
        declared = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "mcp.json")
        document = json.loads(declared.read_text(encoding="utf-8"))
        # Structurally valid on purpose: the payload contract passes it, and what refuses is the
        # question of whether the server it declares starts this repository's bridge launcher.
        entry = dict(document["mcpServers"]["codex-thread-bridge"])
        # A file the package really ships, so the payload contract is satisfied, and the wrong
        # program for this server, which is the only thing left to notice.
        entry["args"] = ["./wiring/crw_stop_hook.py"]
        document["mcpServers"] = {"codex-thread-bridge": entry}
        declared.write_text(json.dumps(document), encoding="utf-8")
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("The replacement has to be that same map",
                      answer["results"][0]["detail"])
        self.assertEqual(host.config(), before)

    def test_a_cache_whose_manifest_declares_other_documents_is_refused(self):
        """Codex loads what the manifest names, so stale files at the old paths prove nothing."""
        cache = (Path(self.host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION)
        host = self.ready()
        manifest = cache / ".codex-plugin" / "plugin.json"
        document = json.loads(manifest.read_text(encoding="utf-8"))
        # The declared documents move, and the originals are left exactly where they were.
        other = cache / "wiring" / "hooks" / "other.json"
        other.write_text(json.dumps({"hooks": {"Stop": [{"hooks": [{
            "type": "command", "command": "python3 \"${PLUGIN_ROOT}/wiring/crw_bridge_mcp.py\"",
            "timeout": 10, "statusMessage": "(x)"}]}]}}), encoding="utf-8")
        document["hooks"] = ["./wiring/hooks/other.json"]
        manifest.write_text(json.dumps(document), encoding="utf-8")
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("this checkout declares", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_a_command_that_only_mentions_the_launcher_is_not_running_it(self):
        """An interpreter runs its first non-option argument, and nothing else on the line."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import steps

        # The impersonations from review, and the real declarations beside them.
        self.assertIsNone(steps._script(["true", "crw_stop_hook.py"]))
        self.assertEqual(steps._script(["python3", "./wiring/crw_stop_hook.py",
                                        "./wiring/crw_bridge_mcp.py"]),
                         "wiring/crw_stop_hook.py")
        self.assertEqual(steps._script(["python3", "${PLUGIN_ROOT}/wiring/crw_stop_hook.py"]),
                         "wiring/crw_stop_hook.py")
        self.assertEqual(steps._script(["python3", "-u", "./wiring/crw_bridge_mcp.py"]),
                         "wiring/crw_bridge_mcp.py")
        self.assertIsNone(steps._script([]))

    def test_a_server_whose_first_argument_is_another_script_is_refused(self):
        """Python runs args[0]; the expected launcher sitting after it never starts."""
        host = self.ready()
        declared = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "mcp.json")
        document = json.loads(declared.read_text(encoding="utf-8"))
        entry = dict(document["mcpServers"]["codex-thread-bridge"])
        entry["args"] = ["./wiring/crw_stop_hook.py", "./wiring/crw_bridge_mcp.py"]
        document["mcpServers"] = {"codex-thread-bridge": entry}
        declared.write_text(json.dumps(document), encoding="utf-8")
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("The replacement has to be that same map",
                      answer["results"][0]["detail"])
        self.assertEqual(host.config(), before)

    def test_the_settings_stay_locked_across_the_removal(self):
        """Noticing afterwards is not keeping the writer out; the lock has to span both."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import hostrecord
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        settings_path = host.home / "crw-completion-hook.json"
        original = steps.hook_standdown
        seen = {}

        def watching(host_view, options, *, apply=False):
            """Ask from inside the removal whether that path is still held by this run."""
            try:
                with hostrecord.Locked(settings_path, timeout=0.2):
                    seen["held"] = False
            except hostrecord.Busy:
                seen["held"] = True
            return original(host_view, options, apply=apply)

        steps.ORDER = tuple((name, watching if name == "hook standdown" else step)
                            for name, step in steps.ORDER)
        self.addCleanup(setattr, steps, "ORDER",
                        tuple((name, original if name == "hook standdown" else step)
                              for name, step in steps.ORDER))
        results = steps.transition(snapshot, {"accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertTrue(seen.get("held"),
                        "the settings lock was not held across the hook removal")
        self.assertEqual(outcomes["hook standdown"], "settled", json.dumps(results)[:700])
        self.assertEqual(outcomes["settings install"], "settled")

    def test_a_cache_declaring_the_launcher_twice_is_refused(self):
        """Two declarations of our own launcher is the duplicate this command exists to end."""
        host = self.ready()
        declared = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "hooks" / "stop-recording-completion.json")
        document = json.loads(declared.read_text(encoding="utf-8"))
        entry = dict(document["hooks"]["Stop"][0]["hooks"][0])
        document["hooks"]["Stop"][0]["hooks"].append(entry)
        declared.write_text(json.dumps(document), encoding="utf-8")
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("the same surface, once each", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_an_option_that_eats_its_argument_is_not_running_the_launcher(self):
        """python3 -c takes source text, so the path after it is never a file Python runs."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import steps

        launcher = "./wiring/crw_stop_hook.py"
        self.assertIsNone(steps._script(["python3", "-c", launcher]))
        self.assertIsNone(steps._script(["python3", "-m", "wiring.crw_stop_hook"]))
        self.assertIsNone(steps._script(["python3", "-W", "ignore", launcher]))
        self.assertIsNone(steps._script(["python3", "--what-is-this", launcher]))
        # The options that do not consume the word after them still resolve.
        self.assertEqual(steps._script(["python3", "-u", launcher]), "wiring/crw_stop_hook.py")
        self.assertEqual(steps._script(["python3", launcher]), "wiring/crw_stop_hook.py")

    def test_a_name_that_merely_starts_with_python3_is_not_an_interpreter(self):
        """python3-does-not-exist is not a Python, and need not even be on the host."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import steps

        launcher = "./wiring/crw_stop_hook.py"
        self.assertIsNone(steps._script(["python3-does-not-exist", launcher]))
        self.assertIsNone(steps._script(["python3x", launcher]))
        self.assertIsNone(steps._script(["pythonista", launcher]))
        # The real spellings, including a versioned one and an absolute path.
        for name in ("python", "python3", "python3.11", "/usr/bin/python3.13"):
            self.assertEqual(steps._script([name, launcher]), "wiring/crw_stop_hook.py", name)

    def test_a_cache_declaring_a_python_that_is_not_one_is_refused(self):
        """It passes every structural check and starts nothing."""
        host = self.ready()
        declared = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "hooks" / "stop-recording-completion.json")
        document = json.loads(declared.read_text(encoding="utf-8"))
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        entry["command"] = entry["command"].replace("python3 ", "python3-does-not-exist ", 1)
        declared.write_text(json.dumps(document), encoding="utf-8")
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("python3-does-not-exist", answer["results"][0]["detail"])
        self.assertIn("this checkout declares", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_a_watched_path_is_locked_across_the_removal_too(self):
        """A registration names it and it was absent; something arriving there is the same event."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.host
        custom, fixed = self.registered_at(host, "registered.json")
        document = json.loads(fixed.read_text(encoding="utf-8"))
        fixed.unlink()
        host.install_plugin()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        retired = steps.settings_retire(snapshot, {}, apply=True)
        self.assertEqual(retired["outcome"], "settled", json.dumps(retired)[:500])
        self.assertIn(str(fixed), retired["watched"], json.dumps(retired)[:500])
        # The installer that owns the default path writing its settings back.
        fixed.write_text(json.dumps(document), encoding="utf-8")
        back = steps._recreated_settings([retired])
        self.assertTrue([item for item in back if str(fixed) in item], json.dumps(back))

    def test_a_versioned_python_this_checkout_does_not_ship_is_refused(self):
        """Half a launcher is not a launcher: the interpreter is part of the declaration."""
        host = self.ready()
        declared = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "hooks" / "stop-recording-completion.json")
        document = json.loads(declared.read_text(encoding="utf-8"))
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        entry["command"] = entry["command"].replace("python3 ", "python3.999999 ", 1)
        declared.write_text(json.dumps(document), encoding="utf-8")
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("python3.999999", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_a_trailing_newline_is_not_the_name_of_an_executable(self):
        """$ matches before a final newline, and nothing can run "python3\n"."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import steps

        self.assertIsNone(steps._script(["python3\n", "./wiring/crw_stop_hook.py"]))
        self.assertIsNone(steps._shape(["python3\n", "./wiring/crw_stop_hook.py"]))

    def test_a_second_cached_server_starting_the_same_launcher_is_refused(self):
        """One owner per surface: another entry running it is a second bridge."""
        host = self.ready()
        declared = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "mcp.json")
        document = json.loads(declared.read_text(encoding="utf-8"))
        document["mcpServers"]["codex-thread-bridge-again"] = dict(
            document["mcpServers"]["codex-thread-bridge"])
        declared.write_text(json.dumps(document), encoding="utf-8")
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("codex-thread-bridge-again", answer["results"][0]["detail"])
        self.assertIn("a second bridge", answer["results"][0]["detail"])
        self.assertEqual(host.config(), before)

    def test_a_cached_hook_with_another_matcher_or_timeout_is_refused(self):
        """A launcher under a restrictive matcher, or a timeout that cannot outlast the adapter."""
        import tempfile
        for field, value in (("matcher", {"sessionSource": "vscode"}), ("timeout", 1)):
            with self.subTest(field=field):
                # Its own host per case: a cache is copied in whole and cannot be installed twice.
                host = Host(Path(tempfile.mkdtemp(dir=self.directory, prefix=field + "-")))
                host.manual_install()
                host.install_plugin()
                cache = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                         / "wiring" / "hooks" / "stop-recording-completion.json")
                document = json.loads(cache.read_text(encoding="utf-8"))
                group = document["hooks"]["Stop"][0]
                if field == "matcher":
                    group["matcher"] = value
                else:
                    group["hooks"][0]["timeout"] = value
                cache.write_text(json.dumps(document), encoding="utf-8")
                before = host.hooks_document()
                code, answer = host.transition("--apply")
                self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
                self.assertIn(field + "=", answer["results"][0]["detail"])
                self.assertEqual(host.hooks_document(), before)

    def test_a_cached_hook_this_cannot_read_is_a_mismatch_not_an_absence(self):
        """env python3 <launcher> starts it a second time; being unreadable does not make it gone."""
        host = self.ready()
        declared = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "hooks" / "stop-recording-completion.json")
        document = json.loads(declared.read_text(encoding="utf-8"))
        entry = dict(document["hooks"]["Stop"][0]["hooks"][0])
        entry["command"] = "env " + entry["command"]
        document["hooks"]["Stop"][0]["hooks"].append(entry)
        declared.write_text(json.dumps(document), encoding="utf-8")
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("a command this cannot read", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_an_idle_relay_store_is_not_work_in_flight(self):
        """The relay is never asked: its snapshot is nonempty when idle, and asking can create it."""
        host = self.ready()
        host.database.write_text("not a real store", encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:1500])
        _, seen = host.call("inspect")
        self.assertIn("did not run the relay status command",
                      " ".join(seen["host"]["inFlight"]["how"]))
        self.assertTrue(seen["host"]["inFlight"]["storeExists"])
        self.assertIsNone(seen["host"]["inFlight"]["liveness"])

    def test_a_plugin_owned_retire_still_hands_over_its_watched_paths(self):
        """Every document that is there names the plugin; an absent registered one is not one."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.host
        custom, fixed = self.registered_at(host, "registered.json")
        document = json.loads(custom.read_text(encoding="utf-8"))
        custom.unlink()
        # The state the fast path is for: the fixed document already names the plugin, and the
        # registration still reads a custom document that is not on disk at this reading.
        plugin_owned = dict(document)
        plugin_owned.update({"owner": "plugin",
                             "adapterInterpreter": str(host.destination / "current" / "bin"
                                                       / "python3"),
                             "adapterEntryPoint": str(host.destination / "current" / "bin"
                                                      / "crw-completion-hook")})
        fixed.write_text(json.dumps(plugin_owned), encoding="utf-8")
        host.install_plugin()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        genuine = steps.settings_retire

        def recreating(host_view, options, *, apply=False):
            """The installer that owns that registration, writing its settings back in the window."""
            answer = genuine(host_view, options, apply=apply)
            if apply:
                custom.write_text(json.dumps(document), encoding="utf-8")
            return answer

        steps.ORDER = tuple((name, recreating if name == "settings retire" else step)
                            for name, step in steps.ORDER)
        self.addCleanup(setattr, steps, "ORDER",
                        tuple((name, genuine if name == "settings retire" else step)
                              for name, step in steps.ORDER))
        before = host.hooks_document()
        results = steps.transition(snapshot, {"accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        # The harm first: without the watched paths the standdown never asks, removes the
        # registration, and reports success over a document the plugin install cannot replace.
        self.assertEqual(outcomes["hook standdown"], "refused", json.dumps(results)[:900])
        standdown = [item for item in results if item["step"] == "hook standdown"][0]
        self.assertIn("written again at", standdown["detail"])
        # The registration is still installed, and it reads the document that came back.
        self.assertEqual(host.hooks_document(), before)
        self.assertTrue(custom.is_file())
        retired = [item for item in results if item["step"] == "settings retire"][0]
        self.assertEqual(retired["outcome"], "already_done", json.dumps(retired)[:600])
        self.assertIn(str(custom), retired.get("watched") or [], json.dumps(retired)[:600])

    def test_a_cache_declaring_another_event_is_refused(self):
        """The Stop entry is intact and the package also answers an event nobody here declares."""
        host = self.ready()
        declared = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "hooks" / "stop-recording-completion.json")
        document = json.loads(declared.read_text(encoding="utf-8"))
        # Structurally valid, and the expected Stop declaration is left exactly as it was: what
        # is added is a second event running the same adapter on turns this package never
        # reasoned about, with a Stop's guard budget.
        document["hooks"]["SessionStart"] = json.loads(json.dumps(document["hooks"]["Stop"]))
        declared.write_text(json.dumps(document), encoding="utf-8")
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("SessionStart", answer["results"][0]["detail"])
        self.assertIn("no SessionStart hook at all", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_a_cache_that_makes_the_bridge_required_is_refused(self):
        """Same command, same arguments, and a failure to start now ends the session."""
        host = self.ready()
        declared = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "mcp.json")
        document = json.loads(declared.read_text(encoding="utf-8"))
        self.assertIs(document["mcpServers"]["codex-thread-bridge"]["required"], False)
        document["mcpServers"]["codex-thread-bridge"]["required"] = True
        declared.write_text(json.dumps(document), encoding="utf-8")
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("required", answer["results"][0]["detail"])
        self.assertIn("The replacement has to be that same map", answer["results"][0]["detail"])
        self.assertEqual(host.config(), before)

    def test_an_interpreter_somewhere_else_is_not_the_one_this_package_declares(self):
        """An absolute Python that is not on the host starts nothing and reads as our python3."""
        host = self.ready()
        declared = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "hooks" / "stop-recording-completion.json")
        document = json.loads(declared.read_text(encoding="utf-8"))
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        entry["command"] = entry["command"].replace("python3 ", "/definitely/missing/python3 ", 1)
        declared.write_text(json.dumps(document), encoding="utf-8")
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("/definitely/missing/python3", answer["results"][0]["detail"])
        self.assertIn("this checkout declares", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_a_cached_command_that_runs_the_launcher_twice_is_refused(self):
        """Codex runs a hook command through a shell, so what follows the launcher runs too."""
        host = self.ready()
        declared = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "hooks" / "stop-recording-completion.json")
        document = json.loads(declared.read_text(encoding="utf-8"))
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        # The first half is this repository's declaration, unchanged, and resolves to the launcher
        # positionally. The second half runs the adapter again on every Stop.
        entry["command"] = entry["command"] + " && " + entry["command"]
        declared.write_text(json.dumps(document), encoding="utf-8")
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("&&", answer["results"][0]["detail"])
        self.assertIn("the same surface, once each", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_an_archive_whose_source_cannot_be_removed_leaves_no_copy(self):
        """Copied and not unlinked is live and archived at once, and a recovery takes the copy."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import steps

        source = Path(self.directory) / "settings-on-a-read-only-volume.json"
        source.write_text('{"kept": true}', encoding="utf-8")
        home = Path(self.directory) / "home-for-failed-archives"
        home.mkdir()
        real_replace, real_unlink = os.replace, os.unlink

        def refuse_to_rename(src, dst, *arguments, **keywords):
            raise OSError(errno.EXDEV, "Invalid cross-device link")

        def refuse_to_unlink(path, *arguments, **keywords):
            if str(path) == str(source):
                raise OSError(errno.EPERM, "Operation not permitted")
            return real_unlink(path, *arguments, **keywords)

        os.replace, os.unlink = refuse_to_rename, refuse_to_unlink
        try:
            with self.assertRaises(OSError):
                steps.retire(source, into=home, stem="crw-completion-hook.json")
        finally:
            os.replace, os.unlink = real_replace, real_unlink
        self.assertTrue(source.is_file(), "the document that was not retired is still live")
        self.assertEqual(sorted(home.glob("crw-completion-hook.json.superseded-*")), [],
                         "a refused retirement left an archive a later recovery would take")

    @needs_reader
    def test_a_table_removal_that_cannot_be_read_back_is_not_reported_as_removed(self):
        """The write landed and nothing can say what it landed on, which is not a removal."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import hostrecord, reading
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        config = host.home / "config.toml"
        genuine = reading.read_text
        genuine_write = hostrecord.atomic_write
        written = []

        def writing(path, text, *arguments, **keywords):
            """The removal itself, which is what makes the next read the read-back."""
            found = genuine_write(path, text, *arguments, **keywords)
            if Path(str(path)) == config:
                written.append(str(path))
            return found

        def failing(path, what, **keywords):
            if written and Path(str(path)) == config:
                # Only after the write: the reads this step takes to prove the span are real.
                return reading.Reading(state=reading.ACCESS_ERROR,
                                       detail="the configuration could not be read back")
            return genuine(path, what, **keywords)

        reading.read_text, hostrecord.atomic_write = failing, writing
        self.addCleanup(setattr, reading, "read_text", genuine)
        self.addCleanup(setattr, hostrecord, "atomic_write", genuine_write)
        answer = steps.mcp_table_standdown(snapshot, {}, apply=True)
        reading.read_text, hostrecord.atomic_write = genuine, genuine_write
        self.assertTrue(written, "the removal never wrote, so this proved nothing")
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:600])
        self.assertIn("written but unverified", answer["detail"])
        self.assertTrue(answer.get("wrote"))
        # The write really did land, which is why this is reported rather than retried silently.
        self.assertNotIn("[mcp_servers." + inventory.SERVER_NAME + "]", host.config())

    @needs_reader
    def test_a_plugin_disabled_after_the_standdown_says_no_hook_fires(self):
        """A refusal after the registration is gone is not a refusal that changed nothing."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        genuine = steps.hook_standdown

        def disabling(host_view, options, *, apply=False):
            """The operator's own disable landing after the readiness check, before the write."""
            (host.home / "config.toml").write_text(
                host.config().replace("enabled = true", "enabled = false", 1), encoding="utf-8")
            return genuine(host_view, options, apply=apply)

        steps.ORDER = tuple((name, disabling if name == "hook standdown" else step)
                            for name, step in steps.ORDER)
        self.addCleanup(setattr, steps, "ORDER",
                        tuple((name, genuine if name == "hook standdown" else step)
                              for name, step in steps.ORDER))
        results = steps.transition(snapshot, {"accept_hook_trust_gap": True}, apply=True)
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["hook standdown"], "settled", json.dumps(results)[:900])
        refused = [item for item in results if item["outcome"] == "refused"]
        self.assertEqual([item["step"] for item in refused], ["mcp record retire"],
                         json.dumps(results)[:900])
        # The plugin is why it refused, and the sentence after it is what the host now is.
        self.assertIn("is disabled", refused[0]["detail"])
        self.assertIn("no completion hook fires at all", refused[0]["detail"])
        self.assertIn("Re-enable the plugin and rerun", refused[0]["detail"])
        self.assertTrue(refused[0]["completionHookAbsent"])
        # And the host really is in that state: the manual registration is gone and the settings
        # that are there name the plugin whose hook cannot load.
        self.assertEqual(host.hooks_document()["hooks"]["Stop"][0]["hooks"], [])
        self.assertEqual(host.settings()["owner"], "plugin")

    @needs_reader
    def test_a_launcher_that_fakes_a_version_is_not_a_python(self):
        """One fixed question has one fixed answer, so the question carries a number per run."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory

        host = self.ready()
        # Answers the installer's probe exactly and does nothing else: no Python behind it.
        liar = Path(self.directory) / "python3-shaped"
        liar.write_text("#!/bin/sh\necho 3.12\n", encoding="utf-8")
        liar.chmod(0o755)
        self.assertFalse(inventory.runs_python(str(liar)))
        document = host.hooks_document()
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        entry["command"] = entry["command"].replace(str(sys.executable), str(liar), 1)
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        found = inventory.read_hook(host.home, repo_root=ROOT)
        self.assertFalse(any(item["proven"] for item in found["entries"]),
                         json.dumps(found["entries"])[:600])
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertEqual(host.hooks_document(), before)

    @needs_reader
    def test_what_proves_a_registration_is_named_in_the_reading(self):
        """Evidence rather than proof, so the reading says which evidence it is."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory

        host = self.ready()
        found = inventory.read_hook(host.home, repo_root=ROOT)
        self.assertTrue(found["entries"])
        for item in found["entries"]:
            self.assertTrue(item["proven"], json.dumps(item)[:400])
            self.assertIn("Not an installation attestation", item["provenBasis"])

    @needs_reader
    def test_a_legacy_table_with_no_record_survives_the_removal(self):
        """With no ownership record the table is the only copy of what the host registered."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.host
        custom = host.version / "bin" / "codex-thread-bridge-of-its-own"
        custom.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        custom.chmod(0o755)
        host.link_skills()
        host.register_hook()
        # =, because argparse reads a value beginning with -- as the next option.
        registered = run([RUNTIME, "register-mcp", "--owner", "user", "--codex-home", host.home,
                          "--bridge-command", custom, "--bridge-arg=--from-the-old-install",
                          "--apply"])
        self.assertEqual(registered.returncode, 0, registered.stdout[-600:])
        # The legacy shape: a table in config.toml and no ownership record beside it.
        (host.home / "crw-bridge-mcp.json").unlink()
        host.install_plugin()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        self.assertEqual(steps.mcp_record_retire(snapshot, {}, apply=True)["outcome"],
                         "already_done")
        removed = steps.mcp_table_standdown(snapshot, {}, apply=True)
        self.assertEqual(removed["outcome"], "settled", json.dumps(removed)[:600])
        # Interrupted right here: the table is gone and the run never reached the record install.
        self.assertNotIn("[mcp_servers." + inventory.SERVER_NAME + "]", host.config())
        self.assertIsNone(host.record())
        again = inventory.snapshot(host.home, repo_root=ROOT)
        self.assertEqual(steps.bridge_command(again), str(custom))
        written = steps.mcp_record_install(again, {}, apply=True)
        self.assertEqual(written["outcome"], "settled", json.dumps(written)[:600])
        self.assertEqual(host.record()["bridgeExecutable"], str(custom))
        self.assertEqual(host.record()["args"], ["--from-the-old-install"])
        # And the archive that carried it across is named in the answer that made it.
        self.assertTrue(removed["preserved"], json.dumps(removed)[:600])

    @needs_reader
    def test_a_stale_archive_does_not_stand_in_for_the_table_being_removed(self):
        """An old archive is history, not a description of the table about to be removed."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import bridgerecord
        from crw_transition import inventory, steps

        host = self.host
        custom = host.version / "bin" / "codex-thread-bridge-of-its-own"
        custom.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        custom.chmod(0o755)
        host.link_skills()
        host.register_hook()
        registered = run([RUNTIME, "register-mcp", "--owner", "user", "--codex-home", host.home,
                          "--bridge-command", custom, "--bridge-arg=--from-the-old-install",
                          "--apply"])
        self.assertEqual(registered.returncode, 0, registered.stdout[-600:])
        (host.home / "crw-bridge-mcp.json").unlink()
        # An archive this host kept from some earlier run, naming a different bridge entirely.
        stale = bridgerecord.document(
            command=str(host.version / "bin" / "codex-thread-bridge"), arguments=["--old"],
            name=inventory.SERVER_NAME, issue="CRW-115", owner="user")
        (host.home / (bridgerecord.RECORD_NAME + ".superseded-20000101T000000Z")).write_text(
            json.dumps(stale), encoding="utf-8")
        host.install_plugin()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        removed = steps.mcp_table_standdown(snapshot, {}, apply=True)
        self.assertEqual(removed["outcome"], "settled", json.dumps(removed)[:600])
        again = inventory.snapshot(host.home, repo_root=ROOT)
        # The live table's identity, not the one the older archive happens to carry.
        self.assertEqual(steps.bridge_command(again), str(custom))
        written = steps.mcp_record_install(again, {}, apply=True)
        self.assertEqual(written["outcome"], "settled", json.dumps(written)[:600])
        self.assertEqual(host.record()["args"], ["--from-the-old-install"])
        self.assertTrue(removed["preserved"], json.dumps(removed)[:600])

    @needs_reader
    def test_an_archive_dated_in_the_future_does_not_outrank_the_one_just_made(self):
        """The stamp in an archive name is an order the recovery reads, not a clock it trusts."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import bridgerecord
        from crw_transition import inventory, steps

        host = self.host
        custom = host.version / "bin" / "codex-thread-bridge-of-its-own"
        custom.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        custom.chmod(0o755)
        host.link_skills()
        host.register_hook()
        registered = run([RUNTIME, "register-mcp", "--owner", "user", "--codex-home", host.home,
                          "--bridge-command", custom, "--bridge-arg=--from-the-old-install",
                          "--apply"])
        self.assertEqual(registered.returncode, 0, registered.stdout[-600:])
        (host.home / "crw-bridge-mcp.json").unlink()
        ahead = bridgerecord.document(
            command=str(host.version / "bin" / "codex-thread-bridge"), arguments=["--old"],
            name=inventory.SERVER_NAME, issue="CRW-115", owner="user")
        (host.home / (bridgerecord.RECORD_NAME + ".superseded-20300101T000000Z")).write_text(
            json.dumps(ahead), encoding="utf-8")
        host.install_plugin()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        removed = steps.mcp_table_standdown(snapshot, {}, apply=True)
        self.assertEqual(removed["outcome"], "settled", json.dumps(removed)[:600])
        again = inventory.snapshot(host.home, repo_root=ROOT)
        self.assertEqual(steps.bridge_command(again), str(custom))
        self.assertEqual(steps.mcp_record_install(again, {}, apply=True)["outcome"], "settled")
        self.assertEqual(host.record()["args"], ["--from-the-old-install"])

    def test_a_single_quoted_launcher_path_is_not_the_one_this_package_declares(self):
        """Single quotes stop the expansion, so the literal directory is what the shell passes."""
        host = self.ready()
        declared = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "hooks" / "stop-recording-completion.json")
        document = json.loads(declared.read_text(encoding="utf-8"))
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        self.assertIn('"', entry["command"])
        entry["command"] = entry["command"].replace('"', "'")
        declared.write_text(json.dumps(document), encoding="utf-8")
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("as written", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    def test_a_quoted_mcp_argument_is_a_filename_with_quotes_in_it(self):
        """No shell runs an MCP argument, so the quotes stay part of the path."""
        host = self.ready()
        declared = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "mcp.json")
        document = json.loads(declared.read_text(encoding="utf-8"))
        entry = document["mcpServers"]["codex-thread-bridge"]
        entry["args"] = ["'" + entry["args"][0] + "'"]
        declared.write_text(json.dumps(document), encoding="utf-8")
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("as written", answer["results"][0]["detail"])
        self.assertIn("The replacement has to be that same map", answer["results"][0]["detail"])
        self.assertEqual(host.config(), before)

    def test_a_marker_root_that_cannot_be_listed_is_a_reading_not_a_crash(self):
        """This reading refuses nothing, so a failure in it must not take the inventory down."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import reading
        from crw_transition import inventory

        unreadable = Path(self.directory) / "marker-nobody-can-list"
        unreadable.mkdir()
        (unreadable / "one").write_text("", encoding="utf-8")
        unreadable.chmod(0o000)
        self.addCleanup(unreadable.chmod, 0o755)
        if os.access(str(unreadable), os.R_OK):
            self.skipTest("this user can list a directory with no permissions")
        answer = inventory.read_in_flight({"markerRoot": str(unreadable)})
        self.assertIn(answer["state"], (reading.ACCESS_ERROR, reading.UNREADABLE))
        self.assertIn("could not be listed", answer["detail"])
        self.assertIsNone(answer["markerHistory"])

    @needs_reader
    def test_a_relative_codex_home_is_settled_before_anything_is_derived(self):
        """Two spellings of one file take one lock twice, and the run waits itself out."""
        import tempfile
        # The flag is this command's own input and travels nowhere, so it is resolved and used.
        host = Host(Path(tempfile.mkdtemp(dir=self.directory, prefix="relative-flag-")))
        host.manual_install()
        host.install_plugin()
        before = host.hooks_document()
        # Relative on purpose, resolved against the working directory this run is given.
        # With no exported home at all, so the flag is the only thing naming one.
        done = run([CLI, "--codex-home", "home", "--dest", "dest", "transition", "--apply",
                    "--accept-hook-trust-gap"], cwd=host.root,
                   env={key: value for key, value in os.environ.items() if key != "CODEX_HOME"})
        answer = json.loads(done.stdout)
        self.assertEqual(done.returncode, 0, json.dumps(answer["results"], indent=2)[:1500])
        outcomes = {item["step"]: item["outcome"] for item in answer["results"]}
        self.assertNotIn("busy", outcomes.values(), json.dumps(answer["results"])[:900])
        self.assertEqual(outcomes["settings retire"], "settled")
        self.assertEqual(outcomes["hook standdown"], "settled")
        self.assertNotEqual(host.hooks_document(), before)
        self.assertEqual(host.settings()["owner"], "plugin")

    @needs_reader
    def test_a_relative_codex_home_in_the_environment_is_refused(self):
        """The launchers read that same variable from their own directory, not from this one."""
        host = self.ready()
        before = (host.config(), host.hooks_document(), host.settings(), host.record())
        # With the flag and without it. The flag moves where this run writes and never reaches a
        # launcher, so it cannot settle what the exported variable means to one.
        for extra in ([], ["--codex-home", str(host.home)]):
            with self.subTest(flag=bool(extra)):
                done = run([CLI, *extra, "--dest", "dest", "transition", "--apply",
                            "--accept-hook-trust-gap"],
                           cwd=host.root, env={**os.environ, "CODEX_HOME": "home"})
                answer = json.loads(done.stdout)
                self.assertEqual(done.returncode, 2, done.stdout[-800:])
                self.assertEqual(answer["outcome"], "refused")
                self.assertIn("CODEX_HOME", answer["error"])
                self.assertIn("relative", answer["error"])
                # Nothing read, nothing written: the refusal is in front of the snapshot.
                self.assertEqual(
                    (host.config(), host.hooks_document(), host.settings(), host.record()),
                    before)

    @needs_reader
    def test_two_absolute_homes_that_disagree_are_reported(self):
        """The flag decides where this run writes; the launchers read the exported one."""
        host = self.ready()
        elsewhere = Path(self.directory) / "another-home"
        elsewhere.mkdir()
        done = run([CLI, "--codex-home", host.home, "--dest", "dest", "transition",
                    "--accept-hook-trust-gap"], cwd=host.root,
                   env={**os.environ, "CODEX_HOME": str(elsewhere)})
        answer = json.loads(done.stdout)
        self.assertEqual(done.returncode, 0, done.stdout[-800:])
        notes = json.dumps(answer["results"][0].get("notes"))
        self.assertIn(str(elsewhere), notes)
        self.assertIn("will not read what this run writes", notes)
        # And two homes that agree say nothing about it.
        agreed = run([CLI, "--codex-home", host.home, "--dest", "dest", "transition",
                      "--accept-hook-trust-gap"], cwd=host.root,
                     env={**os.environ, "CODEX_HOME": str(host.home)})
        self.assertEqual(agreed.returncode, 0, agreed.stdout[-800:])
        self.assertNotIn("will not read what this run writes", agreed.stdout)

    def test_a_cached_launcher_that_is_not_ours_is_refused(self):
        """The declarations can agree while the programs they name do not."""
        host = self.ready()
        launcher = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / "wiring" / "crw_stop_hook.py")
        self.assertTrue(launcher.is_file())
        # A payload census counts this file and the declarations still name it.
        launcher.write_text("#!/usr/bin/env python3\n", encoding="utf-8")
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("wiring/crw_stop_hook.py", answer["results"][0]["detail"])
        self.assertIn("is not the launcher this checkout ships", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    @needs_reader
    def test_absent_settings_are_decided_under_the_lock_a_writer_takes(self):
        """Absent is not stopped until it is held: a supported install can write in the window."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import hostrecord
        from crw_transition import inventory, steps

        host = self.ready()
        settings = host.home / "crw-completion-hook.json"
        plugin_owned = {**json.loads(settings.read_text(encoding="utf-8")), "owner": "plugin",
                        "adapterInterpreter": str(host.destination / "current" / "bin" / "python3"),
                        "adapterEntryPoint": str(host.destination / "current" / "bin"
                                                 / "crw-completion-hook")}
        settings.unlink()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        original = hostrecord.Locked
        made = []

        class Arriving(original):
            """The supported installer writing plugin-owned settings inside the window."""

            def __init__(self, target, timeout=None):
                if not made:
                    made.append(1)
                    settings.write_text(json.dumps(plugin_owned), encoding="utf-8")
                super().__init__(target, timeout)

        hostrecord.Locked = Arriving
        self.addCleanup(setattr, hostrecord, "Locked", original)
        results = steps.disable(snapshot, {}, apply=True)
        hostrecord.Locked = original
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["hook settings"], "settled", json.dumps(results)[:700])
        self.assertFalse(settings.exists(), "the document written in the window is still live")
        self.assertTrue(sorted(host.home.glob("crw-completion-hook.json.superseded-*")))

    @needs_reader
    def test_a_link_that_cannot_be_resolved_is_foreign_rather_than_an_exception(self):
        """skill_unlink re-reads this after the hook and the bridge have already moved."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory

        host = self.ready()
        # Names this repository does not ship, so the loop is the only thing under test.
        looping = host.home / "skills" / "crw-zzz-looping"
        partner = host.home / "skills" / "crw-zzz-partner"
        looping.symlink_to(partner)
        partner.symlink_to(looping)
        genuine = inventory.checkout_of

        def claiming(path):
            """A link this run believed was ours a moment before it stopped resolving."""
            return Path(ROOT) if Path(path).name.startswith("crw-zzz") else genuine(path)

        # 3.13 answers a loop by returning the path; the floor this repository supports raises
        # RuntimeError from a non-strict resolve, and a dead mount raises OSError on any version.
        # Raised here for those two rather than skipped, because the guard is for them.
        real_resolve = Path.resolve

        def resolving(self, *arguments, **keywords):
            if self.name.startswith("crw-zzz"):
                raise RuntimeError("Symbolic link loop: " + str(self))
            return real_resolve(self, *arguments, **keywords)

        Path.resolve = resolving
        self.addCleanup(setattr, Path, "resolve", real_resolve)
        inventory.checkout_of = claiming
        self.addCleanup(setattr, inventory, "checkout_of", genuine)
        found = inventory.read_skill_links(host.home, ROOT)
        inventory.checkout_of = genuine
        Path.resolve = real_resolve
        why = [item["why"] for item in found["foreign"] if "crw-zzz" in item["path"]]
        self.assertTrue(why, json.dumps(found["foreign"])[:500])
        self.assertTrue(any("could not be resolved" in item for item in why), json.dumps(why))
        self.assertFalse([item for item in found["crwOwned"] if "crw-zzz" in item["path"]])

    @needs_reader
    def test_a_future_dated_settings_archive_does_not_outrank_the_one_just_made(self):
        """Every recovery here reads the greatest name, so every retire has to reach it."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        # disable retires what the plugin owns, so the host is transitioned first.
        code, _ = host.transition("--apply")
        self.assertEqual(code, 0)
        live = host.settings()
        self.assertEqual(live["owner"], "plugin")
        # An archive from a copied installation, dated years ahead of this host's clock.
        ahead = {**live, "owner": "user", "markerRoot": str(host.marker / "from-another-host")}
        (host.home / "crw-completion-hook.json.superseded-20300101T000000Z").write_text(
            json.dumps(ahead), encoding="utf-8")
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        results = steps.disable(snapshot, {}, apply=True)
        self.assertEqual({item["step"]: item["outcome"] for item in results}["hook settings"],
                         "settled", json.dumps(results)[:600])
        found, _origin = inventory.newest_retired(host.home)
        self.assertEqual(found["owner"], "plugin", json.dumps(found)[:400])
        self.assertNotEqual(found.get("markerRoot"), ahead["markerRoot"])

    @needs_reader
    def test_a_manifest_that_is_not_an_object_is_reported_not_raised(self):
        """Valid JSON is not a manifest, and this reader exists to report on the payload."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory

        host = self.ready()
        manifest = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                    / ".codex-plugin" / "plugin.json")
        manifest.write_text('["not", "an", "object"]', encoding="utf-8")
        answer = inventory.read_plugin(host.home)
        self.assertFalse(answer["payload"]["manifest"])
        self.assertIn("not an object", answer["detail"])
        code, seen = host.call("inspect")
        self.assertEqual(code, 0, json.dumps(seen)[:600])
        self.assertNotEqual(seen.get("outcome"), "internal_error", json.dumps(seen)[:400])

    @needs_reader
    def test_archives_that_cannot_be_listed_refuse_the_retirement(self):
        """An unlistable directory is not one holding no archives."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        real_scandir = os.scandir

        def refusing(where, *arguments, **keywords):
            if str(where) == str(host.home):
                raise OSError(errno.EACCES, "Permission denied")
            return real_scandir(where, *arguments, **keywords)

        os.scandir = refusing
        self.addCleanup(setattr, os, "scandir", real_scandir)
        with self.assertRaises(OSError):
            steps.retire(host.home / "crw-completion-hook.json")
        os.scandir = real_scandir
        # Nothing was archived and the document is still where it was.
        self.assertTrue((host.home / "crw-completion-hook.json").is_file())
        self.assertEqual(sorted(host.home.glob("crw-completion-hook.json.superseded-*")), [])
        # And the step that calls it answers with a refusal rather than a claim.
        os.scandir = refusing
        answer = steps.settings_retire(inventory.snapshot(host.home, repo_root=ROOT), {},
                                       apply=True)
        os.scandir = real_scandir
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:500])
        self.assertTrue((host.home / "crw-completion-hook.json").is_file())

    @needs_reader
    def test_disable_says_which_surface_is_still_live_when_an_archive_fails(self):
        """A receipt that lost its results cannot say the hook stopped and the bridge did not."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        code, _ = host.transition("--apply")
        self.assertEqual(code, 0)
        real_scandir = os.scandir
        seen = []

        def refusing(where, *arguments, **keywords):
            # Both surfaces archive into the same directory, so the SECOND enumeration is the
            # bridge record's: the settings move and the bridge does not.
            if str(where) == str(host.home):
                seen.append(str(where))
                if len(seen) > 1:
                    raise OSError(errno.EACCES, "Permission denied")
            return real_scandir(where, *arguments, **keywords)

        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        os.scandir = refusing
        try:
            results = steps.disable(snapshot, {}, apply=True)
        finally:
            os.scandir = real_scandir
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["hook settings"], "settled", json.dumps(results)[:700])
        self.assertEqual(outcomes["bridge record"], "refused", json.dumps(results)[:700])
        self.assertIn("new bridge starts are not stopped",
                      [item for item in results if item["step"] == "bridge record"][0]["detail"])
        self.assertTrue((host.home / "crw-bridge-mcp.json").is_file())

    @needs_reader
    def test_a_lock_that_will_not_release_does_not_reverse_the_stop(self):
        """A release that fails after the move does not put the document back."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import hostrecord
        from crw_transition import inventory, steps

        host = self.ready()
        code, _ = host.transition("--apply")
        self.assertEqual(code, 0)
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        original = hostrecord.Locked

        class Releasing(original):
            """The move lands and the lock comes apart badly afterwards."""

            def __exit__(self, *arguments):
                super().__exit__(*arguments)
                raise OSError(errno.EIO, "the lock file could not be removed")

        hostrecord.Locked = Releasing
        try:
            results = steps.disable(snapshot, {}, apply=True)
        finally:
            hostrecord.Locked = original
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["hook settings"], "settled", json.dumps(results)[:800])
        self.assertEqual(outcomes["bridge record"], "settled", json.dumps(results)[:800])
        # And the host really is stopped: both documents are gone and archived.
        self.assertFalse((host.home / "crw-completion-hook.json").exists())
        self.assertFalse((host.home / "crw-bridge-mcp.json").exists())
        self.assertIn("did not come apart cleanly",
                      [item for item in results if item["step"] == "hook settings"][0]["detail"])
        # Both facts survive: the surface stopped, and the cleanup that failed is named.
        for item in results:
            self.assertTrue(item.get("lockCleanupFailed"), json.dumps(item)[:300])
        # And the exit code says so, without moving the stop itself into the refusals.
        import importlib.util
        loading = importlib.util.spec_from_file_location("crw_transition_cli", CLI)
        cli = importlib.util.module_from_spec(loading)
        loading.loader.exec_module(cli)
        self.assertEqual(cli.verdict(results), cli.EXIT_REFUSED, json.dumps(results)[:400])
        self.assertEqual({item["outcome"] for item in results}, {"settled"})

    @needs_reader
    def test_an_unsearchable_parent_does_not_become_a_retirement(self):
        """exists() answering false is not proof that this run moved anything."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_runtime import hostrecord
        from crw_transition import inventory, steps

        host = self.ready()
        code, _ = host.transition("--apply")
        self.assertEqual(code, 0)
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        original = hostrecord.Locked

        class Failing(original):
            """The lock cannot even be taken, so nothing this run did moved anything."""

            def __enter__(self):
                raise OSError(errno.EACCES, "Permission denied")

        # And the parent has lost its execute bit as far as this process can tell, so exists()
        # answers false about documents that are still there. That inference is what is on trial.
        settings = host.home / "crw-completion-hook.json"
        record = host.home / "crw-bridge-mcp.json"
        real_exists = Path.exists

        def hidden(self, *arguments, **keywords):
            if self in (settings, record):
                return False
            return real_exists(self, *arguments, **keywords)

        Path.exists = hidden
        self.addCleanup(setattr, Path, "exists", real_exists)
        hostrecord.Locked = Failing
        try:
            results = steps.disable(snapshot, {}, apply=True)
        finally:
            hostrecord.Locked = original
            Path.exists = real_exists
        outcomes = {item["step"]: item["outcome"] for item in results}
        self.assertEqual(outcomes["hook settings"], "refused", json.dumps(results)[:700])
        self.assertEqual(outcomes["bridge record"], "refused", json.dumps(results)[:700])
        # And the documents really are still there, which is what the refusal says.
        self.assertTrue(settings.is_file())
        self.assertTrue(record.is_file())

    @needs_reader
    def test_a_quoted_relative_token_is_not_an_absolute_interpreter(self):
        """shlex already removed the shell's quoting, so a quote left in a word is a path char."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory

        host = self.ready()
        document = host.hooks_document()
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        # The reviewer's case, built for real: a relative path whose literal characters include
        # apostrophes, resolving to a genuine Python from the directory this command runs in.
        # Stripping the quotes before the absoluteness test turns it into /opt/python, which
        # looks absolute, while the probe keeps resolving the raw token from here.
        import shlex as _shlex
        quoted = Path(host.root) / "'" / "opt"
        quoted.mkdir(parents=True)
        (quoted / "python'").symlink_to(sys.executable)
        words = _shlex.split(entry["command"])
        entry["command"] = _shlex.join(["'/opt/python'"] + words[1:])
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        here = os.getcwd()
        os.chdir(str(host.root))
        self.addCleanup(os.chdir, here)
        found = inventory.read_hook(host.home, repo_root=ROOT)
        os.chdir(here)
        self.assertEqual([item["argv"][0] for item in found["entries"]], ["'/opt/python'"],
                         json.dumps(found["entries"])[:400])
        self.assertFalse(any(item["proven"] for item in found["entries"]),
                         json.dumps(found["entries"])[:600])

    @needs_reader
    def test_a_tilde_spelled_exported_home_is_refused(self):
        """The launchers build Path(CODEX_HOME) and never expand a ~."""
        host = self.ready()
        before = (host.config(), host.hooks_document(), host.settings(), host.record())
        done = run([CLI, "--codex-home", host.home, "--dest", "dest", "transition", "--apply",
                    "--accept-hook-trust-gap"], cwd=host.root,
                   env={**os.environ, "CODEX_HOME": "~/.codex-alt"})
        answer = json.loads(done.stdout)
        self.assertEqual(done.returncode, 2, done.stdout[-800:])
        self.assertIn("CODEX_HOME", answer["error"])
        self.assertIn("~", answer["error"])
        self.assertEqual((host.config(), host.hooks_document(), host.settings(), host.record()),
                         before)

    @needs_reader
    def test_recovery_refuses_when_the_archives_cannot_be_listed(self):
        """The archives are where a custom bridge and its arguments survive an interruption."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        real_scandir = os.scandir

        def refusing(where, *arguments, **keywords):
            if str(where) == str(host.home):
                raise OSError(errno.EACCES, "Permission denied")
            return real_scandir(where, *arguments, **keywords)

        os.scandir = refusing
        self.addCleanup(setattr, os, "scandir", real_scandir)
        with self.assertRaises(OSError):
            steps._retired_record(snapshot)
        answer = steps.mcp_record_install(snapshot, {}, apply=True)
        os.scandir = real_scandir
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer)[:500])
        self.assertIn("could not be listed", answer["detail"])

    @needs_reader
    def test_a_relative_interpreter_is_not_a_registration_this_writer_emits(self):
        """The hook resolves it from each session's workspace; this command resolves it here."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory

        host = self.ready()
        document = host.hooks_document()
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        # A real Python reachable from this command's directory and nowhere in particular later.
        relative = Path(host.root) / "python3"
        relative.symlink_to(sys.executable)
        entry["command"] = entry["command"].replace(str(sys.executable), "./python3", 1)
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        # Read from the directory the relative token resolves in, so what refuses is the rule and
        # not the interpreter being unreachable from here.
        here = os.getcwd()
        os.chdir(str(host.root))
        self.addCleanup(os.chdir, here)
        found = inventory.read_hook(host.home, repo_root=ROOT)
        os.chdir(here)
        self.assertTrue(found["entries"], json.dumps(found)[:400])
        self.assertFalse(any(item["proven"] for item in found["entries"]),
                         json.dumps(found["entries"])[:600])
        before = host.hooks_document()
        done = run([CLI, "--codex-home", host.home, "transition", "--apply",
                    "--accept-hook-trust-gap"], cwd=host.root)
        self.assertEqual(done.returncode, 1, done.stdout[-700:])
        self.assertEqual(host.hooks_document(), before)

    @needs_reader
    def test_an_unreadable_but_searchable_home_refuses_the_retirement(self):
        """3.13 suppresses the scanning error inside glob, so the enumeration cannot use it."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import steps

        host = self.ready()
        settings = host.home / "crw-completion-hook.json"
        host.home.chmod(0o311)
        self.addCleanup(host.home.chmod, 0o755)
        if os.access(str(host.home), os.R_OK):
            self.skipTest("this user can read a directory without the read bit")
        with self.assertRaises(OSError):
            steps.retire(settings)
        host.home.chmod(0o755)
        self.assertTrue(settings.is_file(), "the document was archived on an unread directory")
        self.assertEqual(sorted(host.home.glob("crw-completion-hook.json.superseded-*")), [])

    @needs_reader
    def test_a_suffixed_archive_left_behind_still_decides_the_next_name(self):
        """The whole key orders these, not the stamp alone."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.ready()
        live = host.settings()
        # The unsuffixed counterpart was restored or removed; only the suffixed one is left.
        ahead = {**live, "owner": "user", "markerRoot": str(host.marker / "left-behind")}
        (host.home / "crw-completion-hook.json.superseded-20990101T000000Z-001").write_text(
            json.dumps(ahead), encoding="utf-8")
        made = steps.retire(host.home / "crw-completion-hook.json")
        self.assertTrue(Path(made).name.endswith("-002"), made)
        found, _origin = inventory.newest_retired(host.home)
        self.assertNotEqual(found.get("markerRoot"), ahead["markerRoot"], json.dumps(found)[:300])

    @needs_reader
    def test_a_cache_whose_skills_cannot_be_listed_is_a_reading_not_a_crash(self):
        """The version cache is replaced wholesale, so it can go away between two calls."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory

        host = self.ready()
        skills = (Path(host.home) / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
                  / "skills")
        skills.chmod(0o000)
        self.addCleanup(skills.chmod, 0o755)
        if os.access(str(skills), os.R_OK):
            self.skipTest("this user can list a directory with no permissions")
        answer = inventory.read_plugin(host.home)
        self.assertFalse(answer["payload"]["skills"])
        self.assertEqual(answer["skills"], [])
        self.assertIn("could not be listed", answer["detail"])
        # And the command refuses with its step list rather than failing out of the reading.
        before = host.hooks_document()
        code, seen = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(seen)[:700])
        self.assertTrue(seen.get("results"), json.dumps(seen)[:700])
        self.assertEqual(host.hooks_document(), before)

    @needs_reader
    def test_a_skills_directory_that_cannot_be_listed_is_an_unreadable_inventory(self):
        """skill_unlink re-reads this at the end, after the hook and the bridge have moved."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory

        host = self.ready()
        links = Path(host.home) / "skills"
        links.chmod(0o000)
        self.addCleanup(links.chmod, 0o755)
        if os.access(str(links), os.R_OK):
            self.skipTest("this user can list a directory with no permissions")
        answer = inventory.read_skill_links(host.home, ROOT)
        self.assertIn("could not be listed", answer["unreadable"] or "")
        self.assertEqual(answer["crwOwned"], [])
        # And the run answers with its step list rather than a top-level failure, which is what
        # makes the half-transitioned host visible: every earlier step recorded, the unlink
        # refused, and the reason named.
        code, seen = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(seen)[:700])
        outcomes = {item["step"]: item["outcome"] for item in seen["results"]}
        self.assertEqual(outcomes["skill unlink"], "refused", json.dumps(seen["results"])[:900])
        self.assertEqual(outcomes["hook standdown"], "settled")
        unlink = [item for item in seen["results"] if item["step"] == "skill unlink"][0]
        self.assertIn("could not be listed", unlink["detail"])
        self.assertIn("nothing was removed", unlink["detail"])

    @needs_reader
    def test_a_hooks_value_that_is_not_a_table_is_read_rather_than_raised(self):
        """A configuration can say anything, and this one says hooks is a string."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory

        # No [hooks.state] table, so the top-level key below is the only hooks value there is.
        host = self.host.manual_install().install_plugin(trusted=False)
        path = host.home / "config.toml"
        path.write_text('hooks = "invalid"\n' + path.read_text(encoding="utf-8"),
                        encoding="utf-8")
        answer = inventory.read_plugin(host.home)
        self.assertEqual(answer["configEntry"], "PRESENT", json.dumps(answer)[:500])
        self.assertEqual(answer["trustKeys"], [])
        self.assertFalse(answer["trustKeyPresent"])
        # The modelled refusal, not an internal error.
        code, seen = host.call("inspect")
        self.assertEqual(code, 0, json.dumps(seen)[:700])
        self.assertNotEqual(seen.get("outcome"), "internal_error", json.dumps(seen)[:400])

    @needs_reader
    def test_an_alias_starting_a_custom_bridge_executable_is_found(self):
        """register-mcp takes --bridge-command, so the standard basename is not the only name."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory

        host = self.host
        custom = host.version / "bin" / "a-bridge-of-its-own"
        custom.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        custom.chmod(0o755)
        host.link_skills()
        host.register_hook()
        # A supported install of a bridge whose executable is named something else entirely.
        registered = run([RUNTIME, "register-mcp", "--owner", "user", "--codex-home", host.home,
                          "--bridge-command", custom, "--apply"])
        self.assertEqual(registered.returncode, 0, registered.stdout[-600:])
        # And a second server name starting that same executable, which is the same bridge.
        host.append_config('[mcp_servers.my-bridge]\ncommand = "' + str(custom) + '"\n')
        host.install_plugin()
        found = inventory.read_mcp(host.home)
        self.assertEqual([item["name"] for item in found["aliases"]], ["my-bridge"],
                         json.dumps(found.get("aliases"))[:500])
        # And the command refuses rather than leaving that table beside the plugin's declaration.
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("my-bridge", answer["results"][0]["detail"])
        self.assertEqual(host.config(), before)

    @needs_reader
    def test_a_registration_whose_interpreter_is_not_a_python_is_not_ours(self):
        """The writer renders whatever starts the command, so byte equality is not enough."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory

        host = self.ready()
        document = host.hooks_document()
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        # Our adapter as the argument and something else entirely running it: the words this
        # repository's writer emits, and a command that never reaches the adapter.
        entry["command"] = entry["command"].replace(str(sys.executable), "/bin/true", 1)
        self.assertIn("/bin/true", entry["command"])
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        found = inventory.read_hook(host.home, repo_root=ROOT)
        self.assertTrue(found["entries"], json.dumps(found)[:500])
        self.assertFalse(any(item["proven"] for item in found["entries"]),
                         json.dumps(found["entries"])[:600])
        # And the run leaves that foreign hook exactly where it is.
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertIn("cannot prove is its own adapter", answer["results"][0]["detail"])
        self.assertEqual(host.hooks_document(), before)

    @needs_reader
    def test_an_executable_merely_named_python_is_not_ours(self):
        """A name is not the fact: /tmp/python3 -> /bin/true answers nothing and runs nothing."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory

        host = self.ready()
        liar = Path(self.directory) / "python3"
        liar.symlink_to("/bin/true")
        document = host.hooks_document()
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        entry["command"] = entry["command"].replace(str(sys.executable), str(liar), 1)
        self.assertIn(str(liar), entry["command"])
        (host.home / "hooks.json").write_text(json.dumps(document, indent=2), encoding="utf-8")
        found = inventory.read_hook(host.home, repo_root=ROOT)
        self.assertTrue(found["entries"], json.dumps(found)[:500])
        self.assertFalse(any(item["proven"] for item in found["entries"]),
                         json.dumps(found["entries"])[:600])
        before = host.hooks_document()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"])[:700])
        self.assertEqual(host.hooks_document(), before)

    @needs_reader
    def test_a_python_whose_name_is_not_python_is_still_ours(self):
        """The installer takes --python and validates by running it, so a name rule is too narrow."""
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory

        host = self.host
        # A real Python under a name no basename rule would accept.
        elsewhere = host.version / "bin" / "pypy3"
        elsewhere.symlink_to(sys.executable)
        host.link_skills()
        registered = host.register_hook(python=elsewhere)
        self.assertEqual(registered.returncode, 0, registered.stdout[-600:])
        host.register_mcp()
        host.install_plugin()
        found = inventory.read_hook(host.home, repo_root=ROOT)
        self.assertTrue(found["entries"], json.dumps(found)[:500])
        self.assertTrue(all(item["proven"] for item in found["entries"]),
                        json.dumps(found["entries"])[:600])
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:1200])
        self.assertEqual(host.hooks_document()["hooks"]["Stop"][0]["hooks"], [])
        self.assertEqual(host.settings()["owner"], "plugin")


@needs_reader
class InterruptionAfterEveryStepConverges(TransitionCase):
    """A run stopped after each individual step, then finished by an ordinary rerun.

    The earlier version of this claim rested on a case that stopped at preflight, which proves
    only that nothing happened. This stops the sequence after step 1, after step 2, and so on by
    driving the step functions directly, then runs the real CLI and requires it to reach the same
    end state. That is what "converges on the next run" has to mean.
    """

    def end_state(self, host):
        """The host as JSON with its own root spelled out of it.

        Every host here lives in its own temporary directory, so comparing raw paths would compare
        the directories rather than the states. What is being compared is the SHAPE each run
        arrived at.
        """
        state = {"config": host.config(), "hooks": host.hooks_document(),
                 "settings": host.settings(), "record": host.record(),
                 "skills": sorted(p.name for p in (host.home / "skills").iterdir())
                 if (host.home / "skills").is_dir() else []}
        text = json.dumps(state, indent=2, sort_keys=True)
        return text.replace(str(host.root), "<host>").replace(
            str(Path(host.root).resolve()), "<host>")

    def build(self, label):
        import tempfile
        directory = tempfile.mkdtemp(dir=self.directory, prefix=label + "-")
        host = Host(Path(directory))
        host.manual_install()
        host.install_plugin()
        return host

    def test_stopping_after_each_step_still_converges_to_the_same_host(self):
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        reference = self.build("reference")
        code, _ = reference.transition("--apply")
        self.assertEqual(code, 0)
        wanted = self.end_state(reference)

        options = {"accept_hook_renumbering": False, "accept_hook_trust_gap": True}
        for stop_after in range(1, len(steps.ORDER) + 1):
            with self.subTest(stopAfter=steps.ORDER[stop_after - 1][0]):
                host = self.build("cut%d" % stop_after)
                host_view = inventory.snapshot(host.home, repo_root=ROOT)
                previous = host_view["settings"]["document"]
                for name, step in steps.ORDER[:stop_after]:
                    # Each step decides from disk, so the snapshot is retaken the way a fresh
                    # process would take it. Only the settings source is carried, exactly as the
                    # sequence does.
                    host_view = inventory.snapshot(host.home, repo_root=ROOT)
                    if name == "settings install":
                        answer = step(host_view, options, apply=True, previous=previous)
                    else:
                        answer = step(host_view, options, apply=True)
                    self.assertIn(answer["outcome"], ("settled", "already_done", "would_change"),
                                  name + ": " + str(answer.get("detail")))
                code, answer = host.transition("--apply")
                self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:1500])
                self.assertEqual(self.end_state(host), wanted,
                                 "a run cut after " + steps.ORDER[stop_after - 1][0]
                                 + " did not converge to the same host")


if __name__ == "__main__":
    unittest.main()


POLICY_TABLES = ('\n[mcp_servers.codex-thread-bridge.tools.create_thread]\n'
                 'approval_mode = "approve"\n'
                 '\n[mcp_servers.codex-thread-bridge.tools.send_message_to_thread]\n'
                 'approval_mode = "approve"\n')


class TheApprovalPolicySurvivesTheTransition(TransitionCase):
    """CRW-142. The table is the only thing gating create_thread until it is removed.

    Measured on an isolated home: while the user table exists it wins over the plugin declaration,
    so installing the plugin does not drop the gate -- removing the table does. Before this, the
    transition had no idea approval policy existed. It refused a table carrying one, but only
    because the bytes did not match what it renders, and the reason it printed named the command
    and arguments even when those were exactly right. An operator who believed that reason and
    deleted the policy tables got a passing preflight and a host with no gate.
    """

    def granted(self, host, tables=POLICY_TABLES):
        """Put the policy tables where a host really carries them: inside the registration.

        Appended at the end of the file instead, they sit behind the plugin and trust tables and
        are no longer part of this server's span -- which the command correctly refuses, and which
        is its own case below rather than the setup for every other one.
        """
        text = host.config()
        header = "[mcp_servers.codex-thread-bridge]"
        start = text.index(header) + len(header)
        following = text.find("\n[", start)
        cut = following if following != -1 else len(text)
        (host.home / "config.toml").write_text(
            text[:cut].rstrip("\n") + "\n" + tables.strip("\n") + "\n" + text[cut:],
            encoding="utf-8")
        return host

    @needs_reader
    def test_the_policy_is_read_whatever_spelling_it_is_written_in(self):
        host = self.granted(self.ready())
        code, answer = host.call("inspect")
        self.assertEqual(code, 0)
        self.assertEqual(answer["host"]["mcp"]["policy"]["tools"],
                         {"create_thread": "approve", "send_message_to_thread": "approve"})
        self.assertEqual(answer["policyInEffect"]["tools"],
                         {"create_thread": "approve", "send_message_to_thread": "approve"})

    @needs_reader
    def test_a_quoted_spelling_is_read_and_refused_for_the_reason_it_really_has(self):
        """The case that made the old reason a false statement rather than a conservative one."""
        host = self.ready()
        host.append_config('\n[mcp_servers."codex-thread-bridge".tools.create_thread]\n'
                           'approval_mode = "approve"\n')
        before = host.config()
        code, answer = host.call("inspect")
        self.assertEqual(answer["host"]["mcp"]["policy"]["tools"], {"create_thread": "approve"})
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("cannot render back", answer["results"][0]["detail"])
        self.assertEqual(host.config(), before)

    @needs_reader
    def test_a_tool_the_declaration_does_not_carry_refuses_before_anything_moves(self):
        host = self.ready()
        self.granted(host, '\n[mcp_servers.codex-thread-bridge.tools.steer_thread]\n'
                           'approval_mode = "approve"\n')
        before = host.config(), host.hooks_document(), host.settings()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("steer_thread", answer["results"][0]["detail"])
        self.assertIn("no approval_mode for it", answer["results"][0]["detail"])
        self.assertEqual((host.config(), host.hooks_document(), host.settings()), before)

    @needs_reader
    def test_a_mode_the_declaration_disagrees_with_refuses_and_names_both(self):
        host = self.ready()
        self.granted(host, '\n[mcp_servers.codex-thread-bridge.tools.create_thread]\n'
                           'approval_mode = "prompt"\n')
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        detail = answer["results"][0]["detail"]
        self.assertIn("'prompt'", detail)
        self.assertIn("'approve'", detail)
        self.assertIn("does not choose between them", detail)
        self.assertEqual(host.config(), before)

    @needs_reader
    def test_a_policy_this_command_cannot_interpret_refuses(self):
        host = self.ready()
        self.granted(host, '\n[mcp_servers.codex-thread-bridge.tools.create_thread]\n'
                           'approval_mode = "approve"\nenabled = true\n')
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("enabled", answer["results"][0]["detail"])
        self.assertEqual(host.config(), before)

    @needs_reader
    def test_a_mode_outside_the_measured_set_refuses(self):
        host = self.ready()
        self.granted(host, '\n[mcp_servers.codex-thread-bridge.tools.create_thread]\n'
                           'approval_mode = "always"\n')
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        detail = " ".join(item.get("detail") or "" for item in answer["results"])
        self.assertIn("always", detail)
        self.assertEqual(host.config(), before)

    @needs_reader
    def test_a_declaration_that_cannot_be_read_is_compared_with_nothing_and_refuses(self):
        host = self.granted(self.ready())
        cache = host.home / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
        (cache / "wiring" / "mcp.json").write_text("{ not json", encoding="utf-8")
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertEqual(host.config(), before)

    @needs_reader
    def test_a_registration_field_the_declaration_cannot_reproduce_is_named(self):
        """tool_timeout_sec is the live host's blocker, and it is now said in those words."""
        host = self.granted(self.ready())
        text = host.config().replace('[mcp_servers.codex-thread-bridge]',
                                     '[mcp_servers.codex-thread-bridge]\ntool_timeout_sec = 60')
        (host.home / "config.toml").write_text(text, encoding="utf-8")
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertIn("tool_timeout_sec", answer["results"][0]["detail"])
        self.assertIn("does not reproduce", answer["results"][0]["detail"])
        self.assertEqual(host.config(), text)

    @needs_reader
    def test_a_preserved_policy_is_removed_with_its_table_and_nothing_else(self):
        host = self.granted(self.ready())
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:2000])
        # The bytes, not merely "the server is gone": the removed span now covers several tables
        # with blank lines between them, and an orphaned policy table is the state in which codex
        # fails to load at all.
        after = host.config()
        self.assertNotIn("[mcp_servers.codex-thread-bridge]", after)
        self.assertNotIn("approval_mode", after)
        self.assertNotIn("tools.create_thread", after)
        self.assertIn('[plugins."crw@crw"]', after)
        self.assertIn("[hooks.state.", after)
        standdown = [item for item in answer["results"]
                     if item["step"] == "mcp table standdown"][0]
        self.assertEqual(standdown["removedPolicy"],
                         {"create_thread": "approve", "send_message_to_thread": "approve"})
        # And what serves the bridge afterwards still declares the same gate. Whether a declared
        # mode engages at call time is unmeasured and is not claimed here.
        cache = host.home / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
        declared = json.loads((cache / "wiring" / "mcp.json").read_text(encoding="utf-8"))
        gates = declared["mcpServers"]["codex-thread-bridge"]["tools"]
        self.assertEqual({tool: gate["approval_mode"] for tool, gate in gates.items()},
                         {"create_thread": "approve", "send_message_to_thread": "approve"})

    @needs_reader
    def test_a_host_with_no_policy_at_all_is_unaffected(self):
        """The ordinary register-mcp table. This is the regression guard for existing installs."""
        host = self.ready()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 0, json.dumps(answer["results"], indent=2)[:2000])
        self.assertEqual(answer["policyBefore"]["tools"], {})

    @needs_reader
    def test_running_it_again_after_a_preserved_transition_changes_nothing(self):
        host = self.granted(self.ready())
        self.assertEqual(host.transition("--apply")[0], 0)
        settled = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(host.config(), settled)
        self.assertEqual(self.host.outcomes(answer)["mcp table standdown"], "already_done")

    @needs_reader
    def test_disable_and_remove_gain_no_approval_refusal(self):
        """They never touch the table, so a policy question must not start blocking them."""
        host = self.granted(self.ready())
        self.assertEqual(host.transition("--apply")[0], 0)
        for command in ("disable", "remove"):
            code, answer = host.call(command, "--apply")
            self.assertEqual(code, 0, command + ": "
                             + json.dumps(answer["results"], indent=2)[:1200])
            self.assertIn("policyInEffect", answer)

    @needs_reader
    def test_a_cached_plugin_that_codex_does_not_load_is_not_reported_as_in_effect(self):
        """Devin finding: a cache directory is not a loaded plugin."""
        host = self.granted(self.ready())
        self.assertEqual(host.transition("--apply")[0], 0)
        text = host.config().replace('[plugins."crw@crw"]', '[plugins."gone@gone"]')
        (host.home / "config.toml").write_text(text, encoding="utf-8")
        code, answer = host.call("inspect")
        self.assertEqual(code, 0)
        self.assertEqual(answer["policyInEffect"]["tools"], {})
        self.assertIn("no plugin entry", answer["policyInEffect"]["detail"])

    @needs_reader
    def test_a_candidate_whose_manifest_is_not_an_object_refuses_rather_than_crashing(self):
        """Devin finding: valid JSON is not a manifest, and .get on a list is an internal error."""
        host = self.ready()
        broken = Path(self.directory) / "candidate"
        (broken / ".codex-plugin").mkdir(parents=True)
        (broken / ".codex-plugin" / "plugin.json").write_text("[]", encoding="utf-8")
        code, answer = host.call("check-declaration", "--package", str(broken))
        self.assertEqual(code, 1)
        self.assertNotEqual(answer.get("outcome"), "internal_error")
        self.assertEqual(answer["command"], "check-declaration")

    @needs_reader
    def test_the_standdown_revalidates_the_whole_payload_before_it_writes(self):
        """Devin's red finding, isolated to the step that has to catch it.

        preflight and plugin_refusals both validate the payload, and both run before this step.
        The version cache takes no lock, so a replacement landing after them reaches the removal
        having been judged on a package that is no longer there. Comparing only the tools the user
        gates accepted exactly that: a declaration keeping create_thread = approve and adding a
        second gate with an invalid mode passed, the table went, and the host threw out the whole
        declaration and served no bridge.

        Called directly rather than through the CLI, so the earlier gates cannot be what refuses.
        """
        import sys as _sys
        _sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import inventory, steps

        host = self.granted(self.ready())
        cache = host.home / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
        declaration = cache / "wiring" / "mcp.json"
        document = json.loads(declaration.read_text(encoding="utf-8"))
        gates = document["mcpServers"]["codex-thread-bridge"]["tools"]
        # Everything the user table gates is preserved exactly. The defect is elsewhere.
        gates["some_other_tool"] = {"approval_mode": "definitely-not-a-valid-mode"}
        declaration.write_text(json.dumps(document), encoding="utf-8")

        before = host.config()
        snapshot = inventory.snapshot(host.home, repo_root=ROOT)
        answer = steps.mcp_table_standdown(snapshot, {"accept_hook_trust_gap": True}, apply=True)
        self.assertEqual(answer["outcome"], "refused", json.dumps(answer, indent=2)[:1500])
        self.assertEqual(host.config(), before)
        self.assertIn("[mcp_servers.codex-thread-bridge]", host.config())
        self.assertIn("approval_mode", host.config())

    # ---------------------------------------------------------------- adversarial byte forms
    #
    # D4 widened what the standdown will delete: a proven span can now carry approval tables that
    # an operator wrote by hand. Every case below is a way for table_span, which reads lines, to
    # disagree with tomllib about where a table starts and ends. removal_is_structural exists to
    # catch exactly that, and until these run it is asserted rather than demonstrated.

    def refuses_unchanged(self, host, why=None):
        """Run the transition and require it to change nothing. Returns the refusal detail."""
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1, json.dumps(answer["results"], indent=2)[:1200])
        self.assertEqual(host.config(), before, "the configuration was modified")
        detail = " ".join(item.get("detail") or "" for item in answer["results"])
        if why:
            self.assertIn(why, detail)
        return detail

    @needs_reader
    def test_a_comment_inside_a_policy_block_leaves_the_table_unproven(self):
        host = self.ready()
        self.granted(host, '\n[mcp_servers.codex-thread-bridge.tools.create_thread]\n'
                           '# an operator note\napproval_mode = "approve"\n')
        detail = self.refuses_unchanged(host)
        # The reason says which byte form IS provable, so a reader is not left guessing.
        self.assertIn("exactly two lines", detail)

    @needs_reader
    def test_crlf_line_endings_leave_the_table_unproven(self):
        host = self.granted(self.ready())
        path = host.home / "config.toml"
        path.write_bytes(path.read_text(encoding="utf-8").replace("\n", "\r\n")
                         .encode("utf-8"))
        self.refuses_unchanged(host)

    @needs_reader
    def test_a_dotted_policy_key_leaves_the_table_unproven(self):
        """tools.create_thread.approval_mode is the same policy in a form we cannot render back."""
        host = self.ready()
        text = host.config().replace(
            '[mcp_servers.codex-thread-bridge]',
            '[mcp_servers.codex-thread-bridge]\ntools.create_thread.approval_mode = "approve"')
        (host.home / "config.toml").write_text(text, encoding="utf-8")
        self.refuses_unchanged(host)

    @needs_reader
    def test_an_inline_policy_table_leaves_the_table_unproven(self):
        host = self.ready()
        text = host.config().replace(
            '[mcp_servers.codex-thread-bridge]',
            '[mcp_servers.codex-thread-bridge]\n'
            'tools = { create_thread = { approval_mode = "approve" } }')
        (host.home / "config.toml").write_text(text, encoding="utf-8")
        self.refuses_unchanged(host)

    @needs_reader
    def test_a_fake_header_inside_a_multiline_string_does_not_become_a_table(self):
        """table_span reads lines, and a line inside a multiline string can look exactly like one."""
        host = self.granted(self.ready())
        host.append_config('\n[some.other.table]\nnote = """\n'
                           '[mcp_servers.codex-thread-bridge.tools.create_thread]\n'
                           'approval_mode = "approve"\n"""\n')
        before = host.config()
        code, answer = host.transition("--apply")
        # Whatever it decides, the bystander's string must survive intact and the decision must
        # not have been made by reading that string as structure.
        self.assertIn('note = """', host.config())
        if code == 0:
            self.assertIn("[some.other.table]", host.config())
            self.assertIn("approval_mode", host.config(),
                          "the text inside the multiline string was removed as if it were a table")
        else:
            self.assertEqual(host.config(), before)

    @needs_reader
    def test_an_array_of_tables_named_mcp_servers_is_refused_rather_than_misread(self):
        host = self.ready()
        (host.home / "config.toml").write_text(
            '[[mcp_servers]]\nname = "codex-thread-bridge"\n'
            + host.config(), encoding="utf-8")
        before = host.config()
        code, answer = host.transition("--apply")
        self.assertEqual(code, 1)
        self.assertEqual(host.config(), before)

    # ------------------------------------------------------------- the pre-add gate, exercised
    #
    # The gate judges the CANDIDATE -- the bytes about to be installed -- and carries a payload
    # digest so that running it again against the installed version and comparing digests ties the
    # verdict to what actually landed. Both halves of that are claims until something runs them.

    def candidate(self, edit=None):
        """A copy of this checkout's package, optionally with its declaration edited."""
        root = Path(self.directory) / ("candidate%d" % (len(list(Path(self.directory).glob("candidate*"))) + 1))
        shutil.copytree(ROOT / "plugins" / "crw", root, symlinks=False)
        if edit is not None:
            path = root / "wiring" / "mcp.json"
            document = json.loads(path.read_text(encoding="utf-8"))
            edit(document["mcpServers"]["codex-thread-bridge"])
            path.write_text(json.dumps(document, indent=2) + "\n", encoding="utf-8")
        return root

    @needs_reader
    def test_the_gate_passes_a_candidate_that_preserves_what_this_host_grants(self):
        host = self.granted(self.ready())
        code, answer = host.call("check-declaration", "--package", str(self.candidate()))
        self.assertEqual(code, 0, json.dumps(answer, indent=2)[:1500])
        self.assertEqual(answer["outcome"], "preserves")
        self.assertEqual(answer["granted"], answer["declared"])
        self.assertTrue(answer["payloadDigest"])

    @needs_reader
    def test_the_gate_refuses_a_candidate_that_would_drop_a_gate(self):
        host = self.granted(self.ready())

        def drop(entry):
            entry["tools"].pop("send_message_to_thread")

        code, answer = host.call("check-declaration", "--package", str(self.candidate(drop)))
        self.assertEqual(code, 1)
        self.assertEqual(answer["outcome"], "refused")
        # The package check catches it too, because this package must keep declaring both gates.
        self.assertTrue(answer["refusals"] or answer["packageErrors"])
        self.assertIn("send_message_to_thread",
                      " ".join(answer["refusals"] + answer["packageErrors"]))

    @needs_reader
    def test_the_gate_refuses_a_candidate_that_disagrees_with_this_host(self):
        host = self.ready()
        self.granted(host, '\n[mcp_servers.codex-thread-bridge.tools.create_thread]\n'
                           'approval_mode = "prompt"\n')
        code, answer = host.call("check-declaration", "--package", str(self.candidate()))
        self.assertEqual(code, 1)
        self.assertIn("does not choose between them", " ".join(answer["refusals"]))

    @needs_reader
    def test_the_digest_distinguishes_two_packages_and_matches_an_unmodified_copy(self):
        """What makes the pre-add verdict bindable to the bytes that landed."""
        host = self.granted(self.ready())
        plain = host.call("check-declaration", "--package", str(self.candidate()))[1]

        def widen(entry):
            entry["tools"]["create_thread"]["approval_mode"] = "prompt"

        altered = host.call("check-declaration", "--package", str(self.candidate(widen)))[1]
        self.assertNotEqual(plain["payloadDigest"], altered["payloadDigest"],
                            "two different packages reported the same digest, so comparing"
                            " digests would not tell the operator the bytes had changed")
        # And the installed cache, which is a verbatim copy of the same package, reports the
        # digest the candidate did. That equality is the binding the documented procedure uses.
        cache = host.home / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
        installed = host.call("check-declaration", "--package", str(cache))[1]
        self.assertEqual(installed["payloadDigest"], plain["payloadDigest"])

    @needs_reader
    def test_an_installed_declaration_nobody_can_read_is_not_reported_as_absent(self):
        """Devin finding: unreadable is not absent, and a receipt must not state one as the other."""
        host = self.granted(self.ready())
        self.assertEqual(host.transition("--apply")[0], 0)
        cache = host.home / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
        (cache / "wiring" / "mcp.json").write_text("[]", encoding="utf-8")
        code, answer = host.call("inspect")
        self.assertEqual(code, 0)
        self.assertEqual(answer["policyInEffect"]["state"], "UNREADABLE")
        self.assertEqual(answer["policyInEffect"]["tools"], {})

    @needs_reader
    def test_a_package_that_plainly_declares_another_server_reads_as_absent_not_unreadable(self):
        """Devin finding: an established absence is not a failure to read, and must not say it is."""
        host = self.granted(self.ready())
        self.assertEqual(host.transition("--apply")[0], 0)
        cache = host.home / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
        path = cache / "wiring" / "mcp.json"
        document = json.loads(path.read_text(encoding="utf-8"))
        document["mcpServers"] = {"something-else":
                                  document["mcpServers"]["codex-thread-bridge"]}
        path.write_text(json.dumps(document), encoding="utf-8")
        code, answer = host.call("inspect")
        self.assertEqual(code, 0)
        self.assertEqual(answer["policyInEffect"]["state"], "ABSENT")
        self.assertEqual(answer["policyInEffect"]["tools"], {})
        self.assertIsNone(answer["policyInEffect"]["detail"])

    def declares(self, host, mutate):
        """Rewrite the installed declaration or manifest, then ask inspect what it reads."""
        cache = host.home / "plugins" / "cache" / "crw" / "crw" / PLUGIN_VERSION
        mutate(cache)
        code, answer = host.call("inspect")
        self.assertEqual(code, 0)
        return answer["policyInEffect"]

    @needs_reader
    def test_a_manifest_declaring_an_unusable_mcp_path_is_unreadable_not_absent(self):
        """Devin finding: present and unusable is not the same answer as not declared."""
        host = self.granted(self.ready())
        self.assertEqual(host.transition("--apply")[0], 0)

        def bad_path(cache):
            path = cache / ".codex-plugin" / "plugin.json"
            document = json.loads(path.read_text(encoding="utf-8"))
            document["mcpServers"] = 7
            path.write_text(json.dumps(document), encoding="utf-8")

        found = self.declares(host, bad_path)
        self.assertEqual(found["state"], "UNREADABLE")
        self.assertIsNotNone(found["detail"])

    @needs_reader
    def test_a_server_entry_that_is_not_an_object_is_unreadable_not_absent(self):
        host = self.granted(self.ready())
        self.assertEqual(host.transition("--apply")[0], 0)

        def bad_entry(cache):
            path = cache / "wiring" / "mcp.json"
            document = json.loads(path.read_text(encoding="utf-8"))
            document["mcpServers"]["codex-thread-bridge"] = "not an object"
            path.write_text(json.dumps(document), encoding="utf-8")

        found = self.declares(host, bad_entry)
        self.assertEqual(found["state"], "UNREADABLE")
        self.assertIsNotNone(found["detail"])

    @needs_reader
    def test_a_manifest_that_declares_no_mcp_document_at_all_is_absent(self):
        """The other side of the same split, so neither direction is traded for the other."""
        host = self.granted(self.ready())
        self.assertEqual(host.transition("--apply")[0], 0)

        def no_servers(cache):
            path = cache / ".codex-plugin" / "plugin.json"
            document = json.loads(path.read_text(encoding="utf-8"))
            document.pop("mcpServers")
            path.write_text(json.dumps(document), encoding="utf-8")

        found = self.declares(host, no_servers)
        self.assertEqual(found["state"], "ABSENT")
        self.assertIsNone(found["detail"])
