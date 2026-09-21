# Contention and failure-recovery regression map

What each required scenario is proven by, which proof already existed, and which one
this work added. A row that reuses earlier evidence names it so nobody re-derives it;
a row that adds evidence names the new test. Clock provenance is a column because an
injected clock reproduces arithmetic and ordering and never elapsed time, and because
some of the reused evidence is real-time.

`tests/test_regression_map.py` checks this file against the suite, and the list of what
it checks is itself a claim that can go stale, so it is kept current here. Every class
named below must exist in the module it is attributed to; every module must exist; the
clock column must agree with which modules actually wait on something real; every number
this file prints, the per-module case counts and the sweep's own counts, is read back out
of it and compared against what the suite produces. That module also derives two
inventories the prose used to assert: which booleans the suite measures with, and where it
injects store faults and what interval each one reaches.

## What the two named landings actually changed

The issue expected `ff3c69b5` (management markers) and `6f77ab4` (the
completion-receipt hook) to have moved this package. They did not.
`git show --stat ff3c69b5` is six files under `skills/`;
`git show --stat 6f77ab4` is `scripts/completion_hook.py`,
`scripts/crw_runtime/completion.py`, `scripts/runtime_install.py`,
`docs/runtime-install.md` and two files under `scripts/ci/tests/`.
`git log ff3c69b5^1..6f77ab4 -- packages/codex-session-relay` is empty.

The contention they introduce is therefore not a diff here. It is a diff in what now
calls this package: a coordinator that publishes marker facts and can stop between any
two of them, and a Stop hook that runs `guard-evaluate` as a separate process
against a store the daemon is writing to. Those two callers are what the new tests
reproduce.

## Criterion map

| # | Scenario | Reused evidence | New evidence | Clock |
|---|---|---|---|---|
| 1 | Registration aborted before and after it completes, a lost creation response, duplicate and late binding, wrong owner or generation | `test_intent.py` DerivedState, Binding, Registration; `test_guard.py` UnmanagedAndUnclaimed, Declarations; `test_registry.py` Generations; `test_assignment.py` DuplicateAssignment | `test_registration_contention.py` | injected |
| 2 | Daemon exit and restart, and connection loss, around emit, delivery and acknowledgement, recovered from the persisted waiting records | `test_ack_reconcile.py` RestartRecovery; `test_delivery.py` RestartPreservation; `test_enqueue_durability.py` EnqueueDurability; `test_daemon.py` ReconcileGate; `test_wp1_regressions.py` TransactionRecovery; `test_ack_reconcile.py` VerdictAtomicity | `test_failure_recovery.py` | mixed |
| 3 | Guard timeout, guard error, and the block limit reached, with no infinite repetition and work processed after recovery | `test_guard.py` Bounds, FailureSeparation; `test_guard_property.py` FailureIsNotANormalState | `test_failure_recovery.py` | mixed |
| 4 | needs_changes continuing into the next generation of the same accountable task, with duplicate, out-of-order and late events causing no additional execution | `test_anchor_binding.py` AnchorBinding; `test_supersession.py` PreSendSupersession; `test_receipts.py` GenerationAndScopeRefusals; `test_rereview_deadlock.py` ReReviewIsReachable | none; met by reuse | injected |
| 5 | Two parents from different repositories and Linear projects on one shared store: acknowledgement and revision driven concurrently, completion intake and outbox claim checked sequentially | `test_delivery.py` CrossAssignmentDelivery; `test_fairness.py` SharedChildTurns; `test_sync_outbox.py` Readback, ClaimFencing | `test_multi_parent_isolation.py` | injected |
| 6 | One parent busy, failing or at its retry limit, and one paused, cancelled or archived, while the other keeps progressing | `test_fairness.py` DeliveryFairness; `test_delivery.py` Bounds, HostLifecycle | `test_multi_parent_isolation.py` | injected |
| 7 | Daemon run-limit exit and restart, duplicate startup on the same and on a different store, and an assignment outliving four hours | `test_daemon.py` Bounds, Instance; `test_service.py` Ownership, FourHourBoundary; `test_cli.py` ContestedSocket | `test_operational_scale.py` | mixed |
| 8 | A declared parent and child count and event volume, measured for queue depth, ticks to drain and send ceilings | none; new ground | `test_operational_scale.py` | mixed |

