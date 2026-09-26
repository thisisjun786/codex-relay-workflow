package registry

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// UserTransition is registry.USER_TRANSITION.
const UserTransition = "user_transition"

// Citation is the exception argument of record_settings: none, an id, or CLEAR_EXCEPTION.
type Citation struct {
	ID    string
	Set   bool // an id was cited (possibly ""), Python's exception is not None
	Clear bool
}

// SettingsRecord is record_settings' answer: {taskId, source, settings}.
func settingsAnswer(task, source string, settings contract.OrderedObject) contract.OrderedObject {
	return contract.OrderedObject{{Key: "taskId", Value: task}, {Key: "source", Value: source}, {Key: "settings", Value: settings}}
}

func findingRefusal(finding contract.OrderedObject) error {
	return refuse(contract.RefusalReason(findingText(finding, "code")), "%s", findingText(finding, "detail"))
}

func rolesList(roles []string) string { return pyRepr(anyStrings(roles)) }

// RecordSettings is registry.record_settings. role "" is None.
func (r *Registry) RecordSettings(ctx context.Context, task string, settings contract.OrderedObject, source, role string, citation Citation) (contract.OrderedObject, error) {
	settings = copyObject(settings)
	if role != "" {
		settings = setField(settings, "citedRole", role)
	}
	if citation.Clear {
		settings = dropField(settings, "citedException")
	} else if citation.Set {
		settings = setField(settings, "citedException", citation.ID)
	}
	if err := (TaskSettings{settings}).RequireUsable(); err != nil {
		return nil, err
	}
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		q := r.Store.Querier(ctx)
		var existingText string
		err := q.QueryRowContext(ctx, "SELECT settings FROM authorized_settings WHERE task_id = ?", task).Scan(&existingText)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		exists := err == nil
		var previous contract.OrderedObject
		if exists {
			decoded, err := decodeJSON([]byte(existingText))
			if err != nil {
				return err
			}
			previous, _ = decoded.(contract.OrderedObject)
			carried := citedRole(previous)
			if carried != nil && role != "" && carried != any(role) {
				return refuse(contract.RefusalRoleBindingMismatch, "%s was created citing role %s and this record states %s. The creation role is not something a later write changes; correct whichever of the two is wrong at its source",
					pyStr(task), pyRepr(carried), pyStr(role))
			}
			if carried != nil && role == "" {
				settings = setField(settings, "citedRole", carried)
			}
		}
		bound, contested, err := boundRole(ctx, r.Store, task)
		if err != nil {
			return err
		}
		if !citation.Set && exists && !citation.Clear {
			carried := citedException(previous)
			model, effort := pairOf(settings)
			prevModel, prevEffort := pairOf(previous)
			unchanged := model == prevModel && effort == prevEffort
			stillNeeded := true
			if contested == nil {
				if m, e, ok := declaredPairFor(bound, r.Policy); ok {
					stillNeeded = !(model == any(m) && effort == any(e))
				}
			}
			if carried != nil && stillNeeded && (unchanged || source != UserTransition) {
				settings = setField(settings, "citedException", carried)
			}
		}
		if contested != nil {
			return refuse(contract.RefusalRoleBindingMismatch, "%s holds live bindings at %s; one task holds one role, so there is no single role to record settings against",
				pyStr(task), rolesList(contested))
		}
		if bound != "" {
			if finding := CheckBinding(citedRole(settings), bound, settings, r.Policy); finding != nil {
				return findingRefusal(finding)
			}
		}
		now := r.now()
		if _, err := q.ExecContext(ctx, "INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)"+
			" ON CONFLICT(task_id) DO UPDATE SET settings = excluded.settings,   source = excluded.source, recorded_at = excluded.recorded_at",
			task, pyDumps(settings, true), source, now); err != nil {
			return err
		}
		return journal(ctx, r.Store, "settings_recorded", task, contract.OrderedObject{{Key: "source", Value: source}}, r.now())
	})
	if err != nil {
		return nil, err
	}
	return settingsAnswer(task, source, settings), nil
}

// LoadSettings is registry.load_settings; ok=false is None.
func (r *Registry) LoadSettings(ctx context.Context, task string) (TaskSettings, bool, error) {
	var raw string
	err := r.Store.Querier(ctx).QueryRowContext(ctx, "SELECT settings FROM authorized_settings WHERE task_id = ?", task).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskSettings{}, false, nil
	}
	if err != nil {
		return TaskSettings{}, false, err
	}
	decoded, err := decodeJSON([]byte(raw))
	if err != nil {
		return TaskSettings{}, false, &HostError{Class: "JSONDecodeError", Detail: err.Error()}
	}
	object, _ := decoded.(contract.OrderedObject)
	return TaskSettings{object}, true, nil
}

