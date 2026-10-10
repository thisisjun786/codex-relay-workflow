package role

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// fakeOcx writes an ocx executable into dir that prints a roster holding the one given ID.
func fakeOcx(t *testing.T, dir, id string) {
	t.Helper()
	check(t, os.MkdirAll(dir, 0700))
	check(t, os.WriteFile(filepath.Join(dir, "ocx"), []byte("#!/bin/sh\nprintf '%s' '[{\"namespaced\":\""+id+"\"}]'\n"), 0700))
}

// symlinkTraversal is two process directories, root/one and root/two, each holding a link whose target
// differs, so that link/../../X names root/X from one and root/other/X from two, while a lexical cleaning
// of either directory plus link/../../X names root/X.
func symlinkTraversal(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"one", "two", "target/sub", "other/target/sub"} {
		check(t, os.MkdirAll(filepath.Join(root, dir), 0700))
	}
	check(t, os.Symlink(filepath.Join(root, "target/sub"), filepath.Join(root, "one/link")))
	check(t, os.Symlink(filepath.Join(root, "other/target/sub"), filepath.Join(root, "two/link")))
	return root
}

// CRW-1132: a relative source path is the file the kernel opens from the process directory, symbolic
// links and ".." included; the key does not clean it lexically into another file's name, so the same
// text resolving to two files is two sources, for the native catalog and for the OCX found on PATH.
func TestLiveCatalogRelativeSourceKeepsSymlinkTraversal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symbolic links and fake executable")
	}
	t.Run("native-catalog", func(t *testing.T) {
		root := symlinkTraversal(t)
		check(t, os.WriteFile(filepath.Join(root, "models.json"), []byte(`{"models":["one"]}`), 0600))
		check(t, os.WriteFile(filepath.Join(root, "other/models.json"), []byte(`{"models":["two"]}`), 0600))
		o := liveOptions(t)
		o.Environ = append(o.Environ, "CODEX_MODELS_CACHE_PATH=link/../../models.json", "OPENCODEX_HOME="+root)
		var r CatalogReader
		for i, want := range []string{"one", "two", "one"} {
			t.Chdir(filepath.Join(root, []string{"one", "two"}[i%2]))
			if got := liveRead(t, &r, o); got.Status != "fresh" || len(got.Entries) != 1 || got.Entries[0].ID != want {
				t.Fatalf("read %d answered %+v, want the list %q its directory reads", i, got, want)
			}
		}
	})
	t.Run("ocx-on-path", func(t *testing.T) {
		root := symlinkTraversal(t)
		fakeOcx(t, filepath.Join(root, "bin"), "one")
		fakeOcx(t, filepath.Join(root, "other/bin"), "two")
		o := liveOptions(t)
		o.Environ = append(o.Environ, "PATH=link/../../bin", "OPENCODEX_HOME="+root)
		var r CatalogReader
		for i, want := range []string{"one", "two", "one"} {
			t.Chdir(filepath.Join(root, []string{"one", "two"}[i%2]))
			if got := liveRead(t, &r, o); got.Status != "fresh" || got.Source != ModelOcx || len(got.Entries) != 1 || got.Entries[0].ID != want {
				t.Fatalf("read %d answered %+v, want the roster %q of the ocx its directory finds", i, got, want)
			}
		}
	})
}

// CRW-1132: one request resolves its source once. The native catalog it reads and the OCX it runs are
// the ones its key names, even when the configuration or PATH contents change while it discovers, so a
// list of another source is never stored under this key.
func TestLiveCatalogDiscoveryUsesTheSourceItKeyed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake executable")
	}
	t.Run("native-config-changes-during-discovery", func(t *testing.T) {
		o := liveOptions(t)
		codex, _ := catalogEnv(o.Environ)("CODEX_HOME")
		check(t, os.MkdirAll(codex, 0700))
		check(t, os.WriteFile(filepath.Join(codex, "a.json"), []byte(`{"models":["source-a"]}`), 0600))
		check(t, os.WriteFile(filepath.Join(codex, "b.json"), []byte(`{"models":["source-b"]}`), 0600))
		config := filepath.Join(codex, "config.toml")
		check(t, os.WriteFile(config, []byte(`model_catalog_json = 'a.json'`), 0600))
		calls := 0
		o.RunOcx = func([]string) (string, error) {
			calls++
			check(t, os.WriteFile(config, []byte(`model_catalog_json = 'b.json'`), 0600))
			return "", os.ErrNotExist
		}
		var r CatalogReader
		if got := liveRead(t, &r, o); len(got.Entries) != 1 || got.Entries[0].ID != "source-a" {
			t.Fatalf("the request keyed by a.json answered %+v", got)
		}
		check(t, os.WriteFile(config, []byte(`model_catalog_json = 'a.json'`), 0600))
		o.RunOcx = func([]string) (string, error) { calls++; return "", os.ErrNotExist }
		if got := liveRead(t, &r, o); len(got.Entries) != 1 || got.Entries[0].ID != "source-a" {
			t.Fatalf("a.json answered %+v after %d discovery runs: another file's list was stored under its key", got, calls)
		}
	})
	t.Run("ocx-on-path-changes-during-discovery", func(t *testing.T) {
		root := t.TempDir()
		first, second := filepath.Join(root, "a"), filepath.Join(root, "b")
		fakeOcx(t, first, "ocx-a")
		fakeOcx(t, second, "ocx-b")
		o := liveOptions(t)
		o.Environ = append(o.Environ, "PATH="+first+":"+second, "OPENCODEX_HOME="+root)
		// Now is read after the key and before discovery: making the first ocx unusable there moves the
		// PATH search to the second one while this request runs.
		clock := o.Now
		changed := false
		o.Now = func() time.Time {
			if !changed {
				changed = true
				check(t, os.Chmod(filepath.Join(first, "ocx"), 0600))
			}
			return clock()
		}
		var r CatalogReader
		if got := liveRead(t, &r, o); len(got.Entries) == 1 && got.Entries[0].ID == "ocx-b" {
			t.Fatalf("the request keyed by %s ran the ocx in %s: %+v", first, second, got)
		}
		check(t, os.Chmod(filepath.Join(first, "ocx"), 0700))
		o.Now = clock
		if got := liveRead(t, &r, o); got.Status != "fresh" || len(got.Entries) != 1 || got.Entries[0].ID != "ocx-a" {
			t.Fatalf("the ocx in %s answered %+v: another executable's roster was stored under its key", first, got)
		}
	})
}
