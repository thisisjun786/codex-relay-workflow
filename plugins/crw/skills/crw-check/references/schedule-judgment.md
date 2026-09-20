# Schedule judgment

Use when a check covers a project, an initiative, or an issue that carries agreed
dates. It consumes the [Schedule baseline contract](../../crw-plan/references/integrations.md#schedule-baseline-contract),
which [crw-plan](../../crw-plan/SKILL.md) owns and which CRW-143 adds to that
reference, together with the delivery evidence this skill already pins. Until that
section lands, the fields have no shipped source: read whatever schedule the request
itself supplies, report the schedule as unchecked where it does not, and do not
reconstruct the contract here. The list below names the fields this procedure reads
and is not their definition. It decides
whether each result is ahead of, on, or behind its agreed date, and which later
result a slip actually blocks. It does not write a schedule, define a contract
field, keep a second copy of any date, or replace the criterion comparison it runs
beside. Read [Merge readiness](../../crw-run/references/merge-readiness.md) for the
CI and review evidence a delivery claim rests on; this procedure consumes that
evidence and does not restate it.

## Pin the schedule

Read, per result, the contract's stable ID, baseline target date, current target
date, baseline time, timezone, target kind, actual achievement evidence, change
source, and required predecessors. Add what this skill already pins: the delivery
level that result's own criteria require, the evidence at that level with both its
instant and the time it was observed, the observation instant, and the judgment
time. The observation instant is the query time, because the evidence is only as
current as that read. Report both times; when their calendar dates differ in the
schedule timezone, query again before ruling or state that the verdict stands as
of the query date.

A result is judged only when its kind carries a commitment date. A kind the
contract records as carrying none is kept as evidence, listed under unchecked
scope, and never judged or aggregated. The contract owns which kinds carry a
commitment; read that property rather than restating the vocabulary, and never
invent a target for a kind that has none.

Name the checked and the unchecked scope for the whole request, not only for the
results that happened to be decidable. A result left unchecked is a gap in
coverage and is reported as one.

## Fix the deadline

A date with no time of day has its deadline at the end of that calendar day in the
schedule timezone: it is past due exactly when the calendar date of the observation
in that timezone is later than the target date, so the target day itself is never
late. A date carrying a time of day is compared as an instant, and both boundaries
are inclusive — achievement exactly at the target instant is on time, and an
observation exactly at the deadline has not yet passed it. Normalize every instant
to the schedule timezone before comparing, including the achievement instant and
the observation. Without a timezone the deadline cannot be fixed: that axis is
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
merge-ready flag, and a green check establish neither achievement nor speed. The
achievement instant is the instant of the required-level evidence, never the time
the check read it.

## Order the verdict

Judge each result once per axis, first match wins.

| Row | Condition | Verdict |
|---|---|---|
| 1 | This axis has no date for this result, or no timezone, or the evidence needed to decide is stale | Undecidable, with the reason |
| 2 | Achieved earlier than the target: an earlier calendar day for a day target, an earlier instant for a timed one | Ahead |
| 3 | Achieved on the target day, or at or before the target instant | On plan |
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

For a day-granularity target the difference is whole calendar days in the schedule
timezone: an achieved result reports actual minus target, negative for early, zero
on the target day, positive for a late completion, and an unachieved past-due result
reports elapsed days only, the observation's calendar date minus the target date, at
least one. For a timed target the difference is a signed elapsed duration against
the target instant rather than a day count, because two instants on one calendar day
differ while their day count does not. Report that duration at the precision of the
source timestamps; a difference that is not zero is never rounded or truncated to
zero, and the sign is always kept.

Do not produce a projected completion date, a remaining-work estimate, or a velocity
or rate on any branch. An unachieved result reports how long it has been late and
nothing about when it will land.

## Name the risk

Before the deadline, only these observations make a result at risk, and each carries
one wait class. A required predecessor that is unachieved and is itself late or at
risk on the axis being judged, or that is unachieved and whose target date on that
axis is not earlier than this result's deadline so the ordering cannot hold:
internal coordination wait. An achieved predecessor is never a risk, whatever its
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
most severe in that set: late, then at risk, then on plan, then ahead. The severity
order decides alone, so the scope is ahead only when every member is ahead: one
early result beside an on-time one leaves the scope on plan, because the scope as a
whole was not delivered early.

Where the set is empty, decide in this order. Where no result in the requested scope
has a decidable verdict at all, the overall result is undecidable and the scope is
partially checked; it never defaults to on plan. Otherwise, where any undecidable
result is already due, the overall result is likewise undecidable and partially
checked, because a deadline that passed unchecked cannot be reported as nothing due
yet. Otherwise the scope is on plan, nothing due yet.

Undecidable never enters the severity order. Report it beside the overall verdict as
unchecked scope, naming how many results and why, so an undecidable result cannot
hide a late one and a late one cannot hide missing coverage.

## Judge both axes

Judge every result against the baseline target and against the current one, by the
same rules. A result belongs to an axis when the contract records that axis's date
for it. A result with no baseline date whose change source records that it entered
scope after the baseline time is labelled added after baseline: no baseline verdict,
excluded from the baseline aggregate, judged on the current axis only, and never
reported as undecidable, since it is a deliberate scope decision rather than a gap
in coverage. A result with no baseline date and no such record is undecidable on
the baseline axis, reason no baseline recorded, and the scope is partially checked.

Report both verdicts whenever they differ, with the change source and the instant
the current date took effect. Moving a target date never removes the baseline
verdict or its quantity, and results belonging to different scopes are never
compared at one rate. A pause is a change source and a cause, never a credit:
paused time is not subtracted from elapsed time.

## Trace the impact

Walk forward along required-predecessor edges only. For each unachieved result, name
the successors it reaches through at least one path made entirely of required edges,
and the nearest unachieved successor milestone or project. Another path that is not
required does not cancel that dependency, and a successor reachable only through a
path that is not required is not blocked. Where a successor has several required
predecessors, it is blocked by the ones actually unachieved and the others are named
as not blocking. A project or initiative is affected only where the walk reaches its
own completion condition; waiting on part of a result is never reported as blocking
the whole project.

Do not name the most recently touched or highest-numbered result unless it lies on a
walked path. Where a date, a relation kind, or a predecessor's own state cannot be
read, leave that edge's impact unverified and name the missing input.

## Record the judgment and reuse it

The judgment goes where this skill already records its result and nowhere else: the
linked Linear coordination document under an existing management assignment, the
summary a bounded helper returns to its coordinator, or the unsynced update when the
scope is read-only. A read-only or report-only scope returns findings and never
gains a write merely to leave a reusable record. Keep no second schedule store.

Record what identifies the judgment: the result IDs, the schedule fields as read,
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
dates and instants instead of repeating the audit. That is what lets a consumer take
the verdict without rerunning the criterion comparison. Whether a separate status
report reaches the same verdict is a property of that operation and is not
established here.

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
verdict. Dates are Asia/Seoul and kinds carry a commitment unless stated. Each case
lists every input that bears on its verdict, so an observation it does not list did
not hold, and where it names only a current target the baseline carries the same
date with no change source, leaving both axes in agreement and only the current one
worth discussing.

1. **Early completion.** M1, current target 2026-09-20, required level installed,
   installation receipt 2026-09-18T14:00, observed 2026-09-21T09:00. Ahead, actual
   minus target −2 days. Not on plan, which would ignore the earlier calendar day.

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
   Not S on plan because its own date is still ahead.

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
   erases the recorded delay.

10. **Scope addition.** The project's baseline was recorded 2026-09-01. I-6 has no
    baseline target date, its change source records that it entered scope on
    2026-09-10, its current target is 2026-10-05, and it is unachieved; observed
    2026-09-21. I-6 labelled added after baseline, no baseline verdict, excluded from
    the baseline aggregate, and on plan against its current date. Not a baseline miss,
    not undecidable, and not blended with the baseline scope into one rate. Where instead I-6 has no
    baseline target date and no change source recording when it entered scope, the
    baseline axis is undecidable with reason no baseline recorded and the scope is
    partially checked, while the current axis still decides on plan against
    2026-10-05. Not the whole result undecidable because one axis is, and not the
    missing baseline treated as an addition nobody recorded.

11. **Pause.** I-12, current target 2026-09-15, required level merged, paused
    2026-09-12, source recorded as the user's decision pending a product answer, no
    re-approved date, observed 2026-09-21. Late, elapsed 6 days, cause paused
    2026-09-12 by user decision, wait class user decision wait, the paused interval
    not subtracted. With the current target at 2026-09-30 instead, at risk with the
    same cause and class. Not paused days subtracted, and not a pause read as an
    extension.

12. **Timezone and granularity.** I-13, current target 2026-09-25, day granularity,
    timezone Asia/Seoul; the merge its criteria require landed 2026-09-25T23:30+09:00;
    the reader's own clock shows 2026-09-26T00:30Z. On plan, because the achievement
    falls on the target calendar day in the schedule timezone, not late read from the
    observer's date. With no timezone recorded, undecidable on that axis, reason no
    timezone, listed under unchecked scope, not the host timezone used silently. I-14,
    current target 2026-09-25T12:00, achieved exactly 2026-09-25T12:00: on plan, the
    boundary being inclusive, quantity a signed duration of zero rather than a day
    count. The same target achieved 2026-09-25T13:15: late, actual minus target
    +1h15m, reported as a completed result rather than as elapsed time, not on plan
    from the day-granularity rule applied to a timed target.

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
    assumed clear. Not an impact asserted over inputs nobody could read.

14. **A kind that carries no commitment.** The contract records I-18's kind as
    carrying no commitment date because it records when work actually began, and
    I-19's as carrying none because authority is still pending; I-19 is a required
    predecessor of S, whose current target is 2026-09-25, which is unachieved and
    whose criteria need that pending decision. Observed 2026-09-21. I-18 and I-19 both outside the judged set, kept as evidence and listed
    under unchecked scope; S at risk, cause an approval the criteria require and the
    user has not given, wait class user decision wait. Not a start record read as a
    deadline, and not a target invented for a kind that carries none.

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
