package faults

import (
	"context"
	"database/sql"
)

// expireLeases releases unissued claims and fences issued writes as uncertain.
// Like Python, each call drains at most one oldest-first relink-sized batch.
func (l *Ledger) expireLeases(ctx context.Context) error {
	moment, stamp := l.Clock.Now(), l.Clock.ISO()
	return l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		rows, err := l.Store.All(ctx, "SELECT * FROM fault_publications WHERE state IN ('claimed','issued') AND lease_until IS NOT NULL AND lease_until<=? ORDER BY lease_until,rowid LIMIT 100", moment)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.Text("state") == "claimed" {
				if err = f1Release(ctx, l, r, stamp, "lease_lapsed", nil, ""); err != nil {
					return err
				}
				continue
			}
			id := r.Text("publication_id")
			if _, err = l.exec(ctx, "UPDATE fault_publications SET state='uncertain',claim_token=NULL,lease_owner=NULL,lease_until=NULL,updated_at=?,last_error=COALESCE(last_error,'the lease expired after the write was issued') WHERE publication_id=?", stamp, id); err != nil {
				return err
			}
			if _, err = l.exec(ctx, "UPDATE fault_publication_attempts SET outcome='uncertain' WHERE attempt_id=(SELECT MAX(attempt_id) FROM fault_publication_attempts WHERE publication_id=?)", id); err != nil {
				return err
			}
			if err = f2Notify(ctx, l, r, "uncertain", stamp); err != nil {
				return err
			}
		}
		return nil
	})
}
