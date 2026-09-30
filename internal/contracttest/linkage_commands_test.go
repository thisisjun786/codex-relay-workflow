package contracttest

import (
	"testing"
)

// linkageCommands are the twelve linkage commands todo 26 part A1 registers (cli.py:4132-4391).
// The only cli-shape fixtures that reach them use linkage-bind beside commands other parts port,
// so every one is proved here against the built crw: testdata/fixtures/linkage-cli-cases.json
// replayed through `crw relay` must print the stdout bytes and exit code its golden holds (first
// taken as what the Python CLI printed).
var linkageCommands = []string{"linkage-bind", "linkage-supervise", "linkage-peer", "linkage-attach",
	"linkage-outstanding", "linkage-completion", "linkage-handover", "linkage-directive", "linkage-settle",
	"linkage-down", "linkage-up", "linkage-counterpart"}

func TestLinkageCommands_the_built_crw_prints_what_python_printed(t *testing.T) {
	replayCLICases(t, "linkage-cli-cases.json", false, linkageCommands)
}
