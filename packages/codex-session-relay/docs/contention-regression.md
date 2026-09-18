# Contention and failure-recovery regression map

What each required scenario is proven by, which proof already existed, and which one
this work added. A row that reuses earlier evidence names it so nobody re-derives it;
a row that adds evidence names the new test. Clock provenance is a column because an
injected clock reproduces arithmetic and ordering and never elapsed time, and because
some of the reused evidence is real-time.

`tests/test_regression_map.py` checks this file against the suite: every class named
below must exist in the module it is attributed to, every module must exist, and the
clock column must agree with which modules actually wait on something real.

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
| 2 | Daemon exit and restart, and connection loss, around emit, delivery and acknowledgement, recovered from the persisted waiting records | `test_ack_reconcile.py` RestartRecovery; `test_delivery.py` RestartPreservation; `test_enqueue_durability.py` EnqueueDurability; `test_daemon.py` ReconcileGate | `test_failure_recovery.py` | mixed |
| 3 | Guard timeout, guard error, and the block limit reached, with no infinite repetition and work processed after recovery | `test_guard.py` Bounds, FailureSeparation; `test_guard_property.py` FailureIsNotANormalState | `test_failure_recovery.py` | mixed |
| 4 | needs_changes continuing into the next generation of the same accountable task, with duplicate, out-of-order and late events causing no additional execution | `test_anchor_binding.py` AnchorBinding; `test_supersession.py` PreSendSupersession; `test_receipts.py` GenerationAndScopeRefusals; `test_rereview_deadlock.py` ReReviewIsReachable | none; met by reuse | injected |
| 5 | Two parents from different repositories and Linear projects on one shared store, under simultaneous completion, acknowledgement, revision and outbox activity | `test_delivery.py` CrossAssignmentDelivery; `test_fairness.py` SharedChildTurns; `test_sync_outbox.py` Readback, ClaimFencing | `test_multi_parent_isolation.py` | injected |
| 6 | One parent busy, failing or at its retry limit, and one paused, cancelled or archived, while the other keeps progressing | `test_fairness.py` DeliveryFairness; `test_delivery.py` Bounds, HostLifecycle | `test_multi_parent_isolation.py` | injected |
| 7 | Daemon run-limit exit and restart, duplicate startup on the same and on a different store, and an assignment outliving four hours | `test_daemon.py` Bounds, Instance; `test_service.py` Ownership, FourHourBoundary; `test_cli.py` ContestedSocket | `test_operational_scale.py` | mixed |
| 8 | A declared parent and child count and event volume, measured for queue depth, ticks to drain and send ceilings | none; new ground | `test_operational_scale.py` | mixed |

Criterion 4 is the one row with no new test. `test_anchor_binding.py` already drives
needs_changes into the next generation through real entry points with no test-side
binding, `test_supersession.py` already proves a superseded event opens no generation
and makes no transport call, and `test_receipts.py` already refuses a stale
generation at intake. Adding a fourth version of that would be volume, not coverage.

## What has landed

| Module | Cases | Covers |
|---|---|---|
| `test_registration_contention.py` | 4 | 1 |
| `test_failure_recovery.py` | 6 | 2, 3 |
| `test_multi_parent_isolation.py` | 7 | 5, 6 |
| `test_operational_scale.py` | 8 | 7, 8 |
| `test_regression_map.py` | 4 | 9 |

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
- `test_operational_scale.py` spawns one real replacement worker.
- `test_cli.py`, `test_management_cli.py` and `test_wp1_regressions.py` shell
  out to the command line.
- `test_bridge_adapter.py` waits before asserting a transport worker is still alive.
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
replacements. Whether a replacement worker reads them back is a separate question,
answered by a real process in `test_operational_scale.py` and reported separately.

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

## Findings owned elsewhere, reported rather than fixed

- `guard.SQLITE_TIMEOUT` documents the lock wait the readiness check may spend, and
  nothing passes it anywhere. Its name occurs once in the repository, its own definition;
  `intent.read_only_connection` carries a separate literal of the same value, and
  `guard.lookup_receipt` calls that function without a timeout at all. The wait is
  real and reachable - an exclusive writer makes the guard's read raise after a measured
  2.00 seconds - but the number producing it is the literal, not the constant, so changing
  the constant would change nothing. `test_failure_recovery.py` pins both halves.
  Wiring or removing it would also move `intent.dispatch_generation_state`, so it is
  reported rather than fixed here.
- The workflow-restore section is the only non-essential block in the revision direction
  of `report.render_revision`, so a tight budget removes it first. CRW-94 owns that
  behaviour; nothing here changes it.

## What none of this closes

Whether a new test asserts something an existing test already asserts. No scan can answer
that, and claiming otherwise would be the same kind of overstatement this map exists to
avoid. The reuse column is where that judgement is recorded, not where it is enforced.
