package adapter

import (
	"encoding/json"
	"fmt"
	"testing"
)

func Test28_CursorValuesMatchTheGolden(t *testing.T) {
	shareGoldens(t)
	for i, cursor := range []any{7, true, 0, false, nil, "", []any{}, map[string]any{}, []any{7}, map[string]any{"next": 7}, "next", json.Number("1e-999"), json.Number("7.5")} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			first := page(item("first", "i", "noise"))
			first["nextCursor"] = cursor
			capture(t, scenario{page: 1, answers: []map[string]any{first, page(item("second", "j", "needle"))}, actions: [][]any{{"find", "01child-task", "needle", 2}}})
			more := page(map[string]any{"id": "other"})
			more["nextCursor"] = cursor
			capture(t, scenario{answers: []map[string]any{more, more, more, more}, actions: [][]any{{"turn", "01child-task", "wanted"}}})
			// All opaque types pass through unchanged before a successful discovery.
			capture(t, scenario{store: true, answers: []map[string]any{more, page(map[string]any{"id": "01child-task"}), page(), page(), page()}, actions: [][]any{{"archive", "01child-task", nil}}})
		})
	}
	// Persist a bounded numeric scan, then resume from SQLite's TEXT cursor.
	for _, cursor := range []any{7, true, json.Number("7.5"), json.Number("1e+30"), []any{7}, map[string]any{"next": 7}, json.Number("999999999999999999999999")} {
		more := page(map[string]any{"id": "other"})
		more["nextCursor"] = cursor
		capture(t, scenario{store: true, answers: []map[string]any{more, more, more, more, page(), page(), page(), page(map[string]any{"id": "01child-task"})}, actions: [][]any{{"archive", "01child-task", nil}, {"archive", "01child-task", nil}}})
	}
}
