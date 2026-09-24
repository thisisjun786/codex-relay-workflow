"""Settings that must survive creation and resume, and the causes that must stay apart.

Grounded in behaviour measured against a real App Server (codex-cli 0.154.0): a start carrying
config.model_reasoning_effort came back reporting that effort; sandbox came back as the full
policy object with every default filled in; all four workspace-write policy fields travelled in
the config; read-only network access travelled by no spelling at all; and a resume reported the
thread's real state rather than adopting an override.

Each test is labelled RED where it fails on the pre-change bridge, or COMPATIBILITY where it
pins behaviour that was already correct. Several are both: they assert an unchanged wire format
alongside the new settings receipt, so they fail before the change for the receipt alone.
Measured on the pre-change tree: 21 of these 22 fail, and the one that passes is the
fingerprint-replay compatibility test, which must pass on both sides.
"""

import json

import pytest
from conftest import EFFORT, EXECUTION, MODEL

from codex_thread_bridge.execution import ExecutionRefused
from codex_thread_bridge.settings import (
    SettingsContract,
    UntransmittableSetting,
    normalise_policy,
)

WRITE_POLICY = {
    "type": "workspaceWrite",
    "writableRoots": [],
    "networkAccess": False,
    "excludeTmpdirEnvVar": False,
    "excludeSlashTmp": False,
}


def sent(fake, method):
    return next(params for name, params in fake.calls if name == method)


# Every mutation must now state its model and reasoning effort, so these helpers supply the
# approved pair once instead of at forty call sites. Tests about the guard itself call
# bridge.create_thread and bridge.send_message_to_thread directly, with the pair left out,
# blank or unapproved on purpose.
async def create(bridge, *args, **kwargs):
    return await bridge.create_thread(*args, **{**EXECUTION, **kwargs})


async def send(bridge, request_id, thread_id, message, **kwargs):
    carried = {**EXECUTION, **(kwargs.pop("expected_settings", None) or {})}
    return await bridge.send_message_to_thread(request_id, thread_id, message, carried, **kwargs)


# --------------------------------------------------------------------------- creation


async def test_effort_is_transmitted_in_config_and_confirmed(bridge, fake_server, tmp_path):
    """RED: create_thread had no reasoning_effort parameter at all."""
    fake, _ = fake_server
    result = await create(
        bridge, "effort", str(tmp_path), model="anthropic/claude-opus-5", reasoning_effort="xhigh"
    )
    assert sent(fake, "thread/start")["config"] == {"model_reasoning_effort": "xhigh"}
    assert result["status"] == "accepted"
    settings = result["settings"]
    assert settings["actual"]["reasoningEffort"] == "xhigh"
    assert settings["actual"]["model"] == "anthropic/claude-opus-5"
    assert settings["verification"] == "observed_at_creation"
    assert settings["findings"] == []


async def test_first_full_request_can_use_opus_and_xhigh(bridge, fake_server, tmp_path):
    """RED: the first call that carries a prompt reaches turn/start at the requested settings.

    This is the shape JUN-102 asks for: real work on the first request, not a readiness-only
    turn used to work around a setting that could not be sent.
    """
    fake, _ = fake_server
    result = await create(
        bridge,
        "opus",
        str(tmp_path),
        prompt="Do the actual work now.",
        model="anthropic/claude-opus-5",
        reasoning_effort="xhigh",
    )
    assert result["status"] == "accepted" and result["turnId"]
    assert sent(fake, "turn/start")["input"][0]["text"] == "Do the actual work now."
    assert result["settings"]["actual"]["reasoningEffort"] == "xhigh"


async def test_a_swapped_model_is_not_preserved_and_withholds_the_prompt(
    bridge, fake_server, tmp_path
):
    """RED: the old create_thread never compared the returned model at all."""
    fake, _ = fake_server
    fake.override_creation = {"model": "some-other-model"}
    result = await create(
        bridge, "swap", str(tmp_path), prompt="hello", model="anthropic/claude-opus-5"
    )
    assert result["status"] == "failed"
    assert result["rpcError"]["code"] == "settings_not_preserved"
    assert result["settings"]["findings"][0]["field"] == "model"
    assert fake.count("turn/start") == 0, "the prompt must be withheld"


