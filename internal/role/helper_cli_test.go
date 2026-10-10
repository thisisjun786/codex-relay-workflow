package role

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

func helperCLIDecode[T any](t *testing.T, result HelperResult) T {
	t.Helper()
	if result.Code != 0 {
		t.Fatalf("code %d: %s", result.Code, result.Output)
	}
	var value T
	if err := json.Unmarshal([]byte(result.Output), &value); err != nil {
		t.Fatalf("decode %q: %v", result.Output, err)
	}
	return value
}

// The eight applicable B-class cases in subagent-config/test/cli.test.ts.
// The project-bound trust-token case is replaced by an explicit removal check.
func TestHelperCLIParseShapes(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want HelperArgs
	}{
		{nil, HelperArgs{Action: "list"}},
		{[]string{"list"}, HelperArgs{Action: "list"}},
		{[]string{"help"}, HelperArgs{Action: "help"}},
		{[]string{"get", "reviewer"}, HelperArgs{Action: "get", Role: Reviewer}},
		{[]string{"set", "executor", "--mode", "model", "--model", "m1"}, HelperArgs{Action: "set", Role: Executor, Patch: RolePatch{Mode: Some(ModeModel), Model: Some("m1")}}},
	} {
		if got := ParseHelperArgs(tc.args); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %+v, want %+v", tc.args, got, tc.want)
		}
	}
}

func TestHelperCLIParseErrors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"get", "nope"}, "unknown role 'nope' (expected explorer|reviewer|executor|architect)"},
		{[]string{"get"}, "unknown role '' (expected explorer|reviewer|executor|architect)"},
		{[]string{"set", "nope"}, "unknown role 'nope' (expected explorer|reviewer|executor|architect)"},
		{[]string{"set", "reviewer", "--mode", "weird"}, "--mode must be default|model (got 'weird')"},
		{[]string{"set", "reviewer", "--mode"}, "--mode requires a value"}, // CRW-1117: a missing value is refused
		{[]string{"set", "reviewer"}, "set requires at least one of --mode/--model/--effort/--clear-effort/--prompt/--clear-prompt/--fallback-model/--fallback-effort/--clear-fallback"},
		{[]string{"set", "reviewer", "--bogus"}, "unknown flag '--bogus'"},
		{[]string{"set", "reviewer", "--effort", "turbo"}, "--effort must be low|medium|high|xhigh (got 'turbo')"},
		{[]string{"set", "reviewer", "--effort"}, "--effort requires a value"},
		{[]string{"reset"}, "reset requires exactly one valid role"},
		{[]string{"reset", "nope"}, "reset requires exactly one valid role"},
		{[]string{"reset", "reviewer", "extra"}, "reset requires exactly one valid role"},
		{[]string{"register"}, "usage: subagents register executor|architect"},
		{[]string{"register", "reviewer"}, "usage: subagents register executor|architect"},
		{[]string{"register", "executor", "--global"}, "usage: subagents register executor|architect"},
		{[]string{"trust-token"}, "unknown subcommand 'trust-token'"},
		{[]string{"dispatch"}, "unknown subcommand 'dispatch'"},
		{[]string{"bogus"}, "unknown subcommand 'bogus'"},
		{[]string{"set", "reviewer", "--fallback-model"}, "--fallback-model requires a value"},
		{[]string{"set", "reviewer", "--fallback-model", ""}, "--fallback-model requires a model id"},
		{[]string{"set", "reviewer", "--fallback-model", "\ufeff"}, "--fallback-model requires a model id"},
		{[]string{"set", "reviewer", "--fallback-effort"}, "--fallback-effort requires a value"},
		{[]string{"set", "reviewer", "--fallback-effort", ""}, "invalid --fallback-effort"},
		{[]string{"set", "reviewer", "--fallback-effort", "max"}, "invalid --fallback-effort"},
		{[]string{"set", "reviewer", "--clear-fallback", "--fallback-model", "m2"}, "--clear-fallback cannot be combined with fallback settings"},
		{[]string{"set", "reviewer", "--fallback-effort", "inherit", "--clear-fallback"}, "--clear-fallback cannot be combined with fallback settings"},
		{[]string{"set", "reviewer", "--global", "--effort", "low"}, "unknown flag '--global'"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			parsed := ParseHelperArgs(tc.args)
			if parsed.Err != tc.want {
				t.Fatalf("error = %q, want %q", parsed.Err, tc.want)
			}
			env, _ := home(t)
			got := RunHelper(parsed, env)
			if got.Code != 1 || got.Output != "subagents: "+tc.want {
				t.Fatalf("result = %+v", got)
			}
		})
	}
}

