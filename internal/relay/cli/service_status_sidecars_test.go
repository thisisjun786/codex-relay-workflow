package cli_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-837. `service status` is one of the reads the install swap gate makes of the relay before it lists the state directory for a backup, and its store reads
// (the projects, the worker policy, the store identity) used to open the store with a plain mode=ro connection, which creates relay.sqlite3-shm and an empty
// relay.sqlite3-wal in a directory that held neither. It reads under the Stop path's no-sidecar rule now: immutable=1 where no log holds a frame.

func stateEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestServiceStatusReadsACleanStoreWithoutLeavingSidecars(t *testing.T) {
	home := tempHome(t)
	state := filepath.Join(home, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	testsupport.Create(t, filepath.Join(state, "relay.sqlite3"), "", "go")
	for _, name := range stateEntries(t, state) {
		if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			t.Fatalf("the fixture left %s: the premise of a clean store does not hold", name)
		}
	}
	before := stateEntries(t, state)
	answer := golang(t, home, "--state", state, "service", "status")
	if answer.code != 0 {
		t.Fatal(answer)
	}
	if after := stateEntries(t, state); strings.Join(after, " ") != strings.Join(before, " ") {
		t.Fatalf("service status changed the state directory:\nbefore %v\nafter  %v", before, after)
	}
}
