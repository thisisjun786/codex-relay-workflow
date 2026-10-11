package store

import (
	"context"
	"database/sql"
	"errors"
)

// AgentIntegrationStageRow is one dag_agent_integration_stages row (CRW-1213, R05): a node-level fact of the integration of an agent node's acceptance. The batch freezes the acceptance in an
// intent row before it touches git, the merged mark is a marked row (the fact the relationship kind keeps as the parent's assignment mark), and a mark that could not be written is a
// mark_pending row the next run completes. The batch-level rows (the batch intent, ref_moved) carry no node and no acceptance, so they are written to dag_integration_stages for every batch and
// are not rows of this table.
type AgentIntegrationStageRow struct {
	StageID      string
	BatchID      string
	PlanID       string
	NodeID       string
	AcceptanceID string
	ExecutionID  string
	Stage        string
	Head         string
	Detail       string
	RecordedBy   string
	RecordedAt   string
}

const agentIntegrationStageColumns = "stage_id, batch_id, plan_id, node_id, acceptance_id, execution_id, stage, head, detail, recorded_by, recorded_at"

func scanAgentIntegrationStage(row scanner) (AgentIntegrationStageRow, error) {
	var r AgentIntegrationStageRow
	err := row.Scan(&r.StageID, &r.BatchID, &r.PlanID, &r.NodeID, &r.AcceptanceID, &r.ExecutionID, &r.Stage, &r.Head, &r.Detail, &r.RecordedBy, &r.RecordedAt)
	return r, err
}

// RecordAgentIntegrationStage appends one node-level stage row of an agent acceptance through the caller's transaction. A row that is already stored under the same stage id is the same fact
// written again by a repeated run: it is left as it is and answers false, so a retry of a batch never reaches the key guard of the table.
func RecordAgentIntegrationStage(ctx context.Context, s *Store, r AgentIntegrationStageRow) (bool, error) {
	var stored string
	err := s.q(ctx).QueryRowContext(ctx, "SELECT stage_id FROM dag_agent_integration_stages WHERE stage_id = ?", r.StageID).Scan(&stored)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = s.exec(ctx, "INSERT INTO dag_agent_integration_stages ("+agentIntegrationStageColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		r.StageID, r.BatchID, r.PlanID, r.NodeID, r.AcceptanceID, r.ExecutionID, r.Stage, r.Head, r.Detail, r.RecordedBy, r.RecordedAt)
	return err == nil, err
}

// AgentIntegrationStagesOfPlan reads every node-level stage row of an agent acceptance of a plan, in the order they were written. A store that predates the table holds none.
func AgentIntegrationStagesOfPlan(ctx context.Context, s *Store, plan string) ([]AgentIntegrationStageRow, error) {
	present, err := dagZoneTable(ctx, s, "dag_agent_integration_stages")
	if err != nil || !present {
		return nil, err
	}
	return queryRows(ctx, s, scanAgentIntegrationStage, "SELECT "+agentIntegrationStageColumns+" FROM dag_agent_integration_stages WHERE plan_id = ? ORDER BY rowid", plan)
}
