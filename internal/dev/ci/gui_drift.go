//go:build dev

package ci

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The screen-drift check of `crw-dev ci gui-drift` (CRW-831). The built screen is committed under
// internal/gui/assets and embedded in the binary, so the committed tree is what a Node-free
// checkout serves. This check rebuilds the screen elsewhere and compares the whole tree: an added
// file, a missing file, a renamed asset and a one-byte difference are all refused, and the check
// never passes by comparing the commit against itself.
//
// The caller must have just built the screen: the fresh side is the directory named by --built,
// which is required, and the committed side is the tree of the named revision (HEAD by default)
// read with git, never the working tree - `//go:embed all:assets` compiles whatever the working
// tree holds, so a file that is present but untracked is exactly the drift this must catch.
const (
	// guiAssetsDir is the committed screen tree, relative to the repository root.
	guiAssetsDir = "internal/gui/assets"
	// guiDriftIndex is the file a fresh build always writes; its absence means the directory was
	// never built.
	guiDriftIndex = "index.html"
)

// GuiDrift is `crw-dev ci gui-drift`.
func GuiDrift(args []string, stdout, stderr io.Writer) int {
	flags := newFlags("gui-drift")
	built := flags.String("built", "", "the directory holding the freshly built screen tree (required)")
	revision := flags.String("revision", "HEAD", "the commit whose "+guiAssetsDir+" tree is the oracle")
	if code := parseFlags(flags, "Compare a freshly built screen tree with the committed "+guiAssetsDir+
		" tree of a revision, refusing a new file, a missing file, a renamed asset and a byte difference.",
		args, stdout, stderr); code >= 0 {
		return code
	}
	if *built == "" {
		fmt.Fprintln(stderr, "crw-dev ci gui-drift: error: --built is required: name the directory holding the freshly built screen tree")
		return 2
	}
	root, err := repositoryRoot()
	if err != nil {
		return failf(stderr, "gui-drift: %s", err)
	}
	problems, err := guiDriftProblems(root, *revision, *built)
	if err != nil {
		return failf(stderr, "gui-drift: %s", err)
	}
	if len(problems) > 0 {
		fmt.Fprintf(stderr, "%s does not match a fresh build of the screens:\n", guiAssetsDir)
		for _, problem := range problems {
			fmt.Fprintln(stderr, "  "+problem)
		}
		fmt.Fprintf(stderr, "\nRun `make gui-assets` to rebuild %s and commit the result. Nothing was modified.\n", guiAssetsDir)
		return 1
	}
	fmt.Fprintf(stdout, "The built screens match %s at %s, byte for byte.\n", guiAssetsDir, *revision)
	return 0
}

// guiDriftEntry is one file of the committed tree: its path relative to the assets directory and
// the object that holds its bytes.
type guiDriftEntry struct {
	path string
	oid  string
	size int64
}

// guiDriftProblems compares the committed tree of revision with the freshly built tree in built and
// returns one line per difference, sorted. An error means the comparison could not be made at all,
// which is never a pass: an unreadable repository, an unresolvable revision, a tree that is not a
// blob, a built directory that does not exist, one that holds no index.html, and one that is the
// committed tree itself are all refused.
//
// The working tree at internal/gui/assets is compared too: `//go:embed all:assets` compiles that
// directory, not the revision, so a file a local edit left there would be embedded while the
// built and committed trees still agreed. It is compared with HEAD, whatever revision the build
// side was judged against, because HEAD is what this checkout would build. Every difference - an
// extra file, a modification, a deletion - is reported with the "working tree" marker.
func guiDriftProblems(root, revision, built string) ([]string, error) {
	committed, err := guiDriftCommitted(root, revision)
	if err != nil {
		return nil, err
	}
	builtDir, err := guiDriftBuiltDir(root, built)
	if err != nil {
		return nil, err
	}
	fresh, err := guiDriftFresh(builtDir)
	if err != nil {
		return nil, err
	}
	var problems []string
	workingProblems, err := guiDriftWorkingTree(root)
	if err != nil {
		return nil, err
	}
	problems = append(problems, workingProblems...)
	for path, entry := range committed {
		data, ok := fresh[path]
		if !ok {
			problems = append(problems, path+": committed but not built (a fresh build produces no such file)")
			continue
		}
		committedBytes, err := guiDriftBlob(root, entry.oid)
		if err != nil {
			return nil, err
		}
		// The size from ls-tree is a fast pre-check; the bytes are what decide, so a difference of
		// the same length is still caught.
		if entry.size != int64(len(data)) || !bytes.Equal(committedBytes, data) {
			problems = append(problems, fmt.Sprintf("%s: differs (committed %d bytes, built %d bytes)",
				path, len(committedBytes), len(data)))
		}
	}
	for path := range fresh {
		if _, ok := committed[path]; !ok {
			problems = append(problems, path+": built but not committed (a fresh build produced a file the commit does not hold)")
		}
	}
	sort.Strings(problems)
	return problems, nil
}

