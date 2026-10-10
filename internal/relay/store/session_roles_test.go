package store

import (
	"context"
	"os"
	"reflect"
	"testing"
)

// SessionScopeRoles reads the live binding roles of a session without creating a store (CRW-1084).
func TestSessionScopeRoles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	selection := StateSelection{Path: dir}
	if roles, ok := SessionScopeRoles(ctx, selection, "s1"); ok || roles != nil {
		t.Fatalf("no store: %v %v", roles, ok)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("the read created %v (%v)", entries, err)
	}
	st, err := Open(ctx, selection.DBPath(), "")
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id, role, task, session, status, supersededBy string) {
		t.Helper()
		var sess, sup any
		if session != "" {
			sess = session
		}
		if supersededBy != "" {
			sup = supersededBy
		}
		if _, err := st.DB.ExecContext(ctx, "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, cwd, cxc_session,"+
			" status, revision, supersedes, superseded_by, handover_note, created_at, updated_at) VALUES (?,?,?,?,?,?,NULL,?,?,1,NULL,?,NULL,'t','t')",
			id, role, "issue", id, task, "h", sess, status, sup); err != nil {
			t.Fatal(err)
		}
	}
	insert("b1", "child", "task-a", "", "active", "")
	insert("b2", "parent", "task-b", "", "paused", "")
	insert("b3", "child", "task-c", "sess-c", "active", "")
	insert("b4", "child", "task-d", "", "archived", "")
	insert("b5", "child", "task-e", "", "active", "b6")
	insert("b7", "child", "task-f", "", "active", "")
	insert("b8", "parent", "task-f", "", "active", "")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		session string
		want    []string
	}{
		{"task-a", []string{"child"}},
		{"task-b", []string{"parent"}},
		{"task-c", []string{"child"}},
		{"sess-c", []string{"child"}},
		{"task-d", nil},
		{"task-e", nil},
		{"task-f", []string{"child", "parent"}},
		{"nobody", nil},
	} {
		roles, ok := SessionScopeRoles(ctx, selection, c.session)
		if !ok || !reflect.DeepEqual(roles, c.want) {
			t.Errorf("%s: %v %v, want %v", c.session, roles, ok, c.want)
		}
	}
	if roles, ok := SessionScopeRoles(ctx, selection, ""); ok || roles != nil {
		t.Errorf("empty session: %v %v", roles, ok)
	}
}
