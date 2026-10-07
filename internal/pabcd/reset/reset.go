// Package reset ports CXC v0.2.40 reset.ts under the CRW state-directory name.
// Unlike the oracle, directory links cannot widen a reset's selected scope.
package reset

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"golang.org/x/sys/unix"
)

type ResetScope string

const (
	State     ResetScope = "state"
	Generated ResetScope = "generated"
	Goalplans ResetScope = "goalplans"
	All       ResetScope = "all"
)
const (
	resetInterviewSubdir = "interview"
	resetRecoverySubdir  = "affordance-recovery"
)

type ResetResult struct {
	Scope   ResetScope `json:"scope"`
	Removed []string   `json:"removed"`
	Absent  []string   `json:"absent"`
}

// ParseResetScope defaults to state. Unknown, duplicate or competing flags are
// refused instead of the oracle's destructive default/precedence behavior.
func ParseResetScope(args []string) (ResetScope, error) {
	if len(args) == 0 {
		return State, nil
	}
	if len(args) != 1 {
		return "", errors.New("choose exactly one reset scope")
	}
	switch args[0] {
	case "--state":
		return State, nil
	case "--generated":
		return Generated, nil
	case "--goalplans":
		return Goalplans, nil
	case "--all":
		return All, nil
	}
	return "", fmt.Errorf("unknown reset option: %s", args[0])
}

func resetCandidates(scope ResetScope) []string {
	switch scope {
	case State:
		return []string{state.SessionsSubdir, state.LedgerFile, state.InterviewsSubdir, resetRecoverySubdir}
	case Generated:
		return []string{resetInterviewSubdir}
	case Goalplans:
		return []string{goalplan.GoalplansSubdir}
	case All:
		return []string{"."}
	}
	return nil
}

// resetLinkWalkPin is a directory resetPin observed, identity-checked and opened once. The os.Root
// performs the removals on the leaf; the descriptor is the one taken when the directory was pinned,
// outside the judgement, and every step of the link judgement (lstat, readlink, the search check) is
// an fstatat(AT_SYMLINK_NOFOLLOW) or readlinkat relative to it. The judgement therefore opens no
// file and no directory, so a directory that may be searched but not read answers as the kernel
// does, and the removal still acts on the leaf through the descriptor rather than a name.
type resetLinkWalkPin struct {
	*os.Root
	dir *os.File
}

// resetLinkWalkPinOf adds the judgement descriptor to a directory root that is already open.
// resetPin calls it after the identity check, so a refused pin leaves nothing extra open.
func resetLinkWalkPinOf(root *os.Root) (*resetLinkWalkPin, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	return &resetLinkWalkPin{Root: root, dir: dir}, nil
}

// Close releases the descriptor and the root. The descriptor is closed first so a failure to close
// the root cannot leave it behind.
func (p *resetLinkWalkPin) Close() error {
	err := p.dir.Close()
	if closeErr := p.Root.Close(); err == nil {
		err = closeErr
	}
	return err
}

// resetPin opens a previously observed non-link directory, checks its
// identity, detecting replacement between Lstat and OpenRoot, and takes the
// descriptor the link judgement reads through. Operations then use that
// descriptor, rather than traversing its possibly replaced name.
func resetPin(parent *os.Root, name string, observed os.FileInfo) (*resetLinkWalkPin, error) {
	if observed.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("reset state path must not be a symlink: %s", name)
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	current, err := root.Stat(".")
	if err != nil || !os.SameFile(observed, current) {
		root.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("reset directory changed while opening: %s", name)
	}
	pinned, err := resetLinkWalkPinOf(root)
	if err != nil {
		root.Close()
		return nil, err
	}
	return pinned, nil
}

