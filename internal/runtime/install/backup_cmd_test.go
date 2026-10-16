package install_test

// backup_cmd_test.go is the operator command of CRW-862: `crw install backup-state --to <dir>` takes the same
// stopped byte copy outside an install, refuses while the relay service runs, and holds the store's write gate
// exclusively while it copies. The service reading is faked, as the packet requires (the "service running" case is a
// faked reading): no daemon is started and no live relay state is touched.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"

	_ "modernc.org/sqlite"
)

// backupStateHost is a host with a relay state directory holding a whole store (a database, its write gate and its
// ownership mirror), as a stopped relay leaves one.
func backupStateHost(t *testing.T) *host {
	t.Helper()
	h := newHost(t)
	testsupport.Create(t, filepath.Join(h.relayState, "relay.sqlite3"), "", "go")
	write(t, filepath.Join(h.relayState, "ledger.log"), "line one\n")
	return h
}

// The standalone backup records the route that took it: the operator command takes no swap and reads no in-flight
// cells, so its manifest and result must not claim the guarded install route's checks. Before the fix both carried
// the install route's words, which could be mistaken for evidence from the swap gate.
//
// sequential: replaces the service reading.
func TestTheOperatorCommandRecordsItsOwnRoute(t *testing.T) {
	h := backupStateHost(t)
	restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
		return install.ServiceCell(scope.Stopped, true, "the service answered and reports itself not running")
	})
	defer restore()
	dest := filepath.Join(h.home, "operator-backup")
	result, code := install.BackupState(context.Background(), h.options(), dest)
	if code != install.OK || at(result, "stateBackup", "takenBy") != "operator" {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	var manifest struct {
		TakenBy string `json:"takenBy"`
		Meaning string `json:"meaning"`
	}
	if err := json.Unmarshal(mustRead(t, dest+install.ManifestSuffix), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.TakenBy != "operator" || strings.Contains(manifest.Meaning, "before the swap") {
		t.Fatalf("the manifest claims the install route: %+v", manifest)
	}
}

// ---- CRW-862 review fixes ----

// The write gate the command locks is the one the relay writers lock: beside the database as SQLite resolves it
// (holdGate), which for a linked store is the linked file directory and not the state directory root. Before the fix
// the command locked the state directory gate, so a writer holding the resolved gate kept writing under the copy.
//
// sequential: takes the real write gate and replaces the service reading.
func TestTheOperatorCommandLocksTheGateBesideTheResolvedStore(t *testing.T) {
	h := backupStateHost(t)
	real := filepath.Join(h.relayState, "data", "real.sqlite3")
	testsupport.Create(t, real, "", "go")
	if err := os.Remove(filepath.Join(h.relayState, "relay.sqlite3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(h.relayState, "relay.sqlite3")); err != nil {
		t.Fatal(err)
	}
	restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
		return install.ServiceCell(scope.Stopped, true, "the service answered and reports itself not running")
	})
	defer restore()
	// a relay writer holds the gate beside the resolved database
	held, err := ownership.Lock(filepath.Join(filepath.Dir(real), "write-gate.lock"), true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	dest := filepath.Join(h.home, "operator-backup")
	result, code := install.BackupState(context.Background(), h.options(), dest)
	if code != install.Refused || at(result, "applied") != false || !strings.Contains(text(at(result, "refused")), "write gate") {
		t.Fatalf("a held resolved gate must refuse: exit %d\n%s", code, golden.Canon(result))
	}
	nothingAt(t, dest)
}

// A --state holding a byte that is not UTF-8 is refused as usage: the manifest would otherwise record a source path
// with the bytes replaced, naming no real directory. Before the fix only --to was validated.
//
// sequential: none.
func TestTheOperatorCommandRefusesANonUTF8State(t *testing.T) {
	h := backupStateHost(t)
	var stdout, stderr strings.Builder
	code := install.Main(context.Background(), []string{"backup-state", "--to", filepath.Join(h.home, "b"), "--state", string([]byte{0xff, 0xfe})}, h.env, &stdout, &stderr)
	if code != install.Usage || !strings.Contains(stderr.String(), "not UTF-8") {
		t.Fatalf("exit %d stderr=%q", code, stderr.String())
	}
}

// The integrity gate duplicates a store of any size with a fixed buffer: a multi-megabyte store is duplicated and
// checked without the process holding it in memory, and the copy is byte-identical.
//
// sequential: replaces the service reading.
func TestTheOperatorCommandDuplicatesALargeStoreWithoutHoldingItInMemory(t *testing.T) {
	// a store several times the 1 MiB copy buffer, written through SQLite so it is a real database
	h := newHost(t)
	big := filepath.Join(h.relayState, "relay.sqlite3")
	testsupport.Create(t, big, "", "go")
	db, err := sql.Open("sqlite", big)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE filler (id INTEGER PRIMARY KEY, blob BLOB)"); err != nil {
		t.Fatal(err)
	}
	// one transaction: 64 commits of 64 KiB were 64 syncs, which on a loaded host waited behind every other
	// test's writes (CRW-1263); the store is the same size and a real database either way
	if _, err := db.Exec("WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 64) INSERT INTO filler (blob) SELECT randomblob(65536) FROM n"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(big)
	if err != nil || info.Size() < 4<<20 {
		t.Fatalf("the fixture store is %v bytes: %v", info, err)
	}
	restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
		return install.ServiceCell(scope.Stopped, true, "the service answered and reports itself not running")
	})
	defer restore()
	dest := filepath.Join(h.home, "operator-backup")
	result, code := install.BackupState(context.Background(), h.options(), dest)
	if code != install.OK || at(result, "stateBackup", "restoreCandidate") != true {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dest, "relay.sqlite3")), mustRead(t, big)) {
		t.Fatal("the copy of a large store is not byte-identical")
	}
}

