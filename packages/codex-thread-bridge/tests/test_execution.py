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

    def authorize(self, model, reasoning_effort, *, cwd=None, exception=None, role=None):
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


# ------------------------------------------------------------------- roles (CRW-127)
#
# The third question. The two above it can both be answered correctly by a task that is still on
# the wrong model, because each of CRW's three levels is meant to run on a different pair. These
# cases assert the same thing the rest of this file does: not that an error was raised, but that
# nothing was dispatched.

PARENT_MODEL = "devin/swe-2"
PARENT_EFFORT = "max"
# What the parent ran on for part of 2026-09-21, before it was restored to the pair above. Kept
# because a superseded pair is not a second valid answer, and because it is the case where a
# name is shared across roles: it carries the child's effort under a different model, which
# must not be what makes the check work.
SUPERSEDED_PARENT = ("xai/grok-4.6", "xhigh")
# What the child ran on until 2026-09-23, when it moved to MODEL and kept its effort. Kept for
# the same reason as the parent's: a superseded pair is refused for its role. Because it keeps
# the child's effort name, the model is the only axis that can refuse it.
SUPERSEDED_CHILD = ("anthropic/claude-opus-5", "xhigh")


def roles_policy(directory=None, *, roles=None, allowed=True):
    """A policy declaring roles, with the allowlist present or absent as the case needs."""
    policy = {}
    if allowed:
        policy["allowed"] = [
            {"model": MODEL, "efforts": [EFFORT]},
            {"model": PARENT_MODEL, "efforts": [PARENT_EFFORT]},
        ]
    policy["roles"] = roles if roles is not None else {
        "supervisor": {"expectation": "record"},
        "parent": {"model": PARENT_MODEL, "reasoningEffort": PARENT_EFFORT},
        "child": {"model": MODEL, "reasoningEffort": EFFORT},
    }
    return policy


def declared(directory=None, **kwargs):
    return ExecutionPolicy.from_mapping(roles_policy(directory, **kwargs), digest="roles-digest")


def test_a_role_this_host_never_declared_is_refused_rather_than_defaulted():
    """The whole point of naming a role is to be checked by someone other than yourself.

    A host with no roles section has no answer, and inventing one in code would be the second
    source of truth this feature exists to remove.
    """
    policy = ExecutionPolicy.from_environment({})
    with pytest.raises(ExecutionRefused) as raised:
        policy.authorize(MODEL, EFFORT, role="parent")
    assert raised.value.code == "execution_role_unknown"
    assert raised.value.field == "role"
    # And naming none still behaves exactly as it did before roles existed.
    assert policy.authorize(MODEL, EFFORT).model == MODEL


def test_a_word_that_is_not_a_role_is_refused_like_one_that_was_never_declared():
    with pytest.raises(ExecutionRefused) as raised:
        declared().authorize(MODEL, EFFORT, role="coordinator")
    assert raised.value.code == "execution_role_unknown"


@pytest.mark.parametrize(
    ("role", "model", "effort", "field"),
    [
        # A parent on the child's pair: the exact shape of the observed failure, where both the
        # presence and the allowlist questions answer yes. Since the restore the two roles
        # differ in model and in effort, so this row is wrong on both counts at once.
        ("parent", MODEL, EFFORT, "model"),
        # Then one axis at a time. A wrong-effort case has to state a name that really differs
        # from the role it names or it proves nothing about the comparison: for the parent that
        # is the interim pair's xhigh, which the child still runs on, and for the child it is
        # the parent's max. The last row holds the child's own effort so that only the model is
        # wrong -- the coordinator's retry, which changed the model and kept the effort it
        # already had.
        ("parent", PARENT_MODEL, SUPERSEDED_PARENT[1], "reasoning_effort"),
        ("child", MODEL, PARENT_EFFORT, "reasoning_effort"),
        ("child", PARENT_MODEL, EFFORT, "model"),
    ],
)
def test_a_pair_that_is_not_this_roles_pair_is_refused(role, model, effort, field):
    with pytest.raises(ExecutionRefused) as raised:
        declared().authorize(model, effort, role=role)
    assert raised.value.code == "execution_role_mismatch"
    assert raised.value.field == field


