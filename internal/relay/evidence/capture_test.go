package evidence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

func pythonCapture(t *testing.T, id string) any {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("testdata/capture.py")
	if err != nil {
		t.Fatal(err)
	}
	out := pyoracle.Answer(t, id, func() ([]byte, error) {
		home := t.TempDir()
		cmd := exec.Command("uv", "run", "--no-sync", "python", script, id)
		cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+os.TempDir(), "PYTHONPATH="+filepath.Join(repo, "packages/codex-session-relay/src")+":"+filepath.Join(repo, "packages/codex-session-relay"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("python %s: %v\n%s", id, err, out)
		}
		return out, nil
	})
	var value any
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	if err = dec.Decode(&value); err != nil {
		t.Fatalf("decode %s: %v: %s", id, err, out)
	}
	return value
}
func whole(t *testing.T, id string, got any) {
	t.Helper()
	raw, err := json.Marshal(captureObjects(got))
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err = dec.Decode(&normalized); err != nil {
		t.Fatal(err)
	}
	want := pythonCapture(t, id)
	if !reflect.DeepEqual(normalized, want) {
		t.Fatalf("%s whole output differs\ngo=%s\npython=%s", id, Dumps(normalized, true, true, true), Dumps(want, true, true, true))
	}
}

// The semantic capture retains JSON object shape when production carries ordered records.
func captureObjects(value any) any {
	switch v := value.(type) {
	case contract.OrderedObject:
		out := map[string]any{}
		for _, f := range v {
			out[f.Key] = captureObjects(f.Value)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, item := range v {
			out[k] = captureObjects(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = captureObjects(item)
		}
		return out
	default:
		return value
	}
}
func problemRows(ps []Problem) []any {
	out := make([]any, len(ps))
	for i, p := range ps {
		out[i] = []any{p.Code, p.Detail, p.Incumbent}
	}
	return out
}
func errorRow(value any, err error) any {
	if err == nil {
		return map[string]any{"ok": value}
	}
	return map[string]any{"error": typeName(err), "reason": nil, "detail": err.Error()}
}
func typeName(err error) string {
	if _, ok := err.(*ForgeUsage); ok {
		return "ForgeUsage"
	}
	return "error"
}
