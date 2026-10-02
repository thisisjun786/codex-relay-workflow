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

Do it only when all three hold at the head P that the child reported and the parent verified (a
second refresh of the same candidate has its own reading, below):

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

Update with the forge's call, guarded by the head you verified, and never by rebase, reset or
force-push:

    gh api -X PUT repos/OWNER/REPO/pulls/N/update-branch -f expected_head_sha=<the head you verified>

A 202 says the forge accepted the request, not that the branch moved: read the pull request until
its head is no longer the one you verified. A 422 whose message names a merge conflict is a conflict
(below). Any other 422, such as a head that moved because the child pushed, leaves the branch
untouched and means the verification described a head that is gone, so start again from the top.

On the new head N nothing about P carries over. Before the merge:

- **What N is.** `crw skill base-refresh check --repo <a checkout that has fetched N and the base>
  --previous P --head N --base origin/dev` answers from git alone. It passes (exit 0) only when N
  is P plus merges of the base: walking the first parents from N it meets nothing but merge commits
  of two parents, each second parent already on the base, each tree exactly what git merges from
  the two parents, and the walk ends at P, so no edit, hand resolution or file of its own rides
  along. Exit 1 names what N is instead (`no_update`, `not_built_on_previous`, `not_a_merge`,
  `not_from_base`, `merge_conflicts`, `tree_differs`); exit 2 means git could not answer
  (a missing commit, a shallow checkout, git older than 2.41), which is not a pass: fetch and ask
  again. Run it before the merge, because a head that has landed is an ancestor of its base; a
  replay afterwards names the base tip seen before the landing in `--base`. There is no hand-run
  substitute: plain git commands in a checkout follow its replace refs, merge drivers and
  attributes, and what they print is not this answer. Where the installed `crw` does not list
  `base-refresh` among the `crw skill` families, the parent does not refresh, and the candidate
  goes back to its child as it did before this rule.
- **Every job and the review, on N.** `merge-evidence` on N, without `--restate` because the
  child's record names P, must exit 0: each required job a success at its newest attempt, reviews and
  threads read to the end, and the candidate no longer behind. Jobs still pending, and a reading that
  is unknown or stale, are waiting: read again. Devin reports a `Devin Review` commit status on a
  head it has analysed; read once on a merge-only head, it completed within seconds and left no
  review object, and what posts it is the repository's Devin setup, which is not assumed for every
  head: `gh api repos/OWNER/REPO/commits/N/status`. Pending is waiting, `success` means it ran,
  and its review and threads on N are then read like any other; no status at all is recorded as an
  unavailable reviewer, never as a pass. Every thread on N has to be in the record's `threadsSeen`.

If the base moved again while N's checks ran, N is behind again. A refresh restarts every job, so a
head whose jobs are still running is not refreshed again: wait for them. Then repeat from N, which
is now the current head. `merge-evidence` on N, without `--restate`, reports exactly one problem,
`candidate_behind`, and `pinned.headSha` is the head the parent produced; the forge reports no
conflict; every thread is in `threadsSeen`; the criteria are still the ones ruled verified at P.
Guard the call with N. The check keeps P as its anchor, `--previous P --head <the newest head>`,
so it proves the whole chain again: a chain of updates is still P plus merges of the base and
nothing else. Refresh the candidate that is next to merge, not the whole queue.

Then merge as above: reread the head and base, merge with the expected-head guard on N.

**What still goes back to the child.** A conflict comes before an update: the forge refused the
call, or the state reads `DIRTY`. That update did not happen, so the branch is where the last one
left it: at P when this was the first attempt, at the newest head the parent made when it was a
repeat. The correction is the old base-refresh correction that names that head and the conflicting
base, and it carries the [restoration block](task-packet.md#restoration-block) when an earlier
refresh already moved the branch past what the child holds. Everything else is found on a head the
parent made: a refusal from `base-refresh check`; a required job that failed on N, or a
`Devin Review` status that failed; a thread on N outside `threadsSeen`, or a blocking finding on N
under [impact](#judge-a-finding-by-its-impact). Those corrections name N and not P, and carry the
restoration block because the child's worktree is now behind its branch. Waiting is not a reason to
return it. The route is otherwise the
[needs-changes route](../SKILL.md#return-corrections-to-the-existing-task), unchanged.

**In a DAG-managed project the update comes before `dag-accept`.** `dag-accept` records the head the
forge shows as the accepted head and takes none from the child's report; a head that moves
afterwards reads `stale_head` at `dag-merge-judge` and `dag-merge-request`, leaves the node
`blocked:stale_head`, and the same output cannot be accepted at the new head (`merge_candidate_moved`).
The order is: the verdict `verified`, this refresh and its checks, `dag-accept` (it records N),
`dag-merge-request`, the merge lane, the merge, `assignment-mark merged`, `dag-integration-observe`
(`docs/relay/dag-scheduler.md`). A base that moves after the acceptance, for example while the
candidate waits for its turn, cannot be refreshed by the parent at this baseline: the update would
move the head off the accepted one. That candidate goes back to its child for a new generation as it
did before this rule. The limit is the scheduler's, which has no re-acceptance of a verified refresh.

**On the relay's merge lane**, claim the turn with N (`merge-turn-request --head N`). A claim already
made at P is restated with `merge-turn-ready --head N`, which resets readiness and, for a turn
already holding, issues a new grant: acknowledge it with `merge-turn-acknowledge` and declare the
candidate ready again. `merge-turn-check` states N and the required names read from the reading of N.
It also compares N with the head of any work report recorded for the assignment, and refuses a
different one as `merge_candidate_moved`. Nothing in the product records a work report
(`docs/port/decisions.md`, section 53), so N has no report head to disagree with; a store that holds
such a row naming P anyway refuses N there, when the turn is already held: return the turn and send
the candidate back like any other refusal.

**Record the refresh** where the merge is recorded. `assignment-mark merged --evidence` names N and
the check behind it (the helper's first line and the `merge-evidence` verdict on N), because
`--expected-event` pins the child's report, which names P: the merged head and the receipt revision
differ, as they already do after a child's base refresh, and the evidence text is the only place that
difference is explained.

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
