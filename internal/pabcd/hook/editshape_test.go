package hook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
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

func TestEditShapesRecordedOracle(t *testing.T) {
	var corpus struct {
		Normalization []struct{ Input, Want string }
		Patches       []struct {
			Name, Input string
			Want        []struct {
				File        string
				DigestParts []string
			}
		}
		Advisory string
	}
	data, err := os.ReadFile("testdata/render-oracle.json")
	if err != nil || json.Unmarshal(data, &corpus) != nil {
		t.Fatal("cannot read recorded oracle", err)
	}
	for _, row := range corpus.Normalization {
		if got := hook.NormalizeEditLine(row.Input); got != row.Want {
			t.Errorf("normalize(%q)=%q want%q", row.Input, got, row.Want)
		}
	}
	for _, row := range corpus.Patches {
		t.Run(row.Name, func(t *testing.T) {
			want := make([]hook.FileEditShape, len(row.Want))
			for i, recorded := range row.Want {
				want[i] = hook.FileEditShape{File: recorded.File, Key: strings.Join(recorded.DigestParts, "")}
			}
			if got := hook.FileEditShapes(row.Input); !reflect.DeepEqual(got, want) {
				t.Fatalf("shape=%v want%v", got, want)
			}
		})
	}
	if got := hook.RenderGroundingAdvisory(); got != corpus.Advisory {
		t.Fatalf("advisory=%q want%q", got, corpus.Advisory)
	}
}

func TestRenderLegGatingAndArtifact(t *testing.T) {
	for _, test := range []struct {
		name, tool, enabled string
		subagent            bool
		want                int
	}{{"patch", "apply_patch", "on", false, 1}, {"other", "Bash", "on", false, 0}, {"disabled", "view_image", "off", false, 0}, {"subagent", "view_image", "on", true, 0}, {"malformed", "", "on", false, 0}} {
		t.Run(test.name, func(t *testing.T) {
			cwd, home := t.TempDir(), t.TempDir()
			p := map[string]any{"hook_event_name": "PostToolUse", "session_id": "s1", "cwd": cwd, "tool_name": test.tool, "tool_input": map[string]any{"command": "*** Add File: Page.tsx\n+hello\n*** End Patch"}}
			if test.subagent {
				p["agent_id"], p["agent_type"] = "a1", "worker"
			}
			raw, _ := json.Marshal(p)
			if test.name == "malformed" {
				raw = []byte("{bad")
			}
			env := func(key string) (string, bool) {
				switch key {
				case "CRW_PABCD":
					return test.enabled, true
				case "CODEX_HOME":
					return home, true
				}
				return "", false
			}
			var out, stderr bytes.Buffer
			if code := harness.Hook(context.Background(), []string{"post-tool-use", "--leg", "post-tool-use-tracking-render-observations"}, strings.NewReader(string(raw)), &out, &stderr, env, harness.Legs()); code != 0 || out.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("hook=%d %q %q", code, out.String(), stderr.String())
			}
			if rows := hook.ReadRenderObsRows(cwd); len(rows) != test.want || (len(rows) == 1 && rows[0].Kind != hook.ArtifactModified) {
				t.Fatalf("rows=%v want%d", rows, test.want)
			}
		})
	}
}
