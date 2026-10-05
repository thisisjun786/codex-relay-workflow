package migrate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// isolate gives every environment root a directory of its own, so no test reaches the real ~/.codex, ~/.crw or ~/.codexclaw, and
// returns a base directory without a link in its path (the roots policy refuses one).
func isolate(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	for _, k := range []string{"HOME", "CODEX_HOME", "CRW_HOME", "CODEXCLAW_HOME", "TMPDIR"} {
		dir := filepath.Join(base, "env-"+strings.ToLower(k))
		must(t, os.MkdirAll(dir, 0o755))
		t.Setenv(k, dir)
	}
	return base
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		must(t, os.MkdirAll(p, 0o755))
	}
}

func put(t *testing.T, path, data string, mode fs.FileMode) {
	t.Helper()
	mkdirs(t, filepath.Dir(path))
	must(t, os.WriteFile(path, []byte(data), 0o600))
	must(t, os.Chmod(path, mode))
	old := time.Unix(1577934245, 0) // a fixed old time, so a rewrite shows in a later fingerprint
	must(t, os.Chtimes(path, old, old))
}

func get(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	must(t, err)
	return string(b)
}

// tree lists every path below root.
func tree(t *testing.T, root string) (out []string) {
	t.Helper()
	must(t, filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error { out = append(out, p); return err }))
	return out
}

func wantRefusal(t *testing.T, err error, reason Reason) {
	t.Helper()
	var r *RefusedError
	if !errors.As(err, &r) || r.Reason != reason {
		t.Fatalf("error = %v, want a refusal with reason %q", err, reason)
	}
}

