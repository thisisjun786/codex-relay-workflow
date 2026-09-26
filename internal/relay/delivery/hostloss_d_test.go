package delivery

import (
	"errors"
	"fmt"
	"testing"
)

// test_host_lost_turn.py HLT-28, HLT-29: the bridge adapter's recipient reads (BridgeReads),
// driven through the same call(method, params) pages the Python tests hand BridgeHostAdapter.

const sentAtHLT = 1_700_000_000.0

func turnEntry(id any, startedAt any, status string) Obj {
	return Obj{{Key: "id", Value: id}, {Key: "status", Value: status}, {Key: "startedAt", Value: startedAt}, {Key: "items", Value: []any{}}}
}

func at1(offset float64) any { return sentAtHLT + offset }

// pagesCall is _pages: a thread/turns/list answer per call, newest first, chained by cursor.
func pagesCall(t *testing.T, pages ...[]any) (func(string, Obj) (Obj, error), *[]Obj) {
	var calls []Obj
	return func(method string, params Obj) (Obj, error) {
		if method != "thread/turns/list" {
			t.Fatalf("unexpected call %s", method)
		}
		calls = append(calls, append(Obj(nil), params...))
		index := 0
		if c, ok := get(params, "cursor"); ok && c != nil {
			fmt.Sscan(c.(string), &index)
		}
		var next any
		if index+1 < len(pages) {
			next = fmt.Sprint(index + 1)
		}
		data := []any{}
		if index < len(pages) {
			data = pages[index]
		}
		return Obj{{Key: "data", Value: data}, {Key: "nextCursor", Value: next}}, nil
	}, &calls
}

func (h *hl) lookup(call func(string, Obj) (Obj, error)) (TurnPresence, error) {
	return BridgeReads{Call: call, Page: 2}.FindDispatchedTurn("thread", "wanted", sentAtHLT)
}

func presenceTurn(p TurnPresence) []any {
	return []any{p.Finding, p.Turn.TurnID, p.Turn.Status}
}

func strs(list []string) []any {
	out := []any{}
	for _, v := range list {
		out = append(out, v)
	}
	return out
}

