package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// The Python CLIs this binary replaces are argparse programs: -h/--help and the bridge's
// --version print to stdout and exit 0, and a command line they cannot parse prints usage to
// stderr and exits 2 (measured with `uv run --no-sync codex-thread-bridge --version|bogus` and
// `codex-session-relay` with no command). crw's own help/version follow the same contract.
func TestRun_help_and_version_exit_like_the_python_clis(t *testing.T) {
	for _, test := range []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{[]string{"help"}, 0, "usage: crw", ""},
		{[]string{"--help"}, 0, "usage: crw", ""},
		{[]string{"version"}, 0, version, ""},
		{[]string{"--version"}, 0, version, ""},
		{nil, 2, "", "the following arguments are required: command"},
		{[]string{"bogus"}, 2, "", "invalid choice: 'bogus'"},
		{[]string{"relay"}, 2, "", "the following arguments are required: command"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), "crw", test.args, &stdout, &stderr)
		if code != test.code || !strings.Contains(stdout.String(), test.stdout) || !strings.Contains(stderr.String(), test.stderr) ||
			(test.stdout == "" && stdout.Len() != 0) {
			t.Errorf("%v: code=%d stdout=%q stderr=%q", test.args, code, stdout.String(), stderr.String())
		}
	}
}

// Python's relay CLI has no --version: argparse refuses it with exit 2. The multi-call link
// must answer the same, not print a version the Python CLI never printed.
func TestRun_relay_link_has_no_version_flag_like_python(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), "/x/codex-session-relay", []string{"--version"}, &stdout, &stderr); code != 2 || stdout.Len() != 0 {
		t.Fatalf("code=%d stdout=%q", code, stdout.String())
	}
}

// An unknown ~user in --state is Python's pathlib RuntimeError: exit 3 with the host envelope.
func TestRun_unknown_user_state_is_a_python_host_error(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), "crw", []string{"relay", "--state", "~crw_user_that_does_not_exist_20/s", "store-identity"}, &stdout, &stderr)
	want := "{\n  \"error\": \"host\",\n  \"detail\": \"RuntimeError: Could not determine home directory.\"\n}\n"
	if code != 3 || stdout.String() != want {
		t.Fatalf("code=%d stdout=%q", code, stdout.String())
	}
}

// `crw bridge` and the codex-thread-bridge link both reach the bridge's own entry point, which
// answers --version like `codex-thread-bridge --version` (0.1.0, exit 0).
func TestRun_bridge_mode_and_link_dispatch_to_the_bridge(t *testing.T) {
	for _, call := range []struct {
		program string
		args    []string
	}{{"crw", []string{"bridge", "--version"}}, {"/x/codex-thread-bridge", []string{"--version"}}} {
		// mcp.Run writes to the process's own stdout, so only the exit is observed here; the
		// printed text is covered by internal/bridge/mcp's own tests.
		if code := run(context.Background(), call.program, call.args, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Errorf("%s %v: exit %d", call.program, call.args, code)
		}
	}
}
