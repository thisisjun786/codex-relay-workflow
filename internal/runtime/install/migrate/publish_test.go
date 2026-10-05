package migrate

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

func newPub(t *testing.T) *Publisher {
	t.Helper()
	if !noReplaceSupported {
		t.Skip("no no-replace rename on this platform")
	}
	p, err := NewPublisher()
	must(t, err)
	return p
}

func pub(p *Publisher, d *Dir, leaf, data string, mode fs.FileMode) (Result, error) {
	return p.Publish(d, leaf, strings.NewReader(data), int64(len(data)), mode)
}

// outDir is a fresh pinned destination directory.
func outDir(t *testing.T) (string, *Dir) {
	dir := isolate(t) + "/out"
	mkdirs(t, dir)
	return dir, openDir(t, dir)
}

func ls(t *testing.T, dir string) (names []string) {
	t.Helper()
	es, err := os.ReadDir(dir)
	must(t, err)
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

// fingerprint renders every entry of dir with its mode, modification time and, for a regular file, its bytes.
func fingerprint(t *testing.T, dir string) (out string) {
	t.Helper()
	for _, n := range ls(t, dir) {
		fi, err := os.Lstat(dir + "/" + n)
		must(t, err)
		data := ""
		if fi.Mode().IsRegular() {
			data = get(t, dir+"/"+n)
		}
		out += fmt.Sprintf("%s %v %v %q\n", n, fi.Mode(), fi.ModTime(), data)
	}
	return out
}

func TestPublishKeepsBytesAndMode(t *testing.T) {
	dir, d := outDir(t)
	p := newPub(t)
	for i, mode := range []fs.FileMode{0o644, 0o444, 0o755, 0o600} {
		leaf, data := "f"+strconv.Itoa(i), "data"+strconv.Itoa(i)
		res, err := pub(p, d, leaf, data, mode)
		fi, serr := os.Stat(dir + "/" + leaf)
		if err != nil || serr != nil || res != ResultCopied || fi.Mode().Perm() != mode || get(t, dir+"/"+leaf) != data {
			t.Errorf("%o: %v, %v, %v, %v", mode, res, err, serr, fi)
		}
	}
	for _, mode := range []fs.FileMode{fs.ModeSetuid | 0o755, fs.ModeSticky | 0o755, fs.ModeDir | 0o755} {
		if res, err := pub(p, d, "bad", "x", mode); err == nil || res != ResultFailed {
			t.Errorf("mode %v was accepted: %v, %v", mode, res, err)
		}
	}
	if got := ls(t, dir); len(got) != 4 {
		t.Errorf("directory = %v, want the four files and no temporary", got)
	}
}

func TestPublishNeverReplaces(t *testing.T) {
	for name, c := range map[string]struct {
		setup  func(dir string)
		res    Result
		reason Reason
	}{
		"different bytes":            {func(d string) { put(t, d+"/leaf", "old", 0o640) }, ResultRefused, ReasonDiffers},
		"different bytes, same size": {func(d string) { put(t, d+"/leaf", "datb", 0o640) }, ResultRefused, ReasonDiffers},
		"equal bytes, other mode":    {func(d string) { put(t, d+"/leaf", "data", 0o600) }, ResultAlreadyEqual, ""},
		"link to an equal file": {func(d string) {
			put(t, d+"/target", "data", 0o644)
			must(t, os.Symlink("target", d+"/leaf"))
		}, ResultRefused, ReasonLink},
		"directory": {func(d string) { mkdirs(t, d+"/leaf") }, ResultRefused, ReasonNotRegular},
		"hard-linked": {func(d string) {
			put(t, d+"/leaf", "data", 0o644)
			must(t, os.Link(d+"/leaf", d+"/twin"))
		}, ResultRefused, ReasonHardLinked},
	} {
		t.Run(name, func(t *testing.T) {
			dir, d := outDir(t)
			c.setup(dir)
			before := fingerprint(t, dir)
			res, err := pub(newPub(t), d, "leaf", "data", 0o644)
			if c.reason != "" {
				wantRefusal(t, err, c.reason)
			} else {
				must(t, err)
			}
			if after := fingerprint(t, dir); res != c.res || before != after {
				t.Errorf("result %q (want %q); directory before:\n%safter:\n%s", res, c.res, before, after)
			}
		})
	}
}

func TestPublishRacer(t *testing.T) {
	for name, c := range map[string]struct {
		racer  func(path string)
		res    Result
		reason Reason
		bytes  string
	}{
		"equal bytes":     {func(p string) { put(t, p, "data", 0o600) }, ResultAlreadyEqual, "", "data"},
		"different bytes": {func(p string) { put(t, p, "other", 0o600) }, ResultRefused, ReasonDiffers, "other"},
		"link":            {func(p string) { must(t, os.Symlink("elsewhere", p)) }, ResultRefused, ReasonLink, ""},
		"directory":       {func(p string) { mkdirs(t, p) }, ResultRefused, ReasonNotRegular, ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir, d := outDir(t)
			p := newPub(t)
			p.at = func(step string) error {
				if step == "rename" {
					c.racer(dir + "/leaf")
				}
				return nil
			}
			res, err := pub(p, d, "leaf", "data", 0o644)
			if c.reason != "" {
				wantRefusal(t, err, c.reason)
			} else {
				must(t, err)
			}
			if _, lerr := os.Lstat(dir + "/leaf"); res != c.res || lerr != nil || !slices.Equal(ls(t, dir), []string{"leaf"}) {
				t.Errorf("result %q (want %q), directory %v: the racer's file must stay and our temporary go", res, c.res, ls(t, dir))
			}
			if c.bytes != "" && get(t, dir+"/leaf") != c.bytes {
				t.Error("the racer's bytes changed")
			}
		})
	}
}

