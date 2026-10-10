package configguard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/tomledit"
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
	// Failed lists the flags crw owns whose disable did not succeed (CRW-1145); the ownership is not released while it is
	// not empty, so a retry disables them.
	Failed []FailedFlag `json:"failed"`
	// Released reports a manifest a completed deactivation already released: there is nothing left to revert.
	Released bool `json:"released"`
	// Recovered names what this command recorded of an interrupted earlier change before reverting (CRW-1153).
	Recovered []string `json:"recovered"`
}

// FailedFlag is one flag a deactivation could not disable.
type FailedFlag struct {
	Key      string `json:"key"`
	ExitCode int    `json:"exitCode"`
	Message  string `json:"message"`
}

// configLockPathsPinned answers the path this deactivation must work on, and refuses when the file
// the lock guards cannot be proven. The lock is keyed by the file it guards, but its spelling was
// resolved before this command waited, and a directory symlink in the path can be retargeted during
// the wait. The path is therefore pinned to the file the held sidecar belongs to: the sidecar beside
// the resolved real path must be the very file this lock holds (fstat), or the directory changed
// under the wait and acting now would edit a file this lock does not guard (CRW-899 E1, fail
// closed). The Go-side operations — the drift hash, the read and the restore — work on the pinned
// path, never on a spelling re-resolved after the wait. The injected CLI calls take only feature
// names and read CODEX_HOME from the environment, so they are not pinned by this path.
func configLockPathsPinned(lock *crwdir.ConfigLock) (*configLockPathsPin, error) {
	pinned, ok := configLockPathsRealPath(lock.Target)
	if !ok || !lock.HoldsSidecar(pinned) {
		return nil, fmt.Errorf("the config file's directory changed while the lock was being taken (%s); run the deactivation again", lock.Target)
	}
	// The identities are captured HERE, once, while the sidecar proof above holds. Every later
	// comparison is judged against these captured values, never against the pinned path read from
	// the filesystem again: re-reading it would follow a directory replaced after this point and
	// accept the replacement as the locked file (CRW-899's fifth-generation evaluation).
	// A config file that is already gone is not an error — an uninstall whose config.toml was
	// removed still disables flags — so a missing file leaves the file identity nil and the
	// comparison falls back to comparing resolved paths.
	dirPath := filepath.Dir(pinned)
	dir, err := os.Stat(dirPath)
	if err != nil {
		return nil, fmt.Errorf("the config file's directory could not be inspected (%s); run the deactivation again", dirPath)
	}
	file, err := os.Stat(pinned)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("the config file could not be inspected (%s); run the deactivation again", pinned)
	}
	dirFd, dirErr := configLockPathsOpenDir(dirPath)
	return &configLockPathsPin{lock: lock, path: pinned, file: file, dir: dir, dirFd: dirFd, dirErr: dirErr}, nil
}

// configLockPathsOpenDir opens the pinned directory for a search-only lookup.
func configLockPathsOpenDir(dirPath string) (int, error) {
	fd, err := unix.Open(dirPath, configLockPathsDirFlags, 0)
	if err != nil {
		return -1, err
	}
	return fd, nil
}

// configLockPathsPin is the locked config file as it was when the lock was proven: its resolved path
// and the identities of the file and of its parent directory at that moment. A comparison against a
// later manifest is judged against these captured values, so a path replaced after the pin cannot be
// accepted as the locked file. The lock itself is kept so the pin can re-prove, at the moment of the
// decision, that the pinned path still names the sidecar the lock holds; without that proof the
// captured identities describe a file the lock no longer guards.
type configLockPathsPin struct {
	lock *crwdir.ConfigLock
	path string
	file os.FileInfo
	dir  os.FileInfo
	// dirFd holds the pinned directory open for a search-only lookup; -1 when it could not be opened.
	dirFd  int
	dirErr error
}

// close releases the held directory descriptor.
func (p *configLockPathsPin) close() {
	if p != nil && p.dirFd >= 0 {
		_ = unix.Close(p.dirFd)
		p.dirFd = -1
	}
}