@pytest.mark.parametrize("missing", ["reasoningEffort", "model"])
async def test_a_setting_the_host_never_reports_is_unobservable_not_a_mismatch(
    bridge, fake_server, tmp_path, missing
):
    """RED: an unreported setting is its own cause, and it withholds rather than warns."""
    fake, _ = fake_server
    fake.unreported = {missing}
    result = await create(
        bridge,
        "silent",
        str(tmp_path),
        prompt="hello",
        model="anthropic/claude-opus-5",
        reasoning_effort="xhigh",
    )
    assert result["status"] == "failed"
    assert result["rpcError"]["code"] == "setting_unobservable"
    assert result["settings"]["unobservable"] == [missing]
    assert fake.count("turn/start") == 0


async def test_an_explicit_null_reads_the_same_as_an_absent_field(bridge, fake_server, tmp_path):
    """RED: null says no more about what was applied than a missing key does."""
    fake, _ = fake_server
    fake.override_creation = {"reasoningEffort": None}
    result = await create(
        bridge, "null-effort", str(tmp_path), prompt="hello", reasoning_effort="xhigh"
    )
    assert result["rpcError"]["code"] == "setting_unobservable"
    assert result["settings"]["unobservable"] == ["reasoningEffort"]


async def test_every_workspace_write_policy_field_is_serialised_into_config(
    bridge, fake_server, tmp_path
):
    """RED: the mode string cannot carry roots or the network flag; the config can."""
    fake, _ = fake_server
    root = str(tmp_path / "extra")
    policy = {
        "type": "workspaceWrite",
        "writableRoots": [root],
        "networkAccess": True,
        "excludeTmpdirEnvVar": True,
        "excludeSlashTmp": True,
    }
    result = await create(
        bridge, "policy", str(tmp_path), sandbox="workspace-write", expected_sandbox_policy=policy
    )
    assert sent(fake, "thread/start")["config"]["sandbox_workspace_write"] == {
        "writable_roots": [root],
        "network_access": True,
        "exclude_tmpdir_env_var": True,
        "exclude_slash_tmp": True,
    }
    assert result["status"] == "accepted"
    assert result["settings"]["actual"]["sandbox"] == policy


async def test_a_default_valued_policy_field_is_still_transmitted(bridge, fake_server, tmp_path):
    """RED: a host configured the other way is only overridden by actually sending the value.

    Sending nothing and comparing afterwards would discover the conflict and never resolve it.
    """
    fake, _ = fake_server
    await create(
        bridge,
        "defaults",
        str(tmp_path),
        sandbox="workspace-write",
        expected_sandbox_policy=dict(WRITE_POLICY),
    )
    assert sent(fake, "thread/start")["config"]["sandbox_workspace_write"] == {
        "writable_roots": [],
        "network_access": False,
        "exclude_tmpdir_env_var": False,
        "exclude_slash_tmp": False,
    }


async def test_read_only_network_access_is_refused_before_any_request(
    bridge, fake_server, tmp_path
):
    """RED: no config spelling moved read-only network access on the real host.

    A local limit must not be dressed up as a host disagreement, and it must cost no RPC.
    """
    fake, _ = fake_server
    with pytest.raises(UntransmittableSetting) as raised:
        await create(
            bridge,
            "ro-net",
            str(tmp_path),
            expected_sandbox_policy={"type": "readOnly", "networkAccess": True},
        )
    assert raised.value.code == "setting_untransmittable"
    assert raised.value.field == "sandbox.networkAccess"
    assert fake.calls == [], "nothing may reach the host"


async def test_filled_protocol_defaults_are_not_read_as_a_difference(bridge, tmp_path):
    """RED: the host fills every default in, and a bare expected type must still match."""
    result = await create(
        bridge,
        "bare",
        str(tmp_path),
        sandbox="workspace-write",
        expected_sandbox_policy=dict(WRITE_POLICY),
    )
    assert result["status"] == "accepted"
    assert normalise_policy({"type": "workspaceWrite"}) == WRITE_POLICY


async def test_the_receipt_never_claims_more_than_the_observation(bridge, tmp_path):
    """RED: nothing says "verified"; the claim is scoped to the observation point."""
    result = await create(bridge, "claim", str(tmp_path), reasoning_effort="xhigh")
    settings = result["settings"]
    assert settings["verification"] == "observed_at_creation"
    assert "verified" not in settings["verification"].split("_at_")[0].replace("observed", "")
    assert "no host-side exclusivity" in settings["observationLimits"]


