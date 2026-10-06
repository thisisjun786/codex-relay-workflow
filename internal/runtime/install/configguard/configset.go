package configguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// ConfigSetDeps injects every IO path and the timestamp (CXC config-set.ts:19-23).
type ConfigSetDeps struct {
	CodexHome, ConfigPath string
	Now                   func() string
}

type ConfigSetOutcome struct {
	OK, Changed            bool
	Reason                 string
	Entry                  ManagedKey
	PriorValue, BackupPath *string
	AppliedValue           string
}

type ManagedState struct {
	Entry ManagedKey
	Value *string
}

// ResolveManagedKey is the closed whitelist boundary, including the oracle's trim.
func ResolveManagedKey(id string) (*ManagedKey, string) {
	if entry := FindManagedKey(id); entry != nil {
		return entry, ""
	}
	return nil, fmt.Sprintf("'%s' is not a crw-managed key. Run 'crw install config list' to see what can be set. ", id) +
		"crw edits only whitelisted keys, so it never becomes a general TOML editor."
}

// ApplyManagedKey ports config-set.ts:64-159. nil restores the recorded prior value.
// Atomic owners replace the oracle's truncating writes; other ordering is retained.
func ApplyManagedKey(deps ConfigSetDeps, id string, value *bool) (ConfigSetOutcome, error) {
	entry, reason := ResolveManagedKey(id)
	if entry == nil {
		return ConfigSetOutcome{Reason: reason}, nil
	}
	m, err := readPriorManifest(deps.CodexHome)
	if err != nil || m == nil {
		return ConfigSetOutcome{Reason: "no readable install manifest under this codex home; run 'crw install features enable' first. " +
			"Without it there is nowhere to record the previous value, and 'crw install features disable' could not revert this key."}, nil
	}
	path := deps.ConfigPath
	if path == "" {
		path = filepath.Join(deps.CodexHome, "config.toml")
	}
	// The read, the decision, the backup and the publish of config.toml are one critical section
	// under the sidecar lock every CRW writer of config.toml takes (CRW-866), the shape of
	// activate.go's activationSetKeyLocked: a retrust or an activation that published between this
	// read and this write would otherwise be overwritten with content built from the pre-change
	// bytes, and neither command would report it. The lock covers config.toml only; the install
	// manifest keeps the unlocked publish.
	lock, err := crwdir.LockConfig(path, activationLockWait)
	if err != nil {
		return ConfigSetOutcome{}, err
	}
	released := false
	release := func() {
		if !released {
			released = true
			lock.Release()
		}
	}
	defer release()
	// target is the file the lock guards: the caller's path with a symlink followed, so two
	// writers reaching one file through different spellings share one lock.
	target := lock.Target
	pre, exists, err := activationReadFile(target)
	if err != nil {
		return ConfigSetOutcome{}, err
	}
	content := source.DecodeUTF8(pre)
	keyID := ManagedKeyID(*entry)
	recorded, hadRecord := m.TableKeys[keyID]
	var prior *string
	var res TomlEditResult
	applied := "(absent)"
	if value == nil {
		if !hadRecord {
			return ConfigSetOutcome{Reason: keyID + " is not recorded as set by crw; nothing to unset."}, nil
		}
		prior = recorded.PriorValue
		res = RestoreTableKey(content, entry.Table, entry.Key, prior)
		if prior != nil {
			applied = *prior
		}
	} else {
		if live, found := ReadTableKey(content, entry.Table, entry.Key); found {
			prior = &live
		}
		res = SetTableKey(content, entry.Table, entry.Key, *value)
		applied = strconvBool(*value)
	}
	if res.Action == TomlUnsupportedValue {
		return ConfigSetOutcome{Reason: keyID + " currently holds a value crw will not rewrite; edit config.toml by hand."}, nil
	}
	var backup *string
	if res.Changed {
		if exists || value == nil {
			info, err := os.Stat(path)
			if err != nil {
				return ConfigSetOutcome{}, err
			}
			now := deps.Now
			if now == nil {
				now = func() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
			}
			name := path + ".crw-" + strings.NewReplacer(":", "-", ".", "-").Replace(now()) + ".bak"
			if err := activationBackup(name, pre, info.Mode()); err != nil {
				return ConfigSetOutcome{}, err
			}
			backup = &name
		}
		if err := activationPublish(target, []byte(res.Content)); err != nil {
			return ConfigSetOutcome{}, err
		}
	}
	// The config.toml critical section ends here: the manifest below is not shared with another
	// CRW writer and keeps the unlocked publish.
	release()
	if value == nil {
		delete(m.TableKeys, keyID)
	} else {
		original, owned := prior, res.Changed
		if hadRecord {
			original, owned = recorded.PriorValue, recorded.SetByCodexclaw || res.Changed
		} else {
			m.tableOrder = append(m.tableOrder, keyID)
		}
		m.TableKeys[keyID] = TableKeyRecord{entry.Table, entry.Key, original, applied, owned}
	}
	m.Version = 2
	m.PostActivateHash, err = hashOrNull(path)
	if err != nil {
		return ConfigSetOutcome{}, err
	}
	bytes, err := manifestBytes(m)
	if err != nil {
		return ConfigSetOutcome{}, err
	}
	if err := activationPublish(manifestPath(deps.CodexHome), bytes); err != nil {
		return ConfigSetOutcome{}, err
	}
	return ConfigSetOutcome{OK: true, Changed: res.Changed, Entry: *entry, PriorValue: prior, AppliedValue: applied, BackupPath: backup}, nil
}

// ReadManagedState ports config-set.ts:162-168 without resolving a real home.
func ReadManagedState(configPath string) ([]ManagedState, error) {
	b, _, err := activationReadFile(configPath)
	if err != nil {
		return nil, err
	}
	states := make([]ManagedState, 0)
	for _, entry := range ConfigManagedKeys() {
		var value *string
		if live, found := ReadTableKey(source.DecodeUTF8(b), entry.Table, entry.Key); found {
			value = &live
		}
		states = append(states, ManagedState{Entry: entry, Value: value})
	}
	return states, nil
}
