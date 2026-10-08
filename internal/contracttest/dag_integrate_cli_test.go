package contracttest

import "testing"

// The commit-acceptance and integration commands (CRW-965) are proved against the built crw, every case of
// testdata/fixtures/dag-integrate-cli-cases.json replayed through `crw relay`: each prints the stdout bytes and exit code its
// golden holds. The cases cover each new command's usage and absent-store refusal, the refusal family a host without a
// store answers before any git or verification runs.
func TestDagIntegrateCommands_the_built_crw_prints_its_frozen_answers(t *testing.T) {
	replayCLICases(t, "dag-integrate-cli-cases.json", false, []string{"dag-integrate", "dag-integrate-push", "dag-integration-observe", "dag-accept"})
}
