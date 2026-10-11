package testsupport

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The content snapshot's walk limits (CRW-1182). A link followed below a watched directory is walked within them: a walk past
// the depth limit, the entry budget or the byte budget is an error, so Verify never takes an incomplete snapshot as complete.
// The byte budget covers the regular files a walk reads (a file behind a link counts once per link that reaches it).
const (
	snapshotMaxDepth   = 32
	snapshotMaxEntries = 4096
	snapshotMaxBytes   = 64 << 20
)

// AccountHomes is the set of account homes one test hands to the code under test: HOME, CODEX_HOME and
// CRW_HOME, each a temporary directory the test owns. A test that must show a run reached no account home
// observes these directories and no others. The real ~/.codex and ~/.crw are shared with every other process
// of the host (a live Codex session opens its sqlite files there at any moment), so a listing of them
// cannot tell this run's writes from theirs; the test's own directories can only change by this process.
type AccountHomes struct {
	Home, Codex, CRW string
	before           string
	beforeErr        error
	watchHome        bool
	content          bool
}

// SandboxAccountHomes points HOME, CODEX_HOME and CRW_HOME at fresh temporary directories for the test (t
// restores them) and has the test fail, at its end, if anything was created or removed at the top level of
// the directories the code could take for an account home: HOME/.codex, HOME/.crw, CODEX_HOME and CRW_HOME.
// A test that puts fixtures there calls Rebase once they are in place.
func SandboxAccountHomes(t *testing.T) *AccountHomes {
	t.Helper()
	h := &AccountHomes{Home: t.TempDir(), Codex: t.TempDir(), CRW: t.TempDir()}
	t.Setenv("HOME", h.Home)
	t.Setenv("CODEX_HOME", h.Codex)
	t.Setenv("CRW_HOME", h.CRW)
	h.Rebase()
	t.Cleanup(func() {
		h.Verify(func(msg string) { t.Error(msg) })
	})
	return h
}

// WatchAccountHomes observes homes a test hands to a child process through its environment (a real shell
// started with HOME, CODEX_HOME and CRW_HOME set to these directories), which SandboxAccountHomes does not
// reach because the child never sees the process's own variables. It sets no variable. The test fails, at its
// end, if anything was created or removed at the top level of home, home/.codex, home/.crw, codex and crw; the
// top level of home is watched too, since the child's HOME is the directory its own startup would write to.
// The directories exist when it is called; a test that puts fixtures there calls Rebase once they are in place.
// An empty codex or crw is not watched: a command that writes one of its homes on purpose (a config rewrite
// with its backup) has the test check that home's contents itself.
func WatchAccountHomes(t *testing.T, home, codex, crw string) *AccountHomes {
	t.Helper()
	h := &AccountHomes{Home: home, Codex: codex, CRW: crw, watchHome: true}
	h.Rebase()
	t.Cleanup(func() {
		h.Verify(func(msg string) { t.Error(msg) })
	})
	return h
}

// Rebase takes the current state as the one later changes are measured against: the name-only listing, or the
// content snapshot once Snapshot has been called.
func (h *AccountHomes) Rebase() { h.before, h.beforeErr = h.state() }

// Snapshot switches the homes to the content snapshot and takes it as the baseline. A home that holds fixtures
// calls it once they are in place: an in-place edit of a fixture keeps its name, so the name-only listing does
// not see it. The snapshot walks each directory the code could take for an account home, recursively, and records
// every entry's relative path, mode and, for a regular file, its sha256 digest or, for a symlink, its target. A watched
// directory that is itself a symlink is recorded with its target, and every watched directory is walked at the
// physical directory its spelling resolves to (a link followed by "..", a link at the root).
// HOME itself keeps its top-level names only: a home that a command rewrites on purpose may sit below it (the retrust
// fixture keeps CODEX_HOME at HOME/codex), and a recursion into HOME would read that intended rewrite as a stray write.
func (h *AccountHomes) Snapshot() {
	h.content = true
	h.Rebase()
}

// Listing is the top-level names below each directory the code could take for an account home, sorted, with
// an absent directory marked.
func (h *AccountHomes) Listing() string {
	parts := []string{}
	for _, dir := range h.dirs() {
		parts = append(parts, h.label(dir)+": "+topLevelNames(dir))
	}
	return strings.Join(parts, " | ")
}

// topLevelNames is the sorted names directly below dir, or "absent".
func topLevelNames(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "absent"
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return strings.Join(names, ",")
}

