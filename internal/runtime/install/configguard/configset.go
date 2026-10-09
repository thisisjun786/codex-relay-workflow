package configguard

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	// Recovered names what this command recorded of an interrupted earlier change (CRW-1153).
	Recovered []string
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
	// An interrupted change is recorded first (CRW-1153); a pending activation whose flags must be read is left to the
	// feature commands, which can run Codex.
	recovered, recErr := recoverIntent(deps.CodexHome, path, nil)
	if recErr != nil && !crwdir.Published(recErr) {
		return ConfigSetOutcome{}, recErr
	}
	// The authoritative manifest, read inside the lock so a concurrent writer's manifest is seen
	// rather than a copy that is already stale. One that exists but cannot be read is refused, never replaced (CRW-1153).
	m, err := readOwnedManifest(deps.CodexHome)
	if err != nil {
		return ConfigSetOutcome{}, err
	}
	if m == nil {
		return ConfigSetOutcome{Reason: "no readable install manifest under this codex home; run 'crw install features enable' first. " +
			"Without it there is nowhere to record the previous value, and 'crw install features disable' could not revert this key."}, nil
	}
	// A manifest a completed deactivation released owns nothing any more (CRW-1145): a key set into it would be recorded
	// beside records that no longer describe ownership, and the next deactivation would take the release marker for the
	// answer and leave the new key. A new install record starts with 'crw install features enable'.
	if m.ReleasedAt != nil {
		return ConfigSetOutcome{Reason: "the install was released by 'crw install features disable', so there is no live record to keep this key in; run 'crw install features enable' to start a new one."}, nil
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
		// Unset decides ownership as the deactivation does (CRW-1149): only a key crw set, that still holds the value crw
		// applied, and whose original absence is proven when it was absent, is restored. Anything else writes nothing and
		// keeps the record; the explicit release drops it.
		if why := configUnsetRefusal(m, recorded, pre, content); why != "" {
			return ConfigSetOutcome{Reason: keyID + " " + why + "; config.toml was not changed and crw's record was kept. " +
				"Set the value by hand, or run 'crw install config unset " + keyID + " --release' to drop crw's record and leave config.toml as it is."}, nil
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
	// config.toml, the backup and the manifest that records the key are one transaction (CRW-1153): every destination is
	// checked first, an intent naming the effect is published before config.toml changes, and the manifest is committed from
	// it; a stop in between leaves the intent for the next explicit command to record.
	if err := txPrecheck(target, manifestPath(deps.CodexHome), intentPath(deps.CodexHome)); err != nil {
		return ConfigSetOutcome{}, err
	}
	var unsynced error
	if recErr != nil {
		unsynced = recErr
	}
	original, owned := prior, res.Changed
	if hadRecord {
		original, owned = recorded.PriorValue, recorded.SetByCodexclaw || res.Changed
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
			if err := txStep("backup"); err != nil {
				return ConfigSetOutcome{}, err
			}
			if err := activationBackup(name, pre, info.Mode()); err != nil {
				return ConfigSetOutcome{}, err
			}
			backup = &name
		}
		op, effect := "config-set", intentEffect{Kind: intentKey, Name: keyID, Table: entry.Table, Key: entry.Key, Prior: original, Applied: applied, Owned: hadRecord && recorded.SetByCodexclaw, Attempted: true}
		if value == nil {
			op, effect = "config-unset", intentEffect{Kind: intentRestore, Name: keyID, Table: entry.Table, Key: entry.Key, Prior: recorded.PriorValue, Applied: recorded.AppliedValue, Attempted: true}
		}
		in, err := newIntent(deps.CodexHome, op, path, m)
		if err != nil {
			return ConfigSetOutcome{}, err
		}
		in.Effects = []intentEffect{effect}
		// config.toml changes only on an intent known to be durable (CRW-1153); until then nothing depends on it.
		if err := in.publish("intent"); err != nil {
			in.abandon()
			return ConfigSetOutcome{}, err
		}
		if err := txPublish("config", target, []byte(res.Content), &unsynced); err != nil {
			// Nothing was published over config.toml, so the intent describes nothing in place; it is closed when it can be.
			return ConfigSetOutcome{}, errors.Join(err, closeIntent(deps.CodexHome, &unsynced))
		}
	}
	if value == nil {
		delete(m.TableKeys, keyID)
	} else {
		if !hadRecord {
			m.tableOrder = append(m.tableOrder, keyID)
		}
		m.TableKeys[keyID] = TableKeyRecord{entry.Table, entry.Key, original, applied, owned}
	}
	m.Version = 2
	m.PostActivateHash, err = hashOrNull(path)
	if err != nil {
		return ConfigSetOutcome{}, err
	}
	if res.Changed {
		err = commitManifest(deps.CodexHome, m, &unsynced)
	} else {
		var b []byte
		if b, err = manifestBytes(m); err == nil {
			err = txPublish("manifest", manifestPath(deps.CodexHome), b, &unsynced)
		}
	}
	if err != nil {
		return ConfigSetOutcome{}, err
	}
	return ConfigSetOutcome{OK: true, Changed: res.Changed, Entry: *entry, PriorValue: prior, AppliedValue: applied, BackupPath: backup, Recovered: recovered}, txDurability(unsynced)
}

