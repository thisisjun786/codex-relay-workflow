# Python inventory for the Go port (CRW-140)

Every non-test `.py` file under `packages/`, `scripts/`, `plugins/crw/wiring/` and
`plugins/crw/skills/*/scripts/`, mapped to the P-CRW-58 issue that owns its Go replacement.
Measured on `dev` at `4b4cb46351f712ac1c35b64405c34370021cc691`.

`scripts/port/check_inventory.py` keeps this table honest: it fails when the path column and
`find packages scripts plugins -name '*.py' -not -path '*/tests/*' -not -path '*/__pycache__/*'`
differ, when a line count differs from `wc -l`, when an owning issue is missing, or when a
`retire-with-evidence` row lacks its consumer search or removal trigger.

## Columns

- **lines**: `wc -l` of the file.
- **invoked**: how the file is reached - console script, hook, MCP, CI, skill instruction,
  operator CLI, spawned-by, or imported-by (library module).
- **runs**: who executes it - `end user + relay host` (plugin path and installed runtime),
  `relay host` (daemon, relay library, installer internals), `dev/CI` (never on a user machine).
- **owner**: the Linear issue whose Go work replaces it. CRW-150 foundation (dispatcher, errors,
  shared test support), 151 bridge, 152 store, 153 delivery/faults/routing/reporting,
  154 registry/linkage/coordination, 155 daemon/service, 156 hook, plugin wiring and skill
  scripts, 157 runtime diagnosis, 158 installer and transition, 159 dev harnesses, 160 CI scripts.
- **disposition**: `port` (a Go equivalent is written), `retire-with-evidence` (no Go equivalent;
  removed only when the trigger holds), `keep-as-data` (stays as data; none today).
- **consumer_search**: for retire rows, the search boundary used to find every consumer, and the
  consumers it found. A grep miss alone never retires a file.
- **removal_trigger**: for retire rows, the observable condition after which the file may go.

## Files

Total non-test lines: 95657

