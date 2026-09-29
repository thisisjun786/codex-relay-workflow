package install

import (
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
	commitSelection = commit
	return func() { commitSelection = saved }
}

// ReplacePointerPlacement makes the promotion's pointer move go through place until restored.
func ReplacePointerPlacement(place func(path, target string) error) (restore func()) {
	saved := placePointer
	placePointer = place
	return func() { placePointer = saved }
}

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
