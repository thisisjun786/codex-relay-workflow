package cli

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// designHistory is crw.designReuse's source search (CRW-1329): the runs of the same plan that the same repository's runs
// recorded, newest first, each with its journal's events or the reason it is not a source (unreadable spec or journal, a spec of
// another repository). The query names the run ids only; the journals are read one by one after it is closed.
func designHistory(ctx context.Context, st *store.Store, state, repo, currentRun, plan string) ([]ops.DesignRun, error) {
	rows, err := st.Q(ctx).QueryContext(ctx, `SELECT run_id FROM flow_runs WHERE plan_id = ? AND run_id <> ? AND workflow = 'project-lane' ORDER BY started_at DESC, rowid DESC`, plan, currentRun)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]ops.DesignRun, 0, len(ids))
	for _, id := range ids {
		spec, err := ReadSpec(state, id)
		if err != nil {
			out = append(out, ops.DesignRun{Run: id, Reason: ops.ReuseSourceUnreadable, Detail: err.Error()})
			continue
		}
		if spec.Repo == "" || !sameDirectory(spec.Repo, repo) {
			out = append(out, ops.DesignRun{Run: id, Reason: ops.ReuseRepoMismatch, Detail: "the run's repository is " + spec.Repo})
			continue
		}
		events, hasRun, err := journal.Load(ctx, st, id)
		if err != nil {
			return nil, err
		}
		if !hasRun {
			out = append(out, ops.DesignRun{Run: id, Reason: ops.ReuseSourceUnreadable, Detail: "the run has no journal"})
			continue
		}
		out = append(out, ops.DesignRun{Run: id, Events: events})
	}
	return out, nil
}
