# Task packet and coordination record

Use this when writing a launch prompt or handing off work. Fill in actual values;
remove inapplicable items instead of sending placeholders. Embed the acceptance
criteria unless the worker's access to the linked issue has been established;
another task may not have the same connectors.

## Child task titles

Name each managed issue task as `ISSUE-ID · descriptive task title`,
for example `JUN-44 · 가격 조회 결과를 검증한다` or
`JUN-41 · 기간별 사용량을 집계한다`.
These are formatting examples, not issue assignments.

Use only the real Linear identifier and a Korean description that makes the
assigned result clear. The title after ` · ` may be a natural sentence or phrase
of up to 20 characters, including spaces and punctuation; the issue code and
separator do not count. Do not abbreviate away the task's meaning just to make
it shorter, or pad a clear title to reach 20 characters.
Do not append workflow labels (including `CXC Loop`), repository,
PR number, model, CI status, or merge status. Keep the execution mode in the
packet's `Workflow` field and verify runtime behavior separately. A title or a
chat link does not establish the task's native PR association; keep PR linkage
separate from naming. An explicit user-supplied title takes precedence over
this default. Each implementation packet names its
one issue and intended PR. A batch retains separate packets and issue/PR pairs;
do not use a primary issue to hide a combined delivery. If no issue is linked,
use the known project name instead of inventing an issue number and reconcile
the mapping through `crw-plan` before new implementation dispatch.

The coordinator passes the title through the creation tool's supported title/name
field and includes it in the packet. Prompt text alone does not prove the app title
was set. Read back the actual title using the returned task ID. If the host generates
or changes it, correct that same managed task through a supported title tool within
the creation/management assignment, then verify it. Keep a compliant title on routine
repair follow-ups; normalize a legacy workflow or status suffix on the same task
rather than creating a replacement task to fix a name. If title control or
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
Supervisor: [the initiative ID and task ID of THIS project's designated execution supervisor,
  where one exists; context only. A project contributing to several initiatives still has exactly
  one, and the others are not named here because they only reference its outcome.
  A supervisor works through your parent and is not a route into this task]
Issue/PR mapping: [one implementation issue ID, target repository, and intended PR scope
  or existing PR URL; related issues are dependencies, not additional deliveries.
  For non-PR work, state the result and how it will be verified]
Title: [issue ID · descriptive title of up to 20 characters, following Child task titles]
Workflow: [effective workflow per Default independent execution]

Context:
- Code target: [verified GitHub owner/repo or URL; explicit none for non-code work]
- Reference-only repositories: [if any, not assigned edit targets]
- Repository and existing worktree: [absolute paths, when applicable]
- Remote name/URL and intended integration branch: [verified repository-policy values]
- Issue branch and full baseline commit: [actual values; new assignment or preserved resume]
- Prerequisites: [verified contract/commits and how included]
- Effective model/effort: [values from the request or Default independent execution;
  actual configuration is supplied by creation]
