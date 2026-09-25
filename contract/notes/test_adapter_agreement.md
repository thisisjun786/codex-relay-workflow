# test_adapter_agreement.py - contract conversion notes (lane L3)

Source: `scripts/ci/tests/test_adapter_agreement.py`, 53 `def test_` functions (class A in docs/port/test-map.md).
Totals after round 2 L3: **17 converted fully, 1 converted partially (2 of 3 command configurations), 11 kept under the README mutation-proof exception, 24 blocked** by the exact missing runner kinds below. The 17 includes #53, converted by the round 2 `status` document kind.

## How the conversion preserves the agreement property

The source compares the checkout copy (`scripts/crw_runtime/completion.py`) and the packaged copy (`codex_session_relay/stopadapter.py`) over one invocation, excluding only `configuration`, `journalledAs`, `at`, `elapsedMs`, `guardElapsedMs` and `identityScanMs`, then asserts one field.

* The `agreement` run kind already compares both copies inside the runner and excludes the same six fields. Cases whose stub answers with stdout and exit 0 use it: one fixture per case or per parameter.
* That kind's stub cannot set an exit code, and it cannot leave the runtime absent. Those cases use a pair of fixtures, `__checkout` (run kind `hook`, the real `completion.run`) and `__packaged` (run kind `stop`, the real `stopadapter.py` process). Both fixtures pin the **same** expected values captured from the real programs. Each one checks: the exact stdout, the row count, the row's full key set (`set_eq`, so a field added to one copy only fails), and `eq` on every non-volatile field. `adapterOutcome` equals the value named by the original constant. Because the two fixtures carry byte-identical checks, the copies can only both pass if they agree on every compared field. That is the original property, and it is stronger per field than the original single-field assertion. For the packaged copy, `observe: ["journal"]` also pins exactly one day directory matching `^[0-9]{8}$`. The `hook` runner only reads rows whose names match `[0-9]{8}/[0-9a-f]{32}.json`.
* Packaged fixtures for cases the source ran without `owner` set `owner`, `adapterInterpreter` and `adapterEntryPoint` to null, which undoes the `stop` runner's plugin default. The plugin case keeps the plugin owner in both copies. The checkout copy gets absolute adapter paths, as the source's `settings_document(owner=plugin)` did.
* The unreachable case names the fixed absent path `/nonexistent/crw-contract-absent/relay`, so `detail` is pinned exactly rather than by a regex.

## Cases

