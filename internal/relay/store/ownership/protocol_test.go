package ownership_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/inbox"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"golang.org/x/sys/unix"
)

func seed(t *testing.T) *ownership.Controller {
	t.Helper()
	root := t.TempDir()
	// S is created owner-only by both runtimes; t.TempDir follows the test's umask.
	must(t, os.Chmod(root, 0700))
	path := filepath.Join(root, "relay.sqlite3")
	// This file was produced by the Python Store, not an abbreviated Go schema.
	raw, err := os.ReadFile("../../../../contract/fixtures/sqlite-ddl/python-store.sqlite3")
	must(t, err)
	must(t, os.WriteFile(path, raw, 0600))
	db, err := ownership.OpenExisting(t.Context(), path, "rw")
	must(t, err)
	_, err = db.Exec("DELETE FROM schema_meta WHERE key='socket_path'")
	must(t, err)
	must(t, db.Close())
	// The retained Python fence owns the stopped store, as its initializer leaves it.
	testsupport.Fence(t, path, "python")
	db, err = ownership.OpenExisting(t.Context(), path, "rw")
	must(t, err)
	_, err = db.Exec("INSERT OR REPLACE INTO schema_meta VALUES('test:retained','receipt-and-ack')")
	must(t, err)
	must(t, db.Close())
	c := controller(path)
	// The live host runs both runtimes under umask 002: every existing lock is 0664
	// inside an owner-only (0700) directory. The fixture reproduces those modes.
	for _, name := range lockNames {
		f, e := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_RDWR, 0600)
		must(t, e)
		must(t, f.Close())
		must(t, os.Chmod(filepath.Join(root, name), 0664))
	}
	return c
}

var lockNames = []string{"daemon.lock", "scope.lock", "write-gate.lock", "takeover.lock"}

type lockIdentity struct {
	inode uint64
	mode  uint32
}

func lockIdentities(t *testing.T, root string) map[string]lockIdentity {
	t.Helper()
	out := map[string]lockIdentity{}
	for _, name := range lockNames {
		var st unix.Stat_t
		must(t, unix.Stat(filepath.Join(root, name), &st))
		// Stat_t.Mode is uint16 on darwin and uint32 on linux.
		out[name] = lockIdentity{st.Ino, uint32(st.Mode) & 07777}
	}
	return out
}

