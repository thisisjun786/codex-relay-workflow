package ledger

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Fingerprint matches Python json.dumps([method, params], sort_keys=True,
// separators=(",", ":")) followed by SHA-256, over the values encoding/json (UseNumber) gives a
// request: a json.Number written as the int or float it spells, a lone surrogate held as WTF-8
// as its \u escape, and any other byte that is not UTF-8 as U+FFFD (pyjson.Encode with
// Normalize and ReplacedBytes), the bytes every stored fingerprint was taken over.
func Fingerprint(method string, params map[string]any) (string, error) {
	data, err := pyjson.Encode([]any{method, params}, pyjson.Options{Compact: true, SortKeys: true, Normalize: true, Bytes: pyjson.ReplacedBytes})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}
