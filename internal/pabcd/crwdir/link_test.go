package crwdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listing is every entry below dir with its content, so a test can show that nothing outside the workspace was written.
func listing(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		data, _ := os.ReadFile(path)
		out = append(out, strings.TrimPrefix(path, dir)+"="+string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

// A symbolic link, dangling or not, or a file where .crw belongs is an input error: EnsureDir names the path, writes nothing
// through it and leaves the entry as it was. The race rows plant the link after the mkdir, before the .gitignore is written.
func TestEnsureDirRefusesALinkOrANonDirectoryAtTheStateDirectory(t *testing.T) {
	cases := []struct {
		name, want string
		plant      func(dir, outside string) error
		ignore     func(path, data string) error
	}{
		{"link to an outside directory", "symbolic link", func(dir, outside string) error { return os.Symlink(outside, dir) }, writeExclusive},
		{"dangling link", "symbolic link", func(dir, outside string) error { return os.Symlink(filepath.Join(outside, "gone"), dir) }, writeExclusive},
		{"file in place", "not a directory", func(dir, _ string) error { return os.WriteFile(dir, []byte("a file"), 0o644) }, writeExclusive},
		{"race: a concurrent creator links it and wins the .gitignore", "symbolic link", nil, nil},
		{"race: linked before the .gitignore is written", "symbolic link", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd, outside := t.TempDir(), t.TempDir()
			dir := filepath.Join(cwd, DirName)
			ignore := c.ignore
			swap := func() error { return errors.Join(os.Remove(dir), os.Symlink(outside, dir)) }
			switch {
			case c.plant != nil:
				if err := c.plant(dir, outside); err != nil {
					t.Fatal(err)
				}
			case strings.Contains(c.name, "wins"):
				ignore = func(string, string) error { return errors.Join(swap(), os.ErrExist) }
			default:
				ignore = func(path, data string) error {
					if err := swap(); err != nil {
						return err
					}
					return writeExclusive(path, data)
				}
			}
			before, _ := os.Lstat(dir)
			got, err := ensureDir(cwd, ignore)
			if err == nil || !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), c.want) || got != "" {
				t.Fatalf("ensureDir = %q, %v; want a refusal naming %s and %q", got, err, dir, c.want)
			}
			if left := listing(t, outside); left != "" {
				t.Fatalf("written outside the workspace: %s", left)
			}
			if after, _ := os.Lstat(dir); before != nil && (after == nil || after.Mode() != before.Mode()) {
				t.Fatalf("the entry at %s changed: %v -> %v", dir, before, after)
			}
		})
	}
}

// The shared checks every writer of the tree uses: EnsureSubdir creates real directories and refuses a link at any step, CheckTree
// refuses one without creating anything and stops at the first missing step, and OpenFile and NotLink refuse a link at a file.
func TestTheTreeHelpersRefuseALinkAtEveryStep(t *testing.T) {
	cwd, outside := t.TempDir(), t.TempDir()
	if dir, err := EnsureSubdir(cwd, "a", "b"); err != nil || dir != filepath.Join(cwd, DirName, "a", "b") {
		t.Fatalf("EnsureSubdir = %q, %v", dir, err)
	}
	if err := CheckTree(cwd, "a", "missing", "x"); err != nil {
		t.Fatalf("CheckTree with a missing step: %v", err)
	}
	link := filepath.Join(cwd, DirName, "a", "c")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(cwd, DirName, "a", "f")
	if err := os.Symlink(filepath.Join(outside, "victim"), file); err != nil {
		t.Fatal(err)
	}
	_, ensureErr := EnsureSubdir(cwd, "a", "c", "d")
	_, openErr := OpenFile(file, os.O_WRONLY|os.O_CREATE, 0o666)
	for name, err := range map[string]error{"EnsureSubdir": ensureErr, "CheckTree": CheckTree(cwd, "a", "c", "d"), "OpenFile": openErr, "NotLink": NotLink(file)} {
		if !errors.Is(err, ErrLink) || !strings.Contains(err.Error(), filepath.Join(cwd, DirName, "a")) {
			t.Errorf("%s: %v, want the link refusal", name, err)
		}
	}
	if left := listing(t, outside); left != "" {
		t.Fatalf("written outside the workspace: %s", left)
	}
}