func Test21_HLT28_the_bridge_adapter_looks_back_only_to_the_send(t *testing.T) {
	const cls = "TheAdapterLooksBackOnlyToTheSend."
	t.Run("a turn on a later page", func(t *testing.T) {
		mirror(t, hlt, cls+"test_a_turn_on_a_later_page_is_found", func(h *hl) {
			call, _ := pagesCall(t, []any{turnEntry("newer-1", at1(50), "completed"), turnEntry("newer-2", at1(40), "completed")}, []any{turnEntry("wanted", at1(1), "inProgress")})
			p, err := h.lookup(call)
			mustDo(t, err)
			h.eq(presenceTurn(p))
		})
	})
	t.Run("an older turn ends the search", func(t *testing.T) {
		mirror(t, hlt, cls+"test_a_turn_older_than_the_send_ends_the_search_without_reading_further", func(h *hl) {
			call, calls := pagesCall(t, []any{turnEntry("newer", at1(30), "completed"), turnEntry("before", at1(-600), "completed")}, []any{turnEntry("wanted", at1(-900), "completed")})
			p, err := h.lookup(call)
			mustDo(t, err)
			h.eq([]any{p.Finding, p.Stop, p.Scanned})
			h.eq(len(*calls))
		})
	})
	t.Run("the end of the listing", func(t *testing.T) {
		mirror(t, hlt, cls+"test_the_end_of_the_listing_is_absence", func(h *hl) {
			call, _ := pagesCall(t, []any{turnEntry("newer", at1(30), "completed")})
			p, err := h.lookup(call)
			mustDo(t, err)
			h.eq([]any{p.Finding, p.Stop})
		})
	})
	t.Run("an empty listing", func(t *testing.T) {
		mirror(t, hlt, cls+"test_an_empty_listing_is_not_evidence", func(h *hl) {
			call, _ := pagesCall(t, []any{})
			_, err := h.lookup(call)
			var empty *ListingEmpty
			if !errors.As(err, &empty) {
				t.Fatalf("want HostUnavailable (ListingEmpty), got %v", err)
			}
		})
	})
	t.Run("a bounded scan that never reached the send", func(t *testing.T) {
		mirror(t, hlt, cls+"test_a_bounded_scan_that_never_reached_the_send_is_not_evidence", func(h *hl) {
			var pages [][]any
			for n := 0; n < 25; n++ {
				pages = append(pages, []any{turnEntry(fmt.Sprintf("newer-%d-0", n), at1(float64(1000-n)), "completed"), turnEntry(fmt.Sprintf("newer-%d-1", n), at1(float64(1000-n)), "completed")})
			}
			call, calls := pagesCall(t, pages...)
			_, err := h.lookup(call)
			var bounded *ListingBounded
			if !errors.As(err, &bounded) {
				t.Fatalf("want HostUnavailable (ListingBounded), got %v", err)
			}
			h.eq(len(*calls))
		})
	})
	t.Run("250 later turns are read through", func(t *testing.T) {
		mirror(t, hlt, cls+"test_a_long_history_after_the_send_is_read_through_to_the_send", func(h *hl) {
			var pages [][]any
			for n := 0; n < 5; n++ {
				var page []any
				for m := 0; m < 50; m++ {
					page = append(page, turnEntry(fmt.Sprintf("newer-%d-%d", n, m), at1(float64(1000-n)), "completed"))
				}
				pages = append(pages, page)
			}
			pages = append(pages, []any{turnEntry("before", at1(-600), "completed")})
			call, calls := pagesCall(t, pages...)
			p, err := BridgeReads{Call: call, Page: 50}.FindDispatchedTurn("thread", "wanted", sentAtHLT)
			mustDo(t, err)
			h.eq([]any{p.Finding, p.Stop, p.Scanned})
			h.eq(len(*calls))
		})
	})
	t.Run("a steered turn begun before the send", func(t *testing.T) {
		mirror(t, hlt, cls+"test_a_steered_turn_begun_before_the_send_is_matched_before_the_cutoff", func(h *hl) {
			call, _ := pagesCall(t, []any{turnEntry("wanted", at1(-600), "inProgress"), turnEntry("before", at1(-900), "completed")})
			p, err := h.lookup(call)
			mustDo(t, err)
			h.eq(p.Finding)
		})
	})
	t.Run("an undated turn never ends the search", func(t *testing.T) {
		mirror(t, hlt, cls+"test_a_turn_with_no_start_time_never_ends_the_search", func(h *hl) {
			call, _ := pagesCall(t, []any{turnEntry("undated", nil, "completed"), turnEntry("wanted", at1(2), "completed")})
			p, err := h.lookup(call)
			mustDo(t, err)
			h.eq(p.Finding)
		})
	})
	t.Run("the request shape", func(t *testing.T) {
		mirror(t, hlt, cls+"test_it_asks_for_the_newest_turns_first_without_their_items", func(h *hl) {
			call, calls := pagesCall(t, []any{turnEntry("wanted", at1(1), "completed")})
			_, err := h.lookup(call)
			mustDo(t, err)
			h.eq((*calls)[0])
		})
	})
	t.Run("a match reads on to the first older turn", func(t *testing.T) {
		mirror(t, hlt, cls+"test_a_match_reads_on_to_the_first_turn_older_than_the_send", func(h *hl) {
			call, calls := pagesCall(t, []any{turnEntry("newer", at1(30), "completed"), turnEntry("wanted", at1(1), "completed")},
				[]any{turnEntry("between", at1(-10), "completed"), turnEntry("before", at1(-600), "completed")}, []any{turnEntry("older", at1(-900), "completed")})
			p, err := h.lookup(call)
			mustDo(t, err)
			h.eq([]any{p.Finding, p.Turn.TurnID, p.Scanned})
			h.eq(strs(p.Older))
			h.eq(len(*calls))
		})
	})
	t.Run("a failure reading on keeps the match", func(t *testing.T) {
		mirror(t, hlt, cls+"test_a_failure_reading_on_after_a_match_keeps_the_match", func(h *hl) {
			first := []any{turnEntry("newer", at1(30), "completed"), turnEntry("wanted", at1(1), "completed")}
			call := func(method string, params Obj) (Obj, error) {
				if c, _ := get(params, "cursor"); truthy(c) {
					return nil, &HostError{Kind: "HostUnavailable", Message: "the next page could not be read"}
				}
				return Obj{{Key: "data", Value: first}, {Key: "nextCursor", Value: "1"}}, nil
			}
			p, err := h.lookup(call)
			mustDo(t, err)
			h.eq([]any{p.Finding, p.Turn.TurnID, strs(p.Older)})
		})
	})
}

