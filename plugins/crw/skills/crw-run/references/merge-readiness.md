# Merge readiness

Use before integration, together with the target repository's policy and the
[shared authorization and review rules](../../crw-plan/references/integrations.md#default-dev-integration).
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
[Publish for review when the work is reviewable](../../crw-plan/references/integrations.md#publish-for-review-when-the-work-is-reviewable)
describes, not to merge around the gap. Findings or pending CI on an open pull request
never send it back to draft.

## Disabled reviewer policy

GitHub Codex automatic code review is disabled and is not a CRW completion or
merge gate. Do not trigger it, re-request it after a push, or wait for a fresh-head
Codex review or a settling period. Re-enabling it requires a new explicit user
decision; an old assignment, review comment, or pending check does not supply one.
Record it as disabled/not required, never as a successful review.

This applies to the external GitHub reviewer, not Codex execution tasks, their
model settings, CXC's workflow or independent review. Keep the target repository's
policy for other reviewers, required CI, parent verification, and sufficient
independent evidence. If an enforced GitHub rule still requires the disabled
reviewer, report the specific configuration conflict for authorized correction;
do not bypass the rule or silently change repository settings.

Existing findings remain subject to the triage below, regardless of their author.
A disabled reviewer does not excuse a confirmed defect or justify resolving its
threads without evidence. Refresh stale launch and recovery instructions on the
same parent and child tasks; preserve their work and dependency gates.

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

## Hold the turn only while you can use it

Parents under one supervision land on the same base ref, so the turn on that target
is serialized in the coordination record and claimed before integrating, not after
deciding to merge. Claim with the exact candidate head; a claim with no head cannot
be checked against one later.

Waiting for your own CI is not the same as merging. While required CI, review, or a
base update is still outstanding and the merge has not started, a ready peer candidate
may proceed instead, so return the turn explicitly rather than holding it while you
re-poll. Record what invalidated the readiness — a new head, a base that moved, a
finding that arrived — because only the head is visible from the record itself, and a
peer reading it otherwise cannot tell a candidate that was never ready from one that
stopped being ready.

Asking for the turn, a transport accepting that request, and the holder actually
returning it are three separate facts, and only the third moves the turn. A peer's
report that it handed you the window is that peer's account, not the record; read the
record. Once a merge is in flight or its outcome cannot be established, nothing
releases the target except an observation of the pull request — elapsed time never
becomes one, and cancelling is refused in that state on purpose.

On entry — woken, resumed after a compaction, or restarted — read your own outstanding
claims before acting on whatever prompted you, and re-check current ownership, head and
base before acting on a grant you find. A grant read earlier can have been returned
while you were away; acknowledge it against the record rather than against what you
remember. A parent whose binding is paused keeps its claim and its place in the queue
and cannot acquire or merge under it, so resume the binding, then declare readiness
again to take a target that is free. Nothing hands the turn over on your behalf, and
neither the supervisor nor Jun reassigns the window as a matter of course.

When a target is not moving, report the actual candidate, its revision and the waiting
cause from the record, together with the ready peers behind it. Do not resolve this with
a fixed sleep, a retry cap, or an assumption about how long a particular CI takes.
