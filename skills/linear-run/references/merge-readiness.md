# Merge readiness

Use before integration, together with the target repository's policy and the
[shared authorization and review rules](../../linear-plan/references/integrations.md#default-dev-integration).
This procedure consumes existing CI and review evidence; it does not install a
reviewer, enable an unfinished app, or change branch protections.

## Identify the candidate and gates

Pin the repository, PR, destination branch, current base/head SHAs, and dependencies.
Read applicable repository instructions, effective branch rules/protections, root
CI workflows, and current review configuration. Separate required gates from
optional integrations. Use actual configuration and recent execution evidence;
old bot comments, a nested workflow file, or repository visibility alone do not
establish the active review setup. Lack of access leaves configuration unknown.

Record `isDraft` with the base and head. A pull request that is open for review has
entered review, which is not the same as being ready to merge: the gates below decide
that. A draft candidate is not merge-ready, and the fix is to publish it for review as
[Publish for review when the work is reviewable](../../linear-plan/references/integrations.md#publish-for-review-when-the-work-is-reviewable)
describes, not to merge around the gap. Findings or pending CI on an open pull request
never send it back to draft.

## Check CI for this candidate

Inspect both check runs and commit statuses, including their source identities,
revision coverage, and run/attempt URLs. Select the applicable latest attempt for
each gate; a later valid rerun may supersede an earlier cancelled or failed attempt.
Do not collapse distinct jobs or sources merely because their names match.

Required CI must satisfy the repository's gates for the candidate, including any
required merge-result/base compatibility checks. Pending, missing, failed, cancelled,
or inaccessible required evidence is unresolved. Accept skipped/neutral results
only when the repository's gate semantics make them legitimate for this change;
a green PR summary alone is insufficient. Resolve routine failures and recheck.

No configured CI is not a CI pass. Use the repository's permitted local validation
route if one exists and report that distinction. Do not invent a new hosted CI
requirement, waive an existing one, or claim readiness when necessary validation
is still unknown.

## Inspect review content and coverage

Collect relevant submitted reviews, inline threads, summary comments, statuses,
and independent local review artifacts. Follow pagination where necessary. For
each applicable source, establish who reviewed what base/head or diff, whether
the run completed, and which findings remain. A successful review job can still
contain defects; a completed COMMENTED review can be useful evidence without
being a formal approval. Thread resolution alone does not prove a fix.

Keep completed, running, not run/disabled, unavailable/error, and stale evidence
distinct. A completion label without identifiable revision/scope does not prove
coverage. Reuse earlier review plus verified incremental review where it covers
the current change; refresh evidence invalidated by a new head, changed base, or
dependency. The implementation worker's own completion report is not independent
review. Obtain independent review appropriate to the change and repository policy.

Triage findings against code and acceptance criteria. Send confirmed in-scope
defects to the existing responsible task, verify corrections with focused checks,
and reconcile related threads within authorization. Record an evidence-backed
reason for duplicate, inapplicable, or disputed findings; do not dismiss them to
make the badge green. Material defects and unmet required review gates block merge.

For optional reviewers, observe a running review with bounded waits appropriate
to its expected runtime. If unavailable or stalled, use sufficient independent
review already available or obtain a permitted local review. Record the gap and
continue when the repository's requirements and relevant review coverage are met.
Do not wait for every historical or possible future reviewer. Fallback review
cannot replace required CI, a required review source, or mandatory formal approval.
Inspection does not authorize enabling integrations, new paid usage, or sending
private source to another service.

## Recheck, integrate, and record

Immediately before merging, reread the PR's base/head, relevant CI/review state,
and newly arrived findings. Reconcile changes since the review; refresh only the
proof they invalidate. A relevant unresolved finding still matters even if its
author is an optional reviewer. Use the repository's merge method and the host's
expected-head guard; preserve required base-update or merge-queue behavior.

Serialize integrations sharing a target. Verify the actual landing and resulting
destination revision; an accepted or queued merge request is not a completed merge.
In the existing coordination record, retain the candidate and landed revisions,
CI attempt links/results, review sources and coverage, finding dispositions, and
any permitted fallback or remaining limitation. Keep implementation verification,
integration, and deployment as separate claims.
