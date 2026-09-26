package mergeturn

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

func Test26_MTG_8_malformed_review_keeps_store_unchanged(t *testing.T) {
	w := newFx(t)
	turn := w.held()
	for _, c := range []struct {
		key    string
		value  any
		detail string
	}{{"threadsSeen", json.Number("1"), "threadsSeen is a list of thread identifiers, not a int"}, {"unresolved", []any{}, "unresolved is a whole number, not a list"}} {
		b := defaults()
		b.review = review("hasNextPage", false, "pagesRead", json.Number("1"), "totalCount", json.Number("1"), "threadsSeen", []any{"thread-1"}, "unresolved", json.Number("0"))
		stated := b.review.(contract.OrderedObject)
		for i := range stated {
			if stated[i].Key == c.key {
				stated[i].Value = c.value
			}
		}
		b.review = stated
		before, e := w.s.All(w.ctx, "SELECT * FROM merge_turn_checks WHERE turn_id=?", turn)
		if e != nil {
			t.Fatal(e)
		}
		ledgerBefore, e := w.s.All(w.ctx, "SELECT * FROM merge_turn_ledger WHERE turn_id=?", turn)
		if e != nil {
			t.Fatal(e)
		}
		_, err := w.begin(turn, b)
		if reasonOf(err) != "merge_evidence_malformed" || !strings.Contains(err.Error(), c.detail) {
			t.Fatal(err)
		}
		after, e := w.s.All(w.ctx, "SELECT * FROM merge_turn_checks WHERE turn_id=?", turn)
		if e != nil || !reflect.DeepEqual(before, after) {
			t.Fatal(before, after, e)
		}
		ledgerAfter, e := w.s.All(w.ctx, "SELECT * FROM merge_turn_ledger WHERE turn_id=?", turn)
		if e != nil || !reflect.DeepEqual(ledgerBefore, ledgerAfter) {
			t.Fatal(ledgerBefore, ledgerAfter, e)
		}
	}
	b := defaults()
	b.review = review("hasNextPage", false, "pagesRead", json.Number("1"), "totalCount", json.Number("1"), "unresolved", json.Number("0"))
	_, err := w.begin(turn, b)
	if reasonOf(err) != "merge_review_incomplete" || !strings.Contains(err.Error(), "does not state threadsSeen") {
		t.Fatal(err)
	}
	result, e := w.begin(turn, defaults())
	if e != nil || result["state"] != "merging" {
		t.Fatal(result, e)
	}
}
