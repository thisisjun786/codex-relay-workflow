// Package faults is the relay's operational fault ledger (faults.py, faultsweep.py).
//
// Subset ported for todo 21; todo 22 owns this package. Only what the delivery hold paths
// exercise is here: FaultLedger.record for the observations the sweep derives (identity,
// occurrences, the timeline, suppression, the state transition, the opening and escalation
// publications and the blocking notification), and the two sweep sources that read deliveries
// (delivery_faults, retry_faults) with the clears recovered() derives for their class. Every
// other source, adoption, workspaces, rescoping, reopening and the publication worker are todo
// 22's; a path that would need one of them is refused with an error naming it rather than
// answered differently from Python.
package faults

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// dumps is Python json.dumps(value, ensure_ascii=False, sort_keys=True); compact selects
// separators=(",", ":"). A string is read as Go's range reads it (a byte that is not UTF-8 is
// U+FFFD): the bytes fault ids have always hashed.
func dumps(value any, compact bool) string {
	return pyjson.Dumps(value, pyjson.Options{Compact: compact, SortKeys: true, Unicode: true, Bytes: pyjson.ReplacedAll})
}

// loads decodes JSON as json.Decoder.Decode does into map[string]any / []any / json.Number /
// string / bool / nil, leaving what follows the value unread; a refusal is Decode's own error.
func loads(text string) (any, error) {
	return pyjson.Loads(text, pyjson.LoadOptions{Map: true, Numbers: pyjson.SpelledNumbers, Trailing: pyjson.TrailingAnything})
}

func loadsMap(text string) map[string]any {
	v, err := loads(text)
	if err != nil {
		return map[string]any{}
	}
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func named(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
}
