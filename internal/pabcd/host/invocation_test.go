package host_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/affordance"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// specialHomes are homes whose runtime pointer path holds every character a shell or a Markdown code span reads:
// a space, a single quote, a dollar sign naming a variable the shell has, a double quote, a backtick and a backslash.
var specialHomes = []string{
	"/home/sp ace", "/home/it's", "/home/$A1_TEST_SEGMENT/x", `/home/q"uote`, "/home/b`tick", `/home/b\slash`,
	"/home/all '$A1_TEST_SEGMENT\"`\\ ``x",
}

func pointer(home string) string {
	return filepath.Join(home, ".local", "share", "crw-runtime", "current", "bin", "crw")
}

// CRW-1138 criterion 1: the generated runtime pointer is one literal argument in every supported shell, read by a
// harmless argv printer: no expansion, no split, nothing lost.
func TestInvocationIsOneLiteralArgumentInEveryShell(t *testing.T) {
	var shells []string
	for _, name := range []string{"sh", "dash", "bash", "zsh"} {
		if path, err := exec.LookPath(name); err == nil {
			shells = append(shells, path)
		}
	}
	if len(shells) == 0 {
		t.Skip("no POSIX shell")
	}
	for _, home := range specialHomes {
		inv, err := host.Invocation(func(key string) (string, bool) {
			if key == "HOME" {
				return home, true
			}
			return "", false
		})
		if err != nil {
			t.Fatal(home, err)
		}
		for _, shell := range shells {
			cmd := exec.Command(shell, "-c", `printf '[%s]' `+inv+` pabcd`)
			cmd.Env = append(os.Environ(), "A1_TEST_SEGMENT=expanded")
			out, err := cmd.Output()
			if want := "[" + pointer(home) + "][pabcd]"; err != nil || string(out) != want {
				t.Errorf("%s with HOME=%q: command %s printed %q, %v; want %q", filepath.Base(shell), home, inv, out, err, want)
			}
		}
	}
}

// CRW-1138: the resolved command stays inside its Markdown code span (it holds no backtick), and the resolvers keep
// their rules: only backtick-anchored crw commands are resolved, with the CLI table's words; skill names, chat
// commands and plain words are not.
func TestResolvedCommandsStayInsideTheirCodeSpans(t *testing.T) {
	for _, home := range specialHomes {
		env := func(key string) (string, bool) {
			if key == "HOME" {
				return home, true
			}
			return "", false
		}
		inv, err := host.Invocation(env)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(inv, "`") {
			t.Errorf("HOME=%q: the command %s holds a backtick, which ends its code span", home, inv)
		}
		got := affordance.ResolveCRWCommands("run `crw session current`, load $crw-loop, type /crw or crw skill", env)
		if want := "run `" + inv + " relay session current`, load $crw-loop, type /crw or crw skill"; got != want {
			t.Errorf("affordance: %q, want %q", got, want)
		}
		got = hook.ResolveCRWInDirective("Run `crw pabcd orchestrate status --session x` first; crw-loop stays.", env)
		if want := "Run `" + inv + " pabcd orchestrate status --session x` first; crw-loop stays."; got != want {
			t.Errorf("directive: %q, want %q", got, want)
		}
	}
}

// CRW-1138 criterion 2: CRW_BIN stays a multi-word command prefix, trimmed and never quoted as one path.
func TestCRWBinStaysACommandPrefix(t *testing.T) {
	env := func(key string) (string, bool) {
		switch key {
		case "CRW_BIN":
			return " node \"/opt/my crw/crw.mjs\" ", true
		case "HOME":
			return "/home/b`tick", true
		}
		return "", false
	}
	if got, err := host.Invocation(env); err != nil || got != `node "/opt/my crw/crw.mjs"` {
		t.Fatalf("%q, %v", got, err)
	}
	if got := affordance.ResolveCRWCommands("`crw orchestrate`", env); got != "`node \"/opt/my crw/crw.mjs\" pabcd orchestrate`" {
		t.Errorf("affordance: %q", got)
	}
}
