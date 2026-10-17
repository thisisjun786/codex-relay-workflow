// Package worker owns a private delivery journal, not a replica of the controller store.
package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	_ "modernc.org/sqlite"
)

// Worker journals intent before invoking the adapter. An interrupted intent is
// deliberately not retried, even when the provider's list is temporarily empty.
type Worker struct {
	db   *sql.DB
	peer executor.Peer
	port executor.Port
}

func Open(root string, peer executor.Peer, adapter executor.Adapter) (*Worker, error) {
	if peer.HostID == "" || peer.Incarnation == "" || adapter == nil {
		return nil, errors.New("worker requires enrolled identity and adapter")
	}
	if err := privateDirectory(root); err != nil {
		return nil, err
	}
	file := filepath.Join(root, "delivery.sqlite3")
	if info, err := os.Lstat(file); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("worker journal is foreign or not private")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", file)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{"PRAGMA busy_timeout=5000", "PRAGMA synchronous=FULL", `CREATE TABLE IF NOT EXISTS starts(execution_id TEXT PRIMARY KEY, reference_json TEXT NOT NULL, admission_json TEXT NOT NULL, grant_json TEXT NOT NULL, state TEXT NOT NULL, handle_json TEXT NOT NULL DEFAULT '{}', proof TEXT NOT NULL DEFAULT '')`, `CREATE TABLE IF NOT EXISTS commands(permit_id TEXT PRIMARY KEY,execution_id TEXT NOT NULL,group_json TEXT NOT NULL,owner_pid INTEGER NOT NULL,state TEXT NOT NULL,result_json TEXT NOT NULL DEFAULT '{}')`} {
		if _, err = db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return &Worker{db: db, peer: peer, port: executor.Port{Adapter: adapter}}, nil
}
func privateDirectory(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
		return errors.New("worker root must be an absolute private directory")
	}
	for p := root; p != "/"; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("worker root contains a symlink")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if resolved != root {
		return errors.New("worker root contains a symlink")
	}
	st, err := os.Stat(root)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0077 != 0 {
		return errors.New("worker root must be private (0700)")
	}
	return nil
}
func (w *Worker) Close() error                        { return w.db.Close() }
func (w *Worker) Kind() executor.Kind                 { return w.port.Adapter.Kind() }
func (w *Worker) Capabilities() executor.Capabilities { return w.port.Adapter.Capabilities() }
func jsonText(v any) string                           { b, _ := json.Marshal(v); return string(b) }
func (w *Worker) check(r executor.Reference) error {
	if r.Executor.HostID != w.peer.HostID || r.Executor.Incarnation != w.peer.Incarnation {
		return errors.New("worker identity/incarnation differs from assignment")
	}
	return nil
}
func (w *Worker) transaction(ctx context.Context, fn func(*sql.Conn) error) (err error) {
	c, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err = c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, _ = c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	if err = fn(c); err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "COMMIT")
	return err
}
func (w *Worker) Start(ctx context.Context, a executor.Admission, g executor.Grant) (executor.Handle, error) {
	r := executor.ReferenceOf(a)
	if err := w.check(r); err != nil {
		return executor.Handle{}, err
	}
	if err := executor.ValidateAdmission(a); err != nil {
		return executor.Handle{}, err
	}
	if err := executor.ValidateGrant(a, g, time.Now()); err != nil {
		return executor.Handle{}, err
	}
	var replay *executor.Handle
	err := w.transaction(ctx, func(c *sql.Conn) error {
		var ref, state, handle string
		err := c.QueryRowContext(ctx, "SELECT reference_json,state,handle_json FROM starts WHERE execution_id=?", r.Identity.ExecutionID).Scan(&ref, &state, &handle)
		if err == nil {
			if ref != jsonText(r) {
				return errors.New("execution identity was already bound to another task")
			}
			if state != "running" {
				return fmt.Errorf("start held for reconciliation: %s", state)
			}
			var h executor.Handle
			if err = json.Unmarshal([]byte(handle), &h); err != nil {
				return err
			}
			replay = &h
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = c.ExecContext(ctx, "INSERT INTO starts(execution_id,reference_json,admission_json,grant_json,state) VALUES (?,?,?,?,'starting')", r.Identity.ExecutionID, jsonText(r), jsonText(a), jsonText(g))
		return err
	})
	if err != nil {
		return executor.Handle{}, err
	}
	if replay != nil {
		return *replay, nil
	}
	h, startErr := w.port.Start(ctx, a, g)
	state := "running"
	if startErr != nil {
		state = "start_unknown"
	}
	_, saveErr := w.db.ExecContext(context.WithoutCancel(ctx), "UPDATE starts SET state=?,handle_json=? WHERE execution_id=? AND state='starting'", state, jsonText(h), r.Identity.ExecutionID)
	return h, errors.Join(startErr, saveErr)
}
func (w *Worker) read(ctx context.Context, r executor.Reference) (a executor.Admission, g executor.Grant, state string, err error) {
	if r.Executor.HostID != w.peer.HostID {
		err = errors.New("execution belongs to another host")
		return
	}
	var ref, raw, grant string
	err = w.db.QueryRowContext(ctx, "SELECT reference_json,admission_json,grant_json,state FROM starts WHERE execution_id=?", r.Identity.ExecutionID).Scan(&ref, &raw, &grant, &state)
	if err != nil {
		return
	}
	if ref != jsonText(r) {
		err = errors.New("execution reference changed")
		return
	}
	if err = json.Unmarshal([]byte(raw), &a); err == nil {
		err = json.Unmarshal([]byte(grant), &g)
	}
	return
}
func unknown(reason string) executor.Inspection {
	return executor.Inspection{State: "unknown", NativeSession: executor.Observation{UnknownReason: reason}}
}
func (w *Worker) Inspect(ctx context.Context, r executor.Reference) (executor.Inspection, error) {
	a, _, state, err := w.read(ctx, r)
	if errors.Is(err, sql.ErrNoRows) {
		return unknown("no durable start record; absence is not proof"), nil
	}
	if err != nil {
		return executor.Inspection{}, err
	}
	if state == "terminated" || state == "not_started" {
		var proof string
		if err = w.db.QueryRowContext(ctx, "SELECT proof FROM starts WHERE execution_id=?", r.Identity.ExecutionID).Scan(&proof); err != nil {
			return executor.Inspection{}, err
		}
		return executor.Inspection{State: state, Proof: proof, NativeSession: executor.Observation{UnknownReason: "terminal journal"}}, nil
	}
	i, err := w.port.Inspect(ctx, a)
	if err != nil {
		return executor.Inspection{}, err
	}
	if state == "starting" && i.State == "not_started" {
		return unknown("start intent may still be in flight; negative listing cannot settle it"), nil
	}
	if i.State == "running" && state != "cancel_unknown" {
		h := executor.Handle{Reference: r, NativeSession: i.NativeSession}
		_, err = w.db.ExecContext(ctx, "UPDATE starts SET state='running',handle_json=? WHERE execution_id=? AND state IN ('starting','start_unknown')", jsonText(h), r.Identity.ExecutionID)
		if err != nil {
			return executor.Inspection{}, err
		}
	}
	if i.State == "terminated" || i.State == "not_started" {
		var whole bool
		whole, err = w.sealTerminal(ctx, r.Identity.ExecutionID, i.State, i.Proof)
		if err == nil && !whole {
			return unknown("provider ended; managed command groups still unresolved"), nil
		}
	}
	return i, err
}
func (w *Worker) Cancel(ctx context.Context, r executor.Reference, g executor.Grant) (executor.Cancellation, error) {
	if err := w.RenewGrant(ctx, r, g); err != nil {
		return executor.Cancellation{}, err
	}
	a, last, state, err := w.read(ctx, r)
	if err != nil {
		return executor.Cancellation{}, err
	}
	if g.Owner != last.Owner || g.OwnerEpoch != last.OwnerEpoch {
		return executor.Cancellation{}, errors.New("cancel grant owner is stale")
	}
	if err = executor.ValidateGrant(a, g, time.Now()); err != nil {
		return executor.Cancellation{}, err
	}
	if state == "starting" {
		return executor.Cancellation{}, errors.New("start still in flight; cancellation is unknown")
	}
	if state == "terminated" || state == "not_started" {
		i, err := w.Inspect(ctx, r)
		return executor.Cancellation{Terminated: err == nil, Proof: i.Proof}, err
	}
	if _, err = w.db.ExecContext(ctx, "UPDATE starts SET state='cancel_unknown' WHERE execution_id=?", r.Identity.ExecutionID); err != nil {
		return executor.Cancellation{}, err
	}
	c, err := w.port.Cancel(ctx, a, g)
	if err != nil {
		return executor.Cancellation{}, err
	}
	if c.Terminated {
		c.Terminated, err = w.sealTerminal(ctx, r.Identity.ExecutionID, "terminated", c.Proof)
		if !c.Terminated {
			c.Proof = ""
		}
	}
	return c, err
}
func (w *Worker) Collect(ctx context.Context, r executor.Reference) (executor.Delivery, error) {
	a, _, _, err := w.read(ctx, r)
	if err != nil {
		return executor.Delivery{}, err
	}
	return w.port.Collect(ctx, a)
}

// Sequence is durable across transport processes; receipt time belongs to the
// controller. Restarting a transport never resets the observation sequence.
func (w *Worker) Sequence(ctx context.Context) (int64, error) {
	var n int64
	err := w.transaction(ctx, func(c *sql.Conn) error {
		if _, err := c.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS observations(sequence INTEGER PRIMARY KEY AUTOINCREMENT)"); err != nil {
			return err
		}
		res, err := c.ExecContext(ctx, "INSERT INTO observations DEFAULT VALUES")
		if err != nil {
			return err
		}
		n, err = res.LastInsertId()
		return err
	})
	return n, err
}
func (w *Worker) Drained(ctx context.Context) error {
	var count int
	if err := w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM starts WHERE state NOT IN ('terminated','not_started')").Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("worker still holds unresolved executions")
	}
	return nil
}
func (w *Worker) AuthorizeGroup(ctx context.Context, g Group) error {
	if err := w.check(g.Reference); err != nil {
		return err
	}
	a, grant, state, err := w.read(ctx, g.Reference)
	if err != nil {
		return err
	}
	if state != "running" {
		return errors.New("execution is not running")
	}
	if err = executor.ValidateGrant(a, grant, time.Now()); err != nil {
		return err
	}
	for _, check := range a.Task.Checks {
		if g.Permit.ID == executor.Digest([]byte(a.Task.Identity.ExecutionID+"\x00"+check.ID)) && g.Permit.CPU == check.Resources.CPU && jsonText(g.Args) == jsonText(check.Args) {
			return nil
		}
	}
	return errors.New("command does not match an admitted check and permit")
}

