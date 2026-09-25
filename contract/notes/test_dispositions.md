# test_dispositions.py conversion notes (todo 6, lane L4)

Source: `packages/codex-session-relay/tests/test_dispositions.py`, 61 class-A test functions.
Result: 60 converted, 1 kept (README exception rule), 0 blocked. The four formerly blocked
cases use the round 2 `cli` observed-files and relay-host kinds.
All fixtures are in `contract/fixtures/cli-shape/` and have the prefix `test_dispositions__`. Each
one drives the real relay CLI (`python -m codex_session_relay.cli --state <tmp>/state`) against a
real SQLite store. Expected values were taken from running that program.

## How the pure-derivation cases were converted

The original `TheTurnDisposition`, `TheWorkReport`, `TheDeliveryAxis`, `TheReceivingAxis`,
`TheCounts` and `TheCorrectionBlock` tests fed synthetic flat rows to `dispositions.derive`. The
fixtures instead seed those rows into the relay's real tables (`given.sql_seed`: relationships,
relationship_scope, events, deliveries, delivery_intent, delivery_supersession, acks,
ack_evidence, work_reports, failed_operations) and read them back through `dispositions-show`. The
answer then comes from the real single-statement SQL as well as from `derive`. Row values that the
old tests supplied as columns are now computed from real rows: `reviewable_count` from real
ready_for_review events, `earlier_events` from real earlier-generation events, and `ordering_at`
from `finalized_at`/`first_seen_at`. Two cases could not copy their synthetic rows exactly:

- `test_a_directly_final_event_with_no_finalized_at_still_orders`: `events.first_seen_at` is NOT
  NULL, so the event that had a null ordering key gets `first_seen_at = ''`. The reader maps both
  None and '' to the same sort key (`orderingAt or ""`). The two asserted values (the winner and the
  candidate list) are unchanged.
- `test_a_superseded_assignment_is_not_called_merely_inactive`: the real SQL filters a superseded
  relationship out of the project selector, so this case reads with `--relationship rel-1`. That
  selector answers whatever the status. The asserted `undeliveredReason: null` is unchanged.

The work_reports row seeded for the real-store cases is the exact row that `report.record` wrote in
a real call (contract_version `0.2.28+codex.20260914090142`, evidence `[]`, restore `{}`). No CLI
subcommand records a work report, so there is no process-level path to write it.

In `TheVocabulary`, three tests compared Python module constants against each other: the tuple
equality of `EXECUTION_ONLY` and `EXECUTION_ONLY_OUTCOMES`, `_SQL` text membership, and the
enumeration of `transport`'s names. Those comparisons are not portable, so each test now runs a
fixture that observes the same property through the CLI and keeps its original in-process
assertion. The fourth vocabulary test does the same with the `OBSERVATION_DETAIL` membership check.
Nothing was dropped.

## Case table

