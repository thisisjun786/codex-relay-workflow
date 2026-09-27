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

func pythonFaultCLI(t *testing.T, root, home string, args ...string) cliResult {
	t.Helper()
	cmd := exec.Command("uv", append([]string{"run", "--no-sync", "codex-session-relay"}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	return cliResult{code, stdout.String(), stderr.String()}
}

func goFaultCLI(t *testing.T, args ...string) cliResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, handled := ExecuteAs(context.Background(), "codex-session-relay", args, &stdout, &stderr, nil)
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
			state := filepath.Join(home, "relay")
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
					args := append([]string{"--state", state, "--json", command.name}, tc.args...)
					want := pythonFaultCLI(t, root, home, args...)
					got := goFaultCLI(t, args...)
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
	if want, got := pythonFaultCLI(t, root, home, args...), goFaultCLI(t, args...); got != want {
		t.Fatalf("Python: %#v\nGo: %#v", want, got)
	}
}

func TestFaultObserveMalformedJSONMatchesPython(t *testing.T) {
	root, _ := filepath.Abs("../../..")
	for _, raw := range []string{"{not json", "[1,2"} {
		t.Run(raw, func(t *testing.T) {
			home := t.TempDir()
			args := []string{"--state", filepath.Join(home, "relay"), "--json", "fault-observe", "--observation", raw}
			if want, got := pythonFaultCLI(t, root, home, args...), goFaultCLI(t, args...); got != want {
				t.Fatalf("Python: %#v\nGo: %#v", want, got)
			}
		})
	}
}

func TestFaultKindModuleNestedImportErrorMatchesPython(t *testing.T) {
	root, _ := filepath.Abs("../../..")
	home := t.TempDir()
	args := []string{"--state", filepath.Join(home, "relay"), "--json", "--kind-module", "codex_session_relay.not_real", "fault-attention"}
	if want, got := pythonFaultCLI(t, root, home, args...), goFaultCLI(t, args...); got != want {
		t.Fatalf("Python: %#v\nGo: %#v", want, got)
	}
}