// Audit finding 1 / decision D3: group-writable locks in an owner-only directory are
// the host's normal state. The full cutover and rollback run on them without any
// chmod, replacement or unlink; a group-writable directory still refuses.
func Test30GroupWritableLocksKeepInodeAndMode(t *testing.T) {
	c := seed(t)
	root := filepath.Dir(c.Path)
	before := lockIdentities(t, root)
	activeGo(t, c)
	must(t, c.Rollback(t.Context()))
	assertState(t, c, "python", "active", 3)
	if after := lockIdentities(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("locks changed: %v -> %v", before, after)
	}
	for name, id := range before {
		if id.mode != 0664 {
			t.Fatalf("%s mode %o", name, id.mode)
		}
	}
	must(t, os.Chmod(root, 0770))
	defer os.Chmod(root, 0700)
	if err := c.Begin(t.Context(), "go"); err == nil || !strings.Contains(err.Error(), "unsafe lock file") {
		t.Fatalf("group-writable lock directory accepted: %v", err)
	}
}
func controller(path string) *ownership.Controller {
	return &ownership.Controller{Path: path, ScopeLock: filepath.Join(filepath.Dir(path), "scope.lock"), Identity: ownership.Identity{BootID: "test-boot", PID: os.Getpid(), StartTicks: 1}, Runtime: modelRuntime{}, ValidateSchema: store.ValidateOwnershipSchema, ValidateInbox: inbox.Check}
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// The matrix exercises the durable controller independently of host recovery.
// The candidate model holds the same three real flocks and a real SQLite
// connection, and checks the durable stamp on readiness/activation.
type modelRuntime struct{}

func (modelRuntime) Preflight(context.Context, ownership.Record, string) error { return nil }
func (modelRuntime) Drain(context.Context, ownership.Record) error             { return nil }
func (modelRuntime) Start(ctx context.Context, r ownership.Record) (ownership.CandidateProcess, error) {
	p := &modelCandidate{record: r}
	for _, name := range []string{"daemon.lock", "scope.lock", "write-gate.lock"} {
		f, e := ownership.Lock(filepath.Join(filepath.Dir(r.Database.RealPath), name), name != "write-gate.lock", false)
		if e != nil {
			_ = p.Close()
			return nil, e
		}
		p.locks = append(p.locks, f)
	}
	var err error
	p.db, err = ownership.OpenExisting(ctx, r.Database.RealPath, "rw")
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	s, err := ownership.ReadStamp(ctx, p.db)
	if err == nil {
		err = ownership.Validate(r.Database.RealPath, r, s)
	}
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	return p, nil
}

type modelCandidate struct {
	record ownership.Record
	locks  []*os.File
	db     *sql.DB
}

func (p *modelCandidate) Identity() ownership.Identity {
	return ownership.Identity{BootID: "test-boot", PID: os.Getpid(), StartTicks: 1, Build: "test-build"}
}
func (p *modelCandidate) Activate(ctx context.Context, r ownership.Record) error {
	s, e := ownership.ReadStamp(ctx, p.db)
	if e != nil {
		return e
	}
	return ownership.Validate(r.Database.RealPath, r, s)
}
func (p *modelCandidate) Close() error {
	var err error
	if p.db != nil {
		err = p.db.Close()
		p.db = nil
	}
	for i := len(p.locks) - 1; i >= 0; i-- {
		err = errors.Join(err, p.locks[i].Close())
	}
	p.locks = nil
	return err
}

func activeGo(t *testing.T, c *ownership.Controller) {
	t.Helper()
	must(t, c.Begin(t.Context(), "go"))
	must(t, c.Drain(t.Context()))
	must(t, c.Transfer(t.Context()))
	must(t, c.Activate(t.Context()))
}
func assertState(t *testing.T, c *ownership.Controller, owner, phase string, epoch int64) {
	t.Helper()
	s, e := c.Status(t.Context())
	must(t, e)
	if s.Owner != owner || s.Phase != phase || s.Epoch != epoch {
		t.Fatalf("state: %+v", s)
	}
	db, e := ownership.OpenExisting(t.Context(), c.Path, "ro")
	must(t, e)
	defer db.Close()
	var retained string
	must(t, db.QueryRow("SELECT value FROM schema_meta WHERE key='test:retained'").Scan(&retained))
	if retained != "receipt-and-ack" {
		t.Fatal("lost rows")
	}
}
func Test30HappyRollbackSameLiveStore(t *testing.T) {
	c := seed(t)
	before, e := ownership.Physical(c.Path)
	must(t, e)
	activeGo(t, c)
	assertState(t, c, "go", "active", 2)
	db, e := store.Open(t.Context(), c.Path, "")
	must(t, e)
	_, e = db.DB.Exec("INSERT INTO schema_meta VALUES('test:after-go','accepted-while-go-owned')")
	must(t, e)
	must(t, db.Close())
	must(t, c.Rollback(t.Context()))
	assertState(t, c, "python", "active", 3)
	after, e := ownership.Physical(c.Path)
	must(t, e)
	if before != after {
		t.Fatal("rollback replaced physical DB")
	}
	read, e := ownership.OpenExisting(t.Context(), c.Path, "ro")
	must(t, e)
	defer read.Close()
	var retained string
	must(t, read.QueryRow("SELECT value FROM schema_meta WHERE key='test:after-go'").Scan(&retained))
	if retained != "accepted-while-go-owned" {
		t.Fatal("rollback restored a snapshot")
	}
	must(t, c.Rollback(t.Context()))
	assertState(t, c, "python", "active", 3)
	activeGo(t, c)
	assertState(t, c, "go", "active", 4)
}
func Test30CommitIrreversibleAndResumable(t *testing.T) {
	c := seed(t)
	activeGo(t, c)
	must(t, c.Commit(t.Context()))
	must(t, c.Commit(t.Context()))
	if c.Rollback(t.Context()) == nil {
		t.Fatal("rollback allowed after commit")
	}
	s, e := c.Status(t.Context())
	must(t, e)
	if s.RollbackAllowed {
		t.Fatal("commit not durable")
	}
}

// Each entry is a reachable durable edge, not a no-op injected fault. Every
// listed edge is executed by a real child that exits without running defers.
var crashPoints = map[string][]string{
	"begin":             {"locked", "temp-written", "file-synced", "renamed", "directory-synced"},
	"drain":             {"locked", "drained"},
	"snapshot":          {"backup-synced", "temp-written", "file-synced", "renamed", "directory-synced"},
	"transfer":          {"locked", "barrier", "snapshot", "before-commit", "db-committed", "temp-written", "file-synced", "renamed", "directory-synced"},
	"activate":          {"locked", "temp-written", "file-synced", "renamed", "directory-synced", "ready", "active-published"},
	"activate-active":   {"temp-written", "file-synced", "renamed", "directory-synced"},
	"rollback":          {"locked", "temp-written", "file-synced", "renamed", "directory-synced", "drained"},
	"rollback-transfer": {"barrier", "snapshot", "before-commit", "db-committed", "temp-written", "file-synced", "renamed", "directory-synced"},
	"rollback-activate": {"locked", "temp-written", "file-synced", "renamed", "directory-synced", "ready", "active-published"},
	"rollback-active":   {"temp-written", "file-synced", "renamed", "directory-synced"},
	"commit":            {"locked", "before-commit", "db-committed", "temp-written", "file-synced", "renamed", "directory-synced"},
}

func Test30CrashProcess(t *testing.T) {
	path := os.Getenv("CRW30_CRASH_PATH")
	if path == "" {
		return
	}
	c := controller(path)
	step := os.Getenv("CRW30_STEP")
	point := os.Getenv("CRW30_POINT")
	actual := step
	if step == "rollback-activate" {
		actual = "activate"
	}
	if step == "rollback-active" {
		actual = "activate-active"
	}
	c.Fault = func(s, p string) error {
		if s == actual && p == point {
			os.Exit(91)
		}
		return nil
	}
	var err error
	switch step {
	case "begin":
		err = c.Begin(t.Context(), "go")
	case "drain":
		err = c.Drain(t.Context())
	case "transfer", "snapshot":
		err = c.Transfer(t.Context())
	case "activate", "activate-active", "rollback-activate", "rollback-active":
		err = c.Activate(t.Context())
	case "rollback", "rollback-transfer":
		err = c.Rollback(t.Context())
	case "commit":
		err = c.Commit(t.Context())
	}
	t.Fatalf("crash boundary was not reached: %s/%s: %v", step, point, err)
}
func Test30CrashMatrix(t *testing.T) {
	for _, step := range []string{"begin", "drain", "snapshot", "transfer", "activate", "activate-active", "rollback", "rollback-transfer", "rollback-activate", "rollback-active", "commit"} {
		for _, point := range crashPoints[step] {
			t.Run(step+"/"+point, func(t *testing.T) {
				c := seed(t)
				switch step {
				case "drain", "transfer", "snapshot":
					must(t, c.Begin(t.Context(), "go"))
				case "activate", "activate-active":
					must(t, c.Begin(t.Context(), "go"))
					must(t, c.Transfer(t.Context()))
				case "rollback", "rollback-transfer", "commit":
					activeGo(t, c)
				case "rollback-activate", "rollback-active":
					activeGo(t, c)
					must(t, c.Begin(t.Context(), "python"))
					must(t, c.Transfer(t.Context()))
				}
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				child := exec.CommandContext(ctx, os.Args[0], "-test.run=^Test30CrashProcess$")
				child.Env = append(os.Environ(), "CRW30_CRASH_PATH="+c.Path, "CRW30_STEP="+step, "CRW30_POINT="+point)
				raw, e := child.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(e, &exit) || exit.ExitCode() != 91 {
					t.Fatalf("child: %v %s", e, raw)
				}
				status, e := c.Status(t.Context())
				must(t, e)
				if step == "transfer" && point == "db-committed" {
					if status.Owner != "go" || status.Phase != "starting" || !status.JSONStale {
						t.Fatalf("torn CAS recovery: %+v", status)
					}
				}
				// Recover only from the DB-backed status, never blindly replay old JSON.
				if strings.HasPrefix(step, "rollback") {
					must(t, c.Rollback(t.Context()))
					assertState(t, c, "python", "active", 3)
				} else if step == "commit" {
					must(t, c.Commit(t.Context()))
					assertState(t, c, "go", "active", 2)
				} else {
					must(t, c.Begin(t.Context(), "go"))
					must(t, c.Drain(t.Context()))
					must(t, c.Transfer(t.Context()))
					must(t, c.Activate(t.Context()))
					assertState(t, c, "go", "active", 2)
				}
				final, e := c.Status(t.Context())
				must(t, e)
				t.Logf("CRASH_MATRIX {\"step\":%q,\"crash_point\":%q,\"recovered_state\":%q,\"legal\":true}", step, point, final.String())
			})
		}
	}
}
func Test30BackupIncludesWALAndInventory(t *testing.T) {
	c := seed(t)
	// Queued inbox entries are inventoried on the transfer that carries them, in either
	// direction (the CAS toward Go does not wait for them: the Go candidate drains them,
	// todo 31); this transfer is toward Python.
	activeGo(t, c)
	writer, e := ownership.OpenExisting(t.Context(), c.Path, "rw")
	must(t, e)
	defer writer.Close()
	_, e = writer.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; INSERT INTO schema_meta VALUES('test:wal-only','present')")
	must(t, e)
	// A queued entry is inventoried as the entry the candidate's drain will read, so it is a
	// real one (Test30TransferInspectsTheInboxAsTheDrainReadsIt).
	queued, e := inbox.Envelope("ack", []string{"--event", "event-1", "--ack-turn", "parent-turn", "--ack-proof", "proof-1"})
	must(t, e)
	must(t, inbox.Enqueue(filepath.Dir(c.Path), queued))
	for name, value := range map[string]string{"receiver-ledger.json": "receiver", "transport-ledger.json": "transport"} {
		path := filepath.Join(filepath.Dir(c.Path), name)
		must(t, os.MkdirAll(filepath.Dir(path), 0700))
		must(t, os.WriteFile(path, []byte(value), 0600))
	}
	must(t, c.Begin(t.Context(), "python"))
	must(t, c.Transfer(t.Context()))
	status, e := c.Status(t.Context())
	must(t, e)
	dir := filepath.Join(filepath.Dir(c.Path), "takeover-backups", status.TakeoverID)
	backup, e := ownership.OpenExisting(t.Context(), filepath.Join(dir, "relay.sqlite3"), "ro")
	must(t, e)
	defer backup.Close()
	var wal string
	must(t, backup.QueryRow("SELECT value FROM schema_meta WHERE key='test:wal-only'").Scan(&wal))
	if wal != "present" {
		t.Fatal("backup lost WAL")
	}
	raw, e := os.ReadFile(filepath.Join(dir, "inventory.json"))
	must(t, e)
	var inventory ownership.Inventory
	must(t, json.Unmarshal(raw, &inventory))
	if len(inventory.Files) != 3 || inventory.Tables["schema_meta"] < 7 {
		t.Fatalf("inventory incomplete: %+v", inventory)
	}
}

// Backlog line 40: Step 4 reads S/takeover-inbox as the candidate's drain will (inbox.readEntry
// and validate over the names the decision-25 grammar admits), so what the transfer refuses and
// what the drain fails closed on are the same set. A corrupt, symlinked or non-regular entry is
// refused at Step 4, where the owner has not changed and the recovery is repair or abort
// (cutover.md Step 4), never after Step 5 commits owner=go phase=starting, and so is a replay
// lock the drain cannot open. Any other name outside the grammar and anything below a
// subdirectory are nothing the drain reads, so they refuse nothing.
func Test30TransferInspectsTheInboxAsTheDrainReadsIt(t *testing.T) {
	valid, err := inbox.Envelope("ack", []string{"--event", "event-1", "--ack-turn", "parent-turn", "--ack-proof", "proof-1"})
	must(t, err)
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, directory string) // an inbox that already holds valid
		refused string                               // "" when the transfer proceeds
	}{
		{"a corrupt entry", func(t *testing.T, directory string) {
			must(t, os.WriteFile(filepath.Join(directory, "ack.corrupt"), []byte("not json"), 0600))
		}, "JSONDecodeError: Expecting value: line 1 column 1 (char 0)"},
		{"a non-canonical entry", func(t *testing.T, directory string) {
			must(t, os.WriteFile(filepath.Join(directory, "ack.event-2"), append(append([]byte{}, valid.Raw...), ' '), 0600))
		}, "ValueError: invalid takeover inbox entry: ack.event-2"},
		{"a symlinked entry", func(t *testing.T, directory string) {
			must(t, os.Symlink(valid.ID, filepath.Join(directory, "ack.link")))
		}, "ValueError: invalid takeover inbox entry: ack.link: symbolic link"},
		{"a directory entry", func(t *testing.T, directory string) {
			must(t, os.Mkdir(filepath.Join(directory, "ack.directory"), 0700))
		}, "ValueError: invalid takeover inbox entry: ack.directory: not a regular file"},
		// The drain opens its replay lock before any entry, without following a link and for
		// writing: a lock it cannot open stops it as surely as a corrupt entry.
		{"a symlinked replay lock", func(t *testing.T, directory string) {
			must(t, removeIfPresent(filepath.Join(directory, inbox.ReplayLock)))
			must(t, os.Symlink("elsewhere", filepath.Join(directory, inbox.ReplayLock)))
		}, "OSError: [Errno 40] Too many levels of symbolic links: '"},
		{"a replay lock that is a directory", func(t *testing.T, directory string) {
			must(t, removeIfPresent(filepath.Join(directory, inbox.ReplayLock)))
			must(t, os.Mkdir(filepath.Join(directory, inbox.ReplayLock), 0700))
		}, "IsADirectoryError: [Errno 21] Is a directory: '"},
		{"names the drain never reads", func(t *testing.T, directory string) {
			must(t, os.Symlink("/nonexistent", filepath.Join(directory, "not an entry")))
			must(t, os.Symlink(valid.ID, filepath.Join(directory, ".hidden")))
			nested := filepath.Join(directory, "saved~")
			must(t, os.Mkdir(nested, 0700))
			must(t, os.Symlink("/nonexistent", filepath.Join(nested, "ack.event-1")))
			must(t, unix.Mkfifo(filepath.Join(nested, "fifo"), 0600))
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := seed(t)
			state := filepath.Dir(c.Path)
			must(t, c.Begin(t.Context(), "go"))
			must(t, c.Drain(t.Context()))
			must(t, inbox.Enqueue(state, valid))
			directory := inbox.Directory(state)
			tc.prepare(t, directory)
			err := c.Transfer(t.Context())
			if tc.refused == "" {
				must(t, err)
				status, e := c.Status(t.Context())
				must(t, e)
				raw, e := os.ReadFile(filepath.Join(state, "takeover-backups", status.TakeoverID, "inventory.json"))
				must(t, e)
				var inventory ownership.Inventory
				must(t, json.Unmarshal(raw, &inventory))
				var inboxFiles []string
				for _, file := range inventory.Files {
					if strings.HasPrefix(file.Path, "takeover-inbox/") {
						inboxFiles = append(inboxFiles, file.Path)
					}
				}
				if !reflect.DeepEqual(inboxFiles, []string{"takeover-inbox/" + valid.ID}) {
					t.Fatalf("the inventory's inbox is not the drain's: %v", inboxFiles)
				}
				assertState(t, c, "go", "starting", 2)
				return
			}
			var refusal *ownership.Refused
			if !errors.As(err, &refusal) || !strings.Contains(refusal.Detail, tc.refused) {
				t.Fatalf("transfer over %s: %v", tc.name, err)
			}
			// Refused before the ownership-transfer point: the owner has not changed.
			assertState(t, c, "python", "draining", 1)
			// Recovery is repair (cutover.md Step 4): without the entry, or the lock, that stops
			// the drain, the transfer proceeds.
			entries, e := os.ReadDir(directory)
			must(t, e)
			for _, entry := range entries {
				if entry.Name() != valid.ID && (entry.Name() != inbox.ReplayLock || !entry.Type().IsRegular()) {
					must(t, os.RemoveAll(filepath.Join(directory, entry.Name())))
				}
			}
			must(t, c.Transfer(t.Context()))
			assertState(t, c, "go", "starting", 2)
		})
	}
}

