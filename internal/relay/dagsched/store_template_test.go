package dagsched

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// goexitTB is a testing.TB whose Fatalf ends the calling goroutine the way a failing test's does, without failing the real test.
type goexitTB struct {
	testing.TB
}

func (goexitTB) Helper()               {}
func (goexitTB) Name() string          { return "TestFirstFixture" }
func (goexitTB) Fatalf(string, ...any) { runtime.Goexit() }

// A template whose first build fails must hand every later caller that failure, not the empty directory with a nil error.
func TestStoreTemplateBuildFailureIsRecordedForLaterCallers(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "store-template") // a directory under a regular file: Create cannot make the store
	var b storeTemplateBuild
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.get(goexitTB{t}, dir) // Goexits at Create
	}()
	<-done
	if err := b.get(t, dir); err == nil {
		t.Fatal("a later caller got a nil error after the first build failed")
	}
}

// A copy of the template is made only where nothing exists: a populated database, a symlink to one, or a directory another store owns
// (its mirror or write gate) is refused untouched, the way testsupport.Create refuses, and a free path gets a store that opens.
func TestInstallStoreTemplateRefusesToInitializeOverAnExistingStore(t *testing.T) {
	t.Parallel()
	tmpl := filepath.Join(t.TempDir(), "store-template")
	var b storeTemplateBuild
	if err := b.get(t, tmpl); err != nil {
		t.Fatal(err)
	}
	keep := []byte("another store's bytes")
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, keep, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unchanged := func(path string) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, keep) {
			t.Fatalf("%s was changed: %q, %v", path, got, err)
		}
	}
	t.Run("populated database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		write(path)
		if err := installStoreTemplate(tmpl, path); err == nil {
			t.Fatal("a populated database was initialized over")
		}
		unchanged(path)
	})
	t.Run("symlink to a database", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "elsewhere", "relay.sqlite3")
		write(real)
		path := filepath.Join(dir, "link", "relay.sqlite3")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, path); err != nil {
			t.Fatal(err)
		}
		if err := installStoreTemplate(tmpl, path); err == nil {
			t.Fatal("a symlink to a database was initialized over")
		}
		unchanged(real)
	})
	for _, name := range []string{"takeover.json", "write-gate.lock", "takeover.lock"} {
		t.Run("occupied "+name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "relay.sqlite3")
			occupied := filepath.Join(filepath.Dir(path), name)
			write(occupied)
			if err := installStoreTemplate(tmpl, path); err == nil {
				t.Fatalf("a directory with %s was initialized over", name)
			}
			unchanged(occupied)
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("the database was written beside an occupied %s: %v", name, err)
			}
		})
	}
	t.Run("free path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sub", "relay.sqlite3")
		if err := installStoreTemplate(tmpl, path); err != nil {
			t.Fatal(err)
		}
		s, err := store.Open(context.Background(), path, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	})
}