Criterion 4 is the one row with no new test. `test_anchor_binding.py` already drives
needs_changes into the next generation through real entry points with no test-side
binding, `test_supersession.py` already proves a superseded event opens no generation
and makes no transport call, and `test_receipts.py` already refuses a stale
generation at intake. Adding a fourth version of that would be volume, not coverage.

Criterion 2's new evidence used to be narrower than its test name read, and the difference
was found in review rather than by the predicate above. The store's fault hook is global
and fires just before every COMMIT, so arming it and running a tick kills whichever write
the tick makes first. That was `_record_poll`'s poll observation during `_observe`, not
the attempt claim in `_deliver`, and the case named for attempt-transaction rollback
therefore asserted an empty `attempts` table that was empty by construction. Measured:
with `Store.transaction` changed to COMMIT instead of ROLLBACK on error, that case still
passed.

`ATickInterruptedInsideItsOwnTransaction` now holds two cases and each names the
transaction it kills. The first-write case asserts the interrupted transaction wrote the
poll observation and did not reach an attempt, so the account above is checked here rather
than traced by hand, and what it establishes is unchanged and still real: a tick
interrupted at its first write leaves the store consistent, and a different daemon over a
reopened store completes the handoff exactly once, which is the shape a killed worker has.
The second kills inside `delivery._claim`'s transaction and establishes what the first
cannot reach - the attempt row, its rendered bytes and its reserved send capacity all roll
back, no send escaped, the tick's earlier commits survive, and recovery is exactly once.
That transaction is identified rather than assumed: `attempts` has three writers, and
`attempt_messages` has one, inside `_claim`.

Rollback of a write transaction mid-body remains covered by reused evidence in
`test_wp1_regressions.py` TransactionRecovery and `test_ack_reconcile.py`
VerdictAtomicity, named in this map's criterion 2 row; the new case is not a third copy of
it, because it has to reach `_deliver` through a real tick and then recover across a
process boundary. Where the injections are and what each one reaches is no longer written
here. `tests/test_regression_map.py` derives it, for the same reason the sweep's reach
moved there: a sentence in this file saying how much was checked is the thing that keeps
being wrong.

Criterion 5 is narrower than the issue's wording and the row now says so. Two of the four
paths run from two threads: acknowledgement and `record_verdict`. Completion intake and
the outbox claim and completion run sequentially, and the reused classes beside them are
sequential too. What the concurrent pair establishes is that two parents settling at once
keep their own event, relationship, generation and recipient; what the sequential pair
establishes is that a job cannot be completed against another project's document or under
another job's claim token. Neither is a contended outbox, and calling it one would be the
overstatement this map exists to avoid.

## What has landed

| Module | Cases | Covers |
|---|---|---|
| `test_registration_contention.py` | 4 | 1 |
| `test_failure_recovery.py` | 14 | 2, 3 |
| `test_multi_parent_isolation.py` | 8 | 5, 6 |
| `test_operational_scale.py` | 8 | 7, 8 |
| `test_regression_map.py` | 29 | 9, the sweep's own reach, and where the faults are injected |

## Clock provenance, derived rather than asserted

The rule is deliberately coarse and deliberately per module: a module spends real time if
it reads a real clock or starts a process, and every criterion naming such a module is
*mixed* rather than *injected*, even when most of its own cases move an injected clock.
A criterion does not get to claim the half it prefers. The inventory is derived by an
`ast` scan in `tests/test_regression_map.py` and compared against the tuple
declared there, so a new test that waits on anything real fails until it is named.

Where the boundary sits, since it is a judgement and not an accident: a barrier, a thread
join and a lock acquisition all block, but they block until another thread arrives rather
than until a duration passes. They synchronise without measuring, so a module that only uses
them stays *injected*. Several of the new tests do exactly that. A test that asserted how
long one of those waits took would have to read a clock, and the scan would catch it.

