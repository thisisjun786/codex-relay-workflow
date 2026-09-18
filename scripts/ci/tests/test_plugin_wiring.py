"""One owner registers the completion Stop hook, and the other is refused with its evidence.

A plugin package declares its hooks in its own manifest, so a plugin-owned registration leaves
the user hook file empty. Every check that reads only the hook file therefore answers "nothing
is registered" while two hooks run on every Stop. These cases exercise the record that closes
that gap, in both directions, because either owner can be installed first.

Nothing here touches a real Codex home, an installed runtime or an operational database. The
relay is a file these cases create, and every command runs against a temporary CODEX_HOME.
"""

import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))

from crw_runtime import completion, hooks

RUNTIME_INSTALL = ROOT / "scripts" / "runtime_install.py"


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
        self.stack = __import__("contextlib").ExitStack()
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


if __name__ == "__main__":
    unittest.main()
