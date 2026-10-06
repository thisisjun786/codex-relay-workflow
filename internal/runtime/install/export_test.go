package install

import (
	"context"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// ReplaceSettingsWriter makes every settings write go through write until the returned function
// restores the real one.
func ReplaceSettingsWriter(write func(path string, text []byte) error) (restore func()) {
	saved := writeSettings
	writeSettings = write
	return func() { writeSettings = saved }
}

// ReplaceSelectionCommit makes the promotion's commit go through commit until restored.
func ReplaceSelectionCommit(commit func(path string, definitionVersion int, delta record.Delta) (reading.Reading, error)) (restore func()) {
	saved := commitSelection
	commitSelection = func(_ context.Context, path string, definitionVersion int, delta record.Delta) (reading.Reading, error) {
		return commit(path, definitionVersion, delta)
	}
	return func() { commitSelection = saved }
}

// ReplacePointerPlacement makes the promotion's pointer move go through place until restored.
func ReplacePointerPlacement(place func(path, target string) error) (restore func()) {
	saved := placePointer
	placePointer = place
	return func() { placePointer = saved }
}

// ReplaceStateBackupStep makes the state-directory backup call step (it is called with "copied", after the copy and
// before its verification) until restored: a test makes the backup fail there, or changes the state directory under it.
func ReplaceStateBackupStep(step func(string) error) (restore func()) {
	saved := stateBackupStep
	stateBackupStep = step
	return func() { stateBackupStep = saved }
}

// ReplaceStateBackupListed makes the state-directory backup call listed after its first listing and before anything
// is copied, until restored: a test makes the store's sidecars appear or go between the listing and the copy.
func ReplaceStateBackupListed(listed func() error) (restore func()) {
	saved := stateBackupListed
	stateBackupListed = listed
	return func() { stateBackupListed = saved }
}

// StoreSidecarWal is the manifest's reading of the store's write-ahead log, from what happened to it while the backup
// was made: its bytes were copied, it was listed and had gone by its copy, it was absent from the first listing and
// appeared in the second so it was not copied, or it was absent from both. The appeared reading cannot be reached end
// to end — the swap gate's own read of the store leaves an empty log before the first listing — so a white-box test
// pins it here.
func StoreSidecarWal(copied, gone, appeared bool) string {
	return storeSidecarWal(copied, gone, appeared)
}

// ListingDiff is how two readings of a state directory differ, or "": the comparison the backup's verification makes,
// with the store's two sidecars left out. A white-box test pins the sidecars' exclusion here.
func ListingDiff(a, b []string) string {
	toEntries := func(paths []string) []backedUp {
		out := make([]backedUp, 0, len(paths))
		for _, p := range paths {
			out = append(out, backedUp{Path: p, Kind: "file", Size: 1})
		}
		return out
	}
	return listingDiff(toEntries(a), nil, toEntries(b), nil)
}

// ReplaceDirectorySync makes the backup's directory syncs go through wrap, which is given the directory and the real
// sync, until restored.
func ReplaceDirectorySync(wrap func(path string, next func(string) error) error) (restore func()) {
	saved := syncDirectory
	syncDirectory = func(path string) error { return wrap(path, saved) }
	return func() { syncDirectory = saved }
}

// ReplaceModeSync makes the sync of an entry after its mode is set go through wrap, which is given the path and the
// real sync, until restored.
func ReplaceModeSync(wrap func(path string, next func(string) error) error) (restore func()) {
	saved := syncAfterMode
	syncAfterMode = func(path string) error { return wrap(path, saved) }
	return func() { syncAfterMode = saved }
}

// ListedModes is what the backup's listing of a state directory records for each entry: its mode in the source and
// the mode its copy is to be given.
func ListedModes(source, dbPath string) (map[string][2]os.FileMode, error) {
	entries, _, err := listState(source, dbPath)
	if err != nil {
		return nil, err
	}
	out := map[string][2]os.FileMode{}
	for _, e := range entries {
		out[e.Path] = [2]os.FileMode{os.FileMode(e.Mode), os.FileMode(e.CopyMode)}
	}
	return out, nil
}

// CopyMode is the mode a backup's copy of an entry is given.
func CopyMode(dir bool, mode os.FileMode) os.FileMode { return copyMode(dir, mode) }

// ReplaceBeforeWriteLock runs between every settings or bridge record write's decision and the
// lock it acts under, until restored.
func ReplaceBeforeWriteLock(between func(path string)) (restore func()) {
	saved := beforeWriteLock
	beforeWriteLock = between
	return func() { beforeWriteLock = saved }
}

// ReplaceProcessOwner makes the process table's owner reading go through owner until restored,
// so that a fake /proc can hold another user's processes.
func ReplaceProcessOwner(owner func(dir string) (int, error)) (restore func()) {
	saved := processOwner
	processOwner = owner
	return func() { processOwner = saved }
}
