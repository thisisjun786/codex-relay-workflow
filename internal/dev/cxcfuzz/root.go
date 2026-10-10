//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
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
	for _, dir := range []string{root, env.Home, env.CodexHome, env.CrwHome, env.TmpDir, filepath.Join(root, "codexclaw-home")} {
		if err := homeguard.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// MkdirTempRoot makes a temporary directory for a case or a worker, unless $TMPDIR is the account's
// real home or lies in it (CRW-1186).
func MkdirTempRoot(pattern string) (string, error) {
	if err := homeguard.Refuse(os.TempDir()); err != nil {
		return "", err
	}
	return os.MkdirTemp("", pattern)
}
