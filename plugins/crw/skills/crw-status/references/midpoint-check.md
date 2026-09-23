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

### A turn and a goal are different readings

A parent has two states worth reading and they answer different questions. The turn says whether it
is working right now. The goal says whether anything brings it back when this turn ends. Read both
and report both, and read the parent's effective workflow before judging either.

The workflow is what makes a missing goal a fact or a fault, and since CRW-165 the ordinary answer
is a fact. A project parent runs goal-free by default, so an absent goal is the normal state and
never the stall: what returns it is the delivery path, and that is what the report reads. A parent
running [crw-loop](../../crw-loop/SKILL.md) was given a goal because someone asked for one, so a
missing, paused or unactivatable goal there is worth leading with. Judge against the mode the
parent is actually in, read from its recorded `run_mode` and `observation_path` under
[Start policy](../../crw-run/references/start-policy.md), which now owns which role carries which
lifecycle by default.

For a parent recorded `goal-free-run` the stall question is the delivery path alone, because an
absent goal is deliverable. The other no-goal record is `blocked`, a parent that can neither hold a
goal nor proceed without one, and that one is M14's case rather than a delivery fault. A goal
blocks only where one exists, which means an explicit Loop or a parent carried over from before
the default and recorded `loop` by [Start policy](../../crw-run/references/start-policy.md) rather
than converted, and there
[OPS-8.2](../../crw-run/references/operations.md#ops-82-busy-paused-cancelled-and-archived-parents)
owns which goal statuses the relay treats as blocking, read from there rather than named here. That
is a real stall with a specific cause, and it looks identical from the outside to a parent nobody
has anything to send. Pausing a parent's goal is never the way to quiet it, because that is the act
that revokes its own reachability.

Neither reading decides a contact on its own, and this is where the two rows would otherwise
disagree. The goal says whether the parent returns; the outstanding item says whether there is
anything for it to do when it does. Read the item last and let it decide, because continuation
observed working is evidence about the mechanism and not about this item. An item still recorded
unprocessed at the read you are acting on has not been carried, whatever continuation did earlier,
and an active goal is not evidence that a particular result was delivered.

So send nothing while the item is no longer outstanding, while the parent is inside a turn that
would consume it, or while a handoff carrying that same item is still genuinely in flight, which is
M12's case and is recorded rather than resent. In flight means its turn has not finished or its
outcome is not yet resolved. An acceptance receipt has no lifetime of its own: once that turn has
terminated with the item still outstanding, the handoff did not land, and treating the old receipt
as a reason for silence would suppress every future checkpoint and leave the item with no recovery
path at all. Classify that outcome as a delivery that failed, and send a recovery handoff under a
new identity so it is distinguishable from the first rather than a blind resend, or route it to the
execution owner where the same delivery has already failed that way.

Resume where the parent is idle at the current read, the item is still unprocessed, and nothing in
flight covers it, even where continuation ran after that item landed: a continuation that has
already run and left it outstanding has had its chance, and waiting for a second one is how
approved work sits. The timing of an earlier observation is context for the report, never the
reason for silence.

Three readings get confused with each other and are kept apart. A goal that exists is not a goal
that activated. A goal that activated is not continuation observed actually happening. And a parent
blocked by a goal compatibility problem is neither idle nor complete: reporting it as either hides
the one fact that explains why nothing is moving. Say which of the three you observed and when.

A status call creates, activates or repairs no goal. Find the task by its stable coordinator binding
in that parent's coordination record, and route the goal work as [crw-loop](../../crw-loop/SKILL.md)
lifecycle work, because that is the operation owner for a goal's creation, activation and recovery;
`crw-run` executes the project and does not make a parent's goal. Where the record names no
coordinator, or ownership cannot be established, it is Jun's decision and goes in the report as one.
That routing is about who is told about a broken goal, which is a different question from which
role carries which lifecycle by default; [Start policy](../../crw-run/references/start-policy.md)
owns the second. A goal-free parent is the default rather than a defect, so finding one is not a
finding at all.

Per delivery, from GitHub: the current head, the CI attempt that applies to that head, the reviews
paged to the end, whether the pull request actually merged into its intended target, and whether
anything was installed or demonstrated afterwards. Use
[Merge readiness](../../crw-run/references/merge-readiness.md) for what the checks and reviews
establish and [Implementation Done](../../crw-plan/references/integrations.md#implementation-done)
for what a landing requires. Codex automatic review is disabled and is recorded as not required,
never as a review that passed.

## Surface the disagreements, not the inventory

A list of green rows is not a report. What makes a midpoint check worth asking for is the places
where two levels disagree, and four of them recur:

- The parent is idle while its children's work is still outstanding AND nothing will bring it back.
  Idle alone is not the blocker: under event-driven handoff a parent answers, returns to idle, and
  a delivered event starts its next turn, which
  [OPS-8.1](../../crw-run/references/operations.md#ops-81-parent-continuation-and-waiting) owns.
  What makes that path a verified one is the readiness list in
  [Before a parent may wait idle](../../crw-run/references/start-policy.md#before-a-parent-may-wait-idle),
  read from there rather than from a shorter copy here, because the fact it calls the one usually
  skipped is the one a checkpoint skips too. The blockers are an idle parent with no such path,
  whose child's result sits unread until someone restarts it by hand; a parent
  [OPS-8.2](../../crw-run/references/operations.md#ops-82-busy-paused-cancelled-and-archived-parents)
  puts out of reach, by its own state, its goal status or its assignment's, which nothing resumes
  automatically whatever arrives for it; and a delivery already held after its retry budget ran out,
  which [OPS-8.5](../../crw-run/references/operations.md#ops-85-the-goal-free-parents-wake-path)
  records as terminal and recoverable only by a fresh execution generation. That last one reads as
  an ordinary quiet queue unless held rows are read, so read them.
- A child ended its turn waiting on a judgement. Its work is not blocked by a defect; it is blocked
  by an unanswered question, and the question has an owner who has not seen it.
- The newest report and the newest pull request describe different states. The report is stale, or
  the pull request moved after it, and the summary everybody is reading is no longer true.
- A tracked item sits outside the project it belongs to: an issue read back with no project or the
  wrong one, or an incident the relay's product routing held because no single product, owner or
  project could be chosen. The record and the plan disagree about where the work lives. Where an
  existing rule settles it, it is internal coordination; where it needs a choice between products
  or projects, it is a decision for Jun. Routine accumulation under records that already exist is
  summarized in one line rather than listed.

Report those as the main blockers, before the per-project detail. Then split what is waiting into
three: a normal dependency wait, which is nobody's failure and needs only its reason; the questions
an internal coordinator resolves inside scope it already has; and the ones that genuinely need a new
decision from Jun. Sending the second kind up wastes his attention, and holding the third kind back
stalls the work silently.

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
| `idle` at the current read, holding an unprocessed child result, a decision it was asked for, or a blocker observed cleared, with no handoff for it still in flight | resume it through the existing supported path, carrying the restoration block. The item's current state decides this row, not how recently continuation was last seen working, and an accepted handoff whose turn has ended without consuming the item is a failed delivery rather than a reason to stay silent. A delivery already held after its retry budget ran out is not revived by that resume: the parent's own drain on entry is what finds it, and [OPS-8.5](../../crw-run/references/operations.md#ops-85-the-goal-free-parents-wake-path) makes a fresh execution generation the only recovery |
| `idle` with nothing to coordinate, waiting on a real dependency | record the dependency and what will release it, and send nothing. A dependency wait is not a stall |
| paused or archived, holding an assignment [OPS-8.2](../../crw-run/references/operations.md#ops-82-busy-paused-cancelled-and-archived-parents) records as inactive, or under an explicit no-contact or report-only limit | no contact. Report the state and the exact resume action its owner has to take. Cancellation is an assignment status there rather than an observable task state, so report it as the assignment's and never infer it from a turn that merely ended |
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

A midpoint check is a question somebody asked, which is a different path from the automatic
notices a parent sends upward. Those are held to a completion, a new real block and a decision
only Jun can make, so the channel being quiet is not a reason to answer this with less: the
suppression is of unsolicited turns, not of answers. What each form carries and what the five
delivery states mean are in
[the message both relations are read by](../../crw-plan/references/integrations.md#the-message-both-relations-are-read-by).

## Cases

### M1 One supervision, several parents, mixed task states

Observed: the approved set holds three projects; one parent is `active` with a turn, one is
`idle`, one is `notLoaded`, and their children are spread across all three states.
Action: report each project's own state. For the idle parent with a working child, read its wake
path first and call it a blocker only where that path is missing, where OPS-8.2 puts the parent or
its assignment out of reach, or where a delivery is already held under OPS-8.5.
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
Action: on the checkpoint branch, re-read the result's state and the parent's. Send nothing where
that read shows the result is no longer outstanding, where the parent has entered a turn that will
consume it, or where a handoff carrying the same item is still in flight. Where such a handoff was
accepted but its turn has since ended with the result still unprocessed, that delivery failed:
record it as failed and send a recovery handoff under a new identity. Otherwise resume that parent
through the existing supported path with the
restoration block, then read its state again and report both what was sent and what was observed
after. A parent that is idle now with the result still unprocessed is resumed whether or not its
continuation was seen working at some point after the result landed. On the reading branch, report
the unprocessed result and send nothing.
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

### M10 The request itself carries a limit

Observed: this request carries report-only, read-only, pause or no-contact.
Action: contact nobody, anywhere. A stated limit outranks a standing approval and covers every
parent in scope. Report each project's state and what would release it.
Preserved: the user's decision, exactly as given.

### M10b One parent is paused while the others are not

Observed: no limit on the request, but one parent in the approved set is paused or archived, or
holds an assignment OPS-8.2 records as inactive, and the others are ordinary.
Action: that parent is not contacted and nothing resumes it; report its state and the exact resume
action its owner has to take. The other parents are handled on their own rows as usual. A pause on
one task is a decision about that task, not a stop order for the initiative, and holding back
approved follow-ups elsewhere because of it is its own failure.
Preserved: the paused task untouched, and the rest of the approved work still moving.

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
not report the project as progressing on the strength of a receipt. That silence lasts only while
the handoff is in flight: once its turn has ended with the item still outstanding, the delivery
failed and the next checkpoint recovers it under a new identity rather than matching the old
receipt forever.
Preserved: the five facts as five, which is the whole point of writing them separately, and a
recovery path for a handoff that was accepted and then went nowhere.

### M13 The parent is quiet and the reason is its goal

Observed: a parent has no running turn. In one variant its goal is active with continuation
working; in another it has no goal, or a paused one, or one that cannot activate.
Action: read its effective workflow, then report the turn and the goal as two readings. A goal-free
parent is in the default mode and its absent goal is not the stall; what to check there is whether
its delivery path is reaching it. A Loop parent whose goal is missing or unactivatable is a mode
mismatch and the report leads with that rather than calling it idle, but do not infer from it that
nothing can reach the parent: an absent goal is deliverable under OPS-8.2, so read the delivery
path and the waiting events separately before naming a cause. The goal statuses that genuinely
receive nothing whatever arrives are the delivery-blocking set
[OPS-8.2](../../crw-run/references/operations.md#ops-82-busy-paused-cancelled-and-archived-parents)
classifies, read from there rather than listed again here so the two cannot diverge. Either way
the parent needs nothing only while
nothing is outstanding for it; where something is, the item decides under the precedence above and
a working goal is not a reason for silence. Distinguish a goal that exists, a goal that activated,
and continuation observed actually happening; say which you saw.
Preserved: the difference between quiet and stopped, between a supported mode and a fault, and
between a healthy goal and a delivered item.

### M14 A parent needs a goal it does not have

Observed: the checkpoint establishes that a parent cannot continue because of its goal state.
Action: confirm first that this parent is one a goal is expected of, which since CRW-165 is only a
parent an explicit Loop was requested for, because a goal-free parent
is not stalled by lacking one. For a Loop parent, report the block and route it as crw-loop
lifecycle work to the task named by the parent's stable coordinator binding, or to Jun where none
is named. Do not create, activate or repair a goal from a status call. A checkpoint moves approved
work; it does not change how a task is run.
Preserved: the goal lifecycle's own owner, and an accurate reason for the stall.
