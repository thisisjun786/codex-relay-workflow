package install

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// LauncherName is the fallback Stop launcher the cached Python bootstrap runs from the Codex
// home when its version cache is gone (completion.LAUNCHER_NAME).
const LauncherName = "crw-stop-hook.py"

// LauncherMarker is what every launcher CRW places carries (completion.LAUNCHER_MARKER). It is a
// claim of ownership, not of provenance: CRW put a launcher at that path, not who ran the
// command and not that the bytes are intact.
const LauncherMarker = "crw-stop-hook/1"

// Launcher removal outcomes, in the vocabulary the Python remover answered with.
const (
	LauncherRemoved = "settled"
	LauncherAbsent  = "already_done"
	LauncherWould   = "would_change"
	LauncherRefused = "refused"
	LauncherBusy    = "busy"
)

// RemoveLauncher deletes <codexHome>/crw-stop-hook.py, and only when it is CRW's: a regular file
// carrying LauncherMarker (ported from scripts/crw_transition/steps.py launcher_remove, the one
// remover that checked ownership). The cutover calls it once the retention scan is clear; it is
// deliberately not a command.
//
// A file without the marker is somebody else's and stays where it is. A symlink, a directory or
// anything else is reported by kind and never followed, because removing through a link deletes
// a file nobody named. The marker is proved twice: once to answer, and again under the
// launcher's own .crw-lock immediately before the unlink - the lock the Python placement
// (completion.place_launcher) takes for this path - so a file replaced during the run is not
// deleted as though it were still ours. Without apply it answers what it would do and removes
// nothing.
//
// It never touches the settings. Removing the fallback stops nothing by itself: the declared
// Stop hook and crw-completion-hook.json are separate surfaces, so the answer reports what the
// settings path held afterwards rather than a claim about what still runs.
func RemoveLauncher(codexHome string, apply bool) Object {
	path := filepath.Join(codexHome, LauncherName)
	settings := filepath.Join(codexHome, SettingsName)
	answer := func(outcome, detail string, wrote bool) Object {
		_, err := os.Stat(settings)
		return Object{field("step", "stable launcher"), field("launcher", path), field("outcome", outcome),
			field("detail", detail), field("applied", apply && wrote), field("wrote", wrote),
			field("settingsPath", settings), field("settingsPresent", err == nil)}
	}
	kind := launcherKind(path)
	if kind == "absent" {
		return answer(LauncherAbsent, "there is nothing at "+path, false)
	}
	if kind != "file" {
		return answer(LauncherRefused, "the launcher path holds a "+kind+", so it is not this command's to remove and it is not followed", false)
	}
	found, err := readRegular(path)
	if err != nil {
		return answer(LauncherRefused, "the launcher at "+path+" could not be read ("+store.PythonOSError(err)+"), so it is left alone", false)
	}
	if !bytes.Contains(found, []byte(LauncherMarker)) {
		return answer(LauncherRefused, "the file at "+path+" does not carry "+LauncherMarker+", so it is not this command's to remove", false)
	}
	if !apply {
		return answer(LauncherWould, "would remove "+path, false)
	}
	lock, err := lockLauncher(path, 0)
	if err != nil {
		var busy *record.Busy
		if errors.As(err, &busy) {
			return answer(LauncherBusy, busy.Message, false)
		}
		return answer(LauncherRefused, "the launcher's lock could not be taken ("+store.PythonOSError(err)+"), so nothing was removed", false)
	}
	defer lock.Release()
	if launcherKind(path) != "file" {
		return answer(LauncherRefused, "the launcher path changed after it was read, so nothing was removed", false)
	}
	again, err := readRegular(path)
	if err != nil || !bytes.Contains(again, []byte(LauncherMarker)) {
		return answer(LauncherRefused, "the file at "+path+" was replaced while this ran and no longer carries the marker, so it was left", false)
	}
	if err := os.Remove(path); err != nil {
		return answer(LauncherRefused, "the launcher could not be removed ("+store.PythonOSError(err)+")", false)
	}
	return answer(LauncherRemoved, "removed "+path, true)
}

// lockLauncher takes the launcher's .crw-lock; a test replaces it to act inside the window.
var lockLauncher = record.Lock

// launcherKind is completion.launcher_kind: what is at path, without following anything.
func launcherKind(path string) string {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "absent"
	case err != nil:
		return "unreadable (" + store.PythonOSError(err) + ")"
	case info.Mode()&os.ModeSymlink != 0:
		return "symlink"
	case info.IsDir():
		return "directory"
	case info.Mode().IsRegular():
		return "file"
	}
	return "other"
}

// readRegular reads path only while it is a regular file: a link swapped in after the look is not
// followed and a pipe is not waited on.
func readRegular(path string) ([]byte, error) {
	handle, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EINVAL}
	}
	return io.ReadAll(handle)
}
