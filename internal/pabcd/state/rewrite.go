package state

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// rewriteNumberMaxLen bounds the text of an attempts value the guard will compare exactly; a longer literal is read as a loss.
const rewriteNumberMaxLen = 64

// RewriteKeepsStored says whether writing next back over the session file raw would keep every record the file stores: the one
// data-loss judgement every writer that rewrites the whole state asks, so no writer can carry a narrower copy. It is false when
// RewriteKeepsUnverified refuses the stored unverified-subagent list, otherwise RewriteKeepsInterview's answer for the stored
// interview tracker. A document that is not one JSON object is false: both checks read it as ReadStateStrict does and refuse it.
//
// A legacy D-close recovery marker is NOT part of this judgement (decision 2026-10-07): since CRW-648 the shared reader restores
// the stored legacy flag, so a rewrite no longer loses the marker's distinction, and the CLI reset, which is the recovery for such
// a marker, must stay able to clear it. The writers that refuse such a state do so through DcloseRecoveryLegacy, each exactly as
// it did before this function existed.
//
// Each caller keeps its own handling of an absent file and of a read failure, and its own refusal sentence; this function only
// answers the question about the bytes it is given.
func RewriteKeepsStored(raw []byte, next State) bool {
	if !RewriteKeepsUnverified(raw, next.UnverifiedSubagents) {
		return false
	}
	return RewriteKeepsInterview(raw, next.Interview)
}

// DcloseRecoveryLegacy says whether next holds a D-close recovery marker the reader restored as legacy, whose successor was absent
// or malformed. It is the separate predicate the writers that refuse such a state share, kept out of RewriteKeepsStored: the marker
// survives a write since CRW-648, so refusing it is a per-writer decision about recovery (a legacy marker cannot be read safely by
// the D-close retry), not a data loss. A marker with an explicit null successor is authoritative and is not legacy.
func DcloseRecoveryLegacy(next State) bool {
	return next.DcloseRecovery != nil && next.DcloseRecovery.Legacy
}

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
// The characters of the file are judged first, before any decoding. encoding/json reads an unpaired UTF-16 surrogate escape (such as
// \ud800, which Node keeps and writes back as written) and every byte of invalid UTF-8 as U+FFFD, in the reader and here alike, so
// the two lists would agree while a rewrite replaced the stored text. A file that holds either is refused wherever it sits, because
// the callers rewrite the whole file: a key the reader ignores, or a repeated key that a later one shadows, counts like any other
// text. The comparison of fields keeps its scope.
func RewriteKeepsUnverified(raw []byte, kept []UnverifiedSubagent) bool {
	if !utf8.Valid(raw) || rewriteLosslessUnpaired(raw) {
		return false
	}
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
// losses. A literal over rewriteNumberMaxLen bytes, or with an exponent of more than three digits once its sign and leading zeros
// are removed (3e0000 is the value 3), is read as a loss without being parsed, so a hostile file cannot make the comparison
// expensive.
func rewriteCount(stored any, kept float64) bool {
	if stored == nil {
		return true
	}
	n, ok := stored.(json.Number)
	if !ok || len(n) > rewriteNumberMaxLen {
		return false
	}
	if e := strings.IndexAny(string(n), "eE"); e >= 0 && len(strings.TrimLeft(strings.TrimLeft(string(n)[e+1:], "+-"), "0")) > 3 {
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

// rewriteLosslessUnpaired says whether raw holds a surrogate escape (\uD800 to \uDFFF) that is not half of a pair written high then
// low, which the decoder reads as U+FFFD. It is unpairedSurrogate of internal/pabcd/goalplan/read.go, copied because goalplan
// imports this package, over bytes and bounded by the end of the input, since it runs before the decoder has judged the file. A
// backslash escapes the byte after it, so the u after an escaped backslash starts no escape; a \u without four hex digits is left
// to the decoder, which refuses the file.
func rewriteLosslessUnpaired(raw []byte) bool {
	unit := func(at int) uint64 {
		if at+6 > len(raw) || raw[at] != '\\' || raw[at+1] != 'u' {
			return 0
		}
		n, _ := strconv.ParseUint(string(raw[at+2:at+6]), 16, 16)
		return n
	}
	for i := 0; i+1 < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		switch n := unit(i); {
		case n >= 0xd800 && n <= 0xdbff:
			if m := unit(i + 6); m < 0xdc00 || m > 0xdfff {
				return true
			}
			i += 11
		case n >= 0xdc00 && n <= 0xdfff:
			return true
		case raw[i+1] == 'u':
			i += 5
		default:
			i++
		}
	}
	return false
}
