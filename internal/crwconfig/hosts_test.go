package crwconfig

import (
	"encoding/json"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutionHostRequiresApprovedCanonicalProfiles(t *testing.T) {
	h := ExecutionHost{ID: "host", Transport: "ssh", Endpoint: "approved-alias", KnownHosts: "/synthetic/known_hosts", Root: "/synthetic/worker", Repositories: map[string]string{"owner/repo": "/synthetic/repo"}, Kind: executor.Devin, CapabilityRevision: "cap-1", LaunchProfile: strings.Repeat("a", 64), ToolchainProfile: strings.Repeat("b", 64), AuthProfile: "approved", TrustProfile: "approved", Supervision: "worker-rpc"}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ExecutionHost){func(h *ExecutionHost) { h.Endpoint = "host; touch unwanted" }, func(h *ExecutionHost) { h.TrustProfile = "" }, func(h *ExecutionHost) { h.Supervision = "unconfined" }, func(h *ExecutionHost) { h.Repositories = map[string]string{"/checkout/repo": "/synthetic/repo"} }, func(h *ExecutionHost) { h.Repositories = map[string]string{"../repo": "/synthetic/repo"} }} {
		bad := h
		change(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid enrollment accepted: %+v", bad)
		}
	}
	root := t.TempDir()
	data, _ := json.Marshal(map[string]any{"schema": Schema, "execution": map[string]any{"hosts": []ExecutionHost{h}}})
	file := filepath.Join(root, "config.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(func(key string) string {
		if key == "HOME" {
			return root
		}
		return ""
	}, file)
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := cfg.ExecutionHosts()
	if err != nil || len(hosts) != 1 {
		t.Fatalf("%+v %v", hosts, err)
	}
}
