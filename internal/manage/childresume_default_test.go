package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// CRW-1398 item 1: crw manage child-resume reads a configuration file that does not name child_check.disabled_servers as the
// default list (none) with a warning on standard error and a record in the report, and never refuses for it.

// resumeSentMCPServers is the mcp_servers object of the thread/resume request the host saw, and whether a resume was sent.
func resumeSentMCPServers(t *testing.T, host *fakehost.Server) (map[string]any, bool) {
	t.Helper()
	for _, request := range resumeHostRequests(host) {
		if request.Method != "thread/resume" {
			continue
		}
		var params struct{ Config map[string]any }
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Fatal(err)
		}
		servers, _ := params.Config["mcp_servers"].(map[string]any)
		return servers, true
	}
	return nil, false
}

// resumeNoServersHost is the idle fake host whose status list names no server, as a host with none to stop reports.
func resumeNoServersHost(t *testing.T) *fakehost.Server {
	t.Helper()
	host := resumeHost(t, "idle")
	host.Respond("mcpServerStatus/list", fakehost.Reply{Result: map[string]any{"data": []any{}, "nextCursor": nil}})
	return host
}

func TestResumeReadsAMissingDisabledServersKeyAsTheDefaultWithAWarningAndARecord(t *testing.T) {
	host := resumeNoServersHost(t)
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, _, errOut := resumeEnv(t, exe)
	report, err := resumeRun(context.Background(), e, resumeConfig(host), resumeOptions{relationship: "rel-1", message: "m"})
	if err != nil {
		t.Fatalf("a configuration without child_check.disabled_servers was refused: %v", err)
	}
	if !report.OK || report.TurnID != "turn-1" {
		t.Fatalf("report %+v", report)
	}
	if got := report.DisabledServers; got == nil || got.Source != "default" || len(got.Servers) != 0 ||
		!strings.Contains(got.Warning, "child_check.disabled_servers") {
		t.Fatalf("the record of the list is %+v", got)
	}
	if !strings.Contains(errOut.String(), "warning") || !strings.Contains(errOut.String(), "child_check.disabled_servers") {
		t.Errorf("standard error carries no warning naming the key: %q", errOut.String())
	}
	servers, resumed := resumeSentMCPServers(t, host)
	if !resumed || len(servers) != 0 {
		t.Errorf("the resume (sent %v) switched off %v, want no server", resumed, servers)
	}
}

func TestResumeDryRunReadsAMissingDisabledServersKeyAsTheDefaultToo(t *testing.T) {
	host := resumeNoServersHost(t)
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, _, errOut := resumeEnv(t, exe)
	report, err := resumeRun(context.Background(), e, resumeConfig(host), resumeOptions{relationship: "rel-1", message: "m", dryRun: true})
	if err != nil {
		t.Fatalf("the dry run was refused: %v", err)
	}
	if got := report.DisabledServers; got == nil || got.Source != "default" || got.Warning == "" || !report.DryRun {
		t.Fatalf("record %+v, report %+v", got, report)
	}
	if methods := resumeHostMethods(host); !slices.Equal(methods, []string{"thread/read"}) {
		t.Fatalf("the dry run sent %q", methods)
	}
	if !strings.Contains(errOut.String(), "child_check.disabled_servers") {
		t.Errorf("no warning on standard error: %q", errOut.String())
	}
}

func TestResumeKeepsAPresentDisabledServersListWithoutAWarning(t *testing.T) {
	for name, names := range map[string][]string{"named": {"alpha", "beta"}, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			host := resumeHost(t, "idle")
			if len(names) == 0 {
				host.Respond("mcpServerStatus/list", fakehost.Reply{Result: map[string]any{"data": []any{}, "nextCursor": nil}})
			}
			exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
			e, _, errOut := resumeEnv(t, exe)
			cfg := resumeConfig(host)
			list, err := json.Marshal(map[string]any{"disabled_servers": names})
			if err != nil {
				t.Fatal(err)
			}
			cfg.raw["child_check"] = list
			report, err := resumeRun(context.Background(), e, cfg, resumeOptions{relationship: "rel-1", message: "m"})
			if err != nil {
				t.Fatal(err)
			}
			got := report.DisabledServers
			if got == nil || got.Source != "config" || got.Warning != "" || !slices.Equal(got.Servers, names) {
				t.Fatalf("the record of the list is %+v, want the file's %v", got, names)
			}
			if errOut.Len() != 0 {
				t.Errorf("a key the file names was warned about: %q", errOut.String())
			}
			if servers, _ := resumeSentMCPServers(t, host); len(servers) != len(names) {
				t.Errorf("the resume switched off %v, want %v", servers, names)
			}
		})
	}
}

