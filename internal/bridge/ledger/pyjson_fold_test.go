package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// The ledger's fingerprint and its JSON reader answer as internal/pyjson does with the options the
// fold gives them, over the whole corpus: a stored fingerprint must never change.

var foldedCanonical = pyjson.Options{Compact: true, SortKeys: true, Normalize: true, Bytes: pyjson.ReplacedBytes}

func foldedFingerprint(method string, params map[string]any) (string, error) {
	data, err := pyjson.Encode([]any{method, params}, foldedCanonical)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

var foldedDecodeJSON = pyjson.LoadOptions{Map: true, Numbers: pyjson.SpelledNumbers, Surrogates: true, Deep: true}

// ledgerTypes are the Go values a fingerprint's params hold: what encoding/json and DecodeJSON
// read, and the bridge's own strings, ints and floats.
var ledgerTypes = []string{"map[string]interface {}", "[]interface {}", "string", "bool", "nil", "int64", "int", "float64", "json.Number"}

func TestFoldLedger(t *testing.T) {
	docs := pyjsontest.Recorded(t)
	for _, doc := range docs {
		want, wantErr := DecodeJSON([]byte(doc))
		got, err := pyjson.Loads(doc, foldedDecodeJSON)
		if (wantErr == nil) != (err == nil) || !pyjsontest.Same(want, got) {
			t.Errorf("DecodeJSON(%q) = %#v, %v; folded %#v, %v", doc, want, wantErr, got, err)
		}
	}
	values := pyjsontest.Decoded(docs, func(doc string) (any, error) { return DecodeJSON([]byte(doc)) })
	values = append(values, pyjsontest.Decoded(docs, func(doc string) (any, error) {
		decoder := json.NewDecoder(strings.NewReader(doc))
		decoder.UseNumber()
		var value any
		return value, decoder.Decode(&value)
	})...)
	for _, value := range values {
		if !pyjsontest.Within(value, ledgerTypes...) {
			continue
		}
		params, ok := value.(map[string]any)
		if !ok {
			params = map[string]any{"value": value}
		}
		for _, method := range []string{"create_thread", "m\xff\xed\xa0\x80é"} {
			want, wantErr := Fingerprint(method, params)
			got, err := foldedFingerprint(method, params)
			if !pyjsontest.Errors(wantErr, err) || want != got {
				var b bytes.Buffer
				_ = canonical(&b, []any{method, params})
				data, _ := pyjson.Encode([]any{method, params}, foldedCanonical)
				t.Errorf("Fingerprint(%q, %#v) = %s, %v; folded %s, %v\n%q\n%q", method, params, want, wantErr, got, err, b.String(), data)
			}
		}
	}
}
