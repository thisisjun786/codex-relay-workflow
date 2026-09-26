"""Release workflow against the existing fake gh/git programs."""

import json
import os
import shutil
import subprocess

from .core import ROOT


def run(case, tmp_path):
    import sys
    sys.path.insert(0, str(ROOT / "scripts/ci/tests"))
    import release_steps

    remote = tmp_path / "remote.git"
    checkout = tmp_path / "checkout"
    bin_dir = tmp_path / "bin"
    state = tmp_path / "state"
    bin_dir.mkdir()
    state.mkdir()
    for name in ("ci", "tag", "release", "push"):
        defaults = {"ci": {"case": "success"}, "tag": {"case": "missing"},
                    "release": {"case": "missing"},
                    "push": {"fail_main": False, "lie_main": False}}
        (state / f"{name}.json").write_text(json.dumps({**defaults[name], **case.get("given", {}).get(name, {})}))
    for name in ("gh", "git"):
        target = bin_dir / name
        shutil.copyfile(ROOT / f"scripts/ci/tests/fake_{name}.sh", target)
        target.chmod(0o755)
    (tmp_path / "gh.log").write_text("")
    (tmp_path / "summary").write_text("")
    env = {**os.environ, "HOME": str(tmp_path), "XDG_CONFIG_HOME": str(tmp_path),
           "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
           "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.hooksPath",
           "GIT_CONFIG_VALUE_0": "/dev/null", "GIT_AUTHOR_NAME": "Release Fixture",
           "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_NAME": "Release Fixture",
           "GIT_COMMITTER_EMAIL": "fixture@example.invalid", "PATH": f"{bin_dir}:{os.environ['PATH']}",
           "GH_LOG": str(tmp_path / "gh.log"), "GH_STATE": str(state),
           "GITHUB_STEP_SUMMARY": str(tmp_path / "summary"),
           "GITHUB_REPOSITORY": "fixture/repository", "GITHUB_REF": "refs/heads/dev",
           "ACTOR": "owner", "TRIGGERING_ACTOR": "owner", "OWNER": "owner",
           "RELEASE_TAG": "v0.1.0", "RELEASE_NOTES": "Fixture notes",
           "RELEASE_TOKEN": "fixture-only", "GH_TOKEN": "fixture-read",
           "GIT_REAL": shutil.which("git")}
    def command(args, cwd):
        done = subprocess.run(args, cwd=cwd, env=env, capture_output=True, text=True, check=False)
        assert done.returncode == 0, f"{args}: {done.stderr}"
        return done.stdout.strip()
    command(["git", "init", "--bare", "--initial-branch=main", str(remote)], tmp_path)
    command(["git", "clone", str(remote), str(checkout)], tmp_path)
    command(["git", "commit", "--allow-empty", "-m", "base"], checkout)
    command(["git", "push", "origin", "main"], checkout)
    command(["git", "switch", "-c", "dev"], checkout)
    command(["git", "commit", "--allow-empty", "-m", "candidate"], checkout)
    sha = command(["git", "rev-parse", "HEAD"], checkout)
    command(["git", "push", "origin", "dev"], checkout)
    env["RELEASE_SHA"] = sha
    block = release_steps.step_block(case["run"]["job"], case["run"]["step"])
    done = subprocess.run(["bash", "-c", block["script"]], cwd=checkout,
                          env={**env, **case["run"].get("env", {})}, capture_output=True, text=True)
    return {"exit": done.returncode, "stdout": done.stdout, "stderr": done.stderr,
            "calls": (tmp_path / "gh.log").read_text().splitlines(),
            "remote_main": command(["git", "--git-dir", str(remote), "rev-parse", "main"], tmp_path)}
