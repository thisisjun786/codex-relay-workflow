package configguard

import (
	"encoding/json"
	"math"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

const InstallManifestName = ".crw-install.json"

// The ownership field spellings are the oracle's on-disk schema; the name table does not rename them.
type FailureRecord struct {
	ExitCode float64
	Message  string
}
type FlagRecord struct {
	PriorEnabled, EnabledByCodexclaw, EnableFailed bool
	Failure                                        *FailureRecord
}
type TableKeyRecord struct {
	Table, Key     string
	PriorValue     *string
	AppliedValue   string
	SetByCodexclaw bool
}
type InstallManifest struct {
	Version                      int
	ActivatedAt, ConfigPath      string
	BackupPath, PostActivateHash *string
	Flags                        map[string]FlagRecord
	TableKeys                    map[string]TableKeyRecord
	// ReleasedAt is set by a deactivation that reverted everything it owned (CRW-1145): the records stay as evidence, but
	// they no longer describe ownership, so the next activation starts a new baseline and a second deactivation has nothing to
	// revert. It is written only when set, so a manifest without it keeps the oracle's bytes.
	ReleasedAt *string
	// Unchanged marks an activation that found nothing to change and published nothing; RunBackupPath is the backup this run
	// wrote. Neither is written.
	Unchanged     bool
	RunBackupPath *string
	// Recovered names what this command recorded of an interrupted earlier change (CRW-1153); it is never written.
	Recovered             []string
	flagOrder, tableOrder []string
}

func manifestPath(home string) string { return filepath.Join(home, InstallManifestName) }
func manifestString(v any) *string {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return &s
}
func manifestNumber(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, _ := strconv.ParseFloat(string(n), 64)
	return f, true
}

// parseInstallManifest normalizes only the shapes the oracle checks. Accepted strings retain
// lone UTF-16 surrogates as WTF-8, including original restoration values carried by Activate.
func parseInstallManifest(input string) *InstallManifest {
	raw, err := pyjson.Loads(input, pyjson.LoadOptions{Surrogates: true, Numbers: pyjson.SpelledNumbers, Deep: true})
	if err != nil {
		return nil
	}
	o, ok := raw.(pyjson.Object)
	if !ok {
		return nil
	}
	version, _ := manifestNumber(o.Get("version"))
	path := manifestString(o.Get("configPath"))
	flags, ok := o.Get("flags").(pyjson.Object)
	if (version != 1 && version != 2) || path == nil || !ok {
		return nil
	}
	m := &InstallManifest{Version: int(version), ConfigPath: *path, ActivatedAt: pyjson.Text(o.Get("activatedAt")), BackupPath: manifestString(o.Get("backupPath")), PostActivateHash: manifestString(o.Get("postActivateHash")), ReleasedAt: manifestString(o.Get("releasedAt")), Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{}}
	for _, entry := range flags {
		rec, object := entry.Value.(pyjson.Object)
		_, array := entry.Value.([]any)
		if !object && !array {
			return nil
		}
		flag := FlagRecord{PriorEnabled: rec.Get("priorEnabled") == true, EnabledByCodexclaw: rec.Get("enabledByCodexclaw") == true, EnableFailed: rec.Get("enableFailed") == true}
		if f, ok := rec.Get("failure").(pyjson.Object); ok {
			if code, ok := manifestNumber(f.Get("exitCode")); ok {
				flag.Failure = &FailureRecord{code, pyjson.Text(f.Get("message"))}
			}
		}
		// Assignment to __proto__ changes the oracle object's prototype, not its own entries.
		if entry.Key != "__proto__" {
			m.Flags[entry.Key] = flag
			m.flagOrder = append(m.flagOrder, entry.Key)
		}
	}
	if raw, present := o.Lookup("tableKeys"); present {
		keys, ok := raw.(pyjson.Object)
		if !ok {
			return nil
		}
		for _, entry := range keys {
			rec, ok := entry.Value.(pyjson.Object)
			if !ok {
				return nil
			}
			table, key, applied := manifestString(rec.Get("table")), manifestString(rec.Get("key")), manifestString(rec.Get("appliedValue"))
			prior, present := rec.Lookup("priorValue")
			value := manifestString(prior)
			if table == nil || key == nil || applied == nil || !present || (prior != nil && value == nil) {
				return nil
			}
			if entry.Key != "__proto__" {
				m.TableKeys[entry.Key] = TableKeyRecord{*table, *key, value, *applied, rec.Get("setByCodexclaw") == true}
				m.tableOrder = append(m.tableOrder, entry.Key)
			}
		}
	}
	return m
}

func manifestIndex(key string) (uint64, bool) {
	n, e := strconv.ParseUint(key, 10, 32)
	return n, e == nil && n < math.MaxUint32 && strconv.FormatUint(n, 10) == key
}
func manifestOrder[V any](order []string, values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for _, k := range order {
		if _, ok := values[k]; ok && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	missing := make([]string, 0)
	for k := range values {
		if !slices.Contains(keys, k) {
			missing = append(missing, k)
		}
	}
	slices.Sort(missing)
	keys = append(keys, missing...)
	slices.SortStableFunc(keys, func(a, b string) int {
		ai, ax := manifestIndex(a)
		bi, bx := manifestIndex(b)
		if ax && bx {
			if ai < bi {
				return -1
			}
			if ai > bi {
				return 1
			}
		}
		if ax && !bx {
			return -1
		}
		if bx && !ax {
			return 1
		}
		return 0
	})
	return keys
}
func manifestNullable(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func manifestBytes(m *InstallManifest) ([]byte, error) {
	flags, keys := pyjson.Object{}, pyjson.Object{}
	for _, key := range manifestOrder(m.flagOrder, m.Flags) {
		f := m.Flags[key]
		rec := pyjson.Object{{Key: "priorEnabled", Value: f.PriorEnabled}, {Key: "enabledByCodexclaw", Value: f.EnabledByCodexclaw}, {Key: "enableFailed", Value: f.EnableFailed}}
		if f.Failure != nil {
			var code any
			if !math.IsInf(f.Failure.ExitCode, 0) && !math.IsNaN(f.Failure.ExitCode) {
				n := f.Failure.ExitCode
				if n == 0 {
					n = 0
				}
				b, e := role.Stringify(n, "")
				if e != nil {
					return nil, e
				}
				code = json.Number(b)
			}
			rec = append(rec, pyjson.Field{Key: "failure", Value: pyjson.Object{{Key: "exitCode", Value: code}, {Key: "message", Value: f.Failure.Message}}})
		}
		flags = append(flags, pyjson.Field{Key: key, Value: rec})
	}
	for _, id := range manifestOrder(m.tableOrder, m.TableKeys) {
		r := m.TableKeys[id]
		keys = append(keys, pyjson.Field{Key: id, Value: pyjson.Object{{Key: "table", Value: r.Table}, {Key: "key", Value: r.Key}, {Key: "priorValue", Value: manifestNullable(r.PriorValue)}, {Key: "appliedValue", Value: r.AppliedValue}, {Key: "setByCodexclaw", Value: r.SetByCodexclaw}}})
	}
	body := pyjson.Object{{Key: "version", Value: m.Version}, {Key: "activatedAt", Value: m.ActivatedAt}, {Key: "configPath", Value: m.ConfigPath}, {Key: "backupPath", Value: manifestNullable(m.BackupPath)}, {Key: "postActivateHash", Value: manifestNullable(m.PostActivateHash)}, {Key: "flags", Value: flags}, {Key: "tableKeys", Value: keys}}
	if m.ReleasedAt != nil {
		body = append(body, pyjson.Field{Key: "releasedAt", Value: *m.ReleasedAt})
	}
	b, e := pyjson.Encode(body, pyjson.Options{Indent: 2, Unicode: true})
	if e != nil {
		return nil, e
	}
	return append(b, '\n'), nil
}
