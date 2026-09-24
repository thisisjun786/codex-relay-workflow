"""Approval-class requests are left for the thread's own approver (CRW-225).

Measured on codex-cli 0.154.0 (README "Approval requests, as measured"): the host sends every
approval request to each connection subscribed to the thread, replays a pending one to a connection
that resumes the thread later, and applies the FIRST answer from any of them, an error answer as a
denial. A bridge that answers such a request therefore decides it for the approver, and a bridge
that merely resumes a thread whose own turn waits for approval denies the owner's request. So the
bridge answers none of them and records each one it left.

Each test is labelled RED where it fails on the pre-change bridge, which answered every server
request with -32601, or GREEN where it pins behaviour that must not change.
"""

import pytest
from conftest import EXECUTION

from codex_thread_bridge.rpc import APPROVAL_METHODS, REFUSED_REQUESTS_KEPT

# Pinned from the codex-cli 0.154.0 schema (ServerRequest.json: the requests that ask a human to
# decide), not read from the implementation, so dropping one from APPROVAL_METHODS fails here
# instead of quietly shrinking what is tested.
METHODS = sorted([
    "applyPatchApproval",
    "execCommandApproval",
    "item/commandExecution/requestApproval",
    "item/fileChange/requestApproval",
    "item/permissions/requestApproval",
    "item/tool/requestUserInput",
    "mcpServer/elicitation/request",
])


def test_the_left_set_is_the_schema_set():
    """GREEN: the implementation's set equals the pinned schema set."""
    assert sorted(APPROVAL_METHODS) == METHODS


async def create(bridge, *args, **kwargs):
    return await bridge.create_thread(*args, **{**EXECUTION, **kwargs})


async def send(bridge, request_id, thread_id, message, **kwargs):
    carried = {**EXECUTION, **(kwargs.pop("expected_settings", None) or {})}
    return await bridge.send_message_to_thread(request_id, thread_id, message, carried, **kwargs)


async def settle(bridge, thread_id):
    """One more request/response round trip. The fake reads frames in order, so an answer the
    bridge wrote for an earlier server request is on the fake's side once this returns; without
    it a "no answer" assertion could pass only because the answer had not arrived yet."""
    await bridge.read_thread(thread_id)


async def on_request_thread(bridge, fake, tmp_path):
    created = await create(bridge, "c", str(tmp_path))
    fake.approval_policy = "on-request"
    return created["threadId"]


@pytest.mark.parametrize("method", METHODS)
async def test_a_request_raised_during_the_turn_is_left_for_the_approver(
    bridge, fake_server, tmp_path, method
):
    """RED: every one of these was answered with -32601, which the host applies as a denial."""
    fake, _ = fake_server
    thread_id = await on_request_thread(bridge, fake, tmp_path)
    fake.approval_request_on_turn = method
    result = await send(
        bridge, "m-" + method.replace("/", "-"), thread_id, "report",
        expected_settings={"approval_policy": "on-request"},
    )
    await settle(bridge, thread_id)
    assert result["status"] == "accepted"
    [(ident, raised)] = fake.server_requests
    assert raised == method
    assert fake.answer_to(ident) == [], "the bridge answered a decision that is the approver's"
    entries = result["approvalRequests"]["thisThread"]
    assert [(e["method"], e["answered"]) for e in entries] == [
        (method, "left_for_thread_approver")
    ]
    assert result["approvals"]["onApprovalRequest"] == "left_for_thread_approver"
    assert result["approvals"]["servicedByThisBridge"] is False


@pytest.mark.parametrize("method", METHODS)
async def test_a_request_replayed_on_resume_is_left_for_the_approver(
    bridge, fake_server, tmp_path, method
):
    """RED: the owner's own pending request, replayed to the bridge by its resume, was denied."""
    fake, _ = fake_server
    thread_id = await on_request_thread(bridge, fake, tmp_path)
    fake.approval_request_on_resume = method
    await send(
        bridge, "r-" + method.replace("/", "-"), thread_id, "report",
        expected_settings={"approval_policy": "on-request"},
    )
    await settle(bridge, thread_id)
    [(ident, raised)] = fake.server_requests
    assert raised == method
    assert fake.answer_to(ident) == [], "the bridge denied the owner's pending request"


