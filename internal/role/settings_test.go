package role

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/oracle-settings.json holds what the CXC v0.2.40 oracle's settings API and spawn resolution answered (dist/settings-api.js
// and dist/store.js, global scope, CODEXCLAW_HOME in a temporary directory) over the cases of testdata/record-settings.mjs, recorded
// once under Node 24 (no Node runs here). Every case is replayed through the Go API and must agree on each result, error and the
// store's bytes. By design (I7, I11) a malformed store's error compares the "cannot update subagent config: " prefix only, and the
// oracle's TypeError for an unknown role at resolve is the port's refusal.

type settingsOp struct {
	Op     string   `json:"op"`
	Scope  *string  `json:"scope"`
	Body   string   `json:"body"`
	Role   RoleName `json:"role"`
	Result *string  `json:"result"`
	Error  *string  `json:"error"`
	File   *string  `json:"file"`
}

func TestSettingsOracleReplay(t *testing.T) {
	var cases []struct {
		ID    string `json:"id"`
		Given struct {
			Store *string `json:"store"`
		} `json:"given"`
		Ops []settingsOp `json:"ops"`
	}
	data, err := os.ReadFile(filepath.Join("testdata", "oracle-settings.json"))
	check(t, err)
	if err := json.Unmarshal(data, &cases); err != nil || len(cases) == 0 {
		t.Fatalf("testdata: %v (%d cases)", err, len(cases))
	}
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			env, dir := home(t)
			if c.Given.Store != nil {
				writeStore(t, dir, *c.Given.Store)
			}
			path := filepath.Join(dir, StoreFile)
			for i, op := range c.Ops {
				var scope json.RawMessage
				if op.Scope != nil {
					scope = json.RawMessage(*op.Scope)
				}
				get := func() (Settings, error) { return GetSettings(env, scope) }
				update := func() (Settings, error) { return UpdateSettings(env, json.RawMessage(op.Body)) }
				var got any
				switch op.Op {
				case "get":
					got, err = get()
				case "update":
					got, err = update()
				case "get_response":
					got, err = SettingsResponse(get), nil
				case "update_response":
					got, err = SettingsResponse(update), nil
				case "resolve":
					got, err = ResolveSpawnConfig(env, op.Role)
				default:
					t.Fatalf("op %d: unknown op %q", i, op.Op)
				}
				at := op.Op + " " + op.Body + string(op.Role)
				switch {
				case op.Error != nil && err == nil:
					t.Fatalf("%s: no error, want %q", at, *op.Error)
				case op.Error != nil && !sameSettingsError(c.ID, *op.Error, err.Error()):
					t.Fatalf("%s: error %q, want %q", at, err, *op.Error)
				case op.Error == nil && err != nil:
					t.Fatalf("%s: error %v", at, err)
				case op.Error == nil:
					if text := must(Stringify(got, "")); string(text) != *op.Result {
						t.Fatalf("%s: result\n%s\nwant\n%s", at, text, *op.Result)
					}
				}
				stored, readErr := os.ReadFile(path)
				if (readErr == nil) != (op.File != nil) || (op.File != nil && string(stored) != *op.File) {
					t.Fatalf("%s: store present = %v:\n%s\nwant\n%v", at, readErr == nil, stored, op.File)
				}
				if _, err := os.Stat(path + ".lock"); err == nil {
					t.Fatalf("%s: the lock was left behind", at)
				}
			}
		})
	}
}

func sameSettingsError(id, want, got string) bool {
	switch id {
	case "update_malformed_store", "update_malformed_store_to_primitive":
		const prefix = "cannot update subagent config: "
		return strings.HasPrefix(want, prefix) && strings.HasPrefix(got, prefix)
	case "resolve_unknown_role": // I11: V8's TypeError for an unguarded lookup becomes a refusal
		return want == "Cannot read properties of undefined (reading 'mode')" && got == `unknown role "nobody"`
	}
	return want == got
}

func TestJSString(t *testing.T) { // String() of a parsed JSON value
	for raw, want := range map[string]string{
		"5": "5", "1.5": "1.5", "1e21": "1e+21", "0.0000001": "1e-7", "0.000001": "0.000001", "-0": "0", "1e999": "Infinity", "-1e999": "-Infinity",
		"12345678901234567890": "12345678901234567000", "true": "true", "null": "null", `"x"`: "x", "[]": "", `[null,["a",2]]`: ",a,2",
		"{}": "[object Object]", `{"valueOf":1}`: "[object Object]", `{"a":{"toString":1}}`: "[object Object]",
	} {
		if got, err := jsString(json.RawMessage(raw)); err != nil || got != want {
			t.Errorf("%s: %q, %v, want %q", raw, got, err, want)
		}
	}
	if got, err := jsString(nil); err != nil || got != "undefined" {
		t.Errorf("absent: %q, %v", got, err)
	}
	for _, raw := range []string{`{"toString":null}`, `[1,{"toString":2}]`} { // an own toString member that is no function: ToPrimitive throws
		if _, err := jsString(json.RawMessage(raw)); err == nil || err.Error() != "Cannot convert object to primitive value" {
			t.Errorf("%s: err = %v", raw, err)
		}
	}
}