def test_an_effort_name_belongs_to_its_model_and_never_stands_in_for_another():
    """No alias table exists, and this is the case that would have caught the retry that failed.

    A coordinator whose send was withheld changed the model to the parent's and kept the previous
    effort. Both spellings load as themselves, and each is refused for the other's role. The pair
    the parent ran on in the interim is the one whose effort name differs from the restored one,
    so it is the fixture this rule is checked against rather than an alternative still accepted.
    """
    superseded_model, superseded_effort = SUPERSEDED_PARENT
    policy = declared(roles={
        "parent": {"model": PARENT_MODEL, "reasoningEffort": PARENT_EFFORT},
        "child": {"model": superseded_model, "reasoningEffort": superseded_effort},
    }, allowed=False)
    assert policy.summary()["roles"]["parent"]["reasoningEffort"] == PARENT_EFFORT
    assert policy.summary()["roles"]["child"]["reasoningEffort"] == superseded_effort
    assert PARENT_EFFORT != superseded_effort
    assert policy.authorize(PARENT_MODEL, PARENT_EFFORT, role="parent").reasoning_effort == "max"
    with pytest.raises(ExecutionRefused):
        policy.authorize(PARENT_MODEL, superseded_effort, role="parent")
    with pytest.raises(ExecutionRefused):
        policy.authorize(superseded_model, PARENT_EFFORT, role="child")


def test_the_pair_a_role_used_to_run_on_is_refused_like_any_other_wrong_pair():
    """A superseded pair is not a second valid answer for its role.

    Once the file declares the new one, the old one is what a request citing that role must not
    carry -- which is the whole reason a pair change costs a file edit and not a code change.
    """
    policy = declared()
    with pytest.raises(ExecutionRefused) as raised:
        policy.authorize(*SUPERSEDED_PARENT, role="parent")
    assert raised.value.code == "execution_role_mismatch"
    assert raised.value.allowed == [PARENT_MODEL]


def test_the_pair_the_child_used_to_run_on_is_refused_on_the_model_alone():
    """The child kept its effort name when it moved, so only the model tells the pairs apart.

    No allowlist is declared, so the refusal can only come from the role question.
    """
    superseded_model, superseded_effort = SUPERSEDED_CHILD
    assert superseded_effort == EFFORT
    assert superseded_model != MODEL
    policy = declared(allowed=False)
    with pytest.raises(ExecutionRefused) as raised:
        policy.authorize(superseded_model, superseded_effort, role="child")
    assert raised.value.code == "execution_role_mismatch"
    assert raised.value.field == "model"
    assert policy.authorize(MODEL, EFFORT, role="child").model == MODEL


@pytest.mark.parametrize("missing", [None, "", "   "])
def test_presence_is_settled_before_the_role(missing):
    """An omitted value is reported as omitted, never as the wrong pair for its role."""
    with pytest.raises(ExecutionRefused) as raised:
        declared().authorize(missing, EFFORT, role="parent")
    assert raised.value.code in ("execution_setting_missing", "execution_setting_invalid")
    assert raised.value.field == "model"


def test_a_supervisor_keeps_the_pair_it_was_given_and_the_receipt_says_where_it_came_from():
    """Its model is the user's own selection, so policy declares no pair to compare against."""
    execution = declared(allowed=False).authorize("gpt-6-astra", "high", role="supervisor")
    assert (execution.model, execution.reasoning_effort) == ("gpt-6-astra", "high")
    assert execution.receipt["roleExpectation"]["expectation"] == "record"
    assert execution.receipt["roleExpectation"]["model"] is None


def test_a_supervisor_is_exempt_from_the_role_question_and_not_from_the_allowlist():
    """Declaring no pair for it says who chooses the model, not that the host stops checking."""
    with pytest.raises(ExecutionRefused) as raised:
        declared().authorize("gpt-6-astra", "high", role="supervisor")
    assert raised.value.code == "execution_not_allowed"


