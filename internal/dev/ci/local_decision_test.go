//go:build dev

package ci

import (
	"io"
	"path/filepath"
	"testing"
)

// CRW-964 parent ruling 2, d7: the value a changed-path decision writes to GITHUB_OUTPUT is recorded
// in the step's record entry, so the record shows what the decision was.
func TestLocal_a_decision_step_records_what_it_wrote(t *testing.T) {
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	command := "echo \"changed=true\" >> \"$GITHUB_OUTPUT\""
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan(command), record), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if step := made.Jobs[0].Steps[1]; step.Decision != "changed=true" {
		t.Errorf("the decision is %q, want the line the step wrote", step.Decision)
	}
}
