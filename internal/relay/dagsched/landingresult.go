package dagsched

import (
	"context"
	"database/sql"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// What the parent records about a node's work after the fact (CRW-411), because the store reads no forge for it: whether dev was green or red after the node's pull request landed, whether
// the landing was reverted, and whether the work was a duplicate of another or discarded. The relay keeps the statement and its evidence; it checks none of it. A reverted row changes
// nothing the scheduler calls landed (the observation of containment still stands); it is a record for the release policy and the measurements.
const (
	ResultDevGreen  = "dev_green"
	ResultDevRed    = "dev_red"
	ResultReverted  = "reverted"
	ResultDuplicate = "duplicate"
	ResultDiscarded = "discarded"
)

// ResultKinds are the kinds a result may have, the closed set the table checks.
var ResultKinds = []string{ResultDevGreen, ResultDevRed, ResultReverted, ResultDuplicate, ResultDiscarded}

// MaxResultEvidenceBytes bounds the evidence text of a result.
const MaxResultEvidenceBytes = 1000

var resultCommitPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// ResultInput is what dag-landing-result-record is given about a node: the kind, the commit the statement is about (optional) and the evidence (required: the run, the pull request or the reason).
type ResultInput struct{ Kind, Commit, Evidence string }

// ResultRecord is the answer of RecordLandingResult.
type ResultRecord struct {
	ResultID, PlanID, NodeID, Kind, Commit, Evidence string
	Replayed                                         bool
}

func (in ResultInput) validate() error {
	known := false
	for _, kind := range ResultKinds {
		known = known || in.Kind == kind
	}
	switch {
	case !known:
		return refuse(contract.RefusalMalformedReceipt, "a result is %s, not %q", strings.Join(ResultKinds, ", "), in.Kind)
	case in.Commit != "" && !resultCommitPattern.MatchString(in.Commit):
		return refuse(contract.RefusalMalformedReceipt, "the commit a result is about is 7 to 64 lower-case hex digits, not %q", in.Commit)
	case strings.TrimSpace(in.Evidence) == "":
		return refuse(contract.RefusalMalformedReceipt, "a result names its evidence (the run, the pull request or the reason): the relay keeps the statement and reads no forge")
	case len(in.Evidence) > MaxResultEvidenceBytes || hasControl(in.Evidence):
		return refuse(contract.RefusalMalformedReceipt, "the evidence of a result is at most %d bytes and has no control character", MaxResultEvidenceBytes)
	}
	return nil
}

// RecordLandingResult records what the parent states about the work of an implementation node of the plan (dag-landing-result-record). The actor is the registered parent of the plan's project and
// the session holds the plan's epoch. The id is a digest of the plan, the node, the kind, the commit and the evidence, so the same statement again is a replay; a node may carry several kinds.
func (s *Scheduler) RecordLandingResult(ctx context.Context, plan, node, actor string, in ResultInput) (ResultRecord, error) {
	if err := in.validate(); err != nil {
		return ResultRecord{}, err
	}
	out := ResultRecord{PlanID: plan, NodeID: node, Kind: in.Kind, Commit: in.Commit, Evidence: in.Evidence}
	out.ResultID = digestOf(map[string]any{"plan_id": plan, "node_id": node, "kind": in.Kind, "commit": in.Commit, "evidence": in.Evidence})
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		if err := s.fence(txCtx, q, plan, actor); err != nil {
			return err
		}
		snap, _, err := dag.SnapshotAt(txCtx, q, plan, 0)
		if err != nil {
			return err
		}
		if err := s.requireParent(txCtx, q, snap, actor); err != nil {
			return err
		}
		n, ok := nodeOf(snap, node)
		switch {
		case !ok:
			return refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
		case n.Kind != dag.NodeImplementation:
			return refuse(contract.RefusalDispositionConflict, "node %s is a %s node: it has no pull request to state a result about", node, n.Kind)
		}
		var one int
		found, err := queryOne(txCtx, q, "SELECT 1 FROM dag_landing_results WHERE result_id = ?", []any{out.ResultID}, &one)
		if err != nil {
			return err
		}
		if found {
			out.Replayed = true
			return nil
		}
		_, err = q.ExecContext(txCtx, "INSERT INTO dag_landing_results (result_id, plan_id, node_id, kind, commit_sha, evidence, recorded_by, recorded_at) VALUES (?,?,?,?,?,?,?,?)",
			out.ResultID, plan, node, in.Kind, in.Commit, in.Evidence, actor, s.now())
		return err
	})
	return out, err
}

// nodeResults are the kinds recorded for a node of a plan.
func nodeResults(ctx context.Context, q store.Querier, plan, node string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT DISTINCT kind FROM dag_landing_results WHERE plan_id = ? AND node_id = ?", plan, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			return nil, err
		}
		out[kind] = true
	}
	return out, rows.Err()
}