What each one waits on:

- `test_service.py` starts real subprocesses and polls `time.monotonic` until one
  holds the lock, and runs one real worker for a single short segment outside the
  scripted clock.
- `test_worker_policy.py` starts real policy publishers holding service locks and
  waits on their publication pipe. It stops and replaces those processes to test
  whether a retained policy receipt still names the current worker.
- `test_operational_scale.py` spawns one real replacement worker.
- `test_cli.py`, `test_management_cli.py` and `test_wp1_regressions.py` shell
  out to the command line.
- `test_bridge_adapter.py` waits before asserting a transport worker is still alive.
- `test_dispositions.py` shells out to the command line, because the exit-code rule it pins is
  the part a polling coordinator reads first: an unreadable store must not exit 0, and only a
  real process exit can show that. The same runs prove the command leaves no database behind.
- `test_stop_adapter.py` runs the Stop adapter's console entry point as a real process, because
  exit 2 is the host's blocking code and only a real exit status can show that it never returns one.
- `test_daemon_cadence.py` measures a real run spending its deadline polling.
- `test_failure_recovery.py` waits a real SQLite busy timeout, because a timeout is
  the one thing an injected clock cannot produce. It is why criteria 2 and 3 are both
  mixed: they share that module, and one of its cases waits.

`FourHourBoundary` in `tests/test_service.py` drives the supervisor past four
hours of scripted monotonic time and asserts the store identity, the generations, the
launch count, the segment outcomes, one inherited lock descriptor, one scope descriptor,
one token and an absent socket. Its docstring records what that cannot establish, and the
new work inherits the limit: nothing there observes four real hours, so nothing there
speaks to process memory, write-ahead-log growth, descriptor or socket drift, or a host
that answers differently after hours of uptime. A `FakeWorker` exits in microseconds
and never opens the store, so the crossing shows the supervisor preserving records across
replacements.

Whether a replacement worker reads those records back is a separate question, and this row
used to answer it with one word for three different tables. `ARealWorkerReadsTheHandoffBack`
in `test_operational_scale.py` settles two of them in a real process against a socket that
does not exist: the relationship, which it names in a tick note and writes a poll observation
for, and an acknowledgement left at `unverified_turn`, which `ack.verify_pending_acks` is
the only reader of in a tick - it names that event and records why it could not promote it,
and the `ack_evidence` row is asserted as a CHANGE because `acknowledge()` already wrote
one when the intent was authored.

The owed coordination write is not among them. No tick pass reads `sync_outbox`;
`SyncOutbox.next` and `claim` are reached through their own commands and nothing in that
module runs one. So the crossing shows the supervisor carrying that row, a real worker reads
back the other two, and nobody here reads back the outbox job. That is the whole claim now.

## Scale, stated as a bound rather than a guarantee

Criterion 8 declares its parent count and event volume as module constants, so a report
quotes a number read from source. The suite records the queue depth, the ticks needed to
drain it and the per-tick send ceiling that produced it, and the ceiling is derived from
`RetryPolicy` rather than written down. That is a measurement of this harness on one
machine with a clock that never sleeps. It is evidence that selection stays bounded and
fair at that size. It is not a throughput figure, it says nothing about a host under real
load, and passing at the declared size must not be reported as support for unbounded
parallel operation.

## Baseline

`CRW_PACKAGES_TMPDIR=/var/tmp python3 scripts/ci/packages.py` is the authoritative
run and passed before any change here: codex-session-relay 1206 tests, no empty
collection and no skipped case. A bare `PYTHONPATH=src python3 -m pytest tests` is not
that run and is not the baseline: it has no `codex_thread_bridge` on the path, and
this filesystem does not honour the unreadable directory one of the scope tests depends
on, so it reports failures that belong to the runner.

## Two things every test here was swept for, and how far the sweep actually reaches

Three review rounds found the same shape three times: an assertion that passes in the
situation its own name describes, and prose claiming more than the assertion establishes.
Fixing the instances a fourth time would miss the point, so the suite was checked against two
rules derived from source rather than from memory.

