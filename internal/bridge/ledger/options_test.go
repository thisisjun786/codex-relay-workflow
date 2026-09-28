package ledger

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOptionsClockAndOrderedReceiptAreOptIn(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "CODEX_HOME", "CODEX_SESSION_RELAY_STATE"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	expected := []byte(`{"requestId": "ordered", "status": "accepted", "updatedAt": 1700000000.125}`)
	custom, err := OpenWithOptions(filepath.Join(root, "custom.sqlite3"), Options{Now: func() float64 { return 1700000000.125 }, Encode: func(r Receipt) ([]byte, error) {
		if r["status"] == "accepted" {
			return expected, nil
		}
		return []byte(`{"requestId":"ordered","status":"in_progress_or_unknown","startedAt":1700000000.125,"retrySafe":false,"fingerprintVersion":2}`), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer custom.Close()
	fresh, receipt, err := custom.Begin(context.Background(), "ordered", "method", map[string]any{}, nil)
	if err != nil || !fresh {
		t.Fatalf("begin %v %v", fresh, err)
	}
	if receipt["startedAt"].(interface{ String() string }).String() != "1700000000.125" {
		t.Fatal(receipt)
	}
	receipt["status"] = "accepted"
	if _, err := custom.Save(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	raw, err := custom.RawReceipt(context.Background(), "ordered")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, expected) {
		t.Fatalf("ordered bytes %s", raw)
	}
	plain, err := Open(filepath.Join(root, "default.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if plain.options.Now != nil || plain.options.Encode != nil {
		t.Fatal("default options changed")
	}
	if _, err := os.Stat(filepath.Join(root, "custom.sqlite3")); err != nil {
		t.Fatal(err)
	}
}
