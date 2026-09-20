# Schedule progress

Every midpoint or status report answers one more question by default: is this on plan. It is part
of the report rather than something Jun has to ask for separately, and it is one short verdict
with its reason, not a projection.

## The shared baseline contract

The dates and their meaning come from the shared schedule baseline contract at
[Schedule baseline contract](../../crw-plan/references/integrations.md#schedule-baseline-contract),
which `crw-plan` owns and CRW-143 lands. It supplies the stable ID of each tracked target, its
original baseline date and its currently approved date, the baseline time and timezone, the kind
of target the date applies to, the evidence that counts as actually achieving it, the source of
any change, and the predecessors it requires.

**Pending CRW-143.** Until that section is on `dev`, this reference names it and stops there. Read
whatever dates the project and its milestones actually carry, report them as the unstructured
dates they are, and say that the shared contract is not yet available. Do not invent a substitute
contract, a local field set, or a second place where baselines live: two schedule contracts is the
problem the shared one exists to prevent. `crw-check` verifies the same contract under CRW-144,
so a private variant here would also break the agreement that check depends on.

## Read these, and not the issue count

The original baseline date, the currently approved date, the time and timezone of this check, the
milestone that was supposed to be reached by now, and the evidence that it actually was.

Not the number of issues, not the share marked Done, and not a rate derived from either. Issues
differ in size and a status field is set by hand, so a ratio measures neither progress nor speed.
The question is whether the results that were due by a date exist, which is answered by looking at
the results.

## Five verdicts

| Verdict | What it requires |
|---|---|
| 앞섬 | A target's result is confirmed achieved before its date, with the evidence that achieved it |
| 계획대로 | Everything due by now exists, and nothing due next is already known to be at risk |
| 지연 위험 | The date has not passed, and a concrete condition makes reaching it doubtful |
| 지연 | The date has passed and the result that was due by it does not exist |
| 판단 불가 | No date, no fresh evidence, or a reading that failed |

Mark 앞섬 only where early achievement is actually confirmed. Work that looks fast, a task that
finished its turn, and a burn-down that looks comfortable are not achievements. Risk before a date
and a date that has passed are different verdicts and are never merged into one hedge: 지연 위험
says there is still time, 지연 says there is not.

Where a check only reached part of the scope, judge that part and say so. A verdict covering
projects you did not read is a claim about work you did not do.

## What a verdict does not say

That the results due by a date exist is a statement about the past. It is not a guarantee about the
next date, and the two are reported as separate sentences. Do not report an estimated remaining
time, a predicted completion date, or a speed multiple: nothing in these records supports one, and
a number with no basis is read as a commitment.

A date applies to the result its own criterion names. Where the criterion is installation or
observed operation, a merged pull request is progress toward it and not achievement of it, and the
[evidence states](../SKILL.md#keep-the-evidence-states-apart) stay apart here exactly as they do in
the rest of the report.

## After a replan

A moved date does not erase what happened before it moved. Keep the original baseline, the current
approved date, the distance between them, and why it changed, so that a schedule rewritten twice
still shows both moves. A plan that reports 계획대로 because its dates were moved to wherever the
work had reached is a plan that cannot be late, and it tells Jun nothing.

Separate the causes as well. Scope added, scope removed, and a deliberate pause each move a date
for a different reason and call for different decisions, and folding them into one slip hides which
decision is actually needed.

## Report

Five short parts: the current verdict; the baseline target against where the work actually is; why
it is early or late; which next step is affected; and what needs Jun's decision. One project's
schedule line in a portfolio report is the verdict plus the one reason, with the rest available if
he asks.

## Agreement with check, and the route to it

`crw-check` owns verification of this same contract under CRW-144. The same inputs must produce
the same verdict from either side; that agreement is what makes a status verdict worth trusting
without re-auditing it. Status reads the results that are already trustworthy and only the sources
it still needs. Where a verdict disagrees with check's, or the evidence is too thin to decide, the
route is [crw-check](../../crw-check/SKILL.md) rather than a deeper investigation here.

Do not re-audit every CI run, every review, and every long work record on each call. A midpoint
check is asked for often, and a check that costs as much as an audit stops being asked for.

## Read-only here too

Reporting 지연 changes no date, wakes no parent, and starts no execution. A schedule change belongs
to [crw-plan](../../crw-plan/SKILL.md) and the work to [crw-run](../../crw-run/SKILL.md), and Jun
asking for one hands the scoped request to that owner. Do not set up recurring schedule alerts
without a request that asks for them.
