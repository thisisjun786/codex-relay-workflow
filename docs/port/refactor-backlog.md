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
