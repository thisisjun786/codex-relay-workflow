package registry

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// settings.py vocabulary. The codes are machine-consumed; SettingsDifferAfterLoad and the
// hold texts are caller-visible and kept byte-identical to settings.py.
const (
	SettingsUnavailable             = "settings_unavailable"
	SettingsIncomplete              = "settings_incomplete"
	SettingsMistyped                = "settings_mistyped"
	SettingsNotPreserved            = "settings_not_preserved"
	SettingUnobservable             = "setting_unobservable"
	EnvironmentsUnknown             = "environments_unknown"
	UnverifiablePermissionProfile   = "unverifiable_permission_profile"
	UnsupportedSandboxType          = "unsupported_sandbox_type"
	UnsupportedApprovalPolicy       = "unsupported_approval_policy"
	SettingsDifferAfterLoad         = "settings_differ_after_load"
	ApprovalPolicyDiffersFromRecord = "approval_policy_differs_from_record"
	RuntimeRootsNarrower            = "runtime_roots_narrower_than_record"
	SettingsShow                    = "settings-show"
	ShowEvent                       = "show-event"
)

// Required is settings.REQUIRED, in its order.
var Required = []string{"sandbox", "approvalPolicy", "cwd", "runtimeWorkspaceRoots", "model", "reasoningEffort", "environments"}

// fieldPrecedence is settings.FIELD_PRECEDENCE.
var fieldPrecedence = []string{"sandbox", "cwd", "runtimeWorkspaceRoots", "model", "reasoningEffort"}

// CarriedApprovalPolicies is settings.CARRIED_APPROVAL_POLICIES.
var CarriedApprovalPolicies = []string{"never", "on-request"}

var resumeSandboxMode = map[string]string{
	"workspaceWrite":   "workspace-write",
	"readOnly":         "read-only",
	"dangerFullAccess": "danger-full-access",
}

// policyConfigKeys is settings.POLICY_CONFIG_KEYS, in declaration order.
var policyConfigKeys = map[string][][3]string{
	"workspaceWrite": {
		{"writableRoots", "sandbox_workspace_write", "writable_roots"},
		{"networkAccess", "sandbox_workspace_write", "network_access"},
		{"excludeTmpdirEnvVar", "sandbox_workspace_write", "exclude_tmpdir_env_var"},
		{"excludeSlashTmp", "sandbox_workspace_write", "exclude_slash_tmp"},
	},
}

// policyDefaults is settings.POLICY_DEFAULTS, each in declaration order.
var policyDefaults = map[string]contract.OrderedObject{
	"workspaceWrite": {{Key: "writableRoots", Value: []any{}}, {Key: "networkAccess", Value: false},
		{Key: "excludeTmpdirEnvVar", Value: false}, {Key: "excludeSlashTmp", Value: false}},
	"readOnly":         {{Key: "networkAccess", Value: false}},
	"externalSandbox":  {{Key: "networkAccess", Value: "restricted"}},
	"dangerFullAccess": {},
}

func settingsRefusal(reason, detail string) error {
	return &store.RefusedError{Reason: reason, Detail: detail}
}

// NormalisePolicy is settings.normalise_policy: total, nil for anything unreadable.
func NormalisePolicy(policy any) contract.OrderedObject {
	object, ok := policy.(contract.OrderedObject)
	if !ok {
		return nil
	}
	kindValue, _ := getField(object, "type")
	kind, ok := kindValue.(string)
	if !ok {
		return nil
	}
	declared := policyDefaults[kind]
	merged := copyObject(declared)
	for _, field := range object {
		if field.Key != "type" {
			merged = setField(merged, field.Key, field.Value)
		}
	}
	merged = setField(merged, "type", kind)
	for _, field := range declared {
		value, _ := getField(merged, field.Key)
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
	if roots, ok := getField(merged, "writableRoots"); ok {
		if !textList(roots) {
			return nil
		}
		merged = setField(merged, "writableRoots", append([]any{}, roots.([]any)...))
	}
	return merged
}

// normaliseEnvironments is settings.normalise_environments over an already-readable list.
func normaliseEnvironments(environments any) []any {
	list, ok := environments.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(list))
	for _, entry := range list {
		one := copyObject(entry.(contract.OrderedObject))
		roots, present := getField(one, "runtimeWorkspaceRoots")
		if present && roots != nil {
			one = setField(one, "runtimeWorkspaceRoots", append([]any{}, roots.([]any)...))
		} else {
			cwd, _ := getField(one, "cwd")
			one = setField(one, "runtimeWorkspaceRoots", []any{cwd})
		}
		out = append(out, one)
	}
	return out
}

