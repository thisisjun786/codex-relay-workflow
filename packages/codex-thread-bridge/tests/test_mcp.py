import json
import sys

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client


async def test_mcp_forwards_json_shaped_pagination_cursors(fake_server, tmp_path):
    fake, socket = fake_server
    fake.cursor = lambda offset: json.dumps({"offset": offset, "scope": {"kind": "turns"}})
    fake.offset = lambda cursor: 0 if cursor is None else json.loads(cursor)["offset"]
    fake.threads = {
        f"thread-{i}": {
            "id": f"thread-{i}",
            "cwd": str(tmp_path),
            "status": {"type": "idle"},
            "turns": [{"id": f"turn-{j}", "status": "completed", "items": []} for j in range(2)],
        }
        for i in range(2)
    }
    params = StdioServerParameters(
        command=sys.executable,
        args=[
            "-m",
            "codex_thread_bridge.server",
            "--socket",
            str(socket),
            "--state-dir",
            str(tmp_path / "state"),
        ],
    )
    async with stdio_client(params) as (read, write), ClientSession(read, write) as session:
        await session.initialize()
        for tool, args, page_key, expected_id in (
            ("read_thread", {"thread_id": "thread-0", "limit": 1}, "turnsPage", "turn-0"),
            ("list_threads", {"limit": 1}, None, "thread-1"),
        ):
            first = await session.call_tool(tool, args)
            assert not first.isError
            page = first.structuredContent[page_key] if page_key else first.structuredContent
            cursor = page["nextCursor"]
            second = await session.call_tool(tool, {**args, "cursor": cursor})
            assert not second.isError, second.content
            page = second.structuredContent[page_key] if page_key else second.structuredContent
            assert [item["id"] for item in page["data"]] == [expected_id]
            method = "thread/turns/list" if page_key else "thread/list"
            assert [p["cursor"] for m, p in fake.calls if m == method and "cursor" in p] == [cursor]
    assert not any(
        method in {"thread/start", "thread/resume", "turn/start"} for method, _ in fake.calls
    )


