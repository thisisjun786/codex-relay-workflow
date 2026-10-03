package faults

import (
	"context"
	"fmt"
)

// Relink's writes stay on the same transaction as its bounded selections.
func dRelinkSummary(ctx context.Context, l *Ledger, f row, trigger, publication string) (string, error) {
	rows, e := l.Store.All(ctx, "SELECT * FROM fault_occurrences WHERE fault_id=? ORDER BY rowid DESC LIMIT ?", f.Text("fault_id"), renderedOccur)
	if e != nil {
		return "", e
	}
	return renderSummary(f, trigger, rows, "", publication), nil
}
func dCancelRelinks(ctx context.Context, l *Ledger, id, reason, stamp string) error {
	rows, e := l.Store.All(ctx, "SELECT p.publication_id,p.state,p.attempts,f.product,p.kind FROM fault_publications p JOIN fault_ledger f ON f.fault_id=p.fault_id JOIN fault_publication_payloads pp ON pp.publication_id=p.publication_id WHERE p.fault_id=? AND p.kind='update_record' AND p.state IN ('pending','failed','claimed') AND (CASE WHEN json_valid(pp.payload) THEN json_extract(pp.payload,'$.op') END)='set_project' ORDER BY p.rowid", id)
	if e != nil {
		return e
	}
	for _, r := range rows {
		publication := r.Text("publication_id")
		attempts := integer(r, "attempts")
		if r.Text("state") == "claimed" {
			attempt, e := l.one(ctx, "SELECT attempt_id FROM fault_publication_attempts WHERE publication_id=? ORDER BY attempt_id DESC LIMIT 1", publication)
			if e != nil {
				return e
			}
			if attempt != nil {
				ref := fmt.Sprintf("%s:%d", publication, integer(attempt, "attempt_id"))
				if _, e = l.exec(ctx, "DELETE FROM fault_budget_uses WHERE product=? AND kind=? AND ref=?", r.Text("product"), r.Text("kind"), ref); e != nil {
					return e
				}
				if _, e = l.exec(ctx, "UPDATE fault_publication_attempts SET outcome='cancelled',ended=1,ended_at=? WHERE attempt_id=?", stamp, integer(attempt, "attempt_id")); e != nil {
					return e
				}
			}
			attempts--
		}
		if _, e = l.exec(ctx, "UPDATE fault_publications SET state='cancelled',claim_token=NULL,lease_owner=NULL,lease_until=NULL,last_error=?,updated_at=?,attempts=? WHERE publication_id=?", reason, stamp, attempts, publication); e != nil {
			return e
		}
	}
	return nil
}
func dUnlinkOne(ctx context.Context, l *Ledger, id, stamp string) error {
	f, e := l.one(ctx, "SELECT external_ref FROM fault_ledger WHERE fault_id=?", id)
	if e != nil {
		return e
	}
	if f == nil || f.Get("external_ref") == nil {
		return nil
	}
	link, e := l.one(ctx, "SELECT fault_id FROM fault_links WHERE fault_id=?", id)
	if e != nil {
		return e
	}
	if link == nil {
		_, e = l.exec(ctx, "INSERT INTO fault_links(fault_id,external_ref,project_ref,observed_project_ref,state,revision,updated_at) VALUES(?,?,NULL,NULL,'unlinked',0,?)", id, f.Get("external_ref"), stamp)
	} else {
		_, e = l.exec(ctx, "UPDATE fault_links SET project_ref=NULL,state='unlinked',updated_at=? WHERE fault_id=?", stamp, id)
	}
	if e != nil {
		return e
	}
	return dCancelRelinks(ctx, l, id, "the scope no longer targets a project", stamp)
}
func dRelinkOne(ctx context.Context, l *Ledger, id, project, stamp string) error {
	f, e := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id=?", id)
	if e != nil {
		return e
	}
	if f == nil || f.Get("external_ref") == nil {
		return nil
	}
	link, e := l.one(ctx, "SELECT * FROM fault_links WHERE fault_id=?", id)
	if e != nil {
		return e
	}
	if link == nil {
		if _, e = l.exec(ctx, "INSERT INTO fault_links(fault_id,external_ref,project_ref,observed_project_ref,state,revision,updated_at) VALUES(?,?,?,NULL,'unlinked',0,?)", id, f.Get("external_ref"), project, stamp); e != nil {
			return e
		}
		link, e = l.one(ctx, "SELECT * FROM fault_links WHERE fault_id=?", id)
		if e != nil {
			return e
		}
	}
	if link.Text("observed_project_ref") == project {
		moving, e := l.one(ctx, "SELECT 1 FROM fault_publications p LEFT JOIN fault_publication_payloads pp ON pp.publication_id=p.publication_id WHERE p.fault_id=? AND p.kind='update_record' AND p.state IN ('issued','uncertain') AND (CASE WHEN json_valid(pp.payload) THEN json_extract(pp.payload,'$.op') END)='set_project' AND (CASE WHEN json_valid(pp.payload) THEN json_extract(pp.payload,'$.value') END) IS NOT ? LIMIT 1", id, project)
		if e != nil {
			return e
		}
		if e = dCancelRelinks(ctx, l, id, "superseded by a later target", stamp); e != nil {
			return e
		}
		state := "linked"
		if moving != nil {
			state = "unlinked"
		}
		_, e = l.exec(ctx, "UPDATE fault_links SET project_ref=?,state=?,updated_at=? WHERE fault_id=?", project, state, stamp, id)
		return e
	}
	revision := integer(link, "revision")
	trigger := fmt.Sprintf("update:set_project:%s:r%d", project, revision)
	if link.Text("project_ref") == project && link.Text("state") == "unlinked" {
		live, e := l.one(ctx, "SELECT 1 FROM fault_publications WHERE fault_id=? AND kind='update_record' AND state NOT IN ('cancelled','confirmed') AND trigger_key=?", id, trigger)
		if e != nil {
			return e
		}
		if live != nil {
			return nil
		}
	}
	if e = dCancelRelinks(ctx, l, id, "superseded by a later target", stamp); e != nil {
		return e
	}
	outstanding, e := l.one(ctx, "SELECT 1 FROM fault_publications p LEFT JOIN fault_publication_payloads pp ON pp.publication_id=p.publication_id WHERE p.fault_id=? AND p.kind='update_record' AND p.state IN ('issued','uncertain') AND (CASE WHEN json_valid(pp.payload) THEN json_extract(pp.payload,'$.op') END)='set_project' LIMIT 1", id)
	if e != nil {
		return e
	}
	if outstanding != nil {
		_, e = l.exec(ctx, "UPDATE fault_links SET project_ref=?,state='unlinked',updated_at=? WHERE fault_id=?", project, stamp, id)
		return e
	}
	revision++
	if _, e = l.exec(ctx, "UPDATE fault_links SET project_ref=?,state='unlinked',revision=?,updated_at=? WHERE fault_id=?", project, revision, stamp, id); e != nil {
		return e
	}
	trigger = fmt.Sprintf("update:set_project:%s:r%d", project, revision)
	publication := publicationID(id, "update_record", trigger)
	// A fields update is issued on the owned issue, not on the target team.
	var team any
	summary, e := dRelinkSummary(ctx, l, f, trigger, publication)
	if e != nil {
		return e
	}
	digest := identityDigest(id, "update_record", trigger, integer(f, "cycle"))
	existing, e := l.one(ctx, "SELECT state FROM fault_publications WHERE publication_id=?", publication)
	if e != nil {
		return e
	}
	if existing == nil {
		_, e = l.exec(ctx, "INSERT INTO fault_publications(publication_id,fault_id,kind,trigger_key,cycle,tracker_ref,external_ref,summary,identity_digest,state,attempts,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'pending',0,?,?)", publication, id, "update_record", trigger, integer(f, "cycle"), team, f.Get("external_ref"), summary, digest, stamp, stamp)
	} else if existing.Text("state") == "cancelled" {
		_, e = l.exec(ctx, "UPDATE fault_publications SET state='pending',cycle=?,tracker_ref=?,external_ref=?,summary=?,identity_digest=?,attempts=0,next_attempt_at=NULL,claim_token=NULL,lease_owner=NULL,lease_until=NULL,issued_at=NULL,last_error=NULL,updated_at=? WHERE publication_id=?", integer(f, "cycle"), team, f.Get("external_ref"), summary, digest, stamp, publication)
	}
	if e != nil {
		return e
	}
	payload := fmt.Sprintf(`{"op": "set_project", "value": %s}`, dumps(project, true))
	_, e = l.exec(ctx, "INSERT INTO fault_publication_payloads(publication_id,project_ref,payload,hold_reason,updated_at,target_mode) VALUES(?,NULL,?,NULL,?,'none') ON CONFLICT(publication_id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at,target_mode=excluded.target_mode", publication, payload, stamp)
	return e
}
