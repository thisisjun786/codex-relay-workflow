package manage

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// rootDir is the directory part of path, taken from the text and cleaned of nothing. A path built
// below a configured or environment root keeps the spelling the filesystem resolves: filepath.Dir
// cleans, and cleaning a root that mixes a symbolic link and ".." names a different directory than
// the kernel gives that spelling. Use crwconfig.JoinRoot to build such a path and rootDir to
// take its directory; filepath.Base of the last element is safe to use as it is.
//
// For a path without "." or ".." elements and with no repeated separators it returns what
// filepath.Dir returns.
func rootDir(path string) string {
	separator := string(filepath.Separator)
	i := strings.LastIndex(path, separator)
	if i < 0 {
		return "."
	}
	dir := strings.TrimRight(path[:i], separator)
	if dir == "" {
		return separator
	}
	return dir
}

// rootGlob is the paths below dir whose components match pattern, one pattern per path component,
// in sorted order. It does what filepath.Glob does for a pattern of that shape, except that a
// found name is joined to its directory as raw text: filepath.Glob joins with filepath.Join, which
// cleans, so a ".." that follows a symbolic link in the root would be folded away before the
// filesystem sees it and the search would go on in a different directory. A component without a
// glob character is looked up with Lstat rather than listed, as in filepath.Glob, so only the
// directories a wildcard searches must be readable. A directory that cannot be read has no
// matches, as in filepath.Glob; a malformed pattern is an error.
func rootGlob(dir string, pattern ...string) ([]string, error) {
	if len(pattern) == 0 {
		return nil, nil
	}
	for _, part := range pattern {
		if _, err := filepath.Match(part, ""); err != nil {
			return nil, err
		}
	}
	dirs := []string{dir}
	for i, part := range pattern {
		var next []string
		for _, d := range dirs {
			if !rootGlobHasMeta(part) {
				// a literal is looked up by name, as filepath.Glob does, so its directory need only
				// be searchable, not listable
				path := crwconfig.JoinRoot(d, part)
				if _, err := os.Lstat(path); err == nil {
					next = append(next, path)
				}
				continue
			}
			entries, err := os.ReadDir(d)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if ok, _ := filepath.Match(part, entry.Name()); ok {
					next = append(next, crwconfig.JoinRoot(d, entry.Name()))
				}
			}
		}
		dirs = next
		if i < len(pattern)-1 && len(dirs) == 0 {
			return nil, nil
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// rootGlobHasMeta reports whether part carries a character filepath.Match treats specially, the
// test filepath.Glob makes before it lists a directory instead of looking a name up.
func rootGlobHasMeta(part string) bool {
	magic := `*?[\`
	if runtime.GOOS == "windows" {
		magic = `*?[`
	}
	return strings.ContainsAny(part, magic)
}

// rootWalk calls visit for root and everything below it, a directory before its entries and the
// entries of a directory in name order, like filepath.WalkDir. path is root followed by the names
// walked, joined as raw text (see rootGlob), and rel is those names joined with "/" ("" for root
// itself). root is not looked at before visit sees it; a link is reported, never followed.
func rootWalk(root string, visit func(path, rel string, entry os.DirEntry) error) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	return rootWalkEntry(root, "", rootDirEntry{info}, visit)
}

func rootWalkEntry(path, rel string, entry os.DirEntry, visit func(path, rel string, entry os.DirEntry) error) error {
	if err := visit(path, rel, entry); err != nil {
		return err
	}
	if !entry.IsDir() {
		return nil
	}
	children, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, child := range children {
		childRel := child.Name()
		if rel != "" {
			childRel = rel + "/" + child.Name()
		}
		if err := rootWalkEntry(crwconfig.JoinRoot(path, child.Name()), childRel, child, visit); err != nil {
			return err
		}
	}
	return nil
}

// rootDirEntry is the os.DirEntry of the walk's root, which comes from an Lstat.
type rootDirEntry struct{ info os.FileInfo }

func (d rootDirEntry) Name() string               { return d.info.Name() }
func (d rootDirEntry) IsDir() bool                { return d.info.IsDir() }
func (d rootDirEntry) Type() os.FileMode          { return d.info.Mode().Type() }
func (d rootDirEntry) Info() (os.FileInfo, error) { return d.info, nil }
