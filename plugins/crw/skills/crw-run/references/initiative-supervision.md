# Initiative supervision

Run executes the scope its task is bound to. Where that scope is an initiative, this file is the
entry: how a task becomes that initiative's supervisor, which projects it is executing, when it is
finished, what each parent receives, what comes back, and how it recovers. The roles themselves are
the shared [supervisor, parent and child scope](../../crw-plan/references/integrations.md#supervisor-parent-and-child-scope),
and the project-level procedures [crw-run](../SKILL.md) already owns are not repeated here one
level up.

Nothing here is a transport. It establishes no automatic resume, no supervisor host goal, no
registration a runtime performs and no wake-up.
[OPS-7.4](operations.md#ops-74-three-levels-and-their-routing-identity) records that the bundled
relay holds no supervisor relationship, so every sentence below about a recorded relationship is
conditional on an installation that has one, and an instruction a reader follows is not a store
that enforces it.

## Four requests name an initiative, and one starts a supervision

An initiative turns up in requests that want quite different things, and the difference is in the
request rather than in the link.

| Request | Owner | Effect on binding |
|---|---|---|
| Execute this initiative's agreed projects, naming the initiative | this entry, through [crw-run](../SKILL.md) | binds a supervisor for that initiative: this task where it is free to take it, and otherwise the initiative's existing supervisor or one created for it |
| Define or plan it | [crw-define](../../crw-define/SKILL.md), [crw-plan](../../crw-plan/SKILL.md) | none; planning creates no supervisor and moves no ownership |
| How is it going | a status read | none; it reads existing records and wakes nothing |
| An initiative link carried as context by other work | the operation already running | none; it locates context |

A designation is explicit and names the initiative. A link by itself rebinds no existing parent,
and a title, a folder, a branch or a chat link makes no role at all: identity stays on the stable
IDs under [OPS-7.1](operations.md#ops-71-what-an-assignment-binds). Explicit read-only,
status-only, plan-only, no-create, no-goal and narrower delivery limits survive this routing
exactly as they survive it in Run, and they travel down without widening
([OPS-7.3](operations.md#ops-73-isolation-between-parents)).

Those limits bind every outward step this entry takes, not just the run as a whole. Creating a
task to hold the supervision, delivering a designation to an existing supervisor, and handing a
project to its parent are each contact, each gated on the limits in force at that moment, and each
returned prepared and unsent, named as unsent, where a limit forbids it. The gate is per action and
per project, so a limit that stops one of them does not hold back the rest.

A task's own state is the other thing read before contact. Running and idle are not the only
answers: a paused, cancelled or archived task is a third, and resuming one is a decision its owner
makes rather than something a handoff may do
([OPS-8.2](operations.md#ops-82-busy-paused-cancelled-and-archived-parents)). Its brief waits and
is reported as waiting, with what would release it, because the ordinary message path would start
a turn in work somebody deliberately stopped. The supervision record keeps that brief as pending
against its project, so a parent its owner resumes later has something to find rather than nothing.

## What the binding fixes

Bind three things together, because a supervision missing any of them has no boundary:

- the stable initiative ID and URL, with the revision of the body the finish condition was read from;
- the approved project set, each project by stable ID; and
- the completion boundary, which is that finish condition as the initiative states it, together
  with what the designation excludes.

Membership comes from the designation itself. Where it enumerates projects, that list is the set.
Where it names only the initiative, the set is the initiative's contributing projects at the
revision read, written out project by project in the record, so a later reader compares against a
list that was fixed rather than against a relation that has moved since.

The approved set is the set agreed at designation. A project created or linked to the initiative
afterwards is outside it, and admitting one is a scope change recorded with its source and date,
for the same reason a project link does not approve future backlog additions one level down.
Executing an initiative is therefore never a standing claim on whatever the initiative later
accumulates, and it reaches no work outside it.

### Projects supervised elsewhere

Where a project in the set already answers to a different initiative's execution supervisor, it
keeps that one: this initiative references its outcome instead of issuing it work, and the
reference is recorded as a reference so nobody later reads it as an instruction.

### Which task may hold the binding

The supervisor role carries no project's issues and no issue's implementation: it holds no checkout
and it merges nothing ([OPS-9.3](operations.md#ops-93-the-parent-merges-and-does-not-release)).
That describes the role rather than converting whatever task the designation arrives in, so read
this task's own current binding before binding anything.

A task that already supervises this same initiative is the one that continues, and that is reuse
rather than a new binding. A task bound to a different scope does not become the supervisor by
being handed a designation, and that holds for any such binding rather than a project's alone: a
parent would leave its project without one and its children without an owner, and an issue-bound
task would abandon or blur the implementation it owns. Each task is bound to one Linear level, so
what decides this is whether the task is free, not which level happens to hold it. A user who does
want that task to stop being a parent is making an ownership change, recorded and completed first
rather than produced as a side effect of binding.

The supervision then belongs in a task of its own, or in the existing supervisor where there is
one, and the designation is routed there: to an existing supervisor by the delivery in the reuse
order below, and otherwise to a task created for it the way this workflow creates any independent
task, under [Independent implementation tasks](../SKILL.md#independent-implementation-tasks) for
creation authority and capability, carrying the designation itself as its first prompt. Creating
that task is itself contact and is gated like every other outward step above. Where creation is not
authorized, a limit forbids it, or no creation path is available, say exactly that and bind
nothing: a designation reported unbound is better than a parent quietly repurposed.

### What a recorded relationship settles, and what it does not

Where an installation records these relationships, one live scope per task per role is the rule it
enforces, so a second same-role binding is refused rather than silently replacing the first. Where
nothing records them, and the bundled relay records none, the reuse below is a read and not a lock:
two designations issued at once can both find no supervisor, and the initiative's own record is
what reconciles that afterwards rather than what prevents it.

### Where the record lives

The record is the initiative's own, written by the supervisor under the
[initiative body standard](../../crw-plan/references/integrations.md#initiative-body-standard) and
its comments and updates. The supervision record that makes recovery possible is in
[Coordination record](task-packet.md#coordination-record).

## Reuse before creating anything

Read in this order and stop at the first level that already exists, because every later step
assumes the earlier one was checked:

1. **The supervisor.** If this initiative already has one, that task is the supervisor and this
   designation belongs to it rather than to a second binding. Where this task is that supervisor,
   continue here. Where it is another task, read its record and status first. Where the limits in
   force permit contacting it, deliver the designation the way a parent is addressed: the ordinary
   message path when it is idle, its verified active turn when one is running, each carrying the
   [restoration block](task-packet.md#restoration-block). The task receiving it corroborates it
   the way a parent corroborates a handoff, against its own record and the initiative's read
   directly: a forwarded designation carries the request and not the authority, and one that would
   change the approved set or the completion boundary is a scope change its owner decides rather
   than something a message settles. Where those limits forbid contact,
   return the existing supervisor and the designation that stays unsent. Where no supported route
   reaches it at all, report the exact resume action its owner has to take and bind nothing,
   because a status read is not the execution that was asked for.
2. **Each project's parent.** A project in the set with a parent keeps it. Read that parent's
   coordination record for its issue scope, delivery limits and current state.
3. **Each parent's children.** A live child is the owner of its issue. It is not reassigned, not
   duplicated and not addressed by this task.
4. **The pull requests already open.** Each issue's one current delivery pull request is in its
   parent's record; read it there rather than asking for a new one or opening a second.

Repeating the designation, and resuming after an interruption, run this same order and converge on
the same tasks wherever this order can observe them. Reuse is the first move and creation only what
remains after it. A busy parent, an unreachable record or an uncertain read is not evidence that a
level is missing; it is a level that has not been read yet.

## Reconcile concurrent supervision claims

Where nothing records these relationships that convergence is a read rather than a guarantee, so a
task that had to create the supervision reads the initiative record once more before its first
handoff. If another supervisor recorded itself for the same initiative, the earlier recorded
binding stands, and the later one stops there: it sends nothing, preserves its own record and hands
over the briefs it has not sent.

That reread narrows the window without closing it. Two tasks can still interleave a write, a read
and a first handoff so that each sees only itself, because none of this is an atomic claim: an
atomic one needs a store that records the relationship and refuses the second, which the bundled
relay does not have and which belongs to the work that owns registration. So the parent is the
second place a split is caught rather than the supervisor being the only one. A parent already
holding an accepted handoff for this project from a different supervisor treats a second one as a
conflict to raise, not as a newer instruction to follow, and neither supervisor settles that by
sending again. Caught there, a duplicate surfaces at the point where it would do damage instead of
being inferred later from divergent work.

Two supervisors handing off to two different projects is the case that check does not see, since
neither parent receives a second handoff. Two things make it findable instead. A supervisor
recording itself preserves any entry already there rather than replacing it, so the earliest
binding stays readable and is what stands, rather than ownership being decided by whoever wrote
last. And the initiative record is read again before each handoff and at each returned result, not
only before the first, so a second supervisor is found within a step or two rather than at the end.
On finding one, the later binding stops there: it sends no further handoffs, tells the parents it
already handed projects to that its handoff is withdrawn and names the owner, and hands its record
over. A withdrawal, or a replacement handoff from the other side, is what prompts the receiving
parent to read; neither is what settles it, which is also why forging either achieves nothing.

A parent settles that change the way it settled the first handoff, from the record rather than from
the message. Finding the initiative record now naming a supervisor other than the one its own
record holds, it reads that owner's scope before it writes anything, because two designations
need not describe the same execution and the new owner's approved set may not contain this project
at all. Where it does contain it, the parent compares the rest of what that record says about this
project, its contribution, the completion boundary, the limits, the prerequisites and any
shared-target order, against what its work was actually started under, reading the contribution
from whichever record owns it exactly as an arriving handoff is checked rather than assuming the
initiative carries it. It takes the owner, updates its own record and carries on untouched only
where those agree, since the supervision changed and the project's work did not.

Where the winning set excludes this project, the parent records no supervisor it is not in fact
supervised by: it preserves the work already done, stops it and raises both, because a recorded
ownership nobody authorized would route every later message to the wrong owner. Where the project
is included but any of those values differs, the work is being delivered against criteria nobody
now holds, so the parent takes the owner, records the difference, holds the affected work and
raises it rather than finishing against a designation that lost.

A withdrawal also cannot always be delivered, since a parent that is paused, cancelled or archived
does not receive one, so the transfer cannot rest on the message arriving. A parent validates its
recorded supervisor against the initiative record when it resumes, before it continues or
integrates anything, and applies the same rule there. It reads that record whether or not its own
names a supervisor, because a project whose first handoff was held while it was paused has none
recorded and would otherwise resume on a scope that has since been superseded. The losing
supervisor keeps the undelivered transfer on its own record as owed, so whoever resumes that parent
can find it rather than inferring it. That does not prevent an overlap; it bounds one to the work
already started, which can be reconciled, instead of letting an initiative run to completion under
two owners.

## Hand a project to its parent

What travels down is one project's brief and nothing below it: the project's criteria and the
revision they were read at, its prerequisites including the peers it shares a surface with, the
authority and limits in force, and the current state as locators rather than contents. The shape is
[Project handoff](task-packet.md#project-handoff), written in English like every instruction
between tasks.

Two carriers, chosen by whether the parent exists. A project with no parent gets the handoff as the
first prompt of the new task, prefixed with the project designation so that task runs its own
[Project parent binding](../../crw-plan/references/integrations.md#project-parent-binding). An
existing parent gets it as a [Coordination message](task-packet.md#coordination-message) of kind
project handoff, carrying the [restoration block](task-packet.md#restoration-block) whether that
parent is running or idle, for the reason that section gives.

Both carriers contact another task, so both fall under the gate above: where the designation
forbids contact, or forbids the creation a project without a parent would need, that brief is
prepared and returned unsent rather than delivered quietly.

A handoff sent is not a parent bound, and one accepted is not a project delivered.
[Project handoff](task-packet.md#project-handoff) names those facts and says what each does and
does not establish; they are not restated here.

From there the project is the parent's. It decides the issues, their dependencies and order, which
children exist, and how each delivery is verified and integrated. The supervisor does not repeat
that investigation, write those issue bodies or review those diffs, because running the same
enquiry once per level is how one project's cost becomes three. Nor does it instruct a child: a
change it wants in a child's work is said to that child's parent.

Preparation is per project and never a queue. A project whose brief is ready is handed over now,
while another is still being prepared, and a project whose prerequisites are met does not wait on
an unrelated one. Holding a ready project needs a concrete reason recorded against it: a
prerequisite not yet verified, a shared resource whose order is still being decided, or a limit the
designation imposes.

## What travels back up

A parent returns four things and no more: the project's result, the evidence per criterion, the
problems it could not resolve, and the decisions it needs. Everything else stays where it already
is. The child's per-finding trail stays on its pull request, the parent's issue records stay in the
project, and the raw receipts stay in their private evidence locations; the report carries the
pointer to each.

Real delivery facts are read through the relationships that already hold them rather than recopied
upward. Which pull request delivers an issue, which revision a verdict was recorded against and
whether a merge actually landed are already recorded by the parent and, where an installation holds
the assignment, by that record; the supervisor reads them there and treats
[Implementation Done](../../crw-plan/references/integrations.md#implementation-done) as the test for
a landing. A green check, a parent's acceptance and a completed turn are not that evidence.

The supervisor decides what a pair of parents cannot settle alone: a disagreement, a change that
widens either project's scope, who owns newly discovered work, and shared resources including the
order in which projects reach a shared target. It verifies each reported project outcome against
the initiative's finish condition and stops there. Child delivery, parent acceptance, the merge,
installation, observed behaviour, project completion and initiative completion stay separate
claims, so a few projects marked done do not finish the initiative.

## Recover the supervision

After a compaction, a turn ending or a restart, read the supervision record first, then refresh
only what can have moved: each parent's status and its last returned result, the state of the
requests still outstanding, and the current head and checks of the pull requests already named.
The investigation, the plan and the issue bodies are not regenerated; they are in Linear and in
each parent's record, and rewriting them from this task's view replaces those records with a guess.

A child that is alive keeps its issue. Uncertain delivery, an unreadable record or an elapsed wait
is not proof that a writer is gone, and recovery that cannot read a level reports that level as
unknown rather than treating it as empty. Reconcile before creating, never after.

### Which register each state word came from

The reading itself is the section above. What it returns is not one vocabulary: the words that
describe a parent — active, idle, notLoaded, systemError, blocked, paused, cancelled, archived,
completed — come from five registers, and a recovery that acts on the wrong one acts on a fact
nobody reported.

| Register | Values | Where it is read |
|---|---|---|
| The task, right now | `active`, `idle`, `notLoaded`, `systemError` | the host, per [the bridge](bridge.md#reach-a-task-that-is-already-working) |
| That task's native goal | none, complete, active, paused, blocked, a different unfinished goal, unreadable | its goal tool, under [Parent goal lifecycle](../../crw-loop/references/parent-goal.md) |
| The task's lifecycle, set by a person | paused, cancelled, archived | [OPS-8.2](operations.md#ops-82-busy-paused-cancelled-and-archived-parents) |
| The mode this run recorded when it started | `loop`, `goal-free-run`, `blocked` | the start-policy record, under [Record the start adjudication](../../crw-loop/references/parent-goal.md#record-the-start-adjudication) |
| The project and the record | completed, and a dependency recorded as blocking | Linear and the supervision record |

Label every reading with its register before anything acts on it. The fourth register is the one a
restart is most likely to skip, and it is what makes the second readable: a project parent's goal
is the role default under [Start policy](start-policy.md), so an absent goal is not a resting state
on its own. Read it against the recorded mode — `goal-free-run` accounts for it, `loop` makes it a
finding, and `blocked` means no child should exist yet. Where a record exists, restore it and
re-adjudicate only the fields whose conditions have changed. Where none is found, that is unknown
rather than proof none was written: a record can also be partial or damaged, and adjudicating from
scratch discards whatever the missing entry alone carried, an authorized `goal-free-run` among
them. Report it as unknown, keep what the rest of the record holds, and settle the adjudication
before dispatch instead of dispatching on a new guess.

"Blocked" is the word that most needs its register named, because a blocked goal, a recorded
`blocked` start mode and a dependency the record carries have different owners and different next
actions. "Paused" divides the same way between a goal and a task lifecycle, and "completed"
between a goal and a project; OPS-8.2 keeps a paused goal and a stopped turn separate for the same
reason.

What follows from each reading is not decided here, and it is not decided by a status operation
either. Recovery's own actions stay with
[OPS-8.2](operations.md#ops-82-busy-paused-cancelled-and-archived-parents) for the busy, paused,
cancelled and archived routing, with [Start policy](start-policy.md) for the mode, and with C9
below for a finished project.
[The midpoint check](../../crw-status/references/midpoint-check.md) is read for the shape of a
status reading, its idle rows and its rule that a goal existing, a goal activating and
continuation observed are three separate readings, and not for the goal default, which
start-policy.md fixes and which that file predates. This entry adds only the register each word
came from, which is the thing a restarted supervisor no longer remembers.

One boundary survives the restart with everything else. The per-subject corroboration in
[Restate the run before continuing it](../SKILL.md#restate-the-run-before-continuing-it) is a
parent's work on its own project, so at this level it is read rather than repeated: the
restatement names each project's parent and whether that parent reports the project complete, and
tests that reported outcome against the initiative's finish condition. A supervisor that reopens
every issue and pull request behind a parent's report has taken over the level below, which S28
already forbids.

## Cases

These are the situations this entry has to get right. Three of them are already worked one level
down in the [operations scenarios](operations/scenarios.md) as S29, S30 and S31, and are cited
rather than rewritten.

### C1 Several projects, ready at different times

Observed: the approved set holds four projects. Two have verified prerequisites, one waits on a
contract another project is still delivering, and one has no parent yet.
Clauses: [OPS-7.1](operations.md#ops-71-what-an-assignment-binds),
[OPS-7.3](operations.md#ops-73-isolation-between-parents).
Action: hand the two ready projects to their parents now, and create the missing parent for the
third as its brief is finished, rather than preparing all four and starting together. Record
against the waiting project the exact prerequisite and where its verification will appear.
Preserved: no project is delayed by another project's preparation, and the waiting one is held by a
recorded dependency rather than by a queue.

### C2 A project that contributes to several initiatives

Observed: a project in the approved set is also linked to another initiative, which has its own
execution supervisor.
Clauses: [OPS-7.3](operations.md#ops-73-isolation-between-parents),
[OPS-7.4](operations.md#ops-74-three-levels-and-their-routing-identity); worked as S29.
Action: leave its execution supervisor and its parent as they are, reference its outcome, and
record the reference as a reference. Do not instruct that parent, clone its children or count its
delivery twice. A requirement for that project goes to its own supervisor, which decides it.
Preserved: one execution owner per scope, and no second supervisor reaching into a bound parent.

### C3 The existing parent is busy

Observed: the project's parent exists and its turn is running.
Clauses: [OPS-7.3](operations.md#ops-73-isolation-between-parents).
Action: read its state and steer the verified active turn with the handoff, rather than waiting for
idle or creating a second parent. A transport that refuses a message to an active task is
protecting that turn. If the turn changes between reading and sending, read again and reclassify
instead of retrying blind.
Preserved: one parent per project, its running work intact, and no duplicate writer.

### C4 The existing parent is idle

Observed: the project's parent exists, has no running turn, is not paused, cancelled or archived,
and its last result is older than the current project state.
Clauses: [OPS-7.1](operations.md#ops-71-what-an-assignment-binds).
Action: send the handoff by the ordinary message path with the restoration block, because an idle
task has usually lost the context its first prompt gave it. Refresh the brief to the current
revision before sending rather than resending the original.
Preserved: the same parent, the same binding, and a brief that matches what is true now.

### C5 The binding cannot be settled

Observed: two candidates match by name, or a record names a supervisor whose task cannot be
confirmed, or the designation does not resolve to one initiative.
Clauses: [OPS-7.1](operations.md#ops-71-what-an-assignment-binds),
[OPS-7.2](operations.md#ops-72-never-route-on-a-display-name-or-a-working-directory); the
ownership half is worked as S30.
Action: bind nothing. Report the exact ambiguity with its candidates and what would settle it, and
ask. Meanwhile continue any part of the work whose ownership is not ambiguous. Never pick by title,
by folder or by whichever record was read last.
Preserved: no supervision created on a guess, and no existing parent rebound by accident.

### C6 Read-only or report-only scope

Observed: the request names the initiative, and the scope forbids sending work, writing records or
both.
Clauses: [OPS-7.3](operations.md#ops-73-isolation-between-parents); worked as S31.
Action: resolve the binding, read the projects and their parents, and return the brief each parent
would receive, the reuse order and the gaps, without sending a handoff, creating a task or writing
a record. Name the one missing permission rather than substituting a narrower action and calling
it done.
Preserved: the limit, and a result the user can act on without repeating the investigation.

### C7 No-create scope where parents already exist

Observed: the designation authorizes execution but forbids creating tasks, and some projects
already have parents while others do not.
Clauses: [OPS-7.3](operations.md#ops-73-isolation-between-parents).
Action: settle who supervises first. Where a supervisor already exists, or this task is free to
take the binding, hand each existing parent its project and keep going through it: no-create
removes creation authority, not the execution the designation already carries, and reusing a
verified existing owner is what the project level does already when a creation path is
unavailable. Report only the projects that would need a parent created, naming that one missing
permission. Where no supervisor exists and this task is already bound, so the binding can be
neither taken here nor created elsewhere, nothing holds it: return every brief unsent with that
missing permission, because a handoff nobody supervises is one each receiver would have to
downgrade to a peer request anyway.
Preserved: the creation limit, and every project whose owner already exists still moving rather
than collapsed into a report nobody asked for.

### C8 Two supervisions start at once on different projects

Observed: two designations run concurrently, neither sees the other, and each hands a different
project to its parent before either reads again.
Clauses: [OPS-7.1](operations.md#ops-71-what-an-assignment-binds),
[OPS-7.3](operations.md#ops-73-isolation-between-parents).
Action: nothing here prevents that overlap, so the work is to bound and end it. The earliest
recorded binding stands, because a supervisor recording itself preserves the entry already there.
Each supervisor reads the initiative record again before its next handoff and at each returned
result, so the second is found within a step or two. The later one then sends no further handoffs,
withdraws the ones it made and names the owner. Each parent re-reads the initiative record itself
and applies the transfer rule above in full, including its comparison of the rest of that owner's
scope for this project. This case deliberately does not restate those outcomes; the rule above is
their only statement.
<!-- Do not re-summarise the transfer outcomes here: a summary in this case drifted from the rule
twice while the rule was being sharpened. -->
Preserved: one owner per initiative once it is found, the work already started by either side, and
an honest account of the window, which closes properly only when a store records the relationship
and refuses the second.

### C9 Part of the set is already finished

Observed: two projects in the approved set are complete, with their pull requests merged and their
outcomes verified.
Clauses: [OPS-7.1](operations.md#ops-71-what-an-assignment-binds); landing evidence per
[Implementation Done](../../crw-plan/references/integrations.md#implementation-done).
Action: record the verified outcome and its evidence pointer, and execute only what remains. Do not
reopen a finished project, re-verify a landing already verified at the same revision, or hand its
parent a brief it has already delivered. Completing the initiative still needs its finish condition
to hold on those outcomes together.
Preserved: finished work stays finished, and the initiative is not reported complete because some
of its parts are.
