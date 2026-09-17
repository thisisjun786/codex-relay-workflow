"""What the execution guard refuses, and the proof that nothing was ever dispatched.

Every blocked case here asserts the structured call record of the fake host, because the claim
being made is not "an error was raised" but "no paid turn was started". A refusal that still
reached turn/start would satisfy the first and fail the thing that matters.

The transport is synthetic throughout. No real model is contacted, and nothing here says anything
about an installed bridge or a live App Server.
"""

import json
from pathlib import Path

import pytest
from conftest import EFFORT, EXECUTION, MODEL

from codex_thread_bridge.execution import (
    Execution,
    ExecutionPolicy,
    ExecutionPolicyError,
    ExecutionRefused,
)

UNAPPROVED = "openai/gpt-6-astra"
RELABELLED = ("relabelled/model", "relabelled-effort")


class RelabellingPolicy:
    """A policy that authorizes a pair different from the one the caller asked for.

    A real authorize() is value-preserving, so a test built on one cannot tell a launch made from
    the authorization apart from a launch made from the caller's raw arguments: both are the same
    string. This can. If any path still reads the arguments, the host is asked for the wrong pair
    and every assertion below fails.
    """

    mode = "allowlist"

    def summary(self):
        return {"mode": self.mode, "digest": "stub"}

    def authorize(self, model, reasoning_effort, *, cwd=None, exception=None):
        assert isinstance(model, str) and isinstance(reasoning_effort, str)
        return Execution(
            *RELABELLED,
            {
                "mode": self.mode,
                "digest": "stub",
                "exception": exception,
                "model": RELABELLED[0],
                "reasoningEffort": RELABELLED[1],
            },
        )


def policy_for(directory):
    """An allowlist whose approved efforts differ per model, plus one directory-bound exception."""
    return {
        "allowed": [
            {"model": MODEL, "efforts": [EFFORT]},
            {"model": "openai/gpt-5.6-sol", "efforts": ["high"]},
        ],
        "exceptions": {
            "one-task": {
                "model": UNAPPROVED,
                "reasoningEffort": "high",
                "cwd": [str(Path(directory).resolve())],
                "reason": "the operator's note, which no caller ever sees",
            }
        },
    }


# --------------------------------------------------------------------------- the policy itself


def test_an_unconfigured_host_still_demands_a_stated_pair():
    """The presence question has an answer everywhere; only approval needs a configured file."""
    policy = ExecutionPolicy.from_environment({})
    assert policy.summary() == {"mode": "presence_only", "digest": None}
    with pytest.raises(ExecutionRefused) as raised:
        policy.authorize(None, EFFORT)
    assert raised.value.code == "execution_setting_missing"
    assert raised.value.field == "model"
    # Anything explicitly stated is accepted, because no host answer exists to compare it against.
    assert policy.authorize(UNAPPROVED, "low").model == UNAPPROVED


def test_an_exception_cannot_be_invented_where_none_was_written():
    """A caller may cite an approval the operator wrote. It can never define one."""
    with pytest.raises(ExecutionRefused) as raised:
        ExecutionPolicy.from_environment({}).authorize(MODEL, EFFORT, exception="i-approve-this")
    assert raised.value.code == "execution_exception_unknown"


