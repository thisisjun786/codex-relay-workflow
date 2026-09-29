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

- [todo28] internal/relay/store/{scope.go,authorized.go,manifest.go,manifest_detailed.go,frozen_detailed.go} and internal/relay/capacity/editregion_read.go - the additive descriptor/detailed verifier APIs reuse traversal and hashing but legacy two-value verification loops and capacity component-containment remain separate; consolidation is deferred per Jun rather than replacing existing paths - why deferred: out-of-scope - evidence: .omo/evidence/task-28-crw-go-port.txt (orchestrator scope decision).
- [check28] internal/relay/adapter/{cursor.go,shape.go,json.go} and internal/relay/cxc - switch local Python value helpers to evidence.* after todo 24 merges; switch report_subset.go and delivery inline cxc strings to internal/relay/cxc after todo 24 merges - why deferred: out-of-scope, todo 24 is not on dev and existing call sites must remain unchanged - evidence: Test28_CursorValuesLivePython, Test28_CXCPublicSurfaceLivePython (st_01a0e47a).
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
- [todo23] internal/relay/reception/depth.go - CPython 3.13 JSON decoder nesting boundary is 9993 at the receiver call depth; source supports other Python versions with different parser recursion behavior - why deferred: parity - evidence: .omo/evidence/task-23-crw-go-port.txt part B SR-10 live edge capture
- [todo23] internal/relay/sync/argparse.go - move per-command metadata and Command.Parse adapter to shared internal/relay/argparse after todo24 lands; keep tested width-aware bytes - why deferred: out-of-scope - evidence: .omo/evidence/task-23-crw-go-port.txt part B argparse COLUMNS 80/120/unset
- [todo30] internal/relay/store/ownership/record.go, internal/relay/service/scope.go, internal/testsupport/ownership.go - scope-key derivation is repeated across admission, service registration and fixture seeding; consolidate the production algorithm and give the fixture an independent contract oracle after the port - structural work deferred - evidence: t30-code-review.md M1.
- [todo30] internal/relay/cli/daemon.go - daemon and candidate recovery duplicate the expired-lease held_uncertain UPDATE; extract one recovery operation alongside RecoverOnStart after parity - structural work deferred - evidence: t30-code-review.md M2.
- [todo30] internal/relay/store/ownership/{controller,record}.go, internal/relay/service/{takeover,record}.go, internal/relay/cli/daemon.go and associated protocol tests - split oversized protocol and lifecycle modules only after cutover, preserving the current cross-process crash, lock and candidate-channel tests - structural work deferred - evidence: t30-code-review.md L8.
- [todo31] internal/relay/delivery/cli.go (answer), internal/relay/faults/cli.go (faultAnswer), internal/relay/cli/registry.go (emit) - each command family turns a handler's ending into its printed object and exit code on its own, and the takeover-inbox appliers (delivery.ApplyQueued, faults.ApplyQueued) reuse each family's copy; one classifier behind the single dispatch table named above would serve direct runs and replays alike - out-of-scope - evidence: todo 31 commit, internal/relay/cli/inbox.go replayQueued
- [todo31] internal/relay/store/store.go (OnFirstWritableOpen) - the relay CLI drains the inbox from a hook on a command's first admitted writable store open, standing in for cli.main's single `_ownership_preflight` open; with one dispatch table the CLI could admit the store once before the handler, as Python does, and drop the hook - out-of-scope - evidence: todo 31 commit
- [todo31][review] internal/relay/cli/registry.go (admit), internal/relay/store/store.go (WithAdmitted) - resolves the OnFirstWritableOpen entry above: the dispatch admits a writable command's store once after the selection refusal, imports --kind-module, drains the inbox and hands that store to the handler's own open, and the hook is gone; the admission is still wired into each command family's check callback (faults, registry, delivery) and the table's run, which one dispatch table would make a single call - out-of-scope - evidence: todo 31 review fixes

## Deferred review findings (fix before todo 42)

