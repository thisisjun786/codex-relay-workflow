package configguard

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// SwitchKeyRecord is one Codex config key `crw install switch` changed and the state it found it in.
// PriorLine is the key's line verbatim (nil when the key was absent from its table), so the undo gives
// the byte back; TablePresent false means the table did not exist and the switch left it alone.
type SwitchKeyRecord struct {
	Table, Key   string
	TablePresent bool
	PriorLine    *string
	AppliedValue string
}

// SwitchRoleRecord is one ~/.codex/agents/<role>.toml `crw install switch` handled. PriorOwner is the
// owner it found (a role.Owner value); BackupPath is the copy of a CXC-owned file taken before the CRW
// role file replaced it; AppliedDigest is the SHA-256 of the file the switch installed, "" when it
// installed none.
type SwitchRoleRecord struct {
	Role, Path, PriorOwner string
	BackupPath             *string
	AppliedDigest          string
}

// SwitchRecord is the `switch` section of the install manifest. Keys and Roles hold the values from
// before the first switch to crw, and stay until the switch back to cxc has restored them, so a
// switch that died half way is finished or undone from what it recorded. Pending is true from the
// first change until the switch has completed.
type SwitchRecord struct {
	Active, ChangedAt, By string
	Pending               bool
	ConfigBackup          *string
	Keys                  []SwitchKeyRecord
	Roles                 []SwitchRoleRecord
}

// captured reports whether the record holds the pre-switch values.
func (r *SwitchRecord) captured() bool { return r != nil && len(r.Roles) > 0 }

func switchOptionalString(v any, present bool) (*string, bool) {
	if !present || v == nil {
		return nil, true
	}
	s, ok := v.(string)
	if !ok {
		return nil, false
	}
	return &s, true
}

func parseSwitchRecord(raw any) *SwitchRecord {
	o, ok := raw.(pyjson.Object)
	if !ok {
		return nil
	}
	active, ok1 := o.Get("active").(string)
	changedAt, ok2 := o.Get("changedAt").(string)
	by, ok3 := o.Get("by").(string)
	if !ok1 || !ok2 || !ok3 || (active != "crw" && active != "cxc") {
		return nil
	}
	pending, _ := o.Get("pending").(bool)
	backupRaw, present := o.Lookup("configBackup")
	backup, ok := switchOptionalString(backupRaw, present)
	if !ok {
		return nil
	}
	rec := &SwitchRecord{Active: active, ChangedAt: changedAt, By: by, Pending: pending, ConfigBackup: backup}
	keys, _ := o.Get("keys").([]any)
	for _, item := range keys {
		k, ok := item.(pyjson.Object)
		if !ok {
			return nil
		}
		table, ok1 := k.Get("table").(string)
		key, ok2 := k.Get("key").(string)
		applied, ok3 := k.Get("appliedValue").(string)
		tablePresent, ok4 := k.Get("tablePresent").(bool)
		priorRaw, priorPresent := k.Lookup("priorLine")
		prior, ok5 := switchOptionalString(priorRaw, priorPresent)
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
			return nil
		}
		rec.Keys = append(rec.Keys, SwitchKeyRecord{table, key, tablePresent, prior, applied})
	}
	roles, _ := o.Get("roles").([]any)
	for _, item := range roles {
		r, ok := item.(pyjson.Object)
		if !ok {
			return nil
		}
		name, ok1 := r.Get("role").(string)
		path, ok2 := r.Get("path").(string)
		owner, ok3 := r.Get("priorOwner").(string)
		digest, ok4 := r.Get("appliedDigest").(string)
		backupRaw, backupPresent := r.Lookup("backupPath")
		backup, ok5 := switchOptionalString(backupRaw, backupPresent)
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
			return nil
		}
		rec.Roles = append(rec.Roles, SwitchRoleRecord{name, path, owner, backup, digest})
	}
	return rec
}

func switchRecordObject(r *SwitchRecord) pyjson.Object {
	keys, roles := []any{}, []any{}
	for _, k := range r.Keys {
		keys = append(keys, pyjson.Object{{Key: "table", Value: k.Table}, {Key: "key", Value: k.Key}, {Key: "tablePresent", Value: k.TablePresent}, {Key: "priorLine", Value: manifestNullable(k.PriorLine)}, {Key: "appliedValue", Value: k.AppliedValue}})
	}
	for _, x := range r.Roles {
		roles = append(roles, pyjson.Object{{Key: "role", Value: x.Role}, {Key: "path", Value: x.Path}, {Key: "priorOwner", Value: x.PriorOwner}, {Key: "backupPath", Value: manifestNullable(x.BackupPath)}, {Key: "appliedDigest", Value: x.AppliedDigest}})
	}
	return pyjson.Object{{Key: "active", Value: r.Active}, {Key: "changedAt", Value: r.ChangedAt}, {Key: "by", Value: r.By}, {Key: "pending", Value: r.Pending}, {Key: "configBackup", Value: manifestNullable(r.ConfigBackup)}, {Key: "keys", Value: keys}, {Key: "roles", Value: roles}}
}
