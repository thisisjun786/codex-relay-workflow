# Test-suite map for the Go port (CRW-140)

Every test file under `packages/*/tests` and `scripts/ci/tests`, classified by how it can be
carried to Go and where the property it protects will live. Measured on `dev` at
`4b4cb46351f712ac1c35b64405c34370021cc691`. Classification follows the research draft
(`.omo/drafts/crw-go-port.md` L21-L26); the one file the draft did not list,
`test_approval_routing.py`, is class B.

`scripts/port/check_test_map.py` keeps this table honest: it fails when a `test_*.py` file is
missing, duplicated or stale, when a class is not A/B/C, when a `tests` cell differs from
`grep -c 'def test_'`, when a destination kind is unknown, when a C row does not name its
coupling, or when a stated total below disagrees with the rows or the files.

## Classes

- **A** black box: drives a real process or pipe (subprocess, CLI argv, JSON stdout, SQLite
  file, MCP stdio, hook stdin/exit). Becomes contract-corpus scenarios (todo 6) that both
  suites run.
- **B** Python API, language-independent property: re-expressed as Go package tests by the
  todo that ports the package.
- **C** coupled to Python source (ast, inspect.getsource/signature, `__file__` source reads,
  importlib/exec loading, docstring asserts). Each row names the coupling and the Go check that
  inherits the property; none is dropped without naming its inheritor.

## Columns

- **tests**: `grep -c 'def test_'` of the file.
- **family**: the property the file protects.
- **fixtures**: fixture files or directories the file reads beyond its own module. Nearly every
  relay file also uses `packages/codex-session-relay/tests/support.py` (temporary tree, fake
  clock); every bridge file uses `packages/codex-thread-bridge/tests/conftest.py`.
- **owner**: the plan todo and Linear issue that carries the property to Go.
- **destination**: one or more of `corpus: <domain>` (contract/fixtures/<domain>, todo 6),
  `go-test: <package>` (Go package test), `inventory-check: <check>` (data-driven or go/ast
  check replacing a C-class source inventory), `drop: <reason naming the inheritor>`, joined
  by ` + `. Package names not yet fixed by the plan are written as `<area> package (todo N)`.
- **coupling**: for C rows, what ties the test to Python source (with `file:line`); `-` otherwise.

Not test files, but consumed by tests or skill scripts and carried by the destinations above:
`packages/codex-session-relay/tests/fixtures/{merge_turn_crossed_handoff,merge_turn_oracle,stop_event_r1}.json`,
`packages/codex-session-relay/tests/linear_readback_fixtures.py`,
`plugins/crw/skills/crw-run/scripts/fixtures/{decisions,titles,host}` (decisions and host are
read by `hook_probe.py` through `scripts/ci/contracts.py`, not by a test file; todo 35),
`scripts/ci/tests/{relay_schema_shipped.json,fake_git.sh,fake_gh.sh,release_steps.py}`.

## Totals

Files: 121
Tests: 6053
Class A: files=14 tests=557
Class B: files=87 tests=3353
Class C: files=20 tests=2143

## Files

