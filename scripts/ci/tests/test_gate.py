import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "gate.py"
spec = importlib.util.spec_from_file_location("gate", SCRIPT)
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


class GateTests(unittest.TestCase):
    def env(self):
        return dict(NEEDS_JSON=json.dumps({n: {"result": "success"} for n in gate.JOBS}),
                    EXPECTED_RELEASE="false", BASE_REF="dev", HEAD_REF="codex/task",
                    BASE_REPO="owner/repo", HEAD_REPO="owner/repo")

    def test_development_and_manual_chain(self):
        for base in ("dev", "codex/parent"):
            env = self.env()
            env.update(BASE_REF=base, HEAD_REPO="contributor/fork")
            gate.check(env)

    def test_only_same_repository_dev_can_promote(self):
        for branch, repo, accepted in (("dev", "owner/repo", True),
                                       ("codex/task", "owner/repo", False),
                                       ("dev", "attacker/repo", False)):
            with self.subTest(branch=branch, repo=repo):
                env = self.env()
                env.update(BASE_REF="main", HEAD_REF=branch, HEAD_REPO=repo,
                           EXPECTED_RELEASE="true")
                if accepted:
                    gate.check(env)
                else:
                    with self.assertRaises(ValueError):
                        gate.check(env)

    def test_every_unsuccessful_result_blocks(self):
        for job in gate.JOBS:
            for state in ("failure", "cancelled", "skipped", "pending", "", None, True):
                with self.subTest(job=job, state=state):
                    env = self.env()
                    needs = json.loads(env["NEEDS_JSON"])
                    needs[job]["result"] = state
                    env["NEEDS_JSON"] = json.dumps(needs)
                    with self.assertRaises(ValueError):
                        gate.check(env)

    def test_missing_unexpected_and_malformed_results(self):
        valid = json.loads(self.env()["NEEDS_JSON"])
        invalid = ["", "{", "[]", "null", "true", "{}"]
        for job in gate.JOBS:
            invalid.append(json.dumps({k: v for k, v in valid.items() if k != job}))
            invalid.append(json.dumps(dict(valid, **{job: None})))
        invalid.append(json.dumps(dict(valid, surprise={"result": "success"})))
        for needs in invalid:
            with self.subTest(needs=needs), self.assertRaises(ValueError):
                env = self.env()
                env["NEEDS_JSON"] = needs
                gate.check(env)

    def test_context_and_gate_mismatch(self):
        for key in self.env():
            env = self.env()
            del env[key]
            with self.subTest(missing=key), self.assertRaises((KeyError, ValueError)):
                gate.check(env)
        for changes in ({"BASE_REF": "main"}, {"EXPECTED_RELEASE": "true"},
                        {"EXPECTED_RELEASE": "yes"}, {"HEAD_REPO": " "}):
            env = self.env()
            env.update(changes)
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                gate.check(env)

    def test_cli_missing_input_fails(self):
        env = dict(os.environ)
        for key in self.env():
            env.pop(key, None)
        result = subprocess.run([sys.executable, str(SCRIPT)], env=env,
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 1)
        self.assertIn("Gate failed", result.stderr)


if __name__ == "__main__":
    unittest.main()
