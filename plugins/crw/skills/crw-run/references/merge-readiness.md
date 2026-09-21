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
and independent local review artifacts. For each applicable source, establish who
reviewed what base/head or diff, whether the run completed, and which findings
remain. A successful review job can still contain defects; a completed COMMENTED
review can be useful evidence without being a formal approval. Thread resolution
alone does not prove a fix.

Where a count is the gate, read it to the end and prove that you did. An unresolved
count of zero is the value that OPENS the merge gate, so a truncated page reads as
a pass: sixty of sixty-three threads with nothing unresolved among the sixty once
reported "0 unresolved" about a candidate with three open threads and a P1 among
them. Every connection the verdict rests on - threads, reviews, comments, check
runs, workflow runs, jobs, statuses and the branch's own effective rules - carries
the same obligation, because a second required check on page two is exactly as
invisible as the sixty-first thread was, and a required gate declared by a ruleset
on a later page is missing from the very set that decides what "required" means.

`codex-session-relay merge-evidence --repository owner/name --pull-request N`
performs that reading and returns the record: it enumerates each connection until
its pagination is exhausted, counts unique identifiers against the reported total,
pins the head and base before collecting and re-reads them with the effective rules
afterwards, and reads a zero twice before reporting it. Exit 0 is the ready verdict
alone; a stale, unknown or not-ready answer exits 2 with the whole payload. A
truncated page, a permission error or a head that moved is reported as stale or
unknown and never as green or zero. Its `handoff` is the record a completion report
carries; the dispositions and the criterion evidence stay yours to judge, and it
never resolves a thread.