async def test_only_the_required_pair_is_requested_when_nothing_else_is(
    bridge, fake_server, tmp_path
):
    """RED for the receipt, COMPATIBILITY for the wire.

    The previous contract was that a creation asking for nothing transmitted nothing and reported
    the host's own model as the actual value without claiming anything about it. Model and effort
    can no longer be left unasked. Roots and the sandbox policy still can, and they keep exactly
    the old behaviour: reported, never claimed about.
    """
    fake, _ = fake_server
    result = await create(bridge, "plain", str(tmp_path))
    start = sent(fake, "thread/start")
    assert start["model"] == MODEL
    assert start["config"] == {"model_reasoning_effort": EFFORT}
    assert "runtimeWorkspaceRoots" not in start
    settings = result["settings"]
    # cwd and sandbox are inherent to creating a thread; model and effort are now required. Roots
    # were not asked for, so they are reported as the host's actual value and claimed about in no
    # way.
    assert sorted(settings["requested"]) == ["cwd", "model", "reasoningEffort", "sandbox"]
    assert settings["verification"] == "observed_at_creation"
    assert settings["verified"] == ["cwd", "model", "reasoningEffort", "sandbox"]
    assert settings["actual"]["model"] == MODEL
    assert "runtimeWorkspaceRoots" not in settings["requested"]
    assert settings["actual"]["runtimeWorkspaceRoots"] == [str(tmp_path)]


# ----------------------------------------------------------------------------- resume


def carried(**overrides):
    return {
        "sandbox": "workspace-write",
        "expected_sandbox_policy": dict(WRITE_POLICY),
        **overrides,
    }


async def test_a_resume_always_carries_the_authorized_pair(bridge, fake_server, tmp_path):
    """The old contract was that omitting expected_settings kept the resume byte-identical.

    That is deliberately retired. A turn on an existing thread costs what a new one costs, so a
    send without a stated pair is refused before the thread is even read, and a send with one
    carries it. test_bridge.py pins the same params independently of this file.
    """
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path))
    settled = len(fake.calls)
    with pytest.raises(ExecutionRefused) as raised:
        await bridge.send_message_to_thread("bare", created["threadId"], "hello")
    assert raised.value.code == "execution_setting_missing"
    assert fake.calls[settled:] == [], "a send with no stated pair must not reach the host"
    result = await send(bridge, "m", created["threadId"], "hello")
    assert sent(fake, "thread/resume") == {
        "threadId": created["threadId"],
        "excludeTurns": True,
        "model": MODEL,
        "config": {"model_reasoning_effort": EFFORT},
    }
    # The approval policy is declared, not sent. A resume that carried one would be asking to SET
    # the policy of a thread this bridge did not create, which is the one thing this path must
    # never do; preservation is a property of the request, not of how the host treats it.
    assert "approvalPolicy" not in sent(fake, "thread/resume")
    assert result["status"] == "accepted" and result["turnId"]
    assert result["settings"]["verification"] == "observed_at_resume"
    assert result["settings"]["requested"] == {"model": MODEL, "reasoningEffort": EFFORT}
    # Silence is reported as silence: the observed values are there, unclaimed.
    assert result["settings"]["actual"]["approvalPolicy"] == "never"


async def test_resume_carries_the_settings_it_can_express(bridge, fake_server, tmp_path):
    """RED: the old resume deliberately supplied no cwd, model, sandbox or reasoning at all."""
    fake, _ = fake_server
    created = await create(
        bridge,
        "c",
        str(tmp_path),
        sandbox="workspace-write",
        model="anthropic/claude-opus-5",
        reasoning_effort="xhigh",
        expected_sandbox_policy=dict(WRITE_POLICY),
    )
    await send(
        bridge,
        "m",
        created["threadId"],
        "hello",
        expected_settings=carried(
            cwd=str(tmp_path), model="anthropic/claude-opus-5", reasoning_effort="xhigh"
        ),
    )
    resume = sent(fake, "thread/resume")
    assert resume["sandbox"] == "workspace-write"
    assert "approvalPolicy" not in resume
    assert resume["cwd"] == str(tmp_path)
    assert resume["model"] == "anthropic/claude-opus-5"
    assert resume["config"]["model_reasoning_effort"] == "xhigh"


async def test_a_widened_sandbox_withholds_the_message_before_any_turn(
    bridge, fake_server, tmp_path
):
    """RED: this is the failure the whole issue exists for."""
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path), sandbox="workspace-write")
    fake.override_resume = {"sandbox": {"type": "dangerFullAccess"}}
    before = fake.count("turn/start")
    result = await send(bridge, "m", created["threadId"], "hello", expected_settings=carried())
    assert result["status"] == "failed"
    assert result["rpcError"]["code"] == "settings_not_preserved"
    assert result["settings"]["findings"][0]["field"] == "sandbox"
    assert fake.count("turn/start") == before, "no turn may start"


