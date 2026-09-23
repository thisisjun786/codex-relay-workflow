"""One owner registers the completion Stop hook, and the other is refused with its evidence.

A plugin package declares its hooks in its own manifest, so a plugin-owned registration leaves
the user hook file empty. Every check that reads only the hook file therefore answers "nothing
is registered" while two hooks run on every Stop. These cases exercise the record that closes
that gap, in both directions, because either owner can be installed first.

Nothing here touches a real Codex home, an installed runtime or an operational database. The
relay is a file these cases create, and every command runs against a temporary CODEX_HOME.
"""

import contextlib
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import types
import unittest

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))
sys.path.insert(0, str(ROOT / "scripts" / "ci"))

from crw_runtime import bridgerecord, completion, hooks, reading

import plugin

RUNTIME_INSTALL = ROOT / "scripts" / "runtime_install.py"
BRIDGE_SOURCE = ROOT / "packages" / "codex-thread-bridge" / "src"
BRIDGE_LAUNCHER = ROOT / "plugins" / "crw" / "wiring" / "crw_bridge_mcp.py"

# The bridge's own policy module, loaded from this checkout. It imports nothing outside the
# standard library, so the launcher's variables and digests are compared with the real reader
# here rather than with a copy of its constants.
sys.path.insert(0, str(BRIDGE_SOURCE))
from codex_thread_bridge import execution  # noqa: E402