// removeIfPresent removes path, which may already be absent.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func Test30DisagreementNeverRepairs(t *testing.T) {
	for _, what := range []string{"owner", "epoch", "inode", "build", "transition", "missing-json", "missing-key", "missing-db", "rollback", "protocol", "missing-field", "missing-gate", "scope", "transition-path", "unsupported-db"} {
		t.Run(what, func(t *testing.T) {
			c := seed(t)
			activeGo(t, c)
			admitted, e := ownership.Admit(t.Context(), c.Path)
			must(t, e)
			must(t, admitted.Close())
			r, e := ownership.ReadRecord(c.Path)
			must(t, e)
			switch what {
			case "owner":
				r.Owner = "python"
			case "epoch":
				r.Epoch++
			case "inode":
				r.Database.Inode++
			case "build":
				r.PythonCompatibilityBuild = "other"
			case "transition":
				r.Transition = &ownership.Transition{ID: "foreign", From: "python", To: "go", TargetEpoch: 8}
			case "rollback":
				r.RollbackAllowed = false
			case "protocol":
				r.Protocol = 2
			case "scope":
				socket, key := "/different/socket", "wrong-key"
				r.AppServerSocket = &socket
				r.ScopeKey = &key
			case "transition-path":
				r.Transition = &ownership.Transition{ID: "../elsewhere", From: "python", To: "go", TargetEpoch: 2}
				r.Phase = "draining"
			}
			must(t, ownership.Publish(c.Path, r, nil))
			switch what {
			case "missing-json":
				must(t, os.Remove(filepath.Join(filepath.Dir(c.Path), "takeover.json")))
			case "missing-gate":
				must(t, os.Remove(filepath.Join(filepath.Dir(c.Path), "write-gate.lock")))
			case "unsupported-db":
				db, e := ownership.OpenExisting(t.Context(), c.Path, "rw")
				must(t, e)
				_, e = db.Exec("UPDATE schema_meta SET value='2' WHERE key='writer_protocol'")
				must(t, e)
				must(t, db.Close())
			case "missing-key":
				db, e := ownership.OpenExisting(t.Context(), c.Path, "rw")
				must(t, e)
				_, e = db.Exec("DELETE FROM schema_meta WHERE key='owner'")
				must(t, e)
				must(t, db.Close())
			case "missing-db":
				must(t, os.Remove(c.Path))
			case "missing-field":
				raw, e := os.ReadFile(filepath.Join(filepath.Dir(c.Path), "takeover.json"))
				must(t, e)
				var m map[string]any
				must(t, json.Unmarshal(raw, &m))
				delete(m, "rollbackAllowed")
				raw, e = json.Marshal(m)
				must(t, e)
				must(t, os.WriteFile(filepath.Join(filepath.Dir(c.Path), "takeover.json"), raw, 0600))
			}
			before := treeBytes(t, filepath.Dir(c.Path))
			if a, e := ownership.Admit(t.Context(), c.Path); e == nil {
				_ = a.Close()
				t.Fatal("admitted disagreement")
			}
			after := treeBytes(t, filepath.Dir(c.Path))
			if !reflect.DeepEqual(before, after) {
				t.Fatal("refusal changed source bytes/files")
			}
		})
	}
}
func treeBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	must(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		raw, e := os.ReadFile(path)
		if e == nil {
			m[path] = string(raw)
		}
		return e
	}))
	return m
}

