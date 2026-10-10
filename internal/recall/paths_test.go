package recall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecallHomeAndDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ value, want string }{
		{"", filepath.Join(home, ".codex")}, {" \ufeff", filepath.Join(home, ".codex")},
		{".", wd}, {"  relative  ", filepath.Join(wd, "  relative  ")},
	} {
		env := func(k string) (string, bool) {
			if k == "CODEX_HOME" {
				return c.value, true
			}
			return home, true // The supplied environment's HOME is the one used, not the process's.
		}
		got, err := codexHome(env)
		if err != nil || got != c.want {
			t.Errorf("home(%q) = %q, %v; want %q", c.value, got, err, c.want)
		}
	}
	got, err := codexHome()
	if err != nil || got != home {
		t.Errorf("default env = %q, %v", got, err)
	}
	if sessionsDir(home) != filepath.Join(home, "sessions") || memoriesDir(home) != filepath.Join(home, "memories") {
		t.Fatal("child directories differ")
	}
}

// The direct B-class assertion in recall/test/chat-search.test.ts:52-54, plus filename and Number ties.
func TestRecallVersionedDB(t *testing.T) {
	for _, c := range []struct {
		names []string
		want  string
	}{
		{[]string{"state_1.sqlite", "state_2.sqlite"}, "state_2.sqlite"},
		{[]string{"state_2.sqlite", "state_10.sqlite", "state_99.sqlite-wal", "state_99.sqlite\n", "state_١.sqlite"}, "state_10.sqlite"},
		{[]string{"state_05.sqlite", "state_5.sqlite"}, "state_05.sqlite"},
		{[]string{"state_9007199254740992.sqlite", "state_9007199254740993.sqlite"}, "state_9007199254740992.sqlite"},
		{[]string{"memories_7.sqlite", "state_x.sqlite"}, ""},
	} {
		dir := t.TempDir()
		for _, name := range c.names {
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		want := ""
		if c.want != "" {
			want = filepath.Join(dir, c.want)
		}
		got, err := stateDbPath(dir)
		if err != nil || got != want {
			t.Errorf("%v: %q, %v; want %q", c.names, got, err, want)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memories_7.sqlite"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := memoriesDbPath(dir)
	if err != nil || got != filepath.Join(dir, "memories_7.sqlite") {
		t.Fatal(got, err)
	}
	if got, err := stateDbPath(filepath.Join(dir, "absent")); got != "" || err != nil {
		t.Fatal(got, err)
	}
	if _, err := stateDbPath(filepath.Join(dir, "memories_7.sqlite")); err == nil {
		t.Fatal("existing non-directory must throw")
	}
}

// port: fixed (docs/port-cxc/known-defects/CRW-1123.md, :390 and :391); TestSweepVersionedDBNeedsAUsableFile and
// TestSweepVersionedDBHomeIsResolvedOnce hold the cases. A home with a link and `..` lists and joins one directory.
func TestRecallVersionedDBNamesTheListedDirectory(t *testing.T) {
	root := t.TempDir()
	outer, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(outer, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outer, "state_7.sqlite"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outer, "child"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := stateDbPath(root + "/link/..")
	if err != nil || got != filepath.Join(outer, "state_7.sqlite") {
		t.Fatal(got, err)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatal("the named database must exist", err)
	}
}
