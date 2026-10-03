# Coordination between parents

Relay-owned records, like the linkage tables and for the same reason: the five schemas under
`contract/schema/` (`src/codex_session_relay/schema/` until todo 44) are byte copies of a frozen contract, none of them
describes anything on this page, and nothing here changed them.

Three concerns, three modules, one shared foundation. A merge turn is keyed by a target and
lives for one tenure; an execution slot is keyed by a subject and lives for one generation; an
agreement is keyed by a place in a tree and can outlive both. They share `coordination.py`
for derived identity and retained refusals, and nothing else.

## Why a target is a repository and a base ref

Not a project. One project can own work in several repositories and two projects can share one
base branch, so keying the turn on the project would serialise work that never contends and
fail to serialise work that does. Work on a different target is parallel by construction rather
than by permission, which is what the criterion about independent repositories asks for.

## Identity

    target_key   = "tgt-" + sha256(repository|base_ref)[:32]
    turn_id      = "mtn-" + sha256(target_key|holder_task_id|tenure)[:32]
    slot_id      = "slt-" + sha256(subject_kind|subject_key|tenure)[:32]
    limit_id     = "lim-" + sha256(scope_kind|scope_key|dimension)[:32]
    region_id    = "rgn-" + sha256(repository|base_revision|path|region_kind|region_key)[:32]
    agreement_id = "agr-" + sha256(region_id|low_project|high_project|tenure)[:32]

A **turn** keys WITH its holder, because a claim is one parent's claim, the same reason a scope
binding keys with its task. A **tenure** is in every one of these that can recur: the same
parent taking the turn again, or the same subject being re-reserved after a release, is a
second fact and not a replay of the first. Without it a re-reservation would either collide
with the retained released row or overwrite it, and that row is the evidence a duplicate or
contradictory release is detected against.

An **agreement** sorts its two project keys before hashing, so either side proposing converges
on one record rather than two mirror images. A **region** carries its base revision, which is
what makes currency mechanical instead of remembered: a new revision derives a different
region, so an agreement made against an older tree cannot silently stand for a newer one.

## Nothing advances because time passed

A holder that reached the currency check may already have merged. So:

- `merging` never expires and cannot be cancelled.
- An outcome nobody established is recorded as `unknown`, and `unknown` OCCUPIES the target.
- The only key is `merge-turn-resolve` with an observation: a pull request state and the base
  sha somebody actually looked at. Elapsed time is not an observation and never becomes one.

The refusal a caller gets in that state names `land` and `merge-turn-unknown` as the two routes
out, because "wait longer" is the one answer that never helps.

The one thing that moves because a holder went quiet is a turn that is still `holding`: a holder that
follows the protocol has not begun to merge before `merge-turn-check`, so a holding turn silent past
the holding limit can be passed on (below). That is a recorded act by another parent, not the
passing of time, and it is no proof that the pull request is unmerged.

## The base a landing leaves behind

A landing records the base branch it leaves behind, and the next candidate on that target has to
restate exactly that value before `merge-turn-check` lets it merge. That value used to be typed by
the caller. In the CRW-124 G1 trial on an installed relay both parents typed the base their
candidate had been checked against rather than the one the branch pointed at after the merge, so
their verified successors were refused `merge_currency_stale`, and nothing could correct a landed
turn: `merge-turn-resolve` admits only an unknown one.

So the merge turn reads one fact from the target itself: the commit its base branch points at. It
reads it at the four moments it records a base, and at no other time. One more reading, how the
branch got from the last landing's base to that tip, is taken only by a check that finds the two
differ (below):

| Command | What it reads and records |
| --- | --- |
| `merge-turn-check` | The restated `--base-sha` must be the branch tip now; the reading, not the caller's text, is stored as the base the merge was checked against. When the last landing recorded a different base, it also reads how the branch moved and may record that base again (see "A merge outside the lane") |
| `merge-turn-land` | The branch after the merge, recorded as the base the next candidate must restate. `--landed-sha` is recorded as stated; `--observed-base-sha` is an optional cross-check |
| `merge-turn-resolve` | The branch now, recorded with the outcome. `--observed-base-sha` is cross-checked |
| `merge-turn-restate-base` | The branch now, recorded over the latest landing's base, with the value it replaces kept in the ledger |

