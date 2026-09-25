package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
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
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		expanded = cwd + string(filepath.Separator) + expanded
	}
	return resolvePath(expanded)
}

func resolvePath(path string) (string, error) { return resolvePathDepth(path, 0) }
func resolvePathDepth(path string, depth int) (string, error) {
	if depth > 40 {
		return "", fmt.Errorf("too many symlinks resolving %q", path)
	}
	absolute := path
	if !filepath.IsAbs(path) {
		cwd, err := os.Getwd()
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

// pathlibSpelling matches str(Path(value)): collapse empty and dot components,
// but leave parent components for the OS to traverse after symlink resolution.
func pathlibSpelling(value string) string {
	absolute := strings.HasPrefix(value, "/")
	parts := make([]string, 0)
	for _, part := range strings.Split(value, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	result := strings.Join(parts, "/")
	if absolute {
		return "/" + result
	}
	if result == "" {
		return "."
	}
	return result
}
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
	if !filepath.IsAbs(expanded) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		expanded = cwd + "/" + expanded
	}
	return pathlibSpelling(expanded), nil
}
func expandUser(path string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}
	name, suffix, _ := strings.Cut(path[1:], "/")
	home := homeDir()
	if name != "" {
		user, err := user.Lookup(name)
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory for %q: %w", name, err)
		}
		home = user.HomeDir
	}
	if suffix == "" {
		return home, nil
	}
	return home + "/" + suffix, nil
}
func DiscoverStateDir(socket string) (StateSelection, error) {
	base := os.Getenv("XDG_STATE_HOME")
	source := "xdg"
	detail := "XDG_STATE_HOME=" + base
	if base == "" {
		base = filepath.Join(homeDir(), ".local", "state")
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
	if exists(chosen.DBPath()) {
		return chosen, nil
	}
	if legacy != canonical {
		previous := filepath.Join(root, legacy)
		if exists(filepath.Join(previous, "relay.sqlite3")) {
			chosen.Path = previous
			chosen.SocketScope = legacy
			chosen.Detail += "; kept the directory this socket was already using"
			return chosen, nil
		}
	}
	dirs, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return chosen, nil
	}
	if err != nil {
		return chosen, fmt.Errorf("scan state directories: %w", err)
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
func exists(path string) bool { _, err := os.Stat(path); return err == nil }
