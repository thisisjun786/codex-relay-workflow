# test_coordination_cli.py - contract conversion (round 2 lane L5)

Source: `packages/codex-session-relay/tests/test_coordination_cli.py`. All fixtures run the real Python program in an isolated temporary home. Unconverted source assertions remain unchanged.
Result: 7 converted, 0 kept, 12 blocked; 19 total.


| Case | Status | Fixture or exact missing kind |
| --- | --- | --- |
| TheRegistrationTableIsComplete::test_every_new_command_is_registered_with_a_handler | blocked | parser-handler-table (offline parser default callable) |
| TheRegistrationTableIsComplete::test_every_new_command_is_classified_offline_and_none_needs_a_host | blocked | parser-command-classification (host-required set) |
| TheRegistrationTableIsComplete::test_the_new_commands_are_not_marker_commands | blocked | parser-command-classification (marker set) |
| AFullMergeTurnThroughTheCommandSurface::test_request_and_check_answer_with_their_state_and_an_unread_target_holds | blocked | cli-step-acknowledge + multi-field ledger filtering |
| AFullMergeTurnThroughTheCommandSurface::test_a_refusal_prints_its_reason_and_exits_two | converted | contract/fixtures/cli-shape/test_coordination_cli__test_a_refusal_prints_its_reason_and_exits_two.json |
| AFullMergeTurnThroughTheCommandSurface::test_malformed_json_refuses_instead_of_reporting_a_host_fault | blocked | cli-step-variants with per-flag detail checks |
| AFullMergeTurnThroughTheCommandSurface::test_a_parent_that_came_back_asks_with_the_only_identifier_it_has | converted | contract/fixtures/cli-shape/test_coordination_cli__test_a_parent_that_came_back_asks_with_the_only_identifier_it_has.json |
| AFullMergeTurnThroughTheCommandSurface::test_two_selectors_refuse_instead_of_answering_about_one_of_them | converted | contract/fixtures/cli-shape/test_coordination_cli__test_two_selectors_refuse_instead_of_answering_about_one_of_them.json |
| AFullMergeTurnThroughTheCommandSurface::test_acknowledging_a_grant_twice_converges_on_one_record | blocked | cli-ledger-filter by evidenceKind (length of filtered entries, not full ledger) |
| AFullMergeTurnThroughTheCommandSurface::test_a_grant_that_is_not_this_tenures_is_refused_at_the_surface | converted | contract/fixtures/cli-shape/test_coordination_cli__test_a_grant_that_is_not_this_tenures_is_refused_at_the_surface.json |
| AFullMergeTurnThroughTheCommandSurface::test_a_stated_cause_travels_with_the_readiness_it_withdrew | blocked | cli-ledger-filter by evidenceKind |
| TheCapacityAndRegionSurfaces::test_a_slot_is_reserved_released_and_reported | converted | contract/fixtures/cli-shape/test_coordination_cli__test_a_slot_is_reserved_released_and_reported.json |
| TheCapacityAndRegionSurfaces::test_a_declared_bound_reports_how_its_number_was_reached | converted | contract/fixtures/cli-shape/test_coordination_cli__test_a_declared_bound_reports_how_its_number_was_reached.json |
| TheCapacityAndRegionSurfaces::test_a_region_is_proposed_settled_and_shown | blocked | cli-coordination-steps: runner lacks filtered ledger predicates or shell-replay of generated recovery commands |
| TheCapacityAndRegionSurfaces::test_a_follow_up_nobody_took_cannot_be_reported_done | converted | contract/fixtures/cli-shape/test_coordination_cli__test_a_follow_up_nobody_took_cannot_be_reported_done.json |
| TheCapacityAndRegionSurfaces::test_a_late_acceptance_is_refused_and_the_answering_side_carries_the_terms | blocked | cli-shell-command-execution from output + filtered agreement view |
| TheCapacityAndRegionSurfaces::test_the_named_acceptance_command_runs_for_a_task_id_with_a_space | blocked | cli-shell-command-execution from output |
| TheCapacityAndRegionSurfaces::test_the_command_a_second_successor_refusal_names_runs_as_printed | blocked | cli-shell-command-execution from refusal detail |
| TheCapacityAndRegionSurfaces::test_an_acceptance_with_a_condition_is_a_bad_invocation | blocked | cli-coordination-steps: runner lacks filtered ledger predicates or shell-replay of generated recovery commands |

