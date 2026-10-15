package bridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

// seedCreateReservation stores a create_thread row on id with the given params and status, the way the ledger keeps one:
// Begin, then a Save with the status. A version-less row is a pre-upgrade receipt (fingerprint version 1), seeded as
// owned_legacy_test.go does.
func seedCreateReservation(t *testing.T, store *ledger.Ledger, id string, params map[string]any, status string, versionless bool) {
	t.Helper()
	_, r, err := store.Begin(context.Background(), id, "create_thread", params, nil)
	if err != nil {
		t.Fatal(err)
	}
	if versionless {
		delete(r, "fingerprintVersion")
	}
	r["status"] = status
	if _, err = store.Save(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func TestCreateThreadReservationTellsAPromptedReservationFromAPromptlessOne(t *testing.T) {
	b, _ := testBridge(t)
	ctx := context.Background()
	cwd := t.TempDir()
	row := func(prompt, title any) map[string]any {
		return map[string]any{"cwd": cwd, "sandbox": "read-only", "approvalPolicy": "never", "ephemeral": false, "prompt": prompt, "title": title}
	}
	seedCreateReservation(t, b.Ledger, "prompted", row("hello", "T1"), "not_attempted", false)
	seedCreateReservation(t, b.Ledger, "promptless", row(nil, "T1"), "not_attempted", false)
	seedCreateReservation(t, b.Ledger, "promptless-other-title", row(nil, "T2"), "not_attempted", false)
	seedCreateReservation(t, b.Ledger, "accepted", row(nil, "T1"), "accepted", false)
	seedCreateReservation(t, b.Ledger, "failed", row("hello", "T1"), "failed", false)

	// A version-less (pre-upgrade) receipt was fingerprinted over the resolved cwd; a call naming the symlink matches it
	// only through the legacy fingerprint, as CreateThread's own replay does.
	root := t.TempDir()
	target := filepath.Join(root, "target")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	legacyRow := func(prompt any) map[string]any {
		return map[string]any{"cwd": target, "sandbox": "read-only", "approvalPolicy": "never", "ephemeral": false, "prompt": prompt, "title": nil}
	}
	seedCreateReservation(t, b.Ledger, "legacy-promptless", legacyRow(nil), "not_attempted", true)
	seedCreateReservation(t, b.Ledger, "legacy-prompted", legacyRow("hello"), "not_attempted", true)

	for _, c := range []struct {
		name, id, cwd, prompt, title string
		want                         Reservation
	}{
		{"a prompted not_attempted reservation is other arguments", "prompted", cwd, "", "T1", ReservationOther},
		{"a prompted not_attempted reservation, asked with its own prompt", "prompted", cwd, "hello", "T1", ReservationOther},
		{"a prompt-null not_attempted reservation, asked without a prompt", "promptless", cwd, "", "T1", ReservationPromptless},
		{"a prompt-null not_attempted reservation, asked with a prompt", "promptless", cwd, "hello", "T1", ReservationPromptless},
		{"a prompt-null reservation with another title", "promptless-other-title", cwd, "", "T1", ReservationOther},
		{"no row", "absent", cwd, "", "T1", ReservationNone},
		{"an accepted receipt", "accepted", cwd, "", "T1", ReservationEffect},
		{"a failed receipt", "failed", cwd, "hello", "T1", ReservationEffect},
		{"a version-less prompt-null reservation asked through the symlink", "legacy-promptless", alias, "", "", ReservationPromptless},
		{"a version-less prompted reservation asked through the symlink", "legacy-prompted", alias, "", "", ReservationOther},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := b.CreateThreadReservation(ctx, CreateThread{RequestID: c.id, CWD: c.cwd, Sandbox: "read-only", Prompt: c.prompt, Title: c.title})
			if err != nil || got != c.want {
				t.Fatalf("reservation=%v err=%v, want %v", got, err, c.want)
			}
		})
	}

	t.Run("a ledger read failure is returned", func(t *testing.T) {
		store, err := ledger.Open(filepath.Join(t.TempDir(), "operations.sqlite3"))
		if err != nil {
			t.Fatal(err)
		}
		if err = store.Close(); err != nil {
			t.Fatal(err)
		}
		closed := New(b.RPC, store, executionPolicy())
		got, err := closed.CreateThreadReservation(ctx, CreateThread{RequestID: "prompted", CWD: cwd, Sandbox: "read-only"})
		if err == nil || got != ReservationNone {
			t.Fatalf("reservation=%v err=%v, want a read error", got, err)
		}
	})

	t.Run("no ledger is no reservation", func(t *testing.T) {
		got, err := (&Bridge{}).CreateThreadReservation(ctx, CreateThread{RequestID: "prompted", CWD: cwd, Sandbox: "read-only"})
		if err != nil || got != ReservationNone {
			t.Fatalf("reservation=%v err=%v", got, err)
		}
	})
}
