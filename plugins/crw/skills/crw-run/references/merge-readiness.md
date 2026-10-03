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

A local validation run the parent starts for that route follows the `Processes you start:` line of
the [Launch packet](task-packet.md#launch-packet): it records its pid when it starts or runs under
`timeout`, and is stopped only by that pid, its own process group or the handle the execution tool
returned for it, never by pattern or name, because children run their own tests on the same host at
the same time.

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
A base that only moved is the one disagreement the parent removes itself, under
[Refresh the base yourself when only the base moved](#refresh-the-base-yourself-when-only-the-base-moved).

Serialize integrations sharing a target. Verify the actual landing and resulting
destination revision; an accepted or queued merge request is not a completed merge.
In the existing coordination record, retain the candidate and landed revisions,
CI attempt links/results, review sources and coverage, finding dispositions, and
any permitted fallback or remaining limitation. A base refresh adds the head it started
from, the head it produced and the check between them. Keep implementation verification,
integration, and deployment as separate claims.

### What the handoff discloses, checked at the verdict

The handoff's `disclosures` member ([what a handoff
discloses](task-packet.md#what-a-handoff-discloses)) is read with the rest of the record and
restated like it, never rebuilt. Nothing in the relay grades it, so the comparisons below are what
stands between a rejected High and a merge. They sit after the claim and the acknowledgement and
before the verdict is `verified`, and each is a comparison with a record the parent can read:
mechanical validity, not a second review round
([OPS-9.3](operations.md#ops-93-the-parent-merges-and-does-not-release)). A comparison that fails
returns the candidate through the needs-changes verdict, with the restoration block in the first
finding ([Return corrections to the existing
task](../SKILL.md#return-corrections-to-the-existing-task)).

1. **Every item is there.** A handoff for a pull request with no `disclosures` member, or with an
   item left out, does not describe the candidate and returns to the same child like any other
   disagreement between the record and the re-read
   ([OPS-9.4](operations.md#ops-94-a-new-head-invalidates-the-review-it-outran)). `none` and
   `ran: false` are answers; silence is not.

2. **Every rejected High is answered before `verified`.** Each `internalReview` finding disposed of
   as `rebutted` or `out_of_scope` has a `decisionRequests` entry or names the parent ruling that
   already settled it; one with neither is a hole in the record and the handoff returns. Each
   request is then answered in one of three ways. The rebuttal is confirmed on its evidence: the
   finding is not a defect, nothing is accepted, and the five records of a conditional acceptance do
   not apply. A real residue that is minor and separable, the usual shape of `out_of_scope`, is
   accepted under [conditional acceptance](#conditional-acceptance-and-what-recording-one-costs)
   with its five records. Or a fix is required. A finding in a blocking class under
   [impact](#judge-a-finding-by-its-impact) always gets this answer, whatever label the review gave
   it and whatever the child concluded: it is fixed, or the candidate is reported blocked. A verdict
   that does not name a request is incomplete. Where the child asked first and ended its turn
   `blocked_needs_input`, the answer goes back on that route and the request cites it.

3. **Every commit after the review is accounted for.** With a `reviewedHead`,
   `git rev-list --first-parent <reviewedHead>..<head>` names exactly the merge commits of the
   `baseRefresh` entries and the commits of `commitsAfter` that lie after it; a commit in neither,
   or a listed one after `reviewedHead` that is not there, means the record does not describe the
   candidate. An entry or a commit that lies before `reviewedHead` is history the review covered and
   is not compared, so a later generation's handoff may keep earlier entries or drop them. The
   first-parent line is the window on purpose: a merge of the base made before the review is history
   the review covered, and an unrestricted range would also list the base's own commits. The window
   runs from the head the latest independent review covered through every generation, whoever made
   each merge. The listed commits are changes the review did not see. Hosted-review fixes, base
   merges and digest re-records are expected; the parent asks for an independent check of a commit,
   and of that commit only, where it changes behavior and nothing else covered it. Where there is no
   `reviewedHead` (`ran` is `false`, or only checks limited to some hunks ran) the window starts at
   the assignment's baseline commit and only merges are compared:
   `git rev-list --first-parent --merges <baseline>..<head>` names exactly the merge commits of the
   entries plus any merge listed in `commitsAfter` as not a merge of the base, so a base merge
   nobody listed does not escape the refresh checks, while ordinary commits are the child's own work
   and are not listed one by one. The handoff's independence then rests on the hosted review and the
   parent's own reading, and the verdict says so.

4. **Each refresh is the kind it says.** A refresh the child made is proved by the same check as one
   the parent made ([Refresh the base yourself when only the base
   moved](#refresh-the-base-yourself-when-only-the-base-moved)). For every entry, of any kind, first
   read the merge's parents with `git rev-list --parents -n 1 <head>`: there are exactly two, the
   first is the entry's `previous` and the second is its `merged`, and that second parent is an
   ancestor of the base tip the parent observed itself
   (`git merge-base --is-ancestor <merged> origin/dev`, or the tip seen before the landing). The
   helper's own test that the second parent is on the base is empty when the caller names that
   parent as the base, and it only runs for a `clean` entry. A field that disagrees with the actual
   parents is a wrong receipt: the entry returns to be corrected and the checks run again. Only when
   the actual parents show that the commit is not a merge of the base, because it has not exactly
   two parents or its second parent is not on the observed base, is what it brings in the child's
   own change; it then belongs in `commitsAfter` and is judged by impact. Then, for an entry named
   `clean`, run
   `crw skill base-refresh check --repo <a checkout that has fetched both heads and the base> --previous <previous> --head <head> --base origin/dev`
   and read the answer:

   - Exit 0 confirms the entry, and the refresh reruns the gates only.
   - Exit 1 is read by its reason. `no_update`: the entry names a refresh that did not happen, so
     it is dropped. `not_built_on_previous`: a wrong anchor, more than a hundred merges or a walk
     that reached the base, so the anchor is corrected. `not_a_merge` and `not_from_base`: a commit
     that is not a merge of the base lies in the range. It is the child's own change, belongs in
     `commitsAfter` and gets an independent check of that delta by impact, not of the whole diff.
     `merge_conflicts` and `tree_differs`: the merge carries resolutions or edits, so it is not
     `clean`, and it stays a `baseRefresh` entry. It is reclassified `mechanical` or `manual` by the
     hunks `git show --remerge-diff <head>` prints for it, an added file included, and checked as
     that kind. The check stops at the first refusal it meets from the head and says nothing about
     the commits before it, which is what the accounting in the previous item covers.
   - Exit 2 is no answer. The cause it names is addressed (fetch the commits, use a full checkout,
     git 2.41 or newer) and the entry is unverified until the check has run.

   For a `mechanical` or `manual` entry, `git show --remerge-diff <head>` prints the merge's
   resolutions and each one must be a listed hunk: an unlisted resolution, or an edit riding in the
   merge, returns the entry. The parent reproduces a mechanical hunk's rule and compares; for a
   manual entry it compares the scope and result of the child's independent check with the hunks,
   because that check is the child's and the parent does not repeat it.

5. **The paths are the ones changed.** `git diff --name-only origin/dev...<head>` against
   `changedPaths`: a path left out, or placed in the wrong region, means the record does not
   describe the candidate. Where a region covers part of a file the entry's hunk ranges are compared
   with the declared section the same way (`git diff -U0 origin/dev...<head> -- <path>`); a verdict
   that compared paths only records those regions as unverified, and the count of paths outside
   every region is kept apart from the count of hunks outside a section. Both counts go in the
   coordination record beside the merge ([Recheck, integrate, and
   record](#recheck-integrate-and-record)). Nothing else consumes them yet, and that record is where
   a later calibration of the declarations can read them.

6. **Sibling impact is read before the merge order is chosen.** The registry entries the child added
   are what a union check keeps when the next sibling's branch is refreshed onto this one, and a
   shared interface it changed is what that sibling must read first. A finding that needs a
   sibling's work to change is the parent's to route, through the relay or the coordination record;
   the child does not settle it and does not message the sibling. That sibling's next correction
   carries the landing and the expected conflicts in its [restoration
   block](task-packet.md#restoration-block).

### Refresh the base yourself when only the base moved

The dev ruleset is strict: a pull request has to contain the tip of its base, so each landing leaves
the other candidates behind. Returning a candidate to its child for that alone costs a whole
generation (merge the base, wait for every job and the review, report again) and changes nothing the
parent had verified. When nothing else is wrong the parent updates the branch itself, once, and
proves what it produced. The merge gate does not change: after the update every condition of it has
to hold again on the new head.

**Only the candidate about to merge.** Refresh the candidate that is about to merge, when its turn
comes: the one that holds the merge turn or is next in line for it (first in, first out until
measured, D-16). A landing leaves every other open pull request behind, and that is no reason to
touch them: a refresh restarts every required job, and a head refreshed early is behind again after
the next landing. One merge never starts a refresh of the remaining pull requests, and a parent does
not refresh a queue ahead of its turn to save time later. A candidate that is not next waits, with
the head its child reported.

Do it only when all three hold at the head P that the child reported and the parent verified (a
later refresh of the same candidate is a further step of its own, below):

1. The base is the only thing wrong. `merge-evidence --restate <the child's record>` reports exactly
   two problems, `candidate_behind` and `candidate_moved`, and `pinned.headSha` is the record's
   head. The reading raises `candidate_moved` for a moved head and for a moved base alike, so the
   head comparison is what attributes it to the base. Any other problem (a `late_finding`, a job that
   is not a success, a changed gate, a draft) is not a currency problem and is handled as it always was.
   A merge state of `UNKNOWN`, which the forge reports briefly after a landing, is a reading to repeat.
2. The forge reports no conflict: `pinned.mergeable` is true and the merge state is not `DIRTY`.
3. Every criterion was ruled verified at P: the relay verdict on the report that names P is
   `verified` ([assignment-show](relay.md#verify-the-current-revision)), or, with no relay, the
   parent's own check against the Linear criteria is recorded for P. That ruling includes the
   [disclosure checks](#what-the-handoff-discloses-checked-at-the-verdict) at P.

Read the tip of the base from the forge immediately before the update and keep it: it is D, the dev
tip, and every proof below names it (`gh api repos/OWNER/REPO/branches/dev --jq .commit.sha`; a
pull request's `base.sha` is a snapshot, not the tip). Then update with the forge's call, guarded by
the head you verified, and never by rebase, reset or force-push:

    gh api -X PUT repos/OWNER/REPO/pulls/N/update-branch -f expected_head_sha=<the head you verified>

A 202 says the forge accepted the request, not that the branch moved: read the pull request until
its head is no longer the one you verified. A 422 whose message names a merge conflict is a conflict
(below). Any other 422, such as a head that moved because the child pushed, leaves the branch
untouched and means the verification described a head that is gone, so start again from the top.

On the new head N nothing about P carries over. Before the merge:

- **What N is.** `crw skill base-refresh check --repo <a checkout that has fetched N and D>
  --previous P --head N --base D` answers from git alone, as an identity. It passes (exit 0) only
  when all three hold: N has exactly two parents, P first and D second, in that order;
  `git merge-tree --write-tree P D` exits 0; and the tree it writes is N's own tree. So no edit, hand
  resolution, file of its own, second update or reversed merge rides along. A pass prints an
  `evidence:` line with five fields (`previous`, `dev_tip`, `head`, `tree`, `rule=tree_identity`),
  which the parent copies into the merge record and the merged mark instead of retyping them
  (Record the refresh, below). Exit 1 names what N is instead and quotes what was read on a
  `facts:` line: `no_update` (N is P); `not_built_on_previous` (N is already on the base, or its
  first parent is not P); `not_a_merge` (N does not have two parents); `parents_swapped` (P is the
  second parent: the reverse merge, which merge-ort gives the same tree, so only the parent order
  tells it apart); `not_from_base` (the second parent is not an ancestor of D);
  `not_the_dev_tip` (it is an ancestor of D but not D itself);
  `merge_conflicts` (git cannot merge P and D without a resolution, so N carries one);
  `tree_differs` (N's tree is not what git merges; the paths that differ are named). Exit 2 means
  git could not answer (a missing commit, a shallow checkout, git older than 2.41, a `merge-tree`
  that fails or writes no tree), which is not a pass: fetch and ask again. Run it before the merge,
  because a head that has landed is an ancestor of its base; a replay afterwards names the
  `dev_tip` of the evidence line in `--base`. There is no hand-run substitute: plain git commands
  in a checkout follow its replace refs, merge drivers and attributes, and what they print is not
  this answer. Where the installed `crw` does not list `base-refresh` among the `crw skill`
  families, the parent does not refresh, and the candidate goes back to its child as it did before
  this rule.
- **The safe side.** A refusal, and an exit 2 that a second ask does not clear, leaves N
  unaccepted. The candidate goes back to its child under the needs-changes route below, naming N
  and quoting the first line and the `facts:` line, except where the refusal itself names a step
  the parent takes first: the stepwise proof of a chain and the re-pin after a moved base, both
  below. `git range-diff` of the child's commits as seen from P and from N, and the paths a
  `tree_differs` names, decide what the correction says; neither turns a refusal into a pass, and
  nothing else in this procedure accepts a head the check refused. One possible cause of a
  `tree_differs` on an update the forge made without reporting a conflict is rename handling: the
  check merges with git's defaults and no configuration (merge-ort, rename detection on), the forge
  merges with its own settings and limits, and on a repository that renames files the two can
  differ. A difference can only make the trees differ, so it ends in a refusal and never in a pass.
- **Every job and the review, on N.** `merge-evidence` on N, without `--restate` because the
  child's record names P, must exit 0: each required job a success at its newest attempt, reviews and
  threads read to the end, and the candidate no longer behind. Jobs still pending, and a reading that
  is unknown or stale, are waiting: read again. Devin reports a `Devin Review` commit status on a
  head it has analysed; read once on a merge-only head, it completed within seconds and left no
  review object, and what posts it is the repository's Devin setup, which is not assumed for every
  head: `gh api repos/OWNER/REPO/commits/N/status`. Pending is waiting, `success` means it ran,
  and its review and threads on N are then read like any other; no status at all is recorded as an
  unavailable reviewer, never as a pass. Every thread on N has to be in the record's `threadsSeen`.
- **A required job that failed on N is rerun once, on N.** On a head the parent made, a failed
  required job is a flake or a real failure, and D-12 settles it with one rerun on the same SHA: a job that passes is a flake, one that fails again is a failure.
  In a DAG-managed project that rerun is the scheduler's own `retry_same_sha`: `dag-merge-judge`
  (after `dag-accept`, below) answers `checks_pending` while N's jobs run, `retry_same_sha` on the
  first failure of N once every required check has finished, and `evicted` on a second one. Rerun
  only after the judge has answered `retry_same_sha`, never on seeing red: the store counts only
  failures a judgement recorded, so an earlier rerun would be invisible to it and the second
  failure would earn a second retry. Run the failed jobs again on the same head (`gh run rerun <run
  id> --failed`, which asks for the rerun and proves nothing) and ask the judge again: it reads the
  newest attempt of each job and answers `eligible` (its reason says "after a recorded failure (a
  flaky check)", which is the flake ledger) or, on a second failure, `evicted`, and the candidate
  goes back to its child naming N. In a project with no plan there is no judge: the parent makes
  the same single rerun by hand, reads the new attempt through `merge-evidence` on N (the newest
  attempt of each job counts), and adds one to `ci_reruns` in the merge record, with the job, the
  failed run id and attempt, and the passing attempt. Three sequences, in both modes: a failure and
  then a green rerun lets the candidate go on, with the flake recorded; a failure and then a second
  failure, of the same job or another, is a real failure and the candidate goes back naming N; a
  failure that appears after a green reading of N is a first failure when no rerun was spent on N
  and a second one when it was. It is one rerun per head N; a new head starts its own. A job still
  pending is waited on, never rerun; a failed `Devin Review` status or a review finding is not a
  job and is not rerun.

If the base moved again while N's checks ran, N is behind again. A refresh restarts every job, so a
head whose jobs are still running is not refreshed again: wait for them. Then repeat from N, which
is now the current head: read the new tip D2, update with the call guarded by N, and prove that one
step, `--previous N --head N2 --base D2`. `merge-evidence` on N, without `--restate`, reports exactly
one problem, `candidate_behind`, and `pinned.headSha` is the head the parent produced; the forge
reports no conflict; every thread is in `threadsSeen`; the criteria are still the ones ruled
verified at P. `--previous` is P (the head the relay verdict names) or a head whose `evidence:` line
is already in the coordination record, never a head only the parent has looked at: the proof is
of one update at a time, a chain is sound because each link was proved, and the merged mark,
written after the landing, carries every step's evidence line. The check refuses P to N2 taken in one step as
`not_built_on_previous` (N2's first parent already contains P), and that refusal on a chain is a
call for the stepwise proof, not a return.

If the base moved between reading D and the update call, the update merged a newer commit than D
and the check says `not_from_base` (that commit is not an ancestor of D); `not_the_dev_tip` says
the commit named was newer than the one merged (a pull request's `base.sha` was read for D), or that
the base moved again since. Read the tip of the base from the forge again, T, and run the check with
`--base T`. Exit 0 means N's second parent is T, so N is current and proved; any refusal sends the
candidate back to its child. `--base` is only ever a tip read from the forge: never a head's second
parent, and never a commit that no forge reading named as the tip. The check proves an update
against whatever commit it is named, and an ancestor of the base that was never its tip passes as
readily (a commit of a merged side branch, or an intermediate commit of a push that moved the base
by several commits).

Then merge as above: reread the head and base, merge with the expected-head guard on N.

**What still goes back to the child.** A conflict comes before an update: the forge refused the
call, or the state reads `DIRTY`. When every conflicting file lies in a place the plan declared
`mechanical`, the parent settles the conflict itself ([Resolve a mechanical conflict
yourself](#resolve-a-mechanical-conflict-yourself)); a conflict anywhere else is the child's.
That update did not happen, so the branch is where the last one
left it: at P when this was the first attempt, at the newest head the parent made when it was a
repeat. The correction is the old base-refresh correction that names that head and the conflicting
base, and it carries the [restoration block](task-packet.md#restoration-block) when an earlier
refresh already moved the branch past what the child holds. Everything else is found on a head the
parent made: a refusal from `base-refresh check`; a required job that failed on N again after its one rerun, or a
`Devin Review` status that failed; a thread on N outside `threadsSeen`, or a blocking finding on N
under [impact](#judge-a-finding-by-its-impact). Those corrections name N and not P, and carry the
restoration block because the child's worktree is now behind its branch. Waiting is not a reason to
return it. The route is otherwise the
[needs-changes route](../SKILL.md#return-corrections-to-the-existing-task), unchanged. A conflict that shows after the verdict and before the acceptance takes that route on the same receipt: [a base conflict after the ruling and before the acceptance](#a-base-conflict-after-the-ruling-and-before-the-acceptance).

**In a DAG-managed project the update comes before `dag-accept`, and the jobs on N are read after
it.** `dag-accept` records the head the forge shows as the accepted head and takes none from the
child's report; a head that moves afterwards reads `stale_head` at `dag-merge-judge` and
`dag-merge-request`, leaves the node `blocked:stale_head`, and the same output cannot be accepted at
the new head (`merge_candidate_moved`). An acceptance reads no CI, no review and no thread
(`docs/relay/dag-scheduler.md`, "Accepting a result"), so the order is: the verdict `verified`, the
update and the base-refresh check on N, `dag-accept` (it records N), `dag-merge-judge` for the
jobs on N (`checks_pending` is waited on; the first failure is the `retry_same_sha` above),
`merge-evidence` on N for the reviews and threads, which the judge does not read,
`dag-merge-request`, the merge lane, the merge, `assignment-mark merged`,
`dag-integration-observe`. The judge reads the pull request through the relay's own checkout, so N
has to be fetched there, or it fails as a host problem and writes nothing. A base that moves after
the acceptance, for example while N's jobs run or while the candidate waits for its turn, cannot be
refreshed by the parent at this baseline: the update would move the head off the accepted one, and
the judge reads `stale_base`. That candidate does not go back by a second ruling, because the relay takes none on an accepted head; [a base conflict after the ruling and before the acceptance](#a-base-conflict-after-the-ruling-and-before-the-acceptance) says what remains. The window now includes N's job time, and another project's landing during it
counts; refreshing only the candidate about to merge is what keeps it short. The limit is the
scheduler's, which has no re-acceptance of a verified refresh.

**On the relay's merge lane**, claim the turn with N (`merge-turn-request --head N`). A claim already
made at P is restated with `merge-turn-ready --head N`, which resets readiness and, for a turn
already holding, issues a new grant: acknowledge it with `merge-turn-acknowledge` and declare the
candidate ready again. `merge-turn-check` states N and the required names read from the reading of N.
It also compares N with the head of any work report recorded for the assignment, and refuses a
different one as `merge_candidate_moved`. Nothing in the product records a work report
(`docs/port/decisions.md`, section 53), so N has no report head to disagree with; a store that holds
such a row naming P anyway refuses N there, when the turn is already held: return the turn and send
the candidate back like any other refusal.
A merge made outside the lane needs no step from the parent that made it: the next
`merge-turn-check` finds the base the last landing recorded behind the branch and records it again
itself when it can confirm the move as merge commits no landing recorded
([the base a landing records](relay.md#checking-landing-and-correcting-a-landings-base)).

Everything from the grant to the landing runs in the foreground of the turn and records its steps
with `merge-turn-progress` ([Working inside a merge turn](relay.md#working-inside-a-merge-turn)). Never
start the refresh, the CI wait or the merge in the background: detached work is not tied to the
turn, and a holding turn that records nothing for the holding limit can be passed on to the next
waiter.

**Record the refresh** where the merge is recorded. `assignment-mark merged --evidence` carries the
check's `evidence:` line exactly as printed (previous head, dev tip, new head, tree OID and the rule
applied), one line per step when the candidate was refreshed more than once (after a conflict settled by
a mechanical rule also the `applied:` lines and the wording given in [Resolve a mechanical conflict
yourself](#resolve-a-mechanical-conflict-yourself)), and the
`merge-evidence` verdict on N, because `--expected-event` pins the child's report, which names P:
the merged head and the receipt revision differ, as they already do after a child's base refresh,
and the evidence text is the only place that difference is explained. The merge record in the
coordination record carries the same lines. The parent copies them from the check's output; it
does not retype the fields.

**Record what the lane costs**, beside the landed and candidate revisions: one entry per merge, and
one record per conflict episode, written when the episode closes, so that a candidate that never
lands still leaves its returned episode behind:

    merge-lane: pr=<N> landed=<commit>
      verified_at=<UTC> granted=<UTC> landed_at=<UTC>
      ci_runs=<attempts on every head> ci_runs_parent_heads=<attempts on heads the parent made>
      ci_reruns=<reruns the parent asked for, at most one per head>
      dev_after=<success|failure|unread|pending> evidence=<the evidence line, once per step>
    merge-lane-episode: pr=<N> at=<UTC> outcome=<handled|returned>
      because=<the refresh passed the check | the mechanical check passed | a forge conflict | a refusal
      (code) | an exit 2 not cleared | stale_base | a merge state still UNKNOWN>

An entry for a candidate that was refreshed once and landed after one rerun reads, in shape only
(the numbers are illustrative): verified 01:02:00Z, granted 01:20:00Z, landed 01:41:30Z,
`ci_runs=9 ci_runs_parent_heads=4 ci_reruns=1 dev_after=success`, with one episode record
`outcome=handled`; its lane wait is 18 minutes and its service time S is 21.5 minutes.

- Times, not minutes, are recorded, so that the pre-registered comparison can recompute them. The
  lane wait is `granted` minus `verified_at` (the verdict `verified`); it is not the contract's W,
  the node's implementation time. S, the merge-lane service time, is `landed_at` minus `granted`.
  `granted` is the merge-turn grant (`merge-turn-show`), or, with no lane, the instant the parent
  took the candidate as next to merge (the update call when it needed one). `landed_at` is the
  observed landing (`merge-turn-land`, or the forge's `merged_at`), never the merge request being
  accepted. A time that cannot be read is written `unread` and the figure that needs it is not
  computed, never zero.
- CI runs per merged pull request is the number of workflow-run attempts, each attempt counted once,
  on every head of the pull request until it landed (`gh api
  "repos/OWNER/REPO/actions/runs?head_sha=<head>"`, the `pull_request` runs and their `run_attempt`);
  the entry says apart how many ran on heads the parent made. A commit that was never a pushed head
  has no run.
- Conflicts are counted as episodes, for the base reason only. An episode is one occasion on which
  the only block on a candidate was the base. It is handled when the parent's refresh passed the
  check (`base-refresh check`, or `base-refresh mechanical` for a conflict settled by a rule), and returned
  when the candidate went back to its child for the base: a forge conflict the rules do not settle, a
  refusal, an exit 2 that a second ask did not clear, a `stale_base`, a merge state still
  `UNKNOWN`. Each episode has one record, whether or not the candidate lands later; a candidate
  that needs no refresh has none.
- The conservative return ratio is the returned episode records divided by all episode records of
  a run, with every doubt counted as returned; with no record the ratio is not defined and is
  written so, never as 0. It has no target here: targets belong to the pre-registration of the
  DAG comparison.

Two acceptance lines are stated as what is measured, not as properties of the rule. *0 false
accepts by the checker* is a count over audited passes: the numerator is the passes later shown to
hold anything beyond P plus D, the denominator the passes audited, and a pass not yet audited is
counted apart, not as zero. A pass is audited after the landing by running the check again on the
evidence line's fields (`--base` set to its `dev_tip`) and by reading the landed diff against P.
*0 post-merge dev reds caused by a parent refresh* is a count over read landings: the denominator
is the refreshed candidates' landing commits whose `dev-gate` run (the `push` run on the commit,
`dev_after`) was read, and the numerator the failed runs that the parent traces, from the log and
the diff of P against N, to the combination of P and D that N's jobs passed over. Under the strict
ruleset the landed tree is N's tree when the base takes N by a merge commit or a squash (read it
with `git rev-parse <landing>^{tree}`), so a red there that is not that combination is something
else: a flake (it passes on a rerun of the commit), a job that only runs on a push or an
infrastructure failure before the code ran, or undetermined. Those three are reported beside the
count and are not in it; so are a landing not yet landed (`pending`) and one not read (`unread`),
and none of them is ever counted as zero.

### Resolve a mechanical conflict yourself

A conflict is not always a reason to return a candidate. Two conflicts are frequent and have a fixed answer:
the `plugin.json` version suffix, which is derived from the payload and is wrong on both sides once the
other lands, and entries that two pull requests append to the end of one append-only list. When the plan
declared the place `mechanical` ([Release by region grade](region-grades.md)) the parent settles the
conflict by the rule of the place and proves the result with `crw skill base-refresh mechanical`, so the
candidate does not make a round trip of a whole generation for a merge whose answer was declared.
`check` cannot prove such a head, because it is not a pure merge of the base.

Do it only when the three conditions of the refresh above hold at the verified head P, with one change: the
forge reports a conflict (the update call answered 422 with a merge conflict, or the state reads
`DIRTY`) instead of none. And only when the declarations are known: the file of the regions the candidate
declared, and one for every node whose landing the conflict comes from (`dag-ready`'s `release.basis` names
the holders, keeps at most 16 rows and says by `basis_omitted` when it dropped some). The check takes the
rule of a place only when every declaration given names it, as the scheduler's overlap judgement does, and
it cannot know that the set it was given is complete: a parent that cannot name every node does not go on, and
the candidate goes back to its child. Where the installed `crw skill base-refresh` does not list `mechanical`
among its commands, the parent does not settle a conflict either.

1. Read D, the tip of the base, from the forge as for any refresh. In a checkout of your own (never the child's
   worktree, never one that holds other work), fetch P and D, check P out in a worktree of its own and merge D
   with a merge commit: `git merge --no-ff -m "Merge branch 'dev' into <branch>" <D>`. `git diff --name-only
   --diff-filter=U` lists the conflicting files. A file no declaration covers, a conflict that is not a change
   both sides made to one text file (a deleted or renamed file, a binary file, a link), and a place whose rule
   is `renumber` end it here: `git merge --abort`, and the candidate goes back to its child (below).
2. Settle each conflicting file by its rule and by nothing else. A file that merged cleanly stays as git made it.
   - `union`: keep every line of both sides, each side's lines in their own order, and add nothing. `git show
     :1:<path>` is the base, `:2:<path>` the candidate and `:3:<path>` the dev tip. When both sides only
     appended to the end of the list, the result is the candidate's file followed by the lines the dev tip
     added after the base's last line: `{ git show :2:<path>; git show :3:<path> | tail -n +$(( $(git show
     :1:<path> | wc -l) + 1 )); } > <path>`. Do not settle it by deleting the conflict markers: git moves a
     line both sides added out of the conflict, so what is left holds it once, and the check counts it twice.
   - `regenerate:<command>`: discard both sides' versions, take either side's (`git checkout --theirs
     <path>`) and run the declared command on the merged tree, from the repository root. For
     `plugins/crw/.codex-plugin/plugin.json` that is `go run -tags dev ./cmd/crw-dev ci plugin
     --record-version`, after the merge of the skills has settled, because the suffix digests the whole
     payload. Declare a command that works from the root of a fresh checkout with the caller's `PATH`: the
     check runs it there.
3. Add the settled files and commit; the merge commit has the parents P and D in that order. Before pushing
   anything run, with N the new commit:

       crw skill base-refresh mechanical --repo <your checkout> --previous P --head N --base D \
         --repository OWNER/NAME --regions <the candidate's declaration> --regions <each landed node's>

   Exit 0 says the head is P merged with D, every conflict is in a mechanical place, the head differs from
   git's clean three-way result only there, every union keeps all lines of both sides (the result has as many
   lines as the base plus both sides' additions), and each regeneration command, run twice on a checkout of the
   head (the second time from the dev tip's version of the file), leaves the head's files as they are. The
   command comes from the declaration and runs the head's own code with your rights, as running that head's
   tests would, so it is a head whose changes were verified at P that it is run on. Exit 1 is a refusal, and its
   first line names the code:
   - `no_update`, `not_built_on_previous`, `not_a_merge`, `parents_swapped`, `not_from_base`, `not_the_dev_tip`:
     N is not an update of P by D, as for `check`; `nothing_resolved`: nothing conflicted and nothing was settled
     by a rule, so the proof of N is `check`.
   - `conflict_outside_mechanical`, `differs_outside_mechanical`: a conflicting file, or a file where N differs
     from git's result, is not covered by one rule that every declaration given names (no symbol region, no
     region of another grade touching it, no shared contract surface, and two trees that hold one are exclusive
     together); `conflict_not_content`: the conflict is not one text file that both sides changed (a deletion, a
     rename that git followed or that cannot be excluded, a moved file, a binary file, a link); `ambiguous_base`:
     more than one merge base; `rule_unchecked`: the rule is `renumber`, which names no id and has no check.
   - `union_not_additions`: a side changed or removed a line of the base; `union_line_lost`: a line of a side is
     missing from the result or out of its side's order, or a line both sides added stands once; `union_line_added`:
     the result holds a line more often than the two sides give it (a line of its own, a repeated line, a conflict
     marker); `union_not_conflicted`: the path merged cleanly and N changed it; `union_result_not_a_file`: N
     removed the file or changed its mode.
   - `regeneration_differs`: the command changes a file N has (the version suffix left at one side's value), or
     N holds the file with a mode that git's merge does not give it (a rule rebuilds bytes and leaves the mode alone);
     `regeneration_not_deterministic`: the two runs differ, or the result depends on what the file held before;
     `regeneration_touches_outside`: the command changes a tracked file outside its regions, its bytes or its
     executable bit;
     `regeneration_failed`: it exits non-zero.

   Exit 2 means git or a command could not answer (a timeout, a command that cannot start, a declaration that
   cannot be read): it is not a pass, so fix the cause and ask again, and where a second ask does not clear it
   the candidate goes back.
4. Only on exit 0, push N to the candidate's branch as a fast-forward from P (`git push origin N:<branch>`, never
   forced). A rejected push means the branch moved, so start again from the top. A refused N is never pushed:
   the branch stays at P and the correction is the plain base-refresh correction that names P and the
   conflicting files, with the first line and the `facts:` line of the refusal. From the push on, N is a head the
   parent made, and everything under "On the new head N nothing about P carries over" above applies to it
   unchanged: every required job and the review on N, the one rerun, the order around `dag-accept` and the merge
   lane. The check proves the resolution, not that N works: the jobs do.

**The evidence.** The merged mark and the merge record name the rule applied in plain words, say that the check
passed, and carry the check's output as printed:

    base refresh by mechanical resolution, check passed: append-only union of docs/port/refactor-backlog.md; plugin.json version regenerated
    evidence: previous=<P> dev_tip=<D> head=<N> tree=<N's tree> rule=mechanical_resolution
    applied: union path=docs/port/refactor-backlog.md base_lines=<n> previous_added=<n> dev_added=<n> result_lines=<n>
    applied: regenerate path=plugins/crw/.codex-plugin/plugin.json command="<command>" runs=2 identical=yes matches_head=yes

The words for the two usual rules are "append-only union of <path>" and "plugin.json version regenerated"; any other
regeneration reads "<path> regenerated by <command>". Copy the `evidence:` and `applied:` lines, do not retype them, and
add the `merge-evidence` verdict on N, as for any refresh. A pass is a fact about the resolution: it does not say that a
job, a review or a merge happened on N.

**What goes back to the child.** A refusal, an exit 2 that a second ask does not clear, a conflict in a file no
declaration covers or whose declaration the parent cannot complete, a place whose rule is `renumber`, and a
conflict of any other kind. The correction is the base-refresh correction: it names P, the base tip and the
conflicting files, quotes the refusal, and says which rule the plan declared for each place. The child resolves
the rest, and the hunks it settled by a rule are `mechanical` in its handoff ([What a handoff
discloses](task-packet.md#what-a-handoff-discloses)); the parent may run the same check on the child's merge
(`--previous` the head it verified, `--head` the child's merge commit) instead of reproducing the hunks by hand.

### A base conflict after the ruling and before the acceptance

The verdict `verified` was given, and before `dag-accept` (or, in a project with no plan, before the merged mark) the base conflicts: the forge refuses the update, the state reads `DIRTY`, or another project's landing changed the head you refreshed. A conflict whose every file lies in a `mechanical` place is the parent's to settle first ([Resolve a mechanical conflict yourself](#resolve-a-mechanical-conflict-yourself)); any other candidate goes back to its child, and the way is the needs-changes ruling on the same receipt: the relay replaces a `verified` ruling by `needs_changes` while nothing rests on it, opens the next generation and queues the correction to the same child. (A criteria re-review that is open is decided first, as it always was.) It needs nothing from a conflict handling of the parent's own.

1. **Check that nothing rests on the ruling.** Read `codex-session-relay --state "$RELAY_STATE" assignment-show --relationship <rel>`: the state is `verified` and the head is the event you ruled. No `dag-accept` was recorded for the node: `dag-ready --plan <plan>` reads it as `verifying`, and a node with an acceptance never does (it reads `done:accepted`, `done:integrated`, `stale` or `blocked:stale_head`, an accepted node whose head moved). No `assignment-mark --mark merged` was either. A merge turn of this candidate that you hold is `holding` (`merge-turn-show` names its state); one that is `merging` or of unknown effect is resolved first (`merge-turn-resolve`), and one that landed means the work is on the target, which new work corrects and a ruling does not.
2. **Rule `needs_changes` on the same receipt:**

       codex-session-relay --state "$RELAY_STATE" verdict --event <the verified event> --verdict needs_changes \
         --verdict-turn <own turn> --restoration <criterion id> \
         --finding '<criterion id>=needs_changes:<the correction and its restoration block>'

   The finding carries the [restoration block](task-packet.md#restoration-block): the head P the verdict named, the head N the branch has now, the conflicting base D and the paths the forge or `git merge-tree` names, the siblings' landings, and that this correction asks only for the base to be brought up to date (the child merges the base, names the kind of each merge and reruns that kind's checks and no more).
3. **Read the answer, not the exit code.** A ruling that was replaced answers the new record: `verdict` is `needs_changes`, `nextExecutionGeneration` is the generation the child will report in, and `_supersedes` names the verified ruling it replaced; `assignment-show` then reads `needs_changes`.
4. **Release the turn you hold** once the ruling is given, because the child works on the next generation and the lane must not wait for it: `merge-turn-release --turn <turn> --actor <id> --disposition returned --reason '<what invalidated the readiness>'`.
5. **An older relay changed nothing.** An answer that is still `verified` and carries `_replay` means the installed relay is older than this rule and answered a different verdict with the recorded one: nothing reached the child and `assignment-show` still reads `verified`. Do not repeat the call, and do not open a parallel path to the child (the rule of [Return corrections to the existing task](../SKILL.md#return-corrections-to-the-existing-task)). Record the correction as undelivered on the assignment and hand the decision to whoever owns it, as for a relay that does not carry the restoration declaration.
6. **A refusal says what remains**, with its reason, and wrote nothing: the verified ruling stands. `disposition_conflict` names the cause: a plan accepted the event, the work is marked merged, or a merge turn is merging, of unknown effect or landed. `stale_generation`, `superseded_revision`, `revision_ambiguous` and `relationship_not_active` say that the receipt is not the head of an active relationship, and each names its route. After the acceptance the relay takes no second ruling (unless a criteria re-review is open), so a verdict is not the way back for a candidate returned for any reason after it, a second job failure, a failed `Devin Review` status, a thread outside `threadsSeen` or `stale_base` included. The DAG records a correction only for a node whose stale reading says `correct`; for a result that is current (a base that moved after the acceptance is such a case) there is no recorded route in this build, so report it on the coordination record and do not open a generation that `dag-correct` will refuse.
7. **When the child reports again** in the new generation, the receipt is a new event: acknowledge it and rule it as for any receipt ([the parent verifies](relay.md#the-parent-verifies): `claim`, `ack-proof`, `ack`, `verdict`), and refresh the base yourself if only the base moved again, as above. The child declares the receipt it replaces by its revision hash with `--supersedes-revision` only when it reports again inside the same generation.

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