func itemEntry(turn, kind, text string) Obj {
	var item Obj
	switch kind {
	case "userMessage":
		item = Obj{{Key: "type", Value: kind}, {Key: "id", Value: "i"}, {Key: "content", Value: []any{Obj{{Key: "type", Value: "text"}, {Key: "text", Value: text}}}}}
	case "commandExecution":
		item = Obj{{Key: "type", Value: kind}, {Key: "id", Value: "i"}, {Key: "aggregatedOutput", Value: text}}
	case "agentMessage":
		item = Obj{{Key: "type", Value: kind}, {Key: "id", Value: "i"}, {Key: "text", Value: text}}
	default:
		item = Obj{{Key: "type", Value: kind}, {Key: "id", Value: "i"}, {Key: "fragments", Value: []any{Obj{{Key: "text", Value: text}}}}}
	}
	return Obj{{Key: "turnId", Value: turn}, {Key: "item", Value: item}}
}

func Test21_HLT29_the_in_turn_and_thread_reads_tell_the_message_from_agent_output(t *testing.T) {
	const cls = "TheAdapterLooksBackOnlyToTheSend."
	t.Run("the in-turn read asks for that turn's items oldest first", func(t *testing.T) {
		mirror(t, hlt, cls+"test_the_in_turn_read_asks_for_that_turns_items_oldest_first", func(h *hl) {
			var calls []any
			call := func(method string, params Obj) (Obj, error) {
				calls = append(calls, []any{method, append(Obj(nil), params...)})
				return Obj{{Key: "data", Value: []any{itemEntry("t1", "userMessage", "requestId: tok")}}, {Key: "nextCursor", Value: nil}}, nil
			}
			scan, err := BridgeReads{Call: call, Page: 2}.FindTokenInTurn("thread", "tok", "t1", InTurnItemsMax)
			mustDo(t, err)
			h.eq(scan.Found)
			h.eq(calls)
		})
	})
	t.Run("the in-turn read counts only that turn's user message", func(t *testing.T) {
		mirror(t, hlt, cls+"test_the_in_turn_read_counts_only_that_turns_user_message", func(h *hl) {
			call := func(string, Obj) (Obj, error) {
				return Obj{{Key: "data", Value: []any{itemEntry("other", "userMessage", "requestId: tok"), itemEntry("t1", "commandExecution", "requestId: tok")}}, {Key: "nextCursor", Value: nil}}, nil
			}
			scan, err := BridgeReads{Call: call, Page: 8}.FindTokenInTurn("thread", "tok", "t1", InTurnItemsMax)
			mustDo(t, err)
			h.eq(scan.Found)
			h.eq(scan.Exhausted)
		})
	})
	t.Run("the thread reads tell the message from agent output", func(t *testing.T) {
		mirror(t, hlt, cls+"test_the_thread_reads_tell_the_message_from_agent_output", func(h *hl) {
			items := []any{itemEntry("t2", "commandExecution", "del-x-a1"), itemEntry("t2", "hookPrompt", "del-x-a1")}
			call := func(string, Obj) (Obj, error) {
				return Obj{{Key: "data", Value: items}, {Key: "nextCursor", Value: nil}}, nil
			}
			b := BridgeReads{Call: call, Page: 8}
			only, err := b.FindToken("thread", "del-x-a1", 200, true)
			mustDo(t, err)
			h.eq(only.Found)
			all, err := b.FindToken("thread", "del-x-a1", 200, false)
			mustDo(t, err)
			h.eq(all.Found)
			prompted, err := b.FindTokenSince("thread", "del-x-a1", nil, 200)
			mustDo(t, err)
			h.eq(prompted.Found)
			h.eq([]any{prompted.OtherTurn, prompted.OtherKind})
			items[1] = itemEntry("t2", "fileChange", "del-x-a1")
			written, err := b.FindTokenSince("thread", "del-x-a1", nil, 200)
			mustDo(t, err)
			h.eq(written.Found)
			h.eq(written.OtherKind)
			items = []any{itemEntry("t2", "someLaterKind", "del-x-a1"), itemEntry("t1", "userMessage", "del-x-a1")}
			behind, err := b.FindTokenSince("thread", "del-x-a1", nil, 200)
			mustDo(t, err)
			h.eq(behind.Found)
		})
	})
}
