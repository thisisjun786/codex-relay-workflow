"""Offline reporting-show through the real command line.

The projection itself is owned by omitted.py. These cases prove the command boundary
through a real subprocess: required selectors, no host flag, invalid identity exiting 4,
and an explicit absent store that stays absent while a completed diagnosis exits 0.
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRC = os.path.join(REPO, "src")


def _run(args):
    environment = dict(os.environ)
    environment["PYTHONPATH"] = SRC
    environment.pop("CODEX_SESSION_RELAY_STATE", None)
    return subprocess.run(
        [sys.executable, "-m", "codex_session_relay.cli", *args],
        capture_output=True, text=True, env=environment, timeout=60,
    )


class ReportingShowCli(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-reporting-cli-")
        self.root = Path(self.tmp)
        self.state = self.root / "absent-state"
        self.markers = self.root / "markers"
        self.workspace = self.root / "workspace"
        self.markers.mkdir()
        self.workspace.mkdir()

    def tearDown(self):
        import shutil
        shutil.rmtree(self.tmp, ignore_errors=True)

    def required(self, **overrides):
        values = {
            "--marker-root": str(self.markers),
            "--workspace": str(self.workspace),
            "--assignment": "a" * 64,
            "--session": "session-1",
            "--turn": "turn-1",
        }
        values.update(overrides)
        return [
            "--state", str(self.state),
            "reporting-show",
            "--marker-root", values["--marker-root"],
            "--workspace", values["--workspace"],
            "--assignment", values["--assignment"],
            "--session", values["--session"],
            "--turn", values["--turn"],
        ]

    def test_help_lists_the_command_and_every_required_selector(self):
        from contract.runner import FIXTURES, run_scenario
        run_scenario(FIXTURES / "cli-shape" / "test_reporting_cli__test_help_lists_the_command_and_every_required_selector.json", self.root)

    def test_a_missing_required_flag_exits_2_before_any_read(self):
        from contract.runner import FIXTURES, run_scenario
        run_scenario(FIXTURES / "cli-shape" / "test_reporting_cli__test_a_missing_required_flag_exits_2_before_any_read.json", self.root)

    def test_global_state_is_required_and_a_socket_is_refused(self):
        missing = _run([
            "reporting-show", "--marker-root", str(self.markers),
            "--workspace", str(self.workspace), "--assignment", "a" * 64,
            "--session", "session-1", "--turn", "turn-1",
        ])
        self.assertEqual(missing.returncode, 4, missing.stdout + missing.stderr)
        self.assertIn("--state", json.loads(missing.stdout)["detail"])

        socketed = _run([
            "--state", str(self.state), "--socket", str(self.root / "app.sock"),
            *self.required()[2:],
        ])
        self.assertEqual(socketed.returncode, 4, socketed.stdout + socketed.stderr)
        self.assertIn("--socket", json.loads(socketed.stdout)["detail"])
        self.assertFalse(self.state.exists())

    def test_invalid_identity_exits_4_and_leaves_the_store_absent(self):
        from contract.runner import FIXTURES, run_scenario
        run_scenario(FIXTURES / "cli-shape" / "test_reporting_cli__test_invalid_identity_exits_4_and_leaves_the_store_absent.json", self.root)

    def test_a_completed_observation_exits_0_without_creating_the_store(self):
        from contract.runner import FIXTURES, run_scenario
        run_scenario(FIXTURES / "cli-shape" / "test_reporting_cli__test_a_completed_observation_exits_0_without_creating_the_store.json", self.root)

    def test_doctor_lists_reporting_show_as_offline_and_not_host_required(self):
        from contract.runner import FIXTURES, run_scenario
        run_scenario(FIXTURES / "cli-shape" / "test_reporting_cli__test_doctor_lists_reporting_show_as_offline_and_not_host_required.json", self.root)


if __name__ == "__main__":
    unittest.main()