@pytest.mark.parametrize(
    ("mutation", "expected"),
    [
        ({"allowed": []}, "non-empty list"),
        ({"allowed": [{"model": MODEL}]}, "both model and efforts"),
        ({"allowed": [{"model": MODEL, "efforts": []}]}, "non-empty list of efforts"),
        ({"allowed": [{"model": MODEL, "efforts": [EFFORT], "extra": 1}]}, "unknown keys"),
        (
            {
                "allowed": [
                    {"model": MODEL, "efforts": [EFFORT]},
                    {"model": MODEL, "efforts": ["x"]},
                ]
            },
            "listed twice",
        ),
        ({"models": [MODEL]}, "unknown keys"),
        ({"exceptions": {"e": {"model": MODEL, "cwd": ["/tmp"]}}}, "missing"),
        (
            {"exceptions": {"e": {"model": MODEL, "reasoningEffort": EFFORT, "cwd": []}}},
            "at least one cwd",
        ),
        (
            {"exceptions": {"e": {"model": MODEL, "reasoningEffort": EFFORT, "cwd": ["relative"]}}},
            "canonical and absolute",
        ),
        (
            {
                "exceptions": {
                    "e": {
                        "model": MODEL,
                        "reasoningEffort": EFFORT,
                        "cwd": ["/srv/checkouts/../task"],
                    }
                }
            },
            "canonical and absolute",
        ),
        (
            {"exceptions": {"e": {"model": MODEL, "reasoningEffort": EFFORT, "cwd": ["/srv/x/"]}}},
            "canonical and absolute",
        ),
        (
            {
                "exceptions": {
                    "e": {
                        "model": MODEL,
                        "reasoningEffort": EFFORT,
                        "cwd": ["/srv/" + chr(0) + "/task"],
                    }
                }
            },
            "cannot be resolved",
        ),
        (
            {
                "exceptions": {
                    "x" * 129: {
                        "model": MODEL,
                        "reasoningEffort": EFFORT,
                        "cwd": ["/srv/task"],
                    }
                }
            },
            "an exception id must be a non-empty string",
        ),
        (
            {
                "exceptions": {
                    "e": {
                        "model": MODEL,
                        "reasoningEffort": EFFORT,
                        "cwd": ["/tmp"],
                        "models": [UNAPPROVED],
                    }
                }
            },
            "unknown keys",
        ),
    ],
)
def test_an_unusable_policy_file_is_refused_rather_than_partly_honoured(
    tmp_path, mutation, expected
):
    """A file the operator configured and the server then ignored is the one failure nobody sees."""
    base = {"allowed": [{"model": MODEL, "efforts": [EFFORT]}]}
    path = tmp_path / "policy.json"
    path.write_text(json.dumps({**base, **mutation}))
    with pytest.raises(ExecutionPolicyError, match=expected):
        ExecutionPolicy.from_environment({"CODEX_THREAD_BRIDGE_EXECUTION_POLICY": str(path)})


def test_a_missing_or_malformed_file_stops_the_policy_from_loading(tmp_path):
    absent = tmp_path / "not-here.json"
    with pytest.raises(ExecutionPolicyError, match="cannot read"):
        ExecutionPolicy.from_file(absent)
    broken = tmp_path / "broken.json"
    broken.write_text("{ not json")
    with pytest.raises(ExecutionPolicyError, match="not valid JSON"):
        ExecutionPolicy.from_file(broken)


def test_a_repeated_key_is_refused_rather_than_silently_overwritten(tmp_path):
    """json keeps the last occurrence, so one of the two lists would be enforced invisibly."""
    path = tmp_path / "duplicated.json"
    path.write_text(
        '{"allowed": [{"model": "a", "efforts": ["x"]}],'
        ' "allowed": [{"model": "b", "efforts": ["y"]}]}'
    )
    with pytest.raises(ExecutionPolicyError, match="duplicate key 'allowed'"):
        ExecutionPolicy.from_file(path)
    nested = tmp_path / "nested.json"
    nested.write_text('{"allowed": [{"model": "a", "efforts": ["x"], "model": "b"}]}')
    with pytest.raises(ExecutionPolicyError, match="duplicate key 'model'"):
        ExecutionPolicy.from_file(nested)


def test_the_digest_identifies_the_file_without_disclosing_it(tmp_path):
    path = tmp_path / "policy.json"
    path.write_text(json.dumps(policy_for(tmp_path)))
    policy = ExecutionPolicy.from_file(path)
    summary = policy.summary()
    assert summary["mode"] == "allowlist"
    assert len(summary["digest"]) == 64
    receipt = policy.authorize(MODEL, EFFORT).receipt
    assert str(path) not in json.dumps(receipt)
    assert "operator's note" not in json.dumps(receipt)
    assert receipt["exception"] is None and receipt["digest"] == summary["digest"]


def test_efforts_are_scoped_to_their_model(tmp_path):
    """Two independent lists would approve every crossing of them, which nobody wrote down."""
    policy = ExecutionPolicy.from_mapping(policy_for(tmp_path))
    assert policy.authorize(MODEL, EFFORT).reasoning_effort == EFFORT
    assert policy.authorize("openai/gpt-5.6-sol", "high").model == "openai/gpt-5.6-sol"
    with pytest.raises(ExecutionRefused) as raised:
        policy.authorize(MODEL, "high")
    assert raised.value.code == "execution_not_allowed"
    assert raised.value.field == "reasoning_effort"
    assert raised.value.allowed == [EFFORT]


