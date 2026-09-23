import asyncio
import hashlib
import json
import os
import sys
from pathlib import Path

import pytest

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

APPROVED = {"model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh"}
POLICY = {"allowed": [{"model": APPROVED["model"], "efforts": [APPROVED["reasoning_effort"]]}]}


def server_parameters(socket, state, environment=None):
    return StdioServerParameters(
        command=sys.executable,
        args=[
            "-m",
            "codex_thread_bridge.server",
            "--socket",
            str(socket),
            "--state-dir",
            str(state),
        ],
        env={**os.environ, **(environment or {})},
    )


async def test_the_schema_itself_refuses_a_mutation_that_states_no_pair(fake_server, tmp_path):
    """The strongest signal available to a caller that forgets: the call is not accepted."""
    fake, socket = fake_server
    params = server_parameters(socket, tmp_path / "state")
    async with stdio_client(params) as (read, write), ClientSession(read, write) as session:
        await session.initialize()
        schemas = {tool.name: tool.inputSchema for tool in (await session.list_tools()).tools}
        assert {"model", "reasoning_effort"} <= set(schemas["create_thread"]["required"])
        assert {"model", "reasoning_effort"} <= set(schemas["create_worktree_thread"]["required"])
        assert "expected_settings" in schemas["send_message_to_thread"]["required"]
        incomplete = [
            ("create_thread", {"request_id": "a", "cwd": str(tmp_path)}),
            (
                "create_thread",
                {"request_id": "b", "cwd": str(tmp_path), "model": APPROVED["model"]},
            ),
            ("send_message_to_thread", {"request_id": "d", "thread_id": "t", "message": "x"}),
            (
                "create_worktree_thread",
                {
                    "request_id": "e",
                    "source_repository": str(tmp_path),
                    "starting_revision": "0" * 40,
                    "destination": str(tmp_path / "new"),
                    "worktree_mode": "bridge-managed-retained",
                    "sandbox": "read-only",
                    "expected_sandbox_policy": {"type": "readOnly", "networkAccess": False},
                },
            ),
        ]
        for tool, arguments in incomplete:
            result = await session.call_tool(tool, arguments)
            assert result.isError, (tool, arguments)
    assert not any(
        method in {"thread/start", "thread/resume", "turn/start"} for method, _ in fake.calls
    )


async def test_an_argument_a_caller_invents_grants_it_nothing(fake_server, tmp_path):
    """Measured, not assumed: this tool framework DISCARDS an argument the schema does not define.

    That is worth pinning, because discarding is only safe while nothing about authorization can
    be expressed that way. Both halves are checked here. An invented approval flag alongside an
    unapproved pair changes nothing, and a MISSPELLED policy_exception is dropped rather than
    honoured, so the request falls back to the host's own allowlist and is refused. The failure
    direction is refusal, never admission.
    """
    fake, socket = fake_server
    policy = tmp_path / "policy.json"
    policy.write_text(
        json.dumps(
            {
                **POLICY,
                "exceptions": {
                    "one-task": {
                        "model": "openai/gpt-6-astra",
                        "reasoningEffort": "high",
                        "cwd": [str(tmp_path.resolve())],
                    }
                },
            }
        )
    )
    params = server_parameters(
        socket, tmp_path / "state", {"CODEX_THREAD_BRIDGE_EXECUTION_POLICY": str(policy)}
    )
    unapproved = {"model": "openai/gpt-6-astra", "reasoning_effort": "high"}
    async with stdio_client(params) as (read, write), ClientSession(read, write) as session:
        await session.initialize()
        for request, invented in (
            ("self-approved", {"approved": True}),
            ("self-allowlisted", {"allowed_models": ["openai/gpt-6-astra"]}),
            ("misspelled-exception", {"policy_exceptions": "one-task"}),
        ):
            result = await session.call_tool(
                "create_thread",
                {**unapproved, "request_id": request, "cwd": str(tmp_path), **invented},
            )
            assert result.isError, request
            assert "execution_not_allowed" in str(result.content), request
        # The same pair, with the exception cited under its real name, is the one that passes.
        allowed = await session.call_tool(
            "create_thread",
            {
                **unapproved,
                "request_id": "cited",
                "cwd": str(tmp_path),
                "policy_exception": "one-task",
            },
        )
        assert not allowed.isError, allowed.content
    assert fake.count("thread/start") == 1
    assert fake.count("turn/start") == 0


async def test_an_unconfigured_host_accepts_a_stated_pair_and_says_so(fake_server, tmp_path):
    fake, socket = fake_server
    params = server_parameters(socket, tmp_path / "state")
    async with stdio_client(params) as (read, write), ClientSession(read, write) as session:
        await session.initialize()
        capabilities = await session.call_tool("get_capabilities", {})
        assert capabilities.structuredContent["executionPolicy"] == {
            "mode": "presence_only",
            "digest": None,
            "roles": {},
        }
        created = await session.call_tool(
            "create_thread",
            {**APPROVED, "request_id": "unconfigured", "cwd": str(tmp_path), "prompt": "READY"},
        )
        assert not created.isError, created.content
        assert created.structuredContent["executionPolicy"]["mode"] == "presence_only"
    assert fake.count("thread/start") == 1


