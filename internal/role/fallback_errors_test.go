package role

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

func fallbackFixture(t *testing.T, name string, target any) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fallback", name))
	if err != nil {
		t.Fatal(err)
	}
	if target != nil {
		if err := json.Unmarshal(b, target); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func fallbackTestEnv(t *testing.T) (map[string]string, host.LookupEnv) {
	t.Helper()
	root := t.TempDir()
	m := map[string]string{"HOME": filepath.Join(root, "home"), "CODEX_HOME": filepath.Join(root, "codex"), "CRW_HOME": filepath.Join(root, "global"), "CODEX_MODELS_CACHE_PATH": filepath.Join(root, "absent.json")}
	for _, k := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		if err := os.MkdirAll(m[k], 0700); err != nil {
			t.Fatal(err)
		}
	}
	return m, func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestFallbackFailureOracleFiveGroups(t *testing.T) {
	var cases []struct {
		Group, Name string
		Input       json.RawMessage
		Want        FailureDecision
	}
	fallbackFixture(t, "errors.json", &cases)
	groups := map[string]bool{}
	for _, c := range cases {
		groups[c.Group] = true
		t.Run(c.Name, func(t *testing.T) {
			var input any
			d := json.NewDecoder(bytes.NewReader(c.Input))
			d.UseNumber()
			if err := d.Decode(&input); err != nil {
				t.Fatal(err)
			}
			if got := decodeDispatchFailure(input); !reflect.DeepEqual(got, c.Want) {
				t.Fatalf("got %+v, want %+v", got, c.Want)
			}
		})
	}
	if len(groups) != 5 || len(cases) != 60 {
		t.Fatalf("oracle groups=%d cases=%d", len(groups), len(cases))
	}
}

func TestFallbackFailureJSONAndEnvelopePrecedence(t *testing.T) {
	for _, s := range []string{`{"code":"unsupported_model","unused":1e400}`, "\uFEFFunsupported_model\uFEFF", `{"error":{"last_error":{"response":{"error":"unsupported_model"}}}}`} {
		got := decodeDispatchFailure(s)
		if got.Code == nil || *got.Code != "unsupported_model" || got.Action != "next" {
			t.Fatalf("%q: %+v", s, got)
		}
	}
	for _, v := range []any{nil, map[string]any{"error": nil, "last_error": "insufficient_quota"}, `{"response":[{"error":"insufficient_quota"}]}`, `{"error":"insufficient_quota"} trailing`} {
		if got := decodeDispatchFailure(v); got.Code != nil || got.Action != "unknown" {
			t.Fatalf("%v: %+v", v, got)
		}
	}
}