def test_an_exception_authorizes_one_triple_and_not_a_family(tmp_path):
    policy = ExecutionPolicy.from_mapping(policy_for(tmp_path))
    here = str(Path(tmp_path).resolve())
    assert policy.authorize(UNAPPROVED, "high", cwd=here, exception="one-task").model == UNAPPROVED
    for model, effort, code, field in [
        (MODEL, "high", "execution_not_allowed", "model"),
        (UNAPPROVED, EFFORT, "execution_not_allowed", "reasoning_effort"),
    ]:
        with pytest.raises(ExecutionRefused) as raised:
            policy.authorize(model, effort, cwd=here, exception="one-task")
        assert (raised.value.code, raised.value.field) == (code, field)
    for elsewhere in (None, "/somewhere/else"):
        with pytest.raises(ExecutionRefused) as raised:
            policy.authorize(UNAPPROVED, "high", cwd=elsewhere, exception="one-task")
        assert raised.value.code == "execution_exception_out_of_scope"


# ------------------------------------------------------------------- creation, through the bridge


@pytest.mark.parametrize(
    ("kwargs", "code", "field"),
    [
        ({"reasoning_effort": EFFORT}, "execution_setting_missing", "model"),
        ({"model": MODEL}, "execution_setting_missing", "reasoning_effort"),
        ({"model": "   ", "reasoning_effort": EFFORT}, "execution_setting_invalid", "model"),
        ({"model": MODEL, "reasoning_effort": ""}, "execution_setting_invalid", "reasoning_effort"),
    ],
    ids=["model-omitted", "effort-omitted", "model-blank", "effort-blank"],
)
async def test_a_creation_without_a_stated_pair_sends_nothing(
    bridge, fake_server, tmp_path, kwargs, code, field
):
    fake, _ = fake_server
    with pytest.raises(ExecutionRefused) as raised:
        await bridge.create_thread("guarded", str(tmp_path), **kwargs)
    assert (raised.value.code, raised.value.field) == (code, field)
    assert fake.calls == [], "a refused creation reached the host"
    assert fake.count("turn/start") == 0


async def test_a_refusal_leaves_the_request_id_usable(bridge, fake_server, tmp_path):
    """No ledger row is written, so the caller corrects the arguments under the SAME id.

    Recording a refusal would consume the id and force a new one for the corrected call, which is
    the opposite of what the recovery instructions tell a caller to do.
    """
    fake, _ = fake_server
    with pytest.raises(ExecutionRefused):
        await bridge.create_thread("one-intent", str(tmp_path), prompt="work")
    with pytest.raises(ValueError, match="Unknown request_id"):
        bridge.ledger.get("one-intent")
    corrected = await bridge.create_thread("one-intent", str(tmp_path), prompt="work", **EXECUTION)
    assert corrected["status"] == "accepted"
    assert fake.count("thread/start") == 1 and fake.count("turn/start") == 1


@pytest.mark.parametrize(
    ("model", "effort", "field"),
    [
        (UNAPPROVED, "high", "model"),
        (MODEL, "low", "reasoning_effort"),
        (MODEL, "high", "reasoning_effort"),
    ],
    ids=["unapproved-model", "unapproved-effort", "crossed-pair"],
)
async def test_an_unapproved_pair_is_refused_before_any_call(
    configured_bridge, fake_server, tmp_path, model, effort, field
):
    """The crossed pair is the decisive row: opus/high is two approved values and one unapproved
    combination, so an implementation that allowlists models and efforts separately fails here."""
    fake, _ = fake_server
    bridge = configured_bridge(policy_for(tmp_path))
    with pytest.raises(ExecutionRefused) as raised:
        await bridge.create_thread(
            "blocked", str(tmp_path), prompt="work", model=model, reasoning_effort=effort
        )
    assert raised.value.code == "execution_not_allowed"
    assert raised.value.field == field
    assert fake.calls == []
    assert fake.count("turn/start") == 0


