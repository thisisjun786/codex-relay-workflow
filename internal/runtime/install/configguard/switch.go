package configguard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/switchstate"
)

// SwitchCXCPlugin is the install key of the CXC plugin, the one Codex plugin whose activation the
// switch changes. crw@crw is never touched: the relay, the MCP bridge and the completion hook stay on
// in both positions (CRW-201 decision of 10-10).
const SwitchCXCPlugin = "codexclaw@codexclaw"

// SwitchBy is the writer the switch records in switch.json and the manifest.
const SwitchBy = "crw install switch"

// switchSpecs is the closed list of Codex config keys the switch manages. It is not a ManagedKey:
// `crw install config set` never offers it.
func switchSpecs() []struct{ Table, Key string } {
	return []struct{ Table, Key string }{{Table: `plugins."` + SwitchCXCPlugin + `"`, Key: "enabled"}}
}

// SwitchDeps injects every path and the clock. Fail, when set, is asked before each step and a
// non-nil answer fails that step, so a test can prove each rollback.
type SwitchDeps struct {
	CodexHome, ConfigPath string
	Now                   func() string
	Fail                  func(step string) error
}

// SwitchKeyChange is one config key as the switch found it and left it.
type SwitchKeyChange struct {
	Table, Key, Before, After string
}

// SwitchRoleChange is one role file and what the switch did with it.
type SwitchRoleChange struct {
	Role, Path, Owner, Action string
	BackupPath                *string
}

// SwitchReport is what a switch did.
type SwitchReport struct {
	Active        string
	ChangedAt     string
	ConfigChanged bool
	ConfigBackup  *string
	Keys          []SwitchKeyChange
	Roles         []SwitchRoleChange
	Notes         []string
	ManifestPath  string
}

// ReadInstallManifest is the install manifest under a Codex home, nil when there is none. A file
// that exists but is not a manifest is an error: nothing may overwrite it.
func ReadInstallManifest(home string) (*InstallManifest, error) {
	b, exists, err := activationReadFile(manifestPath(home))
	if err != nil || !exists {
		return nil, err
	}
	m := parseInstallManifest(string(b))
	if m == nil {
		return nil, fmt.Errorf("%s is not a readable install manifest (left unchanged)", manifestPath(home))
	}
	return m, nil
}

type switchTx struct {
	undo []func() error
	fail func(string) error
}

// run performs one step. do returns the undo of what it changed even when it fails half way.
func (t *switchTx) run(step string, do func() (func() error, error)) error {
	if t.fail != nil {
		if err := t.fail(step); err != nil {
			return fmt.Errorf("step %s: %w", step, err)
		}
	}
	undo, err := do()
	if undo != nil {
		t.undo = append(t.undo, undo)
	}
	if err != nil {
		return fmt.Errorf("step %s: %w", step, err)
	}
	return nil
}

// check asks the injected failure for a boundary inside a step.
func (t *switchTx) check(step string) error {
	if t.fail == nil {
		return nil
	}
	if err := t.fail(step); err != nil {
		return fmt.Errorf("step %s: %w", step, err)
	}
	return nil
}

func (t *switchTx) rollback() error {
	var errs []error
	for i := len(t.undo) - 1; i >= 0; i-- {
		if err := t.undo[i](); err != nil {
			errs = append(errs, err)
		}
	}
	t.undo = nil
	return errors.Join(errs...)
}

func switchRoleNames() []role.NativeRoleName { return role.NativeRoles() }

func switchDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// switchAgentsDir refuses a role file whose agents directory is not a real directory (CRW-201 round
// 2): it is asked before every read, backup, write and removal of a role file.
func switchAgentsDir(path string) error { return role.CheckAgentsDirectory(filepath.Dir(path)) }

