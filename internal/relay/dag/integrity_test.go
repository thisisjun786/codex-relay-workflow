package dag

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A Go caller of Put holds a typed Revision, not a document, and the rules of a document must hold for it
// too: what the command line would refuse is never stored, whoever the caller is.
func TestPutRefusesATypedRevisionThatADocumentWouldBeRefusedFor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Revision)
		rule   string
	}{
		{"an empty change list", func(r *Revision) { r.Changes = nil }, RuleEmptyChanges},
		{"an op that does not exist", func(r *Revision) { r.Changes = []Change{{Op: "typo"}} }, RuleUnknownOp},
		{"a malformed criteria digest", func(r *Revision) { r.Changes[0].Node.CriteriaSetDigest = "invalid" }, RuleBadDigest},
		{"a node kind that does not exist", func(r *Revision) { r.Changes[0].Node.Kind = "mystery" }, RuleUnknownNodeKind},
		{"an add_node without its node", func(r *Revision) { r.Changes = []Change{{Op: OpAddNode}} }, RuleMissingField},
		{"an add_edge without its edge", func(r *Revision) { r.Changes = []Change{{Op: OpAddEdge}} }, RuleMissingField},
		{"a retire_node without a node id", func(r *Revision) { r.Changes = []Change{{Op: OpRetireNode}} }, RuleEmptyValue},
		{"an identifier with a space", func(r *Revision) { r.Changes[0].Node.NodeID = "has space" }, RuleBadIdentifier},
		{"a project key that is empty", func(r *Revision) { r.ProjectKey = "" }, RuleEmptyValue},
		{"a negative expected parent", func(r *Revision) { r.ExpectedParent = -1 }, RuleWrongType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s, _ := newRepo(t)
			if _, err := r.Put(context.Background(), decode(t, revDoc("plan", "r1", 0, addNode("a", NodeNonPR)))); err != nil {
				t.Fatal(err)
			}
			before := zoneRows(t, s.DB)
			rev := decode(t, revDoc("plan", "r2", 1, addNode("b", NodeNonPR)))
			tc.mutate(&rev)
			_, err := r.Put(context.Background(), rev)
			if err == nil {
				t.Fatalf("the revision was stored; the plan now reads: %v", firstErr(r.Snapshot(context.Background(), "plan", 0)))
			}
			var p *PlanRejected
			if !errors.As(err, &p) || !hasRule(p, tc.rule, "") {
				t.Fatalf("want a rejection by rule %s, got %v", tc.rule, err)
			}
			after := zoneRows(t, s.DB)
			if fmt.Sprint(before) != fmt.Sprint(after) {
				t.Fatalf("a refused revision changed the zone")
			}
			if _, head, err := r.Snapshot(context.Background(), "plan", 0); err != nil || head != 1 {
				t.Fatalf("the plan must still read at revision 1: head %d, %v", head, err)
			}
		})
	}
}

func firstErr(_ Snapshot, _ int64, err error) error { return err }

