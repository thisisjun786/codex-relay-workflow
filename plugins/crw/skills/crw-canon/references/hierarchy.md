# Hierarchy

The levels of the workflow, who owns what at each, the words used for them, and where the size of each level is set. Every skill that names a level links here instead of describing it again. The sections below are read together with [linear-model.md](linear-model.md) (what each level writes in Linear) and [execution-roles.md](execution-roles.md) (what each level runs as).

## The levels

Jun talks with the manager. The manager, the initiative lead, the project orchestrator and the issue child are the four levels; each is one Codex task bound to the scope in the table, and the binding is what makes the level.

| Level | Relay role id | Korean | Bound to | Talks with |
|---|---|---|---|---|
| manager | store seat | 관리 | the store, the one scope no Linear level owns | Jun, and the initiative leads and project orchestrators below it |
| initiative lead | `supervisor` | 부모 | one initiative ID | the manager above it and its project orchestrators below |
| project orchestrator | `parent` | 자식 | one project ID | its initiative lead above, its issue children below, a peer project orchestrator beside |
| issue child | `child` | 이슈 child | one issue ID | its project orchestrator |
| issue executor | none: an agent call of a project orchestrator's run | 없음 (레벨이 아님) | one issue node for one attempt | the run, through its journal |

The last row is no fifth level. In a flow run the issue executor does the work of one issue node in a thread the run starts: it is not a Codex task of its own, holds no binding and no relay role id, and the run alone speaks to it; what it returns is a receipt the run judges, not a message to a level. An oversized issue is not its work: that issue is an issue child, which coordinates an issue-lane run whose executor calls make the commits. Instruction text uses the level nouns of the first column. The relay role ids `supervisor`, `parent` and `child` are what a relationship store and a host's role policy record; they appear in code font and in this table, and a document that shows one reads it as the row it names. The store seat is recorded with the `supervisor` role and the scope `store`, which makes it a seat and no initiative lead: one task holds one seat, and the initiative leads stay one per initiative. The Korean column is Jun's own words for the levels. A reader is shown them, an instruction does not use them, and they key nothing.

## Scope of each level

