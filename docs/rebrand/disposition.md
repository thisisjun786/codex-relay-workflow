# PABCD disposition in the current flow

This audit separates the old loop's name and orchestration from the capabilities
the current runtime still uses. It records a disposition; it does not remove code,
rename a command, migrate stored state or change an installed runtime.

The inspected `origin/dev` revision is
`73aa64cacba835eea3a8f1edf3908ff268b09a90`. Paths and function names below refer to
that revision. A registered CLI handler is reachable product code, even when the
flow engine does not call it. An import proves a dependency, not the frequency of
live execution.

## Inventory and method

The literal, case-sensitive string `pabcd` occurs **5,787 times in 1,285 tracked
text files** at the inspected revision. Binary files are excluded; tests, fixtures,
generated text and historical documents are included. These are occurrence counts,
not counts of active features or deletion candidates.

| Scope | Matching files | Occurrences |
| --- | --- | --- |
| All tracked text | 1,285 | 5,787 |
| `internal/pabcd/` | 392 | 1,417 |
| `contract/fixtures/` | 293 | 507 |
| `docs/port-cxc/` | 172 | 1,247 |
| `plugins/crw/` | 51 | 196 |

The scoped rows are subsets of the total, not an exhaustive partition. Reproduce
the text inventory against the fixed revision, rather than including this new
document in its own baseline:

```sh
git grep -I -l -e pabcd 73aa64cacba835eea3a8f1edf3908ff268b09a90 -- | wc -l
git grep -I -o -e pabcd 73aa64cacba835eea3a8f1edf3908ff268b09a90 -- | wc -l
```

There are **19 immediate directories and 21 Go packages** under `internal/pabcd`.
The two additional packages are `interview/ledger` and `source/session`; both are
listed separately below. `internal/harness.Verbs()` registers **13** top-level
`crw pabcd` verbs.

The package audit used Go 1.27.1 on linux/amd64:

```sh
go list -json -tags=dev,integration ./...
go list -deps ./cmd/crw
```

The first command supplies ordinary `Imports` separately from `TestImports` and
`XTestImports`, including developer and integration packages. The second confirms
that **all 21 packages are dependencies of the default product binary**. None of
the 21 packages is held only by tests. Tests and recorded fixtures are additional
consumers; they are not the reason these packages remain live.

The import check describes this build configuration. Platform-specific deletion
work must also check the supported release targets before removing code. This
audit authorizes no deletion on any target.

## Disposition meanings

- **`keep` — 남김:** keep the capability as an independent utility, guard,
  maintenance surface or explicitly isolated compatibility component. This does
  not preserve the PABCD brand or require the current package path forever.
- **`merge` — 새 흐름에 합침:** make the existing flow, session, configuration or
  evidence owner responsible for the capability. The destination column is a
  proposed responsibility boundary, not a claim that a replacement command or
  migration already exists. Keep the current implementation until callers and
  stored contracts have been transferred.
- **`delete` — 지움:** eligible only when no non-test product importer remains;
  a registered command is not eligible. Old-loop importers still count. Lack of
  a direct flow import is insufficient. The guard conservatively also counts
  developer-tool imports and imports of nested packages.

**No package or registered command currently qualifies for `delete`.** In
particular, removing the old phase vocabulary does not justify deleting shared
source identity, filesystem protection or evidence acceptance.

## Package disposition

Every row names a Go package. “Legacy path” below means a current product CLI or
hook path, not a test-only use. The two nested rows belong to their named families
but are separate packages in the Go graph.