// The command takes the same artifact outside an install: the copy, its manifest and the integrity gate, with the
// service stopped and the write gate held exclusively.
//
// sequential: replaces the service-reading seam.
func TestTheOperatorCommandTakesTheArtifactOutsideAnInstall(t *testing.T) {
	h := backupStateHost(t)
	restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
		return install.ServiceCell(scope.Stopped, true, "the service answered and reports itself not running")
	})
	defer restore()
	dest := filepath.Join(h.home, "operator-backup")
	result, code := install.BackupState(context.Background(), h.options(), dest)
	if code != install.OK || at(result, "applied") != true || at(result, "stateBackup", "made") != true || at(result, "stateBackup", "restoreCandidate") != true {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if at(result, "stateBackup", "integrityCheck") != "ok" {
		t.Fatalf("the artifact records its integrity: %s", golden.Canon(at(result, "stateBackup")))
	}
	// the copy and its manifest are there, and the manifest records the gate
	var manifest struct {
		IntegrityCheck   string `json:"integrityCheck"`
		RestoreCandidate bool   `json:"restoreCandidate"`
	}
	if err := json.Unmarshal(mustRead(t, dest+install.ManifestSuffix), &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.RestoreCandidate || manifest.IntegrityCheck != "ok" {
		t.Fatalf("manifest: %+v", manifest)
	}
}

// The command refuses while the relay service reads as running: it copies nothing and writes nothing.
//
// sequential: replaces the service-reading seam.
func TestTheOperatorCommandRefusesWhileTheServiceRuns(t *testing.T) {
	h := backupStateHost(t)
	restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
		return install.ServiceCell(scope.Running, true, "the service answered and reports itself running")
	})
	defer restore()
	dest := filepath.Join(h.home, "operator-backup")
	result, code := install.BackupState(context.Background(), h.options(), dest)
	if code != install.Refused || at(result, "applied") != false || !strings.Contains(text(at(result, "refused")), "running") {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	nothingAt(t, dest)
	nothingAt(t, dest+install.ManifestSuffix)
}

// A service reading that could not be taken refuses rather than assuming the service is stopped.
//
// sequential: replaces the service-reading seam.
func TestTheOperatorCommandRefusesAnUnreadableServiceReading(t *testing.T) {
	h := backupStateHost(t)
	restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
		return install.ServiceCell("UNREADABLE", false, "the service could not be asked")
	})
	defer restore()
	dest := filepath.Join(h.home, "operator-backup")
	result, code := install.BackupState(context.Background(), h.options(), dest)
	if code != install.Refused || at(result, "applied") != false || !strings.Contains(text(at(result, "refused")), "could not be established") {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	nothingAt(t, dest)
}

