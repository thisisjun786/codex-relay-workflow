package faults

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
)

type argOption struct {
	name              string
	required, boolean bool
}
type argCommand struct {
	name    string
	options []argOption
}

// faultArgCommands lists each fault command's options for the surface tests, read from the same
// argparse spec as parsing and formatting rather than a second table.
var faultArgCommands = func() []argCommand {
	var commands []argCommand
	for _, name := range Names() {
		c := argCommand{name: name}
		for _, a := range argparse.Specs[name].Actions {
			if a.Kind == "_HelpAction" {
				continue
			}
			c.options = append(c.options, argOption{strings.TrimPrefix(a.Flags[len(a.Flags)-1], "--"), a.Required, a.Kind == "_StoreTrueAction" || a.Kind == "_StoreFalseAction"})
		}
		commands = append(commands, c)
	}
	return commands
}()

type cliResult struct {
	code           int
	stdout, stderr string
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
	goldenParent(t)
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
					args := append([]string{"--json", command.name}, tc.args...)
					got := goFaultCLI(t, append([]string{"--state", state}, args...)...)
					checkGolden(t, "relay "+strings.Join(args, " "), args, runPathsOf(t, home), cliGolden{Code: got.code, Stdout: got.stdout, Stderr: got.stderr, Created: created(t, state)})
				})
			}
		})
	}
}

func TestFaultArgparseAmbiguousPrefixMatchesPython(t *testing.T) {
	home := t.TempDir()
	args := []string{"--state", filepath.Join(home, "relay"), "fault-policy", "--f", "x"}
	got := goFaultCLI(t, args...)
	checkGolden(t, "relay "+strings.Join(args[2:], " "), args[2:], runPathsOf(t, home), cliGolden{Code: got.code, Stdout: got.stdout, Stderr: got.stderr})
}

func TestFaultObserveMalformedJSONMatchesPython(t *testing.T) {
	goldenParent(t)
	for _, raw := range []string{"{not json", "[1,2"} {
		t.Run(raw, func(t *testing.T) {
			home := t.TempDir()
			args := []string{"--json", "fault-observe", "--observation", raw}
			got := goFaultCLI(t, append([]string{"--state", filepath.Join(home, "go", "relay")}, args...)...)
			checkGolden(t, "relay "+strings.Join(args, " "), args, runPathsOf(t, home), cliGolden{Code: got.code, Stdout: got.stdout, Stderr: got.stderr})
		})
	}
}

// A dotted name under the relay's own package that is not codex_session_relay.projects is refused
// like any other unknown kind module: exit 4, naming the value given.
func TestAnUnknownKindModuleUnderTheRelayPackageIsRefused(t *testing.T) {
	home := t.TempDir()
	args := []string{"--state", filepath.Join(home, "relay"), "--json", "--kind-module", "codex_session_relay.not_real", "fault-attention"}
	got := goFaultCLI(t, args...)
	checkGolden(t, "relay "+strings.Join(args[2:], " "), args[2:], runPathsOf(t, home), cliGolden{Code: got.code, Stdout: got.stdout, Stderr: got.stderr})
}
