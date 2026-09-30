package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// slfPython runs the assigned Python scenario live. The scenario emits one complete JSON value;
// the comparison below never picks fields from it.
func slfPython(t *testing.T, id string) any {
	t.Helper()
	raw := pythonOutput(t, id, func() ([]byte, error) {
		repo := repoRoot(t)
		script, err := filepath.Abs("testdata/slf_capture.py")
		if err != nil {
			return nil, err
		}
		home, err := os.MkdirTemp("", "crw-slf-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(home)
		cmd := exec.Command("uv", "run", "--no-sync", "python", script, id)
		cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home, "XDG_DATA_HOME="+home,
			"XDG_CONFIG_HOME="+home, "CODEX_HOME="+home, "TMPDIR="+os.TempDir())
		raw, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("live Python %s: %v\n%s", id, err, raw)
		}
		return raw, nil
	})
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("live Python %s JSON: %v\n%s", id, err, raw)
	}
	return out
}

func plainSLF(value any) any {
	switch v := value.(type) {
	case delivery.Obj:
		out := map[string]any{}
		for _, field := range v {
			out[field.Key] = plainSLF(field.Value)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = plainSLF(v[i])
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, x := range v {
			out[k] = plainSLF(x)
		}
		return out
	default:
		return value
	}
}

func jsonValue(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(plainSLF(value))
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func slfSame(t *testing.T, id string, got any) {
	t.Helper()
	goValue, python := jsonValue(t, got), slfPython(t, id)
	if !reflect.DeepEqual(goValue, python) {
		g, _ := json.Marshal(goValue)
		p, _ := json.Marshal(python)
		t.Fatalf("%s whole output differs\nGo: %s\nPython: %s", id, g, p)
	}
}

func slfObj(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for key := range x {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		o := delivery.Obj{}
		for _, key := range keys {
			o = append(o, delivery.F{Key: key, Value: slfObj(x[key])})
		}
		return o
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = slfObj(x[i])
		}
		return out
	default:
		return v
	}
}

const slfWork = "/workspace/example/worktree"

func slfSandbox() map[string]any {
	return map[string]any{"type": "workspaceWrite", "networkAccess": false, "writableRoots": []any{slfWork}, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
}
func slfBase() map[string]any {
	return map[string]any{"approvalPolicy": "never", "sandbox": slfSandbox(), "cwd": slfWork, "runtimeWorkspaceRoots": []any{slfWork}, "model": "gpt", "reasoningEffort": "high", "environments": []any{map[string]any{"environmentId": "local", "cwd": slfWork, "runtimeWorkspaceRoots": []any{slfWork}}}}
}
func slfCopy(v map[string]any, changes ...map[string]any) map[string]any {
	raw, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	for _, change := range changes {
		for k, value := range change {
			out[k] = value
		}
	}
	return out
}
func slfAnswer(changes map[string]any) map[string]any {
	base := slfCopy(slfBase(), map[string]any{"activePermissionProfile": nil, "thread": map[string]any{"environments": slfBase()["environments"]}})
	return slfCopy(base, changes)
}
func slfSettings(record map[string]any, free bool) delivery.TaskSettings {
	return delivery.TaskSettings{Data: slfObj(record).(delivery.Obj), SettingsFreeResume: free}
}
func slfCheck(record, answer map[string]any, free bool) map[string]any {
	// The guarded send's check as the bridge adapter runs it: registry's recorded-settings
	// predicate, and a settings-free resume that finds a difference names it
	// settings_differ_after_load.
	settings := slfSettings(record, free)
	transmitted := !settings.SettingsFreeResume
	findings := []any{}
	for _, finding := range (registry.TaskSettings{Data: settings.Data}).Mismatches(slfObj(answer), transmitted, false, transmitted) {
		findings = append(findings, finding)
	}
	code := any(nil)
	if len(findings) > 0 {
		for _, f := range findings[0].(contract.OrderedObject) {
			if f.Key == "code" {
				code = f.Value
			}
		}
		if !transmitted && code == registry.SettingsNotPreserved {
			code = registry.SettingsDifferAfterLoad
		}
	}
	return map[string]any{"code": code, "findings": findings}
}
func slfRefusal(err error) map[string]any {
	if err == nil {
		return map[string]any{"ok": nil}
	}
	var refused *store.RefusedError
	if !errors.As(err, &refused) {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"reason": refused.Reason, "detail": refused.Detail}
}

func slfWhole(t *testing.T, id string) {
	base := slfBase()
	switch id {
	case "SLF-1":
		slfSame(t, id, map[string]any{"resume": map[string]any{"threadId": "thread-1", "excludeTurns": true}, "accepted": slfCheck(base, slfAnswer(nil), true), "drift": slfCheck(base, slfAnswer(map[string]any{"model": "other"}), true)})
	case "SLF-2":
		s := slfSettings(base, false)
		slfSame(t, id, map[string]any{"resume": s.ResumeParams("thread-1"), "accepted": slfCheck(base, slfAnswer(nil), false)})
	case "SLF-3":
		narrow := slfCopy(base, map[string]any{"runtimeWorkspaceRoots": []any{slfWork, "/shared"}})
		slfSame(t, id, map[string]any{"narrow": slfCheck(narrow, slfAnswer(nil), true), "wide": slfCheck(base, slfAnswer(map[string]any{"runtimeWorkspaceRoots": []any{slfWork, "/"}}), true), "model": slfCheck(base, slfAnswer(map[string]any{"model": "other"}), true), "effort": slfCheck(base, slfAnswer(map[string]any{"reasoningEffort": "low"}), true)})
	case "SLF-4":
		slfSame(t, id, map[string]any{"settingsFreeResume": true, "resume": map[string]any{"threadId": "sup-thread", "excludeTurns": true}, "accepted": slfCheck(base, slfAnswer(nil), true)})
	case "SLF-5":
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		line := (&Channel{Store: s, Program: exe}).command("show", "--event", "event-1")
		program := strings.Fields(line)[0]
		slfSame(t, id, map[string]any{"absolute": filepath.IsAbs(program), "executable": func() bool { i, e := os.Stat(program); return e == nil && i.Mode()&0111 != 0 }(), "basename": "python3.13"})
	case "SLF-6":
		reading := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "reason": "terminal_without_report", "owed": true, "relationshipId": "rel-1", "executionGeneration": int64(3), "selectors": map[string]any{"turn": "turn-3"}}
		o := ObservationObligation(reading)
		if o == nil {
			t.Fatal("no omission obligation")
		}
		slfSame(t, id, map[string]any{"reading": map[string]any{"reportingState": "unreported", "reason": o.Basis["reason"], "executionGeneration": o.Generation, "selectors": map[string]any{"turn": o.Subject}}, "message": o.Kind + " " + o.Basis["reason"].(string) + " generation " + strconv.FormatInt(o.Generation.(int64), 10) + " " + o.Subject})
	case "SLF-7":
		cases := []map[string]any{slfCopy(base, map[string]any{"runtimeWorkspaceRoots": "/a/bc"}), slfCopy(base, map[string]any{"runtimeWorkspaceRoots": 7}), slfCopy(base, map[string]any{"runtimeWorkspaceRoots": []any{slfWork, 7}}), slfCopy(base, map[string]any{"environments": "local"}), slfCopy(base, map[string]any{"environments": []any{"local"}}), slfCopy(base, map[string]any{"environments": []any{map[string]any{"environmentId": 7, "cwd": slfWork}}}), slfCopy(base, map[string]any{"environments": []any{map[string]any{"environmentId": "local"}}}), slfCopy(base, map[string]any{"environments": []any{map[string]any{"environmentId": "local", "cwd": slfWork, "runtimeWorkspaceRoots": "/a/bc"}}}), slfCopy(base, map[string]any{"environments": []any{map[string]any{"environmentId": "local", "cwd": slfWork, "runtimeWorkspaceRoots": nil}}})}
		out := []any{}
		for _, one := range cases {
			out = append(out, slfRefusal(slfSettings(one, true).RequireUsable()))
		}
		slfSame(t, id, out)
	case "SLF-8":
		top := slfCopy(base, map[string]any{"runtimeWorkspaceRoots": "/a/bc"})
		env := slfCopy(base, map[string]any{"environments": []any{map[string]any{"environmentId": "local", "cwd": slfWork, "runtimeWorkspaceRoots": "/a/bc"}}})
		slfSame(t, id, map[string]any{"top": slfCheck(top, slfAnswer(map[string]any{"runtimeWorkspaceRoots": []any{"/"}}), true), "environment": slfCheck(env, slfAnswer(map[string]any{"thread": map[string]any{"environments": []any{map[string]any{"environmentId": "local", "cwd": slfWork, "runtimeWorkspaceRoots": []any{"/"}}}}}), true)})
	case "SLF-9", "SLF-15":
		answers := []map[string]any{slfAnswer(map[string]any{"runtimeWorkspaceRoots": 123}), slfAnswer(map[string]any{"runtimeWorkspaceRoots": slfWork}), slfAnswer(map[string]any{"runtimeWorkspaceRoots": []any{123}}), slfAnswer(map[string]any{"thread": map[string]any{"environments": "local"}}), slfAnswer(map[string]any{"thread": map[string]any{"environments": []any{7}}}), slfAnswer(map[string]any{"thread": map[string]any{"environments": []any{map[string]any{"cwd": slfWork}}}}), slfAnswer(map[string]any{"thread": map[string]any{"environments": []any{map[string]any{"environmentId": "local", "cwd": slfWork, "runtimeWorkspaceRoots": 123}}}}), slfAnswer(map[string]any{"thread": map[string]any{"environments": []any{map[string]any{"environmentId": "local", "cwd": slfWork, "runtimeWorkspaceRoots": nil}}}}), slfAnswer(map[string]any{"thread": "thread-1"})}
		out := []any{}
		for _, a := range answers {
			out = append(out, map[string]any{"free": slfCheck(base, a, true), "transmitted": slfCheck(base, a, false)})
		}
		slfSame(t, id, out)
	case "SLF-10":
		bad := slfCopy(slfSandbox(), map[string]any{"writableRoots": []any{123}})
		record := slfCopy(base, map[string]any{"sandbox": bad})
		slfSame(t, id, map[string]any{"record": slfRefusal(slfSettings(record, true).RequireUsable()), "both": slfCheck(record, slfAnswer(map[string]any{"sandbox": bad}), true), "answer": slfCheck(base, slfAnswer(map[string]any{"sandbox": bad}), true)})
	case "SLF-11":
		wrong := []struct {
			k string
			v any
		}{{"networkAccess", 0}, {"networkAccess", 1}, {"networkAccess", []any{1}}, {"networkAccess", "false"}, {"networkAccess", nil}, {"excludeTmpdirEnvVar", 0}, {"excludeSlashTmp", 1}, {"writableRoots", "/tmp"}, {"writableRoots", []any{7}}}
		records := []any{}
		for _, w := range wrong {
			bad := slfCopy(slfSandbox(), map[string]any{w.k: w.v})
			records = append(records, slfRefusal(slfSettings(slfCopy(base, map[string]any{"sandbox": bad}), true).RequireUsable()))
		}
		zero := slfCopy(slfSandbox(), map[string]any{"networkAccess": 0})
		listed := slfCopy(slfSandbox(), map[string]any{"networkAccess": []any{1}})
		slfSame(t, id, map[string]any{"records": records, "comparisons": []any{slfCheck(slfCopy(base, map[string]any{"sandbox": zero}), slfAnswer(nil), true), slfCheck(base, slfAnswer(map[string]any{"sandbox": zero}), true), slfCheck(slfCopy(base, map[string]any{"sandbox": listed}), slfAnswer(map[string]any{"sandbox": listed}), true)}})
	case "SLF-12":
		env := []any{map[string]any{"environmentId": "local", "cwd": slfWork}}
		record := slfCopy(base, map[string]any{"environments": env})
		slfSame(t, id, map[string]any{"usable": slfRefusal(slfSettings(record, true).RequireUsable()), "comparison": slfCheck(record, slfAnswer(map[string]any{"thread": map[string]any{"environments": env}}), true)})
	case "SLF-13":
		profile := map[string]any{"id": "profile-1", "extends": nil, "rules": []any{}}
		record := slfCopy(base, map[string]any{"expectedPermissionProfile": profile})
		same := map[string]any{"rules": []any{}, "extends": nil, "id": "profile-1"}
		missing := slfAnswer(nil)
		delete(missing, "activePermissionProfile")
		slfSame(t, id, map[string]any{"usable": slfRefusal(slfSettings(record, true).RequireUsable()), "same": slfCheck(record, slfAnswer(map[string]any{"activePermissionProfile": same}), true), "bool": slfCheck(slfCopy(base, map[string]any{"expectedPermissionProfile": 0}), slfAnswer(map[string]any{"activePermissionProfile": false}), true), "missing": slfCheck(record, missing, true), "none": slfCheck(base, slfAnswer(nil), true)})
	case "SLF-14":
		extra := map[string]any{"environmentId": "local", "cwd": slfWork, "runtimeWorkspaceRoots": []any{slfWork}, "extraAuthorization": "restricted"}
		plain := map[string]any{"environmentId": "local", "cwd": slfWork, "runtimeWorkspaceRoots": []any{slfWork}}
		different := slfCopy(extra, map[string]any{"extraAuthorization": 0})
		slfSame(t, id, map[string]any{"missing": slfCheck(slfCopy(base, map[string]any{"environments": []any{extra}}), slfAnswer(map[string]any{"thread": map[string]any{"environments": []any{plain}}}), true), "extra": slfCheck(slfCopy(base, map[string]any{"environments": []any{plain}}), slfAnswer(map[string]any{"thread": map[string]any{"environments": []any{extra}}}), true), "different": slfCheck(slfCopy(base, map[string]any{"environments": []any{extra}}), slfAnswer(map[string]any{"thread": map[string]any{"environments": []any{different}}}), true), "same": slfCheck(slfCopy(base, map[string]any{"environments": []any{extra}}), slfAnswer(map[string]any{"thread": map[string]any{"environments": []any{extra}}}), true)})
	default:
		t.Fatalf("unknown %s", id)
	}
}
