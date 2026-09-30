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
		root, _ := filepath.Abs("../../..")
		binary, alias := packageBinary(t)
		home := t.TempDir()
		env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/state", "XDG_CONFIG_HOME="+home+"/config", "XDG_CACHE_HOME="+home+"/cache", "XDG_DATA_HOME="+home+"/data", "CODEX_HOME="+home+"/codex", "CRW_MARKER_ROOT="+home+"/markers", "CRW_REFUSE_LIVE_STATE=", "PATH="+root+"/internal/relay/cli/testdata:"+os.Getenv("PATH"))
		assertEvidenceBytes(t, env, filepath.Join(root, ".venv/bin/python"), alias, binary, "rich", []string{"merge-evidence", "--repository", "owner/repo", "--pull-request", "7", "--page-size", "1", "--page-budget", "2"}, "")
	})
}

// assertEvidenceBytes compares the built binary, in both shapes, with what the Python CLI
// answered (recorded: see pythonProcess) for args in env, the scripted forge scenario and input
// on stdin. Python ran against testdata/gh and Go runs against its Go twin (fakeGH).
func assertEvidenceBytes(t *testing.T, env []string, python, alias, binary, scenario string, args []string, input string) {
	t.Helper()
	runEnv := append(append([]string{}, env...), "CRW_FORGE_SCENARIO="+scenario)
	goEnv := append(append([]string{}, runEnv...), goForgePath(t))
	label := scenario + " " + oracleLabel(args...)
	// Match the installed console entry point's C stack depth, not runpy (-m),
	// which consumes another frame at CPython's JSON recursion boundary.
	oracle := `import sys; from codex_session_relay.cli import main; sys.exit(main(sys.argv[1:]))`
	want := pythonProcess(t, oracleKey(t, "console "+label), runEnv, input, python, append([]string{"-c", oracle}, args...)...)
	want.out = evidenceTimestamp.ReplaceAllString(want.out, "<time>")
	for _, shape := range []string{"alias", "multicall"} {
		path, argv := alias, args
		if shape == "multicall" {
			path, argv = binary, append([]string{"relay"}, args...)
			oracle := `import argparse,sys; from codex_session_relay import cli; p=cli.build_parser(); p.prog='crw relay'; children=next(a.choices for a in p._actions if isinstance(a,argparse._SubParsersAction)); [(setattr(c,'prog','crw relay '+n)) for n,c in children.items()]; cli.build_parser=lambda:p; sys.exit(cli.main(sys.argv[1:]))`
			want = pythonProcess(t, oracleKey(t, "crw relay "+label), runEnv, input, python, append([]string{"-c", oracle}, args...)...)
			want.out = evidenceTimestamp.ReplaceAllString(want.out, "<time>")
		}
		got := runParityProcessInput(t, goEnv, input, path, argv...)
		got.out = evidenceTimestamp.ReplaceAllString(got.out, "<time>")
		if got != want {
			t.Fatalf("%s %v byte diff\nGo exit=%d stderr=%q\n%s\nPython exit=%d stderr=%q\n%s", shape, args, got.code, got.err, got.out, want.code, want.err, want.out)
		}
	}
}
