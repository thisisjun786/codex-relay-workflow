# Todo 24 JSON accessor audit

Basis: Python source in `packages/codex-session-relay/src/codex_session_relay`.
This is a compatibility fix, not stricter validation. Wrong types remain values
where Python keeps them, use Python `str`, `bool`, `int`, iteration and identity
semantics where Python does, and fail with the Python exception where it raises.

## Shared mechanism

`evidence/forge_value.go` extends fix X's accessors, rather than adding a second
set: `Dict`, `Items`, `Integer`, `Whole`, `HashKey`, `Equal`, `Item`, `Index`, `Len`,
strict `Iter`, and ordered `Decode`. `Items` implements `value or []`, whereas
`Iter` rejects false/zero/None just as direct Python iteration does.
`PythonError` is the only recovered panic type. The CLI
renders it as a host failure. Transactional expression failures must be recovered
inside the transaction callback, so they return an error and roll back; runtime
panics are not disguised as Python failures.

The paginator retains a raw, optional total and a raw cursor. Missing totals are
not zero. Dictionary/set identities distinguish strings from numbers and reject
unhashable containers. Job sorting evaluates its keys even for a single job,
retains arbitrary-size integer attempts, and does not clamp negative attempts.

## Reachable input paths

| Go location | Python counterpart | Behavior and proof |
| --- | --- | --- |
| evidence/collector.go GraphQL, enumerateREST, enumerateGraphQL | forge.py graphql, rest_step, cursor_step | Missing/raw totals, truthy pagination, iterable length and exact dict errors; real forge subprocess cases. |
| evidence/collector.go candidate, threadFinding, collectDiscussion | forge.py _candidate, _thread_finding, _discussion | Python truthiness, raw author fields, Python text excerpts, index and mapping failures. |
| evidence/collector.go collectChecks | forge.py _checks | int conversion, hashability, numeric identity, stable tuple ordering, no attempt clamp. |
| evidence/collector.go collectGates | forge.py _gates | Fix X's rule semantics retained; ASCII digest and query error excerpts; branch URL quoting. |
| evidence/mergeevidence.go | mergeevidence.py candidate_problems, review_state_problems, shape_problems, checks_problems | Falsy state is unknown; arbitrary-size integer attempts remain integers. |
| supervisor/show.go, readback.go | supervisorchannel.py show, reach, _establishes | Frozen non-objects are displayed, not asserted as maps; malformed readback shapes raise; origin is rendered with str and unhashable origins fail. |
| supervisor/observation.go, reading_subset.go, standing.go; cli/supervisor.go | supervision.py from_observation, unusable_reading; supervisorchannel.py _selectors | Preserve observation numeric kinds and reason values, Python repr in diagnostics, and selector string conversion. |
| delivery/report_render.go, report_revision.go, report_compose.go | report.py _handoff_lines, _evidence_lines, _unresolved_lines, _restore_lines, compose_revision | Stored wrong-shaped values iterate, render or fail as Python; no empty fallback for truthy wrong shapes. |
| delivery/omitted.go | omitted.py observe | A truthy non-path workspace raises the Path type error, reported as evidence_unreadable. |
| supervisor/proposal.go, send.go | supervisorchannel.py _live_proposal, render | Frozen packet dictionary/index reads and number kinds; typed failures inside claim roll back. |
| evidence/envelope.go | envelope.py shown, stage_holds, promotion_refused, check_reach | Truthy detail text and Python mapping reads, no runtime assertion on external values. |
| supervisor/report_record.go, report_review.go, report_subset.go | report.py record, _check_dispositions, _check_review; cxc.py | Wrong status/kind are named, not replaced by empty strings. Disposition result follows threadsSeen, including missing-disposition refusal. |
| managed/engine.go business result | managed.py _business_result | Present wrong-typed status uses str(status); absent status remains unknown. |

`Test24JSONAccessBytes` compares the built multicall binary to the installed
Python console entry point, stdout/stderr bytes, exit code, and every table of an
identical 85-table SQLite seed. The only normalization is sampled timestamps.
The default package test runs 31 representative cases, including one proved
mutation-killing case for each of the 12 shared accessor behaviors. It is neither
skipped under race/short nor given a longer package timeout. The full 1,614-case
matrix is a separately invoked harness; the independent checker's 640-case la.py
and 36-case st.py matrices also remain separate and retain all their cases. Only
scratch paths and the removed checker venv path are adapted.

