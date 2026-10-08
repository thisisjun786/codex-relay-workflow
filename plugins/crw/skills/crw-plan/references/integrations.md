# Linear and CXC integration

Shared guidance and Jun's workflow defaults for `crw-define`, `crw-next`, `crw-plan`, `crw-run`, `crw-loop`, `crw-status`, `crw-check`, `crw-logic`, `crw-tidy`, and `crw-refactor`. Read the operation-specific skill for scope. Apply these defaults within the user's assignment and current host permissions.

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
title alone. A rename or changed initiative relation does not change a binding, and a title is not
read the other way either. The management title carries its project's product-family label by the
rule in [Set the app presentation and record](#set-the-app-presentation-and-record), so the label
is derived from the binding and is never evidence of one: a task is not bound to a project because
its title names that project's family, and a title carrying a different family is a presentation
gap to report rather than a binding to correct. An initiative is
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

Use a concise project summary as the management task title, led by the linked
project's product-family label in brackets, as in `[CRW] 설치형 플러그인 전환`: the
short outcome the project exists for, named so the title also reads as the task
that manages it. Write the label the project actually carries, exactly as Linear
spells it, without expanding, translating, abbreviating or changing its case, and
without a table of families kept beside the workspace's own. Initiative membership
is still not a prefix, and no initiative or multiple initiatives needs no
title-choice question. Preserve an explicit user title: the user's wording stays as
the body and takes the prefix in front of it, while an explicit fixed title, a
no-rename or a read-only constraint leaves the title as it is. Resolve same-name
projects by stable ID under the shared target rules before binding. If their
management titles would be indistinguishable, append a short project-ID suffix. Do
not rename the initiative or project itself, whose own name needs no product
prefix. This convention names the Codex management task; an issue child follows
[Child task titles](../../crw-run/references/task-packet.md#child-task-titles) and a
supervision task keeps its own name.

Take the family from the project's product-family label group. The project read
returns a flat list of label names without the group each belongs to, so the value
carries its own provenance — the label group where a surface shows it, or the family
already recorded in this binding with its source — and a list of labels alone is not
that evidence, because a count is not membership. Exactly one such label is the
family. None is a legitimate classification rather than an error, and more than one
is a question; in both cases leave the title unprefixed, record what is missing as
part of the unverified state below, and ask instead of choosing. Never read the
family from the repository, the working directory, the task's model or the Linear
team key, which often spells the same word and is a different field.

Apply the prefix once. A title already carrying this family keeps it — that is this
bracket alone, or this bracket separated from the body by a space, since a label may
itself contain a bracket and a longer one can begin the same way — and a family name
standing separably in front of the title is absorbed rather than repeated.
Otherwise the body is left exactly as it is: a label joined to what follows it is
part of the sentence, so the subject in `CRW를 설치형 …` and the issue code in
`CRW-137 · …` are text rather than prefixes. A leading bracket that is not this
family is neither overwritten nor stacked behind a second one; report it and take a
decision on that exact bracket, keeping it as body or replacing it, because an
obsolete family and a user's own words are indistinguishable from here.
`crw skill parent-title decide` settles these cases offline, and
`crw skill parent-title replay` holds it to recorded expectations; it proposes a title and writes
nothing. Any later surface that shows or edits this prefix reads the same label
through the same rule and the same helper rather than keeping a mapping of its own.

Discover the supported task rename and sidebar tools and their current schemas.
Apply changes only to the verified current task, and check its resulting title
and pin state in the app listing. If a capability is unavailable, complete the
supported parts and report the gap. Do not edit a session database or global
configuration to simulate a successful binding.

A rename that fails or is unsupported leaves the title unwritten: record the
requested title and the limitation, and carry on with the authorized work. A rename
that is accepted proves transport and nothing more. The title is verified when a
read by task ID returns the requested string; a read returning anything else is a
mismatch to report with both strings rather than a success with a caveat, and no
read at all is unread rather than a failed write.

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
- The task record read by task ID before and after a rename, showing the fields
  that read exposes unchanged. Whatever it does not expose is read from its owning
  surface or recorded unverified, rather than claimed preserved on its behalf.

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
relations change. Existing titles are presentation, whether they carry a
product-family prefix or an initiative one, and neither is a reason to rebind or
rename during recovery. A project whose family has changed does not have its prefix
corrected on sight, and existing parents are not renamed in bulk. Refresh the title
only within an authorized presentation change; do not migrate existing bindings
implicitly.

Load the existing owner for the requested operation:

| Request | Owner |
|---|---|
| Where to start or what to do next | [crw-next](../../crw-next/SKILL.md) |
| Define initiative intent or goal | [crw-define](../../crw-define/SKILL.md) |
| Plan, roadmap, milestones, or issue scope | [crw-plan](../../crw-plan/SKILL.md) |
| Execute the project, coordinate progress, or follow up on delivery | [crw-run](../../crw-run/SKILL.md) |
| Create/restore a project parent's native goal, which exists only where the user explicitly asked for one | [crw-loop](../../crw-loop/SKILL.md) |
| Execute an initiative's approved projects through their existing parents | [crw-run](../../crw-run/SKILL.md), entering at [Initiative supervision](../../crw-run/references/initiative-supervision.md) rather than at the project binding above |
| Compare delivery with accepted requirements | [crw-check](../../crw-check/SKILL.md) |
| Investigate contradictions or broken invariants | [crw-logic](../../crw-logic/SKILL.md) |
| Diagnose structural debt after a verified cycle and plan bounded repairs; execute only selected authorized work | [crw-refactor](../../crw-refactor/SKILL.md) |
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

Today's operation is one implementation issue per packet, and that packet delivers that one
issue: several deliveries for one issue, or one delivery covering several issues, are reconciled
through `crw-plan` before new dispatch. Split work requiring several deliveries into separate
issues with explicit dependencies, even within one repository. Keep a multi-repository
outcome in one project when appropriate, with one issue per repository delivery.
Batches coordinate separate issue/delivery pairs; they do not combine issues into one
delivery. Referencing a related issue is not claiming to deliver or close it. This is today's
operation under the [work-unit rules](../../../../../POLICY.md#work-units-review-and-integration),
not a standing one-to-one rule: several packets for one feature are allowed by policy and
switch on when their support lands.

Keep review fixes on the same issue and delivery. A necessary replacement PR retains
the superseded link and names the one current delivery; it does not create a
second simultaneous delivery for the issue. A new change after that delivery
has landed gets a new issue and delivery, except a defect inside the finished issue's own specification, criteria or promise: that reopens the same issue key ([After the merge](../../crw-run/references/merge-readiness.md#late-review-threads)), and only a defect outside the issue gets a new issue. Research, design, or operational work with
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

Jun's own words for the three levels are 감독 세션, 부모 세션 and 자식 세션, shortened to 감독, 부모
and 자식 once a report has introduced them. English instruction text calls them the supervisor, the
parent and the child, and the ids a relationship store and a host's role policy already record are
`supervisor`, `parent` and `child`
([OPS-7.4](../../crw-run/references/operations.md#ops-74-three-levels-and-their-routing-identity),
[Execution settings by role](#execution-settings-by-role)). One role in three renderings: the
Korean name is what a reader is shown, the English word is what an instruction says, and the id is
what a record keys on.

Writing the names down settles what they are called and nothing else. A display name in either
language keys nothing, so no relationship id, assignment, goal, worktree or task title is
regenerated because this section now spells them out, and each level keeps the title convention
that already fixes it: a verified project parent follows
[Set the app presentation and record](#set-the-app-presentation-and-record), an issue child follows
[Child task titles](../../crw-run/references/task-packet.md#child-task-titles), and a supervision
task keeps its own name. Read
earlier phrasing the same way rather than rewriting it: an initiative task, an initiative
management task or a 상위 관리 세션 is the supervisor under another description. That is how to read
those words, not a licence to substitute them wherever they appear, and the two things that merely
share the word stay excluded exactly as above. A role word is not a designation either: a request,
a prompt, a title or a folder saying 감독 or supervisor designates nobody, so what a task is stays
something read from its binding rather than from what anyone called it.

| Role | Bound to | Coordinates | Instructs |
|---|---|---|---|
| Supervisor | one initiative ID | its initiative's projects: cross-project dependencies, priority, shared resources, and the order in which projects reach a shared target | the parents of those projects |
| Parent | one project ID | its project's issues: dependencies, sequencing, parallel children, delivery verification and integration; and, directly with a peer parent, the surface their projects share | its own children only; a peer parent is asked, never instructed |
| Child | one issue ID | nothing outside its own issue | its own internal helpers |

| Role | Verifies | Merges | Updates in Linear | Complete when |
|---|---|---|---|---|
| Supervisor | each parent's reported project outcome against the initiative's finish condition | nothing; it decides cross-project order, never a landing ([OPS-9.3](../../crw-run/references/operations.md#ops-93-the-parent-integrates-and-does-not-release)) | the initiative record | the initiative's finish condition holds on its projects' verified outcomes |
| Parent | each child's delivery, pull request, checks and review against the issue's accepted criteria | its own project's issues, into their intended target | the project record and the issues it owns | every obligation in the agreed project scope is delivered, integrated and reconciled |
| Child | its own implementation and the local verification of its task branch | never ([OPS-9.3](../../crw-run/references/operations.md#ops-93-the-parent-integrates-and-does-not-release)) | nothing; it returns proposed record changes to its parent | the local verification record of the head passes and its blocking findings are resolved ([OPS-9.2](../../crw-run/references/operations.md#ops-92-what-normal-completion-means)); the issue itself is Done once its parent lands that head on the intended branch, under [Implementation Done](#implementation-done) |

The tables say what each level answers for. What the supervisor spends its time on is the level
above them: it talks with Jun, carries the requests he approves down to the parents, decides which
parent takes which project, answers the midpoint check when he asks for one, collects what the
parents report, and writes the initiative's record. The work it never takes is divided by name
rather than left to the pair below it. The child implements, verifies and resolves the review on
its own pull request; the parent accepts that delivery against the issue's criteria and performs
the merge; the supervisor tests each reported project outcome against the initiative's finish
condition and stops there. Holding no goal of its own narrows none of that: inside an execution
approval still in force it moves the approved work and resumes the responsible parent, which is
what [the midpoint check](../../crw-status/references/midpoint-check.md) already describes.

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
The same boundary holds for every other sub-task an owner opens and for every route a write could
take; what a returned proposal carries, and what the owner does with it, are in
[record writes and returned proposals](#record-writes-and-returned-proposals).

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
expansion of authority nor a new goal ([OPS-7.3](../../crw-run/references/operations.md#ops-73-isolation-between-parents)).
The role decides what goal a task opens, never the other way round. A supervisor opens none and
runs no automatic loop, and a child keeps its own goal and its implementation loop. What a project
parent opens is not this section's fact to hold: it is the decision
[Start policy](../../crw-run/references/start-policy.md#roles-and-the-goal-each-one-opens) records,
that record has already changed once, and it is read there rather than remembered. A coordination
goal that does exist is its own and is never mixed with a child's implementation FSM, and an absent
goal says what a task is waiting on rather than which role it is.

This contract fixes the roles; it does not establish how a supervisor is instantiated. A task
becomes a supervisor only by an explicit designation naming the initiative, and an initiative link
by itself rebinds no existing parent. That designation, the approved project set it fixes, the
completion boundary and the reuse of the parents already running are in
[Initiative supervision](../../crw-run/references/initiative-supervision.md).
Start policy, creation authority and the limits that survive them stay where they
already are in [crw-run](../../crw-run/SKILL.md#independent-implementation-tasks), and this section
references them rather than keeping a second copy.


#### Execution settings by role

Each level runs on the model and reasoning effort decided for its role, and which pair belongs to
which role is a recorded product decision rather than something a task infers. Two roles may be
decided onto the same pair; each is still read and checked as its own role. The supervisor's
model is Jun's own selection and is never propagated, changed by automation, or copied to the
level below it; the parent's and the child's come from the role policy, which may declare more than one
pair for a role.

| Role | Model and effort | Who decides |
| --- | --- | --- |
| Supervisor | its own recorded setting | Jun, directly |
| Parent | the role policy's parent pair | this policy |
| Child | one of the pairs the role policy declares for the child role, chosen per issue | this policy for which pairs exist; [Child pair by issue type](#child-pair-by-issue-type) for which issue gets which |

Read the pair rather than remember it. The host's execution policy declares the pair of each role, or
several pairs for a role, and `get_capabilities` reports what it declares, so a creation states a pair
that policy reports for the role it is creating (for a role that lists several, one entry of its
`pairs`) and passes the role itself so the host checks the answer. Never carry a pair from memory, from
another project, or from a document — including this one, whose names for the child's two pairs, Sonnet
and SOL, say which family a pair belongs to and carry no model or effort. That habit is
what the failure was made of: a coordinator created a project parent on a model it remembered,
and another retried a withheld send by changing the model and keeping the previous effort.
Reasoning-effort names are catalog values belonging to their own model; two that look similar are
not interchangeable, and nothing maps one onto another.

A role that lists several pairs ranks none of them. The policy says which pairs a task may run on and
not which one it does run on; for the child role the issue decides, under
[Child pair by issue type](#child-pair-by-issue-type).

A role the host's policy does not declare, or a pair it does not list for the role, is a blocker to
report, and its recovery is declaring it in that host's execution policy. There is no fallback pair,
because a fallback is a default and a default is the second source of truth this arrangement exists to
remove.

Changing an existing task's model is a user action plus a re-record, never something a message
performs. After the user changes it, the authorization recorded for that task is re-recorded from
a user-attributed source before the next send, because the record is what a send verifies against
and a value observed on the host is evidence of what the task is running rather than a new
approval. Until then the send is refused naming the record, and a task the host reports as not
loaded is left alone rather than resumed under a pair that was never checked against its role.

#### Child pair by issue type

Children run on one of two pairs, and the issue decides which. The pairs are named after the family of
their model: the Sonnet pair is the declared pair whose model is a Claude Sonnet model, and the SOL pair
is the one whose model is a GPT SOL model. This section carries no model name and no effort, because a
document that carries one is a second source of truth beside the host's policy (see
[Execution settings by role](#execution-settings-by-role)); each pair's model and effort are read from
the declared policy when the child is created.

The axis is where the answer comes from. When a reference implementation, a specification, a golden
output or an existing pattern decides what the right result is, the child's work is to meet it exactly
and to keep every outside rule that goes with it: that is the Sonnet pair. When nothing outside the
issue says what is right, because the answer has to be designed or found, it is the SOL pair.

| Shape of the issue | Pair | Example |
| --- | --- | --- |
| A port or a change of language that has an original, where "same as the original" ends the work | Sonnet | moving a module to another language and checking it against the original's recorded outputs |
| A fix that keeps golden, contract or byte compatibility; names, locations and wording; deletions and moves; wiring or registration in many places | Sonnet | renaming a command across its documentation, specs and tests; registering a subcommand in every list that names it |
| Writing (documents, skills, prompts, reports); UI or images judged by eye; operations where the order of steps is the point; an investigation that opens no PR | Sonnet | rewriting a skill's procedure; an installation runbook; a read-only audit of a record |
| A new mechanism, algorithm or state machine | SOL | a rule that decides which work is ready to start; a retry state machine |
| Performance or parallelism that a measurement ends | SOL | cutting a test suite's wall time by running its parts at once |
| A bug of unknown cause; a fix that has already failed twice or more; flakes, races, concurrency, cache consistency | SOL | a failure that appears only under load |
| An implementation that has to decide the structure of a new module | SOL | a new package whose interfaces nobody has drawn |

When choosing, look at four things. Where the answer comes from: something outside the issue that the
result can be compared with, or the child's own design. How many outside rules the work must keep: the
more a child has to follow exactly (a golden, a schema, a naming convention, a long procedure), the more
the issue belongs to the Sonnet pair. The shape of the end: "same as the original" ends a Sonnet issue,
and "a measurement improved, the cause is gone, a new invariant is proven" ends a SOL issue. And the
failure history: a fix that failed twice is an answer still to be found, whatever the issue first
looked like.

Two cases the shape does not settle:

- **A security judgment at the core.** An issue whose core is a security judgment (deletion protection,
  a permission or sandbox decision, secret detection) goes to the Sonnet pair from the start, whatever
  its shape. A SOL turn on such work was refused by the provider's cyber policy, and the issue should
  not meet that refusal in the middle of its delivery.
- **Half and half.** A clear case follows the table. An ambiguous case goes to the pair with fewer
  working children in this parent; a plan written before any child runs counts instead the issues of
  the project that already carry a line, per pair. Once one pair holds more than 60 percent of them,
  border cases go to the other pair whatever the count of working children says. The line then names
  the axis half-half.

**Precedence**, highest first: the user's explicit choice of model, effort or pair for this scope; the
pair the issue body's child pair line records; this table. Host and tool restrictions and the explicit
limits in force keep the place [Default independent execution](#default-independent-execution) gives
them above all three. A choice the user makes for an issue is written onto its line with the reason, so
the record and the creation agree.

**The line.** An implementation issue carries one line, beside its size statement and under none of the
headings the [size check](issue-boundaries.md#check-the-size-of-an-issue) reads, so the line is
neither counted as a criterion nor read as a deliverable:

    Child pair: SOL (answer must be found) - a new retry state machine

The form is `Child pair: <Sonnet or SOL> (<axis>) - <the issue's type and the reason, in a few words>`.
The axis is exactly one of `reference exists`, `answer must be found` and `half-half`. A Korean issue
body may write the label as `자식 pair`; the pair name and the axis words stay as written here, so a
reader or a check can match them. The line records a family, never a model or an effort. An issue with
no line is routed by the table by the parent that dispatches it: the parent writes the answer as the
line on the issue and reads it back before it prepares the packet or creates the child, so a later
parent reading the issue finds the selection there. A note in the parent's own coordination record is
only a pending proposal; where the issue cannot be written, the parent reports that blocker and creates
nothing. The Launch packet that [crw-run](../../crw-run/SKILL.md#prepare-and-dispatch) keeps for an
issue with no recorded pair is the form of an issue this rule has not reached yet, and it is no default
pair for an issue without a line.
The line may also carry the issue's classification, its bundle and the source of the choice, in the form
given under [The extended line](#the-extended-line); a line without them stays valid.

**The packet.** The first packet of a SOL issue takes the form of the "SOL packet" section of
[task-packet.md](../../crw-run/references/task-packet.md); a Sonnet issue's takes the Launch packet.
The parent takes the form from the recorded pair and does not choose it again from the issue text at
dispatch.

**Creating the child.** Read `get_capabilities` and state the entry of the child role's `pairs` (or the
single pair of a role that declares one) that belongs to the line's family, passing `role` so the host
checks the answer. Where the declared list holds exactly one such entry, create with it. Where it holds
none, the parent picks no other pair. That is what a SOL line meets while the policy still declares only
the Sonnet pair, and what any line meets when a model change leaves its family with no declared pair;
the family is the test and not the number of pairs, so a policy whose only child pair is the SOL pair
stops a Sonnet line in the same way. Where the list holds two, nothing in the policy ranks them, so the
parent does not choose between them. Either way the creation is a reported blocker, and the recovery is
the host declaring the pair or the user choosing a pair for the issue: the parent states exactly the
entry the user names, which must be one the declared list holds, and writes the choice onto the line
with its reason; the line still records the family. Under a policy that declares only the Sonnet pair, a
SOL-line issue therefore waits for that choice, because there is no fallback pair.

The host refuses only a pair outside the role (`execution_role_mismatch`). A refusal before any call
sends nothing, and the same request id is used again with the corrected pair (see
[Bridge launch and recovery](../../crw-run/references/bridge.md)). Both pairs are valid for the child
role, so the host cannot see that a pair differs from the line, and that comparison belongs to the
parent: it issues no creation whose pair is of another family than the line.

**After creation.** Read the model, reasoning effort and role back from the creation receipt: they are
the pair that was stated, and they belong to the line's family. The relay records the receipt's pair as
the child's authorization and accepts any pair of the role
([codex-session-relay](../../crw-run/references/relay.md)), so this read-back is where a difference is
caught. A receipt of the other family is a mismatch observed after creation, and the child does not go
on under a pair its line does not name. It ends in one of three ways: the user changes the pair and the
authorization is re-recorded from a user-attributed source; or the user accepts the pair the child came
up on and the parent writes it onto the line with the reason; or, where the recovery rules allow a
replacement, the child is replaced on the right pair. A resend never changes a running task's model.
The receipt shows what was requested and applied; the served model stays unproven where no
served-model evidence exists.

**When SOL fails.** A SOL creation or turn that fails on a provider limit or a policy refusal is not
repeated. The parent releases the issue on the Sonnet pair and records why: it rewrites the line as
`Child pair: Sonnet` with the cause and the date, then creates the child on the declared Sonnet pair. A
refusal before any call leaves no task, so that creation is made under the same request id with the
corrected pair. A failure after the call may have left a task or a ledger row: the earlier task is first
proven inactive and its work and receipts kept, and the new child is a replacement with a new id under
the recovery rules of [Bridge launch and recovery](../../crw-run/references/bridge.md). That
replacement moves the pair, which is the one change of settings
[Completion follow-up in an existing execution workflow](#completion-follow-up-in-an-existing-execution-workflow)
allows.

##### Classify the issue by its kind of work

The shape table names shapes; the classification names the features that make an issue one shape or
another, so that two planners reading one issue write the same tags and a later review can say which tag
was wrong. Every tag is a feature visible when the issue is planned: it needs no result, no run and no
child. The tags and their values are fixed English words, written the same in an issue of any language, so
a reader or a check can match them. No rule here names a programming language, a product or a repository;
examples stand only in the last column.

| Tag | Value | Read as | Example |
| --- | --- | --- | --- |
| `answer` | `reference` | something outside the issue decides what is right: an original, a specification, a golden output, a contract, a naming convention, an existing pattern, or a procedure whose order is the point | moving a module to another language and checking it against the original's outputs; an installation runbook |
| `answer` | `found` | nothing outside the issue decides; the child designs the answer or has to find it | a retry state machine; a failure of unknown cause |
| `answer` | `mixed` | one part has an outside reference and another part has none, and the [boundary rules](issue-boundaries.md) do not split them | a port that also replaces its slowest part with a new mechanism |
| `output` | `code` | a change to code or its tests; documents that only accompany it do not change this | a new subcommand with its tests |
| `output` | `writing` | documents, skills, prompts, reports | rewriting a skill's procedure |
| `output` | `screen` | screens, layouts, images, design | a settings page judged by eye |
| `output` | `data` | transformation or reconciliation of recorded data | matching two ledgers row by row |
| `output` | `investigation` | opens no pull request: a finding, a measurement or an observed state, including an operational check | a read-only audit of a record |
| `procedure` | `goal` | one goal and the check that ends it, both stated up front: a machine check, or a reading against stated criteria | cutting a suite's wall time to a target; rewriting a page until stated criteria hold |
| `procedure` | `steps` | the work is to carry out a long procedure that the issue gives, where the order or the exactness of its steps is the point; writing a procedure is not this | a migration with a fixed order of operations |
| `procedure` | `open` | exploratory: no end is stated up front and the next step depends on what the last one showed | finding why a failure appears only under load |
| `reach` | `one`, `several` | the number of modules the work touches | a fix inside one module; a rename across a command's documentation, specs and tests |
| `check` | `machine`, `read`, `look` | how the result is verified: a machine check (tests, a build, a measurement, a comparison), reading and judging, or looking at screens or images | a test suite; a read-through against a checklist; a look at rendered pages |
| `history` | `fresh`, `retried` | `retried` when an earlier fix of this same problem had already failed twice or more when the issue was planned | a flake that two fixes did not remove |
| `core` | `plain`, `security` | `security` when the core of the work is a security judgment: deletion protection, a permission or sandbox decision, secret detection | a rule that decides what a sandbox may be asked to do |

Write the seven tags when the issue is written or refined, from what the issue states; where its text does
not settle a tag, amend the issue first and do not guess. Each tag takes one value: an issue that has both
kinds of answer is `mixed`, and every other tag takes the value of the work that ends the issue. Where two
values of `procedure` fit, take the one the issue's stated end names: `goal` when a check is stated up
front, `open` when none is, and `steps` only when following a given procedure is the work. `reach`
and `check` are recorded although no row below reads them yet, because a later change to the table can
cite only the features that were written down when the issue was planned.

##### Bundles

The tags map to three bundles: **Sonnet fixed** (the issue runs on the Sonnet pair), **SOL fixed** (the SOL
pair) and **flexible** (either works, and the count rule below picks). Read the rows in order; the first
that applies decides.

| Order | When the tags read | Bundle | Axis on the line | It restates |
| --- | --- | --- | --- | --- |
| 1 | `core=security` | Sonnet fixed | `reference exists` for `answer=reference`, `answer must be found` for `found` or `mixed` | the security rule |
| 2 | `history=retried` | SOL fixed | `answer must be found` | the failure history: a fix that failed twice is an answer still to be found |
| 3 | `answer=reference` | Sonnet fixed | `reference exists` | the shapes of the first three rows wherever something outside the issue decides |
| 4 | `answer=found`, `output=code`, `procedure=goal` or `open` | SOL fixed | `answer must be found` | the shapes of a new mechanism, a measured result, an unknown cause and a new structure |
| 5 | anything else: `answer=mixed`; `answer=found` with an `output` other than `code`; `answer=found`, `output=code`, `procedure=steps` | flexible | `half-half` | the ambiguous case |

Row 5 is an extension that reads the ambiguous case of the half-and-half rule. The shape table's third row
sends writing, screens, operations and investigations to Sonnet by shape, while the axis sends an answer
that nobody outside the issue decides to SOL; for such an issue the two disagree. The row's text is
unchanged: its examples have an answer from outside, which row 3 sends to Sonnet. Row 5 also holds a
disagreement one level down: an unknown-cause failure that comes with a fixed order of investigation steps
is SOL by its answer and Sonnet by its long procedure, while the SOL shapes name only `goal` and `open`.
The 180 combinations of `answer`, `output`, `procedure`, `history` and `core` (3 x 5 x 3 x 2 x 2) are
each caught by exactly one row.

**A flexible issue's pair.** The aim is roughly half of a project's issues on each pair, and the flexible
bundle is the part that can move toward it. The half-and-half rule names the counts; for a flexible issue
apply them in this order. First, once one pair holds more than 60 percent of the project's lines, the other pair, whatever
the working children say. Otherwise the pair with fewer working children in this parent. Where those tie,
or no child is working, the pair that fewer of the project's lines name. Where that ties too, either pair is
correct: the parent picks one and records the pick. Working children 1:1 with lines 6:5 are a tie on the
children and go to the pair with five lines. A fixed bundle is never moved to balance the count; only a
flexible issue moves.

**A pair of the other family.** A pair of the other family than a fixed bundle's stands only with its reason
on the line: the user's choice, or the move to Sonnet under [When SOL fails](#child-pair-by-issue-type)
with its cause and date; or its provider is exhausted in a readable quota snapshot or explicitly unusable,
with the cause and date recorded before replacement. Under row 1 the Sonnet pair is the security rule's own
and needs no further reason. These exceptions change the pair, never the tags or bundle.

##### The extended line

The line may carry the classification, the bundle and the source of the choice after its reason:

    Child pair: Sonnet (reference exists) - rewriting a skill's procedure [class: answer=reference, output=writing, procedure=goal, reach=one, check=read, history=fresh, core=plain; bundle: Sonnet fixed; source: table]

A line in the form given under "The line" stays valid. It is a prefix of the extended form and states the
pair, the axis and the reason without tags, bundle or source:

    Child pair: SOL (answer must be found) - a new retry state machine

The bracketed group is the last bracketed segment of the line and starts with `[class:`. It holds the seven
tags in the order of the table above, then `bundle:` (`Sonnet fixed`, `SOL fixed` or `flexible`), then
`source:`, the step that decided the pair the line names:

- `user choice`: the user named the pair for this scope, and the reason says so (precedence above).
- `table`: this section's rules decided: the family of a fixed bundle, the count rule for a flexible issue,
  or the move to Sonnet under When SOL fails. After that move the tags and the bundle stay as classified,
  the source reads `table` and the reason names the cause and the date. A flexible issue whose source is
  `table` was chosen by the count, unless its reason names such a move.
- `quota`: a readable snapshot decided the pair under [the quota rule](#the-place-for-a-quota-rule),
  including a snapshot-confirmed exhaustion move. An unreadable snapshot never supplies this source.

The axis is the one the Bundles row gives. A Korean issue body may keep the label `자식 pair`; the key
words after it stay as written here.

The line sits on a line of its own after a blank line, outside any list, in a section the size check does
not read. The check keeps a heading that is deeper than a section it already reads inside that section, so
the line gets a heading of the same level as the last heading the check reads, or a shallower one, whose
words the check does not know: `## Child pair` after `## Verification` closes that section. The report then
lists the heading under `unread_headings`, a note that changes no count. With the line there, the check
returns the same decision, counts, edit regions and verification kinds for the body with the extended line,
with the line in the older form and without any line. The tag, bundle and source words match none of the
check's word lists; the reason, being free text, can, so a line left in a section the check reads, or at the
end of one of its lists, is read as that text and can change the counts.

##### The record at release

When a child is released, the dispatching parent records the pair choice in its coordination record, one row
per release, beside the launch record
([Record the pair choice at release](../../crw-run/SKILL.md#record-the-pair-choice-at-release) gives the
steps).

**Where, and why there.** The relay's release request has fixed fields and refuses one it does not know, and
its settings record holds a model and an effort and accepts any pair of the role, so it cannot say why a pair
was chosen; a field there would be a change to the relay, which this rule does not make. The coordination
record already holds each issue's launch facts (request id, task id, settings), outlasts the child, and is
read by the management session and by Jun across projects. The issue's line states the choice made when the
plan was written; the row states what was acted on at release, and the two can differ: an older-form line
classified only at release, a user's choice made after planning, a move to Sonnet under When SOL fails.

**What the row holds.** The issue; the line as read, quoted, which restores the plan-time statement after
the issue or the rules change; the seven tags; the bundle; the pair as a family, with its model and effort
left in the launch record's settings entry, read from the creation receipt; the source at release, one of
`user choice`, `issue body`, `table` and `quota`, where `issue body` means a line already stated the pair
and the parent followed it, and `table` also covers a move under When SOL fails applied at this release; the mark `derived at release` when the line carried no tags; for a flexible
issue whose pair the parent itself chose, the counts it used; for a move under When SOL fails, its cause and
date; the version of the plugin manifest the rules were read from, which says which revision of these rules
classified the issue; the request id and the date. The command report adds `pair`, `source`, `rule`,
`default_pair`, `quota` (values and headroom, or unreadable with its reason), `window` (start, end, counts),
`limits`, and any `cause`, `date` and `classification_reason`. Keep the full report beside the row, including
stale decoded values when present; they are evidence of the read, not current capacity. A later change of the pair (a user's choice, a move under
When SOL fails, a replacement) adds a row and overwrites none.

**A line without tags.** The parent classifies the issue by the tags above, takes the bundle from the Bundles
rows, and marks the row `derived at release`. Derived tags are not a selection: the pair stays the line's
(precedence above), the line is not rewritten, and where the family of the derived bundle differs from the
pair the row says so. That difference is the one exception to the rule that a pair of the other family
stands only with its reason on the line: a line in the older form was written before bundles existed, so its
difference is recorded and not corrected. A tag the issue's text does not settle at release is left blank
in the row with the reason, and where that tag decides the bundle the row reads `undetermined`; a blank tag
never holds a dispatch, and the issue is amended when its owner next writes it. For an issue with no line at all the rule under "The line" holds: the parent writes the
line, in the extended form, and reads it back first.

##### Review the classification and change the table

**Result metrics never change the table.** Quality, speed, cost, review rounds, check failures during the work, quota used and
which model did better are not inputs to a tag, a Bundles row or a shape row, and no counter, score or
automation edits any of them. Judging a result is qualitative, and a table steered by results could tip all
work to one model. The table is refined only by what was visible when the issue was planned, and a
classification is judged against that: a success or a failure after release never reclassifies an issue.

**The review.** A review asks one question of each released issue: was the classification right? It reads the
issue as planned, its line, its release row, and what the delivered change and its packet show about the kind
of work: the modules the change touched, the procedure the packet had to spell out, the form of verification
the criteria used. Examples of a wrong tag: `procedure=goal`, but the packet needed long procedural
instructions; `reach=one`, though the issue's own criteria named several modules; `answer=reference`, though
no reference existed or was named; `check=machine`, though only a reading could judge the criteria. A
feature that only the work revealed, such as a module nobody could name when the issue was planned, is not a
wrong tag: the review records it as not visible at planning, since a change to the table can still ask
whether a new tag could have shown it. A user's choice and a move under When SOL fails are not
classification errors. The output lists the issue, the tag that was wrong, the value that
held and the evidence, a pull request or a packet line, and says nothing about which model did better. The
parent runs it for its own project at close, from the release rows; the management session runs it across
projects when a change is considered; Jun can ask for it.

**Changing the table.** A change to a tag, a Bundles row or a shape row is a decision of the management
session or Jun. The decision is recorded with its evidence: the issues whose classification was wrong, the
tag that was wrong in each and the feature that shows it, and what the change would have given each of
them. The change is then an edit of this section through an ordinary pull request. It applies to lines
written after it, and the lines and release rows already written keep their values. No number of wrong
cases triggers a change by itself; the cases are evidence to read.

##### The place for a quota rule

Run `crw skill pair-choice choose [--snapshot quota.json] [request.json]` at release. The request is a JSON
file or stdin; the optional snapshot is a separate file. The command reads only these supplied inputs:
it never runs OCX, calls its API, reads its operating files or reads the usage ledger. No snapshot producer
is approved yet. Until a cache-only OCX projection exists, run without `--snapshot`: an eligible flexible
issue takes the count rule's default, with `quota.readable: false`, reason `no_snapshot_producer` and source
`table`. Creating a snapshot with a probe-capable OCX command is not this procedure.

**Release facts.** Supply `bundle`, `as_of` (RFC3339), `working` and `lines` (each `{sonnet, sol}`,
nonnegative integers up to 1,000,000), optional `tie` (Sonnet or SOL; omitted picks Sonnet and records it),
and `releases` (`{at, pair, bundle}` rows). Use one row per flexible issue in the window, excluding the
release now being decided; replacements update that issue's contribution rather than count it twice.
Fixed rows are ignored. The parent serializes choosing and recording, so two choices do not share old
counts. Supply `recorded_pair` and `source` for a line: legacy `issue body` and `user choice` preserve it;
an extended flexible `table` or `quota` line is eligible for reselection. An `undetermined` legacy bundle
requires a preserved pair and `classification_reason`; it is never quota-reclassified. A fixed bundle
keeps its recorded pair or table family. None of this classifies tags or edits the table.

For example, a request without a snapshot (the two count objects default to zero when omitted):

```json
{"bundle":"flexible","as_of":"2026-01-01T01:00:00Z","working":{"sonnet":0,"sol":0},"lines":{"sonnet":0,"sol":0},"tie":"Sonnet","releases":[]}
```

**Snapshot contract.** `crw-pair-quota/1` is CRW-owned input, not a claim that OCX produces it. Sonnet
reads `claude`; SOL reads `openai`. Each side has `state` (`available`, `exhausted`, `unknown`),
`observed_at` (RFC3339) and named `windows` with `utilization` (used percent, 0–100) and `reset_at`.
This example is synthetic:

```json
{"schema":"crw-pair-quota/1","claude":{"state":"available","observed_at":"2026-01-01T01:00:00Z","windows":[{"name":"five-hour","utilization":70,"reset_at":"2026-01-01T06:00:00Z"}]},"openai":{"state":"available","observed_at":"2026-01-01T01:00:00Z","windows":[{"name":"five-hour","utilization":20,"reset_at":"2026-01-01T06:00:00Z"}]}}
```

Headroom is 100 minus the highest utilization among windows whose reset is after `as_of`. Expired
windows do not constrain it. Both sides must be complete, with an active window; `exhausted` must agree
with zero headroom and `available` with positive headroom. A missing/unknown side, malformed or partial
snapshot, unknown field, invalid window, future observation, or observation older than 30 minutes makes
the whole snapshot unreadable. Thirty minutes matches OCX's retained last-good report bound found in the source comparison, not its
older ordering-cache age. The parser accepts at most 1 MiB of UTF-8 JSON per input. A failed snapshot-file read
also takes the table default and records its reason. Token consumption never becomes remaining quota. Decimal utilization is compared exactly; numeric
literals are bounded to 128 characters and exponent magnitude 128.

**Decision and guards.** Start with the ordered count default under Bundles. With readable quota, switch
away from it only when the other side has strictly more than 10 percentage points of headroom. The
project's more-than-60-percent line rule still forces the opposite pair. Then cap the projected share of
flexible issues at 60 percent within an epoch-aligned UTC five-hour routing window (distinct from each
provider's quota reset window). If the candidate exceeds it and the other choice fits, use the other.
When neither fits (the first issue, or prior counts 1:1), retain the candidate and record
`window_rounding`: indivisible issues cannot meet 60 percent at those totals. If a feasible window cap
and the project rule force opposite pairs, hold with `guards_conflict`, rather than bypass either.

Confirmed exhaustion takes the other side and exempts balance guards; both exhausted holds. A supplied
`unusable: {sonnet: "cause", sol: "cause"}` is an authoritative provider-failure fact, not a quota estimate:
it applies the table's failure exception even without a snapshot, recording source `table`, cause and
UTC date. Two unavailable sides hold. A fixed bundle moves only for its own exhaustion/unusability;
positive headroom never moves it for balance. An explicit user choice, a legacy flexible line or an
undetermined line holds when its recorded provider is unavailable rather than silently rewriting it.
A fixed legacy line retains the fixed-provider failure exception. Host pair authorization still applies.

**Report and release.** Output is `crw-pair-choice/1`. `pair` is Sonnet or SOL (null on hold), `source`
is the step that decided it, `rule` lists applied rule codes, and the fields under The record at release
retain its quota evidence and counts. Exit 0 chooses; 1 holds; 2 rejects the request; 3 reports request
or output I/O failure. An unreadable snapshot is a reported fallback, not a command failure. Only a
readable quota decision writes source `quota`; preserved fixed choices from earlier `quota` lines now
record `issue body`. Write a changed eligible line and read it back before the packet, keeping tags and
bundle unchanged. Add the release row and report without overwriting earlier rows. The relay enforces
neither using this command nor the truth of supplied counts; missing rows remain review findings.

### The message both relations are read by

A supervisor instructs a parent and a parent reports back; a parent instructs a child and a child
reports back. The two relations carry different machinery and the same questions, so what
identifies a message, what the recipient owes because it arrived, and how far it actually got are
decided once. This section is the workflow rule: which occasion is which, what each form carries,
and when the level above is woken. The record form itself - the field names, the version, and what
each state means to the code that builds a message - belongs to the relay, as `relay-envelope/1`
in `docs/relay/envelope.md`, and is read there rather than copied here.
The words below are the ones that document uses, so a parent and a supervisor mean the same thing
by them.

**What a message asks for.** A request owes an answer from the recipient. A decision owes one from
Jun and from nobody else. A notification owes none. A status response answers a question somebody
put. The word is fixed by the occasion rather than chosen: an initial project assignment, a
midpoint check, a resume, a scope correction, a decision of Jun's carried down, and a user stop are
all requests, because each leaves the parent something to do. A decision Jun has already made is
relayed as a request for exactly that reason - what is owed is the application, not another
opinion. Upward, a completion and a block are notifications, a decision request is a decision, and
a midpoint answer is a status response.

A message carries no authority of its own. Being told something does not widen a scope, approve an
action, or open a goal; authority travels by the assignment, as
[supervisor, parent and child scope](#supervisor-parent-and-child-scope) already says.

**Supervisor to parent** names the request, the approved scope, the constraints, what changed in a
decision already made, the result and the reporting condition expected, and who owns the next step.
**Parent to supervisor** names the result and what changed, the project and issue it is about, the
evidence, what is blocked now and what that blocks, and the next action with its owner. A decision
subject appears only when a user decision is genuinely needed, with the reason and what each choice
costs. Technical judgment, ordinary review fixes and merge approval are the parent's own work and
are not handed upward; merge order between two parents is
[CRW-123 direct coordination](#direct-coordination-between-parents).

**When the level above is woken.** A completion, a new real block, and a decision only the user can
make. Not a CI run changing, not an acknowledgement, not a wait that has not changed, not a child
progressing normally. A supervisor holds no goal and every wake costs it a turn, so an automatic
notification that carries no decision is a cost with no reader. A midpoint check is the other path
and is answered on its own terms: it was asked for, so suppression of automatic notices is not a
reason to leave it unanswered.

**Five things that are not each other.** A send the transport accepted, a message received, a
request understood and agreed to, a change actually applied, and a result verified. Silence is none
of them. A conditional acceptance is not an application, a delivery failure is not a refusal, and
an unconfirmed or stale revision is not a current one. Where a relation has no mechanism for a
stage - and the supervisor relation has one only for a report's send and its readback, and none
above those - the answer is that there is nothing to look at rather than that nobody has looked
yet.

**A report is not a record.** Producing a report, the transport accepting it, the recipient reading
it and Linear holding it are four facts. The obligation to report something upward is discharged by
the record the supervisor reads, and a Linear write that failed leaves it owed however complete the
report text reads. The same logical result reported twice converges on one identity rather than
becoming two; a correction or a withdrawal preserves what it corrects and says which decision now
stands. A recipient that is paused, archived or deliberately out of contact keeps every obligation
it had and is not woken to collect them.
### Record writes and returned proposals

A Linear write belongs to the task whose own record it is, under
[supervisor, parent and child scope](#supervisor-parent-and-child-scope), and that holds for every
sub-task an owner opens rather than for a registered child alone. A planning sub-task drafting a
specification, an implementation child, a review or audit helper and an internal helper agent all
reach the same boundary: they read what their assignment allows, and they create, edit and delete
nothing there, no document, no issue field, no comment, no relation and no status. Whether a relay
holds the assignment decides where a receipt goes, not who writes.

What a sub-task returns instead is the document or issue ID, the revision or updated-at it read,
why the change is needed, the smallest change that would do, and links to the evidence and
artifacts behind it. A local draft of the proposed wording is useful and is still a draft: it is
reported as a proposal waiting on its owner, never as applied in Linear, and a file, a branch or a
message holding that text somewhere else is the same draft.

Exposure is not authorization. Linear write tools appearing in a sub-task's profile, a broad
permission profile, and an assignment naming the very document it is about are none of them a
grant, and an owner's authority over its own record does not travel to a sub-task with the work:
authority comes from the assignment, never from the reachable surface. Nor is there a route around
it, since another connector, a script, a CLI, a scheduled job or the sub-task's own helper agent
writing on its behalf is the same write. An explicit later instruction from the user is decided on
its own terms, for the records it names.

The owner that receives a proposal reads the record as it stands now before applying anything. The
proposal names the revision it was built on, so a record that moved since is reconciled against
the evidence rather than overwritten: the parts that still hold are applied, and a part another
actor has already changed or contradicted is raised instead of being reverted. Apply only what
this assignment's own authority covers, preserve unrelated fields, history and other people's
edits under [Linear holds canonical documents](#linear-holds-canonical-documents), and read the
result back before reporting it, as
[Use the available Linear capability](#use-the-available-linear-capability) requires. A write the
assignment already authorizes is simply made, not sent back to the user for an approval already
given.

A write that fails or returns an ambiguous result stays with that owner: read the current state,
record what happened, and retry the write itself. It is never a reason to re-run the sub-task's
completed implementation or verification, which is finished work at a revision the failed write
never touched. Until a readback confirms it, the record is reported as unwritten with its proposal
still open.

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
under [OPS-9.3](../../crw-run/references/operations.md#ops-93-the-parent-integrates-and-does-not-release). Use the shared
[Coordination message](../../crw-run/references/task-packet.md#coordination-message), reference the
values the existing relationship already holds instead of recopying them, and prefer one message
carrying a real state change over a heartbeat carrying none.

### Resolve the implementation repository

Resolve the issue repository label and its explicit GitHub owner/repo or URL
against its accepted scope, current delivery (its pull request when one exists, otherwise its task branch and head) and existing assignment.
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

An implementation issue is Done when every accepted criterion maps to a commit that actually landed on the
intended integration target, and a partial implementation never closes it. In today's operation one issue runs as
one packet and one delivery, so that mapping is its one current delivery's landing commit
([the work-unit rules](../../../../../POLICY.md#work-units-review-and-integration)). Under the push-only procedure
that commit is the integrator's fast-forward of the verified merged tree; where the delivery is a pull request,
read GitHub's current PR identity, repository, base branch, merged state and landing commit instead. A
related/reference PR, superseded replacement, approval, green CI, merge-ready flag or closed-but-unmerged PR is
not that evidence. Landing a prerequisite branch into another task branch is not integration into the intended
target. Verify the landing rather than treating an accepted integration request as done.

The delivery must carry the issue's accepted implementation scope; a partial landing
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

One fact has one writable canonical location. Every other place that states it carries a summary and a link to that location and is never read as an independent norm, so a passage that disagrees with its canon is out of date rather than a second rule. Three words classify what a document, or a passage inside one, is claiming: `current` is the version in force, `proposal` is text nobody has accepted yet, and `superseded` is a version that once held and is kept for its history. Recency does not decide between them, and an old implementation record never becomes a new product's `current` contract by being the most detailed document anyone can find.

### Where each document type is canonical

Each row fixes one document type's canonical location, the level whose record it is, the scope that location covers, what makes it change, and how the other side refers to it. Owner names who is answerable for keeping that location current, not a separate grant: write authority still comes from the assignment under [supervisor, parent and child scope](#supervisor-parent-and-child-scope), where a standalone issue keeps its own existing owner and no parent is invented for it. This table creates no second ownership rule.

| Document type | Canonical location | Owner | Applies to | Change trigger | Referenced from the other side as |
| --- | --- | --- | --- | --- | --- |
| Initiative definition: goal, finish condition, contributing projects' roles | The Linear initiative page body, written to the [initiative body standard](#initiative-body-standard) | The initiative's execution supervisor where one is designated, otherwise the user | The initiative and every project contributing to it | The goal, finish condition or scope changes, or a contributing project's role does | Its stable ID and URL |
| A project's or issue's own requirements, scope, priority, acceptance criteria and open questions | The Linear project or issue body | The parent, for the project record and the issues it owns; a standalone issue keeps its own existing owner | That project or issue, at the revision read | An accepted requirement, scope or criteria change | The item link, beside the expected behaviour and criteria stated in the pull request's own text, because a private link alone does not carry them |
| Accepted decisions and dispositions | Linear comments on the item the decision binds | The level whose record that item is | The item and anything citing it | A decision is accepted, or a later explicit correction supersedes it | The comment cited by ID and date from the commit, pull request or repository document it settles |
| Progress and status | Linear updates on the project or initiative | The level whose record it is | That subject's reporting period | Material progress, under the record rule in [supervisor, parent and child scope](#supervisor-parent-and-child-scope) | Not copied into the repository at all |
| Architecture, API, schema and module contracts, and the rationale for the technical decision behind them | The implementing repository, versioned with the code that implements them | The implementation owner of the issue whose pull request carries them | That repository at the commit carrying them | The implemented contract changes | The Linear issue citing path and commit, never a copy of the contract |
| Install, run, test and recovery procedures; development, CI and security rules | The same repository, in its own policy and contribution documents | That repository's maintainer | That repository, at that revision | A command, path, prerequisite or rule changes | A Linear link, which does not restate them |
| Research and verification verdicts | Linear, in the issue or the linked coordination document | The level that commissioned the work | The question asked, under the conditions recorded | A verdict is reached, or later evidence overturns it | Links to the repository's reproduction and to the raw evidence |
| Raw, large or private evidence | The durable private evidence root recorded under [OPS-5.4](../../crw-run/references/operations.md#ops-54-evidence-location) | The task that produced it | The run that produced it | A new run replaces it; nothing is edited in place | A citation carrying version or digest, date and access scope, never pasted into Linear or the repository |

The first two rows are levels rather than rivals. The initiative body carries the initiative's own scope, finish condition and each contributing project's role as recorded there, and a project or issue body carries that project's or issue's own requirements and criteria. Project detail is not copied upward, and the initiative's definition is not restated downward.

A research or verification result splits three ways rather than living in one place: the verdict and what was decided from it belong to Linear, the code, configuration and parameters that reproduce it belong to the implementing repository, and the raw output belongs to the evidence store. Work with no code target at all keeps its reproduction in the delivered output identity that [Resolve the implementation repository](#resolve-the-implementation-repository) already holds under the non-PR evidence rules, so having no repository costs it no canonical home. Naming its version, its date and who can reach it is what makes that third part usable later, since a path nobody can open is not evidence. Cite a revision where the source exposes one and its updated-at where it does not; promising a version a connector does not expose produces a reference no one can check.

A project or repository may hold its own explicit rule, and within its own scope that rule wins, because this policy fixes where a type of document lives rather than what a product decides about itself. Read such a rule as narrower rather than competing: a repository's own policy naming its branch, review and release authority is the repository-owned rows above working as intended. Where a project rule genuinely contradicts this one, record the conflict with both sources and route it to [crw-plan](../../crw-plan/SKILL.md) instead of settling it by following whichever document is nearer to hand. Loading this policy migrates nothing on its own.

### Classify an existing document

Before proposing that anything move, write down what exists, one row per document:

| Current location | Content and scope | Canonical location | Disposition (keep / split / migrate / preserve) | Rationale | Conflicts and unverified items |
| --- | --- | --- | --- | --- | --- |

`keep` is for a document already sitting in its canonical location. `split` is for one document mixing types whose canonical locations differ: each part goes to its own, and what stays behind becomes a summary and a link. `migrate` is for a document that belongs elsewhere whole; move it within authorization and leave a link behind. `preserve` is for history and superseded versions: they stay where they are, marked `superseded` with a date and a link to what replaced them, and are never promoted to `current` to fill a gap in a newer product.

The last column is not an overflow for anything awkward. Two binding sources that disagree is a conflict and takes no disposition until someone settles it, and a document nobody could read is unverified with the read that would settle it named. Both words already carry these meanings for [crw-tidy](../../crw-tidy/SKILL.md), which judges records against the rules stated here, so this table defines no second vocabulary. A classification is a reading rather than an authorization: it moves, rewrites and deletes nothing by itself, and a migration still needs a scope that covers it.

### Keep the two sides consistent

When one side changes, the other is checked before the change is called done, and which side moved decides what follows. Two cases look alike in a diff and are not the same thing.

A **requirements change** is the canonical Linear side moving: an accepted change to scope, criteria or a decision. The implementation and the repository documents describing it are now behind, which is the expected consequence rather than a defect. Check the impact by naming the repository contracts, procedures, tests and open task branches the changed requirement reaches. The owner of the changed record raises it, and the implementing issue's owner updates the repository side inside its own assignment, while a sub-task returns the proposal rather than writing it, under [record writes and returned proposals](#record-writes-and-returned-proposals). Where a review is already bound to the old criteria, the registered set is updated before the next claim rather than after it, because a verdict recorded against superseded wording rules on the wrong obligation.

An **implementation mismatch** is the repository side diverging from a requirement nobody changed. It is a defect to route and never a licence to rewrite the requirement to match the code. Check the impact by establishing first whether the behaviour is wrong or only the repository's description of it is, since those have different fixes and different owners. [crw-check](../../crw-check/SKILL.md) carries the finding to the task that owns the implementation; where two currently binding sources require incompatible things, that is a contradiction for [crw-logic](../../crw-logic/SKILL.md) rather than a mismatch.

Dated evidence decides which case applies, never which side is easier to edit. Read both sources with their revision or updated-at and compare them with the accepted decision that last moved either one. Where no such record can be found, the classification is unverified with the missing record named, and the requirement stands until something supersedes it.

Verification closes either case. Every cross-reference names the stable ID or path and the revision or updated-at it was read at, and once either side moves, the other is read again to confirm that the reference still resolves and still says what the citing document claims it says, because a link that resolves is not a link that still agrees. Progress and decision records keep following the rule already stated under [supervisor, parent and child scope](#supervisor-parent-and-child-scope), where decisions and dispositions are comments and material progress is an update; this procedure adds no record shape of its own.

### When the implementing repository changes

A repository-owned document's canonical home is the repository that currently implements the contract it describes, resolved through [Resolve the implementation repository](#resolve-the-implementation-repository) at the time it is read, and never the repository it was first written in. When implementation moves, whether by a split, a merge, a rewrite in another language or a successor product, each repository-owned document is resolved again: it migrates with the contract it describes, one whose contract the new repository does not implement stays where it is as `superseded` with its date and a link to its successor, and none of them becomes the new implementation's `current` contract until an issue there accepts it by criteria. Where a split leaves one contract implemented by more than one repository, the document is split along the boundary each repository actually implements; where the contract itself is genuinely shared, the repository that defines and publishes it is canonical for it and the consuming repositories carry a summary and a link. Until a plan names that repository, the document is a conflict with the decision it needs written beside it, never a canon assigned by guess. The Linear rows do not move, because intent belongs to the product rather than to the code that happened to carry it. What changes there is the repository label, and only on the issues whose edit target actually moves, under the [Linear operating model](#linear-operating-model) and only inside an authorized migration. An issue already delivered against the old target keeps the label recording where its work landed; what is repaired on such an issue is the path and commit its citations point at, never the record of where it was delivered.

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

Only one owner controls an operation. `crw-define` defines initiative intent, `crw-next` selects the next action, `crw-plan` decomposes agreed goals into projects and issues, `crw-run` supplies execution operations at the level the task is bound to, including initiative supervision through project parents, `crw-loop` owns the project parent's native goal and automatic repetition, `crw-status` reports the current situation and its schedule verdict without choosing an action or auditing criteria, `crw-check` compares delivery with intent, `crw-logic` investigates contradictions, `crw-tidy` supplements records that fall short of the rules already agreed, and `crw-refactor` diagnoses structural debt after cycle verification and carries selected repairs into the existing execution owner. A focused audit returns findings to its caller; it does not become another coordinator or recursively dispatch the caller.

`crw-run` owns goal-free execution of one project's agreed scope, including parallel
issue children, verification, integration and newly ready successors. A ready batch
is a scheduling unit; only an explicit narrower request limits delivery to that batch.
[crw-loop](../../crw-loop/SKILL.md) adds creation/restoration of a native parent goal
and automatic host continuation to the same Run execution and scope. Run holds no goal,
which is the default rather than a gap: what returns a waiting parent is the delivery path,
on the readiness its start policy recorded. A goal is what an explicit Loop opens, and
`crw-loop` establishes it. Run inside Loop returns to the
existing owner without another goal. Both reuse [Project parent binding](integrations.md#project-parent-binding).
Verified scoped deliveries establish progress; parent-local source changes and CXC
implementation phases are not completion conditions. Children keep their own CXC
lifecycle. Explicit parent workflow choices and existing CXC state require supported
transitions; `crw-loop` owns goal/hook preflight, activation and recovery rules.

### Completion follow-up in an existing execution workflow

Resolve scope from the full current assignment and prior approvals, not just the latest short message. Authorization survives skill switches. An authorized project-management or execution assignment includes its routine preparation, in-scope corrections, verification, and necessary progress/audit updates to the linked Linear coordination document. Record observed facts and accepted decisions without asking again; do not silently change requirements or issue completion criteria. A status question does not cancel an active run.

Use the current request together with the established assignment. Checking a managed worker's delivery is part of completing that assignment; a short request such as "this issue looks done" does not reset it to a standalone read-only audit. The coordinator sends in-scope corrections or requests for missing verification to the existing responsible task and rechecks the result without another permission round. `crw-run` owns that follow-up; bounded audit helpers return findings to the coordinator.

Explicit read-only, report-only, pause, no-contact, or narrower delivery limits win. An unrelated task link or a standalone audit does not establish execution authority. Existing authorization does not expand to new requirements, changed execution settings, release publication, deployment, issue closure, or messages to unrelated tasks. Reuse authorization that already covers those actions; ask only for the part that needs a new decision.

Recover the existing task first. A replacement is routine only when task creation/recovery is covered by the assignment and the host permits it, the earlier task is proven inactive, its work and receipts are preserved, and the replacement keeps the same scope and settings, the one exception being a child pair moved under [Child pair by issue type](#child-pair-by-issue-type), which is written on the issue's line first with its cause. Uncertain delivery or a read failure is not proof that no writer remains.

### Default independent execution

Write instructions sent to child tasks in English, including initial assignments,
review corrections, active-turn steer messages, resumes, and restoration blocks. The same applies
to a supervisor's instructions to a parent and to messages between peer parents.
Translate the actionable instructions without changing their scope or acceptance
criteria; preserve exact identifiers, URLs, paths, code, and necessary source quotes.
A child also works and writes in English: its own messages and final return, its commit
messages, its pull request's title, body and review replies, and the text of its receipts. The
assignment says so in its `Language:` line. Without that line a child writes in whatever language
its host, its workflow or the last thing it read leaned toward, and children of one project end up
answering in different languages. Exact identifiers, quotations and code stay as they are.
Keep task titles under the existing Korean title convention, and keep user-facing
reports and Linear records in Korean unless explicitly requested otherwise. A child writes neither
of those: it returns proposed Linear changes for its parent to write, and the parent reports to the
user. This language rule applies to future messages; it does not require resending old prompts
or waking existing tasks merely to change their language.
Name the three levels the same way in both registers. A Korean report or record calls them 감독 세션,
부모 세션 and 자식 세션, spelled out where prose first introduces the role and shortened to 감독, 부모
and 자식 after that or in a one-line label, while the English instruction keeps the role words those
records are keyed on. They are one role in two registers, so neither version needs a gloss.

Unless the request chooses otherwise, an independent child task that `crw-run` creates or resumes runs the child pair recorded for its issue under [Child pair by issue type](#child-pair-by-issue-type), with CXC Loop as its workflow, and owns its own host goal, goalplan, and FSM. That pair is one the declared policy lists for the child role, read from it at the time of the call under [Execution settings by role](#execution-settings-by-role) rather than restated here, so one location cannot fall behind the other. Precedence, highest first: host and tool restrictions; the explicit limits in force for this request, such as plan-only, read-only, status-only, no-goal, no-FSM, no-create, or current-task; the user's explicit model, effort, or workflow choice for this scope; then this default: CXC Loop as the workflow and, for the pair, the one the issue body's child pair line records, or the table's answer where the issue has no line. A later explicit instruction supersedes an earlier one only for the same constraint, so every limit it does not contradict stays in force. The result is the effective setting, and an effective Loop workflow carries the same weight as a separately requested one.

CXC `LOOP-DOCS-FIRST-01` applies to a CRW child as CXC states it, to the child's own issue: a single-cycle issue skips the docs-only first cycle, and a child that plans two or more work-phases opens with one, with CXC's roadmap debt for scope found later. A correction generation, a base-refresh generation and a separated publication step are not the first work-phase of new work, so they do not open `LOOP-DOCS-FIRST-01`'s docs-only cycle.

This default binds only `crw-run`'s independent children. The coordinator task, other products' global configuration, and CXC internal helper role routing keep their own settings.

Non-PR work without repository changes uses the working directory, source/result access and durable evidence defined in OPS-5.1 of the [Operations contract](../../crw-run/references/operations.md#ops-51-placement); it does not need Git or PR capability.

A new independent child is also created with enough capability to finish its delivery: file access for its checkout and evidence, git metadata access for its branch and commits, and the network access its push, pull request, and checks require. Broad local capability is the normal case. Worktrees, branches, scope, and recorded ownership separate concurrent work; the effective sandbox and permission profile define the enforced access boundary. The default covers a trusted implementation task inside the operating scope that creates it; an explicit narrower policy for the scope or the assignment governs over it, the effective profile is read back from the creation receipt, and the sandbox itself is never bypassed. Changing the default is a recorded decision rather than something a review performs. Apply the settings through the creation tool's real arguments and verify the returned profile. Tasks already running keep the settings they were created with; this is not authority to widen a live task or bypass a sandbox. See [Operations contract](../../crw-run/references/operations.md) for the owning rules.

The coordinator applies the effective settings through the creation tool's real arguments, sends the bounded issue packet in the initial work prompt, and verifies the returned settings. When CXC Loop is the effective workflow, that prompt invokes the installed `cxc-loop` skill; an explicit non-Loop or no-goal alternative omits that invocation and names the agreed workflow instead. Loop mechanics belong to the child and its `cxc-loop`/`cxc-pabcd` skills; see [Prepare and dispatch](../../crw-run/SKILL.md#prepare-and-dispatch).

- A standalone status request reads existing tasks and evidence and wakes nothing. A checkpoint inside an authorized ongoing initiative is not that request: it inspects the existing parents, hands the pending work the approval already covers to the responsible parent on the resume path that parent already has, and reports the actions actually taken. It starts nothing that already has an owner, and an explicit status-only, report-only, read-only, plan-only, pause or no-contact limit on it still wins.
- A setting the creation path cannot apply is settled before the task exists: use an already-permitted path or effective configuration that applies the requested values, or report the concrete unsupported capability. Never create a task already known to carry the wrong setting, and never silently downgrade it or claim the requested value.
- A mismatch observed after creation is reconciled on that same task.
- A user correction to model, effort, or workflow adjusts the same task where the transport supports it, and is reported otherwise, keeping stable IDs, unchanged permissions, and preserved progress, reconciled before any resend.
- The effective workflow is restated in every later send to that task, not only in the first one. A transport carries model and effort as settings it can check and has no field for the workflow, so a correction or a resume that omits it drops the one setting nothing else restores. Long work, a compaction, and a mid-work instruction each put distance between the original prompt and the task acting on it, and the restatement is what closes that distance. [Task packet](../../crw-run/references/task-packet.md#restoration-block) holds what travels with it.

The first full assignment is checked against [First full assignment required fields](../../crw-run/references/task-packet.md#first-full-assignment-required-fields) before it is sent: the effective workflow with the skills to apply, the issue scope, the verification boundary, the handoff and completion boundary, and the model and effort. They travel in that first request and add no preparation turn before the work. Where a workflow with its own goal and state is effective, the child's first execution also leaves and reports its activation evidence, rather than that evidence appearing only in the final return.

Judge a dispatch on four separate facts rather than one: the instruction the child actually received for this dispatch, the settings the dispatching call requested, the settings the receipt returned at the scope it claims them, and whether the child's own goal and goalplan state show the effective workflow running. Each can hold while the next fails. The served model being the defaulted one, an active native goal, or the skill named in the prompt is not evidence of the last, and a single reading of a child still inside its first turn is not evidence against it. [Dispatch verification](../../crw-run/references/dispatch-verification.md) holds the judgment cases, the classes a missing loop falls into, and the authorized exceptions that are not defects.

### Default parent start policy

A `crw-run` parent settles its start policy once, before it creates the first child of a run, and records the result where the next session reads it. Unless the request chooses otherwise its default child cap is 6: at most six of this parent's own children run at the same time. Precedence, highest first: host and tool restrictions; the explicit limits in force for this request, such as plan-only, read-only, no-create which bars child creation, no-goal which bars only an explicitly requested Loop and leaves the default goal-free Run untouched, or a stated concurrency limit; the user's explicit choice for this scope; a decision already recorded for this same project while the conditions it stands on still hold; then this default. The cap is a ceiling on simultaneous children rather than a batch size, so the parent still dispatches the largest useful set inside it and refills a slot as soon as one genuinely frees, counting a creation whose outcome is unresolved as still holding one.

That number is the value chosen in one 2026-09-18 run and carried forward as the standing default. It is not a measurement of what this or any host supports, and [OPS-8.4](../../crw-run/references/operations.md#ops-84-stating-the-scale-that-was-actually-verified) governs what may be claimed about scale, so a run that needs a different number states its own and records why. The default binds this parent's own children and is not a host-global limit: several parents share one operating scope under [OPS-3.1](../../crw-run/references/operations.md#ops-31-the-operating-scope-is-the-sharing-unit), each counts only its own children while what the others are running informs the observation that can lower the number, and nothing interlocks them.

The role decides which goal a task opens: an initiative management task opens no native goal and runs no automatic loop; a project parent opens none either, waiting idle with no goal while a delivered relay event resumes it, and building no CXC goalplan or FSM; an issue child creates or reuses its own goal for the issue scope and keeps its CXC Loop. That parent default is CRW-165's 2026-09-21 decision, which replaced the 2026-09-20 arrangement in which the parent held its own goal and its continuation was a bounded Stop nudge. An explicit user no-goal limit now agrees with the default and bars only an explicitly requested Loop. [Start policy](../../crw-run/references/start-policy.md) owns the role table, the recorded fields including the pairing matrix and the readiness facts, and the compatibility and evidence rules.

A value this precedence settles is applied without asking. A value it does not settle is a new decision, asked before anything is created rather than after. [Start policy](../../crw-run/references/start-policy.md) owns the recorded fields, the scope each decision carries, when a recorded decision is re-read instead of re-decided, and what bounds the number actually dispatched.

### Publish for review when the work is reviewable

**The repository's integration rule is the push-only procedure.** Internal work integrates by one
integrator who merges the candidate into a local integration tree over `dev`, verifies that tree
with the local full verification (`verification-record/1` with `result: pass`) and fast-forwards
`dev` to it ([POLICY.md](../../../../../POLICY.md#branches-and-authority)). The pull-request route
below is the relay's own mechanism and the route an external contribution uses; while the installed
relay runs it, these clauses describe that path, and where the relay refuses the push-only flow that
is missing official support to report.

This applies where the assignment's scope expressly covers publication. Where it does not, the delivery is local commits or a frozen diff and the question of draft never arises; lacking publication authorization is a reason not to publish, not a reason to publish as a draft.

Where publication IS in scope, draft marks an implementation not yet worth reading. It is not a waiting room until reviewers finish. When the change is complete enough to review, it is published as a pull request that is open for review: created non-draft, or an existing draft transitioned to Ready for review. Ready is review entry, not merge permission and not proof the work is done.

Each step below is a separate recorded fact, in this order: implement with the local validation the change actually needs; open the PR non-draft or transition the existing draft to Ready for review; request the review the repository requires and the assignment's authorization covers, and confirm it actually started, because a request that never started is not a review; reproduce, fix, reply to and resolve findings, refreshing only the review evidence invalidated by a changed head, under the [reviewer policy](../../crw-run/references/merge-readiness.md#reviewer-policy); report `ready_for_parent_review` only once the relevant review, check, and finding gates are met. The coordinator then compares the issue's criteria against the current diff, base, head, checks, and reviews, and may merge under existing authorization. Release and deployment need Jun's approval.

Where the child's workflow runs an independent review of its candidate, that review belongs to the first step above, implementing with the local validation the change needs: it ends on a head, and the child's handoff says which head it reviewed and lists the commits made after it ([what a handoff discloses](../../crw-run/references/task-packet.md#what-a-handoff-discloses)).

Which of those findings has to be fixed before that report, and which may be left with a recorded acceptance, is decided by [impact](../../crw-run/references/merge-readiness.md#judge-a-finding-by-its-impact) rather than by how a reviewer labelled it or how many rounds have already run. A conditional acceptance there belongs to the parent that owns the criteria, and it is recorded as an acceptance carrying its follow-up rather than as a fix.

An optional reviewer that cannot start, stalls, or sits outside the authorized scope does not become an indefinite wait: use the fallback in [Merge readiness](../../crw-run/references/merge-readiness.md), record the gap, and continue. The first run Devin and Codex each make on the pull request is the exception: it is awaited within the [waiting budget](../../crw-run/references/merge-readiness.md#the-one-run-of-each-reviewer-awaited-before-the-receipt) before the receipt, and a run still going when the budget ends is recorded as pending rather than complete ([Devin and Codex reviews are references, not merge gates](../../crw-run/references/merge-readiness.md#devin-and-codex-reviews-are-references-not-merge-gates)). A required review gate is not waivable that way.

Review findings, pending CI, and ordinary revision pushes never send a pull request back to draft. Re-draft only when the implementation itself stops being reviewable.

Two facts are easy to collapse and are recorded separately: a relay receipt whose outcome is `ready_for_review` says the child emitted a reviewable revision, and GitHub `isDraft=false` says the pull request is ready for review. Neither implies the other.

Where publication is in scope but the child cannot execute it, the coordinator performs only that blocked action, from the child's verified artifact, and records the actual resulting state. The child keeps review and fix ownership. That is the exception for a capability-limited task, not a standing parent obligation for children that can publish, and not a way to supply authorization the assignment never had: a coordinator cannot publish on behalf of an assignment whose scope excludes publication.

Explicit draft-only, read-only, or no-remote-write limits win, as do the repository's own requirements. None of this adds an approval prompt to a workflow the user already authorized.

### Default dev integration

Jun authorizes a delivery workflow in which the implementation child carries the work to a reviewable state and the coordinator decides the integration. Under the push-only procedure the child implements, tests, commits on its task branch and pushes it, and the repository's local full verification of the delivered tree is its evidence; the integrator merges that tree into the integration tree over `dev`, verifies it and fast-forwards `dev`. A pull request exists only in the in-flight transition ([In-flight pull requests (transition)](../../crw-run/references/merge-readiness.md#in-flight-pull-requests-transition)); there the child owns every applicable review on it: intake, triage, fixes, replies, and rechecks. It reports normal completion only once the repository's named verification on the current head has passed and any applicable review has finished and blocking findings are resolved, with per-finding evidence. Which findings are blocking, and what a recorded acceptance of the rest costs, are defined in [Judge a finding by its impact](../../crw-run/references/merge-readiness.md#judge-a-finding-by-its-impact); a minor separable residue accepted there is a disposition the parent owns, not an unfinished obligation the report hides. A missing mandatory review or check is reported as blocked rather than as completion. The full contract, including the fallback for a task that cannot write git metadata, is [Operations contract](../../crw-run/references/operations.md).

For the child this authorization is also the explicit push approval that CXC `DEV-GIT-PUSH-01` requires. That rule says never `git push` without the user's explicit approval in the current session, and CXC rule text is loaded into every child, so the packet states which approval it carries: where its `Delivery:` line covers publication, the packet carries Jun's standing authorization above, and the child pushes its own task branch without stopping to ask, then never merges. The authorization covers no force-push, no tag and no push to `dev` or `main`. A packet whose scope excludes publication carries none, and its child pushes nothing. This states which approval satisfies the CXC rule here; it does not change the rule's text.

A child that needs something only a person can give, such as a decision, a credential or an approval its packet does not carry, does not ask with `request_user_input`, which CXC denies while a goal is active. It writes the question out, records `blocked_needs_input` on its turn and, where a relay holds the assignment, emits that outcome without a file, the question being in its blocked file and its final message (see the clause on a child that stopped to ask a question in [OPS-6.2](../../crw-run/references/operations.md#ops-62-record-shape)), so its parent can take the question to Jun.

The coordinator then checks the Linear criteria and the candidate's latest diff, destination tip, head, verification evidence and review resolution, and integrates without another confirmation round when those hold. This is standing user authorization for this workflow, not permission inferred from passing checks, and it supersedes the earlier recommendation that the coordinator avoid integrating. Use [Merge readiness](../../crw-run/references/merge-readiness.md) for the gate detail, preserve unrelated work and branch protections, resolve routine in-scope failures and recheck, then verify the actual landing rather than an accepted integration request. An explicit diff-only, no-integration, or narrower instruction still overrides this default, and the child never integrates.

A base that only moved is the coordinator's to refresh, not the child's: where nothing else is wrong it refreshes the candidate about to merge only, for an in-flight pull request by the forge update, and checks the result, under [Refresh the base yourself when only the base moved](../../crw-run/references/merge-readiness.md#refresh-the-base-yourself-when-only-the-base-moved), instead of returning the candidate for a base-refresh generation. For a task branch nothing is refreshed on the forge: the integrator merges dev into its local integration tree and verifies that tree. A conflict only in places the plan declared `mechanical` is settled the same way, by the declared rule and the `base-refresh mechanical` check ([Resolve a mechanical conflict yourself](../../crw-run/references/merge-readiness.md#resolve-a-mechanical-conflict-yourself)).

Release and deployment are not covered and still require the user. Where merging a branch is known to trigger a release or a deployment, obtain that approval before merging, since the branch name alone does not carry it. A repository requirement that genuinely needs a new decision remains a blocker for that action.

### Whether a relay holds this assignment

Every relay rule in this workflow is written as a condition — "where a relay holds the assignment" — and for a long time nothing said how that is decided. An undecided condition does not read as false; it reads as nothing, so the question was never asked and execution stayed on direct send and steer by default. There are two answers here and no third. Managed start and managed resume decide which one applies, and record the decision.

A relay holds this assignment when all three hold together: the packet's relay block names a shared state directory this process actually resolved; the issue lookup in that directory returns a responsible relationship; and the reading came from the store this process measured rather than some other file at the same path. One command answers all three, because the order between them used to be the hazard rather than the answer: `codex-session-relay --state "$RELAY_STATE" doctor --issue <the exact issue identity>` reports `issue.holds`, the responsible child and relationship, and `issue.storeAgreement`. Act on it only where that agreement reads `same`. Before registration has landed the assignment is still relay-managed if the state directory is agreed and the declared intent names that store; `assignment-find --issue` carries the same provenance under `relay.store` for the same comparison.

Treat a lookup that cannot name its own store as no answer at all. Run the lookup first against a mistyped state directory that holds no store and it is refused with exit 2 `store_absent` and creates nothing, because a read-only relay command never creates a store; that refusal is no answer either: check the path, since the store appears only when the relay's service or a writing command first opens it. Where some other store answers at that path, or an empty one a writer created there, the lookup truthfully reports that nothing is assigned — and a coordinator that believes it opens a second writer for an issue that already has an owner. That is why the answer carries the store it came from and why a disagreeing store is refused rather than reported beside a usable result.

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

Apply the [reviewer policy](../../crw-run/references/merge-readiness.md#reviewer-policy) to new assignments and existing task recovery before deciding which reviews to request or await.

Use the target repository's actual review configuration and observed results. Public/private visibility alone does not determine which reviewers run or what they can access. No named bot, vendor, or repository-management app is a universal dependency; a planned integration is not proof of an operational reviewer. Requirements come from the repository policy, enforced rules, and the assignment.

Judge review coverage, revisions, completion, and finding disposition rather than a tool's name or green badge. A provider's severity badge is not severity either, and neither is the round count or the cost of one more round: [impact](../../crw-run/references/merge-readiness.md#judge-a-finding-by-its-impact) decides that, and it neither exempts a real defect nor promotes a minor one. Reuse sufficient independent evidence, including an authorized local review where policy permits it. Optional integrations do not create an indefinite wait or a new approval round, except the first run of Devin and of Codex, which is awaited before the receipt ([Devin and Codex reviews are references, not merge gates](../../crw-run/references/merge-readiness.md#devin-and-codex-reviews-are-references-not-merge-gates)); required checks and formal approvals still apply. Keep credentials, private-source access, and any new paid usage within the existing scope.

## Evidence and handoff

Pass Linear IDs/links, embedded criteria, repository/revision identity, accepted decision sources, and known gaps. Keep requested, observed, and unverified state separate. Reuse proof only for the same revision, criteria, and environment.

Keep raw launch receipts and sensitive test evidence in established private locations; put only the necessary coordination summary in the canonical Linear document. If record-writing is outside the request or access is unavailable, return an unsynced update for the owner. Installed skills hold procedures, never project state or credentials.

In final reports, mention checks that changed the conclusion and meaningful unavailable evidence. Avoid a ceremonial list of every skill.

### Delivery reach and current usability

A delivery report answers two questions no status word answers: how far this change actually got, and what the reader can use right now. Report the reach as an ordered chain — the source revision, the local commit, the remote branch, the installed link or version, and an observed run of the changed capability — and carry only the links this delivery actually involves. A delivery with no repository target uses the same shape over its own artifacts: the input baseline, the delivered output revision or digest, and the verification actually observed. Where one report covers several independently delivered artifacts or repositories, each gets its own chain, because one artifact's gap says nothing about another's.

Choose the stages from what happened rather than from the chain's full length. Walk the chain in order, naming each stage this delivery has evidence for and, where one exists, the first stage that is missing or was never observed, saying which of the two it is. A delivery with no gap has no such stage to name. The stages are independent: never infer a later one from an earlier one, and never drop an observed later stage because an earlier one is absent, since a commit that was never pushed can still be live through a link resolving to that checkout. A stage this change cannot have, such as an installation surface it never touches, is left out rather than reported as passing, and a stage the issue's own criteria require is always named, as unverified when nothing was observed. A report that lists every stage every time trains its reader to skip the one that matters.

Changing source, installing it, and refreshing an already-loaded conversation are three events, and the installation method decides what the third one costs. A linked installation resolves each skill through a symlink, so an edit is visible to the next read of that file, no reinstall is involved, and a conversation that already read the old text keeps it until the file is read again or a new task starts. A versioned plugin installation resolves a cached copy of a published version, so an edit reaches nobody until the version is bumped and installed again, and a session already running may still hold cache-bound references to the version it started with, which is the copy the next install removes. Report the method actually observed and what the reader must do under it; where it was not checked, say so instead of assuming a linked installation. Reading a link's own target is what establishes which checkout an installed skill resolves to.

Usable now is a claim about a representative user path, run through the installed entry point, with the time and environment of the most recent such run attached. Passing checks, a listed hook, an accepted delivery and a completed issue each establish only themselves. [OPS-6.1](../../crw-run/references/operations.md#ops-61-six-states-that-never-imply-one-another) already records which states never imply one another, and [OPS-11.3](../../crw-run/references/operations.md#ops-113-four-stages-that-are-not-one-event) separates a landed source change from what a host installs and executes. Use those meanings rather than restating them here.

Where the run happened in a test store or a scratch path, or where an always-on process is currently stopped, the demonstration and the operating state are separate lines: what was demonstrated, in which environment, at what time; and whether the real path is serving now. Report readiness from what was observed, since a service seen stopped is not ready and that is a result rather than a gap, and reserve unverified for the part nobody read, naming the observation that would settle it. That unverified is this report's own conclusion about evidence it lacks, not the field value an installation check carries; [OPS-6.2](../../crw-run/references/operations.md#ops-62-record-shape) owns those values and the rule for a measurement time nobody made.

Close with two short lines — whether the reader has to do anything, and the shortest next step to use or verify the change — and answer any question about installation, activation or availability from evidence already held rather than by going to get it, because a reinstall, a service start and a new task each keep their own authorization.
