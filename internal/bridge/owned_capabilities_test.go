package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func capabilityReport(t *testing.T) (map[string]any, *fakehost.Server) {
	t.Helper()
	b, host := testBridge(t)
	report, err := b.GetCapabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return report, host
}

func Test_test_capabilities_keep_bridge_exposure_and_host_support_apart(t *testing.T) {
	report, host := capabilityReport(t)
	exposure, support := object(report["exposure"]), object(report["hostSupport"])
	if exposure["steerActiveTurn"] != true || exposure["goalPause"] != true || exposure["turnInterrupt"] != false || exposure["goalObjectiveWrite"] != false || support["state"] != "unknown_host_version" || support["observedServer"] != "fake Codex/0.153.4" {
		t.Fatalf("report=%v calls=%v", report, host.Requests())
	}
	for _, req := range host.Requests() {
		if strings.HasPrefix(req.Method, "turn/") {
			t.Fatalf("turn method called: %s", req.Method)
		}
	}
}

func Test_test_capabilities_reports_only_flags_about_this_bridge(t *testing.T) {
	report, _ := capabilityReport(t)
	flags := object(report["capabilities"])
	for name, expected := range map[string]bool{"createThread": true, "sendMessage": true, "listReadWait": true, "goalRead": true, "bridgeManagedWorktrees": true, "projectImport": false, "clientSideToolsAndApprovals": false} {
		if flags[name] != expected {
			t.Fatalf("%s=%v", name, flags[name])
		}
	}
	if len(flags) != 7 || report["capabilitiesNote"] == nil {
		t.Fatalf("report=%v", report)
	}
}

func Test_test_capabilities_stops_answering_what_it_never_asked(t *testing.T) {
	report, _ := capabilityReport(t)
	for _, key := range []string{"desktopManagedWorktrees", "desktopProjectRegistry", "goalSet"} {
		if _, present := object(report["capabilities"])[key]; present {
			t.Fatalf("unexpected %s", key)
		}
	}
}

func Test_test_an_unasked_question_carries_a_sentence_and_never_a_value(t *testing.T) {
	report, _ := capabilityReport(t)
	questions := object(report["hostNotProbed"])
	if len(questions) != 2 || report["hostNotProbedNote"] == nil {
		t.Fatalf("report=%v", report)
	}
	for key, flag := range map[string]string{"desktopManagedWorktrees": "bridgeManagedWorktrees", "desktopProjectRegistry": "projectImport"} {
		answer, ok := questions[key].(string)
		if !ok || !strings.HasPrefix(answer, "Not asked.") || !strings.Contains(answer, flag) {
			t.Fatalf("question=%s answer=%v", key, questions[key])
		}
	}
}

func Test_test_a_consumer_can_tell_measured_from_assumed_using_only_the_response(t *testing.T) {
	report, _ := capabilityReport(t)
	questions := object(report["hostNotProbed"])
	if len(object(report["capabilities"])) == 0 || len(questions) == 0 {
		t.Fatalf("report=%v", report)
	}
	for _, block := range []string{"capabilities", "exposure", "hostSupport"} {
		for question := range questions {
			if _, present := object(report[block])[question]; present {
				t.Fatalf("answered %s in %s", question, block)
			}
		}
	}
}

func Test_test_capabilities_asks_the_connected_host_nothing(t *testing.T) {
	b, host := testBridge(t)
	if _, err := b.GetCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, req := range host.Requests() {
		if req.Method != "initialize" && req.Method != "initialized" {
			t.Fatalf("probe=%v", req)
		}
	}
}

func Test_test_two_host_facing_answers_disagree_on_one_connection(t *testing.T) {
	report, _ := capabilityReport(t)
	if object(report["desktopVisibility"])["observedOn"] != "codex-cli 0.153.4" || object(report["desktopVisibility"])["sameVersionConnected"] != true || object(report["hostSupport"])["state"] != "unknown_host_version" || object(object(report["approvals"])["preservationObserved"])["sameVersionConnected"] != false {
		t.Fatalf("report=%v", report)
	}
}

func Test_test_a_tested_host_never_comes_to_cover_an_unanswered_question(t *testing.T) {
	host := fakehost.Start(t)
	host.Respond("initialize", fakehost.Reply{Result: map[string]any{"userAgent": "fake Codex/0.154.0", "platformOs": "linux"}})
	client, err := appserver.Dial(context.Background(), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	b := New(client, nil, executionPolicy())
	report, err := b.GetCapabilities(context.Background())
	if err != nil || object(report["hostSupport"])["state"] != "tested" || len(object(report["hostNotProbed"])) != 2 || object(object(report["approvals"])["preservationObserved"])["sameVersionConnected"] != true || object(report["desktopVisibility"])["sameVersionConnected"] != false {
		t.Fatalf("report=%v err=%v", report, err)
	}
	for _, question := range object(report["hostNotProbed"]) {
		if _, ok := question.(string); !ok {
			t.Fatalf("unanswered question=%v", question)
		}
	}
}

func versionTokens(agent string) []string {
	tokens := []string{}
	for _, match := range hostVersionToken.FindAllStringSubmatch(agent, -1) {
		tokens = append(tokens, match[1])
	}
	return tokens
}

func Test_test_a_version_is_identified_by_token_not_by_substring(t *testing.T) {
	real := "Codex Desktop/0.154.0 (Ubuntu 26.4.0; x86_64) unknown (codex_thread_bridge; 0.1.0)"
	if !connectedVersion(real, "0.154.0") || connectedVersion("Codex Desktop/10.154.0 (linux)", "0.154.0") || len(versionTokens("codex-cli 10.154.0")) != 0 || len(versionTokens("")) != 0 {
		t.Fatal("version token matched a substring")
	}
}

func Test_test_a_prerelease_build_is_not_the_tested_host(t *testing.T) {
	for _, suffix := range []string{"-alpha.1", "-rc.2", "+build.5", "-alpha.1+build.5"} {
		agent := "codex_web_agent/0.154.0" + suffix
		if tokens := versionTokens(agent); len(tokens) != 1 || tokens[0] != "0.154.0"+suffix || connectedVersion(agent, "0.154.0") {
			t.Fatalf("suffix %s: tokens=%v", suffix, tokens)
		}
	}
	if !connectedVersion("Codex Desktop/0.154.0 (Ubuntu 26.4.0; x86_64) unknown (codex_thread_bridge; 0.1.0)", "0.154.0") {
		t.Fatal("tested release beside the bridge version not identified")
	}
}
