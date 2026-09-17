---
name: crw-run
description: "Run a Linear project, milestone, ready batch, or single issue through independent child Codex tasks: reuse the responsible task or create one within user authorization and host rules, then verify delivery using CXC and continue within the agreed scope. Use for execution, progress coordination, or delivery review; use crw-plan for roadmap authoring and crw-check for intent drift. Formerly linear-run."
---

# CRW Run

Use the selected Linear project as the planning source and keep this Codex task
as its coordinator: one parent per project, one child per issue. Follow the shared
[parent and child scope](../crw-plan/references/integrations.md#parent-and-child-scope),
including standalone issues and explicit current-task work. Each child owns its
checkout and execution; this task owns scope, dependencies, dispatch receipts,
review, and the decision to release the next work.

Keep the CXC harness in both coordinator and child tasks. Load the installed
`codexclaw:cxc-dev` and relevant surface skills in each task for its work.
When CXC Loop is the effective workflow, the child loads the installed
`codexclaw:cxc-loop` and `codexclaw:cxc-pabcd` and runs its own goal, goalplan,
and phases under them. Resolve current installed paths; never copy a plugin
version from old records. Those skills own CXC phases and subagent routing. This
skill does not replace their FSM or create a second orchestration system.

Read [Integrations](../crw-plan/references/integrations.md) for Linear document
authority, tools, and Paperthin invocation rules. Use the available Linear tools
and relevant workspace Agent Skills for full documents, criteria, and work records.
Use `readchk` for bundled scope or model corrections, and `catchup` when the user
needs a refreshed briefing. Route roadmap authoring to
[crw-plan](../crw-plan/SKILL.md) within the user's requested scope.

## Determine the requested operation

When the user submits `$crw-run <Linear project link>` as the execution request,
with no narrower operation, treat it as the full project-run request: designate
this task as the fixed parent through `crw-focus`, run the agreed project scope
with CXC Loop, reuse each issue's responsible child or create an independent
child when needed, verify its delivery, perform authorized integration, and
continue with ready work. This submitted shorthand requests those actions; no
expanded prompt or creation keyword is needed. Resolve ownership and agreed
scope first; the link does not approve undefined work or replace another parent.

Explicit status, explanation, plan-only, batch, issue, no-create, no-goal,
no-merge, or current-task limits override the default. An issue or milestone
target stays within that scope; it is not a request to run its whole project.
A quoted example, a question about usage, automatic skill selection, or an
unsubmitted UI prompt does not activate this shorthand. Host and tool restrictions
still govern each action, including task creation and goal activation.

- **Plan/prompts:** inspect project state and prepare task packets; do not launch.
- **Dispatch a named batch:** reuse prior authorization and settings, refresh its
  prerequisites, then launch only that batch.
- **Run a project or milestone:** derive successive ready batches within the
  requested scope and continue after their prerequisites are verified. Do not
  reduce a whole-scope run to the first batch or require approval for each successor.
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
use [crw-focus](../crw-focus/SKILL.md) for its recorded project link,
title, and sidebar pin, then continue the authorized execution here.

## Keep a project run moving

For a requested project Loop, load the installed `cxc-loop` and `cxc-pabcd`
in this coordinator too. Under their current host/session rules, inspect and
reuse its matching goal and plan, or establish them when authorized. The parent's
objective is coordination and verified delivery of the agreed project scope;
each child retains its separate issue goal, session and phases. Never borrow a
child's state as proof of the parent's Loop. Missing required Loop capability
is a reported gap, not an ordinary turn relabeled as an armed Loop. Plain
dispatch, status, binding, and discussion of a future Loop do not arm one.

Record the selected project, authorized issue/milestone scope, delivery limits,
active child IDs, and next dependency in the existing coordination record.
Newly discovered work must fit that scope; an overnight run is not authority to
adopt every future issue or another project's backlog. Preserve explicit batch,
no-goal, no-merge, pause and resource limits.

Repeat within that scope: refresh ownership and prerequisites, actively look
for independent ready issues using the [parallel scheduling rules](#establish-the-project-baseline), dispatch ready
issue packets, observe their actual results, verify and integrate permitted
deliveries, then release the next ready work. A blocked issue holds only its
dependents; continue independent authorized work. Keep each implementation issue
with its own child and delivery PR, and serialize integrations into a shared
target. Non-PR work completes on its verified result.

Choose and record the observation mode before creating or registering children, using
[OPS-8.1](references/operations.md#ops-81-parent-continuation-and-waiting)
to check active-parent receipt compatibility. Refresh existing assignments first;
do not switch a registered issue to another delivery path. An active parent Loop
uses bounded transport waits and continues
after timeouts; it does not rely on a hook or an unverified relay to wake it after
ending its turn. Report meaningful progress while respecting host communication
limits. A quiet worker, elapsed wait or empty ready queue is not completion while
owned work is still running. Do not resend work or create replacement writers
because a wait expired.

Finish only after the agreed scope is verified, the user stops it, a stated
resource limit is reached, or no authorized action remains because of a concrete
blocker. Record unfinished issues, responsible tasks and the exact resume step;
keep host goal completion/blocked rules with CXC and the host tools. Never mark
the project complete merely because the current batch ended. If the host ends
the turn or no supported observation/resume path remains, report continuity as
unverified or interrupted rather than promising unattended progress.

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
`duplicate_assignment` has therefore just created a losing writer, and the transport
offers no way to interrupt it: the bridge's observed capabilities are creation,
messaging, read/list/wait and goal reads, and a follow-up message refuses an active
task. So assign it nothing further, wait for it to stop being active, then tell it to
stand down and preserve its artifact, and reconcile that artifact with the owner the
refusal named. Treat its writes as unreviewed until then, and do not describe stopping
it as something already done. Commands are in
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
| Submitted `$crw-run <Linear project link>` execution request with no narrower operation | Apply the project-run default above, including fixed-parent designation, Loop and child creation/reuse; no expanded prompt is required, and host restrictions still apply |
| Request to create/reuse child tasks, a submitted prompt expressing that intent, or clear project delegation after independent tasks were established as the execution workflow | Reuse the responsible task first; create only when needed within that scope and allowed by the host, without another authorization round |
| Concrete new-task plan followed by the user's acceptance, such as “진행해” or “응” | Execute the accepted plan within its stated scope; do not ask for a creation keyword |
| Resume of an authorized run, including after compaction | Recover its authorization source, scope, and settings from the coordination/recovery record; refresh ownership and prerequisites, then continue its next ready batch without repeating approval |
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
create a project or call `crw-focus` to satisfy this baseline. If a transport
requires a project binding, use a permitted projectless execution path or report
that capability gap; never invent a project ID for a receipt.

For repository-changing work, compare local and remote commit ancestry. Preserve local-only commits and dirty
work. Record a full baseline commit for each task and decide how any prerequisite
changes will reach it. Do not push shared baseline commits through every task.

At initial dispatch and after a completion, blocker, or integration, scan the
remaining agreed scope for useful parallel work rather than selecting only the
next issue. Check verified prerequisites, overlapping edit surfaces, existing
writers, shared runtime resources, and available execution capacity. Dispatch
the largest useful set of independent ready issues within explicit concurrency,
budget, and host limits. A shared repository alone is not a reason to serialize;
separate owned checkouts can carry independent changes.

Do not wait for an entire batch to finish before filling available capacity with
newly ready independent work. Independent issue statuses alone do not establish
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
`ISSUE-ID · short task name · workflow`. Supply the title through the supported
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

## Observe and verify

Use a compact native `wait_threads` snapshot with each task's actual host and
cursor, or the bridge's exact task/turn wait. Batch independent reads. A timeout
does not stop or complete work; it is not a reason to send the prompt again.
Observe the host's wait limits and keep the user informed without repeating
unchanged snapshots.

Describe evidence separately:

| Claim | Evidence required |
|---|---|
| Independent child assigned | Creation or reuse receipt from the independent-task interface, actual task/host ID and ownership; not an internal-subagent handle |
| New task created | Creation receipt and recoverable task ID; reuse is recorded separately |
| Prompt dispatched | Accepted turn ID and matching user message |
| Requested settings applied | Actual returned settings, not prompt text |
| CXC Loop active | Child's active goal and current goalplan/FSM evidence |
| Work delivered | Completed turn plus actual commit/diff and checks for code; verified result with both input baseline and delivered output revision/digest for non-PR work |
| Pull request review handled by the child | Per-finding trail on that PR: the finding, the commit that addressed it, and the recheck |
| Child reports normal completion | Required checks and reviews finished on the current head, blocking findings resolved; a missing mandatory review or check is blocked, not complete |
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
fresh execution generation. Which one applies turns on whether the artifact itself
has to change. See [codex-session-relay](references/relay.md).

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

Report **verified**, **needs changes**, or **unverified**, with concrete evidence,
and distinguish implementation, merge, and deployment. Start a successor
only after its required contracts/revisions are verified and available in its
checkout, and only within the authorized batch/run scope.

After integration, apply [Implementation Done](../crw-plan/references/integrations.md#implementation-done)
before reporting or recording the issue complete. Read back the one delivery PR's
actual merge, intended repository/branch and landing revision. For legacy multi-PR
scope, verify the reconciled deliveries and their combined coverage instead. Retain
existing accepted operational criteria and never infer completion from an automatic status alone.

## Return corrections to the existing task

Apply the completion-follow-up rule in [Integrations](../crw-plan/references/integrations.md#completion-follow-up-in-an-existing-execution-workflow).
Send an actionable packet to the existing responsible task without asking Jun to
approve or relay routine in-scope corrections. Include the issue and criterion,
reviewed revision, expected and observed behavior, reproducer/evidence, required
outcome, and focused verification. For missing proof, request that verification
without prescribing an unsupported code change.

Where a relay holds the assignment, the needs-changes verdict IS the correction: it
opens the next execution generation and queues the revision request to the same
registered child, carrying the superseded event, its digest, and the per-criterion
findings. Record findings with notes, because a correction with no findings is one
nobody can act on. Do not create a task, a second relationship, or a parallel
message path to deliver it. Without a relay, send the packet as described above.

Refresh the task's identity, ownership, current turn, checkout, and prior
correction receipts before sending. Reuse its agreed model, effort, permissions,
and delivery scope. Use the transport's supported follow-up path; if it cannot
accept a message while the task is active, observe until it can. A timeout or an
uncertain send requires reconciliation, never a duplicate writer or blind resend.
Report a concrete access or ownership blocker when no supported path is available.

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

A final update gives task links/IDs, requested and verified settings, actual
progress, review outcome, and the next actionable dependency. Use the host's
created-task directive when required. State remaining capability gaps plainly.
This skill does not install a recurring monitor: use the automation tools only
when ongoing background monitoring is requested.
