package contracttest

import (
	"path/filepath"
	"testing"
)

// mergeTurnCommands are the fifteen merge-turn-* commands todo 26 registers (cli.py:5051-5213).
// internal/relay/mergeturn/testdata/cli_cases.json (make_cli_cases.py) replayed through
// `crw relay` must print the stdout bytes and exit code the Python CLI printed (python_cli.json,
// from gen_cli.py), covering CCL-2..CCL-9 and every command. No skip path, so it holds under
// CRW_CONTRACT_STRICT=1.
var mergeTurnCommands = []string{"merge-turn-request", "merge-turn-ready", "merge-turn-acknowledge",
	"merge-turn-attest", "merge-turn-request-return", "merge-turn-check", "merge-turn-land",
	"merge-turn-unknown", "merge-turn-resolve", "merge-turn-restate-base", "merge-turn-release",
	"merge-turn-withdraw", "merge-turn-show"}

func TestMergeTurnCommands_the_built_crw_prints_what_python_printed(t *testing.T) {
	replayCLICases(t, filepath.Join("internal", "relay", "mergeturn", "testdata"), mergeTurnCommands)
}

// Each CCL property of test_coordination_cli.py, replayed byte for byte against Python.
func mergeTurnCase(t *testing.T, names ...string) {
	replayCLICasesOnly(t, filepath.Join("internal", "relay", "mergeturn", "testdata"), mergeTurnCommands, names)
}

func Test26_CCL_2_a_refusal_prints_its_reason_and_exits_two(t *testing.T) {
	mergeTurnCase(t, "ccl2_refusal_exits_two")
}

func Test26_CCL_3_request_holds_and_an_unread_target_blocks_the_check(t *testing.T) {
	mergeTurnCase(t, "ccl3_unread_target_holds")
}

func Test26_CCL_4_malformed_json_is_a_bad_invocation_naming_the_flag(t *testing.T) {
	mergeTurnCase(t, "ccl4_malformed_json")
}

func Test26_CCL_5_a_returning_parent_asks_by_task_id(t *testing.T) {
	mergeTurnCase(t, "ccl5_parent_task")
}

func Test26_CCL_6_show_takes_exactly_one_selector(t *testing.T) {
	mergeTurnCase(t, "ccl6_selectors")
}

func Test26_CCL_7_acknowledging_twice_converges(t *testing.T) {
	mergeTurnCase(t, "ccl7_acknowledge_twice")
}

func Test26_CCL_8_a_foreign_grant_is_refused_at_the_surface(t *testing.T) {
	mergeTurnCase(t, "ccl8_foreign_grant")
}

func Test26_CCL_9_a_stated_cause_travels_with_the_withdrawn_readiness(t *testing.T) {
	mergeTurnCase(t, "ccl9_withdraw_cause")
}

// Carried from todo 20: show --message on an unsent merge-turn grant renders the grant notice
// (delivery._render_grant with report.required_for_candidate) byte for byte.
func Test26_show_message_renders_an_unsent_merge_turn_grant(t *testing.T) {
	mergeTurnCase(t, "show_message_unsent_grant")
}