// The command holds the store's write gate exclusively: while a relay writer holds it, the command refuses instead of
// copying a store a writer may be reaching.
//
// sequential: takes the real write gate.
func TestTheOperatorCommandRefusesWhileTheWriteGateIsHeld(t *testing.T) {
	h := backupStateHost(t)
	restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
		return install.ServiceCell(scope.Stopped, true, "the service answered and reports itself not running")
	})
	defer restore()
	held, err := ownership.Lock(filepath.Join(h.relayState, "write-gate.lock"), true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	dest := filepath.Join(h.home, "operator-backup")
	result, code := install.BackupState(context.Background(), h.options(), dest)
	if code != install.Refused || at(result, "applied") != false || !strings.Contains(text(at(result, "refused")), "write gate") {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	nothingAt(t, dest)
}

// The command's own flag and its own help surface, and the frozen install usage line does not name it.
//
// sequential: t.Setenv("PATH") is process-wide.
func TestTheOperatorCommandSurface(t *testing.T) {
	fakeBin := t.TempDir()
	writeExecutable(t, filepath.Join(fakeBin, "codex"), []byte("#!/bin/sh\necho codex-cli 0.154.0\n"), 0o755)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := backupStateHost(t)
	common := []string{"--codex-home", h.codex, "--record", h.record, "--socket", h.fake.SocketPath, "--state", h.relayState}
	run := func(args ...string) (string, string, int) {
		var stdout, stderr strings.Builder
		code := install.Main(context.Background(), append(args, common...), h.env, &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}
	// help
	if stdout, _, code := run("backup-state", "--help"); code != install.OK || !strings.Contains(stdout, "crw install backup-state --to <dir>") {
		t.Fatalf("help: exit %d\n%s", code, stdout)
	}
	// --to is required
	if _, stderr, code := run("backup-state"); code != install.Usage || !strings.Contains(stderr, "--to") {
		t.Fatalf("missing --to: exit %d %s", code, stderr)
	}
	// the frozen usage line still names only the commands it named before this one arrived
	var stdout, stderr strings.Builder
	if code := install.Main(context.Background(), []string{"help"}, nil, &stdout, &stderr); code != install.OK {
		t.Fatalf("install help: exit %d", code)
	}
	want := "usage: crw install {install,update,rollback,remove,status,register-mcp,hook,register-service} ...\n"
	if stdout.String() != want {
		t.Fatalf("install usage = %q, want %q", stdout.String(), want)
	}
	// the invalid-choice list names it
	if _, stderr, code := run("unpack"); code != install.Usage || !strings.Contains(stderr, "backup-state") {
		t.Fatalf("invalid choice: exit %d %s", code, stderr)
	}
}

// Failure class 1 (data loss): the listing's probe of a linked store's log must not read a filesystem error that is
// not ENOENT as "there is no log" (CRW-862 generation 3). The store's database resolves to a file whose name is 252
// bytes, so its "-wal" name is 256 bytes, past NAME_MAX: the probe fails with ENAMETOOLONG while the database itself
// opens. Before the fix that error was read as absence, so the copy was taken without the log, the integrity gate
// passed on the database alone and the manifest marked a restore candidate although committed frames may have been
// left out. Refused after the fix, naming the log's path and the error, with no artifact left behind.
//
// The operator command is the route that reaches the listing: the install route reads the store's schema first, and
// that reading refuses on the same too-long name before the listing runs. The control keeps the other half of the
// rule: a linked store whose log really is absent is still copied.
//
// sequential: replaces the service reading.
func TestTheBackupRefusesALogProbeErrorThatIsNotAbsence(t *testing.T) {
	// linkedStore points the state directory's relay.sqlite3 at a database under data/ and returns the path it
	// resolves to, which is the layout a relay that keeps its store elsewhere leaves; the write gate sits beside the
	// resolved database, where the relay's own writers leave it.
	linkedStore := func(t *testing.T, h *host, name string) string {
		t.Helper()
		dir := filepath.Join(h.relayState, "data")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// The database is made under a short name and renamed: SQLite could not create its own sidecars beside a name
		// past NAME_MAX, so it could not be created there at all.
		short := filepath.Join(dir, "real.sqlite3")
		testsupport.Create(t, short, "", "go")
		real := filepath.Join(dir, name)
		if real != short {
			if err := os.Rename(short, real); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Remove(filepath.Join(h.relayState, "relay.sqlite3")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(h.relayState, "relay.sqlite3")); err != nil {
			t.Fatal(err)
		}
		return real
	}
	stopped := func(t *testing.T) {
		t.Helper()
		restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
			return install.ServiceCell(scope.Stopped, true, "the service answered and reports itself not running")
		})
		t.Cleanup(restore)
	}
	t.Run("the probe fails", func(t *testing.T) {
		h := backupStateHost(t)
		stopped(t)
		real := linkedStore(t, h, strings.Repeat("d", 252))
		// the construction this test is about: the database opens and its log's name is past NAME_MAX
		if _, err := os.Lstat(real); err != nil {
			t.Fatalf("the resolved database cannot be examined: %v", err)
		}
		if _, err := os.Lstat(real + "-wal"); !errors.Is(err, syscall.ENAMETOOLONG) {
			t.Fatalf("the fixture needs the -wal name past NAME_MAX: os.Lstat(%q) = %v", real+"-wal", err)
		}
		dest := filepath.Join(h.home, "operator-backup")
		result, code := install.BackupState(context.Background(), h.options(), dest)
		if code != install.Refused || at(result, "applied") != false {
			t.Fatalf("a log probe error that is not absence must refuse: exit %d\n%s", code, golden.Canon(result))
		}
		refused := text(at(result, "refused"))
		if !strings.Contains(refused, real+"-wal") || !strings.Contains(refused, "file name too long") {
			t.Fatalf("the refusal must name the log's path and the error: %s", refused)
		}
		nothingAt(t, dest)
		nothingAt(t, dest+install.ManifestSuffix)
	})
	t.Run("the log is absent", func(t *testing.T) {
		h := backupStateHost(t)
		stopped(t)
		linkedStore(t, h, "real.sqlite3")
		dest := filepath.Join(h.home, "operator-backup")
		result, code := install.BackupState(context.Background(), h.options(), dest)
		if code != install.OK || at(result, "applied") != true || at(result, "stateBackup", "made") != true {
			t.Fatalf("a linked store with no log must still be copied: exit %d\n%s", code, golden.Canon(result))
		}
	})
}

// TestBackupStateOmitsFlowWorktrees (CRW-1585): the state backup is of the store and its records, not
// of a run's regenerable working copies — the worktree roots under a run (land-wt, node-wt, wt and the
// calls' worktrees) are recorded in the manifest as workdir entries, and their bytes are not copied.
func TestBackupStateOmitsFlowWorktrees(t *testing.T) {
	h := backupStateHost(t)
	restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
		return install.ServiceCell(scope.Stopped, true, "the service answered and reports itself not running")
	})
	defer restore()
	// the run's own disposable roots, each a real preserved checkout of the size a repository copy would be
	roots := []string{"land-wt", "node-wt", "wt", "calls/c1/wt"}
	const blob = 8 << 20
	repo := backupRepo(t, blob)
	for _, root := range roots {
		dir := filepath.Join(h.relayState, "flow", "run-scope", filepath.FromSlash(root))
		backupGit(t, repo, "worktree", "add", "--detach", dir, "HEAD")
		preserveBackupFixture(t, h.relayState, "run-scope", root, repo)
	}
	// and the run's own records, which the backup does keep
	writeBackupSpec(t, h.relayState, "run-scope", repo)
	write(t, filepath.Join(h.relayState, "flow", "run-scope", "journal.jsonl"), "{}\n")
	// a real operations database, so its bytes are a real copy
	db, err := sql.Open("sqlite", filepath.Join(h.relayState, "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE backup_probe (value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO backup_probe VALUES ('operations record')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var sourceBytes int64
	if err := filepath.WalkDir(h.relayState, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && d.Name() != "relay.sqlite3-shm" {
			sourceBytes += info.Size()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// the excluded bytes are whatever the four preserved checkouts hold: payload, .gitignore and the .git pointer
	var omittedBytes int64
	for _, root := range roots {
		if err := filepath.WalkDir(filepath.Join(h.relayState, "flow", "run-scope", filepath.FromSlash(root)), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				omittedBytes += info.Size()
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	dest := filepath.Join(h.home, "scoped-backup")
	result, code := install.BackupState(context.Background(), h.options(), dest)
	if code != install.OK {
		t.Fatalf("backup: exit %d\n%s", code, golden.Canon(result))
	}
	// the store and the records are whole; no worktree root exists in the copy
	for _, want := range []string{"relay.sqlite3", "ledger.log", "operations.sqlite3",
		"flow/run-scope/run.json", "flow/run-scope/journal.jsonl"} {
		if got, err := os.ReadFile(filepath.Join(dest, want)); err != nil {
			t.Errorf("backup is missing %s: %v", want, err)
		} else if src, _ := os.ReadFile(filepath.Join(h.relayState, want)); !bytes.Equal(got, src) {
			t.Errorf("backup %s bytes differ", want)
		}
	}
	for _, root := range roots {
		if _, err := os.Lstat(filepath.Join(dest, "flow", "run-scope", filepath.FromSlash(root))); !os.IsNotExist(err) {
			t.Errorf("the workdir %s was copied", root)
		}
	}
	var manifest struct {
		Bytes   int64 `json:"bytes"`
		Entries []struct {
			Path, Kind, LinkTarget string
		} `json:"entries"`
	}
	if err := json.Unmarshal(mustRead(t, dest+install.ManifestSuffix), &manifest); err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture_source_regular_bytes=%d excluded_worktree_bytes=%d backup_regular_bytes=%d", sourceBytes, omittedBytes, manifest.Bytes)
	if manifest.Bytes != sourceBytes-omittedBytes {
		t.Errorf("backup bytes %d; source %d, excluded %d", manifest.Bytes, sourceBytes, omittedBytes)
	}
	found := map[string]bool{}
	for _, e := range manifest.Entries {
		if e.Kind == "workdir" {
			found[e.Path] = true
		}
	}
	for _, root := range roots {
		if !found["flow/run-scope/"+root] {
			t.Errorf("the workdir %s is not in the manifest: %+v", root, manifest.Entries)
		}
	}
}

// TestBackupStateRecordsWorktreeLinks (CRW-1585): a run's worktree link — a dangling one, one that
// names a directory outside the state or the worktree root itself — is recorded as a link, never
// followed and never a refusal.
func TestBackupStateRecordsWorktreeLinks(t *testing.T) {
	for name, link := range map[string]func(t *testing.T, h *host) (linkPath, target string){
		"broken link": func(t *testing.T, h *host) (string, string) {
			p := filepath.Join(h.relayState, "flow", "run-links", "node-wt", "node")
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			return p, filepath.Join(t.TempDir(), "gone")
		},
		"a link outside the state": func(t *testing.T, h *host) (string, string) {
			out := t.TempDir()
			write(t, filepath.Join(out, "sentinel"), "outside\n")
			p := filepath.Join(h.relayState, "flow", "run-links", "node-wt", "node")
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			return p, out
		},
		"a nested alias": func(t *testing.T, h *host) (string, string) {
			out := t.TempDir()
			write(t, filepath.Join(out, "sentinel"), "outside\n")
			p := filepath.Join(h.relayState, "flow", "run-links", "node-wt", "nested", "node")
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			return p, out
		},
		"the worktree root itself is a link": func(t *testing.T, h *host) (string, string) {
			out := t.TempDir()
			write(t, filepath.Join(out, "sentinel"), "outside\n")
			p := filepath.Join(h.relayState, "flow", "run-links", "node-wt")
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			return p, out
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := backupStateHost(t)
			restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
				return install.ServiceCell(scope.Stopped, true, "the service answered and reports itself not running")
			})
			defer restore()
			linkPath, target := link(t, h)
			if err := os.Symlink(target, linkPath); err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(h.relayState, linkPath)
			dest := filepath.Join(h.home, "links-backup")
			result, code := install.BackupState(context.Background(), h.options(), dest)
			if code != install.OK {
				t.Fatalf("a link must not refuse the backup: exit %d\n%s", code, golden.Canon(result))
			}
			var manifest struct {
				Entries []struct {
					Path, Kind, LinkTarget string
				} `json:"entries"`
			}
			if err := json.Unmarshal(mustRead(t, dest+install.ManifestSuffix), &manifest); err != nil {
				t.Fatal(err)
			}
			var entry *struct {
				Path, Kind, LinkTarget string
			}
			for i, e := range manifest.Entries {
				if e.Path == filepath.ToSlash(rel) {
					entry = &manifest.Entries[i]
				}
			}
			if entry == nil || entry.Kind != "symlink" || entry.LinkTarget != target {
				t.Errorf("the link %s is not recorded as a symlink: %+v", rel, manifest.Entries)
			}
			// the link's bytes are not in the backup under its name: the path is absent entirely
			if _, err := os.Lstat(filepath.Join(dest, rel)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the backup holds something at %s: %v", rel, err)
			}
			// the source alias is unchanged and still names its target
			if got, err := os.Readlink(linkPath); err != nil || got != target {
				t.Errorf("the source link changed: %v %q", err, got)
			}
			// and for a real target its bytes are exactly what they were
			if name != "broken link" {
				got, err := os.ReadFile(filepath.Join(target, "sentinel"))
				if err != nil || string(got) != "outside\n" {
					t.Errorf("the target changed: %v %q", err, got)
				}
			}
		})
	}
}

// stopped fakes the stopped-service reading the whole backup command needs.
func stopped(t *testing.T) {
	t.Helper()
	restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
		return install.ServiceCell(scope.Stopped, true, "the service answered and reports itself not running")
	})
	t.Cleanup(restore)
}

func backupGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=backup", "GIT_AUTHOR_EMAIL=backup@example.invalid", "GIT_COMMITTER_NAME=backup", "GIT_COMMITTER_EMAIL=backup@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func backupRepo(t *testing.T, payload int) string {
	t.Helper()
	repo := t.TempDir()
	backupGit(t, repo, "init", "-q")
	write(t, filepath.Join(repo, "payload"), strings.Repeat("x", payload))
	write(t, filepath.Join(repo, ".gitignore"), ".crw/\n")
	backupGit(t, repo, "add", ".")
	backupGit(t, repo, "commit", "-qm", "backup fixture")
	return repo
}

// preserveBackupFixture writes the cleanup proof a completed run's cleanup would have left: the
// preserved checkout ref plus its cleanup.jsonl record.
func preserveBackupFixture(t *testing.T, state, run, rel, repo string) {
	t.Helper()
	head := backupGit(t, repo, "rev-parse", "HEAD")
	tree := backupGit(t, repo, "rev-parse", "HEAD^{tree}")
	key := sha256.Sum256([]byte(rel + "\x00" + head + "\x00" + tree))
	ref := fmt.Sprintf("refs/crw/flow/%s/cleanup/%x", run, key)
	commit := backupGit(t, repo, "commit-tree", tree, "-p", head, "-m", "preserved fixture")
	backupGit(t, repo, "update-ref", ref, commit)
	raw, err := json.Marshal(map[string]string{"path": rel, "head": head, "snapshot": tree, "ref": ref, "commit": commit})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(state, "flow", run, "cleanup.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// writeBackupSpec writes the run's own spec, as a finished run leaves it.
func writeBackupSpec(t *testing.T, state, run, repo string) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"schema": "crw-flow-run/1", "run_id": run, "repo": repo,
		"workflow": "hello", "thread": "fixture-owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(state, "flow", run, "run.json"), string(raw))
}

// TestBackupStateKeepsUnpreservedWorktrees: a worktree with no recovery proof — paused or held work —
// is copied in full, including its ignored evidence, not omitted and not skipped.
func TestBackupStateKeepsUnpreservedWorktrees(t *testing.T) {
	for _, status := range []string{"paused", "held"} {
		t.Run(status, func(t *testing.T) {
			h := backupStateHost(t)
			stopped(t)
			repo := backupRepo(t, 128)
			run := "run-" + status
			rel := filepath.Join("flow", run, "wt", "n1")
			wt := filepath.Join(h.relayState, rel)
			backupGit(t, repo, "worktree", "add", "--detach", wt, "HEAD")
			writeBackupSpec(t, h.relayState, run, repo)
			write(t, filepath.Join(h.relayState, "flow", run, "result.json"), `{"status":"`+status+`"}`)
			write(t, filepath.Join(wt, "payload"), "uncommitted executor change\n")
			write(t, filepath.Join(wt, "pending.txt"), "P_held must survive\n")
			write(t, filepath.Join(wt, ".crw", "evidence", "shot.png"), "ignored evidence\n")
			pending, err := ops.Snapshot(context.Background(), ops.ExecGit("git"), wt)
			if err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(h.home, "unfinished-backup")
			result, code := install.BackupState(context.Background(), h.options(), dest)
			if code != install.OK {
				t.Fatalf("backup: %d\n%s", code, golden.Canon(result))
			}
			for _, file := range []string{"payload", "pending.txt", ".crw/evidence/shot.png"} {
				if got := mustRead(t, filepath.Join(dest, rel, file)); !bytes.Equal(got, mustRead(t, filepath.Join(wt, file))) {
					t.Fatalf("backup lost %s: %q", file, got)
				}
			}
			if err := os.RemoveAll(wt); err != nil {
				t.Fatal(err)
			}
			if err := os.CopyFS(wt, os.DirFS(filepath.Join(dest, rel))); err != nil {
				t.Fatal(err)
			}
			got, err := ops.Snapshot(context.Background(), ops.ExecGit("git"), wt)
			if err != nil || got != pending {
				t.Fatalf("restored pending tree: %s, want %s (%v)", got, pending, err)
			}
		})
	}
}

// TestBackupStateKeepsChangedPreservedWorktree: a preserved checkout whose working files moved past
// the recorded snapshot — including ignored captures — is copied with its new bytes; a stale proof
// never substitutes for them.
func TestBackupStateKeepsChangedPreservedWorktree(t *testing.T) {
	for _, kind := range []string{"tracked", "ignored", "assume-unchanged", "skip-worktree", "bad-record", "overlap", "missing-spec", "wrong-repo"} {
		t.Run(kind, func(t *testing.T) {
			h := backupStateHost(t)
			stopped(t)
			repo := backupRepo(t, 128)
			rel := filepath.Join("flow", "run-changed", "wt", "n1")
			wt := filepath.Join(h.relayState, rel)
			backupGit(t, repo, "worktree", "add", "--detach", wt, "HEAD")
			preserveBackupFixture(t, h.relayState, "run-changed", "wt/n1", repo)
			writeBackupSpec(t, h.relayState, "run-changed", repo)
			file := "payload"
			switch kind {
			case "tracked", "ignored", "assume-unchanged", "skip-worktree":
				if kind == "ignored" {
					file = ".crw/evidence/new.png"
				}
				write(t, filepath.Join(wt, file), "new work after the preserved snapshot\n")
				if kind == "assume-unchanged" || kind == "skip-worktree" {
					backupGit(t, wt, "update-index", "--"+kind, "--", file)
					if got := backupGit(t, wt, "status", "--porcelain"); got != "" {
						t.Fatalf("index fixture did not hide the change: %q", got)
					}
				}
			case "bad-record":
				record := filepath.Join(h.relayState, "flow", "run-changed", "cleanup.jsonl")
				raw := mustRead(t, record)
				write(t, record, string(raw)+"{\"path\":\"../escape\"}\n")
			case "overlap":
				preserveBackupFixture(t, h.relayState, "run-changed", "wt", repo)
			case "missing-spec":
				if err := os.Remove(filepath.Join(h.relayState, "flow", "run-changed", "run.json")); err != nil {
					t.Fatal(err)
				}
			case "wrong-repo":
				writeBackupSpec(t, h.relayState, "run-changed", backupRepo(t, 0))
			}
			want := mustRead(t, filepath.Join(wt, file))
			dest := filepath.Join(h.home, "changed-backup")
			result, code := install.BackupState(context.Background(), h.options(), dest)
			if code != install.OK {
				t.Fatalf("backup: %d\n%s", code, golden.Canon(result))
			}
			if got := mustRead(t, filepath.Join(dest, rel, file)); !bytes.Equal(got, want) {
				t.Fatalf("unusable proof lost working data: %q", got)
			}
		})
	}
}
