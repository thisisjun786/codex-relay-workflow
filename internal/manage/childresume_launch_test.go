package manage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

// CRW-1486/S2 (B1): settings-show gives an unbound child no role finding and no policy digest, so a
// child that only cites its role passes the gate whatever the relay service's launch declaration
// says. child-resume reads the declaration itself for that child: when it contradicts
// CODEX_THREAD_BRIDGE_EXECUTION_POLICY, or cannot be read, the pair's auto-compaction limit cannot be
// told from the file a delivery would read, and the resume is refused naming why. Under a readable
// declaration that unbound child takes its limit from the declared file, and a declared file that
// cannot be read is a refusal too (CRW-1506). Every other child keeps the behaviour it had: a bound one
// (the gate itself refuses it), one that cites no role, and an unbound one under no declaration.

// boundRefusedSettings is the settings-show answer for a task bound to the child role when the
// relay cannot read its launch policy: the gate's own refusal.
const boundRefusedSettings = `{"task":"01child","usable":true,"deliverable":false,"boundRole":"child","rolePolicy":"unresolved","roleFinding":{"code":"role_policy_unconfigured","detail":"the launch declaration and the environment name two files"},"settings":{"sandbox":{"type":"readOnly","networkAccess":false},"approvalPolicy":"never","cwd":"/w","runtimeWorkspaceRoots":["/w"],"model":"m","reasoningEffort":"xhigh"}}`

// uncitedSettings is a record that cites no role and is bound to none.
const uncitedSettings = `{"task":"01child","usable":true,"deliverable":true,"rolePolicy":"unresolved","settings":{"sandbox":{"type":"readOnly","networkAccess":false},"approvalPolicy":"never","cwd":"/w","runtimeWorkspaceRoots":["/w"],"model":"m","reasoningEffort":"xhigh"}}`

// citedUnboundSettings is a record that cites the child role and is bound to none, as settings-show
// answers it: no role finding and no digest.
const citedUnboundSettings = `{"task":"01child","usable":true,"deliverable":true,"citedRole":"child","rolePolicy":"unresolved","settings":{"sandbox":{"type":"readOnly","networkAccess":false},"approvalPolicy":"never","cwd":"/w","runtimeWorkspaceRoots":["/w"],"model":"m","reasoningEffort":"xhigh"}}`

