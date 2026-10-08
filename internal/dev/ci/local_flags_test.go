//go:build dev

package ci

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A full run keeps only the GOFLAGS that do not select or skip tests, so a host GOFLAGS with -run
// cannot make a step pass while running part of the suite.
func TestLocal_a_step_keeps_no_test_selection_from_the_callers_GOFLAGS(t *testing.T) {
	t.Setenv("GOFLAGS", "-run=Nothing -count=0 -list=. -p=4")
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	opts := localRunOptions(repo, localFixturePlan(`test "$GOFLAGS" = "-p=4"`), record)
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if step := made.Jobs[0].Steps[1]; step.Result != localPassed {
		t.Errorf("the step is %q (%s): the test selection reached it", step.Result, step.Reason)
	}
}

// The version probe runs with the steps' isolation: no host toolchain selection and no host HOME.
func TestLocalTools_the_version_probe_has_no_host_toolchain_or_home(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "probe.log")
	script := "#!/bin/sh\n{ echo \"toolchain=[$GOTOOLCHAIN]\"; echo \"home=$HOME\"; } > \"" + log + "\"\necho 'go version go1.27.1 linux/amd64'\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOTOOLCHAIN", "go1.99.0")
	if got := localObserveTool("go", bin, ""); got != "1.27.1" {
		t.Fatalf("the probe reads %q, want 1.27.1", got)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	probe := string(data)
	if strings.Contains(probe, "toolchain=[go1.99.0]") {
		t.Errorf("the probe inherits the host toolchain: %q", probe)
	}
	if home := os.Getenv("HOME"); home != "" && strings.Contains(probe, "home="+home+"\n") {
		t.Errorf("the probe runs with the host HOME: %q", probe)
	}
}

// A base given short or as a ref is recorded as the full commit it names.
func TestLocalCurrentKeys_a_short_base_is_recorded_as_a_full_commit(t *testing.T) {
	repo := newLocalFixture(t)
	full, err := localRev(repo.root, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	opts := localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "record.json"))
	opts.Base = full[:12]
	current, err := localCurrentKeys(opts)
	if err != nil {
		t.Fatal(err)
	}
	if current.BaseCommit != full {
		t.Errorf("the base is recorded as %q, want the full commit %q", current.BaseCommit, full)
	}
}

// The Go probe runs inside a module that carries the verified commit's go.mod, so the toolchain it
// reports is the one a step in that module selects.
func TestLocalTools_the_go_probe_sees_the_commits_go_mod(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "probe.log")
	script := "#!/bin/sh\n{ if [ -f go.mod ]; then echo gomod=yes; else echo gomod=no; fi; } > \"" + log + "\"\necho 'go version go1.27.1 linux/amd64'\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := localObserveToolIn("go", bin, []byte("module probe\n\ngo 1.27\n"), ""); got != "1.27.1" {
		t.Fatalf("the probe reads %q, want 1.27.1", got)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "gomod=yes") {
		t.Errorf("the probe does not run in a module with the commit's go.mod: %q", data)
	}
}
