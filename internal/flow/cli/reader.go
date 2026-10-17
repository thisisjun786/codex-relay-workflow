package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ErrNoRun is the refusal of a command that names a run the store does not hold.
var ErrNoRun = errors.New("the store holds no such run")

// Reader reads the runs of a relay store. It opens the store read-only (nothing is created, no write gate is taken) and, for the
// run lock, knows the state directory the store is in.
type Reader struct {
	St *store.Store
	// State is the relay state directory: <State>/flow/<run>/ holds a run's lock, spec and log.
	State string
}

// ResolveState is the relay state directory a command works on: the one it was given, else the relay's own selection for the
// control socket.
func ResolveState(state, socket string) (store.StateSelection, error) {
	if socket == "" {
		var err error
		if socket, err = store.DefaultSocket(); err != nil {
			return store.StateSelection{}, err
		}
	}
	return store.ResolveStateDir(state, socket)
}

// OpenReader opens the relay store of a state directory for reading.
func OpenReader(ctx context.Context, state, socket string) (*Reader, error) {
	sel, err := ResolveState(state, socket)
	if err != nil {
		return nil, err
	}
	db := sel.DBPath()
	if err := store.CheckReadable(db); err != nil {
		return nil, fmt.Errorf("there is no relay store at %s (%s): %w", db, sel.Detail, err)
	}
	st, err := store.OpenReadOnlyStore(ctx, db)
	if err != nil {
		return nil, err
	}
	return &Reader{St: st, State: sel.Path}, nil
}

// Close ends the store.
func (r *Reader) Close() error { return r.St.Close() }

// Record reads the flow_runs row of a run.
func (r *Reader) Record(ctx context.Context, run string) (RunRecord, error) {
	row := r.St.DB.QueryRowContext(ctx, `SELECT run_id, workflow, script_digest, args_digest, plan_id, plan_revision, actor_task_id, resume_from, started_at FROM flow_runs WHERE run_id = ?`, run)
	rec, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RunRecord{}, fmt.Errorf("%w: %s", ErrNoRun, run)
	}
	return rec, err
}

func scanRecord(row interface{ Scan(...any) error }) (RunRecord, error) {
	var rec RunRecord
	err := row.Scan(&rec.RunID, &rec.Workflow, &rec.ScriptDigest, &rec.ArgsDigest, &rec.PlanID, &rec.PlanRevision, &rec.Owner, &rec.ResumeFrom, &rec.StartedAt)
	return rec, err
}

