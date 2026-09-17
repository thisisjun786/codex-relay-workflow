import asyncio

import pytest
from conftest import EFFORT, EXECUTION, MODEL

from codex_thread_bridge.ledger import Ledger


# Every mutation must now state its model and reasoning effort, so these helpers supply the
# approved pair once instead of at forty call sites. Tests about the guard itself call
# bridge.create_thread and bridge.send_message_to_thread directly, with the pair left out,
# blank or unapproved on purpose.
async def create(bridge, *args, **kwargs):
    return await bridge.create_thread(*args, **{**EXECUTION, **kwargs})


async def send(bridge, request_id, thread_id, message, **kwargs):
    carried = {**EXECUTION, **(kwargs.pop("expected_settings", None) or {})}
    return await bridge.send_message_to_thread(request_id, thread_id, message, carried, **kwargs)


async def test_create_and_followup_carry_the_stated_pair_and_exact_messages(
    bridge, fake_server, tmp_path
):
    """The regression this guard exists for; the previous contract asserted the opposite.

    It pinned "model" not in creation_params and a creation reporting the host's
    configured-default, which is precisely the silent inheritance that started a task on a model
    nobody chose. Both the start and the resume now carry the stated pair.
    """
    fake, _ = fake_server
    first = await create(bridge, "create", str(tmp_path), prompt="  exact\nmessage  ", title="Demo")
    assert first["status"] == "accepted"
    assert first["creation"]["model"] == MODEL
    creation_params = next(p for name, p in fake.calls if name == "thread/start")
    assert creation_params["model"] == MODEL
    assert creation_params["config"] == {"model_reasoning_effort": EFFORT}
    assert "projectId" not in creation_params
    assert not any(name.startswith("thread/goal/") for name, _ in fake.calls)
    assert fake.threads[first["threadId"]]["turns"][0]["items"][0]["text"] == "  exact\nmessage  "
    followup = await send(bridge, "send", first["threadId"], "followup")
    assert followup["status"] == "accepted" and followup["turnId"] == "turn-2"
    assert next(p for name, p in fake.calls if name == "thread/resume") == {
        "threadId": first["threadId"],
        "excludeTurns": True,
        "approvalPolicy": "never",
        "model": MODEL,
        "config": {"model_reasoning_effort": EFFORT},
    }
    result = await bridge.wait_thread(first["threadId"], followup["turnId"], 0)
    assert result["turn"]["items"][0]["text"] == "followup"


