package adapter

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func Test28_HostShapesLivePython(t *testing.T) {
	for _, value := range []any{nil, 5, "bad", false, []any{}} {
		for _, action := range [][]any{{"thread", "thread-1"}, {"turn", "thread-1", "wanted"}, {"archive", "thread-1", nil}} {
			capture(t, scenario{answers: []map[string]any{{"rawResponse": value}}, actions: [][]any{action}})
		}
		s := sendScenario(resume(), []any{"send", "raw-shape", "thread-1", "hi"})
		s.answers[0] = map[string]any{"rawResponse": value}
		capture(t, s)
	}
	for i, value := range []any{nil, false, 0, "", []any{}, "bad", 5, true, []any{1}} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			capture(t, scenario{answers: []map[string]any{{"thread": value}}, actions: [][]any{{"thread", "thread-1"}}})
			capture(t, scenario{answers: []map[string]any{{"thread": map[string]any{"status": value}}}, actions: [][]any{{"thread", "thread-1"}}})
			s := sendScenario(resume(), []any{"send", "shape-send", "thread-1", "hi"})
			s.answers[0] = map[string]any{"thread": value}
			capture(t, s)
			s.answers[0] = map[string]any{"thread": map[string]any{"status": value}}
			capture(t, s)
		})
	}
	for _, value := range []any{5, "no", nil, false, []any{1}, map[string]any{"x": 1}} {
		capture(t, scenario{answers: []map[string]any{{"thread": map[string]any{"status": map[string]any{"type": value}, "canAcceptDirectInput": value}}}, actions: [][]any{{"thread", "thread-1"}}})
		s := sendScenario(resume(), []any{"send", "shape-turn", "thread-1", "hi"})
		s.answers[2] = map[string]any{"turn": map[string]any{"id": value}}
		capture(t, s)
	}
	for _, value := range []any{nil, false, 5, "bad", "", map[string]any{}, map[string]any{"a": 1}, []any{nil}, []any{5}} {
		capture(t, scenario{answers: []map[string]any{{"data": value}}, actions: [][]any{{"turn", "thread-1", "wanted"}}})
	}
	for _, answer := range []map[string]any{{}, {"turn": nil}, {"turn": "bad"}, {"turn": []any{}}, {"turn": map[string]any{}}} {
		s := sendScenario(resume(), []any{"send", "shape-missing", "thread-1", "hi"})
		s.answers[2] = answer
		capture(t, s)
	}
}

// Recheck28's raw answers must reach the operation-specific Python boundary:
// resume verifies every shape, while turn/start indexes result["turn"]["id"].
func Test28_SendResponseShapesLivePython(t *testing.T) {
	values := []any{json.Number("3.5"), []any{map[string]any{"a": 1}}, "x", true, json.Number("0"), []any{}, map[string]any{}, nil}
	for i, value := range values {
		for slot := 0; slot < 3; slot++ {
			t.Run(fmt.Sprintf("shape%d/slot%d", i, slot), func(t *testing.T) {
				s := sendScenario(resume(), []any{"send", "raw-response", "thread-1", "hi"}, []any{"operation", "raw-response"})
				s.answers[slot] = map[string]any{"rawResponse": value}
				capture(t, s)
				if slot == 1 {
					s.settingsFree = true
					capture(t, s)
				}
			})
		}
	}
}

func Test28_LifecycleIntStringsLivePython(t *testing.T) {
	for i, value := range []string{" 7\n", "5_0", "\u0665", "+3", "1.5", "0x10", "99999999999999999999", "-99999999999999999999", "9223372036854775807", "-9223372036854775808", "9223372036854775808", "-9223372036854775809", "", " ", "+", "-", "_5", "5_", "5__0", "+_5", "-\u0665_\uff10", "\u0085+\u0665\u2028", "\x1c7\x1f", "\u00b2", "\U0001d7cf", "1 2", "\u200b5", strings.Repeat("9", 4300), strings.Repeat("1", 4300), strings.Repeat("9", 4301)} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			capture(t, scenario{store: true, answers: []map[string]any{{"thread": map[string]any{"status": map[string]any{"type": "idle"}, "canAcceptDirectInput": value}}}, actions: [][]any{{"lifecycle-record", "thread-1"}}})
		})
	}
}

func Test28_HostShapeLifecyclePersistenceLivePython(t *testing.T) {
	for _, value := range []any{nil, false, true, 0, 5, "5", "no", []any{}, map[string]any{}} {
		capture(t, scenario{store: true, answers: []map[string]any{{"thread": map[string]any{"status": map[string]any{"type": 5}, "canAcceptDirectInput": value}}}, actions: [][]any{{"lifecycle-record", "thread-1"}}})
	}
}

func Test28_ROL_11_SettingsDifferAfterLoad(t *testing.T) {
	r := resume()
	r["model"] = "other-model"
	s := sendScenario(r, []any{"send", "settings-free-mismatch", "thread-1", "hi"})
	s.settingsFree = true
	s.answers[0] = map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}}}
	capture(t, s)
}