| path | tests | class | family | fixtures | owner | destination | coupling |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `packages/codex-session-relay/tests/test_ack_disposition_race.py` | 5 | B | two processes acknowledging one event; which may win | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_ack_reconcile.py` | 40 | B | ACK, verdicts, reconciliation and restart recovery | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_anchor_binding.py` | 14 | B | an anchor binds on every route to dispatched | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_assignment.py` | 58 | B | assignment ledger: one issue, one responsible child | - | todo 25 / CRW-154 | go-test: `internal/relay/registry` (todo 25) | - |
| `packages/codex-session-relay/tests/test_attempt_message_atomicity.py` | 13 | B | a send carries the message its attempt froze | - | todo 18 / CRW-152 | go-test: `internal/relay/store` (todo 18) | - |
| `packages/codex-session-relay/tests/test_bridge_adapter.py` | 75 | B | bridge adapter logic over an injected RPC surface | `codex_session_relay.fakehost` | todo 28 / CRW-154 | go-test: bridge adapter package (todo 28) | - |
| `packages/codex-session-relay/tests/test_bridge_load_roots.py` | 12 | B | a parent loaded by another task's bridge is still reached (CRW-235) | - | todo 28 / CRW-154 | go-test: bridge adapter package (todo 28) | - |
| `packages/codex-session-relay/tests/test_capacity.py` | 30 | B | capacity counting and refusal to infer | - | todo 27 / CRW-154 | go-test: capacity package (todo 27) | - |
| `packages/codex-session-relay/tests/test_child_packets.py` | 82 | B | child packets a receiver can act on (CRW-149) | - | todo 23 / CRW-153 | go-test: `internal/relay/reception` (todo 23) | - |
| `packages/codex-session-relay/tests/test_cli.py` | 86 | C | command surface end to end in-process: flags, JSON, exits, host-free | fixtures/host (inline) | todo 25 / CRW-154 | corpus: cli-shape + corpus: exit-codes + inventory-check: settings-transformation coverage table in `internal/relay/registry` (todo 25) | ast + inspect.getsource over delivery/bridge_adapter/settings (:2390-2418) to derive the TaskSettings calls a send applies |
| `packages/codex-session-relay/tests/test_contract_corpus.py` | 3 | B | corpus discovery and CLI runner proof added in todo 6; not original class-A cases | `contract/fixtures/cli-shape` | todo 6 / CRW-140 | corpus: cli-shape | - |
| `packages/codex-session-relay/tests/test_coordination_cli.py` | 19 | A | operator surface of 26 coordination commands: argv shape, exits, JSON | - | todo 26 / CRW-154 | corpus: cli-shape | - |
| `packages/codex-session-relay/tests/test_coordination_contract.py` | 11 | C | coordination modules: one txn per mutator, no own wait loops, lock wait declared once | - | todo 26 / CRW-154 | inventory-check: go/ast scan of the linkage/merge-turn packages (todo 26) + go-test: transaction-count cases (todo 26) | ast over linkage/mergeturn source (:22, :59) |
| `packages/codex-session-relay/tests/test_criteria_registration.py` | 9 | B | criteria ensure never replaces a set | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_daemon.py` | 19 | B | bounded daemon: automatic invocation, silence when idle | - | todo 29 / CRW-155 | go-test: `internal/relay/daemon` (todo 29) | - |
| `packages/codex-session-relay/tests/test_daemon_cadence.py` | 24 | B | daemon polling cadence supplied by the CLI | `codex_session_relay.fakehost` | todo 29 / CRW-155 | go-test: `internal/relay/daemon` (todo 29) | - |
| `packages/codex-session-relay/tests/test_delivery.py` | 75 | B | delivery: busy handling, dispatch, bounds, reporting (JUN-91) | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_delivery_relation.py` | 14 | B | recipient resolved from linkage, not frozen parent | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_diagnostics.py` | 32 | B | diagnosis words that tell an operator what to do | - | todo 20 / CRW-152 | go-test: doctor/diagnostics package (todo 20) | - |
| `packages/codex-session-relay/tests/test_directive_places.py` | 20 | B | instruction purpose decides competition; held report named (CRW-230) | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_dispositions.py` | 61 | A | list stopped children through the real CLI (CRW-163) | - | todo 25 / CRW-154 | corpus: cli-shape | - |
| `packages/codex-session-relay/tests/test_edit_regions.py` | 73 | B | edit-region agreements and their limits | - | todo 27 / CRW-154 | go-test: edit-regions package (todo 27) | - |
| `packages/codex-session-relay/tests/test_enqueue_durability.py` | 8 | B | terminal observation never outlives its queuing (JUN-167) | - | todo 29 / CRW-155 | go-test: `internal/relay/daemon` (todo 29) | - |
| `packages/codex-session-relay/tests/test_failure_recovery.py` | 14 | C | recovery from stopped process / locked store; bounded read-only connections | - | todo 17 / CRW-152 | inventory-check: go/ast scan that every SQLite open in `internal/relay/store` uses the declared bound (todo 17) + go-test: recovery cases (todo 21) | ast over sibling modules via intent.__file__ (:19, :527) |
| `packages/codex-session-relay/tests/test_fairness.py` | 22 | B | per-parent fairness in the daemon | - | todo 29 / CRW-155 | go-test: `internal/relay/daemon` (todo 29) | - |
| `packages/codex-session-relay/tests/test_fault_contract.py` | 120 | C | corrected fault-ledger contract, one class per blocker/finding | - | todo 22 / CRW-153 | go-test: `internal/relay/faults` (todo 22); the inspect.signature parameter assertions are inherited by the Go compiler (typed call sites in the tests) | inspect.signature on faults/faultsweep (:8, :401, :604, :1794, :1810) |
| `packages/codex-session-relay/tests/test_fault_live_findings.py` | 23 | B | CRW-205 live findings the collector missed | - | todo 22 / CRW-153 | go-test: `internal/relay/faults` (todo 22) | - |
| `packages/codex-session-relay/tests/test_fault_notices.py` | 54 | B | fault notification goes up the supervisor channel once | - | todo 22 / CRW-153 | go-test: `internal/relay/faults` (todo 22) | - |
| `packages/codex-session-relay/tests/test_faults.py` | 122 | C | fault ledger: one breakage, one record, closed by reverification | - | todo 22 / CRW-153 | go-test: `internal/relay/faults` (todo 22) + inventory-check: `go list -deps ./internal/relay/faults/...` contains no network client (todo 22) | ast import scan of faults.py/faultsweep.py (:8, :1279) |
| `packages/codex-session-relay/tests/test_forge_evidence.py` | 73 | B | merge-evidence collector answers independent of reader care | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_guard.py` | 122 | B | Stop decision judged against real receipts | - | todo 33 / CRW-156 | go-test: `internal/relay/hook` (todo 33) | - |
| `packages/codex-session-relay/tests/test_guard_property.py` | 58 | C | every guard evaluation yields a recorded, classified result; no sentinel discarded | - | todo 33 / CRW-156 | inventory-check: go/ast scan over `internal/relay/hook` and `internal/relay/store` for discarded (value, ok) and (value, err) results (todo 33) + go-test: evaluation classes (todo 33) | ast over src/codex_session_relay sentinel modules (:21, :500+) |
| `packages/codex-session-relay/tests/test_host_lost_turn.py` | 98 | B | host-accepted-then-lost turn vs lost ACK (CRW-224) | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_identity.py` | 13 | B | identity derivations vs frozen canonical rendering | - | todo 18 / CRW-152 | go-test: `internal/relay/store` (todo 18) + corpus: event-key | - |
| `packages/codex-session-relay/tests/test_intent.py` | 58 | B | management intent and its state | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_launch_policy.py` | 26 | B | what a service launches its daemon with and why | - | todo 32 / CRW-155 | go-test: `internal/relay/daemon` (todo 32) | - |
| `packages/codex-session-relay/tests/test_linkage.py` | 132 | B | three-level linkage: identity, role contract, refusals, handover | - | todo 26 / CRW-154 | go-test: linkage package (todo 26) | - |
| `packages/codex-session-relay/tests/test_linkage_peer.py` | 16 | B | peer parent links | - | todo 26 / CRW-154 | go-test: linkage package (todo 26) | - |
| `packages/codex-session-relay/tests/test_linkage_queries.py` | 14 | B | bidirectional hierarchy queries | - | todo 26 / CRW-154 | go-test: linkage package (todo 26) | - |
| `packages/codex-session-relay/tests/test_linkage_recovery.py` | 15 | B | two-level store compatibility and interrupted transitions | `codex_session_relay.fakehost` | todo 26 / CRW-154 | go-test: linkage package (todo 26) | - |
| `packages/codex-session-relay/tests/test_managed_adapter_ledger.py` | 4 | B | managed adapter ledger follows the explicit store | - | todo 28 / CRW-154 | go-test: bridge adapter package (todo 28) | - |
| `packages/codex-session-relay/tests/test_managed_execution.py` | 3 | B | managed start sequence as instructed to a coordinator | - | todo 27 / CRW-154 | go-test: managed-start package (todo 27) | - |
| `packages/codex-session-relay/tests/test_managed_reservation.py` | 15 | B | managed admission reservation: one pending request, one owner | - | todo 27 / CRW-154 | go-test: managed-start package (todo 27) | - |
| `packages/codex-session-relay/tests/test_managed_start.py` | 35 | B | managed entry drives registry/criteria/marker state | - | todo 27 / CRW-154 | go-test: managed-start package (todo 27) | - |
| `packages/codex-session-relay/tests/test_management_cli.py` | 28 | A | managed marker through the real command line | - | todo 32 / CRW-155 | corpus: cli-shape + corpus: records | - |
| `packages/codex-session-relay/tests/test_manifest_scope.py` | 51 | B | MANIFEST-CANON-01, path containment, artifact-read stability | - | todo 28 / CRW-154 | go-test: manifest/scope package (todo 28) | - |
| `packages/codex-session-relay/tests/test_marker.py` | 17 | B | create-once marker write protocol | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_merge_evidence.py` | 36 | B | mergeevidence predicates agree with the landed merge turn | `tests/fixtures/merge_turn_oracle.json` | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_merge_target.py` | 19 | B | merge target base branch read from real git repos (CRW-229) | - | todo 26 / CRW-154 | go-test: merge-turn package (todo 26) | - |
| `packages/codex-session-relay/tests/test_merge_turn.py` | 142 | B | whose turn it is to merge | `tests/fixtures/merge_turn_crossed_handoff.json` | todo 26 / CRW-154 | go-test: merge-turn package (todo 26) | - |
| `packages/codex-session-relay/tests/test_merge_turn_wake.py` | 62 | B | freed merge target reaches the parent it was handed to | - | todo 26 / CRW-154 | go-test: merge-turn package (todo 26) | - |
| `packages/codex-session-relay/tests/test_multi_parent_isolation.py` | 8 | B | two parents, two repos, one store | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_observation_budget.py` | 22 | B | observation budget never skips the current generation | - | todo 29 / CRW-155 | go-test: `internal/relay/daemon` (todo 29) | - |
| `packages/codex-session-relay/tests/test_omitted.py` | 35 | B | persisted Stop vs relay settlement as separate evidence | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_on_request_delivery.py` | 11 | B | on-request recipients carried; approvals not answered (CRW-225) | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_operational_scale.py` | 8 | B | how far/how much a bounded daemon carries | `codex_session_relay.fakehost` | todo 29 / CRW-155 | go-test: `internal/relay/daemon` (todo 29) | - |
| `packages/codex-session-relay/tests/test_product_routing.py` | 82 | B | product routing against the fault ledger (CRW-206 matrix) | - | todo 23 / CRW-153 | go-test: `internal/relay/routing` (todo 23) | - |
| `packages/codex-session-relay/tests/test_product_routing_decisions.py` | 103 | C | routing registry, bindings, placement, completion verdicts | - | todo 23 / CRW-153 | go-test: `internal/relay/routing` (todo 23) + inventory-check: `go list -deps ./internal/relay/routing/...` excludes `internal/relay/faults` (todo 23) | ast import scan via ledger_port.__file__ (:559, :635-656) |
| `packages/codex-session-relay/tests/test_project_completion.py` | 17 | B | project-level reading of child reports | - | todo 23 / CRW-153 | go-test: `internal/relay/routing` (todo 23) | - |
| `packages/codex-session-relay/tests/test_receipts.py` | 38 | B | completion receipts: five endings | - | todo 18 / CRW-152 | go-test: `internal/relay/store` (todo 18) | - |
| `packages/codex-session-relay/tests/test_reception_findings.py` | 45 | B | PR #116 review findings on the packet validator | - | todo 23 / CRW-153 | go-test: `internal/relay/reception` (todo 23) | - |
| `packages/codex-session-relay/tests/test_recovery_negatives.py` | 5 | B | what recovery must not do (CRW-125) | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_register_atomicity.py` | 5 | B | one registration is one commit | - | todo 25 / CRW-154 | go-test: `internal/relay/registry` (todo 25) | - |
| `packages/codex-session-relay/tests/test_registration_contention.py` | 4 | B | registration under contention | - | todo 25 / CRW-154 | go-test: `internal/relay/registry` (todo 25) | - |
| `packages/codex-session-relay/tests/test_registration_hold.py` | 3 | B | registration hold with a pinned interleaving | - | todo 25 / CRW-154 | go-test: `internal/relay/registry` (todo 25) | - |
| `packages/codex-session-relay/tests/test_registry.py` | 23 | B | relationships, generations, anchors, lifecycle | - | todo 25 / CRW-154 | go-test: `internal/relay/registry` (todo 25) | - |
| `packages/codex-session-relay/tests/test_regression_map.py` | 29 | C | regression map inventories: citations resolve, clock labels, real-time users, boolean folds | - | todo 9 / CRW-150 | inventory-check: go/ast scan in `internal/contracttest` over the Go test tree listing real-time clock users and fault-injection sites (todo 9) | ast + docstring asserts over the package source (:25, :582) |
| `packages/codex-session-relay/tests/test_report_contract.py` | 87 | B | recipient can act on the message; two vocabularies agree (JUN-131) | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_reporting_cli.py` | 6 | A | offline reporting-show through a real subprocess: selectors, exit 4 | - | todo 24 / CRW-153 | corpus: cli-shape + corpus: exit-codes | - |
| `packages/codex-session-relay/tests/test_rereview_deadlock.py` | 27 | B | criteria re-review is recordable | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_restoration_visibility.py` | 37 | B | restoration block reaches the child (CRW-94) | `src/codex_session_relay/schema/*.json` | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_revision_correction_form.py` | 23 | B | needs_changes revision request carries the correction form | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_revision_roundtrip.py` | 13 | B | correction lineage across a real needs_changes generation | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_rolepolicy.py` | 45 | B | task role checked against recorded authorization | - | todo 25 / CRW-154 | go-test: `internal/relay/registry` (todo 25) | - |
| `packages/codex-session-relay/tests/test_schema_conformance.py` | 15 | B | persisted records conform to the frozen schemas | `src/codex_session_relay/schema/*.json`, `codex_session_relay.fakehost` | todo 18 / CRW-152 | go-test: `internal/relay/store` (todo 18) + corpus: records | - |
| `packages/codex-session-relay/tests/test_service.py` | 123 | B | who owns the daemon and who may stop it | - | todo 29 / CRW-155 | go-test: `internal/relay/daemon` (todo 29) | - |
| `packages/codex-session-relay/tests/test_settings_hold_naming.py` | 31 | B | settings hold names reason and recovery (CRW-235) | - | todo 25 / CRW-154 | go-test: `internal/relay/registry` (todo 25) | - |
| `packages/codex-session-relay/tests/test_settings_preservation.py` | 36 | B | execution settings carried; refuse to send without them | - | todo 25 / CRW-154 | go-test: `internal/relay/registry` (todo 25) | - |
| `packages/codex-session-relay/tests/test_stop_adapter.py` | 17 | A | Stop adapter process: stdin, settings, exit, concurrency | `packages/codex-session-relay/tests/fixtures/stop_event_r1.json` | todo 33 / CRW-156 | corpus: hook | - |
| `packages/codex-session-relay/tests/test_store.py` | 65 | B | durable store: a failed transition is never a success | - | todo 18 / CRW-152 | go-test: `internal/relay/store` (todo 18) | - |
| `packages/codex-session-relay/tests/test_store_reception.py` | 74 | B | packet-check --receiver against a real store | - | todo 23 / CRW-153 | go-test: `internal/relay/reception` (todo 23) | - |
| `packages/codex-session-relay/tests/test_supersession.py` | 22 | B | stale event stopped before the send | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_supervisor_autosend.py` | 22 | B | daemon tick stages and sends what the supervisor is owed | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_supervisor_channel.py` | 165 | B | parent-to-supervisor channel: staged, sent, read back | `codex_session_relay.fakehost` | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_supervisor_envelope.py` | 43 | B | recipient can tell what was asked and what was not said (CRW-148) | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_supervisor_live_findings.py` | 22 | B | CRW-215 live round-trip findings | `codex_session_relay.fakehost` | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_supervisor_omission_store.py` | 29 | B | omitted report derived from the store and sent | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_supervisor_reporting.py` | 70 | B | what the level above is told and still owed (CRW-148) | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_sync_outbox.py` | 76 | B | coordination-document outbox: durable, retried, idempotency | `tests/linear_readback_fixtures.py` | todo 23 / CRW-153 | go-test: `internal/relay/sync` (todo 23) | - |
| `packages/codex-session-relay/tests/test_transfer_phases.py` | 7 | B | relay handling of an expired transfer phase | - | todo 28 / CRW-154 | go-test: bridge adapter package (todo 28) | - |
| `packages/codex-session-relay/tests/test_unknown_send_lost.py` | 69 | B | uncertain send with no trace held, never resent (CRW-231) | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_verification_currency.py` | 41 | B | verification currency, canonical criteria, lineage | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_worker_policy.py` | 18 | A | policy read from the serving process, not the asking shell | - | todo 32 / CRW-155 | corpus: cli-shape | - |
| `packages/codex-session-relay/tests/test_wp1_regressions.py` | 34 | B | wp1 independent-review regressions (receipts, scope) | - | todo 18 / CRW-152 | go-test: `internal/relay/store` (todo 18) | - |
| `packages/codex-thread-bridge/tests/test_approval_routing.py` | 8 | B | approval-class requests left for the thread's approver (CRW-225) | `tests/conftest.py` fake App Server | todo 13 / CRW-151 | go-test: `internal/bridge/appserver` (todo 13) | - |
| `packages/codex-thread-bridge/tests/test_bridge.py` | 74 | B | create/followup/duplicate/conflict/replay through the bridge | `tests/conftest.py` fake App Server | todo 15 / CRW-151 | go-test: `internal/bridge` (todo 15) | - |
| `packages/codex-thread-bridge/tests/test_execution.py` | 63 | B | execution guard refusals; nothing dispatched | `tests/conftest.py` fake App Server | todo 14 / CRW-151 | go-test: `internal/bridge/execution` (todo 14) | - |
| `packages/codex-thread-bridge/tests/test_contract_corpus.py` | 4 | B | corpus discovery and git/App Server runner proofs added in todo 6; not original class-A cases | `contract/fixtures/git`, `contract/fixtures/appserver` | todo 6 / CRW-140 | corpus: git + corpus: appserver | - |
| `packages/codex-thread-bridge/tests/test_ledger.py` | 6 | B | operation ledger: socket aliases, legacy import, re-arm | - | todo 14 / CRW-151 | go-test: `internal/bridge/ledger` (todo 14) + corpus: ledger-fingerprint | - |
| `packages/codex-thread-bridge/tests/test_mcp.py` | 11 | A | stdio MCP server: schema refusals, host policy, plugin launch | `tests/conftest.py` fake App Server | todo 16 / CRW-151 | corpus: mcp-tools | - |
| `packages/codex-thread-bridge/tests/test_rpc.py` | 20 | B | App Server RPC: multiplexing, errors, timeouts, frame limits | `tests/conftest.py` fake App Server | todo 13 / CRW-151 | go-test: `internal/bridge/appserver` (todo 13) + corpus: appserver | - |
| `packages/codex-thread-bridge/tests/test_settings.py` | 40 | B | settings survive create/resume; distinct causes | `tests/conftest.py` fake App Server | todo 14 / CRW-151 | go-test: `internal/bridge/settings` (todo 14) | - |
| `packages/codex-thread-bridge/tests/test_worktree.py` | 28 | A | create_worktree against real git: exact base, collisions, replay | `tests/conftest.py` fake App Server | todo 15 / CRW-151 | corpus: mcp-tools + go-test: `internal/bridge/worktrees` (todo 15) | - |
| `scripts/ci/tests/test_adapter_agreement.py` | 53 | A | packaged vs checkout adapter over the same invocations | `packages/codex-session-relay/tests/fixtures/stop_event_r1.json` | todo 33 / CRW-156 | corpus: hook | - |
| `scripts/ci/tests/test_completion_hook.py` | 221 | A | completion hook adapter vs a fake relay: journal, claims, exits | - | todo 33 / CRW-156 | corpus: hook | - |
| `scripts/ci/tests/test_gate.py` | 15 | C | dev-gate check selection and result judgement | `.github/workflows/ci.yml` | todo 47 / CRW-160 | go-test: `crw-dev ci gate` (todo 47) | importlib spec_from_file_location of scripts/ci/gate.py |
| `scripts/ci/tests/test_hook_comparison.py` | 127 | C | off/on hook comparison harness states and its declared readings | - | todo 46 / CRW-159 | go-test: `internal/dev` hook-compare, build tag dev (todo 46) + inventory-check: go/ast scan of the harness (todo 46) | ast over scripts/hook_comparison.py |
| `scripts/ci/tests/test_install.py` | 9 | C | skill-link install: check/apply, idempotence, foreign paths | - | todo 39 / CRW-158 | go-test: `crw install skills` (todo 39) | importlib load of scripts/install.py |
| `scripts/ci/tests/test_install_acceptance.py` | 61 | C | new install carried through to recovery of the replaced install | - | todo 38 / CRW-158 | go-test: `internal/runtime/install` (todo 38) + inventory-check: go/ast scan of fixture switches (todo 38) | ast over the installer and its fixture (494 reflective sites) |
| `scripts/ci/tests/test_packages.py` | 12 | C | packages check never prints success over an empty or skipped run | - | todo 47 / CRW-160 | drop: packages.py only runs the Python suites and is deleted in todo 44/47; the property (no success over an empty or skipped run) is inherited by todo 9's `CRW_CONTRACT_STRICT=1` skip counting and `go test` exit status | importlib load of scripts/ci/packages.py |
| `scripts/ci/tests/test_parent_title.py` | 9 | A | parent-title helper command surface and coverage guard | `plugins/crw/skills/crw-run/scripts/fixtures/titles` | todo 35 / CRW-156 | corpus: skill-scripts (domain added by todo 35) | - |
| `scripts/ci/tests/test_plugin.py` | 62 | C | plugin package validator vs shapes that install silently wrong | - | todo 47 / CRW-160 | go-test: `crw-dev ci plugin` (todo 47) | importlib load of scripts/ci/plugin.py |
| `scripts/ci/tests/test_plugin_transition.py` | 239 | C | manual-to-plugin transition on synthetic hosts | - | todo 39 / CRW-158 | go-test: `crw install transition` (todo 39) | ast/importlib over crw_transition |
| `scripts/ci/tests/test_plugin_wiring.py` | 188 | C | one owner registers the Stop hook; launchers resolve | - | todo 34 / CRW-156 | go-test: plugin wiring (todo 34) + go-test: isolated Codex home integration (todo 40) | exec of plugins/crw/wiring/crw_bridge_mcp.py (:80) + ast (:2132) |
| `scripts/ci/tests/test_relay_schema_shipped.py` | 1 | A | shipped store objects keep their CREATE text | `scripts/ci/tests/relay_schema_shipped.json` | todo 17 / CRW-152 | corpus: sqlite-ddl | - |
| `scripts/ci/tests/test_release.py` | 10 | A | release workflow steps against fake git/gh | `scripts/ci/tests/{fake_git.sh,fake_gh.sh,release_steps.py}` | todo 12 / CRW-150 | corpus: release (domain added by todo 12) | - |
| `scripts/ci/tests/test_runtime_install.py` | 496 | C | runtime installer and diagnosis on temporary destinations | - | todo 38 / CRW-158 | go-test: `internal/runtime` (todo 37) + go-test: `internal/runtime/install` (todo 38) + inventory-check: go/ast scan of command producers (todo 38) | ast over scripts/runtime_install.py (406 reflective sites) |
| `scripts/ci/tests/test_scope.py` | 14 | C | CI selection evidence from real git changes incl. renames | - | todo 47 / CRW-160 | go-test: `crw-dev ci scope` (todo 47) | importlib load of scripts/ci/scope.py |
| `scripts/ci/tests/test_stop_events.py` | 75 | A | one accepted record per Stop event through host paths (CRW-212) | `packages/codex-session-relay/tests/fixtures/stop_event_r1.json` | todo 46 / CRW-159 | corpus: hook | - |
| `scripts/ci/tests/test_trial_startup.py` | 374 | C | live-trial preflight states asserted against runs (A/C mix) | - | todo 46 / CRW-159 | go-test: `internal/dev` trial-startup, build tag dev (todo 46) + inventory-check: go/ast scan (todo 46) | ast over scripts/trial_startup.py; subprocess runs of the harness |
| `scripts/ci/tests/test_validate.py` | 3 | C | skill structure validator: metadata, links | - | todo 47 / CRW-160 | go-test: `crw-dev ci validate` (todo 47) | importlib load of scripts/ci/validate.py |