func TestHelperCLIListDefaults(t *testing.T) {
	env, dir := home(t)
	cfg := helperCLIDecode[Config](t, RunHelper(ParseHelperArgs(nil), env))
	if !reflect.DeepEqual(cfg, DefaultConfig()) {
		t.Fatalf("default config = %+v", cfg)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("list created a store directory: %v", err)
	}
}

func TestHelperCLISetGetRoundtrip(t *testing.T) {
	env, _ := home(t)
	helperCLIDecode[RoleConfig](t, RunHelper(ParseHelperArgs([]string{"set", "reviewer", "--mode", "model", "--model", "grok-4"}), env))
	cfg := must(ReadConfig(env))
	if cfg.Roles[Reviewer].Model == nil || *cfg.Roles[Reviewer].Model != "grok-4" {
		t.Fatal("set did not persist reviewer model")
	}
	got := helperCLIDecode[RoleConfig](t, RunHelper(ParseHelperArgs([]string{"get", "reviewer"}), env))
	if got.Mode != ModeModel || got.Model == nil || *got.Model != "grok-4" {
		t.Fatalf("get = %+v", got)
	}
}

func TestHelperCLIPromptSetClear(t *testing.T) {
	env, _ := home(t)
	for _, tc := range []struct {
		args []string
		want *string
	}{
		{[]string{"--prompt", "be terse"}, str("be terse")},
		{[]string{"--clear-prompt"}, nil},
		{[]string{"--prompt="}, str("")}, // CRW-1117: an empty value is written --prompt=
	} {
		helperCLIDecode[RoleConfig](t, RunHelper(ParseHelperArgs(append([]string{"set", "executor"}, tc.args...)), env))
		if got := must(ReadConfig(env)).Roles[Executor].PromptOverride; !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q: prompt = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestHelperCLIEffortSetClear(t *testing.T) {
	env, _ := home(t)
	for _, tc := range []struct {
		args []string
		want *EffortName
	}{
		{[]string{"--effort", "high"}, eff(EffortHigh)},
		{[]string{"--clear-effort"}, nil},
	} {
		helperCLIDecode[RoleConfig](t, RunHelper(ParseHelperArgs(append([]string{"set", "explorer"}, tc.args...)), env))
		if got := must(ReadConfig(env)).Roles[Explorer].Effort; !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q: effort = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestHelperCLIStoreValidation(t *testing.T) {
	env, _ := home(t)
	got := RunHelper(HelperArgs{Action: "set", Role: Reviewer, Patch: RolePatch{Mode: Some(ModeModel)}}, env)
	if got.Code != 1 || got.Output != `subagents: mode "model" requires a non-empty model id` {
		t.Fatalf("validation = %+v", got)
	}
}

func TestHelperCLIArchitectIsolation(t *testing.T) {
	env, _ := home(t)
	for _, r := range []RoleName{Reviewer, Architect} {
		helperCLIDecode[RoleConfig](t, RunHelper(ParseHelperArgs([]string{"set", string(r), "--mode", "model", "--model", string(r) + "-only", "--effort", "high"}), env))
	}
	for _, r := range []RoleName{Reviewer, Architect} {
		got := helperCLIDecode[RoleConfig](t, RunHelper(ParseHelperArgs([]string{"get", string(r)}), env))
		if got.Model == nil || *got.Model != string(r)+"-only" {
			t.Fatalf("role %s = %+v", r, got)
		}
	}
}

func TestHelperCLIParserQuirksAndScope(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want HelperArgs
	}{
		// CRW-1117: list and get refuse a trailing token, and a value that starts with '-' is written --flag=value.
		{[]string{"list", "ignored"}, HelperArgs{Action: "list", Err: "list takes no arguments (got 'ignored')"}},
		{[]string{"get", "reviewer", "ignored"}, HelperArgs{Action: "get", Role: Reviewer, Err: "get takes exactly one role (got 'ignored')"}},
		{[]string{"help", "ignored"}, HelperArgs{Action: "help"}},
		{[]string{"--global"}, HelperArgs{Action: "list", Scope: ScopeGlobal}},
		{[]string{"get", "reviewer", "--global"}, HelperArgs{Action: "get", Role: Reviewer, Scope: ScopeGlobal}},
		{[]string{"register", "architect"}, HelperArgs{Action: "register", Role: Architect}},
		{[]string{"register", "executor"}, HelperArgs{Action: "register", Role: Executor}},
		{[]string{"set", "reviewer", "--model="}, HelperArgs{Action: "set", Role: Reviewer, Patch: RolePatch{Model: Some("")}}},
		{[]string{"set", "reviewer", "--prompt=--global"}, HelperArgs{Action: "set", Role: Reviewer, Patch: RolePatch{PromptOverride: Some("--global")}}},
		{[]string{"set", "reviewer", "--model=--global"}, HelperArgs{Action: "set", Role: Reviewer, Patch: RolePatch{Model: Some("--global")}}},
		{[]string{"set", "reviewer", "--fallback-model=--global"}, HelperArgs{Action: "set", Role: Reviewer, Patch: RolePatch{Fallback: Some(FallbackPatch{Model: Some("--global")})}}},
		{[]string{"set", "reviewer", "--prompt", "first", "--clear-prompt", "--prompt", "last", "--global"}, HelperArgs{Action: "set", Role: Reviewer, Scope: ScopeGlobal, Patch: RolePatch{PromptOverride: Some("last")}}},
		{[]string{"set", "reviewer", "--fallback-effort", "inherit"}, HelperArgs{Action: "set", Role: Reviewer, Patch: RolePatch{Fallback: Some(FallbackPatch{Effort: Null[EffortName]()})}}},
	} {
		if got := ParseHelperArgs(tc.args); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %+v, want %+v", tc.args, got, tc.want)
		}
	}
}

func TestHelperCLIFallbackAndReset(t *testing.T) {
	env, dir := home(t)
	helperCLIDecode[RoleConfig](t, RunHelper(ParseHelperArgs([]string{"set", "executor", "--mode", "model", "--model", "primary", "--fallback-model", " backup ", "--fallback-effort", "high"}), env))
	got := helperCLIDecode[RoleConfig](t, RunHelper(ParseHelperArgs([]string{"set", "executor", "--fallback-effort", "inherit"}), env))
	if got.Fallback == nil || got.Fallback.Model != " backup " || got.Fallback.Effort != nil {
		t.Fatalf("fallback = %+v", got.Fallback)
	}
	got = helperCLIDecode[RoleConfig](t, RunHelper(ParseHelperArgs([]string{"set", "executor", "--clear-fallback"}), env))
	if got.Fallback != nil {
		t.Fatal("fallback not cleared")
	}
	before := readText(t, filepath.Join(dir, StoreFile))
	bad := RunHelper(ParseHelperArgs([]string{"set", "executor", "--fallback-model", "primary"}), env)
	if bad.Code != 1 || !strings.Contains(bad.Output, "must differ") {
		t.Fatalf("fallback validation = %+v", bad)
	}
	if readText(t, filepath.Join(dir, StoreFile)) != before {
		t.Fatal("refused patch changed store")
	}
	got = helperCLIDecode[RoleConfig](t, RunHelper(ParseHelperArgs([]string{"reset", "executor", "--global"}), env))
	if !reflect.DeepEqual(got, DefaultRole()) || must(ReadSettings(env)).Overrides[Executor] {
		t.Fatal("reset did not remove override")
	}
}

func TestHelperCLICorruptStoreAndProjectIsolation(t *testing.T) {
	env, dir := home(t)
	path := writeStore(t, dir, "broken JSON\n")
	if got := RunHelper(ParseHelperArgs(nil), env); got.Code != 1 || !strings.Contains(got.Output, "unusable helper role settings") {
		t.Fatalf("corrupt read = %+v, want the store's refusal (CRW-1119)", got)
	}
	for _, args := range [][]string{{"set", "reviewer", "--prompt", "x"}, {"reset", "reviewer"}} {
		got := RunHelper(ParseHelperArgs(args), env)
		if got.Code != 1 || !strings.Contains(got.Output, "cannot update subagent config") {
			t.Fatalf("%q: %+v", args, got)
		}
		if readText(t, path) != "broken JSON\n" {
			t.Fatal("corrupt settings overwritten")
		}
	}
	workspace := t.TempDir()
	check(t, os.Mkdir(filepath.Join(workspace, ".crw"), 0700))
	project := filepath.Join(workspace, ".crw", StoreFile)
	check(t, os.WriteFile(project, []byte("private project bytes"), 0600))
	t.Chdir(workspace)
	clean, _ := home(t)
	helperCLIDecode[RoleConfig](t, RunHelper(ParseHelperArgs([]string{"set", "reviewer", "--prompt", "global"}), clean))
	if readText(t, project) != "private project bytes" {
		t.Fatal("project store changed")
	}
}

func TestHelperCLIRegistration(t *testing.T) {
	env, _ := home(t)
	native, _ := env("CODEX_HOME")
	for _, r := range []RoleName{Executor, Architect} {
		for i, prefix := range []string{"Registered: ", "Already registered: "} {
			got := RunHelper(ParseHelperArgs([]string{"register", string(r)}), env)
			path := filepath.Join(native, "agents", string(r)+".toml")
			want := prefix + path + "\nStart a new Codex session and verify " + string(r) + " appears in the live spawn schema."
			if got.Code != 0 || got.Output != want {
				t.Fatalf("%s attempt%d = %+v", r, i, got)
			}
			if !strings.HasPrefix(readText(t, path), "# crw-managed: ") {
				t.Fatal("missing native definition")
			}
		}
	}
	path := filepath.Join(native, "agents", "architect.toml")
	check(t, os.WriteFile(path, []byte("foreign bytes"), 0600))
	if got := RunHelper(ParseHelperArgs([]string{"register", "architect"}), env); got.Code != 1 || !strings.Contains(got.Output, "differs; preserved") {
		t.Fatalf("foreign = %+v", got)
	}
	if readText(t, path) != "foreign bytes" {
		t.Fatal("foreign role changed")
	}
	if got := RunHelper(ParseHelperArgs([]string{"register", "executor"}), env, ""); got.Code != 1 || !strings.Contains(got.Output, "invalid native role home") {
		t.Fatalf("blank home = %+v", got)
	}
}

func TestHelperCLIStreamsAndTable(t *testing.T) {
	env, _ := home(t)
	for _, tc := range []struct {
		args []string
		code int
		out  string
		err  string
	}{
		{nil, 2, "", "command is required"},
		{[]string{"unknown"}, 2, "", "invalid command"},
		{[]string{"--help"}, 0, "usage: crw role", ""},
		{[]string{"helper", "--help"}, 0, "crw role helper", ""},
		{[]string{"helper", "get", "unknown"}, 1, "subagents: unknown role", ""},
		{[]string{"helper", "dispatch"}, 1, `{"error":"invalid character 'u' looking for beginning of value"}`, ""},
	} {
		var out, errOut bytes.Buffer
		code := CLI(tc.args, strings.NewReader("unread input"), &out, &errOut, env)
		if code != tc.code || (tc.out == "" && out.Len() != 0) || !strings.Contains(out.String(), tc.out) || (tc.err == "" && errOut.Len() != 0) || !strings.Contains(errOut.String(), tc.err) {
			t.Errorf("%q: %d %q %q", tc.args, code, out.String(), errOut.String())
		}
		if out.Len() > 0 && (!strings.HasSuffix(out.String(), "\n") || strings.HasSuffix(out.String(), "\n\n")) {
			t.Error("stdout must end with one newline")
		}
	}
	var out, errOut bytes.Buffer
	if code := CLI([]string{"helper"}, strings.NewReader(""), &out, &errOut, env); code != 0 || errOut.Len() != 0 {
		t.Fatalf("bare helper: %d %q", code, errOut.String())
	}
	var cfg Config
	check(t, json.Unmarshal(out.Bytes(), &cfg))
	if !reflect.DeepEqual(cfg, DefaultConfig()) {
		t.Fatal("bare helper did not list")
	}
	names := []string{}
	for _, c := range HelperCommands() {
		if c.Run == nil {
			t.Fatalf("nil command %s", c.Name)
		}
		names = append(names, c.Name)
	}
	if !reflect.DeepEqual(names, []string{"dispatch", "list", "get", "set", "reset", "register", "help", "--help", "-h"}) {
		t.Fatalf("command table = %v", names)
	}
	// The CLI accepts arbitrary data as a prompt, including markup and CJK text.
	long := strings.Repeat("<안녕>&", 2048)
	got := helperCLIDecode[RoleConfig](t, RunHelper(ParseHelperArgs([]string{"set", "reviewer", "--prompt", long}), env))
	if got.PromptOverride == nil || *got.PromptOverride != long {
		t.Fatal("prompt data truncated or changed")
	}
}

// A compile-time declaration checks the injected environment contract.
var _ host.LookupEnv = os.LookupEnv
