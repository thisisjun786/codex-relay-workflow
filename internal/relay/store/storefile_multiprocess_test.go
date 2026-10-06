//go:build linux

package store

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// CRW-846: a POSIX (fcntl) lock is held per process and per file, so a process that holds a SQLite
// WAL connection on relay.sqlite3 loses that connection's lock the moment it opens and closes any
// other descriptor of the same file. The next process to close then deletes -wal and -shm under
// the live connection, and the rows it committed are gone.
//
// The primary assertion is the holder's own POSIX lock on the store's main inode, read from
// /proc/locks: it must survive every product path that reads the store. That assertion is red on
// the code this test was written against (holdDatabase and CopySnapshot each opened and closed the
// main file) and needs no external interpreter.
//
// The end-to-end layer runs a system-SQLite peer (python3) because a modernc-only peer does not
// reproduce the deletion at all: modernc's own open and close of the store file does not take the
// lock the same way, so the sidecars survive and no rows are lost even when the holder has dropped
// its lock. The peer is the configuration the issue's own reproduction used, and it is skipped
// with a recorded reason where python3 is absent.

// storeFileHolderEnv names the holder child process's store; its absence makes the test a no-op.
const storeFileHolderEnv = "CRW846_HOLDER_DB"

// storeFileHolderProcess is the child, not a test: it holds one store connection and runs the real
// product paths that read the store, then reports its own POSIX locks on the store's main inode.
func TestStoreFileHolderProcess(t *testing.T) {
	path := os.Getenv(storeFileHolderEnv)
	if path == "" {
		return
	}
	ctx := context.Background()
	selection := StateSelection{Path: filepath.Dir(path)}
	s, err := fixtureOpen(ctx, path, "")
	if err != nil {
		fmt.Println("holder-open:", err)
		return
	}
	defer func() { _ = s.Close() }()
	// Hold the connection: one statement pins the pool's single connection, which is what the
	// daemon does for its whole life.
	if _, err := s.Querier(ctx).ExecContext(ctx, "INSERT INTO store_challenge(nonce,written_by,written_at) VALUES('holder','holder','2026-01-01T00:00:00Z')"); err != nil {
		fmt.Println("holder-write:", err)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		fmt.Println("holder-stat:", err)
		return
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		fmt.Println("holder-stat-type")
		return
	}
	mainInode := uint64(stat.Ino)
	fmt.Printf("ready inode=%d\n", mainInode)

	writes := 0
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		switch sc.Text() {
		case "locks":
			fmt.Printf("locks=%d\n", storeFileLocks(os.Getpid(), mainInode))
		case "paths":
			runStoreFilePaths(ctx, path, selection)
			fmt.Printf("locks=%d\n", storeFileLocks(os.Getpid(), mainInode))
		case "write":
			// The daemon's real pattern: it reads through the diagnostic paths and then keeps
			// writing on the connection it holds. A holder that only reads never shows the loss.
			nonce := "holder-" + strconv.Itoa(writes)
			writes++
			if _, err := s.Querier(ctx).ExecContext(ctx, "INSERT INTO store_challenge(nonce,written_by,written_at) VALUES(?,?,?)", nonce, "holder", "2026-01-01T00:00:00Z"); err != nil {
				fmt.Println("holder-write:", err)
				return
			}
			fmt.Println("wrote")
		case "quit":
			return
		}
	}
}

// runStoreFilePaths runs the real product paths the issue names, each of which reads the store.
func runStoreFilePaths(ctx context.Context, path string, selection StateSelection) {
	_ = StoreSocket(path)
	_ = ReadOnlyRows(ctx, selection, "SELECT nonce FROM store_challenge", nil, func(RowScanner) error { return nil })
	_ = NonceLookup(ctx, selection, "holder")
	_, _ = OwnershipMetadata(ctx, path)
	_, _ = ownership.SnapshotMeta(ctx, path)
	_ = Probe(ctx, selection)
}

// storeFileLocks counts the POSIX lock entries this process holds on one inode, as /proc/locks
// reports them. A lock line is: idx TYPE ADVISORY READ <pid> <major:minor:inode> <start> <end>.
func storeFileLocks(pid int, inode uint64) int {
	raw, err := os.ReadFile("/proc/locks")
	if err != nil {
		return -1
	}
	want := strconv.FormatUint(inode, 10)
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[4] != strconv.Itoa(pid) {
			continue
		}
		parts := strings.Split(fields[5], ":")
		if len(parts) == 3 && parts[2] == want {
			count++
		}
	}
	return count
}

func TestStoreFileHandlesSurviveTheReadPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	seed, err := fixtureOpen(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	holder := exec.Command(os.Args[0], "-test.run=^TestStoreFileHolderProcess$")
	holder.Env = append(os.Environ(), storeFileHolderEnv+"="+path)
	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	holder.Stderr = os.Stderr
	if err = holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if holder.ProcessState == nil {
			_ = holder.Process.Kill()
			_ = holder.Wait()
		}
	})
	lines := bufio.NewScanner(stdout)
	if !lines.Scan() || !strings.HasPrefix(lines.Text(), "ready ") {
		t.Fatalf("holder did not start: %q (%v)", lines.Text(), lines.Err())
	}

	ask := func(command string) int {
		t.Helper()
		if _, err := fmt.Fprintln(stdin, command); err != nil {
			t.Fatal(err)
		}
		if !lines.Scan() {
			t.Fatalf("holder did not answer: %v", lines.Err())
		}
		text := lines.Text()
		if !strings.HasPrefix(text, "locks=") {
			t.Fatalf("holder answered %q", text)
		}
		got, err := strconv.Atoi(strings.TrimPrefix(text, "locks="))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	before := ask("locks")
	if before < 1 {
		t.Fatalf("the holder holds no POSIX lock on the store's main inode before any read path ran (locks=%d): the test cannot observe the lock it means to", before)
	}
	after := ask("paths")
	if after < before {
		t.Fatalf("the store's POSIX lock did not survive the read paths: %d lock(s) before, %d after. A process that holds a WAL connection must never close another descriptor of the same file (CRW-846)", before, after)
	}
}

// TestStoreFileHandlesSurviveASystemSQLitePeer is the end-to-end layer: a system-SQLite peer
// (python3) opens and closes the store while the holder holds it, then both write, and every row
// both committed must still be there.
func TestStoreFileHandlesSurviveASystemSQLitePeer(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is absent, so the system-SQLite peer that reproduces the split cannot run; the POSIX-lock assertion in TestStoreFileHandlesSurviveTheReadPaths is the interpreter-free proof")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	seed, err := fixtureOpen(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	holder := exec.Command(os.Args[0], "-test.run=^TestStoreFileHolderProcess$")
	holder.Env = append(os.Environ(), storeFileHolderEnv+"="+path)
	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	holder.Stderr = os.Stderr
	if err = holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if holder.ProcessState == nil {
			_ = holder.Process.Kill()
			_ = holder.Wait()
		}
	})
	lines := bufio.NewScanner(stdout)
	if !lines.Scan() || !strings.HasPrefix(lines.Text(), "ready ") {
		t.Fatalf("holder did not start: %q (%v)", lines.Text(), lines.Err())
	}

	peer := func(script string) {
		t.Helper()
		out, err := exec.Command(python, "-c", "import sqlite3,sys\nc=sqlite3.connect(sys.argv[1],timeout=30)\n"+script+"\nc.close()", path).CombinedOutput()
		if err != nil {
			t.Fatalf("python peer: %v %s", err, out)
		}
	}
	// The peer opens and closes; with the defect the holder's lost lock lets this close delete
	// the log the holder is still writing to.
	peer("c.execute('select count(*) from store_challenge').fetchone()")
	for round := 0; round < 4; round++ {
		if _, err := fmt.Fprintln(stdin, "paths"); err != nil {
			t.Fatal(err)
		}
		if !lines.Scan() {
			t.Fatalf("holder did not answer: %v", lines.Err())
		}
		if _, err := fmt.Fprintln(stdin, "write"); err != nil {
			t.Fatal(err)
		}
		if !lines.Scan() || lines.Text() != "wrote" {
			t.Fatalf("holder did not write: %q (%v)", lines.Text(), lines.Err())
		}
		peer("\nfor i in range(10): c.execute(\"insert into store_challenge(nonce,written_by,written_at) values(?,'peer','2026-01-01T00:00:00Z')\", ('peer-" + strconv.Itoa(round) + "-%d' % (i,),))\nc.commit()")
	}

	check, err := exec.Command(python, "-c", "import sqlite3,sys\nc=sqlite3.connect(sys.argv[1])\nprint(c.execute('select count(*) from store_challenge').fetchone()[0])", path).CombinedOutput()
	if err != nil {
		t.Fatalf("python check: %v %s", err, check)
	}
	rows, err := strconv.Atoi(strings.TrimSpace(string(check)))
	if err != nil {
		t.Fatalf("python check answered %q: %v", check, err)
	}
	// The holder seeded 1 row and wrote 4 more; the peer committed 40.
	if want := 45; rows != want {
		t.Fatalf("the store holds %d rows, want %d: committed rows were lost, which is the WAL split this change prevents", rows, want)
	}
}
