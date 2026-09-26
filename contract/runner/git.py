"""Temporary real git repositories and their observable state."""

import json
import os
import subprocess
import tempfile
from pathlib import Path
from unittest.mock import patch

import anyio


def run(case, tmp_path):
    with tempfile.TemporaryDirectory(prefix="crw-git-", dir="/dev/shm") as raw:
        return _run(case, Path(raw))


def _run(case, tmp_path):
    repo = tmp_path / "source"
    repo.mkdir()
    env = {**os.environ, **{key: str(tmp_path / "home" / key) for key in
           ("HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "CODEX_HOME")},
           "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
           "GIT_AUTHOR_NAME": "Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
           "GIT_COMMITTER_NAME": "Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid"}
    (tmp_path / "home").mkdir()
    def git(*args):
        done = subprocess.run(["git", "-C", str(repo), *args], capture_output=True, text=True,
                              env=env, check=False)
        assert done.returncode == 0, done.stderr
        return done.stdout.strip()
    git("init", "--initial-branch=main")
    for name, contents in case.get("given", {}).get("files", {}).items():
        path = repo / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(contents)
    git("add", ".")
    git("commit", "--allow-empty", "-m", "base")
    base = git("rev-parse", "HEAD")
    for name, contents in case.get("given", {}).get("staged", {}).items():
        (repo / name).write_text(contents)
        git("add", name)
    for name, contents in case.get("given", {}).get("dirty", {}).items():
        (repo / name).write_text(contents)
    if case["run"].get("kind") == "git" and case["run"].get("bridge"):
        with patch.dict(os.environ, env):
            return anyio.run(_bridge_run, case, tmp_path, repo, base, git)
    inspection = None
    for step in case["run"].get("steps", []):
        match step["kind"]:
            case "branch": git("branch", step["name"])
            case "commit":
                (repo / step["file"]).write_text(step["content"])
                git("add", step["file"])
                git("commit", "-m", step["message"])
            case "worktree":
                import asyncio

                from codex_thread_bridge.worktrees import Worktree
                async def create(path=step["path"]):
                    worktree = await Worktree.validate(str(repo), base, str(tmp_path / path))
                    worktree.reserve()
                    await worktree.create()
                    await worktree.checkout()
                    return await worktree.inspect()
                inspection = asyncio.run(create())
            case _: raise AssertionError(f"unknown git step {step['kind']}")
    checkout = tmp_path / next((step["path"] for step in case["run"].get("steps", []) if step["kind"] == "worktree"), "isolated")
    return {"exit": 0, "base": base, "inspection": inspection,
            "checkout": {name: (checkout / name).read_text() if (checkout / name).is_file() else None
                         for name in case["expect"].get("checkout", [])},
            "source": {name: (repo / name).read_text() if (repo / name).is_file() else None
                       for name in case["expect"].get("source", [])},
            "revision": git("rev-parse", "HEAD"),
            "status": git("status", "--porcelain=v1"),
            "worktrees": git("worktree", "list", "--porcelain"),
            "branches": git("branch", "--format=%(refname:short)").splitlines()}


async def _bridge_run(case, root, repo, base, git):
    """Drive the real bridge and inspect only case-local Git, host, and ledger effects."""
    from codex_thread_bridge.bridge import Bridge
    from codex_thread_bridge.ledger import Ledger
    from codex_thread_bridge.rpc import AppServer, TransportError

    from .appserver import fake_host

    given = case.get("given", {})
    for name, value in given.get("git_config", {}).items():
        git("config", "--local", name.replace("${SOURCE}", str(repo)),
            value.replace("${ROOT}", str(root)))
    if given.get("hostile_git"):
        marker = root / "unexpected-effect"
        config = root / "conditional-config"
        config.write_text(f'[filter "conditional"]\n    smudge = "touch {marker}; cat"\n')
        git("config", f"includeIf.gitdir:{repo}/.git/worktrees/.path", str(config))
        script = repo / ".git" / "hooks" / "post-checkout"
        (repo / ".gitattributes").write_text("tracked filter=example\n")
        git("add", ".gitattributes")
        git("commit", "-m", "attributes")
        base = git("rev-parse", "HEAD")
        script.write_text(f"#!/bin/sh\ntouch '{marker}'\ncat\n")
        script.chmod(0o755)
        for name in ("filter.example.smudge", "filter.example.clean", "core.fsmonitor"):
            git("config", name, str(script))
        git("config", "filter.example.required", "true")
        marker.unlink(missing_ok=True)
    args = {
        "request_id": "isolated", "source_repository": str(repo),
        "starting_revision": base, "destination": str(root / "isolated"),
        "worktree_mode": "bridge-managed-retained", "sandbox": "read-only",
        "expected_sandbox_policy": {"type": "readOnly", "networkAccess": False},
        "model": "anthropic/claude-opus-5-5", "reasoning_effort": "xhigh",
        **given.get("arguments", {}),
    }
    def launch_arguments(overrides):
        return {**args, **overrides}
    ledger_path = root / "state" / "operations.sqlite3"
    before_status = git("-c", "core.fsmonitor=false", "status", "--porcelain=v1", "--ignored")
    before_index = git("-c", "core.fsmonitor=false", "diff", "--cached", "--binary")
    if given.get("hostile_git"):
        (root / "unexpected-effect").unlink(missing_ok=True)
    receipts = []
    launch_calls = []
    launch_traces = []
    worktree_snapshots = []
    async with fake_host(given, root) as (fake, socket):
        rpc = AppServer(socket, timeout=1)
        ledger = Ledger(ledger_path)
        bridge = Bridge(rpc, ledger)
        try:
            for step in case["run"]["steps"]:
                match step["kind"]:
                    case "launch":
                        launch = launch_arguments(step.get("arguments", {}))
                        original = rpc.call
                        if failed_method := step.get("fail_before_write"):
                            async def unreachable(method, params, *, _original=original,
                                                  _failed_method=failed_method):
                                if method == _failed_method:
                                    raise TransportError("App Server is not connected")
                                return await _original(method, params)
                            rpc.call = unreachable
                        try:
                            with patch.dict(os.environ, {
                                key: str(root / value) for key, value in step.get("env", {}).items()
                            }):
                                receipts.append(await bridge.create_worktree_thread(**launch))
                        except (ValueError, TransportError) as error:
                            receipts.append({"exception": type(error).__name__, "message": str(error)})
                        finally:
                            rpc.call = original
                        launch_calls.append(len(fake.calls))
                        launch_traces.append([[method, params] for method, params in fake.calls])
                    case "concurrent":
                        results = [None] * len(step["request_ids"])
                        async def invoke(index, request_id, *, _results=results,
                                         _bridge=bridge):
                            _results[index] = await _bridge.create_worktree_thread(
                                **launch_arguments({"request_id": request_id}))
                        async with anyio.create_task_group() as group:
                            for index, request_id in enumerate(step["request_ids"]):
                                group.start_soon(invoke, index, request_id)
                        receipts.extend(results)
                    case "snapshot":
                        worktree_snapshots.append(git("worktree", "list", "--porcelain"))
                    case "restart":
                        ledger.close()
                        ledger = Ledger(ledger_path)
                        bridge = Bridge(rpc, ledger)
                    case "move":
                        (root / step["from"]).rename(root / step["to"])
                    case "remove":
                        path = root / step["path"]
                        if path.is_dir():
                            import shutil
                            shutil.rmtree(path)
                        else:
                            path.unlink()
                    case "host":
                        setattr(fake, step["field"], step["value"])
                    case "seed_legacy":
                        legacy = {**args, **step.get("arguments", {})}
                        params = {key: legacy.get(key) for key in (
                            "source_repository", "starting_revision", "destination", "worktree_mode",
                            "sandbox", "expected_sandbox_policy", "prompt", "title", "model",
                            "reasoning_effort", "app_server_project_id")}
                        fingerprint = ledger._fingerprint(str(legacy["request_id"]),
                                                         "create_worktree_thread", params)
                        ledger.db.execute("INSERT INTO operations VALUES (?, ?, ?)", (
                            legacy["request_id"], fingerprint, json.dumps(step["receipt"])))
                        ledger.db.commit()
                    case unreachable:
                        raise AssertionError(f"unknown bridge step {unreachable!r}")
            ledger_rows = {row[0]: json.loads(row[1]) for row in ledger.db.execute(
                "SELECT request_id, receipt FROM operations")}
            thread_views = {}
            for receipt in receipts:
                if "threadId" in receipt and receipt["threadId"] in fake.threads:
                    thread_views[receipt["threadId"]] = {
                        "read": await bridge.read_thread(receipt["threadId"]),
                        "goal": await bridge.get_goal(receipt["threadId"]),
                    }
        finally:
            await rpc.close()
            ledger.close()
    checkout = root / "isolated"
    git_repo = root / "moved-source" if (root / "moved-source").exists() else repo
    def final_git(*arguments):
        done = subprocess.run(["git", "-C", str(git_repo), "-c", "core.fsmonitor=false", *arguments],
                              capture_output=True, text=True, env=os.environ, check=False)
        assert done.returncode == 0, done.stderr
        return done.stdout.strip()
    observed_paths = {name: (root / name).exists() for name in case["expect"].get("paths", [])}
    final_status = final_git("status", "--porcelain=v1", "--ignored")
    final_worktrees = final_git("worktree", "list", "--porcelain")
    final_revision = final_git("rev-parse", "HEAD")
    final_index = final_git("diff", "--cached", "--binary")
    return {
        "exit": 0, "receipts": receipts, "ledger": ledger_rows, "thread_views": thread_views,
        "launch_calls": launch_calls, "launch_traces": launch_traces,
        "worktree_snapshots": worktree_snapshots,
        "before_status": before_status, "before_index": before_index,
        "after_index": final_index,
        "threads": fake.threads, "calls": [[method, params] for method, params in fake.calls],
        "source": {name: (repo / name).read_text() if (repo / name).is_file() else None
                   for name in case["expect"].get("source", [])},
        "checkout": {name: (checkout / name).read_text() if (checkout / name).is_file() else None
                     for name in case["expect"].get("checkout", [])},
        "paths": observed_paths,
        "status": final_status, "worktrees": final_worktrees,
        "destination": args["destination"], "revision": final_revision, "base": base,
    }
