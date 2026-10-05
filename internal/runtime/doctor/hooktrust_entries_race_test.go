package doctor

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

const hookTrustEntriesRaceEscape = "plugin manifest symlink escapes plugin root: "

// hookTrustEntriesRaceChain links c1 -> c2 -> ... -> cN -> sub/real.json: reading c1 follows N links.
func hookTrustEntriesRaceChain(t *testing.T, root string, n int) {
	t.Helper()
	target := "sub/real.json"
	for i := n; i >= 1; i-- {
		name := "c" + strconv.Itoa(i)
		hookTrustEntriesRaceMust(t, os.Symlink(target, filepath.Join(root, name)))
		target = name
	}
}

func hookTrustEntriesRaceMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// hookTrustEntriesRaceTree builds a plugin root and the files around it under a fresh temporary
// directory, with HOME, CODEX_HOME and CRW_HOME pointed into it so nothing reaches the real homes.
func hookTrustEntriesRaceTree(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	for _, name := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(name, filepath.Join(base, "home", name))
	}
	for name, content := range map[string]string{
		"plugin/h.json": "inside", "plugin/sub/real.json": "inside", "plugin/hooks/h.json": "inside",
		"outside.json": "outside", "outdir/h.json": "outside",
	} {
		path := filepath.Join(base, name)
		hookTrustEntriesRaceMust(t, os.MkdirAll(filepath.Dir(path), 0o755))
		hookTrustEntriesRaceMust(t, os.WriteFile(path, []byte(content), 0o644))
	}
	return filepath.Join(base, "plugin")
}

