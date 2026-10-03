package registry

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// CRW-466: a recorded row may carry the MCP expectation the sender resolved for the send (key
// mcpServers, the shape the bridge's settings package reduces a status list to). The resume then
// carries the switch-offs and the host's answer is compared with the expectation.

const mcpRowJSON = `{"sandbox":{"type":"readOnly","networkAccess":false},"approvalPolicy":"never","cwd":"/w","runtimeWorkspaceRoots":["/w"],"model":"m","reasoningEffort":"xhigh",
"environments":[{"environmentId":"local","cwd":"/w","runtimeWorkspaceRoots":["/w"]}],"mcpServers":{"disabled":["gemini_notebook","oracle"],"enabled":["node_repl"],"pluginsAbsent":["cua@openai-bundled"]}}`

func mcpDecoded(t *testing.T, raw string) contract.OrderedObject {
	t.Helper()
	value, err := decodeJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value.(contract.OrderedObject)
}

// resumeAnswer is what a host that kept the recorded settings answers, with the given mcpServers.
func resumeAnswer(t *testing.T, model, mcp string) contract.OrderedObject {
	t.Helper()
	answer := `{"approvalPolicy":"never","sandbox":{"type":"readOnly","networkAccess":false},"cwd":"/w","runtimeWorkspaceRoots":["/w"],"model":"` + model + `","reasoningEffort":"xhigh","activePermissionProfile":null,
"thread":{"id":"t","environments":[{"environmentId":"local","cwd":"/w","runtimeWorkspaceRoots":["/w"]}]}` + mcp + `}`
	return mcpDecoded(t, answer)
}

const matchingMCP = `,"mcpServers":{"disabled":["gemini_notebook","oracle"],"enabled":["node_repl"],"pluginsAbsent":["cua@openai-bundled"]}`

func TestResumeParamsSwitchOffWhatTheRowsMCPExpectationSays(t *testing.T) {
	row := TaskSettings{mcpDecoded(t, mcpRowJSON)}
	config, _ := row.ResumeParams("t-1").Lookup("config")
	servers, _ := config.(contract.OrderedObject).Lookup("mcp_servers")
	plugins, _ := config.(contract.OrderedObject).Lookup("plugins")
	if canonical(servers) != `{"gemini_notebook":{"enabled":false},"oracle":{"enabled":false}}` || canonical(plugins) != `{"cua@openai-bundled":{"enabled":false}}` {
		t.Fatalf("mcp_servers=%s plugins=%s", canonical(servers), canonical(plugins))
	}
	if effort, _ := config.(contract.OrderedObject).Lookup("model_reasoning_effort"); effort != "xhigh" {
		t.Fatalf("the effort is gone from %s", canonical(config))
	}
	plain := TaskSettings{mcpDecoded(t, `{"sandbox":{"type":"readOnly","networkAccess":false},"approvalPolicy":"never","cwd":"/w","runtimeWorkspaceRoots":["/w"],"model":"m","reasoningEffort":"xhigh","environments":[{"environmentId":"local","cwd":"/w","runtimeWorkspaceRoots":["/w"]}]}`)}
	config, _ = plain.ResumeParams("t-1").Lookup("config")
	if _, has := config.(contract.OrderedObject).Lookup("mcp_servers"); has {
		t.Fatalf("a row with no expectation switches servers off: %s", canonical(config))
	}
}

func TestMismatchesCompareTheMCPExpectationOnlyWhenTheRowCarriesOne(t *testing.T) {
	row := TaskSettings{mcpDecoded(t, mcpRowJSON)}
	if found := row.Mismatches(resumeAnswer(t, "m", matchingMCP), true, false, false); len(found) != 0 {
		t.Fatalf("a matching answer is a finding: %v", found)
	}
	differ := `,"mcpServers":{"disabled":["gemini_notebook"],"enabled":["node_repl"],"pluginsAbsent":["cua@openai-bundled"]}`
	found := row.Mismatches(resumeAnswer(t, "m", differ), true, false, false)
	if len(found) != 1 || found[0].Get("code") != SettingsNotPreserved || found[0].Get("field") != "mcpServers" {
		t.Fatalf("a differing answer: %v", found)
	}
	found = row.Mismatches(resumeAnswer(t, "m", ""), true, false, false)
	if len(found) != 1 || found[0].Get("code") != SettingUnobservable || found[0].Get("field") != "mcpServers" {
		t.Fatalf("an answer with no observation: %v", found)
	}
	found = row.Mismatches(resumeAnswer(t, "other", differ), true, false, false)
	if len(found) != 2 || found[0].Get("field") != "model" || found[1].Get("field") != "mcpServers" {
		t.Fatalf("the model finding must come first: %v", found)
	}
	plain := TaskSettings{mcpDecoded(t, mcpRowJSON)}
	plain.Data = plain.Data[:len(plain.Data)-1]
	if found := plain.Mismatches(resumeAnswer(t, "m", ""), true, false, false); len(found) != 0 {
		t.Fatalf("a row with no expectation compares MCP servers: %v", found)
	}
}

// Something was transmitted for an MCP finding, so it is not renamed as a difference nothing
// could have made agree.
func TestASettingsFreeRefusalKeepsTheCodeOfAnMCPFinding(t *testing.T) {
	mcp := []contract.OrderedObject{finding(SettingsNotPreserved, "mcpServers", "a", "b")}
	if got := SettingsFreeRefusalCode(mcp); got != SettingsNotPreserved {
		t.Fatalf("code = %s", got)
	}
	model := []contract.OrderedObject{finding(SettingsNotPreserved, "model", "a", "b")}
	if got := SettingsFreeRefusalCode(model); got != SettingsDifferAfterLoad {
		t.Fatalf("code = %s", got)
	}
}