// SettingsShow is cli.cmd_settings_show.
func (r *Registry) SettingsShow(ctx context.Context, task string) (contract.OrderedObject, error) {
	settings, ok, err := r.LoadSettings(ctx, task)
	if err != nil {
		return nil, err
	}
	if !ok {
		return contract.OrderedObject{{Key: "task", Value: task}, {Key: "settings", Value: nil}, {Key: "usable", Value: false},
			{Key: "deliverable", Value: false}, {Key: "missing", Value: anyStrings(Required)}, {Key: "recordFinding", Value: nil}}, nil
	}
	bound, contested, err := boundRole(ctx, r.Store, task)
	if err != nil {
		return nil, err
	}
	policy := r.Policy
	var finding any
	switch {
	case contested != nil:
		finding = contract.OrderedObject{{Key: "code", Value: string(contract.RefusalRoleBindingMismatch)}, {Key: "boundRoles", Value: anyStrings(contested)},
			{Key: "recovery", Value: "one task holds one role; resolve these bindings before this task can be checked against either of them"}}
	case bound != "" && !policy.Declared:
		finding = contract.OrderedObject{{Key: "code", Value: string(contract.RefusalRolePolicyUnconfigured)}, {Key: "boundRole", Value: bound},
			{Key: "detail", Value: policy.Detail},
			{Key: "recovery", Value: "set this process's execution policy and restart it; deliveries held meanwhile resume on the next pass"}}
	case bound != "":
		if f := CheckRecord(settings.Data, bound, policy); f != nil {
			finding = f
		}
	}
	var recordFinding any
	if err := settings.RequireUsable(); err != nil {
		code, detail := "unexpected", err.Error()
		var refused *store.RefusedError
		if errors.As(err, &refused) {
			code, detail = refused.Reason, refused.Detail
		}
		recordFinding = contract.OrderedObject{{Key: "code", Value: code}, {Key: "detail", Value: detail}}
	}
	var boundValue, boundRoles, digest any
	if contested == nil && bound != "" {
		boundValue = bound
	}
	if contested != nil {
		boundRoles = anyStrings(contested)
	}
	rolePolicy := "unresolved"
	if policy.Declared {
		digest, rolePolicy = policy.digest, "declared"
	}
	missing := settings.Missing()
	return contract.OrderedObject{
		{Key: "task", Value: task},
		{Key: "settings", Value: settings.Data},
		{Key: "usable", Value: len(missing) == 0},
		{Key: "missing", Value: anyStrings(missing)},
		{Key: "deliverable", Value: recordFinding == nil && finding == nil},
		{Key: "recordFinding", Value: recordFinding},
		{Key: "citedRole", Value: citedRole(settings.Data)},
		{Key: "citedException", Value: citedException(settings.Data)},
		{Key: "boundRole", Value: boundValue},
		{Key: "boundRoles", Value: boundRoles},
		{Key: "rolePolicyDigest", Value: digest},
		{Key: "rolePolicy", Value: rolePolicy},
		{Key: "roleFinding", Value: finding},
	}, nil
}

// describe is rolepolicy.describe.
func describe(finding contract.OrderedObject) string {
	if v, _ := getField(finding, "undeclared"); v == true {
		return "this host's execution policy declares no such role, so its authorization cannot be checked"
	}
	_, hasRecorded := getField(finding, "recorded")
	if cited, _ := getField(finding, "citedException"); cited != nil && !hasRecorded {
		return "its record cites exception " + pyRepr(cited) + ", which this policy does not authorize for that role with this pair and directory"
	}
	if hasRecorded {
		recorded, _ := getField(finding, "recorded")
		expected, _ := getField(finding, "expected")
		return "its recorded authorization is " + pyRepr(recorded) + " while the policy for that role is " + pyRepr(expected)
	}
	if detail, ok := getField(finding, "detail"); ok {
		if text, ok := detail.(string); ok {
			return text
		}
	}
	return "its recorded authorization does not match this policy"
}

// AuthorizedSettings is delivery.authorized_settings: the recorded settings, validated, for any
// sender that resumes a task, or the pre-send refusal (reason and detail) that withholds it.
// settingsFree reports that the pair was not derived from the role's declared pair.
func (r *Registry) AuthorizedSettings(ctx context.Context, task string) (settings TaskSettings, settingsFree bool, err error) {
	settings, ok, err := r.LoadSettings(ctx, task)
	if err != nil {
		return settings, false, err
	}
	if !ok {
		return settings, false, refuse(contract.RefusalSettingsUnavailable, "no authorized settings recorded for %s; register them from the"+
			" creation result before a send can preserve them", pyStr(task))
	}
	if err := settings.RequireUsable(); err != nil {
		return settings, false, err
	}
	role, contested, err := boundRole(ctx, r.Store, task)
	if err != nil {
		return settings, false, err
	}
	if contested != nil {
		return settings, false, refuse(contract.RefusalRoleBindingMismatch, "%s holds live bindings at %s, and one task holds one role. "+
			"Nothing was sent and no turn was started, because checking its authorization against "+
			"either of them would report a clean answer derived from an arbitrary choice. Resolve "+
			"the bindings first.", pyStr(task), rolesList(contested))
	}
	if role == "" {
		return settings, false, nil
	}
	policy := r.Policy
	if !policy.Declared {
		return settings, false, refuse(contract.RefusalRolePolicyUnconfigured, "%s is bound as %s and this process cannot read a role policy to check "+
			"its authorization against: %s. Nothing was sent and no turn was started. Set the policy for this process and the held deliveries resume on the next "+
			"pass.", pyStr(task), pyStr(role), policy.Detail)
	}
	if finding := CheckRecord(settings.Data, role, policy); finding != nil {
		recovery, ok := getField(finding, "recovery")
		if !ok {
			recovery = RoleRecovery
		}
		digest, _ := getField(finding, "digest")
		return settings, false, refuse(contract.RefusalReason(findingText(finding, "code")), "%s is bound as %s: %s (policy %v). Nothing was sent and no turn was started. %v",
			pyStr(task), pyStr(role), describe(finding), digest, recovery)
	}
	expectation, ok := policy.expectation(role)
	model, effort := pairOf(settings.Data)
	derived := citedException(settings.Data) == nil && ok && expectation.Expectation == "pair" &&
		model == any(expectation.Model) && effort == any(expectation.Effort)
	return settings, !derived, nil
}
