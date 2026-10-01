package store

import (
	"encoding/json"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// decodeOrdered reads a stored document (a receipt, a continuation claim) as encoding/json reads
// it, into pyjson's values: its objects in document order and every number a json.Number as it
// is spelled.
func decodeOrdered(data []byte) (any, error) {
	return pyjson.Loads(string(data), pyjson.LoadOptions{Numbers: pyjson.SpelledNumbers})
}

// receiptRecord is how a receipt is stored: json.dumps of what json.loads made of it, so a number
// is written as the int or float it spells.
var receiptRecord = pyjson.Options{Normalize: true}

// jsonInteger answers Python's isinstance(x, int) and not bool for a receipt's value: a JSON
// number with no fraction, within int64.
func jsonInteger(v any) (int64, bool) {
	number, ok := v.(json.Number)
	if !ok || strings.ContainsAny(string(number), ".eE") {
		return 0, false
	}
	value, err := number.Int64()
	return value, err == nil
}
