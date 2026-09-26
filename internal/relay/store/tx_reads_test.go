package store

import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Python runs every read on self.db, the store's one connection, so a read inside composing()
// or transaction() sees the transaction's own uncommitted writes (store.py:1978). These pin
// that a read with the body's ctx goes through the open transaction rather than waiting for it.

// boundedCtx fails the test instead of hanging when a read waits on the held connection.
func boundedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCompose_completes_when_the_body_calls_SetStatus(t *testing.T) {
	// Given: a registered, active relationship.
	s, r := registeredStore(t)
	ctx := boundedCtx(t)
	// When: SetStatus, which reads the relationship before writing, runs inside Compose.
	err := s.Compose(ctx, func(joined context.Context, _ *sql.Conn) error {
		return s.SetStatus(joined, r.ID, "paused", "t")
	})
	// Then: it completes and the status is committed.
	if err != nil {
		t.Fatalf("composed SetStatus: %v", err)
	}
	got, err := s.CurrentRelationship(ctx, r.ID)
	if err != nil || got.Status != "paused" {
		t.Fatalf("status %+v %v", got, err)
	}
}

func TestCompose_completes_when_the_body_calls_Supersede_and_BindAnchor(t *testing.T) {
	// Given: a registered relationship whose generation 1 is still pending.
	s := recordStore(t)
	ctx := boundedCtx(t)
	r := Relationship{ID: "rel-0123456789abcdef", IssueKey: "REL-1", Status: StatusActive, ParentTaskID: "p", ChildTaskID: "c", Generation: 1, ArtifactRoots: "[]", AllowedRecipients: "[]", CreatedAt: "t", UpdatedAt: "t"}
	if err := s.RecordRelationship(ctx, r, Generation{RelationshipID: r.ID, Number: 1, DispatchRequestID: "d1", AnchorState: AnchorPending, OpenedAt: "t"}, "h", "h"); err != nil {
		t.Fatal(err)
	}
	// When: both writers, each reading before it writes, run in one composed transaction.
	err := s.Compose(ctx, func(joined context.Context, _ *sql.Conn) error {
		if err := s.BindAnchor(joined, r.ID, 1, "turn-1", "dispatch_receipt", "t"); err != nil {
			return err
		}
		return s.Supersede(joined, r.ID, "rel-aaaaaaaaaaaaaaaa", "t")
	})
	// Then: it completes, and both writes committed together.
	if err != nil {
		t.Fatalf("composed BindAnchor+Supersede: %v", err)
	}
	g, err := s.Generation(ctx, r.ID, 1)
	if err != nil || g.AnchorState != AnchorBound {
		t.Fatalf("anchor %+v %v", g, err)
	}
	if got, err := s.Relationship(ctx, r.ID); err != nil || got.Status != "archived" {
		t.Fatalf("status %+v %v", got, err)
	}
}

func TestCompose_read_sees_a_relationship_written_earlier_in_the_same_transaction(t *testing.T) {
	// Given: a composed transaction that registers a relationship and has not committed.
	s := recordStore(t)
	ctx := boundedCtx(t)
	r := Relationship{ID: "rel-0123456789abcdef", IssueKey: "REL-1", Status: StatusActive, ParentTaskID: "p", ChildTaskID: "c", Generation: 1, ArtifactRoots: "[]", AllowedRecipients: "[]", CreatedAt: "t", UpdatedAt: "t"}
	g := Generation{RelationshipID: r.ID, Number: 1, DispatchRequestID: "d1", AnchorState: AnchorPending, OpenedAt: "t"}
	err := s.Compose(ctx, func(joined context.Context, _ *sql.Conn) error {
		if err := s.RecordRelationship(joined, r, g, "h", "h"); err != nil {
			return err
		}
		// When: the same transaction reads it back.
		current, err := s.CurrentRelationship(joined, r.ID)
		if err != nil {
			return err
		}
		plain, err := s.Relationship(joined, r.ID)
		if err != nil {
			return err
		}
		// Then: both reads see the uncommitted row, which no other connection can yet.
		if current.ID != r.ID || plain.IssueKey != "REL-1" {
			t.Errorf("read %+v %+v", current, plain)
		}
		if n := durableCount(t, s.Path, `SELECT count(*) FROM relationships`); n != 0 {
			t.Errorf("row committed before the opener: %d", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTransaction_read_with_the_body_ctx_sees_the_uncommitted_write(t *testing.T) {
	// Given: a plain transaction that has written a journal entry and not committed.
	s := recordStore(t)
	ctx := boundedCtx(t)
	err := s.Transaction(ctx, func(txCtx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(txCtx, `INSERT INTO journal (at,kind,subject,detail) VALUES ('t','inside','s','d')`); err != nil {
			return err
		}
		// When: a store reader runs with the body's ctx.
		entries, err := s.Journal(txCtx, "inside", "s")
		// Then: it reads through the open transaction and sees the row.
		if err != nil || len(entries) != 1 {
			t.Errorf("journal inside the transaction: %+v %v", entries, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Every store method that queries must choose its connection through s.q(ctx), so a read
// inside a transaction uses the transaction's connection. Only the pool plumbing itself
// (opening, Transaction acquiring the connection, q, Close) may touch s.DB.
func TestStoreReaders_query_through_the_transaction_aware_querier(t *testing.T) {
	allowed := map[string]bool{"Transaction": true, "q": true, "Close": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		tree, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range tree.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv == nil || allowed[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "DB" {
					return true
				}
				if receiver, ok := sel.X.(*ast.Ident); ok && (receiver.Name == "s") {
					t.Errorf("%s:%s uses s.DB directly; use s.q(ctx)", file, fn.Name.Name)
				}
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Store" {
					t.Errorf("%s:%s uses .Store.DB directly; use Store.q(ctx)", file, fn.Name.Name)
				}
				return true
			})
		}
	}
}
