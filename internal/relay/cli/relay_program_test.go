package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

// relay_program() names the console script installed beside the relay's interpreter, else the
// interpreter running the module. The Go installation's equivalents: the codex-session-relay
// link beside crw, else crw itself followed by the relay mode. Nothing more, nothing less.
func TestRelayProgram_is_the_resolved_executable_and_relay(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if executable, err = filepath.EvalSymlinks(executable); err != nil {
		t.Fatal(err)
	}
	got := cli.RelayProgram()
	if len(got) != 2 || got[0] != executable || got[1] != "relay" {
		t.Fatalf("RelayProgram() = %q, want [%q relay]", got, executable)
	}
}

// Through the built binary, as an operator would paste it: without a link the recovery line
// starts `<crw> relay --state`, and with a codex-session-relay link beside crw it starts
// `<link> --state`, the console-script form Python renders.
func TestRelayProgram_renders_both_installation_shapes_through_the_binary(t *testing.T) {
	found := pythonScenarios(t)["test_a_closed_channel_is_queryable_rather_than_hidden"]
	bin := filepath.Join(t.TempDir(), "bin")
	crw := filepath.Join(bin, "crw")
	build := exec.Command("go", "build", "-o", crw, "./cmd/crw")
	build.Dir = repositoryRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	command := func() string {
		t.Helper()
		output, err := exec.Command(crw, "relay", "--state", found.State, "status").Output()
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		var status struct {
			Deliveries []struct {
				Recovery struct {
					Command string `json:"command"`
				} `json:"recovery"`
			} `json:"deliveries"`
		}
		if err := json.Unmarshal(output, &status); err != nil || len(status.Deliveries) != 1 {
			t.Fatalf("status %s: %v", output, err)
		}
		return status.Deliveries[0].Recovery.Command
	}
	resolved, err := filepath.EvalSymlinks(crw)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := command(), resolved+" relay --state "; !strings.HasPrefix(got, want) {
		t.Fatalf("without a link: %q does not start %q", got, want)
	}
	link := filepath.Join(bin, "codex-session-relay")
	if err := os.Symlink("crw", link); err != nil {
		t.Fatal(err)
	}
	if got, want := command(), filepath.Join(filepath.Dir(resolved), "codex-session-relay")+" --state "; !strings.HasPrefix(got, want) {
		t.Fatalf("with a link: %q does not start %q", got, want)
	}
}
