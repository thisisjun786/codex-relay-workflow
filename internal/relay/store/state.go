package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

type StateSelection struct {
	Path, Source, Detail, SocketScope string
	// DefaultSocket is the default App Server socket (DefaultSocket) that scoped a selection
	// discovery made without --socket, so the selected store can be checked against it; "" for
	// --state, CODEX_SESSION_RELAY_STATE, a given socket, and the legacy default directory. It is
	// never connected to and never stands in for --socket.
	DefaultSocket           string
	Ambiguous, Unidentified []string
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

func resolvePath(path string) (string, error) { return resolvePathDepth(path, 0) }

// resolveLoosely is Path.resolve() as ownership.mirror calls it (Realpath), the path as given
// when the working directory a relative one needs cannot be read.
func resolveLoosely(path string) string {
	resolved, err := Realpath(path)
	if err != nil {
		return path
	}
	return resolved
}

// Realpath is os.path.realpath(path) with strict=False, which Path(path).resolve() is: each
// component is examined from the left and a symbolic link is followed where it stands, before a
// later ".." is applied. A component that cannot be examined (EACCES, ENOENT, ENOTDIR,
// ENAMETOOLONG, an unreadable link) is kept as spelled and the walk goes on beneath it, and a link
// met again before it has resolved (a loop) is kept as spelled, as CPython 3.13 and 3.14 keep it.
// The only failures are the working directory's (os.getcwd) for a relative path and an embedded
// NUL, which Python's lstat refuses with ValueError.
func Realpath(path string) (string, error) {
	if strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("ValueError: embedded null byte")
	}
	// rest is Python's stack of unresolved parts, top last. A marker entry stands for the None
	// realpath pushes above a link's own path: popping it records what the link resolved to.
	type entry struct {
		name   string
		marker bool
	}
	fields := strings.Split(path, "/")
	rest := make([]entry, 0, len(fields))
	for i := len(fields) - 1; i >= 0; i-- {
		rest = append(rest, entry{name: fields[i]})
	}
	count := len(fields)
	resolved := "/"
	if !strings.HasPrefix(path, "/") {
		cwd, err := unix.Getwd()
		if err != nil {
			return "", err
		}
		resolved = cwd
	}
	// seen maps a link to what it resolved to; a link present but not yet done is being resolved.
	type link struct {
		resolved string
		done     bool
	}
	seen := map[string]link{}
	for count > 0 {
		top := rest[len(rest)-1]
		rest = rest[:len(rest)-1]
		if top.marker {
			owner := rest[len(rest)-1]
			rest = rest[:len(rest)-1]
			seen[owner.name] = link{resolved: resolved, done: true}
			continue
		}
		count--
		name := top.name
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			if cut := strings.LastIndex(resolved, "/"); cut > 0 {
				resolved = resolved[:cut]
			} else {
				resolved = "/"
			}
			continue
		}
		next := resolved + "/" + name
		if resolved == "/" {
			next = "/" + name
		}
		info, err := os.Lstat(next)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		if known, ok := seen[next]; ok {
			resolved = next
			if known.done {
				resolved = known.resolved
			}
			continue
		}
		target, err := os.Readlink(next)
		if err != nil {
			resolved = next
			continue
		}
		if strings.HasPrefix(target, "/") {
			resolved = "/"
		}
		seen[next] = link{}
		rest = append(rest, entry{name: next}, entry{marker: true})
		parts := strings.Split(target, "/")
		for i := len(parts) - 1; i >= 0; i-- {
			rest = append(rest, entry{name: parts[i]})
		}
		count += len(parts)
	}
	return resolved, nil
}

