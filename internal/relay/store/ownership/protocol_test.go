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

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"golang.org/x/sys/unix"
)

func seed(t *testing.T) *ownership.Controller {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "relay.sqlite3")
	// This file was produced by the Python Store, not an abbreviated Go schema.
	raw, err := os.ReadFile("../../../../contract/fixtures/sqlite-ddl/python-store.sqlite3")
	must(t, err)
	must(t, os.WriteFile(path, raw, 0600))
	db, err := ownership.OpenExisting(t.Context(), path, "rw")
	must(t, err)
	_, err = db.Exec("DELETE FROM schema_meta WHERE key='socket_path'")
	must(t, err)
	must(t, testsupport.FenceFixture(t.Context(), db, path, "", "python"))
	_, err = db.Exec("INSERT OR REPLACE INTO schema_meta VALUES('test:retained','receipt-and-ack')")
	must(t, err)
	must(t, db.Close())
	c := controller(path)
	for _, name := range []string{"daemon.lock", "scope.lock"} {
		f, e := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_RDWR, 0600)
		must(t, e)
		must(t, f.Close())
	}
	return c
}
func controller(path string) *ownership.Controller {
	return &ownership.Controller{Path: path, ScopeLock: filepath.Join(filepath.Dir(path), "scope.lock"), Identity: ownership.Identity{BootID: "test-boot", PID: os.Getpid(), StartTicks: 1}, Runtime: modelRuntime{}, ValidateSchema: store.ValidateOwnershipSchema}
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

func (modelRuntime) Drain(context.Context, ownership.Record) error { return nil }
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
	writer, e := ownership.OpenExisting(t.Context(), c.Path, "rw")
	must(t, e)
	defer writer.Close()
	_, e = writer.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; INSERT INTO schema_meta VALUES('test:wal-only','present')")
	must(t, e)
	for name, value := range map[string]string{"takeover-inbox/ack.one": "queued", "receiver-ledger.json": "receiver", "transport-ledger.json": "transport"} {
		path := filepath.Join(filepath.Dir(c.Path), name)
		must(t, os.MkdirAll(filepath.Dir(path), 0700))
		must(t, os.WriteFile(path, []byte(value), 0600))
	}
	must(t, c.Begin(t.Context(), "go"))
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
