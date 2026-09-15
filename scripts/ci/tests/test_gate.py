import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "gate.py"
WORKFLOW = Path(__file__).resolve().parents[3] / ".github" / "workflows" / "ci.yml"
spec = importlib.util.spec_from_file_location("gate", SCRIPT)
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)

GATES = {"dev-gate", "release-gate"}


def workflow_jobs():
    """Job name -> body, read from the two-space headers under the workflow's jobs key.

    Deliberately small: it reads this repository's own workflow, whose shape is
    fixed by the file next to it, and it is not a general YAML parser.
    """
    jobs = {}
    current = None
    inside = False
    for line in WORKFLOW.read_text(encoding="utf-8").splitlines():
        if line and not line.startswith(" "):
            inside = line.startswith("jobs:")
            current = None
            continue
        if not inside:
            continue
        header = re.fullmatch(r"  ([a-z][a-z0-9-]*):", line)
        if header:
            current = header[1]
            jobs[current] = []
        elif current is not None:
            jobs[current].append(line)
    return {name: "\n".join(body) for name, body in jobs.items()}


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


class WorkflowTests(unittest.TestCase):
    """The aggregator only means something while it names the jobs that actually run."""

    def test_the_required_set_is_the_set_of_real_jobs(self):
        self.assertEqual(set(workflow_jobs()) - GATES, gate.JOBS)

    def test_the_package_check_is_required(self):
        self.assertIn("packages", gate.JOBS)
        self.assertIn("packages", workflow_jobs())

    def test_both_gates_wait_for_every_required_job(self):
        jobs = workflow_jobs()
        for name in sorted(GATES):
            with self.subTest(gate=name):
                declared = re.search(r"^    needs: \[([^\]]+)\]$", jobs[name], re.MULTILINE)
                self.assertIsNotNone(declared, f"{name} must declare its prerequisites")
                self.assertEqual({part.strip() for part in declared[1].split(",")}, gate.JOBS)

    def test_no_job_may_opt_out_of_its_own_result(self):
        for name, body in workflow_jobs().items():
            with self.subTest(job=name):
                self.assertNotIn("continue-on-error", body)

    def test_the_packages_job_runs_the_check_unconditionally(self):
        body = workflow_jobs()["packages"]
        self.assertIn("scripts/ci/packages.py", body)
        self.assertIsNone(re.search(r"^    if:", body, re.MULTILINE))
        for version in ("'3.11'", "'3.13'"):
            self.assertIn(version, body)

    def test_downloaded_tooling_is_pinned_by_commit_and_checksum(self):
        body = workflow_jobs()["packages"]
        self.assertIsNotNone(re.search(r"uses: astral-sh/setup-uv@[0-9a-f]{40} #", body))
        self.assertIsNotNone(re.search(r"checksum: '[0-9a-f]{64}'", body))
        self.assertIsNotNone(re.search(r"version: '\d+\.\d+\.\d+'", body))


if __name__ == "__main__":
    unittest.main()
