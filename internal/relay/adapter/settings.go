package adapter

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
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
		code := text(field(first, "code"))
		returned := pyvalue.Repr(field(first, "returned"))
		message := code + ": " + text(field(first, "field")) + " returned " + returned + "; message withheld"
		if !transmitted {
			code = registry.SettingsFreeRefusalCode(mismatches)
			message = code + ": " + text(field(first, "field")) + " is " + returned + " on the loaded thread and " + pyvalue.Repr(field(first, "expected")) + " in the record; nothing was transmitted and no turn was started"
		}
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
