package hook_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

func TestCRW1093CorrectionActualIngress(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, tc := range []struct {
			prompt string
			allow  bool
		}{
			{"Example: \"\nremember this\n\"", false},
			{"Example: `\nremember this\n`", false},
			{"Example: ``\nremember this\n``", false},
			{"Remember this,\nbut do not save it to memory.", false},
			{"Remember this,\nbut do not save it.", false},
			{"Remember this but do not store it", false},
			{"Remember this but do not write to memory", false},
			{"Do not\nremember this", false},
			{"이거 기억해 둬,\n하지만 저장하지 마", false},
			{"Remember this preference: do not write tests for documentation-only changes.", true},
			{"Remember this preference: don't save temporary build files.", true},
			{"Please remember\nthis preference: use Go.", true},
			{"note this\ndown", true},
			{"어제 대화 내용 기억해?", false},
			{"기억해?", false},
			{"기억해", true},
			{"기억해. 다음에도 써", true},
			{"remember the following:\n\"example\"", true},
		} {
			t.Run(tc.prompt, func(t *testing.T) {
				cwd := t.TempDir()
				home := filepath.Join(cwd, "codex")
				t.Setenv("HOME", cwd)
				t.Setenv("CODEX_HOME", home)
				t.Setenv("CODEX_SQLITE_HOME", home)
				if err := os.MkdirAll(home, 0700); err != nil {
					t.Fatal(err)
				}
				prompt, _ := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "cwd": cwd, "session_id": "s", "turn_id": "t1", "prompt": tc.prompt})
				promptSubmitLeg(t).Handle(harness.Call{Raw: string(prompt), PabcdEnabled: enabled})
				if state.ReadState(cwd, "s").MemoryWriteRequested != tc.allow {
					t.Error("incorrect prompt marker")
				}
				raw, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "cwd": cwd, "session_id": "s", "turn_id": "t1", "tool_name": "Write", "tool_input": map[string]any{"file_path": filepath.Join(home, "memories", "n.md")}})
				leg := worktreeLeg(t, "pre-tool-use-guarding-memory-write")
				if (leg.Handle(harness.Call{Raw: string(raw)}) == "") != tc.allow {
					t.Error("incorrect protected-write permission")
				}
				if leg.Handle(harness.Call{Raw: string(raw)}) == "" {
					t.Error("second write allowed")
				}
			})
		}
	}
}