| Kind | Target | Disposition | Current use and evidence | Destination or removal condition |
| --- | --- | --- | --- | --- |
| package | `internal/pabcd/attest` | merge | Ordinary importers: `cli`, `fsm`, `hook`. `hook/prompt_orchestrate.go` calls `ValidatePlanArtifacts` and `ValidateWorkPhaseBinding`; `cli/orchestrate_args.go` decodes `Attestation`. No direct flow import. | Transfer source-bound work and plan proof rules to flow's planning and gate boundaries; do not transplant the I/P/A/B/C/D transition machine. Keep the old adapters until their attestation readers are replaced. |
| package | `internal/pabcd/cli` | merge | `internal/harness/pabcd_cli.go` imports it and calls its parsers/runners from the registered verbs. The flow engine uses shared owners directly, not this adapter package. | Distribute commands to the corresponding flow, configuration, evidence and maintenance owners. Preserve stream, exit-code, cancellation and native-session checks while compatibility entry points remain. |
| package | `internal/pabcd/crwdir` | merge | `flow/engine/session_evidence.go` uses `RequireDir`; `flow/ops/verify_ci.go` uses `EnsureSubdir` and `WriteExclusive`. `relay/job` uses `CheckTree`, `OpenFile`, `NotLink` and `Rename`. Many other runtime consumers use the same filesystem rules. | Shared runtime state-directory owner used by flow, relay and hooks. Preserve link refusal, exclusive creation, permissions and atomic publication; changing the import path must not silently move `.crw` data. |
| package | `internal/pabcd/evidence` | merge | `flow/engine/session_evidence.go` calls `NewFlowAssignment`, `EnsureFlowAssignment`, `RecordedAssignment` and `AcceptedReceipt`. `role/spawn/guard.go`, legacy CLI, gate and hook code also import it. | Flow session-call evidence plus the hook/role acceptance boundary. Preserve attempt identity, placement-before-publication, claim/release and receipt provenance; do not replace these assignments with a bare CI PASS. |
| package | `internal/pabcd/fsm` | keep | Ordinary importers are legacy `cli` and `hook`; `cli/loop_unbind.go` calls `ApplyHumanTransition`. It is linked through both current CLI and hooks, with no direct flow import. | Isolate as compatibility for existing phase state until those CLI/hook consumers are retired or redirected. The new flow must not start a second PABCD state machine. Only a later audit with no remaining product importers may mark it delete. |
| package | `internal/pabcd/gate` | merge | `cli`, `goalplan` and `hook` import it. `cli/loop.go` calls `ParseSourceBoundReceipt`; `cli/orchestrate_dclose.go` calls `ValidateCheckReceipt`. Flow's `ops/finalgate.go` separately uses source comparisons, not this package directly. | Transfer the applicable receipt/source validation rules to flow's verification and final-gate boundaries. Preserve current readers until equivalence is demonstrated; do not equate old phase completion with a flow milestone PASS. |
| package | `internal/pabcd/goalplan` | merge | Legacy CLI/hook/reset/review import it. `role/spawn/finalgate.go` calls `ValidateGoalplanSlug` and reads the plan through `GoalplansSubdir`/`GoalplanFile`; developer fuzzing is an additional consumer. | Flow plan input, workflow progress and journal ownership. Migrate frozen-plan/work-phase identities and review bindings explicitly; retain readers for existing goalplan files until migration is complete. |
| package | `internal/pabcd/hook` | merge | `cmd/crw/component_hooks.go` directly calls `GitHubPostInput`, `GitHubPostDecision` and `ChildPushDecision`. `internal/harness/hook.go` dispatches hook legs; `affordance` and installation migration also import this package. | Keep security and evidence guards at hook ingress, and separate them from obsolete loop prompts/transitions. A name cleanup must not disable the post/push guards or flow receipt acceptance. |
| package | `internal/pabcd/host` | merge | Direct consumers include flow engine/command rendering, relay session/job, bridge, management, runtime and role code. `flow/engine/cli.go` reads the flow snapshot with `host.LookupEnv`; `flow/crwcmd` uses `Invocation`, `BinEnv` and `ShellWord`. | Shared host identity and executable-resolution boundary. Preserve environment lookup, native session identity, installed-runtime resolution and quoting while moving ownership out of the old loop namespace. |
| package | `internal/pabcd/interview` | merge | `harness/freeze.go` calls `FreezeCommand` over session state. CLI, FSM, goalplan, state and the nested ledger package also import it. No direct flow/relay/bridge/integrator import. | Flow planning/input clarification and frozen acceptance input, retaining the explicit opt-in policy. Move the useful input contract without restoring an automatic interview phase to every flow. |
| package | `internal/pabcd/interview/ledger` | merge | `cli/scan_record.go` reads `ReadQaEvents`; `cli/orchestrate_transition.go` calls `DimensionsBackedByAnswers`; hooks also import it. These are product paths, not only interview tests. | Transfer interview answer/scan provenance with its input owner. Preserve append history, session binding and unreadable-ledger refusal until old records have a supported reader. |
| package | `internal/pabcd/metric` | keep | CLI and hooks use metric/divergence records. Independently of the old loop, `internal/skill/search/scoring.go` calls `CompareTimestamps`. | Keep the shared timestamp/order utility and optional recorded metrics. Separate these consumers from phase-specific telemetry before retiring that telemetry; do not remove the package merely because flow has its own usage accounting. |
| package | `internal/pabcd/projectcfg` | merge | `harness/pabcd_cli.go` reads/writes interview policy. `harness/hook.go` calls `PabcdEnabled` before gated legs; hook implementations also read configuration. | The runtime configuration owner, with explicit migration of existing keys and defaults. Preserve opt-in behavior; moving a key must not enable old gated hooks or interviews accidentally. |
| package | `internal/pabcd/reset` | keep | `internal/harness/pabcd_reset.go` calls `ParseResetScope`, `RunReset` and `RenderReset` from the registered reset verb. State tests are additional consumers. | Keep bounded operator maintenance for existing state while it exists. Any future replacement must preserve scope and lock checks; removal waits for both the command and old-state maintenance need to disappear. |
| package | `internal/pabcd/review` | merge | CLI/hook review rounds use it. `internal/relay/delivery/report_revision.go` directly uses `MaxBlockers` and `BlockerSuffix` to render a verdict; this is an independent relay dependency. | Shared review result/binding contracts used by flow review and relay delivery. Preserve verdict encoding and old round readers; rename does not authorize changing accepted evidence or blocker semantics. |
| package | `internal/pabcd/shellir` | keep | `hook/childpush.go` calls `AnalyzeScriptEnv`; GitHub-post guards inspect its `Exec`, `Word` and command/script structure. The hook is registered in the product binary. | Keep shell analysis as a security utility. Rehome it independently of the loop name if needed; no deletion or permissive string-match substitute while permission guards consume it. |
| package | `internal/pabcd/source` | merge | `flow/backend/agent` calls `CaptureContext`; `flow/engine.SourceReader` returns `Identity`; `flow/ops/finalgate.go` and `qa.go` call `CommitIdentity` and `Compare`. Relay session and role spawn also consume it. | Shared source provenance for flow verification, QA and evidence. Preserve commit/dirty-tree identity and comparison semantics; this transfer is a prerequisite to removing the old namespace, not an optional follow-up. |
| package | `internal/pabcd/source/session` | merge | `relay/session/cli.go` calls `Bind`, `Resolve` and `Capture`; `role/spawn/finalgate.go` calls session-aware `Capture`. Legacy CLI/gate/hook are additional importers. | Shared native-session-to-source binding behind relay and flow evidence. Move with source identity and preserve alias/foreign-session refusal and the recorded source root. |
| package | `internal/pabcd/state` | merge | Relay session reads `StatePath`, validates `AllPhases` and binds through `EnsureState`. Role spawn, migration and many legacy owners also import it; `harness/freeze.go` reads state for the interview tracker. | Split shared session identity/binding from old phase storage. Keep explicit readers/migration for old state and ledgers; do not reinterpret a stored phase as a flow journal event or discard unreadable records. |
| package | `internal/pabcd/stateroot` | merge | `bridge/mutations.go` and `relay/adapter/transport.go` call `Guard`/`Moved` on thread resume. `manage/childresume.go` uses `Resolve`, `Guard` and `Moved`. | Shared session relocation and state-root safety boundary. Preserve refusal before sending a turn and post-resume movement checks across bridge, relay and management. |
| package | `internal/pabcd/text` | keep | Used throughout the runtime, including `flow/crwcmd` (`Trim`), relay job parsing, command rendering, provider, recall and plugin wiring. It is not specific to phase transitions. | Keep the compatibility-sensitive text helpers as shared utilities. A namespace move may change imports, not trimming/encoding/order behavior or recorded fixture expectations without separate evidence. |

