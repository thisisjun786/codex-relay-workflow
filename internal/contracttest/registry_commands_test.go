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
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// registryCommands are the relay commands todo 25 part A registers (cli.py:4045-4125, :4393).
// No cli-shape fixture exercises them alone (every one also needs emit or dispositions-show),
// so their CLI shape is proved here against the built crw binary: every case of
// testdata/fixtures/registry-cli-cases.json (which internal/relay/registry's own CLI tests replay
// through Execute too), replayed through `crw relay`, must print the exact stdout bytes and exit
// code its golden holds.
var registryCommands = []string{"register", "settings-record", "settings-show", "generation-open",
	"generation-bind", "admit-turn", "relationship-status", "relationship-resume"}

// cliStep is what one step of a CLI case answered: its exit status and its stdout with the
// case's run-specific values (timestamps, the case directory, target keys) as placeholders.
type cliStep struct {
	Exit   int    `json:"exit"`
	Stdout string `json:"stdout"`
}

var isoStamp = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}\+00:00`)

func TestRegistryCommands_the_built_crw_prints_what_python_printed(t *testing.T) {
	replayCLICases(t, "registry-cli-cases.json", false, registryCommands)
}

// replayCLICases replays the cases of testdata/fixtures/<fixture> (each a sequence of seeding
// SQL, written files and `crw relay` command lines) through the built crw and holds each case's
// answers to its golden: every step's exit status and stdout, timestamps as <T>, the case
// directory as <HOME> and, with targetKeys, each merge target key as <target-key-N> in order of
// appearance. The goldens were first taken as what the Python CLI printed (python_cli.json, from
// gen_cli.py). Every command in commands must be exercised by some case.
func replayCLICases(t *testing.T, fixture string, targetKeys bool, commands []string) {
	t.Helper()
	binary, err := crwBinary()
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string][]struct {
		Argv  []string          `json:"argv"`
		Files map[string]string `json:"files"`
		SQL   string            `json:"sql"`
	}
	if err := json.Unmarshal(golden.Fixture(t, fixture), &cases); err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for name, steps := range cases {
		for _, step := range steps {
			if len(step.Argv) > 0 {
				covered[step.Argv[0]] = true
			}
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			targetNames := map[string]string{}
			targetPattern := regexp.MustCompile(`tgt-[0-9a-f]{32}`)
			state := filepath.Join(home, "state")
			var answered []cliStep
			for _, step := range steps {
				for file, text := range step.Files {
					if err := os.WriteFile(filepath.Join(home, file), []byte(strings.ReplaceAll(text, "${HOME}", home)), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if step.SQL != "" {
					// The generator's Store(path): a writer open, which initializes a store still
					// absent (decision 30); a read-only step before it is refused store_absent.
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
				if targetKeys {
					got = targetPattern.ReplaceAllStringFunc(got, func(value string) string {
						if name, ok := targetNames[value]; ok {
							return name
						}
						name := fmt.Sprintf("<target-key-%d>", len(targetNames)+1)
						targetNames[value] = name
						return name
					})
				}
				if stderr.Len() > 0 {
					t.Logf("step %d %v stderr: %s", len(answered), step.Argv, stderr.String())
				}
				answered = append(answered, cliStep{exit, got})
			}
			golden.CheckJSON(t, "steps", answered)
		})
	}
	for _, command := range commands {
		if !covered[command] {
			t.Errorf("no case exercises %s", command)
		}
	}
}
