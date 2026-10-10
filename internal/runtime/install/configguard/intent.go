package configguard

import (
	"crypto/sha256"
	"encoding/hex"
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
// it under the config lock before it does anything else: it records the ownership of every attempted effect that is PROVEN
// to be crw's, and of nothing else. An attempted marker proves only that the effect was about to run: a command stopped
// before the effect, and a user who then made the same change, look alike. The proof is therefore the file itself (below), and
// an effect that is in place without it is left unrecorded and reported. Recovery reverts nothing, so a user's edit made since
// is never undone or adopted; the self-heal hook never recovers, it only reads.
//
// The proof is a done record. An effect is marked done, and the intent published again, after the effect ran: a flag when its
// runner returned, a key when its config.toml publication is in place. A stop before that record leaves the effect attempted
// but not done, and a recovery records nothing for it: it cannot tell crw's change from the same change made later by the user,
// and a record of a change crw did not make would have the deactivation undo the user's setting. The cost is the other way
// round and small: a kill in the instant between an effect and its done record leaves that effect unrecorded and reported, and
// the user's own copy of it stays as it is. A flag is also required to have changed config.toml (PreHash, the fingerprint of the
// file when it was attempted, differs from PostHash, the fingerprint when its runner returned): a runner that exited 0 and
// changed nothing proves nothing about a flag the user enables later.

// InstallIntentName is the intent file beside the install manifest.
const InstallIntentName = ".crw-install.intent.json"

func intentPath(home string) string { return filepath.Join(home, InstallIntentName) }

// configAbsent is the fingerprint of a config.toml that does not exist.
const configAbsent = "absent"

func fingerprintBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// configFingerprint answers the fingerprint of the file path names.
func configFingerprint(path string) (string, error) {
	b, exists, err := activationReadFile(path)
	if err != nil {
		return "", err
	}
	if !exists {
		return configAbsent, nil
	}
	return fingerprintBytes(b), nil
}

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
	// Done is set once the effect ran (see above). PreHash and PostHash are the fingerprints of config.toml when the effect was
	// attempted and when it ended (sha256 of the bytes, "absent" for no file).
	Done     bool   `json:"done,omitempty"`
	PreHash  string `json:"preHash,omitempty"`
	PostHash string `json:"postHash,omitempty"`
}