func Test30WriterProcess(t *testing.T) {
	path := os.Getenv("CRW30_WRITER_PATH")
	if path == "" {
		return
	}
	db, e := store.Open(t.Context(), path, "")
	must(t, e)
	defer db.Close()
	must(t, db.Transaction(t.Context(), func(ctx context.Context, conn *sql.Conn) error {
		if _, e := conn.ExecContext(ctx, "INSERT INTO schema_meta VALUES('test:mid-transaction','retained')"); e != nil {
			return e
		}
		if _, e := fmt.Fprintln(os.Stdout, "WRITER_READY"); e != nil {
			return e
		}
		var b [1]byte
		_, e := io.ReadFull(os.Stdin, b[:])
		return e
	}))
}
func Test30TwoProcessWriterBlocksTransfer(t *testing.T) {
	c := seed(t)
	activeGo(t, c)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^Test30WriterProcess$")
	child.Env = append(os.Environ(), "CRW30_WRITER_PATH="+c.Path)
	input, e := child.StdinPipe()
	must(t, e)
	output, e := child.StdoutPipe()
	must(t, e)
	child.Stderr = os.Stderr
	must(t, child.Start())
	defer func() {
		_ = input.Close()
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	// Subscribe before triggering transfer: the child announces from inside its
	// actual BEGIN IMMEDIATE. No sleep, retry loop, or scheduler assumption.
	ready := make([]byte, len("WRITER_READY\n"))
	_, e = io.ReadFull(output, ready)
	must(t, e)
	if string(ready) != "WRITER_READY\n" {
		t.Fatalf("writer signal %q", ready)
	}
	must(t, c.Begin(t.Context(), "python"))
	barrierPassed := false
	c.Fault = func(step, point string) error {
		if step == "transfer" && point == "barrier" {
			barrierPassed = true
		}
		return nil
	}
	if e = c.Transfer(t.Context()); !errors.Is(e, unix.EWOULDBLOCK) || barrierPassed {
		t.Fatalf("writer did not block at the flock barrier: passed=%t err=%v", barrierPassed, e)
	}
	c.Fault = nil
	assertState(t, c, "go", "draining", 2)
	_, e = input.Write([]byte{1})
	must(t, e)
	must(t, input.Close())
	must(t, child.Wait())
	must(t, c.Transfer(t.Context()))
	must(t, c.Activate(t.Context()))
	assertState(t, c, "python", "active", 3)
	db, e := ownership.OpenExisting(t.Context(), c.Path, "ro")
	must(t, e)
	defer db.Close()
	var retained string
	must(t, db.QueryRow("SELECT value FROM schema_meta WHERE key='test:mid-transaction'").Scan(&retained))
	if retained != "retained" {
		t.Fatal("admitted write lost")
	}
}

func Test30StartingRequiresExactCandidatePermit(t *testing.T) {
	c := seed(t)
	must(t, c.Begin(t.Context(), "go"))
	must(t, c.Transfer(t.Context()))
	r, err := ownership.ReadRecord(c.Path)
	must(t, err)
	valid := ownership.Candidate{TransitionID: r.Transition.ID, Epoch: r.Epoch, Controller: *r.Controller}
	for _, mismatch := range []string{"ordinary", "transition", "epoch", "controller"} {
		t.Run(mismatch, func(t *testing.T) {
			permit := valid
			switch mismatch {
			case "transition":
				permit.TransitionID = "another-transition"
			case "epoch":
				permit.Epoch++
			case "controller":
				permit.Controller.PID++
			}
			ctx := t.Context()
			if mismatch != "ordinary" {
				ctx = ownership.WithCandidate(ctx, permit)
			}
			before := treeBytes(t, filepath.Dir(c.Path))
			if db, e := store.Open(ctx, c.Path, ""); e == nil {
				_ = db.Close()
				t.Fatal("starting writer admitted without exact candidate permit")
			}
			if !reflect.DeepEqual(before, treeBytes(t, filepath.Dir(c.Path))) {
				t.Fatal("candidate refusal changed state")
			}
		})
	}
	db, err := store.Open(ownership.WithCandidate(t.Context(), valid), c.Path, "")
	must(t, err)
	must(t, db.Transaction(t.Context(), func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, "INSERT INTO schema_meta VALUES('test:candidate','recovery')")
		return err
	}))
	must(t, db.Close())
}