def test_a_policy_that_pins_the_supervisors_model_does_not_load():
    """The rule that keeps the parent policy from propagating upward is a load-time refusal."""
    with pytest.raises(ExecutionPolicyError) as raised:
        declared(roles={"supervisor": {"model": PARENT_MODEL, "reasoningEffort": PARENT_EFFORT}})
    assert "supervisor" in str(raised.value)


def test_roles_can_be_declared_without_imposing_an_allowlist_on_every_other_task():
    """Requiring one would narrow unrelated work as a side effect of a CRW decision."""
    policy = declared(allowed=False)
    assert policy.mode == "presence_only"
    assert policy.authorize(PARENT_MODEL, PARENT_EFFORT, role="parent").model == PARENT_MODEL
    # The approval question stays unanswered, so an unlisted pair naming no role still passes.
    assert policy.authorize(UNAPPROVED, "low").model == UNAPPROVED


def test_a_policy_declaring_neither_an_allowlist_nor_a_role_is_still_a_mistake():
    with pytest.raises(ExecutionPolicyError):
        ExecutionPolicy.from_mapping({})


def test_a_role_cannot_declare_a_value_longer_than_a_request_may_state():
    """Otherwise the policy loads and then refuses every request that names it."""
    from codex_thread_bridge import roles
    from codex_thread_bridge.execution import MAXIMUM

    assert roles.SETTING_MAXIMUM == MAXIMUM
    longest = "m" * MAXIMUM
    # No allowlist, so the only rule under test is the ceiling. With one present these
    # fixtures would be refused for naming a pair the allowlist omits, and the length case
    # would pass for the wrong reason.
    assert declared(
        roles={"parent": {"model": longest, "reasoningEffort": "max"}}, allowed=False
    )
    with pytest.raises(ExecutionPolicyError):
        declared(
            roles={"parent": {"model": longest + "m", "reasoningEffort": "max"}}, allowed=False
        )


def test_a_role_pair_the_allowlist_omits_is_refused_at_startup_not_at_creation():
    """Two sections of one file disagreeing, caught where both are readable.

    A declared role pair is still asked the allowlist question -- only an exception skips it --
    so this file describes a parent nobody can create: the request matches its role and then
    fails execution_not_allowed. It used to load cleanly and surface at the first creation
    attempt, as a refusal naming the allowlist rather than the contradiction that caused it.
    """
    with pytest.raises(ExecutionPolicyError) as raised:
        ExecutionPolicy.from_mapping({
            "allowed": [{"model": MODEL, "efforts": [EFFORT]}],
            "roles": {"parent": {"model": PARENT_MODEL, "reasoningEffort": PARENT_EFFORT}},
        })
    # It names the role and the pair, because those are what the operator has to change.
    assert "'parent'" in str(raised.value)
    assert PARENT_MODEL in str(raised.value)
    assert PARENT_EFFORT in str(raised.value)


def test_an_allowed_model_at_an_effort_the_role_needs_is_still_a_disagreement():
    """Efforts are scoped to their model everywhere else, so a model-only match is not one.

    The superseded parent pair is the fixture: its model may be listed while the effort the
    role declares is not, and reading only the model key would approve a file whose parent
    still cannot be created.
    """
    superseded_model, superseded_effort = SUPERSEDED_PARENT
    with pytest.raises(ExecutionPolicyError) as raised:
        ExecutionPolicy.from_mapping({
            "allowed": [{"model": superseded_model, "efforts": ["high"]}],
            "roles": {"parent": {"model": superseded_model,
                                 "reasoningEffort": superseded_effort}},
        })
    assert superseded_effort in str(raised.value)


def test_a_supervisor_is_not_held_to_the_allowlist_at_load_because_it_declares_no_pair():
    """It has nothing to compare. Its authorization is its own recorded settings, and a check
    that invented a pair for it would be the pinning the roles section refuses outright."""
    policy = ExecutionPolicy.from_mapping({
        "allowed": [{"model": MODEL, "efforts": [EFFORT]}],
        "roles": {"supervisor": {"expectation": "record"},
                  "child": {"model": MODEL, "reasoningEffort": EFFORT}},
    })
    assert policy.summary()["mode"] == "allowlist"


