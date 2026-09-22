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

That separation is what a change of pair costs, and the parent row has now paid it twice in a
day. It ran on `devin/swe-2` at `max`, moved to `xai/grok-4.6` at `xhigh` on 2026-09-21, and was
restored to `devin/swe-2` at `max` later that same day. Each move was an edit to the policy file
and a restart, with no line of code changed, because no pair is written in code to go stale. A
superseded pair is not a second valid answer: once the file declares the current one, every other
pair is refused for that role like any other wrong pair, and a superseded pair is kept as a
regression fixture proving exactly that rather than as an alternative the checks still accept.

The supervisor row is deliberately not a pair. Its model is Jun's selection, so the policy
declares `{"expectation": "record"}` for it and the supervisor's authorization is whatever its own
recorded settings say. A `supervisor` entry that tries to carry a `model` or a `reasoningEffort` does
not load: the server refuses it at startup. That is how "the parent policy is never propagated to
the supervisor" is a mechanism rather than a sentence someone has to remember.

## An effort name belongs to its model

Reasoning-effort names are catalog values, and two models that both offer a name do not thereby
offer the same thing. Nothing maps one onto another: there is no alias table, no normalisation
step, and every comparison in both packages is exact string equality.

The clearest case is the parent row itself. The host's catalog reports SWE-2 at medium, high, max
and ultra, and the parent's requested value under that model is exactly `max`, while Opus 5 runs
the child at `xhigh`. A request stating `xhigh` for that parent is refused, and so is one stating
`max` for a child. The interim grok pair made the two names coincide at `xhigh` under different
models, which changed nothing about the rule; the restored pair makes them differ again, which is
why a pair whose two effort names differ is the case the regression fixture is built on.

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

Where both are present they have to agree. A declared role pair is still asked the allowlist
question — only an exception skips it, because an exception is the operator writing a pair down —
so a file declaring `parent` on a pair its own `allowed` omits describes a role nobody could
create: the request matches its role and then fails `execution_not_allowed`. That is refused when
the policy is read rather than at the first creation attempt, and neither section is made to win:
letting `roles` authorize its own pair would turn editing it into a way to widen the allowlist,
and letting `allowed` win would silently unmake a role.

An exception may now carry a `role`. A request that cites a role may cite only an exception
declaring that same role, and an exception with no `role` key may be cited only by a request that
cites none. A directory is not a task identity, and without this an exception written for one task
could be cited by any task in the same directory under any role.

## Moving one task without moving its role

A role's pair is the default for every task holding that role, so moving one task must not move
the role. That is what a named exception is for, and it is the only supported way to express it:
the operator writes an id, its single pair and the directories it covers, a request cites the id,
and nothing else about the role changes.

None is in force as this is written. The one this section was written around has ended: Jun
authorized the CRW-127 coordinator, on 2026-09-21, to receive its idle callbacks on
`ollama-cloud/glm-5.3` at `xhigh` while every other project parent stayed on the parent pair, and
that trial is over. The shape it took is still the shape:

    {
      "exceptions": {
        "<id you choose>": {
          "model": "ollama-cloud/glm-5.3",
          "reasoningEffort": "xhigh",
          "cwd": ["/absolute/path/to/that/checkout"],
          "role": "parent",
          "reason": "who authorized it, when, and that the parent default does not move"
        }
      }
    }

The task id and the directory are deliberately not written here. They are operational values,
they go stale, and the policy file on the host is the only place either is read from; this
document would be a second source competing with it. The authorization itself belongs to the
Linear record that carries the decision.

Be exact about how far that reaches, because the section title overstates it and the mechanism
does not. The policy file has no task-identity field, so an exception is scoped by role and by
directory and by nothing else. The `role` key means only a request citing `parent` may cite this
id, so a child or a supervisor naming it is refused rather than quietly covered. The `cwd` list
means a parent working somewhere else is refused even though it holds the right role. What those
two together do NOT give you is exclusivity: a second parent running in a covered directory could
cite the same id and be authorized. Where that matters, give the exception a directory only one
task works in, and treat "one task" as a property of how you scoped it rather than as something
the checks enforce.

The `roles` section is untouched either way. A reader asking what a project parent runs on still
gets `devin/swe-2` at `max`, because an exception is an exemption from the answer and never a
replacement for it.

The receipt says so too. A creation or send citing this id records `exception` and, under
`roleExpectation`, the pair the role WOULD have required together with `overriddenBy`. A reader of
that receipt can see that an expectation existed and exactly which authorization replaced it, so
"this parent is on a different model" is never something a later reader has to infer.

