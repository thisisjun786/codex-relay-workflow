// Package definition is the Go side of the one compatibility definition (OPS-1.1),
// scripts/crw_runtime/components.json.
//
// The committed file stays definitionVersion 1 and keeps its Python-shaped fields while the
// Python installer and the developer harness read it (until todos 44 and 48). This package
// carries only what a Go install needs - component names, console-script names (the
// compatibility links beside crw), version, licence, upstream provenance and the identity
// tool - and TestDefinitionAgreesWithComponentsJSON keeps it equal to the committed file.
// Nothing here carries a per-target binary digest: release digests live in the release's
// SHA256SUMS and in the host record (docs/port/decisions.md 35).
package definition

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Version is components.json definitionVersion.
const Version = 1

// Upstream is a component's retained provenance.
type Upstream struct{ Remote, Revision, Licence string }

// Component is one component's retained fields.
type Component struct {
	Name            string
	ConsoleScript   string
	Version         string
	LicencePath     string
	IdentityTool    string
	ExerciseCommand string
	Upstream        Upstream
}

// Names of the two components.
const (
	Bridge = "codex-thread-bridge"
	Relay  = "codex-session-relay"
)

// Components is the definition, in components.json order.
var Components = []Component{
	{
		Name: Bridge, ConsoleScript: "codex-thread-bridge", Version: "0.2.0",
		LicencePath: "packages/codex-thread-bridge/LICENSE", IdentityTool: "get_capabilities",
		Upstream: Upstream{Remote: "https://github.com/saidelike/codex-thread-bridge", Revision: "bb684f35b4919a82b09d2290dc26623716803d62", Licence: "MIT"},
	},
	{
		Name: Relay, ConsoleScript: "codex-session-relay", Version: "0.2.0",
		LicencePath: "LICENSE", ExerciseCommand: "doctor",
		Upstream: Upstream{Remote: "none", Revision: "d3394038c108022dc0ff48d48ec878094c67e6df", Licence: "MIT"},
	},
}

// HookScript is the third compatibility link beside crw: the completion hook's entry point.
const HookScript = "crw-completion-hook"

// Links are the names the installer places beside crw, each a symlink to it.
func Links() []string {
	return []string{Components[1].ConsoleScript, Components[0].ConsoleScript, HookScript}
}

// Of is the component with this name.
func Of(name string) (Component, bool) {
	for _, c := range Components {
		if c.Name == name {
			return c, true
		}
	}
	return Component{}, false
}

// Digest is definition.ops12_digest: SHA-256 over a directory, walking every file except
// anything under __pycache__, sorted by POSIX relative path, each contributing its relative
// path, a zero byte and the SHA-256 of its bytes. A subtree that cannot be read fails the walk
// instead of being left out. Symbolic links to directories are not descended; a link to a file
// counts as the file it names. An entry whose target is absent (a dangling link) is not a file,
// as os.DirEntry.is_file answers; any other failure to examine one (a link loop, a directory
// without search permission) fails the walk, as it raises out of files_under.
func Digest(root string) (string, error) {
	var files []string
	pending := []string{root}
	for len(pending) > 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		entries, err := os.ReadDir(dir)
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				if entry.Name() == "__pycache__" {
					continue
				}
				pending = append(pending, path)
				continue
			}
			info, err := os.Stat(path)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				continue
			case err != nil:
				return "", err
			case !info.Mode().IsRegular():
				continue
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return "", err
			}
			files = append(files, filepath.ToSlash(relative))
		}
	}
	sort.Strings(files)
	digest := sha256.New()
	for _, relative := range files {
		if strings.Contains("/"+relative+"/", "/__pycache__/") {
			continue
		}
		file, err := os.Open(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return "", err
		}
		inner := sha256.New()
		_, err = io.Copy(inner, file)
		_ = file.Close()
		if err != nil {
			return "", err
		}
		digest.Write([]byte(relative))
		digest.Write([]byte{0})
		digest.Write(inner.Sum(nil))
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
