package managed

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// hostReady requires a complete archived scan; a listing miss uses thread/read for existence.
func hostReady(ctx context.Context, rpc HostRPC, task string) (string, error) {
	found, archived := false, false
	for _, filter := range []bool{true, false} {
		cursor := ""
		for range 4 {
			params := map[string]any{"limit": 50, "archived": filter, "useStateDbOnly": true}
			if cursor != "" {
				params["cursor"] = cursor
			}
			page, err := rpc.HostCall(ctx, "thread/list", params)
			if err != nil {
				return "", err
			}
			data, ok := page["data"].([]any)
			if filter && !ok {
				return "archived_listing_incomplete", nil
			}
			if ok {
				for _, item := range data {
					if pyjson.Map(item)["id"] == task {
						found, archived = true, filter
						break
					}
				}
			}
			if found {
				break
			}
			if next := page["nextCursor"]; filter && next != nil {
				if _, ok := next.(string); !ok {
					return "archived_listing_incomplete", nil
				}
			}
			cursor = pyjson.Text(page["nextCursor"])
			if cursor == "" {
				break
			}
		}
		if filter && !found && cursor != "" {
			return "archived_listing_incomplete", nil
		}
		if found {
			break
		}
	}
	if archived {
		return "recipient_archived", nil
	}
	var thread map[string]any
	var err error
	if !found {
		// Check existence before asking for a possibly unknown thread's goal. The
		// final send guard repeats this observation; this is not a reservation.
		thread, err = readHostThread(ctx, rpc, task)
		if err != nil {
			return "", err
		}
		if thread == nil {
			return "lifecycle_unknown", nil
		}
	}
	answer, err := rpc.HostCall(ctx, "thread/goal/get", map[string]any{"threadId": task})
	if err != nil {
		return "", err
	}
	if goal := answer["goal"]; goal != nil {
		data, ok := goal.(map[string]any)
		if !ok {
			return "lifecycle_unknown", nil
		}
		status, ok := data["status"].(string)
		if !ok {
			return "lifecycle_unknown", nil
		}
		switch status {
		case "paused":
			return "recipient_paused", nil
		case "usageLimited":
			return "recipient_usage_limited", nil
		case "budgetLimited":
			return "recipient_budget_limited", nil
		}
	}
	if found {
		thread, err = readHostThread(ctx, rpc, task)
		if err != nil {
			return "", err
		}
		if thread == nil {
			return "lifecycle_unknown", nil
		}
	}
	if thread["canAcceptDirectInput"] == false {
		return "recipient_cannot_accept_input", nil
	}
	switch pyjson.Map(thread["status"])["type"] {
	case "idle", "notLoaded":
		return "", nil
	default:
		return "recipient_not_idle", nil
	}
}

// A nil thread is the host's unknown-thread answer or an unreadable thread object.
func readHostThread(ctx context.Context, rpc HostRPC, task string) (map[string]any, error) {
	answer, err := rpc.HostCall(ctx, "thread/read", map[string]any{"threadId": task})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if unreadable(err) {
			return nil, nil
		}
		return nil, err
	}
	return pyjson.Map(answer["thread"]), nil
}
