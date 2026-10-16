package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1486/S2 (finding 3): the registry CLI reads the policy the daemon launches with. A service
// whose execution policy is declared (launch-policy.json in its state directory) is checked against
// that file by settings-show, settings-record and linkage-bind even when the variable is unset in
// the process that asks; a declaration that cannot be read, or one the variable contradicts, is the
// existing unresolved answer (role_policy_unconfigured) with the resolver's reason in the detail.
// With no declaration the answer is the one this process's environment gives, byte for byte.

const launchPolicyTask = "01launchparent"

type launchPolicyStore struct {
	t      *testing.T
	home   string
	state  string
	policy string
	cwd    string
}

// newLaunchPolicyStore is a store holding launchPolicyTask bound as a parent with settings the
// policy at policy declares for that role, set up with the variable naming policy and left with
// the variable unset and this process's role-policy snapshot dropped.
func newLaunchPolicyStore(t *testing.T) *launchPolicyStore {
	t.Helper()
	home := tempHome(t)
	state := ownerOnlyState(t, home, "launch-policy")
	testsupport.Create(t, filepath.Join(state, "relay.sqlite3"), "", "go")
	s := &launchPolicyStore{t: t, home: home, state: state, policy: filepath.Join(home, "policy.json"), cwd: filepath.Join(home, "work")}
	s.writePolicy(s.policy, "anthropic/claude-opus-5")
	if err := os.MkdirAll(s.cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(execution.EnvPolicy, s.policy)
	registry.ResetRolePolicySnapshot()
	t.Cleanup(registry.ResetRolePolicySnapshot)
	s.must("linkage-bind", "--role", "parent", "--scope", "PROJ-1", "--task", launchPolicyTask, "--host", "host")
	s.must("settings-record", "--task", launchPolicyTask, "--settings", s.settings("anthropic/claude-opus-5"))
	os.Unsetenv(execution.EnvPolicy)
	registry.ResetRolePolicySnapshot()
	return s
}

func (s *launchPolicyStore) writePolicy(path, parentModel string) {
	s.t.Helper()
	text := `{"roles": {"supervisor": {"expectation": "record"}, "parent": {"model": "` + parentModel + `", "reasoningEffort": "xhigh"}, "child": {"model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"}}}`
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// settings is the record of a task working in the store's cwd on model at reasoning effort xhigh.
func (s *launchPolicyStore) settings(model string) string {
	cwd := s.cwd
	return `{"sandbox": {"type": "workspaceWrite", "writableRoots": ["` + cwd + `"], "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}, ` +
		`"approvalPolicy": "never", "cwd": "` + cwd + `", "runtimeWorkspaceRoots": ["` + cwd + `"], "model": "` + model + `", "reasoningEffort": "xhigh", ` +
		`"environments": [{"environmentId": "local", "cwd": "` + cwd + `", "runtimeWorkspaceRoots": ["` + cwd + `"]}]}`
}

func (s *launchPolicyStore) declare(text string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.state, "launch-policy.json"), []byte(text), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

func (s *launchPolicyStore) declarePath(path string) {
	s.declare(`{"schemaVersion": 1, "path": "` + path + `", "declaredAt": "2026-10-15T00:00:00Z", "declaredBy": "cli"}`)
}

// run is one registry CLI command against the store in a fresh process snapshot.
func (s *launchPolicyStore) run(argv ...string) answer {
	s.t.Helper()
	registry.ResetRolePolicySnapshot()
	return goCLI(s.t, append([]string{"--state", s.state}, argv...)...)
}

func (s *launchPolicyStore) must(argv ...string) map[string]any {
	s.t.Helper()
	got := goCLI(s.t, append([]string{"--state", s.state}, argv...)...)
	if got.code != 0 {
		s.t.Fatalf("%v: exit %d %s", argv, got.code, got.stdout)
	}
	return object(s.t, got.stdout)
}

// show is settings-show for the bound task in a fresh process snapshot.
func (s *launchPolicyStore) show() map[string]any {
	s.t.Helper()
	registry.ResetRolePolicySnapshot()
	return s.must("settings-show", "--task", launchPolicyTask)
}

func roleFindingOf(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	finding, _ := answer["roleFinding"].(map[string]any)
	if finding == nil {
		t.Fatalf("settings-show has no roleFinding: %v", answer)
	}
	return finding
}

func TestSettingsShowReadsTheDeclaredLaunchPolicy(t *testing.T) {
	s := newLaunchPolicyStore(t)
	s.declarePath(s.policy)
	want := registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: s.policy}).Digest()
	answer := s.show()
	if answer["rolePolicy"] != "declared" || answer["rolePolicyDigest"] != want || answer["deliverable"] != true || answer["roleFinding"] != nil {
		t.Fatalf("settings-show with the variable unset and %s declared: rolePolicy %v digest %v (want %s) deliverable %v roleFinding %v",
			s.policy, answer["rolePolicy"], answer["rolePolicyDigest"], want, answer["deliverable"], answer["roleFinding"])
	}
	// The same file named by the variable as well is one source, not a conflict.
	t.Setenv(execution.EnvPolicy, filepath.Join(s.home, ".", "policy.json"))
	if again := s.show(); again["rolePolicy"] != "declared" || again["deliverable"] != true {
		t.Fatalf("the declaration and the variable name one file: %v", again)
	}
}

