package configguard

import (
	"fmt"
	"os"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

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

// configLockPathsSameFile reports whether two spellings name one file on disk: the same string, or
// two paths the kernel resolves to one device and inode. The manifest is allowed to name the config
// file through a different spelling — CODEX_HOME behind a directory symlink, for example, which
// crwdir's lock resolution leaves spelled through the alias because it follows only a symlink in
// the final component — and a deactivation that treated that as a different file would refuse to
// restore an install it owns. Anything that cannot be shown to be the same file (a path that does
// not exist, or one that fails to stat) is not the same file: this comparison only ever widens
// acceptance to files the kernel proves identical, never to an unreadable or absent path.
func configLockPathsSameFile(a, b string) bool {
	if a == b {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
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

// configLockWritersDeactivateWrites reports whether this deactivation will write config.toml: it
// restores owned table keys, or it asks the injected CLI to disable a flag CRW enabled. With
// neither, uninstall is not gated on the lock, because there is no config.toml write to serialize.
func configLockWritersDeactivateWrites(m *InstallManifest) bool {
	if len(m.TableKeys) > 0 {
		return true
	}
	for _, key := range manifestOrder(m.flagOrder, m.Flags) {
		if f := m.Flags[key]; !f.PriorEnabled && f.EnabledByCodexclaw {
			return true
		}
	}
	return false
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
	markOptedOut := func() { _ = MarkSelfHealOptedOut(deps.CodexHome, now()) }
	r := &DeactivateResult{Disabled: []string{}, SkippedPreExisting: []string{}, NoManifest: true, RestoredKeys: []string{}, SkippedExternal: []SkippedExternal{}}
	noManifest := func() (*DeactivateResult, error) {
		markOptedOut()
		r.NoManifest = true
		return r, nil
	}
	// The first reading decides only whether and where to lock; the reading every decision below uses
	// is taken after the lock is held (CRW-877).
	raw, _ := readTextOrNull(manifestPath(deps.CodexHome))
	if raw == nil {
		return noManifest()
	}
	m := parseInstallManifest(*raw)
	if m == nil {
		return noManifest()
	}
	r.NoManifest = false
	path := deps.ConfigPath
	if path == "" {
		path = m.ConfigPath
	}
	// lockedPath is the spelling the first reading chose; the re-read below must still name the same
	// file for the held lock to be the lock over the manifest's own config file.
	lockedPath := path
	// One critical section for the whole command's writes to config.toml under the sidecar lock
	// every CRW writer of config.toml takes (CRW-866), the shape of activate.go's
	// activationSetKeyLocked: the drift check, the read, the restore, and the injected CLI calls
	// that disable flags and rewrite the same file. A CRW writer that published between the read
	// and the restore, or between the restore and the CLI's own read-modify-write, would otherwise
	// have its change discarded with neither command reporting it. The lock is taken only when this
	// deactivation writes config.toml, so an uninstall with nothing to restore and no flag CRW
	// enabled is never gated on it, and an empty config path names no file to guard.
	if path != "" && configLockWritersDeactivateWrites(m) {
		lock, err := crwdir.LockConfig(path, activationLockWait)
		if err != nil {
			return nil, err
		}
		defer lock.Release()
		path = lock.Target
		// The manifest read before the lock answered only whether and where to lock. An activation
		// that published while this command waited would otherwise be ignored, and the restore would
		// be computed from a manifest that no longer describes the install: the drift hash, the table
		// keys and the flags below all decide from this second reading. A manifest missing or
		// unreadable here answers as the no-manifest branch above does.
		fresh, _ := readTextOrNull(manifestPath(deps.CodexHome))
		if fresh == nil {
			return noManifest()
		}
		if m = parseInstallManifest(*fresh); m == nil {
			return noManifest()
		}
		// The lock is held on the file the first reading named. A manifest that now names a different
		// config file would have this deactivation apply one file's ownership records to another, so
		// it refuses rather than acting under the wrong lock (fail closed). An explicit ConfigPath
		// overrides the manifest in both readings, so only the derived path can disagree.
		if deps.ConfigPath == "" && !configLockPathsSameFile(m.ConfigPath, lock.Target) {
			return nil, fmt.Errorf("the install manifest now names a different config file (%s, was %s); run the deactivation again", m.ConfigPath, lockedPath)
		}
	}
	// The opt-out is recorded only once this deactivation is going to do its work: a busy lock
	// refuses the command before this line, and a refusal must not leave self-healing off for an
	// uninstall that never ran (the commit-order rule: never record an effect that did not happen).
	markOptedOut()
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
