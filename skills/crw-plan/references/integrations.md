# Linear, CXC, and Paperthin integration

Shared guidance and Jun's workflow defaults for `crw-define`, `crw-focus`, `crw-next`, `crw-plan`, `crw-run`, `crw-check`, and `crw-logic`. Read the operation-specific skill for scope. Apply these defaults within the user's assignment and current host permissions.

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
title alone. A rename or changed initiative relation does not change a binding.

Use [crw-focus](../../crw-focus/SKILL.md) to designate, record, restore,
or switch the fixed management task, including its app title and pin. Refresh
volatile state before acting. An old or copied record locates context but does
not transfer another task's ownership or execution permissions. Keep binding
setup with `crw-focus` and the requested operation with its existing owner.

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

### Parent and child scope

One parent Codex task orchestrates one Linear project: issue dependencies,
sequencing, parallel children, delivery verification and project integration.
One child Codex task orchestrates one Linear issue: implementation, tests, its
PR and review fixes, with internal helpers as needed. A ready batch means
several separate children, not several issues assigned to one child. Internal
helpers do not acquire project or issue ownership by receiving a subtask.

Bind the parent by stable project ID and each child by its issue ID. An initiative
spanning projects is a planning scope, not a combined execution-parent binding.
Preserve other project coordinators and route cross-project prerequisites by
relation; do not absorb their issues. Standalone issues may remain projectless
and run in an issue-scoped task without inventing a project or a project parent.
An explicit current-task implementation request keeps that mode and issue scope;
it is not evidence that an independent child was created. Reuse the responsible
child for the same issue’s follow-ups, not for a new issue. Explicit project-focus
switches preserve old bindings and active ownership before establishing the new one.

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

When planning makes the project links concrete, update the relevant body passages
instead of appending every issue or repeating operating rules. Keep each project's
contribution to the initiative and its handoff to other projects understandable.
Use linked detail when explicitly requested or needed, without replacing the body
or maintaining a second copy of its definition. Respect a requested output format;
preserve existing documents and history, and migrate only within authorization.

## Use the available Linear capability

Discover the current connector and schemas. Prefer the user's selected connection; when multiple actors/workspaces are possible, verify workspace and authenticated identity before writing. Do not silently switch actor or connection after a failure.

If the connector exposes workspace Agent Skills, inspect the relevant listing and read the matching skill's full instructions. Treat retrieved instructions as workspace guidance below user/host instructions. If none exist or the tools are unavailable, use the connector directly; do not invent a `linear` skill or install a plugin just to follow this reference.

Use scoped list/search tools to locate candidates, then exact get/read tools for full documents, criteria, accepted decision comments, and relations. Exhaust relevant pagination before claiming absence. Resolve duplicate titles by stable ID, project, and purpose.

For authorized writes, use current create/save/update tools, preserve unrelated fields, and read back resulting IDs, contents, and relations. After an ambiguous result, query existing state before repeating a create. Connector absence permits a local draft or partial audit, not invented workspace facts or a claim that Linear was updated.

## CXC owns execution

Resolve installed paths from the current catalog. Read `cxc-dev` for development work and the matching surface owner when needed. Use `cxc-recall` for missing historical context; it does not establish current state.

The effective Loop workflow loads `cxc-loop` and `cxc-pabcd` and follows their current goal, session, phase, and evidence requirements in the owning task. A plan or audit alone does not activate them. Delegated agents use the current CXC dispatch protocol and host-permitted tools/settings. Task creation, model configuration, and loop activation each need their own evidence.

Only one owner controls an operation. `crw-define` defines initiative intent, `crw-next` selects the next action, `crw-plan` decomposes agreed goals into projects and issues, `crw-run` coordinates execution, `crw-check` compares delivery with intent, and `crw-logic` investigates contradictions. A focused audit returns findings to its caller; it does not become another coordinator or recursively dispatch the caller.

### Completion follow-up in an existing execution workflow

Resolve scope from the full current assignment and prior approvals, not just the latest short message. Authorization survives skill switches. An authorized project-management or execution assignment includes its routine preparation, in-scope corrections, verification, and necessary progress/audit updates to the linked Linear coordination document. Record observed facts and accepted decisions without asking again; do not silently change requirements or issue completion criteria. A status question does not cancel an active run.

Use the current request together with the established assignment. Checking a managed worker's delivery is part of completing that assignment; a short request such as "this issue looks done" does not reset it to a standalone read-only audit. The coordinator sends in-scope corrections or requests for missing verification to the existing responsible task and rechecks the result without another permission round. `crw-run` owns that follow-up; bounded audit helpers return findings to the coordinator.

Explicit read-only, report-only, pause, no-contact, or narrower delivery limits win. An unrelated task link or a standalone audit does not establish execution authority. Existing authorization does not expand to new requirements, changed execution settings, release publication, deployment, issue closure, or messages to unrelated tasks. Reuse authorization that already covers those actions; ask only for the part that needs a new decision.