- Coordinator: [task ID; context only, never a CXC session binding]
- Responsible child: [existing independent task/host ID, or newly created task from the launch receipt]
- Assignment route: [reuse / create; current writer state and authorization source]
- Management marker: [the stable creation request id this launch was issued under, per
  [Prepare and dispatch](../SKILL.md#prepare-and-dispatch)]
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
  Where an initiative above it has a supervisor, that supervisor works through your parent:
  it does not instruct you, and you report to your parent. See
  [Supervisor, parent and child scope](../../crw-plan/references/integrations.md#supervisor-parent-and-child-scope).
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
  On a correction or a resume, read whether that goal and goalplan exist before making
  either. Usually they do, and the work is to continue them: opening a second goal for
  the same assignment is a duplicate rather than a resume, and the coordinator reads it
  as one. Where activation never succeeded there is nothing to continue, and starting it
  then is the activation that was owed rather than a duplicate. Say which of the two
  happened, because from the outside they produce the same new goal.
  Confirming identity means reading it rather than assuming it: your own native task id,
  the native working directory you are actually in, the source checkout that directory
  belongs to, and your real recorded workflow state. Those four can disagree after a
  resume or a compaction, and the disagreement is the thing worth catching. A session
  binding is identity, not proof that a hook ran. Where a relay holds the assignment its
  generation is read from the assignment by lookup, never inferred from your own state
  and never copied from the coordinator's.
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
- Proposed changes to a Linear record, returned rather than written: the document or issue ID,
  the revision you read, the reason, the smallest sufficient change and its evidence.
Stop after this assigned result; do not auto-start another issue.
```

## Non-PR packet

Use this reduced shape for research, design or verification without repository changes.
Keep the shared authorization, task settings and recovery rules above; omit code-only
fields and OPS publication clauses. Relay-specific fields apply only when used. When using the relay, freeze the result
and its verification evidence in a file under an authorized artifact root and emit
that file; link-only completion has no manifest and is not a valid ready receipt.

```text
Task: [one stable issue ID, bounded result, existing owner]
Title: [issue ID · descriptive title of up to 20 characters, following Child task titles]
Coordinator: [actual task/host IDs if delegated; project ID only if one exists]
Scope: [accepted question/outcome, exclusions, dependencies and write authority]
Input baseline: [source IDs, revisions/updated-at evidence and known gaps]
Workflow/settings: [effective workflow, model/effort and actual permission profile]
Working location: [permitted cwd/artifact roots; no invented Git repository]
Verification: [observable acceptance criteria and independent evidence needed]
Return: [actual task ID, result link plus delivered revision/updated-at evidence,
  or durable artifact locator plus digest; verified output snapshot if needed;
  requested/observed title or title-verification limitation;
  criterion evidence, unresolved limitations and next handoff]
Recovery: [issue-linked record or private receipt, dispatch/turn IDs and actual owner]
Relay, if used: [exact issue identity, scope reference, real coordinator/child IDs,
  state directory and authorized recipients; frozen result/evidence artifact path
  and digest under an authorized root; current generation/receipt outcome]
Stop after this issue; do not start another issue or create an empty PR.
```

## Project handoff

Use this when a supervisor hands one of its approved projects to that project's parent. It is the
brief for one project and it stops there: it carries no issue plan, no child assignment and no
review of anything below that parent. The supervisor's own procedure is
[Initiative supervision](initiative-supervision.md).

Two carriers, chosen by whether the parent exists. A project with no parent yet receives this as
the first prompt of the new task, prefixed with its project designation so that task runs its own
[Project parent binding](../../crw-plan/references/integrations.md#project-parent-binding). An
existing parent receives it inside a [Coordination message](#coordination-message) of kind project
handoff, carrying the [restoration block](#restoration-block) when its turn is already running.
Write it in English, like every instruction that travels between tasks.

Keep it short and point at what the existing records already hold.

```text
Initiative / Supervisor: [stable initiative ID, and this supervisor's task id]
Project: [stable project ID and URL, and the parent task id where one already exists]
Criteria: [the project record revision read, and this project's contribution to the initiative's
  finish condition. Issue-level criteria stay in the issues]
Prerequisites: [cross-project prerequisites by relation, each with where its verification will
  appear; and the peer parents this project shares a surface with, to settle with directly]
Authority: [the designation and its date, the limits in force, child-creation authority, and the
  delivery scope: this parent merges its own project's pull requests, while release and
  deployment stay the user's]
Current state: [locators only: the project's coordination record, its live children, its open
  pull requests, and any outcome already verified]
Report back: [a result or blocked coordination message carrying the outcome, the evidence per
  criterion, the unresolved problems and the decisions needed. Detailed logs stay where they are]
Workflow: [the parent's effective workflow, restated because no transport carries it]
```

A handoff that was sent is not a parent bound, and a handoff that was accepted is not a project
delivered. The parent's own binding record and its first returned result are those two facts.

## Coordination message

Use this between parents coordinating directly, between two supervisors coordinating across their
initiatives, for a parent's escalation to its supervisor, for a supervisor's decision returning to
them, and for a supervisor handing one of its approved projects to that project's own parent. It
carries coordination between owners; it is never a route into anyone else's children.
It is not a delivery channel: it carries no receipt, no acknowledgement and no verdict, and it
never instructs another parent's child. The handoff is the one kind here that assigns anything, and
even it assigns only a project to the parent that owns it; that parent binding the project and
returning its result are separate facts the message does not establish. The path and the rules it follows are
[Direct coordination between parents](../../crw-plan/references/integrations.md#direct-coordination-between-parents).

Keep it short. Name what identifies this message, and reference what the existing relationship
already holds instead of recopying it, exactly as the restoration block carries pointers rather
than contents.

```text
Request: [id the sender chose for this message]
Reply to: [on a reply, the id it answers; omit on a first message]
Kind: [proposal | acceptance | conditional acceptance | rejection | correction | result | blocked |
  merge turn request | merge turn assignment | merge turn return | recovery update |
  project handoff.
  The three merge-turn kinds are about the order into a shared target and nothing else: no kind
  here lets one parent assign work to another, because no parent can. Project handoff is the one
  downward assignment, it belongs to a supervisor alone, and only that project's own parent
  receives it]
From / To: [each side's role, task id and Linear scope]
Scope: [the issues, files, interfaces or behaviour this is about, and the base revision]
Asking: [the action or decision required, or the decision being returned]
Because: [where the reason lives: the finding, the pull request, the criterion, the receipt]
Next: [who owns the next step, and what would settle it]
```

A worked pair. The long values stay as references, and the reply is conditional, so it is recorded
as conditional rather than as evidence that anything was applied:

```text
Request: shared-surface-1
Kind: proposal
From / To: parent of project A, task 01a0...a1; to parent of project B, task 01a0...b7
Scope: A's CRW-127 and B's CRW-131 both edit references/operations.md; base dev 89c2c58
Asking: B holds OPS-7 until A's clause lands, and A leaves OPS-8 untouched
Because: overlapping edit surfaces recorded in both projects' coordination records
Next: B, to accept or to name its own constraint

Request: shared-surface-1-r1
Reply to: shared-surface-1
Kind: conditional acceptance
From / To: parent of project B, task 01a0...b7; to parent of project A, task 01a0...a1
Scope: same two issues, same base revision
Asking: nothing yet; accepted on the condition that A lands before B's own review opens
Because: B's child already has a branch at that base
Next: A, to report its landing. Until then this is conditional and is not applied evidence
```

The record of what happened next belongs in the coordination record below, not in another message:
B's parent instructing its own child, that child's change, and the verification are three further
facts, and none of them follows from this reply.

## Restoration block

Every message into a task that is already running carries this block: a needs-changes
correction, a review fix, a resume the coordinator publishes after handling something
on the task's behalf, and a restart after that task was compacted. The first work
prompt stated the assignment once. Ten turns, one compaction and three review rounds
later, none of it is reliably still in the task's context, and a correction that
assumes otherwise is answered from whatever the task still happens to remember.

Keep it short. It restates what the COORDINATOR holds and what the task cannot
reconstruct alone:

- The skills this task runs under, as pointers to the installed skill, not their text.
  On context loss the task re-reads the owning skill from those pointers; it does not
  reload every skill it once had, and an unrelated reference is not part of recovery.
- The effective workflow, restated. A transport carries model and effort as settings
  and has no field for the workflow, so a send that omits it has silently dropped it.
- Assignment identity: the issue, this task's own id, and where a relay holds the
  assignment its relationship id and the generation to emit under, read from the
  assignment rather than copied from the coordinator's own state. On a needs-changes
  correction that is the generation the verdict opens and not the one being superseded,
  because the verdict is what opens it: a block composed beforehand that names the
  current generation names the one the child has just stopped working in, and a receipt
  emitted under it is refused. The relay carries the superseded event and its digest
  itself, so the block does not repeat them.
- The delivery artifact as it stands now: pull request URL, base and head, and which
  required checks and reviews are outstanding on that head.
- The unresolved findings, each with what would settle it.
- The single next action this message is asking for.
- Durable locators for the work the task itself owns: where its plan, its ledger and its
  evidence live, with the identifiers and current revisions the coordinator already holds.
  These are pointers to that task's own records rather than copies of them, and they are what
  lets a task that lost its context find its record instead of starting a second one.

What the block carries about those records is their location and identity, never their
contents. The plan, the ledger, the phase and the goal belong to the task and to its own
workflow skills, they are durable on that task's side, and this repository deliberately
does not define their shape
([OPS-10.3](operations.md#ops-103-where-the-packet-and-report-formats-are-defined)).
Pointing at a record is not defining it; rewriting one is. A coordinator that reconstructs
a child's plan from its own view has replaced that child's record with a guess, and the
child will trust the guess over the record it could have re-read.

The block is context for resuming work already authorized. It requests no new approval,
asks for no readiness-only turn spent confirming receipt, and re-opens nothing the
assignment already settled.

Where a relay holds the assignment there is no second channel to put it on: the
needs-changes verdict is the correction, so the block travels in that verdict's own
findings and notes. Compose it before recording the verdict, because afterwards the
only remaining routes are the ones this workflow forbids, and write the generation the
verdict is about to open rather than the one still current as you write. Those two are
never the same on a correction, and the child acts on the one it was given.

Declare which finding carries it, and put it in the first one. The declaration is what
makes a failure visible; the position is what makes failure unlikely, and the two are
different jobs. A relay that recognises the declaration refuses a correction it cannot
carry BEFORE that correction opens the next generation, so the transaction the coordinator
used to be unable to get back to is one it can now simply not enter;
[codex-session-relay](relay.md#the-parent-verifies) records the flag and the outcomes it
names.

What that establishes is what the relay will put in the bytes, which is a different claim
from the child having read them, so delivery of the block still needs evidence rather than
following from placement. Confirm it from a dispatched attempt and what the child actually
received, never from a queued rendering, which is the bytes a next attempt would send rather
than proof of a send. A relay that records what each attempt froze reports that as
`restoration_attempted` beside the bytes, which is the one measurement about a send rather
than about the next one. Where the block did not arrive, say so plainly: there is still no
supported way to send it again, because the verdict does not resend and the parallel route
stays forbidden. Record it as an undelivered correction on that assignment and hand it to the
coordinator, whose decision it is: to let the child proceed on the context it has, or to
change the relay. Do not invent a transport to close the gap, and do not describe that
correction as delivered. Whether an installed relay recognises a declared restoration block
at all is a property of that installation, and it is asked rather than assumed: the command
either offers the declaration or rejects it as unknown, which is what
[codex-session-relay](relay.md#the-parent-verifies) tells you to check before relying on it.
A version cannot answer, since it stays the same while contents change, so `unmeasured`
belongs to an installation nobody asked, not to every installation but one.

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
- Per direct agreement with a peer parent: the request and reply ids, the counterpart parent and
  its project, the agreed area and base revision, whether the agreement is still conditional and
  on what, and whether a follow-up owner has explicitly accepted. Beside it keep what is still
  open, because that is what recovery reads first: each request sent and not yet answered with
  what would settle it, the revision the agreement stands on, who owns the next step, and each
  follow-up still marked unassigned, raised as a blocker where it is a required dependency. For a
  condition this project accepted, record the child that was instructed and the revision where the
  change took effect, which is the adoption evidence the peer is owed.
- Where this task supervises an initiative: the stable initiative ID with the body revision its
  finish condition was read at, the approved project set with each project's parent task id and
  observed state, the handoff request id sent to each parent and the last result id returned, the
  decisions still owed upward, and the limits the designation imposed. See
  [Initiative supervision](initiative-supervision.md).
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
