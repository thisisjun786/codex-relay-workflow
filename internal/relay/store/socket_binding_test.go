package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// goRefusal is a refusal as the fence's cli.main prints it: {"reason": ..., "detail": ...}.
func goRefusal(t *testing.T, err error) string {
	t.Helper()
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("not a refusal: %T %v", err, err)
	}
	reason, e := json.Marshal(refused.Reason)
	must(t, e)
	detail, e := json.Marshal(refused.Detail)
	must(t, e)
	// json.dumps' default separators and dict order.
	return `{"reason": ` + string(reason) + `, "detail": ` + string(detail) + `}`
}

func readBoth(t *testing.T, path string) (ownership.Record, ownership.Stamp) {
	t.Helper()
	r, err := ownership.ReadRecord(path)
	must(t, err)
	s, err := ownership.SnapshotMeta(t.Context(), path)
	must(t, err)
	return r, s
}

// Decision 30 / cutover.md Record, Socket binding: a store a socketless writable opener
// created (crw-run's `--state S register`) is bound to the first writable opener that passes
// an App Server socket: schema_meta.socket_path is committed, then the mirror publishes
// appServerSocket and the scope key, and nothing else of the record changes. Read-only opens
// never bind; a bound store opened for another socket is refused as the fence refuses it.
func Test30SocketBindingBindsAnUnboundStoreOnce(t *testing.T) {
	// Serial: sets process environment variables, which every other running test would see.
	dir := stateDir(t)
	path := filepath.Join(dir, "relay.sqlite3")
	app, other := filepath.Join(dir, "app.sock"), filepath.Join(dir, "other.sock")
	db, err := Open(t.Context(), path, "")
	must(t, err)
	must(t, db.Close())
	before, _ := readBoth(t, path)
	if before.AppServerSocket != nil || before.ScopeKey != nil {
		t.Fatalf("socketless creation bound the store: %+v", before)
	}

	// A read-only command never binds, even when it names a socket: it reads mode=ro.
	read, err := Open(WithReadOnlyCommand(t.Context()), path, app)
	must(t, err)
	if !read.ReadOnly() {
		t.Fatal("a read-only command was admitted as a writer for a socket its store does not name")
	}
	must(t, read.Close())
	if r, s := readBoth(t, path); r.AppServerSocket != nil || s.SocketPath != "" {
		t.Fatalf("a read-only command bound the store: %+v %+v", r, s)
	}

	db, err = Open(t.Context(), path, app)
	must(t, err)
	must(t, db.Close())
	after, stamp := readBoth(t, path)
	key, err := ownership.ScopeKey(app)
	must(t, err)
	if stamp.SocketPath != app || after.AppServerSocket == nil || *after.AppServerSocket != app || after.ScopeKey == nil || *after.ScopeKey != key {
		t.Fatalf("binding: stamp socket %q, record %+v", stamp.SocketPath, after)
	}
	requireMirrorAgrees(t, path, after, stamp)
	// Only the socket fields and updatedAt changed.
	unchanged := after
	unchanged.AppServerSocket, unchanged.ScopeKey, unchanged.UpdatedAt = nil, nil, before.UpdatedAt
	if !reflect.DeepEqual(unchanged, before) {
		t.Fatalf("binding changed more than the socket:\nbefore %+v\nafter  %+v", before, after)
	}
	// The scope key of a socket is ownership.scope_key's, as the fence recorded it: asked of a
	// fixed socket path under no scope-registry override and under a fixed one, since the key is
	// a digest of the path and of the override's root, and this run's temporary directory is in
	// app's and in the isolation's override.
	const fixedSocket = "/crw-test/app.sock"
	for _, scopes := range []struct{ name, dir string }{{"no-override", ""}, {"override", "/crw-test/scopes"}} {
		t.Run("scope-key-"+scopes.name, func(t *testing.T) {
			t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", scopes.dir)
			key, err := ownership.ScopeKey(fixedSocket)
			if err != nil {
				t.Fatal(err)
			}
			checkText(t, "scope key", key)
		})
	}

	// Socketless and same-socket opens keep working; another socket is refused, unchanged, in the
	// fence's words.
	for _, socket := range []string{"", app} {
		db, err = Open(t.Context(), path, socket)
		must(t, err)
		must(t, db.Close())
	}
	unchangedDir := snapshotDir(t, dir)
	_, err = Open(t.Context(), path, other)
	got := goRefusal(t, err)
	if !reflect.DeepEqual(snapshotDir(t, dir), unchangedDir) {
		t.Fatal("the refused socket changed the state directory")
	}
	checkText(t, "refusal", got)
}

