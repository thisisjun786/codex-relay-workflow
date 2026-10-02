package supervisor

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// CRW-299: what a project visit costs the daemon. The world below is a synthetic project: a
// number of relationships, each with a number of final child events of which the newest is the
// completion (a ready_for_review event with a DONE work report), registered under one project,
// one initiative and the issue each relationship works. The statement counter wraps the store's
// driver, so a test or a benchmark counts what the code sent to SQLite, not how long it took.

const worldProject = "PRJ-W"

// stmtCounter counts the statements a store runs once countStatements has installed it.
type stmtCounter struct{ n atomic.Int64 }

func (c *stmtCounter) count() int64 { return c.n.Load() }

// countedConn is what the relay's store asks of a driver connection (store.guardedConn), so the
// counting wrapper can stand in for it.
type countedConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
	driver.NamedValueChecker
}

type countingConn struct {
	countedConn
	n *atomic.Int64
}

func (c countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.n.Add(1)
	return c.countedConn.ExecContext(ctx, query, args)
}

func (c countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.n.Add(1)
	return c.countedConn.QueryContext(ctx, query, args)
}

type countingDriver struct {
	inner driver.Driver
	n     *atomic.Int64
}

func (d countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	counted, ok := conn.(countedConn)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("the store's driver connection lacks an interface the statement counter forwards")
	}
	return countingConn{counted, d.n}, nil
}

type countingConnector struct {
	driver countingDriver
	dsn    string
}

func (c countingConnector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.dsn) }
func (c countingConnector) Driver() driver.Driver                        { return c.driver }

// countStatements points s at a second connection pool whose connections count every statement
// they run, until the test or benchmark ends. Statements run before the call are not counted.
func countStatements(tb testing.TB, s *store.Store) *stmtCounter {
	tb.Helper()
	path, err := filepath.EvalSymlinks(s.Path)
	if err != nil {
		tb.Fatal(err)
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "rw")
	q.Set("_busy_timeout", "30000")
	u.RawQuery = q.Encode()
	counter := &stmtCounter{}
	db := sql.OpenDB(countingConnector{countingDriver{s.DB.Driver(), &counter.n}, u.String()})
	db.SetMaxOpenConns(1)
	original := s.DB
	s.DB = db
	tb.Cleanup(func() {
		s.DB = original
		_ = db.Close()
	})
	return counter
}

// statementsOf is how many statements f runs on w's store.
func (w *standingWorld) statementsOf(f func()) int64 {
	w.tb.Helper()
	counter := countStatements(w.tb, w.s)
	f()
	return counter.count()
}

type standingWorld struct {
	tb   testing.TB
	c    *Channel
	s    *store.Store
	ctx  context.Context
	at   string
	rels []string
}

func worldStamp(i int) string {
	return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second).Format("2006-01-02T15:04:05.000000+00:00")
}

