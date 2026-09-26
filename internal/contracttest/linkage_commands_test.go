package contracttest

import (
	"path/filepath"
	"testing"
)

// linkageCommands are the twelve linkage commands todo 26 part A1 registers (cli.py:4132-4391).
// The only cli-shape fixtures that reach them use linkage-bind beside commands other parts port,
// so every one is proved here against the built crw: internal/relay/linkage/testdata/cli_cases.json
// replayed through `crw relay` must print the stdout bytes and exit code the Python CLI printed
// (python_cli.json, from gen_cli.py). No skip path, so it holds under CRW_CONTRACT_STRICT=1.
var linkageCommands = []string{"linkage-bind", "linkage-supervise", "linkage-peer", "linkage-attach",
	"linkage-outstanding", "linkage-completion", "linkage-handover", "linkage-directive", "linkage-settle",
	"linkage-down", "linkage-up", "linkage-counterpart"}

func TestLinkageCommands_the_built_crw_prints_what_python_printed(t *testing.T) {
	replayCLICases(t, filepath.Join("internal", "relay", "linkage", "testdata"), linkageCommands)
}
