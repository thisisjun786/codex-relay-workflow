package hook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

func TestRenderLegRecordsObservation(t *testing.T) {
	cwd, home := t.TempDir(), t.TempDir()
	raw, err := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "session_id": "s1", "cwd": cwd, "tool_name": "view_image", "tool_input": map[string]any{}, "tool_response": "ok"})
	if err != nil {
		t.Fatal(err)
	}
	env := func(key string) (string, bool) {
		if key == "CRW_PABCD" {
			return "on", true
		}
		if key == "CODEX_HOME" {
			return home, true
		}
		return "", false
	}
	var out, stderr bytes.Buffer
	code := harness.Hook(context.Background(), []string{"post-tool-use", "--leg", "post-tool-use-tracking-render-observations"}, strings.NewReader(string(raw)), &out, &stderr, env, harness.Legs())
	if code != 0 || out.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("hook = %d %q %q", code, out.String(), stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(cwd, ".crw", "render-observations.jsonl"))
	if err != nil || !bytes.Contains(data, []byte(`"kind":"observation"`)) {
		t.Fatalf("render hook did not record observation: bytes=%q err=%v", data, err)
	}
}