// resetRmIfExists is reset.ts rmIfExists. existsSync follows a link, so a link
// whose target exists is removed (the link itself, never its target) and a
// dangling one is absent and stays; a non-link is removed with its contents.
// A candidate the pinned descriptor cannot even lstat is absent too, which is the
// oracle's answer (existsSync is false for a name the kernel cannot resolve) and
// the direction that keeps the link, so one unreadable candidate no longer stops
// the reset before the ones after it.
func resetRmIfExists(pinned *resetLinkWalkPin, name, display string, result *ResetResult) error {
	info, err := pinned.Lstat(name)
	if err != nil {
		// A candidate the pinned descriptor cannot even lstat is absent: the kernel cannot resolve
		// the name either, which is the oracle's answer (existsSync is false), and it is the
		// direction that keeps the link, so one unreadable candidate no longer stops the reset
		// before the candidates after it.
		result.Absent = append(result.Absent, display)
		return nil
	}
	if info.Mode()&os.ModeSymlink == 0 {
		if err := pinned.RemoveAll(name); err != nil {
			return err
		}
		result.Removed = append(result.Removed, display)
		return nil
	}
	exists, err := resetLinkTargetExists(pinned, name)
	if err != nil {
		return err
	}
	if !exists {
		result.Absent = append(result.Absent, display)
		return nil
	}
	// Remove unlinks the final component without resolving it, so the target
	// is never deleted, inside the workspace or outside it; the verdict above
	// only stats it.
	if err := pinned.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	result.Removed = append(result.Removed, display)
	return nil
}

// resetLinkWalkLimit is how many links the judgement follows before it answers absent. It is the
// ceiling the kernel applies to the link itself, so the walk agrees with the stat the oracle uses:
// Linux allows 40 traversals for one resolution and XNU allows 32. A single ceiling would judge a
// chain of 33 to 40 links present on Darwin, where the kernel answers ELOOP and the oracle keeps
// the link. The count starts at one because the caller already read the candidate link's target.
func resetLinkWalkLimit() int {
	if runtime.GOOS == "darwin" {
		return 32
	}
	return 40
}

// resetLinkTargetExists is existsSync for the link name in a pinned directory.
//
// os.Root resolves a path through the descriptor it holds, but it opens every directory it
// traverses, so a directory that may be searched but not read (0111) fails there where the kernel
// succeeds. The judgement here reads the link target and judges its path components with
// fstatat(AT_SYMLINK_NOFOLLOW) and readlinkat relative to the descriptor resetPin took once, so a
// target that stays inside the root is decided without opening anything — including a chain of
// links that ends in a dot component. A target the walk cannot keep inside the root (an absolute
// target, ".." above it) keeps the older judgement: the descriptor first, then the OS on the root's
// own path, accepted only while that path still names the pinned directory before and after the
// stat; otherwise the verdict could describe another directory than the one the removal acts on, so
// the judgement refuses instead of guessing. Callers pass a bare leaf name.
func resetLinkTargetExists(pinned *resetLinkWalkPin, name string) (bool, error) {
	return resetLinkTargetExistsWith(pinned, name, pinned.Stat)
}