The default four-core test run also keeps representative cases from the CLI argparse,
merge-evidence defect, fix-X, and numeric-downstream matrices. Until todo 44 their exhaustive
expansions ran under the `parity` build tag (`make parity`) against live Python: every
command/argument parser case, every merge-evidence defect and fix-X input, every numeric
downstream action, and the 500-case in-process parser-oracle equivalence sample. Todo 44 removed
the Python implementation, and those expansions, the tag and the target went with it; the
representatives replay Python's recorded answers (`docs/port/oracles/g2.md`).
The bounded default representatives are: four commands spanning intent/reporting/supervisor/
merge-evidence parser families; one rich paginated merge-evidence run; one nested wrong-shaped
required-check rule; and one huge integer plus one NaN downstream action. Parser and defect
representatives are mutation-backed by the generated-spec drift and JSON-access mutation suites;
the fix-X representative is killed by changing `Dict` to accept its wrong-shaped context; the
numeric representatives are killed by replacing arbitrary-size integer/NaN conversion with Go
machine-number parsing.

Until todo 44 the exhaustive JSON-access harness (`internal/relay/cli/testdata/json_access.py`,
run with the workspace Python against a built binary) and the development-only mutation runner
(`json_access_mutations.py`) were checked in beside the tests. The mutation runner first passed
those same 31 representatives, then built one mutant at a time and required an actual byte/table
diff, restoring each source file by byte copy and verifying it with `cmp` even after failure.
Both were Python drivers and left with the Python implementation; the record below is what they
measured.

The 12 CLI mutants covered dict access, string conversion, truthiness, integer conversion, absent totals,
hashability, ASCII dumps, length, numeric indexing, required keys, iteration,
and the marker workspace boundary. Four additional library mutants cover strict
iteration, envelope detail conversion, managed business status and report status.
All 16 produce actual diffs, not compilation failures. The old CLI forge fake now routes by the
actual GraphQL field, so reviews/comments cannot accidentally return thread
payloads; the production strict reader is unchanged by that fixture correction.

`Test24ReportAccessorPython` exercises 72 report-validator inputs against live
Python; `Test24ReportAccessorRefusalWritesNothing` checks refused writes. There
is no report-record CLI registered in either build, and Go cmdEmit has no report
or handoff flag/caller. The report library is tested directly rather than adding
a fictitious product command solely to make a binary test possible. Likewise,
supervisor-send/read and managed-start do not have a runnable host implementation
in this checkout: library integration tests are the executable surface available
here. `Test24EnvelopeAccessorPython` compares 72 malformed envelope inputs;
`Test24PacketAccessorPython` compares 84 mutated frozen packets against the actual
Python renderer, including complete rendered bytes. `Test24ManagedReceiptAccessorPython`
compares 12 transport status values against Python, runs each through the real Go
registration path twice, and asserts no admission or duplicate transport effect.
`Test24MalformedProposalRollsBackClaim` verifies three malformed stored packets
return typed errors without changing the message or writing an attempt.

## Reads not reachable with a wrong JSON type

- `supervisor/standing.go:ReportHolds`: standing, obligationId, decision.report,
  relationId are constructed by Standing/Selection immediately before this call;
  identifiers are typed Obligation fields. No stored JSON enters this intermediate.
- `supervisor/proposal.go` and `stage.go` obligation basis eventId: derived from a
  TEXT event_id SQL column by FromEvent, not a caller JSON value.
- `supervisor/hierarchy.go`: production StoreLinkage.Up constructs levels, owner,
  scopeKey, readable and contention from typed SQL rows. StaticLinkage in tests
  can inject values, but it is not a packet/file reader. No JSON source reaches
  these particular assertions.
- `supervisor/autosend.go:objText/objBool`: OmissionReadings derives reportingState
  and owed locally from the classifier; it does not replay an external reading.
- `supervisor/readback.go` delivered.reason: every branch constructs a string
  locally; the adapter exposes typed TokenScan and TurnInfo, not untyped JSON.
- `supervisor/report_record.go` coverage and threadsSeen: HandoffProblems runs
  shape validation first; required strings, checks and draft types are explicit
  Python-compatible validators, not conversion fallbacks.
- `managed/reservation.go` threadId/turnId: Python explicitly requires nonblank
  strings and otherwise records partial. These are intentional isinstance checks.
- `managed/engine.go` remaining str calls: ParseRequest validates request strings;
  receipt identities are passed through ValidSegment (Python also rejects all
  nonstrings); status comparisons only recognize literal strings. Only business
  status diagnostic interpolation needed conversion.