| path | lines | invoked | runs | owner | disposition | consumer_search | removal_trigger |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `packages/codex-session-relay/src/codex_session_relay/control.py` | 244 | imported by CLI supervisor | end user + relay host | CRW-156 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/inbox.py` | 239 | imported by CLI admission/recovery | end user + relay host | CRW-152 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/ownership.py` | 625 | imported by Store, registration, service, bridge ledger | end user + relay host | CRW-152 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/takeover.py` | 132 | imported by service run candidate entry point | relay host | CRW-152 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/ack.py` | 1098 | imported by cli, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/admission.py` | 209 | imported by cli, daemon, managed, omitted, receipts | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/assignment.py` | 1163 | imported by cli, delivery, dispositions, faultsweep, linkage, reconcile | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/bridge_adapter.py` | 1321 | imported by cli; imports the bridge in-process (:875-900) | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/capacity.py` | 594 | imported by cli | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/cli.py` | 6158 | console script `codex-session-relay` (cli:main); `python -m codex_session_relay.cli` re-exec target of service.py:1728/1827 and supervisorchannel.py:206; run by skills, stopadapter guard call, crw_runtime.scope | end user + relay host | CRW-150 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/clock.py` | 34 | imported by cli | relay host | CRW-152 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/completion.py` | 518 | imported by routing | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/coordination.py` | 129 | imported by capacity, editregion, mergeturn | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/criteria.py` | 481 | imported by ack, assignment, cli, managed, receiver, report | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/currency.py` | 247 | imported by ack, assignment, cli, delivery, guard, receipts, reconcile | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/cxc.py` | 432 | imported by packets, report, supervision | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/daemon.py` | 1333 | imported by cli, service; runs inside the spawned daemon worker | relay host | CRW-155 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/declarations.py` | 210 | imported by cli | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/delivery.py` | 2746 | imported by ack, assignment, cli, daemon, faultsweep, hostloss, reconcile, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/digest.py` | 135 | imported by routing | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/dispositions.py` | 657 | imported by cli | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/editregion.py` | 1511 | imported by cli | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/envelope.py` | 670 | imported by cli, delivery, linkage, packets, receiver, report, supervision, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/errors.py` | 275 | imported by 33 relay modules (RefusalReason, exit-2 envelope) | relay host | CRW-150 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/fakehost.py` | 309 | imported by relay tests (support.py, test_bridge_adapter, test_daemon_cadence, ...); ships in src but no product module imports it | dev/CI | CRW-150 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/faultnotice.py` | 312 | imported by daemon | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/faults.py` | 3919 | imported by cli, daemon, faultnotice, faultsweep, ledger_port, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/faultsweep.py` | 1656 | imported by cli, daemon, delivery | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/forge.py` | 1222 | imported by cli, mergetarget; spawns `gh` (:160) | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/guard.py` | 1174 | imported by cli (`guard`/`guard-evaluate`), omitted | end user + relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/hostadapter.py` | 344 | imported by bridge_adapter, cli, fakehost, hostloss, reconcile | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/hostloss.py` | 604 | imported by daemon, reconcile | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/identity.py` | 185 | imported by 17 relay modules | relay host | CRW-152 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/__init__.py` | 11 | package init; `__version__`, `NO_DELIVERABLE` imported by 13 modules (faultsweep reads `__version__`) | relay host | CRW-150 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/intake.py` | 646 | imported by digest, routing | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/intent.py` | 1363 | imported by cli, faultnotice, guard, managed, omitted, supervision; `registration_hold()` opens the DB directly (:808) | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/ledger_port.py` | 325 | imported by completion, digest, intake, projects, routes, routing | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/lifecycle.py` | 131 | imported by delivery, faultnotice, managed, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/linkage.py` | 2448 | imported by assignment, cli, receiver, registry, rolepolicy | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/managed.py` | 707 | imported by cli; imports bridge settings (:66) | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/manifest.py` | 368 | imported by cli, guard, receipts | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/marker.py` | 486 | imported by cli, faultsweep, guard, intent, managed, omitted | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/mergeevidence.py` | 515 | imported by forge, mergeturn, report | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/mergetarget.py` | 192 | imported by cli, mergeturn; spawns `git` (:95) | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/mergeturn.py` | 2129 | imported by cli, delivery, receiver, reconcile | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/models.py` | 32 | imported by cli, daemon, linkage, managed, receipts, registry | relay host | CRW-152 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/omitted.py` | 647 | imported by cli, faultsweep, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/packets.py` | 1743 | imported by cli, receiver, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/placement.py` | 270 | imported by intake, ledger_port | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/policy.py` | 187 | imported by assignment, daemon, delivery, faultsweep, hostloss, lifecycle, reconcile, service, supervisorchannel | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/products.py` | 584 | imported by cli, completion, digest, intake, ledger_port, placement, projects, routes, routing | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/projects.py` | 453 | imported by intake, routing | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/receipts.py` | 704 | imported by cli, daemon, dispositions, omitted | relay host | CRW-152 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/receiver.py` | 844 | imported by cli | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/reconcile.py` | 924 | imported by cli | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/registry.py` | 1844 | imported by cli, delivery, linkage, managed, packets, receipts, receiver, service | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/report.py` | 2608 | imported by cli, delivery, mergeturn, supervision, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/restoration.py` | 126 | imported by ack, cli, criteria, delivery, report | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/rolepolicy.py` | 541 | imported by bridge_adapter, cli, delivery, linkage, managed, receiver, registry, service; imports bridge execution (:176) | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/routes.py` | 287 | imported by completion, digest, intake, projects, routing | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/routing.py` | 282 | imported by cli | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/scope.py` | 418 | imported by daemon, delivery, editregion, manifest, receipts; Linux F_SETLEASE (:194-216) | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/service.py` | 2204 | imported by cli (`daemon`, `service ...`); spawns the daemon worker | relay host | CRW-155 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/settings.py` | 866 | imported by assignment, bridge_adapter, cli, fakehost, faultsweep, managed, packets, receiver, registry | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/stopadapter.py` | 1385 | console script `crw-completion-hook` (stopadapter:main); started by the plugin launcher crw_stop_hook.py via `adapterEntryPoint` at every Stop | end user + relay host | CRW-156 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/store.py` | 2961 | imported by bridge_adapter, cli, declarations, dispositions, omitted, service, supervisorchannel | relay host | CRW-152 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/supervision.py` | 743 | imported by cli, delivery, faultnotice, faults, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/supervisorchannel.py` | 3248 | imported by assignment, cli, daemon, faultnotice, faults; renders the relay launcher tuple (:206) | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/sync.py` | 899 | imported by cli, faults, receiver, supervision | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/transport.py` | 198 | imported by 13 relay modules (ack, delivery, reconcile, ...) | relay host | CRW-154 | port | - | - |
| `packages/codex-thread-bridge/scripts/check_connection.py` | 60 | components.json `exerciseScript`; spawned by runtime_install.py `measure` (:4539) and `observe_app_server` (:1429) | relay host | CRW-151 | retire-with-evidence | `grep -rn check_connection` over scripts/, docs/, packages/*/README.md, scripts/crw_runtime/components.json (consumers: components.json:19, runtime_install.py:1429/4539, docs/runtime-install.md:81/1336, packages/codex-thread-bridge/README.md:635) | todo 37 components definition v2 replaces `exerciseScript` with `exerciseCommand` (`crw bridge` MCP `tools/list`); file deleted in todo 44 (CRW-141) |
| `packages/codex-thread-bridge/src/codex_thread_bridge/bridge.py` | 1580 | imported by server, relay bridge_adapter | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/effects.py` | 78 | imported by bridge, rpc, worktrees | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/execution.py` | 606 | imported by bridge, server, relay rolepolicy | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/__init__.py` | 3 | package init; `__version__` read by rpc.py:302 (clientInfo) and server.py:397 | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/ledger.py` | 256 | imported by bridge, server, relay bridge_adapter | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/roles.py` | 152 | imported by bridge, execution | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/rpc.py` | 546 | imported by bridge, server, relay bridge_adapter | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/server.py` | 429 | console script `codex-thread-bridge` (server:main); MCP stdio server exec'd by plugins/crw/wiring/crw_bridge_mcp.py (mcp.json) until todo 34, a launcher retired from the package in todo 43 | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/settings.py` | 574 | imported by bridge; relay assignment, bridge_adapter, cli, fakehost, faultsweep, managed, packets, receiver, registry | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/worktrees.py` | 177 | imported by bridge; spawns `git` (:20) | end user + relay host | CRW-151 | port | - | - |
| `plugins/crw/skills/crw-run/scripts/hook_probe.py` | 1547 | skill instruction (hook-contract.md:520,555,603,720 `observe`/`replay`) + CI (scripts/ci/contracts.py:10 `replay`) | end user + relay host | CRW-156 | port | - | - |
| `plugins/crw/skills/crw-run/scripts/parent_title.py` | 414 | skill instruction (crw-plan integrations.md:118) + CI (scripts/ci/contracts.py:22 `replay`) | end user + relay host | CRW-156 | port | - | - |
| `plugins/crw/skills/crw-run/scripts/start_policy.py` | 289 | skill instruction (start-policy.md:114) + CI (scripts/ci/contracts.py:18 `selftest`) | end user + relay host | CRW-156 | port | - | - |
| `scripts/check_operations_contract.py` | 276 | CI: scripts/ci/contracts.py:12 (operations fixtures replay) | dev/CI | CRW-160 | port | - | - |
| `scripts/ci/contracts.py` | 43 | CI: ci.yml:55 `python3 scripts/ci/contracts.py` | dev/CI | CRW-160 | port | - | - |
| `scripts/ci/gate.py` | 45 | CI: ci.yml:122 required-checks gate | dev/CI | CRW-160 | port | - | - |
| `scripts/ci/packages.py` | 331 | CI: ci.yml:104 per-package pytest (RELAY_CONFORMANCE_REQUIRED=1) | dev/CI | CRW-160 | port | - | - |
| `scripts/ci/plugin.py` | 999 | CI: ci.yml:54 | dev/CI | CRW-160 | port | - | - |
| `scripts/ci/scope.py` | 155 | CI: ci.yml:39 path-scope selection | dev/CI | CRW-160 | port | - | - |
| `scripts/ci/validate.py` | 145 | CI: ci.yml:53 link/metadata validation | dev/CI | CRW-160 | port | - | - |
| `scripts/completion_hook.py` | 53 | user-owner Stop hook: `<CODEX_HOME>/hooks.json` entry `<python> <checkout>/scripts/completion_hook.py` written by runtime_install.py `hook --owner user`; imports crw_runtime.completion | end user + relay host | CRW-156 | retire-with-evidence | `grep -rn completion_hook` over scripts/, docs/, packages/ (consumers: crw_runtime/completion.py:59 ENTRY_POINT_NAME, crw_transition/inventory.py:59/529, docs/runtime-install.md:1511, docs/plugin-transition.md:18) + host scan of `<CODEX_HOME>/hooks.json` | the user-owned registration is retired in todo 38 (`crw install hook` is plugin-owned only and writes no hooks.json entry); deleted in todo 44 after todo 43's retention scan finds no hooks.json entry naming it |
| `scripts/crw_runtime/bridgerecord.py` | 454 | imported by runtime_install, crw_transition.inventory | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/check.py` | 69 | imported by runtime_install, trial_startup | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/codexconfig.py` | 267 | imported by runtime_install, crw_transition.inventory | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/completion.py` | 4561 | imported by completion_hook, runtime_install, stop_events, crw_transition.inventory; spawns the guard (:767) and interpreter probes (:1031, :3458) | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/definition.py` | 206 | imported by runtime_install, trial_startup; spawns `git` (:100) | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/firing.py` | 761 | imported by crw_runtime.completion | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/hooks.py` | 229 | imported by runtime_install, crw_runtime.completion, crw_transition.inventory | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/hostrecord.py` | 607 | imported by trial_startup, crw_runtime.{bridgerecord,completion,hooks,residue,staging} | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/__init__.py` | 6 | package init (docstring only); import target for runtime_install, trial_startup, stop_events, completion_hook | relay host | CRW-157 | retire-with-evidence | `grep -rn 'crw_runtime' scripts/ --include=*.py` (importers listed in the Process graph section) | the package is deleted with its last importer: todo 44 deletes runtime_install.py, completion_hook.py and crw_transition, and the step before todo 48 the dev importers (scripts/port/dump_contracts.py, contract/runner, the Go hook testdata, stop_events.py, trial_startup.py, scripts/ci/tests); `grep -rn crw_runtime scripts/` then returns nothing (scope analysis "# Todo 46") |
| `scripts/crw_runtime/ownership.py` | 143 | imported by runtime_install | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/pointer.py` | 171 | imported by runtime_install, crw_runtime.{completion,residue}, crw_transition.inventory | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/reading.py` | 462 | imported by trial_startup, runtime_install, 10 crw_runtime modules and crw_transition.inventory | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/residue.py` | 324 | imported by runtime_install | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/scope.py` | 324 | imported by runtime_install, crw_runtime.swapgate; spawns the relay CLI (:73) | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/staging.py` | 392 | imported by runtime_install, crw_runtime.residue | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/swapgate.py` | 385 | imported by runtime_install | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/text.py` | 28 | imported by runtime_install (`text_prefix`), crw_runtime.codexconfig, crw_runtime.definition | relay host | CRW-157 | retire-with-evidence | `grep -rn text_prefix` and `grep -rn 'crw_runtime.text'` over scripts/ (consumers: runtime_install.py:32, codexconfig.py, definition.py) | no Go counterpart needed (it names Python `str.startswith` call sites; Go uses `strings.HasPrefix` directly); deleted with the crw_runtime package (todo 44, the step before todo 48 at the latest) |
| `scripts/crw_transition/__init__.py` | 8 | package init; `TRANSITION_VERSION = 1` (no in-repo reader outside the package; grep -rn TRANSITION_VERSION) | relay host | CRW-158 | retire-with-evidence | `git grep -n <name> 53caad67 -- plugins/crw/skills .github/workflows 'scripts/ci/*.py' ':!scripts/ci/tests' docs/port/cutover.md docs/port/control-group.md` gives 0 hits for `crw_transition` and, for `plugin_transition`, only ci.yml:83, :99 and :101, the `tests` leg that ran test_plugin_transition.py (removed in this change); the host (~/.codex) is already a plugin host (settings and bridge record owner `plugin`, no CRW entry in hooks.json or config.toml, no crw-* skill link); remaining importer: runtime_install.py (:5077, :5350, through inventory) | the package goes with runtime_install.py in todo 44 (CRW-141) |
| `scripts/crw_transition/inventory.py` | 1123 | imported by runtime_install (lazy :5077/:5350: `read_plugin` for `register-mcp --execution-policy`); spawns install.py (:454) and interpreter probe (:602), both reached only from the retired transition preflight | relay host | CRW-158 | retire-with-evidence | same search as `scripts/crw_transition/__init__.py`; `read_plugin`'s only remaining consumer is runtime_install.py `_policy_launcher_refusal`, itself not ported (functions table below) | deleted with runtime_install.py in todo 44 (CRW-141) |
| `scripts/install.py` | 98 | operator CLI `python3 scripts/install.py --check` or `--apply` (docs/runtime-install.md:18), the legacy equivalent of `crw-dev skills link` (`internal/dev/skills`, todo 39: destination crw-dev, retired-name `LEGACY` detection dropped); spawned by runtime_install.py:294 and crw_transition/inventory.py:454 | end user + relay host | CRW-158 | port | - | - |
| `scripts/port/check_inventory.py` | 113 | dev CLI `python3 scripts/port/check_inventory.py` (this document's check, todo 1) | dev/CI | CRW-160 | retire-with-evidence | `grep -rn check_inventory` over scripts/, docs/, .github/ (consumers: this document only; no CI job runs it yet) | the Python inventory is obsolete once todo 44 (CRW-141) deletes the product Python; deleted with the remaining dev Python in todo 48 |
| `scripts/port/corpus_notes.py` | 230 | imported by scripts/port/check_corpus_count.py (todo 6) | dev/CI | CRW-160 | retire-with-evidence | `grep -rn corpus_notes` over scripts/, docs/, .github/ (consumer: scripts/port/check_corpus_count.py; no CI job runs it yet) | todo 48 retires the Python corpus checker after the Go tests assume its coverage obligation |
| `scripts/port/check_corpus_count.py` | 208 | dev CLI `python3 scripts/port/check_corpus_count.py` (todo 6) | dev/CI | CRW-160 | retire-with-evidence | `grep -rn check_corpus_count` over scripts/, docs/, .github/ (consumer: todo 6 corpus-coverage validation; no CI job runs it yet) | todo 48 retires the Python corpus checker after the Go tests assume its coverage obligation |
| `scripts/port/check_cutover_doc.py` | 149 | dev CLI `python3 scripts/port/check_cutover_doc.py` (todo 5) | dev/CI | CRW-160 | retire-with-evidence | `grep -rn check_cutover_doc` over scripts/, docs/, .github/ (consumer: todo 5 cutover-document validation; no CI job runs it yet) | todo 48 retires the Python cutover checker with the remaining dev Python |
| `scripts/port/check_test_map.py` | 133 | dev CLI `python3 scripts/port/check_test_map.py` (todo 3) | dev/CI | CRW-160 | retire-with-evidence | `grep -rn check_test_map` over scripts/, docs/, .github/ (consumer: todo 3 test-map validation; no CI job runs it yet) | todo 48 retires the Python test-map checker with the remaining dev Python |
| `scripts/port/dump_contracts.py` | 509 | dev CLI `python3 scripts/port/dump_contracts.py` (todo 2) | dev/CI | CRW-160 | retire-with-evidence | `grep -rn dump_contracts` over scripts/, docs/, .github/ (consumer: todo 2 contract-schema generation/check; no CI job runs it yet) | todo 48 retires the Python contract dumper with the remaining dev Python |
| `scripts/port/make_ledger_fixture.py` | 60 | dev CLI `uv run --no-sync python scripts/port/make_ledger_fixture.py` (todo 14) | dev/CI | CRW-160 | retire-with-evidence | `grep -rn make_ledger_fixture` over scripts/, docs/, .github/ and contract/ (consumer: this inventory and todo-14 ledger fixture regeneration; no product invocation) | todo 48 retires Python fixture generation after the Go ledger test owns the committed Python reference fixture |
| `scripts/port/make_sqlite_fixture.py` | 28 | dev CLI `uv run --no-sync python scripts/port/make_sqlite_fixture.py` (todo 17) | dev/CI | CRW-152 | keep-as-data | - | - |
| `scripts/runtime_install.py` | 5853 | operator CLI `python3 scripts/runtime_install.py <cmd>` (docs/runtime-install.md:19); CI `verify-definition` via scripts/ci/contracts.py:15 | end user + relay host | CRW-158 | port | - | - |
| `scripts/stop_events.py` | 59 | dev CLI `python3 scripts/stop_events.py --journal-root ...` (docs/runtime-install.md, Reading it back); ported as `crw-dev stop-events` (`internal/dev/stopevents`, todo 46), and kept until todo 44 because its tests protect the Python adapter | dev/CI | CRW-159 | port | - | - |
| `scripts/trial_startup.py` | 3734 | dev CLI `python3 scripts/trial_startup.py {preflight,ledger} --start` (docs/live-trial.md). Ported in part: `ledger` is `crw-dev trial-ledger` (`internal/dev/trialledger`, todo 46), which reads the trial root and spawns nothing; `preflight` spawns git + relay (:1302) and reads the Python install (consoleScript, the v1 host record), so it gets no port and retires with the Python install in todo 44 | dev/CI | CRW-159 | port | - | - |

### Files deleted with evidence

Deleted files leave the table above, which lists only files that exist. Their evidence stays here.

| path | lines | invoked | owner | consumer_search | removed by |
| --- | --- | --- | --- | --- | --- |
| `plugins/crw/wiring/crw_bridge_mcp.py` | 239 | MCP: the pre-native mcp.json `python3 ./wiring/crw_bridge_mcp.py` (cwd plugin root); exec'd the pointer-named bridge (:230-232). No declaration names it since todo 34 switched mcp.json to `sh ./wiring/crw-bridge.sh` and ported its record contract to `codex-thread-bridge --plugin-launch` (internal/pluginwiring, decision 26) | CRW-156 | `git grep -l crw_bridge_mcp 099d2d7e` (origin/dev at the cutover commit): no declaration in plugins/ names it; the file readers were tests and dev tools (internal/pluginwiring launch_test, internal/runtime/doctor registration_parity_test, internal/runtime/install wiring_test and rollback_test, codex-thread-bridge tests/test_mcp.py, scripts/ci/tests/test_plugin_wiring.py, contract/runner/mcp.py) and runtime_install.py's `register-mcp` probe reference (`PLUGIN_LAUNCH_REFERENCE`), and each now reads the byte-identical copy in `internal/pluginwiring/testdata/pre-native-wiring`; the rest are the pre-native testdata declaration, the doctor's retention reading, comments and docs. A cached pre-native server declaration names the version directory it was loaded from, not a later payload's (docs/port/cutover.md Retention) | todo 43, after the cutover commit (`chore(plugin): retire legacy Python launchers after cutover commit`), which re-recorded the payload version; the copy in `internal/pluginwiring/testdata/pre-native-wiring` is the record contract's oracle and goes with the Python tests in todo 44 |
| `plugins/crw/wiring/crw_stop_hook.py` | 156 | Stop hook: the pre-native `python3 -c <bootstrap>` (hooks/stop-recording-completion.json:8, which `internal/pluginwiring/testdata/pre-native-wiring` keeps) exec'd it, or its copy `<CODEX_HOME>/crw-stop-hook.py`; spawns `adapterInterpreter adapterEntryPoint` (:141). No declaration names it since todo 34 changed the hook command to `crw hook --plugin-launch; exit 0` (decision 26) | CRW-156 | `git grep -l crw_stop_hook 099d2d7e`: no declaration in plugins/ names it; the file readers were tests and dev tools (internal/relay/hook pluginlaunch_test, internal/runtime/install launcher_test, wiring_test and rollback_test, internal/runtime/integration IS-7, scripts/ci/tests/test_plugin_wiring.py, scripts/port/dump_contracts.py) and the Python installer's shim source (crw_runtime/completion.py `LAUNCHER_SOURCE`, used by runtime_install.py `hook` and `hook-status`), and each now reads the byte-identical copy in `internal/pluginwiring/testdata/pre-native-wiring`; the rest are the pre-native testdata declaration, the doctor's retention reading (`launcherEntries`), comments and docs. A cached bootstrap names the pre-native version directory, which the install that brought the native wiring removed, so it reaches the host copy and never a later payload's | todo 43, after the cutover commit, with crw_bridge_mcp.py. The host's `<CODEX_HOME>/crw-stop-hook.py` copy is not package content: the operator removes it once the retention scan reports no reference that could still reach it (docs/port/cutover.md Retention) |
| `scripts/plugin_transition.py` | 398 | operator CLI `python3 scripts/plugin_transition.py {inspect, check-declaration, transition, disable, remove, swap-state}` (docs/plugin-transition.md); `transition`, `disable` and `remove` refused since decision 26 (todo 34) | CRW-158 | `git grep -n <name> 53caad67 -- plugins/crw/skills .github/workflows 'scripts/ci/*.py' ':!scripts/ci/tests' docs/port/cutover.md docs/port/control-group.md` gives 0 hits for `crw_transition` and, for `plugin_transition`, only ci.yml:83, :99 and :101, the `tests` leg that ran test_plugin_transition.py (removed in this change); only its own tests and test_plugin_wiring.py parsed it. The host is already a plugin host (settings and bridge record owner `plugin`, no CRW entry in hooks.json or config.toml, no crw-* skill link), and `crw install` writes only plugin-owned registrations | todo 39, which deletes it with its tests (scope analysis "# 39"); a host that still needs it runs it from a revision before decision 26, where `transition` still runs (docs/plugin-transition.md) |
| `scripts/crw_transition/steps.py` | 2704 | imported by plugin_transition only; spawned scripts/ci/plugin.py (:562) | CRW-158 | same search. `launcher_remove` (the ownership-checked removal of `<CODEX_HOME>/crw-stop-hook.py`) is still needed by todo 43 and is ported as `install.RemoveLauncher`, not a command | todo 39 |
| `scripts/hook_comparison.py` | 2774 | dev CLI `python3 scripts/hook_comparison.py --root <dir> [--require-witness]` (docs/hook-comparison.md, deleted with it); spawned runtime_install.py `hook`, the relay and a tracer | CRW-159 | `git grep` on origin/dev and the todo 30-36 tree finds no consumer in plugins/, the wiring, internal/, cmd/, .github/, scripts/ci/*.py, contract/ or scripts/port/; neither docs/port/control-group.md nor docs/port/cutover.md needs it, and no adoption decision rests on it (hook-comparison.md:384-414). Its only outputs on the relay host are CRW-103 development evidence. Its foreign-registration property (HKC-5/31) is the installer's (`test_completion_hook.py`, todos 34, 38, 40); the hook decision table is the decisions fixtures' and the todo 33 Go tests'. The skill reference that said the harness was in the checkout (crw-plan design-explanation.md) now points to the hook contract's criteria | todo 46 (scope analysis "# Todo 46"), with scripts/ci/tests/test_hook_comparison.py and docs/hook-comparison.md |

## Process-spawn graph

Every `subprocess`, `exec*`, `create_subprocess_exec` and `sys.executable` site in the files
above, found with
`grep -nE 'subprocess\.(run|Popen|call|check_call|check_output)\(|os\.exec[lv]p?e?\(|sys\.executable|create_subprocess_(exec|shell)\(|os\.(system|spawn[lv]p?e?|fork|posix_spawnp?)\('`
(69 sites before this inventory; `scripts/port/check_inventory.py` spawns nothing). "Python" in the target column means the spawn needs a Python interpreter and must
disappear from the product path.

### Product path (end user + relay host)

| site | spawns | target | Go replacement |
| --- | --- | --- | --- |
| `plugins/crw/wiring/hooks/stop-recording-completion.json:8` | host runs `python3 -c <bootstrap>` which `exec`s `crw_stop_hook.py` | Python | `"$HOME/.local/share/crw-runtime/current/bin/crw" hook --plugin-launch; exit 0` shell string (todo 34, decision 26); a turn that cached the bootstrap may still run it, and since todo 43 retired the packaged launcher it reaches only the `<CODEX_HOME>/crw-stop-hook.py` copy. Both run from an installed cache, before and after it is replaced, in `internal/runtime/integration` (todo 40) |
| `plugins/crw/wiring/mcp.json:4-6` | host runs `python3 ./wiring/crw_bridge_mcp.py` | Python | `sh ./wiring/crw-bridge.sh`, which execs `current/bin/codex-thread-bridge --plugin-launch` (todo 34, decision 26); started with HOME alone from an installed cache in `internal/runtime/integration` (todo 40) |
| `plugins/crw/wiring/crw_stop_hook.py:141` (retired from the package in todo 43; the host's `<CODEX_HOME>/crw-stop-hook.py` copy runs it until the operator removes that) | `adapterInterpreter adapterEntryPoint <settings>` = stopadapter.py | Python | kept for the IS-7 window: the settings todo 38 writes name `/usr/bin/env` and `current/bin/crw-completion-hook`, so a cached bootstrap reaches the Go hook (`TestLegacyStopLaunchersReachTheGoHook`, and from an installed cache through the packaged launcher and the `<CODEX_HOME>/crw-stop-hook.py` copy in `internal/runtime/integration`, todo 40, both from the pre-native testdata since todo 43); the native command needs no such spawn, `crw hook` is the adapter |
| `plugins/crw/wiring/crw_bridge_mcp.py:230` (retired from the package in todo 43) | `os.execv` of the pointer-named `codex-thread-bridge` | Python console script until todo 38, then Go | `codex-thread-bridge --plugin-launch` reads the record and execs its own executable as the bridge (internal/pluginwiring, decision 26); a session that cached the older declaration still runs this, reaching the Go bridge the record names |
| `plugins/crw/wiring/crw_bridge_mcp.py:232` (retired from the package in todo 43) | `os.execve` of the same, with execution-policy env | Python console script until todo 38, then Go | the same exec with the policy variables added (decision 26) |
| `packages/codex-session-relay/src/codex_session_relay/stopadapter.py:645` | relay CLI `guard --socket --db-path --mode` | Python console script | in-process guard or `S/control.sock` (todo 33) |
| `packages/codex-session-relay/src/codex_session_relay/service.py:1728` | argv `[sys.executable, -m, codex_session_relay.cli, ...]` (supervisor) | Python self re-exec | `os.Executable()` (todo 29) |
| `packages/codex-session-relay/src/codex_session_relay/service.py:1789` | `Popen` of the :1728 argv, detached, log to `daemon.log` | Python self re-exec | same |
| `packages/codex-session-relay/src/codex_session_relay/service.py:1827` | argv `[sys.executable, -m, codex_session_relay.cli, ...]` (worker) | Python self re-exec | `os.Executable()` (todo 29) |
| `packages/codex-session-relay/src/codex_session_relay/service.py:1847` | `Popen` of the :1827 argv | Python self re-exec | same |
| `packages/codex-session-relay/src/codex_session_relay/supervisorchannel.py:206` | returns `(sys.executable, -m, codex_session_relay.cli)` as the relay launcher written into delivered lines | Python self reference | `crw relay` path (todo 24) |
| `packages/codex-session-relay/src/codex_session_relay/cli.py:5689` | prints `sys.executable -m codex_session_relay.cli` as a hint | Python self reference | `os.Executable()` (todo 8/20) |
| `packages/codex-session-relay/src/codex_session_relay/forge.py:160` | `gh` | non-Python, stays | `exec.Command("gh")` (todo 24) |
| `packages/codex-session-relay/src/codex_session_relay/mergetarget.py:95` | `git` | non-Python, stays | `exec.Command("git")` (todo 26) |
| `packages/codex-thread-bridge/src/codex_thread_bridge/worktrees.py:20` | `git worktree add ...` | non-Python, stays | `exec.Command("git")` (todo 15) |

### Installer and diagnosis (end user + relay host, operator-run)

| site | spawns | target | Go replacement |
| --- | --- | --- | --- |
| `scripts/runtime_install.py:240` | `sys.executable` named in `installedBy` | Python self reference | the crw binary's own path (`internal/runtime/doctor` actingProcess); `crw install` records the issue and the archive, never an interpreter (todo 38) |
| `scripts/runtime_install.py:294` | argv `[sys.executable, scripts/install.py, --check, --dest]` | Python | retired (scope analysis "# 39"): the Go runtime reads no skill links; a developer checks them with `crw-dev skills link --check` (todo 39) |
| `scripts/runtime_install.py:296` | runs the :294 argv | Python | same |
| `scripts/runtime_install.py:528` | `_asked`: `<interpreter> -c <store program>` (store presence/tables, :560-597) | Python | an lstat of the relay's resolved selection and a read-only catalog read that creates no sidecar (`internal/runtime/swapgate` `StorePresence`/`StoreSchema`, decision 36); the candidate's schema from the candidate binary's `crw doctor declared-schema --json` (todo 37) |
| `scripts/runtime_install.py:639` | `<python> -c "import platform;print(platform.python_version())"` version probe | Python | retired (todo 37): Go points have no interpreter dimension (`internal/runtime/record.Dimensions`) |
| `scripts/runtime_install.py:657` | `<python> -c "import <module>"` module location probe | Python | retired (todo 37): `crw doctor` has no `imported` field; the selected runtime kind comes from the pointer target, pyvenv.cfg and the record |
| `scripts/runtime_install.py:858` | `sys.executable` default interpreter for classification | Python | retired (todo 37) |
| `scripts/runtime_install.py:1089` | `sys.executable` fallback for `observe_app_server` | Python | retired (todo 37) |
| `scripts/runtime_install.py:1429` | `<python> check_connection.py --socket` | Python | the App Server dimension is observed through the bridge's own `get_capabilities` over MCP: by the install exercise and by `crw doctor` through the selected bridge (`internal/runtime/exercise`, todo 38) |
| `scripts/runtime_install.py:1456` | `codex --version` | non-Python, stays | `exec.Command("codex")` in `internal/runtime/doctor` (todo 37) |
| `scripts/runtime_install.py:1554` | comment naming the `sys.executable` probe it avoids | none (comment) | - |
| `scripts/runtime_install.py:1637` | `<interpreter> -B -c <settings program>` | Python probe | not ported: `diagnose --trial` is deferred past todo 44 (scope analysis "# 37"); retired with runtime_install.py |
| `scripts/runtime_install.py:1690` | `<interpreter> -B -c <predicate program>` | Python probe | not ported: `diagnose --trial` (deferred past todo 44) |
| `scripts/runtime_install.py:1815` | `<interpreter> -B -c <normalize probe>` | Python probe | not ported: `diagnose --trial` (deferred past todo 44) |
| `scripts/runtime_install.py:1862` | `<interpreter> -B -c <contains probe>` | Python probe | not ported: `diagnose --trial` (deferred past todo 44) |
| `scripts/runtime_install.py:1903` | `<interpreter> -B -c <admits probe>` | Python probe | not ported: `diagnose --trial` (deferred past todo 44) |
| `scripts/runtime_install.py:2327` | `sys.executable` default for `hook --python` | Python | retired (todo 38) |
| `scripts/runtime_install.py:2686` | `perform`: `<interpreter> -m venv` (:2906), `<python> -m pip install` (:2911) | Python venv/pip | the SHA256SUMS-verified archive unpacked from the verified bytes (`internal/runtime/install`, todo 38) |
| `scripts/runtime_install.py:4434` | `<python> -c "import sys;print(sys.prefix)"` | Python probe | retired (todo 37): a Go install's environment is the directory holding bin/crw, named in its install entry |
| `scripts/runtime_install.py:4539` | `<python> check_connection.py` (measure) | Python | the candidate's `bin/codex-thread-bridge` MCP session: tools/list and get_capabilities (`internal/runtime/exercise`, todo 38) |
| `scripts/runtime_install.py:4641` | `sys.executable` default for `measure --python` | Python | retired (todo 38) |
| `scripts/runtime_install.py:4893` | `sys.executable` written as the probe record's command | Python | retired (todo 38) with the launcher-predates-policy probe it belongs to |
| `scripts/runtime_install.py:4899` | launcher probe under the Desktop environment | declared launcher (Python today) | retired (todo 38): a Go launcher reads the record itself (todo 34 `crw bridge --plugin-launch`), so no launcher predates the policy field |
| `scripts/crw_runtime/codexconfig.py:121` | `sys.executable` in a version-refusal message | Python self reference | retired (todo 37) |
| `scripts/crw_runtime/completion.py:767` | `invoke_guard`: relay `guard` subcommand | Python console script | `crw relay guard-evaluate` (todo 37) |
| `scripts/crw_runtime/completion.py:1031` | `_require_python`: `<candidate> -c "print(sys.version_info)"` | Python probe | retired (todo 37) |
| `scripts/crw_runtime/completion.py:3110` | `<relay> guard --help` | Python console script | `crw relay guard-evaluate --help` (todo 37) |
| `scripts/crw_runtime/completion.py:3458` | `_answers_as_an_interpreter`: `<resolved> -c <source>` | Python probe | kept until todos 43/44: hook status still asks it while a registration or settings record may name a Python interpreter (already ported, `internal/relay/hook/status.go` `answersPython`, todo 33) |
| `scripts/crw_runtime/definition.py:100` | `git` | non-Python, stays | retired (todo 37): no git at run time; the build stamps the source tree (decision 34) and the definition keeps no derivable digest (decision 35) |
| `scripts/crw_runtime/scope.py:73` | relay CLI `doctor` (discovery, selected state and root candidate, :114-126) | Python console script | a subprocess of the SELECTED relay executable, Python until the cutover and Go after (`internal/runtime/scope.Relay`, todo 37): the live store refuses in-process opens before todo 42 |
| `scripts/crw_transition/inventory.py:454` | argv `[sys.executable, scripts/install.py, --check, --dest]` | Python | retired (todo 39) with the transition preflight, its only caller |
| `scripts/crw_transition/inventory.py:459` | runs the :454 argv | Python | same |
| `scripts/crw_transition/inventory.py:602` | `_evaluates_python`: `<candidate> -c <arith program>` | Python probe | retired (todo 39) with the transition preflight, its only caller |
| `scripts/crw_transition/steps.py:562` | argv `[sys.executable, scripts/ci/plugin.py, --payload]` | Python | retired (todo 39): steps.py is deleted; the payload check is `crw-dev ci plugin --payload` run by hand |
| `scripts/crw_transition/steps.py:565` | runs the :562 argv | Python | same |

### Dev and CI only (dev/CI; CRW-159, CRW-160)

| site | spawns | target |
| --- | --- | --- |
| `packages/codex-thread-bridge/scripts/check_connection.py:18` | MCP stdio client starting `sys.executable -m codex_thread_bridge.server` | Python |
| `scripts/ci/contracts.py:38` | `sys.executable <skill/contract script> <args>` | Python |
| `scripts/ci/packages.py:53` | `run()` helper: `uv sync` (:179), `uv run --no-sync python -m pytest` (:162), `uv run --no-sync python -c` (:110) | Python |
| `scripts/ci/plugin.py:94` | `git` | non-Python |
| `scripts/ci/plugin.py:118` | `git cat-file --batch` | non-Python |
| `scripts/ci/scope.py:85` | `git` | non-Python |
| `scripts/ci/validate.py:113` | `git ls-files` | non-Python |
| `scripts/trial_startup.py:1302` | `run`: `git` (:1370) and relay CLI argv (:1363) | mixed |

## Python-interpreter probe sites to retire

These ask a candidate interpreter to run Python code. They exist only because the runtime is a
venv; the Go runtime has no interpreter to probe, so each is retired, not ported.

| site | question it answers | retired by |
| --- | --- | --- |
| `scripts/runtime_install.py:1637` | does the installed relay accept these settings (`-B -c <settings program>`) | not ported: `diagnose --trial`, deferred past todo 44 (retired with runtime_install.py) |
| `scripts/runtime_install.py:1690` | does the installed relay admit this predicate (`-B -c <predicate program>`) | not ported: `diagnose --trial`, deferred past todo 44 |
| `scripts/runtime_install.py:4434` | which environment the interpreter reports (`sys.prefix`) | todo 37 (the install entry names the environment holding bin/crw) |
| `scripts/crw_runtime/completion.py:3458` | does the hook's interpreter word answer as a Python | todos 43/44 (kept while a record may name a Python interpreter; ported by todo 33) |
| `scripts/crw_transition/inventory.py:602` | does the candidate evaluate Python | todo 39 |

Siblings of the same kind, found by the spawn grep: `scripts/runtime_install.py:528` (store
programs), `:639` (version), `:657` (module location), `:1815`, `:1862`, `:1903` (path and
admission probes), `scripts/crw_runtime/completion.py:1031` (`_require_python`). The store programs and the
version and module probes retire in todo 37; the path and admission probes belong to
`diagnose --trial`, deferred past todo 44; `_require_python` retires with the Go `crw install
hook` (todo 38), which never registers an interpreter.

## In-process edges that force one binary

- The relay imports the bridge: `bridge_adapter.py:875-900` (ledger, rpc.AppServer,
  bridge.Bridge), `managed.py:66` (settings), `rolepolicy.py:176` (execution); the bridge
  `settings` module is also imported by relay assignment, cli, fakehost, faultsweep, packets,
  receiver and registry. The bridge cannot ship as a separate process from the relay.
- The daemon re-launches its own interpreter (`service.py:1728`, `:1827`,
  `supervisorchannel.py:206`), so the relay CLI and daemon are one program.
- `scripts/runtime_install.py`, `trial_startup.py`, `stop_events.py` and
  `completion_hook.py` all import `crw_runtime`; the package outlives the product Python until
  its last importer goes (todo 44 for the product and installer, the step before todo 48 for
  the dev tools). Todo 46 ported the two dev readings that import it, the per-event Stop judge
  (`crw-dev stop-events`) and the trial ledger (`crw-dev trial-ledger`), and deleted
  `hook_comparison.py`.

## Functions retired with evidence

Functions inside ported files that get no Go equivalent. The file itself is still `port`; only
these definitions are not carried over. Same evidence rule as a retire row: the consumer
search, and the trigger after which the Python definition may go.

| function | why no port | consumer_search | removal_trigger |
| --- | --- | --- | --- |
| `packages/codex-session-relay/src/codex_session_relay/intent.py:930` `dispatch_is_registered` | collapses `dispatch_generation_state` to a boolean for a caller that never came; the detailed answer is ported (`internal/relay/delivery/intent.go`, `DispatchCurrent`) and is what the tests exercise | `grep -rn dispatch_is_registered` over packages/ (src and tests), scripts/, plugins/, docs/ and internal/ at 3684949c: the definition only, 0 callers, 0 tests | deleted with intent.py in todo 44 (CRW-141) |
| `packages/codex-session-relay/src/codex_session_relay/marker.py:71` `_checked_segment` | a raising wrapper over `valid_segment` nothing calls; every marker path check goes through `valid_segment` directly, ported as `delivery.ValidSegment` | `grep -rn _checked_segment` over packages/ (src and tests), scripts/, plugins/, docs/ and internal/ at 3684949c: the definition only, 0 callers, 0 tests | deleted with marker.py in todo 44 (CRW-141) |
| `scripts/crw_runtime/reading.py:217` `where`, `:231` `failure`, `:242` `region` and the exception lattice (`SHAPE_FAILURES`, `READ_FAILURES`, `RESOLVE_FAILURES`, :72-88) | Python's traceback locator and exception classification; the Go reading (`internal/runtime/reading`) keeps the four states and the refusal shape, names the Python exception class a reader would have seen, and reports `raisedAt` null | `grep -rn 'reading.where\|reading.region\|READ_FAILURES'` over scripts/ at 07caf101: runtime_install.py, hook_comparison.py, trial_startup.py, crw_runtime/completion.py and crw_runtime/* only | deleted with the crw_runtime package (todo 44, the step before todo 48 at the latest) |
| `scripts/crw_runtime/definition.py:92` `git`, `:108` `working_tree_clean`, `:125` `pyproject_fields`, `:148` `verify` and `scripts/runtime_install.py:269` `cmd_verify_definition` | re-derive the Python-shaped definition from a checkout; the Go definition keeps no derivable digest (decision 35) and a Go install's source revision is stamped at build (decision 34) | `grep -rn 'pyproject_fields\|definition.verify\|working_tree_clean\|definition.git'` over scripts/ at 07caf101: definition.py, ownership.py and runtime_install.py only; CI's `verify-definition` step (scripts/ci/contracts.py; since todo 44 internal/dev/ci/contracts.go checks only the fields the Go definition retains, and needs no Python) | deleted with runtime_install.py in todo 44 |
| `scripts/runtime_install.py:525` `_asked`, `:568` `store_presence`, `:579` `store_tables`, `:594` `candidate_tables` and their `-c` programs | interpreter probes; replaced by the Go reads of decision 36 | `grep -rn 'store_presence\|store_tables\|candidate_tables'` over scripts/ at 07caf101: runtime_install.py and crw_runtime/swapgate.py (a comment) | deleted with runtime_install.py in todo 44 |
| `scripts/crw_runtime/staging.py:71` `RECORDED` and its branch of `decide` | adopted a populated, claimless directory the record selects (installs made before claims); every env-* directory on the relay host carries a claim, so Go decides FOREIGN there | `ls ~/.local/share/crw-runtime/env-*/.crw-staging-claim.json` on the relay host 2026-09-29: 14 of 14 | deleted with staging.py and the crw_runtime package (todo 44, the step before todo 48 at the latest) |
| `scripts/crw_runtime/hostrecord.py:452` `component_facts` and `outgoing` deltas of `update` | component-level facts feed only the informational `repositoryCommitDrift`; `outgoing` has no reader | `grep -rn 'component_facts\|outgoing'` over scripts/ at 07caf101: hostrecord.py and runtime_install.py writers, and their tests | deleted with runtime_install.py in todo 44 |
| `scripts/crw_runtime/ownership.py:78` `commit_matches`, `tree_matches`, `working_tree_clean` signals | a Go install is an archive, not a checkout: `commit_matches` was never filled and the tree signals describe a checkout (the revision is recorded in the entry's `source`) | `grep -rn commit_matches` over scripts/ at 07caf101: ownership.py and runtime_install.py only | deleted with ownership.py and the crw_runtime package (todo 44, the step before todo 48 at the latest) |
| `scripts/crw_runtime/check.py:14` field `imported` | the location an interpreter imports a package from; a binary has no import | `grep -rn '"imported"\|importedLocation'` over scripts/ at 07caf101: check.py and runtime_install.py, and their tests | deleted with check.py and the crw_runtime package (todo 44, the step before todo 48 at the latest) |
| `scripts/runtime_install.py:4627` `cmd_measure` (the `measure` subcommand) | defer-post-44: the install exercise records the point (OPS-2.4) and `crw doctor` compares it; a standalone re-measure has no caller | `grep -rn 'cmd_measure\|runtime_install.py measure'` over scripts/, plugins/, docs/, packages/, .github/ and internal/ at 1e3583c3: runtime_install.py and docs/runtime-install.md only | deleted with runtime_install.py in todo 44; a Go `crw install measure` is decided after it |
| `scripts/runtime_install.py:5334` `register-mcp --owner user` and `scripts/crw_runtime/codexconfig.py:217` `register` (the config.toml append), `python_needed`, `cross_check` | the plugin declares the bridge; the Go installer writes no config.toml. `render`, `quote` and `key` were kept for todo 39's transition, which retired instead of being ported, so they get no Go port either | `grep -rn 'codexconfig.register\|python_needed\|cross_check'` over scripts/, plugins/, docs/ at 1e3583c3: runtime_install.py, codexconfig.py and scripts/ci/tests only | deleted with runtime_install.py (todo 44) and codexconfig.py (with the crw_runtime package: todo 44, the step before todo 48 at the latest) |
| `scripts/runtime_install.py:2300` `hook --adapter completion --owner user`, `scripts/crw_runtime/completion.py:1105` `duplicate_complaints`, `scripts/crw_runtime/hooks.py:131` `install` (the hooks.json append) | the plugin declares the Stop registration; `crw install hook` writes settings only. `completion.command_for` was kept for todo 39's transition, which retired instead of being ported, so it gets no Go port either | `grep -rn 'duplicate_complaints\|hooks.install'` over scripts/, plugins/, docs/ at 1e3583c3: runtime_install.py, completion.py, hook_comparison.py (dev, todo 46) and scripts/ci/tests only | deleted with runtime_install.py in todo 44 |
| `scripts/runtime_install.py:2300` `hook --hook-command` (an explicit SessionStart hook) | no product caller | `grep -rn -- '--hook-command'` over scripts/, plugins/, docs/ at 1e3583c3: runtime_install.py, completion.py (a message), docs/runtime-install.md and scripts/ci/tests only | deleted with runtime_install.py in todo 44 |
| `scripts/crw_runtime/hooks.py:212` `disable` | no caller | `grep -rn 'hooks.disable'` over scripts/ at 1e3583c3: none | deleted with hooks.py and the crw_runtime package (todo 44, the step before todo 48 at the latest) |
| `scripts/runtime_install.py:3839` `_inherited_registration` | adopted a pre-pointer config.toml registration naming a concrete env path as this command's own; the Go promotion refuses a `codex-thread-bridge` table that does not name the pointer instead | `grep -rn _inherited_registration` over scripts/ at 1e3583c3: runtime_install.py and scripts/ci/tests only | deleted with runtime_install.py in todo 44 |
| `scripts/runtime_install.py:5048` `_policy_launcher_refusal`, `:4923` `_launcher_honours_policy_records`, `:4886` `_probe_launcher_once`, `:5013` `_declared_start` (the launcher-predates-policy probe) | asked whether a cached Python launcher predated the policy field; the Go launcher (todo 34) reads the record itself | `grep -rn '_policy_launcher_refusal\|LAUNCHER_PREDATES_POLICY'` over scripts/, plugins/, docs/ at 1e3583c3: runtime_install.py only | deleted with runtime_install.py in todo 44 |
| `scripts/runtime_install.py:1959` `_trial` and `diagnose --trial`, `--assignment-lookup`, `--observed-tool`, `--expect-relationship`, `--temporary` | defer-post-44: the read-only diagnosis is `crw doctor` (todo 37); the trial creates work and its only reader is docs/live-trial.md, which disowns it | `grep -rn -- '--trial\|--assignment-lookup\|--observed-tool\|--expect-relationship'` over plugins/, docs/, scripts/ at 1e3583c3: runtime_install.py, docs/runtime-install.md, docs/live-trial.md and scripts/ci/tests only | deleted with runtime_install.py in todo 44 |
| `scripts/crw_runtime/completion.py:3045` `launcher_state` `matchesCheckout` (hook-status) and `:2949` `place_launcher` for a Go install | a Go install places no `<CODEX_HOME>/crw-stop-hook.py`; the host's copy stays as it is until todo 43's retention scan, and its settings reach the Go hook through `/usr/bin/env` (decision 18) | `grep -rn 'matchesCheckout\|place_launcher'` over scripts/, plugins/, docs/ at 1e3583c3: runtime_install.py, completion.py, crw_transition/steps.py (todo 39) and scripts/ci/tests only | deleted with runtime_install.py (todo 44) and completion.py (with the crw_runtime package: todo 44, the step before todo 48 at the latest) |

Marker-root resolution exists twice in Python: the relay's (`marker.resolve_marker_root`,
`Path.absolute`, ported as `internal/relay/delivery.ResolveMarkerRoot`) and the installer's copy
in `scripts/crw_runtime/completion.py` (`os.path.abspath`, which folds `..`). They disagree on a
root spelled with `..` after a symlink, so a hook could look under a different tree than the
coordinator publishes to. Todo 38 reuses `delivery.ResolveMarkerRoot` rather than porting the
installer's copy.

## Summary

| owner | files |
| --- | --- |
| CRW-150 | 4 |
| CRW-151 | 11 |
| CRW-152 | 9 |
| CRW-153 | 35 |
| CRW-154 | 19 |
| CRW-155 | 2 |
| CRW-156 | 6 |
| CRW-157 | 17 |
| CRW-158 | 4 |
| CRW-159 | 2 |
| CRW-160 | 14 |
