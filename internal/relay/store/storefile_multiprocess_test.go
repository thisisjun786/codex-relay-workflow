//go:build linux

package store

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

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
// CRW-888: the test carries its own diagnosis, because it failed once on CI with locks=0 and said
// nothing about why. The holder states its lock count on the ready line; a zero count before any
// read path has not been shown to be a lost lock and only costs a merge-lane turn, so it records
// the holder's evidence and skips. A zero count AFTER the read paths is a real loss and still
// fails. The evidence is the /proc/locks lines that name the store's main inode, the holder's own
// descriptors on the store file and its sidecars, and the journal mode.
//
// The end-to-end layer runs a system-SQLite peer (python3) because a modernc-only peer does not
// reproduce the deletion at all: modernc's own open and close of the store file does not take the
// lock the same way, so the sidecars survive and no rows are lost even when the holder has dropped
// its lock. The peer is the configuration the issue's own reproduction used, and it is skipped
// with a recorded reason where python3 is absent.

// storeFileHolderEnv names the holder child process's store; its absence makes the test a no-op.
const storeFileHolderEnv = "CRW846_HOLDER_DB"

// storeFileHolderNoLockEnv makes the holder give up its POSIX lock on the store on demand, so the
// zero-count paths and their diagnostic are observable. "before" (or "1") drops it before the ready
// line, which is the precondition; "after" drops it after the read paths, which is a real loss.
// Only the tests that drive it set it.
const storeFileHolderNoLockEnv = "CRW846_HOLDER_NO_LOCK"

// fOFDSetLK is F_OFD_SETLK (0x25 on linux/amd64, asm-generic/fcntl.h); syscall does not export it.
const fOFDSetLK = 0x25

// storeFileHookLocks keeps the hook's descriptors reachable for the life of the process. A
// descriptor left to the collector would be closed by os.File's finalizer, which releases the OFD
// lock the diagnostic is meant to show -- the same hazard storefile.go's registry records. The
// slice is only ever appended to, and only the hook appends.
var storeFileHookLocks []*os.File

// The holder frames its diagnostic block with these, so the parent reads it to its end without
// knowing how many lines it holds.
const (
	storeFileDiagnosticBegin = "diagnostic-begin"
	storeFileDiagnosticEnd   = "diagnostic-end"
)

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
	switch os.Getenv(storeFileHolderNoLockEnv) {
	case "1", "before":
		storeFileDropOwnLock(path)
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
	// The INSERT is committed and no read path has run, so this count is exactly the precondition
	// the parent states on the ready line instead of re-asking for it.
	count, _ := storeFileLocks(os.Getpid(), mainInode)
	fmt.Printf("ready inode=%d locks=%d\n", mainInode, count)

	writes := 0
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		switch sc.Text() {
		case "diagnose":
			storeFileWriteDiagnostic(os.Stdout, path, mainInode, s, ctx)
		case "paths":
			runStoreFilePaths(ctx, path, selection)
			if os.Getenv(storeFileHolderNoLockEnv) == "after" {
				storeFileDropOwnLock(path)
			}
			after, _ := storeFileLocks(os.Getpid(), mainInode)
			fmt.Printf("locks=%d\n", after)
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

// storeFileDropOwnLock is the CRW846_HOLDER_NO_LOCK hook. It drops this process's POSIX lock on the
// store the way I-563 describes -- a POSIX lock is held per process and per file, so opening and
// closing one more descriptor of the same file takes the lock off the connection that still holds
// it -- and then takes an open-file-description (OFD) lock on a descriptor it keeps, so the
// diagnostic has a real /proc/locks line that names the inode and that the counting rule does not
// count (an OFD lock's owner column is -1).
func storeFileDropOwnLock(path string) {
	if extra, err := os.Open(path); err == nil {
		_ = extra.Close()
	}
	held, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		fmt.Println("holder-hook-open:", err)
		return
	}
	storeFileHookLocks = append(storeFileHookLocks, held)
	lock := syscall.Flock_t{Type: syscall.F_RDLCK, Whence: 0, Start: 0, Len: 0}
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, held.Fd(), fOFDSetLK, uintptr(unsafe.Pointer(&lock))); errno != 0 {
		fmt.Println("holder-hook-lock:", errno)
	}
}

