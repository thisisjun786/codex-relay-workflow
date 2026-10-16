package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// CRW-1330 (CRW-1316 S1): the role table of every production flow command is built from the helper role store
// (CRW_HOME/subagents.json), not from the execution policy's agent-roles; the run pins the canonical digest of the six pairs.

func roleStore(executor, reviewer string) string {
	return `{"roles": {
  "architect": {"mode": "model", "model": "anthropic/claude-opus-5-5", "effort": "xhigh"},
  "executor": {"mode": "model", "model": "` + executor + `", "effort": "xhigh"},
  "executor-t1": {"mode": "model", "model": "anthropic/claude-sonnet-5-5", "effort": "xhigh"},
  "reviewer": {"mode": "model", "model": "` + reviewer + `", "effort": "high"},
  "critic": {"mode": "model", "model": "gpt-6.1-sol", "effort": "xhigh"},
  "explorer": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "high"}}}`
}

// withRoleStore points CRW_HOME at a fresh directory holding text as the store (no file when text is "") and returns the path.
func withRoleStore(t *testing.T, text string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "crw")
	t.Setenv("CRW_HOME", dir)
	path := filepath.Join(dir, role.StoreFile)
	if text != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, text)
	}
	return path
}

func snapshotDigest(t *testing.T) string {
	t.Helper()
	return role.ReadFlowSnapshot(host.LookupEnv(os.LookupEnv)).Digest()
}

func TestCRW1330_LoadPolicyBuildsTheTableFromTheRoleHelper(t *testing.T) {
	store := withRoleStore(t, roleStore("anthropic/claude-haiku-5", "gpt-6.1-sol"))
	// The execution policy still declares agent-roles, on other pairs: the flow does not read them.
	policyPath := filepath.Join(t.TempDir(), "policy.json")
	writeFile(t, policyPath, policyFile("claude-from-the-file", "low"))
	t.Setenv(execution.EnvPolicy, policyPath)
	table, exec, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if table.Source != store || table.Digest != snapshotDigest(t) || len(table.Digest) != 64 {
		t.Fatalf("source %q digest %q", table.Source, table.Digest)
	}
	if _, declared := exec.AgentRoles(); !declared {
		t.Fatal("the execution policy is still returned as loaded")
	}
	want := map[string]struct {
		pair            Pair
		family, sandbox string
	}{
		"architect": {Pair{"anthropic/claude-opus-5-5", "xhigh"}, "architect", SandboxReadOnly}, "executor": {Pair{"anthropic/claude-haiku-5", "xhigh"}, "executor", SandboxWorkspaceWrite},
		"executor-t1": {Pair{"anthropic/claude-sonnet-5-5", "xhigh"}, "executor", SandboxWorkspaceWrite}, "reviewer": {Pair{"gpt-6.1-sol", "high"}, "reviewer", SandboxReadOnly},
		"explorer": {Pair{"anthropic/claude-haiku-5", "high"}, "explorer", SandboxReadOnly},
		"critic":   {Pair{"gpt-6.1-sol", "xhigh"}, "critic", SandboxReadOnly},
	}
	if len(table.Roles) != len(execution.AgentRoleNames) {
		t.Fatalf("roles %+v", table.Roles)
	}
	for name, w := range want {
		spec := table.Roles[name]
		if spec.Pair != w.pair || spec.Family != w.family || spec.Sandbox != w.sandbox || len(spec.ModelAliases) == 0 || spec.AutoCompactTokenLimit != nil {
			t.Errorf("%s: %+v", name, spec)
		}
	}
	// the code's alias table, nothing read from the file
	if len(table.Aliases) != 5 || table.Aliases[0].Key != "opus" || table.Aliases[2].Cause != execution.AliasAmbiguous {
		t.Fatalf("aliases %+v", table.Aliases)
	}
	if joined := strings.Join(table.Warnings, "\n"); !strings.Contains(joined, policyPath) || !strings.Contains(joined, "agent-roles") || !strings.Contains(joined, "no longer reads") {
		t.Fatalf("warnings %q", table.Warnings)
	}
	// Without a policy file the table is the same, with no warning about the file.
	t.Setenv(execution.EnvPolicy, "")
	plain, _, err := LoadPolicy()
	if err != nil || plain.Digest != table.Digest || len(plain.Warnings) != 0 {
		t.Fatalf("%v %+v", err, plain)
	}
	// A policy file that declares no agent-roles is no longer refused: the flow does not need it.
	writeFile(t, policyPath, `{"allowed": [{"model": "m", "efforts": ["high"]}]}`)
	t.Setenv(execution.EnvPolicy, policyPath)
	if _, _, err := LoadPolicy(); err != nil {
		t.Fatalf("a policy without agent-roles: %v", err)
	}
}

func TestCRW1330_PlaceholderTableOnlyUnderTestIsolationWithoutAStore(t *testing.T) {
	withRoleStore(t, "")
	t.Setenv(execution.EnvPolicy, "")
	// testsupport isolates this test binary; with no store the placeholder table applies, as before
	table, _, err := LoadPolicy()
	if err != nil || table.Source != "" || table.Roles["executor"].Pair.Model != "claude-fake-haiku" {
		t.Fatalf("%v %+v", err, table)
	}
	// Outside the isolation (a production process) a missing store is the role helper's reading: four undecided roles and the
	// two compiled defaults (executor-t1, critic), each warned.
	t.Setenv(isolationRootEnv, "")
	table, _, err = LoadPolicy()
	if err != nil || table.Source == "" || table.Roles["explorer"].Pair != (Pair{}) || table.Roles["executor-t1"].Pair != (Pair{"anthropic/claude-sonnet-5-5", "xhigh"}) || table.Roles["critic"].Pair != (Pair{"gpt-6.1-sol", "xhigh"}) || len(table.Warnings) != 6 {
		t.Fatalf("%v %+v", err, table)
	}
}