// Contents is the recursive snapshot of each watched directory, one line per path, with an absent directory marked. The
// recursion covers every directory the code could take for an account home except HOME, which contributes its top-level
// names as Listing lists them. A link below a watched directory is followed to the directory or file it names and walked
// within the limits (CRW-1182): a link is recorded with its target (and a followed target's mode), a dangling link with "(dangling)", a link back to a
// directory above it with "(cycle)", and a link to a directory already walked with "(alias of <first link>)". An entry the
// walk cannot read is marked in its line, the walk goes on past it, and the error names every such entry: an unreadable
// entry is no state, and the same error before and after a run would hide a change to it. A walk past a limit (the depth,
// entry or byte budget) is an error too, and it ends that directory's walk.
func (h *AccountHomes) Contents() (string, error) {
	parts := []string{}
	var errs []error
	for _, dir := range h.dirs() {
		label := h.label(dir)
		info, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			parts = append(parts, label+": absent")
			continue
		}
		if err != nil {
			parts = append(parts, label+" unreadable")
			errs = append(errs, fmt.Errorf("%s: %w", label, err))
			continue
		}
		if dir == h.Home {
			entries, err := os.ReadDir(dir)
			if err != nil {
				parts = append(parts, label+" unreadable")
				errs = append(errs, fmt.Errorf("%s: %w", label, err))
				continue
			}
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			parts = append(parts, label+": "+strings.Join(names, ","))
			continue
		}
		w := walkHomeDir(label, dir, info)
		parts = append(parts, strings.Join(w.lines, "\n"))
		errs = append(errs, w.errs...)
	}
	return strings.Join(parts, "\n"), errors.Join(errs...)
}

// walkHomeDir walks one watched directory. A watched directory that is itself a link keeps its own line (mode and target)
// and a dangling one ends there; the directory it names is walked under the same label. The walk starts at the physical
// directory the spelling resolves to, because a link followed by ".." names the directory the kernel resolves, and a root
// left as spelled would read a lexical counterpart, or none, instead of the home itself. The root is made absolute before it
// is resolved: a relative root would resolve to a relative path while a link with an absolute target resolves to an absolute
// one, and the two spellings of one directory would be two keys of the cycle and alias tests.
func walkHomeDir(label, dir string, info fs.FileInfo) *snapshotWalk {
	w := &snapshotWalk{walked: map[string]string{}}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(dir)
		if err != nil {
			w.fail(label, err)
			return w
		}
		line := label + " " + info.Mode().String() + " -> " + target
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			w.add(line + " (dangling)")
			return w
		}
		w.add(line)
	}
	abs := dir
	if !filepath.IsAbs(dir) {
		// Prefixed, not filepath.Abs: Abs cleans the path and would fold a ".." after a link lexically (see above).
		wd, err := os.Getwd()
		if err != nil {
			w.fail(label, err)
			return w
		}
		abs = wd + string(filepath.Separator) + dir
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		w.fail(label, err)
		return w
	}
	w.entry(label, root, nil, 0)
	return w
}

// snapshotWalk is the walk of one watched directory: its lines, its errors, the real directories walked so far (with the
// first link that reached each, for the alias mark) and the bytes read.
type snapshotWalk struct {
	lines   []string
	errs    []error
	walked  map[string]string
	bytes   int64
	stopped bool
}

// snapshotListed is a test seam: it is told how many entries one directory read returned, so a test can show that the read
// of a directory is bounded by the remaining entry budget. Production leaves it nil.
var snapshotListed func(path string, n int)

func (w *snapshotWalk) add(line string) { w.lines = append(w.lines, line) }

// fail marks an entry the walk could not read and records the error.
func (w *snapshotWalk) fail(name string, err error) {
	w.lines = append(w.lines, name+" unreadable")
	w.errs = append(w.errs, fmt.Errorf("%s: %w", name, err))
}

// stop ends the walk with an error that names the entry at which a limit was reached.
func (w *snapshotWalk) stop(name, what string) {
	if !w.stopped {
		w.stopped = true
		w.errs = append(w.errs, fmt.Errorf("%s: %s, the snapshot is incomplete", name, what))
	}
}

// entry records the path named name at depth directories below the root. path is absolute and physical: names are joined
// below a directory the walk resolved, and a link is followed by os.Stat, never by joining its target. ancestors are the real
// directories that contain path on this walk, the cycle test of a link.
func (w *snapshotWalk) entry(name, path string, ancestors []string, depth int) {
	if w.stopped {
		return
	}
	if len(w.lines) >= snapshotMaxEntries {
		w.stop(name, fmt.Sprintf("more than %d entries", snapshotMaxEntries))
		return
	}
	info, err := os.Lstat(path)
	if err != nil {
		w.fail(name, err)
		return
	}
	line := name + " " + info.Mode().String()
	switch {
	case info.Mode().IsRegular():
		if sum, ok := w.digest(line, path, info.Size()); ok {
			w.add(line + " sha256:" + sum)
		}
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			w.fail(line, err)
			return
		}
		w.link(name, line+" -> "+target, path, ancestors, depth)
	case info.IsDir():
		if first, seen := w.walked[path]; seen {
			// Only a link walked this directory before (a directory is reached through its own parent once): its contents
			// are already in the snapshot under that link's name.
			w.add(line + " (alias of " + first + ")")
			return
		}
		w.add(line)
		w.walked[path] = name
		w.children(name, path, append(slices.Clip(ancestors), path), depth)
	default:
		w.add(line)
	}
}

