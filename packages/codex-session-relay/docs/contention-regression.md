# Contention and failure-recovery regression map

What each required scenario is proven by, which proof already existed before this
work, and which one this work added. A row that reuses earlier evidence names the
file it reuses so that nobody re-derives it; a row that adds evidence names the new
test. Where a scenario is driven by a clock a test moves by hand, the row says so,
because an injected clock reproduces arithmetic and never elapsed time.

## What the two named landings actually changed

The issue was written expecting `ff3c69b5` (management markers) and `6f77ab4`
(the completion-receipt hook) to have moved this package. They did not.
`git show --stat ff3c69b5` is six files under `skills/`; `git show --stat 6f77ab4`
is `scripts/completion_hook.py`, `scripts/crw_runtime/completion.py`,
`scripts/runtime_install.py`, `docs/runtime-install.md` and two files under
`scripts/ci/tests/`. `git log ff3c69b5^1..6f77ab4 -- packages/codex-session-relay`
is empty.

So the contention those landings introduce is not a diff in this package. It is a
diff in what now *calls* this package: the hook adapter runs `guard-evaluate` as a
subprocess against a live store while the daemon is writing to it, and the marker
facts the skills describe are now produced by a coordinator that can stop between
any two of them. The boundary under test is therefore the package's own behaviour
when those callers fail part-way, which is what the scenarios below reproduce.

## Criterion map

| # | Scenario | Reused evidence | New evidence | Clock |
|---|---|---|---|---|
| 1 | Registration aborted before and after it completes, a lost creation response, duplicate and late binding, wrong owner or generation | `test_intent.py` DerivedState, Binding, Registration; `test_registry.py` Generations; `test_guard.py` UnmanagedAndUnclaimed, Declarations; `test_anchor_binding.py` AnchorBinding | `test_registration_contention.py` | injected |
| 2 | Daemon exit and restart, and connection loss, around emit, delivery and acknowledgement, recovered from the persisted waiting records | `test_ack_reconcile.py` RestartRecovery; `test_delivery.py` RestartPreservation; `test_enqueue_durability.py`; `test_daemon.py` ReconcileGate | `test_failure_recovery.py` | injected |
| 3 | Guard timeout, guard error, and the block limit reached, with no infinite repetition and work processed after recovery | `test_guard.py` Bounds, FailureSeparation; `test_guard_property.py` FailureIsNotANormalState | `test_failure_recovery.py` | injected |
| 4 | needs_changes continuing into the next generation of the same accountable task, with duplicate, out-of-order and late events causing no additional execution | `test_supersession.py` PreSendSupersession; `test_rereview_deadlock.py` ReReviewIsReachable; `test_ack_reconcile.py` Verdicts | `test_registration_contention.py` | injected |
| 5 | Two parents from different repositories and Linear projects on one shared store, under simultaneous completion, acknowledgement, revision and outbox activity | `test_delivery.py` CrossAssignmentDelivery; `test_fairness.py` SharedChildTurns; `test_sync_outbox.py` Readback, ClaimFencing | `test_multi_parent_isolation.py` | injected |
| 6 | One parent busy, failing or at its retry limit, and one paused, cancelled or archived, while the other parent keeps progressing | `test_fairness.py` DeliveryFairness; `test_delivery.py` Bounds, HostLifecycle | `test_multi_parent_isolation.py` | injected |
| 7 | Daemon run-limit exit and restart, duplicate startup on the same and on a different store, and an assignment outliving four hours | `test_daemon.py` Bounds, Instance; `test_service.py` Ownership, FourHourBoundary; `test_cli.py` ContestedSocket | `test_operational_scale.py` | injected, see below |
| 8 | A declared parent and child count and event volume, measured for queue depth, ticks to drain and send limits | none; this is new ground | `test_operational_scale.py` | injected |

## What the injected clock does not say

`FourHourBoundary` in `tests/test_service.py` already drives the supervisor past
four hours of scripted monotonic time, and its docstring records what that cannot
establish: nothing there observes four real hours, so nothing there speaks to
process memory, write-ahead-log growth, descriptor or socket drift, or an App
Server that answers differently after hours of uptime. The lifetime work in
criterion 7 is built on that harness and inherits the same limit. The rows above
marked *injected* are arithmetic and ordering, reproduced exactly; the only real
elapsed time anywhere in this package is the single real-process segment in
`test_service.py` and the cadence test in `test_daemon_cadence.py`.

## Scale, stated as a bound rather than a guarantee

Criterion 8 asks for a number, so the suite declares its parent count, child count
and event volume, and records the queue depth, the ticks needed to drain it and the
per-tick send ceiling that produced it. That is a measurement of this harness on
one machine with a clock that never sleeps. It is evidence that selection stays
bounded and fair at that size; it is not a throughput figure, and it says nothing
about a host under real load. Passing at the declared size must not be reported as
support for unbounded parallel operation.

## Things found while writing this, owned elsewhere

- `guard.SQLITE_TIMEOUT` documents the lock wait the readiness check is allowed to
  spend, but nothing reads it: `intent.read_only_connection` passes its own literal
  `2.0`. The bound is in force and the constant that explains it is not wired to it,
  so changing the declared number would change nothing. Pinned by a new test rather
  than left to drift.
- The workflow-restore section is the only non-essential block in the revision
  direction of `report.render_revision`, so it is the first thing a tight budget
  removes. CRW-94 owns that behaviour; nothing here changes it.