func TestSettingsShowRefusesAConflictingLaunchPolicy(t *testing.T) {
	s := newLaunchPolicyStore(t)
	other := filepath.Join(s.home, "other-policy.json")
	s.writePolicy(other, "anthropic/claude-opus-5")
	s.declarePath(s.policy)
	t.Setenv(execution.EnvPolicy, other)
	answer := s.show()
	finding := roleFindingOf(t, answer)
	detail, _ := finding["detail"].(string)
	if finding["code"] != "role_policy_unconfigured" || answer["rolePolicy"] != "unresolved" || answer["deliverable"] != false ||
		!strings.Contains(detail, s.policy) || !strings.Contains(detail, other) {
		t.Fatalf("a declaration and a variable naming two files: rolePolicy %v deliverable %v roleFinding %v", answer["rolePolicy"], answer["deliverable"], finding)
	}

	os.Unsetenv(execution.EnvPolicy)
	s.declare(`{"schemaVersion": 1, "path": `)
	answer = s.show()
	finding = roleFindingOf(t, answer)
	detail, _ = finding["detail"].(string)
	if finding["code"] != "role_policy_unconfigured" || answer["rolePolicy"] != "unresolved" || answer["deliverable"] != false ||
		!strings.Contains(detail, filepath.Join(s.state, "launch-policy.json")) {
		t.Fatalf("an unreadable declaration: rolePolicy %v deliverable %v roleFinding %v", answer["rolePolicy"], answer["deliverable"], finding)
	}
	// An unreadable declaration is not covered by the variable either.
	t.Setenv(execution.EnvPolicy, s.policy)
	if again := s.show(); again["rolePolicy"] != "unresolved" || roleFindingOf(t, again)["code"] != "role_policy_unconfigured" {
		t.Fatalf("the variable covered an unreadable declaration: %v", again)
	}
}

// Pin: with no declaration the answer is the environment's, as it was before the declaration was read.
func TestSettingsShowWithNoLaunchDeclarationAnswersFromTheEnvironment(t *testing.T) {
	s := newLaunchPolicyStore(t)
	t.Setenv(execution.EnvPolicy, s.policy)
	want := registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: s.policy}).Digest()
	if answer := s.show(); answer["rolePolicy"] != "declared" || answer["rolePolicyDigest"] != want || answer["deliverable"] != true {
		t.Fatalf("the variable alone: %v", answer)
	}
	os.Unsetenv(execution.EnvPolicy)
	answer := s.show()
	finding := roleFindingOf(t, answer)
	if finding["code"] != "role_policy_unconfigured" || answer["rolePolicy"] != "unresolved" ||
		finding["detail"] != execution.EnvPolicy+" is not set in this process, so no role policy can be read" {
		t.Fatalf("neither a declaration nor the variable: %v", finding)
	}
}

// CRW-1486 A4: settings-record checks the pair against the declared file with the variable unset. The
// declared file authorizes the parent role on another model than the one the bound task was recorded
// with, so re-recording that pair is refused and the declared pair is recorded. A command that read
// this process's environment instead (unset: no role policy) would accept either.
func TestSettingsRecordChecksTheDeclaredLaunchPolicy(t *testing.T) {
	s := newLaunchPolicyStore(t)
	declared := filepath.Join(s.home, "declared-policy.json")
	s.writePolicy(declared, "anthropic/claude-sonnet-5")
	s.declarePath(declared)
	refused := s.run("settings-record", "--task", launchPolicyTask, "--settings", s.settings("anthropic/claude-opus-5"))
	if refused.code == 0 || object(t, refused.stdout)["reason"] != "role_binding_mismatch" {
		t.Fatalf("settings-record of a pair %s does not authorize for the bound role: exit %d %s", declared, refused.code, refused.stdout)
	}
	if got := s.run("settings-record", "--task", launchPolicyTask, "--settings", s.settings("anthropic/claude-sonnet-5")); got.code != 0 {
		t.Fatalf("settings-record of the pair %s authorizes: exit %d %s", declared, got.code, got.stdout)
	}
	want := registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: declared}).Digest()
	if answer := s.show(); answer["rolePolicyDigest"] != want || answer["deliverable"] != true {
		t.Fatalf("settings-show after recording the declared pair: %v", answer)
	}
}

// CRW-1486 A4: linkage-bind checks the task's recorded settings against the declared file with the
// variable unset. A task recorded on a pair the declared file does not authorize for the parent role
// is not bound as one; a task recorded on the declared pair is.
func TestLinkageBindChecksTheDeclaredLaunchPolicy(t *testing.T) {
	s := newLaunchPolicyStore(t)
	declared := filepath.Join(s.home, "declared-policy.json")
	s.writePolicy(declared, "anthropic/claude-sonnet-5")
	s.declarePath(declared)
	const opus, sonnet = "01launchopus", "01launchsonnet"
	s.must("settings-record", "--task", opus, "--settings", s.settings("anthropic/claude-opus-5"))
	s.must("settings-record", "--task", sonnet, "--settings", s.settings("anthropic/claude-sonnet-5"))
	refused := s.run("linkage-bind", "--role", "parent", "--scope", "PROJ-2", "--task", opus, "--host", "host")
	if refused.code == 0 || object(t, refused.stdout)["reason"] != "role_binding_mismatch" {
		t.Fatalf("linkage-bind of a task on a pair %s does not authorize for the role: exit %d %s", declared, refused.code, refused.stdout)
	}
	if got := s.run("linkage-bind", "--role", "parent", "--scope", "PROJ-3", "--task", sonnet, "--host", "host"); got.code != 0 {
		t.Fatalf("linkage-bind of a task on the pair %s authorizes: exit %d %s", declared, got.code, got.stdout)
	}
}
