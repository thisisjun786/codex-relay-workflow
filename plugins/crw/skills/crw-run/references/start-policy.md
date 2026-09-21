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
| Project parent | None by default. It waits idle with no goal and a relay event brings it back. A goal exists only where the user explicitly asked for one through [crw-loop](../../crw-loop/SKILL.md) | None. It builds no CXC goalplan and no FSM | A fabricated source change to close a goal it does hold |
| Issue child | Creates or reuses its own for the issue scope | Keeps CXC Loop and PABCD | The parent's merge authority, and a second goal where one already exists |

A project parent runs goal-free by default, and that is a decision about what wakes it rather
than a restriction on what it may do. Waiting is the only thing a goal was carrying, and a
delivered relay event carries it better: the parent ends its turn when nothing but waiting
remains, and the event that matters is what starts the next one. An issue child that already has
a goal reuses it, because a second goal opened for the same assignment is a duplicate rather than
a resume.

This supersedes the 2026-09-20 role decision, under which a project parent created or reused its
own goal as its default and its continuation was the bounded Stop nudge recorded below. That
arrangement is kept here as history because a reader meeting an older parent still holding a goal
needs to recognise it as superseded rather than as a second valid answer. The decision that
replaced it is CRW-165's of 2026-09-21.

Recorded here, owned elsewhere, and unchanged by any of this: the initiative task runs the
user-assigned Astra, a project parent runs devin/swe-2 at max, and an issue child runs Opus 5
at xhigh. The parent pair has a history rather than one value, and this row carries the latest
recorded decision rather than an observation of what any task is running. swe-2 at max was the
recorded pair until 2026-09-21, when Jun moved every parent to xai/grok-4.6 at xhigh; Jun's
restoration decision later that same day puts swe-2 at max back and is the decision this row
records. Each superseded step is kept so the transitions can be reproduced and recognised as
superseded rather than as second valid answers.

A single task may be excepted by name. The consolidated CRW-127 coordinator ran
ollama-cloud/glm-5.3 at xhigh under Jun's 2026-09-21 exception, a bounded trial that has since
ended. A named exception does not become a default: it is not propagated to other parents or down
to children, and it is recorded explicitly rather than inferred from an observed pair. It is also
less exclusive than its name suggests: the policy file has no task-identity field and scopes an
exception by role and directory, so exclusivity comes from choosing a directory one task works in.
The id, that directory and the authorization behind them are read from the host policy and the
Linear record rather than from here.

