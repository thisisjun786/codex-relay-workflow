package contracttest

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// DSP-12 (todo 25 part B): dispositions-show is listed in doctor.actorReachability.offlineCommands.
// It lives here rather than in internal/relay/registry because doctor is internal/relay/cli, which
// imports the registry. The built crw answers doctor with an isolated HOME.
func Test25_DSP12_dispositions_show_is_listed_as_offline(t *testing.T) {
	binary, err := crwBinary()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	command := exec.Command(binary, "relay", "--state", filepath.Join(home, "state"), "doctor")
	command.Dir = home
	command.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/xs", "XDG_DATA_HOME="+home+"/xd",
		"XDG_CONFIG_HOME="+home+"/xc", "CODEX_HOME="+home+"/ch", "CODEX_THREAD_BRIDGE_EXECUTION_POLICY=")
	var stdout bytes.Buffer
	command.Stdout = &stdout
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	var answer struct {
		ActorReachability struct {
			OfflineCommands []string `json:"offlineCommands"`
		} `json:"actorReachability"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(answer.ActorReachability.OfflineCommands, "dispositions-show") {
		t.Fatal(answer.ActorReachability.OfflineCommands)
	}
}
