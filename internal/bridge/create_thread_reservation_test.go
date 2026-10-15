package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
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

// seedOperation stores a row of method on id with params and status, as seedCreateReservation does for create_thread.
func seedOperation(t *testing.T, store *ledger.Ledger, id, method string, params map[string]any, status string) {
	t.Helper()
	_, r, err := store.Begin(context.Background(), id, method, params, nil)
	if err != nil {
		t.Fatal(err)
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
	// Rows of another operation on the id, in a status that would otherwise read as a reservation and in one with an
	// effect: neither is a create_thread reservation.
	seedOperation(t, b.Ledger, "other-operation", "send_message_to_thread", map[string]any{"threadId": "thread-1", "message": "hi"}, "not_attempted")
	seedOperation(t, b.Ledger, "other-operation-accepted", "send_message_to_thread", map[string]any{"threadId": "thread-1", "message": "hi"}, "accepted")

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
		{"another operation's not_attempted row", "other-operation", cwd, "", "T1", ReservationOtherOperation},
		{"another operation's accepted row", "other-operation-accepted", cwd, "", "T1", ReservationOtherOperation},
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

// promptRequired is a create_thread as the MCP tool makes it, in cwd, with prompt.
func promptRequired(id, cwd, prompt, title string) CreateThread {
	return CreateThread{RequestID: id, CWD: cwd, Prompt: prompt, Title: title, Sandbox: "read-only", Model: "explicit-model", Effort: "high", PromptRequired: true}
}

func answerCreation(host *fakehost.Server, cwd string) {
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}, "cwd": cwd, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"}}})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
}

func stored(t *testing.T, store *ledger.Ledger, id string) string {
	t.Helper()
	raw, err := store.RawReceipt(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// CRW-1506 (x1506 #1): a create_thread that must bring a prompt and brings none never begins an attempt, whatever the
// MCP layer's own lookup saw: the bridge decides under its mutation lock, after its own lookup. A row that was in flight
// when the caller looked and turned not_attempted before the bridge looked is retired, never re-run, and nothing reaches
// the host; with no row nothing is reserved; a receipt that is not not_attempted is replayed as it is.
func TestCreateThreadThatMustBringAPromptNeverBeginsAnAttemptWithoutOne(t *testing.T) {
	ctx := context.Background()
	t.Run("a reservation that turns not_attempted after the caller's lookup is retired, not re-run", func(t *testing.T) {
		b, host := testBridge(t)
		cwd := t.TempDir()
		answerCreation(host, cwd)
		in := promptRequired("in-flight", cwd, "", "")
		seedCreateReservation(t, b.Ledger, "in-flight", createThreadParams(in), "in_progress_or_unknown", false)
		if held, err := b.CreateThreadReservation(ctx, in); err != nil || held != ReservationEffect {
			t.Fatalf("the caller's lookup: %v %v", held, err)
		}
		flips := 0
		b.waiting = func(id string) {
			if flips++; flips > 1 {
				return
			}
			// The attempt in flight ends with nothing reaching the host, after the caller's lookup and before the bridge's.
			r, err := b.Ledger.Get(ctx, id)
			if err != nil {
				t.Error(err)
				return
			}
			r["status"] = "not_attempted"
			if _, err = b.Ledger.Save(ctx, r); err != nil {
				t.Error(err)
			}
		}
		got, err := b.CreateThread(ctx, in)
		if !errors.Is(err, ErrRetiredBeforePrompt) || got != nil {
			t.Fatalf("answered %v %v", got, err)
		}
		if host.Count("thread/start") != 0 || host.Count("turn/start") != 0 {
			t.Fatalf("the reservation was re-run: %v", host.Requests())
		}
		if r, err := b.Ledger.Get(ctx, "in-flight"); err != nil || !retiredBeforePrompt(r) || r["retrySafe"] != false {
			t.Fatalf("not retired: %v %v", r, err)
		}
		// A later call, with or without a prompt, gets the same answer and reaches nothing.
		b.waiting = nil
		for _, prompt := range []string{"", "hello"} {
			if got, err := b.CreateThread(ctx, promptRequired("in-flight", cwd, prompt, "")); !errors.Is(err, ErrRetiredBeforePrompt) || got != nil {
				t.Fatalf("a later call with prompt %q answered %v %v", prompt, got, err)
			}
		}
		if host.Count("thread/start") != 0 {
			t.Fatalf("a later call reached the host: %v", host.Requests())
		}
	})
	t.Run("a version-less reservation asked through a symlink is retired through the legacy fingerprint", func(t *testing.T) {
		b, host := testBridge(t)
		root := t.TempDir()
		target, alias := filepath.Join(root, "target"), filepath.Join(root, "alias")
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, alias); err != nil {
			t.Fatal(err)
		}
		for i, prompt := range []string{"", "hello"} {
			id := fmt.Sprintf("legacy-%d", i)
			// A pre-upgrade receipt, fingerprinted over the resolved cwd.
			seedCreateReservation(t, b.Ledger, id, createThreadParams(promptRequired(id, target, "", "")), "not_attempted", true)
			if got, err := b.CreateThread(ctx, promptRequired(id, alias, prompt, "")); !errors.Is(err, ErrRetiredBeforePrompt) || got != nil {
				t.Fatalf("prompt %q: answered %v %v", prompt, got, err)
			}
			if r, err := b.Ledger.Get(ctx, id); err != nil || !retiredBeforePrompt(r) {
				t.Fatalf("prompt %q: not retired: %v %v", prompt, r, err)
			}
		}
		if host.Count("thread/start") != 0 {
			t.Fatalf("reached the host: %v", host.Requests())
		}
	})
	t.Run("no row: nothing is reserved", func(t *testing.T) {
		b, host := testBridge(t)
		got, err := b.CreateThread(ctx, promptRequired("absent", t.TempDir(), "", ""))
		if !errors.Is(err, ErrPromptRequired) || got != nil {
			t.Fatalf("answered %v %v", got, err)
		}
		if _, err := b.Ledger.Get(ctx, "absent"); !errors.Is(err, ledger.ErrUnknown) || host.Count("thread/start")+host.Count("turn/start") != 0 {
			t.Fatalf("a refused call reserved (%v) or reached the host: %v", err, host.Requests())
		}
	})
	t.Run("a receipt that is not not_attempted is replayed untouched", func(t *testing.T) {
		b, host := testBridge(t)
		cwd := t.TempDir()
		for _, status := range []string{"accepted", "in_progress_or_unknown", "outcome_unknown", "failed"} {
			in := promptRequired(status, cwd, "", "")
			seedCreateReservation(t, b.Ledger, status, createThreadParams(in), status, false)
			before := stored(t, b.Ledger, status)
			got, err := b.CreateThread(ctx, in)
			if err != nil || got["status"] != status || got["replayed"] != true || stored(t, b.Ledger, status) != before {
				t.Fatalf("%s: answered %v %v", status, got, err)
			}
		}
		if host.Count("thread/start")+host.Count("turn/start") != 0 {
			t.Fatalf("a replay reached the host: %v", host.Requests())
		}
	})
	t.Run("a reservation of other arguments is a conflict and untouched", func(t *testing.T) {
		b, _ := testBridge(t)
		cwd := t.TempDir()
		seedCreateReservation(t, b.Ledger, "prompted", createThreadParams(promptRequired("prompted", cwd, "hello", "")), "not_attempted", false)
		before := stored(t, b.Ledger, "prompted")
		if got, err := b.CreateThread(ctx, promptRequired("prompted", cwd, "", "")); !errors.Is(err, ledger.ErrConflict) || got != nil || stored(t, b.Ledger, "prompted") != before {
			t.Fatalf("answered %v %v", got, err)
		}
	})
}

// CRW-1506 (x1506 #2): a call with a prompt that conflicts with a reservation made with its arguments without a prompt,
// before prompts were required, retires that reservation before it is answered with the way to a new request_id, so no
// bridge that still takes a prompt-less create can resume it. Every other conflict is left as it is, and a caller that
// does not require a prompt (a relay creation) retires nothing.
func TestCreateThreadWithAPromptRetiresItsPromptlessReservationOnTheConflict(t *testing.T) {
	ctx := context.Background()
	b, host := testBridge(t)
	cwd := t.TempDir()
	answerCreation(host, cwd)
	seed := func(id, prompt, title, status string) string {
		seedCreateReservation(t, b.Ledger, id, createThreadParams(promptRequired(id, cwd, prompt, title)), status, false)
		return stored(t, b.Ledger, id)
	}

	relay := seed("relay", "", "", "not_attempted")
	in := promptRequired("relay", cwd, "hello", "")
	in.PromptRequired = false
	if _, err := b.CreateThread(ctx, in); !errors.Is(err, ledger.ErrConflict) || stored(t, b.Ledger, "relay") != relay {
		t.Fatalf("a caller that does not require a prompt: %v", err)
	}

	seed("promptless", "", "", "not_attempted")
	if got, err := b.CreateThread(ctx, promptRequired("promptless", cwd, "hello", "")); !errors.Is(err, ErrRetiredBeforePrompt) || got != nil {
		t.Fatalf("answered %v %v", got, err)
	}
	if r, err := b.Ledger.Get(ctx, "promptless"); err != nil || !retiredBeforePrompt(r) {
		t.Fatalf("not retired: %v %v", r, err)
	}
	// A bridge that still takes a prompt-less create replays the retired receipt.
	old := promptRequired("promptless", cwd, "", "")
	old.PromptRequired = false
	if replay, err := b.CreateThread(ctx, old); err != nil || replay["status"] != "failed" || replay["replayed"] != true {
		t.Fatalf("an old build's retry: %v %v", replay, err)
	}

	for _, c := range []struct{ id, title, status string }{
		{"other-title", "T2", "not_attempted"},
		{"accepted", "", "accepted"},
		{"unknown", "", "outcome_unknown"},
	} {
		before := seed(c.id, "", c.title, c.status)
		if got, err := b.CreateThread(ctx, promptRequired(c.id, cwd, "hello", "")); !errors.Is(err, ledger.ErrConflict) || got != nil || stored(t, b.Ledger, c.id) != before {
			t.Fatalf("%s: answered %v %v", c.id, got, err)
		}
	}
	// A not_attempted row of another operation is never retired, with a prompt or without.
	seedOperation(t, b.Ledger, "other-operation", "send_message_to_thread", map[string]any{"threadId": "thread-1", "message": "hi"}, "not_attempted")
	before := stored(t, b.Ledger, "other-operation")
	for _, prompt := range []string{"hello", ""} {
		if got, err := b.CreateThread(ctx, promptRequired("other-operation", cwd, prompt, "")); !errors.Is(err, ledger.ErrConflict) || got != nil || stored(t, b.Ledger, "other-operation") != before {
			t.Fatalf("another operation's row with prompt %q: answered %v %v", prompt, got, err)
		}
	}
	if host.Count("thread/start") != 0 {
		t.Fatalf("a conflict reached the host: %v", host.Requests())
	}
}

// CRW-1506 (x1506 #3): when a call with a prompt conflicts and the request_id then cannot be read, the read failure is
// returned, not the conflict: a ledger that cannot be read does not show that the row is other arguments.
func TestCreateThreadWithAPromptReturnsTheReadFailureAfterTheConflict(t *testing.T) {
	ctx := context.Background()
	b, host := testBridge(t)
	cwd := t.TempDir()
	seedCreateReservation(t, b.Ledger, "unreadable", createThreadParams(promptRequired("unreadable", cwd, "", "T1")), "not_attempted", false)
	waits := 0
	b.waiting = func(string) {
		// The first wait is the mutation's, which meets the conflict; the second is the retirement's read after it.
		if waits++; waits == 2 {
			if err := b.Ledger.Close(); err != nil {
				t.Error(err)
			}
		}
	}
	got, err := b.CreateThread(ctx, promptRequired("unreadable", cwd, "hello", "T2"))
	if waits != 2 || err == nil || errors.Is(err, ledger.ErrConflict) || errors.Is(err, ErrRetiredBeforePrompt) || got != nil {
		t.Fatalf("after %d waits answered %v %v", waits, got, err)
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("the error is not the read failure: %v", err)
	}
	if host.Count("thread/start")+host.Count("turn/start") != 0 {
		t.Fatalf("reached the host: %v", host.Requests())
	}
}
