# Start policy and child cap

One adjudication settles how a run starts. It happens after the project baseline and before the
first child is created, and what it decides is recorded where the next session will read it. A
value the adjudication can settle is never put to the user as a question; a value it cannot settle
is asked before anything is created rather than after.

This reference belongs to [crw-run](../SKILL.md#prepare-and-dispatch). The standing cap and its
precedence are in [Default parent start policy](../../crw-plan/references/integrations.md#default-parent-start-policy),
and the goal side of the compatibility field is in
[Parent goal lifecycle](../../crw-loop/references/parent-goal.md#record-the-start-adjudication).
Nothing here adds a store, a questionnaire, a planning document or a scheduler: the fields below
are written under the settings and authorization entries the
[coordination record](task-packet.md#coordination-record) already keeps, and the facts that reach
past this project are read from the records that already own them.

## Roles and the goal each one opens

Three roles run this workflow, each bound to one Linear level, and the role decides what kind of
goal the task opens. The bindings themselves are in
[Supervisor, parent and child scope](../../crw-plan/references/integrations.md#supervisor-parent-and-child-scope);
what follows is the goal convention those roles carry.

| Role | Native goal | Implementation loop | What it never takes |
| --- | --- | --- | --- |
| Initiative management task | None, and no automatic loop | None | Development, technical acceptance and merge judgment. It talks with the user, relays requests, reads parent state, resumes within the approved scope, and summarises. |
| Project parent | Creates or reuses its own, aimed at completing the approved project scope, with automatic continuation | None. It builds no CXC goalplan and no FSM | A fabricated source change to close its goal |
| Issue child | Creates or reuses its own for the issue scope | Keeps CXC Loop and PABCD | The parent's merge authority, and a second goal where one already exists |

A project parent's goal is the default rather than something a request has to ask for, and the
decision that set this also carries activation authority for a parent already running. It reaches
no completed project and no unapproved backlog item: neither gets a goal. An issue child that
already has a goal reuses it, because a second goal opened for the same assignment is a duplicate
rather than a resume.

Recorded here, owned elsewhere, and unchanged by any of this: the initiative task runs the
user-assigned Astra, a project parent runs swe-2 at max effort, and an issue child runs Opus 5 at
xhigh. This is a readback, not an authority. The child default is owned by
[Default independent execution](../../crw-plan/references/integrations.md#default-independent-execution)
and the role model policy by its own issue; where either disagrees with this paragraph, they win
and this paragraph is stale. Nothing in this file changes a model, a permission, a worktree, a
project's scope or a child count.


## What is settled, and what is recorded with it

| Field | What it holds |
| --- | --- |
| `run_mode` | `loop`, `goal-free-run` or `blocked`, in the vocabulary [Parent goal lifecycle](../../crw-loop/references/parent-goal.md#record-the-start-adjudication) defines. For a project parent `loop` is the default; `goal-free-run` is the temporary state that exists only while activation is unresolved, or where an explicit user limit forbids a goal. |
| `child_cap` | The ceiling in force, and every bound that produced the number actually dispatched. |
| `host_compatibility` | The preflight outcome, the installed identities it was read at, and the issue that owns an unresolved blocker. |
| `observation_path` | The waiting mode selected under [OPS-8.1](operations.md#ops-81-parent-continuation-and-waiting), with the evidence that it is available. |
| `role` | Which of the three roles above this task actually is, read from its binding rather than its title. It decides which row of that table applies, and so what goal this task opens. |
| `approval_policy` | Whether the approval policy in force can carry the callback this run depends on, recorded apart from goal support and from the observation path. |
| `operating_scope` | What identifies the scope those host facts were read on: host, OS user and App Server under [OPS-3.1](operations.md#ops-31-the-operating-scope-is-the-sharing-unit). Recorded apart from what identifies this run. |

Every field carries three more things, because a field without them cannot be restored later:
nobody reading it can tell what it was allowed to survive.

- **source:** a host or tool restriction, an explicit limit in force for this request, the
  user's choice for this scope, a decision already recorded for this same project, or the
  standing default.
- **scope:** `host`, `project`, `this-run` or `standing`.
- **valid_while:** the conditions the value stands on, written so a later reader can check them.

## Where a decision reaches, and where it stops

A `host` fact describes the machine, its OS user, the installed components, the hook registration
and its observed rule, and the permission profile. It holds for any project on that operating
scope while the identities it was read at still match.

A `project` decision is that project's: its run mode, and a cap that differs from the standing one.
It holds across sessions of the same project and crosses to no other.

A `this-run` allowance was granted for one execution and expires with it. It is restored while that
run continues and is never promoted. Reading one in another project is reading a decision somebody
made about different work.

A `standing` default applies wherever nothing narrower is recorded. Only the user changes one, and
that change is recorded as a decision at that scope. An allowance granted once does not become one.

Because a project's coordination record is that project's, it is not where another project looks
for a host fact. It holds this run's copy and a pointer. The cross-project home of an unresolved
host blocker is the issue that owns it together with its accepted decision, which is also the
record that says whether the blocker has cleared. Another project reads that owning record.

## Re-read on every entry; re-decide only on a change

A new session, a resume after compaction and a later batch are all re-read events. Each re-reads
the identities in `valid_while` and the limits in force, and that read answers one question: does
anything the recorded adjudication stands on differ now?

Where nothing differs, the recorded values are restored and the run continues. Where something
differs, only the affected field is adjudicated again, under the same precedence, and the
superseded value is kept beside what changed.

A field is adjudicated again when an installed component's version, path or digest differs from the
recorded one; when the hook registration or its observed rule differs; when the permission profile
or the exposed tool set differs; when the operating scope differs; when the App Server has
restarted, because the delivery path and the approval policy are re-established with it; when the
user states a limit that did not apply before; or when the issue owning an unresolved blocker
records its resolution.
A new session, a compaction and a later batch satisfy none of those by themselves. A recorded
blocker whose owning record nobody has re-read is a blocker that still stands, not an unknown.

## Three compatibility facts, not one

Preflight at start, on recovery, and after an App Server restart records three separate results,
because one of them passing says nothing about the other two.

1. **Goal support.** Whether the host supports the goal this role opens, read from the exposed
   goal tools rather than assumed. A bridge that can read a goal cannot write one; a read-only
   goal API is not remote goal-write support.
2. **Delivery path.** Whether the active and idle paths can actually carry a callback to this
   task, under [OPS-8.1](operations.md#ops-81-parent-continuation-and-waiting).
3. **Approval-policy compatibility.** Whether the approval policy in force can carry that
   delivery. This has already failed in practice: a supervisor on `approvalPolicy: on-request`
   had its idle callback refused with `unsupported_approval_policy`, and automatic reporting and
   resume stopped.

Record the third as its own fact. It is not a reason to lower an approval policy, and lowering one
is not a repair for it. The bridge declares the policy it believes a thread is on and never sets
it, so a mismatch is something to observe and report rather than to write over.

Also record what the adjudication did not change. A start policy settles a run mode, a cap and the
compatibility facts; it does not alter worktrees, project scope, permissions or child count, and
saying so explicitly is what stops a later reader inferring a change nobody made.

## The parent goal activates; its Stop-continuation does not carry the run

Activation and durable continuation are different claims, and on the measured installation only
the first holds. Keep them apart in every report.

The goal activates: a project parent creates or reuses its native goal and reads it back active.
What follows is not durable automatic continuation. On CXC `0.2.33`, `handleStop` in the
`pabcd-state` component blocks the stop when a goal reads `active` while the phase is `IDLE` and no
orchestration is in flight, and the continuation it injects carries an unconditional directive to
enter PABCD, adding a loop-initialisation line when no goalplan slug is bound. A coordination
parent declines that directive, because this contract forbids it a goalplan or an FSM and forbids
closing a goal early, and it spends the continued turn on its coordination duties instead. The
honest exits the block itself names are completing the goal or recording it blocked.

That budget is finite. Three consecutive stop-blocks are allowed per phase against an absolute
ceiling of twenty-four, and the per-phase allowance recharges only on a phase transition, a
work-phase switch, or a measured improvement. A coordination parent produces none of those by
design, so its allowance never recharges. The result is a bounded number of wake-ups rather than a
durable loop, and it is reported as a bounded nudge and not as automatic continuation.

Because a compaction can lose this, the goal objective and the recovery record both state in plain
words that this is a coordination goal which never runs loop initialisation, never enters PABCD,
and never closes early. A later session reads that before it reads anything else.

This is a known incompatibility rather than a scheduled repair. CRW runs as an overlay on CXC as
installed, and nothing here changes CXC: overriding a hook by registration order, intercepting its
output, patching the plugin cache and writing private state are all excluded, and none of them is
offered as a workaround. The evidence sits in [CRW-29](https://linear.app/jun786/issue/CRW-29) and
[CRW-145](https://linear.app/jun786/issue/CRW-145), and CRW-145 is a documented upstream defect and
proposal held in the backlog, which is not an authorization to execute it, so no CXC-side fix is
promised here.

The supported operation meanwhile is the one above: keep the goal active, decline the steer, and
spend the continued turn on coordination. A durable resolution waits on whatever supported
interface the overlay evaluation establishes, the path under evaluation being supported per-task
or profile hook scoping or an extension interface in
[CRW-129](https://linear.app/jun786/issue/CRW-129), which is deferred.

## When the host cannot support the parent goal

Return the concrete path, the exact error and the impact to the owner of
[CRW-29](https://linear.app/jun786/issue/CRW-29). That is the report and the whole of it. Disabling
a hook, standing up a fake implementation FSM and calling around the guard are not repairs, and
they are not offered to the user as options either, because presenting one as a choice is how it
becomes an approved plan.

Progress already authorized to run goal-free may continue while the compatibility problem is being
fixed, reported as exactly that. It is never described as an activated goal loop, and a run
continuing that way evidences nothing about automatic continuation.

## Five facts that are not one fact

A goal that was accepted is not a goal that is active, and neither is a project that is done.
These are recorded separately and none of them stands in for another:

1. The goal creation was accepted.
2. The goal reads back active, with its objective and its approved scope.
3. A next turn actually continued, observed rather than assumed, and labelled a bounded nudge
   where that is what it was.
4. A child's result was delivered and acknowledged.
5. The project actually completed.

The parent activates through its own native tools and confirms the result itself. Another task
reading its goal establishes none of the five.

## Limits and existing goals survive this default

An explicit user limit on a task, a stop, read-only or no-goal, stays in force and the default
above does not touch it. It is a different thing from the temporary no-goal arrangement that the
role policy supersedes, and the two are recorded separately so that lifting the second never reads
as lifting the first.

An existing paused, blocked or differently scoped unfinished goal is preserved and moved only
through the supported transition its lifecycle defines, which
[Parent goal lifecycle](../../crw-loop/references/parent-goal.md) already sets out row by row. None
is deleted, marked complete or replaced to make room. No token budget is invented; one is set only
where the user supplied it.


## Ask before, never after

Every field is one of two things, and the run states which.

A field the precedence settles is applied and recorded without a question. Asking the user to
confirm a host restriction, a limit already in force, a choice already made for this scope, a
still-valid decision recorded for this same project, or the standing default is asking again for
an approval already given.

A field the precedence does not settle needs a new decision, and it is asked before the first
creation call. Only the action waiting on the answer is held: reading the baseline, preparing
packets, inspecting existing owners and read-only diagnosis continue while it is pending.

A question raised after the action it governs is a defect to record, not an approval obtained.
The reverse is no remedy either. Nothing here reduces what a run may dispatch, and holding a
question until the work is already done is not a way of avoiding it.

## What bounds the number actually dispatched

The children started in one pass are the lower of the ready independent issues the baseline found
and the cap in force minus this parent's children already live, then reduced by whatever the
parent observed in its operating scope. Record each bound and which one decided the result: a pass
limited by ready work and a pass limited by capacity look identical afterwards and recover
differently.

This parent's own live children always count against its cap. Other parents' children never do;
they inform the observation instead.

That observation is a reduction and only ever lowers the number. Where nothing was observed, no
reduction applies and the observation is recorded as `unmeasured`, which states what was read
rather than claiming the host is safe at the cap. Where an observation shows pressure, such as
another parent's live children, a resource reading, or a tool or permission constraint, the parent
lowers or holds dispatch and records the observation that caused it.

Three claims this does not support:

- The standing cap is a policy default, not a demonstration that a host runs that many children
  safely. [OPS-8.4](operations.md#ops-84-stating-the-scale-that-was-actually-verified) governs
  what may be said about scale.
- A raised file-descriptor limit is a changed ceiling, not evidence that a higher cap is safe. The
  exhaustion's cause and repair belong to [CRW-38](https://linear.app/jun786/issue/CRW-38).
- A parent counting its own slots is bookkeeping inside one parent, not an atomic host-global
  guarantee. Two parents each inside their own cap can jointly exceed what either observed, and
  the per-parent bound in
  [OPS-8.3](operations.md#ops-83-fairness-limits-and-error-isolation-proposed) is proposed rather
  than installed.

## Cases this policy is accepted against

These are the cases the policy is judged by. D is the number from the section above, and no case
creates anything before the adjudication: that count is zero in every row. Unless a row says
otherwise the project parent mode is `loop`, holding its own goal.

| # | Case | Question | Creations after | Effective mode and cap | On the next entry |
| --- | --- | --- | --- | --- | --- |
| 1 | New project, the default applies | none | D | `loop` with the parent own goal, standing cap | recorded before the first creation |
| 2 | The same conditions in a later session | none | D, the same as case 1 | identical, decided again by nobody | identities re-read, values unchanged |
| 3 | The same project resumed or compacted | none | D | the restored values | restored from the record, not re-decided |
| 4 | A different project on the same host | none for the cap | D | standing cap; case 5 does not reach here | host facts carry, project decisions do not |
| 5 | The user states a limit of four | none | D with the cap in force at four | source is the explicit limit, scope `this-run` | restored while the run lasts, never promoted |
| 6a | `no-create` in force | none | 0 | the mode as adjudicated, nothing created | the limit recorded as the precedence that applied |
| 6b | `no-goal` in force and the request separately covers goal-free Run | none | D | `goal-free-run` under the cap in force | the limit restored; it bars the goal, not the work |
| 7a | The goal is unsupported and no substitute is approved | none | 0 | `blocked`, owning issue cited | the blocker preserved, not re-asked as new |
| 7c | The goal is unsupported and goal-free progress was already authorized | none | D | `goal-free-run`, temporary, reported as that | path, error and impact returned to CRW-29 owner; never reported as an activated goal loop |
| 7d | The goal activates but its Stop-continuation is the bounded PABCD nudge | none | D | `loop`, the directive declined and the nudge recorded as bounded | goal-active and continuation kept as separate facts; a known incompatibility evidenced in CRW-29 and CRW-145, not a promised fix |
| 7b | A genuinely new decision is required | asked before any creation | 0 until answered | that action alone held | baseline, packets and read-only diagnosis continue |
| 8 | An installed version or hook rule changed | only if the re-read forces one | D | the changed field re-adjudicated, the rest restored | the changed identity recorded against the superseded value |
| 9 | Children of this parent are already live | none | D, which subtracts them | standing cap minus the live children | the existing owners preserved, never replaced |

An edit does not alter a turn that has already loaded these instructions. It does not stop there,
though: where an installation links this checkout, a later read of these instructions loads the
edited text, so a run spanning sessions can continue under instructions it did not start with.
A re-read event above re-reads the conditions a recorded decision stands on, which is not the same
act as loading this file again; neither one implies the other. The local edit, the pull request,
the integration and what is actually installed and running stay separate facts and are reported
separately.
