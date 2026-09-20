# Midpoint check

One word from Jun, 중간점검 or '현재 어디까지 됐어?', arriving in a task that supervises an
initiative. The answer is a short report covering everything that supervision holds and, where the
execution approval is still in force, the approved work moved along. This file
is that entry. The roles it walks are the shared
[supervisor, parent and child scope](../../crw-plan/references/integrations.md#supervisor-parent-and-child-scope),
and the binding it reads is the one
[Initiative supervision](../../crw-run/references/initiative-supervision.md) established. Its
existing row classifying a how-is-it-going request as a read that wakes nothing describes the
reading branch in [the classification](../SKILL.md#classify-the-request-before-deciding-what-this-call-may-do),
which is still the default; a checkpoint inside an execution approval that is still in force is the
other branch, and that branch is what this file is mostly about.

## Find the scope without being given a link

The request will usually carry no link at all, so take the scope from this task's own verified
binding: the initiative ID, the approved project set fixed at designation, each project's parent,
and each parent's children. Walk it down by stable ID. A project that looks related, a worktree
sitting on disk, or a task whose title mentions the product is not in scope unless the binding
reaches it.

A record of this task answering a question about some other project is not a rebinding. The
supervision binding changes by an explicit designation or switch and by nothing else, so a
midpoint check resolves against the initiative this task supervises even when the last thing it
talked about was elsewhere.

Count the approved set and the follow-up backlog separately. Work registered after the designation
is outside the approved scope until an authorized scope change admits it, and reporting them in one
total makes a growing backlog look like slipping delivery, or hides it inside a total that grew.

## Read each level for what only that level knows

Per project: done, in progress, or waiting against the project's own criteria rather than against a
status field somebody set by hand. That judgement is [crw-check](../../crw-check/SKILL.md)'s, not
this check's, so take it from the most recent trustworthy result its parent or a prior check
already produced. What this check reads for itself is the current delivery state: the pull request,
whether it merged, and whether anything was installed. Where no such judgement exists, or the one
on hand is too old or too thin to rely on, report that project's criteria state as unverified and
route it to check. A midpoint check that starts auditing criteria has become the audit it was
supposed to be cheaper than.

Per task, parent and child alike, from the host rather than from its last report: `active` with a
turn id, `idle`, `notLoaded`, or `systemError`, and when its last turn ended and how. The
[bridge](../../crw-run/references/bridge.md) owns those readings and the rule that `notLoaded` and
a system error are neither running nor finished. A task that has not reported for a day may be
working; a task that answered an hour ago may have ended waiting for something.

Per delivery, from GitHub: the current head, the CI attempt that applies to that head, the reviews
paged to the end, whether the pull request actually merged into its intended target, and whether
anything was installed or demonstrated afterwards. Use
[Merge readiness](../../crw-run/references/merge-readiness.md) for what the checks and reviews
establish and [Implementation Done](../../crw-plan/references/integrations.md#implementation-done)
for what a landing requires. Codex automatic review is disabled and is recorded as not required,
never as a review that passed.

## Surface the disagreements, not the inventory

A list of green rows is not a report. What makes a midpoint check worth asking for is the places
where two levels disagree, and three of them recur:

- The parent is idle while one of its children is still working AND nothing will bring it back.
  Idle alone is not the blocker: under event-driven handoff a parent answers, returns to idle, and
  is woken when the child's turn ends by a verified registered assignment, a running delivery
  service and a resume path, which [OPS-8.1](../../crw-run/references/operations.md#ops-81-parent-continuation-and-waiting)
  owns. Read that path before calling it anything. The blocker is an idle parent with no such path,
  whose child's result will sit unread until someone restarts it by hand, and a paused or cancelled
  parent, which nothing resumes automatically no matter what arrives for it.
- A child ended its turn waiting on a judgement. Its work is not blocked by a defect; it is blocked
  by an unanswered question, and the question has an owner who has not seen it.
- The newest report and the newest pull request describe different states. The report is stale, or
  the pull request moved after it, and the summary everybody is reading is no longer true.

Report those as the main blockers, before the per-project detail. Then split what is waiting into
two: the questions an internal coordinator resolves inside scope it already has, and the ones that
genuinely need a new decision from Jun. Sending the first kind up wastes his attention, and holding
the second kind back stalls the work silently.

## Keep one check from becoming an investigation

A midpoint check that reads every transcript and every tool output costs more than the work it is
checking, and it is asked for often. Read the compact snapshot and carry the returned cursor rather
than replaying a thread; read a final answer alone where the conclusion is the only thing that
matters; reuse the trustworthy result a level below already produced instead of recomputing it.

Pagination stays complete. Where a review or check list carries a finding the report depends on,
page it to the end, because a partial page reads exactly like a clean one. Mark the time each
reading was taken, mark anything read only in part, and mark any head that moved during the check.
A report assembled from readings taken minutes apart is a mixture, and saying so is cheaper than
being wrong about which revision it describes.

## What the checkpoint may do

On the reading branch this call queries and nothing else: no Linear write, no task created or
resumed, no message, no wake, no installation, no schedule change. On the checkpoint branch it acts
inside the approval that already exists. Either way it cancels nothing, and the supervisor and the
parents keep every obligation they had before the question arrived.

The initiative level coordinates and does not descend. It hands work to parents and performs
neither a child's implementation, CI or review nor a parent's technical judgement, acceptance or
merge. The progress rules Run owns and the handoff shapes in
[Coordination message](../../crw-run/references/task-packet.md#coordination-message) and the
[restoration block](../../crw-run/references/task-packet.md#restoration-block) are consumed as they
stand.

One reconciliation pass, and that is the whole budget. Read the parents, deliver at most one new
coordination fact or one restoration handoff per parent that needs it, read the resulting state
again, and report. Do not select issues, judge readiness, create or recreate a child, judge CI or
review, accept a delivery, merge anything, or open a polling loop or a retry queue. A second pass is
a checkpoint Jun asks for, not a loop this one starts. That boundary is what keeps a scheduler from
growing inside Status where Run already owns one.

| Observed on a responsible parent | What the checkpoint does |
|---|---|
| `active` with a turn id | nothing that starts it again. Steer only genuinely new information into that exact turn, and nothing at all when there is none |
| `idle` holding an unprocessed child result, a decision it was asked for, or a blocker observed cleared | resume it through the existing supported path, carrying the restoration block |
| `idle` with nothing to coordinate, waiting on a real dependency | record the dependency and what will release it, and send nothing. A dependency wait is not a stall |
| paused, cancelled or archived, or under an explicit no-contact or report-only limit | no contact. Report the state and the exact resume action its owner has to take ([OPS-8.2](../../crw-run/references/operations.md#ops-82-busy-paused-cancelled-and-archived-parents)) |
| `notLoaded`, `systemError`, a read that failed, or a transport that refused the send | unverified or failed, never success. Say what could not be established and what would establish it |

Nothing here recreates a child, duplicates a resume, or nudges a parent that is already moving.

### The idle row needs more than the word idle

`idle` on its own decides nothing, because it is the normal state under event-driven handoff
([OPS-8.1](../../crw-run/references/operations.md#ops-81-parent-continuation-and-waiting)). Before
sending anything, establish from the records the stable parent relationship, the active turn if
there is one, the last processed cursor or result, the outstanding items in the coordination record,
the evidence that a dependency actually released, the verified resume path, and the message identity
that makes a resend detectable. Send only on a confirmed unprocessed result, an unanswered decision
the parent was asked for, or a blocker observed cleared.

Read again immediately before sending, because the state you classified from may be minutes old.
Where a send's outcome is uncertain, reconcile by reading rather than by sending again. Where the
active turn moved between the read and the send, reclassify instead of retrying blind.

### Five facts, recorded as five

A checkpoint that reports 진행시켰다 when it only sent something has reported the wrong thing. These
are five separate facts and they are written separately: the action this call requested; the
transport accepting or refusing it; the parent observed active or resumed afterwards; the parent
assigning its child the follow-up; and that child's work observed moving. A receipt establishes the
second and says nothing about the other four.

Existing IDs, owners, model, effort, permissions and task records are preserved exactly as they
are. Nothing in a checkpoint creates installation or restart authority, and nothing in it starts a
project that was not already approved.

Where Jun asks for follow-up beyond what the standing approval covers, hand that scoped request to
the owner that already has it, [crw-run](../../crw-run/SKILL.md) or
[crw-check](../../crw-check/SKILL.md), rather than acting on it here. Seeing that a project is
stalled is not authority to start one nobody approved.

## Report

Core conclusion, then one short line per project including its
[schedule verdict](schedule-progress.md), then what this checkpoint actually did, then what is
waiting, split between a normal dependency wait, internal coordination, and the decisions that need
Jun. Then the evidence checked, the items left unverified, and any delivery that failed or could
not be confirmed. Korean, short, in that order.

On the reading branch the actions section says so and is empty. On the checkpoint branch it names
each parent contacted, what was sent, and what was observed afterwards. Nothing is described as
handled unless this call performed it, and a request that was accepted is not a result.

## Cases

### M1 One supervision, several parents, mixed task states

Observed: the approved set holds three projects; one parent is `active` with a turn, one is
`idle`, one is `notLoaded`, and their children are spread across all three states.
Action: report each project's own state. For the idle parent with a working child, read its wake
path first and call it a blocker only where that path is missing or the task is paused.
`notLoaded` is neither running nor finished, so read that task again once before
reporting it, as the bridge contract requires; a task that was merely not loaded a second ago may
read `active`. Where it stays `notLoaded`, keep that exact value rather than translating it into
idle or stopped, and report its last turn as unverified. On the reading branch, message and wake
nobody. On the checkpoint branch, the idle parent is a candidate only under the idle row's
observation fields, and the `notLoaded` one is never a send target until a second read resolves it.

### M2 CI is green and the review is not finished

Observed: the pull request's required checks passed on the current head; one review is still
running and one thread is open.
Action: report checks passed and review unfinished as two rows. A green gate is not review
completion, and an open thread is not a resolved finding.

### M3 Merged but not installed

Observed: the pull request landed on the intended target; the issue's criteria also require
installation or live verification, and neither has happened.
Action: report merged and not installed separately, and keep the outstanding obligation visible.
A source merge does not satisfy an installation criterion.

### M4 A reading fails

Observed: a task cannot be read, a connector call errors, or a review listing stops part way.
Action: report that item unverified with the reason and what would settle it, and deliver the rest
of the report. A failed read is not an absence, and it is not a reason to withhold the parts that
did resolve.

### M5 Backlog registered after the designation

Observed: several issues were added to a project after its brief was fixed, and none is approved
scope.
Action: count them separately from the approved set and label them as unapproved follow-up. Do not
fold them into the project's completion ratio in either direction.

### M6 A parent keeps working during the check

Observed: a parent's turn is running while the check is being assembled, and its state changes
between two readings.
Action: mark the reading time, report the later reading, and let the parent continue. The check
does not steer, interrupt or pause it, and its own turn ending is not something this check waits
for.

### M7 A child result arrived and the parent went idle

Observed: the execution approval is still in force, a child returned its delivery, and its parent
has been idle since before that result landed, with the result unprocessed in the coordination
record.
Action: on the checkpoint branch, resume that parent through the existing supported path with the
restoration block, then read its state again and report both what was sent and what was observed
after. On the reading branch, report the unprocessed result and send nothing.
Preserved: the parent, its binding and its child, and the difference between a send and a result.

### M8 The parent is already active

Observed: the parent that would receive the handoff is `active` with a turn id, and it is already
working on the thing the checkpoint would have told it about.
Action: send no resume and no duplicate start. Where the checkpoint holds information that turn
genuinely does not have, steer only that into the exact turn id; where it holds none, send nothing
and record why. A transport that refuses a message to an active task is protecting that turn.
Preserved: one writer per scope, and the running turn intact.

### M9 A normal dependency wait

Observed: a parent is idle with nothing outstanding, waiting on a prerequisite another project is
still delivering.
Action: record the dependency, where its release will show, and send nothing. Do not read the
quiet as a stall, do not nudge, and do not resume it to ask how it is going.
Preserved: the dependency as the reason, rather than the parent as the problem.

### M10 An explicit limit is in force

Observed: the request carries report-only, read-only, pause or no-contact, or a parent is paused,
cancelled or archived.
Action: contact nobody. Report the state, what would release it, and the exact resume action its
owner has to take. A stated limit outranks a standing approval, and resuming work somebody
deliberately stopped is the one thing a checkpoint must not do.
Preserved: the user's decision, exactly as given.

### M11 The read or the send does not land

Observed: a parent reads `notLoaded` or `systemError` after a second read, a coordination record
cannot be read, or the transport refuses the send.
Action: report unverified for a read that failed and failed for a send that was refused, and never
either as success. Name what could not be established and what would establish it, and deliver the
rest of the report. An unreadable level is unknown, not empty.
Preserved: the honest gap, and the parts of the checkpoint that did resolve.

### M12 Accepted is not applied

Observed: the transport returned a receipt for the handoff, and the parent has not been observed
acting on it yet.
Action: record the receipt as the transport accepting the input, and the parent's state as not yet
observed to have resumed. Two rows, not one. Do not resend to make the second row appear, and do
not report the project as progressing on the strength of a receipt.
Preserved: the five facts as five, which is the whole point of writing them separately.