// configLockPathsPinHolds reports whether the pinned path still names the very sidecar this lock
// holds. The pin is captured while the sidecar proof holds, but the directory behind the pinned path
// can be replaced afterwards: swapping the pinned directory with another one makes the pinned
// pathname name a different file while the held sidecar now lives beside the moved directory. The
// captured identities alone cannot see that — the replacement's file and directory are a different
// pair, but a manifest naming the moved directory still matches the captured pair — so the comparison
// re-proves the lock's own sidecar identity here and refuses when it no longer holds (CRW-899 E1,
// fail closed). This re-stat is the lock's identity check, not a re-interpretation of the pin as a
// comparison target: a stale pin makes the answer false, never a different acceptance.
func configLockPathsPinHolds(pinned *configLockPathsPin) bool {
	return pinned != nil && pinned.lock.HoldsSidecar(pinned.path)
}

// configLockPathsParentLive reports whether the directory the pinned pathname names is still the
// directory the pin captured. The sidecar and the file are reached through the pathname, so a
// directory moved away after the pin and replaced under the old name, with hard links to the file and
// the sidecar, passes the sidecar and file checks while the pathname now lies in the replacement. The
// restore would publish there and leave the manifest's file untouched (CRW-899 d1, CRW-993 c1).
func configLockPathsParentLive(pinned *configLockPathsPin) bool {
	if pinned == nil || pinned.dir == nil {
		return false
	}
	live, err := os.Stat(filepath.Dir(pinned.path))
	return err == nil && os.SameFile(live, pinned.dir)
}

// configLockPathsPublishGuard is the check a restore runs immediately before it publishes: the lock
// still holds the pinned sidecar and the pinned pathname still lies in the pinned directory. Nothing is
// published when it refuses. It is called once before the restore is computed and again at the rename
// (CRW-993 c1).
func configLockPathsPublishGuard(pinned *configLockPathsPin) error {
	if !configLockPathsPinHolds(pinned) || !configLockPathsParentLive(pinned) {
		return fmt.Errorf("the config file's directory changed before the restore was published (%s); nothing was written; run the deactivation again", configLockPathsPinnedName(pinned))
	}
	return nil
}

// configLockPathsPinnedName names the pinned path in a refusal, empty for a nil pin.
func configLockPathsPinnedName(pinned *configLockPathsPin) string {
	if pinned == nil {
		return ""
	}
	return pinned.path
}

func readTextOrNull(path string) (*string, error) {
	b, exists, err := activationReadFile(path)
	if err != nil || !exists {
		return nil, err
	}
	s := string(b)
	return &s, nil
}

// configLockPathsRealPath answers the path a config file really has on disk once every symlink in it
// is followed, so two spellings of one file compare equal. Links are resolved first and only then is
// a relative result joined to the resolved working directory: cleaning or absolutising the input
// before the resolution would compare spellings instead of the file, which is the defect CRW-899
// fixes (a relative CODEX_HOME and an absolute manifest path named one file and compared unequal).
// A final component that is not there — an install whose config.toml was removed — is named through
// its resolved parent, so the caller still pins a real path for the directory entry it will use.
// The second answer is false when nothing can be resolved, and every caller treats that as "not the
// same file": the comparison only ever widens acceptance to spellings that name one path.
func configLockPathsRealPath(p string) (string, bool) {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return configLockPathsAbsolute(resolved)
	}
	// Only the final component may be missing — an install whose config.toml was removed. It is
	// named through its parent, and that parent must resolve completely: synthesising a missing
	// intermediate directory would let a path the kernel cannot resolve be answered as the locked
	// file (CRW-899's pre-merge d1). The split is lexical and does NOT clean, so a ".." reaches the
	// kernel applied to the directory it has already resolved rather than being folded into the
	// spelling (E2).
	dir, base := filepath.Split(p)
	if base == "" || base == "." || base == ".." {
		return "", false
	}
	if dir == "" {
		dir = "."
	} else if trimmed := strings.TrimSuffix(dir, string(filepath.Separator)); trimmed != "" {
		// A trailing separator is dropped so the parent resolves as a directory, but the root
		// separator is not: "" would resolve to the working directory instead of "/".
		dir = trimmed
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}
	absDir, ok := configLockPathsAbsolute(realDir)
	if !ok {
		return "", false
	}
	return filepath.Join(absDir, base), true
}