Execution runs at the three levels below the manager, and each level is one Codex task bound to one Linear level by stable ID: an initiative lead to an initiative, a project orchestrator to a project, an issue child to an issue. That binding is what makes a level. A title, a folder, a branch or a chat link is not, so identity stays on the stable IDs under [OPS-7.1](../../crw-run/references/operations.md#ops-71-what-an-assignment-binds) and [OPS-7.4](../../crw-run/references/operations.md#ops-74-three-levels-and-their-routing-identity). Each scope has one active execution owner. Internal helpers acquire no ownership by receiving a subtask, an internal helper agent (the helper role policy's) is not an issue child, and a service-management daemon of the operating system is a different thing that merely shares a name with the relay role `supervisor`.

Writing the names down settles what they are called and nothing else. A display name in either language keys nothing, so no relationship id, assignment, goal, worktree or task title is regenerated because this section spells them out, and each level keeps the title convention that already fixes it: a verified project orchestrator follows [Set the app presentation and record](../../crw-plan/references/integrations.md#set-the-app-presentation-and-record), an issue child follows the [task titles of an issue child](../../crw-run/references/task-packet.md#child-task-titles), and a supervision task keeps its own name. Read earlier phrasing the same way rather than rewriting it: an initiative task, an initiative management task or a 상위 관리 세션 is the initiative lead under another description. That is how to read those words, not a licence to substitute them wherever they appear. A level word is not a designation either: a request, a prompt, a title or a folder saying 관리, 부모, 자식, manager or initiative lead designates nobody, so what a task is stays something read from its binding rather than from what anyone called it.

| Level | Bound to | Coordinates | Instructs |
|---|---|---|---|
| Initiative lead | one initiative ID | its initiative's projects: cross-project dependencies, priority, shared resources, and the order in which projects reach a shared target | the project orchestrators of those projects |
| Project orchestrator | one project ID | its project's issues: dependencies, sequencing, parallel issue children, delivery verification and integration; and, directly with a peer project orchestrator, the surface their projects share | its own issue children only; a peer project orchestrator is asked, never instructed |
| Issue child | one issue ID | nothing outside its own issue | its own internal helpers |

| Level | Verifies | Merges | Updates in Linear | Complete when |
|---|---|---|---|---|
| Initiative lead | each project orchestrator's reported project outcome against the initiative's finish condition | nothing; it decides cross-project order, never a landing ([OPS-9.3](../../crw-run/references/operations.md#ops-93-the-parent-integrates-and-does-not-release)) | the initiative record | the initiative's finish condition holds on its projects' verified outcomes |
| Project orchestrator | each issue child's delivery, checks and review against the issue's accepted criteria | its own project's issues, into their intended target | the project record and the issues it owns | every obligation in the agreed project scope is delivered, integrated and reconciled |
| Issue child | its own implementation and the local verification of its task branch | never ([OPS-9.3](../../crw-run/references/operations.md#ops-93-the-parent-integrates-and-does-not-release)) | nothing; it returns proposed record changes to its project orchestrator | the local verification record of the head passes and its blocking findings are resolved ([OPS-9.2](../../crw-run/references/operations.md#ops-92-what-normal-completion-means)); the issue itself is Done once its project orchestrator lands that head on the intended branch, under [Implementation Done](linear-model.md#implementation-done) |

The tables say what each level answers for. The manager is the level Jun talks with. It holds the store seat, sends to the initiative leads and project orchestrators through `crw manage`, and keeps what only Jun can decide; the initiative, project and issue records belong to the levels below it. What an initiative lead spends its time on is the level above the tables: it carries the requests Jun approves down to the project orchestrators, decides which project orchestrator takes which project, answers the midpoint check when Jun asks for one, collects what the project orchestrators report, and writes the initiative's record. The work it never takes is divided by name rather than left to the pair below it. The issue child implements, verifies and resolves the review on its own delivery (its task branch, or its pull request where one exists); the project orchestrator accepts that delivery against the issue's criteria and performs the integration; the initiative lead tests each reported project outcome against the initiative's finish condition and stops there. Holding no goal of its own narrows none of that: inside an execution approval still in force it moves the approved work and resumes the responsible project orchestrator, which is what [the midpoint check](../../crw-run/references/midpoint-check.md) already describes.

A ready batch means several separate issue children, not several issues assigned to one issue child, and the
same holds a level up: several ready projects mean several project orchestrators. Each level reads the level
below by result and does not repeat its work. An initiative lead does not redo its project orchestrators' issue
investigation, planning or assignment, and a project orchestrator does not redo its issue children's implementation or
review; running the same enquiry once per level is how one project's cost becomes three.

Instructions travel between adjacent levels, and only the owning project orchestrator instructs its own issue children.
An initiative lead that wants an issue child's work changed says so to that issue child's project orchestrator. Where reading a
lower level directly is genuinely necessary, the existing owner stays in place, and the route and
its reason are recorded.

Each level writes its own record and no other. The initiative lead's record is the initiative's own:
an authorized definition change follows the [initiative body standard](linear-model.md#initiative-body-standard),
decisions and dispositions are comments, and material progress is an update. The project orchestrator's records
are the project and the issues it owns. An issue child writes none. It returns the document or issue ID,
the revision it read, the reason, the smallest sufficient change and its evidence to its project orchestrator,
which decides and writes. An initiative lead's authority to write that record comes from its own
assignment exactly as a project orchestrator's does; the binding identifies the scope and grants nothing.
Accepted design text is preserved rather than rewritten.
The same boundary holds for every other sub-task an owner opens and for every route a write could
take; what a returned proposal carries, and what the owner does with it, are in
[record writes and returned proposals](linear-model.md#record-writes-and-returned-proposals).

Bind an initiative lead by stable initiative ID, the project orchestrator by stable project ID and each issue child by its
issue ID. An initiative spanning projects may have one execution initiative lead; it never becomes a
combined execution project orchestrator, and each contributing project keeps its one project orchestrator. A project
contributing to several initiatives keeps one execution initiative lead and one project orchestrator, and the other
initiatives reference its outcome instead of issuing it work, so no second initiative lead instructs
that project orchestrator or clones its issue children. Preserve other project coordinators and route cross-project
prerequisites by relation; do not absorb their issues. A project with no initiative lead, and a
standalone issue with no project, run exactly as they do today in a project- or issue-scoped task:
do not invent an initiative, a project or an upper task to complete the shape. These roles describe
relationships rather than ranks, so an issue-scoped task running without a project orchestrator is the owner of
its own scope rather than a delegated issue child: the rows above that reserve merging and the Linear
record for a project orchestrator describe the delegated case, and a standalone owner carries those duties as far
as its own assignment authorizes them. An explicit
current-task implementation request keeps that mode and issue scope; it is not evidence that an
independent issue child was created. Reuse the responsible issue child for the same issue's follow-ups, not for
a new issue. Explicit project-focus switches preserve old bindings and active ownership before
establishing the new one.

Approvals and limits travel down without widening. The user's restrictions, approvals, settings and
pause or cancel hold in the scope they were given, and being linked to a level above is neither an
expansion of authority nor a new goal ([OPS-7.3](../../crw-run/references/operations.md#ops-73-isolation-between-parents)).
The role decides what goal a task opens, never the other way round. An initiative lead opens none and
runs no automatic loop, and an issue child keeps its own goal and its implementation loop. What a
project orchestrator opens is not this section's fact to hold: it is the decision
[Start policy](../../crw-run/references/start-policy.md#roles-and-the-goal-each-one-opens) records,
that record has already changed once, and it is read there rather than remembered. A coordination
goal that does exist is its own and is never mixed with an issue child's implementation FSM, and an absent
goal says what a task is waiting on rather than which role it is.

This contract fixes the roles; it does not establish how an initiative lead is instantiated. A task
becomes an initiative lead only by an explicit designation naming the initiative, and an initiative link
by itself rebinds no existing project orchestrator. That designation, the approved project set it fixes, the
completion boundary and the reuse of the project orchestrators already running are in
[Initiative supervision](../../crw-run/references/initiative-supervision.md).
Start policy, creation authority and the limits that survive them stay where they
already are in [crw-run](../../crw-run/SKILL.md#independent-implementation-tasks), and this section
references them rather than keeping a second copy.

## The message both relations are read by

An initiative lead instructs a project orchestrator and a project orchestrator reports back; a project orchestrator instructs an issue child and an issue child
reports back. The two relations carry different machinery and the same questions, so what
identifies a message, what the recipient owes because it arrived, and how far it actually got are
decided once. This section is the workflow rule: which occasion is which, what each form carries,
and when the level above is woken. The record form itself - the field names, the version, and what
each state means to the code that builds a message - belongs to the relay, as `relay-envelope/1`
in `docs/relay/envelope.md`, and is read there rather than copied here.
The words below are the ones that document uses, so a project orchestrator and an initiative lead mean the same thing
by them.

**What a message asks for.** A request owes an answer from the recipient. A decision owes one from
Jun and from nobody else. A notification owes none. A status response answers a question somebody
put. The word is fixed by the occasion rather than chosen: an initial project assignment, a
midpoint check, a resume, a scope correction, a decision of Jun's carried down, and a user stop are
all requests, because each leaves the project orchestrator something to do. A decision Jun has already made is
relayed as a request for exactly that reason - what is owed is the application, not another
opinion. Upward, a completion and a block are notifications, a decision request is a decision, and
a midpoint answer is a status response.

A message carries no authority of its own. Being told something does not widen a scope, approve an
action, or open a goal; authority travels by the assignment, as
[scope of each level](#scope-of-each-level) already says.

**Initiative lead to project orchestrator** names the request, the approved scope, the constraints, what changed in a
decision already made, the result and the reporting condition expected, and who owns the next step.
**Project orchestrator to initiative lead** names the result and what changed, the project and issue it is about, the
evidence, what is blocked now and what that blocks, and the next action with its owner. A decision
subject appears only when a user decision is genuinely needed, with the reason and what each choice
costs. Technical judgment, ordinary review fixes and merge approval are the project orchestrator's own work and
are not handed upward; merge order between two project orchestrators is
[CRW-123 direct coordination](#direct-coordination-between-project-orchestrators).

**When the level above is woken.** A completion, a new real block, and a decision only the user can
make. Not a CI run changing, not an acknowledgement, not a wait that has not changed, not an issue child
progressing normally. An initiative lead holds no goal and every wake costs it a turn, so an automatic
notification that carries no decision is a cost with no reader. A midpoint check is the other path
and is answered on its own terms: it was asked for, so suppression of automatic notices is not a
reason to leave it unanswered.

**Five things that are not each other.** A send the transport accepted, a message received, a
request understood and agreed to, a change actually applied, and a result verified. Silence is none
of them. A conditional acceptance is not an application, a delivery failure is not a refusal, and
an unconfirmed or stale revision is not a current one. Where a relation has no mechanism for a
stage - and the initiative lead relation has one only for a report's send and its readback, and none
above those - the answer is that there is nothing to look at rather than that nobody has looked
yet.

**A report is not a record.** Producing a report, the transport accepting it, the recipient reading
it and Linear holding it are four facts. The obligation to report something upward is discharged by
the record the initiative lead reads, and a Linear write that failed leaves it owed however complete the
report text reads. The same logical result reported twice converges on one identity rather than
becoming two; a correction or a withdrawal preserves what it corrects and says which decision now
stands. A recipient that is paused, archived or deliberately out of contact keeps every obligation
it had and is not woken to collect them.

## Direct coordination between project orchestrators

Project orchestrators coordinate with each other directly, and that is the ordinary path rather than an
exception. Two projects whose work meets in the same files, interfaces, data or behaviour settle
between themselves what each will change and what must keep holding. An initiative lead that relayed
every message would become the bottleneck it exists to remove, so it decides only what a pair
cannot settle alone: an agreement they cannot reach, a change that widens either project's scope,
who owns newly discovered work, and shared resources, including the order in which projects reach a
shared target. The resource decision and the merge order are two decisions and are recorded as two.
Where the two projects share no initiative lead, or answer to different ones, neither initiative lead
acquires authority over the pair and none is invented or rebound: the project orchestrators record the unsettled
part, hold only that part while their independent work continues, and raise that decision to the
user, who is the owner it falls to. Two initiative leads coordinate across their initiatives by the same
route and the same form, and neither acquires authority over the other's projects by using it: a
requirement for a project one of them owns is raised with that project's own initiative lead, which
decides it.

Only the owning project orchestrator instructs its own issue children, and no project orchestrator instructs another. A peer
message is a request or an agreement: the project orchestrator that receives it decides what its own issues and
issue children do about it, and it never reaches into the other project's issue children. One project orchestrator cannot
assign work to another, and the initiative lead is the only level that can change who owns what. A peer message is not a delivery: it carries no receipt, no
acknowledgement and no verdict, and it never enters another project orchestrator's registered relationship
([OPS-7.3](../../crw-run/references/operations.md#ops-73-isolation-between-parents),
[OPS-7.4](../../crw-run/references/operations.md#ops-74-three-levels-and-their-routing-identity)).

An agreement names its target issues, the revision it is based on, the exact area each side will
change, the behaviour that must keep holding, the condition for accepting it, and who owns the next
step. Owning a file for this change is not owning every future change to it. Where part of the
surface is unsettled, hold that part and keep the independent work moving. When the base revision
or the interface moves, the agreements that depended on it are confirmed again rather than assumed.

Five things are distinct and are recorded separately: the transport accepted the message, the
recipient understood and agreed, the owning project orchestrator instructed its issue child, the change was actually
made, and the result was verified. Silence is not consent and a successful send is not agreement.
A conditional acceptance is recorded as conditional, together with the condition that would make it
applied, and it is never cited as applied evidence. Those facts are produced along one path that
stays inside the ownership that already exists: the project orchestrator that accepted a condition carries it to
its own issue child by the correction route it already uses, keeps that issue child's adoption evidence, which
is the revision where the change took effect rather than the issue child's acknowledgement, and returns
the outcome to the peer itself. An agreement between two project orchestrators binds no third
project: a follow-up proposal records its trigger, its acceptance condition and whether the next
owner accepted, and it stays marked unassigned until that owner accepts it explicitly. Where the
follow-up is a required dependency, unassigned is escalated as a blocker rather than left standing
as an implied promise.

A merge turn and an edit agreement are different things. Agreeing on an edit grants no merge
permission and creates no new project scope, and the merge itself stays with the owning project orchestrator
under [OPS-9.3](../../crw-run/references/operations.md#ops-93-the-parent-integrates-and-does-not-release). Use the shared
[Coordination message](../../crw-run/references/task-packet.md#coordination-message), reference the
values the existing relationship already holds instead of recopying them, and prefer one message
carrying a real state change over a heartbeat carrying none.

## Sizes at each level

A level's size is set where the work of that level is shaped, and the numbers live there and nowhere else. [crw-define](../../crw-define/SKILL.md#size-rules) sizes an initiative: one outcome, and how many of its projects run at the same time. [crw-plan](../../crw-plan/SKILL.md#size-rules) sizes a project, a milestone and an issue, and asks `crw skill issue-size check` for every issue. [crw-add-issue](../../crw-add-issue/SKILL.md#tell-the-caller-about-the-limits) tells its caller when the issue it adds would pass a project's or a milestone's limit. A size is advice to the person shaping the work, except the project limit that the plan itself enforces.
