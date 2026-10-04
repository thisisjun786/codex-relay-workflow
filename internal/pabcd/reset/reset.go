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

func resetRmIfExists(root *os.Root, name, display string, result *ResetResult) error {
	// Stat preserves existsSync's dangling-link absence; escaping links fail
	// closed. RemoveAll removes final links without recursing through them.
	if _, err := root.Stat(name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			result.Absent = append(result.Absent, display)
			return nil
		}
		return err
	}
	if err := root.RemoveAll(name); err != nil {
		return err
	}
	result.Removed = append(result.Removed, display)
	return nil
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