// storeFileLocks counts the POSIX lock entries this process holds on one inode, as /proc/locks
// reports them, and returns the lines that name the inode but that the rule did not count. A lock
// line is: idx TYPE ADVISORY READ <pid> <major:minor:inode> <start> <end>. The counting rule is
// CRW-846's, unchanged: field 4 is the owner pid and field 5 splits on ':' with the inode last. A
// read failure keeps the -1 count and reports itself as the one uncounted line.
//
// CRW-1054: /proc/locks is not a snapshot. It is a seq_file over the kernel's lock list, and a read
// resumes the walk at the position the previous read reached, so when other processes add or remove
// locks between two reads a held line can be skipped (reproduced with cross-process lock churn: a
// read of a held lock missed it and the next read found it, and no miss survived five reads). So the
// count is taken from the first read that shows a lock. A lock that is really gone reads 0 on every
// read and still returns 0 after storeFileLockReads reads, with the lines of the last read.
func storeFileLocks(pid int, inode uint64) (int, []string) {
	for read := 1; ; read++ {
		raw, err := readProcLocks()
		if err != nil {
			return -1, []string{"read /proc/locks: " + err.Error()}
		}
		count, uncounted := storeFileLockCountsFrom(raw, pid, inode)
		if count >= 1 || read == storeFileLockReads {
			return count, uncounted
		}
		time.Sleep(storeFileLockReadGap)
	}
}

// storeFileLockReads bounds how many reads of /proc/locks storeFileLocks makes before it reports a
// count of 0; storeFileLockReadGap is the pause between two of them.
const (
	storeFileLockReads   = 5
	storeFileLockReadGap = 10 * time.Millisecond
)

// readProcLocks returns the text of one read of /proc/locks. It is a package variable only so the
// reader test can feed storeFileLocks a recorded sequence of texts.
var readProcLocks = func() (string, error) {
	raw, err := os.ReadFile("/proc/locks")
	return string(raw), err
}

// storeFileLockCountsFrom applies the counting rule to the text of /proc/locks and collects the
// naming lines it did not count, so a recorded table pins both halves without a live kernel.
func storeFileLockCountsFrom(raw string, pid int, inode uint64) (int, []string) {
	want := strconv.FormatUint(inode, 10)
	owner := strconv.Itoa(pid)
	count := 0
	var uncounted []string
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" || !storeFileLineNamesInode(line, want) {
			continue
		}
		fields := strings.Fields(line)
		counted := len(fields) >= 6 && fields[4] == owner
		if counted {
			if parts := strings.Split(fields[5], ":"); len(parts) == 3 && parts[2] == want {
				count++
				continue
			}
		}
		uncounted = append(uncounted, line)
	}
	return count, uncounted
}

// storeFileLineNamesInode reports whether any whitespace-separated token of a /proc/locks line
// splits on ':' with the wanted inode last. A waiter line carries the blocked process behind '->'
// and an OFD lock carries -1 as its owner, so neither is counted, but both still name the inode.
func storeFileLineNamesInode(line, want string) bool {
	for _, field := range strings.Fields(line) {
		if parts := strings.Split(field, ":"); len(parts) == 3 && parts[2] == want {
			return true
		}
	}
	return false
}

// storeFileDiagnosticLines is the evidence the issue asks the holder to print when the lock count
// is zero: the /proc/locks lines that name the store's main inode, the lines the counting rule did
// not count, the holder's own descriptors that resolve to the store file and its -wal and -shm
// sidecars, and the journal mode the live connection reports.
func storeFileDiagnosticLines(path string, inode uint64, s *Store, ctx context.Context) []string {
	pid := os.Getpid()
	count, uncounted := storeFileLocks(pid, inode)
	want := strconv.FormatUint(inode, 10)
	lines := []string{fmt.Sprintf("holder-pid=%d store-inode=%d locks=%d", pid, inode, count)}
	naming := 0
	lines = append(lines, "proc-locks-lines-naming-the-store-inode:")
	if raw, err := os.ReadFile("/proc/locks"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) == "" || !storeFileLineNamesInode(line, want) {
				continue
			}
			naming++
			lines = append(lines, "  "+line)
		}
	} else {
		lines = append(lines, "  read /proc/locks: "+err.Error())
	}
	if naming == 0 {
		lines = append(lines, "  (none)")
	}
	lines = append(lines, "proc-locks-uncounted-by-the-counting-rule:")
	if len(uncounted) == 0 {
		lines = append(lines, "  (none)")
	}
	for _, line := range uncounted {
		lines = append(lines, "  "+line)
	}
	lines = append(lines, "holder-descriptors-on-the-store-files:")
	descriptors := storeFileDescriptorsOn(path)
	if len(descriptors) == 0 {
		lines = append(lines, "  (none)")
	}
	lines = append(lines, descriptors...)
	return append(lines, "journal-mode="+storeFileJournalMode(ctx, s))
}