async def test_a_configured_host_admits_the_approved_pair_and_refuses_the_rest(
    fake_server, tmp_path
):
    """Both directions on ONE configured file: a deny-all wiring fails the first half of this."""
    fake, socket = fake_server
    policy = tmp_path / "policy.json"
    policy.write_text(json.dumps(POLICY))
    params = server_parameters(
        socket,
        tmp_path / "state",
        {"CODEX_THREAD_BRIDGE_EXECUTION_POLICY": str(policy)},
    )
    async with stdio_client(params) as (read, write), ClientSession(read, write) as session:
        await session.initialize()
        capabilities = await session.call_tool("get_capabilities", {})
        reported = capabilities.structuredContent["executionPolicy"]
        assert reported["mode"] == "allowlist" and len(reported["digest"]) == 64
        approved = await session.call_tool(
            "create_thread",
            {**APPROVED, "request_id": "approved", "cwd": str(tmp_path), "prompt": "READY"},
        )
        assert not approved.isError, approved.content
        assert approved.structuredContent["executionPolicy"]["digest"] == reported["digest"]
        assert fake.count("thread/start") == 1
        refused = await session.call_tool(
            "create_thread",
            {
                "request_id": "refused",
                "cwd": str(tmp_path),
                "prompt": "READY",
                "model": "openai/gpt-6-astra",
                "reasoning_effort": "high",
            },
        )
        assert refused.isError
        assert "execution_not_allowed" in str(refused.content)
    assert fake.count("thread/start") == 1 and fake.count("turn/start") == 1


async def test_a_configured_policy_that_cannot_be_used_stops_the_server(fake_server, tmp_path):
    """Degrading to presence-only here would be the one failure an operator never notices."""
    fake, socket = fake_server
    broken = tmp_path / "broken.json"
    broken.write_text("{ not json")
    state = tmp_path / "state"
    process = await asyncio.create_subprocess_exec(
        sys.executable,
        "-m",
        "codex_thread_bridge.server",
        "--socket",
        str(socket),
        "--state-dir",
        str(state),
        stdin=asyncio.subprocess.DEVNULL,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
        env={**os.environ, "CODEX_THREAD_BRIDGE_EXECUTION_POLICY": str(broken)},
    )
    _, errors = await asyncio.wait_for(process.communicate(), timeout=60)
    assert process.returncode != 0
    assert "execution_policy_unreadable" in errors.decode()
    assert not state.exists(), "the server stopped before it opened any durable state"
    assert fake.calls == []


# ------------------------------------------------------------- started the way Codex Desktop starts it
#
# Codex Desktop spawns this server through the crw plugin's launcher, with the App Server's bare
# environment and no execution-policy variable. These start it the same way: the repository's own
# launcher, a record under a temporary CODEX_HOME, and nothing else set. The launcher is outside
# this package, so a run of this suite without the repository around it skips here -- and
# scripts/ci/packages.py fails any skipped case, so the repository's own run never skips.

