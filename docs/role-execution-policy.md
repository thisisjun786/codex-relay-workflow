# Role execution policy

CRW runs at three levels and each level is meant to run on a different model and reasoning
effort. Until now nothing in the code knew that. The bridge asked whether a pair was stated and
whether this host approved it, and neither question is whether the pair belongs to the ROLE the
task is being created for. So a project parent created on the wrong model passed every check that
existed, and a child verifying a send against a pair frozen at its parent's creation withheld a
correct message once the user changed that parent.

This document is where the policy lives for a reader. The enforced values live in one place only:
the `roles` section of the operator-owned execution policy file the bridge server reads from its
own environment. Nothing in either package ships a default pair for any role.

## A role is a binding, not a title

A supervisor is bound to an initiative, a parent to a project, a child to an issue, each by stable
ID. That binding is what makes a role; a title, a folder, a branch or a chat link is not. The
relay records it in `scope_bindings` and the role ids here are the same three strings:
`supervisor`, `parent`, `child`.

## The decision this policy carries

| Role | Model | Reasoning effort | Who decides |
| --- | --- | --- | --- |
| `supervisor` | Astra | as selected | Jun, directly |
| `parent` | `devin/swe-2` | `max` | this policy |
| `child` | `anthropic/claude-opus-5` | `xhigh` | this policy |

**This table is a record of the product decision, not a default the code applies.** The decision
itself is Linear CRW-127; the values that are actually enforced are the ones in the host's policy
file. If the two disagree, the file is what runs and the disagreement is the bug.

The supervisor row is deliberately not a pair. Its model is Jun's selection, so the policy
declares `{"expectation": "record"}` for it and the supervisor's authorization is whatever its own
recorded settings say. A `supervisor` entry that tries to carry a `model` or a `reasoningEffort` does
not load: the server refuses it at startup. That is how "the parent policy is never propagated to
the supervisor" is a mechanism rather than a sentence someone has to remember.

## `max` and `xhigh` are two values

The host's model catalog reports SWE-2 at medium, high, max and ultra, and the parent's requested
value is exactly `max`. Opus 5 runs the child at `xhigh`. These are different strings naming
different levels in different catalogs, and nothing maps one to the other. There is no alias
table, no normalisation step, and every comparison in both packages is exact string equality. A
request that states `xhigh` for a parent is refused, and so is one that states `max` for a child.

This is worth stating because the failure it prevents already happened in prose rather than in
code: a coordinator retrying a withheld send changed the model and kept the old effort, and the
host refused it again.

## The file

The policy file is named by `CODEX_THREAD_BRIDGE_EXECUTION_POLICY` in the bridge server process's
own environment. It is read once, at startup, and no tool argument can reach it — a caller that
can approve itself has not been checked by anyone. The `roles` section is new; `allowed` and
`exceptions` are unchanged.

EXAMPLE ONLY. The values below are this host's decision at the time of writing, reproduced to
show the shape. Do not copy it as a default.

    {
      "roles": {
        "supervisor": {"expectation": "record"},
        "parent": {"model": "devin/swe-2", "reasoningEffort": "max"},
        "child":  {"model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"}
      }
    }

`allowed` stays optional when `roles` is declared. That matters: an allowlist restricts every task
on the host, so forcing one in order to declare roles would block other legitimate work. With no
`allowed` the mode stays `presence_only` and only cited roles are enforced. A file declaring
neither `allowed` nor `roles` is an error, because a policy that declares nothing is a mistake and
not an empty policy.

An exception may now carry a `role`. A request that cites a role may cite only an exception
declaring that same role, and an exception with no `role` key may be cited only by a request that
cites none. A directory is not a task identity, and without this an exception written for one task
could be cited by any task in the same directory under any role.

## Reading the policy instead of remembering it

Call `get_capabilities`. Its `executionPolicy` reports the mode, the digest and the declared roles
with their pairs. State the pair it reports for the role being created, and pass the `role`
argument so the host checks the answer rather than trusting the caller.

Never carry a pair from memory, from another project, or from a document — including this one.
That is the habit the incident was made of.

## What each refusal means

| Code | What happened | What to do |
| --- | --- | --- |
| `execution_setting_missing` / `execution_setting_invalid` | the model or the effort was absent or blank | state both; no host default is ever inherited |
| `execution_role_unknown` | a role was cited that this host's policy does not declare | declare it in the host's execution policy; there is no fallback pair |
| `execution_role_mismatch` | the stated pair is not that role's pair | state the pair the policy declares for the role |
| `execution_exception_out_of_scope` | the exception does not cover this directory, or its role and the cited role disagree | cite an exception written for this role and directory |
| `role_policy_unconfigured` | the relay process has no declared role policy and the recipient is role-bound | set the variable for the relay process and restart; withheld deliveries resume by themselves |
| `role_binding_mismatch` | a task's cited role and its bound role disagree, or its recorded pair is not that role's pair | fix the binding or the creation; do not re-record over it |
| `settings_record_stale_for_role` | the recorded authorization is not the role's current pair | re-record it from a user-attributed source |

Every refusal above is decided before any call that costs inference. A creation or resume refused
for one of the first four reasons issues no RPC at all, leaves no ledger row, and for the worktree
path leaves no worktree.

## Activation

Checked-in policy is not enforcement. Declaring the `roles` section on a host is an operator step,
and until it is done nothing here is enforced: a cited role is refused, and a role-bound relay
delivery is withheld with `role_policy_unconfigured`. That withhold is retry-safe on the ordinary
recheck cadence, so configuring the variable and restarting the process resumes every withheld
delivery with nothing lost and no turn started in the meantime.

Source merge, CI, installation, host configuration and provider execution are five separate
claims. This document describes the first three. It is not evidence that any host is configured,
and a host reporting a model back is evidence that it RECORDED the request, never that a provider
served it.

## Changing an existing task

A model change on a running task is a UI action plus a re-record. The code does not perform it.

`thread/resume` carries settings and reports what the thread is on, and `turn/start` is never used
to bind them because its response reports nothing readable back. So the sequence is: change the
setting in the UI, then re-record the authorization with
`relay settings record --task <id> --source user_transition --settings @file`. That `source` value
is the record of the UI action, and `settings show` and `doctor` surface it. A host observation is
evidence of the actual value; it is never a new approval, so nothing adopts a drifting setting on
its own.

Two limits are recorded rather than smoothed over. When the host reports a thread as `notLoaded`,
a resume that transmits a pair cannot distinguish preservation from adoption, so the attempt
records `echoIndependence: "not_established"` and no report calls those settings verified as
preserved. And a `supervisor` observed `notLoaded` is not resumed at all: its pair is the one the
policy does not derive, so transmitting a recorded pair could revert a change Jun made in the UI.

An unsupported transition is recorded as what it is — the host reported the old pair, no turn was
started, applying it needs the UI action — and no guard is bypassed to make it look applied.