func apply(env func(string) (string, bool), body string) (Settings, error) {
	return UpdateSettings(env, json.RawMessage(body))
}

func TestFallbackThroughTheSettingsAPI(t *testing.T) { // fallback-config.test.ts, global layer
	for _, role := range Roles() {
		env, _ := home(t)
		body := func(rest string) string { return `{"scope":"global","role":"` + string(role) + `",` + rest + "}" }
		s := must(apply(env, body(`"mode":"model","model":"primary/a","effort":"high","fallback":{"model":"secondary/b","effort":"low"}`)))
		if f := s.Roles[role].Fallback; f == nil || f.Model != "secondary/b" || f.Effort == nil || *f.Effort != EffortLow {
			t.Fatalf("%s: fallback %+v", role, f)
		}
		s = must(apply(env, body(`"fallback":{"effort":null}`)))
		if f := s.Roles[role].Fallback; f == nil || f.Model != "secondary/b" || f.Effort != nil || s.Roles[role].Effort == nil || *s.Roles[role].Effort != EffortHigh {
			t.Fatalf("%s: effort-only update gave %+v, effort %v", role, f, s.Roles[role].Effort)
		}
		if s = must(apply(env, body(`"fallback":null`))); s.Roles[role].Fallback != nil {
			t.Fatalf("%s: fallback not cleared", role)
		}
		if s = must(apply(env, body(`"inherit":true`))); s.Overrides[role] || s.Sources[role] != SourceSession {
			t.Fatalf("%s: inherit did not reset the role", role)
		}
	}
}

func TestInvalidFallbackUpdatesKeepTheFileBytes(t *testing.T) {
	env, dir := home(t)
	must(apply(env, `{"scope":"global","role":"executor","mode":"model","model":"xai/grok-4.6","fallback":{"model":"cursor/grok-4.6","effort":null}}`))
	before := readText(t, filepath.Join(dir, StoreFile))
	for _, fallback := range []string{`{"model":"xai/grok-4.6"}`, `{"model":" "}`, `{"model":42}`, `{"effort":"bogus"}`, "[]"} {
		if _, err := apply(env, `{"scope":"global","role":"executor","fallback":`+fallback+"}"); err == nil || readText(t, filepath.Join(dir, StoreFile)) != before {
			t.Fatalf("fallback %s: err = %v, or the store changed", fallback, err)
		}
	}
	s := must(apply(env, `{"scope":"global","role":"executor","mode":"default","fallback":{"model":"xai/grok-4.6"}}`)) // a default-mode primary has no model to clash with
	if s.Roles[Executor].Fallback == nil || s.Roles[Executor].Fallback.Model != "xai/grok-4.6" {
		t.Fatalf("fallback = %+v", s.Roles[Executor].Fallback)
	}
	if _, err := apply(env, `{"scope":"global","role":"architect","fallback":{"effort":"low"}}`); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("effort-only creation without a model: %v", err) // "legacy JSON has no fallback, and effort-only creation without model is rejected"
	}
}

func TestSettingsWritesKeepOtherDataAndRefuseWithoutChangingBytes(t *testing.T) { // scopes.test.ts, global layer
	env, dir := home(t)
	path := writeStore(t, dir, `{"extra":{"keep":true},"roles":{"future":{"custom":1},"explorer":{"mode":"default","model":null,"effort":"low","promptOverride":null,"fallback":null}}}`)
	must(apply(env, `{"scope":"global","role":"reviewer","effort":null}`))
	if got := readText(t, path); !strings.Contains(got, `"keep": true`) || !strings.Contains(got, `"custom": 1`) {
		t.Fatalf("unrelated data lost:\n%s", got)
	}
	before := readText(t, path)
	for _, body := range []string{`{"scope":"global","role":"explorer","effort":"bad"}`, `{"scope":"project","role":"explorer"}`, `{"scope":"bad","role":"explorer"}`} {
		if _, err := apply(env, body); err == nil || readText(t, path) != before {
			t.Fatalf("%s: err = %v, or the store changed", body, err)
		}
	}
	writeStore(t, dir, "{broken")
	for _, body := range []string{`{"scope":"global","role":"reviewer","effort":null}`, `{"scope":"global","role":"explorer","inherit":true}`} {
		if _, err := apply(env, body); err == nil || !strings.HasPrefix(err.Error(), "cannot update") || readText(t, path) != "{broken" {
			t.Fatalf("%s: err = %v", body, err)
		}
	}
}
