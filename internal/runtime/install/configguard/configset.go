package configguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/tomledit"
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
	// Unsupported marks a key config.toml defines in a form crw does not edit (CRW-1141); it is not absent, and Reason says
	// what the form is.
	Unsupported bool
	Reason      string
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
	path := deps.ConfigPath
	if path == "" {
		path = filepath.Join(deps.CodexHome, "config.toml")
	}
	// A refusal writes nothing, so it takes no lock and leaves no sidecar: this first read only
	// answers whether there is a manifest at all. The manifest that decides the edit and records
	// the ownership is read again under the lock below.
	if m, err := readPriorManifest(deps.CodexHome); err != nil || m == nil {
		return ConfigSetOutcome{Reason: "no readable install manifest under this codex home; run 'crw install features enable' first. " +
			"Without it there is nowhere to record the previous value, and 'crw install features disable' could not revert this key."}, nil
	}
	// The whole command is one critical section under the sidecar lock every CRW writer of
	// config.toml takes (CRW-866), the shape of activate.go's activationSetKeyLocked: the read, the
	// decision, the backup and the activationPublish of config.toml, and the install manifest that
	// records the key's ownership, are serialized against retrust, the activation and every other
	// CRW writer. Reading the manifest before the lock, or publishing it after the release, lets an
	// overlapping writer publish in that window: its change is overwritten with content built from
	// the pre-change bytes, and the manifest this command writes describes a value that is no
	// longer live, so a later unset restores the wrong prior value (failure class 3, the
	// check-then-act race). The manifest keeps activationPublish; only the boundary moved.
	lock, err := crwdir.LockConfig(path, activationLockWait)
	if err != nil {
		return ConfigSetOutcome{}, err
	}
	defer lock.Release()
	// target is the file the lock guards: the caller's path with a symlink followed, so two
	// writers reaching one file through different spellings share one lock.
	target := lock.Target
	// The authoritative manifest, read inside the lock so a concurrent writer's manifest is seen
	// rather than a copy that is already stale.
	m, err := readPriorManifest(deps.CodexHome)
	if err != nil || m == nil {
		return ConfigSetOutcome{Reason: "no readable install manifest under this codex home; run 'crw install features enable' first. " +
			"Without it there is nowhere to record the previous value, and 'crw install features disable' could not revert this key."}, nil
	}
	pre, exists, err := activationReadFile(target)
	if err != nil {
		return ConfigSetOutcome{}, err
	}
	content := source.DecodeUTF8(pre)
	keyID := ManagedKeyID(*entry)
	recorded, hadRecord := m.TableKeys[keyID]
	// The key is read and edited through the semantic editor (CRW-1141): a config.toml that does not decode is refused, and
	// so is a key written in a form the editor cannot rewrite safely, with config.toml, the manifest and the backups as they
	// were.
	if err := validateConfig(path, content); err != nil {
		return ConfigSetOutcome{Reason: err.Error()}, nil
	}
	var prior *string
	var res TomlEditResult
	var refused string
	applied := "(absent)"
	if value == nil {
		if !hadRecord {
			return ConfigSetOutcome{Reason: keyID + " is not recorded as set by crw; nothing to unset."}, nil
		}
		prior = recorded.PriorValue
		res, refused, err = semanticRestore(content, entry.Table, entry.Key, prior)
		if prior != nil {
			applied = *prior
		}
	} else {
		live, editable := semanticRaw(content, entry.Table, entry.Key)
		if editable {
			prior = live
		}
		res, refused, err = semanticSet(content, entry.Table, entry.Key, *value)
		applied = strconvBool(*value)
	}
	if err != nil {
		return ConfigSetOutcome{Reason: err.Error()}, nil
	}
	if res.Action == TomlUnsupportedValue {
		return ConfigSetOutcome{Reason: keyID + " currently holds a value crw will not rewrite (" + refused + "); edit config.toml by hand."}, nil
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
	content := source.DecodeUTF8(b)
	if err := validateConfig(configPath, content); err != nil {
		return nil, err
	}
	states := make([]ManagedState, 0)
	for _, entry := range ConfigManagedKeys() {
		look := semanticRead(content, entry.Table, entry.Key)
		state := ManagedState{Entry: entry, Unsupported: look.State == tomledit.Unsupported, Reason: look.Reason}
		if look.State == tomledit.Found {
			raw := look.Raw
			state.Value = &raw
		}
		states = append(states, state)
	}
	return states, nil
}