The registration-table tests inspect parser handler defaults and Python sets. These are not interchangeable with successful CLI invocation. The grant-acknowledgement test filters ledger entries by evidence kind, so asserting the full ledger length would not preserve its claim.

# test_management_cli.py - contract conversion (round 2 lane L5)

Source: `packages/codex-session-relay/tests/test_management_cli.py`. All fixtures run the real Python program in an isolated temporary home. Unconverted source assertions remain unchanged.
Result: 6 converted, 0 kept, 22 blocked; 28 total.


| Case | Status | Fixture or exact missing kind |
| --- | --- | --- |
| Lifecycle::test_declare_bind_register_claim_emit_and_then_a_receipted_stop | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| Refusals::test_an_unmanaged_workspace_is_released_and_nothing_is_written | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| Refusals::test_intent_show_says_unmanaged_rather_than_failing | converted | contract/fixtures/cli-shape/test_management_cli__test_intent_show_says_unmanaged_rather_than_failing.json |
| Refusals::test_an_explicit_assignment_with_no_intent_reads_unmanaged | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| Refusals::test_registering_a_relationship_the_relay_never_opened_is_refused | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| Refusals::test_registration_is_refused_when_the_relay_cannot_be_confirmed | converted | contract/fixtures/cli-shape/test_management_cli__test_registration_is_refused_when_the_relay_cannot_be_confirmed.json |
| Refusals::test_a_relationship_from_another_dispatch_is_refused_with_a_reason | converted | contract/fixtures/cli-shape/test_management_cli__test_a_relationship_from_another_dispatch_is_refused_with_a_reason.json |
| Refusals::test_a_disposition_outside_the_vocabulary_is_rejected_by_the_parser | converted | contract/fixtures/cli-shape/test_management_cli__test_a_disposition_outside_the_vocabulary_is_rejected_by_the_parser.json |
| Refusals::test_a_stop_payload_that_is_not_json_is_a_usage_error | converted | contract/fixtures/cli-shape/test_management_cli__test_a_stop_payload_that_is_not_json_is_a_usage_error.json |
| Refusals::test_a_malformed_intent_stays_managed_in_the_explicit_view | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| Refusals::test_an_absent_intent_is_still_unmanaged_in_the_explicit_view | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| Refusals::test_a_stop_payload_file_that_is_not_utf8_is_a_usage_error | blocked | cli-binary-given-files (non-UTF8) |
| Refusals::test_a_hold_that_cannot_be_recorded_is_refused_at_the_command_line | converted | contract/fixtures/cli-shape/test_management_cli__test_a_hold_that_cannot_be_recorded_is_refused_at_the_command_line.json |
| Refusals::test_observe_is_the_default_mode_on_the_command_line | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| GuardStoreSelection::test_an_implicit_selection_is_refused_rather_than_guessed_at | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| GuardStoreSelection::test_a_one_run_state_override_holding_another_socket_s_store_is_refused | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| GuardStoreSelection::test_the_command_the_installed_adapter_builds_refuses_a_mismatched_store | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| GuardStoreSelection::test_settings_that_configure_no_socket_still_classify_rather_than_refusing | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| GuardStoreSelection::test_a_one_run_state_does_not_settle_an_ambiguous_discovery | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| GuardStoreSelection::test_nor_does_it_hold_a_finished_child_in_hold_mode | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| GuardStoreSelection::test_an_inherited_state_directory_does_not_settle_it_either | blocked | cli-shell-command-execution from recovery payload |
| GuardStoreSelection::test_a_one_run_state_beside_a_store_without_provenance_is_refused | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| GuardStoreSelection::test_a_one_run_state_naming_the_only_claiming_store_is_judged | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| GuardStoreSelection::test_once_discovery_names_one_store_a_later_stop_is_judged | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| GuardStoreSelection::test_an_explicit_db_path_keeps_the_exemption_under_the_same_ambiguity | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| GuardStoreSelection::test_the_recorded_intent_db_path_keeps_the_exemption_with_no_flag_at_all | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| GuardStoreSelection::test_a_turn_released_on_its_own_declaration_is_never_refused | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |
| GuardStoreSelection::test_a_readiness_with_no_registered_relationship_is_never_refused | blocked | cli-marker-lifecycle: runner does not create marker workspace and relay registration with selected DB provenance, or observe marker assignment/hook directories; guard selection needs cli-discovery-stores |

