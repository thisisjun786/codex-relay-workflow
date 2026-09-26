package delivery

import (
	"fmt"
	"slices"
	"strings"
)

// Host reading rules shared by every adapter (hostadapter.py).
const (
	TurnStartPrecisionSeconds = 1.0
	DispatchTurnSkewSeconds   = 60.0
	TurnPresent               = "present"
	TurnAbsent                = "absent"
	InTurnItemsMax            = 2000
	userMessage               = "userMessage"
)

var agentOutput = []string{"agentMessage", "collabAgentToolCall", "commandExecution", "contextCompaction", "dynamicToolCall", "enteredReviewMode", "exitedReviewMode", "fileChange", "functionCallOutput", "imageGeneration", "imageView", "mcpToolCall", "plan", "reasoning", "sleep", "subAgentActivity", "webSearch"}

func isMessage(kind string) bool    { return kind == "" || kind == userMessage }
func mayBeMessage(kind string) bool { return !slices.Contains(agentOutput, kind) }

// ListingBounded: the bounded listing never reached the send (HostUnavailable).
type ListingBounded struct{ Message string }

func (e *ListingBounded) Error() string { return e.Message }

// ListingEmpty: the recipient listed no turns at all (HostUnavailable).
type ListingEmpty struct{ Message string }

func (e *ListingEmpty) Error() string { return e.Message }

// TurnPresence is hostadapter.TurnPresence.
type TurnPresence struct {
	Finding   string
	Turn      *TurnInfo
	Scanned   int
	Stop      string
	Seen      []any
	Older     []string
	SeenTurns []TurnInfo
	StopTurn  *TurnInfo
}

// ListingPage is one newest-first page of turns.
type ListingPage struct {
	Turns   []TurnInfo
	Follows bool
}

// Item is one thread item: (turn id, text, type); Type "" means untyped.
type Item struct{ Turn, Text, Type string }

// ItemPage is one page of items.
type ItemPage struct {
	Items   []Item
	Follows bool
}

func olderTurns(turns []TurnInfo, cutoff float64) []string {
	var out []string
	for _, t := range turns {
		if t.StartedAt != nil && *t.StartedAt <= cutoff {
			out = append(out, t.TurnID)
		}
	}
	return out
}

// FindInListing is find_in_listing over pages already read.
func FindInListing(pages []ListingPage, turnID string, sentAt float64) (TurnPresence, error) {
	next := 0
	return FindInListingPaged(func() (ListingPage, error) {
		page := pages[next]
		next++
		return page, nil
	}, len(pages), turnID, sentAt)
}

// FindInListingPaged is find_in_listing over a lazy listing: read yields the next page, at most
// bound pages are read, and nothing past the answer is read. turnID "" lists the turns since a
// send with no turn id; a listed turn without an id is never taken for it.
func FindInListingPaged(read func() (ListingPage, error), bound int, turnID string, sentAt float64) (TurnPresence, error) {
	cutoff := sentAt - TurnStartPrecisionSeconds - DispatchTurnSkewSeconds
	scanned := 0
	seen := []any{}
	var seenTurns []TurnInfo
	for p := 0; p < bound; p++ {
		page, err := read()
		if err != nil {
			return TurnPresence{}, err
		}
		for i, turn := range page.Turns {
			scanned++
			if turnID != "" && turn.TurnID == turnID {
				match := turn
				return TurnPresence{TurnPresent, &match, scanned, "matched", seen, olderAfterMatch(page.Turns[i+1:], page.Follows, read, bound-p-1, cutoff), seenTurns, nil}, nil
			}
			if turn.StartedAt != nil && *turn.StartedAt <= cutoff {
				stop := turn
				return TurnPresence{TurnAbsent, nil, scanned, "older_than_send", seen, olderTurns(page.Turns[i:], cutoff), seenTurns, &stop}, nil
			}
			seen = append(seen, idOf(turn))
			seenTurns = append(seenTurns, turn)
		}
		if !page.Follows {
			if scanned == 0 {
				return TurnPresence{}, &ListingEmpty{"the recipient's turn list is empty, which does not show that a turn is gone"}
			}
			return TurnPresence{TurnAbsent, nil, scanned, "listing_end", seen, nil, seenTurns, nil}, nil
		}
	}
	subject := "the send's turn"
	if turnID != "" {
		subject = "turn " + pyReprValue(turnID)
	}
	return TurnPresence{}, &ListingBounded{fmt.Sprintf("%s was not among %d turns and the bounded listing never reached the send; this is not evidence of absence", subject, scanned)}
}

// idOf is a listed turn's id, None when the host gave none.
func idOf(turn TurnInfo) any {
	if turn.TurnID == "" {
		return nil
	}
	return turn.TurnID
}

// olderAfterMatch is _older_after_match: after a match, read on to the first turn begun before
// the send. A failed read or the page bound gives none; the match stands either way.
func olderAfterMatch(rest []TurnInfo, follows bool, read func() (ListingPage, error), left int, cutoff float64) []string {
	for {
		for i, turn := range rest {
			if turn.StartedAt != nil && *turn.StartedAt <= cutoff {
				return olderTurns(rest[i:], cutoff)
			}
		}
		if !follows || left <= 0 {
			return nil
		}
		page, err := read()
		if err != nil {
			return nil
		}
		left--
		rest, follows = page.Turns, page.Follows
	}
}

// FindTokenIn is find_token_in: a token among a thread's items since a send, newest first.
func FindTokenIn(pages []ItemPage, token string, older []string) TokenScan {
	scanned := 0
	var other [2]any
	for _, page := range pages {
		for _, item := range page.Items {
			if item.Turn != "" && slices.Contains(older, item.Turn) {
				return TokenScan{false, nil, true, scanned, other[0], other[1]}
			}
			scanned++
			if !mayBeMessage(item.Type) || !strings.Contains(item.Text, token) {
				continue
			}
			if isMessage(item.Type) {
				return TokenScan{true, item.Turn, false, scanned, nil, nil}
			}
			if other[0] == nil && other[1] == nil {
				other = [2]any{item.Turn, item.Type}
			}
		}
		if !page.Follows {
			return TokenScan{false, nil, true, scanned, other[0], other[1]}
		}
	}
	return TokenScan{false, nil, false, scanned, other[0], other[1]}
}

// FindTokenInTurnItems is find_token_in_turn_items.
func FindTokenInTurnItems(pages []ItemPage, token, turnID string) TokenScan {
	scanned := 0
	var other [2]any
	for _, page := range pages {
		for _, item := range page.Items {
			scanned++
			if item.Turn != turnID || !mayBeMessage(item.Type) || !strings.Contains(item.Text, token) {
				continue
			}
			if isMessage(item.Type) {
				return TokenScan{true, item.Turn, false, scanned, nil, nil}
			}
			if other[0] == nil && other[1] == nil {
				other = [2]any{item.Turn, item.Type}
			}
		}
		if !page.Follows {
			return TokenScan{false, nil, true, scanned, other[0], other[1]}
		}
	}
	return TokenScan{false, nil, false, scanned, other[0], other[1]}
}
