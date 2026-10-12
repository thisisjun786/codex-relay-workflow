package ops

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// ErrStepConflict is the answer of AppendStep to a step id that is stored with another decision: the same id is one decision.
var ErrStepConflict = errors.New("a step decision with this id is stored with another checkpoint or other evidence")

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

// AppendStep implements Decisions: the same decision again is stored once, another decision under the same id is ErrStepConflict.
func (m *MemoryDecisions) AppendStep(_ context.Context, row StepRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.Step {
		if r.StepID == row.StepID {
			if !sameStep(r, row) {
				return ErrStepConflict
			}
			return nil
		}
	}
	m.Step = append(m.Step, row)
	return nil
}

// sameStep says whether two rows of one step id are one decision: the node and tiers, the checkpoint and the evidence. When the
// decision was made is not part of it.
func sameStep(a, b StepRow) bool {
	return a.NodeID == b.NodeID && a.FromTier == b.FromTier && a.ToTier == b.ToTier && a.CheckpointHead == b.CheckpointHead &&
		a.CheckpointTree == b.CheckpointTree && digestOf(a.Evidence) == digestOf(b.Evidence)
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

// AppendStep implements Decisions: a decision that is stored is not stored again, and another decision under a stored id is
// ErrStepConflict (the table is append-only, so the lane records the other decision under an id of its own).
func (s StoreDecisions) AppendStep(ctx context.Context, row StepRow) error {
	var node, from, to, head, tree, evidenceJSON string
	err := s.DB.QueryRowContext(ctx, `SELECT node_id, from_tier, to_tier, checkpoint_head, checkpoint_tree, evidence_json FROM flow_step_decisions WHERE step_id = ?`, row.StepID).
		Scan(&node, &from, &to, &head, &tree, &evidenceJSON)
	if err == nil {
		var stored map[string]any
		_ = json.Unmarshal([]byte(evidenceJSON), &stored)
		if sameStep(StepRow{NodeID: node, FromTier: from, ToTier: to, CheckpointHead: head, CheckpointTree: tree, Evidence: stored}, row) {
			return nil
		}
		return ErrStepConflict
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
