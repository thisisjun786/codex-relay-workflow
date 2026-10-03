package store

import "context"

// The numbering of a relationship's generations after one was withdrawn (CRW-446). A withdrawn generation keeps its generations row and is recorded in dag_generation_withdrawals (the DAG zone), while
// relationships.execution_generation goes back to the generation the relationship stands on. So a number is never reused: the next generation takes the number after the highest the relationship ever held,
// and a generation's predecessor is the nearest one below it that was not withdrawn. A store that predates the zone has no withdrawal, and every answer below is then the plain n-1 and max+1.

func withdrawalsRecorded(ctx context.Context, q Querier) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'dag_generation_withdrawals'").Scan(&n)
	return n > 0, err
}

// NextGeneration is the number a relationship's next generation takes: one past the highest it ever held, withdrawn generations included.
func NextGeneration(ctx context.Context, q Querier, rid string) (int64, error) {
	var pointer, highest *int64
	if err := q.QueryRowContext(ctx, "SELECT (SELECT execution_generation FROM relationships WHERE relationship_id = ?), (SELECT MAX(execution_generation) FROM generations WHERE relationship_id = ?)", rid, rid).Scan(&pointer, &highest); err != nil {
		return 0, err
	}
	next := int64(1)
	for _, n := range []*int64{pointer, highest} {
		if n != nil && *n >= next {
			next = *n + 1
		}
	}
	return next, nil
}

// GenerationWithdrawn is whether the generation was withdrawn.
func GenerationWithdrawn(ctx context.Context, q Querier, rid string, number int64) (bool, error) {
	if has, err := withdrawalsRecorded(ctx, q); err != nil || !has {
		return false, err
	}
	var n int
	err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM dag_generation_withdrawals WHERE relationship_id = ? AND execution_generation = ?", rid, number).Scan(&n)
	return n > 0, err
}

// RefuseWithdrawn is the refusal every opener and binder of a generation gives for a withdrawn one (stale_generation): a withdrawn generation is final, so it is neither handed back by a repeated
// open nor bound to a turn. Callers ask it inside the transaction that writes.
func RefuseWithdrawn(ctx context.Context, q Querier, rid string, number int64) error {
	withdrawn, err := GenerationWithdrawn(ctx, q, rid, number)
	if err != nil || !withdrawn {
		return err
	}
	return refuse(ReasonStaleGeneration, "generation %d of %s was withdrawn: it was never sent to the child and is closed for good; open a new generation (it takes the next number)", number, rid)
}

// LiveGenerationBefore is the generation a generation follows: the nearest one below it that was not withdrawn. Without a withdrawal that is number-1, as every reader assumed before.
func LiveGenerationBefore(ctx context.Context, q Querier, rid string, number int64) (int64, error) {
	live := number - 1
	for live > 1 {
		withdrawn, err := GenerationWithdrawn(ctx, q, rid, live)
		if err != nil {
			return 0, err
		}
		if !withdrawn {
			break
		}
		live--
	}
	return live, nil
}