// guiDriftWorkingTree compares the working tree at internal/gui/assets with HEAD, which is what a
// local `go build` would embed. The directory must exist and hold exactly HEAD's paths with
// exactly HEAD's bytes: an untracked file, a modification and a deletion are each reported. A
// checkout that did not write the directory at all is an error rather than a pass, so the check
// cannot be satisfied by an empty tree.
func guiDriftWorkingTree(root string) ([]string, error) {
	committed, err := guiDriftCommitted(root, "HEAD")
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, filepath.FromSlash(guiAssetsDir))
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("the working tree has no %s directory", guiAssetsDir)
	}
	working := map[string][]byte{}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		working[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	var problems []string
	for rel, entry := range committed {
		data, ok := working[rel]
		if !ok {
			problems = append(problems, rel+": working tree has no such file (a local build would embed the committed bytes, not this tree)")
			continue
		}
		committedBytes, err := guiDriftBlob(root, entry.oid)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(committedBytes, data) {
			problems = append(problems, fmt.Sprintf("%s: working tree differs from %s (committed %d bytes, working tree %d bytes)",
				rel, entry.path, len(committedBytes), len(data)))
		}
	}
	for rel := range working {
		if _, ok := committed[rel]; !ok {
			problems = append(problems, rel+": in the working tree but not committed (a local build would embed a file the commit does not hold)")
		}
	}
	return problems, nil
}

// guiDriftCommitted reads the committed tree of revision: each blob's path relative to the assets
// directory, its object id and its size. A git failure, an unresolvable revision and a tree entry
// that is not a regular file are errors, so nothing about the oracle is guessed.
func guiDriftCommitted(root, revision string) (map[string]guiDriftEntry, error) {
	out, err := runGit(root, "ls-tree", "-r", "-z", "-l", revision, "--", guiAssetsDir)
	if err != nil {
		return nil, err
	}
	entries := map[string]guiDriftEntry{}
	for _, record := range strings.Split(string(out), "\x00") {
		if record == "" {
			continue
		}
		meta, path, found := strings.Cut(record, "\t")
		if !found {
			return nil, fmt.Errorf("git ls-tree returned an entry without a path: %q", record)
		}
		fields := strings.Fields(meta)
		if len(fields) != 4 || fields[1] != "blob" {
			return nil, fmt.Errorf("%s is not a regular file in %s", path, revision)
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s has no size in %s: %v", path, revision, err)
		}
		if size > largeBlobLimit {
			return nil, fmt.Errorf("%s is %d bytes, over the %d byte limit a committed screen asset may have",
				path, size, int64(largeBlobLimit))
		}
		rel, ok := strings.CutPrefix(path, guiAssetsDir+"/")
		if !ok || rel == "" {
			return nil, fmt.Errorf("git ls-tree named %s outside %s", path, guiAssetsDir)
		}
		entries[rel] = guiDriftEntry{path: path, oid: fields[2], size: size}
	}
	if _, ok := entries[guiDriftIndex]; !ok {
		return nil, fmt.Errorf("%s holds no %s in %s", guiAssetsDir, guiDriftIndex, revision)
	}
	return entries, nil
}

// guiDriftBuiltDir resolves the freshly built tree and refuses a directory that is not a build:
// one that does not exist, one that is not a directory, and one that is the committed tree itself,
// which would compare the commit against itself and always pass.
func guiDriftBuiltDir(root, built string) (string, error) {
	if built == "" {
		return "", fmt.Errorf("--built is required")
	}
	abs := built
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, built)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("--built %s is not a directory holding a fresh build", built)
	}
	if same, err := sameDir(abs, filepath.Join(root, guiAssetsDir)); err != nil {
		return "", err
	} else if same {
		return "", fmt.Errorf("--built names the committed tree %s; name the directory a fresh build wrote", guiAssetsDir)
	}
	return abs, nil
}

// sameDir reports whether two paths name the same directory, judged on the kernel-resolved
// location so a link to the committed tree is refused too.
func sameDir(a, b string) (bool, error) {
	resolvedA, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false, fmt.Errorf("cannot resolve %s: %w", a, err)
	}
	resolvedB, err := filepath.EvalSymlinks(b)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("cannot resolve %s: %w", b, err)
	}
	return resolvedA == resolvedB, nil
}

// guiDriftFresh reads the freshly built tree: each regular file's bytes by its path relative to
// the build directory. A directory that holds no index.html is refused, so a run without a build
// cannot pass by comparing the commit against itself.
func guiDriftFresh(built string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(built, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(built, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(data) > largeBlobLimit {
			return fmt.Errorf("%s is %d bytes, over the %d byte limit a screen asset may have",
				rel, len(data), int64(largeBlobLimit))
		}
		files[rel] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, ok := files[guiDriftIndex]; !ok {
		return nil, fmt.Errorf("the built tree holds no %s, so it was never built", guiDriftIndex)
	}
	return files, nil
}

// guiDriftBlob reads one committed object's bytes.
func guiDriftBlob(root, oid string) ([]byte, error) {
	out, err := runGit(root, "cat-file", "blob", oid)
	if err != nil {
		return nil, err
	}
	return out, nil
}
