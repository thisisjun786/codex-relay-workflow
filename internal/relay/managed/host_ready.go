package managed

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// hostReady requires a complete archived scan; a listing miss uses thread/read for existence.
func hostReady(ctx context.Context, rpc HostRPC, task string) (string, error) {
	return businessResendCheckHost(ctx, rpc, task, false)
}

// A resend already knows its child. A complete archived scan followed by a
// positive thread/read establishes the same lifecycle facts without scanning
// the unarchived listing too, leaving budget for the final history check.
func businessResendCheckHost(ctx context.Context, rpc HostRPC, task string, resend bool) (string, error) {
	found, complete, err := scanThreadListing(ctx, rpc, task, true)
	if err != nil {
		return "", err
	}
	if !complete {
		return "archived_listing_incomplete", nil
	}
	archived := found
	if !found && !resend {
		if found, _, err = scanThreadListing(ctx, rpc, task, false); err != nil {
			return "", err
		}
	}
	if archived {
		return "recipient_archived", nil
	}
	var thread map[string]any
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

// scanThreadListing looks for task in up to four pages of the archived or the unarchived thread
// listing. For the archived listing, complete reports whether the pages read cover the whole
// listing: a page with no data array, an entry with no valid id, an unreadable cursor or a fifth
// page leaves it incomplete, and a miss in an incomplete listing proves nothing. The unarchived
// listing is only a lookup, so complete is always true for it.
func scanThreadListing(ctx context.Context, rpc HostRPC, task string, archived bool) (found, complete bool, err error) {
	cursor := ""
	for range 4 {
		params := map[string]any{"limit": 50, "archived": archived, "useStateDbOnly": true}
		if cursor != "" {
			params["cursor"] = cursor
		}
		page, err := rpc.HostCall(ctx, "thread/list", params)
		if err != nil {
			return false, false, err
		}
		data, ok := page["data"].([]any)
		if archived && !ok {
			return false, false, nil
		}
		for _, item := range data {
			if archived && !delivery.ValidSegment(pyjson.Map(item)["id"]) {
				return false, false, nil
			}
			if pyjson.Map(item)["id"] == task {
				return true, true, nil
			}
		}
		if next := page["nextCursor"]; archived && next != nil {
			if _, ok := next.(string); !ok {
				return false, false, nil
			}
		}
		cursor = pyjson.Text(page["nextCursor"])
		if cursor == "" {
			return false, true, nil
		}
	}
	return false, !archived, nil
}