| Test | Status | Fixture / reason |
| --- | --- | --- |
| TheTurnDisposition::test_a_child_that_reported_nothing_is_not_a_child_that_is_fine | converted | cli-shape/test_dispositions__test_a_child_that_reported_nothing_is_not_a_child_that_is_fine.json |
| TheTurnDisposition::test_a_single_blocked_event_is_the_disposition | converted | cli-shape/test_dispositions__test_a_single_blocked_event_is_the_disposition.json |
| TheTurnDisposition::test_a_reviewable_event_is_counted_and_never_becomes_the_disposition | converted | cli-shape/test_dispositions__test_a_reviewable_event_is_counted_and_never_becomes_the_disposition.json |
| TheTurnDisposition::test_several_reviewable_events_are_all_listed_and_none_is_called_the_head | converted | cli-shape/test_dispositions__test_several_reviewable_events_are_all_listed_and_none_is_called_the_head.json |
| TheTurnDisposition::test_two_events_with_the_same_outcome_anchor_to_the_newest_and_keep_both | converted | cli-shape/test_dispositions__test_two_events_with_the_same_outcome_anchor_to_the_newest_and_keep_both.json |
| TheTurnDisposition::test_disagreeing_outcomes_are_a_contest_even_when_one_is_newer | converted | cli-shape/test_dispositions__test_disagreeing_outcomes_are_a_contest_even_when_one_is_newer__equal.json, cli-shape/test_dispositions__test_disagreeing_outcomes_are_a_contest_even_when_one_is_newer__newer.json |
| TheTurnDisposition::test_a_suppressed_claim_is_listed_and_never_chosen | converted | cli-shape/test_dispositions__test_a_suppressed_claim_is_listed_and_never_chosen.json |
| TheTurnDisposition::test_a_staged_claim_is_listed_and_never_chosen | converted | cli-shape/test_dispositions__test_a_staged_claim_is_listed_and_never_chosen.json |
| TheTurnDisposition::test_a_directly_final_event_with_no_finalized_at_still_orders | converted | cli-shape/test_dispositions__test_a_directly_final_event_with_no_finalized_at_still_orders.json |
| TheTurnDisposition::test_earlier_generations_are_counted_and_not_listed | converted | cli-shape/test_dispositions__test_earlier_generations_are_counted_and_not_listed.json |
| TheWorkReport::test_a_recorded_report_carries_the_status_that_separates_them | converted | cli-shape/test_dispositions__test_a_recorded_report_carries_the_status_that_separates_them.json |
| TheWorkReport::test_no_report_says_the_separation_is_unavailable_rather_than_guessing | converted | cli-shape/test_dispositions__test_no_report_says_the_separation_is_unavailable_rather_than_guessing.json |
| TheDeliveryAxis::test_no_delivery_and_no_intent_is_unmeasured_not_undelivered | converted | cli-shape/test_dispositions__test_no_delivery_and_no_intent_is_unmeasured_not_undelivered.json |
| TheDeliveryAxis::test_an_intent_without_a_delivery_is_a_refusal_that_may_not_last | converted | cli-shape/test_dispositions__test_an_intent_without_a_delivery_is_a_refusal_that_may_not_last.json |
| TheDeliveryAxis::test_a_queued_delivery_is_not_sent_and_carries_the_stores_own_word | converted | cli-shape/test_dispositions__test_a_queued_delivery_is_not_sent_and_carries_the_stores_own_word.json |
| TheDeliveryAxis::test_a_dispatched_delivery_says_dispatched_and_nothing_more | converted | cli-shape/test_dispositions__test_a_dispatched_delivery_says_dispatched_and_nothing_more.json |
| TheDeliveryAxis::test_an_inbox_only_delivery_is_stored_not_woken | converted | cli-shape/test_dispositions__test_an_inbox_only_delivery_is_stored_not_woken.json |
| TheDeliveryAxis::test_an_uncertain_send_is_not_evidence_of_non_delivery | converted | cli-shape/test_dispositions__test_an_uncertain_send_is_not_evidence_of_non_delivery__sending.json, cli-shape/test_dispositions__test_an_uncertain_send_is_not_evidence_of_non_delivery__held_uncertain.json |
| TheDeliveryAxis::test_a_supersession_note_outranks_the_state_but_never_erases_it | converted | cli-shape/test_dispositions__test_a_supersession_note_outranks_the_state_but_never_erases_it.json |
| TheDeliveryAxis::test_a_state_this_reader_has_no_word_for_is_not_folded_into_one | converted | cli-shape/test_dispositions__test_a_state_this_reader_has_no_word_for_is_not_folded_into_one.json |
| TheDeliveryAxis::test_a_staged_event_is_never_reported_as_delivered | converted | cli-shape/test_dispositions__test_a_staged_event_is_never_reported_as_delivered.json |
| TheDeliveryAxis::test_a_suppressed_event_is_called_suppressed_rather_than_staged | converted | cli-shape/test_dispositions__test_a_suppressed_event_is_called_suppressed_rather_than_staged.json |
| TheDeliveryAxis::test_an_acknowledgement_with_no_delivery_row_is_a_disagreement_not_unmeasured | converted | cli-shape/test_dispositions__test_an_acknowledgement_with_no_delivery_row_is_a_disagreement_not_unmeasured.json |
| TheReceivingAxis::test_a_dispatched_send_still_leaves_the_receiving_side_unmeasured | converted | cli-shape/test_dispositions__test_a_dispatched_send_still_leaves_the_receiving_side_unmeasured.json |
| TheReceivingAxis::test_a_host_read_acknowledgement_is_an_observation | converted | cli-shape/test_dispositions__test_a_host_read_acknowledgement_is_an_observation.json |
| TheReceivingAxis::test_an_unverified_acknowledgement_is_a_claim_rather_than_an_observation | converted | cli-shape/test_dispositions__test_an_unverified_acknowledgement_is_a_claim_rather_than_an_observation.json |
| TheReceivingAxis::test_evidence_with_no_acknowledgement_beside_it_has_no_supportable_answer | converted | cli-shape/test_dispositions__test_evidence_with_no_acknowledgement_beside_it_has_no_supportable_answer.json |
| TheReceivingAxis::test_the_acknowledgement_block_keeps_every_field_the_assignment_view_reports | converted | cli-shape/test_dispositions__test_the_acknowledgement_block_keeps_every_field_the_assignment_view_reports.json |
| TheReceivingAxis::test_no_acknowledgement_row_is_unrecorded_rather_than_unverified | converted | cli-shape/test_dispositions__test_no_acknowledgement_row_is_unrecorded_rather_than_unverified.json |
| TheVocabulary::test_the_execution_only_outcomes_are_still_the_stores_own_tuple | converted (+ retained in-process constant check) | cli-shape/test_dispositions__test_the_execution_only_outcomes_are_still_the_stores_own_tuple.json |
| TheVocabulary::test_the_statement_selects_exactly_the_stores_four_outcomes | converted (+ retained in-process _SQL check) | cli-shape/test_dispositions__test_the_statement_selects_exactly_the_stores_four_outcomes.json |
| TheVocabulary::test_every_delivery_state_the_transport_defines_has_a_word | converted (+ retained transport enumeration) | cli-shape/test_dispositions__test_every_delivery_state_the_transport_defines_has_a_word.json |
| TheVocabulary::test_every_word_the_map_produces_has_a_detail_entry | converted (+ retained in-process map check) | cli-shape/test_dispositions__test_every_word_the_map_produces_has_a_detail_entry.json |
| TheCounts::test_the_counts_cannot_disagree_with_the_list_they_summarise | converted | cli-shape/test_dispositions__test_the_counts_cannot_disagree_with_the_list_they_summarise.json |
| TheReadItself::test_a_directory_with_no_database_is_unreadable_rather_than_empty | converted | cli-shape/test_dispositions__test_a_directory_with_no_database_is_unreadable_rather_than_empty.json |
| TheReadItself::test_a_file_that_is_not_a_relay_database_is_unreadable_and_stays_untouched | converted | cli-shape/test_dispositions__test_a_file_that_is_not_a_relay_database_is_unreadable_and_stays_untouched.json |
| TheReadItself::test_a_replaced_file_is_unreadable_rather_than_attributed_to_the_wrong_store | kept | README exception rule: replaces `store.read_only_rows` to stand in for a database renamed between its identity checks and its read, a mid-operation interleave that no fixture can schedule (the docstring says so) |
| AgainstARealStore::test_a_blocked_child_is_enumerable_by_project | converted | cli-shape/test_dispositions__test_a_blocked_child_is_enumerable_by_project.json |
| AgainstARealStore::test_the_three_statuses_that_share_one_outcome_stay_separable | converted | cli-shape/test_dispositions__test_the_three_statuses_that_share_one_outcome_stay_separable.json |
| AgainstARealStore::test_a_child_with_no_work_report_says_the_separation_is_unavailable | converted | cli-shape/test_dispositions__test_a_child_with_no_work_report_says_the_separation_is_unavailable.json |
| AgainstARealStore::test_a_queued_then_dispatched_delivery_is_measured_at_each_step | converted | cli-shape/test_dispositions__test_a_queued_then_dispatched_delivery_is_measured_at_each_step.json |
| AgainstARealStore::test_a_staged_claim_is_recorded_progress_and_never_delivery | converted | cli-shape/test_dispositions__test_a_staged_claim_is_recorded_progress_and_never_delivery.json |
| AgainstARealStore::test_the_project_selector_hides_what_is_not_live_and_the_relationship_selector_does_not | converted | cli-shape/test_dispositions__test_the_project_selector_hides_what_is_not_live_and_the_relationship_selector_does_not.json (archived through `relationship-status`, not a raw UPDATE) |
| AgainstARealStore::test_an_unscoped_assignment_is_absent_by_project_and_present_by_relationship | converted | cli-shape/test_dispositions__test_an_unscoped_assignment_is_absent_by_project_and_present_by_relationship.json |
| AgainstARealStore::test_an_event_in_an_earlier_generation_is_counted_not_listed | converted | cli-shape/test_dispositions__test_an_event_in_an_earlier_generation_is_counted_not_listed.json (generation 2 opened through `generation-open`, not a raw UPDATE) |
| TheCommand::test_the_command_reads_a_blocked_child_through_a_subprocess | converted | cli-shape/test_dispositions__test_the_command_reads_a_blocked_child_through_a_subprocess.json |
| TheCommand::test_an_unreadable_store_refuses_rather_than_reporting_nobody | converted | cli-shape/test_dispositions__test_an_unreadable_store_refuses_rather_than_reporting_nobody.json |
| TheCommand::test_the_command_turns_no_unrelated_file_into_a_relay_database | converted | cli-shape/test_dispositions__test_the_command_turns_no_unrelated_file_into_a_relay_database.json |
| TheCommand::test_the_two_selectors_are_mutually_exclusive_and_one_is_required | converted (before this lane) | cli-shape/test_dispositions__test_the_two_selectors_are_mutually_exclusive_and_one_is_required.json |
| TheCommand::test_the_command_is_listed_as_offline_because_it_opens_no_adapter | converted | cli-shape/test_dispositions__test_the_command_is_listed_as_offline_because_it_opens_no_adapter.json (`doctor` reports `offlineCommands`, which is `list(OFFLINE_COMMANDS)`) |
| TheCorrectionBlock::test_a_child_without_a_correction_has_none_and_counts_nothing | converted | cli-shape/test_dispositions__test_a_child_without_a_correction_has_none_and_counts_nothing.json |
| TheCorrectionBlock::test_a_correction_withheld_by_the_lifecycle_is_named | converted | cli-shape/test_dispositions__test_a_correction_withheld_by_the_lifecycle_is_named.json |
| TheCorrectionBlock::test_a_withheld_correction_without_a_lifecycle_record_is_still_counted | converted | cli-shape/test_dispositions__test_a_withheld_correction_without_a_lifecycle_record_is_still_counted.json |
| TheCorrectionBlock::test_a_busy_deferral_is_not_sent_but_not_withheld | converted | cli-shape/test_dispositions__test_a_busy_deferral_is_not_sent_but_not_withheld.json |
| TheCorrectionBlock::test_a_paused_assignment_names_the_relationship | converted | cli-shape/test_dispositions__test_a_paused_assignment_names_the_relationship.json |
| TheCorrectionBlock::test_a_superseded_assignment_is_not_called_merely_inactive | converted | cli-shape/test_dispositions__test_a_superseded_assignment_is_not_called_merely_inactive.json |
| TheCorrectionBlock::test_a_held_correction_names_its_hold | converted | cli-shape/test_dispositions__test_a_held_correction_names_its_hold.json |
| TheCorrectionBlock::test_a_dispatched_correction_has_no_reason | converted | cli-shape/test_dispositions__test_a_dispatched_correction_has_no_reason.json |
| TheCorrectionBlock::test_a_state_this_reader_has_no_word_for_is_said_so | converted | cli-shape/test_dispositions__test_a_state_this_reader_has_no_word_for_is_said_so.json |
| TheCorrectionBlock::test_a_correction_the_child_already_answered_is_superseded_and_not_counted | converted | cli-shape/test_dispositions__test_a_correction_the_child_already_answered_is_superseded_and_not_counted.json |
| TheCorrectionAgainstARealStore::test_a_withheld_correction_is_listed_with_its_reason | converted | cli-shape/test_dispositions__test_a_withheld_correction_is_listed_with_its_reason.json |

`sqlite-ddl/test_dispositions__test_seeded_sql_is_visible_to_relay_cli.json` came before this lane.
It is the runner's own `sql_seed` proof and does not correspond to any test function in this file.

## Round 2 resolution

- **cli-observed-files**: `expect.observe` now captures the untouched database's size and the
  sorted state-directory entries. Both fixtures assert size 0 and exactly `relay.sqlite3`, so
  neither a schema write nor a WAL/shm sidecar can slip through.
- **cli-relay-host**: `given.host` now starts a case-local Unix App Server for the real relay
  CLI. The delivery fixture reads the queued state, calls `deliver`, then reads the dispatched
  state and current attempt. The correction fixture scripts an archived recipient and checks
  that delivery is withheld as `recipient_archived` with both correction counts preserved.

No case in this file remains blocked.
