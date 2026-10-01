package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var evidenceTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}\+00:00`)

type processResult struct {
	code     int
	out, err string
}

func runParityProcess(t *testing.T, env []string, path string, args ...string) processResult {
	t.Helper()
	return runParityProcessInput(t, env, "", path, args...)
}

func runParityProcessInput(t *testing.T, env []string, input, path string, args ...string) processResult {
	t.Helper()
	if filepath.Base(path) == "crw" || filepath.Base(path) == "codex-session-relay" {
		ownedArgs(t, "", args, "go")
	}
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(input)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	return processResult{code, out.String(), stderr.String()}
}

// The real CLI and gh subprocess surface supplies every collector object shape:
// reviewThread/review/comment, workflow/check/status details, superseded runs,
// gates (including early failure), enumerations/pages, handoff, and restatement.
// Only independently sampled wall-clock timestamps are normalized; object bytes,
// query provenance, exit codes, and stderr are compared without reserialization.
func Test24BuiltBinaryEvidenceDefectBytes(t *testing.T) {
	t.Parallel()
	t.Run("representative", func(t *testing.T) {
		binary, alias := packageBinary(t)
		home := t.TempDir()
		env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/state", "XDG_CONFIG_HOME="+home+"/config", "XDG_CACHE_HOME="+home+"/cache", "XDG_DATA_HOME="+home+"/data", "CODEX_HOME="+home+"/codex", "CRW_MARKER_ROOT="+home+"/markers", "CRW_REFUSE_LIVE_STATE=")
		assertEvidenceBytes(t, env, alias, binary, "rich", []string{"merge-evidence", "--repository", "owner/repo", "--pull-request", "7", "--page-size", "1", "--page-budget", "2"}, "")
	})
}

// assertEvidenceBytes checks the built binary's answer, in both shapes, for args in env, the
// scripted forge scenario (fakeGH) and input on stdin, against the goldens: exit, stdout with its
// wall-clock instants masked, and stderr.
func assertEvidenceBytes(t *testing.T, env []string, alias, binary, scenario string, args []string, input string) {
	t.Helper()
	runEnv := append(append([]string{}, env...), "CRW_FORGE_SCENARIO="+scenario)
	goEnv := append(append([]string{}, runEnv...), goForgePath(t))
	label := scenario + " " + keyLabel(args...)
	keys := map[string]string{"alias": goldenKey(t, "console "+label), "multicall": goldenKey(t, "crw relay "+label)}
	for _, shape := range []string{"alias", "multicall"} {
		path, argv := alias, args
		if shape == "multicall" {
			path, argv = binary, append([]string{"relay"}, args...)
		}
		got := runParityProcessInput(t, goEnv, input, path, argv...)
		got.out = evidenceTimestamp.ReplaceAllString(got.out, "<time>")
		expectRunErr(t, keys[shape], got.code, got.out, got.err, envAnchors(goEnv, args...)...)
	}
}

// envAnchors are the placeholder anchors of a process run with args in env: its arguments and
// each environment value that names a temporary or fixed directory.
func envAnchors(env []string, args ...string) []string {
	anchors := append([]string{}, args...)
	for _, entry := range env {
		if _, value, ok := strings.Cut(entry, "="); ok && (strings.Contains(value, os.TempDir()) || strings.Contains(value, fixedRoot)) {
			anchors = append(anchors, value)
		}
	}
	return anchors
}