func TestResumeOfAnUnboundCitingChildReadsTheLaunchDeclaration(t *testing.T) {
	dir := t.TempDir()
	declared, other := filepath.Join(dir, "declared.json"), filepath.Join(dir, "other.json")
	for path, limit := range map[string]string{declared: "300000", other: "550000"} {
		body := `{"roles": {"child": {"model": "m", "reasoningEffort": "xhigh", "autoCompactTokenLimit": ` + limit + `}}}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record := func(path string) string {
		raw, _ := json.Marshal(map[string]any{"schemaVersion": 1, "path": path, "declaredAt": "2026-10-15T00:00:00Z", "declaredBy": "test"})
		return string(raw)
	}
	for _, tc := range []struct {
		name, answer, declaration, env, configured string
		refused                                    string // the detail a refusal must carry; "" for a resume
		limit                                      float64
	}{
		{name: "unbound citing child, declaration contradicts the environment", answer: citedUnboundSettings, declaration: record(declared), env: other, refused: "launch declaration"},
		{name: "unbound citing child, declaration unreadable", answer: citedUnboundSettings, declaration: `{"schemaVersion": 1, "path": `, env: other, refused: "launch-policy.json"},
		{name: "unbound citing child, declaration unreadable, environment unset", answer: citedUnboundSettings, declaration: `{"schemaVersion": 1, "path": `, configured: other, refused: "launch-policy.json"},
		{name: "bound child, declaration contradicts the environment: the gate's own refusal", answer: boundRefusedSettings, declaration: record(declared), env: other, refused: "role_policy_unconfigured"},
		{name: "child citing no role, declaration contradicts the environment", answer: uncitedSettings, declaration: record(declared), env: other},
		{name: "unbound citing child, declaration agrees with the environment", answer: citedUnboundSettings, declaration: record(declared), env: declared, limit: 300000},
		{name: "unbound citing child, no declaration, environment set", answer: citedUnboundSettings, env: other, limit: 550000},
		{name: "unbound citing child, no declaration, configuration file only", answer: citedUnboundSettings, configured: declared, limit: 300000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := resumeHost(t, "notLoaded")
			exe, _ := resumeRelayScript(t, resumeTestAssignment, tc.answer, 0)
			e, _, _ := resumeEnv(t, exe)
			t.Setenv(execution.EnvPolicy, tc.env)
			state := filepath.Join(t.TempDir(), "relay-store")
			if err := os.Mkdir(state, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.declaration != "" {
				if err := os.WriteFile(filepath.Join(state, "launch-policy.json"), []byte(tc.declaration), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := resumeConfig(host, "alpha")
			cfg.Relay.State = state
			cfg.Bridge.ExecutionPolicy = tc.configured
			_, err := resumeRun(context.Background(), e, cfg, resumeOptions{relationship: "rel-1", message: "m"})
			requests := resumeHostRequests(host)
			if tc.refused != "" {
				var failure *resumeFailure
				if !errors.As(err, &failure) || failure.Reason != resumeSettingsUnavailable || !strings.Contains(failure.Detail, tc.refused) {
					t.Fatalf("want a settings_unavailable refusal naming %q, got %v", tc.refused, err)
				}
				for _, request := range requests {
					if request.Method == "thread/resume" || request.Method == "turn/start" {
						t.Fatalf("the refused resume sent %s", request.Method)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("want a resume, got %v", err)
			}
			var resume map[string]any
			if err := json.Unmarshal(requests[1].Params, &resume); err != nil {
				t.Fatal(err)
			}
			config, _ := resume["config"].(map[string]any)
			got, present := config["model_auto_compact_token_limit"]
			if tc.limit == 0 && present || tc.limit != 0 && got != tc.limit {
				t.Fatalf("the resume config carries limit %v (present %v), want %v", got, present, tc.limit)
			}
		})
	}
}

// CRW-1506 (DI P1, candidate 6): the launch record is readable but names a policy file that cannot be
// read (missing). The resolver answers source "record", state "unreadable"; settings-show of an
// unbound citing child carries no digest. The environment is unset and manage.bridge.execution_policy
// names another readable file. A daemon launched on that state reads the declared file and nothing
// else, so the limit cannot be established from it, and resolving it from the configured file instead
// would send another file's limit: the resume is refused naming the declared file.
func TestDifvR6DeclaredFileUnreadableUnboundCitingChild(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "other.json")
	if err := os.WriteFile(other, []byte(`{"roles": {"child": {"model": "m", "reasoningEffort": "xhigh", "autoCompactTokenLimit": 550000}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "declared-missing.json")
	raw, _ := json.Marshal(map[string]any{"schemaVersion": 1, "path": missing, "declaredAt": "2026-10-15T00:00:00Z", "declaredBy": "test"})
	host := resumeHost(t, "notLoaded")
	exe, _ := resumeRelayScript(t, resumeTestAssignment, citedUnboundSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	t.Setenv(execution.EnvPolicy, "")
	state := filepath.Join(t.TempDir(), "relay-store")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "launch-policy.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := resumeConfig(host, "alpha")
	cfg.Relay.State = state
	cfg.Bridge.ExecutionPolicy = other
	_, err := resumeRun(context.Background(), e, cfg, resumeOptions{relationship: "rel-1", message: "m"})
	var failure *resumeFailure
	if !errors.As(err, &failure) || failure.Reason != resumeSettingsUnavailable || !strings.Contains(failure.Detail, missing) {
		requests := resumeHostRequests(host)
		limit := any(nil)
		if len(requests) > 1 {
			var resume map[string]any
			_ = json.Unmarshal(requests[1].Params, &resume)
			config, _ := resume["config"].(map[string]any)
			limit = config["model_auto_compact_token_limit"]
		}
		t.Fatalf("want a settings_unavailable refusal naming the declared file %s, got %v (resume limit %v from %s)", missing, err, limit, other)
	}
	for _, request := range resumeHostRequests(host) {
		if request.Method == "thread/resume" || request.Method == "turn/start" {
			t.Fatalf("the refused resume sent %s", request.Method)
		}
	}
}

// CRW-1506: an unbound citing child under a readable declaration takes its limit from the declared
// file, the one a daemon launched on that state reads, and not from manage.bridge.execution_policy
// when the environment is unset and the configuration names another readable file.
func TestResumeUnboundCitingChildTakesTheDeclaredFileNotTheConfig(t *testing.T) {
	dir := t.TempDir()
	declared, configured := filepath.Join(dir, "declared.json"), filepath.Join(dir, "configured.json")
	for path, limit := range map[string]string{declared: "300000", configured: "550000"} {
		body := `{"roles": {"child": {"model": "m", "reasoningEffort": "xhigh", "autoCompactTokenLimit": ` + limit + `}}}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(map[string]any{"schemaVersion": 1, "path": declared, "declaredAt": "2026-10-15T00:00:00Z", "declaredBy": "test"})
	host := resumeHost(t, "notLoaded")
	exe, _ := resumeRelayScript(t, resumeTestAssignment, citedUnboundSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	t.Setenv(execution.EnvPolicy, "")
	state := filepath.Join(t.TempDir(), "relay-store")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "launch-policy.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := resumeConfig(host, "alpha")
	cfg.Relay.State = state
	cfg.Bridge.ExecutionPolicy = configured
	if _, err := resumeRun(context.Background(), e, cfg, resumeOptions{relationship: "rel-1", message: "m"}); err != nil {
		t.Fatalf("want a resume, got %v", err)
	}
	requests := resumeHostRequests(host)
	if len(requests) < 2 || requests[1].Method != "thread/resume" {
		t.Fatalf("want thread/resume as the second request, got %v", requests)
	}
	var resume map[string]any
	if err := json.Unmarshal(requests[1].Params, &resume); err != nil {
		t.Fatal(err)
	}
	config, _ := resume["config"].(map[string]any)
	if got := config["model_auto_compact_token_limit"]; got != float64(300000) {
		t.Fatalf("the resume config carries limit %v, want 300000 from the declared file %s (not %s)", got, declared, configured)
	}
}

// CRW-1506: manage.relay.state written with a leading ~ is the directory the relay commands select
// after expanding it, and the launch declaration is read there, not under a literal "~" path.
func TestResumeUnboundCitingChildReadsTheDeclarationUnderAHomeRelativeState(t *testing.T) {
	dir := t.TempDir()
	declared, configured := filepath.Join(dir, "declared.json"), filepath.Join(dir, "configured.json")
	for path, limit := range map[string]string{declared: "300000", configured: "550000"} {
		body := `{"roles": {"child": {"model": "m", "reasoningEffort": "xhigh", "autoCompactTokenLimit": ` + limit + `}}}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(map[string]any{"schemaVersion": 1, "path": declared, "declaredAt": "2026-10-15T00:00:00Z", "declaredBy": "test"})
	host := resumeHost(t, "notLoaded")
	exe, _ := resumeRelayScript(t, resumeTestAssignment, citedUnboundSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	t.Setenv(execution.EnvPolicy, "")
	state := filepath.Join(os.Getenv("HOME"), "relay-store")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "launch-policy.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := resumeConfig(host, "alpha")
	cfg.Relay.State = "~/relay-store"
	cfg.Bridge.ExecutionPolicy = configured
	if _, err := resumeRun(context.Background(), e, cfg, resumeOptions{relationship: "rel-1", message: "m"}); err != nil {
		t.Fatalf("want a resume, got %v", err)
	}
	requests := resumeHostRequests(host)
	if len(requests) < 2 || requests[1].Method != "thread/resume" {
		t.Fatalf("want thread/resume as the second request, got %v", requests)
	}
	var resume map[string]any
	if err := json.Unmarshal(requests[1].Params, &resume); err != nil {
		t.Fatal(err)
	}
	config, _ := resume["config"].(map[string]any)
	if got := config["model_auto_compact_token_limit"]; got != float64(300000) {
		t.Fatalf("the resume config carries limit %v, want 300000 from the file declared under ~/relay-store", got)
	}
}