It did not work, and the way it failed is the reason this section is now written differently.
The sweep said five modules had been checked, and two more instances of the same shape arrived
after this work merged - one of them, `test_multi_parent_isolation.py`'s isolation case, exactly
the kind the predicate below names. That was the third time in this project a sweep's stated
reach was wider than the reach behind it: `ca9de94`'s sweep missed the `drain()` case, that
map's sweep clause was an overstatement and corrected itself in `db6b132e`, and then these.

A sentence saying how much was examined is the thing that keeps being wrong. So the reach is no
longer written here. It is produced by `tests/test_regression_map.py`, and the numbers below
are read back from that suite rather than asserted by this file.

**An assertion must go red when the condition it is named for is violated, and the
predicate for checking that is written here so the next reader knows what was examined.**
A first pass read assertion SHAPE - which comparisons could not fail, which loops could
examine nothing - and that was too narrow: it caught three vacuous spots and missed a
fourth, because the quantity under comparison was computed somewhere else. The predicate
this file now stands on is stronger: *does the quantity an assertion measures mean the same
thing as the condition written above it, following into the helper that computes that
quantity*. Anywhere a helper narrows a state set, an id set or a count and the assertion
only sees the result, the two can disagree silently.

Re-derived under that predicate by hand, five helpers across the five modules narrow something -
by SQL filter, by comprehension filter, or by path glob. Four of them fail safe, and the
reason is worth stating because it is what makes them acceptable rather than lucky: if
`hold_files` or `observations` stopped matching the paths the source writes, they
would return nothing and their exact-count assertions would fail; if `classes_in`
parsed nothing, the citation assertion would fail; and the multi-parent `drain`
measures arrival as `DISPATCHED` directly, which is the condition its callers name.

The fifth did not. `TheDeclaredLoad.drain` counted the backlog as
`state = 'queued'` while `delivery.CLAIMABLE` is
`(queued, deferred_busy, withheld_pre_send)`, so an event withheld before sending left
the measurement and the load read as drained with work still owed. Measured directly:
staging one withheld event gives a queued-only count of 71 against a claimable count of 72,
so that event was invisible to the old predicate. It now counts every claimable state, and
the drain test separately asserts that every loaded event id reached `DISPATCHED` -
named apart from the backlog because a count reaching zero is a statement about what is
still claimable, and arrival is a different statement.

### The part of that predicate the suite now derives

One quantity in it is mechanical: a boolean the source folds out of several inputs, handed to a
caller as one value. `TickReport.quiet` is `not (observed or reconciled or delivered or
deferred or acksVerified or anchorsBound or requeued)`, so `assertFalse(report.quiet)` is
satisfied by any one of seven counters moving and names none of them, while `assertTrue` pins
all seven. Which side is cheap is read off the expression rather than judged, and a place that
asserts the cheap side is a place to read again.

`TheSweepDerivesItsOwnReachRatherThanClaimingIt` produces that reach. Every boolean this
package declares is partitioned into one of three lists and the suite checks the partition is
total: the ones whose fold reduces to a cheap side, the ones that fold where the rule cannot
weigh them, and the ones that reach their value down a single path. Every place the suite
measures something with either folded kind is listed, keyed by the assertion's own text, with
a verdict written beside it. Today that reads 53 booleans as 12 / 31 / 10, and 71 measured
places. Those counts, and the per-module case counts in the landed table above, are read back
out of this file and compared against the suite, so a number here that went stale fails there.

Its first run produced two findings, which is the answer to whether it is bookkeeping.
It named the isolation case this work was opened for, and it named a second one review
had not: `test_wp1_regressions.py` asserted an interrupted claim was suppressed using
only `deliverable()`, which is equally false for a claim that was never recorded, and
alone among its four siblings it asserted no stage beside it. Both are closed here.

