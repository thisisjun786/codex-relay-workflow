package cli_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The live-Python parity tests let the two runtimes take turns on one store, which a host does
// only across a takeover: each runtime refuses a store the other owns. Before a runtime's turn
// the store it is about to use is therefore put into that runtime's ownership with
// testsupport.HandOver, the state a completed takeover leaves. An absent store is left for the
// runtime to create, and a test that copies a store Rehomes the copy itself.

// ownedBy hands the existing store at path to runtime ("python" or "go"). A store runtime already
// owns is left as it is; an absent or empty file is left for runtime to create.
func ownedBy(t *testing.T, path, runtime string) {
	t.Helper()
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) || err == nil && info.Size() == 0 {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	testsupport.HandOver(t, path, runtime)
}

// ownedArgs hands the store an invocation run in dir ("" for this process's directory) selects,
// by flag, environment or discovery, to runtime. An invocation that selects none (help, a usage
// error, an ambiguous or unresolvable selection) leaves every store as it is.
func ownedArgs(t *testing.T, dir string, args []string, runtime string) {
	t.Helper()
	if path := selectedDB(t, dir, args); path != "" {
		ownedBy(t, path, runtime)
	}
}

// selectedDB is the database an invocation run in dir ("" for this process's directory)
// selects, by flag, environment or discovery; "" when it selects none (help, a usage error, an
// ambiguous or unresolvable selection).
func selectedDB(t *testing.T, dir string, args []string) string {
	t.Helper()
	if dir != "" {
		previous, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err = os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.Chdir(previous); err != nil {
				t.Fatal(err)
			}
		}()
	}
	if len(args) > 0 && args[0] == "relay" {
		args = args[1:]
	}
	r := argparse.Parse("", args)
	if r.Message != "" || r.Help || len(r.Remaining) == 0 {
		return ""
	}
	state, socket := "", ""
	if v := r.Values["state"]; len(v) > 0 {
		state = v[0]
	}
	if v := r.Values["socket"]; len(v) > 0 {
		socket = v[0]
	}
	s, e := store.ResolveStateDir(state, socket)
	if e != nil {
		return ""
	}
	return s.DBPath()
}

// selectedStoreID is the store id of the fenced store an invocation selects, "" for none.
func selectedStoreID(t *testing.T, dir string, args []string) string {
	t.Helper()
	path := selectedDB(t, dir, args)
	if path == "" {
		return ""
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		return ""
	}
	stamp, err := ownership.SnapshotMeta(t.Context(), path)
	if err != nil {
		return ""
	}
	return stamp.StoreID
}

// ownedTree hands every store under root to runtime: the stores an oracle run left there.
func ownedTree(t *testing.T, root, runtime string) {
	t.Helper()
	e := filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
		if errors.Is(e, os.ErrNotExist) {
			return nil
		}
		if e != nil {
			return e
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".git") {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == "relay.sqlite3" {
			ownedBy(t, path, runtime)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
}
