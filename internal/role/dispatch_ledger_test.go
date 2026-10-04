package role

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// Recorded twice from CXC v0.2.40; UUIDs and the dispatch brand are normalized,
// with global role settings (the existing store substitution). No Node runs here.
func TestDispatchLedgerOracle(t *testing.T) {
	var corpus struct {
		Cases []struct {
			Name  string
			Role  RoleName
			Steps []struct {
				Input map[string]any
				Want  any
				Error string
			}
			Ledger any
		}
	}
	check(t, json.Unmarshal(must(os.ReadFile("testdata/dispatch/oracle.json")), &corpus))
	if len(corpus.Cases) != 57 {
		t.Fatalf("recorded cases = %d", len(corpus.Cases))
	}
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			env, dir := home(t)
			ws := t.TempDir()
			writeStore(t, dir, `{"roles":{"`+string(c.Role)+`":{"mode":"model","model":"xai/grok-4.6","effort":"high","fallback":{"model":"cursor/grok-4.6","effort":null}}}}`)
			file := filepath.Join(ws, ".crw", "dispatches", "session-test", "task-test.json")
			current, first := "", ""
			ids := map[string]string{}
			for i, step := range c.Steps {
				if config, ok := step.Input["_config"]; ok {
					var patch RolePatch
					check(t, json.Unmarshal(must(Stringify(config, "")), &patch))
					_, err := SetRole(env, c.Role, patch)
					check(t, err)
					continue
				}
				if config, ok := step.Input["_rawConfig"].(map[string]any); ok {
					var d map[string]any
					check(t, json.Unmarshal(must(os.ReadFile(filepath.Join(dir, StoreFile))), &d))
					r := d["roles"].(map[string]any)[string(c.Role)].(map[string]any)
					for k, v := range config {
						r[k] = v
					}
					check(t, os.WriteFile(filepath.Join(dir, StoreFile), must(Stringify(d, "")), 0o600))
					continue
				}
				if edit, ok := step.Input["_edit"].(map[string]any); ok {
					var d map[string]any
					check(t, json.Unmarshal(must(os.ReadFile(file)), &d))
					a := d["attempts"].([]any)[0].(map[string]any)
					if edit["dropTaskFailure"] == true {
						delete(a, "taskFailure")
					}
					if edit["badTaskFailure"] == true {
						a["taskFailure"] = map[string]any{"kind": "timeout", "evidence": "x"}
					}
					if v, ok := edit["dispatchStatus"]; ok {
						d["status"] = v
					}
					if v, ok := edit["attemptStatus"]; ok {
						a["status"] = v
					}
					if edit["extras"] == true {
						d["foreign"] = map[string]any{"nested": []any{1, "retained"}}
						a["foreign"] = "attempt-extra"
						a["candidate"].(map[string]any)["foreign"] = "candidate-extra"
						d["candidates"].([]any)[0].(map[string]any)["foreign"] = "root-candidate-extra"
					}
					check(t, os.WriteFile(file, must(Stringify(d, "")), 0o600))
					continue
				}
				input := map[string]any{"sessionId": "session-test", "dispatchId": "task-test"}
				for k, v := range step.Input {
					if v == "$current" {
						v = current
					}
					if v == "$first" {
						v = first
					}
					input[k] = v
				}
				before, _ := os.ReadFile(file)
				got, err := RunDispatch(ws, input, env)
				if step.Error != "" {
					if err == nil || err.Error() != step.Error {
						t.Fatalf("step %d: error %v, want %q", i, err, step.Error)
					}
					if after, _ := os.ReadFile(file); string(after) != string(before) {
						t.Fatalf("step %d: refused report changed ledger", i)
					}
					continue
				}
				check(t, err)
				current = got.AttemptID
				if first == "" {
					first = current
				}
				var value any
				check(t, json.Unmarshal(must(Stringify(got, "")), &value))
				value = dispatchTestNormalize(value, ids)
				if !reflect.DeepEqual(value, step.Want) {
					t.Fatalf("step %d:\ngot %s\nwant %s", i, must(Stringify(value, "")), must(Stringify(step.Want, "")))
				}
			}
			var ledger any
			check(t, json.Unmarshal(must(os.ReadFile(file)), &ledger))
			if !reflect.DeepEqual(dispatchTestNormalize(ledger, ids), c.Ledger) {
				t.Fatalf("final ledger differs: %s", must(Stringify(ledger, "")))
			}
		})
	}
}

func dispatchTestNormalize(v any, ids map[string]string) any {
	switch x := v.(type) {
	case string:
		re := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`)
		return re.ReplaceAllStringFunc(x, func(id string) string {
			if ids[id] == "" {
				ids[id] = "ATTEMPT_" + string(rune('1'+len(ids)))
			}
			return ids[id]
		})
	case []any:
		for i, e := range x {
			x[i] = dispatchTestNormalize(e, ids)
		}
	case map[string]any: // IDs before marker references, matching oracle result property order.
		if a, ok := x["attemptId"]; ok {
			x["attemptId"] = dispatchTestNormalize(a, ids)
		}
		if a, ok := x["id"]; ok {
			x["id"] = dispatchTestNormalize(a, ids)
		}
		for k, e := range x {
			x[k] = dispatchTestNormalize(e, ids)
		}
	}
	return v
}

func dispatchTestCall(t *testing.T, ws string, env host.LookupEnv, fields map[string]any) DispatchResult {
	t.Helper()
	b := map[string]any{"sessionId": "session-test", "dispatchId": "task-test"}
	for k, v := range fields {
		b[k] = v
	}
	r, err := RunDispatch(ws, b, env)
	check(t, err)
	return r
}

// Used by the boundary cases below, alongside independently recorded sequences.
func dispatchTestError(t *testing.T, ws string, env host.LookupEnv, b map[string]any, want string) {
	t.Helper()
	_, err := RunDispatch(ws, b, env)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error %v, want %q", err, want)
	}
}
