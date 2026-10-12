package manage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// supervisorSettingsAt reads, from the relay store below state, the settings a delivery to the supervisor of a thread carries: the
// model and the reasoning effort of the authorization recorded for the task of its live supervision binding. The store is opened
// read-only; crw manage config has no supervisor block (it holds the management and parent blocks), so the binding is the one source.
func supervisorSettingsAt(ctx context.Context, state, task string) (coreSettings, error) {
	handle, err := relayReadOpenStore(ctx, state)
	if err != nil {
		return coreSettings{}, err
	}
	defer handle.Close()
	var settings coreSettings
	err = handle.ReadSnapshot(ctx, func(ctx context.Context, st *store.Store) error {
		q := st.Q(ctx)
		var bindings int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM scope_bindings WHERE task_id = ? AND role = 'supervisor' AND status = 'active'`, task).Scan(&bindings); err != nil {
			return err
		}
		if bindings == 0 {
			return fmt.Errorf("task %s holds no live supervisor binding", task)
		}
		var raw string
		if err := q.QueryRowContext(ctx, `SELECT settings FROM authorized_settings WHERE task_id = ?`, task).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no authorized settings are recorded for task %s", task)
			}
			return err
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			return fmt.Errorf("the recorded settings of %s are not JSON: %w", task, err)
		}
		model, _ := doc["model"].(string)
		effort, _ := doc["reasoningEffort"].(string)
		if strings.TrimSpace(model) == "" || strings.TrimSpace(effort) == "" {
			return fmt.Errorf("the recorded settings of %s name no model or no reasoning effort", task)
		}
		settings = coreSettings{Model: model, ReasoningEffort: effort}
		return nil
	})
	return settings, err
}
