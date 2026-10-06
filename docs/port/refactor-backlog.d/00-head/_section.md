# Refactor backlog (Go port)

Purpose: structural and design problems found during the Go port that are NOT parity, contract or invariant failures. They are recorded here and deliberately not fixed, because the port's oracle (Python-equivalent by property, byte-exact on contract surfaces) would stop distinguishing a port bug from an intended change. This file is the input to a post-port crw-refactor pass that Jun authorizes separately, after todo 44 (Python removal) and the cutover (todos 42-45). No Linear issues are created from it during the port.

Related records kept where they already live (not moved here): docs/port/decisions.md section 20 (Python defects inherited for parity), docs/port/known-defects.md (Python defects not carried over, intentional replacements), and the `risks` fields of .omo/ulw-execute/ledger.jsonl.

Rules: append-only, one bullet per entry, never edited by a later todo (parallel branches merge as the union). The file is assembled from the fragments under `docs/port/refactor-backlog.d`: add an entry by writing one bullet in a one-entry file `<ISSUE>-<n>.md` in the directory of the section it belongs to, then run `crw-dev ci refactor-backlog --write` and commit the fragment and the regenerated file together. Parity, contract and invariant defects are fixed under decisions.md, not recorded here; if an entry turns out to be one, say so in the entry.

Entry format:
`- [todoNN|checkNN|orchestrator] <Go path or Python origin> - <what> - <why deferred: parity|contract|out-of-scope> - evidence: <commit or evidence file>`
