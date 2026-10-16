package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// CRW-1330 (CRW-1316 S1): get_capabilities describes the flow's agent roles from the helper role store, the snapshot the flow
// itself runs on, and no longer from the execution policy's agent-roles section.

func capabilitiesWith(t *testing.T, policy execution.Policy) map[string]any {
	t.Helper()
	server := fakehost.Start(t)
	client, err := appserver.Dial(context.Background(), server.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	report, err := New(client, nil, policy).GetCapabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return pyjson.Map(report["executionPolicy"])
}

func TestCRW1330_CapabilitiesAgentRolesComeFromTheRoleHelperSnapshot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "crw")
	t.Setenv("CRW_HOME", dir)
	fileRoles := `{"agent-roles": {
  "architect": {"model": "claude-file", "reasoningEffort": "low"}, "executor": {"model": "claude-file", "reasoningEffort": "low"},
  "executor-t1": {"model": "claude-file", "reasoningEffort": "low"}, "reviewer": {"model": "gpt-file", "reasoningEffort": "low"},
  "reviewer-final": {"model": "gpt-file", "reasoningEffort": "low"}, "explorer": {"model": "claude-file", "reasoningEffort": "low"}}}`
	policy, err := execution.FromBytes([]byte(fileRoles), "policy.json")
	if err != nil {
		t.Fatal(err)
	}
	// No store yet: the summary is the snapshot's (four undecided roles, the compiled defaults of the flow-only keys), never the
	// file's pairs. It describes every role of the snapshot (role.FlowRoles; the critic joins it, CRW-1434), whatever the file
	// declares: this file was written before the critic and still names reviewer-final, a retired role key since CRW-1450.
	section := pyjson.Map(capabilitiesWith(t, policy)["agentRoles"])
	if strings.Contains(pyjson.Text(section["source"]), "policy.json") || len(section["roles"].([]any)) != len(role.FlowRoles()) {
		t.Fatalf("agentRoles %v", section)
	}
	for _, entry := range section["roles"].([]any) {
		if strings.Contains(pyjson.Text(pyjson.Map(entry)["model"]), "file") {
			t.Fatalf("a pair from the file: %v", entry)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, role.StoreFile)
	if err := os.WriteFile(store, []byte(`{"roles": {"executor": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "xhigh"}, "reviewer": {"mode": "model", "model": "gpt-6.1-sol", "effort": "high"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	section = pyjson.Map(capabilitiesWith(t, policy)["agentRoles"])
	roles := section["roles"].([]any)
	byRole := map[string]map[string]any{}
	for _, entry := range roles {
		byRole[pyjson.Text(pyjson.Map(entry)["role"])] = pyjson.Map(entry)
	}
	if section["source"] != store || section["digest"] != role.ReadFlowSnapshot(host.LookupEnv(os.LookupEnv)).Digest() || pyjson.Text(pyjson.Map(roles[0])["role"]) != "architect" {
		t.Fatalf("agentRoles %v", section)
	}
	if byRole["executor"]["model"] != "anthropic/claude-haiku-5" || byRole["executor"]["reasoningEffort"] != "xhigh" || byRole["critic"]["model"] != "gpt-6.1-sol" || byRole["critic"]["state"] != "compiled_default" || byRole["architect"]["state"] != "undecided" {
		t.Fatalf("roles %v", byRole)
	}
	if _, listed := byRole["reviewer-final"]; listed {
		t.Fatalf("the retired reviewer-final key is described as a role: %v", byRole)
	}
	// Every role described carries the design's model aliases, the critic's those of the gpt reviewers (CRW-1434).
	for name, entry := range byRole {
		want := execution.DefaultModelAliases(name)
		got, _ := entry["aliases"].([]any)
		if len(want) == 0 || len(got) != len(want) || got[0] != want[0] {
			t.Fatalf("role %s aliases %v, want %v", name, got, want)
		}
	}
	if critic := execution.DefaultModelAliases(execution.AgentCritic); len(critic) != 1 || critic[0] != "ocx-gpt-6-1-sol" {
		t.Fatalf("critic aliases %v", critic)
	}
	// The code's alias table: five rows, none for the explorer or the critic (named by agentType).
	if len(section["aliases"].([]any)) != len(execution.DefaultAliasRows()) || len(section["aliases"].([]any)) != 5 || len(section["warnings"].([]any)) == 0 {
		t.Fatalf("agentRoles %v", section)
	}
	// The relay's policy description (the worker policy receipt) is the file's, unchanged.
	if file := pyjson.Map(policy.Summary()["agentRoles"]); file == nil {
		t.Fatal("the policy's own summary lost its section")
	}
}

func TestCRW1330_CapabilitiesWithNoStoreAndNoSectionDescribeNoAgentRoles(t *testing.T) {
	t.Setenv("CRW_HOME", filepath.Join(t.TempDir(), "crw"))
	if section, listed := capabilitiesWith(t, execution.Policy{})["agentRoles"]; listed {
		t.Fatalf("an unconfigured host describes agent roles: %v", section)
	}
}

// CRW-1330 (evaluation 5a1a64c2 d1): a policy whose agent-roles does not parse loads with the section ignored; get_capabilities
// still describes the flow's roles from the snapshot (the file declares the section), with no pair of the file.
func TestCRW1330_CapabilitiesDescribeTheSnapshotBesideAMalformedSection(t *testing.T) {
	t.Setenv("CRW_HOME", filepath.Join(t.TempDir(), "crw"))
	policy, err := execution.FromBytes([]byte(`{"agent-roles": {"architect": {"model": "claude-file", "reasoningEffort": "low"}}}`), "policy.json")
	if err != nil {
		t.Fatalf("a malformed agent-roles section refuses the policy: %v", err)
	}
	section := pyjson.Map(capabilitiesWith(t, policy)["agentRoles"])
	if section == nil || len(section["roles"].([]any)) != len(role.FlowRoles()) || strings.Contains(pyjson.Text(section["source"]), "policy.json") {
		t.Fatalf("agentRoles %v", section)
	}
	for _, entry := range section["roles"].([]any) {
		if strings.Contains(pyjson.Text(pyjson.Map(entry)["model"]), "file") {
			t.Fatalf("a pair from the file: %v", entry)
		}
	}
}
