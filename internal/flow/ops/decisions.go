package ops

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// MemoryDecisions keeps the decision rows in memory, once for each id.
type MemoryDecisions struct {
	mu    sync.Mutex
	Spawn []SpawnRow
	Step  []StepRow
}

// AppendSpawn implements Decisions.
func (m *MemoryDecisions) AppendSpawn(_ context.Context, row SpawnRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.Spawn {
		if r.DecisionID == row.DecisionID {
			return nil
		}
	}
	m.Spawn = append(m.Spawn, row)
	return nil
}

// AppendStep implements Decisions.
func (m *MemoryDecisions) AppendStep(_ context.Context, row StepRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.Step {
		if r.StepID == row.StepID {
			return nil
		}
	}
	m.Step = append(m.Step, row)
	return nil
}

// StoreDecisions appends the rows to flow_spawn_decisions and flow_step_decisions of the relay store. The run's row in flow_runs
// has to exist (the journal writes it with the run's first row).
type StoreDecisions struct {
	DB SQL
}

// AppendSpawn implements Decisions.
func (s StoreDecisions) AppendSpawn(ctx context.Context, row SpawnRow) error {
	_, err := StoreSpawnRows(ctx, s.DB, []SpawnRow{row})
	return err
}

// AppendStep implements Decisions: a step id that is stored is not stored again.
func (s StoreDecisions) AppendStep(ctx context.Context, row StepRow) error {
	var one int
	err := s.DB.QueryRowContext(ctx, `SELECT 1 FROM flow_step_decisions WHERE step_id = ?`, row.StepID).Scan(&one)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	evidence, err := json.Marshal(row.Evidence)
	if err != nil {
		return err
	}
	at := row.DecidedAt
	if at.IsZero() {
		at = time.Now()
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO flow_step_decisions (step_id, run_id, node_id, from_tier, to_tier, rule_id, evidence_json, checkpoint_head, checkpoint_tree, decided_at)
 VALUES (?,?,?,?,?,?,?,?,?,?)`, row.StepID, row.RunID, row.NodeID, row.FromTier, row.ToTier, row.RuleID, string(evidence), row.CheckpointHead, row.CheckpointTree,
		at.UTC().Format(time.RFC3339Nano))
	return err
}
