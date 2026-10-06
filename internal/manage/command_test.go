package manage

import (
	"context"
	"strings"
	"testing"
)

// Run with no arguments writes the usage to stderr and exits 2.
func TestRunWithoutArgumentsWritesUsageToStderr(t *testing.T) {
	var out, errOut strings.Builder
	if code := Run(context.Background(), nil, strings.NewReader(""), &out, &errOut); code != usageExit || out.Len() != 0 ||
		!strings.Contains(errOut.String(), "usage: crw manage") {
		t.Fatalf("no arguments: %d %q %q", code, out.String(), errOut.String())
	}
}

// -h, --help and help write the usage and the registered commands, name-sorted, to
// stdout and exit 0.
func TestRunHelpWritesTheUsageAndTheRegisteredCommands(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		var out, errOut strings.Builder
		if code := Run(context.Background(), []string{arg}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
			t.Fatalf("%s: %d %q %q", arg, code, out.String(), errOut.String())
		}
		text := out.String()
		if !strings.Contains(text, "usage: crw manage") {
			t.Fatalf("%s: the usage line is missing: %q", arg, text)
		}
		at := -1
		for _, c := range coreCommands() {
			i := strings.Index(text, "  "+c.Name+"\t"+c.Summary)
			if i < 0 {
				t.Fatalf("%s: the line for %q is missing from %q", arg, c.Name, text)
			}
			if i < at {
				t.Errorf("%s: %q is listed out of name order: %q", arg, c.Name, text)
			}
			at = i
		}
	}
}

// A name that is not registered writes the usage to stderr and exits 2.
func TestRunUnknownCommandWritesUsageToStderr(t *testing.T) {
	var out, errOut strings.Builder
	code := Run(context.Background(), []string{"nope"}, strings.NewReader(""), &out, &errOut)
	if code != usageExit || out.Len() != 0 || !strings.Contains(errOut.String(), "invalid command \"nope\"") {
		t.Fatalf("unknown command: %d %q %q", code, out.String(), errOut.String())
	}
}

// Run builds the Env a subcommand runs with and hands it the arguments after its name.
func TestRunBuildsTheEnvAndPassesTheRemainingArguments(t *testing.T) {
	saved := coreRegistry
	defer func() { coreRegistry = saved }()
	var got *Env
	var gotArgs []string
	Register(Command{Name: "core-probe", Summary: "a probe", Run: func(_ context.Context, e *Env, args []string) int {
		got, gotArgs = e, args
		return 7
	}})
	stdin := strings.NewReader("in")
	var out, errOut strings.Builder
	if code := Run(context.Background(), []string{"core-probe", "one", "two"}, stdin, &out, &errOut); code != 7 {
		t.Fatalf("the probe's status: %d", code)
	}
	if got == nil || got.Stdin != stdin || got.Stdout != &out || got.Stderr != &errOut || got.Getenv == nil || got.Now == nil || got.Executable == "" {
		t.Fatalf("the Env is not filled: %+v", got)
	}
	if strings.Join(gotArgs, ",") != "one,two" {
		t.Errorf("the probe got %q", gotArgs)
	}
}

// Register panics on a name that is already registered and leaves the registry as it was.
func TestRegisterPanicsOnADuplicateName(t *testing.T) {
	before := strings.Join(coreNames(), ",")
	defer func() {
		if recover() == nil {
			t.Error("Register did not panic on a duplicate name")
		}
		if after := strings.Join(coreNames(), ","); after != before {
			t.Errorf("the registry changed: %q -> %q", before, after)
		}
	}()
	Register(Command{Name: "config", Summary: "a second config", Run: func(context.Context, *Env, []string) int { return 0 }})
}
