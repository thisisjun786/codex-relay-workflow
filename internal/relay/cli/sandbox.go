package cli

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// The settings rules _sandbox_summary runs, from settings.py. Only the record-side gates are
// here: require_usable and the policy normalisation it depends on.

var resumeSandboxMode = map[string]string{
	"workspaceWrite": "workspace-write", "readOnly": "read-only", "dangerFullAccess": "danger-full-access",
}

// policyDefaults is POLICY_DEFAULTS, in declaration order.
var policyDefaults = map[string]contract.OrderedObject{
	"workspaceWrite": {{Key: "writableRoots", Value: []any{}}, {Key: "networkAccess", Value: false},
		{Key: "excludeTmpdirEnvVar", Value: false}, {Key: "excludeSlashTmp", Value: false}},
	"readOnly":         {{Key: "networkAccess", Value: false}},
	"externalSandbox":  {{Key: "networkAccess", Value: "restricted"}},
	"dangerFullAccess": {},
}

var requiredSettings = []string{"sandbox", "approvalPolicy", "cwd", "runtimeWorkspaceRoots", "model", "reasoningEffort", "environments"}

type settingsRefusal struct{ reason, detail string }

func textList(v any) bool {
	items, ok := v.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if _, ok := item.(string); !ok {
			return false
		}
	}
	return true
}

// normalisePolicy is normalise_policy: nil for anything it cannot read.
func normalisePolicy(policy any) contract.OrderedObject {
	object, ok := policy.(contract.OrderedObject)
	if !ok {
		return nil
	}
	kind, ok := get(object, "type").(string)
	if !ok {
		return nil
	}
	declared := policyDefaults[kind]
	merged := append(contract.OrderedObject{}, declared...)
	for _, field := range object {
		if field.Key == "type" {
			continue
		}
		if at := fieldIndex(merged, field.Key); at >= 0 {
			merged[at].Value = field.Value
		} else {
			merged = append(merged, field)
		}
	}
	if at := fieldIndex(merged, "type"); at >= 0 {
		merged[at].Value = kind
	} else {
		merged = append(merged, contract.Field{Key: "type", Value: kind})
	}
	for _, field := range declared {
		value := get(merged, field.Key)
		var readable bool
		switch field.Value.(type) {
		case bool:
			_, readable = value.(bool)
		case []any:
			readable = textList(value)
		default:
			readable = pyTypeName(value) == pyTypeName(field.Value)
		}
		if !readable {
			return nil
		}
	}
	if has(merged, "writableRoots") && !textList(get(merged, "writableRoots")) {
		return nil
	}
	return merged
}

func sandboxMode(settings contract.OrderedObject) any {
	policy, ok := get(settings, "sandbox").(contract.OrderedObject)
	if !ok {
		return nil
	}
	kind, ok := get(policy, "type").(string)
	if !ok {
		return nil
	}
	if mode, ok := resumeSandboxMode[kind]; ok {
		return mode
	}
	return nil
}

// shape is _shape.
func shape(value any) string {
	if items, ok := value.([]any); ok {
		for _, one := range items {
			if _, ok := one.(string); !ok {
				if one == nil {
					return "a list holding None"
				}
				return "a list holding " + pyTypeName(one)
			}
		}
		return "a list of str"
	}
	if value == nil {
		return "absent"
	}
	return pyTypeName(value)
}

// environmentsProblem is environments_problem: (where, what), or ok=false when readable.
func environmentsProblem(environments any) (string, string, bool) {
	items, ok := environments.([]any)
	if !ok {
		return "", "is " + shape(environments) + ", not a list of environment objects", true
	}
	for index, entry := range items {
		object, ok := entry.(contract.OrderedObject)
		if !ok {
			return fmt.Sprintf("[%d]", index), "is " + shape(entry) + ", not an object", true
		}
		for _, key := range []string{"environmentId", "cwd"} {
			if _, ok := get(object, key).(string); !ok {
				return fmt.Sprintf("[%d].%s", index, key), "is " + shape(get(object, key)) + ", not str", true
			}
		}
		if has(object, "runtimeWorkspaceRoots") && !textList(get(object, "runtimeWorkspaceRoots")) {
			return fmt.Sprintf("[%d].runtimeWorkspaceRoots", index), "is " + shape(get(object, "runtimeWorkspaceRoots")) + ", not a list of str", true
		}
	}
	return "", "", false
}

// requireUsable is TaskSettings.require_usable.
func requireUsable(settings contract.OrderedObject) *settingsRefusal {
	var absent []string
	for _, field := range requiredSettings {
		if get(settings, field) == nil {
			absent = append(absent, field)
		}
	}
	if len(absent) > 0 {
		return &settingsRefusal{"settings_incomplete", "missing " + strings.Join(absent, ", ")}
	}
	var wrong []string
	for _, field := range []string{"cwd", "model", "reasoningEffort"} {
		if _, ok := get(settings, field).(string); !ok {
			wrong = append(wrong, field+" is "+pyTypeName(get(settings, field))+", not str")
		}
	}
	if roots := get(settings, "runtimeWorkspaceRoots"); !textList(roots) {
		wrong = append(wrong, "runtimeWorkspaceRoots is "+shape(roots)+", not a list of str")
	}
	if where, what, bad := environmentsProblem(get(settings, "environments")); bad {
		wrong = append(wrong, "environments"+where+" "+what)
	}
	if len(wrong) > 0 {
		return &settingsRefusal{"settings_mistyped", strings.Join(wrong, "; ")}
	}
	if approval := get(settings, "approvalPolicy"); approval != "never" && approval != "on-request" {
		return &settingsRefusal{"unsupported_approval_policy", "the recorded approvalPolicy is " + pyRepr(approval) +
			"; this transport carries only 'never' and 'on-request', leaving every approval a turn raises with the thread's own approver"}
	}
	if sandboxMode(settings) == nil {
		recorded := get(settings, "sandbox")
		if _, ok := recorded.(contract.OrderedObject); !ok {
			return &settingsRefusal{"unsupported_sandbox_type", "the recorded sandbox is " + pyTypeName(recorded) +
				", not the policy object a creation result reports, so it does not record the full policy a resume would have to restore"}
		}
		return &settingsRefusal{"unsupported_sandbox_type", pyRepr(get(recorded, "type")) +
			" has no ThreadResumeParams.sandbox mode, so it cannot be restored on a resume"}
	}
	if normalisePolicy(get(settings, "sandbox")) == nil {
		return &settingsRefusal{"unsupported_sandbox_type", "the recorded sandbox policy cannot be read in full, so no response could confirm it"}
	}
	return nil
}

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
		return unreadable("the recorded settings are " + pyTypeName(decoded) + ", not an object")
	}
	policy := normalisePolicy(get(settings, "sandbox"))
	if policy == nil {
		return unreadable("the recorded sandbox policy cannot be read")
	}
	refused := requireUsable(settings)
	var refusedBy, detail, resumeMode any
	if refused == nil {
		resumeMode = sandboxMode(settings)
	} else {
		refusedBy, detail = refused.reason, refused.detail
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
