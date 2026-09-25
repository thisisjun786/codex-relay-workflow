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

Total non-test lines: 99396

| path | lines | invoked | runs | owner | disposition | consumer_search | removal_trigger |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `packages/codex-session-relay/src/codex_session_relay/ack.py` | 1094 | imported by cli, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/admission.py` | 205 | imported by cli, daemon, managed, omitted, receipts | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/assignment.py` | 1163 | imported by cli, delivery, dispositions, faultsweep, linkage, reconcile | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/bridge_adapter.py` | 1318 | imported by cli; imports the bridge in-process (:875-900) | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/capacity.py` | 594 | imported by cli | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/cli.py` | 5840 | console script `codex-session-relay` (cli:main); `python -m codex_session_relay.cli` re-exec target of service.py:1680/1779 and supervisorchannel.py:205; run by skills, stopadapter guard call, crw_runtime.scope | end user + relay host | CRW-150 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/clock.py` | 34 | imported by cli | relay host | CRW-152 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/completion.py` | 518 | imported by routing | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/coordination.py` | 129 | imported by capacity, editregion, mergeturn | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/criteria.py` | 481 | imported by ack, assignment, cli, managed, receiver, report | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/currency.py` | 247 | imported by ack, assignment, cli, delivery, guard, receipts, reconcile | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/cxc.py` | 432 | imported by packets, report, supervision | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/daemon.py` | 1333 | imported by cli, service; runs inside the spawned daemon worker | relay host | CRW-155 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/declarations.py` | 208 | imported by cli | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/delivery.py` | 2746 | imported by ack, assignment, cli, daemon, faultsweep, hostloss, reconcile, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/digest.py` | 135 | imported by routing | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/dispositions.py` | 657 | imported by cli | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/editregion.py` | 1511 | imported by cli | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/envelope.py` | 670 | imported by cli, delivery, linkage, packets, receiver, report, supervision, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/errors.py` | 272 | imported by 33 relay modules (RefusalReason, exit-2 envelope) | relay host | CRW-150 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/fakehost.py` | 309 | imported by relay tests (support.py, test_bridge_adapter, test_daemon_cadence, ...); ships in src but no product module imports it | dev/CI | CRW-150 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/faultnotice.py` | 312 | imported by daemon | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/faults.py` | 3882 | imported by cli, daemon, faultnotice, faultsweep, ledger_port, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/faultsweep.py` | 1656 | imported by cli, daemon, delivery | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/forge.py` | 1222 | imported by cli, mergetarget; spawns `gh` (:160) | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/guard.py` | 1123 | imported by cli (`guard`/`guard-evaluate`), omitted | end user + relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/hostadapter.py` | 322 | imported by bridge_adapter, cli, fakehost, hostloss, reconcile | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/hostloss.py` | 604 | imported by daemon, reconcile | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/identity.py` | 184 | imported by 17 relay modules | relay host | CRW-152 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/__init__.py` | 11 | package init; `__version__`, `NO_DELIVERABLE` imported by 13 modules (faultsweep reads `__version__`) | relay host | CRW-150 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/intake.py` | 646 | imported by digest, routing | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/intent.py` | 1342 | imported by cli, faultnotice, guard, managed, omitted, supervision; `registration_hold()` opens the DB directly (:808) | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/ledger_port.py` | 325 | imported by completion, digest, intake, projects, routes, routing | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/lifecycle.py` | 131 | imported by delivery, faultnotice, managed, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/linkage.py` | 2448 | imported by assignment, cli, receiver, registry, rolepolicy | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/managed.py` | 707 | imported by cli; imports bridge settings (:66) | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/manifest.py` | 363 | imported by cli, guard, receipts | relay host | CRW-154 | port | - | - |
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
| `packages/codex-session-relay/src/codex_session_relay/service.py` | 2143 | imported by cli (`daemon`, `service ...`); spawns the daemon worker | relay host | CRW-155 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/settings.py` | 866 | imported by assignment, bridge_adapter, cli, fakehost, faultsweep, managed, packets, receiver, registry | relay host | CRW-154 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/stopadapter.py` | 1209 | console script `crw-completion-hook` (stopadapter:main); started by the plugin launcher crw_stop_hook.py via `adapterEntryPoint` at every Stop | end user + relay host | CRW-156 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/store.py` | 2855 | imported by bridge_adapter, cli, declarations, dispositions, omitted, service, supervisorchannel | relay host | CRW-152 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/supervision.py` | 743 | imported by cli, delivery, faultnotice, faults, supervisorchannel | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/supervisorchannel.py` | 3257 | imported by assignment, cli, daemon, faultnotice, faults; renders the relay launcher tuple (:205) | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/sync.py` | 899 | imported by cli, faults, receiver, supervision | relay host | CRW-153 | port | - | - |
| `packages/codex-session-relay/src/codex_session_relay/transport.py` | 198 | imported by 13 relay modules (ack, delivery, reconcile, ...) | relay host | CRW-154 | port | - | - |
| `packages/codex-thread-bridge/scripts/check_connection.py` | 60 | components.json `exerciseScript`; spawned by runtime_install.py `measure` (:4539) and `observe_app_server` (:1429) | relay host | CRW-151 | retire-with-evidence | `grep -rn check_connection` over scripts/, docs/, packages/*/README.md, scripts/crw_runtime/components.json (consumers: components.json:19, runtime_install.py:1429/4539, docs/runtime-install.md:81/1336, packages/codex-thread-bridge/README.md:635) | todo 37 components definition v2 replaces `exerciseScript` with `exerciseCommand` (`crw bridge` MCP `tools/list`); file deleted in todo 44 (CRW-141) |
| `packages/codex-thread-bridge/src/codex_thread_bridge/bridge.py` | 1580 | imported by server, relay bridge_adapter | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/effects.py` | 78 | imported by bridge, rpc, worktrees | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/execution.py` | 606 | imported by bridge, server, relay rolepolicy | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/__init__.py` | 3 | package init; `__version__` read by rpc.py:302 (clientInfo) and server.py:397 | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/ledger.py` | 177 | imported by bridge, server, relay bridge_adapter | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/roles.py` | 152 | imported by bridge, execution | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/rpc.py` | 546 | imported by bridge, server, relay bridge_adapter | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/server.py` | 424 | console script `codex-thread-bridge` (server:main); MCP stdio server exec'd by plugins/crw/wiring/crw_bridge_mcp.py (mcp.json) | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/settings.py` | 574 | imported by bridge; relay assignment, bridge_adapter, cli, fakehost, faultsweep, managed, packets, receiver, registry | end user + relay host | CRW-151 | port | - | - |
| `packages/codex-thread-bridge/src/codex_thread_bridge/worktrees.py` | 177 | imported by bridge; spawns `git` (:20) | end user + relay host | CRW-151 | port | - | - |
| `plugins/crw/skills/crw-run/scripts/hook_probe.py` | 1547 | skill instruction (hook-contract.md:520,555,603,720 `observe`/`replay`) + CI (scripts/ci/contracts.py:10 `replay`) | end user + relay host | CRW-156 | port | - | - |
| `plugins/crw/skills/crw-run/scripts/parent_title.py` | 414 | skill instruction (crw-plan integrations.md:118) + CI (scripts/ci/contracts.py:22 `replay`) | end user + relay host | CRW-156 | port | - | - |
| `plugins/crw/skills/crw-run/scripts/start_policy.py` | 289 | skill instruction (start-policy.md:114) + CI (scripts/ci/contracts.py:18 `selftest`) | end user + relay host | CRW-156 | port | - | - |
| `plugins/crw/wiring/crw_bridge_mcp.py` | 239 | MCP: mcp.json `python3 ./wiring/crw_bridge_mcp.py` (cwd plugin root); exec's the pointer-named bridge (:230-232) | end user + relay host | CRW-156 | retire-with-evidence | `grep -rn crw_bridge_mcp` over plugins/, scripts/, docs/ (consumers: mcp.json:6, crw_transition/steps.py:40/764, test_plugin_wiring) + host scan of `~/.codex/plugins/cache/crw/crw/*/.mcp.json`/`wiring/*` and `~/.codex/config.toml` command strings | todo 34 switches mcp.json to `sh ./wiring/crw-bridge.sh`; deleted in todo 44 only after todo 43's retention scan reports no live or resumable reference |
| `plugins/crw/wiring/crw_stop_hook.py` | 156 | Stop hook: `python3 -c <bootstrap>` in hooks/stop-recording-completion.json:8 exec's it (or its copy `<CODEX_HOME>/crw-stop-hook.py`); spawns `adapterInterpreter adapterEntryPoint` (:141) | end user + relay host | CRW-156 | retire-with-evidence | `grep -rn crw_stop_hook` and `grep -rn crw-stop-hook` over plugins/, scripts/, docs/ (consumers: stop-recording-completion.json:8, crw_runtime/completion.py:107/437-438 LAUNCHER_SOURCE, crw_transition/steps.py:40, docs/plugin-packaging.md:375) + host scan of cached `wiring/*` and `<CODEX_HOME>/crw-stop-hook.py` | todo 34 changes the hook command to the `crw hook; exit 0` shell string; deleted in todo 44 only after todo 43's retention scan reports no live or resumable turn whose fixed hook command still names it |
| `scripts/check_operations_contract.py` | 241 | CI: scripts/ci/contracts.py:12 (operations fixtures replay) | dev/CI | CRW-160 | port | - | - |
| `scripts/ci/contracts.py` | 43 | CI: ci.yml:55 `python3 scripts/ci/contracts.py` | dev/CI | CRW-160 | port | - | - |
| `scripts/ci/gate.py` | 45 | CI: ci.yml:122 required-checks gate | dev/CI | CRW-160 | port | - | - |
| `scripts/ci/packages.py` | 206 | CI: ci.yml:104 per-package pytest (RELAY_CONFORMANCE_REQUIRED=1) | dev/CI | CRW-160 | port | - | - |
| `scripts/ci/plugin.py` | 964 | CI: ci.yml:54; spawned by crw_transition/steps.py:562 (`--payload`) | dev/CI | CRW-160 | port | - | - |
| `scripts/ci/scope.py` | 155 | CI: ci.yml:39 path-scope selection | dev/CI | CRW-160 | port | - | - |
| `scripts/ci/validate.py` | 145 | CI: ci.yml:53 link/metadata validation | dev/CI | CRW-160 | port | - | - |
| `scripts/completion_hook.py` | 53 | user-owner Stop hook: `<CODEX_HOME>/hooks.json` entry `<python> <checkout>/scripts/completion_hook.py` written by runtime_install.py `hook --owner user`; imports crw_runtime.completion | end user + relay host | CRW-156 | retire-with-evidence | `grep -rn completion_hook` over scripts/, docs/, packages/ (consumers: crw_runtime/completion.py:59 ENTRY_POINT_NAME, crw_transition/inventory.py:59/529, docs/runtime-install.md:1511, docs/plugin-transition.md:18) + host scan of `<CODEX_HOME>/hooks.json` | todo 38 `crw install hook` registers `crw hook` for owner=user; deleted in todo 44 after todo 43's retention scan finds no hooks.json entry naming it |
| `scripts/crw_runtime/bridgerecord.py` | 455 | imported by runtime_install, crw_transition.inventory, crw_transition.steps | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/check.py` | 69 | imported by runtime_install, trial_startup | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/codexconfig.py` | 267 | imported by runtime_install, crw_transition.inventory, crw_transition.steps | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/completion.py` | 4494 | imported by completion_hook, hook_comparison, plugin_transition, runtime_install, stop_events, crw_transition.*; spawns the guard (:767) and interpreter probes (:1031, :3458) | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/definition.py` | 206 | imported by runtime_install, trial_startup; spawns `git` (:100) | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/firing.py` | 761 | imported by crw_runtime.completion | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/hooks.py` | 229 | imported by hook_comparison, runtime_install, crw_runtime.completion, crw_transition.* | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/hostrecord.py` | 607 | imported by trial_startup, crw_runtime.{bridgerecord,completion,hooks,residue,staging}, crw_transition.steps | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/__init__.py` | 6 | package init (docstring only); import target for runtime_install, hook_comparison, trial_startup, stop_events, plugin_transition, completion_hook | relay host | CRW-157 | retire-with-evidence | `grep -rn 'crw_runtime' scripts/ --include=*.py` (importers listed in the Process graph section) | the package is deleted with its last importer in todo 46 (plan: `grep -rn crw_runtime scripts/` returns nothing) |
| `scripts/crw_runtime/ownership.py` | 143 | imported by runtime_install | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/pointer.py` | 171 | imported by runtime_install, crw_runtime.{completion,residue}, crw_transition.inventory | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/reading.py` | 462 | imported by hook_comparison, plugin_transition, trial_startup, runtime_install, 11 crw_runtime/crw_transition modules | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/residue.py` | 324 | imported by runtime_install | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/scope.py` | 324 | imported by runtime_install, crw_runtime.swapgate; spawns the relay CLI (:73) | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/staging.py` | 392 | imported by runtime_install, crw_runtime.residue | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/swapgate.py` | 385 | imported by runtime_install | relay host | CRW-157 | port | - | - |
| `scripts/crw_runtime/text.py` | 28 | imported by runtime_install (`text_prefix`), crw_runtime.codexconfig, crw_runtime.definition | relay host | CRW-157 | retire-with-evidence | `grep -rn text_prefix` and `grep -rn 'crw_runtime.text'` over scripts/ (consumers: runtime_install.py:32, codexconfig.py, definition.py) | no Go counterpart needed (it names Python `str.startswith` call sites; Go uses `strings.HasPrefix` directly); deleted with the crw_runtime package in todo 46 |
| `scripts/crw_transition/__init__.py` | 8 | package init; `TRANSITION_VERSION = 1` (no in-repo reader outside the package; grep -rn TRANSITION_VERSION) | relay host | CRW-158 | port | - | - |
| `scripts/crw_transition/inventory.py` | 1123 | imported by plugin_transition, crw_transition.steps, runtime_install (lazy :5077/:5350); spawns install.py (:454) and interpreter probe (:602) | relay host | CRW-158 | port | - | - |
| `scripts/crw_transition/steps.py` | 2704 | imported by plugin_transition; spawns scripts/ci/plugin.py (:562) | relay host | CRW-158 | port | - | - |
| `scripts/hook_comparison.py` | 2774 | dev CLI `python3 scripts/hook_comparison.py` (docs/hook-comparison.md); spawns runtime_install/relay/tracer | dev/CI | CRW-159 | port | - | - |
| `scripts/install.py` | 98 | operator CLI `python3 scripts/install.py --check` or `--apply` (docs/runtime-install.md:18); spawned by runtime_install.py:294 and crw_transition/inventory.py:454 | end user + relay host | CRW-158 | port | - | - |
| `scripts/plugin_transition.py` | 381 | operator CLI `python3 scripts/plugin_transition.py` (docs/plugin-transition.md) | end user + relay host | CRW-158 | port | - | - |
| `scripts/port/check_inventory.py` | 113 | dev CLI `python3 scripts/port/check_inventory.py` (this document's check, todo 1) | dev/CI | CRW-160 | retire-with-evidence | `grep -rn check_inventory` over scripts/, docs/, .github/ (consumers: this document only; no CI job runs it yet) | the Python inventory is obsolete once todo 44 (CRW-141) deletes the product Python; deleted with the remaining dev Python in todo 48 |
| `scripts/port/corpus_notes.py` | 230 | imported by scripts/port/check_corpus_count.py (todo 6) | dev/CI | CRW-160 | retire-with-evidence | `grep -rn corpus_notes` over scripts/, docs/, .github/ (consumer: scripts/port/check_corpus_count.py; no CI job runs it yet) | todo 48 retires the Python corpus checker after the Go tests assume its coverage obligation |
| `scripts/port/check_corpus_count.py` | 208 | dev CLI `python3 scripts/port/check_corpus_count.py` (todo 6) | dev/CI | CRW-160 | retire-with-evidence | `grep -rn check_corpus_count` over scripts/, docs/, .github/ (consumer: todo 6 corpus-coverage validation; no CI job runs it yet) | todo 48 retires the Python corpus checker after the Go tests assume its coverage obligation |
| `scripts/port/check_cutover_doc.py` | 149 | dev CLI `python3 scripts/port/check_cutover_doc.py` (todo 5) | dev/CI | CRW-160 | retire-with-evidence | `grep -rn check_cutover_doc` over scripts/, docs/, .github/ (consumer: todo 5 cutover-document validation; no CI job runs it yet) | todo 48 retires the Python cutover checker with the remaining dev Python |
| `scripts/port/check_test_map.py` | 133 | dev CLI `python3 scripts/port/check_test_map.py` (todo 3) | dev/CI | CRW-160 | retire-with-evidence | `grep -rn check_test_map` over scripts/, docs/, .github/ (consumer: todo 3 test-map validation; no CI job runs it yet) | todo 48 retires the Python test-map checker with the remaining dev Python |
| `scripts/port/dump_contracts.py` | 487 | dev CLI `python3 scripts/port/dump_contracts.py` (todo 2) | dev/CI | CRW-160 | retire-with-evidence | `grep -rn dump_contracts` over scripts/, docs/, .github/ (consumer: todo 2 contract-schema generation/check; no CI job runs it yet) | todo 48 retires the Python contract dumper with the remaining dev Python |
| `scripts/port/make_sqlite_fixture.py` | 28 | dev CLI `uv run --no-sync python scripts/port/make_sqlite_fixture.py` (todo 17) | dev/CI | CRW-152 | keep-as-data | - | - |
| `scripts/runtime_install.py` | 5810 | operator CLI `python3 scripts/runtime_install.py <cmd>` (docs/runtime-install.md:19); CI `verify-definition` via scripts/ci/contracts.py:15 | end user + relay host | CRW-158 | port | - | - |
| `scripts/stop_events.py` | 59 | dev CLI `python3 scripts/stop_events.py --journal-root ...` (docs/runtime-install.md:1867) | dev/CI | CRW-159 | port | - | - |
| `scripts/trial_startup.py` | 3734 | dev CLI `python3 scripts/trial_startup.py` (docs/live-trial.md); spawns git + relay (:1302) | dev/CI | CRW-159 | port | - | - |

## Process-spawn graph

Every `subprocess`, `exec*`, `create_subprocess_exec` and `sys.executable` site in the files
above, found with
`grep -nE 'subprocess\.(run|Popen|call|check_call|check_output)\(|os\.exec[lv]p?e?\(|sys\.executable|create_subprocess_(exec|shell)\(|os\.(system|spawn[lv]p?e?|fork|posix_spawnp?)\('`
(69 sites before this inventory; `scripts/port/check_inventory.py` spawns nothing). "Python" in the target column means the spawn needs a Python interpreter and must
disappear from the product path.

### Product path (end user + relay host)

| site | spawns | target | Go replacement |
| --- | --- | --- | --- |
| `plugins/crw/wiring/hooks/stop-recording-completion.json:8` | host runs `python3 -c <bootstrap>` which `exec`s `crw_stop_hook.py` | Python | `crw hook; exit 0` shell string (todo 34) |
| `plugins/crw/wiring/mcp.json:4-6` | host runs `python3 ./wiring/crw_bridge_mcp.py` | Python | `sh ./wiring/crw-bridge.sh` (todo 34) |
| `plugins/crw/wiring/crw_stop_hook.py:141` | `adapterInterpreter adapterEntryPoint <settings>` = stopadapter.py | Python | none: `crw hook` is the adapter |
| `plugins/crw/wiring/crw_bridge_mcp.py:230` | `os.execv` of the pointer-named `codex-thread-bridge` | Python console script | none: `crw bridge` |
| `plugins/crw/wiring/crw_bridge_mcp.py:232` | `os.execve` of the same, with execution-policy env | Python console script | none: `crw bridge` |
| `packages/codex-session-relay/src/codex_session_relay/stopadapter.py:472` | relay CLI `guard --socket --db-path --mode` | Python console script | in-process guard or `S/control.sock` (todo 33) |
| `packages/codex-session-relay/src/codex_session_relay/service.py:1680` | argv `[sys.executable, -m, codex_session_relay.cli, ...]` (supervisor) | Python self re-exec | `os.Executable()` (todo 29) |
| `packages/codex-session-relay/src/codex_session_relay/service.py:1741` | `Popen` of the :1680 argv, detached, log to `daemon.log` | Python self re-exec | same |
| `packages/codex-session-relay/src/codex_session_relay/service.py:1779` | argv `[sys.executable, -m, codex_session_relay.cli, ...]` (worker) | Python self re-exec | `os.Executable()` (todo 29) |
| `packages/codex-session-relay/src/codex_session_relay/service.py:1799` | `Popen` of the :1779 argv | Python self re-exec | same |
| `packages/codex-session-relay/src/codex_session_relay/supervisorchannel.py:205` | returns `(sys.executable, -m, codex_session_relay.cli)` as the relay launcher written into delivered lines | Python self reference | `crw relay` path (todo 24) |
| `packages/codex-session-relay/src/codex_session_relay/cli.py:5463` | prints `sys.executable -m codex_session_relay.cli` as a hint | Python self reference | `os.Executable()` (todo 8/20) |
| `packages/codex-session-relay/src/codex_session_relay/forge.py:160` | `gh` | non-Python, stays | `exec.Command("gh")` (todo 24) |
| `packages/codex-session-relay/src/codex_session_relay/mergetarget.py:95` | `git` | non-Python, stays | `exec.Command("git")` (todo 26) |
| `packages/codex-thread-bridge/src/codex_thread_bridge/worktrees.py:20` | `git worktree add ...` | non-Python, stays | `exec.Command("git")` (todo 15) |

### Installer and diagnosis (end user + relay host, operator-run)

| site | spawns | target | Go replacement |
| --- | --- | --- | --- |
| `scripts/runtime_install.py:240` | `sys.executable` named in `installedBy` | Python self reference | binary version string (todo 38) |
| `scripts/runtime_install.py:294` | argv `[sys.executable, scripts/install.py, --check, --dest]` | Python | in-process `crw install skills --check` (todo 39) |
| `scripts/runtime_install.py:296` | runs the :294 argv | Python | same |
| `scripts/runtime_install.py:528` | `_asked`: `<interpreter> -c <store program>` (store presence/tables, :560-597) | Python | in-process store read (todo 37) |
| `scripts/runtime_install.py:639` | `<python> -c "import platform;print(platform.python_version())"` version probe | Python | retired with venvs (todo 37) |
| `scripts/runtime_install.py:657` | `<python> -c "import <module>"` module location probe | Python | retired with venvs (todo 37) |
| `scripts/runtime_install.py:858` | `sys.executable` default interpreter for classification | Python | retired (todo 37) |
| `scripts/runtime_install.py:1089` | `sys.executable` fallback for `observe_app_server` | Python | retired (todo 37) |
| `scripts/runtime_install.py:1429` | `<python> check_connection.py --socket` | Python | `crw bridge` MCP `tools/list` (todo 37) |
| `scripts/runtime_install.py:1456` | `codex --version` | non-Python, stays | `exec.Command("codex")` (todo 37) |
| `scripts/runtime_install.py:1554` | comment naming the `sys.executable` probe it avoids | none (comment) | - |
| `scripts/runtime_install.py:1637` | `<interpreter> -B -c <settings program>` | Python probe | in-process settings check (todo 37) |
| `scripts/runtime_install.py:1690` | `<interpreter> -B -c <predicate program>` | Python probe | in-process predicate (todo 37) |
| `scripts/runtime_install.py:1815` | `<interpreter> -B -c <normalize probe>` | Python probe | in-process path normalisation (todo 37) |
| `scripts/runtime_install.py:1862` | `<interpreter> -B -c <contains probe>` | Python probe | in-process containment (todo 37) |
| `scripts/runtime_install.py:1903` | `<interpreter> -B -c <admits probe>` | Python probe | in-process admission (todo 37) |
| `scripts/runtime_install.py:2327` | `sys.executable` default for `hook --python` | Python | retired (todo 38) |
| `scripts/runtime_install.py:2686` | `perform`: `<interpreter> -m venv` (:2906), `<python> -m pip install` (:2911) | Python venv/pip | archive unpack (todo 38) |
| `scripts/runtime_install.py:4434` | `<python> -c "import sys;print(sys.prefix)"` | Python probe | retired (todo 37) |
| `scripts/runtime_install.py:4539` | `<python> check_connection.py` (measure) | Python | `crw bridge` exercise (todo 38) |
| `scripts/runtime_install.py:4641` | `sys.executable` default for `measure --python` | Python | retired (todo 38) |
| `scripts/runtime_install.py:4893` | `sys.executable` written as the probe record's command | Python | `crw` path (todo 38) |
| `scripts/runtime_install.py:4899` | launcher probe under the Desktop environment | declared launcher (Python today) | `sh ./wiring/crw-bridge.sh` probe (todo 38) |
| `scripts/crw_runtime/codexconfig.py:121` | `sys.executable` in a version-refusal message | Python self reference | retired (todo 37) |
| `scripts/crw_runtime/completion.py:767` | `invoke_guard`: relay `guard` subcommand | Python console script | `crw relay guard-evaluate` (todo 37) |
| `scripts/crw_runtime/completion.py:1031` | `_require_python`: `<candidate> -c "print(sys.version_info)"` | Python probe | retired (todo 37) |
| `scripts/crw_runtime/completion.py:3110` | `<relay> guard --help` | Python console script | `crw relay guard-evaluate --help` (todo 37) |
| `scripts/crw_runtime/completion.py:3458` | `_answers_as_an_interpreter`: `<resolved> -c <source>` | Python probe | retired (todo 37) |
| `scripts/crw_runtime/definition.py:100` | `git` | non-Python, stays | `exec.Command("git")` (todo 37) |
| `scripts/crw_runtime/scope.py:73` | relay CLI `doctor` (discovery, selected state and root candidate, :114-126) | Python console script | in-process call (todo 37) |
| `scripts/crw_transition/inventory.py:454` | argv `[sys.executable, scripts/install.py, --check, --dest]` | Python | in-process (todo 39) |
| `scripts/crw_transition/inventory.py:459` | runs the :454 argv | Python | same |
| `scripts/crw_transition/inventory.py:602` | `_evaluates_python`: `<candidate> -c <arith program>` | Python probe | retired (todo 39) |
| `scripts/crw_transition/steps.py:562` | argv `[sys.executable, scripts/ci/plugin.py, --payload]` | Python | in-process payload check (todo 39) |
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
| `scripts/hook_comparison.py:633` | writes a `#!<sys.executable>` launcher | Python |
| `scripts/hook_comparison.py:687` | argv `[sys.executable, runtime_install.py, hook, ...]` | Python |
| `scripts/hook_comparison.py:704` | runs the :687 argv | Python |
| `scripts/hook_comparison.py:835` | the arm's relay launcher | Python console script |
| `scripts/hook_comparison.py:996` | the hook command under a tracer | Python |
| `scripts/hook_comparison.py:1248` | comment naming `sys.executable` | none (comment) |
| `scripts/hook_comparison.py:1252` | `sys.executable` recorded as `startedExecutable` | Python |
| `scripts/hook_comparison.py:1687` | tracer probe argv with `sys.executable -c` | Python |
| `scripts/hook_comparison.py:1692` | runs the :1687 argv | Python |
| `scripts/hook_comparison.py:1717` | compares traced execs with `sys.executable` | Python |
| `scripts/hook_comparison.py:1721` | `sys.executable` in a message | Python |
| `scripts/hook_comparison.py:2334` | `git status --porcelain` | non-Python |
| `scripts/hook_comparison.py:2371` | `git rev-parse HEAD` | non-Python |
| `scripts/trial_startup.py:1302` | `run`: `git` (:1370) and relay CLI argv (:1363) | mixed |

## Python-interpreter probe sites to retire

These ask a candidate interpreter to run Python code. They exist only because the runtime is a
venv; the Go runtime has no interpreter to probe, so each is retired, not ported.

| site | question it answers | retired by |
| --- | --- | --- |
| `scripts/runtime_install.py:1637` | does the installed relay accept these settings (`-B -c <settings program>`) | todo 37 (in-process settings check) |
| `scripts/runtime_install.py:1690` | does the installed relay admit this predicate (`-B -c <predicate program>`) | todo 37 |
| `scripts/runtime_install.py:4434` | which environment the interpreter reports (`sys.prefix`) | todo 37 (binary digest replaces the environment) |
| `scripts/crw_runtime/completion.py:3458` | does the hook's interpreter word answer as a Python | todo 37 |
| `scripts/crw_transition/inventory.py:602` | does the candidate evaluate Python | todo 39 |

Siblings of the same kind, found by the spawn grep: `scripts/runtime_install.py:528` (store
programs), `:639` (version), `:657` (module location), `:1815`, `:1862`, `:1903` (path and
admission probes), `scripts/crw_runtime/completion.py:1031` (`_require_python`). All retire with
the venv in todos 37-38.

## In-process edges that force one binary

- The relay imports the bridge: `bridge_adapter.py:875-900` (ledger, rpc.AppServer,
  bridge.Bridge), `managed.py:66` (settings), `rolepolicy.py:176` (execution); the bridge
  `settings` module is also imported by relay assignment, cli, fakehost, faultsweep, packets,
  receiver and registry. The bridge cannot ship as a separate process from the relay.
- The daemon re-launches its own interpreter (`service.py:1680`, `:1779`,
  `supervisorchannel.py:205`), so the relay CLI and daemon are one program.
- `scripts/runtime_install.py`, `hook_comparison.py`, `trial_startup.py`, `stop_events.py`,
  `plugin_transition.py` and `completion_hook.py` all import `crw_runtime`; the package outlives
  the product Python until the last dev harness is ported (todo 46).

## Summary

| owner | files |
| --- | --- |
| CRW-150 | 4 |
| CRW-151 | 11 |
| CRW-152 | 5 |
| CRW-153 | 35 |
| CRW-154 | 19 |
| CRW-155 | 2 |
| CRW-156 | 7 |
| CRW-157 | 17 |
| CRW-158 | 6 |
| CRW-159 | 3 |
| CRW-160 | 8 |