This is a readback, not an authority. The child default is owned by
[Default independent execution](../../crw-plan/references/integrations.md#default-independent-execution)
and the role model policy by its own issue, while the value a run actually reads is the roles
section of the host's own execution policy; where any of them disagrees with this paragraph, they
win and this paragraph is stale. Nothing in this file changes a model, a permission, a worktree, a
project's scope or a child count.

## What is settled, and what is recorded with it

| Field | What it holds |
| --- | --- |
| `run_mode` | `goal-free-run`, `loop` or `blocked`, in the vocabulary [Parent goal lifecycle](../../crw-loop/references/parent-goal.md#record-the-start-adjudication) defines. It answers one question only: does this parent hold a native goal. For a project parent `goal-free-run` is the default and needs no separate authorization. `loop` applies where the user explicitly asked for a parent goal. A `no-goal` limit is already satisfied by the default and bars only `loop`. `blocked` is for a parent that can neither hold a goal nor proceed without one. |
| `child_cap` | The ceiling in force, and every bound that produced the number actually dispatched. |
| `host_compatibility` | The preflight outcome, the installed identities it was read at, and the issue that owns an unresolved blocker. |
| `observation_path` | `event-driven-idle`, `active-observation` or `blocked`, selected under [OPS-8.1](operations.md#ops-81-parent-continuation-and-waiting) with the evidence that it is available. It answers the other question: what brings this parent back. These are two fields because one value cannot carry both answers, and the pairing matrix below says which combinations are legal. |
| `role` | Which of the three roles above this task actually is, read from its binding rather than its title. It decides which row of that table applies, and so what goal this task opens. |
| `approval_policy` | Whether the approval policy in force can carry the callback this run depends on, recorded apart from goal support and from the observation path. |
| `operating_scope` | What identifies the scope those host facts were read on: host, OS user and App Server under [OPS-3.1](operations.md#ops-31-the-operating-scope-is-the-sharing-unit). Recorded apart from what identifies this run. |

`run_mode` and `observation_path` are closed vocabularies, and their values are recorded verbatim
as one of the literals declared above. A paraphrase is not a restorable record: these two are the
keys the pairing matrix is checked against and the keys a later session restores from, so a value
that reads correctly but is spelled differently is a record the next reader cannot match, and the
restore it was written for silently becomes a fresh adjudication.

Stating that has not been enough. Four readers of this file on 2026-09-21 wrote `relay_only`,
`goal_free`, `run_only` and `relay_only` again for `run_mode`, and `relay` for
`observation_path` every time. Two of them had been given the requirement, one as prose and one as
a copyable list, and paraphrased anyway.

A consumer stays strict about it. A value outside the declared set is not a value to interpret: it
is an unreadable field, and an unreadable field is re-adjudicated under the trigger list below
exactly as a missing one is. Do not normalise an approximation into a literal, even an obvious
looking one — `relay_only` names an observation path while sitting in `run_mode`, so the obvious
reading and the field it was written in disagree, and two readers guessing independently would
restore two different policies from one record. Re-adjudicating is deterministic and leaves a
record that says so; guessing is neither.

That is the correct rule and it is also not obeyed. A reader given a record containing
`run_mode: relay_only` and asked what to do with it answered that the values were settled and
would be restored and used as they stood. So the consumer side fails the same way the producer
side does, and for the same reason: the field is reconstructed from what it appears to mean rather
than checked against what is declared.

Three interventions were measured while this was failing — asking for the literals in prose,
offering them as a list to copy, and requiring a consumer to reject anything outside the set — and
none of them changed a reader's behaviour at the time. The rule above stays because it is what a
correct implementation does, and because the alternative of tolerant matching is worse: with no
alias table two readers restore two different policies from one record.

So the enumeration is also runnable. [`scripts/start_policy.py`](../scripts/start_policy.py)
parses the pairing table in this file and answers from it:
`vocabulary` prints the legal pairings as lines to copy into a record, and `check` reads a
record and exits non-zero on a value outside the set or an illegal pairing. A producer writes the
two lines by copying command output instead of recalling a literal, and a consumer that doubts a
record runs the same check rather than deciding by eye what an unfamiliar value must have meant.
Because the script reads this table rather than keeping its own copy, renaming a literal here
cannot leave the two disagreeing, and `scripts/ci/contracts.py` runs its selftest, which carries
the four recorded paraphrases as cases that must be rejected.

The failure then stopped reproducing, and not because of that script. Four readers of this file as
it now stands wrote `goal-free-run` with either `event-driven-idle` or `active-observation` —
in the set every time, and the one given no readiness evidence correctly took the unproven path.
Two of the four read deliberately damaged copies: one with every previously wrong value deleted
from the page, one with the paragraph above removed. Neither ablation brought the failure back,
and none of the four ran the check. So the earlier paraphrases are real and this text does not
provoke them, but nothing here identifies what changed between the two revisions, and eight
readers of one model at one effort establish a rate in neither direction. Run the check to settle
a record you doubt; do not read it as the reason records are now well formed.

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

One trigger is not a change in the world at all. A closed-vocabulary field whose recorded value is
missing, or outside the set declared above, is adjudicated again on every re-read, because a value
the reader cannot match was never a restorable record in the first place. That check runs even
where every identity in `valid_while` still holds and nothing else differs, and it is what stops
two consumers of one record — one re-adjudicating an unreadable value, one finding no listed
change and restoring it as it stands — resuming the same parent under two different policies.

## Three compatibility facts, not one

Preflight at start, on recovery, and after an App Server restart records three separate results,
because one of them passing says nothing about the other two.

1. **Goal support.** Whether the host supports the goal this role opens, read from the exposed
   goal tools rather than assumed. A bridge that can read a goal cannot write one; a read-only
   goal API is not remote goal-write support. Under the project parent default no goal is opened,
   so this fact is recorded `not_applicable` for that role and becomes load-bearing only where an
   explicit Loop or an issue child asks the host for a goal.
2. **Delivery path.** Whether the active and idle paths can actually carry a callback to this
   task, under [OPS-8.1](operations.md#ops-81-parent-continuation-and-waiting).
3. **Approval-policy declaration.** Whether the policy the caller declares matches the one the
   task is actually on. This has already failed in practice: a supervisor on
   `approvalPolicy: on-request` had its idle callback refused with
   `unsupported_approval_policy`, and automatic reporting and resume stopped. Read that refusal
   against the transport that produced it, because the two answer differently and the record
   says which one applies. The thread bridge declares this value and never transmits it,
   defaulting an omitted declaration to `never`, so there the refusal is a declaration mismatch
   and naming the policy the task is actually on is what reaches it. The session relay instead
   transmits the recorded policy and admits only `never`, treating a returned `on-request` as a
   push channel that is closed rather than one that is mismatched, so declaring correctly does
   not open it: that recipient is `inbox_only`, which is a terminal delivery state no retry
   revisits.

Each result lands in its own recorded field: goal support in `host_compatibility`, the delivery
path in `observation_path`, and approval-policy compatibility in `approval_policy`. Three results,
three fields, so a later reader can tell which one failed.

Record the third as its own fact, with the transport beside it, because a caller that reads a
relay refusal as a bridge one will wait for a delivery that is not coming. Neither answer is a
reason to lower the policy: on the bridge path declaring correctly is the repair, on the relay
path there is none to make, and lowering one gives away a protection the task was running with
while repairing nothing. A mismatch is observed, declared correctly where that helps, and
otherwise reported as a closed channel rather than written over.

Also record what the adjudication did not change. A start policy settles a run mode, a cap and the
compatibility facts; it does not alter worktrees, project scope, permissions or child count, and
saying so explicitly is what stops a later reader inferring a change nobody made.

## Which pairings are legal

Splitting the goal question from the return question makes some combinations incoherent, so the
product is declared rather than left to judgement.

| | `event-driven-idle` | `active-observation` | `blocked` |
| --- | --- | --- | --- |
| `goal-free-run` | the default | legal: readiness is unproven | legal, transitional |
| `loop` | legal as a compatibility mode, at the cost recorded below | legal | legal, transitional |
| `blocked` | illegal | illegal | the only legal pairing |

`blocked` as a run mode means activation and execution are both unavailable and no child is
created, so pairing it with a return path a running parent would use describes a parent that does
not exist. Either operational path beside it is a record to repair, not a state to act on.

`goal-free-run` or `loop` beside `blocked` is different and is legal: the parent is running and
has no usable wait path. It records the concrete blocker and keeps whatever authorized work does
not depend on the delivery moving.

The `loop` and `event-driven-idle` cell is legal and is not the same path the default gets. A
parent holding an active goal does not reach idle cleanly, because the Stop behaviour recorded
below blocks the stop first. It reaches idle only after that bounded budget releases, and each
block costs it a turn. That difference is the cost the transition was made to remove, and it is
what a usage comparison between the two modes is actually measuring.

## Before a parent may wait idle

`event-driven-idle` is selected on evidence, never on intent. Five facts, and the fifth is the
one that is usually skipped.

1. The assignment is registered in the shared store and routes to this parent under OPS-7.1
   and OPS-7.3.
2. The delivery service for this operating scope is running **and its ticks are progressing**. A
   PID and an `enabled` service are not tick progress, and a manual tick is not unattended
   delivery. Ticks progressing is also not the same as sends being permitted: read the store's
   current failure records for this scope, and treat any applicable service-side pre-send refusal
   as a readiness failure even while the loop runs normally. Their absence does not satisfy this
   on its own — a store with nothing queued has no failures to show — so positive evidence comes
   from a delivery that actually went out on this path, or from checking the identity of the
   configuration the service is deciding under.
3. This parent is reachable as a recipient. That covers the approval policy in force, and it also
   covers this parent's own goal status, which
   [OPS-8.2](operations.md#ops-82-busy-paused-cancelled-and-archived-parents) records as an input
   to deliverability: a parent whose goal is paused has revoked its own readiness and must not be
   left waiting on a delivery that will never arrive.
4. Every disposition this parent is waiting on can actually enqueue a delivery — not completion
   alone, but also a child stopping for a person and a child asking for a decision. A disposition
   that cannot is not covered by a completion-only observation, and the parent either keeps
   `active-observation` for that one or requires the child to emit under it.
5. An unattended idle-to-turn wake has already been observed on this same operating scope, App
   Server instance, store and socket, service instance, approval policy, role-policy identity,
   transport revision and disposition class — by this parent on an earlier run, or by a separate
   probe recipient that proved it first. The role policy belongs in that list because the service
   decides authorization under whichever one its own process can read, so a wake observed under a
   different one is evidence about a different configuration.

Fact 5 is written that way because the obvious version is circular: a parent cannot observe its
own first wake without first yielding idle on the strength of the observation it has not made. A
probe recipient breaks that loop, since a throwaway recipient can establish the scope's capability
without the coordinating parent having to gamble its assignment on it.

Where fact 5 has no evidence it is recorded `unmeasured` and the path is `active-observation`.
That is a decision not to use event-idle, not a fifth fact that passed, and the two must not be
written the same way.

## An explicit Loop activates its goal; its Stop-continuation does not carry the run

This section governs `run_mode: loop` only. Under the default no goal exists, so none of it
applies. Activation and durable continuation are different claims, and on the measured
installation only the first holds. Keep them apart in every report.

The goal activates: a project parent creates or reuses its native goal and reads it back active.
What follows is not durable automatic continuation. Measured on CXC `0.2.33` and re-read on
`0.2.34`, `handleStop` in the `pabcd-state` component blocks the stop when a goal reads `active` while the phase is `IDLE` and no
orchestration is in flight, and the continuation it injects carries an unconditional directive to
enter PABCD, adding a loop-initialisation line when no goalplan slug is bound. A project
parent declines that directive, because this contract forbids it a goalplan or an FSM and forbids
closing a goal before its scope is actually delivered, and it spends the continued turn on its
coordination duties instead. The honest exits the block itself names are completing the goal, which
is honest only once the agreed scope is verified, or recording it blocked.

That budget is finite, though not in the way a first reading suggests. Three consecutive blocks are
allowed, and the next one releases instead, so the turn can end. The release also clears the
per-phase counter, and a cleared counter no longer matches the phase it is compared against, so the
stop after it reads as progress and can open another burst of three. What never resets is the
absolute total: every stop advances it, block and release alike, and twenty-four is the ceiling. A
project parent therefore gets bursts of at most three wake-ups separated by releases, bounded
overall by that total. That is a finite nudge budget, reported as such, and not durable automatic
continuation.

Because a compaction can lose this, the goal objective and the recovery record both state in plain
words that this is a coordination goal, meaning a project parent goal that carries no
implementation loop, and that it never runs loop initialisation, never enters PABCD,
and never closes early. A later session reads that before it reads anything else.

Measured once, and narrowly. On 2026-09-21 a deliberately trivial test actor was created holding an
active goal with nothing to do. Within its first turn its goal moved to `blocked` with no further
external prompt, at 2127 tokens used, and it then held flat at one turn for 270 seconds. Two
goal-free actors in the same harness held flat for 307 seconds with their turn count, newest turn
id and the host's `updatedAt` all unmoved. So the blocked exit this section names was reached
without anyone choosing it.

What that does not establish. The Stop behaviour's firing was not read, so nothing here attributes
the transition to it; only the outcome was observed. Those 2127 tokens cover that actor's whole
first turn, including creating the goal and answering its prompt, rather than an isolated idling or
continuation cost. And one trivial actor is one trivial actor: how often this happens, whether it
must, what a real project parent carrying scope would do, and any saving between the two modes are
all `unmeasured`.

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
above does not touch it. Since 2026-09-21 a `no-goal` limit and the project parent default agree
rather than conflict: the default already opens no goal, so the limit bars only an explicit Loop
and subtracts nothing else. Record it anyway, because a limit the user stated and a default that
happened to match are different facts, and lifting one must never read as lifting the other.

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
child-creation call. Only the action waiting on the answer is held: reading the baseline, preparing
packets, inspecting existing owners and read-only diagnosis continue while it is pending.

A question raised after the action it governs is a defect to record, not an approval obtained.
The reverse is no remedy either. This timing rule does not itself reduce what a run may dispatch,
and holding a question until the work is already done is not a way of avoiding it. The cap, an
explicit limit and the subtractions below are what bound the number.

## What bounds the number actually dispatched

The children started in one pass are the lower of the ready independent issues the baseline found
and the cap in force minus this parent's children already live, then reduced by whatever the
parent observed in its operating scope. Record each bound and which one decided the result: a pass
limited by ready work and a pass limited by capacity look identical afterwards and recover
differently.

This parent's own live children always count against its cap. Other parents' children never do;
they inform the observation instead.

A creation whose outcome is unresolved counts against the cap exactly as a live child does, until
it is reconciled. An unresolved outcome means a writer may exist, so treating its slot as free is
how a parent quietly exceeds its own ceiling while its arithmetic still looks correct.

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
creates a child before the adjudication: that count is zero in every row. Creation here means child
creation; `no-create` bars that. Unless a row says otherwise the project parent mode is
`goal-free-run`, holding no goal, and its observation path is whichever of `event-driven-idle` or
`active-observation` the readiness facts above actually support.

| # | Case | Question | Creations after | Effective mode and cap | On the next entry |
| --- | --- | --- | --- | --- | --- |
| 1 | New project, the default applies | none | D | `goal-free-run` with no parent goal, standing cap | recorded before the first creation, with the observation path and the readiness evidence beside it |
| 2 | A second session adjudicating the same inputs from scratch | none | D, identical to case 1 | `goal-free-run`, standing cap, reached independently | the adjudication is deterministic: same inputs, same record, no question |
| 3 | The same project resumed mid-run or after a compaction | none | D | the restored values, including any `this-run` allowance | restored from the record rather than adjudicated again |
| 4 | A different project on the same host | none for the cap | D | standing cap; case 5 does not reach here | host facts carry, project decisions do not |
| 5 | The user states a limit of four | none | D with the cap in force at four | source is the explicit limit, scope `this-run` | restored while the run lasts, never promoted |
| 6a | `no-create` in force | none | 0 | the mode as adjudicated, no child created | the limit recorded as the precedence that applied |
| 6b | `no-goal` in force | none | D | `goal-free-run` under the cap in force, which is the default anyway | the limit restored; it bars `loop` and changes nothing else, because the default already holds no goal |
| 7a | An explicit Loop was requested, its goal is unsupported, and the user declines goal-free Run in its place | none | 0 | `blocked`, owning issue cited | the blocker preserved, not re-asked as new |
| 7b | A genuinely new decision is required | asked before any creation | 0 until answered | that action alone held | baseline, packets and read-only diagnosis continue |
| 7c | An explicit Loop was requested, its goal is unsupported, and the user has not declined goal-free Run in its place | none | D | `goal-free-run`, reported as the default rather than as a degraded Loop | path, error and impact returned to CRW-29 owner; never reported as an activated goal loop |
| 7d | An explicit Loop activates and its Stop-continuation is the bounded PABCD nudge | none | D | `loop`, the directive declined and the nudge recorded as bounded | goal-active and continuation kept as separate facts; a known incompatibility evidenced in CRW-29 and CRW-145, not a promised fix, and not a reason to open a goal the default does not need |
| 7e | Readiness fact 5 has no evidence on this operating scope | none | D | `goal-free-run` with `active-observation`, fact 5 recorded `unmeasured` | the parent keeps bounded waits and claims no automatic resume until a wake is observed here |
| 8 | An installed version or hook rule changed | only if the re-read forces one | D | the changed field re-adjudicated, the rest restored | the changed identity recorded against the superseded value |
| 9 | Children of this parent are already live | none | D, which subtracts them | `goal-free-run`; the standing cap is unchanged and D subtracts the live children | the existing owners preserved, never replaced |
| 10 | A parent from before this decision is still holding its own active goal | none | D | `loop`, recorded as carried over rather than chosen | the goal is preserved and retired only at its scope boundary under [Parent goal lifecycle](../../crw-loop/references/parent-goal.md); it is never paused to reach the default, because a paused goal is undeliverable |

An edit does not alter a turn that has already loaded these instructions. It does not stop there,
though: where an installation links this checkout, a later read of these instructions loads the
edited text, so a run spanning sessions can continue under instructions it did not start with.
A re-read event above re-reads the conditions a recorded decision stands on, which is not the same
act as loading this file again; neither one implies the other. The local edit, the pull request,
the integration and what is actually installed and running stay separate facts and are reported
separately.