## Registered command disposition

The common entry path for every row is
`cmd/crw/main.go`'s `pabcd` registration → `harness.PabcdContext` →
`harness.Verbs()` in `internal/harness/pabcd.go`. The adapters are in
`internal/harness/pabcd_cli.go`, except `freeze.go` and `pabcd_reset.go`.
This registration is sufficient to rule out “test-only” for all 13 commands.

No dedicated invocation of these CLI verbs was found in flow, relay, bridge or
integrator product code. Shared-library calls are different: they are listed in
the package table. Flow's embedded executor instructions
(`internal/flow/roles/executor.md`, `executor-t1.md`, `executor-qa.md`) explicitly
forbid starting `crw pabcd orchestrate` or `crw pabcd loop` because the flow run
owns execution state. A caller can still supply an arbitrary command to a generic
command runner; that is not evidence of a built-in dependency on a particular verb.

| Kind | Target | Disposition | Current use and evidence | Destination or removal condition |
| --- | --- | --- | --- | --- |
| command | `crw pabcd freeze` | merge | `freezeVerb` → `interview.FreezeCommand`, with `state.ReadState` supplying the tracker. Registered product input-freeze surface; no dedicated new-flow CLI caller found. | Flow's frozen planning/input contract. Preserve the selected session and frozen answer evidence until the input owner has replaced this entry point. |
| command | `crw pabcd plan` | merge | `planVerb` → `cli.ParsePlanCliArgs` → `cli.RunPlanCli`. Registered legacy plan command, not the flow engine's plan-input implementation. | Flow plan input and validation. Define compatibility for existing plan artifacts before removing the old command; do not assume existing flow syntax accepts the same arguments. |
| command | `crw pabcd receipt` | merge | `receiptVerb` → `cli.RunReceiptCLI`; the writing test action checks `nativeSessionRefused` and carries context/stdin/stdout/stderr. | Verification/evidence owners. Preserve cancellation, publication and session/source binding; old receipt formats and `verification-record/1` are not interchangeable merely because both say pass. |
| command | `crw pabcd evidence` | merge | `evidenceVerb` → `cli.RunEvidenceCLI`, after the native-session mutation check. Flow already uses the evidence library without calling this verb. | The flow session-call evidence owner and its explicit operator interface. Keep receipt claim/resolve authority scoped to the recorded assignment. |
| command | `crw pabcd memory` | keep | `memoryVerb` → `cli.RunMemoryCLIContext`, with native-session checks and context-aware write behavior. This is a registered session permission surface. | Keep explicit memory-write permission maintenance separate from orchestration. Do not substitute recall/search for an authorization grant; retire the old spelling only with a compatible permission interface. |
| command | `crw pabcd reset` | keep | `resetVerb` → `reset.RunReset`. It is an explicit maintenance command and is intentionally outside the ordinary native-session mutation refusal used by other verbs. | Keep scoped maintenance for old state until cleanup/migration is complete. The new flow's cancellation/recovery is not by itself a replacement for resetting stored legacy state. |
| command | `crw pabcd config` | merge | `configVerb` owns the `interview` subcommand and calls `projectcfg.ReadPolicy`/`WritePolicy`; other config subcommands are refused. | The configuration owner, with an explicit old-key/default migration. This table proposes ownership; it does not announce a new supported `crw config` option. |
| command | `crw pabcd scan` | merge | `scanVerb` → `cli.RunScanCliContext`; `cli/scan_record.go` appends `ScanCompleted` to interview evidence under the session lock. It is not the repository's secret scanner. | Planning/input consistency evidence. Preserve recorded answers and cancellation-before-append; do not conflate this scan with GitHub verification or Gitleaks. |
| command | `crw pabcd review-round` | merge | `reviewRoundVerb` → `cli.RunReviewRoundCli`; the adapter applies native-session ownership before the runner. | Flow's review/evidence binding and the shared review owner. Preserve round identity, scope and refusal of stale or foreign evidence before redirecting callers. |
| command | `crw pabcd metric` | keep | `metricVerb` → `cli.RunMetricCLIContext`, including its input and interrupted-write handling. It remains a registered optional measurement surface. | Keep existing metric history readable and separate from control flow. Redesigning phase telemetry or merging its data with flow usage records requires an explicit data contract. |
| command | `crw pabcd divergence` | keep | `divergenceVerb` → `cli.RunDivergenceCliContext`; this is registered legacy measurement/state behavior, not flow scheduling. | Retain compatibility for existing divergence records until their consumers and interpretation have a replacement. Do not infer that a similarly named flow branch has the same semantics. |
| command | `crw pabcd loop` | merge | `loopVerb` → `cli.RunLoopCliContext`. Product CLI registration remains, while flow executor instructions forbid starting this second loop. | Flow workflow execution and journal ownership. Transfer needed bind/progress/recovery operations; do not nest the old loop or import its phase machine into a running flow. |
| command | `crw pabcd orchestrate` | merge | `orchestrateVerb` → `cli.RunOrchestrateRead` → `cli.RunOrchestrateTransitionContext` for mutations. Read/status behavior and transition authority are distinct; flow executors are told not to launch it. | Flow execution/control and evidence gates. Replace authorized transitions at their actual callers, preserve old status/history access during migration, and remove the registration only after that compatibility boundary is settled. |