// link records a link found below a watched directory and, when it names a directory, walks that directory. A cycle is a
// directory the link's own ancestors already contain; an alias is a directory walked earlier, through another link or as a
// plain directory. A link to a regular file is read through the link; a link to anything else (a FIFO, a device, a socket) is
// recorded with the target's mode and never opened, since opening it could block or read without end. A followed target's
// mode is part of its link's line, "(target <mode>)", since the target can lie outside every watched directory and no other
// line records it.
func (w *snapshotWalk) link(name, line, path string, ancestors []string, depth int) {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		w.add(line + " (dangling)")
		return
	}
	if err != nil {
		w.fail(line, err)
		return
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		w.fail(line, err)
		return
	}
	if !st.IsDir() {
		if !st.Mode().IsRegular() {
			w.add(line + " (target " + st.Mode().String() + ", not read)")
			return
		}
		line += " (target " + st.Mode().String() + ")"
		if sum, ok := w.digest(line, real, st.Size()); ok {
			w.add(line + " sha256:" + sum)
		}
		return
	}
	if slices.Contains(ancestors, real) {
		w.add(line + " (cycle)")
		return
	}
	if first, seen := w.walked[real]; seen {
		w.add(line + " (alias of " + first + ")")
		return
	}
	w.add(line + " (target " + st.Mode().String() + ")")
	w.walked[real] = name
	w.children(name, real, append(slices.Clip(ancestors), real), depth)
}

// children records the entries of the directory path, which sits depth directories below the root; a linked directory counts
// as one directory level like a plain one.
func (w *snapshotWalk) children(name, path string, ancestors []string, depth int) {
	if depth > snapshotMaxDepth {
		w.stop(name, fmt.Sprintf("past the depth limit of %d directories", snapshotMaxDepth))
		return
	}
	list, over, err := readDirBounded(path, snapshotMaxEntries-len(w.lines))
	if snapshotListed != nil {
		snapshotListed(path, len(list))
	}
	if err != nil {
		w.fail(name, err)
		return
	}
	if over {
		w.stop(name, fmt.Sprintf("more than %d entries", snapshotMaxEntries))
		return
	}
	for _, e := range list {
		if w.stopped {
			return
		}
		w.entry(name+"/"+e.Name(), filepath.Join(path, e.Name()), ancestors, depth+1)
	}
}

// readDirBounded lists the directory path in name order, reading at most room entries and one more as the overflow
// sentinel, so a directory with a great many children costs the budget and not its whole listing. Each child records at least
// one line, so more children than room is past the entry budget whatever they are; over then reports it and the list is
// not walked.
func readDirBounded(path string, room int) (list []os.DirEntry, over bool, err error) {
	if room < 0 {
		return nil, true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	for len(list) <= room {
		batch, e := f.ReadDir(room + 1 - len(list))
		list = append(list, batch...)
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return nil, false, e
		}
	}
	if len(list) > room {
		return list, true, nil
	}
	slices.SortFunc(list, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return list, false, nil
}

// digest is the sha256 of the regular file at path, read within the byte budget.
func (w *snapshotWalk) digest(line, path string, size int64) (string, bool) {
	if w.bytes+size > snapshotMaxBytes {
		w.stop(line, fmt.Sprintf("more than %d bytes read", snapshotMaxBytes))
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		w.fail(line, err)
		return "", false
	}
	defer f.Close()
	// The read is capped at the remaining budget, so a file that grew since its size was taken cannot exceed it.
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, snapshotMaxBytes-w.bytes+1))
	w.bytes += n
	if w.bytes > snapshotMaxBytes {
		w.stop(line, fmt.Sprintf("more than %d bytes read", snapshotMaxBytes))
		return "", false
	}
	if err != nil {
		w.fail(line, err)
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// Verify reports through fail when the state differs from the one Rebase took, or when the content snapshot could
// not read an entry, before the run or after it.
func (h *AccountHomes) Verify(fail func(string)) {
	after, afterErr := h.state()
	switch {
	case h.beforeErr != nil || afterErr != nil:
		fail(fmt.Sprintf("the content snapshot cannot read an account home it was given:\nbefore %v\nafter  %v\nbefore %s\nafter  %s",
			h.beforeErr, afterErr, h.before, after))
	case after != h.before:
		fail(fmt.Sprintf("the run changed an account home it was given:\nbefore %s\nafter  %s", h.before, after))
	}
}

// state is what Rebase and Verify compare: the content snapshot of a home that holds fixtures, else the listing.
func (h *AccountHomes) state() (string, error) {
	if h.content {
		return h.Contents()
	}
	return h.Listing(), nil
}

// dirs are the directories the code could take for an account home, HOME first when the watch covers it; an
// unnamed directory is left out.
func (h *AccountHomes) dirs() []string {
	dirs := []string{filepath.Join(h.Home, ".codex"), filepath.Join(h.Home, ".crw"), h.Codex, h.CRW}
	if h.watchHome {
		dirs = append([]string{h.Home}, dirs...)
	}
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if dir != "" {
			out = append(out, dir)
		}
	}
	return out
}

// label names a watched directory in the listing: HOME for the home, the path under it for a directory below it,
// and the base name for one elsewhere.
func (h *AccountHomes) label(dir string) string {
	if dir == h.Home {
		return "HOME"
	}
	if rel := strings.TrimPrefix(dir, h.Home); rel != dir {
		return rel
	}
	return filepath.Base(dir)
}
