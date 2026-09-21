#!/usr/bin/env python3
"""Check the parent-title helper's command surface and its coverage guard.

The fixtures own the decision table. What they cannot express is the behaviour around them:
that a malformed request is reported as a caller bug rather than a settled title, that the
readback classification reaches the exit codes a caller would branch on, and that the replay
guard actually fails when a decision has no fixture. That last one matters most, because a
coverage check nobody has seen fail is indistinguishable from one that passes everything.
"""

import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "plugins/crw/skills/crw-run/scripts/parent_title.py"
FIXTURES = SCRIPT.parent / "fixtures" / "titles"


def run(args, stdin=""):
    return subprocess.run([sys.executable, str(SCRIPT), *args], input=stdin,
                          capture_output=True, text=True, cwd=ROOT)


class ParentTitleCommands(unittest.TestCase):
    def test_replay_passes_on_the_shipped_fixtures(self):
        result = run(["replay"])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("not evidence that any title was written", result.stdout)

    def test_decide_returns_the_title_on_stdout(self):
        request = {
            "role": "parent", "binding_verified": True,
            "project_labels": ["CRW"], "family_candidates": ["CRW"],
            "observed_title": "설치형 플러그인 전환", "user_title": "none",
        }
        result = run(["decide"], json.dumps(request))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["title"], "[CRW] 설치형 플러그인 전환")

    def test_unreadable_request_exits_two(self):
        self.assertEqual(run(["decide"], "not json").returncode, 2)

    def test_invalid_decision_exits_two(self):
        request = {"role": "coordinator", "binding_verified": True}
        result = run(["decide"], json.dumps(request))
        self.assertEqual(result.returncode, 2)
        self.assertEqual(json.loads(result.stdout)["decision"], "invalid")

    def test_readback_exit_codes_separate_verified_from_the_rest(self):
        same = json.dumps({"requested_title": "[CRW] x", "observed_title": "[CRW] x"})
        self.assertEqual(run(["readback"], same).returncode, 0)
        other = json.dumps({"requested_title": "[CRW] x", "observed_title": "x"})
        self.assertEqual(run(["readback"], other).returncode, 1)
        unread = json.dumps({"requested_title": "[CRW] x", "observed_title": None})
        result = run(["readback"], unread)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(json.loads(result.stdout)["readback"], "unread")


class ReplayGuard(unittest.TestCase):
    def test_an_empty_fixture_set_fails(self):
        with tempfile.TemporaryDirectory() as empty:
            result = run(["replay", "--fixtures", empty])
            self.assertEqual(result.returncode, 1)
            self.assertIn("nothing was checked", result.stderr)

    def test_a_missing_decision_fails_the_coverage_guard(self):
        with tempfile.TemporaryDirectory() as partial:
            shutil.copy(FIXTURES / "family-crw-verbatim.json", Path(partial) / "one.json")
            result = run(["replay", "--fixtures", partial])
            self.assertEqual(result.returncode, 1)
            self.assertIn("No fixture reaches", result.stderr)
            self.assertEqual(run(["replay", "--fixtures", partial,
                                  "--allow-unreached"]).returncode, 0)

    def test_a_wrong_expectation_fails_even_when_unreached_is_allowed(self):
        with tempfile.TemporaryDirectory() as bad:
            fixture = json.loads((FIXTURES / "family-crw-verbatim.json").read_text(encoding="utf-8"))
            fixture["expected"]["title"] = "[WRONG] 설치형 플러그인 전환"
            (Path(bad) / "one.json").write_text(json.dumps(fixture, ensure_ascii=False),
                                                encoding="utf-8")
            self.assertEqual(run(["replay", "--fixtures", bad, "--allow-unreached"]).returncode, 1)

    def _without(self, directory, drop):
        """Copy the shipped fixtures into @directory, leaving out the ones @drop selects."""
        kept = 0
        for path in FIXTURES.glob("*.json"):
            fixture = json.loads(path.read_text(encoding="utf-8"))
            if drop(fixture):
                continue
            shutil.copy(path, Path(directory) / path.name)
            kept += 1
        self.assertGreater(kept, 0)

    def test_each_guard_fails_when_its_own_cases_are_removed(self):
        """A coverage guard nobody has seen fail is indistinguishable from one that passes all.

        Each case drops exactly the fixtures that reach one value and asserts replay names it.
        """
        cases = (
            (lambda f: f.get("expected", {}).get("reason") == "foreign_prefix",
             "No fixture reaches: foreign_prefix"),
            (lambda f: f.get("expected", {}).get("matched") == "bare_label",
             "No fixture reaches the branch: bare_label"),
            (lambda f: f.get("expected", {}).get("readback") == "unread",
             "No fixture reaches readback: unread"),
        )
        for drop, expected in cases:
            with self.subTest(expected=expected), tempfile.TemporaryDirectory() as partial:
                self._without(partial, drop)
                result = run(["replay", "--fixtures", partial])
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn(expected, result.stderr)


if __name__ == "__main__":
    unittest.main()