The `cli` kind prepends `--state` and creates no marker directory/managed registration. Guard-selection tests require multiple independent discovered stores and an adapter-generated argv; asserting only a refusal from a missing store is not equivalent.

# test_reporting_cli.py - contract conversion (round 2 lane L5)

Source: `packages/codex-session-relay/tests/test_reporting_cli.py`. All fixtures run the real Python program in an isolated temporary home. Unconverted source assertions remain unchanged.
Result: 5 converted, 0 kept, 1 blocked; 6 total.


| Case | Status | Fixture or exact missing kind |
| --- | --- | --- |
| ReportingShowCli::test_help_lists_the_command_and_every_required_selector | converted | contract/fixtures/cli-shape/test_reporting_cli__test_help_lists_the_command_and_every_required_selector.json |
| ReportingShowCli::test_a_missing_required_flag_exits_2_before_any_read | converted | contract/fixtures/cli-shape/test_reporting_cli__test_a_missing_required_flag_exits_2_before_any_read.json |
| ReportingShowCli::test_global_state_is_required_and_a_socket_is_refused | blocked | cli-global-options without automatic --state and --socket override |
| ReportingShowCli::test_invalid_identity_exits_4_and_leaves_the_store_absent | converted | contract/fixtures/cli-shape/test_reporting_cli__test_invalid_identity_exits_4_and_leaves_the_store_absent.json |
| ReportingShowCli::test_a_completed_observation_exits_0_without_creating_the_store | converted | contract/fixtures/cli-shape/test_reporting_cli__test_a_completed_observation_exits_0_without_creating_the_store.json |
| ReportingShowCli::test_doctor_lists_reporting_show_as_offline_and_not_host_required | converted | contract/fixtures/cli-shape/test_reporting_cli__test_doctor_lists_reporting_show_as_offline_and_not_host_required.json |

`cli` always prepends `--state`, so it cannot test the missing-global-state branch. Its automatic socket selection cannot assert explicit `--socket` refusal. The original case remains intact.

# test_worker_policy.py - contract conversion (round 2 lane L5)

Source: `packages/codex-session-relay/tests/test_worker_policy.py`. All fixtures run the real Python program in an isolated temporary home. Unconverted source assertions remain unchanged.
Result: 0 converted, 2 kept, 16 blocked; 18 total.


| Case | Status | Fixture or exact missing kind |
| --- | --- | --- |
| WorkerPolicyEvidence::test_configured_caller_cannot_supply_an_unconfigured_workers_policy | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_matching_live_worker_is_ready_and_reads_do_not_republish | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_supervised_receipt_names_worker_not_the_parent_process | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_new_caller_policy_does_not_change_the_workers_snapshot | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_configured_worker_does_not_authorize_an_unconfigured_caller | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_role_pair_cannot_override_the_same_policys_allowlist | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_public_unresolved_snapshot_does_not_publish_private_paths | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_role_pair_mismatch_and_unsupported_requirements_are_not_ready | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_absent_service_read_creates_nothing | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_dead_or_stopped_worker_cannot_qualify_by_retained_receipt | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_receipt_identity_must_match_daemon_worker_pid_and_start_ticks | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_store_replacement_same_store_id_still_invalidates_receipt | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_non_pid_scalars_cannot_select_the_foreground_fallback | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_malformed_or_foreign_receipts_never_pass | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_record_changing_during_read_is_not_a_ready_snapshot | kept | README exception: synchronous mid-read method swap; retain original Python interleaving test |
| WorkerPolicyEvidence::test_lock_io_failure_is_not_ownership_and_missing_lock_is_not_created | kept | README exception: injected flock I/O failure at read boundary; retain original Python test |
| WorkerPolicyEvidence::test_same_digest_with_changed_role_payload_is_not_agreement | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |
| WorkerPolicyEvidence::test_replaced_worker_requires_its_own_publication | blocked | worker-policy-service: runner has no live worker process, owned daemon lock, policy publication/readiness and retained receipt observations |

Worker receipt tests include live process identity (PID and start ticks), process stop/death, lock ownership, store inode replacement, and policy-digest divergence. No `cli`, `stop`, or `hook` kind exposes the worker publication/ownership handshake; translating them to simple file assertions would weaken the tests.