Two consequences worth stating plainly. An exception is never inherited: children created by an
excepted parent are still children and still run the child pair, because the exception authorizes
a pair and carries no other privilege — it relaxes nothing about sandbox, approvals or scope. And
the citation is a fact about a task, not about a pair: it is carried forward while it is still
doing work and is dropped only when the operator says so, never because the two pairs happen to
match.

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

### Turning it on

Four steps, and the order matters because each one is a separate fact.

1. Write the `roles` section into the file `CODEX_THREAD_BRIDGE_EXECUTION_POLICY` names.
2. Set that variable for **every** process that asks a role question: the bridge MCP server, and
   the relay's daemon, CLI and Stop hook. They are separate processes with separate environments,
   and one that misses it refuses role-bound deliveries rather than skipping the check.
3. Restart them. Each holds one snapshot, so an edit to the file changes nothing until it does.
4. Read the result back rather than assuming it: `get_capabilities` reports the bridge's declared
   roles and its policy digest, and `relay doctor` reports the digest the relay resolved. The two
   digests agreeing is what says both processes read the same file; the relay cannot fetch the
   bridge's for itself, because that value comes from an MCP tool and the relay's transport
   speaks App Server RPC.

Nothing before step 4 is evidence. A file written is not a process reading it, a process reading
it is not the other process reading the same one, and either of those is a different fact from a
provider actually serving the model a host recorded.

### The relay's daemon is told once, not once per shell

Step 2 used to mean "export it in whatever shell you restart from", and that made the policy a
service runs on a property of whoever last typed the command: the same `service restart`, typed
in two terminals, produced a service enforcing roles and a service withholding every role-bound
delivery. So the relay's supervised daemon has a declaration of its own:

    codex-session-relay --state <dir> service declare --execution-policy /path/to/execution-policy.json

It is read back before it is recorded — a declaration that does not resolve is refused here
rather than discovered at the next restart by a worker that then withholds everything — and every
later `service start` and `service restart` launches its daemon with it, whatever the calling
shell carries. Workers inherit it from the supervisor that spawned them.

It names the FILE. Editing the policy at that path still takes effect at the next restart, for
the same reason it always did, and this record is not a second place a policy can be written.

Where a shell and the declaration disagree, the launch is refused and names both files, before
anything is stopped. Choosing between them by preference is the failure this ends, and a restart
that stopped a service and then refused to start it would turn a question into an outage. Unset
the variable, or declare the other file.

The reach is exactly the relay's own supervised daemon and its workers. The relay CLI, the Stop
hook and the bridge MCP server each still read their own environment, which is why `doctor`
reports three separate readings: `rolePolicy` is what that CLI process resolved, `workerPolicy`
is what the serving worker resolved and is the effective one, and `launchPolicy` is the input the
next daemon would be given. A declaration made while a service is running is a pending change
until that service restarts, and `service status` says so rather than implying it is in force.

Deliveries held while the policy was unresolved are not lost. The hold is retry-safe on the
ordinary recheck cadence, so they resume on the next pass once the process can read the policy,
with no turn started in the meantime and nothing to replay by hand.

## Changing an existing task

A model change on a running task is a UI action plus a re-record. The code does not perform it.

`thread/resume` carries settings and reports what the thread is on, and `turn/start` is never used
to bind them because its response reports nothing readable back. So the sequence is: change the
setting in the UI, then re-record the authorization with
`codex-session-relay settings-record --task <id> --source user_transition --settings @file`.
That `source` value is the record of the UI action, and `settings-show` and `doctor` surface it.
A host observation is evidence of the actual value; it is never a new approval, so nothing adopts
a drifting setting on its own.

Two limits are recorded rather than smoothed over. When the host reports a thread as `notLoaded`,
a resume that transmits a pair cannot distinguish preservation from adoption, so the attempt
records `echoIndependence: "not_established"` and no report calls those settings verified as
preserved.

And where the pair was not compared against the role's declared pair, a `notLoaded` thread is not
resumed at all. That covers a `supervisor`, whose pair is the one the policy deliberately does not
derive, and any request citing an exception, since an exception exists to skip that comparison.
Transmitting such a pair could restore a value the user has since changed.

This guard is only as good as what the sender says, and the bridge is explicit about where that
stops. It reads no scope binding, so on a send that names no role it cannot tell an unnamed
supervisor from a task that has no role at all, and refusing both would stop unrelated work on
every host that declared a role for something else. The relay resolves the recipient's role from
its binding and owns that refusal. A send through the bridge naming no role is therefore not
covered by this guard, and that is a boundary rather than an oversight.

An unsupported transition is recorded as what it is — the host reported the old pair, no turn was
started, applying it needs the UI action — and no guard is bypassed to make it look applied.