// A plan whose rows no longer agree with its log is the host's failure wherever it is met: a plain read says so,
// and so must the write that would answer a repeated request from those rows or build the next revision on them.
func damage(t testing.TB, s *store.Store) {
	t.Helper()
	for _, statement := range []string{"DROP TRIGGER dag_nodes_no_delete", "DELETE FROM dag_nodes WHERE node_id = 'b'"} {
		if _, err := s.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAWriteNeverTrustsRowsThatDisagreeWithTheLog(t *testing.T) {
	first := revDoc("plan", "r1", 0, addNode("a", NodeNonPR), addNode("b", NodeNonPR))
	for _, tc := range []struct {
		name string
		put  func(t *testing.T) Revision
	}{
		{"a repeated request", func(t *testing.T) Revision { return decode(t, first) }},
		{"the next revision", func(t *testing.T) Revision { return decode(t, revDoc("plan", "r2", 1, addNode("c", NodeNonPR))) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s, _ := newRepo(t)
			if _, err := r.Put(context.Background(), decode(t, first)); err != nil {
				t.Fatal(err)
			}
			damage(t, s)
			if _, _, err := r.Snapshot(context.Background(), "plan", 0); err == nil {
				t.Fatal("the damage must be visible to a read")
			}
			before := zoneRows(t, s.DB)
			res, err := r.Put(context.Background(), tc.put(t))
			var corrupt *CorruptError
			if !errors.As(err, &corrupt) {
				t.Fatalf("want the host's failure (a corrupt plan), got revision %d, replayed %v, error %v", res.RevisionNo, res.Replayed, err)
			}
			if after := zoneRows(t, s.DB); fmt.Sprint(before) != fmt.Sprint(after) {
				t.Fatal("a refused write changed the zone")
			}
		})
	}
}

func TestACommandNeverTrustsRowsThatDisagreeWithTheLog(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	first := writeDoc(t, dir, "r1.json", revDoc("plan", "r1", 0, addNode("a", NodeNonPR), addNode("b", NodeNonPR)))
	if out, code := crw(t, state, "dag-plan-put", "--request", first); code != 0 {
		t.Fatalf("put: %d %s", code, out)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(state, "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"DROP TRIGGER dag_nodes_no_delete", "DELETE FROM dag_nodes WHERE node_id = 'b'"} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, code := crw(t, state, "dag-plan-show", "--plan", "plan"); code != 3 {
		t.Fatalf("the damage must be the host's failure on a read, exit %d", code)
	}
	second := writeDoc(t, dir, "r2.json", revDoc("plan", "r2", 1, addNode("c", NodeNonPR)))
	for name, request := range map[string]string{"repeating the first request": first, "appending a revision": second} {
		if out, code := crw(t, state, "dag-plan-put", "--request", request); code != 3 || !strings.Contains(out, "corrupt") {
			t.Errorf("%s: exit %d\n%s", name, code, out)
		}
	}
}

// interposed runs hook once, just before the nth query it is given: the moment another writer's commit can land
// between two reads of a refusal's early form.
type interposed struct {
	Queryer
	n      int
	hook   func()
	failAt int // the read, counted from 1, that fails with fault instead of running
	fault  error
	seen   int
}

func (q *interposed) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.seen++
	if q.hook != nil && q.seen == q.n {
		q.hook()
	}
	if q.failAt != 0 && q.seen == q.failAt {
		return nil, q.fault
	}
	return q.Queryer.QueryContext(ctx, query, args...)
}

// A request that commits while its own early form is reading is a repeated request, not a stale one: whatever the
// early form saw of the plan after that commit (the head moved on, the node already there), the writing open returns
// the stored result, so the early form must hand it on.
func TestPreflightHandsOnARequestThatCommittedWhileItWasReading(t *testing.T) {
	for n := 1; n <= 5; n++ {
		t.Run(fmt.Sprintf("commit before read %d", n), func(t *testing.T) {
			r, _, path := newRepo(t)
			if _, err := r.Put(context.Background(), decode(t, revDoc("plan", "r1", 0, addNode("a", NodeNonPR)))); err != nil {
				t.Fatal(err)
			}
			rev := decode(t, revDoc("plan", "r2", 1, addNode("b", NodeNonPR)))
			ro, err := store.OpenInPlace(context.Background(), path, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer ro.Close()
			q := &interposed{Queryer: ro, n: n, hook: func() {
				if _, err := r.Put(context.Background(), rev); err != nil {
					t.Error(err)
				}
			}}
			if err := preflightRead(context.Background(), q, rev); err != nil {
				t.Fatalf("the repeated request was refused: %v", err)
			}
			if q.seen < n {
				t.Fatalf("the early form made %d reads, so the commit never landed between two of them", q.seen)
			}
		})
	}
}

// A request that does not commit is still refused: the re-check hands on only what the store holds.
func TestPreflightStillRefusesAStaleRequest(t *testing.T) {
	r, _, path := newRepo(t)
	if _, err := r.Put(context.Background(), decode(t, revDoc("plan", "r1", 0, addNode("a", NodeNonPR)))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Put(context.Background(), decode(t, revDoc("plan", "r2", 1, addNode("b", NodeNonPR)))); err != nil {
		t.Fatal(err)
	}
	err := Preflight(context.Background(), path, decode(t, revDoc("plan", "r3", 1, addNode("c", NodeNonPR))))
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "plan_revision_conflict" {
		t.Fatalf("a stale parent is plan_revision_conflict, got %v", err)
	}
}

// The same request sent by many processes at once is answered by every one of them, one revision being the result.
func TestRepeatedRequestsRacingTheirFirstCopyAreAllAnswered(t *testing.T) {
	r, _, path := newRepo(t)
	if _, err := r.Put(context.Background(), decode(t, revDoc("plan", "r1", 0, addNode("a", NodeNonPR)))); err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 12; round++ {
		rev := decode(t, revDoc("plan", fmt.Sprintf("r%d", round+1), round, addNode(fmt.Sprintf("n%d", round), NodeNonPR)))
		const writers = 12
		errs := make([]error, writers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				if err := Preflight(context.Background(), path, rev); err != nil {
					errs[i] = fmt.Errorf("preflight: %w", err)
					return
				}
				_, errs[i] = r.Put(context.Background(), rev)
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d writer %d: the identical request was refused: %v", round, i, err)
			}
		}
		if _, head, err := r.Snapshot(context.Background(), "plan", 0); err != nil || head != int64(round+1) {
			t.Fatalf("round %d: head %d, %v; one revision per round", round, head, err)
		}
	}
}

// A refusal is handed on only for a request the log holds, and a failure to read the store is never turned into
// either answer: not into a refusal by a second read that failed, and not into "go on" by a second read that worked.
func TestPreflightReturnsAFailureToReadAsItIs(t *testing.T) {
	fault := errors.New("injected storage read failure")
	t.Run("the look-up that follows a refusal fails", func(t *testing.T) {
		r, s, _ := newRepo(t)
		mustPut(t, r, revDoc("plan", "r1", 0, addNode("a", NodeNonPR)))
		mustPut(t, r, revDoc("plan", "r2", 1, addNode("b", NodeNonPR)))
		q := &interposed{Queryer: s.DB, failAt: 4, fault: fault}
		err := preflightRead(context.Background(), q, decode(t, revDoc("plan", "stale", 1, addNode("c", NodeNonPR))))
		if !errors.Is(err, fault) {
			t.Fatalf("the failure of the look-up was answered as %v", err)
		}
	})
	t.Run("a read fails before the request appears in the log", func(t *testing.T) {
		r, s, _ := newRepo(t)
		mustPut(t, r, revDoc("plan", "r1", 0, addNode("a", NodeNonPR)))
		rev := decode(t, revDoc("plan", "r2", 1, addNode("b", NodeNonPR)))
		q := &interposed{Queryer: s.DB, n: 2, hook: func() {
			if _, err := r.Put(context.Background(), rev); err != nil {
				t.Error(err)
			}
		}, failAt: 2, fault: fault}
		err := preflightRead(context.Background(), q, rev)
		if !errors.Is(err, fault) {
			t.Fatalf("the failure to read was answered as %v", err)
		}
	})
}

// A document at the size limit is judged as the document it is: the typed form of it, written out again, is not
// longer for the fields a document may leave out.
func TestACheckedDocumentAtTheSizeLimitIsStillAccepted(t *testing.T) {
	changes := []doc{addNode("a", NodeNonPR), addNode("b", NodeNonPR)}
	var authorities [][]any
	for i := 0; i < 100; i++ {
		e := edgeDoc(fmt.Sprintf("e%d", i), "a", "b", EdgeDecision)
		list := make([]any, 16)
		for j := range list {
			list[j] = fmt.Sprintf("%02d", j)
		}
		authorities = append(authorities, list)
		e["required_authority"] = list
		changes = append(changes, doc{"op": OpAddEdge, "edge": e})
	}
	d := revDoc("plan", "r1", 0, changes...)
	remaining := MaxDocumentBytes - len(canonical(d))
	for _, list := range authorities {
		for j := range list {
			n := remaining
			if n > 1016 {
				n = 1016
			}
			// a text is limited in characters, not bytes: four-byte characters fill the document within it
			list[j] = list[j].(string) + strings.Repeat("😀", n/4) + strings.Repeat("x", n%4)
			remaining -= n
		}
	}
	if remaining != 0 {
		t.Fatalf("the test document does not fill the limit: %d bytes left over", remaining)
	}
	encoded := []byte(canonical(d))
	if len(encoded) != MaxDocumentBytes {
		t.Fatalf("the document is %d bytes, not the limit %d", len(encoded), MaxDocumentBytes)
	}
	rev, err := DecodeRevision(encoded)
	if err != nil {
		t.Fatalf("the document itself is refused: %v", err)
	}
	r, _, path := newRepo(t)
	if err := Preflight(context.Background(), path, rev); err != nil {
		t.Errorf("Preflight refused a valid document of %d bytes: %v", len(encoded), err)
	}
	if _, err := r.Put(context.Background(), rev); err != nil {
		t.Errorf("Put refused a valid document of %d bytes: %v", len(encoded), err)
	}
}
