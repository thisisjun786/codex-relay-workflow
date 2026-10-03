package adapter

import "testing"

func Test28_BAD_17_PreStartGuardRefusalAndRearm(t *testing.T) {
	t.Parallel()
	refusal := map[string]any{"code": "recipient_paused", "message": "paused before the business turn"}
	s := sendScenario(resume(), []any{"guard", "send-recover-1", "thread-1", "hello", refusal}, []any{"guard", "send-recover-1", "thread-1", "hello", nil}, []any{"guard", "send-recover-1", "thread-1", "hello", refusal})
	first := append([]map[string]any{}, s.answers[:2]...)
	first = append(first, map[string]any{"goal": map[string]any{"status": "paused"}})
	s.answers = append(first, s.answers[0], s.answers[1], map[string]any{"goal": nil}, s.answers[2])
	capture(t, s)
}
