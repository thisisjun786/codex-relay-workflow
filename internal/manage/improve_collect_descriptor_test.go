package manage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestImproveSqliteOpensADescriptorPathByItsName is the premise of the CRW-1050 decision to keep a store's
// protection on the identity comparison: SQLite cannot be handed a descriptor. The only way to name one is
// /proc/self/fd/N, which SQLite resolves back to the name the file has now (readlink) and opens by that
// name, looking for the write-ahead log and the shared-memory index beside it. A descriptor of a file whose
// name is gone therefore cannot be opened at all, and one whose name moved is opened at the new name, so
// the descriptor pins nothing that the name does not. If this test starts to fail, SQLite pins the inode
// itself and the decision can be taken again.
func TestImproveSqliteOpensADescriptorPathByItsName(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc/self/fd names a descriptor on Linux only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	seed, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec("CREATE TABLE t(v TEXT); INSERT INTO t VALUES('pinned')"); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	descriptor := fmt.Sprintf("/proc/self/fd/%d", file.Fd())
	if _, err := os.Stat(descriptor); err != nil {
		t.Skipf("/proc/self/fd is not available here: %v", err)
	}
	read := func() (string, error) {
		db, err := sql.Open("sqlite", "file:"+descriptor+"?mode=ro&immutable=1")
		if err != nil {
			return "", err
		}
		defer func() { _ = db.Close() }()
		var v string
		err = db.QueryRow("SELECT v FROM t").Scan(&v)
		return v, err
	}

	moved := path + ".moved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(descriptor); err != nil || target != moved {
		t.Skipf("the descriptor does not name the moved file here: %q %v", target, err)
	}
	if v, err := read(); err != nil || v != "pinned" {
		t.Fatalf("a moved file read through its descriptor: %q %v, want the rows at its new name", v, err)
	}

	if err := os.Remove(moved); err != nil {
		t.Fatal(err)
	}
	// The kernel would still open the inode through the magic link; SQLite does not, because it opens the
	// name the link resolves to.
	if v, err := read(); err == nil || !strings.Contains(err.Error(), "unable to open database file") {
		t.Errorf("a deleted file read through its descriptor: %q %v, want SQLite to fail to open it by name", v, err)
	}
}
