package hook

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Test30StopOwnerReadCreatesNothing drives the built crw hook against a store the Go runtime
// owns (cutover.md Lock order, "The read-only Stop path"). The native hook decides whether it
// may evaluate in-process from the mirror and the durable owner, and that read creates no
// SQLite sidecar beside the store (store.OpenStopRead, Python's ownership.stop_metadata):
// without a WAL connection D is read immutable, and an owner change committed only to a live
// WAL is still seen, so the Stop goes to the control socket instead.
func Test30StopOwnerReadCreatesNothing(t *testing.T) {
	for _, name := range []string{"no-wal", "live-wal-owner"} {
		t.Run(name, func(t *testing.T) {
			home := hookHome(t, 5)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			path := filepath.Join(home, "state", "relay.sqlite3")
			created, err := fixtureStore(ctx, path, "")
			if err != nil {
				t.Fatal(err)
			}
			if err = created.Close(); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"-wal", "-shm"} {
				if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("closed fixture left %s: %v", suffix, err)
				}
			}
			if name == "live-wal-owner" {
				// The durable owner becomes python in the WAL only; D and the mirror say go.
				writer, err := sql.Open("sqlite", "file:"+path)
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Close()
				writer.SetMaxOpenConns(1)
				for _, statement := range []string{"PRAGMA wal_autocheckpoint=0", "UPDATE schema_meta SET value='python' WHERE key='owner'"} {
					if _, err = writer.ExecContext(ctx, statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			listing := func() []string {
				entries, err := os.ReadDir(filepath.Dir(path))
				if err != nil {
					t.Fatal(err)
				}
				names := []string{}
				for _, entry := range entries {
					if entry.Name() != "control.sock" {
						names = append(names, entry.Name())
					}
				}
				return names
			}
			before := listing()
			routed := make(chan bool, 1)
			done, _ := fakeControl(t, home, func(conn net.Conn) error {
				raw, err := bufio.NewReader(conn).ReadBytes('\n')
				if errors.Is(err, io.EOF) && len(raw) == 0 {
					routed <- false // evaluated in-process: the dialed connection carried nothing
					return nil
				}
				if err != nil {
					return err
				}
				routed <- true
				_, err = io.WriteString(conn, `{"decision":"release","state":"answered-by-owner","hook_output":{}}`+"\n")
				return err
			})
			workspace := filepath.Join(home, "unmanaged")
			if err = os.MkdirAll(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			stdout, err := hookCommand(t, home, `{"cwd":"`+workspace+`","session_id":"s","turn_id":"t","stop_hook_active":false}`).Output()
			if err != nil || len(stdout) != 0 {
				t.Fatalf("hook: %v %s", err, stdout)
			}
			awaitHost(t, done)
			if after := listing(); !slices.Equal(after, before) {
				t.Fatalf("the Stop's owner read changed the store directory: %v -> %v", before, after)
			}
			rows := rowsAt(t, home)
			if len(rows) != 1 {
				t.Fatal(rows)
			}
			want, wantRouted := "unmanaged", false
			if name == "live-wal-owner" {
				want, wantRouted = "answered-by-owner", true
			}
			if got := <-routed; got != wantRouted || rows[0]["guardState"] != want {
				t.Fatalf("routed=%v row=%v", got, rows[0])
			}
		})
	}
}