What the derivation does NOT do, said plainly because the alternative is the overstatement this
section exists to end: it does not decide whether a place is a proxy. It supplies the reach and
the polarity; the verdict beside each site is a reading judgement a person wrote. Its blind
spots are declared as data rather than described - a read it cannot attribute a value to, an
occurrence in a write context that is not a producer form it knows, a producer it cannot
reduce - and each list is checked, so a blind spot that grows fails the suite.

It also carries stated boundaries. This package only; `bool`-annotated fields and
`bool`-returning functions only; matching by name; no following through an alias, a dict key
or `**kwargs`; and the fold-free list is declared by key alone, so it can hide one constant
producer being swapped for another. It cannot hide a fold, because a symbol that starts folding
moves lists and breaks the partition.

Two things stay outside all of it: whether a test asserts something another test already
asserts, and whether the condition a test names is the condition worth naming. Both are reading
judgements. The reuse column records the first; the site verdicts now record the second for the
places this derivation reaches, and nothing records it anywhere else.

**A test that depends on a configuration production cannot reach must say so.** This
repository configures write-ahead logging and writes with `BEGIN IMMEDIATE`; nothing in
it sets any other locking mode. The exclusive-lock case in `test_failure_recovery.py` is
the only user of `PRAGMA locking_mode=EXCLUSIVE` anywhere here, so it does not reproduce
hook-versus-daemon contention and no longer claims to. It exists because it is the only way
to reach the bounded-timeout path at all, and what it establishes is the guard's behaviour
when a read cannot complete. The ordinary-writer case beside it is the real contention, and
its measured answer is that the reader is not blocked - which is why criterion 3's evidence
is that pair together rather than the timeout alone.

## Findings owned elsewhere, reported rather than fixed

- The two-literal lock wait is CLOSED, and this passage described it as open until CRW-96
  landed. What it said: `guard.SQLITE_TIMEOUT` documented a bound that
  `intent.read_only_connection` actually set from its own literal, so the constant a reader
  found was not the one producing the measured 2.00-second wait. What is there now:
  `intent.SQLITE_TIMEOUT` sits directly above `read_only_connection` and is the only
  literal, `guard.py` declares no bound of its own and points at that one, and
  `read_only_connection` still takes no timeout parameter, deliberately, so
  `guard.lookup_receipt` and `intent.dispatch_generation_state` cannot be given different
  waits. The signature check in `test_failure_recovery.py` still pins the deciding place.
- `intent.register_relationship` reads the dispatch generation state and then publishes
  `relationship.json` as two operations with nothing held between them. An advance
  committing in that window returns success over a generation the store has already moved
  past, leaving the marker naming a stale one. Found by
  `test_registration_contention.py`, which therefore asserts what actually holds - the
  disagreement stays readable, so the next evaluation sees it - rather than asserting the
  absence of a race the source does not prevent. Closing the window needs the check and the
  publication under one hold, which is a source change this issue does not own.
- `scope.is_within` answers two different ways and this suite exercised one of them. The
  derived inventory lists `return: path.startswith('/')` as a producer path it cannot
  reduce, and that path is the whole answer when the root is `/`, which the five cases in
  `test_manifest_scope.py` never reached because they all use `/a/b`. A case now names
  that branch, positive and negative. What stays open is a scope question rather than a test
  one: whether an authorized root of `/` is reachable at all.
- The workflow-restore section is the only non-essential block in the revision direction
  of `report.render_revision`, so a tight budget removes it first. CRW-94 owns that
  behaviour; nothing here changes it.

## What none of this closes

Whether a new test asserts something an existing test already asserts. No scan can answer
that, and claiming otherwise would be the same kind of overstatement this map exists to
avoid. The reuse column is where that judgement is recorded, not where it is enforced.

`test_managed_start.py` uses subprocess CLI checks and one bounded worker thread to
verify the post-resume guard on its actual thread boundary. Its other scenarios
use a fake host and real SQLite/marker stores; they do not establish live hooks.

`test_reporting_cli.py` starts the real CLI in bounded subprocesses. Its
read-only and usage-exit evidence spends wall time; it does not contact a live
App Server or prove installed hook firing. `test_omitted.py` uses persisted
fixture Stop and settlement records with an injected observation time.
