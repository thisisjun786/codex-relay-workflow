package managed

import "context"

// hostReady bounds the archived/unarchived search before checking goal and runtime state.
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
			if data, ok := page["data"].([]any); ok {
				for _, item := range data {
					if obj(item)["id"] == task {
						found, archived = true, filter
						break
					}
				}
			}
			if found {
				break
			}
			cursor = str(page["nextCursor"])
			if cursor == "" {
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		return "lifecycle_unknown", nil
	}
	if archived {
		return "recipient_archived", nil
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
	answer, err = rpc.HostCall(ctx, "thread/read", map[string]any{"threadId": task})
	if err != nil {
		return "", err
	}
	thread := obj(answer["thread"])
	if thread["canAcceptDirectInput"] == false {
		return "recipient_cannot_accept_input", nil
	}
	switch obj(thread["status"])["type"] {
	case "idle", "notLoaded":
		return "", nil
	default:
		return "recipient_not_idle", nil
	}
}
