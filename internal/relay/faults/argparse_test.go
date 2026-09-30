package faults

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type cliResult struct {
	code           int
	stdout, stderr string
}

// pythonFaultCLI is what live Python answered to a relay command line in a disposable home: exit
// status, both streams, and whether the --state directory existed after it ran (created).
func pythonFaultCLI(t *testing.T, root, home string, args ...string) (answer cliResult, created bool) {
	t.Helper()
	state := ""
	for i, arg := range args {
		if arg == "--state" && i+1 < len(args) {
			state = args[i+1]
		}
	}
	var run pyRun
	pyValue(t, "relay "+strings.Join(args, " "), args, pyRunPaths(t, home), &run, func() (any, error) {
		cmd := exec.Command("uv", append([]string{"run", "--no-sync", "codex-session-relay"}, args...)...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm")
		answer, err := runPython(cmd, true)
		if err != nil {
			return nil, err
		}
		if state != "" {
			answer.Created = !stateAbsent(t, state)
		}
		return answer, nil
	})
	return cliResult{run.Code, run.Stdout, run.Stderr}, run.Created
}

func goFaultCLI(t *testing.T, args ...string) cliResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, handled := executeAsCLI(context.Background(), args, &stdout, &stderr)
	if !handled {
		t.Fatalf("not handled: %v", args)
	}
	return cliResult{code, stdout.String(), stderr.String()}
}

func TestFaultArgparseSurfaceMatchesPython(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range faultArgCommands {
		command := command
		t.Run(command.name, func(t *testing.T) {
			home := t.TempDir()
			// Each runtime keeps its own store: neither writes a store the other owns. A case
			// Python answers without creating its store is asked of Go about the same absent
			// path (oracleState).
			pyState, goState := filepath.Join(home, "relay"), filepath.Join(home, "go", "relay")
			cases := []struct {
				name string
				args []string
			}{
				{"help", []string{"--help"}},
				{"no-args", nil},
				{"unknown", []string{"--definitely-unknown"}},
			}
			if len(command.options) == 0 {
				cases = append(cases, struct {
					name string
					args []string
				}{"abbreviation", []string{"--he"}})
			} else if !command.options[0].required {
				o := command.options[0]
				prefix := "--" + o.name
				for n := 1; n <= len(o.name); n++ {
					candidate := o.name[:n]
					matches := 0
					for _, other := range command.options {
						if strings.HasPrefix(other.name, candidate) {
							matches++
						}
					}
					if matches == 1 {
						prefix = "--" + candidate
						break
					}
				}
				args := []string{prefix}
				if !o.boolean {
					args = append(args, "1")
				}
				cases = append(cases, struct {
					name string
					args []string
				}{"abbreviation", args})
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					args := append([]string{"--json", command.name}, tc.args...)
					want, created := pythonFaultCLI(t, root, home, append([]string{"--state", pyState}, args...)...)
					state := oracleState(created, pyState, goState)
					got := goFaultCLI(t, append([]string{"--state", state}, args...)...)
					neverCreated(t, pyState, state)
					if got != want {
						t.Fatalf("args %v\nPython: code=%d stdout=%q stderr=%q\nGo: code=%d stdout=%q stderr=%q", args, want.code, want.stdout, want.stderr, got.code, got.stdout, got.stderr)
					}
				})
			}
		})
	}
}

func TestFaultArgparseAmbiguousPrefixMatchesPython(t *testing.T) {
	root, _ := filepath.Abs("../../..")
	home := t.TempDir()
	args := []string{"--state", filepath.Join(home, "relay"), "fault-policy", "--f", "x"}
	want, _ := pythonFaultCLI(t, root, home, args...)
	if got := goFaultCLI(t, args...); got != want {
		t.Fatalf("Python: %#v\nGo: %#v", want, got)
	}
}

func TestFaultObserveMalformedJSONMatchesPython(t *testing.T) {
	root, _ := filepath.Abs("../../..")
	for _, raw := range []string{"{not json", "[1,2"} {
		t.Run(raw, func(t *testing.T) {
			home := t.TempDir()
			// Each runtime keeps its own store: neither writes a store the other owns.
			args := []string{"--json", "fault-observe", "--observation", raw}
			want, _ := pythonFaultCLI(t, root, home, append([]string{"--state", filepath.Join(home, "relay")}, args...)...)
			if got := goFaultCLI(t, append([]string{"--state", filepath.Join(home, "go", "relay")}, args...)...); got != want {
				t.Fatalf("Python: %#v\nGo: %#v", want, got)
			}
		})
	}
}

func TestFaultKindModuleNestedImportErrorMatchesPython(t *testing.T) {
	root, _ := filepath.Abs("../../..")
	home := t.TempDir()
	args := []string{"--state", filepath.Join(home, "relay"), "--json", "--kind-module", "codex_session_relay.not_real", "fault-attention"}
	want, _ := pythonFaultCLI(t, root, home, args...)
	if got := goFaultCLI(t, args...); got != want {
		t.Fatalf("Python: %#v\nGo: %#v", want, got)
	}
}
