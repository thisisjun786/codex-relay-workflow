package delivery

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var requiredSettings = []string{"sandbox", "approvalPolicy", "cwd", "runtimeWorkspaceRoots", "model", "reasoningEffort", "environments"}

// CarriedApprovalPolicies are the approval policies this transport carries (settings.py).
var CarriedApprovalPolicies = []string{"never", "on-request"}

var resumeSandboxMode = map[string]string{"workspaceWrite": "workspace-write", "readOnly": "read-only", "dangerFullAccess": "danger-full-access"}

// TaskSettings is settings.TaskSettings: the recorded execution settings a send preserves.
type TaskSettings struct {
	Data               Obj
	SettingsFreeResume bool
}

func (t TaskSettings) missing() []string {
	var absent []string
	for _, field := range requiredSettings {
		v, ok := get(t.Data, field)
		if !ok || v == nil {
			absent = append(absent, field)
		}
	}
	return absent
}

func textList(v any) bool {
	a, ok := v.([]any)
	if !ok {
		return false
	}
	for _, item := range a {
		if _, ok := item.(string); !ok {
			return false
		}
	}
	return true
}

func shapeOf(v any) string {
	if a, ok := v.([]any); ok {
		for _, one := range a {
			if _, ok := one.(string); !ok {
				if one == nil {
					return "a list holding None"
				}
				return "a list holding " + pyvalue.TypeName(one)
			}
		}
		return "a list of str"
	}
	if v == nil {
		return "absent"
	}
	return pyvalue.TypeName(v)
}

func environmentsProblem(v any) (string, string, bool) {
	a, ok := v.([]any)
	if !ok {
		return "", "is " + shapeOf(v) + ", not a list of environment objects", true
	}
	for i, entry := range a {
		o, ok := entry.(Obj)
		if !ok {
			return fmt.Sprintf("[%d]", i), "is " + shapeOf(entry) + ", not an object", true
		}
		for _, key := range []string{"environmentId", "cwd"} {
			value, _ := get(o, key)
			if _, ok := value.(string); !ok {
				return fmt.Sprintf("[%d].%s", i, key), "is " + shapeOf(value) + ", not str", true
			}
		}
		if roots, present := get(o, "runtimeWorkspaceRoots"); present && !textList(roots) {
			return fmt.Sprintf("[%d].runtimeWorkspaceRoots", i), "is " + shapeOf(roots) + ", not a list of str", true
		}
	}
	return "", "", false
}

func (t TaskSettings) mistyped() []string {
	var wrong []string
	for _, field := range []string{"cwd", "model", "reasoningEffort"} {
		if v, _ := get(t.Data, field); pyvalue.TypeName(v) != "str" {
			wrong = append(wrong, field)
		}
	}
	if v, _ := get(t.Data, "runtimeWorkspaceRoots"); !textList(v) {
		wrong = append(wrong, "runtimeWorkspaceRoots")
	}
	if v, _ := get(t.Data, "environments"); func() bool { _, _, bad := environmentsProblem(v); return bad }() {
		wrong = append(wrong, "environments")
	}
	return wrong
}

func (t TaskSettings) mistypedDetail(field string) string {
	value, _ := get(t.Data, field)
	switch field {
	case "runtimeWorkspaceRoots":
		return "runtimeWorkspaceRoots is " + shapeOf(value) + ", not a list of str"
	case "environments":
		where, what, _ := environmentsProblem(value)
		return "environments" + where + " " + what
	}
	return field + " is " + pyvalue.TypeName(value) + ", not str"
}

func (t TaskSettings) sandboxMode() (string, bool) {
	policy, ok := get(t.Data, "sandbox")
	o, isObj := policy.(Obj)
	if !ok || !isObj {
		return "", false
	}
	kind, isText := get(o, "type")
	name, _ := kind.(string)
	if !isText || name == "" {
		if _, s := kind.(string); !s {
			return "", false
		}
	}
	mode, known := resumeSandboxMode[name]
	return mode, known
}

var policyDefaults = map[string]Obj{
	"workspaceWrite":   {{Key: "writableRoots", Value: []any{}}, {Key: "networkAccess", Value: false}, {Key: "excludeTmpdirEnvVar", Value: false}, {Key: "excludeSlashTmp", Value: false}},
	"readOnly":         {{Key: "networkAccess", Value: false}},
	"externalSandbox":  {{Key: "networkAccess", Value: "restricted"}},
	"dangerFullAccess": {},
}

// normalisePolicy is settings.normalise_policy's readability decision (nil when unreadable).
func normalisePolicy(policy any) Obj {
	o, ok := policy.(Obj)
	if !ok {
		return nil
	}
	kind, ok := get(o, "type")
	name, isText := kind.(string)
	if !ok || !isText {
		return nil
	}
	declared := policyDefaults[name]
	merged := append(Obj(nil), declared...)
	for _, f := range o {
		if f.Key != "type" {
			merged = set(merged, f.Key, f.Value)
		}
	}
	merged = set(merged, "type", name)
	for _, d := range declared {
		value, _ := get(merged, d.Key)
		switch d.Value.(type) {
		case bool:
			if _, ok := value.(bool); !ok {
				return nil
			}
		case []any:
			if !textList(value) {
				return nil
			}
		default:
			if pyvalue.TypeName(value) != pyvalue.TypeName(d.Value) {
				return nil
			}
		}
	}
	if roots, ok := get(merged, "writableRoots"); ok && !textList(roots) {
		return nil
	}
	return merged
}