async def test_empty_creation_does_not_dispatch_or_set_goal(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    result = await create(bridge, "empty", str(tmp_path))
    assert "turnId" not in result
    assert fake.count("turn/start") == 0
    assert fake.count("thread/goal/set") == 0


async def test_duplicate_create_and_message_do_not_dispatch_twice(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    results = await asyncio.gather(
        *[create(bridge, "same", str(tmp_path), prompt="hello") for _ in range(3)]
    )
    assert len({r["threadId"] for r in results}) == 1
    assert fake.count("thread/start") == 1 and fake.count("turn/start") == 1
    tid = results[0]["threadId"]
    for _ in range(3):
        await send(bridge, "same-send", tid, "hello again")
    assert fake.count("turn/start") == 2


async def test_conflicting_request_id_fails_without_mutation(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    await create(bridge, "same", str(tmp_path), prompt="first")
    with pytest.raises(ValueError, match="different arguments"):
        await create(bridge, "same", str(tmp_path), prompt="second")
    assert fake.count("thread/start") == 1


async def test_replay_after_cwd_removal_returns_retained_receipt(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    cwd = tmp_path / "checkout"
    cwd.mkdir()
    first = await create(bridge, "create", str(cwd))
    cwd.rmdir()
    calls = len(fake.calls)
    repeated = await create(bridge, "create", str(cwd))
    assert repeated["replayed"] and repeated["threadId"] == first["threadId"]
    assert len(fake.calls) == calls
    with pytest.raises(ValueError, match="different arguments"):
        await create(bridge, "create", str(cwd), prompt="different")


async def test_cwd_symlink_retargeting_does_not_change_request_identity(
    bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    original, other = tmp_path / "original", tmp_path / "other"
    original.mkdir()
    other.mkdir()
    alias = tmp_path / "checkout"
    alias.symlink_to(original, target_is_directory=True)
    first = await create(bridge, "create", str(alias))
    assert first["creation"]["cwd"] == str(original)
    alias.unlink()
    alias.symlink_to(other, target_is_directory=True)
    repeated = await create(bridge, "create", str(alias))
    assert repeated["replayed"] and repeated["threadId"] == first["threadId"]
    alias.unlink()
    assert (await create(bridge, "create", str(alias)))["replayed"]
    assert fake.count("thread/start") == 1


async def test_old_canonical_cwd_fingerprint_still_replays_without_directory(
    bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    cwd = str(tmp_path / "removed-before-upgrade")
    # Version 0.1.0 used this resolved-cwd payload, with no schema version field.
    _, receipt = bridge.ledger.begin(
        "old-create",
        "create_thread",
        {
            "cwd": cwd,
            "sandbox": "read-only",
            "approvalPolicy": "never",
            "ephemeral": False,
            "prompt": None,
            "title": None,
        },
    )
    bridge.ledger.save({**receipt, "status": "accepted", "threadId": "retained-thread"})
    # Reproduces the pre-guard argument shape exactly: no model, no effort. The guard runs after
    # the ledger lookup, so a retained receipt is still answered from the ledger and nothing is
    # sent to the host.
    repeated = await bridge.create_thread("old-create", cwd)
    assert repeated["replayed"] and repeated["threadId"] == "retained-thread"
    assert not fake.calls


async def test_legacy_cwd_symlink_replay_uses_old_fingerprint_only_for_legacy_receipts(
    bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    target = tmp_path / "target"
    target.mkdir()
    alias = tmp_path / "alias"
    alias.symlink_to(target, target_is_directory=True)
    _, receipt = bridge.ledger.begin(
        "legacy",
        "create_thread",
        {
            "cwd": str(target),
            "sandbox": "read-only",
            "approvalPolicy": "never",
            "ephemeral": False,
            "prompt": None,
            "title": None,
        },
    )
    receipt.pop("fingerprintVersion")  # Exact receipt format from the old implementation.
    bridge.ledger.save({**receipt, "threadId": "legacy-thread", "status": "accepted"})
    recovered = await bridge.create_thread("legacy", str(alias))
    assert recovered["replayed"] and recovered["threadId"] == "legacy-thread"
    assert not fake.calls
    with pytest.raises(ValueError, match="different arguments"):
        await bridge.create_thread("legacy", str(alias), prompt="changed")
    await create(bridge, "new", str(target))
    # A new-format request cannot use the legacy escape hatch with different raw arguments.
    with pytest.raises(ValueError, match="different arguments"):
        await create(bridge, "new", str(alias))
    assert fake.count("thread/start") == 1


async def test_lost_creation_response_is_never_retried(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    fake.drop_after = "thread/start"
    result = await create(bridge, "lost", str(tmp_path), prompt="hello")
    assert result["status"] == "outcome_unknown"
    assert "threadId" not in result
    fake.drop_after = None
    repeat = await create(bridge, "lost", str(tmp_path), prompt="hello")
    assert repeat["replayed"] and repeat["status"] == "outcome_unknown"
    assert fake.count("thread/start") == 1 and fake.count("turn/start") == 0


async def test_partial_failure_retains_created_id(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    fake.reject["thread/name/set"] = {"code": -32602, "message": "name rejected"}
    result = await create(bridge, "partial", str(tmp_path), prompt="hello", title="Demo")
    assert result["status"] == "failed" and result["threadId"] == "thread-1"
    assert fake.count("turn/start") == 0
    assert bridge.ledger.get("partial")["threadId"] == "thread-1"


async def test_lost_initial_turn_response_retains_id_without_resend(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    fake.drop_after = "turn/start"
    first = await create(bridge, "lost-turn", str(tmp_path), prompt="hello")
    assert first["status"] == "outcome_unknown" and first["threadId"] == "thread-1"
    await create(bridge, "lost-turn", str(tmp_path), prompt="hello")
    assert fake.count("turn/start") == 1


async def test_environment_mismatch_withholds_prompt(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    fake.override_creation = {"sandbox": {"type": "dangerFullAccess"}}
    result = await create(bridge, "mismatch", str(tmp_path), prompt="hello")
    assert result["status"] == "failed" and result["threadId"] == "thread-1"
    assert fake.count("turn/start") == 0


async def test_desktop_project_id_not_found_stops_before_creation(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    fake.reject["project/read"] = {"code": -32602, "message": "project not found"}
    result = await create(bridge, "project", str(tmp_path), app_server_project_id="desktop-id")
    assert result["status"] == "failed" and fake.count("thread/start") == 0


async def test_busy_thread_is_not_resumed_or_messaged(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    created = await create(bridge, "create", str(tmp_path))
    fake.threads[created["threadId"]]["status"] = {"type": "active"}
    result = await send(bridge, "busy", created["threadId"], "hello")
    assert result["status"] == "failed"
    assert fake.count("thread/resume") == 0 and fake.count("turn/start") == 0


async def test_interactive_approval_policy_withholds_message(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    created = await create(bridge, "create", str(tmp_path))
    fake.approval_policy = "on-request"
    result = await send(bridge, "send", created["threadId"], "hello")
    assert result["status"] == "failed" and "resumed" in result
    assert fake.count("turn/start") == 0


async def test_reads_and_waits_do_not_resume_or_use_other_completed_turn(
    bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    created = await create(bridge, "create", str(tmp_path), prompt="a" * 300)
    count = len(fake.calls)
    read = await bridge.read_thread(created["threadId"], max_text_chars=100)
    assert "truncated" in read["turnsPage"]["data"][0]["items"][0]["text"]
    result = await bridge.wait_thread(created["threadId"], "not-this-turn", 0.02)
    assert result["timedOut"] and result["turn"] is None
    await bridge.list_threads()
    assert all(
        name in {"thread/read", "thread/turns/list", "thread/list"}
        for name, _ in fake.calls[count:]
    )


async def test_history_pagination(bridge, tmp_path):
    created = await create(bridge, "create", str(tmp_path), prompt="first")
    await send(bridge, "send", created["threadId"], "second")
    page1 = await bridge.read_thread(created["threadId"], limit=1)
    page2 = await bridge.read_thread(
        created["threadId"], limit=1, cursor=page1["turnsPage"]["nextCursor"]
    )
    assert page1["turnsPage"]["data"][0]["id"] == "turn-2"
    assert page2["turnsPage"]["data"][0]["id"] == "turn-1"


async def test_low_text_limit_preserves_page_cursors_and_protocol_fields(
    bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    created = await create(bridge, "create", str(tmp_path), prompt="first" * 100)
    await send(bridge, "send", created["threadId"], "second" * 100)
    long_path = "/" + "directory/" * 30
    fake.threads[created["threadId"]]["cwd"] = long_path
    newest = fake.threads[created["threadId"]]["turns"][-1]
    newest["items"][0]["id"] = "item-" + "x" * 120
    page1 = await bridge.read_thread(created["threadId"], limit=1, max_text_chars=100)
    assert page1["thread"]["cwd"] == long_path
    assert page1["turnsPage"]["nextCursor"] == fake.cursor(1)
    assert page1["turnsPage"]["backwardsCursor"] == fake.cursor(0)
    item = page1["turnsPage"]["data"][0]["items"][0]
    assert item["id"] == newest["items"][0]["id"]
    assert item["text"].startswith("second" * 16) and "truncated" in item["text"]
    page2 = await bridge.read_thread(
        created["threadId"], limit=1, cursor=page1["turnsPage"]["nextCursor"], max_text_chars=100
    )
    assert page2["turnsPage"]["data"][0]["id"] == "turn-1"


async def test_list_preserves_long_cursor(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    fake.cursor_padding = 5000
    await create(bridge, "first", str(tmp_path))
    await create(bridge, "second", str(tmp_path))
    first = await bridge.list_threads(limit=1)
    assert first["nextCursor"] == fake.cursor(1)
    second = await bridge.list_threads(limit=1, cursor=first["nextCursor"])
    assert first["data"][0]["id"] != second["data"][0]["id"]


async def test_cancellation_keeps_unknown_receipt_and_prevents_retry(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    started = asyncio.Event()
    original = bridge.rpc.call

    async def slow(method, params):
        if method == "thread/start":
            started.set()
            await asyncio.Event().wait()
        return await original(method, params)

    bridge.rpc.call = slow
    task = asyncio.create_task(create(bridge, "cancel", str(tmp_path)))
    await started.wait()
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert bridge.ledger.get("cancel")["status"] == "outcome_unknown"
    repeat = await create(bridge, "cancel", str(tmp_path))
    assert repeat["replayed"] and fake.count("thread/start") == 0


def test_ledger_survives_restarts_and_is_private(tmp_path):
    path = tmp_path / "private" / "state.sqlite3"
    first = Ledger(path)
    fresh, receipt = first.begin("key", "create", {"cwd": "/example"})
    assert fresh
    first.save({**receipt, "threadId": "known-id"})
    first.close()
    second = Ledger(path)
    try:
        fresh, receipt = second.begin("key", "create", {"cwd": "/example"})
        assert not fresh and receipt["threadId"] == "known-id"
        assert receipt["status"] == "in_progress_or_unknown"
        assert path.stat().st_mode & 0o777 == 0o600
    finally:
        second.close()


async def test_goal_read_validates_id_and_bounds_text_without_mutation(bridge, fake_server):
    fake, _ = fake_server
    with pytest.raises(ValueError, match="thread_id"):
        await bridge.get_goal(" ")
    assert not fake.calls
    objective = "exact objective\nwith whitespace  "
    fake.goal = {"objective": objective, "status": "active"}
    assert (await bridge.get_goal("thread-1"))["goal"]["objective"] == objective
    fake.goal = {"objective": "x" * 5000, "status": "active"}
    result = await bridge.get_goal("thread-1")
    assert result["goal"]["objective"] == (
        "x" * 4000 + "\n[truncated; original length 5000 characters]"
    )
    assert result["goal"]["status"] == "active"
    assert [name for name, _ in fake.calls if name not in {"initialize", "initialized"}] == [
        "thread/goal/get",
        "thread/goal/get",
    ]


@pytest.mark.parametrize(
    "kwargs",
    [
        {"cwd": "relative"},
        {"cwd": "/does-not-exist-ctb"},
        {"prompt": " "},
        {"sandbox": "made-up"},
        {"request_id": ""},
    ],
)
async def test_invalid_create_has_no_api_effects(bridge, fake_server, tmp_path, kwargs):
    fake, _ = fake_server
    params = {"request_id": "valid", "cwd": str(tmp_path), **kwargs}
    with pytest.raises(ValueError):
        await create(bridge, **params)
    assert not fake.calls