LAUNCHER = Path(__file__).resolve().parents[3] / "plugins" / "crw" / "wiring" / "crw_bridge_mcp.py"
CHILD = {"model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}
ROLE_POLICY = {"roles": {"child": CHILD,
                         "parent": {"model": "devin/swe-2", "reasoningEffort": "max"}}}


def launched_by_the_plugin(tmp_path, socket, *, policy=ROLE_POLICY):
    if not LAUNCHER.is_file():
        pytest.skip("the crw plugin launcher is not beside this package: " + str(LAUNCHER))
    home = tmp_path / "codex-home"
    home.mkdir()
    record = {"recordVersion": 1, "owner": "plugin", "serverName": "codex-thread-bridge",
              "bridgeExecutable": sys.executable,
              "args": ["-m", "codex_thread_bridge.server", "--socket", str(socket),
                       "--state-dir", str(tmp_path / "state")]}
    digest = None
    if policy is not None:
        path = tmp_path / "execution-policy.json"
        data = json.dumps(policy).encode()
        path.write_bytes(data)
        digest = hashlib.sha256(data).hexdigest()
        record.update(recordVersion=2, executionPolicy={"path": str(path), "digest": digest})
    (home / "crw-bridge-mcp.json").write_text(json.dumps(record))
    # Only what the host hands a plugin server. The SDK adds its own short default list (HOME,
    # PATH and the like), which carries no execution-policy variable either.
    parameters = StdioServerParameters(command=sys.executable, args=[str(LAUNCHER)],
                                       env={"CODEX_HOME": str(home)})
    return parameters, digest


async def test_the_plugin_launched_bridge_reports_the_host_policy_and_checks_the_role(
    fake_server, tmp_path
):
    fake, socket = fake_server
    params, digest = launched_by_the_plugin(tmp_path, socket)
    async with stdio_client(params) as (read, write), ClientSession(read, write) as session:
        await session.initialize()
        capabilities = await session.call_tool("get_capabilities", {})
        reported = capabilities.structuredContent["executionPolicy"]
        assert reported["digest"] == digest
        assert reported["roles"]["child"] == {"role": "child", "expectation": "pair",
                                              "model": CHILD["model"],
                                              "reasoningEffort": CHILD["reasoningEffort"]}
        before = list(fake.calls)
        refused = await session.call_tool(
            "create_thread",
            {"request_id": "wrong-child", "cwd": str(tmp_path), "prompt": "READY",
             "model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh", "role": "child"},
        )
        assert refused.isError
        assert "execution_role_mismatch" in str(refused.content)
        # Not one call of any kind reached the host for the refused request.
        assert fake.calls == before
        created = await session.call_tool(
            "create_thread",
            {"request_id": "right-child", "cwd": str(tmp_path), "prompt": "READY",
             "model": CHILD["model"], "reasoning_effort": CHILD["reasoningEffort"],
             "role": "child"},
        )
        assert not created.isError, created.content
        assert created.structuredContent["executionPolicy"]["digest"] == digest
    assert fake.count("thread/start") == 1


async def test_a_plugin_record_naming_no_policy_starts_exactly_as_before(fake_server, tmp_path):
    fake, socket = fake_server
    params, _ = launched_by_the_plugin(tmp_path, socket, policy=None)
    async with stdio_client(params) as (read, write), ClientSession(read, write) as session:
        await session.initialize()
        capabilities = await session.call_tool("get_capabilities", {})
        assert capabilities.structuredContent["executionPolicy"] == {
            "mode": "presence_only",
            "digest": None,
            "roles": {},
        }


async def test_a_server_started_expecting_another_digest_never_starts(fake_server, tmp_path):
    """The bridge's own check, for a file that changed after the launcher looked at it."""
    fake, socket = fake_server
    policy = tmp_path / "execution-policy.json"
    policy.write_text(json.dumps(ROLE_POLICY))
    state = tmp_path / "state"
    process = await asyncio.create_subprocess_exec(
        sys.executable,
        "-m",
        "codex_thread_bridge.server",
        "--socket",
        str(socket),
        "--state-dir",
        str(state),
        stdin=asyncio.subprocess.DEVNULL,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
        env={
            **os.environ,
            "CODEX_THREAD_BRIDGE_EXECUTION_POLICY": str(policy),
            "CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST": "0" * 64,
        },
    )
    _, errors = await asyncio.wait_for(process.communicate(), timeout=60)
    assert process.returncode != 0
    assert "execution_policy_unreadable" in errors.decode()
    assert "changed after it was registered" in errors.decode()
    assert not state.exists(), "the server stopped before it opened any durable state"
    assert fake.calls == []


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
        # The same distinction survives the real MCP surface. What this bridge implements is a
        # map of flags; what nobody asked the host is a separate block of sentences, and the
        # question that used to be answered false is not answered here at all.
        flags = caps.structuredContent["capabilities"]
        assert "desktopManagedWorktrees" not in flags
        assert all(isinstance(flag, bool) for flag in flags.values())
        questions = caps.structuredContent["hostNotProbed"]
        assert set(questions) == {"desktopManagedWorktrees", "desktopProjectRegistry"}
        assert all(isinstance(answer, str) for answer in questions.values())
        # Exposure and host support are separate answers over MCP too: this bridge offers
        # steering, withholds interrupt, and does not claim the connected host was tested.
        assert caps.structuredContent["exposure"]["steerActiveTurn"] is True
        assert caps.structuredContent["exposure"]["turnInterrupt"] is False
        assert caps.structuredContent["hostSupport"]["state"] == "unknown_host_version"
        args = {
            "request_id": "mcp-create",
            "cwd": str(tmp_path),
            "prompt": "READY",
            "model": "anthropic/claude-opus-5",
            "reasoning_effort": "xhigh",
        }
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
                "expected_settings": {
                    "model": "anthropic/claude-opus-5",
                    "reasoning_effort": "xhigh",
                },
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
        # The completeness marker has to survive the MCP surface, because that is the only place
        # a caller reading only "items" would ever find out that it is holding the summary view.
        observed = history.structuredContent["observation"]
        assert observed["turnsPageStatus"] == "summary"
        assert observed["itemsView"] == "summary"
        assert observed["detailTurnsRequested"] == 1
        assert observed["detailTurnsObserved"] == 1
        assert "bounded observation" in observed["note"]
        newest, older = history.structuredContent["turnsPage"]["data"]
        assert newest["itemsDetailStatus"] == "complete"
        assert older["itemsDetailStatus"] == "not_requested"
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
                "expected_settings": dict(APPROVED),
            },
        )
        assert not started.isError, started.content
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
                    "model": "anthropic/claude-opus-5",
                    "reasoning_effort": "xhigh",
                },
            )
            assert not result.isError
            receipts.append(result.structuredContent)
    assert receipts[0]["threadId"] == receipts[1]["threadId"]
    assert receipts[1]["replayed"]
    assert fake.count("thread/start") == 1 and fake.count("turn/start") == 1
