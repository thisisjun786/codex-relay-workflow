import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "gate.py"
WORKFLOW = Path(__file__).resolve().parents[3] / ".github" / "workflows" / "ci.yml"
spec = importlib.util.spec_from_file_location("gate", SCRIPT)
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)

GATES = {"dev-gate"}


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
    def env(self, kind="full", event="pull_request", base="dev"):
        paths = {"full": ["scripts/ci/gate.py"], "docs": ["README.md"],
                 "skill": ["plugins/crw/skills/crw-run/SKILL.md"]}[kind]
        selected = {"tests": kind != "docs", "packages": kind == "full"}
        if event == "workflow_dispatch":
            selected = {"tests": True, "packages": True}
        ref = "refs/pull/1/merge" if event == "pull_request" else "refs/heads/dev"
        selection = dict(version=1, event=event, base="a" * 40, head="b" * 40,
                         base_ref=base, ref=ref, changed=paths, unknown=[], unsafe=[],
                         reason="dispatch" if event == "workflow_dispatch" else "paths",
                         selected=selected)
        needs = {name: {"result": "skipped" if selected.get(name) is False else "success"}
                 for name in gate.JOBS}
        needs["selection"]["outputs"] = {"scope": json.dumps(selection)}
        return dict(NEEDS_JSON=json.dumps(needs), EVENT_NAME=event, BASE_REF=base,
                    REF=ref, HEAD_SHA="b" * 40)

    def mutate(self, env, change):
        needs = json.loads(env["NEEDS_JSON"])
        change(needs)
        env["NEEDS_JSON"] = json.dumps(needs)
        return env

    def test_selected_checks_for_each_scope_and_event(self):
        for kind in ("docs", "skill", "full"):
            for event in ("pull_request", "push", "workflow_dispatch"):
                with self.subTest(kind=kind, event=event):
                    gate.check(self.env(kind, event))
        gate.check(self.env(base="codex/parent"))

    def test_main_pr_refused_even_when_all_jobs_succeed(self):
        with self.assertRaises(ValueError):
            gate.check(self.env(base="main"))

    def test_every_unsuccessful_selected_result_blocks(self):
        for job in gate.JOBS:
            for state in ("failure", "cancelled", "skipped", "pending", "neutral", "", None, True):
                with self.subTest(job=job, state=state), self.assertRaises(ValueError):
                    gate.check(self.mutate(self.env(), lambda n: n[job].update(result=state)))

    def test_unselected_jobs_must_be_skipped_not_failed_or_run(self):
        for job in ("tests", "packages"):
            for state in ("success", "failure", "cancelled", "neutral", None):
                with self.subTest(job=job, state=state), self.assertRaises(ValueError):
                    gate.check(self.mutate(self.env("docs"), lambda n: n[job].update(result=state)))

    def test_missing_unexpected_and_malformed_results(self):
        for raw in ("", "{", "[]", "null", "true", "{}"):
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                gate.check(dict(self.env(), NEEDS_JSON=raw))
        for job in gate.JOBS:
            with self.subTest(job=job), self.assertRaises(ValueError):
                gate.check(self.mutate(self.env(), lambda n: n.pop(job)))
        with self.assertRaises(ValueError):
            gate.check(self.mutate(self.env(), lambda n: n.update(extra={"result": "success"})))

    def test_selection_cannot_claim_false_exemption(self):
        for field, value in (("selected", {"tests": False, "packages": False}),
                             ("unknown", ["new/component.py"]), ("head", "old"),
                             ("version", True), ("selected", {"tests": "true", "packages": True})):
            def alter(needs):
                data = json.loads(needs["selection"]["outputs"]["scope"])
                data[field] = value
                needs["selection"]["outputs"]["scope"] = json.dumps(data)
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                gate.check(self.mutate(self.env(), alter))

    def test_context_is_bound_to_actual_candidate(self):
        for key in ("EVENT_NAME", "BASE_REF", "REF", "HEAD_SHA"):
            env = self.env()
            env[key] = "different"
            with self.subTest(key=key), self.assertRaises(ValueError):
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

    def test_gate_waits_for_every_producer(self):
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

    def test_expensive_jobs_follow_selection_and_keep_runtime_coverage(self):
        body = workflow_jobs()["packages"]
        self.assertIn("scripts/ci/packages.py", body)
        self.assertIn("if: needs.selection.outputs.packages == 'true'", body)
        self.assertIn("if: needs.selection.outputs.tests == 'true'", workflow_jobs()["tests"])
        self.assertIn("scripts/ci/contracts.py", workflow_jobs()["validate"])
        self.assertNotIn("scripts/ci/contracts.py", workflow_jobs()["tests"])
        for version in ("'3.11'", "'3.13'"):
            self.assertIn(version, body)

    def test_downloaded_tooling_is_pinned_by_commit_and_checksum(self):
        body = workflow_jobs()["packages"]
        self.assertIsNotNone(re.search(r"uses: astral-sh/setup-uv@[0-9a-f]{40} #", body))
        self.assertIsNotNone(re.search(r"checksum: '[0-9a-f]{64}'", body))
        self.assertIsNotNone(re.search(r"version: '\d+\.\d+\.\d+'", body))

    def test_go_product_always_runs_and_builds_every_static_target(self):
        body = workflow_jobs()["go-product"]
        self.assertNotRegex(body, r"(?m)^    if:")
        self.assertIn("go-version-file: go.mod", body)
        self.assertIn("run: make lint", body)
        self.assertIn("run: make test-part TEST_PART=", body)
        self.assertIn("CGO_ENABLED=0", body)
        for target in ("linux/amd64", "linux/arm64", "darwin/arm64"):
            self.assertIn(target, body)
        for artifact in ("crw_linux_amd64", "crw_linux_arm64", "crw_darwin_arm64", "SHA256SUMS"):
            self.assertRegex(body, rf"(?m)^          name: {artifact}$")
        # The isolated-home integration test installs the binary the dist leg just built.
        self.assertIn('CRW_TEST_BINARY="$PWD/dist/crw_linux_amd64/crw" go test -trimpath'
                      ' -tags integration -count=1 ./internal/runtime/integration/...', body)
        for action in re.findall(r"uses: (\S+)", body):
            self.assertRegex(action, r"@[0-9a-f]{40}$")

    def matrix(self, job, key):
        found = re.search(rf"(?m)^        {key}: \[([^\]]+)\]$", workflow_jobs()[job])
        self.assertIsNotNone(found, f"{job} has no {key} matrix")
        return [part.strip().strip("'") for part in found[1].split(",")]

    def test_go_product_legs_are_lint_dist_and_every_makefile_test_part(self):
        """A Makefile part without a leg would never run in CI; a leg without a part fails."""
        makefile = (WORKFLOW.parents[2] / "Makefile").read_text(encoding="utf-8")
        numbered = re.findall(r"(?m)^TEST_PART_(\d+) :=", makefile)
        filtered = re.search(r"\$\(filter \$\(TEST_PART\),([^)]*)\)", makefile)
        self.assertIsNotNone(filtered)
        self.assertEqual(filtered[1].split(), numbered)
        legs = self.matrix("go-product", "part")
        self.assertEqual(len(legs), len(set(legs)))
        self.assertEqual(sorted(legs), sorted(["lint", "dist", "test-rest"]
                                              + [f"test-{n}" for n in numbered]))

    def test_package_shards_are_every_slice_of_one_total(self):
        shards = self.matrix("packages", "shard")
        total = len(shards)
        self.assertEqual(shards, [f"{index}/{total}" for index in range(1, total + 1)])
        self.assertIn('packages.py --shard "$SHARD"', workflow_jobs()["packages"])

    def installer_step(self):
        """The tests job's run script, unindented as the runner receives it, and its HEAVY list."""
        lines = workflow_jobs()["tests"].splitlines()
        self.assertEqual(lines.count("        run: |"), 1, "the tests job has one script step")
        script = []
        for line in lines[lines.index("        run: |") + 1:]:
            if line.strip() and not line.startswith(" " * 10):
                break
            script.append(line[10:])
        heavy = re.search(r"(?m)^          HEAVY: (.+)$", workflow_jobs()["tests"])
        self.assertIsNotNone(heavy, "the tests job names no HEAVY modules")
        return "\n".join(script) + "\n", heavy[1]

    def test_installer_test_legs_run_every_module_exactly_once(self):
        """Each leg runs the step's own shell over a mirror of scripts/ci/tests plus a module no
        list names yet, with python3 replaced by a recorder: together the legs run every module
        discovery would load, each once, and the unnamed one lands in `rest`."""
        self.assertEqual(self.matrix("tests", "part"), ["heavy", "rest"])
        script, heavy = self.installer_step()
        tests = WORKFLOW.parents[2] / "scripts" / "ci" / "tests"
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            mirror = root / "scripts" / "ci" / "tests"
            mirror.mkdir(parents=True)
            for entry in tests.iterdir():
                if entry.is_file():
                    (mirror / entry.name).touch()
            (mirror / "test_zz_added_later.py").touch()
            listed = sorted(entry.stem for entry in mirror.glob("test*.py"))
            recorder = root / "bin" / "python3"
            recorder.parent.mkdir()
            recorder.write_text('#!/bin/sh\nprintf \'%s\\n\' "$PYTHONPATH" "$@"\n', encoding="utf-8")
            recorder.chmod(0o755)
            ran = {}
            for part in ("heavy", "rest"):
                env = dict(os.environ, PART=part, HEAVY=heavy,
                           PATH=f"{recorder.parent}{os.pathsep}{os.environ.get('PATH', '')}")
                env.pop("BASH_ENV", None)
                # GitHub runs a `run:` block without a `shell:` as `bash -e {0}`.
                done = subprocess.run(["bash", "-e", "-c", script], cwd=root, env=env,
                                      capture_output=True, text=True, timeout=60)
                self.assertEqual(done.returncode, 0, f"{part}: {done.stderr}")
                words = done.stdout.split()
                self.assertEqual(words[:4], ["scripts/ci/tests", "-m", "unittest", "-v"], part)
                ran[part] = words[4:]
                self.assertTrue(ran[part], f"the {part} leg runs no module")
        self.assertEqual(sorted(ran["heavy"] + ran["rest"]), listed)
        self.assertIn("test_zz_added_later", ran["rest"])


if __name__ == "__main__":
    unittest.main()
