package cli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The old live-Python parity corpus predates the fence. Provision stopped
// synthetic stores explicitly in its harness, never in a product opener.
func fenceExisting(t *testing.T, path string) {
	t.Helper()
	if info, e := os.Stat(path); e == nil && info.Size() > 0 {
		if e = testsupport.SeedOwnership(context.Background(), path, "", "go"); e != nil {
			t.Fatal(e)
		}
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
}
func fenceArgs(t *testing.T, args []string) {
	t.Helper()
	if len(args) > 0 && args[0] == "relay" {
		args = args[1:]
	}
	r := argparse.Parse("", args)
	if r.Message != "" || r.Help || len(r.Remaining) == 0 {
		return
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
		return
	}
	fenceExisting(t, s.DBPath())
}
func fenceTree(t *testing.T, root string) {
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
			fenceExisting(t, path)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
}
