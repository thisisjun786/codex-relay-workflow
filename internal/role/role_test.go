package role

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) { testsupport.Main(m) }

// home is a temporary CRW_HOME and an environment whose HOME and CODEX_HOME are temporary too, so no test reads or writes the
// real home.
func home(t *testing.T) (host.LookupEnv, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "crw")
	vars := map[string]string{"CRW_HOME": dir, "HOME": t.TempDir(), "CODEX_HOME": t.TempDir()}
	return func(key string) (string, bool) { v, ok := vars[key]; return v, ok }, dir
}

// must is for setup that cannot fail on a healthy temporary directory; a failure panics, which fails the test with its stack.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func writeStore(t *testing.T, dir, text string) string {
	t.Helper()
	path := filepath.Join(dir, StoreFile)
	check(t, os.MkdirAll(dir, 0o755))
	check(t, os.WriteFile(path, []byte(text), 0o644))
	return path
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	check(t, err)
	return string(data)
}

func str(s string) *string { return &s }

func eff(e EffortName) *EffortName { return &e }

func TestMissingStoreIsDefaults(t *testing.T) { // store.test.ts "AC1"
	env, dir := home(t)
	if cfg := must(ReadConfig(env)); !reflect.DeepEqual(cfg, DefaultConfig()) {
		t.Fatalf("config = %+v, want the defaults", cfg)
	}
	s := must(ReadSettings(env))
	for _, r := range Roles() {
		if s.Sources[r] != SourceSession || s.Overrides[r] || s.Scope != ScopeGlobal {
			t.Fatalf("role %s: source %q override %v scope %q", r, s.Sources[r], s.Overrides[r], s.Scope)
		}
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a read created %s: %v", dir, err)
	}
}

func TestSetPersistsAndReadsBack(t *testing.T) { // "AC2"
	env, dir := home(t)
	cfg := must(SetRole(env, Reviewer, RolePatch{Mode: Some(ModeModel), Model: Some("gpt-5.5"), PromptOverride: Some("Be adversarial.")}))
	want := RoleConfig{Mode: ModeModel, Model: str("gpt-5.5"), PromptOverride: str("Be adversarial.")}
	if !reflect.DeepEqual(cfg.Roles[Reviewer], want) || !reflect.DeepEqual(cfg.Roles[Explorer], DefaultRole()) {
		t.Fatalf("config = %+v", cfg)
	}
	for path, mode := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, StoreFile): 0o600} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v, want %o", path, info, err, mode)
		}
	}
	if entries := must(os.ReadDir(dir)); len(entries) != 1 { // "atomic write leaves no orphan .tmp"
		t.Fatalf("directory holds %d entries, want only the store", len(entries))
	}
}

func TestValidate(t *testing.T) { // "validation: ..." and the oracle's messages
	const efforts = "(must be one of low/medium/high/xhigh or null)"
	for _, c := range []struct {
		name  string
		patch RolePatch
		want  string
	}{
		{"invalid mode", RolePatch{Mode: Some(RoleMode("turbo"))}, `invalid mode "turbo" (must be "default" or "model")`},
		{"null mode", RolePatch{Mode: Null[RoleMode]()}, `invalid mode "null" (must be "default" or "model")`},
		{"model mode without a model", RolePatch{Mode: Some(ModeModel), Model: Null[string]()}, `mode "model" requires a non-empty model id`},
		{"unknown effort", RolePatch{Effort: Some(EffortName("x-high"))}, `invalid effort "x-high" ` + efforts},
		{"blank fallback model", RolePatch{Fallback: Some(FallbackPatch{Model: Some(" \t")})}, "fallback requires a non-empty model id"},
		{"null fallback model", RolePatch{Fallback: Some(FallbackPatch{Model: Null[string]()})}, "fallback requires a non-empty model id"},
		{"unknown fallback effort", RolePatch{Fallback: Some(FallbackPatch{Effort: Some(EffortName("max"))})}, `invalid fallback effort "max" ` + efforts},
		{"fallback equals primary", RolePatch{Mode: Some(ModeModel), Model: Some("m"), Fallback: Some(FallbackPatch{Model: Some("m")})}, "fallback model must differ from the primary model"},
		{"cleared fallback", RolePatch{Fallback: Null[FallbackPatch]()}, ""},
	} {
		if got := Validate(c.patch); (got == nil && c.want != "") || (got != nil && got.Error() != c.want) {
			t.Errorf("%s: got %v, want %q", c.name, got, c.want)
		}
	}
	if got := Efforts(); !reflect.DeepEqual(got, []EffortName{"low", "medium", "high", "xhigh"}) { // "offered set is exactly ..."
		t.Errorf("efforts = %v", got)
	}
}

