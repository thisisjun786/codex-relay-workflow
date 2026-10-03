package registry

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"os"
	"path/filepath"
	"testing"
)

func Test29BridgeUsesPublishedRoleSnapshot(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "policy.json")
	original := []byte(`{"roles":{"parent":{"model":"first-model","reasoningEffort":"high"}}}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	p := ResolveRolePolicy(map[string]string{execution.EnvPolicy: path})
	if !p.Declared {
		t.Fatal(p.Detail)
	}
	if err := os.WriteFile(path, []byte(`{"roles":{"parent":{"model":"second-model","reasoningEffort":"high"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	transport := p.BridgePolicy()
	if transport.Summary()["digest"] != p.Digest() {
		t.Fatal("transport changed the published snapshot")
	}
	if _, err := transport.Authorize(execution.Input{Role: "parent", Model: "first-model", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.Authorize(execution.Input{Role: "parent", Model: "second-model", Effort: "high"}); err == nil {
		t.Fatal("transport reread the changed file")
	}
}
