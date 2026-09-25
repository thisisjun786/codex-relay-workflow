package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestEndpoint_whenAliasImportsLegacy(t *testing.T) {
	// Given: an alias ledger with an accepted request and its real socket target.
	dir := t.TempDir()
	socket := filepath.Join(dir, "socket")
	if err := os.WriteFile(socket, nil, 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(socket, alias); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	supplied, _ := filepath.Abs(alias)
	hash := sha256.Sum256([]byte(supplied))
	old := filepath.Join(state, "operations-"+hex.EncodeToString(hash[:])[:16]+".sqlite3")
	legacy, err := Open(old)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, r, err := legacy.Begin(ctx, "id", "create", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r["status"] = "accepted"
	if _, err := legacy.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	legacy.Close()
	// When: the canonical socket is opened via its alias.
	canonical, ledger, err := Endpoint(alias, state)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	// Then: identity resolves to the real socket and the receipt remains accepted.
	got, err := ledger.Get(ctx, "id")
	if err != nil || canonical != socket || got["status"] != "accepted" {
		t.Fatalf("canonical=%s receipt=%v err=%v", canonical, got, err)
	}
}
func TestEndpoint_whenDanglingAliasResolvesTarget(t *testing.T) {
	// Given: an alias pointing to a socket that does not exist yet.
	dir := t.TempDir()
	target := filepath.Join(dir, "future.sock")
	alias := filepath.Join(dir, "alias.sock")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	// When: the endpoint ledger opens through the alias.
	canonical, l, err := Endpoint(alias, filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// Then: Python Path.resolve(strict=False) names the missing target.
	if canonical != target {
		t.Fatalf("got %s want %s", canonical, target)
	}
	hash := sha256.Sum256([]byte(target))
	want := filepath.Join(dir, "state", "operations-"+hex.EncodeToString(hash[:])[:16]+".sqlite3")
	if _, err := os.Stat(want); err != nil {
		t.Fatal(err)
	}
}

func TestImportLegacy_whenConflictRollsBack(t *testing.T) {
	// Given: two ledgers with one conflicting ID and an alias-only row.
	dir := t.TempDir()
	left, err := Open(filepath.Join(dir, "left.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	right, err := Open(filepath.Join(dir, "right.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	ctx := context.Background()
	if _, _, err := left.Begin(ctx, "id", "create", map[string]any{"value": 1}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := right.Begin(ctx, "extra", "create", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := right.Begin(ctx, "id", "create", map[string]any{"value": 2}, nil); err != nil {
		t.Fatal(err)
	}
	// When: the legacy ledger is imported.
	err = left.ImportLegacy(ctx, filepath.Join(dir, "right.sqlite3"))
	// Then: the conflicting row rejects the entire import, including the earlier extra row.
	if err == nil {
		t.Fatal("accepted conflicting legacy ledger")
	}
	if _, err := left.Get(ctx, "extra"); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
}
func TestBegin_whenRepeatedRetryHistoryIsBounded(t *testing.T) {
	// Given: a fresh request.
	l, err := Open(filepath.Join(t.TempDir(), "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	_, r, err := l.Begin(ctx, "id", "create", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// When: it is marked not attempted and retried eight times.
	for i := range 8 {
		r["status"] = "not_attempted"
		r["error"] = fmt.Sprintf("failure %d", i)
		if _, err := l.Save(ctx, r); err != nil {
			t.Fatal(err)
		}
		fresh, next, err := l.Begin(ctx, "id", "create", map[string]any{}, nil)
		if err != nil || !fresh {
			t.Fatalf("%v %v", fresh, err)
		}
		r = next
	}
	// Then: only the five latest failures survive.
	history, ok := r["priorAttempts"].([]any)
	if !ok || len(history) != 5 || r["attempt"] != 9 {
		t.Fatalf("%v", r)
	}
	if history[0].(map[string]any)["error"] != "failure 3" {
		t.Fatal(history)
	}
}