// configLockPathsAbsolute makes a kernel-resolved path absolute in kernel terms: a relative result
// is joined to the resolved working directory, never to the lexical one, so two spellings of one
// file compare equal however the process was started.
func configLockPathsAbsolute(resolved string) (string, bool) {
	if filepath.IsAbs(resolved) {
		return resolved, true
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	realCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", false
	}
	return filepath.Join(realCwd, resolved), true
}

// configLockPathsSameTarget reports whether a manifest's spelling names the file the lock still
// guards. It is configLockPathsSameTargetReason without the reason.
func configLockPathsSameTarget(spelling string, pinned *configLockPathsPin) bool {
	ok, _ := configLockPathsSameTargetReason(spelling, pinned)
	return ok
}

// configLockPathsSameTargetReason reports whether a manifest's spelling names the file the lock still
// guards. It first re-proves that the pinned path still names the held sidecar
// (configLockPathsPinHolds): a directory replaced after the pin moves the held sidecar away from the
// pinned pathname, so the answer is false whatever the manifest names (CRW-899 E1). The manifest is
// allowed to name the config file through a different spelling, such as CODEX_HOME behind a directory
// symlink. The comparison is directory-entry identity, not inode identity: the restore publishes through
// the pinned path with an atomic rename, which replaces that one pathname, so a hard link to the same
// inode under another name would keep the managed key while this command reported it restored. A hard
// link therefore stays refused. The error is non-nil only when the spelling cannot be proven to name the
// locked entry for a stated reason (CRW-993 d2).
func configLockPathsSameTargetReason(spelling string, pinned *configLockPathsPin) (bool, error) {
	// A directory that lost search permission after the pin cannot have its sidecar looked up, so the
	// lock's proof fails for a reason that is not a different file. That refusal names the reason (CRW-993 d4).
	if pinned != nil {
		if _, err := os.Stat(pinned.path + configLockPathsSidecarSuffix); errors.Is(err, fs.ErrPermission) {
			return false, fmt.Errorf("%s cannot be proven to be the locked config file: its directory cannot be searched (%w)", spelling, err)
		}
	}
	if !configLockPathsPinHolds(pinned) {
		return false, nil
	}
	// The live parent of the pinned pathname must still be the directory the pin captured. Without
	// this, a directory moved after the pin and replaced under the old name is accepted through the
	// hard links the replacement holds, and the restore publishes into the replacement (CRW-993 c1).
	if !configLockPathsParentLive(pinned) {
		return false, nil
	}
	// The pinned pathname itself must still name the pinned file. The restore publishes through
	// pin.path, so an entry renamed away, leaving only a folded-name sibling that still matches the
	// captured file and directory identities, would be accepted here and then restored to a path that
	// no longer exists (CRW-899's thirteenth evaluation).
	if pinned.file != nil {
		if live, err := os.Stat(pinned.path); err != nil || !os.SameFile(live, pinned.file) {
			return false, nil
		}
	}
	real, ok := configLockPathsRealPath(spelling)
	if !ok {
		return false, nil
	}
	if real == pinned.path {
		return true, nil
	}
	if pinned.file == nil {
		// The config file was already gone at pin time: there is nothing to compare it with, and
		// the absent-config path restores nothing.
		return false, nil
	}
	// The spellings differ. On a case-insensitive filesystem they can still name one directory entry,
	// and filepath.EvalSymlinks follows links without canonicalising the case of ordinary components.
	// Such a spelling is the same target only when the directory holds it as the locked entry.
	if !strings.EqualFold(filepath.Base(real), filepath.Base(pinned.path)) {
		return false, nil
	}
	realDir, err := os.Stat(filepath.Dir(real))
	if err != nil || !os.SameFile(realDir, pinned.dir) {
		return false, nil
	}
	if filepath.Base(real) == filepath.Base(pinned.path) {
		// The two spellings reach one file and one parent directory under the SAME basename, so they are
		// one directory entry whatever else the directory holds.
		realFile, err := os.Stat(real)
		if err != nil || !os.SameFile(realFile, pinned.file) {
			return false, nil
		}
		return true, nil
	}
	if configLockPathsReadable(filepath.Dir(pinned.path)) {
		// A readable parent keeps the enumeration the comparison has always used: the candidate must
		// reach the pinned file, and the folded name must hold exactly one entry.
		realFile, err := os.Stat(real)
		if err != nil || !os.SameFile(realFile, pinned.file) {
			return false, nil
		}
		return configLockPathsOneFoldedEntry(real, pinned), nil
	}
	return configLockPathsProbedEntry(real, pinned)
}

