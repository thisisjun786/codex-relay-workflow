package hook_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

func TestCRW1093ActualPromptAndMemoryIngress(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, c := range []struct {
			prompt string
			allow  bool
		}{
			{"Do not remember this", false}, {"이건 기억하지 마", false}, {`"remember this"`, false}, {"~~~\nremember this\n~~~", false},
			{"<task_packet>\nremember this\n</task_packet>", false}, {"remember this", true}, {"이거 기억해 둬", true}, {"don't forget this", true}, {`remember the following: "example"`, true},
		} {
			t.Run(c.prompt, func(t *testing.T) {
				cwd := t.TempDir()
				home := filepath.Join(cwd, "codex")
				t.Setenv("HOME", cwd)
				t.Setenv("CODEX_HOME", home)
				t.Setenv("CODEX_SQLITE_HOME", home)
				if err := os.MkdirAll(home, 0700); err != nil {
					t.Fatal(err)
				}
				prompt, _ := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "cwd": cwd, "session_id": "s", "turn_id": "t1", "prompt": c.prompt})
				promptSubmitLeg(t).Handle(harness.Call{Raw: string(prompt), PabcdEnabled: enabled})
				if state.ReadState(cwd, "s").MemoryWriteRequested != c.allow {
					t.Error("ingress marker differs")
				}
				raw, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "cwd": cwd, "session_id": "s", "turn_id": "t1", "tool_name": "Write", "tool_input": map[string]any{"file_path": filepath.Join(home, "memories", "n.md")}})
				leg := worktreeLeg(t, "pre-tool-use-guarding-memory-write")
				if (leg.Handle(harness.Call{Raw: string(raw)}) == "") != c.allow {
					t.Error("ingress permission differs")
				}
				if leg.Handle(harness.Call{Raw: string(raw)}) == "" {
					t.Error("second write allowed")
				}
			})
		}
	}
}
