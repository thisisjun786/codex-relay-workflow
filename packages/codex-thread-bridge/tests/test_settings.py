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

import pytest

from codex_thread_bridge.settings import UntransmittableSetting, normalise_policy

WRITE_POLICY = {
    "type": "workspaceWrite",
    "writableRoots": [],
    "networkAccess": False,
    "excludeTmpdirEnvVar": False,
    "excludeSlashTmp": False,
}


def sent(fake, method):
    return next(params for name, params in fake.calls if name == method)


# --------------------------------------------------------------------------- creation


async def test_effort_is_transmitted_in_config_and_confirmed(bridge, fake_server, tmp_path):
    """RED: create_thread had no reasoning_effort parameter at all."""
    fake, _ = fake_server
    result = await bridge.create_thread(
        "effort", str(tmp_path), model="anthropic/claude-opus-5", reasoning_effort="xhigh"
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
    result = await bridge.create_thread(
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
    result = await bridge.create_thread(
        "swap", str(tmp_path), prompt="hello", model="anthropic/claude-opus-5"
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
    result = await bridge.create_thread(
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
    result = await bridge.create_thread(
        "null-effort", str(tmp_path), prompt="hello", reasoning_effort="xhigh"
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
    result = await bridge.create_thread(
        "policy", str(tmp_path), sandbox="workspace-write", expected_sandbox_policy=policy
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
    await bridge.create_thread(
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
        await bridge.create_thread(
            "ro-net",
            str(tmp_path),
            expected_sandbox_policy={"type": "readOnly", "networkAccess": True},
        )
    assert raised.value.code == "setting_untransmittable"
    assert raised.value.field == "sandbox.networkAccess"
    assert fake.calls == [], "nothing may reach the host"


async def test_filled_protocol_defaults_are_not_read_as_a_difference(bridge, tmp_path):
    """RED: the host fills every default in, and a bare expected type must still match."""
    result = await bridge.create_thread(
        "bare", str(tmp_path), sandbox="workspace-write", expected_sandbox_policy=dict(WRITE_POLICY)
    )
    assert result["status"] == "accepted"
    assert normalise_policy({"type": "workspaceWrite"}) == WRITE_POLICY


async def test_the_receipt_never_claims_more_than_the_observation(bridge, tmp_path):
    """RED: nothing says "verified"; the claim is scoped to the observation point."""
    result = await bridge.create_thread("claim", str(tmp_path), reasoning_effort="xhigh")
    settings = result["settings"]
    assert settings["verification"] == "observed_at_creation"
    assert "verified" not in settings["verification"].split("_at_")[0].replace("observed", "")
    assert "no host-side exclusivity" in settings["observationLimits"]


async def test_nothing_requested_transmits_nothing_and_claims_nothing(
    bridge, fake_server, tmp_path
):
    """RED for the receipt, COMPATIBILITY for the wire: defaults stay, and unasked-for
    settings are reported without being claimed about."""
    fake, _ = fake_server
    result = await bridge.create_thread("plain", str(tmp_path))
    start = sent(fake, "thread/start")
    assert "config" not in start and "runtimeWorkspaceRoots" not in start
    settings = result["settings"]
    # cwd and sandbox are inherent to creating a thread, so they are always requested and
    # always checked. Model, effort and roots were not asked for: they are reported as the
    # host's actual values but claimed about in no way.
    assert sorted(settings["requested"]) == ["cwd", "sandbox"]
    assert settings["verification"] == "observed_at_creation"
    assert settings["verified"] == ["cwd", "sandbox"]
    assert settings["actual"]["model"] == "configured-default"
    assert "model" not in settings["requested"]


# ----------------------------------------------------------------------------- resume


def carried(**overrides):
    return {
        "sandbox": "workspace-write",
        "expected_sandbox_policy": dict(WRITE_POLICY),
        **overrides,
    }


async def test_resume_without_settings_is_byte_identical_to_the_old_behaviour(
    bridge, fake_server, tmp_path
):
    """COMPATIBILITY for the resume params, RED for the receipt.

    The resume params assertion is the guarantee that MCP is not forced to become the only
    transmission path; test_bridge.py pins the same params independently of this file.
    """
    fake, _ = fake_server
    created = await bridge.create_thread("c", str(tmp_path))
    result = await bridge.send_message_to_thread("m", created["threadId"], "hello")
    assert sent(fake, "thread/resume") == {
        "threadId": created["threadId"],
        "excludeTurns": True,
    }
    assert result["status"] == "accepted" and result["turnId"]
    assert result["settings"]["verification"] == "not_requested"
    assert result["settings"]["requested"] == {}
    # Silence is reported as silence: the observed values are there, unclaimed.
    assert result["settings"]["actual"]["approvalPolicy"] == "never"


async def test_resume_carries_the_settings_it_can_express(bridge, fake_server, tmp_path):
    """RED: the old resume deliberately supplied no cwd, model, sandbox or reasoning at all."""
    fake, _ = fake_server
    created = await bridge.create_thread(
        "c",
        str(tmp_path),
        sandbox="workspace-write",
        model="anthropic/claude-opus-5",
        reasoning_effort="xhigh",
        expected_sandbox_policy=dict(WRITE_POLICY),
    )
    await bridge.send_message_to_thread(
        "m",
        created["threadId"],
        "hello",
        expected_settings=carried(
            cwd=str(tmp_path), model="anthropic/claude-opus-5", reasoning_effort="xhigh"
        ),
    )
    resume = sent(fake, "thread/resume")
    assert resume["sandbox"] == "workspace-write"
    assert resume["approvalPolicy"] == "never"
    assert resume["cwd"] == str(tmp_path)
    assert resume["model"] == "anthropic/claude-opus-5"
    assert resume["config"]["model_reasoning_effort"] == "xhigh"


async def test_a_widened_sandbox_withholds_the_message_before_any_turn(
    bridge, fake_server, tmp_path
):
    """RED: this is the failure the whole issue exists for."""
    fake, _ = fake_server
    created = await bridge.create_thread("c", str(tmp_path), sandbox="workspace-write")
    fake.override_resume = {"sandbox": {"type": "dangerFullAccess"}}
    before = fake.count("turn/start")
    result = await bridge.send_message_to_thread(
        "m", created["threadId"], "hello", expected_settings=carried()
    )
    assert result["status"] == "failed"
    assert result["rpcError"]["code"] == "settings_not_preserved"
    assert result["settings"]["findings"][0]["field"] == "sandbox"
    assert fake.count("turn/start") == before, "no turn may start"


async def test_a_clean_resume_starts_its_turn_with_no_overrides(bridge, fake_server, tmp_path):
    """RED: turn/start reports only the turn, so the bridge binds nothing it cannot read back."""
    fake, _ = fake_server
    created = await bridge.create_thread("c", str(tmp_path), sandbox="workspace-write")
    result = await bridge.send_message_to_thread(
        "m", created["threadId"], "hello", expected_settings=carried()
    )
    assert result["status"] == "accepted"
    assert result["settings"]["verification"] == "observed_at_resume"
    assert set(sent(fake, "turn/start")) == {"threadId", "input"}


async def test_an_interactive_approval_policy_still_decides_alone(bridge, fake_server, tmp_path):
    """RED: a permanently closed channel must not be shadowed by a generic mismatch."""
    fake, _ = fake_server
    created = await bridge.create_thread("c", str(tmp_path), sandbox="workspace-write")
    fake.approval_policy = "on-request"
    fake.override_resume = {"sandbox": {"type": "dangerFullAccess"}}
    result = await bridge.send_message_to_thread(
        "m", created["threadId"], "hello", expected_settings=carried()
    )
    assert result["rpcError"]["code"] == "unsupported_approval_policy"
    assert len(result["settings"]["findings"]) == 1


# ------------------------------------------------------------------------- idempotency


async def test_a_reused_id_replays_without_dispatching_again(bridge, fake_server, tmp_path):
    """RED with settings supplied; the underlying replay rule is long-standing.

    A retry must never create new duplicate work.
    """
    fake, _ = fake_server
    created = await bridge.create_thread("c", str(tmp_path), sandbox="workspace-write")
    first = await bridge.send_message_to_thread(
        "same", created["threadId"], "hello", expected_settings=carried()
    )
    turns = fake.count("turn/start")
    again = await bridge.send_message_to_thread(
        "same", created["threadId"], "hello", expected_settings=carried()
    )
    assert again["replayed"] and again["turnId"] == first["turnId"]
    assert fake.count("turn/start") == turns


async def test_a_reused_id_with_changed_settings_is_rejected(bridge, tmp_path):
    """RED: supplied settings are part of the request identity."""
    created = await bridge.create_thread("c", str(tmp_path), sandbox="workspace-write")
    await bridge.send_message_to_thread(
        "same", created["threadId"], "hello", expected_settings=carried()
    )
    with pytest.raises(ValueError, match="different arguments"):
        await bridge.send_message_to_thread(
            "same",
            created["threadId"],
            "hello",
            expected_settings=carried(model="anthropic/claude-opus-5"),
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
    replayed = await bridge.create_thread("retained", cwd)
    assert replayed["replayed"] and replayed["threadId"] == "older-thread"


# -------------------------------------------------------------- post-acceptance annotation


async def test_the_annotation_records_what_the_thread_reported_after_dispatch(
    bridge, fake_server, tmp_path
):
    """RED: the only signal available about a concurrent change around dispatch."""
    fake, _ = fake_server
    result = await bridge.create_thread(
        "annotated", str(tmp_path), prompt="hello", reasoning_effort="xhigh"
    )
    note = result["settingsAfterDispatch"]
    assert note["concurrentChange"] is False
    assert note["covers"] == ["model", "reasoningEffort", "cwd"]
    assert "neither sandbox nor approvalPolicy" in note["limit"]


async def test_a_failing_annotation_cannot_downgrade_an_accepted_turn(
    bridge, fake_server, tmp_path
):
    """RED: a diagnostic that could strand an acknowledged dispatch would be worse than none."""
    fake, _ = fake_server
    result = await bridge.create_thread(
        "c", str(tmp_path), prompt="hello", reasoning_effort="xhigh"
    )
    assert result["status"] == "accepted"

    fake.reject["thread/read"] = {"code": -32000, "message": "nope"}
    second = await bridge.create_thread(
        "annot-fail", str(tmp_path), prompt="hello", reasoning_effort="xhigh"
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
    created = await bridge.create_thread("c", str(tmp_path), sandbox="workspace-write")
    before = fake.count("turn/start")
    with pytest.raises(ValueError, match="unknown keys"):
        await bridge.send_message_to_thread(
            "typo", created["threadId"], "hello", expected_settings={"reasoningEffort": "xhigh"}
        )
    assert fake.count("turn/start") == before, "nothing may be dispatched"
    with pytest.raises(ValueError, match="unknown keys"):
        await bridge.send_message_to_thread(
            "typo2",
            created["threadId"],
            "hello",
            expected_settings={"model": "anthropic/claude-opus-5", "sandboxPolicy": {}},
        )


async def test_a_replay_answers_from_the_ledger_without_touching_the_host(
    bridge, fake_server, tmp_path
):
    """A retained receipt is the ledger's answer, and reading the host again would spoil it.

    It would make a replay wait on a server that may be offline, observe state from long after
    the dispatch, and overwrite the original annotation with that later observation.
    """
    fake, _ = fake_server
    first = await bridge.create_thread(
        "replayed", str(tmp_path), prompt="hello", reasoning_effort="xhigh"
    )
    assert first["settingsAfterDispatch"]["concurrentChange"] is False
    calls_before = len(fake.calls)

    again = await bridge.create_thread(
        "replayed", str(tmp_path), prompt="hello", reasoning_effort="xhigh"
    )
    assert again["replayed"]
    assert len(fake.calls) == calls_before, "a replay must issue no host call at all"
    assert again["settingsAfterDispatch"] == first["settingsAfterDispatch"]


async def test_a_replay_is_answered_even_when_the_host_has_gone_away(bridge, fake_server, tmp_path):
    """Offline recovery is the case the ledger exists for; it must not wait on a read."""
    fake, _ = fake_server
    first = await bridge.create_thread(
        "offline", str(tmp_path), prompt="hello", reasoning_effort="xhigh"
    )
    fake.reject["thread/read"] = {"code": -32000, "message": "server is gone"}
    again = await bridge.create_thread(
        "offline", str(tmp_path), prompt="hello", reasoning_effort="xhigh"
    )
    assert again["replayed"] and again["turnId"] == first["turnId"]
    assert again["settingsAfterDispatch"] == first["settingsAfterDispatch"]
