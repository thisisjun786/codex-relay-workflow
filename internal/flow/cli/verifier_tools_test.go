package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// CRW-1319: the merged tree of an integration batch verifies itself: with neither crw-dev nor go on PATH and a Go root in the
// configuration, the batch's verifier runs the merged tree's own crw-dev (go run -tags dev ./cmd/crw-dev ci local) with that Go,
// in the merged worktree, with the batch's head, base and record.
func TestCILocalIntegrationRunsTheMergedTreesOwnCrwDev(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := filepath.Join(t.TempDir(), "go")
	log := filepath.Join(t.TempDir(), "argv")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$PWD\" \"$PATH\" \"$GOROOT\" \"$@\" > " + log + "\n"
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "cmd", "crw-dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	// a caller without TMPDIR (the one temp function's default) is no note of its own either (CRW-1557, CRW-1570)
	t.Setenv("TMPDIR", "")
	var notes bytes.Buffer
	verify := CILocalIntegration(crwconfig.VerifierTools{GoRoot: root}, &notes)
	env := []string{"CRW_VERIFY_HEAD=h1", "CRW_VERIFY_BASE=b1", "CRW_VERIFY_RECORD=/r/record.json"}
	if err := verify(context.Background(), tree, env); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"run", "-tags", "dev", "./cmd/crw-dev", "ci", "local", "--commit", "h1", "--base", "b1", "--record", "/r/record.json", "--runner", "local"}
	wantTree, err := filepath.EvalSymlinks(tree)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 3 || got[0] != wantTree || !strings.HasPrefix(got[1], filepath.Join(root, "bin")+":") || got[2] != root || strings.Join(got[3:], " ") != strings.Join(want, " ") {
		t.Errorf("the batch ran %q", got)
	}
	if notes.Len() != 0 {
		t.Errorf("a configured verifier wrote a note: %s", notes.String())
	}
}

// CRW-1319: with nothing configured the batch's verifier takes crw-dev from PATH (fail-open) and writes a note; with nothing on
// PATH either, the batch stops on a host error whose reason names the missing setting, never a failed verification.
func TestCILocalIntegrationWithNothingConfiguredIsAHostError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var notes bytes.Buffer
	verify := CILocalIntegration(crwconfig.VerifierTools{Config: "/home/u/.config/crw/config.json"}, &notes)
	err := verify(context.Background(), t.TempDir(), []string{"CRW_VERIFY_HEAD=h1", "CRW_VERIFY_BASE=b1", "CRW_VERIFY_RECORD=/r/record.json"})
	var host *dagsched.IntegrationVerifierHostError
	if !errors.As(err, &host) || !strings.Contains(host.Detail, "verifier.go_root") || !strings.Contains(host.Detail, "/home/u/.config/crw/config.json") {
		t.Errorf("nothing configured, nothing on PATH: %v", err)
	}
	if !strings.Contains(notes.String(), "fail-open") {
		t.Errorf("no fail-open note: %q", notes.String())
	}
}

// CRW-1319: the lane reads the verifier's tools from the run's configuration file; a file that names none is the PATH fallback,
// and so is a verifier section the verifier could not use: the lane starts, and the verifier's note says the section was ignored
// (final verification d4ed9281: a reversible setting warns and fails open).
func TestLaneVerifierToolsComeFromTheConfiguration(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "crw.json")
	if err := os.WriteFile(config, []byte(`{"verifier":{"go_root":"/opt/go"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": home, "CRW_CONFIG": config}
	getenv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	if tools, err := laneVerifierTools(getenv); err != nil || tools.GoRoot != "/opt/go" || tools.Config != config {
		t.Errorf("configured: %+v %v", tools, err)
	}
	if err := os.WriteFile(config, []byte(`{"verifier":{"go_root":"go"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tools, err := laneVerifierTools(getenv)
	if err != nil || tools.GoRoot != "" || tools.Mode() != "path" || !strings.Contains(tools.Ignored, "verifier.go_root") {
		t.Errorf("a relative go root: %+v %v", tools, err)
	}
	if cmd, err := tools.Command(t.TempDir(), ""); err != nil || !strings.Contains(cmd.Note, "ignored") {
		t.Errorf("the note of a relative go root: %+v %v", cmd, err)
	}
	delete(env, "CRW_CONFIG")
	if tools, err := laneVerifierTools(getenv); err != nil || tools.GoRoot != "" || tools.CrwDev != "" {
		t.Errorf("no file: %+v %v", tools, err)
	}
}
