//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"
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
	env := RootEnv(root)
	for _, dir := range []string{root, env.Home, env.CodexHome, env.CrwHome, env.TmpDir, filepath.Join(root, "codexclaw-home")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}
