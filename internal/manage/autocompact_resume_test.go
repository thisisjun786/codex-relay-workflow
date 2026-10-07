package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

// autoCompactResumePolicy is the child pair child-resume must read the limit from. The command
// resumes a child the relay recorded, and the record cannot hold this value (the host never reports
// it back), so the limit is resolved from the policy by the pair the record states.
func autoCompactResumePolicy(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "execution-policy.json")
	body := "{\"roles\": {\"child\": {\"model\": \"m\", \"reasoningEffort\": \"xhigh\", \"autoCompactTokenLimit\": 550000}}}"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A child the relay recorded is resumed by this command too, and it is the same child whose
// provider cuts the stream above its window. The resume it builds has to carry the pair's limit,
// or reloading a capped child through this command would quietly drop the protection and report
// success without recording that the limit went out.
func TestResumeCarriesTheRecordedPairsAutoCompactLimit(t *testing.T) {
	host := resumeHost(t, "notLoaded")
	// settings-show answers the role the record cites beside the settings; the limit is resolved
	// from that role's pair.
	exe, _ := resumeRelayScript(t, resumeTestAssignment, autoCompactResumeSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	t.Setenv(execution.EnvPolicy, autoCompactResumePolicy(t))
	cfg := resumeConfig(host, "alpha")
	cfg.Bridge.ExecutionPolicy = os.Getenv(execution.EnvPolicy)
	if _, err := resumeRun(context.Background(), e, cfg, resumeOptions{relationship: "rel-1", message: "m"}); err != nil {
		t.Fatal(err)
	}
	var resume map[string]any
	if err := json.Unmarshal(resumeHostRequests(host)[1].Params, &resume); err != nil {
		t.Fatal(err)
	}
	config, _ := resume["config"].(map[string]any)
	if config["model_auto_compact_token_limit"] != float64(550000) {
		t.Fatalf("the resume config does not carry the pair's limit: %v", config)
	}
}

// autoCompactResumeSettings is resumeTestSettings with the role the record cites, which is what the
// relay's settings-show answer carries beside the settings.
const autoCompactResumeSettings = `{"task":"01child","usable":true,"citedRole":"child","settings":{"sandbox":{"type":"readOnly","networkAccess":false},"approvalPolicy":"never","cwd":"/w","runtimeWorkspaceRoots":["/w"],"model":"m","reasoningEffort":"xhigh"}}`

// A pair that declares no limit adds no key: the command behaves exactly as it did before.
func TestResumeWithoutAPolicyLimitSendsNoKey(t *testing.T) {
	host := resumeHost(t, "notLoaded")
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	path := filepath.Join(t.TempDir(), "execution-policy.json")
	if err := os.WriteFile(path, []byte("{\"roles\": {\"child\": {\"model\": \"m\", \"reasoningEffort\": \"xhigh\"}}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(execution.EnvPolicy, path)
	cfg := resumeConfig(host, "alpha")
	cfg.Bridge.ExecutionPolicy = path
	if _, err := resumeRun(context.Background(), e, cfg, resumeOptions{relationship: "rel-1", message: "m"}); err != nil {
		t.Fatal(err)
	}
	var resume map[string]any
	if err := json.Unmarshal(resumeHostRequests(host)[1].Params, &resume); err != nil {
		t.Fatal(err)
	}
	config, _ := resume["config"].(map[string]any)
	if _, present := config["model_auto_compact_token_limit"]; present {
		t.Fatalf("a pair without a limit sent one: %v", config)
	}
}
