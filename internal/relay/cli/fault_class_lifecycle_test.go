package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func Test23FaultClassFreshProcessParity(t *testing.T) {
	root, _ := filepath.Abs("../../..")
	binary, alias := packageBinary(t)
	python := filepath.Join(root, ".venv/bin/python")
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
					pyState := filepath.Join(t.TempDir(), "python")
					goState := filepath.Join(t.TempDir(), "go")
					env := append(os.Environ(), "HOME="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir())
					wantArgs := append([]string{"-m", "codex_session_relay.cli", "--state", pyState}, tc.args...)
					want := runParityProcess(t, env, python, wantArgs...)
					gotArgs := append(append([]string{}, program.lead...), "--state", goState)
					gotArgs = append(gotArgs, tc.args...)
					got := runParityProcess(t, env, program.path, gotArgs...)
					if got != want {
						t.Fatalf("fresh-process difference\nGo=%+v\nPython=%+v", got, want)
					}
				})
			}
		})
	}
}