async def test_a_clean_resume_starts_its_turn_with_no_overrides(bridge, fake_server, tmp_path):
    """RED: turn/start reports only the turn, so the bridge binds nothing it cannot read back."""
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path), sandbox="workspace-write")
    result = await send(bridge, "m", created["threadId"], "hello", expected_settings=carried())
    assert result["status"] == "accepted"
    assert result["settings"]["verification"] == "observed_at_resume"
    assert set(sent(fake, "turn/start")) == {"threadId", "input"}


async def test_an_interactive_approval_policy_still_decides_alone(bridge, fake_server, tmp_path):
    """RED: a permanently closed channel must not be shadowed by a generic mismatch."""
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path), sandbox="workspace-write")
    fake.approval_policy = "on-request"
    fake.override_resume = {"sandbox": {"type": "dangerFullAccess"}}
    result = await send(bridge, "m", created["threadId"], "hello", expected_settings=carried())
    assert result["rpcError"]["code"] == "unsupported_approval_policy"
    assert len(result["settings"]["findings"]) == 1


# ------------------------------------------------------------------------- idempotency


async def test_a_reused_id_replays_without_dispatching_again(bridge, fake_server, tmp_path):
    """RED with settings supplied; the underlying replay rule is long-standing.

    A retry must never create new duplicate work.
    """
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path), sandbox="workspace-write")
    first = await send(bridge, "same", created["threadId"], "hello", expected_settings=carried())
    turns = fake.count("turn/start")
    again = await send(bridge, "same", created["threadId"], "hello", expected_settings=carried())
    assert again["replayed"] and again["turnId"] == first["turnId"]
    assert fake.count("turn/start") == turns


async def test_a_reused_id_with_changed_settings_is_rejected(bridge, tmp_path):
    """RED: supplied settings are part of the request identity."""
    created = await create(bridge, "c", str(tmp_path), sandbox="workspace-write")
    await send(bridge, "same", created["threadId"], "hello", expected_settings=carried())
    with pytest.raises(ValueError, match="different arguments"):
        await send(
            bridge,
            "same",
            created["threadId"],
            "hello",
            expected_settings=carried(model="openai/gpt-5.6-sol"),
        )


async def test_an_omitted_setting_keeps_the_pre_upgrade_fingerprint(bridge, tmp_path):
    """COMPATIBILITY: a receipt retained before this change still replays afterwards."""
    cwd = str(tmp_path)
    fingerprint = bridge.ledger._fingerprint(
        "retained",
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
    bridge.ledger.db.execute(
        "INSERT INTO operations VALUES (?, ?, ?)",
        (
            "retained",
            fingerprint,
            '{"requestId": "retained", "operation": "create_thread", "status": "accepted",'
            ' "threadId": "older-thread", "fingerprintVersion": 2}',
        ),
    )
    bridge.ledger.db.commit()
    # The pre-upgrade arguments carried no model and no effort, so the replay must not either.
    replayed = await bridge.create_thread("retained", cwd)
    assert replayed["replayed"] and replayed["threadId"] == "older-thread"


# -------------------------------------------------------------- post-acceptance annotation


async def test_the_annotation_records_what_the_thread_reported_after_dispatch(
    bridge, fake_server, tmp_path
):
    """RED: the only signal available about a concurrent change around dispatch."""
    fake, _ = fake_server
    result = await create(
        bridge, "annotated", str(tmp_path), prompt="hello", reasoning_effort="xhigh"
    )
    note = result["settingsAfterDispatch"]
    assert note["concurrentChange"] is False
    assert note["covers"] == ["cwd", "model", "reasoningEffort"]
    assert note["unobserved"] == []
    assert "neither sandbox nor approvalPolicy" in note["limit"]


async def test_a_field_missing_after_dispatch_is_unobserved_not_unchanged(
    bridge, fake_server, tmp_path
):
    """The diagnostic must not make the mistake the rest of this contract exists to prevent.

    A field that was observed before dispatch and is absent afterwards was not compared at all.
    Dropping it silently would leave it listed as covered while concurrentChange said false.
    """
    fake, _ = fake_server
    created = await create(
        bridge, "vanish", str(tmp_path), prompt="hello", model="anthropic/claude-opus-5"
    )
    assert created["settingsAfterDispatch"]["unobserved"] == []

    fake.threads[created["threadId"]]["model"] = None
    second = await create(
        bridge, "vanish-2", str(tmp_path), prompt="hello", model="anthropic/claude-opus-5"
    )
    fake.threads[second["threadId"]]["model"] = None
    note = await bridge._annotate_dispatch(
        dict(second, settingsAfterDispatch=None),
        SettingsContract(model="anthropic/claude-opus-5"),
    )
    after = note["settingsAfterDispatch"]
    assert "model" in after["unobserved"], "an absent field is not an unchanged one"
    assert "model" not in after["covers"]


async def test_a_failing_annotation_cannot_downgrade_an_accepted_turn(
    bridge, fake_server, tmp_path
):
    """RED: a diagnostic that could strand an acknowledged dispatch would be worse than none."""
    fake, _ = fake_server
    result = await create(bridge, "c", str(tmp_path), prompt="hello", reasoning_effort="xhigh")
    assert result["status"] == "accepted"

    fake.reject["thread/read"] = {"code": -32000, "message": "nope"}
    second = await create(
        bridge, "annot-fail", str(tmp_path), prompt="hello", reasoning_effort="xhigh"
    )
    assert second["status"] == "accepted" and second["turnId"]
    assert "settingsAfterDispatch" not in second
    assert bridge.ledger.get("annot-fail")["status"] == "accepted"


# ------------------------------------------------------ findings from the PR review


async def test_a_misspelled_setting_key_is_refused_not_discarded(bridge, fake_server, tmp_path):
    """A discarded key looks exactly like a setting that was never requested.

    The MCP schema admits any object, so `reasoningEffort` instead of `reasoning_effort` would
    otherwise request nothing: the resume carries no effort, nothing is compared, and the message
    goes out under a "not_requested" receipt while the caller believes the setting was enforced.
    """
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path), sandbox="workspace-write")
    before = fake.count("turn/start")
    with pytest.raises(ValueError, match="unknown keys"):
        await send(
            bridge,
            "typo",
            created["threadId"],
            "hello",
            expected_settings={"reasoningEffort": "xhigh"},
        )
    assert fake.count("turn/start") == before, "nothing may be dispatched"
    with pytest.raises(ValueError, match="unknown keys"):
        await send(
            bridge,
            "typo2",
            created["threadId"],
            "hello",
            expected_settings={"model": "anthropic/claude-opus-5", "sandboxPolicy": {}},
        )
    assert fake.count("turn/start") == before, "neither misspelling may be dispatched"


