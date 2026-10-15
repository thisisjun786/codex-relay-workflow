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

// The gate that authorized the record read the policy the settings-show subprocess inherited from
// the environment, so that is the policy this command has to resolve the limit from. Reading only
// manage.bridge.execution_policy would find nothing under the supported environment-only
// configuration, while the gate had accepted the capped pair.
func TestResumeResolvesTheLimitFromTheGatePolicyEnvironment(t *testing.T) {
	host := resumeHost(t, "notLoaded")
	exe, _ := resumeRelayScript(t, resumeTestAssignment, autoCompactResumeSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	t.Setenv(execution.EnvPolicy, autoCompactResumePolicy(t))
	cfg := resumeConfig(host, "alpha")
	cfg.Bridge.ExecutionPolicy = ""
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

// The host never reports the value back, so the report has to say what went out and that the host
// cannot confirm it: an operator reading a bare ok cannot tell a capped reload from an uncapped one.
func TestResumeReportsTheLimitItSentAsUnobservable(t *testing.T) {
	host := resumeHost(t, "notLoaded")
	exe, _ := resumeRelayScript(t, resumeTestAssignment, autoCompactResumeSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	t.Setenv(execution.EnvPolicy, autoCompactResumePolicy(t))
	cfg := resumeConfig(host, "alpha")
	cfg.Bridge.ExecutionPolicy = os.Getenv(execution.EnvPolicy)
	report, err := resumeRun(context.Background(), e, cfg, resumeOptions{relationship: "rel-1", message: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if report.AutoCompactTokenLimit == nil || *report.AutoCompactTokenLimit != 550000 {
		t.Fatalf("the report does not carry the limit it sent: %+v", report)
	}
	if !report.AutoCompactUnobservable {
		t.Fatalf("the report does not mark the limit unobservable: %+v", report)
	}
}

// CRW-1000 d1: a dry run sends nothing, so it must not report a limit as sent. The fields that say what
// went out (autoCompactTokenLimit, autoCompactUnobservable) are left empty, and the limit the real run
// would send is stated in its own field.
func TestResumeDryRunDoesNotReportALimitAsSent(t *testing.T) {
	host := resumeHost(t, "notLoaded")
	exe, _ := resumeRelayScript(t, resumeTestAssignment, autoCompactResumeSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	t.Setenv(execution.EnvPolicy, autoCompactResumePolicy(t))
	cfg := resumeConfig(host, "alpha")
	cfg.Bridge.ExecutionPolicy = os.Getenv(execution.EnvPolicy)
	report, err := resumeRun(context.Background(), e, cfg, resumeOptions{relationship: "rel-1", message: "m", dryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.AutoCompactTokenLimit != nil || report.AutoCompactUnobservable {
		t.Fatalf("a dry run that sent nothing reports a limit as sent: %+v", report)
	}
	if report.PlannedAutoCompactTokenLimit == nil || *report.PlannedAutoCompactTokenLimit != 550000 {
		t.Fatalf("the dry run does not state the limit a real run would send: %+v", report)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["autoCompactTokenLimit"]; present {
		t.Fatalf("the dry run's JSON carries autoCompactTokenLimit: %s", raw)
	}
	if _, present := fields["autoCompactUnobservable"]; present {
		t.Fatalf("the dry run's JSON carries autoCompactUnobservable: %s", raw)
	}
	if fields["plannedAutoCompactTokenLimit"] != float64(550000) {
		t.Fatalf("the dry run's JSON does not carry plannedAutoCompactTokenLimit: %s", raw)
	}
}
