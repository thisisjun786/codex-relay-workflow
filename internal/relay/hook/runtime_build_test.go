package hook

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestHookRuntimeBuildRecordsExecutingBinary(t *testing.T) {
	// A stamped binary behind a pointer, separate from relayExecutable in settings.
	built := testsupport.BuildCRW(t, "-ldflags=-X main.version=hook-build-proof")
	for _, payload := range []string{"not json", `{"session_id":"s","turn_id":"t"}`} {
		t.Run(payload, func(t *testing.T) {
			home := hookHome(t, hookTestBudget)
			executable := filepath.Join(home, "bin", "crw")
			if err := testsupport.CopyBinary(built, executable); err != nil {
				t.Fatal(err)
			}
			pointer := filepath.Join(home, "current")
			if err := os.Symlink(executable, pointer); err != nil {
				t.Fatal(err)
			}
			cmd := hookCommand(t, home, payload)
			cmd.Path, cmd.Args[0] = pointer, pointer
			if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
				t.Fatalf("hook: %v %s", err, out)
			}
			doctor := exec.Command(pointer, "relay", "--state", filepath.Join(home, "state"), "doctor")
			doctor.Env = hookEnv(home)
			raw, err := doctor.Output()
			if err != nil {
				t.Fatal(err)
			}
			var report map[string]any
			if err := json.Unmarshal(raw, &report); err != nil {
				t.Fatal(err)
			}
			rows := rowsAt(t, home)
			if len(rows) != 1 {
				t.Fatal(rows)
			}
			identity, ok := rows[0]["runtime"].(map[string]any)
			if !ok || identity["build"] != "hook-build-proof" || identity["build"] != report["ownership"].(map[string]any)["runtime_build"] || identity["executable"] != executable {
				t.Fatalf("row runtime %v; doctor ownership %v; executable %s", rows[0]["runtime"], report["ownership"], executable)
			}
		})
	}
}