func openDir(t *testing.T, path string) *Dir {
	t.Helper()
	d, _, err := pinDir(path)
	if err != nil || d == nil {
		t.Fatalf("pinDir(%s) = %v, %v", path, d, err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestOpenResolvesRoots(t *testing.T) {
	base := isolate(t)
	ws, home := base+"/ws", os.Getenv("HOME")
	mkdirs(t, ws)
	env := map[string]string{"CODEXCLAW_HOME": base + "/eu", "CRW_HOME": base + "/ev", "CODEX_HOME": base + "/ec"}
	for name, c := range map[string]struct {
		env  map[string]string
		o    Options
		want []string
	}{
		"defaults":          {map[string]string{"CODEXCLAW_HOME": "", "CRW_HOME": "", "CODEX_HOME": ""}, Options{}, []string{ws + "/.codexclaw", ws + "/.crw", home + "/.codexclaw", home + "/.crw", home + "/.codex"}},
		"environment":       {env, Options{}, []string{ws + "/.codexclaw", ws + "/.crw", base + "/eu", base + "/ev", base + "/ec"}},
		"explicit over env": {env, Options{FromHome: base + "/xu", ToHome: base + "/xv", CodexHome: base + "/xc"}, []string{ws + "/.codexclaw", ws + "/.crw", base + "/xu", base + "/xv", base + "/xc"}},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			c.o.Scope, c.o.Cwd = ScopeAll, ws
			r, err := Open(c.o)
			must(t, err)
			defer r.Close()
			if got := []string{r.Project.SourcePath, r.Project.DestPath, r.User.SourcePath, r.User.DestPath, r.CodexPath}; !reflect.DeepEqual(got, c.want) {
				t.Errorf("roots = %v, want %v", got, c.want)
			}
		})
	}
}

func TestOpenInputs(t *testing.T) {
	base := isolate(t)
	put(t, base+"/file", "x", 0o644)
	// Only the roots of the selected scope are resolved: these invalid ones belong to the others.
	r, err := Open(Options{Scope: ScopeProject, Cwd: base, FromHome: base + "/file", CodexHome: "../up"})
	must(t, err)
	r.Close()
	if r.User != nil || r.Codex != nil {
		t.Error("user and codex roots were opened for the project scope")
	}
	for _, o := range []Options{{Scope: "everything"}, {Scope: ScopeUser, FromHome: base + "/a/../b"}, {Scope: ScopeUser, ToHome: base + "/missing/dir"}} {
		if r, err := Open(o); err == nil {
			r.Close()
			t.Errorf("Open(%+v) succeeded", o)
		}
	}
	for _, o := range []Options{{Scope: ScopeCodex, CodexHome: "/"}, {Scope: ScopeUser, FromHome: "/", ToHome: base + "/new"}, {Scope: ScopeUser, FromHome: base + "/old", ToHome: "/"}} {
		if r, err := Open(o); err == nil || !strings.Contains(err.Error(), "filesystem root") {
			t.Errorf("Open(%+v) = %v, want the filesystem root refused as a root", o, err)
			if r != nil {
				r.Close()
			}
		}
	}
	if _, err := Open(Options{Scope: ScopeUser, ToHome: base + "/missing/dir"}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an absent ancestor: %v, want ErrNotExist", err)
	}
}

func TestOpenRefusesOverlap(t *testing.T) {
	base := isolate(t)
	ws, a, b, home := base+"/ws", base+"/a", base+"/b", os.Getenv("HOME")
	mkdirs(t, ws, a, b)
	t.Setenv("CODEXCLAW_HOME", "")
	t.Setenv("CRW_HOME", "")
	for name, o := range map[string]Options{
		"user source is the destination":     {Scope: ScopeUser, FromHome: a, ToHome: a},
		"source inside destination":          {Scope: ScopeUser, FromHome: a + "/x", ToHome: a},
		"destination inside source":          {Scope: ScopeUser, FromHome: a, ToHome: a + "/x"},
		"user source is the project root":    {Scope: ScopeAll, Cwd: ws, FromHome: ws + "/.crw", ToHome: b, CodexHome: base + "/c"},
		"workspace is the home directory":    {Scope: ScopeAll, Cwd: home, CodexHome: base + "/c"},
		"codex home inside the user source":  {Scope: ScopeAll, Cwd: ws, FromHome: a, ToHome: b, CodexHome: a + "/c"},
		"codex home inside the project root": {Scope: ScopeAll, Cwd: ws, FromHome: a, ToHome: b, CodexHome: ws + "/.crw/c"},
	} {
		t.Run(name, func(t *testing.T) {
			before := tree(t, base)
			r, err := Open(o)
			if err == nil {
				r.Close()
			}
			wantRefusal(t, err, ReasonOverlap)
			if !reflect.DeepEqual(before, tree(t, base)) {
				t.Error("a refused Open created something")
			}
		})
	}
	// Disjoint roots open, and a Codex home may be the workspace itself: only its leaf names are mapped, in place.
	r, err := Open(Options{Scope: ScopeAll, Cwd: ws, FromHome: a, ToHome: b, CodexHome: ws})
	must(t, err)
	r.Close()
}

func TestOpenRefusesLinks(t *testing.T) {
	base := isolate(t)
	real, lnk, file := base+"/real", base+"/lnk", base+"/file"
	mkdirs(t, real+"/src", real+"/dst", base+"/ws1", base+"/ws2")
	put(t, file, "x", 0o644)
	for target, name := range map[string]string{real: lnk, base + "/missing": base + "/dead", real + "/src": base + "/ws1/.codexclaw", real + "/dst": base + "/ws2/.crw"} {
		must(t, os.Symlink(target, name))
	}
	user := func(from, to string) Options { return Options{Scope: ScopeUser, FromHome: from, ToHome: to} }
	for name, c := range map[string]struct {
		o      Options
		reason Reason
	}{
		"source is a link":              {user(lnk, base+"/new"), ReasonLink},
		"destination is a link":         {user(base+"/old", lnk), ReasonLink},
		"ancestor of the source":        {user(lnk+"/src", base+"/new"), ReasonLink},
		"ancestor of the destination":   {user(base+"/old", lnk+"/dst"), ReasonLink},
		"dangling link":                 {user(base+"/dead", base+"/new"), ReasonLink},
		"codex home is a link":          {Options{Scope: ScopeCodex, CodexHome: lnk}, ReasonLink},
		"project source is a link":      {Options{Scope: ScopeProject, Cwd: base + "/ws1"}, ReasonLink},
		"project destination is a link": {Options{Scope: ScopeProject, Cwd: base + "/ws2"}, ReasonLink},
		"workspace reached by a link":   {Options{Scope: ScopeProject, Cwd: lnk}, ReasonLink},
		// A later pair that is fine must not hide an earlier one that is not.
		"all scopes, project source a link":      {Options{Scope: ScopeAll, Cwd: base + "/ws1", FromHome: base + "/old", ToHome: base + "/new", CodexHome: base + "/cx"}, ReasonLink},
		"all scopes, project destination a link": {Options{Scope: ScopeAll, Cwd: base + "/ws2", FromHome: base + "/old", ToHome: base + "/new", CodexHome: base + "/cx"}, ReasonLink},
		"source is a file":                       {user(file, base+"/new"), ReasonNotDirectory},
		"destination is a file":                  {user(base+"/old", file), ReasonNotDirectory},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := Open(c.o)
			if err == nil {
				r.Close()
			}
			wantRefusal(t, err, c.reason)
		})
	}
}