An absolute path is read with git (`show-ref --verify` on `refs/heads/<branch>`, repository
discovery off, every `GIT_*` variable removed), so revision syntax in a branch name is never
evaluated; the path must be the repository the merge goes into, not a working clone. An
`owner/name` repository is read with one GET through the forge module, the reader
`merge-evidence` uses. Anything else, and any failure to read, is unreadable. Each of these
commands reads before its single transaction, never inside it, and only once the caller and the
turn's state allow the call, so a refused caller never reaches the target.

Three refusals come with it, each leaving the turn where it was:

- `merge_target_unreadable`: nothing could be read, so nothing is recorded. A check stays
  holding (its blocked cause is `target_unreadable`), a landing stays merging, and a merged
  resolution stays unknown. An open or closed resolution still returns the turn, with no base
  recorded, because the next check reads the target itself.
- `merge_base_mismatch`: the caller stated a base the branch does not read. Object names
  are compared in full, ignoring case; an abbreviation is refused, because nothing here can tell
  whether it is ambiguous.
- `merge_base_not_advanced`, from `merge-turn-land` only: the branch still reads the base the
  check read before merging, so the merge is not on it. A candidate that already was the branch
  tip at the check is exempt. A merge that changed nothing because the base already contained the
  candidate is recorded through `merge-turn-unknown` and `merge-turn-resolve --pr-state merged`,
  which has no such rule.

A turn that entered merging before this change was checked against a typed base, so the branch
cannot show whether it landed. `merge-turn-land` refuses it with `merge_evidence_required` and
names the route: `merge-turn-unknown`, then `merge-turn-resolve` from the pull request's state.
A check records its reading on the engine's own holding-to-merging transition, which is how the
two kinds of turn are told apart.

`merge-turn-restate-base --turn <landing> --actor <holder or supervisor> --evidence <why>`
corrects the base the currency check compares against. It admits only the latest landing on the
target, and not while another turn there is merging or unknown. The ledger entry keeps `from`,
`to`, the evidence and the reading's source, and `merge-turn-show --turn` lists them as
`baseRestatements`. It records what the branch reads now, which is what the next candidate has
to restate, so it can also carry a commit written outside the relay after the landing. When a
`merge-turn-check` is refused because the last landing recorded a different base, its detail
names that landing, this command with the holder to run it as, and who else may run it. After a
merge outside the lane the check usually does not need it: see the next section.

Each reading is taken before the command's transaction, never inside it, so a branch that moves
between the reading and the write is recorded as it was read. That errs one way only: the next
candidate is refused against an older base until the landing is restated. The merge itself
still relies on the forge's own expected-head guard.

What this does not establish: `landedSha` is the holder's statement and is not checked for
containment in the branch, so a merge that failed while another write moved the branch is
recorded as landed, with the true base. On a resolved landing `landedSha` is the candidate head,
because no landing commit was reported. The comparison with the last recorded landing is kept
beside the reading on purpose: a move of the branch that no landing recorded is never absorbed
silently. Where the relay can confirm the move it records the base again itself, with the reason
in the ledger; where it cannot, the check stops until the landing's holder or supervisor
restates the base, and that restatement is the record of why the base moved.

## A merge outside the lane

A pull request merged outside the lane (a node the lane cannot serve, a parent using the older
route) moves the base branch past the base the last landing recorded. Before CRW-403 the next
parent's `merge-turn-check` was refused `merge_currency_stale` against that record, and only the
landing's holder or its supervisor could correct it, so one parent's outside merge stopped every
other parent and the waiting parents could not tell why.

Now the check that finds the recorded base differs from the tip it states reads how the branch got
there, before its transaction like the tip: the first-parent line of the base branch from the tip
down to the recorded base, one commit at a time, at most 32 commits and 90 seconds in all: for a
local path the stored commit objects through `git cat-file` with replacement off, so a replace
ref, a grafts file or the commit graph cannot change what is read; for `owner/name` one
`git/commits/<sha>` GET per commit. The move is **confirmed** when all of these hold, and any
other result is unconfirmed:

- the reading runs from the recorded base to the tip this check read, and the line from the tip
  reaches the recorded base within 32 commits without a gap;