## Required transfers for the new flow

**Source identity.** `flow/engine/call.go` asks its backend's `SourceOf` for the
completed call's source. The agent and session backends use `source.CaptureContext`
with state artifacts excluded. `flow/ops/finalgate.go` compares evidence with the
current integration head using `CommitIdentity`/`Compare`, and `qa.go` uses the
same identity rules. Move this contract as one shared owner with its dirty-tree
and exclusion rules; changing names must not broaden evidence reuse.

**State-directory protection.** `crwdir` is shared by evidence publication,
verification directories and relay background-job records. Replacing it with
ordinary directory creation would discard the current link checks and exclusive
publication rules. Transfer the shared filesystem primitive first, retain the
stored `.crw` layout until an explicit migration exists, then update callers.

**Session-call evidence.** `engine.ensureEvidence` creates a flow assignment;
`evidencePlace` journals placement before writing it; bind/recover/release paths
reuse or end the same attempt-bound assignment. The stop-hook/role side accepts
that receipt. Both sides must move together. A workflow journal row, a legacy
phase attestation and a CI verification record serve different acceptance
contracts and must not be collapsed into one unqualified “verified” flag.

**Relay, bridge and integrator.** Relay session binding consumes source/session,
state and host; relay job storage consumes crwdir; relay delivery consumes review
encoding. Bridge resume and relay transport consume stateroot guards. The
`internal/integrator` package has no direct PABCD import in the inspected graph:
`Runner.verify` in `internal/integrator/integrate.go` runs the configured
`VerifyCommand` with the head/base/record contract. If that command selects
`crw flow verify`, the flow verification adapter reaches the shared runtime
helpers. Configurable process execution is not a hard-coded call to the old
orchestrator, and an integrator import search alone cannot establish removal safety.

