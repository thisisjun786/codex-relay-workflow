package mergeturn_test

// headCompareWakeReport records the work report of assignment rel-a (the one the wake tests claim with) that names head.
// merge-turn-ready and merge-turn-check compare the head of a claim that names a relationship with the work reports of that
// relationship (CRW-586), and nothing in the product records one (docs/port/decisions.md, section 53), so these tests record
// the head they restate or check.
func headCompareWakeReport(w *wakeParity, head string) {
	w.t.Helper()
	w.exec(`INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,base_ref,head_sha,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"event-hc-"+head, 1, "rel-a", 3, "rev", "owner/repo", "dev", head, "done", "r", "1", "s", "n", w.clock.ISO())
}