// configLockPathsOneFoldedEntry reports whether the candidate's spelling and the locked file's name
// reach exactly one directory entry. The kernel permits no two entries with fold-equal names on a
// directory that folds case, so one such entry IS the locked entry, and two, a case-sensitive sibling
// or a hard link, are entries a rename over the locked path would not reach. It is used only for a
// readable parent; an unreadable one goes through configLockPathsProbedEntry.
func configLockPathsOneFoldedEntry(real string, pinned *configLockPathsPin) bool {
	dir := filepath.Dir(real)
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A readable parent that still cannot be enumerated cannot prove the count, so the comparison
		// refuses (fail closed).
		return false
	}
	matches := 0
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), filepath.Base(pinned.path)) {
			matches++
		}
	}
	return matches == 1
}

// configLockPathsFold is what the pinned directory answers about case: it folds case, it is
// case-sensitive, or the answer is unknown.
type configLockPathsFold int

const (
	configLockPathsFoldUnknown configLockPathsFold = iota
	configLockPathsFoldFolds
	configLockPathsFoldSensitive
)

// configLockPathsFoldProbe answers whether the pinned directory folds case. It is a variable so a test
// can answer for a directory on a filesystem of either kind (CRW-993 d2).
var configLockPathsFoldProbe = configLockPathsProbeFold

// configLockPathsEntryLookup answers the identity of the entry a name reaches in the pinned directory.
// It is a variable so a test can answer for a lookup the host cannot make (CRW-993 d2).
var configLockPathsEntryLookup = configLockPathsLookupEntry

// configLockPathsSidecarSuffix is the suffix of the sidecar the lock is held on (crwdir's lock suffix).
const configLockPathsSidecarSuffix = ".crw-lock"

// configLockPathsProbeFold learns whether the pinned directory folds case, without listing it and without
// writing into it. The ASCII case-swapped spelling of the sidecar's basename is looked up through the held
// directory descriptor without following a link. ENOENT means the directory is case-sensitive. A hit is
// trusted only when one fstat of the descriptor the lock holds gives the sidecar a link count of one and
// the swapped name reaches that same entry: then the directory folds case. Anything else is unknown, and
// the reason names which of the four causes applies (CRW-993 d1, d2).
func configLockPathsProbeFold(pinned *configLockPathsPin) (configLockPathsFold, string) {
	if pinned == nil || pinned.dirFd < 0 || pinned.lock == nil {
		return configLockPathsFoldUnknown, "the case-swapped lookup failed (the directory has no descriptor)"
	}
	sidecar := filepath.Base(pinned.path) + configLockPathsSidecarSuffix
	swapped, letters := configLockPathsCaseSwap(sidecar)
	if !letters {
		return configLockPathsFoldUnknown, "the name has no ASCII letter"
	}
	dev, ino, err := configLockPathsStatEntry(pinned.dirFd, swapped)
	if errors.Is(err, fs.ErrNotExist) {
		return configLockPathsFoldSensitive, ""
	}
	if err != nil {
		return configLockPathsFoldUnknown, fmt.Sprintf("the case-swapped lookup failed (%v)", err)
	}
	held, err := pinned.lock.HeldInfo()
	if err != nil {
		return configLockPathsFoldUnknown, fmt.Sprintf("the case-swapped lookup failed (the held lock file cannot be stat'ed: %v)", err)
	}
	st, ok := held.Sys().(*syscall.Stat_t)
	if !ok {
		return configLockPathsFoldUnknown, "the case-swapped lookup failed (the lock file's identity is unavailable)"
	}
	if uint64(st.Nlink) != 1 {
		return configLockPathsFoldUnknown, "the sidecar has another name (its link count is not one)"
	}
	hdev, hino, _ := configLockPathsIdentity(held)
	if hdev != dev || hino != ino {
		return configLockPathsFoldUnknown, "the identity differs: the case-swapped name reaches another entry"
	}
	return configLockPathsFoldFolds, ""
}