// shape is settings._shape.
func shape(value any) string {
	if list, ok := value.([]any); ok {
		for _, one := range list {
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

// environmentsProblem is settings.environments_problem: (where, what) or ok=false when readable.
func environmentsProblem(environments any) (string, string, bool) {
	list, ok := environments.([]any)
	if !ok {
		return "", "is " + shape(environments) + ", not a list of environment objects", true
	}
	for index, entry := range list {
		at := "[" + itoa(index) + "]"
		object, ok := entry.(contract.OrderedObject)
		if !ok {
			return at, "is " + shape(entry) + ", not an object", true
		}
		for _, key := range []string{"environmentId", "cwd"} {
			value, _ := getField(object, key)
			if _, ok := value.(string); !ok {
				return at + "." + key, "is " + shape(value) + ", not str", true
			}
		}
		if roots, present := getField(object, "runtimeWorkspaceRoots"); present && !textList(roots) {
			return at + ".runtimeWorkspaceRoots", "is " + shape(roots) + ", not a list of str", true
		}
	}
	return "", "", false
}

// TaskSettings is settings.TaskSettings: a recorded row, complete or unusable.
type TaskSettings struct{ Data contract.OrderedObject }

func (s TaskSettings) get(key string) any { v, _ := getField(s.Data, key); return v }

// Missing is TaskSettings.missing.
func (s TaskSettings) Missing() []string {
	absent := []string{}
	for _, field := range Required {
		if s.get(field) == nil {
			absent = append(absent, field)
		}
	}
	return absent
}

// mistyped is TaskSettings.mistyped; meaningful once Missing is empty.
func (s TaskSettings) mistyped() []string {
	var wrong []string
	for _, field := range []string{"cwd", "model", "reasoningEffort"} {
		if _, ok := s.get(field).(string); !ok {
			wrong = append(wrong, field)
		}
	}
	if !textList(s.get("runtimeWorkspaceRoots")) {
		wrong = append(wrong, "runtimeWorkspaceRoots")
	}
	if _, _, bad := environmentsProblem(s.get("environments")); bad {
		wrong = append(wrong, "environments")
	}
	return wrong
}

func (s TaskSettings) mistypedDetail(field string) string {
	value := s.get(field)
	switch field {
	case "runtimeWorkspaceRoots":
		return "runtimeWorkspaceRoots is " + shape(value) + ", not a list of str"
	case "environments":
		where, what, _ := environmentsProblem(value)
		return "environments" + where + " " + what
	}
	return field + " is " + pyTypeName(value) + ", not str"
}

// RequireUsable is TaskSettings.require_usable: nil, or the refusal with its reason and detail.
func (s TaskSettings) RequireUsable() error {
	if absent := s.Missing(); len(absent) > 0 {
		return settingsRefusal(SettingsIncomplete, "missing "+strings.Join(absent, ", "))
	}
	if wrong := s.mistyped(); len(wrong) > 0 {
		details := make([]string, len(wrong))
		for i, field := range wrong {
			details[i] = s.mistypedDetail(field)
		}
		return settingsRefusal(SettingsMistyped, strings.Join(details, "; "))
	}
	approval := s.get("approvalPolicy")
	if text, ok := approval.(string); !ok || !contains(CarriedApprovalPolicies, text) {
		quoted := make([]string, len(CarriedApprovalPolicies))
		for i, p := range CarriedApprovalPolicies {
			quoted[i] = pyStr(p)
		}
		return settingsRefusal(UnsupportedApprovalPolicy, "the recorded approvalPolicy is "+pyRepr(approval)+
			"; this transport carries only "+strings.Join(quoted, " and ")+
			", leaving every approval a turn raises with the thread's own approver")
	}
	if s.SandboxMode() == "" {
		recorded := s.get("sandbox")
		object, ok := recorded.(contract.OrderedObject)
		if !ok {
			return settingsRefusal(UnsupportedSandboxType, "the recorded sandbox is "+pyTypeName(recorded)+
				", not the policy object a creation result reports, so it does not record the full policy a resume would have to restore")
		}
		kind, _ := getField(object, "type")
		return settingsRefusal(UnsupportedSandboxType, pyRepr(kind)+" has no ThreadResumeParams.sandbox mode, so it cannot be restored on a resume")
	}
	if NormalisePolicy(s.get("sandbox")) == nil {
		return settingsRefusal(UnsupportedSandboxType, "the recorded sandbox policy cannot be read in full, so no response could confirm it")
	}
	return nil
}

// SandboxMode is TaskSettings.sandbox_mode; "" is Python's None.
func (s TaskSettings) SandboxMode() string {
	object, ok := s.get("sandbox").(contract.OrderedObject)
	if !ok {
		return ""
	}
	kind, _ := getField(object, "type")
	text, ok := kind.(string)
	if !ok {
		return ""
	}
	return resumeSandboxMode[text]
}

// ResumeParams is TaskSettings.resume_params.
func (s TaskSettings) ResumeParams(threadID string) contract.OrderedObject {
	config := contract.OrderedObject{{Key: "model_reasoning_effort", Value: s.get("reasoningEffort")}}
	var sandbox any
	if mode := s.SandboxMode(); mode != "" {
		sandbox = mode
	}
	roots, _ := s.get("runtimeWorkspaceRoots").([]any)
	params := contract.OrderedObject{
		{Key: "threadId", Value: threadID},
		{Key: "excludeTurns", Value: true},
		{Key: "sandbox", Value: sandbox},
		{Key: "cwd", Value: s.get("cwd")},
		{Key: "runtimeWorkspaceRoots", Value: append([]any{}, roots...)},
		{Key: "model", Value: s.get("model")},
	}
	policy := NormalisePolicy(s.get("sandbox"))
	kind, _ := getField(policy, "type")
	kindText, _ := kind.(string)
	for _, key := range policyConfigKeys[kindText] {
		if value, ok := getField(policy, key[0]); ok {
			section, _ := getField(config, key[1])
			sectionObject, _ := section.(contract.OrderedObject)
			config = setField(config, key[1], setField(sectionObject, key[2], value))
		}
	}
	return append(params, contract.Field{Key: "config", Value: config})
}

func finding(code, field string, expected, returned any, extra ...contract.Field) contract.OrderedObject {
	out := contract.OrderedObject{{Key: "code", Value: code}, {Key: "field", Value: field},
		{Key: "expected", Value: expected}, {Key: "returned", Value: returned}}
	return append(out, extra...)
}

// Mismatches is TaskSettings.mismatches: ordered findings against a resume response.
func (s TaskSettings) Mismatches(response any, transmitted, exactApprovalPolicy, loadedBefore bool) []contract.OrderedObject {
	object, ok := response.(contract.OrderedObject)
	if !ok {
		return []contract.OrderedObject{finding(SettingUnobservable, "response", "a resume response object", nil,
			contract.Field{Key: "returnedShape", Value: pyTypeName(response)})}
	}
	returnedPolicy, _ := getField(object, "approvalPolicy")
	if returnedPolicy == nil {
		return []contract.OrderedObject{finding(SettingUnobservable, "approvalPolicy", s.get("approvalPolicy"), nil)}
	}
	label := func() any {
		if text, ok := returnedPolicy.(string); ok {
			return text
		}
		return "granular"
	}
	if exactApprovalPolicy && !jsonEqual(returnedPolicy, s.get("approvalPolicy")) {
		return []contract.OrderedObject{finding(UnsupportedApprovalPolicy, "approvalPolicy", s.get("approvalPolicy"), label(),
			contract.Field{Key: "returnedShape", Value: pyTypeName(returnedPolicy)})}
	}
	if text, ok := returnedPolicy.(string); !ok || !contains(CarriedApprovalPolicies, text) {
		return []contract.OrderedObject{finding(UnsupportedApprovalPolicy, "approvalPolicy", anyStrings(CarriedApprovalPolicies), label(),
			contract.Field{Key: "returnedShape", Value: pyTypeName(returnedPolicy)})}
	}
	var found []contract.OrderedObject
	threadValue, _ := getField(object, "thread")
	if threadValue == nil {
		threadValue = contract.OrderedObject{}
	}
	thread, ok := threadValue.(contract.OrderedObject)
	if !ok {
		return []contract.OrderedObject{finding(SettingUnobservable, "environments", s.get("environments"), nil,
			contract.Field{Key: "returnedShape", Value: "thread is " + pyTypeName(threadValue)})}
	}
	returnedEnvironments, _ := getField(thread, "environments")
	if returnedEnvironments == nil {
		return []contract.OrderedObject{finding(EnvironmentsUnknown, "environments", s.get("environments"), nil)}
	}
	if where, what, bad := environmentsProblem(returnedEnvironments); bad {
		return []contract.OrderedObject{finding(SettingUnobservable, "environments", s.get("environments"), nil,
			contract.Field{Key: "returnedShape", Value: "environments" + strings.TrimRight(where+" "+what, " ")})}
	}
	gotEnvironments := normaliseEnvironments(returnedEnvironments)
	narrowable := !transmitted || loadedBefore
	if _, _, bad := environmentsProblem(s.get("environments")); bad {
		found = append(found, finding(SettingsNotPreserved, "environments", s.get("environments"), gotEnvironments))
	} else {
		expected := normaliseEnvironments(s.get("environments"))
		agree := canonical(gotEnvironments) == canonical(expected)
		if narrowable {
			agree = environmentsWithin(gotEnvironments, expected)
		}
		if !agree {
			found = append(found, finding(SettingsNotPreserved, "environments", expected, gotEnvironments))
		}
	}
	recordedRoots := s.get("runtimeWorkspaceRoots")
	rootsReadable := textList(recordedRoots)
	var expectedSandbox any
	if policy := NormalisePolicy(s.get("sandbox")); policy != nil {
		expectedSandbox = policy
	}
	expectations := map[string]any{"sandbox": expectedSandbox, "cwd": s.get("cwd"), "runtimeWorkspaceRoots": recordedRoots,
		"model": s.get("model"), "reasoningEffort": s.get("reasoningEffort")}
	for _, field := range fieldPrecedence {
		expected := expectations[field]
		raw, _ := getField(object, field)
		if raw == nil {
			found = append(found, finding(SettingUnobservable, field, expected, nil))
			continue
		}
		returned := raw
		if field == "sandbox" {
			normalised := NormalisePolicy(raw)
			if normalised == nil || expected == nil {
				shown := expected
				if shown == nil {
					shown = s.get("sandbox")
				}
				found = append(found, finding(SettingsNotPreserved, field, shown, raw))
				continue
			}
			returned = normalised
			if narrowable && sandboxWithin(normalised, expected.(contract.OrderedObject)) {
				continue
			}
		}
		if field == "runtimeWorkspaceRoots" {
			if !textList(returned) {
				found = append(found, finding(SettingUnobservable, field, expected, nil, contract.Field{Key: "returnedShape", Value: shape(returned)}))
				continue
			}
			if narrowable && rootsReadable && rootsWithin(returned.([]any), recordedRoots.([]any)) {
				continue
			}
		}
		if canonical(expected) != canonical(returned) {
			found = append(found, finding(SettingsNotPreserved, field, expected, returned))
		}
	}
	profile, _ := getField(object, "activePermissionProfile")
	expectedProfile := s.get("expectedPermissionProfile")
	if profile == nil && expectedProfile != nil {
		found = append(found, finding(SettingUnobservable, "activePermissionProfile", expectedProfile, nil))
	} else if profile != nil && canonical(profile) != canonical(expectedProfile) {
		found = append(found, finding(UnverifiablePermissionProfile, "activePermissionProfile", expectedProfile, profile))
	}
	return found
}

func rootsWithin(returned, recorded []any) bool {
	for _, root := range returned {
		found := false
		for _, allowed := range recorded {
			if root == allowed {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func sandboxWithin(returned, recorded contract.OrderedObject) bool {
	rt, _ := getField(returned, "type")
	dt, _ := getField(recorded, "type")
	if rt != "workspaceWrite" || dt != "workspaceWrite" {
		return false
	}
	if canonical(dropField(returned, "writableRoots")) != canonical(dropField(recorded, "writableRoots")) {
		return false
	}
	got, _ := getField(returned, "writableRoots")
	allowed, _ := getField(recorded, "writableRoots")
	return textList(got) && textList(allowed) && rootsWithin(got.([]any), allowed.([]any))
}

func environmentsWithin(returned, recorded []any) bool {
	if returned == nil || recorded == nil || len(returned) != len(recorded) {
		return false
	}
	for i := range returned {
		got, allowed := returned[i].(contract.OrderedObject), recorded[i].(contract.OrderedObject)
		if canonical(dropField(got, "runtimeWorkspaceRoots")) != canonical(dropField(allowed, "runtimeWorkspaceRoots")) {
			return false
		}
		gr, _ := getField(got, "runtimeWorkspaceRoots")
		ar, _ := getField(allowed, "runtimeWorkspaceRoots")
		if !rootsWithin(gr.([]any), ar.([]any)) {
			return false
		}
	}
	return true
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	return strings.TrimSpace(strings.Replace(pyRepr(int64(n)), " ", "", -1))
}

// RootsNarrowing is TaskSettings.roots_narrowing: a note for each place a resume reported fewer
// roots than the record (a strict subset), read only after Mismatches found nothing. statusBefore
// is the recipient's status before the resume, nil when unknown.
func (s TaskSettings) RootsNarrowing(response any, statusBefore any) []contract.OrderedObject {
	var notes []contract.OrderedObject
	object, ok := response.(contract.OrderedObject)
	if !ok {
		return notes
	}
	note := func(field string, recorded, observed any) {
		if !textList(recorded) || !textList(observed) {
			return
		}
		rec, obs := recorded.([]any), observed.([]any)
		inRecord := map[any]bool{}
		for _, r := range rec {
			inRecord[r] = true
		}
		inObserved := map[any]bool{}
		for _, o := range obs {
			if !inRecord[o] {
				return
			}
			inObserved[o] = true
		}
		if len(inObserved) >= len(inRecord) {
			return
		}
		notes = append(notes, contract.OrderedObject{{Key: "code", Value: RuntimeRootsNarrower}, {Key: "field", Value: field},
			{Key: "recorded", Value: append([]any{}, rec...)}, {Key: "observed", Value: append([]any{}, obs...)},
			{Key: "statusBeforeResume", Value: statusBefore}})
	}
	observedRoots, _ := getField(object, "runtimeWorkspaceRoots")
	note("runtimeWorkspaceRoots", s.get("runtimeWorkspaceRoots"), observedRoots)
	var returned any
	if thread, ok := getField(object, "thread"); ok {
		if t, ok := thread.(contract.OrderedObject); ok {
			returned, _ = getField(t, "environments")
		}
	}
	recorded := s.get("environments")
	_, _, badReturned := environmentsProblem(returned)
	_, _, badRecorded := environmentsProblem(recorded)
	if returned != nil && !badReturned && !badRecorded {
		got, allowed := normaliseEnvironments(returned), normaliseEnvironments(recorded)
		for i := 0; i < len(got) && i < len(allowed); i++ {
			gr, _ := getField(got[i].(contract.OrderedObject), "runtimeWorkspaceRoots")
			ar, _ := getField(allowed[i].(contract.OrderedObject), "runtimeWorkspaceRoots")
			note("environments["+itoa(i)+"].runtimeWorkspaceRoots", ar, gr)
		}
	}
	sandbox, _ := getField(object, "sandbox")
	gotPolicy, recordedPolicy := NormalisePolicy(sandbox), NormalisePolicy(s.get("sandbox"))
	if gotPolicy != nil && recordedPolicy != nil {
		gt, _ := getField(gotPolicy, "type")
		rt, _ := getField(recordedPolicy, "type")
		if gt == "workspaceWrite" && rt == "workspaceWrite" {
			rw, _ := getField(recordedPolicy, "writableRoots")
			gw, _ := getField(gotPolicy, "writableRoots")
			note("sandbox.writableRoots", rw, gw)
		}
	}
	return notes
}

// TransportSettingsRefusals is transport.SETTINGS_REFUSALS: the resume refusal codes that are
// completed pre-send refusals (withheld_pre_send, retry-safe), never uncertain outcomes.
var TransportSettingsRefusals = []string{SettingsNotPreserved, SettingUnobservable, EnvironmentsUnknown, UnverifiablePermissionProfile, SettingsDifferAfterLoad}

// IsPreSendSettingsRefusal reports whether a resume refusal code withholds before any send.
func IsPreSendSettingsRefusal(code string) bool { return contains(TransportSettingsRefusals, code) }

// RecordSettingsViolation is DeliveryService.record_settings_violation: a dispatch that already
// reached a turn is annotated, never reclassified; the delivery row is not touched.
func (r *Registry) RecordSettingsViolation(ctx context.Context, requestID, eventID string, findings []any) (contract.OrderedObject, error) {
	now := r.now()
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		if err := r.Store.RecordSettingsViolation(ctx, store.AttemptSettingsViolationsRow{RequestID: requestID, EventID: eventID,
			Findings: pyDumps(findings, false), ObservedAt: now}); err != nil {
			return err
		}
		return journal(ctx, r.Store, "dispatch_settings_violation", eventID, contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "findings", Value: findings}}, now)
	})
	if err != nil {
		return nil, err
	}
	return contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "eventId", Value: eventID}, {Key: "findings", Value: findings}}, nil
}

// SettingsViolation is DeliveryService.settings_violation; nil for an unknown request.
func (r *Registry) SettingsViolation(ctx context.Context, requestID string) (contract.OrderedObject, error) {
	row, err := r.Store.SettingsViolation(ctx, requestID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	findings, err := decodeJSON([]byte(row.Findings))
	if err != nil {
		return nil, err
	}
	return contract.OrderedObject{{Key: "findings", Value: findings}, {Key: "observedAt", Value: row.ObservedAt}}, nil
}
