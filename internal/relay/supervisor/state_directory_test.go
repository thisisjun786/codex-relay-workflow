package supervisor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The --state every line this channel writes names is the channel's state directory, Python's
// str(services.selection.path): the store's pathlib parent, which keeps a root of two leading
// slashes that filepath.Dir folds.
func TestTheChannelNamesItsStateDirectoryAsPathlibSpellsIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := store.Open(context.Background(), "/"+filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &Channel{Store: s, Program: "crw relay"}
	if got := c.StoreDirectory(); got != "/"+dir {
		t.Errorf("StoreDirectory() = %q, Python's str(Path(%q).parent) is %q", got, s.Path, "/"+dir)
	}
	if got := c.command("supervisor-show"); !strings.Contains(got, "--state /"+dir+" ") {
		t.Errorf("command names %q, not --state %s", got, "/"+dir)
	}
}
