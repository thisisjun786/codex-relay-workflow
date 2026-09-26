package managed

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

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

// settingsJSON is Python json.dumps(sort_keys=True) for the stored settings text.
func settingsJSON(v any) (string, error) {
	var b strings.Builder
	var write func(any) error
	write = func(value any) error {
		switch x := value.(type) {
		case map[string]any:
			b.WriteByte('{')
			keys := make([]string, 0, len(x))
			for key := range x {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for i, key := range keys {
				if i > 0 {
					b.WriteString(", ")
				}
				k, _ := json.Marshal(key)
				b.Write(k)
				b.WriteString(": ")
				if err := write(x[key]); err != nil {
					return err
				}
			}
			b.WriteByte('}')
		case []string:
			b.WriteByte('[')
			for i, item := range x {
				if i > 0 {
					b.WriteString(", ")
				}
				if err := write(item); err != nil {
					return err
				}
			}
			b.WriteByte(']')
		case []any:
			b.WriteByte('[')
			for i, item := range x {
				if i > 0 {
					b.WriteString(", ")
				}
				if err := write(item); err != nil {
					return err
				}
			}
			b.WriteByte(']')
		default:
			raw, err := json.Marshal(x)
			if err != nil {
				return err
			}
			b.Write(raw)
		}
		return nil
	}
	if err := write(v); err != nil {
		return "", err
	}
	return b.String(), nil
}
