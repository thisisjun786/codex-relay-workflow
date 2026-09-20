---
name: crw-run
description: "Coordinate one Linear project through independent issue children, parallel delivery, verification, integration and successors under the project parent's own native goal. Also supervises an initiative's approved projects through their existing parents, and handles binding/recovery and explicit narrower operations. Use crw-loop to add a parent goal and automatic continuation, crw-plan for planning, and crw-check for intent drift. Formerly linear-run."
---

# CRW Run

Use the selected Linear project as the planning source and keep this Codex task
as its coordinator: one parent per project, one child per issue. Follow the shared
[supervisor, parent and child scope](../crw-plan/references/integrations.md#supervisor-parent-and-child-scope),
including standalone issues and explicit current-task work. Where the initiative above this
project has an execution supervisor, it verifies this parent's reported outcome and instructs this
task, never this task's children; where it has none, nothing changes. An explicit designation
naming an initiative is handled one level up, through
[Initiative supervision](references/initiative-supervision.md), which decides where that binding
belongs rather than assuming it belongs here. Each child owns its
checkout and execution; this task owns scope, dependencies, dispatch receipts,
review, and the decision to release the next work.

Run owns project execution, including newly ready successors within the agreed scope.
[crw-loop](../crw-loop/SKILL.md) owns the parent goal's lifecycle and the host-driven
continuation on that same execution, and the role policy makes that goal the default rather than
a separate request; it does not enlarge Run's project scope.
Load the installed `codexclaw:cxc-dev` and relevant surface skills for development
and review work. A child whose effective workflow is CXC Loop loads the installed
`codexclaw:cxc-loop` and `codexclaw:cxc-pabcd` and owns its goal, goalplan and phases.
Resolve current installed paths; never copy plugin versions from old records.
CXC owns those phases and subagent routing; the parent does not adopt a child's FSM.

Read [Integrations](../crw-plan/references/integrations.md) for Linear document
authority, tools, and Paperthin invocation rules. Use the available Linear tools
and relevant workspace Agent Skills for full documents, criteria, and work records.
Use `readchk` for bundled scope or model corrections, and `catchup` when the user
needs a refreshed briefing. Route roadmap authoring to
[crw-plan](../crw-plan/SKILL.md) within the user's requested scope.

## Determine the requested operation

A submitted `$crw-run <Linear project link>` execution request, where a plugin
installation exposes this skill as `crw:crw-run`, designates or restores
this task as the fixed parent using [Project parent binding](../crw-plan/references/integrations.md#project-parent-binding)
and executes the agreed project scope. Record that scope and delivery boundary,
including its known blocked successors, rather than equating scope with today's ready
batch. Reconcile existing work, reuse/create responsible children, verify and integrate
their results, and admit newly ready in-scope issues as capacity opens. The first batch
is a scheduling choice, not a finish boundary. The link does not approve undefined work,
future backlog additions, another project's work or replacing another parent.

The parent goal's lifecycle and unattended host continuation belong to `crw-loop`, and the role
policy makes that goal the project parent's default rather than an opt-in, so an ordinary project
execution request establishes it through that lifecycle instead of running goal-free. Inside an
authorized Loop, Run returns progress and pending obligations to the same Loop owner without
creating another goal. An explicit no-goal or read-only limit still wins, and existing scope and
authorization survive skill routing.

An initiative link by itself is not a target. Where a request merely cites one, resolve the
project actually being executed under the shared target rules: the link is context and it rebinds
no existing parent. Where the request is an explicit designation to execute that initiative's
agreed projects, that initiative runs through its projects' own parents under
[Initiative supervision](references/initiative-supervision.md), which also settles which task
holds the supervisor binding: this one where it already supervises that same initiative or is free
to take it, and not this one where it is bound to a different scope, since a task holding another
project or issue would leave that work without its owner. Planning, a status read, a
citation, an explanation of how the entry works, a usage question, and a quoted or unsubmitted
example are none of them that designation, and that list is not closed: what makes one is the
explicit request to execute, never the presence of a link.

Explicit status, explanation, plan-only, batch, issue, no-create, no-goal,
no-merge, or current-task limits override the default. An issue or milestone
target stays within that scope; it is not a request to run its whole project.
A quoted example, a question about usage, automatic skill selection, or an
unsubmitted UI prompt does not activate this shorthand. Host and tool restrictions
still govern each action, including task creation and goal activation.

- **Plan/prompts:** inspect project state and prepare task packets; do not launch.
- **Bind/restore only:** apply [Project parent binding](../crw-plan/references/integrations.md#project-parent-binding)
  and report the connection; do not dispatch children or create a goal.
- **Dispatch a named batch:** reuse prior authorization and settings, refresh its
  prerequisites, then launch only that batch.
- **Run a project or milestone:** carry its agreed scope through delivery, including
  successors. A milestone, named batch or issue narrows the same project's assignment.
  The parent goal's lifecycle and host-driven continuation belong to `crw-loop`, and the role
  policy makes that goal the default.
- **Supervise an initiative:** bind the initiative, fix its approved project set and completion
  boundary, and carry it through those projects' existing parents. Issue planning and issue
  execution stay with each parent.
- **Status only:** read existing tasks and evidence without waking them.
- **Verify completed work:** inspect its exact revision and relevant behavior,
  send in-scope corrections to the existing responsible task, and recheck its
  delivery under the established assignment. Respect explicit report-only limits.

A named-model correction steers the current dispatch; it does not authorize
global model or CXC role-config changes. Preserve explicit model and effort per
task; take an unchosen model, effort, or workflow from
[Default independent execution](../crw-plan/references/integrations.md#default-independent-execution).
Do not copy a model choice from a previous project into a new one.

Resolve task-creation authority from the submitted operation above and the
conversation below; never override host requirements. Apply the shared [delivery and integration default](../crw-plan/references/integrations.md#default-dev-integration)
and inherit existing authorization for coordination records and recovery. Release
publication, deployment, issue closure, and unrelated messages need scope covering
those actions. When the user designates this as the fixed management task,
use [Project parent binding](../crw-plan/references/integrations.md#project-parent-binding) for its recorded project link,
title, and sidebar pin, then continue the authorized execution here.

## Keep a project run moving

Run does not initialize, resume, complete or block a parent goal. It keeps progressing
the agreed project scope during execution: dispatch, wait, verify, correct, integrate
and start eligible successors. Before any child dispatch, record its observation path
and the requested delivery boundary under
[OPS-8.1](references/operations.md#ops-81-parent-continuation-and-waiting).

A dispatch-only request may return with pending ownership and an exact next step.
Otherwise continue until the agreed scope is delivered, the user stops, a resource
limit is reached, or no authorized progress is possible. An empty ready queue while
children run calls for bounded observation; a blocked issue does not stop independent
work. If the host ends the turn, preserve the unfinished project and exact resume step,
not a claim that the first batch completed the request. Run alone promises no automatic
future wake-up. A status-only request wakes nothing.

A request from a peer parent is answered here, by this task, under
[Direct coordination between parents](../crw-plan/references/integrations.md#direct-coordination-between-parents):
decide what this project's own issues and children do about it, never reach into the other
project's children, and escalate to the supervisor only an unresolved disagreement, a scope
change, new ownership, or a shared resource including the order into a shared target. Where the
two projects share no supervisor, or answer to different ones, invent none: record the unsettled
part, hold only that part, keep the independent work moving, and raise that decision to the user.
Record the agreement and whether it is still conditional; a peer message moves no delivery by itself.
An accepted condition reaches this project through its own child, on the correction path this task
already uses, and the evidence that it landed is that child's changed revision rather than its
acknowledgement. Return the outcome to the peer from here: a child never answers another project's
parent, and no peer reads this project's child for its answer.

[crw-loop](../crw-loop/SKILL.md) owns the parent host goal and its continuation across turns,
and the role policy makes that goal the default rather than something a request must ask for.
Returning from Run hands control back to that same owner. Explicit pause/no-goal limits still win, and existing
CXC parent state must be reconciled through its supported lifecycle, never reset to
avoid a guard. A parent holding its own goal does not alter child CXC defaults.

## Independent implementation tasks

In this managed execution workflow, implementation belongs to a responsible
independent child Codex task, including a single issue, an existing worktree,
and repairs to an existing PR. The coordinator owns scope selection, preparation,
dispatch, delivery validation, and authorized integration; a child whose assignment
covers publication owns its own pull request, including its checks and its review
cycle. Issue count and checkout availability do not turn the coordinator into the
implementation worker.

Read the coordination record and inspect the existing responsible task and any
current writer before creating or assigning work. Verify its actual task/host
ID, issue scope, current turn, checkout ownership, and execution settings. Reuse
that task for compatible follow-up work on the same issue under the existing assignment. A busy
task, an uncertain send, or an inaccessible record is not evidence that no writer
exists; reconcile before retrying or considering a replacement.

Where a relay holds the assignment, ask it first: an assignment lookup by issue
returns the existing relationship, its responsible child, its current state, and
the next expected action. Registering a different child for an issue that already
has an active or paused assignment is refused in the same transaction that would
have recorded it, so duplicate registered ownership cannot be created even by two
concurrent attempts.

That is a guarantee about the assignment RECORD, and it is not a lock taken before
creation: registration needs the task id creation returns, so two coordinators can
both find an issue unassigned and both create a child before either registers. The
lookup makes reuse the first move and names an existing owner; the registration
transaction is what settles which child owns the issue. A coordinator refused with
`duplicate_assignment` has therefore just created a losing writer. Assign it nothing
further, and tell it to stand down and preserve its artifact without waiting for it to
go idle: read its status and active turn, steer that exact turn with the stand-down
instruction, and use the ordinary message path only if it is already idle. Then
reconcile that artifact with the owner the refusal named. The bridge withholds an
interrupt, so nothing here stops its turn; a delivered instruction is not a stop, its
writes stay unreviewed until reconciled, and stopping it is never described as
something already done. Commands are in
[codex-session-relay](references/relay.md), transport limits in
[codex-thread-bridge](references/bridge.md).

Internal subagents are bounded helpers within their owning task. A child may use
them for its implementation; the coordinator may use them for bounded inspection
or review. A subagent handle, even one represented as a thread ID, is not evidence
of an independent child task. Do not replace the child with parent implementation
or a parent-owned implementation subagent just because the issue is small or a
creation path is unavailable.

Resolve creation and execution intent from the full conversation for this scope,
including prior authorization and the plan the user is accepting. Explicit intent
does not require a particular word such as “create” or “생성”. Accepting a concrete
new-task plan requests its execution; clear delegation in an established
independent-task workflow carries that task intent. Preserve the referenced
project, task/batch/run scope, and settings without asking the user to restate
them. Higher-priority host/tool restrictions still apply.

| Invocation context | Action |
|---|---|
| Submitted `$crw-run <Linear project link>` execution request with no narrower operation | Bind/restore the fixed parent and execute the agreed project scope, including successors, under the parent's own native goal per the role policy; host restrictions and an explicit no-goal limit still apply |
| Submitted execution designation naming a Linear initiative | Resolve where the supervisor binding belongs under [Initiative supervision](references/initiative-supervision.md), never rebinding a task bound to a different scope, while the initiative's own existing supervisor is the task that continues; then fix the approved project set and completion boundary, reuse the existing parents and their children, and hand each parent its project brief. Do not plan or dispatch another parent's issues |
| Request to create/reuse child tasks, a submitted prompt expressing that intent, or clear project delegation after independent tasks were established as the execution workflow | Reuse the responsible task first; create only when needed within that scope and allowed by the host, without another authorization round |
| Concrete new-task plan followed by the user's acceptance, such as “진행해” or “응” | Execute the accepted plan within its stated scope; do not ask for a creation keyword |
| Resume of an authorized run, including after compaction | Recover its authorization source, scope, and settings; refresh ownership and prerequisites, then continue the remaining in-scope obligations, including successors. Preserve an explicitly batch-limited assignment; do not repeat approval already covering the scope |
| Standalone short invocation such as `$crw-run 다음 작업 진행해줘`, with genuinely no creation intent in the conversation or prior authorization for this scope | Prepare the issue packet and inspect ownership; if a new task is needed and the host requires an explicit creation request, obtain only that missing request |

Merely mentioning the skill's name, an unsubmitted UI default prompt, a quoted
example, or task designation without execution is not a child-creation request.
The submitted project-run shorthand above is an execution request. Permission
for an unrelated earlier task does not authorize this scope. Discover a
permitted tool; a different transport does not waive the host's creation rule.
If authority or capability is missing, finish the executable packet and baseline
preparation, report the specific missing permission or tool, and request only
the unresolved decision. Do not silently switch execution mode or widen permissions.

An explicit request to implement within the current task overrides this workflow's
default separation: honor it with CXC and label the actual mode accurately.
Status-only, report-only, read-only, and no-create limits also win. Ordinary
coordinator review, verification, CI inspection, and authorized integration
remain here. Assignment is not Loop evidence: creating or assigning a child task
does not by itself create a goal or prove that a Loop started.

## Establish the project baseline

Read the linked Linear project, canonical planning/decision documents, milestone
descriptions, full issue criteria, and blocking relations through the available
Linear connector. Inspect the
repository's applicable guidance, branch, dirty state, worktrees, and relevant
implementation. Refresh volatile claims before acting. A project's summary prose,
milestone percentage, issue status, merged PR, and deployed behavior can disagree;
record the discrepancy rather than silently treating them as equivalent.
Missing connector access is a concrete limitation, not permission to invent issue details.

For a standalone issue, read that issue, its linked canonical documents and
blocking relations directly; project and milestone reads are inapplicable. Keep
its issue-scoped ownership and the existing management binding unchanged. Do not
create a project or project-parent binding to satisfy this baseline. If a transport
requires a project binding, use a permitted projectless execution path or report
that capability gap; never invent a project ID for a receipt.

For repository-changing work, compare local and remote commit ancestry. Preserve local-only commits and dirty
work. Record a full baseline commit for each task and decide how any prerequisite
changes will reach it. Do not push shared baseline commits through every task.

At initial dispatch and after a completion, blocker, or integration, scan the
agreed project scope for useful parallel work, including newly unblocked successors.
An explicitly limited batch or issue remains limited in both Run and Loop. Check
verified prerequisites, overlapping edit surfaces, existing
writers, shared runtime resources, and available execution capacity. Dispatch
the largest useful set of independent ready issues within explicit concurrency,
budget, and host limits. A shared repository alone is not a reason to serialize;
separate owned checkouts can carry independent changes.

Do not wait for an entire batch to finish before filling available capacity with
eligible independent work inside that boundary. Independent issue statuses alone do not establish
independence: serialize shared schema, persistence, contract, or central UI changes
when separation would cost more than it saves. Keep integration into a shared
target serial and recheck each candidate against the updated base. Record a
concrete dependency, conflict, ownership, or capacity reason for deferring an
otherwise ready issue. Do not invent extra issues or duplicate writers just to
increase concurrency; reconcile an oversized issue through `crw-plan` when a
useful split fits the authorized scope.
Apply the shared [issue-to-PR mapping](../crw-plan/references/integrations.md#issue-to-pr-mapping):
one implementation issue per PR, with one issue/PR pair per implementation
packet. A batch retains those separate pairs. If one issue needs several PRs,
or a proposed PR would deliver several issues, reconcile the plan through
`crw-plan` before new dispatch; preserve existing owners and active work.

Keep the human-readable coordination record in the project's linked Linear
document as part of the management assignment, without a separate recording request.
For a standalone issue, use its existing linked coordination document or a compact
owned section in the issue within the authorized recording scope. Keep private
recovery receipts keyed by the issue and actual task IDs; no project record or
project-level parent binding is required. If delegation or a relay is used, retain
the real coordinator task ID and routing identity; projectless does not mean
coordinatorless. Preserve unrelated issue content on each update.
For explicit read-only scope or unavailable access, return the unsynced update and
retain the task's private recovery receipt. Keep raw launch
receipts and sensitive evidence in an appropriate private location; local
snapshots point to the Linear document and are not another planning source.
Do not store project state or credentials inside this installed skill.

Before assigning a checkout, apply the shared
[repository resolution](../crw-plan/references/integrations.md#resolve-the-implementation-repository)
and put its verified issue target, remote, integration branch and full baseline
in the packet. Reuse an existing task's worktree and issue branch on resume;
classification changes alone never relocate it. Non-PR work can omit those code
fields with its explicit result and verification instead.

### Determine the execution mode

Managed start and managed resume decide, before dispatch, whether a relay holds this
assignment or whether this work is explicitly direct, and record which. Everything below
that reads "where a relay holds the assignment" is that decision, not a guess to be made
again per paragraph. The rule and what to record are in
[Whether a relay holds this assignment](../crw-plan/references/integrations.md#whether-a-relay-holds-this-assignment);
the command and its output are in
[codex-session-relay](references/relay.md#determine-whether-this-store-holds-the-assignment).

Run it from the process that will do the work, because what a task can reach is a property
of that task's profile on that host rather than a fact about the relay. One reading answers
the issue half and the store half together and says whether they agree; act on it only where
they do. A reading that names a store this process did not measure is refused rather than
used, and an unreadable store answers null rather than "nothing is assigned", because those
two are different answers and only one of them is safe to act on.

Record the outcome with the rest of the assignment: the mode, the resolved state directory
and socket, the store identity the reading reported, the delivery owner, and the measured
availability. A resume reads that back rather than deciding again from scratch, which is what
keeps a recovered coordinator from opening a second writer or quietly changing route
mid-assignment. Where the answer is direct, that is written down with its reason, and the
direct path is then the one this skill already describes, unchanged.

## Prepare and dispatch

Read [Task packet](references/task-packet.md) when preparing prompts. Each packet
must stand alone in a fresh context and name its prerequisites, scope, baseline,
acceptance criteria, verification, and return artifacts. For non-PR work without
repository changes, use the source document/data revision as the baseline and
return both that input baseline and the verified output identity: a stable result
link plus delivered revision/updated-at evidence, or a durable file locator plus
its digest. Snapshot the verified output when the source cannot recover old revisions. Omit Git
ancestry, worktree/branch/commit, push/PR/review/merge requirements and their OPS
clauses when they do not apply; do not create a repository or empty PR. Keep task
ownership, access, settings, criteria and recovery evidence. This non-PR path
applies throughout dispatch, observation and completion below.

For repository-changing work, every packet carries the current delivery contract, and where the template's older
delivery menu disagrees the contract wins. Name in the packet that the child owns its
commits, push, the pull request and the review on that same pull request through to
the applicable gates, and that the coordinator performs the merge while release and
deployment remain the user's. Then give the child OPS-5.5 and OPS-9 from
[Operations contract](references/operations.md) as context of its own: cite them by id
where the child can read this repository, and carry the clause text itself where it
cannot, because a packet has to stand alone and a child that never sees OPS-9.1 and
OPS-9.2 can follow the summary above while still requesting review on a draft or
reporting a missing mandatory check as complete. Carry that text rather than a summary
of it. A quotation can be diffed against its source and regenerated when the clause
moves, while a paraphrase cannot, and it is the paraphrase that drifts unnoticed.
Name the capability the child is being created with, and for a task that cannot write
git metadata say so explicitly and assign the fallback, since those are the parts the
contract cannot know. A packet carrying neither clause sends a child the previous
workflow.

Apply [Child task titles](references/task-packet.md#child-task-titles):
`ISSUE-ID · descriptive task title` (title text up to 20 characters, including
spaces; exclude the issue code and separator). Supply the title through the supported
creation field, verify the actual title by task ID, and correct it on the same
managed task when supported. The packet's title alone is not app-state evidence.

Apply [Independent implementation tasks](#independent-implementation-tasks) even
when no new branch or worktree is needed. Non-PR work uses its permitted working
directory and artifact access without Git metadata. For code work, reuse a checkout whose ownership is
verified for the responsible child; a coordinator may prepare it before handoff.
Otherwise follow the project/user placement convention. Record the actual path,
branch, and owner; do not create a second checkout just to prove task separation.
Placement, the ownership columns, and the write split between a coordinator that
prepares git metadata and a child that only edits source are in
[Operations contract](references/operations.md), which also owns the shared relay
service, its durable store, and the parent's continuation and waiting mode.

Discover the live creation tool and schema before claiming availability.
Use native app task tools when suitable. Before proposing or adopting the official
managed worktree as the creation path, consult
[Official worktree verification](references/native-worktree-verification.md), because
a capability flag from another transport is not evidence about that path. Run its probe
only when the current scope authorizes the task creation, resume, writes and pushes it
performs; under a plan-only or no-create scope, reuse still-applicable evidence or
report the path as unverified rather than letting a proposal become live execution.
If using `codex-thread-bridge`, read
[Bridge launch and recovery](references/bridge.md) before any mutation.
If no usable creation tool exists, reuse a verified existing task when its
follow-up path and authorization suffice. Otherwise finish the packets and
preparation, identify the missing capability, and report that nothing was
launched; parent or internal-subagent implementation is not a substitute.

Apply the effective settings through the creation tool's real arguments, and send
the bounded issue packet in the initial work prompt so the child can start the
assigned work. When CXC Loop is the effective workflow, that prompt invokes the
installed `cxc-loop` skill; an explicit non-Loop or no-goal alternative omits that
invocation and names the agreed workflow instead. Verify the returned task ID,
cwd/workspace roots, model, effort, and full permission profile from the creation
receipt, and reconcile a mismatch on that same task. A prompt saying “use this
model” is not a configuration override. A catalog entry is not proof of the model
that served the request. Settle a setting the creation path cannot apply before
creating the task rather than downgrading it.

Record request ID, task ID, host ID when supplied, turn ID, requested/actual title
and settings, and launch outcome. For code work include the checkout and full Git
baseline SHA. For non-PR work include the permitted working location and input
source revision; add the delivered output identity when the result exists. Do not put raw
credentials or full private prompts in public project records.

The request ID and the task ID are not two names for one thing, and the record keeps
both because they become available at different moments. The request ID is chosen
BEFORE the creation call, so it is the one handle that already exists while the task
does not; it is the management marker, it travels in the same full work request as the
assignment, and it is what identifies this creation afterwards. The task ID is the
native identity that reuse, registration and every later mutation route on, and it
exists only once creation has actually returned it. Join the two from the creation
receipt.

What to do when that receipt never arrived depends on the transport, so establish which
one you are on before relying on either path. Where it accepts a caller-chosen id and
exposes a lookup keyed on it, resolve the marker through that lookup rather than issuing
a second creation under a new marker;
[Bridge launch and recovery](references/bridge.md#receipts-and-safe-recovery) is that
mechanism here. Where it does not, and some permitted native creation tools do not, the
marker is still worth recording for the coordination record but it is not a recovery
key: an id that only travelled inside the prompt is indexed nowhere, and treating it as
a handle leaves an uncertain creation filed forever as a writer nobody can find. There
the backend's own task listing is where the search starts. Narrow it only on what the
host cannot rewrite, which in practice is the workspace and a time window around the
call. The title is a hint and never a filter: a host may generate or change it, so
filtering on it can exclude the very task being looked for and leave a live writer
orphaned, which is worse than a wide list. Narrowing is not identifying either way.
These fields are non-unique, two tasks can share them, and adopting a candidate on that
basis is how a coincident task gets registered or steered as somebody else's child.
Bind it only on something that could not be true of another task, such as its own first
user message being the packet that was dispatched, read from that task. Widen the search
before concluding nothing is there, and where nothing is identified the creation stays
unresolved and is reported as such; an unidentified candidate is not the child, and
identity stays on stable IDs.

Neither path always settles. A response lost at the wrong moment leaves the outcome
genuinely unresolved, and an unresolved outcome means a writer may exist. Treat it as
one: create no replacement while it stands, and report it rather than overwrite it.

Where a relay holds the assignment, register the relationship with its authorized
scope, record BOTH the parent's and the child's authorized execution settings from
the actual creation receipts, and record the issue's criteria as the canonical set
so the later verdict rules on the agreed obligations rather than on a reviewer's
recollection. All of that reuses what creation and the issue already provide; none
of it is a readiness round trip and none needs another approval. Registration uses the
task id creation returns, so the packet carries the shared state directory and the exact
issue identity registration was given instead, which depend on nothing creation returns;
the child resolves its own relationship by issue lookup when it emits, which is after the
work rather than at startup. That lookup matches the registered string exactly, so a
display key in the packet where registration used a stable id resolves to nothing. A child
fast enough to reach that lookup before registration lands finds nothing,
reports its completion unemitted and preserves the artifact; recover it from that same task
after registering, in a later turn carrying a continuation claim, rather than creating another
child or writing on its behalf. The receipt it then emits is the same event the on-time one
would have been. Field-level detail, including that recovery and which settings field is
nested in the response, is in
[codex-session-relay](references/relay.md). Without a relay this paragraph does not
apply and the dispatch above stands as written.

When CXC Loop is the effective workflow, the child owns its goal, goalplan, and
phases through its own `cxc-loop` and `cxc-pabcd`; the coordinator checks the
resulting evidence, not the child's internals. Creating a task does not create a
goal; an active turn does not prove the loop is armed. Missing loop prerequisites
are reported explicitly, with no silent substitution of a different workflow.

### Start policy and child cap

Before this run creates its first child or registers an assignment for one, whichever comes first,
settle the start policy and record it in full: every field [Start policy](references/start-policy.md)
names, each carrying its source, its scope and the conditions it stands on. A value the precedence
settles is applied without asking; a value that genuinely needs a new decision is asked before any
child is created, and only the action waiting on that answer is held while independent authorized
work continues. Registration is named beside creation because it does not always follow one: a
reused child already has its task id, so an assignment can become managed with no creation call in
sight. Counting is the separate question, and there a creation still means a child. On a resume,
after a compaction or for a later batch, re-read the identities those values stand on, restore what
has not changed, and adjudicate again only the field whose conditions did. Start policy owns the
fields, the scope each decision carries, the re-adjudication triggers and the cases this is judged
by; the standing cap and its precedence are in
[Default parent start policy](../crw-plan/references/integrations.md#default-parent-start-policy).

The first thing settled is the role, because it decides what this task opens: an initiative
management task runs without a native goal or an automatic loop, a project parent creates or
reuses its own goal for the approved project scope, and an issue child creates or reuses its own
goal and keeps its CXC Loop. Record the role beside the goal state with its objective and approved
scope, the hook compatibility, the observation path, and whether the approval policy in force can
carry the callback this run depends on.

Editing these instructions does not alter a turn that already loaded them, but where an installation
links this checkout a later read of them loads the edited text, so a run spanning sessions can
continue under instructions it did not start with. Report the local change, the pull request, the
integration, and what is actually installed and running as separate facts.

### Check who already owns the workspace

This runs before the checkout above is assigned and before the creation call, and
its result is what the packet's workspace-ownership fields carry. It is written
here because it is one procedure, not because it happens last.

[Operations contract](references/operations.md) OPS-5 owns the workspace-assignment rules
this procedure applies: placement, the ownership columns, who commits, and where evidence
lives. What is here is when to read them, what to compare them against and what to refuse.
Where the two disagree, OPS-5 is the one that changes and this follows it.

Before a checkout is assigned, read what already exists and who is answerable for
it: the repository's worktrees with their branches and locked state, the dirty and
untracked files in each, and any current writer. Then split the result in two, the
resources that existed before this assignment and the ones this assignment will own.
A path matching the convention for this task's name is not evidence that it belongs
to this task, because a name is something two tasks can choose: another writer may
hold it, or it may be what a creation that failed halfway left behind. Where the
convention name collides, the distinguishing suffix goes on the task segment, so the
checkout path and its `codex/<task>` branch move together and the existing checkout
and branch are left as they are; suffixing the path alone leaves the branch collision
in place, and Git refuses a second worktree on a branch another worktree already has
checked out. Record the answer in the ownership columns of
[OPS-5.2](references/operations.md#ops-52-ownership-is-recorded-in-columns-separately-from-the-path),
which is where created by, editing owner, git metadata owner, retention owner and
cleanup authorization live, rather than leaving it to be inferred from the path.

A new checkout is created only where the work actually needs one. The same task
reuses its existing checkout on resume and for follow-up under the same assignment,
and a reclassification alone never relocates it
([OPS-5.1](references/operations.md#ops-51-placement)). A temporary clone inside
another product's directory is not a workspace; where the placement convention
cannot be satisfied, report that rather than leaving a repository somewhere the next
reader cannot tell whose it is. Residue from a half-finished creation stays owned
until it is reconciled, which makes it neither free to adopt nor free to delete.

Before a large clone, dependency install, build or download, check the volume that
will actually receive the bytes and this assignment's own large artifacts, in the
scope the work needs rather than as a survey. Keep the kinds apart while recording
them, because they have different owners and different release conditions: source,
dependency cache, regenerable build output, original data, and verification evidence
a finished task is deliberately keeping. Take the permitted locations from the
operator's working instructions and this repository's exceptions; a fixed path, a
capacity threshold or a retention rule invented here would be a new global policy
this assignment has no authority to make. Two children needing the same large
original share a permitted read-only copy instead of receiving one each. A repeated
whole-disk scan that stops the work is its own failure, and a report that a disk
filled up is not a current measurement of it.

What the assignment permits and what the child can actually write are separate
findings, and for a linked worktree they come apart in one specific place: the
working tree is under the checkout, while the common directory, the per-worktree
index and the reference files are under the original repository, which
`git rev-parse --absolute-git-dir`, `--git-common-dir` and `--git-path index`
resolve. A successful write in the checkout therefore establishes nothing about
writing a branch or a commit, which is the measurement
[OPS-5.5](references/operations.md#ops-55-capability-a-new-child-is-created-with)
asks for before the task exists. When a write is refused, say which refusal it is:
an OS permission, the effective sandbox profile, a read-only mount, or another
writer already holding the path or its lock. One failed command looks the same in
all four cases and they have different answers. None of this is enforced by a
packet, a prompt or a hook; those carry the intent, and what constrains a write is
the sandbox, the permission profile and the filesystem.

A refused Git write is answered on the same child and the same checkout, and which
answer applies follows from which refusal it was. An environmental restriction, an
OS permission on the original repository's Git metadata, a read-only mount, or a lock
whose owning process has already exited, is cleared through its supported route and
the work continues there; establish that a lock is stale before removing it, because
a lock file records only that somebody took it. Another task still holding that path
or that lock is not a restriction to clear but an ownership conflict: leave its work
as it is, serialize behind it or settle it with its owner, and report the blocker
rather than displacing a live writer. Where the refusal is the child's own permission
profile, nothing here widens it: a running task keeps the settings it was created
with, and the supported answer is the
[OPS-5.3](references/operations.md#ops-53-who-commits-and-the-fallback-when-a-task-cannot)
fallback taken deliberately and recorded, with the child still owning the source
edits in that checkout and returning a frozen diff against the recorded baseline
while the coordinator writes the Git metadata. What both cases refuse is the
improvised route: redirecting `GIT_DIR`, cloning the repository somewhere writable,
or a parent commit taken as a workaround rather than as that recorded fallback, each
of which settles a permission problem by leaving the recorded ownership instead of
repairing it, and leaves a delivery the review cannot trace to an owner. `reset`, `clean`,
`stash` and `rebase` are not repairs either: they make the command succeed by
discarding the state that explains the failure. Preserve the files, record why, and
carry a blocker neither route clears to whoever can grant the permission.

Carry the result into the packet's workspace-ownership fields
([Launch packet](references/task-packet.md#launch-packet)), so the child is told
what is not its own instead of deriving ownership from the path it was handed.

### Verify what this dispatch established

The first full assignment carries the fields in
[First full assignment required fields](references/task-packet.md#first-full-assignment-required-fields),
and they travel in that same first request rather than in a preparation turn.

Then judge the dispatch on four separate facts: the instruction the child actually
received for this dispatch, the settings the dispatching call requested, the settings the
receipt returned at the scope it claims them, and whether the child's own goal and
goalplan state show the effective workflow running. A dispatch can pass the first three
and fail the fourth, and on a reused task the evidence for the first two is the
correction or resume itself rather than the original launch.
[Dispatch verification](references/dispatch-verification.md) holds the four verdicts and
their admissible evidence, the classes a missing loop falls into and how each is
recovered on the same task, the recorded regression cases, and the authorized read-only,
no-goal and non-Loop assignments that are not defects.

## Observe and verify

Use a compact native `wait_threads` snapshot with each task's actual host and
cursor, or the bridge's exact task/turn wait. Batch independent reads. A timeout
does not stop or complete work; it is not a reason to send the prompt again.
Observe the host's wait limits and keep the user informed without repeating
unchanged snapshots.

Apply [OPS-8.1 observation-failure handling](references/operations.md#ops-81-parent-continuation-and-waiting)
before interpreting either transport's result. Verify the target task/host and
turn, parse structured results and distinguish read failure from valid waiting
or terminal work. A missing turn or failed read is unknown, even if `timedOut`
is true; Git/PR state cannot replace that observation. Use the bridge's
[assigned-turn procedure](references/bridge.md#observe-the-assigned-turn) when selected.

Describe evidence separately:

| Claim | Evidence required |
|---|---|
| Independent child assigned | Creation or reuse receipt from the independent-task interface, actual task/host ID and ownership; not an internal-subagent handle |
| New task created | Creation receipt and recoverable task ID; reuse is recorded separately |
| Prompt dispatched | Accepted turn ID and matching user message |
| Instruction delivered into a running turn | Accepted steer receipt naming the guarded turn ID; a refused or mismatched turn ID is not delivery |
| Peer read an instruction | The task's own transcript showing the steered input; an acceptance receipt does not establish it |
| Peer acted on an instruction | The changed work, revision or reply; neither acceptance nor a read establishes it |
| Goal paused | The goal read back as paused; it does not establish that a running turn stopped |
| Requested settings applied | Actual returned settings, not prompt text |
| CXC Loop active | Child's active goal and current goalplan/FSM evidence |
| Agreed workflow actually followed | That task's own recorded phases, plan and evidence for this assignment. A skill-loading line, an acceptance receipt, or the instruction quoted back is an indication of receipt, not of compliance |
| Child reused its own existing goal | That task's current goal and goalplan read back under its own identity. A second goal opened for the same assignment is a duplicate, not a resume |
| Work delivered | Completed turn plus actual commit/diff and checks for code; verified result with both input baseline and delivered output revision/digest for non-PR work |
| Pull request review handled by the child | Per-finding trail on that PR: the finding, the commit that addressed it, and the recheck |
| Child reports normal completion | Required checks and reviews finished on the current head, blocking findings resolved; a missing mandatory review or check is blocked, not complete |
| Candidate ready to hand over | The child's handoff record: pull request and head, the verified base, the declared required checks and the runs by id and attempt, the review coverage actually read, and a judged disposition with evidence for every thread seen. A completion naming a pull request and saying nothing about its review is refused, because silence is the failure this exists to catch |
| Parent runs its own goal loop | That task's own active goal and goalplan, read back under its own identity. Parents and children each own one; a parent operating without a goal is an explicitly authorized fallback rather than the default, and is recorded as the choice it is |
| Verified for integration | Coordinator reviewed the exact revision and acceptance criteria |
| Receipt recorded, where a relay holds the assignment | The child's completion receipt with its revision hash and manifest |
| Verification decision, where a relay holds the assignment | A verdict at the current head revision, covering the registered criteria and naming the criteria set it was reviewed against |
| Coordination summary written | The coordinator's own connector write, confirmed by a readback carrying that job's structured record |
| Merged by the coordinator | Linear criteria and the PR's latest diff/base/head/checks/review resolution checked, then the actual landing verified |
| Release or deployment | The user's approval for that action, obtained before a merge known to trigger it |

Do not assume a worktree/task returned by a backend appears in the app's project.
Check the Desktop listing separately when the user needs that association.

After non-PR delivery, verify the delivered output revision/digest against its
input baseline and acceptance criteria. A later edit at the same URL invalidates
reused verification; it is not the same output. No commit or merge is required. After code delivery, identify the
final commit or frozen hashed diff/file bundle, then
check the prerequisite ancestry, scoped diff, acceptance criteria,
meaningful negative cases, and relevant user-visible behavior. Reuse valid proof
for the same revision and criteria; run missing checks or checks invalidated by
integration. A completed turn may contain a failure or interruption.

Where a relay holds the assignment, verify the revision it reports as current. If a
newer revision arrived while the review was in progress, the older result is not a
completion: re-read the current revision and verify that one. Two competing
revisions with no stated supersession are ambiguous, and an ambiguous head withholds
rather than picking whichever arrived later. If the criteria changed after a review,
that review certified wording nobody is judging by now, and there are two routes
back: judging the same revision again against the set now in force, or opening a
fresh execution generation. The same revision can be judged again only while it is
still the one the assignment stands on and any verdict it carries is `verified`;
a changed artifact, or an event already ruled `needs_changes`, `unverified` or
`aborted`, takes the new generation. See [codex-session-relay](references/relay.md).

Use independent review when scope/risk warrants it, through the current CXC
subagent protocol inside the appropriate checkout. A worker's self-report alone
does not fulfill independent verification.

For substantial intent or acceptance uncertainty, use
[crw-check](../crw-check/SKILL.md) as a bounded audit and retain coordination
here. Use [crw-logic](../crw-logic/SKILL.md) for a specific suspected logical
violation and `mandela` for self-confirming evaluation evidence. Use `shower` when
a nontrivial task packet needs a fresh-reader check, and `re0` to refresh that
packet after changes. Do not run every helper on every delivery or auto-invoke
Paperthin's user-only skills.

After verification, the coordinator applies [Default dev integration](../crw-plan/references/integrations.md#default-dev-integration),
unless the assignment limits delivery. Read [Merge readiness](references/merge-readiness.md)
to check current CI and reviewer evidence using the repository's actual configuration.
Serialize integrations that share a target, verify the landing, and update the
coordination record. A capable child owns its commits, push, pull request and the
review handling on it, and reports once the current head is clean; the coordinator
decides and performs the merge, and the child never merges. Release and deployment
still require the user. Delivery ownership and the fallback for a task that cannot
write git metadata are in [Operations contract](references/operations.md).

Read the child's handoff rather than rebuilding it. It already paginated the review and
enumerated the check runs, so collecting them again repeats work this contract just assigned
elsewhere, and re-triaging its findings opens a second review round it already owns. What the
coordinator adds is currency: re-read the head and base immediately before merging and compare
the counts against the record. That is a mechanical check, not a review. Where they disagree,
return the candidate to the same child fail-closed rather than fixing it here.

Report **verified**, **needs changes**, or **unverified**, with concrete evidence,
and distinguish implementation, merge, and deployment. Start a successor
only after its required contracts/revisions are verified and available in its
checkout and it belongs to the agreed project scope or explicit narrower assignment.
This is the same rule in Run and Loop; a goal changes persistence, not scope.

Name what the work is doing in the vocabulary its contract already defines, rather than
in words invented for the report. Ordinary progress, waiting on input, a stop and a
request for verification are conditions the parent has to tell apart, and two contracts
supply the words for different questions. What the child's turn did is a
[turn disposition](references/hook-contract.md#turn-disposition): `in_progress`,
`ready_for_review`, `blocked_needs_input`, `interrupted` or `failed`. Where the
delivery stands is
the [assignment state](references/relay.md#verify-the-current-revision) when a relay
holds it. Report them separately; a child blocked on a person has a turn that stopped
and a delivery that did not move, and one word cannot carry both. A condition neither
vocabulary names is reported as a blocker against the state that does apply, under
[OPS-6.2](references/operations.md#ops-62-record-shape).

After integration, apply [Implementation Done](../crw-plan/references/integrations.md#implementation-done)
before reporting or recording the issue complete. Read back the one delivery PR's
actual merge, intended repository/branch and landing revision. For legacy multi-PR
scope, verify the reconciled deliveries and their combined coverage instead. Retain
existing accepted operational criteria and never infer completion from an automatic status alone.

### Reuse the child's evidence while it still applies

A capable child runs its own checks and carries the review on its own pull
request, so where that evidence still applies the coordinator reads the result
instead of producing it again. What still applies is already settled elsewhere
and is not restated here:
[OPS-9.2](references/operations.md#ops-92-what-normal-completion-means) reuses a
result only at the same revision, criteria and environment and re-runs it when
any of the three moved, and [Merge readiness](references/merge-readiness.md)
refreshes whatever a new head, a changed base or a changed dependency
invalidated. Inside that, polling a check run the child is already carrying,
reading again every finding on a hosted review it has resolved, and re-running a
suite that passed and is still valid are the coordinator doing a level below
itself. Read the head it reports, the conclusions on that head and its
per-finding trail, and accept them as the evidence table above defines them.

Acceptance keeps its own work, which was never the child's. Confirm the reported
head is the head the pull request has now, the base is current and the merge is
clean, every accepted criterion maps to evidence that still applies at that head,
and any finding still open is named. A moved head invalidates the reused result,
and so does a base or a dependency the child never built against: that the base
is current now is not evidence that its checks ever saw this one, so re-run what
that change invalidated rather than read the old conclusion again. Where the
child's evidence is missing, ambiguous, stale or contradicted, or where
independent review is required and has not happened, get that evidence directly:
a shorter report is not a reason to pass a criterion nobody verified.

Reuse changes what the coordinator spends, not what the work cost. Report the
coordinator's own usage and the assignment's total separately, and claim a saving
only from a measurement that actually compares them.

## Return corrections to the existing task

Apply the completion-follow-up rule in [Integrations](../crw-plan/references/integrations.md#completion-follow-up-in-an-existing-execution-workflow).
Send an actionable packet to the existing responsible task without asking Jun to
approve or relay routine in-scope corrections. Include the issue and criterion,
reviewed revision, expected and observed behavior, reproducer/evidence, required
outcome, and focused verification. For missing proof, request that verification
without prescribing an unsupported code change.

Carry the [restoration block](references/task-packet.md#restoration-block) with every
correction. A running task has been working for a while, may have been compacted, and
is being addressed by a coordinator whose context it cannot see, so the facts it needs
to resume correctly are stated again rather than assumed to have survived. That is the
same reason the first request named the workflow: naming it once, at creation, is not
the same as it still being in force ten turns later.

Where a relay holds the assignment, the needs-changes verdict IS the correction: it
opens the next execution generation and queues the revision request to the same
registered child, carrying the superseded event, its digest, and the per-criterion
findings. Record findings with notes, because a correction with no findings is one
nobody can act on. Do not create a task, a second relationship, or a parallel
message path to deliver it. Without a relay, send the packet as described above.

The restoration block rides inside that payload rather than beside it. The findings and
their notes are what the relay actually delivers to the child, so that is where the
block goes, and a verdict is not issued until it is there. Declare which finding carries
it, so a correction the relay cannot carry is refused before it opens the next generation
instead of being discovered after there is nothing left to go back to. Where the installed
relay does not recognise that declaration, that is a limitation recorded and reported on
the assignment, and it is still not a reason to open the parallel path the sentence above
forbids. Nor is there a supported way to send it again afterwards: the verdict does not
resend. Record it as an undelivered correction and hand the decision to whoever owns the
assignment, per [codex-session-relay](references/relay.md#the-parent-verifies) and
[the restoration block](references/task-packet.md#restoration-block).

Refresh the task's identity, ownership, current turn, checkout, and prior
correction receipts before sending. Reuse its agreed model, effort, workflow,
permissions, and delivery scope. Of those the workflow is the one only the message can
carry, because a transport transmits model and effort as settings and has no field for
it. Route by what the task is actually doing: an idle task takes the
ordinary message path, and a task whose turn is running is steered into that turn
using its verified thread and active turn id. A transport that refuses a message to
an active task is protecting its turn, and that is a reason to steer rather than a
reason to wait for idle. If the turn changes between reading the state and sending,
the send is refused: read the state again and reclassify instead of retrying. Send
one correction by one route, never both. A timeout or an
uncertain send requires reconciliation, never a duplicate writer or blind resend.
Report a concrete access or ownership blocker when no supported path is available.
Whether a tool exposes steering and whether the host supports it are established
separately; a capability this bridge withholds is not a capability the host lacks.

Confirm the accepted turn, then inspect the returned revision and rerun the
failed criterion and relevant regression checks. Continue within the same scope
while the evidence supports a next correction; escalate a scope decision or a
blocker that cannot be resolved under the assignment. Report actual progress
without treating dispatch as repair completion. Keep requirements, issue state,
merge, and deployment under their existing authorization.

## Resume and handoff

On resume, read the coordination record and refresh its known task IDs before
creating anything. Recover after uncertain delivery; do not duplicate a writer.
Do not restart a stopped task merely to inspect it.

A resume this task publishes into an existing child carries the same
[restoration block](references/task-packet.md#restoration-block) a correction does,
for the same reason: it is a message into a context this task cannot read. Two
recoveries are easy to write as one event and are not. This coordinator recovering its
own lost context is the paragraph above, done from the coordination record. A child
recovering its own is that task's affair, handled by its own workflow from its own
durable state; the coordinator supplies the pointers and the assignment facts it holds
and does not reconstruct the child's plan on its behalf.

A final update gives task links/IDs, requested and verified settings, actual
progress, review outcome, and the next actionable dependency. Use the host's
created-task directive when required. State remaining capability gaps plainly.
This skill does not install a recurring monitor: use the automation tools only
when ongoing background monitoring is requested.

### Report delivery reach in the final update

The final update above says what the work is; this says how far it got and
what the user can use. Write the chain from
[Delivery reach and current usability](../crw-plan/references/integrations.md#delivery-reach-and-current-usability):
the stages this delivery has evidence for in chain order, any first stage that
is missing or unobserved, and the two closing lines. A merged pull request
completes the remote stage; whether anything was installed from it, and whether
the changed capability has been run since, are separate observations this task
either made or did not. A verified non-PR result uses the same shape over its
input baseline, delivered output and observed verification.

Keep it to the stages this delivery touched. An issue that changed only skill
instructions has no service to activate, and saying that once is shorter than a
readiness list nobody asked for. An issue whose criteria named installation or
live operation keeps that stage in the report even when the answer is unverified.

Do not close a gap in order to report it closed. A question about whether
something is installed or usable is answered from what this task already
observed; installing, starting a service, or creating a task to find out are
separate actions under their own authorization, and
[OPS-2.4](references/operations.md#ops-24-update-and-recovery) owns the update
path when one is actually authorized.

### Account for the resources this run leaves behind

A later management read should be able to say which checkout and which branch a
piece of work used, who was answerable for it, and why anything still on disk is
still there. So the close of a run states that once, in one place: every checkout,
branch, temporary artifact, evidence root and background process this run created,
changed, or is deliberately keeping, each with its owner, the reason it is retained,
and the next action, including who may release it and on what event. It reuses the
records that already exist and adds no new store: the project's Linear coordination
record for the human-readable list, or for a standalone issue the existing linked
coordination document or this task's owned section in that issue, with the private
receipts keyed by the issue and the actual task IDs carrying raw detail under
[OPS-5.4](references/operations.md#ops-54-evidence-location).

Write it against what was found at dispatch rather than against whatever is visible
now, because the comparison is what separates the entries. Pre-existing resources
this run never touched, what it added, what it is keeping on purpose together with
the event that releases it, what a failed creation left behind, and what two
children share are five different answers. Evidence a finished task is deliberately
retaining and residue nobody has looked at are both still on disk and are not the
same entry. A resource whose owner cannot be established is recorded as unknown and
returned as an open question; it does not become this task's by being found by it.

Cleanup is bounded by that ownership. This run may remove its own disposable
resources where the cleanup authorization column already covers them and their
readers have exited. Existing dirty work, another task's cache or evidence, original
data, active worktrees and shared services stay as they are. `reset`, `clean`,
`stash`, `rebase` and forced deletion are not cleanup here, and an old modification
time, a scratch-looking name or a full disk is not proof that anything is
disposable. Keep the three claims apart when reporting: what is proposed for
cleanup, what was actually deleted, and what capacity that actually returned.
Proposing is the default, and anything beyond this run's authorized resources is a
request rather than an action.

Processes get their own line because they outlive the turn that started them. For
each one this run started, record its working directory, purpose and execution
handle, and whether it is still running at handoff; report the ones left running
with their next action. A busy port, a matching command line or a familiar directory
does not identify an owner, so a process this run cannot claim is reported rather
than stopped.

The inventory is a record and not an enforcement. It says who is answerable and what
remains; whether anything is actually prevented from being written or deleted is the
sandbox's and the filesystem's answer, observed separately. A handoff that reports
the inventory has done that much and not more.

### Restate the run before continuing it

Recovery starts by restating the run in five lines, read from the
[coordination record](references/task-packet.md#coordination-record), which is already the place
these live and already forbids a second store:

- **The fixed binding** — the project or standalone issue, this task's own id, and the host it
  runs on, each by its stable identifier.
- **The current temporary target**, where one exists: the other project or issue this task was
  asked to look at, recorded beside the fixed binding rather than in place of it.
- **The approved scope** in force, with the limits that arrived with it and the start policy this
  run recorded, restored under [Start policy and child cap](#start-policy-and-child-cap) and
  adjudicated again only for the fields whose conditions have changed.
- **The current owner** of each piece of live work — the child task, its issue, its pull request.
- **The one next action.**

Those five are what the restatement reports. What it has to establish before reporting them, the
settings in force and the work that has to survive the restart, is the rest of this subsection.

Each line carries the time it was observed, because three claims are easy to write as one: what
the record said when it was written, what a read this turn returned, and what an earlier decision
settled on purpose. A later observation wins only over an earlier observation of the same thing,
in the same register. It never wins over a decision, and it never crosses registers: a host
reporting `idle` says that no turn is running, which is not an answer to whether somebody paused
that task, so a durable pause, a read-only or status-only scope and a no-contact instruction hold
until their own owner records a supported transition
([OPS-8.2](references/operations.md#ops-82-busy-paused-cancelled-and-archived-parents)). What a
fresh reading does supersede is a stale note of its own kind: a "not started" or "last seen idle"
line loses to this turn's reading of that same register and stays as history. A line this turn did
not refresh is marked unrefreshed with its observation time rather than reported as current.

Three endings look alike from outside, and the restatement separates them. A record saying
completed is a claim rather than a verdict, and it is corroborated against the evidence its own
delivery shape requires under
[Implementation Done](../crw-plan/references/integrations.md#implementation-done) and C9 in
[Initiative supervision](references/initiative-supervision.md): for implementation work that is
the pull request landed in its intended target together with every accepted criterion still
outstanding beside it, installation or live verification included where the criteria named them;
for accepted non-PR work it is the agreed observable result, which has no pull request by
design. Where that evidence holds the project is finished, and its parent resting is the shape of
that rather than a failure to recover from. Where the label says completed and the evidence its
shape requires is missing, the project is
unfinished: the gap is what gets reported and the project stays in the active set, because a
status nobody corroborated is the one thing that can hide undelivered work behind a quiet parent.
A project still in progress whose returned result was never judged, or whose recorded blocker has
since cleared, continues inside the approved scope and through the task that already owns it. A
run somebody stopped stays stopped: the finding is recorded with what would release it, and
nothing is sent.

Two requests are answered from the restatement. A status-only request is answered with the current
position first — what is done, what is in progress, what is waiting, and the next observation that
would settle what is still open — and the history after it; the report's form belongs to
[crw-status](../crw-status/SKILL.md), and this task supplies the position rather than a second
format. A request to carry on re-reads the current owner and its delivery state before anything is
sent, then continues through the task that already owns the work, inside the approved scope.

The settings to restate are not all established the same way. Model, effort, profile and
permissions are compared against what the current host reports and what this task is authorized to
use now, under
[Default independent execution](../crw-plan/references/integrations.md#default-independent-execution)
and [Model and settings verification](references/bridge.md#model-and-settings-verification). The
effective workflow is not among them, for the reason the
[restoration block](references/task-packet.md#restoration-block) already gives, so a resume that
omits it has dropped it.

Ownership is settled on the stable identifiers the assignment binds
([OPS-7.1](references/operations.md#ops-71-what-an-assignment-binds)) and never on a title, a pull
request, a branch or a working directory
([OPS-7.2](references/operations.md#ops-72-never-route-on-a-display-name-or-a-working-directory)),
which is why an installation whose title or pin tooling this task cannot reach has a reporting gap
and not a lost binding. Whether a request touches the binding at all belongs to
[Resolve the project target](../crw-plan/references/integrations.md#resolve-the-project-target),
and changing it to
[Project parent binding](../crw-plan/references/integrations.md#project-parent-binding).

What the restatement must not lose is the work already in progress: dirty working trees and
branches, a native goal and its phase state, receipts that were never delivered, and pull requests
still bound to their issues. None of them is replaced by something this task creates because it
could not read an owner. Where management genuinely moves, the handoff is recorded — which task
handed over, which took it, what moved and what did not — so the next restatement reads one owner
instead of inferring two.