// configLockPathsCaseSwap swaps the ASCII letters of name and reports whether it had any.
func configLockPathsCaseSwap(name string) (string, bool) {
	b := []byte(name)
	letters := false
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z':
			b[i] = c - 'a' + 'A'
			letters = true
		case c >= 'A' && c <= 'Z':
			b[i] = c - 'A' + 'a'
			letters = true
		}
	}
	return string(b), letters
}

// configLockPathsLookupEntry answers the identity of the entry name reaches through the pinned
// directory's held descriptor, without following a final link and without listing the directory.
func configLockPathsLookupEntry(pinned *configLockPathsPin, name string) (uint64, uint64, error) {
	if pinned == nil || pinned.dirFd < 0 {
		if pinned != nil && pinned.dirErr != nil {
			return 0, 0, pinned.dirErr
		}
		return 0, 0, fs.ErrPermission
	}
	return configLockPathsStatEntry(pinned.dirFd, name)
}

// configLockPathsStatEntry is fstatat without following a final link, relative to an open directory.
func configLockPathsStatEntry(dirFd int, name string) (uint64, uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, 0, err
	}
	return uint64(st.Dev), uint64(st.Ino), nil
}

// configLockPathsIdentity is the device and inode of a stat result.
func configLockPathsIdentity(info os.FileInfo) (uint64, uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}

// configLockPathsReadable reports whether the process may read the directory's entries. It answers
// from the permission bits, so it lists nothing.
func configLockPathsReadable(dir string) bool {
	return unix.Access(dir, unix.R_OK) == nil
}

