//go:build dev

// Package homeguard keeps the dev harnesses out of the account's real homes (CRW-1186). A harness
// builds its own homes under a case root or a scratch directory and never writes the account's:
// <account home>/.codex, <account home>/.crw and <account home>/.local/share/crw-runtime, where the
// installed Codex, the installed runtime and its hook switch live. The account home is the passwd
// home of the process's user, never $HOME: a run or a test that sets HOME (and CODEX_HOME) to the real
// home, or to a link that leads there, is exactly the case this package refuses, so $HOME cannot
// name it away.
//
// A writer calls Refuse (or RefuseAll) with its destination before it creates anything. A test
// package installs RefuseAccountHome in TestMain, so that a refusal the real account home caused
// fails the package even where the code under test swallowed the error.
package homeguard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// protectedBelow are the directories of the account home no harness writes below.
var protectedBelow = []string{".codex", ".crw", filepath.Join(".local", "share", "crw-runtime")}

// state is the lookup and the record of refusals, shared by the process.
var state struct {
	sync.Mutex
	lookup   func() (string, error) // nil: the passwd home of the process's user
	refused  []string               // paths refused while the real account home was the one looked up
	injected bool
}

// passwdHome is the home the passwd database gives the process's user. user.Current is not used: its
// fallback reads $HOME, which is what a test that points HOME at the real home sets.
func passwdHome() (string, error) {
	account, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return "", err
	}
	if account.HomeDir == "" {
		return "", errors.New("the passwd entry names no home")
	}
	return account.HomeDir, nil
}

// SetAccountHome makes lookup the account home for the process until the returned restore runs: a
// test that has to show the refusal without touching the real home names a fake one. An empty home
// means no account home. Refusals of a fake home are not recorded for the TestMain guard: the test
// that named it expects them.
func SetAccountHome(home string) (restore func()) {
	state.Lock()
	prevLookup, prevInjected := state.lookup, state.injected
	state.lookup = func() (string, error) {
		if home == "" {
			return "", errors.New("no account home")
		}
		return home, nil
	}
	state.injected = true
	state.Unlock()
	return func() {
		state.Lock()
		state.lookup, state.injected = prevLookup, prevInjected
		state.Unlock()
	}
}

// AccountHome is the account's home: the passwd home, or the one SetAccountHome named.
func AccountHome() (string, error) {
	state.Lock()
	lookup := state.lookup
	state.Unlock()
	if lookup != nil {
		return lookup()
	}
	return passwdHome()
}

// Protected is every directory below the account home the harnesses must not write, in the spelling
// the passwd database gives and, where the home is itself reached through a link, the physical one.
// It is empty when the account has no resolvable home.
func Protected() []string {
	home, err := AccountHome()
	if err != nil || !filepath.IsAbs(home) {
		return nil
	}
	homes := []string{filepath.Clean(home)}
	if resolved, err := filepath.EvalSymlinks(home); err == nil && !slices.Contains(homes, resolved) {
		homes = append(homes, resolved)
	}
	var out []string
	for _, h := range homes {
		for _, rel := range protectedBelow {
			out = append(out, filepath.Join(h, rel))
		}
	}
	return out
}

// Error is a destination Refuse turned away.
type Error struct {
	Path     string // the destination as given
	Resolved string // the destination with every link on the way followed
	Home     string // the protected directory it lies in
}

func (e *Error) Error() string {
	where := e.Path
	if e.Resolved != "" && e.Resolved != filepath.Clean(e.Path) {
		where = fmt.Sprintf("%s (which resolves to %s)", e.Path, e.Resolved)
	}
	return fmt.Sprintf("refusing to write %s: it lies in %s, a directory of the account's real home (the passwd home, not $HOME); a harness writes its own temporary homes only", where, e.Home)
}

// Refuse returns an *Error when path is, or lies below, a directory of the account home no harness
// may write (Protected), whether by its name or through the links on the way to it. A part of the path
// that does not exist yet is taken as written below the deepest part that does.
func Refuse(path string) error {
	protected := Protected()
	if len(protected) == 0 {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	resolved := resolve(abs)
	for _, candidate := range []string{abs, resolved} {
		for _, dir := range protected {
			if within(dir, candidate) {
				note(path)
				return &Error{Path: path, Resolved: resolved, Home: dir}
			}
		}
	}
	return nil
}

// RefuseAll is Refuse for each of paths; the first refusal is returned.
func RefuseAll(paths ...string) error {
	for _, p := range paths {
		if err := Refuse(p); err != nil {
			return err
		}
	}
	return nil
}

// note records a refusal of the real account home for the TestMain guard.
func note(path string) {
	state.Lock()
	defer state.Unlock()
	if !state.injected && !slices.Contains(state.refused, path) {
		state.refused = append(state.refused, path)
	}
}

// within is whether path is root or below it, by name.
func within(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolve is path with every link on the way followed. The part that does not exist yet is joined to
// the deepest part that does; a link that points nowhere is resolved as far as it goes.
func resolve(path string) string {
	var rest []string
	cur := filepath.Clean(path)
	for {
		if _, err := os.Lstat(cur); err == nil {
			if real, err := filepath.EvalSymlinks(cur); err == nil {
				slices.Reverse(rest)
				return filepath.Join(append([]string{real}, rest...)...)
			}
			if target, err := os.Readlink(cur); err == nil {
				// a dangling link: the file it would create lies where it points
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(cur), target)
				}
				slices.Reverse(rest)
				return resolve(filepath.Join(append([]string{target}, rest...)...))
			}
			return filepath.Clean(path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return filepath.Clean(path)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(path)
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// switchFile is the hook switch of the real Codex home, the file the CRW-1186 incident left.
func switchFile() string {
	home, err := AccountHome()
	if err != nil || !filepath.IsAbs(home) {
		return ""
	}
	return filepath.Join(home, ".codex", "crw", "switch.json")
}

// fingerprint is what the guard compares for the real switch file: whether it exists, and its
// content, size and modification time.
func fingerprint(path string) string {
	if path == "" {
		return "none"
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "absent"
	}
	raw, _ := os.ReadFile(path)
	return fmt.Sprintf("%s %d %s %q", fi.Mode(), fi.Size(), fi.ModTime().UTC().Format("2006-01-02T15:04:05.000000000Z"), raw)
}

// RefuseAccountHome is a testsupport.Setup (the root it is given is unused) for the TestMain of a
// package whose code writes homes. From the moment it runs, the cleanup it returns fails the package
// when a harness writer was turned away from the real account home (a test handed it HOME or
// CODEX_HOME of the real account), even if the code under test swallowed the error, and when the real
// hook switch, <passwd home>/.codex/crw/switch.json, changed while the tests ran (a child process
// the in-process refusal cannot see wrote it). A refusal of a fake home named with SetAccountHome is
// the test's own business and does not count.
func RefuseAccountHome(string) (func() error, error) {
	state.Lock()
	state.refused = nil
	state.Unlock()
	file := switchFile()
	before := fingerprint(file)
	return func() error {
		state.Lock()
		refused := slices.Clone(state.refused)
		state.refused = nil
		state.Unlock()
		var errs []error
		if len(refused) > 0 {
			errs = append(errs, fmt.Errorf("tests reached for %d destinations in the account's real home (give the code under test a temporary HOME and CODEX_HOME): %s", len(refused), strings.Join(refused, ", ")))
		}
		if after := fingerprint(file); after != before {
			errs = append(errs, fmt.Errorf("the real hook switch %s changed while the tests ran (before: %s; after: %s)", file, before, after))
		}
		return errors.Join(errs...)
	}, nil
}
