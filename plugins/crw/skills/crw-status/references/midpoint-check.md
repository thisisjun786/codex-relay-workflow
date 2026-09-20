# Midpoint check

One word from Jun, 중간점검 or '현재 어디까지 됐어?', arriving in a task that supervises an
initiative, and the answer is a short report covering everything that supervision holds. This file
is that entry. The roles it walks are the shared
[supervisor, parent and child scope](../../crw-plan/references/integrations.md#supervisor-parent-and-child-scope),
and the binding it reads is the one
[Initiative supervision](../../crw-run/references/initiative-supervision.md) established, which
already records that a how-is-it-going request reads existing records and wakes nothing.

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

## The check reads and hands back

An independent check queries and nothing else: no Linear write, no task created or resumed, no
message, no wake, no installation, no schedule change.

A check requested during an authorized run cancels nothing. The supervisor and the parents keep
every continuing obligation they had before the question arrived, and control returns to whoever
was executing. Where Jun asks for follow-up action as well, hand the scoped request to the owner
that already has it, [crw-run](../../crw-run/SKILL.md) or [crw-check](../../crw-check/SKILL.md),
rather than acting from the check. Being able to see that a project is stalled is not authority to
start it, to wake its parent, or to create anything.

## Report

Core conclusion, then one short line per project including its
[schedule verdict](schedule-progress.md), then what is waiting split between internal coordination
and Jun's decision, then the evidence checked and the items left unverified. Korean, short, in that
order. Nothing is described as handled unless this check performed it, which it did not.

## Cases

### M1 One supervision, several parents, mixed task states

Observed: the approved set holds three projects; one parent is `active` with a turn, one is
`idle`, one is `notLoaded`, and their children are spread across all three states.
Action: report each project's own state. For the idle parent with a working child, read its wake
path first and call it a blocker only where that path is missing or the task is paused.
`notLoaded` is neither running nor finished, so read that task again once before
reporting it, as the bridge contract requires; a task that was merely not loaded a second ago may
read `active`. Where it stays `notLoaded`, keep that exact value rather than translating it into
idle or stopped, and report its last turn as unverified. Do not message or wake any of them.

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
