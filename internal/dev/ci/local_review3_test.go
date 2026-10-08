//go:build dev

package ci

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// CRW-964 pre-merge finding d1: a tracked symlink fails the run, because the verified tree may hold no link
// the run could follow outside the commit (a .gitleaks.toml link could name a file the record never sees).
func TestLocal_a_tracked_symlink_fails_the_run(t *testing.T) {
	repo := newLocalFixture(t)
	if err := os.Symlink(filepath.Join(repo.root, "file.txt"), filepath.Join(repo.root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	repo.commit()
	record := filepath.Join(t.TempDir(), "record.json")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), record), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localFail {
		t.Errorf("a run over a tracked symlink passes (result %q)", made.Result)
	}
}

// CRW-964 pre-merge finding d2: a ci.yml setting the reader does not read is refused, never ignored.
func TestLocalWorkflow_a_setting_the_reader_does_not_read_is_refused(t *testing.T) {
	cases := map[string]string{
		"job defaults":       "name: x\n\non:\n  push:\n\njobs:\n  validate:\n    runs-on: ubuntu-24.04\n    defaults:\n      run:\n        working-directory: web\n    steps:\n      - name: a\n        run: echo a\n",
		"step shell":         "name: x\n\non:\n  push:\n\njobs:\n  validate:\n    runs-on: ubuntu-24.04\n    steps:\n      - name: a\n        shell: bash\n        run: echo a\n",
		"top-level defaults": "name: x\n\non:\n  push:\n\ndefaults:\n  run:\n    shell: bash\n\njobs:\n  validate:\n    runs-on: ubuntu-24.04\n    steps:\n      - name: a\n        run: echo a\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseWorkflow(text); err == nil {
				t.Error("the reader accepted a setting it does not read")
			}
		})
	}
}

// CRW-964 pre-merge finding d3: a relative heavy-gate path names the caller's executable, resolved once against
// the directory the run was started in, not against the probe or the step directory.
func TestLocal_a_relative_heavy_gate_is_resolved_against_the_callers_directory(t *testing.T) {
	repo := newLocalFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gate.sh"), []byte("#!/bin/sh\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(start) })
	opts := localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "record.json"))
	opts.HeavyGate = "./gate.sh"
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localPass {
		t.Errorf("a relative gate in the caller's directory fails the run (result %q)", made.Result)
	}
}