// A key that is there but cannot be read is not a missing key: it stays the refusal it was, before the host is contacted.
func TestResumeStillRefusesADisabledServersKeyThatIsNoList(t *testing.T) {
	host := resumeHost(t, "idle")
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	cfg := resumeConfig(host)
	cfg.raw["child_check"] = json.RawMessage(`{"disabled_servers":"alpha"}`)
	_, err := resumeRun(context.Background(), e, cfg, resumeOptions{relationship: "rel-1", message: "m"})
	failure, ok := err.(*resumeFailure)
	if !ok || failure.Reason != resumeDisabledServersUnset || resumeExit(failure) != usageExit {
		t.Fatalf("err = %v", err)
	}
	if n := host.Count("thread/read"); n != 0 {
		t.Errorf("the host was contacted %d times before the key was refused", n)
	}
}

// The command, from a configuration file with a manage object that has no child_check: exit 0, the warning, the record.
func TestResumeCommandRunsFromAConfigFileWithoutChildCheck(t *testing.T) {
	host := resumeNoServersHost(t)
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, out, errOut := resumeEnv(t, exe)
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("CRW_CONFIG", "")
	coreConfigWrite(t, filepath.Join(configHome, "crw", "config.json"), coreConfigMarshal(t, map[string]any{
		"schema": "crw-config/1",
		"manage": map[string]any{"relay": map[string]string{"socket": host.SocketPath}},
	}))
	messageFile := filepath.Join(t.TempDir(), "message.txt")
	if err := os.WriteFile(messageFile, []byte("go on"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := resumeRunCommand(context.Background(), e, []string{"--relationship", "rel-1", "--message-file", messageFile}); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), `"turnId":"turn-1"`) || !strings.Contains(out.String(), `"disabledServers":{"source":"default"`) {
		t.Errorf("the command reported %q", out.String())
	}
	if !strings.Contains(errOut.String(), "child_check.disabled_servers") {
		t.Errorf("the command warned %q", errOut.String())
	}
}

// CRW-1398 item 1 (the notLoaded question): a thread that reports no settings until it is resumed is resumed by a real run with
// the recorded pair (the record is the child's one authorized pair, so unlike the bridge's multi-pair route there is no pair to
// withhold), and the turn starts only when the resume's answer equals the record. A dry run sends nothing, so it cannot learn
// what an unloaded thread runs and says settings_unverified (TestResumeDryRunReportsUnverifiedSettings).
func TestResumeOfANotLoadedThreadSendsTheRecordedPairAndComparesTheAnswer(t *testing.T) {
	for name, answer := range map[string]map[string]any{
		"equal":     {"model": "m", "reasoningEffort": "xhigh"},
		"otherPair": {"model": "m", "reasoningEffort": "low"},
	} {
		t.Run(name, func(t *testing.T) {
			host := resumeNoServersHost(t)
			host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
				"status": map[string]any{"type": "notLoaded"}}}})
			host.Respond("thread/resume", fakehost.Reply{Result: answer})
			exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
			e, _, _ := resumeEnv(t, exe)
			cfg := resumeConfig(host)
			cfg.raw["child_check"] = json.RawMessage(`{"disabled_servers":[]}`)
			_, err := resumeRun(context.Background(), e, cfg, resumeOptions{relationship: "rel-1", message: "m"})
			for _, request := range resumeHostRequests(host) {
				if request.Method != "thread/resume" {
					continue
				}
				var params struct {
					Model  string
					Config map[string]any
				}
				if err := json.Unmarshal(request.Params, &params); err != nil || params.Model != "m" || params.Config["model_reasoning_effort"] != "xhigh" {
					t.Errorf("the resume carried %s (%v), want the recorded pair", request.Params, err)
				}
			}
			if name == "equal" {
				if err != nil || host.Count("turn/start") != 1 {
					t.Fatalf("err = %v, turn/start %d", err, host.Count("turn/start"))
				}
				return
			}
			if failure, ok := err.(*resumeFailure); !ok || failure.Reason != resumeSettingsMismatch || host.Count("turn/start") != 0 {
				t.Fatalf("err = %v, turn/start %d", err, host.Count("turn/start"))
			}
		})
	}
}