| # | Test function | Status | Fixture(s) / reason |
|---|---|---|---|
| 1 | AgreementTests::test_a_held_verdict_is_the_same_hold_in_both | converted | records/test_adapter_agreement__test_a_held_verdict_is_the_same_hold_in_both.json |
| 2 | AgreementTests::test_a_released_verdict_prints_nothing_in_both | converted (pre-existing spike) | records/test_adapter_agreement__test_a_released_verdict_prints_nothing_in_both.json |
| 3 | AgreementTests::test_a_verdict_that_disagrees_with_itself_is_incomplete_in_both | converted | records/test_adapter_agreement__test_a_verdict_that_disagrees_with_itself_is_incomplete_in_both.json |
| 4 | AgreementTests::test_an_error_record_at_each_exit_code_is_its_own_outcome_in_both | converted | records/test_adapter_agreement__test_an_error_record_at_each_exit_code_is_its_own_outcome_in_both__exit_{2,3,4,7}__{checkout,packaged}.json (8) |
| 5 | AgreementTests::test_silence_at_two_and_at_zero_are_different_outcomes_in_both | converted | records/test_adapter_agreement__test_silence_at_two_and_at_zero_are_different_outcomes_in_both__exit_{2,0,9}__{checkout,packaged}.json (6) |
| 6 | AgreementTests::test_unreadable_output_is_not_a_verdict_in_both | converted | records/test_adapter_agreement__test_unreadable_output_is_not_a_verdict_in_both__{garbage,not_an_object}.json |
| 7 | AgreementTests::test_a_runtime_that_cannot_be_run_is_unreachable_in_both | converted | records/test_adapter_agreement__test_a_runtime_that_cannot_be_run_is_unreachable_in_both__{checkout,packaged}.json |
| 8 | AgreementTests::test_a_signalled_runtime_is_signalled_in_both | converted | records/test_adapter_agreement__test_a_signalled_runtime_is_signalled_in_both.json (`agreement` stub signals itself) |
| 9 | AgreementTests::test_a_runtime_past_its_budget_times_out_in_both | converted | records/test_adapter_agreement__test_a_runtime_past_its_budget_times_out_in_both__{checkout,packaged}.json (1 s setting, 30 s stub) |
| 10 | AgreementTests::test_absent_settings_are_absent_in_both | blocked G2 | observable is the settings reader's outcome (`config_absent`), which leaves no row |
| 11 | AgreementTests::test_malformed_settings_are_malformed_in_both | blocked G2 | reader outcome `config_malformed` (probe: 0 rows in both copies) |
| 12 | AgreementTests::test_settings_that_are_a_directory_are_unreadable_in_both | blocked G2, G4 | reader outcome; settings path must be a directory |
| 13 | AgreementTests::test_settings_that_are_not_utf8_are_unreadable_in_both | blocked G2, G4 | reader outcome; settings file must be non-UTF-8 bytes |
| 14 | AgreementTests::test_every_payload_failure_is_the_same_failure_in_both | converted | prior four fixtures plus records/test_adapter_agreement__test_every_payload_failure_is_the_same_failure_in_both__absent.json (`agreement` passes None) |
| 15 | AgreementTests::test_a_faults_only_policy_still_records_an_answer_given_without_an_identity_in_both | converted | records/test_adapter_agreement__test_a_faults_only_policy_still_records_an_answer_given_without_an_identity_in_both__{checkout,packaged}.json (`acceptance` = `unestablished` pinned) |
| 16 | AgreementTests::test_a_no_journal_policy_records_nothing_at_all_in_both | converted | records/test_adapter_agreement__test_a_no_journal_policy_records_nothing_at_all_in_both__{checkout,packaged}.json (`length rows == 0`) |
| 17 | AgreementTests::test_a_plugin_owned_document_is_the_same_answer_in_both | converted | records/test_adapter_agreement__test_a_plugin_owned_document_is_the_same_answer_in_both__{checkout,packaged}.json |
| 18 | AgreementTests::test_a_plugin_owned_document_missing_its_adapter_fields_is_malformed_in_both | blocked G2 | reader outcome (probe: 0 rows) |
| 19 | AgreementTests::test_a_plugin_owned_budget_at_the_launcher_ceiling_is_malformed_in_both | blocked G2 | reader outcome (probe: 0 rows) |
| 20 | AgreementTests::test_a_settings_path_that_is_a_device_is_unreadable_in_both | blocked G2, G4 | settings path `/dev/null`; reader outcome |
| 21 | AgreementTests::test_a_settings_path_that_is_a_named_pipe_is_refused_without_opening_it_in_both | blocked G2, G4 | settings path must be a FIFO; bounded non-blocking run; reader outcome. Not a mid-operation interleave, so it is not a README exception |
| 22 | AgreementTests::test_a_record_is_written_only_to_the_owner_and_only_readable_by_it | converted | records/test_adapter_agreement__test_a_record_is_written_only_to_the_owner_and_only_readable_by_it.json (`row_files` checks count, mode 0600 and name/day shapes) |
| 23-28 | MutationNoticedTests (6: changed_exit_code_mapping, hold_turned_into_a_release, journal_that_stops_being_written, field_dropped_from_the_record, dropped_plugin_owner_requirement, reader_that_stops_refusing_a_non_regular_file) | kept (README exception) | each replaces an attribute of a loaded packaged module and proves comparison notices it; portable data cannot mutate a module |
| 29-41 | EventAcceptanceAgrees (13: stop_another_registration_of_the_host_accepted, replayed_event_accepted_once, three_stops_of_one_turn, late_retry_of_an_earlier_stop, stop_that_repeats_an_earlier_stops_text, every_reason_an_identity_is_unestablished, scan_out_of_time, settings_without_a_journal_root, ledger_that_cannot_be_created, host_file_that_cannot_be_made, no_journal_still_claims, faults_only_leaves_the_duplicate_out, owner_that_faults_after_claiming) | blocked G7/G10 (all); also G8 for scan_out_of_time, every_reason (scan_bound_exceeded subcase) and owner_that_faults_after_claiming | need multi-step Stop sequences in one home, a transcript file per step whose absolute path is in stdin (`${HOME}` in stdin), the stub's guard-call count, and `accepted/` plus host `crw-completion-hook/stop-events/` ledger observation; some need a journal root that is absent, or a file where a directory is expected |
| 42-46 | EventMutationNoticed (5: key_built_from_session_and_turn, identity_minted_per_invocation, copy_that_stops_claiming, arbitrates_only_within_its_journal_root, asks_the_guard_about_a_duplicate) | kept (README exception) | module-attribute mutants prove the agreement comparison notices drift |
| 47 | FaultRecoveryKeepsTheRow::test_an_outcome_that_raises_after_the_row_leaves_the_row_in_both | blocked G8 (+G7) | `record_outcome` must raise in both copies |
| 48 | TheGuardCommandAgrees::test_both_copies_build_the_same_command | partial | records/test_adapter_agreement__test_both_copies_build_the_same_command__{bare,served}__{checkout,packaged}.json pins ordered argv against both real processes; `EVERYTHING` stays Python because `dbPath` makes the runtime refuse before invoking the stub. Missing kind: direct `guard_argv` observation for both copies. |
| 49 | TheGuardCommandAgrees::test_a_configured_socket_is_passed_as_a_global_option_in_both | converted | reuses the two `served` command fixtures (socket before guard-evaluate, exact ordered argv) |
| 50 | TheGuardCommandAgrees::test_no_configured_socket_passes_none_in_both | converted | reuses the two `bare` command fixtures (no socket, exact ordered argv) |
| 51 | TheGuardCommandAgrees::test_a_socket_that_is_not_an_absolute_path_is_malformed_in_both | blocked G2 | `complaints()` result; malformed settings leave no row |
| 52 | TheGuardCommandAgrees::test_a_socket_that_is_not_a_string_is_malformed_in_both | blocked G2 | same |
| 53 | TheGuardCommandAgrees::test_a_document_written_without_a_socket_carries_no_such_key | converted | records/test_adapter_agreement__test_a_document_written_without_a_socket_carries_no_such_key.json (`status` document kind) |

