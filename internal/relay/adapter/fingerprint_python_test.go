package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fingerprintRPC struct{ page []any }

func (r fingerprintRPC) Call(context.Context, string, map[string]any) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"data": r.page})
}

func Test28RecipientFingerprintTurnIDParity(t *testing.T) {
	pages := [][]any{
		{map[string]any{"turnId": json.Number("7"), "item": map[string]any{"id": "item", "text": "body"}}},
		{map[string]any{"turnId": json.Number("1.5"), "item": map[string]any{"id": "item", "text": "body"}}},
		{map[string]any{"turnId": "turn-7", "item": map[string]any{"id": "item", "text": "body"}}},
		{map[string]any{"item": map[string]any{"id": "item", "text": "body"}}},
		{map[string]any{"turnId": "turn", "id": json.Number("7"), "item": map[string]any{"id": json.Number("7"), "value": json.Number("1e20")}}},
		{map[string]any{"turnId": "turn", "id": nil, "item": map[string]any{"id": nil, "value": json.Number("1.50")}}},
	}
	repo, _ := filepath.Abs("../../..")
	input, _ := json.Marshal(pages)
	cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/adapter/testdata/fingerprint_capture.py"))
	cmd.Dir = repo
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python fingerprint oracle: %v\n%s", err, out)
	}
	want := strings.Fields(string(out))
	for i, page := range pages {
		a := New(Options{RPC: fingerprintRPC{page}})
		got, err := a.RecipientFingerprint("thread")
		if err != nil {
			t.Fatal(err)
		}
		if got != want[i] {
			t.Fatalf("case %d: Go %s Python %s", i, got, want[i])
		}
	}
}
