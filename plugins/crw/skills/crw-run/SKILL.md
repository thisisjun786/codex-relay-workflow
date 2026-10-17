---
name: crw-run
description: "Coordinate one milestone as its child: bind, dispatch, observe, verify, correct, integrate and resume; no goal by default, resumed by a relay event. Also the parent's project order, decisions, reports and 중간점검."
---

# CRW Run

Use the selected milestone as the planning source and keep this Codex task
as its coordinator: this task is [the child](../crw-canon/references/hierarchy.md#the-levels)
(project session), one per project, and this skill names every session by
that table. The parent (repository session) above this project orders the
repository's projects, answers the decisions this child raises and reads its reports; it instructs
this child, never this child's subagents or `child` sessions (a code id, not a layer), and its own work is
[the parent's section](#the-parent-the-repository-session) at the end of this skill. A `child` session
is a Codex session bound to one oversized issue.

The default path is a flow run with role subagents: the child carries the project one milestone
at a time as a flow run of the built-in project lane, the run hands the child each step, and the child
spawns that step's role subagent (an architect, an executor, a reviewer, the critic or an explorer: a leaf whose
call the run records) under [Run a milestone with a flow run](#run-a-milestone-with-a-flow-run) and
[How the child works](#how-the-child-works). A node the architect sized for a `child` session starts no
`child` session: the project lane escalates it as `size_split` with detail `child_retired`, and the
escalation goes up to the parent (repository session), which asks for a bundle split of that issue.
The dispatch, observation, verification and correction of a `child` session under
[Independent implementation tasks](#independent-implementation-tasks) are the exception path, the
path of a plan with no run. Follow the shared
[scope of each level](../crw-canon/references/hierarchy.md#scope-of-each-level),
which fixes the names and the ids they are recorded under and covers
standalone issues and explicit current-task work. This child owns scope, dependencies, its runs and
their steps, review, and the decision to release the next work; a `child` session owns its checkout and
execution.

Run owns project execution, including newly ready successors within the agreed scope, and by
default the child holds no goal: it ends its turn when only waiting remains, and a delivered relay
event or a step of its run brings it back. [Goal mode](references/goal-mode.md) holds the lifecycle of
the child's goal where the user explicitly asked for one; it does not enlarge Run's project scope.
Load `crw:crw-dev` and relevant surface skills for development
and review work. A `child` session whose effective workflow is `crw-loop` loads
`crw:crw-loop` and `crw:crw-pabcd` and owns its goal, goalplan and phases.
Resolve current installed paths; never copy plugin versions from old records.
`crw-loop` and `crw-pabcd` own those phases and the helper role policy (`crw role helper`)
owns helper routing; the child does not adopt a `child` session's FSM. How a `child` session consumes its
packet and when a task loops are [crw-loop's lane dispatch](../crw-loop/references/lane-dispatch.md); this
skill writes the packet and does not restate the `child` session's rules.

Read the shared [tracking model](../crw-canon/references/tracking-model.md) for tracking document
authority and access. Use `gh` against the tracking repository
([Use the tracking access](../crw-canon/references/tracking-model.md#use-the-tracking-access)) for full issues, criteria, and work records.
Restate a bundled scope or model correction to resolve it, and refresh the state from the records when the user
needs a refreshed briefing. Route roadmap authoring to
[crw-plan](../crw-plan/SKILL.md) within the user's requested scope.

## Determine the requested operation

A submitted `$crw-run <milestone link>` execution request, where a plugin
installation exposes this skill as `crw:crw-run`, designates or restores
this task as the fixed child using [Project parent binding](../crw-plan/references/integrations.md#project-parent-binding)
and executes the agreed project scope. Record that scope and delivery boundary,
including its known blocked successors, rather than equating scope with today's ready
batch. Reconcile existing work, carry each milestone as a flow run (and, only for a plan with no
flow run, reuse/create responsible `child` sessions and verify and integrate their results), and
admit newly ready in-scope issues as capacity opens. The first batch
is a scheduling choice, not a finish boundary. The link does not approve undefined work,
future backlog additions, another project's work or replacing another child.

An ordinary project execution request opens no goal. The child waits idle and is resumed by the
delivery path, on the readiness recorded under
[OPS-8.1](references/operations.md#ops-81-parent-continuation-and-waiting). A child goal exists
only where the user asked for one, and its lifecycle is [goal mode](references/goal-mode.md); inside
goal mode, Run returns progress and pending obligations to the same goal-mode child without creating
another goal. An explicit no-goal or read-only limit agrees with the default and bars only goal mode,
and existing scope and authorization survive skill routing.

An initiative target is not a project run: an explicit designation to execute an initiative's projects is the work of the parent (repository session), under [the parent's section](#the-parent-the-repository-session), and a link merely cited as context rebinds nothing.

Explicit status, explanation, plan-only, batch, issue, no-create, no-goal,
no-merge, or current-task limits override the default. An issue or milestone
target stays within that scope; it is not a request to run its whole project.
A quoted example, a question about usage, automatic skill selection, or an
unsubmitted UI prompt does not activate this shorthand. Host and tool restrictions
still govern each action, including task creation and goal activation.

- **Plan/prompts:** inspect project state and prepare task packets; do not launch.
- **Bind/restore only:** apply [Project parent binding](../crw-plan/references/integrations.md#project-parent-binding)
  and report the connection; do not dispatch `child` sessions or create a goal.
- **Dispatch a named batch:** reuse prior authorization and settings, refresh its
  prerequisites, then launch only that batch.
- **Run a project or milestone:** carry its agreed scope through delivery, including
  successors. A milestone, named batch or issue narrows the same project's assignment.
  The child holds no goal by default and is resumed by the delivery path; the lifecycle of a
  goal the user explicitly asked for is [goal mode](references/goal-mode.md).
- **Verify completed work:** inspect its exact revision and relevant behavior,
  send in-scope corrections to the existing responsible task, and recheck its
  delivery under the established assignment. Respect explicit report-only limits.

A request only to read where things stand is [crw-status](../crw-status/SKILL.md): it is read-only,
wakes nothing, and this skill has no read-only mode of its own.

A named-model correction steers the current dispatch; it does not authorize
global model or helper role policy changes (`crw role helper`). Preserve explicit model and effort per
task; take an unchosen model, effort, or workflow from
[Default independent execution](../crw-canon/references/delivery.md#default-independent-execution).
Do not copy a model choice from a previous project into a new one.

Resolve task-creation authority from the submitted operation above and the
conversation below; never override host requirements. Apply the shared [delivery and integration default](../crw-canon/references/delivery.md#default-dev-integration)
and inherit existing authorization for coordination records and recovery. Release
publication, deployment, issue closure, and unrelated messages need scope covering
those actions. When the user designates this as the fixed management task,
use [Project parent binding](../crw-plan/references/integrations.md#project-parent-binding) for its recorded project link,
title, and sidebar pin, then continue the authorized execution here.

## Goal mode

Goal mode is Run's one optional mode: the child holds a native host goal, and the host's
automatic continuation carries the run across turns. It is entered only by an explicit request for a
child goal on the project, such as a goal for the run, keeping or restoring a goal the child already
holds, or `$crw-loop` given a milestone and coordination intent. In the last case no loop starts:
say so, and continue here in goal mode. A *loop* is the PABCD completion loop of one task, owned by
`crw-loop`; goal mode is this mode, and the two are different procedures. The child follows
[Goal mode](references/goal-mode.md) and never the `crw-loop` or `crw-pabcd` procedure, with or without
a goal, for as long as it acts as child; the one way a session leaves that role is the
explicit current-task implementation request under task creation below. Nothing else enters it: an ordinary project execution request, a status or explanation request,
a quoted example, automatic skill discovery or an unsubmitted UI prompt opens no goal.

## Keep a project run moving

Run does not initialize, resume, complete or block a child goal. It keeps progressing
the agreed project scope during execution: dispatch, wait, verify, correct, integrate
and start eligible successors. Only for a plan with no flow run, before any `child` session dispatch,
record its observation path and the requested delivery boundary under
[OPS-8.1](references/operations.md#ops-81-parent-continuation-and-waiting).

A dispatch-only request may return with pending ownership and an exact next step.
Otherwise continue until the agreed scope is delivered, the user stops, a resource
limit is reached, or no authorized progress is possible. An empty ready queue while
`child` sessions run (only for a plan with no flow run) calls for bounded observation; a blocked
issue does not stop independent work. If the host ends the turn, preserve the unfinished project and exact resume step,
not a claim that the first batch completed the request. Where the recorded `observation_path`
is `event-driven-idle`, the child yields the turn and the delivery path resumes it on a real
event (a step of its run, or a `child` session's only for a plan with no flow run); where it is
`active-observation` or `blocked`, Run alone promises no automatic future wake-up. A checkpoint handed down by the parent (repository session) is not a status read: it reaches
this child and is handled here as a resume of the authorized run, which re-evaluates outstanding
results (a `child` session's only for a plan with no flow run), pending decision requests, cleared
blockers and empty execution slots, then continues the successors that approval already covers.
Reuse the same child, runs and assignments (and the same `child` sessions, only for a plan with no
flow run); this adds no scheduler and dispatches nothing that already has an owner. That
handoff is a manual push rather than automatic continuation, and an explicit status-only,
report-only, read-only or pause limit on it still wins.

A request from a peer child is answered here, by this task, under
[Direct coordination between children](../crw-canon/references/hierarchy.md#direct-coordination-between-children):
decide what this project's own issues and `child` sessions do about it, never reach into the other
project's `child` sessions, and escalate to the parent (repository session) only an unresolved disagreement, a scope
change, new ownership, or a shared resource including the order into a shared target. Where the
two projects share no parent (repository session), or answer to different ones, invent none: record the unsettled
part, hold only that part, keep the independent work moving, and raise that decision to the user.
Record the agreement and whether it is still conditional; a peer message moves no delivery by itself.
An accepted condition reaches this project on the correction path this task already uses (through
its own `child` session only for a plan with no flow run), and the evidence that it landed is the
changed revision of that correction rather than its acknowledgement. Return the outcome to the peer from here: a `child` session never answers another project's
child, and no peer reads this project's `child` session for its answer.

[Goal mode](references/goal-mode.md) owns a child host goal and its continuation across turns where
the user explicitly asked for one; by default there is none and the delivery path is what returns
the child. Returning from Run hands control back to that mode where goal mode is running.
Explicit pause/no-goal limits still win, and existing
child `.crw/` state must be reconciled through its supported lifecycle, never reset to
avoid a guard. A child holding its own goal does not alter the `child` session `crw-loop` defaults.

## Run a milestone with a flow run

The child carries a project one milestone at a time, and starts one run of the built-in project lane for each milestone. The run does what the child would otherwise do issue by issue: the architect's DESIGN of the milestone and its audit, an executor for each issue node, the verification and the review of each head, the acceptance and the integration, and the close of the milestone with its final gate. Each of those agent calls is a session call: the run publishes it as a step, and the child spawns the step's role subagent. The commands are in [the flow commands](../crw-canon/references/flow.md#the-crw-flow-command-table), and which agent calls the run makes, and why, are the [spawn policy](../crw-canon/references/spawn-policy.md). The issues the run releases are the nodes of the plan the project holds, which `crw flow compile` checks from the [crw-plan handoff](../crw-plan/references/issue-boundaries.md#close-the-plan) before it is put as a plan revision.

- **Start.** Run `crw flow start project-lane --handoff FILE --thread THIS_THREAD` with the plan and the milestone in `--args`. Read the preview first (`--preview` stops after it): it shows the nodes, the width, the role pairs, the budget and the five preflight facts, each `pass`, `warn` or `fail` with its reading. A fact never stops the start: the ones that did not pass are printed, recorded in the run's spec, and the run starts without `--force` (which is accepted and changes nothing). Read a `fail` before the run waits on you and report it with the run; a `warn` on a new thread's first run is expected. A preview that says `blocked` is not overridden. A milestone starts when its prerequisites have landed, and the next one starts after the one before has closed. The run declares each node's edit regions to the relay itself before it releases the node; declare a node's regions before the start only to grade them ([Release by region grade](references/region-grades.md#declare-the-regions)).
- **Take the steps.** Run `crw flow next RUN --wait 600 --json` and do what its steps say, every step of a batch at once:
  - a `spawn` step is `spawn_agent` with the step's `agent_type`, `message`, `model` and `reasoning_effort`, and `fork_context` false; right after it, run the step's `crw flow dispatched RUN CALL --agent ID --k K --attempt N` with the id `spawn_agent` returned. A full-history fork is refused by the spawn hook, because it keeps this thread's pair;
  - a `send_input` step is `send_input` to the step's agent with its message, then its `crw flow dispatched` line;
  - a `close` step is `close_agent` for the step's agent: a finished subagent holds its slot of `agents.max_threads` until it is closed.

  While subagents work, alternate `wait_agent` on the open subagents (a long timeout) with `crw flow next RUN --wait 600 --json`. `crw flow dispatched` binds a subagent once the run has read it on the host (the thread that spawned it, the `crw-flow-op:` marker of its first message and its role's pair); a `pair_mismatch` answer names the subagent to close and the pair to spawn again with, and the step stays. A spawn that fails with `agent thread limit reached` leaves its step too: close what the hint names, then take the step again. The child never copies a subagent's answer into the run: the run reads it from the host, validates and records it, and the child edits no node's directory. `crw flow next` resumes the run itself when no process holds it and it is neither paused nor over, writes the owner binding again when it is missing, and says when the run waits for a decision or is over (`ended`).
- **What this thread handles.** Besides its steps, the thread handles only escalations, which need judgment: an escalation of the run arrives as a notice from the run, `crw flow show RUN` gives the cause, and the thread decides within its own authority, or hands the decision up (see *What goes up* below), and once the parent (repository session) has recorded its answer in the relay's user-decision record (`crw relay decision-answer`) the thread applies that record with `crw flow answer`, which writes the answer to the run's journal. Apply it while the run lives, without pausing it first: an answer to a run a process holds asks the run to stop at its next boundary and returns `run_draining` (exit 1). Then keep the `wait_agent` / `crw flow next RUN --wait 600 --json` loop and take every step next lists, until the run process ends: a deadline or an earlier pause can make the drain a no-op, so the end need not be `paused`; the run reads a working subagent's turn from the host by itself, but it hands out every further step (the next turn's spawn or `send_input`, and each close) only through `crw flow next`, and a step nobody takes holds its call and the run. Once no run process holds it, read `crw flow show RUN`. For a paused, dead, or held run, retry the same `crw flow answer`: it rechecks the question and the run's limits, applies once, and resumes when permitted. If that answer already applied after the paused end, use `crw flow resume RUN`; replaying it starts no process. If the run is completed, cancelled, or failed, inspect its outcome instead of waiting for a paused end. `run_draining` says the stop was asked, not that it was made: a run process an older crw runs drops the ask and goes on, so when next keeps reading the run live without an accepted drain or an earlier stop, `crw flow pause RUN`, then the same answer. A retry refused `stale_revision` means the run asked its question again while it stopped: answer that revision (it resumes the run), or `crw flow resume RUN` to go on without it. When the run says a coordination summary is owed, the thread drains it as a record write (claim, write with `gh`, read back, complete); that is not control of the run.
- **What this thread does not do.** It does not release, correct, accept or integrate a node of a plan a run owns. It never claims the coordinator epoch of a plan a run holds: the run claims the epoch before its first fenced write, so a `dag-coordinator-claim` by this thread would raise the epoch past the run's, the run's next fenced write would be refused with `stale_coordinator_epoch`, and that milestone ends `incomplete`. An out-of-band `dag-*` transition (a release, an acceptance, an integration or a decision written by hand into a run's plan) is raised as an escalation to the parent (repository session), never made here.
- **A run that stopped.** `crw flow ps` and `crw flow show` say whether it is `dead`, `paused`, `held` or over, and the milestone state an ended run came to. A run whose process died is taken again with `crw flow resume`, which keeps its claim (`crw flow next` does it for a run no process holds); a run the operator paused ends `paused` with no completion notice and goes on only with `crw flow resume`; a run an answer drained ends `paused` by `answer` and goes on with the same `crw flow answer` run again (or `crw flow resume`); a call in `recovery_hold` is settled only with `crw flow recover`. `crw flow pause` and `crw flow cancel` follow an instruction, not a hunch.
- **What goes up.** The run reports each closed milestone to the parent (repository session) itself. This thread lifts a project-level blocker or a decision request with `crw flow report-up`, with the evidence file, because an agent node reports nothing upward on its own. A decision request that lifts a question the run waits on names it: `crw flow report-up --kind project_decision_request --run RUN --evidence FILE --question <escalation id>`. The parent (repository session) may then record its answer with that report as the source (`--source report=<report id>`), and `crw flow answer` applies that record to the question the report names.

A node the architect sized `split` with sub-tasks (CRW-1459) is released to one executor, as any executor node is. Its sub-tasks run in their order on the node's one executor thread: the build does the first, and each next one is a `subtask:<node>:<tier>:s<k>` turn of the same thread. A sub-task before the last is committed as a checkpoint against its own edit regions, with no unit check, no verification and no land; the receipt of the last sub-task, and every fix after it, goes through the node's commit and verification, then the one check of the node's whole change (the committed range from the node's base to its head, against every criterion of the node), on which the node's acceptance rests, and the land, so the node is judged once, as a whole. A sub-task that reported failed is named in the next packet with its summary and gap and judged with the node; it is no tier evidence. A fresh thread (a rebuild, the `executor-t1` build after a tier step) starts at the node's last commit, which holds the sub-tasks done, and its packet at the first sub-task not done. The run makes each of these calls itself: this thread takes their steps like those of any node and sends no task packet for a sub-task. A split with no sub-tasks, from a DESIGN recorded before sub-tasks existed (CRW-1435), still goes up as `size_split` and is released to no executor.

A node the architect sized `bundle_split`, an issue that even sub-tasks do not make one node's work, starts no executor: the project lane escalates it as `size_split` with ask `bundle_split_request` (the detail and the evidence carry the plan's `size_reason`), the escalation closes the node, and the run waits for no answer to it and goes on with the other nodes of the milestone. The escalation notice names the request: the parent (repository session) makes the bundle split of the node's issue, and this thread asks it for that split. It writes an evidence file that names the issue, the node, the plan's `size_reason` and the escalation, and runs `crw flow report-up --kind project_decision_request --run RUN --evidence FILE`. A node of an oversized issue that an older DESIGN sized for a `child` session starts no `child` session either: the project lane escalates it as `size_split` with detail `child_retired` (the evidence carries the plan's `size_reason`), releases the node to no executor, and goes on with the other nodes of the milestone. That escalation goes up to the parent (repository session), which asks for a bundle split of that issue. No executor and no `child` session is started for either node, so this thread sends no task packet for it and arms no `crw-pabcd` for it ([who owns control](../crw-canon/references/flow.md#who-owns-control-in-a-flow-managed-run)). The `child`-session rules from [Independent implementation tasks](#independent-implementation-tasks) on, the dispatch of a `child` session and its observation, verification and correction, are the exception path of a plan with no run.

### How the child works

The child splits the work and spawns subagents broadly and boldly: it breaks a milestone into pieces that can run in parallel, runs them at once, and decides the next step from their results rather than following one fixed line. In a flow run that judgment has two places. Inside the run, the project lane chooses by its results: nodes in parallel up to the run's width (the `cap` of `crw.dag`, 3 unless the child widens it in the run's arguments) where their `after` dependencies and edit regions allow, the rounds of the critic's design check, the fix rounds and the tier step, the explorer routes and a re-plan (SP-ARCH-2); the run's agent pool (`--pool`, 6) bounds the calls of every role at once, and the child spawns the steps of a batch together. Outside the run, the child spawns helpers of its own for its judgment (a spawn with no `crw-flow-op:` marker), which the spawn hook allows with the pair of their role and records (SP-NAT-1).

The sequence is fixed: one background explorer for each node of the milestone reads the repository at the design head before the first DESIGN, the architect writes the DESIGN over their findings, the critic rules on it in a separate review execution in the critic's design check, executors in parallel build it (one executor lane for each node: a node sized `split` runs its sub-tasks in order on that one executor thread, and a node sized `bundle_split` goes up as `size_split`, and the parent (repository session) makes its bundle split), the unit check, a separately invoked review tool (Codex review by default, Bugbot on a Codex quota; no reviewer agent or agent thread id), judges each executor's uncommitted work before the commit, except on a node sized `split`, whose sub-tasks before the last are committed as checkpoints with no check and whose one check judges its whole committed change after the commit and the verification of its head (its last sub-task, or a fix after it), a fix round goes back to the same executor thread for each finding (at most two round trips, then the findings are carried) and up a tier when the evidence says so, the verification runs and the head lands; if an agent review's same thread is actually observed, it is warning-only and recorded, and the run continues; model family is provenance only. Besides the background explorers, an explorer is called whenever it is needed, for an open question, a research question or missing evidence. The child decides whether to use the DAG (the project lane) or another way for a piece of work; whatever way it takes, CRW enforces the order, the gates and the records:

- **Order.** No executor before the critic's design check approves, no commit before the unit check except on a node sized `split` (each of its commits, a checkpoint, its last sub-task or a fix after it, goes in with no check before it, and the one check of its whole change follows the verification of its head), no acceptance before the unit check and the verification, no integration before the acceptance; the run enforces it.
- **Gates.** The unit check is a separately invoked review tool; same-thread observations on agent review paths are warning-only, recorded, and do not block. Historical legacy reasons `independence_refused` and `independence_unproven` remain readable as old outcomes. Verification is a passing `verification-record/1`. `crw review` is a reference opinion where a packet asks for one, not a merge gate; the review before a milestone's pull request reaches `dev` is the dev queue's configured review command (below).
- **Records.** The rows of the flow journal, the relay's acceptance and integration records, and the spawn hook's ledger.

A change made another way lands through the same gates: the child lands a head it made outside a run with `crw flow start land-head` ([the flow commands](../crw-canon/references/flow.md#the-crw-flow-command-table); verification, a unit check in the committed mode over `base..head` in a separate tool invocation (Codex review by default, Bugbot on a Codex quota), with no reviewer agent or agent thread id, acceptance and integration). A same-thread review, if actually observed on an agent path, is warning-only and recorded; the run continues. Model family is provenance only. The land does not run while a run of the same plan is going, because it takes the plan's coordinator epoch from that run (the start warns `plan_live_run`); several executors for one issue are CRW-1356. The concurrency cap is the host's `agents.max_threads` (6 by default, one slot staying the child's own): `crw flow next` reports the slots, and a wider run needs a higher `agents.max_threads` on the child's session, which is Jun's setting of the real Codex configuration.

A land-head run whose unit check needs changes does not land the node: it ends `land_review_needs_changes` with no ask, the carried finding ids and its `fix_round` (0 when the start named none), gives the node's release back, and its value hands the child a fix packet (`fix`: the findings, the range, the criteria and the land of the fix). The child gives the packet to a new executor subagent, which fixes the head on a branch from it outside any run, and lands the fixed head with a new `crw flow start land-head` run of the same node, `fix_round` one more, and the authors of the head together with the fixer's thread. After `LAND_FIX_ROUNDS` (two) fix rounds, a check that still needs changes ends `land_fix_rounds` with ask `land_fix_rounds` and no fix packet, and the child hands it up to the parent (repository session) with `crw flow report-up`.

The close of a milestone runs the crosscheck (`crw.crosscheck`): one round of two passes over the integrated range in a separate tool invocation, run by the run itself with no reviewer agent or agent thread id. A same-thread review, if actually observed on an agent path, is warning-only and recorded; the run continues. Model family is provenance only. Missing author model information is journaled under `author_warnings` and does not block; when no review engine is configured, the current refusal is `crosscheck_no_engine`. Historical `crosscheck_no_independent_engine` and `crosscheck_author_undecided` outcomes remain readable. A crosscheck that needs changes keeps the milestone open: the run ends escalated `crosscheck_pending` with ask `crosscheck_pending`, the verdict in the escalation's evidence and in the run's value (`crosscheck`), and the fix packet (`fix`: the blocking findings, the range, the criteria and the route of the correction). The child hands the verdict and the fix packet up to the parent (repository session) with `crw flow report-up` as a decision request; the route the packet names is a correction issue of the milestone naming the finding ids (`crw-add-issue`, a plan revision), a new executor subagent that fixes it from the integration tip, the land of the correction node with `crw flow start land-head`, and the report of the verdict and the landed head up to the parent (repository session). A crosscheck with no verdict (`crosscheck_no_verdict`) or one that failed (ask `crosscheck_failed`) goes up to the parent (repository session) the same way; one that held holds the milestone, and `crw flow resume` runs the round again once the cause is gone.

Ambiguity during a run never opens an interview. A question the child cannot settle within its authority goes up to the parent (repository session) as a decision request with `crw flow report-up` and the evidence file, and the work it does not block continues on recorded assumptions (CRW-1347, CRW-1314).

A flow run lands each node on its target branch by itself and opens no pull request. Where the child lands its milestone through one pull request ([PR-unit landing](../crw-canon/references/delivery.md#pr-unit-landing), CRW-1355), the handoff's target `base_ref` is the milestone integration branch and its `pr_base` is `dev`. The child makes the branch before the run starts with `crw flow pr branch --checkout CHECKOUT --milestone M --name BASE_REF` (`BASE_REF` is the handoff's `base_ref`; created from `dev` once, never moved; `crw flow start` refuses a bundle whose branch is missing), so the run's nodes land there. Once the milestone's issues have landed and the run has closed it, the child opens one pull request from that branch toward `dev` naming them, `crw flow pr open --run RUN --repo OWNER/NAME --head BRANCH --title T --push CHECKOUT` (related issues together, not split finer than the milestone needs; `--run` needs the run's verified `crw.crosscheck` row and is refused with `crosscheck_missing` without one; `--issue REF...` with a text of its own is the manual form). Where a run script calls the `crw.mergeRequest` op (the built-in `project-lane` does not), the op asks the parent (repository session) to merge it, once per head. The child enqueues it with `crw manage integrate --enqueue-pr OWNER/NAME#N`. The child does not merge it: the dev queue merges its head onto `dev` after the verification of the merged tree and the repository's review command, and writes the landing record on the pull request. `crw flow pr show --pr REF` reads its state; a pull request the review refused (`blocked`, with the verdict file) is fixed on the branch, pushed to the same branch (`git push`, never forced; the pull request follows its branch), and enqueued again.

## Independent implementation tasks

This section and the `child`-session rules that follow it are the exception path, for a plan with no
flow run. On the default path the implementation of an
issue is its executor's call, a step the run records, which this child spawns as a role subagent; the
rules about `child` sessions below do not apply to it.

On this path, the implementation of an issue belongs to its `child` session, a responsible
independent Codex task, including an existing worktree and repairs to an existing PR. The child
owns scope selection, preparation, dispatch, delivery validation, and authorized integration; a
`child` session whose assignment covers publication owns its own delivery, including its checks and its
review cycle. Issue count and checkout availability do not turn the child into the
implementation worker.

Read the coordination record and inspect the existing responsible task and any
current writer before creating or assigning work. Verify its actual task/host
ID, issue scope, current turn, checkout ownership, and execution settings. Reuse
that task for compatible follow-up work on the same issue under the existing assignment. A busy
task, an uncertain send, or an inaccessible record is not evidence that no writer
exists; reconcile before retrying or considering a replacement.

Where a relay holds the assignment, ask it first: an assignment lookup by issue
returns the existing relationship, its responsible `child` session, its current state, and
the next expected action. Registering a different `child` session for an issue that already
has an active or paused assignment is refused in the same transaction that would
have recorded it, so duplicate registered ownership cannot be created even by two
concurrent attempts.

That is a guarantee about the assignment RECORD, and it is not a lock taken before
creation: registration needs the task id creation returns, so two coordinators can
both find an issue unassigned and both create a `child` session before either registers. The
lookup makes reuse the first move and names an existing owner; the registration
transaction is what settles which `child` session owns the issue. A coordinator refused with
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

Internal subagents are bounded helpers within their owning task. A `child` session may use
them for its implementation; the child may use them for bounded inspection or review, beside the
role subagents it spawns for the steps of its runs, which are steps the run records. A subagent
handle, even one represented as a thread ID, is not evidence of an independent `child` session. On this
path, do not replace the `child` session with the child's own implementation or a child-owned
implementation subagent just because the issue is small or a creation path is unavailable.

Resolve creation and execution intent from the full conversation for this scope,
including prior authorization and the plan the user is accepting. Explicit intent
does not require a particular word such as “create” or “생성”. Accepting a concrete
new-task plan requests its execution; clear delegation in an established
independent-task workflow carries that task intent. Preserve the referenced
project, task/batch/run scope, and settings without asking the user to restate
them. Higher-priority host/tool restrictions still apply.

| Invocation context | Action |
|---|---|
| Submitted `$crw-run <milestone link>` execution request with no narrower operation | Bind/restore the fixed child and execute the agreed project scope, including successors, with no child goal, ending the turn when only waiting remains once the readiness facts hold; host restrictions and an explicit no-goal limit still apply |
| Request to create/reuse `child` session tasks, a submitted prompt expressing that intent, or clear project delegation after independent tasks were established as the execution workflow | Reuse the responsible task first; create only when needed within that scope and allowed by the host, without another authorization round |
| Concrete new-task plan followed by the user's acceptance, such as “진행해” or “응” | Execute the accepted plan within its stated scope; do not ask for a creation keyword |
| Resume of an authorized run, including after compaction | Recover its authorization source, scope, and settings; refresh ownership and prerequisites, then continue the remaining in-scope obligations, including successors. Preserve an explicitly batch-limited assignment; do not repeat approval already covering the scope |
| Standalone short invocation such as `$crw-run 다음 작업 진행해줘`, with genuinely no creation intent in the conversation or prior authorization for this scope | Prepare the issue packet and inspect ownership; if a new task is needed and the host requires an explicit creation request, obtain only that missing request |

Merely mentioning the skill's name, an unsubmitted UI default prompt, a quoted
example, or task designation without execution is not a `child` session-creation request.
The submitted project-run shorthand above is an execution request. Permission
for an unrelated earlier task does not authorize this scope. Discover a
permitted tool; a different transport does not waive the host's creation rule.
If authority or capability is missing, finish the executable packet and baseline
preparation, report the specific missing permission or tool, and request only
the unresolved decision. Do not silently switch execution mode or widen permissions.

An explicit request to implement within the current task overrides this workflow's
default separation: honor it with `crw-loop` and label the actual mode accurately. For that
task the session is its implementer and not acting as child, which is the supported
role change that lets it follow the loop procedure: it creates no `child` session and dispatches nothing,
and it starts no goal mode. A session bound to the project that holds no child goal does this
and says the child role is paused for that task. A session that holds an active child goal
cannot run the loop under it, because that goal and the loop's would be one host goal: report
the conflict and offer the supported choices (a `child` session implements, or the user ends or blocks the
child goal first), and change neither goal nor binding on its own. Coordination of the other
issues resumes when the loop ends or the user returns the session to the child role.
Status-only, report-only, read-only, and no-create limits also win. Ordinary
coordinator review, verification, CI inspection, and authorized integration
remain here. Assignment is not loop evidence: creating or assigning a `child` session task
does not by itself create a goal or prove that a `crw-loop` started.

## Establish the project baseline

Read the milestone and its description, canonical planning/decision documents,
full issue criteria, sub-issues, and the `Blocked by:` lines through `gh`
(`crw flow plan-input --milestone OWNER/NAME#N` shows the milestone as a plan input). Inspect the
repository's applicable guidance, branch, dirty state, worktrees, and relevant
implementation. Refresh volatile claims before acting. A project's summary prose,
milestone percentage, issue status, merged PR, and deployed behavior can disagree;
record the discrepancy rather than silently treating them as equivalent.
Missing tracking access is a concrete limitation, not permission to invent issue details.

For a standalone issue, read that issue, its linked canonical documents and
blocking relations directly; project and milestone reads are inapplicable. Keep
its issue-scoped ownership and the existing management binding unchanged. Do not
create a project or a project binding to satisfy this baseline. If a transport
requires a project binding, use a permitted projectless execution path or report
that capability gap; never invent a project ID for a receipt.

For repository-changing work, compare local and remote commit ancestry. Preserve local-only commits and dirty
work. Record a full baseline commit for each task and decide how any prerequisite
changes will reach it. Do not push shared baseline commits through every task.

At initial dispatch, after a `child` session's completion, blocker or decision request, after an
integration lands, and on every resume or recovery, scan the agreed project scope for useful
parallel work, including newly unblocked successors. Before the turn ends that scan has run, or
the reason it could not be read is recorded; a final answer describing what would be dispatched
is not the scan. Which read observes each of those events, and what the pass records per
candidate, are in [Re-evaluating the candidate set](references/reevaluation.md). A resume
restores the recorded start adjudication and re-reads what it stands on rather than deciding it
again ([Start policy](references/start-policy.md#re-read-on-every-entry-re-decide-only-on-a-change)).
An explicitly limited batch or issue remains limited in both Run and goal mode. Check
verified prerequisites, overlapping edit surfaces, existing
writers, shared runtime resources, and available execution capacity. An issue that already has a
responsible `child` session is not a candidate, and that is read rather than assumed. Where a relay holds
the assignment, `doctor --issue` answers it from one non-constructing read before anything is
created: `holds` true ends the evaluation for that issue, and `holds` null is an unproven answer
rather than a free slot
([Determine whether this store holds the assignment](references/relay.md#determine-whether-this-store-holds-the-assignment)).
An explicitly direct assignment has no store to ask, so its owner comes from the coordination
record and the existing task the same way the rest of this skill establishes one; a direct run
reads no relay answer and is never held for the absence of one.
Where a relay holds the assignment, a `child` session that stopped for a person is invisible to the
reviewable reads, so enumerate the stopped
`child` sessions with `dispositions-show` ([Which children stopped](references/relay.md#which-children-stopped-and-whether-anyone-was-told))
instead of reading their silence as progress; it reports what a `child` session actually emitted, which is
why [OPS-8.1](references/operations.md#ops-81-parent-continuation-and-waiting) still requires an
emit path or a bounded observation path for the disposition itself. Dispatch
the largest useful set of independent ready issues within explicit concurrency,
budget, and host limits. A shared repository alone is not a reason to serialize;
separate owned checkouts can carry independent changes.

Do not wait for an entire batch to finish before filling available capacity with
eligible independent work inside that boundary. Independent issue statuses alone do not establish
independence: serialize shared schema, persistence, contract, or central UI changes
when separation would cost more than it saves. Keep integration into a shared
target serial and recheck each candidate against the updated base. Record why an otherwise ready
issue was deferred in the closed set
[Decisions and what clears them](references/reevaluation.md#decisions-and-what-clears-them)
defines, so a later pass re-reads the condition that held it rather than the prose that described
it. The set keeps apart what recovers differently: no capacity from capacity nobody measured, an
owner that could not be proved from two the store reports, and a disposition that could not be
read from one the store holds as contested. It carries no escalation value, because a review
round count is not a reason to send an approved correction upward. Do not invent extra issues or duplicate writers just to
increase concurrency; an issue the [size check](#check-the-size-before-dispatch) flags is reconciled through `crw-plan`'s boundary rules before dispatch, when the concept boundary separates it and a useful split fits the authorized scope.
One issue runs as one packet and one delivery today (a task branch and its head for the
integrator, or a pull request where one is named), and a batch keeps those
separate pairs, under the shared [issue-to-PR mapping](../crw-canon/references/tracking-model.md#issue-to-pr-mapping)
and the work-unit rules (`POLICY.md#work-units-review-and-integration` in the CRW repository). Several
packets for one feature are allowed by policy but are not switched on until their
support lands. If one issue needs several PRs, or a proposed PR would deliver
several issues, reconcile the plan through `crw-plan` before new dispatch;
preserve existing owners and active work.

Keep the human-readable coordination record in the milestone's coordination issue
(each comment opens with the `[layer · session]` header; the end condition stays in the milestone description),
as part of the management assignment, without a separate recording request.
For a standalone issue, use its own comments within the authorized recording scope. Keep private
recovery receipts keyed by the issue and actual task IDs; no project record or
project-level child binding is required. If delegation or a relay is used, retain
the real coordinator task ID and routing identity; projectless does not mean
coordinatorless. Preserve unrelated issue content on each update.
For explicit read-only scope or unavailable access, return the unsynced update and
retain the task's private recovery receipt. Keep raw launch
receipts and sensitive evidence in an appropriate private location; local
snapshots point to the issue and are not another planning source.
Do not store project state or credentials inside this installed skill.
A DAG plan's coordination summary is the plan's own queue in the relay, drained by the child with its own `gh` write: take an entry, write it, read it back, confirm it, retry only
that entry ([The coordination summary of a DAG plan](references/relay.md#the-coordination-summary-of-a-dag-plan)).

Before assigning a checkout, apply the shared
[repository resolution](../crw-canon/references/tracking-model.md#resolve-the-implementation-repository)
and put its verified issue target, remote, integration branch and full baseline
in the packet. Reuse an existing task's worktree and issue branch on resume;
classification changes alone never relocate it. Non-PR work can omit those code
fields with its explicit result and verification instead.

### Determine the execution mode

Managed start and managed resume decide, before dispatch, whether a relay holds this
assignment or whether this work is explicitly direct, and record which. Everything below
that reads "where a relay holds the assignment" is that decision, not a guess to be made
again per paragraph. The rule and what to record are in
[Whether a relay holds this assignment](../crw-canon/references/delivery.md#whether-a-relay-holds-this-assignment);
the command and its output are in
[codex-session-relay](references/relay.md#determine-whether-this-store-holds-the-assignment).

Run it from the process that will do the work, because what a task can reach is a property
of that task's profile on that host rather than a fact about the relay. Determine the socket and
the state directory as [One shared state directory](references/relay.md#one-shared-state-directory)
says; a `store_owned_by_other` refusal of an unstamped store there is a wrong selection, not an
unavailable relay, and never by itself a reason for direct mode. One reading answers
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
delivery menu disagrees the contract wins. Name in the packet that the `child` session owns its
commits, push, the task branch and its verification through to
the applicable gates, and that the coordinator performs the merge while release and
deployment remain the user's. Then give the `child` session OPS-5.5 and OPS-9 from
[Operations contract](references/operations.md) as context of its own: cite them by id
where the `child` session can read this repository, and carry the clause text itself where it
cannot, because a packet has to stand alone and a `child` session that never sees OPS-9.1 and
OPS-9.2 can follow the summary above while still requesting review on a draft or
reporting a missing mandatory check as complete. Carry that text rather than a summary
of it. A quotation can be diffed against its source and regenerated when the clause
moves, while a paraphrase cannot, and it is the paraphrase that drifts unnoticed.
Name the capability the `child` session is being created with, and for a task that cannot write
git metadata say so explicitly and assign the fallback, since those are the parts the
contract cannot know. A packet carrying neither clause sends a `child` session the previous
workflow.

Write the first packet in the form of the pair recorded for the issue: the
[Launch packet](references/task-packet.md#launch-packet) for a Sonnet `child` session and the short
[SOL packet](references/task-packet.md#sol-packet) for a SOL `child` session; an issue with no recorded pair keeps the
Launch packet. The rule that picks the pair is "Child pair by issue type" in
[crw-plan's integrations.md](../crw-plan/references/integrations.md), and both forms carry the restoration block and
restate the workflow on every send. The choice itself is recorded at release
([Record the pair choice at release](#record-the-pair-choice-at-release)).

Apply [Child task titles](references/task-packet.md#child-task-titles):
`ISSUE-ID · descriptive task title` (title text up to 20 characters, including
spaces; exclude the issue code and separator). Supply the title through the supported
creation field, verify the actual title by task ID, and correct it on the same
managed task when supported. The packet's title alone is not app-state evidence.
That title names the Codex task, not the pull request, which the `child` session titles in English under
`Language:` ([Child task titles](references/task-packet.md#child-task-titles)). A temporary
directory the packet names is short, and every make target, CI check and script it names for
verification was confirmed to exist when the packet was written ([First full assignment required fields](references/task-packet.md#first-full-assignment-required-fields)).

Choose the `child` session's MCP profile at release, from the issue and its packet, and state it in the packet and in the
release request's `child.settings.mcpProfile`: `ui-qa` for UI or browser QA, `second-opinion` when an
external-model review is named, otherwise `minimal`. A host whose policy declares profiles for the child role
refuses a `child` session whose request states none or one it does not declare. A `child` session that needs a server its profile
lacks raises a decision request; the profile is not widened under it. Selection rule and what the host does
with it: [MCP profiles](references/mcp-profiles.md).


Apply [Independent implementation tasks](#independent-implementation-tasks) even
when no new branch or worktree is needed. Non-PR work uses its permitted working
directory and artifact access without Git metadata. For code work, reuse a checkout whose ownership is
verified for the responsible `child` session; a coordinator may prepare it before handoff.
Otherwise follow the project/user placement convention. Record the actual path,
branch, and owner; do not create a second checkout just to prove task separation.
Placement, the ownership columns, and the write split between a coordinator that
prepares git metadata and a `child` session that only edits source are in
[Operations contract](references/operations.md), which also owns the shared relay
service, its durable store, and the child's continuation and waiting mode.

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
launched; on this path the child's own or an internal subagent's implementation is not a substitute.

Apply the effective settings through the creation tool's real arguments, and send
the bounded issue packet in the initial work prompt so the `child` session can start the
assigned work. When `crw-loop` is the effective workflow, that prompt invokes the
`crw-loop` skill; an explicit non-Loop or no-goal alternative omits that
invocation and names the agreed workflow instead. Verify the returned task ID,
cwd/workspace roots, model, effort, and full permission profile from the creation
receipt, and reconcile a mismatch on that same task. A prompt saying “use this
model” is not a configuration override. A catalog entry is not proof of the model
that served the request. Settle a setting the creation path cannot apply before
creating the task rather than downgrading it.

Record request ID, task ID, host ID when supplied, turn ID, requested/actual title
and settings, and launch outcome, with the pair choice as
[Record the pair choice at release](#record-the-pair-choice-at-release) lists it. For code work include the
checkout and full Git baseline SHA. For non-PR work include the permitted working location and input
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
basis is how a coincident task gets registered or steered as somebody else's `child` session.
Bind it only on something that could not be true of another task, such as its own first
user message being the packet that was dispatched, read from that task. Widen the search
before concluding nothing is there, and where nothing is identified the creation stays
unresolved and is reported as such; an unidentified candidate is not the `child` session, and
identity stays on stable IDs.

Neither path always settles. A response lost at the wrong moment leaves the outcome
genuinely unresolved, and an unresolved outcome means a writer may exist. Treat it as
one: create no replacement while it stands, and report it rather than overwrite it.

Where a relay holds the assignment, register the relationship with its authorized
scope, record BOTH the child's and the `child` session's authorized execution settings from
the actual creation receipts, and record the issue's criteria as the canonical set
so the later verdict rules on the agreed obligations rather than on a reviewer's
recollection. All of that reuses what creation and the issue already provide; none
of it is a readiness round trip and none needs another approval. Registration uses the
task id creation returns, so the packet carries the shared state directory and the exact
issue identity registration was given instead, which depend on nothing creation returns;
the `child` session resolves its own relationship by issue lookup when it emits, which is after the
work rather than at startup. That lookup matches the registered string exactly, so a
display key in the packet where registration used a stable id resolves to nothing. A `child` session
fast enough to reach that lookup before registration lands finds nothing, or, on a host
where registration is the store's first write, is refused `store_absent`, since a read never
creates the store; either way it
reports its completion unemitted and preserves the artifact; recover it from that same task
after registering, in a later turn carrying a continuation claim, rather than creating another
`child` session or writing on its behalf. The receipt it then emits is the same event the on-time one
would have been. Field-level detail, including that recovery and which settings field is
nested in the response, is in
[codex-session-relay](references/relay.md). Without a relay this paragraph does not
apply and the dispatch above stands as written.

When `crw-loop` is the effective workflow, the `child` session owns its goal, goalplan, and
phases through its own `crw-loop` and `crw-pabcd`; the coordinator checks the
resulting evidence, not the `child` session's internals. Creating a task does not create a
goal; an active turn does not prove the loop is armed. Missing loop prerequisites
are reported explicitly, with no silent substitution of a different workflow.

### Check the issue is ready before release

Before an issue gets a packet, a task, a `managed-start` or a DAG release, run `crw skill issue-ready check` on the issue as a GitHub issue gives it once mapped to `id`, `title`, `description` and `labels` (the `gh issue view … --jq` command of [Use the tracking access](../crw-canon/references/tracking-model.md#use-the-tracking-access); JSON on stdin or saved to a file), before the size check below. It judges the [release gate](../crw-canon/references/release-gate.md#the-release-gate): registered criteria, edit regions, a decided answer with no design question left, the test cases to write first, and the done condition. Like the size check, its answer never stops the release: a reversible step is warned and recorded, not refused.

- `ready` (exit 0): the release goes on under the checks below.
- `not_ready` or `design_first` (exit 0, a warning): the decision says which, and each of the `reasons` is printed on stderr as a warning; it names the missing item and the heading that supplies it, and a `design_first` reason names the label or the open question. Write the reasons into the coordination record beside the launch record and report them to the issue's owner. What the child can supply from the evidence at hand it supplies before the release and runs the check again: the content of a missing heading, and for a `design_first` issue the design (an architect-role proposal and the child's decision, written into the decided answer, the label removed). Otherwise the release goes on (the packet, the task, the `managed-start` or the `dag-release`) with the warning recorded. A held issue never holds the rest of the ready set.
- `bypassed` (exit 0): the input carried the explicit approval of the parent (repository session) (`exception`: `exception.issue`, `exception.approved_by`, `exception.approved_on` as YYYY-MM-DD, `exception.statement`) for this issue. Write the report's `exception_record` into the coordination record beside the launch record, then release. Your own judgment that the issue is small, a `child` session's request and a sibling's earlier release are not that approval; without it the record of the release is the warning.
- Exit 2 and 3 mean the issue was not read (exit 2 also for an `exception` that names no approver, issue, date or statement): correct the input and run the check again. An unread answer is not `ready`.

The relay's release request does not carry the issue body, so the warning is this step and not a relay refusal. Where no issue-ready answer was recorded, the dispatch record says so. `crw-tidy` finds the unreleased issues that fall short and a missing `설계 먼저` label.

### Check the size before dispatch

Before an issue gets a packet, a task or a `managed-start`, and before a DAG release of it (a release goes through `managed-start`),
run `crw skill issue-size check` on the issue as a GitHub issue gives it once mapped the same way, its JSON on stdin or saved to a file. The answer is advisory:
it never blocks a dispatch and never exits non-zero on the count alone. Read the report, not only the
exit status: `decision`, `reasons`, `limits`, `concept_questions`, `bundle_reason` when one was given, for an over-baseline issue `proposal`, and `observed.unread_headings`. The command counts only
the sections whose headings it knows, and only criteria written as list items or as rows of a pipe table under such a heading; a list
under a heading it does not know, a bold label, a quote or another list symbol is not counted, and it only warns about a heading. Where
an unread heading looks like criteria, tell the issue's owner and check a local copy with the heading changed, whatever the answer says;
the child writes nothing to the issue. The command counts what the body
states against limits read from recorded delivery; it judges nothing, the same issue gets the same answer, and the limits are in
its report, so this text states none. It also shows the concept-boundary questions and, when the body states one, the scaled line
estimate with the CRW-739 ratio correction.

- `ok` (exit 0): the issue is under every baseline; dispatch goes on under the rules below.
- `over_line` (exit 0): the issue is over a baseline. Read the reasons, the draft proposal and the concept-boundary questions and
  judge the bundle by the `crw-plan` [boundary rules](../crw-plan/references/issue-boundaries.md#decide-the-boundary). The draft is a
  proposal: writing a split follows those boundary rules and the user's approval, and the answer alone neither holds nor splits the
  issue. Record the answer where the dispatch is recorded. Where the concept boundary holds the bundle together, dispatch it and
  record why with `--bundle-reason`.
- `over_line_accepted` (exit 0): the same, with the `--bundle-reason` text carried in the report as `bundle_reason`. Write that
  reason into the coordination record beside the dispatch, so a later reader sees why a bundle over a baseline was accepted.
- Exit 2 (the input or its `depends_on` cannot be read, a code fence in the description is never closed, or the issue has no completion criteria) and exit 3 (the file cannot be read)
  mean the input was not read: correct the input or the issue and run the check again.

This is an instruction the child follows; nothing refuses a dispatch that skipped it, so the run records the answer where it records
the dispatch. The check informs the dispatch; it does not decide it.

### Check the bundle before release

Before a release, check the nodes that are not released yet against the bundle criteria once, as a set, because the boundary a
plan drew at design time can be wrong once the parts are known. This is a bundle check, not a size check: it asks whether the
unreleased nodes still belong to the concept they were planned as, not whether any one of them is large. Use the plan's own
release surface where it offers one, and where a `dag-bundle-candidates` output exists for this plan, read it as the evidence
for which unreleased nodes share a concept; where no such command or output exists, read the unreleased nodes against the
`crw-plan` [boundary rules](../crw-plan/references/issue-boundaries.md#decide-the-boundary) directly.

- Nodes that the bundle criteria hold together are merged by amending the plan first, before any release: the plan revision
  records the merged node, its combined criteria and its single delivery, and only then is the merged node released. Never
  release two nodes that should have been one and reconcile them afterwards.
- An issue that the merge absorbs is closed as Duplicate, and its body and criteria are moved onto the representative issue
  before it is closed, so nothing the merge promised is dropped. Move every relation too, not only the body: the absorbed
  issue's prerequisites, dependents, milestone links and other external relations are transferred to the representative
  issue and read back, and an edge whose two endpoints both fall inside the merge is dropped because the boundary it
  described no longer exists. Record the duplicate link and the representative issue. A successor must never be left blocked
  on a retired node.
- A node that is already released is not touched: its boundary, its owner and its delivery stand as they were, and a later
  bundle check never reaches back into it. Where a released node turns out to have been over-bundled, that is new work with its
  own issue, not a re-split of the released one.

This check changes the plan, not the execution, and it dispatches nothing on its own. A plan amendment that rewrites
completion criteria or closes an absorbed issue is a tracking write, so it needs scope covering that write: where the run is
authorized only to execute the existing issues, prepare the bundle change and return it as a proposal instead of writing it.

### Record the pair choice at release

When an issue is released (a packet, a task, a `managed-start` or a DAG release), its pair choice goes into the
coordination record with the launch record, one row per release, so that a later review can ask whether the
issue was classified right. [The record at release](../crw-plan/references/integrations.md#the-record-at-release)
owns what the row holds and why it lives here and not in the relay; this is the order of the steps.

1. Read the issue's child pair line. With no line, route the issue by the shape table, write the line in the
   [extended form](../crw-plan/references/integrations.md#the-extended-line) and read it back before anything
   else, as [Child pair by issue type](../crw-plan/references/integrations.md#child-pair-by-issue-type) says.
   Where the user chooses a pair for the issue now, write the choice onto the line first, with its reason and
   the source `user choice`.
2. Take the tags and the bundle from the line, and quote the line, with its own source, in the row. The row's
   source is the step that decided at release: `issue body` where the line already stated the pair and you
   follow it, `user choice` for a choice written onto the line now, `table` for an issue you routed because it
   had no line or for a provider-failure table move applied at this release, `quota` when a readable snapshot
   decided the pair. A line in the older form states no tags: classify the issue by the
   [tags](../crw-plan/references/integrations.md#classify-the-issue-by-its-kind-of-work), take the bundle from
   the [Bundles rows](../crw-plan/references/integrations.md#bundles), mark the row `derived at release` and
   leave the line as it is. The pair the line names stands (precedence): where the derived bundle names the
   other family, the row says so and the issue is released on the line's pair. A tag the issue does not settle
   is left blank in the row with the reason, and a blank tag never holds the release.
3. Run `crw skill pair-choice choose [--snapshot quota.json] [request.json]` using the bundle, line
   pair/source, `as_of`, working/project counts, tie pick, and one release row per flexible issue in the
   routing window, as [The place for a quota rule](../crw-plan/references/integrations.md#the-place-for-a-quota-rule)
   defines. No snapshot producer is approved: until a cache-only OCX projection exists, omit `--snapshot`
   and record the table default with `quota.readable: false` and reason `no_snapshot_producer`. Never run
   a live OCX quota command or rebuild membership from operating files for this step. A supplied approved
   `crw-pair-quota/1` snapshot enables the quota rule; the command itself only reads the supplied inputs.
   Read its JSON `pair`, `source`, `rule`, `default_pair`, `quota`, `window`, `limits` and any cause/date or
   classification reason. Exit 1 holds release; correct a request/output failure before dispatch. Record
   the full report beside the row, including read values or unreadable reason. Serialize choice and row
   recording so counts stay current. Extended flexible table/quota lines can change: write the result
   and read it back before the packet, retaining tags/bundle and using source quota only when quota
   decided. Legacy/user choices keep their precedence; fixed-provider failures keep cause/date evidence.
4. Decide the pair (steps 1 to 3) before the first packet is written, and write the row once the request id
   exists: the fields of [The record at release](../crw-plan/references/integrations.md#the-record-at-release),
   naming the family while the model and the effort stay in the launch record's settings entry, read from the
   creation receipt.
5. A later change of the pair (a user's choice, a move under When SOL fails, a replacement) adds a row and
   overwrites none.

Nothing refuses a dispatch that skipped this; the rows are what the review reads, so a missing row shows up
there. The relay's release request has no field for these values
([codex-session-relay](references/relay.md)); where the coordination record cannot be written, report that gap
with the other launch facts that could not be recorded.

### Release by region grade

The child exists to run work in parallel, so an edit region is a signal for the merge order and for the handling of a conflict, not a gate on release. Two nodes that both write a file are not run one
after the other for that reason alone. Only a change two branches cannot both make holds a release back: the same file renamed or deleted, the same function body rewritten in two directions, a shared
contract surface (the contract schema, the CLI spec, the contract goldens), or an overlap whose resolution would cost more than the work. Every other overlap is released and settled when the branches meet.
The child orders the merges and settles the mechanical conflicts by their rule, and the `child` session of the later pull request resolves the rest when it refreshes its base.

In a DAG plan the scheduler applies this by grade: a region is declared `independent`, `mechanical` (with the rule that settles it: `union`, `renumber` or `regenerate:<command>`), `local` or `exclusive`,
`dag-ready` releases mechanical and local overlaps and defers only exclusive ones, and the rule it released a node under, with the rows that rest on and the overlap count by grade, is in the reading and
in the recorded pass. [Release by region grade](references/region-grades.md) says how to declare the grades, how to read the judgement, and what the child does at merge time: deciding the merge order,
settling a mechanical overlap, and when a candidate goes back to its `child` session. A conflict only in mechanical places is settled by the
child and proved by `crw skill base-refresh mechanical` ([Resolve a mechanical conflict
yourself](references/merge-readiness.md#resolve-a-mechanical-conflict-yourself)). Conflicts are measured at every landing and every receipt (`dag-integration-observe`, `dag-accept` and `dag-conflict-sweep`, after a `git fetch`), and a measured conflict of two live nodes
appears in `dag-ready` as a `merge_order` that says which lands first and that the other refreshes its base after it; nothing running is stopped, and the child orders the merges by it ([Read the merge order](references/region-grades.md#read-the-merge-order)).

### Start policy and child cap

Before this run creates its first `child` session or registers an assignment for one, whichever comes first,
settle the start policy and record it in full: every field [Start policy](references/start-policy.md)
names, each carrying its source, its scope and the conditions it stands on. A value the precedence
settles is applied without asking; a value that genuinely needs a new decision is asked before any
`child` session is created, and only the action waiting on that answer is held while independent authorized
work continues. Registration is named beside creation because it does not always follow one: a
reused `child` session already has its task id, so an assignment can become managed with no creation call in
sight. Counting is the separate question, and there a creation still means a `child` session. On a resume,
after a compaction or for a later batch, re-read the identities those values stand on, restore what
has not changed, and adjudicate again only the field whose conditions did. Start policy owns the
fields, the scope each decision carries, the re-adjudication triggers and the cases this is judged
by; the standing cap and its precedence are in
[Default parent start policy](../crw-canon/references/delivery.md#default-parent-start-policy).

The first thing settled is the role, because it decides what this task opens: an initiative
management task runs without a native goal or an automatic loop, a child opens no goal
either and is resumed by the delivery path, and a `child` session creates or reuses its own goal and
keeps its `crw-loop`. Record the role beside the recorded mode and observation path, the approved
scope, the hook compatibility, and whether the approval policy in force can carry the callback this
run depends on. Where a child does hold a goal because one was requested, record its objective and
state beside those.

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

The workspace of a run is this skill's. A Codex-app managed worktree (one per thread under
`$CODEX_HOME/worktrees`) is `crw-worktree-guardian`'s, and branch hygiene is `crw-dev-devops`'s; see
[Worktree owners](../crw-worktree-guardian/SKILL.md#worktree-owners-wg-owner-01).

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
this assignment has no authority to make. Two `child` sessions needing the same large
original share a permitted read-only copy instead of receiving one each. A repeated
whole-disk scan that stops the work is its own failure, and a report that a disk
filled up is not a current measurement of it.

What the assignment permits and what the `child` session can actually write are separate
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

A refused Git write is answered on the same `child` session and the same checkout, and which
answer applies follows from which refusal it was. An environmental restriction, an
OS permission on the original repository's Git metadata, a read-only mount, or a lock
whose owning process has already exited, is cleared through its supported route and
the work continues there; establish that a lock is stale before removing it, because
a lock file records only that somebody took it. Another task still holding that path
or that lock is not a restriction to clear but an ownership conflict: leave its work
as it is, serialize behind it or settle it with its owner, and report the blocker
rather than displacing a live writer. Where the refusal is the `child` session's own permission
profile, nothing here widens it: a running task keeps the settings it was created
with, and the supported answer is the
[OPS-5.3](references/operations.md#ops-53-who-commits-and-the-fallback-when-a-task-cannot)
fallback taken deliberately and recorded, with the `child` session still owning the source
edits in that checkout and returning a frozen diff against the recorded baseline
while the coordinator writes the Git metadata. What both cases refuse is the
improvised route: redirecting `GIT_DIR`, cloning the repository somewhere writable,
or a child commit taken as a workaround rather than as that recorded fallback, each
of which settles a permission problem by leaving the recorded ownership instead of
repairing it, and leaves a delivery the review cannot trace to an owner. `reset`, `clean`,
`stash` and `rebase` are not repairs either: they make the command succeed by
discarding the state that explains the failure. Preserve the files, record why, and
carry a blocker neither route clears to whoever can grant the permission.

Carry the result into the packet's workspace-ownership fields
([Launch packet](references/task-packet.md#launch-packet)), so the `child` session is told
what is not its own instead of deriving ownership from the path it was handed.

### Verify what this dispatch established

The first full assignment carries the fields in
[First full assignment required fields](references/task-packet.md#first-full-assignment-required-fields),
and they travel in that same first request rather than in a preparation turn.

Then judge the dispatch on four separate facts: the instruction the `child` session actually
received for this dispatch, the settings the dispatching call requested, the settings the
receipt returned at the scope it claims them, and whether the `child` session's own goal and
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
| Independent `child` session assigned | Creation or reuse receipt from the independent-task interface, actual task/host ID and ownership; not an internal-subagent handle |
| New task created | Creation receipt and recoverable task ID; reuse is recorded separately |
| Prompt dispatched | Accepted turn ID and matching user message |
| Instruction delivered into a running turn | Accepted steer receipt naming the guarded turn ID; a refused or mismatched turn ID is not delivery |
| Peer read an instruction | The task's own transcript showing the steered input; an acceptance receipt does not establish it |
| Peer acted on an instruction | The changed work, revision or reply; neither acceptance nor a read establishes it |
| Goal paused | The goal read back as paused; it does not establish that a running turn stopped |
| Requested settings applied | Actual returned settings, not prompt text |
| `crw-loop` active | `child` session's active goal and current goalplan/FSM evidence |
| Agreed workflow actually followed | That task's own recorded phases, plan and evidence for this assignment. A skill-loading line, an acceptance receipt, or the instruction quoted back is an indication of receipt, not of compliance |
| `child` session reused its own existing goal | That task's current goal and goalplan read back under its own identity. A second goal opened for the same assignment is a duplicate, not a resume |
| Work delivered | Completed turn plus actual commit/diff and checks for code; verified result with both input baseline and delivered output revision/digest for non-PR work |
| In-flight pull request review handled by the `child` session | Per-finding trail on that pull request (in-flight only): the finding, the commit that addressed it, and the recheck |
| `child` session reports normal completion | The repository's named verification and the applicable reviews finished on the current head, blocking findings resolved; a missing mandatory review or verification is blocked, not complete |
| Candidate ready to hand over | The `child` session's handoff record: the delivered tree and head, the verified destination tip, the verification evidence the repository names (this repository's `verification-record/1`, or the runs by id and attempt where a repository still runs hosted CI), the review coverage actually read, and a judged disposition with evidence for every thread seen. A completion naming a pull request and saying nothing about its review is refused, because silence is the failure this exists to catch |
| Child runs in goal mode | That task's own active goal, read back under its own identity, where goal mode was explicitly requested. A child operating without a goal is the default and needs no authorization; what is recorded instead is its observation path and the readiness facts behind it |
| Verified for integration | Coordinator reviewed the exact revision and acceptance criteria |
| Receipt recorded, where a relay holds the assignment | The `child` session's completion receipt with its revision hash and manifest |
| Verification decision, where a relay holds the assignment | A verdict at the current head revision, covering the registered criteria and naming the criteria set it was reviewed against |
| Coordination summary written | The coordinator's own `gh` write, confirmed by a readback carrying that job's structured record |
| Merged by the coordinator | The issue's criteria and the delivered head's verification checked, then the actual landing on the intended branch verified (for an in-flight pull request, its latest diff, base, head, checks and review resolution) |
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

The child shares the host with every `child` session, so what it starts to verify or integrate follows the
`Processes you start:` line of the [Launch packet](references/task-packet.md#launch-packet) as a
`child` session's runs do: a local build or test run records its pid when it starts or runs under `timeout`,
and is stopped only by that pid, its own process group or the handle the execution tool returned
for it, never by pattern or name. The relay's delivery service is not such a run: it stays the
scope's service, and the child that started it does not end it early
([OPS-4.1](references/operations.md#ops-41-ownership-is-the-operating-scope-not-a-parent)).
A process of a `child` session's that looks hung is that `child` session's to stop; the child raises it with that
`child` session and does not stop it.

What the child runs locally to verify also follows the launch packet's `Go build resources:` line: the
shared build cache, `GOFLAGS=-p=4`, the packages the change touches and a memory-limited scope for
anything heavy. The whole test suite is the verification the repository names for the tree under
verification (this repository's `crw-dev ci local`) and is not repeated locally
([The three gates](references/merge-readiness.md#the-three-gates)).

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

Use independent review when scope/risk warrants it, through the current `crw-dev-code-reviewer`
review protocol inside the appropriate checkout. A worker's self-report alone
does not fulfill independent verification.

For substantial intent or acceptance uncertainty, use
[crw-check](../crw-check/SKILL.md) as a bounded audit and retain coordination
here. Use [crw-check](../crw-check/SKILL.md#find-contradictions) for a specific suspected logical
violation, and check that evaluation evidence does not merely confirm its own assumption. When
a nontrivial task packet needs a fresh-reader check, give it to a context-free reviewer through helper delegation (`crw role helper dispatch`) and rewrite that
packet after changes so it reads as current. Do not run every check on every delivery.

After verification, the coordinator applies [Default dev integration](../crw-canon/references/delivery.md#default-dev-integration),
unless the assignment limits delivery. Read [Merge readiness](references/merge-readiness.md)
to check current CI and reviewer evidence using the repository's actual configuration.
Devin and Codex reviews are references there and not gates: the merge waits for neither, and the `child` session
waits for the one run of each within its waiting budget before its receipt ([Devin and Codex reviews are references, not merge gates](references/merge-readiness.md#devin-and-codex-reviews-are-references-not-merge-gates)).
Serialize integrations that share a target, verify the landing, and update the
coordination record. Work inside a relay merge turn never runs in the background and records
its steps ([Working inside a merge turn](references/relay.md#working-inside-a-merge-turn)). A capable `child` session owns its commits, its push of the task branch, the handoff and the local verification record, and reports once the current head is verified; the integrator fast-forwards the verified tree to dev, and the `child` session never merges. Release and deployment
still require the user. Delivery ownership and the fallback for a task that cannot
write git metadata are in [Operations contract](references/operations.md).

Read the `child` session's handoff rather than rebuilding it. It already paginated the review and
enumerated the check runs, so collecting them again repeats work this contract just assigned
elsewhere, and re-triaging its findings opens a second review round it already owns. What the
coordinator adds is currency: re-read the head and base immediately before merging and compare
the counts against the record. That is a mechanical check, not a review. Where they disagree,
return the candidate to the same `child` session fail-closed rather than fixing it here. A late review
thread on the record's head that the coordinator has itself dispositioned with
`merge-evidence --restate <record> --late-dispositions <file>` is not a disagreement
([how](references/merge-readiness.md#a-late-thread-the-parent-dispositions-itself)). Where the installed relay
lacks that option, [Late review threads](references/merge-readiness.md#late-review-threads) has the temporary procedure.

Report **verified**, **needs changes**, or **unverified**, with concrete evidence,
and distinguish implementation, merge, and deployment. Start a successor
only after its required contracts/revisions are verified and available in its
checkout and it belongs to the agreed project scope or explicit narrower assignment.
This is the same rule in Run and goal mode; a goal changes persistence, not scope.

Name what the work is doing in the vocabulary its contract already defines, rather than
in words invented for the report. Ordinary progress, waiting on input, a stop and a
request for verification are conditions the child has to tell apart, and two contracts
supply the words for different questions. What the `child` session's turn did is a
[turn disposition](references/hook-contract.md#turn-disposition): `in_progress`,
`ready_for_review`, `blocked_needs_input`, `interrupted` or `failed`. Where the
delivery stands is
the [assignment state](references/relay.md#verify-the-current-revision) when a relay
holds it. Report them separately; a `child` session blocked on a person has a turn that stopped
and a delivery that did not move, and one word cannot carry both. A condition neither
vocabulary names is reported as a blocker against the state that does apply, under
[OPS-6.2](references/operations.md#ops-62-record-shape).

A `child` session blocked on a person is answered with a decision, never with a verdict or a message the relay cannot see: the answer goes back through `decision-reply`; for a split approval or a scope change, which open the next generation of the same `child` session, `dag-correct` then records that generation for its DAG node; and the `child` session's result in it is ruled and accepted like any other. Follow [Answering a child that stopped for input](references/relay.md#answering-a-child-that-stopped-for-input). A message sent outside that route leaves only the trace `admit-turn` records, which the same section names as the fallback.

After integration, apply [Implementation Done](../crw-canon/references/tracking-model.md#implementation-done)
before reporting or recording the issue complete. Read back the one delivery's landing: the commit on the intended branch (the integrator's fast-forward) with its repository and branch, or the pull request's actual merge where one was named. For legacy multi-PR
scope, verify the reconciled deliveries and their combined coverage instead. Retain
existing accepted operational criteria and never infer completion from an automatic status alone.

### Reuse the child's evidence while it still applies

A capable `child` session runs its own checks and carries the review on its own pull
request, so where that evidence still applies the coordinator reads the result
instead of producing it again. What still applies is already settled elsewhere
and is not restated here:
[OPS-9.2](references/operations.md#ops-92-what-normal-completion-means) reuses a
result only at the same revision, criteria and environment and re-runs it when
any of the three moved, and [Merge readiness](references/merge-readiness.md)
refreshes whatever a new head, a changed base or a changed dependency
invalidated. Inside that, polling a check run the `child` session is already carrying,
reading again every finding on a hosted review it has resolved, and re-running a
suite that passed and is still valid are the coordinator doing a level below
itself. The coordinator's own verification at the merge gate is the exception: it reads the diff and the
code and reruns the tests the criteria rest on
([the three gates](references/merge-readiness.md#the-three-gates)). Read the head it reports, the conclusions on that head and its
per-finding trail, and accept them as the evidence table above defines them.

Acceptance keeps its own work, which was never the `child` session's. Confirm the reported head is the head the delivery names (the task branch and its head, or the pull request's head where one exists), the base is current and the merge is clean, every accepted criterion maps to evidence that still applies at that head,
and any finding still open is named. The handoff's disclosures are part of that: every decision
request is answered before the verdict, under
[what the handoff discloses, checked at the verdict](references/merge-readiness.md#what-the-handoff-discloses-checked-at-the-verdict).
A merge without a conflict is not yet a merge that builds, so the head merged with the base tip is
built and vetted before the verdict too, with `crw skill merge-build-check`
([Build and vet the merged tree before the verdict](references/merge-readiness.md#build-and-vet-the-merged-tree-before-the-verdict)).
The one head that differs from the report on
purpose is a base refresh the child made itself under
[Merge readiness](references/merge-readiness.md#refresh-the-base-yourself-when-only-the-base-moved),
confirmed by that rule's own checks. Any other moved head invalidates the reused result,
and so does a base or a dependency the `child` session never built against: that the base
is current now is not evidence that its checks ever saw this one, so re-run what
that change invalidated rather than read the old conclusion again. Where the
`child` session's evidence is missing, ambiguous, stale or contradicted, or where
independent review is required and has not happened, get that evidence directly:
a shorter report is not a reason to pass a criterion nobody verified.

Reuse changes what the coordinator spends, not what the work cost. Report the
coordinator's own usage and the assignment's total separately, and claim a saving
only from a measurement that actually compares them.

## Return corrections to the existing task

Apply the completion-follow-up rule in [Integrations](../crw-canon/references/delivery.md#completion-follow-up-in-an-existing-execution-workflow).
Send an actionable packet to the existing responsible task without asking Jun to
approve or relay routine in-scope corrections. Include the issue and criterion,
reviewed revision, expected and observed behavior, reproducer/evidence, required
outcome, and focused verification. For missing proof, request that verification
without prescribing an unsupported code change.

How many rounds this has taken is not a reason to send it upward. Where the criteria and the
correction scope are already settled, the next round goes to the same task with what changed,
and the child judges whether the evidence still supports continuing rather than returning that
judgement to the user. New scope, new authority, and a real contradiction in the criteria are
decisions somebody else owns; a round count is not one of them, and neither is the cost of
having looked again.

Carry the [restoration block](references/task-packet.md#restoration-block) with every
correction. A running task has been working for a while, may have been compacted, and
is being addressed by a coordinator whose context it cannot see, so the facts it needs
to resume correctly are stated again rather than assumed to have survived. That is the
same reason the first request named the workflow: naming it once, at creation, is not
the same as it still being in force ten turns later.

Where a relay holds the assignment, the needs-changes verdict IS the correction (a child that
finished its turn is unloaded within about a minute, so the work of a generation goes in its first
turn; see [the managed start sequence](references/relay.md#the-managed-start-sequence)): it
opens the next execution generation and queues the revision request to the same
registered `child` session, carrying the superseded event, its digest, and the per-criterion
findings. Record findings with notes, because a correction with no findings is one
nobody can act on. Do not create a task, a second relationship, or a parallel
message path to deliver it. Without a relay, send the packet as described above.

The restoration block rides inside that payload rather than beside it. The findings and
their notes are what the relay actually delivers to the `child` session, so that is where the
block goes, and a verdict is not issued until it is there. Declare which finding carries
it, so a correction the relay cannot carry is refused before it opens the next generation
instead of being discovered after there is nothing left to go back to. Where the installed
relay does not recognise that declaration, that is a limitation recorded and reported on
the assignment, and it is still not a reason to open the parallel path the sentence above
forbids. Nor is there a supported way to send it again afterwards: the verdict does not
resend. Record it as an undelivered correction and hand the decision to whoever owns the
assignment, per [codex-session-relay](references/relay.md#the-parent-verifies) and
[the restoration block](references/task-packet.md#restoration-block).

A correction that asks only for the base to be brought up to date is not new scope, and the packet
says so: the `child` session merges the base, names the kind of each merge ([the kinds and what reruns for
each](references/task-packet.md#what-a-handoff-discloses)) and reruns that kind's checks and no
more, so the correction does not ask for an audit of what the base moved under. The restoration
block carries the siblings' landings and the conflicts the child expects.

A base conflict can show after the receipt was ruled `verified` and before the node is accepted. The needs-changes ruling on that same receipt carries it: the relay replaces the verified ruling and opens the next generation, as long as nothing rests on it. The steps, the refusals and what an older relay answers are in [a base conflict after the ruling and before the acceptance](references/merge-readiness.md#a-base-conflict-after-the-ruling-and-before-the-acceptance).

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

A resume this task publishes into an existing `child` session carries the same
[restoration block](references/task-packet.md#restoration-block) a correction does,
for the same reason: it is a message into a context this task cannot read. Two
recoveries are easy to write as one event and are not. This coordinator recovering its
own lost context is the paragraph above, done from the coordination record. A `child` session
recovering its own is that task's affair, handled by its own workflow from its own
durable state; the coordinator supplies the pointers and the assignment facts it holds
and does not reconstruct the `child` session's plan on its behalf.

A final update gives task links/IDs, requested and verified settings, actual
progress, review outcome, and the next actionable dependency. Use the host's
created-task directive when required. State remaining capability gaps plainly.
This skill does not install a recurring monitor: use the automation tools only
when ongoing background monitoring is requested.

### Report delivery reach in the final update

The final update above says what the work is; this says how far it got and
what the user can use. Write the chain from
[Delivery reach and current usability](../crw-canon/references/delivery.md#delivery-reach-and-current-usability):
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

### Close the project's merged assignments

When a relay-managed project is finished, its child closes the merged assignments it leaves live, so the next child can take
the project over with `linkage-handover` and the store stops counting finished work. Read `linkage-completion --project <key>`
first and keep that reading as the completion evidence, because afterwards it reads `unregistered` while nothing is live. Run
`relationship-close-merged --project <key> --actor <own task id>` without `--apply`, read `closable` and `kept`, then repeat
it with `--apply`. A non-empty `kept` is not a finished project: report each kept assignment with its reason. For a supervised
project read `supervisor-standing --project <key>` and do not apply while an entry for a `closable`
assignment (its `relationId`) lacks a `decision.priorReport`, since a report not yet staged cannot be sent once its assignment
is closed; entries of assignments already closed do not block.
[Close the merged assignments](references/relay.md#close-the-merged-assignments-when-the-project-is-done) has the commands and
the way back.

### Account for the resources this run leaves behind

A later management read should be able to say which checkout and which branch a
piece of work used, who was answerable for it, and why anything still on disk is
still there. So the close of a run states that once, in one place: every checkout,
branch, temporary artifact, evidence root and background process this run created,
changed, or is deliberately keeping, each with its owner, the reason it is retained,
and the next action, including who may release it and on what event. It reuses the
records that already exist and adds no new store: the milestone's coordination
issue for the human-readable list, or for a standalone issue
its own comments, with the private
receipts keyed by the issue and the actual task IDs carrying raw detail under
[OPS-5.4](references/operations.md#ops-54-evidence-location).

Write it against what was found at dispatch rather than against whatever is visible
now, because the comparison is what separates the entries. Pre-existing resources
this run never touched, what it added, what it is keeping on purpose together with
the event that releases it, what a failed creation left behind, and what two
`child` sessions share are five different answers. Evidence a finished task is deliberately
retaining and residue nobody has looked at are both still on disk and are not the
same entry. A resource whose owner cannot be established is recorded as unknown and
returned as an open question; it does not become this task's by being found by it.

Cleanup is bounded by that ownership. This run may remove its own disposable
resources where the cleanup authorization column already covers them and their
readers have exited. Existing dirty work, another task's cache or evidence, original
data, active worktrees and shared services stay as they are. `reset`, `clean`,
`stash`, `rebase` and forced deletion are not cleanup here, and an old modification
time, a scratch-looking name or a full disk is not proof that anything is
disposable.
Write scratch files under a `mktemp -d` directory outside the worktree and leave them there; the Codex exec policy refuses `rm -f`, so a file this run must take out of the worktree is deleted with an `apply_patch` `*** Delete File:` hunk. Keep the three claims apart when reporting: what is proposed for
cleanup, what was actually deleted, and what capacity that actually returned.

Observation spends a shared resource as well. Parallel `child` sessions and the task watching them
typically use the same credentials and the same external API allowance, so polling competes with
the work being polled. This task, as the child (project session), owns that aggregate for its
project: before sustained observation it reads the actual budget and reserves headroom for the
`child` sessions ([the observer shares the lanes' quota](../crw-pabcd/references/dispatch-surfaces.md#the-observer-shares-the-lanes-quota-dispatch-poll-budget-01-default),
[waiting on work](../crw-loop/references/waiting.md)).
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

- **The fixed binding** — the project or standalone issue this task executes, the initiative above
  it where one is recorded, and this task's own id, each by its stable identifier, with the host
  this task runs on where that has been established.
- **The current temporary target**, where one exists: the other project or issue this task was
  asked to look at, recorded beside the fixed binding rather than in place of it.
- **The approved scope** in force, with the limits that arrived with it and the start policy this
  run recorded, restored under [Start policy and child cap](#start-policy-and-child-cap) and
  adjudicated again only for the fields whose conditions have changed.
- **The current owner** of each piece of live work — the `child` session task, its issue, its pull request.
- **The one next action.**

Those five are what the restatement reports. What it has to establish before reporting them, the
settings in force and the work that has to survive the restart, is the rest of this subsection. A
restatement that drops the initiative a project was approved under has lost the boundary rather
than a label, which is why the first line carries it.

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

A record saying completed is a claim rather than a verdict, and it is corroborated against the
evidence its own delivery shape requires under
[Implementation Done](../crw-canon/references/tracking-model.md#implementation-done): for implementation work that is
the delivery landed in its intended target together with every accepted criterion still
outstanding beside it, installation or live verification included where the criteria named them;
for accepted non-PR work it is the agreed observable result, which has no pull request by design.
That test runs per subject rather than once, so each scoped issue is corroborated on its own
delivery and the project is finished only when every obligation in its agreed scope is delivered,
integrated and reconciled. That is the child's own completion boundary in
[Scope of each level](../crw-canon/references/hierarchy.md#scope-of-each-level),
and no single pull request reaches it.

Four states then look alike from outside, and the restatement separates them. A project whose
evidence holds is finished, and its child resting is the shape of that rather than a failure to
recover from. A project labelled completed while some subject still lacks the evidence its shape
requires is unfinished: that gap is what gets reported and the project stays in the active set,
because a status nobody corroborated is the one thing that can hide undelivered work behind a
quiet child. A project still in progress whose returned result was never judged, or whose
recorded blocker has since cleared, continues inside the approved scope and through the task that
already owns it. And a run somebody stopped stays stopped: the finding is recorded with what would
release it, and nothing is sent.

Two requests are answered from the restatement. A status-only request is answered with the current
position first — what is done, what is in progress, what is waiting, and the next observation that
would settle what is still open — and the history after it; the report's form belongs to
[crw-status](../crw-status/SKILL.md), and this task supplies the position rather than a second
format. A request to carry on re-reads the current owner and its delivery state before anything is
sent, then continues through the task that already owns the work, inside the approved scope.

The settings to restate are not all established the same way. Model, effort, profile and
permissions are compared against what the current host reports and what this task is authorized to
use now, under
[Default independent execution](../crw-canon/references/delivery.md#default-independent-execution)
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

## The parent: the repository session

This section is the work of [the parent](../crw-canon/references/hierarchy.md#the-levels) (repository session): the one Codex session that represents a repository, above that repository's children. Everywhere else in this skill the task is the child (project session), and the parent is always this repository session. The parent binds itself once with `crw manage supervisor register`, which gives it its management seat and lays a store seat down; it answers the top session above it and instructs its children below it, never their `child` sessions or subagents. Its work is four things: it sets the project order, decides which children run together, answers a decision a child raises, and reads the reports the children send up. It never operates a run: a run belongs to its child, and the commands that start, answer, pause or resume one are the child's.

| What the parent does | Commands it runs |
|---|---|
| Binds its seat and reads it back | `crw manage supervisor register`, `crw manage supervisor show [--task THREAD]` |
| Binds the child of a project it starts | `crw relay linkage-bind --role parent --scope PROJECT --task CHILD --host HOST` from its own turn |
| Hands its seat to another thread | `crw relay linkage-handover --role supervisor --scope-kind management --scope SEAT ...` |
| Orders the projects and starts children | `crw manage capacity` before a child is added; the [coordination message](references/task-packet.md#coordination-message) for each child's brief |
| Answers a decision a child raises | `crw relay decision-answer`, or a decision only Jun takes handed up to the top session |
| Reads the reports | `crw flow reports --project PROJECT --unread` (`crw flow reports --unread` for every report waiting), `crw flow report-ack REPORT --turn TURN` |
| Reads its message queue and clears a backlog | `crw relay supervisor-list --recipient THREAD`, `crw relay supervisor-acknowledge --recipient THREAD --actor THREAD --all-informational` (or `--message ID`) |
| Runs the midpoint check | `crw manage checkpoint`, `crw manage checkpoint record` |

Read the shared [tracking model](../crw-canon/references/tracking-model.md) for document authority and the [milestone description standard](../crw-canon/references/tracking-model.md#milestone-description-standard) for what a milestone description holds. Settling a milestone and its issues belongs to [crw-define](../crw-define/SKILL.md) and the plan to [crw-plan](../crw-plan/SKILL.md).

### Hold the seat, bind the children, hand the seat over

The relay delivers to the parent only what its records route there, so the parent keeps three records true. The rules behind them are the relay's linkage document (`docs/relay/linkage.md` in the CRW repository, its management seats section) and the supervisor channel's (`docs/relay/supervisor-channel.md`, the level above a project); this subsection is the procedure.

1. **Register the seat once.** In a turn of the parent's own thread run `crw manage supervisor register` (the thread is `--task`, else `CODEX_THREAD_ID`, else the configuration's supervisor section). The working directory and the settings file come from the configuration only (`--cwd` and `--settings-file` are refused): the `manage.supervisor` object of the crw configuration file (`CRW_CONFIG`, else `crw/config.json` below `${XDG_CONFIG_HOME:-$HOME/.config}`; `crw manage config` reports the file a run reads) holds `host_id`, `settings_file` (the settings file the pair record cites), `cwd` and the fallback `task_id`, and `--host` alone may stand in for `host_id`. A run that is missing one is refused `supervisor_unconfigured` before the relay is called, and the refusal names each missing key (`manage.supervisor.host_id`, `manage.supervisor.settings_file`), the file the run read or that it read none, and where the thread may come from; write the keys into that file and register again. The seat is keyed by the thread that first registered it; a thread that held the store seat lays it down in the same transaction (a `scope_seat_moved` row), and the relay then reads the store seat through this seat. Registering again answers with the seat the thread holds, and the parents of two repositories hold two seats that never overwrite each other. Read it back with `crw manage supervisor show` before the first child is bound; show reads by thread alone (`--task`, else `CODEX_THREAD_ID`, else the section's `task_id`) and needs no host or settings file.
2. **Bind each child from the parent's own turn.** When the parent starts a child for a project, it binds that child itself: `crw relay linkage-bind --role parent --scope PROJECT --task CHILD --host HOST`, where `--creator` defaults to the caller's `CODEX_THREAD_ID`, the parent's thread. The answer carries the edge from the seat to the project under `managementLink`. From then on the project's milestone reports, blockers, decision requests, escalations, completion digests and the decisions only Jun can make go to the parent through the supervisor channel, and the relay daemon opens a turn in the parent's thread for each (a new turn when it is idle, a resume and a turn when it is not loaded, after the turn when it is busy); a decision raised from the project names this seat, and only its current owner records the answer. A child that binds itself (no creator with a seat) writes no edge. Its project's messages then reach the parent only through a live store seat; a store seat the parent laid down by registering carries it only the decisions the user owes (a `decision_request`, a `flow_jun_decision_request`, a fault notice of kind `decision`), and every other message of that project (a completion, a block, a milestone report, an escalation, a decision the parent itself owes) finds nobody above (`unregistered_scope`) and stays standing, one already staged held unaddressed (`hierarchy_unresolved`), until a management edge above the project or a live store seat names someone; so bind every child from the parent's turn. A binding is never refused for the edge: when another management seat already stands above the project, or a supervisor at the initiative's scope (the legacy configuration), the binding stands, no second edge is written, the answer carries `managementLinkSkipped` (`reason` `link_conflict`, the standing link) and one `management_link_skipped` journal row records it; the standing seat stays the recipient until it is handed over. A second child for the same project is refused `duplicate_scope_owner`.
3. **Hand the seat over, never re-register around it.** When another thread takes the parent's work, the seat moves with `crw relay linkage-handover --role supervisor --scope-kind management --scope SEAT --expect-task OLD --task NEW --host HOST --evidence TEXT --actor ACTOR`: the old binding is archived (`superseded_by`), the new one carries the evidence as its `handover_note`, every edge under the seat (to a project or an initiative) moves its upper owner to the new thread, and a `scope_handover` row records it. A message never sent is addressed to the new owner at its next staging, and the decisions under the seat are answered by the new owner. A seat whose owner ended without a handover is bound again by its key (`crw relay linkage-bind --role supervisor --scope-kind management --scope SEAT --task NEW --host HOST`), which is also how its open decisions are recovered.
4. **Read the queue and clear a backlog.** `crw relay supervisor-list --recipient THREAD [--project KEY] [--state unsent|all] [--hold WORD|none]` reads the parent's supervisor messages in the order the channel sends them, with counts by state, hold, project and obligation kind, and writes nothing. A backlog of old reports, each of which would open a turn of its own, is parked with `crw relay supervisor-acknowledge --recipient THREAD --actor THREAD --all-informational [--project KEY] [--staged-before ISO]`, never by editing the store: it parks only unsent reports that something finished (`completion`, `flow_completion`, `flow_milestone_report`, `lead_report`) under the hold `superseded_by_report`, one `supervisor_message_superseded` journal row each naming the actor, in one transaction. A block or an escalation is parked only by name (`--message ID`, repeatable), and a decision, a fault notice, a held message and a message being sent are never parked; the answer lists every message left with its reason. Staging the same statement again leaves a parked message parked, whether the daemon's staging pass, the sender repeating its report or a rotated sender session stages it; it is released when its content changes while it is still owed (a work report corrected in place or resubmitted, a report sent again with other text) or when it is re-addressed to another recipient. `crw relay supervisor-send --message ID` sends a parked message only while what it is for is still owed, and otherwise sends nothing and leaves it parked. When the hierarchy moved its sender or recipient, the send answers `relation_owner_drift` and changes nothing: stage it again where it came from to re-address it (`crw relay supervisor-stage --event ID` or `--project KEY` for an event's or an omission's report; for a direct message such as a `lead_report` or a `flow_completion`, its sender's own report again, since `supervisor-stage` does not stage direct messages), then send it again.

### Order the projects and group the children

An initiative link by itself is not a target. Where a request merely cites one, resolve the project actually being executed under the shared target rules: the link is context and it rebinds no existing child. Where the request is an explicit designation to execute an initiative's agreed projects, the parent carries them through each project's own child under [Initiative supervision](references/initiative-supervision.md), which also settles which session holds that binding: the repository's parent, and not a session bound to a different scope, since a session holding another project or issue would leave that work without its owner. Planning, a status read, a citation, an explanation of how the entry works, a usage question and a quoted or unsubmitted example are none of them that designation, and that list is not closed: what makes one is the explicit request to execute, never the presence of a link. Explicit status, explanation, plan-only, no-create, no-goal, no-merge and current-task limits override the default and travel down to each project without widening.

Fix the approved project set and the completion boundary, reuse the existing children and their `child` sessions, and hand each child its project brief through the [coordination message](references/task-packet.md#coordination-message). The project order is the parent's decision: which project goes first, which children run together because their projects share nothing that needs an order, and which wait. A project's child starts only when its prerequisites hold and the cap on children that `crw manage capacity` judges allows it; a project waiting on either stays in the approved set and starts when both hold. Do not plan or dispatch another child's issues, and do not reach into any project's `child` sessions or subagents.

Hold the order in which projects reach a shared target, a shared resource and an unresolved disagreement between children as this layer's decisions ([direct coordination](../crw-canon/references/hierarchy.md#direct-coordination-between-children)). A project is finished on its child's verified outcome, corroborated per subject under [Implementation Done](../crw-canon/references/tracking-model.md#implementation-done); the initiative is finished when its finish condition holds on those outcomes. Recovery and restatement of a supervision are in [Initiative supervision](references/initiative-supervision.md#recover-the-supervision). The parent runs without a native goal or an automatic loop; [goal mode](references/goal-mode.md) is a child's optional mode and never the parent's.

### Answer what a child raises

A child hands up an escalation it cannot decide within its own authority, a project blocker and a decision request. The parent answers within the approved scope by recording the answer in the relay's user-decision record (`crw relay decision-answer`), which the child then applies to its run, or by instructing the child through the coordination message. A decision only Jun takes is handed up to the top session with its reason and what each choice costs; the parent does not take it in his place. An answer widens no scope and approves nothing the assignment did not already carry.

### Read the milestone reports

A project's run reports each closed milestone to this layer, and a run that started no node of its milestone reports itself as `project_blocked` with the errors that stopped it; the child lifts a project-level blocker or a decision request the same way (`docs/flow/reporting.md` in the CRW repository says what goes up from a run). Each report arrives as a turn the relay daemon opened in this thread, and its text names its listing: run `crw flow reports --project PROJECT --unread` for each project the seat holds at the start of every turn, so a report whose delivery is still pending or was held (a busy thread past the busy cap, an attempt cap, a send whose outcome is unknown) is not missed. The kinds are `milestone_report` (the integrated range with the evidence of the close), `project_blocked`, `project_decision_request` and `jun_decision_request`; a decision only Jun takes is handed up to the top session, not answered here.

Judge each report with [crw-check](../crw-check/SKILL.md) against the project's finish condition and the milestone's criteria, then leave the read mark with `crw flow report-ack REPORT --turn TURN`, using this turn's id. This layer acknowledges a report only after it has judged it, and an unacknowledged report comes back after a while as `report_unread`. The mark records that the report was read; it does not accept the milestone, move a project or answer a blocker. A blocker or a decision request is answered as the subsection above says. This layer reads and acknowledges reports and never operates a run.

### Long-run midpoint check

A run that goes on for a long time drifts: a plan's boundaries stop matching what the work turned out to be, review keeps sending
work back, and the backlog grows faster than it drains. A midpoint check is the periodic look that catches that while the run is
still authorized, and it is separate from a user's status question, which reads and wakes nothing.

**When one is due.** Any one of these signals is enough, counted over the initiative's approved projects:

- every node of one milestone is integrated, counted once per milestone;
- twenty merges, or eight continuous hours of execution, since the last check;
- three or more needs-changes rulings or split decisions in the last two hours;
- two or more P0 or P1 findings from post-merge evaluation of the initiative's merges since the last check;
- the backlog's net increase exceeds fifteen issues in the last four hours.

Each signal is a crossing, not a standing state: count every window and every milestone from the last recorded check, and
record which crossing the check consumed, so a milestone that is already fully integrated, or a window that has already
crossed its threshold, does not make every later idle turn due again. Where `crw manage checkpoint record` exists it holds
that baseline; where it does not, the coordination record does.

`crw manage checkpoint` reads one project at a time: one report per project, each counted from that project's own baseline,
and no initiative aggregate. A project's `due` is therefore never the initiative's answer: two approved projects with the
same baseline and ten landed merges each are both not due on merges while the initiative has reached twenty. Read the
reports of every approved project and judge the initiative from them:

- A report whose reasons name a signal other than `running_hours` and `backlog_net_4h` is a crossing inside the approved set, so the
  initiative is due: those counts never fall, so the initiative's sum is at least that project's count.
- Otherwise sum each of those counts over the approved projects' reports and compare the sum to the same threshold:
  `merges_since_checkpoint`, `needs_changes_or_split_2h` and `pair_eval_p0p1_since_checkpoint`. A count that is `null` is
  `unmeasured` for that project, and the sum of the measured ones is only a lower bound, so the signal is judged from the
  coordination record instead. `milestone_integrated` is the number of milestones that crossed, so it is due when any
  project's is above zero. The elapsed time is the smallest `running_hours` among the reports, the latest baseline in the set.
- `backlog_net_4h` is a signed net change, issues created minus issues closed in the window, so one project's growth can be
  another's drain and no project's backlog reason is the initiative's answer. Always sum the signed `backlog_net_4h` of every
  approved project first, then compare that sum to the threshold: the backlog signal is due when it exceeds fifteen. A
  project at +16 beside one at -16 is not due on the backlog; two at +10 and +6 are. When any approved project's count is
  `null`, the measured sum bounds nothing in either direction, so the backlog signal is judged from the coordination record.

Where the command is not installed, or a signal is `unmeasured` in a report, the parent judges the same signals from its own
records rather than treating the missing tool as a reason to skip the check.

**When to run it.** On a turn that is only waiting: no merge-lane turn is held, no child needs a decision now, and the next
action would otherwise be idle observation. A midpoint check never interrupts a lane it does not own, and it is not a reason to
stop one.

**What it looks at.** Three things, in this order:

1. A midpoint review of the goal against its criteria, using [crw-check](../crw-check/SKILL.md) and [crw-status](../crw-status/SKILL.md), so a plan that has quietly stopped
   matching its goal is found before the run finishes against the wrong target.
2. A midpoint refactor pass with `crw-refactor`, producing zero to three candidate improvements, judged on dead code,
   duplication and pointless tests. Code the plan has not yet reached is not dead code: a connector or helper that a planned
   node will use is left where it is.
3. A midpoint optimization pass over the packet, the rules, the bundling and the test speed, changing only what the current
   approval covers.

**What it records.** Write the outcome into the coordination record. Where `crw manage checkpoint record` exists, run
`crw manage checkpoint record --project <project> --summary-file <summary>` for every approved project with the same summary, since the
command records one project per call and the next check sums counts that all start at this check. Record the crossings consumed in the
coordination record as well. The midpoint check is a diagnosis: it
records and proposes. Work worth fixing becomes an issue under the concept bundle rules, and anything not worth doing
now stays at the lowest priority label `P3`, but both are tracking writes and need scope covering them: where the current authorization
does not cover creating or relabeling issues, return the candidates as proposals with the check's evidence instead of writing
them. The release schedule and the lanes do not stop: a midpoint check never pauses a lane or holds a release.

### Answer a 중간점검 request

A one-word 중간점검, or 'where are we', arriving in the parent is answered here; the
procedure is [Midpoint check](references/midpoint-check.md). Decide first whether the call may move work or only read,
from the records, in this order, stopping at the first step that decides.

1. An explicit status-only, report-only, read-only, plan-only, pause or no-contact limit in force for this request settles it:
   read the records, write nothing, wake nobody. No later step overrides a limit the user stated.
2. Otherwise this is a checkpoint only if all four hold: this session holds the parent's designation for the same stable
   initiative ID, its approved project set is not yet complete, the execution approval is still in force, and obligations remain
   inside it. A designation and a binding are different things, and an initiative link is neither
   ([Initiative supervision](references/initiative-supervision.md)).
3. Anything else reads, as [crw-status](../crw-status/SKILL.md) does. A bare initiative link, a past assignment, a finished
   execution, or authority you cannot establish from the records is the reading branch. Ambiguity resolves toward reading,
   because a wake that should not have happened cannot be taken back and a report that should have acted can be followed up
   in the next message.

On the checkpoint branch Jun is not asking whether to continue; that was settled when he approved the execution. The call reads
the responsible children, moves the work that approval already covers and that is already waiting on somebody, and
reports what it did. What moves is only what the existing approval covers: a checkpoint starts no new project execution, widens
no scope, and creates no installation, restart or wake authority the assignment did not already carry. Existing permissions,
model, effort, ownership and task records are preserved exactly. The canonical statement of this classification is the shared
bullet in [Default independent execution](../crw-canon/references/delivery.md#default-independent-execution), and it governs.

A status question that arrives during an authorized run is a question, not a cancellation: answer it and return control to the
session that owns the execution, which keeps every obligation it already had.
