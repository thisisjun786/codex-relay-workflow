package role

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

func dispatchTestFixture(t *testing.T) (string, host.LookupEnv, DispatchResult, string) {
	t.Helper()
	ws := t.TempDir()
	env, _ := home(t)
	r := dispatchTestCall(t, ws, env, map[string]any{"action": "start", "role": "executor"})
	return ws, env, r, filepath.Join(ws, ".crw", "dispatches", "session-test", "task-test.json")
}

func dispatchTestSafetyError(t *testing.T, id string) string {
	t.Helper()
	var corpus struct {
		Cases []struct {
			ID, Classification, OracleAction, GoErrorContains string
			OracleWroteOutsideWorkspace                       bool
		}
	}
	check(t, json.Unmarshal(must(os.ReadFile("testdata/dispatch/safety.json")), &corpus))
	for _, c := range corpus.Cases {
		if c.ID == id {
			if c.Classification != "intentionally-changed" || c.OracleAction != "ready" || !c.OracleWroteOutsideWorkspace {
				t.Fatal("missing independent unsafe oracle outcome")
			}
			return c.GoErrorContains
		}
	}
	t.Fatalf("missing safety case %q", id)
	return ""
}

func TestDispatchLedgerPublishFailure(t *testing.T) {
	ws, env, r, file := dispatchTestFixture(t)
	before := must(os.ReadFile(file))
	refused := errors.New("injected publication failure")
	calls := 0
	_, err := dispatchRun(ws, map[string]any{"sessionId": "session-test", "dispatchId": "task-test", "action": "claim", "attemptId": r.AttemptID}, env, func(tmp, final string) error {
		calls++
		if final != file || must(os.Stat(tmp)).Mode().Perm() != 0o600 {
			t.Fatal("wrong publication target or temporary mode")
		}
		if !must(os.Stat(file + ".lock")).IsDir() {
			t.Fatal("rename ran without the owned lock")
		}
		var state map[string]any
		check(t, json.Unmarshal(must(os.ReadFile(tmp)), &state))
		if state["attempts"].([]any)[0].(map[string]any)["claimed"] != true {
			t.Fatal("failure seam did not receive the next state")
		}
		return refused
	})
	if !errors.Is(err, refused) || calls != 1 {
		t.Fatalf("rename calls %d; error %v", calls, err)
	}
	if string(must(os.ReadFile(file))) != string(before) {
		t.Fatal("failed publication changed the previous record")
	}
	if paths := must(filepath.Glob(file + ".*.tmp")); len(paths) != 0 {
		t.Fatalf("temporary files remain: %v", paths)
	}
	if _, err := os.Lstat(file + ".lock"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("owned lock remains: %v", err)
	}
	if dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": r.AttemptID}).Action != "spawn" {
		t.Fatal("publication failure consumed claim")
	}
}

func TestDispatchLedgerLinksAndHeldLock(t *testing.T) {
	for _, rel := range []string{".crw", ".crw/dispatches", ".crw/dispatches/session-test"} {
		t.Run(rel, func(t *testing.T) {
			ws, outside := t.TempDir(), t.TempDir()
			env, _ := home(t)
			check(t, os.WriteFile(filepath.Join(outside, "sentinel"), []byte("untouched"), 0o600))
			link := filepath.Join(ws, rel)
			check(t, os.MkdirAll(filepath.Dir(link), 0o700))
			check(t, os.Symlink(outside, link))
			want := "symlink"
			if rel == ".crw" {
				want = dispatchTestSafetyError(t, "top-directory-link")
			}
			dispatchTestError(t, ws, env, map[string]any{"action": "start", "role": "executor", "sessionId": "session-test", "dispatchId": "task-test"}, want)
			if got := must(os.ReadDir(outside)); len(got) != 1 || got[0].Name() != "sentinel" {
				t.Fatal("linked external directory changed")
			}
		})
	}
	for _, dangling := range []bool{false, true} {
		t.Run("state link", func(t *testing.T) {
			ws, env, _, file := dispatchTestFixture(t)
			target := filepath.Join(t.TempDir(), "record")
			check(t, os.Rename(file, target))
			before := must(os.ReadFile(target))
			link := target
			if dangling {
				link += "-missing"
			}
			check(t, os.Symlink(link, file))
			for _, action := range []string{"start", "status"} {
				dispatchTestError(t, ws, env, map[string]any{"action": action, "role": "executor", "sessionId": "session-test", "dispatchId": "task-test"}, "symlink")
			}
			if string(must(os.ReadFile(target))) != string(before) {
				t.Fatal("linked record changed")
			}
		})
	}
	ws, env, r, file := dispatchTestFixture(t)
	check(t, os.Mkdir(file+".lock", 0o700))
	_, err := RunDispatch(ws, map[string]any{"action": "claim", "sessionId": "session-test", "dispatchId": "task-test", "attemptId": r.AttemptID}, env)
	if !errors.Is(err, fs.ErrExist) || !must(os.Stat(file+".lock")).IsDir() {
		t.Fatalf("held lock was not preserved: %v", err)
	}
}

