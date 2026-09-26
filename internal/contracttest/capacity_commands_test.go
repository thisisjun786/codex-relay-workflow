package contracttest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// capacityCommands are the relay commands todo 27 part A registers (cli.py:5215-5262). Their
// cli-shape fixtures also call linkage-bind (todo 26), so their CLI shape is proved here against
// the built crw: every case of internal/relay/capacity/testdata/cli_cases.json, replayed through
// `crw relay`, must print the stdout bytes and exit code the Python CLI printed (python_cli.json,
// from gen_cli.py). The cases seed their scope bindings with SQL rather than linkage-bind. No skip
// path, so it holds under CRW_CONTRACT_STRICT=1 as it does without it.
var capacityCommands = []string{"slot-reserve", "slot-release", "limit-declare", "usage-observe", "capacity-show"}

func TestCapacityCommands_the_built_crw_prints_what_python_printed(t *testing.T) {
	replayTodo27Cases(t, filepath.Join("internal", "relay", "capacity", "testdata"), "cli_cases.json", "python_cli.json", capacityCommands)
}

// replayTodo27Cases replays one gen_cli.py case file through the built crw and compares each
// step's stdout (timestamps as <T>, the case directory as <HOME>) and exit code with Python's.
func replayTodo27Cases(t *testing.T, dir, casesFile, pythonFile string, commands []string) {
	t.Helper()
	binary, err := crwBinary()
	if err != nil {
		t.Fatal(err)
	}
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string][]struct {
		Argv []string `json:"argv"`
		SQL  string   `json:"sql"`
	}
	var want map[string][]struct {
		Exit   int    `json:"exit"`
		Stdout string `json:"stdout"`
	}
	for file, into := range map[string]any{casesFile: &cases, pythonFile: &want} {
		raw, err := os.ReadFile(filepath.Join(root, dir, file))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatal(err)
		}
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
			state := filepath.Join(home, "state")
			index := 0
			for _, step := range steps {
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
				command := exec.Command(binary, append([]string{"relay", "--state", state}, step.Argv...)...)
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
				if exit != want[name][index].Exit || got != want[name][index].Stdout {
					t.Errorf("step %d %v: crw exit %d\n%s\npython exit %d\n%s\nstderr: %s", index, step.Argv, exit, got, want[name][index].Exit, want[name][index].Stdout, stderr.String())
				}
				index++
			}
		})
	}
	for _, command := range commands {
		if !covered[command] {
			t.Errorf("no case exercises %s", command)
		}
	}
}

// regionCommands are the edit-region commands todo 27 part A registers (cli.py:5271-5387),
// proved the same way against region_cli_cases.json / python_region_cli.json.
var regionCommands = []string{"region-propose", "region-settle", "region-restate-revision", "region-reaffirm",
	"region-followup", "region-followup-accept", "region-followup-settle", "region-show"}

func TestRegionCommands_the_built_crw_prints_what_python_printed(t *testing.T) {
	replayTodo27Cases(t, filepath.Join("internal", "relay", "capacity", "testdata"), "region_cli_cases.json", "python_region_cli.json", regionCommands)
}

// CCL-10..14 include the original cli-shape corpus scenarios. These tests additionally
// run the Python-output replays without relying on the linkage CLI's availability.
func Test27_CCL10_SlotReservedReleasedAndReported(t *testing.T) {
	replayTodo27Cases(t, filepath.Join("internal", "relay", "capacity", "testdata"), "cli_cases.json", "python_cli.json", capacityCommands)
}
func Test27_CCL11_DeclaredBoundProvenance(t *testing.T) {
	replayTodo27Cases(t, filepath.Join("internal", "relay", "capacity", "testdata"), "cli_cases.json", "python_cli.json", capacityCommands)
}
func Test27_CCL12_RegionProposedSettledShown(t *testing.T) {
	replayTodo27Cases(t, filepath.Join("internal", "relay", "capacity", "testdata"), "region_cli_cases.json", "python_region_cli.json", regionCommands)
}
func Test27_CCL13_UnassignedFollowupRefused(t *testing.T) {
	replayTodo27Cases(t, filepath.Join("internal", "relay", "capacity", "testdata"), "region_cli_cases.json", "python_region_cli.json", regionCommands)
}
func Test27_CCL14_LateAcceptanceStaleAndCarriedTerms(t *testing.T) {
	replayTodo27Cases(t, filepath.Join("internal", "relay", "capacity", "testdata"), "region_cli_cases.json", "python_region_cli.json", regionCommands)
}

// Test27_CCL1_capacity_and_region_commands_are_registered_offline_and_not_marker_commands
// compares the shipped doctor's classification and parser dispatch with live Python.
func Test27_CCL1_capacity_and_region_commands_are_registered_offline_and_not_marker_commands(t *testing.T) {
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	python := exec.Command("uv", "run", "--no-sync", "python", "-c", `import json
from codex_session_relay.cli import OFFLINE_COMMANDS,HOST_REQUIRED_COMMANDS,MARKER_COMMANDS_BY_NAME,build_parser
names=['slot-reserve','slot-release','limit-declare','usage-observe','capacity-show','region-propose','region-settle','region-restate-revision','region-reaffirm','region-followup','region-followup-accept','region-followup-settle','region-show']
parser=build_parser()
print(json.dumps({n:{'registered':n in parser._subparsers._group_actions[0].choices,'offline':n in OFFLINE_COMMANDS,'host':n in HOST_REQUIRED_COMMANDS,'marker':n in MARKER_COMMANDS_BY_NAME} for n in names}))`)
	python.Dir = root
	python.Env = append(os.Environ(), "HOME="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir(), "CODEX_HOME="+t.TempDir())
	raw, err := python.CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v %s", err, raw)
	}
	var want map[string]map[string]bool
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	binary, err := crwBinary()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	state := filepath.Join(home, "absent")
	doctor := exec.Command(binary, "relay", "--state", state, "doctor")
	doctor.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home, "XDG_CONFIG_HOME="+home, "CODEX_HOME="+home)
	output, err := doctor.Output()
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatal(err)
	}
	reach := report["actorReachability"].(map[string]any)
	offline := map[string]bool{}
	for _, v := range reach["offlineCommands"].([]any) {
		offline[v.(string)] = true
	}
	host := map[string]bool{}
	for _, v := range reach["hostRequiredCommands"].([]any) {
		host[v.(string)] = true
	}
	if len(want) != 13 {
		t.Fatalf("Python commands=%d", len(want))
	}
	for name, expected := range want {
		command := exec.Command(binary, "relay", "--state", state, name, "--help")
		command.Env = doctor.Env
		help, err := command.CombinedOutput()
		registered := err == nil && strings.Contains(string(help), name)
		got := map[string]bool{"registered": registered, "offline": offline[name], "host": host[name], "marker": false}
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("%s Go=%v Python=%v help=%s", name, got, expected, help)
		}
	}
}
