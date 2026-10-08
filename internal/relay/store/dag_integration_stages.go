package store

import "context"

// IntegrationStageRow is one dag_integration_stages row (CRW-965). A batch writes its stages append-only: the
// intent (the frozen candidates, the base and the old head, written before any git call), ref_moved (the branch
// moved to the verified commit), marked (a node's merged mark was written from its frozen acceptance) and
// mark_pending (a mark that could not be written, completed later by the next run). Batch-level stages carry an
// empty node and acceptance.
type IntegrationStageRow struct {
	StageID      string
	BatchID      string
	PlanID       string
	Stage        string
	NodeID       string
	AcceptanceID string
	EventID      string
	RevisionHash string
	Generation   int64
	HeadSHA      string
	Detail       string
	RecordedBy   string
	RecordedAt   string
}

const integrationStageColumns = "stage_id, batch_id, plan_id, stage, node_id, acceptance_id, event_id, revision_hash, generation, head_sha, detail, recorded_by, recorded_at"

func scanIntegrationStage(row scanner) (IntegrationStageRow, error) {
	var r IntegrationStageRow
	err := row.Scan(&r.StageID, &r.BatchID, &r.PlanID, &r.Stage, &r.NodeID, &r.AcceptanceID, &r.EventID, &r.RevisionHash, &r.Generation, &r.HeadSHA, &r.Detail, &r.RecordedBy, &r.RecordedAt)
	return r, err
}

// RecordIntegrationStage appends one integration stage row.
func RecordIntegrationStage(ctx context.Context, s *Store, r IntegrationStageRow) error {
	_, err := s.exec(ctx, "INSERT INTO dag_integration_stages ("+integrationStageColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
		r.StageID, r.BatchID, r.PlanID, r.Stage, r.NodeID, r.AcceptanceID, r.EventID, r.RevisionHash, r.Generation, r.HeadSHA, r.Detail, r.RecordedBy, r.RecordedAt)
	return err
}

// IntegrationStagesOfPlan reads every stage row of a plan in the order they were written. A store that predates
// the table holds none.
func IntegrationStagesOfPlan(ctx context.Context, s *Store, plan string) ([]IntegrationStageRow, error) {
	present, err := dagZoneTable(ctx, s, "dag_integration_stages")
	if err != nil || !present {
		return nil, err
	}
	return queryRows(ctx, s, scanIntegrationStage, "SELECT "+integrationStageColumns+" FROM dag_integration_stages WHERE plan_id = ? ORDER BY rowid", plan)
}