// resetLinkTargetExistsWith is resetLinkTargetExists with the root stat passed in, so a test can
// watch whether a link's target is opened through the pinned descriptor. statRoot is root.Stat.
func resetLinkTargetExistsWith(pinned *resetLinkWalkPin, name string, statRoot func(string) (os.FileInfo, error)) (bool, error) {
	// A target the walk can keep inside the root is decided by the walk alone, whether or not it
	// ends in a dot component: statRoot would open the target directory for a dot-ending target
	// (O_DIRECTORY, read only), which CRW-554 forbids, and it would answer EACCES for a directory
	// that may only be searched where the kernel answers with the target.
	target, readErr := resetLinkWalkReadlink(pinned.dir, name)
	if readErr == nil {
		if exists, inside := resetLinkWalkTarget(pinned.dir, name, target); inside {
			return exists, nil
		}
	}
	// The target could not be read through the pinned descriptor, and the walk therefore never showed
	// it leaving the root: an access error, a leaf that stopped being a link, or a link that vanished.
	// The kernel cannot resolve the link either, which is what the oracle's existsSync asks, so it is
	// absent and the link is kept. Handing it to the descriptor stat would answer present for a leaf a
	// writer swapped, and the root-path judgement would let an in-root access error refuse the whole
	// reset and would use the directory-opening path the judgement must not use.
	if readErr != nil {
		return false, nil
	}
	// Only a target the walk proved leaves the root keeps the older flow: the descriptor first, which
	// stats the final component without opening it, then the OS on the root's own path.
	_, err := statRoot(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return resetLinkWalkOnTheRootPath(pinned, name)
}

// resetLinkWalkTarget judges a link's readlink text against the pinned directory by walking the
// target's path components with fstatat(AT_SYMLINK_NOFOLLOW) and readlinkat relative to the
// descriptor resetPin took, so the walk opens nothing: a component that is not a link is read with
// fstatat, and the search permission a "." or ".." component needs is asked by looking up a name
// that is never created. A multi-component path is still resolved by the kernel component by
// component, the way the OS-path judgement resolves it too.
//
// The walk reports whether the target exists and whether it stayed inside the root at all; a target
// it cannot keep inside the root — an absolute one, a ".." above it, a spliced link target that is
// absolute — is the caller's to judge on the root's path.
//
// "." is skipped and ".." pops one component after the links before it were expanded; a symlink
// component is read and its target spliced into the components still to walk. Every other outcome
// is decided here and means the target does not exist: a component that is missing, that is not a
// directory where one is needed, or that cannot be searched, a link loop, more hops than
// resetLinkWalkLimit, and any other lstat or readlink error. That is the kernel's answer for the
// link (existsSync is false) and the direction that keeps the link.
//
// The walk resolves the target through one concatenated pathname, so a target whose prefix grows
// past the kernel's own single-pathname limit (PATH_MAX, 4096 on Linux and 1024 on XNU) answers
// ENAMETOOLONG from the walk's own fstatat. That is one of the errors above: the walk cannot decide
// the target, and the answer is absent, which keeps the link. The walk never hands such a target to
// the root-path judgement, which is reserved for a target that really leaves the root.
//
// The hop count starts at one because the caller already read this link's target with readlink:
// the kernel counts that link as the first traversal it allows for the whole resolution, so a chain
// of resetLinkWalkLimit() links inside the target makes one more than the ceiling and must not
// resolve.
func resetLinkWalkTarget(dir *os.File, name, target string) (exists, inside bool) {
	// A target that does not resolve against the pinned directory keeps the OS-path judgement: an
	// absolute one, and on Windows a rooted-without-volume one (backslash keep backslash dot, which
	// filepath.IsAbs does not report) or a drive-relative one (C:keep backslash dot, whose VolumeName
	// is non-empty). On Unix the volume is always empty and a leading separator is already absolute,
	// so this adds nothing there.
	if filepath.IsAbs(target) || filepath.VolumeName(target) != "" || strings.HasPrefix(target, string(filepath.Separator)) {
		return false, false
	}
	sep := string(filepath.Separator)
	remaining := strings.Split(target, sep)
	var walked []string
	hops := 1
	for len(remaining) > 0 {
		component := remaining[0]
		remaining = remaining[1:]
		switch component {
		case "":
			continue
		case ".", "..":
			// The kernel resolves a "." or ".." component by looking it up in the directory the walk
			// has reached, which needs search permission on that directory; fstatat needs none to get
			// there, so ask the kernel here and answer absent when it cannot search. This is the
			// question the last dot component has always asked, now asked at every dot component: the
			// kernel answers EACCES for a directory it cannot search and the link is kept.
			if !resetLinkWalkSearchable(dir, walked) {
				return false, true
			}
			if component == ".." {
				if len(walked) == 0 {
					return false, false // above the root
				}
				walked = walked[:len(walked)-1]
			}
			continue
		}
		path := resetLinkWalkPath(walked, component)
		st, err := resetLinkWalkLstat(dir, path)
		if err != nil {
			return false, true
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			hops++
			if hops > resetLinkWalkLimit() {
				return false, true
			}
			link, err := resetLinkWalkReadlink(dir, path)
			if err != nil {
				return false, true
			}
			if filepath.IsAbs(link) {
				return false, false
			}
			// A relative link target resolves against the directory holding the link, which is the
			// walked prefix: splice it in front of the components still to walk.
			remaining = append(strings.Split(link, sep), remaining...)
			continue
		}
		if len(remaining) == 0 {
			return true, true
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return false, true // a component below a non-directory cannot resolve
		}
		walked = append(walked, component)
	}
	return true, true
}

// resetLinkWalkLstat is the lstat of a path in the pinned directory. It is an fstatat relative to
// the descriptor resetPin took, so it opens nothing on the way, and it does not follow the final
// component.
func resetLinkWalkLstat(dir *os.File, path string) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstatat(int(dir.Fd()), path, &st, unix.AT_SYMLINK_NOFOLLOW)
	return st, err
}

