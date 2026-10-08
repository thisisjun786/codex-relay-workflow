package gui

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/policystore"
)

// TestPolicyWriteCarriesTheKeptPath is the pre-merge evaluation's d2 at the route: the write kept a
// document another writer put there, and the only place those bytes exist is the path the answer
// names. The path was reported only through the warnings, and a caller reading the recovery needs
// it as its own field of the refusal.
func TestPolicyWriteCarriesTheKeptPath(t *testing.T) {
	file := policyHost(t, policyWritableText, true)
	kept := filepath.Join(filepath.Dir(file), "kept-raced.json")
	if err := os.WriteFile(kept, []byte("a document another writer put there\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := policyWriteSeams
	policyWriteSeams = policystore.WriteOptions{
		Running: unavailablePolicyRunning,
		Register: func(context.Context, string) policystore.RegisterAnswer {
			t.Fatal("the registration step ran although the publication kept another writer's bytes")
			return policystore.RegisterAnswer{}
		},
		Swap: func(context.Context, string, []byte, []byte, os.FileMode) ([]byte, string, error) {
			return nil, kept, errors.New("the exchange could not be undone")
		},
	}
	t.Cleanup(func() { policyWriteSeams = previous })
	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	code, body := policyWriteResponse(t, policyServer(t), payload)
	if code != http.StatusInternalServerError || body["error"] != "recovery_needed" {
		t.Fatalf("kept recovery: %d %v", code, body)
	}
	if got, _ := body["kept"].(string); got != kept {
		t.Fatalf("the refusal does not name the kept file: kept = %q, want %q (body %v)", got, kept, body)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("the kept bytes are gone: %v", err)
	}
}