def test_roles_may_still_be_declared_with_no_allowlist_at_all():
    """The check reads the allowlist, so it cannot become a reason to require one. An
    allowlist constrains every task on the host, which is why declaring roles never forces
    one into existence."""
    policy = declared(allowed=False)
    assert policy.summary()["mode"] == "presence_only"
    assert policy.authorize(PARENT_MODEL, PARENT_EFFORT, role="parent").model == PARENT_MODEL


def test_an_exception_answers_the_role_question_and_the_receipt_names_it():
    """The user's explicit authorization wins, and an override is never silent."""
    policy = ExecutionPolicy.from_mapping(
        {
            **roles_policy(),
            "exceptions": {
                "one-task": {
                    "model": UNAPPROVED,
                    "reasoningEffort": "high",
                    "cwd": ["/tmp"],
                    "role": "parent",
                }
            },
        },
        digest="roles-digest",
    )
    execution = policy.authorize(UNAPPROVED, "high", cwd="/tmp", exception="one-task",
                                role="parent")
    assert execution.model == UNAPPROVED
    assert execution.receipt["roleExpectation"]["overriddenBy"] == "one-task"


@pytest.mark.parametrize(
    ("declared_role", "cited_role"),
    [
        # Written for a parent, cited by a child working in the same directory.
        ("parent", "child"),
        # Written without a role, cited by a request that names one. A directory is not a task
        # identity, so this is the loophole that would otherwise let any role in that directory
        # skip its own check.
        (None, "parent"),
    ],
)
def test_an_exception_does_not_cover_a_role_it_was_not_written_for(declared_role, cited_role):
    entry = {"model": UNAPPROVED, "reasoningEffort": "high", "cwd": ["/tmp"]}
    if declared_role is not None:
        entry["role"] = declared_role
    policy = ExecutionPolicy.from_mapping(
        {**roles_policy(), "exceptions": {"one-task": entry}}, digest="roles-digest"
    )
    with pytest.raises(ExecutionRefused) as raised:
        policy.authorize(UNAPPROVED, "high", cwd="/tmp", exception="one-task", role=cited_role)
    assert raised.value.code == "execution_exception_out_of_scope"


def test_an_exception_written_before_roles_existed_still_works_for_a_caller_that_names_none():
    policy = ExecutionPolicy.from_mapping(
        {
            **roles_policy(),
            "exceptions": {
                "one-task": {"model": UNAPPROVED, "reasoningEffort": "high", "cwd": ["/tmp"]}
            },
        },
        digest="roles-digest",
    )
    assert policy.authorize(
        UNAPPROVED, "high", cwd="/tmp", exception="one-task"
    ).model == UNAPPROVED



def test_an_unconfigured_host_still_demands_a_stated_pair():
    """The presence question has an answer everywhere; only approval needs a configured file."""
    policy = ExecutionPolicy.from_environment({})
    # An empty roles map is the honest answer to "which roles does this host declare", and it is
    # what tells a caller that naming one here would be refused rather than silently unchecked.
    assert policy.summary() == {"mode": "presence_only", "digest": None, "roles": {}}
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
    ("role", "pair", "code"),
    [
        ("parent", {"model": MODEL, "reasoning_effort": EFFORT}, "execution_role_mismatch"),
        ("parent", {"model": PARENT_MODEL, "reasoning_effort": SUPERSEDED_PARENT[1]},
         "execution_role_mismatch"),
        ("reviewer", {"model": MODEL, "reasoning_effort": EFFORT}, "execution_role_unknown"),
    ],
)
async def test_a_creation_for_the_wrong_role_never_reaches_the_host(
    configured_bridge, fake_server, tmp_path, role, pair, code
):
    """Decided locally, so the refusal costs nothing and leaves nothing behind.

    The ledger row is the structural half of the claim: authorization runs before begin(), and
    every RPC is issued after it, so an id that is still unknown could not have reached the host.
    """
    fake, _ = fake_server
    bridge = configured_bridge(roles_policy())
    with pytest.raises(ExecutionRefused) as raised:
        await bridge.create_thread("wrong-role", str(tmp_path), prompt="work", role=role, **pair)
    assert raised.value.code == code
    assert fake.calls == [], "a refused creation reached the host"
    assert fake.count("turn/start") == 0
    with pytest.raises(ValueError, match="Unknown request_id"):
        bridge.ledger.get("wrong-role")