// storeFileWriteDiagnostic prints the diagnostic block the parent reads to its end sentinel.
func storeFileWriteDiagnostic(out io.Writer, path string, inode uint64, s *Store, ctx context.Context) {
	fmt.Fprintln(out, storeFileDiagnosticBegin)
	for _, line := range storeFileDiagnosticLines(path, inode, s, ctx) {
		fmt.Fprintln(out, line)
	}
	fmt.Fprintln(out, storeFileDiagnosticEnd)
}

// storeFileReadDiagnostic reads the holder's diagnostic block from the ready-line scanner. A holder
// that dies mid-block still contributes what it printed.
func storeFileReadDiagnostic(lines *bufio.Scanner) string {
	var block []string
	for lines.Scan() {
		block = append(block, lines.Text())
		if lines.Text() == storeFileDiagnosticEnd {
			break
		}
	}
	if len(block) == 0 {
		return "(" + storeFileDiagnosticBegin + " was not reached: " + fmt.Sprint(lines.Err()) + ")"
	}
	return strings.Join(block, "\n")
}

// storeFileDiagnosticMessage is the failure text: the sentence the test has always printed, the
// holder's pid, and the block the holder just produced.
func storeFileDiagnosticMessage(pid, count int, block string) string {
	return fmt.Sprintf("the holder holds no POSIX lock on the store's main inode (locks=%d): the test cannot observe the lock it means to\nholder pid=%d\n%s", count, pid, block)
}

// storeFileDescriptorsOn lists this process's /proc/self/fd entries whose target resolves to the
// store file or one of its sidecars, as "<fd> -> <target>". Both the path the test used and the
// path SQLite resolves it to are accepted, because SQLite opens the sidecars beside whichever it
// opened; a target the kernel marked " (deleted)" still matches, because a -wal unlinked under the
// live connection is exactly what the diagnostic is for.
func storeFileDescriptorsOn(path string) []string {
	spellings := map[string]bool{}
	for _, base := range []string{path, resolveLoosely(path)} {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			spellings[base+suffix] = true
		}
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return []string{"  read /proc/self/fd: " + err.Error()}
	}
	var found []string
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil || !spellings[strings.TrimSuffix(target, " (deleted)")] {
			continue
		}
		found = append(found, "  "+entry.Name()+" -> "+target)
	}
	return found
}

// storeFileJournalMode reads the journal mode from the connection the holder already holds, so the
// diagnostic reports what the live connection says rather than what a second open would say.
func storeFileJournalMode(ctx context.Context, s *Store) string {
	var mode string
	if err := s.Querier(ctx).QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return "unreadable: " + err.Error()
	}
	return mode
}