// A crash between the binding's commit and its publication leaves socket_path set under a
// null mirror. An opener naming another socket is refused; a socketless or read-only opener is
// admitted on the stamp and leaves the mirror as it is (decision 56); the next writable opener
// passing the same socket completes the binding.
func Test30TornSocketBindingIsCompletedOnlyByItsSocket(t *testing.T) {
	// Serial: assigns the package-level bindFault seam, which every other running test would reach.
	dir := stateDir(t)
	path := filepath.Join(dir, "relay.sqlite3")
	app, other := filepath.Join(dir, "app.sock"), filepath.Join(dir, "other.sock")
	db, err := Open(t.Context(), path, "")
	must(t, err)
	must(t, db.Close())
	mirror, err := os.ReadFile(filepath.Join(dir, "takeover.json"))
	must(t, err)
	bindFault = func(point string) error {
		if point == "committed" {
			return errors.New("crash after the socket_path commit")
		}
		return nil
	}
	_, err = Open(t.Context(), path, app)
	bindFault = func(string) error { return nil }
	if err == nil || !strings.Contains(err.Error(), "crash after the socket_path commit") {
		t.Fatalf("binding did not stop at the seam: %v", err)
	}
	if _, s := readBoth(t, path); s.SocketPath != app {
		t.Fatalf("the DB half is not ahead of the mirror: %+v", s)
	}
	if now, _ := os.ReadFile(filepath.Join(dir, "takeover.json")); string(now) != string(mirror) {
		t.Fatal("the mirror changed before the torn binding was completed")
	}
	if err := openClose(t.Context(), path, other); err == nil {
		t.Fatal("another socket's opener was admitted to the torn binding")
	}
	must(t, openClose(t.Context(), path, ""))
	must(t, openFencedClose(WithReadOnlyCommand(t.Context()), path, app))
	if now, _ := os.ReadFile(filepath.Join(dir, "takeover.json")); string(now) != string(mirror) {
		t.Fatal("an opener that does not bind completed the torn binding")
	}
	must(t, openClose(t.Context(), path, app))
	r, s := readBoth(t, path)
	key, err := ownership.ScopeKey(app)
	must(t, err)
	if r.AppServerSocket == nil || *r.AppServerSocket != app || r.ScopeKey == nil || *r.ScopeKey != key || s.SocketPath != app {
		t.Fatalf("torn binding not completed: %+v", r)
	}
	must(t, openClose(t.Context(), path, ""))
}

// Go binds only a store it owns in phase active with no transition, and never the other
// runtime's or one mid-transition: those are refused unchanged, as Python refuses them.
func Test30SocketBindingNeverTouchesAForeignOrMovingStore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ owner, phase string }{{"python", "active"}} {
		t.Run(tc.owner+"-"+tc.phase, func(t *testing.T) {
			dir := stateDir(t)
			path := filepath.Join(dir, "relay.sqlite3")
			must(t, openClose(t.Context(), path, ""))
			raw, err := ownership.OpenExisting(t.Context(), path, "rw")
			must(t, err)
			_, err = raw.Exec("UPDATE schema_meta SET value=? WHERE key='owner'", tc.owner)
			must(t, errors.Join(err, raw.Close()))
			r, err := ownership.ReadRecord(path)
			must(t, err)
			r.Owner, r.Phase = tc.owner, tc.phase
			if tc.phase == "draining" {
				r.Transition = &ownership.Transition{ID: "drain", From: "go", To: "python", TargetEpoch: 2}
			}
			must(t, ownership.Publish(path, r, nil))
			before := snapshotDir(t, dir)
			err = openClose(t.Context(), path, filepath.Join(dir, "app.sock"))
			if RefusalReason(err) != "store_owned_by_other" {
				t.Fatalf("not refused: %v", err)
			}
			if !reflect.DeepEqual(snapshotDir(t, dir), before) {
				t.Fatal("a refused binding changed the state directory")
			}
		})
	}
}

// The binding's write-gate EX waits for other admitted connections (which hold SH for their
// lifetime) at most ownership.LockWait; expiry is Python's retryable LockWaitExpired host
// error and changes nothing.
func Test30SocketBindingWaitsForWritersWithinTheBound(t *testing.T) {
	// Serial: assigns ownership.LockWait, which every other running test would read, and bounds a wait by the wall clock.
	dir := stateDir(t)
	path := filepath.Join(dir, "relay.sqlite3")
	app := filepath.Join(dir, "app.sock")
	holder, err := Open(t.Context(), path, "")
	must(t, err)
	previous := ownership.LockWait
	ownership.LockWait = 300 * time.Millisecond
	defer func() { ownership.LockWait = previous }()
	before := snapshotDir(t, dir)
	_, err = Open(t.Context(), path, app)
	var expired *ownership.LockWaitExpired
	if !errors.As(err, &expired) || err.Error() != "LockWaitExpired: write-gate EX for the socket binding was not acquired within 0.3s; retry" {
		t.Fatalf("bounded wait: %T %v", err, err)
	}
	if RefusalReason(err) != "" {
		t.Fatal("an expired wait must be a host error, not a refusal")
	}
	if !reflect.DeepEqual(snapshotDir(t, dir), before) {
		t.Fatal("an expired binding wait changed the state directory")
	}
	must(t, holder.Close())
	must(t, openClose(t.Context(), path, app))
	if r, _ := readBoth(t, path); r.AppServerSocket == nil || *r.AppServerSocket != app {
		t.Fatalf("not bound after the holder closed: %+v", r)
	}
}

func openClose(ctx context.Context, path, socket string) error {
	db, err := Open(ctx, path, socket)
	if err != nil {
		return err
	}
	return db.Close()
}

// openFencedClose is the writable opener itself under ctx, as a read-only form's admitted
// open reaches it (Python Store(..., bind_socket=False)).
func openFencedClose(ctx context.Context, path, socket string) error {
	db, err := OpenWith(ctx, path, socket, OpenOptions{BusyTimeout: 30 * time.Second})
	if err != nil {
		return err
	}
	return db.Close()
}