async def test_a_replay_answers_from_the_ledger_without_touching_the_host(
    bridge, fake_server, tmp_path
):
    """A retained receipt is the ledger's answer, and reading the host again would spoil it.

    It would make a replay wait on a server that may be offline, observe state from long after
    the dispatch, and overwrite the original annotation with that later observation.
    """
    fake, _ = fake_server
    first = await create(
        bridge, "replayed", str(tmp_path), prompt="hello", reasoning_effort="xhigh"
    )
    assert first["settingsAfterDispatch"]["concurrentChange"] is False
    calls_before = len(fake.calls)

    again = await create(
        bridge, "replayed", str(tmp_path), prompt="hello", reasoning_effort="xhigh"
    )
    assert again["replayed"]
    assert len(fake.calls) == calls_before, "a replay must issue no host call at all"
    assert again["settingsAfterDispatch"] == first["settingsAfterDispatch"]


async def test_a_replay_is_answered_even_when_the_host_has_gone_away(bridge, fake_server, tmp_path):
    """Offline recovery is the case the ledger exists for; it must not wait on a read."""
    fake, _ = fake_server
    first = await create(bridge, "offline", str(tmp_path), prompt="hello", reasoning_effort="xhigh")
    fake.reject["thread/read"] = {"code": -32000, "message": "server is gone"}
    again = await create(bridge, "offline", str(tmp_path), prompt="hello", reasoning_effort="xhigh")
    assert again["replayed"] and again["turnId"] == first["turnId"]
    assert again["settingsAfterDispatch"] == first["settingsAfterDispatch"]


async def test_a_cancelled_annotation_propagates_instead_of_completing(
    bridge, fake_server, tmp_path
):
    """The receipt is already durably accepted, so there is nothing to protect by swallowing it.

    Catching CancelledError here would let a cancelled request, or a shutdown, return as though
    it had finished normally.
    """
    import asyncio

    fake, _ = fake_server
    created = await create(bridge, "cancel-prep", str(tmp_path), prompt="hello")
    assert created["status"] == "accepted"

    receipt = {
        "requestId": "cancel-prep",
        "status": "accepted",
        "threadId": created["threadId"],
        "turnId": created["turnId"],
        "settings": {"actual": {}},
    }

    async def cancelled(_method, _params):
        raise asyncio.CancelledError

    bridge.rpc.call = cancelled
    contract = SettingsContract(model="anthropic/claude-opus-5")
    with pytest.raises(asyncio.CancelledError):
        await bridge._annotate_dispatch(receipt, contract)
    # The acceptance stays exactly as _mutate saved it.
    assert bridge.ledger.get("cancel-prep")["status"] == "accepted"


