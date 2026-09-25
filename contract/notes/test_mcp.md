# Lane L6: test_mcp.py and test_worktree.py

Round 2 classification at this revision. A converted case has a thin source runner and a separate fixture executed against the real Python bridge. A kept case uses the live-interleaving exception in contract/README.md. Blocked cases retain their original assertions and name the missing runner observation or setup; no blocked case is counted as converted just because another fixture covers a similar operation.

Counts (source functions, not parametrized invocations): **test_mcp.py 11 = 11 converted, 0 kept, 0 blocked**; **test_worktree.py 28 = 16 converted, 3 kept, 9 blocked**. The separate `test_contract_git_readiness_proof` is a converted spike test outside the original 28. `test_worktree__test_appserver_project_read_proof.json` has no source function. Three lost-response parameter IDs each have their own fixture. Fix-up: readiness now compares the receipt checkout to the actual destination and creation cwd; both lost-response fixtures with a known thread check its ID is in host threads and its initial revision is the starting base. All three lost-response fixtures compare full ordered host call traces at both replay boundaries, rather than just call counts.

## test_mcp.py

All eleven cases are converted under `contract/fixtures/mcp-tools/test_mcp__<case>.json` and have thin runners in `test_mcp.py`:

- `test_the_schema_itself_refuses_a_mutation_that_states_no_pair`
- `test_an_argument_a_caller_invents_grants_it_nothing`
- `test_an_unconfigured_host_accepts_a_stated_pair_and_says_so`
- `test_a_configured_host_admits_the_approved_pair_and_refuses_the_rest`
- `test_a_configured_policy_that_cannot_be_used_stops_the_server`
- `test_the_plugin_launched_bridge_reports_the_host_policy_and_checks_the_role`
- `test_a_plugin_record_naming_no_policy_starts_exactly_as_before`
- `test_a_server_started_expecting_another_digest_never_starts`
- `test_mcp_forwards_json_shaped_pagination_cursors`
- `test_real_mcp_stdio_discovery_create_read_and_dedup`
- `test_mcp_socket_alias_restart_does_not_repeat_creation`

The MCP runner now has case-local policy files/environment, bare process failure, the real plugin launcher, JSON-shaped cursor configuration, mid-session host mutation, and sequential server restart with socket alias. These formerly blocked kinds no longer block these eleven cases.

## test_worktree.py

Converted under `contract/fixtures/git/test_worktree__<case>[__<param-id>].json` with thin source runners:

- `test_readiness_launch_retains_exact_base_without_carrying_dirty_changes`
- `test_invalid_permission_contract_has_no_filesystem_or_api_effects` (3 parameter IDs)
- `test_only_available_full_commit_ids_are_accepted` (5 parameter IDs)
- `test_environment_mismatch_retains_actual_receipt_and_withholds_prompt` (6 parameter IDs)
- `test_lost_responses_replay_after_restart_without_duplicate_artifacts` (thread-start, thread-name-set, turn-start)
- `test_git_hooks_filters_and_fsmonitor_are_not_run`
- `test_known_thread_failure_retains_worktree_and_prevents_retry`
- `test_a_reserved_destination_is_an_attempt_even_with_no_request_sent`
- `test_a_prompt_whose_frame_never_went_out_is_not_left_unknown`
- `test_a_prompt_the_host_refused_is_recorded_as_refused`
- `test_replay_survives_removed_checkout_and_source_paths`
- `test_concurrent_requests_cannot_adopt_same_destination`
- `test_inherited_git_environment_cannot_redirect_checkout`
- `test_a_dispatched_worktree_task_is_annotated_like_the_other_paths`
- `test_a_retained_receipt_survives_validation_this_version_added`
- `test_a_worktree_launch_transmits_the_pair_it_was_authorized_for`

Kept under the contract/README.md live-interleaving exception, unchanged:

- `test_interrupted_dispatch_is_retained_and_never_repeated` (4 parameter IDs): pause the RPC after the chosen method, then cancel or time out while in flight.
- `test_checkout_change_during_thread_start_withholds_prompt`: switch the checkout branch during a paused thread/start.
- `test_cancellation_after_git_creation_retains_checkout_without_starting_task` (2 parameter IDs): interrupt the exact Git subprocess between creation and checkout.

Blocked (source tests remain intact):

| Case | Exact missing runner kind or observation |
| --- | --- |
| `test_launch_preserves_trailing_whitespace_in_paths` (6 parameter IDs) | `git` bridge needs a pre-launch rename of the source or destination path preserving a trailing space, tab or newline, plus case-root path expansion in arguments; it currently creates a fixed `source` and `isolated`. |
| `test_path_collisions_leave_existing_content_untouched` (5 parameter IDs) | `git` bridge needs pre-launch destination setups (directory, file, symlink, inside source, inside another Git repo) and byte-preserving content observations. |
| `test_mcp_isolated_launch_and_followup_are_durable` | No combined `mcp` + `git` repository kind: two sequential MCP stdio servers must invoke create_worktree_thread, send_message_to_thread and get_operation on one state directory, then inspect the real Git checkout and host turns. |
| `test_a_lost_project_read_leaves_no_worktree_and_keeps_the_id` | `git` bridge needs a mid-step snapshot of destination absence before the retry; final `paths` alone would weaken the first assertion (the retry creates the checkout). |
| `test_a_known_validation_failure_keeps_its_request_id` | `git` bridge needs a pre-existing destination directory setup before the first launch; no `given`/step creates it. |
| `test_destination_conditional_filters_are_disabled_before_checkout` | `git` bridge needs a conditional include config file with a real smudge command and a marker observer, independently of its bundled `hostile_git` setup. |
| `test_a_worktree_launch_without_a_stated_pair_creates_nothing` | `git` bridge does not catch/observe `ExecutionRefused.code` and `.field` for each omitted setting; it currently catches only ValueError/TransportError. |
| `test_a_worktree_exception_covers_only_the_destination_it_names` | `git` bridge does not install a configured ExecutionPolicy with a case-root-bound exception, nor catch/observe `ExecutionRefused.code` on the out-of-scope call. |
| `test_a_worktree_launch_follows_the_authorization_not_the_arguments` | `git` bridge cannot install the custom relabelling policy object; a JSON allowlist is not equivalent to a policy that authorizes a *different* model and effort than requested. Keep as a Python test until a Go package test models the custom policy (contract/README.md declined policy-object kind). |

Remaining cases keep their full original assertions until their fixtures are green on Python.
