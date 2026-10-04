package adapter

import (
	"context"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	bridgesettings "github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
)

// mcpRecovery names what lets a later resume apply the record again. It advises
// the operator; the relay neither unloads a thread nor changes its fixed profile.
func mcpRecovery(record delivery.TaskSettings, status any, findings []contract.OrderedObject) string {
	if status != "idle" {
		return ""
	}
	for _, finding := range findings {
		if finding.Get("code") != bridgesettings.NotPreserved || finding.Get("field") != "mcpServers" {
			continue
		}
		profile := "the recorded MCP settings"
		if name, _ := record.Data.Get("mcpProfile").(string); name != "" {
			profile = fmt.Sprintf("the recorded MCP profile %q", name)
		}
		return ". Recovery: the host ignores MCP overrides on a loaded thread. Only after a completed durable turn and confirmation that its rollout is resumable, release all subscriptions and wait until thread/read reports notLoaded; other clients may keep it loaded, and the host decides when it unloads. If it remains loaded, an operator can use thread/archive then thread/unarchive. The next relay delivery resumes under " + profile + ". Never unload a never-run root."
	}
	return ""
}

// withMCP is the record a send resumes with: a copy that carries no stored mcpServers key (settings-record
// stores any object, and only what this send resolved is its expectation) and, when the record states an
// MCP profile (key mcpProfile) and this host's policy declares profiles for the role it cites, what the
// profile resolved to against the host (key mcpServers) and that expectation. A host does not keep a
// thread's overrides, so every resume sends them again. The caller's record is not changed.
func (a *Adapter) withMCP(ctx context.Context, record *delivery.TaskSettings) (*delivery.TaskSettings, *bridgesettings.MCPExpectation, error) {
	data := make(delivery.Obj, 0, len(record.Data)+1)
	for _, field := range record.Data {
		if field.Key != "mcpServers" {
			data = append(data, field)
		}
	}
	send := &delivery.TaskSettings{Data: data, SettingsFreeResume: record.SettingsFreeResume}
	stated, _ := record.Data.Lookup("mcpProfile")
	cited, _ := record.Data.Lookup("citedRole")
	profile, _ := stated.(string)
	role, _ := cited.(string)
	if role == "" || profile == "" && !(stated == nil && role == "child" && managed.ArmedReplay(ctx)) {
		return send, nil, nil
	}
	declared, ok := a.bridge.Policy.Role(role)
	if !ok || declared.MCP == nil {
		return send, nil, nil
	}
	selection, err := declared.SelectMCP(role, profile)
	if err != nil {
		return nil, nil, err
	}
	if selection.Name == "" {
		return send, nil, nil
	}
	cwd, _ := record.Data.Lookup("cwd")
	cwdText, _ := cwd.(string)
	expected, _, err := bridge.ResolveMCP(ctx, a.hostCall, selection, cwdText)
	if err != nil {
		return nil, nil, err
	}
	send.Data = send.Data.Set("mcpServers", ordered(expected.Shape()))
	return send, expected, nil
}

// observeMCP adds to the host's answer the reduced status list of the thread, which is what the
// settings comparison reads; the raw list is kept on the receipt.
func (a *Adapter) observeMCP(ctx context.Context, thread string, resumed any, expected *bridgesettings.MCPExpectation, receipt map[string]any) (any, error) {
	answer, err := bridge.MCPStatus(ctx, a.hostCall, thread)
	if err != nil {
		return nil, err
	}
	receipt["mcpServerStatus"] = answer
	object, ok := resumed.(contract.OrderedObject)
	if !ok {
		return resumed, nil
	}
	if observed, ok := expected.Observe(answer); ok {
		object = append(contract.OrderedObject(nil), object...).Set("mcpServers", ordered(observed))
	}
	return object, nil
}
