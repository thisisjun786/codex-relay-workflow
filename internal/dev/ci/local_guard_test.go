//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// C1 at run time: a step whose ci.yml run text changed is refused even when its name and place
// match, because the local run would then perform different work than the runner.
func TestLocalPlan_a_changed_run_text_is_refused(t *testing.T) {
	workflow, err := parseWorkflow(localWorkflow(t))
	if err != nil {
		t.Fatal(err)
	}
	plan := localPlan()
	for i := range plan {
		job := workflowJobNamed(workflow, plan[i].name)
		if job == nil {
			continue
		}
		for n := range plan[i].steps {
			if plan[i].steps[n].kind != localRun || n >= len(job.steps) {
				continue
			}
			job.steps[n].run += "\n# changed"
			if problems := localPlanProblems(plan, workflow); len(problems) == 0 {
				t.Errorf("a changed run text of %s step %d is not refused", plan[i].name, n)
			}
			return
		}
	}
	t.Fatal("the table has no run step to change")
}

// C3: a commit range is part of what a record covers, so a record over another range is not reused.
func TestLocalReuse_a_different_commit_range_is_never_reused(t *testing.T) {
	base := verificationRecord{Schema: recordSchema, Result: localPass, TreeHash: "t", CiDigest: "c",
		BaseCommit: "b", Range: "r1", OS: localHostOS(), Arch: localHostArch(),
		Tools: map[string]string{}, Dependencies: map[string]string{}}
	if ok, why := localReuse(base, base); !ok {
		t.Fatalf("an identical record is refused: %s", why)
	}
	other := base
	other.Range = "r2"
	if ok, _ := localReuse(base, other); ok {
		t.Error("a record over another commit range is reused")
	}
}

// C3: GOFLAGS that name an external modfile or overlay read inputs the keys do not cover, so such a
// record is never reused.
func TestLocalReuse_an_external_modfile_or_overlay_is_never_reused(t *testing.T) {
	base := verificationRecord{Schema: recordSchema, Result: localPass, TreeHash: "t", CiDigest: "c",
		BaseCommit: "b", Range: "r1", OS: localHostOS(), Arch: localHostArch(),
		Tools: map[string]string{}, Dependencies: map[string]string{}}
	for _, flags := range []string{"-modfile=/elsewhere/go.mod", "-overlay=/elsewhere/overlay.json"} {
		reused, current := base, base
		reused.GoFlags, current.GoFlags = flags, flags
		if ok, _ := localReuse(reused, current); ok {
			t.Errorf("a record with GOFLAGS %q is reused", flags)
		}
	}
}

// C6 and comment-c6: the version probe runs the tool the step's PATH names, from a directory of its
// own and without the caller's GOENV, so the recorded version is the one the steps use.
func TestLocalTools_the_version_probe_uses_the_steps_PATH_and_an_isolated_directory(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "probe.log")
	script := "#!/bin/sh\n{ echo \"pwd=$PWD\"; echo \"goenv=[$GOENV]\"; } > \"" + log + "\"\necho 'go version go1.27.1 linux/amd64'\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOENV", "/caller/goenv")
	if got := localObserveTool("go", bin); got != "1.27.1" {
		t.Fatalf("the probe reads %q from the step's go, want 1.27.1", got)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	probe := string(data)
	if strings.Contains(probe, "goenv=[/caller/goenv]") {
		t.Errorf("the probe sees the caller's GOENV: %q", probe)
	}
	if wd, _ := os.Getwd(); strings.Contains(probe, "pwd="+wd+"\n") {
		t.Errorf("the probe runs in the caller's directory: %q", probe)
	}
}

// The record is published through a temporary file of its own name, never a fixed name next to it
// that another run could share.
func TestLocalRecord_publication_does_not_touch_a_fixed_temp_name(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record.json")
	stale := record + ".tmp"
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeRecord(record, verificationRecord{Schema: recordSchema, Result: localPass}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(stale); string(data) != "stale" {
		t.Errorf("publishing the record rewrote the fixed name %s", stale)
	}
}
