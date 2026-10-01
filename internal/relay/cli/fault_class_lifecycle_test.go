package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func Test23FaultClassFreshProcessParity(t *testing.T) {
	binary, alias := packageBinary(t)
	observation, err := json.Marshal(map[string]any{
		"schema": "fault-observation/1", "product": "v", "faultClass": "completion_mismatch",
		"severity": "degraded", "signature": map[string]any{"x": "y"}, "occurrenceKey": "o",
		"scope": map[string]any{}, "detail": "d", "evidence": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
	}{
		{"fault-policy-seven", []string{"fault-policy", "--product", "v"}},
		{"fault-observe-product-class-refused", []string{"fault-observe", "--observation", string(observation)}},
		{"route-projects-loads-products", []string{"route-projects", "--product", "v"}},
		{"completion-check-loads-products", []string{"completion-check", "--reading", "{}"}},
		{"explicit-projects-import", []string{"--kind-module", "codex_session_relay.projects", "fault-policy", "--product", "v"}},
		{"sync-status-seven", []string{"sync-status"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, program := range []struct {
				name string
				path string
				lead []string
			}{{"codex-session-relay", alias, nil}, {"crw-relay", binary, []string{"relay"}}} {
				t.Run(program.name, func(t *testing.T) {
					goState := filepath.Join(t.TempDir(), "go")
					// A fresh store: a read-only form never creates one (cutover.md Record).
					testsupport.Create(t, filepath.Join(goState, "relay.sqlite3"), "", "go")
					env := append(os.Environ(), "HOME="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir())
					gotArgs := append(append([]string{}, program.lead...), "--state", goState)
					gotArgs = append(gotArgs, tc.args...)
					got := runParityProcess(t, env, program.path, gotArgs...)
					expectRunErr(t, "fresh process", got.code, got.out, got.err, envAnchors(env, gotArgs...)...)
				})
			}
		})
	}
}
