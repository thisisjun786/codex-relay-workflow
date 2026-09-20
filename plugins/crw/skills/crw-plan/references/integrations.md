# Linear, CXC, and Paperthin integration

Shared guidance and Jun's workflow defaults for `crw-define`, `crw-next`, `crw-plan`, `crw-run`, `crw-loop`, `crw-status`, `crw-check`, `crw-logic`, and `crw-tidy`. Read the operation-specific skill for scope. Apply these defaults within the user's assignment and current host permissions.

## Skill names under each installation

These documents name the skills without a prefix, which is how a linked
installation exposes them. A plugin installation namespaces every skill under the
plugin, so `crw-run` is offered to the model as `crw:crw-run` and the same mapping
applies to each name above. Read a name written here as whichever spelling the
current installation exposes, and use the exposed spelling when invoking a skill
or telling a user how to invoke one. The directory name, the file layout, and the
relative links between these documents are identical in both installations.

## Resolve the project target

Use an explicit target for the current operation. When it is omitted, use the
current assignment and the verified management binding for this task. A
temporary question or link for another project does not change the persistent
binding; a change to that binding needs an explicit designation or switch.
Distinguish Linear project IDs, Codex task/host IDs, Desktop project IDs, and
repository identity. One repository may support several Linear projects, and
one project may involve several repositories. Resolve names to stable project
IDs using the supplied link, verified binding, and semantic scope. If same-name
candidates remain ambiguous, ask before writing or binding; do not pick by
title alone. A rename or changed initiative relation does not change a binding. An initiative is
itself a target only for an explicit designation to execute its approved projects, which binds at
that level through [Initiative supervision](../../crw-run/references/initiative-supervision.md);
cited as context by any other operation it changes no binding.