func TestSetRoleSemantics(t *testing.T) { // default-mode invariant, merged validation, effort and prompt values
	env, _ := home(t)
	step := func(role RoleName, p RolePatch, wantErr string) RoleConfig {
		t.Helper()
		cfg, err := SetRole(env, role, p)
		if (err == nil) != (wantErr == "") || (err != nil && !strings.Contains(err.Error(), wantErr)) {
			t.Fatalf("%s %+v: err = %v, want %q", role, p, err, wantErr)
		}
		return cfg.Roles[role]
	}
	step(Reviewer, RolePatch{Mode: Some(ModeModel), Model: Some("m1")}, "")
	if r := step(Reviewer, RolePatch{Mode: Some(ModeDefault)}, ""); r.Model != nil {
		t.Fatalf("default mode kept the model %v", *r.Model)
	}
	step(Reviewer, RolePatch{Mode: Some(ModeModel)}, "requires a non-empty model") // default cleared the model
	if r := step(Reviewer, RolePatch{Mode: Some(ModeModel), Model: Some("m2")}, ""); *r.Model != "m2" {
		t.Fatalf("model = %v", r.Model)
	}
	if r := step(Reviewer, RolePatch{Mode: Some(ModeModel)}, ""); *r.Model != "m2" { // a bare re-assert passes: the stored role has a model
		t.Fatalf("model = %v", r.Model)
	}
	step(Explorer, RolePatch{Mode: Some(ModeModel)}, "requires a non-empty model") // a fresh role does not
	step(Explorer, RolePatch{Effort: Some(EffortHigh)}, "")
	if r := step(Explorer, RolePatch{Effort: Null[EffortName]()}, ""); r.Effort != nil {
		t.Fatalf("effort = %v, want inherit", r.Effort)
	}
	for _, bad := range []EffortName{"x-high", "max", "none", "minimal"} {
		step(Explorer, RolePatch{Effort: Some(bad)}, "invalid effort")
	}
	if r := step(Explorer, RolePatch{PromptOverride: Null[string]()}, ""); r.PromptOverride != nil {
		t.Fatalf("prompt = %v, want none", r.PromptOverride)
	}
}

func TestMalformedStoreReadsAsDefaultsAndRefusesWrites(t *testing.T) { // "malformed file -> defaults, never throws"
	for _, text := range []string{"{ not json ]", "", `{"roles":[]}`, `{"roles":null}`, "[]", "null", `{"roles":{}} x`, "\ufeff{}"} {
		env, dir := home(t)
		path := writeStore(t, dir, text)
		if cfg := must(ReadConfig(env)); !reflect.DeepEqual(cfg, DefaultConfig()) {
			t.Errorf("%q: config = %+v", text, cfg)
		}
		if _, err := SetRole(env, Explorer, RolePatch{Effort: Some(EffortLow)}); err == nil || !strings.HasPrefix(err.Error(), "cannot update subagent config: ") {
			t.Errorf("%q: set err = %v", text, err)
		}
		if readText(t, path) != text {
			t.Errorf("%q: the store changed", text)
		}
	}
}

