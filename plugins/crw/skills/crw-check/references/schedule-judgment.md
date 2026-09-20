# Schedule judgment

Use when a check covers subjects that carry agreed dates, which the contract defines
as projects, milestones and issues. An initiative-level request is judged through those
subjects rather than as a subject of its own. It consumes the [Schedule baseline contract](../../crw-plan/references/integrations.md#schedule-baseline-contract),
which [crw-plan](../../crw-plan/SKILL.md) owns, together with the delivery evidence
this skill already pins. Where a subject's fields are absent, report that part of the
schedule as unchecked rather than reconstructing them here. It decides whether each
subject is ahead of, on, or behind its agreed date, and which later result a slip
actually blocks. It does not write a schedule, define a contract
field, keep a second copy of any date, or replace the criterion comparison it runs
beside. Read [Merge readiness](../../crw-run/references/merge-readiness.md) for the
CI and review evidence a delivery claim rests on; this procedure consumes that
evidence and does not restate it.

## Pin the schedule

Read, per subject, the contract's Subject, Schedule baseline, Baseline record,
Planned start, Current target, Timezone, Target nature, Actual start and finish,
Change source and Prerequisites, under those names and with its value words as
written, because two operations that paraphrase them stop agreeing about the same
item. Add what this skill already pins: the delivery level that subject's own criteria
require, the evidence at that level with both its instant and the time it was
observed, the observation instant, and the judgment time. The observation instant is the query time, because the evidence is only as
current as that read. Report both times; when their calendar dates differ in the
schedule timezone, query again before ruling or state that the verdict stands as
of the query date.

A subject is judged only where its Target nature is `confirmed` or `provisional`,
the two the contract gives a date. `undetermined` and `awaiting authority` carry none:
they are kept as evidence, listed under unchecked scope, and never judged or
aggregated. A Planned start and an Actual start are not targets either, and an actual
start never becomes one. A `provisional` target that has passed is still late, with
its nature on the report line so a moved planning target is not read as a missed
commitment. Read Target nature; never invent a target for a subject that has none.

A subject the records show was cancelled by an accepted change also leaves the current
judged set. The contract keeps a cancelled subject's dates and gives it no new target,
so a preserved date is not an outstanding delivery, and its Change source already
carries the cancelled IDs that distinguish an accepted removal from a failure to
deliver. Report it as removed, with that change source, rather than judging it. It
still belongs to the baseline axis as something the earlier plan carried, so the scope
change stays visible on both sides rather than quietly shrinking the past.

Name the checked and the unchecked scope for the whole request, not only for the
results that happened to be decidable. A result left unchecked is a gap in
coverage and is reported as one.

## Fix the deadline

The contract makes these targets calendar dates, so a target means the end of that
day in the Timezone and this procedure never invents a time of day for one. A subject
is past due exactly when the calendar date of the observation in that Timezone is
later than its target date, so the target day itself is never late. Normalize every
evidence instant and the observation into the Timezone before taking their calendar
dates, which is what decides a landing late on the target day in one zone and after
midnight in another. Without a timezone the deadline cannot be fixed: that axis is
undecidable with the reason named, and the result is listed under unchecked scope.
Never supply a timezone from the host, a user profile, or memory.

## Decide achievement

A result is achieved only when evidence exists at the delivery level its own
criteria require. This skill already separates source existence, passing checks,
integration, deployment, and observed behavior, and
[Implementation Done](../../crw-plan/references/integrations.md#implementation-done)
already makes merge, installation, and live behavior three separate claims; apply
those rules here rather than restating them. A lower level reached early is an
observation, never an achievement: a merge that landed well before the date does
not achieve a result whose criteria require installation, and the report keeps both
facts, the early merge and the unproven installation.

Issue counts, Done ratios, milestone percentages, a finished agent turn, a
merge-ready flag, and a green check establish neither achievement nor speed. Neither
does a status timestamp produced by a status correction or a bulk edit, which records
when someone fixed the register rather than when the work happened; where two
admissible dates disagree, the earlier evidenced one stands. The
achievement instant is the instant of the required-level evidence, never the time
the check read it.

## Order the verdict

Judge each result once per axis, first match wins.

| Row | Condition | Verdict |
|---|---|---|
| 1 | This axis has no date for this subject, or no Timezone, or the evidence needed to decide is stale, or this is the current axis and the Current target disagrees with the latest Change source's after value | Undecidable, with the reason |
| 2 | Achieved on an earlier calendar day than the target | Ahead |
| 3 | Achieved on the target day | On plan |
| 4 | Achieved after the deadline | Late, reporting actual minus target |
| 5 | Not achieved, observation after the deadline | Late, reporting elapsed time only |
| 6 | Not achieved, observation at or before the deadline, and a listed risk observation holds | At risk, with its cause and wait class |
| 7 | Not achieved, observation at or before the deadline | On plan, as nothing due yet |

Evidence is stale when it was not read at this observation and the result was not
terminal when it was last read. A passed deadline with stale evidence is row 1, not
row 5, because the result may have been achieved since; the report carries the last
known state and the time it was observed.

Ahead is reachable only through row 2 and needs real early achievement at the
required level. Rows 3 and 7 are both on plan and are not interchangeable: row 3
names the achievement, row 7 says only that nothing was due by the observation.
Neither promises anything about a date still ahead, and being before a deadline is
never reported as being on track to meet it.

## Report the difference

Differences are whole calendar days in the Timezone. An achieved subject reports
actual minus target, negative for early, zero on the target day, positive for a late
completion. An unachieved past-due subject reports elapsed days only, the observation's
calendar date minus the target date, at least one.

Do not produce a projected completion date, a remaining-work estimate, or a velocity
or rate on any branch. An unachieved result reports how long it has been late and
nothing about when it will land.

## Name the risk

Before the deadline, only these observations make a result at risk, and each carries
one wait class. A required predecessor that is unachieved and is itself late or at
risk on the axis being judged, or that is unachieved and whose target date on that
axis falls after this subject's deadline, so the ordering cannot hold: internal
coordination wait. Equal dates are not that defect, because two subjects sharing a
target day can both land by the end of it. An achieved predecessor is never a risk, whatever its
date said. The required delivery level
unreached while a lower one has been reached: internal coordination wait, or
external failure where an attempt was made and failed outside the team's control. An
approval or decision the criteria require and the user has not given: user decision
wait. An active pause or blocker, classified by its recorded source — the user
paused it, user decision wait; the team paused it, internal coordination wait; an
outage stopped it, external failure.

Where more than one observation holds, the result is still at risk once any does,
and every observation that holds is reported with its own cause and wait class
rather than one being chosen over the others. A predecessor merely unfinished while
still inside its own deadline is not one of these, or every dependent chain would sit permanently at risk. A predecessor that is
undecidable is not one either; it leaves the successor's impact unverified. Any
other observation is a note in the report and changes no verdict.

## Aggregate the scope

Aggregate over every result with a decidable verdict that is due at or before the
observation, or achieved, or carrying a risk observation. The overall verdict is the
most severe in that set: late, then at risk, then on plan, then ahead. Ahead carries
one condition the severity order cannot express, because the set deliberately omits
subjects that are not due yet: the scope is ahead only when every decidable subject in
it is achieved and at least one of them landed early. A scope that still owes
unfinished work is on plan at best, however early its finished parts arrived, and one
early subject beside an on-time one is on plan for the same reason. The early landing
does not disappear: it is reported as that subject's own lead under the report order.

Where the set is empty, decide in this order. Where no result in the requested scope
has a decidable verdict at all, the overall result is undecidable and the scope is
partially checked; it never defaults to on plan. Otherwise, where any undecidable
result is already due, the overall result is likewise undecidable and partially
checked, because a deadline that passed unchecked cannot be reported as nothing due
yet. Otherwise the scope is on plan, nothing due yet.

Undecidable never enters the severity order, but it does bound how positive the overall
verdict may be. Where any subject that is already due is undecidable, the overall verdict
is never on plan and never ahead: a late or at risk member still decides it, because a
known delay is the more actionable fact and must not be softened by missing evidence
elsewhere, and otherwise the overall is undecidable with the scope partially checked. A
scope cannot report schedule success over a deadline nobody checked. Report the
undecidable subjects beside the overall verdict as unchecked scope, naming how many and
why, so an undecidable subject cannot hide a late one and a late one cannot hide missing
coverage.

## Judge both axes

Judge every subject against the Schedule baseline and against the Current target, by
the same rules. A subject belongs to an axis when the contract records that axis's
date for it, and a Schedule baseline recorded `unconfirmed`, meaning a target exists
but no Baseline record establishing it can be found, makes the baseline axis
undecidable for exactly that reason. Because that baseline is each subject's own earliest
established target, a subject that entered scope later still has one, set by the record
that first gave it a target, and it is judged on both axes like any other. Its Change
source carries when it entered and under whose decision, and the report says so, so a
subject added mid-flight is never counted into an aggregate that claims to describe the
plan as it stood before it arrived, and two scopes are never compared at one rate.

Report both verdicts whenever they differ, with the change source and the instant
the current date took effect. Moving a target date never removes the baseline
verdict or its quantity, and results belonging to different scopes are never
compared at one rate. A pause is a change source and a cause, never a credit:
paused time is not subtracted from elapsed time.

## Trace the impact

Walk forward along required-predecessor edges only, and read two facts about the
predecessor separately, because they fail independently: whether it is achieved, and
whether its schedule can be decided. Four branches follow and they are exhaustive.
A subject known to be achieved blocks nothing, however late it landed, so no walk starts
from it. A subject known to be unachieved and judged late or at risk blocks, and its
successors are named. A subject known to be unachieved whose verdict is undecidable,
whether because a date or a Timezone is missing or because its Current target disagrees
with its latest Change source, blocks in a way this check cannot size: walk it and report
its successors' impact unverified, naming the reason. A subject whose achievement itself
is unknown, which is what stale evidence leaves behind, is walked the same way and
reported unverified for that reason instead. Only a subject that is on plan and
unachieved blocks nothing yet, so work waiting on it is never reported as blocked merely
because it has not finished.
For each starting subject, name the successors it reaches through at least one path
made entirely of required edges,
and the nearest unachieved successor milestone or project. Another path that is not
required does not cancel that dependency, and a successor reachable only through a
path that is not required is not blocked. Where a successor has several required
predecessors, it is blocked by the ones actually unachieved and the others are named
as not blocking. A project or initiative is affected only where the walk reaches its
own completion condition; waiting on part of a result is never reported as blocking
the whole project.

Do not name the most recently touched or highest-numbered result unless it lies on a
walked path. Where a date, a relation kind, or a predecessor's own state cannot be
read, leave that edge's impact unverified and name the missing input. Where the
graph carries one of the defects the contract checks for, a cycle among Prerequisites,
a prerequisite whose target falls after the target of the subject needing it, an issue
carrying a milestone's target from outside that milestone, or a required prerequisite
absent altogether, report it as a finding and leave the impact it distorts unverified
rather than judging through it.

## Record the judgment and reuse it

The judgment goes where this skill already records its result and nowhere else: the
linked Linear coordination document under an existing management assignment, the
summary a bounded helper returns to its coordinator, or the unsynced update when the
scope is read-only. A read-only or report-only scope returns findings and never
gains a write merely to leave a reusable record. Keep no second schedule store.

Where the record reports on the schedule data itself, use the contract's two states as
it fixes them: `recorded in the data` where dates, links and relations were written and
read back, and `not applied in the view` where a scale, an ordering or a connection line
the connector does not expose was left alone. Neither stands for the other.

Record what identifies the judgment: the subject IDs, the schedule fields as read,
the criteria set identity, the implementation revision, the required level with the
evidence instant and observed-at at each level, both axis verdicts with their
quantities, the cause and wait class, the impact list, and the query and judgment
times. The schedule verdict is its own field. It is never a per-criterion
disposition: verified, needs_changes, and unverified remain the three recordable
ones, and a late schedule does not turn a satisfied criterion into needs_changes.

Reuse rests on two separate conditions. An event that already happened, such as a
merge that landed, stays valid, keeps its original observed-at, and is not gathered
again to answer a schedule question. A claim about a current state, such as an
installation being present or absent or an approval outstanding, holds only for the
query that observed it; a later demand for a current verdict either observes it
again or reports that result partially checked with the earlier observed-at. A
verdict itself is reused at the same revision, the same criteria, and the same
schedule fields, which is this skill's existing reuse rule rather than a new one,
and a later reader recomputes the deadline and the difference from the recorded
dates and instants instead of repeating the audit. These conditions are what a consumer
needs in order to take the verdict rather than rerun the criterion comparison; whether
any operation does take it is that operation's own contract, and whether a separate
status report reaches the same verdict is a property of that operation. This section
defines the producing side and establishes neither.

## What the check refuses

Record the verdict, the owner each affected next step returns to, and the route.
Refuse to move a date, rewrite a schedule, change a Done state, start or resume
execution, create a worker, or repeat a notification beyond the one delivery the
record already makes; retrying a failed write is that same delivery, not a repeat.
Return needed schedule changes to [crw-plan](../../crw-plan/SKILL.md) and already
accepted corrections to the existing owner by the path this skill already defines,
and put a needed user decision in the report's last block. A passed or approaching
deadline changes no criterion, no review requirement, and no delivery level.

Refusing to own execution is not the same as narrowing the work around it. Inside an
approved initiative or project execution, a checkpoint includes the parent's
confirmation and whatever already-approved successor work that approval covers, so
being called here does not turn an approved continuation into an independent
read-only audit. Route a schedule or evidence mismatch by the level that owns it. A mismatch contained
in one project returns to that project's parent, which settles within the approval it
already holds whether the approved successor proceeds. Where the mismatch orders work
across projects, return it through the supervisor that owns both. Where the two projects share no
supervisor or answer to different ones, follow
[direct coordination between parents](../../crw-plan/references/integrations.md#direct-coordination-between-parents):
the parents settle what they can between themselves, hold only the unsettled part
while independent work continues, and raise that pair-level decision to the user,
because neither supervisor acquires authority over the pair and none is invented. A
requirement confined to one of those projects is raised with that project's own
supervisor instead. Every route holds the same line: a project parent owns sequencing
inside its own project and never decides whether another project proceeds. A bounded helper returns the finding
to its caller rather than contacting an owner itself, as
[supervisor, parent and child scope](../../crw-plan/references/integrations.md#supervisor-parent-and-child-scope)
already requires. Do not become the new owner of technical execution or of a merge,
and do not build a second path that resumes work. Explicit report-only, read-only,
pause, and no-contact limits still win, and an independent audit keeps its own
lookup scope.

## Order the report

A progress or interim report leads with the schedule: the overall verdict with the
query and judgment times, then the notable lead and delay per result with its
baseline date, current date, achievement or last known evidence, and difference,
then the affected next steps, then internal coordination waits and user decisions.
Keep it short. Where a mismatch was routed, keep three facts apart that collapse
easily into one: that a correction request was sent, that the owning parent actually
resumed, and that a result was verified. Only the third is delivery, and the first
never implies the second. A completion check keeps this skill's existing lead, the verification
result and the action actually taken, and places the schedule block immediately
after it.

## Judgment cases

Each case states what was observed, the expected verdict, and the plausible wrong
answer it rules out. Derive the expectation from the observation before reading the
rules back; that derivation is the check, and matching wording is not. These cases
establish that the procedure agrees with itself. They are not evidence of runtime
behavior, and they do not establish that a separate status report reaches the same
verdict. Timezone is Asia/Seoul and Target nature is `confirmed` unless stated. Each case
lists every input that bears on its verdict, so an observation it does not list did
not hold, and where it names only a Current target the Schedule baseline carries
the same date with no Change source, leaving both axes in agreement and only the
current one worth discussing.

1. **Early completion.** M1, current target 2026-09-20, required level installed,
   installation receipt 2026-09-18T14:00, observed 2026-09-21T09:00. Ahead, actual
   minus target −2 days. Not on plan, which would ignore the earlier calendar day. Where the scope
   also holds I-21, due 2026-10-01 and unachieved with no risk observed, the scope
   overall is on plan, nothing due yet, while M1's own two-day lead is still reported
   as its lead. Not a scope called ahead while work it still owes has not landed. Where I-21 is
   instead due 2026-09-19 and undecidable because its evidence was not re-read, the
   overall is undecidable with the scope partially checked, not on plan and not ahead,
   while M1's lead is still reported. Not schedule success claimed over a deadline
   nobody checked.

2. **Normal progress.** I-7, current target 2026-09-30, required level merged,
   unachieved, no risk observation, observed 2026-09-21. On plan, nothing due yet,
   with no claim about 09-30. Not ahead, which needs a real early achievement.

3. **Overdue and incomplete.** I-8, current target 2026-09-15, required level
   merged, no merge, observed 2026-09-21T09:00. Late, elapsed 6 days, no
   actual-minus-target. Not a projected completion date or a rate.

4. **A late predecessor before the successor's deadline.** P, current target
   2026-09-15, unachieved; S, current target 2026-09-28, with P as a required
   predecessor; observed 2026-09-21. P late by 6 days; S at risk, cause a required
   predecessor late, wait class internal coordination wait, impact S blocked by P.
   Not S on plan because its own date is still ahead. Where instead P's Current target
    is 2026-09-28, the same day as S's, and P is unachieved with nothing else observed,
    S is on plan: two subjects sharing a target day can both land by the end of it, so
    equal dates are not an ordering that cannot hold. Not at risk from a comparison
    that treats equal as late.

5. **No dates.** I-9 has neither a baseline nor a current target date and is the
   only result in scope, observed 2026-09-21. I-9 undecidable on both axes, reason
   no target date, listed under unchecked scope; the overall result undecidable and
   the scope partially checked. Not an overall on plan produced by an empty set.

6. **Future dates with no interim results.** A project whose current target is
   2026-10-31, required level installed, nothing due earlier, no delivery evidence,
   observed 2026-09-21. On plan, nothing due yet, stating that the claim covers only
   up to the observation. Not on track to finish 2026-10-31.

7. **Merged but not installed.** I-10's criteria require installation; the PR merged
   2026-09-18T10:00; current target 2026-09-25; no installation evidence; observed
   2026-09-21. Not achieved; at risk, cause the required level unreached while the
   source is merged, wait class internal coordination wait, and the report keeps
   source merged 2026-09-18, seven days before target, installation unverified. Where
   an installation attempt on 2026-09-20 failed because an upstream registry was
   unreachable, still at risk with wait class external failure. Where that failed attempt
   coincides with an approval the criteria require and the user has not given, still
   at risk with both observations reported, one as external failure and one as user
   decision wait, rather than one class chosen over the other. Not ahead or complete
   read from the early merge.

8. **Stale evidence past a deadline.** I-11, current target 2026-09-15, required
   level merged; the last read, 2026-09-10, showed it unachieved and not terminal; it
   was not read again at this query; observed 2026-09-21. Undecidable, reason evidence
   not re-read since 2026-09-10, carrying last known unachieved at 2026-09-10. Not
   late, which asserts a state nobody observed. Where the scope holds only
   this I-11 and a decidable I-22 that is unachieved, due 2026-10-10 and carrying no
   risk, the aggregation set is empty because I-22 is neither due nor achieved nor at
   risk, yet the already-due I-11 is undecidable, so the overall result is undecidable
   and the scope partially checked. Not nothing due yet, which would report a deadline
   that passed unchecked as though nothing had.

9. **Late against the baseline, on plan against a moved target.** M2, baseline target
   2026-09-10, current target 2026-09-25, moved 2026-09-12, change source replan with
   its decision link, required level merged, unachieved, observed 2026-09-21. Baseline
   axis late by 11 days; current axis on plan, nothing due yet; both reported together
   with the change source and the move instant. Not the current axis alone, which
   erases the recorded delay. Where M2's Current target reads 2026-09-25 while the
    latest Change source records an after value of 2026-09-22, the current axis is
    undecidable until the two are reconciled, and the baseline axis still reports late
    by 11 days, because the Schedule baseline is independent of that disagreement. Not
    both axes lost to one conflict, and not either side silently preferred.

10. **A subject added after the plan began.** The project's plan was recorded
    2026-09-01 and I-6 entered its scope on 2026-09-10, when the record that added it
    set its first target of 2026-09-28; that is its Schedule baseline. Its Current
    target is 2026-10-05 after a later move and it is unachieved. Observed 2026-09-21.
    I-6 judged on both axes like any other subject, on plan against both, with the
    Change source showing it entered on 2026-09-10 and under whose decision, and the
    report keeping it out of any aggregate that claims to describe the plan as it stood
    on 2026-09-01. Not a subject left without a baseline because it arrived late, and
    not one folded into the earlier scope so the two are compared at one rate. Where
    instead I-6 carries a target but no Baseline record establishing it can be found,
    its Schedule baseline is `unconfirmed`, that axis is undecidable for that reason
    and the scope is partially checked, while the current axis still decides on plan
    against 2026-10-05. Not the whole subject undecidable because one axis is. Where instead I-6 carried a
    confirmed 2026-09-20 target and an accepted change cancelled it on 2026-09-15, it
    leaves the current judged set on 2026-09-21 and is reported as removed with that
    Change source, neither late nor counted into the overall verdict, while the
    baseline axis still shows the earlier plan carried it. Not an accepted scope
    removal read as a missed delivery.

11. **Pause.** I-12, current target 2026-09-15, required level merged, paused
    2026-09-12, source recorded as the user's decision pending a product answer, no
    re-approved date, observed 2026-09-21. Late, elapsed 6 days, cause paused
    2026-09-12 by user decision, wait class user decision wait, the paused interval
    not subtracted. With the current target at 2026-09-30 instead, at risk with the
    same cause and class. Not paused days subtracted, and not a pause read as an
    extension.

12. **Timezone at the day boundary.** I-13, Current target 2026-09-25, Timezone
    Asia/Seoul; the merge its criteria require landed 2026-09-25T23:30+09:00; the
    reader's own clock shows 2026-09-26T00:30Z. On plan, because the achievement's
    calendar date in the Timezone is the target day. Not late read from the observer's
    date. With no Timezone recorded, undecidable on that axis for that reason and
    listed under unchecked scope. Not the host timezone used silently. Where the same
    merge instead landed 2026-09-27T09:00+09:00, late with actual minus target of
    +2 days, reported as a completed result. Not elapsed days, which is the quantity
    for a subject still unachieved.

13. **Parallel issues where only one blocks.** I-15, I-16, and I-17 run in parallel,
    all requiring a merge. I-15's current target is 2026-09-15 and it has no merge;
    I-16's is 2026-09-28 and I-17's is 2026-09-30, both unachieved with nothing else
    observed. Only I-15 is a required predecessor of M3, whose current target is
    2026-10-05. The project's completion condition requires M3 and M4, and M4 has no
    required predecessor among the three. Observed 2026-09-21. I-15 late by 6 days;
    I-16 and I-17 on plan, nothing due yet; M3 blocked by the unachieved I-15, with
    I-16 and I-17 named as not blocking; the project affected at M3 while M4 and the
    parallel issues continue, and not reported blocked. Not whole-project blockage,
    and not I-17 named because it is the newest. Where M3 is reachable from
    I-15 both through that required edge and through a second path that is not
    required, M3 is still blocked, because one wholly required path is enough, and
    where the only path from a late I-16 to M3 is not required, M3 is not blocked by
    I-16. Where M3 additionally requires I-16 and I-16 is achieved, M3 is blocked by
    I-15 alone and I-16 is named as required but satisfied. Not a successor dropped
    because some other path to it was not required. Where M3's own target date is absent, or the
    relation between I-15 and M3 cannot be read as required or not, that edge's impact
    is unverified with the missing input named, and where I-16's own state cannot be
    read, its contribution to M3 is unverified rather than assumed blocking or
    assumed clear. Not an impact asserted over inputs nobody could read. Where I-15 is instead on plan against
    a 2026-09-28 target and simply unfinished, no walk starts from it and M3 is not
    reported blocked, because a subject that is on plan blocks nothing yet. Not every
    unfinished predecessor turned into a blocked successor. Where I-15 instead completed
    2026-09-16 against its 2026-09-15 target, it is late by +1 day and M3 is still not
    blocked by it, because a subject that has landed blocks nothing however late it was.
    Not a historical delay read as a current block. Where I-15 is known unachieved but its
    Current target reads 2026-09-28 while its latest Change source records 2026-09-24,
    its verdict is undecidable and M3's impact is reported unverified naming that
    disagreement, rather than M3 being dropped from the report or asserted blocked.

14. **A Target nature that carries no date.** I-18 carries only an Actual start and
    a Target nature of `undetermined`; I-19's Target nature is `awaiting authority`.
    I-19 is a Prerequisite of S, whose Current target is 2026-09-25, which is
    unachieved and whose criteria need the decision I-19 waits on. Observed
    2026-09-21. I-18 and I-19 both outside the judged set, kept as evidence and listed
    under unchecked scope; S at risk, cause an approval the criteria require and the
    user has not given, wait class user decision wait. Not an Actual start read as a
    deadline, and not a target invented for a subject whose Target nature gives none.

15. **Reusing a recorded judgment.** I-20's criteria require installation. A judgment
    recorded 2026-09-21T09:00 observed the merge landed 2026-09-18T10:00 and
    installation not yet present, with current target 2026-09-30, so the verdict was
    at risk, cause the required level unreached while the source is merged, wait class
    internal coordination wait. A later reader at 2026-09-29 holds the same revision,
    criteria, and schedule fields and wants a current verdict. The merge is reused with
    its original observed-at and not gathered again; the installation-not-present claim
    is not carried forward, so it is observed again at this query or I-20 is undecidable
    on that reading, reason the installation state not observed at this query, and the
    scope is reported partially checked with the 2026-09-21 observed-at; the verdict and its cause are
    recomputed from the recorded inputs rather than re-audited. At 2026-10-01, past the
    target and with no new evidence, the verdict is not carried forward and I-20 is
    undecidable pending a re-read rather than late. Not a stale verdict carried past
    the deadline, not a current-state claim treated as a settled event, and not the
    full criterion audit rerun to answer a schedule question. Where instead I-20 had
    completed its required installation on 2026-09-22 against a target of 2026-09-30
    and was recorded ahead by 8 days, and the approved current target later moves to
    2026-09-18, the installation receipt is reused unchanged with its original
    observed-at while the current-axis verdict is recomputed to late by 4 days. Not
    the recorded ahead carried forward on the ground that the achievement itself did
    not change. Where instead I-20 was recorded achieved because its required
    installation was observed present on 2026-09-22, and a current verdict is asked
    for on 2026-10-01 without that state being read again, the achievement does not
    stand on a settled event: the installation is observed again, or I-20 is
    undecidable carrying the 2026-09-22 observed-at. Not a past presence reused as a
    current one, which would report a service nobody has looked at.
16. **A checkpoint inside an approved execution.** An initiative execution the user
    already approved is running, and its project parent holds that approval. A
    checkpoint here finds M5 late by 4 days against a current target of 2026-09-17,
    with its successor M6 unachieved and reachable from M5 through a required edge.
    Observed 2026-09-21. Nothing in the request restricts this check to report-only,
    read-only, paused, or no-contact. M5 late by 4 days; M6 named as blocked; the
    mismatch returned to the owning project parent, which decides inside its existing
    approval whether the approved successor proceeds; and the report keeping the
    correction request it sent apart from whether that parent actually resumed and
    from any result since verified. Not this check taking over the execution or the
    merge, not a second path that resumes the work itself, and not the surrounding
    approved continuation narrowed into an independent read-only audit because a check
    was called. Where the request does carry an explicit report-only, read-only,
    pause, or no-contact limit, that limit wins and the finding is returned without
    contact. Where instead M5 sits in project A and the approved successor waiting on
    it is in project B, the finding returns through the supervisor owning both
    projects, or, where no single supervisor owns them, through direct coordination
    between the two parents with any unresolved ordering escalated to the user. Where projects A and B answer to
    different supervisors, the parents settle what they can, hold only the unsettled
    ordering, and raise that decision to the user, because neither supervisor holds
    authority over the pair; a requirement confined to project B alone is raised with
    B's own supervisor instead. Not one supervisor settling an ordering over a project
    it does not own, and not a supervisor invented or rebound to cover the pair. Not project A's parent deciding whether project B
    proceeds, which is an ordering it does not own. Where this check runs as a
    bounded helper for a coordinator rather than holding the assignment itself, the
    finding goes back to that coordinator and the helper contacts no owner directly.
