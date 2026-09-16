# Task packet and coordination record

Use this when writing a launch prompt or handing off work. Fill in actual values;
remove inapplicable items instead of sending placeholders. Embed the acceptance
criteria unless the worker's access to the linked issue has been established;
another task may not have the same connectors.

## Child task titles

Name each managed implementation task as `ISSUE-ID · short task name · workflow`,
for example `JUN-44 · 가격 조회 계약 · CXC Loop` or
`JUN-41 · 기간 집계 · CXC Loop`. These are formatting examples, not issue assignments.

Use the real Linear identifier, a concise Korean description of the assigned
result, and the agreed execution mode. Use `CXC Loop` when it is the effective
workflow; otherwise use the actual mode, such as `구현` or `검증`. The suffix names
the assignment, not proof that a loop started. Keep runtime verification separate.
Preserve an explicit user-supplied title. Each implementation packet names its
one issue and intended PR. A batch retains separate packets and issue/PR pairs;
do not use a primary issue to hide a combined delivery. If no issue is linked,
use the known project name instead of inventing an issue number and reconcile
the mapping through `crw-plan` before new implementation dispatch.

The coordinator passes the title through the creation tool's supported title/name
field and includes it in the packet. Prompt text alone does not prove the app title
was set. Read back the actual title using the returned task ID. If the host generates
or changes it, correct that same managed task through a supported title tool within
the creation/management assignment, then verify it. Keep the title on routine repair
follow-ups; do not create a replacement task to fix a name. If title control or
read-back is unavailable, record the requested title and limitation and continue
authorized work. Task identity and recovery always use stable IDs, not title matches.

## Launch packet