type installIntent struct {
	Version    int    `json:"version"`
	Op         string `json:"op"`
	ConfigPath string `json:"configPath"`
	// Target is the file ConfigPath resolved to when the intent was made; a key effect is recovered only on that file.
	Target string `json:"target,omitempty"`
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

// txWithDurability is the error of a command that stops on cause after it already published a record whose directory sync
// failed (CRW-1153): the stop is reported together with that uncertainty. The uncertainty is joined as text, not as a
// *crwdir.PublishedError, so the stop is never mistaken for a change that is in place and only not known to be durable.
func txWithDurability(cause, unsynced error) error {
	if cause == nil || unsynced == nil || crwdir.Published(cause) {
		return cause
	}
	return errors.Join(cause, fmt.Errorf("a record this command already published is in place, but a directory could not be synced, so it may not survive a power failure: %v", unsynced))
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
	if target, ok := configLockPathsRealPath(path); ok {
		in.Target = target
	}
	if base != nil {
		b, err := manifestBytes(base)
		if err != nil {
			return nil, err
		}
		in.Base = b
	}
	return in, nil
}

// publish writes the intent. Unlike txPublish it counts a publication whose directory sync failed as a failure: the intent is
// what makes the effects after it recoverable, so no effect may run on an intent that may not survive a power failure
// (CRW-1153). The caller stops, and an intent no effect depends on is removed with abandon.
func (in *installIntent) publish(step string) error {
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	if err := txStep(step); err != nil {
		return err
	}
	if _, _, err := activationReadFile(intentPath(in.home)); err != nil {
		return err
	}
	if err := activationCrwdirPublish(intentPath(in.home), append(b, '\n')); err != nil {
		if crwdir.Published(err) {
			// A plain error: the intent is in place but not known to be durable, which is not a published change.
			return fmt.Errorf("the change intent %s may not survive a power failure (%v), so nothing was changed", intentPath(in.home), err)
		}
		return err
	}
	return nil
}

// abandon removes an intent no effect depends on. It is best effort: a leftover intent without an attempted effect records
// nothing.
func (in *installIntent) abandon() { _ = os.Remove(intentPath(in.home)) }

// attempt marks effect i attempted and publishes the intent before the effect runs. An intent that could not be made durable
// leaves the effect unattempted: it does not run. A publication that failed only its directory sync is in place, so the file
// on disk names the effect attempted although it never runs; the recovery would then record as crw's whatever the user does
// to that flag or key later. The marker is therefore taken back on disk too (CRW-1153): the intent is published again
// without it, and either version a power failure may leave holds the effect unattempted. When that publication fails as well
// and no other effect depends on the intent, the intent is removed; otherwise the error says which effect it names wrongly.
func (in *installIntent) attempt(i int) error {
	in.Effects[i].Attempted = true
	err := in.publish("intent")
	if err == nil {
		return nil
	}
	in.Effects[i].Attempted = false
	if rollback := in.rewrite(); rollback != nil {
		for _, e := range in.Effects {
			if e.Attempted {
				return fmt.Errorf("%w; the intent could not be rewritten either (%v), so it may name %s as attempted although it did not run: check that change before the next 'crw install features enable' or 'disable', which records it", err, rollback, in.Effects[i].Name)
			}
		}
		in.abandon()
	}
	return err
}

// rewrite publishes the intent as it is in memory, counting a publication whose file is in place as done: it only takes
// back a marker, which is safe in either version a power failure may leave.
func (in *installIntent) rewrite() error {
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	if err := txStep("intent-rollback"); err != nil {
		return err
	}
	if err := activationCrwdirPublish(intentPath(in.home), append(b, '\n')); err != nil && !crwdir.Published(err) {
		return err
	}
	return nil
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
// intent that cannot be read, a manifest that cannot be read, a config.toml that does not decode, or flags whose state cannot
// be read are refused, and the intent is kept. An effect that is in place but not proven to be crw's (see the top of this
// file) is not recorded; it is reported.
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
	// The manifest the recovery commits over must be one crw can read: a malformed one is refused, as every writer refuses it,
	// and the intent stays (CRW-1153).
	if _, err := readOwnedManifest(home); err != nil {
		return nil, fmt.Errorf("an interrupted crw change is pending in %s: %w", intentPath(home), err)
	}
	m := &InstallManifest{Version: 2, ConfigPath: in.ConfigPath, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{}}
	if len(in.Base) > 0 {
		if m = parseInstallManifest(string(in.Base)); m == nil {
			return nil, fmt.Errorf("an interrupted crw change in %s holds records crw cannot read; nothing was changed", intentPath(home))
		}
	}
	content, fileExists, err := activationReadFile(path)
	if err != nil {
		return nil, err
	}
	attempted := slices.ContainsFunc(in.Effects, func(e intentEffect) bool { return e.Attempted })
	// What an attempted effect did is read from config.toml: a file that does not decode cannot be read, and recording over it
	// would drop the evidence before the user has fixed the file (CRW-1141). The intent is kept.
	if attempted {
		if err := validateConfig(path, string(content)); err != nil {
			return nil, fmt.Errorf("an interrupted crw change is pending in %s and is kept: %w", intentPath(home), err)
		}
	}
	current := configAbsent
	if fileExists {
		current = fingerprintBytes(content)
	}
	sameFile := true
	if target, ok := configLockPathsRealPath(path); ok && in.Target != "" {
		sameFile = target == in.Target
	}
	proof := intentProof(in.Effects, current)
	var flags map[string]FeatureState
	var recorded []string
	for i, e := range in.Effects {
		if !e.Attempted {
			continue
		}
		switch e.Kind {
		case intentFlag:
			f := m.Flags[e.Name]
			if f.PriorEnabled || f.EnabledByCodexclaw {
				continue
			}
			if run == nil {
				return nil, fmt.Errorf("an interrupted 'crw install features enable' is pending in %s; run 'crw install features enable' or 'disable' first, which can read the flags it changed. Nothing was changed", intentPath(home))
			}
			if flags == nil {
				if flags, err = ReadFeatureStates(run); err != nil {
					return nil, fmt.Errorf("an interrupted crw change is pending in %s, and the flags it may have changed cannot be read (%w); nothing was changed", intentPath(home), err)
				}
			}
			if flags[e.Name] != FeatureEnabled {
				continue
			}
			if !proof.flag[i] {
				recorded = append(recorded, e.Name+" (on, but not shown to be crw's, so left unrecorded)")
				continue
			}
			f.EnabledByCodexclaw, f.EnableFailed, f.Failure = true, false, nil
			m.Flags[e.Name] = f
			if !slices.Contains(m.flagOrder, e.Name) {
				m.flagOrder = append(m.flagOrder, e.Name)
			}
			recorded = append(recorded, e.Name)
		case intentKey:
			live, editable := semanticRaw(string(content), e.Table, e.Key)
			if !editable || live == nil || !tomledit.SameValue(*live, e.Applied) && *live != e.Applied {
				continue
			}
			if !proof.key[i] || !sameFile {
				if _, ok := m.TableKeys[e.Name]; !ok {
					recorded = append(recorded, e.Name+" (set, but not shown to be crw's, so left unrecorded)")
				}
				continue
			}
			owned := e.Owned || e.Prior == nil || !tomledit.SameValue(*e.Prior, e.Applied) && *e.Prior != e.Applied
			if _, ok := m.TableKeys[e.Name]; !ok {
				m.tableOrder = append(m.tableOrder, e.Name)
			}
			m.TableKeys[e.Name] = TableKeyRecord{e.Table, e.Key, e.Prior, e.Applied, owned}
			recorded = append(recorded, e.Name)
		case intentRestore:
			if !sameFile {
				continue
			}
			live, editable := semanticRaw(string(content), e.Table, e.Key)
			restored := editable && (e.Prior == nil && live == nil || e.Prior != nil && live != nil && (*live == *e.Prior || tomledit.SameValue(*live, *e.Prior)))
			if restored {
				delete(m.TableKeys, e.Name)
				recorded = append(recorded, e.Name+" (restored)")
			}
		}
	}
	// The drift baseline moves to the file as it is only when the file is what the chain of crw's own effects ended in; a
	// file that changed since keeps the baseline of the records before (CRW-1141).
	if proof.end && fileExists {
		m.PostActivateHash = &current
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

// intentProofs is what an intent proves about its effects, for the file as it is now.
type intentProofs struct {
	flag, key map[int]bool
	// end is true when config.toml is exactly what the last attempted effect left, which is the baseline a later drift check
	// compares against.
	end bool
}

func intentProof(effects []intentEffect, current string) intentProofs {
	p := intentProofs{flag: map[int]bool{}, key: map[int]bool{}}
	last := -1
	for i, e := range effects {
		if !e.Attempted {
			continue
		}
		last = i
		if !e.Done {
			continue
		}
		switch e.Kind {
		case intentFlag:
			p.flag[i] = e.PostHash != "" && e.PostHash != e.PreHash
		case intentKey:
			p.key[i] = true
		}
	}
	if last >= 0 {
		p.end = effects[last].Done && effects[last].PostHash == current
	}
	return p
}