func (w *Worker) Repository(ctx context.Context, r executor.Reference) (string, error) {
	a, _, _, err := w.read(ctx, r)
	return a.Task.Repository.ID, err
}

// Workspace is shared with the approved provider adapter. Execution identities
// never become path components; each admitted execution has its own checkout.
func Workspace(root string, r executor.Reference) string {
	return filepath.Join(root, "executions", executor.Digest([]byte(r.Identity.ExecutionID)))
}

// RenewGrant consumes authenticated controller authority monotonically. It is
// applied before cancellation effects, including command-group cancellation.
func (w *Worker) RenewGrant(ctx context.Context, r executor.Reference, g executor.Grant) error {
	return w.transaction(ctx, func(c *sql.Conn) error {
		if r.Executor.HostID != w.peer.HostID {
			return errors.New("foreign worker control")
		}
		var reference, admission, grant string
		if err := c.QueryRowContext(ctx, "SELECT reference_json,admission_json,grant_json FROM starts WHERE execution_id=?", r.Identity.ExecutionID).Scan(&reference, &admission, &grant); err != nil {
			return err
		}
		if reference != jsonText(r) {
			return errors.New("foreign execution control")
		}
		var a executor.Admission
		var last executor.Grant
		if err := json.Unmarshal([]byte(admission), &a); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(grant), &last); err != nil {
			return err
		}
		if err := executor.ValidateGrant(a, g, time.Now()); err != nil {
			return err
		}
		if g.OwnerEpoch < last.OwnerEpoch || (g.OwnerEpoch == last.OwnerEpoch && g.Owner != last.Owner) || g.ExpiresAt.Before(last.ExpiresAt) {
			return errors.New("stale control grant")
		}
		_, err := c.ExecContext(ctx, "UPDATE starts SET grant_json=? WHERE execution_id=?", jsonText(g), r.Identity.ExecutionID)
		return err
	})
}
func (w *Worker) CheckProfiles(ctx context.Context, r executor.Reference, launch, toolchain string) error {
	a, _, _, err := w.read(ctx, r)
	if err != nil {
		return err
	}
	if a.Task.Executor.LaunchProfileDigest != launch || a.Task.Executor.ToolchainProfileDigest != toolchain {
		return errors.New("execution launch/toolchain profile changed; restore its approved profile before control")
	}
	return nil
}