async def test_a_corrected_role_request_succeeds_under_the_same_id(
    configured_bridge, fake_server, tmp_path
):
    """Retry re-authorizes rather than inheriting the first attempt's decision."""
    fake, _ = fake_server
    bridge = configured_bridge(roles_policy())
    with pytest.raises(ExecutionRefused):
        await bridge.create_thread("retried", str(tmp_path), prompt="work", role="parent",
                                   **EXECUTION)
    receipt = await bridge.create_thread(
        "retried", str(tmp_path), prompt="work", role="parent",
        model=PARENT_MODEL, reasoning_effort=PARENT_EFFORT,
    )
    assert receipt["status"] == "accepted"
    start = next(params for name, params in fake.calls if name == "thread/start")
    assert start["model"] == PARENT_MODEL
    assert start["config"]["model_reasoning_effort"] == PARENT_EFFORT
    assert receipt["executionPolicy"]["role"] == "parent"
    assert receipt["executionPolicy"]["roleExpectation"]["model"] == PARENT_MODEL


async def test_naming_no_role_leaves_the_request_identical_to_one_made_before_roles_existed(
    configured_bridge, fake_server, tmp_path
):
    """A retained receipt only replays while its request fingerprint is unchanged."""
    fake, _ = fake_server
    bridge = configured_bridge(roles_policy())
    first = await bridge.create_thread("stable", str(tmp_path), prompt="work", **EXECUTION)
    assert first["status"] == "accepted"
    replayed = await bridge.create_thread("stable", str(tmp_path), prompt="work", **EXECUTION)
    assert replayed.get("replayed") is True
    assert fake.count("thread/start") == 1


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
        "role": None,
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


async def test_an_exception_on_the_resume_path_must_state_its_directory(
    configured_bridge, fake_server, tmp_path
):
    """cwd is optional on a send until an exception is cited, and then it is not.

    An exception is bound to directories, so a send that names one without a cwd can never match.
    The refusal says which argument is missing rather than only listing the covered directories.
    """
    fake, _ = fake_server
    mapping = policy_for(tmp_path)
    # The directory the exception itself declares, so the test and the policy cannot disagree.
    covered = mapping["exceptions"]["one-task"]["cwd"][0]
    bridge = configured_bridge(mapping)
    created = await bridge.create_thread(
        "c",
        str(tmp_path),
        model=UNAPPROVED,
        reasoning_effort="high",
        policy_exception="one-task",
    )
    settled = len(fake.calls)
    with pytest.raises(ExecutionRefused) as raised:
        await bridge.send_message_to_thread(
            "no-cwd",
            created["threadId"],
            "hello",
            {"model": UNAPPROVED, "reasoning_effort": "high"},
            "one-task",
        )
    assert raised.value.code == "execution_exception_out_of_scope"
    assert raised.value.field == "cwd", "the refusal names the argument the caller must add"
    assert "must also state its cwd" in str(raised.value)
    assert fake.calls[settled:] == []

    delivered = await bridge.send_message_to_thread(
        "with-cwd",
        created["threadId"],
        "hello",
        {
            "model": UNAPPROVED,
            "reasoning_effort": "high",
            "cwd": covered,
        },
        "one-task",
    )
    assert delivered["status"] == "accepted"
    assert delivered["executionPolicy"]["exception"] == "one-task"



# ------------------------------------------------- residency and what an echo can prove (CRW-127)
#
# Three project parents were asked to resume on a new pair. The one the host had not loaded came
# back reporting the new pair; the two it had loaded came back reporting the old one and their
# messages were withheld. Nobody recorded residency at the time, so the cause is not established,
# and these cases deliberately do not assert one. They run the same request against both candidate
# hosts and assert that the bridge is safe under either.