func switchPublishRole(path string, b []byte) error {
	if err := switchAgentsDir(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	if err := switchAgentsDir(path); err != nil {
		return err
	}
	return activationPublished(crwdir.Publish(path, b))
}

func switchRestoreBytes(path string, prior []byte) func() error {
	return func() error {
		if prior == nil {
			if err := switchAgentsDir(path); err != nil {
				return err
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		}
		return switchPublishRole(path, prior)
	}
}

func switchStamp(now string) string { return strings.NewReplacer(":", "-", ".", "-").Replace(now) }

// RunSwitch makes target ("crw" or "cxc") the plugin whose hooks act, in one critical section under
// the sidecar lock every CRW writer of config.toml takes. The steps, in order: the manifest's switch
// section (with the values from before), <CODEX_HOME>/crw/switch.json, the CXC plugin's enabled key,
// the role files. A step that fails undoes the steps before it, each to the byte it read; an undo that
// fails is reported with the cause. A switch that died without undoing leaves its pending section,
// and running the command again finishes it from the recorded values.
func RunSwitch(deps SwitchDeps, target string) (*SwitchReport, error) {
	if target != string(switchstate.CRW) && target != string(switchstate.CXC) {
		return nil, fmt.Errorf("switch target %q is neither crw nor cxc", target)
	}
	home := deps.CodexHome
	path := deps.ConfigPath
	if path == "" {
		path = filepath.Join(home, "config.toml")
	}
	now := deps.Now
	if now == nil {
		now = func() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
	}
	if err := os.MkdirAll(home, 0o777); err != nil {
		return nil, err
	}
	if err := role.CheckAgentsDirectory(filepath.Join(home, "agents")); err != nil {
		return nil, err
	}
	lock, err := crwdir.LockConfig(path, activationLockWait)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	cfg := lock.Target

	manifestFile := manifestPath(home)
	origManifest, manifestExists, err := activationReadFile(manifestFile)
	if err != nil {
		return nil, err
	}
	var m *InstallManifest
	if manifestExists {
		if m = parseInstallManifest(string(origManifest)); m == nil {
			return nil, fmt.Errorf("%s is not a readable install manifest (left unchanged)", manifestFile)
		}
	}
	prevState, err := switchstate.ReadRaw(home)
	if err != nil {
		return nil, err
	}
	pre, preExists, err := activationReadFile(cfg)
	if err != nil {
		return nil, err
	}
	stamp := now()
	report := &SwitchReport{Active: target, ChangedAt: stamp, ManifestPath: manifestFile, Keys: []SwitchKeyChange{}, Roles: []SwitchRoleChange{}, Notes: []string{}}
	tx := &switchTx{fail: deps.Fail}

	var prior *SwitchRecord
	if m != nil {
		prior = m.Switch
	}
	rec := &SwitchRecord{Active: target, ChangedAt: stamp, By: SwitchBy}
	if prior.captured() {
		*rec = *prior
		rec.Keys = append([]SwitchKeyRecord(nil), prior.Keys...)
		rec.Roles = append([]SwitchRoleRecord(nil), prior.Roles...)
		rec.Active, rec.By = target, SwitchBy
		if prior.Active != target || prior.Pending {
			rec.ChangedAt = stamp
		} else {
			rec.ChangedAt = prior.ChangedAt
		}
	} else if target == string(switchstate.CRW) {
		// The values from before: read now, before anything is written.
		for _, spec := range switchSpecs() {
			rec.Keys = append(rec.Keys, SwitchKeyRecord{Table: spec.Table, Key: spec.Key, AppliedValue: "false"})
		}
		for _, name := range switchRoleNames() {
			rolePath := role.RoleFilePath(home, name)
			raw, err := role.ReadRoleFile(home, name)
			if err != nil {
				return nil, err
			}
			rec.Roles = append(rec.Roles, SwitchRoleRecord{Role: string(name), Path: rolePath, PriorOwner: string(role.OwnerOf(raw))})
		}
	}
	for _, rr := range rec.Roles {
		if err := switchAgentsDir(rr.Path); err != nil {
			return nil, err
		}
	}
	// A key the switch is about to edit that holds a value form it will not rewrite is refused before
	// anything is written, whichever the direction and whether or not it was recorded already.
	for _, k := range rec.Keys {
		if !k.TablePresent && target != string(switchstate.CRW) {
			continue
		}
		if st := ReadTableKeyLine(string(pre), k.Table, k.Key); st.Unsupported {
			return nil, fmt.Errorf("%s.%s currently holds a value crw will not rewrite (or names the key twice); edit config.toml by hand, then run the command again", k.Table, k.Key)
		}
	}
	if target == string(switchstate.CRW) {
		// A CXC table that was not there at the first switch (the plugin was added since) is read
		// now, before the key is changed, so the way back has its line too.
		for i := range rec.Keys {
			k := &rec.Keys[i]
			if k.TablePresent {
				continue
			}
			st := ReadTableKeyLine(string(pre), k.Table, k.Key)
			if st.TablePresent {
				k.TablePresent, k.PriorLine = true, nil
				if st.Found {
					line := st.Line
					k.PriorLine = &line
				}
			}
		}
	}

	// the CXC plugin's enabled key, computed now and written below
	content := string(pre)
	changed := false
	for _, k := range rec.Keys {
		before := "(no table)"
		var after string
		if k.TablePresent {
			st := ReadTableKeyLine(content, k.Table, k.Key)
			before = "(absent)"
			if st.Found {
				before = st.Value
			}
			var edited string
			var did bool
			if target == string(switchstate.CRW) {
				var st TomlKeyLineState
				if edited, st, did = SetTableKeyExact(content, k.Table, k.Key, false); st.Unsupported {
					return nil, fmt.Errorf("%s.%s currently holds a value crw will not rewrite; edit config.toml by hand", k.Table, k.Key)
				}
			} else {
				var err error
				if edited, did, err = RestoreTableKeyExact(content, k.Table, k.Key, k.PriorLine); err != nil {
					// Nothing is written yet, and the record that holds the line stays.
					return nil, fmt.Errorf("the way back to cxc cannot restore %s.%s: %w; the recorded values stay in %s: fix config.toml by hand, then run the command again", k.Table, k.Key, err, manifestFile)
				}
			}
			content, changed = edited, changed || did
			after = "(absent)"
			if live := ReadTableKeyLine(content, k.Table, k.Key); live.Found {
				after = live.Value
			}
		} else {
			after = before
			report.Notes = append(report.Notes, "config.toml has no ["+k.Table+"] table: the CXC plugin is not configured here, so there is no key to change")
		}
		report.Keys = append(report.Keys, SwitchKeyChange{Table: k.Table, Key: k.Key, Before: before, After: after})
	}

	// The activation's drift hash follows the switch only while config.toml is still the file the
	// activation (or an earlier, unfinished switch) left; a file edited since stays drifted.
	base := m
	if base == nil {
		base = &InstallManifest{Version: 2, ConfigPath: path, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{}}
	}
	followHash := false
	if base.PostActivateHash == nil {
		followHash = len(base.TableKeys) == 0 && len(base.Flags) == 0
	} else if preExists {
		have := switchDigest(pre)
		followHash = have == *base.PostActivateHash || (prior != nil && prior.ConfigHash != nil && have == *prior.ConfigHash)
	}
	hashPending := prior != nil && prior.ConfigHash != nil
	if followHash && changed {
		h := switchDigest([]byte(content))
		rec.ConfigHash = &h
	}

	finish := func(err error) (*SwitchReport, error) {
		if rbErr := tx.rollback(); rbErr != nil {
			return nil, fmt.Errorf("%w; the rollback also failed and left the host part way: %v", err, rbErr)
		}
		return nil, err
	}
	writeManifest := func(next *InstallManifest) (func() error, error) {
		b, err := manifestBytes(next)
		if err != nil {
			return nil, err
		}
		undo := func() error {
			if manifestExists {
				return activationPublish(manifestFile, origManifest)
			}
			if err := os.Remove(manifestFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		}
		return undo, activationPublish(manifestFile, b)
	}
	// The pending section, with everything a restart needs, is on disk before the first change.
	if target == string(switchstate.CRW) || prior.captured() {
		rec.Pending = true
		base.Switch = rec
		if err := tx.run("manifest", func() (func() error, error) { return writeManifest(base) }); err != nil {
			return finish(err)
		}
	}

	// switch.json
	current, curErr := switchstate.Parse(prevState)
	if prevState == nil || curErr != nil || string(current.Active) != target {
		err = tx.run("state", func() (func() error, error) {
			undo := func() error {
				if prevState == nil {
					return switchstate.Remove(home)
				}
				return switchstate.WriteRaw(home, prevState)
			}
			return undo, switchstate.Write(home, switchstate.State{Active: switchstate.Active(target), ChangedAt: stamp, By: SwitchBy})
		})
		if err != nil {
			return finish(err)
		}
	}

	if changed {
		err = tx.run("config", func() (func() error, error) {
			name := cfg + ".crw-" + switchStamp(stamp) + ".bak"
			info, err := os.Stat(cfg)
			if err != nil {
				return nil, err
			}
			if err := activationBackup(name, pre, info.Mode()); err != nil {
				return nil, err
			}
			rec.ConfigBackup = &name
			undo := func() error { return activationPublish(cfg, pre) }
			return undo, activationPublish(cfg, []byte(content))
		})
		if err != nil {
			return finish(err)
		}
		report.ConfigChanged = true
	}
	report.ConfigBackup = rec.ConfigBackup
	if !changed && !preExists {
		report.ConfigBackup = nil
	}

	// the role files
	for i := range rec.Roles {
		rr := &rec.Roles[i]
		name := role.NativeRoleName(rr.Role)
		var change SwitchRoleChange
		err = tx.run("role:"+rr.Role, func() (func() error, error) {
			var undo func() error
			var err error
			if target == string(switchstate.CRW) {
				persist := func() error {
					// The backup is recorded before the role file is replaced.
					undo, err := writeManifest(base)
					if undo != nil {
						tx.undo = append(tx.undo, undo)
					}
					if err != nil {
						return err
					}
					return tx.check("role-replace:" + rr.Role)
				}
				change, undo, err = switchRoleToCRW(home, name, rr, stamp, persist)
			} else {
				change, undo, err = switchRoleToCXC(home, name, rr)
			}
			return undo, err
		})
		if err != nil {
			return finish(err)
		}
		if change.Action == "left-modified" && change.BackupPath != nil {
			report.Notes = append(report.Notes, change.Path+" changed since the switch installed it and is kept; the CXC role file it replaced is at "+*change.BackupPath)
		}
		report.Roles = append(report.Roles, change)
	}

	// the manifest's final section
	err = tx.run("manifest-final", func() (func() error, error) {
		final := base
		if target == string(switchstate.CRW) {
			rec.Pending, rec.ConfigHash = false, nil
			final.Switch = rec
		} else {
			if m == nil && !prior.captured() {
				return nil, nil // nothing was ever recorded, so there is nothing to write
			}
			final.Switch = &SwitchRecord{Active: target, ChangedAt: rec.ChangedAt, By: SwitchBy, ConfigBackup: rec.ConfigBackup}
		}
		if followHash && (changed || hashPending) {
			var err error
			if final.PostActivateHash, err = hashOrNull(cfg); err != nil {
				return nil, err
			}
		}
		return writeManifest(final)
	})
	if err != nil {
		return finish(err)
	}
	return report, nil
}

func switchRoleToCRW(home string, name role.NativeRoleName, rr *SwitchRoleRecord, stamp string, persist func() error) (SwitchRoleChange, func() error, error) {
	change := SwitchRoleChange{Role: string(name), Path: rr.Path, BackupPath: rr.BackupPath}
	raw, err := role.ReadRoleFile(home, name)
	if err != nil {
		return change, nil, err
	}
	owner := role.OwnerOf(raw)
	change.Owner = string(owner)
	want, err := role.NativeRoleContent(name)
	if err != nil {
		return change, nil, err
	}
	undo := switchRestoreBytes(rr.Path, raw)
	switch owner {
	case role.OwnerCRW:
		// An existing CRW role file is the user's or an older build's: it is not rewritten, because
		// the way back has nothing to put in its place.
		if bytes.Equal(raw, want) {
			rr.AppliedDigest = switchDigest(want)
			change.Action = "already-crw"
		} else {
			change.Action = "kept-crw"
		}
		return change, nil, nil
	case role.OwnerNone:
		change.Action = "installed"
		// The way back removes the file again unless a CXC backup is to come back in its place.
		if rr.PriorOwner != string(role.OwnerCXC) || rr.BackupPath == nil {
			rr.PriorOwner = string(role.OwnerNone)
		}
		rr.AppliedDigest = switchDigest(want)
		// The digest the way back compares with is recorded before the file is written.
		if err := persist(); err != nil {
			return change, nil, err
		}
		return change, undo, switchPublishRole(rr.Path, want)
	case role.OwnerCXC:
		backup := rr.Path + ".crw-" + switchStamp(stamp) + ".bak"
		if rr.BackupPath != nil {
			// An unfinished run already took this copy: it is kept when it is the file as it is now.
			if saved, err := os.ReadFile(*rr.BackupPath); err == nil && bytes.Equal(saved, raw) {
				backup = *rr.BackupPath
			}
		}
		if backup != derefString(rr.BackupPath) {
			info, err := os.Stat(rr.Path)
			if err != nil {
				return change, nil, err
			}
			if err := switchAgentsDir(backup); err != nil {
				return change, nil, err
			}
			if err := activationBackup(backup, raw, info.Mode()); err != nil {
				return change, nil, err
			}
		}
		// A CXC file that appeared after the first switch is now what the way back restores.
		rr.PriorOwner = string(role.OwnerCXC)
		rr.BackupPath = &backup
		rr.AppliedDigest = switchDigest(want)
		change.BackupPath = &backup
		change.Action = "replaced-cxc"
		if err := persist(); err != nil {
			return change, nil, err
		}
		return change, undo, switchPublishRole(rr.Path, want)
	}
	change.Action = "kept-other"
	return change, nil, nil
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func switchRoleToCXC(home string, name role.NativeRoleName, rr *SwitchRoleRecord) (SwitchRoleChange, func() error, error) {
	change := SwitchRoleChange{Role: string(name), Path: rr.Path, BackupPath: rr.BackupPath}
	raw, err := role.ReadRoleFile(home, name)
	if err != nil {
		return change, nil, err
	}
	owner := role.OwnerOf(raw)
	change.Owner = string(owner)
	undo := switchRestoreBytes(rr.Path, raw)
	// A CRW role file that is not the one the switch installed (crw register of another build, say)
	// is the user's now: it is neither removed nor replaced.
	installedHere := owner == role.OwnerCRW && rr.AppliedDigest != "" && switchDigest(raw) == rr.AppliedDigest
	switch rr.PriorOwner {
	case string(role.OwnerCXC):
		switch {
		case installedHere:
			if rr.BackupPath == nil {
				return change, nil, fmt.Errorf("%s is a CRW role file that replaced a CXC one, but no backup of the CXC file is recorded; restore it by hand or remove the file, then run the command again", rr.Path)
			}
			saved, err := os.ReadFile(*rr.BackupPath)
			if err != nil {
				return change, nil, fmt.Errorf("the backup of the CXC role file is unreadable: %w", err)
			}
			if role.OwnerOf(saved) != role.OwnerCXC {
				return change, nil, fmt.Errorf("the backup %s is no longer a CXC role file; left unchanged", *rr.BackupPath)
			}
			change.Action = "restored-cxc"
			return change, undo, switchPublishRole(rr.Path, saved)
		case owner == role.OwnerCXC:
			change.Action = "already-cxc"
		default:
			change.Action = "left-modified"
		}
	case string(role.OwnerNone):
		switch {
		case installedHere:
			change.Action = "removed"
			return change, undo, switchRestoreBytes(rr.Path, nil)()
		case owner == role.OwnerNone:
			change.Action = "already-absent"
		default:
			change.Action = "left-modified"
		}
	default:
		change.Action = "unchanged"
	}
	return change, nil, nil
}
