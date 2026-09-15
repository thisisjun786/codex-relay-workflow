import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts/install.py"
SOURCES = sorted(p for p in (ROOT / "skills").iterdir() if (p / "SKILL.md").is_file())


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


if __name__ == "__main__":
    unittest.main()