async def test_a_loaded_thread_reports_its_own_pair_and_the_message_is_withheld(
    bridge, fake_server, tmp_path
):
    """The observed idle case: the host answers with what the thread is actually on."""
    fake, _ = fake_server
    created = await bridge.create_thread("resident", str(tmp_path), **EXECUTION)
    thread_id = created["threadId"]
    fake.resident = {thread_id}
    fake.resume_adopts = True
    delivered = await bridge.send_message_to_thread(
        "resident-send", thread_id, "work",
        {"model": PARENT_MODEL, "reasoning_effort": PARENT_EFFORT},
    )
    assert delivered["status"] == "failed"
    assert delivered["statusBeforeResume"] == "idle"
    assert "echoIndependence" not in delivered
    assert delivered["settings"]["findings"][0]["code"] == "settings_not_preserved"
    assert fake.count("turn/start") == 0


@pytest.mark.parametrize("adopts", [True, False])
async def test_an_unloaded_thread_never_lets_an_echo_stand_as_proof_of_preservation(
    bridge, fake_server, tmp_path, adopts
):
    """The observed notLoaded case, run against both candidate hosts.

    Where the host adopts, the echo repeats the request and the settings agree. Where it does not,
    they disagree and the message is withheld. Either way the receipt records that the thread was
    not loaded, so an agreeing echo is recorded as agreement and never as preservation.
    """
    fake, _ = fake_server
    created = await bridge.create_thread("absent", str(tmp_path), **EXECUTION)
    thread_id = created["threadId"]
    fake.resident = set()
    fake.resume_adopts = adopts
    delivered = await bridge.send_message_to_thread(
        "absent-send", thread_id, "work",
        {"model": PARENT_MODEL, "reasoning_effort": PARENT_EFFORT},
    )
    assert delivered["statusBeforeResume"] == "notLoaded"
    assert delivered["echoIndependence"] == "not_established"
    if adopts:
        assert delivered["status"] == "accepted"
        assert delivered["settings"]["findings"] == []
    else:
        assert delivered["status"] == "failed"
        assert delivered["settings"]["findings"][0]["code"] == "settings_not_preserved"
        assert fake.count("turn/start") == 0


# The three mutating paths each have to ask the role question for themselves. Without a case per
# path, deleting the role argument from two of them leaves the suite green.


async def test_a_resume_for_the_wrong_role_never_reads_or_resumes_the_thread(
    configured_bridge, fake_server, tmp_path
):
    fake, _ = fake_server
    bridge = configured_bridge(roles_policy())
    created = await bridge.create_thread(
        "send-role-setup", str(tmp_path), role="child", **EXECUTION
    )
    before = len(fake.calls)
    with pytest.raises(ExecutionRefused) as raised:
        await bridge.send_message_to_thread(
            "send-wrong-role", created["threadId"], "work",
            {"model": MODEL, "reasoning_effort": EFFORT}, None, "parent",
        )
    assert raised.value.code == "execution_role_mismatch"
    assert fake.calls[before:] == [], "a refused send reached the host"
    assert fake.count("turn/start") == 0
    with pytest.raises(ValueError, match="Unknown request_id"):
        bridge.ledger.get("send-wrong-role")


async def test_a_worktree_launch_for_the_wrong_role_creates_nothing(
    configured_bridge, fake_server, tmp_path
):
    """Authorization runs before Worktree.validate, so no checkout exists to clean up."""
    fake, _ = fake_server
    bridge = configured_bridge(roles_policy())
    destination = tmp_path / "never-created"
    with pytest.raises(ExecutionRefused) as raised:
        await bridge.create_worktree_thread(
            "worktree-wrong-role",
            str(tmp_path),
            "0" * 40,
            str(destination),
            "bridge-managed-retained",
            "workspace-write",
            {
                "type": "workspaceWrite",
                "networkAccess": False,
                "writableRoots": [],
                "excludeTmpdirEnvVar": False,
                "excludeSlashTmp": False,
            },
            model=MODEL,
            reasoning_effort=EFFORT,
            role="parent",
        )
    assert raised.value.code == "execution_role_mismatch"
    assert not destination.exists()
    assert fake.calls == []
    with pytest.raises(ValueError, match="Unknown request_id"):
        bridge.ledger.get("worktree-wrong-role")