func TestPublishRenameFailures(t *testing.T) {
	dir, d := outDir(t)
	for name, c := range map[string]struct {
		err    error
		res    Result
		reason Reason
	}{
		"ENOTSUP": {unix.ENOTSUP, ResultRefused, ReasonUnsupported}, "ENOSYS": {unix.ENOSYS, ResultRefused, ReasonUnsupported},
		"EINVAL": {unix.EINVAL, ResultRefused, ReasonUnsupported}, "EIO": {unix.EIO, ResultFailed, ""},
		"EEXIST with nothing there": {unix.EEXIST, ResultFailed, ""},
	} {
		p := newPub(t)
		p.rename = func(int, string, string) error { return c.err }
		res, err := pub(p, d, "leaf", "data", 0o644)
		if c.reason != "" {
			wantRefusal(t, err, c.reason)
		} else if !errors.Is(err, c.err) {
			t.Errorf("%s: %v", name, err)
		}
		if res != c.res || len(ls(t, dir)) != 0 {
			t.Errorf("%s: result %q, directory %v; the leaf and our temporary must be absent", name, res, ls(t, dir))
		}
	}
	res, err := newPub(t).Publish(d, "leaf", strings.NewReader("abc"), 10, 0o644)
	if res != ResultFailed || !errors.Is(err, io.ErrUnexpectedEOF) || len(ls(t, dir)) != 0 {
		t.Errorf("short source: %q, %v, %v", res, err, ls(t, dir))
	}
}

func TestPublishInterruptedBeforeDirSync(t *testing.T) {
	dir, d := outDir(t)
	p, boom := newPub(t), errors.New("interrupted")
	p.at = func(step string) error {
		if step == "dirsync" {
			return boom
		}
		return nil
	}
	if res, err := pub(p, d, "leaf", "data", 0o444); res != ResultFailed || !errors.Is(err, boom) {
		t.Fatalf("interrupted publish: %q, %v", res, err)
	}
	if fi, err := os.Stat(dir + "/leaf"); err != nil || fi.Mode().Perm() != 0o444 || get(t, dir+"/leaf") != "data" {
		t.Fatalf("the final file is not whole: %v, %v", fi, err)
	}
	if res, err := pub(newPub(t), d, "leaf", "data", 0o444); err != nil || res != ResultAlreadyEqual || len(ls(t, dir)) != 1 {
		t.Errorf("rerun: %q, %v, %v", res, err, ls(t, dir))
	}
}

