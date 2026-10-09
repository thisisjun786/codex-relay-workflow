package configguard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/tomledit"
)

// A change crw makes to config.toml and the feature flags, and the install manifest that records crw's ownership of it, are
// one transaction (CRW-1153). Before the first effect an intent is published beside the manifest: the records the change
// starts from, the config file it is about, and each planned effect, marked attempted before it runs. The manifest is
// committed from what was verified and the intent removed. A command that stops between those steps (a failed publication,
// a hard flag failure, a kill) leaves the intent, and the next explicit command (enable, disable, config set, unset) recovers
// it under the config lock before it does anything else: it records the ownership of every attempted effect that is in
// place now, and of nothing else. Recovery reverts nothing, so a user's edit made since is never undone; the self-heal hook
// never recovers, it only reads.

// InstallIntentName is the intent file beside the install manifest.
const InstallIntentName = ".crw-install.intent.json"

func intentPath(home string) string { return filepath.Join(home, InstallIntentName) }

// The kinds of effect an intent records.
const (
	intentFlag    = "flag"    // a declared feature flag crw enables
	intentKey     = "key"     // a managed key crw sets
	intentRestore = "restore" // a managed key crw restores to its prior value (unset)
)

type intentEffect struct {
	Kind string `json:"kind"`
	// Name is the flag, or the key's id (table.key).
	Name  string `json:"name"`
	Table string `json:"table,omitempty"`
	Key   string `json:"key,omitempty"`
	// Prior is the raw value the key had before crw (nil: absent); Applied the raw value crw writes.
	Prior   *string `json:"prior,omitempty"`
	Applied string  `json:"applied,omitempty"`
	// Owned is crw's ownership of the key before this change.
	Owned     bool `json:"owned,omitempty"`
	Attempted bool `json:"attempted"`
}

type installIntent struct {
	Version    int    `json:"version"`
	Op         string `json:"op"`
	ConfigPath string `json:"configPath"`
	// Base is the install manifest the change starts from, as bytes; empty when there was none.
	Base    []byte         `json:"base"`
	Effects []intentEffect `json:"effects"`
	home    string
}

// txHook, when set by a test, runs before each step of a transaction and may fail it (or stop the command) there.
var txHook func(step string) error

func txStep(step string) error {
	if txHook == nil {
		return nil
	}
	return txHook(step)
}

// txPublish publishes like activationPublish but does not count a publication whose directory sync failed as done: it is
// in place, so the transaction continues and records it, and the sync failure is kept in unsynced for the command to report
// (CRW-1153).
func txPublish(step, path string, b []byte, unsynced *error) error {
	if err := txStep(step); err != nil {
		return err
	}
	if _, _, err := activationReadFile(path); err != nil {
		return err
	}
	err := activationCrwdirPublish(path, b)
	if crwdir.Published(err) {
		*unsynced = errors.Join(*unsynced, fmt.Errorf("%s: %w", path, err))
		return nil
	}
	return err
}

// txDurability is the error of a command whose changes are in place and recorded but not known to be durable.
func txDurability(unsynced error) error {
	if unsynced == nil {
		return nil
	}
	return &crwdir.PublishedError{Err: fmt.Errorf("the changes are in place and recorded, but a directory could not be synced, so they may not survive a power failure: %w", unsynced)}
}

// txPrecheck refuses before any change when a file the transaction will publish could not be written: an existing file must
// open for writing and its directory must accept a new entry (CRW-1153). A readable manifest the process cannot write used to
// fail only after config.toml had changed.
func txPrecheck(paths ...string) error {
	for _, p := range paths {
		target := p
		if info, err := os.Lstat(p); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(p)
			if err != nil {
				return fmt.Errorf("%s cannot be written (left unchanged): %w", p, err)
			}
			target = resolved
		}
		if _, err := os.Stat(target); err == nil {
			f, err := os.OpenFile(target, os.O_WRONLY, 0)
			if err != nil {
				return fmt.Errorf("%s cannot be written, so nothing was changed: %w", p, err)
			}
			_ = f.Close()
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s cannot be inspected, so nothing was changed: %w", p, err)
		}
		if err := unix.Access(filepath.Dir(target), unix.W_OK|unix.X_OK); err != nil {
			return fmt.Errorf("the directory of %s cannot be written, so nothing was changed: %w", p, err)
		}
	}
	return nil
}

// readOwnedManifest reads the install manifest a writer is about to replace. Unlike readPriorManifest, a manifest that exists
// but is not one crw can read is refused rather than read as absent, so it is never overwritten (CRW-1153).
func readOwnedManifest(home string) (*InstallManifest, error) {
	b, exists, err := activationReadFile(manifestPath(home))
	if err != nil || !exists {
		return nil, err
	}
	m := parseInstallManifest(string(b))
	if m == nil {
		return nil, fmt.Errorf("the install manifest %s is not one crw can read, so it was left as it is and nothing was changed; move it aside to start a new install record", manifestPath(home))
	}
	return m, nil
}

func newIntent(home, op, path string, base *InstallManifest) (*installIntent, error) {
	in := &installIntent{Version: 1, Op: op, ConfigPath: path, home: home}
	if base != nil {
		b, err := manifestBytes(base)
		if err != nil {
			return nil, err
		}
		in.Base = b
	}
	return in, nil
}

func (in *installIntent) publish(step string, unsynced *error) error {
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	return txPublish(step, intentPath(in.home), append(b, '\n'), unsynced)
}

// attempt marks effect i attempted and publishes the intent before the effect runs.
func (in *installIntent) attempt(i int, unsynced *error) error {
	in.Effects[i].Attempted = true
	return in.publish("intent", unsynced)
}