// storeFileParseReadyLine reads the holder's ready line: "ready inode=N locks=K".
func storeFileParseReadyLine(line string) (uint64, int, bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != "ready" {
		return 0, 0, false
	}
	if !strings.HasPrefix(fields[1], "inode=") || !strings.HasPrefix(fields[2], "locks=") {
		return 0, 0, false
	}
	inode, err := strconv.ParseUint(strings.TrimPrefix(fields[1], "inode="), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	locks, err := strconv.Atoi(strings.TrimPrefix(fields[2], "locks="))
	if err != nil {
		return 0, 0, false
	}
	return inode, locks, true
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
	if !lines.Scan() {
		t.Fatalf("holder did not start: %v", lines.Err())
	}
	_, before, ok := storeFileParseReadyLine(lines.Text())
	if !ok {
		t.Fatalf("holder did not start: %q (%v)", lines.Text(), lines.Err())
	}
	if before < 1 {
		// CRW-888: a count of 0, or a lookup error, before any read path is a failed precondition
		// with the holder's evidence in the failure. A skip or a log alone judges nothing.
		t.Fatalf("%s", storeFileDiagnosticMessage(holder.Process.Pid, before, askStoreFileDiagnostic(t, stdin, lines)))
	}

	after := askStoreFileLocks(t, stdin, lines, "paths")
	if after < 0 {
		// A lookup error is not a transient drop: only a count of 0 after the read paths is a lost lock.
		t.Fatalf("the holder could not read its lock count after the read paths (locks=%d), so the lock was not observed (CRW-888)\n%s", after, askStoreFileDiagnostic(t, stdin, lines))
	}
	if after == 0 {
		t.Fatalf("the store's POSIX lock did not survive the read paths: %d lock(s) before, %d after. A process that holds a WAL connection must never close another descriptor of the same file (CRW-846)\n%s", before, after, askStoreFileDiagnostic(t, stdin, lines))
	}
	if after < before {
		// A lower non-zero count is not a lost lock: closing any descriptor of a file drops every
		// POSIX lock the process holds on that file, so a real loss reads 0, never 1. The main
		// inode's /proc/locks count can carry a transient extra line, so this is logged.
		t.Logf("the holder's POSIX lock count on the store's main inode fell from %d to %d without reaching zero; a transient extra /proc/locks line is the likely cause, and a real loss reads 0", before, after)
	}
}

// askStoreFileLocks sends one command and reads the holder's lock count back.
func askStoreFileLocks(t *testing.T, stdin io.Writer, lines *bufio.Scanner, command string) int {
	t.Helper()
	if _, err := fmt.Fprintln(stdin, command); err != nil {
		t.Fatal(err)
	}
	if !lines.Scan() {
		t.Fatalf("holder did not answer %q: %v", command, lines.Err())
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

// askStoreFileDiagnostic asks the holder for its diagnostic block and returns it.
func askStoreFileDiagnostic(t *testing.T, stdin io.Writer, lines *bufio.Scanner) string {
	t.Helper()
	if _, err := fmt.Fprintln(stdin, "diagnose"); err != nil {
		return "(" + storeFileDiagnosticBegin + " was not asked for: " + err.Error() + ")"
	}
	return storeFileReadDiagnostic(lines)
}

// TestStoreFileLockDiagnosticFromALocklessHolder drives the hook: the holder gives up its lock
// before it reports ready, so the precondition does not hold and the diagnosis must carry the
// holder's own evidence. The test passes; what it pins is the diagnosis, not a failure.
func TestStoreFileLockDiagnosticFromALocklessHolder(t *testing.T) {
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
	holder.Env = append(os.Environ(), storeFileHolderEnv+"="+path, storeFileHolderNoLockEnv+"=1")
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
	if !lines.Scan() {
		t.Fatalf("holder did not start: %v", lines.Err())
	}
	inode, count, ok := storeFileParseReadyLine(lines.Text())
	if !ok {
		t.Fatalf("holder did not start: %q (%v)", lines.Text(), lines.Err())
	}
	if count != 0 {
		t.Fatalf("the lockless hook did not take the holder's lock off the store: ready line %q", lines.Text())
	}
	message := storeFileDiagnosticMessage(holder.Process.Pid, count, askStoreFileDiagnostic(t, stdin, lines))
	for _, want := range []string{
		fmt.Sprintf("store-inode=%d", inode),
		"proc-locks-lines-naming-the-store-inode:",
		"proc-locks-uncounted-by-the-counting-rule:",
		"OFDLCK",
		"holder-descriptors-on-the-store-files:",
		"relay.sqlite3",
		"-wal",
		"journal-mode=",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the diagnostic does not name %q:\n%s", want, message)
		}
	}
}

// TestStoreFileLockDiagnosticAfterTheReadPaths pins the failing path end to end at the process
// boundary: the hook drops the holder's lock during the read paths, so the count the parent reads
// after them is 0 and the block that accompanies that failure names the inode, the uncounted line,
// the descriptors and the journal mode. The suite cannot assert another test's failure, so this
// drives the same holder the parent drives and reads the same answers.
func TestStoreFileLockDiagnosticAfterTheReadPaths(t *testing.T) {
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
	holder.Env = append(os.Environ(), storeFileHolderEnv+"="+path, storeFileHolderNoLockEnv+"=after")
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
	if !lines.Scan() {
		t.Fatalf("holder did not start: %v", lines.Err())
	}
	_, before, ok := storeFileParseReadyLine(lines.Text())
	if !ok {
		t.Fatalf("holder did not start: %q (%v)", lines.Text(), lines.Err())
	}
	if before < 1 {
		t.Fatalf("the hook drops the lock during the read paths, so the precondition must hold: ready line %q", lines.Text())
	}
	if after := askStoreFileLocks(t, stdin, lines, "paths"); after != 0 {
		t.Fatalf("the lock the hook drops during the read paths is still counted: %d lock(s) after", after)
	}
	message := storeFileDiagnosticMessage(holder.Process.Pid, 0, askStoreFileDiagnostic(t, stdin, lines))
	for _, want := range []string{"locks=0", "holder-descriptors-on-the-store-files:", "journal-mode=", "diagnostic-end"} {
		if !strings.Contains(message, want) {
			t.Errorf("the failure message for the lost lock does not name %q:\n%s", want, message)
		}
	}
}

// TestStoreFileReadyLineAndDiagnosticMessage pins the two contracts the parent depends on: the ready
// line the holder writes and the parent parses, and the failure text the diagnostic is carried in.
// Both are strings the two processes agree on, so a change to either has to be deliberate.
func TestStoreFileReadyLineAndDiagnosticMessage(t *testing.T) {
	cases := []struct {
		line  string
		inode uint64
		locks int
		ok    bool
	}{
		{"ready inode=2505008 locks=1", 2505008, 1, true},
		{"ready inode=0 locks=0", 0, 0, true},
		{"ready inode=2505008", 0, 0, false},
		{"ready locks=1 inode=2505008", 0, 0, false},
		{"ready inode=abc locks=1", 0, 0, false},
		{"ready inode=2505008 locks=x", 0, 0, false},
		{"ready inode=2505008 locks=1 extra", 0, 0, false},
		{"locks=1", 0, 0, false},
		{"holder-open: refused", 0, 0, false},
	}
	for _, c := range cases {
		inode, locks, ok := storeFileParseReadyLine(c.line)
		if ok != c.ok || (ok && (inode != c.inode || locks != c.locks)) {
			t.Errorf("storeFileParseReadyLine(%q) = (%d, %d, %v), want (%d, %d, %v)", c.line, inode, locks, ok, c.inode, c.locks, c.ok)
		}
	}
	message := storeFileDiagnosticMessage(4242, 0, storeFileDiagnosticBegin+"\njournal-mode=wal\n"+storeFileDiagnosticEnd)
	for _, want := range []string{"locks=0", "holder pid=4242", storeFileDiagnosticBegin, "journal-mode=wal", storeFileDiagnosticEnd} {
		if !strings.Contains(message, want) {
			t.Errorf("the failure text does not carry %q:\n%s", want, message)
		}
	}
}

// TestStoreFileLocksKeepsItsCountingRule pins both halves of the rule against recorded /proc/locks
// text: the counted shape, and the two shapes the issue names as uncounted.
func TestStoreFileLocksKeepsItsCountingRule(t *testing.T) {
	const inode = 106078217
	const pid = 3095239
	recorded := strings.Join([]string{
		"11: POSIX  ADVISORY  READ 3095239 103:07:106078217 1073741826 1073742335",
		"12: POSIX  ADVISORY  WRITE 4000000 103:07:106078217 0 EOF",
		"13: OFDLCK ADVISORY  READ -1 103:07:106078217 0 EOF",
		"14: -> POSIX  ADVISORY  WRITE 3095239 103:07:106078217 0 EOF",
		"15: POSIX  ADVISORY  READ 3095239 103:07:999999999 0 EOF",
	}, "\n")
	count, uncounted := storeFileLockCountsFrom(recorded, pid, inode)
	if count != 1 {
		t.Errorf("the counting rule counted %d lines, want 1: the owner pid and the inode decide, and only line 11 has both", count)
	}
	want := []string{
		"12: POSIX  ADVISORY  WRITE 4000000 103:07:106078217 0 EOF",
		"13: OFDLCK ADVISORY  READ -1 103:07:106078217 0 EOF",
		"14: -> POSIX  ADVISORY  WRITE 3095239 103:07:106078217 0 EOF",
	}
	if strings.Join(uncounted, "|") != strings.Join(want, "|") {
		t.Errorf("the uncounted lines are %q, want %q", uncounted, want)
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
	if !lines.Scan() {
		t.Fatalf("holder did not start: %v", lines.Err())
	}
	if _, _, ok := storeFileParseReadyLine(lines.Text()); !ok {
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