- every commit on that line is a merge commit (two or more parents);
- none of them is the `landedSha` of a landing on this target;
- no other turn on the target is merging or unknown;
- the latest landing is still the landing, with the recorded base, that was read.

A confirmed move is recorded inside the check's own transaction, by the writer
`merge-turn-restate-base` uses: one `landing_base_restated` ledger entry on the landing under the
next `restate-base:<n>`, the landing's recorded base set to the tip, and the same
`merge_turn_base_restated` journal row with `automatic` and `checkTurnId` added. The entry's
actor is the checking holder; its evidence begins `automatic:` and lists each merge commit with
its subject, so `baseRestatements` keeps the old value, the new value and the reason beside each
other. The check goes on to its other gates, and the restatement stands even if one of them
refuses, because it is a fact about the branch and not about the candidate. The check's answer
carries `landingBaseRestated` (`turnId`, `from`, `to`, `sequence`, `source`, `mergeCommits`) only
when it wrote one. No authority rule applies, because the relay writes what it read, not what the
caller said; the manual command and its rule are unchanged.

An unconfirmed move changes nothing and refuses `merge_currency_stale`, as before. The detail
keeps its sentence, then names the base the branch now reads, says why the relay did not restate
it (the reader cannot read how the branch moved, the read failed, the line does not reach the
recorded base, a commit on it has one parent, a commit is a recorded landing, a turn is in flight,
or the landing changed during the call), and gives the command with the landing and the holder
filled in: `merge-turn-restate-base --turn <landing> --actor <holder> --evidence '<why the base
moved>'`. The supervisor above the landing's project may run it too.

What this does not establish. Confirmation is by commit shape, not by pull request identity: a
hand-made merge commit counts, and branch protection, not the lane, decides who may merge. A
repository whose pull requests land as squash or rebase commits has no merge commits on its
line, so it is never confirmed and keeps the manual command. More than 32 merges since the last
landing is unconfirmed. `landedSha` is a statement (a resolved landing records the candidate
head), so excluding the lane's own landings is a consistency check and not proof. Nothing watches
the branch: a recorded base is brought up to date only when a check finds it behind.

**The merged mark.** `assignment-mark merged` was examined as another way for the lane base to
follow the branch and is not wired to it. A mark names a relationship and the event it integrated,
with free-text evidence; it carries no repository, base ref or commit, so it cannot say which
branch tip it concerns, and reading its text would turn an assertion into a reading. The lane
records only what it read from the target. A mark is also written after the merge, which an
outside merge may never be followed by, and it is written by the registry package, which the
merge lane imports, so a mark that wrote the lane base would need the registry to call back into
the lane. The reading of the branch names the target by construction and is what the check uses.

## A message about a turn is not the turn moving

`merge-turn-request-return` records somebody asking, and queues a notice to the holder through the
delivery engine, which wakes the holder's thread when it is idle. The notice is the grant channel's
(the same delivery kind, recipient and address, so a turn that names no usable assignment is refused
in the same way and journaled as `merge_turn_wake_unaddressed`) and is told apart by the kind inside
its receipt, `merge_turn_return_request`. The ledger entry and the notice are written in one
transaction: if the notice cannot be queued the transaction rolls back and no request is recorded.
A request with nowhere to go is recorded and answered as such. The answer carries `returnNotice`:
`queued` with the event id, `unaddressed` with the reason, or `not_sent` for a turn that holds
nothing. `queued` means the notice is in the delivery queue; the delivery engine sends it and wakes an
idle holder under its usual pacing and holds, so it is not a receipt. One requester asking again
about the same turn keeps the original request and its text and reuses the notice already queued; if
none was queued because the turn then had no address, the replay queues one once an address exists.
The notice reads as current while the turn occupies the target and as `merge_turn_closed` once it
does not; an acknowledgement of the grant does not answer it. `merge-turn-attest` with
`--evidence-kind transport_accepted` records a delivery layer accepting that message. Neither
changes state. Only the holder's own `merge-turn-release` returns the turn (a silent holding turn can
also be passed on, below), and
`merge-turn-show` reports `returnRequestedAt`, `transportAcceptedAt` and `releasedAt` as three
separate facts. The same rule from the other side is that a parent asserting its turn in
conversation changes nothing: only a write by the registered project parent does.

