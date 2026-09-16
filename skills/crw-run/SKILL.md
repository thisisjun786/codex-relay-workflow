---
name: crw-run
description: "Coordinate one Linear issue or a ready batch through independent child Codex tasks: reuse the responsible task or create one within user authorization and host rules, then verify delivery using CXC. Use for execution, progress coordination, or delivery review; use crw-plan for roadmap authoring and crw-check for intent drift. Formerly linear-run."
---

# CRW Run

Use the selected Linear project as the planning source and keep this Codex task
as its coordinator. Each implementation task owns its
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

Resolve task-creation authority as described below; a skill invocation alone
does not override the host's explicit-creation requirement. Apply the shared [delivery and integration default](../crw-plan/references/integrations.md#default-dev-integration)
and inherit existing authorization for coordination records and recovery. Release
publication, deployment, issue closure, and unrelated messages need scope covering
those actions. When the user designates this as the fixed management task,
use [crw-focus](../crw-focus/SKILL.md) for its recorded project link,
title, and sidebar pin, then continue the authorized execution here.

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
that task for compatible follow-up work under the existing assignment. A busy
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
| Request to create/reuse child tasks, a submitted prompt expressing that intent, or clear project delegation after independent tasks were established as the execution workflow | Reuse the responsible task first; create only when needed within that scope and allowed by the host, without another authorization round |
| Concrete new-task plan followed by the user's acceptance, such as “진행해” or “응” | Execute the accepted plan within its stated scope; do not ask for a creation keyword |
| Resume of an authorized run, including after compaction | Recover its authorization source, scope, and settings from the coordination/recovery record; refresh ownership and prerequisites, then continue its next ready batch without repeating approval |
| Standalone short invocation such as `$crw-run 다음 작업 진행해줘`, with genuinely no creation intent in the conversation or prior authorization for this scope | Prepare the issue packet and inspect ownership; if a new task is needed and the host requires an explicit creation request, obtain only that missing request |

The skill's name, an unsubmitted UI default prompt, a quoted example, or task
designation alone is not an explicit user request to create a task. Permission
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

Compare local and remote commit ancestry. Preserve local-only commits and dirty
work. Record a full baseline commit for each task and decide how any prerequisite
changes will reach it. Do not push shared baseline commits through every task.

Derive batches from both dependency edges and overlapping edit surfaces.
Independent issue statuses do not prove independent code changes. Serialize
shared schema, persistence, contract, or central UI changes when separation would
cost more than it saves. A small project may have only two useful parallel tasks.
Apply the shared [issue-to-PR mapping](../crw-plan/references/integrations.md#issue-to-pr-mapping):
one implementation issue per PR, with one issue/PR pair per implementation
packet. A batch retains those separate pairs. If one issue needs several PRs,
or a proposed PR would deliver several issues, reconcile the plan through
`crw-plan` before new dispatch; preserve existing owners and active work.

Keep the human-readable coordination record in the project's linked Linear
document as part of the management assignment, without a separate recording request.
For explicit read-only scope or unavailable access, return the unsynced update and
retain the task's private recovery receipt. Keep raw launch
receipts and sensitive evidence in an appropriate private location; local
snapshots point to the Linear document and are not another planning source.
Do not store project state or credentials inside this installed skill.

## Prepare and dispatch

Read [Task packet](references/task-packet.md) when preparing prompts. Each packet
must stand alone in a fresh context and name its prerequisites, scope, baseline,
acceptance criteria, verification, and return artifacts.

Every packet carries the current delivery contract, and where the template's older
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
when no new branch or worktree is needed. Reuse a checkout whose ownership is
verified for the responsible child; a coordinator may prepare it before handoff.
Otherwise follow the project/user placement convention. Record the actual path,
branch, and owner; do not create a second checkout just to prove task separation.
Placement, the ownership columns, and the write split between a coordinator that
prepares git metadata and a child that only edits source are in
[Operations contract](references/operations.md), which also owns the shared relay
service, its durable store, and how a parent returns to idle after delegating.

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

Record request ID, task ID, host ID when supplied, turn ID, checkout, full
baseline SHA, requested/actual title and settings, and launch outcome. Do not put raw
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
| Work delivered | Completed turn plus actual commit/diff and check artifacts |
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

After delivery, identify the final commit or frozen hashed diff/file bundle, then
check the prerequisite ancestry, scoped diff, acceptance criteria,
meaningful negative cases, and relevant user-visible behavior. Reuse valid proof
for the same revision and criteria; run missing checks or checks invalidated by
integration. A completed turn may contain a failure or interruption.

Where a relay holds the assignment, verify the revision it reports as current. If a
newer revision arrived while the review was in progress, the older result is not a
completion: re-read the current revision and verify that one. Two competing
revisions with no stated supersession are ambiguous, and an ambiguous head withholds
rather than picking whichever arrived later. If the criteria changed after a review,
that review certified wording nobody is judging by now, and re-review runs under a
fresh execution generation rather than by re-deciding the settled event. See
[codex-session-relay](references/relay.md).

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