- [todo28][PR180 4120293585] Parse numeric-string turn start times where Python consumers call float() - Python: packages/codex-session-relay/src/codex_session_relay/ack.py:1082-1094 accepts numeric strings - Go: internal/relay/delivery/adapter.go:44-58 treats them as absent
- [todo28][PR180 4120293932] Add focused mutation coverage for source stability and post-copy frozen-blob re-hashing - Python: packages/codex-session-relay/src/codex_session_relay/manifest.py:226-245 verifies source stability then re-reads the frozen blob - Go: internal/relay/store/manifest.go:86-113,130-143 implements both checks without this review-specific proof
- [todo28][PR180 4120779174] Preserve Python field order in the supervisor-send held response - Python: packages/codex-session-relay/src/codex_session_relay/cli.py:958-966 emits attempted, sent, messageId, state, holdReason, nextEligibleAt, detail in insertion order - Go: internal/relay/cli/supervisor.go:124-127 reconstructs supervisor output order separately
- [todo30][audit 26] Controller status and a `takeover repair-mirror` for the "initial stamp committed, mirror absent" crash state, and Go classifying a gate-only directory as absent - Go: internal/relay/store/ownership.go createAbsent (absent() still requires no write-gate.lock), internal/relay/store/ownership/controller.go read (ReadRecord first) - Python: packages/codex-session-relay/src/codex_session_relay/ownership.py:195-248 - deferred: orchestrator decision D0 (anything partial is refused, never repaired); the temp-name build and link(2) of audit 59 removes Go's DB-without-keys window, so only an explicit operator repair remains open
- [todo30][audit 59] Resume an absent-store creation by republishing the mirror from an exact owner=go epoch-1 stamp - Go: internal/relay/store/ownership.go createAbsent/publishAbsent - Python: packages/codex-session-relay/src/codex_session_relay/ownership.py:212-248 - deferred: decision D0 (never repaired); items 1-3 (temp build, link, publish) are done
- [todo30][audit 31] Transfer's integrity_check and SQLite backup still run inside the 30-second CLI bound (rollback has 30 s plus --ready-timeout); size that bound from the live store - Go: internal/relay/cli/takeover.go (bound), internal/relay/store/ownership/backup.go inspect - Python: n/a - deferred: needs the todo-42 rehearsal's measurement of the live store; the Python half (ready() resets its channel deadline) is package C's
- [todo30][audit 12] Go applies S/takeover-inbox: replay under the candidate permit before ready, on daemon start and before writable CLI work - Go: internal/relay/cli/daemon.go readyCandidate TODO(31), internal/relay/store/ownership/controller.go transfer TODO(31) - Python: packages/codex-session-relay/src/codex_session_relay/inbox.py replay, cli.py recover - deferred: todo 31 (decision D6); until then the seam refuses the CAS toward Go, the Go candidate's readiness and `takeover commit` (PR #185 thread 4128457226) while any entry is queued; the commit refusal's TODO(31) is in controller.go Commit
- [todo30][audit 7] Route the Go `guard-evaluate` CLI to the owner's control.sock as Python's socket_guard does under another owner, draining or starting; until then the Go CLI evaluates in-process, and `TestReadOnlyForms_match_python_in_every_ownership_state` compares guard-evaluate only on an active Python-owned store - Go: internal/relay/cli/guard.go:66, internal/relay/hook/guard.go:27 - Python: packages/codex-session-relay/src/codex_session_relay/cli.py:3942-3968 (stopadapter.socket_guard) - deferred: control-RPC routing belongs with the control client/server fixes (audit 35, 36, 39), not with the read-only store open
- [todo36-integration] Refuse `--kind-module` after a write form's store, as cli.main imports kind modules after `_ownership_preflight`: on an absent store the fence initializes the store before the import refusal, and on a store the other runtime owns it answers `store_owned_by_other` where Go answers the usage refusal (verified for `relationship-status`) - Go: internal/relay/cli/registry.go (the registry and delivery `check` closures call kindModuleRefusal before registry.run/delivery open the store) - Python: packages/codex-session-relay/src/codex_session_relay/cli.py main (`_ownership_preflight`, then `_import_kind_modules`) - deferred: the check closures are shared with the selection refusal, which does precede the store; splitting them is a dispatch change beyond the registry precheck order fixed here (decision 31). The delivery, faults and internal/relay/cli dispatchers were not audited for a write form's own refusal before its store.
- [todo36][audit 25] Refuse a partial store for a read-only form in Python as Go does (the absent-store half landed in both runtimes: reason store_absent, exit 2): a mirror or a gate without D refused, store_owned_by_other, exit 2, where the fence now answers both with a host error ("OperationalError: unable to open database file", exit 3), creating nothing; Go words it "ownership refused: existing database required: lstat <D>: no such file or directory" - Python: packages/codex-session-relay/src/codex_session_relay/cli.py (Services.store), ownership.py (Admission) - Go: internal/relay/store/hold.go (openForRead) - deferred: Python fence source is work package C's; the contract text (cutover.md, Read-only clients, marked pending) landed
- [todo30][audit 47] Read takeover.json bytes as json.loads(bytes) does: detect_encoding accepts a UTF-8 BOM and UTF-16/32 documents, which the shared mirror reader answers as a JSONDecodeError or UnicodeDecodeError - Go: internal/relay/store/fence_wording.go (MirrorRefusal), internal/relay/cli/doctor.go (readOwnership) - Python: packages/codex-session-relay/src/codex_session_relay/ownership.py (mirror) - deferred: no runtime writes such a mirror, and Go's ReadRecord refuses one whatever doctor says; the reader predates this package and is only moved here
- [todo30][audit 50] Decide the Go daemon's installation identity: it records package codex-session-relay at the Go executable's directory, so the host-record revision lookup always answers unknown; report Python's package identity or a crw identity with its own host-record component - Go: internal/relay/adapter/daemon.go:58, internal/relay/faults/sweep.go (host-record lookup) - Python: packages/codex-session-relay/src/codex_session_relay/faultsweep.py:429,540 - deferred: needs a decision (decisions.md 31 records it as open); the version now equals the fence package's 0.2.0
- [todo36][PR185 4128457287] A Stop that falls back to the run's own selection still spends the selected store's recorded-socket read on both sides when routed: the legacy CLI's local refusal and the owner's (decision 24), each under `SQLITE_TIMEOUT`, then the owner's receipt read. With the store held exclusively that is 6.4 s through the CLI (was 8.4 s before the preflight stopped evaluating), past the adapter's 5 s; the Python owner's own fallback evaluation (selection read plus receipt read, 4 s held) also exceeds the Go hook's 3.5 s guard allocation, and the CLI's local path reads the same store three times (selection, check_stop, receipt). A Stop whose intent records dbPath spends one read (2.2 s held) - Python: packages/codex-session-relay/src/codex_session_relay/cli.py cmd_guard_evaluate/_selection_refusal, store.py store_socket, ownership.py stop_metadata - Go: internal/relay/store/location.go storeSocket reads a snapshot copy that waits on no lock - deferred: dropping the CLI's copy of the read changes which refusal wins when an override's store records another socket and its discovery is ambiguous, or needs socket_guard split into connect and send; decision 24 keeps both sides' selection.
- [todo30][PR185 4128457226] `takeover begin --to go` does not look at S/takeover-inbox: with an entry queued, begin publishes draining and drain stops the Python service before the transfer refuses, so the recovery is abort plus an operator `service start` - Go: internal/relay/store/ownership/controller.go begin, internal/relay/service/takeover.go Preflight - Python: n/a - deferred: it strands nothing (the transfer after the barrier, the Go candidate's readiness and commit refuse), entries can still arrive while draining so the transfer check stays the one that decides, and todo 31 removes the seam.
- [todo36][audit 16] Add a read-only marker reader (`inbox show <operation-id>` / `crw relay inbox show`) so a sender holding only `durably_queued` can learn a later exit-2/4 or `inbox-conflict:` result - Go: todo 31 (no inbox consumer yet; internal/relay/service/control.go inbox-submit placeholder) - Python: packages/codex-session-relay/src/codex_session_relay/inbox.py (markers only in schema_meta) - deferred: a new CLI surface in both runtimes that orchestrator decision D7 did not include; it needs relay-cli.json and a Go counterpart in the same change.
- [todo36][audit 16/D7] Decide whether a retired ID whose marker is a refusal (exit != 0) and whose reused bytes differ should be judged again instead of recorded as `inbox-conflict:` - Go: todo 31 must copy whichever rule stands - Python: packages/codex-session-relay/src/codex_session_relay/inbox.py:_replay_entry (implements D7 literally: any digest mismatch is a terminal conflict) - deferred: D7 is binding; under it a corrected ACK after a refused one is never applied, while direct mode would evaluate it.
- [todo36][audit 45] Report ignored (non-grammar) and corrupt inbox names in doctor/status - Go: internal/relay/cli/doctor*.go (no inbox block) - Python: packages/codex-session-relay/src/codex_session_relay/store.py:probe / cli.py doctor (no inbox block) - deferred: doctor --json is a parity surface, so the block must land in both runtimes together; the contract names the operator recovery (move the named entry out) today.
- [todo36][audit 33/61] Verify the darwin LOCAL_PEERCRED peer check on a real darwin host, and give the Python Stop client (`_trusted_guard_peer`) the same darwin branch - Go: internal/relay/hook/peer_darwin.go - Python: packages/codex-session-relay/src/codex_session_relay/control.py:peer_uid (server, tested only with a fake xucred), stopadapter.py:_trusted_guard_peer (client, still SO_PEERCRED only, so darwin owner=go Stops fall back to the ownership refusal) - deferred: no darwin host in this environment; Linux, the todo-42 target, is covered by real-socket tests.
- [todo36][audit 20/24/46] Compare the mirror's scopeKey with the validating process's own scope-registry lock authority on every Python admission, as Go does - Go: internal/relay/store/ownership/record.go Validate ("scope key disagrees with lock authority") - Python: packages/codex-session-relay/src/codex_session_relay/ownership.py:validate (checks socket agreement and a non-empty scopeKey only) - deferred: the key depends on the validating process's CODEX_SESSION_RELAY_SCOPE_DIR, so every in-process opener under another authority (service workers with an isolated registry, test fixtures) would refuse; isolated-root spelling still differs between runtimes (audit 22, package A). Initialization records ScopeRegistry.key under the initializer's authority.
- [todo36][audit 20/24/46] Give 'requested socket disagrees with ownership record' its own refusal reason instead of `store_owned_by_other` (the owner is the same; the store is bound to another App Server socket) - Go: internal/relay/store/ownership.go openFenced (an unwrapped ownership.Refused, exit 3) - Python: packages/codex-session-relay/src/codex_session_relay/store.py Store.__init__ (OwnershipRefused, reason store_owned_by_other, exit 2) - deferred: a new RefusalReason is a CLI JSON contract change in both runtimes; Python keeps audit 24(3)'s store_owned_by_other with an exact detail until both change together.
- [todo36] intent.registration_hold's write-gate SH wait is not decided by SQLITE_TIMEOUT: its docstring promises (None, why) when the lock cannot be taken within that bound, but the fence's Admission takes `LOCK_SH` blocking (cutover.md Lock order: Python's admission SH keeps blocking), so a registration waits out a transfer barrier before it is refused - Go: internal/relay/store/registration_hold.go (admitWrite never blocks on the gate; contention is a refusal) - Python: packages/codex-session-relay/src/codex_session_relay/intent.py registration_hold, ownership.py Admission - deferred: not a Stop path (a Stop never opens the hold; test_failure_recovery.py measures it), and bounding it changes the documented Python admission rule.
- [todo33][CI run 36445722770] internal/relay/hook/status.go:497 answersPython 5 s probe makes the TestDomain/hook `three_hosts_that_look_alike ... recorded_elsewhere` case load-sensitive: under CI load an unanswered probe makes startable() nil and journalSets skips the journal; the contract fixture should not depend on a live interpreter probe (inject the probe or raise the budget in tests)
- [todo35][PR184 4124878069] Readback replay panics on composite values: `replayTitles` compares two JSON arrays/objects with Go interface equality - Python: plugins/crw/skills/crw-run/scripts/parent_title.py compares them structurally and continues - Go: internal/skill/parent_title.go:449-453; add a composite-value replay fixture
- [todo35][PR184 4124878229] A trailing `--` on a no-positional `crw skill` command is counted as an extra argument (exit 2) where argparse accepts it - Go: internal/skill/argparse.go:123-127
- [todo35][PR184 4124878424] Equal nested-array Stop field lists abort host replay with an unhashable-list error before the equality check; Python sorts and compares first and builds sets only when they differ - Go: internal/skill/hook_host.go:99-106 (hostFieldProblems/hostSorted)
- [orchestrator][CI run 36445722770] resolves the todo33 status.go:497 entry above: the `recorded_elsewhere` failure was not the interpreter probe. An unanswered probe leaves startable() nil, which reads `cause_unreadable`; the observed `nothing_recorded` needs both named journals read empty, so the hook itself wrote no row. Reproduced under CPU throttling, where an instrumented binary showed the settings read cut at entry + 100 ms after a scheduling stall. Fixed as a contract defect under decisions.md section 30. The 5 s probe matches Python's INTERPRETER_PROBE_SECONDS and stays
- [todo28][PR180 4121143384] A host `status: null` makes guardedSend record an unknown outcome instead of resuming; check Python bridge_adapter guarded send for a null thread status - Go: internal/relay/adapter/transport.go:233
- [todo28][PR180 4121143529] Inaccessible MANIFEST.json returns an error without problem/unreadable lists; check Python verify_frozen_detailed access classification (EACCES currently matches the Python control per round R28-08) - Go: internal/relay/store/frozen_detailed.go:25
- [todo28][PR180 4121143686] HostCall reads are outside Close draining; check Python transport shutdown for in-flight reads - Go: internal/relay/adapter/transport.go:90
- [todo28][PR180 4121231356] Close closes the shared RPC client during an active read; same root as 4121143686 - Go: internal/relay/adapter/transport.go:90
- [todo28][PR180 4121231471] managedStart uses context.Background after CLI cancellation; check Python managed start cancellation handling - Go: internal/relay/adapter/managed_cli.go:21
- [todo29][PR183 4121593527] Darwin lifecycle unverified: OpenProcess has no usable handle and workerAttributes sets no parent-death behaviour; validate start/stop/orphan cleanup on Darwin against Python before cutover - Go: internal/relay/service/process_darwin.go
- [todo33][PR181 4120778295] Routine (non-parity-tag) coverage for a stalled control.sock peer hitting the guard deadline, deterministic on a slow runner - Python: n/a (no socket; stopadapter.py:466-524 subprocess timeout) - Go: internal/relay/hook/adapter_test.go:219 uses a virtual clock; real socket only in latency_test.go (parity tag)
- [todo35][PR184 4125051877] `hook-probe observe` sorts schema `required`/`properties`/decision `enum` values by their printed spelling; Python `sorted` orders numbers numerically and raises on incomparable mixed types (e.g. enum [2, 10] prints [10, 2] in Go) - Python: plugins/crw/skills/crw-run/scripts/hook_probe.py:671-703 - Go: internal/skill/hook_capability.go:17 (probeSorted)
- [hook][PR188 4127099447] Input that arrives after entry+100 ms but is already readable when the hook first polls stdin is accepted rather than released as stdin_unreadable; a zero-timeout poll cannot timestamp arrival. Intentional under decision 32 (a descheduled hook cannot tell late input from on-time input, and Python bounds stdin only by the 5 s budget); revisit only if a host sends Stop payloads late on purpose - Go: internal/relay/hook/input.go:67 (pollDescriptor)
- [todo36][PR185 4128791771] resolves the Python-client half of the [todo36][audit 33/61] darwin entry above: the Stop client (stopadapter._trusted_guard_peer) and the Python GuardServer (control.py) now read the peer through one stopadapter.peer_uid, which answers SO_PEERCRED on Linux and SOL_LOCAL/LOCAL_PEERCRED (struct xucred) on Darwin, so a darwin owner=go Stop reaches the owner's verdict instead of the ownership refusal. Still deferred: running either runtime's darwin branch on a real darwin host (test_fence.py fakes the xucred answer on a real Linux connection). Not an identity gap, recorded for the same host pass: a control.sock path of 104 bytes or more is reached through /proc/self/fd on Linux only, so on Darwin neither owner can bind it and neither client can reach it (Python control.py/stopadapter.py fall back to the plain path, Go internal/relay/hook/control.go ControlAddress names /proc unconditionally); the gap is symmetric, since no owner serves such a path there.
- [todo31] resolves the [todo30][audit 12] entry above: Go applies S/takeover-inbox in the candidate's recovery before readiness, at daemon start and in supervisor recovery, and before a writable command's first store use; the TODO(31) seams in internal/relay/cli/daemon.go and internal/relay/store/ownership/controller.go (transfer and commit) are gone, and the CAS toward Go no longer waits for an empty inbox (cutover.md Step 6, Wire format). `takeover commit` keeps its empty-inbox precondition as a rule rather than a seam: the controller applies nothing, so it refuses while an entry is queued and names a Go drain (a writable relay command or a service restart) as the recovery (cutover.md Commit point, Test30CommitRefusesOverQueuedInboxEntries). This also resolves the [todo30][PR185 4128457226] begin entry above: no transfer refuses over a queued entry any more, so begin has nothing to look at
- [todo31][audit 45 item 3] Classify takeover-inbox names in the transfer inventory as the drain does: the Step-4 walk descends into subdirectories and refuses any symlink or non-regular file, grammar-valid or not, and does not check a grammar-valid entry's bytes, so a corrupt entry passes the transfer and fails the candidate's recovery instead - Go: internal/relay/store/ownership/backup.go inspect - Python: n/a (the Python controller has no transfer) - deferred: a transfer-side refusal changes Step 4's answers, which todo 31 (the drain) does not own; the candidate's fail-closed recovery keeps the entry and ownership starting, which is safe
- [todo31] A legacy `supervisor-read` entry replays in Go to its no-host usage answer even when the drainer has a socket, where the Python owner verifies the readback through its host - Go: internal/relay/cli/inbox.go replayQueued - Python: packages/codex-session-relay/src/codex_session_relay/inbox.py (REPLAY_KEYS) with cli.cmd_supervisor_read - deferred: no fence build that queued one was ever deployed (scope analysis 31: retire-dead); applying it would need a supervisor channel on the drainer's store with a store-less host, as delivery.QueuedAckHost is for acks; documented in cutover.md Wire format
- [todo31][review] resolves the legacy `supervisor-read` entry above: Go replays it through supervisor.Channel.ReadBack on the drainer's store, with a store-less host opened on first use (Python's _LazyAdapter), so its marker is the fence's; what remains is any discovery cursor a store-backed host would record, as for a queued ack (next entry) - Go: internal/relay/cli/inbox.go replayReadback
- [todo31] A replayed ack's host (delivery.QueuedAckHost) is an adapter that shares no store, so turn discovery during the replay records no discovery_cursors row where Python's adapter records one in the replay's transaction - Go: internal/relay/adapter/cli.go queuedAckHost - Python: packages/codex-session-relay/src/codex_session_relay/bridge_adapter.py (cursor writes on the shared connection) - deferred: the store-backed adapter reads and writes cursors with context.Background() on the pool (adapter.scanListing), which would wait on the drain's own transaction; threading the transaction's context through the adapter is an adapter change
- [todo31] Go transactions joined inside store.Compose do not revalidate admission, where Python's transaction() revalidates on every joined transaction; an ownership change during one replayed handler is therefore seen only when the next entry's transaction opens, which fails the drain there as Python's does, instead of being retained with the entry whose handler was running - Go: internal/relay/store/records.go Transaction (composing branch) - Python: packages/codex-session-relay/src/codex_session_relay/store.py transaction - deferred: a store-wide behaviour of every composed Go command, not the inbox's; the drain revalidates once per entry at its own Compose

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

- [todo29] internal/relay/{service/worker.go,cli/policy.go,adapter/worker_observation.go} - worker policy identity checks have three entry-point-specific implementations; consolidate after the port with whole-receipt and process-race coverage rather than changing caller refusal precedence here - out-of-scope - evidence: task-29-crw-go-port.txt and Test29OrphanKeepsInheritedLocks.

- [todo36] packages/codex-session-relay/src/codex_session_relay/ownership.py:metadata - no-sidecar admission reads a private DB/WAL snapshot, so admission cost scales with database size; a separately specified metadata-only SQLite snapshot reader could reduce that cost - out-of-scope - evidence: task-36-crw-go-port.txt, live-WAL refusal test.
- [todo36] packages/codex-session-relay/src/codex_session_relay/cli.py:READ_ONLY_COMMANDS - lazy writable services historically serve read commands too; command metadata should eventually declare effects rather than maintaining a separate read-only classification - out-of-scope - evidence: task-36-crw-go-port.txt, exhaustive nonqueued writer refusal matrix.

- [todo36-integration] packages/codex-session-relay/src/codex_session_relay/{cli.py:cmd_guard_evaluate,guard.py:_evaluate} - selection-before-owner routing reuses non-recording evaluation to preserve lazy receipt selection; extracting a shared selection-only phase could remove the duplicate marker/receipt read without duplicating its precedence rules - out-of-scope - evidence: unchanged Test33ReviewD6 and test_management_cli.py explicit/intent/marker-only selection cases.
- [todo30] internal/relay/store/{ownership.go,ownership/record.go,location.go} - pre-admission metadata/schema reads use disposable main-plus-WAL copies because SQLite mode=ro can create source sidecars; a dedicated non-writing SQLite VFS would avoid whole-store copy cost without weakening fail-closed admission - out-of-scope design work - evidence: Test30ForeignOwnerCLIRefusesWithoutDBChanges and task-30-crw-go-port.txt.
- [todo30] internal/relay/service/{record.go,takeover.go} - takeover process identity intentionally ignores installationId while ordinary stop requires it, because the controller and outgoing runtime are different installations; share typed identity/exit primitives after parity rather than conflating these authority policies - out-of-scope - evidence: Test30TakeoverBuiltCLI and Test29D1DeterministicReapStates.
- [todo30] internal/relay/cli/readonly.go - the read-only form set repeats cli.py READ_ONLY_COMMANDS and _read_only_command as Go data, checked only by the READ_FORMS matrix; derive both from one declaration of command effects (see the todo36 note above) - out-of-scope - evidence: TestReadOnlyForms_match_python_in_every_ownership_state.
- [todo35][PR184 4122766427] internal/skill/hook_replay.go:pythonReturnSites and plugins/skill_assets.go - fix before todo 44: replace the embedded Python return-site denominator and source labels with Go-owned semantic branch metadata before deleting hook_probe.py. The current go:embed declaration makes source removal a build failure; an already-built binary retains its embedded source and keeps replaying the old denominator. Preserve the per-run Go reach recorder, prove each denominator entry corresponds to a real branch, and retain mutation-backed missing-branch/trace checks rather than substituting a fixed total - why deferred: out-of-scope for Python-equivalent todo 35, explicitly owned by Python removal - evidence: TestHookProbeReturnSitesMatchCanonicalPythonSource and Devin 4122766427.

- [todo32] docs/port/refactor-backlog.md (the CLI command table above) - correction, the table itself left as written: its reference regex requires a `codex-session-relay` prefix, so it marks the eight `intent-*` commands and `service` "Referenced: no", yet both are consumed by instructions a host follows: plugins/crw/skills/crw-run/references/relay.md:85 (`intent-show`), :121 (`intent-declare`), :125 (`intent-register`), :1117-1118 (`intent-claim`, `intent-disposition`), :1246-1249 (the store's service declaration and `service status` launchPolicy.runningDigest); plugins/crw/skills/crw-run/references/hook-contract.md:118-126 (the marker files these commands write); the managed dispatch prompt that tells every child to publish its own `intent-claim` and per-turn disposition (packages/codex-session-relay/src/codex_session_relay/managed.py:569, internal/relay/managed/engine.go:762); docs/role-execution-policy.md:244 (`service declare --execution-policy`). A post-port CLI reduction must not read those rows as unused - out-of-scope - evidence: .omo/ulw-execute/scope-analysis-31-46.md "# 32", grep over plugins/ and docs/ at 3684949c.
- [todo32] internal/relay/{store/state.go:pathlibSpelling,store/ownership/record.go:pathlibSpelling,delivery/marker.go:absolutePath,delivery/intent_cli.go:pathlibString} - str(Path(x)) / Path.absolute() spelling is still implemented four times; todo 32 exported store.PathlibSpelling and store.AbsoluteExpanded and moved the launch declaration, doctor and service declare onto them, the rest can follow (none of them keeps a leading "//" as pathlib does, which is a separate parity question) - out-of-scope - evidence: TestLaunchPolicy_declare_records_the_path_as_spelled.
- [todo32] internal/relay/adapter/worker_observation.go - managed-start still reads worker-policy.json with its own reader and readiness (Read/Ready) beside service.ReadWorkerPolicy and cli.workerReadiness (see the todo29 entry); todo 32 pins all three to one Python oracle per reason (TestWorkerPolicy_every_reason_is_pythons_in_every_reader, TestWorkerPolicy_managed_start_readiness_is_pythons) and fixed the two parity defects that pinning found in it (a missing or unexaminable lock read as unheld rather than unreadable; the scope root taken from filepath.Abs rather than service.ResolveScope); making Read delegate to service.ReadWorkerPolicy would leave one reader - out-of-scope - evidence: internal/relay/service/testdata/worker_reasons.json, TestManagedStartReadsTheScopeTheServiceWrites.
- [todo32 review] internal/relay/store/{ownership.go:CheckStartLikeFence,ownership.go:StartPreflight,hold.go:checkStart,diagnostic_probe.go:ownershipPreflight} - ownership.check_start is read four ways: the marker commands' fence-order reading (mirror, then metadata; a legacy store passes; OS and SQLite failures raised as the host error), the service and daemon start preflight (every failure a store_owned_by_other refusal, a legacy store refused by design, Go's words where validate's are not the refusal's), the read-only opener's, and doctor's probe. The service and daemon preflight could take CheckStartLikeFence's mirror and metadata reading and keep only its legacy refusal, which would give a broken store the fence's host error and words there too; that changes todo 30's surface and its tests, so it was left - out-of-scope - evidence: TestCLI_marker_preflight_answers_what_python_answers (the broken-store cases), internal/relay/cli/daemon.go ownershipPreflight.
- [todo32 review] internal/pyjson - json.loads is now one dependency-neutral package (byte decoding, the scanner's refusals and positions) under both the bridge's policy parser and the relay; internal/relay/store/pyloads.go:LoadsJSON (values) and internal/bridge/execution/decode.go:builder (the policy's values) still build values twice over a scanned document, and internal/dev/ci/pyvalue.go:decodeUTF8 is the second UTF-8 decoder the todo24 entry names (internal/relay/store/pyutf8.go there now lives at internal/pyjson/utf8.go, store.DecodeUTF8 delegating to it) - out-of-scope - evidence: internal/pyjson/testdata/decode.json.
