package dagsched

import (
	"context"
	"database/sql"
	"errors"
	"math"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// StandingCap is the parent's standing child cap (crw-plan integrations.md, "6"): the DAG never runs more children than this unless a cap basis
// (dag_cap_basis, contract 7.3) documents a larger ceiling. D-05 is undecided: nothing here raises it.
const StandingCap = 6

// Capacity is what the slots allow now: the free count of the scope that binds, and where its ceiling came from.
type Capacity struct {
	Free, Ceiling, Held int
	Source              string // declared | clamped_no_basis | standing_cap
	Basis               string // recorded | missing for a declared limit above the standing cap, else ""
	Unmeasured          bool   // an enforced non-runs ceiling has no usage observation
}

// capacity reads the ceilings that apply to a project: the initiative above it, the project and the store, each as capacity.Reserve judges them,
// plus the standing cap at project scope. Free is the smallest headroom; a dimension nobody measured makes the reading unmeasured.
func (s *Scheduler) capacity(ctx context.Context, q store.Querier, project string) (Capacity, error) {
	type scope struct{ kind, key string }
	scopes := []scope{}
	if initiative, err := s.initiativeOf(ctx, q, project); err != nil {
		return Capacity{}, err
	} else if initiative != "" {
		scopes = append(scopes, scope{"initiative", initiative})
	}
	scopes = append(scopes, scope{"project", project}, scope{"store", "store"})
	best := Capacity{Free: math.MaxInt, Source: "standing_cap"}
	projectLimited := false
	consider := func(c Capacity) {
		if c.Free < best.Free {
			best = c
		}
	}
	for _, sc := range scopes {
		limits, err := s.Store.EnforcedExecutionLimits(ctx, sc.kind, sc.key)
		if err != nil {
			return Capacity{}, err
		}
		for _, l := range limits {
			if l.Dimension != "runs" {
				usage, err := s.Store.ExecutionUsage(ctx, sc.kind, sc.key, l.Dimension)
				switch {
				case errors.Is(err, sql.ErrNoRows):
					best.Unmeasured = true
					consider(Capacity{Free: 0, Ceiling: int(math.Ceil(l.Ceiling)), Source: "declared", Unmeasured: true})
				case err != nil:
					return Capacity{}, err
				case usage.Observed >= l.Ceiling:
					consider(Capacity{Free: 0, Ceiling: int(math.Ceil(l.Ceiling)), Source: "declared"})
				}
				continue
			}
			// capacity.Reserve refuses when held >= ceiling, so a ceiling of 0.5 still allows one reservation: the slots are the ceiling rounded up.
			limit := l.Ceiling
			source, basis := "declared", ""
			if limit > StandingCap {
				var one int
				err := q.QueryRowContext(ctx, "SELECT 1 FROM dag_cap_basis WHERE limit_id = ? AND limit_revision = ?", l.LimitID, l.Revision).Scan(&one)
				switch {
				case err == nil:
					basis = "recorded"
				case errors.Is(err, sql.ErrNoRows):
					limit, source, basis = StandingCap, "clamped_no_basis", "missing"
				default:
					return Capacity{}, err
				}
			}
			held, err := s.Store.RunsIn(ctx, sc.kind, sc.key, "held")
			if err != nil {
				return Capacity{}, err
			}
			if sc.kind == "project" {
				projectLimited = true
			}
			ceiling := int(math.Ceil(limit))
			consider(Capacity{Free: max(ceiling-int(held), 0), Ceiling: ceiling, Held: int(held), Source: source, Basis: basis})
		}
	}
	if !projectLimited {
		held, err := s.Store.RunsIn(ctx, "project", project, "held")
		if err != nil {
			return Capacity{}, err
		}
		consider(Capacity{Free: max(StandingCap-int(held), 0), Ceiling: StandingCap, Held: int(held), Source: "standing_cap"})
	}
	unmeasured := best.Unmeasured
	if best.Free == math.MaxInt {
		best = Capacity{Source: "standing_cap"}
	}
	best.Unmeasured = unmeasured
	return best, nil
}

// initiativeOf is the initiative above a project's single registered parent, the way capacity.Reserve finds the scope it also counts against.
func (s *Scheduler) initiativeOf(ctx context.Context, q store.Querier, project string) (string, error) {
	parents, err := projectParents(ctx, q, project)
	if err != nil || len(parents) != 1 {
		return "", err
	}
	result := (&registry.Registry{Store: s.Store}).Up(ctx, registry.UpSelector{Task: sql.NullString{String: parents[0], Valid: true}, Scope: sql.NullString{String: project, Valid: true}})
	if orderedString(result, "state") != "resolved" {
		return "", nil
	}
	for _, f := range result {
		if f.Key != "levels" {
			continue
		}
		levels, _ := f.Value.([]any)
		for _, item := range levels {
			if entry, ok := item.(contract.OrderedObject); ok && orderedString(entry, "scopeKind") == "initiative" {
				return orderedString(entry, "scopeKey"), nil
			}
		}
	}
	return "", nil
}

// projectParents are the live parent bindings of a project scope.
func projectParents(ctx context.Context, q store.Querier, project string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT task_id FROM scope_bindings WHERE role = 'parent' AND scope_kind = 'project' AND scope_key = ? AND status = 'active' AND superseded_by IS NULL ORDER BY task_id", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
