package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
)

var pageView = map[string]any{"summary": "summary", "summary_narrowed": "summary", "not_loaded": "notLoaded", "not_observed": nil}

func observationAttempt(err error, requested map[string]any) map[string]any {
	attempt := map[string]any{"requested": requested, "attribution": "unestablished"}
	var large *appserver.ResponseTooLarge
	if errors.As(err, &large) {
		attempt["frameBytes"] = large.FrameBytes
		attempt["limit"] = large.Limit
		attempt["inFlight"] = large.Methods
		attempt["note"] = "A response frame past this client's limit closed the connection while this request was pending. A frame is refused before its id is read, so it is not established that it was this request's response."
	} else {
		attempt["error"] = errorText(err)
		attempt["note"] = "The connection did not deliver this request's response. What had already been established is returned, and what this request would have added was not observed."
	}
	return attempt
}
func (b *Bridge) turnsPage(ctx context.Context, threadID string, limit int, cursor any) (map[string]any, string, []any) {
	rungs := []struct {
		view   string
		limit  int
		status string
	}{{"summary", limit, "summary"}}
	if limit > 1 {
		rungs = append(rungs, struct {
			view   string
			limit  int
			status string
		}{"summary", 1, "summary_narrowed"})
	}
	rungs = append(rungs, struct {
		view   string
		limit  int
		status string
	}{"notLoaded", limit, "not_loaded"})
	attempts := []any{}
	for _, rung := range rungs {
		params := map[string]any{"threadId": threadID, "limit": rung.limit, "itemsView": rung.view}
		if cursor != nil {
			params["cursor"] = cursor
		}
		result, err := b.call(ctx, "thread/turns/list", params)
		if err == nil {
			return result, rung.status, attempts
		}
		var large *appserver.ResponseTooLarge
		var transport *appserver.TransportError
		if !errors.As(err, &large) && !errors.As(err, &transport) {
			return nil, "not_observed", append(attempts, observationAttempt(err, map[string]any{"itemsView": rung.view, "limit": rung.limit}))
		}
		attempts = append(attempts, observationAttempt(err, map[string]any{"itemsView": rung.view, "limit": rung.limit}))
		if large == nil {
			break
		}
	}
	return nil, "not_observed", attempts
}
func (b *Bridge) readItems(ctx context.Context, threadID string, turn map[string]any, position int) {
	if position >= DetailTurns || text(turn["id"]) == "" {
		turn["itemsDetail"] = nil
		turn["itemsDetailStatus"] = "not_requested"
		return
	}
	attempts := []any{}
	for _, size := range []int{ItemPage, 1} {
		params := map[string]any{"threadId": threadID, "turnId": turn["id"], "limit": size, "sortDirection": "desc"}
		result, err := b.call(ctx, "thread/items/list", params)
		if err != nil {
			var large *appserver.ResponseTooLarge
			var rpc *appserver.RPCError
			switch {
			case errors.As(err, &large):
				attempts = append(attempts, observationAttempt(err, map[string]any{"method": "thread/items/list", "limit": size}))
				continue
			case errors.As(err, &rpc):
				turn["itemsDetail"] = nil
				turn["itemsDetailStatus"] = "refused"
				if rpc.Code == -32601 {
					turn["itemsDetailStatus"] = "method_unavailable"
				}
				turn["itemsDetailNote"] = map[string]any{"code": rpc.Code, "message": rpc.Message, "note": "The host refused the item read. This turn's summary items are unaffected, and none of this is a statement about the thread."}
				return
			default:
				turn["itemsDetail"] = nil
				turn["itemsDetailStatus"] = "not_observed"
				turn["itemsDetailNote"] = map[string]any{"attempts": append(attempts, observationAttempt(err, map[string]any{"limit": size})), "note": "This turn's items were not delivered. Its summary items are what can be seen of it here, and none of this is a fact about the thread."}
				return
			}
		}
		items, _ := result["data"].([]any)
		for l, r := 0, len(items)-1; l < r; l, r = l+1, r-1 {
			items[l], items[r] = items[r], items[l]
		}
		turn["itemsDetail"] = items
		note := map[string]any{}
		if len(attempts) > 0 {
			turn["itemsDetailStatus"] = "narrowed"
			note = map[string]any{"requestedLimit": ItemPage, "observedLimit": size, "attempts": attempts}
		} else if result["nextCursor"] != nil {
			turn["itemsDetailStatus"] = "partial"
		} else {
			turn["itemsDetailStatus"] = "complete"
		}
		if result["nextCursor"] != nil {
			note["observed"] = len(items)
			note["more"] = true
			note["note"] = "These are the most recent items of the turn, in order; earlier ones are not here. read_thread pages turns rather than items, so they cannot be reached through this tool."
		}
		if len(note) > 0 {
			turn["itemsDetailNote"] = note
		}
		return
	}
	turn["itemsDetail"] = nil
	turn["itemsDetailStatus"] = "not_observed"
	turn["itemsDetailNote"] = map[string]any{"attempts": attempts, "note": "This turn's items would not arrive even one at a time, and there is no query narrower than one item. Its summary items are all of it that can be seen here. That is a limit on observation, not a fact about the thread."}
}
func pageNote(status string, requested, observed int) string {
	seen := "Turn items are the host's summary view: each turn's user and agent messages, not its tool calls or their output."
	switch status {
	case "not_observed":
		seen = "No page of turns could be received at all, so only the thread's own metadata is here."
	case "not_loaded":
		seen = "Turns are listed without their summary items, because no page carrying items was received. Their ids, statuses and timestamps are real, and every turn's items field is empty for that reason rather than because the turn had none. What happened to each attempt is in pageAttempts; an oversized frame cannot be attributed to the request that was pending, so this does not say those pages were too large."
	}
	detail := " No item detail was read."
	if requested > 0 {
		turns := "turn"
		if requested > 1 {
			turns = "turns"
		}
		if observed == requested {
			detail = fmt.Sprintf(" Item detail was requested and read for the newest %d %s.", requested, turns)
		} else {
			detail = fmt.Sprintf(" Item detail was requested for the newest %d %s and arrived for %d of them; each turn's itemsDetailStatus says which.", requested, turns, observed)
		}
		detail += " Every other turn on this page is marked not_requested, which is not a statement that it has no items."
	}
	return seen + detail + " This is a bounded observation of a thread rather than its whole history, and it says nothing about whether that thread finished, stalled, or has to be run again."
}
func (b *Bridge) ReadThread(ctx context.Context, threadID string, limit int, cursor any, maxTextChars int) (map[string]any, error) {
	if err := nonempty(threadID, "thread_id", 128); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 || maxTextChars < 100 || maxTextChars > 20000 {
		return nil, &Invalid{"limit must be 1–100 and max_text_chars must be 100–20000"}
	}
	metadata, err := b.call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false})
	if err != nil {
		return nil, err
	}
	page, status, attempts := b.turnsPage(ctx, threadID, limit, cursor)
	var turns []any
	if page != nil {
		turns, _ = page["data"].([]any)
	}
	observed := 0
	for i, item := range turns {
		turn := object(item)
		if turn == nil {
			continue
		}
		b.readItems(ctx, threadID, turn, i)
		switch turn["itemsDetailStatus"] {
		case "complete", "partial", "narrowed":
			observed++
		}
	}
	requested := len(turns)
	if requested > DetailTurns {
		requested = DetailTurns
	}
	observation := map[string]any{"turnsPageStatus": status, "itemsView": pageView[status], "detailTurnsRequested": requested, "detailTurnsObserved": observed, "note": strings.TrimSpace(pageNote(status, requested, observed))}
	if len(attempts) > 0 {
		observation["pageAttempts"] = attempts
	}
	var pageValue any
	if page != nil {
		pageValue = page
	}
	return clipped(map[string]any{"thread": metadata["thread"], "turnsPage": pageValue, "observation": observation}, maxTextChars, false).(map[string]any), nil
}
