package agent_test

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// CRW-1488 (1209 trial finding 12): a thread the flow starts for a call gets the run's repository tools in its shell, as
// shell_environment_policy.set entries the host merges over its own policy (inherit, exclude and the other set entries stay), and
// its role prompt names them. A call with no tools gets neither.
func TestRepositoryToolsReachThreadShell(t *testing.T) {
	e := newEnv(t)
	tools := &engine.RepositoryTools{
		Env:    []string{"PATH=/opt/go-1.27/bin:/usr/bin", "GOROOT=/opt/go-1.27", "GOTOOLCHAIN=local"},
		Prefix: `PATH=/opt/go-1.27/bin:"$PATH" GOROOT=/opt/go-1.27 GOTOOLCHAIN=local`,
		Facts:  []string{"go: /opt/go-1.27/bin/go (verifier.go_root /opt/go-1.27 in /cfg/config.json), the go crw.verify runs"},
	}
	spec := e.policy.Roles["explorer"]
	if _, err := e.backend.StartThread(context.Background(), engine.ThreadSpec{CallID: "c_tools", Role: "explorer", Pair: spec.Pair, Sandbox: spec.Sandbox,
		Cwd: filepath.Join(e.root, "tools"), Tools: tools}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.backend.StartThread(context.Background(), engine.ThreadSpec{CallID: "c_plain", Role: "explorer", Pair: spec.Pair, Sandbox: spec.Sandbox,
		Cwd: filepath.Join(e.root, "plain")}); err != nil {
		t.Fatal(err)
	}
	starts := e.requests("thread/start")
	if len(starts) != 2 {
		t.Fatalf("%d thread/start requests, want 2", len(starts))
	}
	config, _ := starts[0].Params["config"].(map[string]any)
	policy, _ := config["shell_environment_policy"].(map[string]any)
	want := map[string]any{"set": map[string]any{"PATH": "/opt/go-1.27/bin:/usr/bin", "GOROOT": "/opt/go-1.27", "GOTOOLCHAIN": "local"}}
	if !reflect.DeepEqual(policy, want) {
		t.Errorf("shell_environment_policy %v, want %v (config %v)", policy, want, config)
	}
	if config["model_reasoning_effort"] != spec.Pair.Effort {
		t.Errorf("the tools replaced the rest of the config: %v", config)
	}
	dev, _ := starts[0].Params["developerInstructions"].(string)
	if !strings.Contains(dev, "## Repository tools") || !strings.Contains(dev, tools.Facts[0]) {
		t.Errorf("the role prompt does not name the tools:\n%s", dev)
	}
	plainConfig, _ := starts[1].Params["config"].(map[string]any)
	if _, has := plainConfig["shell_environment_policy"]; has {
		t.Errorf("a call with no tools got a shell environment: %v", plainConfig)
	}
	if dev, _ := starts[1].Params["developerInstructions"].(string); strings.Contains(dev, "Repository tools") {
		t.Errorf("a call with no tools names some in its prompt")
	}
}
