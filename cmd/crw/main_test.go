package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// crw's help and version print to stdout and exit 0, and a command line it cannot read prints
// usage to stderr and exits 2, as the relay's and the bridge's command lines do.
func TestRun_help_and_version_exit_0_and_an_unreadable_line_exits_2(t *testing.T) {
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
		{[]string{"bogus"}, 2, "", `invalid choice: "bogus"`},
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

// The relay command line has no --version: its parser refuses it with exit 2, under the
// codex-session-relay link too.
func TestRun_relay_link_has_no_version_flag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), "/x/codex-session-relay", []string{"--version"}, &stdout, &stderr); code != 2 || stdout.Len() != 0 {
		t.Fatalf("code=%d stdout=%q", code, stdout.String())
	}
}

// An unknown ~user in --state is a host error (exit 3) naming the user, worded alike by every
// command family on the one dispatch path (decisions R2B-2, R3C-4).
func TestRun_unknown_user_state_is_a_host_error(t *testing.T) {
	for _, line := range [][]string{{"store-identity"}, {"settings-show", "--task", "t"}, {"claim", "--event", "e"},
		{"intent-show", "--workspace", "w"}, {"fault-show", "--fault", "f"}, {"capacity-show"}, {"route-show"}, {"sync-status", "--relationship", "r"}} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), "crw", append([]string{"relay", "--state", "~crw_user_that_does_not_exist_20/s"}, line...), &stdout, &stderr)
		want := "{\n  \"error\": \"host\",\n  \"detail\": \"the state directory cannot be resolved: cannot determine home directory for \\\"crw_user_that_does_not_exist_20\\\": user: unknown user crw_user_that_does_not_exist_20\"\n}\n"
		if code != 3 || stdout.String() != want {
			t.Fatalf("%v: code=%d stdout=%q", line, code, stdout.String())
		}
	}
}

// `crw bridge` and the codex-thread-bridge link both reach the bridge's own entry point, which
// answers --version like `codex-thread-bridge --version` (the package version, exit 0).
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

// `crw doctor` is the host-level diagnosis, distinct from `crw relay doctor`; its
// declared-schema form is what the swap gate asks a candidate binary.
func TestRun_doctor_dispatches_to_the_host_doctor(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), "crw", []string{"doctor", "declared-schema", "--json"}, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "{\n  \"readable\": true,\n  \"objects\": {") {
		t.Fatalf("declared-schema: code=%d stdout=%.200q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), "crw", []string{"doctor", "bogus"}, &stdout, &stderr); code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), `unknown argument "bogus"`) {
		t.Fatalf("an unknown doctor form: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := run(context.Background(), "crw", []string{"help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "{relay,bridge,hook,skill,doctor,install,help,version}") {
		t.Fatalf("usage: %q", stdout.String())
	}
}

func TestRun_install_dispatches_to_the_installer(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), "crw", []string{"install", "unpack"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), `crw install: error: argument command: invalid choice: "unpack"`) {
		t.Fatalf("an unknown install command: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := run(context.Background(), "crw", []string{"install", "help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "{install,update,rollback,remove,status,register-mcp,hook}") {
		t.Fatalf("install usage: code=%d stdout=%q", code, stdout.String())
	}
}

// The relay command table is complete: every command the relay CLI's root parser (and service's)
// declares is registered, and every command the doctor lists as offline or host required is one.
func TestRun_every_relay_parser_choice_is_registered(t *testing.T) {
	for _, name := range append(argparse.Commands(""), argparse.Commands("service")...) {
		if !dispatch.Registered(name) {
			t.Errorf("relay command %s is not registered", name)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), "crw", []string{"relay", "--state", t.TempDir(), "doctor"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doctor exit %d: %s", code, stderr.String())
	}
	var report struct {
		Reachability struct{ OfflineCommands, HostRequiredCommands []string } `json:"actorReachability"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	listed := append(report.Reachability.OfflineCommands, report.Reachability.HostRequiredCommands...)
	if len(listed) < 100 {
		t.Fatalf("the doctor lists %d commands", len(listed))
	}
	for _, name := range listed {
		if _, ok := dispatch.Lookup(name); !ok {
			t.Errorf("the doctor lists %q, which is not a registered relay command", name)
		}
	}
}

// Every relay command a package registers on the relay CLI is read by its declared parser; the
// CLI has no second parser to fall back on (wave R1).
func TestRun_every_relay_command_has_an_argparse_spec(t *testing.T) {
	names := dispatch.Names()
	if len(names) < 50 {
		t.Fatalf("only %d relay commands registered", len(names))
	}
	for _, name := range names {
		if _, ok := argparse.Specs[name]; !ok {
			t.Errorf("relay command %s has no argparse spec", name)
		}
	}
}

// The relay command line's contract, for every command: --help prints the command's usage and
// options on stdout and exits 0; a line its parser cannot read prints the usage and the reason on
// stderr, nothing on stdout, and exits 2, naming the flag it is missing, the word it does not
// know or the value a choice refuses (decision R3C-1). The handler never runs for either, so no
// store is created.
func TestRun_every_relay_command_line_has_the_usage_contract(t *testing.T) {
	relay := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		state := t.TempDir()
		code := run(context.Background(), "crw", append([]string{"relay", "--state", state}, args...), &stdout, &stderr)
		if _, err := os.Stat(filepath.Join(state, "relay.sqlite3")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%q created a store: %v", args, err)
		}
		return code, stdout.String(), stderr.String()
	}
	for _, name := range dispatch.Names() {
		words := strings.Fields(name)
		spec := argparse.Specs[name]
		code, stdout, stderr := relay(append(words, "--help")...)
		if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "usage: crw relay "+name) {
			t.Errorf("%s --help: exit %d stdout %q stderr %q", name, code, stdout, stderr)
		}
		for _, action := range spec.Actions {
			if !strings.Contains(stdout, action.Flags[0]) {
				t.Errorf("%s --help does not list %s", name, action.Flags[0])
			}
		}
		code, stdout, stderr = relay(append(words, "--definitely-not-a-flag")...)
		if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "usage: crw relay "+name) || !strings.Contains(stderr, "unrecognized arguments: --definitely-not-a-flag") {
			t.Errorf("%s --definitely-not-a-flag: exit %d stdout %q stderr %q", name, code, stdout, stderr)
		}
		for _, action := range spec.Actions {
			if len(action.Choices) == 0 {
				continue
			}
			code, stdout, stderr = relay(append(words, action.Flags[0], "definitely-not-a-choice")...)
			if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "usage: crw relay "+name) || !strings.Contains(stderr, "invalid choice") || !strings.Contains(stderr, "definitely-not-a-choice") {
				t.Errorf("%s %s definitely-not-a-choice: exit %d stdout %q stderr %q", name, action.Flags[0], code, stdout, stderr)
			}
		}
		var required []string
		for _, action := range spec.Actions {
			if action.Required {
				required = append(required, action.Flags[0])
			}
		}
		if len(required) == 0 {
			continue
		}
		code, stdout, stderr = relay(words...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, "the following arguments are required: "+strings.Join(required, ", ")) {
			t.Errorf("%s with no options: exit %d stdout %q stderr %q", name, code, stdout, stderr)
		}
	}
	for _, line := range [][]string{{"no-such-command"}, {"service", "no-such-command"}, {"--no-such-option", "doctor"}, {"--state"}} {
		code, stdout, stderr := relay(line...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, "usage: crw relay") || !strings.Contains(stderr, "error: ") {
			t.Errorf("%q: exit %d stdout %q stderr %q", line, code, stdout, stderr)
		}
	}
}