## A restated head is a new candidate

A holder that refreshes its pull request branch inside its turn has a new head, and the turn still names the old
one. `merge-turn-ready --turn <id> --actor <holder> --head <new> --not-ready` states it, and two rules follow from
the head being a different commit. Both are deliberate: they were written with the lane and kept when it was ported.

- **Readiness is reset.** Readiness is a statement about one head, and it is what lets `merge-turn-check` run, so a head
  that changed cannot inherit it. Without the reset a holder could restate its head and then pass the currency check
  unchallenged, which is the check's whole purpose. A `--ready` given in the same call is therefore accepted and not
  recorded; when the readiness was already off, nothing in the ledger shows that it was asked for, so the answer is where
  it is said (below). The relay does not read CI when readiness is declared: it reads the checks the holder restates at
  `merge-turn-check`. "Declare readiness once the new head's checks have finished" is the order a holder keeps, not a
  condition the relay tests. A claim differs: `merge-turn-request --head <h> --ready` records readiness, because a claim
  has no earlier statement to reset.
- **A holding turn gets its own grant.** The grant it holds names the old head, so the restated candidate gets a new one
  (`grantedFrom` `candidate_restated`) that has to be acknowledged before the check, as any grant does: the holder
  says it re-read the record rather than acting on what it remembered. Without it a holder that had not answered the old
  grant could never answer it, and one that had could merge a candidate it acknowledged nothing about. This grant wakes
  nobody. The answer to the restating call names it, and `merge-turn-show` reads it later. A waiting claim holds no grant;
  it is granted one when it takes the target.

The answer to the restating call carries `readinessReset` whenever the head changed: `previousHead`, `candidateHead`,
`readyRequested` (what the call asked for), `grantId` (the grant the holder now owes, null for a waiting claim) and
`detail`, which names the next steps and says so when a `--ready` was dropped. A call that leaves the head as it is
carries none. The order, with the owner's binding active (`merge-turn-acknowledge` refuses a paused one, so resume it first):