// configUnsetRefusal answers why unset must not restore rec, or "" when crw still owns the key. The live value and the
// backup's are read semantically (CRW-1141), and the decision is DecideKeyRestore, the deactivation's own table: drift is
// a config.toml that changed since crw last recorded its hash, and the backup is the activation's.
func configUnsetRefusal(m *InstallManifest, rec TableKeyRecord, pre []byte, content string) string {
	live, editable := semanticRaw(content, rec.Table, rec.Key)
	if !editable {
		return "is now written in a form crw does not edit (changed)"
	}
	drift := false
	if m.PostActivateHash != nil {
		sum := sha256.Sum256(pre)
		drift = hex.EncodeToString(sum[:]) != *m.PostActivateHash
	}
	var backup *string
	backupKnown := false
	if m.BackupPath != nil && *m.BackupPath != "" {
		if text, err := readTextOrNull(*m.BackupPath); err == nil && text != nil {
			backup, backupKnown = semanticRaw(*text, rec.Table, rec.Key)
		}
	}
	ok, reason := DecideKeyRestore(rec, live, drift, backupKnown, backup)
	switch {
	case ok:
		return ""
	case !rec.SetByCodexclaw:
		return "was not set by crw (it already held " + configShown(rec.PriorValue) + " when crw recorded it)"
	case reason == SkipMissing:
		return "was removed after crw set it (missing)"
	case reason == SkipChanged:
		return "now holds " + configShown(live) + ", not the value crw set (" + rec.AppliedValue + ") (changed)"
	}
	return "cannot be shown to have been absent before crw set it: config.toml changed since and no backup proves it (unverifiable)"
}

func configShown(value *string) string {
	if value == nil {
		return "(unset)"
	}
	return *value
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

// ReleaseManagedKey drops crw's record of a managed key and leaves config.toml as it is (CRW-1149). It is the explicit
// answer to an unset that refused because crw no longer owns the key: the record is the evidence of what crw changed, so it is
// never dropped silently, only when the user asks.
func ReleaseManagedKey(deps ConfigSetDeps, id string) (ConfigSetOutcome, error) {
	entry, reason := ResolveManagedKey(id)
	if entry == nil {
		return ConfigSetOutcome{Reason: reason}, nil
	}
	path := deps.ConfigPath
	if path == "" {
		path = filepath.Join(deps.CodexHome, "config.toml")
	}
	lock, err := crwdir.LockConfig(path, activationLockWait)
	if err != nil {
		return ConfigSetOutcome{}, err
	}
	defer lock.Release()
	recovered, recErr := recoverIntent(deps.CodexHome, path, nil)
	if recErr != nil && !crwdir.Published(recErr) {
		return ConfigSetOutcome{}, recErr
	}
	m, err := readOwnedManifest(deps.CodexHome)
	if err != nil {
		return ConfigSetOutcome{}, err
	}
	if m == nil {
		return ConfigSetOutcome{Reason: "no readable install manifest under this codex home; there is no record to release."}, nil
	}
	keyID := ManagedKeyID(*entry)
	rec, ok := m.TableKeys[keyID]
	if !ok {
		return ConfigSetOutcome{Reason: keyID + " is not recorded as set by crw; nothing to release."}, nil
	}
	delete(m.TableKeys, keyID)
	b, err := manifestBytes(m)
	if err != nil {
		return ConfigSetOutcome{}, err
	}
	unsynced := recErr
	if err := txPublish("manifest", manifestPath(deps.CodexHome), b, &unsynced); err != nil {
		return ConfigSetOutcome{}, err
	}
	return ConfigSetOutcome{OK: true, Entry: *entry, PriorValue: rec.PriorValue, AppliedValue: rec.AppliedValue, Recovered: recovered}, txDurability(unsynced)
}
