package contracttest

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// TestArgumentRefusals_fall_where_the_python_fence_puts_them drives the built crw through the
// command lines the Python fence answered and holds its stdout bytes, exit code and what it left
// in its state directory to the golden (first taken as the Python fence's answer). cli.main
// opens a write form's store in _ownership_preflight before the handler reads its arguments, so
// a write form's own refusal leaves an absent store initialized (decision 30) and a store the
// other runtime owns answers with the ownership refusal. A read-only form's Services.store is
// lazy, so a handler that refuses its arguments before touching it answers that refusal, never
// store_absent, and creates nothing (decision 31): merge-turn-show's selectors and linkage-up's
// --scope.
func TestArgumentRefusals_fall_where_the_python_fence_puts_them(t *testing.T) {
	binary, err := crwBinary()
	if err != nil {
		t.Fatal(err)
	}
	type side struct {
		stdout string
		exit   int
		left   []string
	}
	run := func(t *testing.T, home, state string, argv []string) side {
		t.Helper()
		command := exec.Command(binary, append([]string{"relay", "--state", state}, argv...)...)
		command.Dir = home
		command.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/xs", "XDG_DATA_HOME="+home+"/xd",
			"XDG_CONFIG_HOME="+home+"/xc", "CODEX_HOME="+home+"/ch", "CODEX_THREAD_BRIDGE_EXECUTION_POLICY=")
		var stdout bytes.Buffer
		command.Stdout = &stdout
		exit := 0
		if err := command.Run(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatal(err)
			}
			exit = exitErr.ExitCode()
		}
		var left []string
		entries, err := os.ReadDir(state)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for _, entry := range entries {
			left = append(left, entry.Name())
		}
		return side{strings.ReplaceAll(stdout.String(), state, "<STATE>"), exit, left}
	}
	settingsRecord := []string{"settings-record", "--task", "task-alpha", "--settings", "{}", "--exception", "x", "--clear-exception"}
	for _, c := range []struct {
		name string
		argv []string
		// foreign puts crw before a store the other runtime owns (testsupport.Create).
		foreign bool
		// created is whether crw must leave a store where there was none.
		created bool
		want    string
	}{
		{name: "merge-turn-show two selectors", argv: []string{"merge-turn-show", "--parent-task", "task-alpha", "--repository", "owner/repo", "--base-ref", "dev"}, want: `"bad_invocation"`},
		{name: "merge-turn-show half a target", argv: []string{"merge-turn-show", "--repository", "owner/repo"}, want: `"bad_invocation"`},
		{name: "merge-turn-show no selector", argv: []string{"merge-turn-show"}, want: `"bad_invocation"`},
		{name: "linkage-up scope without task", argv: []string{"linkage-up", "--issue", "REL-1", "--scope", "PRJ-A"}, want: `"bad_invocation"`},
		{name: "settings-record on an absent store", argv: settingsRecord, created: true, want: `"usage"`},
		{name: "settings-record on the other runtime's store", argv: settingsRecord, foreign: true, want: `"store_owned_by_other"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			goState := filepath.Join(home, "go")
			if c.foreign {
				testsupport.Create(t, filepath.Join(goState, "relay.sqlite3"), "", "python")
			}
			goSide := run(t, home, goState, c.argv)
			if !strings.Contains(goSide.stdout, c.want) {
				t.Fatalf("crw answered without %s:\n%s", c.want, goSide.stdout)
			}
			if !c.foreign && (goSide.left != nil) != c.created {
				t.Fatalf("a store left behind: %v, expected %t", goSide.left, c.created)
			}
			golden.CheckJSON(t, "answer", map[string]any{"stdout": goSide.stdout, "exit": goSide.exit, "left": goSide.left}, golden.Substitute(home, "<HOME>"))
		})
	}
}
