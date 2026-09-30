package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func Test24_SCH_1_StoreLiveHierarchy(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("HOME", root)
	s, err := store.Open(ctx, filepath.Join(root, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.DB.ExecContext(ctx, `INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES ('r','CRW-1','active','parent','host','child','host',1,'[]','[]','t','t')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = storeseed.RecordRelationshipScope(ctx, s, "r", "PRJ-1", "t"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []store.ScopeBindingsRow{{BindingID: "b-child", Role: "child", ScopeKind: "issue", ScopeKey: "CRW-1", TaskID: "child", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}, {BindingID: "b-parent", Role: "parent", ScopeKind: "project", ScopeKey: "PRJ-1", TaskID: "parent", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}, {BindingID: "b-supervisor", Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "supervisor", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}} {
		if err = storeseed.InsertScopeBinding(ctx, s, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []store.ScopeLinksRow{{LinkID: "lnk-issue", LinkKind: "execution", UpperKind: "project", UpperKey: "PRJ-1", UpperTaskID: "parent", LowerKind: "issue", LowerKey: "CRW-1", LowerTaskID: "child", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}, {LinkID: "lnk-project", LinkKind: "execution", UpperKind: "initiative", UpperKey: "INI-1", UpperTaskID: "supervisor", LowerKind: "project", LowerKey: "PRJ-1", LowerTaskID: "parent", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}} {
		if err = storeseed.InsertScopeLink(ctx, s, l); err != nil {
			t.Fatal(err)
		}
	}
	c := Channel{Store: s, Linkage: StoreLinkage{s}}
	got, err := c.Resolve(ctx, "r")
	if err != nil || got != (Resolution{"parent", "supervisor", "PRJ-1", "INI-1", "linkage"}) {
		t.Fatalf("resolution %+v: %v", got, err)
	}
	// Retained write refusals do not stop a live hierarchy.
	if err = storeseed.RecordLinkageConflict(ctx, s, store.LinkageConflictsRow{At: "t", ScopeKind: "project", ScopeKey: "PRJ-1", Reason: "role_already_bound", Incumbent: "parent", Challenger: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Resolve(ctx, "r"); err != nil {
		t.Fatalf("historical refusal blocked hierarchy: %v", err)
	}
	if err = storeseed.SetScopeLinkStatus(ctx, s, "lnk-project", "archived", "t2"); err != nil {
		t.Fatal(err)
	}
	_, err = c.Resolve(ctx, "r")
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "unregistered_scope" || !strings.Contains(refusal.Detail, "nobody to report to") {
		t.Fatalf("gap: %v", err)
	}
	if err = storeseed.SetScopeLinkStatus(ctx, s, "lnk-project", "active", "t3"); err != nil {
		t.Fatal(err)
	}
	// A successor without a repointed edge is drift, not permission to send to the old owner.
	if err = storeseed.ArchiveScopeBinding(ctx, s, "b-supervisor", "archived", "b-successor", "t4"); err != nil {
		t.Fatal(err)
	}
	b := store.ScopeBindingsRow{BindingID: "b-successor", Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "successor", HostID: "host", Status: "active", Revision: 2, CreatedAt: "t4", UpdatedAt: "t4"}
	if err = storeseed.InsertScopeBinding(ctx, s, b); err != nil {
		t.Fatal(err)
	}
	_, err = c.Resolve(ctx, "r")
	if !errors.As(err, &refusal) || refusal.Reason != "relation_owner_drift" {
		t.Fatalf("drift: %v", err)
	}
}
