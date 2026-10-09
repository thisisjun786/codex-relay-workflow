package mergeturn

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// A store this package's tests open is a Go-owned, fully initialised relay store: the absent-store
// initializer ran its schema script, the DAG zone and the seed rows, one fsynced transaction after
// another (synchronous=FULL), which is the slow part of every test on a loaded host (CRW-1054: about
// 4 s per store with the disk busy, 260 stores in one package run). The initialisation runs once per test
// process, into a template; every test then gets a byte copy of the template, rehomed so that its
// ownership mirror names the copy. The copy is the very store the initializer would have produced: Open
// runs the same schema script over it and finds nothing to create. A test of the absent-store
// initializer itself does not use this and calls store.Open on an absent path.
var (
	// templateRoot is the process's isolation root (testsupport.Main), which the template is built under and
	// which Main removes when the test process ends.
	templateRoot string
	templateOnce sync.Once
	templateDB   string
	templateErr  error
)

// freshStorePath returns the path of a new, closed, fully initialised Go-owned store in the test's own
// temporary directory.
func freshStorePath(t *testing.T) string {
	t.Helper()
	if templateRoot == "" {
		t.Fatal("the store template has no isolation root: TestMain must run testsupport.Main with setTemplateRoot")
	}
	templateOnce.Do(func() {
		dir, err := os.MkdirTemp(templateRoot, "mergeturn-store-template-")
		if err != nil {
			templateErr = err
			return
		}
		path := filepath.Join(dir, "relay.sqlite3")
		testsupport.Create(t, path, "", "go")
		s, err := store.Open(context.Background(), path, "")
		if err != nil {
			templateErr = err
			return
		}
		if err = s.Close(); err != nil {
			templateErr = err
			return
		}
		templateDB = path
	})
	if templateErr != nil {
		t.Fatal(templateErr)
	}
	if templateDB == "" {
		t.Fatal("the store template was not built")
	}
	dst := filepath.Join(t.TempDir(), "relay.sqlite3")
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(templateDB + suffix)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(dst+suffix, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	testsupport.Rehome(t, dst)
	return dst
}

// openFresh opens a fresh store (freshStorePath) writably and closes it with the test.
func openFresh(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), freshStorePath(t), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// setTemplateRoot is the testsupport.Setup that gives the template its home, inside the root Main removes.
func setTemplateRoot(root string) (func() error, error) {
	templateRoot = root
	return nil, nil
}