async def test_a_cited_exception_that_does_not_exist_or_does_not_match_sends_nothing(
    configured_bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    bridge = configured_bridge(policy_for(tmp_path))
    other = tmp_path / "elsewhere"
    other.mkdir()
    for request, kwargs, code in [
        ("unknown", {"policy_exception": "invented"}, "execution_exception_unknown"),
        ("wrong-model", {"policy_exception": "one-task", "model": MODEL}, "execution_not_allowed"),
        (
            "wrong-effort",
            {"policy_exception": "one-task", "reasoning_effort": "max"},
            "execution_not_allowed",
        ),
    ]:
        with pytest.raises(ExecutionRefused) as raised:
            await bridge.create_thread(
                request,
                str(tmp_path),
                **{"model": UNAPPROVED, "reasoning_effort": "high", **kwargs},
            )
        assert raised.value.code == code, request
    with pytest.raises(ExecutionRefused) as raised:
        await bridge.create_thread(
            "wrong-place",
            str(other),
            model=UNAPPROVED,
            reasoning_effort="high",
            policy_exception="one-task",
        )
    assert raised.value.code == "execution_exception_out_of_scope"
    assert fake.calls == []


async def test_an_authorized_pair_is_the_one_transmitted_and_the_one_compared(
    configured_bridge, fake_server, tmp_path
):
    """The exception's pair is deliberately not the allowlist's, so a launch built from the raw
    arguments instead of the authorization would be visible here rather than cancelling out."""
    fake, _ = fake_server
    bridge = configured_bridge(policy_for(tmp_path))
    receipt = await bridge.create_thread(
        "excepted",
        str(tmp_path),
        prompt="work",
        model=UNAPPROVED,
        reasoning_effort="high",
        policy_exception="one-task",
    )
    assert receipt["status"] == "accepted"
    start = next(p for name, p in fake.calls if name == "thread/start")
    assert start["model"] == UNAPPROVED
    assert start["config"]["model_reasoning_effort"] == "high"
    assert receipt["executionPolicy"]["mode"] == "allowlist"
    assert receipt["executionPolicy"]["exception"] == "one-task"
    assert receipt["executionPolicy"]["model"] == UNAPPROVED
    assert receipt["executionPolicy"]["reasoningEffort"] == "high"
    assert receipt["settings"]["requested"]["model"] == UNAPPROVED
    assert receipt["settings"]["requested"]["reasoningEffort"] == "high"
    assert receipt["settings"]["verification"] == "observed_at_creation"
    assert fake.count("turn/start") == 1


async def test_an_approved_pair_records_the_mode_it_was_approved_under(
    configured_bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    bridge = configured_bridge(policy_for(tmp_path))
    receipt = await bridge.create_thread("approved", str(tmp_path), prompt="work", **EXECUTION)
    assert receipt["status"] == "accepted"
    start = next(p for name, p in fake.calls if name == "thread/start")
    assert start["model"] == MODEL and start["config"]["model_reasoning_effort"] == EFFORT
    assert receipt["executionPolicy"] == {
        "mode": "allowlist",
        "digest": "test-digest",
        "exception": None,
        "model": MODEL,
        "reasoningEffort": EFFORT,
        "limits": receipt["executionPolicy"]["limits"],
    }
    assert "not evidence that a provider served" in receipt["executionPolicy"]["limits"]
    assert fake.count("turn/start") == 1


async def test_an_unavailable_model_is_an_error_and_not_a_substitution(
    bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    fake.reject["thread/start"] = {"code": -32602, "message": "model is not available"}
    receipt = await bridge.create_thread("gone", str(tmp_path), prompt="work", **EXECUTION)
    assert receipt["status"] == "failed"
    assert "model is not available" in receipt["error"]
    assert fake.count("thread/start") == 1, "no second attempt under another model"
    assert fake.count("turn/start") == 0


# --------------------------------------------------------------------- resume, through the bridge


@pytest.mark.parametrize(
    "settings",
    [
        None,
        {},
        {"model": MODEL},
        {"reasoning_effort": EFFORT},
        {"model": MODEL, "reasoning_effort": " "},
    ],
    ids=["omitted", "empty", "model-only", "effort-only", "blank-effort"],
)
async def test_a_resume_without_a_full_pair_never_reads_or_resumes_the_thread(
    bridge, fake_server, tmp_path, settings
):
    fake, _ = fake_server
    created = await bridge.create_thread("c", str(tmp_path), **EXECUTION)
    settled = len(fake.calls)
    with pytest.raises(ExecutionRefused):
        await bridge.send_message_to_thread("blocked", created["threadId"], "hello", settings)
    assert fake.calls[settled:] == []
    assert fake.count("turn/start") == 0


async def test_a_resume_carries_the_authorized_pair_and_is_checked_against_it(
    configured_bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    bridge = configured_bridge(policy_for(tmp_path))
    created = await bridge.create_thread("c", str(tmp_path), **EXECUTION)
    sent = await bridge.send_message_to_thread("m", created["threadId"], "hello", dict(EXECUTION))
    resume = next(p for name, p in fake.calls if name == "thread/resume")
    assert resume["model"] == MODEL
    assert resume["config"]["model_reasoning_effort"] == EFFORT
    assert sent["executionPolicy"]["model"] == MODEL
    assert sent["executionPolicy"]["reasoningEffort"] == EFFORT
    assert sent["settings"]["verification"] == "observed_at_resume"
    assert fake.count("turn/start") == 1


async def test_an_unapproved_pair_is_refused_on_the_resume_path_too(
    configured_bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    bridge = configured_bridge(policy_for(tmp_path))
    created = await bridge.create_thread("c", str(tmp_path), **EXECUTION)
    settled = len(fake.calls)
    with pytest.raises(ExecutionRefused) as raised:
        await bridge.send_message_to_thread(
            "blocked",
            created["threadId"],
            "hello",
            {"model": UNAPPROVED, "reasoning_effort": "high"},
        )
    assert raised.value.code == "execution_not_allowed"
    assert fake.calls[settled:] == []


@pytest.mark.parametrize(
    ("setup", "code"),
    [("override", "settings_not_preserved"), ("unreported", "setting_unobservable")],
)
async def test_a_resume_that_disagrees_withholds_the_message(
    bridge, fake_server, tmp_path, setup, code
):
    fake, _ = fake_server
    created = await bridge.create_thread("c", str(tmp_path), **EXECUTION)
    if setup == "override":
        fake.override_resume = {"model": "some-other-model"}
    else:
        fake.unreported = {"reasoningEffort"}
    before = fake.count("turn/start")
    receipt = await bridge.send_message_to_thread(
        "guarded", created["threadId"], "hello", dict(EXECUTION)
    )
    assert receipt["status"] == "failed"
    assert receipt["settings"]["findings"][0]["code"] == code
    assert fake.count("turn/start") == before, "the message was dispatched anyway"


async def test_the_launch_and_the_comparison_follow_the_authorization_not_the_arguments(
    configured_bridge, fake_server, tmp_path
):
    """The claim is that one Execution record feeds the wire and the check, on both paths.

    With a value-preserving policy that claim is untestable, because the authorized pair and the
    requested pair are the same string. The stub separates them.
    """
    fake, _ = fake_server
    bridge = configured_bridge(RelabellingPolicy())
    receipt = await bridge.create_thread("relabelled", str(tmp_path), prompt="work", **EXECUTION)
    assert receipt["status"] == "accepted"
    start = next(params for name, params in fake.calls if name == "thread/start")
    assert (start["model"], start["config"]["model_reasoning_effort"]) == RELABELLED
    assert receipt["settings"]["requested"]["model"] == RELABELLED[0]
    assert receipt["settings"]["requested"]["reasoningEffort"] == RELABELLED[1]
    assert receipt["executionPolicy"]["model"] == RELABELLED[0]
    delivered = await bridge.send_message_to_thread(
        "relabelled-send", receipt["threadId"], "again", dict(EXECUTION)
    )
    resume = next(params for name, params in fake.calls if name == "thread/resume")
    assert (resume["model"], resume["config"]["model_reasoning_effort"]) == RELABELLED
    assert delivered["status"] == "accepted"
    assert delivered["executionPolicy"]["model"] == RELABELLED[0]


async def test_a_caller_mutating_its_settings_cannot_split_identity_from_dispatch(
    bridge, fake_server, tmp_path
):
    """The ledger fingerprints params only after the mutation lock, so the object matters.

    While one send holds the lock, a second send is waiting with its caller's own dictionary. If
    params held that dictionary rather than a snapshot, a caller mutating it during the wait would
    authorize and dispatch one pair while recording the identity of another: the real arguments
    would then be refused as different, and the mutated ones would replay a dispatch never made
    under them.
    """
    import asyncio

    fake, _ = fake_server
    created = await bridge.create_thread("c", str(tmp_path), **EXECUTION)
    fake.pause_after = "thread/read"
    settings = dict(EXECUTION)

    holding = asyncio.create_task(
        bridge.send_message_to_thread("holding", created["threadId"], "hold", dict(EXECUTION))
    )
    await fake.paused.wait()
    waiting = asyncio.create_task(
        bridge.send_message_to_thread("waiting", created["threadId"], "hello", settings)
    )
    # Let the second request reach the mutation lock, then mutate the dictionary it was given.
    for _ in range(20):
        await asyncio.sleep(0)
    settings["model"] = UNAPPROVED
    fake.pause_after = None
    fake.release.set()

    assert (await holding)["status"] == "accepted"
    dispatched = await waiting
    assert dispatched["status"] == "accepted"
    assert dispatched["executionPolicy"]["model"] == MODEL

    # The identity recorded is the one that was authorized and dispatched, so the real arguments
    # replay and the mutated ones are refused.
    replayed = await bridge.send_message_to_thread(
        "waiting", created["threadId"], "hello", dict(EXECUTION)
    )
    assert replayed["replayed"] and replayed["turnId"] == dispatched["turnId"]
    with pytest.raises(ValueError, match="different arguments"):
        await bridge.send_message_to_thread(
            "waiting",
            created["threadId"],
            "hello",
            {"model": UNAPPROVED, "reasoning_effort": EFFORT},
        )
