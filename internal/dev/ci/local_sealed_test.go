//go:build dev

package ci

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-964 pre-merge finding d1: the record names the origin without its credentials. A remote URL with a
// user and password is written as the URL alone, and the record file carries neither.
func TestLocal_the_record_never_carries_origin_credentials(t *testing.T) {
	repo := newLocalFixture(t)
	runGit(repo.root, "remote", "add", "origin", "https://user:s3cret-value@example.invalid/owner/repo.git")
	record := filepath.Join(t.TempDir(), "record.json")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), record), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeRecord(record, made); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"s3cret-value", "user:"} {
		if strings.Contains(string(data), secret) || strings.Contains(made.Repository, secret) {
			t.Errorf("the record carries origin credentials (%q)", secret)
		}
	}
}

// CRW-964 pre-merge finding d2: a heavy gate that starts the step elsewhere, or with a variable of its own,
// does not change the step: the step runs in its own directory and sees only the sealed environment.
func TestLocal_a_heavy_gate_cannot_change_the_steps_directory_or_environment(t *testing.T) {
	repo := newLocalFixture(t)
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate.sh")
	script := "#!/bin/sh\nCRW_TEST_CANARY=leak\nexport CRW_TEST_CANARY\ncd / || exit 1\nexec \"$@\"\n"
	if err := os.WriteFile(gate, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	command := "test -z \"$CRW_TEST_CANARY\" && case \"$(pwd -P)\" in */tree) ;; *) exit 1 ;; esac"
	opts := localRunOptions(repo, localFixturePlan(command), filepath.Join(dir, "record.json"))
	opts.HeavyGate = gate
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if step := made.Jobs[0].Steps[1]; step.Result != localPassed {
		t.Errorf("the step is %q (%s): the gate changed its directory or its environment", step.Result, step.Reason)
	}
}