// Records lists the runs of the store, oldest first.
func (r *Reader) Records(ctx context.Context) ([]RunRecord, error) {
	rows, err := r.St.DB.QueryContext(ctx, `SELECT run_id, workflow, script_digest, args_digest, plan_id, plan_revision, actor_task_id, resume_from, started_at FROM flow_runs ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("the runs cannot be read: %w", err)
	}
	defer rows.Close()
	var out []RunRecord
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// live says whether a run process holds the run, and its pid (0 when its record cannot be read). A writer's hold (audit, answer,
// cancel, recover: runHolder) is no process of the run, so it reads (false, 0), as a free lock does (CRW-1495 item 6).
func (r *Reader) live(run string) (bool, int) {
	live, pid, _ := runHolder(r.State, run)
	return live, pid
}

// anyHolder says whether any holder, a writer too, holds the run lock, and its pid: the commit barrier of Tail, where a follower
// must not stop before a writer's row is stored.
func (r *Reader) anyHolder(run string) (bool, int) {
	if !LockHeld(r.State, run) {
		return false, 0
	}
	pid, _ := LockHolder(r.State, run)
	return true, pid
}

// Info reads one run's line.
func (r *Reader) Info(ctx context.Context, rec RunRecord) (RunInfo, error) {
	live, pid := r.live(rec.RunID) // before the rows: see Tail
	events, _, err := journal.LoadLean(ctx, r.St, rec.RunID)
	if err != nil {
		return RunInfo{}, err
	}
	return Summarize(rec, events, live, pid), nil
}

// Infos reads the lines of every run of the store, oldest first.
func (r *Reader) Infos(ctx context.Context) ([]RunInfo, error) {
	recs, err := r.Records(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RunInfo, 0, len(recs))
	for _, rec := range recs {
		info, err := r.Info(ctx, rec)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// Detail reads the tree of one run.
func (r *Reader) Detail(ctx context.Context, run string) (RunDetail, error) {
	rec, err := r.Record(ctx, run)
	if err != nil {
		return RunDetail{}, err
	}
	live, pid := r.live(run) // before the rows: see Tail
	events, _, err := journal.LoadLean(ctx, r.St, run)
	if err != nil {
		return RunDetail{}, err
	}
	return Detail(rec, events, live, pid), nil
}

// Row is one journal row as `crw flow watch` and the event stream give it.
type Row struct {
	Seq    int            `json:"seq"`
	Kind   string         `json:"kind"`
	Event  string         `json:"event"`
	CallID string         `json:"call_id,omitempty"`
	Op     string         `json:"op,omitempty"`
	Label  string         `json:"label,omitempty"`
	Data   map[string]any `json:"data,omitempty"`
}

// Page is one answer of Tail: the rows after the cursor in order, the cursor of the next read (the seq of the last row, Cursor
// as it was when there is none) and whether a row beyond the page exists.
type Page struct {
	Run    string `json:"run"`
	After  int    `json:"after"`
	Cursor int    `json:"cursor"`
	More   bool   `json:"more"`
	Rows   []Row  `json:"rows"`
	// State is the run's state when the page was read, from run-process liveness, and Live the follower's commit barrier - any
	// holder, a writer included - so a follower knows when to stop: a paused run no process holds gets no row until a resume
	// (CRW-1487).
	State string `json:"state"`
	Live  bool   `json:"live,omitempty"`
}

// TailPoll is the pause between two reads of a waiting Tail: one second, as the relay's journal-tail.
var TailPoll = time.Second

// Tail reads the rows of a run after the cursor, at most limit of them. With wait it reads again every TailPoll while there is no
// row and the wait is not over, holding no transaction while it sleeps (every read is one short transaction), as
// `crw relay journal-tail --wait` does. A wait that ends with no row answers an empty page whose cursor is after. The page that is
// answered and the state it reports are read in one snapshot (journal.ReadPage), so a cursor never passes a committed row and a
// page is reported over only when no row beyond its cursor existed in it.
func (r *Reader) Tail(ctx context.Context, run string, after, limit int, wait time.Duration) (Page, error) {
	rec, err := r.Record(ctx, run)
	if err != nil {
		return Page{}, err
	}
	deadline := time.Now().Add(wait)
	for {
		if wait > 0 {
			probe, err := journal.ReadPage(ctx, r.St, run, after, limit, false)
			if err != nil {
				return Page{}, err
			}
			if len(probe.Events) == 0 && time.Until(deadline) > 0 {
				timer := time.NewTimer(min(TailPoll, time.Until(deadline)))
				select {
				case <-ctx.Done():
					timer.Stop()
					return Page{}, ctx.Err()
				case <-timer.C:
				}
				continue
			}
		}
		// the lock is read before the snapshot: a holder commits its last row before it lets go, so a run seen without a holder has
		// every row of it in the snapshot that follows. Any holder counts here, a writer too (anyHolder): a follower that stopped on a
		// writer's hold would miss the row the writer is about to store
		barrier, _ := r.anyHolder(run)
		live, pid := r.live(run)
		read, err := journal.ReadPage(ctx, r.St, run, after, limit, true)
		if err != nil {
			return Page{}, err
		}
		p := page(rec, after, read, live, pid)
		p.Live = barrier || live
		return p, nil
	}
}

func page(rec RunRecord, after int, read journal.PageRead, live bool, pid int) Page {
	p := Page{Run: rec.RunID, After: after, Cursor: after, More: read.More, Rows: []Row{}}
	for _, e := range read.Events {
		p.Rows = append(p.Rows, Row{Seq: e.Seq, Kind: e.Kind, Event: e.Event, CallID: e.CallID, Op: e.Op, Label: e.Label, Data: e.Data})
		p.Cursor = e.Seq
	}
	p.State, p.Live = Summarize(rec, read.Lean, live, pid).State, live
	return p
}
