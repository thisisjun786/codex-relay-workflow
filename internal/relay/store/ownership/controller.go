package ownership

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Runtime supplies process lifecycle, not ownership decisions. Start returns
// only after recovery and RPC readiness. The returned candidate must stop if
// its activation channel closes before a matching durable active publication.
type Runtime interface {
	Drain(context.Context, Record) error
	Start(context.Context, Record) (CandidateProcess, error)
}
type CandidateProcess interface {
	Identity() Identity
	Activate(context.Context, Record) error
	Close() error
}
type Controller struct {
	Path           string
	ScopeLock      string
	ScopeKey       string
	Socket         string
	Identity       Identity
	Runtime        Runtime
	ValidateSchema func(context.Context, Queryer) error
	// Fault is nil outside tests; errors model controller death at durable edges.
	Fault func(step, point string) error
}
type Status struct {
	Protocol                 int         `json:"protocol"`
	StoreID                  string      `json:"storeId"`
	Database                 Database    `json:"database"`
	Owner                    string      `json:"owner"`
	Epoch                    int64       `json:"epoch"`
	TakeoverID               string      `json:"takeoverId"`
	Phase                    string      `json:"phase"`
	JSONStale                bool        `json:"jsonStale"`
	RollbackAllowed          bool        `json:"rollbackAllowed"`
	PythonCompatibilityBuild string      `json:"pythonCompatibilityBuild"`
	Transition               *Transition `json:"transition"`
	Holder                   *Identity   `json:"holder"`
}

func (c *Controller) fault(step, point string) error {
	if c.Fault != nil {
		return c.Fault(step, point)
	}
	return nil
}
func (c *Controller) publish(step string, r Record) error {
	return Publish(c.Path, r, func(p string) error { return c.fault(step, p) })
}

