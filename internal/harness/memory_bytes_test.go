package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Exercise the real recovering guard: a reader panic must not silently allow a
// protected write after an exec bytes literal containing invalid UTF-8.
func TestMemoryGuardDecodedBytes(t *testing.T) {
	for _, method := range []string{`write_text("x")`, `touch()`, `mkdir()`} {
		t.Run(method, func(t *testing.T) {
			env, home, _ := hookEnv(t)
			t.Setenv("CODEX_HOME", home)
			cwd := t.TempDir()
			root := filepath.Join(home, "memories")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			program := "from pathlib import Path\ntry: exec(b\"print(1)\\n#\\xff\")\nexcept SyntaxError: pass\nPath(" + `"` + filepath.Join(root, "n") + `").` + method
			raw, err := json.Marshal(map[string]any{
				"hook_event_name": "PreToolUse", "session_id": "s1", "cwd": cwd,
				"tool_name": "Bash", "tool_input": map[string]string{"command": "python3 -c '" + program + "'"},
			})
			if err != nil {
				t.Fatal(err)
			}
			run := func(deny bool) {
				t.Helper()
				code, out, stderr := hook(Legs(), []string{"pre-tool-use", "--leg", "pre-tool-use-guarding-memory-write"}, string(raw), env)
				if code != 0 || stderr != "" {
					t.Fatalf("hook: %d %q %q", code, out, stderr)
				}
				if deny {
					var response struct {
						Output struct {
							Decision string `json:"permissionDecision"`
							Reason   string `json:"permissionDecisionReason"`
						} `json:"hookSpecificOutput"`
					}
					if json.Unmarshal([]byte(out), &response) != nil || response.Output.Decision != "deny" || !strings.Contains(response.Output.Reason, "MEMORY-WRITE-GATE") {
						t.Fatalf("ungranted write was allowed: %q", out)
					}
				} else if out != "" {
					t.Fatalf("granted write was refused: %q", out)
				}
			}
			run(true)
			s := state.DefaultState("s1", "")
			s.MemoryWriteGrant = true
			if err := state.WriteState(cwd, s); err != nil {
				t.Fatal(err)
			}
			run(false)
			if state.ReadState(cwd, "s1").MemoryWriteGrant {
				t.Fatal("granted write did not spend its grant")
			}
			run(true)
		})
	}
}