async def test_real_mcp_stdio_discovery_create_read_and_dedup(fake_server, tmp_path):
    fake, socket = fake_server
    params = StdioServerParameters(
        command=sys.executable,
        args=[
            "-m",
            "codex_thread_bridge.server",
            "--socket",
            str(socket),
            "--state-dir",
            str(tmp_path / "state"),
        ],
    )
    async with stdio_client(params) as (read, write), ClientSession(read, write) as session:
        await session.initialize()
        tools = (await session.list_tools()).tools
        assert {tool.name for tool in tools} == {
            "get_capabilities",
            "create_thread",
            "create_worktree_thread",
            "send_message_to_thread",
            "read_thread",
            "list_threads",
            "wait_thread",
            "get_goal",
            "get_active_turn",
            "steer_thread",
            "pause_goal",
            "get_operation",
        }
        caps = await session.call_tool("get_capabilities", {})
        assert not caps.isError
        assert caps.structuredContent["capabilities"]["desktopManagedWorktrees"] is False
        # Exposure and host support are separate answers over MCP too: this bridge offers
        # steering, withholds interrupt, and does not claim the connected host was tested.
        assert caps.structuredContent["exposure"]["steerActiveTurn"] is True
        assert caps.structuredContent["exposure"]["turnInterrupt"] is False
        assert caps.structuredContent["hostSupport"]["state"] == "unknown_host_version"
        args = {"request_id": "mcp-create", "cwd": str(tmp_path), "prompt": "READY"}
        result = await session.call_tool("create_thread", args)
        assert not result.isError
        receipt = result.structuredContent
        assert receipt["status"] == "accepted"
        repeated = await session.call_tool("create_thread", args)
        assert repeated.structuredContent["replayed"]
        followup = await session.call_tool(
            "send_message_to_thread",
            {
                "request_id": "mcp-send",
                "thread_id": receipt["threadId"],
                "message": "FOLLOWUP",
            },
        )
        sent = followup.structuredContent
        assert sent["status"] == "accepted"
        waited = await session.call_tool(
            "wait_thread",
            {
                "thread_id": receipt["threadId"],
                "turn_id": sent["turnId"],
                "timeout_seconds": 0,
            },
        )
        assert waited.structuredContent["turn"]["items"][0]["text"] == "FOLLOWUP"
        history = await session.call_tool("read_thread", {"thread_id": receipt["threadId"]})
        assert len(history.structuredContent["turnsPage"]["data"]) == 2
        goal = await session.call_tool("get_goal", {"thread_id": receipt["threadId"]})
        assert goal.structuredContent["goal"] is None

        # The whole running-peer path over MCP: an idle thread reports no steerable turn, and
        # steering it is refused rather than quietly turned into a new turn.
        idle = await session.call_tool("get_active_turn", {"thread_id": receipt["threadId"]})
        assert idle.structuredContent["activeTurnId"] is None
        assert idle.structuredContent["steerable"] is False
        refused = await session.call_tool(
            "steer_thread",
            {
                "request_id": "mcp-steer-idle",
                "thread_id": receipt["threadId"],
                "expected_turn_id": sent["turnId"],
                "message": "SCOPE CHANGE",
            },
        )
        assert refused.structuredContent["status"] == "failed"
        assert refused.structuredContent["rpcError"]["code"] == "thread_idle"

        # With a turn actually running, the same call is accepted against the guarded turn.
        fake.complete_turns = False
        started = await session.call_tool(
            "send_message_to_thread",
            {
                "request_id": "mcp-running",
                "thread_id": receipt["threadId"],
                "message": "LONG WORK",
            },
        )
        running_turn = started.structuredContent["turnId"]
        fake.threads[receipt["threadId"]]["status"] = {"type": "active", "activeFlags": []}
        observed = await session.call_tool("get_active_turn", {"thread_id": receipt["threadId"]})
        assert observed.structuredContent["activeTurnId"] == running_turn
        steered = await session.call_tool(
            "steer_thread",
            {
                "request_id": "mcp-steer",
                "thread_id": receipt["threadId"],
                "expected_turn_id": running_turn,
                "message": "SCOPE CHANGE",
            },
        )
        assert steered.structuredContent["status"] == "accepted"
        assert steered.structuredContent["delivery"] == "accepted_not_applied"

        # Pausing is a different action again, and it is refused when there is no goal.
        paused = await session.call_tool(
            "pause_goal", {"request_id": "mcp-pause", "thread_id": receipt["threadId"]}
        )
        assert paused.structuredContent["rpcError"]["code"] == "no_goal"

        invalid = await session.call_tool("create_thread", {**args, "sandbox": "invalid"})
        assert invalid.isError
    assert fake.count("thread/start") == 1 and fake.count("turn/start") == 3
    assert fake.count("turn/steer") == 1 and fake.count("thread/goal/set") == 0


async def test_mcp_socket_alias_restart_does_not_repeat_creation(fake_server, tmp_path):
    fake, socket = fake_server
    alias = socket.parent / "alias.sock"
    alias.symlink_to(socket)
    receipts = []
    for path in [alias, socket]:
        params = StdioServerParameters(
            command=sys.executable,
            args=[
                "-m",
                "codex_thread_bridge.server",
                "--socket",
                str(path),
                "--state-dir",
                str(tmp_path / "state"),
            ],
        )
        async with stdio_client(params) as (read, write), ClientSession(read, write) as session:
            await session.initialize()
            result = await session.call_tool(
                "create_thread",
                {
                    "request_id": "stable-create",
                    "cwd": str(tmp_path),
                    "prompt": "READY",
                },
            )
            assert not result.isError
            receipts.append(result.structuredContent)
    assert receipts[0]["threadId"] == receipts[1]["threadId"]
    assert receipts[1]["replayed"]
    assert fake.count("thread/start") == 1 and fake.count("turn/start") == 1