@pytest.mark.parametrize(
    "malformed",
    [
        "not-a-policy",
        42,
        ["workspaceWrite"],
        {"no_type": 1},
        # Nested malformation: normalise_policy used to raise on each of these, and an exception
        # before turn/start becomes outcome_unknown, the one verdict a delivery cannot reconcile.
        {"type": "workspaceWrite", "writableRoots": None},
        {"type": "workspaceWrite", "writableRoots": 7},
        {"type": "workspaceWrite", "writableRoots": {"a": 1}},
        {"type": {"unhashable": 1}},
        {"type": ["not-a-string"]},
    ],
)
async def test_an_unreadable_sandbox_answer_is_a_refusal_not_a_crash(
    bridge, fake_server, tmp_path, malformed
):
    """A host answer we cannot read is a value we disagree with, not an outcome we do not know.

    Indexing it would raise, and _mutate's generic handler would record outcome_unknown, which
    tells the caller the message MAY have been delivered when nothing was ever sent.
    """
    fake, _ = fake_server
    fake.override_creation = {"sandbox": malformed}
    result = await create(bridge, "unreadable", str(tmp_path), prompt="hello")
    assert result["status"] == "failed", "never outcome_unknown"
    assert result["rpcError"]["code"] == "settings_not_preserved"
    assert result["settings"]["findings"][0]["field"] == "sandbox"
    assert result["settings"]["actual"]["sandbox"] == malformed
    assert fake.count("turn/start") == 0


def test_normalise_policy_never_raises_and_answers_none_for_what_it_cannot_read():
    """Its callers run before turn/start, so raising would misreport a withheld message."""
    for unreadable in (
        None,
        "workspaceWrite",
        42,
        ["workspaceWrite"],
        {"no_type": 1},
        {"type": {"unhashable": 1}},
        {"type": ["not-a-string"]},
        {"type": "workspaceWrite", "writableRoots": None},
        {"type": "workspaceWrite", "writableRoots": 7},
    ):
        assert normalise_policy(unreadable) is None, unreadable
    assert normalise_policy({"type": "workspaceWrite"}) == WRITE_POLICY
    assert normalise_policy({"type": "readOnly"}) == {"type": "readOnly", "networkAccess": False}


async def test_a_matching_mode_does_not_rescue_an_unreadable_policy(bridge, fake_server, tmp_path):
    """Only the mode was requested and the reported type matches, yet it still refuses.

    A policy that cannot be read in full confirms nothing about the thread, so accepting it on
    the strength of its type would be the same silence-as-proof mistake in a smaller place.
    """
    fake, _ = fake_server
    fake.override_creation = {"sandbox": {"type": "workspaceWrite", "writableRoots": None}}
    result = await create(
        bridge, "mode-only", str(tmp_path), prompt="hello", sandbox="workspace-write"
    )
    assert result["status"] == "failed"
    assert result["rpcError"]["code"] == "settings_not_preserved"
    assert fake.count("turn/start") == 0


# ------------------------------------------------- declared approval policy (CRW-122 Phase 1)
#
# The defect: a parent's recovery or merge-turn return to an IDLE supervisor whose approvalPolicy
# is on-request was refused with unsupported_approval_policy BEFORE turn/start, so the report
# could never be delivered. Reproduced against a real isolated app-server on codex-cli 0.154.0
# before any of this existed: status failed, expected "never", returned "on-request",
# attemptedEffects ["thread/resume"], zero turn/start.


async def test_an_idle_thread_on_on_request_accepts_a_declared_delivery(
    bridge, fake_server, tmp_path
):
    """RED: this is the failure the whole phase exists for.

    Declaring the policy the thread is actually on lets the report through. Nothing is relaxed:
    the declaration is compared against the host's answer and is never transmitted.
    """
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path))
    fake.approval_policy = "on-request"
    result = await send(
        bridge, "m", created["threadId"], "parent report",
        expected_settings={"approval_policy": "on-request"},
    )
    assert result["status"] == "accepted", result.get("error")
    assert result["turnId"]
    assert fake.count("turn/start") == 1
    assert result["settings"]["findings"] == []
    # The policy was preserved by not being sent, not by being sent and ignored.
    assert "approvalPolicy" not in sent(fake, "thread/resume")
    assert result["approvals"]["observed"] == "on-request"
    assert result["approvals"]["declared"] == "on-request"
    assert result["approvals"]["transmitted"] is False
    assert result["approvals"]["preservation"] == "omitted_from_resume"


