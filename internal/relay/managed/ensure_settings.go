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
		if existing != "" {
			same, err := sameSettings(existing, settings)
			if err != nil {
				return err
			}
			if !same {
				return settingsConflict(task)
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

// settingsConflict is the refusal for a task whose recorded execution settings differ from the ones offered.
func settingsConflict(task string) error {
	return refusal("relationship_conflict", fmt.Sprintf("'%s' already has execution settings that differ from this record; ensure_only does not overwrite them", task))
}

// sameSettings is whether the recorded settings text and the offered settings are the same JSON value.
func sameSettings(recorded string, settings map[string]any) (bool, error) {
	encoded, err := json.Marshal(settings)
	if err != nil {
		return false, err
	}
	var previous, next any
	if err := json.Unmarshal([]byte(recorded), &previous); err != nil {
		return false, err
	}
	if err := json.Unmarshal(encoded, &next); err != nil {
		return false, err
	}
	return jsonSame(previous, next), nil
}

// SettingsConflict is the refusal EnsureSettings would give for these settings, asked without writing:
// nil when the task has no recorded settings or they are the same value. It reads the store once and
// does not hold a transaction, so it decides what the store says now; EnsureSettings stays the write
// that decides.
func SettingsConflict(ctx context.Context, s *store.Store, task string, settings map[string]any) error {
	var recorded string
	err := s.Querier(ctx).QueryRowContext(ctx, "SELECT settings FROM authorized_settings WHERE task_id=?", task).Scan(&recorded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if recorded == "" {
		return nil
	}
	same, err := sameSettings(recorded, settings)
	if err != nil {
		return err
	}
	if !same {
		return settingsConflict(task)
	}
	return nil
}

// settingsJSON is Python json.dumps(sort_keys=True) for the stored settings text, every scalar as
// encoding/json's json.Marshal writes it (pyjson Marshal): the stored authorized_settings bytes.
func settingsJSON(v any) (string, error) {
	data, err := pyjson.Encode(v, pyjson.Options{SortKeys: true, Marshal: true})
	return string(data), err
}
