# Round 2 lane L7: parent title, shipped schema, release

Todo 6 of crw-go-port. Converted means a source function invokes only its fixture. Kept means the live-interleaving exception in contract/README.md applies. Blocked means the source function and its assertions remain in Python because the named runner kind/observation is absent. No source assertion is weakened.

Counts (source functions): test_parent_title.py 9 = 0 converted, 0 kept, 9 blocked; test_relay_schema_shipped.py 1 = 0 converted, 0 kept, 1 blocked; test_release.py 8 = 0 fully converted, 0 kept, 8 blocked (one happy-path fixture pre-existed).

The README describes general ordered steps, SQL observations and release operations, but the implemented `contract.runner.core.run_scenario` dispatches only entry/hook/status, cli, stop, agreement, git, mcp, release and appserver. `contract.runner.release.run` executes exactly one workflow block in one freshly initialized clone, returning only exit/stdout/stderr/calls/remote_main. Documentation is not an executable runner capability.

## Missing runner kinds and observations

- **P1 parent-title command:** no `parent-title` run kind that invokes the real `plugins/crw/skills/crw-run/scripts/parent_title.py` with `decide`, `readback` or `replay`, raw or JSON stdin and captured exit/stdout/stderr. `cli` invokes the relay module instead.
- **P2 parent-title fixture-set mutation:** no case-local `replay --fixtures` tree generated from the shipped title fixtures, with selective removal by decision/branch/readback or a wrong expected title and a second replay with `--allow-unreached`. This is needed to preserve the coverage-guard mutation assertions, not just a successful replay.
- **S1 shipped-schema comparison:** no `shipped-schema` kind opening a fresh real Store, reading `swapgate.SCHEMA_OBJECTS_QUERY`, comparing every shipped object after `swapgate._normalised` (including lost/changed names) and observing the shipped snapshot cardinality greater than 50. `cli` SQL queries alone do not perform this normalization against the read-only snapshot.
- **R1 release ordered steps and state mutation:** the release kind cannot run several workflow blocks against the *same* clone/remote/fake GH state, change `ci`, `tag`, `release` or `push` between invocations, or override environment per step. One-block fixtures would lose the source tests' refusal/recovery assertions.
- **R2 release Git and fake-remote observations:** release exposes only `remote_main`, not remote tag/ref presence and SHA, local tag SHA, created.txt, gh log slices, or the state of main after failed intermediate steps. The originals assert these independently of exit status.

## test_parent_title.py

| Case | Status | Required kind |
| --- | --- | --- |
| test_replay_passes_on_the_shipped_fixtures | blocked | P1 (exit and replay stdout) |
| test_decide_returns_the_title_on_stdout | blocked | P1 (JSON title and exit) |
| test_unreadable_request_exits_two | blocked | P1 (raw invalid stdin and exit) |
| test_invalid_decision_exits_two | blocked | P1 (decision and exit) |
| test_readback_exit_codes_separate_verified_from_the_rest | blocked | P1 (three ordered readbacks and unread JSON) |
| test_an_empty_fixture_set_fails | blocked | P1 P2 (empty replay tree and stderr) |
| test_a_missing_decision_fails_the_coverage_guard | blocked | P1 P2 (partial tree, refusal, allowed replay) |
| test_a_wrong_expectation_fails_even_when_unreached_is_allowed | blocked | P1 P2 (mutated expected title, refusal) |
| test_each_guard_fails_when_its_own_cases_are_removed | blocked | P1 P2 (three selective fixture removals with distinct guard messages) |

## test_relay_schema_shipped.py

| Case | Status | Required kind |
| --- | --- | --- |
| test_every_shipped_object_keeps_the_text_the_swap_gate_compares | blocked | S1 (lost/changed object comparison and shipped cardinality) |

## test_release.py

| Case | Status | Fixture / required kind |
| --- | --- | --- |
| test_owner_dispatch_inputs | blocked (happy-path fixture pre-existing) | `contract/fixtures/records/test_release__test_owner_dispatch_inputs.json`; its existing source function still asserts the additional owner, rerun, branch, SHA, tag and notes refusals, so it is **not yet a thin runner**. Full conversion requires R1. |
| test_source_requires_latest_exact_push_ci_and_remote_tag | blocked | R1 R2 (CI cases, tag peeling and same remote) |
| test_dry_run_reads_without_publication_and_missing_token_refuses_writes | blocked | R1 R2 (read-only state, absent tag and release before/after refusal) |
| test_publication_creates_source_release_then_fast_forwards_main | blocked | R1 R2 (publish then same-SHA recovery and log delta) |
| test_publication_refuses_conflicts_and_reports_partial_main_failure | blocked | R1 R2 (conflicts, tag-only partial failure, readback mismatch) |
| test_draft_collision_refuses_before_creating_immutable_tag | blocked | R1 R2 (two draft cases and absent remote tag) |
| test_tag_only_failure_reports_and_recovers_same_commit | blocked | R1 R2 (tag-only failure then recovery without rewriting tag) |
| test_release_go_runs_after_publication_and_snapshot_stays_in_validation | kept | Release workflow sequencing and job conditions; the contract corpus has no Go release-workflow runner kind. Go owner: todo 47 (CI scripts). |
| test_release_go_refuses_a_tag_that_is_not_the_released_commit | kept | Release-tag/commit validation through the release workflow; the contract corpus has no Go release-workflow runner kind. Go owner: todo 47 (CI scripts). |
| test_old_and_divergent_sources_do_not_advance_main | blocked | R1 R2 (behind-main and outside-dev cases with remote readbacks) |

The pre-existing `test_owner_dispatch_inputs` fixture checks only the happy owner dispatch; its source test retains all refusal checks. It is counted as blocked until the source can become a thin runner without dropping refusal assertions. No new fixture is added when doing so would only assert a subset of the original case.
