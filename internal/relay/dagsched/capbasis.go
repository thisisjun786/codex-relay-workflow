package dagsched

import (
	"context"
	"database/sql"
	"math"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// CapBasis is the evidence behind a concurrency ceiling above the standing cap (contract 7.3, D-05): the minutes W of a child's wall time and S of the parent's own serial work that the
// ceiling was derived from, and where each came from. The scheduler's capacity reading honours a declared ceiling above the standing cap of 6 only for a limit revision that has a row
// here; the value of the ceiling itself is not decided by this issue and nothing here raises one.
type CapBasis struct {
	LimitID   string
	Revision  int64
	WMinutes  float64
	WSource   string
	SMinutes  float64
	SSource   string
	DecidedBy string
}

// RecordCapBasis writes the basis for one revision of a limit. The limit revision must be the one in the store; both sources are named; the minutes are positive numbers; and the caller is
// the task that declared the limit or the registered parent of its project. An identical record is a replay and a different one for the same key is refused: a basis is evidence, not a setting.
func (s *Scheduler) RecordCapBasis(ctx context.Context, in CapBasis) error {
	switch {
	case in.LimitID == "" || in.Revision < 1 || in.DecidedBy == "":
		return refuse(contract.RefusalMalformedReceipt, "a cap basis names the limit, its revision and who decided")
	case in.WSource == "" || in.SSource == "":
		return refuse(contract.RefusalMalformedReceipt, "a cap basis names where W and S came from")
	case !(in.WMinutes > 0) || !(in.SMinutes > 0) || math.IsInf(in.WMinutes, 0) || math.IsInf(in.SMinutes, 0):
		return refuse(contract.RefusalMalformedReceipt, "W and S are positive numbers of minutes")
	}
	return s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		var scopeKind, scopeKey, declaredBy, dimension string
		found, err := queryOne(txCtx, tx, "SELECT scope_kind, scope_key, declared_by, dimension FROM execution_limits WHERE limit_id = ? AND revision = ?", []any{in.LimitID, in.Revision}, &scopeKind, &scopeKey, &declaredBy, &dimension)
		if err != nil {
			return err
		}
		if !found {
			return refuse(contract.RefusalSlotUnknown, "limit %s has no revision %d in the store", in.LimitID, in.Revision)
		}
		if dimension != "runs" {
			return refuse(contract.RefusalMalformedReceipt, "limit %s is a limit on %s: a concurrency basis belongs to a runs limit", in.LimitID, dimension)
		}
		if in.DecidedBy != declaredBy {
			parents := []string(nil)
			if scopeKind == "project" {
				if parents, err = projectParents(txCtx, tx, scopeKey); err != nil {
					return err
				}
			}
			if len(parents) != 1 || parents[0] != in.DecidedBy {
				return refuse(contract.RefusalScopeRoleMismatch, "task %s neither declared limit %s nor is the registered parent of its scope", in.DecidedBy, in.LimitID)
			}
		}
		var w, sMin float64
		var wSource, sSource string
		recorded, err := queryOne(txCtx, tx, "SELECT w_minutes, w_source, s_minutes, s_source FROM dag_cap_basis WHERE limit_id = ? AND limit_revision = ?", []any{in.LimitID, in.Revision}, &w, &wSource, &sMin, &sSource)
		if err != nil {
			return err
		}
		if recorded {
			if w != in.WMinutes || sMin != in.SMinutes || wSource != in.WSource || sSource != in.SSource {
				return refuse(contract.RefusalDispositionConflict, "revision %d of limit %s has a recorded basis (W %.4g from %s, S %.4g from %s) and this one differs", in.Revision, in.LimitID, w, wSource, sMin, sSource)
			}
			return nil
		}
		_, err = tx.ExecContext(txCtx, "INSERT INTO dag_cap_basis (limit_id, limit_revision, w_minutes, w_source, s_minutes, s_source, decided_by, decided_at) VALUES (?,?,?,?,?,?,?,?)",
			in.LimitID, in.Revision, in.WMinutes, in.WSource, in.SMinutes, in.SSource, in.DecidedBy, s.now())
		return err
	})
}