- `delivery/intent_cli.go` claims/intent/standing: container types are checked by
  Malformed/malformedDisposition before use; claim names must be strings.
- `delivery/omitted.go` identity/dbPath/claim reads: Malformed checks those identity
  fields, Correlated requires names; workspace is not in that validator and is
  therefore fixed separately. scanOmissionContext JSON is built by SQL json_object
  and json_group_array in the same read, not parsed from a stored JSON column.
- `delivery/verify.go`: string lists and environments have passed textList and
  environmentsProblem; expected settings have passed RequireUsable.
- `delivery/service.go` linkage readings: constructed by the typed linkage API.
  Restoration findings come from the already validated receipt contract.
- `evidence/report.go`: text assertions are SQLite TEXT-affinity columns; required
  names deliberately validate list-of-strings, matching report.required_for_candidate.
- CLI flag-value assertions and decoder object-key assertions follow parser and
  JSON decoder guarantees; they are not loose JSON field reads.

## json.dumps option audit

- Forge digest: default separators, sorted keys, ensure_ascii=True. Query errors:
  default separators, insertion order, ensure_ascii=True, excerpt after dumping.
- Supervisor packet and frozen reading: default separators, sorted keys,
  ensure_ascii=False, as supervisorchannel explicitly requests.
- Supervisor withheld/pre-send/expired-claim records: default separators, sorted
  keys, ensure_ascii=True. Settled transport record and readback detail explicitly
  use ensure_ascii=False. Journal calls use default separators/ASCII, field order.
- Supervision blocked-cause hash explicitly uses compact separators and
  ensure_ascii=False. No change to this identity contract.
- Work report and handoff JSON use Python default separators/ASCII and validator
  field order. Grant requiredDeclared explicitly uses ensure_ascii=False.
- Equality-only dumps are not persisted or shown; their options are symmetric.
  The settings comparison now also spells Python's ensure_ascii=True explicitly.

## Boundaries

Excluded registry, faults, mergeturn, cli/policy and capacity loose reads are
listed in refactor-backlog.md, not modified. The independently established 460
numeric/whitespace differences remain byte-identical to their e5b10415 entries.
A pre-existing preview-context difference with a live relationship is recorded
there too; legacy-event renderer probes avoid conflating it with JSON semantics.
No commit, installation, live state access or .omo write belongs to this change.

## Final verification

- Default-timeout race command over relay, contracttest and cmd passed twice.
  CLI package durations: 391.794 seconds and 388.203 seconds. The representative
  byte test itself took 8.20 seconds in the final focused run. No race/short skip
  and no package-timeout increase was introduced.
- Separate accessor matrix: 1,614/1,614 equal. Independent checker: la.py
  640/640 and st.py 36/36 equal, with no differing fields.
- All 16 final mutations failed with a behavioral diff; byte-copy restoration
  passed cmp. Earlier mutation variants that did not compile were rejected as
  invalid evidence, not counted as killed mutants.
- Parser sweep: 169,070/169,070. Merge evidence: 296/296 in each rule mode and
  1,042/1,042 additional cases. Fix-X edge cases: 35/35.
- Numeric/whitespace: 9,672/10,132 equal. Each of the 460 differing case entries
  matches its e5b10415 baseline entry, including outputs; the complete difference
  list also equals fix X's list.
- Stateful replay: 22/22 steps and 85/85 tables in each invocation mode; schema
  equal and zero table differences.
- Build, vet, lint, test-map, inventory, contract dump, generated-spec drift,
  program-prefix tests and git diff --check passed. Test-map counts are 121
  files / 6,053 tests; inventory is 124 files / 99,462 lines; contract tree is
  144 top-level commands / 152 nodes.
- Full CLI and argparse test-binary straces passed from their package directories
  with GODEBUG=asyncpreemptoff=1. All 9,023,257 CLI trace lines and 24,577 argparse
  trace lines were scanned: zero accesses below the user's live state or Codex
  directories. No task test/build/harness process remained at final check.

Limits: library-only paths use live Python library comparisons rather than a
nonexistent host CLI. The malformed-proposal test checks rollback locally, not
all-table Python replay. This work does not prove general JSON parser equivalence
for non-finite constants, float overflow, or every nested order-sensitive repr.
The valid-preview relationship-context difference noted above has not received a
separate baseline-binary proof here. No additional probe classes were started
after the final orchestrator instruction; these limits remain explicit.
