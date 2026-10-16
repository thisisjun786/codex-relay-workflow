package gitprobe

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// gitMeta makes a Git directory at dir: the regular HEAD and objects directory a marker must resolve to.
func gitMeta(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// mk makes dir (and what is above it) and answers it.
func mk(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// put writes text at path (making the directory above it) and answers the path.
func put(t *testing.T, path, text string) string {
	t.Helper()
	mk(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveGitDir(t *testing.T) {
	base := t.TempDir()
	valid := gitMeta(t, filepath.Join(base, "meta"))

	// A linked worktree's Git directory: its own HEAD, its objects reached through commondir.
	common := gitMeta(t, filepath.Join(base, "common"))
	linked := mk(t, filepath.Join(base, "linked-meta"))
	put(t, filepath.Join(linked, "HEAD"), "ref: refs/heads/wt\n")
	put(t, filepath.Join(linked, "commondir"), "../"+filepath.Base(common)+"\n")

	// Marker entries that are links: one to a valid directory, one to a gitfile.
	linkDir := filepath.Join(base, "linkdir")
	if err := os.Symlink(valid, linkDir); err != nil {
		t.Fatal(err)
	}
	gitfile := put(t, filepath.Join(base, "pointer", ".git"), "gitdir: "+valid+"\n")
	linkFile := filepath.Join(base, "linkfile")
	if err := os.Symlink(gitfile, linkFile); err != nil {
		t.Fatal(err)
	}

	// A relative target whose ".." the kernel resolves behind a link: probe/.git names
	// link/../repo/meta, where link leads into other, so the kernel reads other/repo/meta
	// (a lexical join would name probe/repo/meta instead).
	probe := mk(t, filepath.Join(base, "probe"))
	gitMeta(t, mk(t, filepath.Join(base, "other", "repo", "meta")))
	if err := os.Symlink(mk(t, filepath.Join(base, "other", "dir")), filepath.Join(probe, "link")); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(probe, ".git"), "gitdir: link/../repo/meta\n")

	// A marker directory whose HEAD is a directory; one whose objects is a regular file.
	headIsDir := gitMeta(t, mk(t, filepath.Join(base, "headdir", ".git")))
	if err := os.Remove(filepath.Join(headIsDir, "HEAD")); err != nil {
		t.Fatal(err)
	}
	mk(t, filepath.Join(headIsDir, "HEAD"))
	objIsFile := mk(t, filepath.Join(base, "objfile", ".git"))
	put(t, filepath.Join(objIsFile, "HEAD"), "ref: refs/heads/main\n")
	put(t, filepath.Join(objIsFile, "objects"), "x")

	rel, err := filepath.Rel(filepath.Join(base, "rel"), valid)
	if err != nil {
		t.Fatal(err)
	}

	// A marker directory with an objects directory but no HEAD.
	noHead := mk(t, filepath.Join(base, "nohead", ".git"))
	mk(t, filepath.Join(noHead, "objects"))

	// Marker directories with a commondir: one naming a missing target, one empty, one a directory.
	noTarget := gitMeta(t, mk(t, filepath.Join(base, "nocmn", ".git")))
	put(t, filepath.Join(noTarget, "commondir"), "../gone\n")
	emptyTarget := gitMeta(t, mk(t, filepath.Join(base, "emptycmn", ".git")))
	put(t, filepath.Join(emptyTarget, "commondir"), "\n")
	dirTarget := gitMeta(t, mk(t, filepath.Join(base, "dircmn", ".git")))
	mk(t, filepath.Join(dirTarget, "commondir"))

	for _, c := range []struct {
		name   string
		marker string
		want   string
	}{
		{"absent", filepath.Join(base, "absent", ".git"), ""},
		{"absent through a file", filepath.Join(gitfile, ".git"), ""},
		{"empty directory", mk(t, filepath.Join(base, "empty", ".git")), ""},
		{"garbage file", put(t, filepath.Join(base, "garbage", ".git"), "not a gitfile\n"), ""},
		{"gitfile without the prefix", put(t, filepath.Join(base, "noprefix", ".git"), "gitdir:/x\n"), ""},
		{"gitfile with an empty target", put(t, filepath.Join(base, "blank", ".git"), "gitdir: \n"), ""},
		{"HEAD missing", noHead, ""},
		{"HEAD a directory", headIsDir, ""},
		{"objects a regular file", objIsFile, ""},
		{"HEAD and objects", gitMeta(t, mk(t, filepath.Join(base, "plain", ".git"))), filepath.Join(base, "plain", ".git")},
		{"a gitdir holding commondir", linked, linked},
		{"absolute gitfile", gitfile, valid},
		{"relative gitfile", put(t, filepath.Join(base, "rel", ".git"), "gitdir: "+rel+"\n"), filepath.Join(base, "rel") + "/" + rel},
		{"gitfile to an empty directory", put(t, filepath.Join(base, "toempty", ".git"), "gitdir: "+mk(t, filepath.Join(base, "emptytarget"))+"\n"), ""},
		{"gitfile to a missing directory", put(t, filepath.Join(base, "tomissing", ".git"), "gitdir: "+filepath.Join(base, "missing")+"\n"), ""},
		{"symlink to a directory marker", linkDir, linkDir},
		{"symlink to a gitfile", linkFile, valid},
		{"linked worktree gitfile", put(t, filepath.Join(base, "wt", ".git"), "gitdir: "+linked+"\n"), linked},
		{"commondir target missing", noTarget, ""},
		{"commondir empty", emptyTarget, ""},
		{"commondir a directory", dirTarget, ""},
		{"relative target behind a link", filepath.Join(probe, ".git"), probe + "/link/../repo/meta"},
	} {
		got, err := ResolveGitDir(c.marker)
		if err != nil {
			t.Errorf("%s: ResolveGitDir(%s): %v", c.name, c.marker, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: ResolveGitDir(%s) = %q, want %q", c.name, c.marker, got, c.want)
		}
	}
}

// A marker that is a FIFO, or whose commondir is one, is not a repository and is never read: both are
// answered without opening the pipe.
func TestResolveGitDirNeverReadsAFifo(t *testing.T) {
	base := t.TempDir()
	fifoMarker := filepath.Join(base, "fifo", ".git")
	mk(t, filepath.Dir(fifoMarker))
	if err := syscall.Mkfifo(fifoMarker, 0o644); err != nil {
		t.Fatal(err)
	}
	fifoCommon := gitMeta(t, mk(t, filepath.Join(base, "fifocmn", ".git")))
	if err := syscall.Mkfifo(filepath.Join(fifoCommon, "commondir"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{fifoMarker, fifoCommon} {
		done := make(chan string, 1)
		go func() {
			got, err := ResolveGitDir(marker)
			if err != nil {
				t.Errorf("ResolveGitDir(%s): %v", marker, err)
			}
			done <- got
		}()
		select {
		case got := <-done:
			if got != "" {
				t.Errorf("ResolveGitDir(%s) = %q, want none", marker, got)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("ResolveGitDir(%s) blocked on a FIFO", marker)
		}
	}
}

func TestRepositoryAbove(t *testing.T) {
	base := t.TempDir()
	outer := gitMeta(t, filepath.Join(base, "outer", ".git"))
	inner := filepath.Join(base, "outer", "inner")
	mk(t, filepath.Join(inner, ".git"))
	deep := mk(t, filepath.Join(inner, "deep"))
	bare := gitMeta(t, filepath.Join(base, "bare"))
	work := mk(t, filepath.Join(bare, "work"))
	want := filepath.Dir(outer)

	t.Run("an inner empty marker is skipped for the outer repository", func(t *testing.T) {
		got, err := RepositoryAbove(deep, RepositorySearch{})
		if err != nil || got != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	})
	t.Run("a ceiling excludes itself and above", func(t *testing.T) {
		for _, ceilings := range [][]string{{want}, {inner}} {
			got, err := RepositoryAbove(deep, RepositorySearch{Ceilings: ceilings})
			if err != nil || got != "" {
				t.Fatalf("ceilings %v: got %q, %v; want none", ceilings, got, err)
			}
		}
		got, err := RepositoryAbove(deep, RepositorySearch{Ceilings: []string{filepath.Dir(want)}})
		if err != nil || got != want {
			t.Fatalf("a ceiling above the repository keeps it: got %q, %v; want %q", got, err, want)
		}
	})
	t.Run("a bare repository answers only when asked", func(t *testing.T) {
		got, err := RepositoryAbove(work, RepositorySearch{})
		if err != nil || got != "" {
			t.Fatalf("without IncludeBare: got %q, %v; want none", got, err)
		}
		got, err = RepositoryAbove(work, RepositorySearch{IncludeBare: true})
		if err != nil || got != bare {
			t.Fatalf("with IncludeBare: got %q, %v; want %q", got, err, bare)
		}
	})
	t.Run("an unreadable marker is an error, not a repository", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads through the permission bits")
		}
		jail := mk(t, filepath.Join(base, "jail"))
		if err := os.Chmod(jail, 0o000); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(jail, 0o755) }()
		got, err := RepositoryAbove(filepath.Join(jail, "inside"), RepositorySearch{})
		if err == nil {
			t.Fatalf("an unreadable ancestor answered %q without error", got)
		}
	})
}

// A real clone and a real linked worktree answer through their .git as Git lays them out, and a real
// bare repository only when asked — under whatever GIT_* variables the caller's environment carries.
func TestRepositoryAboveRealGitLayouts(t *testing.T) {
	base := t.TempDir()
	run := func(cwd string, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), Timeout)
		defer cancel()
		cmd := Command(ctx, cwd, nil, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, cwd, err, out)
		}
	}
	clone := mk(t, filepath.Join(base, "clone"))
	run(clone, "init", "-q", "-b", "main")
	run(clone, "config", "user.email", "probe@example.invalid")
	run(clone, "config", "user.name", "probe")
	put(t, filepath.Join(clone, "tracked"), "x\n")
	run(clone, "add", "tracked")
	run(clone, "commit", "-qm", "base")
	wt := filepath.Join(base, "worktree")
	run(clone, "worktree", "add", "-q", "--detach", wt)
	bare := mk(t, filepath.Join(base, "bare.git"))
	run(bare, "init", "-q", "--bare", "-b", "main")

	for _, dir := range []string{clone, wt} {
		got, err := RepositoryAbove(dir, RepositorySearch{})
		if err != nil || got != dir {
			t.Errorf("below %s: got %q, %v; want %q", dir, got, err, dir)
		}
	}
	if got, err := RepositoryAbove(mk(t, filepath.Join(wt, "sub", "deep")), RepositorySearch{}); err != nil || got != wt {
		t.Errorf("below the linked worktree: got %q, %v; want %q", got, err, wt)
	}
	if got, err := RepositoryAbove(filepath.Join(bare, "objects"), RepositorySearch{}); err != nil || got != "" {
		t.Errorf("a bare repository without IncludeBare: got %q, %v", got, err)
	}
	if got, err := RepositoryAbove(filepath.Join(bare, "objects"), RepositorySearch{IncludeBare: true}); err != nil || got != bare {
		t.Errorf("a bare repository with IncludeBare: got %q, %v; want %q", got, err, bare)
	}
}
