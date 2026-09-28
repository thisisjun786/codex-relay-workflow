package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

func Test30AllWritableEntrypointsFenced(t *testing.T) {
	for _, entry := range []string{"open", "options", "hold", "registration", "probe"} {
		t.Run(entry, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "relay.sqlite3")
			db, err := fixtureOpen(t.Context(), path, "")
			must(t, err)
			must(t, db.Close())
			raw, err := ownership.OpenExisting(t.Context(), path, "rw")
			must(t, err)
			_, err = raw.Exec("UPDATE schema_meta SET value='python' WHERE key='owner'")
			must(t, err)
			must(t, raw.Close())
			record, err := ownership.ReadRecord(path)
			must(t, err)
			record.Owner = "python"
			must(t, ownership.Publish(path, record, nil))
			before, err := os.ReadFile(path)
			must(t, err)
			switch entry {
			case "open":
				opened, e := Open(t.Context(), path, "")
				if e == nil {
					_ = opened.Close()
					t.Fatal("foreign writer admitted")
				}
			case "options":
				opened, e := OpenWith(t.Context(), path, "", OpenOptions{})
				if e == nil {
					_ = opened.Close()
					t.Fatal("OpenWith bypassed fence")
				}
			case "hold":
				h, why := HoldForWrite(t.Context(), path, time.Second)
				if h != nil || why == "" {
					if h != nil {
						_ = h.Release()
					}
					t.Fatal("hold bypassed fence")
				}
			case "registration":
				must(t, RegistrationHold(t.Context(), path, func(conn *sql.Conn, why string) error {
					if conn != nil || why == "" {
						t.Fatal("registration bypassed fence")
					}
					return nil
				}))
			case "probe":
				p := Probe(t.Context(), StateSelection{Path: filepath.Dir(path)})
				if p.Access.DBWritable {
					t.Fatal("probe bypassed fence")
				}
			}
			after, err := os.ReadFile(path)
			must(t, err)
			if string(before) != string(after) {
				t.Fatal("refusal changed database")
			}
		})
	}
}
func Test30GateHeldUntilConnectionClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	db, err := fixtureOpen(t.Context(), path, "")
	must(t, err)
	gate := filepath.Join(filepath.Dir(path), "write-gate.lock")
	if lock, e := ownership.Lock(gate, true, false); e == nil {
		_ = lock.Close()
		t.Fatal("write gate not held for connection lifetime")
	}
	must(t, db.Close())
	lock, err := ownership.Lock(gate, true, false)
	must(t, err)
	must(t, lock.Close())
}
func Test30TransactionEntryAndDraining(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	db, err := fixtureOpen(t.Context(), path, "")
	must(t, err)
	defer db.Close()
	r, err := ownership.ReadRecord(path)
	must(t, err)
	r.Phase = "draining"
	r.Transition = &ownership.Transition{ID: "drain-test", From: "go", To: "python", TargetEpoch: 2}
	must(t, ownership.Publish(path, r, nil))
	if other, e := Open(t.Context(), path, ""); e == nil {
		_ = other.Close()
		t.Fatal("new writer admitted while draining")
	}
	must(t, db.Transaction(t.Context(), func(ctx context.Context, conn *sql.Conn) error {
		_, e := conn.ExecContext(ctx, "INSERT INTO schema_meta VALUES('test:admitted','finished')")
		return e
	}))
	r.Epoch++
	must(t, ownership.Publish(path, r, nil))
	called := false
	err = db.Transaction(t.Context(), func(context.Context, *sql.Conn) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("transaction did not revalidate admitted epoch")
	}
}
func Test30MissingDatabaseNeverCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing/relay.sqlite3")
	if db, err := Open(t.Context(), path, ""); err == nil {
		_ = db.Close()
		t.Fatal("missing store admitted")
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusal created state", err)
	}
}
func Test30BeforePragmaRechecksPhysicalStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	seed, err := fixtureOpen(t.Context(), path, "")
	must(t, err)
	must(t, seed.Close())
	changed := false
	db, err := OpenWith(t.Context(), path, "", OpenOptions{OnConnect: func() {
		if changed {
			return
		}
		changed = true
		must(t, os.Rename(path, path+".held"))
		must(t, os.WriteFile(path, nil, 0600))
	}})
	if err == nil {
		_ = db.Close()
		t.Fatal("initializer wrote to swapped store")
	}
	info, e := os.Stat(path)
	must(t, e)
	if info.Size() != 0 {
		t.Fatal("swapped database was initialized")
	}
}

func Test30SchemaRefusalNoRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	db, err := fixtureOpen(t.Context(), path, "")
	must(t, err)
	_, err = db.DB.Exec("DROP TABLE attempt_messages")
	must(t, err)
	must(t, db.Close())
	before, err := os.ReadFile(path)
	must(t, err)
	if opened, e := Open(t.Context(), path, ""); e == nil {
		_ = opened.Close()
		t.Fatal("missing required table was repaired")
	}
	after, err := os.ReadFile(path)
	must(t, err)
	if string(before) != string(after) {
		t.Fatal("schema refusal changed source")
	}
}