1. refresh the branch and read its new head;
2. `merge-turn-ready --turn <id> --actor <holder> --head <new> --not-ready`; on a holding turn the answer's `grant` is the new grant;
3. `merge-turn-acknowledge --turn <id> --actor <holder> --grant <grantId> --evidence <what was read>`;
4. wait, inside the turn, for the new head's checks to finish (`merge-turn-progress` keeps the turn alive);
5. `merge-turn-ready --turn <id> --actor <holder> --head <new> --ready` (the head is the turn's own now: recorded, no new grant);
6. `merge-turn-check`, which states the new head, then the merge and `merge-turn-land`.

The acknowledgement (step 3) may also come after step 5; readiness is declared only once the new head's checks have finished. `merge-turn-check` refuses a missing step with its reason unchanged and the next step in its detail: an
undeclared head is `merge_candidate_moved` (declare it with `merge-turn-ready ... --head <h> --ready`), an unanswered grant is
`merge_turn_not_held` (acknowledge it, naming the grant), and a check that restates a head other than the one the turn holds is
`merge_candidate_moved` (restate the one the turn holds, or declare the head you mean with `--not-ready`).

## Progress, the holding limit and passing a silent turn

A holder works inside its turn for as long as a refresh, CI and a merge take, and nothing used to
show whether it was still there: a holder whose session died kept the target occupied until
somebody noticed (23 minutes, with two pull requests waiting). Three things change that.

**The holder records progress.** `merge-turn-progress --turn <id> --actor <holder> --step <step>
[--evidence <what>]` records one step: `base_refresh` after refreshing the candidate,
`ci_started` when its checks start, `ci_polled` while they run, `ci_result` when they finish,
`merge_attempt` when the merge is requested. Only the holder records, only while the turn is
holding or merging, and under the checks `merge-turn-acknowledge` makes (the owning binding is
current and not paused). A record is a ledger entry (`progress_recorded`, key `progress:<n>`).
The turn's clock starts at the grant. After that the newest of these refreshes it, each written by
the holder itself: a progress record, its acknowledgement of the grant, a restated head or
readiness, and a successful currency check (`merge-turn-check`). A tie is decided by rank (a
progress record, then the other things the holder does, then the grant) and then by sequence. A
stored time that cannot be read, or a clock behind the last sign, is no evidence of silence.

**`merge-turn-show` shows it.** The target answer, when a turn occupies the target, carries
`lastProgressAt`, `lastProgress` (`evidenceKind`, `step`, `sequence`, `recordedAt`),
`holdingLimitSeconds`, `stallsAt` and `stalled`; the answer for a single turn does not (a turn's
progress records are in its ledger), and `merge-turn-progress` answers with its own `progress` object. A stalled holding turn reads
`blocked.cause` `holder_stalled` unless the holder no longer owns its project or its binding is
paused, which are read first; a stalled merging turn keeps `merge_in_flight`. `stalled` is true in
both.

**The holding limit is 1200 seconds**: the longest hosted CI job may run 900 seconds (the largest
`timeout-minutes` in `.github/workflows/ci.yml`; a test fails when that changes) plus a 300 second
margin. It measures silence between records, not a CI budget: a holder that records `ci_polled`
while CI runs is not silent. It is a constant of the lane, shown in the target reading and copied
into every pass record and return notice, not a store column.

A turn that is still **holding** and stalled can be passed:
`merge-turn-pass --turn <id> --actor <passer> --evidence <what was observed>`. The passer is the
supervisor above the holder's project or the parent of a live waiting claim on the same target
that still owns its project. Inside one transaction the relay re-reads the ledger, refuses unless
the holder has been silent for the limit (`merge_turn_not_held`, naming when the turn would
stall), writes a `turn_passed` entry that keeps the original values (holder, candidate head,
`heldAt`, the last sign of life and its step, the limit, the seconds of silence, the passer and
the evidence), closes the turn as `passed` with that text as its close reason, and promotes the
next ready waiter in the ordinary order with its grant and wake. The turn keeps its holder,
candidate head, `heldAt` and every ledger entry; only its closure (state, close reason and time) is
written. A merging or unknown turn is never passed (`merge_turn_unresolved`): its holder may
already have merged, and elapsed time is not an observation. A merging turn leaves through
`merge-turn-land`, or `merge-turn-unknown` and then `merge-turn-resolve`; an unknown turn goes
straight to `merge-turn-resolve`. A holder that follows the protocol runs
`merge-turn-check` before it merges, which moves the turn to merging; that is why only a holding
turn can be passed. A pass is a statement about the relay's record, not proof that the pull
request is unmerged: a holder that merged without running `merge-turn-check`, against the protocol,
leaves a merged pull request behind a holding turn that can be passed, so a passer reads the pull
request before relying on the candidate being unmerged. A holder that ran the check and died stays
merging, and a merging turn is never passed.

After a pass the original holder is refused `merge_turn_not_held` on every operation that acts on
that turn (land, release, readiness, check, acknowledge, progress, withdraw), with a detail that
says the turn was passed, when, by whom and after how long, and that it claims the target again with
`merge-turn-request` if the candidate is still wanted; reading the turn and asking about it still
answer. Each refusal is recorded as a contest on the target; the contest is one row per refused task
and state, so `merge-turn-show` keeps the newest attempt's detail. A pass can also reach a holder
that is only slow to wake: a grant whose notice waits in delivery longer than the limit reads as
silent before the holder has seen it, and the holder finds itself refused when it wakes. That costs
a new claim, not a wrong merge.

## Counts and ceilings are different kinds of fact

`runs` is the one dimension this store counts for itself, always with `COUNT(*)` over held
rows and never a stored counter, so there is nothing to decrement twice and nothing to leak
when a process dies between a decrement and its journal row.

Every other dimension - file descriptors, model spend - is a fact about a host or an account
that no number of rows here observes. Such a dimension takes its value only from
`usage-observe`, and with none recorded `capacity-show` answers `proof: unmeasured` rather
than zero. An enforced bound nobody measured refuses `capacity_unmeasured`, which is a
different answer from `capacity_exhausted` on purpose: one says the bound is unknown and the
other says it is reached.

A ceiling lowered below current use is accepted, revokes nothing, reports `overBy` and refuses
the next reservation. This package cannot stop a running child, so reclaiming capacity would be
a claim the record cannot support.

## Regions, not file names

A region is a repository, a base revision, a path and optionally a symbol or data key. Two
parents with business in different parts of one file are not in conflict, and a `tree` claim on
the repository root is refused outright. Containment is by path component, so `src/a` does not
contain `src/ab`. Two symbol regions overlap only when the path AND the key match, so one
function name appearing in two files is two regions.

`region_class=generated` never contests. Two parents both touching a file the integration tree
re-derives are not disagreeing about it; they are both going to regenerate it, and
`region-show` lists such regions under `regenerate` with the command they come from. This
pull request is itself in that situation with `scripts/crw_runtime/components.json`.

## A base move reaches an agreement only when a party records it

A region carries the revision it was proposed on, so an agreement is about a place in one tree.
Nothing here watches a branch, and `merge-turn-land` records no revision mark: a mark is append-only
with one successor per revision, while a landing's recorded base can still be corrected with
`merge-turn-restate-base` (see above), so a mark written from a wrong reading could never be taken
back. A registered parent of any project with an agreement in the repository, open or closed (the
check reads that history, not the agreement being moved; a repository with no agreement at all takes
any registered parent), records the move with `region-restate-revision` once it has read the landed
base, and before it answers or relies on an agreement standing on the older tree. Until somebody
does, a late acceptance on the older tree stands; that is the contract, not an oversight.

One revision has one successor, so only the first move starts at the proposal's revision. Every
later move starts at the end of the recorded chain, which `region-show` reports per agreement as
`currentRevision` and which a refused restatement names with the command to run. A move the chain
already holds answers `alreadyRecorded` and writes no mark; like any restatement it is journalled
and reopens an agreement proposed on the older revision since. A move from a revision no live
agreement stands on and no recorded move reaches is refused, naming where the chains end, because
it would start a chain no agreement follows; a closed agreement stands nowhere, and a repository
with neither a live agreement nor a mark still takes its first one.

After a move, settling an agreement on the older revision is refused as `agreement_revision_stale`,
naming the chain's end, and either side carries it there with `region-reaffirm` (CRW-237). The
successor keeps the original proposer, the constraint and both sides' conditions as written; the
carrying side may restate only its own condition. Carrying accepts the carrier's side on the new
tree. The other side's acceptance was given on the older tree and is not carried: the answer's
`reaffirmation.awaitingAcceptance` names that side's parent, the reason
(`acceptance_on_prior_revision` or `not_yet_accepted`) and the `region-settle` command, and
`nextOwner` names that parent as it stands when read - or, with no single registered parent on that
side, is left empty while the answer says what has to happen first. `statedOn` names the revision
each text was written against and `textFromEarlierRevision` lists those from an older tree, because
a line number inside one points there. A successor carried before carries were recorded has no
`edit_reaffirmations` row; its constraint was copied from what it supersedes, so the revision is
found by following `supersedes` back while the text is unchanged. That carry also made its caller
the proposer and dropped both conditions; `legacyCarry` names the agreement it came from with those
terms, and the next reaffirmation carries the terms from there. Two shapes that older code left
behind are shown but not repaired here: a carry interrupted after it released its predecessor and
before it wrote a successor, and an older successor still waiting for an acceptance, which can be
accepted with the terms it lost. Both predate this change and are left to a follow-up. The first
version made the carrier the proposer, dropped both conditions and cleared the other acceptance
without a word (CRW-124 G3).

An acceptance takes no condition. One given to `region-settle` used to vanish; it is now refused,
as `bad_invocation` at the command line.

## A review thread that arrives after the child's record

A child's handoff record lists the review threads it saw in `reviewCoverage.threadsSeen` and judges
each one. A review that lands afterwards on the same head is not in that list.
`merge-evidence --restate <record>` reads the pull request again and reports every such thread as
a `late_finding`, resolved or not, because the record did not see it and no longer describes the
candidate. The record then went back to the child, which could only answer with a receipt that
named the thread.

When the coordinator can judge a late thread itself it records that judgement in a file and passes
the file to the same command:

    codex-session-relay merge-evidence --repository <owner/name> --pull-request <N> \
      --restate <record.json> --late-dispositions <dispositions.json>

The file is a JSON object whose `lateDispositions` member lists one entry per thread:

    {"lateDispositions": [
      {"threadId": "PRRT_kwDOExample", "disposition": "backlog",
       "evidenceUrl": "https://github.com/<owner>/<name>/issues/123",
       "head": "<the head the record is about>", "grade": "P2"}]}

Every member is a string and none may be blank. `threadId` is the thread's identifier as the
reading's `findings` and a record's `threadsSeen` give it. `disposition` is `answered` (replied on
the thread, no change needed), `backlog` (deferred; the evidence is the follow-up), `resolved` (the
concern is already gone; the evidence shows it) or `refuted` (the finding does not apply; the evidence
is the code that shows it). `evidenceUrl` is an http or https URL a reader can open. `head` is the full
commit sha. `grade` is whatever grade the coordinator gave, such as `P2` or `yellow`: the relay
records it as written and never reads it. Values are kept and matched exactly as written, and a
duplicate key keeps its last value.

These words are the coordinator's. They cover threads outside `threadsSeen` and never stand in for
the child's own `threadDispositions`, whose vocabulary differs (`backlog` is roughly the child's
`accepted`, `refuted` its `disputed`). Only review threads can be disposed of; a review summary or
a pull request comment is never exempted.