This packet targets a verified independent implementation task for one issue/PR
pair, or an explicitly non-PR result. Follow [Independent implementation tasks](../SKILL.md#independent-implementation-tasks)
before dispatch. A packet's wording cannot turn an internal subagent into that
task. Record the existing owner and creation/reuse authorization before sending.
For non-PR work, remove inapplicable Git/worktree/PR/OPS delivery fields and steps
below. Carry the input baseline and delivered output identity separately: stable
link plus output revision/updated-at evidence, or durable file locator plus digest.
Retain a verified snapshot when old linked revisions cannot be recovered. Retain task identity, permissions and recovery information.

```text
Task: [one issue ID and bounded result]
Parent: [one Linear project ID and verified coordinator task ID, or no project
  for a standalone issue; retain the real coordinator task ID if delegated.
  Initiative membership does not assign another project]
Issue/PR mapping: [one implementation issue ID, target repository, and intended PR scope
  or existing PR URL; related issues are dependencies, not additional deliveries.
  For non-PR work, state the result and how it will be verified]
Title: [issue ID · short task name · agreed workflow, following Child task titles]
Workflow: [effective workflow per Default independent execution]

Context:
- Repository and existing worktree: [absolute paths]
- Branch and full baseline commit: [actual values]
- Prerequisites: [verified contract/commits and how included]
- Effective model/effort: [values from the request or Default independent execution;
  actual configuration is supplied by creation]
- Coordinator: [task ID; context only, never a CXC session binding]
- Responsible child: [existing independent task/host ID, or newly created task from the launch receipt]
- Assignment route: [reuse / create; current writer state and authorization source]
- Relay assignment, when one holds this issue: [shared state directory, set as BOTH
  `--state` and `CODEX_SESSION_RELAY_STATE` on every process that reaches the socket,
  and the exact issue identity registration was given, not a display key that differs from it;
  these depend on nothing task creation returns; add the relationship id and current
  generation only where the assignment is already registered, since a newly created
  child's registration needs the task id creation has not returned yet and resolves its
  own relationship by issue lookup]
- Canonical criteria, when registered: [criterion ids and the document they came from]
- Canonical Linear documents: [IDs/URLs and observed revision/date]

Authorized execution:
- Sandbox/permission profile and approval policy: [agreed values]
- Delivery: [where the assignment covers publication, a child-owned PR opened for review, not
  left in draft: branch, pushed head, and that PR's own review cycle. Otherwise local commits or
  a frozen diff. Publication is never inferred from the delivery line alone]
- External actions: [actions covered by the assignment and shared defaults, with any narrower user limits]
- Integration owner/target: [coordinator and verified destination; copy the applicable dev default or explicit delivery limit]
- Runtime/test data access: [agreed sources and operational limits]

Outcome and scope:
[User-visible behavior, acceptance criteria, intended edit surfaces]
[Shared contracts, coordination boundaries, and necessary exclusions]

Execution:
- Read applicable project instructions and relevant source.
- You orchestrate this one issue and its internal helpers. Do not absorb another
  issue into this task or PR. The parent orchestrates one project and owns coordination,
  delivery validation, and authorized integration. Do not adopt the parent's CXC binding.
- Where the assignment covers publication and you can push, own the delivery end to
  end: implement, test, commit, push, open the pull request, then triage, fix, reply
  to and recheck its reviews. Report once the current head's required checks and
  reviews have finished and their blockers are resolved, not when the code is written.
  Without that authorization, or without the access to use it, commit locally or
  return the frozen diff and say which publication you did not perform.
- Open that pull request non-draft, or transition an existing draft to Ready for review
  as soon as the implementation is reviewable, then request the review the repository
  requires and this assignment authorizes, and confirm it actually started. An optional
  reviewer that cannot start or stalls is recorded as a gap and does not hold you;
  a required gate does. Findings, pending CI and your own revision pushes do not send
  it back to draft; fix on the open pull request and re-request a review of the current
  head. Ready is review entry, not merge permission. See
  [Publish for review when the work is reviewable](../../crw-plan/references/integrations.md#publish-for-review-when-the-work-is-reviewable).
- Maintain CXC: load current cxc-dev and relevant surface skills, and follow
  the configured CXC protocol for helpers and review within this task.
- Work in the assigned existing worktree; preserve unrelated changes.
- [Only when a relay holds this assignment:] emit your completion receipt for this
  generation over the actual deliverable paths, from inside your own turn, against
  the shared state directory. Offline that receipt is STAGED until an independent
  observation sees your turn end; that is expected and is not a failure. When you
  re-emit inside the SAME generation, state which revision your new one replaces, so
  the parent finds one current revision rather than two with neither current.
  A correction arrives as a NEW generation, and its first receipt names no predecessor:
  lineage is read within one generation, so naming the previous generation's revision
  declares a predecessor this generation does not contain and leaves it with no
  current head at all.
- [Only when a relay holds this assignment:] if you cannot emit because of the assignment's
  own state rather than your artifact, whether the issue lookup finds no assignment or the
  emit is refused with something like `unbound_generation`, stop there and report the
  completion as UNEMITTED with what you actually saw: the exact lookup result where the
  lookup came back empty, the exact refusal where an emit was rejected. Preserve the artifact
  as produced and
  return your own task id, the issue identity and the state directory you were given. Do not claim
  a receipt you could not write, do not guess a relationship id, and do not wait for the state
  to change: the coordinator closes that gap and recovers the receipt from you, on this same
  task.
- [Only when a relay holds this assignment:] if the lookup instead finds an assignment owned
  by a DIFFERENT task, that is a conflict rather than a delay and it is not recovered through
  you. Report it naming the owner the lookup returned, preserve your artifact, and write
  nothing into that relationship; the coordinator reconciles your work with that owner.
- [When CXC Loop is the effective workflow:]
  `$codexclaw:cxc-loop` — invoke the installed skill, or attach it through the
  creation tool's supported skill field, and run this bounded objective under it and
  `cxc-pabcd` using your own session binding, host goal, and goalplan.
  If a required loop capability is absent, report the exact gap before starting.
- [For an explicit non-Loop or no-goal alternative:] omit that invocation and keep
  the agreed workflow. Assignment alone does not create a goal or activate a Loop.
- Apply the agreed permissions and delivery scope. Do not modify global model or
  role settings to match the requested model.

Verification:
[Specific commands, invariants, negative cases, and real UI/API behavior]
[Allowed test data and runtime boundaries]

Return:
- Actual task ID, worktree, branch, baseline SHA, and final commit SHA if committed.
  For diff-only delivery, return the frozen diff/file bundle path and SHA-256.
- Where a relay holds the assignment and you emitted: receipt event id, revision hash, and
  the generation it was emitted under.
- Where no other task owns the assignment and you still could not emit: the completion marked
  UNEMITTED, the preserved artifact paths, your task id, the issue identity and the state
  directory, and what you actually saw. An empty lookup is reported as the lookup result itself,
  since there is no refusal to quote; a rejected emit is reported as its exact refusal, and a
  refusal like `unbound_generation` means the lookup did find this relationship and generation,
  which the coordinator needs to bind the anchor, so report both. There is no receipt to report.
  A lookup naming another owner is the next case, not this one.
- Where the lookup found a DIFFERENT owner: the same preserved artifact and identity, the
  owner the lookup returned, and that nothing was written into that relationship.
- Requested/observed task title, or the exact title-verification limitation.
- Resulting behavior and scoped changed files.
- Acceptance-criterion evidence, commands/results, and artifact paths.
- Changed contracts and what dependent tasks need.
- Requested/actual model and effort; disclose whether served-model proof exists.
- For CXC Loop: goal/goalplan identifiers, final FSM state, and completion evidence.
- Delivery artifact: [PR URL, pushed head SHA, and the state of its required checks and
  reviews, including how each finding was resolved; or the frozen diff bundle for a
  restricted or narrowed delivery].
- For a pull request: URL, base and head SHAs, `isDraft`, the review receipts for the
  current head, and any unresolved finding. Record the relay receipt's own outcome
  separately; `ready_for_review` there is not `isDraft=false` here.
- Remaining defects, unverified behavior, and possible integration conflicts.
Stop after this assigned result; do not auto-start another issue.
```

## Non-PR packet

Use this reduced shape for research, design or verification without repository changes.
Keep the shared authorization, task settings and recovery rules above; omit code-only
fields and OPS publication clauses. Relay-specific fields apply only when used.

```text
Task: [one stable issue ID, bounded result, existing owner]
Coordinator: [actual task/host IDs if delegated; project ID only if one exists]
Scope: [accepted question/outcome, exclusions, dependencies and write authority]
Input baseline: [source IDs, revisions/updated-at evidence and known gaps]
Workflow/settings: [effective workflow, model/effort and actual permission profile]
Working location: [permitted cwd/artifact roots; no invented Git repository]
Verification: [observable acceptance criteria and independent evidence needed]
Return: [actual task ID, result link plus delivered revision/updated-at evidence,
  or durable artifact locator plus digest; verified output snapshot if needed;
  criterion evidence, unresolved limitations and next handoff]
Recovery: [issue-linked record or private receipt, dispatch/turn IDs and actual owner]
Relay, if used: [exact issue identity, scope reference, real coordinator/child IDs,
  state directory and authorized recipients; current generation/receipt outcome]
Stop after this issue; do not start another issue or create an empty PR.
```

## Coordination record

Use the project's linked canonical Linear coordination document as part of the
management assignment. For a standalone issue, use its existing linked document
or an owned section in that issue and private issue/task recovery receipts. A
project binding is optional; the actual coordinator identity remains required
when delegating or routing relay delivery. Follow [Integrations](../../crw-plan/references/integrations.md#completion-follow-up-in-an-existing-execution-workflow).
For explicit read-only scope or unavailable access, return an unsynced update;
retain private recovery receipts so an interrupted task can still be reconciled. Record only what is
needed to resume:

- Coordinator task ID and fixed project or standalone issue link.
- Each implementation issue's one current PR, repository, and integration target;
  retain superseded PR links as history. Record non-PR results separately.
- Each task's scope, dependency edges, overlap decisions, and code baseline SHA
  or non-PR source revision.
- Where a relay holds the assignment: relationship id, current generation, current
  revision, assignment state, and the synchronisation jobs still owed.
- Actual worktree/branch ownership and how local-only prerequisites are preserved.
- Per mutation: stable request ID, actual task/host/turn IDs, receipt location.
- Independent task creation/reuse route, authorization source, and verified
  responsible owner; keep internal-subagent receipts distinct.
- Requested/observed child title; retain stable task IDs across title changes.
- Requested/actual settings and independent fields for launch, loop, delivery,
  verification, integration, and deployment evidence.
- Last observed status/cursor, final commit, acceptance evidence, and next action.
- For integration: candidate base/head and landed revisions, CI attempt links,
  review sources/coverage, and finding dispositions per [Merge readiness](merge-readiness.md).

Do not create a new database, daemon, or competing local planning document to hold
this table. Each worker writes its own reproducible implementation evidence; the
coordinator writes the authorized Linear summary and verifies it by reading it
back. Keep exact prompts/receipts in private evidence when they contain personal
data, and publish only the necessary sanitized summary.

For diff-only delivery, freeze a complete in-scope diff/file bundle against the
recorded baseline, including added or untracked files needed to reproduce it.
Store its SHA-256 and keep it unchanged while reviewed; a later edit is a new
artifact and needs its own evidence. A working-directory path alone is not a
stable revision.

The coordinator's review compares the exact final commit or hashed diff bundle
against the task's declared baseline and acceptance criteria. A new tip or changed dependency can
invalidate earlier proof. Record findings without silently rewriting the task's
acceptance criteria to make it pass.