type failingDrain struct{ modelRuntime }

func (failingDrain) Drain(context.Context, ownership.Record) error {
	return errors.New("holder did not exit")
}

// pythonCheckStart runs the retained Python fence's own preflight on the store: the
// oracle for whether Python admission reopens after an abort.
func pythonCheckStart(t *testing.T, path string) error {
	t.Helper()
	cmd := exec.Command("../../../../.venv/bin/python", "-c", "import sys\nfrom codex_session_relay import ownership\nownership.check_start(sys.argv[1])", path)
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, raw)
	}
	return nil
}

// Audit findings 2, 6, 10 / decision D2: a failure after begin and before the CAS
// returns to the unchanged owner's active phase; after the CAS abort refuses and names
// the recovery; the reverse transfer from a failed candidate installs a new epoch.
func Test30AbortReturnsToActiveOwner(t *testing.T) {
	t.Run("forward-failed-drain", func(t *testing.T) {
		c := seed(t)
		must(t, c.Begin(t.Context(), "go"))
		c.Runtime = failingDrain{}
		if err := c.Drain(t.Context()); err == nil {
			t.Fatal("failing drain succeeded")
		}
		if err := pythonCheckStart(t, c.Path); err == nil || !strings.Contains(err.Error(), "draining") {
			t.Fatalf("Python admission open while draining: %v", err)
		}
		must(t, c.Abort(t.Context()))
		must(t, c.Abort(t.Context()))
		assertState(t, c, "python", "active", 1)
		r, err := ownership.ReadRecord(c.Path)
		must(t, err)
		if r.Transition != nil {
			t.Fatalf("initial install kept a transition: %+v", r.Transition)
		}
		must(t, pythonCheckStart(t, c.Path))
		c.Runtime = modelRuntime{}
		activeGo(t, c)
		assertState(t, c, "go", "active", 2)
	})
	t.Run("rollback-and-round-trip", func(t *testing.T) {
		c := seed(t)
		activeGo(t, c)
		installed, err := c.Status(t.Context())
		must(t, err)
		must(t, c.Begin(t.Context(), "python"))
		must(t, c.Abort(t.Context()))
		assertState(t, c, "go", "active", 2)
		r, err := ownership.ReadRecord(c.Path)
		must(t, err)
		if r.Transition == nil || r.Transition.ID != installed.TakeoverID || r.Transition.From != "python" || r.Transition.To != "go" || r.Transition.TargetEpoch != 2 {
			t.Fatalf("installed transition not restored: %+v", r.Transition)
		}
		admitted, err := ownership.Admit(t.Context(), c.Path)
		must(t, err)
		must(t, admitted.Close())
		must(t, c.Rollback(t.Context()))
		assertState(t, c, "python", "active", 3)
		must(t, c.Begin(t.Context(), "go"))
		must(t, c.Abort(t.Context()))
		assertState(t, c, "python", "active", 3)
		must(t, pythonCheckStart(t, c.Path))
		activeGo(t, c)
		assertState(t, c, "go", "active", 4)
	})
	t.Run("refused-after-cas", func(t *testing.T) {
		c := seed(t)
		must(t, c.Begin(t.Context(), "go"))
		must(t, c.Transfer(t.Context()))
		err := c.Abort(t.Context())
		if err == nil || !strings.Contains(err.Error(), "takeover activate") || !strings.Contains(err.Error(), "rollback --to python") {
			t.Fatalf("abort after the CAS: %v", err)
		}
		assertState(t, c, "go", "starting", 2)
	})
	t.Run("reverse-transfer-from-failed-python-candidate", func(t *testing.T) {
		c := seed(t)
		activeGo(t, c)
		must(t, c.Begin(t.Context(), "python"))
		must(t, c.Transfer(t.Context()))
		assertState(t, c, "python", "starting", 3)
		if err := c.Abort(t.Context()); err == nil || !strings.Contains(err.Error(), "begin --to go") {
			t.Fatalf("abort after the reverse CAS: %v", err)
		}
		activeGo(t, c)
		assertState(t, c, "go", "active", 4)
	})
}

