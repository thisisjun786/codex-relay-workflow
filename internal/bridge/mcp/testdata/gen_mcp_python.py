"""Record the Python MCP server's caller-visible replies for internal/contracttest/mcp_parity_test.go.

Run from the repository root, with HOME/XDG_*/CODEX_HOME/TMPDIR under a scratch directory:
  uv run --no-sync python internal/bridge/mcp/testdata/gen_mcp_python.py \
      > internal/bridge/mcp/testdata/mcp_python.json

It starts `python -m codex_thread_bridge.server` over stdio against the Python suite's own
FakeServer and runs STEPS, which use the corpus step format so the Go test replays the same list
through `crw bridge`. The case directory is written <HOME>, the fake's socket <SOCKET>, and the
wall-clock fields startedAt, updatedAt and at are dropped.
"""
import asyncio, json, os, sys, tempfile
from pathlib import Path

sys.path.insert(0, "packages/codex-thread-bridge/tests")
from conftest import FakeServer  # noqa: E402
from mcp import ClientSession, StdioServerParameters  # noqa: E402
from mcp.client.stdio import stdio_client  # noqa: E402
from websockets.asyncio.server import unix_serve  # noqa: E402

PAIR = {"model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh"}
CREATE = {"request_id": "made", "cwd": "${HOME}", "prompt": "READY", **PAIR}
STEPS = [
    {"tool": "create_thread", "arguments": CREATE},
    {"tool": "create_thread", "arguments": CREATE},
    {"tool": "get_operation", "arguments": {"request_id": "made"}},
    {"tool": "create_thread", "arguments": {**CREATE, "prompt": "OTHER"}},
    {"tool": "send_message_to_thread", "arguments": {"request_id": "sent", "thread_id": "thread-1", "message": "NEXT", "expected_settings": PAIR}},
    {"tool": "read_thread", "arguments": {"thread_id": "thread-1", "limit": 1}},
    {"tool": "list_threads", "arguments": {}},
    {"tool": "get_active_turn", "arguments": {"thread_id": "thread-1"}},
    {"tool": "wait_thread", "arguments": {"thread_id": "thread-1", "turn_id": "turn-2", "timeout_seconds": 0}},
    {"tool": "get_goal", "arguments": {"thread_id": "thread-1"}},
    {"tool": "steer_thread", "arguments": {"request_id": "steer", "thread_id": "thread-1", "expected_turn_id": "turn-2", "message": "MORE"}},
    {"tool": "pause_goal", "arguments": {"request_id": "pause", "thread_id": "thread-1"}},
    {"tool": "get_operation", "arguments": {"request_id": "missing"}},
    {"tool": "create_thread", "arguments": {"request_id": "rel", "cwd": "relative", **PAIR}},
    {"tool": "create_thread", "arguments": {"request_id": "role", "cwd": "${HOME}", "role": "", **PAIR}},
    {"tool": "create_thread", "arguments": {"request_id": "title", "cwd": "${HOME}", "title": "t" * 501, **PAIR}},
    {"tool": "create_thread", "arguments": {"request_id": "blank", "cwd": "${HOME}", "model": " ", "reasoning_effort": "xhigh"}},
    {"tool": "create_thread", "arguments": {"request_id": "exc", "cwd": "${HOME}", "policy_exception": "none", **PAIR}},
    {"tool": "create_thread", "arguments": {"request_id": "who", "cwd": "${HOME}", "role": "child", **PAIR}},
    {"tool": "create_thread", "arguments": {"request_id": "net", "cwd": "${HOME}", "expected_sandbox_policy": {"type": "readOnly", "networkAccess": True}, **PAIR}},
    {"tool": "create_worktree_thread", "arguments": {"request_id": "wt", "source_repository": "${HOME}", "starting_revision": "0" * 40, "destination": "${HOME}/new", "worktree_mode": "bridge-managed-retained", "sandbox": "workspace-write", "expected_sandbox_policy": {"type": "readOnly", "networkAccess": False}, **PAIR}},
    {"tool": "send_message_to_thread", "arguments": {"request_id": "keys", "thread_id": "thread-1", "message": "x", "expected_settings": {**PAIR, "bogus": 1}}},
    {"tool": "send_message_to_thread", "arguments": {"request_id": "halfpair", "thread_id": "thread-1", "message": "x", "expected_settings": {"model": PAIR["model"]}}},
    {"tool": "list_threads", "arguments": {"limit": 101}},
    {"tool": "list_threads", "arguments": {"limit": "5"}},
    {"tool": "read_thread", "arguments": {"thread_id": "thread-1", "max_text_chars": 99}},
    {"tool": "wait_thread", "arguments": {"thread_id": "thread-1", "turn_id": "u", "timeout_seconds": 51}},
    {"tool": "get_goal", "arguments": {"thread_id": "x" * 129}},
    {"tool": "pause_goal", "arguments": {"request_id": "", "thread_id": "thread-1"}},
    {"tool": "no_such_tool", "arguments": {}},
]
DROPPED = {"startedAt", "updatedAt", "at"}


def scrub(value, home, socket):
    if isinstance(value, dict):
        return {k: scrub(v, home, socket) for k, v in value.items() if k not in DROPPED}
    if isinstance(value, list):
        return [scrub(v, home, socket) for v in value]
    if isinstance(value, str):
        return value.replace(str(socket), "<SOCKET>").replace(home, "<HOME>")
    return value


def resolved(value, home):
    if isinstance(value, dict):
        return {k: resolved(v, home) for k, v in value.items()}
    if isinstance(value, str):
        return value.replace("${HOME}", home)
    return value


async def main():
    with tempfile.TemporaryDirectory(dir=os.environ["TMPDIR"]) as tmp:
        home = str(Path(tmp).resolve() / "case")
        os.mkdir(home)
        socket = Path(tmp) / "app.sock"
        fake = FakeServer()
        async with unix_serve(fake.handle, str(socket)):
            parameters = StdioServerParameters(
                command=sys.executable,
                args=["-m", "codex_thread_bridge.server", "--socket", str(socket), "--state-dir", home + "/state"],
                env=dict(os.environ))
            results = []
            async with stdio_client(parameters, errlog=open(os.devnull, "w")) as (r, w), ClientSession(r, w) as s:
                await s.initialize()
                for step in STEPS:
                    if step["tool"] == "get_active_turn":
                        fake.threads["thread-1"]["status"] = {"type": "active", "activeFlags": []}
                        fake.threads["thread-1"]["turns"][-1]["status"] = "inProgress"
                    result = await s.call_tool(step["tool"], resolved(step["arguments"], home))
                    content = [part.model_dump(mode="json", exclude_none=True) for part in result.content]
                    for part in content:
                        text = part.pop("text")
                        try:
                            part["json"] = scrub(json.loads(text), home, socket)
                        except ValueError:
                            part["text"] = scrub(text, home, socket)
                    results.append({"error": bool(result.isError), "content": content,
                                    "structured": scrub(result.structuredContent, home, socket)})
    json.dump({"steps": STEPS, "results": results}, sys.stdout, indent=1, ensure_ascii=False)
    print()


asyncio.run(main())