async def test_declaring_nothing_still_refuses_an_interactive_thread(bridge, fake_server, tmp_path):
    """COMPATIBILITY: the guard is not deleted, and a caller that declares nothing is unchanged."""
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path))
    fake.approval_policy = "on-request"
    result = await send(bridge, "m", created["threadId"], "hello")
    assert result["status"] == "failed"
    assert result["rpcError"]["code"] == "unsupported_approval_policy"
    assert result["settings"]["findings"][0]["expected"] == "never"
    assert fake.count("turn/start") == 0


async def test_a_policy_that_changed_under_the_caller_is_refused(bridge, fake_server, tmp_path):
    """The declaration is a claim about the thread, so a thread in another state is refused.

    This is the "state changed, judge it again" case: the caller addressed a supervisor it
    believed was on on-request and the host reports untrusted instead.
    """
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path))
    fake.approval_policy = "untrusted"
    result = await send(
        bridge, "m", created["threadId"], "hello",
        expected_settings={"approval_policy": "on-request"},
    )
    assert result["status"] == "failed"
    assert result["rpcError"]["code"] == "unsupported_approval_policy"
    assert result["settings"]["findings"][0]["returned"] == "untrusted"
    assert fake.count("turn/start") == 0


async def test_a_granular_policy_is_observed_but_can_never_be_declared(bridge, fake_server, tmp_path):
    """AskForApproval's fourth shape has no name, so it is refused however it is addressed."""
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path))
    fake.approval_policy = {"granular": {"mcp_elicitations": True, "rules": True,
                                         "sandbox_approval": True}}
    result = await send(
        bridge, "m", created["threadId"], "hello",
        expected_settings={"approval_policy": "on-request"},
    )
    assert result["status"] == "failed"
    assert result["rpcError"]["code"] == "unsupported_approval_policy"
    assert result["settings"]["findings"][0]["returned"] == "granular"
    assert fake.count("turn/start") == 0
    # And a caller cannot declare one either, refused locally before anything is sent.
    settled = len(fake.calls)
    with pytest.raises(ValueError, match="approval_policy must be one of"):
        await send(
            bridge, "granular-declaration", created["threadId"], "hello",
            expected_settings={"approval_policy": "granular"},
        )
    assert fake.calls[settled:] == []


async def test_transmitting_a_policy_would_have_relaxed_an_interactive_thread(
    bridge, fake_server, tmp_path
):
    """Why the parameter is omitted rather than set to the value the caller declared.

    Measured on codex-cli 0.154.0 a resume reports the thread's own policy and ignores the
    parameter, so against that host the old transmission was merely an ill-shaped request. This
    fake models the other host, the one that ACTS on the parameter. Against it the old resume --
    which always carried approvalPolicy "never" -- would have moved an on-request supervisor to
    never and then delivered the message under a policy nobody asked it to change, reporting
    success because the value it found was the value it had just written.

    Sending nothing cannot do that, whichever host is on the other end. The thread stays where it
    was and the send is refused until a caller declares the policy the thread is really on.
    """
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path))
    fake.honour_resume_policy = True
    fake.approval_policy = "on-request"
    result = await send(bridge, "m", created["threadId"], "hello")
    assert "approvalPolicy" not in sent(fake, "thread/resume")
    assert fake.approval_policy == "on-request", "the resume must not have moved the policy"
    assert result["status"] == "failed"
    assert result["rpcError"]["code"] == "unsupported_approval_policy"
    assert fake.count("turn/start") == 0


async def test_a_declared_delivery_still_leaves_a_setter_host_untouched(
    bridge, fake_server, tmp_path
):
    """And declaring the policy does not start transmitting it either."""
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path))
    fake.honour_resume_policy = True
    fake.approval_policy = "on-request"
    result = await send(
        bridge, "m", created["threadId"], "hello",
        expected_settings={"approval_policy": "on-request"},
    )
    assert "approvalPolicy" not in sent(fake, "thread/resume")
    assert fake.approval_policy == "on-request"
    assert result["status"] == "accepted" and fake.count("turn/start") == 1