// Review of decision D2: a reverse transfer begun from a failed candidate's starting
// phase that fails before its CAS aborts back to starting, never to active. No
// candidate of that owner became ready, so no active service may be advertised;
// activation and the reverse transfer stay the only recoveries (cutover Step 5).
func Test30AbortOfReverseBeginReturnsToStarting(t *testing.T) {
	aborted := func(t *testing.T, c *ownership.Controller, owner string, epoch int64, installed string) {
		t.Helper()
		must(t, c.Abort(t.Context()))
		assertState(t, c, owner, "starting", epoch)
		status, err := c.Status(t.Context())
		must(t, err)
		if status.JSONStale || status.Holder != nil || status.TakeoverID != installed {
			t.Fatalf("aborted reverse begin: %+v", status)
		}
		r, err := ownership.ReadRecord(c.Path)
		must(t, err)
		if r.Transition == nil || r.Transition.ID != installed || r.Transition.To != owner || r.Transition.TargetEpoch != epoch {
			t.Fatalf("installed transition not restored: %+v", r.Transition)
		}
		if err = c.Abort(t.Context()); err == nil || !strings.Contains(err.Error(), "takeover activate") {
			t.Fatalf("repeated abort from starting: %v", err)
		}
		assertState(t, c, owner, "starting", epoch)
	}
	t.Run("go-candidate-failed", func(t *testing.T) {
		c := seed(t)
		must(t, c.Begin(t.Context(), "go"))
		must(t, c.Drain(t.Context()))
		must(t, c.Transfer(t.Context()))
		installed, err := c.Status(t.Context())
		must(t, err)
		// The rollback's drain fails before its CAS, as a busy daemon.lock does.
		c.Runtime = failingDrain{}
		if err = c.Rollback(t.Context()); err == nil {
			t.Fatal("rollback with a failing drain succeeded")
		}
		assertState(t, c, "go", "draining", 2)
		aborted(t, c, "go", 2, installed.TakeoverID)
		if a, err := ownership.Admit(t.Context(), c.Path); err == nil {
			_ = a.Close()
			t.Fatal("ordinary Go writer admitted after aborting to starting")
		}
		c.Runtime = modelRuntime{}
		must(t, c.Activate(t.Context()))
		assertState(t, c, "go", "active", 2)
	})
	t.Run("python-candidate-failed", func(t *testing.T) {
		c := seed(t)
		activeGo(t, c)
		must(t, c.Begin(t.Context(), "python"))
		must(t, c.Transfer(t.Context()))
		installed, err := c.Status(t.Context())
		must(t, err)
		must(t, c.Begin(t.Context(), "go"))
		assertState(t, c, "python", "draining", 3)
		aborted(t, c, "python", 3, installed.TakeoverID)
		if err = pythonCheckStart(t, c.Path); err == nil || !strings.Contains(err.Error(), "starting") {
			t.Fatalf("Python admission open after aborting to starting: %v", err)
		}
		activeGo(t, c)
		assertState(t, c, "go", "active", 4)
	})
}

