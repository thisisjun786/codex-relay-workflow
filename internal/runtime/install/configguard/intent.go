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
// a hard flag failure, a kill) leaves the intent, and the next explicit command (enable, disable, config set, unset, release)
// recovers it under the config lock before it does anything else: it records the ownership of every attempted effect that is
// PROVEN to be crw's, and of nothing else. An attempted marker proves only that the effect was about to run: a command stopped
// before the effect, and a user who then made the same change, look alike. Recovery reverts nothing, so a user's edit made
// since is never undone or adopted; the self-heal hook never recovers, it only reads.
//
// The proof is a done record. An effect is marked done, and the intent published again, after the effect ran: a flag when its
// runner returned, a key when its config.toml publication is in place. The done record of a flag carries the flag's own state
// in config.toml before and after crw's run (PreOn, PostOn): the flag is crw's only when its run turned it on there, and a run
// that left it off (whatever else it rewrote) proves that a flag on later is the user's. A changed fingerprint of the whole file
// is no proof of a flag.
//
// An effect that is in place but neither proven crw's nor proven the user's (attempted, but the stop came before its done
// record; or about a file config.toml no longer resolves to) is never recorded as crw's, because a record of a change crw did
// not make would have the deactivation or unset undo the user's setting, and it is never forgotten either: it is kept as a
// pending entry of the intent, carried from intent to intent and reported by every command, until it is resolved. It is
// resolved when it is no longer in place (the flag is off, the key no longer holds the value crw was writing), by 'crw install
// config unset <key> --release' for a key, or by removing the intent file, which every report names.

// InstallIntentName is the intent file beside the install manifest.
const InstallIntentName = ".crw-install.intent.json"

func intentPath(home string) string { return filepath.Join(home, InstallIntentName) }

// configAbsent is the fingerprint of a config.toml that does not exist.
const configAbsent = "absent"

func fingerprintBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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
	// PreOn and PostOn are a flag's own state in config.toml when it was attempted and when its runner returned; nil when the
	// file holds it in a form crw does not read.
	PreOn  *bool `json:"preOn,omitempty"`
	PostOn *bool `json:"postOn,omitempty"`
	// Target is the file the effect was about: the file config.toml resolved to when the effect ended (the CLI may replace
	// config.toml itself, CRW-1144), the intent's Target when it did not end, and for a pending entry the file it is kept for.
	Target string `json:"target,omitempty"`
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
	// Pending are effects an earlier recovery found in place but could not attribute to crw or to the user: they are kept,
	// carried from intent to intent, until they are resolved (see the top of this file).
	Pending []intentEffect `json:"pending,omitempty"`
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
	// The pending entries an earlier recovery kept are carried (the recovery ran under the same lock, so the intent on disk
	// holds nothing else).
	raw, exists, err := activationReadFile(intentPath(home))
	if err != nil {
		return nil, err
	}
	if exists {
		var old installIntent
		if err := json.Unmarshal(raw, &old); err != nil {
			return nil, fmt.Errorf("%s cannot be read, so nothing was changed: %w", intentPath(home), err)
		}
		in.Pending = old.Pending
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

// abandon removes an intent no effect depends on, keeping the pending entries it carries. It is best effort: a leftover
// intent without an attempted effect records nothing.
func (in *installIntent) abandon() {
	var ignored error
	_ = in.close(&ignored)
}

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

// close ends the intent once the manifest that records its effects is committed: it is removed, or, when it carries pending
// entries, replaced by an intent that holds only those.
func (in *installIntent) close(unsynced *error) error {
	if err := txStep("intent-close"); err != nil {
		return err
	}
	if len(in.Pending) > 0 {
		rest := installIntent{Version: 1, Op: "pending", ConfigPath: in.ConfigPath, Target: in.Target, Effects: []intentEffect{}, Pending: in.Pending}
		b, err := json.MarshalIndent(rest, "", "  ")
		if err != nil {
			return err
		}
		if err := activationCrwdirPublish(intentPath(in.home), append(b, '\n')); err != nil {
			if !crwdir.Published(err) {
				return err
			}
			*unsynced = errors.Join(*unsynced, fmt.Errorf("%s: %w", intentPath(in.home), err))
		}
		return nil
	}
	if err := os.Remove(intentPath(in.home)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := crwdir.SyncDir(in.home); err != nil {
		*unsynced = errors.Join(*unsynced, err)
	}
	return nil
}

// commitManifest publishes m as the install manifest and closes the intent. A manifest that cannot be published leaves the
// intent for the next explicit command to record.
func commitManifest(in *installIntent, m *InstallManifest, unsynced *error) error {
	b, err := manifestBytes(m)
	if err != nil {
		return err
	}
	if err := txPublish("manifest", manifestPath(in.home), b, unsynced); err != nil {
		return fmt.Errorf("the change is in place but its ownership record could not be written (%w); the interrupted change is kept in %s, and the next 'crw install features enable' or 'disable' or 'crw install config' change records it", err, intentPath(in.home))
	}
	return in.close(unsynced)
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
// config lock for path. It answers what it recorded and what it keeps pending, for the command to report. An intent about
// another config file, an intent that cannot be read, a manifest that cannot be read, a config.toml that does not decode, or
// flags whose state cannot be read are refused, and the intent is kept. An effect that is in place but not proven crw's or the
// user's (see the top of this file) is kept pending.
func recoverIntent(home, path string, run CodexRunner) ([]string, error) {
	in, err := readPending(home)
	if err != nil || in == nil {
		return nil, err
	}
	if !activationCarries(&InstallManifest{ConfigPath: in.ConfigPath}, path) {
		return nil, fmt.Errorf("an interrupted crw change in %s is about %s, not %s; nothing was changed", intentPath(home), in.ConfigPath, path)
	}
	// The manifest the recovery commits over must be one crw can read: a malformed one is refused, as every writer refuses it,
	// and the intent stays (CRW-1153).
	if _, err := readOwnedManifest(home); err != nil {
		return nil, fmt.Errorf("an interrupted crw change is pending in %s: %w", intentPath(home), err)
	}
	attempted := slices.ContainsFunc(in.Effects, func(e intentEffect) bool { return e.Attempted })
	m := &InstallManifest{Version: 2, ConfigPath: in.ConfigPath, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{}}
	if attempted && len(in.Base) > 0 {
		if m = parseInstallManifest(string(in.Base)); m == nil {
			return nil, fmt.Errorf("an interrupted crw change in %s holds records crw cannot read; nothing was changed", intentPath(home))
		}
	}
	content, fileExists, err := activationReadFile(path)
	if err != nil {
		return nil, err
	}
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
	target, _ := configLockPathsRealPath(path)
	// onFile reports whether config.toml is still the file an effect was about.
	onFile := func(about string) bool { return about == "" || target == "" || target == about }
	sameFile := onFile(in.Target)
	// effectFile reports whether config.toml is still the file effect e ended on.
	effectFile := func(e intentEffect) bool {
		if e.Target != "" {
			return onFile(e.Target)
		}
		return sameFile
	}
	var flags map[string]FeatureState
	flagOn := func(name string) (bool, error) {
		if flags == nil {
			if flags, err = ReadFeatureStates(run); err != nil {
				return false, fmt.Errorf("an interrupted crw change is pending in %s, and the flags it may have changed cannot be read (%w); nothing was changed", intentPath(home), err)
			}
		}
		return flags[name] == FeatureEnabled, nil
	}
	keyInPlace := func(e intentEffect) bool {
		live, editable := semanticRaw(string(content), e.Table, e.Key)
		return editable && live != nil && (tomledit.SameValue(*live, e.Applied) || *live == e.Applied)
	}
	var recorded []string
	var pending []intentEffect
	keep := func(e intentEffect, why string) {
		if e.Target == "" {
			e.Target = in.Target
		}
		if slices.ContainsFunc(pending, func(p intentEffect) bool { return p.Kind == e.Kind && p.Name == e.Name && p.Target == e.Target }) {
			return
		}
		e.Attempted, e.Done = false, false
		pending = append(pending, e)
		recorded = append(recorded, fmt.Sprintf("%s (%s; kept pending in %s until it is resolved: crw does not treat it as its own)", e.Name, why, intentPath(home)))
	}
	// The pending entries an earlier recovery kept stay until they are no longer in place. One about another file, and a flag
	// a command that cannot read the flags meets, cannot be checked and stays.
	for _, e := range in.Pending {
		switch {
		case !onFile(e.Target):
			keep(e, "it was about "+e.Target+", which config.toml no longer names")
		case e.Kind == intentKey:
			if keyInPlace(e) {
				keep(e, "set, but not shown to be crw's")
			}
		case e.Kind == intentFlag && run == nil:
			keep(e, "it may be on, but not shown to be crw's")
		case e.Kind == intentFlag:
			on, err := flagOn(e.Name)
			if err != nil {
				return nil, err
			}
			if on {
				keep(e, "on, but not shown to be crw's")
			}
		}
	}
	proof := intentProof(in.Effects, current)
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
			on, err := flagOn(e.Name)
			if err != nil {
				return nil, err
			}
			switch {
			case !on:
				continue
			case !effectFile(e):
				keep(e, "on in the file config.toml names now, but the interrupted change was about another file")
			case proof.flag[i]:
				f.EnabledByCodexclaw, f.EnableFailed, f.Failure = true, false, nil
				m.Flags[e.Name] = f
				if !slices.Contains(m.flagOrder, e.Name) {
					m.flagOrder = append(m.flagOrder, e.Name)
				}
				recorded = append(recorded, e.Name)
			case proof.notFlag[i]:
				recorded = append(recorded, e.Name+" (on, but crw's run left it off, so it is the user's and left unrecorded)")
			default:
				keep(e, "on, but not shown to be crw's")
			}
		case intentKey:
			if !keyInPlace(e) {
				continue
			}
			switch {
			case !effectFile(e):
				keep(e, "set in the file config.toml names now, but the interrupted change was about another file")
			case !proof.key[i]:
				keep(e, "set, but not shown to be crw's")
			default:
				owned := e.Owned || e.Prior == nil || !tomledit.SameValue(*e.Prior, e.Applied) && *e.Prior != e.Applied
				if _, ok := m.TableKeys[e.Name]; !ok {
					m.tableOrder = append(m.tableOrder, e.Name)
				}
				m.TableKeys[e.Name] = TableKeyRecord{e.Table, e.Key, e.Prior, e.Applied, owned}
				recorded = append(recorded, e.Name)
			}
		case intentRestore:
			if !effectFile(e) {
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
	in.Pending = pending
	var unsynced error
	if attempted {
		// The drift baseline moves to the file as it is only when the file is what the chain of crw's own effects ended in; a
		// file that changed since, or another file, keeps the baseline of the records before (CRW-1141).
		if proof.end && fileExists && effectFile(in.Effects[proof.last]) {
			m.PostActivateHash = &current
		}
		if err := commitManifest(in, m, &unsynced); err != nil {
			return nil, err
		}
	} else if err := in.close(&unsynced); err != nil {
		// An intent without an attempted effect changed nothing: it is closed, keeping what is still pending.
		return nil, err
	}
	if unsynced != nil {
		return recorded, txDurability(unsynced)
	}
	return recorded, nil
}

// intentProofs is what an intent proves about its effects, for the file as it is now.
type intentProofs struct {
	// flag holds the flags crw's run turned on in config.toml; notFlag the flags its run left off there, which a flag on now
	// therefore owes to someone else. key holds the keys whose publication is in place.
	flag, notFlag, key map[int]bool
	// last is the index of the last attempted effect.
	last int
	// end is true when config.toml is exactly what the last attempted effect left, which is the baseline a later drift check
	// compares against.
	end bool
}

func intentProof(effects []intentEffect, current string) intentProofs {
	p := intentProofs{flag: map[int]bool{}, notFlag: map[int]bool{}, key: map[int]bool{}}
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
			if e.PreOn != nil && e.PostOn != nil {
				p.flag[i] = !*e.PreOn && *e.PostOn
				p.notFlag[i] = !*e.PostOn
			}
		case intentKey:
			p.key[i] = true
		}
	}
	if last >= 0 {
		p.last, p.end = last, effects[last].Done && effects[last].PostHash == current
	}
	return p
}
