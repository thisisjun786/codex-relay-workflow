//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// workerHomes are the locations a worker's environment points at, all under the harness's own start-up root:
// the case homes and the XDG base directories. A worker never runs under the caller's homes (CRW-978 c4).
func workerHomes(root string) map[string]string {
	homes := RootEnv(root)
	return map[string]string{
		"HOME":            homes.Home,
		"CODEX_HOME":      homes.CodexHome,
		"CRW_HOME":        homes.CrwHome,
		"CODEXCLAW_HOME":  filepath.Join(root, "codexclaw-home"),
		"TMPDIR":          homes.TmpDir,
		"XDG_CONFIG_HOME": filepath.Join(root, "xdg", "config"),
		"XDG_DATA_HOME":   filepath.Join(root, "xdg", "data"),
		"XDG_CACHE_HOME":  filepath.Join(root, "xdg", "cache"),
		"XDG_STATE_HOME":  filepath.Join(root, "xdg", "state"),
	}
}

// workerRoot makes the start-up root and every directory a worker's environment names under it.
func workerRoot(root string) error {
	if err := PrepareRoot(root); err != nil {
		return err
	}
	for name, dir := range workerHomes(root) {
		if strings.HasPrefix(name, "XDG_") {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
	}
	return nil
}

// workerEnvironment is the environment a worker is launched with: the caller's variables, except that every
// home variable and every XDG_ variable is replaced by its location under the start-up root. The caller's
// values are dropped, not shadowed, so a worker cannot read them back (CRW-978 c4, CRW-971 item 1).
func workerEnvironment(env []string, root string) []string {
	homes := workerHomes(root)
	out := make([]string, 0, len(env)+len(homes))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := homes[name]; replaced || strings.HasPrefix(name, "XDG_") {
			continue
		}
		out = append(out, entry)
	}
	names := make([]string, 0, len(homes))
	for name := range homes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, name+"="+homes[name])
	}
	return out
}
