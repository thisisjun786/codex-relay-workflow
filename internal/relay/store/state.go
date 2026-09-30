package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

type StateSelection struct {
	Path, Source, Detail, SocketScope string
	Ambiguous, Unidentified           []string
}

func (s StateSelection) DBPath() string { return s.Path + "/relay.sqlite3" }
func canonicalSocket(path string) (string, error) {
	expanded, err := expandUser(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		cwd, err := unix.Getwd()
		if err != nil {
			return "", err
		}
		expanded = cwd + string(filepath.Separator) + expanded
	}
	return resolvePath(expanded)
}

func resolvePath(path string) (string, error) { return resolvePathDepth(path, 0, true) }

// resolveLoosely is Path.resolve() as ownership.mirror calls it (os.path.realpath with
// strict=False): a component that cannot be examined (EACCES, ENOTDIR, an unreadable link) is
// kept as spelled, where resolvePath fails.
func resolveLoosely(path string) string {
	resolved, err := resolvePathDepth(path, 0, false)
	if err != nil {
		return path
	}
	return resolved
}

func resolvePathDepth(path string, depth int, strict bool) (string, error) {
	if depth > 40 {
		if !strict {
			return pathlibSpelling(path), nil
		}
		return "", fmt.Errorf("too many symlinks resolving %q", path)
	}
	absolute := path
	if !filepath.IsAbs(path) {
		cwd, err := unix.Getwd()
		if err != nil {
			return "", err
		}
		absolute = cwd + string(filepath.Separator) + path
	}
	// Resolve one component at a time: symlinks must be followed before processing '..'.
	parts := strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator))
	resolved := string(filepath.Separator)
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			resolved = filepath.Dir(resolved)
			continue
		}
		candidate := filepath.Join(resolved, part)
		info, err := os.Lstat(candidate)
		if os.IsNotExist(err) || err != nil && !strict {
			resolved = candidate
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = candidate
			continue
		}
		target, err := os.Readlink(candidate)
		if err != nil && !strict {
			resolved = candidate
			continue
		}
		if err != nil {
			return "", err
		}
		remaining := strings.Join(parts[i+1:], string(filepath.Separator))
		if filepath.IsAbs(target) {
			return resolvePathDepth(target+string(filepath.Separator)+remaining, depth+1, strict)
		}
		return resolvePathDepth(resolved+string(filepath.Separator)+target+string(filepath.Separator)+remaining, depth+1, strict)
	}
	return resolved, nil
}

// pathlibSpelling is str(Path(value)): empty and dot components collapse, parent components stay
// for the OS to traverse after symlink resolution, and exactly two leading slashes stay a root of
// their own, as ownership.PathlibSpelling spells them.
func pathlibSpelling(value string) string { return ownership.PathlibSpelling(value) }
func socketHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}
func ResolveStateDir(explicit, socket string) (StateSelection, error) {
	if explicit != "" {
		path, err := absoluteExpanded(explicit)
		return StateSelection{Path: path, Source: "flag", Detail: "--state " + explicit}, err
	}
	if override := os.Getenv("CODEX_SESSION_RELAY_STATE"); override != "" {
		path, err := absoluteExpanded(override)
		return StateSelection{Path: path, Source: "env", Detail: "CODEX_SESSION_RELAY_STATE=" + override}, err
	}
	return DiscoverStateDir(socket)
}
func absoluteExpanded(path string) (string, error) {
	expanded, err := expandUser(path)
	if err != nil {
		return "", err
	}
	return absolute(expanded)
}

// absolute is str(Path(path).absolute()): the working directory as the kernel names it
// (os.getcwd, where os.Getwd prefers a $PWD that reaches it through a symbolic link) prefixed to a
// relative path, then the pathlib spelling. Nothing is resolved and no ".." is folded.
func absolute(path string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		cwd, err := unix.Getwd()
		if err != nil {
			return "", err
		}
		path = cwd + "/" + path
	}
	return pathlibSpelling(path), nil
}

// ErrNoHome is pathlib's RuntimeError("Could not determine home directory."): an unknown ~user,
// or a ~ when HOME is unset and this user has no passwd entry.
var ErrNoHome = ownership.ErrNoHome

// homeDir is Path.home() (ownership.UserHome): HOME when it is set at all, an empty HOME being
// the root, else the passwd entry.
func homeDir() (string, error) { return ownership.UserHome("") }