- **What an entry does.** A thread leaves `late_finding` when the file holds an entry for it whose
  `head` is exactly the head the record is about (`--restate-head` when given) and the record's
  `threadsSeen` does not list the thread. An entry for another head has no effect, and neither
  does one for a thread the record already lists or one the reading does not show.
- **It is not a resolve.** The fresh reading still grades the forge's own state, so an unresolved
  thread keeps `review_incomplete` in the reading and the restatement whatever is recorded. The
  coordinator resolves the thread on the forge, then records the judgement.
- **A malformed document is refused whole.** In a file that is a JSON object, a missing or non-list
  `lateDispositions`, a non-object entry, a missing, non-string or blank member, a disposition
  outside the set, an evidence URL that is not http or https, a head that is not a sha, or one
  thread twice on one head is `malformed_evidence` and no entry takes effect. A file that cannot be
  read, is not JSON or is not a JSON object, and `--late-dispositions` without `--restate`, are
  usage errors (exit 4) before anything is read from the forge or graded. An empty value counts as
  no flag.
- **What the payload says.** With the flag, `restatement.lateDispositions` lists the entries in file
  order, each with its five members and `effect` `applied` or `ignored`. An ignored entry carries a
  `reason`: `other_head`, else `unknown_thread` (no review thread of that id), else `not_late` (the
  record lists it). The list is empty when the document was rejected or the record could not be
  graded. Without the flag the payload is exactly what it was. No problem code, refusal reason or
  exit code was added.
