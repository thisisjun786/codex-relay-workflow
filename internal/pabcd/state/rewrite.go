package state

import (
	"encoding/json"
	"math/big"
	"strings"
)

// rewriteNumberMaxLen bounds the text of an attempts value the guard will compare exactly; a longer literal is read as a loss.
const rewriteNumberMaxLen = 64

// RewriteKeepsUnverified says whether writing kept back over the session file raw would lose nothing the file stores under
// unverifiedSubagents. kept is the list ReadStateStrict rebuilt from raw: ReconstructUnverified drops a malformed entry, stops at
// MaxUnverifiedSubagents, cuts receiptClaimed to MaxReceiptClaimLen units and replaces a field of the wrong type by its default,
// so the rebuilt list can differ from the stored one in ways a count does not show. The answer is true only when the stored list is
// an array of the same length (an absent or null list is an empty one) whose record i equals kept[i] in every field the reader
// handles: agentId and recordedAt as text, turnId, agentType and receiptClaimed as text or absent, attempts as a number that the
// writer prints back as the same value, or absent, resolvable as a boolean or absent. A field that is absent or null holds nothing a default can lose. A key the
// reader does not handle is not judged, as nowhere else in the file. raw is read as ReadStateStrict reads it, so a file that is
// not a JSON object, a list that is not an array and a record that is not an object are all refused.
//
// Not judged: a lone surrogate escape in a Node-written receipt reads as U+FFFD on both sides, so its rewrite shows no difference.
func RewriteKeepsUnverified(raw []byte, kept []UnverifiedSubagent) bool {
	m := decodeObject(raw)
	if m == nil {
		return false
	}
	list := m["unverifiedSubagents"]
	if list == nil {
		return len(kept) == 0
	}
	items, ok := list.([]any)
	if !ok || len(items) != len(kept) {
		return false
	}
	for i, item := range items {
		if !rewriteKeepsRecord(item, kept[i]) {
			return false
		}
	}
	return true
}

func rewriteKeepsRecord(item any, e UnverifiedSubagent) bool {
	o, ok := item.(map[string]any)
	return ok && rewriteText(o["agentId"], e.AgentID, true) && rewriteText(o["recordedAt"], e.RecordedAt, true) &&
		rewriteText(o["turnId"], e.TurnID, false) && rewriteText(o["agentType"], e.AgentType, false) &&
		rewriteText(o["receiptClaimed"], e.ReceiptClaimed, false) && rewriteFlag(o["resolvable"], e.Resolvable) &&
		rewriteCount(o["attempts"], e.Attempts)
}

// rewriteText is a stored text field that the rebuilt value repeats; absent or null holds nothing unless the field is required.
func rewriteText(stored any, kept string, required bool) bool {
	if stored == nil {
		return !required
	}
	s, ok := stored.(string)
	return ok && s == kept
}

func rewriteFlag(stored any, kept bool) bool {
	if stored == nil {
		return true
	}
	b, ok := stored.(bool)
	return ok && b == kept
}

// rewriteCount compares attempts by exact value with what the writer prints: the file is rewritten with the shortest decimal that
// reads back as the reader's float64, which is not the float64's own value (a stored 1000000000000000128 comes back as
// 1.0000000000000001e+18) and not the stored text (the writer's 1e+23 and 3.0 read as 1e23 and 3). The forms of one value (3, 3.0,
// 1e2, -0) are all kept; a fraction the reader floors to its default, a digit lost beyond 2^53 and a tiny value rounded to zero are
// losses. A literal over rewriteNumberMaxLen bytes, or with an exponent of more than three digits, is read as a loss without
// being parsed, so a hostile file cannot make the comparison expensive.
func rewriteCount(stored any, kept float64) bool {
	if stored == nil {
		return true
	}
	n, ok := stored.(json.Number)
	if !ok || len(n) > rewriteNumberMaxLen {
		return false
	}
	if e := strings.IndexAny(string(n), "eE"); e >= 0 && len(n)-e > 5 { // e, a sign and three digits
		return false
	}
	written, err := json.Marshal(kept)
	if err != nil {
		return false
	}
	was, wasOK := new(big.Rat).SetString(string(n))
	now, nowOK := new(big.Rat).SetString(string(written))
	return wasOK && nowOK && was.Cmp(now) == 0
}