Recover the existing task first. A replacement is routine only when task creation/recovery is covered by the assignment and the host permits it, the earlier task is proven inactive, its work and receipts are preserved, and the replacement keeps the same scope and settings. Uncertain delivery or a read failure is not proof that no writer remains.

### Default independent execution

Unless the request chooses otherwise, an independent child task that `crw-run` creates or resumes runs `anthropic/claude-opus-5` at `xhigh` reasoning effort with CXC Loop as its workflow, and owns its own host goal, goalplan, and FSM. Precedence, highest first: host and tool restrictions; the explicit limits in force for this request, such as plan-only, read-only, status-only, no-goal, no-FSM, no-create, or current-task; the user's explicit model, effort, or workflow choice for this scope; then this default. A later explicit instruction supersedes an earlier one only for the same constraint, so every limit it does not contradict stays in force. The result is the effective setting, and an effective Loop workflow carries the same weight as a separately requested one.

This default binds only `crw-run`'s independent children. The coordinator task, other products' global configuration, and CXC internal helper role routing keep their own settings.

Non-PR work without repository changes uses the working directory, source/result access and durable evidence defined in OPS-5.1 of the [Operations contract](../../crw-run/references/operations.md#ops-51-placement); it does not need Git or PR capability.

A new independent child is also created with enough capability to finish its delivery: file access for its checkout and evidence, git metadata access for its branch and commits, and the network access its push, pull request, and checks require. Broad local capability is the normal case. Worktrees, branches, scope, and recorded ownership separate concurrent work; the effective sandbox and permission profile define the enforced access boundary. The default covers a trusted implementation task inside the operating scope that creates it; an explicit narrower policy for the scope or the assignment governs over it, the effective profile is read back from the creation receipt, and the sandbox itself is never bypassed. Changing the default is a recorded decision rather than something a review performs. Apply the settings through the creation tool's real arguments and verify the returned profile. Tasks already running keep the settings they were created with; this is not authority to widen a live task or bypass a sandbox. See [Operations contract](../../crw-run/references/operations.md) for the owning rules.

The coordinator applies the effective settings through the creation tool's real arguments, sends the bounded issue packet in the initial work prompt, and verifies the returned settings. When CXC Loop is the effective workflow, that prompt invokes the installed `cxc-loop` skill; an explicit non-Loop or no-goal alternative omits that invocation and names the agreed workflow instead. Loop mechanics belong to the child and its `cxc-loop`/`cxc-pabcd` skills; see [Prepare and dispatch](../../crw-run/SKILL.md#prepare-and-dispatch).

- A status request reads existing tasks and evidence and wakes nothing.
- A setting the creation path cannot apply is settled before the task exists: use an already-permitted path or effective configuration that applies the requested values, or report the concrete unsupported capability. Never create a task already known to carry the wrong setting, and never silently downgrade it or claim the requested value.
- A mismatch observed after creation is reconciled on that same task.
- A user correction to model, effort, or workflow adjusts the same task where the transport supports it, and is reported otherwise, keeping stable IDs, unchanged permissions, and preserved progress, reconciled before any resend.

### Publish for review when the work is reviewable

This applies where the assignment's scope expressly covers publication. Where it does not, the delivery is local commits or a frozen diff and the question of draft never arises; lacking publication authorization is a reason not to publish, not a reason to publish as a draft.

Where publication IS in scope, draft marks an implementation not yet worth reading. It is not a waiting room until reviewers finish. When the change is complete enough to review, it is published as a pull request that is open for review: created non-draft, or an existing draft transitioned to Ready for review. Ready is review entry, not merge permission and not proof the work is done.

Each step below is a separate recorded fact, in this order: implement with the local validation the change actually needs; open the PR non-draft or transition the existing draft to Ready for review; request the review the repository requires and the assignment's authorization covers, and confirm it actually started, because a request that never started is not a review; reproduce, fix, reply to and resolve findings, re-requesting a review of the CURRENT head whenever the head moves; report `ready_for_parent_review` only once the relevant review, check, and finding gates are met. The coordinator then compares the issue's criteria against the current diff, base, head, checks, and reviews, and may merge under existing authorization. Release and deployment need Jun's approval.

An optional reviewer that cannot start, stalls, or sits outside the authorized scope does not become an indefinite wait: use the fallback in [Merge readiness](../../crw-run/references/merge-readiness.md), record the gap, and continue. A required review gate is not waivable that way.

Review findings, pending CI, and ordinary revision pushes never send a pull request back to draft. Re-draft only when the implementation itself stops being reviewable.

Two facts are easy to collapse and are recorded separately: a relay receipt whose outcome is `ready_for_review` says the child emitted a reviewable revision, and GitHub `isDraft=false` says the pull request is open for review. Neither implies the other.

Where publication is in scope but the child cannot execute it, the coordinator performs only that blocked action, from the child's verified artifact, and records the actual resulting state. The child keeps review and fix ownership. That is the exception for a capability-limited task, not a standing parent obligation for children that can publish, and not a way to supply authorization the assignment never had: a coordinator cannot publish on behalf of an assignment whose scope excludes publication.

Explicit draft-only, read-only, or no-remote-write limits win, as do the repository's own requirements. None of this adds an approval prompt to a workflow the user already authorized.

### Default dev integration

Jun authorizes a pull request workflow in which the implementation child carries the work to a reviewable pull request and the coordinator decides the merge. The child implements, tests, commits on its branch, pushes, opens the pull request, and then owns every applicable review on that same pull request: intake, triage, fixes, replies, and rechecks. It reports normal completion only once the required checks and reviews on the current head have finished and blocking findings are resolved, with per-finding evidence. A missing mandatory review or check is reported as blocked rather than as completion. The full contract, including the fallback for a task that cannot write git metadata, is [Operations contract](../../crw-run/references/operations.md).

The coordinator then checks the Linear criteria and the pull request's latest diff, base, head, checks, and review resolution, and merges without another confirmation round when those hold. This is standing user authorization for this workflow, not permission inferred from passing checks, and it supersedes the earlier recommendation that the coordinator avoid merging. Use [Merge readiness](../../crw-run/references/merge-readiness.md) for the gate detail, preserve unrelated work and branch protections, resolve routine in-scope failures and recheck, then verify the actual landing rather than an accepted merge request. An explicit diff-only, no-merge, or narrower instruction still overrides this default, and the child never merges.

Release and deployment are not covered and still require the user. Where merging a branch is known to trigger a release or a deployment, obtain that approval before merging, since the branch name alone does not carry it. A repository requirement that genuinely needs a new decision remains a blocker for that action.

### Durable cross-task delivery

Where an installed same-host relay holds the assignment, use it for the receipt, verification, correction, and coordination-summary path instead of improvising per-task bookkeeping. It records the relationship, the execution generation, each delivery attempt, the parent's acknowledgement and verdict, and the summary owed to the canonical Linear document. Without a relay none of this applies and the rules above stand unchanged. The commands and their arguments are in [codex-session-relay](../../crw-run/references/relay.md).

Every process in one assignment must point at the same state directory, or they simply do not see each other. Which commands a given task can run is a capability of that task's profile on that host, discovered from the relay's own environment check rather than assumed: only the commands that reach the App Server need a socket, and everything else works from the store alone. Where a task cannot reach the socket, it uses the store-only subset and a host-capable process owns delivery. On the host and profile measured during JUN-92 a workspace-write task could not write the default state directory or connect to the control socket; reuse that as a recorded observation, not as a universal rule. Offline, a child's readiness claim is staged: it is real recorded progress, it is not delivery, and a report must not call it one.

Ask the relay who is responsible before creating anything. An assignment lookup by issue returns the existing relationship, its child, its current state, and the next expected action. Registering a different child for an issue that already has an active or paused assignment is refused transactionally, so duplicate registered ownership cannot be created; that is a guarantee about the record, not about the host, since a task created outside the relay is still a real task. A pause does not release an issue.

Record BOTH the parent's and the child's authorized execution settings from the actual creation receipts the coordinator already holds, and register the issue's criteria as the canonical set so a later verdict rules on agreed obligations. Both reuse results already in hand; neither is a handshake, and neither needs an extra turn or further approval. Never ask a worker to echo its own settings back, and never widen a task's permissions to make a later send connect.

Verify the revision the relay reports as current. A verdict names the criteria set it was reviewed against, so editing a criterion after the review invalidates that review rather than passing it, and re-review runs under a fresh execution generation rather than by re-deciding a settled event. An integration record names the exact revision it integrated. The coordination summary is a separate concern from execution and is the COORDINATOR'S OWN connector write: the relay holds no credential and has no Linear transport, so it queues the summary and the coordinator executes it, conditionally replacing its owned container rather than appending, then reads the document back and confirms against that job's structured record. A failed summary write is retried through its existing outbox job; this retry does not re-run verification or re-send a correction. OPS-8.3 in [Operations contract](../../crw-run/references/operations.md) records the installed-module tests for this narrow behavior. It is not evidence for the proposed multi-parent fairness, per-parent limits or general delivery-error isolation.

### Installation and operations have one owner

Dependency identity, installation ownership, the shared relay service and its durable store, service lifecycle, workspace assignment, assignment routing identity, how a parent returns to idle after delegating, and the review location of each dependency repository are all defined in one place: [Operations contract](../../crw-run/references/operations.md). Read it before installing, updating, or operating that runtime, and before assigning a checkout. Do not restate its versions, paths, or rules here or in a script, because a second copy of a version or a state directory is the copy that goes stale first.

Two consequences reach every operation in this reference. One relay service and one durable store serve a whole operating scope, meaning one host, one OS user, and one App Server, shared by parents across repositories and Linear projects, so no operation creates a service or a store of its own and no parent shuts down a service other parents are using. And each assignment routes on its bound identifiers rather than on a display name, a branch, or a working directory, so results, corrections, and permissions never cross between parents.

### Review evidence is independent of the provider

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
