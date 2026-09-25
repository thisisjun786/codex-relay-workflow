package bridge

import (
	"context"
	"fmt"
	"time"
)

const DetailTurns = 1
const ItemPage = 10

func clipped(value any, limit int, display bool) any {
	switch v := value.(type) {
	case string:
		if display && len([]rune(v)) > limit {
			return string([]rune(v)[:limit]) + fmt.Sprintf("\n[truncated; original length %d characters]", len([]rune(v)))
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = clipped(item, limit, display)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = clipped(item, limit, k == "text" || k == "preview" || k == "summary" || k == "objective" || k == "aggregatedOutput")
		}
		return out
	}
	return value
}
func (b *Bridge) GetGoal(ctx context.Context, threadID string) (map[string]any, error) {
	if err := nonempty(threadID, "thread_id", 128); err != nil {
		return nil, err
	}
	result, err := b.call(ctx, "thread/goal/get", map[string]any{"threadId": threadID})
	if err != nil {
		return nil, err
	}
	return clipped(result, 4000, false).(map[string]any), nil
}
func (b *Bridge) ListThreads(ctx context.Context, cwd string, limit int, cursor any) (map[string]any, error) {
	if limit < 1 || limit > 100 {
		return nil, &Invalid{"limit must be between 1 and 100"}
	}
	params := map[string]any{"limit": limit, "useStateDbOnly": true}
	if cwd != "" {
		resolved, err := directory(cwd)
		if err != nil {
			return nil, err
		}
		params["cwd"] = resolved
	}
	if cursor != nil {
		params["cursor"] = cursor
	}
	response, err := b.call(ctx, "thread/list", params)
	if err != nil {
		return nil, err
	}
	return clipped(response, 4000, false).(map[string]any), nil
}
func (b *Bridge) ActiveTurn(ctx context.Context, threadID string) (map[string]any, error) {
	if err := nonempty(threadID, "thread_id", 128); err != nil {
		return nil, err
	}
	metadata, err := b.call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false})
	if err != nil {
		return nil, err
	}
	page, err := b.call(ctx, "thread/turns/list", map[string]any{"threadId": threadID, "limit": 1, "itemsView": "summary"})
	if err != nil {
		return nil, err
	}
	status := object(object(metadata["thread"])["status"])
	turns, _ := page["data"].([]any)
	newest := map[string]any{}
	if len(turns) > 0 {
		newest = object(turns[0])
	}
	running := newest["status"] == "inProgress"
	kind := text(status["type"])
	observation := kind
	if kind == "active" && running {
		observation = "active"
	} else if kind == "active" {
		observation = "active_without_in_progress_turn"
	} else if running {
		observation = "in_progress_turn_without_active_status"
	} else if kind == "" {
		observation = "unknown"
	}
	var active, newestID any
	if len(newest) > 0 {
		newestID = newest["id"]
	}
	if running {
		active = newestID
	}
	flags := status["activeFlags"]
	if flags == nil {
		flags = []any{}
	}
	return map[string]any{"threadId": threadID, "status": status, "activeFlags": flags, "activeTurnId": active, "newestTurnId": newestID, "observation": observation, "derivation": "newest turn reported inProgress; the status carries no turn id", "steerable": observation == "active", "snapshot": "observed now; the turn may change before any steer is sent"}, nil
}
func (b *Bridge) WaitThread(ctx context.Context, threadID, turnID string, timeout time.Duration) (map[string]any, error) {
	if err := nonempty(threadID, "thread_id", 128); err != nil {
		return nil, err
	}
	if err := nonempty(turnID, "turn_id", 128); err != nil {
		return nil, err
	}
	if timeout < 0 || timeout > 50*time.Second {
		return nil, &Invalid{"timeout_seconds must be between 0 and 50"}
	}
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var latest map[string]any
	for {
		page, err := b.call(ctx, "thread/turns/list", map[string]any{"threadId": threadID, "limit": 100, "itemsView": "summary"})
		if err != nil {
			return nil, err
		}
		data, _ := page["data"].([]any)
		for _, item := range data {
			turn := object(item)
			if turn["id"] == turnID {
				latest = turn
				break
			}
		}
		terminal := latest != nil && (latest["status"] == "completed" || latest["status"] == "failed" || latest["status"] == "interrupted")
		if terminal || timeout == 0 {
			return map[string]any{"threadId": threadID, "turnId": turnID, "timedOut": !terminal, "turn": optionalTurn(latest), "observation": observation(latest)}, nil
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-wait.Done():
			timer.Stop()
			return map[string]any{"threadId": threadID, "turnId": turnID, "timedOut": true, "turn": optionalTurn(latest), "observation": observation(latest)}, nil
		case <-timer.C:
		}
	}
}
func optionalTurn(turn map[string]any) any {
	if turn == nil {
		return nil
	}
	return clipped(turn, 4000, false)
}
func observation(turn map[string]any) string {
	if turn == nil {
		return "not observed in latest 100 turns"
	}
	return "found"
}