async def test_an_approval_request_during_the_turn_is_left_undecided(bridge, fake_server, tmp_path):
    """Delivering a report and servicing the code it provokes are different capabilities.

    The bridge answers an approval request not at all: measured on codex-cli 0.154.0 the host
    applies the first answer from any subscribed client, an error as a denial, so any answer here
    would decide for the thread's approver. test_approval_routing.py covers every method and the
    replay on resume.
    """
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path))
    fake.approval_policy = "on-request"
    fake.approval_request_on_turn = "item/commandExecution/requestApproval"
    result = await send(
        bridge, "m", created["threadId"], "please proceed",
        expected_settings={"approval_policy": "on-request"},
    )
    await bridge.read_thread(created["threadId"])
    assert result["status"] == "accepted"
    assert fake.server_requests, "the fake raised no request, so nothing was tested"
    assert fake.client_answers == [], "no answer at all: not a refusal, not a decision"
    assert result["approvals"]["servicedByThisBridge"] is False
    assert result["approvals"]["onApprovalRequest"] == "left_for_thread_approver"


async def test_a_refused_send_is_preserved_as_undelivered(bridge, fake_server, tmp_path):
    """A refusal has to say the message did not arrive, not leave it to be inferred."""
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path))
    fake.approval_policy = "on-request"
    result = await send(bridge, "m", created["threadId"], "hello")
    assert result["status"] == "failed"
    assert result["delivery"] == "not_delivered"
    assert "turn/start" not in result["attemptedEffects"]
    assert fake.count("turn/start") == 0


async def test_recovery_processes_one_refused_message_exactly_once(bridge, fake_server, tmp_path):
    """Replay answers from the ledger, and a corrected declaration is a different request."""
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path))
    fake.approval_policy = "on-request"
    first = await send(bridge, "deliver-1", created["threadId"], "parent report")
    assert first["status"] == "failed" and fake.count("turn/start") == 0

    # Replaying the same id repeats the refusal and sends nothing. No ACK loop, no retry storm.
    for _ in range(5):
        again = await send(bridge, "deliver-1", created["threadId"], "parent report")
        assert again["replayed"] and again["delivery"] == "not_delivered"
    assert fake.count("turn/start") == 0

    # Correcting the declaration is a different request, so the old id refuses it outright.
    with pytest.raises(ValueError, match="different arguments"):
        await send(
            bridge, "deliver-1", created["threadId"], "parent report",
            expected_settings={"approval_policy": "on-request"},
        )
    assert fake.count("turn/start") == 0

    # Under a new id it is delivered, once.
    fixed = await send(
        bridge, "deliver-2", created["threadId"], "parent report",
        expected_settings={"approval_policy": "on-request"},
    )
    assert fixed["status"] == "accepted" and fixed["delivery"] == "turn_started"
    assert fake.count("turn/start") == 1
    for _ in range(5):
        replay = await send(
            bridge, "deliver-2", created["threadId"], "parent report",
            expected_settings={"approval_policy": "on-request"},
        )
        assert replay["replayed"] and replay["turnId"] == fixed["turnId"]
    assert fake.count("turn/start") == 1, "a replayed ACK must never start a second turn"


async def test_an_accepted_delivery_is_not_reported_as_completed_work(
    bridge, fake_server, tmp_path
):
    """Acceptance is the host taking the turn, and the receipt must not read as more."""
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path))
    fake.approval_policy = "on-request"
    fake.complete_turns = False
    result = await send(
        bridge, "m", created["threadId"], "hello",
        expected_settings={"approval_policy": "on-request"},
    )
    assert result["status"] == "accepted"
    assert result["delivery"] == "turn_started"
    assert "completed" not in result["deliveryMeaning"].split(".")[0]
    assert "does not say the peer read it" in result["deliveryMeaning"]
    observed = await bridge.wait_thread(created["threadId"], result["turnId"], 0)
    assert observed["turn"]["status"] == "inProgress"
    assert observed["timedOut"] is True


async def test_an_explicit_null_declaration_is_refused_rather_than_read_as_never(
    bridge, fake_server, tmp_path
):
    """A key that was written must never be read as a key that was not.

    Absent declares never. Present-but-null is a caller mistake, and folding it into never would
    accept an invalid declaration and let the caller believe it had stated something. That is the
    same "a discarded key looks identical to a setting never requested" failure the unknown-key
    refusal above exists to prevent, so it is refused the same way: locally, before any RPC.
    """
    fake, _ = fake_server
    created = await create(bridge, "c", str(tmp_path))
    settled = len(fake.calls)
    with pytest.raises(ValueError, match="approval_policy must be one of"):
        await send(
            bridge, "null-declaration", created["threadId"], "hello",
            expected_settings={"approval_policy": None},
        )
    assert fake.calls[settled:] == [], "refused before anything reached the host"