// closeIntent removes the intent once the manifest that records its effects is committed.
func closeIntent(home string, unsynced *error) error {
	if err := txStep("intent-close"); err != nil {
		return err
	}
	if err := os.Remove(intentPath(home)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := crwdir.SyncDir(home); err != nil {
		*unsynced = errors.Join(*unsynced, err)
	}
	return nil
}

// commitManifest publishes m as the install manifest and closes the intent. A manifest that cannot be published leaves the
// intent for the next explicit command to record.
func commitManifest(home string, m *InstallManifest, unsynced *error) error {
	b, err := manifestBytes(m)
	if err != nil {
		return err
	}
	if err := txPublish("manifest", manifestPath(home), b, unsynced); err != nil {
		return fmt.Errorf("the change is in place but its ownership record could not be written (%w); the interrupted change is kept in %s, and the next 'crw install features enable' or 'disable' or 'crw install config' change records it", err, intentPath(home))
	}
	return closeIntent(home, unsynced)
}

// pendingIntentConfig answers the config file a pending intent is about, "" when there is none.
func pendingIntentConfig(home string) (string, error) {
	raw, exists, err := activationReadFile(intentPath(home))
	if err != nil || !exists {
		return "", err
	}
	var in installIntent
	if err := json.Unmarshal(raw, &in); err != nil || in.Version != 1 || in.ConfigPath == "" {
		return "", fmt.Errorf("an interrupted crw change left %s, which crw cannot read; nothing was changed. Inspect it, then remove it to continue", intentPath(home))
	}
	return in.ConfigPath, nil
}

// recoverIntent records an interrupted change before an explicit command does anything else (CRW-1153). The caller holds the
// config lock for path. It answers what it recorded, for the command to report. An intent about another config file, an
// intent that cannot be read, or flags whose state cannot be read are refused, and the intent is kept.
func recoverIntent(home, path string, run CodexRunner) ([]string, error) {
	raw, exists, err := activationReadFile(intentPath(home))
	if err != nil || !exists {
		return nil, err
	}
	var in installIntent
	if err := json.Unmarshal(raw, &in); err != nil || in.Version != 1 {
		return nil, fmt.Errorf("an interrupted crw change left %s, which crw cannot read; nothing was changed. Inspect it, then remove it to continue", intentPath(home))
	}
	if !activationCarries(&InstallManifest{ConfigPath: in.ConfigPath}, path) {
		return nil, fmt.Errorf("an interrupted crw change in %s is about %s, not %s; nothing was changed", intentPath(home), in.ConfigPath, path)
	}
	m := &InstallManifest{Version: 2, ConfigPath: in.ConfigPath, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{}}
	if len(in.Base) > 0 {
		if m = parseInstallManifest(string(in.Base)); m == nil {
			return nil, fmt.Errorf("an interrupted crw change in %s holds records crw cannot read; nothing was changed", intentPath(home))
		}
	}
	content, _, err := activationReadFile(path)
	if err != nil {
		return nil, err
	}
	var flags map[string]bool
	var recorded []string
	for _, e := range in.Effects {
		if !e.Attempted {
			continue
		}
		switch e.Kind {
		case intentFlag:
			if run == nil {
				return nil, fmt.Errorf("an interrupted 'crw install features enable' is pending in %s; run 'crw install features enable' or 'disable' first, which can read the flags it changed. Nothing was changed", intentPath(home))
			}
			if flags == nil {
				if flags, err = ReadDeclaredState(run); err != nil {
					return nil, fmt.Errorf("an interrupted crw change is pending in %s, and the flags it may have changed cannot be read (%w); nothing was changed", intentPath(home), err)
				}
			}
			f := m.Flags[e.Name]
			if flags[e.Name] && !f.PriorEnabled && !f.EnabledByCodexclaw {
				f.EnabledByCodexclaw, f.EnableFailed, f.Failure = true, false, nil
				m.Flags[e.Name] = f
				if !slices.Contains(m.flagOrder, e.Name) {
					m.flagOrder = append(m.flagOrder, e.Name)
				}
				recorded = append(recorded, e.Name)
			}
		case intentKey:
			live, editable := semanticRaw(string(content), e.Table, e.Key)
			if !editable || live == nil || !tomledit.SameValue(*live, e.Applied) && *live != e.Applied {
				continue
			}
			owned := e.Owned || e.Prior == nil || !tomledit.SameValue(*e.Prior, e.Applied) && *e.Prior != e.Applied
			if _, ok := m.TableKeys[e.Name]; !ok {
				m.tableOrder = append(m.tableOrder, e.Name)
			}
			m.TableKeys[e.Name] = TableKeyRecord{e.Table, e.Key, e.Prior, e.Applied, owned}
			recorded = append(recorded, e.Name)
		case intentRestore:
			live, editable := semanticRaw(string(content), e.Table, e.Key)
			restored := editable && (e.Prior == nil && live == nil || e.Prior != nil && live != nil && (*live == *e.Prior || tomledit.SameValue(*live, *e.Prior)))
			if restored {
				delete(m.TableKeys, e.Name)
				recorded = append(recorded, e.Name+" (restored)")
			}
		}
	}
	m.PostActivateHash, err = hashOrNull(path)
	if err != nil {
		return nil, err
	}
	var unsynced error
	if err := commitManifest(home, m, &unsynced); err != nil {
		return nil, err
	}
	if unsynced != nil {
		return recorded, txDurability(unsynced)
	}
	return recorded, nil
}
