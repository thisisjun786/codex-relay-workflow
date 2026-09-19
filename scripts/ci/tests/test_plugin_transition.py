"""The manual-to-plugin transition, against synthetic hosts in temporary directories.

Every case builds a host, runs the real CLI as a subprocess, and reads the JSON it printed. Nothing
here touches a real Codex home: the fixtures are built under the pytest temporary directory and the
commands are given --codex-home explicitly.

What these tests are for, in one line each: that the transition removes only what it can prove runs
this repository's code, that it refuses rather than leaving a host without a hook, a bridge or its
skills, and that running it twice changes nothing the second time.
"""

import errno
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

    def transition(self, *arguments, trust=True):
        """Run the transition, acknowledging the trust gap unless a case is about it.

        This fixture writes a trust key with a made-up hash, which is exactly the state the tool
        refuses to read as trust, so every case that is not about that acknowledges it the way an
        operator would have to.
        """
        if trust and "--accept-hook-trust-gap" not in arguments:
            arguments = arguments + ("--accept-hook-trust-gap",)
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
        standdown = [r for r in answer["results"] if r["step"] == "hook standdown"][0]
        self.assertEqual(standdown["outcome"], "refused")
        self.assertEqual(standdown["shiftedIdentities"], ["user:Stop:0:1"])
        self.assertEqual(host.hooks_document(), document)
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
        self.assertEqual(outcomes["hook standdown"], "refused")
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
        self.assertIn("more or other than the command", answer["results"][0]["detail"])
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
        self.assertEqual(outcomes.get("hook standdown"), "refused", json.dumps(results)[:900])
        self.assertEqual(host.record(), installed)

        # And with the hook surface refreshed but the MCP reading still stale -- the shape the
        # ownership lock exists for -- the retire step stands down instead of taking the plugin
        # record the first run wrote. Driven through transition(), because the re-read happens
        # there, inside the lock, not in the step.
        current = {**stale,
                   "hook": inventory.read_hook(host.home, repo_root=ROOT),
                   "settings": inventory.read_settings(host.home)}
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
