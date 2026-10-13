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
	// No store yet: the summary is the snapshot's (four undecided roles, the two compiled defaults), never the file's pairs.
	section := pyjson.Map(capabilitiesWith(t, policy)["agentRoles"])
	if strings.Contains(pyjson.Text(section["source"]), "policy.json") || len(section["roles"].([]any)) != 6 {
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
	if byRole["executor"]["model"] != "anthropic/claude-haiku-5" || byRole["executor"]["reasoningEffort"] != "xhigh" || byRole["reviewer-final"]["model"] != "gpt-6.1-sol" || byRole["reviewer-final"]["state"] != "compiled_default" || byRole["architect"]["state"] != "undecided" {
		t.Fatalf("roles %v", byRole)
	}
	if len(section["aliases"].([]any)) != 6 || len(section["warnings"].([]any)) == 0 {
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
