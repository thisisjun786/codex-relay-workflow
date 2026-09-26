package delivery

import (
	"encoding/json"
	"strings"
)

// BridgeReads is the recipient-reading half of bridge_adapter.BridgeHostAdapter: the four reads
// the host-loss and reconciliation rules depend on, over one call(method, params) -> result, the
// only way the adapter reaches the host. The adapter itself (transport, ledger, sends) is todo
// 28's; it embeds these so the paging, bounds and item typing stay the ones tested here.
type BridgeReads struct {
	Call func(method string, params Obj) (Obj, error)
	Page int
}

// DispatchedTurnMaxPages is hostadapter.DISPATCHED_TURN_MAX_PAGES.
const DispatchedTurnMaxPages = 20

// BridgePage is bridge_adapter.PAGE.
const BridgePage = 50

func (b BridgeReads) page() int {
	if b.Page > 0 {
		return b.Page
	}
	return BridgePage
}

func list(result Obj) []any {
	data, _ := get(result, "data")
	items, _ := data.([]any)
	return items
}

func cursorOf(result Obj) any {
	next, _ := get(result, "nextCursor")
	if !truthy(next) {
		return nil
	}
	return next
}

func number(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case int64:
		f := float64(n)
		return &f
	case json.Number:
		f, err := n.Float64()
		if err == nil {
			return &f
		}
	}
	return nil
}

// FindDispatchedTurn is find_dispatched_turn: newest first, without items, stopping at the first
// turn begun before the send.
func (b BridgeReads) FindDispatchedTurn(thread, turnID string, sentAt float64) (TurnPresence, error) {
	var cursor any
	read := func() (ListingPage, error) {
		params := Obj{{Key: "threadId", Value: thread}, {Key: "limit", Value: int64(b.page())}, {Key: "itemsView", Value: "notLoaded"}, {Key: "sortDirection", Value: "desc"}}
		if cursor != nil {
			params = append(params, F{Key: "cursor", Value: cursor})
		}
		result, err := b.Call("thread/turns/list", params)
		if err != nil {
			return ListingPage{}, err
		}
		cursor = cursorOf(result)
		var turns []TurnInfo
		for _, raw := range list(result) {
			turn, _ := raw.(Obj)
			id, _ := get(turn, "id")
			status, ok := get(turn, "status")
			if !ok {
				status = "unknown"
			}
			started, _ := get(turn, "startedAt")
			turns = append(turns, TurnInfo{TurnID: pyStrOrEmpty(id), Status: pyStr(status), StartedAt: number(started)})
		}
		return ListingPage{Turns: turns, Follows: cursor != nil}, nil
	}
	return FindInListingPaged(read, DispatchedTurnMaxPages, turnID, sentAt)
}

// itemText is bridge_adapter._item_text.
func itemText(entry Obj) string {
	raw, _ := get(entry, "item")
	item, ok := raw.(Obj)
	if !ok {
		return dumpsSorted(entry)
	}
	for _, key := range []string{"text", "preview", "summary", "aggregatedOutput"} {
		if v, ok := get(item, key); ok {
			if s, isText := v.(string); isText {
				return s
			}
		}
	}
	return dumpsSorted(item)
}

// itemKind is bridge_adapter._item_kind: the host's type, "" when it gives none.
func itemKind(entry Obj) string {
	raw, _ := get(entry, "item")
	item, _ := raw.(Obj)
	kind, _ := get(item, "type")
	s, _ := kind.(string)
	return s
}

func triples(entries []any) []Item {
	out := make([]Item, 0, len(entries))
	for _, raw := range entries {
		entry, _ := raw.(Obj)
		turn, _ := get(entry, "turnId")
		out = append(out, Item{Turn: pyStrOrEmpty(turn), Text: itemText(entry), Type: itemKind(entry)})
	}
	return out
}

// FindToken is find_token: forward paging and honest exhaustion. messageOnly counts only an
// item typed as a user message (or untyped).
func (b BridgeReads) FindToken(thread, token string, limit int, messageOnly bool) (TokenScan, error) {
	var cursor any
	scanned := 0
	for scanned < limit {
		params := Obj{{Key: "threadId", Value: thread}, {Key: "sortDirection", Value: "desc"}, {Key: "limit", Value: int64(min(b.page(), limit-scanned))}}
		if cursor != nil {
			params = append(params, F{Key: "cursor", Value: cursor})
		}
		result, err := b.Call("thread/items/list", params)
		if err != nil {
			return TokenScan{}, err
		}
		entries := list(result)
		for _, item := range triples(entries) {
			scanned++
			if messageOnly && !isMessage(item.Type) {
				continue
			}
			if strings.Contains(item.Text, token) {
				var turn any
				if item.Turn != "" {
					turn = item.Turn
				}
				return TokenScan{Found: true, TurnID: turn, Scanned: scanned}, nil
			}
		}
		if cursor = cursorOf(result); cursor == nil {
			return TokenScan{Exhausted: true, Scanned: scanned}, nil
		}
		if len(entries) == 0 {
			break
		}
	}
	return TokenScan{Scanned: scanned}, nil
}

// itemPages reads thread/items/list pages lazily, up to limit items, for the token rules.
func (b BridgeReads) itemPages(base Obj, limit int) ([]ItemPage, error) {
	var pages []ItemPage
	var cursor any
	read := 0
	for read < limit {
		params := append(append(Obj(nil), base...), F{Key: "limit", Value: int64(min(b.page(), limit-read))})
		if cursor != nil {
			params = append(params, F{Key: "cursor", Value: cursor})
		}
		result, err := b.Call("thread/items/list", params)
		if err != nil {
			return nil, err
		}
		entries := list(result)
		read += len(entries)
		cursor = cursorOf(result)
		pages = append(pages, ItemPage{Items: triples(entries), Follows: cursor != nil})
		if cursor == nil || len(entries) == 0 {
			break
		}
	}
	return pages, nil
}

// FindTokenSince is find_token_since: newest first, stopping at an item of a turn the listing
// showed began before the send.
func (b BridgeReads) FindTokenSince(thread, token string, older []string, limit int) (TokenScan, error) {
	pages, err := b.itemPages(Obj{{Key: "threadId", Value: thread}, {Key: "sortDirection", Value: "desc"}}, limit)
	if err != nil {
		return TokenScan{}, err
	}
	return FindTokenIn(pages, token, older), nil
}

// FindTokenInTurn is find_token_in_turn: one turn's own items, oldest first.
func (b BridgeReads) FindTokenInTurn(thread, token, turnID string, limit int) (TokenScan, error) {
	pages, err := b.itemPages(Obj{{Key: "threadId", Value: thread}, {Key: "turnId", Value: turnID}, {Key: "sortDirection", Value: "asc"}}, limit)
	if err != nil {
		return TokenScan{}, err
	}
	return FindTokenInTurnItems(pages, token, turnID), nil
}