// After the rename the run's temporary name is free again, and a file found under it is not ours: nothing may be unlinked there.
func TestPublishLeavesTheTempNameAloneAfterTheRename(t *testing.T) {
	dir, d := outDir(t)
	p, planted := newPub(t), ""
	p.at = func(step string) error {
		if step == "dirsync" {
			planted = tempName(p.run, p.seq)
			put(t, dir+"/"+planted, "not ours", 0o600)
		}
		return nil
	}
	if res, err := pub(p, d, "leaf", "data", 0o644); err != nil || res != ResultCopied {
		t.Fatalf("publish: %q, %v", res, err)
	}
	if planted == "" || get(t, dir+"/"+planted) != "not ours" || get(t, dir+"/leaf") != "data" {
		t.Errorf("the file at the temporary name was removed, or the leaf is wrong: %v", ls(t, dir))
	}
}

func TestEnsureProjectRoot(t *testing.T) {
	boom := errors.New("interrupted")
	open := func(t *testing.T, ws string) *Pair {
		r, err := Open(Options{Scope: ScopeProject, Cwd: ws})
		must(t, err)
		t.Cleanup(func() { r.Close() })
		return r.Project
	}
	for name, c := range map[string]struct {
		setup  func(crw string)
		reason Reason
		want   string // the .gitignore afterwards
		names  []string
	}{
		"fresh workspace":          {func(string) {}, "", crwdir.GitignoreText, []string{".gitignore"}},
		"existing root with state": {func(crw string) { put(t, crw+"/sessions/s.json", "{}", 0o644) }, "", crwdir.GitignoreText, []string{".gitignore", "sessions"}},
		"existing regular ignore":  {func(crw string) { put(t, crw+"/.gitignore", "mine", 0o600) }, "", "mine", []string{".gitignore"}},
		"link as ignore":           {func(crw string) { mkdirs(t, crw); must(t, os.Symlink("elsewhere", crw+"/.gitignore")) }, ReasonLink, "", nil},
		"directory as ignore":      {func(crw string) { mkdirs(t, crw+"/.gitignore") }, ReasonNotRegular, "", nil},
		"hard-linked ignore": {func(crw string) {
			put(t, crw+"/.gitignore", "mine", 0o600)
			must(t, os.Link(crw+"/.gitignore", crw+"/twin"))
		}, ReasonHardLinked, "", nil},
	} {
		t.Run(name, func(t *testing.T) {
			ws := isolate(t) + "/ws"
			mkdirs(t, ws)
			c.setup(ws + "/.crw")
			root, err := newPub(t).EnsureProjectRoot(open(t, ws))
			if c.reason != "" {
				wantRefusal(t, err, c.reason)
				return
			}
			must(t, err)
			if root == nil || get(t, ws+"/.crw/.gitignore") != c.want || !slices.Equal(ls(t, ws+"/.crw"), c.names) {
				t.Errorf(".crw = %v, .gitignore %q", ls(t, ws+"/.crw"), get(t, ws+"/.crw/.gitignore"))
			}
		})
	}
	t.Run("unreadable ignore is an error, not a retained file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads any file")
		}
		ws := isolate(t) + "/ws"
		put(t, ws+"/.crw/.gitignore", "mine", 0o000)
		root, err := newPub(t).EnsureProjectRoot(open(t, ws))
		var refused *RefusedError
		if root != nil || err == nil || errors.As(err, &refused) {
			t.Errorf("unreadable ignore: %v, %v; want a plain failure and no root", root, err)
		}
		must(t, os.Chmod(ws+"/.crw/.gitignore", 0o600))
		if get(t, ws+"/.crw/.gitignore") != "mine" {
			t.Error("the unreadable ignore was changed")
		}
	})
	// A racer that makes the ignore between the look and the rename is judged like an existing one: a regular file of its own
	// stays (state-migration.md "retain it"; crwdir.EnsureDir keeps one a concurrent creator wrote first), a link is refused.
	for name, c := range map[string]struct {
		racer  func(path string)
		reason Reason
		want   string
	}{
		"different regular file": {func(p string) { put(t, p, "mine", 0o600) }, "", "mine"},
		"equal text":             {func(p string) { put(t, p, crwdir.GitignoreText, 0o600) }, "", crwdir.GitignoreText},
		"link":                   {func(p string) { must(t, os.Symlink("elsewhere", p)) }, ReasonLink, ""},
	} {
		t.Run("racer makes the ignore: "+name, func(t *testing.T) {
			ws := isolate(t) + "/ws"
			mkdirs(t, ws)
			p := newPub(t)
			p.at = func(step string) error {
				if step == "rename" {
					c.racer(ws + "/.crw/.gitignore")
				}
				return nil
			}
			root, err := p.EnsureProjectRoot(open(t, ws))
			if c.reason != "" {
				wantRefusal(t, err, c.reason)
				return
			}
			must(t, err)
			if root == nil || get(t, ws+"/.crw/.gitignore") != c.want || !slices.Equal(ls(t, ws+"/.crw"), []string{".gitignore"}) {
				t.Errorf(".crw = %v, .gitignore %q", ls(t, ws+"/.crw"), get(t, ws+"/.crw/.gitignore"))
			}
		})
	}
	t.Run("interrupted between the mkdir and the ignore", func(t *testing.T) {
		ws := isolate(t) + "/ws"
		mkdirs(t, ws)
		p := newPub(t)
		p.at = func(step string) error {
			if step == "root" {
				return boom
			}
			return nil
		}
		if root, err := p.EnsureProjectRoot(open(t, ws)); root != nil || !errors.Is(err, boom) {
			t.Fatalf("interrupted: %v, %v", root, err)
		}
		if got := ls(t, ws+"/.crw"); len(got) != 0 {
			t.Fatalf("the interrupted run left %v", got)
		}
		// The rerun finds the root, and publishes the absent .gitignore before it hands the root out.
		root, err := newPub(t).EnsureProjectRoot(open(t, ws))
		if err != nil || root == nil || get(t, ws+"/.crw/.gitignore") != crwdir.GitignoreText {
			t.Errorf("rerun: %v, %v", root, err)
		}
	})
}

