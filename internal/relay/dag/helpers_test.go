package dag

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// dig is a stand-in for a digest: 64 lowercase hex characters derived from a name.
func dig(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

type doc = map[string]any

func nodeDoc(id, kind string) doc {
	return doc{"node_id": id, "issue_key": "CRW-" + id, "kind": kind, "criteria_set_digest": dig("criteria " + id)}
}

func addNode(id, kind string) doc { return doc{"op": OpAddNode, "node": nodeDoc(id, kind)} }

func edgeDoc(id, from, to, kind string) doc {
	e := doc{"edge_id": id, "from_node_id": from, "to_node_id": to, "kind": kind}
	switch kind {
	case EdgeIntegrated:
		e["target_repository"], e["target_base_ref"] = "owner/repo", "dev"
	case EdgeDecision:
		e["decision_subject"], e["decision_digest"], e["required_authority"] = "merge holds", dig("subject "+id), []any{"user", "owner"}
	}
	return e
}

func addEdge(id, from, to, kind string) doc {
	return doc{"op": OpAddEdge, "edge": edgeDoc(id, from, to, kind)}
}

// codeEdge is an artifact_verified edge out of an implementation node: it pins the verified head.
func codeEdge(id, from, to string) doc {
	e := edgeDoc(id, from, to, EdgeArtifactVerified)
	e["pins_code_head"], e["target_repository"], e["target_base_ref"] = true, "owner/repo", "dev"
	return doc{"op": OpAddEdge, "edge": e}
}

func revDoc(plan, request string, parent int, changes ...doc) doc {
	cs := make([]any, len(changes))
	for i, c := range changes {
		cs[i] = c
	}
	return doc{"schema": SchemaRevision, "plan_id": plan, "project_key": "P-TEST", "request_id": request,
		"expected_parent_revision": parent, "author_task_id": "task-test", "changes": cs}
}

func raw(t testing.TB, d doc) []byte {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decode(t testing.TB, d doc) Revision {
	t.Helper()
	rev, err := DecodeRevision(raw(t, d))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return rev
}

// forkJoin is the plan the tests share: a design node, two implementation nodes that fork from it, and
// a join that integrates both, then a decision after the join.
//
//	design -(artifact_verified)-> impl-a -(integrated)-> join -(decision)-> ship
//	design -(artifact_verified)-> impl-b -(integrated)-> join
func forkJoin(plan, request string) doc {
	return revDoc(plan, request, 0,
		addNode("design", NodeNonPR), addNode("impl-a", NodeImplementation), addNode("impl-b", NodeImplementation),
		addNode("join", NodeNonPR), addNode("ship", NodeNonPR),
		addEdge("e1", "design", "impl-a", EdgeArtifactVerified), addEdge("e2", "design", "impl-b", EdgeArtifactVerified),
		addEdge("e3", "impl-a", "join", EdgeIntegrated), addEdge("e4", "impl-b", "join", EdgeIntegrated),
		addEdge("e5", "join", "ship", EdgeDecision))
}

// newStore creates a go-owned relay store in a temporary directory (the shape every store has: built
// from the frozen fixture) and opens it writably.
func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	testsupport.Create(t, path, "", "go")
	return openStore(t, path), path
}

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newRepo(t *testing.T) (*Repo, *store.Store, string) {
	t.Helper()
	s, path := newStore(t)
	n := 0
	return &Repo{Store: s, Now: func() string { n++; return fmt.Sprintf("2026-10-02T00:00:%02d.000000+00:00", n) }}, s, path
}

// zoneRows is every row of every dag_ table, for "nothing changed" comparisons.
func zoneRows(t testing.TB, db interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}) map[string][]map[string]any {
	t.Helper()
	return testsupport.TableRows(t, db, "name LIKE 'dag\\_%' ESCAPE '\\'")
}

func total(rows map[string][]map[string]any) int {
	n := 0
	for _, r := range rows {
		n += len(r)
	}
	return n
}

// rejected returns the rejection err carries, failing the test when it is anything else.
func rejected(t testing.TB, err error) *PlanRejected {
	t.Helper()
	var p *PlanRejected
	if !errors.As(err, &p) {
		t.Fatalf("want a rejected plan, got %v", err)
	}
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "malformed_receipt" {
		t.Fatalf("a rejected plan is the refusal malformed_receipt (D-02), got %v", err)
	}
	return p
}

func hasRule(p *PlanRejected, rule, pathContains string) bool {
	for _, v := range p.Violations {
		if v.Rule == rule && strings.Contains(v.Path, pathContains) {
			return true
		}
	}
	return false
}