The migration order is therefore shared identity/filesystem/host safeguards,
then evidence producers and consumers, then plan/review/session-state adapters,
then CLI compatibility and the isolated old FSM. Final names and the wider
migration schedule are outside this disposition table.

## Deletion guard and limits

`internal/dev/ci/rebrand_disposition_test.go` reads the table rather than a second
list of decisions. It fails when the document is absent, a row is malformed, a
current package/registered verb is omitted, or a target is duplicated/unknown.
For `delete`, it checks ordinary imports from `go list` separately from test
imports and rejects still-registered CLI verbs. Nested package imports also block
deletion of their enclosing target.

Because this snapshot has no delete rows, the test additionally changes the
source package's decision to delete in memory and requires the **real**
`flow/engine → pabcd/source` import to be rejected. Synthetic cases distinguish
test-only imports from ordinary imports and cover nested imports and registered
commands. Thus an empty deletion set is not the sole exercise of the guard.

Run the focused contract and the changed-package lint from the repository root:

```sh
go test -tags dev -count=1 ./internal/dev/ci -run '^TestRebrandDisposition'
make lint-changed
```

This protects an import/registration claim, not semantic equivalence of a future
port, live host behavior, or the safety of deleting historical material. Contract
fixtures and port records are evidence, not executable callers; preserve their
provenance and licensed notices when planning later cleanup. Every implementation
move still needs its own behavior tests and a fresh consumer audit.
