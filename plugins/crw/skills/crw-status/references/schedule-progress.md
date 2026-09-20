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

**Pending CRW-143.** Until that section is on `dev`, do not invent a substitute contract, a local
field set, or a second place where baselines live: two schedule contracts is the problem the shared
one exists to prevent, and `crw-check` verifies the same contract under CRW-144, so a private
variant here would break the agreement check depends on.

Without it, two of the five verdicts are unavailable and saying so is the verdict. 계획대로 and
앞섬 both assert that a date was met as originally agreed, and neither can be established while
nothing distinguishes an original baseline from a date that was moved to wherever the work had
reached. The other three survive, because none of them claims that: 지연 needs only a currently
visible approved date that has passed with the result it names visibly absent; 지연 위험 warns
about a date still ahead and promises nothing; and 판단 불가 covers everything else, reported as
판단 불가, 공유 일정 기준 계약 미도입 so the reason reads as the missing contract rather than
missing work. Whatever dates the project and its milestones carry are reported as the unstructured
dates they are, as context beside the verdict and never as the baseline that produced it.

Postponement history is one of the things the missing contract would have preserved, so it is not
available at this stage either. A date that was missed and then moved forward now reads as a date
still ahead, and 지연 cannot be carried on a date nobody can see: two projects with the same
visible record would otherwise get different verdicts depending on who remembered what. Use the
earlier date only where another trustworthy source explicitly holds it together with its target
and its achievement state, and name that source in the report. The replan rules below apply once
the contract supplies that history.

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

A project usually tracks several targets at once, and they can point different ways, so one
project's verdict is decided in a fixed order. Judge the obligations whose dates have already
arrived first: if any of them is short, the project is 지연, whatever else is running early. Only
when nothing already due is short does an early achievement elsewhere make it 앞섬, and only when
that achievement is confirmed. A future target's risk gives 지연 위험 when nothing due is short.
Report the other targets on their own lines rather than averaging them into the verdict.

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

Five short parts, and only the ones that apply: the current verdict; the baseline target against
where the work actually is; why it is early or late; which next step is affected; and what needs
Jun's decision. A 판단 불가 has no early or late to explain, so it carries what was missing and
what would make a verdict possible instead, rather than empty headings or a guess filling them.
One project's schedule line in a portfolio report is the verdict plus the one reason, with the rest
available if he asks.

## Agreement with check, and the route to it

`crw-check` owns verification of this same contract under CRW-144. The same inputs must produce
the same verdict from either side; that agreement is what makes a status verdict worth trusting
without re-auditing it. Status reads the results that are already trustworthy and only the sources
it still needs. Where a verdict disagrees with check's, or the evidence is too thin to decide, the
route is [crw-check](../../crw-check/SKILL.md) rather than a deeper investigation here.

Do not re-audit every CI run, every review, and every long work record on each call. A midpoint
check is asked for often, and a check that costs as much as an audit stops being asked for.

## A verdict is not a schedule change

Reporting 지연 changes no date and starts no execution, on either branch of
[the classification](../SKILL.md#classify-the-request-before-deciding-what-this-call-may-do). A
schedule change belongs to [crw-plan](../../crw-plan/SKILL.md) and new work to
[crw-run](../../crw-run/SKILL.md), and Jun asking for one hands the scoped request to that owner.
On the reading branch nothing is contacted at all. On the checkpoint branch, moving the work an
approval already covers is allowed and moving a date is not, and a late verdict is never the reason
to widen what was approved.

Status sets up no recurring or background schedule alert on either branch, and a request for one is
handed to that owner rather than built here: a check that keeps running on its own is no longer a
check somebody asked for.
