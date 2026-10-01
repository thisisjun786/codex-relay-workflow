package contracttest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// capacityCommands are the relay commands todo 27 part A registers (cli.py:5215-5262). Their
// cli-shape fixtures also call linkage-bind (todo 26), so their CLI shape is proved here against
// the built crw: every case of testdata/fixtures/capacity-cli-cases.json, replayed through
// `crw relay`, must print the stdout bytes and exit code its golden holds (first taken as what the
// Python CLI printed, from gen_cli.py). The cases seed their scope bindings with SQL rather than
// linkage-bind.
var capacityCommands = []string{"slot-reserve", "slot-release", "limit-declare", "usage-observe", "capacity-show"}

func TestCapacityCommands_the_built_crw_prints_what_python_printed(t *testing.T) {
	replayCLICases(t, "capacity-cli-cases.json", false, capacityCommands)
}

// regionCommands are the edit-region commands todo 27 part A registers (cli.py:5271-5387),
// proved the same way over testdata/fixtures/region-cli-cases.json. With the capacity
// test above it owns CCL-10..14 (test_coordination_cli.py): each is one or more of these cases.
var regionCommands = []string{"region-propose", "region-settle", "region-restate-revision", "region-reaffirm",
	"region-followup", "region-followup-accept", "region-followup-settle", "region-show"}

func TestRegionCommands_the_built_crw_prints_what_python_printed(t *testing.T) {
	replayCLICases(t, "region-cli-cases.json", false, regionCommands)
}

// Test27_CCL1_capacity_and_region_commands_are_registered_offline_and_not_marker_commands
// holds the shipped doctor's classification and parser dispatch of the thirteen commands to the
// golden (first taken as how the Python CLI registered and classified them).
func Test27_CCL1_capacity_and_region_commands_are_registered_offline_and_not_marker_commands(t *testing.T) {
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
	classification := map[string]map[string]bool{}
	for _, name := range append(append([]string(nil), capacityCommands...), regionCommands...) {
		command := exec.Command(binary, "relay", "--state", state, name, "--help")
		command.Env = doctor.Env
		help, err := command.CombinedOutput()
		registered := err == nil && strings.Contains(string(help), name)
		classification[name] = map[string]bool{"registered": registered, "offline": offline[name], "host": host[name], "marker": false}
	}
	if len(classification) != 13 {
		t.Fatalf("commands=%d", len(classification))
	}
	golden.CheckJSON(t, "classification", classification)
}
