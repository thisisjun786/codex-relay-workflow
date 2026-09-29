package record

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func resolvePath(path string) (string, error) {
	resolved, err := store.ResolvePath(path)
	if err == nil {
		return resolved, nil
	}
	// Path.resolve() is not strict: a component that cannot be examined is kept as spelled
	// rather than failing the whole question. Only a path with no working directory to anchor
	// it answers nothing.
	if !filepath.IsAbs(path) {
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			return "", cwdErr
		}
		path = cwd + "/" + path
	}
	return filepath.Clean(path), nil
}

// sourceTree is the git tree of the commit this binary was built from, stamped at build time
// (Makefile: -X .../internal/runtime/record.sourceTree=$(git rev-parse HEAD^{tree}), and only
// from a clean working tree, so a build of modified or untracked files stamps nothing). Go's
// build information carries the commit and whether the tree was modified, never a tree hash,
// and both fault sweepers require repositoryTree and subdirectoryTree as 40-hex values.
var sourceTree string

var hexCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Source is the installs[].source block both fault sweepers read as the installed revision
// (faultsweep.installed_revision, internal/relay/faults/sweep.go installation). The Go binary
// is built from the whole module, which is the repository root, so its subdirectory tree IS
// the repository tree. A value the build did not stamp is null, and then both sweepers report
// "this copy's install entry records an incomplete revision (repositoryTree, subdirectoryTree
// missing or malformed), which identifies nothing" rather than a revision nobody measured.
func Source() Object {
	var commit, tree any
	var clean any
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if hexCommit.MatchString(setting.Value) {
					commit = setting.Value
				}
			case "vcs.modified":
				clean = setting.Value == "false"
			}
		}
	}
	if hexCommit.MatchString(sourceTree) {
		tree = sourceTree
	}
	return Object{
		{Key: "repositoryCommit", Value: commit},
		{Key: "repositoryTree", Value: tree},
		{Key: "subdirectoryTree", Value: tree},
		{Key: "workingTreeClean", Value: clean},
	}
}

// Target is the release target this binary was built for, as the archive names it.
func Target() string { return runtime.GOOS + "/" + runtime.GOARCH }

// FileDigest is the SHA-256 of a file's bytes, hex.
func FileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// GoInstall is one Go install entry for component name, for an install whose binary sits at
// <environment>/bin/crw. location is <environment>/bin because that is the directory a running
// Go relay reports as its own (filepath.Dir of its resolved executable), which is how both
// sweepers find their entry. integrity and binaryDigest are the binary's SHA-256; the
// interpreter fields (interpreter, interpreterPath, installMode) are absent, which the
// settings-record decision (docs/port/decisions.md 18) allows. The entry point is the
// component's compatibility name beside the binary.
func GoInstall(environment, consoleScript, digest, reachedVia string, matchesDefinition bool) Object {
	bin := filepath.Join(environment, "bin")
	return Object{
		{Key: "binaryDigest", Value: digest},
		{Key: "digestMatchesDefinition", Value: matchesDefinition},
		{Key: "entryPoint", Value: filepath.Join(bin, consoleScript)},
		{Key: "environment", Value: environment},
		{Key: "integrity", Value: digest},
		{Key: "location", Value: bin},
		{Key: "reachedVia", Value: reachedVia},
		{Key: "source", Value: Source()},
		{Key: "target", Value: Target()},
	}
}
