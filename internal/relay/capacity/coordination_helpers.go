package capacity

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const (
	domainExecution  = registry.DomainExecution
	domainEditRegion = registry.DomainEditRegion
)

func refuse(reason contract.RefusalReason, detail string) error {
	return &store.RefusedError{Reason: string(reason), Detail: detail}
}
func exact(value, what string) error { _, err := registry.CoordinationExact(value, what); return err }
func derive(prefix string, fields ...string) string {
	return registry.CoordinationID(prefix, fields...)
}

// refusal is a contest the edit-region and capacity ledgers record in coordination_conflicts
// before refusing it (recordIn); the stored detail keeps the repr() quoting it was always written
// with. A refusal that is only returned is built with refuse and quotes as Go does.
type refusal struct {
	reason                                         contract.RefusalReason
	detail, domain, subject, incumbent, challenger string
}

func (r *refusal) err() error { return refuse(r.reason, r.detail) }
func recordIn(ctx context.Context, s *store.Store, r *refusal, at string) error {
	return (&registry.Registry{Store: s}).RecordCoordinationConflict(ctx, registry.CoordinationRefusal{Reason: r.reason, Detail: r.detail, Domain: r.domain, Subject: r.subject, Incumbent: r.incumbent, Challenger: r.challenger}, at)
}
func Conflicts(ctx context.Context, s *store.Store, domain, subject string) ([]any, error) {
	rows, err := (&registry.Registry{Store: s}).CoordinationConflicts(ctx, domain, subject)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out, nil
}
func journal(ctx context.Context, s *store.Store, kind, subject string, detail any, at string) error {
	return (&registry.Registry{Store: s}).Journal(ctx, kind, subject, detail, at)
}

func nullable(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

// orNone is Python's `value or None` for a text column.
func orNone(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func noRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// unwrapRefusal returns the refusal a transaction body raised, without the transaction's own
// wrapping, so the caller sees exactly the RelayError Python raises out of its `with` block.
func unwrapRefusal(err error) error {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused
	}
	return err
}
