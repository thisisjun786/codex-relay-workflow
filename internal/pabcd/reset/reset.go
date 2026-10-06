// Package reset ports CXC v0.2.40 reset.ts under the CRW state-directory name.
// Unlike the oracle, directory links cannot widen a reset's selected scope.
package reset

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// resetPin opens a previously observed non-link directory and checks its
// identity, detecting replacement between Lstat and OpenRoot. Operations then
// use that descriptor, rather than traversing its possibly replaced name.
func resetPin(parent *os.Root, name string, observed os.FileInfo) (*os.Root, error) {
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
	return root, nil
}

// resetRmIfExists is reset.ts rmIfExists. existsSync follows a link, so a link
// whose target exists is removed (the link itself, never its target) and a
// dangling one is absent and stays; a non-link is removed with its contents.
func resetRmIfExists(root *os.Root, name, display string, result *ResetResult) error {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		result.Absent = append(result.Absent, display)
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		if err := root.RemoveAll(name); err != nil {
			return err
		}
		result.Removed = append(result.Removed, display)
		return nil
	}
	exists, err := resetLinkTargetExists(root, name)
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
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	result.Removed = append(result.Removed, display)
	return nil
}

// resetLinkWalkLimit is how many links the judgement follows before it hands the target to the OS
// path. The issue fixes it at 40, the ceiling the kernel uses for a whole resolution.
const resetLinkWalkLimit = 40

// resetLinkTargetExists is existsSync for the link name in a pinned root.
//
// os.Root resolves a target that stays inside the root through the descriptor itself, except one
// whose final component is "." or "..": resolving that opens the target directory (O_DIRECTORY,
// read only), which a reset must not do. The walk here reads the link target and judges its path
// components with Lstat and Readlink only, so a target that stays inside the root is decided
// without opening it — including a chain of links that ends in a dot component. A target the walk
// cannot keep inside the root (an absolute target, ".." above it, more hops than resetLinkWalkLimit,
// a component it cannot read) and a link whose Readlink fails keep the older judgement: the
// descriptor first, then the OS on the root's own path, accepted only while that path still names
// the pinned directory before and after the stat; otherwise the verdict could describe another
// directory than the one the removal acts on, so the judgement refuses instead of guessing.
// Callers pass a bare leaf name.
func resetLinkTargetExists(root *os.Root, name string) (bool, error) {
	return resetLinkTargetExistsWith(root, name, root.Stat)
}

// resetLinkTargetExistsWith is resetLinkTargetExists with the root stat passed in, so a test can
// watch whether a link's target is opened through the pinned descriptor. statRoot is root.Stat.
func resetLinkTargetExistsWith(root *os.Root, name string, statRoot func(string) (os.FileInfo, error)) (bool, error) {
	// A target that stays inside the root and ends in "." or ".." is decided by the walk alone:
	// statRoot would open the target directory for it (O_DIRECTORY, read only), which CRW-554 forbids.
	if target, readErr := root.Readlink(name); readErr == nil {
		if exists, dotEnding, inside := resetLinkWalkTarget(root, target); inside && dotEnding {
			return exists, nil
		}
	}
	// Everything else keeps today's flow: the descriptor first, which stats the final component
	// without opening it, then the OS on the root's own path. A target that stays inside the root
	// without ending dot, a target that leaves it, and a link that vanished before this Readlink all
	// reach the descriptor here.
	_, err := statRoot(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return resetLinkWalkOnTheRootPath(root, name)
}

// resetLinkWalkTarget judges a link's Readlink text against the pinned root by walking the target's
// path components with Lstat and Readlink only. It never opens the target itself: the final
// component is read with lstat, so a target whose last component is "." or ".." is decided where
// os.Root would open that directory (O_DIRECTORY, read only) to follow the link to it. A
// multi-component target still has its intermediate directories traversed by os.Root, the way
// every path resolution traverses them, including the kernel lookup the OS-path judgement makes.
//
// The walk reports whether the target exists, whether its final component is "." or ".." (the case
// os.Root cannot stat without opening the target directory), and whether it stayed inside the root
// at all; a target it cannot keep inside the root is the caller's to judge on the root's path.
//
// "." is skipped and ".." pops one component after the links before it were expanded; a symlink
// component is read and its target spliced into the components still to walk. A component that
// does not exist means the target does not exist; a component that is not a directory where one is
// needed means the same; the final component only has to exist.
//
// The hop count starts at one because the caller already read this link's target with Readlink:
// the kernel counts that link as the first of the 40 traversals it allows for the whole
// resolution, so a chain of 40 links inside the target makes 41 and must not resolve.
func resetLinkWalkTarget(root *os.Root, target string) (exists, dotEnding, inside bool) {
	// A target that does not resolve against the pinned directory keeps the OS-path judgement: an
	// absolute one, and on Windows a rooted-without-volume one (backslash keep backslash dot, which
	// filepath.IsAbs does not report) or a drive-relative one (C:keep backslash dot, whose VolumeName
	// is non-empty). On Unix the volume is always empty and a leading separator is already absolute,
	// so this adds nothing there.
	if filepath.IsAbs(target) || filepath.VolumeName(target) != "" || strings.HasPrefix(target, string(filepath.Separator)) {
		return false, false, false
	}
	sep := string(filepath.Separator)
	remaining := strings.Split(target, sep)
	var walked []string
	hops := 1
	for len(remaining) > 0 {
		dotEnding = resetLinkWalkDotEnding(remaining)
		component := remaining[0]
		remaining = remaining[1:]
		switch component {
		case "":
			continue
		case ".", "..":
			if component == ".." && len(walked) == 0 {
				return false, dotEnding, false // above the root
			}
			if len(remaining) == 0 && !resetLinkWalkSearchable(root, walked) {
				// The kernel resolves a final "." or ".." by traversing into the directory the walk
				// reached, which needs search permission on it; the OS stat the oracle uses answers
				// EACCES without that permission and the link is kept. The walk reached the
				// directory with Lstat, which needs none, so ask the kernel here and answer absent
				// when it cannot search: the oracle answer and the data-preserving direction.
				return false, dotEnding, true
			}
			if component == ".." {
				walked = walked[:len(walked)-1]
			}
			continue
		}
		path := resetLinkWalkPath(walked, component)
		info, err := root.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, dotEnding, true
			}
			return false, dotEnding, false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			hops++
			if hops > resetLinkWalkLimit {
				return false, dotEnding, false
			}
			link, err := root.Readlink(path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return false, dotEnding, true
				}
				return false, dotEnding, false
			}
			if filepath.IsAbs(link) {
				return false, dotEnding, false
			}
			// A relative link target resolves against the directory holding the link, which is the
			// walked prefix: splice it in front of the components still to walk.
			remaining = append(strings.Split(link, sep), remaining...)
			continue
		}
		if len(remaining) == 0 {
			return true, dotEnding, true
		}
		if !info.IsDir() {
			return false, dotEnding, true // a component below a non-directory cannot resolve
		}
		walked = append(walked, component)
	}
	return true, dotEnding, true
}

