"""Selection evidence comes from actual Git changes, including both rename sides."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "scope.py"
spec = importlib.util.spec_from_file_location("ci_scope", SCRIPT)
scope = importlib.util.module_from_spec(spec)
spec.loader.exec_module(scope)


class ScopeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.git("init", "-q")
        self.git("config", "user.name", "CI fixture")
        self.git("config", "user.email", "ci@example.invalid")
        self.write("README.md", "initial\n")
        self.commit()
        self.base = self.git("rev-parse", "HEAD").strip()

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.root, text=True,
                                       stderr=subprocess.STDOUT)

    def write(self, path, text):
        target = self.root / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(text)

    def commit(self):
        self.git("add", ".")
        self.git("commit", "-qm", "fixture")

    def select(self, **kwargs):
        return scope.select(self.root, base=kwargs.get("base", self.base), head="HEAD",
                            event=kwargs.get("event", "pull_request"),
                            base_ref=kwargs.get("base_ref", "dev"),
                            ref=kwargs.get("ref", "refs/pull/1/merge"))

    def test_prose_runs_no_expensive_suite(self):
        self.write("docs/releases.md", "release procedure\n")
        self.commit()
        result = self.select()
        self.assertEqual(result["selected"], {"tests": False, "packages": False})
        self.assertEqual(result["reason"], "paths")
        self.assertEqual(result["changed"], ["docs/releases.md"])

    def test_skill_keeps_instruction_tests(self):
        self.write("plugins/crw/skills/crw-run/SKILL.md", "instructions\n")
        self.commit()
        self.assertEqual(self.select()["selected"], {"tests": True, "packages": False})

    def test_runtime_manifest_and_ci_select_both(self):
        for path in ("packages/bridge/src/a.py", "scripts/runtime_install.py",
                     "plugins/crw/wiring/launch.py", "plugins/crw/.codex-plugin/plugin.json",
                     ".github/workflows/ci.yml", "pyproject.toml"):
            with self.subTest(path=path):
                self.assertEqual(scope.classify(path), "full")

    def test_skill_plus_manifest_stays_conservatively_full(self):
        self.write("plugins/crw/skills/crw-run/SKILL.md", "instructions\n")
        self.write("plugins/crw/.codex-plugin/plugin.json", "{}\n")
        self.commit()
        self.assertEqual(self.select()["selected"], {"tests": True, "packages": True})

    def test_unknown_candidate_path_is_not_hidden_by_docs_diff(self):
        self.write("new-component/source.py", "pass\n")
        self.commit()
        self.base = self.git("rev-parse", "HEAD").strip()
        self.write("README.md", "changed\n")
        self.commit()
        result = self.select()
        self.assertEqual(result["unknown"], ["new-component/source.py"])
        self.assertEqual(result["selected"], {"tests": True, "packages": True})

    def test_rename_out_of_source_does_not_become_prose(self):
        self.write("packages/bridge/guide.md", "same\n")
        self.commit()
        self.base = self.git("rev-parse", "HEAD").strip()
        (self.root / "docs").mkdir()
        self.git("mv", "packages/bridge/guide.md", "docs/guide.md")
        self.commit()
        result = self.select()
        self.assertEqual(result["changed"], ["docs/guide.md", "packages/bridge/guide.md"])
        self.assertTrue(result["selected"]["packages"])

    def test_prose_type_or_mode_change_selects_full(self):
        (self.root / "README.md").chmod(0o755)
        self.commit()
        self.assertTrue(self.select()["selected"]["packages"])

    def test_prose_symlink_is_not_a_docs_exemption(self):
        (self.root / "README.md").unlink()
        (self.root / "README.md").symlink_to("LICENSE")
        self.commit()
        self.assertTrue(self.select()["selected"]["packages"])

    def test_empty_unavailable_and_dispatch_choose_full(self):
        for args in ({}, {"base": "0" * 40}, {"base": "absent"},
                     {"event": "workflow_dispatch", "ref": "refs/heads/dev"}):
            with self.subTest(args=args):
                self.assertEqual(self.select(**args)["selected"],
                                 {"tests": True, "packages": True})

    def test_invalid_events_and_main_pr_refuse(self):
        for args in ({"base_ref": "main"}, {"base_ref": ""},
                     {"event": "push", "ref": "refs/heads/main"},
                     {"event": "pull_request_target"}):
            with self.subTest(args=args), self.assertRaises(ValueError):
                self.select(**args)

    def test_dev_push_and_manual_chain_are_supported(self):
        self.write("README.md", "change\n")
        self.commit()
        self.assertFalse(self.select(event="push", ref="refs/heads/dev")["selected"]["tests"])
        self.assertFalse(self.select(base_ref="codex/parent")["selected"]["tests"])

    def test_cli_emits_json_and_boolean_job_outputs(self):
        self.write("README.md", "changed\n")
        self.commit()
        output = self.root / "outputs"
        proc = subprocess.run([sys.executable, str(SCRIPT), "--base", self.base,
                               "--head", "HEAD", "--event", "pull_request",
                               "--base-ref", "dev", "--ref", "refs/pull/1/merge"],
                              cwd=self.root, env=dict(os.environ, GITHUB_OUTPUT=str(output)),
                              capture_output=True, text=True)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        values = dict(line.split("=", 1) for line in output.read_text().splitlines())
        self.assertEqual(values["tests"], "false")
        self.assertEqual(values["packages"], "false")
        self.assertEqual(json.loads(values["scope"]), json.loads(proc.stdout))


if __name__ == "__main__":
    unittest.main()
