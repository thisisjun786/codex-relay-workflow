package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// pythonFence runs a snippet with the retained Python fence (the worktree's .venv), in this
// test process's isolated environment and without writing bytecode into the tree.
func pythonFence(t *testing.T, stdin, script string, args ...string) *exec.Cmd {
	t.Helper()
	command := exec.Command(filepath.Join(repositoryRoot(t), ".venv", "bin", "python"), append([]string{"-c", script}, args...)...)
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if stdin != "" {
		command.Stdin = strings.NewReader(stdin)
	}
	return command
}

// pythonOutput is what the fence prints for script; its answer is recorded (pythonOracle).
func pythonOutput(t *testing.T, script string, args ...string) string {
	t.Helper()
	parts := append([]string{"fence", script}, oracleEnvironmentNow()...)
	raw := pythonOracle(t, append(parts, args...), func() ([]byte, error) {
		raw, err := pythonFence(t, "", script, args...).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("python: %v\n%s", err, raw)
		}
		return raw, nil
	})
	return strings.TrimSpace(string(raw))
}

// pythonRefusal is how the fence answers a writable Store open: "admitted", or the
// OwnershipRefused reason and detail cli.main prints as {"error": "refused", ...}.
const pythonRefusal = `import json, sys
from codex_session_relay.store import Store
from codex_session_relay.ownership import OwnershipRefused
try:
    Store(sys.argv[1], sys.argv[2] or None).close()
except OwnershipRefused as error:
    print(json.dumps({"reason": error.reason.value, "detail": error.detail}))
else:
    print("admitted")`

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
	must(t, ownership.Validate(path, after, stamp))
	// Only the socket fields and updatedAt changed.
	unchanged := after
	unchanged.AppServerSocket, unchanged.ScopeKey, unchanged.UpdatedAt = nil, nil, before.UpdatedAt
	if !reflect.DeepEqual(unchanged, before) {
		t.Fatalf("binding changed more than the socket:\nbefore %+v\nafter  %+v", before, after)
	}
	// The key is the one the Python fence records for a socket: ownership.scope_key, asked of a
	// fixed socket path under no scope-registry override and under a fixed one, since the key
	// is a digest of the path and of the override's root, and this run's temporary directory is
	// in app's and in the isolation's override.
	const fixedSocket = "/crw-test/app.sock"
	for _, scopes := range []struct{ name, dir string }{{"no-override", ""}, {"override", "/crw-test/scopes"}} {
		t.Run("scope-key-"+scopes.name, func(t *testing.T) {
			t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", scopes.dir)
			want, err := ownership.ScopeKey(fixedSocket)
			if python := pythonOutput(t, "import sys\nfrom codex_session_relay import ownership\nprint(ownership.scope_key(sys.argv[1]))", fixedSocket); err != nil || python != want {
				t.Fatalf("scope key of %s %q (%v), the fence records %q", fixedSocket, want, err, python)
			}
		})
	}

	// Socketless and same-socket opens keep working; another socket is refused, unchanged.
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
	// The fence's answer for its own bound store opened for another socket.
	pyDir := stateDir(t)
	pyPath := filepath.Join(pyDir, "relay.sqlite3")
	pyApp, pyOther := filepath.Join(pyDir, "app.sock"), filepath.Join(pyDir, "other.sock")
	for _, socket := range []string{"", pyApp} {
		if answer := pythonOutput(t, pythonRefusal, pyPath, socket); answer != "admitted" {
			t.Fatalf("python open for %q: %s", socket, answer)
		}
	}
	if want := pythonOutput(t, pythonRefusal, pyPath, pyOther); got != want {
		t.Fatalf("go refuses %s\nthe fence refuses %s", got, want)
	}
}

// A crash between the binding's commit and its publication leaves socket_path set under a
// null mirror. Every other opener refuses that record; the next writable opener passing the
// same socket completes the binding, and the start preflight lets that opener through.
func Test30TornSocketBindingIsCompletedOnlyByItsSocket(t *testing.T) {
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
	for name, open := range map[string]func() error{
		"socketless": func() error { return openClose(t.Context(), path, "") },
		"another":    func() error { return openClose(t.Context(), path, other) },
		"read-only":  func() error { return openFencedClose(WithReadOnlyCommand(t.Context()), path, app) },
		"preflight":  func() error { return ownership.CheckStart(t.Context(), path, "") },
	} {
		if err := open(); err == nil {
			t.Fatalf("%s: the torn binding was admitted", name)
		}
	}
	must(t, ownership.CheckStart(t.Context(), path, app))
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
	for _, tc := range []struct{ owner, phase string }{{"python", "active"}, {"go", "draining"}} {
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
