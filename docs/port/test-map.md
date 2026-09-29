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

Files: 122
Tests: 5787
Class A: files=16 tests=633
Class B: files=88 tests=3368
Class C: files=18 tests=1786

## Todo 15 bridge property progress (2026-09-25)

Go tests in `internal/bridge`, each reviewed against its Python property:

| Python file | Todo-15 names | Equivalent Go tests | Deferred |
| --- | ---: | ---: | ---: |
| `test_bridge.py` | 74 | 74 | 0 |
| `test_worktree.py` | 28 | 27 | 1 deferred to todo 16: `test_mcp_isolated_launch_and_followup_are_durable` (MCP stdio entry) |
| `test_settings.py` | 17 | 17 | 0 |
| `test_execution.py` | 27 | 27 | 0 |
| **Total** | **146** | **145** | **1** |

The remaining MCP case is deferred to todo 16's "Expose the bridge as an MCP stdio server and wire `crw bridge`" and "QA scenarios: happy = stdio round trip create_thread against fakehost via an MCP client in Go test". Property parity is recorded row by row in the todo-15 ledger below. The todo-15 bridge fakehost work is separate from the 36 execution and 23 settings package properties counted under todo 14 in the file rows below.

## Todo 22 fault property progress (2026-09-26)

Ported by property in `internal/relay/faults`. Tests named `Test22_FLT_*` and `Test22_FLF_*`
cover the fault ledger, publications, sweep inputs, policy and budget settings, notification
lifecycle, retry/reconciliation, and live findings. Stateful scenarios replay the same operations
against the Go store and the real Python package and compare complete answers and persisted rows.
All fault command argument surfaces are also compared byte for byte with live Python for help,
missing required options, unknown options, option abbreviations, malformed observation JSON, and
nested `--kind-module` import failures. `TestFaultsDependencyInventoryContainsNoNetworkClient`
enforces the `test_faults.py` source-import property with `go list -deps ./internal/relay/faults/...`.

## Todo 21 part A delivery property progress (2026-09-26)

Ported by property (Jun, 2026-09-26). Go tests in `internal/relay/delivery`, named
`Test<ID>_...` after `.omo/ulw-execute/todo21-properties.md`; each runs the same scenario through
the real Python package over the same fixture tree and compares records, refusals and every table
row with Python's.

| Python file | properties | Go tests | python-internal (not ported) |
| --- | ---: | ---: | --- |
| `test_delivery.py` | 35 | 35 | - |
| `test_delivery_relation.py` | 5 | 4 | DRL-0 (drift guard between two Python spellings; Go has one constant) |
| `test_supersession.py` | 8 | 8 | - |
| `test_multi_parent_isolation.py` | 5 | 5 | - |
| `test_on_request_delivery.py` | 9 | 9 | - |
| `test_anchor_binding.py` | 8 | 8 | - |
| `test_criteria_registration.py` | 7 | 7 | - |
| `test_revision_roundtrip.py` | 5 | 5 | - |
| `test_verification_currency.py` | 13 | 12 | VCU-5 (`inspect.signature`; Go `RecordVerdict` has no bypass parameter) |
| **Total** | **95** | **93** | **2** |

The twelve delivery commands (`emit`, `deliver`, `reconcile`, `recover`, `claim`, `ack-proof`,
`ack`, `verdict`, `criteria-register`, `criteria-show`, `revision-head`, `verify-acks`) answer
byte for byte like Python (`TestCLI_every_delivery_command_answers_byte_for_byte_like_python`);
`deliver`, `reconcile`, `recover` and `verify-acks` with `--socket` need the host adapter
(todo 28).

## Todo 21 part B1 intent and marker property progress (2026-09-26)

Go tests in `internal/relay/delivery`, named `Test<ID>_...` after
`.omo/ulw-execute/todo21-properties.md`; each runs one JSON op list through the real `marker.py` /
`intent.py` (`testdata/markerops.py`) and the Go port over the same tree and compares the whole
answers (records, refusal reason and detail, derived states, fact reads).

| Python file | properties | Go tests | python-internal (not ported) |
| --- | ---: | ---: | --- |
| `test_intent.py` | 23 | 23 | - |
| `test_marker.py` | 8 | 8 | - |
| **Total** | **31** | **31** | **0** |

The eight marker commands (`intent-declare`, `intent-attempt`, `intent-bind`, `intent-register`,
`intent-claim`, `intent-disposition`, `intent-resolve`, `intent-show`), with the store records
`intent-claim` and `intent-disposition` mirror (declarations.py), answer byte for byte like Python
(`TestCLI_every_intent_command_answers_byte_for_byte_like_python`); `guard-evaluate` is todo 33.
Since todo 32 the fence's ownership checks run where the fence runs them: `main`'s `check_start`
before the two marker forms that name the selected store (`intent-declare` without `--no-db-path`,
`intent-register` without `--db-path`), `cmd_intent_claim`'s on the store the intent names, and
`intent-disposition`'s `declarations.Held` opening it. A store another runtime owns, or one
draining, refuses them before any marker write; a legacy store passes; a broken one answers the
refusal or host error the fence does. `TestCLI_marker_preflight_answers_what_python_answers`
checks 29 such cases against Python's answers in `testdata/marker_preflight.json`, captured once by
the parity-tagged `TestCLI_marker_preflight_parity_with_live_python` (`make parity`), so the
default suite starts no Python for them.

## Todo 21 part B2 host-loss and unknown-send property progress (2026-09-26)

Go tests in `internal/relay/delivery`, named `Test21_<ID>_...` after
`.omo/ulw-execute/todo21-properties.md`. `testdata/capture.py` runs every Python test of the file,
each in its own tree, and records every value it asserts; the Go twin of each test runs the same
steps in the same tree and must produce the same values, delivery tables, `delivery_stalled`
fault rows and host sends.

| Python file | properties | Go tests | python-internal (not ported) |
| --- | ---: | ---: | --- |
| `test_host_lost_turn.py` | 29 | 29 | - |
| `test_unknown_send_lost.py` | 23 | 22 | USL-18 (`assertIs` on `cli.Services` wiring; covered by USL-15/17) |
| **Total** | **52** | **51** | **1** |

The fault sweep/ledger (todo 22) and the merge-turn grant rule (todo 26) these properties read are
ported as marked subsets in `internal/relay/faults` and `internal/relay/mergeturn`; the recipient
reads of the bridge adapter (todo 28) as `delivery.BridgeReads`.

## Todo 21 part C acknowledgement, re-review and recovery property progress (2026-09-26)

Same method as part B2 (`testdata/capture.py` + the Go twin of each Python test, every asserted
value and the delivery tables compared; the re-review tests also compare the review tables).

| Python file | properties | Go tests | python-internal (not ported) |
| --- | ---: | ---: | --- |
| `test_ack_reconcile.py` | 23 | 23 | - |
| `test_ack_disposition_race.py` | 3 | 3 | - |
| `test_rereview_deadlock.py` | 9 | 9 | - |
| `test_recovery_negatives.py` | 3 | 3 | - |
| **Total** | **38** | **38** | **0** |

Carried from todo 25A into `internal/relay/delivery`: `test_cli.py` CLI-5, CLI-7, CLI-9, CLI-21,
CLI-38 (`Test25_CLI*`, whole stdout against the Python command) and
`test_registration_contention.py` RCT-1, RCT-4 for their `intent.bind` marker half (`Test25_RCT*`;
the guard-evaluate half is todo 33).

Carried from todo 18 into todo 21: `test_attempt_message_atomicity.py` AMA-1..AMA-6
(`Test21_AMA1..6` in `internal/relay/delivery/attempt_message_atomicity_test.go`,
with `cmd_show` AMA-4..AMA-6 in `internal/relay/cli/attempt_message_atomicity_test.go`).
The Go mirrors compare the Python tests' captured assertions via `testdata/capture.py`;
AMA-6 pins oldest-first order independently in both Service.AttemptMessages and CLI show.
`Test21_RegistrationHold_refuses_resolved_live_state` pins the registration hold's
write admission against a symlink into a temporary live-state-shaped directory.

## Todo 37 runtime property progress (2026-09-29)

`scripts/ci/tests/test_runtime_install.py` (493 tests in 93 classes by AST count; the `tests`
cell's 496 is `grep -c`) and `test_install_acceptance.py` (61) are carried by property, not by
test (scope analysis "# 37"). Todo 37 owns the pointer, claim, lock, record-delta, swap-gate,
reading, scope, ownership, check-record and residue properties; the install, update, rollback,
remove, registration, hook and promotion-order classes are todo 38's and are listed there. The
Python answers are captured once by `internal/runtime/testdata/python_goldens.py` into
`goldens.json` (record bytes after seven update deltas, the 180-row staging table, 72
ownership rows, every swap-gate cell and verdict, scope summaries, shapes, the OPS-1.2 walk);
the `parity` tag regenerates them from live Python and adds cross-runtime lock, filesystem,
declared-schema and fault-sweep checks (`internal/runtime/record/parity_test.go`).

| Python class (tests) | property | Go test |
| --- | --- | --- |
| DefinitionTests (4) | the definition agrees with its sources; the OPS-1.2 walk | `TestDefinitionAgreesWithComponentsJSON`, `TestDigestIsTheOPS12Walk`; the digest-drift test retires with committed digests (decision 35) |
| OwnershipTests (4) | five classes in precedence; unreadable is never agreement; only own reusable | `TestClassifyIsPythons` (72 rows); the dirty-checkout fork retires with checkout signals |
| CheckRecordTests (4) | every result stated; untimed is "unknown"; temporary recorded | `TestCheckRecordStatesEveryResult`; `imported` retires |
| HostRecordTests (3), RecordShapeTests (3) | points bound to their dimensions; appended; round trip; container shapes | `TestPointsForComparesEveryDimensionByItsPolicy`, `TestV1RecordRoundTripsByteForByte`, `TestShapeRefusesWhatPythonRefuses`, `TestLoadKeepsFourAnswers` |
| UnreadableDimensionTests (3), DimensionCoverageTests (5) | a dimension's presence policy; gates are not dimensions | `TestPointsForComparesEveryDimensionByItsPolicy` (interpreter and exercise dimensions retire) |
| InstallEntriesCarryTheirRevisionTests (1) | each install entry carries its revision | `TestGoInstallEntryIsAdditive`, `TestSourceWithoutAStampIsNull`, `Test37_SweeperReadsTheGoInstallEntry` |
| AbsenceAnswerTests (7), SelectionRollbackTests (4), PointerOwnershipRollbackTests (3) | the record deltas that put a selection or pointer back to nothing are compare-and-remove | `TestUpdateWritesWhatPythonWrites` (deselect-and-restore, drop-pointer, drop-pointer-elsewhere); the rollback sequencing is todo 38 |
| ReadingBoundaryTests (7), FilesystemPartitionTests (12) | four states from four observations | `TestReadJSONKeepsFourAnswers`, `TestDecodeReadsLikePythonTextMode`, `TestSameDirectoryAsksTheFilesystem`; parity `TestParity_pointer_and_reading_states`; the per-command boundary inventory retires with the Python exception lattice |
| ScopeReadingTests (4), ServiceStateTests (4) | scope readings kept apart; the invocation first | `TestSummariseIsPythons`, `TestServiceStateIsPythons`, `TestRelayRunsTheSelectedExecutable`, `TestFilesystemCandidatesListEveryStoreFile` |
| StagingClaimTests (8), ClaimOwnershipTests (8), StagingLeftoverTests (4) | who may remove a staging: claim, lock, decision | `TestDecideIsPythonsTable` (180 rows), `TestClaimBytesArePythons`, `TestReadClaimKeepsFourAnswersAndOwnership`, `TestLivenessIsTheAdvisoryLock`, `TestAbandonedStagingIsReclaimedAndLiveStagingIsNot`, `TestCreateClaimsAnExclusiveDirectory` (todo 38), `TestClearOwnRemovesOnlyItsOwnFiles` |
| PointerSwapTests (4) | atomic repoint; real directory untouched; unread says nothing | `TestPlaceNamesRemove`, `TestARealDirectoryIsNotAPointer`, `TestAnUnreachablePointerNamesNothingEitherWay`, `TestARelativeTargetResolvesFromTheLinkDirectory`, `TestRepointingNeverLeavesTheLinkMissing` |
| SwapGateTests (11), GateCellCoverageTests (3), SchemaComparisonTests (4), SchemaDepthTests (7) | three cells, three verdicts, whole-statement schema comparison | `TestCellsAndVerdictsArePythons`, `TestBlockingComesOffTheDeclaredCells`, `TestStoreReadingsCreateNothingAndAgreeWithTheDeclaredSchema`, `TestCandidateSchemaIsAskedOfTheCandidate`; parity `TestParity_declared_schema_is_the_python_candidates` |
| BusyLockTests (2), PromotionExclusionTests (6), LockSiblingTests (6) | .crw-lock and promotion-lock exclusion; Busy is its own answer | `TestCrwLockIsExclusiveAndExpiresOnlyWhenStale`, `TestPromotionLockContentionAndTheFileIsNeverUnlinked`, `TestReleaseCandidateKeepsWhatMayBeInUse`, `TestDoctorReportsAHeldPromotionLockAndPromotionRefuses`; parity `TestParity_locks_exclude_across_runtimes` |
| DiagnosisReportsResidue (13), ResidueNeverNamesLiveWork (6), ResidueNeverGuessesAboutAPointer (18) | residue is the installer's own decision; a pointer needs placement evidence | `TestResidueIsWhatTheInstallerWouldReclaim`, `TestAPointerIsResidueOnlyWithPlacementEvidence`, `TestAnUnlistedDestinationIsReportedNotEmpty`, `TestDestinationsAreComparedByTheFilesystem`, `TestDoctorOnAPythonVenvOnlyHost` |
| ReviewFixTests (6), IdentityComparisonTests (7), LockCoverageTests (4), RoundTripSymmetryTests (6) | AST and inspect inventories of Python source | retired: tests of Python internals |

Todo 37's Go tests: 63 in `internal/runtime/...` (16 record, 3 reading, 5 pointer, 7 staging, 4
swapgate, 4 scope, 4 residue, 1 ownership, 2 definition, 12 doctor, and 5 parity-tagged in
record),
`Test37_SweeperReadsTheGoInstallEntry` in `internal/relay/faults` and
`TestRun_doctor_dispatches_to_the_host_doctor` in `cmd/crw`. New Go surface with no Python
counterpart: `crw doctor retention-scan` (`TestRetentionScanReportsExactlyTheSeededReferences`,
`TestRetentionScanResolvesThroughThePointer`, `TestRetentionScanReadsTheWiringSurfaces`,
`TestRetentionScanHoldsForARecentJournalRow`, `TestRetentionScanJudgesLiveProcesses`,
`TestShellWordsSplitsLikeSh`) and the selected runtime kind of `crw doctor`
(`TestDoctorOnAPythonVenvOnlyHost`, `TestDoctorOnAGoBinaryHost`).

