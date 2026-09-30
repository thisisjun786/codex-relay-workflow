package contracttest

import (
	"path/filepath"
	"testing"
)

// mergeTurnCommands are the fifteen merge-turn-* commands todo 26 registers (cli.py:5051-5213).
// internal/relay/mergeturn/testdata/cli_cases.json (make_cli_cases.py) replayed through
// `crw relay` must print the stdout bytes and exit code the Python CLI printed (python_cli.json,
// from gen_cli.py), covering every command and CCL-2..CCL-9, each a named case
// (`-run 'TestMergeTurnCommands/ccl2_refusal_exits_two'`), and the unsent merge-turn grant that
// show --message renders (show_message_unsent_grant, carried from todo 20).
var mergeTurnCommands = []string{"merge-turn-request", "merge-turn-ready", "merge-turn-acknowledge",
	"merge-turn-attest", "merge-turn-request-return", "merge-turn-check", "merge-turn-land",
	"merge-turn-unknown", "merge-turn-resolve", "merge-turn-restate-base", "merge-turn-release",
	"merge-turn-withdraw", "merge-turn-show"}

func TestMergeTurnCommands_the_built_crw_prints_what_python_printed(t *testing.T) {
	replayCLICases(t, filepath.Join("internal", "relay", "mergeturn", "testdata"), mergeTurnCommands)
}
