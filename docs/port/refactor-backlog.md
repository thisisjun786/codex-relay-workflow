# Refactor backlog (Go port)

Purpose: structural and design problems found during the Go port that are NOT parity, contract or invariant failures. They are recorded here and deliberately not fixed, because the port's oracle (Python-equivalent by property, byte-exact on contract surfaces) would stop distinguishing a port bug from an intended change. This file is the input to a post-port crw-refactor pass that Jun authorizes separately, after todo 44 (Python removal) and the cutover (todos 42-45). No Linear issues are created from it during the port.

Related records kept where they already live (not moved here): docs/port/decisions.md section 20 (Python defects inherited for parity), docs/port/known-defects.md (Python defects not carried over, intentional replacements), and the `risks` fields of .omo/ulw-execute/ledger.jsonl.

Rules: append-only, one bullet per entry, never edited by a later todo (parallel branches merge as the union). Parity, contract and invariant defects are fixed under decisions.md, not recorded here; if an entry turns out to be one, say so in the entry.

Entry format:
`- [todoNN|checkNN|orchestrator] <Go path or Python origin> - <what> - <why deferred: parity|contract|out-of-scope> - evidence: <commit or evidence file>`

## Entries

- [orchestrator] internal/relay/supervisor/{pyvalue.go,pythonjson.go,hierarchy.go,reading_subset.go} - Python-style JSON/repr helpers exist twice (todo 26 Dumps/Repr/StrRepr vs todo 24 pythonSorted/pythonRepr/readingRepr); fold to one set - out-of-scope - evidence: check26 st_01a0de36 notes, ledger 2026-09-26
- [orchestrator] internal/relay/supervisor/linkage_subset.go - StoreLinkage.Up duplicates the registry linkage up-walk (todo 26 owns linkage) - out-of-scope - evidence: check26 st_01a0de36 notes
- [orchestrator] internal/relay/cli/registry.go - three command families (registry, delivery, faults) each dispatch through their own ExecuteAs branch with a near-identical selection/kind-module check; one dispatch table would do - out-of-scope - evidence: merges 16371232, cb9eee06, 106b4ebd
- [orchestrator] internal/relay/delivery/cli.go - deliver/reconcile/recover/verify-acks are registered but refuse every --socket until todo 28; consider registering host commands only when a host adapter exists - out-of-scope - evidence: PR #175 review thread PRRT_kwDOUcYZMM6mRdms
- [todo24] internal/relay/supervisor/pythonjson.go vs internal/relay/evidence/pyvalue.go - pythonSorted and evidence.Dumps differ: pythonSorted formats float64 with 'f' (and marshals unknown types), while Dumps uses Python float formatting and fmt fallback; retain both until their caller domains can be reconciled - parity - evidence: todo24 cycle-fix comparison
- [todo24] internal/relay/supervisor/hierarchy.go vs internal/relay/evidence/pyvalue.go - pythonRepr orders linkage map keys by preferred diagnostic fields, while evidence.Repr sorts map keys; retain linkage map formatting - parity - evidence: todo24 cycle-fix comparison
- [todo24] internal/relay/supervisor/linkage_subset.go vs internal/relay/registry/linkage_read.go - StoreLinkage.Up starts from a relationship and returns plain maps, whereas Registry.Up accepts multiple selectors and returns ordered objects with different unregistered/gap and conflict readings; replacing one changes the channel's output - parity - evidence: todo24 cycle-fix comparison
- [todo24] internal/relay/supervisor/reading_subset.go vs internal/relay/evidence/pyvalue.go - readingRepr uses fmt.Sprint for malformed non-string reading values, whereas evidence.Repr uses Python repr for dicts/lists/bools; keep the fallback while sharing nil/string repr - parity - evidence: todo24 cycle-fix comparison
- [todo24] internal/relay/delivery/{currency.go,omitted.go} - HeadRevision and its row readers require *store.Store even for read-only queries, so omissionReceipt must wrap a bounded read-only sql.DB as a Store instead of reusing store.ReadOnly; consider a shared read-query interface after parity work - out-of-scope - evidence: task-24-crw-go-port.txt OMI-2/7 whole-output capture and real reporting-show replay (st_01a0df4e)
- [todo24] internal/relay/supervisor/{capture_test.go compareSupervisorTables, partb_set3_test.go supervisorBinaryPython} - the parity harness parses journal.detail and binary stdout into Go maps before comparing, which drops key order and int/float spelling, so pythonSorted and supervisorOrdered byte differences never show up; compare bytes for persisted JSON and CLI stdout - parity - evidence: check24 st_01a0df79
- [todo24] internal/relay/cli/registry.go - per-command usage is built from flag.FlagSet (alphabetical, every option shown optional) with a reporting-show special case; argparse-compatible usage/help/abbreviation needs a declared argument spec per command - contract - evidence: check24 st_01a0df79
- [todo24] internal/relay/cli/argparse.go and internal/relay/faults/argparse.go (delivery branch) - parser state machines and generated Python help tables remain command-family-local; extract a shared parser package when the branches converge, retaining byte-parity tests - out-of-scope - evidence: st_01a0df9a Test24ArgparsePython, delivery faults/argparse.go comparison
- [todo24] internal/relay/supervisor/autosend.go and packages/codex-session-relay/src/codex_session_relay/daemon.py:_report_upward - project rotation bounds the project count but staging still scans each selected project's full event history; consider a separately specified incremental staging index after parity - out-of-scope - evidence: task-24-crw-go-port.txt st_01a0df9b AUT-9/11 per-tick replay
- [todo24] internal/relay/supervisor/report_record.go:reportStoredValue and internal/relay/cli/supervisor.go:supervisorObjectKeys - domain maps require separate wire-order reconstruction for persistence and public replies; consider typed ordered records rather than additional field-name dispatch after the port - out-of-scope - evidence: st_01a0dfed Test24_ReportStorageBytes and Test24_SOS_5_BuiltBinaryBytes (2026-09-26)
- [todo24] internal/relay/delivery/{report_render.go,report_revision.go} and internal/relay/supervisor/report_subset.go - CXC status meanings are duplicated across completion composition, revision composition and validation; consolidate their ownership after parity rather than coupling packages during this fix - out-of-scope - evidence: st_01a0dfec RES-4 and RCF live replay comparisons (2026-09-26)
- [check24] internal/relay/supervisor/{autosend.go:293,send.go:282,send.go:185} - three overlapping claimability guards make single-guard mutants equivalent; consolidate them - out-of-scope - evidence: final todo-24 checker st_01a0e064
- [check24] internal/relay/supervisor/partd_dir_aut_test.go:Test24_AUT_3_RestartConverges - this is an alias wrapper around AUT_1; delete it or give it a real restart scenario - out-of-scope - evidence: final todo-24 checker st_01a0e064
- [check24] internal/relay/delivery/omitted.go:omissionReceipt - opens its own sql.DB instead of routing through the store read path; route it through that path or document why - out-of-scope - evidence: final todo-24 checker st_01a0e064
- [check24] internal/relay/supervisor/send.go:196-198 - the in-claim older-message check is shadowed by send.go:303-305 - out-of-scope - evidence: final todo-24 checker st_01a0e064
- [check22] internal/relay/faults/{cli.go,commands_c.go,commands_d.go,commands_f1.go,commands_f2.go} - four per-family response/refusal writers (response, cResponse, dResponse, f1Response, f2Response) and hand-kept key-order tables (cOrdered orders map) instead of one ordered-map encoder shared by every fault command; the Test22_* property tests are wrappers that call other tests - out-of-scope - evidence: .omo/evidence/task-22-crw-go-port.txt (check22)
- [todo22] internal/relay/faults/kinds.go - kind policies use package-global registration and map-shaped hook context, while the hook must run on the transaction connection; a typed per-ledger registry would make ownership clearer - out-of-scope, parity paths retain the current extension seam - evidence: Test22_FC_26_PreIssueSavepointWholeOutput and .omo/evidence/task-22-crw-go-port.txt (st_01a0df53)
- [check22] internal/relay/faults/{notice.go,notice_stage.go,d_eligibility.go} - the notification library temporarily owns supervisor notice packet/staging/parking and a supervision.contactable subset because todo 24's channel is not in this checkout; move these subsets into the channel owner when that port lands, preserving the exact-output integration replay - out-of-scope - evidence: .omo/evidence/task-22-crw-go-port.txt, st_01a0df51.
- [check22] internal/relay/faults/{cli.go,commands_c.go,commands_f2.go,ledger.go} - shared cFault now resolves aliases for prune/remediation/resolve, but CLI receipts reconstruct canonical identities separately and other families still repeat lookups; return canonical mutation receipts from the ledger in a later refactor rather than broadening this parity fix - out-of-scope - evidence: st_01a0dfb2, Test22_PruneAliasesAndJournalWholeOutput and task-22-crw-go-port.txt.
- [check22] internal/relay/faults/{target.go,cli.go} - SetWorkspaceTarget uses empty strings for omitted optional values, so the CLI must preserve explicit-empty refusal before calling it; a typed optional target request would remove that representation split - out-of-scope, the fault-target boundary parity is fixed without changing the public ledger API - evidence: st_01a0e014, Test22_FaultTargetValidationWholeOutput and task-22-crw-go-port.txt.
- [check24] internal/relay/cli/supervisor.go:supervisorObjectKeys - guesses record key order from field presence; replace with typed ordered records per payload after the port - out-of-scope - evidence: st_01a0e091 merge-evidence object-order audit and Test24BuiltBinaryEvidenceDefectBytes.
- [check24] internal/relay/delivery/cli.go:parseArgs and internal/relay/registry/cli.go:command.parse - the original parser signals global versus subcommand errors with a magic global: prefix; use typed errors throughout. The shared parser now returns typed Result fields, but the registry adapter retains this prefix - out-of-scope - evidence: st_01a0e091 Test24BuiltBinaryArgparseSweep.
- [check24] internal/relay/argparse/specs.json and command-family handler flag tables - formatting/parsing now share the generated live-parser spec, while handler defaults and value stores remain separately declared; derive handler bindings from the same spec in a later design pass - out-of-scope - evidence: st_01a0e091 shared formatter and 119-command sweep.
- [check24] internal/relay/capacity/cli.go:Precheck - the 13 capacity/region commands retain a separate command parser beside shared argparse; move their command parsing to the generated spec in the follow-up, leaving the repaired shared root handling intact - out-of-scope - evidence: st_01a0e0d6 full built-binary parser sweep.
- [check24] internal/relay/cli/supervisor.go:supervisorObjectKeys and internal/relay/evidence - presence-based ordering still reconstructs most evidence payloads; replace it with ordered evidence records instead of adding more presence switches - out-of-scope - evidence: st_01a0e0d6 provider repr repair preserves ordered providers without changing unrelated payload representation.
- [todo24] internal/relay/store/pyutf8.go and internal/dev/ci/pyvalue.go:decodeUTF8 - strict Python UTF-8 diagnostic semantics are duplicated across product and dev tooling; share a dependency-neutral codec after parity work, without coupling product code to CI - out-of-scope - evidence: st_01a0e0d6 merge-evidence UTF-8 byte comparisons.
- [check24] internal/relay/capacity/cli.go - Precheck and flag.FlagSet parse the capacity arguments twice; replace the double parse in the separate capacity parser follow-up - out-of-scope - evidence: independent todo-24 check, st_01a0e143.
- [check24] internal/relay/cli/registry.go:ExecuteAs - command-family dispatch branches copy the selection-refusal closure; consolidate its ownership after parity rather than changing refusal precedence here - out-of-scope - evidence: independent todo-24 check, st_01a0e143.
- [check24] internal/relay/cli/testdata/gh - the in-repo forge originally modeled one branch-rules shape; multi-rule parity cases now cover two, three, mixed and duplicate types, but fixture ownership and shared pagination remain a later design task - out-of-scope - evidence: Test24BuiltBinaryEvidenceDefectBytes/multi-rules, st_01a0e143.
- [todo24] internal/relay/faults/argparse.go - legacy fault handlers pass text maps; the adapter now carries converted numeric actions in request context to preserve Python values without reparsing, but a typed handler argument struct would make that dependency explicit - out-of-scope - evidence: Test24NumericDownstreamBytes, st_01a0e143.
- [check24] internal/relay/store/{PythonRepr,PyRepr}, internal/relay/evidence/StrRepr and fault-local quoting - repr helpers disagree on escaping and supported values; consolidate only with whole-byte coverage for each caller - out-of-scope - evidence: independent final checker chk24x, st_01a0e26d.
- [check24] internal/relay/evidence/collector.go:enumerateArray,enumerateREST - numbered page logic is duplicated; share the mechanism after parity rather than changing pagination contracts here - out-of-scope - evidence: independent final checker chk24x, st_01a0e26d.
- [check24] internal/relay/evidence/collector.go:collectGates - the early-return !rules.Complete term is redundant with truncating problems; simplify after parity work - out-of-scope - evidence: independent final checker chk24x, st_01a0e26d.
- [check24] internal/relay/cli parity/race tests - the CLI race suite takes roughly five minutes; reduce repeated process/build work without weakening real-surface byte comparisons - out-of-scope - evidence: independent final checker chk24x, st_01a0e26d.
- [todo24-fixY] internal/relay/registry/{assignment.go:601,927-928,holds.go:116-131,record.go:127,rolepolicy.go:49-53,settings.go:282-293} - pre-existing JSON reads use zero-value object/string/list assertions; audit against the corresponding Python expressions, preserving explicit isinstance checks rather than coercing every value - out-of-scope (registry owner) - evidence: source audit at 95a9fddd; todo-24 JSON-access parity work.
- [todo24-fixY] internal/relay/faults/cli.go:438-474, internal/relay/cli/policy.go:66-79, internal/relay/capacity/editregion_read.go:425 - pre-existing external/stored JSON accessors collapse wrong types to empty maps, lists, strings or booleans; trace validation and caller error boundaries before changing them - out-of-scope (faults, policy and capacity owners) - evidence: source audit at 95a9fddd; the existing 460 numeric/whitespace diffs remain a separate baseline.
- [todo24-fixY] internal/relay/mergeturn/check.go:144-148 - nullable SQL text reads ignore assertion success; verify SQLite text-affinity guarantees separately from JSON coercions in the merge-turn owner - out-of-scope - evidence: source audit at 95a9fddd.
- [todo24-fixY] internal/relay/cli/show.go and delivery preview context - Python preview invokes its static renderer without relationship context, while Go reconstructs sender/scope from the relationship; unrelated valid-preview difference observed when seeding a live relationship for JSON accessor tests - out-of-scope of the accessor fix, parity follow-up - evidence: completion preview probe; tests use a legacy event without a live relationship to isolate accessor semantics.

