package managed

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// EnsureSettings is registry.record_settings(..., ensure_only=True)'s write guard.
// The existing source and value are returned without a write when equal.
func EnsureSettings(ctx context.Context, s *store.Store, task string, settings map[string]any, source, at string) (string, error) {
	result := source
	err := s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var existing, retained string
		err := s.Querier(ctx).QueryRowContext(ctx, "SELECT settings, source FROM authorized_settings WHERE task_id=?", task).Scan(&existing, &retained)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		encoded, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		if existing != "" {
			var previous any
			if err := json.Unmarshal([]byte(existing), &previous); err != nil {
				return err
			}
			var next any
			if err := json.Unmarshal(encoded, &next); err != nil {
				return err
			}
			if !jsonSame(previous, next) {
				return refusal("relationship_conflict", fmt.Sprintf("'%s' already has execution settings that differ from this record; ensure_only does not overwrite them", task))
			}
			result = retained
			return nil
		}
		pythonJSON, err := settingsJSON(settings)
		if err != nil {
			return err
		}
		_, err = s.Querier(ctx).ExecContext(ctx, "INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)", task, pythonJSON, source, at)
		return err
	})
	return result, err
}

// settingsJSON is Python json.dumps(sort_keys=True) for the stored settings text, every scalar as
// encoding/json's json.Marshal writes it (pyjson Marshal): the stored authorized_settings bytes.
func settingsJSON(v any) (string, error) {
	data, err := pyjson.Encode(v, pyjson.Options{SortKeys: true, Marshal: true})
	return string(data), err
}