- **What the relay does not decide.** Whether a thread is minor enough to judge here is the
  coordinator's decision. A P0, P1 or security finding goes back to the child as before, and
  nothing in the relay stops an entry that records another grade. The file is unauthenticated,
  like `--required` and `--actor`; the payload carries each entry so the merge record keeps the
  grade and the evidence that were given.

## What this is not
- **`reaffirm` spans two transactions, and that is a choice with a stated reason.** The first
  validates and records its refusals - ownership, an open agreement, a revision that actually
  moved, the end of the recorded chain, a successor place free of another pair's overlap and of
  this pair's own live agreement - and retires nothing. The second is the ordinary `propose`
  path carrying the predecessor: in its one transaction it decides all of that again, checks that
  the destination is the predecessor's own place, pair and link, retires the predecessor, inserts
  the successor with the terms read from the row it retires, records what was carried in
  `edit_reaffirmations` and links the two. Reusing `propose` keeps its shape, overlap, peer and
  classification rules in one copy. The earlier three-transaction order retired first and
  inserted later, so a base move or a same-pair proposal landing in between left a successor on a
  superseded tree or nothing live at all; now an interruption or a lost race leaves the
  predecessor live and the refusal recorded.
- **A review record states every field or none of it counts.** `hasNextPage`, `pagesRead`,
  `totalCount`, `threadsSeen` and `unresolved` must all be present before any is read.
  Absent used to read as satisfied at every one of them, so a record saying nothing passed
  the whole check.
  Each also has its type: `hasNextPage` true or false, the two counts and `unresolved` whole
  numbers, `threadsSeen` a list of thread identifier strings. `merge-turn-check` asks that
  first (CRW-232) and refuses a wrong one as `merge_evidence_malformed` before it reads the
  turn or the target, recording nothing, because a count given as `threadsSeen` raised as a
  host fault and a string `threadsSeen` or a list `unresolved` passed.