async def test_a_named_supervisor_the_host_has_not_loaded_is_not_resumed_at_all(
    configured_bridge, fake_server, tmp_path
):
    """Every other role's pair is derived from policy; a supervisor's is the user's choice.

    So a resume that transmits the RECORDED pair is safe for a parent or a child even where the
    host applies it, and is exactly the mutation to avoid for a supervisor whose recorded pair may
    have gone stale against a change the user made.
    """
    fake, _ = fake_server
    bridge = configured_bridge(roles_policy(allowed=False))
    created = await bridge.create_thread(
        "supervisor", str(tmp_path), model="gpt-6-astra", reasoning_effort="high",
        role="supervisor",
    )
    thread_id = created["threadId"]
    fake.resident = set()
    fake.resume_adopts = True
    delivered = await bridge.send_message_to_thread(
        "supervisor-send", thread_id, "work",
        {"model": "gpt-6-astra", "reasoning_effort": "high"}, None, "supervisor",
    )
    assert delivered["status"] == "failed"
    assert delivered["rpcError"]["code"] == "unverified_pair_for_unloaded_thread"
    assert fake.count("thread/resume") == 0
    assert fake.count("turn/start") == 0


async def test_an_exception_is_not_evidence_that_a_pair_matches_its_roles_policy(
    configured_bridge, fake_server, tmp_path
):
    """An exception exists to SKIP the role comparison, so it cannot stand in for one.

    Gating on "the named role declares a pair" rather than on what actually happened let an
    exception-authorized pair through whenever that role separately declared one, which is how a
    parent exception carrying Astra/high reached a thread whose declared pair was SWE-2/max. The
    guard reads the provenance the authorization recorded instead.
    """
    fake, _ = fake_server
    bridge = configured_bridge({
        **roles_policy(allowed=False),
        "exceptions": {
            "one-task": {
                "model": "gpt-6-astra",
                "reasoningEffort": "high",
                "cwd": [str(tmp_path.resolve())],
                "role": "parent",
            }
        },
    })
    created = await bridge.create_thread(
        "excepted", str(tmp_path), model="gpt-6-astra", reasoning_effort="high",
        policy_exception="one-task", role="parent",
    )
    fake.resident = set()
    fake.resume_adopts = True
    delivered = await bridge.send_message_to_thread(
        "excepted-send", created["threadId"], "work",
        {"model": "gpt-6-astra", "reasoning_effort": "high", "cwd": str(tmp_path.resolve())},
        "one-task", "parent",
    )
    assert delivered["status"] == "failed"
    assert delivered["rpcError"]["code"] == "unverified_pair_for_unloaded_thread"
    assert fake.count("thread/resume") == 0
    assert fake.count("turn/start") == 0


async def test_a_send_that_names_no_role_is_not_guarded_here_and_that_boundary_is_deliberate(
    configured_bridge, fake_server, tmp_path
):
    """What this bridge cannot know, it does not pretend to guard.

    Blocking every unnamed send on a host that declared a role for unrelated tasks would stop
    work that has nothing to do with this policy, and the bridge has no way to tell an unnamed
    supervisor from a task with no role at all: it cannot read scope bindings. So the unnamed
    case is the relay's to refuse, and this case exists to record that boundary rather than to
    leave it as an assumption someone has to notice.
    """
    fake, _ = fake_server
    bridge = configured_bridge(roles_policy())
    created = await bridge.create_thread("unrelated", str(tmp_path), **EXECUTION)
    fake.resident = set()
    fake.resume_adopts = True
    delivered = await bridge.send_message_to_thread(
        "unrelated-send", created["threadId"], "work", dict(EXECUTION)
    )
    assert delivered["status"] == "accepted"
    assert delivered["echoIndependence"] == "not_established"


