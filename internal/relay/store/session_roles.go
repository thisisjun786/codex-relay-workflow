package store

import (
	"context"
	"sort"
)

// SessionScopeRoles is the roles the live scope bindings of the registry give one session (CRW-1084 / CRW-386's seam, read only):
// every distinct role of a binding that is active or paused and was not replaced, whose task id or recorded CXC session is the
// session id. It is the same predicate registry boundRole applies to a task. It reads through ReadOnlyRows, so no store is
// created or migrated, and the file it read is the one at the pathname.
//
// ok is false when nothing could be read (no store, a store without the table, a refused or relocated file): the caller then
// has no role evidence, which is not the same as a session with no binding (ok with no roles).
func SessionScopeRoles(ctx context.Context, selection StateSelection, session string) (roles []string, ok bool) {
	if session == "" {
		return nil, false
	}
	read := ReadOnlyRows(ctx, selection, "SELECT DISTINCT role FROM scope_bindings WHERE (task_id = ? OR cxc_session = ?)"+
		" AND status IN ('active','paused') AND superseded_by IS NULL", []any{session, session}, func(row RowScanner) error {
		var role string
		if err := row.Scan(&role); err != nil {
			return err
		}
		roles = append(roles, role)
		return nil
	})
	if !read.Readable || read.Detail != "" || read.Raised != nil {
		return nil, false
	}
	sort.Strings(roles)
	return roles, true
}
