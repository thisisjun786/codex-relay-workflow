package adapter

import (
	"context"
	"encoding/json"
	"testing"
)

type fingerprintRPC struct{ page []any }

func (r fingerprintRPC) Call(context.Context, string, map[string]any) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"data": r.page})
}

func Test28RecipientFingerprintTurnIDParity(t *testing.T) {
	t.Parallel()
	pages := [][]any{
		{map[string]any{"turnId": json.Number("7"), "item": map[string]any{"id": "item", "text": "body"}}},
		{map[string]any{"turnId": json.Number("1.5"), "item": map[string]any{"id": "item", "text": "body"}}},
		{map[string]any{"turnId": "turn-7", "item": map[string]any{"id": "item", "text": "body"}}},
		{map[string]any{"item": map[string]any{"id": "item", "text": "body"}}},
		{map[string]any{"turnId": "turn", "id": json.Number("7"), "item": map[string]any{"id": json.Number("7"), "value": json.Number("1e20")}}},
		{map[string]any{"turnId": "turn", "id": nil, "item": map[string]any{"id": nil, "value": json.Number("1.50")}}},
	}
	fingerprints := []string{}
	for _, page := range pages {
		a := New(Options{RPC: fingerprintRPC{page}})
		got, err := a.RecipientFingerprint(context.Background(), "thread")
		if err != nil {
			t.Fatal(err)
		}
		fingerprints = append(fingerprints, got)
	}
	expectJSON(t, "fingerprints", fingerprints)
}