// TestHookTrustEntriesReadContained_rootOpen drives hookTrustEntriesReadContained through links that
// stay inside the plugin root, links that leave it, a file that changes while it is read, and files
// that are not regular. The seam observe is called with "open" just before the file is opened.
func TestHookTrustEntriesReadContained_rootOpen(t *testing.T) {
	link := func(t *testing.T, root, name, target string) {
		t.Helper()
		hookTrustEntriesRaceMust(t, os.Symlink(target, filepath.Join(root, name)))
	}
	cases := []struct {
		name, ref string
		setup     func(t *testing.T, root string)
		swap      func(t *testing.T, root string) func(stage string)
		want      string // content of an expected read
		wantErr   string // exact error text
		wantIs    error
		wantText  string // part of the error text
	}{
		{name: "file swapped to an outside link and back around the open", ref: "h.json", wantErr: hookTrustEntriesRaceEscape + "h.json",
			swap: func(t *testing.T, root string) func(string) {
				hook := filepath.Join(root, "h.json")
				return func(stage string) {
					// The sequence of the finding: a regular file at the first resolution, a link to the
					// outside file at the open, the regular file at the re-resolution, the link at the stat.
					hookTrustEntriesRaceMust(t, os.Remove(hook))
					if stage == "open" || stage == "stat" {
						hookTrustEntriesRaceMust(t, os.Symlink("../outside.json", hook))
					} else {
						hookTrustEntriesRaceMust(t, os.WriteFile(hook, []byte("inside"), 0o644))
					}
				}
			}},
		{name: "directory swapped to an outside link at the open", ref: "hooks/h.json", wantErr: hookTrustEntriesRaceEscape + "hooks/h.json",
			swap: func(t *testing.T, root string) func(string) {
				return func(stage string) {
					if stage == "open" {
						hooks := filepath.Join(root, "hooks")
						hookTrustEntriesRaceMust(t, os.Rename(hooks, hooks+".kept"))
						hookTrustEntriesRaceMust(t, os.Symlink("../outdir", hooks))
					}
				}
			}},
		{name: "link inside the root", ref: "link.json", want: "inside",
			setup: func(t *testing.T, root string) { link(t, root, "link.json", "sub/real.json") }},
		{name: "directory link inside the root", ref: "dirlink/real.json", want: "inside",
			setup: func(t *testing.T, root string) { link(t, root, "dirlink", "sub") }},
		{name: "link leaving the root", ref: "out.json", wantErr: hookTrustEntriesRaceEscape + "out.json",
			setup: func(t *testing.T, root string) { link(t, root, "out.json", "../outside.json") }},
		{name: "absolute link that points inside", ref: "abs.json", wantErr: hookTrustEntriesRaceEscape + "abs.json",
			setup: func(t *testing.T, root string) { link(t, root, "abs.json", filepath.Join(root, "sub", "real.json")) }},
		{name: "link that leaves the root and comes back", ref: "back.json", wantErr: hookTrustEntriesRaceEscape + "back.json",
			setup: func(t *testing.T, root string) { link(t, root, "back.json", "../plugin/sub/real.json") }},
		{name: "dangling link outside the root", ref: "far.json", wantErr: hookTrustEntriesRaceEscape + "far.json",
			setup: func(t *testing.T, root string) { link(t, root, "far.json", "../missing.json") }},
		{name: "chain of eight links", ref: "c1", want: "inside",
			setup: func(t *testing.T, root string) { hookTrustEntriesRaceChain(t, root, 8) }},
		{name: "chain of nine links", ref: "c1", wantIs: syscall.ELOOP,
			setup: func(t *testing.T, root string) { hookTrustEntriesRaceChain(t, root, 9) }},
		{name: "plugin root that can be searched but not read", ref: "sub/real.json", wantIs: fs.ErrPermission,
			setup: func(t *testing.T, root string) {
				if os.Geteuid() == 0 {
					t.Skip("root reads every directory")
				}
				// os.OpenRoot opens the directory for reading; opening a file by its path needs search only.
				hookTrustEntriesRaceMust(t, os.Chmod(root, 0o111))
				t.Cleanup(func() { os.Chmod(root, 0o755) })
			}},
		{name: "dangling link inside", ref: "gone.json", wantIs: fs.ErrNotExist,
			setup: func(t *testing.T, root string) { link(t, root, "gone.json", "sub/missing.json") }},
		{name: "directory", ref: "sub", wantIs: syscall.EISDIR},
		{name: "named pipe", ref: "pipe.json", wantText: "not a regular file",
			setup: func(t *testing.T, root string) {
				pipe := filepath.Join(root, "pipe.json")
				hookTrustEntriesRaceMust(t, syscall.Mkfifo(pipe, 0o600))
				// A descriptor that holds the pipe open keeps the read-only open from blocking.
				holder, err := os.OpenFile(pipe, os.O_RDWR|syscall.O_NONBLOCK, 0)
				hookTrustEntriesRaceMust(t, err)
				t.Cleanup(func() { holder.Close() })
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := hookTrustEntriesRaceTree(t)
			if tc.setup != nil {
				tc.setup(t, root)
			}
			var seen []string
			var observe func(string)
			if tc.swap != nil {
				swap := tc.swap(t, root)
				observe = func(stage string) {
					seen = append(seen, stage)
					swap(stage)
				}
			}
			got, err := hookTrustEntriesReadContained(root, tc.ref, observe)
			switch {
			case string(got) == "outside":
				t.Fatalf("read the file outside the plugin root (error %v)", err)
			case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
				t.Fatalf("error = %v (read %q), want %q", err, got, tc.wantErr)
			case tc.wantIs != nil && !errors.Is(err, tc.wantIs):
				t.Fatalf("error = %v, want one that is %v", err, tc.wantIs)
			case tc.wantText != "" && (err == nil || !strings.Contains(err.Error(), tc.wantText)):
				t.Fatalf("error = %v, want one that says %q", err, tc.wantText)
			case tc.want != "" && (err != nil || string(got) != tc.want):
				t.Fatalf("read %q, %v; want %q", got, err, tc.want)
			}
			if tc.swap != nil && !slices.Contains(seen, "open") {
				t.Fatalf("the seam never observed the open (stages %v)", seen)
			}
		})
	}
}