// resetLinkWalkReadlink reads the link target of a path in the pinned directory with readlinkat
// relative to the descriptor. readlinkat fills the buffer and reports how much it wrote instead of
// failing when the target is longer, so a read whose buffer came back full is repeated with a
// larger one: stopping at the first buffer would judge a cut-off path.
func resetLinkWalkReadlink(dir *os.File, path string) (string, error) {
	for size := 128; ; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(int(dir.Fd()), path, buf)
		if err != nil {
			return "", err
		}
		if n < size {
			return string(buf[:n]), nil
		}
	}
}

// resetLinkWalkSearchable reports whether the kernel could look a name up inside the directory the
// walked components name, which is what resolving a "." or ".." component relative to that
// directory needs. It asks through the descriptor resetPin took, where a name inside the directory
// answers ENOENT when the directory may be searched and EACCES when it may not, and it opens no
// directory for reading (CRW-554) and creates nothing. A directory the kernel cannot search is
// reported as not searchable.
//
// ENAMETOOLONG is reported as searchable, not as unsearchable. The probe name is appended to the
// walked components, so the probe can push the concatenated pathname past the kernel's own limit
// where the walk's own pathname for the next component is still short enough to be asked. The error
// then says nothing about search permission, and reading it as "not searchable" would answer absent
// for a directory the kernel can search and keep a live link the oracle removes. The component lstat
// that follows decides such a target, and answers absent only when its own pathname is too long.
func resetLinkWalkSearchable(dir *os.File, walked []string) bool {
	// The name is concatenated by hand, never filepath.Join, which would clean away the "." and
	// ".." components this judgement exists for.
	_, err := resetLinkWalkLstat(dir, resetLinkWalkSearchableName(walked))
	if errors.Is(err, unix.ENAMETOOLONG) {
		return true
	}
	return err == nil || errors.Is(err, unix.ENOENT)
}

// resetLinkWalkSearchableName is the probe name the search question looks up inside the directory the
// walked components name. It is concatenated by hand, never filepath.Join, which would clean away the
// "." and ".." components this judgement exists for.
func resetLinkWalkSearchableName(walked []string) string {
	if len(walked) == 0 {
		return resetLinkWalkSearchProbe
	}
	return strings.Join(walked, string(filepath.Separator)) + string(filepath.Separator) + resetLinkWalkSearchProbe
}

// resetLinkWalkSearchProbe is the name the search probe looks up. It is never created: the point
// is only whether the kernel may look it up, which it answers with ENOENT or EACCES.
const resetLinkWalkSearchProbe = ".crw822searchprobe"

// resetLinkWalkPath is the path of component below the components already walked. It is built by
// concatenation, never filepath.Join, which would clean ".." the kernel resolves physically.
func resetLinkWalkPath(walked []string, component string) string {
	if len(walked) == 0 {
		return component
	}
	return strings.Join(walked, string(filepath.Separator)) + string(filepath.Separator) + component
}