// configLockPathsProbedEntry decides, for an unreadable parent, whether the candidate names the locked
// entry. A folding directory is proved by a by-name lookup through the held descriptor, which must give
// the pinned file's identity. A case-sensitive directory refuses, because a differently cased name is a
// different entry even when it is a hard link. An unknown answer refuses with its reason, which is the one
// exception the same-file restore promise has (CRW-993 d2).
func configLockPathsProbedEntry(real string, pinned *configLockPathsPin) (bool, error) {
	name := filepath.Base(real)
	fold, reason := configLockPathsFoldProbe(pinned)
	switch fold {
	case configLockPathsFoldSensitive:
		return false, nil
	case configLockPathsFoldFolds:
	default:
		return false, fmt.Errorf("%s cannot be shown to be the locked config file: the case behaviour of its directory is unknown, %s", name, reason)
	}
	dev, ino, err := configLockPathsEntryLookup(pinned, name)
	if err != nil {
		return false, fmt.Errorf("%s cannot be looked up in its directory to prove it is the locked config file: %w", name, err)
	}
	pdev, pino, ok := configLockPathsIdentity(pinned.file)
	return ok && dev == pdev && ino == pino, nil
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
	case *live != rec.AppliedValue && !tomledit.SameValue(*live, rec.AppliedValue):
		// The values are compared as TOML values (CRW-1149): the same value written another way is still crw's.
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
func Deactivate(deps DeactivateDeps) (_ *DeactivateResult, err error) {
	now := deps.Now
	if now == nil {
		now = func() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
	}
	// Oracle parity: marker failure never gates uninstall, including early exits. The
	// marker API itself refuses unreadable records rather than replacing their consent data.
	markOptedOut := func() { _ = MarkSelfHealOptedOut(deps.CodexHome, now()) }
	// unsynced is the durability error of a record this command published: it is in place and kept, and the command reports
	// it (CRW-1153) instead of a success the directory sync did not back.
	var unsynced, recoveryDur error
	durability := func() error { return errors.Join(recoveryDur, txDurability(unsynced)) }
	// A deactivation that stops after such a record was published reports the uncertainty with the stop.
	defer func() { err = txWithDurability(err, errors.Join(recoveryDur, unsynced)) }()
	r := &DeactivateResult{Disabled: []string{}, SkippedPreExisting: []string{}, NoManifest: true, RestoredKeys: []string{}, SkippedExternal: []SkippedExternal{}, Failed: []FailedFlag{}, Recovered: []string{}}
	noManifest := func() (*DeactivateResult, error) {
		markOptedOut()
		r.NoManifest = true
		return r, durability()
	}
	// An interrupted change is recorded first, under the lock of the config file it is about, so this deactivation reverts
	// what that change did (CRW-1153).
	if pending, err := pendingIntentConfig(deps.CodexHome); err != nil {
		return nil, err
	} else if pending != "" {
		lockPath := deps.ConfigPath
		if lockPath == "" {
			lockPath = pending
		}
		lock, err := crwdir.LockConfig(lockPath, activationLockWait)
		if err != nil {
			return nil, err
		}
		recovered, err := recoverIntent(deps.CodexHome, lockPath, deps.Run)
		lock.Release()
		if err != nil && !crwdir.Published(err) {
			return nil, err
		}
		r.Recovered = recovered
		// A record in place whose directory sync failed is reported with this command's result (CRW-1153).
		recoveryDur = err
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
	// A manifest a completed deactivation released owns nothing any more: reverting from it again would turn off a flag
	// or reset a key the user set since (CRW-1145).
	released := func() (*DeactivateResult, error) {
		markOptedOut()
		r.Released = true
		return r, durability()
	}
	if m.ReleasedAt != nil {
		return released()
	}
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
	var pin *configLockPathsPin
	// locks and dirInfo re-prove, after every CLI run, that the file the caller's path names is still guarded by a lock this
	// command holds (CRW-1144): a disable may replace config.toml, and another writer may lock the new file.
	var locks *configLocks
	var dirInfo os.FileInfo
	if path != "" && configLockWritersDeactivateWrites(m) {
		lock, err := crwdir.LockConfig(path, activationLockWait)
		if err != nil {
			return nil, err
		}
		defer lock.Release()
		locks = &configLocks{main: lock}
		defer locks.releaseExtra()
		dirInfo, _ = os.Stat(filepath.Dir(lockedPath))
		var pinErr error
		pin, pinErr = configLockPathsPinned(lock)
		if pinErr != nil {
			return nil, pinErr
		}
		defer pin.close()
		path = pin.path
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
		if m.ReleasedAt != nil {
			return released()
		}
		// The lock is held on the file the first reading named. A manifest that now names a different
		// config file would have this deactivation apply one file's ownership records to another, so
		// it refuses rather than acting under the wrong lock (fail closed). An explicit ConfigPath
		// overrides the manifest in both readings, so only the derived path can disagree.
		if deps.ConfigPath != "" {
			// An explicit ConfigPath has no manifest spelling to agree with, but the pinned path must
			// still name the pinned file: the override chooses WHICH file this command acts on, and a
			// directory replacement or a final-component symlink swapped in after the pin would send
			// the read, the restore and the hash to another file without its lock. The same proof the
			// manifest branch uses runs here, against the pinned path itself (CRW-899's eleventh and
			// twelfth evaluations).
			if !configLockPathsSameTarget(pin.path, pin) {
				return nil, fmt.Errorf("the config file's directory changed while the lock was held (%s); run the deactivation again", lockedPath)
			}
		} else if ok, reason := configLockPathsSameTargetReason(m.ConfigPath, pin); !ok {
			if reason != nil {
				return nil, fmt.Errorf("the install manifest names %s, which cannot be proven to be the locked config file (%w); nothing was written", m.ConfigPath, reason)
			}
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
	// A config.toml that does not decode is refused before any restore and before the CLI, which would rewrite it (CRW-1141):
	// whenever this deactivation is to write, a key to restore or a flag to disable.
	if content != nil && configLockWritersDeactivateWrites(m) {
		if err := validateConfig(path, *content); err != nil {
			return nil, err
		}
	}
	if content != nil && len(m.TableKeys) > 0 {
		// The restore is computed under the lock and published only when the pin still holds at the
		// rename; the check runs here first so a refusal is reported before the keys are computed.
		guard := func() error { return configLockPathsPublishGuard(pin) }
		if pin != nil {
			if err := guard(); err != nil {
				return nil, err
			}
		}
		if err := deactivateTableKeys(path, *content, m, r, guard); crwdir.Published(err) {
			// The restored file is in place; its directory sync is reported at the end.
			unsynced = errors.Join(unsynced, err)
		} else if err != nil {
			return nil, err
		}
	}
	live, err := ReadFeatureStates(deps.Run)
	r.FeaturesStateUnavailable = err != nil
	// ran holds the flags whose disable exited 0: they are not yet disabled, only asked to be, and are reported as disabled
	// only once they are read back (CRW-1143).
	ran := []string{}
	for _, key := range manifestOrder(m.flagOrder, m.Flags) {
		flag := m.Flags[key]
		if flag.PriorEnabled {
			r.SkippedPreExisting = append(r.SkippedPreExisting, key)
			continue
		}
		if !flag.EnabledByCodexclaw {
			continue
		}
		// A flag the listing shows off has nothing to disable. A flag it has no row for is not shown off (CRW-1145): while
		// config.toml holds it on, or in a form crw cannot read, the disable still runs and the read-back decides; the
		// install is released only once the flag is confirmed off, or config.toml no longer turns it on.
		if state, present := live[key]; present && (state == FeatureDisabled || state == FeatureUnsupported && !deactivateConfigHolds(path, key)) {
			r.SkippedExternal = append(r.SkippedExternal, SkippedExternal{key, SkipMissing})
			continue
		}
		res := deps.Run([]string{"features", "disable", key})
		if res.ExitCode != 0 {
			// A failed disable is reported, and keeps the ownership for a retry (CRW-1145).
			r.Failed = append(r.Failed, FailedFlag{key, res.ExitCode, activationFailureMessage(res.Stderr)})
		} else {
			ran = append(ran, key)
		}
		// The disable may have replaced config.toml (CRW-1144): the next one runs only under a lock that guards the file the
		// path names now. Otherwise nothing more runs, nothing is released, and the ownership stays for a retry.
		if err := deactivateRecheck(locks, lockedPath, dirInfo); err != nil {
			return r, fmt.Errorf("%w; the flags after %s were not disabled, the disables that ran are not confirmed, and crw's ownership is kept: run 'crw install features disable' again", err, key)
		}
	}
	// An exit 0 is not proof (CRW-1143): the flags are read back, and a flag still enabled, or one that cannot be read back,
	// is a failure that keeps the ownership.
	if len(ran) > 0 {
		observed, err := readFeatureStatesFor(deps.Run, ran)
		confirmed := []string{}
		for _, key := range ran {
			// Only a flag read back disabled is confirmed: a flag without a row (unsupported) says nothing about the value
			// config.toml holds, so it stays crw's (CRW-1143).
			switch {
			case err != nil:
				r.Failed = append(r.Failed, FailedFlag{key, 0, "codex features disable exited 0, but the flags could not be read back to confirm it: " + err.Error()})
			case observed[key] == FeatureDisabled:
				confirmed = append(confirmed, key)
			case observed[key] == FeatureEnabled:
				r.Failed = append(r.Failed, FailedFlag{key, 0, "codex features disable exited 0, but the flag is still enabled"})
			default:
				r.Failed = append(r.Failed, FailedFlag{key, 0, "codex features disable exited 0, but the flag reads " + string(observed[key]) + " (codex features list has no row for it), so the disable is not confirmed"})
			}
		}
		r.Disabled = confirmed
		if err := deactivateRecheck(locks, lockedPath, dirInfo); err != nil {
			return r, fmt.Errorf("%w; the disables are not confirmed and crw's ownership is kept: run 'crw install features disable' again", err)
		}
	}
	// A deactivation that reverted everything it owned releases the manifest: the records stay as evidence, and the next
	// activation starts a new baseline (CRW-1145). A flag that failed to disable, or a key whose provenance could not be
	// proven, is unresolved ownership and keeps the manifest live for a retry. An install that owned nothing is released too,
	// or the next activation would carry its first prior states as if they still described the flags. The release is written
	// only under the config lock (the one this command holds when it owned something, otherwise taken now), and only over a
	// regular file.
	if len(r.Failed) == 0 && !deactivateUnresolved(r) && deactivateRegularFile(manifestPath(deps.CodexHome)) && (pin != nil || path != "") {
		if pin == nil {
			lock, err := crwdir.LockConfig(path, activationLockWait)
			if err != nil {
				return r, fmt.Errorf("crw owned nothing to revert, but the release of the install manifest could not be recorded under the config lock (%w); run 'crw install features disable' again", err)
			}
			defer lock.Release()
			// An activation that published while this command waited owns what its manifest records: that manifest is not
			// released from this command's reading.
			if fresh, _ := readTextOrNull(manifestPath(deps.CodexHome)); fresh == nil || *fresh != *raw {
				return r, errors.New("the install manifest changed while the deactivation ran, so it was not released; run 'crw install features disable' again")
			}
		}
		at := now()
		m.ReleasedAt = &at
		b, err := manifestBytes(m)
		if err != nil {
			return r, err
		}
		if _, _, err := activationReadFile(manifestPath(deps.CodexHome)); err != nil {
			return r, fmt.Errorf("everything crw owned was reverted, but the release could not be recorded in the install manifest: %w", err)
		}
		if err := activationCrwdirPublish(manifestPath(deps.CodexHome), b); crwdir.Published(err) {
			// The release is in place and kept; it is not known to be durable, which the command reports.
			unsynced = errors.Join(unsynced, fmt.Errorf("%s: %w", manifestPath(deps.CodexHome), err))
		} else if err != nil {
			return r, fmt.Errorf("everything crw owned was reverted, but the release could not be recorded in the install manifest: %w", err)
		}
	}
	return r, durability()
}

// deactivateConfigHolds reports whether config.toml may turn the feature flag on: it holds true, holds a form crw cannot read, or
// cannot be read at all. It is asked of a flag the listing has no row for.
func deactivateConfigHolds(path, key string) bool {
	content, err := readTextOrNull(path)
	if err != nil {
		return true
	}
	if content == nil {
		return false
	}
	look := semanticRead(*content, "features", key)
	switch look.State {
	case tomledit.Absent:
		return false
	case tomledit.Found:
		return !tomledit.SameValue(look.Raw, "false")
	}
	return true
}

// deactivateRecheck is configLocks.recheck for a deactivation that holds the config lock; one that holds none (an empty
// config path) has no lock to re-prove.
func deactivateRecheck(locks *configLocks, path string, dir os.FileInfo) error {
	if locks == nil {
		return nil
	}
	return locks.recheck(path, dir)
}

// deactivateUnresolved reports a key the deactivation left because its provenance could not be proven.
func deactivateUnresolved(r *DeactivateResult) bool {
	for _, skipped := range r.SkippedExternal {
		if skipped.Reason == SkipUnverifiable {
			return true
		}
	}
	return false
}

// deactivateRegularFile reports whether path is (or links to) a regular file.
func deactivateRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func deactivateTableKeys(path, content string, m *InstallManifest, r *DeactivateResult, guard func() error) error {
	var backup *string
	if m.BackupPath != nil && *m.BackupPath != "" {
		// A read failure is unknown provenance, not evidence of an absent backup key.
		backup, _ = readTextOrNull(*m.BackupPath)
	}
	changed := false
	for _, id := range manifestOrder(m.tableOrder, m.TableKeys) {
		rec := m.TableKeys[id]
		// The live value and the backup's are read through the semantic editor (CRW-1141). A key the user rewrote in a form
		// the editor does not touch is no longer the value CRW applied; a backup that does not decode proves nothing.
		live, editable := semanticRaw(content, rec.Table, rec.Key)
		if !editable {
			r.SkippedExternal = append(r.SkippedExternal, SkippedExternal{id, SkipChanged})
			continue
		}
		var prior *string
		backupKnown := backup != nil
		if backupKnown {
			prior, backupKnown = semanticRaw(*backup, rec.Table, rec.Key)
		}
		restore, reason := DecideKeyRestore(rec, live, r.FileDrifted, backupKnown, prior)
		if !restore {
			r.SkippedExternal = append(r.SkippedExternal, SkippedExternal{id, reason})
			continue
		}
		edit, _, err := semanticRestore(content, rec.Table, rec.Key, rec.PriorValue)
		if err != nil {
			return err
		}
		if edit.Action == TomlUnsupportedValue {
			r.SkippedExternal = append(r.SkippedExternal, SkippedExternal{id, SkipChanged})
			continue
		}
		content, changed = edit.Content, changed || edit.Changed
		r.RestoredKeys = append(r.RestoredKeys, id)
	}
	if changed {
		return configLockPathsPublishChecked(path, []byte(content), guard)
	}
	return nil
}