// reconcile accepts only the two recoverable torn publications: the exact DB
// CAS named by the draining transition, or rollback_allowed's one-way commit.
// A different identity/epoch/transition is disagreement, never auto-reset.
func (c *Controller) read(ctx context.Context) (Record, Stamp, bool, error) {
	r, err := ReadRecord(c.Path)
	if err != nil {
		return r, Stamp{}, false, err
	}
	s, err := SnapshotMeta(ctx, c.Path)
	if err != nil {
		return r, s, false, err
	}
	physical, err := Physical(c.Path)
	if err != nil {
		return r, s, false, err
	}
	if physical != r.Database || r.StoreID != s.StoreID || r.Protocol != s.Protocol || r.PythonCompatibilityBuild != s.PythonCompatibilityBuild {
		return r, s, false, refuse("store identity disagrees")
	}
	if c.Socket != "" && (r.AppServerSocket == nil || *r.AppServerSocket != c.Socket) {
		return r, s, false, refuse("App Server socket disagrees")
	}
	if c.ScopeKey != "" && (r.ScopeKey == nil || *r.ScopeKey != c.ScopeKey) {
		return r, s, false, refuse("scope identity disagrees")
	}
	stale := false
	if r.Owner != s.Owner || r.Epoch != s.Epoch {
		t := r.Transition
		if r.Phase != "draining" || t == nil || r.Owner != t.From || s.Owner != t.To || s.Epoch != t.TargetEpoch || s.Epoch != r.Epoch+1 || s.TakeoverID != t.ID {
			return r, s, false, refuse("unrecoverable owner/epoch disagreement")
		}
		r.Owner = s.Owner
		r.Epoch = s.Epoch
		r.Phase = "starting"
		r.Holder = nil
		stale = true
	}
	if r.RollbackAllowed != s.RollbackAllowed {
		if !r.RollbackAllowed || s.RollbackAllowed || s.Owner != "go" || r.Phase != "active" {
			return r, s, false, refuse("unrecoverable rollback disagreement")
		}
		r.RollbackAllowed = false
		stale = true
	}
	if err = Validate(c.Path, r, s); err != nil {
		return r, s, false, err
	}
	return r, s, stale, nil
}
func (c *Controller) Status(ctx context.Context) (Status, error) {
	r, s, stale, err := c.read(ctx)
	if err != nil {
		return Status{}, err
	}
	return Status{Protocol, s.StoreID, r.Database, s.Owner, s.Epoch, s.TakeoverID, r.Phase, stale, s.RollbackAllowed, s.PythonCompatibilityBuild, r.Transition, r.Holder}, nil
}
func (c *Controller) serialized(ctx context.Context, step string, run func(Record, Stamp) error) (err error) {
	l, err := Lock(filepath.Join(filepath.Dir(c.Path), "takeover.lock"), true, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, l.Close()) }()
	if err = c.fault(step, "locked"); err != nil {
		return err
	}
	r, s, _, err := c.read(ctx)
	if err != nil {
		return err
	}
	return run(r, s)
}
func (c *Controller) Begin(ctx context.Context, to string) error {
	return c.serialized(ctx, "begin", func(r Record, s Stamp) error { return c.begin("begin", r, s, to) })
}
func (c *Controller) begin(step string, r Record, s Stamp, to string) error {
	if !owner(to) || !s.RollbackAllowed {
		return refuse("transition unavailable")
	}
	if r.Phase != "active" {
		if r.Transition != nil && r.Transition.To == to {
			return nil
		}
		return refuse("another transition is in progress")
	}
	if s.Owner == to {
		return nil
	}
	if s.Epoch == int64(^uint64(0)>>1) {
		return refuse("epoch exhausted")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	r.Phase = "draining"
	r.Transition = &Transition{hex.EncodeToString(id[:]), s.Owner, to, s.Epoch + 1}
	r.Controller = &c.Identity
	return c.publish(step, r)
}
func (c *Controller) Drain(ctx context.Context) error {
	return c.serialized(ctx, "drain", func(r Record, _ Stamp) error {
		if r.Phase == "starting" || r.Phase == "active" {
			return nil
		}
		if c.Runtime == nil {
			return refuse("drain runtime unavailable")
		}
		if err := c.Runtime.Drain(ctx, r); err != nil {
			return err
		}
		return c.fault("drain", "drained")
	})
}

// Barrier excludes inherited daemon/scope descriptions and every admitted CLI
// connection. All locks are released in reverse order, never unlinked.
func (c *Controller) barrier() (func() error, error) {
	var held []*os.File
	closeAll := func() error {
		var err error
		for i := len(held) - 1; i >= 0; i-- {
			err = errors.Join(err, held[i].Close())
		}
		return err
	}
	if c.ScopeLock == "" {
		return nil, refuse("scope lock is required")
	}
	for _, path := range []string{filepath.Join(filepath.Dir(c.Path), "daemon.lock"), c.ScopeLock, filepath.Join(filepath.Dir(c.Path), "write-gate.lock")} {
		// Existing service locks predate the 0600 fence; they may be 0644 under
		// Python's umask. Open those permanent files without changing their inode.
		var f *os.File
		var err error
		if filepath.Base(path) == "write-gate.lock" {
			f, err = Lock(path, true, false)
		} else {
			f, err = serviceLock(path)
		}
		if err != nil {
			return nil, errors.Join(err, closeAll())
		}
		held = append(held, f)
	}
	return closeAll, nil
}
func (c *Controller) Transfer(ctx context.Context) error {
	return c.serialized(ctx, "transfer", func(r Record, s Stamp) error { return c.transfer(ctx, r, s, "transfer") })
}
func (c *Controller) transfer(ctx context.Context, r Record, s Stamp, step string) (err error) {
	if r.Phase == "starting" {
		return c.publish(step, r)
	}
	if r.Phase == "active" && r.Transition != nil && r.Transition.To == s.Owner {
		return nil
	}
	if r.Phase != "draining" || !s.RollbackAllowed {
		return refuse("transfer requires draining and rollback compatibility")
	}
	release, err := c.barrier()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	if err = c.fault(step, "barrier"); err != nil {
		return err
	}
	r, s, _, err = c.read(ctx)
	if err != nil {
		return err
	}
	if r.Phase != "draining" {
		return refuse("transition changed before barrier")
	}
	db, err := OpenExisting(ctx, c.Path, "rw")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if _, err = db.ExecContext(ctx, "PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON"); err != nil {
		return err
	}
	if err = c.inspect(ctx, db, r); err != nil {
		return err
	}
	if err = c.fault(step, "snapshot"); err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, e := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
			err = errors.Join(err, e)
		}
	}()
	current, err := ReadStamp(ctx, conn)
	if err != nil {
		return err
	}
	if current != s {
		return refuse("ownership CAS lost")
	}
	t := r.Transition
	for _, pair := range [][2]string{{"owner", t.To}, {"owner_epoch", strconv.FormatInt(t.TargetEpoch, 10)}, {"takeover_id", t.ID}} {
		if _, err = conn.ExecContext(ctx, "UPDATE schema_meta SET value=? WHERE key=?", pair[1], pair[0]); err != nil {
			return err
		}
	}
	if err = c.fault(step, "before-commit"); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	if err = c.fault(step, "db-committed"); err != nil {
		return err
	}
	r.Owner = t.To
	r.Epoch = t.TargetEpoch
	r.Phase = "starting"
	r.Holder = nil
	return c.publish(step, r)
}
func (c *Controller) Activate(ctx context.Context) error {
	return c.serialized(ctx, "activate", func(r Record, _ Stamp) error { return c.activate(ctx, r) })
}
func (c *Controller) activate(ctx context.Context, r Record) (err error) {
	if r.Phase == "active" {
		return nil
	}
	if r.Phase != "starting" {
		return refuse("activation requires transferred ownership")
	}
	if c.Runtime == nil {
		return refuse("activation runtime unavailable")
	}
	// Re-establish candidate exclusion on resume. A crashed controller's old
	// candidate cannot retain its SH or daemon lock and be displaced.
	release, err := c.barrier()
	if err != nil {
		return err
	}
	r.Controller = &c.Identity
	err = c.publish("activate", r)
	err = errors.Join(err, release())
	if err != nil {
		return err
	}
	candidate, err := c.Runtime.Start(ctx, r)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, candidate.Close()) }()
	if err = c.fault("activate", "ready"); err != nil {
		return err
	}
	identity := candidate.Identity()
	if identity.PID <= 0 || identity.BootID == "" || identity.StartTicks <= 0 || identity.Build == "" {
		return refuse("incomplete readiness identity")
	}
	fresh, _, _, err := c.read(ctx)
	if err != nil {
		return err
	}
	if fresh.Owner != r.Owner || fresh.Epoch != r.Epoch || fresh.Transition.ID != r.Transition.ID {
		return refuse("ownership changed during readiness")
	}
	fresh.Phase = "active"
	fresh.Holder = &identity
	if err = c.publish("activate-active", fresh); err != nil {
		return err
	}
	if err = c.fault("activate", "active-published"); err != nil {
		return err
	}
	return candidate.Activate(ctx, fresh)
}
func (c *Controller) Rollback(ctx context.Context) error {
	return c.serialized(ctx, "rollback", func(r Record, s Stamp) error {
		if !s.RollbackAllowed {
			return refuse("rollback has been committed away")
		}
		if s.Owner == "python" && r.Phase == "active" {
			return nil
		}
		if r.Phase == "starting" && s.Owner == "go" {
			r.Phase = "active"
		}
		if err := c.begin("rollback", r, s, "python"); err != nil {
			return err
		}
		r, s, _, err := c.read(ctx)
		if err != nil {
			return err
		}
		if r.Phase == "draining" {
			if c.Runtime == nil {
				return refuse("drain runtime unavailable")
			}
			if err = c.Runtime.Drain(ctx, r); err != nil {
				return err
			}
			if err = c.fault("rollback", "drained"); err != nil {
				return err
			}
			if err = c.transfer(ctx, r, s, "rollback-transfer"); err != nil {
				return err
			}
		}
		r, _, _, err = c.read(ctx)
		if err != nil {
			return err
		}
		return c.activate(ctx, r)
	})
}
func (c *Controller) Commit(ctx context.Context) error {
	return c.serialized(ctx, "commit", func(r Record, s Stamp) (err error) {
		if s.Owner != "go" || r.Phase != "active" {
			return refuse("commit requires active Go")
		}
		if !s.RollbackAllowed {
			return c.publish("commit", r)
		}
		// Commit changes no owner/epoch; a normal SH admission allows the live
		// daemon to keep serving. The controller lock excludes reverse transfer.
		gate, err := Lock(filepath.Join(filepath.Dir(c.Path), "write-gate.lock"), false, false)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, gate.Close()) }()
		db, err := OpenExisting(ctx, c.Path, "rw")
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, db.Close()) }()
		if _, err = db.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
			return err
		}
		tx, err := db.Conn(ctx)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, tx.Close()) }()
		if _, err = tx.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		committed := false
		defer func() {
			if !committed {
				_, e := tx.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
				err = errors.Join(err, e)
			}
		}()
		current, err := ReadStamp(ctx, tx)
		if err != nil {
			return err
		}
		if current != s {
			return refuse("commit stamp changed")
		}
		result, err := tx.ExecContext(ctx, "UPDATE schema_meta SET value='0' WHERE key='rollback_allowed' AND value='1' AND (SELECT value FROM schema_meta WHERE key='owner')='go' AND (SELECT value FROM schema_meta WHERE key='owner_epoch')=?", strconv.FormatInt(s.Epoch, 10))
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return refuse("commit CAS lost")
		}
		if err = c.fault("commit", "before-commit"); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		committed = true
		if err = c.fault("commit", "db-committed"); err != nil {
			return err
		}
		r.RollbackAllowed = false
		return c.publish("commit", r)
	})
}

func (s Status) String() string {
	return fmt.Sprintf("owner=%s epoch=%d phase=%s jsonStale=%t", s.Owner, s.Epoch, s.Phase, s.JSONStale)
}