// newStandingWorld seeds relationships relationships (rel-0000, rel-0001, ... created in that
// order, all active), each with events events: the first events-1 are ordinary failures, which
// raise nothing, and the last is the completion. Each is reachable up the hierarchy to one
// supervisor, so staging its report works.
func newStandingWorld(tb testing.TB, relationships, events int) *standingWorld {
	tb.Helper()
	ctx := context.Background()
	root := tb.TempDir()
	tb.Setenv("HOME", root)
	s, err := store.Open(ctx, filepath.Join(root, "state", "relay.sqlite3"), "")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := s.Close(); err != nil {
			tb.Error(err)
		}
	})
	w := &standingWorld{tb: tb, s: s, ctx: ctx, at: worldStamp(0), c: &Channel{Store: s, Linkage: StoreLinkage{s}, Program: "/usr/bin/codex-session-relay"}}
	err = s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		q := s.Querier(ctx)
		for _, b := range []store.ScopeBindingsRow{
			{BindingID: "b-parent", Role: "parent", ScopeKind: "project", ScopeKey: worldProject, TaskID: "parent", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"},
			{BindingID: "b-supervisor", Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-W", TaskID: "supervisor", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"},
		} {
			if err := storeseed.InsertScopeBinding(ctx, s, b); err != nil {
				return err
			}
		}
		if err := storeseed.InsertScopeLink(ctx, s, store.ScopeLinksRow{LinkID: "lnk-project", LinkKind: "execution", UpperKind: "initiative", UpperKey: "INI-W", UpperTaskID: "supervisor", LowerKind: "project", LowerKey: worldProject, LowerTaskID: "parent", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
			return err
		}
		for i := 0; i < relationships; i++ {
			rid, issue, child := fmt.Sprintf("rel-%04d", i), fmt.Sprintf("ISS-%04d", i), fmt.Sprintf("child-%04d", i)
			w.rels = append(w.rels, rid)
			created := worldStamp(i)
			if _, err := q.ExecContext(ctx, "INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES (?,?,'active','parent','host',?,'host',1,'[]','[]',?,?)", rid, issue, child, created, created); err != nil {
				return err
			}
			if err := storeseed.RecordRelationshipScope(ctx, s, rid, worldProject, "t"); err != nil {
				return err
			}
			if err := storeseed.InsertScopeBinding(ctx, s, store.ScopeBindingsRow{BindingID: "b-" + rid, Role: "child", ScopeKind: "issue", ScopeKey: issue, TaskID: child, HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
				return err
			}
			if err := storeseed.InsertScopeLink(ctx, s, store.ScopeLinksRow{LinkID: "lnk-" + rid, LinkKind: "execution", UpperKind: "project", UpperKey: worldProject, UpperTaskID: "parent", LowerKind: "issue", LowerKey: issue, LowerTaskID: child, Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
				return err
			}
			for j := 0; j < events; j++ {
				event, outcome, seen := fmt.Sprintf("ev-%04d-%03d", i, j), "failed", worldStamp(i*1000+j)
				if j == events-1 {
					outcome = "ready_for_review"
				}
				if _, err := q.ExecContext(ctx, "INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES (?,?,1,'abc123456789abcdef',?,'child','child',?,'completed','{}',?,?)", event, rid, outcome, "turn-"+event, seen, seen); err != nil {
					return err
				}
				if j == events-1 {
					if _, err := q.ExecContext(ctx, "INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES (?,1,?,1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','the work is done','merge',?)", event, rid, seen); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return w
}

func (w *standingWorld) exec(query string, args ...any) {
	w.tb.Helper()
	if _, err := w.s.DB.ExecContext(w.ctx, query, args...); err != nil {
		w.tb.Fatalf("%v\n%s", err, query)
	}
}

// visit is what the daemon does for the project on a tick, less the omission derivation: the
// standing read and the staging pass over it.
func (w *standingWorld) visit() map[string]any {
	w.tb.Helper()
	answer, err := w.c.stageUnsentWithReadings(w.ctx, worldProject, w.at, nil)
	if err != nil {
		w.tb.Fatal(err)
	}
	return answer
}

// settle runs one visit, which stages every report the project owes, and marks them all sent:
// the state of a project whose reports have gone up, which every later visit only reads.
func (w *standingWorld) settle() {
	w.tb.Helper()
	answer := w.visit()
	if staged := answer["staged"].([]any); len(staged) != len(w.rels) {
		w.tb.Fatalf("settled %d of %d reports: %v", len(staged), len(w.rels), answer["refused"])
	}
	w.exec("UPDATE supervisor_messages SET state='dispatched'")
}

// archive archives relationship rid the way the registry does: its status, and the issue edge
// and binding it held, so nothing can be addressed for it any more.
func (w *standingWorld) archive(rid string) {
	w.tb.Helper()
	var issue string
	if err := w.s.DB.QueryRowContext(w.ctx, "SELECT issue_key FROM relationships WHERE relationship_id=?", rid).Scan(&issue); err != nil {
		w.tb.Fatal(err)
	}
	w.exec("UPDATE relationships SET status='archived' WHERE relationship_id=?", rid)
	w.exec("UPDATE scope_bindings SET status='archived' WHERE scope_kind='issue' AND scope_key=?", issue)
	w.exec("UPDATE scope_links SET status='archived' WHERE lower_kind='issue' AND lower_key=?", issue)
}

// retake registers a new child on the issue of relationship rid once rid was archived, as a fresh
// registration does when it names no successor: a live owner binding and the live execution edge up
// to the project. The old relationship stays archived with no successor, and its report can be
// addressed again.
func (w *standingWorld) retake(rid string) {
	w.tb.Helper()
	var issue string
	if err := w.s.DB.QueryRowContext(w.ctx, "SELECT issue_key FROM relationships WHERE relationship_id=?", rid).Scan(&issue); err != nil {
		w.tb.Fatal(err)
	}
	if err := storeseed.InsertScopeBinding(w.ctx, w.s, store.ScopeBindingsRow{BindingID: "b-new-" + rid, Role: "child", ScopeKind: "issue", ScopeKey: issue, TaskID: "child-new", HostID: "host", Status: "active", Revision: 2, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.tb.Fatal(err)
	}
	if err := storeseed.InsertScopeLink(w.ctx, w.s, store.ScopeLinksRow{LinkID: "lnk-new-" + rid, LinkKind: "execution", UpperKind: "project", UpperKey: worldProject, UpperTaskID: "parent", LowerKind: "issue", LowerKey: issue, LowerTaskID: "child-new", Status: "active", Revision: 2, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.tb.Fatal(err)
	}
}
