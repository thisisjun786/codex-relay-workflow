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

func switchPublishRole(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	return activationPublished(crwdir.Publish(path, b))
}

func switchRestoreBytes(path string, prior []byte) func() error {
	return func() error {
		if prior == nil {
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
		rec.Active, rec.By = target, SwitchBy
		if prior.Active != target || prior.Pending {
			rec.ChangedAt = stamp
		} else {
			rec.ChangedAt = prior.ChangedAt
		}
	} else if target == string(switchstate.CRW) {
		// The values from before: read now, before anything is written.
		for _, spec := range switchSpecs() {
			st := ReadTableKeyLine(string(pre), spec.Table, spec.Key)
			if st.Unsupported {
				return nil, fmt.Errorf("%s.%s currently holds a value crw will not rewrite; edit config.toml by hand", spec.Table, spec.Key)
			}
			k := SwitchKeyRecord{Table: spec.Table, Key: spec.Key, TablePresent: st.TablePresent, AppliedValue: "false"}
			if st.Found {
				line := st.Line
				k.PriorLine = &line
			}
			rec.Keys = append(rec.Keys, k)
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
	base := m
	if base == nil {
		base = &InstallManifest{Version: 2, ConfigPath: path, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{}}
	}

	if target == string(switchstate.CRW) {
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

	// the CXC plugin's enabled key
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
				edited, _, did = SetTableKeyExact(content, k.Table, k.Key, false)
			} else {
				edited, did = RestoreTableKeyExact(content, k.Table, k.Key, k.PriorLine)
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
				change, undo, err = switchRoleToCRW(home, name, rr, stamp)
			} else {
				change, undo, err = switchRoleToCXC(home, name, rr)
			}
			return undo, err
		})
		if err != nil {
			return finish(err)
		}
		report.Roles = append(report.Roles, change)
	}

	// the manifest's final section
	err = tx.run("manifest-final", func() (func() error, error) {
		final := base
		if target == string(switchstate.CRW) {
			rec.Pending = false
			final.Switch = rec
		} else {
			if m == nil && !prior.captured() {
				return nil, nil // nothing was ever recorded, so there is nothing to write
			}
			final.Switch = &SwitchRecord{Active: target, ChangedAt: rec.ChangedAt, By: SwitchBy, ConfigBackup: rec.ConfigBackup}
		}
		if changed {
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

func switchRoleToCRW(home string, name role.NativeRoleName, rr *SwitchRoleRecord, stamp string) (SwitchRoleChange, func() error, error) {
	change := SwitchRoleChange{Role: string(name), Path: rr.Path}
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
		rr.AppliedDigest = switchDigest(want)
		if bytes.Equal(raw, want) {
			change.Action = "already-crw"
			return change, nil, nil
		}
		change.Action = "updated"
		return change, undo, switchPublishRole(rr.Path, want)
	case role.OwnerNone:
		change.Action = "installed"
		rr.AppliedDigest = switchDigest(want)
		return change, undo, switchPublishRole(rr.Path, want)
	case role.OwnerCXC:
		info, err := os.Stat(rr.Path)
		if err != nil {
			return change, nil, err
		}
		backup := rr.Path + ".crw-" + switchStamp(stamp) + ".bak"
		if err := activationBackup(backup, raw, info.Mode()); err != nil {
			return change, nil, err
		}
		rr.BackupPath = &backup
		change.BackupPath = &backup
		change.Action = "replaced-cxc"
		rr.AppliedDigest = switchDigest(want)
		return change, undo, switchPublishRole(rr.Path, want)
	}
	change.Action = "kept-other"
	return change, nil, nil
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
	switch rr.PriorOwner {
	case string(role.OwnerCXC):
		switch owner {
		case role.OwnerCRW:
			if rr.BackupPath == nil {
				change.Action = "left-no-backup"
				return change, nil, nil
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
		case role.OwnerCXC:
			change.Action = "already-cxc"
		default:
			change.Action = "left-modified"
		}
	case string(role.OwnerNone):
		switch owner {
		case role.OwnerCRW:
			change.Action = "removed"
			return change, undo, switchRestoreBytes(rr.Path, nil)()
		case role.OwnerNone:
			change.Action = "already-absent"
		default:
			change.Action = "left-modified"
		}
	default:
		change.Action = "unchanged"
	}
	return change, nil, nil
}
