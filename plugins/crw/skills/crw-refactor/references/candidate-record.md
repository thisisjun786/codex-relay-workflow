# Refactoring candidate record

Use these fields to make a candidate reviewable. Keep evidence proportional to the decision; do not create an empty project or elaborate phase plan just to hold this record. Private transcripts, logs and runtime data stay outside the repository.

## Cycle context

- Target cycle, actual start/end dates (or open end), original design and agreed goal; the verified subset and any unread or unfinished scope.
- Per repository: baseline, delivered revision, relevant PRs, current dirty/active paths and evidence date.
- Preserved features and external contracts; next agreed change or replacement boundary.
- Delivery evidence reused, its applicability and missing installation/runtime proof where relevant.

## Candidate 1–3

| Field | Required answer |
|---|---|
| Problem and origin | What became harder to change? Introduced in this cycle, or older debt exposed by it? |
| Code evidence | Actual path and line at a named revision, callers/tests and relevant change history; distinguish observation from inference |
| Cost of leaving it | A concrete next edit, repeated synchronized change, diagnosis obstacle or supported consumer affected; no invented time savings |
| Smallest repair | Which responsibility/rule moves or disappears, which consumers change, and why less change would not solve the problem |
| Preservation and exclusions | Behavior, external interfaces, compatibility still required, active work and unrelated improvements left alone |
| Same-cause reach | How sibling sites were sought, what was found and what remains unexamined |
| Verification | Baseline reproduction/checks, known failures, required negative controls, preserved behavior and observable maintenance benefit |
| Independence and reversal | Separate PR boundary, dependencies, how the repair can be reverted, and any coupling preventing independent reversal |
| Execution owner | Project parent and candidate-owning undelivered issue/task, a proposed new issue after merged delivery, or the direct implementation owner explicitly chosen by the user; a proposed owner is not a dispatch |
| Recommendation | Do now, defer to a named trigger, or measure an uncertainty first; cite an existing issue when one already owns it |

## Baseline versus result

For each meaningful check record the revision, environment, command, expected observation and observed result. A pre-existing failure needs baseline evidence; an unrun check stays unverified. After a repair, a new failure is investigated against that baseline rather than waived because some other baseline check already failed. Reuse unchanged evidence where applicable and run required repository checks.

Judge benefit separately from green tests. For example, a rule formerly edited in three consumers may now have one owner with those consumers exercising the same behavior; removing compatibility needs evidence that the supported consumers no longer require it. Line count alone establishes neither.