// resetLinkWalkOnTheRootPath is the older judgement for a target the descriptor cannot keep inside
// the root: the OS stats the root's own path, accepted only while that path still names the pinned
// directory before and after the stat, so a renamed pinned directory refuses instead of guessing.
func resetLinkWalkOnTheRootPath(pin *resetLinkWalkPin, name string) (bool, error) {
	pinned, err := pin.Stat(".")
	if err != nil {
		return false, err
	}
	samePinned := func() error {
		current, err := os.Stat(pin.Name())
		if err != nil || !os.SameFile(pinned, current) {
			return fmt.Errorf("reset directory changed while judging a link: %s", name)
		}
		return nil
	}
	if err := samePinned(); err != nil {
		return false, err
	}
	// Concatenated, not filepath.Join: Join cleans "..", which the kernel resolves physically.
	_, statErr := os.Stat(pin.Name() + string(filepath.Separator) + name)
	if err := samePinned(); err != nil {
		return false, err
	}
	return statErr == nil, nil
}

func resetSessions(crw *resetLinkWalkPin, base string, result *ResetResult) error {
	name := state.SessionsSubdir
	display := filepath.Join(base, name)
	info, err := crw.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		result.Absent = append(result.Absent, display)
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("reset state path must not be a symlink: %s", name)
	}
	if !info.IsDir() {
		result.Absent = append(result.Absent, display)
		return nil
	}
	sessions, err := resetPin(crw.Root, name, info)
	if err != nil {
		return err
	}
	defer sessions.Close()
	// The listing reads through the descriptor the pin already took, so pinning the sessions
	// directory opens nothing beyond the pin itself.
	entries, err := sessions.dir.ReadDir(-1)
	if err != nil {
		return err
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			if err := resetRmIfExists(sessions, entry.Name(), filepath.Join(display, entry.Name()), result); err != nil {
				return err
			}
		}
	}
	return nil
}

// RunReset only operates on workspace-local .crw state. State and sessions are
// pinned separately: a sessions -> goalplans alias cannot delete a saved plan.
// All removes the single .crw leaf, including a symlink, without following it.
func RunReset(cwd string, scope ResetScope) (ResetResult, error) {
	result := ResetResult{Scope: scope, Removed: []string{}, Absent: []string{}}
	candidates := resetCandidates(scope)
	if candidates == nil {
		return result, fmt.Errorf("invalid reset scope: %s", scope)
	}
	base := filepath.Join(cwd, crwdir.DirName)
	absent := func() {
		for _, name := range candidates {
			result.Absent = append(result.Absent, filepath.Join(base, name))
		}
	}
	workspace, err := os.OpenRoot(cwd)
	if errors.Is(err, os.ErrNotExist) {
		absent()
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer workspace.Close()
	if scope == All {
		// Lstat keeps an outside directory link removable without following it.
		if _, err := workspace.Lstat(crwdir.DirName); errors.Is(err, os.ErrNotExist) {
			absent()
			return result, nil
		} else if err != nil {
			return result, err
		}
		if err := workspace.RemoveAll(crwdir.DirName); err != nil {
			return result, err
		}
		result.Removed = append(result.Removed, base)
		return result, nil
	}
	info, err := workspace.Lstat(crwdir.DirName)
	if errors.Is(err, os.ErrNotExist) {
		absent()
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return result, fmt.Errorf("reset state path must not be a symlink: %s", crwdir.DirName)
	}
	if !info.IsDir() {
		absent()
		return result, nil
	}
	root, err := resetPin(workspace, crwdir.DirName, info)
	if err != nil {
		return result, err
	}
	defer root.Close()
	if scope == State {
		if err := resetSessions(root, base, &result); err != nil {
			return result, err
		}
		candidates = candidates[1:]
	}
	for _, name := range candidates {
		if err := resetRmIfExists(root, name, filepath.Join(base, name), &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

// RenderReset is reset.ts:95-99; the adapter adds one trailing newline.
func RenderReset(result ResetResult) string {
	head := fmt.Sprintf("reset --%s: removed %d path(s)", result.Scope, len(result.Removed))
	if len(result.Removed) == 0 {
		return head + " (nothing to remove)"
	}
	return head + "\n  - " + strings.Join(result.Removed, "\n  - ")
}
