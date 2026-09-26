package registry

import (
	"context"
	"database/sql"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Subset ported for todo 26; todo 27 owns and extends this for capacity and edit regions.
// coordination.py: shared derived identities and durable contests.
const (
	DomainMergeTarget = "merge_target"
	DomainExecution   = "execution_subject"
	DomainEditRegion  = "edit_region"
)

// CoordinationExact validates a field before it enters a derived identity.
func CoordinationExact(value, what string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", refuse(contract.RefusalUnregisteredScope, "%s must be a non-empty string, not %s", what, pyStr(value))
	}
	if strings.Contains(value, "|") {
		return "", refuse(contract.RefusalUnregisteredScope, "%s must not contain '|', which is the field separator", what)
	}
	return value, nil
}

// CoordinationID derives a relay-owned 128-bit identity from the defining fields in order.
func CoordinationID(prefix string, fields ...string) string {
	return prefix + "-" + sha256Hex(strings.Join(fields, "|"))[:32]
}

// CoordinationRefusal is a decided refusal, committed as a conflict before returning its error.
type CoordinationRefusal struct {
	Reason                                         contract.RefusalReason
	Detail, Domain, Subject, Incumbent, Challenger string
}

func (r CoordinationRefusal) Error() error {
	return &store.RefusedError{Reason: string(r.Reason), Detail: r.Detail}
}

func (r CoordinationRefusal) Record() contract.OrderedObject {
	return contract.OrderedObject{{Key: "domain", Value: r.Domain}, {Key: "subject", Value: r.Subject},
		{Key: "reason", Value: string(r.Reason)}, {Key: "detail", Value: r.Detail},
		{Key: "incumbent", Value: orNone(r.Incumbent)}, {Key: "challenger", Value: orNone(r.Challenger)}}
}

// RecordCoordinationConflict writes through the current transaction connection. The caller
// must return the refusal only after that transaction commits, or the evidence is lost.
func (r *Registry) RecordCoordinationConflict(ctx context.Context, refusal CoordinationRefusal, at string) error {
	_, err := r.Store.Querier(ctx).ExecContext(ctx,
		"INSERT INTO coordination_conflicts (at, domain, subject, reason, incumbent, challenger, detail) VALUES (?,?,?,?,?,?,?)"+
			" ON CONFLICT (domain, subject, reason, incumbent, challenger) DO UPDATE SET at = excluded.at, detail = excluded.detail",
		at, refusal.Domain, refusal.Subject, string(refusal.Reason), refusal.Incumbent, refusal.Challenger, refusal.Detail)
	return err
}

// CoordinationConflicts lists contests in insertion order, with empty identities as null.
func (r *Registry) CoordinationConflicts(ctx context.Context, domain, subject string) ([]contract.OrderedObject, error) {
	rows, err := r.Store.All(ctx, "SELECT * FROM coordination_conflicts WHERE domain = ? AND subject = ? ORDER BY id", domain, subject)
	if err != nil {
		return nil, err
	}
	result := make([]contract.OrderedObject, 0, len(rows))
	for _, row := range rows {
		result = append(result, contract.OrderedObject{{Key: "at", Value: row.Get("at")}, {Key: "domain", Value: row.Get("domain")},
			{Key: "subject", Value: row.Get("subject")}, {Key: "reason", Value: row.Get("reason")},
			{Key: "detail", Value: row.Get("detail")}, {Key: "incumbent", Value: orNone(row.Get("incumbent"))},
			{Key: "challenger", Value: orNone(row.Get("challenger"))}})
	}
	return result, nil
}

// RecordCoordinationRefusal commits a refusal without rolling its conflict back.
func (r *Registry) RecordCoordinationRefusal(ctx context.Context, refusal CoordinationRefusal, at string) error {
	err := r.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		return r.RecordCoordinationConflict(txCtx, refusal, at)
	})
	if err != nil {
		return err
	}
	return refusal.Error()
}