func TestDispatchLedgerBoundsAndIdentity(t *testing.T) {
	for _, astral := range []bool{false, true} {
		for _, extra := range []int{0, 1} {
			t.Run("input limit", func(t *testing.T) {
				ws := t.TempDir()
				env, _ := home(t)
				b := map[string]any{"action": "start", "role": "executor", "sessionId": "session-test", "dispatchId": "task-test", "pad": ""}
				units := dispatchMaxInput - len(utf16.Encode([]rune(string(must(Stringify(b, "")))))) + extra
				pad := strings.Repeat("x", units)
				if astral {
					pad = strings.Repeat("😀", units/2) + strings.Repeat("x", units%2)
				}
				b["pad"] = pad
				_, err := RunDispatch(ws, b, env)
				if extra == 0 {
					check(t, err)
				} else if err == nil || err.Error() != "dispatch input exceeds 64 KiB" {
					t.Fatalf("oversized input: %v", err)
				}
			})
		}
	}
	ws, env, r, file := dispatchTestFixture(t)
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": r.AttemptID})
	for _, s := range []string{strings.Repeat("😀", 1000), " " + strings.Repeat("😀", 999) + " "} {
		got := dispatchTestCall(t, ws, env, map[string]any{"action": "report", "outcome": "created", "attemptId": r.AttemptID, "agentId": "child-a", "observedModel": s})
		if *got.Attempts[0].ObservedModel != strings.TrimSpace(s) {
			t.Fatal("observed model was not trimmed")
		}
	}
	for _, s := range []any{nil, " ", strings.Repeat("😀", 1000) + "x", " " + strings.Repeat("😀", 1000) + " "} {
		before := must(os.ReadFile(file))
		dispatchTestError(t, ws, env, map[string]any{"action": "report", "outcome": "created", "sessionId": "session-test", "dispatchId": "task-test", "attemptId": r.AttemptID, "agentId": "child-a", "observedModel": s}, "invalid observedModel")
		if string(must(os.ReadFile(file))) != string(before) {
			t.Fatal("invalid text changed ledger")
		}
	}
	for _, b := range []map[string]any{{"sessionId": "../escape", "dispatchId": "task-test"}, {"sessionId": "session-test", "dispatchId": "../escape"}, {"sessionId": "session-test", "dispatchId": "task-test", "action": "start", "role": "unknown"}} {
		dispatchTestError(t, t.TempDir(), env, b, "invalid")
	}
	foreign := func(k string) (string, bool) {
		if k == "CODEX_THREAD_ID" {
			return "foreign", true
		}
		return env(k)
	}
	dispatchTestError(t, ws, foreign, map[string]any{"action": "status", "sessionId": "session-test", "dispatchId": "task-test"}, "native main session")
	for _, v := range []any{nil, []any{}, "text", true} {
		if _, err := RunDispatch(ws, v, env); err == nil || err.Error() != "expected a JSON object" {
			t.Fatalf("record %v: %v", v, err)
		}
	}
}

