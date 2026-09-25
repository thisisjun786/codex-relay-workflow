"""Record the Python bridge's caller-visible bytes for internal/bridge round-3 parity tests.

Run from the repository root:
  uv run --no-sync python internal/bridge/testdata/gen_round3.py > internal/bridge/testdata/python_round3.json
with HOME/XDG_*/CODEX_HOME/TMPDIR pointing at a scratch directory. Temporary paths are
replaced by <ROOT> and wall-clock "at" values are dropped.
"""
import asyncio, json, os, subprocess, sys, tempfile
from pathlib import Path

sys.path.insert(0, "packages/codex-thread-bridge/tests")
from conftest import FakeServer  # noqa: E402
from websockets.asyncio.server import unix_serve  # noqa: E402

from codex_thread_bridge.bridge import Bridge  # noqa: E402
from codex_thread_bridge.ledger import Ledger  # noqa: E402
from codex_thread_bridge.rpc import AppServer  # noqa: E402

EX = {"model": "explicit-model", "reasoning_effort": "high"}
RO = {"type": "readOnly", "networkAccess": False}
WW = {"type": "workspaceWrite", "networkAccess": False, "writableRoots": [],
      "excludeTmpdirEnvVar": False, "excludeSlashTmp": False}


def git(cwd, *args):
    return subprocess.check_output(["git", "-C", str(cwd), *args], text=True,
                                   stderr=subprocess.DEVNULL).strip()


async def error_of(coro):
    try:
        result = await coro
    except ValueError as error:
        return str(error)
    return {"status": result["status"], "error": result.get("error")}


