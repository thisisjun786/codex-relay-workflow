package delivery

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// TaskSettings is settings.TaskSettings as a send carries it: the recorded execution settings and
// whether the recipient resumes without them. The rules that read a record (what is complete, what is
// usable, how a resume is shaped and compared) are registry.TaskSettings', the one set of settings
// rules the relay has; this type holds only what the sender adds to the record.
type TaskSettings struct {
	Data               Obj
	SettingsFreeResume bool
}

// ResumeParams is the registry's resume_params over this record: only fields ThreadResumeParams
// defines, and never an approvalPolicy - the host would apply it to a thread the resume loads.
func (t TaskSettings) ResumeParams(thread string) Obj {
	return registry.TaskSettings{Data: t.Data}.ResumeParams(thread)
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
