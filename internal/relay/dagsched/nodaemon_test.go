package dagsched

import (
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

const modulePrefix = "github.com/thisisjun786/codex-relay-workflow/"

// Criterion c9 by construction: the scheduler adds no daemon, no listener and no database. It is a library the relay's commands call; the goal-free parent runs a command, acts on the answer and ends
// its turn. This follows every package the scheduler imports inside the module and refuses the daemon and the service, and the store a whole fork and join leaves is the relay's one database
// and its bridge files.
func TestNoNewStoreOrDaemon(t *testing.T) {
	seen := map[string]bool{}
	var walk func(path, dir string)
	walk = func(path, dir string) {
		if seen[path] {
			return
		}
		seen[path] = true
		pkg, err := build.Default.Import(path, dir, 0)
		if err != nil {
			t.Fatalf("import %s: %v", path, err)
		}
		for _, imp := range pkg.Imports {
			if strings.HasPrefix(imp, modulePrefix) {
				walk(imp, pkg.Dir)
			}
		}
	}
	walk(modulePrefix+"internal/relay/dagsched", ".")
	for path := range seen {
		for _, banned := range []string{"internal/relay/daemon", "internal/relay/service", "internal/relay/supervisor"} {
			if strings.HasPrefix(path, modulePrefix+banned) {
				t.Errorf("the scheduler reaches %s: a daemon or a service must not be part of it", path)
			}
		}
	}
	// the product package imports no network server of its own either
	pkg, err := build.Default.Import(modulePrefix+"internal/relay/dagsched", ".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		if imp == "net" || imp == "net/http" || imp == "database/sql/driver" {
			t.Errorf("dagsched imports %s", imp)
		}
	}
}

// The state directory a fork and join leaves holds the relay's database and the bridge's own files, and no database or socket of the scheduler's.
func TestForkJoinLeavesNoStoreOfItsOwn(t *testing.T) {
	f := newForkJoin(t)
	f.putPlan("fj", 0, "fj-r1", addRelNode("A", dag.NodeNonPR))
	f.release("fj", "A")
	entries, err := os.ReadDir(f.state)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		// the relay's store, and the bridge's own operations ledger that the managed engine already used before this scheduler existed
		if strings.HasSuffix(name, ".sqlite3") && name != "relay.sqlite3" && !strings.HasPrefix(name, "operations-") {
			t.Errorf("a database of the scheduler's own: %s", name)
		}
		if e.Type()&os.ModeSocket != 0 {
			t.Errorf("a socket %s was created in the state directory", name)
		}
		if strings.Contains(strings.ToLower(name), "dag") || strings.Contains(strings.ToLower(name), "sched") {
			t.Errorf("the scheduler left %s in the state directory", filepath.Join(f.state, name))
		}
	}
}
