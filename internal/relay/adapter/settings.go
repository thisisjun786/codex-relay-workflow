package adapter

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// Use registry's recorded-settings predicate, including its total handling of a
// malformed old record and the loaded-root subset check. Do not copy its policy.
func verifyResume(settings delivery.TaskSettings, response any, status any) (contract.OrderedObject, []any, []any) {
	transmitted := !settings.SettingsFreeResume
	recorded := registry.TaskSettings{Data: settings.Data}
	mismatches := recorded.Mismatches(response, transmitted, false, transmitted && status == "idle")
	findings := []any{}
	for _, finding := range mismatches {
		findings = append(findings, finding)
	}
	if len(findings) > 0 {
		first := mismatches[0]
		code := pyjson.Text(first.Get("code"))
		returned := pyvalue.Repr(first.Get("returned"))
		message := code + ": " + pyjson.Text(first.Get("field")) + " returned " + returned + "; message withheld"
		if !transmitted {
			code = registry.SettingsFreeRefusalCode(mismatches)
			// A settings-free resume transmits no pair, which is what this sentence is about. It may
			// still have sent the MCP profile's overrides and the pair's auto-compaction limit, neither
			// of which is a pair, so the sentence names what went out rather than counting nothing.
			sent := "nothing was transmitted"
			_, overrides := settings.Data.Lookup("mcpServers")
			switch {
			case overrides && settings.AutoCompactTokenLimit != nil:
				sent = "only the MCP profile's overrides and the pair's auto-compaction limit were transmitted"
			case overrides:
				sent = "only the MCP profile's overrides were transmitted"
			case settings.AutoCompactTokenLimit != nil:
				sent = "only the pair's auto-compaction limit was transmitted"
			}
			message = code + ": " + pyjson.Text(first.Get("field")) + " is " + returned + " on the loaded thread and " + pyvalue.Repr(first.Get("expected")) + " in the record; " + sent + " and no turn was started"
		}
		message += mcpRecovery(settings, status, mismatches)
		return contract.OrderedObject{{Key: "code", Value: code}, {Key: "message", Value: message}}, findings, nil
	}
	notes := []any{}
	if note := settings.ApprovalDivergence(response); note != nil {
		notes = append(notes, note)
	}
	for _, note := range recorded.RootsNarrowing(response, status) {
		notes = append(notes, note)
	}
	return nil, nil, notes
}