func TestEnsureDest(t *testing.T) {
	base := isolate(t)
	o := Options{Scope: ScopeUser, FromHome: base + "/old", ToHome: base + "/new"}
	r, err := Open(o)
	must(t, err)
	defer r.Close()
	if _, err := os.Lstat(o.ToHome); err == nil || r.User.Source != nil || r.User.Dest != nil {
		t.Fatal("Open pinned or created an absent root")
	}
	d, err := r.User.EnsureDest(0o700)
	must(t, err)
	if fi, err := os.Stat(o.ToHome); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("destination = %v, %v; want a 0700 directory", fi, err)
	}
	if again, _ := r.User.EnsureDest(0o700); again != d {
		t.Error("EnsureDest pinned a second handle")
	}
	// A root that Open found already pinned still has its parent synced; a closed parent makes that visible.
	must(t, r.User.parent.Close())
	if _, err := r.User.EnsureDest(0o700); !errors.Is(err, os.ErrClosed) {
		t.Errorf("EnsureDest on a pinned root did not sync its parent: %v", err)
	}
}

func TestDirOpensOneComponentAtATime(t *testing.T) {
	base := isolate(t)
	mkdirs(t, base+"/in/sub", base+"/out")
	put(t, base+"/out/secret", "x", 0o644)
	put(t, base+"/in/file", "x", 0o644)
	must(t, os.Symlink(base+"/out", base+"/in/lnk"))
	d := openDir(t, base+"/in")
	for _, name := range []string{"", ".", "..", "sub/x", "../out/secret", "lnk/secret", "a\x00b"} {
		if _, err := d.Child(name); err == nil {
			t.Errorf("Child(%q) succeeded", name)
		}
		if _, err := d.EnsureChild(name, 0o755); err == nil {
			t.Errorf("EnsureChild(%q) succeeded", name)
		}
		if f, _, err := d.OpenRegular(name); err == nil {
			f.Close()
			t.Errorf("OpenRegular(%q) succeeded", name)
		}
	}
	for range 2 { // a fresh descriptor each time, so a second listing is the same
		if names, err := d.Names(); err != nil || !reflect.DeepEqual(names, []string{"file", "lnk", "sub"}) {
			t.Errorf("Names = %v, %v", names, err)
		}
	}
	_, err := d.Child("lnk")
	wantRefusal(t, err, ReasonLink)
	_, err = d.Child("file")
	wantRefusal(t, err, ReasonNotDirectory)
	if got := tree(t, base+"/in"); len(got) != 4 {
		t.Errorf("a rejected name created something: %v", got)
	}
}

func TestOpenRegularRefusals(t *testing.T) {
	base := isolate(t)
	in := base + "/in"
	mkdirs(t, in+"/dir")
	put(t, in+"/plain", "hello", 0o644)
	put(t, in+"/twice", "x", 0o644)
	put(t, in+"/setuid", "x", 0o644)
	must(t, unix.Mkfifo(in+"/fifo", 0o644))
	must(t, os.Link(in+"/twice", in+"/twin"))
	must(t, os.Chmod(in+"/setuid", fs.ModeSetuid|0o644))
	must(t, os.Symlink("plain", in+"/link"))
	must(t, os.Symlink("missing", in+"/dangling"))
	d := openDir(t, in)
	for name, reason := range map[string]Reason{
		"fifo": ReasonNotRegular, "dir": ReasonNotRegular, "link": ReasonLink, "dangling": ReasonLink,
		"twice": ReasonHardLinked, "twin": ReasonHardLinked, "setuid": ReasonSetID,
	} {
		f, _, err := d.OpenRegular(name)
		if err == nil {
			f.Close()
		}
		wantRefusal(t, err, reason)
	}
	// What a swap after the type check could open is judged on the descriptor: the same decision, called directly.
	for name, reason := range map[string]Reason{"fifo": ReasonNotRegular, "twice": ReasonHardLinked, "setuid": ReasonSetID} {
		fi, err := os.Lstat(in + "/" + name)
		must(t, err)
		wantRefusal(t, judgeOpened(name, fi), reason)
	}
	if _, _, err := d.OpenRegular("absent"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("absent file: %v, want ErrNotExist", err)
	}
	f, info, err := d.OpenRegular("plain")
	must(t, err)
	defer f.Close()
	if info.Size() != 5 || !info.Mode().IsRegular() {
		t.Errorf("info = %v", info)
	}
}

