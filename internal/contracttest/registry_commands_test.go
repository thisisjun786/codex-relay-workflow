package contracttest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// registryCommands are the relay commands todo 25 part A registers (cli.py:4045-4125, :4393).
// No cli-shape fixture exercises them alone (every one also needs emit or dispositions-show),
// so their CLI shape is proved here against the built crw binary: every case of
// internal/relay/registry/testdata/cli_cases.json, replayed through `crw relay`, must print the
// exact stdout bytes and exit code the Python CLI printed (python_cli.json, from gen_cli.py).
// This test has no skip path, so it holds under CRW_CONTRACT_STRICT=1 as it does without it.
var registryCommands = []string{"register", "settings-record", "settings-show", "generation-open",
	"generation-bind", "admit-turn", "relationship-status", "relationship-resume"}

var isoStamp = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}\+00:00`)

func TestRegistryCommands_the_built_crw_prints_what_python_printed(t *testing.T) {
	replayCLICases(t, filepath.Join("internal", "relay", "registry", "testdata"), registryCommands)
}

// replayCLICases replays <dir>/cli_cases.json through the built `crw relay` and compares every
// step's stdout bytes and exit code with <dir>/python_cli.json; every command in commands must
// be exercised by some case.
func replayCLICases(t *testing.T, dir string, commands []string) {
	t.Helper()
	replayCLICasesOnly(t, dir, commands, nil)
}

// replayCLICasesOnly replays the named cases only (every case when only is nil); commands are
// asserted covered only when every case ran.
func replayCLICasesOnly(t *testing.T, dir string, commands, only []string) {
	t.Helper()
	binary, err := crwBinary()
	if err != nil {
		t.Fatal(err)
	}
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, dir)
	var cases map[string][]struct {
		Argv  []string          `json:"argv"`
		Files map[string]string `json:"files"`
		SQL   string            `json:"sql"`
	}
	var want map[string][]struct {
		Exit   int    `json:"exit"`
		Stdout string `json:"stdout"`
	}
	for file, into := range map[string]any{"cli_cases.json": &cases, "python_cli.json": &want} {
		raw, err := os.ReadFile(filepath.Join(data, file))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatal(err)
		}
	}
	covered := map[string]bool{}
	for name, steps := range cases {
		if only != nil && !slices.Contains(only, name) {
			continue
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			targetNames := map[string]string{}
			targetPattern := regexp.MustCompile(`tgt-[0-9a-f]{32}`)
			state := filepath.Join(home, "state")
			index := 0
			for _, step := range steps {
				for file, text := range step.Files {
					if err := os.WriteFile(filepath.Join(home, file), []byte(strings.ReplaceAll(text, "${HOME}", home)), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if step.SQL != "" {
					s, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), "")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := s.DB.ExecContext(context.Background(), step.SQL); err != nil {
						t.Fatal(err)
					}
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
					continue
				}
				argv := []string{"relay", "--state", state}
				for _, a := range step.Argv {
					argv = append(argv, strings.ReplaceAll(a, "${HOME}", home))
				}
				if len(step.Argv) > 0 {
					covered[step.Argv[0]] = true
				}
				command := exec.Command(binary, argv...)
				command.Dir = home
				command.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/xs", "XDG_DATA_HOME="+home+"/xd",
					"XDG_CONFIG_HOME="+home+"/xc", "CODEX_HOME="+home+"/ch", "CODEX_THREAD_BRIDGE_EXECUTION_POLICY=")
				var stdout, stderr bytes.Buffer
				command.Stdout, command.Stderr = &stdout, &stderr
				exit := 0
				if err := command.Run(); err != nil {
					exitErr, ok := err.(*exec.ExitError)
					if !ok {
						t.Fatal(err)
					}
					exit = exitErr.ExitCode()
				}
				got := strings.ReplaceAll(isoStamp.ReplaceAllString(stdout.String(), "<T>"), home, "<HOME>")
				if dir == filepath.Join("internal", "relay", "mergeturn", "testdata") {
					got = targetPattern.ReplaceAllStringFunc(got, func(value string) string {
						if name, ok := targetNames[value]; ok {
							return name
						}
						name := fmt.Sprintf("<target-key-%d>", len(targetNames)+1)
						targetNames[value] = name
						return name
					})
				}
				if exit != want[name][index].Exit || got != want[name][index].Stdout {
					t.Errorf("step %d %v: crw exit %d\n%s\npython exit %d\n%s\nstderr: %s", index, step.Argv, exit, got, want[name][index].Exit, want[name][index].Stdout, stderr.String())
				}
				index++
			}
		})
	}
	for _, command := range commands {
		if only == nil && !covered[command] {
			t.Errorf("no case exercises %s", command)
		}
	}
}