Use [Project parent binding](integrations.md#project-parent-binding) to designate, record, restore,
or switch the fixed management task, including its app title and pin. Refresh
volatile state before acting. An old or copied record locates context but does
not transfer another task's ownership or execution permissions. Keep binding
setup in that shared procedure and the requested operation with its existing owner.
Run and Loop perform this setup themselves. A binding-only request does not execute work.

## Project parent binding

Run and Loop use this shared setup and recovery procedure before execution.

Make this Codex task the continuing management point for one Linear project.
Preserve that target across follow-up requests and context recovery. This parent
orchestrates one project; its independent children each orchestrate one issue
under the shared [supervisor, parent and child scope](#supervisor-parent-and-child-scope), and a
supervisor above it, where an initiative has one, works through this parent rather than through
its children.

### Establish the link

Resolve the actual current task ID, host when available, and cwd from the host's
current identity and supported task tools. Resolve the supplied Linear project
to its stable ID and URL; read its linked canonical documents and relevant
current work. Inspect repository identity, applicable guidance, branch,
worktrees, and dirty changes when a repository is involved. A folder, task
title, or Desktop project ID is not a Linear project ID.

Look for an existing management binding in the project's linked coordination
record before changing anything. Match task, host, and project IDs. Reuse the
same binding on repeated requests. If another task is already the coordinator,
read its recorded ownership and current status without waking it. Ask only
when the user has not resolved a material ownership conflict.

An explicit replacement can update the coordinator link while preserving the
previous binding and its history. It does not transfer active workers or
execution permissions, message the old task, or unpin/archive it. A request to
inspect another project temporarily does not replace the persistent focus.

### Set the app presentation and record

A request to make this the fixed management task covers its matching title,
sidebar pin, and a compact Linear management record. Respect an explicit title,
no-rename, unpinned, or read-only constraint.

Use a concise project summary as the management task title: the short outcome the
project exists for, named so the title also reads as the task that manages it.
Preserve an explicit
user title. Product family and initiative membership are context, not required
title prefixes; no initiative or multiple initiatives needs no title-choice
question. Resolve same-name projects by stable ID under the shared target rules
before binding. If their management titles would be indistinguishable, append a
short project-ID suffix. Do not rename the initiative or project itself. This
convention names the management task; execution-task titles follow
[Child task titles](../../crw-run/references/task-packet.md#child-task-titles).

Discover the supported task rename and sidebar tools and their current schemas.
Apply changes only to the verified current task, and check its resulting title
and pin state in the app listing. If a capability is unavailable, complete the
supported parts and report the gap. Do not edit a session database or global
configuration to simulate a successful binding.

Reuse a suitable linked coordination document. When only a canonical planning
document exists, a compact management section there is sufficient. Read scoped,
paginated document listings and candidate contents before creating a document.
Create a small project-linked coordination document only if none is suitable.
Preserve specifications, unrelated content, and existing human edits.

Record only what supports recovery:

- Stable Linear project ID/URL and canonical document links.
- Actual management task ID, host when known, and observed task title.
- Repository identity and checkout path when relevant.
- Assignment, delivery limits, and the source/date of the user's designation.
- Verified app title/pin results and any unsynced or unverified part.

Read back the saved document and its project relation. Reconcile uncertain
writes by reading before retrying; an accepted request is not a verified result.
If Linear access is unavailable or writes are outside scope, retain a clearly
unsynced summary in the task's permitted private location and return the missing
step. Never store project bindings in installed skills, repository procedures,
or global memory. A verified title/pin and a verified Linear record are separate
claims; neither proves automatic wakeups or background execution.

### Continue from the fixed project

On recovery, locate this task's recorded binding and refresh live project,
document, and managed-task state before acting. Use current assignment context
and scoped recall to find a lost record. A copied binding for another task does
not assign ownership here. Follow the shared target-resolution rules for
explicit one-off targets and changes to the persistent link.

Keep the recorded project ID when its name, product labels, or initiative
relations change. Existing titles with an initiative prefix are presentation,
not a reason to rebind or rename during recovery. Refresh the title only within
an authorized presentation change; do not migrate existing bindings implicitly.

Load the existing owner for the requested operation:

| Request | Owner |
|---|---|
| Where to start or what to do next | [crw-next](../../crw-next/SKILL.md) |
| Define initiative intent or goal | [crw-define](../../crw-define/SKILL.md) |
| Plan, roadmap, milestones, or issue scope | [crw-plan](../../crw-plan/SKILL.md) |
| Execute the project without a parent goal, coordinate progress, or follow up on delivery | [crw-run](../../crw-run/SKILL.md) |
| Create/restore a parent goal for automatic project continuation | [crw-loop](../../crw-loop/SKILL.md) |
| Execute an initiative's approved projects through their existing parents | [crw-run](../../crw-run/SKILL.md), entering at [Initiative supervision](../../crw-run/references/initiative-supervision.md) rather than at the project binding above |
| Compare delivery with accepted requirements | [crw-check](../../crw-check/SKILL.md) |
| Investigate contradictions or broken invariants | [crw-logic](../../crw-logic/SKILL.md) |
| Find records that fall short of the agreed authoring rules and supplement the clear gaps | [crw-tidy](../../crw-tidy/SKILL.md) |

Keep one operation owner and load only the helpers it needs. Jun need not name
the skills. Binding alone does not launch the backlog, create workers or goals,
activate a CXC Loop, change model settings, or install an automation. When the
same request also authorizes execution, finish the link and continue through
the requesting Run or Loop owner in that scope. Preserve an existing authorized run and its routine
follow-up; a status question does not pause it. When another operation owner
uses this reference for binding setup or recovery, return the result to that caller
instead of recursively starting its operation.

Return the linked project/document, actual app result, recorded scope, and one
next step or meaningful gap. Distinguish completed binding from work execution.

## Linear operating model

Keep goals, product classification, and repository identity separate:

- An initiative describes a goal and its completion condition, not a permanent
  product bucket. A project may have no initiative or contribute to several.
  Preserve its stable ID and issues across those relations; do not clone them
  or sum shared progress as separate output. Verify connector support and read
  back relations before claiming that a requested link exists.
- Create a project when the user requests one, including an accepted proposal
  or a request to create/update a full Linear plan from goal through issues.
  That write request covers the needed hierarchy in its agreed scope; a narrow plan update, large backlog, or multiple
  repositories alone does not authorize extra projects. Consultation and
  draft-only planning do not authorize Linear writes. Reuse existing IDs and meaningful scope. Name the
  result naturally without a fixed product prefix. Do not rename existing
  projects or migrate their relations merely by loading these instructions.
- Product family is a single-choice project label group. Reuse the existing
  workspace values; a common project without one owning product may leave it
  empty and describe its shared scope. Do not split the project automatically
  or treat that empty classification as an error.
- Repository classification belongs on issues, not projects. Use one actual
  edit-target label per implementation issue in the `저장소` issue-label group.
  Name the value after the repository, adding the owner when names collide;
  keep its description to the exact repository URL. Verify label identity,
  group membership, and the resulting assignment. Keep the explicit owner/repo
  or URL in the issue body too. Reference-only and legacy repositories belong
  in context links, not additional execution labels. Non-code work and unresolved
  targets may leave the group empty; resolve a code target before execution.
- Reuse agreed labels. Discuss any additional label scheme when a need arises
  instead of creating it automatically. Preserve unrelated labels; do not copy
  all project labels to issues. Keep descriptions brief. Views, filters, and
  default screens belong to the user and are configured only when requested.

For example, a requested “Complete installation and first launch” project can
have one product family while its core, desktop, and installer issues each carry
their own edit-target repository label. A shared planning project can omit a
product family, and a non-code planning issue can omit a repository label.
Neither example requires an initiative or a product prefix in the project name.

Do not migrate existing labels merely by loading this model. When migration is
authorized, classify each issue from its accepted scope and delivery evidence,
not by copying its former project's repository labels. Preserve historical
multi-repository exceptions without forcing a false single target; reconcile
them before new execution. Remove only the superseded repository project labels,
preserving product labels, context links, unrelated fields and history.

### Issue-to-PR mapping

One implementation issue corresponds to one PR, and that PR delivers one
implementation issue. Split work requiring several PRs into separate issues
with explicit dependencies, even within one repository. Keep a multi-repository
outcome in one project when appropriate, with one issue per repository PR.
Batches coordinate separate issue/PR pairs; they do not combine issues into one
PR. Referencing a related issue is not claiming to deliver or close it.

Keep review fixes on the same issue and PR. A necessary replacement PR retains
the superseded link and names the one current delivery PR; it does not create a
second simultaneous delivery for the issue. A new change after that delivery
has merged gets a new issue and PR. Research, design, or operational work with
no repository change uses an explicit non-PR result and verification; do not
create an empty PR merely to fit the rule.

When existing work breaks this mapping, reconcile its scope and ownership
through `crw-plan` before new dispatch. Preserve active work, IDs, and history;
do not silently split, close, or reassign live issues. Editing these instructions
does not migrate existing work or alter the relay's runtime contracts.

### Schedule baseline contract

A recorded schedule creates facts that later operations read back: what a result was first
expected by, what it is expected by now, what actually happened, and what it waits on. Those
facts already live on Linear items and in the records around them, so this section fixes their
names, their sources and the checks over them rather than adding a second place to keep them.
Nothing here computes a date: there is no schedule database and no scheduler in this model.
`crw-plan` records these fields when a plan write is authorized, and any operation that later
compares progress with the plan, including [crw-check](../../crw-check/SKILL.md) and the
interim status reporting that consumes the same comparison, reads them under these names. A
record uses the field names as written here, and the value words below are fixed, because two
operations that paraphrase them stop agreeing about the same item.

| Field | What it holds, and where it is read from |
| --- | --- |
| Subject | The stable Linear ID and URL of one project, milestone or issue. A title, a branch or a checkout is not a subject. |
| Schedule baseline | The target date that the earliest record established for this subject, including the record a plan write creates at the moment it sets that subject's first target. It is `unconfirmed` only where a target already exists and no record establishing it can be found, and it is never filled in from the item's current date field or its creation time. |
| Baseline record | The record that set the schedule baseline, with its own timestamp: the identified project or initiative update, the planning record, or a dated decision comment. For a first target it is the dated record written alongside that target; for an older target whose establishing record cannot be found, its absence is what makes the schedule baseline `unconfirmed`. |
| Planned start | The date the subject is scheduled to begin, where the item carries one, which today is the project's own start date. It is a plan rather than an observation, so it never stands in for an actual start, and moving it follows the same change rule as a target. |
| Current target | The date the item carries now: the project's own target date, the milestone's target date, or the issue's due date. |
| Timezone | The IANA zone the dates were read and written in, stated rather than assumed. These targets are calendar dates, so a target means the end of that day in this zone and an evidence timestamp is compared in it. |
| Target nature | One of `confirmed`, `provisional`, `undetermined` or `awaiting authority`, recorded in the subject's own description because no Linear field carries it. |
| Actual start and finish | The real dates the work began and ended, each with the evidence that establishes it, and empty where no evidence does. |
| Change source | For the most recent change: its time, whose decision it was, the reason, the scope it covered including the stable IDs added and canceled with it, and the before and after dates, written into the record's own text. |
| Prerequisites | The subjects this one requires, each with the level it is recorded at, issue, milestone or project, and the relation or record line that carries it. |
| Delivery evidence | The same-class delivery records the item's target was last read from: their stable IDs and the times bounding each one, from assignment to the result that finished it, through the pull request and its review where that work had one and to the acceptance of its agreed artifact where it did not. It is `no sample` where no comparable record existed at that reading, and it holds those records rather than any figure derived from them. A change that reads the records again names them in its own entry and becomes the reading this field carries. A change that moves the date without reading them leaves the earlier reading here, and the change source is then what produced the date the item carries now. Records are never rewritten to claim a date they did not set, and `no sample` never stands for a date another source moved. |
| Wait cause | What the subject waits on now, one of `none`, `prerequisite`, `shared surface`, `review`, `decision` or `host`, named together with the subject, region, pull request, decision or stated constraint it waits on and the observable event that ends it. |
| Change reason | The word the most recent change's reason opens with, one of `pulled in`, `scope changed`, `blocked`, `critical path` or `authority`. It classifies the change the change source already records and replaces none of its text. |

`confirmed` is a delivery date an authority agreed to. `provisional` is a planning target the plan
proposed and may move. `undetermined` is an in-scope subject with no target yet. `awaiting authority`
is a subject whose date needs a decision nobody has made. An actual start date is none of these:
it is an observed fact about work that began, it never becomes a target, and a subject that has
started still carries a target nature of its own.

The wait cause words divide the same way. `none` means nothing holds the subject back now, and
whether it has begun is read from its execution state rather than from this word, so the
immediately startable batch is the subjects carrying `none` that are not already reported started.
`prerequisite` names a result this subject cannot finish without, including a whole project an
authority placed behind another. `shared surface` names a region another subject must change
first. `review` covers the subject's own delivery waiting on its review, on the checks required of
it, or on the merge that has not happened. `decision` covers an approval nobody has given, an
installation sign-off and the user's own stop. `host` covers a stated constraint or a slot the
plan cannot create. A wait cause describes the subject carrying it and never its predecessor, so a
subject waiting on a result that is itself in review carries `prerequisite` while that predecessor
carries `review`; where two words still fit, the cause that must resolve first wins and a tie
breaks in the order listed here.

The change reason words are fixed in the same way, and each names the cause rather than the
direction, because the same cause can move a date either way. `scope changed` is a move the
accepted scope's own change produced, including scope canceled from it, which can bring a date
forward. `blocked` names a real blocker on the subject itself, and `critical path` a predecessor's
own recorded move that shifted it. `pulled in` is the earlier move the plan makes after a confirmed
result. `authority` is a date the deciding authority itself set, in either direction, because an
authority can bring a deadline forward with no predecessor result behind it. Where more than one
fits, the first that applies wins, in the order `scope changed`, `blocked`, `critical path`,
`pulled in`, `authority`, so scope an authority approved reads as `scope changed` and a blocker
reads as `blocked` on the subject it blocks and as `critical path` on the successors its recorded
move shifted. Three facts are not change reasons at all: that a checkpoint ran, that the work is
still unfinished, and that the target is approaching or has passed. A target that moves on one of
those records a delay it is hiding.

Records disagree, so their precedence is fixed. The item's own date fields are the planned start and
the current target.
The most recent record carrying a date change is the change source. The earliest record that
established a target is the schedule baseline. The subject's description carries its target nature
and timezone note and nothing that competes with those fields. Each record states its before and
after dates in its own text, so an operation that cannot see a rendered diff still reads them.
Where the current target disagrees with the latest change record's after value, the comparison is
unverified until the two are reconciled; neither side is silently preferred.

A start and a finish take different evidence. An actual finish is evidenced by the landing of the
delivering pull request under [Implementation Done](#implementation-done), by an explicit user
statement, or by a dated decision record. An actual start is evidenced only by a record of the work
beginning: the subject's own start record, an explicit user statement, or a dated decision saying
when it began. A landing dates the finish and says nothing about the beginning, so it never fills an
actual start, and an actual start nothing evidences stays empty rather than borrowing the finish. A
status timestamp that a status correction or a bulk edit produced evidences neither, because it
records when someone fixed the register rather than when the work happened; where two admissible
dates disagree the earlier evidenced one stands. A subject already Done or Canceled keeps the dates it
has and is not given a new target.

A prerequisite is recorded at the level that is actually true. A project-level prerequisite means
the whole project must finish first. When a result needs only particular outputs, the prerequisite
belongs on the issues that produce them or on the milestone that groups them, so the remaining
work in both projects keeps running in parallel. Cross-project prerequisites travel by relation
under [supervisor, parent and child scope](#supervisor-parent-and-child-scope); one project does
not absorb another's issues to express a date, and no finish-to-start relation is invented to make
a chart read better.

Four defects are checked over the resulting graph, and each check reports what it found including
when it found nothing: a cycle among prerequisites; a prerequisite whose target falls after the
target of the result that needs it; an issue carrying a milestone's target while sitting outside
that milestone; and a required prerequisite absent from the graph entirely.

A comparison built on these fields names which datum it measured against, the schedule baseline or
the current target, and reports both where a reader needs both; measuring against one and
presenting it as the other is what makes a moved date look like progress. Naming the verdicts that
comparison reaches belongs to the operation performing it rather than to this contract, so the same
fields never acquire two meanings.

Two report states are named, because an operation reading the result must classify the same fact
the same way. `recorded in the data` means the dates, links and relations were written and read
back. `not applied in the view` means a scale, an ordering or a connection line the connector does
not expose was left alone. Neither stands for the other.

Reading this contract grants nothing. Recording a schedule stays inside the plan write authority
the request already carries; a check-only or draft-only request returns these fields as a proposal
and writes none of them; and a registered date is not an assignment, an execution start, an
installation or a restart. The Plan-side procedure that builds, checks and records these fields is
[Schedule and dependency roadmap](scheduling.md).

A consumer judges the result rather than its wording: every in-scope subject has a row or an
explicit `undetermined`; what was read back equals what was written; running the same request again
adds no milestone and no second record; a Done or Canceled subject's dates are unchanged; and a
target date that moved still shows the schedule baseline it moved from together with the change
source that moved it. Every in-scope subject also carries a wait cause; a target that moved later
carries one of the extension words with the scope, blocker or predecessor entry it names; a target
that moved at all carries the word for what moved it, whichever direction it went, naming the scope
change, the blocker, the predecessor's entry, the confirmed result behind a `pulled in`, or the
authority's own decision; and a schedule whose
targets were all rewritten from one current date fails whatever the request that produced it was
called.

### Supervisor, parent and child scope

Execution runs at three levels, and each level is one Codex task bound to one Linear level by
stable ID: a supervisor to an initiative, a parent to a project, a child to an issue. That binding
is what makes a role. A title, a folder, a branch or a chat link is not, so identity stays on the
stable IDs under [OPS-7.1](../../crw-run/references/operations.md#ops-71-what-an-assignment-binds) and
[OPS-7.4](../../crw-run/references/operations.md#ops-74-three-levels-and-their-routing-identity). Each scope has one active
execution owner. Internal helpers acquire no ownership by receiving a subtask, a CXC internal
helper agent is not a child, and the operating system's process supervisor is a different thing
that happens to share the word.

| Role | Bound to | Coordinates | Instructs |
|---|---|---|---|
| Supervisor | one initiative ID | its initiative's projects: cross-project dependencies, priority, shared resources, and the order in which projects reach a shared target | the parents of those projects |
| Parent | one project ID | its project's issues: dependencies, sequencing, parallel children, delivery verification and integration; and, directly with a peer parent, the surface their projects share | its own children only; a peer parent is asked, never instructed |
| Child | one issue ID | nothing outside its own issue | its own internal helpers |

| Role | Verifies | Merges | Updates in Linear | Complete when |
|---|---|---|---|---|
| Supervisor | each parent's reported project outcome against the initiative's finish condition | nothing; it decides cross-project order, never a landing ([OPS-9.3](../../crw-run/references/operations.md#ops-93-the-parent-merges-and-does-not-release)) | the initiative record | the initiative's finish condition holds on its projects' verified outcomes |
| Parent | each child's delivery, pull request, checks and review against the issue's accepted criteria | its own project's issues, into their intended target | the project record and the issues it owns | every obligation in the agreed project scope is delivered, integrated and reconciled |
| Child | its own implementation and the review on its one pull request | never ([OPS-9.3](../../crw-run/references/operations.md#ops-93-the-parent-merges-and-does-not-release)) | nothing; it returns proposed record changes to its parent | the current head's required checks have passed, its required reviews have finished and its blocking findings are resolved ([OPS-9.2](../../crw-run/references/operations.md#ops-92-what-normal-completion-means)); the issue itself is Done once its parent lands that pull request, under [Implementation Done](#implementation-done) |

A ready batch means several separate children, not several issues assigned to one child, and the
same holds a level up: several ready projects mean several parents. Each level reads the level
below by result and does not repeat its work. A supervisor does not redo its parents' issue
investigation, planning or assignment, and a parent does not redo its children's implementation or
review; running the same enquiry once per level is how one project's cost becomes three.

Instructions travel between adjacent levels, and only the owning parent instructs its own children.
A supervisor that wants a child's work changed says so to that child's parent. Where reading a
lower level directly is genuinely necessary, the existing owner stays in place, and the route and
its reason are recorded.

Each level writes its own record and no other. The supervisor's record is the initiative's own:
an authorized definition change follows the [initiative body standard](#initiative-body-standard),
decisions and dispositions are comments, and material progress is an update. The parent's records
are the project and the issues it owns. A child writes none. It returns the document or issue ID,
the revision it read, the reason, the smallest sufficient change and its evidence to its parent,
which decides and writes. A supervisor's authority to write that record comes from its own
assignment exactly as a parent's does; the binding identifies the scope and grants nothing.
Accepted design text is preserved rather than rewritten.

Bind a supervisor by stable initiative ID, the parent by stable project ID and each child by its
issue ID. An initiative spanning projects may have one execution supervisor; it never becomes a
combined execution parent, and each contributing project keeps its one parent. A project
contributing to several initiatives keeps one execution supervisor and one parent, and the other
initiatives reference its outcome instead of issuing it work, so no second supervisor instructs
that parent or clones its children. Preserve other project coordinators and route cross-project
prerequisites by relation; do not absorb their issues. A project with no supervisor, and a
standalone issue with no project, run exactly as they do today in a project- or issue-scoped task:
do not invent an initiative, a project or an upper task to complete the shape. These roles describe
relationships rather than ranks, so an issue-scoped task running without a parent is the owner of
its own scope rather than a delegated child: the rows above that reserve merging and the Linear
record for a parent describe the delegated case, and a standalone owner carries those duties as far
as its own assignment authorizes them. An explicit
current-task implementation request keeps that mode and issue scope; it is not evidence that an
independent child was created. Reuse the responsible child for the same issue's follow-ups, not for
a new issue. Explicit project-focus switches preserve old bindings and active ownership before
establishing the new one.

Approvals and limits travel down without widening. The user's restrictions, approvals, settings and
pause or cancel hold in the scope they were given, and being linked to a level above is neither an
expansion of authority nor a new goal ([OPS-7.3](../../crw-run/references/operations.md#ops-73-isolation-between-parents)). A
supervisor's or a parent's coordination goal is its own, and neither is mixed with a child's
implementation FSM.

This contract fixes the roles; it does not establish how a supervisor is instantiated. A task
becomes a supervisor only by an explicit designation naming the initiative, and an initiative link
by itself rebinds no existing parent. That designation, the approved project set it fixes, the
completion boundary and the reuse of the parents already running are in
[Initiative supervision](../../crw-run/references/initiative-supervision.md).
Start policy, creation authority and the limits that survive them stay where they
already are in [crw-run](../../crw-run/SKILL.md#independent-implementation-tasks), and this section
references them rather than keeping a second copy.


#### Execution settings by role

Each level runs on its own model and reasoning effort, and which pair belongs to which role is a
recorded product decision rather than something a task infers. The supervisor's model is Jun's own
selection and is never propagated, changed by automation, or copied to the level below it; the
parent's and the child's come from the role policy.

| Role | Model and effort | Who decides |
| --- | --- | --- |
| Supervisor | its own recorded setting | Jun, directly |
| Parent | the role policy's parent pair | this policy |
| Child | the role policy's child pair | this policy |

Read the pair rather than remember it. The host's execution policy declares each role's pair, and
`get_capabilities` reports what it declares, so a creation states the pair that policy reports for
the role it is creating and passes the role itself so the host checks the answer. Never carry a
pair from memory, from another project, or from a document — including this one. That habit is
what the failure was made of: a coordinator created a project parent on a model it remembered,
and another retried a withheld send by changing the model and keeping the previous effort.
Reasoning-effort names are catalog values belonging to their own model; two that look similar are
not interchangeable, and nothing maps one onto another.

A role the host's policy does not declare is a blocker to report, and its recovery is declaring it
in that host's execution policy. There is no fallback pair, because a fallback is a default and a
default is the second source of truth this arrangement exists to remove.

Changing an existing task's model is a user action plus a re-record, never something a message
performs. After the user changes it, the authorization recorded for that task is re-recorded from
a user-attributed source before the next send, because the record is what a send verifies against
and a value observed on the host is evidence of what the task is running rather than a new
approval. Until then the send is refused naming the record, and a task the host reports as not
loaded is left alone rather than resumed under a pair that was never checked against its role.

### Direct coordination between parents

Parents coordinate with each other directly, and that is the ordinary path rather than an
exception. Two projects whose work meets in the same files, interfaces, data or behaviour settle
between themselves what each will change and what must keep holding. A supervisor that relayed
every message would become the bottleneck it exists to remove, so it decides only what a pair
cannot settle alone: an agreement they cannot reach, a change that widens either project's scope,
who owns newly discovered work, and shared resources, including the order in which projects reach a
shared target. The resource decision and the merge order are two decisions and are recorded as two.
Where the two projects share no supervisor, or answer to different ones, neither supervisor
acquires authority over the pair and none is invented or rebound: the parents record the unsettled
part, hold only that part while their independent work continues, and raise that decision to the
user, who is the owner it falls to. Two supervisors coordinate across their initiatives by the same
route and the same form, and neither acquires authority over the other's projects by using it: a
requirement for a project one of them owns is raised with that project's own supervisor, which
decides it.

Only the owning parent instructs its own children, and no parent instructs another. A peer
message is a request or an agreement: the parent that receives it decides what its own issues and
children do about it, and it never reaches into the other project's children. One parent cannot
assign work to another, and the supervisor is the only level that can change who owns what. A peer message is not a delivery: it carries no receipt, no
acknowledgement and no verdict, and it never enters another parent's registered relationship
([OPS-7.3](../../crw-run/references/operations.md#ops-73-isolation-between-parents),
[OPS-7.4](../../crw-run/references/operations.md#ops-74-three-levels-and-their-routing-identity)).

An agreement names its target issues, the revision it is based on, the exact area each side will
change, the behaviour that must keep holding, the condition for accepting it, and who owns the next
step. Owning a file for this change is not owning every future change to it. Where part of the
surface is unsettled, hold that part and keep the independent work moving. When the base revision
or the interface moves, the agreements that depended on it are confirmed again rather than assumed.

Five things are distinct and are recorded separately: the transport accepted the message, the
recipient understood and agreed, the owning parent instructed its child, the change was actually
made, and the result was verified. Silence is not consent and a successful send is not agreement.
A conditional acceptance is recorded as conditional, together with the condition that would make it
applied, and it is never cited as applied evidence. Those facts are produced along one path that
stays inside the ownership that already exists: the parent that accepted a condition carries it to
its own child by the correction route it already uses, keeps that child's adoption evidence, which
is the revision where the change took effect rather than the child's acknowledgement, and returns
the outcome to the peer itself. An agreement between two parents binds no third
project: a follow-up proposal records its trigger, its acceptance condition and whether the next
owner accepted, and it stays marked unassigned until that owner accepts it explicitly. Where the
follow-up is a required dependency, unassigned is escalated as a blocker rather than left standing
as an implied promise.

A merge turn and an edit agreement are different things. Agreeing on an edit grants no merge
permission and creates no new project scope, and the merge itself stays with the owning parent
under [OPS-9.3](../../crw-run/references/operations.md#ops-93-the-parent-merges-and-does-not-release). Use the shared
[Coordination message](../../crw-run/references/task-packet.md#coordination-message), reference the
values the existing relationship already holds instead of recopying them, and prefer one message
carrying a real state change over a heartbeat carrying none.

### Resolve the implementation repository

Resolve the issue repository label and its explicit GitHub owner/repo or URL
against its accepted scope, current delivery PR and existing assignment.
Project context links and any remaining legacy project labels do not assign
repositories to its issues. Identify reference-only repositories separately.
If the issue's label or explicit target conflicts with its PR or ownership record, reconcile the conflict
before writes; a label, folder name or convenient checkout does not break the tie.
Ask only when the current evidence cannot settle a material target choice.

Before new execution, verify the actual remote URL, intended integration branch
from repository policy, and full fetched baseline commit. A default branch and a
remote named `origin` are not universal integration targets. Record the selected
remote name and distinguish a contribution fork from its integration repository.
Use the existing workspace-assignment interface after this identity check; do not
create a new checkout allocator or a fake repository/project to fill missing labels.

On resume, recover the responsible task, checkout and issue branch first. Inspect
its worktrees, dirty state, local-only commits, remote identity and recorded
baseline. Fetching current truth does not authorize resetting, rebasing, cleaning,
stashing, moving or recreating that work. Reconcile ancestry and prerequisites
without replacing an existing assignment with a freshly cloned default branch.
For a new task, use a task-owned checkout under the applicable workspace policy.

Work needing PRs in multiple repositories becomes separate dependent issue/PR
pairs; reading or validating another repository alone does not make it a code
target. Common projects and standalone issues follow the same resolution rule.
Research or design with no repository change may explicitly have no code target;
do not require a remote, branch or Git baseline for that non-PR result.
Keep its source baseline and delivered output identity under the non-PR evidence rules.

### Implementation Done

For the current one-issue/one-PR model, an implementation issue is Done when its
one current delivery PR is actually merged into the intended integration target. Read GitHub's current PR identity,
repository, base branch, merged state and landing commit against the issue's
accepted scope. A related/reference PR, superseded replacement, approval, green
CI, merge-ready flag or closed-but-unmerged PR is not that evidence. Merging a
prerequisite branch into another task branch is not integration into the intended
target. Verify the landing rather than treating an accepted merge request as done.

The PR must deliver the issue's accepted implementation scope; a partial merge
cannot hide remaining required implementation. For an already-approved legacy
multi-PR issue, preserve links, owners and history, inventory required deliveries
and reconcile through `crw-plan` before new dispatch. Its completion uses all
reconciled required PRs actually integrated into the intended target and their
combined coverage of that issue's accepted criteria, not a demand that one PR
cover everything. Assess a PR's contribution to each linked issue independently;
a shared PR cannot complete another issue's remaining scope. Do not mark a legacy
issue complete on its first partial merge or retroactively manufacture completed issues.

Release, deployment, installation and live behavior are separate claims. New
plans track those operational results separately from the implementation PR.
A child's delivery, its parent's acceptance, the merge into the intended target, installation,
observed live behavior, the project's completion and the initiative's completion are seven
separate claims in the same way: one child's finished goal, or a few projects marked Done, does
not establish the level above it.
For an existing issue whose accepted criteria already require installation or
live verification, preserve those obligations until fulfilled or explicitly
re-scoped within authorization; a merge alone does not erase them. Operational
work that is genuinely outside the issue's criteria does not delay its Done.
Any accepted non-PR work, including research, design, verification or operations,
completes on its agreed observable result.
Completion evidence does not supply merge, closure, release or deployment authority.

Read the team's current GitHub status automation when reconciling it with this
rule. Distinguish closing/delivery links from contributing/reference links and
retain one current delivery PR when replacing a PR. A generic merge-to-Done rule
may not establish the intended branch or entire scope; check GitHub evidence even
when Linear already says Done. Record any conflict and correct the owned issue
only within the assignment. Do not test by changing unrelated live issues, silently
change workspace automation, or count a settings screenshot as event-delivery proof.
Respect agreed stale-issue cancellation and archival settings; Canceled is not Done.

## Linear holds canonical documents

Jun keeps product intent, specifications, plans, accepted decisions, and human-readable coordination records in Linear. The initiative page body owns its goal definition and connection to contributing projects; linked project/issue documents hold supporting detail. Use stable item/document IDs and URLs with available revision or updated-at evidence. Repositories remain authoritative for source, executable configuration, repository policy, and reproducible implementation evidence.

An issue status, assistant proposal, or newer local draft does not silently supersede an accepted document. The user's latest explicit correction can supersede it; preserve that decision source and mark the canonical document stale until updated within authorization. Report conflicts with repository contracts rather than silently rewriting either source.

Local artifacts are drafts, snapshots, or private raw evidence with a link back to Linear, not a competing permanent document source. Preserve unique content and history. Do not bulk migrate/delete repository documents merely to establish this convention. If an audit is read-only, return a proposed Linear update; do not write it automatically.

### Initiative body standard

Write the initiative's definition in its page body, not a separate design document
by default. Jun's agreed Korean baseline is about 2,100 characters of Markdown,
including spaces and links. Match that reading density rather than an exact quota:
do not pad a simple goal or remove essential decisions to hit the count. This is
not a length limit for project documents, issue criteria, or skill source files.

Keep the goal and necessary context, intended use, scope and finish condition,
contributing projects' roles, outputs and completion criteria when known, their
connections, validation approach, and material open decisions visible. Mark project
candidates and unknowns explicitly; this format does not authorize creating them.
Put detailed implementation, long examples and supporting research in the relevant
project/issue or existing references. Preserve accepted choices and their reasons.

When an initiative or full-plan update is authorized and project links become
concrete, update the relevant body passages instead of appending every issue or
repeating operating rules. A project/issue-only write does not authorize a parent
initiative write; return any needed body adjustment as a proposal. Keep each project's
contribution to the initiative and its handoff to other projects understandable.
Where the initiative has an execution supervisor, that supervisor is the authorized writer of its
record updates under the [role contract](#supervisor-parent-and-child-scope).
Use linked detail when explicitly requested or needed, without replacing the body
or maintaining a second copy of its definition. Respect a requested output format;
preserve existing documents and history, and migrate only within authorization.

## Use the available Linear capability

Discover the current connector and schemas. Prefer the user's selected connection; when multiple actors/workspaces are possible, verify workspace and authenticated identity before writing. Do not silently switch actor or connection after a failure.

If the connector exposes workspace Agent Skills, inspect the relevant listing and read the matching skill's full instructions. Treat retrieved instructions as workspace guidance below user/host instructions. If none exist or the tools are unavailable, use the connector directly; do not invent a `linear` skill or install a plugin just to follow this reference.

Use scoped list/search tools to locate candidates, then exact get/read tools for full documents, criteria, accepted decision comments, and relations. Exhaust relevant pagination before claiming absence. Resolve duplicate titles by stable ID, project, and purpose.

For authorized writes, use current create/save/update tools, preserve unrelated fields, and read back resulting IDs, contents, and relations. After an ambiguous result, query existing state before repeating a create. Connector absence permits a local draft or partial audit, not invented workspace facts or a claim that Linear was updated.

## Workflow ownership

Resolve installed paths from the current catalog. Read `cxc-dev` for development work and the matching surface owner when needed. Use `cxc-recall` for missing historical context; it does not establish current state.

An effective CXC Loop workflow loads `cxc-loop` and `cxc-pabcd` and follows their current goal, session, phase, and evidence requirements in the owning task. A plan or audit alone does not activate them. Delegated agents use the current CXC dispatch protocol and host-permitted tools/settings. Task creation, model configuration, and loop activation each need their own evidence.

Only one owner controls an operation. `crw-define` defines initiative intent, `crw-next` selects the next action, `crw-plan` decomposes agreed goals into projects and issues, `crw-run` supplies execution operations at the level the task is bound to, including initiative supervision through project parents, `crw-loop` owns explicitly requested parent goals and automatic repetition, `crw-status` reports the current situation and its schedule verdict without choosing an action or auditing criteria, `crw-check` compares delivery with intent, `crw-logic` investigates contradictions, and `crw-tidy` supplements records that fall short of the rules already agreed. A focused audit returns findings to its caller; it does not become another coordinator or recursively dispatch the caller.

`crw-run` owns goal-free execution of one project's agreed scope, including parallel
issue children, verification, integration and newly ready successors. A ready batch
is a scheduling unit; only an explicit narrower request limits delivery to that batch.
[crw-loop](../../crw-loop/SKILL.md) adds creation/restoration of the native parent goal
and automatic host continuation to the same Run execution and scope. Run alone does
not create a parent goal or promise future wake-ups. Run inside Loop returns to the
existing owner without another goal. Both reuse [Project parent binding](integrations.md#project-parent-binding).
Verified scoped deliveries establish progress; parent-local source changes and CXC
implementation phases are not completion conditions. Children keep their own CXC
lifecycle. Explicit parent workflow choices and existing CXC state require supported
transitions; `crw-loop` owns goal/hook preflight, activation and recovery rules.

### Completion follow-up in an existing execution workflow

Resolve scope from the full current assignment and prior approvals, not just the latest short message. Authorization survives skill switches. An authorized project-management or execution assignment includes its routine preparation, in-scope corrections, verification, and necessary progress/audit updates to the linked Linear coordination document. Record observed facts and accepted decisions without asking again; do not silently change requirements or issue completion criteria. A status question does not cancel an active run.

Use the current request together with the established assignment. Checking a managed worker's delivery is part of completing that assignment; a short request such as "this issue looks done" does not reset it to a standalone read-only audit. The coordinator sends in-scope corrections or requests for missing verification to the existing responsible task and rechecks the result without another permission round. `crw-run` owns that follow-up; bounded audit helpers return findings to the coordinator.

Explicit read-only, report-only, pause, no-contact, or narrower delivery limits win. An unrelated task link or a standalone audit does not establish execution authority. Existing authorization does not expand to new requirements, changed execution settings, release publication, deployment, issue closure, or messages to unrelated tasks. Reuse authorization that already covers those actions; ask only for the part that needs a new decision.

Recover the existing task first. A replacement is routine only when task creation/recovery is covered by the assignment and the host permits it, the earlier task is proven inactive, its work and receipts are preserved, and the replacement keeps the same scope and settings. Uncertain delivery or a read failure is not proof that no writer remains.

### Default independent execution

Write instructions sent to child tasks in English, including initial assignments,
review corrections, active-turn steer messages, resumes, and restoration blocks. The same applies
to a supervisor's instructions to a parent and to messages between peer parents.
Translate the actionable instructions without changing their scope or acceptance
criteria; preserve exact identifiers, URLs, paths, code, and necessary source quotes.
Keep task titles under the existing Korean title convention, and keep user-facing
reports and Linear records in Korean unless explicitly requested otherwise. This
language rule applies to future messages; it does not require resending old prompts
or waking existing tasks merely to change their language.

Unless the request chooses otherwise, an independent child task that `crw-run` creates or resumes runs the pair the role policy declares for the child role, with CXC Loop as its workflow, and owns its own host goal, goalplan, and FSM. That pair is read from the declared policy at the time of the call under [Execution settings by role](#execution-settings-by-role) rather than restated here, so one location cannot fall behind the other. Precedence, highest first: host and tool restrictions; the explicit limits in force for this request, such as plan-only, read-only, status-only, no-goal, no-FSM, no-create, or current-task; the user's explicit model, effort, or workflow choice for this scope; then this default. A later explicit instruction supersedes an earlier one only for the same constraint, so every limit it does not contradict stays in force. The result is the effective setting, and an effective Loop workflow carries the same weight as a separately requested one.

This default binds only `crw-run`'s independent children. The coordinator task, other products' global configuration, and CXC internal helper role routing keep their own settings.

Non-PR work without repository changes uses the working directory, source/result access and durable evidence defined in OPS-5.1 of the [Operations contract](../../crw-run/references/operations.md#ops-51-placement); it does not need Git or PR capability.

A new independent child is also created with enough capability to finish its delivery: file access for its checkout and evidence, git metadata access for its branch and commits, and the network access its push, pull request, and checks require. Broad local capability is the normal case. Worktrees, branches, scope, and recorded ownership separate concurrent work; the effective sandbox and permission profile define the enforced access boundary. The default covers a trusted implementation task inside the operating scope that creates it; an explicit narrower policy for the scope or the assignment governs over it, the effective profile is read back from the creation receipt, and the sandbox itself is never bypassed. Changing the default is a recorded decision rather than something a review performs. Apply the settings through the creation tool's real arguments and verify the returned profile. Tasks already running keep the settings they were created with; this is not authority to widen a live task or bypass a sandbox. See [Operations contract](../../crw-run/references/operations.md) for the owning rules.

The coordinator applies the effective settings through the creation tool's real arguments, sends the bounded issue packet in the initial work prompt, and verifies the returned settings. When CXC Loop is the effective workflow, that prompt invokes the installed `cxc-loop` skill; an explicit non-Loop or no-goal alternative omits that invocation and names the agreed workflow instead. Loop mechanics belong to the child and its `cxc-loop`/`cxc-pabcd` skills; see [Prepare and dispatch](../../crw-run/SKILL.md#prepare-and-dispatch).

- A status request reads existing tasks and evidence and wakes nothing.
- A setting the creation path cannot apply is settled before the task exists: use an already-permitted path or effective configuration that applies the requested values, or report the concrete unsupported capability. Never create a task already known to carry the wrong setting, and never silently downgrade it or claim the requested value.
- A mismatch observed after creation is reconciled on that same task.
- A user correction to model, effort, or workflow adjusts the same task where the transport supports it, and is reported otherwise, keeping stable IDs, unchanged permissions, and preserved progress, reconciled before any resend.
- The effective workflow is restated in every later send to that task, not only in the first one. A transport carries model and effort as settings it can check and has no field for the workflow, so a correction or a resume that omits it drops the one setting nothing else restores. Long work, a compaction, and a mid-work instruction each put distance between the original prompt and the task acting on it, and the restatement is what closes that distance. [Task packet](../../crw-run/references/task-packet.md#restoration-block) holds what travels with it.

### Publish for review when the work is reviewable

This applies where the assignment's scope expressly covers publication. Where it does not, the delivery is local commits or a frozen diff and the question of draft never arises; lacking publication authorization is a reason not to publish, not a reason to publish as a draft.

Where publication IS in scope, draft marks an implementation not yet worth reading. It is not a waiting room until reviewers finish. When the change is complete enough to review, it is published as a pull request that is open for review: created non-draft, or an existing draft transitioned to Ready for review. Ready is review entry, not merge permission and not proof the work is done.

Each step below is a separate recorded fact, in this order: implement with the local validation the change actually needs; open the PR non-draft or transition the existing draft to Ready for review; request the review the repository requires and the assignment's authorization covers, and confirm it actually started, because a request that never started is not a review; reproduce, fix, reply to and resolve findings, refreshing only the review evidence invalidated by a changed head, under the [active reviewer policy](../../crw-run/references/merge-readiness.md#disabled-reviewer-policy); report `ready_for_parent_review` only once the relevant review, check, and finding gates are met. The coordinator then compares the issue's criteria against the current diff, base, head, checks, and reviews, and may merge under existing authorization. Release and deployment need Jun's approval.

An optional reviewer that cannot start, stalls, or sits outside the authorized scope does not become an indefinite wait: use the fallback in [Merge readiness](../../crw-run/references/merge-readiness.md), record the gap, and continue. A required review gate is not waivable that way.

Review findings, pending CI, and ordinary revision pushes never send a pull request back to draft. Re-draft only when the implementation itself stops being reviewable.

Two facts are easy to collapse and are recorded separately: a relay receipt whose outcome is `ready_for_review` says the child emitted a reviewable revision, and GitHub `isDraft=false` says the pull request is open for review. Neither implies the other.

Where publication is in scope but the child cannot execute it, the coordinator performs only that blocked action, from the child's verified artifact, and records the actual resulting state. The child keeps review and fix ownership. That is the exception for a capability-limited task, not a standing parent obligation for children that can publish, and not a way to supply authorization the assignment never had: a coordinator cannot publish on behalf of an assignment whose scope excludes publication.

Explicit draft-only, read-only, or no-remote-write limits win, as do the repository's own requirements. None of this adds an approval prompt to a workflow the user already authorized.

### Default dev integration

Jun authorizes a pull request workflow in which the implementation child carries the work to a reviewable pull request and the coordinator decides the merge. The child implements, tests, commits on its branch, pushes, opens the pull request, and then owns every applicable review on that same pull request: intake, triage, fixes, replies, and rechecks. It reports normal completion only once the required checks and reviews on the current head have finished and blocking findings are resolved, with per-finding evidence. A missing mandatory review or check is reported as blocked rather than as completion. The full contract, including the fallback for a task that cannot write git metadata, is [Operations contract](../../crw-run/references/operations.md).

The coordinator then checks the Linear criteria and the pull request's latest diff, base, head, checks, and review resolution, and merges without another confirmation round when those hold. This is standing user authorization for this workflow, not permission inferred from passing checks, and it supersedes the earlier recommendation that the coordinator avoid merging. Use [Merge readiness](../../crw-run/references/merge-readiness.md) for the gate detail, preserve unrelated work and branch protections, resolve routine in-scope failures and recheck, then verify the actual landing rather than an accepted merge request. An explicit diff-only, no-merge, or narrower instruction still overrides this default, and the child never merges.

Release and deployment are not covered and still require the user. Where merging a branch is known to trigger a release or a deployment, obtain that approval before merging, since the branch name alone does not carry it. A repository requirement that genuinely needs a new decision remains a blocker for that action.

### Whether a relay holds this assignment

Every relay rule in this workflow is written as a condition — "where a relay holds the assignment" — and for a long time nothing said how that is decided. An undecided condition does not read as false; it reads as nothing, so the question was never asked and execution stayed on direct send and steer by default. There are two answers here and no third. Managed start and managed resume decide which one applies, and record the decision.

A relay holds this assignment when all three hold together: the packet's relay block names a shared state directory this process actually resolved; the issue lookup in that directory returns a responsible relationship; and the reading came from the store this process measured rather than some other file at the same path. One command answers all three, because the order between them used to be the hazard rather than the answer: `codex-session-relay --state "$RELAY_STATE" doctor --issue <the exact issue identity>` reports `issue.holds`, the responsible child and relationship, and `issue.storeAgreement`. Act on it only where that agreement reads `same`. Before registration has landed the assignment is still relay-managed if the state directory is agreed and the declared intent names that store; `assignment-find --issue` carries the same provenance under `relay.store` for the same comparison.

Treat a lookup that cannot name its own store as no answer at all. Run the lookup first against a mistyped state directory and it creates an empty store, then truthfully reports that nothing is assigned — and a coordinator that believes it opens a second writer for an issue that already has an owner. That is why the answer carries the store it came from and why a disagreeing store is refused rather than reported beside a usable result.

Where the determination says no relay holds it, that is a decision and it is recorded with its reason: the state directory is not writable, no socket is configured or reachable, or this assignment is deliberately direct. Record it in the same coordination record that holds the rest of the assignment, with the mode, the resolved state directory and socket, the store identity the reading reported, the delivery owner, and what availability was actually measured rather than assumed. A resume reads that record back before acting, so the mode survives the coordinator's own context loss.

Three things this must never become. An unavailable relay is never recorded as deliverable, because a receipt nobody can deliver is not progress that a parent may claim. An unavailable relay is never a silent fallback either: direct is chosen and written down, never arrived at by a command that failed quietly. And direct send and steer remain a transport and nothing more — a delivered instruction is not a receipt, an acknowledgement or a verdict, so a direct send is never counted as relay completion evidence, and the same logical instruction never travels both routes at once.

Ordinary conversation is not managed transport. What the relay carries is the assignment's own traffic: the completion receipt, the acknowledgement, the verdict, and the revision request a needs-changes verdict queues. A question to a peer parent, a status answer, a clarification are peer conversation and stay on the ordinary path; forcing them through an assignment event would file conversation as delivery and make the record useless for the thing it exists to prove.

Relay resume and native-goal continuation are different mechanisms and take different proof, and the word "resume" covers both, which is how they get confused. A relay resume is about an assignment: undelivered messages, an unsettled attempt, a generation waiting on its anchor, all recovered from the store. A native goal continuing is about a task deciding to keep working across turns. Neither establishes the other. A recovered assignment says nothing about whether the coordinator's goal is still active, and an active goal is not evidence that a queued correction ever reached the child. Record and prove them separately, and name which one a report is about.

Existing work is not migrated by this. A project already running direct keeps its records, its owners and its pull requests, and switches only at a boundary where switching is safe and explicit. Reporting that current execution is still direct is part of the record, not something the determination hides.

### Durable cross-task delivery

Where an installed same-host relay holds the assignment, use it for the receipt, verification, correction, and coordination-summary path instead of improvising per-task bookkeeping. It records the relationship, the execution generation, each delivery attempt, the parent's acknowledgement and verdict, and the summary owed to the canonical Linear document. Without a relay none of this applies and the rules above stand unchanged. The commands and their arguments are in [codex-session-relay](../../crw-run/references/relay.md).

Every process in one assignment must point at the same state directory, or they simply do not see each other. Which commands a given task can run is a capability of that task's profile on that host, discovered from the relay's own environment check rather than assumed: only the commands that reach the App Server need a socket, and everything else works from the store alone. Where a task cannot reach the socket, it uses the store-only subset and a host-capable process owns delivery. On the host and profile measured during JUN-92 a workspace-write task could not write the default state directory or connect to the control socket; reuse that as a recorded observation, not as a universal rule. Offline, a child's readiness claim is staged: it is real recorded progress, it is not delivery, and a report must not call it one.

Ask the relay who is responsible before creating anything. An assignment lookup by issue returns the existing relationship, its child, its current state, and the next expected action. Registering a different child for an issue that already has an active or paused assignment is refused transactionally, so duplicate registered ownership cannot be created; that is a guarantee about the record, not about the host, since a task created outside the relay is still a real task. A pause does not release an issue.

Record BOTH the parent's and the child's authorized execution settings from the actual creation receipts the coordinator already holds, and register the issue's criteria as the canonical set so a later verdict rules on agreed obligations. Both reuse results already in hand; neither is a handshake, and neither needs an extra turn or further approval. Never ask a worker to echo its own settings back, and never widen a task's permissions to make a later send connect.

Verify the revision the relay reports as current. A verdict names the criteria set it was reviewed against, so editing a criterion after the review invalidates that review rather than passing it, and the review is then claimed again and decided against the set now in force on that same event; a fresh execution generation is for when the artifact itself has to change or the event is no longer the current revision. An integration record names the exact revision it integrated. The coordination summary is a separate concern from execution and is the COORDINATOR'S OWN connector write: the relay holds no credential and has no Linear transport, so it queues the summary and the coordinator executes it, conditionally replacing its owned container rather than appending, then reads the document back and confirms against that job's structured record. A failed summary write is retried through its existing outbox job; this retry does not re-run verification or re-send a correction. OPS-8.3 in [Operations contract](../../crw-run/references/operations.md) records the installed-module tests for this narrow behavior. It is not evidence for the proposed multi-parent fairness, per-parent limits or general delivery-error isolation.

### Installation and operations have one owner

Dependency identity, installation ownership, the shared relay service and its durable store, service lifecycle, workspace assignment, assignment routing identity, the parent's continuation and waiting mode, and the review location of each dependency repository are all defined in one place: [Operations contract](../../crw-run/references/operations.md). Read it before installing, updating, or operating that runtime, and before assigning a checkout. Do not restate its versions, paths, or rules here or in a script, because a second copy of a version or a state directory is the copy that goes stale first.

Two consequences reach every operation in this reference. One relay service and one durable store serve a whole operating scope, meaning one host, one OS user, and one App Server, shared by parents across repositories and Linear projects, so no operation creates a service or a store of its own and no parent shuts down a service other parents are using. And each assignment routes on its bound identifiers rather than on a display name, a branch, or a working directory, so results, corrections, and permissions never cross between parents.

### Review evidence is independent of the provider

Apply the [disabled reviewer policy](../../crw-run/references/merge-readiness.md#disabled-reviewer-policy) to new assignments and existing task recovery before deciding which reviews to request or await.

Use the target repository's actual review configuration and observed results. Public/private visibility alone does not determine which reviewers run or what they can access. No named bot, vendor, or repository-management app is a universal dependency; a planned integration is not proof of an operational reviewer. Requirements come from the repository policy, enforced rules, and the assignment.

Judge review coverage, revisions, completion, and finding disposition rather than a tool's name or green badge. Reuse sufficient independent evidence, including an authorized local review where policy permits it. Optional integrations do not create an indefinite wait or a new approval round; required checks and formal approvals still apply. Keep credentials, private-source access, and any new paid usage within the existing scope.

## Paperthin supplies focused checks

Read the selected installed `SKILL.md` and follow its workflow. Load only skills that answer a concrete question in the operation.

| Situation | Skill and use |
|---|---|
| Bundled or ambiguous instruction | `readchk`: resolve intended scope before spending work |
| Human lost the product context | `catchup`: brief from refreshed state |
| Conflicting copies of a requirement/status | `ssotize`, audit mode: map sources and disagreement |
| Acceptance test or metric validates itself | `mandela`: find missing independent evidence |
| Factual premise needs external verification | `factchk`, with `cxc-search` for public/current lookup |
| Packet/report must stand alone | `shower`: fresh-context cold read when justified and delegation is available |
| Revised document accumulated noise | `re0`: refresh only the authorized artifact |
| Where to start or what follows finished work | [crw-next](../../crw-next/SKILL.md): gather scoped state, use `readchk` for ambiguity and `nba` for one next action |

`hate`, `prism`, `feynman`, and other skills marked `disable-model-invocation` or an equivalent explicit-only policy remain deliberate user choices. The user's current operative request must name the skill or explicitly authorize that named chain. A wrapper selection, quoted example, pasted log, or skill document mentioning it is not opt-in. Preserve the selected skill's output and independence rules.

Read-only scope applies to helpers: `factchk`, `re0`, or `ssotize` findings remain proposals when edits are outside the request. Do not use a helper's broader capabilities to expand scope. Missing helpers produce a disclosed limitation, not a claimed run.

## Evidence and handoff

Pass Linear IDs/links, embedded criteria, repository/revision identity, accepted decision sources, and known gaps. Keep requested, observed, and unverified state separate. Reuse proof only for the same revision, criteria, and environment.

Keep raw launch receipts and sensitive test evidence in established private locations; put only the necessary coordination summary in the canonical Linear document. If record-writing is outside the request or access is unavailable, return an unsynced update for the owner. Installed skills hold procedures, never project state or credentials.

In final reports, mention checks that changed the conclusion and meaningful unavailable evidence. Avoid a ceremonial list of every skill.

### Delivery reach and current usability

A delivery report answers two questions no status word answers: how far this change actually got, and what the reader can use right now. Report the reach as an ordered chain — the source revision, the local commit, the remote branch, the installed link or version, and an observed run of the changed capability — and carry only the links this delivery actually involves. A delivery with no repository target uses the same shape over its own artifacts: the input baseline, the delivered output revision or digest, and the verification actually observed. Where one report covers several independently delivered artifacts or repositories, each gets its own chain, because one artifact's gap says nothing about another's.

Choose the stages from what happened rather than from the chain's full length. Walk the chain in order, naming each stage this delivery has evidence for and, where one exists, the first stage that is missing or was never observed, saying which of the two it is. A delivery with no gap has no such stage to name. The stages are independent: never infer a later one from an earlier one, and never drop an observed later stage because an earlier one is absent, since a commit that was never pushed can still be live through a link resolving to that checkout. A stage this change cannot have, such as an installation surface it never touches, is left out rather than reported as passing, and a stage the issue's own criteria require is always named, as unverified when nothing was observed. A report that lists every stage every time trains its reader to skip the one that matters.

Changing source, installing it, and refreshing an already-loaded conversation are three events, and the installation method decides what the third one costs. A linked installation resolves each skill through a symlink, so an edit is visible to the next read of that file, no reinstall is involved, and a conversation that already read the old text keeps it until the file is read again or a new task starts. A versioned plugin installation resolves a cached copy of a published version, so an edit reaches nobody until the version is bumped and installed again, and a session already running keeps the package it started with. Report the method actually observed and what the reader must do under it; where it was not checked, say so instead of assuming a linked installation. Reading a link's own target is what establishes which checkout an installed skill resolves to.

Usable now is a claim about a representative user path, run through the installed entry point, with the time and environment of the most recent such run attached. Passing checks, a listed hook, an accepted delivery and a completed issue each establish only themselves. [OPS-6.1](../../crw-run/references/operations.md#ops-61-six-states-that-never-imply-one-another) already records which states never imply one another, and [OPS-11.3](../../crw-run/references/operations.md#ops-113-four-stages-that-are-not-one-event) separates a landed source change from what a host installs and executes. Use those meanings rather than restating them here.

Where the run happened in a test store or a scratch path, or where an always-on process is currently stopped, the demonstration and the operating state are separate lines: what was demonstrated, in which environment, at what time; and whether the real path is serving now. Report readiness from what was observed, since a service seen stopped is not ready and that is a result rather than a gap, and reserve unverified for the part nobody read, naming the observation that would settle it. That unverified is this report's own conclusion about evidence it lacks, not the field value an installation check carries; [OPS-6.2](../../crw-run/references/operations.md#ops-62-record-shape) owns those values and the rule for a measurement time nobody made.

Close with two short lines — whether the reader has to do anything, and the shortest next step to use or verify the change — and answer any question about installation, activation or availability from evidence already held rather than by going to get it, because a reinstall, a service start and a new task each keep their own authorization.