// Two spellings of one directory (a case-insensitive volume, a bind mount) pass the lexical check; file identity does not.
func TestOverlapByIdentity(t *testing.T) {
	// tree builds a pinned root: own is its inode (0 when the last component is absent) and chain the inodes down to it.
	tree := func(path string, own uint64, chain ...uint64) pinned {
		p := pinned{path: path}
		for _, n := range chain {
			p.chain = append(p.chain, fileID{1, n})
		}
		if own != 0 {
			p.dir = &Dir{path: path, id: fileID{1, own}}
		}
		return p
	}
	state, alias := tree("/x/State", 5, 1, 5), tree("/x/state", 5, 1, 5)
	for name, c := range map[string]struct {
		trees   []pinned
		codex   *pinned
		overlap bool
	}{
		"another spelling of the same directory":          {[]pinned{state, alias}, nil, true},
		"destination absent below an alias of the source": {[]pinned{state, tree("/x/state/new", 0, 1, 5)}, nil, true},
		"source below an alias of the destination":        {[]pinned{tree("/x/state/old", 7, 1, 5, 7), state}, nil, true},
		"two absent roots spelled alike":                  {[]pinned{tree("/x/.crw", 0, 1, 5), tree("/x/.CRW", 0, 1, 5)}, nil, true},
		"absent codex home spelled like an absent tree":   {[]pinned{tree("/x/.crw", 0, 1, 5)}, func() *pinned { c := tree("/x/.CRW", 0, 1, 5); return &c }(), true},
		"two absent roots with different names":           {[]pinned{tree("/x/.crw", 0, 1, 5), tree("/x/.other", 0, 1, 5)}, nil, false},
		"two absent roots below different directories":    {[]pinned{tree("/x/.crw", 0, 1, 5), tree("/y/.CRW", 0, 1, 6)}, nil, false},
		"disjoint trees":                         {[]pinned{state, tree("/x/other", 6, 1, 6)}, nil, false},
		"codex home below a tree":                {[]pinned{state}, &pinned{path: "/x/state/c", dir: &Dir{id: fileID{1, 8}}, chain: tree("", 8, 1, 5, 8).chain}, true},
		"a tree below the codex home is allowed": {[]pinned{tree("/h/.codex/d", 9, 1, 4, 9)}, func() *pinned { c := tree("/h/.codex", 4, 1, 4); return &c }(), false},
	} {
		err := overlapByIdentity(c.trees, c.codex)
		if c.overlap {
			wantRefusal(t, err, ReasonOverlap)
		} else if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The identities Open compares are the real device and inode of each directory on the way, root first.
	dir := isolate(t) + "/real"
	mkdirs(t, dir)
	d, chain, err := pinDir(dir)
	must(t, err)
	defer d.Close()
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	if len(chain) != len(parts)+1 || chain[len(chain)-1] != d.id {
		t.Fatalf("chain %v for %s (directory id %v)", chain, dir, d.id)
	}
	for i := range chain { // "/", then each prefix of the path
		fi, err := os.Stat("/" + strings.Join(parts[:i], "/"))
		must(t, err)
		if st := fi.Sys().(*syscall.Stat_t); chain[i] != (fileID{uint64(st.Dev), uint64(st.Ino)}) {
			t.Errorf("chain[%d] = %v, os.Stat of that prefix says %v/%v", i, chain[i], st.Dev, st.Ino)
		}
	}
}