Rows 23-28, 29-41 and 42-46 each group several functions; together the table covers all 53.

## runner_gaps

* **G1 resolved for #8, #9, #49 and #50**: `agreement` supports self-signal, and `stop` supports delay and records `calls[].argv`; #48 still needs a direct `guard_argv` kind for the `EVERYTHING` configuration, because a real guard is never launched when `dbPath` refuses the invocation.
* **G2 - settings-reader outcome observation**: an observable exposing each copy's settings decision (`read_configuration` / `read_settings` outcome, whether a document was produced, and `complaints()`), or an `agreement`-style kind that compares them. Blocks #10-13, #18-21, #51, #52; see G11 for the precise requirement.
* **G3 resolved**: `agreement` accepts `stdin: null` and hands both adapters `None` (#14).
* **G4 - non-regular settings path**: settings as a directory, raw non-UTF-8 bytes, a FIFO (run with a bound), or an explicit path such as `/dev/null`. Blocks #12, #13, #20, #21.
* **G5 resolved**: `stop.row_files` exposes pattern-selected row modes (#22).
* **G6 - mutation proofs**: kept Python by the README exception (#23-28 and #42-46); no executable code belongs in a fixture.
* **G7 - complete multi-step agreement**: `stop.steps`, `${HOME}` in stdin, guard-call counts and accepted ledgers now exist for a single copy, but #29-41 and #47 need G10's two-copy comparison. #29 additionally needs declarative host marker seeding at a computed event key; #31, #32, #34 need transcript variants beyond a fixed r1 prefix; #36 needs a declarative absent `journalRoot`; #37-38 need a file occupying the ledger directory before the first step.
* **G8 - fault/limit injection**: scan byte/time bounds, `invoke_guard` raising, `record_outcome` raising. Blocks the scan_out_of_time and owner_faults cases, the `scan_bound_exceeded` subcase of #34, and #47.
* **G9 resolved**: `status` with `document: true` exposes the builder (#53).
* **G10 - two-copy event agreement**: `stop.steps` now executes a sequence, but has no kind that runs *both* adapters on separate homes and compares their returned text, every nonvolatile row/accepted-ledger field, call counts and per-ledger row relations. Blocks #29-41 and #47 in addition to their specific G7/G8 needs; converting only one copy or pinning only acceptance/count would weaken the source test. `given.transcript_r1` also only supplies an unmodified prefix per stop, so the altered third answer and identity-reason transcript variants require a declarative transcript edit/raw-tail kind.
* **G11 - guarded settings decisions**: `stop` process silence cannot prove `read_configuration` and `read_settings` returned the same outcome, or compare `complaints()` fields. Needed for #10-13, #18-21, #51-52 (alongside G4 for nonregular inputs).