// RequireUsable is TaskSettings.require_usable: shape before meaning, every offender named.
func (t TaskSettings) RequireUsable() error {
	if absent := t.missing(); len(absent) > 0 {
		if env, ok := get(t.Data, "environments"); ok {
			if _, isList := env.([]any); isList {
				absent = slices.DeleteFunc(absent, func(s string) bool { return s == "environments" })
			}
		}
		if len(absent) > 0 {
			return refuse(SettingsIncomplete, "missing %s", strings.Join(absent, ", "))
		}
	}
	if wrong := t.mistyped(); len(wrong) > 0 {
		details := make([]string, len(wrong))
		for i, field := range wrong {
			details[i] = t.mistypedDetail(field)
		}
		return refuse(SettingsMistyped, "%s", strings.Join(details, "; "))
	}
	policy, _ := get(t.Data, "approvalPolicy")
	if p, ok := policy.(string); !ok || !slices.Contains(CarriedApprovalPolicies, p) {
		return refuse(UnsupportedApprovalPolicy, "the recorded approvalPolicy is %s; this transport carries only 'never' and 'on-request', leaving every approval a turn raises with the thread's own approver", pyReprValue(policy))
	}
	if _, ok := t.sandboxMode(); !ok {
		recorded, _ := get(t.Data, "sandbox")
		o, isObj := recorded.(Obj)
		if !isObj {
			return refuse(UnsupportedSandboxType, "the recorded sandbox is %s, not the policy object a creation result reports, so it does not record the full policy a resume would have to restore", pyvalue.TypeName(recorded))
		}
		kind, _ := get(o, "type")
		return refuse(UnsupportedSandboxType, "%s has no ThreadResumeParams.sandbox mode, so it cannot be restored on a resume", pyReprValue(kind))
	}
	sandbox, _ := get(t.Data, "sandbox")
	if normalisePolicy(sandbox) == nil {
		return refuse(UnsupportedSandboxType, "the recorded sandbox policy cannot be read in full, so no response could confirm it")
	}
	return nil
}

// RoleGate decides the role-policy half of authorized_settings for a task bound to a scope.
type RoleGate func(ctx context.Context, s *store.Store, taskID string, settings *TaskSettings) error

// DefaultRoleGate is the registry's role check (registry.CheckBoundRole) against the execution
// policy this process was started with: a task bound to no role passes, and a bound task passes
// only when that policy declares a pair its recorded settings are authorized for. Without a
// policy a bound task is withheld as role_policy_unconfigured. A bound task whose pair was not
// derived from its role's pair (a supervisor's recorded pair, an exception) is resumed
// settings-free, as every other sender resumes it, so a later user selection is never reverted.
func DefaultRoleGate(ctx context.Context, s *store.Store, taskID string, settings *TaskSettings) error {
	_, settingsFree, err := registry.CheckBoundRole(ctx, s, taskID, settings.Data, registry.EnvironmentRolePolicy())
	if err == nil {
		settings.SettingsFreeResume = settingsFree
	}
	return err
}

// AuthorizedSettings is delivery.authorized_settings.
func AuthorizedSettings(ctx context.Context, s *store.Store, taskID string, gate RoleGate) (*TaskSettings, error) {
	row, err := one(ctx, s, "SELECT settings FROM authorized_settings WHERE task_id = ?", taskID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, refuse(SettingsUnavailable, "no authorized settings recorded for %s; register them from the creation result before a send can preserve them", pyvalue.StrRepr(taskID))
	}
	data := loadsObj(row.S("settings"))
	settings := &TaskSettings{Data: data}
	// The relay's one set of settings rules (settings.py require_usable) is the registry's.
	if err := (registry.TaskSettings{Data: data}).RequireUsable(); err != nil {
		return nil, err
	}
	if gate == nil {
		gate = DefaultRoleGate
	}
	if err := gate(ctx, s, taskID, settings); err != nil {
		return nil, err
	}
	return settings, nil
}

var policyConfigKeys = map[string][][3]string{
	"workspaceWrite": {{"writableRoots", "sandbox_workspace_write", "writable_roots"}, {"networkAccess", "sandbox_workspace_write", "network_access"}, {"excludeTmpdirEnvVar", "sandbox_workspace_write", "exclude_tmpdir_env_var"}, {"excludeSlashTmp", "sandbox_workspace_write", "exclude_slash_tmp"}},
}

// ResumeParams is TaskSettings.resume_params: only fields ThreadResumeParams defines, and never
// an approvalPolicy - the host would apply it to a thread the resume loads.
func (t TaskSettings) ResumeParams(thread string) Obj {
	mode, _ := t.sandboxMode()
	roots, _ := get(t.Data, "runtimeWorkspaceRoots")
	cwd, _ := get(t.Data, "cwd")
	model, _ := get(t.Data, "model")
	effort, _ := get(t.Data, "reasoningEffort")
	config := Obj{{Key: "model_reasoning_effort", Value: effort}}
	sandbox, _ := get(t.Data, "sandbox")
	policy := normalisePolicy(sandbox)
	for _, k := range policyConfigKeys[str(policy, "type")] {
		if v, ok := get(policy, k[0]); ok {
			section, _ := get(config, k[1])
			o, _ := section.(Obj)
			config = set(config, k[1], set(o, k[2], v))
		}
	}
	return Obj{{Key: "threadId", Value: thread}, {Key: "excludeTurns", Value: true}, {Key: "sandbox", Value: mode}, {Key: "cwd", Value: cwd}, {Key: "runtimeWorkspaceRoots", Value: roots}, {Key: "model", Value: model}, {Key: "config", Value: config}}
}
