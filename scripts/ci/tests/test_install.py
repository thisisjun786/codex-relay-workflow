import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts/install.py"
NAMES = ("crw-check", "crw-define", "crw-focus", "crw-logic", "crw-loop", "crw-next", "crw-plan", "crw-run")
SOURCES = [ROOT / "skills" / name for name in NAMES]


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.dest = self.root / "nested/skills"
        self.env = dict(os.environ, CODEX_HOME=str(self.root / "isolated-codex"))

    def run_cli(self, mode, explicit=True):
        args = [sys.executable, str(SCRIPT), mode]
        if explicit:
            args += ["--dest", str(self.dest)]
        return subprocess.run(args, env=self.env, capture_output=True, text=True)

    def assert_links(self, destination):
        self.assertEqual({p.name for p in destination.iterdir()}, {p.name for p in SOURCES})
        for source in SOURCES:
            link = destination / source.name
            self.assertTrue((source / "SKILL.md").is_file())
            self.assertTrue(link.is_symlink())
            self.assertEqual(link.resolve(), source.resolve())

    def test_missing_check_never_writes(self):
        self.assertEqual(self.run_cli("--check").returncode, 1)
        self.assertFalse(self.dest.parent.exists())

    def test_apply_check_and_idempotent_apply(self):
        self.assertEqual(self.run_cli("--apply").returncode, 0)
        self.assert_links(self.dest)
        before = {p.name: p.lstat().st_ino for p in self.dest.iterdir()}
        self.assertEqual(self.run_cli("--check").returncode, 0)
        self.assertEqual(self.run_cli("--apply").returncode, 0)
        self.assertEqual(before, {p.name: p.lstat().st_ino for p in self.dest.iterdir()})

    def test_foreign_paths_preserved_and_preflight_is_all_or_nothing(self):
        for kind in ("file", "directory", "foreign-link", "dangling-link"):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory(dir=self.root) as folder:
                self.dest = Path(folder) / "skills"
                self.dest.mkdir()
                conflict = self.dest / SOURCES[-1].name
                target = Path(folder) / "foreign"
                if kind == "file":
                    conflict.write_text("preserve me")
                elif kind == "directory":
                    conflict.mkdir()
                    (conflict / "owned").write_text("preserve me")
                else:
                    if kind == "foreign-link":
                        target.mkdir()
                    conflict.symlink_to(target, target_is_directory=True)
                before = conflict.lstat().st_ino
                for mode in ("--check", "--apply"):
                    result = self.run_cli(mode)
                    self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                    self.assertIn("CONFLICT", result.stderr)
                    self.assertEqual(list(self.dest.iterdir()), [conflict])
                    self.assertEqual(conflict.lstat().st_ino, before)
                if kind == "file":
                    self.assertEqual(conflict.read_text(), "preserve me")
                elif kind == "directory":
                    self.assertEqual((conflict / "owned").read_text(), "preserve me")
                else:
                    self.assertEqual(conflict.readlink(), target)

    def test_default_uses_isolated_codex_home(self):
        self.assertEqual(self.run_cli("--apply", explicit=False).returncode, 0)
        self.assert_links(Path(self.env["CODEX_HOME"]) / "skills")
        self.assertFalse(self.dest.exists())

    def test_legacy_entries_survive_apply_and_keep_check_nonzero(self):
        for kind in ("file", "directory", "live-link", "dangling-link", "other-checkout"):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory(dir=self.root) as folder:
                self.dest = Path(folder) / "skills"
                self.dest.mkdir()
                legacy = self.dest / "linear-run"
                target = Path(folder) / "old-checkout/skills/linear-run"
                if kind == "file":
                    legacy.write_text("user edits")
                elif kind == "directory":
                    legacy.mkdir()
                    (legacy / "SKILL.md").write_text("user edits")
                else:
                    if kind == "live-link":
                        target = SOURCES[-1]
                    elif kind == "other-checkout":
                        target.mkdir(parents=True)
                        (target / "SKILL.md").write_text("foreign source")
                    legacy.symlink_to(target, target_is_directory=True)
                inode = legacy.lstat().st_ino
                check = self.run_cli("--check")
                self.assertEqual(check.returncode, 1)
                self.assertIn(f"LEGACY {legacy}", check.stderr)
                self.assertEqual(list(self.dest.iterdir()), [legacy])
                for _ in range(2):
                    result = self.run_cli("--apply")
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertIn("LEGACY", result.stderr)
                    self.assertEqual(legacy.lstat().st_ino, inode)
                    self.assertEqual(self.run_cli("--check").returncode, 1)
                    for source in SOURCES:
                        self.assertEqual((self.dest / source.name).resolve(), source)
                if kind == "file":
                    self.assertEqual(legacy.read_text(), "user edits")
                elif kind == "directory":
                    self.assertEqual((legacy / "SKILL.md").read_text(), "user edits")
                else:
                    self.assertEqual(legacy.readlink(), target)
                    if kind == "other-checkout":
                        self.assertEqual((target / "SKILL.md").read_text(), "foreign source")
                before = {p.name: p.lstat().st_ino for p in self.dest.iterdir()}
                self.assertEqual(self.run_cli("--apply").returncode, 0)
                self.assertEqual(before, {p.name: p.lstat().st_ino for p in self.dest.iterdir()})
                legacy.rename(Path(folder) / "archived-linear-run")
                self.assertEqual(self.run_cli("--check").returncode, 0)
                self.assert_links(self.dest)

    def test_all_legacy_names_report_even_with_canonical_conflict(self):
        self.dest.mkdir(parents=True)
        old_names = ("linear-check", "linear-focus", "linear-logic", "linear-next", "linear-plan", "linear-run")
        for name in old_names:
            (self.dest / name).symlink_to(self.root / "absent" / name)
        conflict = self.dest / "crw-run"
        conflict.write_text("keep this")
        for mode in ("--check", "--apply"):
            result = self.run_cli(mode)
            self.assertEqual(result.returncode, 1)
            for name in old_names:
                self.assertIn(f"LEGACY {self.dest / name}", result.stderr)
                self.assertEqual((self.dest / name).readlink(), self.root / "absent" / name)
            self.assertEqual({p.name for p in self.dest.iterdir()}, set(old_names) | {"crw-run"})
            self.assertEqual(conflict.read_text(), "keep this")


if __name__ == "__main__":
    unittest.main()
