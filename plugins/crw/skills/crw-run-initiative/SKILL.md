---
name: crw-run-initiative
description: "Supervise an initiative's approved projects through their existing project orchestrators: bind the initiative, hand each project its brief, order work across projects, and run the 중간점검 (midpoint check). Use crw-run for one project, crw-status to only read where things stand."
---

# CRW Run Initiative

This task is the initiative lead: it carries an initiative's approved projects to their finish
condition through each project's own orchestrator. The levels and what each owns are the shared
[scope of each level](../crw-canon/references/hierarchy.md#scope-of-each-level). The initiative lead
coordinates the order across projects and instructs project orchestrators, never their issue
children; each project's issues, dispatch, verification and integration stay with that project's
orchestrator under [crw-run](../crw-run/SKILL.md). This skill owns the initiative entry and the
midpoint check; the references it reads stay under `crw-run`, so every link below goes to
`../crw-run/references/`.

Read the shared [Linear model](../crw-canon/references/linear-model.md) for document authority and
the [initiative body standard](../crw-canon/references/linear-model.md#initiative-body-standard) for
what an initiative record holds. Roadmap authoring belongs to [crw-define](../crw-define/SKILL.md)
and [crw-plan](../crw-plan/SKILL.md).

## Decide whether the request designates an initiative

An initiative link by itself is not a target. Where a request merely cites one, resolve the project
actually being executed under the shared target rules: the link is context and it rebinds no
existing project orchestrator. Where the request is an explicit designation to execute that
initiative's agreed projects, that initiative runs through its projects' own orchestrators under
[Initiative supervision](../crw-run/references/initiative-supervision.md), which also settles which
task holds the initiative lead's binding: this one where it already leads that same initiative or is
free to take it, and not this one where it is bound to a different scope, since a task holding
another project or issue would leave that work without its owner. Planning, a status read, a
citation, an explanation of how the entry works, a usage question and a quoted or unsubmitted
example are none of them that designation, and that list is not closed: what makes one is the
explicit request to execute, never the presence of a link.

Explicit status, explanation, plan-only, no-create, no-goal, no-merge and current-task limits
override the default and travel down to each project without widening. Host and tool restrictions
still govern each action, including task creation.

## Supervise the initiative

Submitted execution designation naming a Linear initiative: resolve where the binding belongs under
[Initiative supervision](../crw-run/references/initiative-supervision.md), never rebinding a task
bound to a different scope, while the initiative's own existing lead is the task that continues.
Then fix the approved project set and the completion boundary, reuse the existing project
orchestrators and their issue children, and hand each orchestrator its project brief through the
[coordination message](../crw-run/references/task-packet.md#coordination-message). Do not plan or
dispatch another orchestrator's issues, and do not reach into any project's issue children.

Hold the order in which projects reach a shared target, a shared resource and an unresolved
disagreement between peers as this level's decisions
([direct coordination](../crw-canon/references/hierarchy.md#direct-coordination-between-project-orchestrators)).
A project is finished on its orchestrator's verified outcome, corroborated per subject under
[Implementation Done](../crw-canon/references/linear-model.md#implementation-done); the initiative
is finished when its finish condition holds on those outcomes. Recovery and restatement of a
supervision are in [Initiative supervision](../crw-run/references/initiative-supervision.md#recover-the-supervision).

An initiative lead runs without a native goal or an automatic loop. [Goal mode](../crw-run/references/goal-mode.md)
is a project orchestrator's optional mode and never an initiative lead's.

## Long-run midpoint check

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

- A report whose reasons name a signal other than `running_hours` is a crossing inside the approved set, so the initiative is due.
- Otherwise sum each count over the approved projects' reports and compare the sum to the same threshold:
  `merges_since_checkpoint`, `needs_changes_or_split_2h`, `pair_eval_p0p1_since_checkpoint` and `backlog_net_4h`. A count that
  is `null` is `unmeasured` for that project, and the sum of the measured ones is only a lower bound, so the signal is judged from the
  coordination record instead. `milestone_integrated` is the number of milestones that crossed, so it is due when any
  project's is above zero. The elapsed time is the smallest `running_hours` among the reports, the latest baseline in the set.

Where the command is not installed, or a signal is `unmeasured` in a report, the initiative lead judges the same signals from its own
records rather than treating the missing tool as a reason to skip the check.

**When to run it.** On a turn that is only waiting: no merge-lane turn is held, no issue child needs a decision now, and the next
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
now goes to the sweep label Backlog, but both are Linear writes and need scope covering them: where the current authorization
does not cover creating or relabeling issues, return the candidates as proposals with the check's evidence instead of writing
them. The release schedule and the lanes do not stop: a midpoint check never pauses a lane or holds a release.

## Answer a 중간점검 request

A one-word 중간점검, or 'where are we', arriving in a task that leads an initiative is answered from this skill; the
procedure is [Midpoint check](../crw-run/references/midpoint-check.md). Decide first whether the call may move work or only read,
from the records, in this order, stopping at the first step that decides.

1. An explicit status-only, report-only, read-only, plan-only, pause or no-contact limit in force for this request settles it:
   read the records, write nothing, wake nobody. No later step overrides a limit the user stated.
2. Otherwise this is a checkpoint only if all four hold: this task holds a current initiative-lead designation for the same stable
   initiative ID, its approved project set is not yet complete, the execution approval is still in force, and obligations remain
   inside it. A designation and a binding are different things, and an initiative link is neither
   ([Initiative supervision](../crw-run/references/initiative-supervision.md)).
3. Anything else reads, as [crw-status](../crw-status/SKILL.md) does. A bare initiative link, a past assignment, a finished
   execution, or authority you cannot establish from the records is the reading branch. Ambiguity resolves toward reading,
   because a wake that should not have happened cannot be taken back and a report that should have acted can be followed up
   in the next message.

On the checkpoint branch Jun is not asking whether to continue; that was settled when he approved the execution. The call reads
the responsible project orchestrators, moves the work that approval already covers and that is already waiting on somebody, and
reports what it did. What moves is only what the existing approval covers: a checkpoint starts no new project execution, widens
no scope, and creates no installation, restart or wake authority the assignment did not already carry. Existing permissions,
model, effort, ownership and task records are preserved exactly. The canonical statement of this classification is the shared
bullet in [Default independent execution](../crw-canon/references/delivery.md#default-independent-execution), and it governs.

A status question that arrives during an authorized run is a question, not a cancellation: answer it and return control to the
task that owns the execution, which keeps every obligation it already had.