async def test_a_verified_role_pair_may_still_be_sent_to_a_thread_the_host_has_not_loaded(
    configured_bridge, fake_server, tmp_path
):
    """Adoption is only dangerous where the pair was not derived from policy in the first place."""
    fake, _ = fake_server
    bridge = configured_bridge(roles_policy())
    created = await bridge.create_thread(
        "verified", str(tmp_path), model=PARENT_MODEL, reasoning_effort=PARENT_EFFORT,
        role="parent",
    )
    fake.resident = set()
    fake.resume_adopts = True
    delivered = await bridge.send_message_to_thread(
        "verified-send", created["threadId"], "work",
        {"model": PARENT_MODEL, "reasoning_effort": PARENT_EFFORT}, None, "parent",
    )
    assert delivered["status"] == "accepted"
    assert delivered["echoIndependence"] == "not_established"


async def test_a_host_that_declared_no_roles_keeps_exactly_its_previous_send_behaviour(
    bridge, fake_server, tmp_path
):
    """Nothing changes for a host that never opted in, which is the merge-day case.

    The guard keys on a named role rather than on the host having declared any, so this is the
    weaker of the two compatibility claims; the stronger one is the unnamed send on a host that
    HAS declared roles, covered separately above.
    """
    fake, _ = fake_server
    created = await bridge.create_thread("legacy", str(tmp_path), **EXECUTION)
    fake.resident = set()
    fake.resume_adopts = True
    delivered = await bridge.send_message_to_thread(
        "legacy-send", created["threadId"], "work", dict(EXECUTION)
    )
    assert delivered["status"] == "accepted"
    assert fake.count("turn/start") == 1

def test_a_role_that_is_not_the_supervisor_cannot_opt_out_of_its_own_pair():
    """Declaring `record` elsewhere would exempt that role from the only check that names it."""
    with pytest.raises(ExecutionPolicyError) as raised:
        declared(roles={"parent": {"expectation": "record"}})
    assert "parent" in str(raised.value)


def test_the_supervisor_cannot_be_given_a_pinned_pair_through_the_expectation_key():
    with pytest.raises(ExecutionPolicyError):
        declared(roles={"supervisor": {"expectation": "pair", "model": MODEL,
                                       "reasoningEffort": EFFORT}})


async def test_naming_no_role_produces_the_same_request_identity_as_before_roles_existed(
    configured_bridge, fake_server, tmp_path
):
    """The compatibility claim is about the REQUEST identity, which is what replay keys on.

    A receipt retained before this argument existed replays only while its parameters hash to the
    same value. Observing a replay through this implementation would prove nothing, because an
    always-present `role: None` would be equally consistent with itself. So this watches the
    parameters the bridge actually hands the ledger, and separately pins that the two forms do
    NOT hash alike -- which is what makes the omission load-bearing rather than cosmetic.
    """
    from codex_thread_bridge.ledger import Ledger

    fake, _ = fake_server
    bridge = configured_bridge(roles_policy())
    seen = []
    original = bridge.ledger.begin

    def watching(request_id, method, params, **kwargs):
        seen.append((method, dict(params)))
        return original(request_id, method, params, **kwargs)

    bridge.ledger.begin = watching
    await bridge.create_thread("no-role", str(tmp_path), **EXECUTION)
    await bridge.create_thread(
        "with-role", str(tmp_path), role="parent",
        model=PARENT_MODEL, reasoning_effort=PARENT_EFFORT,
    )
    recorded = {method_params[1].get("role", "absent") for method_params in seen}
    assert "absent" in recorded, "the bridge sent a role key for a caller that named none"
    assert "parent" in recorded

    mark = Ledger._fingerprint
    base = {"cwd": "/w", "sandbox": "read-only", "model": MODEL, "reasoning_effort": EFFORT}
    assert mark("id", "create_thread", base) != mark("id", "create_thread", {**base, "role": None})