// Audit findings 21, 29, 52: a state directory reached through a symlink is the same
// S in both runtimes; its mirror names the resolved S/control.sock.
func Test30ReadRecordThroughSymlinkedState(t *testing.T) {
	c := seed(t)
	link := filepath.Join(t.TempDir(), "state-link")
	must(t, os.Symlink(filepath.Dir(c.Path), link))
	through := filepath.Join(link, "relay.sqlite3")
	must(t, pythonCheckStart(t, through))
	activeGo(t, c)
	r, err := ownership.ReadRecord(through)
	must(t, err)
	if r.RelayRPCSocket != filepath.Join(filepath.Dir(c.Path), "control.sock") {
		t.Fatal(r.RelayRPCSocket)
	}
	admitted, err := ownership.Admit(t.Context(), through)
	must(t, err)
	must(t, admitted.Close())
}

// Audit finding 27: between takeover commit's COMMIT and its mirror publication the
// live Go daemon keeps serving; the reverse disagreement still refuses.
func Test30CommitTearKeepsGoWritersAdmitted(t *testing.T) {
	c := seed(t)
	activeGo(t, c)
	live, err := store.Open(t.Context(), c.Path, "")
	must(t, err)
	defer live.Close()
	raw, err := ownership.OpenExisting(t.Context(), c.Path, "rw")
	must(t, err)
	_, err = raw.Exec("UPDATE schema_meta SET value='0' WHERE key='rollback_allowed'")
	must(t, errors.Join(err, raw.Close()))
	must(t, live.Transaction(t.Context(), func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, "INSERT INTO schema_meta VALUES('test:during-tear','served')")
		return err
	}))
	admitted, err := ownership.Admit(t.Context(), c.Path)
	must(t, err)
	must(t, admitted.Close())
	must(t, c.Commit(t.Context()))
	status, err := c.Status(t.Context())
	must(t, err)
	if status.RollbackAllowed || status.JSONStale {
		t.Fatalf("commit not republished: %+v", status)
	}
	// The opposite disagreement is not a tear: the mirror may never lead the DB.
	r, err := ownership.ReadRecord(c.Path)
	must(t, err)
	raw, err = ownership.OpenExisting(t.Context(), c.Path, "rw")
	must(t, err)
	_, err = raw.Exec("UPDATE schema_meta SET value='1' WHERE key='rollback_allowed'")
	must(t, errors.Join(err, raw.Close()))
	must(t, ownership.Publish(c.Path, r, nil))
	if a, err := ownership.Admit(t.Context(), c.Path); err == nil {
		_ = a.Close()
		t.Fatal("mirror rollbackAllowed=false against DB 1 admitted")
	}
}

