package configguard

import "time"

type DeactivateDeps struct {
	Run                   CodexRunner
	CodexHome, ConfigPath string
	Now                   func() string
}

type SkipReason string

const (
	SkipMissing      SkipReason = "missing"
	SkipChanged      SkipReason = "changed"
	SkipUnverifiable SkipReason = "unverifiable"
)

type SkippedExternal struct {
	Target string     `json:"target"`
	Reason SkipReason `json:"reason"`
}

type DeactivateResult struct {
	Disabled                 []string          `json:"disabled"`
	SkippedPreExisting       []string          `json:"skippedPreExisting"`
	NoManifest               bool              `json:"noManifest"`
	RestoredKeys             []string          `json:"restoredKeys"`
	SkippedExternal          []SkippedExternal `json:"skippedExternal"`
	FileDrifted              bool              `json:"fileDrifted"`
	FeaturesStateUnavailable bool              `json:"featuresStateUnavailable"`
}

func readTextOrNull(path string) (*string, error) {
	b, exists, err := activationReadFile(path)
	if err != nil || !exists {
		return nil, err
	}
	s := string(b)
	return &s, nil
}

// DecideKeyRestore is deactivate.ts's per-key decision table. backupKnown=false means
// unreadable/absent backup; a known nil value means the backup confirms the key was absent.
// A successful decision has an empty reason.
func DecideKeyRestore(rec TableKeyRecord, live *string, drift, backupKnown bool, backup *string) (bool, SkipReason) {
	switch {
	case !rec.SetByCodexclaw:
		return false, SkipChanged
	case live == nil:
		return false, SkipMissing
	case *live != rec.AppliedValue:
		return false, SkipChanged
	case rec.PriorValue == nil && drift && (!backupKnown || backup != nil):
		return false, SkipUnverifiable
	default:
		return true, ""
	}
}

// Deactivate restores owned table keys before asking the injected CLI to disable flags.
// The manifest/backup remain ownership evidence, never overwritten by deactivation.
func Deactivate(deps DeactivateDeps) (*DeactivateResult, error) {
	now := deps.Now
	if now == nil {
		now = func() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
	}
	// Oracle parity: marker failure never gates uninstall, including early exits. The
	// marker API itself refuses unreadable records rather than replacing their consent data.
	_ = MarkSelfHealOptedOut(deps.CodexHome, now())
	r := &DeactivateResult{Disabled: []string{}, SkippedPreExisting: []string{}, NoManifest: true, RestoredKeys: []string{}, SkippedExternal: []SkippedExternal{}}
	raw, _ := readTextOrNull(manifestPath(deps.CodexHome))
	if raw == nil {
		return r, nil
	}
	m := parseInstallManifest(*raw)
	if m == nil {
		return r, nil
	}
	r.NoManifest = false
	path := deps.ConfigPath
	if path == "" {
		path = m.ConfigPath
	}
	if m.PostActivateHash != nil {
		hash, err := hashOrNull(path)
		if err != nil {
			return nil, err
		}
		r.FileDrifted = hash == nil || *hash != *m.PostActivateHash
	}
	// Even without a hash or managed keys, the later CLI may rewrite settings. Never
	// hand it an unreadable file: the oracle's swallowed read can lose the old settings.
	content, err := readTextOrNull(path)
	if err != nil {
		return nil, err
	}
	if content != nil && len(m.TableKeys) > 0 {
		if err := deactivateTableKeys(path, *content, m, r); err != nil {
			return nil, err
		}
	}
	live, err := ReadDeclaredState(deps.Run)
	r.FeaturesStateUnavailable = err != nil
	for _, key := range manifestOrder(m.flagOrder, m.Flags) {
		flag := m.Flags[key]
		if flag.PriorEnabled {
			r.SkippedPreExisting = append(r.SkippedPreExisting, key)
			continue
		}
		if !flag.EnabledByCodexclaw {
			continue
		}
		if enabled, present := live[key]; present && !enabled {
			r.SkippedExternal = append(r.SkippedExternal, SkippedExternal{key, SkipMissing})
			continue
		}
		if deps.Run([]string{"features", "disable", key}).ExitCode == 0 {
			r.Disabled = append(r.Disabled, key)
		}
	}
	return r, nil
}

func deactivateTableKeys(path, content string, m *InstallManifest, r *DeactivateResult) error {
	var backup *string
	if m.BackupPath != nil && *m.BackupPath != "" {
		// A read failure is unknown provenance, not evidence of an absent backup key.
		backup, _ = readTextOrNull(*m.BackupPath)
	}
	changed := false
	for _, id := range manifestOrder(m.tableOrder, m.TableKeys) {
		rec := m.TableKeys[id]
		value, found := ReadTableKey(content, rec.Table, rec.Key)
		var live, prior *string
		if found {
			live = &value
		}
		if backup != nil {
			if v, ok := ReadTableKey(*backup, rec.Table, rec.Key); ok {
				prior = &v
			}
		}
		restore, reason := DecideKeyRestore(rec, live, r.FileDrifted, backup != nil, prior)
		if !restore {
			r.SkippedExternal = append(r.SkippedExternal, SkippedExternal{id, reason})
			continue
		}
		edit := RestoreTableKey(content, rec.Table, rec.Key, rec.PriorValue)
		content, changed = edit.Content, changed || edit.Changed
		r.RestoredKeys = append(r.RestoredKeys, id)
	}
	if changed {
		return activationPublish(path, []byte(content))
	}
	return nil
}
