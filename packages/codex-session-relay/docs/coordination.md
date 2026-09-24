# Coordination between parents

Relay-owned records, like the linkage tables and for the same reason: the five schemas under
`src/codex_session_relay/schema/` are byte copies of a frozen contract, none of them
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

## The base a landing leaves behind

A landing records the base branch it leaves behind, and the next candidate on that target has to
restate exactly that value before `merge-turn-check` lets it merge. That value used to be typed by
the caller. In the CRW-124 G1 trial on an installed relay both parents typed the base their
candidate had been checked against rather than the one the branch pointed at after the merge, so
their verified successors were refused `merge_currency_stale`, and nothing could correct a landed
turn: `merge-turn-resolve` admits only an unknown one.

So the merge turn reads one fact from the target itself: the commit its base branch points at. It
reads it at the four moments it records a base, and at no other time:

| Command | What it reads and records |
| --- | --- |
| `merge-turn-check` | The restated `--base-sha` must be the branch tip now; the reading, not the caller's text, is stored as the base the merge was checked against |
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
- `merge_base_mismatch`: the caller stated a base the branch does not read. Abbreviated and
  upper-case object names are compared as the commits they name.
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
names that landing, this command, and who may run it.

What this does not establish: `landedSha` is the holder's statement and is not checked for
containment in the branch, so a merge that failed while another write moved the branch is
recorded as landed, with the true base. On a resolved landing `landedSha` is the candidate head,
because no landing commit was reported. The comparison with the last recorded landing is kept
beside the reading on purpose: after an outside commit it stops a correct successor until the
landing's holder or supervisor restates the base, and that restatement is the record of why the
base moved outside the relay.

## A message about a turn is not the turn moving

`merge-turn-request-return` records somebody asking. `merge-turn-attest` with
`--evidence-kind transport_accepted` records a delivery layer accepting that message. Neither
changes state. Only the holder's own `merge-turn-release` returns the turn, and
`merge-turn-show` reports `returnRequestedAt`, `transportAcceptedAt` and `releasedAt` as three
separate facts. The same rule from the other side is that a parent asserting its turn in
conversation changes nothing: only a write by the registered project parent does.

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

## What this is not
- **`reaffirm` spans three transactions, and that is a choice with a stated reason.** It
  validates the chain and retires the predecessor in one, proposes the successor through the
  ordinary `propose` path in a second, and links them in a third. Folding them would mean
  duplicating the shape, overlap, peer and classification validation `propose` owns, and a
  second copy of those rules going stale is the failure this package keeps closing. The
  order is chosen so the reachable interruptions are a retired predecessor with no successor,
  or a successor not yet pointing back - both recoverable by proposing again. Neither leaves
  two live agreements on one carry-forward, which is the state the guard index cannot catch.
- **A review record states every field or none of it counts.** `hasNextPage`, `pagesRead`,
  `totalCount`, `threadsSeen` and `unresolved` must all be present before any is read.
  Absent used to read as satisfied at every one of them, so a record saying nothing passed
  the whole check.
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
  cannot see the pull request; the one thing it reads from the target is where the base branch
  points (see above). An operator who wants proof that required CI was green reads
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
  merge turn never reads a forge's rules or checks; it reads only where the base branch points. What the check establishes is that the restated evidence is
  internally consistent and current: every declared required name present and successful on the
  candidate head at its highest submitted attempt, the base matching the last landing recorded
  here, and the review paginated to the end with nothing unresolved. When the turn names an
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
