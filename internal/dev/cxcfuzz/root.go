//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// RootEnv is the environment one case runs under: the case's own root and the homes under it, so
// a target that reads HOME, CODEX_HOME, CRW_HOME or TMPDIR reaches the case's tree and never a
// real one. A target reads them from here; the harness itself sets nothing in its own process.
func RootEnv(root string) Env {
	return Env{
		Root:      root,
		Home:      filepath.Join(root, "home"),
		CodexHome: filepath.Join(root, "codex-home"),
		CrwHome:   filepath.Join(root, "crw-home"),
		TmpDir:    filepath.Join(root, "tmp"),
	}
}

// PrepareRoot makes the case root and every home under it, so a target that reads HOME,
// CODEX_HOME, CRW_HOME, CODEXCLAW_HOME or TMPDIR finds a directory that already exists: a
// target that makes its own temporary file there would otherwise fail on a missing parent.
func PrepareRoot(root string) error {
	// a case root, and every home under it, is made below a directory of the harness's own and never in
	// the account's real home (CRW-1186)
	if err := homeguard.Refuse(root); err != nil {
		return err
	}
	env := RootEnv(root)
	if err := homeguard.MkdirAll(root, 0o755); err != nil {
		return err
	}
	// The root is judged once above; each home is a direct child of it, made here as a plain directory, so it
	// cannot be a link that was not there. Whatever already stands in a home's place (a directory, a link, a file)
	// is judged as before, by the guard's own MkdirAll.
	for _, dir := range []string{env.Home, env.CodexHome, env.CrwHome, env.TmpDir, filepath.Join(root, "codexclaw-home")} {
		if err := os.Mkdir(dir, 0o755); err == nil {
			continue
		}
		if err := homeguard.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// MkdirTempRoot makes a temporary directory for a case or a worker in the temporary directory from
// tmploc, the one place (CRW-1579), unless that is the account's real home or lies in it (CRW-1186).
// A TMPDIR that is not absolute is refused with the reason, and no other place is tried.
func MkdirTempRoot(pattern string) (string, error) {
	parent, err := tmploc.Dir()
	if err != nil {
		return "", err
	}
	if err := homeguard.Refuse(parent); err != nil {
		return "", err
	}
	return tmploc.MkdirTemp(pattern)
}
