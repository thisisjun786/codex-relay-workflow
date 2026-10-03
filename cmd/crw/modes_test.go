package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The modes the usage line and the invalid-choice message advertise are the table's listed rows,
// and pabcd is dispatched without being advertised until the switch lists it.
func TestModeTableDrivesUsageAndDispatch(t *testing.T) {
	var out, errOut strings.Builder
	if code := run(context.Background(), "crw", []string{"nope"}, &out, &errOut); code != parserExit ||
		!strings.Contains(errOut.String(), "usage: crw [-h] [--version] {relay,bridge,hook,skill,doctor,install,help,version} ...") ||
		!strings.Contains(errOut.String(), "(choose from 'relay', 'bridge', 'hook', 'skill', 'doctor', 'install', 'help', 'version')") {
		t.Fatalf("unknown mode: %d %q", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), "crw", []string{"pabcd", "--help"}, &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "usage: crw pabcd") || errOut.Len() != 0 {
		t.Errorf("crw pabcd --help: %d %q %q", code, out.String(), errOut.String())
	}
	errOut.Reset()
	if code := run(context.Background(), "crw", []string{"pabcd"}, &out, &errOut); code != parserExit || !strings.Contains(errOut.String(), "crw pabcd: error: the following arguments are required: verb") {
		t.Errorf("crw pabcd: %d %q", code, errOut.String())
	}
}

// crw hook <event> --leg <leg> reaches the harness through the mode table, and every other argument
// list reaches the Stop adapter as before (its own tests run the binary and read its journal).
func TestHookRoutesALegToTheHarnessAndTheRestToTheStopAdapter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	over := filepath.Join(home, "oversized.json")
	if err := os.WriteFile(over, []byte(strings.Repeat("x", 4*1024*1024+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin := os.Stdin
	t.Cleanup(func() { os.Stdin = stdin })
	for _, c := range []struct {
		args []string
		out  string
	}{
		{[]string{"hook", "stop", "--leg", "stop-checking-pabcd-continuation"}, `{"decision":"block","reason":"[crw] hook input exceeded 4194304 bytes; refusing to bypass policy enforcement"}` + "\n"},
		{[]string{"hook", "stop", "--leg", "no-such-leg"}, ""},
		{[]string{"hook", "stop"}, ""},
		{[]string{"hook", "--plugin-launch"}, ""},
		{[]string{"hook"}, ""},
	} {
		f, err := os.Open(over)
		if err != nil {
			t.Fatal(err)
		}
		os.Stdin = f
		var out, errOut strings.Builder
		code := run(context.Background(), "crw", c.args, &out, &errOut)
		f.Close()
		if code != 0 || out.String() != c.out || errOut.Len() != 0 {
			t.Errorf("%v: %d %q %q, want 0 %q", c.args, code, out.String(), errOut.String(), c.out)
		}
	}
}