// expandUser is Path(path).expanduser() for a path whose first character is ~: the home
// ownership.UserHome names, joined to the rest as pathlib joins components.
func expandUser(path string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}
	name, suffix, _ := strings.Cut(path[1:], "/")
	home, err := ownership.UserHome(name)
	if err != nil {
		if name != "" {
			return "", fmt.Errorf("cannot determine home directory for %q: %w", name, err)
		}
		return "", fmt.Errorf("HOME is not set and this user's passwd entry could not be read: %w", err)
	}
	if suffix = strings.TrimLeft(suffix, "/"); suffix == "" {
		return home, nil
	}
	return strings.TrimSuffix(home, "/") + "/" + suffix, nil
}
func DiscoverStateDir(socket string) (StateSelection, error) {
	base := os.Getenv("XDG_STATE_HOME")
	source := "xdg"
	detail := "XDG_STATE_HOME=" + base
	if base == "" {
		home, err := homeDir()
		if err != nil {
			return StateSelection{}, err
		}
		base = pathlibSpelling(strings.TrimSuffix(home, "/") + "/.local/state")
		source = "home"
		detail = "default under " + base
	}
	base, err := absoluteExpanded(base)
	if err != nil {
		return StateSelection{}, err
	}
	canonical := "default"
	legacy := "default"
	if socket != "" {
		canonicalPath, err := canonicalSocket(socket)
		if err != nil {
			return StateSelection{}, err
		}
		canonical = socketHash(canonicalPath)
		expanded, err := expandUser(socket)
		if err != nil {
			return StateSelection{}, err
		}
		legacy = socketHash(pathlibSpelling(expanded))
	}
	root := base + "/codex-session-relay"
	chosen := StateSelection{Path: root + "/" + canonical, Source: source, Detail: detail, SocketScope: canonical}
	found, err := discoveryExists(chosen.DBPath())
	if err != nil {
		return chosen, err
	}
	if found {
		return chosen, nil
	}
	if legacy != canonical {
		previous := filepath.Join(root, legacy)
		found, err := discoveryExists(filepath.Join(previous, "relay.sqlite3"))
		if err != nil {
			return chosen, err
		}
		if found {
			chosen.Path = previous
			chosen.SocketScope = legacy
			chosen.Detail += "; kept the directory this socket was already using"
			return chosen, nil
		}
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		// Python stores_claiming_socket and stores_without_provenance treat
		// every listing OSError as no candidates. Resolution remains eager;
		// explicit --db-path does not bypass malformed override errors.
		return chosen, nil
	}
	wanted := ""
	if socket != "" {
		wanted, err = canonicalSocket(socket)
		if err != nil {
			return chosen, err
		}
	}
	for _, dir := range dirs {
		if !dir.IsDir() || dir.Name() == canonical {
			continue
		}
		path := filepath.Join(root, dir.Name())
		if !exists(filepath.Join(path, "relay.sqlite3")) {
			continue
		}
		recorded := storeSocket(filepath.Join(path, "relay.sqlite3"))
		if wanted != "" && recorded == wanted {
			chosen.Ambiguous = append(chosen.Ambiguous, path)
		} else if recorded == "" {
			chosen.Unidentified = append(chosen.Unidentified, path)
		}
	}
	sort.Strings(chosen.Ambiguous)
	sort.Strings(chosen.Unidentified)
	if len(chosen.Ambiguous) == 1 {
		chosen.Path = chosen.Ambiguous[0]
		chosen.SocketScope = filepath.Base(chosen.Path)
		chosen.Detail += "; adopted the store already recorded for this socket"
		chosen.Ambiguous = nil
		chosen.Unidentified = nil
		return chosen, nil
	}
	if len(chosen.Ambiguous) > 1 {
		chosen.Detail += fmt.Sprintf("; %d stores already record this socket", len(chosen.Ambiguous))
		chosen.Unidentified = nil
		return chosen, nil
	}
	chosen.Ambiguous = nil
	if len(chosen.Unidentified) > 0 {
		chosen.Detail += fmt.Sprintf("; %d stores here record no socket", len(chosen.Unidentified))
	}
	return chosen, nil
}

// pathlib.Path.exists suppresses absence/non-directory, but propagates access
// errors at the canonical/legacy DB probes before sibling listing begins.
func discoveryExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
		return false, nil
	}
	return false, errors.New(PythonOSError(err))
}
func exists(path string) bool { _, err := os.Stat(path); return err == nil }