func TestCRW1330_ReviewerFamilyOverlapIsWarnedAndRecordedAtRunStart(t *testing.T) {
	withRoleStore(t, roleStore("anthropic/claude-haiku-5", "anthropic/claude-sonnet-5"))
	t.Setenv(execution.EnvPolicy, "")
	table, _, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(table.Warnings, "\n")
	if !strings.Contains(joined, "reviewer") || !strings.Contains(joined, "family") || !strings.Contains(joined, "claude") {
		t.Fatalf("warnings %q", table.Warnings)
	}
	// the run records the warnings in its started row; the table resolves calls as before
	h := newHarness(t)
	h.cfg.Policy = table
	res := h.run(`return await agent.call('look', { agentType: 'explorer', label: 'look' })`)
	if res.Status != RunCompleted {
		t.Fatalf("%s %s", res.ErrorKind, res.Error)
	}
	started := h.journal.Events()[0]
	recorded, _ := json.Marshal(started.Data["policy_warnings"])
	if started.Event != "started" || started.Data["policy_digest"] != table.Digest || started.Data["policy_source"] != table.Source || !strings.Contains(string(recorded), "family") {
		t.Fatalf("started %+v", started.Data)
	}
	// a table without warnings writes no policy_warnings member (the placeholder table's runs keep their rows)
	h2 := newHarness(t)
	h2.run(`return 1`)
	if _, ok := h2.journal.Events()[0].Data["policy_warnings"]; ok {
		t.Fatalf("started %+v", h2.journal.Events()[0].Data)
	}
}

func TestCRW1330_ResumeRepinsWhenAPairOfTheSnapshotChanges(t *testing.T) {
	withRoleStore(t, roleStore("anthropic/claude-haiku-5", "gpt-6.1-sol"))
	t.Setenv(execution.EnvPolicy, "")
	old, _, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.cfg.Policy, h.cfg.RunID = old, "run-pin-helper"
	if res := h.run(`return await agent.call('look', { agentType: 'explorer', label: 'a' })`); res.Status != RunCompleted {
		t.Fatal(res.Error)
	}
	repinned := func() (rows []Event) {
		for _, e := range h.journal.Events() {
			if e.Event == EvPolicyRepinned {
				rows = append(rows, e)
			}
		}
		return rows
	}
	// only a prompt override changes: the digest stays and nothing is repinned
	withRoleStore(t, strings.Replace(roleStore("anthropic/claude-haiku-5", "gpt-6.1-sol"), `"effort": "high"}}}`, `"effort": "high", "promptOverride": "p"}}}`, 1))
	same, _, _ := LoadPolicy()
	h.cfg.Policy = same
	h.run(`return 1`)
	if same.Digest != old.Digest || len(repinned()) != 0 {
		t.Fatalf("digest %s -> %s, repins %+v", old.Digest, same.Digest, repinned())
	}
	// a changed pair repins with the old and the new snapshot digest
	withRoleStore(t, roleStore("anthropic/claude-haiku-6", "gpt-6.1-sol"))
	changed, _, _ := LoadPolicy()
	h.cfg.Policy = changed
	h.run(`return 1`)
	rows := repinned()
	if len(rows) != 1 || rows[0].Data["old"] != old.Digest || rows[0].Data["new"] != changed.Digest || old.Digest == changed.Digest {
		t.Fatalf("repins %+v", rows)
	}
}

func TestCRW1330_FlowRolesShowsTheStoreTheDigestThePairsAndTheWarnings(t *testing.T) {
	store := withRoleStore(t, `{"roles": {
  "architect": {"mode": "model", "model": "anthropic/claude-opus-5-5", "effort": "xhigh"},
  "executor": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "xhigh"},
  "reviewer": {"mode": "model", "model": "gpt-6.1-sol", "effort": "high"},
  "explorer": {"mode": "default"}}}`)
	policyPath := filepath.Join(t.TempDir(), "policy.json")
	writeFile(t, policyPath, policyFile("claude-haiku", "xhigh"))
	t.Setenv(execution.EnvPolicy, policyPath)
	code, out, errOut := runFlow(t, "roles", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d %q", code, errOut)
	}
	var report PolicyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Source == nil || *report.Source != store || report.Digest == nil || *report.Digest != snapshotDigest(t) || report.Placeholder || len(report.Roles) != len(execution.AgentRoleNames) {
		t.Fatalf("%+v", report)
	}
	states := map[string]string{}
	for _, r := range report.Roles {
		states[r.Role] = r.State
	}
	if states["executor"] != "store" || states["executor-t1"] != "compiled_default" || states["explorer"] != "undecided" || states["critic"] != "compiled_default" {
		t.Fatalf("states %v", states)
	}
	joined := strings.Join(report.Warnings, "\n")
	for _, want := range []string{"executor-t1", "explorer", "critic", "agent-roles", "no longer reads"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q: %q", want, report.Warnings)
		}
	}
	code, out, _ = runFlow(t, "roles")
	for _, want := range []string{"roles: " + store, "digest: " + snapshotDigest(t), "executor-t1", "compiled default", "warning: "} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
}