Three distinctions it makes that a reading by hand usually does not. A check is
identified by its workflow run and job name rather than by its name, so two runs
publishing one gate on a head are both binding and a run that REPLACED another -
a re-run, or a cancellation when new CI starts - does not block the run that
replaced it. A required context bound to an integration is answered only by that
integration, so a namesake from another app neither satisfies the gate nor blocks
it. And a submitted review whose state is CHANGES_REQUESTED refuses on its own,
while a summary comment is collected and never graded, because whether prose
describes a defect is a triage judgement and not an observation.

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
make the badge green. Which findings block merge is decided by
[impact](#judge-a-finding-by-its-impact); an unmet required review gate blocks
whatever the findings say.

For optional reviewers, observe a running review with bounded waits appropriate
to its expected runtime. If unavailable or stalled, use sufficient independent
review already available or obtain a permitted local review. Record the gap and
continue when the repository's requirements and relevant review coverage are met.
Do not wait for every historical or possible future reviewer. Fallback review
cannot replace required CI, a required review source, or mandatory formal approval.
Inspection does not authorize enabling integrations, new paid usage, or sending
private source to another service.

## Judge a finding by its impact

Whether a finding blocks is decided by what it does to this change at its current
head. The parent that owns the criteria makes that classification against the list
below. The child triages, fixes what blocks, prepares the evidence and the
current-head readiness, and proposes the rest.

A finding BLOCKS, and is never conditionally accepted, when it is any of these:

- a severe effect on what this change is for, or the failure of a core function it
  delivers;
- a safety effect, or data loss or corruption;
- an authorization, permission, credential, or protection boundary that the change
  weakens or bypasses;
- a required issue or acceptance criterion left unmet. A user's concession moves
  that criterion rather than excusing it, and it counts only once the registered
  criterion carries the conceded wording, under the per-criterion dispositions
  [crw-check](../../crw-check/SKILL.md) owns;
- a material regression this change introduced;
- an unmet required review gate or required check.

A blocking finding is fixed on this pull request, or the candidate is reported
blocked. One that arrives after the child has finished its rounds is raised to the
parent rather than absorbed silently, and the parent decides whether it belongs to
this issue or to a successor.

A regression this change introduced is in scope whatever its size, because this
change caused it, so it is never dispositioned as pre-existing or out of scope. A
material one blocks. A trivial one left by the round's own fix is finished rather
than accepted, because fixing what you just broke costs less than recording why
you left it.

A finding is MINOR AND SEPARABLE when both of these hold: it reaches none of the
classes above, and the residue it leaves is detachable from what this issue
delivers. Separability is established rather than assumed. Wording, formatting, a
clearer phrasing, a naming preference, a hardening suggestion beyond this issue's
scope, and an independent defect that predates this change are the usual shapes.
Being real is what earns it a disposition; being separable is what makes it
acceptable.

A finding whose class is genuinely unclear is raised to the parent as unclassified
rather than settled into this one. The list above is finite and the ways to break
something are not, so "it is not on the list" is not a classification, and neither
is a severity a reviewer happened to print.

A level above the owning parent may raise a finding, and may decide merge order. It
does not reclassify a minor separable residue as blocking by asserting that it is
serious, and reopening a settled delivery takes a finding that actually falls in one
of the classes above.

### Conditional acceptance, and what recording one costs

The parent that owns the criteria owns this judgment, and a child does not grant its
own. A parent may instead grant it in the assignment as a bounded standing decision,
stating the bound it covers. Either way, each acceptance records five things or is
not recorded:

The exchange that produces one is small, and it happens before the handoff rather
than as another review round. The child sends the finding, its own reading of the
effect and the separability, and what it proposes; the parent answers accept or fix.
What the parent decides is the criteria-and-purpose question only, which is whether
this residue matters for what the issue is for. It does not re-read the diff,
re-triage the thread, or re-derive the finding: those stay the child's under
[OPS-9.3](operations.md#ops-93-the-parent-merges-and-does-not-release), and a parent
that finds itself reconstructing the finding has crossed into the child's round and
returns it instead.

1. the exact finding, quoted or linked to its thread;
2. its current effect, stated concretely rather than as "minor";
3. why accepting it now serves this issue's purpose;
4. the follow-up owner and the trigger that reopens it, as a named successor issue
   or an explicitly accepted owner, never as unassigned;
5. the boundary this leaves known-imperfect, so the next reader does not rediscover
   it as new.

A conditional acceptance is NOT a fix, and it never becomes one: not by the thread
being resolved, not by a later round going quiet about it, and not by the issue
closing. Nor is it available for a finding nobody weighed. A disposition written to
clear a gate without assessing what the finding actually does is the thing this
section exists to forbid, and it is worse than another round, because the round
costs time while the false record costs the reader their trust in every other one.

An acceptance recorded by a child is that child ASSERTING a decision the parent
made, and no transport here authenticates the claim. So the parent's pre-merge
restatement confirms each accepted disposition against a decision it actually made,
and an acceptance it does not recognise returns the candidate to that child
fail-closed, exactly as any other disagreement between the record and the re-read
does under [OPS-9.4](operations.md#ops-94-a-new-head-invalidates-the-review-it-outran).
This is the one disposition that legitimises a defect the candidate still carries,
which is why it is confirmed rather than counted.

A bounded follow-up never waives a required criterion. Where accepting a finding
would leave an issue criterion unmet, the criterion governs and the finding blocks.
An obligation deliberately deferred is recorded as an unverified criterion carrying
its deferral, under the per-criterion dispositions
[crw-check](../../crw-check/SKILL.md) owns, and never as a satisfied one.

### Resolve a thread on the judgment actually reached

Resolving a conversation is a button, and a forge gate that counts unresolved
threads measures the button rather than the work. Satisfying that gate is never a
reason to assert a fix that did not happen. Record the judgment that is true and
resolve on that record: `fixed` naming the commit, `accepted` naming the parent's
decision and its follow-up, `not_applicable`, `duplicate` or `already_resolved`
carrying the evidence that the defect itself is gone, or `disputed` carrying the
reason.

`duplicate` and `already_resolved` are claims about the defect rather than about the
thread. A repeated or outdated thread whose defect survives is judged on the defect:
fixed once it is fixed, accepted once a parent has accepted it. An out-of-scope or
pre-existing finding carries a named owner and a follow-up and is not closed
unverified. No rule makes every comment produce a code change, and none makes a
comment that arrives late produce nothing.

### Where the rounds end

Reviewers that re-read a whole diff on every push make "nothing unresolved and no
new round" unreachable: a fix needs a push, a push invites a round, and some rounds
arrive with no push behind them at all. So the condition is about impact rather than
arithmetic. The rounds end when every thread seen on the current head carries a
truthful judged disposition and no blocking finding is outstanding. A further round
that produces only minor separable findings adds dispositions and follow-ups; it
does not reopen a change that has met that condition.

None of this turns a real defect into a non-defect. A reviewer's P1/P2 badge, the
number of rounds, the time spent and the cost of another round are not severity:
they neither exempt a blocking finding nor create one, and a finding on round nine
is judged exactly as one on round one. Repeated rounds over a large diff are a
reason to split the next change rather than a reason to stop reading this one.

Which threads exist, and whether the evidence is still current, are different
questions from impact and keep their own owners: the review-coverage rules in this
procedure, and
[OPS-9.4](operations.md#ops-94-a-new-head-invalidates-the-review-it-outran) for a
new head invalidating the review it outran.

## Recheck, integrate, and record

Immediately before merging, reread the PR's base/head, relevant CI/review state,
and newly arrived findings. Reconcile changes since the review; refresh only the
proof they invalidate. A relevant unresolved finding still matters even if its
author is an optional reviewer. Use the repository's merge method and the host's
expected-head guard; preserve required base-update or merge-queue behavior.

`merge-evidence --restate <record>` is that re-read: it takes a fresh reading of its
own and grades the child's record against it, rather than reading the child's own
numbers back. A thread that arrived on the same head and is not in the record's
`threadsSeen` invalidates the record, which returns to the child that produced it.
The reading and the merge are not one act, and the command does not pretend they
are: the expected-head guard is what closes the gap at the moment of merging, and a
finding that lands after it is a late finding for the original issue's correction
path.

Serialize integrations sharing a target. Verify the actual landing and resulting
destination revision; an accepted or queued merge request is not a completed merge.
In the existing coordination record, retain the candidate and landed revisions,
CI attempt links/results, review sources and coverage, finding dispositions, and
any permitted fallback or remaining limitation. Keep implementation verification,
integration, and deployment as separate claims.
