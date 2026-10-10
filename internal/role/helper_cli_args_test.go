package role

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// helperArgsUnread is a stdin that fails the test when it is read: help and argument refusals answer before stdin.
type helperArgsUnread struct{ t *testing.T }

func (r helperArgsUnread) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Fatal("stdin was read")
	return 0, nil
}

// dispatch --help (and -h, help) prints the dispatch usage and exits 0 without reading stdin, through the command and
// through the role CLI; an argument dispatch does not take is refused before stdin, in the JSON error shape.
func TestHelperArgsDispatchHelp(t *testing.T) {
	env, _ := home(t)
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		var out bytes.Buffer
		if code := DispatchCommand(args, helperArgsUnread{t}, &out, env); code != 0 || !strings.HasPrefix(out.String(), "usage: crw role helper dispatch < request.json\n") || !strings.Contains(out.String(), `"action":"claim"`) {
			t.Fatalf("dispatch %q = %d %q", args, code, out.String())
		}
		var cli, errOut bytes.Buffer
		if code := CLI(append([]string{"helper", "dispatch"}, args...), helperArgsUnread{t}, &cli, &errOut, env); code != 0 || cli.String() != out.String() || errOut.Len() != 0 {
			t.Fatalf("role helper dispatch %q = %d %q %q", args, code, cli.String(), errOut.String())
		}
	}
	var out bytes.Buffer
	code := DispatchCommand([]string{"extra"}, helperArgsUnread{t}, &out, env)
	var answer map[string]string
	if code != 1 || json.Unmarshal(out.Bytes(), &answer) != nil || !strings.Contains(answer["error"], "dispatch takes no arguments (got 'extra')") {
		t.Fatalf("dispatch extra = %d %q", code, out.String())
	}
}

// crw role --help names the dispatch command and its stdin form.
func TestHelperArgsRoleHelpNamesDispatch(t *testing.T) {
	env, _ := home(t)
	var out, errOut bytes.Buffer
	if code := CLI([]string{"--help"}, helperArgsUnread{t}, &out, &errOut, env); code != 0 || !strings.Contains(out.String(), "crw role helper dispatch < request.json") || !strings.Contains(out.String(), "stdin") {
		t.Fatalf("crw role --help = %d %q", code, out.String())
	}
	if help := HelperHelp(); !strings.Contains(help, "crw role helper dispatch < request.json") {
		t.Fatalf("helper help lacks dispatch: %q", help)
	}
}

// list and get refuse a trailing token they do not take; --help or -h after any verb is that verb's help and changes nothing.
func TestHelperArgsTrailingAndVerbHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"list", "extra"}, "list takes no arguments (got 'extra')"},
		{[]string{"get", "reviewer", "extra"}, "get takes exactly one role (got 'extra')"},
		{[]string{"get", "reviewer", "--global", "extra"}, "get takes exactly one role (got '--global')"},
	} {
		if got := ParseHelperArgs(tc.args); got.Err != tc.want {
			t.Errorf("%q: error %q, want %q", tc.args, got.Err, tc.want)
		}
	}
	env, dir := home(t)
	for _, args := range [][]string{
		{"list", "--help"}, {"get", "--help"}, {"get", "reviewer", "--help"}, {"get", "reviewer", "-h"},
		{"set", "reviewer", "--mode", "model", "--help"}, {"set", "--help"}, {"reset", "reviewer", "--help"}, {"reset", "-h"},
		{"register", "--help"}, {"register", "executor", "-h"},
	} {
		if got := ParseHelperArgs(args); !reflect.DeepEqual(got, HelperArgs{Action: "help"}) {
			t.Errorf("%q: got %+v, want help", args, got)
		}
		if result := RunHelper(ParseHelperArgs(args), env); result.Code != 0 || result.Output != HelperHelp() {
			t.Errorf("%q: %d %q", args, result.Code, result.Output)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, StoreFile)); !os.IsNotExist(err) {
		t.Fatalf("verb help wrote the store: %v", err)
	}
}

// A value flag without a value is refused, a following flag is never taken as its value, and a value that starts with '-'
// is written --flag=value; the documented forms keep working.
func TestHelperArgsFlagValues(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"set", "reviewer", "--model"}, "--model requires a value"},
		{[]string{"set", "reviewer", "--prompt"}, "--prompt requires a value"},
		{[]string{"set", "reviewer", "--model", "--effort", "high"}, "--model requires a value; write a value that starts with '-' as --model=<value>"},
		{[]string{"set", "reviewer", "--prompt", "--global"}, "--prompt requires a value; write a value that starts with '-' as --prompt=<value>"},
		{[]string{"set", "reviewer", "--mode"}, "--mode requires a value"},
		{[]string{"set", "reviewer", "--effort"}, "--effort requires a value"},
		{[]string{"set", "reviewer", "--fallback-model"}, "--fallback-model requires a value"},
		{[]string{"set", "reviewer", "--fallback-effort", "--clear-fallback"}, "--fallback-effort requires a value; write a value that starts with '-' as --fallback-effort=<value>"},
		{[]string{"set", "reviewer", "--clear-prompt=x"}, "unknown flag '--clear-prompt=x'"},
	} {
		if got := ParseHelperArgs(tc.args); got.Err != tc.want {
			t.Errorf("%q: error %q, want %q", tc.args, got.Err, tc.want)
		}
	}
	for _, tc := range []struct {
		args []string
		want HelperArgs
	}{
		{[]string{"set", "reviewer", "--prompt=--terse"}, HelperArgs{Action: "set", Role: Reviewer, Patch: RolePatch{PromptOverride: Some("--terse")}}},
		{[]string{"set", "reviewer", "--model=m1", "--effort=high"}, HelperArgs{Action: "set", Role: Reviewer, Patch: RolePatch{Model: Some("m1"), Effort: Some(EffortHigh)}}},
		{[]string{"set", "reviewer", "--prompt="}, HelperArgs{Action: "set", Role: Reviewer, Patch: RolePatch{PromptOverride: Some("")}}},
		{[]string{"set", "executor", "--mode", "model", "--model", "m1"}, HelperArgs{Action: "set", Role: Executor, Patch: RolePatch{Mode: Some(ModeModel), Model: Some("m1")}}},
		{[]string{"set", "reviewer", "--prompt", "first", "--clear-prompt", "--prompt", "last", "--global"}, HelperArgs{Action: "set", Role: Reviewer, Scope: ScopeGlobal, Patch: RolePatch{PromptOverride: Some("last")}}},
		{[]string{"get", "reviewer", "--global"}, HelperArgs{Action: "get", Role: Reviewer, Scope: ScopeGlobal}},
		{[]string{"--global"}, HelperArgs{Action: "list", Scope: ScopeGlobal}},
		{[]string{"list"}, HelperArgs{Action: "list"}},
		{nil, HelperArgs{Action: "list"}},
	} {
		if got := ParseHelperArgs(tc.args); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %+v, want %+v", tc.args, got, tc.want)
		}
	}
}