func TestDispatchLedgerStoredValidation(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		edit       func(map[string]any, map[string]any)
	}{
		{"identity", "identity", func(d, a map[string]any) { d["version"] = 2 }},
		{"role", "identity", func(d, a map[string]any) { d["role"] = "unknown" }},
		{"candidates", "candidates", func(d, a map[string]any) { d["candidates"] = []any{} }},
		{"attempts", "attempts", func(d, a map[string]any) { d["attempts"] = nil }},
		{"dispatch status", "dispatch status", func(d, a map[string]any) { d["status"] = "bogus" }},
		{"status coercion error", "Cannot convert object to primitive value", func(d, a map[string]any) { d["status"] = map[string]any{"toString": nil} }},
		{"attempt id", "attemptId", func(d, a map[string]any) { a["id"] = "../escape" }},
		{"missing model", "stored model", func(d, a map[string]any) { delete(a["candidate"].(map[string]any), "model") }},
		{"missing effort", "stored effort", func(d, a map[string]any) { delete(a["candidate"].(map[string]any), "effort") }},
		{"candidate mismatch", "attempt candidate", func(d, a map[string]any) { a["candidate"].(map[string]any)["model"] = "different/model" }},
		{"claimed", "attempt candidate", func(d, a map[string]any) { a["claimed"] = nil }},
		{"issuance", "spawn issuance", func(d, a map[string]any) { delete(a, "spawnIssued") }},
		{"attempt status", "attempt status", func(d, a map[string]any) { a["status"] = "bogus" }},
		{"agent", "agentId", func(d, a map[string]any) { delete(a, "agentId") }},
		{"code", "code", func(d, a map[string]any) { a["code"] = 42 }},
		{"tool id", "toolUseId", func(d, a map[string]any) { a["toolUseId"] = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, env, _, file := dispatchTestFixture(t)
			var d map[string]any
			check(t, json.Unmarshal(must(os.ReadFile(file)), &d))
			tc.edit(d, d["attempts"].([]any)[0].(map[string]any))
			before := must(Stringify(d, ""))
			check(t, os.WriteFile(file, before, 0o600))
			dispatchTestError(t, ws, env, map[string]any{"action": "status", "sessionId": "session-test", "dispatchId": "task-test"}, tc.want)
			if string(must(os.ReadFile(file))) != string(before) {
				t.Fatal("corrupt state overwritten")
			}
		})
	}
}

func TestDispatchLedgerLegacyOrderAndGitRoot(t *testing.T) {
	ws, env, r, file := dispatchTestFixture(t)
	// Remove exactly the legacy member while preserving the attempt's key order.
	var d map[string]json.RawMessage
	check(t, json.Unmarshal(must(os.ReadFile(file)), &d))
	var list []json.RawMessage
	check(t, json.Unmarshal(d["attempts"], &list))
	a := must(parseObject(list[0]))
	a.remove("taskFailure")
	d["attempts"] = must(Stringify([]object{a}, ""))
	before := must(Stringify(d, ""))
	check(t, os.WriteFile(file, before, 0o600))
	got := dispatchTestCall(t, ws, env, map[string]any{"action": "status"})
	if got.Attempts[0].TaskFailure != nil || string(must(os.ReadFile(file))) != string(before) {
		t.Fatal("status saved legacy normalization")
	}
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": r.AttemptID})
	body := readText(t, file)
	if strings.Index(body, "\"taskFailure\"") < strings.Index(body, "\"toolUseId\"") {
		t.Fatal("legacy member did not append last")
	}
	repo := t.TempDir()
	_, err := git(repo, "init")
	check(t, err)
	sub := filepath.Join(repo, "nested")
	check(t, os.Mkdir(sub, 0o700))
	dispatchTestCall(t, sub, env, map[string]any{"action": "start", "role": "executor"})
	if _, err := os.Stat(filepath.Join(repo, ".crw", "dispatches", "session-test", "task-test.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sub, ".crw")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("ledger was written below git root")
	}
	t.Setenv("GIT_DIR", filepath.Join(repo, ".git"))
	t.Setenv("GIT_WORK_TREE", repo)
	dispatchTestError(t, t.TempDir(), env, map[string]any{"action": "start", "role": "executor", "sessionId": "session-test", "dispatchId": "outside"}, dispatchTestSafetyError(t, "unrelated-git-root"))
}

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
