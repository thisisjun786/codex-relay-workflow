package cli

import (
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The recorded settings' sandbox as doctor reports it, judged by the relay's one set of settings
// rules (registry: settings.py's require_usable, normalise_policy and sandbox_mode).

// sandboxSummary is _sandbox_summary: total, like the helper it leans on.
func sandboxSummary(raw, source, recordedAt any) contract.OrderedObject {
	unreadable := func(detail string) contract.OrderedObject {
		return contract.OrderedObject{{Key: "readable", Value: false}, {Key: "detail", Value: detail}}
	}
	text, ok := raw.(string)
	if !ok {
		return unreadable("the recorded settings are not valid JSON")
	}
	decoded, err := decodeJSON([]byte(text))
	if err != nil {
		return unreadable("the recorded settings are not valid JSON")
	}
	settings, ok := decoded.(contract.OrderedObject)
	if !ok {
		return unreadable("the recorded settings are " + jsonKind(decoded) + ", not an object")
	}
	policy := registry.NormalisePolicy(get(settings, "sandbox"))
	if policy == nil {
		return unreadable("the recorded sandbox policy cannot be read")
	}
	recorded := registry.TaskSettings{Data: settings}
	var refusedBy, detail, resumeMode any
	refused := recorded.RequireUsable()
	// RequireUsable refuses with a *store.RefusedError, the reason and detail doctor shows.
	var reason *store.RefusedError
	if refused == nil {
		resumeMode = recorded.SandboxMode()
	} else if errors.As(refused, &reason) {
		refusedBy, detail = reason.Reason, reason.Detail
	}
	cwd, _ := get(settings, "cwd").(string)
	return contract.OrderedObject{
		{Key: "readable", Value: true},
		{Key: "deliverable", Value: refused == nil},
		{Key: "refusedBy", Value: refusedBy},
		{Key: "detail", Value: detail},
		{Key: "resumeMode", Value: resumeMode},
		{Key: "mode", Value: get(policy, "type")},
		{Key: "writableRoots", Value: get(policy, "writableRoots")},
		{Key: "networkAccess", Value: get(policy, "networkAccess")},
		{Key: "excludeTmpdirEnvVar", Value: get(policy, "excludeTmpdirEnvVar")},
		{Key: "excludeSlashTmp", Value: get(policy, "excludeSlashTmp")},
		{Key: "cwd", Value: nullableText(cwd)},
		{Key: "recordedFrom", Value: source},
		{Key: "recordedAt", Value: recordedAt},
	}
}