## Relay CLI reference table (input for the post-cutover CLI reduction)

This inventory is decision input only. It records use by CRW skills and wiring; it does not recommend removing or changing any command.

## Method

- Frozen source: `origin/dev` at commit `b4349cc9`, checked out read-only in `/dev/shm/clitable`.
- Defined commands: the 144 top-level choices returned by `codex_session_relay.cli.build_parser()` in `packages/codex-session-relay/src/codex_session_relay/cli.py`. Nested `service` actions are options beneath the one top-level `service` command, not additional rows. The declaration cross-check grep was:
  ```regex
(?:subparsers|actions)\.add_parser\(\s*["\']([^"\']+)
  ```
- Reference scope: every regular file below `plugins/crw/skills/**` and `plugins/crw/wiring/**` in the same frozen checkout. A command is referenced only when it occupies the command position in a relay invocation, including a documented command string/argv. Prose mentions and source links such as `service.py` do not count. For each command name substituted for `<COMMAND>`, the exact grep-compatible pattern was:
  ```regex
(?:codex-session-relay|crw\s+relay)(?:\s+(?:\[)?--(?:state|socket|kind-module)(?:=|\s+)\S+(?:\])?|\s+--json)*\s+<COMMAND>(?=[\s`\'",]|$)
  ```
- File counts are distinct path counts, not occurrence counts. Paths are repository-relative; at most three are displayed.

## Totals

- Defined: **144**
- Referenced: **49**
- Unreferenced: **95**

## Families where most commands are unreferenced

- `fault-*`: 31 of 31 unreferenced (0 referenced).
- `merge-turn-*`: 8 of 13 unreferenced (5 referenced).
- `other`: 50 of 75 unreferenced (25 referenced).

## Commands

| Command | Family | Referenced | Referencing files |
|---|---|---:|---|
| `fault-adopt` | `fault-*` | no | 0 |
| `fault-attention` | `fault-*` | no | 0 |
| `fault-cancel` | `fault-*` | no | 0 |
| `fault-claim` | `fault-*` | no | 0 |
| `fault-complete` | `fault-*` | no | 0 |
| `fault-fail` | `fault-*` | no | 0 |
| `fault-fix` | `fault-*` | no | 0 |
| `fault-limit` | `fault-*` | no | 0 |
| `fault-move` | `fault-*` | no | 0 |
| `fault-next` | `fault-*` | no | 0 |
| `fault-notification-ack` | `fault-*` | no | 0 |
| `fault-notification-fail` | `fault-*` | no | 0 |
| `fault-notification-raise` | `fault-*` | no | 0 |
| `fault-notification-reconcile` | `fault-*` | no | 0 |
| `fault-notification-reserve` | `fault-*` | no | 0 |
| `fault-notifications` | `fault-*` | no | 0 |
| `fault-observe` | `fault-*` | no | 0 |
| `fault-operation` | `fault-*` | no | 0 |
| `fault-policy` | `fault-*` | no | 0 |
| `fault-prune` | `fault-*` | no | 0 |
| `fault-queue` | `fault-*` | no | 0 |
| `fault-reconcile` | `fault-*` | no | 0 |
| `fault-relink` | `fault-*` | no | 0 |
| `fault-resolve` | `fault-*` | no | 0 |
| `fault-retry` | `fault-*` | no | 0 |
| `fault-reverify` | `fault-*` | no | 0 |
| `fault-show` | `fault-*` | no | 0 |
| `fault-stage` | `fault-*` | no | 0 |
| `fault-sweep` | `fault-*` | no | 0 |
| `fault-target` | `fault-*` | no | 0 |
| `fault-update` | `fault-*` | no | 0 |
| `merge-turn-acknowledge` | `merge-turn-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `merge-turn-attest` | `merge-turn-*` | no | 0 |
| `merge-turn-check` | `merge-turn-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `merge-turn-land` | `merge-turn-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `merge-turn-ready` | `merge-turn-*` | no | 0 |
| `merge-turn-release` | `merge-turn-*` | no | 0 |
| `merge-turn-request` | `merge-turn-*` | no | 0 |
| `merge-turn-request-return` | `merge-turn-*` | no | 0 |
| `merge-turn-resolve` | `merge-turn-*` | no | 0 |
| `merge-turn-restate-base` | `merge-turn-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `merge-turn-show` | `merge-turn-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `merge-turn-unknown` | `merge-turn-*` | no | 0 |
| `merge-turn-withdraw` | `merge-turn-*` | no | 0 |
| `ack` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `ack-proof` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `admit-turn` | `other` | no | 0 |
| `assignment-find` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `assignment-mark` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `assignment-show` | `other` | yes | 2 - `plugins/crw/skills/crw-run/references/operations/check-result.example.json`<br>`plugins/crw/skills/crw-run/references/relay.md` |
| `capacity-show` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `claim` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `completion-check` | `other` | no | 0 |
| `criteria-register` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `criteria-show` | `other` | no | 0 |
| `daemon` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `deliver` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `dispositions-show` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `doctor` | `other` | yes | 3 - `plugins/crw/skills/crw-plan/references/integrations.md`<br>`plugins/crw/skills/crw-run/references/operations/check-result.example.json`<br>`plugins/crw/skills/crw-run/references/relay.md` |
| `emit` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `generation-bind` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `generation-open` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `guard-evaluate` | `other` | no | 0 |
| `intent-attempt` | `other` | no | 0 |
| `intent-bind` | `other` | no | 0 |
| `intent-claim` | `other` | no | 0 |
| `intent-declare` | `other` | no | 0 |
| `intent-disposition` | `other` | no | 0 |
| `intent-register` | `other` | no | 0 |
| `intent-resolve` | `other` | no | 0 |
| `intent-show` | `other` | no | 0 |
| `limit-declare` | `other` | no | 0 |
| `linkage-attach` | `other` | no | 0 |
| `linkage-bind` | `other` | no | 0 |
| `linkage-completion` | `other` | no | 0 |
| `linkage-counterpart` | `other` | no | 0 |
| `linkage-directive` | `other` | no | 0 |
| `linkage-down` | `other` | no | 0 |
| `linkage-handover` | `other` | no | 0 |
| `linkage-outstanding` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `linkage-peer` | `other` | no | 0 |
| `linkage-settle` | `other` | no | 0 |
| `linkage-supervise` | `other` | no | 0 |
| `linkage-up` | `other` | no | 0 |
| `managed-release` | `other` | no | 0 |
| `managed-show` | `other` | no | 0 |
| `managed-start` | `other` | no | 0 |
| `merge-evidence` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/merge-readiness.md` |
| `packet-check` | `other` | yes | 2 - `plugins/crw/skills/crw-run/references/relay.md`<br>`plugins/crw/skills/crw-run/references/task-packet.md` |
| `product-bind` | `other` | no | 0 |
| `product-register` | `other` | no | 0 |
| `product-show` | `other` | no | 0 |
| `reconcile` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `recover` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `register` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `relationship-resume` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `relationship-status` | `other` | no | 0 |
| `reporting-derive` | `other` | no | 0 |
| `reporting-show` | `other` | no | 0 |
| `revision-head` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `route-classify` | `other` | no | 0 |
| `route-digest` | `other` | no | 0 |
| `route-intake` | `other` | no | 0 |
| `route-policy` | `other` | no | 0 |
| `route-projects` | `other` | no | 0 |
| `route-reconcile` | `other` | no | 0 |
| `route-show` | `other` | no | 0 |
| `service` | `other` | no | 0 |
| `settings-record` | `other` | no | 0 |
| `settings-show` | `other` | no | 0 |
| `show` | `other` | no | 0 |
| `slot-release` | `other` | no | 0 |
| `slot-reserve` | `other` | no | 0 |
| `status` | `other` | no | 0 |
| `store-challenge` | `other` | no | 0 |
| `store-identity` | `other` | no | 0 |
| `usage-observe` | `other` | no | 0 |
| `verdict` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `verify-acks` | `other` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `region-followup` | `region-*` | no | 0 |
| `region-followup-accept` | `region-*` | no | 0 |
| `region-followup-settle` | `region-*` | no | 0 |
| `region-propose` | `region-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `region-reaffirm` | `region-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `region-restate-revision` | `region-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `region-settle` | `region-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `region-show` | `region-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `supervisor-read` | `supervisor-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `supervisor-report-recorded` | `supervisor-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `supervisor-select` | `supervisor-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `supervisor-send` | `supervisor-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `supervisor-show` | `supervisor-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `supervisor-stage` | `supervisor-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `supervisor-standing` | `supervisor-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `sync-claim` | `sync-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `sync-complete` | `sync-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `sync-fail` | `sync-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `sync-next` | `sync-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `sync-operation` | `sync-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `sync-progress` | `sync-*` | no | 0 |
| `sync-reconcile` | `sync-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
| `sync-retry` | `sync-*` | no | 0 |
| `sync-status` | `sync-*` | no | 0 |
| `sync-target` | `sync-*` | yes | 1 - `plugins/crw/skills/crw-run/references/relay.md` |