## Todo 38 runtime property progress (2026-09-29)

The install, update, rollback, remove, registration, hook and promotion-order properties of
`test_runtime_install.py` and `test_install_acceptance.py` are carried by the Go tests of
`internal/runtime/install`, which install real archives of the real `crw` binary into temporary
homes against the fake App Server; no `inventory-check` destination remains (the reflective AST
inventories re-test Python source, scope analysis "# 38"). The settings-record bytes are compared
with the Python writers' through `internal/runtime/testdata/goldens.json` (`settingsDocuments`).

| Python class (tests) | property | Go test |
| --- | --- | --- |
| InstallOrderTests (3), BuildStepNamingTests (4), CleanHostFirstInstallTests (6), EntryPointTests (4) | stage, exercise, record, promote, settle, in that order; the three console names beside the binary | `TestInstallRecordsThePointerAndSettlesTheClaimLast` |
| UpdateRecoveryTests (7), RetriableDestinationTests (4), OwnershipReleaseTests (2), FailureContractTests (4); acceptance ComposedLifecycleTests (5) | a failure before promotion leaves the selection and pointer and releases only this run's directory; a failure after the settings transition (commit, placement, a placement that does not read back) puts the pointer, selection, ownership entry, outgoing and settings back; update, rollback, remove | `TestAFailedUpdateLeavesThePreviousRuntime`, `TestAHeldPromotionLockRefusesAndReleasesTheCandidate`, `TestAFailedPromotionPutsBackEverythingItChanged`, `TestUpdateRollbackAndRemove`, `TestUnpackRefusesAnythingButTheReleaseLayout` |
| SettledRecordTests (25), InterruptedPromotionTests (2), ResumeGateTests (3), ResumeConflictTests (2), IdempotentRepeatTests (5), ReclaimRaceTests (4), NarrowReadingTests (2) | the claim settles last and exit 3 is promoted-not-recorded; a committed promotion resumes; a repeat is already installed; an abandoned staging is reclaimed, a finished one kept, a claimless one foreign | `TestAnInterruptedPromotionIsResumedAndAnUnsettledClaimIsExit3`, `TestAnExistingDirectoryIsDecidedOnItsClaim` |
| PromotionFreshnessTests (4), OverlappingUpdateTests (3), PromotionPointerPathTests (2), PointerPathShapeTests (2), RecordedTargetTests (3), RollbackRaceTests (2); acceptance DaemonStateTests (5) | every promotion judgment reads the record inside the lock; the gate blocks; a restore undoes only this run's write | `TestARunningDaemonOfTheSelectedRuntimeBlocksTheSwap` (a relay daemon of the selected runtime), `TestAStoreSchemaTheCandidateNarrowsBlocksTheSwap`, `TestUpdateRollbackAndRemove` (rollback, rollback again, named rollback); the gate cells are todo 37's |
| PointerOwnershipTests (1), PointerOwnershipLifetimeTests (20), AbsentPointerRollbackTests (5), SettledPointerTests (1) | a link this record never placed is not replaced | `TestAnUnrecordedPointerIsNotReplaced`; the compare-and-replace deltas are todo 37's |
| ConflictCallerTests (7), ConfigScannerTests (6), ReaderDomainTests (5), AuthorizedRepairTests (14); acceptance DistinctionTests (7) | one owner per surface, read with a real TOML reader; an unreadable config.toml refuses; nothing writes config.toml or hooks.json | `TestInstallRefusesASecondOwner`, `TestRegisterMCPWritesTheRecordAndRefusesASecondOwner`, `TestHookWritesTheGoSettingsAndRefusesASecondOwner`; the append-writer cases retire with `register --owner user` |
| HookTests (3), WriteSidePromiseTests (7), PartialApplicationTests (2) | settings and record bytes are Python's and read back; a policy that changed is refused; the Stop settings move with the pointer, replaced in one rename and kept when the write fails | `TestBridgeRecordBytesArePythons`, `TestHookSettingsBytesArePythons`, `TestSupersedeCopiesAsideAndLeavesThePath`, `TestAFailedSettingsWriteKeepsThePythonEraSettings`, `TestPythonEraSettingsMoveWithThePointer`, `TestLegacyStopLaunchersReachTheGoHook`, `TestTheWiringLaunchersStartTheGoBridgeUnderTheRecordedPolicy` |
| LinkConflictTests (4) | a skill link CONFLICT | retired with runtime_install.py's skill-link reading (scope analysis "# 39"); the CONFLICT property itself is `crw-dev skills link`'s (todo 39) |
| InheritedRegistrationTests (2), InheritedRegistrationScopeTests (1), LegacyInstallTests (6), TrialArgumentTests (6), TrialIdentityTests (2), TrialStepGatingTests (3), PreflightCorpusTests (4), SettingsPreflightTests (4), PreflightInputSetTests (3), PairedMemberTests (11), InterpreterIdentityTests (3), RecordedEnvironmentBindingTests (4), SpacedDestinationTests (1), ClaimedComparisonTests (2), StorePlaceTests (4), RelayRuleCoverageTests (2), SelectorCoverageTests (4) | retired surfaces: the inherited registration, pre-claim adoption, `diagnose --trial`, interpreter binding | retired (docs/port/inventory.md, Functions retired with evidence) |
| ReadbackCoverageTests (15), ReadbackSweepControlTests (5), ReadbackUnderLockTests (4), ScannerSightTests (3), DeclaredSetReferenceTests (6), DrawnSetTests (10), OwnReadingTests (5), BorrowedAnswerTests (5), UnclaimedReadingTests (4), IncompleteReadingTests (6), StrengthAndSiteTests (6), BodilessTestTests (3); acceptance SevenReadingsTests (38), ProvenanceTests (6) | AST and inspect inventories of Python source | retired: tests of Python internals |

Todo 38's Go tests: 25 in `internal/runtime/install`, `TestCreateClaimsAnExclusiveDirectory` in
`internal/runtime/staging` (replacing todo 37's temporary-sibling staging, which the install
flow does not use) and `TestRun_install_dispatches_to_the_installer` in `cmd/crw`. New Go surface
with no Python counterpart: the release archive's SHA256SUMS verification
(`TestADigestMismatchIsRefusedBeforeUnpacking`, `TestReleaseAssetsAreFetchedAndVerified`) and the
`crw install` command line (`TestTheCommandLine`).

## Todo 39 skill-link and launcher-removal property progress (2026-09-29)