func resolvePathDepth(path string, depth int) (string, error) {
	if depth > 40 {
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
		if os.IsNotExist(err) {
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
		if err != nil {
			return "", err
		}
		remaining := strings.Join(parts[i+1:], string(filepath.Separator))
		if filepath.IsAbs(target) {
			return resolvePathDepth(target+string(filepath.Separator)+remaining, depth+1)
		}
		return resolvePathDepth(resolved+string(filepath.Separator)+target+string(filepath.Separator)+remaining, depth+1)
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
		path = ownership.JoinCwd(cwd, path)
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

// LegacyDefaultScope is the directory discovery chose before it scoped a selection made without
// --socket by the default App Server socket (docs/port/decisions.md section 73). A store already
// there keeps being used while the default socket's own directory holds none.
const LegacyDefaultScope = "default"

// DefaultSocket is the Codex App Server control socket the bridge defaults to
// (internal/bridge/mcp Defaults): <CODEX_HOME>/app-server-control/app-server-control.sock, with
// CODEX_HOME when it is set and non-empty, else Path.home()/.codex. Discovery scopes the state
// directory by it when no --socket is given; nothing connects to it.
func DefaultSocket() (string, error) {
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		home, err := homeDir()
		if err != nil {
			return "", err
		}
		codexHome = strings.TrimSuffix(home, "/") + "/.codex"
	}
	return PathlibChild(PathlibChild(codexHome, "app-server-control"), "app-server-control.sock"), nil
}

// SocketScope is the directory name discovery gives socket's own store: the digest of the
// socket's canonical path (CanonicalSocket).
func SocketScope(socket string) (string, error) {
	canonical, err := canonicalSocket(socket)
	if err != nil {
		return "", err
	}
	return socketHash(canonical), nil
}

// DiscoverStateDir is the state directory below XDG_STATE_HOME (or ~/.local/state) for socket.
// Without a socket the directory is scoped by DefaultSocket, the one the bridge and a relay
// service started on the default socket use, so a command given no --socket reads the store that
// service serves; it connects to nothing. Only when that directory holds no store, no other store
// records the default socket, and the legacy "default" directory holds one, is "default" kept.
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
	absoluteBase, err := absoluteExpanded(base)
	if err != nil {
		return StateSelection{}, err
	}
	defaulted := socket == ""
	if defaulted {
		if socket, err = DefaultSocket(); err != nil {
			return StateSelection{}, err
		}
		detail += "; scoped by the default Codex App Server socket " + socket
	}
	wanted, err := canonicalSocket(socket)
	if err != nil {
		return StateSelection{}, err
	}
	canonical := socketHash(wanted)
	expanded, err := expandUser(socket)
	if err != nil {
		return StateSelection{}, err
	}
	legacy := socketHash(pathlibSpelling(expanded))
	// Every directory is joined as pathlib joins, never through filepath.Join, which would fold the
	// two leading slashes pathlib keeps as a root of their own.
	root := PathlibChild(absoluteBase, "codex-session-relay")
	chosen := StateSelection{Path: PathlibChild(root, canonical), Source: source, Detail: detail, SocketScope: canonical}
	if defaulted {
		chosen.DefaultSocket = socket
	}
	found, err := discoveryExists(chosen.DBPath())
	if err != nil {
		return chosen, err
	}
	if found {
		return chosen, nil
	}
	if legacy != canonical {
		previous := PathlibChild(root, legacy)
		found, err := discoveryExists(previous + "/relay.sqlite3")
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
	// The legacy default directory is probed with the other two, before any sibling is listed.
	keptDefault := false
	if defaulted {
		if keptDefault, err = discoveryExists(PathlibChild(root, LegacyDefaultScope) + "/relay.sqlite3"); err != nil {
			return chosen, err
		}
	}
	// One walk answers stores_claiming_socket and stores_without_provenance: nothing between them
	// changes what either finds.
	for _, path := range siblingStoreDirs(root, canonical) {
		recorded := storeSocket(path + "/relay.sqlite3")
		if recorded == wanted {
			chosen.Ambiguous = append(chosen.Ambiguous, path)
		} else if recorded == "" {
			chosen.Unidentified = append(chosen.Unidentified, path)
		}
	}
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
	if keptDefault {
		// What a command given no --socket read before discovery scoped it by the default socket.
		chosen.Path = PathlibChild(root, LegacyDefaultScope)
		chosen.SocketScope = LegacyDefaultScope
		// The legacy directory was never scoped by any socket: it is not checked against one.
		chosen.DefaultSocket = ""
		chosen.Detail += "; kept the legacy default directory: it holds a store, the default socket's own directory holds none, and no other store records that socket"
		chosen.Unidentified = nil
		return chosen, nil
	}
	if len(chosen.Unidentified) > 0 {
		chosen.Detail += fmt.Sprintf("; %d stores here record no socket", len(chosen.Unidentified))
	}
	return chosen, nil
}

// PathlibChild is str(Path(parent) / name) for a name holding no slash: the parent as pathlib
// spells it, with two leading slashes kept as their own root, and "." dropped.
func PathlibChild(parent, name string) string {
	spelled := pathlibSpelling(parent)
	switch {
	case spelled == ".":
		return name
	case strings.HasSuffix(spelled, "/"):
		return spelled + name
	}
	return spelled + "/" + name
}

// PathlibParent is str(Path(path).parent): the last component dropped from the pathlib spelling,
// a root ("/" or "//") being its own parent and a single relative component having ".".
func PathlibParent(path string) string {
	spelled := pathlibSpelling(path)
	cut := strings.LastIndex(spelled, "/")
	switch {
	case spelled == "/" || spelled == "//":
		return spelled
	case cut < 0:
		return "."
	case cut == 0:
		return "/"
	case cut == 1 && strings.HasPrefix(spelled, "//"):
		return "//"
	}
	return spelled[:cut]
}

// pathlib.Path.exists suppresses absence/non-directory, but propagates access
// errors at the canonical/legacy DB probes before sibling listing begins. The error keeps its
// str(OSError) wording: the owner's guard reaches it through the selection, and the Stop hook
// that evaluates in process journals it as the row's fault.
func discoveryExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
		return false, nil
	}
	return false, errors.New(StoredOSError(err))
}
func exists(path string) bool { _, err := os.Stat(path); return err == nil }
