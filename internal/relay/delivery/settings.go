package delivery

import (
	"context"
	"fmt"
	"slices"
	"strings"

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

func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case int64:
		return "int"
	case float64:
		return "float"
	case Obj:
		return "dict"
	case []any:
		return "list"
	}
	return fmt.Sprintf("%T", v)
}

func shapeOf(v any) string {
	if a, ok := v.([]any); ok {
		for _, one := range a {
			if _, ok := one.(string); !ok {
				if one == nil {
					return "a list holding None"
				}
				return "a list holding " + pyTypeName(one)
			}
		}
		return "a list of str"
	}
	if v == nil {
		return "absent"
	}
	return pyTypeName(v)
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
		if v, _ := get(t.Data, field); pyTypeName(v) != "str" {
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
	return field + " is " + pyTypeName(value) + ", not str"
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
			if pyTypeName(value) != pyTypeName(d.Value) {
				return nil
			}
		}
	}
	if roots, ok := get(merged, "writableRoots"); ok && !textList(roots) {
		return nil
	}
	return merged
}

func pyReprValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return store.PyRepr(t)
	case int64:
		return fmt.Sprint(t)
	case float64:
		return pyFloat(t)
	case []any:
		parts := make([]string, len(t))
		for i, x := range t {
			parts[i] = pyReprValue(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case Obj:
		parts := make([]string, len(t))
		for i, f := range t {
			parts[i] = store.PyRepr(f.Key) + ": " + pyReprValue(f.Value)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
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
			return refuse(UnsupportedSandboxType, "the recorded sandbox is %s, not the policy object a creation result reports, so it does not record the full policy a resume would have to restore", pyTypeName(recorded))
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
// The registry track owns rolepolicy (todo 25); until it is wired, a bound task is refused as
// Python refuses it when this process has no readable policy.
type RoleGate func(ctx context.Context, s *store.Store, taskID string, settings *TaskSettings) error

// DefaultRoleGate is bound_role + the unresolved-policy refusal.
func DefaultRoleGate(ctx context.Context, s *store.Store, taskID string, _ *TaskSettings) error {
	rows, err := all(ctx, s, "SELECT DISTINCT role FROM scope_bindings WHERE task_id = ? AND status IN (?,?) AND superseded_by IS NULL", taskID, "active", "paused")
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	if len(rows) > 1 {
		roles := make([]string, len(rows))
		for i, r := range rows {
			roles[i] = r.S("role")
		}
		slices.Sort(roles)
		return refuse(RoleBindingMismatch, "%s holds live bindings at %s, and one task holds one role. Nothing was sent and no turn was started, because checking its authorization against either of them would report a clean answer derived from an arbitrary choice. Resolve the bindings first.", store.PyRepr(taskID), reprList(roles))
	}
	return refuse(RolePolicyUnconfigured, "%s is bound as %s and this process cannot read a role policy to check its authorization against: CODEX_THREAD_BRIDGE_EXECUTION_POLICY is not set in this process, so no role policy can be read. Nothing was sent and no turn was started. Set the policy for this process and the held deliveries resume on the next pass.", store.PyRepr(taskID), store.PyRepr(rows[0].S("role")))
}

// AuthorizedSettings is delivery.authorized_settings.
func AuthorizedSettings(ctx context.Context, s *store.Store, taskID string, gate RoleGate) (*TaskSettings, error) {
	row, err := one(ctx, s, "SELECT settings FROM authorized_settings WHERE task_id = ?", taskID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, refuse(SettingsUnavailable, "no authorized settings recorded for %s; register them from the creation result before a send can preserve them", store.PyRepr(taskID))
	}
	data := loadsObj(row.S("settings"))
	settings := &TaskSettings{Data: data}
	if err := settings.RequireUsable(); err != nil {
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
