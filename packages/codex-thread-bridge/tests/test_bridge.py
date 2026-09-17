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


# --- CRW-4: instructing a peer whose turn is already running ---------------------------------


async def running(bridge, fake, tmp_path, request_id="create"):
    """A thread with a turn the host still reports in progress, as a steer target needs."""
    fake.complete_turns = False
    created = await create(bridge, request_id, str(tmp_path), prompt="work")
    fake.threads[created["threadId"]]["status"] = {"type": "active", "activeFlags": []}
    return created["threadId"]


async def test_active_turn_is_derived_from_the_newest_in_progress_turn(
    bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    thread_id = await running(bridge, fake, tmp_path)
    observed = await bridge.active_turn(thread_id)
    assert observed["observation"] == "active" and observed["steerable"]
    assert observed["activeTurnId"] == "turn-1"
    # The status itself carries no turn id; the id is derived and never resumes the thread.
    assert "turnId" not in observed["status"]


async def test_active_turn_reports_idle_and_a_thread_with_no_turns(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    created = await create(bridge, "create", str(tmp_path), prompt="done")
    idle = await bridge.active_turn(created["threadId"])
    assert idle["observation"] == "idle" and idle["activeTurnId"] is None
    assert idle["newestTurnId"] == "turn-1" and not idle["steerable"]

    empty = await create(bridge, "empty", str(tmp_path))
    blank = await bridge.active_turn(empty["threadId"])
    assert blank["activeTurnId"] is None and blank["newestTurnId"] is None


@pytest.mark.parametrize("kind", ["notLoaded", "systemError"])
async def test_active_turn_reports_non_runnable_status_without_raising(
    bridge, fake_server, tmp_path, kind
):
    fake, _ = fake_server
    created = await create(bridge, "create", str(tmp_path), prompt="work")
    fake.threads[created["threadId"]]["status"] = {"type": kind}
    observed = await bridge.active_turn(created["threadId"])
    assert observed["observation"] == kind and observed["activeTurnId"] is None
    assert not observed["steerable"]


async def test_active_turn_names_a_disagreement_between_status_and_turns(
    bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    # The status still says active while the newest turn has finished: the turn ended between
    # the two reads, and the caller must read again rather than steer a finished turn.
    created = await create(bridge, "create", str(tmp_path), prompt="work")
    fake.threads[created["threadId"]]["status"] = {"type": "active", "activeFlags": []}
    stale = await bridge.active_turn(created["threadId"])
    assert stale["observation"] == "active_without_in_progress_turn"
    assert stale["activeTurnId"] is None and not stale["steerable"]

    # And the reverse: a turn is running while the status has not caught up.
    other = await running(bridge, fake, tmp_path, request_id="other")
    fake.threads[other]["status"] = {"type": "idle"}
    behind = await bridge.active_turn(other)
    assert behind["observation"] == "in_progress_turn_without_active_status"


async def test_steer_reaches_the_guarded_turn_and_claims_only_acceptance(
    bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    thread_id = await running(bridge, fake, tmp_path)
    before = len(fake.calls)
    receipt = await bridge.steer_thread("steer", thread_id, "turn-1", "narrow the scope")

    assert receipt["status"] == "accepted"
    assert receipt["delivery"] == "accepted_not_applied"
    assert receipt["steeredTurnId"] == "turn-1"
    # Acceptance is not a read and not an effect; the receipt has to say so itself.
    assert "does not say the peer read it" in receipt["deliveryMeaning"]
    assert "does not say the peer acted on it" in receipt["deliveryMeaning"]
    # Nothing is observable without a resume, and that must not read like nothing was asked for.
    assert receipt["settings"]["verification"] == "not_observable"

    steered = next(p for name, p in fake.calls if name == "turn/steer")
    assert steered["expectedTurnId"] == "turn-1"
    assert steered["input"] == [{"type": "text", "text": "narrow the scope"}]
    # Steering joins a running turn: it resumes nothing, starts nothing, and touches no goal.
    after = [name for name, _ in fake.calls[before:]]
    assert "thread/resume" not in after and "turn/start" not in after
    assert not any(name.startswith("thread/goal/") for name in after)
    assert "config" not in steered and "model" not in steered


@pytest.mark.parametrize(
    ("kind", "code"),
    [
        ("idle", "thread_idle"),
        ("notLoaded", "thread_not_loaded"),
        ("systemError", "thread_system_error"),
    ],
)
async def test_steer_refuses_each_non_active_status_by_name(
    bridge, fake_server, tmp_path, kind, code
):
    fake, _ = fake_server
    created = await create(bridge, "create", str(tmp_path), prompt="work")
    fake.threads[created["threadId"]]["status"] = {"type": kind}
    receipt = await bridge.steer_thread("steer", created["threadId"], "turn-1", "stop")
    assert receipt["status"] == "failed"
    assert receipt["rpcError"]["code"] == code
    assert fake.count("turn/steer") == 0
    # Only an idle peer is sent to the message path; a system error is not an idle peer.
    recommends_message = "send_message_to_thread" in receipt["rpcError"]["message"]
    assert recommends_message is (kind == "idle")


async def test_steer_with_a_stale_turn_id_fails_and_does_not_retarget(
    bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    thread_id = await running(bridge, fake, tmp_path)
    fake.reject["turn/steer"] = {"code": "expected_turn_mismatch", "message": "turn moved on"}
    receipt = await bridge.steer_thread("steer", thread_id, "turn-0", "late instruction")
    assert receipt["status"] == "failed"
    # The host's own rejection is retained; the bridge invents no vocabulary and no fallback.
    assert receipt["rpcError"]["code"] == "expected_turn_mismatch"
    assert "delivery" not in receipt


async def test_a_turn_id_we_did_not_guard_is_a_failure_not_an_acceptance(
    bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    thread_id = await running(bridge, fake, tmp_path)
    fake.steer_turn_id = "turn-99"
    receipt = await bridge.steer_thread("steer", thread_id, "turn-1", "scope change")
    # The guarded target was never established, so nothing may report delivery.
    assert receipt["status"] == "failed"
    assert receipt["rpcError"]["code"] == "steered_turn_mismatch"
    assert receipt["steeredTurnId"] == "turn-99" and receipt["expectedTurnId"] == "turn-1"
    assert receipt.get("delivery") != "accepted_not_applied"


async def test_an_uncertain_steer_replays_and_is_reconciled_by_record(
    bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    thread_id = await running(bridge, fake, tmp_path)
    receipt = await bridge.steer_thread("steer-once", thread_id, "turn-1", "one instruction")
    sent = next(p for name, p in fake.calls if name == "turn/steer")
    # The host records the instruction under this id, so a lost response is settled by reading
    # what the host kept rather than by sending the instruction a second time.
    assert sent["clientUserMessageId"] == "steer:steer-once"
    assert receipt["clientUserMessageId"] == "steer:steer-once"

    replay = await bridge.steer_thread("steer-once", thread_id, "turn-1", "one instruction")
    assert replay["replayed"] and fake.count("turn/steer") == 1
    assert bridge.ledger.get("steer-once")["clientUserMessageId"] == "steer:steer-once"


async def test_the_same_request_id_cannot_be_aimed_at_another_turn(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    thread_id = await running(bridge, fake, tmp_path)
    await bridge.steer_thread("steer", thread_id, "turn-1", "first")
    with pytest.raises(ValueError):
        await bridge.steer_thread("steer", thread_id, "turn-2", "first")
    assert fake.count("turn/steer") == 1


async def test_steer_reports_an_unsupported_method_without_claiming_a_host_wide_gap(
    bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    thread_id = await running(bridge, fake, tmp_path)
    fake.reject["turn/steer"] = {"code": -32601, "message": "turn/steer"}
    before = len(fake.calls)
    receipt = await bridge.steer_thread("steer", thread_id, "turn-1", "instruction")
    assert receipt["status"] == "failed" and receipt["rpcError"]["code"] == -32601
    # No fallback to messaging or interrupting, and no claim about hosts in general.
    after = [name for name, _ in fake.calls[before:]]
    assert "thread/resume" not in after and "turn/start" not in after


@pytest.mark.parametrize("message", ["", "   ", "x" * 100_001])
async def test_steer_rejects_empty_and_oversized_input_before_any_call(
    bridge, fake_server, tmp_path, message
):
    fake, _ = fake_server
    thread_id = await running(bridge, fake, tmp_path)
    count = len(fake.calls)
    with pytest.raises(ValueError):
        await bridge.steer_thread("steer", thread_id, "turn-1", message)
    assert len(fake.calls) == count


async def test_capabilities_keep_bridge_exposure_and_host_support_apart(bridge, fake_server):
    fake, _ = fake_server
    reported = await bridge.capabilities()
    # The bridge exposes steering and pausing, and withholds interrupt and the turn queue.
    assert reported["exposure"]["steerActiveTurn"] and reported["exposure"]["goalPause"]
    assert reported["exposure"]["turnInterrupt"] is False
    assert reported["exposure"]["goalObjectiveWrite"] is False
    # The fake server is not the tested host, so host support is unknown rather than inherited
    # from this bridge's own tool list.
    assert reported["hostSupport"]["state"] == "unknown_host_version"
    assert reported["hostSupport"]["observedServer"] == "fake Codex/0.153.4"
    assert not any(name.startswith("turn/") for name, _ in fake.calls)


# --- CRW-4: pausing a goal, which is not the same as stopping a turn --------------------------


async def test_pause_sets_status_only_and_never_claims_the_turn_stopped(
    bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    thread_id = await running(bridge, fake, tmp_path)
    fake.goal = {"objective": "original objective", "status": "active", "tokenBudget": 100}
    receipt = await bridge.pause_goal("pause", thread_id)

    assert receipt["status"] == "accepted" and receipt["delivery"] == "applied_by_host"
    assert receipt["goalAfter"]["status"] == "paused"
    assert receipt["goalAfter"]["objective"] == "original objective"
    sent = next(p for name, p in fake.calls if name == "thread/goal/set")
    # Sending the objective back would make a pause an objective write, and would restore a
    # stale objective if someone edited it in between.
    assert sent == {"threadId": thread_id, "status": "paused"}
    assert receipt["pause"] == "goal_paused_turn_may_still_be_running"
    assert receipt["concurrency"] == "no_host_precondition_for_goal_status"


async def test_pause_then_steer_the_observed_turn_to_finish_safely(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    thread_id = await running(bridge, fake, tmp_path)
    fake.goal = {"objective": "keep going", "status": "active", "tokenBudget": None}
    paused = await bridge.pause_goal("pause", thread_id)
    assert paused["status"] == "accepted"

    # Pausing left the turn running, so stopping work needs the observed turn steered.
    observed = await bridge.active_turn(thread_id)
    assert observed["observation"] == "active" and observed["activeTurnId"] == "turn-1"
    finished = await bridge.steer_thread(
        "finish", thread_id, observed["activeTurnId"], "Finish the current step safely and stop."
    )
    assert finished["status"] == "accepted"
    order = [name for name, _ in fake.calls if name in {"thread/goal/set", "turn/steer"}]
    assert order == ["thread/goal/set", "turn/steer"]


async def test_pause_refuses_a_thread_with_no_goal(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    created = await create(bridge, "create", str(tmp_path), prompt="work")
    receipt = await bridge.pause_goal("pause", created["threadId"])
    assert receipt["status"] == "failed" and receipt["rpcError"]["code"] == "no_goal"
    assert fake.count("thread/goal/set") == 0


async def test_an_already_paused_goal_is_not_written_again(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    created = await create(bridge, "create", str(tmp_path), prompt="work")
    fake.goal = {"objective": "o", "status": "paused"}
    receipt = await bridge.pause_goal("pause", created["threadId"])
    assert receipt["status"] == "accepted" and receipt["pause"] == "already_paused"
    assert receipt["delivery"] == "no_change"
    assert fake.count("thread/goal/set") == 0


@pytest.mark.parametrize("status", ["complete", "blocked", "usageLimited", "budgetLimited"])
async def test_pause_refuses_a_goal_that_is_not_active(bridge, fake_server, tmp_path, status):
    fake, _ = fake_server
    created = await create(bridge, "create", str(tmp_path), prompt="work")
    fake.goal = {"objective": "o", "status": status}
    receipt = await bridge.pause_goal("pause", created["threadId"])
    # A goal that already ended is never quietly reopened as paused.
    assert receipt["status"] == "failed" and receipt["rpcError"]["code"] == "goal_not_active"
    assert status in receipt["rpcError"]["message"]
    assert fake.count("thread/goal/set") == 0


@pytest.mark.parametrize(
    ("moved", "code"),
    [
        ({"objective": "someone else rewrote this"}, "goal_changed_under_pause"),
        ({"tokenBudget": 999}, "goal_changed_under_pause"),
        ({"status": "active"}, "goal_not_paused"),
    ],
)
async def test_a_goal_that_moved_under_the_pause_is_a_known_failure(
    bridge, fake_server, tmp_path, moved, code
):
    fake, _ = fake_server
    created = await create(bridge, "create", str(tmp_path), prompt="work")
    fake.goal = {"objective": "original", "status": "active", "tokenBudget": 10}
    fake.goal_after_set = moved
    receipt = await bridge.pause_goal("pause", created["threadId"])
    # The set demonstrably happened, so this is a failure with a known outcome, never the
    # outcome_unknown a bare exception would have produced.
    assert receipt["status"] == "failed" and receipt["rpcError"]["code"] == code
    assert receipt["goalBefore"]["objective"] == "original"
    assert receipt["goalAfter"] and receipt.get("delivery") != "applied_by_host"


async def test_a_lost_steer_response_is_unknown_and_never_sent_again(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    thread_id = await running(bridge, fake, tmp_path)
    # The host applies the steer and the response never arrives, which is the case a blind
    # resend would turn into two instructions in one turn.
    fake.drop_after = "turn/steer"
    lost = await bridge.steer_thread("lost-steer", thread_id, "turn-1", "one instruction")
    assert lost["status"] == "outcome_unknown"
    # The correlation id is retained, so the turn's own record settles what happened.
    assert lost["clientUserMessageId"] == "steer:lost-steer"

    fake.drop_after = None
    replay = await bridge.steer_thread("lost-steer", thread_id, "turn-1", "one instruction")
    assert replay["replayed"] and replay["status"] == "outcome_unknown"
    assert fake.count("turn/steer") == 1


async def test_a_lost_pause_response_is_unknown_and_never_sent_again(bridge, fake_server, tmp_path):
    fake, _ = fake_server
    created = await create(bridge, "create", str(tmp_path), prompt="work")
    fake.goal = {"objective": "o", "status": "active", "tokenBudget": None}
    fake.drop_after = "thread/goal/set"
    lost = await bridge.pause_goal("lost-pause", created["threadId"])
    assert lost["status"] == "outcome_unknown"

    fake.drop_after = None
    replay = await bridge.pause_goal("lost-pause", created["threadId"])
    assert replay["replayed"] and fake.count("thread/goal/set") == 1


def test_a_version_is_identified_by_token_not_by_substring():
    from codex_thread_bridge.bridge import TESTED_HOST_VERSION, host_versions

    real = "Codex Desktop/0.154.0 (Ubuntu 26.4.0; x86_64) unknown (codex_thread_bridge; 0.1.0)"
    assert TESTED_HOST_VERSION in host_versions(real)
    # A longer version that merely contains the tested one is not the tested host.
    assert TESTED_HOST_VERSION not in host_versions("Codex Desktop/10.154.0 (linux)")
    assert host_versions("codex-cli 10.154.0") == set()
    assert host_versions("") == set()


def test_a_prerelease_build_is_not_the_tested_host():
    from codex_thread_bridge.bridge import TESTED_HOST_VERSION, host_versions

    # A prerelease or build-metadata token is a different build from the tested release, so
    # hostSupport must not record it as tested. Stopping the token at the suffix would read
    # 0.154.0-alpha.1 as the release and answer the exposure-versus-support question wrongly.
    for suffix in ("-alpha.1", "-rc.2", "+build.5", "-alpha.1+build.5"):
        agent = f"codex_web_agent/{TESTED_HOST_VERSION}{suffix}"
        assert host_versions(agent) == {f"{TESTED_HOST_VERSION}{suffix}"}
        assert TESTED_HOST_VERSION not in host_versions(agent)

    # The tested release itself still identifies, including beside the bridge's own version.
    real = (
        f"Codex Desktop/{TESTED_HOST_VERSION} (Ubuntu 26.4.0; x86_64) "
        "unknown (codex_thread_bridge; 0.1.0)"
    )
    assert TESTED_HOST_VERSION in host_versions(real)
