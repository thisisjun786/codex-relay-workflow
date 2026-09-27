package faults

// These selections match faults.py _repoint_where and _relink_where. They only read
// write state; operation/claim/complete/fail/reconcile own its transitions.
func dRepointQuery() (string, string, []any, []any) {
	mode := "COALESCE(pp.target_mode, CASE WHEN p.kind IN (?) THEN 'team+project' WHEN p.kind IN (?) THEN 'team' END)"
	owned := "(tp.product = f.product AND NOT EXISTS (SELECT 1 FROM fault_ledger o WHERE o.scope_key = f.scope_key AND o.product != f.product))"
	team := "(CASE WHEN " + owned + " THEN t.tracker_ref END)"
	project := "(CASE WHEN " + mode + " = 'team+project' AND " + owned + " THEN tp.project_ref END)"
	base := " FROM fault_publications p JOIN fault_ledger f ON f.fault_id = p.fault_id" +
		" LEFT JOIN fault_targets t ON t.scope_key = f.scope_key" +
		" LEFT JOIN fault_target_projects tp ON tp.scope_key = f.scope_key" +
		" LEFT JOIN fault_publication_payloads pp ON pp.publication_id = p.publication_id" +
		" WHERE p.state IN (?,?) AND " + mode + " IN ('team', 'team+project')" +
		" AND (p.tracker_ref IS NOT " + team + " OR pp.project_ref IS NOT " + project + ")"
	// The selected team precedes the project and WHERE bindings. The count uses
	// only WHERE bindings, preserving the same bounded selection after writes.
	where := []any{"pending", "failed", "open_record", "project_create", "open_record", "project_create"}
	selected := append([]any{"open_record", "project_create"}, where...)
	return "SELECT p.publication_id, " + team + " AS team, " + project + " AS project" + base, base, selected, where
}

func dRepointFaultQuery(id string) (string, string, []any, []any) {
	query, base, selected, where := dRepointQuery()
	// This selector is appended after the comparison predicate, so its binding
	// follows the existing WHERE bindings in both the selected and count queries.
	query += " AND f.fault_id = ?"
	base += " AND f.fault_id = ?"
	selected = append(selected, id)
	where = append(where, id)
	return query, base, selected, where
}

func dRelinkQuery() string {
	const setOpp = "(CASE WHEN json_valid(opp.payload) THEN json_extract(opp.payload, '$.op') END) = 'set_project'"
	const setTwp = "(CASE WHEN json_valid(twp.payload) THEN json_extract(twp.payload, '$.op') END) = 'set_project'"
	return "SELECT f.fault_id, p.project_ref FROM fault_ledger f" +
		" LEFT JOIN fault_target_projects p ON p.scope_key = f.scope_key AND p.product = f.product" +
		" LEFT JOIN fault_links l ON l.fault_id = f.fault_id" +
		" WHERE f.external_ref IS NOT NULL" +
		" AND ((p.project_ref IS NOT NULL" +
		" AND (l.fault_id IS NULL OR l.project_ref IS NOT p.project_ref" +
		" OR (l.state = ? AND NOT EXISTS (SELECT 1 FROM fault_publications op" +
		" LEFT JOIN fault_publication_payloads opp ON opp.publication_id = op.publication_id" +
		" WHERE op.fault_id = f.fault_id AND op.kind = ? AND op.state IN (?,?)" +
		" AND " + setOpp +
		" AND (CASE WHEN json_valid(opp.payload) THEN json_extract(opp.payload, '$.value') END) IS NOT p.project_ref)" +
		" AND (l.observed_project_ref IS p.project_ref" +
		" OR NOT EXISTS (SELECT 1 FROM fault_publications tw" +
		" LEFT JOIN fault_publication_payloads twp ON twp.publication_id = tw.publication_id" +
		" WHERE tw.fault_id = f.fault_id AND tw.kind = ? AND tw.state IN (?,?,?,?,?)" +
		" AND " + setTwp +
		" AND (CASE WHEN json_valid(twp.payload) THEN json_extract(twp.payload, '$.value') END) IS p.project_ref)))))" +
		" OR (p.project_ref IS NULL AND (l.fault_id IS NULL OR l.project_ref IS NOT NULL OR l.state != ?)))"
}
func dRelinkArgs() []any {
	return []any{"unlinked", "update_record", "issued", "uncertain", "update_record", "pending", "claimed", "issued", "uncertain", "failed", "unlinked"}
}
