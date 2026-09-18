"""Check the package validator against the shapes that install silently wrong."""

import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts/ci/plugin.py"
NAMES = ("crw-check", "crw-define", "crw-logic", "crw-loop", "crw-next", "crw-plan", "crw-run")

_spec = importlib.util.spec_from_file_location("crw_plugin_check", SCRIPT)
plugin = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(plugin)


def manifest(**overrides):
    base = {
        "name": "crw",
        "version": "0.1.0",
        "description": "d",
        "author": {"name": "a"},
        "license": "MIT",
        "skills": "./skills/",
        "interface": {
            "displayName": "CRW", "shortDescription": "s", "longDescription": "l",
            "developerName": "a", "category": "Developer Tools",
            "capabilities": ["Skills"], "defaultPrompt": ["p"],
        },
    }
    base.update(overrides)
    return base


def payload(files):
    return {name: ("100644", data.encode()) for name, data in files.items()}


SKILL = "---\nname: crw-run\ndescription: d\n---\n"
GOOD = {
    ".codex-plugin/plugin.json": json.dumps(manifest()),
    "skills/crw-run/SKILL.md": SKILL,
    "skills/crw-run/agents/openai.yaml": "interface:\n",
    "LICENSE": "MIT",
}


class ManifestTests(unittest.TestCase):
    def test_required_fields_and_semver(self):
        self.assertEqual(plugin.manifest_errors(manifest()), [])
        for bad, expected in (({"version": "1.0"}, "not semantic"),
                              ({"author": {"url": "u"}}, "author.name"),
                              ({"skills": ""}, "missing skills")):
            with self.subTest(bad=bad):
                errors = plugin.manifest_errors(manifest(**bad))
                self.assertTrue(any(expected in e for e in errors), errors)

    def test_missing_interface_field_is_refused(self):
        broken = manifest()
        del broken["interface"]["defaultPrompt"]
        self.assertTrue(any("interface.defaultPrompt" in e for e in plugin.manifest_errors(broken)))

    def test_undeclared_components_stay_undeclared(self):
        # Declaring a component replaces default discovery, so an accidental
        # hooks or mcpServers field would change what loads without saying so.
        for field in ("hooks", "mcpServers", "apps"):
            with self.subTest(field=field):
                errors = plugin.manifest_errors(manifest(**{field: "./x.json"}))
                self.assertTrue(any(field in e for e in errors), errors)


class HygieneTests(unittest.TestCase):
    def test_clean_payload_passes(self):
        self.assertEqual(plugin.hygiene(payload(GOOD), "t"), [])

    def test_top_level_allowlist(self):
        errors = plugin.hygiene(payload({**GOOD, "packages/relay.py": "x"}), "t")
        self.assertTrue(any("may ship in the package" in e for e in errors), errors)

    def test_operational_state_and_credentials(self):
        for name in (".codexclaw/sessions/s.json", "skills/crw-run/relay.sqlite3",
                     ".env", "skills/id_rsa"):
            with self.subTest(name=name):
                errors = plugin.hygiene(payload({**GOOD, name: "x"}), "t")
                self.assertTrue(any("may not ship" in e for e in errors), errors)

    def test_personal_paths_are_refused_and_placeholders_are_not(self):
        bad = plugin.hygiene(payload({**GOOD, "skills/crw-run/SKILL.md":
                                      "put it in /home/someone/code/x"}), "t")
        self.assertTrue(any("personal path" in e for e in bad), bad)
        placeholder = plugin.hygiene(payload({**GOOD, "skills/crw-run/SKILL.md":
                                              "put it in <worktree-root>/<project> or /example/home/x"}), "t")
        self.assertEqual(placeholder, [])


class SkillSetTests(unittest.TestCase):
    def test_declared_directory_is_the_skill_set(self):
        errors, found = plugin.skills(payload(GOOD), manifest(), "t")
        self.assertEqual(errors, [])
        self.assertEqual(sorted(found), ["crw-run"])

    def test_interface_metadata_is_required_per_skill(self):
        files = {k: v for k, v in GOOD.items() if not k.endswith("openai.yaml")}
        errors, _ = plugin.skills(payload(files), manifest(), "t")
        self.assertTrue(any("agents/openai.yaml" in e for e in errors), errors)

    def test_declaration_may_not_leave_the_plugin_root(self):
        errors, _ = plugin.skills(payload(GOOD), manifest(skills="../../skills/"), "t")
        self.assertTrue(errors)

    def test_empty_declaration_is_refused(self):
        errors, _ = plugin.skills(payload({".codex-plugin/plugin.json": "{}"}), manifest(), "t")
        self.assertTrue(any("ships no skill" in e for e in errors), errors)


class DigestTests(unittest.TestCase):
    def test_digest_is_order_independent_and_content_sensitive(self):
        first = plugin.digest(payload(GOOD))
        reordered = plugin.digest(payload(dict(reversed(list(GOOD.items())))))
        self.assertEqual(first, reordered)
        changed = dict(GOOD, LICENSE="MIT ")
        self.assertNotEqual(first, plugin.digest(payload(changed)))

    def test_digest_covers_the_file_mode(self):
        executable = dict(payload(GOOD))
        executable["LICENSE"] = ("100755", b"MIT")
        self.assertNotEqual(plugin.digest(payload(GOOD)), plugin.digest(executable))


class CommandTests(unittest.TestCase):
    def run_script(self, *args):
        return subprocess.run([sys.executable, str(SCRIPT), *args], capture_output=True, text=True)

    def write_payload(self, root, files):
        for name, data in files.items():
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(data, encoding="utf-8")

    def test_repository_package_reports_the_namespaced_skill_names(self):
        result = self.run_script("--json")
        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads(result.stdout)
        self.assertEqual(report["expectedSkillNames"], [f"crw:{name}" for name in NAMES])
        self.assertEqual(report["skills"], list(NAMES))
        head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=ROOT,
                              capture_output=True, text=True, check=True).stdout.strip()
        self.assertEqual(report["resolved"], head)
        self.assertEqual(report["digest"], json.loads(self.run_script("--json").stdout)["digest"])

    def test_installed_payload_is_validated_with_the_same_rules(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / "crw"
            self.write_payload(root, GOOD)
            good = self.run_script("--payload", str(root), "--json")
            self.assertEqual(good.returncode, 0, good.stderr)
            self.assertEqual(json.loads(good.stdout)["expectedSkillNames"], ["crw:crw-run"])

    def test_installed_payload_with_a_symlink_is_refused(self):
        # The installer drops symlinks, so a package that relies on one loses those files.
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / "crw"
            self.write_payload(root, GOOD)
            (root / "skills/crw-plan").symlink_to(root / "skills/crw-run", target_is_directory=True)
            result = self.run_script("--payload", str(root))
            self.assertEqual(result.returncode, 1)
            self.assertIn("symlink", result.stderr)


if __name__ == "__main__":
    unittest.main()