The transition retired rather than being ported (scope analysis "# 39"): `scripts/plugin_transition.py`,
`scripts/crw_transition/steps.py` and `scripts/ci/tests/test_plugin_transition.py` (241 tests,
class C, ast/importlib over crw_transition; two of them, `RetiredCommandsRefuse`, proved todo 34's
refusal of the retired commands and go with the tool) are deleted, with the evidence in
[the inventory](inventory.md#files-deleted-with-evidence). What survives of them is the skill
linker and the ownership-checked removal of the fallback launcher.

| Python test (tests) | property | Go test |
| --- | --- | --- |
| test_install.py `test_missing_check_never_writes` | `--check` on a missing install writes nothing and exits 1 | `internal/dev/skills` `TestCheckOnAMissingInstallWritesNothing` |
| test_install.py `test_apply_check_and_idempotent_apply` | apply links every skill; check and apply again change nothing | `TestApplyLinksEverySkillAndARerunChangesNothing` |
| test_install.py `test_foreign_paths_preserved_and_preflight_is_all_or_nothing` | a file, directory, foreign or dangling link at one name is CONFLICT and nothing is written | `TestAnyConflictWritesNothingAndLeavesThePathAsItWas` |
| test_install.py `test_default_uses_isolated_codex_home` | the default destination is `$CODEX_HOME/skills` | `TestTheCommandLinksThisCheckoutIntoTheCodexHome` (with the usage errors, exit 2) |
| test_install.py `test_links_that_name_the_previous_skill_path_are_kept` | a link through the root `skills` alias is LINKED | `TestALinkThroughTheRootSkillsAliasIsLinked` |
| test_install.py `test_declared_skills_path_must_stay_inside_the_plugin` | the manifest's skills path stays inside the plugin, lexically and through a symlink | `TestTheDeclaredSkillsPathMustStayInsideThePlugin` (and `internal/dev/ci` `Test47_PLG_7_SkillsPathStaysInside`) |
| (untested in Python) | a path created between the preflight and the write is refused, not replaced, and the links already made are kept | `TestAPathThatAppearsAfterThePreflightIsNeverReplaced` |
| (untested in Python) | a leading `~` or `~name` is expanded on the destination wherever it comes from, `CODEX_HOME` as well as `--dest`, as install.py's `expanduser` does; an unknown user, and `~` with HOME empty or unset, are usage errors, never a directory named `~` inside the checkout | `TestATildeDestinationIsExpandedWhereverItComesFrom` |
| test_install.py retired-name tests (3) | `LEGACY` reporting of `linear-*` and `crw-focus` | dropped with the detection |
| test_plugin_wiring.py StableLauncherRemovalTest: nothing there, dry run, the file it installed, without the marker, a symlink (5) | removal takes only a regular file carrying `crw-stop-hook/1`, never follows a link, and an absent file is a no-op | `internal/runtime/install` `TestRemoveLauncherOnAnAbsentFileIsANoOp`, `TestRemoveLauncherTakesTheFileItPlacedAndNothingElse`, `TestRemoveLauncherRefusesAFileWithoutTheMarker`, `TestRemoveLauncherNeverFollowsALinkOrADirectory` |
| (the second proof and the lock, untested in Python) | the marker is proved again under the launcher's own `.crw-lock`, the lock the Python placement takes; the wait and the unlink stop when the caller's context ends | `TestRemoveLauncherProvesTheMarkerAgainUnderTheLock`, `TestRemoveLauncherWaitsOnTheLockThePlacementTakes`, `TestRemoveLauncherInterruptedRemovesNothing` |
| StableLauncherRemovalTest: settings written back, a surface that came back, its exit status, disable never claims the fallback (4); PluginGuardBudgetTest `test_the_transition_and_the_installer_share_one_bound` (1) | the transition's `live_again` aggregate and its shared bound | retired with the transition; the Go remover never touches the settings and reports `settingsPresent` as it found them |

## Todo 46 developer-harness property progress (2026-09-29)

Todo 46 ports two readings to the development binary and retires the rest (scope analysis
"# Todo 46"). `scripts/hook_comparison.py`, `scripts/ci/tests/test_hook_comparison.py` (127
tests, class C: the harness's own report shape plus Python-only items) and
`docs/hook-comparison.md` are deleted, with the evidence in
[the inventory](inventory.md#files-deleted-with-evidence). `scripts/stop_events.py`,
`scripts/trial_startup.py` and their tests stay until todo 44: the first protects the Python
adapter, the second is the Python install's preflight.

| Python test (tests) | property | Go test |
| --- | --- | --- |
| test_stop_events.py VerifierTests and the verifier cases of the review-round classes | SEV-5 TRUE and its counters; SEV-6 FALSE; SEV-7 unjudged invocations; SEV-8 ledger integrity; SEV-9 row integrity; SEV-10 records of one event agree; SEV-11 only what the writer writes; SEV-12 the writer's on-disk form; SEV-13 a refused transcript path; SEV-14 window bounds | `internal/dev/stopevents` `TestSEV05_*` .. `TestSEV14_*`, over records the real `crw hook` writes, then mutated; `internal/dev/pyload` (the judges' `json.loads`, as deep as CPython 3.14 reads) |
| test_stop_events.py `test_a_complete_event_reads_true`, `test_rows_from_before_event_identity_are_legacy_and_never_judged` (corpus) | the two frozen `verify` fixtures | `internal/contracttest` hook domain, through a `crw-dev` built once per test binary |
| test_stop_events.py RealPathControls, ReviewRoundOne/Two/Four writer cases | SEV-1..4, the writer's | the Go hook's tests (todo 33) and the corpus |
| test_trial_startup.py Ledger (9), the ledger cases of the hosted rounds | TSU-13 counted apart, TSU-14 and TSU-24 refusals, TSU-30 and TSU-37 (ledger halves) graded without an installation once the window closed, TSU-70 the window opens at the dispatch, TSU-71 corroboration carries only compared times, TSU-90 operator words are text | `internal/dev/trialledger` `TestLedgerGradesAsThePythonLedgerDid` (70 cases against goldens from the Python ledger), `TestTSU13_*`, `TestTSU71_*`, `TestLedgerRefusals`, `TestTSU30_*`, `TestFromISOFormatIsCPythons`, `TestLedgerReadsRecordsAsDeepAsPython` |
| test_trial_startup.py preflight classes | the preflight's readings of the Python install | dropped with the preflight (todo 44) |
| test_hook_comparison.py (127) | HKC-1..46 | retired with the harness |

## Files

| path | tests | class | family | fixtures | owner | destination | coupling |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `packages/codex-session-relay/tests/test_fence_readonly.py` | 10 | B | foreign-owner read matrix, the owner's fault-next lease expiry and terminal inbox replay | takeover-inbox legacy golden; ChannelTestCase | todo 31/36 CRW-152 | go-test: internal/relay/store read-only and inbox | - |
| `packages/codex-session-relay/tests/test_fence.py` | 61 | A | ownership fence, first-socket store binding, decision 25 ingress and inbox replay, Python control socket, one receipt read per Stop, marker-only Stops ask no owner | contract/golden/takeover-inbox | todo 31/36 CRW-152 | go-test: internal/relay/store takeover protocol and socket binding + go-test: internal/relay/cli fence parity + go-test: internal/relay/hook control socket (todo 33) + go-test: internal/relay/inbox and internal/relay/cli inbox (todo 31) | - |
| `packages/codex-session-relay/tests/test_takeover_candidate.py` | 14 | A | decision 28 private designation, bounded channel frames, ready only after recovery and the control socket, durable activation | isolated SQLite and inherited socketpair | todo 30/36 CRW-152 | go-test: internal/relay/service candidate protocol | - |
| `packages/codex-session-relay/tests/test_ack_disposition_race.py` | 5 | B | two processes acknowledging one event; which may win | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_ack_reconcile.py` | 40 | B | ACK, verdicts, reconciliation and restart recovery | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_anchor_binding.py` | 14 | B | an anchor binds on every route to dispatched | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_assignment.py` | 58 | B | assignment ledger: one issue, one responsible child | - | todo 25 / CRW-154 | go-test: `internal/relay/registry` (todo 25: Test25_ASG1..19, whole answers vs testdata/python_assignment.json) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | - |
| `packages/codex-session-relay/tests/test_attempt_message_atomicity.py` | 13 | B | a send carries the message its attempt froze | - | todo 18 / CRW-152 | go-test: `internal/relay/store` (todo 18); DeliveryService pre-claim seam, reconciliation and cmd_show preview halves of 6 cases -> todo 21 `internal/relay/delivery` and `internal/relay/cli` (`Test21_AMA1..6`, plus CLI AMA-4..6) | - |
| `packages/codex-session-relay/tests/test_bridge_adapter.py` | 75 | B | bridge adapter logic over an injected RPC surface | `codex_session_relay.fakehost` | todo 28 / CRW-154 | go-test: internal/relay/adapter Test28_* property captures (todo 28) | - |
| `packages/codex-session-relay/tests/test_bridge_load_roots.py` | 12 | B | a parent loaded by another task's bridge is still reached (CRW-235) | - | todo 28 / CRW-154 | go-test: internal/relay/adapter Test28_* property captures (todo 28) | - |
| `packages/codex-session-relay/tests/test_capacity.py` | 30 | B | capacity counting and refusal to infer | - | todo 27 / CRW-154 | go-test: `internal/relay/capacity` Test27_CAP1..CAP9 (todo 27 part A; every answer compared whole with testdata/python_capacity.json) + go-test: `internal/contracttest` TestCapacityCommands (5 capacity commands, stdout bytes and exit codes vs python_cli.json) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | - |
| `packages/codex-session-relay/tests/test_child_packets.py` | 82 | B | child packets a receiver can act on (CRW-149) | - | todo 23 / CRW-153 | go-test: `internal/relay/reception` (todo 23) | - |
| `packages/codex-session-relay/tests/test_cli.py` | 86 | C | command surface end to end in-process: flags, JSON, exits, host-free | fixtures/host (inline) | todo 25 / CRW-154 | corpus: cli-shape + corpus: exit-codes + inventory-check: settings-transformation coverage table in `internal/relay/registry` (todo 25) | ast + inspect.getsource over delivery/bridge_adapter/settings (:2390-2418) to derive the TaskSettings calls a send applies |
| `packages/codex-session-relay/tests/test_contract_corpus.py` | 3 | B | corpus discovery and CLI runner proof added in todo 6; not original class-A cases | `contract/fixtures/cli-shape` | todo 6 / CRW-140 | corpus: cli-shape | - |
| `packages/codex-session-relay/tests/test_coordination_cli.py` | 19 | A | operator surface of 26 coordination commands: argv shape, exits, JSON | `internal/relay/linkage/coordination_test.go` (CCL-1 linkage commands), `internal/relay/mergeturn/commands_test.go` (CCL-1 merge-turn commands), `internal/contracttest/mergeturn_commands_test.go` (CCL-2..CCL-9, byte-identical replay of `internal/relay/mergeturn/testdata/cli_cases.json`); CCL-10..CCL-14 capacity/region pending todo 27 | todo 26 / CRW-154 | corpus: cli-shape; CCL-10..14 `internal/contracttest` Test27_CCL10..14 (Python-output replays) | - |
| `packages/codex-session-relay/tests/test_coordination_contract.py` | 11 | C | coordination modules: one txn per mutator, no own wait loops, lock wait declared once | CCT-1 Python-internal (not ported); CCT-2 `internal/relay/mergeturn/partc_test.go` Test26_CCT_2_one_bounded_write_then_an_answer | CCT-1 python-internal (not ported); CCT-2 go-test Test26_CCT_2 vs Python counts | go-test: transaction-count cases (todo 26C) | ast over linkage/mergeturn source (:22, :59) |
| `packages/codex-session-relay/tests/test_criteria_registration.py` | 9 | B | criteria ensure never replaces a set | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_daemon.py` | 19 | B | bounded daemon: automatic invocation, silence when idle | - | todo 29 / CRW-155 | go-test: `internal/relay/daemon` (todo 29) | - |
| `packages/codex-session-relay/tests/test_daemon_cadence.py` | 24 | B | daemon polling cadence supplied by the CLI | `codex_session_relay.fakehost` | todo 29 / CRW-155 | go-test: `internal/relay/daemon` (todo 29) | - |
| `packages/codex-session-relay/tests/test_delivery.py` | 75 | B | delivery: busy handling, dispatch, bounds, reporting (JUN-91) | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_delivery_relation.py` | 14 | B | recipient resolved from linkage, not frozen parent | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_diagnostics.py` | 32 | B | diagnosis words that tell an operator what to do | - | todo 20 / CRW-152 | go-test: `internal/relay/cli/diagnostics_test.go` (32 tests -> 23 properties, each run by the real Python test and compared as whole `status`/`show` JSON) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | - |
| `packages/codex-session-relay/tests/test_directive_places.py` | 20 | B | instruction purpose decides competition; held report named (CRW-230) | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` Test24_DIR_1..9 | - |
| `packages/codex-session-relay/tests/test_dispositions.py` | 61 | A | list stopped children through the real CLI (CRW-163) | - | todo 25 / CRW-154 | corpus: cli-shape + go-test: `internal/relay/registry` (todo 25: Test25_DSP1..13, whole stdout vs testdata/python_dispositions.json; DSP-12 in internal/contracttest) | - |
| `packages/codex-session-relay/tests/test_edit_regions.py` | 73 | B | edit-region agreements and their limits | - | todo 27 / CRW-154 | go-test: `internal/relay/capacity` Test27_EDR1..EDR14 (todo 27 part A; every answer compared whole with testdata/python_editregion.json) + go-test: `internal/contracttest` TestRegionCommands (8 region commands, stdout bytes and exit codes vs python_region_cli.json) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | - |
| `packages/codex-session-relay/tests/test_enqueue_durability.py` | 8 | B | terminal observation never outlives its queuing (JUN-167) | - | todo 29 / CRW-155 | go-test: `internal/relay/daemon` (todo 29) | - |
| `packages/codex-session-relay/tests/test_failure_recovery.py` | 15 | C | recovery from stopped process / locked store; bounded read-only connections | - | todo 17 / CRW-152 | inventory-check: go/ast scan that every SQLite open in `internal/relay/store` uses the declared bound (todo 17) + go-test: recovery cases (todo 21) | ast over sibling modules via intent.__file__ (:19, :527) |
| `packages/codex-session-relay/tests/test_fairness.py` | 22 | B | per-parent fairness in the daemon | - | todo 29 / CRW-155 | go-test: `internal/relay/daemon` (todo 29) | - |
| `packages/codex-session-relay/tests/test_fault_contract.py` | 120 | C | corrected fault-ledger contract, one class per blocker/finding | - | todo 22 / CRW-153 | go-test: `internal/relay/faults` (todo 22); the inspect.signature parameter assertions are inherited by the Go compiler (typed call sites in the tests) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | inspect.signature on faults/faultsweep (:8, :401, :604, :1794, :1810) |
| `packages/codex-session-relay/tests/test_fault_live_findings.py` | 23 | B | CRW-205 live findings the collector missed | - | todo 22 / CRW-153 | go-test: `internal/relay/faults` (todo 22) | - |
| `packages/codex-session-relay/tests/test_fault_notices.py` | 54 | B | fault notification goes up the supervisor channel once | - | todo 22 / CRW-153 | go-test: `internal/relay/faults` (todo 22) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | - |
| `packages/codex-session-relay/tests/test_faults.py` | 122 | C | fault ledger: one breakage, one record, closed by reverification | - | todo 22 / CRW-153 | go-test: `internal/relay/faults` (todo 22) + inventory-check: `go list -deps ./internal/relay/faults/...` contains no network client (todo 22) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | ast import scan of faults.py/faultsweep.py (:8, :1279) |
| `packages/codex-session-relay/tests/test_forge_evidence.py` | 73 | B | merge-evidence collector answers independent of reader care | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_guard.py` | 122 | B | Stop decision judged against real receipts | - | todo 33 / CRW-156 | go-test: `internal/relay/hook` (todo 33) | - |
| `packages/codex-session-relay/tests/test_guard_property.py` | 58 | C | every guard evaluation yields a recorded, classified result; no sentinel discarded | - | todo 33 / CRW-156 | inventory-check: go/ast scan over `internal/relay/hook` and `internal/relay/store` for discarded (value, ok) and (value, err) results (todo 33) + go-test: evaluation classes (todo 33) | ast over src/codex_session_relay sentinel modules (:21, :500+) |
| `packages/codex-session-relay/tests/test_host_lost_turn.py` | 98 | B | host-accepted-then-lost turn vs lost ACK (CRW-224) | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_identity.py` | 13 | B | identity derivations vs frozen canonical rendering | - | todo 18 / CRW-152 | go-test: `internal/relay/store` (todo 18) + corpus: event-key | - |
| `packages/codex-session-relay/tests/test_intent.py` | 58 | B | management intent and its state | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_launch_policy.py` | 26 | B | what a service launches its daemon with and why | - | todo 32 / CRW-155 | go-test: `internal/relay/service` TestLaunchPolicy_* (todo 32: ResolveLaunchPolicyAt against 78 declarations resolved once by Python in testdata/launch_policy.json, the policy file's bytes decoded and scanned as json.loads does among them; doctor, status, packet-check and `service run` reading that one resolution and refusing first; `service declare` spelling and failures; the digest the bridge parser and Python compute) + go-test: `internal/relay/service` Test29LaunchPolicyPersistence, Test29D3LaunchSnapshotTwenty (todo 29: declare/status/forget and the start/restart launch snapshot against live Python) | - |
| `packages/codex-session-relay/tests/test_linkage.py` | 132 | B | three-level linkage: identity, role contract, refusals, handover | - | todo 26 / CRW-154 | go-test: `internal/relay/linkage` Test26_LNK1..LNK29 and Test26_LNK_literal_reasons (whole-JSON parity with testdata/python_linkage.json from gen_linkage.py; implementation in `internal/relay/registry` linkage_*.go) + go-test: `internal/contracttest` TestLinkageCommands (12 linkage-* commands, stdout bytes and exit codes vs python_cli.json) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | - |
| `packages/codex-session-relay/tests/test_linkage_peer.py` | 16 | B | peer parent links | - | todo 26 / CRW-154 | go-test: linkage package (todo 26) | - |
| `packages/codex-session-relay/tests/test_linkage_queries.py` | 14 | B | bidirectional hierarchy queries | - | todo 26 / CRW-154 | go-test: linkage package (todo 26) | - |
| `packages/codex-session-relay/tests/test_linkage_recovery.py` | 15 | B | two-level store compatibility and interrupted transitions | `codex_session_relay.fakehost` | todo 26 / CRW-154 | go-test: linkage package (todo 26) | - |
| `packages/codex-session-relay/tests/test_managed_adapter_ledger.py` | 4 | B | managed adapter ledger follows the explicit store | - | todo 28 / CRW-154 | go-test: internal/relay/adapter Test28_* property captures (todo 28) | - |
| `packages/codex-session-relay/tests/test_managed_execution.py` | 3 | B | managed start sequence as instructed to a coordinator | - | todo 27 / CRW-154 | go-test: managed-start package (todo 27) | - |
| `packages/codex-session-relay/tests/test_managed_reservation.py` | 15 | B | managed admission reservation: one pending request, one owner | - | todo 27 / CRW-154 | go-test: managed-start package (todo 27) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | - |
| `packages/codex-session-relay/tests/test_managed_start.py` | 35 | B | managed entry drives registry/criteria/marker state | - | todo 27 / CRW-154 | go-test: managed-start package (todo 27) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | - |
| `packages/codex-session-relay/tests/test_management_cli.py` | 28 | A | managed marker through the real command line | - | todo 32 / CRW-155 | corpus: cli-shape + corpus: records | - |
| `packages/codex-session-relay/tests/test_manifest_scope.py` | 51 | B | MANIFEST-CANON-01, path containment, artifact-read stability | - | todo 28 / CRW-154 | go-test: internal/relay/adapter Test28_MSC_*; store scope reused (todo 28) | - |
| `packages/codex-session-relay/tests/test_marker.py` | 17 | B | create-once marker write protocol | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_merge_evidence.py` | 36 | B | mergeevidence predicates agree with the landed merge turn | `tests/fixtures/merge_turn_oracle.json` | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_merge_target.py` | 19 | B | merge target base branch read from real git repos (CRW-229) | - | todo 26 / CRW-154 | go-test: `internal/relay/mergeturn/{target,forge_target,target_cli,commands}_test.go` (MTG-1..8; forge HTTP status via httptest, whole-CLI parity covered by MTG-6 oracle) | - |
| `packages/codex-session-relay/tests/test_merge_turn.py` | 142 | B | whose turn it is to merge | `tests/fixtures/merge_turn_crossed_handoff.json` | todo 26 / CRW-154 | go-test: merge-turn package (todo 26; `internal/relay/mergeturn/{mergeturn,partc}_test.go`, oracle `testdata/gen_mergeturn.py`, MTN-19 Python-internal) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | - |
| `packages/codex-session-relay/tests/test_merge_turn_wake.py` | 62 | B | freed merge target reaches the parent it was handed to | - | todo 26 / CRW-154 | go-test: `internal/relay/mergeturn/{wake,wake_reports}_test.go` and `internal/relay/delivery/mergegrant_test.go` (MTW-1..10; fake-host dispatch, report readings, whole-JSON parity covered by MTW oracle) | - |
| `packages/codex-session-relay/tests/test_multi_parent_isolation.py` | 8 | B | two parents, two repos, one store | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_observation_budget.py` | 22 | B | observation budget never skips the current generation | - | todo 29 / CRW-155 | go-test: `internal/relay/daemon` (todo 29) | - |
| `packages/codex-session-relay/tests/test_omitted.py` | 35 | B | persisted Stop vs relay settlement as separate evidence | - | todo 24 / CRW-153 | go-test: `internal/relay/delivery` Test24_OMI_1..22 (OMI-3 carried to todo 29 daemon integration) | omitted.py carried from todo 21 |
| `packages/codex-session-relay/tests/test_on_request_delivery.py` | 11 | B | on-request recipients carried; approvals not answered (CRW-225) | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_operational_scale.py` | 8 | B | how far/how much a bounded daemon carries | `codex_session_relay.fakehost` | todo 29 / CRW-155 | go-test: `internal/relay/daemon` (todo 29) | - |
| `packages/codex-session-relay/tests/test_product_routing.py` | 82 | B | product routing against the fault ledger (CRW-206 matrix) | - | todo 23 / CRW-153 | go-test: `internal/relay/routing` (todo 23) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | - |
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
| `packages/codex-session-relay/tests/test_restoration_visibility.py` | 37 | B | restoration block reaches the child (CRW-94) | `src/codex_session_relay/schema/*.json` | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` Test24_RES_1..15 via live Python report captures | - |
| `packages/codex-session-relay/tests/test_revision_correction_form.py` | 23 | B | needs_changes revision request carries the correction form | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` Test24_RCF_1..15 via live Python report captures | - |
| `packages/codex-session-relay/tests/test_revision_roundtrip.py` | 13 | B | correction lineage across a real needs_changes generation | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_rolepolicy.py` | 45 | B | task role checked against recorded authorization | - | todo 25 / CRW-154 | go-test: `internal/relay/registry` (todo 25: Test25_ROL1..22, whole answers vs testdata/python_rolepolicy.json) | - |
| `packages/codex-session-relay/tests/test_schema_conformance.py` | 15 | B | persisted records conform to the frozen schemas | `src/codex_session_relay/schema/*.json`, `codex_session_relay.fakehost` | todo 18 / CRW-152 | go-test: `internal/relay/store` (todo 18) + corpus: records | - |
| `packages/codex-session-relay/tests/test_service.py` | 124 | B | who owns the daemon and who may stop it | - | todo 29 / CRW-155 | go-test: `internal/relay/daemon` (todo 29) | - |
| `packages/codex-session-relay/tests/test_settings_hold_naming.py` | 31 | B | settings hold names reason and recovery (CRW-235) | - | todo 25 / CRW-154 | go-test: `internal/relay/registry` (todo 25) | - |
| `packages/codex-session-relay/tests/test_settings_preservation.py` | 36 | B | execution settings carried; refuse to send without them | - | todo 25 / CRW-154 | go-test: `internal/relay/registry` (todo 25) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | - |
| `packages/codex-session-relay/tests/test_stop_adapter.py` | 18 | A | Stop adapter process: stdin, settings, exit, concurrency, fallback guard deadline | `packages/codex-session-relay/tests/fixtures/stop_event_r1.json` | todo 33 / CRW-156 | corpus: hook | - |
| `packages/codex-session-relay/tests/test_store.py` | 65 | B | durable store: a failed transition is never a success | - | todo 18 / CRW-152 | go-test: `internal/relay/store` (todo 18) | - |
| `packages/codex-session-relay/tests/test_store_reception.py` | 74 | B | packet-check --receiver against a real store | - | todo 23 / CRW-153 | go-test: `internal/relay/reception` (todo 23) | - |
| `packages/codex-session-relay/tests/test_supersession.py` | 22 | B | stale event stopped before the send | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_supervisor_autosend.py` | 22 | B | daemon tick stages and sends what the supervisor is owed | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` Test24_AUT_1,3,4,5,7,8; AUT-2,6,9,10,11 carried to todo 29 daemon loop/cmd/cadence | - |
| `packages/codex-session-relay/tests/test_supervisor_channel.py` | 165 | B | parent-to-supervisor channel: staged, sent, read back | `codex_session_relay.fakehost` | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | - |
| `packages/codex-session-relay/tests/test_supervisor_envelope.py` | 43 | B | recipient can tell what was asked and what was not said (CRW-148) | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_supervisor_live_findings.py` | 22 | B | CRW-215 live round-trip findings | `codex_session_relay.fakehost` | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` Test24_SLF_1..15 | --socket wire round trip remains todo 28 |
| `packages/codex-session-relay/tests/test_supervisor_omission_store.py` | 29 | B | omitted report derived from the store and sent | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` Test24_SOS_1..19_WholeOutputAndSQLite (live Python, incl. the StageUnsent tick step) + go-test: `internal/relay/delivery` omission classifier cases | - |
| `packages/codex-session-relay/tests/test_supervisor_reporting.py` | 70 | B | what the level above is told and still owed (CRW-148) | - | todo 24 / CRW-153 | go-test: `internal/relay/supervisor` (todo 24) | - |
| `packages/codex-session-relay/tests/test_sync_outbox.py` | 76 | B | coordination-document outbox: durable, retried, idempotency | `tests/linear_readback_fixtures.py` | todo 23 / CRW-153 | go-test: `internal/relay/sync` (todo 23) + go-test: `internal/relay/store` (todo 19: typed queries, guard-index refusals and Python-store parity for the tables this file writes) | - |
| `packages/codex-session-relay/tests/test_transfer_phases.py` | 7 | B | relay handling of an expired transfer phase | - | todo 28 / CRW-154 | go-test: internal/relay/adapter Test28_* property captures (todo 28) | - |
| `packages/codex-session-relay/tests/test_unknown_send_lost.py` | 69 | B | uncertain send with no trace held, never resent (CRW-231) | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_verification_currency.py` | 41 | B | verification currency, canonical criteria, lineage | - | todo 21 / CRW-153 | go-test: `internal/relay/delivery` (todo 21) | - |
| `packages/codex-session-relay/tests/test_worker_policy.py` | 18 | A | policy read from the serving process, not the asking shell | - | todo 32 / CRW-155 | go-test: `internal/relay/service` TestWorkerPolicy_every_reason_is_pythons_in_every_reader, TestWorkerPolicy_managed_start_readiness_is_pythons (todo 32: a live worker fixture per case over a copy of one store, every read_worker_policy reason from the service reader, doctor and managed-start against testdata/worker_reasons.json captured once from this file's harness) + go-test: `internal/relay/cli` TestWorkerReadiness_every_reason_is_pythons (todo 32: every worker_readiness reason) | the corpus runner has no live worker process (contract/notes/test_coordination_cli.md); the two kept cases (mid-read record change, lock I/O failure) are the record-changes and lock-missing Go cases |
| `packages/codex-session-relay/tests/test_wp1_regressions.py` | 34 | B | wp1 independent-review regressions (receipts, scope) | - | todo 18 / CRW-152 | go-test: `internal/relay/store` (todo 18) | - |
| `packages/codex-thread-bridge/tests/test_approval_routing.py` | 8 | B | approval-class requests left for the thread's approver (CRW-225) | `tests/conftest.py` fake App Server | todo 13 / CRW-151 | go-test: `internal/bridge/appserver` (todo 13) | - |
| `packages/codex-thread-bridge/tests/test_bridge.py` | 74 | B | create/followup/duplicate/conflict/replay through the bridge | `tests/conftest.py` fake App Server | todo 15 / CRW-151 | go-test: `internal/bridge` (todo 15) | - |
| `packages/codex-thread-bridge/tests/test_execution.py` | 63 | B | execution guard refusals; nothing dispatched | `tests/conftest.py` fake App Server | todo 14 / CRW-151 | go-test: `internal/bridge/execution` (todo 14, 36 of 63) | fake-host no-dispatch cases verified in todo 15: test_a_creation_without_a_stated_pair_sends_nothing, test_a_refusal_leaves_the_request_id_usable, test_a_creation_for_the_wrong_role_never_reaches_the_host, test_a_corrected_role_request_succeeds_under_the_same_id, test_naming_no_role_leaves_the_request_identical_to_one_made_before_roles_existed, test_an_unapproved_pair_is_refused_before_any_call, test_a_cited_exception_that_does_not_exist_or_does_not_match_sends_nothing, test_an_authorized_pair_is_the_one_transmitted_and_the_one_compared, test_an_approved_pair_records_the_mode_it_was_approved_under, test_an_unavailable_model_is_an_error_and_not_a_substitution, test_a_resume_without_a_full_pair_never_reads_or_resumes_the_thread, test_a_resume_carries_the_authorized_pair_and_is_checked_against_it, test_an_unapproved_pair_is_refused_on_the_resume_path_too, test_a_resume_that_disagrees_withholds_the_message, test_the_launch_and_the_comparison_follow_the_authorization_not_the_arguments, test_a_caller_mutating_its_settings_cannot_split_identity_from_dispatch, test_an_exception_on_the_resume_path_must_state_its_directory, test_a_loaded_thread_reports_its_own_pair_and_the_message_is_withheld, test_an_unloaded_thread_never_lets_an_echo_stand_as_proof_of_preservation, test_a_resume_for_the_wrong_role_never_reads_or_resumes_the_thread, test_a_worktree_launch_for_the_wrong_role_creates_nothing, test_a_named_supervisor_the_host_has_not_loaded_is_not_resumed_at_all, test_an_exception_is_not_evidence_that_a_pair_matches_its_roles_policy, test_a_send_that_names_no_role_is_not_guarded_here_and_that_boundary_is_deliberate, test_a_verified_role_pair_may_still_be_sent_to_a_thread_the_host_has_not_loaded, test_a_host_that_declared_no_roles_keeps_exactly_its_previous_send_behaviour, test_naming_no_role_produces_the_same_request_identity_as_before_roles_existed |
| `packages/codex-thread-bridge/tests/test_contract_corpus.py` | 4 | B | corpus discovery and git/App Server runner proofs added in todo 6; not original class-A cases | `contract/fixtures/git`, `contract/fixtures/appserver` | todo 6 / CRW-140 | corpus: git + corpus: appserver | - |
| `packages/codex-thread-bridge/tests/test_ledger.py` | 10 | B | operation ledger: socket aliases, legacy import, re-arm, fail-closed relay state without the relay package, a fence lock alone decided by the relay admission | - | todo 14 / CRW-151 | go-test: `internal/bridge/ledger` (todo 14) + corpus: ledger-fingerprint | - |
| `packages/codex-thread-bridge/tests/test_mcp.py` | 11 | A | stdio MCP server: schema refusals, host policy, plugin launch | `tests/conftest.py` fake App Server | todo 16 / CRW-151 | corpus: mcp-tools | todo 16: all 11 scenarios run against the built `crw bridge` by the Go `mcp` runner, green under `CRW_CONTRACT_STRICT=1`; 9 properties (the two digest-refusal and unusable-policy tests are one property: a policy that cannot be used stops the server before any ledger; the two plugin-launch tests are one property: the bridge under the host's bare environment reports and enforces exactly the registered policy) plus contract surfaces in `internal/contracttest/{mcpnorm_test,mcp_parity_test}.go` (live tools/list equals `contract/schema/bridge-mcp-tools.json`; 30 whole replies equal recorded Python output) |
| `packages/codex-thread-bridge/tests/test_rpc.py` | 20 | B | App Server RPC: multiplexing, errors, timeouts, frame limits | `tests/conftest.py` fake App Server | todo 13 / CRW-151 | go-test: `internal/bridge/appserver` (todo 13) + corpus: appserver | - |
| `packages/codex-thread-bridge/tests/test_settings.py` | 40 | B | settings survive create/resume; distinct causes | `tests/conftest.py` fake App Server | todo 14 / CRW-151 | go-test: `internal/bridge/settings` (todo 14, 23 of 40) | fake-host no-dispatch cases verified in todo 15: test_first_full_request_can_use_opus_and_xhigh, test_a_clean_resume_starts_its_turn_with_no_overrides, test_a_reused_id_replays_without_dispatching_again, test_a_reused_id_with_changed_settings_is_rejected, test_an_omitted_setting_keeps_the_pre_upgrade_fingerprint, test_a_failing_annotation_cannot_downgrade_an_accepted_turn, test_a_misspelled_setting_key_is_refused_not_discarded, test_a_replay_answers_from_the_ledger_without_touching_the_host, test_a_replay_is_answered_even_when_the_host_has_gone_away, test_a_cancelled_annotation_propagates_instead_of_completing, test_transmitting_a_policy_would_have_relaxed_an_interactive_thread, test_a_declared_delivery_still_leaves_a_setter_host_untouched, test_an_approval_request_during_the_turn_is_left_undecided, test_a_refused_send_is_preserved_as_undelivered, test_recovery_processes_one_refused_message_exactly_once, test_an_accepted_delivery_is_not_reported_as_completed_work, test_an_explicit_null_declaration_is_refused_rather_than_read_as_never |
| `packages/codex-thread-bridge/tests/test_worktree.py` | 28 | A | create_worktree against real git: exact base, collisions, replay | `tests/conftest.py` fake App Server | todo 15 / CRW-151 | corpus: mcp-tools + go-test: `internal/bridge/worktrees` (todo 15) | - |
| `scripts/ci/tests/test_adapter_agreement.py` | 53 | A | packaged vs checkout adapter over the same invocations | `packages/codex-session-relay/tests/fixtures/stop_event_r1.json` | todo 33 / CRW-156 | corpus: hook | - |
| `scripts/ci/tests/test_completion_hook.py` | 221 | A | completion hook adapter vs a fake relay: journal, claims, exits | - | todo 33 / CRW-156 | corpus: hook | - |
| `scripts/ci/tests/test_gate.py` | 18 | C | dev-gate check selection, result judgement and parallel CI legs | `.github/workflows/ci.yml`, `Makefile` | todo 47 / CRW-160 | go-test: `crw-dev ci gate` (todo 47; `internal/dev/ci` Test47_GATE_1..10) | importlib spec_from_file_location of scripts/ci/gate.py |
| `scripts/ci/tests/test_install.py` | 9 | C | skill-link install: check/apply, idempotence, foreign paths | - | todo 39 / CRW-158 | go-test: `crw-dev skills link` (`internal/dev/skills`, todo 39) + drop: the 3 retired-name tests, because the `LEGACY` detection retired with its README migration guidance (scope analysis "# 39") | importlib load of scripts/install.py |
| `scripts/ci/tests/test_install_acceptance.py` | 61 | C | new install carried through to recovery of the replaced install | - | todo 38 / CRW-158 | go-test: `internal/runtime/install` (todo 38; lifecycle, failure restore, daemon gate, see the todo 38 table) + drop: SevenReadingsTests and ProvenanceTests re-test the Python fixture's own inventories (scope analysis "# 38") | ast over the installer and its fixture (494 reflective sites) |
| `scripts/ci/tests/test_packages.py` | 27 | C | packages check never prints success over an empty or skipped run; `--shard` splits whole modules disjointly and completely, balanced on recorded seconds | - | todo 47 / CRW-160 | drop: packages.py only runs the Python suites and is deleted in todo 44/47; the property (no success over an empty or skipped run) is inherited by todo 9's `CRW_CONTRACT_STRICT=1` skip counting and `go test` exit status; the shard split leaves with the Python suites it divides | importlib load of scripts/ci/packages.py |
| `scripts/ci/tests/test_parent_title.py` | 9 | A | parent-title helper command surface and coverage guard | `plugins/crw/skills/crw-run/scripts/fixtures/titles` | todo 35 / CRW-156 | corpus: skill-scripts (domain added by todo 35) | - |
| `scripts/ci/tests/test_plugin.py` | 62 | C | plugin package validator vs shapes that install silently wrong | - | todo 47 / CRW-160 | go-test: `crw-dev ci plugin` (todo 47; `internal/dev/ci` Test47_PLG_1..18) | importlib load of scripts/ci/plugin.py |
| `scripts/ci/tests/test_plugin_wiring.py` | 178 | C | one owner registers the Stop hook; launchers resolve | - | todo 34 / CRW-156 | go-test: `internal/pluginwiring` and `internal/relay/hook` TestPluginLaunch_* for StopLauncher, BridgeLauncher*, LauncherContractVersion (todo 34) + go-test: isolated Codex home integration for those and DeclaredStopCommand (todo 40) + go-test: `internal/runtime/install` for Ownership, BridgeRecord*, RegisterMcpDoesNotShadowADeclaredServer and the legacy Stop bootstrap (todo 38) + go-test: `crw-dev ci plugin` for DeclaredComponent, TheDeclaredApprovalPolicyIsChecked (todo 47) + drop: StableLauncherPlacement (retire-dead: the Go installer places no fallback launcher); StableLauncherRemoval left this file with the transition (todo 39: `install.RemoveLauncher`, see the todo 39 table) | exec of plugins/crw/wiring/crw_bridge_mcp.py (:80) + ast (:2132) |
| `scripts/ci/tests/test_relay_schema_shipped.py` | 1 | A | shipped store objects keep their CREATE text | `scripts/ci/tests/relay_schema_shipped.json` | todo 17 / CRW-152 | corpus: sqlite-ddl | - |
| `scripts/ci/tests/test_release.py` | 10 | A | release workflow steps against fake git/gh | `scripts/ci/tests/{fake_git.sh,fake_gh.sh,release_steps.py}` | todo 12 / CRW-150 | corpus: release (domain added by todo 12) | - |
| `scripts/ci/tests/test_runtime_install.py` | 496 | C | runtime installer and diagnosis on temporary destinations | - | todo 38 / CRW-158 | go-test: `internal/runtime/{reading,record,pointer,staging,swapgate,scope,residue,ownership,definition,doctor}` (todo 37) + go-test: `internal/runtime/install` (todo 38) + drop: the reflective AST and inspect inventories of Python source and the retired trial, preflight, inherited-registration and pre-claim paths (scope analysis "# 38") | ast over scripts/runtime_install.py (406 reflective sites) |
| `scripts/ci/tests/test_scope.py` | 14 | C | CI selection evidence from real git changes incl. renames | - | todo 47 / CRW-160 | go-test: `crw-dev ci scope` (todo 47; `internal/dev/ci` Test47_SCOPE_1..7) | importlib load of scripts/ci/scope.py |
| `scripts/ci/tests/test_stop_events.py` | 75 | A | one accepted record per Stop event through host paths (CRW-212) | `packages/codex-session-relay/tests/fixtures/stop_event_r1.json` | todo 46 / CRW-159 | corpus: hook + go-test: `crw-dev stop-events` (`internal/dev/stopevents`, reader properties SEV-5..SEV-14 over records the real `crw hook` writes; SEV-1..4 are the hook's, todo 33) | - |
| `scripts/ci/tests/test_trial_startup.py` | 374 | C | live-trial preflight states asserted against runs (A/C mix) | - | todo 46 / CRW-159 | go-test: `crw-dev trial-ledger` (`internal/dev/trialledger`, the ledger-mode properties TSU-13, -14, -24, -30, -37, -70, -71 and -90 against goldens captured from the Python ledger, todo 46) + drop: the preflight properties, because the preflight reads the Python install (consoleScript, the v1 host record) and retires with it in todo 44 (scope analysis "# Todo 46") | ast over scripts/trial_startup.py; subprocess runs of the harness |
| `scripts/ci/tests/test_validate.py` | 3 | C | skill structure validator: metadata, links | - | todo 47 / CRW-160 | go-test: `crw-dev ci validate` (todo 47; `internal/dev/ci` Test47_VAL_1..2) | importlib load of scripts/ci/validate.py |

## Todo 15 owned-name parity ledger

Round 3 contract tests (caller-visible bytes recorded from Python by `internal/bridge/testdata/gen_round3.py`; they are not mapped to a single Python test): busy thread code, create and worktree mismatch messages, send approvalRequests, the whole get_capabilities document, worktree receipt fields and a crash at the dispatch checkpoint, argument and destination refusals with inclusive limits, and `--detach` on a base that is also a branch name.

Each Python property was compared with its Go test assertion by assertion (parity round, 2026-09-25): 145 equivalent, 1 deferred to todo 16. Rows marked with a corpus fixture are also run by the Go Git runner under `CRW_CONTRACT_STRICT=1`, against the same JSON checks that Python runs.

| Python name | Go test | Verdict | Basis |
| --- | --- | --- | --- |
| `test_bridge.py::test_create_and_followup_carry_the_stated_pair_and_exact_messages` | `owned_bridge_test.go::Test_test_create_and_followup_carry_the_stated_pair_and_exact_messages` | equivalent | creation.model, exact config {model_reasoning_effort}, no thread/goal/*, exact 4-key resume, wait_thread followup text |
| `test_bridge.py::test_empty_creation_does_not_dispatch_or_set_goal` | `owned_bridge_test.go::Test_test_empty_creation_does_not_dispatch_or_set_goal` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_duplicate_create_and_message_do_not_dispatch_twice` | `owned_bridge_test.go::Test_test_duplicate_create_and_message_do_not_dispatch_twice` | equivalent | 3 concurrent creates share threadId; one thread/start, one turn/start; message replay |
| `test_bridge.py::test_conflicting_request_id_fails_without_mutation` | `owned_bridge_test.go::Test_test_conflicting_request_id_fails_without_mutation` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_replay_after_cwd_removal_returns_retained_receipt` | `owned_bridge_test.go::Test_test_replay_after_cwd_removal_returns_retained_receipt` | equivalent | no host call on replay; changed prompt conflicts |
| `test_bridge.py::test_cwd_symlink_retargeting_does_not_change_request_identity` | `owned_bridge_test.go::Test_test_cwd_symlink_retargeting_does_not_change_request_identity` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_old_canonical_cwd_fingerprint_still_replays_without_directory` | `owned_legacy_test.go::Test_test_old_canonical_cwd_fingerprint_still_replays_without_directory` | equivalent | no host call at all |
| `test_bridge.py::test_legacy_cwd_symlink_replay_uses_old_fingerprint_only_for_legacy_receipts` | `owned_legacy_test.go::Test_test_legacy_cwd_symlink_replay_uses_old_fingerprint_only_for_legacy_receipts` | equivalent | no host call; changed-prompt legacy conflict; new-format alias conflict |
| `test_bridge.py::test_lost_creation_response_is_never_retried` | `owned_legacy_test.go::Test_test_lost_creation_response_is_never_retried` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_partial_failure_retains_created_id` | `owned_legacy_test.go::Test_test_partial_failure_retains_created_id` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_lost_initial_turn_response_retains_id_without_resend` | `owned_observe_test.go::Test_test_lost_initial_turn_response_retains_id_without_resend` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_environment_mismatch_withholds_prompt` | `owned_observe_test.go::Test_test_environment_mismatch_withholds_prompt` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_desktop_project_id_not_found_stops_before_creation` | `owned_observe_test.go::Test_test_desktop_project_id_not_found_stops_before_creation` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_busy_thread_is_not_resumed_or_messaged` | `owned_observe_test.go::Test_test_busy_thread_is_not_resumed_or_messaged` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_interactive_approval_policy_withholds_message` | `owned_observe_test.go::Test_test_interactive_approval_policy_withholds_message` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_reads_and_waits_do_not_resume_or_use_other_completed_turn` | `owned_observe_test.go::Test_test_reads_and_waits_do_not_resume_or_use_other_completed_turn` | equivalent | truncated read text; only read/list methods after read, wait, list |
| `test_bridge.py::test_history_pagination` | `owned_observe_test.go::Test_test_history_pagination` | equivalent | second request carries first nextCursor on the wire |
| `test_bridge.py::test_a_read_never_asks_for_the_full_item_view` | `owned_observe_test.go::Test_test_a_read_never_asks_for_the_full_item_view` | equivalent | exact 3-call sequence; exact 5-key observation; both note fragments |
| `test_bridge.py::test_only_the_newest_turn_has_its_items_read` | `owned_observe_test.go::Test_test_only_the_newest_turn_has_its_items_read` | equivalent | not_requested status/null detail and note |
| `test_bridge.py::test_a_page_too_large_is_narrowed_and_then_dropped_to_ids` | `owned_oversize_test.go::Test_test_a_page_too_large_is_narrowed_and_then_dropped_to_ids` | equivalent | stateful host at 64KiB/100KiB like small_frame_bridge; all three rungs |
| `test_bridge.py::test_an_item_page_that_will_not_arrive_is_asked_again_smaller` | `owned_oversize_test.go::Test_test_an_item_page_that_will_not_arrive_is_asked_again_smaller` | equivalent | stateful 64KiB host, limit>1 oversize |
| `test_bridge.py::test_items_that_will_not_arrive_at_all_are_reported_not_claimed` | `owned_read_test.go::Test_test_items_that_will_not_arrive_at_all_are_reported_not_claimed` | equivalent | oversized frames (not disconnect); 2 attempts; both note fragments; counts |
| `test_bridge.py::test_a_turn_with_more_items_than_one_page_says_so` | `owned_read_test.go::Test_test_a_turn_with_more_items_than_one_page_says_so` | equivalent | stateful host, ItemPage+4 tool items (Python: ITEM_PAGE=3, 7 items); newest page in order, desc, empty summary |
| `test_bridge.py::test_tool_output_inside_item_detail_is_truncated_and_marked` | `owned_read_test.go::Test_test_tool_output_inside_item_detail_is_truncated_and_marked` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_a_host_that_cannot_read_items_still_answers_the_read` | `owned_read_test.go::Test_test_a_host_that_cannot_read_items_still_answers_the_read` | equivalent | -32601 and -32000 with code/message/counts/note |
| `test_bridge.py::test_a_mutation_caught_in_someone_elses_oversized_frame_is_unknown` | `owned_oversize_test.go::Test_test_a_mutation_caught_in_someone_elses_oversized_frame_is_unknown` | equivalent | error starts ResponseTooLarge: (bridge now renders Python type-name prefix) |
| `test_bridge.py::test_a_page_that_never_arrives_is_a_gap_in_the_answer_not_a_failed_read` | `owned_read_test.go::Test_test_a_page_that_never_arrives_is_a_gap_in_the_answer_not_a_failed_read` | equivalent | 1 attempt, TransportError: prefix, unestablished, no frameBytes |
| `test_bridge.py::test_item_detail_that_never_arrives_does_not_fail_a_read_that_did` | `owned_read_test.go::Test_test_item_detail_that_never_arrives_does_not_fail_a_read_that_did` | equivalent | TransportError: prefix, note, summary status, counts |
| `test_bridge.py::test_a_frame_belonging_to_nobody_can_drive_the_ladder_down` | `owned_oversize_test.go::Test_test_a_frame_belonging_to_nobody_can_drive_the_ladder_down` | equivalent | turn ids order, empty items, complete newest detail, three note fragments |
| `test_bridge.py::test_low_text_limit_preserves_page_cursors_and_protocol_fields` | `owned_read_test.go::Test_test_low_text_limit_preserves_page_cursors_and_protocol_fields` | equivalent | stateful host cursors 1:/0:, long cwd and item id, second page turn-1 |
| `test_bridge.py::test_list_preserves_long_cursor` | `owned_retry_test.go::Test_test_list_preserves_long_cursor` | equivalent | long cursor returned and carried on second request |
| `test_bridge.py::test_cancellation_keeps_unknown_receipt_and_prevents_retry` | `owned_cancel_test.go::Test_test_cancellation_keeps_unknown_receipt_and_prevents_retry` | equivalent | cancellation now propagates context.Canceled after the receipt is saved |
| `test_bridge.py::test_cancelling_before_anything_was_sent_leaves_the_id_usable` | `owned_cancel_test.go::Test_test_cancelling_before_anything_was_sent_leaves_the_id_usable` | equivalent | propagated cancel; not_attempted; attempt 2; two turn/start in total |
| `test_bridge.py::test_a_lost_read_before_a_message_leaves_the_request_id_usable` | `owned_retry_test.go::Test_test_a_lost_read_before_a_message_leaves_the_request_id_usable` | equivalent | ledger priorAttempts[0].status not_attempted |
| `test_bridge.py::test_a_lost_project_read_never_created_a_thread` | `owned_retry_test.go::Test_test_a_lost_project_read_never_created_a_thread` | equivalent | attemptedEffects empty; no thread/start |
| `test_bridge.py::test_a_lost_handshake_is_not_an_unknown_creation` | `owned_retry_test.go::Test_test_a_lost_handshake_is_not_an_unknown_creation` | equivalent | attemptedEffects empty; no thread/start |
| `test_bridge.py::test_a_request_that_never_reached_a_socket_can_be_retried` | `owned_retry_test.go::Test_test_a_request_that_never_reached_a_socket_can_be_retried` | equivalent | attemptedEffects empty; same ledger, live client accepts |
| `test_bridge.py::test_a_lost_resume_response_is_unknown_and_never_resent` | `owned_retry_test.go::Test_test_a_lost_resume_response_is_unknown_and_never_resent` | equivalent | effects [thread/resume]; one resume, one turn/start total |
| `test_bridge.py::test_a_lost_message_turn_response_is_unknown_and_never_resent` | `owned_retry_test.go::Test_test_a_lost_message_turn_response_is_unknown_and_never_resent` | equivalent | effects [thread/resume, turn/start]; two turn/start total |
| `test_bridge.py::test_ledger_survives_restarts_and_is_private` | `owned_retry_test.go::Test_test_ledger_survives_restarts_and_is_private` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_goal_read_validates_id_and_bounds_text_without_mutation` | `owned_active_test.go::Test_test_goal_read_validates_id_and_bounds_text_without_mutation` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_invalid_create_has_no_api_effects` | `owned_active_test.go::Test_test_invalid_create_has_no_api_effects` | equivalent | all five inputs; no host method at all |
| `test_bridge.py::test_active_turn_is_derived_from_the_newest_in_progress_turn` | `owned_active_test.go::Test_test_active_turn_is_derived_from_the_newest_in_progress_turn` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_active_turn_reports_idle_and_a_thread_with_no_turns` | `owned_active_test.go::Test_test_active_turn_reports_idle_and_a_thread_with_no_turns` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_active_turn_reports_non_runnable_status_without_raising` | `owned_active_more_test.go::Test_test_active_turn_reports_non_runnable_status_without_raising` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_active_turn_names_a_disagreement_between_status_and_turns` | `owned_active_more_test.go::Test_test_active_turn_names_a_disagreement_between_status_and_turns` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_steer_reaches_the_guarded_turn_and_claims_only_acceptance` | `owned_active_more_test.go::Test_test_steer_reaches_the_guarded_turn_and_claims_only_acceptance` | equivalent | input exactly [{type:text,text}]; no model/config; no resume/turn/start/goal |
| `test_bridge.py::test_steer_refuses_each_non_active_status_by_name` | `owned_active_more_test.go::Test_test_steer_refuses_each_non_active_status_by_name` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_steer_with_a_stale_turn_id_fails_and_does_not_retarget` | `owned_steer_test.go::Test_test_steer_with_a_stale_turn_id_fails_and_does_not_retarget` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_a_turn_id_we_did_not_guard_is_a_failure_not_an_acceptance` | `owned_steer_test.go::Test_test_a_turn_id_we_did_not_guard_is_a_failure_not_an_acceptance` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_an_uncertain_steer_replays_and_is_reconciled_by_record` | `owned_steer_test.go::Test_test_an_uncertain_steer_replays_and_is_reconciled_by_record` | equivalent | wire and ledger clientUserMessageId |
| `test_bridge.py::test_the_same_request_id_cannot_be_aimed_at_another_turn` | `owned_steer_test.go::Test_test_the_same_request_id_cannot_be_aimed_at_another_turn` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_steer_reports_an_unsupported_method_without_claiming_a_host_wide_gap` | `owned_steer_test.go::Test_test_steer_reports_an_unsupported_method_without_claiming_a_host_wide_gap` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_steer_rejects_empty_and_oversized_input_before_any_call` | `owned_steer_test.go::Test_test_steer_rejects_empty_and_oversized_input_before_any_call` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_capabilities_keep_bridge_exposure_and_host_support_apart` | `owned_capabilities_test.go::Test_test_capabilities_keep_bridge_exposure_and_host_support_apart` | equivalent | no turn/* request |
| `test_bridge.py::test_capabilities_reports_only_flags_about_this_bridge` | `owned_capabilities_test.go::Test_test_capabilities_reports_only_flags_about_this_bridge` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_capabilities_stops_answering_what_it_never_asked` | `owned_capabilities_test.go::Test_test_capabilities_stops_answering_what_it_never_asked` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_an_unasked_question_carries_a_sentence_and_never_a_value` | `owned_capabilities_test.go::Test_test_an_unasked_question_carries_a_sentence_and_never_a_value` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_a_consumer_can_tell_measured_from_assumed_using_only_the_response` | `owned_capabilities_test.go::Test_test_a_consumer_can_tell_measured_from_assumed_using_only_the_response` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_capabilities_asks_the_connected_host_nothing` | `owned_capabilities_test.go::Test_test_capabilities_asks_the_connected_host_nothing` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_two_host_facing_answers_disagree_on_one_connection` | `owned_capabilities_test.go::Test_test_two_host_facing_answers_disagree_on_one_connection` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_a_tested_host_never_comes_to_cover_an_unanswered_question` | `owned_capabilities_test.go::Test_test_a_tested_host_never_comes_to_cover_an_unanswered_question` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_pause_sets_status_only_and_never_claims_the_turn_stopped` | `owned_pause_test.go::Test_test_pause_sets_status_only_and_never_claims_the_turn_stopped` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_pause_then_steer_the_observed_turn_to_finish_safely` | `owned_pause_test.go::Test_test_pause_then_steer_the_observed_turn_to_finish_safely` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_pause_refuses_a_thread_with_no_goal` | `owned_pause_test.go::Test_test_pause_refuses_a_thread_with_no_goal` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_an_already_paused_goal_is_not_written_again` | `owned_pause_test.go::Test_test_an_already_paused_goal_is_not_written_again` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_pause_refuses_a_goal_that_is_not_active` | `owned_pause_test.go::Test_test_pause_refuses_a_goal_that_is_not_active` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_a_goal_that_moved_under_the_pause_is_a_known_failure` | `owned_pause_test.go::Test_test_a_goal_that_moved_under_the_pause_is_a_known_failure` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_a_lost_steer_response_is_unknown_and_never_sent_again` | `owned_pause_retry_test.go::Test_test_a_lost_steer_response_is_unknown_and_never_sent_again` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_a_lost_pause_response_is_unknown_and_never_sent_again` | `owned_pause_retry_test.go::Test_test_a_lost_pause_response_is_unknown_and_never_sent_again` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_a_lost_read_before_a_steer_is_not_an_unknown_steer` | `owned_pause_retry_test.go::Test_test_a_lost_read_before_a_steer_is_not_an_unknown_steer` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_a_lost_goal_read_never_paused_anything` | `owned_pause_retry_test.go::Test_test_a_lost_goal_read_never_paused_anything` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_bridge.py::test_a_version_is_identified_by_token_not_by_substring` | `owned_capabilities_test.go::Test_test_a_version_is_identified_by_token_not_by_substring` | equivalent | Desktop real agent, 10.154.0, codex-cli 10.154.0 no tokens, empty |
| `test_bridge.py::test_a_prerelease_build_is_not_the_tested_host` | `owned_capabilities_test.go::Test_test_a_prerelease_build_is_not_the_tested_host` | equivalent | four suffixes kept whole in the token; tested release beside bridge version |
| `test_worktree.py::test_readiness_launch_retains_exact_base_without_carrying_dirty_changes` | `owned_worktree_test.go::Test_test_readiness_launch_retains_exact_base_without_carrying_dirty_changes` + corpus `contract/fixtures/git/test_worktree__test_readiness_launch_retains_exact_base_without_carrying_dirty_changes*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_launch_preserves_trailing_whitespace_in_paths` | `owned_worktree_paths_test.go::Test_test_launch_preserves_trailing_whitespace_in_paths` | equivalent | dirty source like Python fixture (source stays unstaged); checkoutBeforeDispatch; READY turn |
| `test_worktree.py::test_invalid_permission_contract_has_no_filesystem_or_api_effects` | `owned_worktree_test.go::Test_test_invalid_permission_contract_has_no_filesystem_or_api_effects` + corpus `contract/fixtures/git/test_worktree__test_invalid_permission_contract_has_no_filesystem_or_api_effects*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_path_collisions_leave_existing_content_untouched` | `owned_worktree_test.go::Test_test_path_collisions_leave_existing_content_untouched` | equivalent | five collisions; content preserved; no thread |
| `test_worktree.py::test_only_available_full_commit_ids_are_accepted` | `owned_worktree_test.go::Test_test_only_available_full_commit_ids_are_accepted` + corpus `contract/fixtures/git/test_worktree__test_only_available_full_commit_ids_are_accepted*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_lost_responses_replay_after_restart_without_duplicate_artifacts` | `owned_worktree_replay_test.go::Test_test_lost_responses_replay_after_restart_without_duplicate_artifacts` + corpus `contract/fixtures/git/test_worktree__test_lost_responses_replay_after_restart_without_duplicate_artifacts*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_mcp_isolated_launch_and_followup_are_durable` | - | deferred todo 16: "Expose the bridge as an MCP stdio server and wire `crw bridge`"; "QA scenarios: happy = stdio round trip create_thread against fakehost via an MCP client in Go test" | the test drives the MCP stdio server (python -m codex_thread_bridge.server), which todo 16 creates |
| `test_worktree.py::test_git_hooks_filters_and_fsmonitor_are_not_run` | `owned_worktree_git_test.go::Test_test_git_hooks_filters_and_fsmonitor_are_not_run` + corpus `contract/fixtures/git/test_worktree__test_git_hooks_filters_and_fsmonitor_are_not_run*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_interrupted_dispatch_is_retained_and_never_repeated` | `owned_worktree_interrupted_test.go::Test_test_interrupted_dispatch_is_retained_and_never_repeated` | equivalent | 2 stages x cancel/timeout; created state; replay issues no request |
| `test_worktree.py::test_environment_mismatch_retains_actual_receipt_and_withholds_prompt` | `owned_worktree_more_test.go::Test_test_environment_mismatch_retains_actual_receipt_and_withholds_prompt` + corpus `contract/fixtures/git/test_worktree__test_environment_mismatch_retains_actual_receipt_and_withholds_prompt*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_checkout_change_during_thread_start_withholds_prompt` | `owned_worktree_more_test.go::Test_test_checkout_change_during_thread_start_withholds_prompt` | equivalent | failed, threadId retained, detached false, no turn/start |
| `test_worktree.py::test_cancellation_after_git_creation_retains_checkout_without_starting_task` | `owned_git_cancel_test.go::Test_test_cancellation_after_git_creation_retains_checkout_without_starting_task` | equivalent | add/checkout-index; phase; revision; tracked presence; no host method; replay |
| `test_worktree.py::test_known_thread_failure_retains_worktree_and_prevents_retry` | `owned_worktree_failure_test.go::Test_test_known_thread_failure_retains_worktree_and_prevents_retry` + corpus `contract/fixtures/git/test_worktree__test_known_thread_failure_retains_worktree_and_prevents_retry*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_a_lost_project_read_leaves_no_worktree_and_keeps_the_id` | `owned_worktree_failure_test.go::Test_test_a_lost_project_read_leaves_no_worktree_and_keeps_the_id` | equivalent | no recoveryRequired, no destination; attempt 2 accepted |
| `test_worktree.py::test_a_reserved_destination_is_an_attempt_even_with_no_request_sent` | `owned_worktree_failure_test.go::Test_test_a_reserved_destination_is_an_attempt_even_with_no_request_sent` + corpus `contract/fixtures/git/test_worktree__test_a_reserved_destination_is_an_attempt_even_with_no_request_sent*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_a_known_validation_failure_keeps_its_request_id` | `owned_worktree_failure_test.go::Test_test_a_known_validation_failure_keeps_its_request_id` | equivalent | "must be absent", empty effects, replay failed, no thread |
| `test_worktree.py::test_a_prompt_whose_frame_never_went_out_is_not_left_unknown` | `owned_worktree_failure_test.go::Test_test_a_prompt_whose_frame_never_went_out_is_not_left_unknown` + corpus `contract/fixtures/git/test_worktree__test_a_prompt_whose_frame_never_went_out_is_not_left_unknown*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_a_prompt_the_host_refused_is_recorded_as_refused` | `owned_worktree_failure_test.go::Test_test_a_prompt_the_host_refused_is_recorded_as_refused` + corpus `contract/fixtures/git/test_worktree__test_a_prompt_the_host_refused_is_recorded_as_refused*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_replay_survives_removed_checkout_and_source_paths` | `owned_worktree_failure_test.go::Test_test_replay_survives_removed_checkout_and_source_paths` + corpus `contract/fixtures/git/test_worktree__test_replay_survives_removed_checkout_and_source_paths*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_concurrent_requests_cannot_adopt_same_destination` | `owned_worktree_git_test.go::Test_test_concurrent_requests_cannot_adopt_same_destination` + corpus `contract/fixtures/git/test_worktree__test_concurrent_requests_cannot_adopt_same_destination*` (Go runner, strict) | equivalent | fixture + Go test: first paused at thread/start holds the reservation; same id replays, other id fails; an independent bridge contending then fails "must be absent" (fails if Reserve adopts) |
| `test_worktree.py::test_inherited_git_environment_cannot_redirect_checkout` | `owned_worktree_git_test.go::Test_test_inherited_git_environment_cannot_redirect_checkout` + corpus `contract/fixtures/git/test_worktree__test_inherited_git_environment_cannot_redirect_checkout*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_destination_conditional_filters_are_disabled_before_checkout` | `owned_worktree_git_test.go::Test_test_destination_conditional_filters_are_disabled_before_checkout` | equivalent | includeIf smudge filter marker never created |
| `test_worktree.py::test_a_dispatched_worktree_task_is_annotated_like_the_other_paths` | `owned_worktree_more_test.go::Test_test_a_dispatched_worktree_task_is_annotated_like_the_other_paths` + corpus `contract/fixtures/git/test_worktree__test_a_dispatched_worktree_task_is_annotated_like_the_other_paths*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_a_retained_receipt_survives_validation_this_version_added` | `owned_worktree_legacy_test.go::Test_test_a_retained_receipt_survives_validation_this_version_added` + corpus `contract/fixtures/git/test_worktree__test_a_retained_receipt_survives_validation_this_version_added*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_a_worktree_launch_without_a_stated_pair_creates_nothing` | `bridge_test.go::Test_test_a_worktree_launch_without_a_stated_pair_creates_nothing` | equivalent | model and effort each omitted; code/field; no destination; one worktree; no host method |
| `test_worktree.py::test_a_worktree_launch_transmits_the_pair_it_was_authorized_for` | `owned_worktree_more_test.go::Test_test_a_worktree_launch_transmits_the_pair_it_was_authorized_for` + corpus `contract/fixtures/git/test_worktree__test_a_worktree_launch_transmits_the_pair_it_was_authorized_for*` (Go runner, strict) | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_worktree.py::test_a_worktree_exception_covers_only_the_destination_it_names` | `owned_worktree_more_test.go::Test_test_a_worktree_exception_covers_only_the_destination_it_names` | equivalent | out-of-scope with no host method; accepted launch model+effort; exception recorded |
| `test_worktree.py::test_a_worktree_launch_follows_the_authorization_not_the_arguments` | `owned_relabel_test.go::Test_test_a_worktree_launch_follows_the_authorization_not_the_arguments` | equivalent | relabelled pair on wire, requested and policy receipt |
| `test_settings.py::test_first_full_request_can_use_opus_and_xhigh` | `owned_settings_more_test.go::Test_test_first_full_request_can_use_opus_and_xhigh` | equivalent | turnId, prompt text, actual xhigh |
| `test_settings.py::test_a_clean_resume_starts_its_turn_with_no_overrides` | `owned_settings_test.go::Test_test_a_clean_resume_starts_its_turn_with_no_overrides` | equivalent | carried() workspace-write contract; observed_at_resume; turn/start keys exactly threadId,input |
| `test_settings.py::test_a_reused_id_replays_without_dispatching_again` | `owned_settings_test.go::Test_test_a_reused_id_replays_without_dispatching_again` | equivalent | carried() contract; same turnId; no new turn/start |
| `test_settings.py::test_a_reused_id_with_changed_settings_is_rejected` | `owned_settings_test.go::Test_test_a_reused_id_with_changed_settings_is_rejected` | equivalent | carried() with changed model conflicts |
| `test_settings.py::test_an_omitted_setting_keeps_the_pre_upgrade_fingerprint` | `owned_settings_more_test.go::Test_test_an_omitted_setting_keeps_the_pre_upgrade_fingerprint` | equivalent | no host method on replay |
| `test_settings.py::test_a_failing_annotation_cannot_downgrade_an_accepted_turn` | `owned_settings_more_test.go::Test_test_a_failing_annotation_cannot_downgrade_an_accepted_turn` | equivalent | accepted, turnId, no annotation, ledger accepted |
| `test_settings.py::test_a_misspelled_setting_key_is_refused_not_discarded` | `owned_settings_test.go::Test_test_a_misspelled_setting_key_is_refused_not_discarded` | equivalent | both Python inputs; "unknown key"; no host method |
| `test_settings.py::test_a_replay_answers_from_the_ledger_without_touching_the_host` | `owned_settings_test.go::Test_test_a_replay_answers_from_the_ledger_without_touching_the_host` | equivalent | creation xhigh; concurrentChange false; replay no call; annotation equal |
| `test_settings.py::test_a_replay_is_answered_even_when_the_host_has_gone_away` | `owned_settings_test.go::Test_test_a_replay_is_answered_even_when_the_host_has_gone_away` | equivalent | host closed; replay turnId and annotation equal |
| `test_settings.py::test_a_cancelled_annotation_propagates_instead_of_completing` | `owned_settings_more_test.go::Test_test_a_cancelled_annotation_propagates_instead_of_completing` | equivalent | cancel during annotation read returns context.Canceled; ledger stays accepted |
| `test_settings.py::test_transmitting_a_policy_would_have_relaxed_an_interactive_thread` | `owned_delivery_test.go::Test_test_transmitting_a_policy_would_have_relaxed_an_interactive_thread` | equivalent | setter host (applies a sent approvalPolicy); omitted on wire; rpcError.code unsupported_approval_policy (bridge now sets code); no turn |
| `test_settings.py::test_a_declared_delivery_still_leaves_a_setter_host_untouched` | `owned_delivery_test.go::Test_test_a_declared_delivery_still_leaves_a_setter_host_untouched` | equivalent | setter host; omitted on wire; accepted; one turn/start |
| `test_settings.py::test_an_approval_request_during_the_turn_is_left_undecided` | `owned_delivery_test.go::Test_test_an_approval_request_during_the_turn_is_left_undecided` | equivalent | server request raised, no answer; approvals fields |
| `test_settings.py::test_a_refused_send_is_preserved_as_undelivered` | `owned_delivery_test.go::Test_test_a_refused_send_is_preserved_as_undelivered` | equivalent | not_delivered; no turn/start effect or request |
| `test_settings.py::test_recovery_processes_one_refused_message_exactly_once` | `owned_delivery_test.go::Test_test_recovery_processes_one_refused_message_exactly_once` | equivalent | 5 replays, changed declaration conflict, new id once, 5 replays |
| `test_settings.py::test_an_accepted_delivery_is_not_reported_as_completed_work` | `owned_settings_more_test.go::Test_test_an_accepted_delivery_is_not_reported_as_completed_work` | equivalent | turn_started; first sentence lacks "completed"; wait inProgress timedOut |
| `test_settings.py::test_an_explicit_null_declaration_is_refused_rather_than_read_as_never` | `owned_settings_more_test.go::Test_test_an_explicit_null_declaration_is_refused_rather_than_read_as_never` | equivalent | "approval_policy must be one of" (bridge message aligned); no host method |
| `test_execution.py::test_a_creation_without_a_stated_pair_sends_nothing` | `bridge_test.go::Test_test_a_creation_without_a_stated_pair_sends_nothing` | equivalent | 4 omitted/blank cases; code+field; no host method |
| `test_execution.py::test_a_refusal_leaves_the_request_id_usable` | `bridge_test.go::Test_test_a_refusal_leaves_the_request_id_usable` | equivalent | no ledger row; corrected id: one thread/start, one turn/start |
| `test_execution.py::test_a_creation_for_the_wrong_role_never_reaches_the_host` | `owned_execution_test.go::Test_test_a_creation_for_the_wrong_role_never_reaches_the_host` | equivalent | roles_policy() and all 3 Python cases; no host method; no ledger row |
| `test_execution.py::test_a_corrected_role_request_succeeds_under_the_same_id` | `owned_execution_test.go::Test_test_a_corrected_role_request_succeeds_under_the_same_id` | equivalent | roles_policy(); parent pair on wire; role and roleExpectation |
| `test_execution.py::test_naming_no_role_leaves_the_request_identical_to_one_made_before_roles_existed` | `owned_execution_more_test.go::Test_test_naming_no_role_leaves_the_request_identical_to_one_made_before_roles_existed` | equivalent | roles_policy(); replay, one thread/start |
| `test_execution.py::test_an_unapproved_pair_is_refused_before_any_call` | `owned_execution_test.go::Test_test_an_unapproved_pair_is_refused_before_any_call` | equivalent | policy_for(); unapproved model, effort, crossed MODEL/high; no host method |
| `test_execution.py::test_a_cited_exception_that_does_not_exist_or_does_not_match_sends_nothing` | `owned_execution_test.go::Test_test_a_cited_exception_that_does_not_exist_or_does_not_match_sends_nothing` | equivalent | policy_for(); 4 cases incl. elsewhere dir; no host method |
| `test_execution.py::test_an_authorized_pair_is_the_one_transmitted_and_the_one_compared` | `owned_execution_more_test.go::Test_test_an_authorized_pair_is_the_one_transmitted_and_the_one_compared` | equivalent | exception pair on wire; mode, exception, model, effort, requested, verification; one turn |
| `test_execution.py::test_an_approved_pair_records_the_mode_it_was_approved_under` | `owned_execution_more_test.go::Test_test_an_approved_pair_records_the_mode_it_was_approved_under` | equivalent | exact 7-key executionPolicy incl. digest and limits |
| `test_execution.py::test_an_unavailable_model_is_an_error_and_not_a_substitution` | `owned_execution_test.go::Test_test_an_unavailable_model_is_an_error_and_not_a_substitution` | equivalent | "model is not available"; one thread/start; no turn |
| `test_execution.py::test_a_resume_without_a_full_pair_never_reads_or_resumes_the_thread` | `bridge_test.go::Test_test_a_resume_without_a_full_pair_never_reads_or_resumes_the_thread` | equivalent | 5 settings cases; no host method |
| `test_execution.py::test_a_resume_carries_the_authorized_pair_and_is_checked_against_it` | `owned_execution_more_test.go::Test_test_a_resume_carries_the_authorized_pair_and_is_checked_against_it` | equivalent | policy_for(); resume pair; policy model/effort; observed_at_resume; one turn |
| `test_execution.py::test_an_unapproved_pair_is_refused_on_the_resume_path_too` | `owned_execution_more_test.go::Test_test_an_unapproved_pair_is_refused_on_the_resume_path_too` | equivalent | execution_not_allowed; no host method |
| `test_execution.py::test_a_resume_that_disagrees_withholds_the_message` | `owned_execution_resume_test.go::Test_test_a_resume_that_disagrees_withholds_the_message` | equivalent | override and unreported codes; no turn/start |
| `test_execution.py::test_the_launch_and_the_comparison_follow_the_authorization_not_the_arguments` | `owned_relabel_test.go::Test_test_the_launch_and_the_comparison_follow_the_authorization_not_the_arguments` | equivalent | relabelled pair on start (model+effort) and resume; receipts |
| `test_execution.py::test_a_caller_mutating_its_settings_cannot_split_identity_from_dispatch` | `owned_mutation_snapshot_test.go::Test_test_a_caller_mutating_its_settings_cannot_split_identity_from_dispatch` | equivalent | Python schedule: holding send paused, second send at the mutation lock, caller map mutated; authorized/replay/conflict (fails without the entry snapshot) |
| `test_execution.py::test_an_exception_on_the_resume_path_must_state_its_directory` | `owned_execution_resume_test.go::Test_test_an_exception_on_the_resume_path_must_state_its_directory` | equivalent | cwd field and "must also state its cwd"; no host method; with cwd accepted |
| `test_execution.py::test_a_loaded_thread_reports_its_own_pair_and_the_message_is_withheld` | `owned_execution_resume_test.go::Test_test_a_loaded_thread_reports_its_own_pair_and_the_message_is_withheld` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_execution.py::test_an_unloaded_thread_never_lets_an_echo_stand_as_proof_of_preservation` | `owned_execution_resume_test.go::Test_test_an_unloaded_thread_never_lets_an_echo_stand_as_proof_of_preservation` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_execution.py::test_a_resume_for_the_wrong_role_never_reads_or_resumes_the_thread` | `owned_execution_resume_test.go::Test_test_a_resume_for_the_wrong_role_never_reads_or_resumes_the_thread` | equivalent | roles_policy() superseded pair; no host method; no ledger row |
| `test_execution.py::test_a_worktree_launch_for_the_wrong_role_creates_nothing` | `owned_execution_test.go::Test_test_a_worktree_launch_for_the_wrong_role_creates_nothing` | equivalent | Python arguments (workspace-write, 0*40); no destination, no host method, no ledger row |
| `test_execution.py::test_a_named_supervisor_the_host_has_not_loaded_is_not_resumed_at_all` | `owned_execution_resume_test.go::Test_test_a_named_supervisor_the_host_has_not_loaded_is_not_resumed_at_all` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_execution.py::test_an_exception_is_not_evidence_that_a_pair_matches_its_roles_policy` | `owned_execution_roles_test.go::Test_test_an_exception_is_not_evidence_that_a_pair_matches_its_roles_policy` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_execution.py::test_a_send_that_names_no_role_is_not_guarded_here_and_that_boundary_is_deliberate` | `owned_execution_roles_test.go::Test_test_a_send_that_names_no_role_is_not_guarded_here_and_that_boundary_is_deliberate` | equivalent | roles_policy(); unloaded; accepted; echoIndependence |
| `test_execution.py::test_a_verified_role_pair_may_still_be_sent_to_a_thread_the_host_has_not_loaded` | `owned_execution_roles_test.go::Test_test_a_verified_role_pair_may_still_be_sent_to_a_thread_the_host_has_not_loaded` | equivalent | roles_policy() parent pair; unloaded; accepted; one turn |
| `test_execution.py::test_a_host_that_declared_no_roles_keeps_exactly_its_previous_send_behaviour` | `owned_execution_roles_test.go::Test_test_a_host_that_declared_no_roles_keeps_exactly_its_previous_send_behaviour` | equivalent | same inputs, host effects and receipt/refusal fields as Python |
| `test_execution.py::test_naming_no_role_produces_the_same_request_identity_as_before_roles_existed` | `owned_execution_roles_test.go::Test_test_naming_no_role_produces_the_same_request_identity_as_before_roles_existed` | equivalent | rows the bridge wrote: no-role row matches key-free params, role row includes role; role:nil hashes differently |

## Todo 23A partial implementation (e5b10415 base)

- `test_product_routing_decisions.py`: `internal/relay/routing/parity_test.go` replays the original Python scenarios for PRD-1, 3-9, 11-12, 19-20; `routes_test.go` replays PRD-18 and 22, including whole persisted table rows. PRD-2, 17 and 21 have partial library/storage tests only, not completed property claims. All 17 named tests have behavioral mutation proofs in `.omo/evidence/task-23-crw-go-port.txt` (part A). Placement and completion replies additionally compare Python insertion-order JSON bytes.
- `test_product_routing.py`: PR-1 through PR-27 remain unimplemented/unmapped. Ledger-backed routing requires public context-aware ledger APIs and an accessible extension-kind declaration; no substitute ledger or CLI-through-second-Store adapter was written.
- `test_project_completion.py`: PC-1 through PC-4 remain unmapped here. The existing implementation and CLI live in `internal/relay/registry/linkage_cli.go`; they were not duplicated.
- Python-internal PRD-14, PRD-15 and PC-0 retain the skip rationale in the todo-23 property inventory. This partial result does not mark todo 23 complete.

## Todo 23B sync and reception (e5b10415 base)

| Python file | Property IDs | Go tests | Evidence |
| --- | --- | --- | --- |
| `test_sync_outbox.py` | SO-1..14 | `internal/relay/sync`: `Test23_SO_1_OutboxIdentity` through `Test23_SO_14_EnqueueJournal` | live Python replies and every written table, real Linear block bytes; verdict caller and rollback replay |
| `test_child_packets.py` | CP-1..19 | `internal/relay/sync`: `Test23_CP_<n>_PythonScenario` | each original scenario replayed through independent Go packet entry points, complete JSON bytes |
| `test_reception_findings.py` | RF-1..11 | `internal/relay/sync`: `Test23_RF_<n>_PythonScenario` | original scenario sweep including wrong shapes and unpaired surrogate refusal; CP-14/RF-8 share implementation, not scenario wrappers |
| `test_store_reception.py` | SR-1..18 | `internal/relay/sync`: `Test23_SR_<n>_PythonScenario` | built `crw relay packet-check`, complete stdout and ledger bytes; direct receiver ladder callers replayed; SR-15 drives ambiguous selection through CLI |

Implementation lives in `internal/relay/sync` and `internal/relay/reception`. Existing delivery verdict-sync integration, registry linkage/settings, and the single Python JSON dumper in `supervisor` are reused. The `evidence` package named by the task prompt is absent from this base; no parallel dumper was added.

Additional checks: `Test23_CLI_ArgparseBytes` (COLUMNS 80, 120 and unset), `Test23_CLI_RegistryIntegration`, `Test23_SyncCommandsBuiltBinaryWholeBytes` (both executable names; injected clock/token in contract-only binary), `Test23_LedgerPythonGoInteropAndBlockingLock`. Behavioral mutation proofs for all 62 IDs and exact commands/results are recorded under part B in `.omo/evidence/task-23-crw-go-port.txt`. The production host-adapter socket round trip remains todo 28; no sync or packet-check command needs a transport.


## Todo 23A completed coverage (supersedes the partial entry above)

All 51 contract/behavior properties from `test_product_routing.py`,
`test_product_routing_decisions.py`, and `test_project_completion.py` have independent
live-Python replay tests in `internal/relay/routing`. Each test selects its own
original scenarios; `testdata/properties.md` is the local scenario inventory.
PRD-14/15 and PC-0 are the only omitted Python-internal properties (signature/AST
introspection and duplicate constant identity); compile-time imports plus PR-25
cover the Go equivalents. Project completion reuses registry/linkage, not a second
implementation.

| Property | Named Go test(s) |
| --- | --- |
| PR-1 | `Test23_PR_1_PlacementIntegration` |
| PR-2 | `Test23_PR_2_ControlGroups` |
| PR-3 | `Test23_PR_3_LostCreate` |
| PR-4 | `Test23_PR_4_CompletionIntegration` |
| PR-5 | `Test23_PR_5_UnplacedMismatch` |
| PR-6 | `Test23_PR_6_RemediationHistory` |
| PR-7 | `Test23_PR_7_ProjectProposals` |
| PR-8 | `Test23_PR_8_ProjectPreIssue` |
| PR-9 | `Test23_PR_9_ProjectMembers` |
| PR-10 | `Test23_PR_10_ProposalPaging` |
| PR-11 | `Test23_PR_11_ProjectLinkage` |
| PR-12 | `Test23_PR_12_OriginIsolation` |
| PR-13 | `Test23_PR_13_CompletionReplay` |
| PR-14 | `Test23_PR_14_ExistingItems` |
| PR-15 | `Test23_PR_15_SharedCause` |
| PR-16 | `Test23_PR_16_Classification` |
| PR-17 | `Test23_PR_17_Reporting` |
| PR-18 | `Test23_PR_18_StaticKind`; `Test23_PR_18_BuiltProjectKind` |
| PR-19 | `Test23_PR_19_LateOwner` |
| PR-20 | `Test23_PR_20_RegistryChanges` |
| PR-21 | `Test23_PR_21_TestTargets` |
| PR-22 | `Test23_PR_22_TransactionBoundaries` |
| PR-23 | `Test23_PR_23_LateBinding` |
| PR-24 | `Test23_PR_24_OccurrenceReplay` |
| PR-25 | `Test23_PR_25_StaticRegistry` |
| PR-26 | `Test23_PR_26_FilingRollback` |
| PR-27 | `Test23_PR_27_CurrentRun` |
| PRD-1 | `Test23_PRD_1_Validation` |
| PRD-2 | `Test23_PRD_2_PolicyCLI`; `Test23_PRD_2_PolicyLibrary` |
| PRD-3 | `Test23_PRD_3_FollowUpChecks` |
| PRD-4 | `Test23_PRD_4_Coverage` |
| PRD-5 | `Test23_PRD_5_ProductResolution` |
| PRD-6 | `Test23_PRD_6_Workspace` |
| PRD-7 | `Test23_PRD_7_Placement` |
| PRD-8 | `Test23_PRD_8_SimulatedIsolation` |
| PRD-9 | `Test23_PRD_9_LabelsAndObligations` |
| PRD-10 | `Test23_PRD_10_RegistryCLI`; `Test23_PRD_10_RegistryStore` |
| PRD-11 | `Test23_PRD_11_CompletionChecks` |
| PRD-12 | `Test23_PRD_12_CompletionClosure` |
| PRD-13 | `Test23_PRD_13_LedgerGateCLI`; `Test23_PRD_13_LedgerGate` |
| PRD-16 | `Test23_PRD_16_UnreadableJSON` |
| PRD-17 | `Test23_PRD_17_SurfaceClassification`; `Test23_PRD_17_ClassificationLibrary` |
| PRD-18 | `Test23_PRD_18_RouteRows` |
| PRD-19 | `Test23_PRD_19_Attention` |
| PRD-20 | `Test23_PRD_20_ProjectEligibility` |
| PRD-21 | `Test23_PRD_21_ReplayStorage` |
| PRD-22 | `Test23_PRD_22_ProposalRotation` |
| PC-1 | `Test23_PC_1_NoAnswer` |
| PC-2 | `Test23_PC_2_EveryChild` |
| PC-3 | `Test23_PC_3_ReadFailures` |
| PC-4 | `Test23_PC_4_ReachableConsumer` |

Evidence: `.omo/evidence/task-23-crw-go-port.txt`, part A, records all mappings,
51 killed behavioral mutations (file/line, before/after, observed diff), and
byte-copy plus `cmp` restores. The final PR-22 proof moves the actual binding
read outside Compose; PR-26 commits a failed filing instead of rolling it back.
Those supersede the earlier observer-only mutation examples.

Receipts/refusals compare whole live-Python values and routing CLI replies compare
insertion-order JSON bytes. Store replays compare every table except store identity
metadata (`schema_meta`), retaining serialized JSON text and numeric spelling.
`Test23_ArgparseWidthsBuiltBinary` covers accepted and rejected arguments for all
11 commands, both executable names, at COLUMNS=80, 120 and unset, including bare
`--`, `-- x`, abbreviations, dash-leading values and boolean `=value` errors.
`Test23_BuiltCommandRoundTrips` and `Test23_PR_18_BuiltProjectKind` exercise real
SQLite state through the built dispatcher, including refusals, duplicate intake,
classification replay, and queue/claim/operation/complete project publication.
Clocks/tokens are injected through a test-only build overlay, never normalized.
The shared `argparse` implementation/spec originated with todo 24; routing registers
through `cli.Commands`, without an extra pre-dispatch path.

Validation: full relay/contract race suite, lint, static binary build, routing-only
strict parser contract and applicable strict cli-shape/linkage cases. Broad strict
cli-shape/hook failures are other todos' unported host/daemon/reporting/guard/hook
surfaces; exact fixture names and owners are listed in the evidence. No existing
cli-shape/hook fixture names a routing command. Isolation traces found zero successful
accesses under the live host state, Codex, or runtime roots. Production `--socket`
round trip remains todo 28; the 11 routing commands are offline.