func TestPersistedValuesAreNormalisedPerField(t *testing.T) { // "partial/invalid role values normalized per-field", "unknown persisted effort"
	env, dir := home(t)
	writeStore(t, dir, `{"roles":{"reviewer":{"mode":"model","model":123,"promptOverride":7},
		"executor":{"mode":"default","model":null,"effort":"ultra","promptOverride":null},
		"explorer":{"mode":"model","model":"m","effort":"low","promptOverride":"","fallback":{"model":" f ","effort":"nope"}},
		"architect":{"mode":"model","model":"m","fallback":{"model":"  "}}}}`)
	roles := must(ReadConfig(env)).Roles
	for role, want := range map[RoleName]RoleConfig{
		Reviewer:  DefaultRole(), // model 123 is invalid: fail safe to default; the prompt 7 is not a string
		Executor:  DefaultRole(), // an unknown effort inherits
		Explorer:  {Mode: ModeModel, Model: str("m"), Effort: eff(EffortLow), PromptOverride: str(""), Fallback: &RoleFallback{Model: " f "}},
		Architect: {Mode: ModeModel, Model: str("m")},
	} {
		if !reflect.DeepEqual(roles[role], want) {
			t.Errorf("%s = %+v, want %+v", role, roles[role], want)
		}
	}
}

func TestPublishFailureKeepsStore(t *testing.T) {
	env, dir := home(t)
	must(SetRole(env, Reviewer, RolePatch{Effort: Some(EffortLow)}))
	path := filepath.Join(dir, StoreFile)
	before := readText(t, path)
	doc := &object{{"roles", &object{}}}
	if err := writeRaw(path, doc, func(tmp, final string) error { return errors.New("rename refused") }); err == nil || err.Error() != "rename refused" {
		t.Fatalf("err = %v", err)
	}
	if readText(t, path) != before {
		t.Fatal("the failed publish changed the store")
	}
	if entries := must(os.ReadDir(dir)); len(entries) != 1 {
		t.Fatalf("the temporary file was left behind: %v", entries)
	}
}

func TestStorePath(t *testing.T) {
	for _, c := range []struct {
		vars map[string]string
		want string
	}{
		{map[string]string{"CRW_HOME": "/x/crw", "HOME": "/h"}, "/x/crw/subagents.json"},
		{map[string]string{"CRW_HOME": "  /x/y \t", "HOME": "/h"}, "/x/y/subagents.json"},
		{map[string]string{"CRW_HOME": " \n", "HOME": "/h"}, "/h/.crw/subagents.json"},
		{map[string]string{"HOME": "/h"}, "/h/.crw/subagents.json"},
		{map[string]string{"HOME": " h b "}, " h b /.crw/subagents.json"}, // only an explicit CRW_HOME is trimmed
	} {
		env := func(key string) (string, bool) { v, ok := c.vars[key]; return v, ok }
		if got := must(StorePath(env)); got != c.want {
			t.Errorf("%v: %q, want %q", c.vars, got, c.want)
		}
	}
}

func TestParseScope(t *testing.T) { // configScope; the oracle's default "project" layer is gone (decision 7)
	for value, want := range map[string]string{"global": "", "project": `invalid scope "project"`, "galaxy": `invalid scope "galaxy"`} {
		if scope, err := ParseScope(&value); (err == nil) != (want == "") || (err != nil && err.Error() != want) || (err == nil && scope != ScopeGlobal) {
			t.Errorf("%q: %v %v", value, scope, err)
		}
	}
	if scope, err := ParseScope(nil); err != nil || scope != ScopeGlobal {
		t.Errorf("absent scope: %v %v", scope, err)
	}
}

func TestDeepNestingIsRefused(t *testing.T) { // I9: encoding/json stops at 10,000 levels, JSON.parse does not
	env, dir := home(t)
	text := `{"deep":` + strings.Repeat("[", 10001) + strings.Repeat("]", 10001) + `,"roles":{"explorer":{"mode":"model","model":"m"}}}`
	path := writeStore(t, dir, text)
	if cfg := must(ReadConfig(env)); !reflect.DeepEqual(cfg, DefaultConfig()) {
		t.Fatalf("a store nested past the limit read as %+v", cfg)
	}
	if _, err := SetRole(env, Explorer, RolePatch{Effort: Some(EffortLow)}); err == nil || readText(t, path) != text {
		t.Fatalf("set: %v", err)
	}
}