func TestOlderTempsAreReportedNotTouched(t *testing.T) {
	dir, d := outDir(t)
	p := newPub(t)
	stale := []string{tempName(rand.Text(), 1), tempName(rand.Text(), 7)}
	slices.Sort(stale)
	others := []string{".migrate-foo.tmp", tempPrefix + strings.Repeat("A", 26) + "-x" + tempSuffix, tempPrefix + strings.Repeat("A", 25) + "-1" + tempSuffix, "user.tmp", tempName(p.run, 99)}
	for _, n := range append(slices.Clone(stale), others...) {
		put(t, dir+"/"+n, "partial", 0o600)
	}
	if res, err := pub(p, d, "leaf", "data", 0o644); err != nil || res != ResultCopied {
		t.Fatalf("publish beside older temporaries: %q, %v", res, err)
	}
	for range 2 {
		if got, err := p.OlderTemps(d); err != nil || !slices.Equal(got, stale) {
			t.Errorf("OlderTemps = %v, %v; want %v", got, err, stale)
		}
	}
	for _, n := range append(slices.Clone(stale), others...) {
		if get(t, dir+"/"+n) != "partial" {
			t.Errorf("%s was changed", n)
		}
	}
	if run, ok := tempRun(tempName(p.run, 3)); !ok || run != p.run || len(filepath.Base(tempName(p.run, 3))) > 255 {
		t.Error("tempRun does not read the names Publish writes")
	}
}
