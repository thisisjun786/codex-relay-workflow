package hook

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func Test33ControlHandler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, server := controlPair(t)
	defer client.Close()
	done := make(chan error, 1)
	ownerState := t.TempDir()
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- context.Canceled
			}
		}()
		done <- HandleControl(ctx, server, ownerState)
	}()
	result, err := RequestGuard(ctx, client, Object{}, GuardOptions{Root: t.TempDir(), Mode: Observe, Now: "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if get(result, "decision") != "release" || get(result, "state") != "unmanaged" {
		t.Fatal(result)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
func Test33OwnedGuardRunsInProcess(t *testing.T) {
	home := hookHome(t, 5)
	ctx := context.Background()
	db, err := fixtureStore(ctx, filepath.Join(home, "state/relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.DB.ExecContext(ctx, "INSERT OR REPLACE INTO schema_meta(key,value) VALUES('owner','go')"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	done, _ := fakeControl(t, home, func(conn net.Conn) error {
		raw, err := io.ReadAll(conn)
		if err != nil {
			return err
		}
		if len(raw) != 0 {
			t.Errorf("owned guard unexpectedly issued RPC: %s", raw)
		}
		return nil
	})
	cmd := hookCommand(t, home, `{}`)
	out, err := cmd.CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatalf("%v %s", err, out)
	}
	awaitHost(t, done)
	rows := rowsAt(t, home)
	if len(rows) != 1 || rows[0]["guardState"] != "unmanaged" {
		t.Fatal(rows)
	}
}
func Test33DeadlineReachesNestedWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := t.TempDir()
	if _, err := RecordObservation(ctx, root, Object{{Key: "sessionId", Value: "s"}, {Key: "turnId", Value: "t"}}, root); err != context.Canceled {
		t.Fatal(err)
	}
	_, _, _, err := store.HashArtifactContext(ctx, "/never-open", []string{"/"}, false)
	if err != context.Canceled {
		t.Fatal(err)
	}
	if text := evidence.Dumps([]any{EventKeyTag, "s", "t", false, "i"}, true, false, true); text == "" {
		t.Fatal("empty event key preimage")
	}
}
