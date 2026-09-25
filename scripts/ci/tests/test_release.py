import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile
import unittest

import release_steps


ROOT = Path(__file__).resolve().parents[3]
REQUIRED = ("bash", "git", "jq")


def command(args, env, cwd, check=False):
    result = subprocess.run(args, cwd=cwd, env=env, text=True, capture_output=True)
    if check and result.returncode != 0:
        raise AssertionError(f"{args}: {result.stderr or result.stdout}")
    return result


class ReleaseWorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        missing = [name for name in REQUIRED if shutil.which(name) is None]
        if missing:
            raise AssertionError(f"required commands missing: {', '.join(missing)}")
        cls.blocks = {
            "inputs": release_steps.step_block("validate", "release-inputs"),
            "source": release_steps.step_block("validate", "release-source"),
            "credentials": release_steps.step_block("validate", "release-credentials"),
            "publish": release_steps.step_block("publish", "release-publish"),
        }
        publish_job = release_steps.job_body("publish")
        if "needs: validate" not in publish_job or "inputs.dry_run == false" not in publish_job:
            raise AssertionError("publish must follow validation and a real publication request")
        if "if: inputs.dry_run == false" not in cls.blocks["credentials"]["metadata"]:
            raise AssertionError("credential check must run only for publication")

    def setUp(self):
        self.root = Path(tempfile.mkdtemp(prefix="crw-release-", dir=os.environ.get("TMPDIR")))
        self.addCleanup(shutil.rmtree, self.root, True)
        self.remote = self.root / "remote.git"
        self.checkout = self.root / "checkout"
        self.bin = self.root / "bin"
        self.log = self.root / "gh.log"
        self.summary = self.root / "summary"
        self.state = self.root / "state"
        self.bin.mkdir()
        self.state.mkdir()
        self.log.write_text("")
        self.summary.write_text("")
        (self.state / "ci.json").write_text(json.dumps({"case": "success"}))
        (self.state / "tag.json").write_text(json.dumps({"case": "missing"}))
        (self.state / "release.json").write_text(json.dumps({"case": "missing"}))
        (self.state / "push.json").write_text(json.dumps({"fail_main": False, "lie_main": False}))
        self._write_gh()
        self._write_git()
        self.env = self._base_env()
        command(["git", "init", "--bare", "--initial-branch=main", str(self.remote)], self.env, self.root, check=True)
        command(["git", "clone", str(self.remote), str(self.checkout)], self.env, self.root, check=True)
        command(["git", "commit", "--allow-empty", "-m", "base"], self.env, self.checkout, check=True)
        self.base = self.git("rev-parse", "HEAD")
        command(["git", "push", "origin", "main"], self.env, self.checkout, check=True)
        command(["git", "switch", "-c", "dev"], self.env, self.checkout, check=True)
        command(["git", "commit", "--allow-empty", "-m", "candidate"], self.env, self.checkout, check=True)
        self.candidate = self.git("rev-parse", "HEAD")
        command(["git", "push", "origin", "dev"], self.env, self.checkout, check=True)
        self.env["RELEASE_SHA"] = self.candidate


    def _write_git(self):
        real = shutil.which("git")
        if real is None:
            raise AssertionError("git is required")
        source = Path(__file__).with_name("fake_git.sh")
        target = self.bin / "git"
        target.write_text(source.read_text(encoding="utf-8"))
        target.chmod(target.stat().st_mode | stat.S_IEXEC)
        self.git_real = real

    def _base_env(self):
        env = os.environ.copy()
        env.update({
            "HOME": str(self.root),
            "XDG_CONFIG_HOME": str(self.root),
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_CONFIG_GLOBAL": "/dev/null",
            "GIT_CONFIG_COUNT": "1",
            "GIT_CONFIG_KEY_0": "core.hooksPath",
            "GIT_CONFIG_VALUE_0": "/dev/null",
            "GIT_AUTHOR_NAME": "Release Fixture",
            "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
            "GIT_COMMITTER_NAME": "Release Fixture",
            "GIT_COMMITTER_EMAIL": "fixture@example.invalid",
            "PATH": f"{self.bin}{os.pathsep}{env.get('PATH', '')}",
            "GH_LOG": str(self.log),
            "GH_STATE": str(self.state),
            "GITHUB_STEP_SUMMARY": str(self.summary),
            "GITHUB_REPOSITORY": "fixture/repository",
            "GITHUB_REF": "refs/heads/dev",
            "ACTOR": "owner",
            "TRIGGERING_ACTOR": "owner",
            "OWNER": "owner",
            "RELEASE_SHA": "",
            "RELEASE_TAG": "v0.1.0",
            "RELEASE_NOTES": "Fixture notes",
            "RELEASE_TOKEN": "fixture-only",
            "GH_TOKEN": "fixture-read",
            "GIT_REAL": self.git_real,
        })
        return env

    def git(self, *args, cwd=None):
        result = command(["git", *args], self.env, cwd or self.checkout, check=True)
        return result.stdout.strip()

    def remote_sha(self, ref):
        return command(["git", "--git-dir", str(self.remote), "rev-parse", ref], self.env, self.root, check=True).stdout.strip()

    def run_block(self, name, **overrides):
        env = self.env.copy()
        env.update(overrides)
        return command(["bash", "-c", self.blocks[name]["script"]], env, self.checkout)

    def set_case(self, kind, **values):
        path = self.state / f"{kind}.json"
        current = json.loads(path.read_text())
        current.update(values)
        path.write_text(json.dumps(current))

    def assert_pass(self, result, label):
        self.assertEqual(result.returncode, 0, f"{label}: {result.stderr or result.stdout}")

    def assert_refuse(self, result, label):
        self.assertNotEqual(result.returncode, 0, f"{label}: unexpectedly succeeded")

    def _write_gh(self):
        source = Path(__file__).with_name("fake_gh.sh")
        target = self.bin / "gh"
        target.write_text(source.read_text(encoding="utf-8"))
        target.chmod(target.stat().st_mode | stat.S_IEXEC)

    def test_owner_dispatch_inputs(self):
        self.assert_pass(self.run_block("inputs"), "owner dispatch")
        self.assert_pass(self.run_block("inputs", RELEASE_TAG="v0.1.0-rc.1"), "prerelease tag")
        refused = {
            "caller": {"ACTOR": "intruder"},
            "rerun": {"TRIGGERING_ACTOR": "intruder"},
            "branch": {"GITHUB_REF": "refs/heads/main"},
            "sha": {"RELEASE_SHA": "1234567"},
            "uppercase": {"RELEASE_SHA": self.candidate.upper()},
            "tag": {"RELEASE_TAG": "v0.1.0; false"},
            "notes": {"RELEASE_NOTES": "   "},
        }
        for label, overrides in refused.items():
            with self.subTest(label=label):
                self.assert_refuse(self.run_block("inputs", **overrides), label)

    def test_source_requires_latest_exact_push_ci_and_remote_tag(self):
        self.assert_pass(self.run_block("source"), "valid source")
        self.set_case("ci", older=True, conclusion="failure", run_number=3)
        self.assert_refuse(self.run_block("source"), "older success does not hide latest failure")
        cases = (
            ("missing", {}),
            ("error", {}),
            ("running", {"status": "in_progress", "conclusion": ""}),
            ("failed", {"conclusion": "failure"}),
            ("cancelled", {"conclusion": "cancelled"}),
            ("wrong-sha", {"sha": "0" * 40}),
            ("pr-only", {"event": "pull_request"}),
            ("manual", {"event": "workflow_dispatch"}),
            ("wrong-branch", {"branch": "main"}),
            ("latest-cancelled", {}),
            ("latest-attempt-failed", {}),
            ("stale-attempt", {}),
            ("paged-pending", {}),
            ("bad-page", {}),
        )
        for label, values in cases:
            with self.subTest(label=label):
                self.set_case("ci", case="success", older=False, conclusion="success",
                              status="completed", event="push", branch="dev", sha="", attempt=1)
                self.set_case("ci", case=label, **values)
                self.assert_refuse(self.run_block("source"), label)
        self.set_case("ci", case="paged-success")
        self.assert_pass(self.run_block("source"), "paginated successes still select the newest")
        self.set_case("ci", case="success")
        self.set_case("tag", case="commit", object_sha=self.base, object_type="commit")
        self.assert_refuse(self.run_block("source"), "lightweight tag points elsewhere")
        self.set_case("tag", case="annotated", object_sha="a" * 40, object_type="tag",
                      peeled_sha=self.base, peeled_type="commit")
        self.assert_refuse(self.run_block("source"), "annotated tag peels to another commit")
        self.set_case("tag", peeled_sha=self.candidate)
        self.assert_pass(self.run_block("source"), "annotated tag peels to selected commit")
        self.set_case("tag", case="error")
        self.assert_refuse(self.run_block("source"), "tag api error")
        self.set_case("tag", case="annotated", peel="error")
        self.assert_refuse(self.run_block("source"), "annotated peel error")

    def test_dry_run_reads_without_publication_and_missing_token_refuses_writes(self):
        self.assert_pass(self.run_block("inputs"), "dry-run inputs")
        self.assert_pass(self.run_block("source"), "dry-run source")
        self.assertEqual(self.remote_sha("main"), self.base)
        missing = command(["git", "--git-dir", str(self.remote), "show-ref", "--verify", "--quiet", "refs/tags/v0.1.0"], self.env, self.root)
        self.assertNotEqual(missing.returncode, 0)
        self.assertEqual(self.log.read_text(encoding="utf-8").count("release create"), 0)
        self.assert_refuse(self.run_block("credentials", RELEASE_TOKEN=""), "publication without token")
        self.assert_refuse(self.run_block("publish", GH_TOKEN=""), "publish without token")
        self.assertEqual(self.remote_sha("main"), self.base)
        self.assertFalse((self.state / "created.txt").exists())

    def test_publication_creates_source_release_then_fast_forwards_main(self):
        self.set_case("release", case="error")
        self.assert_refuse(self.run_block("publish"), "release lookup error")
        self.assertEqual(self.remote_sha("main"), self.base)
        self.assertFalse((self.state / "created.txt").exists())
        self.set_case("release", case="missing")
        self.assert_pass(self.run_block("publish"), "publish")
        self.assertEqual(self.remote_sha("main"), self.candidate)
        self.assertEqual(self.remote_sha("refs/tags/v0.1.0"), self.candidate)
        created = (self.state / "created.txt").read_text(encoding="utf-8")
        self.assertIn("--verify-tag", created)
        self.assertIn(self.candidate, created)
        self.assertNotIn("--prerelease", created)
        self.set_case("tag", case="commit", object_sha=self.candidate, object_type="commit")
        self.set_case("release", case="existing", draft=False, target=self.candidate)
        before = self.log.read_text(encoding="utf-8")
        self.assert_pass(self.run_block("publish"), "same sha recovery")
        after = self.log.read_text(encoding="utf-8")[len(before):]
        self.assertNotIn("release create", after)
        self.assertEqual(self.remote_sha("main"), self.candidate)

    def test_publication_refuses_conflicts_and_reports_partial_main_failure(self):
        self.set_case("tag", case="annotated", object_sha="b" * 40, object_type="tag",
                      peeled_sha=self.base, peeled_type="commit")
        self.assert_refuse(self.run_block("publish"), "remote annotated conflict")
        self.assertEqual(self.remote_sha("main"), self.base)
        self.assertFalse((self.state / "created.txt").exists())
        self.set_case("tag", case="missing")
        self.git("tag", "v0.9.0", self.base)
        self.assert_refuse(self.run_block("publish", RELEASE_TAG="v0.9.0"), "local tag points elsewhere")
        missing = command(["git", "--git-dir", str(self.remote), "rev-parse", "--verify", "refs/tags/v0.9.0"], self.env, self.root)
        self.assertNotEqual(missing.returncode, 0)
        self.assertEqual(self.git("rev-parse", "refs/tags/v0.9.0^{commit}"), self.base)
        self.assertFalse((self.state / "created.txt").exists())
        self.set_case("release", case="existing", draft=True, target=self.candidate)
        self.assert_refuse(self.run_block("publish"), "draft release")
        self.set_case("release", case="existing", draft=False, target=self.base)
        self.assert_refuse(self.run_block("publish"), "release target mismatch")
        self.set_case("release", case="missing")
        self.set_case("push", fail_main=True)
        self.assert_refuse(self.run_block("publish", RELEASE_TAG="v0.1.1"), "main update failure")
        self.assertEqual(self.remote_sha("refs/tags/v0.1.1"), self.candidate)
        self.assertEqual(self.remote_sha("main"), self.base)
        self.assertTrue((self.state / "created.txt").exists())
        self.set_case("push", fail_main=False, lie_main=True)
        self.set_case("tag", case="commit", object_sha=self.candidate, object_type="commit")
        self.set_case("release", case="existing", draft=False, target=self.candidate)
        self.assert_refuse(self.run_block("publish", RELEASE_TAG="v0.1.1"), "main readback mismatch")

    def test_old_and_divergent_sources_do_not_advance_main(self):
        self.git("commit", "--allow-empty", "-m", "newer main")
        advanced = self.git("rev-parse", "HEAD")
        self.git("push", "origin", "HEAD:main")
        self.assert_refuse(self.run_block("source"), "source behind main")
        self.assert_refuse(self.run_block("publish", RELEASE_TAG="v0.4.0"), "publish behind main")
        self.assertEqual(self.remote_sha("main"), advanced)
        self.git("reset", "--hard", "origin/dev")
        restored = self.git("rev-parse", "HEAD")
        self.git("push", "--force-with-lease", "origin", "HEAD:main")
        self.git("switch", "-c", "outside")
        command(["git", "commit", "--allow-empty", "-m", "outside"], self.env, self.checkout, check=True)
        outside = self.git("rev-parse", "HEAD")
        self.assert_refuse(self.run_block("source", RELEASE_SHA=outside), "outside dev")
        self.assert_refuse(self.run_block("publish", RELEASE_SHA=outside, RELEASE_TAG="v0.5.0"), "publish outside")
        self.assertEqual(self.remote_sha("main"), restored)
        self.assertFalse((self.state / "created.txt").exists())


if __name__ == "__main__":
    unittest.main()
