package delivery

import (
	"slices"
)

// Settings verification of a resume response (settings.TaskSettings.mismatches,
// approval_divergence, roots_narrowing): the detector the guarded send runs between
// thread/resume and turn/start. The wire half of that send - thread/read, the resume, the ledger
// and turn/start - is the bridge adapter's (todo 28), which calls VerifyResume.

const (
	SettingsNotPreserved          = "settings_not_preserved"
	SettingUnobservable           = "setting_unobservable"
	EnvironmentsUnknown           = "environments_unknown"
	UnverifiablePermissionProfile = "unverifiable_permission_profile"
	SettingsDifferAfterLoad       = "settings_differ_after_load"
	ApprovalDiffersFromRecord     = "approval_policy_differs_from_record"
	RuntimeRootsNarrower          = "runtime_roots_narrower_than_record"
)

func canonicalJSON(v any) string { return dumpsSorted(v) }

func textListOf(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, x := range a {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func rootsWithin(returned, recorded any) bool {
	allowed := textListOf(recorded)
	for _, r := range textListOf(returned) {
		if !slices.Contains(allowed, r) {
			return false
		}
	}
	return true
}

func without(o Obj, key string) Obj {
	var out Obj
	for _, f := range o {
		if f.Key != key {
			out = append(out, f)
		}
	}
	return out
}

func sandboxWithin(returned, recorded Obj) bool {
	if str(returned, "type") != "workspaceWrite" || str(recorded, "type") != "workspaceWrite" {
		return false
	}
	if canonicalJSON(without(returned, "writableRoots")) != canonicalJSON(without(recorded, "writableRoots")) {
		return false
	}
	got, _ := get(returned, "writableRoots")
	allowed, _ := get(recorded, "writableRoots")
	return textList(got) && textList(allowed) && rootsWithin(got, allowed)
}

func normaliseEnvironments(v any) []any {
	list, _ := v.([]any)
	out := make([]any, 0, len(list))
	for _, e := range list {
		o := append(Obj(nil), e.(Obj)...)
		if roots, _ := get(o, "runtimeWorkspaceRoots"); roots == nil {
			cwd, _ := get(o, "cwd")
			o = set(o, "runtimeWorkspaceRoots", []any{cwd})
		}
		out = append(out, o)
	}
	return out
}

func environmentsWithin(returned, recorded []any) bool {
	if len(returned) != len(recorded) {
		return false
	}
	for i := range returned {
		got, allowed := returned[i].(Obj), recorded[i].(Obj)
		if canonicalJSON(without(got, "runtimeWorkspaceRoots")) != canonicalJSON(without(allowed, "runtimeWorkspaceRoots")) {
			return false
		}
		g, _ := get(got, "runtimeWorkspaceRoots")
		a, _ := get(allowed, "runtimeWorkspaceRoots")
		if !rootsWithin(g, a) {
			return false
		}
	}
	return true
}

func settingsFinding(code, field string, expected, returned any, extra ...F) Obj {
	o := Obj{{Key: "code", Value: code}, {Key: "field", Value: field}, {Key: "expected", Value: expected}, {Key: "returned", Value: returned}}
	return append(o, extra...)
}

// Mismatches is TaskSettings.mismatches: ordered findings against a resume response.
func (t TaskSettings) Mismatches(response any, transmitted, loadedBefore bool) []any {
	r, ok := response.(Obj)
	if !ok {
		return []any{settingsFinding(SettingUnobservable, "response", "a resume response object", nil, F{Key: "returnedShape", Value: pyTypeName(response)})}
	}
	recorded, _ := get(t.Data, "approvalPolicy")
	returned, _ := get(r, "approvalPolicy")
	if returned == nil {
		return []any{settingsFinding(SettingUnobservable, "approvalPolicy", recorded, nil)}
	}
	if p, isText := returned.(string); !isText || !slices.Contains(CarriedApprovalPolicies, p) {
		label := any("granular")
		if isText {
			label = p
		}
		return []any{settingsFinding(UnsupportedApprovalPolicy, "approvalPolicy", []any{"never", "on-request"}, label, F{Key: "returnedShape", Value: pyTypeName(returned)})}
	}
	var found []any
	environments, _ := get(t.Data, "environments")
	thread, _ := get(r, "thread")
	if thread == nil {
		thread = Obj{}
	}
	th, isObj := thread.(Obj)
	if !isObj {
		return append(found, settingsFinding(SettingUnobservable, "environments", environments, nil, F{Key: "returnedShape", Value: "thread is " + pyTypeName(thread)}))
	}
	returnedEnv, _ := get(th, "environments")
	if returnedEnv == nil {
		return append(found, settingsFinding(EnvironmentsUnknown, "environments", environments, nil))
	}
	if where, what, bad := environmentsProblem(returnedEnv); bad {
		return append(found, settingsFinding(SettingUnobservable, "environments", environments, nil, F{Key: "returnedShape", Value: "environments" + where + " " + what}))
	}
	got := normaliseEnvironments(returnedEnv)
	narrowable := !transmitted || loadedBefore
	expectedEnv := normaliseEnvironments(environments)
	if !(narrowable && environmentsWithin(got, expectedEnv) || !narrowable && canonicalJSON(got) == canonicalJSON(expectedEnv)) {
		found = append(found, settingsFinding(SettingsNotPreserved, "environments", expectedEnv, got))
	}
	sandbox, _ := get(t.Data, "sandbox")
	expectations := map[string]any{"sandbox": normalisePolicy(sandbox)}
	for _, k := range []string{"cwd", "runtimeWorkspaceRoots", "model", "reasoningEffort"} {
		expectations[k], _ = get(t.Data, k)
	}
	for _, field := range []string{"sandbox", "cwd", "runtimeWorkspaceRoots", "model", "reasoningEffort"} {
		expected := expectations[field]
		raw, _ := get(r, field)
		if raw == nil {
			found = append(found, settingsFinding(SettingUnobservable, field, expected, nil))
			continue
		}
		value := raw
		if field == "sandbox" {
			normalised := normalisePolicy(raw)
			if normalised == nil || expected.(Obj) == nil {
				found = append(found, settingsFinding(SettingsNotPreserved, field, expected, raw))
				continue
			}
			if narrowable && sandboxWithin(normalised, expected.(Obj)) {
				continue
			}
			value = normalised
		}
		if field == "runtimeWorkspaceRoots" {
			if !textList(raw) {
				found = append(found, settingsFinding(SettingUnobservable, field, expected, nil, F{Key: "returnedShape", Value: shapeOf(raw)}))
				continue
			}
			if narrowable && textList(expected) && rootsWithin(raw, expected) {
				continue
			}
		}
		if canonicalJSON(expected) != canonicalJSON(value) {
			found = append(found, settingsFinding(SettingsNotPreserved, field, expected, value))
		}
	}
	profile, _ := get(r, "activePermissionProfile")
	expectedProfile, _ := get(t.Data, "expectedPermissionProfile")
	if profile == nil && expectedProfile != nil {
		found = append(found, settingsFinding(SettingUnobservable, "activePermissionProfile", expectedProfile, nil))
	} else if profile != nil && canonicalJSON(profile) != canonicalJSON(expectedProfile) {
		found = append(found, settingsFinding(UnverifiablePermissionProfile, "activePermissionProfile", expectedProfile, profile))
	}
	return found
}

// ApprovalDivergence is approval_divergence: a carried policy other than the recorded one.
func (t TaskSettings) ApprovalDivergence(response any) any {
	r, ok := response.(Obj)
	if !ok {
		return nil
	}
	observed, _ := get(r, "approvalPolicy")
	recorded, _ := get(t.Data, "approvalPolicy")
	p, isText := observed.(string)
	if observed == recorded || !isText || !slices.Contains(CarriedApprovalPolicies, p) {
		return nil
	}
	return Obj{{Key: "code", Value: ApprovalDiffersFromRecord}, {Key: "field", Value: "approvalPolicy"}, {Key: "recorded", Value: recorded}, {Key: "observed", Value: observed}}
}

// VerifyResume is the guarded send's step between thread/resume and turn/start: the refusal the
// receipt carries (rpcError, settingsFindings) or the notes an accepted send carries.
func VerifyResume(t TaskSettings, resumed any, statusBefore string) (rpcError Obj, findings []any, notes []any) {
	transmitted := !t.SettingsFreeResume
	findings = t.Mismatches(resumed, transmitted, transmitted && statusBefore == "idle")
	if len(findings) > 0 {
		first := findings[0].(Obj)
		code := str(first, "code")
		expected, _ := get(first, "expected")
		returned, _ := get(first, "returned")
		if !transmitted {
			if code == SettingsNotPreserved {
				code = SettingsDifferAfterLoad
			}
			return Obj{{Key: "code", Value: code}, {Key: "message", Value: code + ": " + str(first, "field") + " is " + pyReprValue(returned) + " on the loaded thread and " + pyReprValue(expected) + " in the record; nothing was transmitted and no turn was started"}}, findings, nil
		}
		return Obj{{Key: "code", Value: code}, {Key: "message", Value: code + ": " + str(first, "field") + " returned " + pyReprValue(returned) + "; message withheld"}}, findings, nil
	}
	if note := t.ApprovalDivergence(resumed); note != nil {
		notes = append(notes, note)
	}
	return nil, nil, notes
}
