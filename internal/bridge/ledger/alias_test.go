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

// CRW-1561: an alias row that never dispatched (not_attempted) is imported on the first start through the alias, and
// canonical's later state of the same request (here its retirement) is no conflict when the next start imports the alias
// ledger again. Another fingerprint, or an alias row past not_attempted that canonical changed, is still the conflict.
func TestEndpoint_whenAliasReservationIsRetiredTheNextStartImportsWithoutConflict(t *testing.T) {
	ctx := context.Background()
	promptless := map[string]any{"cwd": "/resolved", "prompt": nil}
	// fixture makes a socket, an alias to it and a state directory, and answers the alias ledger's and the canonical
	// ledger's file names.
	fixture := func(t *testing.T) (alias, state, aliasLedger, canonicalLedger string) {
		t.Helper()
		dir := t.TempDir()
		socket := filepath.Join(dir, "socket")
		if err := os.WriteFile(socket, nil, 0600); err != nil {
			t.Fatal(err)
		}
		alias = filepath.Join(dir, "alias")
		if err := os.Symlink(socket, alias); err != nil {
			t.Fatal(err)
		}
		state = filepath.Join(dir, "state")
		name := func(path string) string {
			abs, err := filepath.Abs(path)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(abs))
			return filepath.Join(state, "operations-"+hex.EncodeToString(hash[:])[:16]+".sqlite3")
		}
		return alias, state, name(alias), name(socket)
	}
	open := func(t *testing.T, path string) *Ledger {
		t.Helper()
		l, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	conflict := func(aliasLedger string) string {
		return `Conflicting retained request "id" in legacy socket ledger ` + aliasLedger + "; no requests will be dispatched. Preserve both ledgers."
	}

	t.Run("a retired not_attempted reservation imports again without conflict", func(t *testing.T) {
		alias, state, aliasLedger, _ := fixture(t)
		legacy := open(t, aliasLedger)
		seed(t, legacy, "id", promptless, "not_attempted", false)
		legacy.Close()
		_, l, err := Endpoint(alias, state)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := l.Get(ctx, "id"); err != nil || got["status"] != "not_attempted" {
			t.Fatalf("first import: %v %v", got, err)
		}
		retired, _, err := l.Retire(ctx, "id", "create_thread", promptless, nil, "retired before a prompt was required")
		if err != nil || !retired {
			t.Fatalf("retire: %v %v", retired, err)
		}
		settled := raw(t, l, "id")
		l.Close()
		_, l, err = Endpoint(alias, state)
		if err != nil {
			t.Fatalf("the next start refused the retired reservation: %v", err)
		}
		defer l.Close()
		if now := raw(t, l, "id"); now != settled {
			t.Fatalf("the retired receipt was replaced:\n got: %s\nwant: %s", now, settled)
		}
	})

	t.Run("another fingerprint is still the conflict", func(t *testing.T) {
		alias, state, aliasLedger, canonicalLedger := fixture(t)
		legacy := open(t, aliasLedger)
		seed(t, legacy, "id", promptless, "not_attempted", false)
		legacy.Close()
		canonical := open(t, canonicalLedger)
		seed(t, canonical, "id", map[string]any{"cwd": "/other", "prompt": nil}, "not_attempted", false)
		before := raw(t, canonical, "id")
		canonical.Close()
		_, l, err := Endpoint(alias, state)
		if err == nil {
			l.Close()
			t.Fatal("another fingerprint imported without conflict")
		}
		if err.Error() != conflict(aliasLedger) {
			t.Fatalf("refusal\n got: %v\nwant: %s", err, conflict(aliasLedger))
		}
		canonical = open(t, canonicalLedger)
		defer canonical.Close()
		if now := raw(t, canonical, "id"); now != before {
			t.Fatalf("the canonical receipt changed:\n got: %s\nwant: %s", now, before)
		}
	})

	// The scope of CRW-1561 item 2: a row that had progressed past not_attempted in the alias ledger and was then changed
	// in canonical is still the conflict (follow-up F1 changes this assertion when it lands).
	t.Run("an alias row saved started and then accepted in canonical is still the conflict", func(t *testing.T) {
		alias, state, aliasLedger, _ := fixture(t)
		legacy := open(t, aliasLedger)
		seed(t, legacy, "id", promptless, "started", false)
		legacy.Close()
		_, l, err := Endpoint(alias, state)
		if err != nil {
			t.Fatal(err)
		}
		got, err := l.Get(ctx, "id")
		if err != nil || got["status"] != "started" {
			t.Fatalf("first import: %v %v", got, err)
		}
		got["status"] = "accepted"
		if _, err := l.Save(ctx, got); err != nil {
			t.Fatal(err)
		}
		l.Close()
		_, l, err = Endpoint(alias, state)
		if err == nil {
			l.Close()
			t.Fatal("a changed started row imported without conflict")
		}
		if err.Error() != conflict(aliasLedger) {
			t.Fatalf("refusal\n got: %v\nwant: %s", err, conflict(aliasLedger))
		}
	})
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
	want := `Conflicting retained request "id" in legacy socket ledger ` + filepath.Join(dir, "right.sqlite3") + "; no requests will be dispatched. Preserve both ledgers."
	if err == nil || err.Error() != want {
		t.Fatalf("refusal\n got: %v\nwant: %s", err, want)
	}
	if _, err := left.Get(ctx, "extra"); !errors.Is(err, ErrUnknown) || err.Error() != "Unknown request_id" {
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