async def main(out):
    with tempfile.TemporaryDirectory(dir=os.environ["TMPDIR"]) as tmp:
        d = Path(tmp)
        sock, fake = d / "app.sock", FakeServer()
        async with unix_serve(fake.handle, str(sock)):
            rpc = AppServer(sock, timeout=1)
            ledger = Ledger(d / "l.sqlite3")
            b = Bridge(rpc, ledger)
            cwd = d / "cwd"
            cwd.mkdir()
            created = await b.create_thread("c", str(cwd), **EX)
            tid = created["threadId"]

            fake.threads[tid]["status"] = {"type": "active"}
            r = await b.send_message_to_thread("busy", tid, "hi", dict(EX))
            out["busy"] = {"rpcError": r["rpcError"], "error": r["error"]}
            fake.threads[tid]["status"] = {"type": "idle"}

            fake.override_creation = {"model": "other"}
            r = await b.create_thread("mm", str(cwd), prompt="p", **EX)
            out["create_model_mismatch"] = {"rpcError": r["rpcError"], "error": r["error"]}
            fake.override_creation = {"sandbox": {"type": "dangerFullAccess"}}
            r = await b.create_thread("ms", str(cwd), prompt="p", **EX)
            out["create_sandbox_mismatch"] = {"rpcError": r["rpcError"], "error": r["error"]}
            fake.override_creation = {}

            fake.approval_request_on_turn = "item/commandExecution/requestApproval"
            fake.approval_policy = "on-request"
            r = await b.send_message_to_thread("ap", tid, "go",
                                               {**EX, "approval_policy": "on-request"})
            for entry in r["approvalRequests"]["thisThread"]:
                entry.pop("at")
            out["approvalRequests"] = r["approvalRequests"]
            fake.approval_request_on_turn = None
            fake.approval_policy = "never"

            caps = await b.capabilities()
            caps["socket"] = "<SOCKET>"
            out["capabilities"] = caps

            refusals = {}
            for limit in (0, 101):
                refusals[f"read_limit_{limit}"] = await error_of(b.read_thread(tid, limit=limit))
                refusals[f"list_limit_{limit}"] = await error_of(b.list_threads(limit=limit))
            for chars in (99, 20001):
                refusals[f"read_chars_{chars}"] = await error_of(
                    b.read_thread(tid, max_text_chars=chars))
            for seconds in (-0.001, 50.001):
                refusals[f"wait_{seconds}"] = await error_of(b.wait_thread(tid, "turn-1", seconds))
            refusals["send_roots_relative"] = await error_of(b.send_message_to_thread(
                "rr", tid, "hi", {**EX, "runtime_workspace_roots": ["relative"]}))
            refusals["create_roots_relative"] = await error_of(b.create_thread(
                "cr", str(cwd), runtime_workspace_roots=["relative"], **EX))
            refusals["send_sandbox_bogus"] = await error_of(b.send_message_to_thread(
                "sb", tid, "hi", {**EX, "sandbox": "bogus"}))
            refusals["send_cwd_blank"] = await error_of(b.send_message_to_thread(
                "sc", tid, "hi", {**EX, "cwd": " "}))
            refusals["send_policy_without_type"] = await error_of(b.send_message_to_thread(
                "sp", tid, "hi", {**EX, "expected_sandbox_policy": {"no_type": 1}}))
            refusals["create_policy_relative_root"] = await error_of(b.create_thread(
                "cp", str(cwd), sandbox="workspace-write",
                expected_sandbox_policy={**WW, "writableRoots": ["rel"]}, **EX))

            source = d / "source"
            source.mkdir()
            git(source, "init", "-q")
            git(source, "config", "user.email", "t@e.invalid")
            git(source, "config", "user.name", "T")
            (source / "tracked").write_text("base\n")
            git(source, "add", "tracked")
            git(source, "commit", "-qm", "base")
            rev = git(source, "rev-parse", "HEAD")

            async def launch(request_id, **kw):
                args = dict(request_id=request_id, source_repository=str(source),
                            starting_revision=rev, destination=str(d / request_id),
                            worktree_mode="bridge-managed-retained", sandbox="read-only",
                            expected_sandbox_policy=RO, **EX)
                args.update(kw)
                return await error_of(b.create_worktree_thread(**args))

            ww = {"sandbox": "workspace-write"}
            refusals["wt_bogus_type"] = await launch("a", expected_sandbox_policy={"type": "bogus"})
            refusals["wt_flag_before_roots"] = await launch(
                "b", **ww, expected_sandbox_policy={**WW, "excludeSlashTmp": "no",
                                                    "writableRoots": ["rel"]})
            refusals["wt_roots_relative"] = await launch(
                "c", **ww, expected_sandbox_policy={**WW, "writableRoots": ["rel"]})
            refusals["wt_roots_not_list"] = await launch(
                "d", **ww, expected_sandbox_policy={**WW, "writableRoots": "rel"})
            refusals["wt_roots_not_string"] = await launch(
                "e", **ww, expected_sandbox_policy={**WW, "writableRoots": [7]})
            refusals["wt_mode_disagrees"] = await launch("f", sandbox="workspace-write")
            for name in ("prompt", "title", "app_server_project_id", "source_repository"):
                refusals[f"wt_blank_{name}"] = await launch("g", **{name: " "})
            refusals["wt_parent_missing"] = await launch("h", destination=str(d / "no" / "x"))
            (d / "afile").write_text("x")
            refusals["wt_parent_is_file"] = await launch("i", destination=str(d / "afile" / "x"))
            git(source, "worktree", "add", "-q", "--detach", str(d / "stale"), rev)
            subprocess.check_call(["rm", "-rf", str(d / "stale")])
            refusals["wt_registered_worktree"] = await launch("j", destination=str(d / "stale"))

            fake.override_creation = {"model": "other"}
            out["worktree_model_mismatch"] = await launch("k", prompt="p")
            fake.override_creation = {"sandbox": {"type": "workspaceWrite",
                                                  "writableRoots": ["/w"]}}
            out["worktree_sandbox_mismatch"] = await launch("l", prompt="p")
            fake.override_creation = {}

            git(source, "branch", rev)
            r = await b.create_worktree_thread("branch-named-like-sha", str(source), rev,
                                               str(d / "branchsha"), "bridge-managed-retained",
                                               "read-only", RO, **EX)
            out["branch_named_like_sha"] = {"status": r["status"],
                                            "detached": r["worktree"]["detached"]}

            fake.pause_after = "turn/start"
            task = asyncio.create_task(b.create_worktree_thread(
                "disp", str(source), rev, str(d / "disp"), "bridge-managed-retained",
                "read-only", RO, prompt="p", **EX))
            await fake.paused.wait()
            row = json.loads(Ledger(d / "l.sqlite3").db.execute(
                "select receipt from operations where request_id='disp'").fetchone()[0])
            out["dispatching_row"] = {"keys": sorted(row), **{k: row.get(k) for k in (
                "phase", "status", "initialPrompt", "recoveryRequired", "recovery")}}
            fake.release.set()
            fake.pause_after = None
            accepted = await task
            out["worktree_accepted_keys"] = sorted(accepted)

            (d / "taken").mkdir()
            r = await b.create_worktree_thread("taken", str(source), rev, str(d / "taken"),
                                               "bridge-managed-retained", "read-only", RO, **EX)
            out["worktree_validation_failure"] = {"keys": sorted(r), "desktopProjectAssociation":
                                                  r["desktopProjectAssociation"]}
            out["refusals"] = refusals
            await rpc.close()
            ledger.close()
        return str(d)


result = {}
tmp = asyncio.run(main(result))
print(json.dumps(result, indent=1, sort_keys=True).replace(tmp, "<ROOT>"))