async def test_a_client_side_tool_request_is_still_refused(bridge, fake_server, tmp_path):
    """GREEN: a request that is not a decision keeps its explicit -32601; nothing is faked."""
    fake, _ = fake_server
    thread_id = await on_request_thread(bridge, fake, tmp_path)
    fake.approval_request_on_turn = "item/tool/call"
    result = await send(
        bridge, "tool", thread_id, "report", expected_settings={"approval_policy": "on-request"}
    )
    await settle(bridge, thread_id)
    [(ident, _method)] = fake.server_requests
    [answer] = fake.answer_to(ident)
    assert answer["error"]["code"] == -32601
    assert [e["answered"] for e in result["approvalRequests"]["thisThread"]] == ["refused"]


async def test_interleaved_requests_are_all_reported_with_what_happened(
    bridge, fake_server, tmp_path
):
    """RED: the receipt window counted refusals only, so a left request had no place in it."""
    fake, _ = fake_server
    thread_id = await on_request_thread(bridge, fake, tmp_path)
    fake.approval_request_on_turn = [
        "item/commandExecution/requestApproval",
        "item/tool/call",
        "item/fileChange/requestApproval",
    ]
    result = await send(
        bridge, "mixed", thread_id, "report", expected_settings={"approval_policy": "on-request"}
    )
    await settle(bridge, thread_id)
    report = result["approvalRequests"]
    assert [(e["method"], e["answered"]) for e in report["thisThread"]] == [
        ("item/commandExecution/requestApproval", "left_for_thread_approver"),
        ("item/tool/call", "refused"),
        ("item/fileChange/requestApproval", "left_for_thread_approver"),
    ]
    assert report["approvalsLeftForThisThread"] == 2
    assert report["refusedForThisThread"] == 1
    assert report["notRetained"] == 0
    answered = {answer["id"] for answer in fake.client_answers}
    assert answered == {fake.server_requests[1][0]}


async def test_requests_beyond_the_kept_bound_are_counted_not_dropped(
    bridge, fake_server, tmp_path
):
    """RED: past the bound the deque forgot entries and the receipt could not say so."""
    fake, _ = fake_server
    thread_id = await on_request_thread(bridge, fake, tmp_path)
    over = REFUSED_REQUESTS_KEPT + 5
    fake.approval_request_on_turn = ["item/commandExecution/requestApproval"] * over
    result = await send(
        bridge, "many", thread_id, "report", expected_settings={"approval_policy": "on-request"}
    )
    await settle(bridge, thread_id)
    report = result["approvalRequests"]
    assert len(report["thisThread"]) == REFUSED_REQUESTS_KEPT
    assert report["notRetained"] == 5
    assert fake.client_answers == []


async def test_the_refusal_stream_keeps_its_old_meaning(bridge, fake_server, tmp_path):
    """RED (Devin, PR #157): refusals_since is a compatibility name. It reports refusals only,
    with its old count key, so a caller still on it never reads a left request as a refusal."""
    fake, _ = fake_server
    thread_id = await on_request_thread(bridge, fake, tmp_path)
    mark = bridge.rpc.refusal_mark()
    fake.approval_request_on_turn = ["item/commandExecution/requestApproval", "item/tool/call"]
    await send(bridge, "compat", thread_id, "report", expected_settings={"approval_policy": "on-request"})
    await settle(bridge, thread_id)
    refusals = bridge.rpc.refusals_since(mark, thread_id)
    assert [e["method"] for e in refusals["thisThread"]] == ["item/tool/call"]
    assert refusals["approvalsRefusedForThisThread"] == 0


async def test_a_burst_of_approvals_does_not_evict_an_earlier_refusal(bridge, fake_server, tmp_path):
    """RED (Devin, PR #157): left approvals shared the refusal buffer, so more of them than its
    bound pushed an earlier refusal out of the compatibility view that used to keep it."""
    fake, _ = fake_server
    thread_id = await on_request_thread(bridge, fake, tmp_path)
    mark = bridge.rpc.refusal_mark()
    fake.approval_request_on_turn = ["item/tool/call"] + [
        "item/commandExecution/requestApproval"] * (REFUSED_REQUESTS_KEPT + 3)
    result = await send(bridge, "burst", thread_id, "report",
                        expected_settings={"approval_policy": "on-request"})
    await settle(bridge, thread_id)
    refusals = bridge.rpc.refusals_since(mark, thread_id)
    assert [e["method"] for e in refusals["thisThread"]] == ["item/tool/call"]
    report = result["approvalRequests"]
    assert report["notRetained"] == 4, "the inclusive stream still reports its own gap"