# A policy the bridge's parser accepts, declaring roles and no allowlist.
POLICY = {"roles": {"parent": {"model": "devin/swe-2", "reasoningEffort": "max"},
                    "child": {"model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}}}
# Another valid policy, so a changed file is a different policy rather than a broken one.
OTHER_POLICY = {"roles": {"parent": {"model": "devin/swe-2", "reasoningEffort": "max"},
                          "child": {"model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"}}}

# A launcher as it stood before record version 2, reduced to what it did with a record: stand down
# for another owner, refuse any version but 1, and exec what the record names.
V1_LAUNCHER = (
    "import json, os, sys\n"
    "from pathlib import Path\n"
    "record = Path(os.environ['CODEX_HOME']) / 'crw-bridge-mcp.json'\n"
    "document = json.loads(record.read_text())\n"
    "if document.get('recordVersion') != 1:\n"
    "    sys.stderr.write('crw bridge launcher: the record is version %r' %"
    " document.get('recordVersion'))\n"
    "    raise SystemExit(2)\n"
    "executable = document['bridgeExecutable']\n"
    "os.execv(executable, [executable, *document['args']])\n"
)


def write_policy(path, mapping=POLICY):
    data = json.dumps(mapping, indent=2).encode("utf-8")
    Path(path).write_bytes(data)
    return hashlib.sha256(data).hexdigest()


def load_launcher():
    """The packaged launcher as a module. Its main() runs only under __main__.

    Compiled from its source rather than imported, because an import writes a __pycache__ into
    the plugin root, and everything in that directory ships: the payload check then refuses it.
    """
    module = types.ModuleType("crw_bridge_mcp_under_test")
    module.__file__ = str(BRIDGE_LAUNCHER)
    exec(compile(BRIDGE_LAUNCHER.read_text(encoding="utf-8"), str(BRIDGE_LAUNCHER), "exec"),
         module.__dict__)
    return module

try:  # The configuration reader arrived in 3.11 and this repository still supports 3.10.
    import tomllib  # noqa: F401
    TOML_READER = True
except ImportError:
    TOML_READER = False


def run(*arguments):
    """One command, its exit status, and the JSON it emitted. Never one without the others."""
    finished = subprocess.run([sys.executable, str(RUNTIME_INSTALL), *arguments],
                              capture_output=True, text=True)
    try:
        emitted = json.loads(finished.stdout)
    except ValueError:
        emitted = None
    return finished.returncode, emitted, finished.stdout + finished.stderr


class Home:
    """A temporary Codex home with a destination whose pointer names a relay that exists."""

    def __init__(self, stack):
        root = Path(stack.enter_context(tempfile.TemporaryDirectory(prefix="crw114-")))
        self.codex_home = root / "codex"
        self.destination = root / "dest"
        binaries = self.destination / "current" / "bin"
        binaries.mkdir(parents=True)
        relay = binaries / "codex-session-relay"
        relay.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        relay.chmod(0o755)
        self.codex_home.mkdir(parents=True)

    def hook(self, *extra):
        return run("hook", "--adapter", "completion", "--codex-home", str(self.codex_home),
                   "--dest", str(self.destination), *extra)

    @property
    def settings(self):
        return self.codex_home / completion.CONFIG_NAME

    @property
    def hook_file(self):
        return self.codex_home / "hooks.json"

    def settings_document(self):
        return json.loads(self.settings.read_text(encoding="utf-8"))

    def registrations(self):
        found = hooks.read(self.hook_file)
        if not found.usable or not isinstance(found.value, dict):
            return []
        return completion.adapter_entries(found.value, completion.EVENT)


class OwnershipTest(unittest.TestCase):

    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.home = Home(self.stack)

    # ------------------------------------------------------------------ the user owner

    def test_user_owner_document_carries_no_owner_key(self):
        """The default document stays what installed hosts already hold, byte for byte.

        Adding the key to what this command generates would make an ordinary reinstall differ
        from the file on disk, and write_configuration refuses settings that say something else.
        A compatibility break disguised as a new field.
        """
        status, emitted, output = self.home.hook("--apply")
        self.assertEqual(status, 0, output)
        self.assertNotIn("owner", self.home.settings_document())
        self.assertEqual(completion.owner_of(self.home.settings_document()),
                         completion.OWNER_USER)
        self.assertEqual(len(self.home.registrations()), 1, output)

    def test_user_owner_reinstall_is_unchanged(self):
        status, _, _ = self.home.hook("--apply")
        self.assertEqual(status, 0)
        status, emitted, output = self.home.hook("--apply")
        self.assertEqual(status, 0, output)
        self.assertEqual(emitted["settings"]["outcome"], completion.CONFIG_UNCHANGED)
        self.assertEqual(len(self.home.registrations()), 1, output)

    # ------------------------------------------------------------------ the plugin owner

    def test_plugin_owner_writes_settings_and_registers_nothing(self):
        status, emitted, output = self.home.hook("--owner", "plugin", "--apply")
        self.assertEqual(status, 0, output)
        self.assertTrue(self.home.settings.exists(), output)
        self.assertFalse(self.home.hook_file.exists(),
                         "the plugin owner must not touch the hook file: " + output)
        self.assertEqual(emitted["registrations"], [])
        self.assertIsNone(emitted["result"])

    def test_plugin_owner_records_the_adapter_the_launcher_has_to_reach(self):
        """A packaged registration cannot resolve this checkout, so the install records it."""
        status, _, output = self.home.hook("--owner", "plugin", "--apply")
        self.assertEqual(status, 0, output)
        document = self.home.settings_document()
        self.assertEqual(document["owner"], completion.OWNER_PLUGIN)
        self.assertEqual(Path(document["adapterEntryPoint"]),
                         ROOT / "scripts" / completion.ENTRY_POINT_NAME)
        self.assertTrue(Path(document["adapterInterpreter"]).is_absolute())

    def test_plugin_settings_missing_the_adapter_are_not_readable(self):
        """The reader refuses a plugin document the launcher could not act on."""
        document = completion.configuration(relay="/opt/relay/bin/codex-session-relay",
                                            codex_home="/tmp/codex",
                                            owner=completion.OWNER_PLUGIN)
        document["adapterEntryPoint"] = None
        self.assertTrue(any("adapterEntryPoint" in complaint
                            for complaint in completion.complaints(document)))

    def test_owner_must_be_one_this_reader_knows(self):
        document = completion.configuration(relay="/opt/relay/bin/codex-session-relay",
                                            codex_home="/tmp/codex")
        document["owner"] = "marketplace"
        self.assertTrue(any("owner" in complaint
                            for complaint in completion.complaints(document)))
        self.assertEqual(completion.owner_of({"owner": "marketplace"}), completion.OWNER_USER)

    # ------------------------------------------------------------------ the refusals

    def test_user_owner_is_refused_when_the_plugin_owns_the_event(self):
        status, _, output = self.home.hook("--owner", "plugin", "--apply")
        self.assertEqual(status, 0, output)
        status, emitted, output = self.home.hook("--apply")
        self.assertNotEqual(status, 0, output)
        self.assertIn(completion.OWNER_PLUGIN, emitted["error"])
        self.assertIsNone(emitted["settings"])
        self.assertFalse(self.home.hook_file.exists(), output)

    def test_plugin_owner_is_refused_when_the_hook_file_already_registers_it(self):
        status, _, output = self.home.hook("--apply")
        self.assertEqual(status, 0, output)
        before = self.home.settings.read_bytes()
        status, emitted, output = self.home.hook("--owner", "plugin", "--apply")
        self.assertNotEqual(status, 0, output)
        self.assertEqual(emitted["registrations"], ["user:Stop:0:0"])
        self.assertEqual(self.home.settings.read_bytes(), before,
                         "a refused run writes nothing: " + output)

    def test_an_unreadable_hook_file_refuses_both_owners(self):
        self.home.hook_file.write_text("{ not json", encoding="utf-8")
        for extra in ((), ("--owner", "plugin")):
            status, emitted, output = self.home.hook(*extra)
            self.assertNotEqual(status, 0, output)
            self.assertIsNone(emitted["settings"], output)

    def test_owner_is_not_accepted_for_an_explicit_hook_command(self):
        """--owner names who registers an adapter this repository owns, and nothing else."""
        status, emitted, output = run("hook", "--codex-home", str(self.home.codex_home),
                                      "--hook-command", "/bin/true", "--owner", "plugin",
                                      "--apply")
        self.assertNotEqual(status, 0, output)
        self.assertIn("--owner", emitted["error"])
        self.assertFalse(self.home.hook_file.exists(), output)

    def test_a_plan_writes_nothing_for_either_owner(self):
        for extra in ((), ("--owner", "plugin")):
            status, emitted, output = self.home.hook(*extra)
            self.assertEqual(status, 0, output)
            self.assertFalse(self.home.settings.exists(), output)
            self.assertFalse(self.home.hook_file.exists(), output)

    def test_a_guard_budget_the_launcher_cannot_outlast_is_refused(self):
        status, emitted, output = self.home.hook(
            "--owner", "plugin", "--guard-timeout",
            str(completion.LAUNCHER_CEILING_SECONDS), "--apply")
        self.assertNotEqual(status, 0, output)
        self.assertFalse(self.home.settings.exists(),
                         "a refused run writes nothing: " + output)

    # ------------------------------------------------------------ what hook-status can say

    def status(self):
        _, emitted, output = run("hook-status", "--codex-home", str(self.home.codex_home))
        self.assertIsNotNone(emitted, output)
        return emitted

    def test_status_probes_both_programs_the_launcher_starts(self):
        """A plugin-owned hook has no hook-file entry, so the registration cells answer about
        a file and say nothing about this host. Either recorded program can go missing alone."""
        self.assertEqual(self.home.hook("--owner", "plugin", "--apply")[0], 0)
        answer = self.status()
        self.assertEqual(answer["adapterEntryPoint"]["value"], reading.PRESENT, answer)
        self.assertEqual(answer["adapterInterpreter"]["value"], reading.PRESENT, answer)

    def test_status_names_a_recorded_interpreter_that_is_gone(self):
        self.assertEqual(self.home.hook("--owner", "plugin", "--apply")[0], 0)
        document = json.loads(self.home.settings.read_text(encoding="utf-8"))
        document["adapterInterpreter"] = str(self.home.codex_home / "removed-venv" / "python")
        self.home.settings.write_text(json.dumps(document), encoding="utf-8")
        answer = self.status()
        self.assertNotEqual(answer["adapterInterpreter"]["value"], reading.PRESENT, answer)

    def test_status_reports_the_owner_without_claiming_a_registration(self):
        """Writing the settings registers nothing, and this command reads no installed package,
        so it must not name one as the place the hook is registered."""
        self.assertEqual(self.home.hook("--owner", "plugin", "--apply")[0], 0)
        answer = self.status()
        self.assertEqual(answer["registrationOwner"]["value"], completion.OWNER_PLUGIN, answer)
        self.assertIsNone(answer["registrationOwner"]["registeredWhere"], answer)
        self.assertIn(completion.OWNER_PLUGIN, answer["registrationOwner"]["note"], answer)
        # The hook-file cell keeps answering about the hook file, which holds nothing.
        self.assertNotEqual(answer["registration"]["value"], reading.PRESENT, answer)

    def test_status_still_points_the_user_owner_at_the_hook_file(self):
        self.assertEqual(self.home.hook("--apply")[0], 0)
        answer = self.status()
        self.assertEqual(answer["registrationOwner"]["value"], completion.OWNER_USER, answer)
        self.assertEqual(answer["registrationOwner"]["registeredWhere"],
                         str(self.home.codex_home / "hooks.json"), answer)


    def test_the_same_budget_is_still_accepted_for_the_user_owner(self):
        """The ceiling belongs to the packaged launcher, which the user owner does not run."""
        status, emitted, output = self.home.hook(
            "--guard-timeout", str(completion.LAUNCHER_CEILING_SECONDS), "--apply")
        self.assertEqual(status, 0, output)

    def install_with_override(self, override, *extra):
        environment = dict(os.environ, CRW_COMPLETION_HOOK_CONFIG=override)
        return subprocess.run(
            [sys.executable, str(RUNTIME_INSTALL), "hook", "--adapter", "completion",
             "--codex-home", str(self.home.codex_home), "--dest", str(self.home.destination),
             *extra, "--apply"], capture_output=True, text=True, env=environment)

    def test_no_settings_override_survives_a_plugin_install(self):
        """Relative or absolute, the hook rediscovers the path from the session's own
        environment and not from this one, so neither spelling can be relied on.

        An absolute override is the worse of the two, because installation succeeds and every
        later Stop reads the Codex home where nothing was written.
        """
        for override in ("hook/settings.json",
                         str(self.home.codex_home / "elsewhere" / "settings.json")):
            finished = self.install_with_override(override, "--owner", "plugin")
            self.assertNotEqual(finished.returncode, 0, override + ": " + finished.stdout)
            self.assertIn("CRW_COMPLETION_HOOK_CONFIG", finished.stdout, override)
            self.assertFalse(self.home.settings.exists(), finished.stdout)
            self.assertFalse((self.home.codex_home / "elsewhere").exists(), finished.stdout)

    def test_plugin_settings_land_where_the_launcher_derives_them(self):
        """The invariant the refusal exists to leave behind."""
        self.assertEqual(self.home.hook("--owner", "plugin", "--apply")[0], 0)
        self.assertEqual(self.home.settings,
                         self.home.codex_home / completion.CONFIG_NAME)
        self.assertTrue(self.home.settings.exists())

    def test_the_user_owner_still_takes_an_override(self):
        """It writes the path it resolved into the command it registers, so it survives."""
        settled = self.home.codex_home / "elsewhere" / "settings.json"
        finished = self.install_with_override(str(settled))
        self.assertEqual(finished.returncode, 0, finished.stdout + finished.stderr)
        self.assertTrue(settled.exists(), finished.stdout)



# ---------------------------------------------------------------- the bridge MCP record


class BridgeRecordTest(unittest.TestCase):

    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.home = Home(self.stack)
        self.bridge = self.home.destination / "current" / "bin" / "codex-thread-bridge"
        self.bridge.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        self.bridge.chmod(0o755)

    def register(self, *extra):
        return run("register-mcp", "--codex-home", str(self.home.codex_home),
                   "--bridge-command", str(self.bridge), *extra)

    @property
    def record(self):
        return self.home.codex_home / bridgerecord.RECORD_NAME

    def test_a_relative_bridge_command_is_refused(self):
        """The launcher runs from the installed package, the one place a runtime must not be."""
        with self.assertRaises(ValueError):
            bridgerecord.document(command="./codex-thread-bridge")

    def test_plugin_owner_writes_the_record_and_not_the_configuration(self):
        status, emitted, output = self.register("--owner", "plugin", "--apply")
        self.assertEqual(status, 0, output)
        self.assertTrue(self.record.exists(), output)
        self.assertFalse((self.home.codex_home / "config.toml").exists(),
                         "the plugin owner must not write the configuration: " + output)
        document = json.loads(self.record.read_text(encoding="utf-8"))
        self.assertEqual(document["owner"], bridgerecord.OWNER_PLUGIN)
        self.assertEqual(document["bridgeExecutable"], str(self.bridge))

    def test_user_owner_is_refused_when_the_plugin_owns_the_server(self):
        self.assertEqual(self.register("--owner", "plugin", "--apply")[0], 0)
        status, emitted, output = self.register("--apply")
        self.assertNotEqual(status, 0, output)
        self.assertIn(bridgerecord.OWNER_PLUGIN, emitted["detail"])
        self.assertFalse((self.home.codex_home / "config.toml").exists(), output)

    def test_plugin_owner_is_refused_when_the_configuration_registers_it(self):
        # The entry is written as text rather than by registering it, because registering reads
        # the file back and that reader arrived in Python 3.11. This case is about the refusal,
        # and the refusal has to hold on the documented minimum interpreter too.
        (self.home.codex_home / "config.toml").write_text(
            "[mcp_servers.codex-thread-bridge]\ncommand = " + json.dumps(str(self.bridge))
            + "\nargs = []\n", encoding="utf-8")
        status, emitted, output = self.register("--owner", "plugin", "--apply")
        self.assertNotEqual(status, 0, output)
        self.assertFalse(self.record.exists(), "a refused run writes nothing: " + output)
        if TOML_READER:
            self.assertIn("already starts this bridge", emitted["detail"], output)
        else:
            # Without the reader the question was not answered, and an unanswered question
            # refuses rather than defaulting. That is the same guarantee, stated as itself.
            self.assertIn("not established", emitted["detail"], output)

    def test_a_record_that_says_something_else_is_not_overwritten(self):
        self.assertEqual(self.register("--owner", "plugin", "--apply")[0], 0)
        other = self.home.destination / "current" / "bin" / "other-bridge"
        other.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        before = self.record.read_bytes()
        status, emitted, output = run("register-mcp", "--codex-home", str(self.home.codex_home),
                                      "--bridge-command", str(other), "--owner", "plugin",
                                      "--apply")
        self.assertNotEqual(status, 0, output)
        self.assertEqual(self.record.read_bytes(), before, output)

    def test_a_plan_writes_nothing(self):
        status, emitted, output = self.register("--owner", "plugin")
        self.assertEqual(status, 0, output)
        self.assertFalse(self.record.exists(), output)

    def test_a_record_that_could_not_be_read_refuses_both_owners(self):
        """Not only the malformed one. Every way of not reading it leaves ownership unknown."""
        self.record.write_text("{ not json", encoding="utf-8")
        for extra in ((), ("--owner", "plugin")):
            status, emitted, output = self.register(*extra)
            self.assertNotEqual(status, 0, output)
            self.assertIn("not established", emitted["detail"], output)
            self.assertFalse((self.home.codex_home / "config.toml").exists(), output)

    def test_the_plugin_owner_registers_only_the_declared_name(self):
        """A custom name would check one entry and start another."""
        status, emitted, output = self.register("--owner", "plugin", "--name", "alias",
                                                "--apply")
        self.assertNotEqual(status, 0, output)
        self.assertIn("alias", emitted["detail"], output)
        self.assertFalse(self.record.exists(), output)

    @unittest.skipUnless(TOML_READER, "reading the configuration needs Python 3.11")
    def test_a_legacy_alias_is_found_by_what_it_starts(self):
        """A registration made before the record existed carries no owner anywhere.

        The configuration is its only evidence and --name chose the table it sits under, so
        checking the declared name alone looks straight past it and the plugin record is
        written beside a bridge that is already registered.
        """
        self.configuration().write_text(
            "[mcp_servers.team-bridge]\ncommand = " + json.dumps(str(self.bridge))
            + "\nargs = []\n", encoding="utf-8")
        before = self.configuration().read_bytes()
        status, emitted, output = self.register("--owner", "plugin", "--apply")
        self.assertNotEqual(status, 0, output)
        self.assertIn("team-bridge", emitted["detail"], output)
        self.assertFalse(self.record.exists(), "a refused run writes nothing: " + output)
        self.assertEqual(self.configuration().read_bytes(), before, output)

    @unittest.skipUnless(TOML_READER, "reading the configuration needs Python 3.11")
    def test_a_legacy_alias_under_another_destination_is_found_too(self):
        """Two destinations are still two bridges, so the console script name is enough."""
        self.configuration().write_text(
            "[mcp_servers.team-bridge]\n"
            "command = \"/somewhere/else/current/bin/codex-thread-bridge\"\nargs = []\n",
            encoding="utf-8")
        status, emitted, output = self.register("--owner", "plugin", "--apply")
        self.assertNotEqual(status, 0, output)
        self.assertFalse(self.record.exists(), output)

    @unittest.skipUnless(TOML_READER, "reading the configuration needs Python 3.11")
    def test_an_unrelated_server_does_not_block_the_plugin_owner(self):
        """The positive control: this must recognise a bridge, not every registration."""
        self.configuration().write_text(
            "[mcp_servers.unrelated]\ncommand = \"/bin/true\"\nargs = []\n", encoding="utf-8")
        status, emitted, output = self.register("--owner", "plugin", "--apply")
        self.assertEqual(status, 0, output)
        self.assertTrue(self.record.exists(), output)

    @unittest.skipUnless(TOML_READER, "reading the configuration needs Python 3.11")
    def test_the_user_owner_is_refused_a_second_name_for_a_legacy_bridge(self):
        """The same scan the plugin owner got. A legacy alias carries no record either way."""
        self.configuration().write_text(
            "[mcp_servers.team-bridge]\ncommand = " + json.dumps(str(self.bridge))
            + "\nargs = []\n", encoding="utf-8")
        before = self.configuration().read_bytes()
        status, emitted, output = self.register("--apply")
        self.assertNotEqual(status, 0, output)
        self.assertIn("team-bridge", emitted["detail"], output)
        self.assertEqual(self.configuration().read_bytes(), before, output)
        self.assertFalse(self.record.exists(), output)

    @unittest.skipUnless(TOML_READER, "reading the configuration needs Python 3.11")
    def test_a_legacy_registration_can_acquire_its_own_record(self):
        """The migration route has to stay open: registering under the name already there
        adds no second table, so it is the one bridge entry that must not refuse."""
        self.configuration().write_text(
            "[mcp_servers.team-bridge]\ncommand = " + json.dumps(str(self.bridge))
            + "\nargs = []\n", encoding="utf-8")
        status, emitted, output = self.register("--name", "team-bridge", "--apply")
        self.assertEqual(status, 0, output)
        self.assertEqual(self.sections(), ["team-bridge"], output)
        self.assertEqual(json.loads(self.record.read_text())["serverName"], "team-bridge")

    @unittest.skipUnless(TOML_READER, "reading the configuration needs Python 3.11")
    def test_a_different_issue_does_not_break_an_unchanged_registration(self):
        """installedBy is evidence about who wrote the record, not part of what Codex starts."""
        self.assertEqual(self.register("--apply")[0], 0)
        before = self.record.read_bytes()
        status, emitted, output = self.register("--issue", "CRW-200", "--apply")
        self.assertEqual(status, 0, output)
        self.assertEqual(emitted["record"]["outcome"], bridgerecord.UNCHANGED, output)
        self.assertEqual(self.record.read_bytes(), before,
                         "the recorded issue is left where it is: " + output)

    @unittest.skipUnless(TOML_READER, "reading the configuration needs Python 3.11")
    def test_a_record_this_launcher_could_not_act_on_is_not_unchanged(self):
        """Identity is not enough to call a record installed. A version the launcher refuses
        would otherwise let this command exit 0 over a bridge that cannot start."""
        self.assertEqual(self.register("--apply")[0], 0)
        document = json.loads(self.record.read_text(encoding="utf-8"))
        document["recordVersion"] = bridgerecord.RECORD_VERSION + 1
        self.record.write_text(json.dumps(document), encoding="utf-8")
        status, emitted, output = self.register("--apply")
        self.assertNotEqual(status, 0, output)

    def test_the_record_identity_is_what_codex_starts(self):
        base = bridgerecord.document(command="/opt/x/bin/codex-thread-bridge",
                                     name="codex-thread-bridge", issue="CRW-114")
        relabelled = bridgerecord.document(command="/opt/x/bin/codex-thread-bridge",
                                           name="codex-thread-bridge", issue="CRW-200")
        self.assertTrue(bridgerecord.same_registration(base, relabelled))
        for changed in (bridgerecord.document(command="/opt/y/bin/codex-thread-bridge",
                                              name="codex-thread-bridge"),
                        bridgerecord.document(command="/opt/x/bin/codex-thread-bridge",
                                              name="alias"),
                        bridgerecord.document(command="/opt/x/bin/codex-thread-bridge",
                                              name="codex-thread-bridge",
                                              arguments=["--socket", "/tmp/s"])):
            self.assertFalse(bridgerecord.same_registration(base, changed), changed)

    # ------------------------------------------------------------ the record decides first

    def configuration(self):
        return self.home.codex_home / "config.toml"

    def sections(self):
        if not self.configuration().exists():
            return []
        prefix = "[mcp_servers."
        return sorted(line.strip()[len(prefix):-1].strip('"') for line
                      in self.configuration().read_text(encoding="utf-8").splitlines()
                      if line.strip().startswith(prefix) and line.strip().endswith("]"))

    def assert_refused_without_touching_the_configuration(self, before, status, emitted,
                                                          output):
        """A refusal that already appended a table is the failure, not the exit status.

        Asserted on the bytes rather than on the section list, because a rewrite that happened
        to produce the same set of names would still mean the file was replaced under a run
        that reported it had written nothing.
        """
        self.assertNotEqual(status, 0, output)
        self.assertFalse(emitted["wrote"], output)
        self.assertEqual(self.configuration().read_bytes(), before,
                         "a refused run leaves the configuration byte-identical: " + output)

    @unittest.skipUnless(TOML_READER, "registering reads the configuration back, and that"
                                      " reader arrived in Python 3.11")
    def test_a_second_name_is_refused_before_the_configuration_is_touched(self):
        """alias first, then the default name.

        Without the pre-check the second run appends its own table and only then finds the
        record naming the first, so the command reports a refusal while the host carries two
        bridge registrations.
        """
        self.assertEqual(self.register("--name", "alias", "--apply")[0], 0)
        self.assertEqual(self.sections(), ["alias"])
        before = self.configuration().read_bytes()
        status, emitted, output = self.register("--apply")
        self.assert_refused_without_touching_the_configuration(before, status, emitted, output)
        self.assertIn("serverName", emitted["detail"], output)
        self.assertEqual(self.sections(), ["alias"], output)

    @unittest.skipUnless(TOML_READER, "registering reads the configuration back, and that"
                                      " reader arrived in Python 3.11")
    def test_the_other_order_is_refused_the_same_way(self):
        """The default name first, then an alias. Either can be installed first."""
        self.assertEqual(self.register("--apply")[0], 0)
        self.assertEqual(self.sections(), ["codex-thread-bridge"])
        before = self.configuration().read_bytes()
        status, emitted, output = self.register("--name", "alias", "--apply")
        self.assert_refused_without_touching_the_configuration(before, status, emitted, output)
        self.assertEqual(self.sections(), ["codex-thread-bridge"], output)

    @unittest.skipUnless(TOML_READER, "registering reads the configuration back, and that"
                                      " reader arrived in Python 3.11")
    def test_a_record_naming_another_command_refuses_before_the_write_too(self):
        """The same class wearing a different face: the name agrees and the command does not."""
        other = self.home.destination / "current" / "bin" / "other-bridge"
        other.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        other.chmod(0o755)
        self.assertEqual(self.register("--apply")[0], 0)
        # The entry is removed so the configuration itself raises no conflict, which is what
        # leaves the record as the only thing that can catch the disagreement.
        text = self.configuration().read_text(encoding="utf-8")
        self.configuration().write_text(text.split("[mcp_servers.codex-thread-bridge]")[0],
                                        encoding="utf-8")
        before = self.configuration().read_bytes()
        status, emitted, output = run("register-mcp", "--codex-home",
                                      str(self.home.codex_home), "--bridge-command",
                                      str(other), "--apply")
        self.assert_refused_without_touching_the_configuration(before, status, emitted, output)
        self.assertIn("bridgeExecutable", emitted["detail"], output)

    @unittest.skipUnless(TOML_READER, "registering reads the configuration back, and that"
                                      " reader arrived in Python 3.11")
    def test_an_unchanged_reregistration_is_still_accepted(self):
        """The positive control: the refusal above must not swallow an ordinary rerun."""
        self.assertEqual(self.register("--apply")[0], 0)
        status, emitted, output = self.register("--apply")
        self.assertEqual(status, 0, output)
        self.assertEqual(emitted["record"]["outcome"], bridgerecord.UNCHANGED, output)

    @unittest.skipUnless(TOML_READER, "registering reads the configuration back, and that"
                                      " reader arrived in Python 3.11")
    def test_a_bridge_command_this_command_has_always_taken_still_registers(self):
        """A user-owned record is read for its owner and never executed, so it carries what
        the configuration registered rather than what a packaged launcher would need."""
        status, emitted, output = run("register-mcp", "--codex-home",
                                      str(self.home.codex_home), "--bridge-command",
                                      "codex-thread-bridge", "--apply")
        self.assertEqual(status, 0, output)
        self.assertEqual(self.sections(), ["codex-thread-bridge"], output)
        self.assertEqual(json.loads(self.record.read_text())["bridgeExecutable"],
                         "codex-thread-bridge")

    def test_the_plugin_owner_still_needs_an_absolute_command(self):
        """Relaxing the record for the user owner must not relax the launcher's own path."""
        with self.assertRaises(ValueError):
            bridgerecord.document(command="codex-thread-bridge",
                                  owner=bridgerecord.OWNER_PLUGIN)
        self.assertEqual(bridgerecord.document(command="codex-thread-bridge",
                                               owner=bridgerecord.OWNER_USER)["owner"],
                         bridgerecord.OWNER_USER)


class BridgeRecordPolicyTest(unittest.TestCase):
    """register-mcp --execution-policy: the record names the file and its digest, and nothing else.

    The policy is judged by the bridge's own parser before anything is written, a rerun that
    changes nothing writes nothing, and every other difference is refused like any other conflict.
    """

    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.home = Home(self.stack)
        self.bridge = self.home.destination / "current" / "bin" / "codex-thread-bridge"
        self.bridge.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        self.bridge.chmod(0o755)
        self.policy = self.home.destination.parent / "execution-policy.json"
        self.digest = write_policy(self.policy)

    @property
    def record(self):
        return self.home.codex_home / bridgerecord.RECORD_NAME

    def register(self, *extra, policy=None):
        arguments = ["register-mcp", "--codex-home", str(self.home.codex_home),
                     "--bridge-command", str(self.bridge), "--owner", "plugin"]
        if policy is not False:
            arguments += ["--execution-policy", str(policy or self.policy)]
        return run(*arguments, *extra)

    def install_package(self, launcher_text=None, *, marketplace="crw", plugin="crw",
                        version="0.4.0"):
        """A cached package; the shipped crw one unless a launcher is given."""
        root = self.home.codex_home / "plugins" / "cache" / marketplace / plugin / version
        shutil.copytree(ROOT / "plugins" / "crw", root)
        if launcher_text is not None:
            (root / "wiring" / "crw_bridge_mcp.py").write_text(launcher_text, encoding="utf-8")
        return root / "wiring" / "crw_bridge_mcp.py"

    def enable(self, *keys, enabled=True):
        """Register plugins in the Codex configuration the way an installation does."""
        text = "".join('[plugins."' + key + '"]\nenabled = ' + ("true" if enabled else "false")
                       + "\n" for key in keys)
        (self.home.codex_home / "config.toml").write_text(text, encoding="utf-8")

    @staticmethod
    def older_launcher():
        """A launcher from before version 2: it reads the record and starts only version 1."""
        return V1_LAUNCHER

    @staticmethod
    def shipped_launcher_with(old, new):
        """The shipped launcher with one behaviour taken away, still declaring version 2."""
        shipped = BRIDGE_LAUNCHER.read_text(encoding="utf-8")
        assert shipped.count(old) == 1, old
        assert "POLICY_RECORD_VERSION = " + str(bridgerecord.POLICY_RECORD_VERSION) in shipped
        return shipped.replace(old, new)

    def test_the_record_names_the_file_and_its_digest_and_nothing_it_says(self):
        status, emitted, output = self.register("--apply")
        self.assertEqual(status, 0, output)
        document = json.loads(self.record.read_text(encoding="utf-8"))
        self.assertEqual(document["recordVersion"], bridgerecord.POLICY_RECORD_VERSION)
        self.assertEqual(document["executionPolicy"],
                         {"path": str(self.policy), "digest": self.digest})
        self.assertEqual(bridgerecord.complaints(document), [])
        # The record carries no policy content. The report carries what get_capabilities
        # discloses anyway, the mode and the role pairs, and says whose parser judged it.
        raw = self.record.read_text(encoding="utf-8")
        for pair in POLICY["roles"].values():
            self.assertNotIn(pair["model"], raw)
        self.assertEqual(emitted["executionPolicy"]["digest"], self.digest)
        self.assertEqual(emitted["executionPolicy"]["roles"],
                         execution.ExecutionPolicy.from_mapping(POLICY).summary()["roles"])
        self.assertIn(str(BRIDGE_SOURCE), emitted["executionPolicy"]["parsedWith"])
        self.assertFalse((self.home.codex_home / "config.toml").exists(), output)

    def test_a_plan_judges_the_policy_and_writes_nothing(self):
        status, emitted, output = self.register()
        self.assertEqual(status, 0, output)
        self.assertEqual(emitted["outcome"], bridgerecord.WOULD_CREATE, output)
        self.assertEqual(emitted["executionPolicy"]["digest"], self.digest, output)
        self.assertFalse(self.record.exists(), output)

    def test_an_unchanged_rerun_is_unchanged_and_writes_nothing(self):
        self.assertEqual(self.register("--apply")[0], 0)
        before = self.record.read_bytes()
        status, emitted, output = self.register("--apply")
        self.assertEqual(status, 0, output)
        self.assertEqual(emitted["outcome"], bridgerecord.UNCHANGED, output)
        self.assertEqual(self.record.read_bytes(), before, output)

    def test_an_edited_policy_is_a_different_registration_and_is_refused(self):
        """Another VALID policy at the same path: the digest is what differs."""
        self.assertEqual(self.register("--apply")[0], 0)
        before = self.record.read_bytes()
        write_policy(self.policy, OTHER_POLICY)
        status, emitted, output = self.register("--apply")
        self.assertNotEqual(status, 0, output)
        self.assertEqual(emitted["outcome"], bridgerecord.DIFFERS, output)
        self.assertIn(bridgerecord.POLICY_FIELD, emitted["differingFields"], output)
        self.assertIn("aside", emitted["repair"], output)
        self.assertEqual(self.record.read_bytes(), before, output)

    def test_every_other_difference_in_the_policy_is_refused_too(self):
        self.assertEqual(self.register("--apply")[0], 0)
        before = self.record.read_bytes()
        other = self.home.destination.parent / "other-policy.json"
        write_policy(other)
        for label, policy in (("another file with the same bytes", other),
                              ("no policy at all", False)):
            with self.subTest(label):
                status, emitted, output = self.register("--apply", policy=policy)
                self.assertNotEqual(status, 0, output)
                self.assertEqual(emitted["outcome"], bridgerecord.DIFFERS, output)
                self.assertEqual(self.record.read_bytes(), before, output)

    def test_a_record_written_before_the_field_existed_is_not_rewritten_in_place(self):
        """Adding a policy changes what the bridge starts under, so it is a conflict as well."""
        self.assertEqual(self.register("--apply", policy=False)[0], 0)
        before = self.record.read_bytes()
        self.assertEqual(json.loads(before)["recordVersion"], bridgerecord.RECORD_VERSION)
        status, emitted, output = self.register("--apply")
        self.assertNotEqual(status, 0, output)
        self.assertIn("aside", emitted["repair"], output)
        self.assertEqual(self.record.read_bytes(), before, output)
        # The repair it names works: once the old record is out of the way, the same run lands.
        self.record.rename(self.record.with_name(bridgerecord.RECORD_NAME + ".pre-policy"))
        status, emitted, output = self.register("--apply")
        self.assertEqual(status, 0, output)
        self.assertEqual(json.loads(self.record.read_text())["executionPolicy"]["digest"],
                         self.digest)

    def test_a_policy_the_bridge_would_refuse_is_never_recorded(self):
        contradiction = {"allowed": [{"model": "devin/swe-2", "efforts": ["max"]}],
                         "roles": POLICY["roles"]}
        broken = self.home.destination.parent / "broken.json"
        cases = {
            "not JSON": (lambda: broken.write_text("{ not json", encoding="utf-8"), broken),
            "a role pair its own allowlist omits":
                (lambda: write_policy(broken, contradiction), broken),
            "no such file": (lambda: None, self.home.destination.parent / "absent.json"),
            # A file by the padded name exists and parses, so the padding is the only refusal.
            "a padded path": (lambda: write_policy(Path(str(self.policy) + " ")),
                              str(self.policy) + " "),
            "an unknown user's home": (lambda: None, "~crw218-no-such-user/policy.json"),
        }
        for label, (prepare, policy) in cases.items():
            with self.subTest(label):
                prepare()
                status, emitted, output = self.register("--apply", policy=policy)
                self.assertNotEqual(status, 0, output)
                self.assertEqual(emitted["outcome"], "execution_policy_unreadable", output)
                self.assertFalse(emitted["wrote"], output)
                self.assertFalse(self.record.exists(), output)

    def test_a_pipe_named_as_the_policy_is_refused_without_holding_the_lock(self):
        """Registration decides under the ownership lock, so a read that blocks holds every run."""
        pipe = self.home.destination.parent / "policy.fifo"
        os.mkfifo(pipe)
        finished = subprocess.run(
            [sys.executable, str(RUNTIME_INSTALL), "register-mcp", "--codex-home",
             str(self.home.codex_home), "--bridge-command", str(self.bridge), "--owner",
             "plugin", "--execution-policy", str(pipe), "--apply"],
            capture_output=True, text=True, timeout=60)
        emitted = json.loads(finished.stdout)
        self.assertEqual(finished.returncode, 1, finished.stdout + finished.stderr)
        self.assertEqual(emitted["outcome"], "execution_policy_unreadable")
        self.assertIn("not a regular file", emitted["detail"])
        self.assertFalse(self.record.exists())
        # And the lock was released: the next run is not held up by the refused one.
        self.assertEqual(self.register("--apply")[0], 0)

    def test_a_path_this_system_cannot_encode_is_a_refusal_and_not_a_traceback(self):
        import runtime_install
        policy, why = runtime_install._execution_policy_reading(str(self.policy) + "\ud800")
        self.assertIsNone(policy)
        self.assertIn("encoded", why)
        self.assertNotEqual(bridgerecord.policy_path_complaints("/p\ud800"), [])

    def test_reading_a_record_that_is_a_pipe_answers_without_blocking(self):
        """Every reader of the record goes through this, register-mcp and the transition too."""
        pipe = self.home.destination.parent / "record.fifo"
        os.mkfifo(pipe)
        done = subprocess.run(
            [sys.executable, "-c",
             "import sys\nsys.path.insert(0, sys.argv[1])\n"
             "from crw_runtime import bridgerecord\n"
             "for follow in (True, False):\n"
             "    print(bridgerecord.read_json_without_blocking(sys.argv[2], 'x',"
             " follow=follow).state)\n"
             "print(bridgerecord.read(sys.argv[2])[1])\n",
             str(ROOT / "scripts"), str(pipe)],
            capture_output=True, text=True, timeout=30)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(done.stdout.split(), ["UNREADABLE"] * 3, done.stdout)

    @unittest.skipUnless(TOML_READER, "which crw package loads is read from the configuration")
    def test_a_cached_manifest_that_is_a_pipe_is_refused_without_blocking(self):
        launcher = self.install_package()
        manifest = launcher.parent.parent / ".codex-plugin" / "plugin.json"
        manifest.unlink()
        os.mkfifo(manifest)
        self.enable("crw@crw")
        finished = subprocess.run(
            [sys.executable, str(RUNTIME_INSTALL), "register-mcp", "--codex-home",
             str(self.home.codex_home), "--bridge-command", str(self.bridge), "--owner",
             "plugin", "--execution-policy", str(self.policy), "--apply"],
            capture_output=True, text=True, timeout=60)
        emitted = json.loads(finished.stdout)
        self.assertEqual(finished.returncode, 1, finished.stdout + finished.stderr)
        self.assertEqual(emitted["outcome"], "launcher_not_established")
        self.assertFalse(self.record.exists())

    def test_the_user_owner_is_refused_a_policy_it_would_never_read(self):
        status, emitted, output = run("register-mcp", "--codex-home", str(self.home.codex_home),
                                      "--bridge-command", str(self.bridge),
                                      "--execution-policy", str(self.policy), "--apply")
        self.assertEqual(status, 2, output)
        self.assertIn("--execution-policy", emitted["detail"], output)
        self.assertFalse(self.record.exists(), output)
        self.assertFalse((self.home.codex_home / "config.toml").exists(), output)
        with self.assertRaises(ValueError):
            bridgerecord.document(command=str(self.bridge), owner=bridgerecord.OWNER_USER,
                                  execution_policy={"path": str(self.policy),
                                                    "digest": self.digest})

    def test_a_relative_path_is_recorded_as_the_absolute_path_it_names(self):
        finished = subprocess.run(
            [sys.executable, str(RUNTIME_INSTALL), "register-mcp", "--codex-home",
             str(self.home.codex_home), "--bridge-command", str(self.bridge), "--owner",
             "plugin", "--execution-policy", self.policy.name, "--apply"],
            capture_output=True, text=True, cwd=str(self.policy.parent))
        self.assertEqual(finished.returncode, 0, finished.stdout + finished.stderr)
        self.assertEqual(json.loads(self.record.read_text())["executionPolicy"]["path"],
                         str(self.policy))

    @unittest.skipUnless(TOML_READER, "which crw package loads is read from the configuration")
    def test_an_installed_launcher_that_predates_the_record_refuses_the_write(self):
        """Written first, the record would stop every new thread's bridge until the update."""
        old = self.install_package(launcher_text=self.older_launcher())
        self.enable("crw@crw")
        for extra in ((), ("--apply",)):
            with self.subTest(extra or "plan"):
                status, emitted, output = self.register(*extra)
                self.assertNotEqual(status, 0, output)
                self.assertEqual(emitted["outcome"], "launcher_predates_policy", output)
                self.assertIn(str(old), emitted["detail"], output)
                self.assertFalse(self.record.exists(), output)
        # A record without a policy is what that launcher reads, and it is not held up.
        self.assertEqual(self.register("--apply", policy=False)[0], 0)

    @unittest.skipUnless(TOML_READER, "which crw package loads is read from the configuration")
    def test_a_launcher_that_only_declares_the_version_is_asked_what_it_does(self):
        """A constant proves nothing; each missing behaviour is found by running it."""
        variants = {
            "accepts only version 1": (
                "    if version not in (RECORD_VERSION, POLICY_RECORD_VERSION):\n",
                "    if version != RECORD_VERSION:\n", "did not start a bridge"),
            "drops the policy on exec": (
                "            os.execve(executable, [executable, *arguments], environment)\n",
                "            os.execv(executable, [executable, *arguments])\n",
                "without handing it the recorded policy"),
            "ignores the digest": (
                "    if actual != digest:\n", "    if False:\n",
                "digest no longer matches"),
        }
        for label, (old, new, reason) in variants.items():
            with self.subTest(label):
                shutil.rmtree(self.home.codex_home / "plugins", ignore_errors=True)
                launcher = self.install_package(
                    launcher_text=self.shipped_launcher_with(old, new))
                self.enable("crw@crw")
                status, emitted, output = self.register("--apply")
                self.assertNotEqual(status, 0, output)
                self.assertEqual(emitted["outcome"], "launcher_predates_policy", output)
                self.assertIn(str(launcher), emitted["detail"], output)
                self.assertIn(reason, emitted["detail"], output)
                self.assertFalse(self.record.exists(), output)

    @unittest.skipUnless(TOML_READER, "which crw package loads is read from the configuration")
    def test_the_enabled_crw_launcher_decides_and_unrelated_packages_do_not(self):
        """Another plugin may declare a server with the same name through an older launcher."""
        self.install_package()
        self.install_package(launcher_text=self.older_launcher(), marketplace="tools",
                             plugin="other")
        self.enable("crw@crw", "other@tools")
        status, emitted, output = self.register("--apply")
        self.assertEqual(status, 0, output)
        self.assertEqual(emitted["outcome"], bridgerecord.CREATED, output)

    @unittest.skipUnless(TOML_READER, "which crw package loads is read from the configuration")
    def test_a_disabled_or_unregistered_crw_package_starts_nothing_and_holds_nothing_up(self):
        self.install_package(launcher_text=self.older_launcher())
        for label, keys, enabled in (("disabled", ("crw@crw",), False), ("unregistered", (), True)):
            with self.subTest(label):
                self.enable(*keys, enabled=enabled)
                status, emitted, output = self.register()
                self.assertEqual(status, 0, output)
                self.assertEqual(emitted["outcome"], bridgerecord.WOULD_CREATE, output)

    @unittest.skipUnless(TOML_READER, "which crw package loads is read from the configuration")
    def test_a_crw_selection_that_is_ambiguous_or_unreadable_is_refused_by_name(self):
        cases = {
            "two cached versions": (lambda: (self.install_package(),
                                             self.install_package(version="0.5.0"),
                                             self.enable("crw@crw")), "more than one cached"),
            "two marketplaces": (lambda: (self.install_package(),
                                          self.enable("crw@crw", "crw@elsewhere")),
                                 "more than one marketplace"),
            "an unreadable configuration": (lambda: (self.install_package(),
                                                     (self.home.codex_home / "config.toml")
                                                     .write_text("[plugins\n", encoding="utf-8")),
                                            "could not be read"),
        }
        for label, (prepare, reason) in cases.items():
            with self.subTest(label):
                shutil.rmtree(self.home.codex_home / "plugins", ignore_errors=True)
                prepare()
                status, emitted, output = self.register("--apply")
                self.assertNotEqual(status, 0, output)
                self.assertFalse(self.record.exists(), output)
                if label == "an unreadable configuration":
                    # The ownership check reads the same file first and refuses on it too; either
                    # refusal names the reason, and neither writes.
                    self.assertIn("read", emitted["detail"], output)
                    continue
                self.assertEqual(emitted["outcome"], "launcher_not_established", output)
                self.assertIn(reason, emitted["detail"], output)

    @unittest.skipUnless(TOML_READER, "which crw package loads is read from the configuration")
    def test_the_launcher_this_package_ships_accepts_the_write(self):
        self.install_package()
        self.enable("crw@crw")
        status, emitted, output = self.register("--apply")
        self.assertEqual(status, 0, output)
        self.assertEqual(emitted["outcome"], bridgerecord.CREATED, output)

    @unittest.skipIf(TOML_READER, "the floor this repository supports has no configuration reader")
    def test_on_the_floor_an_installed_package_is_refused_rather_than_guessed_at(self):
        self.install_package()
        self.enable("crw@crw")
        status, emitted, output = self.register("--apply")
        self.assertNotEqual(status, 0, output)
        self.assertFalse(self.record.exists(), output)

    def test_the_policy_is_part_of_what_the_record_starts(self):
        base = bridgerecord.document(command="/opt/x/bin/codex-thread-bridge",
                                     name="codex-thread-bridge",
                                     execution_policy={"path": "/p", "digest": "a" * 64})
        for changed in (
                bridgerecord.document(command="/opt/x/bin/codex-thread-bridge",
                                      name="codex-thread-bridge"),
                bridgerecord.document(command="/opt/x/bin/codex-thread-bridge",
                                      name="codex-thread-bridge",
                                      execution_policy={"path": "/q", "digest": "a" * 64}),
                bridgerecord.document(command="/opt/x/bin/codex-thread-bridge",
                                      name="codex-thread-bridge",
                                      execution_policy={"path": "/p", "digest": "b" * 64})):
            self.assertFalse(bridgerecord.same_registration(base, changed), changed)

    def test_the_reader_refuses_every_shape_the_writer_never_produces(self):
        good = bridgerecord.document(command="/opt/x/bin/codex-thread-bridge",
                                     name="codex-thread-bridge",
                                     execution_policy={"path": "/p", "digest": "a" * 64})
        self.assertEqual(bridgerecord.complaints(good), [])
        wrong = {
            "version 1 naming a policy": dict(good, recordVersion=bridgerecord.RECORD_VERSION),
            "version 2 naming none": {k: v for k, v in good.items() if k != "executionPolicy"},
            "a user-owned policy record": dict(good, owner=bridgerecord.OWNER_USER),
            "a padded path": dict(good, executionPolicy={"path": "/p ", "digest": "a" * 64}),
            "a relative path": dict(good, executionPolicy={"path": "p", "digest": "a" * 64}),
            "a bad digest": dict(good, executionPolicy={"path": "/p", "digest": "A" * 64}),
            "an extra key": dict(good, executionPolicy={"path": "/p", "digest": "a" * 64,
                                                        "roles": {}}),
        }
        for label, document in wrong.items():
            with self.subTest(label):
                self.assertNotEqual(bridgerecord.complaints(document), [])


# ---------------------------------------------------------------- the packaged launchers


class StopLauncherTest(unittest.TestCase):
    """The launcher may never cost a turn, and may never be the second hook on one Stop."""

    LAUNCHER = ROOT / "plugins/crw/wiring/crw_stop_hook.py"
    PAYLOAD = json.dumps({"hook_event_name": "Stop", "session_id": "s", "turn_id": "t"})

    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.home = Path(self.stack.enter_context(tempfile.TemporaryDirectory(prefix="crw114-")))
        self.adapter = self.home / "adapter.py"
        self.seen = self.home / "seen.txt"
        self.adapter.write_text(
            "import sys\n"
            "open(%r, 'a').write(sys.stdin.read() + '\\n')\n"
            "sys.stdout.write('{\"decision\": \"block\"}')\n" % str(self.seen),
            encoding="utf-8")

    def settings(self, **overrides):
        document = {"configVersion": 1, "event": "Stop", "owner": "plugin",
                    "relayExecutable": "/opt/relay/bin/codex-session-relay",
                    "markerRoot": "/opt/marker", "mode": "observe", "timeoutSeconds": 5,
                    "adapterInterpreter": sys.executable,
                    "adapterEntryPoint": str(self.adapter)}
        document.update(overrides)
        (self.home / "crw-completion-hook.json").write_text(json.dumps(document),
                                                            encoding="utf-8")

    def fire(self):
        return subprocess.run([sys.executable, str(self.LAUNCHER)], input=self.PAYLOAD,
                              capture_output=True, text=True,
                              env={"PATH": os.environ["PATH"], "CODEX_HOME": str(self.home)})

    def test_it_runs_the_adapter_and_forwards_only_what_it_printed(self):
        self.settings()
        done = self.fire()
        self.assertEqual(done.returncode, 0)
        self.assertEqual(done.stdout, '{"decision": "block"}')
        self.assertEqual(done.stderr, "")
        self.assertIn("turn_id", self.seen.read_text(encoding="utf-8"))

    def test_it_stands_down_when_the_user_owns_the_registration(self):
        """Installing the package on a host that already registered the hook cannot be refused
        from here, so the second copy is prevented at run time as well."""
        self.settings(owner="user")
        done = self.fire()
        self.assertEqual((done.returncode, done.stdout, done.stderr), (0, "", ""))
        self.assertFalse(self.seen.exists())

    def test_it_stands_down_when_the_owner_key_is_absent(self):
        self.settings()
        document = json.loads((self.home / "crw-completion-hook.json").read_text())
        del document["owner"]
        (self.home / "crw-completion-hook.json").write_text(json.dumps(document))
        done = self.fire()
        self.assertEqual((done.returncode, done.stdout, done.stderr), (0, "", ""))
        self.assertFalse(self.seen.exists())

    def test_absent_unreadable_and_nonsense_settings_all_release_in_silence(self):
        for content in (None, "{ not json", json.dumps(["a list"]),
                        json.dumps({"owner": "plugin"})):
            if content is None:
                (self.home / "crw-completion-hook.json").unlink(missing_ok=True)
            else:
                (self.home / "crw-completion-hook.json").write_text(content, encoding="utf-8")
            done = self.fire()
            self.assertEqual((done.returncode, done.stdout, done.stderr), (0, "", ""),
                             repr(content))

    def test_a_relative_adapter_path_is_not_run(self):
        self.settings(adapterEntryPoint="adapter.py")
        done = self.fire()
        self.assertEqual((done.returncode, done.stdout, done.stderr), (0, "", ""))
        self.assertFalse(self.seen.exists())

    def test_an_inherited_settings_override_does_not_redirect_the_launcher(self):
        """Installation can only refuse the environment it sees; a session starts under
        another one. Reading a single path is what makes the location an invariant."""
        self.settings()
        for override in ("elsewhere/settings.json", str(self.home / "elsewhere.json"),
                         "~/crw-completion-hook.json"):
            done = subprocess.run(
                [sys.executable, str(self.LAUNCHER)], input=self.PAYLOAD, capture_output=True,
                text=True, cwd="/",
                env={"PATH": os.environ["PATH"], "HOME": str(self.home),
                     "CODEX_HOME": str(self.home),
                     "CRW_COMPLETION_HOOK_CONFIG": override})
            self.assertEqual(done.returncode, 0, override)
            self.assertEqual(done.stdout, '{"decision": "block"}',
                             override + " redirected the launcher: " + done.stderr)

    def test_an_adapter_that_crashes_still_costs_nothing(self):
        self.adapter.write_text("raise SystemExit(2)\n", encoding="utf-8")
        self.settings()
        done = self.fire()
        self.assertEqual((done.returncode, done.stdout, done.stderr), (0, "", ""))

    def test_the_launcher_ceiling_and_the_installer_agree(self):
        """The launcher cannot import the module that refuses budgets reaching its ceiling."""
        source = self.LAUNCHER.read_text(encoding="utf-8")
        self.assertIn("MAX_SECONDS = " + str(completion.LAUNCHER_CEILING_SECONDS), source)

    def test_a_guard_budget_reaching_that_ceiling_is_not_installable_for_the_plugin(self):
        document = completion.configuration(
            relay="/opt/relay/bin/codex-session-relay", codex_home="/tmp/codex",
            owner=completion.OWNER_PLUGIN, adapter_interpreter=sys.executable,
            adapter_entry_point=str(self.adapter),
            timeout=completion.LAUNCHER_CEILING_SECONDS)
        self.assertTrue(any("packaged launcher" in complaint
                            for complaint in completion.complaints(document)))

    def test_the_shipped_default_budget_leaves_the_launcher_margin(self):
        self.assertLess(completion.DEFAULT_TIMEOUT_SECONDS,
                        completion.LAUNCHER_CEILING_SECONDS)


class BridgeLauncherTest(unittest.TestCase):
    """The opposite failure direction: a server that cannot start says why."""

    LAUNCHER = ROOT / "plugins/crw/wiring/crw_bridge_mcp.py"

    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.home = Path(self.stack.enter_context(tempfile.TemporaryDirectory(prefix="crw114-")))

    def start(self):
        return subprocess.run([sys.executable, str(self.LAUNCHER)], capture_output=True,
                              text=True, input="",
                              env={"PATH": os.environ["PATH"], "CODEX_HOME": str(self.home)})

    def record(self, **overrides):
        document = {"recordVersion": 1, "owner": "plugin", "serverName": "codex-thread-bridge",
                    "bridgeExecutable": "/does/not/exist", "args": []}
        document.update(overrides)
        (self.home / bridgerecord.RECORD_NAME).write_text(json.dumps(document), encoding="utf-8")

    def test_an_absent_record_names_the_command_that_writes_it(self):
        done = self.start()
        self.assertEqual(done.returncode, 2)
        # Both owners, because the right command depends on who owns the server and naming
        # only one sends half of the hosts at the wrong repair.
        self.assertIn("register-mcp --apply", done.stderr)
        self.assertIn("--owner plugin", done.stderr)

    def test_the_user_owner_records_itself_so_the_launcher_can_stand_down(self):
        """A host that registered the bridge here and later installs the package."""
        stack = contextlib.ExitStack()
        self.addCleanup(stack.close)
        home = Home(stack)
        bridge = home.destination / "current" / "bin" / "codex-thread-bridge"
        bridge.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        bridge.chmod(0o755)
        status, emitted, output = run("register-mcp", "--codex-home", str(home.codex_home),
                                      "--bridge-command", str(bridge), "--apply")
        record = home.codex_home / bridgerecord.RECORD_NAME
        if not TOML_READER:
            # A user registration reads the configuration back, and that reader arrived in
            # 3.11. On the floor this repository supports the command refuses and writes
            # nothing, which is the behaviour to assert here rather than a record it never made.
            self.assertNotEqual(status, 0, output)
            self.assertFalse(record.exists(), output)
            return
        self.assertEqual(status, 0, output)
        self.assertTrue(record.exists(), output)
        self.assertEqual(json.loads(record.read_text())["owner"], bridgerecord.OWNER_USER)
        done = subprocess.run([sys.executable, str(self.LAUNCHER)], capture_output=True,
                              text=True, input="",
                              env={"PATH": os.environ["PATH"],
                                   "CODEX_HOME": str(home.codex_home)})
        self.assertEqual(done.returncode, 2)
        self.assertIn("registers it", done.stderr)

    def test_a_user_owned_record_refuses_to_start_a_second_bridge(self):
        self.record(owner="user")
        done = self.start()
        self.assertEqual(done.returncode, 2)
        self.assertIn("second one", done.stderr)

    def test_a_relative_executable_is_refused(self):
        self.record(bridgeExecutable="./codex-thread-bridge")
        done = self.start()
        self.assertEqual(done.returncode, 2)
        self.assertIn("absolute path", done.stderr)

    def test_a_missing_runtime_points_at_the_installer(self):
        self.record()
        done = self.start()
        self.assertEqual(done.returncode, 2)
        self.assertIn("pointer names the runtime", done.stderr)

    def test_it_starts_the_executable_the_record_names(self):
        proof = self.home / "started"
        executable = self.home / "bridge"
        executable.write_text("#!/bin/sh\necho started > %s\n" % proof, encoding="utf-8")
        executable.chmod(0o755)
        self.record(bridgeExecutable=str(executable), args=["--stdio"])
        done = self.start()
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertTrue(proof.exists(), done.stderr)

    def test_a_record_version_this_launcher_does_not_read_is_refused(self):
        self.record(recordVersion=bridgerecord.POLICY_RECORD_VERSION + 1)
        done = self.start()
        self.assertEqual(done.returncode, 2)
        self.assertIn("version", done.stderr)

    def test_falsy_arguments_that_are_not_a_list_are_refused(self):
        """or [] would turn each of these into no arguments and start the runtime anyway."""
        for arguments in (False, 0, "", {}, [1], "--stdio"):
            self.record(args=arguments)
            done = self.start()
            self.assertEqual(done.returncode, 2, repr(arguments))
            self.assertIn("args", done.stderr, repr(arguments))

    def test_a_record_naming_another_server_is_refused(self):
        self.record(serverName="something-else")
        done = self.start()
        self.assertEqual(done.returncode, 2)
        self.assertIn("something-else", done.stderr)

    def test_the_launcher_and_the_declaration_agree_on_the_server_name(self):
        declared = json.loads((ROOT / "plugins/crw/wiring/mcp.json").read_text(encoding="utf-8"))
        self.assertEqual(sorted(declared["mcpServers"]), ["codex-thread-bridge"])
        self.assertIn('DECLARED_SERVER = "codex-thread-bridge"',
                      self.LAUNCHER.read_text(encoding="utf-8"))

    def test_the_launcher_and_the_writer_agree_on_the_record_version(self):
        """Compared as values, with the writer and with the bridge that reads the variables."""
        launcher = load_launcher()
        self.assertEqual((launcher.RECORD_VERSION, launcher.POLICY_RECORD_VERSION),
                         bridgerecord.RECORD_VERSIONS)
        self.assertEqual((launcher.POLICY_FIELD, tuple(launcher.POLICY_KEYS)),
                         (bridgerecord.POLICY_FIELD, tuple(bridgerecord.POLICY_KEYS)))
        self.assertEqual((launcher.POLICY_VARIABLE, launcher.DIGEST_VARIABLE),
                         (execution.ENVIRONMENT_VARIABLE, execution.DIGEST_VARIABLE))

    def test_it_starts_the_executable_the_record_names_unchanged(self):
        proof = self.home / "started"
        executable = self.home / "bridge"
        executable.write_text("#!/bin/sh\necho started > %s\n" % proof, encoding="utf-8")
        executable.chmod(0o755)
        self.record(bridgeExecutable=str(executable), args=["--stdio"])
        done = self.start()
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertTrue(proof.exists(), done.stderr)


class BridgeLauncherPolicyTest(unittest.TestCase):
    """A record naming the host's policy: the bridge gets it, or does not start.

    Codex hands the launcher the App Server's bare environment, so these start it with nothing but
    PATH and CODEX_HOME. The bridge it execs is a probe that asks the bridge's own policy module
    what it would enforce, through from_environment -- the call the real server makes in main() --
    and writes the answer down. A probe that never wrote anything was never started.
    """

    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.home = Path(self.stack.enter_context(tempfile.TemporaryDirectory(prefix="crw218-")))
        self.proof = self.home / "proof.json"
        self.probe = self.home / "probe.py"
        self.probe.write_text(
            "import json, os, sys\n"
            "sys.path.insert(0, %r)\n"
            "from codex_thread_bridge import execution\n"
            "try:\n"
            "    answer = {'summary': execution.ExecutionPolicy.from_environment(os.environ)"
            ".summary()}\n"
            "except execution.ExecutionPolicyError as error:\n"
            "    answer = {'refused': str(error)}\n"
            "answer['variables'] = {name: os.environ.get(name) for name in"
            " (execution.ENVIRONMENT_VARIABLE, execution.DIGEST_VARIABLE)}\n"
            "open(sys.argv[1], 'w').write(json.dumps(answer))\n" % str(BRIDGE_SOURCE),
            encoding="utf-8")
        self.policy = self.home / "execution-policy.json"
        self.digest = write_policy(self.policy)

    def reference(self, **overrides):
        reference = {"path": str(self.policy), "digest": self.digest}
        reference.update(overrides)
        return reference

    def record(self, **overrides):
        document = {"recordVersion": bridgerecord.POLICY_RECORD_VERSION, "owner": "plugin",
                    "serverName": "codex-thread-bridge", "bridgeExecutable": sys.executable,
                    "args": [str(self.probe), str(self.proof)],
                    "executionPolicy": self.reference()}
        document.update(overrides)
        for key in [name for name, value in document.items() if value is None]:
            del document[key]
        (self.home / bridgerecord.RECORD_NAME).write_text(json.dumps(document), encoding="utf-8")

    def start(self, **environment):
        return subprocess.run([sys.executable, str(BRIDGE_LAUNCHER)], capture_output=True,
                              text=True, input="",
                              env={"PATH": os.environ["PATH"], "CODEX_HOME": str(self.home),
                                   **environment})

    def answer(self):
        return json.loads(self.proof.read_text(encoding="utf-8")) if self.proof.exists() else None

    def assert_refused(self, done, *needles):
        self.assertEqual(done.returncode, 2, done.stderr)
        self.assertIsNone(self.answer(), "a refused start must not reach the bridge")
        for needle in needles:
            self.assertIn(needle, done.stderr)

    def test_the_bridge_is_started_under_the_policy_the_record_names(self):
        self.record()
        done = self.start()
        self.assertEqual(done.returncode, 0, done.stderr)
        answer = self.answer()
        self.assertEqual(answer["variables"], {execution.ENVIRONMENT_VARIABLE: str(self.policy),
                                               execution.DIGEST_VARIABLE: self.digest})
        self.assertEqual(answer["summary"]["digest"], self.digest)
        self.assertEqual(answer["summary"]["roles"],
                         execution.ExecutionPolicy.from_mapping(POLICY).summary()["roles"])
        # The launcher's digest and the bridge's are the same function of the same bytes.
        self.assertEqual(self.digest, execution.ExecutionPolicy.from_file(self.policy)
                         .summary()["digest"])

    def test_a_version_1_record_starts_with_no_policy_exactly_as_before(self):
        self.record(recordVersion=bridgerecord.RECORD_VERSION, executionPolicy=None)
        done = self.start()
        self.assertEqual(done.returncode, 0, done.stderr)
        answer = self.answer()
        self.assertEqual(answer["summary"], {"mode": "presence_only", "digest": None, "roles": {}})
        self.assertEqual(answer["variables"], {execution.ENVIRONMENT_VARIABLE: None,
                                               execution.DIGEST_VARIABLE: None})

    def test_a_version_1_record_passes_its_environment_through_untouched(self):
        """Exactly as before means a variable the host did set still reaches the bridge."""
        self.record(recordVersion=bridgerecord.RECORD_VERSION, executionPolicy=None)
        done = self.start(**{execution.ENVIRONMENT_VARIABLE: str(self.policy)})
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(self.answer()["summary"]["digest"], self.digest)

    def test_a_version_1_record_that_names_a_policy_is_refused(self):
        """Started as version 1 it would run the bridge without the policy it names."""
        self.record(recordVersion=bridgerecord.RECORD_VERSION)
        self.assert_refused(self.start(), "version 2")

    def test_a_policy_record_without_a_usable_reference_is_refused(self):
        cases = {
            "absent": None,
            "not an object": "/etc/policy.json",
            "no digest": {"path": str(self.policy)},
            "an extra key": dict(self.reference(), note="x"),
            "a relative path": self.reference(path="execution-policy.json"),
            "a newline": self.reference(path=str(self.policy) + "\n"),
            "an empty path": self.reference(path=""),
            "a short digest": self.reference(digest=self.digest[:32]),
            "an uppercase digest": self.reference(digest=self.digest.upper()),
        }
        for label, reference in cases.items():
            with self.subTest(label):
                self.record(executionPolicy=reference)
                self.assert_refused(self.start())

    def test_a_padded_path_is_refused_even_when_a_file_by_that_name_exists(self):
        """The bridge strips the variable, so it would open the unpadded file instead.

        The padded file exists and matches its digest, so only the padding check stands between
        this record and a bridge started under a file other than the one the record names.
        """
        padded = Path(str(self.policy) + " ")
        digest = write_policy(padded, OTHER_POLICY)
        self.record(executionPolicy=self.reference(path=str(padded), digest=digest))
        self.assert_refused(self.start(), "whitespace")

    def test_a_policy_the_launcher_cannot_read_is_refused_naming_the_record(self):
        absent = self.home / "absent.json"
        directory = self.home / "a-directory"
        directory.mkdir()
        cases = [("absent", absent), ("a directory", directory)]
        if os.geteuid() != 0:
            # root reads through a mode of 000, so the case exists only for everyone else.
            locked = self.home / "locked.json"
            write_policy(locked)
            locked.chmod(0)
            self.addCleanup(locked.chmod, 0o600)
            cases.append(("unreadable", locked))
        for label, path in cases:
            with self.subTest(label):
                self.record(executionPolicy=self.reference(path=str(path)))
                self.assert_refused(self.start(), str(self.home / bridgerecord.RECORD_NAME),
                                    "register-mcp")

    def test_a_pipe_named_as_the_policy_is_refused_without_blocking(self):
        """Opening a FIFO for reading blocks until a writer arrives; the start must not wait."""
        pipe = self.home / "policy.fifo"
        os.mkfifo(pipe)
        self.record(executionPolicy=self.reference(path=str(pipe)))
        done = subprocess.run([sys.executable, str(BRIDGE_LAUNCHER)], capture_output=True,
                              text=True, input="", timeout=30,
                              env={"PATH": os.environ["PATH"], "CODEX_HOME": str(self.home)})
        self.assert_refused(done, "not a regular file")

    def test_a_record_that_is_a_pipe_is_refused_without_blocking(self):
        (self.home / bridgerecord.RECORD_NAME).unlink(missing_ok=True)
        os.mkfifo(self.home / bridgerecord.RECORD_NAME)
        done = subprocess.run([sys.executable, str(BRIDGE_LAUNCHER)], capture_output=True,
                              text=True, input="", timeout=30,
                              env={"PATH": os.environ["PATH"], "CODEX_HOME": str(self.home)})
        self.assert_refused(done, "not a regular file")

    def test_a_policy_changed_after_it_was_registered_is_refused(self):
        """Another VALID policy, so this fails on the digest and not on a parse."""
        self.record()
        replaced = write_policy(self.policy, OTHER_POLICY)
        self.assertNotEqual(replaced, self.digest)
        self.assert_refused(self.start(), self.digest, replaced)

    def test_an_inherited_variable_naming_another_file_is_refused(self):
        other = self.home / "other-policy.json"
        write_policy(other, OTHER_POLICY)
        self.record()
        self.assert_refused(self.start(**{execution.ENVIRONMENT_VARIABLE: str(other)}),
                            str(other), "Unset the variable")

    def test_an_inherited_digest_that_disagrees_is_refused(self):
        self.record()
        self.assert_refused(self.start(**{execution.DIGEST_VARIABLE: "0" * 64}), "0" * 64)

    def test_an_inherited_variable_naming_the_same_file_is_not_a_conflict(self):
        self.record()
        spelled = str(self.home / "." / self.policy.name)
        done = self.start(**{execution.ENVIRONMENT_VARIABLE: spelled,
                             execution.DIGEST_VARIABLE: self.digest})
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(self.answer()["variables"][execution.ENVIRONMENT_VARIABLE],
                         str(self.policy))

    def test_an_empty_inherited_variable_is_unset_as_the_bridge_reads_it(self):
        self.record()
        done = self.start(**{execution.ENVIRONMENT_VARIABLE: "  ",
                             execution.DIGEST_VARIABLE: ""})
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(self.answer()["summary"]["digest"], self.digest)

    def test_a_user_owned_policy_record_still_stands_down(self):
        self.record(owner="user")
        self.assert_refused(self.start(), "second one")


# ---------------------------------------------------------------- the package check


class DeclaredComponentTest(unittest.TestCase):
    """The rules the measured loading behaviour turned into package requirements."""

    def payload(self, **files):
        return {name: ("100644", data.encode()) for name, data in files.items()}

    def test_an_inline_hooks_document_is_refused(self):
        with self.assertRaises(ValueError):
            plugin.declared_hooks({"hooks": {"Stop": []}})

    def test_a_list_and_a_string_are_both_accepted(self):
        self.assertEqual(plugin.declared_hooks({"hooks": "./a.json"}), ["./a.json"])
        self.assertEqual(plugin.declared_hooks({"hooks": ["./a.json"]}), ["./a.json"])

    def test_declared_roots_come_from_the_manifest(self):
        _, roots, errors = plugin.declared_components(
            {"skills": "./skills/", "hooks": ["./wiring/hooks/stop.json"],
             "mcpServers": "./wiring/mcp.json"})
        self.assertEqual(errors, [])
        self.assertEqual(roots, {".codex-plugin", "LICENSE", "skills", "wiring"})

    def test_an_inline_mcp_table_and_apps_are_refused(self):
        _, _, errors = plugin.declared_components(
            {"skills": "./skills/", "mcpServers": {"x": {}}, "apps": "./apps"})
        self.assertEqual(len(errors), 2, errors)

    def test_a_hook_timeout_over_the_host_clamp_is_refused(self):
        document = json.dumps({"hooks": {"Stop": [{"hooks": [
            {"type": "command", "command": "x", "timeout": 600}]}]}})
        errors = plugin.hook_document_errors("h.json", document.encode(), "release")
        self.assertTrue(any("may not exceed" in problem for problem in errors), errors)

    def test_a_hook_that_is_not_a_command_is_refused(self):
        document = json.dumps({"hooks": {"Stop": [{"hooks": [{"type": "mcp"}]}]}})
        self.assertTrue(plugin.hook_document_errors("h.json", document.encode(), "release"))

    def test_an_mcp_server_without_cwd_is_refused(self):
        document = json.dumps({"mcpServers": {"b": {"command": "python3",
                                                    "args": ["./w/run.py"]}}})
        errors = plugin.mcp_document_errors("m.json", document.encode(),
                                            self.payload(**{"w/run.py": "x"}), "release")
        self.assertTrue(any("cwd" in problem for problem in errors), errors)

    def test_a_variable_in_an_mcp_argument_is_refused(self):
        document = json.dumps({"mcpServers": {"b": {
            "command": "python3", "cwd": ".", "args": ["${PLUGIN_ROOT}/w/run.py"]}}})
        errors = plugin.mcp_document_errors("m.json", document.encode(), self.payload(),
                                            "release")
        self.assertTrue(any("literal text" in problem for problem in errors), errors)

    def test_an_absolute_mcp_argument_is_refused(self):
        document = json.dumps({"mcpServers": {"b": {
            "command": "python3", "cwd": ".", "args": ["/opt/run.py"]}}})
        errors = plugin.mcp_document_errors("m.json", document.encode(), self.payload(),
                                            "release")
        self.assertTrue(any("absolute path" in problem for problem in errors), errors)

    def test_an_mcp_argument_the_package_does_not_ship_is_refused(self):
        document = json.dumps({"mcpServers": {"b": {
            "command": "python3", "cwd": ".", "args": ["./w/missing.py"]}}})
        errors = plugin.mcp_document_errors("m.json", document.encode(), self.payload(),
                                            "release")
        self.assertTrue(any("does not ship" in problem for problem in errors), errors)

    def test_a_command_that_is_not_a_string_is_reported_not_raised(self):
        """A finding that ends the run in a traceback hides itself and everything after it."""
        for command in (1, True, {"a": 1}, None):
            document = json.dumps({"mcpServers": {"b": {
                "command": command, "cwd": ".", "args": ["./w/run.py"]}}})
            errors = plugin.mcp_document_errors("m.json", document.encode(),
                                                self.payload(**{"w/run.py": "x"}), "release")
            self.assertTrue(any("needs a command" in problem for problem in errors),
                            (command, errors))

    def test_the_shipped_package_passes_its_own_rules(self):
        """A positive control, so the rules above are not merely rejecting everything."""
        errors, result = plugin.check_revision("HEAD")
        self.assertEqual(errors, [])
        self.assertEqual(result["expectedSkillNames"],
                         ["crw:crw-check", "crw:crw-define", "crw:crw-logic", "crw:crw-loop",
                          "crw:crw-next", "crw:crw-plan", "crw:crw-refactor", "crw:crw-run",
                          "crw:crw-status", "crw:crw-tidy"])


if __name__ == "__main__":
    unittest.main()


class TheDeclaredApprovalPolicyIsChecked(unittest.TestCase):
    """CRW-142. The only signal a bad approval declaration ever produces.

    Measured on an isolated home: an invalid approval_mode in a PLUGIN declaration makes
    codex plugin add exit 0 and codex mcp list exit 0 with ZERO entries. The server disappears and
    no disabled_reason is recorded, because there is no entry left to carry one. The same mistake
    in a user configuration is a loud error naming the valid set. So the host will not tell anyone
    that the bridge is gone, and this check is what does.
    """

    def payload(self, **files):
        return {name: ("100644", data.encode()) for name, data in files.items()}

    def declaration(self, tools, server="codex-thread-bridge"):
        entry = {"command": "python3", "cwd": ".", "args": ["./w/run.py"]}
        if tools is not None:
            entry["tools"] = tools
        return json.dumps({"mcpServers": {server: entry}}).encode()

    def errors(self, tools, server="codex-thread-bridge"):
        return plugin.mcp_document_errors("m.json", self.declaration(tools, server),
                                          self.payload(**{"w/run.py": "x"}), "release")

    def test_the_measured_set_is_what_is_accepted(self):
        for mode in plugin.APPROVAL_MODES:
            required = dict(plugin.REQUIRED_TOOL_APPROVALS["codex-thread-bridge"])
            gates = {tool: {"approval_mode": value} for tool, value in required.items()}
            gates["other_tool"] = {"approval_mode": mode}
            self.assertEqual(self.errors(gates), [], mode)

    def test_a_mode_outside_the_measured_set_is_refused(self):
        gates = {tool: {"approval_mode": value}
                 for tool, value in plugin.REQUIRED_TOOL_APPROVALS["codex-thread-bridge"].items()}
        gates["create_thread"] = {"approval_mode": "always"}
        found = self.errors(gates)
        self.assertTrue(any("not one of" in problem for problem in found), found)

    def test_a_key_this_check_does_not_know_is_refused(self):
        gates = {tool: {"approval_mode": value}
                 for tool, value in plugin.REQUIRED_TOOL_APPROVALS["codex-thread-bridge"].items()}
        gates["create_thread"] = {"approval_mode": "approve", "enabled": True}
        found = self.errors(gates)
        self.assertTrue(any("disappears without a word" in problem for problem in found), found)

    def test_a_tools_value_that_is_not_an_object_is_refused(self):
        self.assertTrue(self.errors(["create_thread"]))
        self.assertTrue(self.errors({}))
        self.assertTrue(self.errors({"create_thread": "approve"}))

    def test_dropping_a_required_gate_is_refused(self):
        found = self.errors({"create_thread": {"approval_mode": "approve"}})
        self.assertTrue(any("send_message_to_thread" in problem for problem in found), found)

    def test_weakening_a_required_gate_is_refused(self):
        gates = {tool: {"approval_mode": value}
                 for tool, value in plugin.REQUIRED_TOOL_APPROVALS["codex-thread-bridge"].items()}
        gates["create_thread"] = {"approval_mode": "auto"}
        found = self.errors(gates)
        self.assertTrue(any("must gate create_thread" in problem for problem in found), found)

    def test_a_server_with_no_required_gate_may_declare_none(self):
        self.assertEqual(self.errors(None, server="some-other-server"), [])

    def test_the_shipped_declaration_carries_the_gate(self):
        document = json.loads(
            (ROOT / "plugins" / "crw" / "wiring" / "mcp.json").read_text(encoding="utf-8"))
        gates = document["mcpServers"]["codex-thread-bridge"]["tools"]
        self.assertEqual({tool: gate["approval_mode"] for tool, gate in gates.items()},
                         plugin.REQUIRED_TOOL_APPROVALS["codex-thread-bridge"])

    def test_this_repository_never_invokes_the_plugin_mutation_itself(self):
        """Which is why check-declaration is a gate an operator runs, not an interception."""
        # Read as syntax, not as text. Grepping for the words matched the sentences these
        # commands print about what they deliberately do NOT do -- "the plugin cache and its
        # config.toml entry, which codex plugin remove owns" -- and a check that cannot tell a
        # sentence from an invocation proves nothing about either.
        import ast
        offenders = []
        for path in sorted((ROOT / "scripts").rglob("*.py")):
            if "tests" in path.parts:
                continue
            tree = ast.parse(path.read_text(encoding="utf-8", errors="replace"), str(path))
            for node in ast.walk(tree):
                if not isinstance(node, (ast.List, ast.Tuple)):
                    continue
                words = [item.value for item in node.elts
                         if isinstance(item, ast.Constant) and isinstance(item.value, str)]
                if "codex" in words and "plugin" in words:
                    offenders.append(str(path) + ": " + repr(words))
        self.assertEqual(offenders, [])


@unittest.skipUnless(TOML_READER, "register-mcp cannot read a configuration without tomllib, so"
                                  " on the 3.10 floor it refuses every registration for that"
                                  " reason and none of these cases would be about the guard")
class RegisterMcpDoesNotShadowADeclaredServer(unittest.TestCase):
    """CRW-142, the register-mcp half. Escalated to the operations lane and authorised by it.

    _mcp_ownership already refuses a user registration when the ownership record names the plugin,
    and _other_bridge_tables catches the same bridge under a different table name. The path left
    open is exactly: the plugin declares the server, no plugin-owned record blocks the write, and
    the run registers that same table name. The record check passes on absent, the other-names
    check excludes the name being written, and codexconfig.register appends a user table.

    It is reachable in one ordinary sequence. transition --apply writes a plugin-owned record;
    remove --apply retires it and deliberately leaves the plugin cache and its config entry alone,
    because codex plugin remove owns those. A register-mcp run after that finds no record and a
    plugin that still declares the server. Measured: a user table wins over the declaration, so the
    appended table shadows it, and with no approval fields it serves the bridge ungated.
    """

    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.home = Home(self.stack)

    def install_plugin(self, declares=True):
        cache = (self.home.codex_home / "plugins" / "cache" / "crw" / "crw" / "0.2.0")
        (cache / ".codex-plugin").mkdir(parents=True)
        (cache / ".codex-plugin" / "plugin.json").write_text(
            (ROOT / "plugins" / "crw" / ".codex-plugin" / "plugin.json").read_text(
                encoding="utf-8"), encoding="utf-8")
        (cache / "wiring").mkdir(parents=True)
        document = json.loads(
            (ROOT / "plugins" / "crw" / "wiring" / "mcp.json").read_text(encoding="utf-8"))
        if not declares:
            document["mcpServers"] = {"something-else": document["mcpServers"]["codex-thread-bridge"]}
        (cache / "wiring" / "mcp.json").write_text(json.dumps(document), encoding="utf-8")
        config = self.home.codex_home / "config.toml"
        existing = config.read_text(encoding="utf-8") if config.is_file() else ""
        config.write_text(existing + '\n[plugins."crw@crw"]\nenabled = true\n', encoding="utf-8")
        return cache

    def register(self, *extra):
        return run("register-mcp", "--owner", "user", "--codex-home", str(self.home.codex_home),
                   "--bridge-command", str(self.home.destination / "current" / "bin"
                                           / "codex-thread-bridge"), *extra)

    def config(self):
        path = self.home.codex_home / "config.toml"
        return path.read_text(encoding="utf-8") if path.is_file() else ""

    def test_the_shadowing_write_is_refused_and_nothing_is_written(self):
        self.install_plugin()
        before = self.config()
        status, emitted, output = self.register("--apply")
        self.assertEqual(status, 1, output)
        self.assertIn("declares", emitted["detail"])
        self.assertIn("codex-thread-bridge", emitted["detail"])
        self.assertIs(emitted["applied"], False)
        self.assertIs(emitted["wrote"], False)
        self.assertIs(emitted["otherTablesPreserved"], True)
        self.assertEqual(self.config(), before)
        self.assertNotIn("[mcp_servers.codex-thread-bridge]", self.config())

    def test_a_dry_run_refuses_the_same_way(self):
        self.install_plugin()
        status, emitted, output = self.register()
        self.assertEqual(status, 1, output)
        self.assertIs(emitted["wrote"], False)

    def test_without_the_plugin_the_ordinary_registration_still_works(self):
        """The manual install is the whole reason this command exists; it must be untouched."""
        before = self.config()
        status, emitted, output = self.register("--apply")
        self.assertEqual(status, 0, output)
        self.assertIn("[mcp_servers.codex-thread-bridge]", self.config())
        self.assertNotEqual(self.config(), before)

    def test_a_plugin_that_declares_another_server_does_not_block_this_one(self):
        self.install_plugin(declares=False)
        status, emitted, output = self.register("--apply")
        self.assertEqual(status, 0, output)
        self.assertIn("[mcp_servers.codex-thread-bridge]", self.config())

    def test_a_different_table_name_is_a_separate_gap_and_is_pinned_not_guarded(self):
        """Measured, and deliberately NOT closed by this guard. Reported to the operations lane.

        The authorised guard is about SHADOWING: a user table under the name the package declares,
        which wins over the declaration. Registering the same bridge under another name is a
        different failure -- two bridge servers side by side rather than one hidden behind the
        other -- and _other_bridge_tables only compares against tables already in the
        configuration, never against what a package declares. Widening this guard to cover it
        would be a second contract, so it is measured and handed back instead of absorbed.
        """
        self.install_plugin()
        status, emitted, output = self.register("--apply", "--name", "my-bridge")
        self.assertEqual(status, 0, output)
        self.assertEqual(emitted["serversNow"], ["my-bridge"])

    @unittest.skipUnless(TOML_READER, "register-mcp needs a configuration reader")
    def test_a_cached_manifest_that_is_not_an_object_refuses_rather_than_crashing(self):
        """Devin finding: valid JSON is not a manifest, and .get on a list is not a refusal."""
        cache = self.install_plugin()
        (cache / ".codex-plugin" / "plugin.json").write_text("[]", encoding="utf-8")
        before = self.config()
        status, emitted, output = self.register("--apply")
        self.assertEqual(status, 1, output)
        self.assertNotEqual(emitted.get("outcome"), "internal_error")
        self.assertIn("could not be read", emitted["detail"])
        self.assertEqual(self.config(), before)


# ------------------------------------------------- the declaration that outlives the cache


class DeclaredStopCommandTest(unittest.TestCase):
    """CRW-178. The command a turn holds must survive its version cache being replaced.

    A plugin hook command is fixed when the turn starts, with the plugin root already resolved
    into it. Replacing the package removes that directory whole, and python3 exits 2 for a
    missing script -- the number the hook protocol reads as "block this turn". Measured on the
    real incident: one removed directory, eleven repeated Stop prompts in one turn and eight in
    a second task's turn, and neither turn able to end until a compatibility path was restored;
    an isolated reproduction of the same manoeuvre produced thirty-seven in one turn. So these
    cases drive the ACTUAL declared command through
    a shell, the way the host runs it, rather than asserting anything about its text.
    """

    DECLARATION = ROOT / "plugins/crw/wiring/hooks/stop-recording-completion.json"
    PAYLOAD = b'{"hook_event_name": "Stop", "session_id": "s", "turn_id": "t"}'

    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.root = Path(self.stack.enter_context(tempfile.TemporaryDirectory(prefix="crw178-")))
        document = json.loads(self.DECLARATION.read_text(encoding="utf-8"))
        entry = document["hooks"]["Stop"][0]["hooks"][0]
        self.command = entry["command"]
        self.timeout = entry["timeout"]
        self.witness = self.root / "witness.txt"

    def plant(self, path, tag, body="raise SystemExit(0)\n"):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("open(%r, 'a').write(%r + chr(10))\n%s" % (str(self.witness), tag, body),
                        encoding="utf-8")

    def fire(self, plugin_root, codex_home, command=None, with_root=True):
        if self.witness.exists():
            self.witness.unlink()
        environment = {"PATH": os.environ["PATH"], "CODEX_HOME": str(codex_home)}
        if with_root:
            environment["PLUGIN_ROOT"] = str(plugin_root)
        done = subprocess.run(["/bin/sh", "-lc", command or self.command], input=self.PAYLOAD,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=environment)
        ran = self.witness.read_text(encoding="utf-8").split() if self.witness.exists() else []
        # The one number that must never appear, whatever else happened.
        self.assertNotEqual(done.returncode, 2,
                            "the blocking exit code escaped: " + repr(done.stderr[:200]))
        return done, ran

    def homes(self):
        """An ordinary home, and one whose path carries a space, a quote and a dollar."""
        plain = self.root / "plain home"
        awkward = self.root / "it's $HOME really"
        for home in (plain, awkward):
            home.mkdir(parents=True, exist_ok=True)
        return plain, awkward

    def test_the_packaged_copy_runs_while_the_cache_is_there(self):
        """The current version always wins, so an older fallback can never outrank it."""
        cache = self.root / "cache" / "0.4.0"
        self.plant(cache / "wiring" / "crw_stop_hook.py", "PACKAGED")
        for home in self.homes():
            self.plant(home / "crw-stop-hook.py", "FALLBACK")
            done, ran = self.fire(cache, home)
            self.assertEqual((done.returncode, ran), (0, ["PACKAGED"]), str(home))
            self.assertEqual(done.stdout, b"")

    def test_the_fallback_answers_when_the_cache_was_replaced_mid_turn(self):
        """The incident itself: the path the turn holds is gone before the turn ends."""
        gone = self.root / "cache" / "0.3.0-removed"
        for home in self.homes():
            self.plant(home / "crw-stop-hook.py", "FALLBACK")
            done, ran = self.fire(gone, home)
            self.assertEqual((done.returncode, ran), (0, ["FALLBACK"]), str(home))
            self.assertEqual((done.stdout, done.stderr), (b"", b""))

    def test_neither_candidate_releases_the_turn_in_silence(self):
        """A host with the package and no runtime has nothing to run and loses nothing.

        This is the case the old declaration turned into a termination loop, and it is not a
        blanket suppression: the launcher's own contract is already that a Stop it cannot judge
        is a Stop it releases. What sat outside that contract was the interpreter failing to
        open its own argument, and that is what this closes.
        """
        gone = self.root / "cache" / "0.3.0-removed"
        home = self.root / "empty home"
        home.mkdir()
        done, ran = self.fire(gone, home)
        self.assertEqual((done.returncode, ran, done.stdout, done.stderr), (0, [], b"", b""))

    def test_a_candidate_that_fails_while_running_is_reported_and_not_retried(self):
        """Read failure and run failure are different, and only the first may fall through.

        A launcher that raises after doing half its work has already acted on this Stop, so
        running the fallback as well would process one Stop twice. The error surfaces instead,
        as exit 1, which the host reads as an ordinary failure rather than as a hold.
        """
        cache = self.root / "cache" / "0.4.0"
        self.plant(cache / "wiring" / "crw_stop_hook.py", "PACKAGED",
                   "raise OSError(13, 'after side effects')\n")
        home = self.root / "home"
        home.mkdir()
        self.plant(home / "crw-stop-hook.py", "FALLBACK")
        done, ran = self.fire(cache, home)
        self.assertEqual(ran, ["PACKAGED"])
        self.assertEqual(done.returncode, 1)
        self.assertIn(b"OSError", done.stderr)

    def test_a_truncated_launcher_is_reported_rather_than_run(self):
        cache = self.root / "cache" / "0.4.0"
        (cache / "wiring").mkdir(parents=True)
        (cache / "wiring" / "crw_stop_hook.py").write_text("def (\n", encoding="utf-8")
        home = self.root / "home"
        home.mkdir()
        done, ran = self.fire(cache, home)
        self.assertEqual((done.returncode, ran), (1, []))
        self.assertIn(b"SyntaxError", done.stderr)

    def test_a_directory_at_either_candidate_is_stepped_over(self):
        cache = self.root / "cache" / "0.4.0"
        (cache / "wiring" / "crw_stop_hook.py").mkdir(parents=True)
        home = self.root / "home"
        home.mkdir()
        (home / "crw-stop-hook.py").mkdir()
        done, ran = self.fire(cache, home)
        self.assertEqual((done.returncode, ran, done.stdout), (0, [], b""))

    def test_an_unexpanded_plugin_root_falls_through_rather_than_failing(self):
        """A host that does not substitute the variable leaves the literal text behind."""
        home = self.root / "home"
        home.mkdir()
        self.plant(home / "crw-stop-hook.py", "FALLBACK")
        done, ran = self.fire(None, home, with_root=False)
        self.assertEqual((done.returncode, ran), (0, ["FALLBACK"]))

    def test_the_real_launcher_still_answers_through_the_declaration(self):
        """Not a stub: the packaged launcher, reached the way the host reaches it."""
        cache = self.root / "cache" / "0.4.0"
        (cache / "wiring").mkdir(parents=True)
        shutil.copyfile(ROOT / "plugins/crw/wiring/crw_stop_hook.py",
                        cache / "wiring" / "crw_stop_hook.py")
        home = self.root / "home"
        home.mkdir()
        adapter = home / "adapter.py"
        seen = home / "seen.txt"
        adapter.write_text("import sys\n"
                           "open(%r, 'a').write(sys.stdin.read())\n"
                           "sys.stdout.write('{\"decision\": \"block\"}')\n" % str(seen),
                           encoding="utf-8")
        (home / "crw-completion-hook.json").write_text(json.dumps(
            {"configVersion": 1, "event": "Stop", "owner": "plugin", "mode": "observe",
             "timeoutSeconds": 5, "adapterInterpreter": sys.executable,
             "adapterEntryPoint": str(adapter)}), encoding="utf-8")
        done, _ = self.fire(cache, home)
        self.assertEqual((done.returncode, done.stdout), (0, b'{"decision": "block"}'))
        self.assertIn("turn_id", seen.read_text(encoding="utf-8"))

    def test_a_launcher_that_exits_nonzero_is_reported_not_swallowed(self):
        """Devin review: an explicit failure must not become a success.

        The launcher's own contract is to exit 0 on every path. A copy that breaks it is saying
        something, and the bootstrap converting that into 0 would hide exactly the failure the
        launcher went out of its way to report. It is reported as 1 rather than as the code the
        launcher chose, because 2 is the host's blocking code and no path here may produce it.
        """
        for code in (1, 3, 2):
            with self.subTest(code=code):
                cache = self.root / ("cache-%d" % code) / "0.4.0"
                self.plant(cache / "wiring" / "crw_stop_hook.py", "PACKAGED",
                           "raise SystemExit(%d)\n" % code)
                home = self.root / ("home-%d" % code)
                home.mkdir(parents=True, exist_ok=True)
                self.plant(home / "crw-stop-hook.py", "FALLBACK")
                done, ran = self.fire(cache, home)
                self.assertEqual(ran, ["PACKAGED"])
                self.assertEqual(done.returncode, 1)

    def test_a_launcher_that_exits_zero_explicitly_is_still_a_success(self):
        cache = self.root / "cache-ok" / "0.4.0"
        self.plant(cache / "wiring" / "crw_stop_hook.py", "PACKAGED", "raise SystemExit(0)\n")
        home = self.root / "home-ok"
        home.mkdir(parents=True, exist_ok=True)
        done, ran = self.fire(cache, home)
        self.assertEqual((done.returncode, ran), (0, ["PACKAGED"]))


    def test_system_exit_codes_follow_the_interpreter_not_their_truthiness(self):
        """Devin review: SystemExit.code is not restricted to integers.

        CPython exits 0 only for None and an integer zero, and False is an integer zero. Every
        other object, including a falsey one like '' or 0.0, exits 1. Testing truthiness would
        call those two a success and hide a launcher that failed on purpose.
        """
        successes = ("None", "0", "False")
        failures = ("1", "3", "2", "''", "0.0", "'boom'", "[]")
        for literal in successes + failures:
            with self.subTest(code=literal):
                cache = self.root / ("c-" + str(abs(hash(literal)))) / "0.4.0"
                self.plant(cache / "wiring" / "crw_stop_hook.py", "PACKAGED",
                           "raise SystemExit(%s)\n" % literal)
                home = self.root / ("h-" + str(abs(hash(literal))))
                home.mkdir(parents=True, exist_ok=True)
                done, ran = self.fire(cache, home)
                self.assertEqual(ran, ["PACKAGED"])
                self.assertEqual(done.returncode, 0 if literal in successes else 1,
                                 "SystemExit(%s) was mapped to %d" % (literal, done.returncode))


    def test_an_integer_subclass_is_judged_by_its_value_not_its_equality(self):
        """Devin review: CPython derives the status from the stored value, not from __eq__.

        int is subclassable and __eq__ is overridable, so comparing the code with zero can
        dispatch into a method that answers something unrelated to the number. int(code) reads
        the value the interpreter would use, which is the rule this declaration claims to follow.
        """
        shapes = {
            "__eq__": "class E(int):\n"
                      "    def __eq__(self, other):\n"
                      "        return %s\n"
                      "    def __hash__(self):\n"
                      "        return 0\n"
                      "raise SystemExit(E(%d))\n",
            "__int__": "class E(int):\n"
                       "    def __int__(self):\n"
                       "        return %s\n"
                       "raise SystemExit(E(%d))\n",
        }
        for name, template in sorted(shapes.items()):
            lies = "True" if name == "__eq__" else "0"
            truths = "False" if name == "__eq__" else "5"
            for says, value, expected in ((lies, 5, 1), (truths, 0, 0)):
                with self.subTest(override=name, value=value, says=says):
                    tag = "%s-%s-%d" % (name.strip("_"), says, value)
                    cache = self.root / ("sub-" + tag) / "0.4.0"
                    self.plant(cache / "wiring" / "crw_stop_hook.py", "PACKAGED",
                               template % (says, value))
                    home = self.root / ("subhome-" + tag)
                    home.mkdir(parents=True, exist_ok=True)
                    done, ran = self.fire(cache, home)
                    self.assertEqual(ran, ["PACKAGED"])
                    self.assertEqual(done.returncode, expected,
                                     "E(%d) with %s -> %s was mapped to %d"
                                     % (value, name, says, done.returncode))


    def test_the_declaration_stays_within_the_timeout_the_host_clamps(self):
        self.assertLessEqual(self.timeout, plugin.HOOK_TIMEOUT_SECONDS)


class LauncherContractVersionTest(unittest.TestCase):
    """An installed fallback outlives the package that wrote it, so it may meet a later contract."""

    LAUNCHER = ROOT / "plugins/crw/wiring/crw_stop_hook.py"

    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.home = Path(self.stack.enter_context(tempfile.TemporaryDirectory(prefix="crw178-")))
        self.seen = self.home / "seen.txt"
        self.adapter = self.home / "adapter.py"
        self.adapter.write_text("open(%r, 'a').write('ran')\n" % str(self.seen),
                                encoding="utf-8")

    def fire(self, **overrides):
        document = {"configVersion": 1, "event": "Stop", "owner": "plugin", "mode": "observe",
                    "timeoutSeconds": 5, "adapterInterpreter": sys.executable,
                    "adapterEntryPoint": str(self.adapter)}
        document.update(overrides)
        for key in [name for name, value in document.items() if value is None]:
            del document[key]
        (self.home / "crw-completion-hook.json").write_text(json.dumps(document),
                                                            encoding="utf-8")
        done = subprocess.run([sys.executable, str(self.LAUNCHER)], input="{}",
                              capture_output=True, text=True,
                              env={"PATH": os.environ["PATH"], "CODEX_HOME": str(self.home)})
        return done, self.seen.exists()

    def test_the_contract_it_implements_is_acted_on(self):
        done, ran = self.fire()
        self.assertEqual((done.returncode, ran), (0, True))

    def test_settings_written_before_the_key_existed_are_still_acted_on(self):
        done, ran = self.fire(configVersion=None)
        self.assertEqual((done.returncode, ran), (0, True))

    def test_a_contract_it_does_not_implement_is_stood_down_from(self):
        """A copy left by an older install does nothing rather than guess at a later document."""
        done, ran = self.fire(configVersion=2)
        self.assertEqual((done.returncode, done.stdout, done.stderr, ran), (0, "", "", False))

    def test_the_marker_it_carries_is_the_one_the_installer_looks_for(self):
        self.assertIn(completion.LAUNCHER_MARKER, self.LAUNCHER.read_text(encoding="utf-8"))


class StableLauncherPlacementTest(unittest.TestCase):
    """Who may write the fallback, and what it refuses to write over."""

    SOURCE = ROOT / "plugins/crw/wiring/crw_stop_hook.py"

    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.home = Path(self.stack.enter_context(tempfile.TemporaryDirectory(prefix="crw178-")))
        self.path = completion.launcher_path(self.home)

    def place(self, **kwargs):
        return completion.place_launcher(self.path, self.SOURCE, **kwargs)

    def test_a_dry_run_writes_nothing_and_says_what_it_would_do(self):
        answer = self.place()
        self.assertEqual(answer["outcome"], completion.LAUNCHER_WOULD_PLACE)
        self.assertFalse(answer["applied"])
        self.assertFalse(self.path.exists())

    def test_applying_installs_the_bytes_this_checkout_ships_and_reads_them_back(self):
        answer = self.place(apply=True)
        self.assertEqual(answer["outcome"], completion.LAUNCHER_PLACED)
        self.assertEqual(self.path.read_bytes(), self.SOURCE.read_bytes())
        self.assertEqual(answer["digest"], answer["sourceDigest"])

    def test_a_second_run_changes_nothing(self):
        self.place(apply=True)
        self.assertEqual(self.place(apply=True)["outcome"], completion.LAUNCHER_UNCHANGED)

    def test_a_file_without_the_marker_is_left_exactly_where_it_is(self):
        """Ownership, not provenance: what it proves is that CRW put a launcher here."""
        self.path.write_text("not ours\n", encoding="utf-8")
        answer = self.place(apply=True)
        self.assertEqual(answer["outcome"], completion.LAUNCHER_FOREIGN)
        self.assertEqual(self.path.read_text(encoding="utf-8"), "not ours\n")

    def test_a_symlink_is_reported_by_kind_and_never_followed(self):
        target = self.home / "elsewhere.py"
        target.write_text("someone else\n", encoding="utf-8")
        self.path.symlink_to(target)
        answer = self.place(apply=True)
        self.assertEqual(answer["outcome"], completion.LAUNCHER_NOT_A_FILE)
        self.assertEqual(answer["kind"], "symlink")
        self.assertTrue(self.path.is_symlink())
        self.assertEqual(target.read_text(encoding="utf-8"), "someone else\n")

    def test_a_directory_is_refused_rather_than_replaced(self):
        self.path.mkdir()
        answer = self.place(apply=True)
        self.assertEqual((answer["outcome"], answer["kind"]),
                         (completion.LAUNCHER_NOT_A_FILE, "directory"))
        self.assertTrue(self.path.is_dir())

    def test_a_packaged_source_that_cannot_be_read_places_nothing(self):
        answer = completion.place_launcher(self.path, self.home / "absent.py", apply=True)
        self.assertEqual(answer["outcome"], completion.LAUNCHER_SOURCE_MISSING)
        self.assertFalse(self.path.exists())

    def test_a_legacy_document_is_refused_before_a_fallback_is_placed(self):
        """Devin review: a fallback must not be wired to settings this command will not accept.

        Measured rather than assumed: the ownership precondition reads the live document and
        runs the same complaints() over it, so a host carrying the old eight second budget is
        refused there, before the placement block is reached at all. The emitted receipt carries
        no launcher cell and nothing is written. This case pins that ordering, because the
        placement is only safe while some earlier precondition keeps an unusable document from
        ever reaching it.
        """
        home = self.home / "legacy"
        home.mkdir()
        (home / completion.CONFIG_NAME).write_text(json.dumps(
            {"configVersion": 1, "event": "Stop", "owner": "plugin", "mode": "observe",
             "timeoutSeconds": 8, "adapterInterpreter": sys.executable,
             "adapterEntryPoint": sys.executable, "relayExecutable": "/bin/true",
             "markerRoot": str(home / "marker")}), encoding="utf-8")
        relay = self.home / "relay"
        relay.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        relay.chmod(0o755)
        done = subprocess.run(
            [sys.executable, str(ROOT / "scripts" / "runtime_install.py"), "hook",
             "--adapter", "completion", "--owner", "plugin", "--codex-home", str(home),
             "--relay-command", str(relay), "--apply"], capture_output=True, text=True)
        self.assertNotEqual(done.returncode, 0, done.stdout[:400])
        emitted = json.loads(done.stdout)
        self.assertIsNone(emitted.get("launcher"))
        self.assertIn("timeoutSeconds", emitted["error"])
        self.assertFalse(completion.launcher_path(home).exists())


    def test_a_budget_the_plugin_owner_may_not_record_places_no_fallback(self):
        """Devin review, by the route the first measurement missed.

        The earlier check exercised a host that already carried a bad document, and the
        ownership precondition caught that one before the placement. A fresh host with the
        budget on the command line took a different road: the document was only judged inside
        write_configuration, which runs after the launcher is placed, so the run left a
        launcher on disk and then refused the settings. The judgement moved into the
        preconditions, where "nothing was written" is promised.
        """
        relay = self.home / "relay"
        relay.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        relay.chmod(0o755)
        for budget, expected in ((8, False), (7, True)):
            with self.subTest(budget=budget):
                home = self.home / ("budget-%d" % budget)
                done = subprocess.run(
                    [sys.executable, str(ROOT / "scripts" / "runtime_install.py"), "hook",
                     "--adapter", "completion", "--owner", "plugin", "--codex-home", str(home),
                     "--relay-command", str(relay), "--guard-timeout", str(budget), "--apply"],
                    capture_output=True, text=True)
                placed = completion.launcher_path(home).exists()
                self.assertEqual(placed, expected,
                                 "budget %d: launcher placed=%s\n%s"
                                 % (budget, placed, done.stdout[:400]))
                settled = (home / completion.CONFIG_NAME).exists()
                self.assertEqual(settled, expected, "budget %d: settings=%s" % (budget, settled))
                if expected:
                    self.assertEqual(done.returncode, 0, done.stdout[:300])
                else:
                    self.assertNotEqual(done.returncode, 0, done.stdout[:300])
                    self.assertIn("timeoutSeconds", json.loads(done.stdout)["error"])


    def test_the_state_reader_separates_the_marker_from_the_digest(self):
        self.place(apply=True)
        state = completion.launcher_state(self.home, self.SOURCE)
        self.assertEqual((state["kind"], state["carriesMarker"], state["matchesCheckout"]),
                         ("file", True, True))
        self.path.write_text(self.SOURCE.read_text(encoding="utf-8") + "# drift\n",
                             encoding="utf-8")
        drifted = completion.launcher_state(self.home, self.SOURCE)
        self.assertTrue(drifted["carriesMarker"])
        self.assertFalse(drifted["matchesCheckout"])


class StableLauncherRemovalTest(unittest.TestCase):
    """Removal takes only the file it put there, and says what the host held afterwards."""

    SOURCE = ROOT / "plugins/crw/wiring/crw_stop_hook.py"

    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.home = Path(self.stack.enter_context(tempfile.TemporaryDirectory(prefix="crw178-")))
        self.path = completion.launcher_path(self.home)
        sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import steps
        self.steps = steps
        self.host = {"codexHome": str(self.home)}

    def remove(self, **kwargs):
        return self.steps.launcher_remove(self.host, {}, **kwargs)

    def test_nothing_there_is_already_done(self):
        self.assertEqual(self.remove(apply=True)["outcome"], self.steps.ALREADY)

    def test_a_dry_run_removes_nothing(self):
        completion.place_launcher(self.path, self.SOURCE, apply=True)
        self.assertEqual(self.remove()["outcome"], self.steps.WOULD)
        self.assertTrue(self.path.is_file())

    def test_the_file_it_installed_is_the_file_it_removes(self):
        completion.place_launcher(self.path, self.SOURCE, apply=True)
        answer = self.remove(apply=True)
        self.assertEqual(answer["outcome"], self.steps.SETTLED)
        self.assertFalse(self.path.exists())

    def test_a_file_without_the_marker_is_refused(self):
        self.path.write_text("not ours\n", encoding="utf-8")
        answer = self.remove(apply=True)
        self.assertEqual(answer["outcome"], self.steps.REFUSED)
        self.assertEqual(self.path.read_text(encoding="utf-8"), "not ours\n")

    def test_a_symlink_is_refused_and_its_target_is_untouched(self):
        target = self.home / "elsewhere.py"
        target.write_text("someone else\n", encoding="utf-8")
        self.path.symlink_to(target)
        answer = self.remove(apply=True)
        self.assertEqual(answer["outcome"], self.steps.REFUSED)
        self.assertIn("symlink", answer["detail"])
        self.assertTrue(target.is_file())

    def test_settings_written_back_around_the_run_are_reported_not_hidden(self):
        """Two files, two locks. The race is not prevented here; it is made impossible to miss."""
        completion.place_launcher(self.path, self.SOURCE, apply=True)
        (self.home / completion.CONFIG_NAME).write_text("{}", encoding="utf-8")
        answer = self.remove(apply=True)
        # Not settled: settled is read as "stopped", and a host whose settings came back can be
        # called again through the packaged copy. The file was still removed, and the detail says so.
        self.assertEqual(answer["outcome"], self.steps.LIVE_AGAIN)
        self.assertFalse(self.path.exists())
        self.assertTrue(answer["settingsPresent"])
        self.assertIn("are NOT stopped", answer["detail"])

    def test_a_surface_that_came_back_is_not_listed_as_stopped(self):
        """Devin review: the aggregate claim has to agree with the host, not with an earlier step."""
        completion.place_launcher(self.path, self.SOURCE, apply=True)
        (self.home / completion.CONFIG_NAME).write_text("{}", encoding="utf-8")
        claims = self.steps.stop_claims([self.remove(apply=True)], self.steps.REMOVE_CLAIMS)
        self.assertFalse(any("fallback" in claim for claim in claims["stopped"]))
        self.assertTrue(any("fallback" in claim for claim in claims["stillLive"]))

    def test_a_surface_that_came_back_makes_the_command_exit_nonzero(self):
        """Devin review: shell automation reads the exit status, not the JSON.

        live_again means the file really was removed and the surface is callable again anyway.
        A zero here would let a cleanup script carry on against a host where the adapter can
        still be reached, so it gets its own status rather than being folded into a refusal.
        """
        sys.path.insert(0, str(ROOT / "scripts"))
        import plugin_transition

        completion.place_launcher(self.path, self.SOURCE, apply=True)
        (self.home / completion.CONFIG_NAME).write_text("{}", encoding="utf-8")
        answer = self.remove(apply=True)
        self.assertEqual(answer["outcome"], self.steps.LIVE_AGAIN)
        self.assertEqual(plugin_transition.verdict([answer]), plugin_transition.EXIT_INCOMPLETE)
        self.assertNotEqual(plugin_transition.EXIT_INCOMPLETE, plugin_transition.EXIT_OK)
        # settled alone still exits zero, so the new status is not a blanket nonzero.
        settled = dict(answer, outcome=self.steps.SETTLED)
        self.assertEqual(plugin_transition.verdict([settled]), plugin_transition.EXIT_OK)


    def test_disable_never_claims_the_fallback_it_does_not_touch(self):
        """disable stops calls and deletes nothing, so the fallback is not its surface to report."""
        claims = self.steps.stop_claims([])
        self.assertFalse(any("fallback" in claim for claim in claims["stillLive"]))

class PluginGuardBudgetTest(unittest.TestCase):
    """The budget a plugin-owned document may record, enforced where both writers validate.

    The packaged launcher waits min(timeoutSeconds + MARGIN, CEILING). Refusing only at the
    ceiling let 8 through, and at 8 the launcher deadline is 9 while the adapter is still
    allowed 8: the margin is gone and the launcher kills the adapter in the middle of writing
    the record of its own timeout. The bound lived in the transition only, so
    runtime_install.py hook --owner plugin accepted a document the transition refused.
    """

    def document(self, budget):
        return {"configVersion": 1, "event": "Stop", "owner": "plugin", "mode": "observe",
                "timeoutSeconds": budget, "adapterInterpreter": "/usr/bin/python3",
                "adapterEntryPoint": "/opt/crw/bin/crw-completion-hook",
                "relayExecutable": "/opt/crw/bin/codex-session-relay",
                "markerRoot": "/opt/crw/marker"}

    def test_the_bound_is_the_ceiling_minus_the_margin(self):
        self.assertEqual(completion.MAX_PLUGIN_GUARD_SECONDS,
                         completion.LAUNCHER_CEILING_SECONDS
                         - completion.LAUNCHER_MARGIN_SECONDS)
        self.assertEqual(completion.MAX_PLUGIN_GUARD_SECONDS, 7)

    def test_a_budget_that_collapses_the_margin_is_refused(self):
        for budget in (8, 8.5, 9, 10):
            with self.subTest(budget=budget):
                found = completion.complaints(self.document(budget))
                self.assertTrue([item for item in found if "timeoutSeconds" in item],
                                "budget %r was accepted: %r" % (budget, found))

    def test_a_budget_that_keeps_the_margin_is_accepted(self):
        for budget in (1, 5, 7):
            with self.subTest(budget=budget):
                found = completion.complaints(self.document(budget))
                self.assertEqual([item for item in found if "timeoutSeconds" in item], [],
                                 "budget %r was refused" % budget)

    def test_the_transition_and_the_installer_share_one_bound(self):
        """They disagreed before: one refused 8 and the other wrote it."""
        sys.path.insert(0, str(ROOT / "scripts"))
        from crw_transition import steps

        self.assertEqual(steps.MAX_GUARD_SECONDS, completion.MAX_PLUGIN_GUARD_SECONDS)
        self.assertEqual(steps.LAUNCHER_MARGIN_SECONDS, completion.LAUNCHER_MARGIN_SECONDS)

    def test_the_launcher_mirrors_the_same_two_numbers(self):
        """The launcher cannot import this module, so the numbers are asserted to agree."""
        source = (ROOT / "plugins/crw/wiring/crw_stop_hook.py").read_text(encoding="utf-8")
        self.assertIn("MAX_SECONDS = " + str(completion.LAUNCHER_CEILING_SECONDS), source)
        self.assertIn("MARGIN_SECONDS = " + str(completion.LAUNCHER_MARGIN_SECONDS), source)