type refusingPreflight struct {
	modelRuntime
	drained *bool
}

func (refusingPreflight) Preflight(_ context.Context, _ ownership.Record, to string) error {
	return fmt.Errorf("no %s candidate can be launched", to)
}
func (r refusingPreflight) Drain(context.Context, ownership.Record) error {
	*r.drained = true
	return nil
}

// Decision D1: every launch precondition is checked before a durable edge. A missing
// one refuses begin, a resumed rollback before its drain, transfer before the CAS and
// activation before its launch; nothing is stranded.
func Test30PreflightPrecedesEveryDurableEdge(t *testing.T) {
	c := seed(t)
	drained := false
	c.Runtime = refusingPreflight{drained: &drained}
	before := treeBytes(t, filepath.Dir(c.Path))
	if err := c.Begin(t.Context(), "go"); err == nil {
		t.Fatal("begin published without a launchable candidate")
	}
	if !reflect.DeepEqual(before, treeBytes(t, filepath.Dir(c.Path))) {
		t.Fatal("refused begin changed the state directory")
	}
	c.Runtime = modelRuntime{}
	activeGo(t, c)
	must(t, c.Begin(t.Context(), "python"))
	c.Runtime = refusingPreflight{drained: &drained}
	if err := c.Rollback(t.Context()); err == nil || drained {
		t.Fatalf("resumed rollback drained=%t: %v", drained, err)
	}
	if err := c.Transfer(t.Context()); err == nil {
		t.Fatal("transfer committed without a launchable candidate")
	}
	assertState(t, c, "go", "draining", 2)
	c.Runtime = modelRuntime{}
	must(t, c.Transfer(t.Context()))
	c.Runtime = refusingPreflight{drained: &drained}
	if err := c.Activate(t.Context()); err == nil {
		t.Fatal("activation launched without its preconditions")
	}
	assertState(t, c, "python", "starting", 3)
}