- **Every identity here is asserted, not authenticated.** `--actor`, `--task` and the forge
  evidence passed to `merge-turn-check` are all caller-supplied, and this package has no
  transport authentication to check them against. What the ownership rules buy is that a
  caller must name an identity the store already records as owning the scope, and that every
  refusal is retained with both parties - not that the caller is who it says. That is the
  same trust boundary `linkage-bind --task` and `assignment-mark --actor` already sit on, and
  closing it needs an authenticated channel, which is a different piece of work. Anything
  able to invoke the CLI is already inside the boundary.
- **Forge evidence is cross-checked, never observed.** `merge-turn-check` verifies that what
  the caller restated is internally consistent and current against what this store knows. It
  cannot see the pull request; what it reads from the target is where the base branch points
  and, only when the last landing's recorded base is older than that tip, how the branch got
  there (see above). An operator who wants proof that required CI was green reads
  the forge, not this record.

- **An agreement confers nothing.** Not merge permission, not authority to instruct, not a
  wider artifact scope. `mergeturn.py` never reads `edit_agreements`, and a test runs the same
  claim sequence on two stores differing only by an agreed overlapping region and compares the
  answers. There is deliberately no check comparing a region path against an assignment's
  artifact roots: region paths are repository-relative, `artifact_roots` are absolute host
  paths, and nothing here records where a repository is checked out. A check that cannot be
  performed correctly but reads like enforcement is worse than its absence.
- **An unknown target stays blocked until somebody observes it.** Nothing here watches a forge
  on its own; a base branch is read only when a command records a base. So a coordination loop with no recovery step will wedge a target after a holder dies. That is
  the correct failure for the criterion and a follow-up for whoever owns that loop, not a
  defect repaired here.
- **Region disjointness is declarative.** The relay cannot parse code, so two names for one
  function are two regions.
- **A total is per store.** `resolve_state_dir` selects a store per socket, so two sockets are
  two totals and neither is a host-wide number.
- **Required checks are restated, not discovered.** `merge-turn-check --required` is the
  caller's declaration of what branch protection requires, stored as `requiredDeclared`. The
  merge turn never reads a forge's rules or checks; it reads where the base branch points and, in the one case described under "A merge outside the lane", how it moved. What the check establishes is that the restated evidence is
  internally consistent and current: every declared required name present and successful on the
  candidate head at its highest submitted attempt, the base matching the last landing recorded
  here (or restated by the check itself after a confirmed merge outside the lane), and the
  review paginated to the end with nothing unresolved. When the turn names an
  assignment, the head must also be the one its current work reports name: the latest
  head-bearing submission of every event in the newest generation that names a head, read per
  event because submission numbers count per event. Two different current heads are refused as
  `revision_ambiguous` whichever event was resubmitted more often; one current head that is not
  the candidate is refused as `merge_candidate_moved`.
  The merge-turn grant notice fills that flag in for its recipient, because the command it hands
  over is one a parent runs as written, and omitting `--required` declares that nothing is
  required. The names come from the candidate's own recorded handoff: the `requiredDeclared`
  that `merge-evidence` read from the branch's effective rules, taken from the same current
  reports `merge-turn-check` compares the head against (`report.current_reports` is the one
  selection both read). They must all name the granted head, target and base and agree, and the
  notice names the report it quotes. They are a proposal the caller restates, not a discovery:
  the check stores whatever `--required` its caller passes. Where no such reading is recorded,
  the notice leaves a placeholder and names the `merge-evidence` reading to take instead.
- **A release restating a different reason is refused.** The first reason wins and a later
  notification restates it. Completion, failure, a resume and a duplicated notification all
  arrive as a release of one subject, so the second must not be a second release.
- **A green suite is not an installed runtime.** The evidence for everything above is
  `tests/test_merge_turn.py`, `tests/test_capacity.py`, `tests/test_edit_regions.py`,
  `tests/test_coordination_contract.py` and `tests/test_coordination_cli.py`. Whether an
  installed relay on a real host records any of this is separate evidence.