// resetLinkWalkSearchable reports whether the kernel could look a name up inside the directory the
// walked components name, which is what resolving a final "." or ".." relative to that directory
// needs. It asks through the pinned root's own descriptor, where a name inside the directory
// answers ENOENT when the directory may be searched and EACCES when it may not, and it opens no
// directory for reading (CRW-554) and creates nothing. A directory the kernel cannot search, and a
// question it cannot answer at all, are both reported as not searchable.
func resetLinkWalkSearchable(root *os.Root, walked []string) bool {
	name := resetLinkWalkSearchProbe
	if len(walked) > 0 {
		name = strings.Join(walked, string(filepath.Separator)) + string(filepath.Separator) + name
	}
	dir, err := root.Open(".")
	if err != nil {
		return false
	}
	defer dir.Close()
	var st unix.Stat_t
	// The name is concatenated by hand, never filepath.Join, which would clean away the "." and
	// ".." components this judgement exists for.
	err = unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	return err == nil || errors.Is(err, unix.ENOENT)
}

// resetLinkWalkSearchProbe is the name the search probe looks up. It is never created: the point
// is only whether the kernel may look it up, which it answers with ENOENT or EACCES.
const resetLinkWalkSearchProbe = ".crw822searchprobe"

// resetLinkWalkDotEnding reports whether the last component still to walk is "." or "..", ignoring
// trailing separators. It is read at the top of each step so it survives both a spliced link target
// and an early exit.
func resetLinkWalkDotEnding(remaining []string) bool {
	for i := len(remaining) - 1; i >= 0; i-- {
		switch remaining[i] {
		case "":
			continue
		case ".", "..":
			return true
		}
		return false
	}
	return false
}

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
func resetLinkWalkOnTheRootPath(root *os.Root, name string) (bool, error) {
	pinned, err := root.Stat(".")
	if err != nil {
		return false, err
	}
	samePinned := func() error {
		current, err := os.Stat(root.Name())
		if err != nil || !os.SameFile(pinned, current) {
			return fmt.Errorf("reset directory changed while judging a link: %s", name)
		}
		return nil
	}
	if err := samePinned(); err != nil {
		return false, err
	}
	// Concatenated, not filepath.Join: Join cleans "..", which the kernel resolves physically.
	_, statErr := os.Stat(root.Name() + string(filepath.Separator) + name)
	if err := samePinned(); err != nil {
		return false, err
	}
	return statErr == nil, nil
}

func resetSessions(root *os.Root, base string, result *ResetResult) error {
	name := state.SessionsSubdir
	display := filepath.Join(base, name)
	info, err := root.Lstat(name)
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
	sessions, err := resetPin(root, name, info)
	if err != nil {
		return err
	}
	defer sessions.Close()
	file, err := sessions.Open(".")
	if err != nil {
		return err
	}
	entries, err := file.ReadDir(-1)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
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
